#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
OUT=$(mktemp -d "${TMPDIR:-/tmp}/premium-harness.XXXXXX")
trap 'rm -rf "$OUT"' EXIT

GO_TRADER_PREMIUM_HARNESS_OUT="$OUT" go -C "$ROOT/scheduler" test -run '^TestPremiumObservationPipeline$' -count=1 .

for f in harness.json payload_btc_1.json payload_btc_e.json payload_eth_e.json payload_btc_2.json recording/run.manifest.json; do
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

echo "PASS: premium replay harness (sealed quote/oracle payloads and decisions match the independent recording replay; tampered, truncated and contradicted recordings refused)"
