//go:build linux

package backend

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/css521/agentenv/internal/sandbox"
)

// Ignore patterns come in two flavors, distinguished by whether they contain a
// "/":
//
//   - ANCHORED (contains "/"): matched against the rootfs-relative path from
//     the root, e.g. "var/lib/apt/lists". Matches the path itself, anything
//     under it, and ".<ext>" siblings (so "x.json" covers "x.json.tmp").
//   - SEGMENT/GLOB (no "/"): matched against EVERY path segment with
//     filepath.Match, e.g. ".claude" ignores any directory named .claude at
//     any depth (~/.claude AND a project's ./.claude); "*.tmp.*" ignores
//     atomic-write temp files anywhere. This is what makes the defaults below
//     "just work" without anyone listing exact paths via env.
//
// alwaysIgnore: never snapshot, regardless of config — runtime mount points the
// sandbox remounts fresh, and agentenv's own pivot_root scratch dir.
var alwaysIgnore = []string{"proc", "sys", "dev", ".pivot_old"}

// baseIgnore: sensible built-in defaults so agents' bookkeeping churn doesn't
// bury the snapshots that matter. AGENTENV_IGNORE EXTENDS this (never replaces).
// Tuned from real Claude Code sessions, but all are generically ephemeral:
//   - ephemeral dirs: tmp, var/tmp, var/cache, var/lib/apt/lists
//   - per-user caches/state: .cache, .npm, .local (any depth)
//   - agent state: .claude (Claude Code's settings/history; rewritten constantly)
//   - atomic-write + editor temp files: *.tmp.*, *.swp, *~ (any depth)
var baseIgnore = []string{
	"tmp", "var/tmp", "var/cache", "var/lib/apt/lists",
	".cache", ".npm", ".local", ".claude",
	"*.tmp.*", "*.swp", "*~",
}

