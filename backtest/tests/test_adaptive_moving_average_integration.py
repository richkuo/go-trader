import gzip
import csv
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

from atr import ensure_atr_indicator
from backtester import Backtester, validate_close_capabilities
from close_registry_loader import evaluate as close_evaluate
from closed_bar import FRAME_INTERVAL_ATTR, FRAME_TIMESTAMP_KIND_ATTR, declare_frame_cadence
from eval_windows import expand_sweep
from optimizer import DEFAULT_PARAM_RANGES
from parity_diff import ParityConfig, compute_parity_frame, summarize
from registry_loader import load_registry
from strategy_composition import evaluate_open_close, finalize_decision

NAME = "adaptive_moving_average"
STUDY = os.path.join(_REPO, "backtest", "candidates", "adaptive_moving_average_1641")
DATA = os.path.join(_REPO, "backtest", "candidates", "chande_momentum_1661", "data")
CLOSE = [{"name": "tiered_tp_pct", "params": {}}]
INPUT_COLUMNS = ["open", "high", "low", "close", "volume"]
SEED = {"efficiency_period": 10, "fast_period": 2, "slow_period": 30, "slope_span": 3, "recursion_length": 120}
SMALL = {"efficiency_period": 4, "fast_period": 2, "slow_period": 12, "slope_span": 2, "recursion_length": 20}
BOUNDARY = {"efficiency_period": 1, "fast_period": 2, "slow_period": 30, "slope_span": 1, "recursion_length": 197}
DECISION_COLUMNS = ["ama_value", "ama_prev_value", "ama_slope_ref", "ama_slope", "ama_efficiency",
                    "ama_smoothing", "ama_valid", "ama_reason_code", "signal"]


def _reg():
    return load_registry("futures")


def _apply(df, params=None):
    return _reg().apply_strategy(NAME, df, params)


def _need(p):
    return p["recursion_length"] + p["efficiency_period"] + max(p["slope_span"], 1)


def _declare(df, freq="4h", kind="open"):
    return declare_frame_cadence(df, freq, kind)


def _walk(n, seed, start=100.0, freq="4h"):
    rng = np.random.default_rng(seed)
    close = start * np.exp(np.cumsum(rng.normal(0.0, 0.02, n)))
    open_ = np.concatenate([[start], close[:-1]])
    high = np.maximum(open_, close) * (1.0 + rng.uniform(0.001, 0.01, n))
    low = np.minimum(open_, close) * (1.0 - rng.uniform(0.001, 0.01, n))
    volume = rng.uniform(100.0, 1000.0, n)
    df = pd.DataFrame({"open": open_, "high": high, "low": low, "close": close, "volume": volume})
    df.index = pd.date_range("2026-01-01", periods=n, freq=freq)
    return _declare(df, freq)


def _from_closes(closes, freq="4h"):
    c = np.asarray(closes, dtype=float)
    df = pd.DataFrame({"open": c, "high": c + 0.5, "low": c - 0.5, "close": c, "volume": np.full(len(c), 10.0)})
    df.index = pd.date_range("2026-01-01", periods=len(c), freq=freq)
    return _declare(df, freq)


def _frozen(symbol, tf, bars):
    with gzip.open(os.path.join(DATA, f"{symbol}_{tf}_candles.csv.gz"), "rt") as fh:
        rows = list(csv.reader(fh))[1:]
    df = pd.DataFrame(rows, columns=["timestamp"] + INPUT_COLUMNS).astype(
        {"timestamp": "int64", **{c: float for c in INPUT_COLUMNS}})
    df.index = pd.to_datetime(df["timestamp"], unit="ms")
    df = df[INPUT_COLUMNS].iloc[-bars:].copy()
    return _declare(df, tf)


def _ref_ama(c, t, p):
    e, length = p["efficiency_period"], p["recursion_length"]
    fast = 2.0 / (p["fast_period"] + 1.0)
    slow = 2.0 / (p["slow_period"] + 1.0)
    a = c[t - length]
    sc = None
    er = None
    for k in range(t - length + 1, t + 1):
        change = abs(c[k] - c[k - e])
        noise = abs(c[k - e + 1] - c[k - e])
        for i in range(k - e + 2, k + 1):
            noise = noise + abs(c[i] - c[i - 1])
        er = change / noise if noise > 0 else 0.0
        root = er * (fast - slow) + slow
        sc = root * root
        a = a + sc * (c[k] - a)
    return a, er, sc


