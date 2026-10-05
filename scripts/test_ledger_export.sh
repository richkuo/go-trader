#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "$SCRIPT_DIR/.." && pwd)

if [[ -z "${GO_TRADER_BIN:-}" || ! -x "${GO_TRADER_BIN:-}" ]]; then
    echo "FAIL: set GO_TRADER_BIN to an executable go-trader binary" >&2
    exit 1
fi
GO_TRADER_BIN=$(cd "$(dirname "$GO_TRADER_BIN")" && pwd)/$(basename "$GO_TRADER_BIN")

if env -u GO_TRADER_BIN bash "$SCRIPT_DIR/test_ledger_export.sh" >/dev/null 2>&1; then
    echo "FAIL: the suite passed with GO_TRADER_BIN unset" >&2
    exit 1
fi
echo "ok: the suite fails closed when GO_TRADER_BIN is unset"

WORK=$(mktemp -d "${TMPDIR:-/tmp}/ledger-export.XXXXXX")
WRITER_PID=""
cleanup() {
    if [[ -n "$WRITER_PID" ]]; then
        kill "$WRITER_PID" >/dev/null 2>&1 || true
        wait "$WRITER_PID" 2>/dev/null || true
    fi
    chmod -R u+rwX "$WORK" 2>/dev/null || true
    rm -rf "$WORK"
}
trap cleanup EXIT

fail() {
    echo "FAIL: $*" >&2
    if [[ -s "$WORK/last.err" ]]; then
        echo "--- last stderr:" >&2
        tail -n 40 "$WORK/last.err" >&2
    fi
    exit 1
}

ok() {
    echo "ok: $*"
}

CLEAN_ENV=(env -i "PATH=/usr/bin:/bin" "HOME=$WORK")

gt() {
    "${CLEAN_ENV[@]}" "$GO_TRADER_BIN" "$@" >"$WORK/last.out" 2>"$WORK/last.err"
}

expect_rc() {
    local want=$1 label=$2
    shift 2
    local rc=0
    gt "$@" || rc=$?
    [[ "$rc" == "$want" ]] || fail "$label: exit $rc, want $want"
}

OS=$(uname -s)
if [[ "$OS" != "Linux" ]]; then
    mkdir -p "$WORK/cfg" "$WORK/state"
    printf '{"config_version": 19, "db_file": "%s/state/primary.db", "strategies": []}\n' "$WORK" >"$WORK/cfg/config.json"
    expect_rc 1 "capture on $OS" export capture --config "$WORK/cfg/config.json" --output-dir "$WORK/snap"
    grep -q "Linux private mount namespace" "$WORK/last.err" || fail "capture on $OS did not name the missing confinement"
    [[ ! -e "$WORK/snap" ]] || fail "capture on $OS created its output directory"
    ok "capture on $OS refuses before creating anything"
    if [[ "${LEDGER_EXPORT_REQUIRE_CAPTURE:-0}" == "1" ]]; then
        fail "LEDGER_EXPORT_REQUIRE_CAPTURE=1 but $OS cannot run the active-WAL capture proof"
    fi
    echo "SKIP: capture-dependent checks need Linux with private mount namespaces; this does not prove the active-WAL capture criterion"
    exit 0
fi

FIXTURE_GO="${FIXTURE_GO:-$(command -v go || true)}"
[[ -n "$FIXTURE_GO" ]] || fail "the Go toolchain is required to build the fixture helper from scheduler/go.mod"
FX="$WORK/ledger_fixture"
"$FIXTURE_GO" -C "$REPO_ROOT/scheduler" build -o "$FX" ../scripts/fixtures/ledger_fixture.go
PINNED=$(awk '$1 == "modernc.org/sqlite" {print $2; exit}' "$REPO_ROOT/scheduler/go.mod")
HELPER_DRIVER=$("$FX" version)
[[ "$HELPER_DRIVER" == "modernc.org/sqlite $PINNED" ]] || fail "fixture helper driver is '$HELPER_DRIVER', scheduler/go.mod pins $PINNED"
echo "== host: $(uname -a)"
echo "== user: $(id)"
echo "== fixture helper driver: $HELPER_DRIVER (scheduler/go.mod pins $PINNED)"

STRACE=$(command -v strace || true)
if [[ -z "$STRACE" ]]; then
    [[ "${LEDGER_EXPORT_REQUIRE_STRACE:-0}" != "1" ]] || fail "LEDGER_EXPORT_REQUIRE_STRACE=1 but strace is not installed"
    echo "NOTE: strace is not installed; the syscall audit of capture is skipped (fingerprints still run)"
fi

make_config() {
    local dir=$1 layout=$2 version=${3:-19} extra=${4:-}
    mkdir -p "$dir/cfg" "$dir/state"
    local storage
    if [[ "$layout" == "split" ]]; then
        storage="\"db_file\": \"$dir/state/primary.db\", \"paper_db_file\": \"$dir/state/paper.db\", \"paper_sources\": [{\"id\": \"alpha\", \"db_file\": \"$dir/state/alpha.db\"}],"
    else
        storage="\"db_file\": \"$dir/state/primary.db\","
    fi
    local src_strategy=""
    if [[ "$layout" == "split" ]]; then
        src_strategy=', {"id": "hl-src-btc", "type": "perps", "platform": "hyperliquid", "script": "shared_scripts/check_hyperliquid.py", "args": ["sma_crossover", "BTC", "1h", "--mode=paper"], "capital": 1000, "paper_source": "alpha", "storage_strategy_id": "hl-paper-btc"}'
    fi
    cat >"$dir/cfg/config.json" <<JSON
{
  "config_version": $version,
  "interval_seconds": 600,
  $storage
  "discord": {"token": "fixture-discord-token-not-exported", "report_github_token": "fixture-report-token-not-copied"},
  "telegram": {"bot_token": "fixture-telegram-token-not-copied"},
  "strategies": [$extra
    {"id": "hl-live-btc", "type": "perps", "platform": "hyperliquid", "script": "shared_scripts/check_hyperliquid.py", "args": ["sma_crossover", "BTC", "1h", "--mode=live"], "capital": 1000},
    {"id": "hl-live-eth", "type": "perps", "platform": "hyperliquid", "script": "shared_scripts/check_hyperliquid.py", "args": ["sma_crossover", "ETH", "1h", "--mode=live"], "capital": 1000},
    {"id": "hl-live-sol", "type": "perps", "platform": "hyperliquid", "script": "shared_scripts/check_hyperliquid.py", "args": ["sma_crossover", "SOL", "1h", "--mode=live"], "capital": 1000},
    {"id": "hl-manual-eth", "type": "manual", "platform": "hyperliquid", "symbol": "ETH", "timeframe": "1h", "capital": 500, "leverage": 1},
    {"id": "hl-paper-btc", "type": "perps", "platform": "hyperliquid", "script": "shared_scripts/check_hyperliquid.py", "args": ["sma_crossover", "BTC", "1h", "--mode=paper"], "capital": 1000},
    {"id": "momentum-btc", "type": "spot", "script": "shared_scripts/check_strategy.py", "args": ["momentum", "BTC/USDT", "1h"], "capital": 1000}$src_strategy
  ]
}
JSON
}

create_schema() {
    local dir=$1
    local rc=0
    (cd "$dir" && env -i "PATH=/usr/bin:/bin" "HOME=$WORK" HYPERLIQUID_SECRET_KEY=fixture-schema-only \
        "$GO_TRADER_BIN" export tradingview --config "$dir/cfg/config.json" --all --output "$dir/schema.csv") \
        >"$WORK/schema.out" 2>"$WORK/schema.err" || rc=$?
    grep -q "no trade data found" "$WORK/schema.err" || { cat "$WORK/schema.err" >&2; fail "schema creation through the migrating opener failed (rc $rc)"; }
    rm -f "$dir/schema.csv"
}

