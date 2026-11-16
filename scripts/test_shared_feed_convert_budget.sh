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

TREE="$WORK/consumer-tree"
mkdir -p "$TREE"
cat >"$TREE/go-trader" <<SH
#!/bin/sh
echo "\$*" >>"$WORK/direct-probe.log"
exit 0
SH
chmod +x "$TREE/go-trader"
(
    update_tree_foreign_owner() { printf '4242:4243'; }
    update_probe_as_owner() { printf '%s\n' "$*" >>"$WORK/owner-probe.log"; }
    probe_as_tree_owner "$TREE" "$CONFIG"
) >/dev/null 2>&1 || fail "the owner probe path failed"
[[ "$(cat "$WORK/owner-probe.log")" == "$TREE 4242:4243 $TREE/go-trader stdin:$CONFIG" ]] \
    || fail "a tree owned by another user was not probed as that owner with the config on stdin: $(cat "$WORK/owner-probe.log" 2>/dev/null)"
[[ ! -e "$WORK/direct-probe.log" ]] || fail "the tree's binary ran directly although another user owns the tree"
ok "a consumer tree owned by another user is probed as that owner, and its binary never runs as the caller"
(
    update_tree_foreign_owner() { :; }
    probe_as_tree_owner "$TREE" "$CONFIG"
) >/dev/null 2>&1 || fail "the direct probe path failed"
[[ "$(cat "$WORK/direct-probe.log")" == "probe --config $CONFIG" ]] || fail "a tree the caller owns was not probed directly: $(cat "$WORK/direct-probe.log" 2>/dev/null)"
ok "a tree the caller owns is probed directly, as before"
[[ "$(grep -c 'go-trader probe --config' "$SCRIPT_DIR/shared-feed-convert.sh")" == "2" ]] \
    || fail "shared-feed-convert.sh runs a tree binary outside probe_as_tree_owner and run_probe_live"

HEALTH_MODE="$WORK/health.mode"
HEALTH_PORT_FILE="$WORK/health.port"
python3 - "$CONFIG" "$HEALTH_MODE" "$HEALTH_PORT_FILE" <<'PY' &
import http.server, json, sys
config, mode_path, port_path = sys.argv[1:4]
class H(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass
    def do_GET(self):
        mode = open(mode_path).read().strip()
        b = json.load(open(config))["feed"]["request_budget"]
        charged = {"hostile": "PWD[$(touch " + mode_path + ".pwned)]", "low": 4}.get(mode, 23)
        body = {"request_budget": {"per_minute": b["per_minute"], "window_used": 4,
                "totals": {"refused": 0, "by_reason": {"bootstrap": 3}},
                "weight": {"participation": "enabled", "ceiling": 1200, "weight_per_minute": b["weight_per_minute"],
                           "charged": charged, "pending": 0, "in_flight": 0,
                           "totals": {"refused": 0, "reserved_by_reason": {"bootstrap": 30}}}}}
        data = json.dumps(body).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)
srv = http.server.HTTPServer(("127.0.0.1", 0), H)
open(port_path, "w").write(str(srv.server_address[1]))
srv.serve_forever()
PY
HEALTH_PID=$!
trap 'kill "$HEALTH_PID" 2>/dev/null || true; rm -rf "$WORK"' EXIT
for _ in $(seq 1 50); do [[ -s "$HEALTH_PORT_FILE" ]] && break; sleep 0.1; done
[[ -s "$HEALTH_PORT_FILE" ]] || fail "the fixture health server did not start"
need_root() { :; }
printf 'feeds ready at=1\nport backup %s\n' "$(cat "$HEALTH_PORT_FILE")" >"$JOURNAL"
largest=$(calibrate_largest_read) || fail "the real probe printed no largest_read"
[[ "$largest" =~ ^[1-9][0-9]*$ ]] || fail "largest_read is $largest"

