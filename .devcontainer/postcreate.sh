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
  # still use the single-line sources.list. Rewrite whichever is present.
  sudo sed -ri 's|http(s)?://(deb|security)\.debian\.org|http\1://mirrors.aliyun.com|g' \
    /etc/apt/sources.list.d/*.sources /etc/apt/sources.list 2>/dev/null || true
fi

sudo apt-get update
sudo apt-get install -y -qq libbtrfs-dev make
