import contextlib
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
from backtester import Backtester
from close_registry_loader import evaluate as close_evaluate
from closed_bar import (
    FRAME_DECISION_TIME_ATTR,
    FRAME_RECEIVED_ORDER_DEFECTS_ATTR,
    TIMESTAMP_KIND_OPEN,
    declare_frame_cadence,
)
from eval_windows import expand_sweep
from optimizer import DEFAULT_PARAM_RANGES
from parity_diff import ParityConfig
from registry_loader import load_registry
from strategy_composition import evaluate_open_close, finalize_decision

NAME = "schaff_trend_cycle"
MANIFEST = os.path.join(_REPO, "backtest", "candidates", "schaff_trend_cycle_1651_manifest.json")
SEED = os.path.join(_REPO, "backtest", "candidates", "schaff_trend_cycle_1651", "candidate_seed.json")
CLOSE = [{"name": "tiered_tp_pct", "params": {}}]
INPUT_COLUMNS = ["open", "high", "low", "close", "volume"]
REPLAY = 192
NEED = 193
FOUR_H = 14_400_000
DEFAULTS = {"fast_period": 23, "slow_period": 50, "cycle_length": 10, "smoothing": 0.5,
            "lower_threshold": 25.0, "upper_threshold": 75.0}
DOMAIN_ENDPOINTS = [
    dict(DEFAULTS, fast_period=2, slow_period=3, cycle_length=2, smoothing=0.25, lower_threshold=0.5,
         upper_threshold=99.5),
    dict(DEFAULTS, fast_period=32, slow_period=64, cycle_length=16, smoothing=1.0),
    dict(DEFAULTS, fast_period=2, slow_period=64, cycle_length=16, smoothing=0.25),
    dict(DEFAULTS, fast_period=31, slow_period=32, cycle_length=2, smoothing=1.0),
]


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


def _walk(n, seed, start=100.0, sigma=0.02):
    rng = np.random.default_rng(seed)
    close = start * np.exp(np.cumsum(rng.normal(0.0, sigma, n)))
    open_ = np.concatenate([[start], close[:-1]])
    high = np.maximum(open_, close) * (1.0 + rng.uniform(0.001, 0.01, n))
    low = np.minimum(open_, close) * (1.0 - rng.uniform(0.001, 0.01, n))
    volume = rng.uniform(100.0, 1000.0, n)
    df = pd.DataFrame({"open": open_, "high": high, "low": low, "close": close, "volume": volume})
    df.index = pd.date_range("2026-01-01", periods=n, freq="4h")
    return declare_frame_cadence(df, "4h", TIMESTAMP_KIND_OPEN)


def ref_endpoint(close, t, fast, slow, cycle, smooth):
    w = [float(x) for x in close[t - REPLAY + 1:t + 1]]
    af = 2.0 / (fast + 1)
    asl = 2.0 / (slow + 1)
    ef = es = w[0]
    macd = [ef - es]
    for x in w[1:]:
        ef = af * x + (1.0 - af) * ef
        es = asl * x + (1.0 - asl) * es
        macd.append(ef - es)
    kept = macd[slow - 1:]
    zero = False
    s1 = []
    raw1 = None
    for k in range(cycle - 1, len(kept)):
        win = kept[k - cycle + 1:k + 1]
        lo, hi = min(win), max(win)
        if not hi - lo > 0:
            zero = True
            raw1 = 0.0
        else:
            raw1 = 100.0 * (kept[k] - lo) / (hi - lo)
        s1.append(raw1 if not s1 else s1[-1] + smooth * (raw1 - s1[-1]))
    value = None
    raw2 = None
    for k in range(cycle - 1, len(s1)):
        win = s1[k - cycle + 1:k + 1]
        lo, hi = min(win), max(win)
        if not hi - lo > 0:
            zero = True
            raw2 = 0.0
        else:
            raw2 = 100.0 * (s1[k] - lo) / (hi - lo)
        value = raw2 if value is None else value + smooth * (raw2 - value)
    return {"value": value, "macd": macd[-1], "stoch1_raw": raw1, "stoch1": s1[-1], "stoch2_raw": raw2,
            "zero": zero}


def ref_decision(cur, prev, lower, upper):
    if cur["zero"] or prev["zero"]:
        return None
    if prev["value"] <= lower < cur["value"] and cur["macd"] > prev["macd"]:
        return 1
    if prev["value"] >= upper > cur["value"] and cur["macd"] < prev["macd"]:
        return -1
    return 0


REFERENCE_PARAMS = [DEFAULTS, dict(DEFAULTS, fast_period=12, slow_period=26, cycle_length=8,
                                   lower_threshold=20.0, upper_threshold=80.0)] + DOMAIN_ENDPOINTS


