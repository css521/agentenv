#!/bin/bash
# Inner command for make-claude-gif.sh, run INSIDE the rewindable-claude image
# under asciinema. Invokes the image's own entrypoint (which runs
# `supervise --self-rollback -- ...`) so this is the REAL Claude Code rolling
# back its OWN environment via the agentenv__checkout MCP tool. Reads PROMPT and
# ANTHROPIC_* from the environment.
exec /usr/local/bin/rewindable-claude-entrypoint \
  claude --permission-mode acceptEdits --verbose \
    --allowedTools Write,Bash,mcp__agentenv__agentenv__log,mcp__agentenv__agentenv__checkout \
    -p "$PROMPT"
