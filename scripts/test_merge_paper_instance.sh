#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
source "${SCRIPT_DIR}/update_helpers.sh"
MERGE="${SCRIPT_DIR}/merge-paper-instance.sh"

: "${GO_TRADER_BIN:?set GO_TRADER_BIN to a built go-trader binary}"
: "${MERGE_PAPER_FIXTURE_DIR:?set MERGE_PAPER_FIXTURE_DIR to a directory holding live.db and paper.db}"
UNIT_TEMPLATE="${MERGE_PAPER_UNIT_TEMPLATE:-${SCRIPT_DIR}/../systemd/go-trader@.service}"

T=$(mktemp -d "${TMPDIR:-/tmp}/merge-paper-test.XXXXXX")
trap 'rm -rf "$T"' EXIT

fail() {
    echo "FAIL: $*" >&2
    exit 1
}

assert_eq() {
    local got="$1" want="$2" msg="$3"
    [[ "$got" == "$want" ]] || fail "$msg (got=$got want=$want)"
}

assert_rc() {
    local got="$1" want="$2" msg="$3"
    [[ "$got" == "$want" ]] || { echo "$out" >&2; fail "$msg (got=$got want=$want)"; }
}

assert_contains() {
    local text="$1" needle="$2" msg="$3"
    [[ "$text" == *"$needle"* ]] || { echo "$text" >&2; fail "$msg (missing '$needle')"; }
}

json_get() {
    python3 -c '
import json, sys
doc = json.load(open(sys.argv[1]))
for part in sys.argv[2].split("."):
    doc = doc[int(part)] if isinstance(doc, list) else doc.get(part)
    if doc is None:
        print("")
        sys.exit(0)
print(json.dumps(doc) if isinstance(doc, (dict, list)) else doc)
' "$1" "$2"
}

live_cfg_json() {
    local db="$1"
    cat <<JSON
{
  "config_version": 19,
  "interval_seconds": 300,
  "db_file": "$db",
  "replay_log_path": "$T/shared/replay.db",
  "market_feed": "rest",
  "portfolio_risk": {"max_drawdown_pct": 25, "daily_max_loss_usd": 500},
  "discord": {"enabled": false, "token": "", "channels": {"hyperliquid": "C-live"}},
  "strategies": [
    {"id": "hl-x", "type": "perps", "platform": "hyperliquid",
     "script": "shared_scripts/check_hyperliquid.py",
     "args": ["vwap", "ETH", "1h", "--mode=live"],
     "capital": 100, "leverage": 5, "margin_per_trade_usd": 50, "replay_sharing": "live_mirror"}
  ]
}
JSON
}

paper_cfg_json() {
    local db="$1" risk="${2:-{\"max_drawdown_pct\": 50, \"daily_max_loss_usd\": 500\}}"
    cat <<JSON
{
  "config_version": 19,
  "interval_seconds": 600,
  "db_file": "$db",
  "replay_log_path": "$T/shared/replay.db",
  "status_port": 8098,
  "log_dir": "paper-logs",
  "portfolio_risk": $risk,
  "discord": {"enabled": false, "token": "", "channels": {"hyperliquid": "C-paper"}},
  "strategies": [
    {"id": "hl-x", "type": "perps", "platform": "hyperliquid",
     "script": "shared_scripts/check_hyperliquid.py",
     "args": ["vwap", "ETH", "1h", "--mode=paper"],
     "capital": 100, "leverage": 5, "margin_per_trade_usd": 50, "replay_sharing": "live_mirror"}
  ]
}
JSON
}

setup() {
    local name="$1" paper_db_mode="${2:-base}"
    F="$T/$name"
    BASE="$F/var/lib/go-trader"
    OPT="$F/opt"
    UNITS="$F/units"
    mkdir -p "$BASE/live" "$BASE/paper" "$OPT/go-trader-live/scheduler" "$OPT/go-trader-paper/scheduler" "$UNITS" "$F/bin" "$T/shared"
    cp "$GO_TRADER_BIN" "$OPT/go-trader-live/go-trader"
    cp "$GO_TRADER_BIN" "$OPT/go-trader-paper/go-trader"
    cp "$UNIT_TEMPLATE" "$UNITS/go-trader@.service"
    printf 'HYPERLIQUID_SECRET_KEY=fixture\n' > "$OPT/go-trader-live/.env"
    printf 'HYPERLIQUID_SECRET_KEY=fixture\n' > "$OPT/go-trader-paper/.env"
    cp "$MERGE_PAPER_FIXTURE_DIR/live.db" "$BASE/live/state.db"
    LIVE_DB="$BASE/live/state.db"
    if [[ "$paper_db_mode" == "tree" ]]; then
        cp "$MERGE_PAPER_FIXTURE_DIR/paper.db" "$OPT/go-trader-paper/scheduler/state.db"
        PAPER_DB="$OPT/go-trader-paper/scheduler/state.db"
        PAPER_DB_CFG="scheduler/state.db"
    else
        cp "$MERGE_PAPER_FIXTURE_DIR/paper.db" "$BASE/paper/state.db"
        PAPER_DB="$BASE/paper/state.db"
        PAPER_DB_CFG="$PAPER_DB"
    fi
    live_cfg_json "$LIVE_DB" > "$BASE/live/config.json"
    paper_cfg_json "$PAPER_DB_CFG" > "$BASE/paper/config.json"
    printf 'inactive\n' > "$F/live.state"
    printf 'inactive\n' > "$F/paper.state"
    cat > "$F/bin/systemctl" <<EOS
#!/usr/bin/env bash
case "\$1" in
    is-active)
        case "\$2" in
            go-trader@live.service) cat "$F/live.state" ;;
            go-trader@paper.service) cat "$F/paper.state" ;;
            *) echo inactive ;;
        esac
        ;;
esac
EOS
    chmod +x "$F/bin/systemctl"
    LIVE_CFG="$BASE/live/config.json"
    PAPER_CFG="$BASE/paper/config.json"
    DROPIN="$UNITS/go-trader@live.service.d/50-merge-paper-paper.conf"
    JOURNAL="$BASE/live/merge-paper-paper.journal"
    EXPECT_DROPIN=$'[Service]\nReadWritePaths='"$(update_canonical_db_path "$(dirname "$PAPER_DB")")"
}

run_merge() {
    MERGE_PAPER_SYSTEMCTL="$F/bin/systemctl" MERGE_PAPER_SYSTEMD_ANALYZE="/nonexistent/systemd-analyze" \
        bash "$MERGE" --live live --paper paper --base "$BASE" --deploy-root "$OPT" --unit-dir "$UNITS" "$@"
}

fingerprints() {
    printf '%s|%s' "$(update_db_fingerprint "$LIVE_DB" | tr '\n' ' ')" "$(update_db_fingerprint "$PAPER_DB" | tr '\n' ' ')"
}

hold_lock_in_background() {
    local path="$1"
    python3 -c '
import fcntl, os, sys, time
fd = os.open(sys.argv[1], os.O_CREAT | os.O_RDWR, 0o644)
fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
os.write(fd, b"4242\n")
print("held", flush=True)
time.sleep(60)
' "$path" > "$T/holder.out" &
    HOLD_PID=$!
    for _ in $(seq 1 100); do
        [[ -s "$T/holder.out" ]] && break
        sleep 0.05
    done
}