T_COLS="rowid, strategy_id, timestamp, symbol, position_id, side, quantity, price, value, trade_type, details, exchange_order_id, exchange_fee, is_close, realized_pnl, pnl_gross, fee_source, regime, entry_atr, stop_loss_atr_mult, stop_loss_trigger_px, stop_loss_oid, tp_oids_json, tp_tiers_json, manual"

seed_split() {
    local dir=$1
    "$FX" exec "$dir/state/primary.db" \
        "INSERT INTO strategies (id, type, platform, cash, initial_capital) VALUES ('hl-live-btc','perps','hyperliquid',1000,1000), ('hl-live-eth','perps','hyperliquid',1000,1000), ('hl-manual-eth','manual','hyperliquid',500,500)" \
        "INSERT INTO trades ($T_COLS) VALUES (5,'hl-live-btc','2026-01-01T00:00:00Z','BTC','pos-1','buy',0.1,50000,5000,'perps','Open long BTC','oid-5',2.5,0,0,0,'userfills','trending_up',120.5,1.5,49000,9007199254740993,'[11,12]','[{\"atr_multiple\":1,\"close_fraction\":0.5}]',0)" \
        "INSERT INTO trades ($T_COLS) VALUES (6,'hl-manual-eth','2026-01-01T00:30:00Z','ETH','pos-m1','buy',1,3000,3000,'manual','Manual open','oid-6',1,0,0,0,'userfills','',0,NULL,0,0,'','',1)" \
        "INSERT INTO trades ($T_COLS) VALUES (7,'hl-manual-eth','2026-01-01T04:00:00Z','ETH','pos-m1','sell',1,3100,3100,'manual','Manual close','oid-7',1,1,100,0,'userfills','',0,NULL,0,0,'','',1)" \
        "INSERT INTO trades ($T_COLS) VALUES (9,'hl-live-btc','2026-01-01T01:00:00.123456789+02:00','BTC','pos-1','buy',0.05,51000,2550,'scale_in','Scale-in long','oid-9',1.25,0,0,0,'userfills','trending_up',120.5,1.5,49000,9007199254740993,'[11,12]','',0)" \
        "INSERT INTO trades ($T_COLS) VALUES (17,'hl-live-btc','2026-01-01T02:00:00Z','BTC','pos-1','sell',0.05,52000,2600,'perps','Partial close','oid-17',1.25,1,100,1,'userfills','trending_up',120.5,1.5,49000,9007199254740993,'[11,12]','',0)" \
        "INSERT INTO trades ($T_COLS) VALUES (18,'hl-live-btc','2026-01-01T02:00:00Z','BTC','','funding',0,0,0,'funding','Funding payment','funding:abc',0,0,-0.75,1,'','',0,NULL,0,0,'','',0)" \
        "INSERT INTO trade_diagnostics (rowid, strategy_id, position_id, symbol, side, close_reason, entry_price, exit_price, quantity, realized_pnl, closed_at) VALUES (40,'hl-live-btc','pos-1','BTC','long','tp_tier',50333.3,53000,0.1,300,'2026-01-01T03:00:00.500Z')" \
        "INSERT INTO trade_diagnostics (rowid, strategy_id, position_id, symbol, side, close_reason, entry_price, exit_price, quantity, realized_pnl, closed_at) VALUES (41,'hl-manual-eth','pos-m1','ETH','long','manual',3000,3100,1,100,'2026-01-01T04:00:00Z'), (42,'hl-manual-eth','pos-m1','ETH','long','manual_dup',3000,3100,1,100,'2026-01-01T04:00:00+00:00')" \
        "INSERT INTO wallet_transfers (rowid, platform, account, time_ms, kind, amount_usd, dedup_id) VALUES (3,'hyperliquid','0xabc',1767225600000,'funding_orphan',-0.33,'funding_orphan:x1'), (4,'hyperliquid','0xabc',1767225600001,'deposit',100,'deposit:y1'), (5,'okx','acct',1767225600002,'funding_orphan',1,'funding_orphan:okx')" \
        "WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c WHERE x < 1203) INSERT INTO trades (rowid, strategy_id, timestamp, symbol, position_id, side, quantity, price, value, trade_type, exchange_fee, is_close, realized_pnl, pnl_gross) SELECT 10000 + x * 3, 'hl-live-eth', '2026-02-01T00:00:00Z', 'ETH', 'pos-e' || x, CASE WHEN x % 2 = 0 THEN 'sell' ELSE 'buy' END, 1, 3000, 3000, 'perps', 0.5, x % 2 = 0, CASE WHEN x % 2 = 0 THEN 10 ELSE 0 END, 0 FROM c"
    "$FX" exec "$dir/state/paper.db" \
        "INSERT INTO strategies (id, type, platform, cash, initial_capital) VALUES ('hl-paper-btc','perps','hyperliquid',1000,1000)" \
        "INSERT INTO trades ($T_COLS) VALUES (2,'hl-paper-btc','2026-03-01T00:00:00Z','BTC','pp-1','buy',0.2,40000,8000,'perps','Paper open','',4,0,0,0,'modeled','',0,NULL,0,0,'','',0)" \
        "INSERT INTO trades ($T_COLS) VALUES (3,'hl-paper-btc','2026-03-01T05:00:00Z','BTC','pp-1','sell',0.2,41000,8200,'perps','Paper close','',4,1,196,0,'modeled','',0,NULL,0,0,'','',0)"
    "$FX" exec "$dir/state/alpha.db" \
        "INSERT INTO strategies (id, type, platform, cash, initial_capital) VALUES ('hl-paper-btc','perps','hyperliquid',1000,1000)" \
        "INSERT INTO trades ($T_COLS) VALUES (2,'hl-paper-btc','2026-04-01T00:00:00Z','BTC','src-1','buy',0.3,30000,9000,'perps','Source open','',3,0,0,0,'modeled','',0,NULL,0,0,'','',0)"
}

WAL_ONLY_PRIMARY=(
    "INSERT INTO trades ($T_COLS) VALUES (1001,'hl-live-btc','2026-01-01T03:00:00.5Z','BTC','pos-1','sell',0.1,53000,5300,'perps','Full close','oid-1001',-0.5,1,300,0,'userfills','trending_up',120.5,1.5,49000,9007199254740993,'[11,12]','',0)"
    "INSERT INTO trades ($T_COLS) VALUES (4000,'hl-live-btc','2026-01-02T00:00:00Z','BTC','','buy',1,3000,3000,'','',  '',1.5,0,0,0,'','',0,NULL,0,0,'','',0)"
    "INSERT INTO wallet_transfers (rowid, platform, account, time_ms, kind, amount_usd, dedup_id) VALUES (2000,'hyperliquid','0xabc',1767229200000,'funding_orphan',0.125,'funding_orphan:x2')"
)
WAL_ONLY_PAPER=("INSERT INTO trades ($T_COLS) VALUES (900,'hl-paper-btc','2026-03-02T00:00:00Z','BTC','pp-2','buy',0.1,42000,4200,'perps','Paper reopen','',2,0,0,0,'modeled','',0,NULL,0,0,'','',0)")
WAL_ONLY_ALPHA=("INSERT INTO trades ($T_COLS) VALUES (77,'hl-paper-btc','2026-04-02T00:00:00Z','BTC','src-1','sell',0.3,31000,9300,'perps','Source close','',3,1,294,0,'modeled','',0,NULL,0,0,'','',0)")

