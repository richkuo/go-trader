# regime_adaptive entry filters (issue 2017)

Offline study. It asks whether an existing entry filter leaves `regime_adaptive` with a positive return after fees and funding.

The historical audit frame is spot-registry long entries on BTC, ETH and SOL at 1h and 4h, on the pinned Binance US candles, with Hyperliquid fees and funding. The perps frame is a research context at 30m and 1h for long, short and both. It is not a deployment record. A perps verdict needs native Hyperliquid candles over the frozen judgment windows and a lot-aware execution spec. The pinned venue files are the recent tail, and the scored book is the cash cache path.

No command sends an order. The study does not change a live config, a paper config, a registry default, the quarantine roster, or `edge_status`.

## Commands

Run these from the repository root, in order. `lock` must come before `select`. `select` writes the shortlist before `judge` scores a judgment window.

```
uv run --no-sync python backtest/candidates/regime_adaptive_2017/run_study.py verify-inputs
uv run --no-sync python backtest/candidates/regime_adaptive_2017/run_study.py lock
uv run --no-sync python backtest/candidates/regime_adaptive_2017/run_study.py coverage
uv run --no-sync python backtest/candidates/regime_adaptive_2017/run_study.py select --cache /tmp/ra2017.db --work /tmp/ra2017
uv run --no-sync python backtest/candidates/regime_adaptive_2017/run_study.py judge --cache /tmp/ra2017.db --work /tmp/ra2017
uv run --no-sync python backtest/candidates/regime_adaptive_2017/run_study.py render
```

Scoring reads `backtest/candidates/tema_cross_2016/data` by hash, loads a new cache, and blocks network access. The cache and checkpoints stay outside the repository.
