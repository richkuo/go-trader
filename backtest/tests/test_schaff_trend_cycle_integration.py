import contextlib
import json
import os
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
from backtester import Backtester
from close_registry_loader import evaluate as close_evaluate
from closed_bar import (
    FRAME_DECISION_TIME_ATTR,
    FRAME_RECEIVED_ORDER_DEFECTS_ATTR,
    TIMESTAMP_KIND_NATIVE_CLOSE,
    TIMESTAMP_KIND_OPEN,
    declare_frame_cadence,
)
from eval_windows import expand_sweep
from optimizer import DEFAULT_PARAM_RANGES
from parity_diff import ParityConfig, compute_parity_frame, summarize
from registry_loader import load_registry
from strategy_composition import evaluate_open_close, finalize_decision

NAME = "schaff_trend_cycle"
MANIFEST = os.path.join(_REPO, "backtest", "candidates", "schaff_trend_cycle_1651_manifest.json")
SEED = os.path.join(_REPO, "backtest", "candidates", "schaff_trend_cycle_1651", "candidate_seed.json")
CLOSE = [{"name": "tiered_tp_pct", "params": {}}]
INPUT_COLUMNS = ["open", "high", "low", "close", "volume"]
REPLAY = 192
NEED = 193
FOUR_H = 14_400_000
DEFAULTS = {"fast_period": 23, "slow_period": 50, "cycle_length": 10, "smoothing": 0.5,
            "lower_threshold": 25.0, "upper_threshold": 75.0}
DOMAIN_ENDPOINTS = [
    dict(DEFAULTS, fast_period=2, slow_period=3, cycle_length=2, smoothing=0.25, lower_threshold=0.5,
         upper_threshold=99.5),
    dict(DEFAULTS, fast_period=32, slow_period=64, cycle_length=16, smoothing=1.0),
    dict(DEFAULTS, fast_period=2, slow_period=64, cycle_length=16, smoothing=0.25),
    dict(DEFAULTS, fast_period=31, slow_period=32, cycle_length=2, smoothing=1.0),
]


def _reg():
    return load_registry("futures")


@contextlib.contextmanager
def _case(label):
    try:
        yield
    except AssertionError as exc:
        raise AssertionError(f"case {label}: {exc}") from exc
    except pytest.fail.Exception as exc:
        pytest.fail(f"case {label}: {exc}")
    except Exception as exc:
        exc.add_note(f"case {label}")
        raise


def _apply(df, params=None):
    return _reg().apply_strategy(NAME, df, params)


def _walk(n, seed, start=100.0, sigma=0.02):
    rng = np.random.default_rng(seed)
    close = start * np.exp(np.cumsum(rng.normal(0.0, sigma, n)))
    open_ = np.concatenate([[start], close[:-1]])
    high = np.maximum(open_, close) * (1.0 + rng.uniform(0.001, 0.01, n))
    low = np.minimum(open_, close) * (1.0 - rng.uniform(0.001, 0.01, n))
    volume = rng.uniform(100.0, 1000.0, n)
    df = pd.DataFrame({"open": open_, "high": high, "low": low, "close": close, "volume": volume})
    df.index = pd.date_range("2026-01-01", periods=n, freq="4h")
    return declare_frame_cadence(df, "4h", TIMESTAMP_KIND_OPEN)


def ref_endpoint(close, t, fast, slow, cycle, smooth):
    w = [float(x) for x in close[t - REPLAY + 1:t + 1]]
    af = 2.0 / (fast + 1)
    asl = 2.0 / (slow + 1)
    ef = es = w[0]
    macd = [ef - es]
    for x in w[1:]:
        ef = af * x + (1.0 - af) * ef
        es = asl * x + (1.0 - asl) * es
        macd.append(ef - es)
    kept = macd[slow - 1:]
    zero = False
    s1 = []
    raw1 = None
    for k in range(cycle - 1, len(kept)):
        win = kept[k - cycle + 1:k + 1]
        lo, hi = min(win), max(win)
        if not hi - lo > 0:
            zero = True
            raw1 = 0.0
        else:
            raw1 = 100.0 * (kept[k] - lo) / (hi - lo)
        s1.append(raw1 if not s1 else s1[-1] + smooth * (raw1 - s1[-1]))
    value = None
    raw2 = None
    for k in range(cycle - 1, len(s1)):
        win = s1[k - cycle + 1:k + 1]
        lo, hi = min(win), max(win)
        if not hi - lo > 0:
            zero = True
            raw2 = 0.0
        else:
            raw2 = 100.0 * (s1[k] - lo) / (hi - lo)
        value = raw2 if value is None else value + smooth * (raw2 - value)
    return {"value": value, "macd": macd[-1], "stoch1_raw": raw1, "stoch1": s1[-1], "stoch2_raw": raw2,
            "zero": zero}


