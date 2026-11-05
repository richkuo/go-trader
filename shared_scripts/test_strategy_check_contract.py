import dataclasses
import functools
import importlib.util
import json
import os
import random
import subprocess
import sys
from typing import Any, Callable, Dict, Optional, Tuple

import numpy as np
import pandas as pd
import pytest

REPO = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
CHECK_HL = os.path.join(REPO, "shared_scripts", "check_hyperliquid.py")
CREDENTIAL_PREFIXES = ("HYPERLIQUID", "OKX", "ROBINHOOD", "TOPSTEP", "PROJECTX")
REPLACED_PER_STRATEGY_TESTS = (
    "test_an_already_closed_final_bar_is_the_decision_bar",
    "test_an_entry_data_hold_keeps_close_management_for_an_open_position",
    "test_batch_paper_slot_matches_solo_and_an_invalid_slot_fails_alone",
    "test_batch_refuses_a_malformed_acknowledgement",
    "test_batch_refuses_a_non_boolean_acknowledgement",
    "test_candidate_without_mode_is_refused",
    "test_check_decision_matches_the_evaluator_and_a_forming_bar_cannot_create_a_closed_bar_entry",
    "test_check_decision_matches_the_evaluator_at_full_precision_and_a_forming_bar_cannot_create_a_closed_bar_entry",
    "test_check_decision_matches_the_evaluator_at_full_precision_on_the_200_row_frame",
    "test_close_fallback_on_the_candidate_without_mode_is_refused",
    "test_close_fallback_paper_evaluates",
    "test_close_fallback_parameters_are_preflighted_before_any_market_fetch",
    "test_close_fallback_reference_is_gated_separately",
    "test_closed_bar_batch_matches_individual_checks_and_isolates_a_failing_slot",
    "test_cutoff_at_the_bar_boundary_selects_the_decision_bar",
    "test_elder_ray_pullback_without_mode_is_refused",
    "test_explicit_paper_evaluation_runs_both_directions_on_sealed_input_without_orders",
    "test_fisher_reversal_without_mode_is_refused",
    "test_forming_bar_cannot_create_a_closed_bar_entry",
    "test_forming_candle_changes_never_change_the_selected_closed_decision",
    "test_forming_candle_that_would_create_an_entry_is_excluded",
    "test_invalid_entry_data_holds_the_entry_and_keeps_close_management",
    "test_invalid_parameters_are_preflight_errors",
    "test_invalid_parameters_are_refused_before_any_candle_fetch",
    "test_invalid_parameters_are_refused_before_any_market_fetch",
    "test_klinger_without_mode_is_refused",
    "test_missing_mode_is_refused_for_a_close_fallback_reference",
    "test_missing_mode_is_refused_for_the_open_reference",
    "test_paper_and_acknowledged_evaluation_run_on_sealed_input_without_orders",
    "test_retained_comparators_still_evaluate",
    "test_retained_strategies_still_import_and_evaluate",
    "test_schaff_without_mode_is_refused",
    "test_stochastic_momentum_index_without_mode_is_refused",
)
H = 3_600_000
FOUR_H = 4 * H
DAY = 86_400_000
T0 = 1_699_999_200_000
DAY_T0 = 1_772_323_200_000
CCE_FORMING = 1_772_460_000_000
NATIVE_CLOSE = "native_close_inclusive"
LADDER = [{"profit_pct": 0.01, "close_fraction": 0.25}, {"profit_pct": 0.02, "close_fraction": 0.5},
          {"profit_pct": 0.03, "close_fraction": 0.75}, {"profit_pct": 0.04, "close_fraction": 1.0}]
TP_LADDER = {"name": "tiered_tp_pct", "params": {"tp_tiers": LADDER}}
TP_EMPTY = {"name": "tiered_tp_pct", "params": {}}
TP_3PCT = {"name": "tiered_tp_pct", "params": {"tp_tiers": [{"profit_pct": 0.03, "close_fraction": 1.0}]}}
TIME_STOP = {"name": "time_stop", "params": {"max_bars": 1}}
ACKED = ["--mode=live", "--allow-no-edge"]
EVAL_EXTRAS = (["--mode=paper"], ACKED)
DEFAULT = object()


def held(side, cost):
    return ["--position-side", side, f"--position-avg-cost={cost}", "--position-qty=1", "--position-initial-qty=1"]


HELD_LONG = held("long", 50)
HELD_SHORT = held("short", 5000)


def env():
    out = {k: v for k, v in os.environ.items() if not k.startswith(CREDENTIAL_PREFIXES)}
    out["GO_TRADER_HL_OHLCV_CACHE"] = "0"
    return out


def run(argv, stdin=None):
    return subprocess.run(
        [sys.executable, CHECK_HL] + argv,
        input="" if stdin is None else json.dumps(stdin),
        capture_output=True, text=True, cwd=REPO, env=env(), timeout=240,
    )


def payload(proc):
    return json.loads(proc.stdout.strip().splitlines()[-1])


def strip(out):
    return {k: v for k, v in out.items() if k not in ("timestamp", "id")}


def rows_of(bars):
    return [[b["T"], b["o"], b["h"], b["l"], b["c"], b["v"]] for b in bars]


def frame(bars, step=H, *, timing=True, rows=None):
    rows = rows_of(bars) if rows is None else rows
    out = {"rows": rows, "required": len(rows), "bars": len(rows), "coverage_short": False,
           "first_open_ms": bars[0]["t"], "last_open_ms": bars[-1]["t"], "last_close_ms": bars[-1]["T"],
           "last_recv_at_ms": bars[-1]["t"], "source": "ws", "ready": True, "forming_bar_included": True}
    if timing:
        out["timing"] = {"rule": "hyperliquid_native_close", "interval_ms": step,
                         "bars": [[b["t"], b["T"], True] for b in bars]}
    return out


def market(frames, *, cutoff_ms, mid, snapshot_id="test/1"):
    return {"version": 4, "snapshot_id": snapshot_id, "generation": 1, "sealed_at_ms": cutoff_ms,
            "decision_cutoff_ms": cutoff_ms, "frames": frames, "feed_complete": True,
            "mids": {"BTC": {"px": mid, "recv_at_ms": cutoff_ms, "source": "ws", "age_ms": 0,
                             "stale": False, "confirmed": True}}}


def envelope(bars, step, key, *, cutoff=None, mid=None, snapshot_id="test/1", **frame_kw):
    cutoff = bars[-1]["t"] + step // 2 if cutoff is None else cutoff
    mid = bars[-1]["c"] if mid is None else mid
    return {"v": 2, "market": market({key: frame(bars, step, **frame_kw)}, cutoff_ms=cutoff, mid=mid,
                                     snapshot_id=snapshot_id)}


