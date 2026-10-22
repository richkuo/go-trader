#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "${SCRIPT_DIR}/.." && pwd)

: "${GO_TRADER_BIN:?set GO_TRADER_BIN to a built go-trader binary}"
skip() {
    if [[ "${ROBINHOOD_SESSION_SERVICE_FIXTURE_REQUIRE_RUN:-0}" == "1" ]]; then
        echo "FAIL: ROBINHOOD_SESSION_SERVICE_FIXTURE_REQUIRE_RUN=1 but this host cannot run the fixture: $*" >&2
        exit 1
    fi
    echo "SKIP: $*"
    exit 0
}
[[ "$(uname -s)" == "Linux" ]] || skip "Linux with systemd required"
[[ "$(id -u)" == "0" ]] || skip "root required"
[[ "$(ps -o comm= -p 1 2>/dev/null)" == "systemd" ]] || skip "systemd as PID 1 required"
command -v systemd-run >/dev/null 2>&1 || skip "systemd-run required"
command -v runuser >/dev/null 2>&1 || skip "runuser required"
PY3=$(command -v python3) || skip "python3 required"
"$PY3" -c 'import sys; sys.exit(0 if sys.version_info[:2] == (3, 12) else 1)' || skip "python3 3.12 required"
[[ -n "${FIXTURE_UV:-}" && -x "${FIXTURE_UV}" ]] || skip "FIXTURE_UV must name a uv binary"
[[ ! -e /nonexistent ]] || skip "/nonexistent exists on this host, so the home-folder check would prove nothing"

fail() {
    echo "FAIL: $*" >&2
    exit 1
}

ID="rhfixture-$$-$RANDOM"
USER_NAME="gt${ID//[^a-z0-9]/}"
USER_NAME="${USER_NAME:0:31}"
DEPLOY="/opt/go-trader-${ID}"
STATE="/var/lib/go-trader-${ID}"
UNIT="go-trader-${ID}.service"
SESSION="${STATE}/robinhood-session"
REQUESTS="${DEPLOY}/logs/requests.jsonl"

cleanup() {
    systemctl stop "$UNIT" 2>/dev/null || true
    systemctl reset-failed "$UNIT" 2>/dev/null || true
    rm -rf "$DEPLOY" "$STATE"
    userdel "$USER_NAME" 2>/dev/null || true
}
trap cleanup EXIT

useradd --system --home-dir /nonexistent --no-create-home --shell /usr/sbin/nologin "$USER_NAME"
mkdir -p "$DEPLOY/scheduler" "$DEPLOY/logs" "$STATE"
for item in platforms shared_scripts shared_tools shared_strategies backtest pyproject.toml uv.lock .python-version; do
    cp -R "$REPO_ROOT/$item" "$DEPLOY/"
done
cp "$GO_TRADER_BIN" "$DEPLOY/go-trader"
cat > "$DEPLOY/shared_scripts/check_price.py" <<'PYSTUB'
import json
import sys

print(json.dumps({symbol: 100.0 for symbol in sys.argv[1:]}))
PYSTUB

echo "== build the service virtualenv"
(cd "$DEPLOY" && UV_PROJECT_ENVIRONMENT="$DEPLOY/.venv" UV_LINK_MODE=copy UV_PYTHON_DOWNLOADS=never \
    "$FIXTURE_UV" sync --frozen --no-dev --no-install-project --python "$PY3") || fail "uv sync for the service virtualenv failed"

cat > "$DEPLOY/routes.json" <<'JSON'
{
  "GET https://api.robinhood.com/positions/?nonzero=true": [{"status": 200, "json": {"next": null, "results": []}}],
  "GET https://nummus.robinhood.com/holdings/": [{"status": 200, "json": {"next": null, "results": []}}]
}
JSON
cat > "$STATE/config.json" <<JSON
{
  "config_version": 20,
  "interval_seconds": 300,
  "db_file": "$STATE/state.db",
  "log_dir": "logs",
  "discord": {"enabled": false, "token": "", "channels": {}},
  "strategies": [
    {"id": "rh-session-fixture", "type": "spot", "platform": "robinhood",
     "script": "shared_scripts/check_robinhood.py",
     "args": ["momentum_pro", "BTC", "1h", "--mode=live"],
     "capital": 100, "paused": true}
  ]
}
JSON
chown -R "$USER_NAME:$USER_NAME" "$DEPLOY" "$STATE"
chmod 755 "$DEPLOY"
runuser -u "$USER_NAME" -- "$DEPLOY/.venv/bin/python3" -c 'import robin_stocks, pyotp, pandas, yfinance' || fail "the service user cannot import the Robinhood dependencies"

