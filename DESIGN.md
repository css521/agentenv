# agentenv — Rewindable Virtual Agent Environment (Design)

A sandbox whose **filesystem and installed dependencies can be rolled back to
any prior snapshot at any time**, with **zero agent intrusion**: the agent
runs unmodified, calls no API, and the environment versions itself
automatically on every change.

Rollback reverts the rootfs; running processes inside the env are killed (no
process-memory restore, no CRIU). The **agent loop itself survives** because
it lives one layer up — under `agentenv supervise`, which relaunches the
agent from the restored environment.

## Decisions (current)

- **Language**: Go. **No mandatory base image** — bring your own rootfs via
  `init --from <dir|/>` (the common path: wrap an existing container image)
  or `init --tarball <path|URL>` (for air-gapped / scripted seeding).
- **Minimal deps**:
  - `containerd/btrfs/v2` — cgo, **only** when built `-tags btrfs`.
  - `creack/pty` + `golang.org/x/sys` + `golang.org/x/term` — interactive
    shell + namespace plumbing.
  - `modelcontextprotocol/go-sdk` — `agentenv mcp` (MCP server over stdio).
- **Portability is the goal**: must run from a privileged VM down to a
  **restricted K8s pod** (non-root, no `privileged`, default seccomp +
  Unconfined for nested userns). Nothing core may require privilege.
- **Pluggable backends + capability probe** (`internal/backend`), picked at
  startup; explicit override via `AGENTENV_BACKEND=copy|btrfs`:
  - **copy (rootless)** — *default, pure-Go static build*. Linux user
    namespaces map container-root → caller's uid (so a non-root pod gains
    *namespaced* CAP_SYS_ADMIN for mount/pivot_root), plus plain-copy
    snapshots that hardlink-share unchanged files with the parent node.
    Probes `FICLONE` at startup and uses reflinks on XFS/btrfs/ZFS/bcachefs
    when available — snapshots become near O(1).
  - **btrfs (privileged)** — `-tags btrfs`, requires root + btrfs fs;
    instant CoW subvolume snapshots.
- **Capture is change-driven, not channel-driven** — inotify watches the
  rootfs and an auto-snapshot fires on change (shell commands *and* direct
  file edits); a periodic `Token()` (btrfs generation / copy fingerprint) is
  the backstop when inotify is exhausted (`ENOSPC` falls back gracefully).
  The agent never calls commit. Snapshots are debounced and labelled with
  the changed file list.
- **Branch exploration** is first-class. The DAG is a tree; `tournament`
  forks N candidates into isolated workspaces, runs each plus a test
  command in parallel, and keeps the first that passes.
- **Rollback semantics**: revert the rootfs, kill processes inside the env.
  The agent *loop* survives because it lives under `supervise` (outside the
  rolled-back env). No CRIU; no process-memory snapshot.

> Earlier iterations used go-containerregistry + runc/libcontainer and
> assumed a privileged "control-root + inner-root" topology on btrfs. That
> was dropped: the heavy deps were trimmed, and the privilege assumption
> fails in a restricted pod — hence the rootless default backend.

## Topology — three layers, agent unaware of two of them

```
Host
  └─ (1) launcher — one-shot, host side; the ONLY thing that "starts the
        sandbox". Simplest form is `agentenv supervise -- <agent>` inside
        an image built from Dockerfile.control.
        │
        └─ (2) supervise — the long-running parent process; owns the repo
              lock, runs the capturer (inotify + debounced snapshots), and
              relaunches the agent on out-of-band checkout. Never rolled
              back itself.
              │
              └─ (3) inner env — the rootfs that gets snapshotted and
                    rolled back. Shell, apt, the agent itself, every other
                    process. Rollback = kill processes here + restore the
                    rootfs from the target node.
```

### Why this resolves the bootstrap

"Starting the sandbox" and "managing the environment" are two different
responsibilities at two different layers:

- The **launcher** (host side, one-shot) starts the sandbox. In practice
  that's `docker run …/k8s pod spec`, with `Dockerfile.control` as the base
  image; its entrypoint runs `agentenv init --from /` then `supervise --
  <agent>`.
