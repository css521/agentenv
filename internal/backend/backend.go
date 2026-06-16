//go:build linux

// Package backend abstracts the two environment-specific concerns — how to run a
// command in a rootfs (Runner) and how to snapshot/restore that rootfs
// (Snapshotter) — behind interfaces, so the orchestration layer (DAG, capture,
// retention, serve/watch) is identical everywhere.
//
// Backends register themselves via init() and are selected at startup by Detect,
// which picks the highest-priority one available in the current environment:
//
//	btrfs   (priority 100) — privileged host on a btrfs filesystem; CoW, fastest.
//	                          Only compiled in with `-tags btrfs` (needs cgo+libbtrfs).
//	copy    (priority 0)   — rootless (user namespace) + plain-copy snapshots;
//	                          no privilege, no special kernel features, any fs.
//	                          The default, runs in a restricted K8s pod.
package backend

import (
	"fmt"
	"io"
	"os/exec"
	"sort"
)

// Runner executes commands inside a rootfs directory/subvolume.
type Runner interface {
	// Run executes args in rootfs (foreground) and waits.
	Run(rootfs string, args, env []string, stdin io.Reader, stdout, stderr io.Writer) error
	// Start launches args in rootfs detached; caller Wait()s/Kill()s the returned cmd.
	Start(rootfs string, args, env []string, out io.Writer) (*exec.Cmd, error)
	// Shell runs an interactive command in rootfs on a PTY wired to the caller's
	// terminal, returning its exit code.
	Shell(rootfs string, args, env []string) (int, error)
	// ShellHook is Shell plus an onStart callback invoked with the child process
	// right after it starts, so a supervisor can register it for kill-on-rollback.
	ShellHook(rootfs string, args, env []string, onStart func(*exec.Cmd)) (int, error)
}

// Snapshotter manages the writable working rootfs and immutable node snapshots.
type Snapshotter interface {
	// WorkRoot is the absolute path of the writable working rootfs (Runner uses it).
	WorkRoot() string
	// NodePath is the on-disk path of a node's rootfs (a real directory/subvolume,
	// browsable for diff/show).
	NodePath(id string) string
	// WorkExists reports whether the working rootfs currently exists.
	WorkExists() bool
	// NewEmptyWork creates a fresh, empty writable working rootfs.
	NewEmptyWork() error
	// RestoreWork rebuilds the working rootfs from an immutable node (derive/rollback).
	RestoreWork(nodeID string) error
	// Freeze captures the current working rootfs as immutable node nodeID. parentID
	// is the node it descends from ("" for root) — backends may use it to share
	// unchanged data with the parent (e.g. hardlinks).
	Freeze(nodeID, parentID string) error
	// DeleteNode removes a node's snapshot from disk.
	DeleteNode(nodeID string) error
	// NodeIDsOnDisk lists node IDs present on disk (for GC of orphans).
	NodeIDsOnDisk() ([]string, error)
	// Token returns an opaque value that changes when the working rootfs changes
	// (btrfs generation / content fingerprint); used as a backstop change check.
	Token() (string, error)
	// Ignored reports whether a rootfs-relative path is excluded from snapshots
	// (so the change watcher skips it too).
	Ignored(rel string) bool

	// NewWorkspace creates a fresh writable rootfs seeded from fromNodeID and
	// returns its absolute path. Unlike WorkRoot, this is an ADDITIONAL writable
	// tree (the main work/current is untouched) — used by Tournament to run N
	// candidates concurrently, each in its own isolated rootfs. Caller is
	// responsible for DeleteWorkspace.
	NewWorkspace(fromNodeID string) (string, error)
	// FreezeFrom captures an explicit workspace rootfs as immutable node nodeID
	// descending from parentID. Analogous to Freeze() but with an explicit src,
	// so multiple Tournament branches can freeze in parallel.
	FreezeFrom(workspacePath, nodeID, parentID string) error
	// DeleteWorkspace removes a workspace created by NewWorkspace.
	DeleteWorkspace(workspacePath string) error
}

// Backend bundles a Runner + Snapshotter and the capture cadence that suits it.
type Backend struct {
	Name           string
	Runner         Runner
	Snapshotter    Snapshotter
	PollMillis     int // how often the capturer samples Token()
	DebounceMillis int // how long changes must settle before snapshotting
}

type candidate struct {
	name      string
	priority  int
	available func(root string) bool
	build     func(root string) (*Backend, error)
}

var candidates []candidate

func register(c candidate) { candidates = append(candidates, c) }

// Detect picks the best available backend for root, or honors AGENTENV_BACKEND.
func Detect(root string) (*Backend, error) {
	ordered := append([]candidate(nil), candidates...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].priority > ordered[j].priority })
	for _, c := range ordered {
		if c.available(root) {
			return c.build(root)
		}
	}
	return nil, fmt.Errorf("no usable backend for %s", root)
}

// ForceName returns the named backend regardless of probe (for AGENTENV_BACKEND).
func ForceName(root, name string) (*Backend, error) {
	for _, c := range candidates {
		if c.name == name {
			return c.build(root)
		}
	}
	return nil, fmt.Errorf("backend %q not compiled in", name)
}
