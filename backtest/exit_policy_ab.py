#!/usr/bin/env python3
"""exit_policy_ab.py — M6 regime-conditioned, incumbent-relative exit-policy A/B (#1066).

The controlled exit experiment the M-series was missing. M2
(``run_backtest.py --mode optimize --sweep-close``) searches a grid for the
*best* exit; M3 (``exit_diagnostics.py``) explains *why* one bleeds. Neither
answers the question an operator actually has after #1059: *does this one
specific exit change beat the incumbent, holding the open / data / regime
classification fixed, attributed by regime?* M6 does exactly that.

Three things separate this from a tidier A/B script (the #1059 one-off had none):

1. **Incumbent-relative, fail-loud.** The control arm's exit resolves from a
   named baseline config the way the live daemon would (``--baseline-config``
   → ``run_backtest.load_strategy_config``, v15-gated for #951 parity), never a
   frozen ladder literal. It replays the incumbent's COMPLETE live exit policy —
   the close evaluator AND the strategy-level stop/trailing fields
   (``STOP_FIELD_KEYS``: ``stop_loss_atr_mult`` / ``trailing_stop_atr_regime`` /
   …), which the ``Backtester`` consumes as real exit logic — so the A/B is never
   measured against a phantom incumbent with a weaker exit than runs live. The
   candidate arm holds those same stops fixed by default (``--candidate-stops
   inherit``, isolating the close-evaluator change) or drops them
   (``--candidate-stops drop``, candidate close refs become the entire exit). If
   the strategy is absent or has no close, the harness ERRORS — it never silently
   substitutes a different baseline (the open-as-close fallback is available only
   via the explicit ``--incumbent-close none``).

2. **Walk-forward / out-of-sample.** Both arms run across the versioned audit
   ``WINDOWS`` imported from ``eval_windows.py`` (in-sample / OOS / held-out), so
   a candidate's edge is shown per window, not on a single fitted period.

3. **The unpaired-N confound, handled at the source.** An exit change shifts
   re-entry timing, so the two arms free-run a different number of trades. A
   naive "pair the trades that share an entry bar" is *selection-biased* — a
   longer exit blocks re-entries, so which entries survive into the paired set
   is itself shaped by the policy under test. Instead M6 runs an
   **entry-locked per-entry replay**: it takes the incumbent's realised entry
   bars as a fixed schedule and replays the candidate exit *independently per
   entry* (one forced entry at a time, so a longer candidate exit can never drop
   an entry), yielding an unbiased paired sample → Wilcoxon signed-rank + sign
   test + paired bootstrap CI on per-entry ΔPnL, bucketed by entry regime. The
   realistic free-running arms (each trading its own re-entry schedule) are kept
   as the unpaired aggregate view (+ unpaired bootstrap), and the divergence
   between the two trade universes is reported, never hidden.

Per-entry replay requires a *self-contained* candidate exit (ATR / trailing /
tiered-TP / time / zscore — fires off price/ATR/bars). A signal-reversal
("open-as-close") candidate has no rule to replay in isolation (its exits come
from the strategy's own later signals, which the single-entry replay removes);
the harness detects this and reports the unpaired view only, loudly, rather than
fabricating a paired number.

All scoring/aggregation/statistics below the I/O line are pure (operate on lists
of plain dicts), unit-tested without data access — same architecture as
``eval_windows.py`` / ``exit_diagnostics.py``. The audit-identical data slices
(``WINDOWS`` / ``DATASETS`` / ``PLATFORM`` / fees) are imported from
``eval_windows.py`` so M6 sees byte-identical data to the rest of the suite.

Usage:
  # Replace the strategy's whole live exit with a candidate stop (--candidate-stops
  # drop so the atr_stop is measured alone, NOT stacked under the incumbent's stop):
  uv run --no-sync python backtest/exit_policy_ab.py \\
      --baseline-config /var/lib/go-trader/config.json --strategy hl-btc-ranging \\
      --candidate-close '[{"name":"atr_stop","params":{"atr_mult":2.5}}]' \\
      --candidate-stops drop

  # Candidate vs an explicit named control exit, gated to one regime substate:
  uv run --no-sync python backtest/exit_policy_ab.py --strategy squeeze_momentum \\
      --incumbent-close '[{"name":"tiered_tp_atr","params":{}}]' \\
      --candidate-close '[{"name":"trailing_stop_atr_mult","params":{"atr_mult":3}}]' \\
      --allowed-regimes ranging_quiet --regime-classifier composite \\
      --windows is,oos
"""

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

