"""Tests for shared_tools/atr.py."""

import math
import importlib.util
import pathlib

import pandas as pd
import numpy as np
import pytest

spec = importlib.util.spec_from_file_location(
    "atr", pathlib.Path(__file__).parent / "atr.py"
)
_atr_mod = importlib.util.module_from_spec(spec)
spec.loader.exec_module(_atr_mod)
standard_atr = _atr_mod.standard_atr
ensure_atr_indicator = _atr_mod.ensure_atr_indicator


def _make_ohlcv(n: int = 30, seed: int = 42) -> pd.DataFrame:
    rng = np.random.default_rng(seed)
    close = 100 + np.cumsum(rng.normal(0, 1, n))
    high = close + rng.uniform(0.1, 1.0, n)
    low = close - rng.uniform(0.1, 1.0, n)
    return pd.DataFrame({"open": close, "high": high, "low": low, "close": close, "volume": 1.0})


def test_standard_atr_length():
    df = _make_ohlcv(30)
    result = standard_atr(df, period=14)
    assert len(result) == 30


def test_standard_atr_first_period_minus_one_nan():
    df = _make_ohlcv(30)
    result = standard_atr(df, period=14)
    # TR[0] = high[0]-low[0] (pandas max skips the NaN prev_close terms), so
    # the first ATR window of 14 completes at row 13. Rows 0-12 are NaN.
    assert result.iloc[:13].isna().all()
    assert not math.isnan(result.iloc[13])


def test_standard_atr_positive_after_warmup():
    df = _make_ohlcv(30)
    result = standard_atr(df, period=14)
    valid = result.dropna()
    assert len(valid) > 0
    assert (valid > 0).all()


def test_standard_atr_hand_computed():
    # Construct a simple deterministic case: flat OHLCV so TR = high - low each bar.
    # high = 102, low = 98 every bar → TR = 4 every bar → ATR(3) = 4 after row 3.
    n = 10
    df = pd.DataFrame({
        "open": [100.0] * n,
        "high": [102.0] * n,
        "low": [98.0] * n,
        "close": [100.0] * n,
        "volume": [1.0] * n,
    })
    result = standard_atr(df, period=3)
    # After the 3-bar window plus 1-row shift, index 3 should be 4.0
    assert math.isclose(result.iloc[3], 4.0, rel_tol=1e-9)
    for i in range(3, n):
        assert math.isclose(result.iloc[i], 4.0, rel_tol=1e-9)


def test_ensure_atr_indicator_injects_when_missing():
    df = _make_ohlcv(30)
    assert "atr" not in df.columns
    out = ensure_atr_indicator(df)
    assert "atr" in out.columns
    assert out is df  # mutates in-place


def test_ensure_atr_indicator_noop_when_present():
    df = _make_ohlcv(30)
    sentinel = pd.Series([99.0] * 30, index=df.index)
    df["atr"] = sentinel
    ensure_atr_indicator(df)
    pd.testing.assert_series_equal(df["atr"], sentinel, check_names=False)


def test_ensure_atr_indicator_idempotent():
    df = _make_ohlcv(30)
    ensure_atr_indicator(df)
    first = df["atr"].copy()
    ensure_atr_indicator(df)
    pd.testing.assert_series_equal(df["atr"], first)
