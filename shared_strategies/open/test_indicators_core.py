"""Tests for indicators_core.py (#1281) — shared Wilder RSI / true-range / ATR
math plus the registration-time parameter-constraint layer.

The equivalence tests pin the shared functions against verbatim copies of the
inline blocks they replaced (byte-identical series, including the rounded vs
unrounded split and per-site ``min_periods`` overrides). Any numeric change —
e.g. the #1277 Wilder-RMA standardization — must update these references
deliberately, never silently.
"""

import importlib.util
import inspect
import os
import sys

import numpy as np
import pandas as pd
import pytest

from indicators_core import (
    atr_from_true_range,
    atr_sma,
    atr_sma_series,
    round_atr_large,
    true_range,
    true_range_series,
    wilder_rsi,
)

_OPEN_DIR = os.path.dirname(os.path.abspath(__file__))
_ROOT = os.path.abspath(os.path.join(_OPEN_DIR, "..", ".."))


def _load_by_path(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def _ohlcv(scale=1.0, n=300, seed=7):
    """Random-walk OHLCV; scale=200 exercises the ATR >= 100 rounding branch."""
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


# --- Verbatim references: the inline blocks the shared module replaced -------


def _ref_standard_atr(df, period):
    """shared_tools/atr.py:standard_atr + the _inline_atr copies (rounded)."""
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
    """registry.py supertrend/squeeze/order_blocks + sweep_squeeze_combo copies."""
    tr = pd.concat([
        df["high"] - df["low"],
        (df["high"] - df["close"].shift(1)).abs(),
        (df["low"] - df["close"].shift(1)).abs(),
    ], axis=1).max(axis=1)
    return tr.rolling(window=period).mean()


def _ref_chart_patterns_atr(highs, lows, close):
    """chart_patterns.py copy: min_periods=1, unrounded."""
    tr = pd.concat([
        highs - lows,
        (highs - close.shift(1)).abs(),
        (lows - close.shift(1)).abs(),
    ], axis=1).max(axis=1)
    return tr.rolling(window=14, min_periods=1).mean()


def _ref_consolidation_research_atr(df, period):
    """backtest/consolidation_research.py copy: min_periods=1, rounded."""
    high, low, close = df["high"], df["low"], df["close"]
    prev_close = close.shift(1)
    tr = pd.concat(
        [high - low, (high - prev_close).abs(), (low - prev_close).abs()],
        axis=1,
    ).max(axis=1)
    series = tr.rolling(window=period, min_periods=1).mean()
    return series.where(series < 100, series.round(0))


def _ref_tr_native_atr(high, low, close, period):
    """regime_adaptive_htf.py tr_native copy (the variant-named 20th site)."""
    tr_native = pd.concat([
        high - low,
        (high - close.shift(1)).abs(),
        (low - close.shift(1)).abs(),
    ], axis=1).max(axis=1)
    _atr_native = tr_native.rolling(window=period).mean()
    return _atr_native.where(_atr_native < 100, _atr_native.round(0))


def _ref_wilder_rsi(close, period):
    """The Wilder-RSI block inlined in 8 files."""
    delta = close.diff()
    gain = delta.clip(lower=0)
    loss = (-delta).clip(lower=0)
    avg_gain = gain.ewm(alpha=1 / period, min_periods=period, adjust=False).mean()
    avg_loss = loss.ewm(alpha=1 / period, min_periods=period, adjust=False).mean()
    rs = avg_gain / avg_loss
    return 100 - (100 / (1 + rs))


# --- Equivalence: shared functions reproduce every replaced variant ----------


@pytest.mark.parametrize("scale", [0.5, 1.0, 200.0])
def test_atr_sma_matches_standard_atr_reference(scale):
    df = _ohlcv(scale)
    pd.testing.assert_series_equal(
        atr_sma(df, 14), _ref_standard_atr(df, 14), check_exact=True
    )


@pytest.mark.parametrize("scale", [1.0, 200.0])
@pytest.mark.parametrize("period", [10, 14, 20])
def test_atr_sma_unrounded_matches_registry_reference(scale, period):
    df = _ohlcv(scale)
    pd.testing.assert_series_equal(
        atr_sma(df, period, round_large=False),
        _ref_unrounded_atr(df, period),
        check_exact=True,
    )


@pytest.mark.parametrize("scale", [1.0, 200.0])
def test_atr_sma_series_min_periods_matches_chart_patterns_reference(scale):
    df = _ohlcv(scale)
    pd.testing.assert_series_equal(
        atr_sma_series(df["high"], df["low"], df["close"], 14,
                       round_large=False, min_periods=1),
        _ref_chart_patterns_atr(df["high"], df["low"], df["close"]),
        check_exact=True,
    )


@pytest.mark.parametrize("scale", [1.0, 200.0])
def test_atr_sma_min_periods_rounded_matches_consolidation_research(scale):
    df = _ohlcv(scale)
    pd.testing.assert_series_equal(
        atr_sma(df, 14, min_periods=1),
        _ref_consolidation_research_atr(df, 14),
        check_exact=True,
    )


@pytest.mark.parametrize("scale", [1.0, 200.0])
def test_atr_sma_series_matches_tr_native_reference(scale):
    df = _ohlcv(scale)
    high = df["high"].astype(float)
    low = df["low"].astype(float)
    close = df["close"].astype(float)
    pd.testing.assert_series_equal(
        atr_sma_series(high, low, close, 20),
        _ref_tr_native_atr(high, low, close, 20),
        check_exact=True,
    )


def test_atr_from_true_range_matches_composed_path():
    df = _ohlcv(200.0)
    tr = true_range(df)
    pd.testing.assert_series_equal(
        atr_from_true_range(tr, 14), atr_sma(df, 14), check_exact=True
    )
    pd.testing.assert_series_equal(tr, true_range_series(
        df["high"], df["low"], df["close"]), check_exact=True)


def test_atr_int_input_matches_float_input_values():
    dfi = _ohlcv(1.0).round(0).astype({"high": int, "low": int, "close": int})
    ref = _ref_unrounded_atr(dfi, 14)
    got = atr_sma(dfi, 14, round_large=False)
    pd.testing.assert_series_equal(got, ref.astype(float), check_exact=True)


@pytest.mark.parametrize("period", [3, 14])
def test_wilder_rsi_matches_reference(period):
    df = _ohlcv(1.0)
    pd.testing.assert_series_equal(
        wilder_rsi(df["close"], period),
        _ref_wilder_rsi(df["close"], period),
        check_exact=True,
    )


def test_wilder_rsi_extremes_and_warmup():
    rising = pd.Series(np.linspace(1, 10, 20))
    rsi = wilder_rsi(rising, 3)
    assert rsi.iloc[:2].isna().all()  # warmup window is NaN, not 50/0
    assert (rsi.iloc[3:] == 100.0).all()  # no losses -> 100
    falling = pd.Series(np.linspace(10, 1, 20))
    assert (wilder_rsi(falling, 3).iloc[3:] == 0.0).all()


def test_round_atr_large_convention():
    s = pd.Series([0.4321, 99.9, 100.0, 123.456])
    out = round_atr_large(s)
    assert out.iloc[0] == 0.4321 and out.iloc[1] == 99.9
    assert out.iloc[2] == 100.0 and out.iloc[3] == 123.0


def test_out_of_tree_consumers_delegate_to_shared_module():
    df = _ohlcv(200.0)
    atr_mod = _load_by_path("_t_atr", os.path.join(_ROOT, "shared_tools", "atr.py"))
    pd.testing.assert_series_equal(
        atr_mod.standard_atr(df, 14), atr_sma(df, 14), check_exact=True
    )
    research = _load_by_path(
        "_t_research", os.path.join(_ROOT, "backtest", "consolidation_research.py")
    )
    pd.testing.assert_series_equal(
        research.atr(df, 14), atr_sma(df, 14, min_periods=1), check_exact=True
    )
    pd.testing.assert_series_equal(
        research.true_range(df), true_range(df), check_exact=True
    )


# --- Parameter constraints (#1281) -------------------------------------------


def _load_registry():
    return _load_by_path("_t_registry_1281", os.path.join(_OPEN_DIR, "registry.py"))


def _df():
    return _ohlcv(1.0, n=120)


def test_constraint_violations_raise_valueerror_naming_strategy():
    reg = _load_registry()
    cases = [
        ("sma_crossover", {"fast_period": 50, "slow_period": 20}),
        ("ema_crossover", {"fast_period": 26, "slow_period": 26}),
        ("rsi", {"period": 0}),
        ("rsi", {"period": -5}),
        ("mean_reversion", {"entry_std": 1.0, "exit_std": 1.0}),
        ("mean_reversion", {"lookback": 0}),
        ("stoch_rsi", {"oversold": 80, "overbought": 20}),
        ("macd", {"fast_period": 26, "slow_period": 12}),
        ("bear_pullback_st", {"ema_short": 200, "ema_mid": 50}),
        ("regime_adaptive_htf", {"period": -3}),
    ]
    for name, kwargs in cases:
        with pytest.raises(ValueError) as exc:
            reg.STRATEGIES[name]["fn"](_df(), **kwargs)
        assert name in str(exc.value)
        assert "constraint" in str(exc.value)


def test_zero_disable_sentinels_stay_accepted():
    reg = _load_registry()
    df = _df()
    # Documented 0 = "disabled" params must not be rejected.
    reg.STRATEGIES["anchored_vwap"]["fn"](df, gate_rsi_period=0, gate_ema_period=0)
    reg.STRATEGIES["regime_adaptive"]["fn"](df, slow_trend_lookback=0)
    reg.STRATEGIES["session_breakout"]["fn"](df, atr_multiplier=0.0)
    reg.STRATEGIES["momentum_pro"]["fn"](df, vol_mult=0)


def test_all_default_params_satisfy_their_declared_constraints():
    reg = _load_registry()
    df = _df()
    for name, entry in reg.STRATEGIES.items():
        if not entry["constraints"]:
            continue
        # Calling with pure defaults must never trip a constraint.
        entry["fn"](df)


def test_variant_default_params_satisfy_constraints():
    reg = _load_registry()
    df = _df()
    for name, entry in reg.STRATEGIES.items():
        if not entry["constraints"]:
            continue
        for platform, variant in entry["variants"].items():
            overrides = variant.get("default_params")
            if overrides:
                entry["fn"](df, **overrides)


def test_apply_strategy_shim_path_validates():
    strategies = _load_by_path(
        "_t_spot_shim_1281", os.path.join(_OPEN_DIR, "spot", "strategies.py")
    )
    with pytest.raises(ValueError, match="constraint"):
        strategies.apply_strategy("mean_reversion", _df(),
                                  {"entry_std": 1.0, "exit_std": 1.0})
    # Valid overrides still run.
    out = strategies.apply_strategy("mean_reversion", _df(),
                                    {"entry_std": 2.0, "exit_std": 0.5})
    assert "signal" in out.columns


def test_wrapper_signature_stays_transparent():
    reg = _load_registry()
    fn = reg.STRATEGIES["mean_reversion"]["fn"]
    params = inspect.signature(fn).parameters
    assert "entry_std" in params and "df" in params  # functools.wraps + __wrapped__


def test_unparseable_constraint_fails_at_registration():
    reg = _load_registry()
    with pytest.raises(ValueError, match="unparseable"):
        reg.register("_bad_constraint", "x", {"a": 1}, constraints=["a !! b"])(
            lambda df, a=1: df
        )


def test_optimizer_treats_constraint_violation_as_skippable():
    # The walk-forward fold loop catches _EXPECTED_FOLD_ERRORS around
    # apply_strategy and skips the combo; the constraint ValueError must be in
    # that set so sweeps containing invalid combos (e.g. mean_reversion
    # entry_std=1.0 x exit_std=1.0) degrade to a skip instead of crashing.
    # optimizer.py needs backtest/ on sys.path for its own imports
    # (registry_loader, backtester) — mirror run_backtest.py's wiring.
    sys.path.insert(0, os.path.join(_ROOT, "backtest"))
    try:
        optimizer = _load_by_path(
            "_t_optimizer_1281", os.path.join(_ROOT, "backtest", "optimizer.py")
        )
    finally:
        sys.path.remove(os.path.join(_ROOT, "backtest"))
    assert ValueError in optimizer._EXPECTED_FOLD_ERRORS
