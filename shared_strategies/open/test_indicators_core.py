import os

import numpy as np
import pandas as pd
import pytest

from shared_strategies.open.conftest import load_module

_OPEN_DIR = os.path.dirname(os.path.abspath(__file__))
_ROOT = os.path.abspath(os.path.join(_OPEN_DIR, "..", ".."))

_INDICATORS_CORE = load_module("_indicators_core_test", os.path.join(_OPEN_DIR, "indicators_core.py"))
atr_sma = _INDICATORS_CORE.atr_sma


_load_by_path = load_module


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


def _ref_standard_atr(df, period):
    high = df["high"].astype(float)
    low = df["low"].astype(float)
    prev_close = df["close"].astype(float).shift(1)
    tr = pd.concat(
        [high - low, (high - prev_close).abs(), (low - prev_close).abs()],
        axis=1,
    ).max(axis=1)
    atr = tr.rolling(window=period).mean()
    return atr.where(atr < 100, atr.round(0))


def _ref_unrounded_atr(df, period):
    tr = pd.concat([
        df["high"] - df["low"],
        (df["high"] - df["close"].shift(1)).abs(),
        (df["low"] - df["close"].shift(1)).abs(),
    ], axis=1).max(axis=1)
    return tr.rolling(window=period).mean()


@pytest.mark.parametrize("scale", [0.5, 1.0, 200.0])
def test_atr_sma_matches_standard_atr_reference(scale):
    df = _ohlcv(scale)
    pd.testing.assert_series_equal(
        atr_sma(df, 14), _ref_standard_atr(df, 14), check_exact=True
    )


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


@pytest.mark.parametrize("scale", [1.0, 200.0])
@pytest.mark.parametrize("period", [5, 14])
def test_wilder_atr_matches_hand_computed_rma(scale, period):
    df = _ohlcv(scale)
    got = atr_sma(df, period, method="wilder")
    ref = _ref_wilder_atr(df, period)
    pd.testing.assert_series_equal(got, ref, check_exact=False, rtol=1e-12)


def test_standard_atr_reexport_threads_wilder():
    df = _ohlcv(200.0)
    atr_mod = _load_by_path("_t_atr_1277", os.path.join(_ROOT, "shared_tools", "atr.py"))
    pd.testing.assert_series_equal(
        atr_mod.standard_atr(df, 14, method="wilder"),
        atr_sma(df, 14, method="wilder"),
        check_exact=True,
    )
    assert atr_mod.latest_atr(df, method="wilder") != atr_mod.latest_atr(df)
