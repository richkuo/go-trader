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

TARGET="$WORK/root-only-target"
printf 'ROOT-ONLY\n' >"$TARGET"
chmod 0600 "$TARGET"
target_sum=$(sha "$TARGET")
target_mode() { python3 -c 'import os,sys; print(format(os.stat(sys.argv[1]).st_mode & 0o7777, "o"))' "$1"; }
ln -sfn "$TARGET" "$CONFIG.candidate-shared-feed"
ln -sfn "$TARGET" "$CONFIG.restore-shared-feed"
: >"$SYSTEMCTL_LOG"
apply_backup_budget '{"per_minute":160,"startup":60,"weight_per_minute":650,"weight_startup":300,"weight_reserve":200}' >/dev/null 2>&1 \
    || fail "a safe budget change failed with symlinks planted in the config directory"
[[ "$(py budget-of "$CONFIG")" == '{"per_minute":160,"startup":60,"weight_per_minute":650,"weight_startup":300,"weight_reserve":200}' ]] \
    || fail "the budget change did not land on the real config: $(py budget-of "$CONFIG")"
[[ "$(sha "$TARGET")" == "$target_sum" && "$(target_mode "$TARGET")" == "600" ]] || fail "a budget change wrote through a planted symlink"
[[ -L "$CONFIG.candidate-shared-feed" && ! -L "$CONFIG" ]] || fail "a budget change replaced the planted symlink or left the config as a symlink"
ok "a budget change with symlinks planted in the config directory completes on the real config and leaves the symlink target unchanged"

kept_sum=$(sha "$CONFIG")
CAL_KEEP="$WORK/calibrate-keep-2.json"
cp -p "$CONFIG" "$CAL_KEEP"
py set-budget "$CONFIG" '{"per_minute":9,"startup":9,"weight_per_minute":9,"weight_startup":9,"weight_reserve":9}'
rc=0
(calibrate_fail "fixture failure with a planted restore symlink") >"$WORK/calibrate-2.out" 2>&1 || rc=$?
[[ $rc -eq 24 ]] || fail "calibrate_fail exited $rc, want 24"
[[ "$(sha "$CONFIG")" == "$kept_sum" ]] || fail "calibrate_fail did not restore the kept bytes past a planted restore symlink"
[[ "$(sha "$TARGET")" == "$target_sum" && "$(target_mode "$TARGET")" == "600" ]] || fail "calibrate_fail wrote through a planted symlink"
ok "a calibration failure restores the kept bytes and never writes through a planted restore symlink"

mv "$CONFIG" "$CONFIG.real"
ln -s "$TARGET" "$CONFIG"
rc=0
apply_backup_budget '{"per_minute":170,"startup":60,"weight_per_minute":650,"weight_startup":300,"weight_reserve":200}' >/dev/null 2>&1 || rc=$?
[[ $rc -ne 0 ]] || fail "a budget change read a config that was swapped to a symlink"
[[ "$(sha "$TARGET")" == "$target_sum" && -L "$CONFIG" ]] || fail "a budget change touched the symlink target or replaced the swapped symlink"
rm "$CONFIG"
mv "$CONFIG.real" "$CONFIG"
rm -f "$CONFIG.candidate-shared-feed" "$CONFIG.restore-shared-feed"
ok "a config swapped to a symlink is refused before root reads or writes through it"

PLAN_TMP=$(mktemp -d "$WORK/plan-probe.XXXXXX")
printf '{}\n' >"$PLAN_TMP/feed-backup.json"
printf '{}\n' >"$PLAN_TMP/go-trader@a.json"
chmod 0600 "$PLAN_TMP/feed-backup.json" "$PLAN_TMP/go-trader@a.json"
(
    chown() { printf '%s\n' "$*" >>"$WORK/plan-chown.log"; }
    grant_plan_probe_read "$PLAN_TMP" 4242:4243
)
[[ "$(cat "$WORK/plan-chown.log")" == "0:0 $PLAN_TMP"$'\n'"4242:4243 $PLAN_TMP/go-trader@a.json" ]] \
    || fail "the plan probe grant changed owners beyond root on the directory and the probe user on the shadow files: $(cat "$WORK/plan-chown.log")"
[[ "$(target_mode "$PLAN_TMP")" == "711" && "$(target_mode "$PLAN_TMP/go-trader@a.json")" == "600" && "$(target_mode "$PLAN_TMP/feed-backup.json")" == "600" ]] \
    || fail "plan probe modes: dir $(target_mode "$PLAN_TMP"), shadow $(target_mode "$PLAN_TMP/go-trader@a.json"), feed $(target_mode "$PLAN_TMP/feed-backup.json")"
