#!/usr/bin/env bash
# Record the killer demo and render a GIF that can be embedded in README.md.
#
# Runs the demo inside Docker as uid 1001 with no privileged (mirrors the
# restricted-pod case), records with asciinema, then renders with `agg`.
# Outputs: docs/demo.cast and docs/demo.gif.
#
#   bash scripts/make-demo-gif.sh
set -euo pipefail

cd "$(dirname "$0")/.."
mkdir -p docs

# The recording happens inside Docker so this script needs no host-side tools.
# Inside the container we:
#   1) install asciinema (pip) and download the agg binary (Cast -> GIF renderer)
#   2) build agentenv as the demo user (uid 1001)
#   3) asciinema-record the demo running as that user
#   4) render the cast to docs/demo.gif
docker run --rm --security-opt seccomp=unconfined \
  -v "$PWD":/src \
  -v "${HOME}/go/pkg/mod":/go/pkg/mod \
  -e GOPROXY="${GOPROXY:-http://goproxy.alibaba-inc.com,direct}" \
  -w /src golang:1.26 bash -c '
    set -e
    # --- 1) install asciinema + agg as root ---
    sed -i "s#\(deb\|security\)\.debian\.org#mirrors.aliyun.com#g" /etc/apt/sources.list.d/debian.sources 2>/dev/null || true
    apt-get update -qq
    # fonts-dejavu-core supplies DejaVu Sans Mono, one of the default agg font families.
    apt-get install -y -qq python3-pip wget ca-certificates fonts-dejavu-core >/dev/null 2>&1
    pip3 install --quiet --break-system-packages asciinema 2>/dev/null || pip3 install --quiet asciinema
    arch=$(uname -m)
    case "$arch" in
      x86_64)  AGG_URL=https://github.com/asciinema/agg/releases/download/v1.5.0/agg-x86_64-unknown-linux-gnu ;;
      aarch64) AGG_URL=https://github.com/asciinema/agg/releases/download/v1.5.0/agg-aarch64-unknown-linux-gnu ;;
      *) echo "unsupported arch: $arch" >&2; exit 1 ;;
    esac
    wget -qO /usr/local/bin/agg "$AGG_URL"
    chmod +x /usr/local/bin/agg

    # --- 2) build agentenv as the demo user (uid 1001) ---
    useradd -u 1001 -m demo
    mkdir -p /home/demo/.cache /home/demo/bin
    chown -R demo:demo /home/demo /src/docs
    su demo -c "
      cd /src
      HOME=/home/demo GOMODCACHE=/go/pkg/mod GOCACHE=/home/demo/.cache \
      CGO_ENABLED=0 GOPROXY=$GOPROXY \
        go build -o /home/demo/bin/agentenv .
    "

    # --- 3a) pre-init the env OUTSIDE the recording so the GIF doesnt show the
    #         ~2-minute base-image download. The demo skips init when it sees the
    #         pre-seeded AGENTENV_ROOT.
    arch=$(uname -m | sed "s/x86_64/amd64/;s/aarch64/arm64/")
    : "${AGENTENV_DEMO_TARBALL:=https://cdimage.ubuntu.com/ubuntu-base/releases/24.04/release/ubuntu-base-24.04.4-base-${arch}.tar.gz}"
    su demo -c "
      HOME=/home/demo \
      PATH=/home/demo/bin:/usr/local/bin:/usr/bin:/bin \
      AGENTENV_ROOT=/home/demo/agentfs \
      agentenv init --tarball '$AGENTENV_DEMO_TARBALL' >/dev/null
    "
    # --- 3b) record (still as uid 1001, the headline) ---
    su demo -c "
      HOME=/home/demo \
      PATH=/home/demo/bin:/usr/local/bin:/usr/bin:/bin \
      AGENTENV_ROOT=/home/demo/agentfs \
      TERM=xterm-256color \
      asciinema rec --overwrite --cols 100 --rows 32 \
        -c \"bash /src/examples/demo/killer-demo.sh\" \
        /src/docs/demo.cast
    "

    # --- 4) render the cast to a GIF (run as root, faster) ---
    # The demo has built-in reading pauses; render at original speed so they land.
    agg --theme monokai --speed 1.0 --font-size 15 \
      /src/docs/demo.cast /src/docs/demo.gif
    ls -la /src/docs/demo.cast /src/docs/demo.gif
  '

echo
echo "wrote: $(pwd)/docs/demo.cast"
echo "wrote: $(pwd)/docs/demo.gif"
