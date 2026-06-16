#!/bin/bash
# Step 4 — end-to-end rollback smoke test.
#
# Proves the headline feature: an MCP tool call (agentenv__checkout) actually
# rolls the rootfs back, not just the HEAD pointer. Without this we have no
# evidence the read-only tools tested in mcp-smoke.sh translate to a working
# "time-travel" story.
#
# Flow:
#   1. init from /seed (single file: greeting.txt)              → node R
#   2. mutate rootfs (write file2, file3 via `agentenv exec`)   → N1, N2
#   3. assert HEAD = N2 and all three files exist
#   4. drive `agentenv mcp` over stdio: checkout R              ← the test
#   5. assert HEAD = R and file2/file3 are GONE (only greeting.txt left)
#   6. checkout forward to N2 via MCP                            ← also a test
#   7. assert HEAD = N2 and all three files are back
#
# Also smoke-checks the new diagnostics (#5/#6):
#   - errPrefix appears in isError content
#   - permission-denied path on the socket reports both uids

set -euo pipefail

log() { printf '\033[36m[rollback]\033[0m %s\n' "$*"; }
ok()  { printf '\033[32m  ok\033[0m  %s\n' "$*"; }
die() { printf '\033[31m  FAIL\033[0m %s\n' "$*" >&2; exit 1; }

# mcpCall <root> <method> <params-json>  →  prints one JSON response line.
# Spawns `agentenv mcp` fresh per batch and sends initialize + the request.
# We send both in one batch so the trailing `sleep 1` keeps stdin open long
# enough for both responses to flush before EOF.
mcpCall() {
  local method="$1" params="$2"
  {
    printf '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"x","version":"1"}}}\n'
    printf '{"jsonrpc":"2.0","id":2,"method":"%s","params":%s}\n' "$method" "$params"
    sleep 1
  } | agentenv mcp 2>/dev/null | jq -c 'select(.id == 2)' | head -1
}

# mcpCheckout <node>  →  prints the text content from the tool response.
mcpCheckout() {
  local node="$1"
  mcpCall "tools/call" \
    "{\"name\":\"agentenv__checkout\",\"arguments\":{\"node\":\"$node\"}}" \
    | jq -r '.result.content[0].text // .error.message // empty'
}

log "1. seed + init"
# Seed from the container's own rootfs so the sandbox has a real /bin/bash to
# run. Seeding from a hand-built /seed dir with just one file works for the
# read-only MCP smoke, but `exec` inside that rootfs has no shell — silently
# failing in ways that mask whether rollback worked.
mkdir -p /agentfs
echo "greeting" > /greeting.txt
agentenv init --from / >/dev/null
ROOT=$(agentenv head)
ok "root node R = $ROOT"

log "2. start daemon"
agentenv daemon >/tmp/daemon.log 2>&1 &
DAEMON_PID=$!
trap 'kill $DAEMON_PID 2>/dev/null || true' EXIT
for i in {1..20}; do
  [ -S /agentfs/agentenv.sock ] && break
  sleep 0.1
done
[ -S /agentfs/agentenv.sock ] || die "daemon socket never appeared"
ok "socket up"

# The daemon log should mention uid (#6). Check.
if ! grep -q "uid=" /tmp/daemon.log; then
  die "#6 regression: daemon startup log missing uid: $(cat /tmp/daemon.log)"
fi
ok "#6: daemon log includes uid ($(grep uid= /tmp/daemon.log))"

log "3. mutate rootfs through exec → auto-snapshots"
# The daemon runs `bash -lc <cmd>`, so we pass the whole shell command as one
# argument. ctl joins remaining argv with spaces, so quoting must survive that.
agentenv ctl exec "echo v2 > /file2.txt" >/dev/null
sleep 1   # let inotify+debounce settle and Token() fire
agentenv ctl exec "echo v3 > /file3.txt" >/dev/null
sleep 1
N2=$(agentenv head)
[ "$N2" != "$ROOT" ] || die "HEAD never advanced past root"
ok "HEAD advanced to $N2"

# Pre-rollback state: all three files present
for f in greeting.txt file2.txt file3.txt; do
  agentenv ctl exec "test -f /$f" >/dev/null 2>&1 || die "pre-rollback: missing /$f"
done
ok "pre-rollback rootfs has greeting.txt + file2.txt + file3.txt"

log "4. MCP-driven checkout back to ROOT  ← THE actual test"
CHECKOUT_TEXT=$(mcpCheckout "$ROOT")
echo "    response: $CHECKOUT_TEXT"
echo "$CHECKOUT_TEXT" | grep -q "$ROOT" || die "checkout response missing $ROOT"
ok "MCP returned new HEAD"

NOW=$(agentenv head)
[ "$NOW" = "$ROOT" ] || die "HEAD after checkout = $NOW, want $ROOT"
ok "HEAD really is $ROOT after MCP checkout"

log "5. verify rootfs ACTUALLY rolled back"
agentenv ctl exec "test -f /greeting.txt"   >/dev/null 2>&1 || die "rollback wiped greeting.txt — that's the ROOT file, should be there"
agentenv ctl exec "test ! -f /file2.txt"    >/dev/null 2>&1 || die "rollback did NOT remove file2.txt"
agentenv ctl exec "test ! -f /file3.txt"    >/dev/null 2>&1 || die "rollback did NOT remove file3.txt"
ok "rootfs is at R: only greeting.txt, no file2/file3"

log "6. MCP-driven checkout FORWARD to N2"
mcpCheckout "$N2" >/dev/null
[ "$(agentenv head)" = "$N2" ] || die "HEAD after forward checkout = $(agentenv head), want $N2"
agentenv ctl exec "test -f /file2.txt" >/dev/null 2>&1 || die "forward checkout did NOT restore file2.txt"
agentenv ctl exec "test -f /file3.txt" >/dev/null 2>&1 || die "forward checkout did NOT restore file3.txt"
ok "forward checkout restored file2 + file3"

log "7. #5 anti-hallucination prefix — checkout with bogus node"
BAD=$(mcpCall "tools/call" '{"name":"agentenv__checkout","arguments":{"node":"DOES_NOT_EXIST"}}')
IS_ERR=$(echo "$BAD" | jq -r '.result.isError // false')
TEXT=$(echo "$BAD"  | jq -r '.result.content[0].text // empty')
[ "$IS_ERR" = "true" ] || die "expected isError on bad node, got $BAD"
echo "$TEXT" | grep -q "TOOL CALL FAILED" || die "#5 regression: error text missing anti-hallucination prefix: $TEXT"
echo "$TEXT" | grep -q "do not"  -i || die "#5 regression: error text doesn't tell the model not to fabricate: $TEXT"
ok "#5: bad checkout produced anti-hallucination error text"

log "8. #6 cross-uid hint — try to connect from a different user"
# Create a non-root user and try `agentenv ctl head` as them. The socket
# is 0600 owned by root, so connect should fail with our new uid diagnostic.
useradd -m -d /home/probe probe 2>/dev/null || true
DIAG=$(su - probe -c "agentenv ctl head" 2>&1 || true)
echo "$DIAG" | grep -qE 'uid=0' || die "#6 regression: cross-uid error missing daemon uid: $DIAG"
echo "$DIAG" | grep -qE "uid=$(id -u probe)" || die "#6 regression: cross-uid error missing probe uid: $DIAG"
echo "$DIAG" | grep -qi "same user"           || die "#6 regression: cross-uid error missing actionable hint: $DIAG"
ok "#6: cross-uid connect surfaced both uids and a hint"

log "DONE — rollback + new diagnostics all green"
