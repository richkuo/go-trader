#!/usr/bin/env python3
"""gross_edge_noise.py — M1 step-2 sample-noise adjudicator for `graduate_m1`
fee-audit verdicts (#1054).

The M5 screen (#999, fee_audit.py) graduates a strategy when its mean per-leg
GROSS return is positive while net is not — "a real edge exists under the
churn; raise selectivity". On a thin trade sample that premise itself needs
adjudication: a +0.27%/leg gross mean over 37 trades can be indistinguishable
from zero, in which case there is no edge to salvage and the honest M1 verdict
is deprecate, not mechanism work. This tool answers exactly that question,
BEFORE any selectivity effort is spent.

For each (window x dataset) it re-runs the fee audit's zero-friction gross leg
(``eval_windows.run_leg`` with ``commission_pct=0.0, slippage_pct=0.0`` — the
identical harness and therefore the identical trade universe) and pools the
per-trade gross returns. On the pooled sample it reports:

  - a one-sided SIGN-FLIP PERMUTATION test on the mean (the pre-registered
    primary test: under H0 "no edge, wins and losses of a given magnitude
    equally likely", each return's sign is a fair coin; p = share of sign-flip
    resamples whose mean >= the observed mean, add-one smoothed);
  - a seeded percentile BOOTSTRAP CI on the mean (exit_policy_ab.bootstrap_ci)
    plus the bootstrap P(mean <= 0);
  - the exact two-sided SIGN TEST and the tie-corrected WILCOXON signed-rank
    (both reused from exit_policy_ab) as supporting views;

and the same permutation/bootstrap pair over the per-LEG gross returns — the
per-leg mean is literally the M5 screen statistic being adjudicated.

The verdict keys off the primary test alone: ``distinguishable_positive``
needs permutation p < alpha (default 0.05); a non-positive pooled mean is
``no_positive_edge``; anything else is ``indistinguishable_from_zero``.
Supporting views are reported, never blended into the verdict — one
pre-registered test, no p-hacking across four.

Overlapping windows (e.g. ``is`` and ``2025H1`` share 2025-06-10→07-01) would
double-count the overlap period in the pooled sample — and NOT as byte-equal
duplicates: each leg's indicators warm up from its own window start, so the
same calendar period fires *non-identical* entries across windows and an
exact (dataset, entry_date) key would drop nothing. The trade-level pool
therefore dedupes by CALENDAR COVERAGE: per dataset, the first window pooled
claims its full [start, end) range, and any later-pooled trade whose entry
falls inside an already-claimed range is dropped (first-window-wins, order =
the --windows order); exact-duplicate keys are dropped too. Both dropped
counts are reported, never silent. Per-LEG returns are atomic (one compounded
number per whole window), so the leg-level pool cannot be partially deduped —
instead any pairwise window overlap is computed and disclosed alongside the
leg-level stats.

All statistics are stdlib-only and deterministic under ``--seed`` (same
conventions as exit_policy_ab.py); the pure helpers are unit-tested without
data access in backtest/tests/test_gross_edge_noise.py.

Usage:
  # Adjudicate the M5 verdict slices (the screen's own is+oos pair)
  uv run --no-sync python backtest/gross_edge_noise.py \\
      --strategy regime_adaptive_htf --registry spot

  # Wider pooled sample across every M1 window (held-out included)
  uv run --no-sync python backtest/gross_edge_noise.py \\
      --strategy regime_adaptive_htf --windows is,oos,2023,2024,2025H1

  # Short leg of a short-only strategy
  uv run --no-sync python backtest/gross_edge_noise.py \\
      --strategy bear_pullback_st --registry futures --direction short
"""

from __future__ import annotations

import argparse
import json
import os
import statistics
import sys
from random import Random
from typing import List, Optional, Sequence

_THIS_DIR = os.path.dirname(os.path.abspath(__file__))
if _THIS_DIR not in sys.path:
    sys.path.insert(0, _THIS_DIR)
sys.path.insert(0, os.path.join(_THIS_DIR, "..", "shared_tools"))

from eval_windows import (  # noqa: E402  (path bootstrap above)
    DATASETS,
    DEFAULT_CAPITAL,
    WINDOWS,
    dataset_key,
    parse_dataset_arg,
    run_leg,
)
from exit_policy_ab import (  # noqa: E402
    DEFAULT_BOOTSTRAP_RESAMPLES,
    DEFAULT_SEED,
    bootstrap_ci,
    sign_test,
    wilcoxon_signed_rank,
)

DEFAULT_WINDOWS = ("is", "oos")  # the M5 screen's own slices (#999)
DEFAULT_ALPHA = 0.05

