#!/usr/bin/env bash
# Start the local demo: mock providers + upstream + relay.
# 前置：cd backend && go build -o upstream.exe ./cmd/upstream && go build -o relay.exe ./cmd/relay && go build -o relaymock.exe ./cmd/relaymock
set -e
cd "$(dirname "$0")/.." # backend/

pkill -f relaymock.exe 2>/dev/null || true
pkill -f upstream.exe 2>/dev/null || true
pkill -f relay.exe 2>/dev/null || true
sleep 1

./relaymock.exe -config demo/relaymock.yaml > demo/relaymock.log 2>&1 &
./upstream.exe -config demo/upstream.yaml > upstream.log 2>&1 &

for _ in $(seq 1 50); do
  if curl -sf -H "Authorization: Bearer local-service-key" http://127.0.0.1:18100/internal/v1/health >/dev/null; then
    break
  fi
  sleep 0.1
done

register() {
  curl -sf -X POST http://127.0.0.1:18100/admin/accounts \
    -H "X-Admin-Key: change-upstream-admin-key" -H "Content-Type: application/json" \
    -d "{\"name\":\"$1\",\"type\":\"api-key\",\"protocol\":\"$2\",\"base_url\":\"http://127.0.0.1:$3\",\"api_key\":\"$4\",\"models\":{\"$5\":\"$5\"}}" \
    >/dev/null || true
}
register st-a openai-chat 9001 sk-mock-a gpt-4o
register cl-01 anthropic 9003 sk-mock-c claude-sonnet-4

./relay.exe -config demo/relay.yaml > relay.log 2>&1 &
for _ in $(seq 1 50); do
  if curl -sf http://127.0.0.1:18099/health >/dev/null; then
    echo
    echo "ModelSurge up: http://127.0.0.1:18099 (api key: change-client-key)"
    exit 0
  fi
  sleep 0.1
done

echo "relay did not become ready" >&2
exit 1
