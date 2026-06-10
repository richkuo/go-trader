"""Tests for the backtest-vs-live parity diff tool (#906 D7.4).

The tool's job is to detect strategies whose bar-N decision depends on the
frame they were computed in (full-frame vectorized vs trailing live window).
These tests prove both directions: a window-invariant strategy diffs clean,
and a deliberately frame-dependent strategy is caught.
"""

import os
import sys

import numpy as np
import pandas as pd
import pytest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from parity_diff import (
    LIVE_MIN_CANDLES,
    compute_parity_frame,
    summarize,
)
from registry_loader import load_registry


def _ohlcv(n: int = 300, seed: int = 7) -> pd.DataFrame:
    rng = np.random.default_rng(seed)
    drift = np.linspace(0, 12, n)
    wave = 4.0 * np.sin(np.linspace(0, 14, n))
    noise = rng.normal(0, 0.4, n)
    close = 100.0 + drift + wave + noise
    df = pd.DataFrame({
        "open": close + rng.normal(0, 0.1, n),
        "high": close + np.abs(rng.normal(0, 0.5, n)) + 0.2,
        "low": close - np.abs(rng.normal(0, 0.5, n)) - 0.2,
        "close": close,
        "volume": rng.uniform(900, 1100, n),
    }, index=pd.date_range("2024-01-01", periods=n, freq="1h"))
    return df


def test_window_invariant_strategy_diffs_clean():
    """sma_crossover at bar N only needs the trailing slow-period bars, so
    a full window evaluation must equal the full-frame vectorized value on
    every compared bar."""
    df = _ohlcv(260)
    frame = compute_parity_frame(
        df, "sma_crossover",
        params={"fast_period": 10, "slow_period": 30},
        window=120,
    )
    result = summarize(frame)
    assert result["bars_compared"] > 100
    assert result["clean"], (
        f"window-invariant strategy should not diff: "
        f"{frame[~frame['match']].head()}"
    )


def test_frame_dependent_strategy_is_caught():
    """A strategy keyed on the FULL frame's mean (classic silent-parity
    bug: full-series normalization) must produce mismatches — the live
    window's mean differs from the backtest frame's mean."""
    reg = load_registry("spot")

    def full_frame_mean_strategy(df: pd.DataFrame) -> pd.DataFrame:
        out = df.copy()
        out["signal"] = (out["close"] > out["close"].mean()).astype(int)
        return out

    name = "_parity_diff_test_frame_dependent"
    reg.STRATEGY_REGISTRY[name] = {
        "fn": full_frame_mean_strategy,
        "description": "test-only frame-dependent strategy",
        "default_params": {},
    }
    try:
        df = _ohlcv(260)
        frame = compute_parity_frame(df, name, window=60)
        result = summarize(frame)
        assert result["mismatches"] > 0, (
            "frame-dependent strategy must be detected by the parity diff"
        )
        assert "first_mismatch" in result
    finally:
        del reg.STRATEGY_REGISTRY[name]


def test_regime_labels_diff_clean_per_bar():
    """latest_regime on the trailing window must match compute_regime's
    full-frame label on every bar — the per-bar generalization of the
    last-bar parity test in test_backtester_regime.py."""
    df = _ohlcv(220)
    frame = compute_parity_frame(
        df, "sma_crossover",
        params={"fast_period": 10, "slow_period": 30},
        window=120,
        regime_enabled=True,
    )
    assert "bt_regime" in frame.columns and "live_regime" in frame.columns
    regime_mismatch = frame[frame["bt_regime"] != frame["live_regime"]]
    assert regime_mismatch.empty, regime_mismatch.head()


def test_expanding_window_mode():
    """window=None replays live with an ever-growing frame from bar
    LIVE_MIN_CANDLES on; sma_crossover converges once the slow period is
    seeded, so only the comparison start moves."""
    df = _ohlcv(120)
    frame = compute_parity_frame(
        df, "sma_crossover",
        params={"fast_period": 5, "slow_period": 15},
        window=None,
    )
    assert len(frame) == len(df) - (LIVE_MIN_CANDLES - 1)
    assert summarize(frame)["clean"]


def test_stride_thins_comparison():
    df = _ohlcv(200)
    full = compute_parity_frame(
        df, "sma_crossover", params={"fast_period": 5, "slow_period": 15},
        window=60,
    )
    thinned = compute_parity_frame(
        df, "sma_crossover", params={"fast_period": 5, "slow_period": 15},
        window=60, stride=5,
    )
    assert len(thinned) == (len(full) + 4) // 5


def test_window_below_live_minimum_rejected():
    df = _ohlcv(100)
    with pytest.raises(ValueError, match="window must be >="):
        compute_parity_frame(df, "sma_crossover", window=10)