start_writer() {
    local fifo_in="$WORK/writer.in" fifo_out="$WORK/writer.out"
    rm -f "$fifo_in" "$fifo_out"
    mkfifo "$fifo_in" "$fifo_out"
    "$FX" writer "$@" <"$fifo_in" >"$fifo_out" &
    WRITER_PID=$!
    exec 7>"$fifo_in"
    exec 8<"$fifo_out"
    local reply
    read -r reply <&8
    [[ "$reply" == "ready" ]] || fail "writer did not start: $reply"
}

writer_cmd() {
    echo "$1" >&7
    local reply
    read -r reply <&8
    [[ "$reply" == "ok" || "$reply" == "closed" || "$reply" == "crashing" ]] || fail "writer command failed: $1 -> $reply"
}

stop_writer() {
    local how=${1:-close}
    echo "$how" >&7
    local reply
    read -r reply <&8 || true
    wait "$WRITER_PID" 2>/dev/null || true
    WRITER_PID=""
    exec 7>&- 8<&-
}

capture() {
    local cfg=$1 out=$2 trace=${3:-}
    local rc=0
    if [[ -n "$trace" && -n "$STRACE" ]]; then
        "${CLEAN_ENV[@]}" "$STRACE" -f -y -qq -o "$trace" \
            -e trace=open,openat,creat,unlink,unlinkat,rename,renameat,renameat2,truncate,ftruncate,fchown,fchownat,chown,lchown,fchmod,fchmodat,chmod,mkdir,mkdirat,rmdir,link,linkat,symlink,symlinkat,write,pwrite64,writev,pwritev,fallocate,utimensat,mmap,setxattr,fsetxattr \
            "$GO_TRADER_BIN" export capture --config "$cfg" --output-dir "$out" >"$WORK/last.out" 2>"$WORK/last.err" || rc=$?
    else
        gt export capture --config "$cfg" --output-dir "$out" || rc=$?
    fi
    return $rc
}

ledger() {
    gt export ledger --manifest "$1" --partition "$2" --strategy "$3" --output "$4"
}

expect_export_refused() {
    local label=$1 manifest=$2 part=$3 strat=$4 out=$5 pattern=$6
    local rc=0
    ledger "$manifest" "$part" "$strat" "$out" || rc=$?
    [[ "$rc" == "1" ]] || fail "$label: exit $rc, want 1"
    [[ ! -e "$out" ]] || fail "$label: left an output file"
    grep -Eq "$pattern" "$WORK/last.err" || fail "$label: stderr does not match /$pattern/"
    ok "$label refused"
}

get() {
    "$FX" get "$@"
}

eq() {
    local label=$1 got=$2 want=$3
    [[ "$got" == "$want" ]] || fail "$label: got '$got', want '$want'"
}

# ---------------------------------------------------------------- split fixture
SPLIT="$WORK/split"
make_config "$SPLIT" split
create_schema "$SPLIT"
seed_split "$SPLIT"
start_writer "$SPLIT/state/primary.db" "$SPLIT/state/paper.db" "$SPLIT/state/alpha.db"
for s in "${WAL_ONLY_PRIMARY[@]}"; do writer_cmd "exec 0 $s"; done
for s in "${WAL_ONLY_PAPER[@]}"; do writer_cmd "exec 1 $s"; done
for s in "${WAL_ONLY_ALPHA[@]}"; do writer_cmd "exec 2 $s"; done
for f in primary paper alpha; do
    [[ -s "$SPLIT/state/$f.db-wal" ]] || fail "$f has no uncheckpointed WAL frames"
done
MAIN_ONLY=$("$FX" query "$SPLIT/state/primary.db" "SELECT COUNT(*) FROM trades WHERE rowid IN (1001, 4000)")
eq "WAL-only rows absent from the main primary file" "$MAIN_ONLY" "0"
ok "fixture holds committed rows only in uncheckpointed WAL frames (writer pid $WRITER_PID still open)"

"$FX" fingerprint "$WORK/src.before.json" "$SPLIT/state" "$SPLIT/cfg"
SNAP="$WORK/snapshots/split"
mkdir -p "$WORK/snapshots"
if ! capture "$SPLIT/cfg/config.json" "$SNAP" "$WORK/capture.trace"; then
    if grep -q "cannot start the capture worker in a private mount namespace" "$WORK/last.err"; then
        [[ -e "$SNAP" ]] && fail "a capture refused for missing namespace rights left its output directory"
        [[ "${LEDGER_EXPORT_REQUIRE_CAPTURE:-0}" != "1" ]] || fail "LEDGER_EXPORT_REQUIRE_CAPTURE=1 but this host cannot create the capture namespace: $(tail -n 1 "$WORK/last.err")"
        stop_writer close
        echo "SKIP: this Linux host refuses the private mount namespace capture needs (root with CAP_SYS_ADMIN or unprivileged user namespaces); the refusal left no output, but the active-WAL capture criterion is not proved here"
        exit 0
    fi
    fail "active-WAL capture refused"
fi
"$FX" fingerprint "$WORK/src.after.json" "$SPLIT/state" "$SPLIT/cfg"
cmp -s "$WORK/src.before.json" "$WORK/src.after.json" || { diff "$WORK/src.before.json" "$WORK/src.after.json" | head -30 >&2; fail "capture changed the source inventory"; }
ok "capture left every source file, sidecar and neighbor byte-identical with unchanged inode, mode, owner and times"
if [[ -n "$STRACE" ]]; then
    "$FX" trace-check "$WORK/capture.trace" "$SPLIT/state" "$SPLIT/cfg" || fail "strace saw a successful source mutation during capture"
    ok "syscall audit: no successful create, write, truncate, chown, unlink or writable shared mapping under the source directories"
fi
[[ -s "$SNAP/capture.json" ]] || fail "capture wrote no manifest"
eq "manifest method" "$(get "$SNAP/capture.json" capture_method)" "vacuum_into"
eq "manifest DSN" "$(get "$SNAP/capture.json" confinement.source_dsn_parameters)" "mode=ro&cache=private&readonly_shm=1"
eq "manifest files" "$(get "$SNAP/capture.json" files)" "array:3"
for i in 0 1 2; do
    eq "file $i integrity" "$(get "$SNAP/capture.json" "files.$i.integrity_check")" "ok"
    eq "file $i journal mode" "$(get "$SNAP/capture.json" "files.$i.journal_mode")" "delete"
    eq "file $i source journal mode" "$(get "$SNAP/capture.json" "files.$i.source_journal_mode")" "wal"
done
echo "== capture manifest: driver=$(get "$SNAP/capture.json" driver) sqlite=$(get "$SNAP/capture.json" sqlite_version) user_namespace=$(get "$SNAP/capture.json" confinement.user_namespace)"
eq "snapshot primary rowids" "$("$FX" query "$SNAP/state/primary.db" "SELECT group_concat(rowid) FROM (SELECT rowid FROM trades WHERE strategy_id <> 'hl-live-eth' ORDER BY rowid)")" "5,6,7,9,17,18,1001,4000"
eq "snapshot many-row ids" "$("$FX" query "$SNAP/state/primary.db" "SELECT COUNT(*), MIN(rowid), MAX(rowid) FROM trades WHERE strategy_id = 'hl-live-eth'")" "1203|10003|13609"
eq "snapshot paper rowids" "$("$FX" query "$SNAP/state/paper.db" "SELECT group_concat(rowid) FROM (SELECT rowid FROM trades ORDER BY rowid)")" "2,3,900"
eq "snapshot alpha rowids" "$("$FX" query "$SNAP/state/paper-source-alpha.db" "SELECT group_concat(rowid) FROM (SELECT rowid FROM trades ORDER BY rowid)")" "2,77"
eq "snapshot wallet rowids" "$("$FX" query "$SNAP/state/primary.db" "SELECT group_concat(rowid) FROM (SELECT rowid FROM wallet_transfers ORDER BY rowid)")" "3,4,5,2000"
for f in primary paper paper-source-alpha; do
    eq "$f header" "$(od -An -tu1 -j18 -N2 "$SNAP/state/$f.db" | tr -s ' ' | sed 's/^ //')" "1 1"
