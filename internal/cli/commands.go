//go:build linux

package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/css521/agentenv/internal/daemonclient"
	"github.com/css521/agentenv/internal/mcp"
	"github.com/css521/agentenv/internal/repo"
)

// cmdMCP runs the MCP (Model Context Protocol) server on stdio. Claude Code
// (and any other MCP host) spawns this binary, sends JSON-RPC on stdin, reads
// responses on stdout. The server is a thin bridge: every tool call becomes a
// single round-trip on the daemon's unix socket. So the typical setup is:
//
//  1. start the daemon once:    agentenv daemon
//  2. point Claude Code at:     {"command":"agentenv","args":["mcp"]}
//
// Sockets are resolved via --socket / AGENTENV_SOCKET / AGENTENV_ROOT in that
// order. Cancelled by SIGINT/SIGTERM (typical: Claude Code closes stdin).
func cmdMCP(args []string, version string) error {
	sock := daemonclient.SocketPath(flagValue(args, "--socket"))
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	return mcp.Serve(ctx, sock, version)
}

// cmdInit creates the root node from one of two sources, exactly one of which
// must be specified:
//
//	--from <dir|/>      seed the rootfs from an existing directory tree (the
//	                    "wrap my container" path; what Dockerfile.control uses)
//	--tarball <p|URL>   extract a .tar or .tar.gz from a local path or http(s)
//	                    URL (handy for demos and air-gapped setups)
//
// We deliberately do NOT scrape Ubuntu's mirror or pull from container
// registries — that surface area was brittle (HTML changes break it), needed
// network from the agent host, and didn't carry its weight for the project's
// "self-hosted, runs in restricted environments" positioning. Users who want a
// container image rootfs can `docker export` it to a tarball and pass it via
// --tarball.
func cmdInit(r *repo.Repo, args []string) error {
	from := flagValue(args, "--from")
	tarball := flagValue(args, "--tarball")
	switch {
	case from != "" && tarball != "":
		return fmt.Errorf("--from and --tarball are mutually exclusive")
	case from != "":
		n, err := r.InitFrom(from)
		if err != nil {
			return err
		}
		fmt.Printf("initialized root node %s (seeded from %s)\n", n.ID, from)
		return nil
	case tarball != "":
		n, err := r.InitTarball(tarball)
		if err != nil {
			return err
		}
		fmt.Printf("initialized root node %s (extracted from %s)\n", n.ID, tarball)
		return nil
	default:
		return fmt.Errorf("usage: agentenv init --from <dir> | --tarball <path-or-URL>")
	}
}

func cmdExec(r *repo.Repo, args []string) error {
	cmd := after(args, "--")
	if len(cmd) == 0 {
		return fmt.Errorf("usage: agentenv exec -- <cmd> [args...]")
	}
	code, err := r.Exec(cmd, os.Stdin, os.Stdout, os.Stderr)
	if err != nil {
		return err
	}
	// Propagate the inner command's exit code as a typed error so cli.Run can
	// translate it into the process's exit code AFTER deferred Release of the
	// repo lock fires. The previous code called os.Exit(code) here directly,
	// which bypassed the defer and silently left the flock held until kernel
	// cleanup.
	if code != 0 {
		return ExitError(code)
	}
	return nil
}

func cmdCommit(r *repo.Repo, args []string) error {
	n, err := r.Commit(flagValue(args, "-m"), "")
	if err != nil {
		return err
	}
	fmt.Printf("committed %s  %q\n", n.ID, n.Message)
	return nil
}

func cmdCheckout(r *repo.Repo, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: agentenv checkout <node-id>")
	}
	if err := r.Checkout(args[0]); err != nil {
		return err
	}
	fmt.Printf("HEAD is now %s\n", r.Head())
	return nil
}

func cmdLog(r *repo.Repo, _ []string) error {
	fmt.Print(r.Tree())
	return nil
}

func cmdHead(r *repo.Repo, _ []string) error {
	fmt.Println(r.Head())
	return nil
}

func cmdBranches(r *repo.Repo, _ []string) error {
	head := r.Head()
	for _, n := range r.Leaves() {
		marker := ""
		if n.ID == head {
			marker = "  <- HEAD"
		}
		fmt.Printf("%s  %q%s\n", n.ID, n.Message, marker)
	}
	return nil
}

func cmdShow(r *repo.Repo, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: agentenv show <node-id>")
	}
	ch, parent, err := r.Show(args[0])
	if err != nil {
		return err
	}
	if parent == "" {
		fmt.Println("(root node — full base rootfs)")
	}
	printChanges(ch)
	return nil
}

func cmdDiff(r *repo.Repo, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: agentenv diff <node-a> <node-b>")
	}
	ch, err := r.Diff(args[0], args[1])
	if err != nil {
		return err
	}
	printChanges(ch)
	return nil
}