from eval_windows import (  # noqa: E402  (path bootstrap above)
    DATASETS,
    DEFAULT_CAPITAL,
    PLATFORM,
    WINDOWS,
    dataset_key,
    parse_dataset_arg,
)
from exit_diagnostics import trade_metrics  # noqa: E402  (per-leg net% netting, M3 SSoT)

# Default bootstrap configuration. Fixed seed so a run is reproducible and the
# unit tests are deterministic (no scipy / numpy in the stats path).
DEFAULT_BOOTSTRAP_RESAMPLES = 10000
DEFAULT_CI = 0.95
DEFAULT_SEED = 1066

# Regime label stamped on an entry whose classifier produced an empty/unknown
# bucket (warmup rows, mid-series NaN) — kept distinct so it never silently
# merges with a real regime.
UNKNOWN_REGIME = "?"

# Strategy-level stop/trailing exit fields a live config carries ALONGSIDE the
# close evaluator (config.go's seven mutually-exclusive HL stop owners, minus the
# duplicate accessors). ``load_strategy_config`` resolves these the way the daemon
# would (#951 parity) and the ``Backtester`` consumes each as real exit logic —
# so the control arm MUST replay them, or it measures a phantom incumbent with a
# weaker exit than the live strategy actually runs.
STOP_FIELD_KEYS = (
    "stop_loss_atr_mult",
    "stop_loss_pct",
    "stop_loss_margin_pct",
    "trailing_stop_atr_mult",
    "trailing_stop_pct",
    "stop_loss_atr_regime",
    "trailing_stop_atr_regime",
)


# ===========================================================================
# Pure statistics (stdlib only; deterministic).
# ===========================================================================

def _norm_cdf(z: float) -> float:
    """Standard-normal CDF via erf (no scipy)."""
    return 0.5 * (1.0 + math.erf(z / math.sqrt(2.0)))


def _binom_two_sided_p(k: int, n: int, p: float = 0.5) -> float:
    """Two-sided exact binomial p-value for k successes in n trials.

    p = 2·min(P(X≤k), P(X≥k)) clamped to 1.0. n is a trade count (small), so the
    exact sum via ``math.comb`` is cheap and avoids a normal approximation on
    the tiny samples M6 routinely sees.
    """
    if n <= 0:
        return 1.0
    k = max(0, min(k, n))

    def _cdf(upper: int) -> float:
        return sum(math.comb(n, i) * (p ** i) * ((1.0 - p) ** (n - i))
                   for i in range(0, upper + 1))

    lower_tail = _cdf(k)
    upper_tail = 1.0 - _cdf(k - 1) if k > 0 else 1.0
    return min(1.0, 2.0 * min(lower_tail, upper_tail))


def sign_test(deltas: Sequence[float], zero_tol: float = 1e-12) -> dict:
    """Two-sided sign test on paired deltas (does the candidate move ΔPnL?).

    Zeros (|Δ| ≤ ``zero_tol``) are dropped, not split — the conservative
    convention. ``p_value`` is the exact binomial two-sided p under H0 p=0.5.
    """
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
    """Tie-averaged ranks (1-based) of ``values`` and the tie-group sizes."""
    order = sorted(range(len(values)), key=lambda i: values[i])
    ranks = [0.0] * len(values)
    tie_sizes: List[int] = []
    i = 0
    while i < len(order):
        j = i
        while j + 1 < len(order) and values[order[j + 1]] == values[order[i]]:
            j += 1
        avg_rank = (i + 1 + j + 1) / 2.0  # average of 1-based ranks i+1..j+1
        for t in range(i, j + 1):
            ranks[order[t]] = avg_rank
        tie_sizes.append(j - i + 1)
        i = j + 1
    return ranks, tie_sizes


