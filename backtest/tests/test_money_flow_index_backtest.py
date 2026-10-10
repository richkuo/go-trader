import json
import math
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
from backtester import Backtester
from close_registry_loader import evaluate as close_evaluate
from eval_windows import expand_sweep
from optimizer import DEFAULT_PARAM_RANGES
from parity_diff import ParityConfig, compute_parity_frame, summarize
from registry_loader import load_registry
from strategy_composition import evaluate_open_close, finalize_decision

NAME = "money_flow_index_reversal"
CANDIDATE_DIR = os.path.join(_REPO, "backtest", "candidates", "money_flow_index_reversal_1658")
MANIFEST = os.path.join(CANDIDATE_DIR, "study_manifest.json")
CLOSE = [{"name": "tiered_tp_atr", "params": {"tp_tiers": [{"atr_multiple": 2.0, "close_fraction": 1.0}]}}]
INPUT_COLUMNS = ["open", "high", "low", "close", "volume"]


def _reg():
    return load_registry("futures")


def _apply(df, params):
    return _reg().apply_strategy(NAME, df, params)


def _flat_frame(rows):
    df = pd.DataFrame(rows, columns=["close", "volume"], dtype=float)
    df["open"] = df["close"]
    df["high"] = df["close"]
    df["low"] = df["close"]
    df = df[INPUT_COLUMNS]
    df.index = pd.date_range("2026-01-01", periods=len(df), freq="4h")
    return df


def _tp(h, low, c):
    return h / 3.0 + low / 3.0 + c / 3.0


def _expected_mfi(p, q):
    if p > 0:
        return 100.0 / (1.0 + q / p)
    return 0.0


def _walk(n, seed, start=100.0):
    rng = np.random.default_rng(seed)
    close = start * np.exp(np.cumsum(rng.normal(0.0, 0.02, n)))
    open_ = np.concatenate([[start], close[:-1]])
    high = np.maximum(open_, close) * (1.0 + rng.uniform(0.001, 0.01, n))
    low = np.minimum(open_, close) * (1.0 - rng.uniform(0.001, 0.01, n))
    volume = rng.uniform(100.0, 1000.0, n)
    df = pd.DataFrame({"open": open_, "high": high, "low": low, "close": close, "volume": volume})
    df.index = pd.date_range("2026-01-01", periods=n, freq="4h")
    return df


def test_registration_is_futures_only_bidirectional_no_edge_and_hidden():
    fut = _reg()
    entry = fut.STRATEGY_REGISTRY[NAME]
    assert entry["default_params"] == {"lookback": 14, "oversold": 20.0, "overbought": 80.0}
    assert entry["edge_status"] == "no_edge"
    assert entry["edge_source"] in ("unvalidated", "study_fail", "study_inconclusive")
    assert os.path.isfile(os.path.join(_REPO, entry["edge_ref"]))
    assert fut._registry.STRATEGIES[NAME]["short_entries"] is True
    assert fut._registry.STRATEGIES[NAME]["platforms"] == ("futures",)
    assert NAME in fut._registry.short_entry_strategies()
    assert NAME not in fut.list_strategies()
    assert NAME not in load_registry("spot").STRATEGY_REGISTRY
    assert set(DEFAULT_PARAM_RANGES[NAME]) == {"lookback", "oversold", "overbought"}


