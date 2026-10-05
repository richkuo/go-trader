#!/usr/bin/env python3

from __future__ import annotations

import argparse
import json
import math
import os
import statistics
import sys
from collections import OrderedDict
from random import Random
from typing import Callable, Dict, List, Optional, Sequence, Tuple

_THIS_DIR = os.path.dirname(os.path.abspath(__file__))
if _THIS_DIR not in sys.path:
    sys.path.insert(0, _THIS_DIR)
sys.path.insert(0, os.path.join(_THIS_DIR, "..", "shared_tools"))

from eval_windows import (
    DATASETS,
    DEFAULT_CAPITAL,
    FEE_PLATFORM,
    WINDOWS,
    dataset_key,
    parse_dataset_arg,
)
from exit_diagnostics import trade_metrics

DEFAULT_BOOTSTRAP_RESAMPLES = 10000
DEFAULT_CI = 0.95
DEFAULT_SEED = 1066

INTRABAR_RESOLUTION = "ohlc_walk"

UNKNOWN_REGIME = "?"

STOP_FIELD_KEYS = (
    "stop_loss_atr_mult",
    "stop_loss_pct",
    "stop_loss_margin_pct",
    "trailing_stop_atr_mult",
    "trailing_stop_pct",
    "stop_loss_atr_mult_regime",
    "trailing_stop_atr_mult_regime",
    "leverage",
    "max_drawdown_pct",
    "trailing_stop_min_move_pct",
    "stop_platform",
    "strategy_type",
    "capability_context",
)


CANDIDATE_STOP_MODES = ("inherit", "drop")
CANDIDATE_STOP_OVERRIDE_KEYS = ("stop_loss_atr_mult", "trailing_stop_atr_mult")
STOP_OWNER_SELECTOR_KEYS = (
    "stop_loss_atr_mult",
    "stop_loss_pct",
    "stop_loss_margin_pct",
    "trailing_stop_atr_mult",
    "trailing_stop_pct",
    "stop_loss_atr_mult_regime",
    "trailing_stop_atr_mult_regime",
)
CANDIDATE_STOP_OVERRIDE_SOURCE = "candidate_stops"


def parse_candidate_stops(value):
    if isinstance(value, str):
        if value in CANDIDATE_STOP_MODES:
            return value
        raise ValueError(
            f"candidate_stops must be one of {list(CANDIDATE_STOP_MODES)} or an object "
            f"with exactly one of {list(CANDIDATE_STOP_OVERRIDE_KEYS)}, got {value!r}")
    if not isinstance(value, dict):
        raise ValueError(
            f"candidate_stops must be one of {list(CANDIDATE_STOP_MODES)} or an object "
            f"with exactly one of {list(CANDIDATE_STOP_OVERRIDE_KEYS)}, got "
            f"{type(value).__name__}")
    unknown = sorted(str(k) for k in value if k not in CANDIDATE_STOP_OVERRIDE_KEYS)
    if unknown:
        raise ValueError(
            f"candidate_stops has unknown key(s) {unknown}; the object takes exactly one "
            f"of {list(CANDIDATE_STOP_OVERRIDE_KEYS)} (ATR multipliers)")
    if len(value) != 1:
        raise ValueError(
            f"candidate_stops object must set exactly one of "
            f"{list(CANDIDATE_STOP_OVERRIDE_KEYS)}, got {sorted(value)}")
    (key, raw), = value.items()
    number = None
    if not isinstance(raw, bool) and isinstance(raw, (int, float)):
        number = float(raw)
    if number is None or not math.isfinite(number) or number <= 0:
        raise ValueError(
            f"candidate_stops {key} must be a finite positive ATR multiplier, got {raw!r}")
    return {key: number}


def candidate_stops_mode(selection) -> str:
    return "replace" if isinstance(selection, dict) else selection


def is_stop_only_candidate(close_refs: Optional[Sequence[dict]], selection) -> bool:
    return not close_refs and isinstance(selection, dict)


def _norm_cdf(z: float) -> float:
    return 0.5 * (1.0 + math.erf(z / math.sqrt(2.0)))


def _binom_two_sided_p(k: int, n: int, p: float = 0.5) -> float:
    if n <= 0:
        return 1.0
    k = max(0, min(k, n))

    log_p = math.log(p)
    log_q = math.log(1.0 - p)
    lg_n1 = math.lgamma(n + 1)

    def _log_pmf(i: int) -> float:
        return (lg_n1 - math.lgamma(i + 1) - math.lgamma(n - i + 1)
                + i * log_p + (n - i) * log_q)

    def _cdf(upper: int) -> float:
        return sum(math.exp(_log_pmf(i)) for i in range(0, upper + 1))

    lower_tail = _cdf(k)
    upper_tail = 1.0 - _cdf(k - 1) if k > 0 else 1.0
    return min(1.0, 2.0 * min(lower_tail, upper_tail))


def sign_test(deltas: Sequence[float], zero_tol: float = 1e-12) -> dict:
    pos = sum(1 for d in deltas if d > zero_tol)
    neg = sum(1 for d in deltas if d < -zero_tol)
    zero = len(deltas) - pos - neg
    n = pos + neg
    k = min(pos, neg)
    return {
        "n": n,
        "n_pos": pos,
        "n_neg": neg,
        "n_zero": zero,
        "p_value": round(_binom_two_sided_p(k, n), 6),
    }


def _ranks_tie_averaged(values: Sequence[float]) -> Tuple[List[float], List[int]]:
    order = sorted(range(len(values)), key=lambda i: values[i])
    ranks = [0.0] * len(values)
    tie_sizes: List[int] = []
    i = 0
    while i < len(order):
        j = i
        while j + 1 < len(order) and values[order[j + 1]] == values[order[i]]:
            j += 1
        avg_rank = (i + 1 + j + 1) / 2.0
        for t in range(i, j + 1):
            ranks[order[t]] = avg_rank
        tie_sizes.append(j - i + 1)
        i = j + 1
    return ranks, tie_sizes


def wilcoxon_signed_rank(deltas: Sequence[float], zero_tol: float = 1e-12) -> dict:
    nz = [d for d in deltas if abs(d) > zero_tol]
    n = len(nz)
    if n == 0:
        return {"n": 0, "w": 0.0, "z": 0.0, "p_value": 1.0}
    ranks, tie_sizes = _ranks_tie_averaged([abs(d) for d in nz])
    w_pos = sum(r for r, d in zip(ranks, nz) if d > 0)
    mean_w = n * (n + 1) / 4.0
    tie_term = sum(t ** 3 - t for t in tie_sizes)
    var_w = (n * (n + 1) * (2 * n + 1) - tie_term / 2.0) / 24.0
    if var_w <= 0:
        return {"n": n, "w": round(w_pos, 4), "z": 0.0, "p_value": 1.0}
    diff = w_pos - mean_w
    cc = 0.5 if diff > 0 else (-0.5 if diff < 0 else 0.0)
    z = (diff - cc) / math.sqrt(var_w)
    p = 2.0 * (1.0 - _norm_cdf(abs(z)))
    return {"n": n, "w": round(w_pos, 4), "z": round(z, 4),
            "p_value": round(min(1.0, max(0.0, p)), 6)}


