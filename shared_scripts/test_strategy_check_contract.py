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
EVAL_EXTRAS = (["--mode=paper"], ["--mode", "paper"], ["--mode=live", "--allow-no-edge"], ["--allow-no-edge"])
DEFAULT = object()


def held(side, cost):
    return ["--position-side", side, f"--position-avg-cost={cost}", "--position-qty=1", "--position-initial-qty=1"]


HELD_LONG = held("long", 50)
HELD_SHORT_4000 = held("short", 4000)
HELD_SHORT_5000 = held("short", 5000)
HELD_LONG_90 = held("long", 90)


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


def erp_bars(n, step, *, side=1, recover_last=True, start=400.0):
    out = []
    for i in range(n):
        close = start + 0.5 * i if side == 1 else start + 200.0 - 0.5 * i
        pulled = n - 4 <= i <= n - 2 or (i == n - 1 and not recover_last)
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


def calm_forming_bar(bars):
    out = [dict(b) for b in bars]
    out[-1] = dict(out[-1], h=bars[-2]["c"] + 30.0, l=bars[-2]["c"] - 30.0, c=bars[-2]["c"] - 20.0)
    return out


def fisher_calm_forming_bar(bars):
    out = [dict(b) for b in bars]
    prev = bars[-2]["c"]
    out[-1] = dict(out[-1], o=prev, h=prev + 9.0, l=prev - 0.5, c=prev + 8.0)
    return out


def erp_calm_forming_bar(bars):
    return erp_bars(len(bars), H, recover_last=False)


def scaled_forming_bar(scale):
    def build(bars):
        last = dict(bars[-1])
        last.update(c=last["c"] * scale, h=max(last["o"], last["c"] * scale) * 1.01,
                    l=min(last["o"], last["c"] * scale) * 0.99)
        return bars[:-1] + [last]
    build.__name__ = f"scaled_forming_bar_{scale}"
    return build


def geometry_at(index):
    def mutate(bars):
        out = [dict(b) for b in bars]
        out[index] = dict(out[index], h=out[index]["l"] - 1.0)
        return out
    return mutate


def open_geometry_at_minus_10(bars):
    return [dict(b, o=b["h"] * 2) if i == len(bars) - 10 else b for i, b in enumerate(bars)]


def negative_volume_everywhere(bars):
    return [dict(b, v=-1.0) for b in bars]


def negative_volume_at_minus_10(bars):
    return [dict(b, v=-1.0) if i == len(bars) - 10 else b for i, b in enumerate(bars)]


def reordered(bars):
    out = [dict(b) for b in bars]
    out[-30], out[-29] = out[-29], out[-30]
    return out


def duplicated(bars):
    out = [dict(b) for b in bars]
    out[-30] = dict(out[-31])
    return out


def deleted_at(index):
    def mutate(bars):
        out = [dict(b) for b in bars]
        del out[index]
        return out
    return mutate


def close_above_high_at(index, *, factor=None, offset=None):
    def mutate(bars):
        out = [dict(b) for b in bars]
        h = out[index]["h"]
        out[index] = dict(out[index], c=h * factor if factor is not None else h + offset)
        return out
    return mutate


@dataclasses.dataclass(frozen=True)
class Defect:
    name: str
    mutate: Callable
    expect: Dict[bool, Dict[str, Any]]
    cutoff: Optional[Callable] = None
    check: Optional[Callable] = None


@dataclasses.dataclass(frozen=True)
class Held:
    args: Any
    close_signal: int
    mid: Optional[Callable] = None

    def argv(self, bars):
        return list(self.args(bars) if callable(self.args) else self.args)


@dataclasses.dataclass(frozen=True)
class Case:
    name: str
    ref: str
    prefix: str
    params: Dict[str, Any]
    tf: str
    solo: Callable
    entry: Callable
    tp: Dict[str, Any]
    helds: Tuple[Held, ...]
    defects: Tuple[Defect, ...]
    closed_flags: Tuple[bool, ...] = (True, False)
    default_closes: Optional[Tuple[Dict[str, Any], ...]] = None
    limit: int = 200
    snapshot_id: str = "test/1"
    boundary_cutoff: bool = False
    invalid: Tuple[Tuple[Dict[str, Any], Optional[str]], ...] = ()
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
    cutoffs: Tuple[Tuple[int, int, int, Optional[str]], ...] = ()
    eval_ts: Optional[Tuple[str, str]] = None
    forming: Optional[Dict[str, Any]] = None
    evaluator: Optional[Dict[str, Any]] = None
    opposite_entry_closes: bool = False
    stale_holds: bool = False
    cadence_gap_holds: bool = False
    time_stop_hold: bool = False
    entry_params: Optional[Dict[str, Any]] = None
    eval_tp: Optional[Dict[str, Any]] = None
    batch_entry: Optional[Callable] = None
    batch_check: Optional[Callable] = None
    cutoff_check: Optional[Callable] = None

    def reason(self):
        return f"{self.prefix}_reason"

    def closed_params(self):
        return self.params if self.entry_params is None else self.entry_params

    def admission_tp(self):
        return self.tp if self.eval_tp is None else self.eval_tp


J_CUTOFFS = ((-1, 1, 0, None), (0, 0, 1, None), (1, 0, 1, None))
W_CUTOFFS = ((-1, 1, 0, None), (0, 0, 1, "entry_long"), (1, 0, 1, "entry_long"), (H - 1, 0, 1, "entry_long"),
             (H, 0, 0, "decision_bar_stale"), (5 * H, 0, 0, "decision_bar_stale"))


def _solo4h(bars):
    return envelope(bars, FOUR_H, "BTC|4h")


def _hourly(bars):
    return envelope(bars, H, "BTC|1h", snapshot_id="3600s/1")


def _held_untimed(prefix, label):
    return {"labels": {f"{prefix}_reason": label}, "indicators": {f"{prefix}_valid": 0.0}}


def _geometry_defect(prefix):
    return Defect("geometry", geometry_at(-5), {True: {"held": True, "empty": True},
                                                False: _held_untimed(prefix, "invalid_candle_geometry")})


def _negative_volume_defect(prefix, code, label=None):
    untimed = {"indicators": {f"{prefix}_reason_code": code, f"{prefix}_valid": 0.0}}
    if label:
        untimed["labels"] = {f"{prefix}_reason": label}
    return Defect("negative_volume", negative_volume_everywhere,
                  {True: {"held": True, "hold_reason": "negative volume", "empty": True}, False: untimed})


def _walk_defects(prefix):
    return (
        Defect("negative_volume", negative_volume_at_minus_10,
               {True: {"held": True, "hold_reason": "negative volume"},
                False: _held_untimed(prefix, "negative_volume")}),
        Defect("reordered", reordered,
               {True: {"held": True, "hold_reason": "opens before"}, False: _held_untimed(prefix, "timestamp_order")}),
        Defect("duplicated", duplicated,
               {True: {"held": True, "hold_reason": "share opening time"},
                False: _held_untimed(prefix, "cadence_gap")}),
    )


def _day_gap_and_geometry(prefix):
    return (
        Defect("gap", deleted_at(150), {True: {"held": False, "labels": {f"{prefix}_reason": "cadence_gap"},
                                               "indicators": {f"{prefix}_valid": 0.0}}}),
        Defect("geometry", geometry_at(150), {True: {"held": True, "hold_reason": "high below low"}}),
    )


def _bars_minus_2_close(bars):
    return float(bars[-2]["c"])


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


def _jump_forming(prefix, calm, code, ts, labels=None, exact=False, timeframe="1h"):
    return {"calms": (calm,), "drop_reference": True, "reason_code": (f"{prefix}_reason_code", code),
            "closed_df": (timeframe, NATIVE_CLOSE) if timeframe else (None, None),
            "labels": labels or {}, "labels_exact": exact, "eval_ts": (f"{prefix}_eval_ts_ms", ts),
            "untimed": {"signal": 1, "open_action": "long"}}


def _walk_forming(prefix):
    return {"calms": (scaled_forming_bar(0.5), scaled_forming_bar(1.5)), "drop_reference": True,
            "reason_code": None, "closed_df": None, "labels": {}, "labels_exact": False, "eval_ts": None,
            "untimed": {"signal": 0, "labels": {f"{prefix}_reason": "decision_time_missing"}}}


