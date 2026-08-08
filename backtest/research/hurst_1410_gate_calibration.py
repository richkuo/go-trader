#!/usr/bin/env python3
"""#1410: calibration study for a FUTURE Hurst-based entry gate — report only.

#1409 added the Hurst exponent estimator (``hurst_exponent`` in
``shared_strategies/open/indicators_core.py``) and surfaced it as an
observability metric. Nothing measured whether entry outcomes actually vary
with H at entry time, so any gate threshold would be a guess. This one-shot
produces that evidence and renders ``backtest/research/hurst_1410_gate_calibration.md``.

REPORT-PATH CONTRACT (#1422). ``backtest/research/hurst_gate_calibration.md`` is
the LIVE-EVIDENCE contract path — it is what ``scheduler/hurst_gate.go``,
``docs/ARCHITECTURE.md`` and #1412's Stage 0 gate cite, and it is owned by
``hurst_1422_gate_power.py``. This study is a FROZEN negative-result artifact and
must never write that file: a later ``--render-only`` here would silently revert
the live evidence to this study's superseded verdict. Its own render lives beside
its JSON at ``hurst_1410_gate_calibration.md``.

ADVISORY-ONLY INVARIANT (#1409, stamped beside ``hurst_exponent``): no consumer
of that estimator may feed gating, sizing, or config surfaces. This script is a
report-only research harness. It writes exactly two artifacts (a JSON aggregate
and a Markdown report) and touches no scheduler, config, or live path. The
"gate" and "size" arms below are OFFLINE SIMULATIONS used to decide whether such
a gate should ever be built — they ship nothing.

Design constants are PRE-REGISTERED at module level and echoed into both
artifacts, so the Recommendation is the mechanical output of fixed rules rather
than a hand-curated conclusion.

Method
------
Part A — bucketing. Run each family exemplar ungated on the M1 harness
(``eval_windows.run_leg``), stamp every trade with H at its entry decision, and
report per-bucket trade count / win rate / mean+median net return / compounded
return / trade-sequence max drawdown. NaN is its own bucket and is never
coerced to 0.5.

Part B — hard-gate sweeps. Real ``Backtester`` re-runs with entry signals
masked while the hysteresis gate is disarmed (``+1`` zeroed, ``-1`` closes
untouched), using Backtester kwargs identical to ``run_leg``. Deltas vs the
ungated leg: max drawdown, total return, chop loss, trade count.

Part C — sizing variant. Exact re-compounding of the ungated per-trade net
returns with a per-trade multiplier ``m = clamp(1 + gain * e, 0.0, 1.5)``.
Under fraction-of-equity sizing this transform is exact for return and
trade-sequence drawdown. Its drawdown is TRADE-GRANULAR, while Part B
drawdowns are bar-level — the report says so at the table.

Statistics — one-sided seeded permutation tests, all hypotheses corrected
together with ``regime_stats.benjamini_hochberg``; raw and corrected results
are reported side by side.

Usage
-----
  uv run --no-sync python backtest/research/hurst_1410_gate_calibration.py \\
      [--jobs 4] [--write-report] [--only momentum] [--windows is,oos] \\
      [--datasets BTC/USDT:1h,...] [--out-dir /tmp/hurst_1410]
"""
from __future__ import annotations

import argparse
import json
import math
import os
import sys
import time
from concurrent.futures import ThreadPoolExecutor
from typing import Optional, Sequence

_THIS_DIR = os.path.dirname(os.path.abspath(__file__))
_BACKTEST = os.path.abspath(os.path.join(_THIS_DIR, ".."))
_ROOT = os.path.abspath(os.path.join(_BACKTEST, ".."))
for _p in (_BACKTEST, _ROOT, os.path.join(_ROOT, "shared_tools")):
    if _p not in sys.path:
        sys.path.insert(0, _p)

import numpy as np
import pandas as pd

from eval_windows import (  # noqa: E402  (sys.path bootstrap must run first)
    DATASETS,
    DEFAULT_CAPITAL,
    FEE_PLATFORM,
    HELD_OUT_WINDOWS,
    PLATFORM,
    PROTOCOL_WINDOWS,
    WINDOWS,
    dataset_key,
    leg_from_results,
    run_leg,
    trade_samples_from_results,
)
from regime_stats import benjamini_hochberg  # noqa: E402

# #1409 SSoT estimator. Package import first (repo root on sys.path), file-spec
# fallback second — the same two-step shared_tools/regime.py uses. Never
# reimplement DFA here.
try:
    from shared_strategies.open.indicators_core import hurst_exponent  # noqa: E402
except ImportError:  # pragma: no cover - direct-path execution fallback
    import importlib.util

    _HURST_SPEC = importlib.util.spec_from_file_location(
        "_hurst_1410_indicators_core",
        os.path.join(_ROOT, "shared_strategies", "open", "indicators_core.py"),
    )
    if _HURST_SPEC is None or _HURST_SPEC.loader is None:
        raise
    _HURST_MODULE = importlib.util.module_from_spec(_HURST_SPEC)
    _HURST_SPEC.loader.exec_module(_HURST_MODULE)
    hurst_exponent = _HURST_MODULE.hurst_exponent


# ---------------------------------------------------------------------------
# Pre-registered design constants. Changing any of these changes the study;
# they are serialized verbatim into the JSON and the report so a reader can
# tell that the Recommendation was not tuned after seeing the numbers.
# ---------------------------------------------------------------------------
SCHEMA_VERSION = 1
ISSUE = 1410

# Strategy families. The exemplars are spot-registry names run on the M1
# long-leg path with registry default params — the same harness incumbents
# eval_windows scores. The report states this mapping explicitly.
FAMILY_MOMENTUM = "momentum"
FAMILY_MEAN_REVERSION = "mean_reversion"
FAMILY_EXEMPLARS = {
    FAMILY_MOMENTUM: ("momentum", "vol_momentum", "squeeze_momentum"),
    FAMILY_MEAN_REVERSION: ("mean_reversion", "atr_band_revert"),
}
FAMILIES = (FAMILY_MOMENTUM, FAMILY_MEAN_REVERSION)

# `atr_band_revert` emits only +1 opens on the spot path (zero close signals),
# so ungated it opens once and never exits — a degenerate one-trade leg that
# no gate study can read. Pair it with a documented exit stack so it produces
# real round trips. Recorded in the report's family-mapping section.
EXEMPLAR_CLOSE_OVERRIDES = {
    "atr_band_revert": {
        "close_strategies": [{"name": "tiered_tp_atr", "params": {}}],
        "stop_loss_atr_mult": 2.0,
    },
}

# Buckets on H at entry. NaN is its own bucket (#1409: the estimator returns
# NaN for insufficient data and must never be read as "measured random walk").
BUCKET_NAN = "NaN"
BUCKETS = ("<0.45", "0.45-0.50", "0.50-0.55", ">=0.55", BUCKET_NAN)

# Rolling Hurst window lengths in bars; all above HURST_DFA_MIN_POINTS (100).
HURST_WINDOWS = (128, 256, 512)

# Gate senses. A momentum family arms on HIGH H (persistence); a
# mean-reversion family arms on LOW H (anti-persistence).
SENSE_HIGH = "arms_on_high_h"
SENSE_LOW = "arms_on_low_h"
FAMILY_SENSE = {
    FAMILY_MOMENTUM: SENSE_HIGH,
    FAMILY_MEAN_REVERSION: SENSE_LOW,
}

# Hysteresis (arm, disarm) pairs, both senses. SENSE_HIGH arms when H >= arm
# and disarms when H < disarm (arm > disarm). SENSE_LOW mirrors it downward:
# arms when H <= arm, disarms when H > disarm (arm < disarm).
GATE_PAIRS = {
    FAMILY_MOMENTUM: ((0.55, 0.50), (0.60, 0.52), (0.52, 0.48)),
    FAMILY_MEAN_REVERSION: ((0.45, 0.50), (0.40, 0.48), (0.48, 0.52)),
}

# Gate state machine: INITIAL STATE ARMED (ungated behaviour until evidence
# says otherwise), and NaN neither arms nor disarms (the state holds). This
# makes the gate a pure overlay and stops warmup bars manufacturing benefit.
GATE_INITIAL_ARMED = True

# Sizing variant: signed edge e, multiplier m = clamp(1 + gain*e, lo, hi);
# NaN gives m = 1.0 (never 0.5-as-H, never a fabricated edge).
SIZING_GAINS = (2.5, 5.0)
SIZING_CLAMP_LO = 0.0
SIZING_CLAMP_HI = 1.5
SIZING_NAN_MULTIPLIER = 1.0

# Decision floors. A config with too few suppressed or too few kept trades is
# degenerate and unrecommendable — this is what stops "trade nothing" winning.
MIN_SUPPRESSED_TRADES = 20
MIN_KEPT_TRADES = 30

# Economic acceptance thresholds.
RETURN_TOLERANCE_PP = 1.0        # absolute floor on allowed return give-up
RETURN_TOLERANCE_FRAC = 0.1      # or 10% of |ungated return|, whichever larger
HELD_OUT_MIN_NON_DEGRADING = 2   # of the 3 held-out windows

# Inference.
ALPHA = 0.05
N_PERM = 10000
SEED = ISSUE  # 1410 — fixed so a re-run reproduces every p-value

# Bars of extra lead pulled ahead of a window start so the shifted decision
# series is defined on the window's first bar (needs 2; 4 is margin).
STAMP_LEAD_BARS = 4

