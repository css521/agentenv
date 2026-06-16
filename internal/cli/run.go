//go:build linux

// Package cli implements the agentenv command-line interface — the command
// registry, one-shot handlers (commands.go), long-running modes (session.go),
// the out-of-band socket client (ctl.go), and shared helpers (cli.go). main.go
// calls Run() after handling the sandbox re-exec branch.
package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/css521/agentenv/internal/repo"
)

// Run parses os.Args[1:], dispatches, and returns the exit code main should
// surface. It does NOT call os.Exit itself — that bypasses deferred Release()
// of the repo lock (and any other defers a handler set up). main() is the only
// place allowed to terminate the process.
func Run(version string) int {
	args := os.Args[1:]
	if len(args) == 0 {
		usage()
		return 2
	}
	name, rest := args[0], args[1:]

	switch name {
	case "version", "--version", "-v":
		fmt.Println("agentenv", version)
		return 0
	case "help", "--help", "-h":
		usage()
		return 0
	case "ctl":
		// Pure socket client — no repo/lock — drives a running daemon/supervise.
		if err := cmdCtl(rest); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		return 0
	case "mcp":
		// MCP server (stdio JSON-RPC) — bridges Claude Code / other MCP hosts
		// to a running daemon. No repo/lock here either: it forwards every
		// call as a fresh socket round-trip, so the daemon owns concurrency.
		// `version` flows from main.resolveVersion → the MCP initialize
		// handshake's serverInfo.version, so hosts log which build they're
		// talking to.
		if err := cmdMCP(rest, version); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		return 0
	}

	cmd := commands[name]
	if cmd == nil {
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", name)
		usage()
		return 2
	}

	// Per-subcommand help: `agentenv <cmd> -h / --help` prints just that
	// command's usage line. Always cheap; runs before opening the repo / lock.
	if hasHelpFlag(rest) {
		fmt.Printf("agentenv %s %s\n  %s\n", cmd.name, cmd.args, cmd.summary)
		return 0
	}

	root := rootDir()

	// CRITICAL ORDERING: take the lock BEFORE opening the repo. Otherwise
	// `repo.Open` reads meta.json during a window where another mutating
	// session can be writing it, producing a torn read or an inconsistent
	// in-memory DAG. Read-only commands skip the lock (they only read, they
	// don't write back, so a slightly stale view is acceptable).
	var lock *repo.Lock
	if cmd.mutates {
		var err error
		lock, err = repo.AcquireLock(root)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		defer lock.Release()
	}

	be, err := openBackend(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	r, err := repo.Open(root, be)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}

	if err := cmd.run(r, rest); err != nil {
		// ExitError = "the inner command produced a non-zero exit; propagate it
		// transparently". Anything else is a real agentenv error.
		var ee ExitError
		if errors.As(err, &ee) {
			return int(ee)
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}