ok "the plan probe directory stays root-owned and unwritable by the owner; only the probe user can read each shadow config, and no group can"

mkdir -p "$WORK/real-dir"
ln -s "$WORK/real-dir" "$WORK/link-dir"
printf '{"a":1}\n' >"$WORK/src.json"
rc=0
py copy "$WORK/src.json" "$WORK/link-dir/out.json" 2>"$WORK/walk.err" || rc=$?
[[ $rc -ne 0 && ! -e "$WORK/real-dir/out.json" ]] || fail "a write went through a directory symlink placed in a directory a normal account owns"
grep -q "in a directory another account can change" "$WORK/walk.err" || fail "no parent symlink refusal: $(cat "$WORK/walk.err")"
ok "a symlinked parent directory that a normal account controls is refused, and nothing is written through it"

ucp_check() {
    local unit="$1" target="$2" link="$WORK/ucp/$3"
    mkdir -p "$(dirname "$link")"
    ln -sfn "$target" "$link"
    (
        unit_prop() { printf '/opt/whatever'; }
        update_execstart_config_path() { printf '%s' "$link"; }
        unit_config_path "$unit"
    ) 2>"$WORK/ucp.err"
}
mkdir -p "$ROOT/var/lib/go-trader/inst1"
printf '{}\n' >"$ROOT/var/lib/go-trader/inst1/config.json"
got=$(ucp_check go-trader@inst1.service "$ROOT/var/lib/go-trader/inst1/config.json" a/config.json) \
    || fail "a symlink to the unit's own config was refused: $(cat "$WORK/ucp.err")"
[[ "$got" == "$(cd "$ROOT/var/lib/go-trader/inst1" && pwd -P)/config.json" ]] || fail "unit_config_path returned $got"
for target in "$CONFIG" "$ROOT/var/lib/go-trader/inst2/config.json" "$STATE_DIR/convert.journal"; do
    if ucp_check go-trader@inst1.service "$target" b/config.json >/dev/null; then
        fail "a consumer config symlink to $target was accepted"
    fi
done
ok "a consumer config symlink resolves only to the unit's own config, never into a feed, another unit or the shared-feed state"

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

CANON=$(cd "$WORK" && pwd -P)
LEGACY="$CANON/legacy"
VICTIM="$CANON/victim"
OTHER="$ROOT/var/lib/go-trader/inst2/config.json"
mkdir -p "$LEGACY/scheduler" "$VICTIM" "$(dirname "$OTHER")"
cat >"$LEGACY/scheduler/config.json" <<'JSON'
{"strategies": [{"id": "hl-sma-btc", "type": "perps", "platform": "hyperliquid", "script": "shared_scripts/check_hyperliquid.py", "args": ["sma_crossover", "BTC", "1h", "--mode=paper"]}]}
JSON
cp "$LEGACY/scheduler/config.json" "$VICTIM/config.json"
cp "$LEGACY/scheduler/config.json" "$OTHER"
chmod 0640 "$VICTIM/config.json" "$OTHER"
victim_sum=$(sha "$VICTIM/config.json")
other_sum=$(sha "$OTHER")
LUNIT=go-trader-legacy.service
switch_journal() {
    printf '%s\n' "verify pass at=1" \
        "budget calibrated per_minute=120 startup=60 weight_per_minute=600 weight_startup=400 weight_reserve=200 peak=10 weight_peak=300 bootstrap=5 bootstrap_weight=100 baseline_peak=none fixed=no at=2" \
        "consumer $LUNIT config=$LEGACY/scheduler/config.json shadow=$SHADOW_DIR/go-trader-legacy.json" \
        "consumers-set units=$LUNIT at=3" >"$JOURNAL"
}
legacy_units() {
    unit_prop() {
        case "$2" in
            WorkingDirectory) [[ "$1" == "$LUNIT" ]] && printf '%s' "$LEGACY" || printf '%s' "$CANON/elsewhere" ;;
            *) printf '' ;;
        esac
    }
    update_execstart_config_path() { :; }
}
switch_journal
got=$(legacy_units; consumer_config_now "$LUNIT" "$LEGACY/scheduler/config.json" && echo same) || fail "an unchanged legacy config path was refused"
[[ "$got" == "same" ]] || fail "consumer_config_now printed $got"
ok "an unchanged regular consumer config path passes the switch and rollback recheck"