CASE_LIST = [
    Case(
        name="adaptive_moving_average", prefix="ama", params=AMA_PARAMS, tf="4h",
        ref="backtest/candidates/adaptive_moving_average_1641/REPORT.md",
        solo=lambda: _solo4h(flat_bars(220, FOUR_H, forming_jump=8.0)),
        entry=lambda side=1: flat_bars(260, H, forming_jump=8.0), tp=TP_EMPTY,
        helds=(Held(tuple(HELD_LONG), -1),),
        defects=(_negative_volume_defect("ama", 18.0, "invalid_volume"),),
        invalid=(({**AMA_PARAMS, "fast_period": 12}, None),
                 ({**AMA_PARAMS, "recursion_length": 197, "efficiency_period": 1, "slope_span": 2}, None),
                 ({**AMA_PARAMS, "slope_span": 1.5}, None), ({**AMA_PARAMS, "efficiency_period": True}, None)),
        eval_spec={"closed": False, "indicators": {"ama_recursion_length": 20.0, "ama_valid": 1.0},
                   "labels": {"ama_reason": "entry_long"}, "signal": 1, "open_action": "long"},
        batch_bad=({**AMA_PARAMS, "fast_period": 12}, None), solo_signal=1, batch_signals=(0, 1),
        cutoffs=J_CUTOFFS, eval_ts=("ama_eval_ts_ms", "t"),
        forming=_jump_forming("ama", calm_forming_bar, 0.0, "t", {"ama_reason": "no_cross"}),
        evaluator={"sides": (1,), "bars": lambda side: flat_bars(260, H, forming_jump=8.0), "columns": AMA_COLUMNS,
                   "full": False, "round": False, "timeframe": "1h", "label": "entry_long", "label_match": True,
                   "identity": ama_identity},
        cadence_gap_holds=True,
    ),
    Case(
        name="calendar_conditioned_entry", prefix="cce", params=CCE_SMALL, tf="1h",
        ref="backtest/candidates/calendar_conditioned_entry_1653/REPORT.md",
        solo=lambda: _hourly(cce_bars()), entry=lambda side=1: cce_bars(), tp=TP_3PCT,
        helds=(Held(tuple(HELD_LONG_90), -1, _bars_minus_2_close),),
        defects=(Defect("inconsistent_candle", close_above_high_at(-60, factor=1.01),
                        {True: {"labels": {"cce_reason": "history_inconsistent_candle"}}}),),
        closed_flags=(True,), default_closes=(TIME_STOP,), limit=800, snapshot_id="3600s/1",
        invalid=(({**CCE_SMALL, "refit_bars": 700}, "refit_bars"),), time_stop_hold=True,
    ),
    Case(
        name="camarilla_pivot_reversal", prefix="cpr", params={"expected_interval_ms": H}, tf="1h",
        ref="backtest/candidates/camarilla_pivot_reversal_1669/REPORT.md",
        solo=lambda: _hourly(cpr_bars([100.0, 94.0, 95.0, 96.0])),
        entry=lambda side=1: cpr_bars([100.0, 94.0, 95.0, 96.0]), tp=TP_3PCT,
        helds=(Held(tuple(HELD_LONG_90), -1, lambda bars: 96.0),),
        defects=(Defect("inconsistent_candle", close_above_high_at(187, offset=5.0),
                        {True: {"labels": {"cpr_reason": "inconsistent_candle"}}}),),
        closed_flags=(True,), default_closes=(TP_3PCT,), snapshot_id="3600s/1",
    ),
    Case(
        name="elder_force_pullback", prefix="efp", params={}, tf="1h",
        ref="backtest/candidates/elder_force_pullback_1668/REPORT.md",
        solo=lambda: _hourly(efp_bars(EFP_CROSS_AT)), entry=lambda side=1: efp_bars(EFP_CROSS_AT), tp=TP_LADDER,
        helds=(Held(tuple(HELD_LONG_90), -1, _bars_minus_2_close),),
        defects=_day_gap_and_geometry("efp"), closed_flags=(True,), default_closes=(TP_LADDER,),
        snapshot_id="3600s/1",
        invalid=(({"recursion_length": 40}, "recursion_length"), ({"volume_weighting": True}, "volume_weighting"),
                 ({"trend_period": 190, "slope_span": 9}, "required history")),
    ),
    Case(
        name="elder_ray_pullback", prefix="erp", params=ERP_PARAMS, tf="4h",
        ref="backtest/candidates/elder_ray_pullback_1648/REPORT.md",
        solo=lambda: _solo4h(erp_bars(220, FOUR_H)), entry=lambda side=1: erp_bars(260, H), tp=TP_EMPTY,
        helds=(Held(tuple(HELD_LONG), -1), Held(tuple(HELD_SHORT_5000), 1)),
        defects=(_negative_volume_defect("erp", 16.0),),
        invalid=(({**ERP_PARAMS, "ema_period": 1}, "constraint 'ema_period >= 2' violated"),
                 ({**ERP_PARAMS, "ema_period": 13.5}, "ema_period must be an integer"),
                 ({**ERP_PARAMS, "slope_span": True}, "slope_span must be an integer"),
                 ({**ERP_PARAMS, "pullback_window": 17}, "constraint 'pullback_window <= 16' violated"),
                 ({**ERP_PARAMS, "pullback_window": "8"}, "pullback_window must be an integer"),
                 ({"slope_span": 0}, None)),
        eval_spec={"closed": False, "indicators": {"erp_ema_period": 13.0, "erp_valid": 1.0},
                   "labels": {"erp_reason": "entry_long"}, "signal": 1, "open_action": "long"},
        batch_bad=(dict(ERP_PARAMS, ema_period=40), None), solo_signal=1, batch_signals=(0, 1),
        cutoffs=J_CUTOFFS, eval_ts=("erp_eval_ts_ms", "T"),
        forming=_jump_forming("erp", erp_calm_forming_bar, 2.0, "T", {"erp_reason": "pullback_long"}, exact=True,
                              timeframe=None),
        evaluator={"sides": (1, -1), "bars": lambda side: erp_bars(260, H, side=side), "columns": ERP_COLUMNS,
                   "full": False, "round": False, "timeframe": None, "label": None, "label_match": True,
                   "identity": erp_identity},
    ),
    Case(
        name="fisher_reversal", prefix="fisher", params=FISHER_PARAMS, tf="4h",
        ref="backtest/candidates/fisher_reversal_1646/REPORT.md",
        solo=lambda: _solo4h(fisher_bars(220, FOUR_H)), entry=lambda side=1: fisher_bars(260, H, forming_jump=30.0),
        tp=TP_EMPTY, helds=(Held(tuple(HELD_LONG), -1),), defects=(_negative_volume_defect("fisher", 16.0),),
        eval_spec={"closed": False, "indicators": {"fisher_lookback": 9.0},
                   "indicators_in": {"fisher_valid": (0.0, 1.0)}},
        batch_signals=(0, 1), cutoffs=J_CUTOFFS, eval_ts=("fisher_eval_ts_ms", "T"),
        forming=_jump_forming("fisher", fisher_calm_forming_bar, 0.0, "T", timeframe=None),
        evaluator={"sides": (1,), "bars": lambda side: fisher_bars(260, H, forming_jump=30.0),
                   "columns": FISHER_COLUMNS, "full": False, "round": True, "timeframe": None, "label": None,
                   "label_match": False, "identity": None},
    ),
    WalkCase(
        name="klinger_divergence", prefix="klinger", params=KLINGER_PARAMS, tf="4h", walk=klinger_walk_bars,
        ref="backtest/candidates/klinger_divergence_1662/REPORT.md",
        solo=lambda: _solo4h(klinger_walk_bars(240, FOUR_H)),
        entry=walk_entry("klinger_divergence", klinger_walk_bars), tp=TP_EMPTY,
        helds=(Held(tuple(HELD_LONG), -1), Held(tuple(HELD_SHORT_5000), 1)),
        defects=_walk_defects("klinger"), boundary_cutoff=True,
        invalid=((dict(KLINGER_PARAMS, fast_period=55), "fast_period"),
                 (dict(KLINGER_PARAMS, left_span=1.5), "left_span"),
                 (dict(KLINGER_PARAMS, replay_bars=150), "replay_bars"),
                 (dict(KLINGER_PARAMS, divergence_test="yes"), "divergence_test")),
        eval_spec={"closed": True, "indicators": {"klinger_fast_period": 34.0, "klinger_support_bars": 172.0},
                   "label_present": True},
        peers=("breakout", "on_balance_volume_divergence", "schaff_trend_cycle", "volume_weighted"),
        batch_peer="volume_weighted", batch_bad=(dict(KLINGER_PARAMS, fast_period=55), "fast_period"),
        batch_signals=(1, 0), reconstruct=klinger_reconstruct, cutoffs=W_CUTOFFS, forming=_walk_forming("klinger"),
        opposite_entry_closes=True, stale_holds=True,
    ),
    Case(
        name="mass_index_reversal", prefix="mir", params={}, tf="1h",
        ref="backtest/candidates/mass_index_reversal_1670/REPORT.md",
        solo=lambda: _hourly(mir_bars(MIR_DOWN_AT)), entry=lambda side=1: mir_bars(MIR_DOWN_AT), tp=TP_3PCT,
        helds=(Held(tuple(HELD_LONG_90), -1, _bars_minus_2_close),),
        defects=_day_gap_and_geometry("mir"), closed_flags=(True,), default_closes=(TP_3PCT,),
        snapshot_id="3600s/1",
        invalid=(({"recursion_length": 40}, "recursion_length"), ({"bulge_filter": True}, "bulge_filter"),
                 ({"max_bulge_age": 150}, "required history")),
    ),
    WalkCase(
        name="schaff_trend_cycle", prefix="stc", params=STC_PARAMS, tf="4h",
        ref="backtest/candidates/schaff_trend_cycle_1651/REPORT.md",
        solo=lambda: _solo4h(walk_bars(240, FOUR_H)), entry=walk_entry("schaff_trend_cycle", walk_bars), tp=TP_EMPTY,
        helds=(Held(tuple(HELD_LONG), -1), Held(tuple(HELD_SHORT_5000), 1)),
        defects=_walk_defects("stc"), boundary_cutoff=True,
        invalid=((dict(STC_PARAMS, fast_period=50), "fast_period"), (dict(STC_PARAMS, cycle_length=1.5), "cycle_length"),
                 (dict(STC_PARAMS, lower_threshold=80.0), "lower_threshold"),
                 (dict(STC_PARAMS, smoothing=True), "smoothing")),
        eval_spec={"closed": True, "indicators": {"stc_fast_period": 23.0, "stc_required_history": 193.0},
                   "label_present": True},
        peers=("breakout", "fisher_reversal", "macd", "stoch_rsi"), batch_peer="macd",
        batch_bad=(dict(STC_PARAMS, fast_period=50), "fast_period"), batch_signals=(1, 0),
        reconstruct=stc_reconstruct, cutoffs=W_CUTOFFS, forming=_walk_forming("stc"),
        opposite_entry_closes=True, stale_holds=True,
    ),
    WalkCase(
        name="stochastic_momentum_index", prefix="smi", params=SMI_PARAMS, tf="4h",
        ref="backtest/candidates/stochastic_momentum_index_1663/REPORT.md",
        solo=lambda: _solo4h(walk_bars(240, FOUR_H)), entry=walk_entry("stochastic_momentum_index", walk_bars),
        tp=TP_3PCT, helds=(Held(tuple(HELD_LONG), -1), Held(tuple(HELD_SHORT_5000), 1)),
        defects=(
            Defect("geometry", open_geometry_at_minus_10,
                   {True: _held_untimed("smi", "invalid_candle_geometry"),
                    False: _held_untimed("smi", "invalid_candle_geometry")}),
            Defect("reordered", reordered,
                   {True: {"held": True, "hold_reason": "opens before"},
                    False: _held_untimed("smi", "timestamp_order")}),
        ),
        boundary_cutoff=True,
        invalid=((dict(SMI_PARAMS, smoothing_period=8), "recursion_length"),
                 (dict(SMI_PARAMS, recursion_length=190), "at most 198"),
                 (dict(SMI_PARAMS, range_period=2.5), "range_period"), (dict(SMI_PARAMS, oversold=50.0), "oversold"),
                 (dict(SMI_PARAMS, signal_period=True), "signal_period")),
        eval_spec={"closed": True, "indicators": {"smi_range_period": 10.0, "smi_required_history": 71.0},
                   "label_present": True},
        peers=("mean_reversion_pro", "schaff_trend_cycle", "stoch_rsi"), batch_peer="stoch_rsi",
        batch_bad=(dict(SMI_PARAMS, recursion_length=10), "recursion_length"), batch_signals=(1, 0),
        reconstruct=smi_reconstruct, cutoffs=W_CUTOFFS, forming=_walk_forming("smi"),
        opposite_entry_closes=True, stale_holds=True,
    ),
    Case(
        name="trix_momentum", prefix="trix", params=TRIX_PARAMS, tf="4h",
        ref="backtest/candidates/trix_momentum_1659/REPORT.md",
        solo=lambda: _solo4h(flat_bars(220, FOUR_H, forming_jump=8.0)),
        solo_short=lambda: _solo4h(flat_bars(220, FOUR_H, forming_jump=-8.0)),
        entry=lambda side=1: flat_bars(260, H, forming_jump=8.0), tp=TP_LADDER,
        helds=(Held(tuple(HELD_LONG), -1), Held(tuple(HELD_SHORT_4000), 1)),
        defects=(_geometry_defect("trix"),),
        invalid=(({**TRIX_PARAMS, "recursion_length": 17}, None),
                 ({**TRIX_PARAMS, "recursion_length": 198, "period": 3}, None),
                 ({**TRIX_PARAMS, "signal_period": 20}, None), ({**TRIX_PARAMS, "period": True}, None)),
        eval_spec={"closed": False, "indicators": {"trix_recursion_length": 20.0, "trix_valid": 1.0},
                   "labels": {"trix_reason": "entry_long"}, "signal": 1, "open_action": "long",
                   "short_label": "entry_short"},
        batch_bad=({**TRIX_PARAMS, "recursion_length": 17}, None), solo_signal=1, batch_signals=(0, 1),
        cutoffs=J_CUTOFFS, eval_ts=("trix_eval_ts_ms", "t"),
        forming=_jump_forming("trix", calm_forming_bar, 0.0, "t", {"trix_reason": "no_cross"}),
        evaluator={"sides": (1,), "bars": lambda side: flat_bars(260, H, forming_jump=8.0), "columns": TRIX_COLUMNS,
                   "full": True, "round": False, "timeframe": "1h", "label": "entry_long",
                   "label_match": True, "identity": trix_identity},
        cadence_gap_holds=True, fallback_with_params=False,
    ),
    Case(
        name="true_strength_index", prefix="tsi", params=TSI_PARAMS, tf="4h",
        ref="backtest/candidates/true_strength_index_1667/REPORT.md",
        solo=lambda: _solo4h(tsi_long(220, FOUR_H)), solo_short=lambda: _solo4h(tsi_short(220, FOUR_H)),
        entry=lambda side=1: tsi_long(), tp=TP_LADDER,
        helds=(Held(tuple(HELD_LONG), -1), Held(tuple(HELD_SHORT_4000), 1)),
        defects=(_geometry_defect("tsi"),),
        invalid=(({**TSI_PARAMS, "recursion_length": 17}, None),
                 ({**TSI_PARAMS, "recursion_length": 199, "slow_period": 3}, None),
                 ({**TSI_PARAMS, "signal_period": 18}, None), ({**TSI_PARAMS, "slow_period": True}, None),
                 ({**TSI_PARAMS, "fast_period": 3}, None)),
        eval_spec={"closed": False,
                   "indicators": {"tsi_recursion_length": 18.0, "tsi_required_history": 19.0, "tsi_valid": 1.0},
                   "labels": {"tsi_reason": "entry_long"}, "signal": 1, "open_action": "long",
                   "short_label": "entry_short"},
        batch_bad=({**TSI_PARAMS, "recursion_length": 17}, None), solo_signal=1, batch_signals=(0, 1),
        cutoffs=J_CUTOFFS, eval_ts=("tsi_eval_ts_ms", "t"),
        forming=_jump_forming("tsi", calm_forming_bar, 0.0, "t", {"tsi_reason": "no_cross"}),
        evaluator={"sides": (1,), "bars": lambda side: tsi_long(), "columns": TSI_COLUMNS, "full": True,
                   "round": False, "timeframe": "1h", "label": "entry_long", "label_match": True,
                   "identity": tsi_identity},
        cadence_gap_holds=True, fallback_with_params=False,
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


def mark_held(side):
    def args(bars):
        return held(side, bars[-1]["c"] * (0.9 if side == "long" else 1.1))
    return args


def mark(bars):
    return bars[-1]["c"]


MARK_HELDS = (Held(mark_held("long"), -1, mark), Held(mark_held("short"), 1, mark))


OBV_PARAMS = {"left_span": 2, "right_span": 2, "min_separation": 3, "max_separation": 20,
              "volume_threshold": 0.1, "setup_expiry": 5, "volume_test": True}
OBV_CLOSES = ([100.0] * 103 + [100.0, 97.0, 91.0, 97.0, 100.0, 100.0, 100.0]
              + [99.0, 98.0, 96.0, 98.0, 100.0, 101.0, 100.0, 98.0, 96.0, 95.0, 97.0, 98.0, 98.5, 100.0]
              + [100.5, 101.0, 101.5, 102.0])
OBV_ENTRY = 123
TP_2PCT = {"name": "tiered_tp_pct", "params": {"tp_tiers": [{"profit_pct": 0.02, "close_fraction": 1.0}]}}


def obv_bars(last, *, bad_volume_at=None):
    out = []
    prev = None
    for i, c in enumerate(OBV_CLOSES[:last + 1]):
        v = 5.0 if prev is None or c == prev else (10.0 if c > prev else 1.0)
        if bad_volume_at is not None and i == bad_volume_at:
            v = -1.0
        out.append(hour_bar(i, c, c + 1.0, c - 1.0, c, v))
        prev = c
    return out


def obv_closed_entry(case, bars, offset, out):
    if offset != 5_000:
        return
    assert out["open_action"] == "long"
    assert out["indicators"]["obvd_long_reclaim"] == 99.0
    untimed = check(case, bars, closed=False)
    assert out["indicators"]["obvd_long_divergence"] == untimed["indicators"]["obvd_long_divergence"]
    direct = strategies().apply_strategy(case.name, frame_df(rows_of(bars)), case.closed_params())
    assert int(direct["signal"].iloc[-1]) == 1
    assert out["indicators"]["obvd_support_start_ts_ms"] == float(direct["obvd_support_start_ts_ms"].iloc[-1])


def cmo_bars(closes):
    out = []
    prev = closes[0]
    for i, c in enumerate(closes):
        out.append(hour_bar(i, prev, max(prev, c) + 0.5, min(prev, c) - 0.5, c, 1000.0 + i))
        prev = c
    return out


def cmo_gapped(_bars):
    bars = cmo_bars([100.0] * 60 + [101.0, 102.0, 103.0])
    return bars[:-3] + [dict(b, t=b["t"] + H, T=b["T"] + H) for b in bars[-3:]]


def cmo_held(bars):
    return held("long", 100 if bars[-1]["c"] >= 103.0 else 50)


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


def mfi_calm_forming_bar(bars):
    out = [dict(b) for b in bars]
    out[-1] = set_bar(out[-1], bars[-2]["c"], 1.0)
    return out


def mfi_volumes(case, bars, enabled, untimed):
    assert enabled["indicators"]["mfi_volume"] == bars[-2]["v"]
    assert untimed["indicators"]["mfi_volume"] == bars[-1]["v"]


EOM_PARAMS = {"eom_period": 3, "breakout_window": 4}
EOM_LABELS = ("no_breakout", "breakout_continuation", "entry_long", "entry_short", "indicator_disagrees",
              "zero_indicator")


def eom_bars(side=1, *, zero_volume_at=None):
    rows = [(100.0, 101.0, 99.0, 100.0, 1000.0)] * 60 + [(100.0, 104.0, 100.0, 103.5, 1000.0)]
    out = []
    for i, (o, h, low, c, v) in enumerate(rows):
        out.append(hour_bar(i, o, h, low, c, 0.0 if i == zero_volume_at else v))
    return out


def eom_closed_entry(case, bars, enabled, untimed):
    closed = check(case, bars, closed=True, cutoff=bars[-1]["t"] + H + 5_000)
    assert closed["closed_bar_decision"]["forming_rows_dropped"] == 0
    assert closed["signal"] == 1 and closed["open_action"] == "long"
    assert closed["indicators"]["eom_eval_ts_ms"] == bars[-1]["t"]
    assert closed["indicator_labels"]["eom_reason"] == "entry_long"
    df = frame_df(rows_of(bars))
    df.attrs["bar_interval_ms"] = H
    direct = strategies().apply_strategy(case.name, df, case.closed_params())
    assert int(direct["signal"].iloc[-1]) == 1
    assert closed["indicators"]["eom_smoothed"] == float(direct["eom_smoothed"].iloc[-1])
    assert closed["indicators"]["eom_raw"] == float(direct["eom_raw"].iloc[-1])
    for values in ((103.5, 140.0, 103.0, 139.0, 50.0), (103.5, 104.0, 60.0, 61.0, 0.0),
                   (103.5, 103.6, 103.4, 103.5, 9_000.0)):
        extended = bars + [hour_bar(len(bars), *values)]
        out = check(case, extended, closed=True, cutoff=extended[-1]["t"] + H // 3)
        assert out["closed_bar_decision"]["forming_rows_dropped"] == 1, values
        assert out["signal"] == closed["signal"] == 1, values
        assert out["indicators"] == closed["indicators"], values
        assert out["indicator_labels"] == closed["indicator_labels"], values


AROON_PARAMS = {"period": 25, "strength": 70.0}


def aroon_bars(n, *, spike_at=None, flat=False, bad_at=None):
    out = []
    price = 200.0
    for i in range(n):
        if not flat:
            price -= 0.5
        o, h, low, c = price + 0.2, price + 0.4, price - 0.4, price
        if spike_at is not None and i == spike_at:
            o, h, low, c = price + 0.3, price + 150.0, price + 0.2, price + 149.0
        if flat:
            o = h = low = c = price
        if bad_at is not None and i == bad_at:
            h, low = low, h
        out.append(hour_bar(i, o, h, low, c, 1000.0))
    return out


RCR_ADMISSION = {"fit_window": 50, "band_multiplier": 2.0, "slope_ceiling": 0.10, "min_residual_std": 0.0}
RCR_PARAMS = dict(RCR_ADMISSION, fit_window=8)
RCR_PATTERN = (0.5, -0.5, -0.5, 0.5)


def rcr_bars(n, *, closes=None, flat=False, bad_at=None):
    out = []
    for i in range(n):
        c = 100.0 if flat else 100.0 + RCR_PATTERN[i % 4]
        if closes and i in closes:
            c = closes[i]
        o, h, low = c, c + 0.6, c - 0.6
        if bad_at is not None and i == bad_at:
            c = o = low = 0.0
        out.append(hour_bar(i, o, h, low, c, 1000.0))
    return out


def rcr_untimed_row(case, bars, enabled, untimed):
    assert untimed["indicators"]["rcr_row_ts_ms"] == bars[-1]["T"]


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


def wr_long_cross(n):
    bars = wr_zigzag(n - 6)
    px = bars[-1][3]
    for _ in range(5):
        px -= 1.5
        bars.append((px + 1.5, px + 1.5, px, px))
    bars.append((px, px + 3.0, px, px + 2.9))
    return bars


def wr_bars(tuples):
    return [hour_bar(i, o, h, low, c, 1000.0 + i) for i, (o, h, low, c) in enumerate(tuples)]


def wr_solo():
    bars = wr_bars(wr_zigzag(80))
    return {"v": 2, "market": {"version": 4, "snapshot_id": "test/1", "generation": 1, "sealed_at_ms": T0,
                               "frames": {"BTC|1h": frame(bars, H)}, "feed_complete": True}}


def wr_defect(name, mutate, reason, code):
    def bars(_bars):
        return wr_bars(mutate(wr_long_cross(120)))

    def direct(case, bars):
        res = strategies().apply_strategy(case.name, frame_df(rows_of(bars)), case.closed_params())
        assert res["wr_reason"].iloc[-1] == reason
        assert int(res["signal"].iloc[-1]) == 0
    return Defect(name, bars, {False: {"indicators": {"wr_reason_code": code}, "absent": ("wr",)}}, check=direct)


WR_DEFECTS = (
    wr_defect("nonfinite_input", lambda b: b[:-3] + [(b[-3][0], b[-3][1], b[-3][2], float("nan"))] + b[-2:],
              "nonfinite_input", 11),
    wr_defect("inconsistent_hlc", lambda b: b[:-3] + [(b[-3][0], b[-3][2] - 1.0, b[-3][1], b[-3][3])] + b[-2:],
              "inconsistent_hlc", 13),
    wr_defect("nonpositive_price", lambda b: b[:-2] + [(0.0, 0.0, 0.0, 0.0)] + b[-1:], "nonpositive_price", 12),
    wr_defect("zero_range_window", lambda b: b[:-20] + [(100.0, 100.0, 100.0, 100.0)] * 20, "zero_range_window", 16),
)


def _plain_forming(**kw):
    out = {"calms": (), "drop_reference": True, "reason_code": None, "closed_df": (None, None), "labels": {},
           "labels_exact": False, "eval_ts": None}
    out.update(kw)
    return out


FOLDED_CASES = [
    Case(
        name="aroon_trend", prefix="aroon", params=AROON_PARAMS, tf="4h",
        ref="backtest/candidates/aroon_trend_1643/REPORT.md", solo=legacy_solo,
        entry=lambda side=1: aroon_bars(61, spike_at=59), tp=TP_3PCT, default_closes=(TP_3PCT,), helds=MARK_HELDS,
        defects=(
            Defect("flat", lambda bars: aroon_bars(61, flat=True),
                   {closed: {"indicators": {"aroon_reason_code": 14.0, "aroon_valid": 0.0}} for closed in (True, False)}),
            Defect("bad", lambda bars: aroon_bars(61, bad_at=59),
                   {True: {"held": True, "empty": True},
                    False: {"indicators": {"aroon_reason_code": 12.0, "aroon_valid": 0.0}}}),
        ),
        invalid=(({"period": True}, "period"), ({"period": 2.5}, "period must be an integer"), ({"period": 1}, "period"),
                 ({"period": 199}, "period"), ({"period": "25"}, "period"),
                 ({"strength": None}, "strength must be a number"), ({"strength": 100.5}, "strength"),
                 ({"strength": "70"}, "strength must be a number")),
        eval_spec={"closed": False, "indicators": {"aroon_valid": 1.0},
                   "indicators_in": {"aroon_reason_code": (-1, 0, 1, 2)}},
        batch_peer="breakout", batch_bad=({"period": 2.5}, "period must be an integer"), batch_signals=(1, -1),
        forming=_plain_forming(bars=lambda side=1: aroon_bars(60, spike_at=59),
                               reason_code=("aroon_reason_code", 0.0), closed_indicators={"aroon_down": 100.0},
                               untimed={"signal": 1, "indicators": {"aroon_reason_code": 1.0}}),
    ),
    Case(
        name="chande_momentum", prefix="cmo", params={}, tf="1h",
        ref="backtest/candidates/chande_momentum_1661/REPORT.md", solo=legacy_hourly_solo,
        entry=lambda side=1: cmo_bars([100.0] * 220 + [101.0, 96.0]), tp=TP_3PCT,
        helds=(Held(cmo_held, -1, lambda bars: 104.0),),
        defects=(Defect("gap", cmo_gapped,
                        {True: {"held": False, "indicators": {"cmo_valid": 0.0, "cmo_reason_code": 16.0}},
                         False: {"indicators": {"cmo_valid": 0.0, "cmo_reason_code": 16.0}}}),),
        eval_spec={"closed": False, "indicators": {"cmo_valid": 1.0},
                   "present": ("cmo", "cmo_prev", "cmo_gain_sum", "cmo_loss_sum", "cmo_close", "cmo_eval_ts_ms",
                               "cmo_valid", "cmo_reason_code")},
        batch_signals=(1, -1),
    ),
    Case(
        name="ease_of_movement_breakout", prefix="eom", params={}, entry_params=EOM_PARAMS, tf="4h",
        ref="backtest/candidates/ease_of_movement_1671/REPORT.md", solo=legacy_solo, entry=eom_bars,
        tp=TP_2PCT, eval_tp={"name": "tiered_tp_atr"}, helds=(Held(tuple(HELD_LONG_90), -1),),
        defects=(Defect("zero_volume", lambda bars: eom_bars(zero_volume_at=60),
                        {closed: {"labels": {"eom_reason": "invalid_volume"}} for closed in (True, False)},
                        cutoff=lambda bars: bars[-1]["t"] + H + 5_000),),
        eval_spec={"closed": False, "indicators": {"eom_interval_ms": 14_400_000.0, "eom_valid": 1.0},
                   "absent": ("eom_eval_ts_ms",), "labels_in": {"eom_reason": EOM_LABELS}},
        batch_peer="breakout", batch_signals=(0, 1),
        forming=_plain_forming(labels={"eom_reason": "no_breakout"}, eval_ts=("eom_eval_ts_ms", "t"),
                               untimed={"signal": 1, "open_action": "long", "absent": ("eom_eval_ts_ms",)},
                               check=eom_closed_entry),
    ),
    Case(
        name="money_flow_index_reversal", prefix="mfi", params=MFI_ADMISSION, entry_params=MFI_PARAMS, tf="4h",
        ref="backtest/candidates/money_flow_index_reversal_1658/REPORT.md", solo=legacy_solo, entry=mfi_bars,
        tp=TP_EMPTY, eval_tp={"name": "tiered_tp_atr", "params": {}}, helds=(Held(tuple(HELD_LONG), -1),),
        defects=(_negative_volume_defect("mfi", 15.0),),
        eval_spec={"closed": False, "indicators": {"mfi_lookback": 14.0}, "indicators_in": {"mfi_valid": (0.0, 1.0)}},
        batch_signals=(0, 1),
        forming=_plain_forming(calms=(mfi_calm_forming_bar,), reason_code=("mfi_reason_code", 0.0),
                               closed_columns=("mfi_value", "mfi_prev_value", "mfi_positive_total"),
                               eval_ts=("mfi_eval_ts_ms", "T"), untimed={"signal": 1, "open_action": "long"},
                               check=mfi_volumes),
    ),
    Case(
        name="on_balance_volume_divergence", prefix="obvd", params={}, entry_params=OBV_PARAMS, tf="4h",
        ref="backtest/candidates/on_balance_volume_divergence_1657/REPORT.md", solo=legacy_solo,
        entry=lambda side=1: obv_bars(OBV_ENTRY), tp=TP_2PCT, eval_tp={"name": "tiered_tp_pct"},
        helds=(Held(tuple(HELD_LONG_90), -1),),
        defects=(Defect("negative_volume", lambda bars: obv_bars(OBV_ENTRY + 1, bad_volume_at=OBV_ENTRY),
                        {True: {"held": True, "hold_reason": "volume", "empty": True},
                         False: {"indicators": {"obvd_reason_code": 16.0}}}),),
        eval_spec={"closed": False, "indicators": {"obvd_support_bars": 59.0}},
        batch_peer="breakout", batch_signals=(0, 1),
        cutoffs=((-1, 1, 0, None), (0, 0, 1, None), (1, 0, 1, None), (5_000, 0, 1, None)),
        cutoff_check=obv_closed_entry,
        forming=_plain_forming(reason_code=("obvd_reason_code", 3.0), untimed={"signal": 1, "open_action": "long"}),
    ),
    Case(
        name="regression_channel_reversion", prefix="rcr", params=RCR_ADMISSION, entry_params=RCR_PARAMS, tf="4h",
        ref="backtest/candidates/regression_channel_reversion_1644/REPORT.md", solo=legacy_solo,
        entry=lambda side=1: rcr_bars(40, closes={38: 97.5, 39: 100.0}), tp=TP_3PCT, default_closes=(TP_3PCT,),
        helds=MARK_HELDS,
        defects=(
            Defect("flat", lambda bars: rcr_bars(41, flat=True),
                   {closed: {"indicators": {"rcr_reason_code": 17.0, "rcr_valid": 0.0},
                             "labels": {"rcr_reason": "degenerate_fit"}} for closed in (True, False)}),
            Defect("bad", lambda bars: rcr_bars(41, bad_at=38),
                   {True: {"held": True, "empty": True},
                    False: {"indicators": {"rcr_reason_code": 16.0, "rcr_valid": 0.0},
                            "labels": {"rcr_reason": "nonpositive_price"}}}),
        ),
        invalid=(({"fit_window": True}, "fit_window"), ({"fit_window": 2.5}, "fit_window must be an integer"),
                 ({"fit_window": 1}, "fit_window"), ({"fit_window": 198}, "fit_window"),
                 ({"fit_window": "50"}, "fit_window must be an integer"), ({"band_multiplier": 0}, "band_multiplier"),
                 ({"band_multiplier": None}, "band_multiplier must be a number"),
                 ({"slope_ceiling": -0.1}, "slope_ceiling"),
                 ({"min_residual_std": "1"}, "min_residual_std must be a number")),
        eval_spec={"closed": False, "indicators": {"rcr_fit_window": 50.0, "rcr_interval_ms": 14_400_000.0},
                   "indicators_in": {"rcr_reason_code": (-1, 0, 1, 2, 3, 17)}, "label_present": True},
        batch_peer="breakout", batch_bad=({"fit_window": 2.5}, "fit_window must be an integer"),
        batch_entry=lambda side=1: rcr_bars(40, closes={37: 97.5, 38: 100.0}), batch_signals=(1, 0),
        batch_check=rcr_batch_labels, cutoffs=((-1, 1, 0, None), (0, 0, 1, None)), eval_ts=("rcr_eval_ts_ms", "t"),
        forming=_plain_forming(labels={"rcr_reason": "no_reentry"}, eval_ts=("rcr_eval_ts_ms", "t"),
                               untimed={"signal": 1, "labels": {"rcr_reason": "entry_long"},
                                        "absent": ("rcr_eval_ts_ms",)},
                               check=rcr_untimed_row),
    ),
    Case(
        name="ultimate_oscillator", prefix="uo", params={}, tf="4h",
        ref="backtest/candidates/ultimate_oscillator_1664/REPORT.md", solo=legacy_solo,
        entry=lambda side=1: flat_bars(260, H), tp=TP_3PCT, helds=(Held(tuple(HELD_LONG), -1),), defects=(),
        eval_spec={"closed": False, "indicators": {}, "present": ("uo_reason_code",)},
    ),
    Case(
        name="williams_r_reversal", prefix="wr", params={}, tf="1h",
        ref="backtest/candidates/williams_r_reversal_1650/REPORT.md", solo=wr_solo,
        entry=lambda side=1: wr_bars(wr_long_cross(130)), tp=WR_TP,
        helds=(Held(tuple(held("long", 50) + ["--position-entry-atr=2"]), -1, lambda bars: 100.0),),
        defects=WR_DEFECTS, closed_flags=(False,),
        invalid=(({"lookback": 2.5}, None), ({"lookback": "14"}, None), ({"lookback": True}, None),
                 ({"oversold": -10.0, "overbought": -20.0}, None), ({"overbought": 1.0}, None)),
        eval_spec={"closed": False, "indicators": {}}, batch_bad=({"lookback": 7.5}, None),
        cutoffs=((-1, 1, 0, None), (0, 0, 1, None), (1, 0, 1, None)),
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


def case_closed_params(attr=None):
    out = []
    for name in CASE_NAMES:
        case = CASES[name]
        if attr is not None and not getattr(case, attr):
            continue
        for closed in case.closed_flags if attr == "defects" else (True, False):
            out.append(pytest.param(name, closed, id=f"{name}-{'closed' if closed else 'untimed'}"))
    return out


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


def decision_cutoff(case, bars):
    return boundary(bars) if case.boundary_cutoff else None


def eth_envelope(case):
    out = case.solo()
    key = f"BTC|{case.tf}"
    frames = out["market"]["frames"]
    return {"v": 2, "market": dict(out["market"], frames={f"ETH|{case.tf}": frames[key]})}


def assert_expect(out, expect, where):
    meta = out.get("closed_bar_decision") or {}
    if "held" in expect:
        assert meta.get("held") is expect["held"], where
    if expect.get("hold_reason"):
        assert expect["hold_reason"] in meta["hold_reason"], where
    if expect.get("empty"):
        assert out["indicators"] == {}, where
    for key, value in expect.get("labels", {}).items():
        assert out["indicator_labels"][key] == value, (where, key)
    for key, value in expect.get("indicators", {}).items():
        assert out["indicators"][key] == value, (where, key)
    for key in expect.get("absent", ()):
        assert key not in out["indicators"], (where, key)


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


@pytest.mark.parametrize("name", CASE_NAMES)
def test_effective_open_and_close_fallback_are_gated(name):
    case = CASES[name]
    solo = case.solo()
    open_refs = json.dumps({"open": {"name": name, "params": case.params}})
    opened = f"open strategy '{name}' is edge_status=no_edge"
    closed = f"close strategy '{name}' is edge_status=no_edge"
    sealed = ["--market-stdin"]
    source = f"source {strategies()._registry.STRATEGIES[name]['edge_source']}"
    variants = [
        ("positional_missing_fetch", [name, "BTC", case.tf], None, (opened, source, case.ref, "missing mode")),
        ("fallback_missing_fetch", ["breakout", "BTC", case.tf, "--strategy-refs", fallback_refs(case)], None,
         (closed, "missing mode")),
        ("fallback_live_fetch", ["breakout", "BTC", case.tf, "--strategy-refs", fallback_refs(case), "--mode=live"],
         None, (closed, "explicit live mode")),
        ("open_live", ["breakout", "BTC", case.tf, "--strategy-refs", open_refs, *sealed, "--mode=live"], solo,
         (opened, "explicit live mode")),
        ("open_missing", ["breakout", "BTC", case.tf, "--strategy-refs", open_refs, *sealed], solo,
         (opened, "missing mode")),
        ("fallback_live", ["breakout", "BTC", case.tf, "--strategy-refs", fallback_refs(case), *sealed, "--mode=live"],
         solo, (closed, "explicit live mode")),
        ("fallback_missing", ["breakout", "BTC", case.tf, "--strategy-refs", fallback_refs(case), *sealed], solo,
         (closed, "missing mode")),
        ("fallback_bad_ack", ["breakout", "BTC", case.tf, "--strategy-refs", fallback_refs(case), *sealed,
                              "--mode=live", "--allow-no-edge=yes"], solo, (closed, "invalid acknowledgement")),
        ("fallback_repeated_mode", ["breakout", "BTC", case.tf, "--strategy-refs", fallback_refs(case), *sealed,
                                    "--mode=paper", "--mode=paper"], solo, (closed, "invalid mode input")),
    ]
    for variant, argv, stdin, needles in variants:
        proc = run(argv, stdin)
        assert proc.returncode == 1, (variant, proc.stderr[-2000:])
        out = payload(proc)
        assert out["signal"] == 0, variant
        if argv[0] == name:
            assert out["strategy"] == name, variant
        for needle in needles:
            assert needle in out["error"], (variant, needle, out["error"])
        if stdin is None:
            assert "Fetching" not in proc.stderr, variant


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


@pytest.mark.parametrize("name", cases_with("invalid"))
def test_invalid_parameters_fail_preflight_before_market_input_is_read(name):
    case = CASES[name]
    for params, needle in case.invalid:
        where = (json.dumps(params, sort_keys=True), needle)
        strategy_refs = refs(case, params=params)
        forms = [
            ("open_fetch", [name, "BTC", case.tf, "--mode=paper", "--strategy-refs", strategy_refs], None, "open"),
            ("params_flag_fetch", [name, "BTC", case.tf, "--mode=paper", "--params", json.dumps(params)], None, "open"),
            ("open_sealed", [name, "BTC", case.tf, "--mode=paper", "--strategy-refs", strategy_refs,
                             "--market-stdin"], eth_envelope(case), "open"),
            ("fallback_fetch", ["breakout", "BTC", case.tf, "--mode=paper", "--strategy-refs",
                                fallback_refs(case, params)], None, "close"),
            ("fallback_sealed", ["breakout", "BTC", case.tf, "--mode=paper", "--strategy-refs",
                                 fallback_refs(case, params), "--market-stdin"], eth_envelope(case), "close"),
        ]
        for form, argv, stdin, role in forms:
            proc = run(argv, stdin)
            assert proc.returncode == 1, (form, where, proc.stderr[-2000:])
            out = payload(proc)
            assert out["signal"] == 0, (form, where)
            assert f"{role} strategy '{name}' has invalid parameters" in out["error"], (form, where, out["error"])
            if needle:
                assert needle in out["error"], (form, where, out["error"])
            if form.endswith("fetch"):
                assert "Fetching" not in proc.stderr, (form, where)
            if form.endswith("sealed"):
                assert f"BTC|{case.tf}" not in out["error"], (form, where)


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
        {"id": "paper-space", "strategy": name, "mode": "paper", "mode_args": ["--mode", "paper"],
         "strategy_refs": strategy_refs},
        {"id": "acked", "strategy": name, "mode": "live", "mode_args": ["--mode=live"], "allow_no_edge": True,
         "strategy_refs": strategy_refs},
        {"id": "unacked-live", "strategy": name, "mode": "live", "mode_args": ["--mode=live"],
         "strategy_refs": strategy_refs},
        {"id": "false-live", "strategy": name, "mode": "live", "mode_args": ["--mode=live"], "allow_no_edge": False,
         "strategy_refs": strategy_refs},
        {"id": "missing", "strategy": name, "mode": "paper", "strategy_refs": strategy_refs},
        {"id": "repeated", "strategy": name, "mode": "paper", "mode_args": ["--mode=paper", "--mode=paper"],
         "strategy_refs": strategy_refs},
        {"id": "repeated-mixed", "strategy": name, "mode": "paper", "mode_args": ["--mode=paper", "--mode=live"],
         "strategy_refs": strategy_refs},
        dict(keyed, id="keys-list-paper", close_strategies=[name], mode="paper", mode_args=["--mode=paper"]),
        dict(keyed, id="keys-list-acked", close_strategies=[name], mode="live", mode_args=["--mode=live"],
             allow_no_edge=True),
        dict(keyed, id="keys-list-live", close_strategies=[name], mode="live", mode_args=["--mode=live"]),
        dict(keyed, id="keys-list-missing", close_strategies=[name], mode="paper"),
        dict(keyed, id="keys-str-paper", close_strategies=name, mode="paper", mode_args=["--mode=paper"]),
        dict(keyed, id="keys-str-live", close_strategies=name, mode="live", mode_args=["--mode=live"]),
        {"id": "fallback-live", "strategy": "breakout", "mode": "live", "mode_args": ["--mode=live"],
         "strategy_refs": fallback},
        {"id": "fallback-missing", "strategy": "breakout", "mode": "paper", "strategy_refs": fallback},
        {"id": "fallback-paper", "strategy": "breakout", "mode": "paper", "mode_args": ["--mode=paper"],
         "strategy_refs": fallback},
        {"id": "fallback-acked", "strategy": "breakout", "mode": "live", "mode_args": ["--mode=live"],
         "allow_no_edge": True, "strategy_refs": fallback},
    ]
    oks = ["paper", "paper-space", "acked", "fallback-paper", "fallback-acked", "keys-list-paper", "keys-list-acked",
           "keys-str-paper"]
    closed_gate = f"close strategy '{name}' is edge_status=no_edge"
    refused = {"unacked-live": "explicit live mode", "false-live": "explicit live mode", "missing": "missing mode",
               "repeated": "invalid mode input", "repeated-mixed": "invalid mode input",
               "fallback-live": closed_gate, "fallback-missing": "missing mode", "keys-list-live": closed_gate,
               "keys-list-missing": "missing mode", "keys-str-live": closed_gate}
    if case.batch_bad is not None:
        bad_params, bad_needle = case.batch_bad
        slots.append({"id": "bad-params", "strategy": name, "mode": "paper", "mode_args": ["--mode=paper"],
                      "strategy_refs": json.loads(refs(case, closes=[case.admission_tp()], params=bad_params))})
        slots.append({"id": "bad-fallback-params", "strategy": "breakout", "mode": "paper",
                      "mode_args": ["--mode=paper"], "strategy_refs": json.loads(fallback_refs(case, bad_params))})
        refused["bad-params"] = f"open strategy '{name}' has invalid parameters"
        refused["bad-fallback-params"] = f"close strategy '{name}' has invalid parameters"
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
    if case.batch_bad is not None and case.batch_bad[1]:
        assert case.batch_bad[1] in results["bad-params"]["error"]
    for slot_id in ("fallback-live", "keys-list-live", "keys-str-live"):
        assert "explicit live mode" in results[slot_id]["error"], slot_id
    for slot_id in ("keys-list-paper", "keys-list-acked", "keys-str-paper"):
        assert results[slot_id]["close_strategies"] == [name], slot_id
    solo = payload(run([name, "BTC", case.tf, "--mode=paper", "--ohlcv-limit", str(case.limit), "--strategy-refs",
                        json.dumps(strategy_refs), "--market-stdin"], stdin))
    if case.solo_signal is not None:
        assert solo["signal"] == case.solo_signal
    for key in ("signal", "open_action", "close_fraction", "indicators", "indicator_labels"):
        assert results["paper"].get(key) == solo.get(key), key
        assert results["acked"].get(key) == solo.get(key), key
    bad_ack = {"id": "bad-ack", "strategy": name, "mode": "live", "mode_args": ["--mode=live"],
               "allow_no_edge": "yes", "strategy_refs": json.loads(refs(case))}
    paper = {"id": "paper", "strategy": name, "mode": "paper", "mode_args": ["--mode=paper"],
             "strategy_refs": json.loads(refs(case))}
    for malformed_slots in ([bad_ack], [paper, bad_ack]):
        proc = batch(case, malformed_slots, stdin)
        where = [s["id"] for s in malformed_slots]
        assert proc.returncode == 1, where
        out = payload(proc)
        assert "JSON boolean" in json.dumps(out), where
        assert out["results"] == [] and out["error_scope"] == "shared_state", where
        assert "allow_no_edge must be a JSON boolean" in out["error"], where
        assert all(r.get("signal", 0) == 0 for r in out["results"]), where


@pytest.mark.parametrize("name", cases_with("cutoffs"))
def test_cutoff_boundaries_select_and_age_the_decision_bar(name):
    case = CASES[name]
    bars = case.entry(1)
    for offset, dropped, signal, reason in case.cutoffs:
        out = check(case, bars, closed=True, cutoff=boundary(bars) + offset)
        assert out["closed_bar_decision"]["forming_rows_dropped"] == dropped, offset
        assert out["signal"] == signal, offset
        if reason:
            assert out["indicator_labels"][case.reason()] == reason, offset
        if offset == 0 and case.eval_ts:
            key, stamp = case.eval_ts
            assert out["indicators"][key] == float(bars[-1][stamp]), offset
        if case.cutoff_check is not None:
            case.cutoff_check(case, bars, offset, out)


@pytest.mark.parametrize("name", cases_with("forming"))
def test_a_forming_bar_cannot_create_a_closed_bar_entry(name):
    case = CASES[name]
    spec = case.forming
    bars = spec.get("bars", case.entry)(1)
    cutoff = bars[-1]["t"] + H // 2
    enabled = check(case, bars, closed=True, cutoff=cutoff)
    meta = enabled["closed_bar_decision"]
    assert meta["forming_rows_dropped"] == 1 and meta["held"] is False
    assert enabled["signal"] == 0 and enabled["open_action"] == "none"
    for key, value in spec.get("closed_indicators", {}).items():
        assert enabled["indicators"][key] == value, key
    references = [build(bars) for build in spec["calms"]]
    if spec["drop_reference"]:
        references.append(bars[:-1])
    for index, other_bars in enumerate(references):
        other = check(case, other_bars, closed=True, cutoff=cutoff)
        assert other["signal"] == enabled["signal"], index
        assert other["indicators"] == enabled["indicators"], index
        assert other.get("indicator_labels") == enabled.get("indicator_labels"), index
        assert other["closed_bar_decision"]["input_sha256"] == meta["input_sha256"], index
    if spec["labels_exact"]:
        assert enabled["indicator_labels"] == spec["labels"]
    for key, value in spec["labels"].items():
        assert enabled["indicator_labels"][key] == value, key
    if spec["reason_code"]:
        key, code = spec["reason_code"]
        timeframe, kind = spec["closed_df"]
        closed_df = strategies().apply_strategy(name, frame_df(rows_of(bars[:-1]), timeframe, kind),
                                                case.closed_params())
        assert enabled["indicators"][key] == float(closed_df[key].iloc[-1]) == code
        for col in spec.get("closed_columns", ()):
            assert enabled["indicators"][col] == float(closed_df[col].iloc[-1]), col
    if spec["eval_ts"]:
        key, stamp = spec["eval_ts"]
        assert enabled["indicators"][key] == float(bars[-2][stamp])
    untimed = check(case, bars, closed=False)
    expect = spec["untimed"]
    assert untimed["signal"] == expect["signal"]
    assert "closed_bar_decision" not in untimed
    if "open_action" in expect:
        assert untimed["open_action"] == expect["open_action"]
    for key, value in expect.get("labels", {}).items():
        assert untimed["indicator_labels"][key] == value, key
    for key, value in expect.get("indicators", {}).items():
        assert untimed["indicators"][key] == value, key
    for key in expect.get("absent", ()):
        assert key not in untimed["indicators"], key
    if spec.get("check"):
        spec["check"](case, bars, enabled, untimed)


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
        held_args = case.helds[held_index].argv(bars)
        same_side = case.helds[1 - held_index].argv(bars)
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


@pytest.mark.parametrize("name,closed", case_closed_params("defects"))
def test_invalid_entry_data_holds_the_entry_and_keeps_protection(name, closed):
    case = CASES[name]
    strategy_refs = erefs(case, closes=[case.tp])
    for defect in case.defects:
        bars = defect.mutate(case.entry(1))
        cutoff = defect.cutoff(bars) if defect.cutoff else decision_cutoff(case, bars)
        expect = defect.expect[closed]
        if defect.check is not None:
            defect.check(case, bars)
        for position in case.helds:
            args = position.argv(bars)
            where = (defect.name, args[1])
            mid = position.mid(bars) if position.mid else None
            out = check(case, bars, closed=closed, extra=args, cutoff=cutoff, mid=mid, strategy_refs=strategy_refs)
            assert_expect(out, expect, where)
            assert out["open_action"] == "none", where
            assert out["close_strategy"] == case.tp["name"] and out["close_fraction"] == 1.0, where
            assert out["signal"] == position.close_signal, where
            assert out["price"] == (bars[-1]["c"] if mid is None else mid), where
        flat = check(case, bars, closed=closed, cutoff=cutoff, strategy_refs=strategy_refs)
        assert_expect(flat, expect, (defect.name, "flat"))
        assert flat["signal"] == 0 and flat["open_action"] == "none", (defect.name, "flat")
        if case.time_stop_hold:
            stop_flat = check(case, bars, closed=closed, cutoff=cutoff)
            assert_expect(stop_flat, expect, (defect.name, "time_stop_flat"))
            assert stop_flat["signal"] == 0 and stop_flat["open_action"] == "none"
            stop_held = check(case, bars, closed=closed, extra=case.helds[0].argv(bars), cutoff=cutoff)
            assert stop_held["close_strategy"] in ("", "time_stop")
            assert stop_held["close_fraction"] == 0.0 and stop_held["signal"] == 0


@pytest.mark.parametrize("name", CASE_NAMES)
def test_missing_timing_holds_the_closed_bar_decision_and_keeps_protection(name):
    case = CASES[name]
    bars = case.entry(1)
    strategy_refs = erefs(case, closes=[case.tp])
    cutoff = decision_cutoff(case, bars)
    position = case.helds[0]
    mid = position.mid(bars) if position.mid else None
    out = check(case, bars, closed=True, extra=position.argv(bars), timing=False, cutoff=cutoff, mid=mid,
                strategy_refs=strategy_refs)
    assert out["closed_bar_decision"]["held"] is True
    assert "timing" in out["closed_bar_decision"]["hold_reason"]
    assert out["indicators"] == {}
    assert out["open_action"] == "none"
    assert out["close_fraction"] == 1.0 and out["signal"] == position.close_signal
    flat = check(case, bars, closed=True, timing=False, cutoff=cutoff, strategy_refs=strategy_refs)
    assert flat["signal"] == 0 and flat["closed_bar_decision"]["held"] is True
    bare = check(case, bars, closed=True, timing=False, cutoff=cutoff)
    assert bare["closed_bar_decision"]["held"] is True
    assert "timing" in bare["closed_bar_decision"]["hold_reason"]
    assert bare["signal"] == 0 and bare["indicators"] == {}


@pytest.mark.parametrize("name", cases_with("cadence_gap_holds"))
def test_cadence_gap_holds_the_entry_and_keeps_protection(name):
    case = CASES[name]
    bars = case.entry(1)
    del bars[-10]
    strategy_refs = erefs(case, closes=[case.tp])
    for position in case.helds:
        args = position.argv(bars)
        out = check(case, bars, closed=False, extra=args, timing=False, strategy_refs=strategy_refs)
        assert out["indicator_labels"][case.reason()] == "cadence_gap", args[1]
        assert out["open_action"] == "none" and out["close_fraction"] == 1.0, args[1]
        assert out["signal"] == position.close_signal, args[1]
    flat = check(case, bars, closed=False, timing=False, strategy_refs=strategy_refs)
    assert flat["signal"] == 0 and flat["indicator_labels"][case.reason()] == "cadence_gap"


@pytest.mark.parametrize("name", cases_with("stale_holds"))
def test_stale_closed_bar_holds_the_entry_and_keeps_protection(name):
    case = CASES[name]
    bars = case.entry(1)
    strategy_refs = erefs(case, closes=[case.tp])
    stale = boundary(bars) + 3 * H
    out = check(case, bars, closed=True, extra=case.helds[0].argv(bars), cutoff=stale, strategy_refs=strategy_refs)
    assert out["closed_bar_decision"]["held"] is False
    assert out["indicator_labels"][case.reason()] == "decision_bar_stale"
    assert out["close_fraction"] == 1.0 and out["signal"] == -1
    flat = check(case, bars, closed=True, cutoff=stale, strategy_refs=strategy_refs)
    assert flat["signal"] == 0 and flat["open_action"] == "none"


@pytest.mark.parametrize("name,closed", case_closed_params())
def test_missing_decision_frame_is_an_error_without_a_private_fetch(name, closed):
    case = CASES[name]
    bars = case.entry(1)
    argv = [name, "BTC", "1h", "--mode=paper", "--market-stdin", "--ohlcv-limit", str(case.limit),
            "--strategy-refs", erefs(case)]
    if closed:
        argv.append("--closed-bar-decisions")
    cutoff = boundary(bars) if case.boundary_cutoff else bars[-1]["t"] + H // 2
    stdin = {"v": 2, "market": market({"ETH|1h": frame(bars, H)}, cutoff_ms=cutoff, mid=bars[-1]["c"],
                                      snapshot_id=case.snapshot_id)}
    proc = run(argv, stdin)
    assert proc.returncode == 1
    out = payload(proc)
    assert out["signal"] == 0
    assert "BTC|1h" in out["error"]
