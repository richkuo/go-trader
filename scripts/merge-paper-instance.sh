#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
source "${SCRIPT_DIR}/update_helpers.sh"

PAPER_ALIAS_PY="${SCRIPT_DIR}/paper_alias.py"
if [[ ! -f "$PAPER_ALIAS_PY" ]]; then
    echo "ERROR: missing $PAPER_ALIAS_PY - the shared -paper alias rule" >&2
    exit 2
fi

EXIT_USAGE=2
EXIT_LOCK_CONTENDED=3
EXIT_RESTORE_FAILED=4
EXIT_SOURCE_CHANGED=5
EXIT_DEPLOY_MISSING=10
EXIT_VERSION_MISMATCH=11
EXIT_BINARY_INCOMPATIBLE=12
EXIT_CONFIG_MISSING=13
EXIT_UNIT_ACTIVE=14
EXIT_PAPER_NOT_PAPER=15
EXIT_DB_IDENTITY=16
EXIT_LIVE_PAPER_DB_CONFLICT=17
EXIT_CONFIG_MIGRATION=18
EXIT_NEW_TARGET_REFUSED=19
EXIT_INSPECTION_REFUSED=20
EXIT_COMPOSE_REFUSED=21
EXIT_PROOF_REFUSED=22
EXIT_OVERRIDE_REFUSED=23
EXIT_JOURNAL_STATE=24

usage() {
    cat <<'EOF'
usage: merge-paper-instance.sh --live <instance> [--paper <instance>]
         [--source <id>=<instance>]... [--apply | --rollback | --diff]
         [--align-to-live] [--base <dir>] [--deploy-root <dir>] [--unit-dir <dir>]
         [--live-unit <unit>] [--paper-unit <unit>]
       merge-paper-instance.sh --new-target <name> --status-port <n>
         [--root-from <instance>] [--paper <instance>] [--source <id>=<instance>]...
         [--apply | --rollback | --diff] [--align-to-live] [--base <dir>]
         [--deploy-root <dir>] [--unit-dir <dir>] [--paper-unit <unit>]

Defaults: --base /var/lib/go-trader, --deploy-root /opt (deployments at
<root>/go-trader-<instance>), --unit-dir /etc/systemd/system, units
go-trader@<instance>.service. Without --apply nothing outside the staging
area changes.

--new-target <name> folds every named deployment into a new template service
go-trader@<name>, so no folded deployment becomes the target. Every folded
deployment must run go-trader@<instance>.service from the installed template,
under one service user and one release. --root-from (default: the first
folded deployment, --paper first, then --source by id) gives the root
settings: every refuse-on-difference key must agree across the folded
deployments, and the other dropped keys come from it. status_port comes from
--status-port, which a running process must not have bound, and db_file is
<base>/<name>/state.db, which starts empty. Root portfolio_risk is the root
deployment's block, and every source keeps its own effective limits in
paper_sources[].portfolio_risk. The dry run stages every file in a temporary
directory and proves the fold with the first folded deployment's binary.
--apply creates <base>/<name>/ (0700) with config.json (0600) and the journal,
clones the first folded deployment's git origin into <deploy-root>/go-trader-<name>,
links scheduler/config.json, writes a 0600 .env from the folded deployments'
.env files (a variable with two values refuses, and no value is printed;
GO_TRADER_SERVICE naming a folded unit becomes go-trader@<name>.service),
builds the tree with scripts/update.sh --rsync-from <first folded deployment>,
proves the fold again with the new binary, installs go-trader@.service and
the journald namespace config only when absent, and writes the drop-ins. It
enables and starts nothing. --rollback with the same fold arguments refuses
while go-trader@<name> is active; otherwise it disables that unit, removes the
drop-ins and moves both new directories aside as .rolled-back-<run_id>. The
folded configs are never changed in this mode, and no database is rolled back.

In both modes leaderboard_summaries is the union of the target's entries and
every folded deployment's entries, keyed like the scheduler (platform, ticker
or *, channel). An identical entry appears once, and one key with two
different entries refuses (exit 21).

--paper folds one deployment into the default paper partition: every strategy
is aliased as <base>-paper, and the numeric suffix is incremented
(<base>-paper2, <base>-paper3, ...) until the name is free in the merged
config. --source <id>=<instance> folds a deployment into its own partition
paper:<id>: it adds one paper_sources entry with that id and the deployment's
database, aliases every strategy as <base>-paper-<id> (again with a numeric
suffix when taken), and stamps paper_source=<id> on it. Both may be given, and
--source may repeat. The alias becomes the in-process id and
storage_strategy_id keeps the bare stored id from the folded database, so the
stored books are untouched. An id that already carries its own alias keeps it
when the name is free.

The merged unit gets one drop-in per folded deployment
(50-merge-paper-<id>.conf for a source, 50-merge-paper-<instance>.conf for
--paper), so every folded database directory stays writable. One journal per
run names every folded deployment, and --rollback needs the same --paper and
--source arguments as the apply it undoes.

--diff reads only the config files and prints, per folded deployment, every
differing root key (refuse-on-difference, dropped with the live value kept, or
unknown), the alias every strategy would take, every discord channel key the
merge would add (channel-plan lines, the same content compose prints), plus
compose refuses it can see without inspect (replay_log_path when a paper
mirror is present and the merged config would still have a live mirror,
discord clashes from strategies compose would newly merge). A paper channel
value that already routes through a merged key the resolver reads first adds
no key and is named as not added. dm_channels keys are always added, since the
paper DM route reads that exact key. It is not a dry run: inspect-based
portfolio_risk refuses still need the full pipeline.
Units may stay running and no lock or binary is used.
--align-to-live is valid only with --diff or --apply: it writes live's
shared root values to <paper-config>.aligned for every folded deployment and
never changes a source; --apply then composes from those files. Exit codes:
2 usage, 3 lock contention, 4 restore failed, 5 source changed before apply,
10-19 preflight refusals (18: a config migration is pending or the configs
carry different versions; 19: a --new-target refusal), 20-24 inspection,
compose, proof, override and journal refusals. The binaries run against
copies of every config; the deployment files are never rewritten.
EOF
}

LIVE=""
PAPER=""
NEW_TARGET=""
STATUS_PORT=""
ROOT_FROM=""
MODE="dry-run"
ALIGN_TO_LIVE=0
BASE="/var/lib/go-trader"
DEPLOY_ROOT="/opt"
UNIT_DIR="/etc/systemd/system"
LIVE_UNIT=""
PAPER_UNIT=""
SYSTEMCTL="${MERGE_PAPER_SYSTEMCTL:-systemctl}"
SYSTEMD_ANALYZE="${MERGE_PAPER_SYSTEMD_ANALYZE:-systemd-analyze}"
FAIL_AFTER="${MERGE_PAPER_FAIL_AFTER:-}"
BUILD_CMD="${MERGE_PAPER_BUILD_CMD:-}"
JOURNALD_ETC="${MERGE_PAPER_JOURNALD_DIR:-/etc/systemd}"
declare -a SOURCE_SPEC=()

while [[ $# -gt 0 ]]; do
    case "$1" in
        --live) LIVE="${2:-}"; shift 2 ;;
        --new-target) NEW_TARGET="${2:-}"; shift 2 ;;
        --status-port) STATUS_PORT="${2:-}"; shift 2 ;;
        --root-from) ROOT_FROM="${2:-}"; shift 2 ;;
        --paper) PAPER="${2:-}"; shift 2 ;;
        --source) SOURCE_SPEC+=("${2:-}"); shift 2 ;;
        --apply)
            [[ "$MODE" == "dry-run" ]] || { echo "ERROR: use only one of --apply, --rollback, --diff" >&2; usage >&2; exit "$EXIT_USAGE"; }
            MODE="apply"; shift ;;
        --rollback)
            [[ "$MODE" == "dry-run" ]] || { echo "ERROR: use only one of --apply, --rollback, --diff" >&2; usage >&2; exit "$EXIT_USAGE"; }
            MODE="rollback"; shift ;;
        --diff)
            [[ "$MODE" == "dry-run" ]] || { echo "ERROR: use only one of --apply, --rollback, --diff" >&2; usage >&2; exit "$EXIT_USAGE"; }
            MODE="diff"; shift ;;
        --align-to-live) ALIGN_TO_LIVE=1; shift ;;
        --base) BASE="${2:-}"; shift 2 ;;
        --deploy-root) DEPLOY_ROOT="${2:-}"; shift 2 ;;
        --unit-dir) UNIT_DIR="${2:-}"; shift 2 ;;
        --live-unit) LIVE_UNIT="${2:-}"; shift 2 ;;
        --paper-unit) PAPER_UNIT="${2:-}"; shift 2 ;;
        -h|--help) usage; exit 0 ;;
        *) echo "ERROR: unknown argument $1" >&2; usage >&2; exit "$EXIT_USAGE"; ;;
    esac
done

fail() {
    local code="$1"; shift
    echo "REFUSED (exit $code): $*" >&2
    exit "$code"
}

# The source id follows the same rule the scheduler and the alias helper use, so
# a name the config loader would reject never reaches a staged file.
validate_source_id() {
    GO_TRADER_SCRIPT_DIR="$SCRIPT_DIR" python3 -c '
import os
import sys
sys.path.insert(0, os.environ["GO_TRADER_SCRIPT_DIR"])
from paper_alias import paper_alias_suffix
print("ok" if paper_alias_suffix(sys.argv[1]) else "bad")
' "$1"
}

