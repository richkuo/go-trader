#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
IN=$(mktemp -d "${TMPDIR:-/tmp}/macro-harness-in.XXXXXX")
OUT=$(mktemp -d "${TMPDIR:-/tmp}/macro-harness-out.XXXXXX")
trap 'rm -rf "$IN" "$OUT"' EXIT

HOUR=3600000
NOW_MS=$(( $(date -u +%s) * 1000 ))
T0=$(( NOW_MS / HOUR * HOUR - 102 * HOUR ))

cd "$ROOT"
uv run --no-sync python backtest/macro_event_replay.py fixture --out "$IN" --t0 "$T0" > "$IN/fixture.json"

GO_TRADER_MACRO_HARNESS_T0="$T0" GO_TRADER_MACRO_HARNESS_IN="$IN" GO_TRADER_MACRO_HARNESS_OUT="$OUT" \
    go -C "$ROOT/scheduler" test -run '^TestMacroEventFeedPipeline$' -count=1 .

for f in harness.json payload_ws.json payload_shared.json seal.json; do
    if [[ ! -s "$OUT/$f" ]]; then
        echo "FAIL: the Go harness did not write $f" >&2
        exit 1
    fi
done

uv run --no-sync python backtest/macro_event_replay.py harness --dir "$OUT"

printf ' ' >> "$IN/quotes.json"
if GO_TRADER_MACRO_HARNESS_T0="$T0" GO_TRADER_MACRO_HARNESS_IN="$IN" \
    go -C "$ROOT/scheduler" test -run '^TestMacroEventFeedPipeline$' -count=1 . > "$OUT/tamper.log" 2>&1; then
    echo "FAIL: the Go owner served a quote artifact whose bytes no longer match the pinned sha256" >&2
    exit 1
fi
grep -q "sha256" "$OUT/tamper.log" || { echo "FAIL: tamper refusal did not name the digest mismatch" >&2; tail -20 "$OUT/tamper.log" >&2; exit 1; }

echo "PASS: macro-event replay harness (Python-imported artifacts sealed by the Go owner give equal websocket, shared and offline decisions; altered artifact bytes refused)"
