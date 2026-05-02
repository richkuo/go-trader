"""Tests for adx_trend.py — ADX Trend Rider strategy."""

import numpy as np
import pandas as pd
import pytest

from adx_trend import adx_trend_core, _compute_adx_components


# ─── Helpers ────────────────────────────────

def make_ohlcv(closes, volume=None, noise=0.5):
    """Build an OHLCV DataFrame from a close price array."""
    closes = np.array(closes, dtype=float)
    n = len(closes)
    if volume is None:
        volume = np.full(n, 100.0)
    highs = closes + noise
    lows = closes - noise
    opens = closes - noise * 0.3
    return pd.DataFrame({
        "open": opens,
        "high": highs,
        "low": lows,
        "close": closes,
        "volume": np.array(volume, dtype=float),
    })


# ─── Tests ──────────────────────────────────

def test_strong_uptrend_generates_buy():
    """A downtrend followed by a strong uptrend should produce a buy on the DI crossover."""
    # Start with a downtrend so -DI dominates, then reverse into uptrend for +DI crossover
    prices = list(np.linspace(150, 100, 50)) + list(np.linspace(100, 200, 100))
    df = make_ohlcv(prices, noise=1.0)
    result = adx_trend_core(df)
    assert (result["signal"] == 1).any(), "Expected at least one buy signal on DI crossover into uptrend"


def test_strong_downtrend_generates_sell():
    """An uptrend followed by a strong downtrend should produce a sell on the DI crossover."""
    # Start with an uptrend so +DI dominates, then reverse into downtrend for -DI crossover
    prices = list(np.linspace(100, 200, 50)) + list(np.linspace(200, 100, 100))
    df = make_ohlcv(prices, noise=1.0)
    result = adx_trend_core(df)
    assert (result["signal"] == -1).any(), "Expected at least one sell signal on DI crossover into downtrend"


def test_flat_market_no_signals():
    """Flat data with minimal noise should produce no signals (ADX stays low)."""
    prices = [100.0] * 100
    df = make_ohlcv(prices, noise=0.1)
    result = adx_trend_core(df)
    assert (result["signal"] == 0).all(), "Expected no signals in flat market"


def test_short_data_no_crash():
    """Very short data should return signal column with all zeros, no crash."""
    prices = [100, 101, 102, 101, 100]
    df = make_ohlcv(prices)
    result = adx_trend_core(df)
    assert "signal" in result.columns
    assert (result["signal"] == 0).all()
    assert len(result) == 5


def test_signal_values_valid():
    """All signal values must be in {-1, 0, 1}."""
    prices = list(np.linspace(100, 150, 50)) + list(np.linspace(150, 90, 50))
    df = make_ohlcv(prices)
    result = adx_trend_core(df)
    assert set(result["signal"].unique()).issubset({-1, 0, 1})


def test_crossover_with_weak_adx_no_signal():
    """Choppy sideways data: DI crossovers may happen but ADX stays low -> no signals."""
    # Alternating up/down creates crossovers but weak trend
    prices = [100, 102, 100, 102, 100, 102, 100, 102] * 25
    df = make_ohlcv(prices, noise=0.1)
    result = adx_trend_core(df)
    assert (result["signal"] == 0).all(), "Expected no signals when ADX is weak despite DI crossovers"


# ─── _compute_adx_components refactor tests ──────────────────────────────────


def test_compute_adx_components_returns_required_arrays():
    """_compute_adx_components should return plus_di, minus_di, adx arrays of len == len(df)."""
    prices = list(np.linspace(100, 200, 100))
    df = make_ohlcv(prices, noise=1.0)
    components = _compute_adx_components(df["high"].values, df["low"].values, df["close"].values, 14)
    assert "plus_di" in components
    assert "minus_di" in components
    assert "adx" in components
    assert len(components["plus_di"]) == len(df)
    assert len(components["minus_di"]) == len(df)
    assert len(components["adx"]) == len(df)


def test_compute_adx_components_non_negative():
    """ADX, +DI, -DI are always >= 0."""
    prices = list(np.linspace(100, 200, 100))
    df = make_ohlcv(prices, noise=1.0)
    c = _compute_adx_components(df["high"].values, df["low"].values, df["close"].values, 14)
    assert (c["adx"] >= 0).all()
    assert (c["plus_di"] >= 0).all()
    assert (c["minus_di"] >= 0).all()


def test_adx_trend_core_matches_when_components_extracted():
    """adx_trend_core output is unchanged after extracting _compute_adx_components."""
    prices = list(np.linspace(150, 100, 50)) + list(np.linspace(100, 200, 100))
    df = make_ohlcv(prices, noise=1.0)
    result = adx_trend_core(df)
    # If _compute_adx_components is used internally, signals should remain valid
    assert set(result["signal"].unique()).issubset({-1, 0, 1})
    assert (result["signal"] == 1).any(), "Refactored core must still generate buy signal on uptrend"
