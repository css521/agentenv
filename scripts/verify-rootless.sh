#!/usr/bin/env bash
# Unified rootless E2E suite — one Docker run, one init, all assertions for the
# copy/rootless backend (the path agentenv uses in restricted K8s pods).
#
# Run as uid 1001, no --privileged, no extra caps:
#   docker run --rm --user 1001:1001 --security-opt seccomp=unconfined \
#     -v "$PWD":/src:ro -v "$HOME/go/pkg/mod":/go/pkg/mod:ro \
#     -e GOPROXY="$GOPROXY" -e CGO_ENABLED=0 -e GOCACHE=/tmp/gocache -e HOME=/tmp \
#     -w /src golang:1.26 bash /src/scripts/verify-rootless.sh
#
# Covers (in one ubuntu-init session so the slow image download happens once):
#   1) backend probe surfaces +reflink when supported (status command)
#   2) chmod-only change is preserved across commit + checkout (mode integrity)
#   3) system-package rollback (apt install tree+jq, checkout, only tree remains)
#   4) streaming exec: stdout frames arrive line-by-line, not buffered
#   5) tag set/list/checkout-by-name + checkout-by-ID-prefix
#   6) tournament runs candidates in TRUE PARALLEL (3×3s ≈ 3s wall, not 9s)
#   7) setuid/setgid/sticky bits preserved through init --from
#   8) newID has no collisions on rapid commits
set -euo pipefail

cd /src
# Use Aliyun mirror inside the env for apt (the only network step).
M='APT::Sandbox::User=root'
ALI_REWRITE='sed -i "s#//ports.ubuntu.com/ubuntu-ports#//mirrors.aliyun.com/ubuntu-ports#g" /etc/apt/sources.list.d/ubuntu.sources 2>/dev/null || true'

CGO_ENABLED=0 go build -o /tmp/agentenv .

# Pinned Ubuntu base rootfs (used by every script that does `agentenv init` for
# tests / demos). Override AGENTENV_DEMO_TARBALL to point at a different mirror,
# a local file, or a different distro tarball. arm64 / amd64 selected by uname.
arch=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
: "${AGENTENV_DEMO_TARBALL:=https://cdimage.ubuntu.com/ubuntu-base/releases/24.04/release/ubuntu-base-24.04.4-base-${arch}.tar.gz}"

# Inline streaming-test client (in Go, so we never need to apt-install python3
# from a non-root container). Built once; reused in section 4.
cat > /tmp/stream-test.go <<'GO'
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"time"
)

func main() {
	conn, err := net.Dial("unix", os.Args[1])
	if err != nil {
		fmt.Println("connect:", err)
		os.Exit(2)
	}
	defer conn.Close()
	req := `{"op":"exec","cmd":"for i in 1 2 3; do echo line$i; sleep 1; done"}` + "\n"
	conn.Write([]byte(req))
	t0 := time.Now()
	rd := bufio.NewReader(conn)
	var deltas []float64
	for {
		line, err := rd.ReadBytes('\n')
		if err != nil {
			os.Exit(2)
		}
		var f struct {
			Stdout, Error string
			OK            bool
		}
		json.Unmarshal(line, &f)
		if f.Stdout != "" {
			fmt.Printf("  +%4.1fs  %s", time.Since(t0).Seconds(), f.Stdout)
			deltas = append(deltas, time.Since(t0).Seconds())
		}
		if f.Error != "" {
			fmt.Println("error:", f.Error)
			os.Exit(1)
		}
		if f.OK {
			break
		}
	}
	maxGap := 0.0
	for i := 1; i < len(deltas); i++ {
		g := deltas[i] - deltas[i-1]
		if g > maxGap {
			maxGap = g
		}
	}
	if maxGap >= 0.5 {
		fmt.Printf("PASS: streaming works (max gap %.1fs — output arrived as produced)\n", maxGap)
	} else {
		fmt.Printf("FAIL: output was buffered (max gap %.1fs)\n", maxGap)
		os.Exit(1)
	}
}
GO
CGO_ENABLED=0 go build -o /tmp/stream-test /tmp/stream-test.go

export PATH=/tmp:$PATH AGENTENV_ROOT=/tmp/agentfs
mkdir -p /tmp/agentfs
agentenv init --tarball "$AGENTENV_DEMO_TARBALL" >/dev/null
agentenv exec -- bash -lc "$ALI_REWRITE" >/dev/null

# ============================================================================
echo "############ 1) agentenv status surfaces backend / inotify / disk ############"
agentenv status | tee /tmp/status.out
grep -qE 'backend\s+copy' /tmp/status.out || { echo FAIL; exit 1; }
grep -qE 'inotify limit\s+[0-9]+' /tmp/status.out || { echo FAIL; exit 1; }
if grep -q '+reflink' /tmp/status.out; then echo "PASS: status surfaces backend (+reflink active)"
else echo "PASS: status surfaces backend (reflink falls back to byte-copy on this fs)"; fi

# ============================================================================
echo "############ 2) chmod-only change preserved across commit+checkout ############"
agentenv exec -- bash -lc 'echo hi > /root/script.sh && chmod 644 /root/script.sh' >/dev/null
agentenv commit -m "mode 644" >/dev/null
N644=$(agentenv head)
agentenv exec -- bash -lc 'chmod 755 /root/script.sh' >/dev/null
agentenv commit -m "mode 755" >/dev/null
agentenv checkout "$N644" >/dev/null
mode=$(agentenv exec -- bash -lc 'stat -c %a /root/script.sh' 2>/dev/null)
[ "$mode" = "644" ] && echo "PASS: chmod-only change restored on checkout (mode=$mode)" || { echo "FAIL: mode=$mode (want 644)"; exit 1; }

