#!/usr/bin/env bash
# Regenerate docs/openapi.json from the live daemon's /openapi.json.
#
# The HTTP transport uses huma to derive the OpenAPI 3.1 spec from the typed
# Go handler signatures at runtime — so the authoritative source is whatever
# `GET /openapi.json` returns from a running daemon. We keep a snapshot of
# that file in the repo so:
#   - GitHub can render it (the spec viewer + the README link work without
#     running anything),
#   - generated SDK builds can pin to a known version,
#   - PRs that change the surface show a diff in CI review.
#
# Run before tagging a release; commit the result alongside the code change.
#
#   make openapi-snapshot
set -euo pipefail
cd "$(dirname "$0")/.."

# Build a binary for this host and start a one-off daemon in an ephemeral
# rootfs. Everything is cleaned up in the EXIT trap.
ARCH=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/;s/arm64/arm64/')
BIN=$(mktemp -t ae-snap-bin.XXXXXX)
ROOT=$(mktemp -d -t ae-snap-root.XXXXXX)
LOG=$(mktemp -t ae-snap-log.XXXXXX)
PIDFILE=$(mktemp -t ae-snap-pid.XXXXXX)
PORT="${PORT:-18799}"
cleanup() {
  [ -f "$PIDFILE" ] && kill "$(cat "$PIDFILE")" 2>/dev/null || true
  rm -f "$BIN" "$LOG" "$PIDFILE"
  rm -rf "$ROOT"
}
trap cleanup EXIT

echo "building agentenv for $ARCH..."
GOOS=linux GOARCH=$ARCH CGO_ENABLED=0 go build -o "$BIN" . >/dev/null

# Run the daemon inside Docker (rootless userns needs Linux + relaxed seccomp).
docker run --rm -d \
  --platform="linux/$ARCH" \
  --security-opt seccomp=unconfined --security-opt apparmor=unconfined \
  --name agentenv-openapi-snapshot \
  --entrypoint /bin/sh \
  -v "$BIN":/usr/local/bin/agentenv:ro \
  -p "127.0.0.1:$PORT:8911" \
  ubuntu:24.04 \
  -c "mkdir -p /seed && AGENTENV_ROOT=/aenv agentenv init --from /seed >/dev/null 2>&1 && AGENTENV_HTTP_TOKEN=snap AGENTENV_ROOT=/aenv exec agentenv daemon --http :8911" \
  > "$PIDFILE"

# Wait for the listener.
for _ in $(seq 1 30); do
  if curl -fsS -H 'Authorization: Bearer snap' "http://127.0.0.1:$PORT/openapi.json" -o docs/openapi.json 2>/dev/null; then
    break
  fi
  sleep 0.3
done
test -s docs/openapi.json || { echo "snapshot failed — daemon never served /openapi.json"; exit 1; }

# Pretty-print so diffs are readable. (huma serves compact JSON.)
python3 -m json.tool docs/openapi.json > docs/openapi.json.tmp && mv docs/openapi.json.tmp docs/openapi.json

docker rm -f agentenv-openapi-snapshot >/dev/null 2>&1 || true
echo "wrote docs/openapi.json ($(wc -c <docs/openapi.json) bytes)"
