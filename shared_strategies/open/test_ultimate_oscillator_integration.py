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
    "_uo_futures_strategies_test", os.path.join(_HERE, "futures", "strategies.py"))
_SPOT = load_module(
    "_uo_spot_strategies_test", os.path.join(_HERE, "spot", "strategies.py"))
_CHECK_HL = load_module(
    "_uo_check_hyperliquid_test", os.path.join(_ROOT, "shared_scripts", "check_hyperliquid.py"))

from close_registry_loader import evaluate as close_evaluate
from strategy_composition import evaluate_open_close, finalize_decision

NAME = "ultimate_oscillator"
DEFAULTS = {"short_window": 7, "mid_window": 14, "long_window": 28,
            "bullish_threshold": 70.0, "bearish_threshold": 30.0, "breakout_lookback": 20}
NEED = 29
TIERS = {"tp_tiers": [{"profit_pct": 0.03, "close_fraction": 1.0}]}
COLS = ["signal", "uo", "uo_avg_short", "uo_avg_mid", "uo_avg_long", "uo_bp_sum_long",
        "uo_tr_sum_long", "donchian_upper", "donchian_lower", "uo_raw_event", "uo_valid",
        "uo_reason", "uo_reason_code"]


def run(df, params=None):
    return _FUTURES.apply_strategy(NAME, df, params)


