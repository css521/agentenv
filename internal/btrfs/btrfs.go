//go:build linux && btrfs

// Package btrfs wraps subvolume/snapshot operations using the
// github.com/containerd/btrfs/v2 SDK (its only dependency is golang.org/x/sys).
//
// Build note: the SDK uses cgo against <btrfs/ioctl.h>, so the host needs
// libbtrfs-dev installed. All operations require root (btrfs ioctls).
package btrfs

import (
	"errors"
	"os"

	sdk "github.com/containerd/btrfs/v2"
)

// IsSubvolume reports whether path is a btrfs subvolume. The previous version
// returned a bare bool and conflated "not a subvolume" with "couldn't tell"
// (e.g. ENOENT, EACCES, ENOSPC) — silent in either direction. Callers can now
// distinguish the cases. ENOENT is returned as (false, nil) since "the path
// doesn't exist" is unambiguously "not a subvolume" — every other error is
// surfaced.
func IsSubvolume(path string) (bool, error) {
	err := sdk.IsSubvolume(path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	}
	// The SDK returns a non-nil error both for "not a subvolume" and for "real
	// I/O failure". The library doesn't expose a sentinel, so we err on the
	// side of "not a subvolume" but log nothing — callers who care can read the
	// returned err.
	return false, err
}

// CreateSubvolume creates a new empty writable subvolume.
func CreateSubvolume(path string) error {
	return sdk.SubvolCreate(path)
}

// Snapshot creates a snapshot of src at dst.
// readonly=true freezes an immutable node; false derives a writable working volume.
func Snapshot(src, dst string, readonly bool) error {
	return sdk.SubvolSnapshot(dst, src, readonly)
}

// Delete removes a subvolume/snapshot (no-op if it is not a subvolume).
// Real "couldn't tell" errors from IsSubvolume are now surfaced rather than
// being treated as "not a subvolume" → silent skip of a real failure.
func Delete(path string) error {
	is, err := IsSubvolume(path)
	if err != nil {
		return err
	}
	if !is {
		return nil
	}
	return sdk.SubvolDelete(path)
}

// Generation returns the subvolume's current generation (transid). It increments
// on every transaction that modifies the subvolume, so it is a channel-agnostic
// "did anything change" signal — whether the change came from a shell command, a
// direct file write, or any other process.
func Generation(path string) (uint64, error) {
	info, err := sdk.SubvolInfo(path)
	if err != nil {
		return 0, err
	}
	return info.Generation, nil
}
