#!/bin/bash
# Step A — protocol-level smoke test for `agentenv mcp`.
#
# Drives the MCP server with a hand-crafted JSON-RPC sequence on stdio (the
# exact wire format Claude Code uses) and asserts each response. Proves the
# Linux build + SDK transport + tool dispatch + daemon socket round-trip all
# work in a real Linux process — without needing Claude Code or any
# credentials in the container.
#
# Exits 0 on success, non-zero with a short reason on any failed assertion.

set -euo pipefail

log() { printf '\033[36m[smoke]\033[0m %s\n' "$*"; }
ok()  { printf '\033[32m  ok\033[0m  %s\n' "$*"; }
die() { printf '\033[31m  FAIL\033[0m %s\n' "$*" >&2; exit 1; }

log "1. seed a tiny rootfs and init the repo"
mkdir -p /agentfs /seed
echo "hello from agentenv" > /seed/greeting.txt
agentenv init --from /seed >/dev/null
ROOT_ID=$(agentenv head)
[ -n "$ROOT_ID" ] || die "no HEAD after init"
ok "root node $ROOT_ID"

log "2. start daemon in background"
agentenv daemon >/tmp/daemon.log 2>&1 &
DAEMON_PID=$!
trap 'kill $DAEMON_PID 2>/dev/null || true' EXIT
for i in {1..20}; do
  [ -S /agentfs/agentenv.sock ] && break
  sleep 0.1
done
[ -S /agentfs/agentenv.sock ] || die "daemon socket never appeared (log: $(cat /tmp/daemon.log))"
ok "socket /agentfs/agentenv.sock"

log "3. drive 'agentenv mcp' with a JSON-RPC handshake on stdio"
# Compose the canonical Claude Code call sequence:
#   1) initialize
#   2) notifications/initialized
#   3) tools/list
#   4) tools/call agentenv__head
#   5) tools/call agentenv__log
#   6) tools/call agentenv__show {node: ROOT_ID}
#   7) tools/call agentenv__show {}            ← schema-rejected, should isError
#
# The trailing `sleep 2` keeps stdin open so the SDK has time to write all
# responses before EOF triggers shutdown. Without it the heredoc closes
# immediately and the SDK's read loop exits mid-batch with only the first
# response or two flushed. Claude Code keeps stdin open for the session, so
# this is purely a quirk of script-driven testing.
{
  cat <<EOF
{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"smoke","version":"1"}}}
{"jsonrpc":"2.0","method":"notifications/initialized"}
{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}
{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"agentenv__head","arguments":{}}}
{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"agentenv__log","arguments":{}}}
{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"agentenv__show","arguments":{"node":"$ROOT_ID"}}}
{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"agentenv__show","arguments":{}}}
EOF
  sleep 2
} | agentenv mcp 2>/tmp/mcp.err > /tmp/mcp.out || die "agentenv mcp crashed (stderr: $(cat /tmp/mcp.err))"

# JSON-RPC says responses can arrive in any order — the SDK runs each request
# on its own goroutine, so fast tool calls overtake slow ones. Index by `id`
# instead of by line number.
RESP_COUNT=$(wc -l < /tmp/mcp.out | tr -d ' ')
[ "$RESP_COUNT" = "6" ] || die "expected 6 responses, got $RESP_COUNT"
ok "got 6 JSON-RPC responses"

# byid <n> — extract the response with matching id from /tmp/mcp.out.
byid() {
  jq -c "select(.id == $1)" /tmp/mcp.out | head -1
}

# --- id=1: initialize -------------------------------------------------------
INIT=$(byid 1)
PROTOCOL=$(echo "$INIT" | jq -r '.result.protocolVersion // empty')
SRV_NAME=$(echo "$INIT" | jq -r '.result.serverInfo.name // empty')
[ -n "$PROTOCOL" ] || die "initialize: no protocolVersion ($INIT)"
[ "$SRV_NAME" = "agentenv" ] || die "initialize: serverInfo.name = $SRV_NAME, want agentenv"
ok "initialize → protocol=$PROTOCOL serverInfo=$SRV_NAME"

# --- id=2: tools/list -------------------------------------------------------
TOOLS_LIST=$(byid 2)
TOOL_COUNT=$(echo "$TOOLS_LIST" | jq '.result.tools | length')
[ "$TOOL_COUNT" = "6" ] || die "tools/list: expected 6 tools, got $TOOL_COUNT"
for want in agentenv__head agentenv__log agentenv__branches agentenv__show agentenv__diff agentenv__checkout; do
  echo "$TOOLS_LIST" | jq -e ".result.tools[] | select(.name == \"$want\")" >/dev/null \
    || die "tools/list: missing $want"
done
ok "tools/list → all 6 tools registered"

# --- id=3: tools/call agentenv__head ----------------------------------------
HEAD_TEXT=$(byid 3 | jq -r '.result.content[0].text // empty')
echo "$HEAD_TEXT" | grep -q "$ROOT_ID" || die "head: response $HEAD_TEXT missing $ROOT_ID"
ok "agentenv__head → $HEAD_TEXT"

# --- id=4: tools/call agentenv__log -----------------------------------------
LOG_TEXT=$(byid 4 | jq -r '.result.content[0].text // empty')
echo "$LOG_TEXT" | grep -q "$ROOT_ID" || die "log: missing root node id"
echo "$LOG_TEXT" | grep -q "<- HEAD" || die "log: missing HEAD marker"
ok "agentenv__log → contains root + HEAD marker"

# --- id=5: tools/call agentenv__show {node: ROOT} ---------------------------
SHOW_TEXT=$(byid 5 | jq -r '.result.content[0].text // empty')
echo "$SHOW_TEXT" | grep -q "greeting.txt" || die "show: missing greeting.txt in $SHOW_TEXT"
ok "agentenv__show $ROOT_ID → lists greeting.txt"

# --- id=6: tools/call agentenv__show {} (no node) — schema rejection --------
BAD_RESP=$(byid 6)
# The SDK can surface schema-validation failure as either isError=true in the
# tool result OR as a top-level JSON-RPC error; accept either.
IS_ERR=$(echo "$BAD_RESP" | jq -r '.result.isError // false')
HAS_ERR=$(echo "$BAD_RESP" | jq -r '.error.code // empty')
if [ "$IS_ERR" != "true" ] && [ -z "$HAS_ERR" ]; then
  die "show {} should have been rejected, got $BAD_RESP"
fi
ok "agentenv__show {} → rejected (isError=$IS_ERR jsonrpc-error=$HAS_ERR)"

log "DONE — all assertions passed"
