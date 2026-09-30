#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "$SCRIPT_DIR/.." && pwd)
TOOL="$SCRIPT_DIR/migrate-service-layout.py"

: "${GO_TRADER_BIN:?set GO_TRADER_BIN to a built go-trader binary}"
[[ "$(uname -s)" == "Linux" ]] || { echo "SKIP: Linux with systemd required"; exit 0; }
[[ "$(id -u)" == "0" ]] || { echo "SKIP: root required"; exit 0; }
[[ -d /run/systemd/system ]] || { echo "SKIP: systemd must be PID 1"; exit 0; }
for t in systemd-run python3 rsync git runuser flock; do
    command -v "$t" >/dev/null 2>&1 || { echo "SKIP: $t required"; exit 0; }
done
PY3=$(command -v python3)
SCENARIOS="${FIXTURE_SCENARIOS:-refuse plan confirm apply latch conflict resume signal stages kill fold}"

ID="fx$(( RANDOM % 9000 + 1000 ))"
BASE_PORT=$(( 18000 + RANDOM % 800 ))
WORK=$(mktemp -d)
declare -a CREATED_UNITS=() CREATED_DIRS=() INSTANCES=()
STATE_ROOT=/var/lib/go-trader/service-layout
TEMPLATE_PRESENT=0
[[ -e /etc/systemd/system/go-trader@.service ]] && TEMPLATE_PRESENT=1
JOURNALD_PRESENT=0
[[ -e /etc/systemd/journald@go-trader.conf ]] && JOURNALD_PRESENT=1

fail() {
    if [[ -s "$WORK/last.out" ]]; then
        echo "--- last command output:" >&2
        tail -n 80 "$WORK/last.out" >&2
    fi
    echo "FAIL: $*" >&2
    exit 1
}

note() {
    echo "== $*"
}

cleanup() {
    local u i
    for u in "${CREATED_UNITS[@]}"; do
        systemctl stop "$u" >/dev/null 2>&1 || true
    done
    for i in "${INSTANCES[@]}"; do
        systemctl stop "go-trader@$i.service" >/dev/null 2>&1 || true
        systemctl disable "go-trader@$i.service" >/dev/null 2>&1 || true
        systemctl unmask "go-trader@$i.service" >/dev/null 2>&1 || true
        systemctl unmask --runtime "go-trader@$i.service" >/dev/null 2>&1 || true
        rm -rf "/opt/go-trader-$i" "/opt/go-trader-$i".rolled-back-* "/var/lib/go-trader/$i" "/var/lib/go-trader/$i".rolled-back-* "${STATE_ROOT:?}/$i"
        rm -rf "/etc/systemd/system/go-trader@$i.service.d"
    done
    for u in "${CREATED_UNITS[@]}"; do
        rm -f "/etc/systemd/system/$u"
        rm -rf "/etc/systemd/system/$u.d"
        find /etc/systemd/system -lname "*$u" -delete 2>/dev/null || true
    done
    [[ "$TEMPLATE_PRESENT" == "1" ]] || rm -f /etc/systemd/system/go-trader@.service
    [[ "$JOURNALD_PRESENT" == "1" ]] || rm -f /etc/systemd/journald@go-trader.conf
    systemctl daemon-reload >/dev/null 2>&1 || true
    for d in "${CREATED_DIRS[@]}"; do
        rm -rf "$d"
    done
    rm -rf "$WORK"
}
trap cleanup EXIT

tool() {
    "$PY3" "$TOOL" "$@"
}

expect_exit() {
    local want="$1"; shift
    local rc=0
    "$@" >"$WORK/last.out" 2>&1 || rc=$?
    if [[ "$rc" != "$want" ]]; then
        tail -n 60 "$WORK/last.out" >&2
        fail "'$*' exited $rc, want $want"
    fi
}

unit_prop() {
    systemctl show "$1" -p "$2" --value 2>/dev/null || true
}

health_field() {
    local port="$1" field="$2"
    curl -s -m 5 "http://127.0.0.1:$port/health" 2>/dev/null | "$PY3" -c 'import json,sys
try:
    v = json.load(sys.stdin)
except Exception:
    sys.exit(0)
for p in sys.argv[1].split("."):
    v = v.get(p) if isinstance(v, dict) else None
print("" if v is None else v)' "$field" || true
}

wait_health() {
    local unit="$1" port="$2" i pid hpid
    for i in $(seq 1 90); do
        pid=$(unit_prop "$unit" MainPID)
        if [[ -n "$pid" && "$pid" != "0" ]]; then
            hpid=$(health_field "$port" pid)
            [[ "$hpid" == "$pid" ]] && return 0
        fi
        sleep 1
    done
    journalctl -u "$unit" -n 60 --no-pager >&2 || true
    fail "$unit did not answer /health on port $port with its pid"
}

sql() {
    "$PY3" - "$1" "$2" <<'PY'
import sqlite3, sys
con = sqlite3.connect(sys.argv[1], timeout=10)
try:
    cur = con.execute(sys.argv[2])
    rows = cur.fetchall()
    con.commit()
finally:
    con.close()
for r in rows:
    print("|".join("" if v is None else str(v) for v in r))
PY
}

tree_fp() {
    local d="$1"
    (cd "$d" && find . -path ./logs -prune -o -path ./.git -prune -o -type f ! -name '*.db' ! -name '*.db-wal' ! -name '*.db-shm' ! -name '*.lock' -print0 | sort -z | xargs -0 sha256sum | sha256sum | awk '{print $1}')
}

file_fp() {
    if [[ -e "$1" ]]; then sha256sum "$1" | awk '{print $1}'; else echo absent; fi
}

write_stubs() {
    local dir="$1" stub
    for stub in check_hyperliquid.py fetch_candles.py strategy_tuner_schema.py check_regime.py simulate_strategy.py; do
        cat > "$dir/shared_scripts/$stub" <<'PYSTUB'
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
}

