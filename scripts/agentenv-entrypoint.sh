#!/bin/sh
# Transparent injection entrypoint: seed the managed environment from this
# image's filesystem (once), then run the given agent command INSIDE it under
# supervision (auto-snapshot + rollback-restart). The agent is unchanged and
# unaware of agentenv — it runs any command, in any language, at any path.
#
#   ENTRYPOINT ["/usr/local/bin/agentenv-entrypoint"]   CMD = your agent command
#
# Rollback is out-of-band, via the control socket (default
# $AGENTENV_ROOT/agentenv.sock), e.g. from a sidecar/orchestrator. Read-only
# inspection also works via the CLI (agentenv log / show / diff) — they take no
# lock.
#
# Security note:
#   `init --from /` snapshots the whole container rootfs, which on Kubernetes
#   includes mounted secrets (/run/secrets, /var/run/secrets/*) and may include
#   /etc/shadow. agentenv stores snapshots under AGENTENV_ROOT, where the agent
#   can read them. To keep secrets out of the snapshot store we ALWAYS exclude a
#   short default list; you can extend it via AGENTENV_EXCLUDE (colon-separated).
set -e

: "${AGENTENV_ROOT:=/var/lib/agentenv}"
export AGENTENV_ROOT
mkdir -p "$AGENTENV_ROOT"

# Default exclusions for the seed-from-/ case. /proc, /sys, /dev are already
# handled by agentenv internally (they're remounted in the sandbox).
DEFAULT_EXCLUDE="/etc/shadow:/etc/gshadow:/run/secrets:/var/run/secrets:/root/.ssh:/etc/ssh/ssh_host_*_key"
EXCLUDE="${AGENTENV_EXCLUDE:-$DEFAULT_EXCLUDE}"

if [ ! -f "$AGENTENV_ROOT/meta.json" ]; then
  echo "agentenv: seeding managed environment from / (one-time copy)"
  echo "agentenv: excluding secret paths: $EXCLUDE"
  # Move excluded files out of the way for the duration of `init --from /`, then
  # put them back. (init --from doesn't expose a --exclude flag yet — this is
  # the pragmatic shell-level solution.)
  STASH="$(mktemp -d /tmp/agentenv-stash.XXXXXX)"
  i=0
  OLDIFS=$IFS; IFS=':'
  for pat in $EXCLUDE; do
    for src in $pat; do   # glob in case pat is a wildcard
      [ -e "$src" ] || continue
      dst="$STASH/$i"; i=$((i+1))
      mv "$src" "$dst" 2>/dev/null && echo "$src" > "$dst.path" || true
    done
  done
  IFS=$OLDIFS

  agentenv init --from /

  for meta in "$STASH"/*.path; do
    [ -f "$meta" ] || continue
    src=$(cat "$meta"); dst="${meta%.path}"
    mv "$dst" "$src" 2>/dev/null || true
  done
  rm -rf "$STASH"
fi

exec agentenv supervise -- "$@"