def ref_decision(cur, prev, lower, upper):
    if cur["zero"] or prev["zero"]:
        return None
    if prev["value"] <= lower < cur["value"] and cur["macd"] > prev["macd"]:
        return 1
    if prev["value"] >= upper > cur["value"] and cur["macd"] < prev["macd"]:
        return -1
    return 0


def test_registration_is_futures_only_bidirectional_no_edge_hidden_and_seeded():
    fut = _reg()
    entry = fut.STRATEGY_REGISTRY[NAME]
    raw = fut._registry.STRATEGIES[NAME]
    with open(SEED) as fh:
        seed = json.load(fh)
    assert entry["default_params"] == seed["params"] == DEFAULTS
    assert entry["edge_status"] == "no_edge"
    assert entry["edge_source"] in ("unvalidated", "study_fail", "study_inconclusive")
    assert entry["edge_ref"] == "backtest/candidates/schaff_trend_cycle_1651/REPORT.md"
    assert raw["short_entries"] is True and raw["platforms"] == ("futures",)
    assert raw["param_validator"] is not None
    assert NAME in fut._registry.short_entry_strategies()
    assert NAME in fut._registry.DISCOVERY_HIDDEN_STRATEGIES
    assert NAME not in fut.list_strategies()
    assert NAME not in load_registry("spot").STRATEGY_REGISTRY
    assert DEFAULT_PARAM_RANGES[NAME] == seed["optimizer_grid"]
    for _, params in expand_sweep(dict(DEFAULTS), [(k, list(v)) for k, v in DEFAULT_PARAM_RANGES[NAME].items()]):
        fut._registry.validate_params(NAME, params)


@pytest.mark.parametrize("params", [DEFAULTS, dict(DEFAULTS, fast_period=12, slow_period=26, cycle_length=8,
                                                   lower_threshold=20.0, upper_threshold=80.0)] + DOMAIN_ENDPOINTS)
def test_values_and_decisions_match_an_independent_reference(params):
    df = _walk(520, 7)
    out = _apply(df, params)
    close = df["close"].to_numpy()
    p = (params["fast_period"], params["slow_period"], params["cycle_length"], params["smoothing"])
    assert (out["stc_reason"].iloc[:NEED - 1] == "insufficient_history").all()
    seen = set()
    for t in range(REPLAY - 1, len(df)):
        cur = ref_endpoint(close, t, *p)
        row = out.iloc[t]
        assert row["stc_value_valid"] == (0.0 if cur["zero"] else 1.0)
        if not cur["zero"]:
            for key in ("value", "macd", "stoch1_raw", "stoch1", "stoch2_raw"):
                assert row[f"stc_{key}" if key != "value" else "stc_value"] == cur[key], (t, key)
        if t < NEED - 1:
            continue
        prev = ref_endpoint(close, t - 1, *p)
        want = ref_decision(cur, prev, params["lower_threshold"], params["upper_threshold"])
        if want is None:
            assert int(row["signal"]) == 0 and row["stc_valid"] == 0.0 and row["stc_reason"] == "zero_range"
            continue
        assert row["stc_valid"] == 1.0
        assert row["stc_prev_value"] == prev["value"] and row["stc_prev_macd"] == prev["macd"]
        assert int(row["signal"]) == want, t
        assert row["stc_reason_code"] == {1: 1.0, -1: -1.0}.get(want, row["stc_reason_code"])
        seen.add(want)
    if params is DEFAULTS:
        assert seen == {-1, 0, 1}


def _first(out, side, reason=None):
    rows = np.flatnonzero(out["signal"].to_numpy() == side) if reason is None else \
        np.flatnonzero(out["stc_reason"].to_numpy() == reason)
    assert len(rows)
    return int(rows[0])


