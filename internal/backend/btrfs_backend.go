//go:build linux && btrfs

package backend

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"golang.org/x/sys/unix"

	"github.com/css521/agentenv/internal/btrfs"
	"github.com/css521/agentenv/internal/sandbox"
)

// BTRFS_SUPER_MAGIC identifies a btrfs filesystem in statfs.
const btrfsSuperMagic = 0x9123683E

func init() {
	register(candidate{
		name:     "btrfs",
		priority: 100,
		available: func(root string) bool {
			if os.Geteuid() != 0 {
				return false // btrfs ioctls need real root
			}
			var st unix.Statfs_t
			if err := unix.Statfs(root, &st); err != nil {
				return false
			}
			return int64(st.Type) == btrfsSuperMagic
		},
		build: newBtrfsBackend,
	})
}

func newBtrfsBackend(root string) (*Backend, error) {
	s := &btrfsSnap{nodes: filepath.Join(root, "nodes"), work: filepath.Join(root, "work", "current")}
	if err := os.MkdirAll(s.nodes, 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(s.work), 0o755); err != nil {
		return nil, err
	}
	return &Backend{
		Name:           "btrfs (privileged)",
		Runner:         privilegedRunner{},
		Snapshotter:    s,
		PollMillis:     500,
		DebounceMillis: 1000,
	}, nil
}

// privilegedRunner runs commands using namespaces but no user namespace (the
// process is already real root).
type privilegedRunner struct{}

func (privilegedRunner) Run(rootfs string, args, env []string, stdin io.Reader, stdout, stderr io.Writer) error {
	return sandbox.Run(rootfs, args, env, stdin, stdout, stderr)
}

func (privilegedRunner) Start(rootfs string, args, env []string, out io.Writer) (*exec.Cmd, error) {
	return sandbox.Start(rootfs, args, env, out)
}

func (privilegedRunner) Shell(rootfs string, args, env []string) (int, error) {
	return sandbox.RunPTY(rootfs, args, env, false)
}

func (privilegedRunner) ShellHook(rootfs string, args, env []string, onStart func(*exec.Cmd)) (int, error) {
	return sandbox.RunPTYHook(rootfs, args, env, false, onStart)
}

// btrfsSnap snapshots via btrfs subvolumes (copy-on-write, O(1)).
type btrfsSnap struct {
	nodes string
	work  string
}

func (s *btrfsSnap) WorkRoot() string { return s.work }

// WorkExists answers "does the working subvolume exist?" — for "couldn't tell"
// errors (e.g. transient I/O), report `true` so callers don't blow it away and
// recreate; the next mutation will surface a real error.
func (s *btrfsSnap) WorkExists() bool {
	is, err := btrfs.IsSubvolume(s.work)
	if err != nil {
		return true
	}
	return is
}
func (s *btrfsSnap) NodePath(id string) string { return filepath.Join(s.nodes, id) }

func (s *btrfsSnap) NewEmptyWork() error {
	_ = btrfs.Delete(s.work)
	return btrfs.CreateSubvolume(s.work)
}

func (s *btrfsSnap) RestoreWork(nodeID string) error {
	if err := btrfs.Delete(s.work); err != nil {
		return err
	}
	return btrfs.Snapshot(s.NodePath(nodeID), s.work, false)
}

func (s *btrfsSnap) Freeze(nodeID, _ string) error {
	return btrfs.Snapshot(s.work, s.NodePath(nodeID), true)
}

func (s *btrfsSnap) DeleteNode(nodeID string) error { return btrfs.Delete(s.NodePath(nodeID)) }

// Ignored: the btrfs backend snapshots the whole subvolume, so nothing is ignored.
func (s *btrfsSnap) Ignored(string) bool { return false }

// workspacesDir is the parent of all transient parallel writable subvolumes.
func (s *btrfsSnap) workspacesDir() string { return filepath.Join(s.nodes, "..", "workspaces") }

// NewWorkspace creates a writable subvolume snapshot of fromNodeID for parallel
// exploration. btrfs subvolume snapshots are O(1) so N parallel workspaces are
// effectively free.
func (s *btrfsSnap) NewWorkspace(fromNodeID string) (string, error) {
	if err := os.MkdirAll(s.workspacesDir(), 0o755); err != nil {
		return "", err
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	dir := filepath.Join(s.workspacesDir(), hex.EncodeToString(b[:]))
	if err := btrfs.Snapshot(s.NodePath(fromNodeID), dir, false); err != nil {
		return "", err
	}
	return dir, nil
}

// FreezeFrom snapshots a workspace subvolume into a read-only node subvolume.
// parentID is unused (btrfs subvolume snapshots already share extents).
func (s *btrfsSnap) FreezeFrom(workspacePath, nodeID, _ string) error {
	return btrfs.Snapshot(workspacePath, s.NodePath(nodeID), true)
}

// DeleteWorkspace deletes a workspace subvolume.
func (s *btrfsSnap) DeleteWorkspace(path string) error { return btrfs.Delete(path) }

func (s *btrfsSnap) NodeIDsOnDisk() ([]string, error) {
	entries, err := os.ReadDir(s.nodes)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		ids = append(ids, e.Name())
	}
	return ids, nil
}

// Token returns the subvolume generation (after a syncfs so it reflects recent
// writes — btrfs only advances the generation on transaction commit).
func (s *btrfsSnap) Token() (string, error) {
	if fd, err := unix.Open(s.work, unix.O_RDONLY|unix.O_DIRECTORY, 0); err == nil {
		_ = unix.Syncfs(fd)
		_ = unix.Close(fd)
	}
	g, err := btrfs.Generation(s.work)
	if err != nil {
		return "", err
	}
	return strconv.FormatUint(g, 10), nil
}
