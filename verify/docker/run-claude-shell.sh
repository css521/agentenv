#!/bin/bash
# Step B — interactive Claude Code session driving agentenv via MCP.
#
# Mounts ~/.claude (the directory you asked for) so any host-side state Claude
# Code wrote (settings, conversation history, CLAUDE.md, plugins) is visible
# in the container.
#
# Does NOT mount ~/.claude.json — that file holds OAuth tokens / session
# state, and the container needs to mutate its MCP-server list. Keeping the
# container's .claude.json separate avoids leaking host credentials AND keeps
# `claude mcp add agentenv` from persisting back to your host shell. The
# trade-off: you'll need to `claude` (and authenticate) inside the container
# once per session.
#
# Build first:
#   docker build --platform=linux/arm64 -f verify/docker/Dockerfile.claude-shell \
#       -t agentenv-claude-shell verify/docker/

set -e

CLAUDE_DIR="${HOME}/.claude"
[ -d "$CLAUDE_DIR" ] || { echo "no $CLAUDE_DIR — run 'claude' once on the host first"; exit 1; }

docker run --rm -it \
  --platform=linux/arm64 \
  -v "${CLAUDE_DIR}":/root/.claude \
  -e ANTHROPIC_API_KEY="${ANTHROPIC_API_KEY:-}" \
  -e ANTHROPIC_BASE_URL="${ANTHROPIC_BASE_URL:-}" \
  -e ANTHROPIC_MODEL="${ANTHROPIC_MODEL:-}" \
  agentenv-claude-shell
