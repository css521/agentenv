//go:build linux

package repo

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Change is one path that differs between two nodes.
type Change struct {
	Kind byte // '+' added, '-' removed, 'M' modified
	Path string
}

// Diff lists the paths that differ between node aID and node bID (either may
// be "" to mean an empty tree). Accepts tag names or unique ID prefixes; ref
// resolution and DAG access happen UNDER the same opMu so a concurrent commit
// or retention pass can't change the resolution between resolve and use.
func (r *Repo) Diff(aRef, bRef string) ([]Change, error) {
	r.opMu.Lock()
	defer r.opMu.Unlock()
	var a, b string
	if aRef != "" {
		a = r.resolveRefLocked(aRef)
		if a == "" {
			return nil, fmt.Errorf("unknown ref: %s", aRef)
		}
	}
	if bRef != "" {
		b = r.resolveRefLocked(bRef)
		if b == "" {
			return nil, fmt.Errorf("unknown ref: %s", bRef)
		}
	}
	return r.diffCore(a, b)
}

// Show returns what a node changed relative to its parent (like `git show --stat`),
// along with the parent ID ("" if it is the root). Accepts refs. Same lock
// discipline as Diff: resolve + read happen atomically.
func (r *Repo) Show(ref string) ([]Change, string, error) {
	r.opMu.Lock()
	defer r.opMu.Unlock()
	id := r.resolveRefLocked(ref)
	if id == "" {
		return nil, "", fmt.Errorf("unknown ref: %s", ref)
	}
	n := r.dag.Nodes[id]
	ch, err := r.diffCore(n.Parent, id)
	return ch, n.Parent, err
}

func (r *Repo) diffCore(aID, bID string) ([]Change, error) {
	var aPath, bPath string
	if aID != "" {
		aPath = r.be.Snapshotter.NodePath(aID)
	}
	if bID != "" {
		bPath = r.be.Snapshotter.NodePath(bID)
	}
	ai, err := indexTree(aPath)
	if err != nil {
		return nil, err
	}
	bi, err := indexTree(bPath)
	if err != nil {
		return nil, err
	}

	var out []Change
	for p, bsig := range bi {
		if asig, ok := ai[p]; !ok {
			out = append(out, Change{'+', p})
		} else if asig != bsig {
			out = append(out, Change{'M', p})
		}
	}
	for p := range ai {
		if _, ok := bi[p]; !ok {
			out = append(out, Change{'-', p})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// indexTree maps each path under root to a signature (mode+size, plus mtime for
// regular files and target for symlinks) so changes are detected cheaply.
func indexTree(root string) (map[string]string, error) {
	idx := map[string]string{}
	if root == "" {
		return idx, nil
	}
	err := filepath.Walk(root, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return nil // tolerate transient walk errors
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == "." {
			return nil
		}
		sig := fmt.Sprintf("%v:%d", fi.Mode(), fi.Size())
		if fi.Mode().IsRegular() {
			sig += fmt.Sprintf(":%d", fi.ModTime().UnixNano())
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			if t, e := os.Readlink(path); e == nil {
				sig += ":->" + t
			}
		}
		idx[rel] = sig
		return nil
	})
	return idx, err
}