echo "== preflight refusals"
setup preflight
rm -rf "$OPT/go-trader-paper"
out=$(run_merge 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "10" "missing paper deployment exits 10"
setup preflight2
printf '#!/usr/bin/env bash\necho other-version\n' > "$OPT/go-trader-paper/go-trader"
chmod +x "$OPT/go-trader-paper/go-trader"
out=$(run_merge 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "11" "binary version mismatch exits 11"
setup preflight3
rm -f "$PAPER_CFG"
out=$(run_merge 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "13" "missing paper config exits 13"
setup preflight4
printf 'active\n' > "$F/live.state"
out=$(run_merge 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "14" "active live unit exits 14"
assert_contains "$out" "go-trader@live.service is active" "active unit named"
setup preflight5
python3 - "$PAPER_CFG" <<'PY'
import json, sys
p = sys.argv[1]
cfg = json.load(open(p))
cfg["strategies"][0]["args"][-1] = "--mode=live"
json.dump(cfg, open(p, "w"))
PY
out=$(run_merge 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "15" "paper config with a live strategy exits 15"
setup preflight6
python3 - "$PAPER_CFG" "$LIVE_DB" <<'PY'
import json, sys
p = sys.argv[1]
cfg = json.load(open(p))
cfg["db_file"] = sys.argv[2]
json.dump(cfg, open(p, "w"))
PY
out=$(run_merge 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "16" "paper db_file resolving to the live db exits 16"
setup preflight7
python3 - "$LIVE_CFG" <<'PY'
import json, sys
p = sys.argv[1]
cfg = json.load(open(p))
cfg["paper_db_file"] = "/somewhere/else.db"
json.dump(cfg, open(p, "w"))
PY
out=$(run_merge 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "17" "live paper_db_file naming another file exits 17"

echo "== lock contention"
setup contention
hold_lock_in_background "$(update_canonical_db_path "$PAPER_DB").lock"
out=$(run_merge 2>&1) && rc=0 || rc=$?
kill "$HOLD_PID" 2>/dev/null || true
wait "$HOLD_PID" 2>/dev/null || true
assert_rc "$rc" "3" "held ownership lock exits 3"
assert_contains "$out" "pid=4242" "contention names the holder pid"
setup contention2
hold_lock_in_background "$(update_canonical_db_path "$PAPER_DB").manual-action.lock"
out=$(run_merge 2>&1) && rc=0 || rc=$?
kill "$HOLD_PID" 2>/dev/null || true
wait "$HOLD_PID" 2>/dev/null || true
assert_rc "$rc" "3" "held manual-action lock exits 3"
assert_contains "$out" "manual-action.lock" "manual-action contention names the lock"

echo "== dry run"
setup dry
before=$(fingerprints)
out=$(run_merge 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "dry run exits 0 (rc=$rc)"; }
assert_eq "$(fingerprints)" "$before" "dry run leaves both databases and wals byte-identical"
assert_contains "$out" "VERDICT: READY" "dry run verdict"
assert_contains "$out" "1 strategies mapped, 0 orphan, 1 positions, 1 pending actions, latch=true" "paper file summary"
assert_contains "$out" "rename hl-x -> hl-x-paper (storage_strategy_id=hl-x)" "collision rename"
assert_contains "$out" "hl-x -> hl-x-paper (1 position(s))" "staged paper book keeps its position under the alias"
assert_contains "$out" "stamp hl-x-paper interval_seconds=600" "paper root cadence stamped"
assert_contains "$out" "ReadWritePaths=$(update_canonical_db_path "$BASE/paper")" "writable directive names the paper db directory"
assert_contains "$out" "retire the paper instance's status port (8098)" "status port retirement note"
assert_contains "$out" "dropped paper root key log_dir" "dropped keys listed"
staged="$LIVE_CFG.merge-staged"
[[ -f "$staged" ]] || fail "staged config written"
assert_eq "$(json_get "$staged" strategies.1.id)" "hl-x-paper" "staged paper id"
assert_eq "$(json_get "$staged" strategies.1.storage_strategy_id)" "hl-x" "staged storage alias"
assert_eq "$(json_get "$staged" strategies.1.replay_source_id)" "hl-x" "staged replay source"
assert_eq "$(json_get "$staged" strategies.1.interval_seconds)" "600" "staged interval"
assert_eq "$(json_get "$staged" paper_db_file)" "$(update_canonical_db_path "$PAPER_DB")" "staged paper_db_file"
assert_eq "$(json_get "$staged" portfolio_risk.paper.max_drawdown_pct)" "50" "paper risk override"
assert_eq "$(json_get "$staged" discord.channels.hyperliquid-paper)" "C-paper" "paper channel key added"
assert_eq "$(json_get "$staged" discord.channels.hyperliquid)" "C-live" "live channel kept"
[[ "$(json_get "$staged" status_port)" == "" ]] || fail "paper status_port must not reach the merged config"
[[ ! -e "$DROPIN" ]] || fail "dry run must not install the drop-in"
assert_eq "$(cat "$LIVE_CFG")" "$(live_cfg_json "$LIVE_DB")" "dry run leaves the live config untouched"
perm=$(python3 -c 'import os,sys; print(oct(os.stat(sys.argv[1]).st_mode & 0o777))' "$staged")
assert_eq "$perm" "0o600" "staged config mode 0600"

echo "== dry run with a deploy-tree paper database"
setup tree tree
out=$(run_merge 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "deploy-tree dry run exits 0 (rc=$rc)"; }
assert_contains "$out" "ReadWritePaths=$(update_canonical_db_path "$OPT/go-trader-paper/scheduler")" "ReadWritePaths directive for a deploy-tree db"

echo "== compose refusals"
setup zero
paper_cfg_json "$PAPER_DB" '{"max_drawdown_pct": 50}' > "$PAPER_CFG"
before=$(fingerprints)
out=$(run_merge 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "21" "zero paper override refuses before writes"
assert_contains "$out" "portfolio_risk.paper.daily_max_loss_usd would be zero" "zero override names the field"
[[ ! -e "$LIVE_CFG.merge-staged" ]] || fail "refused compose leaves no staged config"
assert_eq "$(fingerprints)" "$before" "refused compose leaves databases untouched"
setup regime
python3 - "$PAPER_CFG" <<'PY'
import json, sys
p = sys.argv[1]
cfg = json.load(open(p))
cfg["regime"] = {"enabled": True, "period": 14, "adx_threshold": 25}
json.dump(cfg, open(p, "w"))
PY
out=$(run_merge 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "21" "differing regime block refuses"
assert_contains "$out" "root key regime differs" "regime refusal names the key"
setup channel
python3 - "$LIVE_CFG" <<'PY'
import json, sys
p = sys.argv[1]
cfg = json.load(open(p))
cfg["discord"]["channels"]["hyperliquid-paper"] = "C-other"
json.dump(cfg, open(p, "w"))
PY
out=$(run_merge 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "21" "conflicting -paper channel refuses"
assert_contains "$out" "discord.channels.hyperliquid-paper" "channel refusal names the key"

echo "== second suffix, with and without a live id collision"
setup suffix
python3 - "$LIVE_CFG" <<'PY'
import json, sys
p = sys.argv[1]
cfg = json.load(open(p))
del cfg["strategies"][0]["capital"]
for sid in ("hl-x-paper", "hl-y-paper"):
    cfg["strategies"].append({"id": sid, "type": "perps", "platform": "hyperliquid",
        "script": "shared_scripts/check_hyperliquid.py", "args": ["vwap", "BTC", "1h", "--mode=live"],
        "leverage": 5, "margin_per_trade_usd": 50})
json.dump(cfg, open(p, "w"))
PY
python3 - "$PAPER_CFG" <<'PY'
import json, sys
p = sys.argv[1]
cfg = json.load(open(p))
cfg["strategies"].append({"id": "hl-y", "type": "perps", "platform": "hyperliquid",
    "script": "shared_scripts/check_hyperliquid.py", "args": ["vwap", "BTC", "1h", "--mode=paper"],
    "capital": 100, "leverage": 5, "margin_per_trade_usd": 50})
json.dump(cfg, open(p, "w"))
PY
out=$(run_merge 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "second suffix dry run exits 0 (rc=$rc)"; }
assert_contains "$out" "rename hl-x -> hl-x-paper2 (storage_strategy_id=hl-x)" "a colliding paper id takes the second suffix when the first is used"
assert_contains "$out" "rename hl-y -> hl-y-paper2 (storage_strategy_id=hl-y)" "a paper id with no live collision takes the second suffix too"

echo "== a paper id that already carries the alias keeps it"
setup aliased
python3 - "$PAPER_CFG" <<'PY'
import json, sys
p = sys.argv[1]
cfg = json.load(open(p))
s = cfg["strategies"][0]
s["id"] = "hl-x-paper"
s["storage_strategy_id"] = "hl-x"
s.pop("replay_sharing", None)
json.dump(cfg, open(p, "w"))
PY
out=$(run_merge 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "an already aliased paper id reaches READY (rc=$rc)"; }
assert_contains "$out" "keep hl-x-paper (already carries the -paper alias; storage_strategy_id=hl-x)" "an already aliased id is not renamed again"
assert_eq "$(json_get "$LIVE_CFG.merge-staged" strategies.1.id)" "hl-x-paper" "the already aliased id is staged as it is"

echo "== invalid explicit storage mapping on the paper side"
setup badmap
python3 - "$PAPER_CFG" <<'PY'
import json, sys
p = sys.argv[1]
cfg = json.load(open(p))
cfg["strategies"][0]["storage_strategy_id"] = "ghost"
json.dump(cfg, open(p, "w"))
PY
out=$(run_merge 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "20" "an explicit storage id that orphans the stored book refuses at inspection"
assert_contains "$out" "orphan book(s): hl-x" "orphan named"

echo "== apply"
setup apply
before=$(fingerprints)
orig_cfg=$(cat "$LIVE_CFG")
out=$(run_merge --apply 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "apply exits 0 (rc=$rc)"; }
assert_contains "$out" "VERDICT: APPLIED" "apply verdict"
assert_eq "$(fingerprints)" "$before" "apply leaves both databases byte-identical"
assert_eq "$(json_get "$LIVE_CFG" strategies.1.storage_strategy_id)" "hl-x" "installed config carries the alias"
assert_eq "$(cat "$DROPIN")" "$EXPECT_DROPIN" "installed drop-in"
assert_eq "$(cat "$LIVE_CFG.pre-merge-paper")" "$orig_cfg" "previous config retained"
grep -qx complete "$JOURNAL" || fail "journal marked complete"
[[ ! -e "$LIVE_CFG.merge-staged" ]] || fail "staged config consumed by apply"
installed_cfg=$(cat "$LIVE_CFG")
out=$(run_merge --apply 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "0" "repeated apply is a no-op success"
assert_contains "$out" "nothing to do" "repeated apply reports no-op"
assert_eq "$(cat "$LIVE_CFG")" "$installed_cfg" "repeated apply changes nothing"
assert_eq "$(fingerprints)" "$before" "repeated apply leaves databases untouched"
rm -f "$JOURNAL"
out=$(run_merge 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "dry run over a merged live config exits 0 (rc=$rc)"; }
assert_contains "$out" "hl-x already merged; skipped" "repeat run skips the merged strategy"
assert_contains "$out" "2 existing + 0 added strategies" "repeat run appends nothing"
assert_eq "$(json_get "$LIVE_CFG.merge-staged" strategies.1.id)" "hl-x-paper" "repeat run keeps one paper copy"
[[ "$(json_get "$LIVE_CFG.merge-staged" strategies.2.id)" == "" ]] || fail "repeat run must not append a duplicate"

echo "== rollback"
setup rollback
before=$(fingerprints)
orig_cfg=$(cat "$LIVE_CFG")
out=$(run_merge --apply 2>&1) || { echo "$out" >&2; fail "apply before rollback"; }
out=$(run_merge --rollback 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "rollback exits 0 (rc=$rc)"; }
assert_eq "$(cat "$LIVE_CFG")" "$orig_cfg" "rollback restores the previous config"
[[ ! -e "$DROPIN" ]] || fail "rollback removes a drop-in that was absent before"
grep -qx rolled-back "$JOURNAL" || fail "journal marked rolled-back"
assert_eq "$(fingerprints)" "$before" "rollback leaves databases untouched"

echo "== rollback with a pre-existing drop-in"
setup rollback2
mkdir -p "$(dirname "$DROPIN")"
printf '[Service]\nNice=5\n' > "$DROPIN"
out=$(run_merge --apply 2>&1) || { echo "$out" >&2; fail "apply with prior drop-in"; }
assert_eq "$(cat "$DROPIN")" "$EXPECT_DROPIN" "override replaced"
out=$(run_merge --rollback 2>&1) || { echo "$out" >&2; fail "rollback with prior drop-in"; }
assert_eq "$(cat "$DROPIN")" $'[Service]\nNice=5' "rollback restores the prior drop-in"

echo "== interruption after the config step"
setup interrupt
before=$(fingerprints)
orig_cfg=$(cat "$LIVE_CFG")
out=$(MERGE_PAPER_FAIL_AFTER=config run_merge --apply 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "4" "interrupted apply exits 4"
assert_eq "$(cat "$LIVE_CFG")" "$orig_cfg" "interrupted apply restores the config"
[[ ! -e "$DROPIN" ]] || fail "interrupted apply leaves no drop-in"
grep -qx rolled-back "$JOURNAL" || fail "interrupted journal marked rolled-back"
assert_eq "$(fingerprints)" "$before" "interrupted apply leaves databases untouched"
out=$(run_merge --apply 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "retry after interruption exits 0 (rc=$rc)"; }
assert_eq "$(json_get "$LIVE_CFG" strategies.1.id)" "hl-x-paper" "retry installs the merged config"
assert_eq "$(cat "$DROPIN")" "$EXPECT_DROPIN" "retry installs the drop-in"

echo "== interruption after the override step"
setup interrupt2
orig_cfg=$(cat "$LIVE_CFG")
out=$(MERGE_PAPER_FAIL_AFTER=override run_merge --apply 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "4" "interrupted override exits 4"
assert_eq "$(cat "$LIVE_CFG")" "$orig_cfg" "config restored after an override-step interruption"
[[ ! -e "$DROPIN" ]] || fail "drop-in removed after an override-step interruption"

echo "== failed override installation"
setup unwritable
orig_cfg=$(cat "$LIVE_CFG")
chmod 555 "$UNITS"
out=$(run_merge --apply 2>&1) && rc=0 || rc=$?
chmod 755 "$UNITS"
assert_rc "$rc" "4" "unwritable unit dir exits 4"
assert_eq "$(cat "$LIVE_CFG")" "$orig_cfg" "config restored after a failed override install"
[[ ! -e "$DROPIN" ]] || fail "no drop-in after a failed override install"
grep -qx rolled-back "$JOURNAL" || fail "failed override journal marked rolled-back"

echo "== resume an interrupted journal"
setup resume
orig_cfg=$(cat "$LIVE_CFG")
out=$(run_merge 2>&1) || { echo "$out" >&2; fail "dry run before resume"; }
cp "$LIVE_CFG" "$LIVE_CFG.pre-merge-paper"
cp "$LIVE_CFG.merge-staged" "$LIVE_CFG"
printf 'run_id x\noverride_prior absent\nconfig begin\nconfig done\n' > "$JOURNAL"
out=$(run_merge --apply 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "resume then apply exits 0 (rc=$rc)"; }
assert_contains "$out" "records an interrupted apply; restoring" "resume restores first"
assert_eq "$(json_get "$LIVE_CFG" strategies.1.id)" "hl-x-paper" "resume completes the merge"
ls "$BASE/live"/merge-paper-paper.journal.rolled-back.* >/dev/null 2>&1 || fail "interrupted journal archived"

echo "== source config changed before apply"
setup changed
python3 - "$LIVE_CFG" <<'PY'
import json, sys
p = sys.argv[1]
cfg = json.load(open(p))
cfg["portfolio_risk"]["paper"] = {"max_drawdown_pct": 10}
json.dump(cfg, open(p, "w"))
PY
out=$(run_merge --apply 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "21" "existing different portfolio_risk.paper refuses"

echo "== live config without a portfolio_risk block"
setup norisk
python3 - "$LIVE_CFG" <<'PY'
import json, sys
p = sys.argv[1]
cfg = json.load(open(p))
del cfg["portfolio_risk"]
json.dump(cfg, open(p, "w"))
PY
out=$(run_merge 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "live config without portfolio_risk reaches READY (rc=$rc)"; }
assert_contains "$out" "portfolio_risk root materialized from the effective live view" "materialized root reported"
staged="$LIVE_CFG.merge-staged"
assert_eq "$(json_get "$staged" portfolio_risk.max_drawdown_pct)" "25" "the loader's default live drawdown limit stays explicit in the merged root"
assert_eq "$(json_get "$staged" portfolio_risk.warn_threshold_pct)" "60" "the loader's default warn threshold stays explicit in the merged root"
assert_eq "$(json_get "$staged" portfolio_risk.paper.max_drawdown_pct)" "50" "paper override kept when live had no block"
assert_eq "$(json_get "$staged" portfolio_risk.paper.daily_max_loss_usd)" "500" "paper daily loss limit kept when live had no block"

echo "== live config with an empty portfolio_risk block"
setup emptyrisk
python3 - "$LIVE_CFG" <<'PY'
import json, sys
p = sys.argv[1]
cfg = json.load(open(p))
cfg["portfolio_risk"] = {}
json.dump(cfg, open(p, "w"))
PY
out=$(run_merge 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "20" "a live config the binary refuses to load exits under the inspection code, never as an incompatible binary"
assert_contains "$out" "portfolio_risk.max_drawdown_pct must be in (0, 100]" "the loader error is shown"

echo "== paper config without a portfolio_risk block"
setup paperdefault
python3 - "$PAPER_CFG" <<'PY'
import json, sys
p = sys.argv[1]
cfg = json.load(open(p))
del cfg["portfolio_risk"]
json.dump(cfg, open(p, "w"))
PY
out=$(run_merge 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "21" "a paper deployment with no daily loss limit cannot inherit the live one"
assert_contains "$out" "portfolio_risk.paper.daily_max_loss_usd would be zero" "effective paper value drives the zero-inherits refusal"
python3 - "$LIVE_CFG" <<'PY'
import json, sys
p = sys.argv[1]
cfg = json.load(open(p))
cfg["portfolio_risk"] = {"max_drawdown_pct": 25}
json.dump(cfg, open(p, "w"))
PY
out=$(run_merge 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "paper defaults equal to the live limits reach READY (rc=$rc)"; }
[[ "$(json_get "$LIVE_CFG.merge-staged" portfolio_risk.paper)" == "" ]] || fail "equal effective limits need no paper override"

echo "== preflight passes a rejected layout through"
setup rejected
python3 - "$PAPER_CFG" "$BASE/paper/other.db" <<'PY'
import json, sys
p = sys.argv[1]
cfg = json.load(open(p))
cfg["paper_db_file"] = sys.argv[2]
json.dump(cfg, open(p, "w"))
PY
HYPERLIQUID_SECRET_KEY=fixture "$GO_TRADER_BIN" storage-inspect --json --config "$PAPER_CFG" >"$T/rejected.json" 2>/dev/null && fail "rejected precondition: storage-inspect must exit nonzero"
[[ "$(json_get "$T/rejected.json" rejections.0)" == *"state file"* ]] || fail "rejected precondition: the binary reports a rejection with its JSON"
out=$(run_merge 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "16" "a layout the binary rejects with a full report passes the binary probe and reaches the later refusals, never exit 12"

echo "== dry run reports the journal state"
setup journal
out=$(run_merge 2>&1) || { echo "$out" >&2; fail "dry run before journal cases"; }
cp "$LIVE_CFG" "$LIVE_CFG.pre-merge-paper"
cp "$LIVE_CFG.merge-staged" "$LIVE_CFG"
printf 'run_id x\noverride_prior absent\nconfig begin\nconfig done\n' > "$JOURNAL"
out=$(run_merge 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "24" "a dry run over an interrupted journal refuses"
assert_contains "$out" "records an interrupted apply" "dry run names the interrupted state"
assert_eq "$(cat "$LIVE_CFG")" "$(cat "$LIVE_CFG.merge-staged")" "a dry run never restores"
setup journal2
out=$(run_merge --apply 2>&1) || { echo "$out" >&2; fail "apply before complete-journal dry run"; }
out=$(run_merge 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "dry run over a complete journal exits 0 (rc=$rc)"; }
assert_contains "$out" "is complete and every deployment file matches its result" "dry run reports the complete journal"
assert_contains "$out" "VERDICT: READY" "complete journal dry run still certifies"
python3 - "$LIVE_CFG" <<'PY'
import json, sys
p = sys.argv[1]
cfg = json.load(open(p))
cfg["interval_seconds"] = 301
json.dump(cfg, open(p, "w"))
PY
out=$(run_merge 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "24" "a dry run over a complete journal whose files changed refuses like apply"
setup journal3
out=$(MERGE_PAPER_FAIL_AFTER=config run_merge --apply 2>&1) || true
grep -qx rolled-back "$JOURNAL" || fail "journal3 precondition: rolled-back journal"
out=$(run_merge 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "dry run over a rolled-back journal exits 0 (rc=$rc)"; }
assert_contains "$out" "records a rolled-back run" "dry run reports the rolled-back journal"
[[ -f "$JOURNAL" ]] || fail "a dry run never archives the journal"

echo "== rollback keeps a copy of a hand-edited merged config"
setup edited
orig_cfg=$(cat "$LIVE_CFG")
out=$(run_merge --apply 2>&1) || { echo "$out" >&2; fail "apply before edited rollback"; }
python3 - "$LIVE_CFG" <<'PY'
import json, sys
p = sys.argv[1]
cfg = json.load(open(p))
cfg["interval_seconds"] = 302
json.dump(cfg, open(p, "w"))
PY
edited_cfg=$(cat "$LIVE_CFG")
printf '[Service]\nNice=7\n' > "$DROPIN"
out=$(run_merge --rollback 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "rollback after a hand edit exits 0 (rc=$rc)"; }
assert_eq "$(cat "$LIVE_CFG")" "$orig_cfg" "rollback restores the previous config"
archive=$(ls "$LIVE_CFG".merge-edited.* 2>/dev/null | head -n 1)
[[ -n "$archive" ]] || fail "rollback keeps a copy of the edited config"
assert_eq "$(cat "$archive")" "$edited_cfg" "the archived copy holds the hand edit"
assert_contains "$out" "copy kept at $archive" "rollback names the archived copy"
dropin_archive=$(ls "$DROPIN".merge-edited.* 2>/dev/null | head -n 1)
[[ -n "$dropin_archive" ]] || fail "rollback keeps a copy of the edited drop-in"
assert_eq "$(cat "$dropin_archive")" $'[Service]\nNice=7' "the archived drop-in holds the hand edit"
[[ ! -e "$DROPIN" ]] || fail "rollback removes the drop-in that was absent before"
setup clean
out=$(run_merge --apply 2>&1) || { echo "$out" >&2; fail "apply before clean rollback"; }
out=$(run_merge --rollback 2>&1) || { echo "$out" >&2; fail "clean rollback"; }
[[ -z "$(ls "$LIVE_CFG".merge-edited.* 2>/dev/null)" ]] || fail "a clean rollback archives nothing"

echo "== configs below the current version are inspected without a rewrite"
setup oldcfg
python3 - "$LIVE_CFG" "$PAPER_CFG" <<'PY'
import json, sys
for p in sys.argv[1:]:
    cfg = json.load(open(p))
    cfg["config_version"] = 15
    json.dump(cfg, open(p, "w"))
PY
live_before=$(cat "$LIVE_CFG"); paper_before=$(cat "$PAPER_CFG")
out=$(run_merge 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "dry run over v15 configs exits 0 (rc=$rc)"; }
assert_eq "$(cat "$LIVE_CFG")" "$live_before" "a dry run never rewrites the live config (pending migration stays on disk)"
assert_eq "$(cat "$PAPER_CFG")" "$paper_before" "a dry run never rewrites the paper config"
[[ ! -e "$LIVE_CFG.tmp" && ! -e "$PAPER_CFG.tmp" ]] || fail "no migration temp file may appear beside a deployment config"
assert_eq "$(json_get "$LIVE_CFG.merge-staged" config_version)" "15" "the staged config keeps the live config version for the daemon to migrate as the service user"
setup mixedcfg
python3 - "$PAPER_CFG" <<'PY'
import json, sys
p = sys.argv[1]
cfg = json.load(open(p))
cfg["config_version"] = 15
json.dump(cfg, open(p, "w"))
PY
out=$(run_merge 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "18" "configs at different versions refuse before any lock or inspection"
assert_contains "$out" "config_version differs" "version mismatch named"

echo "== a root key set only in the live config is compared too"
setup liveonly
python3 - "$LIVE_CFG" <<'PY'
import json, sys
p = sys.argv[1]
cfg = json.load(open(p))
cfg["market_feed"] = "websocket"
json.dump(cfg, open(p, "w"))
PY
out=$(run_merge 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "21" "live-only market_feed=websocket refuses instead of moving the paper strategies onto the sealed feed"
assert_contains "$out" "root key market_feed differs" "the live-only key is named"
setup liveonly2
python3 - "$LIVE_CFG" <<'PY'
import json, sys
p = sys.argv[1]
cfg = json.load(open(p))
cfg["telegram"] = {"enabled": True, "token": "t", "channels": {"hyperliquid": "T-live"}}
json.dump(cfg, open(p, "w"))
PY
out=$(run_merge 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "21" "a live-only telegram block refuses"
assert_contains "$out" "root key telegram differs" "the live-only telegram key is named"

echo "== a paper strategy with no stored book passes"
setup nobook
python3 - "$PAPER_CFG" <<'PY'
import json, sys
p = sys.argv[1]
cfg = json.load(open(p))
cfg["strategies"].append({"id": "hl-new", "type": "perps", "platform": "hyperliquid",
    "script": "shared_scripts/check_hyperliquid.py", "args": ["vwap", "BTC", "1h", "--mode=paper"],
    "capital": 100, "leverage": 5, "margin_per_trade_usd": 50})
json.dump(cfg, open(p, "w"))
PY
out=$(run_merge 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "a configured paper strategy without a book reaches READY (rc=$rc)"; }
assert_contains "$out" "1 configured strategy without a stored book yet" "the bookless strategy is reported"
assert_contains "$out" "rename hl-new -> hl-new-paper (storage_strategy_id=hl-new)" "a paper strategy with no live counterpart is aliased too"
assert_eq "$(json_get "$LIVE_CFG.merge-staged" strategies.2.id)" "hl-new-paper" "the bookless strategy is moved under its alias"
assert_eq "$(json_get "$LIVE_CFG.merge-staged" strategies.2.storage_strategy_id)" "hl-new" "the alias keeps the bare stored id"

echo "== one-shot root-key conflict report"
setup manykeys
python3 - "$PAPER_CFG" <<'PY'
import json, sys
p = sys.argv[1]
cfg = json.load(open(p))
cfg["regime"] = {"enabled": True}
cfg["telegram"] = {"enabled": True, "token": "paper-token"}
cfg["notify_ratchet_triggers"] = True
cfg["channels"] = {"ops": "paper-ops"}
json.dump(cfg, open(p, "w"))
PY
paper_before=$(cat "$PAPER_CFG")
out=$(run_merge 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "21" "N root-key conflicts refuse once"
assert_contains "$out" "root key regime differs" "regime is listed"
assert_contains "$out" "root key telegram differs" "telegram is listed in the same refuse"
assert_contains "$out" "root key notify_ratchet_triggers differs" "notify_ratchet_triggers is listed in the same refuse"
assert_contains "$out" "root key channels differs" "unknown root key channels is listed"
assert_contains "$out" "dropped paper root key log_dir (live value kept)" "dropped log_dir is listed while refusing"
assert_contains "$out" "dropped paper root key status_port (live value kept)" "dropped status_port is listed while refusing"
[[ ! -e "$LIVE_CFG.merge-staged" ]] || fail "one-shot refuse leaves no staged config"
assert_eq "$(cat "$PAPER_CFG")" "$paper_before" "a refused compose never edits the paper config"

echo "== --diff is a static preview"
setup diffpreview
python3 - "$PAPER_CFG" <<'PY'
import json, sys
p = sys.argv[1]
cfg = json.load(open(p))
cfg["regime"] = {"enabled": True}
cfg["telegram"] = {"enabled": True, "token": "paper-token"}
cfg["channels"] = {"ops": "paper-ops"}
json.dump(cfg, open(p, "w"))
PY
printf 'active\n' > "$F/live.state"
rm -f "$OPT/go-trader-paper/go-trader"
hold_lock_in_background "$(update_canonical_db_path "$PAPER_DB").lock"
out=$(run_merge --diff 2>&1) && rc=0 || rc=$?
kill "$HOLD_PID" 2>/dev/null || true
wait "$HOLD_PID" 2>/dev/null || true
assert_rc "$rc" "0" "--diff exits 0 while units run, a binary is missing, and a lock is held"
assert_contains "$out" "diff: refuse-on-difference regime" "--diff classifies regime"
assert_contains "$out" "diff: refuse-on-difference telegram" "--diff classifies telegram"
assert_contains "$out" "diff: unknown channels" "--diff classifies an unknown root key"
assert_contains "$out" "diff: dropped log_dir" "--diff classifies dropped log_dir"
assert_contains "$out" "(live value kept)" "--diff marks dropped keys as live-value-kept"
assert_contains "$out" "inspect-based portfolio_risk refuses need a dry run" "--diff does not claim a clean merge"
assert_contains "$out" "diff: alias hl-x -> hl-x-paper (storage_strategy_id=hl-x)" "--diff previews the alias the apply step would create"
assert_contains "$out" "diff: channel-plan discord.channels.hyperliquid-paper=C-paper" "--diff names the -paper channel key the compose step would add"
[[ ! -e "$LIVE_CFG.merge-staged" ]] || fail "--diff must not write a staged config"
[[ ! -e "$JOURNAL" ]] || fail "--diff must not write a journal"

echo "== --diff names compose refuses it can see without inspect"
setup diffreplay
python3 - "$PAPER_CFG" <<'PY'
import json, sys
p = sys.argv[1]
cfg = json.load(open(p))
cfg["replay_log_path"] = "/tmp/other-replay.db"
json.dump(cfg, open(p, "w"))
PY
out=$(run_merge --diff 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "0" "--diff with a paper-mirror replay_log_path clash exits 0"
assert_contains "$out" "diff: compose-refuse replay_log_path" "--diff names a paper-mirror replay_log_path clash"
setup diffchannel
python3 - "$LIVE_CFG" <<'PY'
import json, sys
p = sys.argv[1]
cfg = json.load(open(p))
cfg["discord"]["channels"]["hyperliquid-paper"] = "C-other"
json.dump(cfg, open(p, "w"))
PY
out=$(run_merge --diff 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "0" "--diff with a -paper channel clash exits 0"
assert_contains "$out" "diff: compose-refuse discord.channels.hyperliquid-paper" "--diff names a discord -paper clash"

echo "== --diff previews every discord channel key the merge would add"
setup diffchannelplan
python3 - "$PAPER_CFG" <<'PY'
import json, sys
p = sys.argv[1]
cfg = json.load(open(p))
cfg["discord"]["trade_alert_channels"] = {"hyperliquid": "T-paper"}
cfg["discord"]["dm_channels"] = {"hyperliquid": "D-paper"}
json.dump(cfg, open(p, "w"))
PY
out=$(run_merge --diff 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "0" "--diff with all three paper channel maps exits 0"
assert_contains "$out" "diff: channel-plan discord.channels.hyperliquid-paper=C-paper" "--diff names the channels key it would add"
assert_contains "$out" "diff: channel-plan discord.trade_alert_channels.hyperliquid-paper=T-paper" "--diff names the trade_alert_channels key it would add"
assert_contains "$out" "diff: channel-plan discord.dm_channels.hyperliquid-paper=D-paper" "--diff names the dm_channels key it would add"
[[ ! -e "$LIVE_CFG.merge-staged" ]] || fail "--diff must not write a staged config while previewing the channel plan"

echo "== a paper channel value equal to live adds no -paper key"
setup samechannel
python3 - "$PAPER_CFG" <<'PY'
import json, sys
p = sys.argv[1]
cfg = json.load(open(p))
cfg["discord"]["channels"]["hyperliquid"] = "C-live"
json.dump(cfg, open(p, "w"))
PY
out=$(run_merge --diff 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "0" "--diff with an identical paper channel exits 0"
assert_contains "$out" "diff: channel-plan discord.channels.hyperliquid-paper not added" "--diff names the channel key it would skip"
out=$(run_merge 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "0" "dry run with an identical paper channel exits 0"
assert_contains "$out" "compose: discord.channels.hyperliquid-paper not added" "compose names the same skip --diff previewed"
staged="$LIVE_CFG.merge-staged"
assert_eq "$(json_get "$staged" discord.channels.hyperliquid)" "C-live" "the merged map keeps the single bare channel key"
assert_eq "$(json_get "$staged" discord.channels.hyperliquid-paper)" "" "no -paper key is added when the paper value repeats the live value"
assert_eq "$(json_get "$staged" discord.trade_alert_channels)" "" "no empty trade_alert_channels map is written"
assert_eq "$(json_get "$staged" discord.dm_channels)" "" "no empty dm_channels map is written"

echo "== an identical paper channel skips its -paper key when a live-only channel exists"
setup scopeisolation
python3 - "$LIVE_CFG" "$PAPER_CFG" <<'PY'
import json, sys
live_p, paper_p = sys.argv[1], sys.argv[2]
live = json.load(open(live_p))
paper = json.load(open(paper_p))
live["discord"]["channels"]["okx"] = "C-okx-live"
paper["discord"]["channels"]["hyperliquid"] = "C-live"
paper["discord"]["dm_channels"] = {"hyperliquid-paper": "D-live"}
live["discord"]["dm_channels"] = {"hyperliquid": "D-live", "okx": "D-okx-live"}
json.dump(live, open(live_p, "w"))
json.dump(paper, open(paper_p, "w"))
PY
out=$(run_merge --diff 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "0" "--diff with a live-only second channel exits 0"
assert_contains "$out" "diff: channel-plan discord.channels.hyperliquid-paper not added" "--diff says the redundant channels key is skipped"
assert_contains "$out" "diff: channel-plan discord.dm_channels.hyperliquid-paper=D-live" "the dm map has no bare-key fallback, so its key is always added"
out=$(run_merge 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "0" "dry run with a live-only second channel exits 0"
staged="$LIVE_CFG.merge-staged"
assert_eq "$(json_get "$staged" discord.channels.hyperliquid)" "C-live" "the merged map keeps the single bare channel key"
assert_eq "$(json_get "$staged" discord.channels.hyperliquid-paper)" "" "no -paper channel key is added when the paper value already routes through the bare key"
assert_eq "$(json_get "$staged" discord.channels.okx)" "C-okx-live" "the live-only channel is untouched"
assert_eq "$(json_get "$staged" discord.dm_channels.hyperliquid-paper)" "D-live" "the dm -paper key survives, because tradeAlertRoutes never falls back off it for a paper strategy"

echo "== a paper dm map with only a bare key refuses at proof"
setup dmbarekey
python3 - "$LIVE_CFG" "$PAPER_CFG" <<'PY'
import json, sys
live_p, paper_p = sys.argv[1], sys.argv[2]
live = json.load(open(live_p))
paper = json.load(open(paper_p))
paper["discord"]["channels"]["hyperliquid"] = "C-live"
paper["discord"]["dm_channels"] = {"hyperliquid": "D-live"}
live["discord"]["dm_channels"] = {"hyperliquid": "D-live"}
json.dump(live, open(live_p, "w"))
json.dump(paper, open(paper_p, "w"))
PY
out=$(run_merge --diff 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "0" "--diff with a bare paper dm key exits 0"
assert_contains "$out" "diff: channel-plan discord.dm_channels.hyperliquid-paper=D-live" "compose still promotes the bare dm key into -paper"
out=$(run_merge 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "22" "dry run with a bare paper dm key refuses at proof"
assert_contains "$out" "notification.dm_channel" "proof names the dm channel that compose would add"
staged="$LIVE_CFG.merge-staged"
assert_eq "$(json_get "$staged" discord.dm_channels.hyperliquid-paper)" "D-live" "compose still wrote the -paper dm key before proof refused"

echo "== a type-keyed dm map still gets its -paper key"
setup dmtypekey
python3 - "$LIVE_CFG" "$PAPER_CFG" <<'PY'
import json, sys
live_p, paper_p = sys.argv[1], sys.argv[2]
live = json.load(open(live_p))
paper = json.load(open(paper_p))
paper["discord"]["channels"]["hyperliquid"] = "C-live"
live["discord"]["dm_channels"] = {"perps": "D-perps"}
paper["discord"]["dm_channels"] = {"perps": "D-perps"}
json.dump(live, open(live_p, "w"))
json.dump(paper, open(paper_p, "w"))
PY
out=$(run_merge 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "22" "dry run with a type-keyed dm map refuses at proof"
assert_contains "$out" "notification.dm_channel" "proof names the dm channel compose would add from the type key"
staged="$LIVE_CFG.merge-staged"
assert_eq "$(json_get "$staged" discord.dm_channels.hyperliquid-paper)" "D-perps" "a dm value resolved from the bare type key still writes the -paper key"
assert_eq "$(json_get "$staged" discord.channels.hyperliquid-paper)" "" "the channels map still skips its redundant key"

echo "== --diff names a same-platform type-keyed discord self-conflict"
setup difftypes
python3 - "$PAPER_CFG" <<'PY'
import json, sys
p = sys.argv[1]
cfg = json.load(open(p))
cfg["strategies"].append({
    "id": "hl-opt", "type": "options", "platform": "hyperliquid",
    "script": "shared_scripts/check_hyperliquid.py",
    "args": ["vwap", "ETH", "1h", "--mode=paper"],
    "capital": 100, "leverage": 5, "margin_per_trade_usd": 50,
})
cfg["discord"]["channels"] = {"perps": "Ch-perps", "options": "Ch-opt"}
cfg["discord"]["trade_alert_channels"] = {"perps": "T-perps", "options": "T-opt"}
cfg["discord"]["dm_channels"] = {"perps": "D-perps", "options": "D-opt"}
json.dump(cfg, open(p, "w"))
PY
out=$(run_merge --diff 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "0" "--diff with two types on one platform exits 0"
assert_contains "$out" "diff: compose-refuse discord.channels.hyperliquid-paper" "--diff names a channels type-keyed self-conflict"
assert_contains "$out" "diff: compose-refuse discord.trade_alert_channels.hyperliquid-paper" "--diff names a trade_alert_channels type-keyed self-conflict"
assert_contains "$out" "diff: compose-refuse discord.dm_channels.hyperliquid-paper" "--diff names a dm_channels type-keyed self-conflict"

echo "== --diff skips already-merged paper strategies for discord used"
setup diffmerged
python3 - "$LIVE_CFG" "$PAPER_CFG" "$PAPER_DB" <<'PY'
import json, os, sys
live_p, paper_p, paper_db = sys.argv[1], sys.argv[2], sys.argv[3]
live = json.load(open(live_p))
paper = json.load(open(paper_p))
moved = json.loads(json.dumps(paper["strategies"][0]))
moved["id"] = "hl-x-paper"
moved["storage_strategy_id"] = "hl-x"
live["strategies"].append(moved)
live["paper_db_file"] = os.path.realpath(paper_db) if os.path.exists(paper_db) else os.path.abspath(paper_db)
paper["strategies"].append({
    "id": "hl-opt", "type": "options", "platform": "hyperliquid",
    "script": "shared_scripts/check_hyperliquid.py",
    "args": ["vwap", "ETH", "1h", "--mode=paper"],
    "capital": 100, "leverage": 5, "margin_per_trade_usd": 50,
})
paper["discord"]["channels"] = {"perps": "Ch-perps", "options": "Ch-opt"}
paper["discord"]["trade_alert_channels"] = {"perps": "T-perps", "options": "T-opt"}
paper["discord"]["dm_channels"] = {"perps": "D-perps", "options": "D-opt"}
json.dump(live, open(live_p, "w"))
json.dump(paper, open(paper_p, "w"))
PY
out=$(run_merge --diff 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "0" "--diff with an already-merged type plus a new type exits 0"
case "$out" in
    *"diff: compose-refuse discord.channels.hyperliquid-paper"*|*"diff: compose-refuse discord.trade_alert_channels.hyperliquid-paper"*|*"diff: compose-refuse discord.dm_channels.hyperliquid-paper"*)
        echo "$out" >&2
        fail "--diff must not preview a type-keyed discord self-conflict when one type is already merged"
        ;;
esac

echo "== --diff replay_log_path follows compose any_mirror"
setup diffreplayoff
python3 - "$LIVE_CFG" "$PAPER_CFG" "$PAPER_DB" <<'PY'
import json, os, sys
live_p, paper_p, paper_db = sys.argv[1], sys.argv[2], sys.argv[3]
live = json.load(open(live_p))
paper = json.load(open(paper_p))
moved = json.loads(json.dumps(paper["strategies"][0]))
moved["id"] = "hl-x-paper"
moved["storage_strategy_id"] = "hl-x"
moved.pop("replay_sharing", None)
live["strategies"].append(moved)
live["paper_db_file"] = os.path.realpath(paper_db) if os.path.exists(paper_db) else os.path.abspath(paper_db)
paper["replay_log_path"] = "/tmp/other-replay.db"
json.dump(live, open(live_p, "w"))
json.dump(paper, open(paper_p, "w"))
PY
out=$(run_merge --diff 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "0" "--diff with a detached live-side mirror exits 0"
case "$out" in
    *"diff: compose-refuse replay_log_path"*)
        echo "$out" >&2
        fail "--diff must not preview replay_log_path when the merged config has no live mirror"
        ;;
esac
setup diffreplayon
python3 - "$LIVE_CFG" "$PAPER_CFG" "$PAPER_DB" <<'PY'
import json, os, sys
live_p, paper_p, paper_db = sys.argv[1], sys.argv[2], sys.argv[3]
live = json.load(open(live_p))
paper = json.load(open(paper_p))
moved = json.loads(json.dumps(paper["strategies"][0]))
moved["id"] = "hl-x-paper"
moved["storage_strategy_id"] = "hl-x"
live["strategies"].append(moved)
live["paper_db_file"] = os.path.realpath(paper_db) if os.path.exists(paper_db) else os.path.abspath(paper_db)
paper["replay_log_path"] = "/tmp/other-replay.db"
json.dump(live, open(live_p, "w"))
json.dump(paper, open(paper_p, "w"))
PY
out=$(run_merge --diff 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "0" "--diff with a still-active already-merged mirror exits 0"
assert_contains "$out" "diff: compose-refuse replay_log_path" "--diff still names replay_log_path when an already-merged mirror is active"

echo "== --align-to-live is opt-in"
setup alignflag
out=$(run_merge --align-to-live 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "2" "--align-to-live without --diff or --apply is usage"
setup aligndiff
python3 - "$PAPER_CFG" <<'PY'
import json, sys
p = sys.argv[1]
cfg = json.load(open(p))
cfg["regime"] = {"enabled": True}
cfg["telegram"] = {"enabled": True, "token": "paper-token"}
json.dump(cfg, open(p, "w"))
PY
paper_before=$(cat "$PAPER_CFG")
printf 'active\n' > "$F/paper.state"
out=$(run_merge --diff --align-to-live 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "0" "--diff --align-to-live exits 0 while a unit is active"
assert_eq "$(cat "$PAPER_CFG")" "$paper_before" "--align-to-live never mutates the paper source"
[[ -f "$PAPER_CFG.aligned" ]] || fail "--diff --align-to-live writes the aligned paper config"
assert_eq "$(json_get "$PAPER_CFG.aligned" regime)" "" "aligned file drops a paper-only regime so it matches live"
assert_contains "$out" "align: regime before=" "align records the regime before-value"
assert_contains "$out" "align: telegram before=" "align records the telegram before-value"

echo "== --align-to-live with --apply"
setup alignapply
python3 - "$PAPER_CFG" <<'PY'
import json, sys
p = sys.argv[1]
cfg = json.load(open(p))
cfg["regime"] = {"enabled": True}
cfg["telegram"] = {"enabled": True, "token": "paper-token"}
json.dump(cfg, open(p, "w"))
PY
paper_before=$(cat "$PAPER_CFG")
out=$(run_merge --apply --align-to-live 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "--apply --align-to-live exits 0 (rc=$rc)"; }
assert_contains "$out" "VERDICT: APPLIED" "--apply --align-to-live reaches apply"
assert_eq "$(cat "$PAPER_CFG")" "$paper_before" "--apply --align-to-live never mutates the paper source"
[[ -f "$PAPER_CFG.aligned" ]] || fail "--apply --align-to-live preserves the aligned paper config"
assert_eq "$(json_get "$PAPER_CFG.aligned" regime)" "" "applied alignment drops paper-only regime"
grep -q '^align: regime before=' "$JOURNAL" || fail "journal records aligned regime with before-value"
grep -q '^align: telegram before=' "$JOURNAL" || fail "journal records aligned telegram with before-value"
assert_eq "$(json_get "$LIVE_CFG" strategies.1.id)" "hl-x-paper" "aligned apply still moves the paper strategy"

echo "== --apply --align-to-live proves an aligned stop default"
setup alignapplystop
python3 - "$PAPER_CFG" <<'PY'
import json, sys
p = sys.argv[1]
cfg = json.load(open(p))
cfg["default_stop_loss_atr_mult"] = 2.5
cfg["strategies"].append({
    "id": "hl-y", "type": "perps", "platform": "hyperliquid",
    "script": "shared_scripts/check_hyperliquid.py",
    "args": ["vwap", "BTC", "1h", "--mode=paper"],
    "capital": 100, "leverage": 5, "margin_per_trade_usd": 50,
    "stop_loss_atr_mult": 3.0,
})
json.dump(cfg, open(p, "w"))
PY
paper_before=$(cat "$PAPER_CFG")
out=$(run_merge --apply --align-to-live 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "--apply --align-to-live with a differing stop default exits 0 (rc=$rc)"; }
assert_contains "$out" "VERDICT: APPLIED" "--apply --align-to-live reaches apply when default_stop_loss_atr_mult is aligned"
assert_eq "$(cat "$PAPER_CFG")" "$paper_before" "--apply --align-to-live never mutates the paper source when aligning a stop default"
assert_eq "$(json_get "$PAPER_CFG.aligned" default_stop_loss_atr_mult)" "" "aligned file drops paper's default_stop_loss_atr_mult so it matches live"
assert_eq "$(json_get "$PAPER_CFG.aligned" strategies.1.stop_loss_atr_mult)" "3.0" "aligned file keeps an explicit strategy stop override"

source_cfg_json() {
    local db="$1" port="$2" channel="${3:-C-src}" risk="${4:-{\"max_drawdown_pct\": 50, \"daily_max_loss_usd\": 500\}}"
    cat <<JSON
{
  "config_version": 19,
  "interval_seconds": 600,
  "db_file": "$db",
  "replay_log_path": "$T/shared/replay.db",
  "status_port": $port,
  "portfolio_risk": $risk,
  "discord": {"enabled": false, "token": "", "channels": {"hyperliquid": "$channel"}},
  "strategies": [
    {"id": "hl-x", "type": "perps", "platform": "hyperliquid",
     "script": "shared_scripts/check_hyperliquid.py",
     "args": ["vwap", "ETH", "1h", "--mode=paper"],
     "capital": 100, "leverage": 5, "margin_per_trade_usd": 50}
  ]
}
JSON
}

add_source() {
    local instance="$1" port="$2" channel="${3:-C-src}" risk="${4:-}"
    mkdir -p "$BASE/$instance" "$OPT/go-trader-$instance/scheduler"
    cp "$GO_TRADER_BIN" "$OPT/go-trader-$instance/go-trader"
    printf 'HYPERLIQUID_SECRET_KEY=fixture\n' > "$OPT/go-trader-$instance/.env"
    cp "$MERGE_PAPER_FIXTURE_DIR/paper.db" "$BASE/$instance/state.db"
    if [[ -n "$risk" ]]; then
        source_cfg_json "$BASE/$instance/state.db" "$port" "$channel" "$risk" > "$BASE/$instance/config.json"
    else
        source_cfg_json "$BASE/$instance/state.db" "$port" "$channel" > "$BASE/$instance/config.json"
    fi
}

run_merge_args() {
    MERGE_PAPER_SYSTEMCTL="$F/bin/systemctl" MERGE_PAPER_SYSTEMD_ANALYZE="/nonexistent/systemd-analyze" \
        bash "$MERGE" --live live --base "$BASE" --deploy-root "$OPT" --unit-dir "$UNITS" "$@"
}

db_fingerprints() {
    local out="" d
    for d in "$@"; do
        out+="$(update_db_fingerprint "$d" | tr '\n' ' ')|"
    done
    printf '%s' "$out"
}

echo "== two paper sources fold into their own partitions"
setup sources
add_source coin-btc 8101 C-btc
add_source coin-eth 8102 C-eth
BTC_DB="$BASE/coin-btc/state.db"
ETH_DB="$BASE/coin-eth/state.db"
BTC_CANON=$(update_canonical_db_path "$BTC_DB")
ETH_CANON=$(update_canonical_db_path "$ETH_DB")
BTC_DROPIN="$UNITS/go-trader@live.service.d/50-merge-paper-btc.conf"
ETH_DROPIN="$UNITS/go-trader@live.service.d/50-merge-paper-eth.conf"
SRC_JOURNAL="$BASE/live/merge-paper-btc+eth.journal"
before=$(db_fingerprints "$LIVE_DB" "$BTC_DB" "$ETH_DB")
orig_cfg=$(cat "$LIVE_CFG")
out=$(run_merge_args --source btc=coin-btc --source eth=coin-eth 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "two-source dry run exits 0 (rc=$rc)"; }
assert_contains "$out" "VERDICT: READY" "two-source dry run verdict"
assert_eq "$(db_fingerprints "$LIVE_DB" "$BTC_DB" "$ETH_DB")" "$before" "two-source dry run leaves every database byte-identical"
assert_contains "$out" "rename hl-x -> hl-x-paper-btc (storage_strategy_id=hl-x)" "the btc source takes its own alias"
assert_contains "$out" "rename hl-x -> hl-x-paper-eth (storage_strategy_id=hl-x)" "the eth source takes its own alias"
assert_contains "$out" "stamp hl-x-paper-btc paper_source=btc (partition paper:btc)" "the moved block names its partition"
assert_contains "$out" "paper:btc file $BTC_CANON: 1 strategies mapped, 0 orphan, 1 positions" "the staged layout maps the btc book"
assert_contains "$out" "paper:eth file $ETH_CANON: 1 strategies mapped, 0 orphan, 1 positions" "the staged layout maps the eth book"
assert_contains "$out" "hl-x -> hl-x-paper-btc (1 position(s))" "the btc book keeps its position under the source alias"
assert_contains "$out" "proof: 3 strategies compared, no effective difference" "every moved strategy is proven"
assert_contains "$out" "retire the coin-btc instance's status port (8101)" "each folded instance gets a retirement note"
assert_contains "$out" "retire the coin-eth instance's status port (8102)" "the second folded instance gets one too"
live_canon=$(update_canonical_db_path "$LIVE_DB")
assert_contains "$out" "$live_canon.lock $live_canon.manual-action.lock $BTC_CANON.lock $BTC_CANON.manual-action.lock $ETH_CANON.lock $ETH_CANON.manual-action.lock" "the handoff locks the primary file first, then every source by id, in the runtime's order"
staged="$LIVE_CFG.merge-staged"
assert_eq "$(json_get "$staged" strategies.1.id)" "hl-x-paper-btc" "staged btc id"
assert_eq "$(json_get "$staged" strategies.1.paper_source)" "btc" "staged btc paper_source"
assert_eq "$(json_get "$staged" strategies.1.storage_strategy_id)" "hl-x" "staged btc storage alias"
assert_eq "$(json_get "$staged" strategies.2.id)" "hl-x-paper-eth" "staged eth id"
assert_eq "$(json_get "$staged" paper_sources.0.id)" "btc" "paper_sources is ordered by id"
assert_eq "$(json_get "$staged" paper_sources.0.db_file)" "$BTC_CANON" "the btc entry owns the btc database"
assert_eq "$(json_get "$staged" paper_sources.0.label)" "coin-btc" "the btc entry is labelled with its instance"
assert_eq "$(json_get "$staged" paper_sources.0.portfolio_risk.max_drawdown_pct)" "50" "the btc entry carries the source's own drawdown limit"
assert_eq "$(json_get "$staged" paper_sources.1.id)" "eth" "the eth entry is present"
assert_eq "$(json_get "$staged" paper_db_file)" "" "a source-only handoff never sets paper_db_file"
assert_eq "$(json_get "$staged" discord.channels.hyperliquid-paper:btc)" "C-btc" "the btc source gets its own channel key"
assert_eq "$(json_get "$staged" discord.channels.hyperliquid-paper:eth)" "C-eth" "the eth source gets its own channel key"
assert_eq "$(json_get "$staged" discord.channels.hyperliquid)" "C-live" "the live channel is kept"
[[ ! -e "$BTC_DROPIN" ]] || fail "a dry run must not install a source drop-in"

out=$(run_merge_args --source btc=coin-btc --source eth=coin-eth --apply 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "two-source apply exits 0 (rc=$rc)"; }
assert_contains "$out" "VERDICT: APPLIED" "two-source apply verdict"
assert_eq "$(db_fingerprints "$LIVE_DB" "$BTC_DB" "$ETH_DB")" "$before" "two-source apply leaves every database byte-identical"
assert_eq "$(cat "$BTC_DROPIN")" $'[Service]\nReadWritePaths='"$(update_canonical_db_path "$BASE/coin-btc")" "the btc drop-in makes its database directory writable"
assert_eq "$(cat "$ETH_DROPIN")" $'[Service]\nReadWritePaths='"$(update_canonical_db_path "$BASE/coin-eth")" "the eth drop-in makes its database directory writable"
assert_eq "$(json_get "$LIVE_CFG" paper_sources.0.id)" "btc" "the installed config declares the btc source"
assert_eq "$(cat "$LIVE_CFG.pre-merge-btc+eth")" "$orig_cfg" "the previous config is retained under the run key"
grep -qx complete "$SRC_JOURNAL" || fail "the two-source journal is marked complete"
grep -qx "folds btc eth" "$SRC_JOURNAL" || fail "the journal names every folded deployment"
grep -qx "source.btc coin-btc" "$SRC_JOURNAL" || fail "the journal names each source's instance"

installed_cfg=$(cat "$LIVE_CFG")
out=$(run_merge_args --source btc=coin-btc --source eth=coin-eth --apply 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "0" "a repeated two-source apply is a no-op success"
assert_contains "$out" "nothing to do" "the repeated apply reports no-op"
assert_eq "$(cat "$LIVE_CFG")" "$installed_cfg" "the repeated apply changes nothing"

out=$(run_merge_args --source btc=coin-btc --source eth=coin-eth 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "repeat two-source dry run exits 0 (rc=$rc)"; }
assert_contains "$out" "preflight: live config already declares paper source btc (repeat run)" "a repeat run recognises the declared source"
assert_contains "$out" "hl-x already merged; skipped" "a repeat run moves no book twice"
assert_contains "$out" "3 existing + 0 added strategies" "a repeat run appends nothing"

out=$(run_merge_args --source btc=coin-btc --source eth=coin-eth --rollback 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "two-source rollback exits 0 (rc=$rc)"; }
assert_eq "$(cat "$LIVE_CFG")" "$orig_cfg" "rollback restores the pre-merge config"
[[ ! -e "$BTC_DROPIN" ]] || fail "rollback removes the btc drop-in"
[[ ! -e "$ETH_DROPIN" ]] || fail "rollback removes the eth drop-in"
assert_eq "$(db_fingerprints "$LIVE_DB" "$BTC_DB" "$ETH_DB")" "$before" "two-source rollback leaves every database untouched"

echo "== rollback after the merged process wrote state"
setup srcwritten
add_source coin-btc 8101 C-btc
add_source coin-eth 8102 C-eth
BTC_DB="$BASE/coin-btc/state.db"
ETH_DB="$BASE/coin-eth/state.db"
BTC_DROPIN="$UNITS/go-trader@live.service.d/50-merge-paper-btc.conf"
ETH_DROPIN="$UNITS/go-trader@live.service.d/50-merge-paper-eth.conf"
orig_cfg=$(cat "$LIVE_CFG")
out=$(run_merge_args --source btc=coin-btc --source eth=coin-eth --apply 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "apply before the post-write rollback exits 0 (rc=$rc)"; }
python3 -c '
import sqlite3
import sys
conn = sqlite3.connect(sys.argv[1])
conn.execute("CREATE TABLE IF NOT EXISTS merge_rollback_probe (k TEXT)")
conn.execute("INSERT INTO merge_rollback_probe VALUES (?)", ("written-after-apply",))
conn.commit()
conn.close()
' "$ETH_DB"
written=$(db_fingerprints "$ETH_DB")
out=$(run_merge_args --source btc=coin-btc --source eth=coin-eth --rollback 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "rollback after the merged process wrote state exits 0 (rc=$rc)"; }
assert_eq "$(cat "$LIVE_CFG")" "$orig_cfg" "a post-write rollback still restores the pre-merge config"
[[ ! -e "$BTC_DROPIN" ]] || fail "a post-write rollback removes the btc drop-in"
[[ ! -e "$ETH_DROPIN" ]] || fail "a post-write rollback removes the eth drop-in"
assert_eq "$(db_fingerprints "$ETH_DB")" "$written" "a post-write rollback never rewrites a stored book"

echo "== an interrupted two-source apply restores every installed file"
setup srcfail
add_source coin-btc 8101 C-btc
add_source coin-eth 8102 C-eth
BTC_DROPIN="$UNITS/go-trader@live.service.d/50-merge-paper-btc.conf"
ETH_DROPIN="$UNITS/go-trader@live.service.d/50-merge-paper-eth.conf"
SRC_JOURNAL="$BASE/live/merge-paper-btc+eth.journal"
orig_cfg=$(cat "$LIVE_CFG")
out=$(MERGE_PAPER_FAIL_AFTER=config run_merge_args --source btc=coin-btc --source eth=coin-eth --apply 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "4" "an interrupted two-source apply exits 4"
assert_eq "$(cat "$LIVE_CFG")" "$orig_cfg" "the config step is undone"
[[ ! -e "$BTC_DROPIN" ]] || fail "no drop-in survives a config-step interruption"
grep -qx rolled-back "$SRC_JOURNAL" || fail "the interrupted two-source journal is marked rolled-back"
out=$(MERGE_PAPER_FAIL_AFTER=override run_merge_args --source btc=coin-btc --source eth=coin-eth --apply 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "4" "an interrupted override step exits 4"
assert_eq "$(cat "$LIVE_CFG")" "$orig_cfg" "the config is restored after an override-step interruption"
[[ ! -e "$BTC_DROPIN" ]] || fail "the btc drop-in is removed after an override-step interruption"
[[ ! -e "$ETH_DROPIN" ]] || fail "the eth drop-in is removed after an override-step interruption"

echo "== the default paper partition and a source fold in one run"
setup bothparts
add_source coin-btc 8101 C-btc '{"max_drawdown_pct": 60, "daily_max_loss_usd": 500}'
out=$(run_merge_args --paper paper --source btc=coin-btc 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "--paper with --source exits 0 (rc=$rc)"; }
staged="$LIVE_CFG.merge-staged"
assert_eq "$(json_get "$staged" strategies.1.id)" "hl-x-paper" "the default paper partition keeps the bare alias"
assert_eq "$(json_get "$staged" strategies.1.paper_source)" "" "the default paper partition names no source"
assert_eq "$(json_get "$staged" strategies.2.id)" "hl-x-paper-btc" "the source takes the named alias"
assert_eq "$(json_get "$staged" portfolio_risk.paper.max_drawdown_pct)" "50" "the default paper override still comes from the paper deployment"
assert_eq "$(json_get "$staged" paper_sources.0.portfolio_risk.max_drawdown_pct)" "60" "the source override is written against the default paper partition"
assert_eq "$(json_get "$staged" paper_db_file)" "$(update_canonical_db_path "$PAPER_DB")" "the default paper partition still owns paper_db_file"
assert_contains "$out" "proof: 3 strategies compared, no effective difference" "both folded deployments are proven"

echo "== refusals that guard several sources"
setup srcsamedb
add_source coin-btc 8101 C-btc
add_source coin-eth 8102 C-eth
python3 - "$BASE/coin-eth/config.json" "$BASE/coin-btc/state.db" <<'PY'
import json, sys
p = sys.argv[1]
cfg = json.load(open(p))
cfg["db_file"] = sys.argv[2]
json.dump(cfg, open(p, "w"))
PY
out=$(run_merge_args --source btc=coin-btc --source eth=coin-eth 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "16" "two sources on one database exit 16"
assert_contains "$out" "every partition needs its own file" "the same-database refusal names the rule"

setup srcdeclared
add_source coin-btc 8101 C-btc
python3 - "$LIVE_CFG" <<'PY'
import json, sys
p = sys.argv[1]
cfg = json.load(open(p))
cfg["paper_sources"] = [{"id": "btc", "db_file": "/somewhere/else.db"}]
json.dump(cfg, open(p, "w"))
PY
out=$(run_merge_args --source btc=coin-btc 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "17" "a declared source on another database exits 17"
assert_contains "$out" "already declares paper source btc" "the conflict names the source id"

setup srcnested
add_source coin-btc 8101 C-btc
python3 - "$BASE/coin-btc/config.json" <<'PY'
import json, sys
p = sys.argv[1]
cfg = json.load(open(p))
cfg["paper_sources"] = [{"id": "inner", "db_file": "/tmp/inner.db"}]
json.dump(cfg, open(p, "w"))
PY
out=$(run_merge_args --source btc=coin-btc 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "16" "a source deployment that folded sources of its own exits 16"
assert_contains "$out" "already declares paper_sources" "the refusal names the key"

setup srcsplit
add_source coin-btc 8101 C-btc
python3 - "$BASE/coin-btc/config.json" <<'PY'
import json, sys
p = sys.argv[1]
cfg = json.load(open(p))
cfg["paper_db_file"] = "/tmp/split.db"
json.dump(cfg, open(p, "w"))
PY
out=$(run_merge_args --source btc=coin-btc 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "16" "a source deployment with its own split exits 16"
assert_contains "$out" "source btc config already sets paper_db_file" "the split refusal names the source"

setup srcregime
add_source coin-btc 8101 C-btc
python3 - "$BASE/coin-btc/config.json" <<'PY'
import json, sys
p = sys.argv[1]
cfg = json.load(open(p))
cfg["regime"] = {"enabled": True, "period": 14, "adx_threshold": 25}
json.dump(cfg, open(p, "w"))
PY
out=$(run_merge_args --source btc=coin-btc 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "21" "a root-key difference in a source refuses"
assert_contains "$out" "source btc: root key regime differs" "the root-key refusal names the source"

setup srcreplay
add_source coin-btc 8101 C-btc
add_source coin-eth 8102 C-eth
for inst in coin-btc coin-eth; do
python3 - "$BASE/$inst/config.json" <<'PY'
import json, sys
p = sys.argv[1]
cfg = json.load(open(p))
cfg["strategies"][0]["replay_sharing"] = "live_mirror"
json.dump(cfg, open(p, "w"))
PY
done
out=$(run_merge_args --source btc=coin-btc --source eth=coin-eth 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "21" "two sources mirroring one live strategy refuse"
assert_contains "$out" "is already mirrored by" "the replay refusal names the second owner"

echo "== --diff previews a source alias, its channel key and a redundant one"
setup srcdiff
add_source coin-btc 8101 C-btc
add_source coin-eth 8102 C-live
out=$(run_merge_args --source btc=coin-btc --source eth=coin-eth --diff 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "--diff with sources exits 0 (rc=$rc)"; }
assert_contains "$out" "diff: source btc = instance coin-btc (partition paper:btc)" "--diff names each source"
assert_contains "$out" "diff: alias hl-x -> hl-x-paper-btc (storage_strategy_id=hl-x)" "--diff previews the source alias"
assert_contains "$out" "diff: stamp hl-x-paper-btc paper_source=btc" "--diff previews the paper_source stamp"
assert_contains "$out" "diff: channel-plan discord.channels.hyperliquid-paper:btc=C-btc" "--diff names the source channel key it would add"
assert_contains "$out" "diff: channel-plan discord.channels.hyperliquid-paper:eth=C-live" "--diff keeps a source key the bare platform key alone would route"

echo "== a source channel key is pruned only when the paper key already carries it"
setup srcprune
add_source coin-btc 8101 C-live
python3 - "$LIVE_CFG" <<'PY'
import json, sys
p = sys.argv[1]
cfg = json.load(open(p))
cfg["discord"]["channels"]["hyperliquid-paper"] = "C-live"
json.dump(cfg, open(p, "w"))
PY
out=$(run_merge_args --source btc=coin-btc --diff 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "--diff over a pinned paper key exits 0 (rc=$rc)"; }
assert_contains "$out" "discord.channels.hyperliquid-paper:btc not added (paper value C-live already routes through discord.channels.hyperliquid-paper)" "a source key falls through to the paper key, never to the bare platform key"
out=$(run_merge_args --source btc=coin-btc --apply 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "the pinned-key apply exits 0 (rc=$rc)"; }
assert_eq "$(json_get "$LIVE_CFG" discord.channels.hyperliquid-paper:btc)" "" "the pruned source key stays out of the merged config"

echo "== a later paper fold cannot move a folded source's channel"
setup srclater
add_source coin-btc 8101 C-live
python3 - "$PAPER_CFG" <<'PY'
import json, sys
p = sys.argv[1]
cfg = json.load(open(p))
cfg["discord"]["channels"]["hyperliquid"] = "C-other-paper"
json.dump(cfg, open(p, "w"))
PY
out=$(run_merge_args --source btc=coin-btc --apply 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "the source apply exits 0 (rc=$rc)"; }
assert_eq "$(json_get "$LIVE_CFG" discord.channels.hyperliquid-paper:btc)" "C-live" "the source keeps an explicit key while no paper key pins its route"
out=$(run_merge_args --source btc=coin-btc --paper paper --apply 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "the later paper fold exits 0 (rc=$rc)"; }
assert_eq "$(json_get "$LIVE_CFG" discord.channels.hyperliquid-paper)" "C-other-paper" "the later fold adds the paper key the resolver reads first"
assert_eq "$(json_get "$LIVE_CFG" discord.channels.hyperliquid-paper:btc)" "C-live" "the earlier source still routes to its own channel"

echo "== an alias that is taken takes the next suffix"
setup srcalias
add_source coin-btc 8101 C-btc
python3 - "$LIVE_CFG" <<'PY'
import json, sys
p = sys.argv[1]
cfg = json.load(open(p))
cfg["strategies"].append({"id": "hl-x-paper-btc", "type": "perps", "platform": "hyperliquid",
    "script": "shared_scripts/check_hyperliquid.py", "args": ["vwap", "BTC", "1h", "--mode=live"],
    "capital": 100, "leverage": 5, "margin_per_trade_usd": 50})
json.dump(cfg, open(p, "w"))
PY
out=$(run_merge_args --source btc=coin-btc --diff 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "--diff over a taken source alias exits 0 (rc=$rc)"; }
assert_contains "$out" "diff: alias hl-x -> hl-x-paper-btc2 (storage_strategy_id=hl-x)" "a taken source alias takes the numeric suffix"

echo "== source argument refusals"
setup srcusage
out=$(run_merge_args --source coin-btc 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "2" "--source without <id>=<instance> exits 2"
out=$(run_merge_args --source BTC=coin-btc 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "2" "an upper-case source id exits 2"
out=$(run_merge_args --source paper=coin-btc 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "2" "a reserved source id exits 2"
assert_contains "$out" "invalid paper source id 'paper'" "the reserved id is named"
out=$(run_merge_args --source btc=live 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "2" "folding the live instance exits 2"
out=$(run_merge_args --source btc=coin-btc --source btc=coin-eth 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "2" "one id naming two deployments exits 2"

echo "== a source id may not take a drop-in name an applied merge already owns"
setup keyclash
add_source coin-a 8101 C-a
add_source coin-b 8102 C-b
add_source coin-z 8103 C-z
A_DROPIN="$UNITS/go-trader@live.service.d/50-merge-paper-coin-a.conf"
out=$(run_merge_args --paper coin-a --source btc=coin-b --apply 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "apply before the key clash exits 0 (rc=$rc)"; }
a_dropin_before=$(cat "$A_DROPIN")
out=$(run_merge_args --source coin-a=coin-z 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "24" "a source id equal to an applied --paper instance name refuses"
assert_contains "$out" "already folded 'coin-a' as partition paper" "the refusal names the partition that owns the drop-in"
assert_contains "$out" "50-merge-paper-coin-a.conf" "the refusal names the drop-in both runs would write"
assert_eq "$(cat "$A_DROPIN")" "$a_dropin_before" "the refused run leaves the applied drop-in alone"
out=$(run_merge_args --paper coin-a --source btc=coin-b --source eth=coin-z 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "adding a source to an applied merge still certifies (rc=$rc)"; }
assert_contains "$out" "VERDICT: READY" "a later run that keeps every fold key on its own partition reaches READY"

echo "== the reverse direction is refused too"
setup keyclash2
add_source coin-a 8101 C-a
add_source coin-b 8102 C-b
add_source a 8103 C-plain
out=$(run_merge_args --source a=coin-a --source btc=coin-b --apply 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "source-first apply exits 0 (rc=$rc)"; }
out=$(run_merge_args --paper a 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "24" "a --paper instance named like an applied source refuses"
assert_contains "$out" "already folded 'a' as partition paper:a" "the reverse refusal names the source partition"

echo "== an interrupted apply under another fold set stops every later run"
setup otherjournal
add_source coin-btc 8101 C-btc
printf 'run_id x\nfold ghost paper:ghost coin-ghost\nconfig begin\nconfig done\n' > "$BASE/live/merge-paper-ghost.journal"
out=$(run_merge_args --source btc=coin-btc 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "24" "a dry run refuses while another journal records an interrupted apply"
assert_contains "$out" "merge-paper-ghost.journal records an interrupted apply" "the refusal names the other journal"
out=$(run_merge_args --source btc=coin-btc --apply 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "24" "an apply refuses on the same evidence"
printf 'rolled-back\n' >> "$BASE/live/merge-paper-ghost.journal"
out=$(run_merge_args --source btc=coin-btc 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "a rolled-back other journal does not refuse (rc=$rc)"; }
printf 'run_id y\nfold other paper:other coin-other\ncomplete\n' > "$BASE/live/merge-paper-other.journal"
out=$(run_merge_args --source btc=coin-btc 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "a complete other journal does not refuse (rc=$rc)"; }

echo "== rolling an older run back after a newer merge is refused, not silently reverted"
setup outoforder
add_source coin-btc 8101 C-btc
orig_cfg=$(cat "$LIVE_CFG")
out=$(run_merge_args --paper paper --apply 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "first apply exits 0 (rc=$rc)"; }
first_cfg=$(cat "$LIVE_CFG")
out=$(run_merge_args --source btc=coin-btc --apply 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "second apply exits 0 (rc=$rc)"; }
out=$(run_merge_args --paper paper --rollback 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "24" "rolling the older run back after a newer merge refuses"
assert_contains "$out" "merge-paper-btc.journal installed" "the refusal names the newer run"
assert_eq "$(json_get "$LIVE_CFG" paper_sources.0.id)" "btc" "the newer merge is still installed"
out=$(run_merge_args --source btc=coin-btc --rollback 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "rolling the newest run back stays one command (rc=$rc)"; }
assert_eq "$(cat "$LIVE_CFG")" "$first_cfg" "the newest rollback restores the previous merge"
out=$(run_merge_args --paper paper --rollback 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "the older rollback then succeeds (rc=$rc)"; }
assert_eq "$(cat "$LIVE_CFG")" "$orig_cfg" "rolling back in order restores the pre-merge config"

echo "== rollback needs only the retained files, not the folded deployment"
setup rollbackgone
add_source coin-btc 8101 C-btc
orig_cfg=$(cat "$LIVE_CFG")
BTC_DROPIN="$UNITS/go-trader@live.service.d/50-merge-paper-btc.conf"
out=$(run_merge_args --source btc=coin-btc --apply 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "apply before deployment retirement exits 0 (rc=$rc)"; }
rm -r "$OPT/go-trader-coin-btc"
out=$(run_merge_args --source btc=coin-btc --rollback 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "rollback after the deployment is retired exits 0 (rc=$rc)"; }
assert_eq "$(cat "$LIVE_CFG")" "$orig_cfg" "rollback restores the config without the deployment"
[[ ! -e "$BTC_DROPIN" ]] || fail "rollback removes the drop-in without the deployment"

echo "== rollback without the folded config names the database it cannot lock"
setup rollbacknocfg
add_source coin-btc 8101 C-btc
BTC_DROPIN="$UNITS/go-trader@live.service.d/50-merge-paper-btc.conf"
orig_cfg=$(cat "$LIVE_CFG")
out=$(run_merge_args --source btc=coin-btc --apply 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "apply before the config is removed exits 0 (rc=$rc)"; }
rm "$BASE/coin-btc/config.json"
out=$(run_merge_args --source btc=coin-btc --rollback 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "rollback without the folded config exits 0 (rc=$rc)"; }
assert_contains "$out" "is gone; its database is neither locked nor fingerprinted" "the rollback names the database it cannot cover"
assert_eq "$(cat "$LIVE_CFG")" "$orig_cfg" "rollback restores the config without the folded config"
[[ ! -e "$BTC_DROPIN" ]] || fail "rollback removes the drop-in without the folded config"

echo "== stacked merges that share a fold key each keep their own retained drop-in"
setup stackretain
add_source coin-a 8101 C-a
add_source coin-b 8102 C-b
mkdir -p "$(dirname "$DROPIN")"
printf '[Service]\nNice=7\n' > "$DROPIN"
prior_dropin=$(cat "$DROPIN")
orig_cfg=$(cat "$LIVE_CFG")
out=$(run_merge_args --paper paper --apply 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "the first stacked apply exits 0 (rc=$rc)"; }
first_cfg=$(cat "$LIVE_CFG")
out=$(run_merge_args --paper paper --source a=coin-a --apply 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "the second stacked apply exits 0 (rc=$rc)"; }
second_cfg=$(cat "$LIVE_CFG")
out=$(run_merge_args --paper paper --source a=coin-a --source b=coin-b --apply 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "the third stacked apply exits 0 (rc=$rc)"; }
out=$(run_merge_args --paper paper --source a=coin-a --source b=coin-b --rollback 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "rolling the newest stacked run back exits 0 (rc=$rc)"; }
assert_eq "$(cat "$LIVE_CFG")" "$second_cfg" "the newest rollback restores the second merge"
out=$(run_merge_args --paper paper --source a=coin-a --rollback 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "rolling the middle stacked run back exits 0 (rc=$rc)"; }
assert_eq "$(cat "$LIVE_CFG")" "$first_cfg" "the middle rollback restores the first merge"
out=$(run_merge_args --paper paper --rollback 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "rolling the oldest stacked run back exits 0 (rc=$rc)"; }
assert_eq "$(cat "$LIVE_CFG")" "$orig_cfg" "the oldest rollback restores the pre-merge config"
assert_eq "$(cat "$DROPIN")" "$prior_dropin" "the drop-in that existed before any merge comes back byte-for-byte"

echo "== a repeat apply of one stacked run stays a no-op"
setup stackrepeat
add_source coin-a 8101 C-a
out=$(run_merge_args --paper paper --apply 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "the apply before the repeat exits 0 (rc=$rc)"; }
out=$(run_merge_args --paper paper --source a=coin-a --apply 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "the stacked apply exits 0 (rc=$rc)"; }
stacked_cfg=$(cat "$LIVE_CFG")
out=$(run_merge_args --paper paper --source a=coin-a --apply 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "the repeat apply exits 0 (rc=$rc)"; }
assert_contains "$out" "nothing to do" "a repeat apply of the same arguments changes nothing"
assert_eq "$(cat "$LIVE_CFG")" "$stacked_cfg" "the repeat apply leaves the merged config alone"

echo "== a merge from a release that recorded no folds still owns its drop-in name"
setup legacyjournal
add_source coin-z 8103 C-z
add_source coin-b 8102 C-b
LEGACY_JOURNAL="$BASE/live/merge-paper-p.journal"
legacy_journal_body() {
    printf 'run_id 20260101000000-1\nlive_config a\npaper_config b\nlive_db c\npaper_db d\n'
    printf 'staged_config e\nstaged_override f\noverride_prior absent\n'
    printf 'config begin\nconfig done\noverride begin\noverride done\n'
    printf 'result_config g\nresult_override h\ncomplete\n'
}
legacy_journal_body > "$LEGACY_JOURNAL"
out=$(run_merge_args --source p=coin-z --source b=coin-b 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "24" "a source id equal to a legacy merge's key refuses"
assert_contains "$out" "already folded 'p' as partition paper" "the refusal reads a journal that carries no fold line"
assert_contains "$out" "50-merge-paper-p.conf" "the refusal names the drop-in both runs would write"
out=$(run_merge_args --source b=coin-b 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "a legacy journal whose key this run never folds does not refuse (rc=$rc)"; }
legacy_journal_body > "$BASE/live/merge-paper-paper.journal"
out=$(run_merge_args --paper paper --source b=coin-b 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "a legacy journal whose key keeps partition paper does not refuse (rc=$rc)"; }
rm -f "$BASE/live/merge-paper-paper.journal"
printf 'rolled-back\n' >> "$LEGACY_JOURNAL"
out=$(run_merge_args --source p=coin-z --source b=coin-b 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "a rolled-back legacy journal does not refuse (rc=$rc)"; }

echo "== a rollback refuses while another journal records an interrupted apply"
setup rollbackinterrupted
add_source coin-btc 8101 C-btc
orig_cfg=$(cat "$LIVE_CFG")
out=$(run_merge_args --paper paper --apply 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "the apply before the interrupted run exits 0 (rc=$rc)"; }
first_cfg=$(cat "$LIVE_CFG")
printf 'run_id z\nfold btc paper:btc coin-btc\nconfig begin\nconfig done\n' > "$BASE/live/merge-paper-btc.journal"
out=$(run_merge_args --paper paper --rollback 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "24" "a rollback refuses while another journal records an interrupted apply"
assert_contains "$out" "merge-paper-btc.journal records an interrupted apply" "the rollback refusal names the other journal"
assert_eq "$(cat "$LIVE_CFG")" "$first_cfg" "the refused rollback leaves the config alone"
printf 'complete\n' >> "$BASE/live/merge-paper-btc.journal"
out=$(run_merge_args --paper paper --rollback 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "a complete other journal does not refuse a rollback (rc=$rc)"; }
assert_eq "$(cat "$LIVE_CFG")" "$orig_cfg" "the rollback restores the pre-merge config"
printf 'rolled-back\n' >> "$BASE/live/merge-paper-btc.journal"
out=$(run_merge_args --paper paper --rollback 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "a rolled-back other journal does not refuse a rollback (rc=$rc)"; }
assert_contains "$out" "already rolled back" "the second rollback stays a no-op"

echo "== a rollback of the interrupted run itself still restores"
setup rollbackself
add_source coin-btc 8101 C-btc
orig_cfg=$(cat "$LIVE_CFG")
out=$(MERGE_PAPER_FAIL_AFTER=config run_merge_args --source btc=coin-btc --apply 2>&1) && rc=0 || rc=$?
assert_rc "$rc" "4" "the interrupted apply exits 4"
assert_eq "$(cat "$LIVE_CFG")" "$orig_cfg" "the interrupted apply restores the config itself"
out=$(run_merge_args --source btc=coin-btc --rollback 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "rolling the interrupted run itself back exits 0 (rc=$rc)"; }
assert_eq "$(cat "$LIVE_CFG")" "$orig_cfg" "the rollback of the interrupted run keeps the pre-merge config"

echo "== rollback skips a folded database inside a deployment that is gone"
setup rollbacktree
add_source coin-btc 8101 C-btc
mv "$BASE/coin-btc/state.db" "$OPT/go-trader-coin-btc/scheduler/state.db"
source_cfg_json "scheduler/state.db" 8101 C-btc > "$BASE/coin-btc/config.json"
BTC_DROPIN="$UNITS/go-trader@live.service.d/50-merge-paper-btc.conf"
orig_cfg=$(cat "$LIVE_CFG")
out=$(run_merge_args --source btc=coin-btc --apply 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "apply over a deployment-tree database exits 0 (rc=$rc)"; }
rm -r "$OPT/go-trader-coin-btc"
out=$(run_merge_args --source btc=coin-btc --rollback 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "rollback after the deployment tree is deleted exits 0 (rc=$rc)"; }
assert_contains "$out" "is gone; it is neither locked nor fingerprinted" "the rollback names the database it cannot lock"
assert_eq "$(cat "$LIVE_CFG")" "$orig_cfg" "rollback restores the config without the deployment tree"
[[ ! -e "$BTC_DROPIN" ]] || fail "rollback removes the drop-in without the deployment tree"

echo "== rollback still refuses while another process holds a database lock"
setup rollbacklock
add_source coin-btc 8101 C-btc
out=$(run_merge_args --source btc=coin-btc --apply 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "apply before the lock test exits 0 (rc=$rc)"; }
hold_lock_in_background "$(update_canonical_db_path "$BASE/coin-btc/state.db").lock"
out=$(run_merge_args --source btc=coin-btc --rollback 2>&1) && rc=0 || rc=$?
kill "$HOLD_PID" 2>/dev/null || true
wait "$HOLD_PID" 2>/dev/null || true
assert_rc "$rc" "3" "a held source lock still refuses a rollback"

echo "== a partition's expected books come from the staged config, not from one run's fold"
setup partitioncount
paper_cfg_json "$PAPER_DB_CFG" '{"max_drawdown_pct": 25, "daily_max_loss_usd": 500}' > "$PAPER_CFG"
python3 - "$PAPER_CFG" <<'PY'
import json, sys
p = sys.argv[1]
cfg = json.load(open(p))
cfg["discord"]["channels"]["hyperliquid"] = "C-live"
json.dump(cfg, open(p, "w"))
PY
python3 - "$LIVE_CFG" <<'PY'
import json, sys
p = sys.argv[1]
cfg = json.load(open(p))
cfg["strategies"].append({"id": "hl-own-paper", "type": "perps", "platform": "hyperliquid",
    "script": "shared_scripts/check_hyperliquid.py", "args": ["vwap", "BTC", "1h", "--mode=paper"],
    "capital": 100, "leverage": 5, "margin_per_trade_usd": 50})
json.dump(cfg, open(p, "w"))
PY
out=$(run_merge_args --paper paper 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "a dry run with a live-side paper strategy exits 0 (rc=$rc)"; }
assert_contains "$out" "1 configured strategy without a stored book yet" "the paper partition expects the live config's own paper strategy too"
assert_contains "$out" "proof: 3 strategies compared, no effective difference" "a fold that changes nothing for the live side's own paper strategy still proves"

echo "OK: merge-paper-instance tests passed"
