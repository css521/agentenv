//go:build linux

package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/css521/agentenv/internal/backend"
	"github.com/css521/agentenv/internal/repo"
)

// ExitError is what a handler returns to ask cli.Run to exit with a specific
// non-zero code AFTER all deferred cleanup (notably the repo flock release)
// has run. cmdExec uses this to transparently propagate the inner command's
// status without calling os.Exit (which would bypass the defers).
type ExitError int

func (e ExitError) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

// command is one CLI subcommand. run receives the args following the command name.
type command struct {
	name    string
	args    string // argument form, for usage
	summary string
	mutates bool // takes the exclusive repo lock
	run     func(r *repo.Repo, args []string) error
}

// commandList is the ordered registry (drives both dispatch and usage). It is kept
// small and role-oriented: one transparent runtime (supervise), one programmatic
// API (daemon), one human entry (shell), one-shot (exec), plus history/inspection.
var commandList = []*command{
	{"init", "--from <dir|/> | --tarball <path|URL>", "create root node by seeding from a dir or extracting a tar(.gz)", true, cmdInit},
	{"supervise", "-- <agent cmd>", "run an unmodified agent inside the env; auto-snapshot; restart on rollback", true, cmdSupervise},
	{"daemon", "[--socket path]", "serve the JSON API over a unix socket (for orchestrators)", true, cmdDaemon},
	{"shell", "", "interactive shell inside the env; auto-snapshots changes", true, cmdShell},
	{"exec", "-- <cmd...>", "run one command in the environment (scripting/CI)", true, cmdExec},
	{"commit", "-m <msg>", "snapshot the environment as a new node", true, cmdCommit},
	{"checkout", "<ref>", "roll the whole environment back to any node (accepts tag/prefix)", true, cmdCheckout},
	{"tag", "[name] [ref]", "list/get/set/delete named refs (e.g. tag winner <id>)", true, cmdTag},
	{"tournament", "[--base <ref>] --test \"<cmd>\" [--keep] -- \"cand1\" ...", "fork base, run each candidate, keep first that passes test", true, cmdTournament},
	{"status", "", "one-screen runtime summary (backend, HEAD, disk, procs, limits)", false, cmdStatus},
	{"log", "", "show the commit-DAG (with branches)", false, cmdLog},
	{"head", "", "print the current HEAD node id", false, cmdHead},
	{"branches", "", "list branch tips (distinct explored end-states)", false, cmdBranches},
	{"show", "<node>", "files this node changed vs its parent", false, cmdShow},
	{"diff", "<a> <b>", "files that differ between two nodes", false, cmdDiff},
	{"delete", "<node>", "remove a node from the DAG (children re-parent to its parent)", true, cmdDelete},
	{"gc", "", "delete orphan snapshots (reclaims sparsified ones)", true, cmdGC},
}

// commands indexes commandList by name. Populated in init() (not a var
// initializer) to avoid an initialization cycle: commandList references the serve
// handler, which references commands.
var commands map[string]*command

func init() {
	commands = make(map[string]*command, len(commandList))
	for _, c := range commandList {
		commands[c.name] = c
	}
}

func usage() {
	fmt.Fprint(os.Stderr, "agentenv — self-hostable rewindable environment for agents\n\nUsage: agentenv <command> [args]\n\n")
	for _, c := range commandList {
		fmt.Fprintf(os.Stderr, "  %-26s %s\n", c.name+" "+c.args, c.summary)
	}
	fmt.Fprintln(os.Stderr, "  ctl <op> [--socket p]      drive a running daemon/supervise out-of-band (e.g. ctl checkout <id>)")
	fmt.Fprintln(os.Stderr, "  mcp [--socket p]           MCP server over stdio (for Claude Code / other MCP hosts)")
	fmt.Fprintln(os.Stderr, "  version                    print version")
	fmt.Fprintln(os.Stderr, `
Env: AGENTENV_ROOT (default /agentfs) — any filesystem (copy backend) or btrfs.
     AGENTENV_BACKEND=copy|btrfs forces a backend; AGENTENV_KEEP_RECENT tunes
     retention; AGENTENV_IGNORE (default tmp,var/tmp,var/cache) excludes paths.`)
}

func rootDir() string {
	if v := os.Getenv("AGENTENV_ROOT"); v != "" {
		return v
	}
	return "/agentfs"
}

// openBackend selects the backend (AGENTENV_BACKEND override, else auto-detect).
func openBackend(root string) (*backend.Backend, error) {
	if name := os.Getenv("AGENTENV_BACKEND"); name != "" {
		return backend.ForceName(root, name)
	}
	return backend.Detect(root)
}

// after returns the args following sep, or nil if sep is absent.
func after(argv []string, sep string) []string {
	for i, a := range argv {
		if a == sep {
			return argv[i+1:]
		}
	}
	return nil
}

// flagValue returns the value of flag, supporting both forms:
//
//	--flag value   (next argv element)
//	--flag=value   (single argv element with = separator)
//
// Returns "" when the flag is absent or has no value.
func flagValue(argv []string, flag string) string {
	prefix := flag + "="
	for i, a := range argv {
		if a == flag && i+1 < len(argv) {
			return argv[i+1]
		}
		if strings.HasPrefix(a, prefix) {
			return a[len(prefix):]
		}
	}
	return ""
}

// hasHelpFlag reports whether -h / --help appears in argv (before any "--", so
// `agentenv exec -- bash --help` still passes --help to the inner command).
func hasHelpFlag(argv []string) bool {
	for _, a := range argv {
		if a == "--" {
			return false
		}
		if a == "-h" || a == "--help" {
			return true
		}
	}
	return false
}

// printChanges renders a diff with a summary, capping output so a large (e.g.
// root) diff does not flood the terminal.
func printChanges(ch []repo.Change) {
	const limit = 200
	var add, del, mod int
	for _, c := range ch {
		switch c.Kind {
		case '+':
			add++
		case '-':
			del++
		case 'M':
			mod++
		}
	}
	for i, c := range ch {
		if i == limit {
			fmt.Printf("... (%d more)\n", len(ch)-limit)
			break
		}
		fmt.Printf("%c %s\n", c.Kind, c.Path)
	}
	fmt.Printf("%d changed: +%d added, -%d removed, M%d modified\n", len(ch), add, del, mod)
}