done
ls -A "$SNAP/state" | sort | tr '\n' ' ' | grep -qx "paper-source-alpha.db paper.db primary.db " || fail "snapshot state directory holds sidecars: $(ls -A "$SNAP/state")"
grep -q '"db_file": "state/primary.db"' "$SNAP/config.json" || fail "config copy does not point at the snapshot"
for token in fixture-discord-token fixture-report-token fixture-telegram-token; do
    grep -q "$token" "$SPLIT/cfg/config.json" || fail "source configuration lost $token"
    grep -q "$token" "$SNAP/config.json" && fail "config copy kept the credential $token"
done
grep -q '"telegram"' "$SNAP/config.json" || fail "config copy dropped the notifier sections"
ok "snapshot set: integrity ok, rollback mode, WAL-only rows and noncontiguous row ids preserved, config copy (no notifier or report credentials) and manifest hashed"

writer_cmd "exec 0 INSERT INTO trades ($T_COLS) VALUES (7000,'hl-live-btc','2026-05-01T00:00:00Z','BTC','pos-9','buy',1,1,1,'perps','after capture','',0,0,0,0,'','',0,NULL,0,0,'','',0)"
stop_writer close

# --------------------------------------------------------- capture refusals
NOWAL_OUT="$WORK/snapshots/nowal"
rc=0; capture "$SPLIT/cfg/config.json" "$NOWAL_OUT" || rc=$?
eq "capture with the writer stopped" "$rc" "1"
grep -q "no -wal file" "$WORK/last.err" || fail "stopped-writer refusal did not explain the missing WAL"
[[ ! -e "$NOWAL_OUT" ]] || fail "refused capture left its output directory"
ok "capture of a WAL-mode file with no live connection is refused and cleaned up"

STALE="$WORK/stale"
make_config "$STALE" unsplit
create_schema "$STALE"
start_writer "$STALE/state/primary.db"
writer_cmd "exec 0 INSERT INTO strategies (id, type, platform) VALUES ('hl-live-btc','perps','hyperliquid')"
stop_writer crash
"$FX" fingerprint "$WORK/stale.before.json" "$STALE/state" "$STALE/cfg"
rc=0; capture "$STALE/cfg/config.json" "$WORK/snapshots/stale" || rc=$?
eq "capture of a crashed writer's WAL" "$rc" "1"
grep -q "needs WAL recovery" "$WORK/last.err" || fail "stale WAL refusal did not name recovery"
"$FX" fingerprint "$WORK/stale.after.json" "$STALE/state" "$STALE/cfg"
cmp -s "$WORK/stale.before.json" "$WORK/stale.after.json" || fail "refused stale capture changed the source"
[[ ! -e "$WORK/snapshots/stale" ]] || fail "refused stale capture left output"
ok "recovery-dependent WAL (stale -shm, no live connection) is refused with the source unchanged"

rm -f "$STALE/state/primary.db-shm"
rc=0; capture "$STALE/cfg/config.json" "$WORK/snapshots/noshm" || rc=$?
eq "capture with -wal and no -shm" "$rc" "1"
grep -q "no -shm" "$WORK/last.err" || fail "missing -shm refusal unexplained"
ok "WAL without shared memory is refused"

HOT="$WORK/hot"
make_config "$HOT" unsplit
create_schema "$HOT"
"$FX" exec "$HOT/state/primary.db" "PRAGMA journal_mode=DELETE"
printf 'not-empty' >"$HOT/state/primary.db-journal"
rc=0; capture "$HOT/cfg/config.json" "$WORK/snapshots/hot" || rc=$?
eq "capture beside a rollback journal" "$rc" "1"
grep -q "rollback journal" "$WORK/last.err" || fail "journal refusal unexplained"
[[ "$(cat "$HOT/state/primary.db-journal")" == "not-empty" ]] || fail "journal changed"
ok "a source with a rollback journal is refused"

MISSING="$WORK/missing"
make_config "$MISSING" split
create_schema "$MISSING"
rm -f "$MISSING/state/alpha.db"
rc=0; capture "$MISSING/cfg/config.json" "$WORK/snapshots/missing" || rc=$?
eq "capture with a configured file missing" "$rc" "1"
grep -q "is missing" "$WORK/last.err" || fail "missing-file refusal unexplained"
[[ ! -e "$WORK/snapshots/missing" ]] || fail "missing-file capture created output"
ok "a missing configured state file is refused before any output exists"

rc=0; capture "$SPLIT/cfg/config.json" "$SPLIT/state/inside" || rc=$?
eq "capture into a source directory" "$rc" "1"
[[ ! -e "$SPLIT/state/inside" ]] || fail "capture wrote inside a source directory"
rc=0; capture "$SPLIT/cfg/config.json" "$SNAP" || rc=$?
eq "capture over an existing directory" "$rc" "1"
rc=0; gt export capture --config "$SPLIT/cfg/config.json" --output-dir "$WORK/x" --output-dir "$WORK/y" || rc=$?
eq "capture duplicate flag" "$rc" "2"
ok "capture refuses output inside a source directory, an existing output and duplicate flags"

MIG="$WORK/migrate"
make_config "$MIG" unsplit 15
create_schema "$MIG"
make_config "$MIG" unsplit 15
"$FX" exec "$MIG/state/primary.db" "INSERT OR IGNORE INTO strategies (id, type, platform) VALUES ('hl-live-btc','perps','hyperliquid')"
MIG_SHA=$("$FX" sha256 "$MIG/cfg/config.json")
start_writer "$MIG/state/primary.db"
"$FX" fingerprint "$WORK/mig.before.json" "$MIG/state" "$MIG/cfg"
env -i "PATH=/usr/bin:/bin" "HOME=$WORK" DISCORD_BOT_TOKEN=fixture-env-discord-token TELEGRAM_BOT_TOKEN=fixture-env-telegram-token GO_TRADER_GITHUB_TOKEN=fixture-env-report-token \
    "$GO_TRADER_BIN" export capture --config "$MIG/cfg/config.json" --output-dir "$WORK/snapshots/migrate" >"$WORK/last.out" 2>"$WORK/last.err" \
    || fail "capture of a migration-needed configuration with environment notifier tokens refused"
"$FX" fingerprint "$WORK/mig.after.json" "$MIG/state" "$MIG/cfg"
stop_writer close
cmp -s "$WORK/mig.before.json" "$WORK/mig.after.json" || fail "capture changed a migration-needed source"
eq "migration-needed source config hash" "$("$FX" sha256 "$MIG/cfg/config.json")" "$MIG_SHA"
grep -q '"config_version": 15' "$WORK/snapshots/migrate/config.json" || fail "config copy was migrated on disk"
grep -q 'fixture-' "$WORK/snapshots/migrate/config.json" && fail "migration-needed config copy kept a credential: $(grep -o 'fixture-[a-z-]*' "$WORK/snapshots/migrate/config.json" | sort -u | tr '\n' ' ')"
ok "a configuration that needs migration is captured with its source bytes unchanged (migration stays in memory) and no credential in the copy, with environment tokens set"