- The **agent**, already running inside the inner env, just runs commands.
  It doesn't know agentenv exists. Rollback is driven from outside via the
  daemon socket (`agentenv ctl checkout …`) or via MCP from Claude Code
  (`agentenv__checkout`); `supervise` kills the agent, restores the rootfs,
  relaunches the agent.

## On-disk layout (`AGENTENV_ROOT`, default `/agentfs`)

The default works on any filesystem (the copy backend is fs-agnostic). The
btrfs backend, when used, requires `AGENTENV_ROOT` to be on a btrfs fs.

```
<root>/
  nodes/<id>/    one snapshot per immutable DAG node (read-only for btrfs;
                  a hardlink-shared dir for copy)
  work/current/  the live writable inner-env rootfs (commands run here)
  meta.json     commit-DAG metadata, fsync-safe atomic writes
  agentenv.sock unix socket exposed by `daemon` / `supervise` (mode 0600;
                  same uid required to connect)
  agentenv.lock cross-process flock guarding the repo
```

## commit-DAG

- One `Node` == one snapshot == an immutable point in time.
- Single parent → a tree (enough for environment rollback; extend to a DAG
  if merges are ever needed). `branches` lists the leaf nodes.
- `HEAD` = the node the current inner-env was derived from. Tags
  (`agentenv tag winner <id>`) give human-readable names to interesting nodes.
- Retention is DVR-style: a sliding window of recent nodes is kept dense,
  older history is sparsified; `gc` reclaims disk from orphan snapshots.

## Core operations

| Command            | Effect |
|--------------------|--------|
| `init --from <dir>` / `init --tarball <p>` | seed root from a directory tree (one-time copy) or extract a `.tar(.gz)`; freeze as the root node |
| `supervise -- <cmd>` | start the inner agent under auto-capture; survives rollbacks (relaunches the agent from the restored env) |
| `daemon`           | serve the newline-JSON protocol on the unix socket (for orchestrators / `ctl`) |
| `mcp`              | MCP server over stdio (Claude Code / any MCP host); bridges tool calls to the daemon socket |
| `exec -- <cmd>`    | one-shot run inside the inner env (scripting/CI; takes the repo lock so can't run alongside `daemon`/`supervise`) |
| `ctl <op> [...]`   | out-of-band client for a running daemon/supervise (no lock; safe while the agent is live) |
| `commit -m <msg>`  | manual snapshot (auto-capture usually does this automatically) |
| `checkout <ref>`   | kill processes in the inner env, restore work/current from `<ref>`, move HEAD |
| `tag [name] [ref]` | list / get / set / delete named refs |
| `tournament --base … --test "…" -- "c1" "c2" …` | fork N candidates in parallel workspaces, run each + test, keep the first that passes |
| `status`           | one-screen runtime summary (backend, HEAD, disk, ignore prefixes, inotify limit) |
| `log` / `head` / `branches` / `show` / `diff` | inspect the commit-DAG |
| `gc`               | delete orphan snapshots not referenced by the DAG |

### Rollback flow (`checkout`)

Backend-agnostic, defined by the `Snapshotter` interface in
`internal/backend`:

1. Kill processes still running in the inner env (other processes die).
2. `RestoreWork(<target>)` — copy backend rebuilds `work/current` from the
   target node's hardlink-shared tree (copies only the diff); btrfs swaps
   the subvolume.
3. Move HEAD to `<target>`; persist `meta.json` (fsync + atomic rename).
4. If running under `supervise`: relaunch the agent process from the
   restored env. The supervise process itself is never touched.

## Notes / trade-offs

- `apt` installs into system paths (`/usr`, `/etc`, `/var`), so rollback
  must revert the whole inner-env rootfs — which is exactly one snapshot
  restore.
- Network side-effects, wall-clock time, and already-sent requests cannot
  be rolled back. Real binaries + network are inherently non-deterministic.
- The copy backend's rootless runner does require nested user namespaces in
  the pod/container. On restrictive AppArmor (Ubuntu 22.04+) you need
  `seccomp=Unconfined` and possibly `apparmor=Unconfined`. The k8s example
  pod spec covers this.
- btrfs + libcontainer paths are Linux-only (cgo for btrfs). Develop on a
  Linux VM or a loopback btrfs image; the macOS host is for editing only.
- Not a security boundary against hostile binaries — rootless gives
  isolation, not adversarial isolation. Run untrusted code behind a
  VM/microVM.
