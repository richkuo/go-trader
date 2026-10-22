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
from backtester import Backtester, validate_close_capabilities
from close_registry_loader import evaluate as close_evaluate
from closed_bar import (
    FRAME_DECISION_TIME_ATTR,
    FRAME_RECEIVED_ORDER_DEFECTS_ATTR,
    TIMESTAMP_KIND_NATIVE_CLOSE,
    TIMESTAMP_KIND_OPEN,
    declare_frame_cadence,
)
from eval_windows import expand_sweep
from optimizer import DEFAULT_PARAM_RANGES, generate_param_grid, max_indicator_lookback, strategy_warmup_bars
from parity_diff import ParityConfig, compute_parity_frame, summarize
from registry_loader import load_registry
from strategy_composition import evaluate_open_close, finalize_decision

NAME = "klinger_divergence"
MANIFEST = os.path.join(_REPO, "backtest", "candidates", "klinger_divergence_1662_manifest.json")
SEED = os.path.join(_REPO, "backtest", "candidates", "klinger_divergence_1662", "candidate_seed.json")
CLOSE = [{"name": "tiered_tp_pct", "params": {"tp_tiers": [{"profit_pct": 0.02, "close_fraction": 1.0}]}}]
INPUT_COLUMNS = ["open", "high", "low", "close", "volume"]
FOUR_H = 14_400_000
DEFAULTS = {"fast_period": 34, "slow_period": 55, "signal_period": 13, "replay_bars": 112, "left_span": 3,
            "right_span": 3, "min_separation": 5, "max_separation": 40, "setup_expiry": 12,
            "divergence_test": True}
FAST = dict(DEFAULTS, fast_period=5, slow_period=13, signal_period=5, replay_bars=40, left_span=2,
            right_span=2, min_separation=3, max_separation=20, setup_expiry=8)
TINY = {"fast_period": 2, "slow_period": 3, "signal_period": 2, "replay_bars": 4, "left_span": 1,
        "right_span": 1, "min_separation": 1, "max_separation": 1, "setup_expiry": 1, "divergence_test": True}
DOMAIN_ENDPOINTS = [
    dict(TINY),
    dict(DEFAULTS, fast_period=120, slow_period=150, signal_period=150, replay_bars=151, left_span=10,
         right_span=10, min_separation=1, max_separation=10, setup_expiry=17),
    dict(DEFAULTS, fast_period=2, slow_period=3, signal_period=2, replay_bars=4, left_span=10, right_span=10,
         min_separation=100, max_separation=100, setup_expiry=50),
    dict(DEFAULTS, replay_bars=128, left_span=3, right_span=3, max_separation=40, setup_expiry=12),
]


def _support(p):
    return p["replay_bars"] + p["left_span"] + p["max_separation"] + p["right_span"] + p["setup_expiry"] + 2


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


def _frame(open_, high, low, close, volume, start="2026-01-01"):
    df = pd.DataFrame({"open": open_, "high": high, "low": low, "close": close, "volume": volume})
    df.index = pd.date_range(start, periods=len(df), freq="4h")
    return declare_frame_cadence(df, "4h", TIMESTAMP_KIND_OPEN)


def _walk(n, seed, start=100.0, sigma=0.02, tick=None):
    rng = np.random.default_rng(seed)
    close = start * np.exp(np.cumsum(rng.normal(0.0, sigma, n)))
    open_ = np.concatenate([[start], close[:-1]])
    high = np.maximum(open_, close) * (1.0 + rng.uniform(0.001, 0.01, n))
    low = np.minimum(open_, close) * (1.0 - rng.uniform(0.001, 0.01, n))
    if tick:
        open_, close = np.round(open_ / tick) * tick, np.round(close / tick) * tick
        high = np.maximum(np.ceil(high / tick) * tick, np.maximum(open_, close))
        low = np.minimum(np.floor(low / tick) * tick, np.minimum(open_, close))
    volume = rng.uniform(100.0, 1000.0, n)
    return _frame(open_, high, low, close, volume)


def ref_endpoint(h, l, c, v, t, p):
    L = p["replay_bars"]
    af, asl, ag = 2.0 / (p["fast_period"] + 1), 2.0 / (p["slow_period"] + 1), 2.0 / (p["signal_period"] + 1)
    trend = -1.0
    dm = float(h[t - L] - l[t - L])
    cm = dm
    zero = not cm > 0
    vf = float(v[t - L]) * 2.0 * ((dm / cm if cm > 0 else 0.0) - 1.0) * trend * 100.0
    fe = se = vf
    osc = fe - se
    sig = osc
    for j in range(t - L + 1, t + 1):
        nt = 1.0 if (h[j] + l[j]) + c[j] > (h[j - 1] + l[j - 1]) + c[j - 1] else -1.0
        dm = float(h[j] - l[j])
        cm = cm + dm if nt == trend else float(h[j - 1] - l[j - 1]) + dm
        trend = nt
        if not cm > 0:
            zero = True
        vf = float(v[j]) * 2.0 * ((dm / cm if cm > 0 else 0.0) - 1.0) * trend * 100.0
        fe = af * vf + (1.0 - af) * fe
        se = asl * vf + (1.0 - asl) * se
        osc = fe - se
        sig = ag * osc + (1.0 - ag) * sig
    return {"trend": trend, "dm": dm, "cm": cm, "vf": vf, "fast": fe, "slow": se, "osc": osc, "sig": sig,
            "zero": zero}


