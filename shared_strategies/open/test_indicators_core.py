
import inspect
import math
import os
import sys

import numpy as np
import pandas as pd
import pytest

from shared_strategies.open.conftest import load_module

_OPEN_DIR = os.path.dirname(os.path.abspath(__file__))
_ROOT = os.path.abspath(os.path.join(_OPEN_DIR, "..", ".."))

_INDICATORS_CORE = load_module("_indicators_core_test", os.path.join(_OPEN_DIR, "indicators_core.py"))
HURST_DFA_MIN_POINTS = _INDICATORS_CORE.HURST_DFA_MIN_POINTS
HURST_RS_MIN_POINTS = _INDICATORS_CORE.HURST_RS_MIN_POINTS
atr_from_true_range = _INDICATORS_CORE.atr_from_true_range
atr_sma = _INDICATORS_CORE.atr_sma
atr_sma_series = _INDICATORS_CORE.atr_sma_series
hurst_exponent = _INDICATORS_CORE.hurst_exponent
hurst_rescaled_range = _INDICATORS_CORE.hurst_rescaled_range
round_atr_large = _INDICATORS_CORE.round_atr_large
true_range = _INDICATORS_CORE.true_range
true_range_series = _INDICATORS_CORE.true_range_series
wilder_rsi = _INDICATORS_CORE.wilder_rsi
normalize_atr_method = _INDICATORS_CORE.normalize_atr_method
_hurst_dfa_fluctuation = _INDICATORS_CORE._hurst_dfa_fluctuation
_HURST_RS_MIN_BLOCK = _INDICATORS_CORE._HURST_RS_MIN_BLOCK
_HURST_RS_NUM_BLOCKS = _INDICATORS_CORE._HURST_RS_NUM_BLOCKS
_hurst_rs_block_sizes = _INDICATORS_CORE._hurst_rs_block_sizes
_hurst_rs_statistic = _INDICATORS_CORE._hurst_rs_statistic
_anis_lloyd_expected_rs = _INDICATORS_CORE._anis_lloyd_expected_rs


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


def _ref_chart_patterns_atr(highs, lows, close):
    tr = pd.concat([
        highs - lows,
        (highs - close.shift(1)).abs(),
        (lows - close.shift(1)).abs(),
    ], axis=1).max(axis=1)
    return tr.rolling(window=14, min_periods=1).mean()


def _ref_consolidation_research_atr(df, period):
    high, low, close = df["high"], df["low"], df["close"]
    prev_close = close.shift(1)
    tr = pd.concat(
        [high - low, (high - prev_close).abs(), (low - prev_close).abs()],
        axis=1,
    ).max(axis=1)
    series = tr.rolling(window=period, min_periods=1).mean()
    return series.where(series < 100, series.round(0))


def _ref_tr_native_atr(high, low, close, period):
    tr_native = pd.concat([
        high - low,
        (high - close.shift(1)).abs(),
        (low - close.shift(1)).abs(),
    ], axis=1).max(axis=1)
    _atr_native = tr_native.rolling(window=period).mean()
    return _atr_native.where(_atr_native < 100, _atr_native.round(0))


def _ref_wilder_rsi(close, period):
    delta = close.diff()
    gain = delta.clip(lower=0)
    loss = (-delta).clip(lower=0)
    avg_gain = gain.ewm(alpha=1 / period, min_periods=period, adjust=False).mean()
    avg_loss = loss.ewm(alpha=1 / period, min_periods=period, adjust=False).mean()
    rs = avg_gain / avg_loss
    return 100 - (100 / (1 + rs))


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
    assert rsi.iloc[:2].isna().all()
    assert (rsi.iloc[3:] == 100.0).all()
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
    out = strategies.apply_strategy("mean_reversion", _df(),
                                    {"entry_std": 2.0, "exit_std": 0.5})
    assert "signal" in out.columns


def test_validate_params_checks_without_running_and_matches_wrapper():
    reg = _load_registry()
    reg.validate_params("sma_crossover", {"fast_period": 10, "slow_period": 40})
    with pytest.raises(ValueError) as exc:
        reg.validate_params("sma_crossover", {"fast_period": -1})
    assert "sma_crossover" in str(exc.value) and "constraint" in str(exc.value)


