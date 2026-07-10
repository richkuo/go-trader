#!/usr/bin/env python3
"""Holding-time / excursion diagnostics for exit-quality refinement (#997 M3).

Step 1 of methodology M3 in one command: *where does a strategy's PnL die* —
early reversal (loser runs straight against the entry), late giveback (winner
peaks then bleeds back), or fee churn on the exit leg? The answer points at the
mechanism to try next (atr_stop / time_stop / zscore_target), instead of a
blind close-param sweep.

It runs the SAME audit-identical harness as the M1 scorer: it imports the
versioned ``WINDOWS`` / ``DATASETS`` / ``FEE_PLATFORM`` from ``eval_windows.py`` so
diagnosis and scoring see byte-identical data slices, then reads the per-trade
hold telemetry the backtester stamps (#997: ``bars_held``, ``mfe_pct`` /
``mae_pct`` excursions, ``entry_fee`` / ``exit_fee``, ``exit_reason``).

Usage:
  uv run --no-sync python backtest/exit_diagnostics.py --strategy ichimoku_cloud
  uv run --no-sync python backtest/exit_diagnostics.py --strategy ichimoku_cloud \
      --registry spot --windows is,oos --json /tmp/diag.json
  # diagnose a candidate WITH close refs (use --direction long to keep it
  # long-only; the open/close engine opens shorts on signal=-1 otherwise):
  uv run --no-sync python backtest/exit_diagnostics.py --strategy ichimoku_cloud \
      --close-strategies '[{"name":"atr_stop","params":{"atr_mult":2.5}}]' \
      --direction long

All aggregation logic is pure (operates on lists of trade dicts) so it is unit
tested without data access — same architecture as eval_windows.py.
"""

from __future__ import annotations

import argparse
import json
import math
import os
import statistics
import sys
from typing import List, Optional

_THIS_DIR = os.path.dirname(os.path.abspath(__file__))
if _THIS_DIR not in sys.path:
    sys.path.insert(0, _THIS_DIR)

from eval_windows import (  # noqa: E402  (path bootstrap above)
    DATASETS,
    DEFAULT_CAPITAL,
    FEE_PLATFORM,
    WINDOWS,
    dataset_key,
    parse_dataset_arg,
)

# --------------------------------------------------------------------------
# Tunable classification thresholds (documented; not magic numbers).
# --------------------------------------------------------------------------

# Holding-time buckets (inclusive bar-count ranges).
HOLD_BUCKETS = [(1, 1), (2, 5), (6, 20), (21, 50), (51, math.inf)]

# A favourable excursion below this (percent) is "never meaningfully in profit".
MFE_MIN_PCT = 1.0
# late_giveback: captured at most this fraction of the peak favourable move.
CAPTURE_FRAC = 0.5
# early_reversal: a loser whose peak favourable move stayed under this (percent).
EARLY_MFE_MAX_PCT = 0.5


# --------------------------------------------------------------------------
# Pure per-trade + aggregation helpers (unit-tested without data access).
# --------------------------------------------------------------------------

