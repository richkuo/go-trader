import gzip
import importlib.util
import json
import os
import subprocess
import sys

import numpy as np
import pandas as pd
import pytest

_HERE = os.path.dirname(os.path.abspath(__file__))
_REPO = os.path.abspath(os.path.join(_HERE, "..", ".."))
_TOOLS = os.path.join(_REPO, "shared_tools")
if _TOOLS not in sys.path:
    sys.path.insert(0, _TOOLS)

import offline_manifest as om
from atr import ensure_atr_indicator
from backtester import Backtester, validate_close_capabilities
from close_registry_loader import evaluate as close_evaluate
from eval_windows import expand_sweep
from optimizer import DEFAULT_PARAM_RANGES
from parity_diff import ParityConfig, compute_parity_frame, summarize
from registry_loader import load_registry
from strategy_composition import evaluate_open_close, finalize_decision

NAME = "elder_ray_pullback"
STUDY_DIR = os.path.join(_REPO, "backtest", "candidates", "elder_ray_pullback_1648")
MANIFEST = os.path.join(_REPO, "backtest", "candidates", "elder_ray_pullback_1648_manifest.json")
SEED = os.path.join(STUDY_DIR, "candidate_seed.json")
CLOSE = [{"name": "tiered_tp_pct", "params": {}}]
INPUT_COLUMNS = ["open", "high", "low", "close", "volume"]
HORIZON = 64
DEFAULTS = {"ema_period": 13, "slope_span": 3, "pullback_window": 8}
DOMAIN_ENDPOINTS = [
    {"ema_period": e, "slope_span": s, "pullback_window": w}
    for e in (2, 32) for s in (1, 8) for w in (1, 16)
]
DECISION_COLUMNS = [
    "erp_ema", "erp_ema_prior", "erp_slope", "erp_bull_power", "erp_bear_power", "erp_setup_side",
    "erp_setup_age", "erp_ema_valid", "erp_slope_valid", "erp_valid", "erp_reason", "erp_reason_code", "signal",
]


def _reg():
    return load_registry("futures")


def _apply(df, params=None):
    return _reg().apply_strategy(NAME, df, params)


def _need(params):
    return HORIZON + params["slope_span"] + params["pullback_window"] + 1


def _walk(n, seed, start=100.0, freq="4h", vol=0.02):
    rng = np.random.default_rng(seed)
    close = start * np.exp(np.cumsum(rng.normal(0.0, vol, n)))
    open_ = np.concatenate([[start], close[:-1]])
    high = np.maximum(open_, close) * (1.0 + rng.uniform(0.001, 0.01, n))
    low = np.minimum(open_, close) * (1.0 - rng.uniform(0.001, 0.01, n))
    volume = rng.uniform(100.0, 1000.0, n)
    df = pd.DataFrame({"open": open_, "high": high, "low": low, "close": close, "volume": volume})
    df.index = pd.date_range("2026-01-01", periods=n, freq=freq)
    return df


def _frame(close, low, high, open_=None):
    close = np.asarray(close, dtype=float)
    df = pd.DataFrame({"open": close if open_ is None else np.asarray(open_, dtype=float),
                       "high": np.asarray(high, dtype=float), "low": np.asarray(low, dtype=float),
                       "close": close, "volume": np.full(len(close), 10.0)})
    df.index = pd.date_range("2026-01-01", periods=len(close), freq="4h")
    return df


def _ref_ema(close, t, period):
    alpha = 2.0 / (period + 1.0)
    beta = 1.0 - alpha
    e = close[t - 63]
    for k in range(t - 62, t + 1):
        e = alpha * close[k] + beta * e
    return e