def test_validate_params_catches_cross_param_against_defaults():
    reg = _load_registry()
    with pytest.raises(ValueError, match="fast_period < slow_period"):
        reg.validate_params("sma_crossover", {"fast_period": 100})


def test_validate_params_unknown_strategy_raises():
    reg = _load_registry()
    with pytest.raises(ValueError, match="unknown strategy"):
        reg.validate_params("not_a_strategy", {"x": 1})


def test_validate_param_value_single_param_only():
    reg = _load_registry()
    with pytest.raises(ValueError, match="constraint"):
        reg.validate_param_value("sma_crossover", "fast_period", -1)
    reg.validate_param_value("sma_crossover", "fast_period", 100)


def test_validate_param_value_unknown_strategy_raises():
    reg = _load_registry()
    with pytest.raises(ValueError, match="unknown strategy"):
        reg.validate_param_value("not_a_strategy", "x", 1)


def test_build_registry_entries_carry_constraints():
    reg = _load_registry()
    built = reg.build_registry("spot", include_hidden=True)
    assert built["sma_crossover"]["constraints"] == (
        "fast_period > 0", "fast_period < slow_period")


def test_shim_reexports_validate_params_and_value():
    strategies = _load_by_path(
        "_t_spot_shim_1338", os.path.join(_OPEN_DIR, "spot", "strategies.py")
    )
    strategies.validate_params("sma_crossover", {"fast_period": 10, "slow_period": 40})
    with pytest.raises(ValueError, match="constraint"):
        strategies.validate_param_value("sma_crossover", "fast_period", 0)


def test_wrapper_signature_stays_transparent():
    reg = _load_registry()
    fn = reg.STRATEGIES["mean_reversion"]["fn"]
    params = inspect.signature(fn).parameters
    assert "entry_std" in params and "df" in params


def test_unparseable_constraint_fails_at_registration():
    reg = _load_registry()
    with pytest.raises(ValueError, match="unparseable"):
        reg.register("_bad_constraint", "x", {"a": 1}, constraints=["a !! b"])(
            lambda df, a=1: df
        )


def test_constraint_unknown_lhs_fails_at_registration():
    reg = _load_registry()
    with pytest.raises(ValueError, match="b"):
        reg.register("_bad_lhs", "x", {"a": 1}, constraints=["b > 0"])(
            lambda df, a=1: df
        )


def test_constraint_unknown_rhs_param_fails_at_registration():
    reg = _load_registry()
    with pytest.raises(ValueError, match="d"):
        reg.register("_bad_rhs", "x", {"a": 1, "c": 2}, constraints=["a < d"])(
            lambda df, a=1, c=2: df
        )


def test_constraint_variant_only_param_is_accepted():
    reg = _load_registry()
    reg.register(
        "_variant_param",
        "x",
        {"a": 1},
        platforms=("spot", "futures"),
        variants={"futures": {"default_params": {"e": 3}}},
        constraints=["e > 0"],
    )(lambda df, a=1, e=3: df)


def test_constraint_numeric_literal_rhs_is_accepted():
    reg = _load_registry()
    reg.register("_numeric_rhs", "x", {"a": 1}, constraints=["a > 0"])(
        lambda df, a=1: df
    )


def test_optimizer_treats_constraint_violation_as_skippable():
    sys.path.insert(0, os.path.join(_ROOT, "backtest"))
    try:
        optimizer = _load_by_path(
            "_t_optimizer_1281", os.path.join(_ROOT, "backtest", "optimizer.py")
        )
    finally:
        sys.path.remove(os.path.join(_ROOT, "backtest"))
    assert ValueError in optimizer._EXPECTED_FOLD_ERRORS


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


def test_wilder_atr_never_integer_rounds():
    df = _ohlcv(200.0)
    got = atr_sma(df, 14, method="wilder").dropna()
    assert (got >= 100).any()
    assert (got != got.round(0)).any(), "wilder output looks integer-rounded"
    got_flag_off = atr_sma(df, 14, method="wilder", round_large=False).dropna()
    pd.testing.assert_series_equal(got, got_flag_off, check_exact=True)


def test_wilder_atr_warmup_and_min_periods():
    df = _ohlcv(1.0, n=40)
    got = atr_sma(df, 14, method="wilder")
    assert got.iloc[:13].isna().all()
    assert not pd.isna(got.iloc[13])
    early = atr_sma(df, 14, method="wilder", min_periods=1)
    assert not pd.isna(early.iloc[0])


