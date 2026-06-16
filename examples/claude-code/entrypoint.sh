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

# Run the agent under `supervise`: it auto-snapshots AND serves a control
# socket, so you can roll back WHILE the agent is running — from another
# terminal: `docker exec <c> agentenv ctl checkout <node>`. On rollback the
# agent is killed and relaunched from the restored environment; you never have
# to exit it. With `docker run -it`, supervise runs the agent on a PTY (Claude
# Code's interactive TUI works); without a TTY it runs headless.
#
#   no args        → supervise Claude Code (docker run -it ... → interactive)
#   args           → supervise that command instead (e.g. `claude -p "..."`,
#                    or `bash -l` if you'd rather launch claude by hand)
if [ "$#" -eq 0 ]; then
  exec agentenv supervise -- claude
else
  exec agentenv supervise -- "$@"
fi
