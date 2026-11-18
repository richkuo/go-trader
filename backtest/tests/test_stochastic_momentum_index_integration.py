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
from closed_bar import (
    FRAME_DECISION_TIME_ATTR,
    FRAME_RECEIVED_ORDER_DEFECTS_ATTR,
    TIMESTAMP_KIND_OPEN,
    declare_frame_cadence,
)
from parity_diff import ParityConfig
from registry_loader import load_registry

NAME = "stochastic_momentum_index"
MANIFEST = os.path.join(_REPO, "backtest", "candidates", "stochastic_momentum_index_1663_manifest.json")
SEED = os.path.join(_REPO, "backtest", "candidates", "stochastic_momentum_index_1663", "candidate_seed.json")
TIERS = {"tp_tiers": [{"profit_pct": 0.03, "close_fraction": 1.0}]}
CLOSE = [{"name": "tiered_tp_pct", "params": TIERS}]
INPUT_COLUMNS = ["open", "high", "low", "close", "volume"]
FOUR_H = 14_400_000
DEFAULTS = {"range_period": 10, "smoothing_period": 3, "signal_period": 3, "recursion_length": 60,
            "oversold": -40.0, "overbought": 40.0}
NEED = 71
DOMAIN_ENDPOINTS = [
    dict(DEFAULTS, range_period=1, smoothing_period=1, signal_period=1, recursion_length=8, oversold=-99.5,
         overbought=99.5),
    dict(DEFAULTS, range_period=190, smoothing_period=1, signal_period=1, recursion_length=8),
    dict(DEFAULTS, range_period=6, smoothing_period=24, signal_period=24, recursion_length=192),
    dict(DEFAULTS, range_period=14, smoothing_period=4, signal_period=2, recursion_length=32),
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


def _need(params):
    return int(params["recursion_length"]) + int(params["range_period"]) + 1


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


def ref_endpoint(high, low, close, t, params):
    k, length = int(params["range_period"]), int(params["recursion_length"])
    a = 2.0 / (int(params["smoothing_period"]) + 1)
    b = 2.0 / (int(params["signal_period"]) + 1)
    keep_a, keep_b = 1.0 - a, 1.0 - b
    rel, rng = [], []
    for i in range(t - length, t + 1):
        hh = max(float(x) for x in high[i - k + 1:i + 1])
        ll = min(float(x) for x in low[i - k + 1:i + 1])
        rel.append(float(close[i]) - (hh + ll) / 2.0)
        rng.append(hh - ll)
    n1 = n2 = rel[0]
    d1 = d2 = rng[0]
    zero = not d2 > 0
    osc = 200.0 * n2 / d2 if d2 > 0 else float("nan")
    line = osc
    for j in range(1, length + 1):
        n1 = a * rel[j] + keep_a * n1
        n2 = a * n1 + keep_a * n2
        d1 = a * rng[j] + keep_a * d1
        d2 = a * d1 + keep_a * d2
        if not d2 > 0:
            zero = True
            osc = float("nan")
        else:
            osc = 200.0 * n2 / d2
        line = b * osc + keep_b * line
    return {"value": osc, "line": line, "diff": osc - line, "rel": n2, "range": d2, "seed_rel": rel[0],
            "seed_range": rng[0], "zero": zero}


def ref_decision(cur, prev, params):
    if cur["zero"] or prev["zero"]:
        return None
    lo, hi = params["oversold"], params["overbought"]
    if prev["value"] <= lo < cur["value"] and prev["diff"] <= 0.0 < cur["diff"]:
        return 1
    if prev["value"] >= hi > cur["value"] and prev["diff"] >= 0.0 > cur["diff"]:
        return -1
    return 0


REFERENCE_PARAMS = [DEFAULTS, dict(DEFAULTS, range_period=14, smoothing_period=4, signal_period=2,
                                   recursion_length=80, oversold=-30.0, overbought=50.0)] + DOMAIN_ENDPOINTS


def _first(out, side=None, reason=None):
    if reason is not None:
        rows = np.flatnonzero(out["smi_reason"].to_numpy() == reason)
    else:
        rows = np.flatnonzero(out["signal"].to_numpy() == side)
    assert len(rows)
    return int(rows[0])


def _holds_at_end(df, params, reason):
    before = df.copy()
    attrs = dict(df.attrs)
    out = _apply(df, params)
    pd.testing.assert_frame_equal(df, before)
    assert df.attrs == attrs
    pd.testing.assert_index_equal(out.index, before.index)
    for col in df.columns:
        pd.testing.assert_series_equal(out[col], before[col])
    assert int(out["signal"].iloc[-1]) == 0
    assert out["smi_valid"].iloc[-1] == 0.0
    assert out["smi_reason"].iloc[-1] == reason, out["smi_reason"].iloc[-1]
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


def _received_out_of_order_rows_hold_only_inside_their_span():
    df = _walk(500, 5)
    clean = _apply(df, DEFAULTS)
    marked = df.copy()
    bad = 250
    marked.attrs[FRAME_RECEIVED_ORDER_DEFECTS_ATTR] = [int(df.index[bad].value // 1_000_000)]
    out = _apply(marked, DEFAULTS)
    assert (out["smi_reason"].iloc[bad:bad + NEED] == "timestamp_order").all()
    assert (out["signal"].iloc[bad:bad + NEED] == 0).all()
    pd.testing.assert_frame_equal(out.iloc[:bad], clean.iloc[:bad])
    pd.testing.assert_frame_equal(out.iloc[bad + NEED:], clean.iloc[bad + NEED:])


def _a_defect_holds_only_inside_its_span_and_recovery_matches_a_clean_frame():
    df = _walk(500, 5)
    bad = 250
    corrupt = df.copy()
    corrupt.iloc[bad, corrupt.columns.get_loc("high")] = np.nan
    out = _apply(corrupt, DEFAULTS)
    clean = _apply(df, DEFAULTS)
    assert (out["smi_reason"].iloc[bad:bad + NEED] == "nonfinite_input").all()
    assert (out["smi_valid"].iloc[bad:bad + NEED] == 0.0).all()
    pd.testing.assert_frame_equal(out.iloc[:bad], clean.iloc[:bad])
    pd.testing.assert_frame_equal(out.iloc[bad + NEED:], clean.iloc[bad + NEED:])
    gapped = df.drop(df.index[bad])
    out = _apply(gapped, DEFAULTS)
    assert (out["smi_reason"].iloc[bad:bad + NEED - 1] == "cadence_gap").all()
    assert out["smi_reason"].iloc[bad + NEED - 1] != "cadence_gap"


BOUNDARY_PARAMS = [
    dict(DEFAULTS, recursion_length=24),
    dict(DEFAULTS, recursion_length=188, range_period=10),
    dict(DEFAULTS, range_period=190, smoothing_period=1, signal_period=1, recursion_length=8),
    dict(DEFAULTS, range_period=10.0, smoothing_period=3.0, signal_period=3.0, recursion_length=60.0),
]


def _frozen(key, bars):
    manifest = om.load_manifest(MANIFEST)
    candles = om.load_candles(om.dataset_by_key(manifest, key))
    frame = candles[INPUT_COLUMNS].iloc[-bars:].copy()
    return declare_frame_cadence(frame, "4h", TIMESTAMP_KIND_OPEN)


DECISION_COLUMNS = ["smi_value", "smi_prev_value", "smi_signal_line", "smi_prev_signal_line", "smi_diff",
                    "smi_prev_diff", "smi_valid", "smi_reason_code", "signal"]


def test_a_200_row_input_with_a_forming_row_decides_on_199_closed_rows_at_the_longest_span():
    params = dict(DEFAULTS, range_period=6, smoothing_period=24, signal_period=24, recursion_length=192)
    df = _frozen("BTC 4h", 400)
    full = _apply(df, params)
    closed = df.iloc[-201:-1]
    assert len(closed.iloc[1:]) == 199
    got = _apply(closed.iloc[1:], params)
    assert got["smi_reason"].iloc[-1] != "insufficient_history"
    pd.testing.assert_series_equal(got[DECISION_COLUMNS].iloc[-1], full[DECISION_COLUMNS].iloc[-2],
                                   check_names=False)
    assert _apply(closed.iloc[2:], params)["smi_reason"].iloc[-1] == "insufficient_history"


def _runtime(df, kind, cutoff=None, boundary=None):
    out = df.copy()
    declare_frame_cadence(out, "4h", kind)
    out.attrs[FRAME_DECISION_TIME_ATTR] = {} if cutoff is None else {"cutoff_ms": cutoff, "boundary_ms": boundary}
    return out


def _entry_frame(side):
    base = _walk(900, 11)
    i = _first(_apply(base, DEFAULTS), 1 if side == "long" else -1)
    return base.iloc[:i + 1].copy()


def _bt(params, **kw):
    base = dict(initial_capital=1000.0, platform="hyperliquid", open_strategy={"name": NAME, "params": params},
                close_strategies=CLOSE, stop_loss_atr_mult=1.0, direction="both")
    base.update(kw)
    return Backtester(**base)


def test_simulator_fills_next_open_with_explicit_exits_no_reversal_or_scale_in_and_long_only_opens_no_shorts():
    df = _walk(1500, 5)
    sig = ensure_atr_indicator(_apply(df, DEFAULTS))
    with _case("test_simulator_fills_next_open_with_explicit_exits_and_no_reversal_or_scale_in"):
        _simulator_fills_next_open_with_explicit_exits_and_no_reversal_or_scale_in(df, sig)
    with _case("test_simulator_long_only_arm_never_opens_shorts"):
        _simulator_long_only_arm_never_opens_shorts(sig)


def _simulator_fills_next_open_with_explicit_exits_and_no_reversal_or_scale_in(df, sig):
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
        assert len(legs) == 1
        spans.append((entry, max(pd.Timestamp(leg["exit_date"]) for leg in legs)))
    spans.sort()
    for (_, prev_exit), (next_entry, _) in zip(spans, spans[1:]):
        assert next_entry >= prev_exit


def _simulator_long_only_arm_never_opens_shorts(sig):
    res = _bt(DEFAULTS, direction="long").run(sig, strategy_name=NAME, symbol="BTC/USDT", timeframe="4h",
                                              save=False)
    assert res["trades"] and {t["side"] for t in res["trades"]} == {"long"}


def _cfg(params, **kw):
    base = dict(strategy_name=NAME, params=params, registry="futures", platform="hyperliquid",
                symbol="BTC", timeframe="4h")
    base.update(kw)
    return ParityConfig(**base)