make_deployment() {
    local name="$1" port="$2" layout="${3:-plain}"
    local src="/root/gt-$ID-$name" unit="go-trader-$ID-$name.service" outside="/srv/gt-$ID-$name"
    CREATED_DIRS+=("$src" "$outside")
    CREATED_UNITS+=("$unit")
    mkdir -p "$src" "$outside"
    chmod 700 "$src"
    (cd "$REPO_ROOT" && git ls-files -z | tar --null -T - -cf -) | tar -xf - -C "$src"
    cp "$GO_TRADER_BIN" "$src/go-trader"
    mkdir -p "$src/.venv/bin" "$src/logs"
    ln -s "$PY3" "$src/.venv/bin/python3"
    write_stubs "$src"
    if [[ "$layout" != "nogit" ]]; then
        git -C "$src" init -q
        git -C "$src" -c user.email=fixture@example.invalid -c user.name=fixture add -A
        git -C "$src" -c user.email=fixture@example.invalid -c user.name=fixture commit -qm fixture
    fi
    "$PY3" - "$src/scheduler/config.json" "$port" "$layout" "$outside" "$name" <<'PY'
import json, sys
path, port, layout, outside, name = sys.argv[1], int(sys.argv[2]), sys.argv[3], sys.argv[4], sys.argv[5]
def strat(sid, extra=None):
    s = {"id": sid, "type": "perps", "platform": "hyperliquid", "script": "shared_scripts/check_hyperliquid.py",
         "args": ["vwap", "ETH", "1h", "--mode=paper"], "capital": 100, "leverage": 2, "margin_per_trade_usd": 10}
    s.update(extra or {})
    return s
cfg = {"config_version": 19, "interval_seconds": 3600, "log_dir": "logs", "status_port": port,
       "discord": {"enabled": False, "token": "", "channels": {}},
       "strategies": [strat("hl-%s-a" % name), strat("hl-%s-b" % name)]}
if layout in ("split", "paperfile"):
    cfg["paper_db_file"] = outside + "/paper.db"
if layout == "split":
    cfg["paper_sources"] = [{"id": "btc", "db_file": "scheduler/source-btc.db"}]
    cfg["strategies"].append(strat("hl-%s-src" % name, {"paper_source": "btc"}))
if layout == "live":
    cfg["strategies"].append({"id": "hl-%s-live" % name, "type": "perps", "platform": "hyperliquid",
                              "script": "shared_scripts/check_hyperliquid.py", "args": ["vwap", "ETH", "1h", "--mode=live"],
                              "capital": 100, "leverage": 2, "margin_per_trade_usd": 10})
json.dump(cfg, open(path, "w"), indent=2)
PY
    cat > "$src/.env" <<ENV
FIXTURE_SECRET='a b\$c'
GO_TRADER_SERVICE=$unit
ENV
    if [[ "$layout" == "live" ]]; then
        cat >> "$src/.env" <<ENV
HYPERLIQUID_SECRET_KEY=0x$(printf '1%.0s' $(seq 1 64))
HYPERLIQUID_ACCOUNT_ADDRESS=0x$(printf '2%.0s' $(seq 1 40))
GO_TRADER_ALLOW_MISSING_STATE=1
ENV
    fi
    chmod 600 "$src/.env"
    cat > "/etc/systemd/system/$unit" <<UNIT
[Unit]
Description=fixture $name
After=network.target

[Service]
Type=simple
WorkingDirectory=$src
ExecStart=$src/go-trader --config $src/scheduler/config.json
EnvironmentFile=$src/.env
Environment="FIXTURE_INLINE=x y"
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
UNIT
    systemctl daemon-reload
    systemctl enable --now "$unit" >/dev/null 2>&1
    wait_health "$unit" "$port"
}

latch_kill_switch() {
    "$PY3" - "$1" <<'PY' || fail "could not latch the kill switch in $1"
import sqlite3, sys
con = sqlite3.connect(sys.argv[1], timeout=10)
n = con.execute("UPDATE portfolio_risk SET kill_switch_active = 1, kill_switch_at = '2026-09-01T00:00:00Z'").rowcount
con.commit()
con.close()
assert n > 0, "no portfolio_risk row"
PY
}

wait_journal_stage() {
    local inst="$1" stage="$2" i
    for i in $(seq 1 300); do
        if [[ -f "$STATE_ROOT/$inst/journal.jsonl" ]] && grep -q "\"stage\": \"$stage\"" "$STATE_ROOT/$inst/journal.jsonl"; then
            return 0
        fi
        sleep 0.5
    done
    fail "journal of $inst never reached $stage"
}

locks_all_busy() {
    "$PY3" - "$@" <<'PY'
import errno, fcntl, os, sys
busy = []
free = []
for p in sys.argv[1:]:
    fd = os.open(p, os.O_RDONLY)
    try:
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        free.append(p)
        fcntl.flock(fd, fcntl.LOCK_UN)
    except OSError as e:
        if e.errno not in (errno.EWOULDBLOCK, errno.EAGAIN):
            raise
        busy.append(p)
    finally:
        os.close(fd)
if free:
    print("FREE: " + " ".join(free))
    sys.exit(1)
print("all %d locks busy" % len(busy))
PY
}

simulate_reboot() {
    local u
    for u in "$@"; do
        systemctl stop "$u" >/dev/null 2>&1 || true
    done
    for u in "$@"; do
        if [[ "$(systemctl is-enabled "$u" 2>/dev/null || true)" == "enabled" ]]; then
            systemctl start "$u" >/dev/null 2>&1 || true
        fi
    done
    sleep 3
    local active=0
    for u in "$@"; do
        [[ "$(systemctl is-active "$u" 2>/dev/null || true)" == "active" ]] && active=$((active + 1))
    done
    [[ $active -le 1 ]] || fail "after a simulated reboot $active of ($*) are active"
    echo "reboot simulation: $active of $# units active"
}

assert_source_running() {
    local unit="$1" port="$2"
    [[ "$(systemctl is-active "$unit")" == "active" ]] || fail "$unit is not active"
    [[ "$(unit_prop "$unit" LoadState)" == "loaded" ]] || fail "$unit is not loaded (masked?)"
    [[ "$(systemctl is-enabled "$unit" 2>/dev/null || true)" == "enabled" ]] || fail "$unit lost its enabled state"
    wait_health "$unit" "$port"
}