func ignoreList() []string {
	out := append([]string{}, alwaysIgnore...)
	out = append(out, baseIgnore...)
	if v := os.Getenv("AGENTENV_IGNORE"); v != "" {
		for _, p := range strings.Split(v, ",") {
			// '/'-containing patterns are anchored; trim only outer slashes.
			// Segment/glob patterns are kept verbatim.
			if strings.Contains(p, "/") {
				p = strings.Trim(strings.TrimSpace(p), "/")
			} else {
				p = strings.TrimSpace(p)
			}
			if p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

func init() {
	register(candidate{
		name:      "copy",
		priority:  0,
		available: func(string) bool { return true }, // universal fallback
		build:     newCopyBackend,
	})
}

func newCopyBackend(root string) (*Backend, error) {
	s := &copySnap{root: root, nodes: filepath.Join(root, "nodes"), work: filepath.Join(root, "work", "current"), ignore: ignoreList()}
	if err := os.MkdirAll(s.nodes, 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(s.work), 0o755); err != nil {
		return nil, err
	}
	// Probe once at startup: try a reflink (FICLONE) under the snapshot store. On
	// XFS / btrfs / bcachefs / ZFS this is ~free and makes every snapshot near
	// O(1) just like btrfs subvolumes — without needing root or cgo. Falls back
	// silently to byte-copy on ext4 / overlayfs / fuseblk / etc.
	s.reflink = probeReflink(root)
	name := "copy (rootless)"
	if s.reflink {
		name = "copy (rootless +reflink)"
	}
	return &Backend{
		Name:        name,
		Runner:      rootlessRunner{},
		Snapshotter: s,
		// Snapshots are plain copies and Token() walks the tree, so poll/settle
		// slower than btrfs to keep overhead down (especially on network storage).
		PollMillis:     2000,
		DebounceMillis: 3000,
	}, nil
}

// rootlessRunner runs commands in a user namespace (no host privilege required).
type rootlessRunner struct{}

func (rootlessRunner) Run(rootfs string, args, env []string, stdin io.Reader, stdout, stderr io.Writer) error {
	return sandbox.RunRootless(rootfs, args, env, stdin, stdout, stderr)
}

func (rootlessRunner) Start(rootfs string, args, env []string, out io.Writer) (*exec.Cmd, error) {
	return sandbox.StartRootless(rootfs, args, env, out)
}

func (rootlessRunner) Shell(rootfs string, args, env []string) (int, error) {
	return sandbox.RunPTY(rootfs, args, env, true)
}

func (rootlessRunner) ShellHook(rootfs string, args, env []string, onStart func(*exec.Cmd)) (int, error) {
	return sandbox.RunPTYHook(rootfs, args, env, true, onStart)
}

// copySnap snapshots a rootfs by copying directories (no CoW). Snapshots share
// unchanged files with their parent node via hardlinks, so a snapshot costs
// O(changed) data; a restore is a full copy (independent writable tree).
type copySnap struct {
	root    string
	nodes   string
	work    string
	ignore  []string // rootfs-relative paths whose contents are not snapshotted
	reflink bool     // probed once: whether FICLONE succeeds on this fs
}

func (s *copySnap) WorkRoot() string { return s.work }

// Ignored reports whether a rootfs-relative path is under an ignored prefix.
func (s *copySnap) Ignored(rel string) bool { return s.ignored(rel) }

// IgnorePatterns returns the effective ignore patterns (for display).
func (s *copySnap) IgnorePatterns() []string { return s.ignore }

// workspacesDir is the parent of all transient parallel rootfs trees. Workspaces
// live OUTSIDE work/ and nodes/ so they never get caught by status scans, GC, or
// the change watcher (only work/current is watched).
func (s *copySnap) workspacesDir() string { return filepath.Join(s.root, "workspaces") }

// NewWorkspace returns a fresh writable rootfs seeded from fromNodeID. Uses
// reflink when the underlying fs supports it, so creating N parallel workspaces
// is near-free on XFS / btrfs.
func (s *copySnap) NewWorkspace(fromNodeID string) (string, error) {
	if err := os.MkdirAll(s.workspacesDir(), 0o755); err != nil {
		return "", err
	}
	// 16 hex chars of randomness is plenty for transient workspace names.
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	dir := filepath.Join(s.workspacesDir(), hex.EncodeToString(b[:]))
	if err := copyTree(s.NodePath(fromNodeID), dir, "", nil, s.reflink); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

// FreezeFrom is Freeze with an explicit src — Tournament uses it to snapshot
// each parallel workspace as a node without going through work/current.
func (s *copySnap) FreezeFrom(workspacePath, nodeID, parentID string) error {
	dst := s.NodePath(nodeID)
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	linkBase := ""
	if parentID != "" {
		linkBase = s.NodePath(parentID)
	}
	return copyTree(workspacePath, dst, linkBase, s.ignored, s.reflink)
}

// DeleteWorkspace removes a workspace tree.
func (s *copySnap) DeleteWorkspace(path string) error { return os.RemoveAll(path) }

func (s *copySnap) ignored(rel string) bool {
	var segs []string // lazily split for segment/glob patterns
	for _, p := range s.ignore {
		if strings.Contains(p, "/") {
			// Anchored: rel itself, anything under it, or a ".<ext>" sibling
			// (so "a/b.json" also covers "a/b.json.tmp"/".lock").
			if rel == p || strings.HasPrefix(rel, p+"/") || strings.HasPrefix(rel, p+".") {
				return true
			}
			continue
		}
		// Segment/glob: match any path component (basename at any depth).
		if segs == nil {
			segs = strings.Split(rel, "/")
		}
		for _, seg := range segs {
			if seg == p {
				return true
			}
			if ok, _ := filepath.Match(p, seg); ok {
				return true
			}
		}
	}
	return false
}

func (s *copySnap) WorkExists() bool {
	fi, err := os.Stat(s.work)
	return err == nil && fi.IsDir()
}

func (s *copySnap) NewEmptyWork() error {
	if err := os.RemoveAll(s.work); err != nil {
		return err
	}
	return os.MkdirAll(s.work, 0o755)
}

func (s *copySnap) NodePath(id string) string { return filepath.Join(s.nodes, id) }

func (s *copySnap) RestoreWork(nodeID string) error {
	node := s.NodePath(nodeID)
	if _, err := os.Stat(s.work); err != nil {
		// No current working tree: a full, independent copy.
		return copyTree(node, s.work, "", nil, s.reflink)
	}
	// Incremental: make the existing working tree match the node, copying only the
	// differing files and deleting extras — O(changed) data instead of O(total),
	// which matters on network storage. Ignored paths (tmp, caches) are left as-is.
	return syncTree(node, s.work, s.ignored, s.reflink)
}

func (s *copySnap) Freeze(nodeID, parentID string) error {
	dst := s.NodePath(nodeID)
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	linkBase := ""
	if parentID != "" {
		linkBase = s.NodePath(parentID)
	}
	return copyTree(s.work, dst, linkBase, s.ignored, s.reflink)
}

func (s *copySnap) DeleteNode(nodeID string) error { return os.RemoveAll(s.NodePath(nodeID)) }

func (s *copySnap) NodeIDsOnDisk() ([]string, error) {
	entries, err := os.ReadDir(s.nodes)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() {
			ids = append(ids, e.Name())
		}
	}
	return ids, nil
}

// Token fingerprints the working tree (file count + total size + newest mtime).
// It changes whenever a file is added, removed, or modified.
func (s *copySnap) Token() (string, error) {
	var count, size int64
	var maxMod int64
	err := filepath.Walk(s.work, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return nil // skip transient errors during a live tree walk
		}
		if rel, rerr := filepath.Rel(s.work, path); rerr == nil && rel != "." && s.ignored(rel) {
			if fi.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		count++
		if fi.Mode().IsRegular() {
			size += fi.Size()
		}
		if m := fi.ModTime().UnixNano(); m > maxMod {
			maxMod = m
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d:%d:%d", count, size, maxMod), nil
}

// copyTree recursively copies src into dst. When linkBase is non-empty, a regular
// file whose counterpart in linkBase has the same size/mtime/mode is hardlinked
// from linkBase instead of copied (sharing data with the parent node). When
// useReflink is true, files that need to be copied use FICLONE (O(1) on
// supporting fs) before falling back to byte-copy. Char/block devices, FIFOs and
// sockets are skipped (the sandbox provides /dev).
func copyTree(src, dst, linkBase string, ignored func(rel string) bool, useReflink bool) error {
	return filepath.Walk(src, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)

		if ignored != nil && rel != "." && ignored(rel) {
			if fi.IsDir() {
				// Keep the (empty) directory but don't snapshot its contents.
				_ = os.MkdirAll(target, fi.Mode().Perm())
				return filepath.SkipDir
			}
			return nil
		}

		switch {
		case fi.IsDir():
			return os.MkdirAll(target, fi.Mode().Perm())
		case fi.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			_ = os.Remove(target)
			return os.Symlink(link, target)
		case fi.Mode().IsRegular():
			if linkBase != "" {
				base := filepath.Join(linkBase, rel)
				// Hardlinks share the inode (and therefore the mode). If permissions
				// changed we must NOT hardlink-share — copy the file instead.
				if bfi, err := os.Lstat(base); err == nil && bfi.Mode().IsRegular() &&
					bfi.Mode().Perm() == fi.Mode().Perm() &&
					bfi.Size() == fi.Size() && bfi.ModTime().Equal(fi.ModTime()) {
					if os.Link(base, target) == nil {
						return nil // shared with parent node via hardlink
					}
				}
			}
			return copyFile(path, target, fi, useReflink)
		default:
			return nil // skip devices/fifos/sockets
		}
	})
}

// syncTree makes dst match src in place, copying only differing entries and
// removing extras. Paths for which ignored returns true are left untouched in dst
// (and not synced from src). Used for incremental restore. When useReflink is
// true, file copies use FICLONE (O(1)) before falling back to byte-copy.
func syncTree(src, dst string, ignored func(rel string) bool, useReflink bool) error {
	seen := map[string]bool{}
	err := filepath.Walk(src, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil || rel == "." {
			return err
		}
		if ignored != nil && ignored(rel) {
			if fi.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		seen[rel] = true
		target := filepath.Join(dst, rel)
		switch {
		case fi.IsDir():
			if dfi, e := os.Lstat(target); e == nil && !dfi.IsDir() {
				_ = os.RemoveAll(target)
			}
			return os.MkdirAll(target, fi.Mode().Perm())
		case fi.Mode()&os.ModeSymlink != 0:
			link, e := os.Readlink(path)
			if e != nil {
				return e
			}
			if cur, e := os.Readlink(target); e == nil && cur == link {
				return nil
			}
			_ = os.RemoveAll(target)
			return os.Symlink(link, target)
		case fi.Mode().IsRegular():
			// Include mode in the "unchanged" check so chmod-only changes still get
			// restored on checkout (and so the hardlink-share heuristic stays sound).
			if dfi, e := os.Lstat(target); e == nil && dfi.Mode().IsRegular() &&
				dfi.Mode().Perm() == fi.Mode().Perm() &&
				dfi.Size() == fi.Size() && dfi.ModTime().Equal(fi.ModTime()) {
				return nil // unchanged
			}
			_ = os.RemoveAll(target)
			return copyFile(path, target, fi, useReflink)
		default:
			return nil
		}
	})
	if err != nil {
		return err
	}
	// Remove entries present in dst but not in src (and not ignored).
	var remove []string
	filepath.Walk(dst, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		rel, e := filepath.Rel(dst, path)
		if e != nil || rel == "." {
			return nil
		}
		if ignored != nil && ignored(rel) {
			// Returning SkipDir from a NON-directory entry tells filepath.Walk
			// to skip the entire remainder of the parent dir, silently dropping
			// sibling files. SkipDir is only valid on dir entries.
			if fi.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !seen[rel] {
			remove = append(remove, path)
			if fi.IsDir() {
				return filepath.SkipDir
			}
		}
		return nil
	})
	for _, p := range remove {
		_ = os.RemoveAll(p)
	}
	return nil
}

func copyFile(src, dst string, fi os.FileInfo, useReflink bool) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	// Prefer FICLONE (O(1), no data copied) on XFS / btrfs / bcachefs; transparently
	// falls back to byte-copy on ext4 / overlayfs / fuseblk. Preserves mtime so the
	// hardlink-sharing heuristic stays sound across snapshots.
	return copyOrReflink(in, dst, fi.Mode().Perm(), fi.ModTime(), useReflink)
}
