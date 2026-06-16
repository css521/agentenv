#!/usr/bin/env bash
# End-to-end verification, meant to run inside a privileged Linux container.
# Builds agentenv (cgo + libbtrfs), creates a loopback btrfs, and exercises the
# rewind闭环: filesystem rollback, dependency (apt) rollback, and
# checkout-kills-background-processes.
set -euo pipefail

echo "############ 0. build deps + binary ############"
# Use the Aliyun mirror for the Debian build container (much faster in this network).
sed -i 's#\(deb\|security\)\.debian\.org#mirrors.aliyun.com#g' /etc/apt/sources.list.d/debian.sources 2>/dev/null || true
apt-get update -qq
apt-get install -y -qq libbtrfs-dev btrfs-progs >/dev/null
cd /src
CGO_ENABLED=1 go build -tags btrfs -o /usr/local/bin/agentenv .
echo "built: $(command -v agentenv)"

echo "############ 1. loopback btrfs at /agentfs ############"
truncate -s 10G /agentfs.img
mkfs.btrfs -q /agentfs.img
mkdir -p /agentfs
mount -o loop /agentfs.img /agentfs
export AGENTENV_ROOT=/agentfs
findmnt /agentfs

echo "############ 2. init from tarball ############"
arch=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
: "${AGENTENV_DEMO_TARBALL:=https://cdimage.ubuntu.com/ubuntu-base/releases/24.04/release/ubuntu-base-24.04.4-base-${arch}.tar.gz}"
agentenv init --tarball "$AGENTENV_DEMO_TARBALL"
# Point the inner-env apt at the Aliyun ports mirror (arm64 uses ports.ubuntu.com).
agentenv exec -- bash -lc \
  'sed -i "s#//ports.ubuntu.com/ubuntu-ports#//mirrors.aliyun.com/ubuntu-ports#g; s#//archive.ubuntu.com/ubuntu#//mirrors.aliyun.com/ubuntu#g" /etc/apt/sources.list.d/ubuntu.sources 2>/dev/null || true'

echo "############ 3. filesystem rollback (no network) ############"
agentenv exec -- bash -lc 'echo hello > /root/a.txt; cat /root/a.txt'
agentenv commit -m "has a.txt"
NODE_A=$(agentenv log | grep 'has a.txt' | awk '{print $2}')
echo "node A = $NODE_A"
agentenv exec -- bash -lc 'echo world > /root/b.txt'
agentenv commit -m "has a.txt + b.txt"
echo "-- tree --"; agentenv log
echo "-- rolling back to node A --"
agentenv checkout "$NODE_A"
echo "-- /root after rollback (expect a.txt only) --"
agentenv exec -- bash -lc 'ls -1 /root'
if agentenv exec -- bash -lc 'test -f /root/b.txt'; then
  echo "FAIL: b.txt should be gone after rollback"; exit 1
fi
echo "PASS: filesystem rolled back (b.txt gone, a.txt kept)"

echo "############ 4. dependency (apt) rollback ############"
agentenv exec -- bash -lc 'apt-get -o APT::Sandbox::User=root update -qq && apt-get install -y -qq tree' || { echo "apt failed (network?)"; SKIP_APT=1; }
if [[ -z "${SKIP_APT:-}" ]]; then
  agentenv commit -m "tree installed"
  NODE_TREE=$(agentenv log | grep 'tree installed' | awk '{print $2}')
  agentenv exec -- bash -lc 'apt-get install -y -qq jq'
  agentenv exec -- bash -lc 'command -v tree && command -v jq'
  echo "-- rolling back to before jq --"
  agentenv checkout "$NODE_TREE"
  agentenv exec -- bash -lc 'command -v tree && echo "tree kept"'
  if agentenv exec -- bash -lc 'command -v jq >/dev/null'; then
    echo "FAIL: jq should be gone after rollback"; exit 1
  fi
  echo "PASS: dependency rolled back (jq gone, tree kept)"
fi

# Auto-capture, process-kill-on-rollback, and retention are backend-agnostic
# (the repo/capture layer is shared) and are verified on the copy backend by
# scripts/verify-supervise.sh. This btrfs suite focuses on the snapshot/rollback
# core: filesystem and dependency rollback above.

echo "############ ALL DONE ############"