def _strict(values, q, left, right, lows):
    centre = values[q]
    others = list(values[q - left:q]) + list(values[q + 1:q + right + 1])
    return all(centre < x for x in others) if lows else all(centre > x for x in others)


def ref_pivots(df, p):
    h, l = df["high"].to_numpy(), df["low"].to_numpy()
    left, right = p["left_span"], p["right_span"]
    return {side: [q for q in range(left, len(df) - right) if _strict(values, q, left, right, lows)]
            for side, values, lows in (("long", l, True), ("short", h, False))}


def ref_signal(df, k, p, osc, sig, all_pivots):
    h, l = df["high"].to_numpy(), df["low"].to_numpy()
    right = p["right_span"]
    emits = {}
    for side, values, lows, want in (("long", l, True, 1), ("short", h, False, -1)):
        pivots = [q for q in all_pivots[side] if q + right <= k]
        emits[side] = False
        if not pivots:
            continue
        q = pivots[-1]
        c = q + right
        if k == c or k - c > p["setup_expiry"]:
            continue
        cands = [x for x in pivots if q - p["max_separation"] <= x <= q - p["min_separation"]]
        if not cands:
            continue
        pv = max(cands)
        if p["divergence_test"]:
            ok = (values[q] < values[pv] and osc[q] > osc[pv]) if lows else (values[q] > values[pv] and osc[q] < osc[pv])
            if not ok:
                continue
        first = None
        for e in range(c + 1, k + 1):
            up = osc[e - 1] <= sig[e - 1] and osc[e] > sig[e]
            down = osc[e - 1] >= sig[e - 1] and osc[e] < sig[e]
            if (up and want == 1) or (down and want == -1):
                first = e
                break
        emits[side] = first == k
    if emits["long"] and not emits["short"]:
        return 1
    if emits["short"] and not emits["long"]:
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
    assert entry["edge_ref"] == "backtest/candidates/klinger_divergence_1662/REPORT.md"
    assert raw["short_entries"] is True and raw["platforms"] == ("futures",)
    assert raw["param_validator"] is not None
    assert NAME in fut._registry.short_entry_strategies()
    assert NAME in fut._registry.DISCOVERY_HIDDEN_STRATEGIES
    assert NAME not in fut.list_strategies()
    assert NAME not in load_registry("spot").STRATEGY_REGISTRY
    assert DEFAULT_PARAM_RANGES[NAME] == seed["optimizer_grid"]
    combos = expand_sweep(dict(DEFAULTS), [(k, list(v)) for k, v in DEFAULT_PARAM_RANGES[NAME].items()])
    assert len(combos) == 768
    for _, params in combos:
        fut._registry.validate_params(NAME, params)
    assert max(_support(p) for _, p in combos) == 188 and _support(DEFAULTS) == 172


def test_walk_forward_warmup_covers_the_largest_support_of_the_grid():
    ranges = DEFAULT_PARAM_RANGES[NAME]
    grid = generate_param_grid(ranges)
    assert strategy_warmup_bars(NAME, grid, ranges) == max(_support(dict(DEFAULTS, **p)) for p in grid) == 188
    one = {"replay_bars": [96]}
    warmup = strategy_warmup_bars(NAME, generate_param_grid(one), one)
    assert warmup == _support(dict(DEFAULTS, replay_bars=96)) == 156
    assert warmup >= max_indicator_lookback(one)
    for other in ("elder_force_pullback", "true_strength_index"):
        assert strategy_warmup_bars(other, generate_param_grid(DEFAULT_PARAM_RANGES[other]),
                                    DEFAULT_PARAM_RANGES[other]) == 199

