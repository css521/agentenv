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

# Run under `supervise`: it auto-snapshots AND serves a control socket, so you
# can roll back WHILE work is in progress — from another terminal:
# `docker exec <c> agentenv ctl checkout <node>`. On rollback the supervised
# process is killed and relaunched from the restored environment. With
# `docker run -it`, supervise runs on a PTY (interactive); without a TTY it
# runs headless.
#
# Default: a login SHELL — you land at a prompt inside the sandbox and start
# `claude` (or anything) yourself; everything you do is auto-snapshotted. A
# rollback relaunches the shell from the restored env (re-run claude after).
#
# --self-rollback: the control socket lives inside the sandbox and a rollback
# does NOT kill the agent. So Claude Code can roll back its OWN environment via
# the agentenv__checkout MCP tool and keep running — explore, undo, continue,
# all from inside one session. (External rollback still works too: from another
# terminal, `docker exec <c> agentenv ctl --socket <ROOT>/work/current/.agentenv/control.sock checkout <id>`.)
#
#   no args  → supervised interactive shell (start claude yourself)
#   args     → supervise that command instead (e.g. `claude -p "..."`)
if [ "$#" -eq 0 ]; then
  exec agentenv supervise --self-rollback -- bash -l
else
  exec agentenv supervise --self-rollback -- "$@"
fi
