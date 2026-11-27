---
paths:
  - "backtest/**"
  - "shared_strategies/**"
  - "shared_tools/**"
  - "docs/backtesting-registry.md"
---

## Backtest

- `funding_fetcher.py`:`merge_asof` backward,DISJOINT `funding_coverage`;in-coverage gaps refill by strict download only,`funding_venue_gaps`=venue-absent(charge 0),failed refill writes nothing;`regime.py` ATR `simple`
- `ledger_compare.py`:sim tier close needs winning `resting_fill`;same costs
- Harness map `docs/backtesting-registry.md`:add/deprecate PR updates row.
- SL-vs-TP races default `ohlc_walk`
- HTF series keyed by candle OPEN time MUST `.shift(1)` BEFORE `reindex` ffill.
