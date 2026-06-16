#!/usr/bin/env bash
# agentenv killer demo — tells a story with REAL shell commands every developer
# recognizes (mkdir/echo/cat/chmod/sed/grep/which/rm). Showcases the unique value:
# rolling back the WHOLE environment (a script in /usr/local/bin, a system config
# in /etc, AND a project in /workspace) with one command, plus branch exploration.
#
# Record & render a shareable GIF:   bash scripts/make-demo-gif.sh
set -euo pipefail

H='\033[1;36m'    # scene title
N='\033[0;37m'    # narration
CMD='\033[0;33m'  # the actual shell command being run
OK='\033[1;32m'   # success / payoff
OFF='\033[0m'

# Reading-paced helpers — the shell SOURCE itself is the documentation.
title() { printf "\n${H}━━━━━━━━━━━━━━━ %s ━━━━━━━━━━━━━━━${OFF}\n" "$1"; sleep 1; }
say()   { printf "${N}%s${OFF}\n" "$1"; sleep 1; }
ae()    { printf "${CMD}\$ agentenv %s${OFF}\n" "$*"; sleep 0.4; agentenv "$@"; sleep 1; }
# inenv: print one line of shell, then run it INSIDE the env (output visible).
inenv() { printf "${CMD}\$ %s${OFF}\n" "$1"; sleep 0.4; agentenv exec -- bash -lc "$1"; sleep 1; }
# quiet: same but suppress stdout (for multi-step setup that would clutter the GIF).
quiet() { printf "${CMD}\$ %s${OFF}\n" "$1"; sleep 0.4; agentenv exec -- bash -lc "$1" >/dev/null 2>&1; sleep 0.7; }
beat()  { sleep "${1:-2}"; }

if ! command -v agentenv >/dev/null; then
  cd /src && CGO_ENABLED=0 go build -o /tmp/agentenv . && export PATH=/tmp:$PATH
fi
export AGENTENV_ROOT=${AGENTENV_ROOT:-/tmp/agentfs}
# Honor a pre-seeded env (so the slow `init` doesn't bloat the GIF).
if [ ! -f "$AGENTENV_ROOT/meta.json" ]; then
  rm -rf "$AGENTENV_ROOT" && mkdir -p "$AGENTENV_ROOT"
  PRE_INIT=true
fi

# ==========================================================================
title "agentenv — time-travel & branch any environment, rootless"
say "An AI agent runs unmodified inside the env. agentenv records every change."
say "Any command, any language, any path. No privileged container. No special API."
beat 2

# ==========================================================================
title "Scene 1 — the agent sets up some real work"
if [ "${PRE_INIT:-}" = "true" ]; then
  arch=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
  : "${AGENTENV_DEMO_TARBALL:=https://cdimage.ubuntu.com/ubuntu-base/releases/24.04/release/ubuntu-base-24.04.4-base-${arch}.tar.gz}"
  ae init --tarball "$AGENTENV_DEMO_TARBALL"
fi
say "(a CLI tool in /usr/local/bin, a bashrc alias in /etc, a project in /workspace)"
quiet "echo '#!/bin/sh' > /usr/local/bin/calc && echo 'echo \$((\$1 + \$2))' >> /usr/local/bin/calc && chmod +x /usr/local/bin/calc"
quiet "echo \"alias ll='ls -la'\" >> /etc/bash.bashrc"
quiet "mkdir -p /workspace/myapp && echo 'tiny calculator demo' > /workspace/myapp/README.md"
ae commit -m "set up: calc tool + bashrc alias + project"
GOOD=$(agentenv head)
say "verify everything works:"
inenv "calc 2 3"
inenv "grep alias /etc/bash.bashrc"
inenv "ls /workspace/myapp"
beat 2

# ==========================================================================
title "Scene 2 — the agent breaks everything"
say "(one bad turn — deletes the tool, strips the alias, blows away the project)"
quiet "rm /usr/local/bin/calc && sed -i '/alias ll=/d' /etc/bash.bashrc && rm -rf /workspace/myapp"
ae commit -m "wrecked"
say "the env is wrecked — installed tool, system config, AND project all gone:"
inenv "which calc || echo calc:GONE"
inenv "grep alias /etc/bash.bashrc 2>/dev/null || echo bashrc-alias:GONE"
inenv "ls /workspace/myapp 2>/dev/null || echo /workspace:GONE"
beat 3

# ==========================================================================
title "Scene 3 — time travel: ONE command restores the whole environment"
say "Not just files in git — installed tools and system config come back too."
ae checkout "$GOOD"
inenv "calc 2 3"
inenv "grep alias /etc/bash.bashrc"
inenv "ls /workspace/myapp"
printf "${OK}✓ everything is back, anywhere on disk${OFF}\n"
beat 3

# ==========================================================================
title "Scene 4 — branch & explore: try 3 fixes IN PARALLEL, keep the one that passes"
agentenv tag good "$GOOD" >/dev/null
say "Task: pick the operator that makes 'mul 3 4' equal 12 — try '+', '*', '-' in parallel."
say "Each candidate sleeps 3s simulating real build time, so the speedup is visible:"
say "sequential would take 9s; truly parallel ≈ 3s."
echo
printf "${CMD}\$ agentenv tournament --base=good --keep --test='[ \"\$(mul 3 4)\" = 12 ]' -- \\\\${OFF}\n"
printf "${CMD}     'sleep 3; echo -e \"#!/bin/sh\\\\necho \\\$((\\\$1 + \\\$2))\" > /usr/local/bin/mul && chmod +x /usr/local/bin/mul' \\\\${OFF}\n"
printf "${CMD}     'sleep 3; echo -e \"#!/bin/sh\\\\necho \\\$((\\\$1 * \\\$2))\" > /usr/local/bin/mul && chmod +x /usr/local/bin/mul' \\\\${OFF}\n"
printf "${CMD}     'sleep 3; echo -e \"#!/bin/sh\\\\necho \\\$((\\\$1 - \\\$2))\" > /usr/local/bin/mul && chmod +x /usr/local/bin/mul'${OFF}\n"
sleep 1
t0=$(date +%s)
agentenv tournament --base=good --keep --test='[ "$(mul 3 4)" = 12 ]' -- \
  'sleep 3; echo -e "#!/bin/sh\necho \$((\$1 + \$2))" > /usr/local/bin/mul && chmod +x /usr/local/bin/mul' \
  'sleep 3; echo -e "#!/bin/sh\necho \$((\$1 * \$2))" > /usr/local/bin/mul && chmod +x /usr/local/bin/mul' \
  'sleep 3; echo -e "#!/bin/sh\necho \$((\$1 - \$2))" > /usr/local/bin/mul && chmod +x /usr/local/bin/mul'
t1=$(date +%s)
printf "\n${OK}↑ 3 × 3s candidates, wall-clock $((t1-t0))s (sequential would be ≥9s) — truly parallel${OFF}\n"
beat 3

# ==========================================================================
title "the resulting history"
agentenv log
beat 2
printf "\n${OK}rewind any node • branch & keep the best • agent zero-changes • self-hosted, unprivileged${OFF}\n"
beat 4