# Warm-up depth a DATASET must carry BEFORE the earliest window start for the
# entry stamp to be defined on that window's first bar. Rolling H at bar i
# needs bars [i-W+1, i], and the entry stamp at fill bar p reads the rolling
# value at p-2 (decision_series shift 1 + fill shift 1), so p >= W+1 is the
# exact requirement; WARMUP_MARGIN_BARS = 2 keeps one bar of margin and
# matches the pre-registered "window start minus W+2 bars" convention.
WARMUP_MARGIN_BARS = 2

_DEFAULT_JSON_OUT = os.path.join(_THIS_DIR, "hurst_1410_gate_calibration.json")
# NOT "hurst_gate_calibration.md" — that is the live-evidence contract path owned
# by hurst_1422_gate_power.py (see the REPORT-PATH CONTRACT note in the module
# docstring). A --render-only run here must never overwrite it.
_DEFAULT_REPORT_OUT = os.path.join(_THIS_DIR, "hurst_1410_gate_calibration.md")

WINDOW_ORDER = tuple(
    sorted(WINDOWS, key=lambda w: (WINDOWS[w][0], w))
)  # chronological by window start — the dedup iteration order


# ---------------------------------------------------------------------------
# Pure helpers (unit-tested without data access).
# ---------------------------------------------------------------------------

def required_lead_bars(hurst_window: int) -> int:
    """Bars a dataset must carry BEFORE the earliest window start so the entry
    stamp is defined on that window's first bar. See ``WARMUP_MARGIN_BARS``."""
    return int(hurst_window) + WARMUP_MARGIN_BARS


def warmup_lead_bars(index, first_needed) -> int:
    """Bars in ``index`` that fall strictly BEFORE ``first_needed``."""
    return int(pd.Index(index).searchsorted(pd.Timestamp(first_needed),
                                            side="left"))


def warmup_audit(leads: dict, hurst_windows: Sequence[int]) -> dict:
    """Record — never assume — the warm-up depth every scored bar rests on.

    The report claims the ``NaN`` bucket is empty as a property of the harness.
    That claim is only true when every dataset carries ``required_lead_bars``
    of history before the earliest window start. This turns the claim into a
    measurement the run stores, so a reader of the committed JSON can tell
    which case produced the shipped tables.
    """
    required = max((required_lead_bars(hw) for hw in hurst_windows), default=0)
    short = sorted(k for k, v in leads.items() if int(v) < required)
    return {
        "required_bars": required,
        "lead_bars": {k: int(leads[k]) for k in sorted(leads)},
        "min_lead_bars": min((int(v) for v in leads.values()), default=0),
        "sufficient": not short,
        "insufficient_datasets": short,
    }


# Metadata stored beside every cached rolling-Hurst array, so a cache HIT is
# indistinguishable from a fresh recompute. Length alone is not enough: the
# NaN coverage of an array depends on the ``first_needed`` it was computed
# with, and two runs over one dataset with different --windows selections
# produce same-length arrays with very different coverage.
CACHE_META_FIELDS = 4  # first_needed_ns, index_first_ns, index_last_ns, length


def cache_meta(index, first_needed) -> np.ndarray:
    """Identity of one cached rolling-Hurst array."""
    idx = pd.Index(index)
    return np.array([
        pd.Timestamp(first_needed).value,
        pd.Timestamp(idx[0]).value,
        pd.Timestamp(idx[-1]).value,
        len(idx),
    ], dtype=np.int64)


def cache_entry_is_usable(meta, index, first_needed) -> bool:
    """True only when reusing the cached array cannot change any read value.

    Requires the SAME bar series (first bar, last bar, length) and a cached
    ``first_needed`` no LATER than the one this run needs — an array computed
    from further back is a superset of the requested coverage, while one
    computed from later carries NaN on bars this run reads.
    """
    if meta is None:
        return False
    arr = np.asarray(meta)
    if arr.shape != (CACHE_META_FIELDS,):
        return False
    want = cache_meta(index, first_needed)
    if not (int(arr[1]) == int(want[1]) and int(arr[2]) == int(want[2])
            and int(arr[3]) == int(want[3])):
        return False
    return int(arr[0]) <= int(want[0])


def bucket_label(h) -> str:
    """Bucket for one H reading.

    NaN / None is its own bucket and is NEVER coerced to 0.5 — "unknown"
    stays distinguishable from "measured random walk" (#1409). Boundaries are
    half-open upward: ``<0.45``, ``[0.45,0.50)``, ``[0.50,0.55)``, ``>=0.55``.
    """
    if h is None:
        return BUCKET_NAN
    value = float(h)
    if not math.isfinite(value):
        return BUCKET_NAN
    if value < 0.45:
        return "<0.45"
    if value < 0.50:
        return "0.45-0.50"
    if value < 0.55:
        return "0.50-0.55"
    return ">=0.55"


def validate_gate_pair(arm: float, disarm: float, sense: str) -> None:
    """Reject a pair whose thresholds do not form hysteresis in its sense."""
    if sense == SENSE_HIGH:
        if not arm > disarm:
            raise ValueError(
                f"{SENSE_HIGH} pair needs arm > disarm, got arm={arm} disarm={disarm}")
    elif sense == SENSE_LOW:
        if not arm < disarm:
            raise ValueError(
                f"{SENSE_LOW} pair needs arm < disarm, got arm={arm} disarm={disarm}")
    else:
        raise ValueError(f"unknown gate sense {sense!r}")


def hysteresis_mask(values: Sequence[float], arm: float, disarm: float,
                    sense: str) -> np.ndarray:
    """Armed-state boolean series over a decision-H series.

    Starts ARMED (``GATE_INITIAL_ARMED``). A NaN reading neither arms nor
    disarms — the state holds — so warmup bars never manufacture a gate
    benefit and "unknown" is never treated as an edge signal.
    """
    validate_gate_pair(arm, disarm, sense)
    armed = bool(GATE_INITIAL_ARMED)
    out = np.empty(len(values), dtype=bool)
    for i, raw in enumerate(values):
        value = float("nan") if raw is None else float(raw)
        if math.isfinite(value):
            if sense == SENSE_HIGH:
                if value >= arm:
                    armed = True
                elif value < disarm:
                    armed = False
            else:  # SENSE_LOW
                if value <= arm:
                    armed = True
                elif value > disarm:
                    armed = False
        out[i] = armed
    return out


def mask_entry_signals(signal: Sequence[float], armed: Sequence[bool]) -> np.ndarray:
    """Zero entry signals (``> 0``) on disarmed bars; closes (``< 0``) pass.

    A gate may only stop NEW exposure. Suppressing a close would strand a
    position the ungated arm would have exited, which is a different (and
    unsafe) experiment.
    """
    sig = np.asarray(signal, dtype=float)
    arm = np.asarray(armed, dtype=bool)
    if sig.shape != arm.shape:
        raise ValueError(f"signal/armed length mismatch: {sig.shape} vs {arm.shape}")
    out = np.where((sig > 0) & (~arm), 0.0, sig)
    return out.astype(int)


def size_multiplier(h, sense: str, gain: float) -> float:
    """Per-trade size multiplier from one H reading.

    ``e`` is the signed edge in the family's sense; ``m = clamp(1 + gain*e)``.
    NaN gives exactly 1.0 — unchanged size. NaN is never mapped to H=0.5 and
    never produces a size opinion of its own.
    """
    if sense not in (SENSE_HIGH, SENSE_LOW):
        raise ValueError(f"unknown gate sense {sense!r}")
    if h is None:
        return SIZING_NAN_MULTIPLIER
    value = float(h)
    if not math.isfinite(value):
        return SIZING_NAN_MULTIPLIER
    edge = (value - 0.5) if sense == SENSE_HIGH else (0.5 - value)
    return float(min(SIZING_CLAMP_HI, max(SIZING_CLAMP_LO, 1.0 + gain * edge)))


def compound_equity(returns_pct: Sequence[float],
                    multipliers: Optional[Sequence[float]] = None) -> tuple:
    """Trade-sequence compounded return and max drawdown, both in percent.

    ``equity *= 1 + m_i * r_i / 100``. Equity is floored at 0 and stays there
    (a busted account cannot recover), mirroring the backtester's liquidation
    floor convention. Returns ``(total_return_pct, max_drawdown_pct)`` with the
    drawdown reported as a NON-POSITIVE number, same sign convention as
    ``Backtester.max_drawdown_pct``.
    """
    rets = list(returns_pct)
    if multipliers is None:
        mults = [1.0] * len(rets)
    else:
        mults = list(multipliers)
        if len(mults) != len(rets):
            raise ValueError(
                f"multiplier count {len(mults)} != return count {len(rets)}")
    equity = 1.0
    peak = 1.0
    max_dd = 0.0
    for r, m in zip(rets, mults):
        equity *= 1.0 + float(m) * float(r) / 100.0
        if equity < 0.0:
            equity = 0.0
        if equity > peak:
            peak = equity
        if peak > 0:
            dd = (equity - peak) / peak * 100.0
            if dd < max_dd:
                max_dd = dd
    return round((equity - 1.0) * 100.0, 6), round(max_dd, 6)


def chop_loss(returns_pct: Sequence[float]) -> float:
    """Summed MAGNITUDE of losing trades — the cost a chop filter should cut.

    Reported as a non-negative number; smaller is better.
    """
    return round(float(sum(-float(r) for r in returns_pct if float(r) < 0.0)), 6)


def win_rate(returns_pct: Sequence[float]) -> Optional[float]:
    rets = [float(r) for r in returns_pct]
    if not rets:
        return None
    return round(sum(1 for r in rets if r > 0) / len(rets) * 100.0, 4)


