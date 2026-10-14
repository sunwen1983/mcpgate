#!/usr/bin/env bash
# demo.sh — end-to-end demo of mcpgate against the echo backend.
#
# Builds mcpgate + echo-backend, starts the gateway on :8080, then shows:
#   1. 401 without an API key
#   2. tools/list through the gateway (X-Cache: MISS, then HIT)
#   3. tools/call add (cached: echo/add are cacheable in examples/config.yaml)
#   4. rate limiting: 12 rapid requests against the demo key (5 req/s, burst 10)
#      produce 429s once the bucket drains
#   5. /healthz and a /metrics excerpt
set -euo pipefail
cd "$(dirname "$0")/.."

GATEWAY_PORT=8080
API_KEY="demo-key-123"
BASE="http://localhost:$GATEWAY_PORT"
WORK=/tmp/mcpgate-demo
GATEWAY_PID=""

cleanup() {
  if [ -n "$GATEWAY_PID" ]; then kill "$GATEWAY_PID" 2>/dev/null || true; fi
  rm -f ./echo-backend
}
trap cleanup EXIT

mkdir -p "$WORK"

echo "== building =="
go build -o "$WORK/mcpgate" ./cmd/mcpgate
go build -o "$WORK/echo-backend" ./examples/echo-backend
# The echo backend is resolved relative to the gateway's working directory
# in examples/config.yaml ("./echo-backend"), so put a copy there.
cp "$WORK/echo-backend" ./echo-backend

echo "== starting gateway =="
"$WORK/mcpgate" -config examples/config.yaml > "$WORK/gateway.log" 2>&1 &
GATEWAY_PID=$!
for _ in $(seq 1 30); do
  curl -sf "$BASE/healthz" > /dev/null && break
  sleep 0.2
done

echo
echo "== 1. no API key -> 401 =="
curl -s -X POST "$BASE/mcp/echo" \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}'
echo

echo "== 2. tools/list, first call -> X-Cache: MISS =="
curl -s -D - -X POST "$BASE/mcp/echo" \
  -H 'Content-Type: application/json' -H "X-API-Key: $API_KEY" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' | grep -i '^X-Cache\|"name"'
echo

echo "== 2b. tools/list, second call -> X-Cache: HIT =="
curl -s -D - -X POST "$BASE/mcp/echo" \
  -H 'Content-Type: application/json' -H "X-API-Key: $API_KEY" \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' | grep -i '^X-Cache'
echo

echo "== 3. tools/call add(2, 3) =="
curl -s -X POST "$BASE/mcp/echo" \
  -H 'Content-Type: application/json' -H "X-API-Key: $API_KEY" \
  -d '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"add","arguments":{"a":2,"b":3}}}'
echo

echo "== 4. rate limit: 12 rapid requests (demo key: 5 req/s, burst 10) =="
for _ in $(seq 1 12); do
  code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/mcp/echo" \
    -H 'Content-Type: application/json' -H "X-API-Key: $API_KEY" \
    -d '{"jsonrpc":"2.0","id":9,"method":"tools/list"}')
  printf '%s ' "$code"
done
echo "(expect 200s, then 429s once the bucket drains)"
echo

echo "== 5. /healthz + /metrics excerpt =="
curl -s "$BASE/healthz"
curl -s "$BASE/metrics" | grep '^mcpgate_' | head -8
echo
echo "demo done. gateway log: $WORK/gateway.log"
