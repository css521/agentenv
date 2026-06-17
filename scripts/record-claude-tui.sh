#!/usr/bin/env bash
# Record a GIF of a REAL interactive Claude Code session rolling back its own
# environment (the iconic TUI with the "⏺ agentenv__checkout(...)" tool cards).
#
# IMPORTANT: run this IN YOUR TERMINAL — it needs a real TTY and you drive
# Claude live. It cannot be produced headlessly (claude -p only prints the final
# answer; the step-by-step tool cards exist only in the interactive UI).
#
#   ANTHROPIC_API_KEY=... [ANTHROPIC_BASE_URL=... ANTHROPIC_MODEL=...] \
#     bash scripts/record-claude-tui.sh
#
# What happens: you land in Claude Code inside the rewindable sandbox. Give it a
# task, let it work, then ask it to roll back — e.g.:
#
#   create /work/calc.py with a bug, then use the agentenv__checkout tool to
#   roll the whole environment back to the init node, and confirm the file is gone
#
# Watch it call agentenv__checkout and KEEP RUNNING. Exit with Ctrl-D / /exit;
# the GIF renders to docs/claude-rewind.gif.
set -euo pipefail
cd "$(dirname "$0")/.."
: "${ANTHROPIC_API_KEY:?set ANTHROPIC_API_KEY}"
mkdir -p docs

IMAGE="${IMAGE:-rewindable-claude-rec}"   # built via scripts/Dockerfile.recorder (has asciinema + agg)
PLATFORM="${PLATFORM:-linux/$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')}"

docker run --rm -it --platform="$PLATFORM" \
  --security-opt seccomp=unconfined --security-opt apparmor=unconfined \
  -v "$PWD/docs":/out \
  -e ANTHROPIC_API_KEY -e ANTHROPIC_BASE_URL -e ANTHROPIC_MODEL \
  --entrypoint bash "$IMAGE" -lc '
    command -v asciinema agg >/dev/null || { echo "image lacks asciinema/agg — build scripts/Dockerfile.recorder" >&2; exit 1; }
    echo
    echo "  Claude Code starts now, inside the rewindable env."
    echo "  Try: \"write /work/calc.py with a bug, then use agentenv__checkout to roll the env back to the init node and confirm it is gone\""
    echo "  Exit with Ctrl-D when done — the GIF will render."
    echo
    sleep 2
    TERM=xterm-256color asciinema rec --overwrite --cols 100 --rows 30 \
      -c "/usr/local/bin/rewindable-claude-entrypoint claude --permission-mode acceptEdits" \
      /out/claude-rewind.cast
    agg --theme monokai --speed 1.2 --font-size 15 /out/claude-rewind.cast /out/claude-rewind.gif
    echo "wrote docs/claude-rewind.gif"
  '
echo "wrote: $(pwd)/docs/claude-rewind.gif"