def market(n=500, seed=1664, freq="1h", start="2025-01-01", base=200.0):
    rng = np.random.RandomState(seed)
    t = np.arange(n)
    drift = np.where((t // 60) % 2 == 0, 0.45, -0.45)
    close = base + np.cumsum(drift + rng.randn(n) * 0.6)
    open_ = np.concatenate([[close[0]], close[:-1]])
    span = np.abs(rng.randn(n)) * 0.5 + 0.1
    df = pd.DataFrame({"open": open_, "high": np.maximum(open_, close) + span,
                       "low": np.minimum(open_, close) - span, "close": close,
                       "volume": np.ones(n)})
    df.index = pd.date_range(start, periods=n, freq=freq)
    return df


def trend(n, step, seed):
    rng = np.random.RandomState(seed)
    close = 200.0 + step * np.arange(n) + rng.randn(n) * 0.4
    open_ = np.concatenate([[close[0]], close[:-1]])
    df = pd.DataFrame({"open": open_, "high": np.maximum(open_, close) + 0.3,
                       "low": np.minimum(open_, close) - 0.3, "close": close,
                       "volume": np.ones(n)})
    df.index = pd.date_range("2025-01-01", periods=n, freq="4h")
    return df


def breakout_frame():
    rows = []
    for i in range(40):
        c = 100.0 + (0.8 if i % 2 else -0.8)
        rows.append((c, 101.5, 98.5, c))
    rows.append((100.8, 102.2, 100.0, 102.0))
    rows.append((102.0, 104.5, 101.9, 104.0))
    df = pd.DataFrame(rows, columns=["open", "high", "low", "close"])
    df["volume"] = 1.0
    df.index = pd.date_range("2025-01-01", periods=len(df), freq="4h")
    return df


def reference(df, i, p):
    h, l, c = (df[k].to_numpy(dtype=float) for k in ("high", "low", "close"))

    def avg(w):
        bp = tr = 0.0
        for k in range(i - w + 1, i + 1):
            floor = min(l[k], c[k - 1])
            bp += c[k] - floor
            tr += max(h[k], c[k - 1]) - floor
        return bp / tr

    uo = 100.0 * (4 * avg(p["short_window"]) + 2 * avg(p["mid_window"]) + avg(p["long_window"])) / 7
    L = p["breakout_lookback"]
    upper, lower = max(h[i - L:i]), min(l[i - L:i])
    prev_upper, prev_lower = max(h[i - 1 - L:i - 1]), min(l[i - 1 - L:i - 1])
    event = 1 if (c[i] > upper and c[i - 1] <= prev_upper) else (
        -1 if (c[i] < lower and c[i - 1] >= prev_lower) else 0)
    return uo, upper, lower, event


def same(a, b):
    for col in COLS:
        x, y = a[col], b[col]
        if isinstance(x, float) and np.isnan(x):
            assert isinstance(y, float) and np.isnan(y), col
        else:
            assert x == y, (col, x, y)


def test_registration_is_futures_only_bidirectional_hidden_no_edge():
    entry = _FUTURES.STRATEGY_REGISTRY[NAME]
    assert (entry["edge_status"], entry["edge_source"]) == ("no_edge", "study_inconclusive")
    assert entry["edge_ref"] == "backtest/candidates/ultimate_oscillator_1664/REPORT.md"
    assert os.path.isfile(os.path.join(_ROOT, entry["edge_ref"]))
    assert entry["default_params"] == DEFAULTS
    assert NAME not in _FUTURES.list_strategies()
    assert NAME not in _FUTURES.DISCOVERY_STRATEGY_REGISTRY
    assert NAME not in _SPOT.STRATEGY_REGISTRY
    reg = _FUTURES._registry
    assert NAME in reg.DISCOVERY_HIDDEN_STRATEGIES
    assert NAME in reg.short_entry_strategies()
    assert reg.STRATEGIES[NAME]["platforms"] == ("futures",)


def test_oscillator_and_events_match_an_independent_calculation():
    df = market()
    out = run(df)
    valid = np.flatnonzero(out["uo_valid"].to_numpy())
    assert valid[0] == NEED - 1 and len(valid) == len(df) - NEED + 1
    for i in valid:
        uo, upper, lower, event = reference(df, i, DEFAULTS)
        row = out.iloc[i]
        assert row["uo"] == pytest.approx(uo, rel=1e-12, abs=1e-12)
        assert 0.0 <= row["uo"] <= 100.0
        assert row["donchian_upper"] == upper and row["donchian_lower"] == lower
        assert row["uo_raw_event"] == event
        expected = 1 if event == 1 and uo > 70.0 else (-1 if event == -1 and uo < 30.0 else 0)
        assert row["signal"] == expected
    donchian = _FUTURES.apply_strategy("donchian_breakout", df, {"entry_period": 20, "exit_period": 10})
    assert (donchian["signal"].to_numpy()[valid] == out["uo_raw_event"].to_numpy()[valid]).all()
    assert (out["signal"] == 1).sum() > 0 and (out["signal"] == -1).sum() > 0


def test_threshold_equality_and_opposing_oscillator_hold():
    df = market()
    out = run(df)
    k = int(np.flatnonzero((out["uo_raw_event"] == 1).to_numpy() & out["uo_valid"].to_numpy())[0])
    u = float(out["uo"].iloc[k])
    at = run(df, {**DEFAULTS, "bullish_threshold": u, "bearish_threshold": u / 2}).iloc[k]
    assert (at["signal"], at["uo_reason"]) == (0, "unconfirmed_long")
    below = run(df, {**DEFAULTS, "bullish_threshold": float(np.nextafter(u, 0)),
                     "bearish_threshold": u / 2}).iloc[k]
    assert (below["signal"], below["uo_reason"]) == (1, "entry_long")
    opposed = run(df, {**DEFAULTS, "bearish_threshold": float(np.nextafter(u, 100)),
                       "bullish_threshold": (u + 100) / 2}).iloc[k]
    assert (opposed["signal"], opposed["uo_reason"]) == (0, "unconfirmed_long")

    j = int(np.flatnonzero((out["uo_raw_event"] == -1).to_numpy() & out["uo_valid"].to_numpy())[0])
    v = float(out["uo"].iloc[j])
    at = run(df, {**DEFAULTS, "bearish_threshold": v, "bullish_threshold": (v + 100) / 2}).iloc[j]
    assert (at["signal"], at["uo_reason"]) == (0, "unconfirmed_short")
    above = run(df, {**DEFAULTS, "bearish_threshold": float(np.nextafter(v, 100)),
                     "bullish_threshold": (v + 100) / 2}).iloc[j]
    assert (above["signal"], above["uo_reason"]) == (-1, "entry_short")


def test_failed_confirmation_is_never_released_by_a_later_bar():
    df = breakout_frame()
    out = run(df)
    assert out["uo_raw_event"].iloc[40] == 1
    assert out["uo_raw_event"].iloc[41] == 0
    assert out["close"].iloc[41] > out["donchian_upper"].iloc[41]
    u40, u41 = out["uo"].iloc[40], out["uo"].iloc[41]
    assert u41 > u40
    gate = {**DEFAULTS, "bullish_threshold": (u40 + u41) / 2, "bearish_threshold": 1.0}
    held = run(df, gate)
    assert (held["signal"].iloc[40], held["uo_reason"].iloc[40]) == (0, "unconfirmed_long")
    assert (held["signal"].iloc[41], held["uo_reason"].iloc[41]) == (0, "no_breakout")
    assert (held["signal"] == 0).all()
    passed = run(df, {**DEFAULTS, "bullish_threshold": float(np.nextafter(u40, 0)), "bearish_threshold": 1.0})
    assert passed["signal"].iloc[40] == 1 and passed["signal"].iloc[41] == 0


def test_rising_falling_and_constant_series():
    up = run(trend(300, 0.6, 1))
    down = run(trend(300, -0.6, 2))
    assert (up["signal"] == 1).sum() > 0 and (up["signal"] == -1).sum() == 0
    assert (down["signal"] == -1).sum() > 0 and (down["signal"] == 1).sum() == 0
    flat = pd.DataFrame({"open": 100.0, "high": 101.0, "low": 99.0, "close": 100.0, "volume": 1.0},
                        index=pd.date_range("2025-01-01", periods=80, freq="1h"))
    out = run(flat)
    valid = out[out["uo_valid"]]
    assert len(valid) == 80 - NEED + 1
    assert (valid["uo"] == 50.0).all() and (valid["uo_reason"] == "no_breakout").all()
    still = flat.assign(high=100.0, low=100.0)
    zero = run(still)
    assert (zero["uo_reason"].iloc[NEED - 1:] == "zero_range").all()
    assert (zero["signal"] == 0).all() and not zero["uo_valid"].any()


@pytest.mark.parametrize("col,offset,value,reason", [
    ("high", 0, np.nan, "nonfinite_input"),
    ("close", 0, np.inf, "nonfinite_input"),
    ("close", -1, np.nan, "nonfinite_input"),
    ("low", 0, 0.0, "nonpositive_input"),
    ("close", 0, -1.0, "nonpositive_input"),
    ("close", 0, 1e9, "inconsistent_candle"),
    ("low", -5, 1e9, "inconsistent_candle"),
])
def test_bad_candle_holds_its_whole_dependency_span_then_recovers(col, offset, value, reason):
    df = market(200)
    clean = run(df)
    j = 120 + offset
    bad = df.copy()
    bad.iloc[j, bad.columns.get_loc(col)] = value
    out = run(bad)
    for i in range(j, j + NEED):
        assert not out["uo_valid"].iloc[i]
        assert out["signal"].iloc[i] == 0
        assert out["uo_reason"].iloc[i] == reason
    for i in list(range(NEED - 1, j)) + list(range(j + NEED, len(df))):
        same(clean.iloc[i], out.iloc[i])


def test_timestamp_defects_hold_and_both_timestamp_forms_agree():
    df = market(200)
    clean = run(df)
    dup = df.copy()
    idx = list(dup.index)
    idx[120] = idx[119]
    dup.index = pd.DatetimeIndex(idx)
    out = run(dup)
    assert (out["uo_reason"].iloc[120:120 + NEED - 1] == "timestamp_order").all()
    assert out["uo_reason"].iloc[120 + NEED - 1] == "cadence_gap"
    assert out["uo_valid"].iloc[120 + NEED]
    back = df.copy()
    idx = list(back.index)
    idx[120] = idx[118]
    back.index = pd.DatetimeIndex(idx)
    assert run(back)["uo_reason"].iloc[120] == "timestamp_order"
    gap = df.drop(df.index[120])
    out = run(gap)
    assert (out["uo_reason"].iloc[120:120 + NEED - 1] == "cadence_gap").all()
    assert out["uo_valid"].iloc[120 + NEED - 1]
    assert (out["signal"].iloc[120:120 + NEED - 1] == 0).all()
    nat = df.copy()
    idx = list(nat.index)
    idx[120] = pd.NaT
    nat.index = pd.DatetimeIndex(idx)
    assert (run(nat)["uo_reason"].iloc[120:120 + NEED] == "timestamp_missing").all()

    col = df.reset_index(drop=True)
    col["timestamp"] = (df.index.asi8 // 1_000_000).astype("int64")
    by_col = run(col)
    for i in range(len(df)):
        same(clean.iloc[i], by_col.iloc[i])
    col_bad = col.copy()
    col_bad["timestamp"] = col_bad["timestamp"].astype(float)
    col_bad.loc[120, "timestamp"] = np.nan
    assert run(col_bad)["uo_reason"].iloc[120] == "timestamp_missing"
    none = run(df.reset_index(drop=True))
    assert (none["uo_reason"] == "missing_timestamps").all() and (none["signal"] == 0).all()
    missing = run(df.drop(columns=["close"]))
    assert (missing["uo_reason"] == "missing_columns:close").all()


def test_zero_range_overflow_and_history_boundaries():
    df = market(200)
    last = float(df["close"].iloc[-1])
    flat = pd.DataFrame({"open": last, "high": last, "low": last, "close": last, "volume": 1.0},
                        index=pd.date_range(df.index[-1] + pd.Timedelta(hours=1), periods=7, freq="1h"))
    out = run(pd.concat([df, flat]))
    assert out["uo_reason"].iloc[-1] == "zero_range"
    assert out["uo_tr_sum_short"].iloc[-1] == 0.0 and out["uo_tr_sum_long"].iloc[-1] > 0
    huge = pd.DataFrame({"open": 1.5e308, "high": 1.7e308, "low": 1e300, "close": 1.5e308,
                         "volume": 1.0}, index=pd.date_range("2025-01-01", periods=40, freq="1h"))
    big = run(huge)
    assert (big["uo_reason"].iloc[NEED - 1:] == "nonfinite_computed").all()
    assert (big["signal"] == 0).all()
    exact = run(df.iloc[-NEED:])
    assert exact["uo_valid"].iloc[-1]
    same(run(df).iloc[-1], exact.iloc[-1])
    short = run(df.iloc[-(NEED - 1):])
    assert (short["uo_reason"] == "insufficient_history").all()


@pytest.mark.parametrize("params,need", [
    (None, NEED),
    ({**DEFAULTS, "long_window": 40, "breakout_lookback": 40}, 42),
    ({**DEFAULTS, "long_window": 198, "breakout_lookback": 10}, 199),
])
def test_full_series_every_prefix_and_200_bar_window_agree(params, need):
    df = market(480, seed=7)
    full = run(df, params)
    assert (full["signal"] != 0).sum() > 0 or need == 199
    for i in range(len(df)):
        same(full.iloc[i], run(df.iloc[: i + 1], params).iloc[-1])
        if i + 1 >= need:
            same(full.iloc[i], run(df.iloc[max(0, i - 199): i + 1], params).iloc[-1])


def test_appending_a_smaller_future_step_never_changes_earlier_decisions():
    df = market(260, freq="4h")
    base = run(df)
    extra = market(40, seed=99, freq="1h", start=str(df.index[-1] + pd.Timedelta(hours=1)))
    full = run(pd.concat([df, extra]))
    for i in range(len(df)):
        same(base.iloc[i], full.iloc[i])
    assert (full["uo_reason"].iloc[len(df):len(df) + NEED - 2] == "cadence_gap").all()
    assert full["uo_valid"].iloc[len(df) + NEED - 2]


@pytest.mark.parametrize("params", [
    {"short_window": True},
    {"short_window": 7.5},
    {"short_window": float("nan")},
    {"short_window": 0},
    {"mid_window": 7},
    {"long_window": 14},
    {"long_window": 199},
    {"breakout_lookback": 198},
    {"breakout_lookback": 0},
    {"bullish_threshold": True},
    {"bullish_threshold": float("inf")},
    {"bearish_threshold": float("nan")},
    {"bearish_threshold": 70.0},
    {"bullish_threshold": 100.0},
    {"bearish_threshold": 0.0},
    {"bullish_threshold": "70"},
])
def test_invalid_configuration_raises_before_evaluation(params):
    with pytest.raises(ValueError):
        run(market(60), {**DEFAULTS, **params})


def test_integral_float_windows_are_accepted():
    out = run(market(60), {**DEFAULTS, "short_window": 7.0, "breakout_lookback": 20.0})
    same(run(market(60)).iloc[-1], out.iloc[-1])


def _compose(df, position_side, position_ctx=None):
    evaluation = evaluate_open_close(
        _FUTURES.apply_strategy, _FUTURES.get_strategy, df, NAME, NAME,
        ["tiered_tp_pct"], position_side, None, position_ctx,
        close_evaluate=close_evaluate,
        market_ctx={"mark_price": float(df["close"].iloc[-1])},
        close_params_by_name={"tiered_tp_pct": TIERS},
    )
    return evaluation, finalize_decision(evaluation, position_side)


def _entry_slice():
    df = market()
    out = run(df)
    i = int(np.flatnonzero(out["signal"].to_numpy() == 1)[0])
    return df.iloc[: i + 1], float(df["close"].iloc[i])


def test_composer_entry_suppression_and_close_priority_with_invalid_entry_input():
    window, mark = _entry_slice()
    _, flat = _compose(window, "")
    assert flat["signal"] == 1 and flat["open_action"] == "long"
    held = {"side": "long", "bars_held": 3, "avg_cost": mark, "current_quantity": 1.0}
    _, dup = _compose(window, "long", held)
    assert dup["signal"] == 0 and dup["close_fraction"] == 0.0
    short_ctx = {"side": "short", "bars_held": 3, "avg_cost": mark, "current_quantity": 1.0}
    _, opposite = _compose(window, "short", short_ctx)
    assert opposite["signal"] == 0 and opposite["close_fraction"] == 0.0

    win_ctx = {"side": "long", "bars_held": 3, "avg_cost": mark / 1.04, "current_quantity": 1.0}
    _, both = _compose(window, "long", win_ctx)
    assert both["close_fraction"] == 1.0 and both["signal"] == -1
    bad = window.copy()
    bad.iloc[-1, bad.columns.get_loc("high")] = np.nan
    evaluation, decision = _compose(bad, "long", win_ctx)
    assert evaluation.open_result_df["uo_reason"].iloc[-1] == "nonfinite_input"
    assert decision["close_fraction"] == 1.0 and decision["signal"] == -1
    gap = window.drop(window.index[-5])
    evaluation, decision = _compose(gap, "long", win_ctx)
    assert evaluation.open_result_df["uo_reason"].iloc[-1] == "cadence_gap"
    assert decision["close_fraction"] == 1.0 and decision["signal"] == -1
    short_win = {"side": "short", "bars_held": 3, "avg_cost": mark * 1.04, "current_quantity": 1.0}
    _, short_close = _compose(window.drop(columns=["low"]), "short", short_win)
    assert short_close["close_fraction"] == 1.0 and short_close["signal"] == 1


def test_composer_matches_full_series_on_bounded_windows():
    df = market(360, seed=11)
    full = run(df)
    for i in range(NEED - 1, len(df)):
        evaluation, decision = _compose(df.iloc[max(0, i - 199): i + 1], "")
        assert evaluation.open_signal == int(full["signal"].iloc[i])
        assert decision["signal"] == int(full["signal"].iloc[i])


def _hl_candles(df):
    return [[int(ts.value // 1_000_000), r.open, r.high, r.low, r.close, r.volume]
            for ts, r in df.iterrows()]


def test_hyperliquid_slot_admission_and_numeric_diagnostics():
    df = _CHECK_HL._make_dataframe(_hl_candles(market(200)))
    shared = _CHECK_HL.build_shared_signal_state("BTC", "1h", df=df)
    deps = _CHECK_HL._signal_check_deps()
    for slot in ({"id": "missing", "strategy": NAME},
                 {"id": "live", "strategy": NAME, "mode_args": ["--mode=live"]},
                 {"id": "open", "strategy": "breakout", "open_strategy": NAME,
                  "close_strategies": "tiered_tp_pct", "mode_args": ["--mode=live"]},
                 {"id": "fallback", "strategy": "breakout", "open_strategy": "breakout",
                  "close_strategies": NAME, "mode_args": ["--mode=live"]}):
        with pytest.raises(ValueError, match="allow_no_edge"):
            _CHECK_HL.evaluate_signal_slot(shared, slot, deps=deps)
    paper = _CHECK_HL.evaluate_signal_slot(
        shared, {"id": "paper", "strategy": NAME, "mode_args": ["--mode=paper"]}, deps=deps)
    acked = _CHECK_HL.evaluate_signal_slot(
        shared, {"id": "acked", "strategy": NAME, "mode_args": ["--mode=live"], "allow_no_edge": True},
        deps=deps)
    assert paper["strategy"] == acked["strategy"] == NAME
    assert paper["signal"] == acked["signal"]
    for key in ("uo_reason_code", "uo_valid", "uo_bullish_threshold", "uo_bearish_threshold"):
        assert key in paper["indicators"]
    fallback = _CHECK_HL.evaluate_signal_slot(
        shared, {"id": "fb", "strategy": "breakout", "open_strategy": "breakout",
                 "close_strategies": NAME, "mode_args": ["--mode", "paper"]}, deps=deps)
    assert fallback["close_strategies"] == [NAME]
