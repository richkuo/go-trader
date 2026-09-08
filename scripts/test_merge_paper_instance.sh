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

echo "== second collision suffix"
setup suffix
python3 - "$LIVE_CFG" <<'PY'
import json, sys
p = sys.argv[1]
cfg = json.load(open(p))
del cfg["strategies"][0]["capital"]
cfg["strategies"].append({"id": "hl-x-paper", "type": "perps", "platform": "hyperliquid",
    "script": "shared_scripts/check_hyperliquid.py", "args": ["vwap", "BTC", "1h", "--mode=live"],
    "leverage": 5, "margin_per_trade_usd": 50})
json.dump(cfg, open(p, "w"))
PY
out=$(run_merge 2>&1) && rc=0 || rc=$?
[[ "$rc" == "0" ]] || { echo "$out" >&2; fail "second suffix dry run exits 0 (rc=$rc)"; }
assert_contains "$out" "rename hl-x -> hl-x-paper2 (storage_strategy_id=hl-x)" "second suffix used only when the first is taken"

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
assert_contains "$out" "is complete and both deployment files match its result" "dry run reports the complete journal"
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
assert_eq "$(json_get "$LIVE_CFG.merge-staged" strategies.2.id)" "hl-new" "the bookless strategy is still moved"

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

echo "OK: merge-paper-instance tests passed"
