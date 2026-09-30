#!/usr/bin/env bash
# start.sh -- start ONLY the web dashboard (:8090).
#
# Standalone like master-master/start.sh: the main scripts/start-services.sh
# does not know this file. This is the first-run entry point -- with no
# run/secrets.env yet, the rest of the stack cannot start, but the dashboard
# can: it serves the Setup tab (packages, secrets.env editor, setup runner,
# service control) that bootstraps everything else.
#
# ADMIN_KEY resolution (first hit wins):
#   1. $ADMIN_KEY from the environment,
#   2. ADMIN_KEY from run/secrets.env (after the first setup),
#   3. ADMIN_KEY from run/web-dashboard.env,
#   4. freshly generated into run/web-dashboard.env (0600).
# After the first setup the generated key is copied into run/secrets.env by
# the operator (the Setup tab shows it), so both files agree from then on.
set -u
cd "$(dirname "$0")/.." || exit 1
ROOT="$PWD"
RUN_DIR="$ROOT/run"
export REPO_ROOT="$ROOT"

if [ ! -x "$RUN_DIR/web-dashboard" ]; then
  echo "missing binary: run/web-dashboard -- run bash scripts/setup.sh (or build it: cd web-dashboard && go build -o ../run/web-dashboard .)" >&2
  exit 1
fi
mkdir -p "$RUN_DIR"

# pidfile first (portable and exact), then pgrep.
pid="$(cat "$RUN_DIR/web-dashboard.pid" 2>/dev/null)"
if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
  echo "already running: web-dashboard ($pid)"
  exit 0
fi

resolve_key() {
  if [ -n "${ADMIN_KEY:-}" ]; then printf '%s' "$ADMIN_KEY"; return 0; fi
  if [ -f "$RUN_DIR/secrets.env" ]; then
    v="$(grep -E '^ADMIN_KEY=' "$RUN_DIR/secrets.env" | tail -1 | cut -d= -f2-)"
    if [ -n "$v" ]; then printf '%s' "$v"; return 0; fi
  fi
  if [ -f "$RUN_DIR/web-dashboard.env" ]; then
    # shellcheck disable=SC1091
    . "$RUN_DIR/web-dashboard.env"
    if [ -n "${ADMIN_KEY:-}" ]; then printf '%s' "$ADMIN_KEY"; return 0; fi
  fi
  return 1
}

if ! ADMIN_KEY="$(resolve_key)"; then
  ADMIN_KEY="$(openssl rand -hex 32 2>/dev/null || head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"
  printf 'ADMIN_KEY=%s\n' "$ADMIN_KEY" >"$RUN_DIR/web-dashboard.env"
  chmod 600 "$RUN_DIR/web-dashboard.env"
  echo "[*] generated first-run admin key into run/web-dashboard.env (0600)"
  echo "    after the first setup, copy it into run/secrets.env (the Setup tab shows it)"
fi
export ADMIN_KEY
export DASHBOARD_ADDR="${DASHBOARD_ADDR:-:8090}"
export RUN_DIR

"$RUN_DIR/web-dashboard" >>"$RUN_DIR/web-dashboard.log" 2>&1 &
echo $! >"$RUN_DIR/web-dashboard.pid"
echo "started web-dashboard $!"
sleep 2
printf ':8090 '
curl -s -m 2 "http://127.0.0.1:8090/health" || printf 'no response'
echo
echo "open: http://127.0.0.1:8090/ (SSH tunnel from your machine for remote hosts)"
