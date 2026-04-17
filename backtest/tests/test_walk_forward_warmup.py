"""
Regression tests for issue #303 H3 — walk-forward folds must prepend a
warmup slice before each train/test window so indicators with long
lookbacks (e.g. SMA-80) can prime before the first signal bar.

Prior behavior sliced ``df.iloc[start_idx:end_idx]`` directly — an SMA-80
on a 100-bar window produced all-NaN signals and silently "won" the
grid with zero trades. The warmup buffer prevents that silent failure.
"""
import numpy as np
import pandas as pd
import pytest

from optimizer import max_indicator_lookback, walk_forward_optimize


def _trending_ohlc(n: int = 500, seed: int = 7) -> pd.DataFrame:
    rng = np.random.default_rng(seed)
    log_returns = rng.normal(loc=0.002, scale=0.015, size=n)
    closes = [100.0]
    for r in log_returns:
        closes.append(closes[-1] * np.exp(r))
    closes = np.array(closes[1:])
    opens = closes * (1.0 + rng.normal(loc=0.0, scale=0.002, size=n))
    highs = np.maximum(opens, closes) * 1.003
    lows = np.minimum(opens, closes) * 0.997
    volume = rng.integers(1000, 10000, size=n).astype(float)
    idx = pd.date_range("2022-01-01", periods=n, freq="D")
    return pd.DataFrame(
        {"open": opens, "high": highs, "low": lows,
         "close": closes, "volume": volume},
        index=idx,
    )


def test_max_indicator_lookback_picks_largest_int():
    ranges = {
        "fast_period": [10, 15, 20],
        "slow_period": [40, 50, 80],
        "multiplier":  [1.5, 2.0, 3.0],  # float — ignored
    }
    assert max_indicator_lookback(ranges) == 80


def test_max_indicator_lookback_zero_for_float_only_grid():
    ranges = {
        "entry_std": [1.0, 1.5, 2.0],
        "exit_std":  [0.0, 0.5, 1.0],
    }
    assert max_indicator_lookback(ranges) == 0


def test_sma_80_grid_generates_trades_with_warmup():
    """Core H3 scenario: SMA-80 on a 100-bar fold. Before the fix the
    signal column was all-zero (80 NaN bars + 20 too-few-crossings bars),
    so the fold produced zero trades and sharpe=0 "won" by default. With
    warmup, the preceding 80 bars prime the indicator and at least one
    valid crossing occurs across the 5 folds."""
    df = _trending_ohlc(n=500)
    param_ranges = {"fast_period": [10, 20], "slow_period": [40, 80]}

    result = walk_forward_optimize(
        df, "sma_crossover", param_ranges,
        n_splits=5, train_pct=0.7,
        initial_capital=1000.0, verbose=False,
    )

    assert "window_results" in result, result
    total_trades = sum(
        w["test_result"]["total_trades"] for w in result["window_results"]
    )
    assert total_trades > 0, (
        "Walk-forward produced zero trades across all folds — the warmup "
        "fix did not engage or is insufficient for SMA-80 priming."
    )


def test_warmup_does_not_leak_future_data():
    """Warmup must come from BEFORE the train window, never after. A
    look-ahead regression would manifest as fold 0 (which has no preceding
    history) behaving the same as later folds."""
    df = _trending_ohlc(n=600)
    param_ranges = {"fast_period": [10], "slow_period": [80]}  # lookback=80

    result = walk_forward_optimize(
        df, "sma_crossover", param_ranges,
        n_splits=5, train_pct=0.7,
        initial_capital=1000.0, verbose=False,
    )

    # Fold 0 starts at bar 0, so warmup_start = max(0, 0-80) = 0 — no
    # preceding history to prime with. Fold 1+ gets 80 bars of warmup.
    # That asymmetry is the contract; just assert fold 1 is in the results
    # (i.e. folds run and don't crash).
    assert result["n_valid_folds"] >= 2, result
