#!/usr/bin/env bash
# Record a GIF of REAL Claude Code rolling back its own environment via the
# agentenv__checkout MCP tool. Unlike the scripted demos, this runs the actual
# `claude` binary inside the rewindable-claude image and captures its output.
#
# Needs the rewindable-claude image built locally and an API key in the env:
#   ANTHROPIC_API_KEY=... [ANTHROPIC_BASE_URL=... ANTHROPIC_MODEL=...] \
#     bash scripts/make-claude-gif.sh
#
# How it works: a small recorder container (with the docker CLI + asciinema +
# agg, and the host docker socket mounted) records a sibling `docker run` of the
# rewindable-claude image driving `claude -p`. Output: docs/claude-rewind.{cast,gif}.
set -euo pipefail
cd "$(dirname "$0")/.."
mkdir -p docs

: "${ANTHROPIC_API_KEY:?set ANTHROPIC_API_KEY}"
# Default to the prebuilt recorder image (rewindable-claude + asciinema + agg);
# build it once with scripts/Dockerfile.recorder so recording is fast.
IMAGE="${IMAGE:-rewindable-claude-rec}"
PLATFORM="${PLATFORM:-linux/$(uname -m | sed 's/x86_64/amd64/;s/arm64/arm64/;s/aarch64/arm64/')}"

# The prompt: explicit steps so Claude reliably uses the MCP tools on camera.
# MUST be a single line — it travels through several layers of shell quoting.
PROMPT='You have agentenv MCP tools. Do these steps, printing a one-line note before each: (1) write /work/calc.py containing "def add(a, b): return a - b  # BUG should be +"; (2) call agentenv__log, the first node whose message starts with "init from" is the clean start; (3) call agentenv__checkout with that node id to roll the WHOLE environment back; (4) run: cat /work/calc.py  — it should be gone; (5) in one sentence, say you rolled your own environment back and are still running.'

export PROMPT   # must be exported so `docker run -e PROMPT` forwards it

# Record INSIDE the recorder image (rewindable-claude + asciinema + agg, built
# once via scripts/Dockerfile.recorder). asciinema records the image's own
# entrypoint driving `claude -p` — the REAL Claude Code self-rollback.
docker run --rm --platform="$PLATFORM" \
  --security-opt seccomp=unconfined --security-opt apparmor=unconfined \
  -v "$PWD/docs":/out \
  -v "$PWD/scripts/_claude-gif-inner.sh":/rec.sh:ro \
  -e ANTHROPIC_API_KEY -e ANTHROPIC_BASE_URL -e ANTHROPIC_MODEL -e PROMPT \
  --entrypoint bash \
  "$IMAGE" -c '
    set -e
    command -v asciinema >/dev/null || { echo "recorder image missing asciinema — fetch-recording-tools.sh + build scripts/Dockerfile.recorder" >&2; exit 1; }

    # /rec.sh (mounted) invokes the image entrypoint → supervise --self-rollback
    # -- claude. asciinema (v3 binary) records in v2 so agg 1.5 can read it.
    TERM=xterm-256color asciinema rec --overwrite -f asciicast-v2 --cols 100 --rows 30 \
      -c "bash /rec.sh" /out/claude-rewind.cast

    agg --font-dir "${AGENTENV_GIF_FONT_DIR:-/usr/local/share/fonts}" --font-family "${AGENTENV_GIF_FONT:-JetBrains Mono}" \
      --theme monokai --speed 1.5 --font-size 15 /out/claude-rewind.cast /out/claude-rewind.gif
    ls -la /out/claude-rewind.cast /out/claude-rewind.gif
  '
echo "wrote: $(pwd)/docs/claude-rewind.gif"
