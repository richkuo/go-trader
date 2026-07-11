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


# --- #1277: method threading --------------------------------------------------


def _make_big_ohlcv(n: int = 60, seed: int = 7) -> pd.DataFrame:
    """BTC-scale frame whose ATR exceeds 100, exercising the #887 rounding."""
    rng = np.random.default_rng(seed)
    close = 50_000 + np.cumsum(rng.normal(0, 300, n))
    high = close + rng.uniform(50, 400, n)
    low = close - rng.uniform(50, 400, n)
    return pd.DataFrame({"open": close, "high": high, "low": low, "close": close, "volume": 1.0})


def test_ensure_atr_indicator_threads_method():
    big = _make_big_ohlcv()
    simple = ensure_atr_indicator(big.copy(), period=14)["atr"].dropna()
    wilder = ensure_atr_indicator(big.copy(), period=14, method="wilder")["atr"].dropna()
    # Simple path integer-rounds >=100 (#887); wilder never does.
    assert (simple == simple.round(0)).all()
    assert (wilder != wilder.round(0)).any()


def test_ensure_atr_indicator_preserves_strategy_atr_regardless_of_method():
    # A strategy-emitted `atr` column always wins — the #1277 cutover must
    # never re-base it (the cutover-roster invariant).
    df = _make_ohlcv(30)
    df["atr"] = 42.0
    out = ensure_atr_indicator(df, period=14, method="wilder")
    assert (out["atr"] == 42.0).all()


def test_latest_atr_rejects_unknown_method():
    df = _make_ohlcv(30)
    try:
        latest_atr(df, method="rma")
    except ValueError as exc:
        assert "atr_method" in str(exc)
    else:
        raise AssertionError("unknown method must fail loud")


def test_regime_classifier_pinned_to_simple():
    """#1277: regime atr_pct must stay on the frozen simple math even when a
    caller would resolve wilder for stop geometry — composite thresholds and
    the #1085 directional certifications were calibrated on simple."""
    spec_r = importlib.util.spec_from_file_location(
        "_t_regime_1277", pathlib.Path(__file__).parent / "regime.py"
    )
    regime_mod = importlib.util.module_from_spec(spec_r)
    spec_r.loader.exec_module(regime_mod)
    big = _make_big_ohlcv()
    got = regime_mod._atr_at_end(big, 14)
    want = float(standard_atr(big, 14, method="simple").iloc[-1])
    assert got == want
    # Sanity: the wilder value differs on this frame, so the equality above
    # genuinely proves the pin (not a frame where both methods coincide).
    assert got != float(standard_atr(big, 14, method="wilder").iloc[-1])
