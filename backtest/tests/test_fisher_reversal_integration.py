import json
import math
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
from backtester import Backtester, validate_close_capabilities
from close_registry_loader import evaluate as close_evaluate
from eval_windows import expand_sweep
from optimizer import DEFAULT_PARAM_RANGES
from parity_diff import ParityConfig, compute_parity_frame, summarize
from registry_loader import load_registry
from strategy_composition import evaluate_open_close, finalize_decision

NAME = "fisher_reversal"
MANIFEST = os.path.join(_REPO, "backtest", "candidates", "fisher_reversal_1646_manifest.json")
SEED = os.path.join(_REPO, "backtest", "candidates", "fisher_reversal_1646", "candidate_seed.json")
CLOSE = [{"name": "tiered_tp_pct", "params": {}}]
INPUT_COLUMNS = ["open", "high", "low", "close", "volume"]
HORIZON = 64
DOMAIN_ENDPOINTS = [
    {"lookback": 2, "extreme": 0.5},
    {"lookback": 2, "extreme": 3.0},
    {"lookback": 32, "extreme": 0.5},
    {"lookback": 32, "extreme": 3.0},
]


def _reg():
    return load_registry("futures")


def _apply(df, params=None):
    return _reg().apply_strategy(NAME, df, params)


def _walk(n, seed, start=100.0, freq="4h"):
    rng = np.random.default_rng(seed)
    close = start * np.exp(np.cumsum(rng.normal(0.0, 0.02, n)))
    open_ = np.concatenate([[start], close[:-1]])
    high = np.maximum(open_, close) * (1.0 + rng.uniform(0.001, 0.01, n))
    low = np.minimum(open_, close) * (1.0 - rng.uniform(0.001, 0.01, n))
    volume = rng.uniform(100.0, 1000.0, n)
    df = pd.DataFrame({"open": open_, "high": high, "low": low, "close": close, "volume": volume})
    df.index = pd.date_range("2026-01-01", periods=n, freq=freq)
    return df


def _from_mids(mids):
    mids = np.asarray(mids, dtype=float)
    df = pd.DataFrame({"open": mids, "high": mids + 0.5, "low": mids - 0.5, "close": mids,
                       "volume": np.full(len(mids), 10.0)})
    df.index = pd.date_range("2026-01-01", periods=len(mids), freq="4h")
    return df


def _ref_fisher(mid, t, lookback):
    x = 0.0
    f = 0.0
    for k in range(t - HORIZON + 1, t + 1):
        window = mid[k - lookback + 1:k + 1]
        lo, hi = min(window), max(window)
        n = 2 * (mid[k] - lo) / (hi - lo) - 1
        x = 0.33 * n + 0.67 * x
        x = min(max(x, -1 + 1e-6), 1 - 1e-6)
        f = 0.5 * math.log((1 + x) / (1 - x)) + 0.5 * f
    return f


def _ref_signal(a, b, c, extreme):
    if a > b and b <= c and b < -extreme:
        return 1
    if a < b and b >= c and b > extreme:
        return -1
    return 0


def test_registration_is_futures_only_bidirectional_no_edge_hidden_and_seeded():
    fut = _reg()
    entry = fut.STRATEGY_REGISTRY[NAME]
    raw = fut._registry.STRATEGIES[NAME]
    with open(SEED) as fh:
        seed = json.load(fh)
    assert entry["default_params"] == seed["params"] == {"lookback": 9, "extreme": 1.5}
    assert entry["edge_status"] == "no_edge"
    assert entry["edge_source"] in ("unvalidated", "study_fail", "study_inconclusive")
    assert entry["edge_ref"] == "backtest/candidates/fisher_reversal_1646/REPORT.md"
    assert os.path.isfile(os.path.join(_REPO, entry["edge_ref"]))
    assert raw["short_entries"] is True and raw["platforms"] == ("futures",)
    assert NAME in fut._registry.short_entry_strategies()
    assert NAME in fut._registry.DISCOVERY_HIDDEN_STRATEGIES
    assert NAME not in fut.list_strategies()
    assert NAME not in load_registry("spot").STRATEGY_REGISTRY
    assert DEFAULT_PARAM_RANGES[NAME] == seed["optimizer_grid"]