@pytest.mark.parametrize("rows,params,sig,reason,prev,cur", [
    ([(12, 1), (6, 2), (12, 1), (15, 1)], {"lookback": 2, "oversold": 50.0, "overbought": 90.0},
     1, "entry_long", 50.0, 100.0),
    ([(12, 1), (6, 2), (3, 4), (6, 2)], {"lookback": 2, "oversold": 50.0, "overbought": 90.0},
     0, "no_cross", 0.0, 50.0),
    ([(12, 1), (6, 2), (12, 1), (9, 2)], {"lookback": 2, "oversold": 10.0, "overbought": 50.0},
     -1, "entry_short", 50.0, 40.0),
    ([(3, 1), (6, 2), (12, 1), (6, 2)], {"lookback": 2, "oversold": 10.0, "overbought": 50.0},
     0, "no_cross", 100.0, 50.0),
    ([(10, 1), (9, 1), (8, 1), (9, 1)], {"lookback": 2, "oversold": 20.0, "overbought": 80.0},
     1, "entry_long", 0.0, None),
    ([(10, 1), (11, 1), (12, 1), (11, 1)], {"lookback": 2, "oversold": 20.0, "overbought": 80.0},
     -1, "entry_short", 100.0, None),
])
def test_hand_derived_crossings_and_threshold_equality(rows, params, sig, reason, prev, cur):
    df = _flat_frame(rows)
    before = df.copy()
    out = _apply(df, params)
    pd.testing.assert_frame_equal(df, before)
    pd.testing.assert_frame_equal(out[INPUT_COLUMNS], before)
    prices = [r[0] for r in rows]
    vols = [r[1] for r in rows]
    tps = [_tp(p, p, p) for p in prices]
    p_tot = sum(tps[i] * vols[i] for i in (2, 3) if tps[i] > tps[i - 1])
    q_tot = sum(tps[i] * vols[i] for i in (2, 3) if tps[i] < tps[i - 1])
    last = out.iloc[-1]
    assert last["mfi_positive_total"] == p_tot and last["mfi_negative_total"] == q_tot
    assert last["mfi_value"] == _expected_mfi(p_tot, q_tot)
    if cur is not None:
        assert last["mfi_value"] == cur
    assert last["mfi_prev_value"] == prev
    assert int(last["signal"]) == sig and last["mfi_reason"] == reason
    assert bool(last["mfi_valid"]) is True
    assert (out["signal"].iloc[:-1] == 0).all()
    assert list(out["mfi_reason"].iloc[:3]) == ["insufficient_history"] * 3
    assert math.isnan(out["mfi_value"].iloc[1]) and not math.isnan(out["mfi_value"].iloc[2])
    for col in ("mfi_high", "mfi_low", "mfi_close", "mfi_volume"):
        assert last[col] == last[col.replace("mfi_", "")]


def _holds(df, params, reason):
    before = df.copy()
    out = _apply(df, params)
    pd.testing.assert_frame_equal(df, before)
    for col in df.columns:
        pd.testing.assert_series_equal(out[col], before[col])
    assert (out["signal"] == 0).all()
    assert not out["mfi_valid"].any()
    assert out["mfi_reason"].iloc[-1].startswith(reason), out["mfi_reason"].iloc[-1]
    return out


P2 = {"lookback": 2, "oversold": 20.0, "overbought": 80.0}


@pytest.mark.parametrize("mutate,reason", [
    (lambda df: df.drop(columns=["volume"]), "missing_columns:volume"),
    (lambda df: df.drop(columns=["high", "volume"]), "missing_columns:high,volume"),
    (lambda df: df.assign(volume=np.nan), "nonfinite_input"),
    (lambda df: df.assign(volume=np.inf), "nonfinite_input"),
    (lambda df: df.assign(high=np.nan), "nonfinite_input"),
    (lambda df: df.assign(volume=-1.0), "negative_volume"),
    (lambda df: df.assign(low=0.0), "nonpositive_price"),
    (lambda df: df.assign(high=df["low"] * 0.5), "inconsistent_hlc"),
    (lambda df: df.assign(volume=0.0), "zero_total_flow"),
    (lambda df: df.assign(open=50.0, high=50.0, low=50.0, close=50.0), "zero_total_flow"),
    (lambda df: df.assign(volume=1e308), "arithmetic_overflow"),
    (lambda df: df.set_axis(df.index[::-1]), "timestamp_order"),
    (lambda df: df.iloc[:3], "insufficient_history"),
])
def test_invalid_entry_data_is_a_reasoned_hold_that_preserves_the_frame(mutate, reason):
    df = mutate(_flat_frame([(10, 1), (11, 1), (12, 1), (11, 1), (10, 1), (11, 1)]))
    _holds(df, P2, reason)


