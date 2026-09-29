#!/usr/bin/env bash

set -euo pipefail

THIS_SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
source "${THIS_SCRIPT_DIR}/update_helpers.sh"

usage() {
    cat <<'EOF'
Usage: scripts/feed-parity.sh [--since <time>] [--until <time>]
           (--feed-unit <unit> | --feed-log <label>=<file>) ...
           (--consumer-unit <unit> | --consumer-log <label>=<file>) ...

Joins the market feed's [feed-seal] records with each shared-mode scheduler's
[feed-audit] and [feed-payload] records by key and fails when:
  - a feed or consumer input has no records (empty input),
  - a consumer skipped a key of a cadence while its audit records listed that
    cadence as active (missing coverage),
  - a consumer's seal hash differs from the feed's hash for that key and instance,
  - two consumers built different market payloads from the same seal and frame
    specification, or a consumer audited a key the feed never sealed.
Accepted exceptions are listed and do not fail the run: degraded keys (no
compatible seal, reason logged), skipped keys (the key passed its give-up time
before the consumer evaluated it, for example after a restart), consumers served by different sources or
instances for one key (source-switch race), and REST mark fallback coins.

Journal inputs read the unit's own LogNamespace (journalctl --namespace=+<ns>).
--since/--until are passed to journalctl; file inputs are read whole.
EOF
}

since=""
until_arg=""
declare -a feed_inputs=()
declare -a consumer_inputs=()

while [[ $# -gt 0 ]]; do
    case "$1" in
        --since) since="${2:-}"; shift 2 ;;
        --until) until_arg="${2:-}"; shift 2 ;;
        --feed-unit) feed_inputs+=("unit:${2:-}"); shift 2 ;;
        --feed-log) feed_inputs+=("file:${2:-}"); shift 2 ;;
        --consumer-unit) consumer_inputs+=("unit:${2:-}"); shift 2 ;;
        --consumer-log) consumer_inputs+=("file:${2:-}"); shift 2 ;;
        -h|--help) usage; exit 0 ;;
        *) echo "unknown arg: $1" >&2; usage >&2; exit 2 ;;
    esac
done