def test_values_match_an_independent_reference_and_the_truth_table():
    df = _walk(420, 7)
    mid = ((df["high"] + df["low"]) / 2.0).tolist()
    for params in [{"lookback": 9, "extreme": 0.5}, {"lookback": 2, "extreme": 0.75}, {"lookback": 32, "extreme": 1.0}]:
        out = _apply(df, params)
        need = params["lookback"] + HORIZON + 1
        assert (out["fisher_valid"].iloc[:need - 1] == 0.0).all()
        assert (out["fisher_reason"].iloc[:need - 1] == "insufficient_history").all()
        assert out["fisher_valid"].iloc[need - 1] == 1.0
        seen = set()
        for t in range(need - 1, len(df)):
            a = _ref_fisher(mid, t, params["lookback"])
            b = _ref_fisher(mid, t - 1, params["lookback"])
            c = _ref_fisher(mid, t - 2, params["lookback"])
            row = out.iloc[t]
            assert (row["fisher_value"], row["fisher_trigger"], row["fisher_prev_trigger"]) == (a, b, c)
            sig = _ref_signal(a, b, c, params["extreme"])
            assert int(row["signal"]) == sig
            assert row["fisher_reason_code"] == {1: 1.0, -1: -1.0, 0: 0.0}[sig]
            seen.add(sig)
        assert seen == {-1, 0, 1}


def test_equal_previous_comparison_qualifies_and_equal_current_does_not():
    falling = list(np.linspace(300.0, 100.0, 150))
    df = _from_mids(falling + [falling[-1] + 30.0])
    out = _apply(df, {"lookback": 9, "extreme": 0.5})
    last, prev = out.iloc[-1], out.iloc[-2]
    assert last["fisher_trigger"] == last["fisher_prev_trigger"] < -0.5
    assert last["fisher_value"] > last["fisher_trigger"]
    assert int(last["signal"]) == 1 and last["fisher_reason"] == "entry_long"
    assert prev["fisher_value"] == prev["fisher_trigger"] == prev["fisher_prev_trigger"]
    assert int(prev["signal"]) == 0 and prev["fisher_reason"] == "no_cross"

    rising = list(np.linspace(100.0, 300.0, 150))
    out = _apply(_from_mids(rising + [rising[-1] - 30.0]), {"lookback": 9, "extreme": 0.5})
    last = out.iloc[-1]
    assert last["fisher_trigger"] == last["fisher_prev_trigger"] > 0.5
    assert int(last["signal"]) == -1 and last["fisher_reason"] == "entry_short"


@pytest.mark.parametrize("side", [1, -1])
def test_trigger_exactly_at_the_extreme_is_not_an_entry(side):
    df = _walk(600, 11)
    out = _apply(df, {"lookback": 9, "extreme": 0.5})
    rows = [i for i in np.flatnonzero(out["signal"].to_numpy() == side)
            if 0.5 < abs(out["fisher_trigger"].iloc[i]) < 3.0]
    assert rows
    i = int(rows[0])
    edge = abs(float(out["fisher_trigger"].iloc[i]))
    at = _apply(df, {"lookback": 9, "extreme": edge})
    inside = _apply(df, {"lookback": 9, "extreme": float(np.nextafter(edge, 0.0))})
    assert int(at["signal"].iloc[i]) == 0 and at["fisher_reason"].iloc[i] == "no_cross"
    assert int(inside["signal"].iloc[i]) == side


def _holds_at_end(df, params, reason):
    before = df.copy()
    out = _apply(df, params)
    pd.testing.assert_frame_equal(df, before)
    for col in df.columns:
        pd.testing.assert_series_equal(out[col], before[col])
    assert int(out["signal"].iloc[-1]) == 0
    assert out["fisher_valid"].iloc[-1] == 0.0
    assert out["fisher_reason"].iloc[-1] == reason, out["fisher_reason"].iloc[-1]
    numeric = out.drop(columns=["fisher_reason"]).filter(like="fisher_")
    assert np.isfinite(numeric.to_numpy(dtype=float)).all()
    return out