# ============================================================================
echo "############ 3) system-package rollback (apt install tree, then jq, rollback drops jq) ############"
agentenv exec -- bash -lc "apt-get -o $M update -qq && apt-get -o $M install -y -qq tree" >/dev/null
agentenv commit -m "tree installed" >/dev/null
NTREE=$(agentenv head)
agentenv exec -- bash -lc "apt-get -o $M install -y -qq jq" >/dev/null
agentenv exec -- bash -lc 'command -v tree && command -v jq' >/dev/null
agentenv checkout "$NTREE" >/dev/null
agentenv exec -- bash -lc 'command -v tree >/dev/null' || { echo "FAIL: tree gone"; exit 1; }
if agentenv exec -- bash -lc 'command -v jq >/dev/null'; then echo "FAIL: jq should be gone"; exit 1; fi
echo "PASS: system package rollback (tree kept, jq gone)"

# ============================================================================
echo "############ 4) streaming exec via daemon: frames arrive as produced ############"
SOCK=/tmp/agentfs/api.sock
agentenv daemon --socket "$SOCK" >/tmp/daemon.out 2>&1 & DPID=$!
trap 'kill $DPID 2>/dev/null || true; wait $DPID 2>/dev/null || true' EXIT
sleep 1
/tmp/stream-test "$SOCK"
kill $DPID 2>/dev/null || true; wait $DPID 2>/dev/null || true; trap - EXIT

# ============================================================================
echo "############ 5) tag set/list/checkout-by-name + ID prefix ############"
agentenv tag good "$NTREE" >/dev/null
agentenv tag | grep -q '^good ' || { echo "FAIL: tag list"; exit 1; }
agentenv exec -- bash -lc 'echo dirty > /root/dirty' >/dev/null
agentenv commit -m "dirty" >/dev/null
agentenv checkout good >/dev/null
[ ! -f /tmp/agentfs/work/current/root/dirty ] || { echo "FAIL: dirty file should be gone after checkout good"; exit 1; }
agentenv checkout "${NTREE:0:6}" >/dev/null   # ID prefix
echo "PASS: tag + checkout-by-name + checkout-by-ID-prefix"

# ============================================================================
echo "############ 6) tournament runs 3 candidates IN PARALLEL ############"
agentenv tag base "$NTREE" >/dev/null
t0=$(date +%s)
agentenv tournament --base=base --keep --test='[ "$(cat /root/answer)" = 42 ]' -- \
  'sleep 3; echo 41 > /root/answer' \
  'sleep 3; echo 42 > /root/answer' \
  'sleep 3; echo 99 > /root/answer' >/tmp/t.out 2>&1
t1=$(date +%s); dt=$((t1 - t0))
echo "  elapsed: ${dt}s (sequential ≥9s, parallel target <8s)"
[ "$dt" -lt 8 ] || { echo "FAIL: tournament took ${dt}s, looks sequential"; cat /tmp/t.out; exit 1; }
got=$(agentenv exec -- bash -lc 'cat /root/answer' 2>/dev/null)
[ "$got" = "42" ] || { echo "FAIL: winner wrong (answer=$got)"; exit 1; }
echo "PASS: tournament parallel (${dt}s) + correct winner kept"

# ============================================================================
echo "############ 7) init --from preserves setuid/setgid/sticky ############"
DONOR=/tmp/donor
rm -rf "$DONOR" && mkdir -p "$DONOR/bin" "$DONOR/share/sticky"
echo '#!/bin/sh' > "$DONOR/bin/setuid-bin" && chmod 4755 "$DONOR/bin/setuid-bin"
echo '#!/bin/sh' > "$DONOR/bin/setgid-bin" && chmod 2755 "$DONOR/bin/setgid-bin"
chmod 1777 "$DONOR/share/sticky"
mkdir -p /tmp/donor-env
AGENTENV_ROOT=/tmp/donor-env agentenv init --from "$DONOR" >/dev/null
ok=true
for pp in "bin/setuid-bin:4755" "bin/setgid-bin:2755" "share/sticky:1777"; do
  p=${pp%%:*}; want=${pp##*:}
  got=$(stat -c %a "/tmp/donor-env/work/current/$p" 2>/dev/null)
  [ "$got" = "$want" ] && echo "  /$p $got ✓" || { echo "  /$p $got != $want ✗"; ok=false; }
done
$ok && echo "PASS: setuid/setgid/sticky preserved through init --from" || { echo FAIL; exit 1; }

# ============================================================================
echo "############ 8) newID: no collisions on rapid commits ############"
BEFORE=$(agentenv log | grep -coE '[0-9a-f]{24}')
for i in $(seq 1 30); do
  agentenv exec -- bash -lc "echo $i > /root/n$i" >/dev/null
  agentenv commit -m "n$i" >/dev/null
done
AFTER=$(agentenv log | grep -coE '[0-9a-f]{24}')
UNIQ=$(agentenv log | grep -oE '[0-9a-f]{24}' | sort -u | wc -l | tr -d ' ')
[ "$((AFTER - BEFORE))" = "30" ] && [ "$UNIQ" = "$AFTER" ] \
  && echo "PASS: 30 rapid commits → 30 distinct nodes, no collisions" \
  || { echo "FAIL: lost commits (before=$BEFORE after=$AFTER unique=$UNIQ)"; exit 1; }

echo "############ ROOTLESS ALL DONE ############"
