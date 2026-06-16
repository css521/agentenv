//go:build linux

package repo

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Status is a one-shot snapshot of the repo's runtime state (for `agentenv status`).
type Status struct {
	Backend    string
	Root       string
	Head       string
	NodeCount  int
	LeafCount  int
	ProcCount  int
	DiskBytes  int64 // bytes on disk under root (real, hardlink-shared files counted once)
	PollMs     int
	DebounceMs int
	Ignore     []string // rootfs-relative paths currently excluded from snapshots
}

// Status returns a runtime snapshot. Cheap: takes the lock briefly and stats the
// root tree once.
func (r *Repo) Status() Status {
	r.opMu.Lock()
	nodeCount := len(r.dag.Nodes)
	leaf := 0
	for _, n := range r.dag.Nodes {
		if len(n.Children) == 0 {
			leaf++
		}
	}
	head := r.dag.Head
	r.opMu.Unlock()

	r.mu.Lock()
	procs := len(r.procs)
	r.mu.Unlock()

	// Surface effective ignore prefixes by probing the well-known defaults — the
	// btrfs backend returns false for all, the copy backend returns true for the
	// configured set. Cheap and informative without changing the interface.
	var ign []string
	for _, p := range []string{"tmp", "var/tmp", "var/cache", "proc", "sys", "dev", ".pivot_old"} {
		if r.be.Snapshotter.Ignored(p) {
			ign = append(ign, p)
		}
	}

	return Status{
		Backend:    r.be.Name,
		Root:       r.root,
		Head:       head,
		NodeCount:  nodeCount,
		LeafCount:  leaf,
		ProcCount:  procs,
		DiskBytes:  dirRealSize(r.root),
		PollMs:     r.be.PollMillis,
		DebounceMs: r.be.DebounceMillis,
		Ignore:     ign,
	}
}

// devIno is a (device, inode) pair — the unique identity of an on-disk file
// across mounts. Inode numbers alone are NOT unique across filesystems, so a
// dedup keyed only on inode would over-count files under one mount and
// under-count files that legitimately share an inode number on different
// mounts. (P1-5c review finding.)
type devIno struct{ Dev, Ino uint64 }

// dirRealSize walks dir summing on-disk bytes. Hardlink-shared files are
// counted once via (dev, inode) dedup. The previous version used st.Ino alone,
// silently breaking when AGENTENV_ROOT crosses a mount boundary. fi.Size()
// reports apparent size (sparse files over-count); on the snapshot store
// that's acceptable — sparse files are rare in extracted rootfs trees, and
// `du -B1` would be slow.
func dirRealSize(dir string) int64 {
	seen := map[devIno]struct{}{}
	var total int64
	filepath.Walk(dir, func(_ string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return nil
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			key := devIno{Dev: uint64(st.Dev), Ino: st.Ino}
			if _, dup := seen[key]; dup {
				return nil
			}
			seen[key] = struct{}{}
		}
		total += fi.Size()
		return nil
	})
	return total
}

// humanBytes formats a byte count as a short human-readable string (B/KB/MB/GB).
// Used by the init --from progress readout. (The CLI layer has its own copy for
// `status` output; this one keeps the repo package self-contained.)
func humanBytes(n int64) string {
	const k = 1024
	switch {
	case n < k:
		return fmt.Sprintf("%dB", n)
	case n < k*k:
		return fmt.Sprintf("%.1fKB", float64(n)/k)
	case n < k*k*k:
		return fmt.Sprintf("%.1fMB", float64(n)/(k*k))
	default:
		return fmt.Sprintf("%.2fGB", float64(n)/(k*k*k))
	}
}
