#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
source "${SCRIPT_DIR}/update_helpers.sh"

: "${GO_TRADER_BIN:?set GO_TRADER_BIN to a built go-trader binary}"
[[ "$(uname -s)" == "Linux" ]] || { echo "SKIP: Linux with systemd required"; exit 0; }
[[ "$(id -u)" == "0" ]] || { echo "SKIP: root required"; exit 0; }
command -v systemd-run >/dev/null 2>&1 || { echo "SKIP: systemd-run required"; exit 0; }
PY3=$(command -v python3) || { echo "SKIP: python3 required"; exit 0; }

fail() {
    echo "FAIL: $*" >&2
    exit 1
}

ID="fixture-$$-$RANDOM"
USER_NAME="gt${ID//[^a-z0-9]/}"
USER_NAME="${USER_NAME:0:31}"
DEPLOY="/opt/go-trader-${ID}"
STATE_BASE="/var/lib/go-trader-${ID}"
UNIT="go-trader-${ID}.service"

cleanup() {
    update_stop_state_lock_holder
    systemctl stop "$UNIT" 2>/dev/null || true
    systemctl reset-failed "$UNIT" 2>/dev/null || true
    rm -rf "$DEPLOY" "$STATE_BASE"
    userdel "$USER_NAME" 2>/dev/null || true
}
trap cleanup EXIT

useradd --system --no-create-home --shell /usr/sbin/nologin "$USER_NAME"
mkdir -p "$DEPLOY/scheduler" "$DEPLOY/logs" "$DEPLOY/shared_scripts" "$DEPLOY/.venv/bin" "$STATE_BASE/live" "$STATE_BASE/paper"
cp "$GO_TRADER_BIN" "$DEPLOY/go-trader"
ln -s "$PY3" "$DEPLOY/.venv/bin/python3"
for stub in check_hyperliquid.py fetch_candles.py strategy_tuner_schema.py check_regime.py simulate_strategy.py; do
    cat > "$DEPLOY/shared_scripts/$stub" <<'PYSTUB'
import json
import sys

argv = sys.argv[1:]
if "--probe-only" in argv:
    print(json.dumps({"ok": True}))
    sys.exit(0)
if "--market-stdin" in argv:
    sys.stdin.read()
print(json.dumps({
    "strategy": "vwap", "symbol": "ETH", "timeframe": "1h", "signal": 0, "price": 100.0,
    "indicators": {}, "mode": "paper", "platform": "hyperliquid", "timestamp": "2026-01-01T00:00:00Z",
    "regime": "ranging", "score": 0.0, "classifier": "adx", "metrics": {"adx": 10.0},
    "windows": {"default": {"regime": "ranging", "score": 0.0, "classifier": "adx", "metrics": {"adx": 10.0}}},
    "atr": 1.0, "candles": [],
}))
PYSTUB
done
cat > "$STATE_BASE/live/config.json" <<JSON
{
  "config_version": 19,
  "interval_seconds": 300,
  "db_file": "$STATE_BASE/live/state.db",
  "paper_db_file": "$STATE_BASE/paper/state.db",
  "log_dir": "logs",
  "discord": {"enabled": false, "token": "", "channels": {}},
  "strategies": [
    {"id": "hl-fixture", "type": "perps", "platform": "hyperliquid",
     "script": "shared_scripts/check_hyperliquid.py",
     "args": ["vwap", "ETH", "1h", "--mode=paper"],
     "capital": 100, "leverage": 2, "margin_per_trade_usd": 10}
  ]
}
JSON
chown -R "$USER_NAME:$USER_NAME" "$DEPLOY" "$STATE_BASE"
chmod 755 "$DEPLOY"

override_directive=$(update_paper_override_directive "$STATE_BASE/paper")
[[ "$override_directive" == "StateDirectory=go-trader-${ID}/paper" ]] || fail "override directive for the paper state dir: $override_directive"

run_unit() {
    systemd-run --wait --pipe --collect --quiet --unit "$UNIT" \
        --uid="$USER_NAME" --gid="$USER_NAME" \
        -p WorkingDirectory="$DEPLOY" \
        -p "StateDirectory=go-trader-${ID}/live" \
        -p "$override_directive" \
        -p ProtectSystem=strict \
        -p PrivateTmp=true \
        -p NoNewPrivileges=true \
        -p "ReadWritePaths=$DEPLOY/scheduler $DEPLOY/logs" \
        -- "$DEPLOY/go-trader" --config "$STATE_BASE/live/config.json" --once
}

echo "== first start under the sandbox (--once)"
first_log=$(run_unit 2>&1) && first_rc=0 || first_rc=$?
echo "$first_log" | tail -n 40
[[ "$first_rc" == "0" ]] || fail "first --once exited $first_rc"
[[ -f "$STATE_BASE/live/state.db" ]] || fail "live state file not written under the sandbox"
[[ -f "$STATE_BASE/paper/state.db" ]] || fail "paper state file not written under the sandbox"
[[ "$first_log" == *"[storage] layout: split"* ]] || fail "startup did not report the split layout"
[[ "$first_log" == *"[config] portfolio scopes:"* ]] || fail "startup did not report the scope counts"
ls -la "$STATE_BASE/live" "$STATE_BASE/paper"

echo "== second start while the handoff holds the paper lock"
touch "$STATE_BASE/paper/state.db.lock" "$STATE_BASE/paper/state.db.manual-action.lock"
chown "$USER_NAME:$USER_NAME" "$STATE_BASE/paper/state.db.lock" "$STATE_BASE/paper/state.db.manual-action.lock"
update_start_state_lock_holder "$STATE_BASE/paper/state.db" || fail "lock holder did not start"
second_log=$(run_unit 2>&1) && second_rc=0 || second_rc=$?
echo "$second_log" | tail -n 20
[[ "$second_rc" == "79" ]] || fail "second start exited $second_rc, want 79 (exit 79 = another owner holds a state file)"
[[ "$second_log" == *"pid ${UPDATE_LOCK_HOLDER_PID}"* ]] || fail "exit 79 did not name the holder pid ${UPDATE_LOCK_HOLDER_PID}"
update_stop_state_lock_holder

echo "OK: merge-paper service fixture passed (exit 79 for the second start)"
