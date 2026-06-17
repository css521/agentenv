#!/usr/bin/env bash
# Drive the agentenv HTTP API with plain curl — no SDK, no client lib. The
# point: any tool that can hit a REST endpoint can drive the daemon, and the
# interactive docs at /docs (auto-generated from huma's OpenAPI 3.1 spec) tell
# a Web/AI developer exactly what's available.
#
# Start the daemon with HTTP enabled in another terminal:
#   agentenv daemon --http 127.0.0.1:8911              # loopback, no auth
#   AGENTENV_HTTP_TOKEN=secret agentenv daemon --http :8911   # exposed; token required
#
# Then:
#   bash examples/http_client.sh
set -euo pipefail
HOST="${HOST:-http://127.0.0.1:8911}"
AUTH=()
[ -n "${AGENTENV_HTTP_TOKEN:-}" ] && AUTH=(-H "Authorization: Bearer $AGENTENV_HTTP_TOKEN")

ae() { curl -fsS "${AUTH[@]}" "$@"; }

echo "=== HEAD ===";    ae "$HOST/v1/head"        | jq .
echo "=== log ===";     ae "$HOST/v1/log"         | jq '.nodes[] | {id, message, head, leaf}'
echo "=== commit ===";  ae -X POST "$HOST/v1/commit" -H 'content-type: application/json' \
                          -d '{"message":"snapshot from curl"}' | jq .
NEW=$(ae "$HOST/v1/head" | jq -r .head)
echo "new HEAD = $NEW"

echo "=== show this node ==="; ae "$HOST/v1/nodes/$NEW" | jq '.changes | length'

# delete a non-HEAD node by id — for the demo, the previous HEAD
PREV=$(ae "$HOST/v1/log" | jq -r '.nodes | map(select(.head|not)) | .[0].id // empty')
if [ -n "$PREV" ]; then
  echo "=== delete $PREV ==="
  ae -X DELETE "$HOST/v1/nodes/$PREV" | jq .
fi

echo "=== OpenAPI summary ==="
ae "$HOST/openapi.json" | jq '{openapi, info: .info | {title, version}, paths: .paths | keys}'
echo
echo "open $HOST/docs in a browser for the interactive UI"
