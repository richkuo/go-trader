import contextlib
import math
import os
import sys

import numpy as np
import pandas as pd
import pytest

_HERE = os.path.dirname(os.path.abspath(__file__))
_REPO = os.path.abspath(os.path.join(_HERE, "..", ".."))
for _p in (os.path.join(_REPO, "shared_tools"), os.path.join(_REPO, "shared_strategies", "open")):
    if _p not in sys.path:
        sys.path.insert(0, _p)

import eval_windows
import optimizer
from atr import ensure_atr_indicator
from backtester import Backtester
from close_registry_loader import evaluate as close_evaluate
from closed_bar import (
    FRAME_DECISION_TIME_ATTR,
    FRAME_INTERVAL_ATTR,
    FRAME_RECEIVED_ORDER_DEFECTS_ATTR,
    FRAME_TIMESTAMP_KIND_ATTR,
    TIMESTAMP_KIND_NATIVE_CLOSE,
    TIMESTAMP_KIND_OPEN,
    declare_frame_cadence,
)
from optimizer import DEFAULT_PARAM_RANGES, strategy_warmup_bars
from registry_loader import load_registry
from strategy_composition import StrategyReference, evaluate_open_close, finalize_decision, preflight_reference_params

NAME = "calendar_conditioned_entry"
REF = "backtest/candidates/calendar_conditioned_entry_1653/REPORT.md"
H1 = 3_600_000
DAY = 86_400_000
WEEK = 7 * DAY
EPOCH = 345_600_000
P = {"training_bars": 2016, "holding_bars": 2, "refit_bars": 168, "min_effective_obs": 4, "confidence": 0.9,
     "cost_hurdle": 0.001, "expected_interval_ms": H1}
N_BARS = 2016 + 168 + 600
LONG_CELL = 14
SHORT_CELL = 3 * 24 + 3
CONTRACT = {"close_strategies": [{"name": "time_stop", "params": {"max_bars": P["holding_bars"]}}],
            "direction": "both", "stop_loss_atr_mult": 3.0, "comparison_mode": "approximate"}


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


def _apply(df, **over):
    return _reg().apply_strategy(NAME, df, {**P, **over})


def _frame_from_opens(opens, start="2026-01-05", tf_ms=H1, kind=TIMESTAMP_KIND_OPEN):
    opens = np.asarray(opens, dtype="float64")
    close = np.concatenate([opens[1:], [opens[-1]]])
    idx = pd.date_range(start, periods=len(opens), freq=pd.Timedelta(milliseconds=tf_ms))
    df = pd.DataFrame({"open": opens, "high": np.maximum(opens, close) * 1.0005,
                       "low": np.minimum(opens, close) * 0.9995, "close": close, "volume": 100.0}, index=idx)
    df.index.name = "datetime"
    tf = {H1: "1h", 4 * H1: "4h", 900_000: "15m"}[tf_ms]
    return declare_frame_cadence(df, tf, kind)


def _calendar(n=N_BARS, seed=1653, noise=0.002, effect=0.01, start="2026-01-05", tf_ms=H1):
    rng = np.random.default_rng(seed)
    idx = pd.date_range(start, periods=n, freq=pd.Timedelta(milliseconds=tf_ms))
    cell = np.asarray(idx.dayofweek * 24 + idx.hour)
    r = rng.normal(0.0, noise, n)
    r[cell == LONG_CELL] += effect
    r[cell == SHORT_CELL] -= effect
    opens = 100.0 * np.exp(np.concatenate([[0.0], np.cumsum(r[:-1])]))
    return _frame_from_opens(opens, start=start, tf_ms=tf_ms)


def _trend(step, noise, n=N_BARS, seed=7):
    rng = np.random.default_rng(seed)
    r = step + rng.normal(0.0, noise, n) if noise else np.full(n, step)
    return _frame_from_opens(100.0 * np.exp(np.concatenate([[0.0], np.cumsum(r[:-1])])))


def _ms(df):
    idx = df.index
    if idx.tz is not None:
        idx = idx.tz_convert("UTC").tz_localize(None)
    return np.asarray(idx.as_unit("ms").asi8, dtype=np.int64)