mv "$LEGACY/scheduler" "$LEGACY/scheduler.real"
ln -s "$VICTIM" "$LEGACY/scheduler"
rc=0
(legacy_units; SELECTED=("$LUNIT"); cmd_switch) >"$WORK/sw1.out" 2>&1 || rc=$?
[[ $rc -eq 20 ]] || fail "switch through a swapped consumer directory exited $rc, want 20: $(cat "$WORK/sw1.out")"
grep -q "no longer resolves to $LEGACY/scheduler/config.json" "$WORK/sw1.out" || fail "switch did not name the changed path: $(cat "$WORK/sw1.out")"
[[ "$(sha "$VICTIM/config.json")" == "$victim_sum" && "$(target_mode "$VICTIM/config.json")" == "640" ]] || fail "switch changed the config behind a swapped directory"
compgen -G "$VICTIM/config.json.pre-shared-feed.*" >/dev/null && fail "switch wrote a pre-switch copy into the folder behind a swapped directory"
ok "switch refuses a consumer whose config directory became a symlink after 'consumers'; the target config keeps its bytes and mode and gets no pre-switch copy"

printf 'BACKUP\n' >"$WORK/legacy-backup.json"
printf '%s\n' "switch $LUNIT config=$LEGACY/scheduler/config.json backup=$WORK/legacy-backup.json pre=$(sha "$WORK/legacy-backup.json") post=$victim_sum pid=1 written" >>"$JOURNAL"
rc=0
(legacy_units; SELECTED=("$LUNIT"); OPT_ALL=0; cmd_rollback) >"$WORK/rb1.out" 2>&1 || rc=$?
[[ $rc -eq 30 ]] || fail "rollback through a swapped consumer directory exited $rc, want 30: $(cat "$WORK/rb1.out")"
grep -q "nothing is restored through a changed path" "$WORK/rb1.out" || fail "rollback did not refuse at the path recheck: $(cat "$WORK/rb1.out")"
[[ "$(sha "$VICTIM/config.json")" == "$victim_sum" ]] || fail "rollback wrote the config behind a swapped directory"
compgen -G "$VICTIM/config.json.pre-rollback.*" >/dev/null && fail "rollback wrote a copy into the folder behind a swapped directory"
ok "rollback refuses a consumer whose config directory became a symlink after the switch, and writes nothing behind it"

rm "$LEGACY/scheduler"
mv "$LEGACY/scheduler.real" "$LEGACY/scheduler"
mv "$LEGACY/scheduler/config.json" "$LEGACY/scheduler/config.json.real"
ln -s "$OTHER" "$LEGACY/scheduler/config.json"
switch_journal
rc=0
(legacy_units; SELECTED=("$LUNIT"); cmd_switch) >"$WORK/sw2.out" 2>&1 || rc=$?
[[ $rc -eq 20 ]] || fail "switch through a config symlink into another unit exited $rc, want 20: $(cat "$WORK/sw2.out")"
[[ "$(sha "$OTHER")" == "$other_sum" ]] || fail "switch changed another unit's config"
compgen -G "$OTHER.pre-shared-feed.*" >/dev/null && fail "switch wrote a pre-switch copy beside another unit's config"
rc=0
(legacy_units; scheduler_units() { printf '%s\n' "$LUNIT"; }; check_selection "$LUNIT") >"$WORK/sel2.out" 2>&1 || rc=$?
[[ $rc -eq 2 ]] || fail "plan and feeds selection of a config symlink into another unit exited $rc, want 2: $(cat "$WORK/sel2.out")"
grep -q "ERROR $LUNIT: its config path" "$WORK/sel2.out" || fail "the selection refusal did not name $LUNIT: $(cat "$WORK/sel2.out")"
ok "a consumer config symlink into another unit's state folder is refused by the plan and feeds selection and by switch; that config does not change"

INST1_DIR="$CANON/inst1-tree"
mkdir -p "$INST1_DIR/scheduler"
ln -sfn "$ROOT/var/lib/go-trader/inst1/config.json" "$INST1_DIR/scheduler/config.json"
own_now=$(
    unit_prop() { [[ "$2" == WorkingDirectory ]] && printf '%s' "$INST1_DIR" || printf ''; }
    update_execstart_config_path() { :; }
    unit_config_path go-trader@inst1.service
) || fail "the migrated-layout symlink to the unit's own config was refused"
(
    unit_prop() { [[ "$2" == WorkingDirectory ]] && printf '%s' "$INST1_DIR" || printf ''; }
    update_execstart_config_path() { :; }
    consumer_config_now go-trader@inst1.service "$own_now"
) 2>"$WORK/own.err" || fail "the recheck refused the migrated-layout path it recorded: $(cat "$WORK/own.err")"
ok "a migrated-layout config symlink to the unit's own state folder passes the switch and rollback recheck"