def _ref_decisions(df, params):
    p, s, w = params["ema_period"], params["slope_span"], params["pullback_window"]
    close, high, low = df["close"].tolist(), df["high"].tolist(), df["low"].tolist()
    n = len(close)

    def parts(t):
        e = _ref_ema(close, t, p)
        return e, e - _ref_ema(close, t - s, p), high[t] - e, low[t] - e

    def cond_long(t):
        _, slope, _, bear = parts(t)
        return slope > 0 and bear < 0

    def cond_short(t):
        _, slope, bull, _ = parts(t)
        return slope < 0 and bull > 0

    def run(cond, t):
        r, k = 0, t - 1
        while r <= w and cond(k):
            r += 1
            k -= 1
        return r

    out = {}
    for t in range(_need(params) - 1, n):
        e, slope, bull, bear = parts(t)
        rl, rs = run(cond_long, t), run(cond_short, t)
        sig = 0
        if 1 <= rl <= w and slope > 0 and bear >= 0:
            sig = 1
        elif 1 <= rs <= w and slope < 0 and bull <= 0:
            sig = -1
        out[t] = (e, slope, bull, bear, sig, rl, rs)
    return out


def test_registration_is_futures_only_bidirectional_no_edge_hidden_and_seeded():
    fut = _reg()
    entry = fut.STRATEGY_REGISTRY[NAME]
    raw = fut._registry.STRATEGIES[NAME]
    with open(SEED) as fh:
        seed = json.load(fh)
    assert entry["default_params"] == seed["params"] == DEFAULTS
    assert entry["edge_status"] == "no_edge"
    assert entry["edge_source"] in ("unvalidated", "study_fail", "study_inconclusive")
    assert entry["edge_ref"] == "backtest/candidates/elder_ray_pullback_1648/REPORT.md"
    assert os.path.isfile(os.path.join(_REPO, entry["edge_ref"]))
    assert raw["short_entries"] is True and raw["platforms"] == ("futures",)
    assert callable(raw["param_validator"])
    assert NAME in fut._registry.short_entry_strategies()
    assert NAME in fut._registry.DISCOVERY_HIDDEN_STRATEGIES
    assert NAME not in fut.list_strategies()
    assert NAME not in load_registry("spot").STRATEGY_REGISTRY
    assert DEFAULT_PARAM_RANGES[NAME] == seed["optimizer_grid"]


@pytest.mark.parametrize("platform", ["spot", "futures"])
def test_discovery_output_is_byte_identical_to_the_pre_change_capture(platform):
    proc = subprocess.run([sys.executable, os.path.join(_REPO, "shared_strategies", "open", platform, "strategies.py"),
                           "--list-json"], capture_output=True, cwd=_REPO, timeout=120)
    assert proc.returncode == 0, proc.stderr
    with open(os.path.join(STUDY_DIR, f"discovery_before_{platform}.json"), "rb") as fh:
        before = fh.read()
    with open(os.path.join(STUDY_DIR, f"discovery_after_{platform}.json"), "rb") as fh:
        after = fh.read()
    assert proc.stdout == before == after
    assert NAME.encode() not in proc.stdout


def test_values_and_signals_match_an_independent_reference():
    df = _walk(420, 7)
    sides = set()
    for params in [DEFAULTS, {"ema_period": 2, "slope_span": 1, "pullback_window": 1},
                   {"ema_period": 32, "slope_span": 8, "pullback_window": 16},
                   {"ema_period": 8, "slope_span": 5, "pullback_window": 4}]:
        out = _apply(df, params)
        need = _need(params)
        assert (out["erp_valid"].iloc[:need - 1] == 0.0).all()
        assert (out["erp_reason"].iloc[:need - 1] == "insufficient_history").all()
        assert (out["signal"].iloc[:need - 1] == 0).all()
        ref = _ref_decisions(df, params)
        for t, (e, slope, bull, bear, sig, rl, rs) in ref.items():
            row = out.iloc[t]
            assert row["erp_valid"] == 1.0
            assert (row["erp_ema"], row["erp_slope"], row["erp_bull_power"], row["erp_bear_power"]) == (
                e, slope, bull, bear)
            assert int(row["signal"]) == sig, t
            if sig:
                assert row["erp_reason"] == ("entry_long" if sig == 1 else "entry_short")
                assert row["erp_reason_code"] == float(sig)
                assert row["erp_setup_side"] == float(sig)
                assert row["erp_setup_age"] == float(rl if sig == 1 else rs)
            sides.add(sig)
    assert sides == {-1, 0, 1}