VERDICT_DISTINGUISHABLE = "distinguishable_positive"
VERDICT_INDISTINGUISHABLE = "indistinguishable_from_zero"
VERDICT_NO_EDGE = "no_positive_edge"


# ---------------------------------------------------------------------------
# Pure statistics (stdlib only; deterministic; unit-tested without data).
# ---------------------------------------------------------------------------

def summarize_returns(values: Sequence[float], zero_tol: float = 1e-12) -> dict:
    """Descriptive summary of a return sample (percent units throughout)."""
    n = len(values)
    if n == 0:
        return {"n": 0, "mean": None, "median": None, "min": None,
                "max": None, "n_pos": 0, "n_neg": 0, "n_zero": 0}
    return {
        "n": n,
        "mean": round(statistics.fmean(values), 6),
        "median": round(statistics.median(values), 6),
        "min": round(min(values), 6),
        "max": round(max(values), 6),
        "n_pos": sum(1 for v in values if v > zero_tol),
        "n_neg": sum(1 for v in values if v < -zero_tol),
        "n_zero": sum(1 for v in values
                      if -zero_tol <= v <= zero_tol),
    }


def sign_flip_permutation(values: Sequence[float],
                          n_resamples: int = DEFAULT_BOOTSTRAP_RESAMPLES,
                          seed: int = DEFAULT_SEED) -> dict:
    """One-sided sign-flip permutation test on the sample mean.

    H0: returns are symmetric about zero (no directional edge) — each value's
    sign is a fair coin flip. ``p_value`` is the one-sided
    P(resampled mean >= observed mean), add-one smoothed
    ((#exceedances + 1) / (n_resamples + 1)) so a reported p is never exactly
    0 off a finite resample count. Empty sample → p 1.0 (nothing to test).
    """
    n = len(values)
    if n == 0:
        return {"n": 0, "mean": None, "p_value": 1.0, "n_resamples": 0}
    observed = statistics.fmean(values)
    rng = Random(seed)
    exceed = 0
    for _ in range(n_resamples):
        total = 0.0
        for v in values:
            total += v if rng.random() < 0.5 else -v
        if total / n >= observed:
            exceed += 1
    return {
        "n": n,
        "mean": round(observed, 6),
        "p_value": round((exceed + 1) / (n_resamples + 1), 6),
        "n_resamples": n_resamples,
    }


def bootstrap_p_mean_le_zero(values: Sequence[float],
                             n_resamples: int = DEFAULT_BOOTSTRAP_RESAMPLES,
                             seed: int = DEFAULT_SEED) -> Optional[float]:
    """Bootstrap P(resampled mean <= 0) — the CI view collapsed to one number.

    Same index-resampling scheme as exit_policy_ab.bootstrap_ci (kept separate
    because that helper returns only the interval endpoints). None on an
    empty sample; on a single-value sample the point estimate decides.
    """
    n = len(values)
    if n == 0:
        return None
    if n < 2:
        return 1.0 if values[0] <= 0 else 0.0
    rng = Random(seed)
    le_zero = 0
    for _ in range(n_resamples):
        total = 0.0
        for _k in range(n):
            total += values[rng.randrange(n)]
        if total / n <= 0.0:
            le_zero += 1
    return round(le_zero / n_resamples, 6)


def _entry_in_range(entry_date: str, window_range: tuple) -> bool:
    """Is an ISO ``entry_date`` inside a window's [start, end) calendar range?

    Comparison is lexicographic, valid for zero-padded ISO timestamps (the
    Backtester stamps ``str(pd.Timestamp)``); ``end=None`` means open-ended.
    A bare-date bound compares correctly against a full timestamp: a start
    of "2025-06-10" precedes "2025-06-10 00:00:00", and an end of
    "2025-07-01" excludes "2025-07-01 00:00:00".
    """
    start, end = window_range
    if start and entry_date < start:
        return False
    if end and entry_date >= end:
        return False
    return True


def dedupe_samples(samples: List[dict]) -> tuple:
    """Drop pooled trades that are the same physical entry counted twice.

    Key = (dataset, entry_date) — the byte-identical replay case (e.g. the
    same window listed twice). The real overlap case (per-window warmup
    makes overlap entries NON-identical, so this key never collides) is
    handled by the calendar-coverage guard in ``pool_trade_samples``, which
    needs leg structure this flat view lacks. Returns
    (deduped_list, n_dropped); order is preserved (first occurrence wins).
    """
    seen = set()
    out = []
    dropped = 0
    for s in samples:
        key = (s.get("dataset"), s.get("entry_date"))
        if key in seen:
            dropped += 1
            continue
        seen.add(key)
        out.append(s)
    return out, dropped


