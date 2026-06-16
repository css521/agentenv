package dag

import (
	"testing"
	"time"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()

	r, err := Load(dir)
	if err != nil {
		t.Fatalf("Load empty: %v", err)
	}
	if len(r.Nodes) != 0 {
		t.Fatalf("fresh repo should be empty, got %d nodes", len(r.Nodes))
	}

	root := &Node{ID: "aaa", Message: "root", CreatedAt: time.Unix(1, 0)}
	child := &Node{ID: "bbb", Parent: "aaa", Message: "child", CreatedAt: time.Unix(2, 0)}
	r.Add(root)
	r.Add(child)
	r.Head = "bbb"
	if err := r.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	r2, err := Load(dir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if r2.Head != "bbb" {
		t.Errorf("Head = %q, want bbb", r2.Head)
	}
	if len(r2.Nodes) != 2 {
		t.Fatalf("reloaded nodes = %d, want 2", len(r2.Nodes))
	}
	if n, ok := r2.Get("bbb"); !ok || n.Parent != "aaa" {
		t.Errorf("child parent = %q, want aaa", n.Parent)
	}
}

func TestAddLinksParentChild(t *testing.T) {
	r, _ := Load(t.TempDir())
	r.Add(&Node{ID: "p"})
	r.Add(&Node{ID: "c1", Parent: "p"})
	r.Add(&Node{ID: "c2", Parent: "p"})

	p, _ := r.Get("p")
	if len(p.Children) != 2 {
		t.Fatalf("parent children = %d, want 2", len(p.Children))
	}
}

func TestRootsSortedByTime(t *testing.T) {
	r, _ := Load(t.TempDir())
	r.Add(&Node{ID: "late", CreatedAt: time.Unix(20, 0)})
	r.Add(&Node{ID: "early", CreatedAt: time.Unix(10, 0)})
	r.Add(&Node{ID: "child", Parent: "early", CreatedAt: time.Unix(30, 0)})

	roots := r.Roots()
	if len(roots) != 2 {
		t.Fatalf("roots = %d, want 2 (child is not a root)", len(roots))
	}
	if roots[0].ID != "early" || roots[1].ID != "late" {
		t.Errorf("roots not time-sorted: %s, %s", roots[0].ID, roots[1].ID)
	}
}