def test_window_total_overflow_holds_while_each_flow_is_finite():
    df = _flat_frame([(10, 1), (11, 1e307), (12, 1e307), (13, 1e307)])
    out = _apply(df, P2)
    assert np.isfinite(out["mfi_raw_flow"].iloc[1:]).all()
    assert out["mfi_reason"].iloc[-1] == "arithmetic_overflow"
    assert (out["signal"] == 0).all()


def test_a_bad_bar_invalidates_its_dependent_windows_and_recovery_needs_full_history():
    df = _walk(40, 3)
    bad = 20
    df.iloc[bad, df.columns.get_loc("volume")] = np.nan
    out = _apply(df, {"lookback": 5, "oversold": 30.0, "overbought": 70.0})
    reasons = out["mfi_reason"]
    assert reasons.iloc[bad] == "nonfinite_input"
    span = range(bad, bad + 5 + 2)
    assert all(reasons.iloc[i] == "nonfinite_input" for i in span)
    assert not out["mfi_valid"].iloc[bad:bad + 7].any()
    assert out["mfi_valid"].iloc[bad + 7]
    clean = _apply(df.iloc[bad + 1:], {"lookback": 5, "oversold": 30.0, "overbought": 70.0})
    np.testing.assert_array_equal(out["mfi_value"].iloc[bad + 7:].to_numpy(),
                                  clean["mfi_value"].iloc[6:].to_numpy())


@pytest.mark.parametrize("params", [
    {"lookback": 1}, {"lookback": 199}, {"lookback": 14.5}, {"lookback": True}, {"lookback": float("nan")},
    {"oversold": -1.0}, {"overbought": 101.0}, {"oversold": 80.0, "overbought": 80.0},
    {"oversold": float("inf")}, {"overbought": True}, {"oversold": "20"},
])
def test_invalid_parameters_are_explicit_errors(params):
    with pytest.raises(ValueError):
        _apply(_walk(30, 1), {**{"lookback": 14, "oversold": 20.0, "overbought": 80.0}, **params})


@pytest.mark.parametrize("params", [
    {"lookback": 2, "oversold": 0.0, "overbought": 100.0},
    {"lookback": 198, "oversold": 20.0, "overbought": 80.0},
])
def test_supported_bounds_decide_inside_the_200_bar_check_window(params):
    df = _walk(260, 11)
    full = _apply(df, params)
    tail = _apply(df.iloc[-200:], params)
    assert bool(tail["mfi_valid"].iloc[-1])
    assert tail["mfi_value"].iloc[-1] == full["mfi_value"].iloc[-1]
    assert tail["signal"].iloc[-1] == full["signal"].iloc[-1]
    short = _apply(df.iloc[-(params["lookback"] + 1):], params)
    assert short["mfi_reason"].iloc[-1] == "insufficient_history"


def _composer(df, params, side="", position_ctx=None):
    reg = _reg()
    evaluation = evaluate_open_close(
        reg.apply_strategy, reg.get_strategy, df, NAME, NAME, ["tiered_tp_atr"], side, params=params,
        position_ctx=position_ctx, close_evaluate=close_evaluate,
        close_params_by_name={"tiered_tp_atr": CLOSE[0]["params"]})
    return evaluation, finalize_decision(evaluation, side)


def test_composer_emits_both_entry_directions_and_blocks_entries_while_open():
    long_df = _flat_frame([(10, 1), (9, 1), (8, 1), (9, 1)])
    short_df = _flat_frame([(10, 1), (11, 1), (12, 1), (11, 1)])
    _, flat_long = _composer(long_df, P2)
    _, flat_short = _composer(short_df, P2)
    assert (flat_long["open_action"], flat_long["signal"]) == ("long", 1)
    assert (flat_short["open_action"], flat_short["signal"]) == ("short", -1)
    _, held = _composer(short_df, P2, "long", {"side": "long", "avg_cost": 11.0, "entry_atr": 50.0,
                                                "current_quantity": 1.0, "initial_quantity": 1.0})
    assert held["open_action"] == "short" and held["close_fraction"] == 0.0 and held["signal"] == 0


