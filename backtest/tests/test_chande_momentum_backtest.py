import math
import os
import sys

import numpy as np
import pandas as pd
import pytest

_REPO = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
if os.path.join(_REPO, "shared_tools") not in sys.path:
    sys.path.insert(0, os.path.join(_REPO, "shared_tools"))

from atr import ensure_atr_indicator
from backtester import Backtester
from close_registry_loader import evaluate as close_evaluate
from parity_diff import _ENGINE_SLIPPAGE_PCT, ParityConfig, compute_parity_frame, summarize
from registry_loader import load_registry
from strategy_composition import evaluate_open_close, finalize_decision

NAME = "chande_momentum"
H = 3_600_000
PARAMS = {"period": 5, "threshold": 25.0, "expected_interval_ms": H}
TP = {"name": "tiered_tp_pct", "params": {"tp_tiers": [{"profit_pct": 0.03, "close_fraction": 1.0}]}}
COLS = ["signal", "cmo_reason", "cmo_valid", "cmo", "cmo_prev", "cmo_gain_sum", "cmo_loss_sum",
        "cmo_close", "cmo_eval_ts_ms"]


def _reg():
    return load_registry("futures")


def _apply(df, params=PARAMS):
    return _reg().apply_strategy(NAME, df, dict(params))


def _frame(closes, freq="1h", start="2026-01-01", index=None):
    closes = np.asarray(closes, dtype=float)
    opens = np.concatenate([[closes[0]], closes[:-1]])
    with np.errstate(invalid="ignore"):
        high = np.fmax(opens, closes) + 0.5
        low = np.fmin(opens, closes) - 0.5
    df = pd.DataFrame({"open": opens, "high": high, "low": low, "close": closes, "volume": 1.0})
    df.index = index if index is not None else pd.date_range(start, periods=len(df), freq=freq)
    return df


def _walk(n, seed=1661, base=100.0):
    rng = np.random.RandomState(seed)
    t = np.arange(n)
    return np.round(base + 6.0 * np.sin(t / 9.0) + np.cumsum(rng.randn(n) * 0.8), 2)


def _ref(closes, period, i):
    ups = sum(max(closes[j] - closes[j - 1], 0.0) for j in range(i - period + 1, i + 1))
    downs = sum(max(closes[j - 1] - closes[j], 0.0) for j in range(i - period + 1, i + 1))
    osc = 0.0 if ups + downs == 0 else 100.0 * (ups - downs) / (ups + downs)
    return ups, downs, osc


def _same(a, b):
    for col in COLS:
        x, y = a[col], b[col]
        if isinstance(x, float) and isinstance(y, float) and math.isnan(x) and math.isnan(y):
            continue
        assert x == y, (col, x, y)


def test_registry_values_match_an_independent_calculation_and_the_sma_rsi_identity():
    closes = _walk(120)
    out = _apply(_frame(closes), dict(PARAMS, period=9))
    for i in range(10, 120):
        ups, downs, osc = _ref(list(closes), 9, i)
        assert out["cmo_gain_sum"].iloc[i] == ups
        assert out["cmo_loss_sum"].iloc[i] == downs
        assert out["cmo"].iloc[i] == pytest.approx(osc, abs=1e-12)
        assert out["cmo_prev"].iloc[i] == out["cmo"].iloc[i - 1]
        if ups + downs:
            sma_rsi = 100.0 * (ups / 9) / ((ups / 9) + (downs / 9))
            assert out["cmo"].iloc[i] == pytest.approx(2.0 * sma_rsi - 100.0, abs=1e-9)
        assert out["cmo_close"].iloc[i] == closes[i]
    wilder = _reg().apply_strategy("rsi", _frame(closes), {"period": 9})
    assert not np.allclose(wilder["rsi"].iloc[30:], (out["cmo"].iloc[30:] + 100.0) / 2.0)