def test_hand_calculated_rising_falling_equal_and_trend_change_recurrences():
    p = dict(TINY)
    a2, a3 = 2.0 / 3.0, 0.5
    rising = _frame([10, 11, 12, 13, 14], [11, 12, 13, 14, 15], [9, 10, 11, 12, 13], [10, 11, 12, 13, 14],
                    [1.0, 1.0, 1.0, 1.0, 1.0])
    out = _apply(rising, p).iloc[4]
    vf = [0.0, 2.0 * (2.0 / 4.0 - 1.0) * 100.0, 2.0 * (2.0 / 6.0 - 1.0) * 100.0,
          2.0 * (2.0 / 8.0 - 1.0) * 100.0, 2.0 * (2.0 / 10.0 - 1.0) * 100.0]
    fe = se = vf[0]
    sig = 0.0
    for x in vf[1:]:
        fe = a2 * x + (1.0 - a2) * fe
        se = a3 * x + (1.0 - a3) * se
        sig = a2 * (fe - se) + (1.0 - a2) * sig
    assert (out["klinger_trend"], out["klinger_dm"], out["klinger_cm"]) == (1.0, 2.0, 10.0)
    assert out["klinger_volume_force"] == vf[-1] == -160.0
    assert out["klinger_oscillator"] == fe - se and out["klinger_signal_line"] == sig
    assert out["klinger_endpoint_valid"] == 1.0

    falling = _frame([14, 13, 12, 11, 10], [15, 14, 13, 12, 11], [13, 12, 11, 10, 9], [14, 13, 12, 11, 10],
                     [1.0] * 5)
    down = _apply(falling, p).iloc[4]
    assert (down["klinger_trend"], down["klinger_cm"]) == (-1.0, 10.0)
    assert down["klinger_volume_force"] == 160.0
    assert down["klinger_oscillator"] == -out["klinger_oscillator"]

    equal = _frame([10] * 5, [11, 12, 10, 11, 11], [9, 8, 10, 9, 9], [10] * 5, [1.0] * 5)
    flat = _apply(equal, p).iloc[4]
    assert flat["klinger_trend"] == -1.0
    assert flat["klinger_cm"] == 2.0 + 4.0 + 0.0 + 2.0 + 2.0
    assert flat["klinger_volume_force"] == 1.0 * 2.0 * (2.0 / 10.0 - 1.0) * -1.0 * 100.0

    change = _frame([10, 11, 12, 11, 10], [11, 12, 13, 12, 11], [9, 10, 11, 10, 9], [10, 11, 12, 11, 10],
                    [1.0, 2.0, 3.0, 4.0, 5.0])
    turn = _apply(change, p).iloc[4]
    assert turn["klinger_trend"] == -1.0
    assert turn["klinger_cm"] == 2.0 + 2.0 + 2.0
    assert turn["klinger_volume_force"] == 5.0 * 2.0 * (2.0 / 6.0 - 1.0) * -1.0 * 100.0
    restart = _apply(change.iloc[:4], p).iloc[3]
    assert np.isnan(restart["klinger_cm"]) and restart["klinger_endpoint_valid"] == 0.0


def _flat_pair(df, pos):
    out = df.copy()
    level = float(df["high"].iloc[pos - 2]) * 1.5
    for col in ("open", "high", "low", "close"):
        out.iloc[pos - 1:pos + 1, out.columns.get_loc(col)] = level
    return out


def test_zero_range_zero_volume_and_zero_cm_rules():
    p = dict(TINY)
    one_flat = _frame([10, 11, 12, 13, 14], [11, 12, 12, 14, 15], [9, 10, 12, 12, 13], [10, 11, 12, 13, 14],
                      [1.0] * 5)
    row = _apply(one_flat, p).iloc[4]
    assert row["klinger_endpoint_valid"] == 1.0 and row["klinger_cm"] == 2.0 + 2.0 + 0.0 + 2.0 + 2.0
    seed_flat = _frame([10, 11, 12, 13, 14], [10, 12, 13, 14, 15], [10, 10, 11, 12, 13], [10, 11, 12, 13, 14],
                       [1.0] * 5)
    assert _apply(seed_flat, p).iloc[4]["klinger_endpoint_valid"] == 0.0
    df = _walk(400, 3)
    held = _flat_pair(df, 300)
    pos = 300
    out = _apply(held, FAST)
    zero_rows = out.index[(out["klinger_reason"] == "zero_cm")]
    assert len(zero_rows) and out.loc[zero_rows, "signal"].eq(0).all()
    assert (out["klinger_reason"].iloc[pos:pos + 3] == "zero_cm").all()
    quiet = _apply(df.assign(volume=0.0), FAST)
    assert (quiet["signal"] == 0).all()
    valid = quiet["klinger_valid"] == 1.0
    assert valid.any() and (quiet.loc[valid, "klinger_oscillator"] == 0.0).all()
    assert set(quiet.loc[valid, "klinger_reason"]) <= {"no_divergence", "no_pivot_pair", "no_setup"}
    assert (quiet.loc[valid, "klinger_reason"] == "no_divergence").any()