def window_overlaps(window_names: List[str],
                    windows: Optional[dict] = None) -> List[dict]:
    """Pairwise calendar overlaps among the requested windows (leg-level
    disclosure: a leg's return is one atomic number per window, so an
    overlap cannot be partially deduped out of the leg pool — it is
    reported instead).

    Returns [{"windows": (a, b), "start": ..., "end": ..., "days": float}]
    for every pair whose [start, end) ranges intersect; ``end=None`` is
    open-ended. Dates are ISO strings; days is the intersection length.
    """
    from datetime import datetime

    if windows is None:
        windows = WINDOWS

    def _parse(bound: Optional[str], default: str) -> datetime:
        if bound is None:
            return datetime.fromisoformat(default)
        return datetime.fromisoformat(bound[:19])

    out = []
    far_future = "9999-01-01"
    for i, a in enumerate(window_names):
        for b in window_names[i + 1:]:
            a_start = _parse(windows[a][0], "0001-01-01")
            a_end = _parse(windows[a][1], far_future)
            b_start = _parse(windows[b][0], "0001-01-01")
            b_end = _parse(windows[b][1], far_future)
            lo = max(a_start, b_start)
            hi = min(a_end, b_end)
            if lo < hi:
                out.append({
                    "windows": (a, b),
                    "start": lo.date().isoformat(),
                    "end": hi.date().isoformat(),
                    "days": round((hi - lo).total_seconds() / 86400.0, 2),
                })
    return out


def noise_verdict(mean: Optional[float], permutation_p: float,
                  alpha: float = DEFAULT_ALPHA) -> str:
    """Verdict off the pre-registered primary test (see module docstring)."""
    if mean is None or mean <= 0:
        return VERDICT_NO_EDGE
    if permutation_p < alpha:
        return VERDICT_DISTINGUISHABLE
    return VERDICT_INDISTINGUISHABLE


def analyze_sample(values: Sequence[float],
                   n_resamples: int = DEFAULT_BOOTSTRAP_RESAMPLES,
                   seed: int = DEFAULT_SEED,
                   alpha: float = DEFAULT_ALPHA) -> dict:
    """Full stats block for one pooled return sample (trade- or leg-level)."""
    perm = sign_flip_permutation(values, n_resamples=n_resamples, seed=seed)
    boot = bootstrap_ci(list(values), n_resamples=n_resamples, seed=seed)
    return {
        "summary": summarize_returns(values),
        "permutation": perm,
        "bootstrap": boot,
        "bootstrap_p_mean_le_zero": bootstrap_p_mean_le_zero(
            values, n_resamples=n_resamples, seed=seed),
        "sign_test": sign_test(list(values)),
        "wilcoxon": wilcoxon_signed_rank(list(values)),
        "alpha": alpha,
        "verdict": noise_verdict(perm["mean"], perm["p_value"], alpha),
    }


# ---------------------------------------------------------------------------
# Leg execution (I/O; everything above stays pure).
# ---------------------------------------------------------------------------

def collect_gross_legs(reg, name: str, params: Optional[dict],
                       datasets: List[tuple], window_names: List[str],
                       capital: float = DEFAULT_CAPITAL,
                       direction: Optional[str] = None) -> List[dict]:
    """Zero-friction gross legs with per-trade samples attached.

    Identical harness to fee_audit.py's gross run (run_leg with commission
    and slippage zeroed), so the trade universe is the one the M5 verdict
    was computed on. Legs with no data are skipped (reported by the caller
    via the leg count); a raising leg propagates — a noise verdict computed
    on a silently partial sample would overstate its own coverage.
    """
    legs = []
    for wname in window_names:
        window = WINDOWS[wname]
        for symbol, timeframe in datasets:
            leg = run_leg(reg, name, params, symbol, timeframe, window,
                          capital=capital, direction=direction,
                          commission_pct=0.0, slippage_pct=0.0,
                          keep_trades=True)
            if leg is None:
                continue
            leg["window"] = wname
            leg["dataset"] = dataset_key(symbol, timeframe)
            legs.append(leg)
    return legs


