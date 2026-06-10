"""Tests for mean_reversion_pro.py — trend-filtered mean-reversion strategy."""

import numpy as np
import pandas as pd

from mean_reversion_pro import mean_reversion_pro_core


def make_ohlcv(closes, noise=0.5, volume=100.0):
    closes = np.asarray(closes, dtype=float)
    n = len(closes)
    return pd.DataFrame({
        "open": closes - noise * 0.3,
        "high": closes + noise,
        "low": closes - noise,
        "close": closes,
        "volume": np.full(n, volume),
    })


def make_choppy_with_extremes(base=100.0, cycles=14, seed=5):
    """Low-ADX chop with sharp V-dips and spikes that drive RSI to true
    oversold/overbought extremes — a realistic ranging market (a smooth sine
    reads as high-ADX alternating trends, which the no-trend gate rejects)."""
    rng = np.random.RandomState(seed)
    seg = []
    for k in range(cycles):
        seg += list(base + rng.randn(12) * 0.4)              # quiet range
        if k % 2 == 0:
            seg += [base - 3, base - 6, base - 9, base - 11, base - 7, base - 2]
        else:
            seg += [base + 3, base + 6, base + 9, base + 11, base + 7, base + 2]
    return np.array(seg, dtype=float)


def test_columns_present():
    out = mean_reversion_pro_core(make_ohlcv(make_choppy_with_extremes()))
    for col in ("signal", "z_score", "adx", "rsi"):
        assert col in out.columns


def test_warmup_returns_no_signal():
    out = mean_reversion_pro_core(make_ohlcv([100.0] * 30))
    assert (out["signal"] == 0).all()


def test_empty_df_is_safe():
    df = pd.DataFrame(columns=["open", "high", "low", "close", "volume"])
    out = mean_reversion_pro_core(df)
    assert "signal" in out.columns
    assert len(out) == 0


def test_oscillating_range_fires_both_sides():
    """A clean low-ADX oscillation should produce both long and short
    reversion entries."""
    out = mean_reversion_pro_core(make_ohlcv(make_choppy_with_extremes()), entry_std=1.5)
    assert (out["signal"] == 1).any(), "expected at least one long reversion"
    assert (out["signal"] == -1).any(), "expected at least one short reversion"


def test_strong_trend_blocks_entries():
    """A strong, steady trend (high ADX) must be filtered out — the whole
    point of the no-trend gate (no falling-knife fades)."""
    closes = np.linspace(100, 300, 400)  # relentless uptrend → high ADX
    out = mean_reversion_pro_core(make_ohlcv(closes, noise=0.2))
    assert (out["signal"] == 0).all()


def test_adx_max_is_respected():
    """With adx_max = 0, the no-trend gate can never open → no entries."""
    out = mean_reversion_pro_core(make_ohlcv(make_choppy_with_extremes()), adx_max=0.0)
    assert (out["signal"] == 0).all()


def test_rsi_confirmation_required():
    """With impossible RSI thresholds (oversold below 0, overbought above 100),
    the oscillator confirmation can never be satisfied → no entries."""
    out = mean_reversion_pro_core(
        make_ohlcv(make_choppy_with_extremes()),
        rsi_oversold=-1.0,
        rsi_overbought=101.0,
    )
    assert (out["signal"] == 0).all()