def _percentile(sorted_xs: Sequence[float], q: float) -> float:
    if len(sorted_xs) == 1:
        return float(sorted_xs[0])
    rank = (q / 100.0) * (len(sorted_xs) - 1)
    lo = math.floor(rank)
    hi = math.ceil(rank)
    if lo == hi:
        return float(sorted_xs[lo])
    return float(sorted_xs[lo] + (sorted_xs[hi] - sorted_xs[lo]) * (rank - lo))


def bootstrap_ci(samples: Sequence[float],
                 statistic: Callable[[Sequence[float]], float] = None,
                 n_resamples: int = DEFAULT_BOOTSTRAP_RESAMPLES,
                 ci: float = DEFAULT_CI, seed: int = DEFAULT_SEED) -> dict:
    stat = statistic or (lambda xs: statistics.fmean(xs))
    n = len(samples)
    if n == 0:
        return {"point": None, "lo": None, "hi": None, "n_resamples": 0}
    point = stat(samples)
    if n < 2:
        return {"point": round(point, 6), "lo": round(point, 6),
                "hi": round(point, 6), "n_resamples": 0}
    rng = Random(seed)
    reps = []
    for _ in range(n_resamples):
        resample = [samples[rng.randrange(n)] for _ in range(n)]
        reps.append(stat(resample))
    reps.sort()
    alpha = (1.0 - ci) / 2.0
    return {
        "point": round(point, 6),
        "lo": round(_percentile(reps, alpha * 100.0), 6),
        "hi": round(_percentile(reps, (1.0 - alpha) * 100.0), 6),
        "n_resamples": n_resamples,
    }


def unpaired_diff_ci(control: Sequence[float], candidate: Sequence[float],
                     n_resamples: int = DEFAULT_BOOTSTRAP_RESAMPLES,
                     ci: float = DEFAULT_CI, seed: int = DEFAULT_SEED) -> dict:
    if not control or not candidate:
        pt = None
        if control and not candidate:
            pt = -statistics.fmean(control)
        elif candidate and not control:
            pt = statistics.fmean(candidate)
        return {"point": (round(pt, 6) if pt is not None else None),
                "lo": None, "hi": None, "n_resamples": 0}
    nc, nk = len(control), len(candidate)
    point = statistics.fmean(candidate) - statistics.fmean(control)
    if nc < 2 or nk < 2:
        return {"point": round(point, 6), "lo": round(point, 6),
                "hi": round(point, 6), "n_resamples": 0}
    rng = Random(seed)
    reps = []
    for _ in range(n_resamples):
        c = statistics.fmean([control[rng.randrange(nc)] for _ in range(nc)])
        k = statistics.fmean([candidate[rng.randrange(nk)] for _ in range(nk)])
        reps.append(k - c)
    reps.sort()
    alpha = (1.0 - ci) / 2.0
    return {
        "point": round(point, 6),
        "lo": round(_percentile(reps, alpha * 100.0), 6),
        "hi": round(_percentile(reps, (1.0 - alpha) * 100.0), 6),
        "n_resamples": n_resamples,
    }


def paired_delta_summary(deltas: Sequence[float],
                         n_resamples: int = DEFAULT_BOOTSTRAP_RESAMPLES,
                         ci: float = DEFAULT_CI, seed: int = DEFAULT_SEED) -> dict:
    deltas = list(deltas)
    if not deltas:
        return {"n": 0, "mean": None, "median": None,
                "sign_test": sign_test(deltas),
                "signed_rank": wilcoxon_signed_rank(deltas),
                "bootstrap": bootstrap_ci(deltas, n_resamples=n_resamples,
                                          ci=ci, seed=seed)}
    return {
        "n": len(deltas),
        "mean": round(statistics.fmean(deltas), 6),
        "median": round(statistics.median(deltas), 6),
        "sign_test": sign_test(deltas),
        "signed_rank": wilcoxon_signed_rank(deltas),
        "bootstrap": bootstrap_ci(deltas, n_resamples=n_resamples, ci=ci, seed=seed),
    }


def collapse_entry(legs: Sequence[dict]) -> Optional[dict]:
    legs = [l for l in legs if l]
    if not legs:
        return None
    metrics = [trade_metrics(l) for l in legs]
    notionals = [float(l.get("shares", 0.0) or 0.0) * float(l.get("entry_price", 0.0) or 0.0)
                 for l in legs]
    total_notional = sum(notionals)
    if total_notional > 0:
        net_pct = sum(m["net_pct"] * w for m, w in zip(metrics, notionals)) / total_notional
        gross_pct = sum(m["gross_pct"] * w for m, w in zip(metrics, notionals)) / total_notional
    else:
        net_pct = statistics.fmean(m["net_pct"] for m in metrics)
        gross_pct = statistics.fmean(m["gross_pct"] for m in metrics)
    return {
        "entry_date": str(legs[0].get("entry_date", "")),
        "side": str(legs[0].get("side", "") or ""),
        "net_pct": net_pct,
        "gross_pct": gross_pct,
        "mfe_pct": max(m["mfe_pct"] for m in metrics),
        "mae_pct": min(m["mae_pct"] for m in metrics),
        "bars_held": max(m["bars_held"] for m in metrics),
        "n_legs": len(legs),
        "exit_reason": str(legs[-1].get("exit_reason", "") or ""),
    }


def group_entries(trades: Sequence[dict]) -> "OrderedDict[str, List[dict]]":
    groups: "OrderedDict[str, List[dict]]" = OrderedDict()
    for t in trades:
        key = str(t.get("entry_date", ""))
        groups.setdefault(key, []).append(t)
    return groups


def free_arm_entries(trades: Sequence[dict]) -> List[dict]:
    out = []
    for legs in group_entries(trades).values():
        rec = collapse_entry(legs)
        if rec is not None:
            out.append(rec)
    return out


def exit_reason_counts(entries: Sequence[Optional[dict]]) -> dict:
    counts: Dict[str, int] = {}
    for e in entries:
        if e:
            reason = str(e.get("exit_reason") or "")
            counts[reason] = counts.get(reason, 0) + 1
    return {k: counts[k] for k in sorted(counts)}


def arm_summary(results: Optional[dict]) -> dict:
    if not results:
        return {"trades": 0, "entries": 0, "win_rate": None, "mean_net_pct": None,
                "total_net_pct": None, "total_return_pct": None,
                "max_drawdown_pct": None, "sharpe": None, "liquidated": False,
                "exit_reasons": {}}
    entries = free_arm_entries(results.get("trades", []) or [])
    nets = [e["net_pct"] for e in entries]
    return {
        "exit_reasons": exit_reason_counts(entries),
        "trades": int(results.get("total_trades", len(entries)) or 0),
        "entries": len(entries),
        "win_rate": (round(sum(1 for x in nets if x > 0) / len(nets), 4) if nets else None),
        "mean_net_pct": (round(statistics.fmean(nets), 4) if nets else None),
        "total_net_pct": (round(sum(nets), 4) if nets else None),
        "total_return_pct": _round_or_none(results.get("total_return_pct")),
        "max_drawdown_pct": _round_or_none(results.get("max_drawdown_pct")),
        "sharpe": _round_or_none(results.get("sharpe_ratio")),
        "liquidated": bool(results.get("liquidated")),
    }


