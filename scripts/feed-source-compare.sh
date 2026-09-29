#!/usr/bin/env bash

set -euo pipefail

usage() {
    cat <<'EOF'
Usage: scripts/feed-source-compare.sh --binary <go-trader>
           --primary-socket <path> --backup-socket <path> [--key <unix>] ...
       scripts/feed-source-compare.sh --primary-seal <file> --backup-seal <file>

Compares the closed bars and funding records of the same deadline key sealed by
two feed services (normally the websocket primary and the REST backup). With
sockets and no --key, every key that both feeds still retain is compared. Seals
are read with `go-trader feed-fetch`, which verifies each seal's hash.

Compared per candle key: every closed bar (close time before the deadline) in
the open-time range both seals hold, field by field (open, high, low, close,
volume), plus bars present in only one seal inside that range. A candle key
that either seal marks not ready (for example `not_due`, `failed` or
`budget_exhausted` on the backup) is skipped with a NOTE line naming both
statuses, since its last closed bar can be a partial bar from an earlier
refresh. Funding records are compared by time and rate for every coin.

Excluded on purpose: source labels, receive times, readiness detail, instance,
generation, seal time, forming bars, mids and current funding scalars. They
differ between independent sources by design.

Exit status: 0 when no difference is found, 3 when differences are listed,
1 on a read error, 2 on bad usage. Every difference is printed with both values.
EOF
}

binary=""
primary_socket=""
backup_socket=""
primary_seal=""
backup_seal=""
declare -a keys=()

while [[ $# -gt 0 ]]; do
    case "$1" in
        --binary) binary="${2:-}"; shift 2 ;;
        --primary-socket) primary_socket="${2:-}"; shift 2 ;;
        --backup-socket) backup_socket="${2:-}"; shift 2 ;;
        --primary-seal) primary_seal="${2:-}"; shift 2 ;;
        --backup-seal) backup_seal="${2:-}"; shift 2 ;;
        --key) keys+=("${2:-}"); shift 2 ;;
        -h|--help) usage; exit 0 ;;
        *) echo "unknown arg: $1" >&2; usage >&2; exit 2 ;;
    esac
done

work=$(mktemp -d "${TMPDIR:-/tmp}/feed-source-compare.XXXXXX")
trap 'rm -rf "$work"' EXIT
pairs="$work/pairs.tsv"
: >"$pairs"

if [[ -n "$primary_seal" || -n "$backup_seal" ]]; then
    [[ -f "$primary_seal" && -f "$backup_seal" ]] || { echo "feed-source-compare: --primary-seal and --backup-seal must both name files" >&2; exit 2; }
    printf 'file\t%s\t%s\n' "$primary_seal" "$backup_seal" >>"$pairs"
