# ultimate_oscillator_1664

Research-only study for issue #1664: Donchian price breakouts confirmed by the
Ultimate Oscillator on the same closed bar, for Hyperliquid perpetuals. The
result is in `REPORT.md` (rendered from `results.json`). The strategy stays
registered with `edge_status="no_edge"` whatever the verdict; paper evaluation
needs an explicit `--mode=paper`, and live use needs the operator's
`allow_no_edge: true`. Nothing here writes a config, a default or a service.

## Files

- `candidate_seed.json`: the runnable candidate (seed parameters, direction,
  close owner and stop owner). Its parameters must equal the registry defaults;
  the driver refuses to run otherwise.
- `study_spec.json`: signal contract, event truth table, parameter grid,
  windows, close and stop ownership, selection, sample, coverage and verdict
  rules. It was frozen before any held-out result was read.
- `data/`: frozen inputs and `frozen_inputs.json` with the sha256 of every
  file. Binance US spot candles (1h and 4h, BTC/ETH/SOL) stand in for
  Hyperliquid perpetual prices; Hyperliquid funding rows, coverage intervals
  and confirmed venue-absent hours come from `shared_tools/funding_fetcher.py`.
- `discovery_{spot,futures}_{before,after}.json`: normal `--list-json`
  output before and after registration (byte-identical).
- `run_study.py`: the driver. `results.json` and `REPORT.md`: its output.

## Commands

```bash
# Reproduce the study from the frozen inputs (no network; private SQLite cache)
uv run --no-sync python backtest/candidates/ultimate_oscillator_1664/run_study.py

# Add frozen-frame decision parity (solo and explicit-paper batch, full prefix and 200 bars)
uv run --no-sync python backtest/candidates/ultimate_oscillator_1664/run_study.py --parity

# Re-render REPORT.md from the committed results.json
uv run --no-sync python backtest/candidates/ultimate_oscillator_1664/run_study.py --render-only

# Refresh the frozen inputs from a scratch copy of the OHLCV cache (network)
uv run --no-sync python backtest/candidates/ultimate_oscillator_1664/run_study.py --freeze /path/to/scratch_cache.db
```

`--freeze` extends the scratch cache's candles to 2026-10-01, downloads
Hyperliquid funding into a separate fresh database next to it, and rewrites
`data/`. A normal run checks every pinned hash, rebuilds a private cache from
`data/`, and disables both the candle fetcher and the funding adapter, so a
missing row becomes a recorded coverage gap and never a silent download.

## Design notes

- Every leg runs through the unchanged M1 `run_leg` (`backtest/eval_windows.py`)
  with `funding_mode="charge"`, the real registry and the real `Backtester`.
  The driver only observes each `Backtester.run` result to read positions.
- The paired control is `donchian_breakout` at the candidate's breakout
  lookback, with its entries zeroed wherever the candidate's `uo_valid` column
  is false. It never reads the oscillator threshold or the confirmed signal.
  The raw unmasked control is reported beside it.
- The M1 2023 window is replaced by `2023H2` (2023-07-01 to 2024-01-01):
  Hyperliquid funding history starts on 2023-05-12 and prints only every
  8 hours until 2023-06-07, so earlier held hours cannot be priced.
- The ordinary M1 path starts each frame at the window start, so the first
  required-history bars of each window hold with `insufficient_history`.
- No manifest (venue-candle) leg runs: the only frozen Hyperliquid venue
  candles (#1649) span this study's selection window, so they cannot serve as
  held-out evidence.
