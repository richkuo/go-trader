# Batched Hyperliquid check benchmark (#1442)

This file is the measurement the batched check lane rests on. No speedup may
be claimed without it, and any target stated later has to be arithmetically
reachable for the group size it names.

## What was measured

Two arms, same host, same configuration, same candles:

- **unbatched** — N sequential `check_hyperliquid.py <strategy> BTC 1h`
  invocations, the shape the dispatch loop produces today.
- **batched** — one `check_hyperliquid.py --batch-check` invocation carrying N
  slots on stdin.

Both arms are network-free. `GO_TRADER_HL_OHLCV_FIXTURE` pins the candles
(`hl_batch_candles.json`, 200 hourly bars) and `--mark-price` is always
supplied, so `adapter.get_spot_price` is never reached. The `#839` on-disk
OHLCV cache is disabled for both arms, so the comparison measures compute
rather than cache luck. Funding-aware strategies are excluded from the
workload: they reach the network regardless of the candle fixture.

Workload strategies cycle through `breakout`, `momentum_pro`,
`mean_reversion_pro`, `rsi_bb_combo`.

Reproduce with:

```bash
uv run --no-sync python scripts/bench_hl_batch.py \
    --sizes 2,5,10,20 --reps 10 --warmups 2 --json docs/benchmarks/hl_batch_raw.json
```

## Host

| Field | Value |
|---|---|
| Platform | macOS-26.6.2-arm64-arm-64bit |
| Processor | arm |
| CPU count | 10 |
| Python | 3.12.9 |

**This is a developer workstation, not the 2 GB / 2 CPU deployment class.**
The deployment-class numbers are not in this artifact. What the run does
establish is the SHAPE of the cost — a fixed interpreter-plus-import cost per
process start, and a per-slot marginal cost far below it — and that shape is
what the batch removes. Re-run the same command on the deployment host before
quoting a deployment-class figure.

## Results (10 repetitions after 2 warmups)

| N | arm | wall median (s) | wall p95 (s) | processor time median (s) | process starts |
|---|---|---|---|---|---|
| 2 | unbatched | 1.0649 | 1.1375 | 1.0530 | 2 |
| 2 | batched | 0.5247 | 0.6214 | 0.5169 | 1 |
| 5 | unbatched | 2.8055 | 3.0265 | 2.7261 | 5 |
| 5 | batched | 0.5458 | 0.6986 | 0.5383 | 1 |
| 10 | unbatched | 5.0720 | 5.2812 | 5.0158 | 10 |
| 10 | batched | 0.7066 | 1.0977 | 0.6426 | 1 |
| 20 | unbatched | 10.3598 | 11.0852 | 10.2193 | 20 |
| 20 | batched | 0.5169 | 0.5465 | 0.5095 | 1 |

Raw per-repetition records: `hl_batch_raw.json`.

## Reading the numbers

- **The per-strategy cost is dominated by process start plus imports.** The
  unbatched arm scales almost exactly linearly at about 0.52 s per strategy;
  the batched arm stays near 0.52 s whether it carries 2 slots or 20.
- **The per-slot marginal cost is below this run's noise floor.** Across N=2 to
  N=20 the batched median moved from 0.5247 s to 0.5169 s — a difference
  smaller than the spread between repetitions, so the honest statement is that
  the marginal cost per additional slot is under about 10 ms on this host, not
  that it is zero. A deployment-host run is needed for a tighter bound.
- **This measures the batch alone, not the batch plus #1441.** Issue #1441's
  persistent worker pool removes the same interpreter-and-import cost by a
  different route. The combined figure is measured only after both land, using
  #1441's own committed benchmark.
- **Peak child RSS shows no regression and cannot show a reduction here.**
  `ru_maxrss` for `RUSAGE_CHILDREN` is the maximum over all terminated
  children, and both arms run their children sequentially, so both report one
  interpreter's peak (about 108 MB in every row). The batch does reduce the
  NUMBER of interpreter starts per cycle from N to 1 for a group, which is the
  input to #1441's pool-size decision; it does not lower the peak of a
  sequential loop, and this artifact does not claim it does.

## What this does not establish

- Any figure for the 2 GB / 2 CPU deployment class.
- Any figure with the funding-aware strategies, the HTF filter, or a live
  regime computation in the workload.
- Anything about end-to-end cycle time, which is dominated by the exchange
  round trips this benchmark deliberately removes.