assert_target_down() {
    local inst="$1"
    local st
    st=$(systemctl is-active "go-trader@$inst.service" 2>/dev/null || true)
    [[ "$st" != "active" && "$st" != "activating" ]] || fail "go-trader@$inst.service is $st"
    [[ "$(systemctl is-enabled "go-trader@$inst.service" 2>/dev/null || true)" != "enabled" ]] || fail "go-trader@$inst.service is still enabled"
}

scenario_refuse() {
    note "refusals before any change"
    local port=$((BASE_PORT + 1)) unit="go-trader-$ID-refuse.service" inst="r-$ID"
    INSTANCES+=("$inst")
    make_deployment refuse "$port"
    if [[ "$TEMPLATE_PRESENT" == "0" ]]; then
        printf '[Service]\nExecStart=/bin/true\n' > /etc/systemd/system/go-trader@.service
        expect_exit 15 tool plan --unit "$unit" --instance "$inst"
        rm -f /etc/systemd/system/go-trader@.service
        systemctl daemon-reload
    fi
    mkdir -p "/etc/systemd/system/$unit.d"
    printf '[Service]\nExecStartPre=/bin/true\n' > "/etc/systemd/system/$unit.d/10-fixture.conf"
    systemctl daemon-reload
    systemctl restart "$unit"
    wait_health "$unit" "$port"
    expect_exit 10 tool plan --unit "$unit" --instance "$inst"
    printf '[Service]\nEnvironment=PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin\n' > "/etc/systemd/system/$unit.d/10-fixture.conf"
    systemctl daemon-reload
    systemctl restart "$unit"
    wait_health "$unit" "$port"
    expect_exit 13 tool plan --unit "$unit" --instance "$inst"
    rm -rf "/etc/systemd/system/$unit.d"
    systemctl daemon-reload
    "$PY3" - "/root/gt-$ID-refuse/scheduler/config.json" <<'PY'
import json, sys
c = json.load(open(sys.argv[1])); c["replay_log_path"] = "scheduler/replay.db"; json.dump(c, open(sys.argv[1], "w"), indent=2)
PY
    systemctl restart "$unit"
    wait_health "$unit" "$port"
    expect_exit 15 tool plan --unit "$unit" --instance "$inst"
    "$PY3" - "/root/gt-$ID-refuse/scheduler/config.json" <<'PY'
import json, sys
c = json.load(open(sys.argv[1])); c.pop("replay_log_path"); json.dump(c, open(sys.argv[1], "w"), indent=2)
PY
    systemctl restart "$unit"
    wait_health "$unit" "$port"
    mkdir -p "/opt/go-trader-$inst"
    expect_exit 14 tool plan --unit "$unit" --instance "$inst"
    rmdir "/opt/go-trader-$inst"
    expect_exit 2 tool apply --unit "$unit" --instance "../x"
    expect_exit 0 tool plan --unit "$unit" --instance "$inst"
    [[ ! -e "$STATE_ROOT/$inst" ]] || fail "plan created $STATE_ROOT/$inst"
    systemctl stop "$unit"
    echo "refusals OK"
}

scenario_plan() {
    note "plan and status are read-only"
    local port=$((BASE_PORT + 2)) unit="go-trader-$ID-quiet.service" inst="q-$ID" src="/root/gt-$ID-quiet"
    INSTANCES+=("$inst")
    make_deployment quiet "$port" split
    sleep 3
    local before after
    before=$(
        file_fp "/etc/systemd/system/$unit"; file_fp "$src/.env"; file_fp "$src/scheduler/config.json"; tree_fp "$src"
        for f in "$src/scheduler/state.db" "/srv/gt-$ID-quiet/paper.db" "$src/scheduler/source-btc.db"; do
            for s in "" -wal -shm .lock .manual-action.lock; do echo "$f$s $(file_fp "$f$s")"; done
        done
        ls -la "$src/scheduler" "/srv/gt-$ID-quiet" | awk '{print $1, $3, $4, $NF}'
        [[ -e "$STATE_ROOT" ]] && echo "root-present" || echo "root-absent"
        [[ -e "$STATE_ROOT/migrate.lock" ]] && file_fp "$STATE_ROOT/migrate.lock" || echo nolock
    )
    expect_exit 0 tool plan --unit "$unit" --instance "$inst"
    grep -q "plan id:" "$WORK/last.out" || fail "plan printed no plan id"
    grep -q "database primary" "$WORK/last.out" || fail "plan printed no database mapping"
    grep -q "database paper:btc" "$WORK/last.out" || fail "plan did not map the paper source"
    if grep -q "a b" "$WORK/last.out"; then fail "plan printed a secret value"; fi
    expect_exit 0 tool status
    expect_exit 0 tool status --instance "$inst"
    after=$(
        file_fp "/etc/systemd/system/$unit"; file_fp "$src/.env"; file_fp "$src/scheduler/config.json"; tree_fp "$src"
        for f in "$src/scheduler/state.db" "/srv/gt-$ID-quiet/paper.db" "$src/scheduler/source-btc.db"; do
            for s in "" -wal -shm .lock .manual-action.lock; do echo "$f$s $(file_fp "$f$s")"; done
        done
        ls -la "$src/scheduler" "/srv/gt-$ID-quiet" | awk '{print $1, $3, $4, $NF}'
        [[ -e "$STATE_ROOT" ]] && echo "root-present" || echo "root-absent"
        [[ -e "$STATE_ROOT/migrate.lock" ]] && file_fp "$STATE_ROOT/migrate.lock" || echo nolock
    )
    if [[ "$before" != "$after" ]]; then
        diff <(echo "$before") <(echo "$after") >&2 || true
        fail "plan or status changed a file"
    fi
    [[ ! -e "$STATE_ROOT/$inst" ]] || fail "plan created a journal directory"
    echo "plan/status read-only OK"
}