FORGED="$WORK/forged"
mkdir -p "$FORGED/state" "$FORGED/out"
printf '{"protect": ["%s"], "destination_dir": "%s", "files": [{"role": "primary", "source": "%s/primary.db", "dev": 0, "ino": 0, "dest": "%s/primary.db"}]}\n' \
    "$FORGED/state" "$FORGED/out" "$FORGED/state" "$FORGED/out" >"$WORK/forged.plan"
MOUNTS_BEFORE=$(sort /proc/self/mountinfo | awk '{print $5, $6}')
rc=0; "${CLEAN_ENV[@]}" "$GO_TRADER_BIN" export __capture-worker <"$WORK/forged.plan" >"$WORK/last.out" 2>"$WORK/last.err" || rc=$?
eq "worker run directly with a hand-written plan" "$rc" "1"
grep -q "not in a private mount namespace" "$WORK/last.err" || fail "direct worker run was not refused by the namespace guard"
eq "mount table after a direct worker run" "$(sort /proc/self/mountinfo | awk '{print $5, $6}')" "$MOUNTS_BEFORE"
touch "$FORGED/state/still-writable" || fail "a direct worker run left the protected directory read-only"
ok "the capture worker run directly (same mount namespace as its parent) refuses before changing any mount"
if command -v unshare >/dev/null 2>&1 && unshare --user --map-root-user true >/dev/null 2>&1; then
    rc=0; "${CLEAN_ENV[@]}" unshare --user --map-root-user "$GO_TRADER_BIN" export __capture-worker <"$WORK/forged.plan" >"$WORK/last.out" 2>"$WORK/last.err" || rc=$?
    eq "worker run in a new user namespace that shares the parent's mount namespace" "$rc" "1"
    grep -q "not in a private mount namespace" "$WORK/last.err" || fail "worker in a shared mount namespace was not refused by the namespace guard"
    eq "mount table after a user-namespace worker run" "$(sort /proc/self/mountinfo | awk '{print $5, $6}')" "$MOUNTS_BEFORE"
    ok "the capture worker in a new user namespace without its own mount namespace refuses before changing any mount"
else
    echo "NOTE: unshare --user is unavailable here; the shared-mount-namespace worker refusal in a new user namespace is not checked"
fi

# ------------------------------------------------------------- exports
EXPORTS="$WORK/exports"
mkdir -p "$EXPORTS" "$WORK/cwd"
"$FX" fingerprint "$WORK/snap.before.json" "$SNAP"

(cd "$WORK/cwd" && gt export ledger --manifest "$SNAP/capture.json" --partition live --strategy hl-live-btc --output "$EXPORTS/live.json") || fail "live export refused"
[[ -z "$(ls -A "$WORK/cwd")" ]] || fail "export created files in its working directory"
LIVE="$EXPORTS/live.json"
eq "schema" "$(get "$LIVE" schema)" "go-trader.booked-ledger"
eq "schema_version" "$(get "$LIVE" schema_version)" "1"
eq "manifest hash" "$(get "$LIVE" capture_manifest_sha256)" "$("$FX" sha256 "$SNAP/capture.json")"
eq "time basis" "$(get "$LIVE" time_basis)" "UTC"
eq "selection" "$(get "$LIVE" selection.partition selection.process_strategy_id selection.storage_strategy_id selection.source_role selection.platform | tr '\n' ' ')" "live hl-live-btc hl-live-btc primary hyperliquid "
eq "consistency" "$(get "$LIVE" capture.consistency)" "transactional_per_file"
echo "== export inspected_revision: $(get "$LIVE" inspected_revision); capture source_revision: $(get "$LIVE" capture.source_revision)"
eq "config basis" "$(get "$LIVE" current_effective_configuration.basis)" "current_at_capture"
grep -q "fixture-discord-token" "$LIVE" && fail "export leaked a notifier token"
"$FX" evidence-shape "$LIVE" >/dev/null || fail "evidence fields do not follow the version 1 shape"
eq "live event keys" "$("$FX" events "$LIVE" event_key | tr '\n' ' ')" "primary/trades/5 primary/trades/9 primary/trades/17 primary/trades/18 primary/trades/1001 primary/trades/4000 "
eq "event kinds" "$("$FX" events "$LIVE" event_kind.value | tr '\n' ' ')" "non_close scale_in close funding close non_close "
eq "row net pnl" "$("$FX" events "$LIVE" row_net_pnl.value | tr '\n' ' ')" "0 0 98.75 -0.75 300 0 "
eq "ledger delta" "$("$FX" events "$LIVE" ledger_delta.value | tr '\n' ' ')" "-2.5 -1.25 98.75 -0.75 300 -1.5 "
eq "raw fees" "$("$FX" events "$LIVE" exchange_fee.value | tr '\n' ' ')" "2.5 1.25 1.25 0 -0.5 1.5 "
eq "gross flags" "$("$FX" events "$LIVE" pnl_gross.value | tr '\n' ' ')" "false false true true false false "
eq "timestamps" "$("$FX" events "$LIVE" timestamp | tr '\n' ' ')" "2026-01-01T00:00:00Z 2025-12-31T23:00:00.123456789Z 2026-01-01T02:00:00Z 2026-01-01T02:00:00Z 2026-01-01T03:00:00.5Z 2026-01-02T00:00:00Z "
eq "raw offset timestamp" "$(get "$LIVE" events.1.timestamp_raw)" "2026-01-01T01:00:00.123456789+02:00"
eq "funding allocation" "$(get "$LIVE" events.3.position_allocation.value events.3.position_id.status events.3.position_id.reason | tr '\n' ' ')" "unallocated unavailable not_recorded "
eq "close reason matched" "$(get "$LIVE" events.4.close_reason.value events.4.close_reason.provenance.0.kind events.4.close_reason.provenance.0.source_row_id events.4.close_extent.value | tr '\n' ' ')" "tp_tier matched 40 full "
eq "partial close reason" "$(get "$LIVE" events.2.close_reason.status events.2.close_reason.reason events.2.close_extent.status | tr '\n' ' ')" "unavailable not_recorded unavailable "
eq "open close reason" "$(get "$LIVE" events.0.close_reason.status | tr '\n' ' ')" "not_applicable "
eq "oid precision" "$(get "$LIVE" events.0.stop_loss_oid.value)" "9007199254740993"
eq "geometry" "$(get "$LIVE" events.0.tp_oids_json.value events.0.entry_atr.value events.0.stop_loss_atr_mult.value events.0.stop_loss_trigger_px.value | tr '\n' '|')" "[11,12]|120.5|1.5|49000|"
eq "legacy unavailable history" "$(get "$LIVE" events.5.fee_source.reason events.5.entry_atr.reason events.5.entry_atr.raw_value events.5.stop_loss_oid.raw_value events.5.stop_loss_atr_mult.reason events.5.tp_tiers_json.reason events.5.trade_type.status | tr '\n' ' ')" "unstamped unstamped 0 0 unstamped unstamped unavailable "
eq "derived provenance" "$(get "$LIVE" events.2.row_net_pnl.provenance.0.kind events.2.row_net_pnl.provenance.0.source_field events.2.row_net_pnl.raw_value | tr '\n' '|')" "derived|tradeNetPnL(pnl_gross, realized_pnl, exchange_fee)|null|"
eq "wallet context" "$(get "$LIVE" wallet_orphan_context.status wallet_orphan_context.ownership wallet_orphan_context.allocation wallet_orphan_context.records | tr '\n' ' ')" "available live_wallet unallocated array:2 "
eq "wallet records" "$(get "$LIVE" wallet_orphan_context.records.0.event_key wallet_orphan_context.records.1.event_key wallet_orphan_context.records.1.amount_usd wallet_orphan_context.records.1.time_ms wallet_orphan_context.records.1.account wallet_orphan_context.records.1.process_strategy_id | tr '\n' ' ')" "primary/wallet_transfers/3 primary/wallet_transfers/2000 0.125 1767229200000 0xabc null "
ok "live export: complete selected ledger with WAL-only rows, raw fees and gross flags, both accounting helpers, explicit unavailable history, unallocated funding and separate wallet context"

