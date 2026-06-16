#!/bin/bash
# Launch an interactive, rewindable Claude Code session — for use inside a dev
# container / GitHub Codespace (where api.anthropic.com is reachable).
#
#   scripts/rewindable-claude.sh
#
# What it does:
#   1. Seeds a managed rootfs from / (once; cached in AGENTENV_ROOT afterward).
#      Claude Code's state churn (~/.claude*) and caches are ignored so they
#      don't pollute the snapshot history.
#   2. Drops you into a shell INSIDE that rootfs with auto-snapshot running —
#      run `claude` there; every change is captured.
#   3. Rewind between sessions:  agentenv log / agentenv checkout <id>
#
# Auth: export ANTHROPIC_API_KEY (and ANTHROPIC_BASE_URL / ANTHROPIC_MODEL if
# you use a gateway) before running — they're forwarded into the sandbox.
set -e

export AGENTENV_ROOT="${AGENTENV_ROOT:-$HOME/.agentenv-claude}"
# Extend the built-in ignores so Claude Code's own state isn't snapshotted.
export AGENTENV_IGNORE="${AGENTENV_IGNORE:-root/.claude,root/.cache,root/.npm,home/vscode/.claude,home/vscode/.cache,home/vscode/.npm}"
# Forward auth/proxy env into the sandbox where `claude` runs.
export AGENTENV_FORWARD="${AGENTENV_FORWARD:-ANTHROPIC_*,CLAUDE_*,HTTP_PROXY,HTTPS_PROXY,NO_PROXY,ALL_PROXY}"

if ! command -v agentenv >/dev/null 2>&1; then
  echo "agentenv not on PATH — build it first: go build -o ~/go/bin/agentenv ." >&2
  exit 1
fi

if [ ! -f "$AGENTENV_ROOT/meta.json" ]; then
  echo "Seeding managed rootfs from / into $AGENTENV_ROOT (one-time)..."
  agentenv init --from /
fi

echo "Entering rewindable shell. Run 'claude' here; rewind later with:"
echo "  agentenv log   /   agentenv checkout <node>"
exec agentenv shell