def _ref_signal(c, t, p):
    s = p["slope_span"]
    a, _, _ = _ref_ama(c, t, p)
    b, _, _ = _ref_ama(c, t - 1, p)
    r, _, _ = _ref_ama(c, t - s, p)
    if c[t] > a and c[t - 1] <= b and a - r > 0:
        return 1, a, b, r
    if c[t] < a and c[t - 1] >= b and a - r < 0:
        return -1, a, b, r
    return 0, a, b, r


def test_registration_metadata_grid_and_hidden_discovery():
    fut = _reg()
    entry = fut.STRATEGY_REGISTRY[NAME]
    raw = fut._registry.STRATEGIES[NAME]
    with open(os.path.join(STUDY, "protocol.json")) as fh:
        proto = json.load(fh)
    assert entry["default_params"] == SEED == proto["seed_params"]
    assert entry["edge_status"] == "no_edge"
    assert entry["edge_source"] in ("unvalidated", "study_fail", "study_inconclusive")
    assert entry["edge_ref"] == "backtest/candidates/adaptive_moving_average_1641/REPORT.md"
    assert os.path.isfile(os.path.join(_REPO, entry["edge_ref"]))
    assert raw["short_entries"] is True and raw["platforms"] == ("futures",)
    assert raw["param_validator"] is not None
    assert NAME in fut._registry.short_entry_strategies()
    assert NAME in fut._registry.DISCOVERY_HIDDEN_STRATEGIES
    assert NAME not in fut.list_strategies()
    assert NAME not in load_registry("spot").STRATEGY_REGISTRY
    assert DEFAULT_PARAM_RANGES[NAME] == proto["grids"][NAME]
    combos = [p for _, p in expand_sweep(SEED, [(k, list(v)) for k, v in DEFAULT_PARAM_RANGES[NAME].items()])]
    assert len(combos) == 162
    assert max(_need(p) for p in combos) == 185 and _need(SEED) == 133
    for platform in ("spot", "futures"):
        out = subprocess.run([sys.executable, os.path.join(_REPO, "shared_strategies", "open", platform,
                                                          "strategies.py"), "--list-json"],
                             capture_output=True, text=True, cwd=_REPO, check=True).stdout
        for phase in ("before", "after"):
            with open(os.path.join(STUDY, f"discovery_{phase}_{platform}.json")) as fh:
                assert json.loads(fh.read()) == json.loads(out)
        assert NAME not in out


def test_values_match_an_independent_scalar_reference_and_both_entry_directions():
    df = _walk(420, 7)
    c = df["close"].tolist()
    seen = set()
    for p in (SMALL, dict(SMALL, slope_span=1, efficiency_period=1), dict(SMALL, fast_period=1, slow_period=2),
              dict(SEED, recursion_length=60)):
        out = _apply(df, p)
        need = _need(p)
        assert (out["ama_valid"].iloc[:need - 1] == 0.0).all()
        assert (out["ama_reason"].iloc[:need - 1] == "insufficient_history").all()
        assert out["ama_valid"].iloc[need - 1] == 1.0
        for t in range(need - 1, len(df)):
            sig, a, b, r = _ref_signal(c, t, p)
            _, er, sc = _ref_ama(c, t, p)
            row = out.iloc[t]
            assert (row["ama_value"], row["ama_prev_value"], row["ama_slope_ref"]) == (a, b, r)
            assert (row["ama_efficiency"], row["ama_smoothing"]) == (er, sc)
            assert row["ama_slope"] == a - r
            assert row["ama_seed_close"] == c[t - p["recursion_length"]]
            assert int(row["signal"]) == sig
            assert row["ama_reason_code"] == {1: 1.0, -1: -1.0}.get(sig, row["ama_reason_code"])
            if sig == 0:
                assert row["ama_reason"] in ("no_cross", "slope_disagrees")
            seen.add(sig)
    assert seen == {-1, 0, 1}


