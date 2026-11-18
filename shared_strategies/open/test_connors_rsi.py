import os
import sys

import numpy as np
import pandas as pd
import pytest

from shared_strategies.open.conftest import load_module

_HERE = os.path.dirname(os.path.abspath(__file__))
_ROOT = os.path.abspath(os.path.join(_HERE, "..", ".."))
_TOOLS = os.path.join(_ROOT, "shared_tools")
if _TOOLS not in sys.path:
    sys.path.insert(0, _TOOLS)

_FUTURES = load_module(
    "_crsi_futures_strategies_test", os.path.join(_HERE, "futures", "strategies.py"))
_SPOT = load_module(
    "_crsi_spot_strategies_test", os.path.join(_HERE, "spot", "strategies.py"))
_CHECK_HL = load_module(
    "_crsi_check_hyperliquid_test", os.path.join(_ROOT, "shared_scripts", "check_hyperliquid.py"))

from close_registry_loader import evaluate as close_evaluate
from strategy_composition import evaluate_open_close, finalize_decision

NAME = "connors_rsi_reversion"
DEFAULTS = {"price_period": 3, "streak_period": 2, "rank_window": 100,
            "oversold": 10.0, "overbought": 90.0}
SPAN = 50
CAP = 20
COLS = ["signal", "crsi_price_rsi", "crsi_streak", "crsi_streak_rsi", "crsi_rank",
        "crsi", "crsi_prev", "crsi_valid", "crsi_reason", "crsi_reason_code"]


def run(df, params=None):
    return _FUTURES.apply_strategy(NAME, df, params)


def need_for(rank_window):
    return max(SPAN + CAP + 1, rank_window + 2) + 1


def frame(closes, start="2025-01-01", freq="4h"):
    closes = np.asarray(closes, dtype=float)
    df = pd.DataFrame({"open": closes, "high": closes, "low": closes,
                       "close": closes, "volume": np.ones(len(closes))})
    df.index = pd.date_range(start, periods=len(closes), freq=freq)
    return df


def market(n=420, seed=1645):
    rng = np.random.RandomState(seed)
    t = np.arange(n)
    close = 100.0 * np.exp(0.08 * np.sin(t / 25.0) + np.cumsum(rng.randn(n) * 0.012))
    open_ = np.concatenate([[close[0]], close[:-1]])
    span = np.abs(rng.randn(n)) * 0.004 * close + 0.01
    df = pd.DataFrame({"open": open_, "high": np.maximum(open_, close) + span,
                       "low": np.minimum(open_, close) - span, "close": close,
                       "volume": 50 + np.abs(rng.randn(n)) * 40})
    df.index = pd.date_range("2025-01-01", periods=n, freq="4h")
    return df


def test_registered_no_edge_futures_and_hidden():
    entry = _FUTURES.STRATEGY_REGISTRY[NAME]
    assert (entry["edge_status"], entry["edge_source"], entry["edge_ref"]) == (
        "no_edge", "study_fail", "backtest/candidates/connors_rsi_reversion_1645/REPORT.md")
    assert entry["default_params"] == DEFAULTS
    assert NAME not in _FUTURES.list_strategies()
    assert NAME not in _FUTURES.DISCOVERY_STRATEGY_REGISTRY
    assert NAME not in _SPOT.STRATEGY_REGISTRY


def _same(a, b):
    for col in COLS:
        x, y = a[col], b[col]
        if isinstance(x, float) and np.isnan(x):
            assert isinstance(y, float) and np.isnan(y), col
        else:
            assert x == y, (col, x, y)


def test_unchanged_close_resets_streak():
    closes = [100.0 + (k % 7) for k in range(150)] + [99.0, 101.0, 102.0, 103.0, 103.0]
    out = run(frame(closes))
    assert out["crsi_streak"].iloc[-2] == 3.0
    assert out["crsi_streak"].iloc[-1] == 0.0


def _first_entry(out, side):
    hits = np.flatnonzero(out["signal"].to_numpy() == side)
    assert len(hits) > 0
    return int(hits[0])


BAD_CLOSE_CASES = [
    (float("nan"), "nonfinite_input"),
    (float("inf"), "nonfinite_input"),
    (0.0, "nonpositive_close"),
    (-5.0, "nonpositive_close"),
]


def test_bad_close_holds_for_every_decision_whose_span_includes_it(subtests):
    for value, reason in BAD_CLOSE_CASES:
        with subtests.test(value=value, reason=reason):
            df = market(300, seed=3)
            df.iloc[150, df.columns.get_loc("close")] = value
            out = run(df)
            need = need_for(DEFAULTS["rank_window"])
            assert out["crsi_reason"].iloc[150] == reason
            affected = out.iloc[150: 150 + need]
            assert (affected["signal"] == 0).all()
            assert (~affected["crsi_valid"]).all()
            assert (affected["crsi_reason"] == reason).all()
            assert bool(out["crsi_valid"].iloc[150 + need])