run_unit() {
    systemd-run --wait --pipe --collect --quiet --unit "$UNIT" \
        --uid="$USER_NAME" --gid="$USER_NAME" \
        -p WorkingDirectory="$DEPLOY" \
        -p "StateDirectory=go-trader-${ID}" \
        -p ProtectSystem=strict \
        -p PrivateTmp=true \
        -p NoNewPrivileges=true \
        -p "ReadWritePaths=$DEPLOY/scheduler $DEPLOY/logs" \
        -E PYTHONDONTWRITEBYTECODE=1 \
        -E "PYTHONPATH=$DEPLOY/shared_scripts/testdata/robinhood_offline" \
        -E RH_OFFLINE_STUB_MODE=routes \
        -E "RH_OFFLINE_STUB_ROUTES=$DEPLOY/routes.json" \
        -E "RH_OFFLINE_STUB_LOG=$REQUESTS" \
        -E ROBINHOOD_USERNAME=fixture-user \
        -E ROBINHOOD_PASSWORD=fixture-password \
        -E ROBINHOOD_TOTP_SECRET=JBSWY3DPEHPK3PXP \
        -E GO_TRADER_ALLOW_MISSING_STATE=1 \
        -- "$DEPLOY/go-trader" --config "$STATE/config.json" --once
}

token_posts() {
    if [[ ! -f "$REQUESTS" ]]; then
        echo 0
        return
    fi
    grep -c '"url": "https://api.robinhood.com/oauth2/token/"' "$REQUESTS" || true
}

echo "== first start under the sandbox (--once)"
first_log=$(run_unit 2>&1) && first_rc=0 || first_rc=$?
echo "$first_log" | tail -n 40
[[ "$first_rc" == "0" ]] || fail "first --once exited $first_rc"
[[ "$first_log" == *"[robinhood] session folder: $SESSION"* ]] || fail "startup did not report the session folder $SESSION"
[[ "$first_log" != *"Robinhood login failed"* ]] || fail "the live Robinhood login failed under the sandbox"
[[ "$first_log" == *"robin_stocks: Starting login process..."* ]] || fail "the live Robinhood check did not log in"
[[ "$(stat -c '%a %U' "$SESSION")" == "700 $USER_NAME" ]] || fail "session folder is $(stat -c '%a %U' "$SESSION"), want 700 $USER_NAME"
shopt -s nullglob
pickles=("$SESSION"/robinhood-*.pickle)
shopt -u nullglob
[[ "${#pickles[@]}" == "1" ]] || fail "want one stored session in $SESSION, found ${#pickles[@]}"
[[ "$(stat -c '%a %U' "${pickles[0]}")" == "600 $USER_NAME" ]] || fail "stored session is $(stat -c '%a %U' "${pickles[0]}"), want 600 $USER_NAME"
[[ ! -e /nonexistent ]] || fail "the service created /nonexistent"
first_posts=$(token_posts)
[[ "$first_posts" -ge 1 ]] || fail "the first start sent no password login"
ls -la "$SESSION"

echo "== second start reuses the stored session"
mv "$REQUESTS" "$REQUESTS.first"
second_log=$(run_unit 2>&1) && second_rc=0 || second_rc=$?
echo "$second_log" | tail -n 20
[[ "$second_rc" == "0" ]] || fail "second --once exited $second_rc"
[[ "$second_log" != *"Robinhood login failed"* ]] || fail "the second live Robinhood login failed"
second_posts=$(token_posts)
[[ "$second_posts" == "0" ]] || fail "the second start sent $second_posts password logins, want 0"
grep -q '"url": "https://api.robinhood.com/positions/?nonzero=true"' "$REQUESTS" || fail "the second start did not check the stored session"
[[ ! -e /nonexistent ]] || fail "the service created /nonexistent"

attempts=$("$PY3" -c 'import sqlite3, sys; print(sqlite3.connect(sys.argv[1]).execute("SELECT COUNT(*) FROM robinhood_order_attempts").fetchone()[0])' "$STATE/state.db")
[[ "$attempts" == "0" ]] || fail "robinhood_order_attempts has $attempts rows, want 0 for a paused strategy"

echo "OK: robinhood session service fixture passed (state-directory session, owner-only modes, no home writes, reuse on second start)"