P = {"lookback": 5, "extreme": 0.5}


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
    (lambda df: df.assign(volume=np.r_[df["volume"].to_numpy()[:-1], np.inf]), "nonfinite_input"),
    (lambda df: df.assign(low=np.r_[df["low"].to_numpy()[:-1], 0.0]), "nonpositive_price"),
    (lambda df: df.assign(open=np.r_[df["open"].to_numpy()[:-1], df["high"].iloc[-1] * 2]), "invalid_candle_geometry"),
    (lambda df: df.assign(close=np.r_[df["close"].to_numpy()[:-1], df["low"].iloc[-1] / 2]), "invalid_candle_geometry"),
    (lambda df: df.assign(high=np.r_[df["high"].to_numpy()[:-1], df["low"].iloc[-1] / 2]), "invalid_candle_geometry"),
    (lambda df: df.assign(volume=np.r_[df["volume"].to_numpy()[:-1], -1.0]), "negative_volume"),
    (lambda df: df.set_axis(pd.DatetimeIndex(list(df.index[:-1]) + [df.index[-2]])), "timestamp_order"),
    (lambda df: df.set_axis(pd.DatetimeIndex(list(df.index[:-1]) + [df.index[-3]])), "timestamp_order"),
    (lambda df: _shift_index(df, len(df) - 1, pd.Timedelta(hours=4)), "irregular_cadence"),
    (lambda df: _shift_index(df, len(df) - 30, pd.Timedelta(hours=1)), "irregular_cadence"),
    (lambda df: df.assign(open=100.0, high=101.0, low=99.0, close=100.0), "flat_range"),
    (lambda df: df.iloc[-(P["lookback"] + HORIZON):], "insufficient_history"),
])
def test_invalid_entry_data_is_a_reasoned_numeric_hold_that_preserves_the_frame(mutate, reason):
    _holds_at_end(mutate(_walk(160, 3)), P, reason)