@pytest.mark.parametrize("params", [DEFAULTS, FAST] + DOMAIN_ENDPOINTS)
def test_values_and_decisions_match_an_independent_reference(params):
    df = _walk(560, 7, tick=0.05)
    out = _apply(df, params)
    h, l, c, v = (df[k].to_numpy() for k in ("high", "low", "close", "volume"))
    L = params["replay_bars"]
    S = _support(params)
    osc = np.full(len(df), np.nan)
    sig = np.full(len(df), np.nan)
    for t in range(L, len(df)):
        ref = ref_endpoint(h, l, c, v, t, params)
        row = out.iloc[t]
        assert row["klinger_endpoint_valid"] == (0.0 if ref["zero"] else 1.0)
        if ref["zero"]:
            continue
        for key, col in (("trend", "trend"), ("dm", "dm"), ("cm", "cm"), ("vf", "volume_force"),
                         ("fast", "fast_ema"), ("slow", "slow_ema"), ("osc", "oscillator"),
                         ("sig", "signal_line")):
            assert row[f"klinger_{col}"] == ref[key], (t, key)
        osc[t], sig[t] = ref["osc"], ref["sig"]
    assert (out["klinger_reason"].iloc[:S - 1] == "insufficient_history").all()
    seen = set()
    pivots = ref_pivots(df, params)
    for k in range(S - 1, len(df)):
        if out["klinger_valid"].iloc[k] != 1.0:
            continue
        want = ref_signal(df, k, params, osc, sig, pivots)
        assert int(out["signal"].iloc[k]) == want, k
        seen.add(want)
    if params in (DEFAULTS, FAST):
        assert seen == {-1, 0, 1}


def _states(out, side, state):
    return np.flatnonzero(out[f"klinger_{side}_state"].to_numpy() == state)


def test_event_rules_and_the_signal_cross_ablation_on_one_divergence_run():
    df = _walk(4000, 21, tick=0.05)
    out = _apply(df, FAST)
    with _case("test_event_rules_cover_arming_replacement_expiry_consumption_and_equal_pivots"):
        _event_rules_cover_arming_replacement_expiry_consumption_and_equal_pivots(out.copy())
    with _case("test_signal_cross_ablation_drops_only_the_divergence_test"):
        _signal_cross_ablation_drops_only_the_divergence_test(df, out.copy())


def _event_rules_cover_arming_replacement_expiry_consumption_and_equal_pivots(out):
    reasons = set(out["klinger_reason"])
    assert {"entry_long", "entry_short", "setup_armed", "awaiting_cross", "setup_consumed", "no_divergence",
            "no_pivot_pair", "no_setup"} <= reasons
    expiry = FAST["setup_expiry"]
    for side in ("long", "short"):
        emitted = _states(out, side, 1)
        assert len(emitted)
        confirm = out[f"klinger_{side}_confirm_ts_ms"].to_numpy()
        eval_ts = out["klinger_eval_ts_ms"].to_numpy()
        lag = (eval_ts[emitted] - confirm[emitted]) / FOUR_H
        assert lag.min() >= 1 and lag.max() <= expiry
        cross = out["klinger_cross"].to_numpy()
        assert (cross[emitted] == (1.0 if side == "long" else -1.0)).all()
        armed = _states(out, side, 2)
        assert len(armed) and (out["signal"].to_numpy()[armed] != (1 if side == "long" else -1)).all()
        for e in emitted[:20]:
            after = out[f"klinger_{side}_state"].to_numpy()[e + 1:e + expiry + 1]
            same = confirm[e + 1:e + expiry + 1] == confirm[e]
            assert ((after[same] == 4) | (after[same] == 0)).all()
    long_p1 = out["klinger_long_pivot1_price"].to_numpy()
    long_p2 = out["klinger_long_pivot2_price"].to_numpy()
    state = out["klinger_long_state"].to_numpy()
    equal = np.flatnonzero((long_p1 == long_p2) & np.isfinite(long_p1))
    assert len(equal) and (state[equal] == 7).all()
    confirm = out["klinger_long_confirm_ts_ms"].to_numpy()
    replaced = np.flatnonzero((np.diff(confirm) > 0) & np.isfinite(confirm[:-1]) & (state[:-1] == 3)) + 1
    assert len(replaced)
    assert (out["signal"].to_numpy()[np.flatnonzero(out["klinger_reason"].to_numpy() == "opposite_events")] == 0).all()


def _signal_cross_ablation_drops_only_the_divergence_test(df, div):
    abl = _apply(df, dict(FAST, divergence_test=False))
    for col in ("klinger_oscillator", "klinger_signal_line", "klinger_cross", "klinger_long_confirm_ts_ms",
                "klinger_short_pivot1_ts_ms"):
        np.testing.assert_array_equal(div[col].to_numpy(), abl[col].to_numpy())
    for side, sign in (("long", 1), ("short", -1)):
        d = div["signal"].to_numpy() == sign
        a = abl["signal"].to_numpy() == sign
        assert not (d & ~a).any() and a.sum() > d.sum()
    assert "no_divergence" not in set(abl["klinger_reason"])


