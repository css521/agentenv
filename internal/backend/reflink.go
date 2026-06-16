//go:build linux

package backend

import (
	"io"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// reflinkCopy attempts to create dst as a copy-on-write clone of src using the
// FICLONE ioctl. On supporting filesystems (XFS, btrfs, bcachefs, ZFS via
// projects/zfs >= 2.2) the clone shares blocks and completes in O(1) regardless
// of file size — so a "copy" snapshot becomes effectively free.
//
// Caller passes an already-open src and a dst path; on success dst is created
// with the file's mode and mtime preserved. On any failure dst is removed (no
// half-written file) and a non-nil error is returned so the caller can fall back
// to a byte copy.
func reflinkCopy(src *os.File, dst string, mode os.FileMode, mtime time.Time) error {
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if err := unix.IoctlFileClone(int(out.Fd()), int(src.Fd())); err != nil {
		out.Close()
		_ = os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	// Preserve mtime so the unchanged-file heuristic (size+mtime+mode) stays sound
	// across snapshots.
	return os.Chtimes(dst, mtime, mtime)
}

// probeReflink tries one clone in the snapshot store to decide whether reflink
// is supported. The probe files are short-lived and best-effort cleaned up.
func probeReflink(root string) bool {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return false
	}
	srcPath := filepath.Join(root, ".reflink-probe-src")
	dstPath := filepath.Join(root, ".reflink-probe-dst")
	defer os.Remove(srcPath)
	defer os.Remove(dstPath)
	if err := os.WriteFile(srcPath, []byte("agentenv reflink probe"), 0o644); err != nil {
		return false
	}
	src, err := os.Open(srcPath)
	if err != nil {
		return false
	}
	defer src.Close()
	dst, err := os.OpenFile(dstPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return false
	}
	defer dst.Close()
	return unix.IoctlFileClone(int(dst.Fd()), int(src.Fd())) == nil
}

// copyOrReflink wraps reflink-or-fallback so callers don't carry the conditional.
func copyOrReflink(src *os.File, dst string, mode os.FileMode, mtime time.Time, useReflink bool) error {
	if useReflink {
		if err := reflinkCopy(src, dst, mode, mtime); err == nil {
			return nil // O(1) clone succeeded
		}
		// Most likely cause: probe succeeded but a specific file (e.g. a special
		// inode or cross-fs) refuses cloning. Fall through to byte copy.
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, src); err != nil {
		out.Close()
		_ = os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chtimes(dst, mtime, mtime)
}