def test_warmup_rising_falling_and_mirrored_crossings():
    closes = [100.0] * 10 + [100.0 + k for k in range(1, 9)] + [108.0 - k for k in range(1, 15)]
    out = _apply(_frame(closes))
    assert (out["cmo_reason"].iloc[:6] == "insufficient_history").all()
    assert not out["cmo_valid"].iloc[:6].any() and out["cmo_valid"].iloc[6]
    assert (out["signal"].iloc[:6] == 0).all()
    longs = list(np.flatnonzero(out["signal"].to_numpy() == 1))
    shorts = list(np.flatnonzero(out["signal"].to_numpy() == -1))
    assert longs == [10]
    assert out["cmo"].iloc[10] == 100.0 and out["cmo_prev"].iloc[10] == 0.0
    assert out["cmo_reason"].iloc[10] == "entry_long"
    assert len(shorts) == 1
    s = shorts[0]
    assert out["cmo"].iloc[s] < -25.0 <= out["cmo_prev"].iloc[s]
    assert out["cmo_reason"].iloc[s] == "entry_short"


def test_constant_prices_read_zero_and_never_fire():
    out = _apply(_frame([50.0] * 40))
    valid = out["cmo_valid"].to_numpy()
    assert valid[6:].all()
    assert (out["cmo"].iloc[5:] == 0.0).all()
    assert (out["cmo_reason"].iloc[6:] == "flat_price").all()
    assert (out["signal"] == 0).all()


def test_flat_window_then_rise_fires_from_the_neutral_value():
    out = _apply(_frame([50.0] * 12 + [51.0, 52.0]))
    assert out["cmo"].iloc[11] == 0.0 and out["cmo_reason"].iloc[11] == "flat_price"
    assert out["signal"].iloc[12] == 1 and out["cmo"].iloc[12] == 100.0


@pytest.mark.parametrize("sign", [1, -1])
def test_threshold_equality_never_fires_and_a_cross_from_exactly_the_threshold_does(sign):
    closes = [100.0] * 8 + [100.0 - 3 * sign, 100.0 - 3 * sign, 100.0 - 3 * sign,
                            100.0 + 2 * sign, 100.0 + 3 * sign]
    out = _apply(_frame(closes), dict(PARAMS, period=4))
    eq = 11
    assert out["cmo"].iloc[eq] == sign * 25.0
    assert out["cmo_prev"].iloc[eq] == -sign * 100.0
    assert out["signal"].iloc[eq] == 0 and out["cmo_reason"].iloc[eq] == "threshold_boundary"
    assert out["cmo_prev"].iloc[eq + 1] == sign * 25.0
    assert out["cmo"].iloc[eq + 1] == sign * 100.0
    assert out["signal"].iloc[eq + 1] == sign
    assert out["cmo_reason"].iloc[eq + 1] == ("entry_long" if sign > 0 else "entry_short")


def _defect_case(kind, clean, j):
    df = clean.copy()
    idx = df.index.copy()
    if kind == "nonfinite_close":
        df.iloc[j, df.columns.get_loc("close")] = np.nan
    elif kind == "infinite_close":
        df.iloc[j, df.columns.get_loc("close")] = np.inf
    elif kind == "nonpositive_close":
        df.iloc[j, df.columns.get_loc("close")] = 0.0
    elif kind == "negative_close":
        df.iloc[j, df.columns.get_loc("close")] = -1.0
    elif kind == "missing_timestamp":
        vals = list(idx)
        vals[j] = pd.NaT
        df.index = pd.DatetimeIndex(vals)
    elif kind == "duplicate_timestamp":
        vals = list(idx)
        vals[j] = vals[j - 1]
        df.index = pd.DatetimeIndex(vals)
    elif kind == "descending_timestamp":
        vals = list(idx)
        vals[j] = vals[j - 1] - pd.Timedelta(hours=1)
        df.index = pd.DatetimeIndex(vals)
    elif kind == "missing_bar":
        df = df.drop(df.index[j])
    return df