rc=0
(
    scheduler_units() { printf '%s\n' "$LUNIT" go-trader-plain.service; }
    legacy_units
    print_inventory
) >"$WORK/inv.out" 2>&1 || rc=$?
[[ $rc -eq 0 ]] || fail "the inventory stopped on a refused unit that is not selected (exit $rc): $(cat "$WORK/inv.out")"
grep -q "$LUNIT: refused as a consumer: $LUNIT: its config path" "$WORK/inv.out" || fail "the inventory did not report the refused unit: $(cat "$WORK/inv.out")"
grep -q "go-trader-plain.service user=" "$WORK/inv.out" || fail "the inventory stopped before the next unit: $(cat "$WORK/inv.out")"
ok "the inventory reports a refused unit that is not selected and continues to the next unit"

MAIN_TREE="$CANON/main-tree"
MAIN_CFG="$ROOT/var/lib/go-trader/config.json"
mkdir -p "$MAIN_TREE/scheduler"
cp "$LEGACY/scheduler/config.json.real" "$MAIN_CFG"
ln -sfn "$MAIN_CFG" "$MAIN_TREE/scheduler/config.json"
main_sum=$(sha "$MAIN_CFG")
rm "$LEGACY/scheduler/config.json"
mv "$LEGACY/scheduler/config.json.real" "$LEGACY/scheduler/config.json"
host_units() {
    scheduler_units() { printf '%s\n' go-trader.service "$LUNIT" go-trader@inst1.service ${EXTRA_UNIT:-}; }
    unit_prop() {
        case "$2" in
            WorkingDirectory)
                case "$1" in
                    go-trader.service) printf '%s' "$MAIN_TREE" ;;
                    "$LUNIT") printf '%s' "${LEGACY_WD:-$LEGACY}" ;;
                    go-trader@inst1.service) printf '%s' "$INST1_DIR" ;;
                    *) printf '%s' "$CANON/b-tree" ;;
                esac
                ;;
            ExecStart) [[ "$1" == go-trader-b.service ]] && printf '%s' "$LEGACY/scheduler/config.json" || printf '' ;;
            *) printf '' ;;
        esac
    }
    update_execstart_config_path() { printf '%s' "$1"; }
}
main_refusals() {
    local label="$1" rc=0
    switch_journal
    (host_units; SELECTED=("$LUNIT"); cmd_switch) >"$WORK/main-sw.out" 2>&1 || rc=$?
    [[ $rc -eq 20 ]] || fail "$label: switch exited $rc, want 20: $(cat "$WORK/main-sw.out")"
    rc=0
    (host_units; check_selection "$LUNIT") >"$WORK/main-sel.out" 2>&1 || rc=$?
    [[ $rc -eq 2 ]] || fail "$label: check_selection exited $rc, want 2: $(cat "$WORK/main-sel.out")"
    grep -q "ERROR $LUNIT: its config path" "$WORK/main-sel.out" || fail "$label: the selection refusal did not name $LUNIT: $(cat "$WORK/main-sel.out")"
    [[ "$(sha "$MAIN_CFG")" == "$main_sum" ]] || fail "$label: the go-trader.service config changed"
    compgen -G "$MAIN_CFG.pre-shared-feed.*" >/dev/null && fail "$label: a pre-switch copy appeared beside the go-trader.service config"
    return 0
}
mv "$LEGACY/scheduler/config.json" "$LEGACY/scheduler/config.json.real"
ln -s "$MAIN_CFG" "$LEGACY/scheduler/config.json"
main_refusals "a non-template unit config symlink to /var/lib/go-trader/config.json"
ok "a non-template unit whose config is a symlink to the go-trader.service config is refused by selection (exit 2, by name) and by switch (exit 20); that config keeps its bytes and gets no pre-switch copy"
rm "$LEGACY/scheduler/config.json"
mv "$LEGACY/scheduler/config.json.real" "$LEGACY/scheduler/config.json"
mv "$LEGACY/scheduler" "$LEGACY/scheduler.real"
ln -s "$ROOT/var/lib/go-trader" "$LEGACY/scheduler"
main_refusals "a non-template unit scheduler directory symlink to /var/lib/go-trader"
ok "a non-template unit whose scheduler directory is a symlink to /var/lib/go-trader is refused the same way"
rm "$LEGACY/scheduler"
mv "$LEGACY/scheduler.real" "$LEGACY/scheduler"