def _values_and_decisions_match_an_independent_reference(df, params):
    out = _apply(df, params)
    close = df["close"].to_numpy()
    p = (params["fast_period"], params["slow_period"], params["cycle_length"], params["smoothing"])
    assert (out["stc_reason"].iloc[:NEED - 1] == "insufficient_history").all()
    seen = set()
    for t in range(REPLAY - 1, len(df)):
        cur = ref_endpoint(close, t, *p)
        row = out.iloc[t]
        assert row["stc_value_valid"] == (0.0 if cur["zero"] else 1.0)
        if not cur["zero"]:
            for key in ("value", "macd", "stoch1_raw", "stoch1", "stoch2_raw"):
                assert row[f"stc_{key}" if key != "value" else "stc_value"] == cur[key], (t, key)
        if t < NEED - 1:
            continue
        prev = ref_endpoint(close, t - 1, *p)
        want = ref_decision(cur, prev, params["lower_threshold"], params["upper_threshold"])
        if want is None:
            assert int(row["signal"]) == 0 and row["stc_valid"] == 0.0 and row["stc_reason"] == "zero_range"
            continue
        assert row["stc_valid"] == 1.0
        assert row["stc_prev_value"] == prev["value"] and row["stc_prev_macd"] == prev["macd"]
        assert int(row["signal"]) == want, t
        assert row["stc_reason_code"] == {1: 1.0, -1: -1.0}.get(want, row["stc_reason_code"])
        seen.add(want)
    if params is DEFAULTS:
        assert seen == {-1, 0, 1}


def _first(out, side, reason=None):
    rows = np.flatnonzero(out["signal"].to_numpy() == side) if reason is None else \
        np.flatnonzero(out["stc_reason"].to_numpy() == reason)
    assert len(rows)
    return int(rows[0])


def _holds_at_end(df, params, reason):
    before = df.copy()
    out = _apply(df, params)
    pd.testing.assert_frame_equal(df, before)
    for col in df.columns:
        pd.testing.assert_series_equal(out[col], before[col])
    assert int(out["signal"].iloc[-1]) == 0
    assert out["stc_valid"].iloc[-1] == 0.0
    assert out["stc_reason"].iloc[-1] == reason, out["stc_reason"].iloc[-1]
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
    (lambda df: df.assign(low=np.r_[df["low"].to_numpy()[:-1], 0.0]), "nonpositive_price"),
    (lambda df: df.assign(close=np.r_[df["close"].to_numpy()[:-1], -1.0]), "nonpositive_price"),
    (lambda df: df.assign(open=np.r_[df["open"].to_numpy()[:-1], df["high"].iloc[-1] * 2]), "invalid_candle_geometry"),
    (lambda df: df.assign(high=np.r_[df["high"].to_numpy()[:-1], df["low"].iloc[-1] / 2]), "invalid_candle_geometry"),
    (lambda df: df.assign(volume=np.r_[df["volume"].to_numpy()[:-1], -1.0]), "negative_volume"),
    (lambda df: _with_index(df, list(df.index[:-1]) + [df.index[-2]]), "timestamp_order"),
    (lambda df: _with_index(df, list(df.index[:-2]) + [df.index[-1], df.index[-2]]), "timestamp_order"),
    (lambda df: _shift_index(df, len(df) - 1, pd.Timedelta(hours=4)), "cadence_gap"),
    (lambda df: _shift_index(df, len(df) - 150, pd.Timedelta(hours=1)), "cadence_gap"),
    (lambda df: df.iloc[-(NEED - 1):], "insufficient_history"),
]


