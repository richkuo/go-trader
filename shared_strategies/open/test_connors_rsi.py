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


def _seeded_wilder(changes, period):
    gains = [max(c, 0.0) for c in changes]
    losses = [max(-c, 0.0) for c in changes]
    g = sum(gains[:period]) / period
    l = sum(losses[:period]) / period
    for k in range(period, len(changes)):
        g = g + (gains[k] - g) / period
        l = l + (losses[k] - l) / period
    return 50.0 if g + l == 0 else 100.0 * g / (g + l)


def _streak_at(closes, j):
    count = 0
    direction = 0
    for k in range(CAP):
        d = closes[j - k] - closes[j - k - 1]
        s = 1 if d > 0 else (-1 if d < 0 else 0)
        if s == 0 or (direction and s != direction):
            break
        direction = s
        count += 1
    return direction * count


def reference_crsi(closes, i, p):
    price_changes = [closes[j] - closes[j - 1] for j in range(i - SPAN + 1, i + 1)]
    streak_changes = [_streak_at(closes, j) - _streak_at(closes, j - 1)
                      for j in range(i - SPAN + 1, i + 1)]
    rets = [closes[j] / closes[j - 1] - 1.0 for j in range(i - p["rank_window"], i + 1)]
    latest, prior = rets[-1], rets[:-1]
    rank = 100.0 * (sum(r < latest for r in prior) + 0.5 * sum(r == latest for r in prior)) / len(prior)
    price_rsi = _seeded_wilder(price_changes, p["price_period"])
    streak_rsi = _seeded_wilder(streak_changes, p["streak_period"])
    return price_rsi, _streak_at(closes, i), streak_rsi, rank, (price_rsi + streak_rsi + rank) / 3.0


def test_registered_no_edge_futures_and_hidden():
    entry = _FUTURES.STRATEGY_REGISTRY[NAME]
    assert (entry["edge_status"], entry["edge_source"], entry["edge_ref"]) == (
        "no_edge", "study_fail", "backtest/candidates/connors_rsi_reversion_1645/REPORT.md")
    assert entry["default_params"] == DEFAULTS
    assert NAME not in _FUTURES.list_strategies()
    assert NAME not in _FUTURES.DISCOVERY_STRATEGY_REGISTRY
    assert NAME not in _SPOT.STRATEGY_REGISTRY


def test_independent_reference_matches_every_recorded_value_and_entry():
    df = market()
    out = run(df)
    closes = df["close"].tolist()
    need = need_for(DEFAULTS["rank_window"])
    entries = 0
    for i in range(need - 2, len(df)):
        price_rsi, streak, streak_rsi, rank, crsi = reference_crsi(closes, i, DEFAULTS)
        row = out.iloc[i]
        assert row["crsi_price_rsi"] == pytest.approx(price_rsi, rel=1e-9, abs=1e-9)
        assert row["crsi_streak"] == streak
        assert row["crsi_streak_rsi"] == pytest.approx(streak_rsi, rel=1e-9, abs=1e-9)
        assert row["crsi_rank"] == rank
        assert row["crsi"] == pytest.approx(crsi, rel=1e-9, abs=1e-9)
        if i < need - 1:
            assert row["crsi_reason"] == "insufficient_history"
            continue
        prev = out["crsi"].iloc[i - 1]
        assert row["crsi_prev"] == prev
        expected = 0
        if prev < 10.0 <= row["crsi"]:
            expected = 1
        elif prev > 90.0 >= row["crsi"]:
            expected = -1
        assert int(row["signal"]) == expected
        entries += expected != 0
        assert row["crsi_eval_ts_ms"] == df.index[i].value // 1_000_000
    assert entries > 0
    assert (out["signal"].iloc[: need - 1] == 0).all()
    assert df.equals(market())


def _same(a, b):
    for col in COLS:
        x, y = a[col], b[col]
        if isinstance(x, float) and np.isnan(x):
            assert isinstance(y, float) and np.isnan(y), col
        else:
            assert x == y, (col, x, y)


@pytest.mark.parametrize("params", [None, {"price_period": 10, "streak_period": 10, "rank_window": 150,
                                           "oversold": 35.0, "overbought": 65.0}])
def test_full_series_prefix_and_bounded_tail_agree(params):
    df = market(380, seed=11)
    full = run(df, params)
    p = {**DEFAULTS, **(params or {})}
    need = need_for(p["rank_window"])
    assert need <= 200
    assert (full["signal"] != 0).sum() > 0
    for i in range(len(df)):
        _same(full.iloc[i], run(df.iloc[: i + 1], params).iloc[-1])
        if i + 1 >= need:
            _same(full.iloc[i], run(df.iloc[max(0, i - 199): i + 1], params).iloc[-1])
    _same(full.iloc[-1], run(df.iloc[len(df) - need:], params).iloc[-1])
    too_short = run(df.iloc[len(df) - need + 1:], params).iloc[-1]
    assert too_short["crsi_reason"] == "insufficient_history"
    assert int(too_short["signal"]) == 0


def test_constant_price_is_neutral_and_never_enters():
    out = run(frame([100.0] * 160))
    last = out.iloc[-1]
    assert last["crsi_price_rsi"] == 50.0
    assert last["crsi_streak"] == 0.0
    assert last["crsi_streak_rsi"] == 50.0
    assert last["crsi_rank"] == 50.0
    assert last["crsi"] == 50.0
    assert last["crsi_reason"] == "no_cross"
    assert (out["signal"] == 0).all()


