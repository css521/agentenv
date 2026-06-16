//go:build linux

// Package watch provides an unprivileged, recursive inotify watcher used to
// detect changes to the working rootfs without polling/walking the whole tree
// (which is expensive on network filesystems). It works on any filesystem whose
// writes go through the kernel VFS, including the sandbox's bind-mounted rootfs.
package watch

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
)

const evMask = unix.IN_MODIFY | unix.IN_CREATE | unix.IN_DELETE |
	unix.IN_MOVED_FROM | unix.IN_MOVED_TO | unix.IN_CLOSE_WRITE |
	unix.IN_ATTRIB | unix.IN_DONT_FOLLOW

// Watcher recursively watches a directory tree and coalesces change events.
type Watcher struct {
	fd           int
	stopR, stopW int // self-pipe to unblock the poll loop on Close
	root         string
	ignored      func(rel string) bool

	mu      sync.Mutex
	wd2path map[int32]string
	changed map[string]bool // accumulated changed rel paths since last Drain

	notify chan struct{}
	done   chan struct{}
}

// New starts watching root (and, recursively, its directories), skipping any path
// for which ignored returns true. ignored may be nil.
func New(root string, ignored func(rel string) bool) (*Watcher, error) {
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC)
	if err != nil {
		return nil, err
	}
	var p [2]int
	if err := unix.Pipe2(p[:], unix.O_CLOEXEC); err != nil {
		unix.Close(fd)
		return nil, err
	}
	w := &Watcher{
		fd: fd, stopR: p[0], stopW: p[1], root: root, ignored: ignored,
		wd2path: map[int32]string{},
		changed: map[string]bool{},
		notify:  make(chan struct{}, 1),
		done:    make(chan struct{}),
	}
	if err := w.addRecursive(root); err != nil {
		// kernel inotify watch limit blew us out — close and signal the caller to
		// fall back to Token polling instead of running a half-blind watcher.
		_ = unix.Close(fd)
		_ = unix.Close(p[0])
		_ = unix.Close(p[1])
		return nil, fmt.Errorf("inotify watch limit exceeded (%w); raise fs.inotify.max_user_watches", err)
	}
	go w.loop()
	return w, nil
}

// Changed delivers a coalesced signal whenever something under root changes.
func (w *Watcher) Changed() <-chan struct{} { return w.notify }

// Drain returns and clears the set of changed paths (rootfs-relative) observed so
// far — used to label snapshots with what actually changed.
func (w *Watcher) Drain() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]string, 0, len(w.changed))
	for p := range w.changed {
		out = append(out, p)
	}
	w.changed = map[string]bool{}
	sort.Strings(out)
	return out
}

// Close stops the watcher and releases its fds.
func (w *Watcher) Close() {
	_, _ = unix.Write(w.stopW, []byte{0}) // wake the poll loop
	<-w.done
	_ = unix.Close(w.fd)
	_ = unix.Close(w.stopR)
	_ = unix.Close(w.stopW)
	close(w.notify)
}

func (w *Watcher) rel(path string) string {
	r, err := filepath.Rel(w.root, path)
	if err != nil {
		return ""
	}
	return r
}

// addRecursive returns an error only for ENOSPC (the kernel inotify watch limit),
// which means this watcher can never see the full tree → the capturer should fall
// back to Token polling. Other transient add failures are ignored.
func (w *Watcher) addRecursive(dir string) error {
	var limitHit bool
	filepath.Walk(dir, func(path string, fi os.FileInfo, err error) error {
		if err != nil || !fi.IsDir() {
			return nil
		}
		if rel := w.rel(path); rel != "." && w.ignored != nil && w.ignored(rel) {
			return filepath.SkipDir
		}
		wd, err := unix.InotifyAddWatch(w.fd, path, evMask)
		if err != nil {
			if err == unix.ENOSPC {
				limitHit = true
			}
			return nil
		}
		w.mu.Lock()
		w.wd2path[int32(wd)] = path
		w.mu.Unlock()
		return nil
	})
	if limitHit {
		return unix.ENOSPC
	}
	return nil
}

func (w *Watcher) loop() {
	defer close(w.done)
	buf := make([]byte, 64*1024)
	fds := []unix.PollFd{
		{Fd: int32(w.fd), Events: unix.POLLIN},
		{Fd: int32(w.stopR), Events: unix.POLLIN},
	}
	for {
		fds[0].Revents, fds[1].Revents = 0, 0
		if _, err := unix.Poll(fds, -1); err != nil {
			if err == unix.EINTR {
				continue
			}
			return
		}
		if fds[1].Revents != 0 {
			return // Close() signaled
		}
		if fds[0].Revents&unix.POLLIN == 0 {
			continue
		}
		n, err := unix.Read(w.fd, buf)
		if err != nil {
			if err == unix.EINTR || err == unix.EAGAIN {
				continue
			}
			return
		}
		touched := false
		for off := 0; off+unix.SizeofInotifyEvent <= n; {
			ev := (*unix.InotifyEvent)(unsafe.Pointer(&buf[off]))
			nameLen := int(ev.Len)
			name := ""
			if nameLen > 0 {
				b := buf[off+unix.SizeofInotifyEvent : off+unix.SizeofInotifyEvent+nameLen]
				name = string(b[:clen(b)])
			}
			off += unix.SizeofInotifyEvent + nameLen

			w.mu.Lock()
			dir := w.wd2path[ev.Wd]
			w.mu.Unlock()
			if dir == "" {
				continue
			}
			p := dir
			if name != "" {
				p = filepath.Join(dir, name)
			}
			rel := w.rel(p)
			if rel != "." && w.ignored != nil && w.ignored(rel) {
				continue
			}
			w.mu.Lock()
			w.changed[rel] = true
			w.mu.Unlock()
			touched = true
			// A newly created/moved-in directory needs its own watch.
			if ev.Mask&unix.IN_ISDIR != 0 && ev.Mask&(unix.IN_CREATE|unix.IN_MOVED_TO) != 0 {
				w.addRecursive(p)
			}
		}
		if touched {
			select {
			case w.notify <- struct{}{}:
			default:
			}
		}
	}
}

func clen(b []byte) int {
	for i, c := range b {
		if c == 0 {
			return i
		}
	}
	return len(b)
}