else
    [[ -n "$binary" && -x "$binary" ]] || { echo "feed-source-compare: --binary must name the go-trader binary" >&2; exit 2; }
    [[ -n "$primary_socket" && -n "$backup_socket" ]] || { echo "feed-source-compare: need --primary-socket and --backup-socket" >&2; exit 2; }
    if [[ ${#keys[@]} -eq 0 ]]; then
        "$binary" feed-fetch --socket "$primary_socket" --describe >"$work/primary.describe" || exit 1
        "$binary" feed-fetch --socket "$backup_socket" --describe >"$work/backup.describe" || exit 1
        while IFS= read -r k; do
            keys+=("$k")
        done < <(python3 - "$work/primary.describe" "$work/backup.describe" <<'PY'
import json
import sys
a = set(json.load(open(sys.argv[1]))["describe"]["retained_keys"])
b = set(json.load(open(sys.argv[2]))["describe"]["retained_keys"])
for k in sorted(a & b):
    print(k)
PY
)
        if [[ ${#keys[@]} -eq 0 ]]; then
            echo "feed-source-compare: the two feeds retain no common key" >&2
            exit 1
        fi
    fi
    for k in "${keys[@]}"; do
        [[ "$k" =~ ^[0-9]+$ ]] || { echo "feed-source-compare: key $k is not a Unix second count" >&2; exit 2; }
        "$binary" feed-fetch --socket "$primary_socket" --key "$k" --out "$work/p-$k.seal" || exit 1
        "$binary" feed-fetch --socket "$backup_socket" --key "$k" --out "$work/b-$k.seal" || exit 1
        printf 'file\t%s\t%s\n' "$work/p-$k.seal" "$work/b-$k.seal" >>"$pairs"
    done
fi

python3 - "$pairs" <<'PY'
import json
import sys

FIELDS = ("o", "h", "l", "c", "v")
differences = 0
skipped_keys = 0
compared_bars = 0
compared_records = 0
pairs = [line.rstrip("\n").split("\t")[1:] for line in open(sys.argv[1]) if line.strip()]
for primary_path, backup_path in pairs:
    p = json.load(open(primary_path))
    b = json.load(open(backup_path))
    if p["key"] != b["key"]:
        print(f"feed-source-compare: ERROR seals are for different keys ({p['key']} and {b['key']})")
        sys.exit(1)
    key = p["key"]
    deadline_ms = key * 1000
    tag = f"key {key} ({p['source']}/{p['instance']} vs {b['source']}/{b['instance']})"
    pk = {(k["symbol"], k["timeframe"]): k for k in p["keys"]}
    bk = {(k["symbol"], k["timeframe"]): k for k in b["keys"]}
    for name in sorted(set(pk) ^ set(bk)):
        where = "primary" if name in pk else "backup"
        differences += 1
        print(f"feed-source-compare: DIFF {tag} {name[0]}|{name[1]}: only the {where} seal has this key")
    for name in sorted(set(pk) & set(bk)):
        label = f"{name[0]}|{name[1]}"
        pr_ready = pk[name].get("readiness") or {}
        br_ready = bk[name].get("readiness") or {}
        if not pr_ready.get("ready") or not br_ready.get("ready"):
            skipped_keys += 1
            print(f"feed-source-compare: NOTE {tag} {label}: skipped because a seal marks it not ready (primary {pr_ready.get('status')}, backup {br_ready.get('status')})")
            continue
        pbars = {bar["t"]: bar for bar in pk[name]["bars"] if bar["e"] < deadline_ms}
        bbars = {bar["t"]: bar for bar in bk[name]["bars"] if bar["e"] < deadline_ms}
        if not pbars or not bbars:
            print(f"feed-source-compare: NOTE {tag} {label}: no closed bars in one seal (primary {len(pbars)}, backup {len(bbars)})")
            continue
        lo = max(min(pbars), min(bbars))
        hi = min(max(pbars), max(bbars))
        for t in sorted(set(pbars) | set(bbars)):
            if t < lo or t > hi:
                continue
            if t not in pbars or t not in bbars:
                where = "primary" if t in pbars else "backup"
                differences += 1
                print(f"feed-source-compare: DIFF {tag} {label} bar open={t}: only the {where} seal has this closed bar")
                continue
            compared_bars += 1
            for f in FIELDS:
                if pbars[t][f] != bbars[t][f]:
                    differences += 1
                    print(f"feed-source-compare: DIFF {tag} {label} bar open={t} {f}: primary={pbars[t][f]!r} backup={bbars[t][f]!r}")
    pf = {f["coin"]: f for f in p.get("funding", [])}
    bf = {f["coin"]: f for f in b.get("funding", [])}
    for coin in sorted(set(pf) & set(bf)):
        pr = {r["time"]: r["rate"] for r in pf[coin].get("records") or []}
        br = {r["time"]: r["rate"] for r in bf[coin].get("records") or []}
        if not pr or not br:
            continue
        lo = max(min(pr), min(br))
        hi = min(max(pr), max(br))
        for t in sorted(set(pr) | set(br)):
            if t < lo or t > hi:
                continue
            if t not in pr or t not in br:
                where = "primary" if t in pr else "backup"
                differences += 1
                print(f"feed-source-compare: DIFF {tag} funding {coin} time={t}: only the {where} seal has this record")
                continue
            compared_records += 1
            if pr[t] != br[t]:
                differences += 1
                print(f"feed-source-compare: DIFF {tag} funding {coin} time={t}: primary={pr[t]!r} backup={br[t]!r}")
print(f"feed-source-compare: {len(pairs)} key pair(s), {compared_bars} closed bars and {compared_records} funding records compared, {skipped_keys} candle key(s) skipped as not ready, {differences} difference(s)")
sys.exit(3 if differences else 0)
PY
