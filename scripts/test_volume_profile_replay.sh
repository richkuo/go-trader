#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
OUT=$(mktemp -d "${TMPDIR:-/tmp}/profile-harness.XXXXXX")
trap 'rm -rf "$OUT"' EXIT

GO_TRADER_PROFILE_HARNESS_OUT="$OUT" go -C "$ROOT/scheduler" test -run '^TestVolumeProfileObservationPipeline$' -count=1 .

for f in harness.json recording/run.manifest.json; do
    if [[ ! -s "$OUT/$f" ]]; then
        echo "FAIL: the Go harness did not write $f" >&2
        exit 1
    fi
done

cd "$ROOT"
uv run --no-sync python backtest/observation_replay.py import-profiles --run-dir "$OUT/recording" --out-dir "$OUT/series" > "$OUT/import.json"
uv run --no-sync python backtest/observation_replay.py harness-profiles --dir "$OUT"

SEG=$(ls "$OUT"/recording/segment-*.jsonl | head -1)
cp "$SEG" "$OUT/original.jsonl"
printf '{"k":"trades"}\n' >> "$SEG"
if uv run --no-sync python backtest/observation_replay.py import-profiles --run-dir "$OUT/recording" > /dev/null 2> "$OUT/tamper.err"; then
    echo "FAIL: the profile importer accepted a segment whose bytes no longer match its manifest hash" >&2
    exit 1
fi
grep -q "sha256" "$OUT/tamper.err" || { echo "FAIL: tamper refusal did not name the hash mismatch" >&2; cat "$OUT/tamper.err" >&2; exit 1; }

head -c 300 "$OUT/original.jsonl" > "$SEG"
if uv run --no-sync python backtest/observation_replay.py import-profiles --run-dir "$OUT/recording" > /dev/null 2>&1; then
    echo "FAIL: the profile importer accepted a truncated segment" >&2
    exit 1
fi
cp "$OUT/original.jsonl" "$SEG"

uv run --no-sync python - "$OUT" <<'PY'
import copy, hashlib, json, os, shutil, sys
sys.path.insert(0, "backtest")
sys.path.insert(0, "shared_tools")
import trade_flow_replay as tfr
import volume_profile_replay as vpr
out = sys.argv[1]
run_dir = os.path.join(out, "recording")
series = vpr.read_profile_series(os.path.join(out, "series", "BTC_volume_profile.json.gz"))
final = [s for s in series["sessions"] if s["status"] == "final"]
if len(final) != 1 or final[0]["artifact"]["volume"] != "4.32":
    raise SystemExit(f"FAIL: the rebuilt BTC series holds {len(final)} final sessions: {[s['status'] for s in series['sessions']]}")
work = os.path.join(out, "contradicted")
shutil.copytree(run_dir, work)
path = os.path.join(work, "run.manifest.json")
with open(path) as fh:
    run = json.load(fh)
done = False
for idx, seg in enumerate(run["segments"]):
    blob_path = os.path.join(work, seg["file"])
    with open(blob_path) as fh:
        lines = fh.read().split("\n")
    for i, line in enumerate(lines):
        if '"k":"trades"' in line and '\\"px\\":\\"104\\"' in line:
            rec = json.loads(line)
            rec["raw"] = rec["raw"].replace('"px":"104"', '"px":"104.5"', 1)
            lines[i] = json.dumps(rec, separators=(",", ":"), sort_keys=True)
            done = True
            break
    if done:
        body = "\n".join(lines).encode("utf-8")
        with open(blob_path, "wb") as fh:
            fh.write(body)
        seg["sha256"] = hashlib.sha256(body).hexdigest()
        with open(os.path.join(work, seg["file"] + ".manifest.json"), "w") as fh:
            json.dump(seg, fh)
        run["segments"][idx] = seg
        with open(path, "w") as fh:
            json.dump(run, fh)
        break
if not done:
    raise SystemExit("FAIL: the harness recording holds no BTC trade at price 104")
try:
    vpr.load_profile_recording(work)
except tfr.TradeRecordingError as exc:
    if "disagrees with the recorder" not in str(exc):
        raise SystemExit(f"FAIL: wrong refusal for a rehashed altered trade price: {exc}")
else:
    raise SystemExit("FAIL: the profile importer accepted a trade price that contradicts the recorder events")
open_run = os.path.join(out, "unclosed")
shutil.copytree(run_dir, open_run)
with open(os.path.join(open_run, "run.manifest.json")) as fh:
    run = json.load(fh)
run["closed"] = False
with open(os.path.join(open_run, "run.manifest.json"), "w") as fh:
    json.dump(run, fh)
loaded = vpr.load_profile_recording(open_run)
payload = vpr.payload_from_profile_series(loaded["series"]["BTC"], 900_000)
if payload["available"] or loaded["series"]["BTC"]["economic_evidence"]:
    raise SystemExit("FAIL: an unclosed recording supplied a usable volume profile")
PY

echo "PASS: volume-profile replay harness (sealed profile sessions, hand-calculated value areas and decisions match the independent recording replay; tampered, truncated, contradicted and unclosed recordings refused)"
