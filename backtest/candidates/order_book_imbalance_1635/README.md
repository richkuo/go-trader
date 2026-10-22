# order_book_imbalance (issue 1635, Delivery 2)

Research-only, production-refused candidate. Delivery 1 (PR 1837) supplies the typed `l2_book` owner, recorder, importer, sealed transport and accessor. This directory holds the Delivery 2 study.

Status at this commit: specification frozen (`candidate_spec.json`). The evaluator, refusals, event simulator and study driver come after the recording. No recording made under this specification existed at the freeze. The earlier 6-hour recording was used only to measure venue cadence (`cadence_evidence` in the spec) and is never scored.

## Recording plan

| Item | Value |
|---|---|
| Coins | BTC, ETH |
| Recorder kinds | `open_interest` (raw `activeAssetCtx` frames carry `markPx`), `l2_book` |
| Duration | 840 hours (35 UTC days); at least 30 eligible complete UTC days are needed to score |
| Segments | 1 hour |
| 1m candles | `run_study.py capture-candles` every 24 hours (72-hour requests; the venue keeps about 3.5 days) |
| Venue metadata | `run_study.py capture-venue` at the start and end |
| Funding | fetched after the recording (history is available afterwards) |
| Storage | about 73 MB/day raw for the book stream; the recording stays outside git |

## Commands for a Linux host

Requirements: `git`, Go 1.26.2 or later (the recorder is pure Go, so `CGO_ENABLED=0` works), `python3` 3.8 or later (the capture script uses only the standard library), outbound HTTPS and WSS to `api.hyperliquid.xyz`, and about 3 GB of free disk. The host opens no state database, needs no credentials and sends no order.

The recorder must be built from the commit whose importer will read the recording. If PR 1835 (recorder schema rules) merges before the recording starts, use the rebased commit of this branch.

```bash
# 1. Checkout (the branch must be reachable from the host: push it, or copy a git bundle)
git clone https://github.com/richkuo/go-trader.git obi1635 && cd obi1635
git fetch origin cc/issue-1635-delivery-2-order-book-imbalance-study
git checkout <FREEZE_SHA>

# 2. Build the recorder
CGO_ENABLED=0 go -C scheduler build -o "$PWD/go-trader-recorder" .
./go-trader-recorder version --json

# 3. Output layout (the recording directory must not exist yet or must be empty)
OUT="$HOME/obi1635-run"
mkdir -p "$OUT/logs" "$OUT/candles" "$OUT/venue"
git rev-parse HEAD > "$OUT/logs/source_commit.txt"

# 4. Venue metadata at the start
python3 backtest/candidates/order_book_imbalance_1635/run_study.py capture-venue --out-dir "$OUT/venue" --label start

# 5. Recorder (840 hours); keep the PID to stop it only by that PID
nohup ./go-trader-recorder record-observations --coins BTC,ETH --kinds open_interest,l2_book \
    --segment 1h --duration 840h --out-dir "$OUT/recording" > "$OUT/logs/recorder.log" 2>&1 &
echo $! > "$OUT/logs/recorder.pid"

# 6. Candle capture loop: captures now, then every 24 hours, once more after the run closes,
#    then pins venue metadata again
nohup bash backtest/candidates/order_book_imbalance_1635/capture_loop.sh "$OUT" > "$OUT/logs/capture.log" 2>&1 &
echo $! > "$OUT/logs/capture.pid"
```

Progress checks:

```bash
tail -n 5 "$OUT/logs/recorder.log"          # per-minute book readiness lines and segment rotations
tail -n 5 "$OUT/logs/capture.log"
wc -l "$OUT/candles/captures.jsonl"           # two lines (BTC, ETH) per capture
ls "$OUT/recording" | tail -n 3
```

To stop early, send SIGTERM to the PID in `recorder.pid` only. The recorder seals the open segment and closes the run manifest. The capture loop then sees the closed run, takes its last capture and exits.

## Files to copy back

Copy the whole `$OUT` directory, for example `rsync -a host:obi1635-run/ <local dir>/`:

- `recording/run.manifest.json`, every `recording/segment-*.jsonl` and every `recording/segment-*.jsonl.manifest.json`
- `candles/captures.jsonl` and every `candles/candles_1m_<coin>_<ms>.json.gz`
- `venue/captures.jsonl` and both `venue/venue_meta_{start,end}_<ms>.json.gz`
- `logs/recorder.log`, `logs/capture.log`, `logs/source_commit.txt`

The run is usable as evidence only if `run.manifest.json` has `"closed": true` and `"dropped_total": 0`, and `observation_replay.py import-books` accepts it.
