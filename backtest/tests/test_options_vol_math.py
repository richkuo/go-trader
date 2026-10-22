import math

import numpy as np
import pytest

from backtest_options import calc_historical_vol


def _numpy_vol(closes, window):
    closes = np.asarray(closes[-(window + 1):], dtype=float)
    log_returns = np.log(closes[1:] / closes[:-1])
    return float(np.std(log_returns, ddof=0) * math.sqrt(365))


def test_vol_matches_numpy_reference_for_random_walk_trend_and_exact_minimum():
    case = "test_matches_numpy_for_random_walk"
    rng = np.random.default_rng(42)
    log_returns = rng.normal(loc=0.0005, scale=0.03, size=120)
    closes = [100.0]
    for r in log_returns:
        closes.append(closes[-1] * math.exp(r))

    window = 30
    got = calc_historical_vol(closes, window=window)
    expected = _numpy_vol(closes, window=window)
    assert got == pytest.approx(expected, rel=1e-9), case

    case = "test_trending_window_vol_matches_reference"
    closes = [100.0 * (1.01 ** i) for i in range(60)]
    got = calc_historical_vol(closes, window=30)
    expected = _numpy_vol(closes, window=30)
    assert got == pytest.approx(expected, rel=1e-9), case
    assert got < 0.01, case

    case = "test_exact_minimum_length_computes_vol"
    window = 14
    rng = np.random.default_rng(7)
    log_returns = rng.normal(loc=0.0, scale=0.02, size=window)
    closes = [100.0]
    for r in log_returns:
        closes.append(closes[-1] * math.exp(r))

    assert len(closes) == window + 1, case
    got = calc_historical_vol(closes, window=window)
    expected = _numpy_vol(closes, window=window)
    assert got == pytest.approx(expected, rel=1e-9), case
    assert got != 0.5, case


def test_too_little_history_returns_default_and_flat_prices_give_zero_vol():
    for case_id, closes, window in [
        ("short_history", [100.0, 101.0, 102.0], 14),
        ("one_below_minimum", [100.0 + i for i in range(14)], 14),
    ]:
        assert calc_historical_vol(closes, window=window) == 0.5, \
            f"test_too_little_history_returns_default[{case_id}]"

    closes = [100.0] * 50
    assert calc_historical_vol(closes, window=14) == pytest.approx(0.0), \
        "test_flat_prices_give_zero_vol"
