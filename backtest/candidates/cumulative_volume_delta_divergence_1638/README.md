# Cumulative volume delta divergence (#1638)

Research-only. `cumulative_volume_delta_divergence` is registered `edge_status="no_edge"` and `production_refused=True`. It is hidden from normal discovery. Every production path refuses it in every mode, including explicit paper mode and `allow_no_edge: true`: config load, probe, read-only inspection and reload (`scheduler/production_refusal.go`), the add-strategy command, and the Python check-script admission for solo and batch checks (`shared_tools/strategy_composition.py`). Promotion is a separate reviewed decision. `REPORT.md` holds the verdict.

## What is frozen

- `candidate_spec.json`: the signal and timing contract, the trade contract, units, side meaning, parameter seeds, bounds and grid, the exit and stop owners, windows, the selection rule, the price-only baseline and the verdict criteria. It was committed before any study window was scored. It was amended once before scoring (trade identity and recording run; see `data_plan.recording_restarts`).
- `study_manifest.json` (`offline_candle_manifest/v3`): 5m candles, hourly funding, venue meta, an l2 snapshot, quoted fee, minimum, lot and WebSocket documentation, one recorded trade-flow series per coin, the venue 1m candles used to measure completeness, and the side evidence files. Every input is pinned by sha256.
- `data/recording/`: the raw recording run. It holds the run manifest, per-segment manifests and gzip copies of every JSONL segment. The importer verifies each segment's sha256 over the decompressed bytes and replays every raw frame.
- `data/side_evidence/`: `side_evidence.py` output. Each file samples accepted trades from one recorded segment and keeps, for both users of each trade, the public `userFillsByTime` request, the response sha256 and the matching fill's side, `crossed` flag, px, sz, time and hash.

## Source, identity and side

- Live source: Hyperliquid WebSocket `{"type": "trades", "coin": <coin>}`. Each element carries `coin`, `side`, `px`, `sz` (decimal strings), `time` (venue block milliseconds), `hash`, `tid` and `users` `[buyer, seller]`. The first frame after a subscription replays recent trades.
- Identity: the venue documents `tid` as a 50-bit hash of the buyer and seller order ids and prescribes `(block_time, coin, tid)` for a globally unique trade id. The feed scopes that identity by venue host and perps namespace.
- Side: the documentation does not state the aggressor meaning of `side`. The study measures it: for sampled recorded trades, the user whose fill has `crossed=true` (the taker) is checked against `side` (B = buyer, A = seller). No order is submitted.
- Completeness: a healthy socket does not prove that every venue trade arrived. The decision rule requires the exact sum of a bar's minute totals to equal that bar's candle volume. The study also compares every complete minute's total and tally with the venue 1m candle `v` and `n`.
- Historical archive: Hyperliquid publishes node trade archives only in a requester-pays bucket. AWS access was not available where this study was built, so no archive window is used and unavailable history is never replaced with current values.

## Reproduce

All commands run from the repository root. The recorder subscribes only to public market data. It opens no state database, takes no trading lock and sends no order.

```
go -C scheduler build -o ../go-trader .
./go-trader record-observations --coins BTC,ETH --kinds trade_flow --out-dir <empty dir> --duration 13h50m --segment 1h
uv run --no-sync python backtest/observation_replay.py import-trades --run-dir <dir>
uv run --no-sync python backtest/candidates/cumulative_volume_delta_divergence_1638/side_evidence.py --segment <dir>/segment-00001.jsonl --out <file>
uv run --no-sync python backtest/candidates/cumulative_volume_delta_divergence_1638/run_study.py prepare --run-dir <dir>
uv run --no-sync python backtest/candidates/cumulative_volume_delta_divergence_1638/run_study.py pin-side-evidence <file> [<file> ...]
uv run --no-sync python backtest/offline_manifest.py acquire --manifest backtest/candidates/cumulative_volume_delta_divergence_1638/study_manifest.json --write-hashes
uv run --no-sync python backtest/candidates/cumulative_volume_delta_divergence_1638/run_study.py acquire-completeness
uv run --no-sync python backtest/candidates/cumulative_volume_delta_divergence_1638/run_study.py finalize
uv run --no-sync python backtest/offline_manifest.py verify --manifest backtest/candidates/cumulative_volume_delta_divergence_1638/study_manifest.json
uv run --no-sync python backtest/candidates/cumulative_volume_delta_divergence_1638/run_study.py run
uv run --no-sync python backtest/candidates/cumulative_volume_delta_divergence_1638/run_study.py run --render-only
```

`run` needs no network. It re-imports the committed raw recording, refuses when the rebuilt series differs from the committed series files, and recomputes the side-evidence and completeness summaries from the pinned files. `prepare` never overwrites a frozen recording and `pin-side-evidence` never overwrites an evidence file.

## Result

The verdict is **INCONCLUSIVE** (see `REPORT.md`). The registry records `edge_source="study_inconclusive"`, and the candidate stays production-refused.

- Data quality: the 13h50m recording (run started 2026-10-13 02:25:55Z) has 14 hashed segments, 0 dropped records and 0 malformed frames. The independent Python replay reproduced every per-trade outcome and all 1682 recorder events. Every complete minute that has a venue 1m candle (823 per coin) matches that candle's volume and trade count exactly.
- Side meaning: in 239 sampled recorded trades across three segments, the taker identified from public `userFillsByTime` `crossed` flags agreed with `side` every time, with no fill-content mismatch.
- Coverage: the venue closed the socket about every three hours. The recorder console logged `websocket: close 1000 (normal): Expired` four times, and the recording dates the four disconnects at 05:18, 08:19, 11:12 and 14:07 UTC (a fifth is the planned shutdown at 16:15). Each close made one minute invalid. Under the frozen complete-session rule, each one removes the rest of its 4h session. Only 20 of 96 train bars and 26 of 48 test bars per coin had usable flow, which is under the frozen 80% floor.
- Economics: no train grid point reached 6 positions, so the seed parameters were scored. On the held-out session the candidate made no entry and the price-only baseline made one short (net $-4.09 at cost x1). With fewer than 30 held-out positions, the frozen criteria give no economic verdict.
- A useful verdict needs a recorder on an always-on host for weeks, or the measured requester-pays archive. Session coverage also needs a reconnect that keeps a session usable, for example a short overlap subscription before the venue expiry, under a new frozen specification. This issue does not change the frozen rule after scoring.

## Limits

- The recorded span is about 14 hours on one host. That is far too short for an economic claim; the frozen sample rule (30 held-out positions) makes an inconclusive verdict the expected outcome. The study shows the pipeline end to end on real venue data.
- `execution_spec` models fees, spread, slippage, lot floors and the venue minimum. It does not model leverage, margin or liquidation. Every arm runs at full capital and 1x, where liquidation cannot occur.
- `time_stop` reads the simulator's held-bar count, which no live close context supplies, so the close comparison is approximate (incomplete close parity).
- The decision deadline is the bar close and the fill is the next bar open, the same timing as every candle strategy; every trade interval a decision uses ends at least one bar before its deadline. Live latency between the deadline and an order is not modeled.
- Every M1 window in `eval_windows.py` predates the recording and has no trade flow, so none is scored.
