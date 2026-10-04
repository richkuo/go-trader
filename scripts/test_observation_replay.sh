#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
OUT=$(mktemp -d "${TMPDIR:-/tmp}/obs-harness.XXXXXX")
trap 'rm -rf "$OUT"' EXIT

GO_TRADER_OBS_HARNESS_OUT="$OUT" go -C "$ROOT/scheduler" test -run '^TestOpenInterestObservationPipeline$' -count=1 .

for f in harness.json payload_1.json payload_2.json recording/run.manifest.json; do
    if [[ ! -s "$OUT/$f" ]]; then
        echo "FAIL: the Go harness did not write $f" >&2
        exit 1
    fi
done

cd "$ROOT"
uv run --no-sync python backtest/observation_replay.py import --run-dir "$OUT/recording" > "$OUT/import.json"
uv run --no-sync python backtest/observation_replay.py harness --dir "$OUT"

cp "$OUT/recording/segment-00001.jsonl" "$OUT/tampered.jsonl"
printf '{"k":"obs"}\n' >> "$OUT/recording/segment-00001.jsonl"
if uv run --no-sync python backtest/observation_replay.py import --run-dir "$OUT/recording" > /dev/null 2> "$OUT/tamper.err"; then
    echo "FAIL: the importer accepted a segment whose bytes no longer match its manifest hash" >&2
    exit 1
fi
grep -q "sha256" "$OUT/tamper.err" || { echo "FAIL: tamper refusal did not name the hash mismatch" >&2; cat "$OUT/tamper.err" >&2; exit 1; }
cp "$OUT/tampered.jsonl" "$OUT/recording/segment-00001.jsonl"

head -c 200 "$OUT/tampered.jsonl" > "$OUT/recording/segment-00001.jsonl"
if uv run --no-sync python backtest/observation_replay.py import --run-dir "$OUT/recording" > /dev/null 2>&1; then
    echo "FAIL: the importer accepted a truncated segment" >&2
    exit 1
fi

echo "PASS: observation replay harness (sealed payload decisions match recording replay; tampered and truncated recordings refused)"