def test_previous_equality_qualifies_and_current_equality_or_flat_slope_does_not():
    p = SMALL
    flat = [100.0] * 80
    up = _apply(_from_closes(flat + [101.0]), p).iloc[-1]
    assert up["ama_prev_value"] == up["ama_prev_close"] == 100.0
    assert int(up["signal"]) == 1 and up["ama_reason"] == "entry_long"
    down = _apply(_from_closes(flat + [99.0]), p).iloc[-1]
    assert int(down["signal"]) == -1 and down["ama_reason"] == "entry_short"
    level = _apply(_from_closes(flat + [100.0]), p)
    assert (level["signal"] == 0).all()
    last = level.iloc[-1]
    assert last["ama_close"] == last["ama_value"] == 100.0 and last["ama_slope"] == 0.0
    assert last["ama_reason"] == "no_cross" and last["ama_efficiency"] == 0.0
    falling = [200.0 - i for i in range(80)]
    q = dict(p, slope_span=5)
    prev = float(_apply(_from_closes(falling), q)["ama_value"].iloc[-1])
    for closes, sign in ((falling + [prev + 0.5], 1), ([100.0 + i for i in range(80)], -1)):
        if sign == -1:
            rise_prev = float(_apply(_from_closes(closes), q)["ama_value"].iloc[-1])
            closes = closes + [rise_prev - 0.5]
        row = _apply(_from_closes(closes), q).iloc[-1]
        crossed = ((row["ama_close"] - row["ama_value"]) * sign > 0
                   and (row["ama_prev_close"] - row["ama_prev_value"]) * sign <= 0)
        assert crossed and row["ama_slope"] * sign < 0
        assert int(row["signal"]) == 0 and row["ama_reason"] == "slope_disagrees" and row["ama_reason_code"] == 2.0


def _holds_at_end(df, params, reason):
    before = df.copy()
    attrs = dict(df.attrs)
    out = _apply(df, params)
    pd.testing.assert_frame_equal(df, before)
    assert df.attrs == attrs
    for col in df.columns:
        pd.testing.assert_series_equal(out[col], before[col])
    assert int(out["signal"].iloc[-1]) == 0
    assert out["ama_valid"].iloc[-1] == 0.0
    assert out["ama_reason"].iloc[-1].split(":")[0] == reason, out["ama_reason"].iloc[-1]
    assert out["ama_reason_code"].iloc[-1] >= 10.0
    return out


def _set_last(df, col, value):
    out = df.copy()
    out.iloc[-1, out.columns.get_loc(col)] = value
    return out


def _with_index(df, idx):
    out = df.set_axis(pd.DatetimeIndex(idx))
    out.attrs = dict(df.attrs)
    return out


@pytest.mark.parametrize("mutate,reason", [
    (lambda df: df.drop(columns=["volume"]), "missing_columns"),
    (lambda df: df.drop(columns=["open", "high"]), "missing_columns"),
    (lambda df: df.reset_index(drop=True), "missing_timestamps"),
    (lambda df: _with_index(df, list(df.index[:-1]) + [pd.NaT]), "missing_timestamps"),
    (lambda df: _set_last(df, "high", np.nan), "nonfinite_input"),
    (lambda df: _set_last(df, "volume", np.inf), "nonfinite_input"),
    (lambda df: _set_last(df.astype({"close": object}), "close", "x"), "nonfinite_input"),
    (lambda df: _set_last(df, "low", 0.0), "nonpositive_price"),
    (lambda df: _set_last(df, "open", df["high"].iloc[-1] * 2), "invalid_candle_geometry"),
    (lambda df: _set_last(df, "close", df["low"].iloc[-1] / 2), "invalid_candle_geometry"),
    (lambda df: _set_last(df, "high", df["low"].iloc[-1] / 2), "invalid_candle_geometry"),
    (lambda df: _set_last(df, "volume", -1.0), "invalid_volume"),
    (lambda df: _with_index(df, list(df.index[:-1]) + [df.index[-2]]), "timestamp_order"),
    (lambda df: _with_index(df, list(df.index[:-1]) + [df.index[-3]]), "timestamp_order"),
    (lambda df: _with_index(df, list(df.index[:-1]) + [df.index[-1] + pd.Timedelta(hours=4)]), "cadence_gap"),
    (lambda df: df.drop(df.index[-20]), "cadence_gap"),
    (lambda df: df.iloc[-(_need(SMALL) - 1):], "insufficient_history"),
])
def test_invalid_market_input_is_a_reasoned_hold_that_preserves_the_frame(mutate, reason):
    _holds_at_end(mutate(_walk(120, 3)), SMALL, reason)


