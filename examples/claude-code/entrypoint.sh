#!/bin/bash
# Entrypoint for the rewindable Claude Code image.
#
# `docker run -it <image>` drops you into an interactive shell INSIDE the
# agentenv sandbox, with auto-capture running. You start `claude` yourself;
# everything it (and you) do to the filesystem is auto-snapshotted, and you
# rewind the whole environment with `agentenv checkout`.
#
# Auth: pass `-e ANTHROPIC_API_KEY=...` (and optionally ANTHROPIC_BASE_URL /
# ANTHROPIC_MODEL). The image bakes `AGENTENV_FORWARD=ANTHROPIC_*,CLAUDE_*,...`
# so agentenv forwards those into the sandbox env where `claude` runs.
set -e

: "${AGENTENV_ROOT:=/var/lib/agentenv}"
export AGENTENV_ROOT

# Fallback seed (normally already baked at build time — see Dockerfile).
if [ ! -f "$AGENTENV_ROOT/meta.json" ]; then
  echo "rewindable-claude: seeding managed rootfs (first run)..."
  agentenv init --from /
fi

# No args → interactive shell inside the sandbox (you run `claude` from here).
# Args → run them inside the sandbox instead (e.g. a one-shot command), still
# on a PTY with auto-capture. Either way it's `agentenv shell`, which holds the
# repo lock + runs the capturer; rewind between sessions with `agentenv
# checkout` (the lock is free once you exit).
if [ "$#" -eq 0 ]; then
  exec agentenv shell
else
  exec agentenv shell -- "$@"
fi