def wilcoxon_signed_rank(deltas: Sequence[float], zero_tol: float = 1e-12) -> dict:
    """Wilcoxon signed-rank test (normal approximation, tie/zero corrected).

    Zeros dropped (Wilcoxon convention). Returns the positive-rank sum ``w``, the
    continuity-corrected z, and a two-sided ``p_value``. With < 1 non-zero pair
    the test is undefined → p_value 1.0. The normal approximation is standard for
    the dozens-to-hundreds of trades M6 sees; exactness lives in ``sign_test``.
    """
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
    # Continuity correction toward the mean.
    diff = w_pos - mean_w
    cc = 0.5 if diff > 0 else (-0.5 if diff < 0 else 0.0)
    z = (diff - cc) / math.sqrt(var_w)
    p = 2.0 * (1.0 - _norm_cdf(abs(z)))
    return {"n": n, "w": round(w_pos, 4), "z": round(z, 4),
            "p_value": round(min(1.0, max(0.0, p)), 6)}


def _percentile(sorted_xs: Sequence[float], q: float) -> float:
    """Linear-interpolation percentile (q in 0..100); ``sorted_xs`` non-empty."""
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
    """Seeded percentile bootstrap CI for ``statistic`` over ``samples``.

    Deterministic given ``seed`` (stdlib ``random.Random`` index resampling). With
    < 2 samples the CI collapses to the point estimate. ``statistic`` defaults to
    the mean.
    """
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
    """Bootstrap CI for mean(candidate) − mean(control) as independent samples.

    The honest unpaired view: each arm free-runs its own trade universe, so the
    two samples are resampled independently. Point estimate is the raw
    difference of means; ``None`` arms (no trades) yield a ``None`` CI.
    """
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
    """Full paired verdict on per-entry ΔPnL: mean/median, sign + signed-rank, CI."""
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


# ===========================================================================
# Pure per-entry aggregation (collapse close legs → one record per entry).
# ===========================================================================

def collapse_entry(legs: Sequence[dict]) -> Optional[dict]:
    """Collapse all close legs of ONE position into a single per-entry record.

    A tiered-TP exit emits several legs sharing one ``entry_date``; a plain
    stop/trailing/time exit emits one. Net% is the notional-weighted mean of the
    per-leg net% (each leg already nets its own entry+exit fee via the M3
    ``trade_metrics`` SSoT, so no fee is double-counted across partials). MFE is
    the max favourable and MAE the most adverse excursion seen across the legs;
    bars_held is the longest leg (the full hold).
    """
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
    else:  # degenerate (zero-size legs): fall back to a simple mean
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
    """Group close legs into per-entry buckets keyed by ``entry_date``.

    Insertion-ordered so the entry schedule keeps its chronological order (the
    backtester emits trades in close order; the first leg of each entry fixes its
    slot). Used to turn a free-run trade list into per-entry records.
    """
    groups: "OrderedDict[str, List[dict]]" = OrderedDict()
    for t in trades:
        key = str(t.get("entry_date", ""))
        groups.setdefault(key, []).append(t)
    return groups


def free_arm_entries(trades: Sequence[dict]) -> List[dict]:
    """Free-run trade list → ordered list of collapsed per-entry records."""
    out = []
    for legs in group_entries(trades).values():
        rec = collapse_entry(legs)
        if rec is not None:
            out.append(rec)
    return out


def arm_summary(results: Optional[dict]) -> dict:
    """Portfolio-level summary of one free-running arm (the unpaired view).

    Max drawdown is intrinsically a sequential-equity property, so it lives HERE
    (per arm), never in the per-entry/per-regime paired table — a per-entry
    sample has no portfolio drawdown. Net%/win-rate are computed from the
    collapsed per-entry records so they compose tiered-TP partials correctly.
    """
    if not results:
        return {"trades": 0, "entries": 0, "win_rate": None, "mean_net_pct": None,
                "total_net_pct": None, "total_return_pct": None,
                "max_drawdown_pct": None, "sharpe": None, "liquidated": False}
    entries = free_arm_entries(results.get("trades", []) or [])
    nets = [e["net_pct"] for e in entries]
    return {
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


# ===========================================================================
# Pure paired-sample assembly + per-regime delta table.
# ===========================================================================

def build_paired_rows(control_entries: Sequence[dict],
                      candidate_by_date: Dict[str, Optional[dict]],
                      regime_by_date: Dict[str, str]) -> Tuple[List[dict], dict]:
    """Pair each incumbent entry with its entry-locked candidate replay.

    ``control_entries``  — collapsed per-entry records from the incumbent free run
                           (the fixed entry schedule).
    ``candidate_by_date``— {entry_date → collapsed candidate replay record | None}
                           (None = the per-entry replay produced no trade, e.g.
                           the forced entry could not open; counted, not paired).
    ``regime_by_date``   — {entry_date → regime label at the decision bar}.

    Returns (rows, diag) where each row carries control/candidate net%, the
    ΔPnL, excursions, and the regime; ``diag`` counts the schedule vs how many
    paired so the divergence is reported, never hidden.
    """
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
    """Per-bucket control/candidate aggregates + paired ΔPnL verdict."""
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
        # MAE is the per-entry analog of drawdown (portfolio max-DD is not
        # regime-decomposable); report the median adverse excursion per arm.
        "control_median_mae_pct": _med([r["control_mae_pct"] for r in rows]),
        "candidate_median_mae_pct": _med([r["candidate_mae_pct"] for r in rows]),
        "control_median_mfe_pct": _med([r["control_mfe_pct"] for r in rows]),
        "candidate_median_mfe_pct": _med([r["candidate_mfe_pct"] for r in rows]),
        "paired_delta": paired_delta_summary(deltas, n_resamples=n_resamples,
                                             ci=ci, seed=seed),
    }