def _trend_frame(n, pullbacks, *, rising=True, deep=10.0, shallow=1.0):
    close = np.array([100.0 + i if rising else 1000.0 - i for i in range(n)])
    if rising:
        low = close - shallow
        high = close + 1.0
        for start, length in pullbacks:
            low[start:start + length] = close[start:start + length] - deep
    else:
        high = close + shallow
        low = close - 1.0
        for start, length in pullbacks:
            high[start:start + length] = close[start:start + length] + deep
    return _frame(close, low, high)


@pytest.mark.parametrize("rising", [True, False])
@pytest.mark.parametrize("window", [1, 4, 8, 16])
def test_age_equal_to_the_window_enters_and_one_more_bar_expires(rising, window):
    params = dict(DEFAULTS, pullback_window=window)
    side = 1 if rising else -1
    ok_start, bad_start = 150, 200
    df = _trend_frame(260, [(ok_start, window), (bad_start, window + 1)], rising=rising)
    out = _apply(df, params)
    assert out["erp_reason"].iloc[ok_start] == ("pullback_long" if rising else "pullback_short")
    assert out["erp_setup_age"].iloc[ok_start] == 0.0
    assert out["erp_setup_age"].iloc[ok_start + window - 1] == float(window - 1)
    hit = ok_start + window
    assert int(out["signal"].iloc[hit]) == side
    assert out["erp_setup_age"].iloc[hit] == float(window)
    miss = bad_start + window + 1
    assert out["erp_reason"].iloc[bad_start + window] == ("pullback_long" if rising else "pullback_short")
    assert out["erp_setup_age"].iloc[bad_start + window] == float(window)
    assert int(out["signal"].iloc[miss]) == 0
    assert out["erp_reason"].iloc[miss] == "setup_expired"
    assert (out["signal"].to_numpy() != 0).sum() == 1


def test_zero_bear_power_recovers_and_the_next_float_below_zero_does_not():
    df = _trend_frame(220, [(150, 3)])
    out = _apply(df, DEFAULTS)
    assert int(out["signal"].iloc[153]) == 1
    ema = float(out["erp_ema"].iloc[153])
    assert df["close"].iloc[153] > ema
    at_zero = df.copy()
    at_zero.iloc[153, at_zero.columns.get_loc("low")] = ema
    zero_out = _apply(at_zero, DEFAULTS)
    assert zero_out["erp_bear_power"].iloc[153] == 0.0
    assert int(zero_out["signal"].iloc[153]) == 1 and zero_out["erp_reason"].iloc[153] == "entry_long"
    below = df.copy()
    below.iloc[153, below.columns.get_loc("low")] = float(np.nextafter(ema, 0.0))
    below_out = _apply(below, DEFAULTS)
    assert below_out["erp_bear_power"].iloc[153] < 0.0
    assert int(below_out["signal"].iloc[153]) == 0 and below_out["erp_reason"].iloc[153] == "pullback_long"


def test_zero_bull_power_recovers_a_short():
    df = _trend_frame(220, [(150, 3)], rising=False)
    out = _apply(df, DEFAULTS)
    assert int(out["signal"].iloc[153]) == -1
    ema = float(out["erp_ema"].iloc[153])
    at_zero = df.copy()
    at_zero.iloc[153, at_zero.columns.get_loc("high")] = ema
    zero_out = _apply(at_zero, DEFAULTS)
    assert zero_out["erp_bull_power"].iloc[153] == 0.0
    assert int(zero_out["signal"].iloc[153]) == -1


def test_one_setup_gives_one_entry_and_a_new_pullback_is_needed_for_another():
    df = _trend_frame(240, [(150, 3), (160, 2)])
    out = _apply(df, DEFAULTS)
    entries = np.flatnonzero(out["signal"].to_numpy() == 1)
    assert entries.tolist() == [153, 162]
    assert (out["erp_reason"].iloc[154:160] == "no_setup").all()


def test_an_exact_zero_slope_clears_a_live_setup():
    close = [100.0] * 100 + [200.0] * 140
    df = _frame(close, [c - 1e-9 for c in close], [c + 1.0 for c in close])
    out = _apply(df, {"ema_period": 2, "slope_span": 1, "pullback_window": 16})
    hits = np.flatnonzero(((out["erp_reason"] == "trend_cleared") & (out["erp_slope"] == 0.0)
                           & (out["erp_valid"] == 1.0)).to_numpy())
    assert len(hits) == 1
    t = int(hits[0])
    assert out["erp_reason"].iloc[t - 1] == "pullback_long" and out["erp_slope"].iloc[t - 1] > 0
    assert int(out["signal"].iloc[t]) == 0
    assert (out["signal"] == 0).all()


