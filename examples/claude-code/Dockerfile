# A ready-to-use rewindable Claude Code box.
#
# `docker run` drops you into a sandbox that already has Claude Code installed,
# with the managed rootfs baked at BUILD time (SEED_AT_BUILD) so startup is
# instant. Everything Claude Code does to the filesystem — edits, `npm install`,
# `apt-get`, files written anywhere — is auto-snapshotted, and you can roll the
# WHOLE environment back to any point.
#
# Build (context = repo root, so the agentenv binary can be compiled from source):
#   docker build -f examples/claude-code/Dockerfile -t rewindable-claude .
#
# Use it (see this folder's README for the full workflow):
#   docker run -d --name rc \
#     --security-opt seccomp=unconfined --security-opt apparmor=unconfined \
#     -e ANTHROPIC_API_KEY=sk-... \
#     rewindable-claude
#   docker exec -it rc agentenv shell      # inside: run `claude`, work normally
#   docker exec rc agentenv log            # see auto-snapshots
#   docker exec rc agentenv checkout <id>  # rewind the whole env
#
# Why not `agentenv supervise -- claude`? supervise backgrounds the agent with
# its output going to a log file (no TTY/stdin), which is right for headless
# agents but breaks interactive Claude Code. So this image idles, and you enter
# an interactive, auto-snapshotting shell with `agentenv shell` over docker exec.

# --- stage 1: build the static agentenv binary from the repo ---
FROM golang:1.26 AS build
ARG GOPROXY
ENV GOPROXY=${GOPROXY:-https://proxy.golang.org,direct}
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -o /agentenv .

# --- stage 2: Node base + Claude Code + agentenv, seeded at build time ---
FROM node:22-bookworm-slim

# Optional mirrors for slow/blocked networks (e.g. mainland China). Empty by
# default — OSS users hit the upstream registries unchanged. Enable with:
#   --build-arg APT_MIRROR=mirrors.aliyun.com \
#   --build-arg NPM_REGISTRY=https://registry.npmmirror.com
ARG APT_MIRROR=
ARG NPM_REGISTRY=

# Claude Code CLI. git is handy inside the sandbox; ca-certificates for TLS.
RUN if [ -n "$APT_MIRROR" ]; then \
      sed -ri "s#deb\.debian\.org#$APT_MIRROR#g" \
        /etc/apt/sources.list.d/*.sources /etc/apt/sources.list 2>/dev/null || true; \
    fi \
 && apt-get update \
 && apt-get install -y --no-install-recommends git ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && if [ -n "$NPM_REGISTRY" ]; then npm config set registry "$NPM_REGISTRY"; fi \
 && npm install -g @anthropic-ai/claude-code

COPY --from=build /agentenv /usr/local/bin/agentenv
COPY examples/claude-code/entrypoint.sh /usr/local/bin/rewindable-claude-entrypoint
RUN chmod +x /usr/local/bin/agentenv /usr/local/bin/rewindable-claude-entrypoint

# A scratch project directory the agent can work in.
RUN mkdir -p /work
WORKDIR /work

ENV AGENTENV_ROOT=/var/lib/agentenv
# Forward Claude Code's auth/config into the sandbox by NAME — the user passes
# `-e ANTHROPIC_API_KEY=...` on docker run and agentenv forwards it (and the
# rest of the ANTHROPIC_*/CLAUDE_* families + proxies) into the env where the
# agent runs. docker exec sees these because they're real container env, not a
# runtime export.
ENV AGENTENV_FORWARD=ANTHROPIC_*,CLAUDE_*,HTTP_PROXY,HTTPS_PROXY,NO_PROXY

# No AGENTENV_IGNORE needed: agentenv's built-in defaults already ignore Claude
# Code's state churn (.claude at any depth), atomic temp files (*.tmp.*), and
# caches (.cache/.npm/.local, apt lists) — so the snapshot DAG shows only the
# changes Claude makes to your project. Set AGENTENV_IGNORE only to add more.

# Bake the managed rootfs at build time: the slow `init --from /` copy becomes
# a cached image layer instead of a per-start tax. The entrypoint then sees an
# existing meta.json and skips straight to serving.
RUN agentenv init --from /

ENTRYPOINT ["/usr/local/bin/rewindable-claude-entrypoint"]
# No CMD: `docker run -it <image>` lands you in an interactive shell inside the
# sandbox — start `claude` yourself. Pass a command to run that instead.