own_main=$(host_units; unit_config_path go-trader.service) || fail "go-trader.service with the migrated symlink to its own config was refused"
[[ "$own_main" == "$(cd "$(dirname "$MAIN_CFG")" && pwd -P)/config.json" ]] || fail "go-trader.service resolved to $own_main"
own_inst1=$(host_units; unit_config_path go-trader@inst1.service) || fail "go-trader@inst1.service with its own state config was refused"
[[ "$own_inst1" == "$(cd "$ROOT/var/lib/go-trader/inst1" && pwd -P)/config.json" ]] || fail "go-trader@inst1.service resolved to $own_inst1"
own_legacy=$(host_units; unit_config_path "$LUNIT") || fail "a regular legacy config owned by one unit was refused"
[[ "$own_legacy" == "$LEGACY/scheduler/config.json" ]] || fail "the legacy unit resolved to $own_legacy"
ok "go-trader.service with the migrated symlink to its own config, go-trader@inst1.service with its own state config and a regular legacy config all pass"

rc=0
(EXTRA_UNIT=go-trader-b.service; host_units; unit_config_path "$LUNIT") >"$WORK/shared-cfg.out" 2>&1 || rc=$?
[[ $rc -eq 2 ]] || fail "a config that another unit also uses exited $rc, want 2: $(cat "$WORK/shared-cfg.out")"
grep -q "which is also the config of go-trader-b.service" "$WORK/shared-cfg.out" || fail "the shared-config refusal did not name the other unit: $(cat "$WORK/shared-cfg.out")"
ok "a regular config path that another discovered unit also uses is refused and names that unit"

LEG_STATE="$ROOT/var/lib/go-trader/legacy/config.json"
mkdir -p "$(dirname "$LEG_STATE")"
cp "$LEGACY/scheduler/config.json" "$LEG_STATE"
state_dir_units() {
    host_units
    unit_prop() {
        case "$2" in
            WorkingDirectory) [[ "$1" == "$LUNIT" ]] && printf '%s' "$LEGACY" || printf '%s' "$MAIN_TREE" ;;
            StateDirectory) [[ "$1" == "$LUNIT" ]] && printf '%s' "${LEG_STATE_DIR:-}" || printf '' ;;
            *) printf '' ;;
        esac
    }
}
mv "$LEGACY/scheduler/config.json" "$LEGACY/scheduler/config.json.real"
ln -s "$LEG_STATE" "$LEGACY/scheduler/config.json"
got=$(LEG_STATE_DIR="go-trader/legacy"; state_dir_units; unit_config_path "$LUNIT") \
    || fail "a non-template unit with StateDirectory=go-trader/legacy was refused its own state config"
[[ "$got" == "$(cd "$(dirname "$LEG_STATE")" && pwd -P)/config.json" ]] || fail "the StateDirectory unit resolved to $got"
for sd in "" "go-trader" "go-trader/feed-backup" "go-trader/other" "go-trader/legacy/.." "elsewhere/legacy"; do
    if (LEG_STATE_DIR="$sd"; state_dir_units; unit_config_path "$LUNIT") >/dev/null 2>&1; then
        fail "a non-template unit with StateDirectory=$sd was allowed to follow a symlink to $LEG_STATE"
    fi
done
rm "$LEGACY/scheduler/config.json"
ln -s "$MAIN_CFG" "$LEGACY/scheduler/config.json"
if (LEG_STATE_DIR="go-trader/legacy"; state_dir_units; unit_config_path "$LUNIT") >/dev/null 2>&1; then
    fail "a StateDirectory unit was allowed to follow a symlink to the go-trader.service config"
fi
rm "$LEGACY/scheduler/config.json"
mv "$LEGACY/scheduler/config.json.real" "$LEGACY/scheduler/config.json"
ok "a non-template unit follows a config symlink only into its own go-trader StateDirectory; a missing, shared, feed, other or escaping StateDirectory and the go-trader.service config are refused"

rc=0
(
    host_units
    unit_prop() { [[ "$2" == ExecStart ]] && printf '%s' "$ROOT/var/lib/go-trader/inst1/config.json"$'\n'"$CANON/evil/config.json" || printf ''; }
    unit_config_path go-trader@inst1.service
) >"$WORK/cntrl.out" 2>&1 || rc=$?
[[ $rc -eq 2 ]] || fail "a config path with a newline exited $rc, want 2: $(cat "$WORK/cntrl.out")"
grep -q "holds a control character" "$WORK/cntrl.out" || fail "the newline path was not refused by name: $(cat "$WORK/cntrl.out")"
ok "a config path that holds a newline is refused before any ownership comparison"

