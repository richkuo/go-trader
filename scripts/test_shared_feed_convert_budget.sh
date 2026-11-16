#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_DIR=$(cd "$SCRIPT_DIR/.." && pwd)
WORK=$(mktemp -d "${TMPDIR:-/tmp}/shared-feed-budget-test.XXXXXX")
trap 'rm -rf "$WORK"' EXIT

fail() {
    echo "FAIL: $*" >&2
    exit 1
}

ok() {
    echo "ok: $*"
}

sha() {
    python3 -c 'import hashlib, sys; print(hashlib.sha256(open(sys.argv[1], "rb").read()).hexdigest())' "$1"
}

BIN="${GO_TRADER_BIN:-}"
if [[ -z "$BIN" ]]; then
    BIN="$WORK/go-trader"
    go -C "$REPO_DIR/scheduler" build -o "$BIN" . || fail "go build of the feed binary failed"
fi

ROOT="$WORK/root"
FEED_DIR="$ROOT/opt/go-trader-feed-backup"
CONFIG="$ROOT/var/lib/go-trader/feed-backup/config.json"
mkdir -p "$FEED_DIR" "$(dirname "$CONFIG")" "$WORK/bin"
cp "$BIN" "$FEED_DIR/go-trader"

cat >"$WORK/consumer.json" <<'JSON'
{
  "market_feed": "shared",
  "shared_market_feed": {"primary_socket": "/run/go-trader-feed-primary/feed.sock", "backup_socket": "/run/go-trader-feed-backup/feed.sock"},
  "strategies": [
    {"id": "hl-sma-btc", "type": "perps", "platform": "hyperliquid", "script": "shared_scripts/check_hyperliquid.py",
     "args": ["sma_crossover", "BTC", "1h", "--mode=paper"], "capital": 1000, "max_drawdown_pct": 50, "interval_seconds": 3600}
  ]
}
JSON

python3 - "$CONFIG" "$WORK/consumer.json" <<'PY'
import json, sys
json.dump({"role": "feed", "status_port": 8191,
           "feed": {"source": "rest", "socket_path": "/run/go-trader-feed-backup/feed.sock", "consumer_configs": [sys.argv[2]],
                    "request_budget": {"per_minute": 120, "startup": 60, "weight_per_minute": 600, "weight_startup": 400, "weight_reserve": 200}},
           "strategies": []}, open(sys.argv[1], "w"), indent=2)
PY
chmod 0600 "$CONFIG"

SYSTEMCTL_LOG="$WORK/systemctl.log"
cat >"$WORK/bin/systemctl" <<SH
#!/usr/bin/env bash
printf '%s\n' "\$*" >>"$SYSTEMCTL_LOG"
exit 0
SH
chmod +x "$WORK/bin/systemctl"
export PATH="$WORK/bin:$PATH"
export SHARED_FEED_CONVERT_ROOT="$ROOT"

source "$SCRIPT_DIR/shared-feed-convert.sh"

[[ "$(feed_config backup)" == "$CONFIG" ]] || fail "SHARED_FEED_CONVERT_ROOT did not move the backup config path ($(feed_config backup))"
ok "the path-root override moves the feed paths under $ROOT"

if (OPT_PER_MINUTE=120 OPT_STARTUP=60; new_backup_budget >/dev/null); then
    fail "a new backup with request limits and no weight policy was accepted"
fi
ok "a new backup without a weight policy has no budget"

rc=0
(OPT_WEIGHT_PER_MINUTE=600; validate_budget_opts) >/dev/null 2>&1 || rc=$?
[[ $rc -eq 2 ]] || fail "a partial weight flag set exited $rc, want 2"
ok "a partial weight flag set is a usage error"

budget=$(OPT_PER_MINUTE=120 OPT_STARTUP=60 OPT_WEIGHT_PER_MINUTE=600 OPT_WEIGHT_STARTUP=400 OPT_WEIGHT_RESERVE=200 new_backup_budget) \
    || fail "a full flag set produced no budget"
[[ "$budget" == '{"per_minute":120,"startup":60,"weight_per_minute":600,"weight_startup":400,"weight_reserve":200}' ]] \
    || fail "the flag budget is $budget"