scenario_apply() {
    note "apply, repeat, rollback with target changes, re-apply"
    local port=$((BASE_PORT + 3)) unit="go-trader-$ID-live.service" inst="l-$ID" src="/root/gt-$ID-live"
    local peer="go-trader-$ID-peer.service" peer_port=$((BASE_PORT + 4))
    INSTANCES+=("$inst")
    make_deployment live "$port" split
    make_deployment peer "$peer_port"
    local peer_pid src_version
    peer_pid=$(unit_prop "$peer" MainPID)
    src_version=$(health_field "$port" version)
    tool plan --unit "$unit" --instance "$inst" >"$WORK/plan.out" 2>&1 || { cat "$WORK/plan.out"; fail "plan refused"; }
    local plan_id
    plan_id=$(sed -n 's/.*plan id: \([0-9a-f]*\).*/\1/p' "$WORK/plan.out" | tail -n 1)
    [[ -n "$plan_id" ]] || fail "no plan id"
    grep -q "change: access: other accounts cannot reach $src, so /opt/go-trader-$inst is created 0700" "$WORK/plan.out" || { cat "$WORK/plan.out"; fail "plan did not report the private tree mode"; }
    grep -q "change: access: config .* -> /var/lib/go-trader/$inst/config.json (0600 go-trader:go-trader)" "$WORK/plan.out" || { cat "$WORK/plan.out"; fail "plan did not report the config mode change"; }
    expect_exit 19 tool apply --unit "$unit" --instance "$inst" --plan-id 0000000000000000
    local sentinel="$WORK/release-snapshot"
    MIGRATE_SERVICE_LAYOUT_PAUSE_AT="snapshot:$sentinel" "$PY3" "$TOOL" apply --unit "$unit" --instance "$inst" --plan-id "$plan_id" >"$WORK/apply.out" 2>&1 &
    local apid=$!
    wait_journal_stage "$inst" snapshot
    local -a locks=()
    local db
    for db in "$src/scheduler/state.db" "/srv/gt-$ID-live/paper.db" "$src/scheduler/source-btc.db" \
              "/opt/go-trader-$inst/scheduler/state.db" "/var/lib/go-trader/$inst/paper-state.db" "/opt/go-trader-$inst/scheduler/source-btc.db"; do
        locks+=("$db.lock" "$db.manual-action.lock")
    done
    locks_all_busy "${locks[@]}" || fail "a lock was free while the transfer ran"
    local once_rc=0
    (cd "$src" && timeout 30 ./go-trader --once --config "$src/scheduler/config.json" >"$WORK/once.out" 2>&1) || once_rc=$?
    [[ "$once_rc" == "79" ]] || { cat "$WORK/once.out"; fail "a concurrent --once in the source exited $once_rc, want 79"; }
    touch "$sentinel"
    local arc=0
    wait "$apid" || arc=$?
    [[ "$arc" == "0" ]] || { cat "$WORK/apply.out"; fail "apply exited $arc"; }
    sed -n '/source: unit=/,$p' "$WORK/apply.out" | grep -v '^\[service-layout\] change:' || true
    grep -q "execution proof:" "$WORK/apply.out" || fail "apply printed no execution proof"
    grep -q "preserved primary" "$WORK/apply.out" || fail "apply printed no preservation proof"
    local tunit="go-trader@$inst.service"
    [[ "$(systemctl is-active "$tunit")" == "active" ]] || fail "$tunit is not active"
    [[ "$(unit_prop "$tunit" User)" == "go-trader" ]] || fail "$tunit does not run as go-trader"
    [[ "$(unit_prop "$tunit" ProtectSystem)" == "strict" ]] || fail "$tunit lacks ProtectSystem=strict"
    [[ "$(unit_prop "$tunit" PrivateTmp)" == "yes" ]] || fail "$tunit lacks PrivateTmp"
    local tpid
    tpid=$(unit_prop "$tunit" MainPID)
    [[ "$(health_field "$port" pid)" == "$tpid" ]] || fail "health pid differs from $tunit MainPID"
    [[ "$(health_field "$port" version)" == "$src_version" ]] || fail "the target reports another version"
    [[ "$(unit_prop "$unit" LoadState)" == "masked" ]] || fail "$unit is not masked"
    [[ "$(systemctl is-active "$unit" 2>/dev/null || true)" != "active" ]] || fail "$unit is still active"
    [[ "$(unit_prop "$peer" MainPID)" == "$peer_pid" ]] || fail "the peer unit restarted"
    "$PY3" - "/proc/$tpid/environ" "$tunit" <<'PY' || fail "the target environment differs"
import sys
env = dict(x.split("=", 1) for x in open(sys.argv[1], "rb").read().decode().split("\0") if "=" in x)
assert env.get("FIXTURE_SECRET") == "a b$c", env.get("FIXTURE_SECRET")
assert env.get("FIXTURE_INLINE") == "x y", env.get("FIXTURE_INLINE")
assert env.get("GO_TRADER_SERVICE") == sys.argv[2], env.get("GO_TRADER_SERVICE")
PY
    [[ "$(stat -c '%a %U' "/opt/go-trader-$inst/.env")" == "600 go-trader" ]] || fail ".env is not 600 go-trader"
    [[ "$(stat -c '%a %U' "/opt/go-trader-$inst")" == "700 go-trader" ]] || fail "the tree of a private source is not 700 go-trader"
    [[ "$(stat -c '%a %U' "/var/lib/go-trader/$inst/config.json" "/opt/go-trader-$inst/scheduler/state.db" "/var/lib/go-trader/$inst/paper-state.db" "/opt/go-trader-$inst/scheduler/source-btc.db" | sort -u)" == "600 go-trader" ]] || fail "the target config or a database is not 600 go-trader"
    [[ "$(stat -c '%a' "/var/lib/go-trader/$inst/tuning_runs")" == "700" ]] || fail "tuning_runs is not 700"
    [[ "$(stat -c '%U' "/opt/go-trader-$inst" "/opt/go-trader-$inst/scheduler" "/var/lib/go-trader/$inst/tuning_runs" | sort -u)" == "go-trader" ]] || fail "the new tree is not owned by go-trader"
    [[ ! -e "/opt/go-trader-$inst/scheduler/ohlcv_cache.sqlite3" ]] || fail "the OHLCV cache was copied"
    [[ "$(file_fp "/opt/go-trader-$inst/go-trader")" == "$(file_fp "$src/go-trader")" ]] || fail "binary differs"
    [[ "$(readlink "/opt/go-trader-$inst/scheduler/config.json")" == "/var/lib/go-trader/$inst/config.json" ]] || fail "no transition symlink"
    [[ -f "/var/lib/go-trader/$inst/paper-state.db" ]] || fail "the outside paper database did not move to the state directory"
    grep -q '"paper_db_file": "/var/lib/go-trader/'"$inst"'/paper-state.db"' "/var/lib/go-trader/$inst/config.json" || fail "paper_db_file was not rewritten"
    expect_exit 0 tool apply --unit "$unit" --instance "$inst"
    grep -q "already applied" "$WORK/last.out" || fail "a repeated apply did not report already applied"
    expect_exit 0 tool status --instance "$inst"

    note "rollback returns the target's newer records"
    local tdb="/opt/go-trader-$inst/scheduler/state.db" tpaper="/var/lib/go-trader/$inst/paper-state.db"
    sql "$tdb" "INSERT INTO trades (strategy_id, timestamp, symbol, side, quantity, price, value, trade_type, details) VALUES ('hl-live-a', '2026-09-01T00:00:00Z', 'ETH', 'buy', 1, 100, 100, 'fixture', 'fixture-trade-$ID')"
    sql "$tpaper" "INSERT INTO pending_manual_actions (strategy_id, action, symbol, side, quantity, fill_price, created_at) VALUES ('fixture-unknown', 'open', 'ETH', 'buy', 1, 100, '2026-09-01T00:00:00Z')"
    sql "$tpaper" "UPDATE strategies SET risk_peak_value = 4242 WHERE id = 'hl-live-a'" || true
    local tcycle
    tcycle=$(sql "$tdb" "SELECT cycle_count FROM app_state WHERE id = 1")
    local s2="$WORK/release-recovery"
    MIGRATE_SERVICE_LAYOUT_PAUSE_AT="recovery-snapshot:$s2" "$PY3" "$TOOL" rollback --instance "$inst" >"$WORK/rb.out" 2>&1 &
    local rpid=$!
    wait_journal_stage "$inst" recovery-snapshot
    locks_all_busy "${locks[@]}" || fail "a lock was free during the reverse transfer"
    touch "$s2"
    local rrc=0
    wait "$rpid" || rrc=$?
    [[ "$rrc" == "0" ]] || { cat "$WORK/rb.out"; fail "rollback exited $rrc"; }
    assert_source_running "$unit" "$port"
    assert_target_down "$inst"
    [[ -n "$(sql "$src/scheduler/state.db" "SELECT 1 FROM trades WHERE details = 'fixture-trade-$ID'")" ]] || fail "the target's new trade is not in the source"
    [[ -n "$(sql "/srv/gt-$ID-live/paper.db" "SELECT 1 FROM pending_manual_actions WHERE strategy_id = 'fixture-unknown'")" ]] || fail "the target's pending action is not in the source"
    local scycle
    scycle=$(sql "$src/scheduler/state.db" "SELECT cycle_count FROM app_state WHERE id = 1")
    [[ "$scycle" -ge "$tcycle" ]] || fail "source cycle_count $scycle is older than the target's $tcycle"
    ls -d "/opt/go-trader-$inst".rolled-back-* >/dev/null 2>&1 || fail "the target tree was not kept aside"
    expect_exit 0 tool rollback --instance "$inst"
    grep -q "already closed" "$WORK/last.out" || fail "a repeated rollback did not report already closed"

    note "apply again after the rollback"
    expect_exit 0 tool apply --unit "$unit" --instance "$inst"
    [[ "$(systemctl is-active "go-trader@$inst.service")" == "active" ]] || fail "re-apply did not start the target"
    [[ -n "$(sql "/opt/go-trader-$inst/scheduler/state.db" "SELECT 1 FROM trades WHERE details = 'fixture-trade-$ID'")" ]] || fail "re-apply lost the returned trade"
    expect_exit 0 tool rollback --instance "$inst"
    assert_source_running "$unit" "$port"
    systemctl stop "$peer"
    echo "apply/rollback OK"
}