def test_threshold_equality_rules_and_strict_crossings():
    df = _walk(900, 11)
    out = _apply(df, DEFAULTS)
    for side in (1, -1):
        with _case(f"side={side}"):
            _threshold_equality_rules_and_strict_crossings(df, out, side)


def _threshold_equality_rules_and_strict_crossings(df, out, side):
    i = _first(out, side)
    prev, cur = float(out["stc_prev_value"].iloc[i]), float(out["stc_value"].iloc[i])
    key = "lower_threshold" if side == 1 else "upper_threshold"
    at_prev = _apply(df, dict(DEFAULTS, **{key: prev}))
    assert int(at_prev["signal"].iloc[i]) == side
    at_cur = _apply(df, dict(DEFAULTS, **{key: cur}))
    assert int(at_cur["signal"].iloc[i]) == 0 and at_cur["stc_reason"].iloc[i] == "no_cross"
    beyond = float(np.nextafter(prev, -np.inf if side == 1 else np.inf))
    past = _apply(df, dict(DEFAULTS, **{key: beyond}))
    assert int(past["signal"].iloc[i]) == 0 and past["stc_reason"].iloc[i] == "no_cross"


def test_a_crossing_with_the_wrong_macd_slope_holds_and_sustained_conditions_do_not_repeat():
    df = _walk(3000, 1)
    out = _apply(df, DEFAULTS)
    i = _first(out, 0, "slope_disagrees")
    row = out.iloc[i]
    up = row["stc_prev_value"] <= 25.0 < row["stc_value"]
    assert (row["stc_macd_slope"] <= 0) if up else (row["stc_macd_slope"] >= 0)
    assert int(row["signal"]) == 0
    signal = out["signal"].to_numpy()
    entries = np.flatnonzero(signal != 0)
    assert len(entries) > 20
    for a in entries:
        if a + 1 < len(signal):
            assert signal[a + 1] != signal[a]
    above = out["stc_value"].to_numpy() > 25.0
    sustained = np.flatnonzero(above[1:] & above[:-1]) + 1
    assert len(sustained) and not (signal[sustained] == 1).any()


def _holds_at_end(df, params, reason):
    before = df.copy()
    out = _apply(df, params)
    pd.testing.assert_frame_equal(df, before)
    for col in df.columns:
        pd.testing.assert_series_equal(out[col], before[col])
    assert int(out["signal"].iloc[-1]) == 0
    assert out["stc_valid"].iloc[-1] == 0.0
    assert out["stc_reason"].iloc[-1] == reason, out["stc_reason"].iloc[-1]
    return out


def _with_index(df, idx):
    out = df.set_axis(pd.DatetimeIndex(idx))
    out.attrs = dict(df.attrs)
    return out


def _shift_index(df, pos, delta):
    idx = list(df.index)
    idx[pos] = idx[pos] + delta
    return _with_index(df, idx)


def _no_cadence(df):
    out = df.copy()
    out.attrs = {}
    return out


INVALID_ENTRY_DATA = [
    (lambda df: df.drop(columns=["volume"]), "missing_columns"),
    (_no_cadence, "missing_cadence"),
    (lambda df: df.reset_index(drop=True), "missing_timestamps"),
    (lambda df: _with_index(df, list(df.index[:-1]) + [pd.NaT]), "missing_timestamps"),
    (lambda df: df.assign(high=np.r_[df["high"].to_numpy()[:-1], np.nan]), "nonfinite_input"),
    (lambda df: df.assign(volume=np.r_[df["volume"].to_numpy()[:-1], np.inf]), "nonfinite_input"),
    (lambda df: df.assign(low=np.r_[df["low"].to_numpy()[:-1], 0.0]), "nonpositive_price"),
    (lambda df: df.assign(close=np.r_[df["close"].to_numpy()[:-1], -1.0]), "nonpositive_price"),
    (lambda df: df.assign(open=np.r_[df["open"].to_numpy()[:-1], df["high"].iloc[-1] * 2]), "invalid_candle_geometry"),
    (lambda df: df.assign(high=np.r_[df["high"].to_numpy()[:-1], df["low"].iloc[-1] / 2]), "invalid_candle_geometry"),
    (lambda df: df.assign(volume=np.r_[df["volume"].to_numpy()[:-1], -1.0]), "negative_volume"),
    (lambda df: _with_index(df, list(df.index[:-1]) + [df.index[-2]]), "timestamp_order"),
    (lambda df: _with_index(df, list(df.index[:-2]) + [df.index[-1], df.index[-2]]), "timestamp_order"),
    (lambda df: _shift_index(df, len(df) - 1, pd.Timedelta(hours=4)), "cadence_gap"),
    (lambda df: _shift_index(df, len(df) - 150, pd.Timedelta(hours=1)), "cadence_gap"),
    (lambda df: df.iloc[-(NEED - 1):], "insufficient_history"),
]


