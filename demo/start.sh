#!/usr/bin/env bash
# Start the full local demo: 3 mock upstreams + relayd + Flutter client.
set -e
cd "$(dirname "$0")/.."

pkill -f relaymock.exe 2>/dev/null || true
pkill -f 'relayd.exe' 2>/dev/null || true
sleep 1

./relaymock.exe -config demo/relaymock.yaml > demo/relaymock.log 2>&1 &
./relayd.exe -config relayd.yaml > relayd.log 2>&1 &
sleep 1

curl -sf -H "Authorization: Bearer sk-local-change-me" http://127.0.0.1:8081/admin/summary \
  && echo && echo "relayd up: relay=:8080 admin=:8081"

CLIENT=frontend/build/windows/x64/runner/Release/relayd_client.exe
if [ -f "$CLIENT" ]; then
  (cd "$(dirname "$CLIENT")" && ./relayd_client.exe > /dev/null 2>&1 &)
  echo "client started (token: sk-local-change-me)"
fi
