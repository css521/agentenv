#!/bin/bash
# Entrypoint for the rewindable Claude Code image.
#
#  - Forwards ANTHROPIC_* / CLAUDE_* host env into the agentenv sandbox by
#    re-exporting them as AGENTENV_PASS_<VAR> (agentenv's allow-list strips the
#    prefix and passes them through to commands run inside the env). Without
#    this, `claude` inside `agentenv shell` wouldn't see your API key.
#  - Seeds the managed rootfs if it wasn't baked at build time (fallback).
#  - `idle` (default CMD): keep the container alive so you drive it over
#    `docker exec` (agentenv shell / log / checkout). Any other args run
#    headless under supervise.
set -e

: "${AGENTENV_ROOT:=/var/lib/agentenv}"
export AGENTENV_ROOT

# Forward auth/config into the sandbox. e.g. ANTHROPIC_API_KEY → the agent
# command inside `agentenv shell` sees ANTHROPIC_API_KEY. We mirror the whole
# ANTHROPIC_* and CLAUDE_* families plus a couple of proxy vars.
for var in $(env | grep -oE '^(ANTHROPIC_[A-Z_]+|CLAUDE_[A-Z_]+|HTTPS?_PROXY|NO_PROXY)=' | tr -d '='); do
  val="$(printenv "$var" || true)"
  [ -n "$val" ] && export "AGENTENV_PASS_$var=$val"
done

# Fallback seed (normally already baked at build time via SEED_AT_BUILD).
if [ ! -f "$AGENTENV_ROOT/meta.json" ]; then
  echo "rewindable-claude: seeding managed rootfs (first run)..."
  agentenv init --from /
fi

case "${1:-idle}" in
  idle)
    cat <<'EOF'
rewindable-claude is ready. Drive it from your host:

  docker exec -it <container> agentenv shell      # work inside; run `claude`
  docker exec    <container> agentenv log         # list auto-snapshots
  docker exec    <container> agentenv checkout <id>  # rewind the whole env

(Run agentenv shell + checkout sequentially — they share the repo lock.)
EOF
    # Idle forever; all real work happens over docker exec.
    exec tail -f /dev/null
    ;;
  *)
    # Headless: run whatever was passed under supervise (auto-snapshot +
    # rollback-restart). Good for `claude -p "<task>"` style one-shots.
    exec agentenv supervise -- "$@"
    ;;
esac
