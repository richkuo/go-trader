#!/usr/bin/env bash
set -euo pipefail
out="${1:-run.sh}"
repo_root=$(git rev-parse --show-toplevel 2>/dev/null || pwd)
cd "$repo_root"
cat >"$out" <<'EOS'
#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
PIDFILE="${GO_TRADER_PIDFILE:-./go-trader.pid}"
if [[ -f .env ]]; then
  set -a
  source .env
  set +a
fi
./go-trader --config scheduler/config.json "$@" &
echo $! >"$PIDFILE"
wait
EOS
chmod +x "$out"
if [[ "$out" == /* ]]; then
  echo "Wrote $out (trader PID -> \$GO_TRADER_PIDFILE or ./go-trader.pid)"
else
  echo "Wrote $(pwd)/$out (trader PID -> \$GO_TRADER_PIDFILE or ./go-trader.pid)"
fi