def per_regime_table(rows: Sequence[dict], n_resamples: int = DEFAULT_BOOTSTRAP_RESAMPLES,
                     ci: float = DEFAULT_CI, seed: int = DEFAULT_SEED) -> dict:
    """Per-regime + overall paired delta table from the paired rows.

    Buckets the paired entries by their decision-bar regime label and computes,
    per bucket and for ALL entries: control vs candidate mean/total net%, win
    rate, median MFE/MAE, and the paired ΔPnL verdict (sign test + signed-rank +
    bootstrap CI). Regime keys are sorted for deterministic output.
    """
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


# ===========================================================================
# I/O — data loading, the two free arms, and the entry-locked per-entry replay.
# Everything above this line is pure and unit-tested without data access.
# ===========================================================================

# Self-contained close evaluators whose exit is a pure function of price / ATR /
# bars / indicators on the held position — replayable for a single isolated
# entry. A close NOT in this set (signal-reversal / open-as-close) has no rule to
# replay per-entry, so the paired engine declines rather than fabricate.
_REPLAYABLE_CLOSE_NAMES = {
    "atr_stop",
    "time_stop",
    "zscore_target",
    "tiered_tp_atr",
    "tiered_tp_atr_live",
    "trailing_stop_atr_mult",
    "trailing_stop_atr_regime",
    "stop_loss_atr_mult",
    # #1152: the ratchet ladders and the frozen-at-open regime TP are
    # self-contained — they fire off price/ATR/the regime label stamped at open
    # (a pure function of bar data the replay preserves), never off later
    # signals, so the single-entry replay isolates them faithfully. The
    # per-tick re-resolution variants (tiered_tp_atr_live_regime*) stay out:
    # _dynamic is HL-live-only (load_strategy_config rejects it) and the plain
    # live_regime variant has no validation run behind it yet.
    "trailing_tp_ratchet",
    "trailing_tp_ratchet_regime",
    "tiered_tp_atr_regime",
}


def candidate_is_replayable(close_refs: Optional[Sequence[dict]]) -> bool:
    """True iff every candidate close ref is a self-contained (per-entry replayable) exit."""
    if not close_refs:
        return False  # open-as-close: no rule to isolate per entry
    return all(isinstance(r, dict) and r.get("name") in _REPLAYABLE_CLOSE_NAMES
               for r in close_refs)


def _prepare_signals(reg, open_name: str, params: Optional[dict], df):
    """apply_strategy + ensure ATR once for a (dataset, window). Reused per entry."""
    from atr import ensure_atr_indicator
    df_signals = reg.apply_strategy(open_name, df, params)
    df_signals = ensure_atr_indicator(df_signals)
    return df_signals


def _regime_label_series(df, regime_cfg: dict):
    """Bar-close regime labels for attribution (un-shifted; classifier from config).

    Returns a list aligned to ``df.index`` (so position p ↔ bar p). Computed on a
    copy so the gating arms compute their own (shifted) regime independently.
    """
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