def test_invalid_entry_inputs_keep_the_close_owner_running():
    df = _flat_frame([(10, 1), (11, 1), (12, 1), (13, 1), (14, 1)]).assign(volume=np.nan)
    ctx = {"side": "long", "avg_cost": 10.0, "entry_atr": 1.0, "current_quantity": 1.0, "initial_quantity": 1.0}
    evaluation, decision = _composer(df, P2, "long", ctx)
    assert evaluation.open_result_df["mfi_reason"].iloc[-1] == "nonfinite_input"
    assert decision["open_action"] == "none"
    assert decision["close_strategy"] == "tiered_tp_atr" and decision["close_fraction"] == 1.0
    assert decision["signal"] == -1
    price_bad = _flat_frame([(10, 1), (11, 1), (12, 1), (13, 1), (14, 1)]).assign(low=-1.0)
    _, decision = _composer(price_bad, P2, "short", {**ctx, "side": "short", "avg_cost": 20.0})
    assert decision["open_action"] == "none" and decision["signal"] == 1


def _bt(**kw):
    base = dict(initial_capital=1000.0, platform="hyperliquid",
                open_strategy={"name": NAME, "params": kw.pop("params")},
                close_strategies=CLOSE, stop_loss_atr_mult=1.0, direction="both")
    base.update(kw)
    return Backtester(**base)