INVALID_PARAMS = [
    {"price_period": True},
    {"price_period": 1},
    {"price_period": 11},
    {"price_period": 3.5},
    {"streak_period": 1},
    {"streak_period": 11},
    {"rank_window": 19},
    {"rank_window": 151},
    {"oversold": 0.0},
    {"overbought": 100.0},
    {"oversold": 50.0, "overbought": 50.0},
    {"oversold": 60.0, "overbought": 40.0},
    {"oversold": float("nan")},
    {"overbought": False},
]


def _compose(df, position_side, position_ctx=None, params=None):
    evaluation = evaluate_open_close(
        _FUTURES.apply_strategy, _FUTURES.get_strategy, df, NAME, NAME,
        ["time_stop"], position_side, params, position_ctx,
        close_evaluate=close_evaluate,
        market_ctx={"mark_price": float(df["close"].iloc[-1])},
        close_params_by_name={"time_stop": {"max_bars": 5}},
    )
    return evaluation, finalize_decision(evaluation, position_side)


def test_composer_entry_duplicate_and_reversal_blocking():
    df = market()
    out = run(df)
    i = _first_entry(out, 1)
    window = df.iloc[: i + 1]
    _, flat_decision = _compose(window, "")
    assert flat_decision["signal"] == 1 and flat_decision["open_action"] == "long"
    held = {"side": "long", "bars_held": 1, "avg_cost": 100.0, "current_quantity": 1.0}
    _, dup = _compose(window, "long", held)
    assert dup["open_action"] == "long" and dup["signal"] == 0
    short_ctx = {"side": "short", "bars_held": 1, "avg_cost": 100.0, "current_quantity": 1.0}
    _, opposite = _compose(window, "short", short_ctx)
    assert opposite["close_fraction"] == 0.0 and opposite["signal"] == 0
    j = _first_entry(out, -1)
    _, short_entry = _compose(df.iloc[: j + 1], "")
    assert short_entry["signal"] == -1 and short_entry["open_action"] == "short"


def test_composer_close_wins_and_survives_bad_entry_data():
    df = market()
    out = run(df)
    i = _first_entry(out, 1)
    window = df.iloc[: i + 1]
    due = {"side": "long", "bars_held": 5, "avg_cost": 100.0, "current_quantity": 1.0}
    _, both = _compose(window, "long", due)
    assert both["close_fraction"] == 1.0 and both["signal"] == -1
    bad = window.copy()
    bad.iloc[-1, bad.columns.get_loc("close")] = float("nan")
    evaluation, decision = _compose(bad, "long", due)
    assert evaluation.open_result_df["crsi_reason"].iloc[-1] == "nonfinite_input"
    assert decision["open_action"] == "none"
    assert decision["close_fraction"] == 1.0 and decision["signal"] == -1
    short_due = {"side": "short", "bars_held": 7, "avg_cost": 100.0, "current_quantity": 1.0}
    _, short_close = _compose(window.drop(columns=["close"]).assign(close=np.nan), "short", short_due)
    assert short_close["close_fraction"] == 1.0 and short_close["signal"] == 1


def _hl_candles(df):
    return [[int(ts.value // 1_000_000), r.open, r.high, r.low, r.close, r.volume]
            for ts, r in df.iterrows()]


def test_production_hyperliquid_paths_admit_explicit_paper_and_refuse_unacknowledged_live():
    df = _CHECK_HL._make_dataframe(_hl_candles(market(200)))
    shared = _CHECK_HL.build_shared_signal_state("BTC", "4h", df=df)
    with pytest.raises(ValueError, match="allow_no_edge"):
        _CHECK_HL.evaluate_signal_slot(shared, {"id": "solo", "strategy": NAME})
    with pytest.raises(ValueError, match="allow_no_edge"):
        _CHECK_HL.evaluate_signal_slot(
            shared, {"id": "open", "strategy": "breakout", "open_strategy": NAME,
                     "close_strategies": "time_stop"})
    with pytest.raises(ValueError, match="allow_no_edge"):
        _CHECK_HL.evaluate_signal_slot(
            shared, {"id": "close", "strategy": "breakout", "open_strategy": "breakout",
                     "close_strategies": NAME})
    deps = _CHECK_HL._signal_check_deps()
    peer = _CHECK_HL.evaluate_signal_slot(shared, {"id": "peer", "strategy": "breakout"}, deps=deps)
    assert peer["strategy"] == "breakout"
    with pytest.raises(ValueError, match="allow_no_edge"):
        _CHECK_HL.evaluate_signal_slot(shared, {"id": "batched", "strategy": NAME}, deps=deps)
    paper = _CHECK_HL.evaluate_signal_slot(shared, {"id": "paper", "strategy": NAME, "mode_args": ["--mode=paper"]})
    acked = _CHECK_HL.evaluate_signal_slot(
        shared, {"id": "acked", "strategy": NAME, "mode_args": ["--mode=live"], "allow_no_edge": True})
    assert paper["strategy"] == acked["strategy"] == NAME
    assert (paper["signal"], paper["price"]) == (acked["signal"], acked["price"])
    fallback = _CHECK_HL.evaluate_signal_slot(
        shared, {"id": "fallback", "strategy": "breakout", "open_strategy": "breakout",
                 "close_strategies": NAME, "mode_args": ["--mode", "paper"]})
    assert fallback["close_strategies"] == [NAME]