scenario_stages() {
    note "forced failure after every stage"
    local port=$((BASE_PORT + 5)) unit="go-trader-$ID-ff.service" inst="f-$ID" src="/root/gt-$ID-ff"
    INSTANCES+=("$inst")
    make_deployment ff "$port"
    local stage pid_before rc
    for stage in begin tree-intent tree config verify-runtime template prepared; do
        pid_before=$(unit_prop "$unit" MainPID)
        rc=0
        MIGRATE_SERVICE_LAYOUT_FAIL_AFTER="$stage" "$PY3" "$TOOL" apply --unit "$unit" --instance "$inst" >"$WORK/ff.out" 2>&1 || rc=$?
        [[ "$rc" == "20" ]] || { cat "$WORK/ff.out"; fail "failure after $stage exited $rc, want 20"; }
        [[ "$(unit_prop "$unit" MainPID)" == "$pid_before" ]] || fail "failure after $stage restarted the source"
        [[ ! -e "/opt/go-trader-$inst" && ! -e "/var/lib/go-trader/$inst" ]] || fail "failure after $stage left target artifacts"
        echo "stage $stage: exit 20, source untouched"
    done
    for stage in source-stop-intent source-stopped source-mask-intent source-masked locks snapshot transfer validated; do
        rc=0
        MIGRATE_SERVICE_LAYOUT_FAIL_AFTER="$stage" "$PY3" "$TOOL" apply --unit "$unit" --instance "$inst" >"$WORK/ff.out" 2>&1 || rc=$?
        [[ "$rc" == "21" ]] || { cat "$WORK/ff.out"; fail "failure after $stage exited $rc, want 21"; }
        assert_source_running "$unit" "$port"
        assert_target_down "$inst"
        [[ ! -e "/opt/go-trader-$inst" ]] || fail "failure after $stage left the target tree"
        echo "stage $stage: exit 21, source restored"
    done
    for stage in target-enable-intent target-installed target-start-attempt health execution; do
        rc=0
        MIGRATE_SERVICE_LAYOUT_FAIL_AFTER="$stage" "$PY3" "$TOOL" apply --unit "$unit" --instance "$inst" >"$WORK/ff.out" 2>&1 || rc=$?
        [[ "$rc" == "22" ]] || { cat "$WORK/ff.out"; fail "failure after $stage exited $rc, want 22"; }
        assert_source_running "$unit" "$port"
        assert_target_down "$inst"
        echo "stage $stage: exit 22, target records returned"
    done
    rc=0
    MIGRATE_SERVICE_LAYOUT_FAIL_AFTER="snapshot,recovery-begin" "$PY3" "$TOOL" apply --unit "$unit" --instance "$inst" >"$WORK/last.out" 2>&1 || rc=$?
    [[ "$rc" == "30" ]] || fail "a failed source recovery exited $rc, want 30"
    [[ "$(systemctl is-active "$unit" 2>/dev/null || true)" != "active" ]] || fail "the source runs after a failed recovery"
    simulate_reboot "$unit" "go-trader@$inst.service"
    expect_exit 0 tool rollback --instance "$inst"
    assert_source_running "$unit" "$port"
    assert_target_down "$inst"
    [[ ! -e "/etc/systemd/system/go-trader@$inst.service" ]] || fail "the target stays masked after the recovery"
    rc=0
    MIGRATE_SERVICE_LAYOUT_FAIL_AFTER="execution,recovery-snapshot" "$PY3" "$TOOL" apply --unit "$unit" --instance "$inst" >"$WORK/last.out" 2>&1 || rc=$?
    [[ "$rc" == "30" ]] || fail "a failed reverse transfer after the start exited $rc, want 30"
    simulate_reboot "$unit" "go-trader@$inst.service"
    [[ "$(systemctl is-active "go-trader@$inst.service" 2>/dev/null || true)" != "active" ]] || fail "the target restarted after a failed recovery"
    expect_exit 0 tool rollback --instance "$inst"
    assert_source_running "$unit" "$port"
    assert_target_down "$inst"
    [[ ! -e "/etc/systemd/system/go-trader@$inst.service" ]] || fail "the target stays masked after the rollback"
    systemctl stop "$unit"
    echo "forced failures OK"
}