def test_simulator_fills_next_bar_open_with_explicit_exit_and_no_reversal_or_scale_in():
    params = {"lookback": 7, "oversold": 30.0, "overbought": 70.0}
    df = _walk(600, 5)
    sig = ensure_atr_indicator(_apply(df, params))
    res = _bt(params=params).run(sig, strategy_name=NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    trades = res["trades"]
    assert {t["side"] for t in trades} == {"long", "short"}
    signal_at = sig["signal"]
    last_exit = None
    for t in trades:
        entry = pd.Timestamp(t["entry_date"])
        pos = sig.index.get_loc(entry)
        assert signal_at.iloc[pos - 1] == (1 if t["side"] == "long" else -1)
        assert t["entry_price"] == pytest.approx(df["open"].iloc[pos], rel=0.01)
        assert t["exit_reason"].split(":")[0] in ("sl", "tiered_tp_atr", "end_of_data"), t["exit_reason"]
        if last_exit is not None:
            assert entry >= last_exit
        last_exit = pd.Timestamp(t["exit_date"])
    assert len({t["entry_date"] for t in trades}) == len(trades)


def test_simulator_closes_and_stops_when_entry_inputs_fail():
    params = {"lookback": 7, "oversold": 30.0, "overbought": 70.0}
    df = _walk(400, 5)
    sig = ensure_atr_indicator(_apply(df, params))
    first = next(int(i) for i in np.flatnonzero(sig["signal"].to_numpy() != 0)
                 if i > 30 and np.isfinite(sig["atr"].iloc[i]))
    corrupt = df.copy()
    corrupt.iloc[first + 1:, corrupt.columns.get_loc("volume")] = np.nan
    bad = ensure_atr_indicator(_apply(corrupt, params))
    assert (bad["signal"].iloc[:first + 1] == sig["signal"].iloc[:first + 1]).all()
    assert (bad["signal"].iloc[first + 1:] == 0).all()
    assert (bad["mfi_reason"].iloc[first + 1:] == "nonfinite_input").all()
    res = _bt(params=params).run(bad, strategy_name=NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    entries = [pd.Timestamp(t["entry_date"]) for t in res["trades"]]
    assert max(entries) == bad.index[first + 1]
    t = res["trades"][-1]
    assert pd.Timestamp(t["entry_date"]) == bad.index[first + 1]
    assert t["exit_reason"].split(":")[0] in ("sl", "tiered_tp_atr")


def _frozen(key, bars):
    manifest = om.load_manifest(MANIFEST)
    candles = om.load_candles(om.dataset_by_key(manifest, key))
    return candles[INPUT_COLUMNS].iloc[-bars:]


def test_full_series_prefix_and_bounded_values_are_bit_identical_across_the_grid():
    df = _frozen("ETH 4h", 460)
    specs = [(k, list(v)) for k, v in DEFAULT_PARAM_RANGES[NAME].items()]
    for _, params in expand_sweep({"lookback": 14, "oversold": 20.0, "overbought": 80.0}, specs):
        full = _apply(df, params)
        for end in (230, 345, 460):
            tail = _apply(df.iloc[end - 200:end], params)
            prefix = _apply(df.iloc[:end], params)
            for frame in (tail, prefix):
                assert frame["mfi_value"].iloc[-1] == full["mfi_value"].iloc[end - 1]
                assert frame["mfi_positive_total"].iloc[-1] == full["mfi_positive_total"].iloc[end - 1]
                assert frame["signal"].iloc[-1] == full["signal"].iloc[end - 1]


def _cfg(params, **kw):
    base = dict(strategy_name=NAME, params=params, registry="futures", platform="hyperliquid",
                symbol="BTC", timeframe="4h")
    base.update(kw)
    return ParityConfig(**base)


PARITY_PARAMS = [
    {"lookback": 7, "oversold": 30.0, "overbought": 70.0},
    {"lookback": 14, "oversold": 20.0, "overbought": 80.0},
    {"lookback": 2, "oversold": 0.0, "overbought": 100.0},
    {"lookback": 198, "oversold": 20.0, "overbought": 80.0},
]


@pytest.mark.parametrize("window", [200, None])
@pytest.mark.parametrize("params", PARITY_PARAMS)
def test_entry_parity_on_frozen_candles(window, params):
    frame = compute_parity_frame(_frozen("BTC 4h", 320), cfg=_cfg(params), window=window)
    result = summarize(frame)
    assert result["bars_compared"] > 0
    assert result["clean"], frame[~frame["match"]].head()


@pytest.mark.parametrize("window", [200, None])
def test_composed_parity_covers_both_sides_with_the_close_owner(window):
    params = {"lookback": 7, "oversold": 30.0, "overbought": 70.0}
    cfg = _cfg(params, close_refs=json.loads(json.dumps(CLOSE)), direction="both")
    frame = compute_parity_frame(_frozen("SOL 4h", 420), cfg=cfg, window=window)
    result = summarize(frame)
    assert result["mismatches"] == 0 and result["clean"], frame[~frame["match"]].head()
    assert (frame["live_open_action"] == "long").any() and (frame["live_open_action"] == "short").any()
    closing = frame[frame["live_close_fraction"] > 0]
    assert (closing["live_signal"] == -1).any() and (closing["live_signal"] == 1).any()


@pytest.mark.parametrize("window", [200, None])
def test_batched_explicit_paper_parity_matches_solo(window):
    params = {"lookback": 7, "oversold": 30.0, "overbought": 70.0}
    cfg = _cfg(params, close_refs=json.loads(json.dumps(CLOSE)), direction="both", batched=True)
    frame = compute_parity_frame(_frozen("BTC 4h", 260), cfg=cfg, window=window)
    result = summarize(frame)
    assert result["bars_compared"] > 0
    assert result["clean"] and result["batch_clean"], frame[~frame["match"]].head()


def test_candidate_parity_entry_point_runs_offline():
    script = os.path.join(CANDIDATE_DIR, "parity_check.py")
    proc = subprocess.run(
        [sys.executable, script, "--registry", "futures", "--window", "200", "--bars", "230",
         "--dataset", "BTC 4h", "--mode", "entry"],
        capture_output=True, text=True, cwd=_REPO, timeout=600)
    assert proc.returncode == 0, proc.stdout + proc.stderr
    assert proc.stdout.strip().endswith("clean")