ledger "$SNAP/capture.json" live hl-live-eth "$EXPORTS/many.json" || fail "multi-page export refused"
eq "multi-page count" "$(get "$EXPORTS/many.json" events)" "array:1203"
"$FX" events "$EXPORTS/many.json" source_row_id >"$WORK/many.ids"
eq "multi-page ids" "$(sort -n "$WORK/many.ids" | uniq | wc -l | tr -d ' ')" "1203"
cmp -s "$WORK/many.ids" <(seq 10003 3 13609) || fail "multi-page rows are missing, duplicated or out of storage order"
ok "keyset paging across 3 pages neither omits nor duplicates rows and keeps storage order"

ledger "$SNAP/capture.json" live hl-manual-eth "$EXPORTS/manual.json" || fail "manual export refused"
eq "manual events" "$("$FX" events "$EXPORTS/manual.json" event_key | tr '\n' ' ')" "primary/trades/6 primary/trades/7 "
eq "ambiguous evidence" "$(get "$EXPORTS/manual.json" events.1.close_reason.reason events.0.manual.value | tr '\n' ' ')" "ambiguous_evidence true "
ok "Hyperliquid manual owner exports; duplicate diagnostics stay ambiguous"

ledger "$SNAP/capture.json" paper hl-paper-btc "$EXPORTS/paper.json" || fail "default paper export refused"
eq "paper events" "$("$FX" events "$EXPORTS/paper.json" event_key | tr '\n' ' ')" "paper/trades/2 paper/trades/3 paper/trades/900 "
eq "paper wallet" "$(get "$EXPORTS/paper.json" wallet_orphan_context.status wallet_orphan_context.records | tr '\n' ' ')" "not_applicable array:0 "
ok "default-paper export reads only the paper file"

ledger "$SNAP/capture.json" paper:alpha hl-src-btc "$EXPORTS/alpha.json" || fail "named paper export refused"
eq "alpha selection" "$(get "$EXPORTS/alpha.json" selection.process_strategy_id selection.storage_strategy_id selection.source_role | tr '\n' ' ')" "hl-src-btc hl-paper-btc paper:alpha "
eq "alpha events" "$("$FX" events "$EXPORTS/alpha.json" event_key storage_strategy_id process_strategy_id | tr '\n' ' ')" "paper:alpha/trades/2|hl-paper-btc|hl-src-btc paper:alpha/trades/77|hl-paper-btc|hl-src-btc "
ok "named paper source export: identical storage ids in two files stay with their own owner"

expect_export_refused "wrong partition" "$SNAP/capture.json" paper hl-live-btc "$EXPORTS/r1.json" "belongs to partition live"
expect_export_refused "named source under default paper" "$SNAP/capture.json" paper hl-src-btc "$EXPORTS/r2.json" "belongs to partition paper:alpha"
expect_export_refused "unsupported owner" "$SNAP/capture.json" paper momentum-btc "$EXPORTS/r3.json" "supports Hyperliquid perps"
expect_export_refused "unknown strategy" "$SNAP/capture.json" live hl-nope "$EXPORTS/r4.json" "not in the captured configuration"
expect_export_refused "missing stored strategy row" "$SNAP/capture.json" live hl-live-sol "$EXPORTS/r5.json" "stores 0 strategy rows"
expect_export_refused "unknown paper source" "$SNAP/capture.json" paper:zeta hl-src-btc "$EXPORTS/r6.json" "belongs to partition"
for args in \
    "--manifest $SNAP/capture.json --partition live --strategy hl-live-btc --strategy hl-live-eth --output $EXPORTS/a.json" \
    "--manifest $SNAP/capture.json --partition live --strategy hl-live-btc --output $EXPORTS/a.json extra" \
    "--manifest $SNAP/capture.json --partition live --all --output $EXPORTS/a.json" \
    "--manifest $SNAP/capture.json --partition live --strategy  --output $EXPORTS/a.json" \
    "--manifest $SNAP/capture.json --partition live --output $EXPORTS/a.json" \
    "--manifest $SNAP/capture.json --config $SPLIT/cfg/config.json --partition live --strategy hl-live-btc --output $EXPORTS/a.json" \
    "--manifest $SNAP/capture.json --partition demo --strategy hl-live-btc --output $EXPORTS/a.json"; do
    rc=0
    # shellcheck disable=SC2086
    gt export ledger $args || rc=$?
    eq "argument refusal ($args)" "$rc" "2"
    [[ ! -e "$EXPORTS/a.json" ]] || fail "argument refusal created output"
done
ok "duplicate, extra, empty, missing, --all, --config and unknown-partition arguments exit 2 before output"

echo "existing" >"$EXPORTS/exists.json"
rc=0; ledger "$SNAP/capture.json" live hl-live-btc "$EXPORTS/exists.json" || rc=$?
eq "existing output" "$rc" "1"
eq "existing output untouched" "$(cat "$EXPORTS/exists.json")" "existing"
ln -s "$SNAP/state/primary.db" "$EXPORTS/symlink.json"
rc=0; ledger "$SNAP/capture.json" live hl-live-btc "$EXPORTS/symlink.json" || rc=$?
eq "symlink output alias" "$rc" "1"
echo "elsewhere" >"$WORK/hardlink-target.txt"
ln "$WORK/hardlink-target.txt" "$EXPORTS/hardlink.json" 2>/dev/null && {
    rc=0; ledger "$SNAP/capture.json" live hl-live-btc "$EXPORTS/hardlink.json" || rc=$?
    eq "hard-link output alias" "$rc" "1"
    rm -f "$EXPORTS/hardlink.json"
}
rc=0; ledger "$SNAP/capture.json" live hl-live-btc "$SNAP/out.json" || rc=$?
eq "output inside snapshot" "$rc" "1"
ln -s "$SNAP" "$WORK/snaplink"
rc=0; ledger "$SNAP/capture.json" live hl-live-btc "$WORK/snaplink/out.json" || rc=$?
eq "output through a symlinked snapshot parent" "$rc" "1"
rc=0; ledger "$SNAP/capture.json" live hl-live-btc "$SPLIT/state/out.json" || rc=$?
eq "output inside a recorded source directory" "$rc" "1"
[[ ! -e "$SPLIT/state/out.json" ]] || fail "export wrote into a source directory"
rc=0; ledger "$SNAP/capture.json" live hl-live-btc "$SPLIT/cfg/out.json" || rc=$?
eq "output inside the source configuration directory" "$rc" "1"
rc=0; ledger "$SNAP/capture.json" live hl-live-btc "$EXPORTS/x.db-wal" || rc=$?
eq "reserved sidecar output name" "$rc" "1"
rm -f "$EXPORTS/symlink.json"
ok "existing, symlink and hard-link outputs, outputs in snapshot or source directories and sidecar names are refused"