DEFECTS = {
    "nonfinite_close": ("nonfinite_close", 2),
    "infinite_close": ("nonfinite_close", 2),
    "nonpositive_close": ("nonpositive_close", 2),
    "negative_close": ("nonpositive_close", 2),
    "missing_timestamp": ("missing_timestamp", 2),
    "duplicate_timestamp": ("duplicate_timestamp", 2),
    "descending_timestamp": ("descending_timestamp", 2),
    "missing_bar": ("cadence_gap", 1),
}


@pytest.mark.parametrize("kind", sorted(DEFECTS))
def test_each_defect_holds_with_a_reason_and_resumes_once_both_windows_clear(kind):
    period = 5
    clean = _frame(_walk(90, seed=7))
    j = 40
    df = _defect_case(kind, clean, j)
    out = _apply(df)
    reason, offset = DEFECTS[kind]
    resume = j + period + offset
    assert out["cmo_reason"].iloc[j] == reason
    blocked = out.iloc[j:resume]
    assert not blocked["cmo_valid"].any()
    assert (blocked["signal"] == 0).all()
    assert set(blocked["cmo_reason"]) <= {reason, "cadence_gap", "descending_timestamp", "missing_timestamp"}
    assert out["cmo_valid"].iloc[resume:].all()
    assert out["cmo_valid"].iloc[period + 1:j].all()
    ref = _apply(clean)
    shift = 1 if kind == "missing_bar" else 0
    for i in range(resume, len(out)):
        assert out["cmo_gain_sum"].iloc[i] == pytest.approx(
            _ref(list(df["close"]), period, i)[0], abs=1e-9)
        if not shift:
            assert out["cmo"].iloc[i] == ref["cmo"].iloc[i]
            assert out["signal"].iloc[i] == ref["signal"].iloc[i]


def test_cadence_comes_from_the_fixed_interval_and_never_from_the_frame():
    four_hour = _frame(_walk(60), freq="4h")
    held = _apply(four_hour)
    assert not held["cmo_valid"].any() and (held["signal"] == 0).all()
    assert (held["cmo_reason"].iloc[1:] == "cadence_gap").all()
    ok = _apply(four_hour, dict(PARAMS, expected_interval_ms=4 * H))
    assert ok["cmo_valid"].iloc[6:].all()
    hourly = _frame(_walk(60))
    assert np.array_equal(ok["cmo"].to_numpy(), _apply(hourly)["cmo"].to_numpy(), equal_nan=True)


def test_timestamp_column_and_timezone_inputs_match_the_naive_index():
    df = _frame(_walk(80))
    base = _apply(df)
    col = df.reset_index(drop=True)
    col["timestamp"] = df.index.as_unit("ms").asi8
    by_col = _apply(col)
    tz = df.tz_localize("UTC").tz_convert("America/New_York")
    by_tz = _apply(tz)
    for other in (by_col, by_tz):
        for c in ("signal", "cmo", "cmo_valid", "cmo_eval_ts_ms", "cmo_reason"):
            assert np.array_equal(base[c].to_numpy(), other[c].to_numpy(), equal_nan=c != "cmo_reason"
                                  and base[c].dtype.kind == "f")


def test_missing_inputs_hold_without_raising():
    no_close = _frame(_walk(30)).drop(columns=["close"])
    out = _apply(no_close)
    assert (out["signal"] == 0).all() and (out["cmo_reason"] == "missing_columns:close").all()
    no_ts = _frame(_walk(30)).reset_index(drop=True)
    out = _apply(no_ts)
    assert (out["signal"] == 0).all() and (out["cmo_reason"] == "missing_timestamps").all()
    empty = _apply(_frame(_walk(30)).iloc[:0])
    assert len(empty) == 0 and "cmo_reason" in empty.columns
    junk = _frame(_walk(30)).reset_index(drop=True)
    junk["timestamp"] = "not-a-time"
    out = _apply(junk)
    assert (out["signal"] == 0).all() and (out["cmo_reason"] == "missing_timestamp").all()