def _round_or_none(v, prec: int = 4):
    return round(float(v), prec) if v is not None else None


def build_paired_rows(control_entries: Sequence[dict],
                      candidate_by_date: Dict[str, Optional[dict]],
                      regime_by_date: Dict[str, str]) -> Tuple[List[dict], dict]:
    rows: List[dict] = []
    unmatched = 0
    for ctrl in control_entries:
        date = ctrl["entry_date"]
        cand = candidate_by_date.get(date)
        if cand is None:
            unmatched += 1
            continue
        rows.append({
            "entry_date": date,
            "regime": regime_by_date.get(date, UNKNOWN_REGIME) or UNKNOWN_REGIME,
            "side": ctrl["side"],
            "control_net_pct": ctrl["net_pct"],
            "candidate_net_pct": cand["net_pct"],
            "delta_net_pct": cand["net_pct"] - ctrl["net_pct"],
            "control_mfe_pct": ctrl["mfe_pct"],
            "candidate_mfe_pct": cand["mfe_pct"],
            "control_mae_pct": ctrl["mae_pct"],
            "candidate_mae_pct": cand["mae_pct"],
            "control_bars_held": ctrl["bars_held"],
            "candidate_bars_held": cand["bars_held"],
        })
    diag = {
        "schedule_entries": len(control_entries),
        "paired": len(rows),
        "unmatched": unmatched,
    }
    return rows, diag


def _delta_block(rows: Sequence[dict], n_resamples: int, ci: float, seed: int) -> dict:
    n = len(rows)
    ctrl_net = [r["control_net_pct"] for r in rows]
    cand_net = [r["candidate_net_pct"] for r in rows]
    deltas = [r["delta_net_pct"] for r in rows]

    def _winrate(xs):
        return round(sum(1 for x in xs if x > 0) / len(xs), 4) if xs else None

    def _med(xs):
        return round(statistics.median(xs), 4) if xs else None

    return {
        "n": n,
        "control_mean_net_pct": (round(statistics.fmean(ctrl_net), 4) if ctrl_net else None),
        "candidate_mean_net_pct": (round(statistics.fmean(cand_net), 4) if cand_net else None),
        "control_total_net_pct": (round(sum(ctrl_net), 4) if ctrl_net else None),
        "candidate_total_net_pct": (round(sum(cand_net), 4) if cand_net else None),
        "control_win_rate": _winrate(ctrl_net),
        "candidate_win_rate": _winrate(cand_net),
        "delta_win_rate": (round(_winrate(cand_net) - _winrate(ctrl_net), 4)
                           if ctrl_net and cand_net else None),
        "control_median_mae_pct": _med([r["control_mae_pct"] for r in rows]),
        "candidate_median_mae_pct": _med([r["candidate_mae_pct"] for r in rows]),
        "control_median_mfe_pct": _med([r["control_mfe_pct"] for r in rows]),
        "candidate_median_mfe_pct": _med([r["candidate_mfe_pct"] for r in rows]),
        "paired_delta": paired_delta_summary(deltas, n_resamples=n_resamples,
                                             ci=ci, seed=seed),
    }


def per_regime_table(rows: Sequence[dict], n_resamples: int = DEFAULT_BOOTSTRAP_RESAMPLES,
                     ci: float = DEFAULT_CI, seed: int = DEFAULT_SEED) -> dict:
    by_regime: "OrderedDict[str, List[dict]]" = OrderedDict()
    for r in rows:
        by_regime.setdefault(r["regime"], []).append(r)
    regimes = {}
    for label in sorted(by_regime.keys()):
        regimes[label] = _delta_block(by_regime[label], n_resamples, ci, seed)
    return {
        "all": _delta_block(list(rows), n_resamples, ci, seed),
        "by_regime": regimes,
    }


def replay_capability(close_refs: Optional[Sequence[dict]],
                      comparison_mode: Optional[str] = None,
                      candidate_stops=None) -> dict:
    from backtester import CloseCapabilityError, validate_close_capabilities
    if not close_refs:
        stop_only = is_stop_only_candidate(close_refs, candidate_stops)
        return {"replayable": stop_only, "refusal": None, "close_validation": None}
    try:
        validation = validate_close_capabilities(
            close_refs=close_refs, comparison_mode=comparison_mode,
            platform=FEE_PLATFORM, consumer="entry_replay", phase="preflight")
    except CloseCapabilityError as exc:
        replay_only = all(r["reason_code"] == "UNSUPPORTED_REPLAY_CAPABILITY"
                          for r in exc.reasons)
        return {"replayable": False,
                "refusal": None if replay_only else exc.to_dict(),
                "close_validation": exc.validation.to_dict()}
    return {"replayable": True, "refusal": None,
            "close_validation": validation.to_dict()}


def candidate_is_replayable(close_refs: Optional[Sequence[dict]],
                            comparison_mode: Optional[str] = None) -> bool:
    return replay_capability(close_refs, comparison_mode)["replayable"]


def _prepare_signals(reg, open_name: str, params: Optional[dict], df):
    from atr import ensure_atr_indicator
    df_signals = reg.apply_strategy(open_name, df, params)
    df_signals = ensure_atr_indicator(df_signals)
    return df_signals


def _regime_label_series(df, regime_cfg: dict):
    from regime import ensure_regime_columns
    work = df.copy()
    ensure_regime_columns(
        work,
        period=int(regime_cfg.get("period", 14)),
        adx_threshold=float(regime_cfg.get("adx_threshold", 20.0)),
        classifier=str(regime_cfg.get("classifier", "adx")),
        thresholds=regime_cfg.get("thresholds"),
        windows_spec=regime_cfg.get("windows_spec"),
        gate_window=str(regime_cfg.get("gate_window", "") or ""),
    )
    return [str(x or "") for x in work["regime"].tolist()]


def _arm_stop_owner(close_refs: Optional[Sequence[dict]], stops: Optional[dict],
                    regime_windows_spec=None) -> Optional[str]:
    from backtester import CapabilityContext, build_stop_capability_context
    stops = stops or {}
    fields = {k: v for k, v in stops.items()
              if k not in ("stop_platform", "strategy_type", "capability_context")}
    context = build_stop_capability_context(
        platform=stops.get("stop_platform") or FEE_PLATFORM,
        strategy_type=stops.get("strategy_type") or "perps",
        close_refs=close_refs, fields=fields,
        regime_windows_spec=regime_windows_spec,
        capability_context=stops.get("capability_context"))
    if not isinstance(context, CapabilityContext) or context.resolved_stop_owner is None:
        return None
    return context.resolved_stop_owner["name"]


