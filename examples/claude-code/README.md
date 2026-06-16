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

`docker run -it` drops you at a **shell inside** the rewindable sandbox, running
under `agentenv supervise` — you start `claude` (or anything) yourself,
everything is auto-snapshotted, and a control socket lets you roll back **while
work is still in progress**. Nested user namespaces need relaxed seccomp/AppArmor
(same as any agentenv rootless run):

```bash
docker run -it --name rc \
  --security-opt seccomp=unconfined --security-opt apparmor=unconfined \
  -e ANTHROPIC_API_KEY=sk-ant-... \
  rewindable-claude
# idealab / proxy users: also -e ANTHROPIC_BASE_URL=... -e ANTHROPIC_MODEL=...
```

You land at a prompt inside the sandbox. Start Claude Code yourself:

```text
$ claude --permission-mode acceptEdits     # root → use this (not --dangerously-skip-permissions)
  ... work normally; every change is auto-snapshotted ...
```

`ANTHROPIC_*` / `CLAUDE_*` are forwarded into the sandbox automatically — the
image bakes `AGENTENV_FORWARD=ANTHROPIC_*,CLAUDE_*,...`, so a plain
`-e ANTHROPIC_API_KEY=...` reaches `claude` inside.

> Running as root, Claude refuses `--dangerously-skip-permissions`. Use the
> normal permission prompts, or launch with
> `docker run -it ... rewindable-claude claude --permission-mode acceptEdits`.

> If Claude shows "Failed to connect to api.anthropic.com" behind a regional
> block: its interactive startup pings api.anthropic.com directly (headless
> `-p` doesn't). Route the container through a proxy with a permitted egress —
> e.g. OrbStack ▸ Settings ▸ Network ▸ Proxy (SOCKS5 works there at the network
> layer), excluding your inference gateway's host so it stays direct.

### Rewind — while work is in progress

From **another terminal** (your shell + Claude keep running in the first one):

```bash
docker exec rc agentenv ctl log            # the snapshot DAG
docker exec rc agentenv ctl checkout <id>  # roll the WHOLE env back to <id>
```

On checkout, supervise kills the supervised shell (and Claude running in it),
restores the environment, and relaunches the shell from the restored state —
you don't exit anything by hand; terminal 1 just lands back at a fresh prompt
in the rolled-back env (re-run `claude` to continue). This is why the image
uses `supervise`, not a plain interactive `shell`: a bare shell would hold the
repo lock and force you to exit before you could roll back.

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
- `entrypoint.sh` runs `agentenv supervise -- claude` (or your command). supervise
  auto-snapshots AND serves a control socket; with a TTY (`docker run -it`) it
  runs the agent on a PTY so Claude Code's interactive TUI works, and on rollback
  it kills + relaunches the agent from the restored env. The env allow-list
  forwards `AGENTENV_FORWARD`-named vars (baked to cover `ANTHROPIC_*`/`CLAUDE_*`)
  so `claude` sees your key.
- `AGENTENV_IGNORE=root/.claude,root/.cache,root/.npm` keeps Claude Code's own
  state churn (its atomic `~/.claude.json.tmp`/`.lock` writes) out of the
  snapshot history, so the DAG shows only changes to your project.

## Caveats

- **~2× image size**: the image keeps both its own `/` and the seeded copy under
  `/var/lib/agentenv` (overlay layers don't dedup like reflink/hardlink).
- **Not a security boundary** against hostile code — rootless isolation, not a VM.
- Rollback reverts the filesystem; running processes in the env are killed (no
  process-memory restore). Your Claude Code session in the shell is what you
  re-launch after a `checkout`.