def test_missing_or_wrong_cadence_holds_every_row():
    df = _walk(120, 3)
    df.attrs.pop(FRAME_INTERVAL_ATTR)
    out = _apply(df, SMALL)
    assert (out["ama_reason"] == "missing_cadence").all() and (out["signal"] == 0).all()
    wrong = _walk(120, 3)
    wrong.attrs[FRAME_INTERVAL_ATTR] = 3_600_000
    out = _apply(wrong, SMALL)
    assert (out["signal"] == 0).all()
    assert set(out["ama_reason"].iloc[_need(SMALL) - 1:]) == {"cadence_gap"}


def test_overflowing_prices_hold_as_nonfinite_derived():
    closes = [1e308 if i % 2 == 0 else 1.7e308 for i in range(120)]
    df = _from_closes(closes)
    df["high"] = df["close"]
    df["low"] = df["close"]
    out = _holds_at_end(df, SMALL, "nonfinite_derived")
    assert (out["signal"] == 0).all()


def test_constant_prices_never_enter_and_report_finite_values():
    out = _apply(_from_closes([250.0] * 200), SEED)
    assert (out["signal"] == 0).all()
    valid = out[out["ama_valid"] == 1.0]
    assert len(valid) == 200 - _need(SEED) + 1
    assert (valid["ama_reason"] == "no_cross").all()
    assert (valid["ama_value"] == 250.0).all() and (valid["ama_efficiency"] == 0.0).all()


def test_a_defect_holds_only_inside_its_dependency_window():
    df = _walk(300, 5)
    need = _need(SMALL)
    bad = 150
    corrupt = df.copy()
    corrupt.iloc[bad, corrupt.columns.get_loc("volume")] = np.nan
    out = _apply(corrupt, SMALL)
    clean = _apply(df, SMALL)
    assert (out["ama_reason"].iloc[bad:bad + need] == "nonfinite_input").all()
    assert (out["signal"].iloc[bad:bad + need] == 0).all()
    pd.testing.assert_frame_equal(out.iloc[:bad], clean.iloc[:bad])
    pd.testing.assert_frame_equal(out.iloc[bad + need:], clean.iloc[bad + need:])
    gapped = df.drop(df.index[bad])
    out = _apply(gapped, SMALL)
    assert (out["ama_reason"].iloc[bad:bad + need - 1] == "cadence_gap").all()
    assert out["ama_valid"].iloc[bad + need - 1] == 1.0


@pytest.mark.parametrize("params", [
    {"efficiency_period": 0}, {"efficiency_period": 65}, {"efficiency_period": 9.5}, {"efficiency_period": True},
    {"efficiency_period": float("nan")}, {"efficiency_period": "10"}, {"fast_period": 0}, {"fast_period": 30},
    {"fast_period": 31}, {"slow_period": 1}, {"slow_period": 257}, {"slow_period": float("inf")},
    {"slope_span": 0}, {"slope_span": False}, {"recursion_length": 0}, {"recursion_length": 198},
    {"recursion_length": 120.5}, {"recursion_length": 197, "efficiency_period": 1, "slope_span": 2},
    {"recursion_length": 160, "efficiency_period": 30, "slope_span": 10},
])
def test_invalid_parameters_fail_validation(params):
    p = {**SEED, **params}
    reg = _reg()
    with pytest.raises(ValueError):
        reg._registry.validate_params(NAME, p)
    with pytest.raises(ValueError):
        _apply(_walk(120, 1), p)
    from adaptive_moving_average import adaptive_moving_average_core
    with pytest.raises(ValueError):
        adaptive_moving_average_core(_walk(120, 1), **p)


def test_boundary_history_of_199_rows_and_integral_floats():
    df = _walk(260, 2)
    out = _apply(df, BOUNDARY)
    assert _need(BOUNDARY) == 199
    assert out["ama_valid"].iloc[197] == 0.0 and out["ama_reason"].iloc[197] == "insufficient_history"
    assert out["ama_valid"].iloc[198] == 1.0
    exact = _apply(df.iloc[:199], BOUNDARY)[DECISION_COLUMNS].iloc[-1]
    pd.testing.assert_series_equal(exact, out[DECISION_COLUMNS].iloc[198], check_names=False)
    floats = {k: float(v) for k, v in SEED.items()}
    pd.testing.assert_frame_equal(_apply(df, floats), _apply(df, SEED))


