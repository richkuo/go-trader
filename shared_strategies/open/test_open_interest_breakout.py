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
    "_oib_futures_strategies_test", os.path.join(_HERE, "futures", "strategies.py"))
_SPOT = load_module(
    "_oib_spot_strategies_test", os.path.join(_HERE, "spot", "strategies.py"))
_CHECK_HL = load_module(
    "_oib_check_hyperliquid_test", os.path.join(_ROOT, "shared_scripts", "check_hyperliquid.py"))

from close_registry_loader import evaluate as close_evaluate
from strategy_composition import evaluate_open_close, finalize_decision

NAME = "open_interest_breakout"
HOUR = 3_600_000
MINUTE = 60_000
T0 = 1_767_225_600_000
SMALL = {"price_lookback": 3, "oi_lookback": 2, "oi_change_threshold": 0.01}


def frame(closes, highs=None, lows=None, start=T0):
    closes = np.asarray(closes, dtype=float)
    highs = closes + 0.5 if highs is None else np.asarray(highs, dtype=float)
    lows = closes - 0.5 if lows is None else np.asarray(lows, dtype=float)
    df = pd.DataFrame({"open": closes, "high": highs, "low": lows, "close": closes,
                       "volume": np.ones(len(closes))})
    df["timestamp"] = start + HOUR * np.arange(len(closes))
    df.index = pd.to_datetime(df["timestamp"], unit="ms")
    return df