def test_wilder_differs_from_simple():
    df = _ohlcv(1.0)
    simple = atr_sma(df, 14).dropna()
    wilder = atr_sma(df, 14, method="wilder").dropna()
    common = simple.index.intersection(wilder.index)
    assert not simple.loc[common].equals(wilder.loc[common])


def test_explicit_simple_is_byte_identical_to_default():
    df = _ohlcv(200.0)
    pd.testing.assert_series_equal(
        atr_sma(df, 14, method="simple"), atr_sma(df, 14), check_exact=True
    )


def test_normalize_atr_method_vocabulary():
    assert normalize_atr_method(None) == "simple"
    assert normalize_atr_method("") == "simple"
    assert normalize_atr_method(" Wilder ") == "wilder"
    assert normalize_atr_method("SIMPLE") == "simple"
    for bad in ("rma", "ema", "wilders"):
        with pytest.raises(ValueError, match="atr_method"):
            normalize_atr_method(bad)


def test_unknown_method_fails_loud_at_choke_point():
    df = _ohlcv(1.0)
    with pytest.raises(ValueError, match="atr_method"):
        atr_sma(df, 14, method="rma")


def test_standard_atr_reexport_threads_wilder():
    df = _ohlcv(200.0)
    atr_mod = _load_by_path("_t_atr_1277", os.path.join(_ROOT, "shared_tools", "atr.py"))
    pd.testing.assert_series_equal(
        atr_mod.standard_atr(df, 14, method="wilder"),
        atr_sma(df, 14, method="wilder"),
        check_exact=True,
    )
    assert atr_mod.latest_atr(df, method="wilder") != atr_mod.latest_atr(df)


def _ar1_log_price_series(n, phi, seed, drift=0.0, scale=100.0):
    rng = np.random.RandomState(seed)
    eps = rng.randn(n)
    steps = np.zeros(n)
    for i in range(1, n):
        steps[i] = phi * steps[i - 1] + eps[i]
    log_price = np.cumsum(steps) * 0.01 + np.linspace(0.0, drift, n)
    return pd.Series(scale * np.exp(log_price))


def test_hurst_random_walk_near_half():
    close = _ar1_log_price_series(2000, phi=0.0, seed=1)
    h = hurst_exponent(close)
    assert 0.35 <= h <= 0.65, h


def test_hurst_persistent_series_above_half():
    close = _ar1_log_price_series(2000, phi=0.7, seed=2)
    h = hurst_exponent(close)
    assert h > 0.55, h


def test_hurst_mean_reverting_series_below_half():
    close = _ar1_log_price_series(2000, phi=-0.6, seed=3)
    h = hurst_exponent(close)
    assert h < 0.45, h


def test_hurst_insufficient_data_returns_nan():
    close = pd.Series(np.linspace(100.0, 110.0, HURST_DFA_MIN_POINTS))
    assert np.isnan(hurst_exponent(close))


def test_hurst_exactly_at_minimum_is_not_nan():
    close = _ar1_log_price_series(HURST_DFA_MIN_POINTS + 1, phi=0.0, seed=4)
    h = hurst_exponent(close)
    assert not np.isnan(h)


def test_hurst_constant_price_returns_nan():
    close = pd.Series(np.full(300, 100.0))
    assert np.isnan(hurst_exponent(close))


def test_hurst_non_positive_price_returns_nan():
    close = pd.Series(np.concatenate([np.full(150, 100.0), [-1.0], np.full(150, 100.0)]))
    assert np.isnan(hurst_exponent(close))


def test_hurst_never_raises_on_degenerate_input():
    for close in (
        pd.Series([], dtype=float),
        pd.Series([100.0]),
        pd.Series(np.full(500, float("nan"))),
    ):
        h = hurst_exponent(close)
        assert np.isnan(h)


def test_hurst_deterministic():
    close = _ar1_log_price_series(500, phi=0.3, seed=5)
    assert hurst_exponent(close) == hurst_exponent(close)


def test_hurst_custom_min_points():
    close = _ar1_log_price_series(60, phi=0.0, seed=6)
    assert np.isnan(hurst_exponent(close, min_points=100))
    assert not np.isnan(hurst_exponent(close, min_points=50))