scenario_kill() {
    note "process loss and reboot during apply"
    local port=$((BASE_PORT + 6)) unit="go-trader-$ID-kill.service" inst="k-$ID" src="/root/gt-$ID-kill"
    INSTANCES+=("$inst")
    make_deployment kill "$port"
    local stage rc
    for stage in source-masked transfer target-start-attempt health; do
        rc=0
        MIGRATE_SERVICE_LAYOUT_KILL_AFTER="$stage" "$PY3" "$TOOL" apply --unit "$unit" --instance "$inst" >"$WORK/kill.out" 2>&1 || rc=$?
        [[ "$rc" == "137" ]] || { cat "$WORK/kill.out"; fail "kill after $stage exited $rc, want 137"; }
        tool status --instance "$inst" >"$WORK/st.out" 2>&1 || true
        grep -q "incomplete" "$WORK/st.out" || { cat "$WORK/st.out"; fail "status does not report the interrupted transaction"; }
        expect_exit 18 tool apply --unit "$unit" --instance "$inst"
        simulate_reboot "$unit" "go-trader@$inst.service"
        expect_exit 0 tool rollback --instance "$inst"
        assert_source_running "$unit" "$port"
        assert_target_down "$inst"
        echo "kill after $stage: recovered"
    done
    systemctl stop "$unit"
    echo "interrupted transactions OK"
}

scenario_fold() {
    note "paper fold on migrated units uses the standard defaults"
    local lport=$((BASE_PORT + 7)) pport=$((BASE_PORT + 8))
    local lunit="go-trader-$ID-flive.service" punit="go-trader-$ID-fpaper.service" linst="fl-$ID" pinst="fp-$ID"
    INSTANCES+=("$linst" "$pinst")
    make_deployment flive "$lport" paperfile
    make_deployment fpaper "$pport" nogit
    expect_exit 0 tool apply --unit "$lunit" --instance "$linst"
    expect_exit 0 tool apply --unit "$punit" --instance "$pinst"
    systemctl stop "go-trader@$linst.service" "go-trader@$pinst.service"
    expect_exit 0 bash "$SCRIPT_DIR/merge-paper-instance.sh" --live "$linst" --source "fx=$pinst"
    grep -q "VERDICT: READY" "$WORK/last.out" || fail "the dry-run fold did not pass"
    expect_exit 0 bash "$SCRIPT_DIR/merge-paper-instance.sh" --live "$linst" --source "fx=$pinst" --apply
    systemctl daemon-reload
    systemctl disable "go-trader@$pinst.service" >/dev/null 2>&1 || true
    systemctl start "go-trader@$linst.service"
    wait_health "go-trader@$linst.service" "$lport"
    local logs="" i
    for i in $(seq 1 30); do
        logs=$( { journalctl --namespace=+go-trader -u "go-trader@$linst.service" -n 400 --no-pager 2>/dev/null; journalctl -u "go-trader@$linst.service" -n 400 --no-pager 2>/dev/null; } || true)
        grep -q "\[storage\] layout: split" <<<"$logs" && break
        sleep 1
    done
    grep -q "\[storage\] layout: split" <<<"$logs" || { echo "$logs" | tail -n 60 >&2; fail "the folded live unit did not report the split layout"; }
    systemctl stop "go-trader@$linst.service"
    grep -q '"db_file": "/opt/go-trader-'"$pinst"'/scheduler/state.db"' "/var/lib/go-trader/$linst/config.json" || fail "the fold did not keep the paper source at its own path"
    expect_exit 0 tool rollback --instance "$linst"
    assert_source_running "$lunit" "$lport"
    systemctl stop "$lunit"
    echo "fold OK"
}