def _backtester_kwargs(open_name: str, params: Optional[dict],
                       close_refs: Optional[Sequence[dict]], direction: Optional[str],
                       capital: float, gate: dict,
                       stops: Optional[dict] = None,
                       comparison_mode: Optional[str] = None) -> dict:
    from backtester import STOP_OWNERS_NEEDING_LABEL
    use_regime = (bool(gate.get("allowed_regimes"))
                  or _arm_stop_owner(close_refs, stops, gate.get("windows_spec"))
                  in STOP_OWNERS_NEEDING_LABEL)
    kw = dict(
        comparison_mode=comparison_mode,
        initial_capital=capital, platform=FEE_PLATFORM,
        intrabar_resolution=INTRABAR_RESOLUTION,
        open_strategy={"name": open_name, "params": dict(params or {})},
        close_strategies=(list(close_refs) if close_refs else None),
        direction=direction,
        regime_enabled=use_regime,
        regime_period=int(gate.get("period", 14)),
        regime_adx_threshold=float(gate.get("adx_threshold", 20.0)),
        regime_windows_spec=gate.get("windows_spec"),
        allowed_regimes=(list(gate["allowed_regimes"]) if gate.get("allowed_regimes") else None),
    )
    for k in STOP_FIELD_KEYS:
        v = (stops or {}).get(k)
        if v is not None:
            kw[k] = v
    return kw


def _stop_only_frame(df_signals):
    if "open_action" in df_signals.columns or any(
            c == "close_fraction" or str(c).startswith("close_fraction:")
            for c in df_signals.columns):
        raise ValueError(
            "a stop-only candidate needs a signal-only open frame, but the open strategy "
            "already emits open_action/close_fraction columns")
    out = df_signals.copy()
    out["close_fraction"] = 0.0
    return out


def run_free_arm(reg, open_name: str, params: Optional[dict], df_signals,
                 close_refs: Optional[Sequence[dict]], direction: Optional[str],
                 capital: float, gate: dict, symbol: str, timeframe: str,
                 stops: Optional[dict] = None,
                 comparison_mode: Optional[str] = None) -> dict:
    from backtester import Backtester
    bt = Backtester(**_backtester_kwargs(open_name, params, close_refs, direction,
                                         capital, gate, stops, comparison_mode))
    return bt.run(df_signals.copy(), strategy_name=open_name, symbol=symbol,
                  timeframe=timeframe, params=params, save=False)


def replay_candidate_for_entry(reg, open_name: str, params: Optional[dict], df_signals,
                               sig_pos: int, side_sign: int,
                               candidate_close: Sequence[dict], direction: Optional[str],
                               capital: float, gate: dict, symbol: str,
                               timeframe: str, stops: Optional[dict] = None,
                               comparison_mode: Optional[str] = None) -> Optional[dict]:
    from backtester import Backtester
    one = df_signals.copy()
    sig_col = one.columns.get_loc("signal")
    one.iloc[:, sig_col] = 0
    one.iloc[sig_pos, sig_col] = int(side_sign)
    bt = Backtester(**_backtester_kwargs(open_name, params, candidate_close, direction,
                                         capital, gate, stops, comparison_mode))
    results = bt.run(one, strategy_name=open_name, symbol=symbol,
                     timeframe=timeframe, params=params, save=False)
    return collapse_entry(results.get("trades", []) or [])


def evaluate_dataset_window(reg, spec: dict, symbol: str, timeframe: str,
                            window: tuple) -> Optional[dict]:
    from data_fetcher import load_cached_data
    from run_backtest import FUNDING_COLUMN_STRATEGIES, _attach_funding_if_needed

    start, end = window
    df = load_cached_data(symbol, timeframe, start_date=start, end_date=end)
    if df.empty:
        return None
    if spec["open_name"] in FUNDING_COLUMN_STRATEGIES:
        df = _attach_funding_if_needed(df, spec["open_name"], symbol, start)

    df_signals = _prepare_signals(reg, spec["open_name"], spec.get("params"), df)
    regime_series = _regime_label_series(df_signals, spec["regime_cfg"])
    pos_by_date = {str(ts): i for i, ts in enumerate(df_signals.index)}

    candidate_signals = (_stop_only_frame(df_signals) if spec.get("candidate_stop_only")
                         else df_signals)

    control_results = run_free_arm(
        reg, spec["open_name"], spec.get("params"), df_signals,
        spec.get("incumbent_close"), spec.get("direction"), spec["capital"],
        spec["gate"], symbol, timeframe, spec.get("control_stops"),
        comparison_mode=spec.get("comparison_mode"))
    candidate_results = run_free_arm(
        reg, spec["open_name"], spec.get("params"), candidate_signals,
        spec.get("candidate_close"), spec.get("direction"), spec["capital"],
        spec["gate"], symbol, timeframe, spec.get("candidate_stops"),
        comparison_mode=spec.get("comparison_mode"))

    control_entries = free_arm_entries(control_results.get("trades", []) or [])

    paired_rows: List[dict] = []
    paired_diag = {"schedule_entries": len(control_entries), "paired": 0,
                   "unmatched": 0, "replayable": spec["replayable"]}
    if spec["replayable"]:
        candidate_by_date: Dict[str, Optional[dict]] = {}
        regime_by_date: Dict[str, str] = {}
        for ctrl in control_entries:
            date = ctrl["entry_date"]
            fill_pos = pos_by_date.get(date)
            if fill_pos is None or fill_pos - 1 < 0:
                candidate_by_date[date] = None
                continue
            sig_pos = fill_pos - 1
            label = regime_series[sig_pos] if 0 <= sig_pos < len(regime_series) else ""
            regime_by_date[date] = label or UNKNOWN_REGIME
            side_sign = -1 if ctrl["side"] == "short" else 1
            candidate_by_date[date] = replay_candidate_for_entry(
                reg, spec["open_name"], spec.get("params"), candidate_signals, sig_pos,
                side_sign, spec["candidate_close"], spec.get("direction"),
                spec["capital"], spec["gate"], symbol, timeframe,
                spec.get("candidate_stops"),
                comparison_mode=spec.get("comparison_mode"))
        paired_rows, paired_diag = build_paired_rows(
            control_entries, candidate_by_date, regime_by_date)
        paired_diag["replayable"] = True
        paired_diag["candidate_exit_reasons"] = exit_reason_counts(
            list(candidate_by_date.values()))

    table = per_regime_table(paired_rows, n_resamples=spec["n_resamples"],
                             ci=spec["ci"], seed=spec["seed"]) if paired_rows else None

    ctrl_free_nets = [e["net_pct"] for e in control_entries]
    cand_free_entries = free_arm_entries(candidate_results.get("trades", []) or [])
    cand_free_nets = [e["net_pct"] for e in cand_free_entries]
    unpaired = unpaired_diff_ci(ctrl_free_nets, cand_free_nets,
                                n_resamples=spec["n_resamples"], ci=spec["ci"],
                                seed=spec["seed"])

    return {
        "dataset": dataset_key(symbol, timeframe),
        "control_arm": arm_summary(control_results),
        "candidate_arm": arm_summary(candidate_results),
        "unpaired_delta_net_pct": unpaired,
        "paired_diag": paired_diag,
        "per_regime": table,
        "close_validation": {
            "control": control_results.get("close_validation"),
            "candidate": candidate_results.get("close_validation"),
        },
    }