@pytest.mark.parametrize("bad", [
    {"period": 0}, {"period": 199}, {"period": 20.5}, {"period": True}, {"period": float("nan")},
    {"period": float("inf")}, {"period": "20"},
    {"threshold": 0}, {"threshold": 100}, {"threshold": -10}, {"threshold": float("nan")},
    {"threshold": float("inf")}, {"threshold": True}, {"threshold": "25"},
    {"expected_interval_ms": 0}, {"expected_interval_ms": -H}, {"expected_interval_ms": 1.5},
    {"expected_interval_ms": True}, {"expected_interval_ms": float("nan")}, {"expected_interval_ms": None},
])
def test_invalid_parameters_are_configuration_errors(bad):
    with pytest.raises(ValueError):
        _apply(_frame(_walk(40)), dict(PARAMS, **bad))


def test_period_endpoints_and_the_200_bar_check_window():
    closes = _walk(220, seed=3)
    one = _apply(_frame(closes), dict(PARAMS, period=1))
    assert not one["cmo_valid"].iloc[:2].any() and one["cmo_valid"].iloc[2:].all()
    assert set(one["cmo"].iloc[2:].unique()) <= {-100.0, 0.0, 100.0}
    at_200 = _apply(_frame(closes[:200]), dict(PARAMS, period=198))
    assert list(np.flatnonzero(at_200["cmo_valid"].to_numpy())) == [199]
    at_199 = _apply(_frame(closes[:199]), dict(PARAMS, period=198))
    assert not at_199["cmo_valid"].any()
    assert at_199["cmo_reason"].iloc[-1] == "insufficient_history"
    assert _apply(_frame(closes[:200]), dict(PARAMS, period=198.0))["cmo_valid"].iloc[-1]


def _defective_walk():
    closes = _walk(160, seed=11)
    df = _frame(closes)
    df.iloc[30, df.columns.get_loc("close")] = np.nan
    vals = list(df.index)
    vals[70] = vals[69]
    vals[110] = pd.NaT
    df.index = pd.DatetimeIndex(vals)
    df = df.drop(df.index[130])
    return df


@pytest.mark.parametrize("period", [1, 5, 9])
def test_every_decision_equals_its_prefix_and_its_required_suffix(period):
    df = _defective_walk()
    params = dict(PARAMS, period=period)
    full = _apply(df, params)
    for i in range(len(df)):
        prefix = _apply(df.iloc[: i + 1], params).iloc[-1]
        suffix = _apply(df.iloc[max(0, i - period - 1): i + 1], params).iloc[-1]
        _same(full.iloc[i], prefix)
        _same(full.iloc[i], suffix)


def test_data_outside_the_required_history_never_changes_a_decision():
    df = _frame(_walk(120, seed=5))
    params = dict(PARAMS, period=6)
    base = _apply(df, params)
    later = pd.date_range(df.index[-1] + pd.Timedelta(minutes=30), periods=10, freq="30min")
    extended = pd.concat([df, _frame(_walk(10, seed=9), index=later)])
    ext = _apply(extended, params)
    for i in range(len(df)):
        _same(base.iloc[i], ext.iloc[i])
    i = 90
    for k in (0, 40, i - 6 - 2):
        poisoned = df.copy()
        poisoned.iloc[k, poisoned.columns.get_loc("close")] = -5.0
        vals = list(poisoned.index)
        vals[k] = vals[k] - pd.Timedelta(minutes=7)
        poisoned.index = pd.DatetimeIndex(vals)
        _same(base.iloc[i], _apply(poisoned, params).iloc[i])


def _bt(**kw):
    base = dict(initial_capital=1000.0, platform="hyperliquid",
                open_strategy={"name": NAME, "params": PARAMS},
                close_strategies=[TP], stop_loss_atr_mult=1.0, direction="both")
    base.update(kw)
    return Backtester(**base)