NT=0
TARGET_WORD="live"
if [[ -n "$NEW_TARGET" ]]; then
    [[ -z "$LIVE" ]] || fail "$EXIT_USAGE" "--new-target and --live are exclusive"
    [[ -z "$LIVE_UNIT" ]] || fail "$EXIT_USAGE" "--live-unit applies only with --live; the new target's unit is go-trader@<name>.service"
    [[ "$(update_validate_instance_name "$NEW_TARGET")" == "ok" ]] || fail "$EXIT_USAGE" "invalid new target name '$NEW_TARGET'"
    [[ "$STATUS_PORT" =~ ^[0-9]{1,5}$ ]] && (( 10#$STATUS_PORT >= 1 && 10#$STATUS_PORT <= 65535 )) || \
        fail "$EXIT_USAGE" "--new-target needs --status-port <n> with 1 <= n <= 65535"
    STATUS_PORT=$((10#$STATUS_PORT))
    NT=1
    TARGET_WORD="target"
    LIVE="$NEW_TARGET"
else
    [[ -z "$STATUS_PORT" ]] || fail "$EXIT_USAGE" "--status-port applies only with --new-target"
    [[ -z "$ROOT_FROM" ]] || fail "$EXIT_USAGE" "--root-from applies only with --new-target"
fi
[[ -n "$LIVE" ]] || { usage >&2; exit "$EXIT_USAGE"; }
[[ -n "$PAPER" || ${#SOURCE_SPEC[@]} -gt 0 ]] || { usage >&2; exit "$EXIT_USAGE"; }
[[ "$(update_validate_instance_name "$LIVE")" == "ok" ]] || fail "$EXIT_USAGE" "invalid live instance name '$LIVE'"
if [[ "$ALIGN_TO_LIVE" == "1" && "$MODE" != "diff" && "$MODE" != "apply" ]]; then
    fail "$EXIT_USAGE" "--align-to-live is only valid with --diff or --apply"
fi
[[ -z "$LIVE_UNIT" ]] && LIVE_UNIT="go-trader@${LIVE}.service"
[[ -z "$PAPER_UNIT" ]] && PAPER_UNIT="go-trader@${PAPER:-paper}.service"

LIVE_DEPLOY="${DEPLOY_ROOT%/}/go-trader-${LIVE}"
LIVE_BIN="${LIVE_DEPLOY}/go-trader"
LIVE_CFG="${BASE%/}/${LIVE}/config.json"
TARGET_DIR="${BASE%/}/${LIVE}"
STAGE_DIR=""
KEEP_STAGE=0

WORK=$(mktemp -d "${TMPDIR:-/tmp}/merge-paper-instance.XXXXXX")
cleanup() {
    update_stop_state_lock_holder
    rm -rf "$WORK"
    if [[ -n "$STAGE_DIR" && "$KEEP_STAGE" != "1" ]]; then
        rm -rf "$STAGE_DIR"
    fi
}
trap cleanup EXIT

if [[ "$NT" == "1" ]]; then
    STAGE_DIR=$(mktemp -d "${TMPDIR:-/tmp}/merge-paper-new-target-${LIVE}.XXXXXX")
    chmod 0700 "$STAGE_DIR"
    STAGED_CFG="${STAGE_DIR}/config.json"
    STAGED_MAP="${STAGE_DIR}/config.json.map.json"
    STAGE_PREFIX="${STAGE_DIR}/"
else
    STAGED_CFG="${LIVE_CFG}.merge-staged"
    STAGED_MAP="${LIVE_CFG}.merge-staged.map.json"
    STAGE_PREFIX="${TARGET_DIR}/"
fi

# One row per folded deployment, in the runtime's partition order: the default
# paper partition first, then every source by id. Every later loop (preflight,
# locks, compose, proof, apply, rollback) walks this table in the same order.
declare -a FOLD_KEY=() FOLD_ID=() FOLD_SFX=() FOLD_INSTANCE=() FOLD_PARTITION=()
declare -a FOLD_DEPLOY=() FOLD_BIN=() FOLD_CFG=() FOLD_UNIT=() FOLD_DROPIN=()
declare -a FOLD_RETAINED_DROPIN=() FOLD_LEGACY_RETAINED_DROPIN=()
declare -a FOLD_STAGED_OVERRIDE=() FOLD_CFG_COPY=()
declare -a FOLD_COMPOSE_CFG=() FOLD_INSPECT=() FOLD_DB=() FOLD_COUNT=() FOLD_PORT=()
declare -a FOLD_FP_CFG=() FOLD_FP_DB=() FOLD_MERGED=()

add_fold() {
    local id="$1" instance="$2" key sfx partition
    [[ "$(update_validate_instance_name "$instance")" == "ok" ]] || fail "$EXIT_USAGE" "invalid instance name '$instance'"
    [[ "$instance" != "$LIVE" ]] || fail "$EXIT_USAGE" "folded instance '$instance' is the $TARGET_WORD instance"
    if [[ -n "$id" ]]; then
        key="$id"
        sfx=".$id"
        partition="paper:${id}"
    else
        key="$instance"
        sfx=""
        partition="paper"
    fi
    local i
    for i in "${!FOLD_KEY[@]}"; do
        [[ "${FOLD_INSTANCE[$i]}" != "$instance" ]] || fail "$EXIT_USAGE" "instance '$instance' is folded twice"
        [[ "${FOLD_KEY[$i]}" != "$key" ]] || fail "$EXIT_USAGE" "'$key' names two folded deployments; a source id and a --paper instance name cannot collide"
    done
    FOLD_KEY+=("$key")
    FOLD_ID+=("$id")
    FOLD_SFX+=("$sfx")
    FOLD_INSTANCE+=("$instance")
    FOLD_PARTITION+=("$partition")
    FOLD_DEPLOY+=("${DEPLOY_ROOT%/}/go-trader-${instance}")
    FOLD_BIN+=("${DEPLOY_ROOT%/}/go-trader-${instance}/go-trader")
    FOLD_CFG+=("${BASE%/}/${instance}/config.json")
    if [[ -n "$id" ]]; then
        FOLD_UNIT+=("go-trader@${instance}.service")
    else
        FOLD_UNIT+=("$PAPER_UNIT")
    fi
    FOLD_DROPIN+=("$(update_unit_dropin_path "$UNIT_DIR" "$LIVE_UNIT" "50-merge-paper-${key}")")
    FOLD_STAGED_OVERRIDE+=("${STAGE_PREFIX}merge-paper-${key}.override.staged")
    FOLD_CFG_COPY+=("")
    FOLD_COMPOSE_CFG+=("")
    FOLD_INSPECT+=("")
    FOLD_DB+=("")
    FOLD_COUNT+=("")
    FOLD_PORT+=("")
    FOLD_FP_CFG+=("")
    FOLD_FP_DB+=("")
    FOLD_MERGED+=("0")
}

[[ -z "$PAPER" ]] || add_fold "" "$PAPER"
if [[ ${#SOURCE_SPEC[@]} -gt 0 ]]; then
    declare -a SORTED_SPEC=()
    while IFS= read -r spec; do
        SORTED_SPEC+=("$spec")
    done < <(printf '%s\n' "${SOURCE_SPEC[@]}" | LC_ALL=C sort -t= -k1,1)
    for spec in "${SORTED_SPEC[@]}"; do
        [[ "$spec" == *=* ]] || fail "$EXIT_USAGE" "--source takes <id>=<instance>, got '$spec'"
        src_id="${spec%%=*}"
        src_instance="${spec#*=}"
        [[ -n "$src_id" && -n "$src_instance" ]] || fail "$EXIT_USAGE" "--source takes <id>=<instance>, got '$spec'"
        [[ "$(validate_source_id "$src_id")" == "ok" ]] || \
            fail "$EXIT_USAGE" "invalid paper source id '$src_id'; use [a-z0-9][a-z0-9_-]{0,31} and not live, paper or primary"
        add_fold "$src_id" "$src_instance"
    done
fi

FOLD_COUNT_TOTAL=${#FOLD_KEY[@]}
MERGE_KEY=$(IFS='+'; printf '%s' "${FOLD_KEY[*]}")
JOURNAL="${BASE%/}/${LIVE}/merge-paper-${MERGE_KEY}.journal"
RETAINED_CFG="${LIVE_CFG}.pre-merge-${MERGE_KEY}"

# Two runs can share a fold key and still own different journals, so the copy of
# the drop-in a run replaces carries the whole key set. A path built from the
# fold key alone would let a later run overwrite the copy an earlier run still
# needs, and that run's rollback would then have nothing to restore.
for i in "${!FOLD_KEY[@]}"; do
    FOLD_RETAINED_DROPIN+=("${FOLD_DROPIN[$i]}.pre-merge-${MERGE_KEY}")
    FOLD_LEGACY_RETAINED_DROPIN+=("${FOLD_DROPIN[$i]}.pre-merge")
done

fold_desc() {
    local i="$1"
    if [[ -n "${FOLD_ID[$i]}" ]]; then
        printf 'source %s (instance %s, partition %s)' "${FOLD_ID[$i]}" "${FOLD_INSTANCE[$i]}" "${FOLD_PARTITION[$i]}"
    else
        printf 'paper instance %s (partition paper)' "${FOLD_INSTANCE[$i]}"
    fi
}

ROOT_IDX=0
if [[ -n "$ROOT_FROM" ]]; then
    ROOT_IDX=-1
    for i in "${!FOLD_KEY[@]}"; do
        if [[ "${FOLD_INSTANCE[$i]}" == "$ROOT_FROM" ]]; then
            ROOT_IDX=$i
        fi
    done
    [[ "$ROOT_IDX" -ge 0 ]] || fail "$EXIT_USAGE" "--root-from $ROOT_FROM names no folded deployment; name one --paper or --source instance"
fi

MERGE_PY=$(cat <<'PY'
import json
import os
import sys

sys.path.insert(0, os.environ["GO_TRADER_SCRIPT_DIR"])
from paper_alias import paper_alias_base
from paper_alias import paper_alias_suffix

MODE_LIVE = "live"
MODE_PAPER = "paper"
PAPER_SOURCE_SEPARATOR = ":"
TARGET_LABEL = os.environ.get("MERGE_TARGET_LABEL") or "live"

RISK_FIELDS = [
    "max_drawdown_pct",
    "max_notional_usd",
    "warn_threshold_pct",
    "daily_max_loss_usd",
    "daily_max_loss_pct",
    "max_same_direction_notional_usd",
    "max_asset_concentration_pct",
]
REFUSE_ON_DIFFERENCE = [
    "regime",
    "correlation",
    "user_defaults",
    "default_stop_loss_atr_mult",
    "market_feed",
    "shared_market_feed",
    "role",
    "notify_tp_sl_fills",
    "notify_ratchet_triggers",
    "platforms",
    "risk_free_rate",
    "alert_throttle_interval",
    "kill_switch_reset_dm_timeout",
    "telegram",
]
DROPPED = [
    "status_port",
    "log_dir",
    "log_level",
    "auto_update",
    "leaderboard_post_time",
    "summary_frequency",
    "tradingview_export",
    "tuning",
    "config_version",
    "db_file",
    "paper_db_file",
    "paper_sources",
    "interval_seconds",
    "atr_method",
    "portfolio_risk",
    "discord",
    "strategies",
    "replay_log_path",
]
MERGED = ["leaderboard_summaries"]
CHANNEL_MAPS = ["channels", "trade_alert_channels", "dm_channels"]
CHANNEL_MAPS_WITH_BARE_KEY_FALLBACK = ["channels", "trade_alert_channels"]
COMPOSE_DROP_SILENT = (
    "strategies",
    "portfolio_risk",
    "discord",
    "db_file",
    "paper_db_file",
    "paper_sources",
    "config_version",
    "interval_seconds",
    "atr_method",
    "replay_log_path",
)

def load(path):
    with open(path) as f:
        text = f.read()
    try:
        return json.loads(text)
    except ValueError as exc:
        head = "\n".join(text.splitlines()[:5])
        refuse("%s is not a JSON document (%s); first lines:\n%s" % (path, exc, head))

def refuse(msg):
    print("REFUSE: %s" % msg)
    sys.exit(1)

# One folded deployment. The legacy --paper deployment carries an empty id and
# lands in the default paper partition; every --source deployment carries its
# own id and lands in paper:<id>.
def fold_prefix(f):
    return "source %s: " % f["id"] if f["id"] else ""

def fold_label(f):
    return "source %s" % f["id"] if f["id"] else "paper"

def root_value(cfg, key):
    v = cfg.get(key)
    if key == "market_feed":
        return (v or "").strip() or "rest"
    if key == "role":
        return (v or "").strip() or "scheduler"
    return v

def dump_root(cfg, key):
    return json.dumps(cfg.get(key), sort_keys=True)

def collect_root_diffs(live, paper):
    refuse_keys = []
    unknown_keys = []
    dropped_keys = []
    for key in REFUSE_ON_DIFFERENCE:
        if (key in paper or key in live) and root_value(paper, key) != root_value(live, key):
            refuse_keys.append(key)
    for key in sorted(set(paper) | set(live)):
        if key in MERGED:
            continue
        if key in DROPPED or key in REFUSE_ON_DIFFERENCE:
            if key in DROPPED and key in paper and key not in COMPOSE_DROP_SILENT:
                if paper.get(key) != live.get(key):
                    dropped_keys.append(key)
            continue
        if paper.get(key) != live.get(key):
            unknown_keys.append(key)
    return refuse_keys, unknown_keys, dropped_keys

def print_root_diff_report(live, paper, refuse_keys, unknown_keys, dropped_keys):
    t = TARGET_LABEL
    for key in refuse_keys:
        print("diff: refuse-on-difference %s %s=%s paper=%s" % (key, t, dump_root(live, key), dump_root(paper, key)))
    for key in unknown_keys:
        print("diff: unknown %s %s=%s paper=%s" % (key, t, dump_root(live, key), dump_root(paper, key)))
    for key in dropped_keys:
        print("diff: dropped %s %s=%s paper=%s (%s value kept)" % (key, t, dump_root(live, key), dump_root(paper, key), t))
    if not refuse_keys and not unknown_keys and not dropped_keys:
        print("diff: no root-key differences")
        return
    print("diff: %d refuse-on-difference, %d unknown, %d dropped" % (len(refuse_keys), len(unknown_keys), len(dropped_keys)))

def refuse_root_conflicts(conflicts):
    t = TARGET_LABEL
    for f, live, paper, refuse_keys, unknown_keys, dropped_keys in conflicts:
        pre = fold_prefix(f)
        for key in refuse_keys:
            print("REFUSE: %sroot key %s differs between %s and paper configs (an absent key is compared too, since the merged config would apply the %s value to the moved strategies): %s=%s paper=%s" % (
                pre, key, t, t, t, dump_root(live, key), dump_root(paper, key)))
        for key in unknown_keys:
            print("REFUSE: %sroot key %s differs between %s and paper configs and is not a known drop (an absent key is compared too): %s=%s paper=%s" % (
                pre, key, t, t, dump_root(live, key), dump_root(paper, key)))
        for key in dropped_keys:
            print("%sdropped paper root key %s (%s value kept): %s=%s paper=%s" % (
                pre, key, t, t, dump_root(live, key), dump_root(paper, key)))
    sys.exit(1)

def leaderboard_key(e):
    ticker = str(e.get("ticker") or "").strip().lower() or "*"
    return "%s:%s:%s" % (str(e.get("platform") or "").lower(), ticker, str(e.get("channel") or ""))

def leaderboard_entries(cfg, label):
    v = cfg.get("leaderboard_summaries")
    if v is None:
        return []
    if not isinstance(v, list) or not all(isinstance(e, dict) for e in v):
        refuse("%s leaderboard_summaries is not a list of objects" % label)
    return v

def merge_leaderboards(sources):
    merged = []
    origin = []
    seen = {}
    conflicts = []
    for label, entries in sources:
        for e in entries:
            key = leaderboard_key(e)
            if key not in seen:
                seen[key] = len(merged)
                merged.append(json.loads(json.dumps(e)))
                origin.append(label)
                continue
            first = merged[seen[key]]
            if first == e:
                continue
            conflicts.append("leaderboard_summaries key %s: %s has %s and %s has %s" % (
                key, origin[seen[key]], json.dumps(first, sort_keys=True), label, json.dumps(e, sort_keys=True)))
    return merged, origin, conflicts

def fold_leaderboard_sources(live, loaded):
    out = [(TARGET_LABEL, leaderboard_entries(live, TARGET_LABEL))]
    for f, cfg in loaded:
        out.append((fold_label(f), leaderboard_entries(cfg, fold_label(f))))
    return out

def write_json_atomic(path, doc, chown_from):
    tmp = path + ".tmp"
    fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w") as f:
        json.dump(doc, f, indent=2)
        f.write("\n")
    st = os.stat(chown_from)
    try:
        os.chown(tmp, st.st_uid, st.st_gid)
    except PermissionError:
        pass
    os.replace(tmp, path)

def cmd_root_diff(live_path, plan_path):
    live = load(live_path)
    plan = load(plan_path)
    loaded = [(f, load(f["config"])) for f in plan["folds"]]
    plans = compose_alias_plans(live, loaded)
    all_after = list(strategies(live))
    for f, cfg, aplan, skipped in plans:
        for s, pid in aplan:
            block = json.loads(json.dumps(s))
            block["id"] = pid
            all_after.append(block)
    any_mirror = any(mirror_source(s) is not None for s in all_after)
    merged_discord = json.loads(json.dumps(live.get("discord") or {}))
    conflicts = []
    channel_lines = []
    for f, cfg, aplan, skipped in plans:
        if f["id"]:
            print("diff: source %s = instance %s (partition %s)" % (f["id"], f["instance"], f["partition"]))
        refuse_keys, unknown_keys, dropped_keys = collect_root_diffs(live, cfg)
        print_root_diff_report(live, cfg, refuse_keys, unknown_keys, dropped_keys)
        previews, report = collect_compose_refuse_previews(live, cfg, f, aplan, any_mirror, merged_discord)
        for label, live_v, paper_v in previews:
            print("diff: compose-refuse %s%s live=%s paper=%s" % (fold_prefix(f), label, live_v, paper_v))
        channel_lines.extend(report)
        for s, pid in aplan:
            if pid != s["id"]:
                print("diff: alias %s -> %s (storage_strategy_id=%s)" % (s["id"], pid, storage_id(s)))
            else:
                print("diff: alias %s kept (already carries the %s alias)" % (s["id"], paper_alias_suffix(f["id"])))
            if f["id"]:
                print("diff: stamp %s paper_source=%s" % (pid, f["id"]))
        for sid in skipped:
            print("diff: alias %s already merged; no rename" % sid)
    for line in channel_lines:
        print("diff: channel-plan %s" % line)
    if not channel_lines:
        print("diff: channel-plan no discord channel keys to add")
    boards, board_origin, board_conflicts = merge_leaderboards(fold_leaderboard_sources(live, loaded))
    for e, label in zip(boards, board_origin):
        print("diff: leaderboard %s (from %s)" % (json.dumps(e, sort_keys=True), label))
    if not boards:
        print("diff: leaderboard no leaderboard_summaries entries")
    for msg in board_conflicts:
        print("diff: compose-refuse %s" % msg)
    print("diff: config-file preview only; inspect-based portfolio_risk refuses need a dry run")

def cmd_align(live_path, paper_path, out_path):
    live = load(live_path)
    paper = load(paper_path)
    refuse_keys, unknown_keys, _dropped = collect_root_diffs(live, paper)
    aligned = json.loads(json.dumps(paper))
    copied = 0
    for key in list(refuse_keys) + unknown_keys:
        before = paper[key] if key in paper else None
        if key in live:
            aligned[key] = json.loads(json.dumps(live[key]))
        elif key in aligned:
            del aligned[key]
        after = aligned[key] if key in aligned else None
        print("align: %s before=%s after=%s" % (key, json.dumps(before, sort_keys=True), json.dumps(after, sort_keys=True)))
        copied += 1
    write_json_atomic(out_path, aligned, paper_path)
    if copied == 0:
        print("align: no non-dropped root keys to copy")

def strategy_mode(s):
    args = s.get("args") or []
    args = [a for a in args if isinstance(a, str)]
    for i, a in enumerate(args):
        if a == "--mode=live":
            return MODE_LIVE
        if a == "--mode" and i + 1 < len(args) and args[i + 1] == "live":
            return MODE_LIVE
    return MODE_PAPER

def strategies(cfg):
    out = cfg.get("strategies")
    if not isinstance(out, list):
        return []
    return [s for s in out if isinstance(s, dict) and isinstance(s.get("id"), str)]

def storage_id(s):
    sid = s.get("storage_strategy_id")
    if isinstance(sid, str) and sid.strip():
        return sid.strip()
    return s["id"]

def strategy_source(s):
    return (s.get("paper_source") or "").strip()

def declared_paper_sources(cfg):
    out = []
    for src in cfg.get("paper_sources") or []:
        if isinstance(src, dict) and isinstance(src.get("id"), str) and src["id"].strip():
            out.append(src)
    return out

def is_hl_perps(s):
    platform = s.get("platform") or ("hyperliquid" if s["id"].startswith("hl-") else "")
    return s.get("type") == "perps" and platform == "hyperliquid"

def mirror_source(s):
    if s.get("replay_sharing") != "live_mirror" or strategy_mode(s) == MODE_LIVE or not is_hl_perps(s):
        return None
    src = s.get("replay_source_id")
    if isinstance(src, str) and src.strip():
        return src.strip()
    return s["id"]

def paper_channel_target(platform, source):
    if not source:
        return "%s-paper" % platform
    return "%s-paper%s%s" % (platform, PAPER_SOURCE_SEPARATOR, source)

# The merged resolver reads <platform>-paper:<id>, then <platform>-paper, then
# the bare platform key, then the strategy type. This names the first key below
# the one the merge would add, so an addition that changes nothing is pruned.
def merged_channel_route_key(mm, platform, stype, source=""):
    keys = []
    if source:
        keys.append("%s-paper" % platform)
    keys.extend([platform, stype])
    for key in keys:
        if mm.get(key):
            return key
    return ""

def apply_paper_discord_maps(merged_discord, paper_discord, used, source=""):
    report = []
    conflicts = []
    for map_key in CHANNEL_MAPS:
        pm = paper_discord.get(map_key) or {}
        mm = merged_discord.get(map_key)
        if mm is None:
            mm = {}
        added = []
        pinned = set()
        routed = {}
        for platform, stype in sorted(used):
            val = ""
            src = ""
            for key in ("%s-paper" % platform, platform, stype):
                if pm.get(key):
                    val = pm[key]
                    src = key
                    break
            if not val:
                continue
            target = paper_channel_target(platform, source)
            if target in mm and mm[target] != val:
                conflicts.append((
                    "discord.%s.%s" % (map_key, target),
                    mm[target],
                    val,
                    "discord.%s.%s is %r in the live config but the paper deployment routes to %r" % (map_key, target, mm[target], val),
                ))
            elif target not in mm:
                mm[target] = val
                added.append((target, val))
            if mm.get(target) == val:
                routed.setdefault(target, []).append((platform, stype))
                if src == "%s-paper" % platform:
                    pinned.add(target)
        passthrough = []
        for key in sorted(pm):
            val = pm[key]
            if not key.endswith("-paper"):
                continue
            target = paper_channel_target(key[: -len("-paper")], source)
            if target not in mm and val:
                mm[target] = val
                passthrough.append("discord.%s.%s=%s" % (map_key, target, val))
            elif target in mm and mm[target] != val:
                conflicts.append((
                    "discord.%s.%s" % (map_key, target),
                    mm[target],
                    val,
                    "discord.%s.%s differs: live=%r paper=%r" % (map_key, target, mm[target], val),
                ))
        candidates = []
        for target, val in added:
            if map_key not in CHANNEL_MAPS_WITH_BARE_KEY_FALLBACK:
                continue
            if mm.get(target) != val or target in pinned:
                continue
            routed_pairs = routed.get(target, [])
            route_keys = [merged_channel_route_key(mm, p, t, source) for p, t in routed_pairs]
            if not route_keys or not all(k and mm.get(k) == val for k in route_keys):
                continue
            # A later merge can add <platform>-paper between a source key and the
            # bare platform key it falls through to, and the resolver reads that
            # key first for every paper strategy. Prune a source key only when
            # <platform>-paper already carries the value, so no later fold can
            # move this partition's route.
            if source and any(k != "%s-paper" % p for k, (p, _t) in zip(route_keys, routed_pairs)):
                continue
            candidates.append(target)
        pruned = set(candidates)
        for target, val in added:
            if mm.get(target) != val:
                continue
            if target in pruned:
                del mm[target]
                report.append("discord.%s.%s not added (paper value %s already routes through discord.%s.%s)" % (
                    map_key, target, val, map_key, merged_channel_route_key(mm, routed[target][0][0], routed[target][0][1], source)))
            else:
                report.append("discord.%s.%s=%s" % (map_key, target, val))
        report.extend(passthrough)
        if mm:
            merged_discord[map_key] = mm
    return report, conflicts

# A repeat run recognises a deployment the live config already carries. Preflight
# decides it by canonical path and hands the verdict down in the plan file, so
# the shell and this helper can never disagree over a path a symlink or a mount
# spells differently. Only on a repeat may a stored book already sit in the
# merged config.
def fold_already_merged(live, f):
    already = f.get("merged") == "1"
    mine = set(storage_id(s) for s in strategies(live)
               if strategy_mode(s) == MODE_PAPER and strategy_source(s) == f["id"])
    return already, mine

def resolve_paper_alias(sid, taken, remaining, source=""):
    suffix = paper_alias_suffix(source)
    base = paper_alias_base(sid, source)
    if base is None:
        base = sid
    elif sid not in taken and sid not in remaining:
        return sid
    n = 1
    while True:
        cand = "%s%s" % (base, suffix) if n == 1 else "%s%s%d" % (base, suffix, n)
        if cand not in taken and cand not in remaining:
            return cand
        n += 1

def compose_alias_plans(live, loaded):
    taken = set(s["id"] for s in strategies(live))
    remaining = set()
    for _f, cfg in loaded:
        for s in strategies(cfg):
            remaining.add(s["id"])
    out = []
    for f, cfg in loaded:
        already_merged, mine = fold_already_merged(live, f)
        plan = []
        skipped = []
        for s in strategies(cfg):
            remaining.discard(s["id"])
            if already_merged and storage_id(s) in mine:
                skipped.append(s["id"])
                continue
            pid = resolve_paper_alias(s["id"], taken, remaining, f["id"])
            taken.add(pid)
            plan.append((s, pid))
        out.append((f, cfg, plan, skipped))
    return out

def used_route_pairs(blocks):
    used = set()
    for b in blocks:
        platform = b.get("platform") or ("hyperliquid" if b["id"].startswith("hl-") else "")
        used.add((platform, b.get("type") or ""))
    return used

def collect_compose_refuse_previews(live, cfg, f, aplan, any_mirror, merged_discord):
    previews = []
    seen = set()
    if any_mirror and (live.get("replay_log_path") or "") != (cfg.get("replay_log_path") or ""):
        if any(mirror_source(s) is not None for s in strategies(cfg)):
            seen.add("replay_log_path")
            previews.append(("replay_log_path", json.dumps(live.get("replay_log_path"), sort_keys=True), json.dumps(cfg.get("replay_log_path"), sort_keys=True)))
    used = used_route_pairs([s for s, _pid in aplan])
    report, conflicts = apply_paper_discord_maps(merged_discord, cfg.get("discord") or {}, used, f["id"])
    for label, live_v, paper_v, _msg in conflicts:
        if label not in seen:
            seen.add(label)
            previews.append((label, json.dumps(live_v, sort_keys=True), json.dumps(paper_v, sort_keys=True)))
    return previews, report

# The staged file for a partition may hold a book for every strategy the staged
# config places in that partition, including the live config's own paper
# strategies once the merged process has run a cycle. Counting only this run's
# moved strategies would refuse an unchanged repeat certification.
def cmd_partition_count(path, source):
    cfg = load(path)
    n = 0
    for s in strategies(cfg):
        if strategy_mode(s) == MODE_PAPER and strategy_source(s) == source:
            n += 1
    print(n)

def cmd_classify(path):
    cfg = load(path)
    counts = {MODE_LIVE: 0, MODE_PAPER: 0}
    for s in strategies(cfg):
        counts[strategy_mode(s)] += 1
    risk = cfg.get("portfolio_risk")
    sources = {}
    for src in declared_paper_sources(cfg):
        sources[src["id"].strip()] = (src.get("db_file") or "").strip()
    print(json.dumps({
        "live": counts[MODE_LIVE],
        "paper": counts[MODE_PAPER],
        "strategy_count": len(strategies(cfg)),
        "db_file": (cfg.get("db_file") or "scheduler/state.db").strip() or "scheduler/state.db",
        "paper_db_file": (cfg.get("paper_db_file") or "").strip(),
        "paper_sources": sources,
        "status_port": cfg.get("status_port"),
        "nested_paper_risk": isinstance(risk, dict) and "paper" in risk,
        "config_version": cfg.get("config_version"),
    }))

def cmd_get(path, key):
    doc = load(path)
    for part in key.split("."):
        if isinstance(doc, list):
            doc = doc[int(part)]
        else:
            doc = doc.get(part)
        if doc is None:
            print("")
            return
    if isinstance(doc, (dict, list)):
        print(json.dumps(doc))
    elif isinstance(doc, bool):
        print("true" if doc else "false")
    else:
        print(doc)

def listof(doc, key):
    v = doc.get(key)
    return v if isinstance(v, list) else []

def cmd_storage_check(path, holder_pid, expect_mapped, label):
    si = load(path)
    for key in ("files", "rejections"):
        si[key] = listof(si, key)
    for fi in si["files"]:
        for key in ("strategies", "orphans", "portfolio_risk_rows"):
            fi[key] = listof(fi, key)
    problems = []
    for fi in si.get("files", []):
        if fi.get("lock_held") and str(fi.get("lock_holder_pid", "")) != str(holder_pid):
            problems.append("%s file %s is owned by pid %s, not the handoff lock holder %s" % (
                fi.get("role"), fi.get("path"), fi.get("lock_holder_pid"), holder_pid))
    for r in si.get("rejections", []):
        problems.append("storage rejection: %s" % r)
    paper_files = [fi for fi in si.get("files", []) if fi.get("role") == label]
    for fi in paper_files:
        if fi.get("orphans"):
            problems.append("%s file %s holds %d orphan book(s): %s" % (
                fi.get("role"), fi.get("path"), len(fi["orphans"]),
                ", ".join(o.get("storage_strategy_id", "?") for o in fi["orphans"])))
        if expect_mapped != "-" and fi.get("present") and len(fi.get("strategies", [])) > int(expect_mapped):
            problems.append("%s file %s maps %d strategies; the paper config carries only %s" % (
                fi.get("role"), fi.get("path"), len(fi.get("strategies", [])), expect_mapped))
    if problems:
        for p in problems:
            print("REFUSE: %s" % p)
        sys.exit(1)
    for fi in paper_files:
        if not fi.get("present"):
            print("%s file %s: absent" % (fi.get("role"), fi.get("path")))
            continue
        latched = any(r.get("kill_switch_active") for r in fi.get("portfolio_risk_rows", []))
        positions = sum(int(r.get("position_count", 0)) for r in fi.get("strategies", []))
        print("%s file %s: %d strategies mapped, %d orphan, %d positions, %d pending actions, latch=%s" % (
            fi.get("role"), fi.get("path"), len(fi.get("strategies", [])), len(fi.get("orphans", [])),
            positions, int(fi.get("pending_manual_actions", 0)), "true" if latched else "false"))
        for row in fi.get("strategies", []):
            print("  %s -> %s (%d position(s))" % (row.get("storage_strategy_id"), row.get("process_strategy_id"), int(row.get("position_count", 0))))
        if expect_mapped != "-" and len(fi.get("strategies", [])) < int(expect_mapped):
            print("  %d configured strateg%s without a stored book yet (never ran a cycle); nothing to move for them" % (
                int(expect_mapped) - len(fi.get("strategies", [])), "y" if int(expect_mapped) - len(fi.get("strategies", [])) == 1 else "ies"))

def effective_root(cfg, key, default):
    v = cfg.get(key)
    if v is None or v == "" or v == 0:
        return default
    return v

def risk_fields(view):
    out = {}
    if not isinstance(view, dict):
        return out
    for k in RISK_FIELDS:
        v = view.get(k) or 0
        if v != 0:
            out[k] = v
    return out

def effective_scope_risk(inspect_path, scope):
    docs = load(inspect_path)
    views = [s.get("scope_risk") for s in docs if isinstance(s, dict) and s.get("scope") == scope]
    if not views:
        return None
    first = views[0]
    for v in views[1:]:
        if risk_fields(v) != risk_fields(first):
            refuse("inspect %s reports different %s-scope risk views across strategies: %s vs %s" % (
                inspect_path, scope, json.dumps(risk_fields(first), sort_keys=True), json.dumps(risk_fields(v), sort_keys=True)))
    return risk_fields(first)

def cmd_compose(live_path, plan_path, out_path, map_path, inspect_live_path):
    live = load(live_path)
    plan = load(plan_path)
    merged = json.loads(json.dumps(live))
    report = []
    loaded = []
    for f in plan["folds"]:
        cfg = load(f["config"])
        pre = fold_prefix(f)
        if cfg.get("paper_db_file"):
            refuse("%sthe paper config already splits its own state (paper_db_file); the handoff handles one primary file per side" % pre)
        if declared_paper_sources(cfg):
            refuse("%sthe paper config already declares paper_sources; hand off one deployment per source, never a deployment that folded sources of its own" % pre)
        for s in strategies(cfg):
            if strategy_mode(s) == MODE_LIVE:
                refuse("%spaper config strategy %s runs --mode=live" % (pre, s["id"]))
        loaded.append((f, cfg))

    plans = compose_alias_plans(live, loaded)
    fold_blocks = []
    for f, cfg, aplan, skipped in plans:
        renames = {}
        new_strats = []
        for s, pid in aplan:
            block = json.loads(json.dumps(s))
            if pid != s["id"]:
                renames[s["id"]] = pid
                block["id"] = pid
                if "storage_strategy_id" not in block:
                    block["storage_strategy_id"] = s["id"]
                report.append("rename %s -> %s (storage_strategy_id=%s)" % (s["id"], pid, block["storage_strategy_id"]))
            else:
                report.append("keep %s (already carries the %s alias; storage_strategy_id=%s)" % (
                    s["id"], paper_alias_suffix(f["id"]), storage_id(s)))
            if f["id"]:
                block["paper_source"] = f["id"]
                report.append("stamp %s paper_source=%s (partition %s)" % (block["id"], f["id"], f["partition"]))
            new_strats.append((s, block))
        live_interval = effective_root(live, "interval_seconds", 600)
        paper_interval = effective_root(cfg, "interval_seconds", 600)
        live_atr = (live.get("atr_method") or "simple").strip().lower() or "simple"
        paper_atr = (cfg.get("atr_method") or "simple").strip().lower() or "simple"
        for s, block in new_strats:
            if paper_interval != live_interval and not block.get("interval_seconds"):
                block["interval_seconds"] = paper_interval
                report.append("stamp %s interval_seconds=%s (paper root cadence)" % (block["id"], paper_interval))
            if paper_atr != live_atr and block.get("type") != "options" and not (block.get("atr_method") or "").strip():
                block["atr_method"] = paper_atr
                report.append("stamp %s atr_method=%s (paper root method)" % (block["id"], paper_atr))
        fold_blocks.append((f, cfg, renames, new_strats, skipped))

    merged_strats = [s for s in (merged.get("strategies") or []) if isinstance(s, dict)]
    pairs = [(s, {}) for s in merged_strats]
    for f, cfg, renames, new_strats, skipped in fold_blocks:
        for _s, block in new_strats:
            pairs.append((block, renames))
    all_after = [b for b, _r in pairs]
    by_id = dict((s["id"], s) for s in all_after if isinstance(s.get("id"), str))
    claimed = {}
    for block, rmap in pairs:
        src = mirror_source(block)
        if src is None:
            continue
        if block.get("replay_source_id"):
            continue
        if src in rmap and src != block["id"]:
            continue
        claimed.setdefault(src, []).append(block["id"])
    for f, cfg, renames, new_strats, skipped in fold_blocks:
        for s, block in new_strats:
            src = mirror_source(s)
            if src is None:
                continue
            explicit = bool((s.get("replay_source_id") or "").strip())
            if explicit:
                if src in renames:
                    block["replay_source_id"] = renames[src]
                    report.append("remap %s replay_source_id %s -> %s" % (block["id"], src, renames[src]))
                    src = renames[src]
            else:
                block["replay_source_id"] = src
                report.append("map %s replay_source_id=%s" % (block["id"], src))
            target = by_id.get(src)
            if target is None:
                refuse("replay mirror %s: source %s is not in the merged config" % (block["id"], src))
            if strategy_mode(target) != MODE_LIVE:
                refuse("replay mirror %s: source %s does not run --mode=live" % (block["id"], src))
            if target.get("replay_sharing") != "live_mirror":
                refuse("replay mirror %s: source %s does not set replay_sharing=live_mirror" % (block["id"], src))
            owners = [o for o in claimed.get(src, []) if o != block["id"]]
            for o in all_after:
                if o is block or o.get("id") == block["id"]:
                    continue
                if (o.get("replay_source_id") or "").strip() == src:
                    owners.append(o["id"])
            if owners:
                refuse("replay mirror %s: source %s is already mirrored by %s" % (block["id"], src, ", ".join(sorted(set(owners)))))
            claimed.setdefault(src, []).append(block["id"])
    any_mirror = any(mirror_source(s) is not None for s in all_after)
    if any_mirror:
        for f, cfg, renames, new_strats, skipped in fold_blocks:
            if (live.get("replay_log_path") or "") == (cfg.get("replay_log_path") or ""):
                continue
            if any(mirror_source(s) is not None for s in strategies(cfg)):
                refuse("%sroot key replay_log_path differs: live=%r paper=%r" % (
                    fold_prefix(f), live.get("replay_log_path"), cfg.get("replay_log_path")))

    live_risk = live.get("portfolio_risk")
    for f, cfg, renames, new_strats, skipped in fold_blocks:
        cfg_risk = cfg.get("portfolio_risk")
        if isinstance(cfg_risk, dict) and "paper" in cfg_risk:
            refuse("%spaper config nests portfolio_risk.paper" % fold_prefix(f))
    if live_risk is not None and not isinstance(live_risk, dict):
        refuse("live config portfolio_risk is not an object")
    live_eff = effective_scope_risk(inspect_live_path, MODE_LIVE)
    if live_eff is None:
        if isinstance(live_risk, dict) and "paper" in live_risk:
            refuse("the live config runs no live strategy and already carries portfolio_risk.paper; the effective live risk limits cannot be separated from the override")
        live_eff = effective_scope_risk(inspect_live_path, MODE_PAPER)
    if live_eff is None and plan.get("new_target"):
        root_fold = [f for f in plan["folds"] if f["key"] == plan["root_key"]][0]
        live_eff = effective_scope_risk(root_fold["inspect"], MODE_PAPER)
        if live_eff is None:
            refuse("the root deployment %s's inspect document carries no paper-scope strategy; the effective root risk limits are unknown" % root_fold["instance"])
        report.append("portfolio_risk root effective view taken from the root deployment %s: %s" % (
            root_fold["instance"], json.dumps(live_eff, sort_keys=True)))
    if live_eff is None:
        refuse("the live inspect document carries no strategy; the effective live risk limits are unknown")
    root = dict(live_risk) if isinstance(live_risk, dict) else {}
    existing_paper = root.pop("paper", None)
    if not risk_fields(root):
        root = dict((k, v) for k, v in live_eff.items())
        report.append("portfolio_risk root materialized from the effective live view: %s%s" % (
            json.dumps(root, sort_keys=True), "" if isinstance(live_risk, dict) else " (live config had no portfolio_risk block; the loader default now stays explicit)"))

    def source_override(f, above, above_label, existing, existing_label):
        eff = effective_scope_risk(f["inspect"], MODE_PAPER)
        if eff is None:
            refuse("%sthe paper inspect document carries no paper-scope strategy; the effective paper risk limits are unknown" % fold_prefix(f))
        override = {}
        for k in RISK_FIELDS:
            lv = above.get(k, 0)
            pv = eff.get(k, 0)
            if pv == lv:
                continue
            if pv == 0:
                refuse("%s%s.%s would be zero while %s sets %s; zero inherits the limit above and cannot disable it. Set an explicit paper value" % (
                    fold_prefix(f), existing_label, k, above_label, lv))
            override[k] = pv
        if existing is not None:
            existing_eff = dict(above)
            existing_eff.update(risk_fields(existing))
            override_eff = dict(above)
            override_eff.update(override)
            if existing_eff != override_eff:
                refuse("%s%s already exists with different values: %s vs paper deployment %s" % (
                    fold_prefix(f), existing_label, json.dumps(existing, sort_keys=True), json.dumps(override, sort_keys=True)))
            override = risk_fields(existing)
        return override

    legacy = None
    for f, cfg, renames, new_strats, skipped in fold_blocks:
        if not f["id"]:
            legacy = f
    if legacy is not None:
        paper_override = source_override(legacy, live_eff, "live", existing_paper, "portfolio_risk.paper")
    else:
        paper_override = risk_fields(existing_paper) if existing_paper is not None else {}
    if paper_override:
        root["paper"] = paper_override
        report.append("portfolio_risk.paper=%s" % json.dumps(paper_override, sort_keys=True))
    if root:
        merged["portfolio_risk"] = root
    paper_partition_eff = dict(live_eff)
    paper_partition_eff.update(paper_override)

    entries = []
    for src in declared_paper_sources(merged):
        entries.append(json.loads(json.dumps(src)))
    by_source_id = dict((e["id"].strip(), e) for e in entries)
    for f, cfg, renames, new_strats, skipped in fold_blocks:
        if not f["id"]:
            continue
        entry = by_source_id.get(f["id"])
        existing_risk = entry.get("portfolio_risk") if entry is not None else None
        label = "paper_sources[%s].portfolio_risk" % f["id"]
        override = source_override(f, paper_partition_eff, "the default paper partition", existing_risk, label)
        if entry is None:
            entry = {"id": f["id"], "db_file": f["db"]}
            entries.append(entry)
            by_source_id[f["id"]] = entry
            report.append("paper_sources[%s].db_file=%s" % (f["id"], f["db"]))
        else:
            entry["db_file"] = f["db"]
            report.append("paper_sources[%s] already declared; db_file unchanged" % f["id"])
        if not (entry.get("label") or "").strip():
            entry["label"] = f["instance"]
        if override:
            entry["portfolio_risk"] = override
            report.append("%s=%s" % (label, json.dumps(override, sort_keys=True)))
    if entries:
        entries.sort(key=lambda e: e["id"].strip())
        merged["paper_sources"] = entries

    merged_discord = merged.setdefault("discord", {})
    for f, cfg, renames, new_strats, skipped in fold_blocks:
        used = used_route_pairs([b for _s, b in new_strats])
        discord_report, discord_conflicts = apply_paper_discord_maps(merged_discord, cfg.get("discord") or {}, used, f["id"])
        if discord_conflicts:
            refuse("%s%s" % (fold_prefix(f), discord_conflicts[0][3]))
        report.extend(discord_report)

    boards, board_origin, board_conflicts = merge_leaderboards(fold_leaderboard_sources(live, loaded))
    if board_conflicts:
        for msg in board_conflicts:
            print("REFUSE: %s" % msg)
        sys.exit(1)
    if boards:
        merged["leaderboard_summaries"] = boards
        for i, (e, label) in enumerate(zip(boards, board_origin)):
            report.append("leaderboard_summaries[%d]=%s (from %s)" % (i, json.dumps(e, sort_keys=True), label))

    conflicts = []
    dropped_all = []
    dropped_lines = []
    for f, cfg, renames, new_strats, skipped in fold_blocks:
        refuse_keys, unknown_keys, dropped = collect_root_diffs(live, cfg)
        for key in dropped:
            if key not in dropped_all:
                dropped_all.append(key)
            dropped_lines.append("%sdropped paper root key %s (%s value kept): %s=%s paper=%s" % (
                fold_prefix(f), key, TARGET_LABEL, TARGET_LABEL, dump_root(live, key), dump_root(cfg, key)))
        if refuse_keys or unknown_keys:
            conflicts.append((f, live, cfg, refuse_keys, unknown_keys, dropped))
    if conflicts:
        refuse_root_conflicts(conflicts)

    added = []
    for f, cfg, renames, new_strats, skipped in fold_blocks:
        added.extend(b for _s, b in new_strats)
    merged["strategies"] = merged_strats + added
    if legacy is not None:
        merged["paper_db_file"] = legacy["db"]
    write_json_atomic(out_path, merged, live_path)
    fold_map = []
    all_renames = {}
    all_ids = []
    all_original = []
    all_skipped = []
    for f, cfg, renames, new_strats, skipped in fold_blocks:
        fold_map.append({
            "key": f["key"],
            "id": f["id"],
            "instance": f["instance"],
            "partition": f["partition"],
            "renames": renames,
            "ids": [b["id"] for _s, b in new_strats],
            "original_ids": [s["id"] for s, _b in new_strats],
            "skipped": skipped,
        })
        all_renames.update(renames)
        all_ids.extend(b["id"] for _s, b in new_strats)
        all_original.extend(s["id"] for s, _b in new_strats)
        all_skipped.extend(skipped)
    with open(map_path, "w") as fh:
        json.dump({
            "renames": all_renames,
            "paper_ids": all_ids,
            "paper_original_ids": all_original,
            "skipped": all_skipped,
            "dropped": dropped_all,
            "folds": fold_map,
        }, fh, indent=2)
    for line in report:
        print("compose: %s" % line)
    for line in dropped_lines:
        print("compose: %s" % line)
    for sid in all_skipped:
        print("compose: %s already merged; skipped" % sid)
    print("compose: %d existing + %d added strategies%s" % (
        len(merged_strats), len(added),
        ", paper_db_file=%s" % legacy["db"] if legacy is not None else ""))
    for f, cfg, renames, new_strats, skipped in fold_blocks:
        if f["id"]:
            print("compose: partition %s owns %s (%d added, %d already merged)" % (
                f["partition"], f["db"], len(new_strats), len(skipped)))

def normalize(doc):
    if isinstance(doc, dict):
        out = {}
        for k, v in doc.items():
            if k in ("id", "storage_strategy_id", "partition", "paper_source") or k.endswith("_explicit"):
                continue
            if k == "replay":
                v = dict((kk, vv) for kk, vv in v.items() if kk != "source_id")
            if k == "notification":
                v = dict((kk, vv) for kk, vv in v.items() if not kk.endswith("_key"))
            if k == "scope_risk" and isinstance(v, dict):
                v = dict((kk, vv) for kk, vv in v.items() if kk != "zero_override_inherits")
            out[k] = normalize(v)
        return out
    if isinstance(doc, list):
        return [normalize(v) for v in doc]
    return doc

def flatten(doc, prefix=""):
    if isinstance(doc, dict):
        out = {}
        for k in sorted(doc):
            out.update(flatten(doc[k], prefix + "." + k if prefix else k))
        return out
    return {prefix: json.dumps(doc, sort_keys=True)}

def index_inspect(path):
    return dict((s["id"], s) for s in load(path) if isinstance(s, dict) and isinstance(s.get("id"), str))

def cmd_diff(staged_path, live_path, plan_path, map_path):
    staged = index_inspect(staged_path)
    live = index_inspect(live_path)
    m = load(map_path)
    plan = load(plan_path)
    inspects = dict((f["key"], f["inspect"]) for f in plan["folds"])
    problems = []
    checked = 0
    for orig, before in sorted(live.items()):
        after = staged.get(orig)
        if after is None:
            problems.append("live strategy %s is missing from the staged config" % orig)
            continue
        checked += compare(orig, before, after, problems)
    for fold in m["folds"]:
        source = index_inspect(inspects[fold["key"]])
        for orig in fold["original_ids"]:
            before = source.get(orig)
            new_id = fold["renames"].get(orig, orig)
            after = staged.get(new_id)
            if before is None or after is None:
                problems.append("paper strategy %s (staged as %s) is missing from an inspect document" % (orig, new_id))
                continue
            checked += compare("%s->%s" % (orig, new_id), before, after, problems)
            label = "%s->%s" % (orig, new_id)
            if after.get("partition") != fold["partition"]:
                problems.append("%s partition: staged=%s want=%s" % (label, after.get("partition"), fold["partition"]))
            if (after.get("paper_source") or "") != fold["id"]:
                problems.append("%s paper_source: staged=%s want=%s" % (label, after.get("paper_source"), fold["id"] or "(unset)"))
    if problems:
        for p in problems:
            print("REFUSE: %s" % p)
        sys.exit(1)
    print("proof: %d strategies compared, no effective difference" % checked)

def compare(label, before, after, problems):
    a = flatten(normalize(before))
    b = flatten(normalize(after))
    for k in sorted(set(a) | set(b)):
        if a.get(k) != b.get(k):
            problems.append("%s %s: before=%s after=%s" % (label, k, a.get(k, "<absent>"), b.get(k, "<absent>")))
    return 1

def cmd_target_base(root_path, out_path, db_file, status_port):
    root = load(root_path)
    base = json.loads(json.dumps(root))
    base["strategies"] = []
    base["db_file"] = db_file
    base["status_port"] = int(status_port)
    for key in ("paper_db_file", "paper_sources", "leaderboard_summaries"):
        base.pop(key, None)
    write_json_atomic(out_path, base, root_path)

def env_helpers():
    import importlib.util
    path = os.path.join(os.environ["GO_TRADER_SCRIPT_DIR"], "migrate-service-layout.py")
    spec = importlib.util.spec_from_file_location("migrate_service_layout", path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod.parse_env_file, mod.env_file_line

def owner_ids(user, group):
    import grp
    import pwd
    return pwd.getpwnam(user).pw_uid, grp.getgrnam(group).gr_gid

def cmd_env_merge(out_path, new_unit, user, group, *pairs):
    parse_env_file, env_file_line = env_helpers()
    merged = {}
    setters = {}
    problems = []
    instances = []
    for i in range(0, len(pairs), 2):
        instance, path = pairs[i], pairs[i + 1]
        instances.append(instance)
        env = dict(parse_env_file(path)) if os.path.isfile(path) else {}
        own = ("go-trader@%s.service" % instance, "go-trader@%s" % instance)
        for key in sorted(env):
            val = env[key]
            if key == "GO_TRADER_SERVICE" and val.strip() in own:
                val = new_unit
            if key in merged and merged[key] != val:
                problems.append("variable %s differs between %s and %s" % (key, setters[key][0], instance))
                continue
            merged.setdefault(key, val)
            setters.setdefault(key, []).append(instance)
    if problems:
        for p in problems:
            print("REFUSE: %s (values not shown)" % p)
        sys.exit(1)
    for key in sorted(merged):
        missing = [x for x in instances if x not in setters[key]]
        note = "; absent in %s" % ", ".join(missing) if missing else ""
        print("env: %s from %s%s" % (key, ", ".join(setters[key]), note))
    print("env: %d variable(s) for %s" % (len(merged), new_unit))
    if out_path == "-":
        return
    body = "".join(env_file_line(k, merged[k]) for k in sorted(merged))
    uid, gid = owner_ids(user, group)
    tmp = out_path + ".tmp"
    fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "wb") as f:
        f.write(body.encode("utf-8", "surrogateescape"))
    os.chown(tmp, uid, gid)
    os.replace(tmp, out_path)
    if dict(parse_env_file(out_path)) != merged:
        refuse("the written %s does not parse back to the merged variables" % out_path)

def cmd_install_file(src, dest, mode, user, group):
    uid, gid = owner_ids(user, group)
    with open(src, "rb") as f:
        data = f.read()
    tmp = dest + ".tmp"
    fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_EXCL, int(mode, 8))
    with os.fdopen(fd, "wb") as f:
        f.write(data)
    os.chmod(tmp, int(mode, 8))
    os.chown(tmp, uid, gid)
    os.replace(tmp, dest)

def cmd_port_bound(port):
    import errno
    import socket
    for host in ("127.0.0.1", "0.0.0.0"):
        s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        try:
            s.bind((host, int(port)))
        except OSError as exc:
            if exc.errno == errno.EADDRINUSE:
                print("bound")
                return
            raise
        finally:
            s.close()
    print("free")

def main():
    cmd = sys.argv[1]
    args = sys.argv[2:]
    if cmd == "classify":
        cmd_classify(*args)
    elif cmd == "get":
        cmd_get(*args)
    elif cmd == "storage-check":
        cmd_storage_check(*args)
    elif cmd == "partition-count":
        cmd_partition_count(*args)
    elif cmd == "compose":
        cmd_compose(*args)
    elif cmd == "diff":
        cmd_diff(*args)
    elif cmd == "root-diff":
        cmd_root_diff(*args)
    elif cmd == "align":
        cmd_align(*args)
    elif cmd == "target-base":
        cmd_target_base(*args)
    elif cmd == "env-merge":
        cmd_env_merge(*args)
    elif cmd == "install-file":
        cmd_install_file(*args)
    elif cmd == "port-bound":
        cmd_port_bound(*args)
    else:
        refuse("unknown helper command %s" % cmd)

main()
PY
)

py() {
    GO_TRADER_SCRIPT_DIR="$SCRIPT_DIR" MERGE_TARGET_LABEL="$TARGET_WORD" python3 -c "$MERGE_PY" "$@"
}

cfg_get() {
    py get "$1" "$2"
}

unit_state() {
    "$SYSTEMCTL" is-active "$1" 2>/dev/null || true
}

require_units_stopped() {
    local unit state i
    local -a units=("$LIVE_UNIT")
    for i in "${!FOLD_KEY[@]}"; do
        units+=("${FOLD_UNIT[$i]}")
    done
    for unit in "${units[@]}"; do
        state=$(unit_state "$unit")
        case "$state" in
            active|activating|reloading)
                fail "$EXIT_UNIT_ACTIVE" "unit $unit is $state; stop every unit the handoff touches first"
                ;;
        esac
    done
}

run_bin() {
    local deploy="$1" bin="$2"; shift 2
    (
        cd "$deploy" || exit 1
        if [[ -f "$deploy/.env" ]]; then
            set -a
            . "$deploy/.env"
            set +a
        fi
        exec "$bin" "$@"
    )
}

fold_side() {
    local i="$1"
    if [[ -n "${FOLD_ID[$i]}" ]]; then
        printf 'source %s' "${FOLD_ID[$i]}"
    else
        printf 'paper'
    fi
}

write_aligned_fold() {
    local i="$1" aligned="${FOLD_CFG[$i]}.aligned" rc=0 out=""
    out=$(py align "$COMPOSE_BASE_CFG" "${FOLD_CFG[$i]}" "$aligned") || rc=$?
    if [[ "$rc" != "0" ]]; then
        printf '%s\n' "$out" >&2
        fail "$EXIT_COMPOSE_REFUSED" "could not write aligned paper config $aligned"
    fi
    printf '%s\n' "$out"
    echo "align: wrote $aligned (source paper config unchanged)"
    ALIGN_OUT+="$out"$'\n'
    FOLD_COMPOSE_CFG[$i]="$aligned"
}

# The plan file is the one description of the fold table the Python helper
# reads, so the shell and the helper can never disagree on which deployment
# owns which partition.
write_plan() {
    local path="$1" i
    local -a args=()
    for i in "${!FOLD_KEY[@]}"; do
        args+=("${FOLD_KEY[$i]}" "${FOLD_ID[$i]}" "${FOLD_INSTANCE[$i]}" "${FOLD_PARTITION[$i]}" \
               "${FOLD_COMPOSE_CFG[$i]}" "${FOLD_DB[$i]}" "${FOLD_INSPECT[$i]}" "${FOLD_MERGED[$i]}")
    done
    python3 -c '
import json
import sys
path, live, new_target, root_key = sys.argv[1:5]
rest = sys.argv[5:]
folds = []
for i in range(0, len(rest), 8):
    key, sid, instance, partition, config, db, inspect, merged = rest[i:i + 8]
    folds.append({"key": key, "id": sid, "instance": instance, "partition": partition,
                  "config": config, "db": db, "inspect": inspect, "merged": merged})
json.dump({"live_config": live, "new_target": new_target == "1", "root_key": root_key, "folds": folds},
          open(path, "w"), indent=2)
' "$path" "$COMPOSE_BASE_CFG" "$NT" "${FOLD_KEY[$ROOT_IDX]}" "${args[@]}"
}

classify_field() {
    printf '%s' "$1" | python3 -c 'import json,sys; v = json.load(sys.stdin)[sys.argv[1]]; print("" if v is None else v)' "$2"
}

TOOL_DEPLOY="$LIVE_DEPLOY"
TOOL_BIN="$LIVE_BIN"
if [[ "$NT" == "1" ]]; then
    [[ "$BASE" == /* && "$DEPLOY_ROOT" == /* ]] || fail "$EXIT_USAGE" "--new-target needs an absolute --base and --deploy-root; the new config names its database by absolute path"
    ROOT_CFG="${FOLD_CFG[$ROOT_IDX]}"
    COMPOSE_BASE_CFG="$WORK/target-base.json"
    if [[ "$MODE" == "rollback" && ! -f "$ROOT_CFG" ]]; then
        printf '{"db_file": "%s", "strategies": []}\n' "${TARGET_DIR}/state.db" > "$COMPOSE_BASE_CFG"
    else
        [[ -f "$ROOT_CFG" ]] || fail "$EXIT_CONFIG_MISSING" "config $ROOT_CFG is missing"
        if ! py target-base "$ROOT_CFG" "$COMPOSE_BASE_CFG" "${TARGET_DIR}/state.db" "$STATUS_PORT" >/dev/null; then
            fail "$EXIT_CONFIG_MISSING" "could not read the root config $ROOT_CFG"
        fi
    fi
    TOOL_DEPLOY="${FOLD_DEPLOY[0]}"
    TOOL_BIN="${FOLD_BIN[0]}"
else
    [[ -f "$LIVE_CFG" ]] || fail "$EXIT_CONFIG_MISSING" "config $LIVE_CFG is missing"
    COMPOSE_BASE_CFG="$LIVE_CFG"
fi
live_class=$(py classify "$COMPOSE_BASE_CFG")
live_cv=$(classify_field "$live_class" config_version)
live_db_rel=$(classify_field "$live_class" db_file)
live_paper_db=$(classify_field "$live_class" paper_db_file)
LIVE_DB=$(update_resolve_config_db_path "$LIVE_DEPLOY" "$live_db_rel")
LIVE_DB_CANON=$(update_canonical_db_path "$LIVE_DB")
live_declared_sources=$(printf '%s' "$live_class" | python3 -c 'import json,sys; print(json.dumps(json.load(sys.stdin)["paper_sources"]))')
live_declared_db() {
    printf '%s' "$live_declared_sources" | python3 -c 'import json,sys; print(json.load(sys.stdin).get(sys.argv[1], ""))' "$1"
}
live_declared_ids() {
    printf '%s' "$live_declared_sources" | python3 -c 'import json,sys; print("\n".join(sorted(json.load(sys.stdin))))'
}
live_paper_canon=""
if [[ -n "$live_paper_db" ]]; then
    live_paper_canon=$(update_canonical_db_path "$(update_resolve_config_db_path "$LIVE_DEPLOY" "$live_paper_db")")
fi

# Preflight owns the repeat-run verdict and carries it in the plan file, so the
# Python helper never re-derives it from a raw config string that a symlink or a
# mount can spell differently than the canonical path.
fold_merged_flag() {
    local i="$1" declared canon
    if [[ -n "${FOLD_ID[$i]}" ]]; then
        declared=$(live_declared_db "${FOLD_ID[$i]}")
        [[ -n "$declared" ]] || { printf '0'; return 0; }
        canon=$(update_canonical_db_path "$(update_resolve_config_db_path "$LIVE_DEPLOY" "$declared")")
    else
        [[ -n "$live_paper_canon" ]] || { printf '0'; return 0; }
        canon="$live_paper_canon"
    fi
    if [[ -n "${FOLD_DB[$i]}" && "$canon" == "${FOLD_DB[$i]}" ]]; then
        printf '1'
    else
        printf '0'
    fi
}

ALIGN_OUT=""
if [[ "$MODE" == "diff" ]]; then
    for i in "${!FOLD_KEY[@]}"; do
        [[ -f "${FOLD_CFG[$i]}" ]] || fail "$EXIT_CONFIG_MISSING" "config ${FOLD_CFG[$i]} is missing"
    done
    echo "merge-paper-instance: $TARGET_WORD=$LIVE ($LIVE_CFG) folding $FOLD_COUNT_TOTAL deployment(s) mode=diff"
    [[ "$NT" != "1" ]] || echo "  new target: root settings from ${FOLD_INSTANCE[$ROOT_IDX]} ($ROOT_CFG), status_port $STATUS_PORT, db_file ${TARGET_DIR}/state.db"
    for i in "${!FOLD_KEY[@]}"; do
        echo "  fold: $(fold_desc "$i") config ${FOLD_CFG[$i]}"
        FOLD_COMPOSE_CFG[$i]="${FOLD_CFG[$i]}"
        FOLD_DB[$i]=""
        if fold_class=$(py classify "${FOLD_CFG[$i]}"); then
            fold_db_rel=$(classify_field "$fold_class" db_file)
            if [[ -n "$fold_db_rel" ]]; then
                FOLD_DB[$i]=$(update_canonical_db_path "$(update_resolve_config_db_path "${FOLD_DEPLOY[$i]}" "$fold_db_rel")")
            fi
        fi
    done
    for i in "${!FOLD_KEY[@]}"; do
        FOLD_MERGED[$i]=$(fold_merged_flag "$i")
    done
    write_plan "$WORK/plan.json"
    if ! py root-diff "$COMPOSE_BASE_CFG" "$WORK/plan.json"; then
        fail "$EXIT_CONFIG_MISSING" "could not read the live and paper configs for --diff"
    fi
    if [[ "$ALIGN_TO_LIVE" == "1" ]]; then
        for i in "${!FOLD_KEY[@]}"; do
            write_aligned_fold "$i"
        done
    fi
    exit 0
fi

echo "merge-paper-instance: $TARGET_WORD=$LIVE ($LIVE_DEPLOY, $LIVE_CFG) folding $FOLD_COUNT_TOTAL deployment(s) mode=$MODE"
for i in "${!FOLD_KEY[@]}"; do
    echo "  fold: $(fold_desc "$i") deploy ${FOLD_DEPLOY[$i]} config ${FOLD_CFG[$i]} unit ${FOLD_UNIT[$i]}"
done

TEMPLATE_UNIT="${UNIT_DIR%/}/go-trader@.service"
NT_USER=""
NT_GROUP=""
NT_NAMESPACE=""
NT_JOURNALD=""

nt_unit_prop() {
    "$SYSTEMCTL" show -p "$2" --value "$1" 2>/dev/null || true
}

nt_require_target_absent() {
    local unit_file="${UNIT_DIR%/}/${LIVE_UNIT}" state enabled
    [[ ! -e "$LIVE_DEPLOY" && ! -L "$LIVE_DEPLOY" ]] || fail "$EXIT_NEW_TARGET_REFUSED" "deployment directory $LIVE_DEPLOY already exists; choose another --new-target name or remove it by hand"
    [[ ! -e "$TARGET_DIR" && ! -L "$TARGET_DIR" ]] || fail "$EXIT_NEW_TARGET_REFUSED" "config directory $TARGET_DIR already exists and holds no journal of this fold set; choose another --new-target name or remove it by hand"
    [[ ! -e "$unit_file" && ! -L "$unit_file" ]] || fail "$EXIT_NEW_TARGET_REFUSED" "unit file $unit_file already exists"
    [[ ! -e "${unit_file}.d" ]] || fail "$EXIT_NEW_TARGET_REFUSED" "drop-in directory ${unit_file}.d already exists"
    state=$(unit_state "$LIVE_UNIT")
    case "$state" in
        active|activating|reloading|deactivating|refreshing|failed)
            fail "$EXIT_NEW_TARGET_REFUSED" "unit $LIVE_UNIT is $state; systemd already runs or ran a service with this name"
            ;;
    esac
    enabled=$("$SYSTEMCTL" is-enabled "$LIVE_UNIT" 2>/dev/null || true)
    case "$enabled" in
        enabled|enabled-runtime|linked|linked-runtime|masked|masked-runtime|alias)
            fail "$EXIT_NEW_TARGET_REFUSED" "unit $LIVE_UNIT is $enabled; systemd already loads a service with this name"
            ;;
    esac
}

nt_require_template_layout() {
    local i frag user group ns src
    for i in "${!FOLD_KEY[@]}"; do
        frag=$(nt_unit_prop "${FOLD_UNIT[$i]}" FragmentPath)
        [[ "$frag" == "$TEMPLATE_UNIT" ]] || \
            fail "$EXIT_NEW_TARGET_REFUSED" "${FOLD_UNIT[$i]} loads from '${frag:-<none>}', not from $TEMPLATE_UNIT; move it to the template layout first (scripts/migrate-service-layout.py)"
        user=$(nt_unit_prop "${FOLD_UNIT[$i]}" User)
        group=$(nt_unit_prop "${FOLD_UNIT[$i]}" Group)
        [[ -n "$user" ]] || fail "$EXIT_NEW_TARGET_REFUSED" "${FOLD_UNIT[$i]} names no service user"
        if [[ -z "$NT_USER" ]]; then
            NT_USER="$user"
            NT_GROUP="$group"
        elif [[ "$user" != "$NT_USER" || "$group" != "$NT_GROUP" ]]; then
            fail "$EXIT_NEW_TARGET_REFUSED" "service users differ: ${FOLD_UNIT[0]} runs as $NT_USER:${NT_GROUP:-<default>}, ${FOLD_UNIT[$i]} as $user:${group:-<default>}; the new service runs as one user"
        fi
    done
    if [[ -z "$NT_GROUP" ]]; then
        NT_GROUP=$(id -gn "$NT_USER" 2>/dev/null) || fail "$EXIT_NEW_TARGET_REFUSED" "service user $NT_USER does not exist on this host"
    fi
    id -u "$NT_USER" >/dev/null 2>&1 || fail "$EXIT_NEW_TARGET_REFUSED" "service user $NT_USER does not exist on this host"
    src="${FOLD_DEPLOY[0]}/systemd/go-trader@.service"
    [[ -f "$src" ]] || fail "$EXIT_NEW_TARGET_REFUSED" "$src is missing; the release ships the template unit"
    if [[ -e "$TEMPLATE_UNIT" ]] && ! cmp -s "$src" "$TEMPLATE_UNIT"; then
        fail "$EXIT_NEW_TARGET_REFUSED" "installed $TEMPLATE_UNIT differs from the release's $src; run scripts/update.sh --restart in a folded deployment first"
    fi
    ns=$(update_unit_log_namespace "$src")
    if [[ -n "$ns" ]]; then
        [[ "$ns" =~ ^[A-Za-z0-9_-]+$ ]] || fail "$EXIT_NEW_TARGET_REFUSED" "LogNamespace '$ns' in $src is not a plain name"
        NT_NAMESPACE="$ns"
        NT_JOURNALD=$(update_journald_conf_path "$JOURNALD_ETC" "$ns")
        [[ -f "${FOLD_DEPLOY[0]}/systemd/journald@${ns}.conf" ]] || fail "$EXIT_NEW_TARGET_REFUSED" "${FOLD_DEPLOY[0]}/systemd/journald@${ns}.conf is missing; the unit logs to namespace $ns"
        if [[ -e "$NT_JOURNALD" ]] && ! cmp -s "${FOLD_DEPLOY[0]}/systemd/journald@${ns}.conf" "$NT_JOURNALD"; then
            fail "$EXIT_NEW_TARGET_REFUSED" "installed $NT_JOURNALD differs from the release's copy; run scripts/update.sh --restart in a folded deployment first"
        fi
    fi
}

nt_env_merge() {
    local out="$1" i
    local -a pairs=()
    for i in "${!FOLD_KEY[@]}"; do
        pairs+=("${FOLD_INSTANCE[$i]}" "${FOLD_DEPLOY[$i]}/.env")
    done
    py env-merge "$out" "$LIVE_UNIT" "$NT_USER" "$NT_GROUP" "${pairs[@]}"
}

nt_git() {
    git -c safe.directory='*' "$@"
}

NT_ORIGIN=""
NT_SRC_SHA=""
NT_SRC_BRANCH=""
nt_preflight() {
    local out rc=0
    nt_require_target_absent
    nt_require_template_layout
    NT_ORIGIN=$(nt_git -C "${FOLD_DEPLOY[0]}" remote get-url origin 2>/dev/null || true)
    [[ -n "$NT_ORIGIN" ]] || fail "$EXIT_NEW_TARGET_REFUSED" "${FOLD_DEPLOY[0]} has no git origin; the new deployment clones it so update.sh can update it later"
    NT_SRC_SHA=$(nt_git -C "${FOLD_DEPLOY[0]}" rev-parse HEAD 2>/dev/null || true)
    NT_SRC_BRANCH=$(nt_git -C "${FOLD_DEPLOY[0]}" symbolic-ref --quiet --short HEAD 2>/dev/null || true)
    [[ "$(py port-bound "$STATUS_PORT")" == "free" ]] || fail "$EXIT_NEW_TARGET_REFUSED" "status port $STATUS_PORT is bound by a running process; choose a free --status-port"
    out=$(nt_env_merge -) || rc=$?
    if [[ "$rc" != "0" ]]; then
        printf '%s\n' "$out" >&2
        fail "$EXIT_NEW_TARGET_REFUSED" "the folded deployments' .env files disagree; make every variable agree first"
    fi
    printf '%s\n' "$out"
    echo "preflight: new target $LIVE_UNIT runs as $NT_USER:$NT_GROUP; tree $LIVE_DEPLOY cloned from $NT_ORIGIN and built from ${FOLD_DEPLOY[0]}; config $LIVE_CFG; status_port $STATUS_PORT"
}

nt_jv() {
    grep "^$1 " "$JOURNAL" | tail -n 1 | cut -d' ' -f2- || true
}

nt_journal_state() {
    local matches=1 i
    [[ -f "$JOURNAL" ]] || return 0
    if ! grep -qx complete "$JOURNAL"; then
        fail "$EXIT_JOURNAL_STATE" "journal $JOURNAL records an interrupted apply; run --rollback --new-target $LIVE with the same fold arguments before a new run"
    fi
    [[ "$(update_file_fingerprint "$LIVE_CFG")" == "$(nt_jv result_config)" ]] || matches=0
    for i in "${!FOLD_KEY[@]}"; do
        [[ "$(update_file_fingerprint "${FOLD_DROPIN[$i]}")" == "$(nt_jv "result_override${FOLD_SFX[$i]}")" ]] || matches=0
    done
    [[ "$matches" == "1" ]] || fail "$EXIT_JOURNAL_STATE" "journal $JOURNAL is complete but $LIVE_CFG or a drop-in changed since; inspect by hand"
    echo "journal: $JOURNAL is complete and every file matches its result; nothing to do. Run the printed cutover if it has not run yet."
    exit 0
}

# A rollback restores the retained config and drop-ins. It needs the journal, the
# retained files and the database locks, and nothing else: the post-apply notes
# tell the operator to retire the folded deployments, so demanding them back
# would make the undo impossible exactly when it is wanted.
live_version=""
if [[ "$MODE" != "rollback" ]]; then
    [[ "$NT" != "1" ]] || nt_journal_state
    [[ "$NT" == "1" ]] || [[ -d "$LIVE_DEPLOY" ]] || fail "$EXIT_DEPLOY_MISSING" "deployment directory $LIVE_DEPLOY is missing"
    for i in "${!FOLD_KEY[@]}"; do
        [[ -d "${FOLD_DEPLOY[$i]}" ]] || fail "$EXIT_DEPLOY_MISSING" "deployment directory ${FOLD_DEPLOY[$i]} is missing"
    done
    [[ -x "$TOOL_BIN" ]] || fail "$EXIT_DEPLOY_MISSING" "binary $TOOL_BIN is missing or not executable"
    for i in "${!FOLD_KEY[@]}"; do
        [[ -x "${FOLD_BIN[$i]}" ]] || fail "$EXIT_DEPLOY_MISSING" "binary ${FOLD_BIN[$i]} is missing or not executable"
    done
    live_version=$(run_bin "$TOOL_DEPLOY" "$TOOL_BIN" version 2>/dev/null || true)
    [[ -n "$live_version" ]] || fail "$EXIT_VERSION_MISMATCH" "the $TARGET_WORD binary $TOOL_BIN reports no version; update both deployments to one release first"
    for i in "${!FOLD_KEY[@]}"; do
        fold_version=$(run_bin "${FOLD_DEPLOY[$i]}" "${FOLD_BIN[$i]}" version 2>/dev/null || true)
        [[ "$live_version" == "$fold_version" ]] || \
            fail "$EXIT_VERSION_MISMATCH" "binary versions differ: $TARGET_WORD='$live_version' $(fold_side "$i")='$fold_version'; update both deployments to one release first"
    done
    for i in "${!FOLD_KEY[@]}"; do
        [[ -f "${FOLD_CFG[$i]}" ]] || fail "$EXIT_CONFIG_MISSING" "config ${FOLD_CFG[$i]} is missing"
    done
    [[ "$NT" != "1" ]] || nt_preflight
fi
require_units_stopped
LIVE_CFG_COPY="$WORK/live-config.json"
fp_live_cfg=$(update_file_fingerprint "$COMPOSE_BASE_CFG")
if [[ "$MODE" != "rollback" ]]; then
    cp "$COMPOSE_BASE_CFG" "$LIVE_CFG_COPY"
    for i in "${!FOLD_KEY[@]}"; do
        FOLD_CFG_COPY[$i]="$WORK/fold-${FOLD_KEY[$i]}-config.json"
        cp "${FOLD_CFG[$i]}" "${FOLD_CFG_COPY[$i]}"
        FOLD_FP_CFG[$i]=$(update_file_fingerprint "${FOLD_CFG[$i]}")
    done
fi

config_copy_intact() {
    local side="$1" copy="$2" want="$3" what="$4"
    if [[ "$(update_file_fingerprint "$copy")" != "$want" ]]; then
        fail "$EXIT_CONFIG_MIGRATION" "the $side binary rewrote its config while running $what (a config migration is pending); this release inspects read-only, so update the $side deployment to it, or start the unit once as the service user to migrate the file, then re-run"
    fi
}

probe_side() {
    local side="$1" deploy="$2" bin="$3" cfg="$4" want="$5" tag="$6" layout
    run_bin "$deploy" "$bin" storage-inspect --json --config "$cfg" >"$WORK/probe-$tag.json" 2>"$WORK/probe-$tag.err" || true
    config_copy_intact "$side" "$cfg" "$want" "storage-inspect"
    if [[ ! -s "$WORK/probe-$tag.json" ]]; then
        cat "$WORK/probe-$tag.err" >&2
        if grep -q "failed to load config" "$WORK/probe-$tag.err"; then
            fail "$EXIT_INSPECTION_REFUSED" "$side binary refuses to load $cfg; fix the config errors above first"
        fi
        fail "$EXIT_BINARY_INCOMPATIBLE" "$side binary cannot inspect its storage layout (storage-inspect --json produced no report)"
    fi
    if ! layout=$(cfg_get "$WORK/probe-$tag.json" layout 2>/dev/null) || [[ -z "$layout" ]]; then
        cat "$WORK/probe-$tag.err" >&2
        fail "$EXIT_BINARY_INCOMPATIBLE" "$side binary's storage-inspect --json carries no layout; a release with the early ownership-lock contract is required"
    fi
}

if [[ "$MODE" != "rollback" ]]; then
    probe_side "$TARGET_WORD" "$TOOL_DEPLOY" "$TOOL_BIN" "$LIVE_CFG_COPY" "$fp_live_cfg" live
    for i in "${!FOLD_KEY[@]}"; do
        probe_side "$(fold_side "$i")" "${FOLD_DEPLOY[$i]}" "${FOLD_BIN[$i]}" "${FOLD_CFG_COPY[$i]}" "${FOLD_FP_CFG[$i]}" "fold-${FOLD_KEY[$i]}"
    done
fi

for i in "${!FOLD_KEY[@]}"; do
    side=$(fold_side "$i")
    if [[ ! -f "${FOLD_CFG[$i]}" ]]; then
        echo "rollback: $side config ${FOLD_CFG[$i]} is gone; its database is neither locked nor fingerprinted by this run"
        continue
    fi
    fold_class=$(py classify "${FOLD_CFG[$i]}")
    fold_db_rel=$(classify_field "$fold_class" db_file)
    FOLD_DB[$i]=$(update_canonical_db_path "$(update_resolve_config_db_path "${FOLD_DEPLOY[$i]}" "$fold_db_rel")")
    FOLD_PORT[$i]=$(classify_field "$fold_class" status_port)
    FOLD_COUNT[$i]=$(classify_field "$fold_class" strategy_count)
    if [[ "$MODE" == "rollback" && ! -f "${FOLD_DB[$i]}" ]]; then
        # A retired deployment can take its database directory with it while the
        # config under $BASE outlives it. There is no book to protect and no
        # directory to create the lock file in, so cover neither.
        echo "rollback: $side database ${FOLD_DB[$i]} is gone; it is neither locked nor fingerprinted by this run"
        FOLD_DB[$i]=""
        continue
    fi
    [[ "$MODE" != "rollback" ]] || continue
    fold_live_count=$(classify_field "$fold_class" live)
    [[ "$fold_live_count" == "0" ]] || fail "$EXIT_PAPER_NOT_PAPER" "$side config runs $fold_live_count live strategy(ies); every strategy must be paper"
    [[ "${FOLD_COUNT[$i]}" != "0" ]] || fail "$EXIT_PAPER_NOT_PAPER" "$side config has no strategies"
    fold_cv=$(classify_field "$fold_class" config_version)
    [[ -n "$live_cv" && "$live_cv" == "$fold_cv" ]] || \
        fail "$EXIT_CONFIG_MIGRATION" "config_version differs: live='$live_cv' $side='$fold_cv'; start each unit once on the current release so both files carry one version, then re-run"
    fold_nested=$(classify_field "$fold_class" nested_paper_risk)
    [[ "$fold_nested" == "False" ]] || fail "$EXIT_PAPER_NOT_PAPER" "$side config nests portfolio_risk.paper"
    fold_split=$(classify_field "$fold_class" paper_db_file)
    [[ -z "$fold_split" ]] || fail "$EXIT_DB_IDENTITY" "$side config already sets paper_db_file=$fold_split; the handoff handles one primary file per side"
    fold_sources=$(printf '%s' "$fold_class" | python3 -c 'import json,sys; print(" ".join(sorted(json.load(sys.stdin)["paper_sources"])))')
    [[ -z "$fold_sources" ]] || fail "$EXIT_DB_IDENTITY" "$side config already declares paper_sources ($fold_sources); hand off one deployment per source, never a deployment that folded sources of its own"
    [[ "${FOLD_DB[$i]}" != "$LIVE_DB_CANON" ]] || fail "$EXIT_DB_IDENTITY" "$side db_file resolves to the live db_file ($LIVE_DB_CANON)"
    for j in "${!FOLD_KEY[@]}"; do
        [[ "$j" -lt "$i" ]] || continue
        [[ "${FOLD_DB[$j]}" != "${FOLD_DB[$i]}" ]] || \
            fail "$EXIT_DB_IDENTITY" "$side and $(fold_side "$j") resolve to the same database (${FOLD_DB[$i]}); every partition needs its own file"
    done
    [[ -f "${FOLD_DB[$i]}" ]] || fail "$EXIT_DB_IDENTITY" "$side database ${FOLD_DB[$i]} is absent; there is no book to move"
    if [[ -n "${FOLD_ID[$i]}" ]]; then
        declared=$(live_declared_db "${FOLD_ID[$i]}")
        if [[ -n "$declared" ]]; then
            declared_canon=$(update_canonical_db_path "$(update_resolve_config_db_path "$LIVE_DEPLOY" "$declared")")
            [[ "$declared_canon" == "${FOLD_DB[$i]}" ]] || \
                fail "$EXIT_LIVE_PAPER_DB_CONFLICT" "live config already declares paper source ${FOLD_ID[$i]} with db_file=$declared, which is not the ${FOLD_INSTANCE[$i]} instance's database (${FOLD_DB[$i]})"
            echo "preflight: live config already declares paper source ${FOLD_ID[$i]} (repeat run)"
        fi
        [[ -z "$live_paper_canon" || "$live_paper_canon" != "${FOLD_DB[$i]}" ]] || \
            fail "$EXIT_LIVE_PAPER_DB_CONFLICT" "live config already owns ${FOLD_DB[$i]} as paper_db_file; the default paper partition and source ${FOLD_ID[$i]} cannot share one file"
    else
        if [[ -n "$live_paper_canon" ]]; then
            [[ "$live_paper_canon" == "${FOLD_DB[$i]}" ]] || \
                fail "$EXIT_LIVE_PAPER_DB_CONFLICT" "live config already sets paper_db_file=$live_paper_db, which is not the ${FOLD_INSTANCE[$i]} instance's database (${FOLD_DB[$i]})"
            echo "preflight: live config already references the paper database (repeat run)"
        fi
    fi
    while IFS= read -r other_id; do
        [[ -n "$other_id" ]] || continue
        [[ "$other_id" != "${FOLD_ID[$i]}" ]] || continue
        other_db=$(live_declared_db "$other_id")
        [[ -n "$other_db" ]] || continue
        other_canon=$(update_canonical_db_path "$(update_resolve_config_db_path "$LIVE_DEPLOY" "$other_db")")
        [[ "$other_canon" != "${FOLD_DB[$i]}" ]] || \
            fail "$EXIT_LIVE_PAPER_DB_CONFLICT" "live config already declares paper source $other_id on ${FOLD_DB[$i]}; that file cannot also own $(fold_side "$i")"
    done < <(live_declared_ids)
done

has_legacy=0
for i in "${!FOLD_KEY[@]}"; do
    [[ -n "${FOLD_ID[$i]}" ]] || has_legacy=1
done
if [[ -n "$live_paper_canon" && "$has_legacy" == "0" ]]; then
    echo "preflight: live config keeps its own paper_db_file ($live_paper_canon); this run only adds source partitions"
fi

for i in "${!FOLD_KEY[@]}"; do
    FOLD_MERGED[$i]=$(fold_merged_flag "$i")
done

fold_db_list=""
declare -a LOCK_DBS=()
if [[ "$NT" != "1" || -f "$LIVE_DB_CANON" ]]; then
    LOCK_DBS+=("$LIVE_DB_CANON")
fi
for i in "${!FOLD_KEY[@]}"; do
    [[ -n "${FOLD_DB[$i]}" ]] || continue
    fold_db_list+=" $(fold_side "$i")=${FOLD_DB[$i]}"
    LOCK_DBS+=("${FOLD_DB[$i]}")
done
echo "preflight: versions=$live_version ${TARGET_WORD}_db=$LIVE_DB_CANON$fold_db_list"

HOLDER_PID=""
if [[ ${#LOCK_DBS[@]} -gt 0 ]]; then
    if ! update_start_state_lock_holder "${LOCK_DBS[@]}"; then
        fail "$EXIT_LOCK_CONTENDED" "a database lock is held by another process; see the CONTENDED line above"
    fi
    HOLDER_PID="$UPDATE_LOCK_HOLDER_PID"
    lock_list=""
    for db in "${LOCK_DBS[@]}"; do
        lock_list+=$(update_state_lock_paths "$db" | tr '\n' ' ')
    done
    echo "locks: held by pid $HOLDER_PID on $lock_list"
fi

fp_live_db=$(update_db_fingerprint "$LIVE_DB_CANON" | tr '\n' ' ')
for i in "${!FOLD_KEY[@]}"; do
    [[ -n "${FOLD_DB[$i]}" ]] || continue
    FOLD_FP_DB[$i]=$(update_db_fingerprint "${FOLD_DB[$i]}" | tr '\n' ' ')
done

check_db_fingerprints() {
    local stage="$1" now changed="" i
    now=$(update_db_fingerprint "$LIVE_DB_CANON" | tr '\n' ' ')
    if [[ "$now" != "$fp_live_db" ]]; then
        changed+=" live before=[$fp_live_db] after=[$now]"
    fi
    for i in "${!FOLD_KEY[@]}"; do
        [[ -n "${FOLD_DB[$i]}" ]] || continue
        now=$(update_db_fingerprint "${FOLD_DB[$i]}" | tr '\n' ' ')
        if [[ "$now" != "${FOLD_FP_DB[$i]}" ]]; then
            changed+=" $(fold_side "$i") before=[${FOLD_FP_DB[$i]}] after=[$now]"
        fi
    done
    if [[ -n "$changed" ]]; then
        echo "CRITICAL: a database changed during $stage:$changed" >&2
        return 1
    fi
    echo "$stage: database fingerprints unchanged"
}

journal_has() {
    [[ -f "$JOURNAL" ]] && grep -qx "$1" "$JOURNAL"
}

journal_value() {
    [[ -f "$JOURNAL" ]] || return 0
    grep "^$1 " "$JOURNAL" | tail -n 1 | cut -d' ' -f2- || true
}

# A release before this one recorded no fold lines. Such a journal describes
# exactly one fold, whose key is in its file name and whose partition is always
# paper, and it named its retained drop-in copy from that key alone.
journal_is_legacy() {
    [[ -f "$1" ]] && ! grep -q '^fold ' "$1"
}

journal_legacy_key() {
    local base="${1##*/}"
    base="${base#merge-paper-}"
    printf '%s' "${base%.journal}"
}

archive_if_edited() {
    local path="$1" label="$2"; shift 2
    local current known
    [[ -e "$path" ]] || return 0
    current=$(update_file_fingerprint "$path")
    for known in "$@"; do
        [[ -n "$known" && "$current" == "$known" ]] && return 0
    done
    local archive="${path}.merge-edited.$(date +%Y%m%d%H%M%S)"
    if cp -p "$path" "$archive"; then
        echo "restore: $label $path no longer matches what the apply installed or found; copy kept at $archive" >&2
        return 0
    fi
    echo "CRITICAL: $label $path was edited after the apply and could not be archived to $archive" >&2
    return 1
}

restore_from_retained() {
    local failed=0 i sfx dropin retained
    if journal_has "config done" || journal_has "config begin"; then
        archive_if_edited "$LIVE_CFG" "config" "$(journal_value live_config)" "$(journal_value staged_config)" "$(journal_value result_config)" || failed=1
        if [[ "$failed" == "1" ]]; then
            :
        elif [[ -f "$RETAINED_CFG" ]]; then
            if mv -f "$RETAINED_CFG" "$LIVE_CFG"; then
                echo "restore: $LIVE_CFG restored from $RETAINED_CFG"
            else
                echo "CRITICAL: could not restore $LIVE_CFG from $RETAINED_CFG" >&2
                failed=1
            fi
        elif journal_has "config done"; then
            echo "CRITICAL: retained copy $RETAINED_CFG is missing; $LIVE_CFG holds the merged config" >&2
            failed=1
        fi
    fi
    for i in "${!FOLD_KEY[@]}"; do
        sfx="${FOLD_SFX[$i]}"
        dropin="${FOLD_DROPIN[$i]}"
        retained="${FOLD_RETAINED_DROPIN[$i]}"
        if [[ ! -f "$retained" ]] && journal_is_legacy "$JOURNAL" && [[ -f "${FOLD_LEGACY_RETAINED_DROPIN[$i]}" ]]; then
            retained="${FOLD_LEGACY_RETAINED_DROPIN[$i]}"
        fi
        journal_has "override done${sfx}" || journal_has "override begin${sfx}" || continue
        archive_if_edited "$dropin" "override" "$(journal_value "override_prior_fp${sfx}")" "$(journal_value "staged_override${sfx}")" "$(journal_value "result_override${sfx}")" || { failed=1; continue; }
        if journal_has "override_prior${sfx} absent"; then
            if [[ -e "$dropin" ]]; then
                if rm -f "$dropin"; then
                    echo "restore: $dropin removed (absent before the merge)"
                else
                    echo "CRITICAL: could not remove $dropin" >&2
                    failed=1
                fi
            fi
        elif [[ -f "$retained" ]]; then
            if mv -f "$retained" "$dropin"; then
                echo "restore: $dropin restored from $retained"
            else
                echo "CRITICAL: could not restore $dropin" >&2
                failed=1
            fi
        elif journal_has "override done${sfx}"; then
            echo "CRITICAL: retained copy $retained is missing; $dropin holds the merge override" >&2
            failed=1
        fi
    done
    if [[ "$failed" == "1" ]]; then
        echo "state: config=$LIVE_CFG retained=$RETAINED_CFG journal=$JOURNAL" >&2
        for i in "${!FOLD_KEY[@]}"; do
            echo "state: $(fold_side "$i") dropin=${FOLD_DROPIN[$i]} retained=${FOLD_RETAINED_DROPIN[$i]}" >&2
        done
        return 1
    fi
    printf 'rolled-back\n' >> "$JOURNAL"
    return 0
}

# Every merge for this live instance leaves one journal beside the live config.
# A run that reads only its own journal cannot see a half-applied merge under
# another fold set, and cannot see that an applied merge already owns one of the
# drop-in names this run would write.
merge_journals() {
    local j
    shopt -s nullglob
    for j in "${BASE%/}/${LIVE}"/merge-paper-*.journal; do
        printf '%s\n' "$j"
    done
    shopt -u nullglob
}

journal_value_in() {
    grep "^$2 " "$1" | tail -n 1 | cut -d' ' -f2- || true
}

journal_fold_partition() {
    if journal_is_legacy "$1"; then
        [[ "$(journal_legacy_key "$1")" == "$2" ]] || return 0
        printf 'paper'
        return 0
    fi
    awk -v k="$2" '$1 == "fold" && $2 == k { print $3 }' "$1" | tail -n 1
}

# An unfinished apply under another fold set can own the live config and some of
# its drop-ins at once. Every mode reads it, rollback included: restoring over a
# half-applied merge strands that run's journal and its installed drop-ins.
scan_other_journals_interrupted() {
    local action="$1" j
    while IFS= read -r j; do
        [[ -n "$j" && "$j" != "$JOURNAL" ]] || continue
        grep -qx rolled-back "$j" && continue
        grep -qx complete "$j" && continue
        fail "$EXIT_JOURNAL_STATE" "journal $j records an interrupted apply; $LIVE_CFG may hold its merged config while some of its drop-ins were never installed. Roll that run back (--rollback with its own --paper and --source arguments) or re-apply it before $action"
    done < <(merge_journals)
}

scan_other_journals() {
    local j i owner
    scan_other_journals_interrupted "certifying this one"
    while IFS= read -r j; do
        [[ -n "$j" && "$j" != "$JOURNAL" ]] || continue
        grep -qx rolled-back "$j" && continue
        grep -qx complete "$j" || continue
        for i in "${!FOLD_KEY[@]}"; do
            owner=$(journal_fold_partition "$j" "${FOLD_KEY[$i]}")
            [[ -n "$owner" ]] || continue
            [[ "$owner" != "${FOLD_PARTITION[$i]}" ]] || continue
            fail "$EXIT_JOURNAL_STATE" "journal $j already folded '${FOLD_KEY[$i]}' as partition $owner; this run would hand the same drop-in $(basename "${FOLD_DROPIN[$i]}") to partition ${FOLD_PARTITION[$i]} and leave $owner with no writable-path directive. Name the source differently"
        done
    done < <(merge_journals)
}

# A rollback restores the config this run replaced. When a later merge installed
# the config that is there now, restoring would discard that merge and strand its
# journal, so the newer run is rolled back first.
journal_that_installed_live_config() {
    local j fp
    fp=$(update_file_fingerprint "$LIVE_CFG")
    while IFS= read -r j; do
        [[ -n "$j" && "$j" != "$JOURNAL" ]] || continue
        grep -qx rolled-back "$j" && continue
        grep -qx complete "$j" || continue
        [[ "$(journal_value_in "$j" result_config)" == "$fp" ]] || continue
        printf '%s' "$j"
        return 0
    done < <(merge_journals)
}

nt_undo() {
    local run_id failed=0 i sfx dropin dropin_dir tree_aside dir_aside
    run_id=$(journal_value run_id)
    [[ -n "$run_id" ]] || run_id="$(date +%Y%m%d%H%M%S)-$$"
    tree_aside="${LIVE_DEPLOY}.rolled-back-${run_id}"
    dir_aside="${TARGET_DIR}.rolled-back-${run_id}"
    printf 'rollback begin\n' >> "$JOURNAL"
    for i in "${!FOLD_KEY[@]}"; do
        sfx="${FOLD_SFX[$i]}"
        dropin="${FOLD_DROPIN[$i]}"
        journal_has "override begin${sfx}" || continue
        [[ -e "$dropin" ]] || continue
        case "$(update_file_fingerprint "$dropin")" in
            "$(journal_value "staged_override${sfx}")"|"$(journal_value "result_override${sfx}")")
                if rm -f "$dropin"; then
                    echo "rollback: removed $dropin"
                else
                    echo "CRITICAL: could not remove $dropin" >&2
                    failed=1
                fi
                ;;
            *)
                if mv -f "$dropin" "${TARGET_DIR}/$(basename "$dropin").edited"; then
                    echo "rollback: $dropin was edited after the apply; moved to ${dir_aside}/$(basename "$dropin").edited"
                else
                    echo "CRITICAL: could not move the edited $dropin aside" >&2
                    failed=1
                fi
                ;;
        esac
    done
    dropin_dir="${UNIT_DIR%/}/${LIVE_UNIT}.d"
    if [[ -d "$dropin_dir" ]]; then
        rmdir "$dropin_dir" 2>/dev/null || echo "rollback: $dropin_dir still holds files this run did not write; left in place" >&2
    fi
    if journal_has "template installed"; then
        echo "rollback: $TEMPLATE_UNIT was installed by this run and stays, since other go-trader@ units read it"
    fi
    if journal_has "journald installed"; then
        echo "rollback: $(journal_value journald_path) was installed by this run and stays"
    fi
    if [[ "$failed" == "0" ]] && journal_has "deploy begin" && [[ -e "$LIVE_DEPLOY" || -L "$LIVE_DEPLOY" ]]; then
        if [[ -e "$tree_aside" ]]; then
            echo "CRITICAL: $tree_aside already exists; $LIVE_DEPLOY left in place" >&2
            failed=1
        elif mv "$LIVE_DEPLOY" "$tree_aside"; then
            echo "rollback: moved $LIVE_DEPLOY to $tree_aside"
        else
            echo "CRITICAL: could not move $LIVE_DEPLOY aside" >&2
            failed=1
        fi
    fi
    if [[ "$failed" == "1" ]]; then
        echo "state: journal=$JOURNAL tree=$LIVE_DEPLOY config_dir=$TARGET_DIR" >&2
        return 1
    fi
    if [[ -e "$dir_aside" ]]; then
        echo "CRITICAL: $dir_aside already exists; $TARGET_DIR left in place" >&2
        return 1
    fi
    if ! mv "$TARGET_DIR" "$dir_aside"; then
        echo "CRITICAL: could not move $TARGET_DIR aside" >&2
        return 1
    fi
    echo "rollback: moved $TARGET_DIR to $dir_aside (the new target's state.db, if the service ever ran, stays there)"
    JOURNAL="${dir_aside}/$(basename "$JOURNAL")"
    LIVE_DB_CANON="${dir_aside}/state.db"
    printf 'rolled-back\n' >> "$JOURNAL"
    return 0
}

nt_print_restart() {
    local i
    echo "to run the folded deployments again, in this order:"
    echo "  1. $SYSTEMCTL daemon-reload"
    for i in "${!FOLD_KEY[@]}"; do
        echo "  $((i + 2)). $SYSTEMCTL enable --now ${FOLD_UNIT[$i]}"
    done
    echo "  the folded configs were never changed, so each deployment resumes on its own status port; records the combined service wrote stay in each source database"
}

nt_rollback() {
    local j
    if [[ ! -f "$JOURNAL" ]]; then
        for j in "${TARGET_DIR}".rolled-back-*/"$(basename "$JOURNAL")"; do
            if [[ -f "$j" ]] && grep -qx rolled-back "$j"; then
                echo "rollback: $j records a finished rollback; nothing to do"
                check_db_fingerprints "rollback" || exit "$EXIT_RESTORE_FAILED"
                nt_print_restart
                exit 0
            fi
        done
        if [[ -d "$TARGET_DIR" ]]; then
            for j in "$TARGET_DIR"/merge-paper-*.journal; do
                [[ -f "$j" ]] && echo "rollback: $TARGET_DIR holds $(basename "$j"); roll back with the fold arguments that journal names" >&2
            done
        fi
        fail "$EXIT_JOURNAL_STATE" "no journal at $JOURNAL; nothing to roll back"
    fi
    if ! "$SYSTEMCTL" disable "$LIVE_UNIT" >"$WORK/disable.out" 2>&1; then
        cat "$WORK/disable.out" >&2
        fail "$EXIT_RESTORE_FAILED" "could not disable $LIVE_UNIT; nothing else changed"
    fi
    echo "rollback: $LIVE_UNIT disabled"
    nt_undo || exit "$EXIT_RESTORE_FAILED"
    check_db_fingerprints "rollback" || exit "$EXIT_RESTORE_FAILED"
    echo "rollback: complete; no database was written or restored"
    nt_print_restart
    exit 0
}

nt_apply_failed() {
    echo "apply: $1" >&2
    if nt_undo; then
        check_db_fingerprints "rollback" || exit "$EXIT_RESTORE_FAILED"
        echo "apply: rolled back; every unit stays stopped and every folded deployment is unchanged" >&2
    fi
    exit "$EXIT_RESTORE_FAILED"
}

nt_apply() {
    local run_id i sfx dropin fp_new new_version
    [[ "$(update_file_fingerprint "$COMPOSE_BASE_CFG")" == "$fp_live_cfg" ]] || fail "$EXIT_SOURCE_CHANGED" "$COMPOSE_BASE_CFG changed during the run"
    for i in "${!FOLD_KEY[@]}"; do
        [[ "$(update_file_fingerprint "${FOLD_CFG[$i]}")" == "${FOLD_FP_CFG[$i]}" ]] || fail "$EXIT_SOURCE_CHANGED" "${FOLD_CFG[$i]} changed during the run"
    done
    check_db_fingerprints "pre-apply" || exit "$EXIT_SOURCE_CHANGED"
    require_units_stopped
    nt_require_target_absent

    run_id="$(date +%Y%m%d%H%M%S)-$$"
    mkdir -m 0700 "$TARGET_DIR" || fail "$EXIT_NEW_TARGET_REFUSED" "could not create $TARGET_DIR"
    chown "$NT_USER:$NT_GROUP" "$TARGET_DIR" || { rmdir "$TARGET_DIR"; fail "$EXIT_NEW_TARGET_REFUSED" "could not give $TARGET_DIR to $NT_USER:$NT_GROUP"; }
    {
        printf 'run_id %s\n' "$run_id"
        printf 'new_target %s\n' "$LIVE"
        printf 'service_user %s:%s\n' "$NT_USER" "$NT_GROUP"
        printf 'created %s\n' "$TARGET_DIR"
        printf 'staged_config %s\n' "$fp_staged_cfg"
        printf 'folds %s\n' "$(IFS=' '; printf '%s' "${FOLD_KEY[*]}")"
        for i in "${!FOLD_KEY[@]}"; do
            printf 'fold %s %s %s\n' "${FOLD_KEY[$i]}" "${FOLD_PARTITION[$i]}" "${FOLD_INSTANCE[$i]}"
        done
        for i in "${!FOLD_KEY[@]}"; do
            sfx="${FOLD_SFX[$i]}"
            printf 'source_config%s %s\n' "$sfx" "${FOLD_FP_CFG[$i]}"
            printf 'source_db%s %s\n' "$sfx" "${FOLD_FP_DB[$i]}"
            printf 'staged_override%s %s\n' "$sfx" "$(update_file_fingerprint "${FOLD_STAGED_OVERRIDE[$i]}")"
            printf 'override_prior%s absent\n' "$sfx"
        done
        if [[ -n "$ALIGN_OUT" ]]; then
            printf '%s' "$ALIGN_OUT"
        fi
    } > "$JOURNAL"
    chmod 0600 "$JOURNAL"
    echo "apply: created $TARGET_DIR (journal $JOURNAL)"

    printf 'deploy begin\n' >> "$JOURNAL"
    nt_git clone --quiet "$NT_ORIGIN" "$LIVE_DEPLOY" || nt_apply_failed "git clone of $NT_ORIGIN into $LIVE_DEPLOY failed"
    if [[ -n "$NT_SRC_BRANCH" ]] && nt_git -C "$LIVE_DEPLOY" rev-parse --verify --quiet "refs/remotes/origin/$NT_SRC_BRANCH" >/dev/null; then
        nt_git -C "$LIVE_DEPLOY" checkout --quiet -B "$NT_SRC_BRANCH" --track "origin/$NT_SRC_BRANCH" || nt_apply_failed "could not check out branch $NT_SRC_BRANCH in $LIVE_DEPLOY"
    fi
    if [[ -n "$NT_SRC_SHA" ]] && nt_git -C "$LIVE_DEPLOY" cat-file -e "${NT_SRC_SHA}^{commit}" 2>/dev/null; then
        nt_git -C "$LIVE_DEPLOY" reset --quiet --hard "$NT_SRC_SHA" || nt_apply_failed "could not move $LIVE_DEPLOY to ${FOLD_DEPLOY[0]}'s commit $NT_SRC_SHA"
        echo "apply: cloned $NT_ORIGIN into $LIVE_DEPLOY at ${FOLD_DEPLOY[0]}'s commit $NT_SRC_SHA"
    else
        echo "apply: cloned $NT_ORIGIN into $LIVE_DEPLOY; ${FOLD_DEPLOY[0]}'s commit ${NT_SRC_SHA:-<none>} is not in origin, so the build copies its tree over the clone"
    fi
    printf 'deploy done\n' >> "$JOURNAL"

    printf 'config begin\n' >> "$JOURNAL"
    py install-file "$STAGED_CFG" "$LIVE_CFG" 0600 "$NT_USER" "$NT_GROUP" || nt_apply_failed "could not install $LIVE_CFG"
    fp_new=$(update_file_fingerprint "$LIVE_CFG")
    [[ "$fp_new" == "$fp_staged_cfg" ]] || nt_apply_failed "the installed $LIVE_CFG differs from the proven staged config"
    mkdir -p "$LIVE_DEPLOY/scheduler" || nt_apply_failed "could not create $LIVE_DEPLOY/scheduler"
    ln -sfn "$LIVE_CFG" "$LIVE_DEPLOY/scheduler/config.json" || nt_apply_failed "could not link $LIVE_DEPLOY/scheduler/config.json"
    printf 'config done\n' >> "$JOURNAL"
    echo "apply: installed $LIVE_CFG and linked $LIVE_DEPLOY/scheduler/config.json to it"
    [[ "$FAIL_AFTER" != "config" ]] || nt_apply_failed "MERGE_PAPER_FAIL_AFTER=config"

    printf 'env begin\n' >> "$JOURNAL"
    nt_env_merge "$LIVE_DEPLOY/.env" >/dev/null || nt_apply_failed "the folded deployments' .env files changed and now disagree"
    printf 'env done\n' >> "$JOURNAL"
    echo "apply: wrote $LIVE_DEPLOY/.env (0600)"

    printf 'build begin\n' >> "$JOURNAL"
    echo "apply: building $LIVE_DEPLOY from ${FOLD_DEPLOY[0]}"
    if [[ -n "$BUILD_CMD" ]]; then
        (cd "$LIVE_DEPLOY" && "$BUILD_CMD" --rsync-from "${FOLD_DEPLOY[0]}") || nt_apply_failed "$BUILD_CMD --rsync-from ${FOLD_DEPLOY[0]} failed in $LIVE_DEPLOY"
    else
        (cd "$LIVE_DEPLOY" && bash scripts/update.sh --rsync-from "${FOLD_DEPLOY[0]}") || nt_apply_failed "update.sh --rsync-from ${FOLD_DEPLOY[0]} failed in $LIVE_DEPLOY"
    fi
    [[ -L "$LIVE_DEPLOY/scheduler/config.json" && "$(readlink "$LIVE_DEPLOY/scheduler/config.json")" == "$LIVE_CFG" ]] || \
        nt_apply_failed "the build replaced $LIVE_DEPLOY/scheduler/config.json; it must stay a link to $LIVE_CFG"
    [[ "$(update_file_fingerprint "$LIVE_CFG")" == "$fp_staged_cfg" ]] || nt_apply_failed "the build rewrote $LIVE_CFG"
    [[ -x "$LIVE_BIN" ]] || nt_apply_failed "the build left no executable $LIVE_BIN"
    new_version=$(run_bin "$LIVE_DEPLOY" "$LIVE_BIN" version 2>/dev/null || true)
    [[ "$new_version" == "$live_version" ]] || nt_apply_failed "the new binary reports version '$new_version', the folded deployments run '$live_version'"
    mkdir -p "$LIVE_DEPLOY/logs" || nt_apply_failed "could not create $LIVE_DEPLOY/logs"
    chown -R "$NT_USER:$NT_GROUP" "$LIVE_DEPLOY" || nt_apply_failed "could not give $LIVE_DEPLOY to $NT_USER:$NT_GROUP"
    printf 'build done\n' >> "$JOURNAL"
    echo "apply: built $LIVE_DEPLOY ($new_version)"
    [[ "$FAIL_AFTER" != "build" ]] || nt_apply_failed "MERGE_PAPER_FAIL_AFTER=build"

    ( run_staged_proof "$LIVE_DEPLOY" "$LIVE_BIN" "new tree's binary" ) || nt_apply_failed "the new tree's binary refused the staged config"
    verify_overrides "" || nt_apply_failed "systemd-analyze verify rejects $LIVE_UNIT with the override and the new tree"

    printf 'template begin\n' >> "$JOURNAL"
    if [[ ! -e "$TEMPLATE_UNIT" ]]; then
        install -m 0644 "$LIVE_DEPLOY/systemd/go-trader@.service" "$TEMPLATE_UNIT" || nt_apply_failed "could not install $TEMPLATE_UNIT"
        printf 'template installed\n' >> "$JOURNAL"
        echo "apply: installed $TEMPLATE_UNIT"
    elif ! cmp -s "$LIVE_DEPLOY/systemd/go-trader@.service" "$TEMPLATE_UNIT"; then
        nt_apply_failed "installed $TEMPLATE_UNIT differs from the new tree's copy"
    fi
    if [[ -n "$NT_NAMESPACE" ]]; then
        if [[ ! -e "$NT_JOURNALD" ]]; then
            install -m 0644 "$LIVE_DEPLOY/systemd/journald@${NT_NAMESPACE}.conf" "$NT_JOURNALD" || nt_apply_failed "could not install $NT_JOURNALD"
            printf 'journald installed\njournald_path %s\n' "$NT_JOURNALD" >> "$JOURNAL"
            "$SYSTEMCTL" try-restart "systemd-journald@${NT_NAMESPACE}.service" >/dev/null 2>&1 || true
            echo "apply: installed $NT_JOURNALD"
        elif ! cmp -s "$LIVE_DEPLOY/systemd/journald@${NT_NAMESPACE}.conf" "$NT_JOURNALD"; then
            nt_apply_failed "installed $NT_JOURNALD differs from the new tree's copy"
        fi
    fi
    printf 'template done\n' >> "$JOURNAL"

    for i in "${!FOLD_KEY[@]}"; do
        sfx="${FOLD_SFX[$i]}"
        dropin="${FOLD_DROPIN[$i]}"
        printf 'override begin%s\n' "$sfx" >> "$JOURNAL"
        mkdir -p "$(dirname "$dropin")" || nt_apply_failed "could not create $(dirname "$dropin")"
        cp "${FOLD_STAGED_OVERRIDE[$i]}" "$(dirname "$dropin")/.$(basename "$dropin").staged" 2>/dev/null || \
            nt_apply_failed "could not stage the override under $(dirname "$dropin")"
        mv -f "$(dirname "$dropin")/.$(basename "$dropin").staged" "$dropin" || nt_apply_failed "could not install $dropin"
        chmod 0644 "$dropin"
        printf 'override done%s\n' "$sfx" >> "$JOURNAL"
        echo "apply: installed $dropin"
    done
    [[ "$FAIL_AFTER" != "override" ]] || nt_apply_failed "MERGE_PAPER_FAIL_AFTER=override"

    check_db_fingerprints "post-apply" || nt_apply_failed "a database changed during the apply"
    {
        printf 'result_config %s\n' "$(update_file_fingerprint "$LIVE_CFG")"
        for i in "${!FOLD_KEY[@]}"; do
            printf 'result_override%s %s\n' "${FOLD_SFX[$i]}" "$(update_file_fingerprint "${FOLD_DROPIN[$i]}")"
        done
        printf 'complete\n'
    } >> "$JOURNAL"
    echo "VERDICT: APPLIED (journal $JOURNAL). $LIVE_UNIT is neither enabled nor started; run the commands above in order."
    exit 0
}

if [[ "$MODE" == "rollback" && "$NT" == "1" ]]; then
    nt_rollback
fi

if [[ "$MODE" == "rollback" ]]; then
    [[ -f "$JOURNAL" ]] || fail "$EXIT_JOURNAL_STATE" "no journal at $JOURNAL; nothing to roll back"
    scan_other_journals_interrupted "rolling this one back"
    newer_journal=$(journal_that_installed_live_config || true)
    if [[ -n "$newer_journal" ]]; then
        fail "$EXIT_JOURNAL_STATE" "$LIVE_CFG is the config $newer_journal installed, a merge later than this one; restoring $RETAINED_CFG would discard that merge and leave its own rollback refused. Roll $newer_journal back first"
    fi
    if journal_has "rolled-back"; then
        echo "rollback: journal already rolled back; nothing to do"
        check_db_fingerprints "rollback" || exit "$EXIT_RESTORE_FAILED"
        exit 0
    fi
    if ! restore_from_retained; then
        exit "$EXIT_RESTORE_FAILED"
    fi
    check_db_fingerprints "rollback" || exit "$EXIT_RESTORE_FAILED"
    echo "rollback: complete; every unit stays stopped. Re-run the dry run before any new apply."
    exit 0
fi

scan_other_journals

if [[ -f "$JOURNAL" ]]; then
    if journal_has "complete"; then
        matches=1
        [[ "$(update_file_fingerprint "$LIVE_CFG")" == "$(journal_value result_config)" ]] || matches=0
        for i in "${!FOLD_KEY[@]}"; do
            [[ "$(update_file_fingerprint "${FOLD_DROPIN[$i]}")" == "$(journal_value "result_override${FOLD_SFX[$i]}")" ]] || matches=0
        done
        if [[ "$matches" == "1" ]]; then
            if [[ "$MODE" == "apply" ]]; then
                echo "apply: journal $JOURNAL is complete and every deployment file matches its result; nothing to do"
                check_db_fingerprints "apply" || exit "$EXIT_RESTORE_FAILED"
                exit 0
            fi
            echo "journal: $JOURNAL is complete and every deployment file matches its result; this dry run composes over the merged config and --apply is a no-op"
        else
            fail "$EXIT_JOURNAL_STATE" "journal $JOURNAL is complete but $LIVE_CFG or a drop-in changed since; inspect by hand and remove the journal to merge again"
        fi
    elif journal_has "rolled-back"; then
        if [[ "$MODE" == "apply" ]]; then
            mv -f "$JOURNAL" "${JOURNAL}.rolled-back.$(date +%Y%m%d%H%M%S)"
            echo "apply: previous journal was rolled back; archived it and starting fresh"
        else
            echo "journal: $JOURNAL records a rolled-back run; --apply archives it and starts fresh"
        fi
    elif [[ "$MODE" == "apply" ]]; then
        echo "apply: journal $JOURNAL records an interrupted apply; restoring the retained files first"
        if ! restore_from_retained; then
            exit "$EXIT_RESTORE_FAILED"
        fi
        check_db_fingerprints "resume" || exit "$EXIT_RESTORE_FAILED"
        mv -f "$JOURNAL" "${JOURNAL}.rolled-back.$(date +%Y%m%d%H%M%S)"
        echo "apply: interrupted apply rolled back; continuing with a fresh run"
    else
        fail "$EXIT_JOURNAL_STATE" "journal $JOURNAL records an interrupted apply; $LIVE_CFG may hold the half-applied merge. Run --rollback, or --apply which restores the retained files first, before certifying a dry run"
    fi
fi

cp "$COMPOSE_BASE_CFG" "$LIVE_CFG_COPY"
fp_live_cfg=$(update_file_fingerprint "$COMPOSE_BASE_CFG")
for i in "${!FOLD_KEY[@]}"; do
    cp "${FOLD_CFG[$i]}" "${FOLD_CFG_COPY[$i]}"
    FOLD_FP_CFG[$i]=$(update_file_fingerprint "${FOLD_CFG[$i]}")
done

run_bin "$TOOL_DEPLOY" "$TOOL_BIN" storage-inspect --json --config "$LIVE_CFG_COPY" >"$WORK/storage-live.json" 2>"$WORK/storage-live.err" || true
[[ -s "$WORK/storage-live.json" ]] || { cat "$WORK/storage-live.err" >&2; fail "$EXIT_INSPECTION_REFUSED" "$TARGET_WORD storage-inspect produced no report"; }
if ! py storage-check "$WORK/storage-live.json" "$HOLDER_PID" - primary; then
    fail "$EXIT_INSPECTION_REFUSED" "$TARGET_WORD storage inspection refused"
fi
for i in "${!FOLD_KEY[@]}"; do
    tag="fold-${FOLD_KEY[$i]}"
    run_bin "${FOLD_DEPLOY[$i]}" "${FOLD_BIN[$i]}" storage-inspect --json --config "${FOLD_CFG_COPY[$i]}" >"$WORK/storage-$tag.json" 2>"$WORK/storage-$tag.err" || true
    [[ -s "$WORK/storage-$tag.json" ]] || { cat "$WORK/storage-$tag.err" >&2; fail "$EXIT_INSPECTION_REFUSED" "$(fold_side "$i") storage-inspect produced no report"; }
    echo "inspect: $(fold_desc "$i")"
    if ! py storage-check "$WORK/storage-$tag.json" "$HOLDER_PID" "${FOLD_COUNT[$i]}" primary; then
        fail "$EXIT_INSPECTION_REFUSED" "$(fold_side "$i") storage inspection refused"
    fi
done
if ! run_bin "$TOOL_DEPLOY" "$TOOL_BIN" inspect --all --json --config "$LIVE_CFG_COPY" >"$WORK/inspect-live.json" 2>"$WORK/inspect-live.err"; then
    cat "$WORK/inspect-live.err" >&2
    fail "$EXIT_INSPECTION_REFUSED" "$TARGET_WORD inspect --all --json failed"
fi
for i in "${!FOLD_KEY[@]}"; do
    tag="fold-${FOLD_KEY[$i]}"
    FOLD_INSPECT[$i]="$WORK/inspect-$tag.json"
    FOLD_COMPOSE_CFG[$i]="${FOLD_CFG[$i]}"
    if ! run_bin "${FOLD_DEPLOY[$i]}" "${FOLD_BIN[$i]}" inspect --all --json --config "${FOLD_CFG_COPY[$i]}" >"${FOLD_INSPECT[$i]}" 2>"$WORK/inspect-$tag.err"; then
        cat "$WORK/inspect-$tag.err" >&2
        fail "$EXIT_INSPECTION_REFUSED" "$(fold_side "$i") inspect --all --json failed"
    fi
done
config_copy_intact "$TARGET_WORD" "$LIVE_CFG_COPY" "$fp_live_cfg" "inspection"
[[ "$(update_file_fingerprint "$COMPOSE_BASE_CFG")" == "$fp_live_cfg" ]] || fail "$EXIT_SOURCE_CHANGED" "$COMPOSE_BASE_CFG changed during inspection"
for i in "${!FOLD_KEY[@]}"; do
    config_copy_intact "$(fold_side "$i")" "${FOLD_CFG_COPY[$i]}" "${FOLD_FP_CFG[$i]}" "inspection"
    [[ "$(update_file_fingerprint "${FOLD_CFG[$i]}")" == "${FOLD_FP_CFG[$i]}" ]] || fail "$EXIT_SOURCE_CHANGED" "${FOLD_CFG[$i]} changed during inspection"
done
check_db_fingerprints "inspection" || exit "$EXIT_INSPECTION_REFUSED"

if [[ "$ALIGN_TO_LIVE" == "1" ]]; then
    for i in "${!FOLD_KEY[@]}"; do
        write_aligned_fold "$i"
        aligned_copy="$WORK/fold-${FOLD_KEY[$i]}-aligned.json"
        cp "${FOLD_COMPOSE_CFG[$i]}" "$aligned_copy"
        fp_aligned=$(update_file_fingerprint "$aligned_copy")
        if ! run_bin "${FOLD_DEPLOY[$i]}" "${FOLD_BIN[$i]}" inspect --all --json --config "$aligned_copy" >"${FOLD_INSPECT[$i]}" 2>"$WORK/inspect-aligned-${FOLD_KEY[$i]}.err"; then
            cat "$WORK/inspect-aligned-${FOLD_KEY[$i]}.err" >&2
            fail "$EXIT_INSPECTION_REFUSED" "$(fold_side "$i") inspect --all --json failed on the aligned config"
        fi
        config_copy_intact "$(fold_side "$i")" "$aligned_copy" "$fp_aligned" "aligned inspection"
    done
fi

write_plan "$WORK/plan.json"
if ! py compose "$COMPOSE_BASE_CFG" "$WORK/plan.json" "$STAGED_CFG" "$STAGED_MAP" "$WORK/inspect-live.json"; then
    rm -f "$STAGED_CFG" "$STAGED_MAP"
    fail "$EXIT_COMPOSE_REFUSED" "merged config could not be composed"
fi

fp_staged_cfg=$(update_file_fingerprint "$STAGED_CFG")

run_staged_proof() {
    local deploy="$1" bin="$2" who="$3" staged_count i
    run_bin "$deploy" "$bin" storage-inspect --json --config "$STAGED_CFG" >"$WORK/storage-staged.json" 2>"$WORK/storage-staged.err" || true
    [[ -s "$WORK/storage-staged.json" ]] || { cat "$WORK/storage-staged.err" >&2; fail "$EXIT_PROOF_REFUSED" "staged storage-inspect produced no report"; }
    echo "proof: staged layout ($who)"
    for i in "${!FOLD_KEY[@]}"; do
        staged_count=$(py partition-count "$STAGED_CFG" "${FOLD_ID[$i]}")
        if ! py storage-check "$WORK/storage-staged.json" "$HOLDER_PID" "$staged_count" "${FOLD_PARTITION[$i]}"; then
            fail "$EXIT_PROOF_REFUSED" "the $who rejects the staged storage layout for ${FOLD_PARTITION[$i]}"
        fi
    done
    if ! run_bin "$deploy" "$bin" inspect --all --json --config "$STAGED_CFG" >"$WORK/inspect-staged.json" 2>"$WORK/inspect-staged.err"; then
        cat "$WORK/inspect-staged.err" >&2
        fail "$EXIT_PROOF_REFUSED" "the $who cannot load the staged config"
    fi
    if ! py diff "$WORK/inspect-staged.json" "$WORK/inspect-live.json" "$WORK/plan.json" "$STAGED_MAP"; then
        fail "$EXIT_PROOF_REFUSED" "effective settings differ between the source deployments and the staged config"
    fi
    [[ "$(update_file_fingerprint "$STAGED_CFG")" == "$fp_staged_cfg" ]] || \
        fail "$EXIT_PROOF_REFUSED" "the $who rewrote $STAGED_CFG during the proof; the staged config is not what was proven"
    check_db_fingerprints "proof" || exit "$EXIT_PROOF_REFUSED"
}

if [[ "$NT" == "1" ]]; then
    run_staged_proof "$TOOL_DEPLOY" "$TOOL_BIN" "first folded deployment's binary"
else
    run_staged_proof "$TOOL_DEPLOY" "$TOOL_BIN" "live binary"
fi

for i in "${!FOLD_KEY[@]}"; do
    fold_db_dir=$(dirname "${FOLD_DB[$i]}")
    directive=$(update_paper_override_directive "$fold_db_dir")
    [[ -n "$directive" ]] || fail "$EXIT_OVERRIDE_REFUSED" "cannot derive a writable-path directive for $fold_db_dir"
    printf '[Service]\n%s\n' "$directive" > "${FOLD_STAGED_OVERRIDE[$i]}"
    echo "override: ${FOLD_DROPIN[$i]}"
    sed 's/^/override:   /' "${FOLD_STAGED_OVERRIDE[$i]}"
done
verify_overrides() {
    local stand_in="$1" unit_src
    if ! command -v "$SYSTEMD_ANALYZE" >/dev/null 2>&1; then
        echo "override: systemd-analyze not available; verify skipped"
        return 0
    fi
    unit_src="$UNIT_DIR/$LIVE_UNIT"
    if [[ ! -f "$unit_src" && "$LIVE_UNIT" == *@*.service ]]; then
        unit_src="$UNIT_DIR/${LIVE_UNIT%%@*}@.service"
    fi
    if [[ ! -f "$unit_src" ]]; then
        echo "override: unit file for $LIVE_UNIT not found under $UNIT_DIR; systemd-analyze verify skipped"
        return 0
    fi
    rm -rf "$WORK/units"
    mkdir -p "$WORK/units/${LIVE_UNIT}.d"
    if [[ -n "$stand_in" ]]; then
        sed "s#/opt/go-trader-%i#${stand_in}#g" "$unit_src" > "$WORK/units/$LIVE_UNIT"
    else
        cp "$unit_src" "$WORK/units/$LIVE_UNIT"
    fi
    if [[ -d "$UNIT_DIR/${LIVE_UNIT}.d" ]]; then
        cp "$UNIT_DIR/${LIVE_UNIT}.d"/*.conf "$WORK/units/${LIVE_UNIT}.d/" 2>/dev/null || true
    fi
    for i in "${!FOLD_KEY[@]}"; do
        cp "${FOLD_STAGED_OVERRIDE[$i]}" "$WORK/units/${LIVE_UNIT}.d/50-merge-paper-${FOLD_KEY[$i]}.conf"
    done
    if ! "$SYSTEMD_ANALYZE" verify "$WORK/units/$LIVE_UNIT" >"$WORK/analyze.out" 2>&1; then
        cat "$WORK/analyze.out" >&2
        return 1
    fi
    if [[ -n "$stand_in" ]]; then
        echo "override: systemd-analyze verify passed (the unit's /opt/go-trader-%i paths read from $stand_in until the new tree exists)"
    else
        echo "override: systemd-analyze verify passed"
    fi
}

if [[ "$NT" == "1" ]]; then
    verify_overrides "${FOLD_DEPLOY[0]}" || fail "$EXIT_OVERRIDE_REFUSED" "systemd-analyze verify rejects $LIVE_UNIT with the override"
else
    verify_overrides "" || fail "$EXIT_OVERRIDE_REFUSED" "systemd-analyze verify rejects $LIVE_UNIT with the override"
fi

if [[ "$NT" == "1" ]]; then
    echo "new target: deployment $LIVE_DEPLOY (git clone of $NT_ORIGIN, built from ${FOLD_DEPLOY[0]})"
    echo "new target: config $LIVE_CFG (0600, $NT_USER:$NT_GROUP), $LIVE_DEPLOY/scheduler/config.json -> $LIVE_CFG, $LIVE_DEPLOY/.env (0600)"
    echo "new target: unit $LIVE_UNIT from $TEMPLATE_UNIT$([[ -e "$TEMPLATE_UNIT" ]] || echo " (installed by the apply)")${NT_JOURNALD:+, journald $NT_JOURNALD$([[ -e "$NT_JOURNALD" ]] || echo " (installed by the apply)")}"
    echo "new target: root settings from ${FOLD_INSTANCE[$ROOT_IDX]}, status_port $STATUS_PORT, db_file ${TARGET_DIR}/state.db (starts empty)"
fi

{
    echo "after apply, run in this order:"
    echo "  1. $SYSTEMCTL daemon-reload"
    step=2
    for i in "${!FOLD_KEY[@]}"; do
        if [[ "$NT" == "1" ]]; then
            echo "  $step. $SYSTEMCTL disable --now ${FOLD_UNIT[$i]}"
        else
            echo "  $step. $SYSTEMCTL disable ${FOLD_UNIT[$i]}"
        fi
        step=$((step + 1))
    done
    if [[ "$NT" == "1" ]]; then
        echo "  $step. $SYSTEMCTL enable --now $LIVE_UNIT"
    else
        echo "  $step. $SYSTEMCTL start $LIVE_UNIT"
    fi
    step=$((step + 1))
    live_log_namespace=$("$SYSTEMCTL" show -p LogNamespace --value "$LIVE_UNIT" 2>/dev/null || true)
    [[ -n "$live_log_namespace" || "$NT" != "1" ]] || live_log_namespace="$NT_NAMESPACE"
    echo "  $step. $(update_journalctl_unit_command "$LIVE_UNIT" "$live_log_namespace") -n 50 | grep '\[storage\]'"
    step=$((step + 1))
    for i in "${!FOLD_KEY[@]}"; do
        if [[ "$NT" == "1" ]]; then
            echo "  $step. retire the ${FOLD_INSTANCE[$i]} instance's status port${FOLD_PORT[$i]:+ (${FOLD_PORT[$i]})} and any tunnel mapping that pointed at it; the combined process serves every partition on port $STATUS_PORT"
        else
            echo "  $step. retire the ${FOLD_INSTANCE[$i]} instance's status port${FOLD_PORT[$i]:+ (${FOLD_PORT[$i]})} and any tunnel mapping that pointed at it; the combined process serves every partition on the live port"
        fi
        step=$((step + 1))
    done
}

if [[ "$MODE" != "apply" ]]; then
    staged_list="$STAGED_CFG"
    for i in "${!FOLD_KEY[@]}"; do
        staged_list+=", ${FOLD_STAGED_OVERRIDE[$i]}"
    done
    if [[ "$NT" == "1" ]]; then
        KEEP_STAGE=1
        echo "VERDICT: READY (dry run; nothing outside the staging directory $STAGE_DIR changed; staged files: $staged_list)"
    else
        echo "VERDICT: READY (dry run; staged files: $staged_list)"
    fi
    exit 0
fi

[[ "$NT" != "1" ]] || nt_apply

[[ "$(update_file_fingerprint "$LIVE_CFG")" == "$fp_live_cfg" ]] || fail "$EXIT_SOURCE_CHANGED" "$LIVE_CFG changed during the run"
for i in "${!FOLD_KEY[@]}"; do
    [[ "$(update_file_fingerprint "${FOLD_CFG[$i]}")" == "${FOLD_FP_CFG[$i]}" ]] || fail "$EXIT_SOURCE_CHANGED" "${FOLD_CFG[$i]} changed during the run"
done
check_db_fingerprints "pre-apply" || exit "$EXIT_SOURCE_CHANGED"
require_units_stopped

run_id="$(date +%Y%m%d%H%M%S)-$$"
{
    printf 'run_id %s\n' "$run_id"
    printf 'live_config %s\n' "$fp_live_cfg"
    printf 'live_db %s\n' "$fp_live_db"
    printf 'staged_config %s\n' "$(update_file_fingerprint "$STAGED_CFG")"
    printf 'folds %s\n' "$(IFS=' '; printf '%s' "${FOLD_KEY[*]}")"
    for i in "${!FOLD_KEY[@]}"; do
        printf 'fold %s %s %s\n' "${FOLD_KEY[$i]}" "${FOLD_PARTITION[$i]}" "${FOLD_INSTANCE[$i]}"
    done
    for i in "${!FOLD_KEY[@]}"; do
        sfx="${FOLD_SFX[$i]}"
        if [[ -n "${FOLD_ID[$i]}" ]]; then
            printf 'source%s %s\n' "$sfx" "${FOLD_INSTANCE[$i]}"
            printf 'source_config%s %s\n' "$sfx" "${FOLD_FP_CFG[$i]}"
            printf 'source_db%s %s\n' "$sfx" "${FOLD_FP_DB[$i]}"
        else
            printf 'paper_config %s\n' "${FOLD_FP_CFG[$i]}"
            printf 'paper_db %s\n' "${FOLD_FP_DB[$i]}"
        fi
        printf 'staged_override%s %s\n' "$sfx" "$(update_file_fingerprint "${FOLD_STAGED_OVERRIDE[$i]}")"
        if [[ -e "${FOLD_DROPIN[$i]}" ]]; then
            printf 'override_prior%s present\n' "$sfx"
            printf 'override_prior_fp%s %s\n' "$sfx" "$(update_file_fingerprint "${FOLD_DROPIN[$i]}")"
        else
            printf 'override_prior%s absent\n' "$sfx"
        fi
    done
    if [[ -n "$ALIGN_OUT" ]]; then
        printf '%s' "$ALIGN_OUT"
    fi
} > "$JOURNAL"

apply_failed() {
    echo "apply: $1" >&2
    if restore_from_retained; then
        check_db_fingerprints "rollback" || exit "$EXIT_RESTORE_FAILED"
        echo "apply: rolled back; every unit stays stopped" >&2
        exit "$EXIT_RESTORE_FAILED"
    fi
    exit "$EXIT_RESTORE_FAILED"
}

printf 'config begin\n' >> "$JOURNAL"
cp -p "$LIVE_CFG" "$RETAINED_CFG" || apply_failed "could not retain $LIVE_CFG"
mv -f "$STAGED_CFG" "$LIVE_CFG" || apply_failed "could not install the staged config"
printf 'config done\n' >> "$JOURNAL"
echo "apply: installed $LIVE_CFG (previous copy at $RETAINED_CFG)"
[[ "$FAIL_AFTER" != "config" ]] || apply_failed "MERGE_PAPER_FAIL_AFTER=config"

for i in "${!FOLD_KEY[@]}"; do
    sfx="${FOLD_SFX[$i]}"
    dropin="${FOLD_DROPIN[$i]}"
    printf 'override begin%s\n' "$sfx" >> "$JOURNAL"
    if [[ -e "$dropin" ]]; then
        cp -p "$dropin" "${FOLD_RETAINED_DROPIN[$i]}" || apply_failed "could not retain $dropin"
    fi
    if ! mkdir -p "$(dirname "$dropin")"; then
        apply_failed "could not create $(dirname "$dropin")"
    fi
    if ! cp "${FOLD_STAGED_OVERRIDE[$i]}" "$(dirname "$dropin")/.$(basename "$dropin").staged" 2>/dev/null; then
        apply_failed "could not stage the override under $(dirname "$dropin")"
    fi
    mv -f "$(dirname "$dropin")/.$(basename "$dropin").staged" "$dropin" || apply_failed "could not install $dropin"
    printf 'override done%s\n' "$sfx" >> "$JOURNAL"
    echo "apply: installed $dropin"
done
[[ "$FAIL_AFTER" != "override" ]] || apply_failed "MERGE_PAPER_FAIL_AFTER=override"

rm -f "$STAGED_MAP"
for i in "${!FOLD_KEY[@]}"; do
    rm -f "${FOLD_STAGED_OVERRIDE[$i]}"
done
check_db_fingerprints "post-apply" || exit "$EXIT_RESTORE_FAILED"
{
    printf 'result_config %s\n' "$(update_file_fingerprint "$LIVE_CFG")"
    for i in "${!FOLD_KEY[@]}"; do
        printf 'result_override%s %s\n' "${FOLD_SFX[$i]}" "$(update_file_fingerprint "${FOLD_DROPIN[$i]}")"
    done
    printf 'complete\n'
} >> "$JOURNAL"
echo "VERDICT: APPLIED (journal $JOURNAL). Run the commands above in order."
exit 0