def _holds_at_end(df, params, reason):
    before = df.copy()
    out = _apply(df, params)
    pd.testing.assert_frame_equal(df, before)
    for col in df.columns:
        pd.testing.assert_series_equal(out[col], before[col])
    assert int(out["signal"].iloc[-1]) == 0
    assert out["klinger_valid"].iloc[-1] == 0.0
    assert out["klinger_reason"].iloc[-1] == reason, out["klinger_reason"].iloc[-1]
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
    (lambda df: df.assign(volume=np.r_[df["volume"].to_numpy()[:-1], np.nan]), "nonfinite_input"),
    (lambda df: df.assign(low=np.r_[df["low"].to_numpy()[:-1], 0.0]), "nonpositive_price"),
    (lambda df: df.assign(close=np.r_[df["close"].to_numpy()[:-1], -1.0]), "nonpositive_price"),
    (lambda df: df.assign(open=np.r_[df["open"].to_numpy()[:-1], df["high"].iloc[-1] * 2]), "invalid_candle_geometry"),
    (lambda df: df.assign(high=np.r_[df["high"].to_numpy()[:-1], df["low"].iloc[-1] / 2]), "invalid_candle_geometry"),
    (lambda df: df.assign(volume=np.r_[df["volume"].to_numpy()[:-1], -1.0]), "negative_volume"),
    (lambda df: _with_index(df, list(df.index[:-1]) + [df.index[-2]]), "timestamp_order"),
    (lambda df: _with_index(df, list(df.index[:-2]) + [df.index[-1], df.index[-2]]), "timestamp_order"),
    (lambda df: _shift_index(df, len(df) - 1, pd.Timedelta(hours=4)), "cadence_gap"),
    (lambda df: _shift_index(df, len(df) - 150, pd.Timedelta(hours=1)), "cadence_gap"),
    (lambda df: df.iloc[-(_support(DEFAULTS) - 1):], "insufficient_history"),
]


def test_invalid_entry_data_is_a_reasoned_hold_that_preserves_the_frame():
    for i, (mutate, reason) in enumerate(INVALID_ENTRY_DATA):
        with _case(f"{i}-{reason}"):
            _holds_at_end(mutate(_walk(400, 3)), DEFAULTS, reason)


@pytest.mark.parametrize("params", [DEFAULTS, FAST])
def test_reads_stay_inside_the_support_span(params):
    df = _walk(900, 5)
    full = _apply(df, params)
    S = _support(params)
    for k in (S + 50, 500, 899):
        outside = df.copy()
        for col in INPUT_COLUMNS:
            outside.iloc[k - S, outside.columns.get_loc(col)] = np.nan
        outside.iloc[:k - S, outside.columns.get_loc("volume")] *= 7.0
        got = _apply(outside, params).iloc[k]
        pd.testing.assert_series_equal(got, full.iloc[k], check_names=False)
        inside = df.copy()
        inside.iloc[k - S + 1, inside.columns.get_loc("volume")] = np.nan
        held = _apply(inside, params).iloc[k]
        assert held["klinger_reason"] == "nonfinite_input" and int(held["signal"]) == 0