scenario_conflict() {
    note "rollback refuses a source changed outside the transaction; a failed reverse transfer holds both units"
    local port=$((BASE_PORT + 10)) unit="go-trader-$ID-cf.service" inst="x-$ID" src="/root/gt-$ID-cf"
    INSTANCES+=("$inst")
    make_deployment cf "$port" split
    expect_exit 0 tool apply --unit "$unit" --instance "$inst"
    local txn snapdir
    txn=$(find "$STATE_ROOT/$inst" -mindepth 1 -maxdepth 1 -type d -printf '%f\n' | sort | tail -n 1)
    snapdir="$STATE_ROOT/$inst/$txn/evidence/snapshot"
    [[ -f "$snapdir/db_file.db" ]] || fail "no apply snapshot at $snapdir"
    local tcfg="/var/lib/go-trader/$inst/config.json"
    cp -p "$tcfg" "$WORK/cf-config.json"
    "$PY3" - "$tcfg" <<'PY'
import json, sys
c = json.load(open(sys.argv[1]))
c["paper_sources"].append({"id": "eth", "db_file": "scheduler/source-eth.db"})
json.dump(c, open(sys.argv[1], "w"), indent=2)
PY
    expect_exit 31 tool rollback --instance "$inst"
    grep -q "paper_sources\[1\].db_file was added or changed after the apply" "$WORK/last.out" || { cat "$WORK/last.out"; fail "rollback did not name the added state file"; }
    [[ "$(systemctl is-active "go-trader@$inst.service")" == "active" ]] || fail "a refused rollback stopped the target"
    "$PY3" - "$tcfg" "/var/lib/go-trader/$inst/paper-source-eth.db" <<'PY'
import json, sys
c = json.load(open(sys.argv[1]))
c["paper_sources"][1]["db_file"] = sys.argv[2]
json.dump(c, open(sys.argv[1], "w"), indent=2)
PY
    expect_exit 31 tool rollback --instance "$inst"
    grep -q "paper_sources\[1\].db_file /var/lib/go-trader/$inst/paper-source-eth.db was added after the apply inside /var/lib/go-trader/$inst, which the rollback moves aside" "$WORK/last.out" || { cat "$WORK/last.out"; fail "rollback did not refuse a state file inside a moved-aside folder"; }
    cp -p "$WORK/cf-config.json" "$tcfg"
    sql "$src/scheduler/state.db" "INSERT INTO trades (strategy_id, timestamp, symbol, side, quantity, price, value) VALUES ('hl-cf-a', '2026-09-02T00:00:00Z', 'ETH', 'buy', 1, 1, 1)"
    expect_exit 31 tool rollback --instance "$inst"
    [[ "$(systemctl is-active "go-trader@$inst.service")" == "active" ]] || fail "a refused rollback stopped the target"
    [[ "$(unit_prop "$unit" LoadState)" == "masked" ]] || fail "a refused rollback unmasked the source"
    rm -f "$src/scheduler/state.db-wal" "$src/scheduler/state.db-shm"
    cp "$snapdir/db_file.db" "$src/scheduler/state.db"
    [[ -f "$snapdir/db_file.db-wal" ]] && cp "$snapdir/db_file.db-wal" "$src/scheduler/state.db-wal"
    sql "/opt/go-trader-$inst/scheduler/source-btc.db" "INSERT INTO trades (strategy_id, timestamp, symbol, side, quantity, price, value, details) VALUES ('hl-cf-src', '2026-09-03T00:00:00Z', 'ETH', 'buy', 1, 1, 1, 'late-$ID')"
    local rc=0
    MIGRATE_SERVICE_LAYOUT_FAIL_AFTER="recovery-write-intent#2" "$PY3" "$TOOL" rollback --instance "$inst" >"$WORK/last.out" 2>&1 || rc=$?
    [[ "$rc" == "30" ]] || fail "a failed reverse transfer exited $rc, want 30"
    grep -q "evidence in" "$WORK/last.out" || fail "the recovery failure does not name its evidence"
    [[ "$(systemctl is-active "go-trader@$inst.service" 2>/dev/null || true)" != "active" ]] || fail "the target runs after a failed recovery"
    [[ "$(systemctl is-active "$unit" 2>/dev/null || true)" != "active" ]] || fail "the source runs after a failed recovery"
    simulate_reboot "$unit" "go-trader@$inst.service"
    tool status --instance "$inst" >"$WORK/st.out" 2>&1 || true
    grep -q "ACTION: run 'rollback" "$WORK/st.out" || { cat "$WORK/st.out"; fail "status does not ask for a rollback"; }
    expect_exit 0 tool rollback --instance "$inst"
    assert_source_running "$unit" "$port"
    assert_target_down "$inst"
    [[ -n "$(sql "$src/scheduler/source-btc.db" "SELECT 1 FROM trades WHERE details = 'late-$ID'")" ]] || fail "the resumed rollback lost the target's late trade"
    systemctl stop "$unit"
    echo "conflict and recovery failure OK"
}

scenario_signal() {
    note "signals during apply recover"
    local port=$((BASE_PORT + 11)) unit="go-trader-$ID-sig.service" inst="s-$ID"
    INSTANCES+=("$inst")
    make_deployment sig "$port"
    local apid rc
    MIGRATE_SERVICE_LAYOUT_PAUSE_AT="transfer:$WORK/never" "$PY3" "$TOOL" apply --unit "$unit" --instance "$inst" >"$WORK/last.out" 2>&1 &
    apid=$!
    wait_journal_stage "$inst" transfer
    kill -TERM "$apid"
    rc=0
    wait "$apid" || rc=$?
    [[ "$rc" == "21" ]] || fail "SIGTERM before the target start exited $rc, want 21"
    assert_source_running "$unit" "$port"
    MIGRATE_SERVICE_LAYOUT_PAUSE_AT="health:$WORK/never" "$PY3" "$TOOL" apply --unit "$unit" --instance "$inst" >"$WORK/last.out" 2>&1 &
    apid=$!
    sleep 1
    wait_journal_stage "$inst" health
    kill -HUP "$apid"
    rc=0
    wait "$apid" || rc=$?
    [[ "$rc" == "22" ]] || fail "SIGHUP after the target start exited $rc, want 22"
    [[ -s "$STATE_ROOT/$inst/interrupted.log" ]] || fail "SIGHUP did not keep the recovery output"
    assert_source_running "$unit" "$port"
    assert_target_down "$inst"
    systemctl stop "$unit"
    echo "signals OK"
}