def trade_metrics(t: dict) -> dict:
    """Derive gross/net/fee fractions + excursions from a stamped trade dict.

    ``pnl_pct`` / ``mfe_pct`` / ``mae_pct`` are already percent (Trade.to_dict).
    Fees are absolute; convert to percent of this leg's notional using shares
    and prices so the result is independent of which run path booked the leg
    (Trade.pnl is net of both fees at every close site since #1241, but pnl_pct
    stays gross on all paths, so we key off pnl_pct and re-derive fees here).
    """
    gross_pct = float(t.get("pnl_pct", 0.0) or 0.0)
    shares = float(t.get("shares", 0.0) or 0.0)
    entry_price = float(t.get("entry_price", 0.0) or 0.0)
    exit_price = float(t.get("exit_price", 0.0) or 0.0)
    entry_fee = float(t.get("entry_fee", 0.0) or 0.0)
    exit_fee = float(t.get("exit_fee", 0.0) or 0.0)
    entry_notional = shares * entry_price
    exit_notional = shares * exit_price
    fee_pct = 0.0
    if entry_notional > 0:
        fee_pct += entry_fee / entry_notional * 100.0
    if exit_notional > 0:
        fee_pct += exit_fee / exit_notional * 100.0
    net_pct = gross_pct - fee_pct

    mfe_pct = float(t.get("mfe_pct", 0.0) or 0.0)
    mae_pct = float(t.get("mae_pct", 0.0) or 0.0)
    entry_atr = float(t.get("entry_atr", 0.0) or 0.0)
    mae_atr = 0.0
    mfe_atr = 0.0
    if entry_atr > 0 and entry_price > 0:
        mae_atr = abs(mae_pct / 100.0 * entry_price) / entry_atr
        mfe_atr = abs(mfe_pct / 100.0 * entry_price) / entry_atr
    return {
        "gross_pct": gross_pct,
        "net_pct": net_pct,
        "fee_pct": fee_pct,
        "bars_held": int(t.get("bars_held", 0) or 0),
        "mfe_pct": mfe_pct,
        "mae_pct": mae_pct,
        "mfe_atr": mfe_atr,
        "mae_atr": mae_atr,
        "bars_to_mfe": int(t.get("bars_to_mfe", 0) or 0),
        "bars_to_mae": int(t.get("bars_to_mae", 0) or 0),
        "giveback_pct": mfe_pct - gross_pct,
        "exit_reason": str(t.get("exit_reason", "") or ""),
    }


def _bucket_label(bars: int) -> str:
    for lo, hi in HOLD_BUCKETS:
        if lo <= bars <= hi:
            return f"{lo}+" if hi == math.inf else (f"{lo}" if lo == hi else f"{lo}-{hi}")
    return "0"


def _median(xs):
    xs = [x for x in xs if x is not None]
    return round(statistics.median(xs), 4) if xs else None


def _mean(xs):
    xs = [x for x in xs if x is not None]
    return round(statistics.mean(xs), 4) if xs else None


def holding_time_summary(metrics: List[dict]) -> dict:
    """Bars-held distribution + PnL bucketed by holding time."""
    if not metrics:
        return {"trades": 0, "buckets": [], "bars_held": {}}
    bars = [m["bars_held"] for m in metrics]
    quant = {
        "min": min(bars), "max": max(bars), "mean": _mean(bars),
        "median": _median(bars),
        "p25": round(_pct(bars, 25), 2), "p75": round(_pct(bars, 75), 2),
        "p90": round(_pct(bars, 90), 2),
    }
    buckets = []
    for lo, hi in HOLD_BUCKETS:
        members = [m for m in metrics if lo <= m["bars_held"] <= hi]
        if not members:
            continue
        nets = [m["net_pct"] for m in members]
        buckets.append({
            "bucket": f"{lo}+" if hi == math.inf else (f"{lo}" if lo == hi else f"{lo}-{hi}"),
            "count": len(members),
            "win_rate": round(sum(1 for x in nets if x > 0) / len(members), 3),
            "mean_net_pct": _mean(nets),
            "total_net_pct": round(sum(nets), 2),
            "mean_gross_pct": _mean([m["gross_pct"] for m in members]),
        })
    return {"trades": len(metrics), "bars_held": quant, "buckets": buckets}


def _pct(xs, q):
    """Linear-interpolation percentile (q in 0..100); xs non-empty."""
    s = sorted(xs)
    if len(s) == 1:
        return float(s[0])
    rank = (q / 100.0) * (len(s) - 1)
    lo = math.floor(rank)
    hi = math.ceil(rank)
    if lo == hi:
        return float(s[lo])
    return float(s[lo] + (s[hi] - s[lo]) * (rank - lo))


