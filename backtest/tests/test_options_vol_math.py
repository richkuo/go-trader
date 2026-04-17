"""
Regression tests for issue #302 — historical volatility must use log returns
and variance around the sample mean, matching
``numpy.std(log_returns, ddof=0) * sqrt(365)``.
"""
import math

import numpy as np
import pytest

from backtest_options import calc_historical_vol


def _numpy_vol(closes, window):
    closes = np.asarray(closes[-(window + 1):], dtype=float)
    log_returns = np.log(closes[1:] / closes[:-1])
    return float(np.std(log_returns, ddof=0) * math.sqrt(365))


def test_matches_numpy_for_random_walk():
    rng = np.random.default_rng(42)
    log_returns = rng.normal(loc=0.0005, scale=0.03, size=120)
    closes = [100.0]
    for r in log_returns:
        closes.append(closes[-1] * math.exp(r))

    window = 30
    got = calc_historical_vol(closes, window=window)
    expected = _numpy_vol(closes, window=window)
    assert got == pytest.approx(expected, rel=1e-9)


def test_trending_window_vol_matches_reference():
    """Non-zero mean return is where the old ``sum(r**2)/n`` overstated vol."""
    closes = [100.0 * (1.01 ** i) for i in range(60)]
    got = calc_historical_vol(closes, window=30)
    expected = _numpy_vol(closes, window=30)
    assert got == pytest.approx(expected, rel=1e-9)
    # Pure trend has ~zero realised vol around the mean.
    assert got < 0.01


def test_short_history_returns_default():
    assert calc_historical_vol([100.0, 101.0, 102.0], window=14) == 0.5


def test_exact_minimum_length_computes_vol():
    """len(closes) == window + 1 is the minimum valid input."""
    window = 14
    rng = np.random.default_rng(7)
    log_returns = rng.normal(loc=0.0, scale=0.02, size=window)
    closes = [100.0]
    for r in log_returns:
        closes.append(closes[-1] * math.exp(r))

    assert len(closes) == window + 1
    got = calc_historical_vol(closes, window=window)
    expected = _numpy_vol(closes, window=window)
    assert got == pytest.approx(expected, rel=1e-9)
    assert got != 0.5  # guard: not the short-history default


def test_one_below_minimum_returns_default():
    """len(closes) == window — one short of the minimum valid length."""
    window = 14
    closes = [100.0 + i for i in range(window)]
    assert calc_historical_vol(closes, window=window) == 0.5


def test_flat_prices_give_zero_vol():
    closes = [100.0] * 50
    assert calc_historical_vol(closes, window=14) == pytest.approx(0.0)
