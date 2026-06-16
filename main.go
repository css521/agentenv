//go:build linux

// Command agentenv is a self-hostable, rewindable virtual environment for agents:
// it auto-snapshots the environment, rolls back to any node, and branches so an
// agent can explore several approaches and keep the winner.
//
// This file is the entry point only. It does TWO things:
//  1. Detect when the process was re-executed as the sandbox child (PID 1 of the
//     new namespaces) and hand control to sandbox.Child — this MUST run before
//     any other initialization or Go-side state.
//  2. Otherwise, hand off to the CLI dispatcher (internal/cli).
package main

import (
	"os"
	"runtime/debug"

	"github.com/css521/agentenv/internal/cli"
	"github.com/css521/agentenv/internal/sandbox"
)

// version is overridden at release time via -ldflags "-X main.version=..."
// (goreleaser does this). For `go install github.com/css521/agentenv@latest`
// there are no ldflags, so we fall back to runtime/debug.ReadBuildInfo() which
// gives the module version (e.g. "v0.1.0") or the VCS commit — see resolveVersion.
var version = "dev"

func main() {
	// Re-exec branch: we are the namespaced child that becomes the command.
	if sandbox.IsChild() {
		sandbox.Child() // never returns on success
		return
	}
	// All exit codes flow through here so deferred lock releases inside cli.Run
	// actually fire — the previous code called os.Exit from various error paths,
	// which silently dropped the agentenv repo flock. (See lock.go for context.)
	os.Exit(cli.Run(resolveVersion()))
}

// resolveVersion returns a meaningful version string in all install paths:
//   - goreleaser (ldflags injected) → the release tag, e.g. "v0.1.0"
//   - `go install <module>@vX.Y.Z`  → "vX.Y.Z" from ReadBuildInfo().Main.Version
//   - `go install <module>@latest`  → same, resolved by the toolchain
//   - `go build .` from a checkout  → short VCS commit, e.g. "abc1234"
//   - anything else (no ldflags, no VCS info) → "dev"
func resolveVersion() string {
	if version != "dev" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return version
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" && len(s.Value) >= 7 {
			rev := s.Value[:7]
			for _, s2 := range info.Settings {
				if s2.Key == "vcs.modified" && s2.Value == "true" {
					rev += "-dirty"
				}
			}
			return rev
		}
	}
	return version
}