def _run_bt(df, **kw):
    sig = ensure_atr_indicator(_apply(df))
    before = sig.copy()
    return before, _bt(**kw).run(sig, strategy_name=NAME, symbol="BTC/USDT", timeframe="1h", save=False)


def test_long_entry_fills_at_the_next_bar_open_and_the_explicit_close_owns_the_exit():
    closes = [100.0] * 30 + [100.5, 101.0, 101.4, 101.8, 102.6, 103.4, 104.6, 105.5, 106.0]
    df = _frame(closes)
    sig, res = _run_bt(df)
    assert int(np.flatnonzero(sig["signal"].to_numpy() == 1)[0]) == 30
    t = res["trades"][0]
    assert t["side"] == "long"
    assert t["entry_date"] == str(df.index[31])
    assert t["entry_price"] == pytest.approx(df["open"].iloc[31] * (1 + _ENGINE_SLIPPAGE_PCT))
    assert t["exit_reason"].startswith("tiered_tp_pct") or "tp" in t["exit_reason"]


def test_short_entry_fills_at_the_next_bar_open():
    closes = [100.0] * 30 + [99.5, 99.0, 98.6, 98.2, 97.4, 96.6, 95.4, 94.5, 94.0]
    df = _frame(closes)
    sig, res = _run_bt(df)
    assert int(np.flatnonzero(sig["signal"].to_numpy() == -1)[0]) == 30
    t = res["trades"][0]
    assert t["side"] == "short"
    assert t["entry_date"] == str(df.index[31])
    assert t["entry_price"] == pytest.approx(df["open"].iloc[31] * (1 - _ENGINE_SLIPPAGE_PCT))


def test_opposite_and_repeated_signals_never_reverse_or_add_while_open():
    closes = ([100.0] * 30 + [100.4, 100.8, 101.0, 101.1, 101.2] + [100.9, 100.6, 100.3, 100.0, 99.8]
              + [100.2, 100.6, 101.0, 101.2] + [101.0] * 30)
    df = _frame(closes)
    sig, res = _run_bt(df, stop_loss_atr_mult=None, stop_loss_pct=50.0,
                       close_strategies=[{"name": "tiered_tp_pct",
                                          "params": {"tp_tiers": [{"profit_pct": 0.5, "close_fraction": 1.0}]}}])
    s = sig["signal"].to_numpy()
    assert (s == 1).sum() >= 2 and (s == -1).sum() >= 1
    trades = res["trades"]
    assert len(trades) == 1
    assert trades[0]["side"] == "long"
    assert trades[0]["shares"] == pytest.approx(trades[0].get("initial_shares", trades[0]["shares"]))


@pytest.mark.parametrize("path,want", [("up", "tp"), ("down", "sl")])
def test_invalid_entry_data_keeps_close_and_stop_management(path, want):
    rise = [100.0] * 30 + [100.5, 101.0, 101.4, 101.8]
    tail = [102.0, 103.5, 104.5, 105.5, 106.0] if path == "up" else [101.0, 99.0, 97.0, 95.0, 93.0]
    df = _frame(rise + tail)
    df = df.drop(df.index[len(rise)])
    sig, res = _run_bt(df)
    after = sig.iloc[len(rise):]
    assert (after["signal"] == 0).all()
    assert (after["cmo_reason"] == "cadence_gap").all()
    t = res["trades"][0]
    assert pd.Timestamp(t["exit_date"]) > df.index[len(rise) - 1]
    if want == "tp":
        assert "tp" in t["exit_reason"]
    else:
        assert t["exit_reason"] == "sl"


