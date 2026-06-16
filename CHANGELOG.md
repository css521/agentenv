# Changelog

All notable changes to this project are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/), and the project aims to follow
[Semantic Versioning](https://semver.org/).

## [0.1.0] - 2026-06-16

First public release. The whole rewindable-environment story end-to-end —
copy + btrfs backends, auto-capture, transparent supervise, branch
tournament, and an MCP server that lets Claude Code (and any MCP host)
drive rollback natively.

### Added
- **MCP server** (`agentenv mcp`) — Model Context Protocol server over
  stdio, built on the official `github.com/modelcontextprotocol/go-sdk`,
  exposing 6 tools (`agentenv__head`/`log`/`branches`/`show`/`diff`/
  `checkout`) that bridge to a running `agentenv daemon`. With this an
  MCP host can ask "where am I in history?" and "undo to here" without
  any custom integration. Error responses carry an anti-hallucination
  prefix so the model can't quietly fabricate a result after a tool failure.
- **`internal/daemonclient`** package — the in-process client for the
  daemon's unix-socket protocol. `ctl` and the MCP server now share one
  implementation (Dial / Recv / Roundtrip / Drain / SocketPath); a single
  fix to the protocol propagates to both surfaces.
- **Cross-uid diagnostics** — connecting to a daemon socket owned by
  another uid now reports both uids and tells you to align them, instead
  of surfacing a bare "permission denied". Daemon startup logs the uid
  + socket mode on the first line of stderr.
- **`agentenv ctl exec` propagates inner exit codes** via a typed
  `ExitError`, so scripts like `agentenv ctl exec "test -f X" && ...`
  actually branch on the inner command. Previously the daemon's exit code
  only reached stderr as `[exit N]` and `ctl` returned 0 regardless.
- **Docker smoke harnesses** under `verify/docker/`:
  `mcp-smoke.sh` (protocol-level: drives `agentenv mcp` with real
  JSON-RPC and asserts 6 tools + tool calls); `rollback-smoke.sh`
  (end-to-end: mutates rootfs, MCP-driven checkout, asserts files
  actually roll back both ways); `claude-shell` (interactive Claude
  Code session with daemon + MCP preconfigured).
- Rewindable environment with a content-tree (commit-DAG) of snapshots.
- Pluggable backends with startup capability probe:
  - `copy` (rootless): user-namespace runner + plain-copy snapshots; no privilege,
    runs in a restricted Kubernetes pod (default, pure-Go static build).
  - `btrfs` (privileged): copy-on-write subvolume snapshots (`-tags btrfs`).
- Auto-capture: the environment versions itself on any change (shell commands or
  direct file edits), debounced; no explicit commit required.
- Commands: `init` (incl. `--from <dir|/>`), `exec`, `spawn`, `shell`, `serve`,
  `watch`, `commit`, `checkout`, `log`, `head`, `branches`, `show`, `diff`,
  `retain`, `gc`.
- Branch exploration: fork the environment, try approaches in parallel branches,
  keep the winner.
- Retention (DVR-style sparsification) bounding history; `gc` reclaims disk.
- Cross-process lock and open-time reconciliation of dangling snapshots.
- `agentenv supervise -- <agent>`: runs an unmodified agent INSIDE the managed
  environment (any command/language/path), auto-snapshots everything, and on an
  out-of-band rollback kills and restarts the agent from the restored environment.
- `Dockerfile.control` + entrypoint: wrap any agent image so its whole environment
  is rewindable with zero agent changes (`init --from /` + `supervise`).
- `agentenv daemon`: newline-JSON API over a unix socket (returns each command's
  snapshot node id); clients in `examples/` for Python, Go, and Java.
- Trimmed the CLI to a small role-oriented set (removed `serve`/`watch`/`spawn`/
  `ps`/`kill`/`retain`; their behavior is covered by supervise/daemon/auto-capture).
- `agentenv status`: one-screen runtime summary (backend, HEAD, disk, procs,
  capture cadence, ignore prefixes, inotify watch limit).
- `agentenv tag`: list/get/set named refs (e.g. `tag winner <id>`); `checkout`
  accepts tag names and unique ID prefixes (not just full IDs).
- `agentenv tournament`: forks N candidate branches from a base, runs each plus a
  test command, keeps the first branch that passes — the branch-exploration
  primitive as a single command (also via daemon op `tournament` + ctl).
- copy backend: snapshots now respect file mode (chmod-only changes get restored
  on checkout; hardlink-share heuristic only shares when mode matches).
- inotify: gracefully detects watch-limit (ENOSPC) and falls back to Token
  polling with a one-time stderr warning.
- copy backend: probes `FICLONE` at startup and uses it for file copies on
  supporting filesystems (XFS, btrfs, bcachefs, ZFS, modern overlay) — snapshots
  become near O(1) instead of O(file-size). `agentenv status` shows `+reflink`
  when the fast path is active; transparent byte-copy fallback otherwise.
- `daemon` exec is now streaming: stdout/stderr arrive as NDJSON frames as the
  inner command produces them, ending with a terminal `{"ok":true,"exit":N,
  "node":"..."}` frame. Long builds/tests show progress to agent harnesses
  instead of going silent until completion. `agentenv ctl exec ...` prints
  output live; the Python/Go example clients read the streamed protocol.
- **PTY**: hand-rolled `/dev/ptmx` / TIOCSPTLCK / TIOCGPTN / termios bit-flip
  code (~140 LOC of brittle ioctl wrangling) replaced by `creack/pty` +
  `golang.org/x/term` (~30 LOC of well-tested calls). Same behavior, far less
  surface area for terminal bugs.
- **CLI**: `--flag=value` form accepted in addition to `--flag value`;
  per-subcommand `-h` / `--help` prints just that command's usage.
- **Correctness fix** (`dag.Save`): meta.json is now written with full
  fsync(tmp) + atomic rename + fsync(parent). A kernel/host crash mid-write
  used to be able to leave meta.json zero-sized, losing the entire commit DAG.
- **Bug fix** (`internal/image`): `http.Client` now has timeouts (10 min total,
  30 s for response headers). Previously a hung Ubuntu mirror would make
  `agentenv init` block forever instead of failing fast.
- **API server**: switched from `bufio.Scanner` (16 MB line cap, silent
  truncation) to `json.Decoder` over the socket — no max-line ceiling, cleaner
  error surface.
- **Shutdown**: daemon/supervise use `signal.NotifyContext` instead of a manual
  signal channel + relay goroutine; the same context drives api.Serve, the
  supervise loop, and tailFile, so SIGINT/SIGTERM tear everything down at once.
- **Output**: `agentenv status` uses `text/tabwriter` for column alignment.
- inotify-based change detection (event-driven, cheap idle) with a Token backstop;
  incremental checkout (copy only the diff); `AGENTENV_IGNORE` excludes ephemeral
  paths; auto-snapshot labels list the changed files.

[0.1.0]: https://github.com/css521/agentenv/releases/tag/v0.1.0
