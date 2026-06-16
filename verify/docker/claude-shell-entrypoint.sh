#!/bin/bash
# Step B entrypoint — start agentenv daemon, wire Claude Code to it via MCP,
# then exec whatever the user passed as CMD (default: an interactive bash).
set -e

log() { printf '\033[36m[entrypoint]\033[0m %s\n' "$*"; }

# --- 1. Seed a rootfs and init the agentenv repo. -----------------------------
log "seeding /seed and initializing agentenv repo at /agentfs"
mkdir -p /agentfs /seed
[ -f /seed/greeting.txt ] || echo "hello from agentenv" > /seed/greeting.txt
if [ ! -f /agentfs/meta.json ]; then
  agentenv init --from /seed >/dev/null
fi
ROOT=$(agentenv head)
log "HEAD = $ROOT"

# --- 2. Start the daemon if it isn't already running. ------------------------
if [ ! -S /agentfs/agentenv.sock ]; then
  log "starting agentenv daemon → /tmp/agentenv-daemon.log"
  agentenv daemon >/tmp/agentenv-daemon.log 2>&1 &
  for i in {1..20}; do
    [ -S /agentfs/agentenv.sock ] && break
    sleep 0.1
  done
fi
[ -S /agentfs/agentenv.sock ] || { echo "daemon socket never appeared"; cat /tmp/agentenv-daemon.log; exit 1; }

# --- 3. Register the agentenv MCP server with Claude Code. -------------------
# `claude mcp add` writes to /root/.claude.json. That file is NOT bind-mounted
# from the host (the host's copy holds OAuth tokens we want isolated), so this
# entry only lives in the container — gone when the container exits.
log "registering 'agentenv' MCP server with Claude Code"
claude mcp remove agentenv -s user 2>/dev/null || true
claude mcp add agentenv -s user -- agentenv mcp

cat <<EOF

=================================================================
Step B is ready. Inside this shell:

  1. If this is your first time, authenticate Claude Code:
       claude
     and follow the login flow (browser OAuth or pasted API key).
     On Mac the keychain credentials don't transfer through the
     mount, so a fresh login is expected.

  2. Then ask Claude to drive the MCP server, e.g.:
       claude -p "Call the agentenv__head MCP tool and tell me the HEAD id."
     or interactively:
       claude
       > what does the agentenv__log tool return?

  3. Other things to try:
       claude -p "Use agentenv__show to list the files at node $ROOT"
       claude -p "Run agentenv__diff between two nodes and summarize"

agentenv state:
  HEAD       : $ROOT
  socket     : /agentfs/agentenv.sock
  daemon log : /tmp/agentenv-daemon.log
  MCP config : see  cat ~/.claude.json | jq .mcpServers
=================================================================

EOF

exec "$@"
