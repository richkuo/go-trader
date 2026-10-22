#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
OUT=$(mktemp -d "${TMPDIR:-/tmp}/premium-harness.XXXXXX")
trap 'rm -rf "$OUT"' EXIT

GO_TRADER_PREMIUM_HARNESS_OUT="$OUT" GO_TRADER_V3_KINDS_OUT="$OUT/v3" \
    go -C "$ROOT/scheduler" test -run '^(TestPremiumObservationPipeline|TestV3RecordingSchemaTypesEachKind)$' -count=1 .

for f in harness.json payload_btc_1.json payload_btc_e.json payload_eth_e.json payload_btc_2.json recording/run.manifest.json \
    v3/premium/run.manifest.json v3/book/run.manifest.json v3/mixed/run.manifest.json; do
    if [[ ! -s "$OUT/$f" ]]; then
        echo "FAIL: the Go harness did not write $f" >&2
        exit 1
    fi
done

cd "$ROOT"
uv run --no-sync python backtest/observation_replay.py import-premium --run-dir "$OUT/recording" > "$OUT/import.json"
uv run --no-sync python backtest/observation_replay.py harness-premium --dir "$OUT" > "$OUT/harness.out"

SEG=$(ls "$OUT"/recording/segment-*.jsonl | head -1)
cp "$SEG" "$OUT/original.jsonl"
printf '{"k":"bbo"}\n' >> "$SEG"
if uv run --no-sync python backtest/observation_replay.py import-premium --run-dir "$OUT/recording" > /dev/null 2> "$OUT/tamper.err"; then
    echo "FAIL: the importer accepted a segment whose bytes no longer match its manifest hash" >&2
    exit 1
fi
grep -q "sha256" "$OUT/tamper.err" || { echo "FAIL: tamper refusal did not name the hash mismatch" >&2; cat "$OUT/tamper.err" >&2; exit 1; }

head -c 300 "$OUT/original.jsonl" > "$SEG"
if uv run --no-sync python backtest/observation_replay.py import-premium --run-dir "$OUT/recording" > /dev/null 2>&1; then
    echo "FAIL: the importer accepted a truncated segment" >&2
    exit 1
fi
cp "$OUT/original.jsonl" "$SEG"

uv run --no-sync python - "$OUT/recording" <<'PY'
import hashlib, json, os, sys
sys.path.insert(0, "backtest")
import premium_replay as pr
run_dir = sys.argv[1]
path = os.path.join(run_dir, "run.manifest.json")
with open(path) as fh:
    run = json.load(fh)
target = None
for idx, seg in enumerate(run["segments"]):
    blob_path = os.path.join(run_dir, seg["file"])
    with open(blob_path) as fh:
        lines = fh.read().split("\n")
    for i, line in enumerate(lines):
        if '"k":"psample"' in line and '"status":"sampled"' in line:
            rec = json.loads(line)
            rec["quote"]["bid"] = "1"
            lines[i] = json.dumps(rec, separators=(",", ":"), sort_keys=True)
            target = idx
            break
    if target is not None:
        break
if target is None:
    raise SystemExit("FAIL: the harness recording holds no sampled premium record")
body = "\n".join(lines).encode("utf-8")
digest = hashlib.sha256(body).hexdigest()
with open(blob_path, "wb") as fh:
    fh.write(body)
seg["sha256"] = digest
with open(os.path.join(run_dir, seg["file"] + ".manifest.json"), "w") as fh:
    json.dump(seg, fh)
run["segments"][target] = seg
with open(path, "w") as fh:
    json.dump(run, fh)
try:
    pr.load_premium_recording(run_dir)
except pr.PremiumRecordingError as exc:
    if "disagrees with the recorder" not in str(exc):
        raise SystemExit(f"FAIL: wrong refusal for a rehashed altered sample record: {exc}")
else:
    raise SystemExit("FAIL: the importer accepted a sample record that its raw quote and context frames contradict")
PY

uv run --no-sync python - "$OUT/v3" "$ROOT/backtest/candidates/perpetual_premium_reversion_1639/data/recording" <<'PY'
import copy, hashlib, json, os, shutil, sys
sys.path.insert(0, "backtest")
import book_replay
import observation_replay
import premium_replay

root, frozen = sys.argv[1], sys.argv[2]
IMPORTERS = {
    "l2_book": (book_replay.load_book_recording, book_replay.BookRecordingError),
    "perp_premium": (premium_replay.load_premium_recording, premium_replay.PremiumRecordingError),
    "open_interest": (observation_replay.load_recording, observation_replay.RecordingError),
}


def manifest(d):
    with open(os.path.join(d, "run.manifest.json")) as fh:
        return json.load(fh)


