#!/usr/bin/env bash
set -uo pipefail

OUT=${1:?usage: capture_loop.sh <study output dir>}
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
PY=${PYTHON:-python3}
EVERY=${CAPTURE_EVERY_SECONDS:-86400}
mkdir -p "$OUT/candles" "$OUT/venue"

closed() {
    [[ -f "$OUT/recording/run.manifest.json" ]] && "$PY" -c 'import json,sys; sys.exit(0 if json.load(open(sys.argv[1])).get("closed") else 1)' "$OUT/recording/run.manifest.json"
}

capture() {
    local tries=0
    until "$PY" "$HERE/run_study.py" capture-candles --out-dir "$OUT/candles"; do
        tries=$((tries + 1))
        echo "$(date -u +%FT%TZ) capture-candles failed (attempt $tries); retrying in 600 s" >&2
        if (( tries >= 12 )); then
            echo "$(date -u +%FT%TZ) capture-candles gave up after $tries attempts; the next scheduled capture still overlaps this window" >&2
            return 1
        fi
        sleep 600
    done
    echo "$(date -u +%FT%TZ) capture-candles ok"
}

while true; do
    capture
    if closed; then
        break
    fi
    slept=0
    while (( slept < EVERY )); do
        sleep 300
        slept=$((slept + 300))
        if closed; then
            break
        fi
    done
    if closed; then
        capture
        break
    fi
done
"$PY" "$HERE/run_study.py" capture-venue --out-dir "$OUT/venue" --label end
echo "$(date -u +%FT%TZ) capture loop finished"