def _backtester_kwargs(open_name: str, params: Optional[dict],
                       close_refs: Optional[Sequence[dict]], direction: Optional[str],
                       capital: float, gate: dict,
                       stops: Optional[dict] = None) -> dict:
    """Shared Backtester kwargs so both arms + the replay gate identically.

    ``stops`` is the strategy-level stop/trailing exit surface (``STOP_FIELD_KEYS``)
    spread into the constructor verbatim — these are real exit logic the
    ``Backtester`` consumes (``run_backtest.run_single_backtest`` threads the same
    fields). The control arm passes the incumbent's resolved stops so it replays
    the *complete* live exit policy, not just the close evaluator.
    """
    use_regime = bool(gate.get("allowed_regimes"))
    kw = dict(
        initial_capital=capital, platform=PLATFORM,
        open_strategy={"name": open_name, "params": dict(params or {})},
        close_strategies=(list(close_refs) if close_refs else None),
        direction=direction,
        regime_enabled=use_regime,
        regime_period=int(gate.get("period", 14)),
        regime_adx_threshold=float(gate.get("adx_threshold", 20.0)),
        regime_windows_spec=gate.get("windows_spec"),
        allowed_regimes=(list(gate["allowed_regimes"]) if gate.get("allowed_regimes") else None),
    )
    # Spread only the present (non-None) stop fields; the Backtester defaults each
    # to None. STOP_FIELD_KEYS never collides with the keys set above.
    for k in STOP_FIELD_KEYS:
        v = (stops or {}).get(k)
        if v is not None:
            kw[k] = v
    return kw


def run_free_arm(reg, open_name: str, params: Optional[dict], df_signals,
                 close_refs: Optional[Sequence[dict]], direction: Optional[str],
                 capital: float, gate: dict, symbol: str, timeframe: str,
                 stops: Optional[dict] = None) -> dict:
    """Run one free-running arm over the prepared signals → full results dict."""
    from backtester import Backtester
    bt = Backtester(**_backtester_kwargs(open_name, params, close_refs, direction,
                                         capital, gate, stops))
    return bt.run(df_signals.copy(), strategy_name=open_name, symbol=symbol,
                  timeframe=timeframe, params=params, save=False)


def replay_candidate_for_entry(reg, open_name: str, params: Optional[dict], df_signals,
                               sig_pos: int, side_sign: int,
                               candidate_close: Sequence[dict], direction: Optional[str],
                               capital: float, gate: dict, symbol: str,
                               timeframe: str, stops: Optional[dict] = None) -> Optional[dict]:
    """Entry-locked replay: open ONE position at ``sig_pos`` under the candidate exit.

    The signal column is overwritten so the only entry is ``side_sign`` at
    ``sig_pos`` (raw bar before the fill — the backtester's internal shift(1)
    fills it at ``sig_pos+1``). All other indicator/regime columns are untouched,
    so the candidate close evaluator sees the same ATR / regime context the free
    arm did. Every returned leg belongs to this one entry → collapse them.
    """
    from backtester import Backtester
    one = df_signals.copy()
    sig_col = one.columns.get_loc("signal")
    one.iloc[:, sig_col] = 0
    one.iloc[sig_pos, sig_col] = int(side_sign)
    bt = Backtester(**_backtester_kwargs(open_name, params, candidate_close, direction,
                                         capital, gate, stops))
    results = bt.run(one, strategy_name=open_name, symbol=symbol,
                     timeframe=timeframe, params=params, save=False)
    return collapse_entry(results.get("trades", []) or [])


