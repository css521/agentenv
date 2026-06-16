//go:build linux

package repo

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/css521/agentenv/internal/watch"
)

// Capturer auto-snapshots the inner-env whenever it changes and settles. Change
// detection is channel-agnostic and, where possible, event-driven:
//
//   - An inotify Watcher signals on any change (shell command OR direct file
//     edit), so the idle path does no filesystem walking.
//   - A periodic Token() check is kept as a backstop (and as the only mechanism
//     if the watcher can't be created).
//
// This removes the need for the agent to ever call "commit" — the environment
// versions itself like a DVR.
type Capturer struct {
	r        *Repo
	tick     time.Duration // cheap timer cadence (no fs walk)
	debounce time.Duration // how long changes must settle before snapshotting
	backstop int           // run a Token() check every N ticks

	mu             sync.Mutex
	pendingLabel   string
	paused         bool
	dirty          bool
	lastChange     time.Time
	lastSnapToken  string
	watcher        *watch.Watcher
	fallbackWarned bool // emit the polling-fallback warning at most once
	onSnapshot     func(id, label string)

	stop chan struct{}
	done chan struct{}
}

// SetOnSnapshot registers a callback invoked after each auto-snapshot (UI feedback).
func (c *Capturer) SetOnSnapshot(fn func(id, label string)) {
	c.mu.Lock()
	c.onSnapshot = fn
	c.mu.Unlock()
}

// StartCapturer launches the background capture loop and records it on the repo so
// Checkout can coordinate (pause) with it.
func (r *Repo) StartCapturer() *Capturer {
	tok, _ := r.token()
	c := &Capturer{
		r:             r,
		tick:          500 * time.Millisecond,
		debounce:      time.Duration(r.be.DebounceMillis) * time.Millisecond,
		lastSnapToken: tok,
		stop:          make(chan struct{}),
		done:          make(chan struct{}),
	}
	c.mu.Lock()
	c.restartWatcherLocked()
	if c.watcher != nil {
		c.backstop = int(30 * time.Second / c.tick) // verify ~every 30s
	} else {
		c.backstop = max(1, r.be.PollMillis/int(c.tick/time.Millisecond))
	}
	c.mu.Unlock()
	r.cap = c
	go c.run()
	return c
}

func (c *Capturer) run() {
	defer close(c.done)
	t := time.NewTicker(c.tick)
	defer t.Stop()
	n := 0
	for {
		select {
		case <-c.stop:
			c.mu.Lock()
			if c.watcher != nil {
				c.watcher.Close()
				c.watcher = nil
			}
			c.mu.Unlock()
			return
		case <-t.C:
			c.mu.Lock()
			if c.paused {
				c.mu.Unlock()
				continue
			}
			n++
			if !c.dirty && (c.watcher == nil || n >= c.backstop) {
				n = 0
				if tok, err := c.r.token(); err == nil && tok != c.lastSnapToken {
					c.dirty = true
					c.lastChange = time.Now()
				}
			}
			if c.dirty && time.Since(c.lastChange) >= c.debounce {
				c.snapshotLocked("")
				c.dirty = false
			}
			c.mu.Unlock()
		}
	}
}

// restartWatcherLocked (re)creates the inotify watcher on the current work root.
// Called at start and after a checkout swaps the working tree. Caller holds c.mu.
func (c *Capturer) restartWatcherLocked() {
	if c.watcher != nil {
		c.watcher.Close()
		c.watcher = nil
	}
	w, err := watch.New(c.r.workRoot(), c.r.be.Snapshotter.Ignored)
	if err != nil {
		// Fall back to Token polling. Warn once so the operator knows.
		if !c.fallbackWarned {
			fmt.Fprintf(os.Stderr, "agentenv: inotify unavailable (%v) — using Token polling (slower; raise fs.inotify.max_user_watches if hitting the limit)\n", err)
			c.fallbackWarned = true
		}
		return
	}
	c.watcher = w
	go c.forward(w)
}

// forward marks the capturer dirty whenever its watcher signals a change.
func (c *Capturer) forward(w *watch.Watcher) {
	for range w.Changed() {
		c.mu.Lock()
		if c.watcher == w && !c.paused {
			c.dirty = true
			c.lastChange = time.Now()
		}
		c.mu.Unlock()
	}
}

// snapshotLocked captures the current state as a new node. Caller holds c.mu.
//
// LOCK DISCIPLINE: the heavy I/O (Commit/applyRetention/token-resync) used to
// run while c.mu was held — that blocked forward()'s `c.mu.Lock()` for the
// entire snapshot duration, meaning inotify events arriving mid-snapshot were
// silently lost (forward couldn't even mark the dirty flag). We now release
// c.mu around the I/O. While unlocked, NO field of c is read or written
// (everything we need is captured in locals first), so re-entry is safe; the
// onSnapshot callback also runs without c.mu so a slow callback can't stall
// the watcher.
func (c *Capturer) snapshotLocked(label string) {
	changed := c.drainLocked()
	if label == "" {
		if c.pendingLabel != "" {
			label = c.pendingLabel
		} else {
			label = autoLabel(changed)
		}
	}
	c.pendingLabel = ""
	cb := c.onSnapshot

	c.mu.Unlock() // RELEASE: Commit/applyRetention can take seconds on a big tree
	n, err := c.r.Commit(label, label)
	var newToken string
	if err == nil {
		c.r.applyRetention()
		newToken, _ = c.r.token()
	}
	c.mu.Lock() // RE-ACQUIRE before mutating capturer fields

	if err != nil {
		return
	}
	if newToken != "" {
		c.lastSnapToken = newToken
	}
	if cb != nil {
		// Run the callback without c.mu so user code can't deadlock the watcher.
		c.mu.Unlock()
		cb(n.ID, n.Message)
		c.mu.Lock()
	}
}

func (c *Capturer) drainLocked() []string {
	if c.watcher != nil {
		return c.watcher.Drain()
	}
	return nil
}

// autoLabel summarizes the changed paths for a snapshot label.
func autoLabel(changed []string) string {
	if len(changed) == 0 {
		return "auto"
	}
	const show = 3
	if len(changed) <= show {
		return "auto: " + strings.Join(changed, ", ")
	}
	return fmt.Sprintf("auto: %s (+%d more)", strings.Join(changed[:show], ", "), len(changed)-show)
}

// SetLabel hints the label for the next snapshot (e.g. the command being run).
func (c *Capturer) SetLabel(s string) {
	c.mu.Lock()
	c.pendingLabel = s
	c.mu.Unlock()
}

// Flush snapshots immediately if the env changed since the last node (used right
// after a shell command so the node lands promptly with the command's label).
func (c *Capturer) Flush(label string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if tok, err := c.r.token(); err == nil && tok != c.lastSnapToken {
		c.snapshotLocked(label)
		c.dirty = false
	}
}

// pause stops the loop from snapshotting (used during checkout).
func (c *Capturer) pause() {
	c.mu.Lock()
	c.paused = true
	c.mu.Unlock()
}

// resumeBaseline re-establishes the watcher on the (new) work tree and resumes.
func (c *Capturer) resumeBaseline() {
	c.mu.Lock()
	c.restartWatcherLocked()
	if tok, err := c.r.token(); err == nil {
		c.lastSnapToken = tok
	}
	c.dirty = false
	c.paused = false
	c.mu.Unlock()
}

// Stop ends the loop and waits for it to exit.
func (c *Capturer) Stop() {
	close(c.stop)
	<-c.done
}
