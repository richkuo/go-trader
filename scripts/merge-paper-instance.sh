#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
source "${SCRIPT_DIR}/update_helpers.sh"

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
EXIT_INSPECTION_REFUSED=20
EXIT_COMPOSE_REFUSED=21
EXIT_PROOF_REFUSED=22
EXIT_OVERRIDE_REFUSED=23
EXIT_JOURNAL_STATE=24

usage() {
    cat <<'EOF'
usage: merge-paper-instance.sh --live <instance> --paper <instance> [--apply | --rollback | --diff]
         [--align-to-live] [--base <dir>] [--deploy-root <dir>] [--unit-dir <dir>]
         [--live-unit <unit>] [--paper-unit <unit>]

Defaults: --base /var/lib/go-trader, --deploy-root /opt (deployments at
<root>/go-trader-<instance>), --unit-dir /etc/systemd/system, units
go-trader@<instance>.service. Without --apply nothing outside the staging
area changes. --diff reads only the two config files and prints every
differing root key (refuse-on-difference, dropped with the live value kept,
or unknown) plus compose refuses it can see without inspect (replay_log_path
when a paper mirror is present, discord -paper clashes from strategies compose
would newly merge). It is not a dry run: inspect-based portfolio_risk refuses
still need the full pipeline.
Units may stay running and no lock or binary is used.
--align-to-live is valid only with --diff or --apply: it writes live's
shared root values to <paper-config>.aligned and never changes the paper
source; --apply then composes from that file. Exit codes: 2 usage, 3 lock
contention, 4 restore failed, 5 source changed before apply, 10-18 preflight
refusals (18: a config migration is pending or the two configs carry
different versions), 20-24 inspection, compose, proof, override and journal
refusals. The binaries run against copies of both configs; the deployment
files are never rewritten.
EOF
}

LIVE=""
PAPER=""
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

while [[ $# -gt 0 ]]; do
    case "$1" in
        --live) LIVE="${2:-}"; shift 2 ;;
        --paper) PAPER="${2:-}"; shift 2 ;;
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

[[ -n "$LIVE" && -n "$PAPER" ]] || { usage >&2; exit "$EXIT_USAGE"; }
[[ "$(update_validate_instance_name "$LIVE")" == "ok" ]] || fail "$EXIT_USAGE" "invalid live instance name '$LIVE'"
[[ "$(update_validate_instance_name "$PAPER")" == "ok" ]] || fail "$EXIT_USAGE" "invalid paper instance name '$PAPER'"
[[ "$LIVE" != "$PAPER" ]] || fail "$EXIT_USAGE" "live and paper instances must differ"
if [[ "$ALIGN_TO_LIVE" == "1" && "$MODE" != "diff" && "$MODE" != "apply" ]]; then
    fail "$EXIT_USAGE" "--align-to-live is only valid with --diff or --apply"
fi
[[ -z "$LIVE_UNIT" ]] && LIVE_UNIT="go-trader@${LIVE}.service"
[[ -z "$PAPER_UNIT" ]] && PAPER_UNIT="go-trader@${PAPER}.service"

LIVE_DEPLOY="${DEPLOY_ROOT%/}/go-trader-${LIVE}"
PAPER_DEPLOY="${DEPLOY_ROOT%/}/go-trader-${PAPER}"
LIVE_BIN="${LIVE_DEPLOY}/go-trader"
PAPER_BIN="${PAPER_DEPLOY}/go-trader"
LIVE_CFG="${BASE%/}/${LIVE}/config.json"
PAPER_CFG="${BASE%/}/${PAPER}/config.json"
STAGED_CFG="${LIVE_CFG}.merge-staged"
STAGED_MAP="${LIVE_CFG}.merge-staged.map.json"
STAGED_OVERRIDE="${BASE%/}/${LIVE}/merge-paper-${PAPER}.override.staged"
JOURNAL="${BASE%/}/${LIVE}/merge-paper-${PAPER}.journal"
RETAINED_CFG="${LIVE_CFG}.pre-merge-${PAPER}"
DROPIN=$(update_unit_dropin_path "$UNIT_DIR" "$LIVE_UNIT" "50-merge-paper-${PAPER}")
RETAINED_DROPIN="${DROPIN}.pre-merge"

WORK=$(mktemp -d "${TMPDIR:-/tmp}/merge-paper-instance.XXXXXX")
cleanup() {
    update_stop_state_lock_holder
    rm -rf "$WORK"
}
trap cleanup EXIT