def evaluate_dataset_window(reg, spec: dict, symbol: str, timeframe: str,
                            window: tuple) -> Optional[dict]:
    """A/B one (dataset, window): free arms, entry-locked replay, regime table.

    ``spec`` carries: open_name, params, direction, incumbent_close,
    candidate_close, gate (allowed_regimes + classifier params for the entry
    gate), regime_cfg (classifier for attribution), capital, and the bootstrap
    knobs. Returns the structured per-dataset result, or None if the window has
    no data.
    """
    from data_fetcher import load_cached_data
    from run_backtest import FUNDING_COLUMN_STRATEGIES, _attach_funding_if_needed

    start, end = window
    df = load_cached_data(symbol, timeframe, start_date=start, end_date=end)
    if df.empty:
        return None
    if spec["open_name"] in FUNDING_COLUMN_STRATEGIES:
        df = _attach_funding_if_needed(df, spec["open_name"], symbol, start)

    df_signals = _prepare_signals(reg, spec["open_name"], spec.get("params"), df)
    # Positions and attribution labels MUST be derived from df_signals — the exact
    # frame the backtester indexes (run_free_arm/replay both run on df_signals) — not
    # the pre-apply_strategy df. They share an index today, but a future open
    # strategy that drops warmup rows or reindexes would silently misalign every
    # forced-entry replay (one.iloc[sig_pos] lands on the wrong bar) and the regime
    # bucket of every paired delta. Anchoring on df_signals makes the gate ≡
    # attribution ≡ replay-injection coupling explicit instead of accidental.
    regime_series = _regime_label_series(df_signals, spec["regime_cfg"])
    pos_by_date = {str(ts): i for i, ts in enumerate(df_signals.index)}

    # Free-running arms (realistic, own re-entry schedule each). The control arm
    # carries the incumbent's strategy-level stops (full live exit fidelity); the
    # candidate arm carries them only under --candidate-stops inherit (held fixed
    # so the A/B isolates the close-evaluator change), none under drop.
    control_results = run_free_arm(
        reg, spec["open_name"], spec.get("params"), df_signals,
        spec.get("incumbent_close"), spec.get("direction"), spec["capital"],
        spec["gate"], symbol, timeframe, spec.get("control_stops"))
    candidate_results = run_free_arm(
        reg, spec["open_name"], spec.get("params"), df_signals,
        spec.get("candidate_close"), spec.get("direction"), spec["capital"],
        spec["gate"], symbol, timeframe, spec.get("candidate_stops"))

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
                candidate_by_date[date] = None  # cannot place a pre-fill signal
                continue
            sig_pos = fill_pos - 1
            label = regime_series[sig_pos] if 0 <= sig_pos < len(regime_series) else ""
            regime_by_date[date] = label or UNKNOWN_REGIME
            side_sign = -1 if ctrl["side"] == "short" else 1
            candidate_by_date[date] = replay_candidate_for_entry(
                reg, spec["open_name"], spec.get("params"), df_signals, sig_pos,
                side_sign, spec["candidate_close"], spec.get("direction"),
                spec["capital"], spec["gate"], symbol, timeframe,
                spec.get("candidate_stops"))
        paired_rows, paired_diag = build_paired_rows(
            control_entries, candidate_by_date, regime_by_date)
        paired_diag["replayable"] = True

    table = per_regime_table(paired_rows, n_resamples=spec["n_resamples"],
                             ci=spec["ci"], seed=spec["seed"]) if paired_rows else None

    # Unpaired (realistic) net-PnL difference between the free arms.
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
    }


# ===========================================================================
# Incumbent / candidate / regime resolution (fail loud).
# ===========================================================================

def _parse_close_arg(raw: str, label: str) -> Optional[List[dict]]:
    """Parse a --incumbent-close/--candidate-close JSON list (or the literal 'none')."""
    if raw is None:
        return None
    if raw.strip().lower() == "none":
        return None  # explicit open-as-close
    refs = json.loads(raw)
    if not isinstance(refs, list) or not all(
            isinstance(r, dict) and r.get("name") for r in refs):
        raise SystemExit(f"{label} must be a JSON list of close refs "
                         f"[{{\"name\":..., \"params\":...}}] or the literal 'none'")
    return refs


def _stops_from_kwargs(kwargs: dict) -> dict:
    """Collect the non-None strategy-level stop fields from a load_strategy_config result.

    ``load_strategy_config`` returns every ``STOP_FIELD_KEYS`` entry (None when the
    strategy doesn't set it). Keep only the present ones so ``_backtester_kwargs``
    spreads exactly the live exit surface and nothing else.
    """
    return {k: kwargs.get(k) for k in STOP_FIELD_KEYS if kwargs.get(k) is not None}


# Live entry-shaping fields that M6's entry-locked replay cannot reproduce
# faithfully: invert_signal flips the traded side (BUY↔SELL), regime_directional_policy
# mutates the entry direction per regime, and profile_allocation swaps the open
# params per profile (a different signal series altogether). When a baseline config's
# incumbent sets any of these, the control arm would NOT match the live incumbent's
# entries — so M6 refuses rather than A/B a phantom incumbent (matching
# load_strategy_config's own loud-reject convention for un-modellable fields).
_UNREPLAYABLE_ENTRY_SHAPERS = (
    "invert_signal",
    "regime_directional_policy",
    "profile_allocation",
)


