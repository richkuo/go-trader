#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
source "${SCRIPT_DIR}/update_helpers.sh"

rows_file=$(mktemp)
trap 'rm -f "$rows_file"' EXIT
rows=0

add_row() {
    local source="$1" cfg_path="$2" deploy_dir="$3"
    printf '%s\t%s\t%s\n' "$source" "$cfg_path" "$deploy_dir" >> "$rows_file"
    rows=$((rows + 1))
}

if [[ $# -gt 0 ]]; then
    for dir in "$@"; do
        dir=$(canonicalize_deployment_dir "$dir")
        add_row "$dir" "${dir}scheduler/config.json" "$dir"
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
        wd=$(systemctl show "$unit" -p WorkingDirectory --value 2>/dev/null || true)
        if [[ -z "$cfg_path" ]]; then
            if [[ -z "$wd" ]]; then
                add_row "$unit" "-" ""
                continue
            fi
            cfg_path="${wd%/}/scheduler/config.json"
        fi
        add_row "$unit" "$cfg_path" "$wd"
    done
fi

if [[ "$rows" -eq 0 ]]; then
    echo "ERROR: nothing to audit" >&2
    exit 2
fi

python3 - "$rows_file" <<'PY'
import json
import os
import subprocess
import sys

WATCHED = [
    "interval_seconds",
    "leverage",
    "sizing_leverage",
    "margin_per_trade_usd",
    "risk_per_trade_pct",
    "capital",
    "capital_pct",
    "initial_capital",
]
IDENTITY_KEYS = ("platform", "type", "symbol", "timeframe")
MISSING = object()
INSPECT_WRAPPER = 'cd "$1" || exit 1; if [ -f .env ]; then set -a; . ./.env; set +a; fi; exec ./go-trader inspect --all --json --config "$2"'


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


def id_note(block, key):
    sid = block.get("id")
    if isinstance(sid, str) and sid != key:
        return " [id=%s]" % sid
    return ""


def strip_paper_suffix(sid):
    if sid.endswith("-paper"):
        return sid[: -len("-paper")]
    head, sep, tail = sid.rpartition("-paper")
    if sep and tail.isdigit():
        return head
    return None


def identity(block):
    args = [a for a in (block.get("args") or []) if isinstance(a, str)]
    args = strip_mode(args)
    platform = block.get("platform") or ("hyperliquid" if block["id"].startswith("hl-") else "")
    symbol = block.get("symbol") or (args[1] if len(args) > 1 else "")
    timeframe = block.get("timeframe") or (args[2] if len(args) > 2 and not args[2].startswith("--") else "")
    return {"platform": platform, "type": block.get("type") or "", "symbol": symbol, "timeframe": timeframe}


def effective_view(deploy_dir, cfg_path):
    if not deploy_dir:
        return None, None
    binary = os.path.join(deploy_dir, "go-trader")
    if not os.access(binary, os.X_OK):
        return None, None
    try:
        proc = subprocess.run(["bash", "-c", INSPECT_WRAPPER, "_", deploy_dir, cfg_path],
                              capture_output=True, text=True, timeout=120)
    except (OSError, subprocess.SubprocessError) as exc:
        return None, "inspect failed: %s" % exc
    if proc.returncode != 0:
        return None, "inspect exit %d: %s" % (proc.returncode, proc.stderr.strip().splitlines()[-1:] or "")
    try:
        docs = json.loads(proc.stdout)
    except ValueError as exc:
        return None, "inspect output is not JSON: %s" % exc
    return dict((d["id"], d) for d in docs if isinstance(d, dict) and isinstance(d.get("id"), str)), None


def fmt(v):
    if v is MISSING:
        return "-"
    return json.dumps(v)


rows = []
with open(sys.argv[1]) as f:
    for line in f:
        line = line.rstrip("\n")
        if line:
            parts = line.split("\t")
            while len(parts) < 3:
                parts.append("")
            rows.append(parts)

print("go-trader live/paper config drift audit (#1430)")
print("%-40s %-60s %s" % ("DEPLOYMENT", "CONFIG", "STRATEGIES live/paper/unset"))

bad = 0
entries = []
for source, path, deploy_dir in rows:
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
    effective, eff_err = effective_view(deploy_dir, path)
    if eff_err:
        print("%-40s %-60s %s" % (source, path, "FAIL (effective view unreadable: %s)" % eff_err))
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
        eff = effective.get(s["id"]) if effective is not None else None
        entries.append({"source": source, "mode": mode, "block": s, "effective": eff, "has_effective": effective is not None})
    marker = "" if effective is not None else " (RAW: no go-trader binary beside the config)"
    print("%-40s %-60s %d/%d/%d%s" % (source, path, counts["live"], counts["paper"], counts["unset"], marker))

lives = [e for e in entries if e["mode"] == "live"]
live_ids = set(e["block"]["id"] for e in lives)
ambiguous = []
unpaired_alias = []
by_key = {}
for e in entries:
    if e["mode"] == "live":
        by_key.setdefault(e["block"]["id"], []).append(e)
        continue
    block = e["block"]
    sid = block["id"]
    src = block.get("replay_source_id")
    if isinstance(src, str) and src.strip():
        key = src.strip()
    elif sid in live_ids:
        key = sid
    else:
        base = strip_paper_suffix(sid)
        storage = block.get("storage_strategy_id")
        if base is not None and base in live_ids:
            if isinstance(storage, str) and storage.strip() == base:
                key = base
            else:
                ambiguous.append(e)
                continue
        else:
            if base is not None:
                unpaired_alias.append((e, base))
            key = sid
    by_key.setdefault(key, []).append(e)

for e, base in unpaired_alias:
    print()
    print("UNPAIRED (no live twin) %s at %s — the -paper alias names %s and no audited deployment runs a live strategy with that id; add the live twin or audit the deployment that holds it"
          % (e["block"]["id"], e["source"], base))

for e in ambiguous:
    print()
    print("AMBIGUOUS %s at %s — the -paper suffix alone does not prove a twin of %s; set replay_source_id or storage_strategy_id to pair it"
          % (e["block"]["id"], e["source"], strip_paper_suffix(e["block"]["id"])))

drift_pairs = 0
skip_pairs = 0
sync_pairs = 0
incompatible_pairs = 0
for sid in sorted(by_key):
    group = by_key[sid]
    lives_g = sorted((e for e in group if e["mode"] == "live"), key=lambda e: e["source"])
    papers = sorted((e for e in group if e["mode"] == "paper"), key=lambda e: e["source"])
    unsets = sorted((e for e in group if e["mode"] == "unset"), key=lambda e: e["source"])
    if not lives_g:
        continue
    if not papers and unsets:
        papers = unsets
        unsets = []
    for u in unsets:
        print()
        print("UNPAIRED (unset mode) %s at %s — no --mode token; the daemon runs it as paper but an explicit --mode=paper twin exists. Add --mode=paper to audit it directly."
              % (sid, u["source"]))
    if not papers:
        continue
    for live in lives_g:
        for paper in papers:
            lblock, pblock = live["block"], paper["block"]
            use_effective = live["effective"] is not None and paper["effective"] is not None
            watched_diffs = []
            for k in WATCHED:
                if use_effective:
                    lv = live["effective"].get(k, MISSING)
                    pv = paper["effective"].get(k, MISSING)
                    if lv is None:
                        lv = MISSING
                    if pv is None:
                        pv = MISSING
                else:
                    lv = lblock.get(k, MISSING)
                    pv = pblock.get(k, MISSING)
                if lv is MISSING and pv is MISSING:
                    continue
                if lv is MISSING or pv is MISSING or lv != pv:
                    watched_diffs.append((k, lv, pv))
            other_diffs = []
            for k in sorted((set(lblock) | set(pblock)) - set(WATCHED) - {"id", "replay_source_id", "storage_strategy_id"}):
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
            lid, pid = identity(lblock), identity(pblock)
            incompatible = [(k, lid[k], pid[k]) for k in IDENTITY_KEYS if lid[k] != pid[k]]
            print()
            print("PAIR %s" % sid)
            print("  live : %s%s" % (live["source"], id_note(lblock, sid)))
            note = " (no --mode token — daemon default is paper)" if paper["mode"] == "unset" else ""
            print("  paper: %s%s%s" % (paper["source"], id_note(pblock, sid), note))
            basis = "effective" if use_effective else "RAW"
            for k, lv, pv in incompatible:
                print("  INCOMPATIBLE %-16s live=%-14s paper=%s" % (k, json.dumps(lv), json.dumps(pv)))
            for k, lv, pv in watched_diffs:
                print("  DRIFT  %-22s live=%-14s paper=%s (%s)" % (k, fmt(lv), fmt(pv), basis))
            for k, lv, pv in other_diffs:
                print("  OTHER  %-22s live=%-14s paper=%s" % (k, fmt(lv), fmt(pv)))
            if incompatible:
                incompatible_pairs += 1
                skip_pairs += 1
                print("  VERDICT: SKIP — INCOMPATIBLE: the pair differs in %s; these are not twins, leave this pair alone" % ", ".join(k for k, _, _ in incompatible))
            elif watched_diffs and other_diffs:
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
                print("  VERDICT: IN SYNC (%s)" % basis)

print()
if bad:
    print("VERDICT: FAIL — %d deployment(s) unreadable; cannot certify the fleet" % bad)
    sys.exit(1)
if ambiguous:
    print("VERDICT: FAIL — %d ambiguous twin(s), %d incompatible pair(s) left alone; resolve the pairing before trusting the audit"
          % (len(ambiguous), incompatible_pairs))
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