def test_timestamp_column_and_native_close_frames_decide_the_same_way():
    df = _walk(260, 3)
    plain = df.reset_index(drop=True)
    plain["timestamp"] = df.index.as_unit("ms").asi8.astype("int64")
    plain.attrs = dict(df.attrs)
    a = _apply(df, SMALL)
    b = _apply(plain, SMALL)
    np.testing.assert_array_equal(a["signal"].to_numpy(), b["signal"].to_numpy())
    np.testing.assert_array_equal(a["ama_value"].to_numpy(), b["ama_value"].to_numpy())
    native = df.set_axis(df.index + pd.Timedelta(hours=4) - pd.Timedelta(milliseconds=1))
    _declare(native, "4h", "native_close_inclusive")
    c = _apply(native, SMALL)
    np.testing.assert_array_equal(a["signal"].to_numpy(), c["signal"].to_numpy())
    np.testing.assert_array_equal(a["ama_eval_ts_ms"].to_numpy(), c["ama_eval_ts_ms"].to_numpy())
    assert a["ama_eval_ts_ms"].iloc[-1] == df.index[-1].value // 1_000_000
    native.attrs.pop(FRAME_TIMESTAMP_KIND_ATTR)
    d = _apply(native, SMALL)
    np.testing.assert_array_equal(a["signal"].to_numpy(), d["signal"].to_numpy())
    assert d["ama_eval_ts_ms"].isna().all()


def _grid_edges():
    return [SEED, BOUNDARY, {"efficiency_period": 20, "fast_period": 3, "slow_period": 50, "slope_span": 5,
                             "recursion_length": 160}, dict(SEED, slope_span=1)]


def test_full_series_every_prefix_and_every_200_row_frame_agree_on_frozen_candles():
    df = _frozen("BTC", "1h", 520)
    for p in _grid_edges():
        need = _need(p)
        full = _apply(df, p)
        assert full["ama_valid"].iloc[need - 1:].all()
        for end in range(need, len(df) + 1):
            prefix = _apply(df.iloc[:end], p)[DECISION_COLUMNS].iloc[-1]
            pd.testing.assert_series_equal(prefix, full[DECISION_COLUMNS].iloc[end - 1], check_names=False)
            if end >= 200:
                tail = _apply(df.iloc[end - 200:end], p)[DECISION_COLUMNS].iloc[-1]
                pd.testing.assert_series_equal(tail, full[DECISION_COLUMNS].iloc[end - 1], check_names=False)
        span = _apply(df.iloc[len(df) - need:], p)[DECISION_COLUMNS].iloc[-1]
        pd.testing.assert_series_equal(span, full[DECISION_COLUMNS].iloc[-1], check_names=False)
        short = _apply(df.iloc[len(df) - need + 1:], p)
        assert short["ama_reason"].iloc[-1] == "insufficient_history"
        assert {-1, 1} <= set(full["signal"].unique()) or p is BOUNDARY


def test_rows_outside_the_dependency_window_cannot_change_a_decision():
    df = _frozen("ETH", "4h", 400)
    p = SEED
    need = _need(p)
    t = 380
    full = _apply(df, p)
    early = df.copy()
    early.iloc[: t - need + 1, early.columns.get_loc("close")] *= 1.5
    early.iloc[: t - need + 1, early.columns.get_loc("high")] *= 1.5
    future = df.copy()
    future.iloc[t + 1:, future.columns.get_loc("volume")] = np.nan
    for mutated in (early, future):
        got = _apply(mutated, p)
        pd.testing.assert_series_equal(got[DECISION_COLUMNS].iloc[t], full[DECISION_COLUMNS].iloc[t],
                                       check_names=False)


def _composer(df, params, side="", position_ctx=None):
    reg = _reg()
    evaluation = evaluate_open_close(
        reg.apply_strategy, reg.get_strategy, df, NAME, NAME, ["tiered_tp_pct"], side, params=params,
        position_ctx=position_ctx, close_evaluate=close_evaluate, close_params_by_name={"tiered_tp_pct": {}})
    return evaluation, finalize_decision(evaluation, side)


def test_composer_emits_both_directions_and_holds_entries_while_a_position_is_open():
    flat = [100.0] * 80
    long_df = _from_closes(flat + [101.0])
    short_df = _from_closes(flat + [99.0])
    _, d = _composer(long_df, SMALL)
    assert (d["open_action"], d["signal"]) == ("long", 1)
    _, d = _composer(short_df, SMALL)
    assert (d["open_action"], d["signal"]) == ("short", -1)
    for df in (long_df, short_df):
        mark = float(df["close"].iloc[-1])
        for side in ("long", "short"):
            ctx = {"side": side, "avg_cost": mark, "entry_atr": 50.0, "current_quantity": 1.0, "initial_quantity": 1.0}
            _, held = _composer(df, SMALL, side, ctx)
            assert held["signal"] == 0 and held["close_fraction"] == 0.0


