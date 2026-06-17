#!/usr/bin/env bash
# Fetch the static tools the GIF recorder needs — asciinema (v3, single Rust
# binary), agg (cast→GIF renderer), and one monospace .ttf — into
# scripts/.recorder-bin/. No apt, no pip, no python: scripts/Dockerfile.recorder
# just COPYs these in.
#
# Behind a firewall? Route through a proxy, e.g.:
#   ALL_PROXY=socks5h://127.0.0.1:13659 bash scripts/fetch-recording-tools.sh
# (curl honors ALL_PROXY/HTTPS_PROXY.) Override an arch with ARCH=x86_64.
set -euo pipefail
cd "$(dirname "$0")"
mkdir -p .recorder-bin
ARCH="${ARCH:-$(uname -m | sed 's/arm64/aarch64/;s/amd64/x86_64/')}"
ASC_VER="${ASC_VER:-3.2.1}"
AGG_VER="${AGG_VER:-1.5.0}"   # 1.5 reads asciicast-v2 (we record with -f asciicast-v2)

get() { echo "fetching $2"; curl -fL -m 300 -o "$1" "$2"; }

get .recorder-bin/asciinema \
  "https://github.com/asciinema/asciinema/releases/download/v${ASC_VER}/asciinema-${ARCH}-unknown-linux-gnu"
get .recorder-bin/agg \
  "https://github.com/asciinema/agg/releases/download/v${AGG_VER}/agg-${ARCH}-unknown-linux-gnu"
get .recorder-bin/JetBrainsMono-Regular.ttf \
  "https://github.com/JetBrains/JetBrainsMono/raw/master/fonts/ttf/JetBrainsMono-Regular.ttf"

# DejaVu Sans Mono — fallback for box-drawing / geometric shapes that JetBrains
# Mono doesn't cover (Claude Code's TUI uses '⏺' and box characters).
# Distributed only inside a tarball; extract the one .ttf we need.
if [ ! -f .recorder-bin/DejaVuSansMono.ttf ]; then
  echo "fetching DejaVuSansMono.ttf"
  curl -fL -m 300 -o .recorder-bin/dejavu.tar.bz2 \
    "https://github.com/dejavu-fonts/dejavu-fonts/releases/download/version_2_37/dejavu-fonts-ttf-2.37.tar.bz2"
  tar -xjf .recorder-bin/dejavu.tar.bz2 -C .recorder-bin/ \
    --strip-components=2 dejavu-fonts-ttf-2.37/ttf/DejaVuSansMono.ttf
  rm -f .recorder-bin/dejavu.tar.bz2
fi

chmod +x .recorder-bin/asciinema .recorder-bin/agg
echo "ok → scripts/.recorder-bin/ (build: docker build -f scripts/Dockerfile.recorder -t rewindable-claude-rec scripts/)"