def dedup_entries(rows: Sequence[dict]) -> list:
    """Collapse the same physical entry seen through overlapping windows.

    Key is ``(strategy, symbol, timeframe, entry_date)`` — the eval_windows
    entry-date convention, extended with strategy/symbol/timeframe so two
    genuinely different physical entries that merely share a timestamp are
    never collapsed. Windows are iterated in CHRONOLOGICAL start order and the
    first occurrence wins, so the result is independent of scheduling order.
    """
    order = {name: i for i, name in enumerate(WINDOW_ORDER)}
    ordered = sorted(
        rows,
        key=lambda r: (
            order.get(r["window"], len(order)),
            str(r["strategy"]),
            str(r["symbol"]),
            str(r["timeframe"]),
            str(r["entry_date"]),
        ),
    )
    seen = set()
    out = []
    for row in ordered:
        key = (str(row["strategy"]), str(row["symbol"]),
               str(row["timeframe"]), str(row["entry_date"]))
        if key in seen:
            continue
        seen.add(key)
        out.append(row)
    return out


def permutation_pvalue_group_diff(values: Sequence[float],
                                  suppressed: Sequence[bool],
                                  n_perm: int = N_PERM,
                                  seed: int = SEED) -> Optional[float]:
    """One-sided p for "suppressed trades are WORSE than kept trades".

    Statistic is ``mean(kept) - mean(suppressed)``; the null shuffles the
    kept/suppressed labels across the pooled trades. ``p = (1 + #{shuffled >=
    observed}) / (n_perm + 1)`` — the add-one convention, so p is never 0.
    Returns None when either group is empty (no testable contrast).
    """
    vals = np.asarray(values, dtype=float)
    mask = np.asarray(suppressed, dtype=bool)
    if vals.shape != mask.shape:
        raise ValueError("values/suppressed length mismatch")
    n = vals.size
    k = int(mask.sum())
    if n == 0 or k == 0 or k == n:
        return None
    total = float(vals.sum())
    sup_sum = float(vals[mask].sum())
    observed = (total - sup_sum) / (n - k) - sup_sum / k
    rng = np.random.default_rng(seed)
    ge = 0
    remaining = int(n_perm)
    chunk_size = max(1, min(remaining, max(1, 5_000_000 // max(n, 1))))
    while remaining > 0:
        take = min(chunk_size, remaining)
        keys = rng.random((take, n))
        picks = np.argpartition(keys, k - 1, axis=1)[:, :k]
        sums = vals[picks].sum(axis=1)
        stats = (total - sums) / (n - k) - sums / k
        ge += int(np.count_nonzero(stats >= observed))
        remaining -= take
    return round((1.0 + ge) / (n_perm + 1.0), 6)


def permutation_pvalue_weighted(returns: Sequence[float],
                                multipliers: Sequence[float],
                                n_perm: int = N_PERM,
                                seed: int = SEED) -> Optional[float]:
    """One-sided p for "this multiplier assignment beats a random one".

    Statistic is ``mean(m_i * r_i)``; the null shuffles the multipliers across
    trades, which keeps both marginal distributions fixed and tests only the
    PAIRING (the thing a size rule claims to get right). Add-one convention.
    Returns None when the multipliers carry no variation (nothing to shuffle).
    """
    rets = np.asarray(returns, dtype=float)
    mults = np.asarray(multipliers, dtype=float)
    if rets.shape != mults.shape:
        raise ValueError("returns/multipliers length mismatch")
    n = rets.size
    if n == 0 or float(np.ptp(mults)) == 0.0:
        return None
    observed = float(np.mean(rets * mults))
    rng = np.random.default_rng(seed)
    ge = 0
    remaining = int(n_perm)
    chunk_size = max(1, min(remaining, max(1, 5_000_000 // max(n, 1))))
    tile = np.broadcast_to(mults, (chunk_size, n))
    while remaining > 0:
        take = min(chunk_size, remaining)
        # rng.permuted shuffles each row independently in O(n) — cheaper than
        # argsorting a random key matrix, and the null is identical.
        shuffled = rng.permuted(np.array(tile[:take], copy=True), axis=1)
        stats = (shuffled * rets).mean(axis=1)
        ge += int(np.count_nonzero(stats >= observed))
        remaining -= take
    return round((1.0 + ge) / (n_perm + 1.0), 6)


def config_verdict(cfg: dict) -> tuple:
    """Pure accept/reject for one swept config. Returns ``(passed, reasons)``.

    A config wins only when EVERY pre-registered rule holds:
      * volume floors met (suppressed and kept),
      * BH-corrected significant,
      * on BOTH protocol windows: drawdown reduced, chop loss reduced, and
        return not given up beyond the tolerance,
      * drawdown does not degrade on at least ``HELD_OUT_MIN_NON_DEGRADING``
        of the three held-out windows.

    ``dd_delta`` and ``chop_delta`` are MAGNITUDE deltas (gated minus
    ungated); negative means improvement.
    """
    reasons = []
    if int(cfg.get("n_suppressed") or 0) < MIN_SUPPRESSED_TRADES:
        reasons.append(
            f"only {cfg.get('n_suppressed')} suppressed trades "
            f"(floor {MIN_SUPPRESSED_TRADES})")
    if int(cfg.get("n_kept") or 0) < MIN_KEPT_TRADES:
        reasons.append(
            f"only {cfg.get('n_kept')} kept trades (floor {MIN_KEPT_TRADES})")
    if not cfg.get("bh_reject"):
        reasons.append(
            f"not significant after Benjamini-Hochberg (raw p={cfg.get('p_raw')})")
    windows = cfg.get("windows") or {}
    for name in PROTOCOL_WINDOWS:
        row = windows.get(name)
        if not row or not row.get("n_legs"):
            reasons.append(f"{name}: no legs")
            continue
        if not (row["dd_delta"] < 0):
            reasons.append(f"{name}: drawdown not reduced ({row['dd_delta']:+.2f} pp)")
        if not (row["chop_delta"] < 0):
            reasons.append(f"{name}: chop loss not reduced ({row['chop_delta']:+.2f} pp)")
        tol = max(RETURN_TOLERANCE_PP, RETURN_TOLERANCE_FRAC * abs(row["ret_ungated"]))
        if not (row["ret_gated"] >= row["ret_ungated"] - tol):
            reasons.append(
                f"{name}: return give-up {row['ret_ungated'] - row['ret_gated']:.2f} pp "
                f"exceeds tolerance {tol:.2f} pp")
    non_degrading = sum(
        1 for name in HELD_OUT_WINDOWS
        if (windows.get(name) or {}).get("n_legs") and windows[name]["dd_delta"] <= 0
    )
    if non_degrading < HELD_OUT_MIN_NON_DEGRADING:
        reasons.append(
            f"drawdown holds on only {non_degrading}/{len(HELD_OUT_WINDOWS)} held-out "
            f"windows (need {HELD_OUT_MIN_NON_DEGRADING})")
    return (not reasons), reasons


def protocol_dd_reduction(cfg: dict) -> float:
    """Pooled protocol-window drawdown reduction, positive = better.

    Tie-break metric: the summed magnitude improvement over ``is`` and ``oos``.
    """
    windows = cfg.get("windows") or {}
    total = 0.0
    for name in PROTOCOL_WINDOWS:
        row = windows.get(name)
        if row and row.get("n_legs"):
            total += -float(row["dd_delta"])
    return round(total, 6)


def decide_recommendation(configs: Sequence[dict]) -> dict:
    """Mechanically derive the Recommendation from the swept configs.

    Returns ``{"verdict": "config"|"inconclusive", "families": {...},
    "justification": str}``. A family with at least one passing config gets its
    best one (largest pooled protocol drawdown reduction); a family with none
    gets an explicit no-gate statement. With no family winning anywhere the
    verdict is ``inconclusive`` and the report prints exactly ``INCONCLUSIVE``.
    """
    families = {}
    for family in FAMILIES:
        own = [c for c in configs if c.get("family") == family]
        passing = []
        for cfg in own:
            ok, reasons = config_verdict(cfg)
            if ok:
                passing.append(cfg)
        if passing:
            best = sorted(
                passing,
                key=lambda c: (-protocol_dd_reduction(c), c["config_id"]),
            )[0]
            families[family] = {"winner": best, "n_tested": len(own),
                                "n_passing": len(passing)}
        else:
            families[family] = {"winner": None, "n_tested": len(own),
                                "n_passing": 0}
    any_winner = any(v["winner"] for v in families.values())
    if any_winner:
        return {"verdict": "config", "families": families, "justification": ""}
    tested = sum(v["n_tested"] for v in families.values())
    considered = [c for c in configs if c.get("family") in families]
    n_significant = sum(1 for c in considered if c.get("bh_reject"))
    n_economics_only = 0
    for cfg in considered:
        _, reasons = config_verdict(cfg)
        if reasons and all("Benjamini-Hochberg" in r for r in reasons):
            n_economics_only += 1
    return {
        "verdict": "inconclusive",
        "families": families,
        "justification": (
            f"No configuration of the {tested} tested passed the pre-registered "
            f"acceptance rule. {n_significant} reached Benjamini-Hochberg "
            f"significance at alpha={ALPHA}; {n_economics_only} met every economic "
            f"condition but failed only on significance. A Hurst entry gate is "
            f"not supported by this evidence."
        ),
    }


# ---------------------------------------------------------------------------
# Hurst series (look-ahead safe).
# ---------------------------------------------------------------------------

def rolling_hurst(close: pd.Series, window: int,
                  first_needed: Optional[pd.Timestamp] = None) -> pd.Series:
    """Rolling H: the value at bar ``i`` uses closes ``[i-window+1, i]``.

    This is the BAR-CLOSE series; it is NOT the decision series. Bars with
    fewer than ``window`` observations are NaN, as is any window the estimator
    itself refuses (#1409 semantics: NaN means unknown).

    ``first_needed`` trims the computation to the bars a caller actually reads
    (with ``STAMP_LEAD_BARS`` of margin for the shifts) — purely a cost cut,
    it never changes a computed value, because H at bar ``i`` depends only on
    that bar's own trailing window.
    """
    if window < 2:
        raise ValueError(f"hurst window must be >= 2, got {window}")
    prices = close.astype(float)
    n = len(prices)
    values = np.full(n, np.nan, dtype=float)
    start = window - 1
    if first_needed is not None:
        pos = int(prices.index.searchsorted(pd.Timestamp(first_needed)))
        start = max(start, pos - STAMP_LEAD_BARS)
    for i in range(start, n):
        values[i] = hurst_exponent(prices.iloc[i - window + 1: i + 1])
    return pd.Series(values, index=prices.index, name=f"hurst_{window}")


def decision_series(rolling: pd.Series) -> pd.Series:
    """H available to a decision taken AT signal bar N: bars through N-1.

    One ``shift(1)`` off the bar-close series. The backtester fills a bar-N
    signal at bar N+1's open, so this is strictly closed-bar information and is
    one bar MORE conservative than the live regime gate (which reads bar N's
    label for the N+1 fill). The extra bar of lag is deliberate: it makes the
    calibration immune to any bar-close ambiguity.
    """
    return rolling.shift(1)


def entry_stamp_series(rolling: pd.Series) -> pd.Series:
    """Decision H indexed by FILL bar.

    A trade's ``entry_date`` is the fill bar N+1, but the value that gated it
    was the decision value at signal bar N. That is the bar-close series
    shifted twice.
    """
    return rolling.shift(2)


# ---------------------------------------------------------------------------
# Engine arms. `_run_arm` mirrors eval_windows.run_leg's Backtester
# construction exactly; every leg additionally verifies its ungated arm
# against run_leg itself, so the mirror can never drift silently.
# ---------------------------------------------------------------------------

_MIRRORED_LEG_KEYS = ("sharpe", "return_pct", "max_dd_pct", "ddadj", "trades",
                      "liquidated")


def slice_window(full: pd.DataFrame, window: tuple) -> pd.DataFrame:
    """The exact bar slice ``run_leg`` scores: ``[start, end)``.

    ``end=None`` (the ``oos`` window) means "latest cached bar" and skips the
    upper bound, matching run_leg.
    """
    start, end = window
    df = full[full.index >= pd.Timestamp(start)]
    if end is not None:
        df = df[df.index < pd.Timestamp(end)]
    return df


def _run_arm(reg, name: str, symbol: str, timeframe: str, df: pd.DataFrame,
             armed: Optional[np.ndarray], overrides: dict) -> Optional[dict]:
    """One Backtester run on a pre-sliced frame, optionally entry-masked.

    Kwargs are byte-identical to ``eval_windows.run_leg``'s no-regime,
    no-profile path: FEE_PLATFORM fee model, the Backtester's 5 bps slippage
    default, ``ohlc_walk`` intrabar resolution, registry default params.
    """
    from atr import ensure_atr_indicator
    from backtester import Backtester
    from run_backtest import FUNDING_COLUMN_STRATEGIES

    if name in FUNDING_COLUMN_STRATEGIES:
        raise ValueError(
            f"{name} needs the funding column; this study's exemplars must not")
    if df.empty:
        return None

    strat = reg.STRATEGY_REGISTRY.get(name)
    if strat is None:
        raise SystemExit(f"Unknown strategy {name!r}")
    strat_params = strat["default_params"]
    close_strategies = overrides.get("close_strategies")

    df_signals = reg.apply_strategy(name, df, strat_params)
    if armed is not None:
        df_signals = df_signals.copy()
        df_signals["signal"] = mask_entry_signals(
            df_signals["signal"].fillna(0).to_numpy(), armed)
    if close_strategies:
        df_signals = ensure_atr_indicator(df_signals)

    bt = Backtester(
        initial_capital=DEFAULT_CAPITAL, platform=FEE_PLATFORM,
        open_strategy={"name": name, "params": dict(strat_params or {})},
        close_strategies=close_strategies,
        direction=None, invert_signal=False,
        stop_loss_atr_mult=overrides.get("stop_loss_atr_mult"),
        trailing_stop_atr_mult=overrides.get("trailing_stop_atr_mult"),
        profile_allocation=None,
        regime_enabled=False,
        regime_period=14,
        regime_adx_threshold=20.0,
        allowed_regimes=None,
        regime_windows_spec=None,
        commission_pct=None,
        intrabar_resolution="ohlc_walk",
    )
    results = bt.run(df_signals, strategy_name=name, symbol=symbol,
                     timeframe=timeframe, params=strat_params, save=False)
    leg = leg_from_results(results)
    leg["trade_samples"] = trade_samples_from_results(results)
    return leg


def _leg_metrics(leg: Optional[dict]) -> dict:
    """Bar-level engine metrics + the trade-derived chop loss for one arm."""
    if leg is None:
        return {"return_pct": 0.0, "max_dd_pct": 0.0, "chop_loss": 0.0, "trades": 0}
    rets = [s["pnl_pct_net"] for s in leg.get("trade_samples") or []]
    return {
        "return_pct": float(leg["return_pct"]),
        "max_dd_pct": float(leg["max_dd_pct"]),
        "chop_loss": chop_loss(rets),
        "trades": int(leg["trades"]),
    }


# ---------------------------------------------------------------------------
# Per-leg work unit.
# ---------------------------------------------------------------------------

# Config ids appear inside Markdown table cells, so the separator must not be
# a pipe — GitHub splits cells on `|` even inside a code span.
CONFIG_ID_SEP = "/"


def gate_config_id(family: str, hurst_window: int, arm: float, disarm: float) -> str:
    return CONFIG_ID_SEP.join(
        (family, "gate", f"W{hurst_window}", f"arm{arm:g}", f"dis{disarm:g}"))


def size_config_id(family: str, hurst_window: int, gain: float) -> str:
    return CONFIG_ID_SEP.join(
        (family, "size", f"W{hurst_window}", f"gain{gain:g}"))


def build_leg(reg, family: str, exemplar: str, symbol: str, timeframe: str,
              window_name: str, full: pd.DataFrame, hurst_by_window: dict,
              verify_mirror: bool = True) -> Optional[dict]:
    """Every arm for one (exemplar, dataset, window) cell.

    Returns the ungated leg's stamped trades plus one gated leg per
    (hurst window x gate pair), or None when the cell has no bars.
    """
    window = WINDOWS[window_name]
    overrides = EXEMPLAR_CLOSE_OVERRIDES.get(exemplar, {})
    df = slice_window(full, window)
    if df.empty:
        return None
    sense = FAMILY_SENSE[family]

    ungated = _run_arm(reg, exemplar, symbol, timeframe, df, None, overrides)
    if ungated is None:
        return None

    # Mirror guard: the ungated arm must reproduce eval_windows.run_leg
    # exactly. Without this the gated arms could silently be scored on a
    # different harness than the M1 baseline they are compared against.
    mirror_ok = None
    if verify_mirror:
        reference = run_leg(reg, exemplar, None, symbol, timeframe, window,
                            capital=DEFAULT_CAPITAL,
                            close_strategies=overrides.get("close_strategies"),
                            stop_loss_atr_mult=overrides.get("stop_loss_atr_mult"),
                            trailing_stop_atr_mult=overrides.get("trailing_stop_atr_mult"),
                            keep_trades=True)
        mirror_ok = reference is not None and all(
            reference.get(k) == ungated.get(k) for k in _MIRRORED_LEG_KEYS)
        if not mirror_ok:
            raise AssertionError(
                f"gated-arm mirror diverged from eval_windows.run_leg for "
                f"{exemplar} {symbol} {timeframe} {window_name}: "
                f"{ {k: (reference or {}).get(k) for k in _MIRRORED_LEG_KEYS} } vs "
                f"{ {k: ungated.get(k) for k in _MIRRORED_LEG_KEYS} }")

    index_keys = [str(ts) for ts in df.index]
    key_pos = {k: i for i, k in enumerate(index_keys)}

    # H stamps per hurst window, indexed by FILL bar (shift(2)).
    stamps = {}
    decisions = {}
    for hw, rolling in hurst_by_window.items():
        stamps[hw] = entry_stamp_series(rolling).reindex(df.index).to_numpy(dtype=float)
        decisions[hw] = decision_series(rolling).reindex(df.index).to_numpy(dtype=float)

    # Armed state per gate config, on the leg's own decision series (initial
    # state armed), then shifted one bar so it is indexed by FILL bar.
    armed_signal_bar = {}
    armed_fill_bar = {}
    for hw in hurst_by_window:
        for arm, disarm in GATE_PAIRS[family]:
            cid = gate_config_id(family, hw, arm, disarm)
            mask = hysteresis_mask(decisions[hw], arm, disarm, sense)
            armed_signal_bar[cid] = mask
            shifted = np.empty_like(mask)
            shifted[0] = bool(GATE_INITIAL_ARMED)
            shifted[1:] = mask[:-1]
            armed_fill_bar[cid] = shifted

    trades = []
    for sample in ungated.get("trade_samples") or []:
        key = str(sample["entry_date"])
        pos = key_pos.get(key)
        if pos is None:
            raise AssertionError(
                f"trade entry_date {key!r} is not a bar of the {window_name} slice "
                f"for {exemplar} {symbol} {timeframe}")
        trades.append({
            "strategy": exemplar,
            "symbol": symbol,
            "timeframe": timeframe,
            "window": window_name,
            "entry_date": key,
            "pnl_pct_net": float(sample["pnl_pct_net"]),
            "h": {hw: (None if not math.isfinite(stamps[hw][pos])
                       else float(stamps[hw][pos])) for hw in hurst_by_window},
            "armed": {cid: bool(armed_fill_bar[cid][pos]) for cid in armed_fill_bar},
        })

    gated = {}
    for cid, mask in armed_signal_bar.items():
        arm_leg = _run_arm(reg, exemplar, symbol, timeframe, df, mask, overrides)
        gated[cid] = _leg_metrics(arm_leg)

    return {
        "family": family,
        "strategy": exemplar,
        "symbol": symbol,
        "timeframe": timeframe,
        "dataset": dataset_key(symbol, timeframe),
        "window": window_name,
        "bars": int(len(df)),
        "mirror_verified": mirror_ok,
        "ungated": _leg_metrics(ungated),
        "gated": gated,
        "trades": trades,
    }


# ---------------------------------------------------------------------------
# Aggregation.
# ---------------------------------------------------------------------------

def bucket_tables(trades: Sequence[dict], hurst_window: int) -> dict:
    """Part A: per-bucket outcome table over an already-deduped trade pool."""
    by_bucket = {b: [] for b in BUCKETS}
    for t in trades:
        by_bucket[bucket_label(t["h"].get(hurst_window))].append(t["pnl_pct_net"])
    out = {}
    for bucket, rets in by_bucket.items():
        total_return, max_dd = compound_equity(rets)
        out[bucket] = {
            "trades": len(rets),
            "win_rate_pct": win_rate(rets),
            "mean_pnl_pct_net": round(float(np.mean(rets)), 6) if rets else None,
            "median_pnl_pct_net": round(float(np.median(rets)), 6) if rets else None,
            "compounded_return_pct": total_return,
            "trade_seq_max_dd_pct": max_dd,
            "chop_loss_pct": chop_loss(rets),
        }
    return out


def _window_rows_gate(legs: Sequence[dict], family: str, config_id: str) -> dict:
    """Per-window mean deltas for a gate config, across that family's legs."""
    rows = {}
    for wname in WINDOWS:
        cells = [lg for lg in legs
                 if lg["family"] == family and lg["window"] == wname
                 and config_id in lg["gated"]]
        if not cells:
            rows[wname] = {"n_legs": 0}
            continue
        dd_deltas, chop_deltas, ret_g, ret_u, trades_g, trades_u = [], [], [], [], [], []
        for lg in cells:
            u, g = lg["ungated"], lg["gated"][config_id]
            dd_deltas.append(abs(g["max_dd_pct"]) - abs(u["max_dd_pct"]))
            chop_deltas.append(g["chop_loss"] - u["chop_loss"])
            ret_g.append(g["return_pct"])
            ret_u.append(u["return_pct"])
            trades_g.append(g["trades"])
            trades_u.append(u["trades"])
        rows[wname] = {
            "n_legs": len(cells),
            "dd_delta": round(float(np.mean(dd_deltas)), 6),
            "chop_delta": round(float(np.mean(chop_deltas)), 6),
            "ret_gated": round(float(np.mean(ret_g)), 6),
            "ret_ungated": round(float(np.mean(ret_u)), 6),
            "trades_gated": int(sum(trades_g)),
            "trades_ungated": int(sum(trades_u)),
        }
    return rows


def _window_rows_size(legs: Sequence[dict], family: str, hurst_window: int,
                      gain: float) -> dict:
    """Per-window mean deltas for a sizing config.

    Both arms are TRADE-GRANULAR re-compoundings of the same ungated trade
    sequence (baseline m=1 vs the sized multipliers), so the comparison is
    like-for-like. It is NOT comparable to the bar-level Part B drawdowns.
    """
    sense = FAMILY_SENSE[family]
    rows = {}
    for wname in WINDOWS:
        cells = [lg for lg in legs
                 if lg["family"] == family and lg["window"] == wname]
        if not cells:
            rows[wname] = {"n_legs": 0}
            continue
        dd_deltas, chop_deltas, ret_g, ret_u = [], [], [], []
        n_used = 0
        for lg in cells:
            rets = [t["pnl_pct_net"] for t in lg["trades"]]
            if not rets:
                continue
            mults = [size_multiplier(t["h"].get(hurst_window), sense, gain)
                     for t in lg["trades"]]
            base_ret, base_dd = compound_equity(rets)
            sized_ret, sized_dd = compound_equity(rets, mults)
            dd_deltas.append(abs(sized_dd) - abs(base_dd))
            chop_deltas.append(
                chop_loss([m * r for m, r in zip(mults, rets)]) - chop_loss(rets))
            ret_g.append(sized_ret)
            ret_u.append(base_ret)
            n_used += 1
        if not n_used:
            rows[wname] = {"n_legs": 0}
            continue
        rows[wname] = {
            "n_legs": n_used,
            "dd_delta": round(float(np.mean(dd_deltas)), 6),
            "chop_delta": round(float(np.mean(chop_deltas)), 6),
            "ret_gated": round(float(np.mean(ret_g)), 6),
            "ret_ungated": round(float(np.mean(ret_u)), 6),
            "trades_gated": int(sum(len(lg["trades"]) for lg in cells)),
            "trades_ungated": int(sum(len(lg["trades"]) for lg in cells)),
        }
    return rows


def build_configs(legs: Sequence[dict], pooled: dict, hurst_windows: Sequence[int],
                  n_perm: int, seed: int) -> list:
    """Every swept config with its raw p-value and per-window economics."""
    configs = []
    for family in FAMILIES:
        sense = FAMILY_SENSE[family]
        trades = pooled.get(family) or []
        for hw in hurst_windows:
            for arm, disarm in GATE_PAIRS[family]:
                cid = gate_config_id(family, hw, arm, disarm)
                suppressed = [not t["armed"][cid] for t in trades
                              if cid in t["armed"]]
                values = [t["pnl_pct_net"] for t in trades if cid in t["armed"]]
                p_raw = permutation_pvalue_group_diff(
                    values, suppressed, n_perm=n_perm, seed=seed)
                configs.append({
                    "config_id": cid,
                    "family": family,
                    "mode": "gate",
                    "sense": sense,
                    "hurst_window": hw,
                    "arm": arm,
                    "disarm": disarm,
                    "gain": None,
                    "n_pooled_trades": len(values),
                    "n_suppressed": int(sum(suppressed)),
                    "n_kept": int(len(suppressed) - sum(suppressed)),
                    "p_raw": p_raw,
                    "windows": _window_rows_gate(legs, family, cid),
                })
            for gain in SIZING_GAINS:
                cid = size_config_id(family, hw, gain)
                rets = [t["pnl_pct_net"] for t in trades]
                mults = [size_multiplier(t["h"].get(hw), sense, gain) for t in trades]
                p_raw = permutation_pvalue_weighted(
                    rets, mults, n_perm=n_perm, seed=seed)
                # A sizing config's "suppressed" analogue is the down-weighted
                # side (m < 1); the same volume floors then apply unchanged.
                n_down = int(sum(1 for m in mults if m < 1.0))
                configs.append({
                    "config_id": cid,
                    "family": family,
                    "mode": "size",
                    "sense": sense,
                    "hurst_window": hw,
                    "arm": None,
                    "disarm": None,
                    "gain": gain,
                    "n_pooled_trades": len(rets),
                    "n_suppressed": n_down,
                    "n_kept": int(len(mults) - n_down),
                    "p_raw": p_raw,
                    "windows": _window_rows_size(legs, family, hw, gain),
                })
    return configs


def apply_bh(configs: Sequence[dict], alpha: float = ALPHA) -> None:
    """One Benjamini-Hochberg correction over EVERY tested hypothesis.

    Untestable configs (p is None — no contrast to test) are excluded from the
    p-value list but still counted in the BH denominator via ``family_size``,
    so dropping them can never make the correction more permissive.
    """
    testable = [c for c in configs if c.get("p_raw") is not None]
    if not testable:
        for cfg in configs:
            cfg["bh_reject"] = False
        return
    flags = benjamini_hochberg([c["p_raw"] for c in testable], alpha=alpha,
                               family_size=len(configs))
    for cfg, flag in zip(testable, flags):
        cfg["bh_reject"] = bool(flag)
    for cfg in configs:
        cfg.setdefault("bh_reject", False)


# ---------------------------------------------------------------------------
# Report rendering. The Recommendation is always the FINAL section and is a
# mechanical render of decide_recommendation — never hand-written.
# ---------------------------------------------------------------------------

def _fmt(value, digits: int = 2, suffix: str = "") -> str:
    if value is None:
        return "-"
    if isinstance(value, float) and not math.isfinite(value):
        return "-"
    return f"{value:.{digits}f}{suffix}"


def render_recommendation(decision: dict, configs: Sequence[dict]) -> str:
    """The `## Recommendation` body: per-family config lines, or INCONCLUSIVE."""
    lines = ["## Recommendation", ""]
    if decision["verdict"] == "inconclusive":
        lines.append("INCONCLUSIVE")
        lines.append("")
        lines.append(decision["justification"])
        lines.append("")
        lines.append(
            "Do not build a Hurst entry gate on this evidence. Re-run this "
            "study before revisiting the question.")
        return "\n".join(lines) + "\n"

    for family in FAMILIES:
        entry = decision["families"][family]
        winner = entry["winner"]
        lines.append(f"### {family}")
        lines.append("")
        if winner is None:
            lines.append(
                f"No configuration of the {entry['n_tested']} tested beat ungated on "
                f"drawdown under the pre-registered rule. Do not gate or size the "
                f"{family} family on the Hurst exponent.")
            lines.append("")
            continue
        proto = {w: (winner["windows"].get(w) or {}) for w in PROTOCOL_WINDOWS}
        evidence = "; ".join(
            f"{w}: drawdown {_fmt(proto[w].get('dd_delta'), 2, ' pp')}, "
            f"chop {_fmt(proto[w].get('chop_delta'), 2, ' pp')}, "
            f"return {_fmt(proto[w].get('ret_gated'))} vs "
            f"{_fmt(proto[w].get('ret_ungated'))}"
            for w in PROTOCOL_WINDOWS)
        if winner["mode"] == "gate":
            lines.append(f"- Mode: **gate** (hard entry gate, hysteresis)")
            lines.append(f"- Arm / disarm: **{winner['arm']:g} / {winner['disarm']:g}** "
                         f"({winner['sense']})")
        else:
            lines.append(f"- Mode: **size** (entry size multiplier, no hard gate)")
            lines.append(f"- Gain: **{winner['gain']:g}**, "
                         f"m = clamp(1 + gain x e, {SIZING_CLAMP_LO:g}, "
                         f"{SIZING_CLAMP_HI:g}), NaN -> "
                         f"{SIZING_NAN_MULTIPLIER:g} ({winner['sense']})")
        lines.append(f"- Hurst window length: **{winner['hurst_window']} bars**")
        lines.append(f"- Evidence: raw p={_fmt(winner['p_raw'], 4)}, "
                     f"Benjamini-Hochberg significant at alpha={ALPHA}; "
                     f"{winner['n_suppressed']} suppressed / {winner['n_kept']} kept "
                     f"pooled trades; {evidence}.")
        lines.append(f"- Config id: `{winner['config_id']}` "
                     f"({entry['n_passing']}/{entry['n_tested']} tested configs passed).")
        lines.append("")
    return "\n".join(lines).rstrip() + "\n"


_NAN_POLICY_NOTE = (
    "A live consumer has a bounded fetch depth and WILL see NaN at start-up "
    "and after a data gap, so the NaN policy still has to be decided: NaN is "
    "unknown, never 0.5, and it holds the gate state.")


def render_nan_bucket_note(warmup) -> str:
    """The `NaN` bucket paragraph, rendered from the run's MEASURED warm-up.

    The emptiness of the NaN bucket is a property of the warm-up depth the
    datasets happened to carry, so the report states the measurement rather
    than asserting the property. A payload with no recorded audit (an older
    JSON re-rendered with ``--render-only``) says so instead of claiming it.
    """
    if not warmup:
        return ("The `NaN` bucket's contents depend on warm-up depth, and this "
                "run recorded no warm-up audit, so whether H was defined on "
                "every scored bar is NOT attested here. Re-run the study to "
                "record it. " + _NAN_POLICY_NOTE)
    required = warmup["required_bars"]
    if warmup["sufficient"]:
        return (f"The `NaN` bucket is EMPTY here because of the harness, not "
                f"the estimator, and this run MEASURED the condition that "
                f"makes it so. H at a scored bar needs {required} bars of "
                f"history before the earliest window start; the thinnest "
                f"dataset in this run carried {warmup['min_lead_bars']}, so H "
                f"is defined on every scored bar. On a thinner cache the run "
                f"prints a warm-up warning and this bucket carries real "
                f"trades. " + _NAN_POLICY_NOTE)
    short = ", ".join(f"`{d}`" for d in warmup["insufficient_datasets"])
    return (f"The `NaN` bucket is POPULATED here. H at a scored bar needs "
            f"{required} bars of history before the earliest window start, "
            f"and {len(warmup['insufficient_datasets'])} dataset(s) carried "
            f"less ({short}; thinnest {warmup['min_lead_bars']} bars), so the "
            f"first bars of the earliest window are unscored. Read the "
            f"`NaN` rows below as a warm-up artefact of this cache, not as an "
            f"estimator refusal. " + _NAN_POLICY_NOTE)


def render_report(payload: dict) -> str:
    """Full Markdown report. `## Recommendation` is guaranteed to be last."""
    cfgs = payload["configs"]
    decision = payload["decision"]
    const = payload["pre_registered"]
    out = []
    a = out.append

    a("# Hurst gate calibration study (#1410)")
    a("")
    a("Report-only calibration evidence for a POSSIBLE future Hurst-based entry "
      "gate. Nothing here is wired to the scheduler, to config, or to any live "
      "path. The #1409 estimator carries an advisory-only invariant, and this "
      "study honours it: the gate and size arms below are offline simulations "
      "that exist to decide whether such a gate should ever be built.")
    a("")
    a(f"Generated by `backtest/research/hurst_1410_gate_calibration.py` "
      f"(schema {payload['schema_version']}). Every number below is rendered "
      f"from `hurst_1410_gate_calibration.json`, produced by the same run.")
    a("")

    a("## Pre-registered design")
    a("")
    a("These constants live at the top of the study script and were fixed "
      "before the sweep ran. The Recommendation is the mechanical output of "
      "the acceptance rule applied to them.")
    a("")
    a(f"- Estimator: `hurst_exponent` from "
      f"`shared_strategies/open/indicators_core.py` (#1409 SSoT, detrended "
      f"fluctuation analysis over log returns). Never reimplemented here.")
    a(f"- NaN policy: NaN is its OWN bucket, never coerced to 0.5. It neither "
      f"arms nor disarms the gate (state holds) and gives a size multiplier of "
      f"exactly {const['sizing']['nan_multiplier']:g}.")
    a(f"- Hurst window lengths: {', '.join(str(w) for w in const['hurst_windows'])} bars.")
    a(f"- Buckets on H at entry: {', '.join(const['buckets'])}.")
    a(f"- Windows (verbatim from `backtest/eval_windows.py`): "
      f"{', '.join(f'{k} {v[0]}..{v[1] or 'latest'}' for k, v in const['windows'].items())}.")
    a(f"- Protocol windows: {', '.join(const['protocol_windows'])}; "
      f"held-out: {', '.join(const['held_out_windows'])}.")
    a(f"- Datasets: {', '.join(const['datasets'])}.")
    a(f"- Data source exchange: `{const['platform']}`. Fee model: "
      f"`{const['fee_platform']}` (plus the Backtester's 5 bps slippage "
      f"default). The two platform axes are independent and are never coupled.")
    a(f"- Capital per leg: {const['capital']:g}. Intrabar resolution: "
      f"`ohlc_walk`.")
    a(f"- Gate state machine: initial state "
      f"{'ARMED' if const['gate_initial_armed'] else 'DISARMED'}; hysteresis "
      f"pairs per family as listed below.")
    a(f"- Sizing: m = clamp(1 + gain x e, {const['sizing']['clamp_lo']:g}, "
      f"{const['sizing']['clamp_hi']:g}) with gains "
      f"{', '.join(f'{g:g}' for g in const['sizing']['gains'])}.")
    a(f"- Volume floors: >= {const['min_suppressed']} suppressed and "
      f">= {const['min_kept']} kept pooled trades per config.")
    a(f"- Inference: one-sided permutation test, {const['n_perm']} "
      f"permutations, seed {const['seed']}; Benjamini-Hochberg at alpha="
      f"{const['alpha']} over all {len(cfgs)} hypotheses "
      f"(`backtest/regime_stats.py:benjamini_hochberg`, the same helper "
      f"`auto_suggest.py` uses).")
    a("")

    a("### Family mapping")
    a("")
    a("| Family | Sense | Exemplars | Gate pairs (arm/disarm) |")
    a("|--------|-------|-----------|--------------------------|")
    for family in FAMILIES:
        pairs = ", ".join(f"{a_:g}/{d:g}" for a_, d in GATE_PAIRS[family])
        a(f"| `{family}` | {FAMILY_SENSE[family]} | "
          f"{', '.join('`' + e + '`' for e in FAMILY_EXEMPLARS[family])} | {pairs} |")
    a("")
    a("Exemplars run on the spot registry with registry default params, on the "
      "M1 long-leg signal path.")
    for name, ov in sorted(EXEMPLAR_CLOSE_OVERRIDES.items()):
        closes = ", ".join(f"`{c['name']}`" for c in ov.get("close_strategies") or [])
        a("")
        a(f"`{name}` emits only `+1` opens on the spot path (zero close "
          f"signals), so ungated it opens once and never exits. It is paired "
          f"with {closes} plus `stop_loss_atr_mult="
          f"{ov.get('stop_loss_atr_mult')}` so it produces real round trips. "
          f"Both arms of every comparison use that same stack, so the "
          f"gated-vs-ungated contrast is unaffected.")
    a("")

    a("### Look-ahead invariant")
    a("")
    a("Rolling H at bar `i` uses closes `[i-W+1, i]`. The DECISION series is "
      "that series shifted one bar, so a signal evaluated at bar N reads H "
      "computed from bars through N-1. The backtester fills a bar-N signal at "
      "bar N+1's open, so a trade stamped at its fill bar reads the decision "
      "value from its signal bar (the bar-close series shifted twice). This is "
      "one bar MORE conservative than the live regime gate, which reads bar "
      "N's label for the N+1 fill; the extra lag is deliberate.")
    a("")
    a("Each leg computes rolling H on a padded frame that starts before the "
      "window, then slices to the eval window with the exclusive-end rule "
      "before signals are computed, so the ungated arm stays identical to "
      "`eval_windows.run_leg`. Every leg verifies that identity against "
      "`run_leg` itself: "
      f"{payload['run_summary']['mirror_verified_legs']} of "
      f"{payload['run_summary']['legs']} legs verified.")
    a("")
    a("Estimator caveat (from the #1409 docstring): DFA carries a small upward "
      "bias at short sample sizes that a longer fetch does not remove. On "
      "memoryless data the estimate's own mean is about 0.51 at n=201 and "
      "n=101, and the n=101 spread is wide enough that individual memoryless "
      "draws regularly read 0.65-0.70. A single high reading is not evidence "
      "of persistence on its own. The shortest window used here is "
      f"{min(const['hurst_windows'])} bars.")
    a("")

    a("## Part A - outcomes bucketed by H at entry")
    a("")
    a("Ungated legs only. Trades are pooled per family across datasets and "
      "windows and deduplicated on "
      "`(strategy, symbol, timeframe, entry_date)`, iterating windows in "
      "chronological start order with first occurrence winning. Drawdown here "
      "is TRADE-GRANULAR (the compounded trade sequence), not the bar-level "
      "engine drawdown used in Part B.")
    a("")
    a(render_nan_bucket_note(payload["run_summary"].get("warmup")))
    a("")
    for family in FAMILIES:
        a(f"### {family}")
        a("")
        for hw in const["hurst_windows"]:
            table = payload["buckets"][family][str(hw)]
            a(f"**Hurst window {hw} bars**")
            a("")
            a("| Bucket | Trades | Win rate | Mean net % | Median net % | "
              "Compounded % | Trade-seq max DD % | Chop loss |")
            a("|--------|-------:|---------:|-----------:|-------------:|"
              "-------------:|-------------------:|----------:|")
            for bucket in BUCKETS:
                row = table[bucket]
                a(f"| `{bucket}` | {row['trades']} | "
                  f"{_fmt(row['win_rate_pct'], 1, '%')} | "
                  f"{_fmt(row['mean_pnl_pct_net'])} | "
                  f"{_fmt(row['median_pnl_pct_net'])} | "
                  f"{_fmt(row['compounded_return_pct'])} | "
                  f"{_fmt(row['trade_seq_max_dd_pct'])} | "
                  f"{_fmt(row['chop_loss_pct'])} |")
            a("")

    a("## Part B / C - sweep results, raw and Benjamini-Hochberg corrected")
    a("")
    a("`gate` rows are real Backtester re-runs with entry signals masked while "
      "the gate is disarmed (closes never masked); their drawdowns are "
      "bar-level. `size` rows re-compound the same ungated trade sequence with "
      "the size multiplier; their drawdowns are trade-granular. Never compare "
      "a `gate` drawdown to a `size` drawdown.")
    a("")
    a("`dd delta` and `chop delta` are MAGNITUDE deltas (arm minus ungated) "
      "averaged over that window's legs - negative means improvement.")
    a("")
    a("| Config | Mode | W | Thresholds | Pooled sup/kept | raw p | BH sig | "
      "is dd | is chop | is ret (arm/base) | oos dd | oos chop | "
      "oos ret (arm/base) | Verdict |")
    a("|--------|------|--:|-----------|-----------------|------:|:------:|"
      "------:|--------:|-------------------|-------:|---------:|"
      "--------------------|---------|")
    for cfg in cfgs:
        ok, reasons = config_verdict(cfg)
        thresholds = (f"{cfg['arm']:g}/{cfg['disarm']:g}" if cfg["mode"] == "gate"
                      else f"gain {cfg['gain']:g}")
        rows = {w: (cfg["windows"].get(w) or {}) for w in PROTOCOL_WINDOWS}
        verdict = "PASS" if ok else "; ".join(reasons[:2])
        a(f"| `{cfg['config_id']}` | {cfg['mode']} | {cfg['hurst_window']} | "
          f"{thresholds} | {cfg['n_suppressed']}/{cfg['n_kept']} | "
          f"{_fmt(cfg['p_raw'], 4)} | {'yes' if cfg['bh_reject'] else 'no'} | "
          f"{_fmt(rows['is'].get('dd_delta'))} | {_fmt(rows['is'].get('chop_delta'))} | "
          f"{_fmt(rows['is'].get('ret_gated'))} / {_fmt(rows['is'].get('ret_ungated'))} | "
          f"{_fmt(rows['oos'].get('dd_delta'))} | {_fmt(rows['oos'].get('chop_delta'))} | "
          f"{_fmt(rows['oos'].get('ret_gated'))} / {_fmt(rows['oos'].get('ret_ungated'))} | "
          f"{verdict} |")
    a("")

    a("### Held-out windows (drawdown magnitude delta, negative = improvement)")
    a("")
    a("| Config | " + " | ".join(HELD_OUT_WINDOWS) + " | non-degrading |")
    a("|--------|" + "|".join(["------:"] * len(HELD_OUT_WINDOWS)) + "|--------------:|")
    for cfg in cfgs:
        cells = []
        n_ok = 0
        for w in HELD_OUT_WINDOWS:
            row = cfg["windows"].get(w) or {}
            cells.append(_fmt(row.get("dd_delta")))
            if row.get("n_legs") and row["dd_delta"] <= 0:
                n_ok += 1
        a(f"| `{cfg['config_id']}` | " + " | ".join(cells) +
          f" | {n_ok}/{len(HELD_OUT_WINDOWS)} |")
    a("")

    a("## Acceptance rule")
    a("")
    a("A config wins for its family only when ALL of the following hold:")
    a("")
    a(f"1. Pooled suppressed trades >= {const['min_suppressed']} and pooled kept "
      f"trades >= {const['min_kept']} (a config that simply trades nothing can "
      f"never win).")
    a(f"2. Significant after one Benjamini-Hochberg correction across all "
      f"{len(cfgs)} hypotheses at alpha={const['alpha']}.")
    a("3. On BOTH protocol windows: mean drawdown magnitude falls, chop loss "
      "falls, and the return give-up stays within "
      f"max({const['return_tolerance_pp']:g} pp, "
      f"{const['return_tolerance_frac']:g} x |ungated return|).")
    a(f"4. Drawdown does not degrade on at least "
      f"{const['held_out_min_non_degrading']} of the "
      f"{len(HELD_OUT_WINDOWS)} held-out windows.")
    a("")
    a("Tie-break among passing configs: largest pooled protocol-window "
      "drawdown reduction.")
    a("")
    a("Rule 2 is what separates a real edge from arithmetic. ANY gate that "
      "removes trades lowers drawdown and chop loss on a losing book, so those "
      "columns alone prove nothing. The permutation test asks the different "
      "question a gate must answer: are the trades this gate SUPPRESSES worse "
      "than the trades it KEEPS, beyond what a random split of the same trades "
      "would produce? A config with large drawdown reductions but no "
      "significance is trading less, not trading better.")
    a("")

    a("## Run summary")
    a("")
    rs = payload["run_summary"]
    a(f"- Legs scored: {rs['legs']} ungated + {rs['gated_arms']} gated arms.")
    a(f"- Pooled deduplicated trades: " +
      ", ".join(f"{f} {rs['pooled_trades'][f]}" for f in FAMILIES) +
      f" (before dedup: " +
      ", ".join(f"{f} {rs['raw_trades'][f]}" for f in FAMILIES) + ").")
    a(f"- Hypotheses tested: {len(cfgs)}; BH-significant: "
      f"{sum(1 for c in cfgs if c['bh_reject'])}.")
    wu = rs.get("warmup")
    if wu:
        a(f"- Warm-up lead before the earliest window start: min "
          f"{wu['min_lead_bars']} bars, required {wu['required_bars']} — "
          f"{'sufficient on every dataset' if wu['sufficient'] else 'SHORT on ' + ', '.join(wu['insufficient_datasets'])}.")
    a(f"- Wall time: {rs['elapsed_sec']:.0f} s.")
    a("")

    a(render_recommendation(decision, cfgs))
    return "\n".join(out)


# ---------------------------------------------------------------------------
# Driver.
# ---------------------------------------------------------------------------

def report_from_payload(payload: dict) -> str:
    """Render the report from a committed JSON payload.

    The stored ``decision`` keeps only the winner's id (to stay compact), so
    the full decision is recomputed from the stored configs. That recompute is
    a pure function of the same numbers, which is exactly the property the
    report contract needs: the Recommendation cannot drift away from the
    evidence it is rendered beside.
    """
    rebuilt = dict(payload)
    rebuilt["decision"] = decide_recommendation(payload["configs"])
    return render_report(rebuilt)


def _parse_datasets(raw: Optional[str]) -> list:
    if not raw:
        return list(DATASETS)
    out = []
    for token in raw.split(","):
        token = token.strip()
        if not token:
            continue
        symbol, _, timeframe = token.partition(":")
        if not timeframe:
            raise SystemExit(f"--datasets entry must be SYMBOL:TIMEFRAME, got {token!r}")
        out.append((symbol, timeframe))
    return out


def _parse_windows(raw: Optional[str]) -> list:
    if not raw:
        return [w for w in WINDOW_ORDER]
    names = [t.strip() for t in raw.split(",") if t.strip()]
    for name in names:
        if name not in WINDOWS:
            raise SystemExit(f"unknown window {name!r}; known: {sorted(WINDOWS)}")
    return [w for w in WINDOW_ORDER if w in names]


def main(argv: Optional[Sequence[str]] = None) -> int:
    p = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    p.add_argument("--jobs", type=int, default=4, help="worker threads")
    p.add_argument("--out-dir", default=None,
                   help="optional dir for the rolling-Hurst npz cache")
    p.add_argument("--only", default=None,
                   help=f"comma-separated families to run ({', '.join(FAMILIES)})")
    p.add_argument("--windows", default=None, help="comma-separated window names")
    p.add_argument("--datasets", default=None,
                   help="comma-separated SYMBOL:TIMEFRAME")
    p.add_argument("--hurst-windows", default=None,
                   help="comma-separated rolling Hurst window lengths")
    p.add_argument("--n-perm", type=int, default=N_PERM)
    p.add_argument("--seed", type=int, default=SEED)
    p.add_argument("--json-out", default=_DEFAULT_JSON_OUT)
    p.add_argument("--report-out", default=_DEFAULT_REPORT_OUT)
    p.add_argument("--write-report", action="store_true",
                   help="render the Markdown report next to the JSON")
    p.add_argument("--no-mirror-check", action="store_true",
                   help="skip the per-leg eval_windows.run_leg identity check")
    p.add_argument("--render-only", action="store_true",
                   help="re-render the report from an existing --json-out; "
                        "runs no backtests")
    args = p.parse_args(argv)

    if args.render_only:
        with open(args.json_out) as fh:
            payload = json.load(fh)
        report = report_from_payload(payload)
        with open(args.report_out, "w") as fh:
            fh.write(report)
        print(f"[1410] re-rendered {args.report_out} from {args.json_out}")
        return 0

    families = FAMILIES
    if args.only:
        wanted = [t.strip() for t in args.only.split(",") if t.strip()]
        for f in wanted:
            if f not in FAMILIES:
                raise SystemExit(f"unknown family {f!r}; known: {list(FAMILIES)}")
        families = tuple(f for f in FAMILIES if f in wanted)
    datasets = _parse_datasets(args.datasets)
    window_names = _parse_windows(args.windows)
    hurst_windows = (tuple(int(t) for t in args.hurst_windows.split(","))
                     if args.hurst_windows else HURST_WINDOWS)

    started = time.time()
    from data_fetcher import load_cached_data
    from registry_loader import load_registry
    reg = load_registry("spot")

    # 1. Preload every dataset ONCE (data-source axis: PLATFORM), so the
    #    backtest fan-out never touches SQLite concurrently.
    print(f"[1410] loading {len(datasets)} datasets from {PLATFORM} cache...")
    frames = {}
    for symbol, timeframe in datasets:
        frames[(symbol, timeframe)] = load_cached_data(
            symbol, timeframe, exchange_id=PLATFORM)

    first_needed = min(pd.Timestamp(WINDOWS[w][0]) for w in window_names)

    # Measure — do not assume — the warm-up depth every scored bar rests on.
    # The report's "the NaN bucket is empty by construction" claim is only
    # true when this audit says `sufficient`; on a thinner cache the run says
    # so out loud and the rendered report names the shortfall.
    warmup = warmup_audit(
        {dataset_key(s, t): warmup_lead_bars(frames[(s, t)].index, first_needed)
         for s, t in datasets},
        hurst_windows)
    if not warmup["sufficient"]:
        print(f"[1410] WARNING: warm-up shortfall — "
              f"{len(warmup['insufficient_datasets'])} dataset(s) carry fewer "
              f"than {warmup['required_bars']} bars before "
              f"{first_needed.date()}: "
              f"{', '.join(warmup['insufficient_datasets'])}. H is UNDEFINED "
              f"on the first scored bars there, so the NaN bucket will carry "
              f"real trades. NaN stays its own bucket (never 0.5) and holds "
              f"the gate state.")
    else:
        print(f"[1410] warm-up OK: min lead {warmup['min_lead_bars']} bars "
              f"before {first_needed.date()} (need {warmup['required_bars']}).")

    # 2. Rolling Hurst per (dataset, W), computed once over the padded span.
    print(f"[1410] computing rolling Hurst for {len(datasets)}x"
          f"{len(hurst_windows)} (dataset, window) pairs...")
    hurst: dict = {}
    cache_path = None
    if args.out_dir:
        os.makedirs(args.out_dir, exist_ok=True)
        cache_path = os.path.join(args.out_dir, "hurst_1410_rolling.npz")
    cached = {}
    if cache_path and os.path.exists(cache_path):
        with np.load(cache_path, allow_pickle=False) as z:
            cached = {k: z[k] for k in z.files}

    def _hurst_job(job):
        (symbol, timeframe), hw = job
        key = f"{symbol}|{timeframe}|{hw}"
        frame = frames[(symbol, timeframe)]
        if key in cached and cache_entry_is_usable(
                cached.get(f"meta|{key}"), frame.index, first_needed):
            return job, pd.Series(cached[key], index=frame.index)
        return job, rolling_hurst(frame["close"], hw, first_needed=first_needed)

    jobs = [(ds, hw) for ds in datasets for hw in hurst_windows]
    with ThreadPoolExecutor(max_workers=max(1, args.jobs)) as pool:
        for job, series in pool.map(_hurst_job, jobs):
            hurst[job] = series
    if cache_path:
        arrays = {}
        for ds, hw in jobs:
            key = f"{ds[0]}|{ds[1]}|{hw}"
            arrays[key] = hurst[(ds, hw)].to_numpy(dtype=float)
            arrays[f"meta|{key}"] = cache_meta(
                frames[ds].index, first_needed)
        np.savez_compressed(cache_path, **arrays)

    # 3. Fan out the legs.
    units = [(family, exemplar, symbol, timeframe, wname)
             for family in families
             for exemplar in FAMILY_EXEMPLARS[family]
             for (symbol, timeframe) in datasets
             for wname in window_names]
    print(f"[1410] scoring {len(units)} legs "
          f"({len(hurst_windows) * 3} gated arms each)...")

    def _leg_job(unit):
        family, exemplar, symbol, timeframe, wname = unit
        by_window = {hw: hurst[((symbol, timeframe), hw)] for hw in hurst_windows}
        return build_leg(reg, family, exemplar, symbol, timeframe, wname,
                         frames[(symbol, timeframe)], by_window,
                         verify_mirror=not args.no_mirror_check)

    with ThreadPoolExecutor(max_workers=max(1, args.jobs)) as pool:
        legs = [lg for lg in pool.map(_leg_job, units) if lg is not None]
    legs.sort(key=lambda lg: (lg["family"], lg["strategy"], lg["dataset"], lg["window"]))

    # 4. Pool + dedup trades per family.
    pooled = {}
    raw_counts = {}
    for family in families:
        rows = [t for lg in legs if lg["family"] == family for t in lg["trades"]]
        raw_counts[family] = len(rows)
        pooled[family] = dedup_entries(rows)
    for family in FAMILIES:
        pooled.setdefault(family, [])
        raw_counts.setdefault(family, 0)

    # 5. Sweep + inference.
    configs = build_configs(legs, pooled, hurst_windows, args.n_perm, args.seed)
    configs = [c for c in configs if c["family"] in families]
    apply_bh(configs, alpha=ALPHA)
    decision = decide_recommendation(configs)

    buckets = {family: {str(hw): bucket_tables(pooled[family], hw)
                        for hw in hurst_windows}
               for family in FAMILIES}

    payload = {
        "schema_version": SCHEMA_VERSION,
        "issue": ISSUE,
        "pre_registered": {
            "families": {f: list(FAMILY_EXEMPLARS[f]) for f in FAMILIES},
            "family_sense": dict(FAMILY_SENSE),
            "exemplar_close_overrides": EXEMPLAR_CLOSE_OVERRIDES,
            "buckets": list(BUCKETS),
            "hurst_windows": list(hurst_windows),
            "gate_pairs": {f: [list(p) for p in GATE_PAIRS[f]] for f in FAMILIES},
            "gate_initial_armed": GATE_INITIAL_ARMED,
            "sizing": {"gains": list(SIZING_GAINS), "clamp_lo": SIZING_CLAMP_LO,
                       "clamp_hi": SIZING_CLAMP_HI,
                       "nan_multiplier": SIZING_NAN_MULTIPLIER},
            "min_suppressed": MIN_SUPPRESSED_TRADES,
            "min_kept": MIN_KEPT_TRADES,
            "return_tolerance_pp": RETURN_TOLERANCE_PP,
            "return_tolerance_frac": RETURN_TOLERANCE_FRAC,
            "held_out_min_non_degrading": HELD_OUT_MIN_NON_DEGRADING,
            "alpha": ALPHA,
            "n_perm": args.n_perm,
            "seed": args.seed,
            "windows": {k: list(v) for k, v in WINDOWS.items()},
            "protocol_windows": list(PROTOCOL_WINDOWS),
            "held_out_windows": list(HELD_OUT_WINDOWS),
            "datasets": [dataset_key(s, t) for s, t in datasets],
            "platform": PLATFORM,
            "fee_platform": FEE_PLATFORM,
            "capital": DEFAULT_CAPITAL,
        },
        "run_summary": {
            "legs": len(legs),
            "gated_arms": sum(len(lg["gated"]) for lg in legs),
            "mirror_verified_legs": sum(1 for lg in legs if lg["mirror_verified"]),
            "raw_trades": raw_counts,
            "pooled_trades": {f: len(pooled[f]) for f in FAMILIES},
            "warmup": warmup,
            "elapsed_sec": round(time.time() - started, 2),
        },
        "buckets": buckets,
        "configs": configs,
        "legs": [{k: v for k, v in lg.items() if k != "trades"} for lg in legs],
        "decision": {
            "verdict": decision["verdict"],
            "justification": decision["justification"],
            "families": {f: {"n_tested": d["n_tested"], "n_passing": d["n_passing"],
                             "winner": (d["winner"] or {}).get("config_id")}
                         for f, d in decision["families"].items()},
        },
    }

    with open(args.json_out, "w") as fh:
        json.dump(payload, fh, indent=2, sort_keys=False)
        fh.write("\n")
    print(f"[1410] wrote {args.json_out}")

    # render_report reads the in-memory decision (with winner objects) so the
    # Recommendation can print the winner's full config; the JSON keeps only
    # the id to stay compact.
    payload_for_report = dict(payload)
    payload_for_report["decision"] = decision
    report = render_report(payload_for_report)
    if args.write_report:
        with open(args.report_out, "w") as fh:
            fh.write(report)
        print(f"[1410] wrote {args.report_out}")
    else:
        print(render_recommendation(decision, configs))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
