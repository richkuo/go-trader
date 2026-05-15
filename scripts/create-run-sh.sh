#!/usr/bin/env bash
# Emit a minimal run.sh for RESTART_MODE=signal (see #766 / scripts/update.sh).
# Usage: bash scripts/create-run-sh.sh [path-to-run.sh]
set -euo pipefail
out="${1:-run.sh}"
repo_root=$(git rev-parse --show-toplevel 2>/dev/null || pwd)
cd "$repo_root"
cat >"$out" <<'EOS'
#!/usr/bin/env bash
# Managed by scripts/create-run-sh.sh — edit argv after ./go-trader as needed.
set -euo pipefail
cd "$(dirname "$0")"
PIDFILE="${GO_TRADER_PIDFILE:-./go-trader.pid}"
if [[ -f .env ]]; then
  set -a
  # shellcheck disable=SC1091
  source .env
  set +a
fi
./go-trader --config scheduler/config.json "$@" &
# update.sh verify/rollback poll the pidfile until HEALTH_TIMEOUT — no need to sync before echo.
echo $! >"$PIDFILE"
wait
EOS
chmod +x "$out"
if [[ "$out" == /* ]]; then
  echo "Wrote $out (trader PID -> \$GO_TRADER_PIDFILE or ./go-trader.pid)"
else
  echo "Wrote $(pwd)/$out (trader PID -> \$GO_TRADER_PIDFILE or ./go-trader.pid)"
fi
