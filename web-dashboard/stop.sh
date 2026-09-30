#!/usr/bin/env bash
# stop.sh -- stop the web dashboard (pidfile-exact, like master-master/stop.sh).
set -u
cd "$(dirname "$0")/.." || exit 1
RUN_DIR="$PWD/run"

pid="$(cat "$RUN_DIR/web-dashboard.pid" 2>/dev/null)"
if [ -z "$pid" ]; then
  echo "not running: no pidfile"
  exit 0
fi
if kill -0 "$pid" 2>/dev/null; then
  kill "$pid"
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    kill -0 "$pid" 2>/dev/null || break
    sleep 1
  done
  if kill -0 "$pid" 2>/dev/null; then
    echo "still running ($pid); left alone"
    exit 1
  fi
fi
rm -f "$RUN_DIR/web-dashboard.pid"
echo "stopped web-dashboard"