@pytest.mark.parametrize("mutation", ["volume", "geometry", "cadence", "insufficient"])
def test_entry_data_hold_keeps_the_close_owner_executable(mutation):
    df = _walk(200, 9)
    if mutation == "volume":
        df = _set_last(df, "volume", -5.0)
    elif mutation == "geometry":
        df = _set_last(df, "open", df["high"].iloc[-1] * 2)
    elif mutation == "cadence":
        df = _with_index(df, list(df.index[:-1]) + [df.index[-1] + pd.Timedelta(hours=2)])
    else:
        df = df.iloc[-(_need(SMALL) - 1):]
    mark = float(df["close"].iloc[-1])
    ctx = {"side": "long", "avg_cost": mark / 1.05, "entry_atr": 1.0, "current_quantity": 1.0,
           "initial_quantity": 1.0}
    evaluation, decision = _composer(df, SMALL, "long", ctx)
    assert evaluation.open_result_df["ama_valid"].iloc[-1] == 0.0
    assert decision["open_action"] == "none"
    assert decision["close_strategy"] == "tiered_tp_pct" and decision["close_fraction"] == 1.0
    assert decision["signal"] == -1
    _, decision = _composer(df, SMALL, "short", dict(ctx, side="short", avg_cost=mark * 1.05))
    assert decision["open_action"] == "none" and decision["close_fraction"] == 1.0 and decision["signal"] == 1
    _, decision = _composer(df, SMALL, "long", dict(ctx, avg_cost=mark / 1.015))
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
    sig = ensure_atr_indicator(_apply(df, SMALL))
    bt = _bt(SMALL)
    res = bt.run(sig, strategy_name=NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    trades = res["trades"]
    assert {t["side"] for t in trades} == {"long", "short"}
    reasons = {t["exit_reason"].split(":")[0] for t in trades}
    assert reasons <= {"sl", "tiered_tp_pct", "end_of_data"} and {"sl", "tiered_tp_pct"} <= reasons
    by_entry = {}
    for t in trades:
        by_entry.setdefault((t["entry_date"], t["side"]), []).append(t)
    spans = []
    for (entry_date, side), legs in by_entry.items():
        pos = sig.index.get_loc(pd.Timestamp(entry_date))
        want = 1 if side == "long" else -1
        assert int(sig["signal"].iloc[pos - 1]) == want
        opened = float(df["open"].iloc[pos])
        assert legs[0]["entry_price"] == pytest.approx(opened * (1 + want * bt.slippage_pct), rel=1e-12)
        spans.append((pd.Timestamp(entry_date), max(pd.Timestamp(l["exit_date"]) for l in legs)))
    spans.sort()
    for (_, prev_exit), (next_entry, _) in zip(spans, spans[1:]):
        assert next_entry >= prev_exit
    assert any(len(legs) > 1 for legs in by_entry.values())
    skipped = [i for i in np.flatnonzero(sig["signal"].to_numpy() != 0)
               if not any(pd.Timestamp(e) == sig.index[min(i + 1, len(sig) - 1)] for e, _ in by_entry)]
    assert skipped


def test_simulator_keeps_closing_and_stopping_after_entry_inputs_fail():
    df = _walk(600, 5)
    sig = ensure_atr_indicator(_apply(df, SMALL))
    first = next(int(i) for i in np.flatnonzero(sig["signal"].to_numpy() != 0)
                 if i > 100 and np.isfinite(sig["atr"].iloc[i]))
    corrupt = df.copy()
    corrupt.iloc[first + 1:, corrupt.columns.get_loc("volume")] = np.nan
    bad = ensure_atr_indicator(_apply(corrupt, SMALL))
    assert (bad["signal"].iloc[:first + 1] == sig["signal"].iloc[:first + 1]).all()
    assert (bad["signal"].iloc[first + 1:] == 0).all()
    res = _bt(SMALL).run(bad, strategy_name=NAME, symbol="BTC/USDT", timeframe="4h", save=False)
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
@pytest.mark.parametrize("params", [SEED, BOUNDARY])
def test_raw_entry_parity_on_frozen_candles(window, params):
    frame = compute_parity_frame(_frozen("BTC", "4h", 420), cfg=_cfg(params), window=window)
    result = summarize(frame)
    assert result["bars_compared"] > 0
    assert result["clean"], frame[~frame["match"]].head()


@pytest.mark.parametrize("window", [200, None])
def test_composed_parity_covers_flat_long_and_short_contexts(window):
    cfg = _cfg(SMALL, close_refs=json.loads(json.dumps(CLOSE)), direction="both")
    frame = compute_parity_frame(_frozen("SOL", "4h", 520), cfg=cfg, window=window)
    result = summarize(frame)
    assert result["mismatches"] == 0 and result["clean"], frame[~frame["match"]].head()
    assert (frame["live_open_action"] == "long").any() and (frame["live_open_action"] == "short").any()
    closing = frame[frame["live_close_fraction"] > 0]
    assert (closing["live_signal"] == -1).any() and (closing["live_signal"] == 1).any()


@pytest.mark.parametrize("window", [200, None])
def test_batched_explicit_paper_parity_matches_solo(window):
    cfg = _cfg(SMALL, close_refs=json.loads(json.dumps(CLOSE)), direction="both", batched=True)
    frame = compute_parity_frame(_frozen("BTC", "4h", 320), cfg=cfg, window=window)
    result = summarize(frame)
    assert result["bars_compared"] > 0
    assert result["clean"] and result["batch_clean"], frame[~frame["match"]].head()


_CACHE_SCRIPT = r"""
import json, sys
sys.path.insert(0, sys.argv[2])
import run_study as rs
proto = rs.load_protocol()
_, source = rs.verify_frozen(proto)
drop = json.loads(sys.argv[3])
rs.build_cache(sys.argv[1], proto, source, drop_funding={k: tuple(v) for k, v in drop.items()} or None)
rs.forbid_network()
import pandas as pd
import eval_windows as ew
from data_fetcher import load_cached_data
from registry_loader import load_registry
reg = load_registry("futures")
case = sys.argv[4]
out = {}
if case == "warmup":
    window = ("2025-03-01", "2025-05-01")
    params = dict(proto["seed_params"])
    close = proto["close_strategies"]
    warm = ew.run_leg(reg, "adaptive_moving_average", params, "BTC/USDT", "4h", window, close_strategies=close,
                      direction="both", stop_loss_atr_mult=1.0, warmup_bars=300, keep_positions=True)
    cold = ew.run_leg(reg, "adaptive_moving_average", params, "BTC/USDT", "4h", window, close_strategies=close,
                      direction="both", stop_loss_atr_mult=1.0, keep_positions=True)
    full = load_cached_data("BTC/USDT", "4h", start_date="2024-06-01", end_date=window[1])
    full = full[full.index < pd.Timestamp(window[1])]
    sig = reg.apply_strategy("adaptive_moving_average", full, params)
    inwin = sig[sig.index >= pd.Timestamp(window[0])]
    out["warm"] = {k: warm[k] for k in ("warmup", "trades", "funding_incomplete") if k in warm}
    out["warm_entries"] = [p["entry_date"] for p in warm["positions"]]
    out["cold_entries"] = [p["entry_date"] for p in cold["positions"]]
    out["full_signal_bars"] = [str(t) for t in inwin.index[inwin["signal"].to_numpy() != 0]]
    out["first_window_bar"] = str(inwin.index[0])
    out["warm_funding_expected_hours"] = warm["funding"]["coverage"]["expected_hours"]
    out["cold_funding_expected_hours"] = cold["funding"]["coverage"]["expected_hours"]
    errors = {}
    for label, kw in (("gap", dict(window=("2023-02-07", "2023-03-01"), timeframe="1h")),
                      ("manifest", dict(window=window, timeframe="4h", manifest_ctx={"manifest": {}, "window": "x"})),
                      ("bool", dict(window=window, timeframe="4h", warmup=True)),
                      ("negative", dict(window=window, timeframe="4h", warmup=-1)),
                      ("open_start", dict(window=(None, "2025-05-01"), timeframe="4h"))):
        try:
            ew.run_leg(reg, "adaptive_moving_average", params, "BTC/USDT", kw["timeframe"], kw["window"],
                       close_strategies=close, direction="both", stop_loss_atr_mult=1.0,
                       warmup_bars=kw.get("warmup", 300), manifest_ctx=kw.get("manifest_ctx"))
            errors[label] = None
        except ValueError as exc:
            errors[label] = str(exc)
    try:
        ew.run_leg(reg, "funding_skew", None, "BTC/USDT", "4h", window, warmup_bars=300)
        errors["funding_input"] = None
    except ValueError as exc:
        errors["funding_input"] = str(exc)
    out["errors"] = errors
else:
    proto["symbols"] = ["BTC/USDT"]
    proto["windows"]["probe"] = ["2025-03-01", "2025-04-01"]
    proto["held_out_windows"] = ["probe"]
    runner = rs.Runner(proto)
    sel = {"ema_crossover": {"fast_period": 12, "slow_period": 26},
           "vol_momentum": {"mom_window": 24, "entry_threshold": 0.3, "eff_entry": 0.35}}
    held = {"probe": runner.evaluate("adaptive_moving_average", proto["seed_params"], "1h", "probe")}
    stress = {"probe": runner.evaluate("adaptive_moving_average", proto["seed_params"], "1h", "probe",
                                       slippage_mult=2.0)}
    comps = {c: {"probe": runner.evaluate(c, sel[c], "1h", "probe")} for c in proto["comparators"]}
    out["verdict"] = rs.group_verdict(proto, held, stress, comps)
    out["legs"] = {"candidate": held["probe"]["legs"], "comparators": {c: comps[c]["probe"]["legs"] for c in comps},
                   "bars": held["probe"]["bars"]}
print(json.dumps(out, default=str))
"""


def _cache_case(tmp_path, case, drop=None):
    proc = subprocess.run(
        [sys.executable, "-c", _CACHE_SCRIPT, str(tmp_path / "cache.db"), STUDY, json.dumps(drop or {}), case],
        capture_output=True, text=True, cwd=os.path.join(_REPO, "backtest"), timeout=1800,
        env={**os.environ, "PYTHONPATH": os.pathsep.join([os.path.join(_REPO, "backtest"), _TOOLS])})
    assert proc.returncode == 0, proc.stderr[-4000:]
    return json.loads(proc.stdout.strip().splitlines()[-1])


def test_cached_m1_warmup_scores_only_the_window_with_warmed_indicators(tmp_path):
    out = _cache_case(tmp_path, "warmup")
    warm = out["warm"]
    assert warm["warmup"] == {"bars": 300, "first_bar": "2025-01-10 00:00:00", "scored_first_bar": "2025-03-01 00:00:00"}
    assert out["first_window_bar"] == "2025-03-01 00:00:00"
    assert min(out["warm_entries"]) >= "2025-03-01"
    expected = [str(pd.Timestamp(b) + pd.Timedelta(hours=4)) for b in out["full_signal_bars"]]
    assert out["warm_entries"] and set(out["warm_entries"]) <= set(expected)
    assert min(out["cold_entries"]) > min(out["warm_entries"])
    assert out["warm_funding_expected_hours"] == out["cold_funding_expected_hours"]
    errors = out["errors"]
    assert "warmup needs the 300 consecutive bars" in errors["gap"] and "missing" in errors["gap"]
    assert "cached path only" in errors["manifest"]
    assert "non-negative integer" in errors["bool"] and "non-negative integer" in errors["negative"]
    assert "explicit start" in errors["open_start"]
    assert "reads funding as an entry input" in errors["funding_input"]


def test_missing_held_hour_funding_makes_every_arm_unscorable_and_the_verdict_inconclusive(tmp_path):
    lo = int(pd.Timestamp("2025-03-01").timestamp() * 1000)
    hi = int(pd.Timestamp("2025-04-01").timestamp() * 1000)
    out = _cache_case(tmp_path, "funding", {"BTC": [lo, hi]})
    verdict = out["verdict"]
    assert verdict["verdict"] == "inconclusive"
    bad = verdict["checks"]["unscorable_windows"]["probe"]
    for label in ("candidate BTC/USDT 1h", "candidate x2 slippage BTC/USDT 1h", "ema_crossover BTC/USDT 1h",
                  "vol_momentum BTC/USDT 1h", "incumbent median BTC/USDT 1h"):
        assert label in bad, bad
    assert verdict["checks"]["scorable_windows"] == []
    assert out["legs"]["candidate"]["BTC/USDT 1h"]["funding"]["unpriced_held_hours"] > 0
    funding = out["legs"]["candidate"]["BTC/USDT 1h"]["funding"]
    assert funding["priced_hours"] == funding["present_hours"] - funding["venue_absent_hours"]
    assert 0 <= funding["priced_hours"] < funding["expected_hours"]
    assert funding["coverage_complete"] is False