def test_received_out_of_order_rows_hold_only_inside_their_span():
    df = _walk(800, 5)
    clean = _apply(df, DEFAULTS)
    S = _support(DEFAULTS)
    marked = df.copy()
    bad = 300
    marked.attrs[FRAME_RECEIVED_ORDER_DEFECTS_ATTR] = [int(df.index[bad].value // 1_000_000)]
    out = _apply(marked, DEFAULTS)
    assert (out["klinger_reason"].iloc[bad:bad + S] == "timestamp_order").all()
    assert (out["signal"].iloc[bad:bad + S] == 0).all()
    pd.testing.assert_frame_equal(out.iloc[:bad], clean.iloc[:bad])
    pd.testing.assert_frame_equal(out.iloc[bad + S:], clean.iloc[bad + S:])


INVALID_PARAMS = [
    {"fast_period": 1}, {"fast_period": 121}, {"slow_period": 2}, {"slow_period": 151},
    {"fast_period": 55, "slow_period": 55}, {"slow_period": 112}, {"signal_period": 112},
    {"signal_period": 1}, {"replay_bars": 191}, {"left_span": 0}, {"right_span": 11},
    {"min_separation": 0}, {"min_separation": 41, "max_separation": 40}, {"max_separation": 101},
    {"setup_expiry": 0}, {"setup_expiry": 51}, {"replay_bars": 150, "max_separation": 40},
    {"fast_period": 12.5}, {"fast_period": True}, {"fast_period": "34"}, {"slow_period": float("nan")},
    {"replay_bars": float("inf")}, {"divergence_test": 1}, {"divergence_test": "true"},
]


def test_invalid_parameters_are_explicit_errors():
    for params in INVALID_PARAMS:
        with _case(params):
            with pytest.raises(ValueError):
                _apply(_walk(300, 1), dict(DEFAULTS, **params))
            with pytest.raises(ValueError):
                _reg()._registry.validate_params(NAME, dict(DEFAULTS, **params))


def test_support_bound_and_integral_floats_are_accepted():
    df = _walk(400, 1)
    at_bound = dict(DEFAULTS, replay_bars=140, max_separation=40)
    assert _support(at_bound) == 200
    _reg()._registry.validate_params(NAME, at_bound)
    with pytest.raises(ValueError):
        _reg()._registry.validate_params(NAME, dict(at_bound, replay_bars=141))
    floats = dict(DEFAULTS, fast_period=34.0, slow_period=55.0, replay_bars=112.0)
    pd.testing.assert_frame_equal(_apply(df, floats), _apply(df, DEFAULTS))


@pytest.mark.parametrize("params", DOMAIN_ENDPOINTS + [DEFAULTS])
def test_support_rows_are_required_and_sufficient_at_every_supported_bound(params):
    df = _walk(450, 9)
    S = _support(params)
    full = _apply(df, params)
    for end in range(S, 450, 41):
        exact = _apply(df.iloc[end - S:end], params)
        assert exact["klinger_reason"].iloc[-1] != "insufficient_history"
        assert exact["klinger_reason"].iloc[-1] == full["klinger_reason"].iloc[end - 1]
        short = _apply(df.iloc[end - S + 1:end], params)
        assert short["klinger_reason"].iloc[-1] == "insufficient_history"


def _frozen(key, bars):
    manifest = om.load_manifest(MANIFEST)
    candles = om.load_candles(om.dataset_by_key(manifest, key))
    frame = candles[INPUT_COLUMNS].iloc[-bars:].copy()
    return declare_frame_cadence(frame, "4h", TIMESTAMP_KIND_OPEN)


DECISION_COLUMNS = ["klinger_oscillator", "klinger_signal_line", "klinger_prev_oscillator",
                    "klinger_prev_signal_line", "klinger_cross", "klinger_valid", "klinger_long_state",
                    "klinger_short_state", "klinger_long_pivot1_ts_ms", "klinger_short_pivot2_ts_ms",
                    "klinger_reason_code", "signal"]


def test_full_series_prefix_exact_span_and_200_row_tail_agree_for_grid_samples_and_bounds():
    df = _frozen("ETH 4h", 700)
    specs = [(k, list(v)) for k, v in DEFAULT_PARAM_RANGES[NAME].items()]
    grid = [p for _, p in expand_sweep(dict(DEFAULTS), specs)]
    for params in grid[::97] + [grid[-1]] + DOMAIN_ENDPOINTS + [dict(DEFAULTS, divergence_test=False)]:
        S = _support(params)
        full = _apply(df, params)
        decisions = np.flatnonzero(full["signal"].to_numpy() != 0)
        ends = sorted({int(i) + 1 for i in decisions[decisions >= 200][:3]} | {250, 450, 700})
        for end in ends:
            for part in (df.iloc[:end], df.iloc[end - 200:end], df.iloc[end - S:end]):
                got = _apply(part, params)[DECISION_COLUMNS].iloc[-1]
                pd.testing.assert_series_equal(got, full[DECISION_COLUMNS].iloc[end - 1], check_names=False)


def _runtime(df, kind, cutoff=None, boundary=None):
    out = df.copy()
    declare_frame_cadence(out, "4h", kind)
    out.attrs[FRAME_DECISION_TIME_ATTR] = {} if cutoff is None else {"cutoff_ms": cutoff, "boundary_ms": boundary}
    return out


def _first(out, side):
    rows = np.flatnonzero(out["signal"].to_numpy() == side)
    assert len(rows)
    return int(rows[0])


def test_check_time_frames_hold_unless_the_last_bar_is_the_latest_closed_bar():
    base = _walk(1500, 11)
    hist = _apply(base, DEFAULTS)
    i = _first(hist, 1)
    for kind in (TIMESTAMP_KIND_OPEN, TIMESTAMP_KIND_NATIVE_CLOSE):
        with _case(kind):
            _check_time_frames_hold_unless_the_last_bar_is_the_latest_closed_bar(base, hist, i, kind)


def _check_time_frames_hold_unless_the_last_bar_is_the_latest_closed_bar(base, hist, i, kind):
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
        assert out["klinger_reason"].iloc[-1] == reason
        assert out["klinger_oscillator"].iloc[-1] == hist["klinger_oscillator"].iloc[i]
        assert int(out["signal"].iloc[-2]) == int(hist["signal"].iloc[i - 1])


def _composer(df, params, side="", position_ctx=None, closes=("tiered_tp_pct",), open_name=NAME):
    reg = _reg()
    evaluation = evaluate_open_close(
        reg.apply_strategy, reg.get_strategy, df, open_name, open_name, list(closes), side, params=params,
        position_ctx=position_ctx, close_evaluate=close_evaluate,
        close_params_by_name={"tiered_tp_pct": CLOSE[0]["params"]} if "tiered_tp_pct" in closes else None)
    return evaluation, finalize_decision(evaluation, side)


def _entry_frame(side):
    base = _walk(1500, 11)
    i = _first(_apply(base, DEFAULTS), 1 if side == "long" else -1)
    return base.iloc[:i + 1].copy()


def test_composer_emits_both_directions_holds_entries_while_open_and_closes_on_its_own_opposite_entry():
    long_frame = _entry_frame("long")
    short_frame = _entry_frame("short")
    with _case("test_composer_emits_both_directions_and_holds_entries_while_a_position_is_open"):
        _composer_emits_both_directions_and_holds_entries_while_a_position_is_open(long_frame, short_frame)
    with _case("test_close_fallback_on_the_candidate_closes_on_its_own_opposite_entry"):
        _close_fallback_on_the_candidate_closes_on_its_own_opposite_entry(short_frame.copy())


def _composer_emits_both_directions_and_holds_entries_while_a_position_is_open(long_frame, short_frame):
    _, flat_long = _composer(long_frame.copy(), DEFAULTS)
    _, flat_short = _composer(short_frame.copy(), DEFAULTS)
    assert (flat_long["open_action"], flat_long["signal"]) == ("long", 1)
    assert (flat_short["open_action"], flat_short["signal"]) == ("short", -1)
    for frame in (long_frame.copy(), short_frame.copy()):
        mark = float(frame["close"].iloc[-1])
        for side in ("long", "short"):
            ctx = {"side": side, "avg_cost": mark, "entry_atr": 50.0, "current_quantity": 1.0,
                   "initial_quantity": 1.0}
            _, held = _composer(frame, DEFAULTS, side, ctx)
            assert held["signal"] == 0 and held["close_fraction"] == 0.0


def _close_fallback_on_the_candidate_closes_on_its_own_opposite_entry(frame):
    mark = float(frame["close"].iloc[-1])
    ctx = {"side": "long", "avg_cost": mark, "entry_atr": 50.0, "current_quantity": 1.0, "initial_quantity": 1.0}
    _, decision = _composer(frame, DEFAULTS, "long", ctx, closes=(NAME,))
    assert decision["close_strategy"] == NAME and decision["close_fraction"] == 1.0 and decision["signal"] == -1


def test_invalid_entry_frame_keeps_the_close_owner_executable():
    for mutation in ("volume", "geometry", "cadence", "stale", "zero_cm"):
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
    elif mutation == "zero_cm":
        df = _flat_pair(df, len(df) - 2)
    else:
        last = int(df.index[-1].value // 1_000_000)
        df = _runtime(df, TIMESTAMP_KIND_OPEN, last + 3 * FOUR_H, last + FOUR_H)
    mark = float(df["close"].iloc[-1])
    ctx = {"side": "long", "avg_cost": mark / 1.05, "entry_atr": 1.0, "current_quantity": 1.0,
           "initial_quantity": 1.0}
    evaluation, decision = _composer(df, DEFAULTS, "long", ctx)
    assert evaluation.open_result_df["klinger_valid"].iloc[-1] == 0.0
    assert decision["open_action"] == "none"
    assert decision["close_strategy"] == "tiered_tp_pct" and decision["close_fraction"] == 1.0
    assert decision["signal"] == -1
    _, decision = _composer(df, DEFAULTS, "short", dict(ctx, side="short", avg_cost=mark * 1.05))
    assert decision["open_action"] == "none" and decision["close_fraction"] == 1.0 and decision["signal"] == 1
    _, decision = _composer(df, DEFAULTS, "long", dict(ctx, avg_cost=mark / 1.015))
    assert decision["close_fraction"] == 0.0 and decision["signal"] == 0
    _, decision = _composer(df, DEFAULTS, "", None)
    assert decision["signal"] == 0 and decision["open_action"] == "none"


def test_close_and_stop_owners_pass_strict_capability_validation():
    report = validate_close_capabilities(close_refs=CLOSE, comparison_mode=None, platform="hyperliquid",
                                         strategy_type="perps", phase="preflight").to_dict()
    assert report["close_eligibility"] == "eligible" and not report["approximations"] and not report["refusals"]


def _bt(params, **kw):
    base = dict(initial_capital=1000.0, platform="hyperliquid", open_strategy={"name": NAME, "params": params},
                close_strategies=CLOSE, stop_loss_atr_mult=1.0, direction="both")
    base.update(kw)
    return Backtester(**base)


def test_simulator_fills_next_open_both_arms_and_keeps_closing_after_entry_inputs_fail():
    df = _walk(3000, 5)
    sig = ensure_atr_indicator(_apply(df, FAST))
    with _case("test_simulator_fills_next_open_with_explicit_exits_and_no_reversal_or_scale_in"):
        _simulator_fills_next_open_with_explicit_exits_and_no_reversal_or_scale_in(df, sig.copy())
    with _case("test_simulator_long_only_arm_never_opens_shorts"):
        _simulator_long_only_arm_never_opens_shorts(sig.copy())
    with _case("test_simulator_keeps_closing_and_stopping_after_entry_inputs_fail"):
        _simulator_keeps_closing_and_stopping_after_entry_inputs_fail(df, sig.copy())


def _simulator_fills_next_open_with_explicit_exits_and_no_reversal_or_scale_in(df, sig):
    res = _bt(FAST).run(sig, strategy_name=NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    trades = res["trades"]
    assert {t["side"] for t in trades} == {"long", "short"}
    reasons = {t["exit_reason"].split(":")[0] for t in trades}
    assert reasons <= {"sl", "tiered_tp_pct", "end_of_data"} and "tiered_tp_pct" in reasons and "sl" in reasons
    spans = []
    for t in trades:
        entry = pd.Timestamp(t["entry_date"])
        pos = sig.index.get_loc(entry)
        assert int(sig["signal"].iloc[pos - 1]) == (1 if t["side"] == "long" else -1)
        assert t["entry_price"] == pytest.approx(df["open"].iloc[pos], rel=0.01)
        spans.append((entry, pd.Timestamp(t["exit_date"])))
    spans.sort()
    for (_, prev_exit), (next_entry, _) in zip(spans, spans[1:]):
        assert next_entry >= prev_exit


def _simulator_long_only_arm_never_opens_shorts(sig):
    res = _bt(FAST, direction="long").run(sig, strategy_name=NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    assert res["trades"] and {t["side"] for t in res["trades"]} == {"long"}


def _simulator_keeps_closing_and_stopping_after_entry_inputs_fail(df, sig):
    first = next(int(i) for i in np.flatnonzero(sig["signal"].to_numpy() != 0)
                 if i > 300 and np.isfinite(sig["atr"].iloc[i]))
    corrupt = df.copy()
    corrupt.iloc[first + 1:, corrupt.columns.get_loc("volume")] = np.nan
    bad = ensure_atr_indicator(_apply(corrupt, FAST))
    assert (bad["signal"].iloc[:first + 1] == sig["signal"].iloc[:first + 1]).all()
    assert (bad["signal"].iloc[first + 1:] == 0).all()
    res = _bt(FAST).run(bad, strategy_name=NAME, symbol="BTC/USDT", timeframe="4h", save=False)
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
@pytest.mark.parametrize("params", [DEFAULTS, FAST, DOMAIN_ENDPOINTS[3]])
def test_entry_parity_on_frozen_candles(window, params):
    frame = compute_parity_frame(_frozen("BTC 4h", 460), cfg=_cfg(params), window=window)
    result = summarize(frame)
    assert result["bars_compared"] > 0
    assert result["clean"], frame[~frame["match"]].head()


@pytest.mark.parametrize("window", [200, None])
def test_composed_parity_covers_both_sides_with_the_close_owner(window):
    cfg = _cfg(FAST, close_refs=json.loads(json.dumps(CLOSE)), direction="both")
    frame = compute_parity_frame(_frozen("SOL 4h", 900), cfg=cfg, window=window)
    result = summarize(frame)
    assert result["mismatches"] == 0 and result["clean"], frame[~frame["match"]].head()
    assert (frame["live_open_action"] == "long").any() and (frame["live_open_action"] == "short").any()
    closing = frame[frame["live_close_fraction"] > 0]
    assert (closing["live_signal"] == -1).any() and (closing["live_signal"] == 1).any()


@pytest.mark.parametrize("window", [200, None])
def test_batched_explicit_paper_parity_matches_solo(window):
    cfg = _cfg(FAST, close_refs=json.loads(json.dumps(CLOSE)), direction="both", batched=True)
    frame = compute_parity_frame(_frozen("BTC 4h", 520), cfg=cfg, window=window)
    result = summarize(frame)
    assert result["bars_compared"] > 0
    assert result["clean"] and result["batch_clean"], frame[~frame["match"]].head()
    assert (frame["live_open_action"] != "none").any()
