import os

import numpy as np
import pandas as pd

from shared_strategies.open.conftest import load_module

_OPEN_DIR = os.path.dirname(os.path.abspath(__file__))
_ROOT = os.path.abspath(os.path.join(_OPEN_DIR, "..", ".."))

_INDICATORS_CORE = load_module("_indicators_core_test", os.path.join(_OPEN_DIR, "indicators_core.py"))
atr_sma = _INDICATORS_CORE.atr_sma


def _ohlcv(scale=1.0, n=300, seed=7):
    rng = np.random.RandomState(seed)
    close = scale * (100 + np.cumsum(rng.randn(n) * scale))
    high = close + np.abs(rng.randn(n)) * scale
    low = close - np.abs(rng.randn(n)) * scale
    open_ = close + rng.randn(n) * 0.1 * scale
    return pd.DataFrame(
        {"open": open_, "high": high, "low": low, "close": close,
         "volume": np.full(n, 100.0)},
        index=pd.date_range("2026-01-01", periods=n, freq="1h"),
    )


def _ref_unrounded_atr(df, period):
    tr = pd.concat([
        df["high"] - df["low"],
        (df["high"] - df["close"].shift(1)).abs(),
        (df["low"] - df["close"].shift(1)).abs(),
    ], axis=1).max(axis=1)
    return tr.rolling(window=period).mean()


def _ref_wilder_atr(df, period):
    tr = _ref_unrounded_atr(df, 1)
    out = []
    prev = None
    for v in tr:
        prev = v if prev is None else prev + (v - prev) / period
        out.append(prev)
    series = pd.Series(out, index=df.index)
    series.iloc[: period - 1] = float("nan")
    return series


def test_wilder_atr_matches_hand_computed_rma(subtests):
    for period in [5, 14]:
        for scale in [1.0, 200.0]:
            with subtests.test(period=period, scale=scale):
                df = _ohlcv(scale)
                got = atr_sma(df, period, method="wilder")
                ref = _ref_wilder_atr(df, period)
                pd.testing.assert_series_equal(got, ref, check_exact=False, rtol=1e-12)