def _reject_unreplayable_entry_shapers(kwargs: dict) -> None:
    """Raise if a baseline-resolved incumbent uses an entry-shaper M6 can't replay.

    Operates on a ``load_strategy_config`` result. Truthy detection handles both the
    bool (``invert_signal``) and the dict (``regime_directional_policy`` /
    ``profile_allocation``) forms; absent/None/empty → no offence.
    """
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


def _candidate_stops(mode: str, incumbent_stops: dict) -> dict:
    """Resolve the candidate arm's stop fields from the --candidate-stops mode.

    ``inherit`` (default) holds the incumbent's strategy-level stops fixed on the
    candidate arm so the A/B isolates the close-evaluator change; ``drop`` runs the
    candidate arm with NO strategy-level stops so its close refs are the entire
    exit (a full exit-policy replacement). The control arm ALWAYS keeps the
    incumbent stops regardless of this mode.
    """
    if mode == "drop":
        return {}
    return dict(incumbent_stops or {})


# Candidate close evaluators that ARE protective price stops — under
# ``--candidate-stops inherit`` they stack under the incumbent's own stop and the
# effective exit becomes the tighter of the two, so the A/B reflects mostly the
# inherited stop, not the candidate in isolation. Take-profit ladders
# (``tiered_tp_atr``) and mean-reversion targets (``zscore_target``) don't
# min()-stack on the downside, so they're excluded (no spurious warning).
_STOP_CLASS_CANDIDATE_NAMES = {
    "atr_stop",
    "stop_loss_atr_mult",
    "trailing_stop_atr_mult",
    "trailing_stop_atr_regime",
}


def _candidate_stacks_on_inherited_stop(candidate_close: Optional[Sequence[dict]],
                                        mode: str, incumbent_stops: dict) -> bool:
    """True iff a stop-class candidate would stack under an inherited incumbent stop.

    Only fires under ``inherit`` mode WITH a non-empty incumbent stop AND a
    stop-class candidate — the exact combination where the candidate's protective
    stop is masked by the tighter inherited one. ``drop`` mode, no inherited stop,
    or a non-stop candidate (TP ladder) → False.
    """
    if mode != "inherit" or not incumbent_stops or not candidate_close:
        return False
    return any(isinstance(r, dict) and r.get("name") in _STOP_CLASS_CANDIDATE_NAMES
               for r in candidate_close)


def resolve_from_baseline(config_path: str, strategy_id: str) -> dict:
    """Resolve open + incumbent close + stops from a live config (v15-gated, #951).

    Returns {open_name, params, incumbent_close, stops, direction, allowed_regimes,
    regime_section}. ``stops`` is the incumbent's strategy-level stop/trailing exit
    surface (``STOP_FIELD_KEYS``) so the control arm replays the COMPLETE live exit
    policy, not just the close evaluator. Fails loud (the load_strategy_config
    guards) on pre-v15, missing strategy, or backtester-incompatible close — and on
    an incumbent using an entry-shaper M6 can't replay (``invert_signal`` /
    ``regime_directional_policy`` / ``profile_allocation``) — never a silent fallback.
    """
    from run_backtest import load_strategy_config
    import json as _json
    kwargs = load_strategy_config(config_path, strategy_id)
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
    """Synthesize a one-window composite spec.

    ``ensure_regime_columns`` (and the backtester gate) honour the composite
    classifier ONLY via ``windows_spec`` — the bare ``classifier=`` kwarg is dead
    (regime.py). So a composite request with no explicit windows must synthesize a
    spec, or attribution/gate would silently fall back to ADX labels.
    """
    return {"attribution": {"classifier": "composite", "period": int(period)}}


def resolve_regime_cfg(args, regime_section: dict) -> dict:
    """Resolve the attribution+gate classifier: CLI override > baseline config > ADX.

    Always returns a (classifier, windows_spec) pair that is internally
    consistent: composite ⇒ a non-None windows_spec (real or synthesized), adx ⇒
    windows_spec None. Both the attribution series and the backtester gate read
    the SAME windows_spec, so they can never diverge.
    """
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
    # No CLI override: inherit the baseline config's composite windows if present.
    windows = (regime_section or {}).get("windows")
    if windows:
        return {**base, "classifier": "composite", "windows_spec": windows}
    return {**base, "classifier": "adx", "windows_spec": None}


# ===========================================================================
# Reporting.
# ===========================================================================

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