success_layout() {
    local unit="$1" cfg="$2" label="$3" shadow pre mode uid rc=0 name
    shadow="$SHADOW_DIR/${unit%.service}.json"
    mkdir -p "$SHADOW_DIR"
    cp "$WORK/consumer-template.json" "$cfg"
    chmod 0640 "$cfg"
    pre=$(sha "$cfg")
    mode=$(target_mode "$cfg")
    uid=$(python3 -c 'import os,sys; print(os.stat(sys.argv[1]).st_uid)' "$cfg")
    py write-shared "$cfg" "$shadow" "$(feed_socket primary)" "$(feed_socket backup)" 0600
    for name in primary backup; do
        mkdir -p "$(dirname "$(feed_config "$name")")"
        python3 - "$(feed_config "$name")" "$shadow" <<'PY'
import json, sys
json.dump({"role": "feed", "feed": {"source": "rest", "consumer_configs": [sys.argv[2]]}, "strategies": []}, open(sys.argv[1], "w"), indent=2)
PY
        chmod 0600 "$(feed_config "$name")"
    done
    printf '%s\n' "verify pass at=1" \
        "budget calibrated per_minute=120 startup=60 weight_per_minute=600 weight_startup=400 weight_reserve=200 peak=10 weight_peak=300 bootstrap=5 bootstrap_weight=100 baseline_peak=none fixed=no at=2" \
        "consumer $unit config=$cfg shadow=$shadow" "consumers-set units=$unit at=3" >"$JOURNAL"
    rm -f "$WORK/at-reload.json"
    (
        host_units
        reload_feed() { [[ -e "$WORK/at-reload.json" ]] || cp "$cfg" "$WORK/at-reload.json"; }
        http_to_file() { printf '{"consumers":[{"path":"%s","loaded":true}]}' "$cfg" >"$2"; }
        wait_active_fresh() { printf '4242'; }
        FAIL_AFTER=feeds-reload
        SELECTED=("$unit")
        cmd_switch
    ) >"$WORK/succ-sw.out" 2>&1 || rc=$?
    [[ $rc -eq 26 ]] || fail "$label: switch with a forced failure after the feed reload exited $rc, want 26: $(cat "$WORK/succ-sw.out")"
    [[ "$(py is-shared "$WORK/at-reload.json" "$(feed_socket primary)" "$(feed_socket backup)")" == "yes" ]] || fail "$label: switch did not write shared mode before the feed reload"
    [[ "$(sha "$cfg")" == "$pre" && "$(target_mode "$cfg")" == "$mode" ]] || fail "$label: the switch failure restore left different bytes or mode"
    [[ "$(python3 -c 'import os,sys; print(os.stat(sys.argv[1]).st_uid)' "$cfg")" == "$uid" ]] || fail "$label: the restore changed the config owner"
    local copy
    copy=$(compgen -G "$cfg.pre-shared-feed.*" | head -n 1)
    [[ -n "$copy" && "$(sha "$copy")" == "$pre" ]] || fail "$label: no byte-identical pre-switch copy beside the config"
    grep -q "^switch $unit .* rolled_back mode=byte restarted=yes" "$JOURNAL" || fail "$label: the journal has no byte restore record: $(cat "$JOURNAL")"
    for name in primary backup; do
        [[ "$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["feed"]["consumer_configs"])' "$(feed_config "$name")")" == "['$shadow']" ]] \
            || fail "$label: $(feed_unit "$name") was not repointed back to the shadow config"
    done

    local backup="$cfg.pre-shared-feed.manual" post
    py copy "$cfg" "$backup"
    py write-shared "$cfg" "$cfg" "$(feed_socket primary)" "$(feed_socket backup)" "$(py mode-of "$backup")"
    py chown-ref "$backup" "$cfg"
    post=$(sha "$cfg")
    for name in primary backup; do py replace-consumer "$(feed_config "$name")" "$shadow" "$cfg"; done
    printf '%s\n' "switch $unit config=$cfg backup=$backup pre=$pre post=$post pid=1 written" >>"$JOURNAL"
    rc=0
    (
        host_units
        wait_active_fresh() { printf '4242'; }
        SELECTED=("$unit")
        OPT_ALL=0
        cmd_rollback
    ) >"$WORK/succ-rb.out" 2>&1 || rc=$?
    [[ $rc -eq 0 ]] || fail "$label: rollback exited $rc: $(cat "$WORK/succ-rb.out")"
    [[ "$(sha "$cfg")" == "$pre" && "$(target_mode "$cfg")" == "$mode" ]] || fail "$label: rollback left different bytes or mode"
    [[ "$(python3 -c 'import os,sys; print(os.stat(sys.argv[1]).st_uid)' "$cfg")" == "$uid" ]] || fail "$label: rollback changed the config owner"
    grep -q "restored (byte) and healthy" "$WORK/succ-rb.out" || fail "$label: rollback did not report the byte restore: $(cat "$WORK/succ-rb.out")"
}
cp "$LEGACY/scheduler/config.json" "$WORK/consumer-template.json"
success_layout go-trader@inst1.service "$(cd "$ROOT/var/lib/go-trader/inst1" && pwd -P)/config.json" "migrated layout"
ok "migrated layout: switch writes shared mode, a forced failure after the feed reload restores the byte-identical config, owner and mode, and rollback of a written switch restores it too"
success_layout "$LUNIT" "$LEGACY/scheduler/config.json" "legacy tree"
ok "legacy tree: the same switch, failure restore and rollback succeed with byte-identical bytes, owner and mode"