def test_invalid_entry_data_is_a_reasoned_hold_that_preserves_the_frame():
    for i, (mutate, reason) in enumerate(INVALID_ENTRY_DATA):
        with _case(f"{i}-{reason}"):
            _holds_at_end(mutate(_walk(400, 3)), DEFAULTS, reason)


def test_constant_prices_hold_on_zero_range_and_zero_volume_is_valid():
    df = _walk(400, 3).assign(open=100.0, high=101.0, low=99.0, close=100.0)
    _holds_at_end(df, DEFAULTS, "zero_range")
    base = _walk(400, 3)
    zero_volume = base.assign(volume=0.0)
    a, b = _apply(base, DEFAULTS), _apply(zero_volume, DEFAULTS)
    np.testing.assert_array_equal(a["signal"].to_numpy(), b["signal"].to_numpy())
    assert (b["stc_reason"] == a["stc_reason"]).all()


def test_received_out_of_order_rows_hold_only_inside_their_span():
    df = _walk(700, 5)
    clean = _apply(df, DEFAULTS)
    marked = df.copy()
    bad = 300
    marked.attrs[FRAME_RECEIVED_ORDER_DEFECTS_ATTR] = [int(df.index[bad].value // 1_000_000)]
    out = _apply(marked, DEFAULTS)
    assert (out["stc_reason"].iloc[bad:bad + NEED] == "timestamp_order").all()
    assert (out["signal"].iloc[bad:bad + NEED] == 0).all()
    pd.testing.assert_frame_equal(out.iloc[:bad], clean.iloc[:bad])
    pd.testing.assert_frame_equal(out.iloc[bad + NEED:], clean.iloc[bad + NEED:])


def test_a_defect_holds_only_inside_its_span_and_recovery_matches_a_clean_frame():
    df = _walk(700, 5)
    bad = 300
    corrupt = df.copy()
    corrupt.iloc[bad, corrupt.columns.get_loc("volume")] = np.nan
    out = _apply(corrupt, DEFAULTS)
    clean = _apply(df, DEFAULTS)
    assert (out["stc_reason"].iloc[bad:bad + NEED] == "nonfinite_input").all()
    assert (out["stc_valid"].iloc[bad:bad + NEED] == 0.0).all()
    pd.testing.assert_frame_equal(out.iloc[:bad], clean.iloc[:bad])
    pd.testing.assert_frame_equal(out.iloc[bad + NEED:], clean.iloc[bad + NEED:])
    gapped = df.drop(df.index[bad])
    out = _apply(gapped, DEFAULTS)
    assert (out["stc_reason"].iloc[bad:bad + NEED - 1] == "cadence_gap").all()
    assert out["stc_reason"].iloc[bad + NEED - 1] != "cadence_gap"


INVALID_PARAMS = [
    {"fast_period": 1}, {"fast_period": 33}, {"slow_period": 2}, {"slow_period": 65},
    {"fast_period": 30, "slow_period": 30}, {"fast_period": 40, "slow_period": 30}, {"cycle_length": 1},
    {"cycle_length": 17}, {"fast_period": 12.5}, {"fast_period": True}, {"fast_period": "23"},
    {"slow_period": float("nan")}, {"smoothing": 0.2}, {"smoothing": 1.01}, {"smoothing": float("inf")},
    {"smoothing": True}, {"lower_threshold": 0.0}, {"upper_threshold": 100.0},
    {"lower_threshold": 75.0, "upper_threshold": 75.0}, {"lower_threshold": 80.0, "upper_threshold": 70.0},
    {"lower_threshold": float("nan")}, {"upper_threshold": "75"},
]


def test_invalid_parameters_are_explicit_errors():
    for params in INVALID_PARAMS:
        with _case(params):
            with pytest.raises(ValueError):
                _apply(_walk(300, 1), dict(DEFAULTS, **params))
            with pytest.raises(ValueError):
                _reg()._registry.validate_params(NAME, dict(DEFAULTS, **params))


def test_integral_float_periods_are_accepted():
    df = _walk(300, 1)
    floats = dict(DEFAULTS, fast_period=23.0, slow_period=50.0, cycle_length=10.0)
    pd.testing.assert_frame_equal(_apply(df, floats), _apply(df, DEFAULTS))


@pytest.mark.parametrize("params", DOMAIN_ENDPOINTS + [DEFAULTS])
def test_193_bars_are_required_and_sufficient_at_every_supported_bound(params):
    df = _walk(400, 9)
    full = _apply(df, params)
    for end in range(NEED, 400, 37):
        exact = _apply(df.iloc[end - NEED:end], params)
        assert exact["stc_reason"].iloc[-1] != "insufficient_history"
        assert exact["stc_reason"].iloc[-1] == full["stc_reason"].iloc[end - 1]
        short = _apply(df.iloc[end - NEED + 1:end], params)
        assert short["stc_reason"].iloc[-1] == "insufficient_history"


def _frozen(key, bars):
    manifest = om.load_manifest(MANIFEST)
    candles = om.load_candles(om.dataset_by_key(manifest, key))
    frame = candles[INPUT_COLUMNS].iloc[-bars:].copy()
    return declare_frame_cadence(frame, "4h", TIMESTAMP_KIND_OPEN)


def _grid_and_endpoints():
    specs = [(k, list(v)) for k, v in DEFAULT_PARAM_RANGES[NAME].items()]
    return [p for _, p in expand_sweep(dict(DEFAULTS), specs)] + DOMAIN_ENDPOINTS


DECISION_COLUMNS = ["stc_value", "stc_prev_value", "stc_macd", "stc_prev_macd", "stc_valid",
                    "stc_reason_code", "signal"]


def test_full_series_prefix_exact_span_and_200_row_tail_agree_for_the_grid_and_bounds():
    df = _frozen("ETH 4h", 700)
    for params in _grid_and_endpoints():
        full = _apply(df, params)
        decisions = np.flatnonzero(full["signal"].to_numpy() != 0)
        ends = sorted({int(i) + 1 for i in decisions[decisions >= 200][:2]} | {250, 400, 700})
        for end in ends:
            for part in (df.iloc[:end], df.iloc[end - 200:end], df.iloc[end - NEED:end]):
                got = _apply(part, params)[DECISION_COLUMNS].iloc[-1]
                pd.testing.assert_series_equal(got, full[DECISION_COLUMNS].iloc[end - 1], check_names=False)


def _runtime(df, kind, cutoff=None, boundary=None):
    out = df.copy()
    declare_frame_cadence(out, "4h", kind)
    out.attrs[FRAME_DECISION_TIME_ATTR] = {} if cutoff is None else {"cutoff_ms": cutoff, "boundary_ms": boundary}
    return out


def test_check_time_frames_hold_unless_the_last_bar_is_the_latest_closed_bar():
    base = _walk(900, 11)
    hist = _apply(base, DEFAULTS)
    for kind in (TIMESTAMP_KIND_OPEN, TIMESTAMP_KIND_NATIVE_CLOSE):
        with _case(kind):
            _check_time_frames_hold_unless_the_last_bar_is_the_latest_closed_bar(base, hist, kind)


def _check_time_frames_hold_unless_the_last_bar_is_the_latest_closed_bar(base, hist, kind):
    i = _first(hist, 1)
    df = base.iloc[:i + 1].copy()
    if kind == TIMESTAMP_KIND_NATIVE_CLOSE:
        df.index = df.index + pd.Timedelta(hours=4) - pd.Timedelta(milliseconds=1)
    last_ts = int(df.index[-1].value // 1_000_000)
    boundary = last_ts + FOUR_H if kind == TIMESTAMP_KIND_OPEN else last_ts + 1
    cases = [
        (boundary, boundary, 1, "entry_long"),
        (boundary + FOUR_H - 1, boundary, 1, "entry_long"),
        (boundary + FOUR_H, boundary, 0, "decision_bar_stale"),
        (boundary - 1, boundary, 0, "decision_bar_future"),
        (boundary + 5, boundary - FOUR_H, 0, "decision_boundary_mismatch"),
        (None, None, 0, "decision_time_missing"),
    ]
    for cutoff, declared, signal, reason in cases:
        out = _apply(_runtime(df, kind, cutoff, declared), DEFAULTS)
        assert int(out["signal"].iloc[-1]) == signal, (cutoff, declared)
        assert out["stc_reason"].iloc[-1] == reason
        assert out["stc_value"].iloc[-1] == hist["stc_value"].iloc[i]
        assert int(out["signal"].iloc[-2]) == int(hist["signal"].iloc[i - 1])


def _composer(df, params, side="", position_ctx=None, closes=("tiered_tp_pct",), open_name=NAME):
    reg = _reg()
    evaluation = evaluate_open_close(
        reg.apply_strategy, reg.get_strategy, df, open_name, open_name, list(closes), side, params=params,
        position_ctx=position_ctx, close_evaluate=close_evaluate,
        close_params_by_name={"tiered_tp_pct": {}} if "tiered_tp_pct" in closes else None)
    return evaluation, finalize_decision(evaluation, side)


def _entry_frame(side):
    base = _walk(900, 11)
    i = _first(_apply(base, DEFAULTS), 1 if side == "long" else -1)
    return base.iloc[:i + 1].copy()


def test_composer_emits_both_directions_and_holds_entries_while_a_position_is_open():
    _, flat_long = _composer(_entry_frame("long"), DEFAULTS)
    _, flat_short = _composer(_entry_frame("short"), DEFAULTS)
    assert (flat_long["open_action"], flat_long["signal"]) == ("long", 1)
    assert (flat_short["open_action"], flat_short["signal"]) == ("short", -1)
    for frame in (_entry_frame("long"), _entry_frame("short")):
        mark = float(frame["close"].iloc[-1])
        for side in ("long", "short"):
            ctx = {"side": side, "avg_cost": mark, "entry_atr": 50.0, "current_quantity": 1.0,
                   "initial_quantity": 1.0}
            _, held = _composer(frame, DEFAULTS, side, ctx)
            assert held["signal"] == 0 and held["close_fraction"] == 0.0


def test_close_fallback_on_the_candidate_closes_on_its_own_opposite_entry():
    frame = _entry_frame("short")
    mark = float(frame["close"].iloc[-1])
    ctx = {"side": "long", "avg_cost": mark, "entry_atr": 50.0, "current_quantity": 1.0, "initial_quantity": 1.0}
    _, decision = _composer(frame, DEFAULTS, "long", ctx, closes=(NAME,))
    assert decision["close_strategy"] == NAME and decision["close_fraction"] == 1.0 and decision["signal"] == -1


def test_invalid_entry_frame_keeps_the_close_owner_executable():
    for mutation in ("volume", "geometry", "cadence", "stale"):
        with _case(mutation):
            _invalid_entry_frame_keeps_the_close_owner_executable(mutation)


def _invalid_entry_frame_keeps_the_close_owner_executable(mutation):
    df = _walk(400, 9)
    if mutation == "volume":
        df = df.assign(volume=np.r_[df["volume"].to_numpy()[:-1], -5.0])
    elif mutation == "geometry":
        df = df.assign(open=np.r_[df["open"].to_numpy()[:-1], df["high"].iloc[-1] * 2])
    elif mutation == "cadence":
        df = _shift_index(df, len(df) - 1, pd.Timedelta(hours=2))
    else:
        last = int(df.index[-1].value // 1_000_000)
        df = _runtime(df, TIMESTAMP_KIND_OPEN, last + 3 * FOUR_H, last + FOUR_H)
    mark = float(df["close"].iloc[-1])
    ctx = {"side": "long", "avg_cost": mark / 1.05, "entry_atr": 1.0, "current_quantity": 1.0,
           "initial_quantity": 1.0}
    evaluation, decision = _composer(df, DEFAULTS, "long", ctx)
    assert evaluation.open_result_df["stc_valid"].iloc[-1] == 0.0
    assert decision["open_action"] == "none"
    assert decision["close_strategy"] == "tiered_tp_pct" and decision["close_fraction"] == 1.0
    assert decision["signal"] == -1
    _, decision = _composer(df, DEFAULTS, "short", dict(ctx, side="short", avg_cost=mark * 1.05))
    assert decision["open_action"] == "none" and decision["close_fraction"] == 1.0 and decision["signal"] == 1
    _, decision = _composer(df, DEFAULTS, "long", dict(ctx, avg_cost=mark / 1.015))
    assert decision["close_fraction"] == 0.25 and decision["signal"] == -1
    _, decision = _composer(df, DEFAULTS, "", None)
    assert decision["signal"] == 0 and decision["open_action"] == "none"


def _bt(params, **kw):
    base = dict(initial_capital=1000.0, platform="hyperliquid", open_strategy={"name": NAME, "params": params},
                close_strategies=CLOSE, stop_loss_atr_mult=1.0, direction="both")
    base.update(kw)
    return Backtester(**base)


def test_simulator_fills_next_open_with_explicit_exits_partials_no_reversal_or_scale_in_and_long_only_opens_no_shorts():
    df = _walk(1500, 5)
    sig = ensure_atr_indicator(_apply(df, DEFAULTS))
    with _case("test_simulator_fills_next_open_with_explicit_exits_partials_and_no_reversal_or_scale_in"):
        _simulator_fills_next_open_with_explicit_exits_partials_and_no_reversal_or_scale_in(df, sig)
    with _case("test_simulator_long_only_arm_never_opens_shorts"):
        _simulator_long_only_arm_never_opens_shorts(sig)


def _simulator_fills_next_open_with_explicit_exits_partials_and_no_reversal_or_scale_in(df, sig):
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


def _simulator_long_only_arm_never_opens_shorts(sig):
    res = _bt(DEFAULTS, direction="long").run(sig, strategy_name=NAME, symbol="BTC/USDT", timeframe="4h",
                                              save=False)
    assert res["trades"] and {t["side"] for t in res["trades"]} == {"long"}


def test_simulator_keeps_closing_and_stopping_after_entry_inputs_fail():
    df = _walk(1200, 5)
    sig = ensure_atr_indicator(_apply(df, DEFAULTS))
    first = next(int(i) for i in np.flatnonzero(sig["signal"].to_numpy() != 0)
                 if i > 250 and np.isfinite(sig["atr"].iloc[i]))
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
@pytest.mark.parametrize("params", DOMAIN_ENDPOINTS + [DEFAULTS])
def test_entry_parity_on_frozen_candles(window, params):
    frame = compute_parity_frame(_frozen("BTC 4h", 420), cfg=_cfg(params), window=window)
    result = summarize(frame)
    assert result["bars_compared"] > 0
    assert result["clean"], frame[~frame["match"]].head()


@pytest.mark.parametrize("window", [200, None])
def test_composed_parity_covers_both_sides_with_the_close_owner(window):
    params = dict(DEFAULTS, fast_period=12, slow_period=26, cycle_length=8)
    cfg = _cfg(params, close_refs=json.loads(json.dumps(CLOSE)), direction="both")
    frame = compute_parity_frame(_frozen("SOL 4h", 900), cfg=cfg, window=window)
    result = summarize(frame)
    assert result["mismatches"] == 0 and result["clean"], frame[~frame["match"]].head()
    assert (frame["live_open_action"] == "long").any() and (frame["live_open_action"] == "short").any()
    closing = frame[frame["live_close_fraction"] > 0]
    assert (closing["live_signal"] == -1).any() and (closing["live_signal"] == 1).any()


@pytest.mark.parametrize("window", [200, None])
def test_batched_explicit_paper_parity_matches_solo(window):
    params = dict(DEFAULTS, fast_period=12, slow_period=26, cycle_length=8)
    cfg = _cfg(params, close_refs=json.loads(json.dumps(CLOSE)), direction="both", batched=True)
    frame = compute_parity_frame(_frozen("BTC 4h", 480), cfg=cfg, window=window)
    result = summarize(frame)
    assert result["bars_compared"] > 0
    assert result["clean"] and result["batch_clean"], frame[~frame["match"]].head()
    assert (frame["live_open_action"] != "none").any()
