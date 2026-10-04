# Open-interest breakout confirmation (#1637)

Research-only. `open_interest_breakout` is registered `backtest_only=True`, is hidden from normal discovery, and every live check script refuses it. Promotion is a separate reviewed decision. `REPORT.md` holds the verdict.

## What is frozen

- `candidate_spec.json`: signal, comparisons, units, time basis, endpoint rule, warm-up, data-quality limits, parameter bounds and grid, exit and stop owners, windows, selection rule, baselines and verdict criteria. It was committed in b86a12ac before any study window was scored.
- `study_manifest.json` (`offline_candle_manifest/v2`): 5m candles, hourly funding, venue meta, an l2 snapshot, quoted fee and minimum documentation, and one recorded open-interest series per coin. Every input is pinned by sha256.
- `data/recording/`: the raw recording run. It holds the run manifest, per-segment manifests and gzip copies of every JSONL segment. The importer verifies each segment's sha256 over the decompressed bytes.

## Source and units

- Live source: Hyperliquid WebSocket `{"type": "activeAssetCtx", "coin": <coin>}`. `ctx.openInterest` is a decimal string with no event timestamp, so the only time basis is local receipt time. `event_ms` stays null and is never described as an exchange time.
- Units: base coin. On 2026-10-04 the captured public `metaAndAssetCtxs` response gave BTC `openInterest` 37114.07922 (5 decimals, the same as BTC `szDecimals` 5) at `markPx` 84772.0, about $3.1B notional. ETH and SOL behaved the same way (4 and 2 decimals).
- Historical archive: `s3://hyperliquid-archive/asset_ctxs/[date].csv.lz4` is requester-pays. Hyperliquid documents it as uploaded about monthly with possible gaps. AWS access was not available where this study was built, so the archive's cadence, units, timestamp field and coverage are unmeasured and no archive window is used. A forward recording is the only source.

## Reproduce

All commands run from the repository root. The recorder subscribes only to public market data. It opens no state database, takes no trading lock and sends no order.

```
go -C scheduler build -o ../go-trader .
./go-trader record-observations --coins BTC,ETH,SOL --out-dir <empty dir> --duration 14h --segment 1h
uv run --no-sync python backtest/observation_replay.py import --run-dir <dir>
uv run --no-sync python backtest/candidates/open_interest_breakout_1637/run_study.py prepare --run-dir <dir>
uv run --no-sync python backtest/offline_manifest.py acquire --manifest backtest/candidates/open_interest_breakout_1637/study_manifest.json --write-hashes
uv run --no-sync python backtest/candidates/open_interest_breakout_1637/run_study.py finalize
uv run --no-sync python backtest/offline_manifest.py verify --manifest backtest/candidates/open_interest_breakout_1637/study_manifest.json
uv run --no-sync python backtest/candidates/open_interest_breakout_1637/run_study.py run
uv run --no-sync python backtest/candidates/open_interest_breakout_1637/run_study.py run --render-only
```

`run` needs no network. It re-imports the committed raw recording and refuses when the rebuilt series differs from the committed series files. `prepare` never overwrites a frozen recording.

## Result

The verdict is **INCONCLUSIVE** (see `REPORT.md`). The recording run had nine sessions. It was continuous from 03:04 to 05:41 UTC. After that the stream stopped eight times, for 14 to 33 minutes each (about 3.1 hours in total), with connection resets. The host appears to have slept, but this is not verified. Every stopped stretch is in the data as a disconnect gap or a stale sample, so 0% of held-out bars had a valid open-interest window. The candidate and the coverage-matched Donchian baseline therefore made no held-out entries, while the unfiltered Donchian made 7 entries (net $-15.55 at cost x1). No train grid point reached 6 positions, so the seed parameters were scored. A useful verdict needs a recorder on an always-on host for weeks of 5m bars (or the archive, once measured), followed by a new frozen specification.

## Limits

- The recorded span is six hours. That is far too short for an economic claim, so the frozen criteria make the expected verdict inconclusive. The study demonstrates the pipeline end to end on real venue data.
- `execution_spec` models fees, spread, slippage, lot floors and the venue minimum. It does not model leverage, margin or liquidation. Every arm runs at full capital and 1x, where liquidation cannot occur.
- Receipt-time parity holds by construction: the live payload and the replay use the same receipt-stamped samples. An archive window, if one is added later, would use the archive's own timestamp and cannot prove receipt-time parity.
- Every M1 window in `eval_windows.py` predates the recording. Each is listed in `REPORT.md` with its coverage, and none is filled from another venue or from current snapshots.