def excursion_summary(metrics: List[dict]) -> dict:
    """MFE/MAE excursion profile, aggregate + per holding-time bucket.

    MAE/MFE are reported in ATR multiples too — read the atr_stop knob straight
    off ``mae_atr`` percentiles (set the stop just beyond winners' typical MAE).
    """
    if not metrics:
        return {"aggregate": {}, "buckets": []}
    winners = [m for m in metrics if m["net_pct"] > 0]

    def _profile(ms):
        return {
            "count": len(ms),
            "median_mfe_pct": _median([m["mfe_pct"] for m in ms]),
            "median_mae_pct": _median([m["mae_pct"] for m in ms]),
            "median_mae_atr": _median([m["mae_atr"] for m in ms]),
            "p80_mae_atr": round(_pct([m["mae_atr"] for m in ms], 80), 3) if ms else None,
            "p90_mae_atr": round(_pct([m["mae_atr"] for m in ms], 90), 3) if ms else None,
            "median_giveback_pct": _median([m["giveback_pct"] for m in ms]),
            "median_bars_to_mfe": _median([m["bars_to_mfe"] for m in ms]),
            "median_bars_to_mae": _median([m["bars_to_mae"] for m in ms]),
        }

    buckets = []
    for lo, hi in HOLD_BUCKETS:
        members = [m for m in metrics if lo <= m["bars_held"] <= hi]
        if members:
            prof = _profile(members)
            prof["bucket"] = f"{lo}+" if hi == math.inf else (f"{lo}" if lo == hi else f"{lo}-{hi}")
            buckets.append(prof)
    return {
        "aggregate": _profile(metrics),
        "winners_only": _profile(winners),
        "buckets": buckets,
    }


def classify_bleed_modes(metrics: List[dict]) -> dict:
    """Tag each trade with the dominant bleed mode → where the PnL dies.

    Priority (first match wins):
      fee_churn      — fees flipped a gross win to a net loss, or the gross
                       price move was smaller than the round-trip fee.
      late_giveback  — peaked >= MFE_MIN_PCT favourable then captured <=
                       CAPTURE_FRAC of it (gave the profit back). → time_stop.
      early_reversal — a net loser that never got meaningfully favourable
                       (MFE < EARLY_MFE_MAX_PCT): ran against from the open.
                       → atr_stop.
      clean_win / clean_loss — the residual.
    """
    counts: dict = {}
    for m in metrics:
        gross, net, fee = m["gross_pct"], m["net_pct"], m["fee_pct"]
        mfe = m["mfe_pct"]
        if (gross > 0 and net <= 0) or (abs(gross) < fee):
            mode = "fee_churn"
        elif mfe >= MFE_MIN_PCT and gross <= mfe * CAPTURE_FRAC:
            mode = "late_giveback"
        elif net < 0 and mfe < EARLY_MFE_MAX_PCT:
            mode = "early_reversal"
        elif net > 0:
            mode = "clean_win"
        else:
            mode = "clean_loss"
        bucket = counts.setdefault(mode, {"count": 0, "total_net_pct": 0.0})
        bucket["count"] += 1
        bucket["total_net_pct"] += net

    n = len(metrics) or 1
    out = {}
    for mode, b in counts.items():
        out[mode] = {
            "count": b["count"],
            "share": round(b["count"] / n, 3),
            "total_net_pct": round(b["total_net_pct"], 2),
            "mean_net_pct": round(b["total_net_pct"] / b["count"], 4),
        }
    return out


def fee_churn_summary(metrics: List[dict]) -> dict:
    """Exit-leg fee-drag read + exit-reason tally."""
    if not metrics:
        return {"trades": 0}
    fees = [m["fee_pct"] for m in metrics]
    gross_abs = [abs(m["gross_pct"]) for m in metrics]
    flipped = sum(1 for m in metrics if m["gross_pct"] > 0 and m["net_pct"] <= 0)
    dominated = sum(1 for m in metrics if abs(m["gross_pct"]) < m["fee_pct"])
    total_gross = sum(gross_abs) or 1.0
    reasons: dict = {}
    for m in metrics:
        reasons[m["exit_reason"]] = reasons.get(m["exit_reason"], 0) + 1
    return {
        "trades": len(metrics),
        "mean_fee_pct": _mean(fees),
        "fee_drag_ratio": round(sum(fees) / total_gross, 4),
        "trades_flipped_to_loss_by_fees": flipped,
        "trades_fee_dominated": dominated,
        "exit_reasons": dict(sorted(reasons.items())),
    }


