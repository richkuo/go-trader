# Composite regime calibration (#795)

Run before committing default `thresholds` in `RegimeWindowSpec` / `shared_tools/regime.py`.

## Assets and horizons

- Symbols: BTC, ETH, SOL
- Timeframes: 30m, 1h, 1d
- Lookback: 1 year

## Analyses

1. State frequency per asset/timeframe (sanity-check rare labels like `ranging_directional`)
2. Forward 1-day return distribution per label (predictive power)
3. Confusion matrix vs ADX baseline on the same windows

## Outputs

Document chosen defaults for `return_pct`, `range_pct`, and `adx` in the issue/PR when calibration completes.
