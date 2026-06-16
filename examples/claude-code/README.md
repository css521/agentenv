# Rewindable Claude Code

A ready-to-use Docker image: **Claude Code running inside an agentenv sandbox**,
so every filesystem change it makes — edits, `npm install`, `apt-get`, files
written anywhere — is auto-snapshotted and you can roll the **whole environment**
back to any point.

The managed rootfs is baked at **build time** (`init --from /` runs once during
`docker build`, cached in a layer), so container start is instant — no slow
per-start copy.

## Build

Context is the repo root (so the agentenv binary compiles from source):

```bash
docker build -f examples/claude-code/Dockerfile -t rewindable-claude .
```

Slow network (mainland China)? Pass mirrors — all optional, default to upstream:

```bash
# Pre-pull the base via a fast mirror once (daemon registry-mirror or explicit):
docker pull docker.m.daocloud.io/library/node:22-bookworm-slim
docker tag  docker.m.daocloud.io/library/node:22-bookworm-slim node:22-bookworm-slim

docker build -f examples/claude-code/Dockerfile \
  --build-arg APT_MIRROR=mirrors.aliyun.com \
  --build-arg NPM_REGISTRY=https://registry.npmmirror.com \
  --build-arg GOPROXY=https://goproxy.cn,direct \
  -t rewindable-claude .
```

> First build is heavy regardless of mirrors: it pulls the Node base, npm-installs
> Claude Code, and seeds the rootfs. It's all cached afterward — subsequent builds
> and every container start are fast.

## Use it

`docker run -it` drops you straight into a shell **inside** the rewindable
sandbox — you start `claude` yourself, and everything it does is auto-snapshotted.
Nested user namespaces need relaxed seccomp/AppArmor (same as any agentenv
rootless run):

```bash
docker run -it --name rc \
  --security-opt seccomp=unconfined --security-opt apparmor=unconfined \
  -e ANTHROPIC_API_KEY=sk-ant-... \
  rewindable-claude
# idealab / proxy users: also -e ANTHROPIC_BASE_URL=... -e ANTHROPIC_MODEL=...
```

You land in a shell inside the sandbox. Just run Claude Code:

```text
$ claude            # start it yourself; do your work normally
  ... claude edits files, runs commands — all auto-snapshotted ...
$ exit              # leave the shell; the final state is captured too
```

`ANTHROPIC_*` / `CLAUDE_*` are forwarded into the sandbox automatically — the
image bakes `AGENTENV_FORWARD=ANTHROPIC_*,CLAUDE_*,...`, so a plain
`-e ANTHROPIC_API_KEY=...` reaches `claude` inside.

> Running as root, Claude refuses `--dangerously-skip-permissions`. Use the
> normal permission prompts, or `claude --permission-mode acceptEdits` for a
> smoother flow.

### Rewind

The interactive shell holds the repo lock, so rewind **between** sessions: exit
the shell, then drive agentenv over `docker exec` (the lock is free), then
re-enter.

```bash
docker exec rc agentenv log            # the snapshot DAG
docker exec rc agentenv checkout <id>  # roll the WHOLE env back to <id>
docker exec -it rc agentenv shell      # re-enter from the restored state, run claude again
```

When done:

```bash
docker rm -f rc
```

## Headless one-shot

Pass a command after the image to run it inside the sandbox instead of an
interactive shell (still auto-snapshotted):

```bash
docker run --rm \
  --security-opt seccomp=unconfined --security-opt apparmor=unconfined \
  -e ANTHROPIC_API_KEY=sk-ant-... \
  rewindable-claude  claude --permission-mode acceptEdits -p "create hello.py that prints 42"
```

## How it works

- `Dockerfile` bakes `agentenv init --from /` at build time → the image ships a
  ready managed rootfs (`/var/lib/agentenv`). See the top-level `Dockerfile.control`
  for the same `SEED_AT_BUILD` mechanic applied to any agent image.
- `entrypoint.sh` runs `agentenv shell` (no args) or `agentenv shell -- <cmd>`
  (args) — an interactive PTY inside the sandbox with auto-capture running. The
  env allow-list forwards `AGENTENV_FORWARD`-named vars (baked to cover
  `ANTHROPIC_*`/`CLAUDE_*`) so `claude` sees your key.
- Why `agentenv shell` and not `agentenv supervise`? `supervise` backgrounds its
  agent with output to a log file (no TTY/stdin) — right for headless agents,
  wrong for an interactive REPL like Claude Code.

## Caveats

- **~2× image size**: the image keeps both its own `/` and the seeded copy under
  `/var/lib/agentenv` (overlay layers don't dedup like reflink/hardlink).
- **Not a security boundary** against hostile code — rootless isolation, not a VM.
- Rollback reverts the filesystem; running processes in the env are killed (no
  process-memory restore). Your Claude Code session in the shell is what you
  re-launch after a `checkout`.
