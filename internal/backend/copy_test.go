//go:build linux

package backend

import "testing"

func TestIgnoredMatching(t *testing.T) {
	// Mix of anchored (with "/") and segment/glob (no "/") patterns.
	s := &copySnap{ignore: []string{"var/lib/apt/lists", ".claude*", ".cache", "*.tmp.*"}}

	cases := []struct {
		rel  string
		want bool
	}{
		// anchored: the path, anything under it, and ".<ext>" siblings
		{"var/lib/apt/lists", true},
		{"var/lib/apt/lists/lock", true},
		// segment/glob: a named dir at ANY depth (~/.claude AND ./.claude)
		{".claude", true},
		{".claude/settings.local.json", true},
		{"root/.claude", true},
		{"root/.claude/history.jsonl", true},
		{"work/.claude/settings.local.json", true},
		{"home/vscode/.cache/x", true},
		// glob: atomic-write temp files anywhere
		{"hello.py.tmp.9.08cb3a25d56c", true},
		{"work/hello.py.tmp.9.08cb3a25d56c", true},
		{"root/.claude.json", true},                // .claude* matches the file too
		{"root/.claude.json.lock", true},           // .claude* covers the lock sibling
		{"root/.claude.json.tmp.8.0ee0fa83", true}, // *.tmp.* (and .claude*)
		// must NOT over-match
		{"work/hello.py", false},
		{"var/lib/apt/extended_states", false},
		{"home/user/notes.md", false},
	}
	for _, c := range cases {
		if got := s.ignored(c.rel); got != c.want {
			t.Errorf("ignored(%q) = %v, want %v", c.rel, got, c.want)
		}
	}
}

func TestIgnoreListAlwaysAndExtends(t *testing.T) {
	// alwaysIgnore + baseIgnore are always present; AGENTENV_IGNORE extends.
	t.Setenv("AGENTENV_IGNORE", "myproj/cache,node_modules")
	list := ignoreList()
	has := func(p string) bool {
		for _, x := range list {
			if x == p {
				return true
			}
		}
		return false
	}
	for _, must := range []string{
		// always
		".pivot_old", "proc", "sys", "dev",
		// base defaults
		"tmp", "var/cache", "var/lib/apt/lists", ".claude*", ".cache", "*.tmp.*",
		// user extensions
		"myproj/cache", "node_modules",
	} {
		if !has(must) {
			t.Errorf("ignoreList() missing %q; got %v", must, list)
		}
	}
}