def test_composer_suppresses_entries_while_open_and_keeps_close_on_invalid_entry_data():
    fut = _reg()
    closes = [100.0] * 10 + [100.5, 101.0]
    crossing = _frame(closes)
    pos = {"side": "long", "avg_cost": 100.0, "current_quantity": 1.0, "initial_quantity": 1.0}
    open_eval = evaluate_open_close(fut.apply_strategy, fut.get_strategy, crossing.iloc[:11], NAME, None,
                                    ["tiered_tp_pct"], "long", dict(PARAMS), pos, close_evaluate=close_evaluate,
                                    market_ctx={"mark_price": 100.5},
                                    close_params_by_name={"tiered_tp_pct": TP["params"]})
    assert open_eval.open_signal == 1
    decided = finalize_decision(open_eval, "long")
    assert decided["signal"] == 0 and decided["close_fraction"] == 0.0

    gapped = crossing.drop(crossing.index[9])
    held = evaluate_open_close(fut.apply_strategy, fut.get_strategy, gapped, NAME, None, ["tiered_tp_pct"],
                               "long", dict(PARAMS), pos, close_evaluate=close_evaluate,
                               market_ctx={"mark_price": 104.0},
                               close_params_by_name={"tiered_tp_pct": TP["params"]})
    assert held.open_signal == 0
    assert held.open_result_df["cmo_reason"].iloc[-1] == "cadence_gap"
    out = finalize_decision(held, "long")
    assert out["close_fraction"] == 1.0 and out["close_strategy"] == "tiered_tp_pct" and out["signal"] == -1

    flat = finalize_decision(evaluate_open_close(fut.apply_strategy, fut.get_strategy, crossing.iloc[:11], NAME,
                                                 None, ["tiered_tp_pct"], "", dict(PARAMS), {},
                                                 close_evaluate=close_evaluate, market_ctx={"mark_price": 100.5},
                                                 close_params_by_name={"tiered_tp_pct": TP["params"]}), "")
    assert flat["open_action"] == "long" and flat["signal"] == 1


def _market(n=420, seed=16610):
    rng = np.random.RandomState(seed)
    t = np.arange(n)
    close = np.round(1000 + 25 * np.sin(t / 11.0) + np.cumsum(rng.randn(n) * 3.0), 2)
    df = _frame(close, start="2025-03-01")
    df["high"] = df["high"] + np.round(np.abs(rng.randn(n)) * 2.0, 2)
    df["low"] = df["low"] - np.round(np.abs(rng.randn(n)) * 2.0, 2)
    return df


def _parity_cfg(**kw):
    base = dict(strategy_name=NAME, params={"period": 10, "threshold": 25.0, "expected_interval_ms": H},
                registry="futures", platform="hyperliquid", symbol="BTC", timeframe="1h",
                close_refs=[TP], direction="both")
    base.update(kw)
    return ParityConfig(**base)


@pytest.mark.parametrize("window", [None, 200])
def test_non_batched_parity_is_clean(window):
    frame = compute_parity_frame(_market(), cfg=_parity_cfg(), window=window)
    s = summarize(frame)
    assert s["bars_compared"] > 0 and s["mismatches"] == 0
    assert s["clean"] is True and s["close_parity"] == "unverified"
    assert (frame["live_signal"] == 1).any() and (frame["live_signal"] == -1).any()


@pytest.mark.parametrize("window", [None, 200])
def test_batched_explicit_paper_parity_is_clean(window):
    frame = compute_parity_frame(_market(300), cfg=_parity_cfg(batched=True), window=window)
    s = summarize(frame)
    assert s["bars_compared"] > 0 and s["mismatches"] == 0 and s.get("batch_mismatches", 0) == 0
    assert s["clean"] is True


def test_period_198_parity_at_the_window_boundary():
    cfg = _parity_cfg(params={"period": 198, "threshold": 10.0, "expected_interval_ms": H})
    frame = compute_parity_frame(_market(260), cfg=cfg, window=200)
    s = summarize(frame)
    assert s["bars_compared"] > 0 and s["mismatches"] == 0 and s["clean"] is True
