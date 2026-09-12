#!/usr/bin/env bash
# Start the local demo: mock upstreams + relayd.
# 前置：cd backend && go build -o relayd.exe ./cmd/relayd && go build -o relaymock.exe ./cmd/relaymock
set -e
cd "$(dirname "$0")/.." # backend/

pkill -f relaymock.exe 2>/dev/null || true
pkill -f 'relayd.exe' 2>/dev/null || true
sleep 1

./relaymock.exe -config demo/relaymock.yaml > demo/relaymock.log 2>&1 &
./relayd.exe -config relayd.yaml > relayd.log 2>&1 &
sleep 1

# mock 账号注册（SQLite-only：yaml 不配上游；已存在 409 忽略）
register() {
  curl -sf -X POST http://127.0.0.1:8080/admin/accounts \
    -H "X-Admin-Key: sk-admin-change-me" -H "Content-Type: application/json" \
    -d "{\"name\":\"$1\",\"type\":\"api-key\",\"protocol\":\"$2\",\"base_url\":\"http://127.0.0.1:$3\",\"api_key\":\"$4\",\"models\":{\"$5\":\"$5\"}}" \
    >/dev/null || true
}
register st-a openai-chat 9001 sk-mock-a gpt-4o
register cl-01 anthropic 9003 sk-mock-c claude-sonnet-4

curl -sf http://127.0.0.1:8080/health \
  && echo && echo "relayd up: http://127.0.0.1:8080 (api key: sk-local-change-me)"