ok "a full flag set produces the complete five-field budget"

mkdir -p "$STATE_DIR"
printf 'budget calibrated per_minute=120 startup=60 peak=10 bootstrap=5 baseline_peak=none fixed=no at=1\n' >"$JOURNAL"
if budget_calibrated; then
    fail "a calibrated journal record without weight fields counts as calibrated"
fi
printf 'budget calibrated per_minute=120 startup=60 weight_per_minute=600 weight_startup=400 weight_reserve=200 peak=10 weight_peak=300 bootstrap=5 bootstrap_weight=100 baseline_peak=none fixed=no at=2\n' >>"$JOURNAL"
budget_calibrated || fail "a calibrated journal record with all five fields does not count as calibrated"
[[ "$(new_backup_budget)" == "$budget" ]] || fail "the journal budget differs from the flag budget"
ok "only a journal record with the full weight policy counts as calibrated"

before=$(sha "$CONFIG")
: >"$SYSTEMCTL_LOG"
rc=0
apply_backup_budget '{"per_minute":120,"startup":60,"weight_per_minute":900,"weight_startup":400,"weight_reserve":200}' 2>"$WORK/unsafe.err" || rc=$?
[[ $rc -ne 0 ]] || fail "an unsafe candidate budget was applied"
grep -q 'exceeds the 1200 venue weight ceiling' "$WORK/unsafe.err" || fail "the real probe did not name the ceiling refusal: $(cat "$WORK/unsafe.err")"
[[ "$(sha "$CONFIG")" == "$before" ]] || fail "the live config changed after a refused candidate"
[[ ! -s "$SYSTEMCTL_LOG" ]] || fail "a refused candidate sent: $(cat "$SYSTEMCTL_LOG")"
[[ ! -e "$CONFIG.candidate-shared-feed" ]] || fail "the refused candidate file was left behind"
ok "an unsafe candidate fails the real probe; the live config sha256 is unchanged and no SIGHUP was sent"

apply_backup_budget '{"per_minute":150,"startup":60,"weight_per_minute":700,"weight_startup":300,"weight_reserve":200}' >/dev/null 2>&1 \
    || fail "a safe candidate budget was refused"
[[ "$(py budget-of "$CONFIG")" == '{"per_minute":150,"startup":60,"weight_per_minute":700,"weight_startup":300,"weight_reserve":200}' ]] \
    || fail "the safe candidate was not written: $(py budget-of "$CONFIG")"
grep -qx 'kill -s HUP go-trader@feed-backup.service' "$SYSTEMCTL_LOG" || fail "a safe candidate sent no SIGHUP: $(cat "$SYSTEMCTL_LOG")"
ok "a safe candidate passes the probe, replaces the config and sends SIGHUP"

original=$(sha "$CONFIG")
CAL_KEEP="$WORK/calibrate-keep.json"
cp -p "$CONFIG" "$CAL_KEEP"
py set-budget "$CONFIG" '{"per_minute":9,"startup":9,"weight_per_minute":9,"weight_startup":9,"weight_reserve":9}'
[[ "$(sha "$CONFIG")" != "$original" ]] || fail "the fixture did not change the config"
: >"$SYSTEMCTL_LOG"
rc=0
(calibrate_fail "fixture failure") >"$WORK/calibrate.out" 2>&1 || rc=$?
[[ $rc -eq 24 ]] || fail "calibrate_fail exited $rc, want 24"
[[ "$(sha "$CONFIG")" == "$original" ]] || fail "calibrate_fail did not restore the byte copy"
grep -q "sha256 $original" "$WORK/calibrate.out" || fail "calibrate_fail did not report the restored sha256: $(cat "$WORK/calibrate.out")"
grep -qx 'kill -s HUP go-trader@feed-backup.service' "$SYSTEMCTL_LOG" || fail "calibrate_fail sent no SIGHUP after the restore"
ok "a calibration failure restores a byte-identical config and reloads the backup"

echo "PASS: shared-feed-convert budget (a new backup needs the full weight policy; an unsafe candidate fails the real probe with the config bytes unchanged and no SIGHUP; a calibration failure restores a byte-identical config)"