def _parse_close_arg(raw: str, label: str) -> Optional[List[dict]]:
    if raw is None:
        return None
    if raw.strip().lower() == "none":
        return None
    refs = json.loads(raw)
    if not isinstance(refs, list) or not all(
            isinstance(r, dict) and r.get("name") for r in refs):
        raise SystemExit(f"{label} must be a JSON list of close refs "
                         f"[{{\"name\":..., \"params\":...}}] or the literal 'none'")
    return refs


def _stops_from_kwargs(kwargs: dict) -> dict:
    return {k: kwargs.get(k) for k in STOP_FIELD_KEYS if kwargs.get(k) is not None}


def _stops_for_json(stops: Optional[dict]) -> Optional[dict]:
    if stops is None:
        return None
    return {k: (v.to_dict() if hasattr(v, "to_dict") else v) for k, v in stops.items()}


_UNREPLAYABLE_ENTRY_SHAPERS = (
    "invert_signal",
    "regime_directional_policy",
    "profile_allocation",
)


def _reject_unreplayable_entry_shapers(kwargs: dict) -> None:
    offenders = [k for k in _UNREPLAYABLE_ENTRY_SHAPERS if kwargs.get(k)]
    if offenders:
        raise SystemExit(
            f"--baseline-config strategy uses live entry-shaping field(s) {offenders} "
            f"that M6's entry-locked replay cannot reproduce faithfully "
            f"(invert_signal flips the traded side, regime_directional_policy mutates "
            f"the entry direction per regime, profile_allocation swaps open params per "
            f"profile) — the control arm would silently NOT trade the live incumbent's "
            f"entries, so the A/B would compare against a phantom incumbent. M6 refuses "
            f"rather than mislead. To A/B this strategy's EXIT, drive the open side "
            f"explicitly instead of resolving it from the config: --incumbent-close "
            f"'<live close json>' --direction <long|short> (drop --baseline-config).")


def _stops_have_owner(stops: Optional[dict]) -> bool:
    if not stops:
        return False
    from backtester import build_stop_capability_context
    fields = {k: v for k, v in stops.items()
              if k not in ("stop_platform", "strategy_type", "capability_context")}
    context = build_stop_capability_context(
        platform=stops.get("stop_platform") or FEE_PLATFORM,
        strategy_type=stops.get("strategy_type") or "perps",
        close_refs=None, fields=fields)
    owner = context.resolved_stop_owner["name"]
    if owner == "legacy":
        return any(stops.get(k) not in (None, 0, 0.0) for k in STOP_FIELD_KEYS[:7])
    return owner != "none"


def _override_capability_context(context, key: str, value: float):
    from backtester import CapabilityContext
    if not isinstance(context, CapabilityContext) or context.errors:
        return context
    thawed = context.to_dict()
    raw = thawed["raw_fields"]
    for k in STOP_OWNER_SELECTOR_KEYS:
        raw[k] = {"present": False, "value": None}
    raw[key] = {"present": True, "value": value}
    evidence = thawed["input_evidence"]
    live_units = evidence.get("resolved_live_units")
    if isinstance(live_units, dict) and isinstance(live_units.get("value"), dict):
        values = dict(live_units["value"])
        for k in STOP_OWNER_SELECTOR_KEYS:
            values[k] = None
        values[key] = value
        evidence["resolved_live_units"] = {**live_units, "value": values}
    evidence["candidate_stop_override"] = {
        "status": "verified", "source": CANDIDATE_STOP_OVERRIDE_SOURCE,
        "value": {key: value}}
    return CapabilityContext(raw_fields=raw, resolved_stop_owner=None,
                             input_evidence=evidence)


def _candidate_stops(selection, incumbent_stops: dict) -> dict:
    if selection == "drop":
        return {}
    if not isinstance(selection, dict):
        return dict(incumbent_stops or {})
    (key, value), = parse_candidate_stops(selection).items()
    out = {k: v for k, v in (incumbent_stops or {}).items()
           if k not in STOP_OWNER_SELECTOR_KEYS}
    out[key] = value
    if out.get("capability_context") is not None:
        out["capability_context"] = _override_capability_context(
            out["capability_context"], key, value)
    return out


def resolve_candidate_stop_policy(close_refs: Optional[Sequence[dict]], selection,
                                  incumbent_stops: Optional[dict],
                                  comparison_mode: Optional[str] = None,
                                  regime_windows_spec=None) -> Tuple[dict, str]:
    from backtester import build_stop_capability_context, validate_close_capabilities
    stops = _candidate_stops(selection, incumbent_stops or {})
    fields = {k: v for k, v in stops.items()
              if k not in ("stop_platform", "strategy_type", "capability_context")}
    strategy_type = stops.get("strategy_type") or "perps"
    context = build_stop_capability_context(
        platform=stops.get("stop_platform") or FEE_PLATFORM,
        strategy_type=strategy_type, close_refs=close_refs, fields=fields,
        regime_windows_spec=regime_windows_spec,
        capability_context=stops.get("capability_context"))
    validate_close_capabilities(
        close_refs=close_refs, comparison_mode=comparison_mode, platform=FEE_PLATFORM,
        strategy_type=strategy_type, consumer="engine", phase="preflight",
        capability_context=context)
    return stops, context.resolved_stop_owner["name"]


_STOP_CLASS_CANDIDATE_NAMES = {
    "atr_stop",
    "stop_loss_atr_mult",
    "trailing_stop_atr_mult",
    "trailing_stop_atr_mult_regime",
}


def _candidate_stacks_on_inherited_stop(candidate_close: Optional[Sequence[dict]],
                                        mode: str, incumbent_stops: dict) -> bool:
    if mode != "inherit" or not candidate_close or not _stops_have_owner(incumbent_stops):
        return False
    return any(isinstance(r, dict) and r.get("name") in _STOP_CLASS_CANDIDATE_NAMES
               for r in candidate_close)


def resolve_from_baseline(config_path: str, strategy_id: str,
                          comparison_mode: Optional[str] = None) -> dict:
    from run_backtest import load_strategy_config
    import json as _json
    kwargs = load_strategy_config(config_path, strategy_id,
                                  comparison_mode=comparison_mode)
    _reject_unreplayable_entry_shapers(kwargs)
    open_ref = kwargs.get("open_strategy") or {}
    with open(config_path) as fh:
        cfg = _json.load(fh)
    sc = next((s for s in cfg.get("strategies", []) or []
               if s.get("id") == strategy_id), {})
    return {
        "open_name": open_ref.get("name"),
        "params": dict(open_ref.get("params") or {}) or None,
        "incumbent_close": kwargs.get("close_strategies") or None,
        "stops": _stops_from_kwargs(kwargs),
        "direction": kwargs.get("direction"),
        "allowed_regimes": sc.get("allowed_regimes") or None,
        "regime_section": cfg.get("regime") or {},
    }