SHARED_STATE="$ROOT/var/lib/go-trader/shared/config.json"
mkdir -p "$(dirname "$SHARED_STATE")"
cp "$WORK/consumer-template.json" "$SHARED_STATE"
template_units() {
    scheduler_units() { printf '%s\n' go-trader@inst1.service go-trader@inst3.service ${EXTRA_UNIT:-}; }
    unit_prop() {
        case "$2" in
            WorkingDirectory)
                case "$1" in
                    go-trader@inst1.service) printf '%s' "$INST1_DIR" ;;
                    "$LUNIT") printf '%s' "$LEGACY" ;;
                    *) printf '%s' "$CANON/other-$1" ;;
                esac
                ;;
            StateDirectory)
                case "$1" in
                    go-trader@inst1.service) printf 'go-trader/inst1 go-trader/shared' ;;
                    go-trader@inst3.service) printf 'go-trader/inst3 go-trader/shared' ;;
                    go-trader-twin.service) printf 'go-trader/legacy' ;;
                    "$LUNIT") printf 'go-trader/legacy' ;;
                esac
                ;;
            *) printf '' ;;
        esac
    }
    update_execstart_config_path() { :; }
}
ln -sfn "$SHARED_STATE" "$INST1_DIR/scheduler/config.json"
rc=0
(template_units; unit_config_path go-trader@inst1.service) >"$WORK/shared-dir.out" 2>&1 || rc=$?
[[ $rc -eq 2 ]] || fail "a template instance config symlink into go-trader/shared exited $rc, want 2: $(cat "$WORK/shared-dir.out")"
ln -sfn "$ROOT/var/lib/go-trader/inst1/config.json" "$INST1_DIR/scheduler/config.json"
got=$(template_units; unit_config_path go-trader@inst1.service) || fail "go-trader@inst1.service with its own state config was refused next to go-trader/shared"
[[ "$got" == "$(cd "$ROOT/var/lib/go-trader/inst1" && pwd -P)/config.json" ]] || fail "go-trader@inst1.service resolved to $got"
mv "$LEGACY/scheduler/config.json" "$LEGACY/scheduler/config.json.real"
ln -s "$LEG_STATE" "$LEGACY/scheduler/config.json"
got=$(template_units; scheduler_units() { printf '%s\n' "$LUNIT"; }; unit_config_path "$LUNIT") \
    || fail "a unit that alone lists StateDirectory=go-trader/legacy was refused its own state config"
[[ "$got" == "$(cd "$(dirname "$LEG_STATE")" && pwd -P)/config.json" ]] || fail "the StateDirectory unit resolved to $got"
rc=0
(template_units; scheduler_units() { printf '%s\n' "$LUNIT" go-trader-twin.service; }; unit_config_path "$LUNIT") >"$WORK/twin.out" 2>&1 || rc=$?
[[ $rc -eq 2 ]] || fail "a StateDirectory that another active unit also lists was accepted as own (exit $rc): $(cat "$WORK/twin.out")"
rm "$LEGACY/scheduler/config.json"
mv "$LEGACY/scheduler/config.json.real" "$LEGACY/scheduler/config.json"
ok "the template's shared folder and a StateDirectory that another active unit also lists are never an own config home; an instance's own state config still passes"

