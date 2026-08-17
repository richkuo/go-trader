#!/usr/bin/env bash
# check-live-paper-config-drift.sh — READ-ONLY fleet audit of live/paper
# per-strategy cadence + sizing drift (#1430).
#
# When one strategy id runs in both a live and a paper deployment, the two
# per-strategy config blocks can drift in cadence (interval_seconds) and
# sizing (leverage, sizing_leverage, margin_per_trade_usd, capital,
# capital_pct, initial_capital), making the two books incomparable. This
# script finds every live/paper pair across the fleet and prints the drift.
#
# A pair is a sync CANDIDATE only when its differences are limited to
# cadence, sizing, and the --mode arg; any OTHER differing field (script,
# args beyond --mode, close_strategy, …) flags the pair SKIP — leave it
# alone, it has a documented reason to differ or needs human review.
#
# Usage:
#   bash scripts/check-live-paper-config-drift.sh                    # auto-discover active systemd deployments (#1055)
#   bash scripts/check-live-paper-config-drift.sh /opt/go-trader ... # audit explicit deployment dirs instead
#
# Discovery mirrors update.sh --all: active go-trader systemd units, reading
# each unit's ExecStart --config path (the #1056 out-of-tree location) and
# falling back to <WorkingDirectory>/scheduler/config.json (the transition
# symlink, or the legacy in-tree file).
#
# The script never writes anything — no config rewrite, no daemon
# interaction. Mode classification mirrors the daemon: a strategy is live
# when its args carry --mode=live (or "--mode live"), paper when they carry
# --mode=paper; anything else is "unset" and never forms a pair.
#
# Exit codes:
#   0 — every live/paper pair in sync on cadence/sizing (SKIP-only flags do
#       not gate: those pairs are deliberately left alone)
#   1 — cadence/sizing drift found, or a deployment unreadable/missing
#   2 — nothing to audit (an empty audit is NOT a verified fleet)
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=update_helpers.sh
source "${SCRIPT_DIR}/update_helpers.sh"

rows_file=$(mktemp)
trap 'rm -f "$rows_file"' EXIT
rows=0

add_row() {
    local source="$1" cfg_path="$2"
    printf '%s\t%s\n' "$source" "$cfg_path" >> "$rows_file"
    rows=$((rows + 1))
}