def pool_trade_samples(legs: List[dict],
                       windows: Optional[dict] = None) -> tuple:
    """Pool per-trade samples across legs, counting each calendar period once.

    Two first-window-wins guards (pooling order = the --windows order):

    1. exact key (dataset, entry_date) — byte-identical replays;
    2. calendar coverage — per dataset, every leg already pooled claims its
       window's full [start, end) range (claimed even when the leg fired no
       trades: the strategy sampled that period and chose not to enter). A
       later leg from a different window drops any trade whose entry falls
       inside a claimed range, even though warmup divergence means its
       timestamp never collides with the first window's trades — the real
       overlap case an exact key cannot catch (``is`` ∩ ``2025H1`` fire
       non-identical entries over 2025-06-10→07-01).

    ``windows`` defaults to the harness WINDOWS map (injectable for tests).
    Returns (samples, n_exact_dropped, n_overlap_dropped); both drop counts
    are surfaced by the caller, never silent.
    """
    if windows is None:
        windows = WINDOWS
    covered: dict = {}  # dataset -> [(window_name, (start, end)), ...]
    seen = set()
    pooled = []
    dropped_exact = 0
    dropped_overlap = 0
    for leg in legs:
        ds = leg["dataset"]
        wname = leg["window"]
        wrange = windows[wname]
        claimed = covered.setdefault(ds, [])
        for s in leg.get("trade_samples") or []:
            key = (ds, s["entry_date"])
            if key in seen:
                dropped_exact += 1
                continue
            if any(w != wname and _entry_in_range(s["entry_date"], r)
                   for w, r in claimed):
                dropped_overlap += 1
                continue
            seen.add(key)
            pooled.append({"dataset": ds, "window": wname,
                           "entry_date": s["entry_date"],
                           "pnl_pct": s["pnl_pct"]})
        if all(w != wname for w, _ in claimed):
            claimed.append((wname, wrange))
    return pooled, dropped_exact, dropped_overlap


# ---------------------------------------------------------------------------
# Reporting / CLI.
# ---------------------------------------------------------------------------

def _fmt(v, prec=3):
    return "-" if v is None else f"{v:+.{prec}f}"


def format_report(name: str, registry: str, window_names: List[str],
                  legs: List[dict], n_exact_dropped: int,
                  n_overlap_dropped: int, overlaps: List[dict],
                  trade_stats: dict, leg_stats: dict) -> str:
    lines = [
        f"gross-edge noise check: {name} ({registry} registry, "
        f"windows: {', '.join(window_names)}; friction zeroed)",
        "",
        f"{'window':<8} {'dataset':<14} {'trades':>6} {'leg gross %':>12}",
    ]
    for leg in legs:
        lines.append(f"{leg['window']:<8} {leg['dataset']:<14} "
                     f"{leg['trades']:>6} {_fmt(leg['return_pct'], 2):>12}")

    ts, ls = trade_stats["summary"], leg_stats["summary"]
    tp, lp = trade_stats["permutation"], leg_stats["permutation"]
    tb, lb = trade_stats["bootstrap"], leg_stats["bootstrap"]
    drop_notes = []
    if n_exact_dropped:
        drop_notes.append(f"{n_exact_dropped} exact duplicate(s) dropped")
    if n_overlap_dropped:
        drop_notes.append(f"{n_overlap_dropped} window-overlap entr(ies) "
                          f"dropped by calendar coverage")
    lines += [
        "",
        f"pooled trade-level gross returns: n={ts['n']}"
        + (f" ({'; '.join(drop_notes)})" if drop_notes else "")
        + (f", mean {_fmt(ts['mean'])}%/trade, median {_fmt(ts['median'])}, "
           f"min {_fmt(ts['min'], 2)}, max {_fmt(ts['max'], 2)}, "
           f"positive {ts['n_pos']}/{ts['n']}" if ts["n"] else ""),
    ]
    if ts["n"]:
        lines += [
            f"  sign-flip permutation (one-sided, {tp['n_resamples']} "
            f"resamples): p = {tp['p_value']:.4f}   <- primary test",
            f"  bootstrap 95% CI on mean: [{_fmt(tb['lo'])}, {_fmt(tb['hi'])}], "
            f"P(mean<=0) = {trade_stats['bootstrap_p_mean_le_zero']:.4f}",
            f"  sign test (two-sided exact): {ts['n_pos']}/{ts['n']} positive, "
            f"p = {trade_stats['sign_test']['p_value']:.4f}",
            f"  Wilcoxon signed-rank (two-sided): "
            f"p = {trade_stats['wilcoxon']['p_value']:.4f}",
        ]
    lines += [
        "",
        f"per-leg gross returns (the M5 screen statistic): n={ls['n']} legs, "
        + (f"mean {_fmt(ls['mean'])}%/leg" if ls["n"] else "no legs"),
    ]
    if ls["n"]:
        lines += [
            f"  sign-flip permutation (one-sided, {lp['n_resamples']} "
            f"resamples): p = {lp['p_value']:.4f}",
            f"  bootstrap 95% CI on mean: [{_fmt(lb['lo'])}, {_fmt(lb['hi'])}], "
            f"P(mean<=0) = {leg_stats['bootstrap_p_mean_le_zero']:.4f}",
        ]
    if overlaps:
        pairs = "; ".join(f"{a}∩{b} {o['start']}→{o['end']} ({o['days']}d)"
                          for o in overlaps for a, b in [o["windows"]])
        lines.append(
            f"  CAVEAT: leg returns are atomic per window, so the leg-level "
            f"pool counts overlapping windows wholesale — overlap: {pairs}")
    lines += [
        "",
        f"verdict (trade-level primary, alpha={trade_stats['alpha']}): "
        f"{trade_stats['verdict'].upper()}",
    ]
    return "\n".join(lines)


