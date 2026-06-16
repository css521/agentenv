//go:build linux

package repo

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"golang.org/x/sys/unix"
)

// Lock holds an exclusive flock on the repo root so two mutating sessions
// cannot corrupt meta.json. Read-only commands skip it.
//
// Why a struct instead of a `func() / *os.File` pair: the *os.File must stay
// reachable until release, but a `func()` closure that captures it doesn't keep
// it from being GC'd if the GC's escape analysis decides the closure itself is
// short-lived (it can be — callers `defer release()` then drop the variable).
// A returned struct is naturally kept by the caller's `defer lock.Release()`.
type Lock struct {
	f *os.File
}

// AcquireLock takes the exclusive non-blocking flock and returns a Lock the
// caller must Release(). The kernel also drops the flock on process exit, but
// callers SHOULD route process termination through main (not os.Exit from
// random call sites) so deferred Release fires and so any in-flight save can
// finish.
func AcquireLock(root string) (*Lock, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(root, ".lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another agentenv session is active on %s (%w)", root, err)
	}
	return &Lock{f: f}, nil
}

// Release drops the flock and closes the file. KeepAlive is belt-and-suspenders:
// it ensures the *os.File survives until this method runs even under aggressive
// GC reordering (so the kernel never drops the lock prematurely from the GC
// finalizer).
func (l *Lock) Release() {
	if l == nil || l.f == nil {
		return
	}
	_ = unix.Flock(int(l.f.Fd()), unix.LOCK_UN)
	_ = l.f.Close()
	runtime.KeepAlive(l.f)
}