scenario_resume() {
    note "a rollback interrupted by process loss resumes"
    local port=$((BASE_PORT + 12)) unit="go-trader-$ID-rs.service" inst="z-$ID" src="/root/gt-$ID-rs"
    INSTANCES+=("$inst")
    make_deployment rs "$port" split
    local stage rc n=0
    for stage in "recovery-write-intent#2" recovery-transferred; do
        n=$((n + 1))
        expect_exit 0 tool apply --unit "$unit" --instance "$inst"
        sql "/opt/go-trader-$inst/scheduler/source-btc.db" "INSERT INTO trades (strategy_id, timestamp, symbol, side, quantity, price, value, details) VALUES ('hl-rs-src', '2026-09-04T00:00:00Z', 'ETH', 'buy', 1, 1, 1, 'resume-$n-$ID')"
        rc=0
        MIGRATE_SERVICE_LAYOUT_KILL_AFTER="$stage" "$PY3" "$TOOL" rollback --instance "$inst" >"$WORK/last.out" 2>&1 || rc=$?
        [[ "$rc" == "137" ]] || fail "kill after $stage exited $rc, want 137"
        simulate_reboot "$unit" "go-trader@$inst.service"
        [[ "$(systemctl is-active "go-trader@$inst.service" 2>/dev/null || true)" != "active" ]] || fail "the target restarted during a rollback"
        expect_exit 18 tool apply --unit "$unit" --instance "$inst"
        expect_exit 0 tool rollback --instance "$inst"
        assert_source_running "$unit" "$port"
        assert_target_down "$inst"
        [[ -n "$(sql "$src/scheduler/source-btc.db" "SELECT 1 FROM trades WHERE details = 'resume-$n-$ID'")" ]] || fail "the resumed rollback after $stage lost the target's trade"
        echo "rollback killed after $stage: resumed"
    done
    systemctl stop "$unit"
    echo "rollback resume OK"
}

scenario_confirm() {
    note "a live deployment needs --confirm-live before any change"
    local port=$((BASE_PORT + 9)) unit="go-trader-$ID-clive.service" inst="c-$ID" src="/root/gt-$ID-clive"
    INSTANCES+=("$inst")
    make_deployment clive "$port" live
    tool plan --unit "$unit" --instance "$inst" >"$WORK/last.out" 2>&1 || true
    grep -q "LIVE: apply needs --confirm-live $unit" "$WORK/last.out" || fail "plan did not flag the live deployment"
    local pid_before
    pid_before=$(unit_prop "$unit" MainPID)
    expect_exit 17 tool apply --unit "$unit" --instance "$inst"
    expect_exit 17 tool apply --unit "$unit" --instance "$inst" --confirm-live go-trader-other.service
    [[ ! -e "$STATE_ROOT/$inst" && ! -e "/opt/go-trader-$inst" ]] || fail "a refused live apply changed the host"
    [[ "$(unit_prop "$unit" MainPID)" == "$pid_before" ]] || fail "a refused live apply touched $unit"
    systemctl stop "$unit"
    echo "live confirmation OK"
}

scenario_latch() {
    note "a strategy the portfolio kill switch holds passes the execution proof as held"
    local port=$((BASE_PORT + 13)) unit="go-trader-$ID-latch.service" inst="h-$ID" src="/root/gt-$ID-latch" i
    INSTANCES+=("$inst")
    make_deployment latch "$port" split
    for i in $(seq 1 90); do
        [[ -n "$(health_field "$port" run_evidence.last_state_save)" ]] && break
        sleep 1
    done
    [[ -n "$(health_field "$port" run_evidence.last_state_save)" ]] || fail "$unit never saved its state"
    systemctl stop "$unit"
    latch_kill_switch "$src/scheduler/source-btc.db"
    systemctl start "$unit"
    wait_health "$unit" "$port"
    for i in $(seq 1 90); do
        [[ "$(health_field "$port" run_evidence.held.hl-latch-src.reason)" == "portfolio_kill_switch" ]] && break
        sleep 1
    done
    [[ "$(health_field "$port" run_evidence.held.hl-latch-src.reason)" == "portfolio_kill_switch" ]] || fail "$unit does not report hl-latch-src as held"
    tool plan --unit "$unit" --instance "$inst" >"$WORK/plan.out" 2>&1 || { cat "$WORK/plan.out"; fail "plan refused"; }
    grep -q "note: the portfolio kill switch of the running daemon held hl-latch-src" "$WORK/plan.out" || { cat "$WORK/plan.out"; fail "plan did not report the kill switch hold"; }
    local plan_id sentinel="$WORK/release-latch"
    plan_id=$(sed -n 's/.*plan id: \([0-9a-f]*\).*/\1/p' "$WORK/plan.out" | tail -n 1)
    MIGRATE_SERVICE_LAYOUT_PAUSE_AT="validated:$sentinel" "$PY3" "$TOOL" apply --unit "$unit" --instance "$inst" --plan-id "$plan_id" >"$WORK/apply.out" 2>&1 &
    local apid=$!
    wait_journal_stage "$inst" validated
    latch_kill_switch "/opt/go-trader-$inst/scheduler/source-btc.db"
    touch "$sentinel"
    local arc=0
    wait "$apid" || arc=$?
    [[ "$arc" == "0" ]] || { cat "$WORK/apply.out"; fail "apply with a latched paper source exited $arc"; }
    grep -q "execution proof: .* held by the portfolio kill switch" "$WORK/apply.out" || { cat "$WORK/apply.out"; fail "the execution proof printed no kill switch count"; }
    [[ "$(health_field "$port" run_evidence.held.hl-latch-src.reason)" == "portfolio_kill_switch" ]] || fail "the target did not hold hl-latch-src during the proof"
    echo "kill switch hold OK"
}

grep -q "migrate-service-layout" "$SCRIPT_DIR/update.sh" && fail "update.sh must never call the migration"
for s in $SCENARIOS; do
    "scenario_$s"
done
echo "OK: migrate-service-layout fixture passed ($SCENARIOS)"
