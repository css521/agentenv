#!/usr/bin/env bash
# Verify `agentenv supervise`: an unmodified agent runs INSIDE the env, its changes
# are auto-snapshotted, and an out-of-band rollback (via the socket) kills and
# restarts the agent from the restored environment.
set -euo pipefail

cd /src
sed -i 's#\(deb\|security\)\.debian\.org#mirrors.aliyun.com#g' /etc/apt/sources.list.d/debian.sources 2>/dev/null || true
apt-get update -qq && apt-get install -y -qq python3 >/dev/null 2>&1
CGO_ENABLED=0 go build -o /tmp/agentenv .
export PATH=/tmp:$PATH AGENTENV_ROOT=/tmp/agentfs
mkdir -p /tmp/agentfs
arch=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
: "${AGENTENV_DEMO_TARBALL:=https://cdimage.ubuntu.com/ubuntu-base/releases/24.04/release/ubuntu-base-24.04.4-base-${arch}.tar.gz}"
agentenv init --tarball "$AGENTENV_DEMO_TARBALL" >/dev/null

# A plain, unmodified "agent": appends "tick N" (its own in-memory counter) every
# 4s. It calls no agentenv API. Run it under supervise.
AGENT='n=0; while true; do echo "tick $n" >> /root/agentlog; n=$((n+1)); sleep 4; done'
agentenv supervise -- bash -c "$AGENT" >/tmp/sup.out 2>&1 &
SUP=$!
sleep 14   # let it tick a few times -> a few auto-snapshots

echo "=== agentlog before rollback ==="
cat /tmp/agentfs/work/current/root/agentlog

echo "=== roll back via the socket to an early node ==="
python3 - <<'PY'
import socket, json
s = socket.socket(socket.AF_UNIX); s.connect("/tmp/agentfs/agentenv.sock"); f = s.makefile("rwb")
def call(**r): f.write((json.dumps(r)+"\n").encode()); f.flush(); return json.loads(f.readline())
nodes = call(op="log")["nodes"]
print("nodes:", [(n["id"][:8], n["message"]) for n in nodes])
target = nodes[1]["id"] if len(nodes) > 1 else nodes[0]["id"]
print("checkout ->", target[:8])
print("resp:", call(op="checkout", node=target))
PY

sleep 6
echo "=== supervise output (expect restart on rollback) ==="
grep -E "agent pid|killed by rollback|restarting" /tmp/sup.out || true
echo "=== agentlog after rollback (agent restarted -> 'tick 0' reappears) ==="
cat /tmp/agentfs/work/current/root/agentlog

kill "$SUP" 2>/dev/null || true; wait "$SUP" 2>/dev/null || true

if grep -q "restarting from the restored env" /tmp/sup.out; then
  echo "PASS: agent ran inside the env, was rolled back and restarted (zero agent changes)"
else
  echo "FAIL: no restart-on-rollback observed"; exit 1
fi
echo "############ SUPERVISE ALL DONE ############"