# ===========================================================================
# CLI.
# ===========================================================================

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
    p.add_argument("--candidate-stops", choices=["inherit", "drop"], default="inherit",
                   help="How the candidate arm treats the incumbent's strategy-level "
                        "stops (resolved from --baseline-config). 'inherit' (default) "
                        "holds them fixed so the A/B isolates the close-evaluator "
                        "change; 'drop' runs the candidate with NO strategy-level stop "
                        "so its close refs are the entire exit (full-policy "
                        "replacement). The control arm always keeps the incumbent "
                        "stops either way.")
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
    return p


def _resolve_spec(args) -> dict:
    """Build the immutable per-run spec from args (fail-loud incumbent resolution)."""
    open_name = args.strategy
    params = json.loads(args.params) if args.params else None
    direction = args.direction
    incumbent_close = None
    incumbent_stops: dict = {}
    regime_section: dict = {}
    config_allowed_regimes = None

    if args.baseline_config:
        resolved = resolve_from_baseline(args.baseline_config, args.strategy)
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
        # The explicit-close path carries no strategy-level stops (only --baseline-config
        # resolves them); --candidate-stops is therefore a no-op here.
        if args.candidate_stops == "drop":
            print("[WARN] --candidate-stops drop has no effect without --baseline-config "
                  "(the explicit --incumbent-close path resolves no strategy-level stops).",
                  file=sys.stderr)

    candidate_close = _parse_close_arg(args.candidate_close, "--candidate-close")
    direction = direction or "long"

    candidate_stops = _candidate_stops(args.candidate_stops, incumbent_stops)
    if _candidate_stacks_on_inherited_stop(candidate_close, args.candidate_stops,
                                           incumbent_stops):
        print("[WARN] the candidate is a protective stop AND --candidate-stops inherit "
              "(default) keeps the incumbent's stop, so the candidate stacks under it "
              "— the effective exit is the TIGHTER of the two and the A/B reflects "
              "mostly the inherited stop, not the candidate in isolation. Pass "
              "--candidate-stops drop to measure the candidate stop alone.",
              file=sys.stderr)

    regime_cfg = resolve_regime_cfg(args, regime_section)
    # #1066 finding-2: the backtester's entry gate has no gate-window parameter —
    # it default-picks the primary window of a composite windows_spec (regime.py),
    # exactly why run_backtest.load_strategy_config rejects a named regime_gate_window
    # as having no bar-level parity. A user-supplied --gate-window naming one window
    # of a MULTI-window spec would steer attribution to a window the gate cannot
    # honor, silently mis-bucketing every regime-conditioned delta. Reject loudly.
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
    return {
        "open_name": open_name,
        "params": params,
        "direction": direction,
        "incumbent_close": incumbent_close,
        "candidate_close": candidate_close,
        "control_stops": incumbent_stops,
        "candidate_stops": candidate_stops,
        "candidate_stops_mode": args.candidate_stops,
        "replayable": candidate_is_replayable(candidate_close),
        "gate": gate,
        "regime_cfg": regime_cfg,
        "capital": args.capital,
        "n_resamples": args.bootstrap_resamples,
        "ci": args.ci,
        "seed": args.seed,
    }


def main(argv: Optional[List[str]] = None) -> int:
    args = build_parser().parse_args(argv)
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
    print(f"candidate close: {spec['candidate_close'] or 'open-as-close (signal reversal)'}")
    ctrl_stops = spec.get("control_stops") or {}
    print(f"incumbent stops (control arm): {ctrl_stops or 'none'}"
          + (f"  |  candidate arm stops: {spec['candidate_stops_mode']} "
             f"({spec.get('candidate_stops') or 'none'})" if ctrl_stops else ""))
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

    if args.json_out:
        payload = {
            "open": {"name": spec["open_name"], "params": spec["params"],
                     "direction": spec["direction"]},
            "incumbent_close": spec["incumbent_close"],
            "candidate_close": spec["candidate_close"],
            "control_stops": spec.get("control_stops"),
            "candidate_stops": spec.get("candidate_stops"),
            "candidate_stops_mode": spec.get("candidate_stops_mode"),
            "replayable": spec["replayable"],
            "regime_cfg": spec["regime_cfg"],
            "gate_allowed_regimes": spec["gate"]["allowed_regimes"],
            "registry": args.registry,
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