if [[ $# -gt 0 ]]; then
    for dir in "$@"; do
        dir=$(canonicalize_deployment_dir "$dir")
        add_row "$dir" "${dir}scheduler/config.json"
    done
else
    if ! command -v systemctl >/dev/null 2>&1; then
        echo "ERROR: systemctl not available and no deployment dirs given — pass dirs explicitly" >&2
        exit 2
    fi
    globs=()
    while IFS= read -r g; do
        [[ -n "$g" ]] && globs+=("$g")
    done < <(update_systemd_unit_globs)
    units=()
    while IFS= read -r unit; do
        [[ -n "$unit" ]] && units+=("$unit")
    done < <(systemctl list-units --type=service --state=active --no-legend --plain "${globs[@]}" 2>/dev/null | awk '{print $1}')
    if [[ ${#units[@]} -eq 0 ]]; then
        echo "ERROR: no active go-trader systemd units found — an empty audit is not a verified fleet" >&2
        exit 2
    fi
    for unit in "${units[@]}"; do
        execstart=$(systemctl show "$unit" -p ExecStart --value 2>/dev/null || true)
        cfg_path=$(update_execstart_config_path "$execstart")
        if [[ -z "$cfg_path" ]]; then
            wd=$(systemctl show "$unit" -p WorkingDirectory --value 2>/dev/null || true)
            if [[ -z "$wd" ]]; then
                # Record an unreadable row so the audit fails loudly instead of
                # silently skipping a deployment.
                add_row "$unit" "-"
                continue
            fi
            cfg_path="${wd%/}/scheduler/config.json"
        fi
        add_row "$unit" "$cfg_path"
    done
fi

if [[ "$rows" -eq 0 ]]; then
    echo "ERROR: nothing to audit" >&2
    exit 2
fi

python3 - "$rows_file" <<'PY'
import json
import sys

# Cadence + sizing fields the #1430 sync runbook is allowed to touch.
WATCHED = [
    "interval_seconds",
    "leverage",
    "sizing_leverage",
    "margin_per_trade_usd",
    "capital",
    "capital_pct",
    "initial_capital",
]
MISSING = object()


def classify(args):
    # Mirrors scheduler/state_presence.go isLiveArgs: --mode=live or
    # "--mode live" wins; then explicit paper; anything else is unset and
    # never forms a pair.
    for i, a in enumerate(args):
        if a == "--mode=live":
            return "live"
        if a == "--mode" and i + 1 < len(args) and args[i + 1] == "live":
            return "live"
    for i, a in enumerate(args):
        if a == "--mode=paper":
            return "paper"
        if a == "--mode" and i + 1 < len(args) and args[i + 1] == "paper":
            return "paper"
    return "unset"


def strip_mode(args):
    # Remove the --mode token(s) so a pair whose args differ ONLY by
    # --mode=live vs --mode=paper is not flagged as other-drift.
    out = []
    skip = False
    for a in args:
        if skip:
            skip = False
            continue
        if a in ("--mode=live", "--mode=paper"):
            continue
        if a == "--mode":
            skip = True
            continue
        out.append(a)
    return out


def fmt(v):
    if v is MISSING:
        return "-"
    return json.dumps(v)


rows = []
with open(sys.argv[1]) as f:
    for line in f:
        line = line.rstrip("\n")
        if line:
            rows.append(line.partition("\t"))

print("go-trader live/paper config drift audit (#1430)")
print("%-40s %-60s %s" % ("DEPLOYMENT", "CONFIG", "STRATEGIES live/paper/unset"))

bad = 0
entries = []  # (id, source, mode, block)
for source, _, path in rows:
    try:
        with open(path) as f:
            cfg = json.load(f)
    except Exception:
        print("%-40s %-60s %s" % (source, path, "FAIL (unreadable)"))
        bad += 1
        continue
    strategies = cfg.get("strategies")
    if not isinstance(strategies, list):
        print("%-40s %-60s %s" % (source, path, "FAIL (no strategies list)"))
        bad += 1
        continue
    counts = {"live": 0, "paper": 0, "unset": 0}
    for s in strategies:
        if not isinstance(s, dict) or not isinstance(s.get("id"), str):
            continue
        args = s.get("args")
        if not isinstance(args, list):
            args = []
        args = [a for a in args if isinstance(a, str)]
        mode = classify(args)
        counts[mode] += 1
        entries.append((s["id"], source, mode, s))
    print("%-40s %-60s %d/%d/%d" % (source, path, counts["live"], counts["paper"], counts["unset"]))

by_id = {}
for e in entries:
    by_id.setdefault(e[0], []).append(e)

drift_pairs = 0
skip_pairs = 0
sync_pairs = 0
for sid in sorted(by_id):
    group = by_id[sid]
    lives = sorted((e for e in group if e[2] == "live"), key=lambda e: e[1])
    papers = sorted((e for e in group if e[2] == "paper"), key=lambda e: e[1])
    if not lives or not papers:
        continue
    for live in lives:
        for paper in papers:
            lblock, pblock = live[3], paper[3]
            watched_diffs = []
            for k in WATCHED:
                lv = lblock.get(k, MISSING)
                pv = pblock.get(k, MISSING)
                if lv is MISSING and pv is MISSING:
                    continue
                if lv is MISSING or pv is MISSING or lv != pv:
                    watched_diffs.append((k, lv, pv))
            other_diffs = []
            for k in sorted((set(lblock) | set(pblock)) - set(WATCHED) - {"id"}):
                lv = lblock.get(k, MISSING)
                pv = pblock.get(k, MISSING)
                if k == "args":
                    la = strip_mode(lv) if isinstance(lv, list) else lv
                    pa = strip_mode(pv) if isinstance(pv, list) else pv
                    if la != pa:
                        other_diffs.append((k, la, pa))
                    continue
                if lv is MISSING and pv is MISSING:
                    continue
                if lv is MISSING or pv is MISSING or lv != pv:
                    other_diffs.append((k, lv, pv))
            print()
            print("PAIR %s" % sid)
            print("  live : %s" % live[1])
            print("  paper: %s" % paper[1])
            for k, lv, pv in watched_diffs:
                print("  DRIFT  %-22s live=%-14s paper=%s" % (k, fmt(lv), fmt(pv)))
            for k, lv, pv in other_diffs:
                print("  OTHER  %-22s live=%-14s paper=%s" % (k, fmt(lv), fmt(pv)))
            if watched_diffs and other_diffs:
                skip_pairs += 1
                print("  VERDICT: SKIP — cadence/sizing drift present, but other fields differ; leave this pair alone")
            elif watched_diffs:
                drift_pairs += 1
                print("  VERDICT: CANDIDATE — differences limited to cadence/sizing/--mode")
            elif other_diffs:
                skip_pairs += 1
                print("  VERDICT: SKIP (in sync on cadence/sizing) — other fields differ; leave this pair alone")
            else:
                sync_pairs += 1
                print("  VERDICT: IN SYNC")

print()
if bad:
    print("VERDICT: FAIL — %d deployment(s) unreadable; cannot certify the fleet" % bad)
    sys.exit(1)
if drift_pairs:
    print("VERDICT: DRIFT — %d live/paper pair(s) with syncable cadence/sizing drift, %d SKIP, %d in sync"
          % (drift_pairs, skip_pairs, sync_pairs))
    sys.exit(1)
total = drift_pairs + skip_pairs + sync_pairs
if total == 0:
    print("VERDICT: OK — no live/paper pairs found across %d deployment(s)" % len(rows))
else:
    print("VERDICT: OK — %d pair(s) in sync, %d flagged SKIP (left alone)" % (sync_pairs, skip_pairs))
sys.exit(0)
PY