def diagnose_trades(trades: List[dict]) -> dict:
    """Full diagnostic for one set of trade dicts."""
    metrics = [trade_metrics(t) for t in trades]
    return {
        "holding_time": holding_time_summary(metrics),
        "excursion": excursion_summary(metrics),
        "bleed_modes": classify_bleed_modes(metrics),
        "fee_churn": fee_churn_summary(metrics),
    }


# --------------------------------------------------------------------------
# Leg execution (I/O; everything above stays pure).
# --------------------------------------------------------------------------

def run_leg_trades(reg, name: str, params: Optional[dict], symbol: str,
                   timeframe: str, window: tuple, capital: float,
                   close_strategies: Optional[List[dict]] = None,
                   direction: Optional[str] = None,
                   invert_signal: bool = False) -> Optional[List[dict]]:
    """One (strategy, dataset, window) leg → its list of trade dicts.

    Mirrors eval_windows.run_leg's harness construction exactly (audit-identical
    fees/data) but returns the full per-trade telemetry instead of collapsed
    metrics.
    """
    from atr import ensure_atr_indicator
    from data_fetcher import load_cached_data
    from backtester import Backtester
    from run_backtest import FUNDING_COLUMN_STRATEGIES, _attach_funding_if_needed

    start, end = window
    df = load_cached_data(symbol, timeframe, start_date=start, end_date=end)
    if df.empty:
        return None
    if name in FUNDING_COLUMN_STRATEGIES:
        df = _attach_funding_if_needed(df, name, symbol, start)

    strat = reg.STRATEGY_REGISTRY.get(name)
    if strat is None:
        raise SystemExit(f"Unknown strategy {name!r}; available: {reg.list_strategies()}")
    strat_params = params if params is not None else strat["default_params"]

    df_signals = reg.apply_strategy(name, df, strat_params)
    # Always inject ATR so entry_atr telemetry (and MAE-in-ATR readout) is
    # populated even for strategies that emit no `atr` column. This is
    # diagnostics-only: with no ATR stop/close configured the backtester reads
    # the series solely to stamp Trade.entry_atr, so trade behaviour — and any
    # downstream M1 score, which runs through eval_windows, not here — is
    # unchanged.
    df_signals = ensure_atr_indicator(df_signals)

    bt = Backtester(
        initial_capital=capital, platform=FEE_PLATFORM,
        open_strategy={"name": name, "params": dict(strat_params or {})},
        close_strategies=close_strategies,
        direction=direction, invert_signal=invert_signal,
    )
    results = bt.run(df_signals, strategy_name=name, symbol=symbol,
                     timeframe=timeframe, params=strat_params, save=False)
    return results.get("trades", [])


def build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(
        description="Exit-quality holding-time / excursion diagnostics (#997 M3)")
    p.add_argument("--strategy", required=True, help="Open-strategy name")
    p.add_argument("--params", default=None, help="Open params JSON (default: registry defaults)")
    p.add_argument("--registry", choices=["spot", "futures"], default="spot")
    p.add_argument("--windows", default=None,
                   help=f"Comma list of windows (default: is,oos). Known: {', '.join(WINDOWS)}")
    p.add_argument("--datasets", default=None,
                   help="Comma list of SYMBOL:TIMEFRAME (default: the six audit datasets)")
    p.add_argument("--capital", type=float, default=DEFAULT_CAPITAL)
    p.add_argument("--close-strategies", default=None,
                   help="Close refs JSON to diagnose WITH exits applied")
    p.add_argument("--direction", default=None, choices=["long", "short", "both"],
                   help="Open-side gate (set 'long' when using --close-strategies "
                        "to stay long-only; the open/close engine opens shorts otherwise)")
    p.add_argument("--invert-signal", action="store_true")
    p.add_argument("--json", default=None, dest="json_out",
                   help="Write the full structured diagnostic to this path")
    return p