def test_timestamp_column_is_accepted_without_a_datetime_index():
    df = _walk(160, 3)
    plain = df.reset_index(drop=True)
    plain["timestamp"] = (df.index.asi8 // 1_000_000).astype("int64")
    a = _apply(df, P)
    b = _apply(plain, P)
    np.testing.assert_array_equal(a["fisher_value"].to_numpy(), b["fisher_value"].to_numpy())
    np.testing.assert_array_equal(a["signal"].to_numpy(), b["signal"].to_numpy())


def test_a_defect_holds_only_inside_its_span_and_recovery_matches_a_clean_frame():
    df = _walk(400, 5)
    bad = 200
    need = P["lookback"] + HORIZON + 1
    corrupt = df.copy()
    corrupt.iloc[bad, corrupt.columns.get_loc("volume")] = np.nan
    out = _apply(corrupt, P)
    clean = _apply(df, P)
    assert (out["fisher_reason"].iloc[bad:bad + need] == "nonfinite_input").all()
    assert (out["fisher_valid"].iloc[bad:bad + need] == 0.0).all()
    assert (out["signal"].iloc[bad:bad + need] == 0).all()
    pd.testing.assert_frame_equal(out.iloc[:bad], clean.iloc[:bad])
    pd.testing.assert_frame_equal(out.iloc[bad + need:], clean.iloc[bad + need:])
    gapped = df.drop(df.index[bad])
    out = _apply(gapped, P)
    assert (out["fisher_reason"].iloc[bad:bad + need - 1] == "irregular_cadence").all()
    assert out["fisher_valid"].iloc[bad + need - 1] == 1.0


@pytest.mark.parametrize("params", [
    {"lookback": 1}, {"lookback": 33}, {"lookback": 9.5}, {"lookback": True}, {"lookback": float("nan")},
    {"lookback": "9"}, {"extreme": 0.49}, {"extreme": 3.01}, {"extreme": float("inf")}, {"extreme": True},
    {"extreme": "1.5"},
])
def test_invalid_parameters_are_explicit_errors(params):
    with pytest.raises(ValueError):
        _apply(_walk(120, 1), {**{"lookback": 9, "extreme": 1.5}, **params})


def test_integral_float_lookback_is_accepted():
    df = _walk(120, 1)
    pd.testing.assert_frame_equal(_apply(df, {"lookback": 9.0, "extreme": 1.5}), _apply(df, {"lookback": 9, "extreme": 1.5}))


def _frozen(key, bars):
    manifest = om.load_manifest(MANIFEST)
    candles = om.load_candles(om.dataset_by_key(manifest, key))
    return candles[INPUT_COLUMNS].iloc[-bars:]


def _grid_and_endpoints():
    specs = [(k, list(v)) for k, v in DEFAULT_PARAM_RANGES[NAME].items()]
    return [p for _, p in expand_sweep({"lookback": 9, "extreme": 1.5}, specs)] + DOMAIN_ENDPOINTS


DECISION_COLUMNS = ["fisher_value", "fisher_trigger", "fisher_prev_trigger", "fisher_valid",
                    "fisher_reason_code", "signal"]


def test_full_series_prefix_exact_span_and_200_row_tail_agree_for_every_permitted_extreme():
    df = _frozen("ETH 4h", 500)
    for params in _grid_and_endpoints():
        need = params["lookback"] + HORIZON + 1
        assert need <= 97
        full = _apply(df, params)
        valid_ends = [int(i) + 1 for i in np.flatnonzero(full["fisher_valid"].to_numpy() == 1.0) if i >= 200]
        ends = sorted({valid_ends[0], valid_ends[len(valid_ends) // 2], valid_ends[-1], 300, 500})
        for end in ends:
            frames = (df.iloc[:end], df.iloc[end - 200:end], df.iloc[end - need:end])
            for part in frames:
                got = _apply(part, params)[DECISION_COLUMNS].iloc[-1]
                pd.testing.assert_series_equal(got, full[DECISION_COLUMNS].iloc[end - 1], check_names=False)
            short = _apply(df.iloc[end - need + 1:end], params)
            assert short["fisher_reason"].iloc[-1] == "insufficient_history"


def _composer(df, params, side="", position_ctx=None):
    reg = _reg()
    evaluation = evaluate_open_close(
        reg.apply_strategy, reg.get_strategy, df, NAME, NAME, ["tiered_tp_pct"], side, params=params,
        position_ctx=position_ctx, close_evaluate=close_evaluate, close_params_by_name={"tiered_tp_pct": {}})
    return evaluation, finalize_decision(evaluation, side)


def _entry_frame(side):
    falling = list(np.linspace(300.0, 100.0, 150))
    rising = list(np.linspace(100.0, 300.0, 150))
    return _from_mids(falling + [falling[-1] + 30.0]) if side == "long" else _from_mids(rising + [rising[-1] - 30.0])


def test_composer_emits_both_directions_and_holds_entries_while_a_position_is_open():
    p = {"lookback": 9, "extreme": 0.5}
    _, flat_long = _composer(_entry_frame("long"), p)
    _, flat_short = _composer(_entry_frame("short"), p)
    assert (flat_long["open_action"], flat_long["signal"]) == ("long", 1)
    assert (flat_short["open_action"], flat_short["signal"]) == ("short", -1)
    df = _entry_frame("short")
    mark = float(df["close"].iloc[-1])
    ctx = {"side": "long", "avg_cost": mark, "entry_atr": 50.0, "current_quantity": 1.0, "initial_quantity": 1.0}
    _, held = _composer(df, p, "long", ctx)
    assert held["close_fraction"] == 0.0 and held["signal"] == 0
    df = _entry_frame("long")
    mark = float(df["close"].iloc[-1])
    _, held = _composer(df, p, "long", dict(ctx, avg_cost=mark))
    assert held["signal"] == 0 and held["close_fraction"] == 0.0


@pytest.mark.parametrize("mutation", ["volume", "geometry", "cadence"])
def test_invalid_entry_frame_keeps_the_close_owner_executable(mutation):
    df = _walk(200, 9)
    if mutation == "volume":
        df = df.assign(volume=np.r_[df["volume"].to_numpy()[:-1], -5.0])
    elif mutation == "geometry":
        df = df.assign(open=np.r_[df["open"].to_numpy()[:-1], df["high"].iloc[-1] * 2])
    else:
        df = _shift_index(df, len(df) - 1, pd.Timedelta(hours=2))
    mark = float(df["close"].iloc[-1])
    ctx = {"side": "long", "avg_cost": mark / 1.05, "entry_atr": 1.0, "current_quantity": 1.0,
           "initial_quantity": 1.0}
    evaluation, decision = _composer(df, P, "long", ctx)
    assert evaluation.open_result_df["fisher_valid"].iloc[-1] == 0.0
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
    params = {"lookback": 9, "extreme": 0.75}
    df = _walk(900, 5)
    sig = ensure_atr_indicator(_apply(df, params))
    res = _bt(params).run(sig, strategy_name=NAME, symbol="BTC/USDT", timeframe="4h", save=False)
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
        spans.append((entry, max(pd.Timestamp(l["exit_date"]) for l in legs)))
    spans.sort()
    for (_, prev_exit), (next_entry, _) in zip(spans, spans[1:]):
        assert next_entry >= prev_exit
    assert any(len(legs) > 1 for legs in by_entry.values())


def test_simulator_keeps_closing_and_stopping_after_entry_inputs_fail():
    params = {"lookback": 9, "extreme": 0.75}
    df = _walk(600, 5)
    sig = ensure_atr_indicator(_apply(df, params))
    first = next(int(i) for i in np.flatnonzero(sig["signal"].to_numpy() != 0)
                 if i > 100 and np.isfinite(sig["atr"].iloc[i]))
    corrupt = df.copy()
    corrupt.iloc[first + 1:, corrupt.columns.get_loc("volume")] = np.nan
    bad = ensure_atr_indicator(_apply(corrupt, params))
    assert (bad["signal"].iloc[:first + 1] == sig["signal"].iloc[:first + 1]).all()
    assert (bad["signal"].iloc[first + 1:] == 0).all()
    res = _bt(params).run(bad, strategy_name=NAME, symbol="BTC/USDT", timeframe="4h", save=False)
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
@pytest.mark.parametrize("params", DOMAIN_ENDPOINTS + [{"lookback": 9, "extreme": 1.5}])
def test_entry_parity_on_frozen_candles(window, params):
    frame = compute_parity_frame(_frozen("BTC 4h", 320), cfg=_cfg(params), window=window)
    result = summarize(frame)
    assert result["bars_compared"] > 0
    assert result["clean"], frame[~frame["match"]].head()


@pytest.mark.parametrize("window", [200, None])
def test_composed_parity_covers_both_sides_with_the_close_owner(window):
    params = {"lookback": 5, "extreme": 0.75}
    cfg = _cfg(params, close_refs=json.loads(json.dumps(CLOSE)), direction="both")
    frame = compute_parity_frame(_frozen("SOL 4h", 520), cfg=cfg, window=window)
    result = summarize(frame)
    assert result["mismatches"] == 0 and result["clean"], frame[~frame["match"]].head()
    assert (frame["live_open_action"] == "long").any() and (frame["live_open_action"] == "short").any()
    closing = frame[frame["live_close_fraction"] > 0]
    assert (closing["live_signal"] == -1).any() and (closing["live_signal"] == 1).any()


@pytest.mark.parametrize("window", [200, None])
def test_batched_explicit_paper_parity_matches_solo(window):
    params = {"lookback": 5, "extreme": 0.75}
    cfg = _cfg(params, close_refs=json.loads(json.dumps(CLOSE)), direction="both", batched=True)
    frame = compute_parity_frame(_frozen("BTC 4h", 300), cfg=cfg, window=window)
    result = summarize(frame)
    assert result["bars_compared"] > 0
    assert result["clean"] and result["batch_clean"], frame[~frame["match"]].head()