def _composite_windows_spec(period: int) -> dict:
    return {"attribution": {"classifier": "composite", "period": int(period)}}


def resolve_regime_cfg(args, regime_section: dict) -> dict:
    base = {"period": args.regime_period, "adx_threshold": args.regime_adx_threshold,
            "gate_window": args.gate_window or ""}
    if args.regime_windows_json:
        spec = json.loads(args.regime_windows_json)
        return {**base, "classifier": "composite", "windows_spec": spec}
    if args.regime_classifier == "composite":
        return {**base, "classifier": "composite",
                "windows_spec": _composite_windows_spec(args.regime_period),
                "gate_window": args.gate_window or "attribution"}
    if args.regime_classifier == "adx":
        return {**base, "classifier": "adx", "windows_spec": None}
    windows = (regime_section or {}).get("windows")
    if windows:
        return {**base, "classifier": "composite", "windows_spec": windows}
    return {**base, "classifier": "adx", "windows_spec": None}


def _p(v, prec=2):
    return f"{v:+.{prec}f}" if isinstance(v, (int, float)) else "    -"


def _sig_mark(p_value: Optional[float]) -> str:
    if p_value is None:
        return ""
    if p_value < 0.01:
        return "***"
    if p_value < 0.05:
        return "** "
    if p_value < 0.10:
        return "*  "
    return "   "


def format_dataset_report(res: dict) -> str:
    lines = [f"\n  ── {res['dataset']} ──"]
    ca, ka = res["control_arm"], res["candidate_arm"]
    lines.append(
        f"    free arms (realistic): control {ca['entries']} entries "
        f"net {_p(ca['total_net_pct'])}% (win {_pct(ca['win_rate'])}, "
        f"maxDD {_p(ca['max_drawdown_pct'])}%)  |  candidate {ka['entries']} entries "
        f"net {_p(ka['total_net_pct'])}% (win {_pct(ka['win_rate'])}, "
        f"maxDD {_p(ka['max_drawdown_pct'])}%)")
    u = res["unpaired_delta_net_pct"]
    lines.append(f"    unpaired Δ mean-net/entry: {_p(u['point'])}%  "
                 f"95% CI [{_p(u['lo'])}, {_p(u['hi'])}]")
    diag = res["paired_diag"]
    if not diag.get("replayable"):
        lines.append("    paired: UNAVAILABLE — candidate exit is signal-reversal "
                     "(no per-entry rule to isolate); unpaired view only.")
        return "\n".join(lines)
    lines.append(f"    paired (entry-locked): {diag['paired']}/{diag['schedule_entries']} "
                 f"incumbent entries replayed"
                 + (f", {diag['unmatched']} unmatched" if diag.get("unmatched") else ""))
    table = res.get("per_regime")
    if not table:
        lines.append("    (no paired entries)")
        return "\n".join(lines)
    lines.append(f"    {'regime':<18} {'n':>4} {'ctrlNet':>9} {'candNet':>9} "
                 f"{'Δnet/e':>8} {'Δwin':>7} {'signed-rank p':>14}")
    for label in list(table["by_regime"].keys()) + ["ALL"]:
        blk = table["all"] if label == "ALL" else table["by_regime"][label]
        pd = blk["paired_delta"]
        sr = pd["signed_rank"]["p_value"]
        lines.append(
            f"    {label:<18} {blk['n']:>4} "
            f"{_p(blk['control_mean_net_pct'])!s:>9} {_p(blk['candidate_mean_net_pct'])!s:>9} "
            f"{_p(pd['mean'])!s:>8} {_pct(blk['delta_win_rate'], signed=True)!s:>7} "
            f"{(_p(sr,4) if sr is not None else '-')!s:>10} {_sig_mark(sr)}")
    return "\n".join(lines)


def _pct(v, signed: bool = False):
    if not isinstance(v, (int, float)):
        return "  -"
    return (f"{v*100:+.1f}%" if signed else f"{v*100:.0f}%")


def format_window_report(window_name: str, window: tuple, datasets: List[dict]) -> str:
    start, end = window
    out = [f"\n== window {window_name} ({start} → {end or 'latest'}) =="]
    for res in datasets:
        out.append(format_dataset_report(res))
    return "\n".join(out)


def format_summary(per_window: "OrderedDict[str, List[dict]]") -> str:
    out = ["\n== summary: candidate − incumbent, mean Δnet%/entry (ALL regimes) =="]
    out.append(f"  {'window':<10} {'paired n':>9} {'Δnet/e':>9} "
               f"{'signed-rank p':>14} {'unpaired Δ':>11}")
    for wname, datasets in per_window.items():
        tot_paired = sum((d["paired_diag"]["paired"] for d in datasets), 0)
        deltas = []
        for d in datasets:
            t = d.get("per_regime")
            if t and t["all"]["paired_delta"]["mean"] is not None:
                deltas.append((t["all"]["paired_delta"]["mean"], t["all"]["n"]))
        wmean = (round(sum(m * n for m, n in deltas) / sum(n for _, n in deltas), 4)
                 if deltas and sum(n for _, n in deltas) else None)
        u_points = [d["unpaired_delta_net_pct"]["point"] for d in datasets
                    if d["unpaired_delta_net_pct"]["point"] is not None]
        umean = round(statistics.fmean(u_points), 4) if u_points else None
        out.append(f"  {wname:<10} {tot_paired:>9} {_p(wmean)!s:>9} "
                   f"{'(per-dataset above)':>14} {_p(umean)!s:>11}")
    out.append("  * p<.10  ** p<.05  *** p<.01 (Wilcoxon signed-rank on per-entry ΔPnL)")
    out.append("  Δnet/e = candidate − incumbent net % per paired entry; "
               "positive favours the candidate exit.")
    return "\n".join(out)