MERGE_PY=$(cat <<'PY'
import json
import os
import sys

MODE_LIVE = "live"
MODE_PAPER = "paper"

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
    "auto_update",
    "leaderboard_post_time",
    "leaderboard_summaries",
    "summary_frequency",
    "tradingview_export",
    "tuning",
    "config_version",
    "db_file",
    "paper_db_file",
    "interval_seconds",
    "atr_method",
    "portfolio_risk",
    "discord",
    "strategies",
    "replay_log_path",
]
CHANNEL_MAPS = ["channels", "trade_alert_channels", "dm_channels"]
COMPOSE_DROP_SILENT = (
    "strategies",
    "portfolio_risk",
    "discord",
    "db_file",
    "paper_db_file",
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

def root_value(cfg, key):
    v = cfg.get(key)
    if key == "market_feed":
        return (v or "").strip() or "rest"
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
        if key in DROPPED or key in REFUSE_ON_DIFFERENCE:
            if key in DROPPED and key in paper and key not in COMPOSE_DROP_SILENT:
                if paper.get(key) != live.get(key):
                    dropped_keys.append(key)
            continue
        if paper.get(key) != live.get(key):
            unknown_keys.append(key)
    return refuse_keys, unknown_keys, dropped_keys

def print_root_diff_report(live, paper, refuse_keys, unknown_keys, dropped_keys):
    for key in refuse_keys:
        print("diff: refuse-on-difference %s live=%s paper=%s" % (key, dump_root(live, key), dump_root(paper, key)))
    for key in unknown_keys:
        print("diff: unknown %s live=%s paper=%s" % (key, dump_root(live, key), dump_root(paper, key)))
    for key in dropped_keys:
        print("diff: dropped %s live=%s paper=%s (live value kept)" % (key, dump_root(live, key), dump_root(paper, key)))
    if not refuse_keys and not unknown_keys and not dropped_keys:
        print("diff: no root-key differences")
        return
    print("diff: %d refuse-on-difference, %d unknown, %d dropped" % (len(refuse_keys), len(unknown_keys), len(dropped_keys)))

def refuse_root_conflicts(live, paper, refuse_keys, unknown_keys, dropped_keys):
    for key in refuse_keys:
        print("REFUSE: root key %s differs between live and paper configs (an absent key is compared too, since the merged config would apply the live value to the moved strategies): live=%s paper=%s" % (
            key, dump_root(live, key), dump_root(paper, key)))
    for key in unknown_keys:
        print("REFUSE: root key %s differs between live and paper configs and is not a known drop (an absent key is compared too): live=%s paper=%s" % (
            key, dump_root(live, key), dump_root(paper, key)))
    for key in dropped_keys:
        print("dropped paper root key %s (live value kept): live=%s paper=%s" % (
            key, dump_root(live, key), dump_root(paper, key)))
    sys.exit(1)

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

def cmd_root_diff(live_path, paper_path, paper_db_abs=""):
    live = load(live_path)
    paper = load(paper_path)
    refuse_keys, unknown_keys, dropped_keys = collect_root_diffs(live, paper)
    print_root_diff_report(live, paper, refuse_keys, unknown_keys, dropped_keys)
    for label, live_v, paper_v in collect_compose_refuse_previews(live, paper, paper_db_abs):
        print("diff: compose-refuse %s live=%s paper=%s" % (label, live_v, paper_v))
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

def apply_paper_discord_maps(merged_discord, paper_discord, used):
    report = []
    conflicts = []
    for map_key in CHANNEL_MAPS:
        pm = paper_discord.get(map_key) or {}
        mm = merged_discord.get(map_key)
        if mm is None:
            mm = {}
        for platform, stype in sorted(used):
            val = ""
            for key in ("%s-paper" % platform, platform, stype):
                if pm.get(key):
                    val = pm[key]
                    break
            if not val:
                continue
            target = "%s-paper" % platform
            if target in mm and mm[target] != val:
                conflicts.append((
                    "discord.%s.%s" % (map_key, target),
                    mm[target],
                    val,
                    "discord.%s.%s is %r in the live config but the paper deployment routes to %r" % (map_key, target, mm[target], val),
                ))
            elif target not in mm:
                mm[target] = val
                report.append("discord.%s.%s=%s" % (map_key, target, val))
        for key in sorted(pm):
            val = pm[key]
            if key.endswith("-paper") and key not in mm and val:
                mm[key] = val
                report.append("discord.%s.%s=%s" % (map_key, key, val))
            elif key.endswith("-paper") and key in mm and mm[key] != val:
                conflicts.append((
                    "discord.%s.%s" % (map_key, key),
                    mm[key],
                    val,
                    "discord.%s.%s differs: live=%r paper=%r" % (map_key, key, mm[key], val),
                ))
        if mm:
            merged_discord[map_key] = mm
    return report, conflicts

def compose_paper_already_merged(live, paper_db_abs):
    live_paper_storage = set(storage_id(s) for s in strategies(live) if strategy_mode(s) == MODE_PAPER)
    already_merged = live.get("paper_db_file", "") == paper_db_abs
    return already_merged, live_paper_storage

def collect_compose_refuse_previews(live, paper, paper_db_abs=""):
    previews = []
    seen = set()
    paper_strats = strategies(paper)
    if paper_strats and any(mirror_source(s) is not None for s in paper_strats):
        if (live.get("replay_log_path") or "") != (paper.get("replay_log_path") or ""):
            label = "replay_log_path"
            seen.add(label)
            previews.append((label, json.dumps(live.get("replay_log_path"), sort_keys=True), json.dumps(paper.get("replay_log_path"), sort_keys=True)))
    already_merged, live_paper_storage = compose_paper_already_merged(live, paper_db_abs)
    used = set()
    for s in paper_strats:
        if already_merged and storage_id(s) in live_paper_storage:
            continue
        platform = s.get("platform") or ("hyperliquid" if s["id"].startswith("hl-") else "")
        used.add((platform, s.get("type") or ""))
    merged_discord = json.loads(json.dumps(live.get("discord") or {}))
    _report, conflicts = apply_paper_discord_maps(merged_discord, paper.get("discord") or {}, used)
    for label, live_v, paper_v, _msg in conflicts:
        if label not in seen:
            seen.add(label)
            previews.append((label, json.dumps(live_v, sort_keys=True), json.dumps(paper_v, sort_keys=True)))
    return previews

def cmd_classify(path):
    cfg = load(path)
    counts = {MODE_LIVE: 0, MODE_PAPER: 0}
    for s in strategies(cfg):
        counts[strategy_mode(s)] += 1
    risk = cfg.get("portfolio_risk")
    print(json.dumps({
        "live": counts[MODE_LIVE],
        "paper": counts[MODE_PAPER],
        "strategy_count": len(strategies(cfg)),
        "db_file": (cfg.get("db_file") or "scheduler/state.db").strip() or "scheduler/state.db",
        "paper_db_file": (cfg.get("paper_db_file") or "").strip(),
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

def cmd_compose(live_path, paper_path, paper_db_abs, out_path, map_path, inspect_live_path, inspect_paper_path):
    live = load(live_path)
    paper = load(paper_path)
    merged = json.loads(json.dumps(live))
    report = []
    live_strats = strategies(live)
    paper_strats = strategies(paper)
    if paper.get("paper_db_file"):
        refuse("the paper config already splits its own state (paper_db_file); the handoff handles one primary file per side")
    for s in paper_strats:
        if strategy_mode(s) == MODE_LIVE:
            refuse("paper config strategy %s runs --mode=live" % s["id"])

    live_ids = set(s["id"] for s in live_strats)
    already_merged, live_paper_storage = compose_paper_already_merged(live, paper_db_abs)
    taken = set(live_ids)
    remaining = set(s["id"] for s in paper_strats)
    renames = {}
    skipped = []
    new_strats = []
    for s in paper_strats:
        remaining.discard(s["id"])
        if already_merged and storage_id(s) in live_paper_storage:
            skipped.append(s["id"])
            continue
        pid = s["id"]
        if pid in taken:
            n = 1
            while True:
                cand = "%s-paper" % s["id"] if n == 1 else "%s-paper%d" % (s["id"], n)
                if cand not in taken and cand not in remaining:
                    break
                n += 1
            pid = cand
        block = json.loads(json.dumps(s))
        if pid != s["id"]:
            renames[s["id"]] = pid
            block["id"] = pid
            if "storage_strategy_id" not in block:
                block["storage_strategy_id"] = s["id"]
            report.append("rename %s -> %s (storage_strategy_id=%s)" % (s["id"], pid, block["storage_strategy_id"]))
        taken.add(pid)
        new_strats.append((s, block))

    live_interval = effective_root(live, "interval_seconds", 600)
    paper_interval = effective_root(paper, "interval_seconds", 600)
    live_atr = (live.get("atr_method") or "simple").strip().lower() or "simple"
    paper_atr = (paper.get("atr_method") or "simple").strip().lower() or "simple"
    for s, block in new_strats:
        if paper_interval != live_interval and not block.get("interval_seconds"):
            block["interval_seconds"] = paper_interval
            report.append("stamp %s interval_seconds=%s (paper root cadence)" % (block["id"], paper_interval))
        if paper_atr != live_atr and block.get("type") != "options" and not (block.get("atr_method") or "").strip():
            block["atr_method"] = paper_atr
            report.append("stamp %s atr_method=%s (paper root method)" % (block["id"], paper_atr))

    merged_strats = list(merged.get("strategies") or [])
    all_after = [s for s in merged_strats if isinstance(s, dict)] + [b for _, b in new_strats]
    by_id = dict((s["id"], s) for s in all_after if isinstance(s.get("id"), str))
    claimed = {}
    for s in all_after:
        src = mirror_source(s)
        if src is None:
            continue
        if s.get("replay_source_id"):
            continue
        if src in renames and src != s["id"]:
            continue
        claimed.setdefault(src, []).append(s["id"])
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
    if any_mirror and (live.get("replay_log_path") or "") != (paper.get("replay_log_path") or ""):
        if paper_strats and any(mirror_source(s) is not None for s in paper_strats):
            refuse("root key replay_log_path differs: live=%r paper=%r" % (live.get("replay_log_path"), paper.get("replay_log_path")))

    paper_risk = paper.get("portfolio_risk")
    live_risk = live.get("portfolio_risk")
    if isinstance(paper_risk, dict) and "paper" in paper_risk:
        refuse("paper config nests portfolio_risk.paper")
    if live_risk is not None and not isinstance(live_risk, dict):
        refuse("live config portfolio_risk is not an object")
    live_eff = effective_scope_risk(inspect_live_path, MODE_LIVE)
    paper_eff = effective_scope_risk(inspect_paper_path, MODE_PAPER)
    if paper_eff is None:
        refuse("the paper inspect document carries no paper-scope strategy; the effective paper risk limits are unknown")
    if live_eff is None:
        if isinstance(live_risk, dict) and "paper" in live_risk:
            refuse("the live config runs no live strategy and already carries portfolio_risk.paper; the effective live risk limits cannot be separated from the override")
        live_eff = effective_scope_risk(inspect_live_path, MODE_PAPER)
    if live_eff is None:
        refuse("the live inspect document carries no strategy; the effective live risk limits are unknown")
    root = dict(live_risk) if isinstance(live_risk, dict) else {}
    existing = root.pop("paper", None)
    if not risk_fields(root):
        root = dict((k, v) for k, v in live_eff.items())
        report.append("portfolio_risk root materialized from the effective live view: %s%s" % (
            json.dumps(root, sort_keys=True), "" if isinstance(live_risk, dict) else " (live config had no portfolio_risk block; the loader default now stays explicit)"))
    override = {}
    for k in RISK_FIELDS:
        lv = live_eff.get(k, 0)
        pv = paper_eff.get(k, 0)
        if pv == lv:
            continue
        if pv == 0:
            refuse("portfolio_risk.paper.%s would be zero while live sets %s; zero inherits the live limit and cannot disable it. Set an explicit paper value" % (k, lv))
        override[k] = pv
    if existing is not None:
        existing_eff = dict(live_eff)
        existing_eff.update(risk_fields(existing))
        override_eff = dict(live_eff)
        override_eff.update(override)
        if existing_eff != override_eff:
            refuse("portfolio_risk.paper already exists with different values: %s vs paper deployment %s" % (json.dumps(existing, sort_keys=True), json.dumps(override, sort_keys=True)))
        override = risk_fields(existing)
    if override:
        root["paper"] = override
        report.append("portfolio_risk.paper=%s" % json.dumps(override, sort_keys=True))
    if root:
        merged["portfolio_risk"] = root

    paper_discord = paper.get("discord") or {}
    merged_discord = merged.setdefault("discord", {})
    used = set()
    for s, block in new_strats:
        platform = block.get("platform") or ("hyperliquid" if block["id"].startswith("hl-") else "")
        used.add((platform, block.get("type") or ""))
    discord_report, discord_conflicts = apply_paper_discord_maps(merged_discord, paper_discord, used)
    if discord_conflicts:
        refuse(discord_conflicts[0][3])
    report.extend(discord_report)

    refuse_keys, unknown_keys, dropped = collect_root_diffs(live, paper)
    if refuse_keys or unknown_keys:
        refuse_root_conflicts(live, paper, refuse_keys, unknown_keys, dropped)

    merged["strategies"] = merged_strats + [b for _, b in new_strats]
    merged["paper_db_file"] = paper_db_abs
    write_json_atomic(out_path, merged, live_path)
    with open(map_path, "w") as f:
        json.dump({
            "renames": renames,
            "paper_ids": [b["id"] for _, b in new_strats],
            "paper_original_ids": [s["id"] for s, _ in new_strats],
            "skipped": skipped,
            "dropped": dropped,
        }, f, indent=2)
    for line in report:
        print("compose: %s" % line)
    for key in dropped:
        print("compose: dropped paper root key %s (live value kept)" % key)
    for sid in skipped:
        print("compose: %s already merged; skipped" % sid)
    print("compose: %d existing + %d added strategies, paper_db_file=%s" % (len(merged_strats), len(new_strats), paper_db_abs))

def normalize(doc):
    if isinstance(doc, dict):
        out = {}
        for k, v in doc.items():
            if k in ("id", "storage_strategy_id") or k.endswith("_explicit"):
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

def cmd_diff(staged_path, live_path, paper_path, map_path):
    staged = dict((s["id"], s) for s in load(staged_path))
    live = dict((s["id"], s) for s in load(live_path))
    paper = dict((s["id"], s) for s in load(paper_path))
    m = load(map_path)
    renames = m["renames"]
    problems = []
    checked = 0
    for orig, before in sorted(live.items()):
        after = staged.get(orig)
        if after is None:
            problems.append("live strategy %s is missing from the staged config" % orig)
            continue
        checked += compare(orig, before, after, problems)
    for orig in m["paper_original_ids"]:
        before = paper.get(orig)
        new_id = renames.get(orig, orig)
        after = staged.get(new_id)
        if before is None or after is None:
            problems.append("paper strategy %s (staged as %s) is missing from an inspect document" % (orig, new_id))
            continue
        checked += compare("%s->%s" % (orig, new_id), before, after, problems)
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

def main():
    cmd = sys.argv[1]
    args = sys.argv[2:]
    if cmd == "classify":
        cmd_classify(*args)
    elif cmd == "get":
        cmd_get(*args)
    elif cmd == "storage-check":
        cmd_storage_check(*args)
    elif cmd == "compose":
        cmd_compose(*args)
    elif cmd == "diff":
        cmd_diff(*args)
    elif cmd == "root-diff":
        cmd_root_diff(*args)
    elif cmd == "align":
        cmd_align(*args)
    else:
        refuse("unknown helper command %s" % cmd)

main()
PY
)

py() {
    python3 -c "$MERGE_PY" "$@"
}

cfg_get() {
    py get "$1" "$2"
}

unit_state() {
    "$SYSTEMCTL" is-active "$1" 2>/dev/null || true
}

require_units_stopped() {
    local unit state
    for unit in "$LIVE_UNIT" "$PAPER_UNIT"; do
        state=$(unit_state "$unit")
        case "$state" in
            active|activating|reloading)
                fail "$EXIT_UNIT_ACTIVE" "unit $unit is $state; stop both units before the handoff"
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

write_aligned_paper() {
    local aligned="${PAPER_CFG}.aligned" rc=0
    ALIGN_OUT=$(py align "$LIVE_CFG" "$PAPER_CFG" "$aligned") || rc=$?
    if [[ "$rc" != "0" ]]; then
        printf '%s\n' "$ALIGN_OUT" >&2
        fail "$EXIT_COMPOSE_REFUSED" "could not write aligned paper config $aligned"
    fi
    printf '%s\n' "$ALIGN_OUT"
    echo "align: wrote $aligned (source paper config unchanged)"
}

ALIGN_OUT=""
if [[ "$MODE" == "diff" ]]; then
    for c in "$LIVE_CFG" "$PAPER_CFG"; do
        [[ -f "$c" ]] || fail "$EXIT_CONFIG_MISSING" "config $c is missing"
    done
    echo "merge-paper-instance: live=$LIVE ($LIVE_CFG) paper=$PAPER ($PAPER_CFG) mode=diff"
    diff_paper_db=""
    if paper_class=$(py classify "$PAPER_CFG"); then
        paper_db_rel=$(printf '%s' "$paper_class" | python3 -c 'import json,sys; print(json.load(sys.stdin)["db_file"])')
        if [[ -n "$paper_db_rel" ]]; then
            diff_paper_db=$(update_canonical_db_path "$(update_resolve_config_db_path "$PAPER_DEPLOY" "$paper_db_rel")")
        fi
    fi
    if ! py root-diff "$LIVE_CFG" "$PAPER_CFG" "$diff_paper_db"; then
        fail "$EXIT_CONFIG_MISSING" "could not read live/paper configs for --diff"
    fi
    if [[ "$ALIGN_TO_LIVE" == "1" ]]; then
        write_aligned_paper
    fi
    exit 0
fi

echo "merge-paper-instance: live=$LIVE ($LIVE_DEPLOY, $LIVE_CFG) paper=$PAPER ($PAPER_DEPLOY, $PAPER_CFG) mode=$MODE"

for d in "$LIVE_DEPLOY" "$PAPER_DEPLOY"; do
    [[ -d "$d" ]] || fail "$EXIT_DEPLOY_MISSING" "deployment directory $d is missing"
done
for b in "$LIVE_BIN" "$PAPER_BIN"; do
    [[ -x "$b" ]] || fail "$EXIT_DEPLOY_MISSING" "binary $b is missing or not executable"
done
live_version=$(run_bin "$LIVE_DEPLOY" "$LIVE_BIN" version 2>/dev/null || true)
paper_version=$(run_bin "$PAPER_DEPLOY" "$PAPER_BIN" version 2>/dev/null || true)
[[ -n "$live_version" && "$live_version" == "$paper_version" ]] || \
    fail "$EXIT_VERSION_MISMATCH" "binary versions differ: live='$live_version' paper='$paper_version'; update both deployments to one release first"
for c in "$LIVE_CFG" "$PAPER_CFG"; do
    [[ -f "$c" ]] || fail "$EXIT_CONFIG_MISSING" "config $c is missing"
done
require_units_stopped
LIVE_CFG_COPY="$WORK/live-config.json"
PAPER_CFG_COPY="$WORK/paper-config.json"
cp "$LIVE_CFG" "$LIVE_CFG_COPY"
cp "$PAPER_CFG" "$PAPER_CFG_COPY"
fp_live_cfg=$(update_file_fingerprint "$LIVE_CFG")
fp_paper_cfg=$(update_file_fingerprint "$PAPER_CFG")

config_copy_intact() {
    local side="$1" copy="$2" want="$3" what="$4"
    if [[ "$(update_file_fingerprint "$copy")" != "$want" ]]; then
        fail "$EXIT_CONFIG_MIGRATION" "the $side binary rewrote its config while running $what (a config migration is pending); this release inspects read-only, so update the $side deployment to it, or start the unit once as the service user to migrate the file, then re-run"
    fi
}

for pair in "live|$LIVE_DEPLOY|$LIVE_BIN|$LIVE_CFG_COPY|$fp_live_cfg" "paper|$PAPER_DEPLOY|$PAPER_BIN|$PAPER_CFG_COPY|$fp_paper_cfg"; do
    IFS='|' read -r side deploy bin cfg want <<<"$pair"
    run_bin "$deploy" "$bin" storage-inspect --json --config "$cfg" >"$WORK/probe-$side.json" 2>"$WORK/probe-$side.err" || true
    config_copy_intact "$side" "$cfg" "$want" "storage-inspect"
    if [[ ! -s "$WORK/probe-$side.json" ]]; then
        cat "$WORK/probe-$side.err" >&2
        if grep -q "failed to load config" "$WORK/probe-$side.err"; then
            fail "$EXIT_INSPECTION_REFUSED" "$side binary refuses to load $cfg; fix the config errors above first"
        fi
        fail "$EXIT_BINARY_INCOMPATIBLE" "$side binary cannot inspect its storage layout (storage-inspect --json produced no report)"
    fi
    if ! layout=$(cfg_get "$WORK/probe-$side.json" layout 2>/dev/null) || [[ -z "$layout" ]]; then
        cat "$WORK/probe-$side.err" >&2
        fail "$EXIT_BINARY_INCOMPATIBLE" "$side binary's storage-inspect --json carries no layout; a release with the early ownership-lock contract is required"
    fi
done

paper_class=$(py classify "$PAPER_CFG")
live_class=$(py classify "$LIVE_CFG")
paper_live_count=$(printf '%s' "$paper_class" | python3 -c 'import json,sys; print(json.load(sys.stdin)["live"])')
[[ "$paper_live_count" == "0" ]] || fail "$EXIT_PAPER_NOT_PAPER" "paper config runs $paper_live_count live strategy(ies); every strategy must be paper"
paper_strategy_count=$(printf '%s' "$paper_class" | python3 -c 'import json,sys; print(json.load(sys.stdin)["strategy_count"])')
[[ "$paper_strategy_count" != "0" ]] || fail "$EXIT_PAPER_NOT_PAPER" "paper config has no strategies"
paper_cv=$(printf '%s' "$paper_class" | python3 -c 'import json,sys; print(json.load(sys.stdin)["config_version"] or "")')
live_cv=$(printf '%s' "$live_class" | python3 -c 'import json,sys; print(json.load(sys.stdin)["config_version"] or "")')
[[ -n "$live_cv" && "$live_cv" == "$paper_cv" ]] || \
    fail "$EXIT_CONFIG_MIGRATION" "config_version differs: live='$live_cv' paper='$paper_cv'; start each unit once on the current release so both files carry one version, then re-run"
paper_nested=$(printf '%s' "$paper_class" | python3 -c 'import json,sys; print(json.load(sys.stdin)["nested_paper_risk"])')
[[ "$paper_nested" == "False" ]] || fail "$EXIT_PAPER_NOT_PAPER" "paper config nests portfolio_risk.paper"
paper_split=$(printf '%s' "$paper_class" | python3 -c 'import json,sys; print(json.load(sys.stdin)["paper_db_file"])')
[[ -z "$paper_split" ]] || fail "$EXIT_DB_IDENTITY" "paper config already sets paper_db_file=$paper_split; the handoff handles one primary file per side"
paper_db_rel=$(printf '%s' "$paper_class" | python3 -c 'import json,sys; print(json.load(sys.stdin)["db_file"])')
live_db_rel=$(printf '%s' "$live_class" | python3 -c 'import json,sys; print(json.load(sys.stdin)["db_file"])')
live_paper_db=$(printf '%s' "$live_class" | python3 -c 'import json,sys; print(json.load(sys.stdin)["paper_db_file"])')
paper_status_port=$(printf '%s' "$paper_class" | python3 -c 'import json,sys; print(json.load(sys.stdin)["status_port"] or "")')
PAPER_DB=$(update_resolve_config_db_path "$PAPER_DEPLOY" "$paper_db_rel")
LIVE_DB=$(update_resolve_config_db_path "$LIVE_DEPLOY" "$live_db_rel")
PAPER_DB_CANON=$(update_canonical_db_path "$PAPER_DB")
LIVE_DB_CANON=$(update_canonical_db_path "$LIVE_DB")
[[ "$PAPER_DB_CANON" != "$LIVE_DB_CANON" ]] || fail "$EXIT_DB_IDENTITY" "paper db_file resolves to the live db_file ($LIVE_DB_CANON)"
if [[ -n "$live_paper_db" ]]; then
    live_paper_canon=$(update_canonical_db_path "$(update_resolve_config_db_path "$LIVE_DEPLOY" "$live_paper_db")")
    [[ "$live_paper_canon" == "$PAPER_DB_CANON" ]] || \
        fail "$EXIT_LIVE_PAPER_DB_CONFLICT" "live config already sets paper_db_file=$live_paper_db, which is not the $PAPER instance's database ($PAPER_DB_CANON)"
    echo "preflight: live config already references the paper database (repeat run)"
fi
[[ -f "$PAPER_DB_CANON" ]] || fail "$EXIT_DB_IDENTITY" "paper database $PAPER_DB_CANON is absent; there is no book to move"
echo "preflight: versions=$live_version paper_db=$PAPER_DB_CANON live_db=$LIVE_DB_CANON"

if ! update_start_state_lock_holder "$LIVE_DB_CANON" "$PAPER_DB_CANON"; then
    fail "$EXIT_LOCK_CONTENDED" "a database lock is held by another process; see the CONTENDED line above"
fi
HOLDER_PID="$UPDATE_LOCK_HOLDER_PID"
echo "locks: held by pid $HOLDER_PID on $(update_state_lock_paths "$LIVE_DB_CANON" | tr '\n' ' ')$(update_state_lock_paths "$PAPER_DB_CANON" | tr '\n' ' ')"

fp_live_db=$(update_db_fingerprint "$LIVE_DB_CANON" | tr '\n' ' ')
fp_paper_db=$(update_db_fingerprint "$PAPER_DB_CANON" | tr '\n' ' ')

check_db_fingerprints() {
    local stage="$1" now_live now_paper
    now_live=$(update_db_fingerprint "$LIVE_DB_CANON" | tr '\n' ' ')
    now_paper=$(update_db_fingerprint "$PAPER_DB_CANON" | tr '\n' ' ')
    if [[ "$now_live" != "$fp_live_db" || "$now_paper" != "$fp_paper_db" ]]; then
        echo "CRITICAL: a database changed during $stage: live before=[$fp_live_db] after=[$now_live] paper before=[$fp_paper_db] after=[$now_paper]" >&2
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
    local failed=0
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
    if journal_has "override done" || journal_has "override begin"; then
        archive_if_edited "$DROPIN" "override" "$(journal_value override_prior_fp)" "$(journal_value staged_override)" "$(journal_value result_override)" || failed=1
        if [[ "$failed" == "1" ]]; then
            :
        elif journal_has "override_prior absent"; then
            if [[ -e "$DROPIN" ]]; then
                if rm -f "$DROPIN"; then
                    echo "restore: $DROPIN removed (absent before the merge)"
                else
                    echo "CRITICAL: could not remove $DROPIN" >&2
                    failed=1
                fi
            fi
        elif [[ -f "$RETAINED_DROPIN" ]]; then
            if mv -f "$RETAINED_DROPIN" "$DROPIN"; then
                echo "restore: $DROPIN restored from $RETAINED_DROPIN"
            else
                echo "CRITICAL: could not restore $DROPIN" >&2
                failed=1
            fi
        elif journal_has "override done"; then
            echo "CRITICAL: retained copy $RETAINED_DROPIN is missing; $DROPIN holds the merge override" >&2
            failed=1
        fi
    fi
    if [[ "$failed" == "1" ]]; then
        echo "state: config=$LIVE_CFG retained=$RETAINED_CFG dropin=$DROPIN retained=$RETAINED_DROPIN journal=$JOURNAL" >&2
        return 1
    fi
    printf 'rolled-back\n' >> "$JOURNAL"
    return 0
}

if [[ "$MODE" == "rollback" ]]; then
    [[ -f "$JOURNAL" ]] || fail "$EXIT_JOURNAL_STATE" "no journal at $JOURNAL; nothing to roll back"
    if journal_has "rolled-back"; then
        echo "rollback: journal already rolled back; nothing to do"
        check_db_fingerprints "rollback" || exit "$EXIT_RESTORE_FAILED"
        exit 0
    fi
    if ! restore_from_retained; then
        exit "$EXIT_RESTORE_FAILED"
    fi
    check_db_fingerprints "rollback" || exit "$EXIT_RESTORE_FAILED"
    echo "rollback: complete; both units stay stopped. Re-run the dry run before any new apply."
    exit 0
fi

if [[ -f "$JOURNAL" ]]; then
    if journal_has "complete"; then
        rec_cfg=$(journal_value result_config)
        rec_ovr=$(journal_value result_override)
        if [[ "$(update_file_fingerprint "$LIVE_CFG")" == "$rec_cfg" && "$(update_file_fingerprint "$DROPIN")" == "$rec_ovr" ]]; then
            if [[ "$MODE" == "apply" ]]; then
                echo "apply: journal $JOURNAL is complete and both deployment files match its result; nothing to do"
                check_db_fingerprints "apply" || exit "$EXIT_RESTORE_FAILED"
                exit 0
            fi
            echo "journal: $JOURNAL is complete and both deployment files match its result; this dry run composes over the merged config and --apply is a no-op"
        else
            fail "$EXIT_JOURNAL_STATE" "journal $JOURNAL is complete but $LIVE_CFG or $DROPIN changed since; inspect by hand and remove the journal to merge again"
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

cp "$LIVE_CFG" "$LIVE_CFG_COPY"
cp "$PAPER_CFG" "$PAPER_CFG_COPY"
fp_live_cfg=$(update_file_fingerprint "$LIVE_CFG")
fp_paper_cfg=$(update_file_fingerprint "$PAPER_CFG")

run_bin "$LIVE_DEPLOY" "$LIVE_BIN" storage-inspect --json --config "$LIVE_CFG_COPY" >"$WORK/storage-live.json" 2>"$WORK/storage-live.err" || true
run_bin "$PAPER_DEPLOY" "$PAPER_BIN" storage-inspect --json --config "$PAPER_CFG_COPY" >"$WORK/storage-paper.json" 2>"$WORK/storage-paper.err" || true
[[ -s "$WORK/storage-live.json" ]] || { cat "$WORK/storage-live.err" >&2; fail "$EXIT_INSPECTION_REFUSED" "live storage-inspect produced no report"; }
[[ -s "$WORK/storage-paper.json" ]] || { cat "$WORK/storage-paper.err" >&2; fail "$EXIT_INSPECTION_REFUSED" "paper storage-inspect produced no report"; }
if ! py storage-check "$WORK/storage-live.json" "$HOLDER_PID" - primary; then
    fail "$EXIT_INSPECTION_REFUSED" "live storage inspection refused"
fi
echo "inspect: paper deployment"
if ! py storage-check "$WORK/storage-paper.json" "$HOLDER_PID" "$paper_strategy_count" primary; then
    fail "$EXIT_INSPECTION_REFUSED" "paper storage inspection refused"
fi
if ! run_bin "$LIVE_DEPLOY" "$LIVE_BIN" inspect --all --json --config "$LIVE_CFG_COPY" >"$WORK/inspect-live.json" 2>"$WORK/inspect-live.err"; then
    cat "$WORK/inspect-live.err" >&2
    fail "$EXIT_INSPECTION_REFUSED" "live inspect --all --json failed"
fi
if ! run_bin "$PAPER_DEPLOY" "$PAPER_BIN" inspect --all --json --config "$PAPER_CFG_COPY" >"$WORK/inspect-paper.json" 2>"$WORK/inspect-paper.err"; then
    cat "$WORK/inspect-paper.err" >&2
    fail "$EXIT_INSPECTION_REFUSED" "paper inspect --all --json failed"
fi
config_copy_intact live "$LIVE_CFG_COPY" "$fp_live_cfg" "inspection"
config_copy_intact paper "$PAPER_CFG_COPY" "$fp_paper_cfg" "inspection"
[[ "$(update_file_fingerprint "$LIVE_CFG")" == "$fp_live_cfg" ]] || fail "$EXIT_SOURCE_CHANGED" "$LIVE_CFG changed during inspection"
[[ "$(update_file_fingerprint "$PAPER_CFG")" == "$fp_paper_cfg" ]] || fail "$EXIT_SOURCE_CHANGED" "$PAPER_CFG changed during inspection"
check_db_fingerprints "inspection" || exit "$EXIT_INSPECTION_REFUSED"

PAPER_COMPOSE="$PAPER_CFG"
if [[ "$ALIGN_TO_LIVE" == "1" ]]; then
    write_aligned_paper
    PAPER_COMPOSE="${PAPER_CFG}.aligned"
fi
if ! py compose "$LIVE_CFG" "$PAPER_COMPOSE" "$PAPER_DB_CANON" "$STAGED_CFG" "$STAGED_MAP" "$WORK/inspect-live.json" "$WORK/inspect-paper.json"; then
    rm -f "$STAGED_CFG" "$STAGED_MAP"
    fail "$EXIT_COMPOSE_REFUSED" "merged config could not be composed"
fi

fp_staged_cfg=$(update_file_fingerprint "$STAGED_CFG")
run_bin "$LIVE_DEPLOY" "$LIVE_BIN" storage-inspect --json --config "$STAGED_CFG" >"$WORK/storage-staged.json" 2>"$WORK/storage-staged.err" || true
[[ -s "$WORK/storage-staged.json" ]] || { cat "$WORK/storage-staged.err" >&2; fail "$EXIT_PROOF_REFUSED" "staged storage-inspect produced no report"; }
echo "proof: staged layout"
staged_paper_count=$(python3 -c 'import json,sys; print(len(json.load(open(sys.argv[1]))["paper_ids"]) + len(json.load(open(sys.argv[1]))["skipped"]))' "$STAGED_MAP")
if ! py storage-check "$WORK/storage-staged.json" "$HOLDER_PID" "$staged_paper_count" paper; then
    fail "$EXIT_PROOF_REFUSED" "the live binary rejects the staged storage layout"
fi
if ! run_bin "$LIVE_DEPLOY" "$LIVE_BIN" inspect --all --json --config "$STAGED_CFG" >"$WORK/inspect-staged.json" 2>"$WORK/inspect-staged.err"; then
    cat "$WORK/inspect-staged.err" >&2
    fail "$EXIT_PROOF_REFUSED" "the live binary cannot load the staged config"
fi
if ! py diff "$WORK/inspect-staged.json" "$WORK/inspect-live.json" "$WORK/inspect-paper.json" "$STAGED_MAP"; then
    fail "$EXIT_PROOF_REFUSED" "effective settings differ between the source deployments and the staged config"
fi
[[ "$(update_file_fingerprint "$STAGED_CFG")" == "$fp_staged_cfg" ]] || \
    fail "$EXIT_PROOF_REFUSED" "the live binary rewrote $STAGED_CFG during the proof; the staged config is not what was proven"
check_db_fingerprints "proof" || exit "$EXIT_PROOF_REFUSED"

paper_db_dir=$(dirname "$PAPER_DB_CANON")
directive=$(update_paper_override_directive "$paper_db_dir")
[[ -n "$directive" ]] || fail "$EXIT_OVERRIDE_REFUSED" "cannot derive a writable-path directive for $paper_db_dir"
printf '[Service]\n%s\n' "$directive" > "$STAGED_OVERRIDE"
echo "override: $DROPIN"
sed 's/^/override:   /' "$STAGED_OVERRIDE"
if command -v "$SYSTEMD_ANALYZE" >/dev/null 2>&1; then
    unit_src="$UNIT_DIR/$LIVE_UNIT"
    if [[ ! -f "$unit_src" && "$LIVE_UNIT" == *@*.service ]]; then
        unit_src="$UNIT_DIR/${LIVE_UNIT%%@*}@.service"
    fi
    if [[ -f "$unit_src" ]]; then
        mkdir -p "$WORK/units/${LIVE_UNIT}.d"
        cp "$unit_src" "$WORK/units/$LIVE_UNIT"
        if [[ -d "$UNIT_DIR/${LIVE_UNIT}.d" ]]; then
            cp "$UNIT_DIR/${LIVE_UNIT}.d"/*.conf "$WORK/units/${LIVE_UNIT}.d/" 2>/dev/null || true
        fi
        cp "$STAGED_OVERRIDE" "$WORK/units/${LIVE_UNIT}.d/50-merge-paper-${PAPER}.conf"
        if ! "$SYSTEMD_ANALYZE" verify "$WORK/units/$LIVE_UNIT" >"$WORK/analyze.out" 2>&1; then
            cat "$WORK/analyze.out" >&2
            fail "$EXIT_OVERRIDE_REFUSED" "systemd-analyze verify rejects $LIVE_UNIT with the override"
        fi
        echo "override: systemd-analyze verify passed"
    else
        echo "override: unit file for $LIVE_UNIT not found under $UNIT_DIR; systemd-analyze verify skipped"
    fi
else
    echo "override: systemd-analyze not available; verify skipped"
fi

cat <<EOF
after apply, run in this order:
  1. $SYSTEMCTL daemon-reload
  2. $SYSTEMCTL disable $PAPER_UNIT
  3. $SYSTEMCTL start $LIVE_UNIT
  4. journalctl -u $LIVE_UNIT -n 50 | grep '\[storage\]'
  5. retire the paper instance's status port${paper_status_port:+ ($paper_status_port)} and any tunnel mapping that pointed at it; the combined process serves both scopes on the live port
EOF

if [[ "$MODE" != "apply" ]]; then
    echo "VERDICT: READY (dry run; staged files: $STAGED_CFG, $STAGED_OVERRIDE)"
    exit 0
fi

[[ "$(update_file_fingerprint "$LIVE_CFG")" == "$fp_live_cfg" ]] || fail "$EXIT_SOURCE_CHANGED" "$LIVE_CFG changed during the run"
[[ "$(update_file_fingerprint "$PAPER_CFG")" == "$fp_paper_cfg" ]] || fail "$EXIT_SOURCE_CHANGED" "$PAPER_CFG changed during the run"
check_db_fingerprints "pre-apply" || exit "$EXIT_SOURCE_CHANGED"
require_units_stopped

run_id="$(date +%Y%m%d%H%M%S)-$$"
{
    printf 'run_id %s\n' "$run_id"
    printf 'live_config %s\n' "$fp_live_cfg"
    printf 'paper_config %s\n' "$fp_paper_cfg"
    printf 'live_db %s\n' "$fp_live_db"
    printf 'paper_db %s\n' "$fp_paper_db"
    printf 'staged_config %s\n' "$(update_file_fingerprint "$STAGED_CFG")"
    printf 'staged_override %s\n' "$(update_file_fingerprint "$STAGED_OVERRIDE")"
    if [[ -n "$ALIGN_OUT" ]]; then
        printf '%s\n' "$ALIGN_OUT"
    fi
    if [[ -e "$DROPIN" ]]; then
        printf 'override_prior present\n'
        printf 'override_prior_fp %s\n' "$(update_file_fingerprint "$DROPIN")"
    else
        printf 'override_prior absent\n'
    fi
} > "$JOURNAL"

apply_failed() {
    echo "apply: $1" >&2
    if restore_from_retained; then
        check_db_fingerprints "rollback" || exit "$EXIT_RESTORE_FAILED"
        echo "apply: rolled back; both units stay stopped" >&2
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

printf 'override begin\n' >> "$JOURNAL"
if [[ -e "$DROPIN" ]]; then
    cp -p "$DROPIN" "$RETAINED_DROPIN" || apply_failed "could not retain $DROPIN"
fi
if ! mkdir -p "$(dirname "$DROPIN")"; then
    apply_failed "could not create $(dirname "$DROPIN")"
fi
if ! cp "$STAGED_OVERRIDE" "$(dirname "$DROPIN")/.$(basename "$DROPIN").staged" 2>/dev/null; then
    apply_failed "could not stage the override under $(dirname "$DROPIN")"
fi
mv -f "$(dirname "$DROPIN")/.$(basename "$DROPIN").staged" "$DROPIN" || apply_failed "could not install $DROPIN"
printf 'override done\n' >> "$JOURNAL"
echo "apply: installed $DROPIN"
[[ "$FAIL_AFTER" != "override" ]] || apply_failed "MERGE_PAPER_FAIL_AFTER=override"

rm -f "$STAGED_MAP" "$STAGED_OVERRIDE"
check_db_fingerprints "post-apply" || exit "$EXIT_RESTORE_FAILED"
{
    printf 'result_config %s\n' "$(update_file_fingerprint "$LIVE_CFG")"
    printf 'result_override %s\n' "$(update_file_fingerprint "$DROPIN")"
    printf 'complete\n'
} >> "$JOURNAL"
echo "VERDICT: APPLIED (journal $JOURNAL). Run the commands above in order."
exit 0