def test_hurst_random_walk_mean_near_half_at_live_frame_size():
    values = [
        hurst_exponent(_ar1_log_price_series(201, phi=0.0, seed=100 + i))
        for i in range(200)
    ]
    values = [v for v in values if not np.isnan(v)]
    assert len(values) > 150, "too many NaN draws to measure the null mean"
    mean_h = float(np.mean(values))
    assert abs(mean_h - 0.5) < 0.03, mean_h


def test_hurst_random_walk_mean_near_half_at_enriched_column_frame_size():
    values = [
        hurst_exponent(_ar1_log_price_series(HURST_DFA_MIN_POINTS + 1, phi=0.0, seed=200 + i))
        for i in range(200)
    ]
    values = [v for v in values if not np.isnan(v)]
    assert len(values) > 150, "too many NaN draws to measure the null mean"
    mean_h = float(np.mean(values))
    assert abs(mean_h - 0.5) < 0.03, mean_h


def test_hurst_random_walk_sd_within_caveat_at_live_frame_size():
    values = [
        hurst_exponent(_ar1_log_price_series(201, phi=0.0, seed=10_000 + i))
        for i in range(1000)
    ]
    values = np.array([v for v in values if not np.isnan(v)])
    assert len(values) > 800, "too many NaN draws to measure the null sd"
    sd = float(values.std())
    assert 0.05 < sd < 0.11, sd


def test_hurst_random_walk_sd_within_caveat_at_enriched_column_frame_size():
    values = [
        hurst_exponent(_ar1_log_price_series(HURST_DFA_MIN_POINTS + 1, phi=0.0, seed=20_000 + i))
        for i in range(1000)
    ]
    values = np.array([v for v in values if not np.isnan(v)])
    assert len(values) > 800, "too many NaN draws to measure the null sd"
    sd = float(values.std())
    assert 0.09 < sd < 0.16, sd


def test_hurst_random_walk_high_percentile_exceeds_no_memory_band_at_enriched_column_frame_size():
    values = [
        hurst_exponent(_ar1_log_price_series(HURST_DFA_MIN_POINTS + 1, phi=0.0, seed=30_000 + i))
        for i in range(1000)
    ]
    values = np.array([v for v in values if not np.isnan(v)])
    assert len(values) > 800, "too many NaN draws to measure the percentile"
    p95 = float(np.percentile(values, 95))
    assert 0.60 < p95 < 0.85, p95