def test_an_opposite_slope_clears_a_live_long_setup():
    close = np.array([100.0 + i for i in range(200)])
    close[155:] = close[154] - 30.0 * np.arange(1, 46)
    close = np.maximum(close, 1.0)
    low = close - 1.0
    low[150:155] = close[150:155] - 10.0
    df = _frame(close, low, close + 1.0)
    out = _apply(df, DEFAULTS)
    assert out["erp_reason"].iloc[154] == "pullback_long"
    cleared = [i for i in range(155, 170) if out["erp_reason"].iloc[i] == "trend_cleared"]
    assert cleared and out["erp_slope"].iloc[cleared[0]] <= 0
    assert (out["signal"].iloc[150:170] != 1).all()


def test_constant_input_never_arms():
    df = _frame([150.0] * 200, [149.0] * 200, [151.0] * 200)
    out = _apply(df, DEFAULTS)
    valid = out["erp_valid"] == 1.0
    assert valid.any()
    assert (out.loc[valid, "erp_slope"] == 0.0).all()
    assert (out.loc[valid, "erp_reason"] == "no_setup").all()
    assert (out["signal"] == 0).all()


def _holds_at_end(df, params, reason):
    before = df.copy()
    out = _apply(df, params)
    pd.testing.assert_frame_equal(df, before)
    for col in df.columns:
        pd.testing.assert_series_equal(out[col], before[col])
    assert int(out["signal"].iloc[-1]) == 0
    assert out["erp_valid"].iloc[-1] == 0.0
    assert out["erp_reason"].iloc[-1] == reason, out["erp_reason"].iloc[-1]
    numeric = out.drop(columns=["erp_reason"]).filter(like="erp_")
    assert np.isfinite(numeric.to_numpy(dtype=float)).all()
    return out


P = {"ema_period": 8, "slope_span": 2, "pullback_window": 4}


def _shift_index(df, pos, delta):
    idx = list(df.index)
    idx[pos] = idx[pos] + delta
    return df.set_axis(pd.DatetimeIndex(idx))


@pytest.mark.parametrize("mutate,reason", [
    (lambda df: df.drop(columns=["volume"]), "missing_columns"),
    (lambda df: df.drop(columns=["open", "high"]), "missing_columns"),
    (lambda df: df.reset_index(drop=True), "missing_timestamps"),
    (lambda df: df.set_axis(pd.DatetimeIndex(list(df.index[:-1]) + [pd.NaT])), "missing_timestamps"),
    (lambda df: df.assign(high=np.r_[df["high"].to_numpy()[:-1], np.nan]), "nonfinite_input"),
    (lambda df: df.assign(close=np.r_[df["close"].to_numpy()[:-1], np.inf]), "nonfinite_input"),
    (lambda df: df.assign(volume=np.r_[df["volume"].to_numpy()[:-1], np.inf]), "nonfinite_input"),
    (lambda df: df.assign(close=df["close"].astype(object).where(df.index != df.index[-1], "x")), "nonfinite_input"),
    (lambda df: df.assign(low=np.r_[df["low"].to_numpy()[:-1], 0.0]), "nonpositive_price"),
    (lambda df: df.assign(open=np.r_[df["open"].to_numpy()[:-1], df["high"].iloc[-1] * 2]), "invalid_candle_geometry"),
    (lambda df: df.assign(close=np.r_[df["close"].to_numpy()[:-1], df["low"].iloc[-1] / 2]), "invalid_candle_geometry"),
    (lambda df: df.assign(high=np.r_[df["high"].to_numpy()[:-1], df["low"].iloc[-1] / 2]), "invalid_candle_geometry"),
    (lambda df: df.assign(volume=np.r_[df["volume"].to_numpy()[:-1], -1.0]), "negative_volume"),
    (lambda df: df.set_axis(pd.DatetimeIndex(list(df.index[:-1]) + [df.index[-2]])), "timestamp_order"),
    (lambda df: df.set_axis(pd.DatetimeIndex(list(df.index[:-1]) + [df.index[-3]])), "timestamp_order"),
    (lambda df: _shift_index(df, len(df) - 1, pd.Timedelta(hours=4)), "irregular_cadence"),
    (lambda df: _shift_index(df, len(df) - 30, pd.Timedelta(hours=1)), "irregular_cadence"),
    (lambda df: df.iloc[-(_need(P) - 1):], "insufficient_history"),
    (lambda df: df.iloc[:0], None),
])
def test_invalid_entry_data_is_a_reasoned_numeric_hold_that_preserves_the_frame(mutate, reason):
    df = mutate(_walk(160, 3))
    if reason is None:
        out = _apply(df, P)
        assert out.empty and "erp_reason" in out.columns and "signal" in out.columns
        return
    _holds_at_end(df, P, reason)


