#!/usr/bin/env bash
# agentenv self-rollback demo — an agent makes SYSTEM-LEVEL changes, realizes
# it went the wrong way, and rolls the WHOLE environment back ITSELF, then keeps
# going. The one thing git can't do (system binaries, /etc) and the agent never
# even restarts. Zero mounts, rootless.
#
# Record & render a shareable GIF:   DEMO_SCRIPT=examples/demo/self-rollback-demo.sh bash scripts/make-demo-gif.sh
set -euo pipefail

H='\033[1;36m'   # scene title
N='\033[0;37m'   # narration (the agent "thinking")
CMD='\033[0;33m' # the actual command
OK='\033[1;32m'  # payoff
BAD='\033[1;31m' # the mistake
OFF='\033[0m'

title() { printf "\n${H}━━━━━━━━━━ %s ━━━━━━━━━━${OFF}\n" "$1"; sleep 1.2; }
say()   { printf "${N}🤖 %s${OFF}\n" "$1"; sleep 1.3; }
ae()    { printf "${CMD}\$ agentenv %s${OFF}\n" "$*"; sleep 0.4; agentenv "$@"; sleep 1; }
inenv() { printf "${CMD}\$ %s${OFF}\n" "$1"; sleep 0.4; agentenv exec -- bash -lc "$1"; sleep 1; }
quiet() { agentenv exec -- bash -lc "$1" >/dev/null 2>&1; }

if ! command -v agentenv >/dev/null; then
  cd /src && CGO_ENABLED=0 go build -o /tmp/agentenv . && export PATH=/tmp:$PATH
fi
export AGENTENV_ROOT=${AGENTENV_ROOT:-/tmp/agentfs}

# ==========================================================================
title "agentenv — an agent that undoes its own mistakes"
say "I'm an AI agent running INSIDE a rewindable environment."
say "Everything I do — files, system binaries, /etc, installs — is auto-snapshotted."
GOOD=$(agentenv head)
sleep 1

title "1. Approach A — I change the SYSTEM, not just files"
say "Installing a 'greet' command into /usr/local/bin (a system binary)..."
inenv "printf '#!/bin/sh\necho hello form greet\n' > /usr/local/bin/greet && chmod +x /usr/local/bin/greet"
say "Test it:"
inenv "greet"
printf "${BAD}🤖 Typo — 'form', not 'from'. And this whole approach is a dead end.${OFF}\n"; sleep 2
ae commit -m "approach A (broken)"

title "2. I roll the WHOLE environment back — MYSELF"
say "git can't undo a system binary. I can — and I won't even restart."
ae checkout "$GOOD"
say "Same agent, still running. /usr/local/bin/greet is gone, system-wide:"
inenv "command -v greet || echo 'greet: gone'"
sleep 1

title "3. Approach B — try again from the clean state"
inenv "printf '#!/bin/sh\necho hello from greet\n' > /usr/local/bin/greet && chmod +x /usr/local/bin/greet"
inenv "greet"
ae commit -m "approach B (works)"
printf "${OK}🤖 Fixed. I explored a dead end, undid the whole environment, kept going.${OFF}\n"; sleep 2

title "the history — A was a dead end, B is the keeper"
ae log
printf "\n${OK}agentenv — rewind the whole environment, rootless. The agent drives it.${OFF}\n"
sleep 2
