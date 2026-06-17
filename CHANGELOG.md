# Changelog

All notable changes to this project are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/), and the project aims to follow
[Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added
- New README hero GIF: real Claude Code, running inside `rewindable-claude`,
  deletes its own binary then calls `agentenv__checkout` MCP tool to roll the
  whole environment back — the binary is restored, the Claude session keeps
  running. The one demo `git` can't reproduce.
- `scripts/record-claude-tui.sh` + `scripts/fetch-recording-tools.sh` +
  `scripts/Dockerfile.recorder`: apt-free recording pipeline (static
  `asciinema` v3 + `agg` + a bundled monospace TTF; `agg --font-dir` covers
  Claude TUI's box-drawing glyphs via a DejaVu fallback). Recorder image is a
  pure `COPY` over `rewindable-claude` — seconds to build.

### Removed
- Obsolete shell-scripted demos and recorders that the real Claude Code GIF
  supersedes: `examples/demo/` (killer-demo + self-rollback-demo + README),
  `scripts/make-demo-gif.sh`, `scripts/make-claude-gif.sh`,
  `scripts/_claude-gif-inner.sh`, `scripts/rewindable-claude.sh`,
  `docs/demo.{cast,gif}`, `docs/self-rollback.{cast,gif}`,
  Makefile `demo` target.

## [0.2.0] - 2026-06-16

The "agent rolls back itself" release: an agent can now explore, undo, and
continue — from inside one running session — and prune dead ends, all via its
own MCP tools, while still capturing system-level changes.

### Added
- **`supervise --self-rollback`**: the agent rolls back its OWN environment
  (via the `agentenv__checkout` MCP tool / `ctl checkout`) and KEEPS RUNNING.
  The control socket lives inside the sandbox (`work/current/.agentenv/
  control.sock`, an always-ignored dir) so the agent can reach the daemon from
  within the env; a checkout reverts the rootfs in place WITHOUT killing the
  agent. Survival works because the in-place sync only touches changed files —
  the agent's own runtime is unchanged across a session's snapshots.
- **Interactive `supervise`**: with a TTY, runs the agent on a PTY (interactive
  REPLs like Claude Code work) while serving the control socket; without a TTY
  it stays headless (backgrounded + log-tail) for autonomous agents. One
  command covers both. Interactive mode is silent (no banner) so it doesn't
  pollute the agent's terminal.
- **Delete a node**: `agentenv delete <node>` (+ `ctl delete`, + the
  `agentenv__delete` MCP tool). Splices the node out, re-parenting its children
  to its parent so descendants survive; refuses to delete HEAD or the only
  node. Safe even when children hardlink-share its files.
- **Auto-route to a running session**: a lock-taking command (checkout / commit
  / delete / exec / tag / gc / tournament) that can't get the repo lock because
  a daemon/supervise holds it is transparently routed through that session's
  control socket — so `agentenv checkout <id>` just works whether or not a
  daemon is up.
- `agentenv shell -- <cmd>` runs an arbitrary program inside the sandbox on a PTY.
- `AGENTENV_FORWARD=NAME,PREFIX_*` forwards named env vars into the sandbox by
  value-at-call-time (complements `AGENTENV_PASS_<VAR>=val`). Wrapper images
  bake `ENV AGENTENV_FORWARD=ANTHROPIC_*,CLAUDE_*` so users just pass `-e ...`.
- `init --from` prints throttled copy progress + a final summary.
- `Dockerfile.control` gains `--build-arg SEED_AT_BUILD=1` to bake the managed
  rootfs at build time (cached layer, instant start; also can't capture
  runtime-mounted K8s secrets).
- `examples/claude-code/`: a ready-to-use rewindable Claude Code image —
  `docker run -it`, start `claude`, every change auto-snapshotted, and Claude
  can roll back / prune its own history via MCP. Published to
  `ghcr.io/css521/rewindable-claude`.
- Release pipeline pushes multi-arch images for both the agentenv binary
  (`ghcr.io/css521/agentenv`) and the Claude Code env
  (`ghcr.io/css521/rewindable-claude`).
- `.devcontainer/`: Go 1.26 image, opt-in `AGENTENV_CN_MIRROR=1` apt mirror.
  `.dockerignore` keeps the build context small.

### Changed
- Snapshot ignore overhaul: glob/segment patterns matched at any path depth
  (`.claude*`, `*.tmp.*`), sensible built-in defaults (agent state + caches +
  atomic temp files), `AGENTENV_IGNORE` now EXTENDS rather than replaces, and a
  set of always-ignored runtime paths. Empty-change snapshots are skipped. Net
  effect: the DAG shows only real work, not the agent's bookkeeping churn.
- `agentenv status` reports the backend's actual ignore patterns.

### Fixed
- `agentenv ctl exec` propagates the inner command's exit code (was always 0).
- Cross-uid daemon-socket connect reports both uids + a hint; daemon/headless
  supervise log their uid on startup.
- apt works inside the rootless sandbox (`examples/claude-code` bakes
  `APT::Sandbox::User "root"` — userns forbids apt's setgroups privilege-drop).

## [0.1.1] - 2026-06-16

### Changed
- `mcp.Serve` now takes the agentenv binary's resolved release tag and
  reports it in the MCP `initialize` handshake's `serverInfo.version`
  (previously hard-coded to `"0.1.0"`). Single source of truth — bumping
  the next tag no longer needs a code edit.

### Fixed
- `gofmt` formatting on three files (`internal/cli/ctl.go`,
  `internal/mcp/server.go`, `internal/mcp/server_test.go`); CI was red
  on the first push.
- `staticcheck` ST1005: dropped trailing `...` from two `tournament`
  usage error strings (illustrative, not literal).

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

[0.2.0]: https://github.com/css521/agentenv/releases/tag/v0.2.0
[0.1.1]: https://github.com/css521/agentenv/releases/tag/v0.1.1
[0.1.0]: https://github.com/css521/agentenv/releases/tag/v0.1.0