@pytest.mark.parametrize("params", DOMAIN_ENDPOINTS)
def test_largest_finite_prices_stay_finite_because_the_average_is_a_convex_combination(params):
    huge = np.finfo(float).max
    rng = np.random.default_rng(4)
    close = huge * (1.0 - rng.uniform(0.0, 1e-3, 200))
    df = _frame(close, close * (1.0 - 1e-4), np.full(200, huge))
    out = _apply(df, params)
    numeric = out.drop(columns=["erp_reason"]).filter(like="erp_")
    assert np.isfinite(numeric.to_numpy(dtype=float)).all()
    assert (out["erp_valid"].iloc[_need(params) - 1:] == 1.0).all()
    assert not (out["erp_reason"] == "arithmetic_overflow").any()


def test_timestamp_column_is_accepted_without_a_datetime_index():
    df = _walk(200, 3)
    plain = df.reset_index(drop=True)
    plain["timestamp"] = (df.index.asi8 // 1_000_000).astype("int64")
    a = _apply(df, P)
    b = _apply(plain, P)
    for col in DECISION_COLUMNS:
        np.testing.assert_array_equal(a[col].to_numpy(), b[col].to_numpy())


def test_a_defect_holds_only_inside_its_span_and_recovery_matches_a_clean_frame():
    df = _walk(400, 5)
    bad = 200
    need = _need(P)
    corrupt = df.copy()
    corrupt.iloc[bad, corrupt.columns.get_loc("volume")] = np.nan
    out = _apply(corrupt, P)
    clean = _apply(df, P)
    assert (out["erp_reason"].iloc[bad:bad + need] == "nonfinite_input").all()
    assert (out["erp_valid"].iloc[bad:bad + need] == 0.0).all()
    assert (out["signal"].iloc[bad:bad + need] == 0).all()
    pd.testing.assert_frame_equal(out[DECISION_COLUMNS].iloc[:bad], clean[DECISION_COLUMNS].iloc[:bad])
    pd.testing.assert_frame_equal(out[DECISION_COLUMNS].iloc[bad + need:], clean[DECISION_COLUMNS].iloc[bad + need:])
    gapped = df.drop(df.index[bad])
    out = _apply(gapped, P)
    assert (out["erp_reason"].iloc[bad:bad + need - 1] == "irregular_cadence").all()
    assert out["erp_valid"].iloc[bad + need - 1] == 1.0


@pytest.mark.parametrize("params", [
    {"ema_period": 1}, {"ema_period": 33}, {"ema_period": 13.5}, {"ema_period": True},
    {"ema_period": float("nan")}, {"ema_period": "13"}, {"ema_period": None},
    {"slope_span": 0}, {"slope_span": 9}, {"slope_span": 2.5}, {"slope_span": False}, {"slope_span": float("inf")},
    {"pullback_window": 0}, {"pullback_window": 17}, {"pullback_window": 3.25}, {"pullback_window": "8"},
    {"pullback_window": -1},
])
def test_invalid_parameters_are_explicit_errors_before_evaluation(params):
    reg = _reg()
    merged = {**DEFAULTS, **params}
    with pytest.raises((ValueError, TypeError)):
        reg._registry.validate_params(NAME, merged)
    with pytest.raises((ValueError, TypeError)):
        _apply(_walk(120, 1), merged)


def test_integral_float_parameters_are_accepted_as_integers():
    df = _walk(200, 1)
    floats = {"ema_period": 13.0, "slope_span": 3.0, "pullback_window": 8.0}
    _reg()._registry.validate_params(NAME, floats)
    pd.testing.assert_frame_equal(_apply(df, floats), _apply(df, DEFAULTS))


def _frozen(key, bars):
    manifest = om.load_manifest(MANIFEST)
    candles = om.load_candles(om.dataset_by_key(manifest, key))
    return candles[INPUT_COLUMNS].iloc[-bars:]


def _grid_and_endpoints():
    specs = [(k, list(v)) for k, v in DEFAULT_PARAM_RANGES[NAME].items()]
    return [p for _, p in expand_sweep(dict(DEFAULTS), specs)] + DOMAIN_ENDPOINTS


def test_full_series_prefix_exact_span_and_200_row_tail_agree():
    df = _frozen("ETH 4h", 500)
    for params in _grid_and_endpoints():
        need = _need(params)
        full = _apply(df, params)
        ends = sorted({need, 201, 260, 300, 377, 450, 500})
        for end in ends:
            frames = [df.iloc[:end], df.iloc[end - need:end]]
            if end >= 200:
                frames.append(df.iloc[end - 200:end])
            for part in frames:
                got = _apply(part, params)[DECISION_COLUMNS].iloc[-1]
                pd.testing.assert_series_equal(got, full[DECISION_COLUMNS].iloc[end - 1], check_names=False)
            short = _apply(df.iloc[end - need + 1:end], params)
            assert short["erp_reason"].iloc[-1] == "insufficient_history"


def _composer(df, params, side="", position_ctx=None):
    reg = _reg()
    evaluation = evaluate_open_close(
        reg.apply_strategy, reg.get_strategy, df, NAME, NAME, ["tiered_tp_pct"], side, params=params,
        position_ctx=position_ctx, close_evaluate=close_evaluate, close_params_by_name={"tiered_tp_pct": {}})
    return evaluation, finalize_decision(evaluation, side)


def test_composer_emits_both_directions_and_holds_entries_while_a_position_is_open():
    long_df = _trend_frame(154, [(150, 3)])
    short_df = _trend_frame(154, [(150, 3)], rising=False)
    _, flat_long = _composer(long_df, DEFAULTS)
    _, flat_short = _composer(short_df, DEFAULTS)
    assert (flat_long["open_action"], flat_long["signal"]) == ("long", 1)
    assert (flat_short["open_action"], flat_short["signal"]) == ("short", -1)
    for df, side in ((long_df, "long"), (short_df, "short"), (long_df, "short"), (short_df, "long")):
        mark = float(df["close"].iloc[-1])
        ctx = {"side": side, "avg_cost": mark, "entry_atr": 50.0, "current_quantity": 1.0, "initial_quantity": 1.0}
        _, held = _composer(df, DEFAULTS, side, ctx)
        assert held["signal"] == 0 and held["close_fraction"] == 0.0


@pytest.mark.parametrize("mutation", ["volume", "geometry", "cadence", "warmup", "nonfinite"])
def test_invalid_entry_frame_keeps_the_close_owner_executable(mutation):
    df = _walk(200, 9)
    if mutation == "volume":
        df = df.assign(volume=np.r_[df["volume"].to_numpy()[:-1], -5.0])
    elif mutation == "geometry":
        df = df.assign(open=np.r_[df["open"].to_numpy()[:-1], df["high"].iloc[-1] * 2])
    elif mutation == "cadence":
        df = _shift_index(df, len(df) - 1, pd.Timedelta(hours=2))
    elif mutation == "warmup":
        df = df.iloc[-40:]
    else:
        df = df.assign(volume=np.r_[df["volume"].to_numpy()[:-5], np.nan, df["volume"].to_numpy()[-4:]])
    mark = float(df["close"].iloc[-1])
    ctx = {"side": "long", "avg_cost": mark / 1.05, "entry_atr": 1.0, "current_quantity": 1.0,
           "initial_quantity": 1.0}
    evaluation, decision = _composer(df, P, "long", ctx)
    assert evaluation.open_result_df["erp_valid"].iloc[-1] == 0.0
    assert decision["open_action"] == "none"
    assert decision["close_strategy"] == "tiered_tp_pct" and decision["close_fraction"] == 1.0
    assert decision["signal"] == -1
    short_ctx = dict(ctx, side="short", avg_cost=mark * 1.05)
    _, decision = _composer(df, P, "short", short_ctx)
    assert decision["open_action"] == "none" and decision["close_fraction"] == 1.0 and decision["signal"] == 1
    partial = dict(ctx, avg_cost=mark / 1.015)
    _, decision = _composer(df, P, "long", partial)
    assert decision["close_fraction"] == 0.25


def test_close_and_stop_owners_pass_strict_capability_validation():
    report = validate_close_capabilities(close_refs=CLOSE, comparison_mode=None, platform="hyperliquid",
                                         strategy_type="perps", phase="preflight").to_dict()
    assert report["close_eligibility"] == "eligible" and not report["approximations"] and not report["refusals"]


def _bt(params, **kw):
    base = dict(initial_capital=1000.0, platform="hyperliquid", open_strategy={"name": NAME, "params": params},
                close_strategies=CLOSE, stop_loss_atr_mult=1.0, direction="both")
    base.update(kw)
    return Backtester(**base)


def test_simulator_fills_next_open_with_explicit_exits_partials_and_no_reversal_or_scale_in():
    df = _walk(900, 5)
    sig = ensure_atr_indicator(_apply(df, DEFAULTS))
    res = _bt(DEFAULTS).run(sig, strategy_name=NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    trades = res["trades"]
    assert {t["side"] for t in trades} == {"long", "short"}
    reasons = {t["exit_reason"].split(":")[0] for t in trades}
    assert reasons <= {"sl", "tiered_tp_pct", "end_of_data"} and "tiered_tp_pct" in reasons and "sl" in reasons
    by_entry = {}
    for t in trades:
        by_entry.setdefault((t["entry_date"], t["side"]), []).append(t)
    spans = []
    for (entry_date, side), legs in by_entry.items():
        entry = pd.Timestamp(entry_date)
        pos = sig.index.get_loc(entry)
        assert int(sig["signal"].iloc[pos - 1]) == (1 if side == "long" else -1)
        assert legs[0]["entry_price"] == pytest.approx(df["open"].iloc[pos], rel=0.01)
        spans.append((entry, max(pd.Timestamp(leg["exit_date"]) for leg in legs)))
    spans.sort()
    for (_, prev_exit), (next_entry, _) in zip(spans, spans[1:]):
        assert next_entry >= prev_exit
    assert any(len(legs) > 1 for legs in by_entry.values())


def test_simulator_keeps_closing_and_stopping_after_entry_inputs_fail():
    df = _walk(600, 5)
    sig = ensure_atr_indicator(_apply(df, DEFAULTS))
    first = next(int(i) for i in np.flatnonzero(sig["signal"].to_numpy() != 0)
                 if i > 120 and np.isfinite(sig["atr"].iloc[i]))
    corrupt = df.copy()
    corrupt.iloc[first + 1:, corrupt.columns.get_loc("volume")] = np.nan
    bad = ensure_atr_indicator(_apply(corrupt, DEFAULTS))
    assert (bad["signal"].iloc[:first + 1] == sig["signal"].iloc[:first + 1]).all()
    assert (bad["signal"].iloc[first + 1:] == 0).all()
    res = _bt(DEFAULTS).run(bad, strategy_name=NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    last_entry = max(pd.Timestamp(t["entry_date"]) for t in res["trades"])
    assert last_entry == bad.index[first + 1]
    final = [t for t in res["trades"] if pd.Timestamp(t["entry_date"]) == last_entry]
    assert {t["exit_reason"].split(":")[0] for t in final} <= {"sl", "tiered_tp_pct"}


def _cfg(params, **kw):
    base = dict(strategy_name=NAME, params=params, registry="futures", platform="hyperliquid",
                symbol="BTC", timeframe="4h")
    base.update(kw)
    return ParityConfig(**base)


@pytest.mark.parametrize("window", [200, None])
@pytest.mark.parametrize("params", [DOMAIN_ENDPOINTS[0], DOMAIN_ENDPOINTS[-1], DEFAULTS])
def test_entry_parity_on_frozen_candles(window, params):
    frame = compute_parity_frame(_frozen("BTC 4h", 320), cfg=_cfg(params), window=window)
    result = summarize(frame)
    assert result["bars_compared"] > 0
    assert result["clean"], frame[~frame["match"]].head()


@pytest.mark.parametrize("window", [200, None])
def test_composed_parity_covers_both_sides_with_the_close_owner(window):
    cfg = _cfg(DEFAULTS, close_refs=json.loads(json.dumps(CLOSE)), direction="both")
    frame = compute_parity_frame(_frozen("SOL 4h", 520), cfg=cfg, window=window)
    result = summarize(frame)
    assert result["mismatches"] == 0 and result["clean"], frame[~frame["match"]].head()
    assert (frame["live_open_action"] == "long").any() and (frame["live_open_action"] == "short").any()
    closing = frame[frame["live_close_fraction"] > 0]
    assert (closing["live_signal"] == -1).any() and (closing["live_signal"] == 1).any()


@pytest.mark.parametrize("window", [200, None])
def test_batched_explicit_paper_parity_matches_solo(window):
    cfg = _cfg(DEFAULTS, close_refs=json.loads(json.dumps(CLOSE)), direction="both", batched=True)
    frame = compute_parity_frame(_frozen("BTC 4h", 300), cfg=cfg, window=window)
    result = summarize(frame)
    assert result["bars_compared"] > 0
    assert result["clean"] and result["batch_clean"], frame[~frame["match"]].head()


def _load_study():
    path = os.path.join(STUDY_DIR, "run_study.py")
    spec = importlib.util.spec_from_file_location("_erp_run_study", path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def _rewrite_gz_csv(src, dst, keep):
    with gzip.open(src, "rt") as fh:
        lines = fh.read().splitlines()
    kept = [lines[0]] + [line for i, line in enumerate(lines[1:]) if keep(i, line)]
    with open(dst, "wb") as raw:
        with gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=0) as fh:
            fh.write(("\n".join(kept) + "\n").encode("utf-8"))


def test_missing_funding_refuses_the_coverage_gate_and_gives_an_inconclusive_verdict(tmp_path):
    study = _load_study()
    with open(MANIFEST) as fh:
        raw = json.load(fh)
    base = os.path.dirname(MANIFEST)
    refs = [raw["venue_reference"]["meta"], raw["venue_reference"]["l2_snapshot"]]
    refs += [ds[key] for ds in raw["datasets"] for key in ("candles", "funding")]
    for ref in refs:
        dst = tmp_path / ref["path"]
        dst.parent.mkdir(parents=True, exist_ok=True)
        dst.write_bytes(open(os.path.join(base, ref["path"]), "rb").read())
    eth = next(d for d in raw["datasets"] if d["coin"] == "ETH")
    test_start = int(pd.Timestamp(raw["windows"]["test"]["start"], tz="UTC").timestamp() * 1000)
    gapped = tmp_path / eth["funding"]["path"]

    def keep(_, line):
        ts = int(float(line.split(",")[0]))
        return not (test_start + 10 * 3_600_000 <= ts < test_start + 13 * 3_600_000)

    _rewrite_gz_csv(os.path.join(base, eth["funding"]["path"]), str(gapped), keep)
    eth["funding"]["sha256"] = om.sha256_file(str(gapped))
    path = tmp_path / "manifest.json"
    path.write_text(json.dumps(raw))
    manifest = om.load_manifest(str(path))
    gate = study.preflight(manifest)
    assert gate["complete"] is False
    refused = {(r["dataset"], r["window"]) for r in gate["refused"]}
    assert refused == {("ETH 4h", "test")}
    assert any("funding" in p for r in gate["refused"] for p in r["problems"])
    result = study.run(str(path))
    assert result["verdict"]["outcome"] == "inconclusive"
    assert "coverage gate refused ETH 4h test" in result["verdict"]["reason"]
    assert "arms" not in result


def test_frozen_manifest_gate_is_complete_and_hashes_verify():
    study = _load_study()
    manifest = om.load_manifest(MANIFEST)
    gate = study.preflight(manifest)
    assert gate["complete"], gate["refused"]
    assert len(gate["windows"]) == 6