def _fmt_bleed(bleed: dict) -> str:
    order = ["early_reversal", "late_giveback", "fee_churn", "clean_win", "clean_loss"]
    lines = []
    for mode in order:
        if mode in bleed:
            b = bleed[mode]
            lines.append(f"    {mode:14s} {b['count']:5d} ({b['share']*100:5.1f}%)  "
                         f"net {b['total_net_pct']:+8.2f}%  (mean {b['mean_net_pct']:+.3f}%)")
    return "\n".join(lines) or "    (no trades)"


def format_report(per_window: dict) -> str:
    out = []
    for wname, datasets in per_window.items():
        out.append(f"\n=== window: {wname} ===")
        for ds, diag in datasets.items():
            ht = diag["holding_time"]
            if ht["trades"] == 0:
                out.append(f"  {ds}: 0 trades")
                continue
            bh = ht["bars_held"]
            fc = diag["fee_churn"]
            out.append(f"  {ds}: {ht['trades']} trades  "
                       f"bars_held median {bh['median']} (p90 {bh['p90']})  "
                       f"fee_drag {fc['fee_drag_ratio']}  "
                       f"flipped_by_fees {fc['trades_flipped_to_loss_by_fees']}")
            out.append("    holding-time buckets (net%):")
            for b in ht["buckets"]:
                out.append(f"      {b['bucket']:7s} n={b['count']:4d}  win {b['win_rate']*100:5.1f}%  "
                           f"total {b['total_net_pct']:+8.2f}%  mean {b['mean_net_pct']:+.3f}%")
            agg = diag["excursion"]["aggregate"]
            win = diag["excursion"]["winners_only"]
            out.append(f"    excursion: median MFE {agg['median_mfe_pct']}%  "
                       f"MAE {agg['median_mae_pct']}% ({agg['median_mae_atr']} ATR, "
                       f"p80 {agg['p80_mae_atr']} ATR)  giveback {agg['median_giveback_pct']}%")
            out.append(f"    winners({win['count']}): MFE {win['median_mfe_pct']}%  "
                       f"giveback {win['median_giveback_pct']}%  bars_to_MFE {win['median_bars_to_mfe']}")
            out.append("    bleed modes (where the PnL dies):")
            out.append(_fmt_bleed(diag["bleed_modes"]))
    return "\n".join(out)


def main(argv: Optional[List[str]] = None) -> int:
    args = build_parser().parse_args(argv)

    close_strategies = None
    if args.close_strategies:
        close_strategies = json.loads(args.close_strategies)
        if not isinstance(close_strategies, list):
            raise SystemExit("--close-strategies must be a JSON list of refs")
    if close_strategies and args.direction is None:
        print("[WARN] --close-strategies set without --direction: the open/close "
              "engine opens shorts on signal=-1. Pass --direction long for a "
              "long-only diagnostic.", file=sys.stderr)

    params = json.loads(args.params) if args.params else None

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

    per_window: dict = {}
    for wname in window_names:
        window = WINDOWS[wname]
        per_window[wname] = {}
        for symbol, timeframe in datasets:
            ds = dataset_key(symbol, timeframe)
            trades = run_leg_trades(
                reg, args.strategy, params, symbol, timeframe, window,
                args.capital, close_strategies=close_strategies,
                direction=args.direction, invert_signal=args.invert_signal,
            )
            per_window[wname][ds] = diagnose_trades(trades or [])

    print(f"strategy: {args.strategy}  registry: {args.registry}  "
          f"close: {close_strategies or 'none'}  direction: {args.direction or 'default'}")
    print(format_report(per_window))

    if args.json_out:
        payload = {
            "strategy": args.strategy,
            "registry": args.registry,
            "params": params,
            "close_strategies": close_strategies,
            "direction": args.direction,
            "windows": per_window,
        }
        with open(args.json_out, "w") as fh:
            json.dump(payload, fh, indent=2, default=float)
        print(f"\nwrote {args.json_out}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
