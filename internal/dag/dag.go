// Package dag maintains the commit-DAG of environment snapshots.
//
// Design:
//   - Each Node maps to one read-only btrfs snapshot (an immutable state).
//   - A single parent per node is enough for environment rollback, so this is a
//     tree; extend to a real DAG later if merges are ever needed.
//   - Metadata is persisted as a single JSON file (meta.json): single writer,
//     zero external dependencies.
package dag

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Node is an immutable node in the DAG, i.e. one commit.
type Node struct {
	ID        string            `json:"id"`
	Parent    string            `json:"parent"`   // empty for the root node
	Children  []string          `json:"children"` // child IDs, forming the tree
	Message   string            `json:"message"`  // git-like commit message
	Command   string            `json:"command"`  // action that produced it, e.g. "apt install nginx"
	CreatedAt time.Time         `json:"created_at"`
	Meta      map[string]string `json:"meta,omitempty"`
}

// Repo is the whole commit graph plus the current HEAD and named tags.
//
// CONCURRENCY CONTRACT: Repo is NOT goroutine-safe on its own. All callers
// (currently the orchestration layer in internal/repo) MUST hold a higher-level
// mutex — repo.Repo.opMu — across any read or write of Nodes / Tags / Head.
// This is intentional: the agentenv DAG is small and updates are infrequent,
// so a single repo-wide lock is simpler than embedding a mutex here and trying
// to keep the lock-ordering between dag.Repo and the snapshotter consistent.
// The doc comment exists so a future caller from a new package doesn't assume
// dag.Repo is safe to share.
type Repo struct {
	Nodes map[string]*Node  `json:"nodes"`
	Head  string            `json:"head"`           // node the current working subvolume derives from
	Tags  map[string]string `json:"tags,omitempty"` // human name → node ID

	path string // path to meta.json, not serialized
}

// Load reads root/meta.json, returning an empty repo if it does not exist.
func Load(root string) (*Repo, error) {
	p := filepath.Join(root, "meta.json")
	r := &Repo{
		Nodes: map[string]*Node{},
		Tags:  map[string]string{},
		path:  p,
	}
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, r); err != nil {
		return nil, fmt.Errorf("parse meta.json: %w", err)
	}
	r.path = p
	if r.Nodes == nil {
		r.Nodes = map[string]*Node{}
	}
	if r.Tags == nil {
		r.Tags = map[string]string{}
	}
	return r, nil
}

// Save writes meta.json atomically and crash-safely. meta.json is the only
// source of truth for the commit DAG (every node, parent, tag, HEAD), so a torn
// or zero-byte file would lose the entire history. Sequence:
//
//  1. write the new contents to a sibling temp file,
//  2. fsync the temp file so the bytes are durably on disk,
//  3. rename → meta.json (atomic on POSIX),
//  4. fsync the parent directory so the rename itself is durable.
//
// Steps 2 and 4 are what plain `os.WriteFile`+`os.Rename` is missing — after a
// kernel/host crash without them, the rename can land first and the data later,
// or the new file can be empty.
func (r *Repo) Save() error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp := r.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil { // (2) flush data + metadata of the new file
		f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, r.path); err != nil { // (3) atomic swap into place
		_ = os.Remove(tmp)
		return err
	}
	// (4) fsync the parent so the new directory entry survives a crash. Best-
	// effort: not all filesystems require it (and some refuse fsync on dirs).
	if d, err := os.Open(filepath.Dir(r.path)); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}

// Add attaches a node under its parent and inserts it into the graph.
func (r *Repo) Add(n *Node) {
	r.Nodes[n.ID] = n
	if n.Parent != "" {
		if p := r.Nodes[n.Parent]; p != nil {
			p.Children = append(p.Children, n.ID)
		}
	}
}

// Get returns a node by ID.
func (r *Repo) Get(id string) (*Node, bool) {
	n, ok := r.Nodes[id]
	return n, ok
}

// Delete removes node id from the graph, splicing it out: its children are
// re-parented to id's parent (so the tree stays connected — deleting a middle
// node keeps its descendants), and any tags pointing at id are dropped. The
// caller is responsible for the policy checks (not HEAD, not the only node) and
// for removing the on-disk snapshot. Returns id's former parent ("" if it was a
// root) and whether the node existed.
func (r *Repo) Delete(id string) (parent string, ok bool) {
	n, ok := r.Nodes[id]
	if !ok {
		return "", false
	}
	parent = n.Parent
	for _, cid := range n.Children {
		c := r.Nodes[cid]
		if c == nil {
			continue
		}
		c.Parent = parent
		if parent != "" {
			if p := r.Nodes[parent]; p != nil {
				p.Children = append(p.Children, cid)
			}
		}
	}
	if parent != "" {
		if p := r.Nodes[parent]; p != nil {
			p.Children = removeID(p.Children, id)
		}
	}
	delete(r.Nodes, id)
	for name, tid := range r.Tags {
		if tid == id {
			delete(r.Tags, name)
		}
	}
	return parent, true
}

func removeID(ids []string, id string) []string {
	out := ids[:0]
	for _, x := range ids {
		if x != id {
			out = append(out, x)
		}
	}
	return out
}

// Roots returns all nodes without a parent (normally just one).
func (r *Repo) Roots() []*Node {
	var out []*Node
	for _, n := range r.Nodes {
		if n.Parent == "" {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}
