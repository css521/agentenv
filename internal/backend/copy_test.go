//go:build linux

package backend

import "testing"

func TestIgnoredMatching(t *testing.T) {
	s := &copySnap{ignore: []string{"proc", "tmp", "var/cache", "root/.claude"}}

	cases := []struct {
		rel  string
		want bool
	}{
		// exact dir match + anything under it
		{"proc", true},
		{"proc/1/status", true},
		{"tmp", true},
		{"tmp/scratch.txt", true},
		{"var/cache/apt/archives/x.deb", true},
		// the entry itself as a dir
		{"root/.claude", true},
		{"root/.claude/settings.json", true},
		// sibling files "owned" by the entry via extension — the key fix for
		// agents writing atomic temp/lock files next to a config
		{"root/.claude.json", true},
		{"root/.claude.json.lock", true},
		{"root/.claude.json.tmp.8.0ee0fa832c02", true},
		// must NOT over-match: different top-level names that merely share a prefix
		{"root/.clauderc", false}, // no dot/slash boundary after ".claude"
		{"procfs", false},
		{"tmpfile", false},
		{"var/cacheable", false},
		{"home/user/file", false},
		{"root/work/main.go", false},
	}
	for _, c := range cases {
		if got := s.ignored(c.rel); got != c.want {
			t.Errorf("ignored(%q) = %v, want %v", c.rel, got, c.want)
		}
	}
}

func TestIgnoreListAlwaysAndExtends(t *testing.T) {
	// alwaysIgnore + baseIgnore are always present; AGENTENV_IGNORE extends.
	t.Setenv("AGENTENV_IGNORE", "root/.claude,custom/dir")
	list := ignoreList()
	has := func(p string) bool {
		for _, x := range list {
			if x == p {
				return true
			}
		}
		return false
	}
	for _, must := range []string{".pivot_old", "proc", "sys", "dev", "tmp", "var/cache", "root/.claude", "custom/dir"} {
		if !has(must) {
			t.Errorf("ignoreList() missing %q; got %v", must, list)
		}
	}
}