def build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(
        description="M1 step-2 gross-edge sample-noise adjudicator (#1054)")
    p.add_argument("--strategy", required=True,
                   help="Strategy name (registry default params unless --params)")
    p.add_argument("--params", default=None,
                   help="Params JSON (default: registry default_params)")
    p.add_argument("--registry", choices=["spot", "futures"], default="spot")
    p.add_argument("--direction", default=None, choices=["long", "short"],
                   help="Entry side (default: the long/flat audit harness)")
    p.add_argument("--windows", default=",".join(DEFAULT_WINDOWS),
                   help=f"Comma list of windows (default: "
                        f"{','.join(DEFAULT_WINDOWS)} — the M5 screen pair). "
                        f"Known: {', '.join(WINDOWS)}")
    p.add_argument("--datasets", default=None,
                   help="Comma list of SYMBOL:TIMEFRAME (default: the six "
                        "audit datasets)")
    p.add_argument("--capital", type=float, default=DEFAULT_CAPITAL)
    p.add_argument("--resamples", type=int, default=DEFAULT_BOOTSTRAP_RESAMPLES)
    p.add_argument("--seed", type=int, default=DEFAULT_SEED)
    p.add_argument("--alpha", type=float, default=DEFAULT_ALPHA,
                   help=f"Significance level for the primary test "
                        f"(default {DEFAULT_ALPHA})")
    p.add_argument("--json", default=None, dest="json_out",
                   help="Write the full structured result to this path")
    return p


def main(argv: Optional[List[str]] = None) -> int:
    args = build_parser().parse_args(argv)

    window_names = [w.strip() for w in args.windows.split(",") if w.strip()]
    unknown = [w for w in window_names if w not in WINDOWS]
    if unknown:
        raise SystemExit(f"unknown windows {unknown}; known: {list(WINDOWS)}")

    if args.datasets:
        datasets = [parse_dataset_arg(d) for d in args.datasets.split(",")
                    if d.strip()]
    else:
        datasets = list(DATASETS)

    params = json.loads(args.params) if args.params else None

    from registry_loader import load_registry
    reg = load_registry(args.registry)
    if args.strategy not in reg.STRATEGY_REGISTRY:
        raise SystemExit(f"Unknown strategy {args.strategy!r}; available: "
                         f"{reg.list_strategies()}")

    legs = collect_gross_legs(reg, args.strategy, params, datasets,
                              window_names, capital=args.capital,
                              direction=args.direction)
    samples, n_exact, n_overlap = pool_trade_samples(legs)
    overlaps = window_overlaps(window_names)
    trade_values = [s["pnl_pct"] for s in samples]
    leg_values = [leg["return_pct"] for leg in legs]

    trade_stats = analyze_sample(trade_values, n_resamples=args.resamples,
                                 seed=args.seed, alpha=args.alpha)
    leg_stats = analyze_sample(leg_values, n_resamples=args.resamples,
                               seed=args.seed, alpha=args.alpha)

    print(format_report(args.strategy, args.registry, window_names, legs,
                        n_exact, n_overlap, overlaps, trade_stats, leg_stats))

    if args.json_out:
        payload = {
            "strategy": args.strategy,
            "registry": args.registry,
            "params": params,
            "direction": args.direction or "long",
            "windows": {w: list(WINDOWS[w]) for w in window_names},
            "datasets": [dataset_key(s, t) for s, t in datasets],
            "capital": args.capital,
            "resamples": args.resamples,
            "seed": args.seed,
            "alpha": args.alpha,
            "legs": legs,
            "pooled_exact_duplicates_dropped": n_exact,
            "pooled_overlap_entries_dropped": n_overlap,
            "window_overlaps": overlaps,
            "trade_level": trade_stats,
            "leg_level": leg_stats,
        }
        with open(args.json_out, "w") as fh:
            json.dump(payload, fh, indent=2, default=str)
        print(f"\nwrote {args.json_out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
