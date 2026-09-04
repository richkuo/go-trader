
import math

import numpy as np
import pandas as pd
import pytest

from shared_tools.conftest import load_module, make_ohlcv

_atr_mod = load_module("_atr_test", __file__.replace("test_atr.py", "atr.py"))
standard_atr = _atr_mod.standard_atr
ensure_atr_indicator = _atr_mod.ensure_atr_indicator
latest_atr = _atr_mod.latest_atr


def _make_close(n: int = 30, seed: int = 42, start: float = 100.0, scale: float = 1.0) -> np.ndarray:
    rng = np.random.default_rng(seed)
    return start + np.cumsum(rng.normal(0, scale, n))


def test_standard_atr_contract():
    df = make_ohlcv(_make_close(30), volume=1.0, noise=0.5)
    result = standard_atr(df, period=14)
    assert len(result) == 30
    assert result.iloc[:13].isna().all()
    assert not math.isnan(result.iloc[13])
    valid = result.dropna()
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
    df = make_ohlcv(_make_close(30), volume=1.0, noise=0.5)
    assert "atr" not in df.columns
    out = ensure_atr_indicator(df)
    assert "atr" in out.columns
    assert out is df


@pytest.mark.parametrize("kwargs", [{}, {"period": 14, "method": "wilder"}])
def test_ensure_atr_indicator_noop_when_present(kwargs):
    df = make_ohlcv(_make_close(30), volume=1.0, noise=0.5)
    sentinel = pd.Series([99.0] * 30, index=df.index)
    df["atr"] = sentinel
    ensure_atr_indicator(df, **kwargs)
    pd.testing.assert_series_equal(df["atr"], sentinel, check_names=False)


def test_latest_atr_returns_last_finite_value():
    df = make_ohlcv(_make_close(30), volume=1.0, noise=0.5)
    expected = standard_atr(df, period=14).iloc[-1]
    assert math.isclose(latest_atr(df), float(expected), rel_tol=1e-12)


@pytest.mark.parametrize("df,period", [
    (make_ohlcv(_make_close(5), volume=1.0, noise=0.5), 14),
    (pd.DataFrame({"open": [], "high": [], "low": [], "close": [], "volume": []}), 14),
    (make_ohlcv([100.0] * 30, volume=1.0, noise=0.0), 14),
])
def test_latest_atr_returns_zero_when_no_positive_value(df, period):
    assert latest_atr(df, period=period) == 0.0


def test_ensure_atr_indicator_threads_method():
    big = make_ohlcv(_make_close(60, seed=7, start=50_000, scale=300), volume=1.0, noise=200)
    simple = ensure_atr_indicator(big.copy(), period=14)["atr"].dropna()
    wilder = ensure_atr_indicator(big.copy(), period=14, method="wilder")["atr"].dropna()
    assert (simple == simple.round(0)).all()
    assert (wilder != wilder.round(0)).any()


def test_latest_atr_rejects_unknown_method():
    df = make_ohlcv(_make_close(30), volume=1.0, noise=0.5)
    try:
        latest_atr(df, method="rma")
    except ValueError as exc:
        assert "atr_method" in str(exc)
    else:
        raise AssertionError("unknown method must fail loud")


def test_regime_classifier_pinned_to_simple():
    regime_mod = load_module("_regime_atr_test", __file__.replace("test_atr.py", "regime.py"))
    big = make_ohlcv(_make_close(60, seed=7, start=50_000, scale=300), volume=1.0, noise=200)
    got = regime_mod._atr_at_end(big, 14)
    want = float(standard_atr(big, 14, method="simple").iloc[-1])
    assert got == want
    assert got != float(standard_atr(big, 14, method="wilder").iloc[-1])