def accepted(d, kind):
    load, _ = IMPORTERS[kind]
    out = load(d)
    if kind == "l2_book" and (out["report"]["frames"] == 0 or out["report"]["mismatch_count"]):
        raise SystemExit(f"FAIL: {d}: the book importer did not replay its frames cleanly: {out['report']['mismatches'][:2]}")
    if kind == "perp_premium" and out["report"]["events_compared"] == 0:
        raise SystemExit(f"FAIL: {d}: the premium importer compared no sample event")
    return out


def refused(d, kind, needle, label):
    load, err = IMPORTERS[kind]
    try:
        load(d)
    except err as exc:
        if needle not in str(exc):
            raise SystemExit(f"FAIL: {label}: the {kind} importer refused for another reason: {exc}")
        return
    raise SystemExit(f"FAIL: {label}: the {kind} importer accepted the run")


for name, kinds in (("premium", ["perp_premium"]), ("book", ["l2_book"]), ("mixed", ["l2_book", "open_interest", "perp_premium"])):
    d = os.path.join(root, name)
    run = manifest(d)
    if run["schema"] != "go_trader_observation_run/v3" or run["kinds"] != kinds:
        raise SystemExit(f"FAIL: {name}: run schema {run['schema']} kinds {run['kinds']}")
    for kind in kinds:
        accepted(d, kind)
    for kind in set(IMPORTERS) - set(kinds):
        refused(d, kind, "did not record", f"{name} run read as {kind}")

frozen_run = manifest(frozen)
if (frozen_run["schema"], frozen_run["kinds"], "premium_contract" in frozen_run, "book_contract" in frozen_run) != (
        "go_trader_observation_run/v3", ["perp_premium"], True, False):
    raise SystemExit("FAIL: the frozen #1639 recording no longer has its recorded shape")
if accepted(frozen, "perp_premium")["report"]["events_compared"] != 1300:
    raise SystemExit("FAIL: the frozen #1639 recording no longer replays all 1300 sample events")

work = os.path.join(root, "work")


def variant(name, edit_run=None, edit_header=None):
    shutil.rmtree(work, ignore_errors=True)
    shutil.copytree(os.path.join(root, name), work)
    run = manifest(work)
    if edit_header is not None:
        seg = run["segments"][0]
        path = os.path.join(work, seg["file"])
        with open(path, "rb") as fh:
            lines = fh.read().decode("utf-8").split("\n")
        header = json.loads(lines[0])
        edit_header(header)
        lines[0] = json.dumps(header, separators=(",", ":"), sort_keys=True)
        blob = "\n".join(lines).encode("utf-8")
        with open(path, "wb") as fh:
            fh.write(blob)
        seg["sha256"] = hashlib.sha256(blob).hexdigest()
        seg["bytes"] = len(blob)
        with open(path + ".manifest.json", "w") as fh:
            json.dump(seg, fh)
    if edit_run is not None:
        edit_run(run)
    with open(os.path.join(work, "run.manifest.json"), "w") as fh:
        json.dump(run, fh)
    return work


book_contract = manifest(os.path.join(root, "book"))["book_contract"]
premium_contract = manifest(os.path.join(root, "premium"))["premium_contract"]
d = variant("premium", edit_run=lambda r: r.pop("premium_contract"))
refused(d, "perp_premium", "records perp_premium but carries no premium_contract", "premium run without its contract")
d = variant("premium", edit_run=lambda r: r.__setitem__("book_contract", copy.deepcopy(book_contract)))
refused(d, "perp_premium", "carries book_contract but does not record l2_book", "premium run with a book contract")
d = variant("book", edit_run=lambda r: r.__setitem__("premium_contract", copy.deepcopy(premium_contract)))
refused(d, "l2_book", "carries premium_contract but does not record perp_premium", "book run with a premium contract")
d = variant("mixed", edit_run=lambda r: r.pop("book_contract"))
for kind in ("l2_book", "perp_premium", "open_interest"):
    refused(d, kind, "records l2_book but carries no book_contract", f"mixed run without a book contract read as {kind}")
d = variant("mixed", edit_run=lambda r: r.__setitem__("kinds", r["kinds"] + ["funding"]))
for kind in ("l2_book", "perp_premium", "open_interest"):
    refused(d, kind, "unknown kind", f"mixed run with an unknown kind read as {kind}")
d = variant("mixed", edit_header=lambda h: h.pop("premium_contract"))
for kind in ("l2_book", "perp_premium", "open_interest"):
    refused(d, kind, "header records perp_premium but carries no premium_contract", f"mixed header without a premium contract read as {kind}")
d = variant("mixed", edit_header=lambda h: h.__setitem__("kinds", ["l2_book", "perp_premium"]))
refused(d, "l2_book", "lists different kinds than the run", "mixed header whose kinds differ from the run")
shutil.rmtree(work)
PY

echo "PASS: premium replay harness (sealed quote/oracle payloads and decisions match the independent recording replay; premium-only, book-only and mixed v3 runs import by their typed contracts and kind/contract mismatches are refused; tampered, truncated and contradicted recordings refused)"
