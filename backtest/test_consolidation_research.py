
import os
import sys

import numpy as np
import pandas as pd

sys.path.insert(0, os.path.dirname(__file__))

from consolidation_research import detect_regression_flatness, detect_volatility_contraction


def _candles(highs, lows, closes, opens=None):
    n = len(closes)
    idx = pd.date_range("2024-01-01", periods=n, freq="1h")
    opens = opens if opens is not None else closes
    return pd.DataFrame(
        {
            "open": opens,
            "high": highs,
            "low": lows,
            "close": closes,
            "volume": [1.0] * n,
        },
        index=idx,
    )


def test_regression_flatness_detects_flat_run():
    flat = np.full(16, 100.0) + np.random.RandomState(0).normal(0, 0.05, 16)
    ramp = np.linspace(100.5, 130.0, 16)
    closes = np.concatenate([flat, ramp])
    df = _candles(closes + 0.1, closes - 0.1, closes)
    eps = detect_regression_flatness(df, min_bars=8)
    assert eps, "should detect the flat run"
    assert eps[0].start_idx < 16
