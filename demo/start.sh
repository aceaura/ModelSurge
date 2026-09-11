#!/usr/bin/env bash
# Start the local demo: mock upstreams + relayd.
set -e
cd "$(dirname "$0")/.."

pkill -f relaymock.exe 2>/dev/null || true
pkill -f 'relayd.exe' 2>/dev/null || true
sleep 1

./relaymock.exe -config demo/relaymock.yaml > demo/relaymock.log 2>&1 &
./relayd.exe -config relayd.yaml > relayd.log 2>&1 &
sleep 1

curl -sf http://127.0.0.1:8080/health \
  && echo && echo "relayd up: http://127.0.0.1:8080 (api key: sk-local-change-me)"