def test_hurst_dfa_fluctuation_vectorization_matches_naive_per_segment_polyfit():
    def naive_fluctuation(profile, scale):
        n = len(profile)
        n_segments = n // scale
        if n_segments < 1:
            return float("nan")
        t = np.arange(scale, dtype=float)
        starts = [profile[: n_segments * scale]]
        tail = profile[n - n_segments * scale:]
        if not np.array_equal(tail, starts[0]):
            starts.append(tail)
        sq_residuals = []
        for block in starts:
            for seg in block.reshape(n_segments, scale):
                coeffs = np.polyfit(t, seg, 1)
                trend = np.polyval(coeffs, t)
                sq_residuals.append(float(np.mean((seg - trend) ** 2)))
        return float(np.sqrt(np.mean(sq_residuals)))

    rng = np.random.default_rng(7)
    for _ in range(50):
        n = int(rng.integers(20, 400))
        scale = int(rng.integers(4, max(5, n // 3)))
        profile = np.cumsum(rng.normal(0, 1, n))
        expected = naive_fluctuation(profile, scale)
        actual = _hurst_dfa_fluctuation(profile, scale)
        if np.isnan(expected):
            assert np.isnan(actual)
            continue
        assert actual == pytest.approx(expected, rel=1e-9)


def test_hurst_rs_random_walk_near_half():
    close = _ar1_log_price_series(2000, phi=0.0, seed=1)
    h = hurst_rescaled_range(close)
    assert 0.35 <= h <= 0.65, h


def test_hurst_rs_persistent_series_above_half():
    close = _ar1_log_price_series(2000, phi=0.7, seed=2)
    h = hurst_rescaled_range(close)
    assert h > 0.5, h


def test_hurst_rs_mean_reverting_series_below_half():
    close = _ar1_log_price_series(2000, phi=-0.6, seed=3)
    h = hurst_rescaled_range(close)
    assert h < 0.5, h


def test_hurst_rs_orders_the_three_regimes():
    mr = hurst_rescaled_range(_ar1_log_price_series(2000, phi=-0.6, seed=3))
    rw = hurst_rescaled_range(_ar1_log_price_series(2000, phi=0.0, seed=1))
    persistent = hurst_rescaled_range(_ar1_log_price_series(2000, phi=0.7, seed=2))
    assert mr < rw < persistent, (mr, rw, persistent)


def test_hurst_rs_insufficient_data_returns_nan():
    close = _ar1_log_price_series(HURST_RS_MIN_POINTS, phi=0.0, seed=4)
    assert np.isnan(hurst_rescaled_range(close))


def test_hurst_rs_exactly_at_minimum_is_not_nan():
    close = _ar1_log_price_series(HURST_RS_MIN_POINTS + 1, phi=0.0, seed=4)
    assert not np.isnan(hurst_rescaled_range(close))


def test_hurst_rs_constant_price_returns_nan():
    close = pd.Series(np.full(300, 100.0))
    assert np.isnan(hurst_rescaled_range(close))


def test_hurst_rs_non_positive_price_returns_nan():
    close = pd.Series(np.concatenate([np.full(150, 100.0), [-1.0], np.full(150, 100.0)]))
    assert np.isnan(hurst_rescaled_range(close))


def test_hurst_rs_never_raises_on_degenerate_input():
    for close in (
        pd.Series([], dtype=float),
        pd.Series([100.0]),
        pd.Series(np.full(500, float("nan"))),
    ):
        assert np.isnan(hurst_rescaled_range(close))


def test_hurst_rs_never_returns_half_as_a_sentinel():
    for close in (
        pd.Series([], dtype=float),
        pd.Series([100.0]),
        pd.Series(np.full(500, float("nan"))),
        pd.Series(np.full(300, 100.0)),
        _ar1_log_price_series(HURST_RS_MIN_POINTS, phi=0.0, seed=4),
    ):
        for corrected in (True, False):
            assert np.isnan(hurst_rescaled_range(close, corrected=corrected))


def test_hurst_rs_deterministic():
    close = _ar1_log_price_series(500, phi=0.3, seed=5)
    assert hurst_rescaled_range(close) == hurst_rescaled_range(close)
    assert (hurst_rescaled_range(close, corrected=False)
            == hurst_rescaled_range(close, corrected=False))


def test_hurst_rs_custom_min_points():
    close = _ar1_log_price_series(120, phi=0.0, seed=6)
    assert np.isnan(hurst_rescaled_range(close, min_points=150))
    assert not np.isnan(hurst_rescaled_range(close, min_points=80))


def test_hurst_rs_anis_lloyd_correction_beats_the_raw_slope_on_a_random_walk():
    close = _ar1_log_price_series(2000, phi=0.0, seed=1)
    corrected = hurst_rescaled_range(close)
    raw = hurst_rescaled_range(close, corrected=False)
    assert corrected != raw
    assert abs(corrected - 0.5) < abs(raw - 0.5), (corrected, raw)


def test_hurst_rs_anis_lloyd_correction_recentres_the_null_mean():
    corrected = []
    raw = []
    for i in range(200):
        close = _ar1_log_price_series(2000, phi=0.0, seed=40_000 + i)
        corrected.append(hurst_rescaled_range(close))
        raw.append(hurst_rescaled_range(close, corrected=False))
    mean_corrected = float(np.nanmean(corrected))
    mean_raw = float(np.nanmean(raw))
    assert mean_raw > 0.53, mean_raw
    assert abs(mean_corrected - 0.5) < abs(mean_raw - 0.5), (mean_corrected, mean_raw)
    assert abs(mean_corrected - 0.5) < 0.03, mean_corrected


def test_hurst_rs_is_noisier_than_dfa_at_the_live_frame_size():
    rs = []
    dfa = []
    for i in range(400):
        close = _ar1_log_price_series(HURST_RS_MIN_POINTS + 1, phi=0.0, seed=50_000 + i)
        rs.append(hurst_rescaled_range(close))
        dfa.append(hurst_exponent(close))
    sd_rs = float(np.nanstd(rs))
    sd_dfa = float(np.nanstd(dfa))
    assert sd_rs > sd_dfa, (sd_rs, sd_dfa)


def test_hurst_rs_block_sizes_mirror_the_dfa_scale_grid_shape():
    for n in (64, 100, 128, 256, 512, 2000):
        blocks = _hurst_rs_block_sizes(n)
        assert blocks.dtype.kind == "i"
        assert list(blocks) == sorted(set(blocks))
        assert int(blocks[0]) >= _HURST_RS_MIN_BLOCK
        assert int(blocks[-1]) == max(_HURST_RS_MIN_BLOCK, n // 4)
        assert len(blocks) <= _HURST_RS_NUM_BLOCKS


def test_hurst_rs_returns_nan_when_the_block_grid_collapses():
    n_returns = 4 * _HURST_RS_MIN_BLOCK
    assert len(_hurst_rs_block_sizes(n_returns)) == 1
    close = _ar1_log_price_series(n_returns + 1, phi=0.0, seed=8)
    assert np.isnan(hurst_rescaled_range(close, min_points=n_returns))


def test_hurst_rs_statistic_matches_a_naive_per_block_range_over_sd():
    def naive(series, block):
        n = len(series)
        n_blocks = n // block
        if n_blocks < 1:
            return float("nan")
        starts = [series[: n_blocks * block]]
        tail = series[n - n_blocks * block:]
        if not np.array_equal(tail, starts[0]):
            starts.append(tail)
        ratios = []
        for part in starts:
            for seg in part.reshape(n_blocks, block):
                sd = float(np.std(seg))
                if sd <= 0:
                    continue
                z = np.cumsum(seg - float(np.mean(seg)))
                ratios.append((float(np.max(z)) - float(np.min(z))) / sd)
        if not ratios:
            return float("nan")
        return float(np.mean(ratios))

    rng = np.random.default_rng(1474)
    for _ in range(50):
        n = int(rng.integers(40, 600))
        block = int(rng.integers(8, max(9, n // 3)))
        series = rng.normal(0, 1, n)
        expected = naive(series, block)
        actual = _hurst_rs_statistic(series, block)
        if np.isnan(expected):
            assert np.isnan(actual)
            continue
        assert actual == pytest.approx(expected, rel=1e-9)


def test_anis_lloyd_expectation_matches_the_published_closed_form():
    def closed_form(n):
        tail = sum(math.sqrt((n - i) / i) for i in range(1, n))
        if n > 340:
            front = 1.0 / math.sqrt(n * math.pi / 2.0)
        else:
            front = math.gamma((n - 1) / 2.0) / (math.sqrt(math.pi)
                                                 * math.gamma(n / 2.0))
        return ((n - 0.5) / n) * front * tail

    for n in (2, 3, 8, 16, 32, 64, 128, 170, 340, 341, 500, 1000):
        assert _anis_lloyd_expected_rs(n) == pytest.approx(closed_form(n), rel=1e-9)


def test_anis_lloyd_expectation_approaches_the_brownian_limit_from_below():
    limit = math.sqrt(math.pi / 2.0)
    sizes = (16, 32, 64, 128, 256, 512, 1024, 2048, 4096)
    ratios = [_anis_lloyd_expected_rs(n) / math.sqrt(n) for n in sizes]
    assert all(r < limit for r in ratios), ratios
    assert ratios == sorted(ratios), ratios
    assert ratios[-1] > 0.98 * limit, ratios[-1]


def test_anis_lloyd_expectation_grows_like_the_square_root_of_the_block():
    sizes = np.array([256, 512, 1024, 2048, 4096], dtype=float)
    values = np.array([_anis_lloyd_expected_rs(int(n)) for n in sizes])
    slope, _intercept = np.polyfit(np.log(sizes), np.log(values), 1)
    assert slope == pytest.approx(0.5, abs=0.02), slope


def test_anis_lloyd_expectation_is_undefined_below_two_blocks():
    assert np.isnan(_anis_lloyd_expected_rs(1))
    assert np.isnan(_anis_lloyd_expected_rs(0))


def test_hurst_dfa_estimator_is_untouched_by_the_rs_addition():
    close = _ar1_log_price_series(2000, phi=0.0, seed=1)
    assert hurst_exponent(close) == pytest.approx(0.5011, abs=5e-4)
    assert hurst_exponent(close) != hurst_rescaled_range(close)
