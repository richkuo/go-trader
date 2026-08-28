#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
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
    unsets = sorted((e for e in group if e[2] == "unset"), key=lambda e: e[1])
    if not lives:
        continue
    if not papers and unsets:
        papers = unsets
        unsets = []
    for u in unsets:
        print()
        print("UNPAIRED (unset mode) %s at %s — no --mode token; the daemon runs it as paper but an explicit --mode=paper twin exists. Add --mode=paper to audit it directly."
              % (sid, u[1]))
    if not papers:
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
            note = " (no --mode token — daemon default is paper)" if paper[2] == "unset" else ""
            print("  paper: %s%s" % (paper[1], note))
            for k, lv, pv in watched_diffs:
                print("  DRIFT  %-22s live=%-14s paper=%s" % (k, fmt(lv), fmt(pv)))
            for k, lv, pv in other_diffs:
                print("  OTHER  %-22s live=%-14s paper=%s" % (k, fmt(lv), fmt(pv)))
            if watched_diffs and other_diffs:
                skip_pairs += 1
                drift_pairs += 1
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
    print("VERDICT: DRIFT — %d live/paper pair(s) with cadence/sizing drift, %d flagged SKIP (left alone), %d in sync"
          % (drift_pairs, skip_pairs, sync_pairs))
    sys.exit(1)
total = drift_pairs + skip_pairs + sync_pairs
if total == 0:
    print("VERDICT: OK — no live/paper pairs found across %d deployment(s)" % len(rows))
else:
    print("VERDICT: OK — %d pair(s) in sync, %d flagged SKIP (left alone)" % (sync_pairs, skip_pairs))
sys.exit(0)
PY