"$FX" fingerprint "$WORK/snap.after.json" "$SNAP"
cmp -s "$WORK/snap.before.json" "$WORK/snap.after.json" || { diff "$WORK/snap.before.json" "$WORK/snap.after.json" | head -20 >&2; fail "exports or refusals changed the snapshot set"; }
ok "snapshot, configuration copy and manifest stay byte-identical with the same inventory across every export and refusal"

ledger "$SNAP/capture.json" live hl-live-btc "$EXPORTS/live.again.json" || fail "repeat export refused"
cmp -s "$LIVE" "$EXPORTS/live.again.json" || fail "repeated export is not byte-identical"
mkdir -p "$WORK/elsewhere/deeper"
cp -a "$SNAP" "$WORK/elsewhere/deeper/moved-snapshot"
ledger "$WORK/elsewhere/deeper/moved-snapshot/capture.json" live hl-live-btc "$EXPORTS/live.moved.json" || fail "relocated export refused"
cmp -s "$LIVE" "$EXPORTS/live.moved.json" || fail "relocated export differs"
ok "repeated and relocated exports are byte-identical and keep the same event keys"

# --------------------------------------------------- tampered snapshot copies
tamper() {
    local name=$1
    rm -rf "$WORK/tamper"
    cp -a "$SNAP" "$WORK/tamper"
    echo "$WORK/tamper"
}

T=$(tamper wal)
"$FX" exec "$T/state/paper.db" "PRAGMA journal_mode=WAL"
"$FX" rehash "$T/capture.json"
expect_export_refused "WAL-mode snapshot with matching hashes" "$T/capture.json" live hl-live-btc "$EXPORTS/t1.json" "not in rollback-journal mode"

T=$(tamper emptywal)
: >"$T/state/primary.db-wal"
expect_export_refused "empty -wal sidecar" "$T/capture.json" live hl-live-btc "$EXPORTS/t2.json" "unexpected entries"
T=$(tamper journal)
printf 'x' >"$T/state/primary.db-journal"
expect_export_refused "nonempty journal" "$T/capture.json" live hl-live-btc "$EXPORTS/t3.json" "unexpected entries"
T=$(tamper shm)
: >"$T/state/paper-source-alpha.db-shm"
expect_export_refused "empty -shm sidecar" "$T/capture.json" live hl-live-btc "$EXPORTS/t4.json" "unexpected entries"
T=$(tamper extra)
echo extra >"$T/notes.txt"
expect_export_refused "extra snapshot file" "$T/capture.json" live hl-live-btc "$EXPORTS/t5.json" "unexpected entries"
T=$(tamper hash)
"$FX" exec "$T/state/primary.db" "UPDATE trades SET realized_pnl = 301 WHERE rowid = 1001"
expect_export_refused "changed snapshot bytes" "$T/capture.json" live hl-live-btc "$EXPORTS/t6.json" "hash does not match"
T=$(tamper cfg)
printf ' ' >>"$T/config.json"
expect_export_refused "changed configuration copy" "$T/capture.json" live hl-live-btc "$EXPORTS/t7.json" "configuration copy .* hash does not match"
T=$(tamper escape)
"$FX" json-set "$T/capture.json" files.1.relative_path '"../paper.db"'
expect_export_refused "manifest path escape" "$T/capture.json" live hl-live-btc "$EXPORTS/t8.json" "not a clean relative path"
T=$(tamper live)
"$FX" json-set "$T/config.json" db_file "\"$SPLIT/state/primary.db\""
"$FX" rehash "$T/capture.json"
expect_export_refused "config copy pointing at live files" "$T/capture.json" live hl-live-btc "$EXPORTS/t9.json" "manifest maps primary"
T=$(tamper missing)
rm -f "$T/state/paper-source-alpha.db"
expect_export_refused "missing snapshot file" "$T/capture.json" live hl-live-btc "$EXPORTS/t10.json" "paper-source-alpha.db"
T=$(tamper hardlink)
ln "$T/state/paper.db" "$T/paper-alias.db"
expect_export_refused "hard-linked snapshot input" "$T/capture.json" live hl-live-btc "$EXPORTS/t11.json" "unexpected entries|hard links"
T=$(tamper version)
"$FX" json-set "$T/capture.json" manifest_version 2
expect_export_refused "unsupported manifest version" "$T/capture.json" live hl-live-btc "$EXPORTS/t12.json" "unsupported capture manifest version"
T=$(tamper owner)
"$FX" exec "$T/state/paper.db" "INSERT INTO strategies (id, type, platform) VALUES ('hl-live-btc','perps','hyperliquid')" "PRAGMA journal_mode=DELETE"
"$FX" rehash "$T/capture.json"
expect_export_refused "ownership-inspection rejection" "$T/capture.json" live hl-live-btc "$EXPORTS/t13.json" "ownership inspection rejected"
if [[ "$(id -u)" != "0" ]]; then
    T=$(tamper perm)
    chmod 000 "$T/state/primary.db"
    expect_export_refused "unreadable snapshot file" "$T/capture.json" live hl-live-btc "$EXPORTS/t14.json" "permission denied"
    chmod 600 "$T/state/primary.db"
else
    echo "NOTE: running as root; the permission-denied refusal is not exercised (root bypasses file modes)"
fi
expect_export_refused "broken manifest path" "$WORK/nowhere/capture.json" live hl-live-btc "$EXPORTS/t15.json" "no such file"
ok "tampered, aliased, escaped, sidecar-bearing, WAL-mode and owner-rejected snapshot sets are refused with no output"