def build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(
        description="M6 regime-conditioned, incumbent-relative exit-policy A/B (#1066)")
    p.add_argument("--strategy", required=True,
                   help="Open-strategy name, OR (with --baseline-config) the live "
                        "strategy id whose open + incumbent close to resolve")
    p.add_argument("--params", default=None,
                   help="Open params JSON (ignored when --baseline-config supplies them)")
    p.add_argument("--registry", choices=["spot", "futures"], default="spot")
    p.add_argument("--baseline-config", default=None,
                   help="Live v15 config: resolves the incumbent close (and open) "
                        "for --strategy the way the daemon would (#951-gated). "
                        "Fail-loud — no silent fallback.")
    p.add_argument("--incumbent-close", default=None,
                   help="Explicit control close refs JSON, or 'none' for open-as-close. "
                        "Required when --baseline-config is absent.")
    p.add_argument("--candidate-close", required=True,
                   help="Candidate close refs JSON under test (or 'none' to test "
                        "removing the exit). The thing being A/B'd.")
    p.add_argument("--candidate-stops", default="inherit",
                   help="How the candidate arm treats the incumbent's strategy-level "
                        "stops (resolved from --baseline-config). 'inherit' (default) "
                        "holds them fixed so the A/B isolates the close-evaluator "
                        "change; 'drop' runs the candidate with NO strategy-level stop "
                        "so its close refs are the entire exit (full-policy "
                        "replacement). A JSON object with exactly one of "
                        "stop_loss_atr_mult or trailing_stop_atr_mult (a finite "
                        "positive ATR multiplier) REPLACES every inherited stop-owner "
                        "selector on the candidate arm and keeps the platform, type, "
                        "leverage, drawdown and trailing-move inputs; with an empty "
                        "--candidate-close it is a stop-only candidate (no close "
                        "evaluator, no signal-reversal exit, paired replay enabled). "
                        "The control arm always keeps the incumbent stops.")
    p.add_argument("--direction", default=None, choices=["long", "short", "both"],
                   help="Entry side held fixed across both arms (default: long, "
                        "or the baseline config's direction)")
    p.add_argument("--allowed-regimes", action="append", default=None, metavar="LABEL",
                   help="Gate entries to this regime label (repeatable). Applied "
                        "identically to both arms so the entry universe is shared.")
    p.add_argument("--regime-classifier", default=None, choices=["adx", "composite"],
                   help="Attribution/gate classifier override (default: the baseline "
                        "config's, else adx)")
    p.add_argument("--regime-period", type=int, default=14)
    p.add_argument("--regime-adx-threshold", type=float, default=20.0)
    p.add_argument("--regime-windows-json", default=None,
                   help="Composite windows_spec JSON (classifier=composite)")
    p.add_argument("--gate-window", default=None,
                   help="Named window key inside a composite windows_spec to classify on")
    p.add_argument("--windows", default=None,
                   help=f"Comma list of windows (default: is,oos). Known: {', '.join(WINDOWS)}")
    p.add_argument("--datasets", default=None,
                   help="Comma list of SYMBOL:TIMEFRAME (default: the six audit datasets)")
    p.add_argument("--capital", type=float, default=DEFAULT_CAPITAL)
    p.add_argument("--bootstrap-resamples", type=int, default=DEFAULT_BOOTSTRAP_RESAMPLES)
    p.add_argument("--ci", type=float, default=DEFAULT_CI)
    p.add_argument("--seed", type=int, default=DEFAULT_SEED)
    p.add_argument("--json", default=None, dest="json_out",
                   help="Write the full structured result to this path")
    p.add_argument("--intrabar-resolution", dest="intrabar_resolution",
                   choices=["ohlc_walk", "bar_close"], default="ohlc_walk",
                   help="Same-bar SL/TP race resolution (#1271); bar_close "
                        "reproduces pre-#1271 legacy baselines")
    p.add_argument("--comparison-mode", dest="comparison_mode", default=None,
                   metavar="MODE",
                   help="#1683 close comparison mode for both arms. Omitted = "
                        "strict: refuses time_stop/zscore_target (no live input) "
                        "and HL-live-only closes. 'approximate' A/Bs research "
                        "exits; the JSON carries close_validation with incomplete "
                        "parity.")
    return p


def _parse_candidate_stops_arg(raw: str):
    text = str(raw).strip()
    if text in CANDIDATE_STOP_MODES:
        return text
    try:
        value = json.loads(text)
    except json.JSONDecodeError as exc:
        raise SystemExit(
            f"--candidate-stops must be {' or '.join(CANDIDATE_STOP_MODES)}, or a JSON "
            f"object with exactly one of {list(CANDIDATE_STOP_OVERRIDE_KEYS)}: {exc}")
    try:
        return parse_candidate_stops(value)
    except ValueError as exc:
        raise SystemExit(f"--candidate-stops: {exc}")


def _resolve_spec(args) -> dict:
    open_name = args.strategy
    stop_selection = _parse_candidate_stops_arg(args.candidate_stops)
    params = json.loads(args.params) if args.params else None
    direction = args.direction
    incumbent_close = None
    incumbent_stops: dict = {}
    regime_section: dict = {}
    config_allowed_regimes = None

    if args.baseline_config:
        try:
            resolved = resolve_from_baseline(args.baseline_config, args.strategy,
                                             args.comparison_mode)
        except ValueError as exc:
            raise SystemExit(str(exc))
        if not resolved["open_name"]:
            raise SystemExit(
                f"{args.baseline_config}: strategy {args.strategy!r} resolved no "
                f"open_strategy.name")
        open_name = resolved["open_name"]
        if params is None:
            params = resolved["params"]
        incumbent_close = resolved["incumbent_close"]
        incumbent_stops = resolved["stops"]
        if direction is None:
            direction = resolved["direction"]
        config_allowed_regimes = resolved["allowed_regimes"]
        regime_section = resolved["regime_section"]
        if args.incumbent_close is not None:
            raise SystemExit("--incumbent-close conflicts with --baseline-config; "
                             "the baseline config IS the incumbent. Drop one.")
    else:
        if args.incumbent_close is None:
            raise SystemExit(
                "no incumbent: pass --baseline-config <v15 config> --strategy <id> "
                "to resolve the live close, or --incumbent-close '<json>' (or "
                "--incumbent-close none for an explicit open-as-close control).")
        incumbent_close = _parse_close_arg(args.incumbent_close, "--incumbent-close")
        if stop_selection == "drop":
            print("[WARN] --candidate-stops drop has no effect without --baseline-config "
                  "(the explicit --incumbent-close path resolves no strategy-level stops).",
                  file=sys.stderr)

    candidate_close = _parse_close_arg(args.candidate_close, "--candidate-close")
    direction = direction or "long"
    from backtester import CloseCapabilityError, validate_close_capabilities
    for label, refs in (("incumbent close", incumbent_close),
                        ("candidate close", candidate_close)):
        try:
            validate_close_capabilities(close_refs=refs,
                                        comparison_mode=args.comparison_mode,
                                        platform=FEE_PLATFORM, phase="preflight")
        except CloseCapabilityError as exc:
            raise SystemExit(f"{label}: {exc}")
    replay = replay_capability(candidate_close, args.comparison_mode, stop_selection)
    if replay["refusal"] is not None:
        raise SystemExit(f"candidate close: {replay['refusal']['message']}")

    candidate_stops = _candidate_stops(stop_selection, incumbent_stops)
    if _candidate_stacks_on_inherited_stop(candidate_close, stop_selection,
                                           incumbent_stops):
        print("[WARN] the candidate is a protective stop AND --candidate-stops inherit "
              "(default) keeps the incumbent's stop, so the candidate stacks under it "
              "— the effective exit is the TIGHTER of the two and the A/B reflects "
              "mostly the inherited stop, not the candidate in isolation. Pass "
              "--candidate-stops drop to measure the candidate stop alone.",
              file=sys.stderr)

    regime_cfg = resolve_regime_cfg(args, regime_section)
    if args.gate_window:
        ws = regime_cfg.get("windows_spec") or {}
        if len(ws) > 1:
            raise SystemExit(
                f"--gate-window {args.gate_window!r} selects one window of a "
                f"multi-window spec for attribution, but the backtester's entry gate "
                f"has no gate-window parameter and default-picks the primary window "
                f"(regime.py) — so the gate and the regime attribution would classify "
                f"on different windows and silently mis-bucket the A/B (same reason "
                f"run_backtest rejects a named regime_gate_window). Use a single-window "
                f"--regime-windows-json so gate and attribution agree, or drop "
                f"--gate-window (both then default-pick the same window).")
        if ws and args.gate_window not in ws:
            raise SystemExit(
                f"--gate-window {args.gate_window!r} names no window in the resolved "
                f"windows_spec (keys: {sorted(ws)}).")
    allowed_regimes = args.allowed_regimes or config_allowed_regimes
    gate = {
        "allowed_regimes": allowed_regimes,
        "classifier": regime_cfg["classifier"],
        "period": regime_cfg["period"],
        "adx_threshold": regime_cfg["adx_threshold"],
        "windows_spec": regime_cfg["windows_spec"],
        "gate_window": regime_cfg["gate_window"],
    }
    if isinstance(stop_selection, dict):
        try:
            candidate_stops, _ = resolve_candidate_stop_policy(
                candidate_close, stop_selection, incumbent_stops,
                args.comparison_mode, gate["windows_spec"])
        except CloseCapabilityError as exc:
            raise SystemExit(f"candidate stops: {exc}")
    return {
        "open_name": open_name,
        "params": params,
        "direction": direction,
        "incumbent_close": incumbent_close,
        "candidate_close": candidate_close,
        "control_stops": incumbent_stops,
        "candidate_stops": candidate_stops,
        "candidate_stops_mode": candidate_stops_mode(stop_selection),
        "candidate_stops_selection": (stop_selection if isinstance(stop_selection, dict)
                                      else None),
        "candidate_stop_only": is_stop_only_candidate(candidate_close, stop_selection),
        "control_stop_owner": _arm_stop_owner(incumbent_close, incumbent_stops,
                                              gate["windows_spec"]),
        "candidate_stop_owner": _arm_stop_owner(candidate_close, candidate_stops,
                                                gate["windows_spec"]),
        "replayable": replay["replayable"],
        "comparison_mode": args.comparison_mode,
        "gate": gate,
        "regime_cfg": regime_cfg,
        "capital": args.capital,
        "n_resamples": args.bootstrap_resamples,
        "ci": args.ci,
        "seed": args.seed,
    }


