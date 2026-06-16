# Contributing to agentenv

Thanks for your interest! agentenv is a small, dependency-light Go project.

## Development

```bash
# Default build (pure Go, rootless copy backend) — works on any Linux:
CGO_ENABLED=0 go build -o agentenv .

# Optional btrfs backend (needs root + btrfs + libbtrfs-dev):
CGO_ENABLED=1 go build -tags btrfs -o agentenv .
```

Most of agentenv is Linux-only (namespaces, pivot_root). On macOS you can edit and
cross-compile (`GOOS=linux CGO_ENABLED=0 go build ./...`) and run the portable
unit tests, but exercise the runtime in a Linux VM or Docker.

The `.devcontainer/` config is opt-in to a mainland-China apt mirror — set
`AGENTENV_CN_MIRROR=1` on your host before "Reopen in Container" if
deb.debian.org is slow for you; leave it unset everywhere else.

## Before opening a PR

```bash
gofmt -l .                 # must print nothing
go vet ./...
go test ./...              # portable unit tests (dag, image, ...)
```

End-to-end behavior is verified by the scripts in `scripts/` (run in a privileged
or rootless container — see the README "Verify it yourself" section):

- `scripts/verify-rootless.sh` — rootless (non-root, no privileged) path
- `scripts/verify.sh` — privileged btrfs path (`-tags btrfs`)
- `examples/demo/killer-demo.sh` — narrated demo used to record the README GIF

## Guidelines

- Keep external dependencies minimal. Current direct deps:
  `containerd/btrfs/v2` (cgo, `-tags btrfs` only), `creack/pty`,
  `golang.org/x/sys`, `golang.org/x/term`, and
  `modelcontextprotocol/go-sdk` (for `agentenv mcp`).
  New deps need a good reason.
- New environment-specific behavior should go behind the `Runner`/`Snapshotter`
  interfaces in `internal/backend`, keeping the orchestration layer portable.
- Comments and identifiers in English.
- Be honest about limitations in docs (security boundary, performance, etc.).

## Reporting issues

Include: OS/kernel (`uname -a`), whether the pod/host allows unprivileged user
namespaces (`unshare -Urm true; echo $?`), the backend agentenv selected
(visible in `agentenv status`), and the exact command + output.
