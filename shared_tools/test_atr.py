"""Tests for shared_tools/atr.py."""

import math
import importlib.util
import pathlib

import pandas as pd
import numpy as np

spec = importlib.util.spec_from_file_location(
    "atr", pathlib.Path(__file__).parent / "atr.py"
)
_atr_mod = importlib.util.module_from_spec(spec)
spec.loader.exec_module(_atr_mod)
standard_atr = _atr_mod.standard_atr
ensure_atr_indicator = _atr_mod.ensure_atr_indicator
latest_atr = _atr_mod.latest_atr


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


def test_standard_atr_warmup_nan():
    df = _make_ohlcv(30)
    result = standard_atr(df, period=14)
    # TR[0] = high[0]-low[0] (max skips NaN prev_close terms), so the first
    # ATR window of 14 completes at row 13. Rows 0-12 are NaN.
    assert result.iloc[:13].isna().all()
    assert not math.isnan(result.iloc[13])


def test_standard_atr_positive_after_warmup():
    df = _make_ohlcv(30)
    valid = standard_atr(df, period=14).dropna()
    assert len(valid) > 0
    assert (valid > 0).all()


def test_standard_atr_hand_computed():
    n = 10
    df = pd.DataFrame({
        "open": [100.0] * n,
        "high": [102.0] * n,
        "low": [98.0] * n,
        "close": [100.0] * n,
        "volume": [1.0] * n,
    })
    result = standard_atr(df, period=3)
    assert math.isclose(result.iloc[3], 4.0, rel_tol=1e-9)
    for i in range(3, n):
        assert math.isclose(result.iloc[i], 4.0, rel_tol=1e-9)


def test_ensure_atr_indicator_injects_when_missing():
    df = _make_ohlcv(30)
    assert "atr" not in df.columns
    out = ensure_atr_indicator(df)
    assert "atr" in out.columns
    assert out is df


def test_ensure_atr_indicator_noop_when_present():
    df = _make_ohlcv(30)
    sentinel = pd.Series([99.0] * 30, index=df.index)
    df["atr"] = sentinel
    ensure_atr_indicator(df)
    pd.testing.assert_series_equal(df["atr"], sentinel, check_names=False)


def test_latest_atr_returns_last_finite_value():
    df = _make_ohlcv(30)
    expected = standard_atr(df, period=14).iloc[-1]
    assert math.isclose(latest_atr(df), float(expected), rel_tol=1e-12)


def test_latest_atr_zero_when_warmup_incomplete():
    # Only 5 rows, period=14 → all NaN, no positive value to return.
    df = _make_ohlcv(5)
    assert latest_atr(df, period=14) == 0.0


def test_latest_atr_zero_for_empty_series():
    df = pd.DataFrame({"open": [], "high": [], "low": [], "close": [], "volume": []})
    assert latest_atr(df) == 0.0


def test_latest_atr_strict_positive():
    # Construct a flat-bar dataframe where TR == 0 → ATR == 0 → latest_atr returns 0.
    n = 30
    df = pd.DataFrame({
        "open": [100.0] * n,
        "high": [100.0] * n,
        "low": [100.0] * n,
        "close": [100.0] * n,
        "volume": [1.0] * n,
    })
    assert latest_atr(df) == 0.0