INST1_STATE="$ROOT/var/lib/go-trader/inst1"
INST1_SAVED="$ROOT/var/lib/go-trader/inst1.saved"
REDIRECT="$CANON/redirect-target"
mkdir -p "$REDIRECT"
cp "$WORK/consumer-template.json" "$REDIRECT/config.json"
chmod 0640 "$REDIRECT/config.json"
redirect_sum=$(sha "$REDIRECT/config.json")
inst1_recorded="$(cd "$INST1_STATE" && pwd -P)/config.json"
mv "$INST1_STATE" "$INST1_SAVED"
ln -s "$REDIRECT" "$INST1_STATE"
direct_units() {
    template_units
    unit_prop() {
        case "$2" in
            ExecStart) printf '%s' "$INST1_STATE/config.json" ;;
            StateDirectory) printf 'go-trader/inst1 go-trader/shared' ;;
            *) printf '' ;;
        esac
    }
    update_execstart_config_path() { printf '%s' "$1"; }
}
rc=0
(direct_units; scheduler_units() { printf '%s\n' go-trader@inst1.service; }; check_selection go-trader@inst1.service) >"$WORK/redir-sel.out" 2>&1 || rc=$?
[[ $rc -eq 2 ]] || fail "selection through a symlinked instance state folder exited $rc, want 2: $(cat "$WORK/redir-sel.out")"
printf '%s\n' "verify pass at=1" \
    "budget calibrated per_minute=120 startup=60 weight_per_minute=600 weight_startup=400 weight_reserve=200 peak=10 weight_peak=300 bootstrap=5 bootstrap_weight=100 baseline_peak=none fixed=no at=2" \
    "consumer go-trader@inst1.service config=$inst1_recorded shadow=$SHADOW_DIR/go-trader@inst1.json" "consumers-set units=go-trader@inst1.service at=3" >"$JOURNAL"
rc=0
(direct_units; SELECTED=(go-trader@inst1.service); cmd_switch) >"$WORK/redir-sw.out" 2>&1 || rc=$?
[[ $rc -eq 20 ]] || fail "switch through a symlinked instance state folder exited $rc, want 20: $(cat "$WORK/redir-sw.out")"
[[ "$(sha "$REDIRECT/config.json")" == "$redirect_sum" && "$(target_mode "$REDIRECT/config.json")" == "640" ]] || fail "switch changed the config behind a symlinked state folder"
compgen -G "$REDIRECT/config.json.pre-shared-feed.*" >/dev/null && fail "switch wrote a pre-switch copy behind a symlinked state folder"
rm "$INST1_STATE"
mv "$INST1_SAVED" "$INST1_STATE"
ok "a template instance whose state folder was replaced by a symlink is refused by selection (exit 2) and by switch (exit 20); the target keeps its bytes and mode and gets no pre-switch copy"

LEGACY_WD="$WORK/legacy"
got=$(host_units; unit_config_path "$LUNIT") || fail "a legacy unit whose working directory passes through a root-owned folder symlink was refused"
[[ "$got" == "$LEGACY/scheduler/config.json" ]] || fail "the legacy unit through a root-owned folder symlink resolved to $got"
success_layout "$LUNIT" "$LEGACY/scheduler/config.json" "legacy tree through a root-owned folder symlink"
unset LEGACY_WD
if [[ "$WORK" != "$CANON" ]]; then
    ok "a legacy unit whose working directory passes through a root-owned folder symlink ($WORK resolves to $CANON) is accepted, and switch, its failure restore and rollback succeed"
else
    ok "a legacy unit whose working directory is canonical is accepted, and switch, its failure restore and rollback succeed (no root-owned folder symlink on this host's temp path)"
fi

echo "PASS: shared-feed-convert budget (a new backup needs the full weight policy; an unsafe candidate fails the real probe with the config bytes unchanged and no SIGHUP; a calibration failure restores a byte-identical config; calibrate keeps room for the largest single read and refuses non-numeric /health values; a tree owned by another user is probed only as its owner; no root write follows a planted symlink or a symlinked parent; a consumer config symlink resolves only to the own config of that unit, or into its own go-trader StateDirectory, and a config another unit also uses is refused; switch and rollback refuse a config path that changed after it was recorded; the inventory reports a refused unit that is not selected and continues; a config path with a control character, a symlinked state folder or a shared state folder is refused; switch, its failure restore and rollback succeed on the migrated layout and a legacy tree, also through a root-owned folder symlink)"