func cmdStatus(r *repo.Repo, _ []string) error {
	s := r.Status()
	headShort := s.Head
	if len(headShort) > 12 {
		headShort = headShort[:12]
	}
	ign := "(none)"
	if len(s.Ignore) > 0 {
		ign = strings.Join(s.Ignore, ", ")
	}
	maxWatches := readUintFile("/proc/sys/fs/inotify/max_user_watches")

	fmt.Println("agentenv")
	// tabwriter aligns the value column automatically — no more %-26s magic that
	// breaks the moment a row label gets renamed.
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintf(w, "  backend\t%s\n", s.Backend)
	fmt.Fprintf(w, "  root\t%s  (%s on disk)\n", s.Root, humanBytes(s.DiskBytes))
	fmt.Fprintf(w, "  HEAD\t%s\n", headShort)
	fmt.Fprintf(w, "  nodes\t%d total, %d branch tip(s)\n", s.NodeCount, s.LeafCount)
	fmt.Fprintf(w, "  procs (tracked)\t%d\n", s.ProcCount)
	fmt.Fprintf(w, "  capture cadence\tpoll=%dms  debounce=%dms\n", s.PollMs, s.DebounceMs)
	fmt.Fprintf(w, "  ignore\t%s\n", ign)
	fmt.Fprintf(w, "  inotify limit\t%d watches (per user, from /proc/sys/fs/inotify/max_user_watches)\n", maxWatches)
	return w.Flush()
}

// readUintFile reads a single number from /proc (returns 0 on failure).
func readUintFile(path string) uint64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	var n uint64
	for _, c := range strings.TrimSpace(string(b)) {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + uint64(c-'0')
	}
	return n
}

func humanBytes(n int64) string {
	const k = 1024
	switch {
	case n < k:
		return fmt.Sprintf("%dB", n)
	case n < k*k:
		return fmt.Sprintf("%.1fKB", float64(n)/k)
	case n < k*k*k:
		return fmt.Sprintf("%.1fMB", float64(n)/(k*k))
	default:
		return fmt.Sprintf("%.2fGB", float64(n)/(k*k*k))
	}
}

// cmdTag manages named refs.
//   - no args        → list all tags
//   - <name>         → print the node id that <name> resolves to
//   - <name> <ref>   → set <name> to point to <ref> (a node id or another tag)
//   - <name> ""      → delete the tag
func cmdTag(r *repo.Repo, args []string) error {
	switch len(args) {
	case 0:
		tags := r.Tags()
		if len(tags) == 0 {
			fmt.Println("(no tags)")
			return nil
		}
		// stable order
		var names []string
		for n := range tags {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			fmt.Printf("%-20s %s\n", n, tags[n])
		}
		return nil
	case 1:
		id := r.ResolveRef(args[0])
		if id == "" {
			return fmt.Errorf("unknown ref: %s", args[0])
		}
		fmt.Println(id)
		return nil
	default:
		return r.SetTag(args[0], args[1])
	}
}

// cmdTournament: forks N branches from a base, runs each candidate, then runs a
// test command in each. Keeps the first branch whose test passes.
//
//	agentenv tournament --base <ref|HEAD> --test "<cmd>" [--keep] -- "cmd1" "cmd2" ...
func cmdTournament(r *repo.Repo, args []string) error {
	base := flagValue(args, "--base")
	test := flagValue(args, "--test")
	if test == "" {
		return fmt.Errorf("usage: agentenv tournament [--base <ref>] --test \"<cmd>\" [--keep] -- \"cand1\" \"cand2\"")
	}
	cands := after(args, "--")
	if len(cands) == 0 {
		return fmt.Errorf("provide at least one candidate after --")
	}
	keep := false
	for _, a := range args {
		if a == "--keep" {
			keep = true
		}
	}

	pairs := make([]struct{ Name, Cmd string }, len(cands))
	for i, c := range cands {
		pairs[i] = struct{ Name, Cmd string }{Name: fmt.Sprintf("%c", 'A'+i), Cmd: c}
	}

	res, err := r.Tournament(base, test, pairs, keep)
	if err != nil {
		return err
	}
	fmt.Printf("base: %s\n", res.Base)
	for _, c := range res.Candidates {
		verdict := fmt.Sprintf("exit %d", c.TestExit)
		if c.TestExit == 0 {
			verdict = "PASS"
		}
		// Clip the inline [cmd] so the summary stays one line per branch even
		// when candidates are long shell pipelines (use show <node> for the full
		// command). 12-char node id; ~50-char cmd fits ~100-col terminals.
		cmd := c.Cmd
		if len(cmd) > 50 {
			cmd = cmd[:49] + "…"
		}
		fmt.Printf("  branch %s  →  %s  (%s)  [%s]\n", c.Name, c.Node, verdict, cmd)
	}
	if res.Winner == "" {
		fmt.Println("no candidate passed the test")
		return nil
	}
	fmt.Printf("winner: branch %s  (node %s)\n", res.Winner, res.WinnerNode)
	if keep {
		fmt.Printf("HEAD is now %s\n", r.Head())
	}
	return nil
}

func cmdGC(r *repo.Repo, _ []string) error {
	removed, err := r.GC()
	if err != nil {
		return err
	}
	fmt.Printf("removed %d orphan snapshots: %v\n", len(removed), removed)
	return nil
}
