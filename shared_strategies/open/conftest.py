"""Shared pytest helpers and fixtures for strategy tests."""

import numpy as np
import pandas as pd
import pytest


def make_ohlcv(closes, volume=None, noise=0.5, index=None):
    """Build an OHLCV DataFrame from a close price array.

    Args:
        closes: Array-like of close prices.
        volume: Optional array of volumes (default: constant 100).
        noise: Spread between open/high/low and close.
        index: Optional index (e.g. DatetimeIndex for VWAP tests).
    """
    closes = np.array(closes, dtype=float)
    n = len(closes)
    if volume is None:
        volume = np.full(n, 100.0)
    highs = closes + noise
    lows = closes - noise
    opens = closes - noise * 0.3
    df = pd.DataFrame({
        "open": opens,
        "high": highs,
        "low": lows,
        "close": closes,
        "volume": np.array(volume, dtype=float),
    })
    if index is not None:
        df.index = index
    return df


def make_trending_up(n=100, start=100, step=0.5, noise=0.1):
    """Generate an upward trending price series."""
    trend = np.linspace(start, start + step * n, n)
    jitter = np.random.RandomState(42).randn(n) * noise
    return trend + jitter


def make_trending_down(n=100, start=200, step=0.5, noise=0.1):
    """Generate a downward trending price series."""
    trend = np.linspace(start, start - step * n, n)
    jitter = np.random.RandomState(42).randn(n) * noise
    return trend + jitter


def make_flat(n=100, price=100.0):
    """Generate flat price series."""
    return np.full(n, price)


def make_volatile(n=100, center=100.0, amplitude=10.0, seed=42):
    """Generate a volatile oscillating price series."""
    rng = np.random.RandomState(seed)
    return center + amplitude * np.sin(np.linspace(0, 8 * np.pi, n)) + rng.randn(n) * 0.5


@pytest.fixture
def empty_df():
    """Empty OHLCV DataFrame."""
    return pd.DataFrame(columns=["open", "high", "low", "close", "volume"])


@pytest.fixture
def single_row_df():
    """Single-row OHLCV DataFrame."""
    return make_ohlcv([100.0])


@pytest.fixture
def flat_df():
    """Flat price DataFrame (100 bars at 100.0)."""
    return make_ohlcv(make_flat(100), noise=0)


@pytest.fixture
def uptrend_df():
    """Strongly trending up DataFrame (100 bars, 100 -> 150)."""
    return make_ohlcv(make_trending_up(100, start=100, step=0.5))


@pytest.fixture
def downtrend_df():
    """Strongly trending down DataFrame (100 bars, 200 -> 150)."""
    return make_ohlcv(make_trending_down(100, start=200, step=0.5))


@pytest.fixture
def volatile_df():
    """Oscillating price DataFrame for mean-reversion strategies."""
    return make_ohlcv(make_volatile(200))


@pytest.fixture
def nan_df():
    """DataFrame with NaN values sprinkled in."""
    closes = np.array([100.0] * 50)
    closes[5] = np.nan
    closes[20] = np.nan
    closes[35] = np.nan
    return make_ohlcv(closes, noise=0)
