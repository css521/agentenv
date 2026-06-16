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

Start the box (it idles; you drive it over `docker exec`). nested user namespaces
need relaxed seccomp/AppArmor — the same requirement as any agentenv rootless run:

```bash
docker run -d --name rc \
  --security-opt seccomp=unconfined --security-opt apparmor=unconfined \
  -e ANTHROPIC_API_KEY=sk-ant-... \
  rewindable-claude
# idealab / proxy users: also -e ANTHROPIC_BASE_URL=... -e ANTHROPIC_MODEL=...
```

`ANTHROPIC_*` / `CLAUDE_*` env is forwarded into the sandbox automatically (the
entrypoint re-exports them as `AGENTENV_PASS_*`).

Work inside the rewindable env:

```bash
docker exec -it rc agentenv shell      # interactive shell INSIDE the sandbox
#   > claude                           # run Claude Code; do your work
#   > exit                             # leave; changes are auto-snapshotted
```

Inspect history and rewind (run these between shell sessions — `shell` and
`checkout` share the repo lock, so don't overlap them):

```bash
docker exec rc agentenv log            # the snapshot DAG
docker exec rc agentenv checkout <id>  # roll the WHOLE env back to <id>
docker exec -it rc agentenv shell      # re-enter from the restored state
```

When done:

```bash
docker rm -f rc
```

## Headless one-shot

Pass a command instead of idling — it runs under `supervise` (auto-snapshot,
restart-on-rollback), good for scripted tasks:

```bash
docker run --rm \
  --security-opt seccomp=unconfined --security-opt apparmor=unconfined \
  -e ANTHROPIC_API_KEY=sk-ant-... \
  rewindable-claude  claude -p "create a hello.py that prints 42"
```

## How it works

- `Dockerfile` bakes `agentenv init --from /` at build time → the image ships a
  ready managed rootfs (`/var/lib/agentenv`). See the top-level `Dockerfile.control`
  for the same `SEED_AT_BUILD` mechanic applied to any agent image.
- `entrypoint.sh` forwards auth env into the sandbox and either idles (default)
  or runs your command under `supervise`.
- Interactive Claude Code uses `agentenv shell` (PTY + auto-capture) rather than
  `supervise`, because `supervise` backgrounds its agent without a TTY — right for
  headless agents, wrong for an interactive REPL.

## Caveats

- **~2× image size**: the image keeps both its own `/` and the seeded copy under
  `/var/lib/agentenv` (overlay layers don't dedup like reflink/hardlink).
- **Not a security boundary** against hostile code — rootless isolation, not a VM.
- Rollback reverts the filesystem; running processes in the env are killed (no
  process-memory restore). Your Claude Code session in the shell is what you
  re-launch after a `checkout`.