def main(argv: Optional[List[str]] = None) -> int:
    args = build_parser().parse_args(argv)
    global INTRABAR_RESOLUTION
    INTRABAR_RESOLUTION = args.intrabar_resolution
    spec = _resolve_spec(args)

    if args.windows:
        window_names = [w.strip() for w in args.windows.split(",") if w.strip()]
        unknown = [w for w in window_names if w not in WINDOWS]
        if unknown:
            raise SystemExit(f"unknown windows {unknown}; known: {list(WINDOWS)}")
    else:
        window_names = ["is", "oos"]

    if args.datasets:
        datasets = [parse_dataset_arg(d) for d in args.datasets.split(",") if d.strip()]
    else:
        datasets = list(DATASETS)

    from registry_loader import load_registry
    reg = load_registry(args.registry)

    print(f"open: {spec['open_name']} (params: {spec['params'] or 'registry defaults'}, "
          f"registry: {args.registry}, direction: {spec['direction']})")
    print(f"incumbent close: {spec['incumbent_close'] or 'open-as-close (signal reversal)'}")
    if spec.get("candidate_stop_only"):
        print("candidate close: none (stop-only: the candidate stop is the whole exit; "
              "no signal-reversal exit)")
    else:
        print(f"candidate close: "
              f"{spec['candidate_close'] or 'open-as-close (signal reversal)'}")
    ctrl_stops = spec.get("control_stops") or {}
    show_candidate = ctrl_stops or spec.get("candidate_stops_selection")
    print(f"incumbent stops (control arm): {ctrl_stops or 'none'}"
          + (f"  |  candidate arm stops: {spec['candidate_stops_mode']} "
             f"({spec.get('candidate_stops') or 'none'})" if show_candidate else ""))
    print(f"stop owners: control={spec.get('control_stop_owner')} "
          f"candidate={spec.get('candidate_stop_owner')} (stop_units: engine_fraction; "
          f"ATR multipliers are unitless)")
    print(f"regime: classifier={spec['regime_cfg']['classifier']}"
          + (f", gate={spec['gate']['allowed_regimes']}" if spec['gate']['allowed_regimes']
             else ", gate=none (attribution only)"))
    if not spec["replayable"]:
        print("[WARN] candidate exit is not per-entry replayable (signal-reversal): "
              "paired analysis is unavailable; reporting the unpaired view only.",
              file=sys.stderr)

    per_window: "OrderedDict[str, List[dict]]" = OrderedDict()
    for wname in window_names:
        window = WINDOWS[wname]
        results = []
        for symbol, timeframe in datasets:
            res = evaluate_dataset_window(reg, spec, symbol, timeframe, window)
            if res is not None:
                results.append(res)
        per_window[wname] = results
        print(format_window_report(wname, window, results))

    print(format_summary(per_window))
    from backtester import aggregate_close_validations, format_close_validation
    print(format_close_validation(aggregate_close_validations(
        v for results in per_window.values() for d in results
        for v in (d["close_validation"]["control"], d["close_validation"]["candidate"]))))

    if args.json_out:
        payload = {
            "open": {"name": spec["open_name"], "params": spec["params"],
                     "direction": spec["direction"]},
            "incumbent_close": spec["incumbent_close"],
            "candidate_close": spec["candidate_close"],
            "control_stops": _stops_for_json(spec.get("control_stops")),
            "candidate_stops": _stops_for_json(spec.get("candidate_stops")),
            "stop_units": "engine_fraction",
            "candidate_stops_mode": spec.get("candidate_stops_mode"),
            "candidate_stops_selection": spec.get("candidate_stops_selection"),
            "candidate_stop_only": bool(spec.get("candidate_stop_only")),
            "control_stop_owner": spec.get("control_stop_owner"),
            "candidate_stop_owner": spec.get("candidate_stop_owner"),
            "replayable": spec["replayable"],
            "comparison_mode": spec.get("comparison_mode"),
            "close_validation": aggregate_close_validations(
                v for results in per_window.values() for d in results
                for v in (d["close_validation"]["control"],
                          d["close_validation"]["candidate"])),
            "regime_cfg": spec["regime_cfg"],
            "gate_allowed_regimes": spec["gate"]["allowed_regimes"],
            "registry": args.registry,
            "intrabar_resolution": INTRABAR_RESOLUTION,
            "windows": {w: list(WINDOWS[w]) for w in window_names},
            "datasets": [dataset_key(s, t) for s, t in datasets],
            "bootstrap": {"n_resamples": spec["n_resamples"], "ci": spec["ci"],
                          "seed": spec["seed"]},
            "results": per_window,
        }
        with open(args.json_out, "w") as fh:
            json.dump(payload, fh, indent=2, default=str)
        print(f"\nwrote {args.json_out}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