@functools.lru_cache(maxsize=None)
def strategies():
    tools = os.path.join(REPO, "shared_tools")
    if tools not in sys.path:
        sys.path.insert(0, tools)
    path = os.path.join(REPO, "shared_strategies", "open", "futures", "strategies.py")
    spec = importlib.util.spec_from_file_location("_strategy_check_contract_futures_strategies", path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def frame_df(rows, timeframe=None, kind=None):
    strategies()
    df = pd.DataFrame(rows, columns=["timestamp", "open", "high", "low", "close", "volume"])
    df["datetime"] = pd.to_datetime(df["timestamp"], unit="ms", utc=True)
    df = df.set_index("datetime").sort_index()
    if timeframe is None:
        return df
    from closed_bar import declare_frame_cadence
    return declare_frame_cadence(df, timeframe, kind)


def flat_bars(n, step, *, forming_jump=None, start=400.0):
    out = []
    for i in range(n):
        mid = start
        if forming_jump is not None and i == n - 1:
            mid = start + forming_jump
        out.append({"t": T0 + i * step, "T": T0 + (i + 1) * step - 1,
                    "o": start, "h": max(start, mid) + 0.5, "l": min(start, mid) - 0.5, "c": mid, "v": 1000.0 + i})
    return out


TSI_LONG_SETUP = [399.0, 398.5]
TSI_SHORT_SETUP = [401.0, 401.5]


def tsi_bars(n, step, tail, start=400.0):
    closes = [start] * (n - len(tail)) + list(tail)
    out = []
    prev = start
    for i, c in enumerate(closes):
        out.append({"t": T0 + i * step, "T": T0 + (i + 1) * step - 1, "o": prev, "h": max(prev, c) + 0.5,
                    "l": min(prev, c) - 0.5, "c": c, "v": 1000.0 + i})
        prev = c
    return out


def tsi_long(n=260, step=H, jump=8.0):
    return tsi_bars(n, step, TSI_LONG_SETUP + [TSI_LONG_SETUP[-1] + jump])


def tsi_short(n=260, step=H, jump=8.0):
    return tsi_bars(n, step, TSI_SHORT_SETUP + [TSI_SHORT_SETUP[-1] - jump])


def fisher_bars(n, step, *, forming_jump=None, start=400.0):
    out = []
    for i in range(n):
        if i < n - 160:
            mid = start + (3.0 if i % 2 == 0 else -2.5)
        else:
            mid = start - 0.5 * (i - (n - 160))
        if forming_jump is not None and i == n - 1:
            mid = out[-1]["c"] + forming_jump
        out.append({"t": T0 + i * step, "T": T0 + (i + 1) * step - 1,
                    "o": mid, "h": mid + 0.5, "l": mid - 0.5, "c": mid, "v": 1000.0 + i})
    return out


def erp_bars(n, step, *, side=1, start=400.0):
    out = []
    for i in range(n):
        close = start + 0.5 * i if side == 1 else start + 200.0 - 0.5 * i
        pulled = n - 4 <= i <= n - 2
        if side == 1:
            low, high = close - (10.0 if pulled else 1.0), close + 1.0
        else:
            low, high = close - 1.0, close + (10.0 if pulled else 1.0)
        out.append({"t": T0 + i * step, "T": T0 + (i + 1) * step - 1,
                    "o": close, "h": high, "l": low, "c": close, "v": 1000.0 + i})
    return out


def walk_bars(n, step, seed=3, start=400.0):
    rng = np.random.default_rng(seed)
    close = start * np.exp(np.cumsum(rng.normal(0.0, 0.01, n)))
    out = []
    prev = start
    for i in range(n):
        c = float(close[i])
        o = float(prev)
        out.append({"t": T0 + i * step, "T": T0 + (i + 1) * step - 1, "o": o, "h": max(o, c) * 1.002,
                    "l": min(o, c) * 0.998, "c": c, "v": 1000.0 + i})
        prev = c
    return out


def klinger_walk_bars(n, step, seed=3, start=400.0):
    rng = np.random.default_rng(seed)
    close = start * np.exp(np.cumsum(rng.normal(0.0, 0.01, n)))
    wick = rng.uniform(0.0005, 0.006, (n, 2))
    volume = rng.uniform(500.0, 5000.0, n)
    out = []
    prev = start
    for i in range(n):
        c = float(close[i])
        o = float(prev)
        out.append({"t": T0 + i * step, "T": T0 + (i + 1) * step - 1, "o": o, "h": max(o, c) * (1.0 + wick[i, 0]),
                    "l": min(o, c) * (1.0 - wick[i, 1]), "c": c, "v": float(volume[i])})
        prev = c
    return out


def cce_bars(n=800, effect=0.02):
    rng = random.Random(1653)
    out = []
    price = 100.0
    for k in range(n):
        t = CCE_FORMING - (n - 1 - k) * H
        rel = t - 345_600_000
        cell = ((rel // 86_400_000) % 7) * 24 + (rel % 86_400_000) // H
        move = rng.gauss(0.0, 0.001) + (effect if cell == 14 else 0.0)
        close = price * (1.0 + move)
        out.append({"t": t, "T": t + H - 1, "o": price, "h": max(price, close) * 1.0002,
                    "l": min(price, close) * 0.9998, "c": close, "v": 1000.0})
        price = close
    return out


def cpr_bar(t, o, c, h=None, lo=None):
    return {"t": t, "T": t + H - 1, "o": o, "h": max(o, c) + 0.1 if h is None else h,
            "l": min(o, c) - 0.1 if lo is None else lo, "c": c, "v": 1000.0}


def cpr_bars(current, days_before=8):
    out = []
    t = DAY_T0
    for _ in range(days_before):
        for k in range(24):
            if k == 0:
                out.append(cpr_bar(t, 100.0, 100.0, h=110.0, lo=99.9))
            elif k == 23:
                out.append(cpr_bar(t, 100.0, 100.0, h=100.1, lo=90.0))
            else:
                out.append(cpr_bar(t, 100.0, 100.0))
            t += H
    prev = 100.0
    for c in current:
        out.append(cpr_bar(t, prev, c))
        prev = c
        t += H
    return out


EFP_CROSS_AT = 211


def efp_closes(sign=1, n=260):
    step = np.full(n, 0.5)
    step[200:205] = -2.0
    step[0] = 0.0
    return 100.0 + np.cumsum(step) if sign > 0 else 400.0 - np.cumsum(step)


def efp_bars(last_closed, sign=1):
    close = efp_closes(sign)
    end = last_closed + 2
    out = []
    for i in range(end - 200, end):
        t = DAY_T0 + i * H
        o = float(close[i - 1])
        c = float(close[i])
        out.append({"t": t, "T": t + H - 1, "o": o, "h": max(o, c) + 0.5, "l": min(o, c) - 0.5, "c": c, "v": 100.0})
    return out


MIR_UP_AT, MIR_DOWN_AT = 205, 217


def mir_series(slope=0.05, n=260):
    rng = np.ones(n)
    rng[200:208] = 3.0
    mid = 100.0 + slope * np.arange(n)
    mid[MIR_DOWN_AT:] = mid[MIR_DOWN_AT:] - 5.0 * np.sign(slope)
    return rng, mid


def mir_bars(last_closed, slope=0.05, forming=True):
    rng, mid = mir_series(slope)
    end = last_closed + (2 if forming else 1)
    out = []
    for i in range(end - 200, end):
        t = DAY_T0 + i * H
        out.append({"t": t, "T": t + H - 1, "o": float(mid[i]), "h": float(mid[i] + rng[i] / 2),
                    "l": float(mid[i] - rng[i] / 2), "c": float(mid[i]), "v": 100.0})
    return out


def stc_reconstruct(ind):
    lower, upper = ind["stc_lower_threshold"], ind["stc_upper_threshold"]
    if ind.get("stc_valid") != 1.0:
        return 0
    if ind["stc_prev_value"] <= lower < ind["stc_value"] and ind["stc_macd"] > ind["stc_prev_macd"]:
        return 1
    if ind["stc_prev_value"] >= upper > ind["stc_value"] and ind["stc_macd"] < ind["stc_prev_macd"]:
        return -1
    return 0


def _klinger_side_entry(ind, side):
    key = f"klinger_{side}_"
    if ind.get(key + "armed") != 1.0 or ind.get(key + "state") == 4.0:
        return False
    lag = (ind["klinger_eval_ts_ms"] - ind[key + "confirm_ts_ms"]) / ind["klinger_interval_ms"]
    if not 1 <= lag <= ind["klinger_setup_expiry"]:
        return False
    if side == "long":
        diverges = ind[key + "pivot2_price"] < ind[key + "pivot1_price"] and \
            ind[key + "pivot2_oscillator"] > ind[key + "pivot1_oscillator"]
        crosses = ind["klinger_prev_oscillator"] <= ind["klinger_prev_signal_line"] and \
            ind["klinger_oscillator"] > ind["klinger_signal_line"]
    else:
        diverges = ind[key + "pivot2_price"] > ind[key + "pivot1_price"] and \
            ind[key + "pivot2_oscillator"] < ind[key + "pivot1_oscillator"]
        crosses = ind["klinger_prev_oscillator"] >= ind["klinger_prev_signal_line"] and \
            ind["klinger_oscillator"] < ind["klinger_signal_line"]
    return diverges and crosses


def klinger_reconstruct(ind):
    if ind.get("klinger_valid") != 1.0:
        return 0
    long_, short = _klinger_side_entry(ind, "long"), _klinger_side_entry(ind, "short")
    if long_ and not short:
        return 1
    if short and not long_:
        return -1
    return 0


def smi_reconstruct(ind):
    lower, upper = ind["smi_oversold"], ind["smi_overbought"]
    if ind.get("smi_valid") != 1.0:
        return 0
    value, prev = ind["smi_value"], ind["smi_prev_value"]
    diff, prev_diff = ind["smi_diff"], ind["smi_prev_diff"]
    assert diff == value - ind["smi_signal_line"] and prev_diff == prev - ind["smi_prev_signal_line"]
    if prev <= lower < value and prev_diff <= 0.0 < diff:
        return 1
    if prev >= upper > value and prev_diff >= 0.0 > diff:
        return -1
    return 0


def trix_identity(ind, bars):
    assert ind["trix_diff"] == ind["trix_value"] - ind["trix_signal_line"] > 0
    assert ind["trix_prev_diff"] <= 0 and ind["trix_value"] > 0
    assert ind["trix_value"] == 100.0 * (ind["trix_ema3"] - ind["trix_ema3_base"]) / ind["trix_ema3_base"]


def tsi_identity(ind, bars):
    assert ind["tsi_diff"] == ind["tsi_value"] - ind["tsi_signal_line"] > 0
    assert ind["tsi_prev_diff"] == 0.0 and ind["tsi_value"] > 0
    assert ind["tsi_value"] == 100.0 * (ind["tsi_double_change"] / ind["tsi_double_abs"])
    assert ind["tsi_value"] != round(ind["tsi_value"], 6)


def ama_identity(ind, bars):
    assert "ama_eval_ts_ms" not in ind
    assert ind["ama_close"] - ind["ama_value"] > 0
    assert ind["ama_prev_close"] <= ind["ama_prev_value"]
    assert ind["ama_slope"] > 0


def erp_identity(ind, bars):
    assert ind["erp_eval_ts_ms"] == float(bars[-1]["T"])


@dataclasses.dataclass(frozen=True)
class Case:
    name: str
    ref: str
    prefix: str
    params: Dict[str, Any]
    tf: str
    solo: Callable
    tp: Dict[str, Any]
    entry: Optional[Callable] = None
    helds: Tuple[Tuple[str, ...], ...] = ()
    default_closes: Optional[Tuple[Dict[str, Any], ...]] = None
    limit: int = 200
    snapshot_id: str = "test/1"
    boundary_cutoff: bool = False
    fallback_evaluates: bool = True
    fallback_with_params: bool = True
    eval_spec: Optional[Dict[str, Any]] = None
    solo_short: Optional[Callable] = None
    peers: Tuple[str, ...] = ()
    batch_peer: Optional[str] = None
    batch_bad: Optional[Tuple[Dict[str, Any], Optional[str]]] = None
    solo_signal: Optional[int] = None
    batch_signals: Optional[Tuple[int, int]] = None
    reconstruct: Optional[Callable] = None
    evaluator: Optional[Dict[str, Any]] = None
    opposite_entry_closes: bool = False
    entry_params: Optional[Dict[str, Any]] = None
    eval_tp: Optional[Dict[str, Any]] = None
    batch_entry: Optional[Callable] = None
    batch_check: Optional[Callable] = None

    def reason(self):
        return f"{self.prefix}_reason"

    def closed_params(self):
        return self.params if self.entry_params is None else self.entry_params

    def admission_tp(self):
        return self.tp if self.eval_tp is None else self.eval_tp


def _solo4h(bars):
    return envelope(bars, FOUR_H, "BTC|4h")


def _hourly(bars):
    return envelope(bars, H, "BTC|1h", snapshot_id="3600s/1")


@functools.lru_cache(maxsize=None)
def _walk_entry_index(name, side):
    case = CASES[name]
    bars = case.walk(1200, H)
    out = strategies().apply_strategy(name, frame_df(rows_of(bars), "1h", NATIVE_CLOSE), case.closed_params())
    sig = out["signal"].to_numpy()
    return next(int(i) for i in np.flatnonzero(sig == side) if i >= 260 and sig[i - 1] == 0)


def walk_entry(name, walk):
    def entry(side=1):
        return walk(1200, H)[:_walk_entry_index(name, side) + 1]
    return entry


@dataclasses.dataclass(frozen=True)
class WalkCase(Case):
    walk: Callable = walk_bars


TRIX_PARAMS = {"period": 3, "signal_period": 4, "recursion_length": 20}
TSI_PARAMS = {"slow_period": 3, "fast_period": 2, "signal_period": 2, "recursion_length": 18}
AMA_PARAMS = {"efficiency_period": 4, "fast_period": 2, "slow_period": 12, "slope_span": 2, "recursion_length": 20}
ERP_PARAMS = {"ema_period": 13, "slope_span": 3, "pullback_window": 8}
FISHER_PARAMS = {"lookback": 9, "extreme": 0.5}
STC_PARAMS = {"fast_period": 23, "slow_period": 50, "cycle_length": 10, "smoothing": 0.5,
              "lower_threshold": 25.0, "upper_threshold": 75.0}
KLINGER_PARAMS = {"fast_period": 34, "slow_period": 55, "signal_period": 13, "replay_bars": 112, "left_span": 3,
                  "right_span": 3, "min_separation": 5, "max_separation": 40, "setup_expiry": 12,
                  "divergence_test": True}
SMI_PARAMS = {"range_period": 10, "smoothing_period": 3, "signal_period": 3, "recursion_length": 60,
              "oversold": -40.0, "overbought": 40.0}
CCE_SMALL = {"training_bars": 672, "holding_bars": 1, "refit_bars": 24, "min_effective_obs": 2, "confidence": 0.5,
             "cost_hurdle": 0.0, "expected_interval_ms": H}

TRIX_COLUMNS = ("trix_value", "trix_prev_value", "trix_signal_line", "trix_prev_signal_line", "trix_diff",
                "trix_prev_diff", "trix_ema3", "trix_ema3_base", "trix_prev_ema3", "trix_prev_ema3_base",
                "trix_close", "trix_prev_close", "trix_seed_close", "trix_prev_seed_close", "trix_valid",
                "trix_value_valid", "trix_reason_code", "trix_required_history")
TSI_COLUMNS = ("tsi_value", "tsi_prev_value", "tsi_signal_line", "tsi_prev_signal_line", "tsi_diff", "tsi_prev_diff",
               "tsi_slow_change", "tsi_double_change", "tsi_slow_abs", "tsi_double_abs", "tsi_prev_double_abs",
               "tsi_first_change", "tsi_signal_seed_change", "tsi_close", "tsi_prev_close", "tsi_seed_close",
               "tsi_prev_seed_close", "tsi_valid", "tsi_value_valid", "tsi_reason_code", "tsi_required_history")
AMA_COLUMNS = ("ama_value", "ama_prev_value", "ama_slope_ref", "ama_slope", "ama_efficiency", "ama_smoothing",
               "ama_close", "ama_prev_close", "ama_seed_close", "ama_valid", "ama_reason_code")
ERP_COLUMNS = ("erp_ema", "erp_slope", "erp_bull_power", "erp_bear_power", "erp_setup_side", "erp_setup_age",
               "erp_valid", "erp_reason_code", "erp_eval_ts_ms", "erp_close")
FISHER_COLUMNS = ("fisher_value", "fisher_trigger", "fisher_prev_trigger", "fisher_valid", "fisher_reason_code")


CASE_LIST = [
    Case(
        name="adaptive_moving_average", prefix="ama", params=AMA_PARAMS, tf="4h",
        ref="backtest/candidates/adaptive_moving_average_1641/REPORT.md",
        solo=lambda: _solo4h(flat_bars(220, FOUR_H, forming_jump=8.0)),
        entry=lambda side=1: flat_bars(260, H, forming_jump=8.0), tp=TP_EMPTY,
        eval_spec={"closed": False, "indicators": {"ama_recursion_length": 20.0, "ama_valid": 1.0},
                   "labels": {"ama_reason": "entry_long"}, "signal": 1, "open_action": "long"},
        batch_bad=({**AMA_PARAMS, "fast_period": 12}, None), solo_signal=1, batch_signals=(0, 1),
        evaluator={"sides": (1,), "bars": lambda side: flat_bars(260, H, forming_jump=8.0), "columns": AMA_COLUMNS,
                   "full": False, "round": False, "timeframe": "1h", "label": "entry_long", "label_match": True,
                   "identity": ama_identity},
    ),
    Case(
        name="calendar_conditioned_entry", prefix="cce", params=CCE_SMALL, tf="1h",
        ref="backtest/candidates/calendar_conditioned_entry_1653/REPORT.md",
        solo=lambda: _hourly(cce_bars()), tp=TP_3PCT, default_closes=(TIME_STOP,), limit=800, snapshot_id="3600s/1",
    ),
    Case(
        name="camarilla_pivot_reversal", prefix="cpr", params={"expected_interval_ms": H}, tf="1h",
        ref="backtest/candidates/camarilla_pivot_reversal_1669/REPORT.md",
        solo=lambda: _hourly(cpr_bars([100.0, 94.0, 95.0, 96.0])), tp=TP_3PCT, default_closes=(TP_3PCT,),
        snapshot_id="3600s/1",
    ),
    Case(
        name="elder_force_pullback", prefix="efp", params={}, tf="1h",
        ref="backtest/candidates/elder_force_pullback_1668/REPORT.md",
        solo=lambda: _hourly(efp_bars(EFP_CROSS_AT)), tp=TP_LADDER, default_closes=(TP_LADDER,),
        snapshot_id="3600s/1",
    ),
    Case(
        name="elder_ray_pullback", prefix="erp", params=ERP_PARAMS, tf="4h",
        ref="backtest/candidates/elder_ray_pullback_1648/REPORT.md",
        solo=lambda: _solo4h(erp_bars(220, FOUR_H)), entry=lambda side=1: erp_bars(260, H), tp=TP_EMPTY,
        eval_spec={"closed": False, "indicators": {"erp_ema_period": 13.0, "erp_valid": 1.0},
                   "labels": {"erp_reason": "entry_long"}, "signal": 1, "open_action": "long"},
        batch_bad=(dict(ERP_PARAMS, ema_period=40), None), solo_signal=1, batch_signals=(0, 1),
        evaluator={"sides": (1, -1), "bars": lambda side: erp_bars(260, H, side=side), "columns": ERP_COLUMNS,
                   "full": False, "round": False, "timeframe": None, "label": None, "label_match": True,
                   "identity": erp_identity},
    ),
    Case(
        name="fisher_reversal", prefix="fisher", params=FISHER_PARAMS, tf="4h",
        ref="backtest/candidates/fisher_reversal_1646/REPORT.md",
        solo=lambda: _solo4h(fisher_bars(220, FOUR_H)), entry=lambda side=1: fisher_bars(260, H, forming_jump=30.0),
        tp=TP_EMPTY,
        eval_spec={"closed": False, "indicators": {"fisher_lookback": 9.0},
                   "indicators_in": {"fisher_valid": (0.0, 1.0)}},
        batch_signals=(0, 1),
        evaluator={"sides": (1,), "bars": lambda side: fisher_bars(260, H, forming_jump=30.0),
                   "columns": FISHER_COLUMNS, "full": False, "round": True, "timeframe": None, "label": None,
                   "label_match": False, "identity": None},
    ),
    WalkCase(
        name="klinger_divergence", prefix="klinger", params=KLINGER_PARAMS, tf="4h", walk=klinger_walk_bars,
        ref="backtest/candidates/klinger_divergence_1662/REPORT.md",
        solo=lambda: _solo4h(klinger_walk_bars(240, FOUR_H)),
        entry=walk_entry("klinger_divergence", klinger_walk_bars), tp=TP_EMPTY,
        helds=(HELD_LONG, HELD_SHORT), boundary_cutoff=True,
        eval_spec={"closed": True, "indicators": {"klinger_fast_period": 34.0, "klinger_support_bars": 172.0},
                   "label_present": True},
        peers=("breakout", "on_balance_volume_divergence", "schaff_trend_cycle", "volume_weighted"),
        batch_peer="volume_weighted", batch_bad=(dict(KLINGER_PARAMS, fast_period=55), "fast_period"),
        batch_signals=(1, 0), reconstruct=klinger_reconstruct, opposite_entry_closes=True,
    ),
    Case(
        name="mass_index_reversal", prefix="mir", params={}, tf="1h",
        ref="backtest/candidates/mass_index_reversal_1670/REPORT.md",
        solo=lambda: _hourly(mir_bars(MIR_DOWN_AT)), tp=TP_3PCT, default_closes=(TP_3PCT,),
        snapshot_id="3600s/1",
    ),
    WalkCase(
        name="schaff_trend_cycle", prefix="stc", params=STC_PARAMS, tf="4h",
        ref="backtest/candidates/schaff_trend_cycle_1651/REPORT.md",
        solo=lambda: _solo4h(walk_bars(240, FOUR_H)), entry=walk_entry("schaff_trend_cycle", walk_bars), tp=TP_EMPTY,
        helds=(HELD_LONG, HELD_SHORT), boundary_cutoff=True,
        eval_spec={"closed": True, "indicators": {"stc_fast_period": 23.0, "stc_required_history": 193.0},
                   "label_present": True},
        peers=("breakout", "fisher_reversal", "macd", "stoch_rsi"), batch_peer="macd",
        batch_bad=(dict(STC_PARAMS, fast_period=50), "fast_period"), batch_signals=(1, 0),
        reconstruct=stc_reconstruct, opposite_entry_closes=True,
    ),
    WalkCase(
        name="stochastic_momentum_index", prefix="smi", params=SMI_PARAMS, tf="4h",
        ref="backtest/candidates/stochastic_momentum_index_1663/REPORT.md",
        solo=lambda: _solo4h(walk_bars(240, FOUR_H)), entry=walk_entry("stochastic_momentum_index", walk_bars),
        tp=TP_3PCT, helds=(HELD_LONG, HELD_SHORT), boundary_cutoff=True,
        eval_spec={"closed": True, "indicators": {"smi_range_period": 10.0, "smi_required_history": 71.0},
                   "label_present": True},
        peers=("mean_reversion_pro", "schaff_trend_cycle", "stoch_rsi"), batch_peer="stoch_rsi",
        batch_bad=(dict(SMI_PARAMS, recursion_length=10), "recursion_length"), batch_signals=(1, 0),
        reconstruct=smi_reconstruct, opposite_entry_closes=True,
    ),
    Case(
        name="trix_momentum", prefix="trix", params=TRIX_PARAMS, tf="4h",
        ref="backtest/candidates/trix_momentum_1659/REPORT.md",
        solo=lambda: _solo4h(flat_bars(220, FOUR_H, forming_jump=8.0)),
        solo_short=lambda: _solo4h(flat_bars(220, FOUR_H, forming_jump=-8.0)),
        entry=lambda side=1: flat_bars(260, H, forming_jump=8.0), tp=TP_LADDER,
        eval_spec={"closed": False, "indicators": {"trix_recursion_length": 20.0, "trix_valid": 1.0},
                   "labels": {"trix_reason": "entry_long"}, "signal": 1, "open_action": "long",
                   "short_label": "entry_short"},
        batch_bad=({**TRIX_PARAMS, "recursion_length": 17}, None), solo_signal=1, batch_signals=(0, 1),
        evaluator={"sides": (1,), "bars": lambda side: flat_bars(260, H, forming_jump=8.0), "columns": TRIX_COLUMNS,
                   "full": True, "round": False, "timeframe": "1h", "label": "entry_long",
                   "label_match": True, "identity": trix_identity},
        fallback_with_params=False,
    ),
    Case(
        name="true_strength_index", prefix="tsi", params=TSI_PARAMS, tf="4h",
        ref="backtest/candidates/true_strength_index_1667/REPORT.md",
        solo=lambda: _solo4h(tsi_long(220, FOUR_H)), solo_short=lambda: _solo4h(tsi_short(220, FOUR_H)),
        entry=lambda side=1: tsi_long(), tp=TP_LADDER,
        eval_spec={"closed": False,
                   "indicators": {"tsi_recursion_length": 18.0, "tsi_required_history": 19.0, "tsi_valid": 1.0},
                   "labels": {"tsi_reason": "entry_long"}, "signal": 1, "open_action": "long",
                   "short_label": "entry_short"},
        batch_bad=({**TSI_PARAMS, "recursion_length": 17}, None), solo_signal=1, batch_signals=(0, 1),
        evaluator={"sides": (1,), "bars": lambda side: tsi_long(), "columns": TSI_COLUMNS, "full": True,
                   "round": False, "timeframe": "1h", "label": "entry_long", "label_match": True,
                   "identity": tsi_identity},
        fallback_with_params=False,
    ),
]


LEGACY_T0 = 1_700_000_000_000


def legacy_solo(step=FOUR_H, key="BTC|4h", snapshot_id="14400s/1"):
    rows = []
    price = 100.0
    for i in range(220):
        drift = 0.9 if (i // 25) % 2 == 0 else -0.8
        price = max(price + drift + (1.4 if i % 5 == 0 else -0.6), 5.0)
        rows.append([LEGACY_T0 + i * step, price - 0.4, price + 1.5, price - 1.5, price, 1000.0 + i])
    frames = {key: {"rows": rows, "required": 200, "bars": len(rows), "coverage_short": False,
                    "first_open_ms": rows[0][0], "last_open_ms": rows[-1][0], "last_close_ms": rows[-1][0],
                    "last_recv_at_ms": rows[-1][0], "source": "ws", "ready": True, "forming_bar_included": True}}
    mids = {"BTC": {"px": rows[-1][4], "recv_at_ms": rows[-1][0], "source": "ws", "age_ms": 0, "stale": False,
                    "confirmed": True}}
    return {"v": 2, "market": {"version": 4, "snapshot_id": snapshot_id, "generation": 1,
                               "sealed_at_ms": rows[-1][0], "frames": frames, "mids": mids, "feed_complete": True}}


def legacy_hourly_solo():
    return legacy_solo(H, "BTC|1h", "3600s/1")


def hour_bar(i, o, h, low, c, v):
    return {"t": T0 + i * H, "T": T0 + (i + 1) * H - 1, "o": o, "h": h, "l": low, "c": c, "v": v}


OBV_PARAMS = {"left_span": 2, "right_span": 2, "min_separation": 3, "max_separation": 20,
              "volume_threshold": 0.1, "setup_expiry": 5, "volume_test": True}
OBV_CLOSES = ([100.0] * 103 + [100.0, 97.0, 91.0, 97.0, 100.0, 100.0, 100.0]
              + [99.0, 98.0, 96.0, 98.0, 100.0, 101.0, 100.0, 98.0, 96.0, 95.0, 97.0, 98.0, 98.5, 100.0]
              + [100.5, 101.0, 101.5, 102.0])
OBV_ENTRY = 123
TP_2PCT = {"name": "tiered_tp_pct", "params": {"tp_tiers": [{"profit_pct": 0.02, "close_fraction": 1.0}]}}


def obv_bars(last):
    out = []
    prev = None
    for i, c in enumerate(OBV_CLOSES[:last + 1]):
        v = 5.0 if prev is None or c == prev else (10.0 if c > prev else 1.0)
        out.append(hour_bar(i, c, c + 1.0, c - 1.0, c, v))
        prev = c
    return out


def cmo_bars(closes):
    out = []
    prev = closes[0]
    for i, c in enumerate(closes):
        out.append(hour_bar(i, prev, max(prev, c) + 0.5, min(prev, c) - 0.5, c, 1000.0 + i))
        prev = c
    return out


def zigzag_bars(n):
    out = []
    price = 100.0
    for i in range(n):
        price = price + (0.4 if i % 2 == 0 else -0.35)
        out.append(hour_bar(i, price - 0.1, price + 0.5, price - 0.5, price, 1000.0 + i))
    return out


def set_bar(bar, price, volume):
    return dict(bar, o=price + 0.1, h=price + 0.5, l=price - 0.5, c=price, v=volume)


MFI_ADMISSION = {"lookback": 14, "oversold": 20.0, "overbought": 80.0}
MFI_PARAMS = {"lookback": 2, "oversold": 30.0, "overbought": 70.0}


def mfi_bars(side=1):
    bars = zigzag_bars(240)
    base = bars[-5]["c"]
    bars[-4] = set_bar(bars[-4], base - 2.0, 1000.0)
    bars[-3] = set_bar(bars[-3], base - 4.0, 1000.0)
    bars[-2] = set_bar(bars[-2], base - 6.0, 1000.0)
    bars[-1] = set_bar(bars[-1], base - 1.0, 50_000.0)
    return bars


EOM_PARAMS = {"eom_period": 3, "breakout_window": 4}
EOM_LABELS = ("no_breakout", "breakout_continuation", "entry_long", "entry_short", "indicator_disagrees",
              "zero_indicator")


def eom_bars(side=1):
    rows = [(100.0, 101.0, 99.0, 100.0, 1000.0)] * 60 + [(100.0, 104.0, 100.0, 103.5, 1000.0)]
    return [hour_bar(i, o, h, low, c, v) for i, (o, h, low, c, v) in enumerate(rows)]


AROON_PARAMS = {"period": 25, "strength": 70.0}


def aroon_bars(n, *, spike_at=None):
    out = []
    price = 200.0
    for i in range(n):
        price -= 0.5
        o, h, low, c = price + 0.2, price + 0.4, price - 0.4, price
        if spike_at is not None and i == spike_at:
            o, h, low, c = price + 0.3, price + 150.0, price + 0.2, price + 149.0
        out.append(hour_bar(i, o, h, low, c, 1000.0))
    return out


RCR_ADMISSION = {"fit_window": 50, "band_multiplier": 2.0, "slope_ceiling": 0.10, "min_residual_std": 0.0}
RCR_PARAMS = dict(RCR_ADMISSION, fit_window=8)
RCR_PATTERN = (0.5, -0.5, -0.5, 0.5)


def rcr_bars(n, *, closes=None):
    out = []
    for i in range(n):
        c = 100.0 + RCR_PATTERN[i % 4]
        if closes and i in closes:
            c = closes[i]
        out.append(hour_bar(i, c, c + 0.6, c - 0.6, c, 1000.0))
    return out


def rcr_batch_labels(clean, mixed):
    assert clean["off"]["indicator_labels"]["rcr_reason"] == "no_reentry"
    assert "fit_window" in mixed["bad-params"]["error"]


WR_TP = {"name": "tiered_tp_atr", "params": {"tp_tiers": [{"atr_multiple": 2.0, "close_fraction": 1.0}]}}


def wr_zigzag(n, start=100.0):
    out = []
    price = start
    for i in range(n):
        price += 0.4 if i % 2 == 0 else -0.35
        out.append((price - 0.1, price + 0.5, price - 0.5, price))
    return out


def wr_bars(tuples):
    return [hour_bar(i, o, h, low, c, 1000.0 + i) for i, (o, h, low, c) in enumerate(tuples)]


def wr_solo():
    bars = wr_bars(wr_zigzag(80))
    return {"v": 2, "market": {"version": 4, "snapshot_id": "test/1", "generation": 1, "sealed_at_ms": T0,
                               "frames": {"BTC|1h": frame(bars, H)}, "feed_complete": True}}


FOLDED_CASES = [
    Case(
        name="aroon_trend", prefix="aroon", params=AROON_PARAMS, tf="4h",
        ref="backtest/candidates/aroon_trend_1643/REPORT.md", solo=legacy_solo,
        entry=lambda side=1: aroon_bars(61, spike_at=59), tp=TP_3PCT, default_closes=(TP_3PCT,),
        eval_spec={"closed": False, "indicators": {"aroon_valid": 1.0},
                   "indicators_in": {"aroon_reason_code": (-1, 0, 1, 2)}},
        batch_peer="breakout", batch_bad=({"period": 2.5}, "period must be an integer"), batch_signals=(1, -1),
    ),
    Case(
        name="chande_momentum", prefix="cmo", params={}, tf="1h",
        ref="backtest/candidates/chande_momentum_1661/REPORT.md", solo=legacy_hourly_solo,
        entry=lambda side=1: cmo_bars([100.0] * 220 + [101.0, 96.0]), tp=TP_3PCT,
        eval_spec={"closed": False, "indicators": {"cmo_valid": 1.0},
                   "present": ("cmo", "cmo_prev", "cmo_gain_sum", "cmo_loss_sum", "cmo_close", "cmo_eval_ts_ms",
                               "cmo_valid", "cmo_reason_code")},
        batch_signals=(1, -1),
    ),
    Case(
        name="ease_of_movement_breakout", prefix="eom", params={}, entry_params=EOM_PARAMS, tf="4h",
        ref="backtest/candidates/ease_of_movement_1671/REPORT.md", solo=legacy_solo, entry=eom_bars,
        tp=TP_2PCT, eval_tp={"name": "tiered_tp_atr"},
        eval_spec={"closed": False, "indicators": {"eom_interval_ms": 14_400_000.0, "eom_valid": 1.0},
                   "absent": ("eom_eval_ts_ms",), "labels_in": {"eom_reason": EOM_LABELS}},
        batch_peer="breakout", batch_signals=(0, 1),
    ),
    Case(
        name="money_flow_index_reversal", prefix="mfi", params=MFI_ADMISSION, entry_params=MFI_PARAMS, tf="4h",
        ref="backtest/candidates/money_flow_index_reversal_1658/REPORT.md", solo=legacy_solo, entry=mfi_bars,
        tp=TP_EMPTY, eval_tp={"name": "tiered_tp_atr", "params": {}},
        eval_spec={"closed": False, "indicators": {"mfi_lookback": 14.0}, "indicators_in": {"mfi_valid": (0.0, 1.0)}},
        batch_signals=(0, 1),
    ),
    Case(
        name="on_balance_volume_divergence", prefix="obvd", params={}, entry_params=OBV_PARAMS, tf="4h",
        ref="backtest/candidates/on_balance_volume_divergence_1657/REPORT.md", solo=legacy_solo,
        entry=lambda side=1: obv_bars(OBV_ENTRY), tp=TP_2PCT, eval_tp={"name": "tiered_tp_pct"},
        eval_spec={"closed": False, "indicators": {"obvd_support_bars": 59.0}},
        batch_peer="breakout", batch_signals=(0, 1),
    ),
    Case(
        name="regression_channel_reversion", prefix="rcr", params=RCR_ADMISSION, entry_params=RCR_PARAMS, tf="4h",
        ref="backtest/candidates/regression_channel_reversion_1644/REPORT.md", solo=legacy_solo,
        entry=lambda side=1: rcr_bars(40, closes={38: 97.5, 39: 100.0}), tp=TP_3PCT, default_closes=(TP_3PCT,),
        eval_spec={"closed": False, "indicators": {"rcr_fit_window": 50.0, "rcr_interval_ms": 14_400_000.0},
                   "indicators_in": {"rcr_reason_code": (-1, 0, 1, 2, 3, 17)}, "label_present": True},
        batch_peer="breakout", batch_bad=({"fit_window": 2.5}, "fit_window must be an integer"),
        batch_entry=lambda side=1: rcr_bars(40, closes={37: 97.5, 38: 100.0}), batch_signals=(1, 0),
        batch_check=rcr_batch_labels,
    ),
    Case(
        name="ultimate_oscillator", prefix="uo", params={}, tf="4h",
        ref="backtest/candidates/ultimate_oscillator_1664/REPORT.md", solo=legacy_solo, tp=TP_3PCT,
        eval_spec={"closed": False, "indicators": {}, "present": ("uo_reason_code",)},
    ),
    Case(
        name="williams_r_reversal", prefix="wr", params={}, tf="1h",
        ref="backtest/candidates/williams_r_reversal_1650/REPORT.md", solo=wr_solo, tp=WR_TP,
        eval_spec={"closed": False, "indicators": {}}, batch_bad=({"lookback": 7.5}, None),
    ),
]
CASES = {case.name: case for case in CASE_LIST + FOLDED_CASES}
CASE_NAMES = sorted(CASES)


def _research_tools():
    for path in (os.path.join(REPO, "shared_tools"), os.path.join(REPO, "backtest")):
        if path not in sys.path:
            sys.path.insert(0, path)


def _research_bars(closes, step, start, spread=None):
    out = []
    for i, c in enumerate(closes):
        o = closes[i - 1] if i else c
        hi, lo = (max(o, c) + 0.5, min(o, c) - 0.5) if spread is None else (max(o, c) * (1 + spread), min(o, c) * (1 - spread))
        out.append({"t": start + i * step, "T": start + (i + 1) * step - 1, "o": o, "h": hi, "l": lo, "c": c,
                    "v": 1.0})
    return out


def _research_market(frames, cutoff, coin, mid, **sections):
    out = market(frames, cutoff_ms=cutoff, mid=mid)
    out["mids"] = {coin: dict(out["mids"]["BTC"], px=mid)}
    out.update(sections)
    return out


def _research_decimal(value, places="0.0001"):
    from decimal import ROUND_HALF_EVEN, Decimal
    text = format(Decimal(repr(float(value))).quantize(Decimal(places), rounding=ROUND_HALF_EVEN).normalize(), "f")
    return text.rstrip("0").rstrip(".") if "." in text else text


CVD_STEP = 300_000
CVD_T0 = 1_767_225_600_000
CVD_PARAMS = {"left_span": 2, "right_span": 2, "min_separation": 2, "max_separation": 36, "delta_threshold": 0.05,
              "setup_expiry": 4, "session_hours": 4, "expected_interval_ms": CVD_STEP, "delta_confirmation": True}


def cvd_research_market(side_input=True):
    _research_tools()
    from trade_flow_replay import CONTRACT, PAYLOAD_CONTRACT_KEYS, payload_from_series
    n, p1, p2 = 40, 29, 35
    closes, highs, lows, intervals = [], [], [], []
    for i in range(n):
        lo, hi, cl = 100.0, 101.0, 100.5
        if i == p1:
            lo, hi, cl = 95.0, 96.0, 95.5
        if i == p2:
            lo, hi, cl = 94.0, 95.0, 94.5
        if i == n - 1:
            hi, cl = 102.0, 101.5
        closes.append(cl), highs.append(hi), lows.append(lo)
        buy = "0.12" if p1 < i <= p2 else "0.1"
        for k in range(5):
            s = CVD_T0 + i * CVD_STEP + k * 60_000
            intervals.append({"start_ms": s, "end_ms": s + 60_000, "buy": buy,
                              "sell": _research_decimal(0.2 - float(buy)), "total": "0.2", "trades": 3,
                              "records_hash": f"{s:064x}", "session": 1, "finalized_ms": s + 66_000,
                              "status": "complete", "invalid_ms": None, "reason": ""})
    bars = []
    for i in range(n):
        o = min(max(closes[i - 1] if i else closes[i], lows[i]), highs[i])
        bars.append({"t": CVD_T0 + i * CVD_STEP, "T": CVD_T0 + (i + 1) * CVD_STEP - 1, "o": o, "h": highs[i],
                     "l": lows[i], "c": closes[i], "v": 1.0})
    series = {"coin": "BTC", **{k: CONTRACT[k] for k in PAYLOAD_CONTRACT_KEYS}, "intervals": intervals,
              "economic_evidence": True, "refusals": []}
    flows = {"BTC|trade_flow": {**payload_from_series(series, CVD_STEP), "kind": "trade_flow", "coin": "BTC",
                                "available": True}} if side_input else {}
    return _research_market({"BTC|5m": frame(bars, CVD_STEP, timing=False)}, bars[-1]["T"] + 1, "BTC", 101.5,
                            trade_flows=flows)


OBI_PARAMS = {"depth_levels": 5, "entry_threshold": 0.3, "confirmation_observations": 3, "max_spread_bps": 5.0,
              "min_combined_depth_usd": 100000.0, "max_snapshot_age_ms": 2000}


def obi_research_market(side_input=True):
    _research_tools()
    import hashlib
    from book_inputs import CONTRACT
    base = 1_785_542_410_000

    def book(seq, recv, bid_sz, ask_sz):
        bids = [[f"{100000 - i}", f"{bid_sz}", 1] for i in range(5)]
        asks = [[f"{100001 + i}", f"{ask_sz}", 1] for i in range(5)]
        event = recv - 300
        return {"seq": seq, "event_ms": event, "recv_ms": recv, "session": 1, "bid_depth": 5, "ask_depth": 5,
                "identity": hashlib.sha256(f"{seq}/{event}/{bid_sz}/{ask_sz}".encode()).hexdigest(),
                "bids": bids, "asks": asks}

    books = [book(2, base - 6_000, 1, 1)] + [book(3 + i, base + i * 5_400, 3, 1) for i in range(4)]
    crossing = books[3]
    deadline = -(-max(crossing["event_ms"], crossing["recv_ms"]) // 1000) * 1000
    section = {"kind": "l2_book", "coin": "BTC", **CONTRACT, "window_ms": 60_000, "capacity": 256, "depth": 5,
               "cutoff_ms": deadline, "retained_from_ms": deadline - 60_000, "prefix_complete": True,
               "owner_instance": "contract", "snapshot_id": "contract", "available": True,
               "observations": [b for b in books if deadline - 60_000 <= b["recv_ms"] <= deadline],
               "markers": [{"seq": 1, "at_ms": base - 9_000, "reason": "subscribed", "session": 1, "detail": ""}]}
    bars = _research_bars([100000.5] * 40, 60_000, deadline - 40 * 60_000)
    return _research_market({"BTC|1m": frame(bars, 60_000, timing=False)}, deadline, "BTC", 100000.5,
                            l2_books={"BTC|l2_book": section} if side_input else {})


PPR_PARAMS = {"lookback_samples": 10, "entry_z": 2.0, "turn_span": 1, "variance_floor": 1e-10, "max_spread_bps": 5.0,
              "max_quote_age_ms": 5_000, "max_oracle_age_ms": 5_000, "max_skew_ms": 3_000, "max_abs_premium": 0.01,
              "max_mark_dislocation": 0.005, "expected_interval_ms": 60_000}


def ppr_research_market(side_input=True):
    _research_tools()
    from premium_replay import CONTRACT, PAYLOAD_CONTRACT_KEYS, payload_from_series
    step, t0, oracle, n = 60_000, 1_767_225_600_000, 100.0, 40
    premiums = [1e-4 * (k % 3) for k in range(n)]
    premiums[n - 2], premiums[n - 1] = 8e-4, 4e-4
    samples = []
    for i, p in enumerate(premiums):
        t = t0 + (i + 1) * step
        mid = oracle * (1.0 + p)
        half = mid * 0.5 / 10_000
        samples.append({"t": t, "status": "sampled", "finalized_ms": t + 1, "invalid_ms": None,
                        "quote": {"bid": _research_decimal(mid - half, "0.000001"), "bid_sz": "1.5", "bid_n": 3,
                                  "ask": _research_decimal(mid + half, "0.000001"), "ask_sz": "2", "ask_n": 4,
                                  "event_ms": t - 1_200, "recv_ms": t - 1_000, "session": 1, "seq": t // step},
                        "context": {"oracle": _research_decimal(oracle), "mark": _research_decimal(mid, "0.000001"),
                                    "mid": None, "oracle_event_ms": None, "recv_ms": t - 2_000, "session": 1,
                                    "seq": t // step}})
    series = {"coin": "BTC", **{k: CONTRACT[k] for k in PAYLOAD_CONTRACT_KEYS}, "samples": samples,
              "economic_evidence": True, "refusals": []}
    bars = _research_bars([oracle * (1.0 + p) for p in premiums], step, t0, spread=0.0004)
    return _research_market({"BTC|1m": frame(bars, step, timing=False)}, bars[-1]["T"] + 1, "BTC", bars[-1]["c"],
                            perp_premiums={"BTC|perp_premium": payload_from_series(series, step)} if side_input else {})


VPR_STEP = 900_000
VPR_PARAMS = {"profile_bins": 4, "value_area_fraction": 0.6, "session_bounds": False, "expected_interval_ms": VPR_STEP}


def vpr_research_market(side_input=True):
    _research_tools()
    import copy
    import hashlib
    import volume_profile_inputs as vpi
    from trade_flow_replay import _Interval
    from volume_profile_replay import (PAYLOAD_KEYS, SERIES_SCHEMA, ProfileReplay, _session_record,
                                       payload_from_profile_series, sessions_sha256)
    day = 1_772_409_600_000
    profile = ProfileReplay("BTC")
    tid = day // 1000
    for px, count in (("100", 2), ("101", 1), ("102", 4), ("103", 1), ("104", 2), ("105", 3), ("106", 1), ("108", 2)):
        for _ in range(count):
            tid += 1
            at = day + (tid % vpi.MINUTES) * 60_000
            profile.add_trade({"coin": "BTC", "tid": tid, "time": at + 10, "side": "B", "px": px, "sz": "0.01",
                               "units": 1_000_000, "hash": f"{tid:064x}", "buyer": "0xa", "seller": "0xb"},
                              hashlib.sha256(str(tid).encode()).digest(), at, day + 1)
    for m in range(vpi.MINUTES):
        interval = _Interval(day + m * 60_000, 1)
        interval.status = "complete"
        profile.finalize_minute(interval, day + vpi.SESSION_MS + 5_000 if m == vpi.MINUTES - 1
                                else day + (m + 1) * 60_000 + 5_000)
    sessions = [copy.deepcopy(_session_record(d)) for _, d in sorted(profile.all_days().items())]
    series = {"series_schema": SERIES_SCHEMA, "kind": "volume_profile", "coin": "BTC",
              **{k: vpi.CONTRACT[k] for k in PAYLOAD_KEYS}, "sessions": sessions,
              "sessions_sha256": sessions_sha256(sessions), "economic_evidence": True, "refusals": []}
    closes = [104.0] * 96 + [104.0, 101.0, 103.0, 104.0]
    bars = [{"t": day + i * VPR_STEP, "T": day + (i + 1) * VPR_STEP - 1, "o": c, "h": c + 0.2, "l": c - 0.2, "c": c,
             "v": 10.0} for i, c in enumerate(closes)]
    cutoff = bars[-1]["t"] + VPR_STEP // 2
    profiles = {"BTC|volume_profile": payload_from_profile_series(series, VPR_STEP, cutoff)} if side_input else {}
    return _research_market({"BTC|15m": frame(bars, VPR_STEP)}, cutoff, "BTC", 104.0, volume_profiles=profiles)


MEB_CALENDAR_SHA = "1" * 64
MEB_QUOTE_SHA = "2" * 64
MEB_PARAMS = {"expected_interval_ms": H, "range_bars": 6, "post_window_bars": 4, "confirm_bars": 1,
              "max_spread_frac": 0.0005, "max_quote_age_ms": 60_000, "cooldown_bars": 24, "holding_bars": 6,
              "spread_gate": True, "calendar_sha256": MEB_CALENDAR_SHA, "quote_sha256": MEB_QUOTE_SHA,
              "calendar_path": "contract/calendar.json", "quote_path": "contract/quotes.json"}


def meb_research_market(side_input=True):
    _research_tools()
    import macro_event_inputs as mi
    t0, event_bar = 1_767_571_200_000, 100
    release = t0 + event_bar * H
    source = {"source_id": "contract", "publisher": "test", "url": "contract:macro", "retrieved_ms": t0,
              "sha256": "0" * 64, "published_ms": None, "note": "contract fixture"}
    event = {"event_id": "contract/fomc", "event_type": mi.EVENT_TYPE_FOMC, "revision": 1, "status": "scheduled",
             "scheduled_local": pd.Timestamp(release, unit="ms").strftime("%Y-%m-%dT%H:%M:%S"),
             "source_timezone": "UTC", "known_ms": t0 - 200 * H, "publication_evidence": "contract",
             "source_id": "contract"}
    coverage = [{"event_type": mi.EVENT_TYPE_FOMC, "start_ms": t0 - 1000 * H, "end_ms": t0 + 1000 * H}]
    calendar, _ = mi.import_calendar([event], [source], coverage, "fixture", [mi.EVENT_TYPE_FOMC])
    n = event_bar + 2
    quote_rows = [{"event_ms": t0 + i * H - 2_000, "recv_ms": t0 + i * H - 1_500, "bid": "100.000000",
                   "ask": "100.010000", "source_id": "contract"} for i in range(n + 1)]
    quotes, _ = mi.import_quotes(quote_rows, [{k: v for k, v in source.items() if k != "published_ms"}], "BTC",
                                 "fixture")
    closes = [100.0] * n
    closes[event_bar], closes[event_bar + 1] = 101.2, 101.5
    bars = _research_bars(closes, H, t0)
    cutoff = bars[-1]["t"] + H // 2
    sections = {"macro_events": {MEB_CALENDAR_SHA: mi.calendar_payload(calendar, MEB_CALENDAR_SHA, cutoff)},
                "macro_quotes": {f"BTC|{MEB_QUOTE_SHA}": mi.quote_payload(quotes, MEB_QUOTE_SHA, cutoff)}}
    return _research_market({"BTC|1h": frame(bars, H)}, cutoff, "BTC", 101.5,
                            **(sections if side_input else dict(sections, macro_quotes={})))


BLL_PARAMS = {"training_bars": 48, "lag_bars": 1, "horizon_bars": 2, "impulse_z": 2.0, "min_r2": 0.0,
              "min_edge_bps": 0.0, "cost_bps": 0.0, "max_data_age_ms": 86_400_000, "expected_interval_ms": H}


def bll_research_market(side_input=True):
    import math
    rng = random.Random(1640)
    n, t0 = 120, 1_767_225_600_000
    leader = [rng.gauss(0, 0.004) for _ in range(n)]
    for k in range(5, n, 9):
        leader[k] = 0.02 * (1 if (k // 9) % 2 else -1)
    follower = [0.7 * sum(leader[k - j] for j in (1, 2) if k - j >= 0) / 2 + rng.gauss(0, 0.0005) for k in range(n)]
    leader[n - 2] = 0.03
    btc, eth = [30000.0], [2000.0]
    for k in range(1, n):
        btc.append(btc[-1] * math.exp(leader[k]))
        eth.append(eth[-1] * math.exp(follower[k]))
    eth_bars, btc_bars = _research_bars(eth, H, t0, spread=0.001), _research_bars(btc, H, t0, spread=0.001)
    frames = {"ETH|1h": frame(eth_bars, H)}
    if side_input:
        frames["BTC|1h"] = frame(btc_bars, H)
    return _research_market(frames, eth_bars[-1]["t"] + H // 2, "ETH", eth[-1])


@dataclasses.dataclass(frozen=True)
class ResearchCase:
    name: str
    prefix: str
    symbol: str
    tf: str
    params: Dict[str, Any]
    close: Dict[str, Any]
    market: Callable
    signal: int
    reason: str
    unavailable: str
    closed: bool = False


RESEARCH_CASE_LIST = [
    ResearchCase("cumulative_volume_delta_divergence", "cvd", "BTC", "5m", CVD_PARAMS, TP_3PCT, cvd_research_market,
                 1, "entry_long", "trade_flow_unavailable"),
    ResearchCase("order_book_imbalance", "obi", "BTC", "1m", OBI_PARAMS, TP_3PCT, obi_research_market,
                 1, "entry_long", "book_unavailable"),
    ResearchCase("perpetual_premium_reversion", "ppr", "BTC", "1m", PPR_PARAMS, TP_3PCT, ppr_research_market,
                 -1, "entry_short", "premium_unavailable"),
    ResearchCase("volume_profile_reclaim", "vpr", "BTC", "15m", VPR_PARAMS, TP_3PCT, vpr_research_market,
                 1, "entry_long", "profile_input_missing", closed=True),
    ResearchCase("macro_event_breakout", "meb", "BTC", "1h", MEB_PARAMS, {"name": "time_stop", "params": {"max_bars": 6}},
                 meb_research_market, 1, "entry_long", "quotes_unavailable", closed=True),
    ResearchCase("bitcoin_lead_lag", "bll", "ETH", "1h", BLL_PARAMS, TP_3PCT, bll_research_market,
                 1, "entry_long", "leader_unavailable", closed=True),
]
RESEARCH_CASES = {case.name: case for case in RESEARCH_CASE_LIST}

EXEMPT = {
    "awesome_oscillator": "no check-script contract tests yet; only the backtest harness test covers it",
    "chaikin_money_flow_breakout": "no check-script contract tests yet; only the backtest harness test covers it",
    "commodity_channel_trend": "no check-script contract tests yet; only the backtest harness test covers it",
    "connors_rsi_reversion": "no check-script contract tests yet; only the backtest harness test covers it",
    "dual_thrust": "no check-script contract tests yet; only the backtest harness test covers it",
    "open_interest_breakout": "no check-script contract tests yet; only the backtest harness tests cover it",
    "relative_vigor_index": "no check-script contract tests yet; only the backtest harness test covers it",
    "vortex_trend": "check-script admission tests use it as the generic no-edge solo in test_no_edge_admission.py",
}


def contract_selector(registry):
    return sorted(
        name for name, entry in registry.STRATEGIES.items()
        if entry.get("edge_status") == "no_edge"
        and tuple(entry.get("platforms") or ()) == ("futures",)
        and entry.get("short_entries") is True
        and str(entry.get("edge_ref") or "").startswith("backtest/candidates/")
        and not entry.get("quote")
    )


def cases_with(attr):
    return [name for name in CASE_NAMES if getattr(CASES[name], attr)]


def refs(case, closes=DEFAULT, params=None):
    out = {"open": {"name": case.name, "params": dict(case.params if params is None else params)}}
    closes = case.default_closes if closes is DEFAULT else closes
    if closes is not None:
        out["closes"] = list(closes)
    return json.dumps(out)


def erefs(case, closes=DEFAULT, params=None):
    return refs(case, closes=closes, params=case.closed_params() if params is None else params)


def fallback_refs(case, params=None):
    close = {"name": case.name}
    if params is not None:
        close["params"] = params
    return json.dumps({"open": {"name": "breakout"}, "closes": [close]})


def check(case, bars, *, closed, extra=(), timing=True, cutoff=None, mid=None, limit=None, rows=None,
          strategy_refs=None):
    argv = [case.name, "BTC", "1h", "--mode=paper", "--market-stdin", "--ohlcv-limit", str(limit or case.limit),
            "--strategy-refs", erefs(case) if strategy_refs is None else strategy_refs, *extra]
    if closed:
        argv.append("--closed-bar-decisions")
    stdin = envelope(bars, H, "BTC|1h", cutoff=cutoff, mid=mid, snapshot_id=case.snapshot_id, timing=timing,
                     rows=rows)
    proc = run(argv, stdin)
    assert proc.returncode == 0, (case.name, proc.stderr[-3000:] + proc.stdout[-2000:])
    return json.loads(proc.stdout)


def boundary(bars):
    return bars[-1]["T"] + 1


def test_every_registered_study_candidate_has_a_case_or_a_recorded_exemption():
    registry = strategies()._registry
    selected = contract_selector(registry)
    assert not set(CASES) & set(EXEMPT), sorted(set(CASES) & set(EXEMPT))
    assert not (set(CASES) | set(EXEMPT)) & set(RESEARCH_CASES)
    assert sorted(set(RESEARCH_CASES)) == sorted(n for n in selected if registry.STRATEGIES[n].get("production_refused"))
    assert sorted(set(CASES) | set(EXEMPT) | set(RESEARCH_CASES)) == selected, {
        "missing_case": sorted(set(selected) - set(CASES) - set(EXEMPT) - set(RESEARCH_CASES)),
        "not_selected": sorted((set(CASES) | set(EXEMPT) | set(RESEARCH_CASES)) - set(selected)),
    }
    for name in CASE_NAMES:
        entry = registry.STRATEGIES[name]
        assert entry["edge_ref"] == CASES[name].ref, name
        assert name in registry.no_edge_strategies(), name
    for name, reason in sorted(EXEMPT.items()):
        assert reason.strip(), name


def research_refs(case, closes=None):
    return {"open": {"name": case.name, "params": dict(case.params)},
            "closes": [dict(case.close)] if closes is None else closes}


@pytest.mark.parametrize("name", sorted(RESEARCH_CASES))
def test_research_explicit_paper_evaluation_runs_on_complete_sealed_input_and_every_other_mode_is_refused(name):
    case = RESEARCH_CASES[name]
    label = f"{case.prefix}_reason"
    stdin = {"v": 2, "market": case.market()}
    strategy_refs = research_refs(case)
    base = [case.name, case.symbol, case.tf, "--market-stdin", "--strategy-refs", json.dumps(strategy_refs)]
    outs = []
    for extra in (["--mode=paper"], ["--mode", "paper"]) + ((["--mode=paper", "--closed-bar-decisions"],) if case.closed else ()):
        proc = run(base + extra, stdin)
        assert proc.returncode == 0, (extra, proc.stderr[-2000:])
        out = payload(proc)
        assert not out.get("error"), (extra, out.get("error"))
        assert out["signal"] == case.signal and out["indicator_labels"][label] == case.reason, (extra, out)
        assert out["open_strategy"] == case.name and out["close_strategies"] == [case.close["name"]], extra
        assert "fill" not in out and "order" not in out and "Fetching" not in proc.stderr, extra
        outs.append(out)
    assert outs[0]["indicators"] == outs[1]["indicators"]
    if case.closed:
        assert outs[2]["closed_bar_decision"]["held"] is False, outs[2]["closed_bar_decision"]
    for extra in (["--mode=live", "--allow-no-edge"], ["--mode=live"], ["--allow-no-edge"], []):
        proc = run(base + extra, stdin)
        assert proc.returncode == 1, extra
        assert f"open strategy '{case.name}' is production_refused" in payload(proc)["error"], extra
        assert "Fetching" not in proc.stderr, extra
    proc = run(base[:3] + ["--market-stdin", "--mode=paper", "--strategy-refs", json.dumps(
        {"open": {"name": "breakout"}, "closes": [{"name": case.name, "params": dict(case.params)}]})], stdin)
    assert proc.returncode == 1 and "sealed inputs attach only to the open strategy" in payload(proc)["error"]
    missing = {"v": 2, "market": case.market(side_input=False)}
    proc = run(base + ["--mode=paper"], missing)
    assert proc.returncode == 0, proc.stderr[-2000:]
    held = payload(proc)
    assert held["signal"] == 0 and held["indicator_labels"][label] == case.unavailable, held
    assert "Fetching" not in proc.stderr
    slots = [{"id": "paper", "strategy": case.name, "mode": "paper", "mode_args": ["--mode=paper"],
              "strategy_refs": strategy_refs},
             {"id": "acked", "strategy": case.name, "mode": "live", "mode_args": ["--mode=live"],
              "allow_no_edge": True, "strategy_refs": strategy_refs}]
    proc = run(["--batch-check", f"--symbol={case.symbol}", f"--timeframe={case.tf}", "--market-stdin"],
               dict(stdin, slots=slots))
    results = {r["id"]: r for r in payload(proc)["results"]}
    for key in ("signal", "open_action", "indicators", "indicator_labels"):
        assert results["paper"].get(key) == outs[0].get(key), key
    assert f"open strategy '{case.name}' is production_refused" in results["acked"]["error"]


@pytest.mark.parametrize("name", cases_with("fallback_evaluates"))
def test_close_fallback_paper_and_acknowledged_live_evaluate(name):
    case = CASES[name]
    params = case.params if case.fallback_with_params else None
    for extra in (["--mode=paper"], ACKED):
        proc = run(["breakout", "BTC", case.tf, "--strategy-refs", fallback_refs(case, params), "--market-stdin"]
                   + extra, case.solo())
        assert proc.returncode == 0, (extra, proc.stderr[-2000:])
        out = payload(proc)
        assert not out.get("error"), extra
        assert out["close_strategies"] == [name], extra


@pytest.mark.parametrize("name", cases_with("eval_spec"))
def test_paper_or_acknowledged_evaluation_runs_on_sealed_input_without_orders(name):
    case = CASES[name]
    spec = case.eval_spec
    tp = case.admission_tp()
    strategy_refs = refs(case, closes=[tp])
    flags = ["--closed-bar-decisions"] if spec["closed"] else []
    outs = []
    for extra in EVAL_EXTRAS:
        proc = run([name, "BTC", case.tf, "--strategy-refs", strategy_refs, "--market-stdin"] + flags + extra,
                   case.solo())
        assert proc.returncode == 0, (extra, proc.stderr[-2000:])
        out = payload(proc)
        assert not out.get("error"), extra
        assert out["strategy"] == name and out["signal"] in (-1, 0, 1), extra
        assert out["open_strategy"] == name and out["close_strategies"] == [tp["name"]], extra
        for key, value in spec["indicators"].items():
            assert out["indicators"][key] == value, (extra, key)
        for key, allowed in spec.get("indicators_in", {}).items():
            assert out["indicators"][key] in allowed, (extra, key)
        for key in spec.get("present", ()):
            assert key in out["indicators"], (extra, key)
        for key in spec.get("absent", ()):
            assert key not in out["indicators"], (extra, key)
        for key, allowed in spec.get("labels_in", {}).items():
            assert out["indicator_labels"][key] in allowed, (extra, key)
        for key, value in spec.get("labels", {}).items():
            assert out["indicator_labels"][key] == value, (extra, key)
        if spec.get("label_present"):
            assert out["indicator_labels"][case.reason()], extra
        if "signal" in spec:
            assert out["signal"] == spec["signal"] and out["open_action"] == spec["open_action"], extra
        assert "fill" not in out and "order" not in out, extra
        outs.append(out)
    for out in outs[1:]:
        assert out["indicators"] == outs[0]["indicators"]
    if spec.get("check"):
        spec["check"](outs[0])
    if case.solo_short is not None:
        short = payload(run([name, "BTC", case.tf, "--strategy-refs", strategy_refs, "--market-stdin",
                             "--mode=paper"] + flags, case.solo_short()))
        assert short["signal"] == -1 and short["open_action"] == "short"
        assert short["indicator_labels"][case.reason()] == spec["short_label"]


@pytest.mark.parametrize("name", cases_with("peers"))
def test_retained_peer_strategies_still_evaluate(name):
    case = CASES[name]
    for peer in case.peers:
        proc = run([peer, "BTC", case.tf, "--mode=paper", "--market-stdin"], case.solo())
        assert proc.returncode == 0, (peer, proc.stderr[-2000:])
        assert not payload(proc).get("error"), peer


def batch(case, slots, stdin):
    return run(["--batch-check", "--symbol=BTC", f"--timeframe={case.tf}", "--ohlcv-limit", str(case.limit),
                "--market-stdin"], dict(stdin, slots=slots))


@pytest.mark.parametrize("name", cases_with("eval_spec"))
def test_batch_slots_apply_the_same_admission_and_match_solo(name):
    case = CASES[name]
    strategy_refs = json.loads(refs(case, closes=[case.admission_tp()]))
    fallback = json.loads(fallback_refs(case))
    keyed = {"strategy": "breakout", "open_strategy": "breakout"}
    slots = [
        {"id": "paper", "strategy": name, "mode": "paper", "mode_args": ["--mode=paper"],
         "strategy_refs": strategy_refs},
        {"id": "acked", "strategy": name, "mode": "live", "mode_args": ["--mode=live"], "allow_no_edge": True,
         "strategy_refs": strategy_refs},
        {"id": "unacked-live", "strategy": name, "mode": "live", "mode_args": ["--mode=live"],
         "strategy_refs": strategy_refs},
        dict(keyed, id="keys-list-paper", close_strategies=[name], mode="paper", mode_args=["--mode=paper"]),
        dict(keyed, id="keys-list-live", close_strategies=[name], mode="live", mode_args=["--mode=live"]),
        {"id": "fallback-live", "strategy": "breakout", "mode": "live", "mode_args": ["--mode=live"],
         "strategy_refs": fallback},
        {"id": "fallback-paper", "strategy": "breakout", "mode": "paper", "mode_args": ["--mode=paper"],
         "strategy_refs": fallback},
        {"id": "fallback-acked", "strategy": "breakout", "mode": "live", "mode_args": ["--mode=live"],
         "allow_no_edge": True, "strategy_refs": fallback},
    ]
    oks = ["paper", "acked", "fallback-paper", "fallback-acked", "keys-list-paper"]
    closed_gate = f"close strategy '{name}' is edge_status=no_edge"
    refused = {"unacked-live": "explicit live mode", "fallback-live": closed_gate, "keys-list-live": closed_gate}
    if case.batch_bad is not None:
        slots.append({"id": "bad-params", "strategy": name, "mode": "paper", "mode_args": ["--mode=paper"],
                      "strategy_refs": json.loads(refs(case, closes=[case.admission_tp()], params=case.batch_bad[0]))})
        refused["bad-params"] = f"open strategy '{name}' has invalid parameters"
        if case.batch_bad[1]:
            refused["bad-params"] = case.batch_bad[1]
    if case.batch_peer:
        slots.append({"id": "peer", "strategy": case.batch_peer, "mode": "paper", "mode_args": ["--mode=paper"]})
        oks.append("peer")
    stdin = case.solo()
    proc = batch(case, slots, stdin)
    assert proc.returncode == 1
    results = {r["id"]: r for r in payload(proc)["results"]}
    for ok in oks:
        assert not results[ok].get("error"), results[ok]
    for slot_id, needle in sorted(refused.items()):
        assert needle in results[slot_id]["error"], (slot_id, results[slot_id].get("error"))
        assert results[slot_id]["signal"] == 0 and not results[slot_id].get("indicators"), slot_id
    assert results["keys-list-paper"]["close_strategies"] == [name]
    solo = payload(run([name, "BTC", case.tf, "--mode=paper", "--ohlcv-limit", str(case.limit), "--strategy-refs",
                        json.dumps(strategy_refs), "--market-stdin"], stdin))
    if case.solo_signal is not None:
        assert solo["signal"] == case.solo_signal
    for key in ("signal", "open_action", "close_fraction", "indicators", "indicator_labels"):
        assert results["paper"].get(key) == solo.get(key), key
        assert results["acked"].get(key) == solo.get(key), key


def evaluator_params():
    out = []
    for name in cases_with("evaluator"):
        for side in CASES[name].evaluator["sides"]:
            out.append(pytest.param(name, side, id=f"{name}-{'long' if side == 1 else 'short'}"))
    return out


@pytest.mark.parametrize("name,side", evaluator_params())
def test_check_decision_matches_the_evaluator_at_full_precision(name, side):
    case = CASES[name]
    spec = case.evaluator
    bars = spec["bars"](side)
    out = check(case, bars, closed=False)
    df = strategies().apply_strategy(name, frame_df(rows_of(bars[-200:]), spec["timeframe"]), case.closed_params())
    last = df.iloc[-1]
    assert out["signal"] == int(last["signal"]) == side
    assert out["open_action"] == ("long" if side == 1 else "short")
    full = None
    if spec["full"]:
        full = strategies().apply_strategy(name, frame_df(rows_of(bars), spec["timeframe"]), case.closed_params())
    for col in spec["columns"]:
        want = round(float(last[col]), 6) if spec["round"] else float(last[col])
        assert out["indicators"][col] == want, col
        if full is not None:
            assert out["indicators"][col] == float(full[col].iloc[-1]), col
    if spec["label_match"]:
        assert out["indicator_labels"][case.reason()] == last[case.reason()]
    if spec["label"]:
        assert out["indicator_labels"][case.reason()] == spec["label"]
    if spec["identity"]:
        spec["identity"](out["indicators"], bars)


@pytest.mark.parametrize("name", cases_with("batch_signals"))
def test_closed_bar_batch_matches_individual_checks(name):
    case = CASES[name]
    bars = (case.batch_entry or case.entry)(1)
    cutoff = boundary(bars) if case.boundary_cutoff else bars[-1]["t"] + H // 2
    stdin = {"v": 2, "market": market({"BTC|1h": frame(bars, H)}, cutoff_ms=cutoff, mid=bars[-1]["c"])}
    strategy_refs = json.loads(erefs(case))
    valid = [
        {"id": "on", "strategy": name, "mode": "paper", "mode_args": ["--mode=paper"],
         "strategy_refs": strategy_refs, "closed_bar_decisions": True},
        {"id": "off", "strategy": name, "mode": "paper", "mode_args": ["--mode=paper"],
         "strategy_refs": strategy_refs},
    ]
    if case.batch_peer:
        valid.append({"id": "peer", "strategy": case.batch_peer, "mode": "paper", "mode_args": ["--mode=paper"],
                      "closed_bar_decisions": True})
    failing = [{"id": "refused", "strategy": name, "mode": "live", "mode_args": ["--mode=live"],
                "strategy_refs": strategy_refs, "closed_bar_decisions": True}]
    if case.batch_bad is not None:
        failing.append({"id": "bad-params", "strategy": name, "mode": "paper", "mode_args": ["--mode=paper"],
                        "strategy_refs": json.loads(erefs(case, params=case.batch_bad[0])),
                        "closed_bar_decisions": True})
    argv = ["--batch-check", "--symbol=BTC", "--timeframe=1h", "--ohlcv-limit", "200", "--atr-method=simple",
            "--market-stdin"]
    proc = run(argv, dict(stdin, slots=valid))
    assert proc.returncode == 0, proc.stderr + proc.stdout
    clean = {r["id"]: r for r in json.loads(proc.stdout)["results"]}
    mixed = {r["id"]: r for r in payload(run(argv, dict(stdin, slots=valid + failing)))["results"]}
    assert "explicit live mode" in mixed["refused"]["error"] and mixed["refused"]["signal"] == 0
    assert not mixed["refused"].get("indicators")
    if case.batch_bad is not None:
        assert "has invalid parameters" in mixed["bad-params"]["error"] and mixed["bad-params"]["signal"] == 0
        assert not mixed["bad-params"].get("indicators")
    for slot_id, closed in (("on", True), ("off", False)):
        argv_single = [name, "BTC", "1h", "--mode=paper", "--market-stdin", "--ohlcv-limit", "200",
                       "--atr-method=simple", "--strategy-refs", erefs(case)]
        if closed:
            argv_single.append("--closed-bar-decisions")
        single = run(argv_single, stdin)
        assert single.returncode == 0, single.stderr
        for results in (clean, mixed):
            assert not results[slot_id].get("error"), results[slot_id]
            assert strip(results[slot_id]) == strip(json.loads(single.stdout)), slot_id
    want_on, want_off = case.batch_signals
    assert clean["on"]["signal"] == want_on
    assert clean["off"]["signal"] == want_off
    if case.reconstruct is not None:
        assert case.reconstruct(clean["on"]["indicators"]) == want_on
    if case.batch_peer:
        assert not clean["peer"].get("error") and not mixed["peer"].get("error")
    if case.batch_check is not None:
        case.batch_check(clean, mixed)


@pytest.mark.parametrize("name", cases_with("opposite_entry_closes"))
def test_closed_bar_close_fallback_closes_on_an_opposite_entry(name):
    case = CASES[name]
    for entry_side, held_index, close_signal in ((-1, 0, -1), (1, 1, 1)):
        bars = case.entry(entry_side)
        held_args = case.helds[held_index]
        same_side = case.helds[1 - held_index]
        argv = ["breakout", "BTC", "1h", "--mode=paper", "--market-stdin", "--ohlcv-limit", "200",
                "--closed-bar-decisions", "--strategy-refs", fallback_refs(case, case.closed_params())]
        stdin = envelope(bars, H, "BTC|1h", cutoff=boundary(bars))
        proc = run(argv + held_args, stdin)
        assert proc.returncode == 0, proc.stderr + proc.stdout
        out = json.loads(proc.stdout)
        assert out["closed_bar_decision"]["held"] is False, entry_side
        assert out["close_strategies"] == [name], entry_side
        assert out["close_strategy"] == name and out["close_fraction"] == 1.0, entry_side
        assert out["signal"] == close_signal, entry_side
        kept = json.loads(run(argv + same_side, stdin).stdout)
        assert kept["close_fraction"] == 0.0, entry_side
