#!/bin/bash
# Devcontainer postCreate hook.
#
# Two responsibilities:
#  1. If $AGENTENV_CN_MIRROR=1 (passed in from the host's env via
#     devcontainer.json's containerEnv block), rewrite Debian's apt sources
#     to mirrors.aliyun.com so apt-get doesn't stall on deb.debian.org.
#     Useful inside mainland China — OSS contributors elsewhere skip it.
#  2. Install the optional build deps libbtrfs-dev (for `-tags btrfs`
#     builds — the default pure-Go build needs nothing) and make.

set -e

if [ "$AGENTENV_CN_MIRROR" = "1" ]; then
  echo "[postcreate] AGENTENV_CN_MIRROR=1 → switching apt sources to mirrors.aliyun.com"
  # `*.sources` is Debian's modern deb822 format (trixie/sid); older releases
  # still use the single-line sources.list. Rewrite whichever is present. Only
  # the host needs swapping (security lives at deb.debian.org/debian-security,
  # not a separate host); use '#' as the sed delimiter so it doesn't collide
  # with any '|' in the expression.
  sudo sed -ri 's#deb\.debian\.org#mirrors.aliyun.com#g' \
    /etc/apt/sources.list.d/*.sources /etc/apt/sources.list 2>/dev/null || true
fi

sudo apt-get update
sudo apt-get install -y -qq libbtrfs-dev make

# Build the agentenv binary onto PATH so `agentenv` and the rewindable-claude
# launcher just work in the dev container / Codespace without a manual build.
echo "[postcreate] building agentenv → ~/go/bin/agentenv"
mkdir -p "$HOME/go/bin"
CGO_ENABLED=0 go build -o "$HOME/go/bin/agentenv" .

# Install Claude Code if npm is present and it isn't already there, so the
# rewindable-claude launcher has something to wrap. (Skipped if npm is missing
# — the launcher still works for any other command.)
if command -v npm >/dev/null 2>&1 && ! command -v claude >/dev/null 2>&1; then
  echo "[postcreate] installing @anthropic-ai/claude-code"
  [ "$AGENTENV_CN_MIRROR" = "1" ] && npm config set registry https://registry.npmmirror.com || true
  sudo npm install -g @anthropic-ai/claude-code || \
    echo "[postcreate] claude-code install failed (install it yourself: npm i -g @anthropic-ai/claude-code)"
fi