echo normal >"$HEALTH_MODE"
py set-budget "$CONFIG" '{"per_minute":120,"startup":60,"weight_per_minute":600,"weight_startup":400,"weight_reserve":200}'
rc=0
(OPT_WINDOW=2 cmd_calibrate) >"$WORK/cal-floor.out" 2>&1 || rc=$?
[[ $rc -eq 0 ]] || fail "calibrate with a measured weight peak of 23 exited $rc: $(cat "$WORK/cal-floor.out")"
wpm=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["feed"]["request_budget"]["weight_per_minute"])' "$CONFIG")
[[ $wpm -ge $((largest + 2)) && $wpm -ge 35 ]] || fail "calibrate wrote weight_per_minute $wpm, below the largest read $largest plus the mids keep"
ok "a measured weight peak of 23 calibrates weight_per_minute to $wpm, at least the largest single read $largest plus the mids keep (2)"

echo low >"$HEALTH_MODE"
py set-budget "$CONFIG" '{"per_minute":120,"startup":60,"weight_per_minute":600,"weight_startup":400,"weight_reserve":200}'
rc=0
(OPT_WINDOW=2 cmd_calibrate) >"$WORK/cal-low.out" 2>&1 || rc=$?
[[ $rc -eq 0 ]] || fail "calibrate with a measured weight peak of 4 exited $rc: $(cat "$WORK/cal-low.out")"
wpm=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["feed"]["request_budget"]["weight_per_minute"])' "$CONFIG")
[[ $wpm -eq $((largest + 2)) ]] || fail "calibrate with a weight peak of 4 wrote weight_per_minute $wpm, want the floor $((largest + 2))"
ok "a measured weight peak of 4 (1.5x = 6) is raised to the floor $wpm, the largest single read $largest plus the mids keep (2)"

echo normal >"$HEALTH_MODE"
py set-budget "$CONFIG" '{"per_minute":120,"startup":60,"weight_per_minute":5,"weight_startup":5,"weight_reserve":1190}'
before=$(sha "$CONFIG")
rc=0
(OPT_WINDOW=2 cmd_calibrate) >"$WORK/cal-nofit.out" 2>&1 || rc=$?
[[ $rc -eq 24 ]] || fail "calibrate with no room for the largest read exited $rc, want 24: $(cat "$WORK/cal-nofit.out")"
grep -q "does not fit the 1200 ceiling less weight_reserve 1190" "$WORK/cal-nofit.out" || fail "calibrate did not name the floor that does not fit: $(cat "$WORK/cal-nofit.out")"
[[ "$(sha "$CONFIG")" == "$before" ]] || fail "calibrate wrote a smaller policy when the floor did not fit"
ok "calibrate fails with a clear message and keeps the config when the largest read does not fit the ceiling less the reserve"

echo hostile >"$HEALTH_MODE"
py set-budget "$CONFIG" '{"per_minute":120,"startup":60,"weight_per_minute":600,"weight_startup":400,"weight_reserve":200}'
before=$(sha "$CONFIG")
rc=0
(OPT_WINDOW=2 cmd_calibrate) >"$WORK/cal-hostile.out" 2>&1 || rc=$?
[[ $rc -eq 24 ]] || fail "calibrate on a non-numeric /health value exited $rc, want 24: $(cat "$WORK/cal-hostile.out")"
[[ ! -e "$HEALTH_MODE.pwned" ]] || fail "a /health value ran a command in calibrate arithmetic"
grep -q "not a whole number" "$WORK/cal-hostile.out" || fail "calibrate did not name the non-numeric value: $(cat "$WORK/cal-hostile.out")"
[[ "$(sha "$CONFIG")" == "$before" ]] || fail "calibrate changed the config after a non-numeric /health value"
ok "a non-numeric /health value stops calibrate, runs no command and keeps the config"

echo "PASS: shared-feed-convert budget (a new backup needs the full weight policy; an unsafe candidate fails the real probe with the config bytes unchanged and no SIGHUP; a calibration failure restores a byte-identical config; calibrate keeps room for the largest single read and refuses non-numeric /health values; a tree owned by another user is probed only as its owner)"