def _received_out_of_order_rows_hold_only_inside_their_span():
    df = _walk(700, 5)
    clean = _apply(df, DEFAULTS)
    marked = df.copy()
    bad = 300
    marked.attrs[FRAME_RECEIVED_ORDER_DEFECTS_ATTR] = [int(df.index[bad].value // 1_000_000)]
    out = _apply(marked, DEFAULTS)
    assert (out["stc_reason"].iloc[bad:bad + NEED] == "timestamp_order").all()
    assert (out["signal"].iloc[bad:bad + NEED] == 0).all()
    pd.testing.assert_frame_equal(out.iloc[:bad], clean.iloc[:bad])
    pd.testing.assert_frame_equal(out.iloc[bad + NEED:], clean.iloc[bad + NEED:])


def test_a_defect_holds_only_inside_its_span_and_recovery_matches_a_clean_frame(subtests):
    with subtests.test(case="nonfinite_volume_and_cadence_gap"):
        _a_defect_holds_only_inside_its_span_and_recovery_matches_a_clean_frame()
    with subtests.test(case="received_out_of_order"):
        _received_out_of_order_rows_hold_only_inside_their_span()


def _a_defect_holds_only_inside_its_span_and_recovery_matches_a_clean_frame():
    df = _walk(700, 5)
    bad = 300
    corrupt = df.copy()
    corrupt.iloc[bad, corrupt.columns.get_loc("volume")] = np.nan
    out = _apply(corrupt, DEFAULTS)
    clean = _apply(df, DEFAULTS)
    assert (out["stc_reason"].iloc[bad:bad + NEED] == "nonfinite_input").all()
    assert (out["stc_valid"].iloc[bad:bad + NEED] == 0.0).all()
    pd.testing.assert_frame_equal(out.iloc[:bad], clean.iloc[:bad])
    pd.testing.assert_frame_equal(out.iloc[bad + NEED:], clean.iloc[bad + NEED:])
    gapped = df.drop(df.index[bad])
    out = _apply(gapped, DEFAULTS)
    assert (out["stc_reason"].iloc[bad:bad + NEED - 1] == "cadence_gap").all()
    assert out["stc_reason"].iloc[bad + NEED - 1] != "cadence_gap"


def _frozen(key, bars):
    manifest = om.load_manifest(MANIFEST)
    candles = om.load_candles(om.dataset_by_key(manifest, key))
    frame = candles[INPUT_COLUMNS].iloc[-bars:].copy()
    return declare_frame_cadence(frame, "4h", TIMESTAMP_KIND_OPEN)


def _grid_and_endpoints():
    specs = [(k, list(v)) for k, v in DEFAULT_PARAM_RANGES[NAME].items()]
    return [p for _, p in expand_sweep(dict(DEFAULTS), specs)] + DOMAIN_ENDPOINTS


DECISION_COLUMNS = ["stc_value", "stc_prev_value", "stc_macd", "stc_prev_macd", "stc_valid",
                    "stc_reason_code", "signal"]


def _runtime(df, kind, cutoff=None, boundary=None):
    out = df.copy()
    declare_frame_cadence(out, "4h", kind)
    out.attrs[FRAME_DECISION_TIME_ATTR] = {} if cutoff is None else {"cutoff_ms": cutoff, "boundary_ms": boundary}
    return out


def _composer(df, params, side="", position_ctx=None, closes=("tiered_tp_pct",), open_name=NAME):
    reg = _reg()
    evaluation = evaluate_open_close(
        reg.apply_strategy, reg.get_strategy, df, open_name, open_name, list(closes), side, params=params,
        position_ctx=position_ctx, close_evaluate=close_evaluate,
        close_params_by_name={"tiered_tp_pct": {}} if "tiered_tp_pct" in closes else None)
    return evaluation, finalize_decision(evaluation, side)


def _entry_frame(side):
    base = _walk(900, 11)
    i = _first(_apply(base, DEFAULTS), 1 if side == "long" else -1)
    return base.iloc[:i + 1].copy()


def test_close_fallback_on_the_candidate_closes_on_its_own_opposite_entry():
    frame = _entry_frame("short")
    mark = float(frame["close"].iloc[-1])
    ctx = {"side": "long", "avg_cost": mark, "entry_atr": 50.0, "current_quantity": 1.0, "initial_quantity": 1.0}
    _, decision = _composer(frame, DEFAULTS, "long", ctx, closes=(NAME,))
    assert decision["close_strategy"] == NAME and decision["close_fraction"] == 1.0 and decision["signal"] == -1


def _bt(params, **kw):
    base = dict(initial_capital=1000.0, platform="hyperliquid", open_strategy={"name": NAME, "params": params},
                close_strategies=CLOSE, stop_loss_atr_mult=1.0, direction="both")
    base.update(kw)
    return Backtester(**base)


def test_simulator_fills_next_open_with_explicit_exits_partials_no_reversal_or_scale_in_and_long_only_opens_no_shorts():
    df = _walk(1500, 5)
    sig = ensure_atr_indicator(_apply(df, DEFAULTS))
    with _case("test_simulator_fills_next_open_with_explicit_exits_partials_and_no_reversal_or_scale_in"):
        _simulator_fills_next_open_with_explicit_exits_partials_and_no_reversal_or_scale_in(df, sig)
    with _case("test_simulator_long_only_arm_never_opens_shorts"):
        _simulator_long_only_arm_never_opens_shorts(sig)


def _simulator_fills_next_open_with_explicit_exits_partials_and_no_reversal_or_scale_in(df, sig):
    res = _bt(DEFAULTS).run(sig, strategy_name=NAME, symbol="BTC/USDT", timeframe="4h", save=False)
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
        spans.append((entry, max(pd.Timestamp(leg["exit_date"]) for leg in legs)))
    spans.sort()
    for (_, prev_exit), (next_entry, _) in zip(spans, spans[1:]):
        assert next_entry >= prev_exit
    assert any(len(legs) > 1 for legs in by_entry.values())


def _simulator_long_only_arm_never_opens_shorts(sig):
    res = _bt(DEFAULTS, direction="long").run(sig, strategy_name=NAME, symbol="BTC/USDT", timeframe="4h",
                                              save=False)
    assert res["trades"] and {t["side"] for t in res["trades"]} == {"long"}


def _cfg(params, **kw):
    base = dict(strategy_name=NAME, params=params, registry="futures", platform="hyperliquid",
                symbol="BTC", timeframe="4h")
    base.update(kw)
    return ParityConfig(**base)