def observations(df, value_at, *, offset=MINUTE // 2, drop=(), session_at=None, **overrides):
    first = int(df["timestamp"].iloc[0]) - HOUR
    last = int(df["timestamp"].iloc[-1]) + HOUR + 1
    samples = []
    seq = 0
    for t in range(first + offset, last, MINUTE):
        seq += 1
        if any(a <= t < b for a, b in drop):
            continue
        samples.append({"recv_ms": t, "event_ms": None, "value": float(value_at(t)),
                        "session": session_at(t) if session_at else 1, "seq": seq})
    out = {"available": True, "kind": "open_interest", "units": "base", "time_basis": "receipt",
           "source": "fixture", "cadence_ms": MINUTE, "bar_interval_ms": HOUR,
           "bar_endpoint_offset_ms": HOUR, "samples": samples, "gaps": []}
    out.update(overrides)
    return out


def run(df, obs, params=SMALL):
    return _FUTURES.apply_strategy(NAME, df, {**params, "open_interest_observations": obs})


def endpoint(df, i):
    return int(df["timestamp"].iloc[i]) + HOUR


def ramp(df, start_value, end_value, i, lookback=2):
    a, b = endpoint(df, i - lookback), endpoint(df, i) - 5 * MINUTE

    def value(t):
        if t <= a:
            return start_value
        if t >= b:
            return end_value
        return start_value + (end_value - start_value) * (t - a) / (b - a)
    return value


FLAT = [100.0] * 6


def last(df, obs, params=SMALL):
    return run(df, obs, params).iloc[-1]


def test_registered_no_edge_futures_and_hidden():
    entry = _FUTURES.STRATEGY_REGISTRY[NAME]
    assert (entry["edge_status"], entry["edge_source"], entry["edge_ref"]) == (
        "no_edge", "study_inconclusive", "backtest/candidates/open_interest_breakout_1637/REPORT.md")
    assert entry["default_params"] == {
        "price_lookback": 20, "oi_lookback": 4, "oi_change_threshold": 0.002,
        "max_observation_age_ms": 120000, "observation_cadence_ms": 60000,
        "min_coverage": 0.95, "max_gap_ms": 300000}
    assert NAME not in _FUTURES.list_strategies()
    assert NAME not in _FUTURES.DISCOVERY_STRATEGY_REGISTRY
    assert NAME not in _SPOT.STRATEGY_REGISTRY


TRUTH_TABLE_CASES = [
    (110.0, 1030.0, 1, "entry_long"),
    (110.0, 1005.0, 0, "oi_below_threshold"),
    (110.0, 970.0, 0, "oi_below_threshold"),
    (90.0, 1030.0, -1, "entry_short"),
    (90.0, 970.0, 0, "oi_below_threshold"),
    (100.2, 1030.0, 0, "no_breakout"),
]


def test_endpoint_sample_at_the_close_counts_and_one_after_does_not():
    df = frame(FLAT + [110.0])
    e = endpoint(df, len(df) - 1)
    at_close = observations(df, lambda t: 1030.0 if t >= e else 1000.0, offset=0)
    row = last(df, at_close)
    assert row["oib_oi_current_ms"] == e and row["signal"] == 1
    after_close = observations(df, lambda t: 1030.0 if t > e else 1000.0, offset=0)
    after_close["samples"].append({"recv_ms": e + 1, "event_ms": None, "value": 1030.0, "session": 1,
                                   "seq": after_close["samples"][-1]["seq"] + 1})
    after_close["samples"].sort(key=lambda s: (s["recv_ms"], s["seq"]))
    row = last(df, after_close)
    assert row["oib_oi_current_ms"] == e and row["signal"] == 0
    assert row["oib_reason"] == "oi_below_threshold"


def test_gap_counts_only_once_it_was_detected_before_the_close():
    df = frame(FLAT + [110.0])
    e = endpoint(df, len(df) - 1)
    good = ramp(df, 1000.0, 1030.0, len(df) - 1)
    late = observations(df, good, gaps=[{"start_ms": e - 40 * MINUTE, "end_ms": e - 39 * MINUTE,
                                         "detected_ms": e + 30_000, "reason": "disconnected"}])
    assert last(df, late)["signal"] == 1
    known = observations(df, good, gaps=[{"start_ms": e - 40 * MINUTE, "end_ms": e - 39 * MINUTE,
                                          "detected_ms": e - 39 * MINUTE, "reason": "disconnected"}])
    row = last(df, known)
    assert row["signal"] == 0 and row["oib_reason"] == "observation_gap"
    bad = observations(df, good, gaps=[{"start_ms": e - 40 * MINUTE, "end_ms": None,
                                        "detected_ms": e - 41 * MINUTE, "reason": "disconnected"}])
    assert last(df, bad)["oib_reason"] == "unsupported_observations"


INVALID_PARAMS = [
    {"price_lookback": 1}, {"price_lookback": 201}, {"oi_lookback": 0}, {"oi_lookback": 2.5},
    {"oi_change_threshold": -0.1}, {"oi_change_threshold": 1.0}, {"oi_change_threshold": True},
    {"min_coverage": 0.0}, {"min_coverage": 1.01}, {"max_gap_ms": 30_000},
    {"max_observation_age_ms": 400_000}, {"observation_cadence_ms": 500},
]


def _market(n, seed):
    rng = np.random.RandomState(seed)
    closes = 100 + np.cumsum(rng.randn(n) * 1.2)
    df = frame(closes, closes + np.abs(rng.randn(n)) * 0.6 + 0.1, closes - np.abs(rng.randn(n)) * 0.6 - 0.1)
    drift = 0.02 * np.sin(np.arange(n) * 1.7 + seed)
    level = 1e4 * np.concatenate([[1.0], np.cumprod(1.0 + drift)[:-1]])
    opens = df["timestamp"].to_numpy()

    def value(t):
        i = min(max(int(np.searchsorted(opens, t, side="right")) - 1, 0), n - 1)
        frac = (t - opens[i]) / HOUR
        return level[i] * (1.0 + drift[i] * frac)
    return df, value


def test_live_forming_bar_carries_only_a_fresh_closed_decision():
    df = frame(FLAT + [110.0, 110.0])
    i = len(df) - 2
    value = ramp(df, 1000.0, 1030.0, i)
    fresh = run(df, observations(df, value, cutoff_ms=endpoint(df, i) + 30_000))
    assert fresh["oib_reason"].iloc[-1] == "entry_long" and fresh["oib_carried"].iloc[-1]
    assert fresh["signal"].iloc[-1] == 1
    stale = run(df, observations(df, value, cutoff_ms=endpoint(df, i) + 10 * MINUTE))
    assert stale["signal"].iloc[-1] == 0 and stale["oib_reason"].iloc[-1] == "bar_not_closed"
    early = run(df, observations(df, value, cutoff_ms=endpoint(df, i) - 1))
    assert early["oib_reason"].iloc[-2] == "bar_not_closed" and early["signal"].iloc[-2] == 0


def _compose(df, side, position_ctx=None, obs=None, close_strategies=("time_stop",)):
    params = {**SMALL, "open_interest_observations": obs}
    evaluation = evaluate_open_close(
        _FUTURES.apply_strategy, _FUTURES.get_strategy, df, NAME, NAME, list(close_strategies), side,
        params, position_ctx, close_evaluate=close_evaluate,
        market_ctx={"mark_price": float(df["close"].iloc[-1]), "atr": 1.0},
        close_params_by_name={"time_stop": {"max_bars": 3},
                              "tiered_tp_pct": {"tp_tiers": [{"profit_pct": 0.05, "close_fraction": 1.0}]}})
    return evaluation, finalize_decision(evaluation, side)


def test_composer_keeps_the_close_when_open_interest_is_missing():
    df = frame(FLAT + [110.0])
    _, flat_entry = _compose(df, "", obs=observations(df, ramp(df, 1000.0, 1030.0, len(df) - 1)))
    assert flat_entry["open_action"] == "long" and flat_entry["signal"] == 1

    due = {"side": "long", "bars_held": 3, "avg_cost": 100.0, "current_quantity": 1.0}
    evaluation, decision = _compose(df, "long", due, obs={"available": False, "reason": "feed gap"})
    assert evaluation.open_result_df["oib_reason"].iloc[-1] == "observations_unavailable"
    assert decision["open_action"] == "none"
    assert decision["close_fraction"] == 1.0 and decision["signal"] == -1

    short_due = {"side": "short", "bars_held": 4, "avg_cost": 120.0, "current_quantity": 1.0}
    _, short_close = _compose(df, "short", short_due, obs=None)
    assert short_close["close_fraction"] == 1.0 and short_close["signal"] == 1

    tp = {"side": "long", "bars_held": 1, "avg_cost": 100.0, "current_quantity": 1.0,
          "initial_quantity": 1.0}
    _, tp_close = _compose(df, "long", tp, obs=None, close_strategies=("tiered_tp_pct",))
    assert tp_close["close_fraction"] == 1.0 and tp_close["signal"] == -1


def _hl_candles(df):
    return [[int(t) + HOUR - 1, r.open, r.high, r.low, r.close, r.volume]
            for t, r in zip(df["timestamp"], df.itertuples())]


def test_production_hyperliquid_paths_admit_explicit_paper_and_refuse_unacknowledged_live():
    df, value = _market(120, seed=3)
    rows = _hl_candles(df)
    obs = observations(df, value, bar_endpoint_offset_ms=1, cutoff_ms=int(df["timestamp"].iloc[-1]) + HOUR)
    market = {"version": 4, "snapshot_id": "feed/1", "frames": {"BTC|1h": {"ready": True, "rows": rows}},
              "observations": {"BTC|open_interest": dict(obs, coin="BTC")}}
    shared = _CHECK_HL.build_shared_signal_state("BTC", "1h", market=market)
    for slot in ({"id": "solo", "strategy": NAME},
                 {"id": "open", "strategy": "breakout", "open_strategy": NAME,
                  "close_strategies": "time_stop"},
                 {"id": "close", "strategy": "breakout", "open_strategy": "breakout",
                  "close_strategies": NAME}):
        with pytest.raises(ValueError, match="allow_no_edge"):
            _CHECK_HL.evaluate_signal_slot(shared, slot)
    peer = _CHECK_HL.evaluate_signal_slot(shared, {"id": "peer", "strategy": "breakout"})
    assert peer["strategy"] == "breakout"
    paper = _CHECK_HL.evaluate_signal_slot(shared, {"id": "paper", "strategy": NAME, "mode_args": ["--mode=paper"]})
    acked = _CHECK_HL.evaluate_signal_slot(
        shared, {"id": "acked", "strategy": NAME, "mode_args": ["--mode=live"], "allow_no_edge": True})
    assert paper["strategy"] == acked["strategy"] == NAME
    assert (paper["signal"], paper["price"]) == (acked["signal"], acked["price"])
    fallback = _CHECK_HL.evaluate_signal_slot(
        shared, {"id": "fallback", "strategy": "breakout", "open_strategy": "breakout",
                 "close_strategies": NAME, "mode_args": ["--mode", "paper"]})
    assert fallback["close_strategies"] == [NAME]