# --------------------------------------------------- malformed rows and schemas
BAD="$WORK/bad"
BAD_EXTRA=""
for id in hl-bad-ts hl-empty-ts hl-bad-bool hl-bad-json hl-bad-num hl-inf; do
    BAD_EXTRA="$BAD_EXTRA
    {\"id\": \"$id\", \"type\": \"perps\", \"platform\": \"hyperliquid\", \"script\": \"shared_scripts/check_hyperliquid.py\", \"args\": [\"sma_crossover\", \"BTC\", \"1h\", \"--mode=live\"], \"capital\": 1000},"
done
make_config "$BAD" unsplit 19 "$BAD_EXTRA"
create_schema "$BAD"
"$FX" exec "$BAD/state/primary.db" \
    "INSERT INTO strategies (id, type, platform) VALUES ('hl-bad-ts','perps','hyperliquid'), ('hl-empty-ts','perps','hyperliquid'), ('hl-bad-bool','perps','hyperliquid'), ('hl-bad-json','perps','hyperliquid'), ('hl-bad-num','perps','hyperliquid'), ('hl-inf','perps','hyperliquid'), ('hl-live-btc','perps','hyperliquid')" \
    "INSERT INTO trades ($T_COLS) VALUES (1,'hl-bad-ts','2026-13-40 25:00','BTC','p','buy',1,1,1,'perps','','',0,0,0,0,'','',0,NULL,0,0,'','',0)" \
    "INSERT INTO trades ($T_COLS) VALUES (2,'hl-empty-ts','','BTC','p','buy',1,1,1,'perps','','',0,0,0,0,'','',0,NULL,0,0,'','',0)" \
    "INSERT INTO trades ($T_COLS) VALUES (3,'hl-bad-bool','2026-01-01T00:00:00Z','BTC','p','buy',1,1,1,'perps','','',0,2,0,0,'','',0,NULL,0,0,'','',0)" \
    "INSERT INTO trades ($T_COLS) VALUES (4,'hl-bad-json','2026-01-01T00:00:00Z','BTC','p','buy',1,1,1,'perps','','',0,0,0,0,'','',0,NULL,0,0,'[1,','',0)" \
    "INSERT INTO trades ($T_COLS) VALUES (5,'hl-bad-num','2026-01-01T00:00:00Z','BTC','p','buy','abc',1,1,'perps','','',0,0,0,0,'','',0,NULL,0,0,'','',0)" \
    "INSERT INTO trades ($T_COLS) VALUES (6,'hl-inf','2026-01-01T00:00:00Z','BTC','p','buy',1e999,1,1,'perps','','',0,0,0,0,'','',0,NULL,0,0,'','',0)" \
    "INSERT INTO trades ($T_COLS) VALUES (7,'hl-live-btc','2026-01-01T00:00:00Z','BTC','p','buy',1,1,1,'perps','','',0,0,0,0,'','',0,NULL,0,0,'','',0)"
start_writer "$BAD/state/primary.db"
capture "$BAD/cfg/config.json" "$WORK/snapshots/bad" || fail "capture of malformed-row fixture refused"
stop_writer close
B="$WORK/snapshots/bad/capture.json"
"$FX" fingerprint "$WORK/bad.before.json" "$WORK/snapshots/bad"
expect_export_refused "invalid timestamp" "$B" live hl-bad-ts "$EXPORTS/b1.json" "invalid timestamp"
expect_export_refused "empty timestamp" "$B" live hl-empty-ts "$EXPORTS/b2.json" "invalid timestamp .*empty timestamp"
expect_export_refused "malformed Boolean" "$B" live hl-bad-bool "$EXPORTS/b3.json" "malformed Boolean"
expect_export_refused "malformed geometry" "$B" live hl-bad-json "$EXPORTS/b4.json" "corrupt JSON geometry"
expect_export_refused "text in a numeric column" "$B" live hl-bad-num "$EXPORTS/b5.json" "want a number"
expect_export_refused "non-finite amount" "$B" live hl-inf "$EXPORTS/b6.json" "non-finite"
"$FX" fingerprint "$WORK/bad.after.json" "$WORK/snapshots/bad"
cmp -s "$WORK/bad.before.json" "$WORK/bad.after.json" || fail "malformed-row refusals changed the snapshot"
ok "invalid and empty timestamps, malformed Booleans, corrupt geometry, text amounts and non-finite amounts fail the whole export"

LEG="$WORK/legacy"
make_config "$LEG" unsplit
create_schema "$LEG"
"$FX" exec "$LEG/state/primary.db" \
    "INSERT INTO strategies (id, type, platform) VALUES ('hl-live-btc','perps','hyperliquid'), ('hl-paper-btc','perps','hyperliquid')" \
    "INSERT INTO trades ($T_COLS) VALUES (11,'hl-live-btc','2026-01-01T00:00:00Z','BTC','p','buy',1,1,1,'perps','','',0,0,0,0,'','',0,NULL,0,0,'','',0)" \
    "ALTER TABLE trades DROP COLUMN fee_source" \
    "ALTER TABLE trades DROP COLUMN pnl_gross"
start_writer "$LEG/state/primary.db"
capture "$LEG/cfg/config.json" "$WORK/snapshots/legacy" || fail "capture of the legacy-schema fixture refused"
stop_writer close
expect_export_refused "migration-required accounting schema" "$WORK/snapshots/legacy/capture.json" live hl-live-btc "$EXPORTS/l1.json" "lacks mandatory accounting column\(s\) pnl_gross"

NOT="$WORK/notrades"
make_config "$NOT" unsplit
create_schema "$NOT"
"$FX" exec "$NOT/state/primary.db" "INSERT INTO strategies (id, type, platform) VALUES ('hl-live-btc','perps','hyperliquid')" "DROP TABLE trades"
start_writer "$NOT/state/primary.db"
capture "$NOT/cfg/config.json" "$WORK/snapshots/notrades" || fail "capture of the no-trades fixture refused"
stop_writer close
expect_export_refused "unreadable schema (no trades table)" "$WORK/snapshots/notrades/capture.json" live hl-live-btc "$EXPORTS/n1.json" "has no trades table"
ok "schemas that need migration or lack the ledger table are refused without migration"

# ---------------------------------------------------------- unsplit layout
UN="$WORK/unsplit"
make_config "$UN" unsplit
create_schema "$UN"
"$FX" exec "$UN/state/primary.db" \
    "INSERT INTO strategies (id, type, platform) VALUES ('hl-live-btc','perps','hyperliquid'), ('hl-paper-btc','perps','hyperliquid')" \
    "INSERT INTO trades ($T_COLS) VALUES (3,'hl-live-btc','2026-01-01T00:00:00Z','BTC','u-1','buy',1,10,10,'perps','','',0.25,0,0,0,'userfills','',0,NULL,0,0,'','',0)" \
    "INSERT INTO trades ($T_COLS) VALUES (8,'hl-paper-btc','2026-01-01T00:00:00Z','BTC','u-2','buy',1,10,10,'perps','','',0.25,0,0,0,'modeled','',0,NULL,0,0,'','',0)"
start_writer "$UN/state/primary.db"
writer_cmd "exec 0 INSERT INTO trades ($T_COLS) VALUES (12,'hl-paper-btc','2026-01-01T01:00:00Z','BTC','u-2','sell',1,11,11,'perps','','',0.25,1,0.75,0,'modeled','',0,NULL,0,0,'','',0)"
capture "$UN/cfg/config.json" "$WORK/snapshots/unsplit" || fail "unsplit capture refused"
stop_writer close
U="$WORK/snapshots/unsplit/capture.json"
ledger "$U" live hl-live-btc "$EXPORTS/u-live.json" || fail "unsplit live export refused"
ledger "$U" paper hl-paper-btc "$EXPORTS/u-paper.json" || fail "unsplit paper export refused"
eq "unsplit live" "$("$FX" events "$EXPORTS/u-live.json" event_key | tr '\n' ' ')" "primary/trades/3 "
eq "unsplit paper" "$("$FX" events "$EXPORTS/u-paper.json" event_key | tr '\n' ' ')" "primary/trades/8 primary/trades/12 "
expect_export_refused "unsplit missing stored row" "$U" live hl-live-eth "$EXPORTS/u3.json" "stores 0 strategy rows"
ok "unsplit layout: ownership comes from the identity map, not the primary fallback"

# ----------------------------------------------------- interrupted publication
seen_absent=0
seen_complete=0
for delay in 0 0.005 0.01 0.02 0.04 0.08 0.15 0.3; do
    out="$EXPORTS/int-$delay.json"
    "${CLEAN_ENV[@]}" "$GO_TRADER_BIN" export ledger --manifest "$SNAP/capture.json" --partition live --strategy hl-live-eth --output "$out" >/dev/null 2>&1 &
    pid=$!
    sleep "$delay"
    kill -9 "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
    if [[ -e "$out" ]]; then
        cmp -s "$out" "$EXPORTS/many.json" || fail "an interrupted export published a partial or different file ($out)"
        seen_complete=$((seen_complete + 1))
    else
        seen_absent=$((seen_absent + 1))
    fi
done
for f in "$EXPORTS"/.*.staging; do
    [[ -e "$f" ]] && rm -f "$f"
done
ok "interrupted exports publish either nothing or the complete file ($seen_absent absent, $seen_complete complete)"

"$FX" fingerprint "$WORK/snap.final.json" "$SNAP"
cmp -s "$WORK/snap.before.json" "$WORK/snap.final.json" || { diff "$WORK/snap.before.json" "$WORK/snap.final.json" | head -20 >&2; fail "the snapshot set changed during the suite"; }
echo "PASS: ledger capture and export (active-WAL capture without source effects, version 1 export contract, every refusal leaves inputs unchanged)"