def test_rising_and_falling_series_are_mirrors_with_capped_streak_and_tied_ranks():
    up = run(frame([2.0 ** k for k in range(160)])).iloc[-1]
    assert up["crsi_price_rsi"] == 100.0
    assert up["crsi_streak"] == CAP
    assert up["crsi_streak_rsi"] == 50.0
    assert up["crsi_rank"] == 50.0
    assert up["crsi"] == pytest.approx(200.0 / 3.0)
    down = run(frame([2.0 ** -k for k in range(160)])).iloc[-1]
    assert down["crsi_price_rsi"] == 0.0
    assert down["crsi_streak"] == -CAP
    assert down["crsi_streak_rsi"] == 50.0
    assert down["crsi_rank"] == 50.0
    assert down["crsi"] == pytest.approx(100.0 / 3.0)
    assert up["crsi"] + down["crsi"] == pytest.approx(100.0)


def test_unchanged_close_resets_streak():
    closes = [100.0 + (k % 7) for k in range(150)] + [99.0, 101.0, 102.0, 103.0, 103.0]
    out = run(frame(closes))
    assert out["crsi_streak"].iloc[-2] == 3.0
    assert out["crsi_streak"].iloc[-1] == 0.0


def _first_entry(out, side):
    hits = np.flatnonzero(out["signal"].to_numpy() == side)
    assert len(hits) > 0
    return int(hits[0])


def test_threshold_boundaries_are_inclusive_on_the_current_bar_and_strict_on_the_previous():
    df = market()
    base = run(df)
    crsi = base["crsi"].to_numpy()
    valid = base["crsi_valid"].to_numpy()
    i = next(k for k in range(1, len(df)) if valid[k] and crsi[k - 1] < crsi[k] < 50.0 and crsi[k - 1] > 0)
    at_current = run(df, {"oversold": float(crsi[i])})
    assert at_current["signal"].iloc[i] == 1
    assert at_current["crsi_reason"].iloc[i] == "entry_long"
    at_previous = run(df, {"oversold": float(crsi[i - 1])})
    assert at_previous["signal"].iloc[i] == 0
    j = next(k for k in range(1, len(df)) if valid[k] and crsi[k - 1] > crsi[k] > 50.0 and crsi[k - 1] < 100)
    at_current = run(df, {"overbought": float(crsi[j])})
    assert at_current["signal"].iloc[j] == -1
    assert at_current["crsi_reason"].iloc[j] == "entry_short"
    at_previous = run(df, {"overbought": float(crsi[j - 1])})
    assert at_previous["signal"].iloc[j] == 0


def test_long_and_short_entries_cover_both_sides_with_reasons():
    out = run(market())
    longs = out[out["signal"] == 1]
    shorts = out[out["signal"] == -1]
    assert len(longs) > 0 and len(shorts) > 0
    assert (longs["crsi_reason"] == "entry_long").all()
    assert (shorts["crsi_reason"] == "entry_short").all()
    assert (longs["crsi_prev"] < 10.0).all() and (longs["crsi"] >= 10.0).all()
    assert (shorts["crsi_prev"] > 90.0).all() and (shorts["crsi"] <= 90.0).all()
    assert set(out["crsi_reason"]) <= {"entry_long", "entry_short", "no_cross", "below_oversold",
                                       "above_overbought", "insufficient_history"}


@pytest.mark.parametrize("value,reason", [
    (float("nan"), "nonfinite_input"),
    (float("inf"), "nonfinite_input"),
    (0.0, "nonpositive_close"),
    (-5.0, "nonpositive_close"),
])
def test_bad_close_holds_for_every_decision_whose_span_includes_it(value, reason):
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


def test_missing_column_and_timestamp_order_hold():
    missing = run(frame([100.0] * 160).drop(columns=["close"]))
    assert (missing["signal"] == 0).all()
    assert missing["crsi_reason"].iloc[-1] == "missing_columns:close"
    df = market(200)
    idx = list(df.index)
    idx[-1] = idx[-2]
    df.index = pd.DatetimeIndex(idx)
    row = run(df).iloc[-1]
    assert row["crsi_reason"] == "timestamp_order"
    assert int(row["signal"]) == 0
    with_col = market(200).reset_index(drop=True)
    with_col["timestamp"] = [1_000 * i for i in range(200)]
    with_col.loc[199, "timestamp"] = 3_000
    assert run(with_col).iloc[-1]["crsi_reason"] == "timestamp_order"


@pytest.mark.parametrize("params", [
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
])
def test_invalid_parameters_raise(params):
    with pytest.raises(ValueError):
        run(market(200), params)


def test_integral_float_periods_accepted():
    out = run(market(200), {"price_period": 3.0, "rank_window": 100.0})
    assert out["crsi_reason"].iloc[-1] in {"no_cross", "below_oversold", "above_overbought",
                                           "entry_long", "entry_short"}


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


def test_composer_matches_full_series_on_bounded_windows():
    df = market(360, seed=7)
    full = run(df)
    need = need_for(DEFAULTS["rank_window"])
    for i in range(need - 1, len(df)):
        evaluation, decision = _compose(df.iloc[max(0, i - 199): i + 1], "")
        assert evaluation.open_signal == int(full["signal"].iloc[i])
        assert decision["signal"] == int(full["signal"].iloc[i])


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