def _reference(df, i, p=P):
    from statistics import NormalDist
    ms = _ms(df)
    if df.attrs.get(FRAME_TIMESTAMP_KIND_ATTR) == TIMESTAMP_KIND_NATIVE_CLOSE:
        ms = ms - p["expected_interval_ms"] + 1
    iv, T, H, R = p["expected_interval_ms"], p["training_bars"], p["holding_bars"], p["refit_bars"]
    pos = {int(t): k for k, t in enumerate(ms)}
    boundary = int(ms[i]) + iv
    grid = R * iv
    refit_boundary = EPOCH + ((boundary - EPOCH) // grid) * grid
    first = refit_boundary - T * iv
    nxt = int(ms[i]) + iv
    want = (((nxt - EPOCH) // DAY) % 7) * 24 + ((nxt - EPOCH) % DAY) // 3_600_000
    opens = df["open"].to_numpy(dtype="float64")
    clusters = {}
    for t in range(first, refit_boundary - iv - H * iv + 1, iv):
        rel = t - EPOCH
        cell = ((rel // DAY) % 7) * 24 + (rel % DAY) // 3_600_000
        if cell != want:
            continue
        label = opens[pos[t + H * iv]] / opens[pos[t]] - 1.0
        clusters.setdefault(rel // WEEK, []).append(label)
    x = np.array([np.mean(v) for _, v in sorted(clusters.items())])
    n = len(x)
    out = {"refit_boundary": refit_boundary, "cell": want, "n": n, "raw": sum(len(v) for v in clusters.values())}
    if n == 0:
        return dict(out, signal=0, reason="empty_cell")
    m = float(np.mean(x))
    dev = x - m
    g0 = float(np.mean(dev ** 2))
    L = max(1, math.ceil(H * iv / WEEK))
    s = g0
    for lag in range(1, L + 1):
        if lag < n:
            s += 2.0 * (1.0 - lag / (L + 1.0)) * float(np.sum(dev[lag:] * dev[:-lag])) / n
    v = max(s, g0)
    n_eff = n * g0 / v if v > 0 else 0.0
    se = math.sqrt(v * n / (n - 1) / n) if n > 1 else float("nan")
    z = NormalDist().inv_cdf(1.0 - (1.0 - p["confidence"]) / 336)
    long_lb = m - p["cost_hurdle"] - z * se
    short_lb = -m - p["cost_hurdle"] - z * se
    signal, reason = 0, "below_threshold"
    if long_lb > 0 and not short_lb > 0:
        signal, reason = 1, "entry_long"
    elif short_lb > 0 and not long_lb > 0:
        signal, reason = -1, "entry_short"
    if n_eff < p["min_effective_obs"]:
        signal, reason = 0, "insufficient_effective_obs"
    if not se > 0:
        signal, reason = 0, "no_uncertainty_estimate"
    return dict(out, mean=m, se=se, n_eff=n_eff, long_lb=long_lb, short_lb=short_lb, signal=signal, reason=reason)


def test_decisions_match_an_independent_weekly_cluster_reference():
    df = _calendar()
    out = _apply(df)
    valid = np.flatnonzero(out["cce_valid"].to_numpy() == 1.0)
    assert len(valid) > 500
    checked = set()
    for i in valid:
        ref = _reference(df, int(i))
        key = (ref["refit_boundary"], ref["cell"])
        assert int(out["signal"].iloc[i]) == ref["signal"], (i, out["cce_reason"].iloc[i], ref)
        assert out["cce_reason"].iloc[i] == ref["reason"]
        assert int(out["cce_refit_boundary_ms"].iloc[i]) == ref["refit_boundary"]
        assert int(out["cce_next_cell"].iloc[i]) == ref["cell"]
        if key in checked:
            continue
        checked.add(key)
        assert int(out["cce_weekly_obs"].iloc[i]) == ref["n"] and int(out["cce_raw_obs"].iloc[i]) == ref["raw"]
        for col, k in (("cce_gross_mean", "mean"), ("cce_std_error", "se"), ("cce_effective_obs", "n_eff"),
                       ("cce_long_lower_bound", "long_lb"), ("cce_short_lower_bound", "short_lb")):
            assert out[col].iloc[i] == pytest.approx(ref[k], rel=1e-9, abs=1e-12), (i, col)
        assert out["cce_long_net_edge"].iloc[i] == ref["mean"] - P["cost_hurdle"]
        assert out["cce_short_net_edge"].iloc[i] == -ref["mean"] - P["cost_hurdle"]
    sig = out["signal"].to_numpy()
    nxt = out["cce_next_cell"].to_numpy()
    for side, cells in ((1, {LONG_CELL - 1, LONG_CELL}), (-1, {SHORT_CELL - 1, SHORT_CELL})):
        got = nxt[sig == side].astype(int)
        assert cells <= set(got) and np.isin(got, list(cells)).mean() >= 0.8, (side, sorted(set(got)))
    assert out["cce_family_size"].iloc[-1] == 336.0 and out["cce_nw_lag"].iloc[-1] == 1.0


def test_rising_falling_constant_and_directional_cost_handling():
    rising = _apply(_trend(0.003, 0.0005))
    falling = _apply(_trend(-0.003, 0.0005))
    flat = _apply(_trend(0.0, 0.0))
    for out, side in ((rising, 1), (falling, -1)):
        valid = out["cce_valid"] == 1.0
        assert valid.sum() > 500 and (out.loc[valid, "signal"] == side).all()
    valid = flat["cce_valid"] == 1.0
    assert valid.sum() > 500 and set(flat.loc[valid, "cce_reason"]) == {"no_uncertainty_estimate"}
    assert int(flat["signal"].abs().sum()) == 0
    costly = _apply(_trend(0.003, 0.0005), cost_hurdle=0.01)
    valid = costly["cce_valid"] == 1.0
    assert int(costly["signal"].abs().sum()) == 0 and set(costly.loc[valid, "cce_reason"]) == {"below_threshold"}
    mean = costly.loc[valid, "cce_gross_mean"]
    assert (mean > 0).all() and (mean < 0.01).all()
    assert (costly.loc[valid, "cce_long_net_edge"] == mean - 0.01).all()
    assert (costly.loc[valid, "cce_short_net_edge"] == -mean - 0.01).all()
    assert (costly.loc[valid, "cce_short_lower_bound"] < 0).all()


def test_effective_sample_floor_and_refit_boundaries():
    df = _calendar()
    out = _apply(df)
    i = int(np.flatnonzero(out["signal"].to_numpy() == 1)[0])
    n_eff = float(out["cce_effective_obs"].iloc[i])
    assert int(_apply(df, min_effective_obs=int(math.floor(n_eff)))["signal"].iloc[i]) == 1
    tight = _apply(df, min_effective_obs=int(math.floor(n_eff)) + 1)
    assert int(tight["signal"].iloc[i]) == 0 and tight["cce_reason"].iloc[i] == "insufficient_effective_obs"
    valid = out[out["cce_valid"] == 1.0]
    boundary = valid["cce_refit_boundary_ms"].astype(np.int64)
    assert ((boundary - EPOCH) % (P["refit_bars"] * H1) == 0).all()
    assert (valid["cce_refit_bar_open_ms"].astype(np.int64) == boundary - H1).all()
    assert (valid["cce_training_first_open_ms"].astype(np.int64) == boundary - P["training_bars"] * H1).all()
    eval_boundary = valid["cce_eval_ts_ms"].astype(np.int64) + H1
    changes = np.flatnonzero(np.diff(boundary.to_numpy()) != 0) + 1
    assert len(changes) >= 2
    assert (eval_boundary.to_numpy()[changes] == boundary.to_numpy()[changes]).all()
    assert (eval_boundary.to_numpy()[changes - 1] - boundary.to_numpy()[changes - 1] == (P["refit_bars"] - 1) * H1).all()


def test_labels_are_completed_before_the_refit_that_consumes_them():
    df = _calendar()
    out = _apply(df)
    rows = out.index[out["cce_valid"] == 1.0]
    refit_open = int(out.loc[rows[400], "cce_refit_bar_open_ms"])
    r = int(np.flatnonzero(_ms(df) == refit_open)[0])
    block = (out["cce_refit_bar_open_ms"] == refit_open).to_numpy()
    after = df.copy()
    after.iloc[r + 1, after.columns.get_loc("open")] *= 1.05
    after.iloc[r + 1, after.columns.get_loc("high")] = max(after["high"].iloc[r + 1], after["open"].iloc[r + 1])
    changed_after = _apply(after)
    for col in ("signal", "cce_gross_mean", "cce_std_error", "cce_reason"):
        assert list(changed_after.loc[block, col].astype(str)) == list(out.loc[block, col].astype(str)), col
    at = df.copy()
    at.iloc[r, at.columns.get_loc("open")] *= 1.05
    at.iloc[r, at.columns.get_loc("high")] = max(at["high"].iloc[r], at["open"].iloc[r])
    at.iloc[r - 1, at.columns.get_loc("close")] = at["open"].iloc[r]
    at.iloc[r - 1, at.columns.get_loc("high")] = max(at["high"].iloc[r - 1], at["open"].iloc[r])
    changed_at = _apply(at)
    exit_cell = int(((refit_open - P["holding_bars"] * H1 - EPOCH) // DAY % 7) * 24
                    + ((refit_open - P["holding_bars"] * H1 - EPOCH) % DAY) // H1)
    rows_in_cell = block & (out["cce_next_cell"].to_numpy() == exit_cell)
    assert rows_in_cell.any()
    assert (changed_at.loc[rows_in_cell, "cce_gross_mean"] != out.loc[rows_in_cell, "cce_gross_mean"]).all()


def _tz_variants(df):
    utc = df.copy()
    utc.index = df.index.tz_localize("UTC")
    ny = df.copy()
    ny.index = df.index.tz_localize("UTC").tz_convert("America/New_York")
    ms_unit = df.copy()
    ms_unit.index = df.index.as_unit("ms")
    native = df.copy()
    native.index = df.index + pd.Timedelta(milliseconds=H1 - 1)
    for v in (utc, ny, ms_unit, native):
        v.attrs = dict(df.attrs)
    native.attrs[FRAME_TIMESTAMP_KIND_ATTR] = TIMESTAMP_KIND_NATIVE_CLOSE
    return {"aware_utc": utc, "america_new_york": ny, "ms_unit": ms_unit, "native_close": native}


def test_naive_aware_unit_and_native_close_indexes_give_the_same_buckets_and_decisions():
    df = _calendar(start="2026-03-02")
    base = _apply(df)
    cols = ["signal", "cce_reason", "cce_eval_ts_ms", "cce_next_cell", "cce_refit_boundary_ms", "cce_gross_mean",
            "cce_std_error"]
    for label, variant in _tz_variants(df).items():
        got = _apply(variant)
        for col in cols:
            assert list(got[col].astype(str)) == list(base[col].astype(str)), (label, col)
    assert int(base["signal"].abs().sum()) >= 4


def _defect(df, kind, k):
    out = df.copy()
    out.attrs = dict(df.attrs)
    col = out.columns.get_loc
    if kind == "cadence_gap":
        out = out.drop(out.index[k])
        out.attrs = dict(df.attrs)
    elif kind == "duplicate_timestamp":
        out = pd.concat([out.iloc[:k], out.iloc[k - 1:k], out.iloc[k:]])
        out.attrs = dict(df.attrs)
    elif kind == "descending_timestamp":
        order = list(range(len(out)))
        order[k], order[k + 1] = order[k + 1], order[k]
        out = out.iloc[order]
        out.attrs = dict(df.attrs)
    elif kind == "received_out_of_order":
        out.attrs[FRAME_RECEIVED_ORDER_DEFECTS_ATTR] = [int(_ms(df)[k])]
    elif kind == "nonfinite_price":
        out.iloc[k, col("close")] = np.nan
    elif kind == "nonpositive_price":
        out.iloc[k, col("low")] = 0.0
    elif kind == "inconsistent_candle":
        out.iloc[k, col("close")] = out["high"].iloc[k] * 1.01
    elif kind == "grid_misaligned":
        idx = list(out.index)
        idx[k] = idx[k] + pd.Timedelta(minutes=1)
        out.index = pd.DatetimeIndex(idx, name="datetime")
    return out


INPUT_DEFECTS = [
    ("cadence_gap", "cadence_gap"),
    ("duplicate_timestamp", "duplicate_timestamp"),
    ("descending_timestamp", "descending_timestamp"),
    ("received_out_of_order", "received_out_of_order"),
    ("nonfinite_price", "nonfinite_price"),
    ("nonpositive_price", "nonpositive_price"),
    ("inconsistent_candle", "inconsistent_candle"),
    ("grid_misaligned", "grid_misaligned"),
]


def test_input_defects_hold_with_a_reason_never_rewrite_earlier_decisions_and_recover_on_clean_history():
    df = _calendar(n=N_BARS + 2600)
    clean = _apply(df)
    for kind, row_reason in INPUT_DEFECTS:
        with _case(kind):
            _input_defect_holds_and_recovers(df, clean, kind, row_reason)


def _input_defect_holds_and_recovers(df, clean, kind, row_reason):
    k = 2400
    bad = _defect(df, kind, k)
    out = _apply(bad)
    for col in ("signal", "cce_reason", "cce_gross_mean"):
        assert list(out[col].iloc[:k].astype(str)) == list(clean[col].iloc[:k].astype(str)), col
    flagged = k + 1 if kind == "descending_timestamp" else k
    assert out["cce_reason"].iloc[flagged] == row_reason and int(out["signal"].iloc[flagged]) == 0
    after = out["cce_reason"].iloc[flagged + 1:flagged + 200]
    assert after.str.startswith("history_").all() or (after == "cadence_gap").any()
    assert (out["cce_valid"].iloc[k:k + P["training_bars"]] == 0.0).all()
    recovered = np.flatnonzero(out["cce_valid"].to_numpy() == 1.0)
    first = int(recovered[recovered > k][0])
    assert int(out["cce_history_bars"].iloc[first]) >= int(out["cce_required_bars"].iloc[first])
    assert int(out["cce_history_bars"].iloc[first - 1]) < int(out["cce_required_bars"].iloc[first - 1])


def test_future_rows_never_change_earlier_signals_tables_or_refit_timing():
    from calendar_conditioned_entry import calendar_refit_tables
    df = _calendar()
    full = _apply(df)
    cols = ["signal", "cce_reason", "cce_refit_boundary_ms", "cce_gross_mean", "cce_long_lower_bound"]
    for cut in range(P["training_bars"] + 150, len(df), 7):
        last = _apply(df.iloc[:cut + 1]).iloc[-1]
        for col in cols:
            assert str(last[col]) == str(full[col].iloc[cut]), (cut, col)
    tables = calendar_refit_tables(df, **P)
    assert len(tables) >= 4
    pos = {int(t): i for i, t in enumerate(_ms(df))}
    for t in tables:
        prefix = calendar_refit_tables(df.iloc[:pos[t["refit_bar_open_ms"]] + 1], **P)
        assert prefix[-1] == t
    cut = 1300
    mutated = df.copy()
    mutated.iloc[cut:, mutated.columns.get_loc("open")] *= 1.3
    mutated.iloc[cut:, mutated.columns.get_loc("high")] *= 1.3
    mutated.iloc[cut:, mutated.columns.get_loc("low")] *= 1.3
    mutated.iloc[cut:, mutated.columns.get_loc("close")] *= 1.3
    mut = _apply(mutated)
    for col in cols:
        assert list(mut[col].iloc[:cut - 1].astype(str)) == list(full[col].iloc[:cut - 1].astype(str)), col


def test_shifting_the_frame_start_keeps_every_decision_that_still_has_its_history():
    df = _calendar(n=N_BARS + 600)
    full = _apply(df)
    for shift in (1, 37, 167, 168, 500):
        with _case(shift):
            _shifted_start_keeps_decisions(df, full, shift)


def _shifted_start_keeps_decisions(df, full, shift):
    out = _apply(df.iloc[shift:])
    kept = 0
    for j in range(len(out)):
        i = j + shift
        if out["cce_valid"].iloc[j] == 1.0:
            kept += 1
            for col in ("signal", "cce_reason", "cce_refit_boundary_ms", "cce_gross_mean"):
                assert str(out[col].iloc[j]) == str(full[col].iloc[i]), (shift, i, col)
        elif full["cce_valid"].iloc[i] == 1.0:
            assert out["cce_reason"].iloc[j] == "insufficient_history"
    assert kept > 400


def test_missing_history_columns_and_timing_hold_with_reasons():
    df = _calendar()
    short = _apply(df.iloc[:P["training_bars"] - 1])
    assert set(short["cce_reason"]) == {"insufficient_history"} and int(short["signal"].abs().sum()) == 0
    assert (short["cce_required_bars"] > short["cce_history_bars"]).all()
    undeclared = df.copy()
    undeclared.attrs = {}
    assert set(_apply(undeclared)["cce_reason"]) == {"undeclared_timing"}
    contradiction = df.copy()
    contradiction.attrs = {FRAME_INTERVAL_ATTR: 4 * H1, FRAME_TIMESTAMP_KIND_ATTR: TIMESTAMP_KIND_OPEN}
    assert set(_apply(contradiction)["cce_reason"]) == {"timing_contradiction"}
    assert set(_apply(df.drop(columns=["low"]))["cce_reason"]) == {"missing_columns"}
    out = _apply(df.reset_index(drop=True))
    assert set(out["cce_reason"]) == {"missing_timestamps"} and len(out) == len(df)


def test_check_time_frames_hold_unless_the_last_bar_is_the_latest_closed_bar():
    df = _calendar()
    full = _apply(df)
    i = int(np.flatnonzero(full["signal"].to_numpy() == 1)[-1])
    frame = df.iloc[:i + 1].copy()
    frame.attrs = dict(df.attrs)
    boundary = int(_ms(frame)[-1]) + H1
    for cutoff, declared, signal, reason in [
        (boundary, boundary, 1, "entry_long"),
        (boundary + H1, boundary, 0, "decision_bar_stale"),
        (boundary - 1, boundary, 0, "decision_bar_future"),
        (boundary, boundary - H1, 0, "decision_boundary_mismatch"),
        (None, None, 0, "decision_time_missing"),
    ]:
        runtime = frame.copy()
        runtime.attrs = dict(frame.attrs)
        runtime.attrs[FRAME_DECISION_TIME_ATTR] = {} if cutoff is None else {"cutoff_ms": cutoff,
                                                                             "boundary_ms": declared}
        out = _apply(runtime)
        assert int(out["signal"].iloc[-1]) == signal and out["cce_reason"].iloc[-1] == reason
        assert out["cce_gross_mean"].iloc[-1] == full["cce_gross_mean"].iloc[i]


INVALID_PARAMS = [
    {"training_bars": True}, {"training_bars": "4368"}, {"training_bars": None}, {"training_bars": 4368.5},
    {"training_bars": float("nan")}, {"holding_bars": 0}, {"holding_bars": 4368}, {"refit_bars": 0},
    {"refit_bars": 5000}, {"min_effective_obs": 1}, {"confidence": 0.0}, {"confidence": 1.0},
    {"confidence": float("inf")}, {"confidence": True}, {"cost_hurdle": -0.001}, {"cost_hurdle": float("nan")},
    {"expected_interval_ms": 7_000_000}, {"expected_interval_ms": 3_600_000.5},
]


def test_invalid_parameters_are_refused_before_evaluation():
    reg = _reg()
    for bad in INVALID_PARAMS:
        with _case(bad):
            with pytest.raises(ValueError):
                reg.validate_params(NAME, bad)
            refs = [StrategyReference("open", NAME)]
            with pytest.raises(ValueError, match=f"open strategy '{NAME}' has invalid parameters"):
                preflight_reference_params(refs, bad, None, reg.get_strategy, reg.validate_params)
            with pytest.raises((ValueError, TypeError)):
                reg.apply_strategy(NAME, _calendar(n=50), {**P, **bad})


def _backtest(df, p=P, stop=3.0, direction="both", mode="approximate"):
    signals = ensure_atr_indicator(_reg().apply_strategy(NAME, df, dict(p)))
    bt = Backtester(initial_capital=1000.0, platform="hyperliquid",
                    close_strategies=[{"name": "time_stop", "params": {"max_bars": p["holding_bars"]}}],
                    direction=direction, stop_loss_atr_mult=stop, comparison_mode=mode)
    return signals, bt.run(signals, strategy_name=NAME, symbol="TST", timeframe="1h", params=dict(p), save=False)


def test_the_simulator_closes_both_directions_at_the_label_horizon_under_one_close_owner():
    df = _calendar()
    signals, res = _backtest(df)
    trades = res["trades"]
    assert {t["side"] for t in trades} == {"long", "short"}
    ms = _ms(df)
    pos = {pd.Timestamp(t): k for k, t in enumerate(df.index)}
    opens = df["open"].to_numpy()
    for t in trades:
        e = pos[pd.Timestamp(t["entry_date"])]
        x = pos[pd.Timestamp(t["exit_date"])]
        assert int(signals["signal"].iloc[e - 1]) == (1 if t["side"] == "long" else -1)
        assert t["exit_reason"] == f"time_stop:{P['holding_bars']}" and t["bars_held"] == P["holding_bars"]
        assert x - e == P["holding_bars"] and ms[x] - ms[e] == P["holding_bars"] * H1
        label = opens[x] / opens[e] - 1.0
        gross = t["exit_price"] / t["entry_price"]
        slip = (1 - 0.0005) / (1 + 0.0005) if t["side"] == "long" else (1 + 0.0005) / (1 - 0.0005)
        assert gross == pytest.approx((1 + label) * slip, rel=1e-9)
    validation = res["close_validation"]
    assert validation["mode"] == "approximate" and validation["incomplete_parity"] is True
    assert {a["reason_code"] for a in validation["approximations"]} == {"RESEARCH_ONLY_CLOSE_CONTEXT"}
    with pytest.raises(ValueError):
        _backtest(df, mode="strict")


def test_protection_fires_before_the_horizon_and_an_entry_data_hold_keeps_position_management():
    df = _calendar()
    p = {**P, "holding_bars": 6}
    signals = _reg().apply_strategy(NAME, df, p)
    n = int(np.flatnonzero(signals["signal"].to_numpy() == 1)[0])
    crash = df.copy()
    crash.attrs = dict(df.attrs)
    crash.iloc[n + 2, crash.columns.get_loc("low")] = crash["open"].iloc[n + 2] * 0.5
    _, res = _backtest(crash, p=p)
    first = res["trades"][0]
    assert first["side"] == "long" and first["exit_reason"] == "sl" and first["bars_held"] < 6
    gapped = df.drop(df.index[n + 3])
    gapped.attrs = dict(df.attrs)
    out, res = _backtest(gapped, p=p)
    assert out["cce_reason"].iloc[n + 3] == "cadence_gap" and (out["cce_valid"].iloc[n + 3:n + 50] == 0.0).all()
    first = res["trades"][0]
    assert pd.Timestamp(first["entry_date"]) == gapped.index[n + 1]
    assert first["exit_reason"] == "time_stop:6" and first["bars_held"] == 6


def test_the_composer_blocks_entries_while_positioned_and_keeps_the_time_stop_close():
    df = _calendar()
    out = _apply(df)
    i = int(np.flatnonzero(out["signal"].to_numpy() == -1)[0])
    frame = df.iloc[:i + 1].copy()
    frame.attrs = dict(df.attrs)
    reg = _reg()

    def decide(side, ctx):
        ev = evaluate_open_close(reg.apply_strategy, reg.get_strategy, frame, NAME, NAME, ["time_stop"], side,
                                 dict(P), ctx, close_evaluate=close_evaluate,
                                 market_ctx={"mark_price": float(frame["close"].iloc[-1])},
                                 close_params_by_name={"time_stop": {"max_bars": P["holding_bars"]}})
        return finalize_decision(ev, side)

    flat = decide("", {})
    assert flat["open_action"] == "short" and flat["signal"] == -1
    held = decide("long", {"side": "long", "avg_cost": 100.0, "current_quantity": 1.0, "initial_quantity": 1.0,
                           "bars_held": 1})
    assert held["open_action"] == "short" and held["close_fraction"] == 0.0 and held["signal"] == 0
    due = decide("long", {"side": "long", "avg_cost": 100.0, "current_quantity": 1.0, "initial_quantity": 1.0,
                          "bars_held": P["holding_bars"]})
    assert due["close_fraction"] == 1.0 and due["close_strategy"] == "time_stop" and due["signal"] == -1
    live = decide("long", {"side": "long", "avg_cost": 100.0, "current_quantity": 1.0, "initial_quantity": 1.0})
    assert live["close_fraction"] == 0.0 and live["signal"] == 0


def test_walk_forward_warmup_covers_training_and_refit_history_without_seeding(monkeypatch):
    grid = optimizer.generate_param_grid(DEFAULT_PARAM_RANGES[NAME])
    want = max(p["training_bars"] + p["refit_bars"] - 1 for p in grid)
    assert strategy_warmup_bars(NAME, grid, DEFAULT_PARAM_RANGES[NAME]) == want
    assert strategy_warmup_bars("rsi", [{}], DEFAULT_PARAM_RANGES["rsi"]) == optimizer.max_indicator_lookback(
        DEFAULT_PARAM_RANGES["rsi"])
    df = _calendar(n=N_BARS + 400)
    full = _apply(df)
    need = P["training_bars"] + P["refit_bars"] - 1
    late = [b for b in range(need, len(df)) if int(full["cce_required_bars"].iloc[b]) == need]
    b = late[0]
    ext = _apply(df.iloc[b - need + 1:b + 1])
    for col in ("signal", "cce_reason", "cce_gross_mean", "cce_refit_boundary_ms"):
        assert str(ext[col].iloc[-1]) == str(full[col].iloc[b]), col
    assert _apply(df.iloc[b - need + 2:b + 1])["cce_reason"].iloc[-1] == "insufficient_history"

    real_run = Backtester.run

    def unseeded_run(self, signals, **kw):
        if kw.get("warmup_prefix") is not None or kw.get("starting_long") is not None:
            raise AssertionError("the walk-forward warmup seeder ran for an entry-only candidate")
        return real_run(self, signals, **kw)

    monkeypatch.setattr(Backtester, "run", unseeded_run)
    small = {k: [P[k]] for k in ("training_bars", "holding_bars", "refit_bars", "min_effective_obs", "confidence",
                                 "cost_hurdle")}
    result = optimizer.walk_forward_optimize(
        _calendar(n=6000), NAME, small, n_splits=2, registry="futures", timeframe="1h", platform="hyperliquid",
        verbose=False, close_strategies=CONTRACT["close_strategies"], direction="both", stop_loss_atr_mult=3.0,
        comparison_mode="approximate")
    assert result["n_valid_folds"] >= 1


def test_walk_forward_scores_each_holding_horizon_with_its_own_time_stop(monkeypatch):
    scored = []
    real_run = Backtester.run

    def run(self, signals, **kw):
        res = real_run(self, signals, **kw)
        scored.append((kw["params"]["holding_bars"], {t["exit_reason"] for t in res["trades"]}))
        return res

    monkeypatch.setattr(Backtester, "run", run)
    ranges = {**{k: [P[k]] for k in ("training_bars", "refit_bars", "min_effective_obs", "confidence",
                                     "cost_hurdle")}, "holding_bars": [2, 3]}
    close = [{"name": "time_stop", "params": {"max_bars": 4}}]
    result = optimizer.walk_forward_optimize(
        _calendar(n=6000), NAME, ranges, n_splits=2, registry="futures", timeframe="1h", platform="hyperliquid",
        verbose=False, close_strategies=close, direction="both", stop_loss_atr_mult=3.0,
        comparison_mode="approximate")
    assert close == [{"name": "time_stop", "params": {"max_bars": 4}}]
    assert result["n_valid_folds"] >= 1 and result["close_stack_grid_size"] == 2
    assert {h for h, _ in scored} == {2, 3}
    for h, reasons in scored:
        assert {r for r in reasons if r.startswith("time_stop")} == {f"time_stop:{h}"}
    for w in result["window_results"]:
        assert w["best_close_stack_spec"]["close_strategies"] == [
            {"name": "time_stop", "params": {"max_bars": w["best_params"]["holding_bars"]}}]


def test_cached_m1_warmup_scores_the_window_flat_from_a_complete_history_prefix(monkeypatch):
    import data_fetcher
    df = _calendar(n=N_BARS + 1500)
    window_start = df.index[N_BARS + 300]
    window = (str(window_start), str(df.index[-1] + pd.Timedelta(hours=1)))

    def cached(symbol, timeframe, start_date=None, end_date=None, **_k):
        out = df[(df.index >= pd.Timestamp(start_date)) & (df.index <= pd.Timestamp(end_date))].copy()
        out.attrs = dict(df.attrs)
        return out

    monkeypatch.setattr(data_fetcher, "load_cached_data", cached)
    reg = _reg()
    need = P["training_bars"] + P["refit_bars"] - 1
    leg = eval_windows.run_leg(reg, NAME, dict(P), "TST/USDT", "1h", window, keep_trades=True, funding_mode="off",
                               warmup_bars=need, **{k: v for k, v in CONTRACT.items()})
    assert leg["warmup"]["bars"] == need and leg["warmup"]["scored_first_bar"] == str(window_start)
    full = _apply(df)
    first = full.index.get_loc(window_start)
    expected = full.iloc[first:-1]
    entries = sorted(str(df.index[i + 1]) for i in range(first, len(df) - 1) if int(full["signal"].iloc[i]) != 0)
    positions = leg["execution"]["position_list"]
    assert positions and str(positions[0]["entry_date"]) >= str(window_start)
    assert {p["entry_date"] for p in positions} <= set(entries)
    assert int(expected["cce_valid"].iloc[0]) == 1
    with pytest.raises(ValueError, match="warmup needs"):
        eval_windows.run_leg(reg, NAME, dict(P), "TST/USDT", "1h", (str(df.index[200]), window[1]),
                             funding_mode="off", warmup_bars=need, **CONTRACT)
    base = eval_windows.run_leg(reg, "rsi", None, "TST/USDT", "1h", window, funding_mode="off", **CONTRACT)
    zero = eval_windows.run_leg(reg, "rsi", None, "TST/USDT", "1h", window, funding_mode="off", warmup_bars=0,
                                **CONTRACT)
    assert base == zero and "warmup" not in zero