if [[ ${#feed_inputs[@]} -eq 0 || ${#consumer_inputs[@]} -eq 0 ]]; then
    echo "feed-parity: need at least one feed input and one consumer input" >&2
    usage >&2
    exit 2
fi

work=$(mktemp -d "${TMPDIR:-/tmp}/feed-parity.XXXXXX")
trap 'rm -rf "$work"' EXIT

collect() {
    local kind="$1" spec="$2" idx="$3"
    local label out
    if [[ "$spec" == unit:* ]]; then
        local unit="${spec#unit:}"
        [[ -n "$unit" ]] || { echo "feed-parity: empty unit name" >&2; exit 2; }
        label="$unit"
        out="$work/${kind}-${idx}.log"
        local fragment namespace cmd
        fragment=$(systemctl show -p FragmentPath --value "$unit" 2>/dev/null || true)
        namespace=$(update_unit_log_namespace "$fragment")
        cmd=$(update_journalctl_unit_command "$unit" "$namespace")
        local -a argv=()
        read -r -a argv <<<"$cmd"
        argv+=(-o cat --no-pager)
        [[ -n "$since" ]] && argv+=(--since "$since")
        [[ -n "$until_arg" ]] && argv+=(--until "$until_arg")
        echo "feed-parity: reading $unit via: ${argv[*]}" >&2
        "${argv[@]}" >"$out" 2>/dev/null || true
    else
        local pair="${spec#file:}"
        if [[ "$pair" != *=* ]]; then
            echo "feed-parity: log input must be <label>=<file>, got $pair" >&2
            exit 2
        fi
        label="${pair%%=*}"
        out="${pair#*=}"
        [[ -f "$out" ]] || { echo "feed-parity: $out is not a file" >&2; exit 2; }
    fi
    printf '%s\t%s\t%s\n' "$kind" "$label" "$out" >>"$work/inputs.tsv"
}

i=0
for spec in "${feed_inputs[@]}"; do
    collect feed "$spec" "$i"
    i=$((i + 1))
done
for spec in "${consumer_inputs[@]}"; do
    collect consumer "$spec" "$i"
    i=$((i + 1))
done

python3 - "$work/inputs.tsv" <<'PY'
import re
import sys
from collections import defaultdict

FIELD = re.compile(r'(\w+)=("(?:[^"\\]|\\.)*"|\S+)')


def fields(line):
    out = {}
    for k, v in FIELD.findall(line):
        if v.startswith('"') and v.endswith('"'):
            v = v[1:-1]
        out[k] = v
    return out


feeds = []
consumers = []
with open(sys.argv[1]) as fh:
    for row in fh:
        kind, label, path = row.rstrip("\n").split("\t")
        (feeds if kind == "feed" else consumers).append((label, path))

failures = []
exceptions = []

seals = {}
for label, path in feeds:
    n = 0
    with open(path, errors="replace") as fh:
        for line in fh:
            idx = line.find("[feed-seal]")
            if idx < 0:
                continue
            f = fields(line[idx:])
            if f.get("status") != "sealed":
                if f.get("status") in ("missed", "failed"):
                    exceptions.append(f"feed {label}: key {f.get('key')} {f.get('status')} ({f.get('reason', '')})")
                continue
            n += 1
            seals[(f["instance"], int(f["key"]))] = (label, f)
    if n == 0:
        failures.append(f"feed {label}: no sealed [feed-seal] records (empty input)")

feed_keys = sorted({k for (_, k) in seals})
lo = feed_keys[0] if feed_keys else None
hi = feed_keys[-1] if feed_keys else None

audits = {}
payloads = defaultdict(dict)
fallbacks = []
for label, path in consumers:
    recs = {}
    with open(path, errors="replace") as fh:
        for line in fh:
            idx = line.find("[feed-audit]")
            if idx >= 0:
                f = fields(line[idx:])
                if "mark_fallback" in f:
                    if f["mark_fallback"]:
                        fallbacks.append(f"consumer {label}: key {f['key']} REST mark fallback for {f['mark_fallback']}")
                    continue
                key = int(f["key"])
                if f.get("status") == "skipped" and key in recs:
                    continue
                recs[key] = f
                continue
            idx = line.find("[feed-payload]")
            if idx >= 0:
                f = fields(line[idx:])
                spec = (int(f["key"]), f.get("frames", ""), f.get("coins", ""))
                prev = payloads[spec].get(label)
                if prev and prev != f["sha256"]:
                    failures.append(f"consumer {label}: key {spec[0]} frames {spec[1]} built two different payloads ({prev[:12]} vs {f['sha256'][:12]})")
                payloads[spec][label] = f["sha256"]
    if not recs:
        failures.append(f"consumer {label}: no [feed-audit] records (empty input)")
    audits[label] = recs

for label, recs in audits.items():
    if not recs:
        continue
    keys = sorted(recs)
    started = None
    for f in recs.values():
        if f.get("at", "").isdigit():
            at = int(f["at"])
            started = at if started is None else min(started, at)
    runs = defaultdict(list)
    open_runs = {}
    for k in keys:
        active = {int(c) for c in recs[k].get("cadences", "none").split(",") if c.isdigit()}
        for c in [c for c in open_runs if c not in active]:
            runs[c].append(open_runs.pop(c))
        for c in active:
            if c in open_runs:
                open_runs[c][1] = k
            else:
                open_runs[c] = [k, k]
    for c, run in open_runs.items():
        runs[c].append(run)
    expected = set()
    for c, spans in runs.items():
        for first, last in spans:
            if lo is not None:
                first = max(first, lo)
                last = min(last, hi)
            start = ((first + c - 1) // c) * c
            if started is not None:
                start = max(start, (started // c) * c)
            for k in range(start, last + 1, c):
                expected.add(k)
    missing = sorted(expected - set(recs))
    if missing:
        failures.append(f"consumer {label}: {len(missing)} expected key(s) missing: {', '.join(map(str, missing[:20]))}{' ...' if len(missing) > 20 else ''}")
    for k in keys:
        f = recs[k]
        if f.get("status") == "degraded":
            exceptions.append(f"consumer {label}: key {k} degraded ({f.get('reason', '')})")
            continue
        if f.get("status") == "skipped":
            exceptions.append(f"consumer {label}: key {k} skipped for {f.get('ids', '')} ({f.get('reason', '')})")
            continue
        if lo is None or not (lo <= k <= hi):
            continue
        seal = seals.get((f.get("instance"), k))
        if seal is None:
            failures.append(f"consumer {label}: key {k} was served by instance {f.get('instance')} but no feed input logged that seal")
            continue
        if seal[1]["hash"] != f.get("hash"):
            failures.append(f"consumer {label}: key {k} hash {f.get('hash', '')[:12]} differs from feed {seal[0]} hash {seal[1]['hash'][:12]}")

labels = sorted(audits)
same_source = 0
switch = 0
for i, a in enumerate(labels):
    for b in labels[i + 1:]:
        for k in sorted(set(audits[a]) & set(audits[b])):
            fa, fb = audits[a][k], audits[b][k]
            if fa.get("status") != "sealed" or fb.get("status") != "sealed":
                continue
            if (fa.get("instance"), fa.get("source")) != (fb.get("instance"), fb.get("source")):
                switch += 1
                exceptions.append(f"key {k}: {a} used {fa.get('source')}/{fa.get('instance')} and {b} used {fb.get('source')}/{fb.get('instance')} (source-switch race, not a match)")
                continue
            same_source += 1
            if fa.get("hash") != fb.get("hash"):
                failures.append(f"key {k}: {a} and {b} report different hashes from the same source and instance")

payload_matches = 0
for spec, per in sorted(payloads.items()):
    if len(per) < 2:
        continue
    key = spec[0]
    served = {}
    for lbl in per:
        f = audits.get(lbl, {}).get(key)
        served[lbl] = (f.get("instance"), f.get("hash")) if f else None
    groups = defaultdict(list)
    for lbl, sha in per.items():
        groups[served[lbl]].append((lbl, sha))
    for seal_id, members in groups.items():
        if len(members) < 2:
            continue
        shas = {sha for _, sha in members}
        if len(shas) > 1:
            failures.append(f"key {key} frames {spec[1]} coins {spec[2]}: consumers built different payloads from one seal: " +
                            ", ".join(f"{lbl}={sha[:12]}" for lbl, sha in sorted(members)))
        else:
            payload_matches += 1

print(f"feed-parity: {len(seals)} sealed feed records, keys {lo}..{hi}")
for label in labels:
    recs = audits[label]
    sealed = sum(1 for f in recs.values() if f.get("status") == "sealed")
    skipped = sum(1 for f in recs.values() if f.get("status") == "skipped")
    print(f"feed-parity: consumer {label}: {len(recs)} audited keys ({sealed} sealed, {len(recs) - sealed - skipped} degraded, {skipped} skipped)")
print(f"feed-parity: {same_source} cross-consumer same-source key matches, {switch} source-switch exceptions, {payload_matches} shared payload specs matched")
for line in fallbacks:
    print("feed-parity: EXCEPTION " + line)
for line in exceptions:
    print("feed-parity: EXCEPTION " + line)
for line in failures:
    print("feed-parity: FAIL " + line)
if failures:
    print(f"feed-parity: FAIL ({len(failures)} problem(s))")
    sys.exit(1)
print("feed-parity: PASS")
PY
