#!/usr/bin/env python3
"""#1422: Hurst entry-gate POWER study — report only.

The #1410 study (``hurst_1410_gate_calibration.py``) returned ``INCONCLUSIVE``:
0 of 30 swept configurations reached Benjamini-Hochberg significance at
alpha=0.05, while 16 met every economic condition and failed only on
significance. Three properties of that design, not "Hurst does not work",
explain the null:

1. ONE re-used sample. 5 exemplars x 6 audit datasets x 5 windows = 150 legs,
   and all 30 hypotheses were scored on that same pooled trade set. Nothing in
   it was out-of-sample with respect to the sweep.
2. A 30-hypothesis grid that was a convenience, not a hypothesis, so the
   Benjamini-Hochberg denominator was 30.
3. A null that assumes trades are exchangeable. Both #1410 permutation tests
   shuffle freely across the pooled trades, but a momentum entry on BTC 1h and
   the concurrent one on ETH 1h are strongly correlated. Free shuffling counts
   correlated rows as independent information.

This one-shot answers the question decisively rather than re-running the same
design. It (A) MEASURES the detection limit of the old design and of this one,
(B) adds genuinely independent samples and reports EFFECTIVE sample size beside
nominal, (C) replaces the null with a cluster rotation that matches the
dependence structure, (D) scores a small primary hypothesis set on data #1410
never saw, so hypothesis selection cannot contaminate it, and (E) renders the
joint ADX x Hurst bucket analysis #1412's Stage 0 gate requires.

An ``INCONCLUSIVE`` verdict WITH a measured power ceiling is a legitimate and
valuable outcome: it converts #1412 Stage 0 from "maybe" into a defensible "no"
and closes the question, instead of licensing a third re-run.

REPORT-PATH CONTRACT (#1424). ``backtest/research/hurst_gate_calibration.md`` is
the live-evidence path cited by ``scheduler/hurst_gate.go``,
``docs/ARCHITECTURE.md`` and #1412's Stage 0 gate. This study OWNED it until
#1424 superseded it; the owner is now ``hurst_1424_gate_resolution.py``. This
study is a SUPERSEDED artifact and must never write the contract path again —
its own render lives beside it at ``hurst_1422_gate_power.md``, exactly as this
study did to #1410's render. #1424 inherits this design wholesale and changes
three things: one pre-registered primary hypothesis instead of four, pre-2020
calendar clusters from two additional venues, and a bounded-variance primary
target. Read #1424's report for the live verdict.

REPORT-ONLY. Zero scheduler, config, gating, sizing, or live-path changes. The
gate and size arms are OFFLINE SIMULATIONS. #1411's ``hurst_gate`` ships
default-off with no recommended thresholds and is untouched by this study;
whether that stays true is what the Recommendation section decides.

Method
------
Part A — outcome buckets by H at entry, per family per Hurst window, over the
expanded pool (the #1410 shape, on more data).

Part B — hard-gate sweeps: real ``Backtester`` re-runs with entry signals masked
while a hysteresis gate is disarmed. Closes are NEVER masked.

Part C — size sweeps: the ungated trade sequence re-compounded with a size
multiplier.

Part D — joint ADX x Hurst buckets and the #1412 Stage 0 separation verdict.

Inference — every hypothesis carries TWO p-values: the #1410 free shuffle (for
continuity) and the cluster rotation (which the verdict reads). Two disjoint
Benjamini-Hochberg families: the PRIMARY set (scored only on cells #1410 never
saw) and the EXPLORATORY grid. They never share a denominator.

Usage
-----
  uv run --no-sync python backtest/research/hurst_1422_gate_power.py \
      --jobs 8 --write-report --out-dir /tmp/hurst1422
  uv run --no-sync python backtest/research/hurst_1422_gate_power.py --render-only
  uv run --no-sync python backtest/research/hurst_1422_gate_power.py --fetch-only
"""

import argparse
import json
import math
import os
import sys
import time
import traceback
from concurrent.futures import ThreadPoolExecutor
from typing import Optional, Sequence

_THIS_DIR = os.path.dirname(os.path.abspath(__file__))
_BACKTEST = os.path.abspath(os.path.join(_THIS_DIR, ".."))
_ROOT = os.path.abspath(os.path.join(_BACKTEST, ".."))
for _p in (_THIS_DIR, _BACKTEST, _ROOT, os.path.join(_ROOT, "shared_tools")):
    if _p not in sys.path:
        sys.path.insert(0, _p)

import numpy as np  # noqa: E402
import pandas as pd  # noqa: E402

from eval_windows import (  # noqa: E402  (sys.path bootstrap must run first)
    DATASETS as AUDIT_DATASETS,
    DEFAULT_CAPITAL,
    FEE_PLATFORM,
    HELD_OUT_WINDOWS as AUDIT_HELD_OUT_WINDOWS,
    PLATFORM,
    PROTOCOL_WINDOWS as AUDIT_PROTOCOL_WINDOWS,
    WINDOWS as AUDIT_WINDOWS,
    dataset_key,
    leg_from_results,
    run_leg,
)
from regime_stats import benjamini_hochberg  # noqa: E402

# The #1410 study is the SSoT for every helper this study did not have to
# change. Imported by unambiguous module name off research/ on sys.path — the
# pattern test_hurst_1410_gate_calibration.py uses, and the one that stays safe
# under the #1304 `pytest -n auto` parallel run.
import hurst_1410_gate_calibration as study1410  # noqa: E402

bucket_label = study1410.bucket_label
cache_entry_is_usable = study1410.cache_entry_is_usable
cache_meta = study1410.cache_meta
chop_loss = study1410.chop_loss
compound_equity = study1410.compound_equity
decision_series = study1410.decision_series
entry_stamp_series = study1410.entry_stamp_series
hysteresis_mask = study1410.hysteresis_mask
mask_entry_signals = study1410.mask_entry_signals
permutation_pvalue_group_diff = study1410.permutation_pvalue_group_diff
permutation_pvalue_weighted = study1410.permutation_pvalue_weighted
required_lead_bars = study1410.required_lead_bars
rolling_hurst = study1410.rolling_hurst
size_multiplier = study1410.size_multiplier
slice_window = study1410.slice_window
validate_gate_pair = study1410.validate_gate_pair
warmup_audit = study1410.warmup_audit
warmup_lead_bars = study1410.warmup_lead_bars
win_rate = study1410.win_rate

BUCKETS = study1410.BUCKETS
BUCKET_NAN = study1410.BUCKET_NAN
EXEMPLAR_CLOSE_OVERRIDES = study1410.EXEMPLAR_CLOSE_OVERRIDES
FAMILIES = study1410.FAMILIES
FAMILY_EXEMPLARS = study1410.FAMILY_EXEMPLARS
FAMILY_SENSE = study1410.FAMILY_SENSE
GATE_INITIAL_ARMED = study1410.GATE_INITIAL_ARMED
GATE_PAIRS = study1410.GATE_PAIRS
HURST_WINDOWS = study1410.HURST_WINDOWS
SENSE_HIGH = study1410.SENSE_HIGH
SENSE_LOW = study1410.SENSE_LOW
SIZING_CLAMP_HI = study1410.SIZING_CLAMP_HI
SIZING_CLAMP_LO = study1410.SIZING_CLAMP_LO
SIZING_GAINS = study1410.SIZING_GAINS
SIZING_NAN_MULTIPLIER = study1410.SIZING_NAN_MULTIPLIER
_MIRRORED_LEG_KEYS = study1410._MIRRORED_LEG_KEYS

# ---------------------------------------------------------------------------
# Pre-registered design constants. Fixed before the sweep ran; serialized
# verbatim into the JSON and the report so a reader can tell the Recommendation
# was not tuned after seeing the numbers.
# ---------------------------------------------------------------------------
SCHEMA_VERSION = 3   # v3: pool-matched observed separations (mde.observed_separation_pp_by_pool)
ISSUE = 1422
SEED = ISSUE  # 1422 — fixed so a re-run reproduces every p-value

# History floor. Everything before this is not fetched, so a window may never
# start earlier than HISTORY_SINCE + the deepest Hurst lead.
HISTORY_SINCE = "2020-01-01"

# The three PRE-2023 windows. #1410 scored nothing before 2023-01-01, so these
# are the backbone of the primary (uncontaminated) cohort.
NEW_WINDOWS = {
    "2020H2": ("2020-07-01", "2021-01-01"),
    "2021":   ("2021-01-01", "2022-01-01"),
    "2022":   ("2022-01-01", "2023-01-01"),
}
# The five #1410 windows are reused VERBATIM from eval_windows — never
# redefined here, so a drift in the M1 bar fails loud below instead of
# silently rescoring the exploratory arm on different spans.
WINDOWS = dict(AUDIT_WINDOWS)
for _k, _v in NEW_WINDOWS.items():
    if _k in WINDOWS:
        raise AssertionError(f"new window {_k!r} collides with an eval_windows window")
    WINDOWS[_k] = _v

WINDOW_ORDER = tuple(sorted(WINDOWS, key=lambda w: (WINDOWS[w][0], w)))

# Economics windows for the PRIMARY cohort: two protocol windows and the rest
# held out. Pre-2023 tape only, so a primary verdict never rests on a window
# #1410 already mined.
PRIMARY_PROTOCOL_WINDOWS = ("2021", "2022")
PRIMARY_HELD_OUT_WINDOWS = ("2020H2", "is", "oos", "2023", "2024", "2025H1")
# The exploratory arm keeps #1410's split byte-identical.
EXPLORATORY_PROTOCOL_WINDOWS = tuple(AUDIT_PROTOCOL_WINDOWS)
EXPLORATORY_HELD_OUT_WINDOWS = tuple(AUDIT_HELD_OUT_WINDOWS)

# The base symbols #1410 scored. A trade on any OTHER symbol is primary by
# construction, whatever window it lands in.
AUDIT_SYMBOLS = tuple(sorted({s for s, _ in AUDIT_DATASETS}))

# Datasets. The audit six verbatim, plus 2h on the audit symbols, plus the
# most-liquid non-BTC-clique names binanceus carries. 15m/30m are deliberately
# EXCLUDED: a 512-bar Hurst window at 15m spans about five days, which measures
# microstructure rather than the multi-week persistence this study is about,
# and same-tape resampling adds almost nothing to effective N anyway.
NEW_TIMEFRAME_DATASETS = [(sym, "2h") for sym in AUDIT_SYMBOLS]
NEW_SYMBOL_DATASETS = [
    (sym, tf)
    for sym in ("BNB/USDT", "XRP/USDT", "DOGE/USDT", "LTC/USDT",
                "ADA/USDT", "LINK/USDT")
    for tf in ("1h", "4h")
]
DATASETS = list(AUDIT_DATASETS) + NEW_TIMEFRAME_DATASETS + NEW_SYMBOL_DATASETS

# The exact (dataset, window) grid #1410 scored. Frozen from eval_windows so a
# future edit there fails this assertion instead of silently shrinking the
# exclusion set and letting contaminated cells into the primary cohort.
D_1410 = frozenset(
    (dataset_key(s, t), w) for (s, t) in AUDIT_DATASETS for w in AUDIT_WINDOWS
)
if len(D_1410) != 30:
    raise AssertionError(
        f"#1410 scored a 6x5 grid; eval_windows now yields {len(D_1410)} cells. "
        f"Re-derive the primary cohort before trusting this study.")

COHORT_PRIMARY = "primary"
COHORT_EXPLORATORY = "exploratory"

# Primary hypothesis set: per family x mode, the config with the smallest raw
# p in the COMMITTED #1410 JSON. Selection on #1410's outcomes is legitimate
# here ONLY because the primary cohort is disjoint from the data those outcomes
# came from. The expected ids are pinned so a regenerated #1410 JSON can never
# silently swap the primary set underneath the pre-registration.
PRIMARY_CONFIG_IDS = (
    "momentum/gate/W512/arm0.52/dis0.48",
    "momentum/size/W512/gain2.5",
    "mean_reversion/gate/W128/arm0.4/dis0.48",
    "mean_reversion/size/W128/gain5",
)

# Cluster rotation. A permutation is ONE calendar offset shared by every
# dataset, so concurrent correlated trades move together under the null and the
# hysteresis autocorrelation of the label run survives.
MIN_OFFSET_DAYS = 30          # no near-identity alignment may count as a draw
MIN_CLUSTER_SPAN_DAYS = 3 * MIN_OFFSET_DAYS
N_PERM = 10000
N_PERM_MDE = 2000             # cheaper null for the MDE grid; stated in report

# Minimum-detectable-effect search grid, in percentage points per trade.
MDE_GRID_STEP = 0.1
MDE_GRID_MAX = 5.0
MDE_REFINE_STEP = 0.02

# Coverage density floor. A cell must carry at least this fraction of the bars
# a complete cache would hold inside its window, or it is dropped. Without it a
# delisting gap (Binance.US delisted XRP for most of 2021-2023) would score a
# whole year on a few hundred bars and weight it like a complete one.
MIN_WINDOW_BAR_FRACTION = 0.8

# Decision floors, applied to EFFECTIVE N (not row count) — this is what makes
# them carry #1410's meaning, "this many INDEPENDENT trades", on a pool whose
# rows are correlated.
MIN_SUPPRESSED_EFFECTIVE = 20.0
MIN_KEPT_EFFECTIVE = 30.0

# Economic acceptance thresholds — #1410's values, unchanged.
RETURN_TOLERANCE_PP = 1.0
RETURN_TOLERANCE_FRAC = 0.1
# #1410 required 2 of 3 held-out windows to hold. Generalized to a fraction so
# one rule covers both cohorts: 2/3 of the held-out windows that carry legs,
# and at least 3 must carry legs for the check to be testable at all.
HELD_OUT_MIN_FRACTION = 2.0 / 3.0
HELD_OUT_MIN_WINDOWS = 3

ALPHA = 0.05

# ADX (Part D). Wilder period 14 — compute_regime's default — and the composite
# classifier's default split threshold.
ADX_PERIOD = 14
ADX_SPLIT = 25.0
JOINT_H_BUCKETS = ("<0.45", "0.45-0.55", ">0.55", BUCKET_NAN)
JOINT_ADX_BUCKETS = ("<25", ">=25", BUCKET_NAN)
NO_JOINT_SEPARATION = "NO JOINT SEPARATION"
# Two families tested for joint separation, so Bonferroni the pair.
JOINT_ALPHA = ALPHA / 2.0

_DEFAULT_JSON_OUT = os.path.join(_THIS_DIR, "hurst_1422_gate_power.json")
# NOT "hurst_gate_calibration.md" — that is the live-evidence contract path, and
# #1424 owns it now (see the REPORT-PATH CONTRACT note in the module docstring).
# A --render-only here must never be able to revert the live evidence to this
# superseded study.
_DEFAULT_REPORT_OUT = os.path.join(_THIS_DIR, "hurst_1422_gate_power.md")


# ---------------------------------------------------------------------------
# Pure helpers (unit-tested without data access).
# ---------------------------------------------------------------------------

def cell_cohort(symbol: str, timeframe: str, window_name: str) -> str:
    """Which inference cohort one (dataset, window) cell belongs to.

    PRIMARY iff the window is one #1410 never scored, OR the symbol is one
    #1410 never scored. Everything else is EXPLORATORY.

    The symbol/window test is deliberately COARSER than the (dataset, window)
    grid. BTC 2h over a #1410 window is not in that grid, but it is the same
    TAPE #1410 mined at 1h and 4h over the same months — scoring a hypothesis
    there that was chosen by reading #1410's p-values would be selection on the
    same data wearing a different timeframe. Only a genuinely new period or a
    genuinely new asset is clean.

    This is the whole defence against selection contamination: the primary
    hypotheses were chosen by reading #1410's p-values, so they may only ever
    be scored on tape those p-values did not come from.
    """
    if window_name not in WINDOWS:
        raise ValueError(f"unknown window {window_name!r}")
    if window_name not in AUDIT_WINDOWS:
        return COHORT_PRIMARY
    return COHORT_EXPLORATORY if symbol in AUDIT_SYMBOLS else COHORT_PRIMARY


def joint_h_bucket(h) -> str:
    """Coarse H bucket for the joint ADX x Hurst table (#1412 Stage 0).

    Deliberately coarser than #1410's four-way ``bucket_label`` — Stage 0 asks
    a three-way question. NaN is its own bucket and is never coerced to 0.5.
    """
    if h is None:
        return BUCKET_NAN
    value = float(h)
    if not math.isfinite(value):
        return BUCKET_NAN
    if value < 0.45:
        return "<0.45"
    if value <= 0.55:
        return "0.45-0.55"
    return ">0.55"


def joint_adx_bucket(adx) -> str:
    """High/low ADX split at the composite classifier's default threshold.

    NaN is its own bucket. That matters: ``compute_regime`` fills ADX warm-up
    bars with 0.0, not NaN, so an unmasked stamp would silently file every
    warm-up bar under "low ADX". ``adx_entry_stamp`` masks them first.
    """
    if adx is None:
        return BUCKET_NAN
    value = float(adx)
    if not math.isfinite(value):
        return BUCKET_NAN
    return ">=25" if value >= ADX_SPLIT else "<25"


def anti_signal_side(h: float, sense: str) -> bool:
    """True when H sits on the side that family's gate SUPPRESSES.

    Keyed on the sense, never on the family, so a new family that reuses an
    existing sense needs no edit here. An unknown sense raises rather than
    defaulting — a silent default would invert an injected contrast.
    """
    if sense == SENSE_HIGH:      # arms on high H -> suppresses low H
        return float(h) < 0.5
    if sense == SENSE_LOW:       # arms on low H  -> suppresses high H
        return float(h) >= 0.5
    raise ValueError(f"unknown gate sense {sense!r}")


def count_overlapping_pairs(a_start, a_end, b_start, b_end) -> int:
    """Ordered pairs (i, j) whose intervals overlap: a_start_i < b_end_j and
    b_start_j < a_end_i.

    Exact and O(n log n): because ``b_start_j <= b_end_j``, the set
    ``{j : b_end_j <= a_start_i}`` is contained in ``{j : b_start_j < a_end_i}``
    whenever ``a_start_i < a_end_i``, so the count is one searchsorted minus
    another. Used for the effective-N denominator, where the pair count between
    two datasets is what carries their correlation.
    """
    a_s = np.sort(np.asarray(a_start, dtype=np.int64))
    a_e = np.asarray(a_end, dtype=np.int64)[np.argsort(np.asarray(a_start, dtype=np.int64))]
    b_s = np.sort(np.asarray(b_start, dtype=np.int64))
    b_e = np.sort(np.asarray(b_end, dtype=np.int64))
    if a_s.size == 0 or b_s.size == 0:
        return 0
    lt_end = np.searchsorted(b_s, a_e, side="left")
    le_start = np.searchsorted(b_e, a_s, side="right")
    return int(np.sum(np.maximum(lt_end - le_start, 0)))


def pairwise_trade_rho(rho_by_symbol: dict, sym_a: str, sym_b: str) -> float:
    """Correlation credited to a pair of trades on two symbols.

    Same symbol (any timeframe) is the SAME TAPE, so 1.0. Otherwise the
    symbol-level daily-log-return correlation, CLIPPED to [0, 1]: a negative
    correlation would raise effective N, and this study refuses that credit
    rather than letting anti-correlation manufacture power.
    """
    if sym_a == sym_b:
        return 1.0
    raw = rho_by_symbol.get((sym_a, sym_b))
    if raw is None:
        raw = rho_by_symbol.get((sym_b, sym_a))
    if raw is None or not math.isfinite(float(raw)):
        # Unknown correlation is treated as fully correlated — the
        # conservative direction, never a free grant of independence.
        return 1.0
    return float(min(1.0, max(0.0, float(raw))))


def effective_n(trades: Sequence[dict], rho_by_symbol: dict) -> float:
    """Effective sample size of a pooled trade set.

    ``N_eff = N^2 / sum_ij rho_ij`` with ``rho_ii = 1`` and, for ``i != j``,
    the symbol-level correlation when the two trades' holding periods OVERLAP
    in calendar time and 0 when they do not. Two trades that never coexist
    carry independent information however correlated their assets are.

    No matrix is inverted anywhere, so a near-singular correlation structure
    cannot blow the estimate up; with a unit diagonal and off-diagonals in
    [0, 1] the result is bounded ``1 <= N_eff <= N`` by construction.
    """
    rows = [t for t in trades if t.get("entry_ns") is not None
            and t.get("exit_ns") is not None]
    n = len(rows)
    if n == 0:
        return 0.0
    by_ds: dict = {}
    for t in rows:
        by_ds.setdefault((t["symbol"], t["timeframe"]), []).append(t)
    total = float(n)  # the diagonal
    keys = sorted(by_ds)
    for a in keys:
        rows_a = by_ds[a]
        a_start = [r["entry_ns"] for r in rows_a]
        a_end = [r["exit_ns"] for r in rows_a]
        for b in keys:
            rho = pairwise_trade_rho(rho_by_symbol, a[0], b[0])
            if rho <= 0.0:
                continue
            rows_b = by_ds[b]
            pairs = count_overlapping_pairs(
                a_start, a_end, [r["entry_ns"] for r in rows_b],
                [r["exit_ns"] for r in rows_b])
            if a == b:
                # count_overlapping_pairs counts every i against itself; the
                # diagonal is already in `total`.
                pairs -= len(rows_a)
            total += rho * float(pairs)
    if total <= 0.0:
        return float(n)
    return round(min(float(n), max(1.0, float(n) * float(n) / total)), 4)


def cluster_rotation_offsets(trades: Sequence[dict]) -> dict:
    """Per-dataset chronological ordering and span, the rotation's substrate.

    Returns ``{dataset_key: {"order": [idx...], "ns": [entry_ns...],
    "span_days": int}}`` with each dataset's trade indices sorted by entry time.
    """
    by_ds: dict = {}
    for i, t in enumerate(trades):
        by_ds.setdefault(dataset_key(t["symbol"], t["timeframe"]), []).append(i)
    out = {}
    for key, idxs in by_ds.items():
        idxs = sorted(idxs, key=lambda i: (trades[i]["entry_ns"], i))
        ns = [int(trades[i]["entry_ns"]) for i in idxs]
        span_days = int((ns[-1] - ns[0]) // 86_400_000_000_000) if len(ns) > 1 else 0
        out[key] = {"order": idxs, "ns": ns, "span_days": span_days}
    return out


def effective_offset_days(offset_days: int, span_days: int) -> int:
    """The shared calendar offset folded into ONE dataset's rotatable band.

    Each dataset rotates on its own circular calendar, so an offset longer than
    a dataset's span HAS to wrap. Leaving it unwrapped is not a harmless edge
    case: ``searchsorted`` would run off the end, the modulo would turn the
    shift into exactly 0, and that dataset would hand the null draw its OBSERVED
    label ordering. Unrotated rows carry the observed contrast into the null
    statistic, so the bias is one-directional — every cluster p comes out too
    high. Because the draw range is the POOL's longest span, a ragged pool
    (a ~900-day dataset beside a ~2,200-day one) hits that path on most draws.

    The fold lands in ``[MIN_OFFSET_DAYS, span - MIN_OFFSET_DAYS]``, the same
    band ``_admissible_offsets`` draws from.

    In this study it is a DORMANT guard: the draw range is capped at the
    pool's SHORTEST span (see ``_admissible_offsets``), so every drawn offset
    already fits every retained dataset and this function returns its argument
    unchanged. It exists so a direct caller, or a future range that is not
    capped, still cannot produce the identity rotation.
    """
    span = int(span_days)
    lo = MIN_OFFSET_DAYS
    hi = span - MIN_OFFSET_DAYS
    if hi <= lo:
        # Too short to host the guard band at all; such a dataset is dropped by
        # `usable_cluster_rows` before it reaches a null, and this stays only so
        # a direct caller still gets a real rotation instead of the identity.
        return max(1, span // 2)
    width = hi - lo + 1
    return lo + ((int(offset_days) - lo) % width)


def rotation_shift_counts(clusters: dict, offset_days: int) -> dict:
    """Trades-per-dataset falling inside the first ``offset_days`` of its span.

    Rotating a dataset's chronological label vector by THIS many positions is
    the discrete stand-in for shifting it ``offset_days`` in calendar time. The
    offset is shared across datasets, so two concurrent datasets shift by the
    same calendar amount and their labels stay aligned under the null; a dataset
    whose span cannot hold the whole offset folds it (``effective_offset_days``)
    rather than declining to rotate.

    The returned shift is always in ``[1, len-1]`` for a dataset with two or
    more trades, so no dataset can contribute its observed label ordering to a
    null draw.
    """
    out = {}
    for key, info in clusters.items():
        ns = np.asarray(info["ns"], dtype=np.int64)
        n = len(ns)
        if n < 2:
            out[key] = 0
            continue
        eff = effective_offset_days(int(offset_days), int(info["span_days"]))
        cut = int(np.searchsorted(ns, ns[0] + eff * 86_400_000_000_000,
                                  side="left"))
        cut %= n
        # `eff >= 1` puts at least ns[0] inside the cut, so this only fires on a
        # dataset whose whole span is under a day — belt and braces, never the
        # identity.
        out[key] = cut or 1
    return out


def _rotate_values(values: np.ndarray, clusters: dict, shifts: dict) -> np.ndarray:
    """Values rotated cyclically WITHIN each dataset by that dataset's shift."""
    out = np.array(values, copy=True)
    for key, info in clusters.items():
        order = info["order"]
        k = shifts.get(key, 0) % max(1, len(order))
        if k == 0 or len(order) < 2:
            continue
        src = np.asarray(values, dtype=out.dtype)[order]
        out[order] = np.roll(src, k)
    return out


def usable_cluster_rows(trades: Sequence[dict]) -> tuple:
    """(row_indices, excluded_datasets) — rows the cluster null can rotate.

    A dataset whose scored span is shorter than ``MIN_CLUSTER_SPAN_DAYS`` cannot
    host a meaningful rotation, so its rows leave the CONTRAST as well as the
    null. Excluding it from the offset range alone would be a label, not an
    exclusion: its rows would still be scored, still sit in the observed
    statistic, and still enter every draw carrying their observed alignment.
    """
    clusters = cluster_rotation_offsets(trades)
    excluded = sorted(k for k, v in clusters.items()
                      if v["span_days"] < MIN_CLUSTER_SPAN_DAYS)
    dropped = set(excluded)
    idx = [i for i, t in enumerate(trades)
           if dataset_key(t["symbol"], t["timeframe"]) not in dropped]
    return idx, excluded


def _admissible_offsets(clusters: dict) -> tuple:
    """``(lo, hi)`` calendar offsets a draw may use, or ``()`` when none can.

    The range is bounded by the SHORTEST span in the pool, not the longest.
    That is what keeps the null's whole purpose intact: every retained dataset
    can host every drawn offset without wrapping, so two concurrent datasets
    shift by the SAME calendar amount in every draw and their correlated labels
    move together. Drawing against the longest span instead would send offsets
    past a shorter dataset's end — and whichever way that is handled it costs
    something. Leaving it unrotated hands the null that dataset's observed
    alignment (p too high). Wrapping it into its own band rotates it, but by a
    different calendar amount than its neighbours, which understates the
    cross-dataset correlation the null is supposed to preserve and makes p too
    LOW. A false positive is the worse failure, so the shared range is capped
    and the wrap (``effective_offset_days``) stays only as a dormant guard.

    ``MIN_OFFSET_DAYS`` still trims both ends. The offsets it leaves are long
    against a hysteresis run's autocorrelation, which is what a rotation has to
    break — so a long dataset rotating by a small FRACTION of its span still
    has its label alignment destroyed.
    """
    if not clusters:
        return ()
    span = min(v["span_days"] for v in clusters.values())
    lo, hi = MIN_OFFSET_DAYS, span - MIN_OFFSET_DAYS
    if hi < lo:
        return ()
    return (lo, hi)


def cluster_permutation_pvalue_group_diff(trades: Sequence[dict],
                                          values: Sequence[float],
                                          suppressed: Sequence[bool],
                                          n_perm: int = N_PERM,
                                          seed: int = SEED) -> dict:
    """One-sided cluster p for "suppressed trades are WORSE than kept trades".

    The null is a shared circular CALENDAR rotation: one offset per draw,
    applied to every dataset's chronologically ordered label vector by the
    number of that dataset's trades inside the offset. Concurrent trades on
    correlated assets therefore move TOGETHER, and a label run's autocorrelation
    survives the rotation (only the wrap seam breaks it) — neither is true of
    the #1410 free shuffle.

    Returns ``{"p": float|None, "n_draws": int, "excluded_datasets": [...],
    "n_scored": int, "n_excluded_trades": int, "offset_range": [lo, hi]|None}``.
    ``p`` is None when the pool cannot host a rotation at all; the caller must
    treat that as untestable, never as "not significant".
    """
    vals = np.asarray(values, dtype=float)
    mask = np.asarray(suppressed, dtype=bool)
    if vals.shape != mask.shape or len(trades) != vals.size:
        raise ValueError("trades/values/suppressed length mismatch")
    n_in = vals.size
    if n_in == 0:
        return {"p": None, "n_draws": 0, "excluded_datasets": [], "n_scored": 0,
                "n_excluded_trades": 0, "offset_range": None,
                "reason": "no testable contrast"}
    idx, excluded = usable_cluster_rows(trades)
    n_excluded = n_in - len(idx)
    if not idx:
        return {"p": None, "n_draws": 0, "excluded_datasets": excluded,
                "n_scored": 0, "n_excluded_trades": n_excluded,
                "offset_range": None,
                "reason": "no dataset spans enough calendar time to rotate"}
    trades = [trades[i] for i in idx]
    vals = vals[idx]
    mask = mask[idx]
    n = vals.size
    k = int(mask.sum())
    if k == 0 or k == n:
        return {"p": None, "n_draws": 0, "excluded_datasets": excluded,
                "n_scored": n, "n_excluded_trades": n_excluded,
                "offset_range": None, "reason": "no testable contrast"}
    clusters = cluster_rotation_offsets(trades)
    bounds = _admissible_offsets(clusters)
    if not bounds:
        return {"p": None, "n_draws": 0, "excluded_datasets": excluded,
                "n_scored": n, "n_excluded_trades": n_excluded,
                "offset_range": None,
                "reason": "no dataset spans enough calendar time to rotate"}
    lo, hi = bounds
    observed = float(vals[~mask].mean() - vals[mask].mean())
    rng = np.random.default_rng(seed)
    offsets = rng.integers(lo, hi + 1, size=int(n_perm))
    ge = 0
    draws = 0
    for off in offsets:
        shifts = rotation_shift_counts(clusters, int(off))
        rot = _rotate_values(mask, clusters, shifts).astype(bool)
        kk = int(rot.sum())
        if kk == 0 or kk == n:
            continue
        draws += 1
        stat = float(vals[~rot].mean() - vals[rot].mean())
        if stat >= observed:
            ge += 1
    if draws == 0:
        return {"p": None, "n_draws": 0, "excluded_datasets": excluded,
                "n_scored": n, "n_excluded_trades": n_excluded,
                "offset_range": [int(lo), int(hi)],
                "reason": "every rotation collapsed the split"}
    return {"p": round((1.0 + ge) / (draws + 1.0), 6), "n_draws": draws,
            "excluded_datasets": excluded, "n_scored": n,
            "n_excluded_trades": n_excluded,
            "n_distinct_offsets": int(hi) - int(lo) + 1,
            "offset_range": [int(lo), int(hi)]}


def cluster_permutation_pvalue_weighted(trades: Sequence[dict],
                                        returns: Sequence[float],
                                        multipliers: Sequence[float],
                                        n_perm: int = N_PERM,
                                        seed: int = SEED) -> dict:
    """One-sided cluster p for "this multiplier PAIRING beats a rotated one".

    Same shared calendar rotation as the gate variant, applied to the
    multiplier vector. Both marginal distributions stay fixed, so only the
    pairing — the thing a size rule claims to get right — is tested.
    """
    rets = np.asarray(returns, dtype=float)
    mults = np.asarray(multipliers, dtype=float)
    if rets.shape != mults.shape or len(trades) != rets.size:
        raise ValueError("trades/returns/multipliers length mismatch")
    n_in = rets.size
    if n_in == 0:
        return {"p": None, "n_draws": 0, "excluded_datasets": [], "n_scored": 0,
                "n_excluded_trades": 0, "offset_range": None,
                "reason": "multipliers carry no variation"}
    idx, excluded = usable_cluster_rows(trades)
    n_excluded = n_in - len(idx)
    if not idx:
        return {"p": None, "n_draws": 0, "excluded_datasets": excluded,
                "n_scored": 0, "n_excluded_trades": n_excluded,
                "offset_range": None,
                "reason": "no dataset spans enough calendar time to rotate"}
    trades = [trades[i] for i in idx]
    rets = rets[idx]
    mults = mults[idx]
    # Checked AFTER the exclusion: variation the dropped rows carried is not
    # variation this test can use.
    if float(np.ptp(mults)) == 0.0:
        return {"p": None, "n_draws": 0, "excluded_datasets": excluded,
                "n_scored": rets.size, "n_excluded_trades": n_excluded,
                "offset_range": None, "reason": "multipliers carry no variation"}
    clusters = cluster_rotation_offsets(trades)
    bounds = _admissible_offsets(clusters)
    if not bounds:
        return {"p": None, "n_draws": 0, "excluded_datasets": excluded,
                "n_scored": rets.size, "n_excluded_trades": n_excluded,
                "offset_range": None,
                "reason": "no dataset spans enough calendar time to rotate"}
    lo, hi = bounds
    observed = float(np.mean(rets * mults))
    rng = np.random.default_rng(seed)
    offsets = rng.integers(lo, hi + 1, size=int(n_perm))
    ge = 0
    draws = 0
    for off in offsets:
        shifts = rotation_shift_counts(clusters, int(off))
        rot = _rotate_values(mults, clusters, shifts)
        draws += 1
        if float(np.mean(rets * rot)) >= observed:
            ge += 1
    if draws == 0:
        return {"p": None, "n_draws": 0, "excluded_datasets": excluded,
                "n_scored": rets.size, "n_excluded_trades": n_excluded,
                "offset_range": [int(lo), int(hi)], "reason": "no valid draw"}
    return {"p": round((1.0 + ge) / (draws + 1.0), 6), "n_draws": draws,
            "excluded_datasets": excluded, "n_scored": rets.size,
            "n_excluded_trades": n_excluded,
            "n_distinct_offsets": int(hi) - int(lo) + 1,
            "offset_range": [int(lo), int(hi)]}


def _rank1_threshold(family_size: int, alpha: float = ALPHA) -> float:
    """The HARDEST bar Benjamini-Hochberg can impose on one hypothesis.

    BH rejects the smallest p only when ``p <= alpha/m``. Using that as the MDE
    bar makes the reported detection limit conservative and independent of how
    the OTHER hypotheses happened to land.
    """
    return float(alpha) / float(max(1, int(family_size)))


def min_detectable_effect(trades: Sequence[dict],
                          values: Sequence[float],
                          suppressed: Sequence[bool],
                          family_size: int,
                          *,
                          cluster: bool = True,
                          n_perm: int = N_PERM_MDE,
                          seed: int = SEED,
                          alpha: float = ALPHA) -> Optional[float]:
    """Smallest per-trade edge this design could have detected, in pp.

    Injection is a deterministic shift: every SUPPRESSED trade's net return is
    lowered by ``d`` percentage points, which raises the observed statistic by
    exactly ``d`` while the null re-pools the shifted values. The search is a
    fixed grid then one refinement pass — no bisection, because a permutation
    p-value is only approximately monotone in ``d`` and a grid is trivially
    reproducible.

    Returns None when even the largest grid point cannot clear the rank-1
    Benjamini-Hochberg threshold — a design that cannot detect a 5 pp/trade
    edge is reported as such, never as a number it did not reach.

    Raises when the permutation count cannot RESOLVE the threshold. With the
    add-one convention the smallest p a run can produce is ``1/(n_perm+1)``, so
    too few draws would make every effect look undetectable and the report
    would publish "no power" when the truth is "no permutations". That failure
    must be loud, never a silent None.
    """
    vals = np.asarray(values, dtype=float)
    mask = np.asarray(suppressed, dtype=bool)
    bar = _rank1_threshold(family_size, alpha)
    floor = 1.0 / (float(n_perm) + 1.0)
    if floor > bar:
        raise ValueError(
            f"n_perm={n_perm} cannot resolve the rank-1 Benjamini-Hochberg bar "
            f"{bar:g} for a family of {family_size} (smallest reachable p is "
            f"{floor:g}); raise --n-perm-mde to at least "
            f"{int(math.ceil(1.0 / bar))}")
    if vals.size == 0 or int(mask.sum()) in (0, vals.size):
        return None

    def _p_at(d: float) -> Optional[float]:
        shifted = vals - np.where(mask, float(d), 0.0)
        if cluster:
            res = cluster_permutation_pvalue_group_diff(
                trades, shifted, mask, n_perm=n_perm, seed=seed)
            return res.get("p")
        return permutation_pvalue_group_diff(shifted, mask, n_perm=n_perm,
                                             seed=seed)

    coarse = None
    steps = int(round(MDE_GRID_MAX / MDE_GRID_STEP))
    for i in range(steps + 1):
        d = round(i * MDE_GRID_STEP, 6)
        p = _p_at(d)
        if p is not None and p <= bar:
            coarse = d
            break
    if coarse is None:
        return None
    if coarse == 0.0:
        return 0.0
    lo = max(0.0, coarse - MDE_GRID_STEP)
    fine = coarse
    n_fine = int(round(MDE_GRID_STEP / MDE_REFINE_STEP))
    for i in range(n_fine + 1):
        d = round(lo + i * MDE_REFINE_STEP, 6)
        p = _p_at(d)
        if p is not None and p <= bar:
            fine = d
            break
    return round(float(fine), 4)


def held_out_verdict(windows: dict, held_out_names: Sequence[str]) -> tuple:
    """(passed, n_non_degrading, n_with_legs) for the held-out drawdown rule.

    #1410 required 2 of 3. Generalized to the same 2/3 FRACTION so one rule
    covers both cohorts, with at least ``HELD_OUT_MIN_WINDOWS`` windows
    carrying legs — otherwise the check is untestable and fails closed.
    """
    with_legs = [n for n in held_out_names
                 if (windows.get(n) or {}).get("n_legs")]
    non_deg = sum(1 for n in with_legs if windows[n]["dd_delta"] <= 0)
    if len(with_legs) < HELD_OUT_MIN_WINDOWS:
        return False, non_deg, len(with_legs)
    need = math.ceil(HELD_OUT_MIN_FRACTION * len(with_legs))
    return non_deg >= need, non_deg, len(with_legs)


def config_verdict(cfg: dict) -> tuple:
    """Pure accept/reject for one swept config. Returns ``(passed, reasons)``.

    #1410's rule shape, with two deliberate changes:
      * the volume floors are on EFFECTIVE N, so a pool of correlated rows
        cannot buy its way past them;
      * significance reads the CLUSTER p, never the free-shuffle p. An
        untestable cluster p (None) fails closed.
    """
    reasons = []
    if float(cfg.get("n_suppressed_effective") or 0.0) < MIN_SUPPRESSED_EFFECTIVE:
        reasons.append(
            f"only {_fmt(cfg.get('n_suppressed_effective'), 1)} effective suppressed "
            f"trades (floor {MIN_SUPPRESSED_EFFECTIVE:g})")
    if float(cfg.get("n_kept_effective") or 0.0) < MIN_KEPT_EFFECTIVE:
        reasons.append(
            f"only {_fmt(cfg.get('n_kept_effective'), 1)} effective kept trades "
            f"(floor {MIN_KEPT_EFFECTIVE:g})")
    if cfg.get("p_cluster") is None:
        reasons.append("cluster permutation p is untestable "
                       f"({cfg.get('cluster_reason') or 'no draws'})")
    elif not cfg.get("bh_reject"):
        reasons.append(
            f"not significant after Benjamini-Hochberg on the cluster p "
            f"(cluster p={cfg.get('p_cluster')})")
    windows = cfg.get("windows") or {}
    for name in cfg.get("protocol_windows") or ():
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
    ok, non_deg, n_with = held_out_verdict(
        windows, cfg.get("held_out_windows") or ())
    if not ok:
        reasons.append(
            f"drawdown holds on only {non_deg}/{n_with} held-out windows with legs "
            f"(need {HELD_OUT_MIN_FRACTION:.2f} of them, min {HELD_OUT_MIN_WINDOWS} "
            f"windows)")
    return (not reasons), reasons


def protocol_dd_reduction(cfg: dict) -> float:
    """Pooled protocol-window drawdown reduction, positive = better."""
    windows = cfg.get("windows") or {}
    total = 0.0
    for name in cfg.get("protocol_windows") or ():
        row = windows.get(name)
        if row and row.get("n_legs"):
            total += -float(row["dd_delta"])
    return round(total, 6)


def decide_recommendation(configs: Sequence[dict], mde: dict) -> dict:
    """Mechanically derive the Recommendation.

    ONLY primary-cohort configs can win — the exploratory grid is reported for
    completeness and can never produce a recommendation, because its
    hypotheses were selected by reading the same data they are scored on.
    """
    primary = [c for c in configs if c.get("cohort") == COHORT_PRIMARY]
    families = {}
    for family in FAMILIES:
        own = [c for c in primary if c.get("family") == family]
        passing = [c for c in own if config_verdict(c)[0]]
        if passing:
            best = sorted(passing,
                          key=lambda c: (-protocol_dd_reduction(c), c["config_id"]))[0]
            families[family] = {"winner": best, "n_tested": len(own),
                                "n_passing": len(passing)}
        else:
            families[family] = {"winner": None, "n_tested": len(own),
                                "n_passing": 0}
    if any(v["winner"] for v in families.values()):
        return {"verdict": "config", "families": families, "justification": ""}

    tested = sum(v["n_tested"] for v in families.values())
    n_significant = sum(1 for c in primary if c.get("bh_reject"))
    n_untestable = sum(1 for c in primary if c.get("p_cluster") is None)
    best = None
    for cfg in primary:
        p = cfg.get("p_cluster")
        if p is not None and (best is None or p < best[0]):
            best = (p, cfg["config_id"])

    def _limit(label: str) -> str:
        value = mde.get(f"pooled_{label}_cluster")
        if value is None:
            return f"nothing below {MDE_GRID_MAX:.1f} pp per trade"
        return f"{value:.2f} pp per trade"

    power_text = ""
    if "pooled_1410_cluster" in mde and "pooled_primary_cluster" in mde:
        power_text = (
            f"Measured detection limits under the cluster null: the #1410 design "
            f"could resolve {_limit('1410')}; this expanded design resolves "
            f"{_limit('primary')}. ")

    # The relationship between the raw H split's separation and the detection
    # limit is a FACT of this run, so state it from the numbers rather than
    # asserting a direction. Getting this backwards would be the study's worst
    # possible failure: a "no edge" claim on a design that simply could not see
    # one, or the reverse.
    obs = (mde.get("observed_separation_pp_by_pool") or {}).get("primary") or {}
    limit = mde.get("pooled_primary_cluster")
    p0 = mde.get("pooled_primary_cluster_p0")
    detail = ""
    if obs and limit is None:
        detail = (
            f"The primary cohort's detection limit is above {MDE_GRID_MAX:.1f} "
            f"pp per trade, so this design resolves no edge of any size the "
            f"injection grid covers. Nothing about the presence or absence of "
            f"an edge follows from its null result.")
    elif obs and limit is not None:
        largest = max(abs(float(v)) for v in obs.values())
        if largest >= limit:
            detail = (
                f"The raw anti-signal split — each family's own suppressed side "
                f"— does separate by up to {largest:.2f} pp per "
                f"trade, ABOVE the {limit:.2f} pp the design can resolve"
                + (f", and that contrast alone sits at cluster p={p0:.4f}"
                   if p0 is not None else "")
                + ". So the per-trade signal is not absent — what fails is the "
                "conversion of it into an implementable gate: none of the "
                "pre-registered hysteresis or sizing configurations captures it "
                "into a significant, economically acceptable form. A future "
                "attempt should change the RULE, not gather more of the same data.")
        else:
            detail = (
                f"The largest raw anti-signal separation on that same cohort is "
                f"{largest:.2f} pp per trade, BELOW the {limit:.2f} pp the "
                f"design can resolve. A separation under the limit is INVISIBLE "
                f"to this design, so the null result excludes an edge of "
                f"{limit:.2f} pp or larger and says nothing either way about "
                f"one the size the buckets show. Power is the binding "
                f"constraint here, not the absence of a signal.")
    best_text = (f" The strongest primary hypothesis reached cluster "
                 f"p={best[0]:.4f} (`{best[1]}`)." if best else "")
    return {
        "verdict": "inconclusive",
        "families": families,
        "justification": (
            f"No configuration of the {tested} primary hypotheses passed the "
            f"pre-registered acceptance rule. {n_significant} reached "
            f"Benjamini-Hochberg significance on the cluster permutation at "
            f"alpha={ALPHA}; {n_untestable} were untestable.{best_text} "
            f"{power_text}{detail}"
        ).strip(),
    }


def joint_adx_hurst_table(trades: Sequence[dict], hurst_window: int) -> dict:
    """Part D: the ADX-level x Hurst-level cell table for one family."""
    cells = {}
    for a in JOINT_ADX_BUCKETS:
        for h in JOINT_H_BUCKETS:
            cells[f"{a}|{h}"] = []
    for t in trades:
        a = joint_adx_bucket(t.get("adx"))
        h = joint_h_bucket((t.get("h") or {}).get(hurst_window))
        cells[f"{a}|{h}"].append(float(t["pnl_pct_net"]))
    out = {}
    for key, rets in cells.items():
        total_return, max_dd = compound_equity(rets)
        out[key] = {
            "trades": len(rets),
            "win_rate_pct": win_rate(rets),
            "mean_pnl_pct_net": round(float(np.mean(rets)), 6) if rets else None,
            "compounded_return_pct": total_return,
            "trade_seq_max_dd_pct": max_dd,
        }
    return out


def joint_separation_verdict(trades: Sequence[dict], hurst_window: int,
                             n_perm: int = N_PERM,
                             seed: int = SEED) -> dict:
    """#1412 Stage 0: does high-ADX + low-H separate beyond high-ADX alone?

    Restricted to ``ADX >= 25`` trades, contrasts ``H < 0.45`` against
    ``H >= 0.45`` under the cluster rotation. Material separation requires BOTH
    a Bonferroni-corrected significant cluster p AND an effect at least as
    large as the detection limit — an effect the design cannot resolve is not
    evidence, however large the point estimate looks.

    That limit is measured HERE, on the rows this contrast actually scores and
    at the bar this contrast is actually corrected by (``JOINT_ALPHA``, i.e.
    rank-1 over the families). Borrowing a limit measured on a different pool
    sets the bar by how much data some OTHER test had.

    The uninjected p and the limit use the SAME permutation count and seed.
    One consequence follows from that and is deliberate: a contrast whose p
    already clears the bar has a measured limit of 0.0, so the materiality
    condition is a FLOOR rather than a second hurdle. Different resolutions
    could contradict each other at the boundary and make that otherwise-dead
    branch reachable. The verdict still fails closed when the limit is
    unreachable, when no rotation exists, and when the pool cannot be rotated
    at all — the cases it was written for.
    """
    pool = [t for t in trades if joint_adx_bucket(t.get("adx")) == ">=25"]
    low = [t for t in pool
           if joint_h_bucket((t.get("h") or {}).get(hurst_window)) == "<0.45"]
    high = [t for t in pool
            if joint_h_bucket((t.get("h") or {}).get(hurst_window))
            in ("0.45-0.55", ">0.55")]
    base = {
        "hurst_window": hurst_window,
        "n_high_adx": len(pool),
        "n_low_h": len(low),
        "n_high_h": len(high),
        "mde_pp": None,
    }
    if not low or not high:
        return {**base, "separated": False, "p_cluster": None,
                "delta_mean_pp": None,
                "reason": "one side of the ADX>=25 contrast is empty"}
    sub = low + high
    values = [float(t["pnl_pct_net"]) for t in sub]
    suppressed = [True] * len(low) + [False] * len(high)
    # Score the delta on the same rows the null keeps, so the effect and the
    # p-value describe one pool.
    idx, excluded = usable_cluster_rows(sub)
    base["cluster_excluded_datasets"] = excluded
    base["n_scored"] = len(idx)
    if not idx:
        return {**base, "separated": False, "p_cluster": None,
                "delta_mean_pp": None,
                "reason": "no dataset spans enough calendar time to rotate"}
    sub = [sub[i] for i in idx]
    values = [values[i] for i in idx]
    suppressed = [suppressed[i] for i in idx]
    if not any(suppressed) or all(suppressed):
        return {**base, "separated": False, "p_cluster": None,
                "delta_mean_pp": None,
                "reason": "one side of the ADX>=25 contrast is empty"}
    v = np.asarray(values, dtype=float)
    m = np.asarray(suppressed, dtype=bool)
    delta = float(v[~m].mean() - v[m].mean())
    res = cluster_permutation_pvalue_group_diff(sub, values, suppressed,
                                                n_perm=n_perm, seed=seed)
    p = res.get("p")
    joint_mde = min_detectable_effect(sub, values, suppressed,
                                      family_size=len(FAMILIES), cluster=True,
                                      n_perm=n_perm, seed=seed)
    base["mde_pp"] = joint_mde
    big_enough = (joint_mde is not None and abs(delta) >= float(joint_mde))
    separated = bool(p is not None and p <= JOINT_ALPHA and big_enough)
    reason = ""
    if p is None:
        reason = res.get("reason") or "cluster p untestable"
    elif p > JOINT_ALPHA:
        reason = (f"cluster p={p} above the Bonferroni bar "
                  f"{JOINT_ALPHA:g}")
    elif not big_enough:
        reason = (f"separation {abs(delta):.2f} pp is below the measured "
                  f"detection limit "
                  f"{'unreachable' if joint_mde is None else f'{joint_mde:.2f} pp'}")
    return {**base, "separated": separated, "p_cluster": p,
            "delta_mean_pp": round(delta, 6), "reason": reason}


def dedup_entries(rows: Sequence[dict], window_order: Sequence[str]) -> list:
    """#1410's dedup, parameterized on the window order (which grew here).

    Key is ``(strategy, symbol, timeframe, entry_date)``; windows are iterated
    in chronological start order and the first occurrence wins, so the result
    does not depend on scheduling order.
    """
    order = {name: i for i, name in enumerate(window_order)}
    ordered = sorted(
        rows,
        key=lambda r: (
            order.get(r["window"], len(order)),
            str(r["strategy"]), str(r["symbol"]),
            str(r["timeframe"]), str(r["entry_date"]),
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


def timeframe_minutes(timeframe: str) -> int:
    """Bar length in minutes, for the coverage density check."""
    unit = str(timeframe)[-1].lower()
    value = int(str(timeframe)[:-1])
    scale = {"m": 1, "h": 60, "d": 1440, "w": 10080}
    if unit not in scale:
        raise ValueError(f"unsupported timeframe {timeframe!r}")
    return value * scale[unit]


def expected_bars(window: tuple, timeframe: str, reference_last) -> int:
    """Bars a COMPLETE cache would hold inside a window.

    ``reference_last`` closes an open-ended window and MUST be a run-level
    reference — the latest bar any dataset in the run reached — never the
    cell's own last bar. A per-cell cap lets a dataset that stops early inside
    the window define its own denominator and score 100% dense, which is the
    exact failure this density check exists to catch.
    """
    start = pd.Timestamp(window[0])
    if window[1] is not None:
        # A CLOSED window's expectation is its own span, for the same reason.
        end = pd.Timestamp(window[1])
    else:
        if reference_last is None:
            return 0
        end = pd.Timestamp(reference_last)
    if end <= start:
        return 0
    minutes = (end - start).total_seconds() / 60.0
    return int(minutes // timeframe_minutes(timeframe))


def coverage_audit(frames: dict, window_names: Sequence[str],
                   hurst_windows: Sequence[int]) -> dict:
    """Which (dataset, window) cells the cache can actually support.

    A cell survives only when its dataset carries enough lead before the window
    start for the deepest Hurst window's entry stamp AND enough bars INSIDE the
    window. The density floor is what catches a delisting gap: Binance.US
    delisted XRP for most of 2021-2023, so an unchecked "more than zero bars"
    rule would score a whole year on a few hundred bars and quietly weight it
    equally with a complete one. Everything dropped is listed in the manifest —
    never scored on partial history and never silently absent.
    """
    need_lead = max((required_lead_bars(hw) for hw in hurst_windows), default=0)
    # ONE reference bar for every open-ended cell in the run. Measuring each
    # dataset against its own last bar would make a dataset that stopped early
    # score 100% dense and contribute weeks against other datasets' months.
    last_bars = [f.index[-1] for f in frames.values()
                 if f is not None and not f.empty]
    reference_last = max(last_bars) if last_bars else None
    cells = {}
    dropped = []
    for (symbol, timeframe), frame in sorted(frames.items()):
        key = dataset_key(symbol, timeframe)
        for wname in window_names:
            start, end = WINDOWS[wname]
            ok = True
            reason = ""
            if frame is None or frame.empty:
                ok, reason = False, "no cached bars"
            else:
                lead = warmup_lead_bars(frame.index, pd.Timestamp(start))
                in_window = len(slice_window(frame, WINDOWS[wname]))
                want = expected_bars(WINDOWS[wname], timeframe, reference_last)
                if lead < need_lead:
                    ok = False
                    reason = (f"lead {lead} bars before {start} < required "
                              f"{need_lead}")
                elif in_window <= 0:
                    ok = False
                    reason = f"no bars inside [{start}, {end or 'latest'})"
                elif want > 0 and in_window < MIN_WINDOW_BAR_FRACTION * want:
                    ok = False
                    reason = (f"only {in_window} of ~{want} expected bars inside "
                              f"[{start}, {end or 'latest'}) "
                              f"({in_window / want:.0%} < "
                              f"{MIN_WINDOW_BAR_FRACTION:.0%}) — data gap")
            cells[f"{key}|{wname}"] = ok
            if not ok:
                dropped.append({"dataset": key, "window": wname, "reason": reason})
    return {
        "required_lead_bars": need_lead,
        "min_window_bar_fraction": MIN_WINDOW_BAR_FRACTION,
        "reference_last_bar": None if reference_last is None else str(reference_last),
        "n_cells": len(cells),
        "n_kept": int(sum(1 for v in cells.values() if v)),
        "n_dropped": len(dropped),
        "cells": cells,
        "dropped": dropped,
    }


def scored_warmup_leads(frames: dict, coverage: dict,
                        scored_windows: Sequence[str]) -> dict:
    """Warm-up lead per dataset, measured against the windows IT ACTUALLY SCORES.

    Not against the earliest window in the run. A dataset whose early cells the
    coverage audit already dropped — a late listing like SOL, a delisting gap
    like XRP — has no unscored bars to warn about. Auditing it against a window
    it never touches would make the report print "the NaN bucket carries real
    trades" over a table whose NaN buckets are empty, which is worse than no
    warning at all: it teaches the reader to distrust a correct table.
    """
    leads = {}
    for (symbol, timeframe), frame in frames.items():
        if frame is None or frame.empty:
            continue
        key = dataset_key(symbol, timeframe)
        own = [w for w in scored_windows
               if (coverage.get("cells") or {}).get(f"{key}|{w}")]
        if not own:
            continue
        own_first = min(pd.Timestamp(WINDOWS[w][0]) for w in own)
        leads[key] = warmup_lead_bars(frame.index, own_first)
    return leads


def symbol_return_correlations(frames: dict) -> dict:
    """Symbol-level Pearson correlation of DAILY log returns.

    Computed off the finest cached timeframe per symbol, resampled to 1d, over
    the overlapping span. This is the input the effective-N estimator credits
    between two different symbols; same-symbol pairs never consult it.
    """
    finest = {}
    for (symbol, timeframe), frame in sorted(frames.items()):
        if frame is None or frame.empty:
            continue
        rank = (timeframe_minutes(timeframe), timeframe)
        current = finest.get(symbol)
        if current is None or rank < current[0]:
            finest[symbol] = (rank, frame)

    daily = {}
    for symbol in sorted(finest):
        frame = finest[symbol][1]
        closes = frame["close"].astype(float)
        d = closes.resample("1D").last().dropna()
        if len(d) < 30:
            continue
        daily[symbol] = np.log(d).diff().dropna()
    out = {}
    syms = sorted(daily)
    for i, a in enumerate(syms):
        for b in syms[i + 1:]:
            joined = pd.concat([daily[a], daily[b]], axis=1, join="inner").dropna()
            if len(joined) < 30:
                continue
            if (joined.iloc[:, 0].nunique() < 2
                    or joined.iloc[:, 1].nunique() < 2):
                # Pearson correlation is undefined for a constant return
                # series. Skip it explicitly instead of relying on corr() to
                # emit NaN (and a runtime warning) for the finite guard below.
                continue
            rho = float(joined.iloc[:, 0].corr(joined.iloc[:, 1]))
            if math.isfinite(rho):
                out[(a, b)] = round(rho, 6)
    return out


# ---------------------------------------------------------------------------
# ADX stamp (look-ahead safe, warm-up honest).
# ---------------------------------------------------------------------------

def adx_series(frame: pd.DataFrame, period: int = ADX_PERIOD) -> pd.Series:
    """Per-bar ADX with WARM-UP BARS MASKED TO NaN.

    ``compute_regime`` fills warm-up bars with 0.0, not NaN
    (``shared_tools/regime.py``), so an unmasked stamp would file every warm-up
    bar under "low ADX" — a silent, systematic mislabel at the start of every
    frame. NaN is unknown and stays its own bucket, exactly as H does.
    """
    from regime import _compute_adx_components  # noqa: WPS437 - the resolved SSoT
    n = len(frame)
    values = np.full(n, np.nan, dtype=float)
    if n == 0 or n <= period:
        return pd.Series(values, index=frame.index, name="adx")
    comp = _compute_adx_components(frame["high"].values, frame["low"].values,
                                   frame["close"].values, period)
    arr = np.asarray(comp["adx"], dtype=float)
    start = int(comp["adx_start"])
    values[start:] = arr[start:]
    return pd.Series(values, index=frame.index, name="adx")


def adx_entry_stamp(frame: pd.DataFrame, period: int = ADX_PERIOD) -> pd.Series:
    """ADX indexed by FILL bar, on H's exact shift convention.

    ``decision_series`` is ``shift(1)`` (a bar-N signal reads bars through N-1)
    and the fill-bar stamp is ``shift(2)`` (the trade fills at N+1 but was
    gated by the decision at N). ADX must use the SAME two shifts or the two
    features in the joint table would sit on different look-ahead rules.
    """
    return entry_stamp_series(adx_series(frame, period=period))


# ---------------------------------------------------------------------------
# Engine arms.
# ---------------------------------------------------------------------------

def trade_samples_with_span(results: dict) -> list:
    """``eval_windows.trade_samples_from_results`` plus the holding period.

    ``exit_date`` is on every Backtester trade dict but the shared helper does
    not carry it, and the effective-N estimator needs the interval to decide
    which trades coexist. ``pnl_pct``/``pnl_pct_net`` are computed identically
    to the shared helper so the two never diverge.
    """
    out = []
    for t in results.get("trades") or []:
        gross = float(t["pnl_pct"])
        notional = float(t.get("shares") or 0.0) * float(t.get("entry_price") or 0.0)
        net = (float(t["pnl"]) / notional * 100.0
               if notional > 0 and t.get("pnl") is not None else gross)
        out.append({
            "entry_date": str(t["entry_date"]),
            "exit_date": str(t.get("exit_date")),
            "pnl_pct": gross,
            "pnl_pct_net": round(net, 6),
        })
    return out


def _run_arm(reg, name: str, symbol: str, timeframe: str, df: pd.DataFrame,
             armed: Optional[np.ndarray], overrides: dict) -> Optional[dict]:
    """One Backtester run on a pre-sliced frame, optionally entry-masked.

    Kwargs mirror ``eval_windows.run_leg``'s no-regime, no-profile path exactly;
    every leg additionally verifies its ungated arm against ``run_leg`` itself,
    so the mirror can never drift silently.
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
    leg["trade_samples"] = trade_samples_with_span(results)
    return leg


def _leg_metrics(leg: Optional[dict]) -> dict:
    if leg is None:
        return {"return_pct": 0.0, "max_dd_pct": 0.0, "chop_loss": 0.0, "trades": 0}
    rets = [s["pnl_pct_net"] for s in leg.get("trade_samples") or []]
    return {
        "return_pct": float(leg["return_pct"]),
        "max_dd_pct": float(leg["max_dd_pct"]),
        "chop_loss": chop_loss(rets),
        "trades": int(leg["trades"]),
    }


CONFIG_ID_SEP = study1410.CONFIG_ID_SEP
gate_config_id = study1410.gate_config_id
size_config_id = study1410.size_config_id


def build_leg(reg, family: str, exemplar: str, symbol: str, timeframe: str,
              window_name: str, full: pd.DataFrame, hurst_by_window: dict,
              adx_stamp: pd.Series, verify_mirror: bool = True) -> Optional[dict]:
    """Every arm for one (exemplar, dataset, window) cell.

    #1410's ``build_leg`` plus the entry-ADX stamp and the cohort label; reads
    this study's window dict rather than #1410's module global.
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

    stamps = {}
    decisions = {}
    for hw, rolling in hurst_by_window.items():
        stamps[hw] = entry_stamp_series(rolling).reindex(df.index).to_numpy(dtype=float)
        decisions[hw] = decision_series(rolling).reindex(df.index).to_numpy(dtype=float)
    adx_vals = adx_stamp.reindex(df.index).to_numpy(dtype=float)

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

    cohort = cell_cohort(symbol, timeframe, window_name)
    trades = []
    for sample in ungated.get("trade_samples") or []:
        key = str(sample["entry_date"])
        pos = key_pos.get(key)
        if pos is None:
            raise AssertionError(
                f"trade entry_date {key!r} is not a bar of the {window_name} slice "
                f"for {exemplar} {symbol} {timeframe}")
        try:
            exit_ns = int(pd.Timestamp(sample["exit_date"]).value)
        except (ValueError, TypeError):
            exit_ns = None
        trades.append({
            "strategy": exemplar,
            "symbol": symbol,
            "timeframe": timeframe,
            "window": window_name,
            "cohort": cohort,
            "entry_date": key,
            "entry_ns": int(pd.Timestamp(key).value),
            "exit_ns": exit_ns,
            "pnl_pct_net": float(sample["pnl_pct_net"]),
            "adx": (None if not math.isfinite(adx_vals[pos])
                    else float(adx_vals[pos])),
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
        "cohort": cohort,
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
        by_bucket[bucket_label((t.get("h") or {}).get(hurst_window))].append(
            t["pnl_pct_net"])
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


def _cohort_legs(legs: Sequence[dict], family: str, cohort: str) -> list:
    return [lg for lg in legs
            if lg["family"] == family and lg["cohort"] == cohort]


def _window_rows_gate(legs: Sequence[dict], family: str, cohort: str,
                      config_id: str) -> dict:
    """Per-window mean deltas for a gate config, over that cohort's legs only."""
    rows = {}
    own = _cohort_legs(legs, family, cohort)
    for wname in WINDOW_ORDER:
        cells = [lg for lg in own
                 if lg["window"] == wname and config_id in lg["gated"]]
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


def _window_rows_size(legs: Sequence[dict], family: str, cohort: str,
                      hurst_window: int, gain: float) -> dict:
    """Per-window mean deltas for a sizing config, over that cohort's legs only.

    Both arms are TRADE-GRANULAR re-compoundings of the same ungated sequence,
    so they are like-for-like with each other and NOT with a bar-level Part B
    drawdown.
    """
    sense = FAMILY_SENSE[family]
    rows = {}
    own = _cohort_legs(legs, family, cohort)
    for wname in WINDOW_ORDER:
        cells = [lg for lg in own if lg["window"] == wname]
        if not cells:
            rows[wname] = {"n_legs": 0}
            continue
        dd_deltas, chop_deltas, ret_g, ret_u = [], [], [], []
        n_used = 0
        for lg in cells:
            rets = [t["pnl_pct_net"] for t in lg["trades"]]
            if not rets:
                continue
            mults = [size_multiplier((t.get("h") or {}).get(hurst_window), sense, gain)
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


def _config_shell(family: str, cohort: str, mode: str, hw: int,
                  arm=None, disarm=None, gain=None) -> dict:
    cid = (gate_config_id(family, hw, arm, disarm) if mode == "gate"
           else size_config_id(family, hw, gain))
    protocol = (PRIMARY_PROTOCOL_WINDOWS if cohort == COHORT_PRIMARY
                else EXPLORATORY_PROTOCOL_WINDOWS)
    held_out = (PRIMARY_HELD_OUT_WINDOWS if cohort == COHORT_PRIMARY
                else EXPLORATORY_HELD_OUT_WINDOWS)
    return {
        "config_id": cid,
        "cohort": cohort,
        "family": family,
        "mode": mode,
        "sense": FAMILY_SENSE[family],
        "hurst_window": hw,
        "arm": arm,
        "disarm": disarm,
        "gain": gain,
        "protocol_windows": list(protocol),
        "held_out_windows": list(held_out),
    }


def _sweep_grid(cohort: str, hurst_windows: Sequence[int]) -> list:
    """(family, mode, hw, arm, disarm, gain) tuples this cohort tests.

    PRIMARY tests only the four pinned hypotheses; EXPLORATORY tests the full
    #1410 grid. The two lists are disjoint by construction, which is what keeps
    their Benjamini-Hochberg denominators from ever merging.
    """
    grid = []
    for family in FAMILIES:
        for hw in hurst_windows:
            for arm, disarm in GATE_PAIRS[family]:
                cid = gate_config_id(family, hw, arm, disarm)
                if cohort == COHORT_PRIMARY and cid not in PRIMARY_CONFIG_IDS:
                    continue
                grid.append((family, "gate", hw, arm, disarm, None))
            for gain in SIZING_GAINS:
                cid = size_config_id(family, hw, gain)
                if cohort == COHORT_PRIMARY and cid not in PRIMARY_CONFIG_IDS:
                    continue
                grid.append((family, "size", hw, None, None, gain))
    return grid


def build_configs(legs: Sequence[dict], pooled: dict, hurst_windows: Sequence[int],
                  rho_by_symbol: dict, n_perm: int, seed: int) -> list:
    """Every swept config in both cohorts, with both p-values and economics."""
    configs = []
    for cohort in (COHORT_PRIMARY, COHORT_EXPLORATORY):
        for family, mode, hw, arm, disarm, gain in _sweep_grid(cohort, hurst_windows):
            sense = FAMILY_SENSE[family]
            trades = [t for t in (pooled.get(family) or [])
                      if t["cohort"] == cohort]
            cfg = _config_shell(family, cohort, mode, hw, arm, disarm, gain)
            cid = cfg["config_id"]
            if mode == "gate":
                sub = [t for t in trades if cid in t["armed"]]
                # ONE pool for everything downstream. A dataset the cluster null
                # cannot rotate leaves the free p, the counts and the effective-N
                # floors too, so no config can clear a volume floor on rows that
                # never entered the significance test.
                idx, excluded = usable_cluster_rows(sub)
                n_excluded = len(sub) - len(idx)
                sub = [sub[i] for i in idx]
                values = [t["pnl_pct_net"] for t in sub]
                suppressed = [not t["armed"][cid] for t in sub]
                cfg["p_raw"] = permutation_pvalue_group_diff(
                    values, suppressed, n_perm=n_perm, seed=seed)
                cluster = cluster_permutation_pvalue_group_diff(
                    sub, values, suppressed, n_perm=n_perm, seed=seed)
                sup_rows = [t for t, s in zip(sub, suppressed) if s]
                kept_rows = [t for t, s in zip(sub, suppressed) if not s]
                cfg["windows"] = _window_rows_gate(legs, family, cohort, cid)
            else:
                sub = list(trades)
                idx, excluded = usable_cluster_rows(sub)
                n_excluded = len(sub) - len(idx)
                sub = [sub[i] for i in idx]
                rets = [t["pnl_pct_net"] for t in sub]
                mults = [size_multiplier((t.get("h") or {}).get(hw), sense, gain)
                         for t in sub]
                cfg["p_raw"] = permutation_pvalue_weighted(
                    rets, mults, n_perm=n_perm, seed=seed)
                cluster = cluster_permutation_pvalue_weighted(
                    sub, rets, mults, n_perm=n_perm, seed=seed)
                # A sizing config's "suppressed" analogue is the down-weighted
                # side (m < 1); the same volume floors then apply unchanged.
                sup_rows = [t for t, m in zip(sub, mults) if m < 1.0]
                kept_rows = [t for t, m in zip(sub, mults) if m >= 1.0]
                cfg["windows"] = _window_rows_size(legs, family, cohort, hw, gain)
            cfg["p_cluster"] = cluster.get("p")
            cfg["cluster_draws"] = cluster.get("n_draws")
            cfg["cluster_excluded_datasets"] = excluded
            cfg["cluster_excluded_trades"] = n_excluded
            cfg["cluster_offset_range"] = cluster.get("offset_range")
            cfg["cluster_distinct_offsets"] = cluster.get("n_distinct_offsets")
            cfg["cluster_reason"] = cluster.get("reason")
            cfg["n_pooled_trades"] = len(sub)
            cfg["n_suppressed"] = len(sup_rows)
            cfg["n_kept"] = len(kept_rows)
            cfg["n_pooled_effective"] = effective_n(sub, rho_by_symbol)
            cfg["n_suppressed_effective"] = effective_n(sup_rows, rho_by_symbol)
            cfg["n_kept_effective"] = effective_n(kept_rows, rho_by_symbol)
            configs.append(cfg)
    return configs


def apply_bh_by_cohort(configs: Sequence[dict], alpha: float = ALPHA) -> None:
    """Benjamini-Hochberg over the CLUSTER p, separately per cohort.

    The primary and exploratory families never share a denominator: the primary
    hypotheses were selected by reading #1410's outcomes, so pooling them with
    the exploratory grid would either inflate the primary penalty or lend the
    exploratory arm the primary arm's protection. Untestable configs stay in
    each denominator via ``family_size``, so dropping them can never make a
    correction more permissive.
    """
    for cohort in (COHORT_PRIMARY, COHORT_EXPLORATORY):
        own = [c for c in configs if c.get("cohort") == cohort]
        # RESET, never setdefault: a stale True from an earlier pass must not
        # survive a correction that would not grant it.
        for cfg in own:
            cfg["bh_reject"] = False
        testable = [c for c in own if c.get("p_cluster") is not None]
        if not testable:
            continue
        flags = benjamini_hochberg([c["p_cluster"] for c in testable], alpha=alpha,
                                   family_size=len(own))
        for cfg, flag in zip(testable, flags):
            cfg["bh_reject"] = bool(flag)


# ---------------------------------------------------------------------------
# Report rendering. `## Recommendation` is always the FINAL section and is a
# mechanical render of decide_recommendation — never hand-written.
# ---------------------------------------------------------------------------

def _fmt(value, digits: int = 2, suffix: str = "") -> str:
    if value is None:
        return "-"
    if isinstance(value, float) and not math.isfinite(value):
        return "-"
    return f"{value:.{digits}f}{suffix}"


def _fmt_p(value) -> str:
    return "-" if value is None else f"{value:.4f}"


def render_recommendation(decision: dict, mde: dict) -> str:
    lines = ["## Recommendation", ""]
    if decision["verdict"] == "inconclusive":
        lines.append("INCONCLUSIVE")
        lines.append("")
        lines.append(decision["justification"])
        lines.append("")
        # The closing guidance must follow the SAME branch the justification
        # took. A fixed "no edge exists" sign-off under a run that measured a
        # resolvable separation would be the report telling a reader the
        # opposite of its own table.
        obs = ((mde.get("observed_separation_pp_by_pool") or {})
               .get("primary") or {})
        limit = mde.get("pooled_primary_cluster")
        rule_failed = bool(
            obs and limit is not None
            and max(abs(float(v)) for v in obs.values()) >= limit)
        lines.append(
            "Do not build a Hurst entry gate on this evidence. That is where "
            "this agrees with #1410; the reason differs, and the reason is what "
            "should drive what happens next.")
        lines.append("")
        if rule_failed:
            lines.append(
                "This run resolved a separation the design CAN see, and no "
                "tested rule converted it into a gate. So more data of the same "
                "kind is the one thing that will not help. If the question is "
                "revisited, change the RULE: the hysteresis band and the size "
                "curve swept here are the suspects, not the sample. Any such "
                "attempt must re-register its hypotheses BEFORE looking at these "
                "numbers and score them on cells this study did not use, or it "
                "inherits exactly the selection problem this design was built "
                "to avoid.")
        else:
            lines.append(
                "This run did NOT resolve the separation its own buckets show, "
                "so its null is a statement about the design's power and not "
                "about the market. Do not read it as evidence that no edge "
                "exists. What the run does establish is an upper bound: an edge "
                "at or above the primary cohort's detection limit would have "
                "been caught, and none was. Settling the smaller effect needs a "
                "design that can see it — a sharper contrast, a larger primary "
                "cohort, or fewer pre-registered hypotheses sharing the "
                "correction — and it must re-register those hypotheses BEFORE "
                "looking at these numbers, or it inherits exactly the selection "
                "problem this design was built to avoid.")
        # The pools OUTSIDE the confirmatory cohort can be better powered than
        # it is, because they carry more rows. Where one of them resolves its
        # own separation, its uninjected p is evidence the primary cohort is
        # too underpowered to supply — reported as the exploratory context it
        # is, never as a substitute for the pre-registered test.
        resolved = []
        for key, label in (("1410", "the #1410 design's cells"),
                           ("primary", "the primary cohort"),
                           ("exploratory", "this study's exploratory grid")):
            c = mde.get(f"pooled_{key}_cluster")
            seps = ((mde.get("observed_separation_pp_by_pool") or {})
                    .get(key) or {})
            p0 = mde.get(f"pooled_{key}_cluster_p0")
            if not seps or c is None or p0 is None:
                continue
            largest = max(abs(float(v)) for v in seps.values())
            if largest >= c:
                resolved.append(
                    f"{label} ({largest:.2f} pp separation against a "
                    f"{c:.2f} pp limit, cluster p={p0:.4f})")
        if resolved:
            lines.append("")
            lines.append(
                "One reading the table supports and the pre-registered test "
                "cannot: pools outside the confirmatory cohort carry more rows, "
                "so they resolve a separation this design can see, and it does "
                "not survive the cluster null — " + "; ".join(resolved) + ". "
                "That contrast is EXPLORATORY: it was not pre-registered, and "
                "on those rows the split is chosen after seeing them. It "
                "licenses no gate. What it does show is that the visible "
                "separation there is consistent with whole datasets moving "
                "together, rather than with H sorting trades within them.")
        lines.append("")
        lines.append(
            "#1411's `hurst_gate` stays DEFAULT-OFF with no recommended "
            "thresholds. Nothing in this report licenses shipping one.")
        return "\n".join(lines) + "\n"

    for family in FAMILIES:
        entry = decision["families"][family]
        winner = entry["winner"]
        lines.append(f"### {family}")
        lines.append("")
        if winner is None:
            lines.append(
                f"No configuration of the {entry['n_tested']} primary hypotheses "
                f"tested beat ungated under the pre-registered rule. Do not gate "
                f"or size the {family} family on the Hurst exponent.")
            lines.append("")
            continue
        proto = {w: (winner["windows"].get(w) or {}) for w in winner["protocol_windows"]}
        evidence = "; ".join(
            f"{w}: drawdown {_fmt(proto[w].get('dd_delta'), 2, ' pp')}, "
            f"chop {_fmt(proto[w].get('chop_delta'), 2, ' pp')}, "
            f"return {_fmt(proto[w].get('ret_gated'))} vs "
            f"{_fmt(proto[w].get('ret_ungated'))}"
            for w in winner["protocol_windows"])
        if winner["mode"] == "gate":
            lines.append("- Mode: **gate** (hard entry gate, hysteresis)")
            lines.append(f"- Arm / disarm: **{winner['arm']:g} / {winner['disarm']:g}** "
                         f"({winner['sense']})")
        else:
            lines.append("- Mode: **size** (entry size multiplier, no hard gate)")
            lines.append(f"- Gain: **{winner['gain']:g}**, "
                         f"m = clamp(1 + gain x e, {SIZING_CLAMP_LO:g}, "
                         f"{SIZING_CLAMP_HI:g}), NaN -> "
                         f"{SIZING_NAN_MULTIPLIER:g} ({winner['sense']})")
        lines.append(f"- Hurst window length: **{winner['hurst_window']} bars**")
        lines.append(f"- Evidence: cluster p={_fmt_p(winner['p_cluster'])} "
                     f"(free-shuffle p={_fmt_p(winner['p_raw'])}), "
                     f"Benjamini-Hochberg significant at alpha={ALPHA}; "
                     f"{_fmt(winner['n_suppressed_effective'], 1)} effective "
                     f"suppressed / {_fmt(winner['n_kept_effective'], 1)} effective "
                     f"kept trades (of {winner['n_suppressed']}/{winner['n_kept']} "
                     f"nominal); {evidence}.")
        lines.append(f"- Config id: `{winner['config_id']}` "
                     f"({entry['n_passing']}/{entry['n_tested']} primary configs "
                     f"passed).")
        lines.append("")
    return "\n".join(lines).rstrip() + "\n"


def _render_bucket_table(table: dict) -> list:
    lines = [
        "| Bucket | Trades | Win rate | Mean net % | Median net % | Compounded % | "
        "Trade-seq max DD % | Chop loss |",
        "|--------|-------:|---------:|-----------:|-------------:|-------------:|"
        "-------------------:|----------:|",
    ]
    for bucket in BUCKETS:
        row = table.get(bucket) or {}
        lines.append(
            f"| `{bucket}` | {row.get('trades', 0)} | "
            f"{_fmt(row.get('win_rate_pct'), 1, '%')} | "
            f"{_fmt(row.get('mean_pnl_pct_net'))} | "
            f"{_fmt(row.get('median_pnl_pct_net'))} | "
            f"{_fmt(row.get('compounded_return_pct'))} | "
            f"{_fmt(row.get('trade_seq_max_dd_pct'))} | "
            f"{_fmt(row.get('chop_loss_pct'))} |")
    lines.append("")
    return lines


def _render_config_table(cfgs: Sequence[dict], protocol: Sequence[str]) -> list:
    head = ("| Config | Mode | W | Pooled N (eff) | sup/kept eff | free p | cluster p | "
            "BH sig |")
    sep = ("|--------|------|--:|----------------|--------------|-------:|----------:|"
           ":------:|")
    for w in protocol:
        head += f" {w} dd | {w} chop | {w} ret (arm/base) |"
        sep += "------:|--------:|-------------------|"
    head += " Verdict |"
    sep += "---------|"
    lines = [head, sep]
    for cfg in cfgs:
        row = (f"| `{cfg['config_id']}` | {cfg['mode']} | {cfg['hurst_window']} | "
               f"{cfg['n_pooled_trades']} ({_fmt(cfg['n_pooled_effective'], 1)}) | "
               f"{_fmt(cfg['n_suppressed_effective'], 1)}/"
               f"{_fmt(cfg['n_kept_effective'], 1)} | "
               f"{_fmt_p(cfg['p_raw'])} | {_fmt_p(cfg['p_cluster'])} | "
               f"{'yes' if cfg.get('bh_reject') else 'no'} |")
        for w in protocol:
            r = cfg["windows"].get(w) or {}
            if not r.get("n_legs"):
                row += " - | - | - |"
            else:
                row += (f" {_fmt(r['dd_delta'])} | {_fmt(r['chop_delta'])} | "
                        f"{_fmt(r['ret_gated'])} / {_fmt(r['ret_ungated'])} |")
        ok, reasons = config_verdict(cfg)
        row += f" {'PASS' if ok else '; '.join(reasons)} |"
        lines.append(row)
    lines.append("")
    return lines


def _render_joint_table(table: dict) -> list:
    lines = [
        "| ADX | H | Trades | Win rate | Mean net % | Compounded % | Trade-seq max DD % |",
        "|-----|---|-------:|---------:|-----------:|-------------:|-------------------:|",
    ]
    for a in JOINT_ADX_BUCKETS:
        for h in JOINT_H_BUCKETS:
            row = table.get(f"{a}|{h}") or {}
            lines.append(
                f"| `{a}` | `{h}` | {row.get('trades', 0)} | "
                f"{_fmt(row.get('win_rate_pct'), 1, '%')} | "
                f"{_fmt(row.get('mean_pnl_pct_net'))} | "
                f"{_fmt(row.get('compounded_return_pct'))} | "
                f"{_fmt(row.get('trade_seq_max_dd_pct'))} |")
    lines.append("")
    return lines


def render_report(payload: dict) -> str:
    """Full Markdown report. `## Recommendation` is guaranteed to be last."""
    pre = payload["pre_registered"]
    run = payload["run_summary"]
    cfgs = payload["configs"]
    mde = payload.get("mde") or {}
    decision = payload["decision"]
    hurst_windows = pre["hurst_windows"]

    out = []
    out.append("# Hurst gate power study (#1422)")
    out.append("")
    out.append(
        "**SUPERSEDED by the #1424 resolution study.** This file is NOT the "
        "live-evidence contract path. `backtest/research/hurst_gate_calibration.md` "
        "is that path — `scheduler/hurst_gate.go`, `docs/ARCHITECTURE.md` and "
        "#1412's Stage 0 gate all read it — and "
        "`research/hurst_1424_gate_resolution.py` owns it now. #1424 keeps this "
        "design and changes three things: ONE pre-registered primary hypothesis "
        "instead of four, pre-2020 calendar clusters from two additional venues, "
        "and a bounded-variance primary target. Read #1424's report for the live "
        "verdict; read this one for the design it inherits and for the interim "
        "look #1424 discloses.")
    out.append("")
    out.append(
        "Report-only evidence on whether a Hurst-based entry gate is worth "
        "building. Nothing here is wired to the scheduler, to config, or to any "
        "live path. It supersedes the #1410 study, whose own render lives at "
        "`hurst_1410_gate_calibration.md`.")
    out.append("")
    out.append(
        "#1410 returned INCONCLUSIVE on a single re-used sample, a 30-hypothesis "
        "grid, and a null that treated correlated concurrent trades as "
        "independent. This study fixes all three and MEASURES the detection "
        "limit. That last part is what makes a null result readable: with a "
        "measured limit beside the observed separation, an inconclusive verdict "
        "says WHICH of two very different things happened — the edge is too "
        "small to matter, or the edge is visible and the tested RULE fails to "
        "capture it. The Recommendation section states which one this run found, "
        "derived from the numbers rather than assumed.")
    out.append("")
    out.append(
        f"Generated by `backtest/research/hurst_1422_gate_power.py` (schema "
        f"{payload['schema_version']}). Every number below is rendered from "
        f"`hurst_1422_gate_power.json`, produced by the same run.")
    out.append("")

    out.append("## Pre-registered design")
    out.append("")
    out.append(
        "These constants live at the top of the study script and were fixed "
        "before the sweep ran. The Recommendation is the mechanical output of "
        "the acceptance rule applied to them.")
    out.append("")
    out.append(
        "- Estimator: `hurst_exponent` from "
        "`shared_strategies/open/indicators_core.py` (#1409 SSoT). Never "
        "reimplemented here.")
    out.append(
        "- NaN policy: NaN is its OWN bucket for BOTH H and ADX, never coerced "
        "to 0.5 / 25. It neither arms nor disarms the gate (state holds) and "
        "gives a size multiplier of exactly 1.")
    out.append(f"- Hurst window lengths: {', '.join(str(h) for h in hurst_windows)} bars.")
    out.append(f"- Buckets on H at entry: {', '.join('`' + b + '`' for b in BUCKETS)}.")
    out.append(f"- History floor: {pre['history_since']}. Data source exchange: "
               f"`{pre['platform']}`. Fee model: `{pre['fee_platform']}` (plus the "
               f"Backtester's 5 bps slippage default). The two platform axes are "
               f"independent and are never coupled.")
    out.append(f"- Datasets ({len(pre['datasets'])}): "
               f"{', '.join('`' + d + '`' for d in pre['datasets'])}.")
    out.append("- Windows: " + "; ".join(
        f"`{k}` {v[0]}..{v[1] or 'latest'}" for k, v in pre["windows"].items()) + ".")
    out.append(
        f"- Primary cohort economics: protocol "
        f"{', '.join('`' + w + '`' for w in PRIMARY_PROTOCOL_WINDOWS)}; held-out "
        f"{', '.join('`' + w + '`' for w in PRIMARY_HELD_OUT_WINDOWS)}.")
    out.append(
        f"- Exploratory cohort economics: #1410's split verbatim — protocol "
        f"{', '.join('`' + w + '`' for w in EXPLORATORY_PROTOCOL_WINDOWS)}; held-out "
        f"{', '.join('`' + w + '`' for w in EXPLORATORY_HELD_OUT_WINDOWS)}.")
    out.append(
        f"- Volume floors on EFFECTIVE N: >= {MIN_SUPPRESSED_EFFECTIVE:g} suppressed "
        f"and >= {MIN_KEPT_EFFECTIVE:g} kept. Held-out rule: drawdown must not "
        f"degrade on >= 2/3 of the held-out windows carrying legs, with at least "
        f"{HELD_OUT_MIN_WINDOWS} such windows.")
    out.append(
        f"- Inference: free-shuffle permutation (the #1410 null, for continuity) "
        f"AND a shared circular calendar rotation (the cluster null, which the "
        f"verdict reads). {pre['n_perm']} draws, seed {pre['seed']}, minimum "
        f"offset {MIN_OFFSET_DAYS} days. Benjamini-Hochberg at alpha={ALPHA}, "
        f"applied SEPARATELY to the primary and exploratory cohorts.")
    offs = [c.get("cluster_distinct_offsets") for c in cfgs
            if c.get("cluster_distinct_offsets")]
    out.append(
        f"- Rotation on ragged spans: offsets are drawn against the pool's "
        f"SHORTEST span, so every retained dataset hosts every offset and two "
        f"concurrent datasets always shift by the same calendar amount. "
        f"Drawing against the longest span instead would push offsets past a "
        f"shorter dataset's end, and both ways out of that cost something: "
        f"leaving it unrotated hands the null its observed alignment (p too "
        f"high), and wrapping it rotates it by a different calendar amount than "
        f"its neighbours, which understates the correlation the null exists to "
        f"preserve (p too LOW). The false positive is the worse failure, so the "
        f"range is capped and the wrap survives only as a dormant guard. "
        + (f"Narrowest shared range in this run: {min(offs)} distinct offsets. "
           if offs else "")
        + f"A dataset under {MIN_CLUSTER_SPAN_DAYS} days cannot host the "
          f"`[{MIN_OFFSET_DAYS}, span-{MIN_OFFSET_DAYS}]` band at all and leaves "
          f"the contrast, the counts and the effective-N floors together, never "
          f"just the offset range.")
    out.append(
        f"- Minimum detectable effect: deterministic shift of the suppressed "
        f"group — each family's OWN anti-signal side, so the injection runs in "
        f"the direction that family's gate would exploit — grid "
        f"{MDE_GRID_STEP:g} pp to {MDE_GRID_MAX:g} pp then a "
        f"{MDE_REFINE_STEP:g} pp refinement, scored against the rank-1 "
        f"Benjamini-Hochberg threshold with {pre['n_perm_mde']} draws.")
    out.append(f"- Joint table: ADX period {ADX_PERIOD} (Wilder), split at "
               f"{ADX_SPLIT:g} (the composite classifier default). Its "
               f"significance p and contrast-local detection limit both use "
               f"the inference resolution of {pre['n_perm']} draws and seed "
               f"{pre['seed']}; they cannot disagree at the boundary merely "
               f"because one used the lower {pre['n_perm_mde']}-draw general "
               f"MDE resolution.")
    out.append("")

    out.append("### Cohorts — why the primary set is trustworthy")
    out.append("")
    out.append(
        "The four PRIMARY hypotheses were chosen by reading #1410's raw "
        "p-values (per family and mode, the smallest). That selection is only "
        "legitimate because the primary cohort is DISJOINT from the data those "
        "p-values came from: a cell is primary iff it is absent from #1410's "
        "6-dataset x 5-window grid — a pre-2023 window, or a symbol #1410 never "
        "scored. A new TIMEFRAME on an audit symbol over a #1410 window is the "
        "same tape resampled and stays exploratory.")
    out.append("")
    out.append("Primary hypotheses: " +
               ", ".join(f"`{c}`" for c in PRIMARY_CONFIG_IDS) + ".")
    out.append("")
    out.append(
        f"The exploratory grid re-runs all {run['n_exploratory_configs']} #1410 "
        "configurations over the expanded data under its own correction. It can "
        "never produce a recommendation.")
    out.append("")

    out.append("### Look-ahead invariant")
    out.append("")
    out.append(
        "Rolling H at bar `i` uses closes `[i-W+1, i]`. The DECISION series is "
        "that shifted one bar, so a signal at bar N reads H through N-1; the "
        "backtester fills a bar-N signal at N+1's open, so the FILL-BAR stamp is "
        "the bar-close series shifted twice. Entry ADX uses the SAME two shifts, "
        "so both features in the joint table sit on one look-ahead rule. ADX "
        "warm-up bars are masked to NaN — `compute_regime` fills them with 0.0, "
        "which would otherwise file every warm-up bar under \"low ADX\".")
    out.append("")

    out.append("## Coverage and effective sample size")
    out.append("")
    cov = run.get("coverage") or {}
    out.append(
        f"{cov.get('n_kept', 0)} of {cov.get('n_cells', 0)} (dataset, window) "
        f"cells carried enough history to score: "
        f"{cov.get('required_lead_bars', 0)} bars of lead before the window start, "
        f"and at least "
        f"{float(cov.get('min_window_bar_fraction') or 0) * 100:.0f}% of the bars a "
        f"complete cache would hold inside the window. That density floor is what "
        f"catches a delisting gap — a year present as a few hundred bars is not a "
        f"year. An open-ended window is closed at ONE run-level reference bar "
        f"(`{cov.get('reference_last_bar') or '-'}`, the latest bar any dataset "
        f"reached), never at each dataset's own last bar — otherwise a dataset "
        f"that stopped early would define its own denominator and always score "
        f"100% dense. {cov.get('n_dropped', 0)} cells were DROPPED, listed below; "
        f"nothing is silently scored on partial data.")
    out.append("")
    dropped = cov.get("dropped") or []
    if dropped:
        out.append("| Dataset | Window | Why dropped |")
        out.append("|---------|--------|-------------|")
        for d in dropped:
            out.append(f"| `{d['dataset']}` | `{d['window']}` | {d['reason']} |")
        out.append("")
    else:
        out.append("No cells were dropped.")
        out.append("")

    out.append(
        "Effective N is `N^2 / sum_ij rho_ij`, with `rho_ij` the symbol-level "
        "daily-return correlation when two trades' holding periods OVERLAP and 0 "
        "when they do not; same-symbol pairs are the same tape and take 1.0, and "
        "correlations are clipped to `[0, 1]` so anti-correlation can never "
        "manufacture power. It is printed beside nominal N in every table that "
        "feeds a p-value, and the volume floors are applied to it — a pool of "
        "correlated rows cannot buy its way past them.")
    out.append("")
    ex_names = sorted({d for c in cfgs
                       for d in (c.get("cluster_excluded_datasets") or [])})
    ex_rows = max((int(c.get("cluster_excluded_trades") or 0) for c in cfgs),
                  default=0)
    if ex_names:
        out.append(
            f"Datasets too short to host a calendar rotation, and therefore "
            f"dropped from the contrast, the counts and the effective-N floors "
            f"alike (up to {ex_rows} rows on a single config): "
            + ", ".join(f"`{d}`" for d in ex_names) + ".")
    else:
        out.append(
            "Every dataset spans enough calendar time to host a rotation, so no "
            "rows were dropped from any cluster contrast.")
    out.append("")

    rho = run.get("symbol_correlations") or {}
    if rho:
        syms = sorted({s for pair in rho for s in pair.split("|")})
        out.append("Symbol daily-return correlation matrix:")
        out.append("")
        out.append("| | " + " | ".join(f"`{s}`" for s in syms) + " |")
        out.append("|---|" + "---|" * len(syms))
        for a in syms:
            cells = []
            for b in syms:
                if a == b:
                    cells.append("1.00")
                else:
                    v = rho.get(f"{a}|{b}", rho.get(f"{b}|{a}"))
                    cells.append(_fmt(v))
            out.append(f"| `{a}` | " + " | ".join(cells) + " |")
        out.append("")

    out.append("## Measured detection limit")
    out.append("")
    out.append(
        "The smallest per-trade edge (percentage points of net return) each "
        "design could have detected, under the cluster null at the rank-1 "
        "Benjamini-Hochberg threshold. This is the number that turns "
        "\"inconclusive\" from a mystery into a bound.")
    out.append("")
    by_pool = mde.get("observed_separation_pp_by_pool") or {}
    out.append("| Pool | Cluster MDE (pp/trade) | Free MDE (pp/trade) | "
               "Largest separation ON THAT POOL (pp/trade) | Resolvable? | "
               "Cluster p at zero injection | Free p at zero injection |")
    out.append("|------|-----------------------:|--------------------:|"
               "------------------------------------------:|:-----------:|"
               "----------------------------:|-------------------------:|")
    for key, label in (("1410", "#1410 design (its 30-hypothesis grid)"),
                       ("primary", "this study, primary cohort"),
                       ("exploratory", "this study, exploratory grid")):
        c = mde.get(f"pooled_{key}_cluster")
        f = mde.get(f"pooled_{key}_free")
        seps = by_pool.get(key) or {}
        largest = (max(abs(float(v)) for v in seps.values()) if seps else None)
        if largest is None or c is None:
            resolvable = "-"
        else:
            resolvable = "yes" if largest >= c else "NO"
        out.append(f"| {label} | "
                   f"{'> ' + f'{MDE_GRID_MAX:g}' if c is None else _fmt(c)} | "
                   f"{'> ' + f'{MDE_GRID_MAX:g}' if f is None else _fmt(f)} | "
                   f"{_fmt(largest)} | {resolvable} | "
                   f"{_fmt_p(mde.get(f'pooled_{key}_cluster_p0'))} | "
                   f"{_fmt_p(mde.get(f'pooled_{key}_free_p0'))} |")
    out.append("")
    out.append(
        "The \"p at zero injection\" columns are the SAME contrast with no "
        "injected edge: each family's suppressed side against its kept side — "
        "`H < 0.5` for a family that arms on high H, `H >= 0.5` for one that "
        "arms on low H. The separation column is that same contrast measured on "
        "the SAME rows as that pool's limit, so the two are comparable; a "
        "whole-study separation read against a sub-cohort's limit would not be. "
        "`Resolvable? = NO` means the separation sits under the limit and the "
        "design cannot see an effect that small — such a pool's null bounds the "
        "edge from above and says nothing about whether a smaller one exists.")
    out.append("")
    obs = mde.get("observed_separation_pp") or {}
    if obs:
        out.append(
            "Observed per-family bucket separations across ALL cohorts pooled "
            "(context for Part A; not the like-for-like comparison above):")
        out.append("")
        out.append("| Family | Hurst window | Observed separation (pp/trade) |")
        out.append("|--------|-------------:|-------------------------------:|")
        for key in sorted(obs):
            fam, hw = key.split("|")
            out.append(f"| `{fam}` | {hw} | {_fmt(obs[key])} |")
        out.append("")

    out.append("## Part A - outcomes bucketed by H at entry")
    out.append("")
    out.append(
        "Ungated legs only, pooled per family across datasets and windows and "
        "deduplicated on `(strategy, symbol, timeframe, entry_date)`. Drawdown "
        "here is TRADE-GRANULAR (the compounded trade sequence), not the "
        "bar-level engine drawdown used in Part B.")
    out.append("")
    out.append(study1410.render_nan_bucket_note(run.get("warmup")))
    out.append("")
    for family in FAMILIES:
        out.append(f"### {family}")
        out.append("")
        for hw in hurst_windows:
            out.append(f"**Hurst window {hw} bars**")
            out.append("")
            out.extend(_render_bucket_table(
                (payload["buckets"].get(family) or {}).get(str(hw)) or {}))

    out.append("## Part B / C - primary hypotheses")
    out.append("")
    out.append(
        "`gate` rows are real Backtester re-runs with entry signals masked while "
        "the gate is disarmed (closes never masked); their drawdowns are "
        "bar-level. `size` rows re-compound the same ungated trade sequence with "
        "the size multiplier; their drawdowns are trade-granular. Never compare a "
        "`gate` drawdown to a `size` drawdown. `dd` and `chop` are MAGNITUDE "
        "deltas (arm minus ungated) averaged over that window's legs — negative "
        "means improvement. The verdict reads the CLUSTER p.")
    out.append("")
    primary = [c for c in cfgs if c["cohort"] == COHORT_PRIMARY]
    out.extend(_render_config_table(primary, PRIMARY_PROTOCOL_WINDOWS))

    out.append("## Part B / C - exploratory grid (#1410's configs, expanded data)")
    out.append("")
    out.append(
        "Reported for completeness under its OWN Benjamini-Hochberg correction. "
        "These hypotheses were selected and scored on overlapping evidence, so "
        "nothing here can produce a recommendation.")
    out.append("")
    exploratory = [c for c in cfgs if c["cohort"] == COHORT_EXPLORATORY]
    out.extend(_render_config_table(exploratory, EXPLORATORY_PROTOCOL_WINDOWS))

    out.append("## Part D - joint ADX x Hurst buckets (#1412 Stage 0)")
    out.append("")
    out.append(
        "The question #1412's Stage 0 asks: do joint buckets separate beyond "
        "what either metric alone predicts — specifically, is high-ADX + "
        "low-Hurst (a strong move on anti-persistent tape) materially worse for "
        "momentum-style entries than high-ADX alone? Material separation "
        "requires BOTH a Bonferroni-corrected significant cluster p AND an "
        "effect at least as large as the measured detection limit — and that "
        "limit is measured on THIS contrast's own rows, at the same Bonferroni "
        "bar, so the size an effect must reach is set by the evidence the test "
        "actually has.")
    out.append("")
    joint = payload.get("joint") or {}
    for family in FAMILIES:
        entry = joint.get(family) or {}
        out.append(f"### {family}")
        out.append("")
        if not entry:
            out.append("No trades pooled for this family.")
            out.append("")
            continue
        out.extend(_render_joint_table(entry.get("table") or {}))
        v = entry.get("verdict") or {}
        if v.get("separated"):
            out.append(
                f"**Separation found.** Within `ADX >= {ADX_SPLIT:g}`, "
                f"`H < 0.45` entries underperform `H >= 0.45` entries by "
                f"{_fmt(v.get('delta_mean_pp'))} pp per trade "
                f"(cluster p={_fmt_p(v.get('p_cluster'))}, Bonferroni bar "
                f"{JOINT_ALPHA:g}; detection limit "
                f"{_fmt(v.get('mde_pp'))} pp).")
        else:
            out.append(f"**{NO_JOINT_SEPARATION}** — {v.get('reason') or 'no contrast'}.")
        out.append("")

    all_sep = [(joint.get(f) or {}).get("verdict", {}).get("separated")
               for f in FAMILIES]
    if any(all_sep):
        out.append(
            "At least one family separates on the joint buckets, so #1412's "
            "Stage 0 gate is satisfied for that family and Stage 1 may proceed "
            "on this evidence.")
    else:
        out.append(
            f"`{NO_JOINT_SEPARATION}` on every family. #1412's Stage 0 gate says "
            f"to write this verdict, STOP, and record that full fusion of Hurst "
            f"into the composite classifier is not justified — the standalone "
            f"`hurst_gate` (#1411), which ships default-off, remains the correct "
            f"amount of Hurst.")
    out.append("")

    out.append("## Acceptance rule")
    out.append("")
    out.append("A primary config wins for its family only when ALL of the following hold:")
    out.append("")
    out.append(f"1. Effective suppressed trades >= {MIN_SUPPRESSED_EFFECTIVE:g} and "
               f"effective kept trades >= {MIN_KEPT_EFFECTIVE:g}.")
    out.append(f"2. Significant after Benjamini-Hochberg at alpha={ALPHA} ON THE "
               f"CLUSTER p, within the primary cohort's own denominator.")
    out.append("3. On BOTH primary protocol windows: mean drawdown magnitude falls, "
               "chop loss falls, and the return give-up stays within "
               f"max({RETURN_TOLERANCE_PP:g} pp, "
               f"{RETURN_TOLERANCE_FRAC:g} x |ungated return|).")
    out.append("4. Drawdown does not degrade on at least 2/3 of the held-out windows "
               f"that carry legs, with at least {HELD_OUT_MIN_WINDOWS} such windows.")
    out.append("")
    out.append(
        "Rules 1 and 2 are what separate a real edge from arithmetic. ANY gate "
        "that removes trades lowers drawdown and chop loss on a losing book, so "
        "those columns alone prove nothing — that is exactly what produced "
        "#1410's 16 economics-only configs. The cluster permutation asks the "
        "question a gate must answer, with correlated concurrent trades moving "
        "together under the null: are the trades this gate SUPPRESSES worse than "
        "the ones it KEEPS, beyond what a calendar rotation of the same labels "
        "would produce? And effective N stops a pool of correlated rows from "
        "clearing a volume floor it has no independent information to clear.")
    out.append("")

    out.append("## Run summary")
    out.append("")
    out.append(f"- Legs scored: {run['legs']} ungated + {run['gated_arms']} gated arms.")
    out.append(f"- Harness identity: {run['mirror_verified_legs']} of {run['legs']} "
               f"ungated legs reproduced `eval_windows.run_leg` exactly.")
    out.append("- Pooled deduplicated trades: " + "; ".join(
        f"{f} {run['pooled_trades'][f]} "
        f"(primary {run['pooled_primary'][f]}, exploratory "
        f"{run['pooled_exploratory'][f]})" for f in FAMILIES) + ".")
    out.append(f"- Hypotheses: {run['n_primary_configs']} primary, "
               f"{run['n_exploratory_configs']} exploratory; "
               f"Benjamini-Hochberg-significant: {run['n_primary_significant']} "
               f"primary, {run['n_exploratory_significant']} exploratory.")
    warm = run.get("warmup") or {}
    out.append(f"- Warm-up lead before the earliest scored window start: min "
               f"{warm.get('min_lead_bars', 0)} bars, required "
               f"{warm.get('required_bars', 0)} — "
               f"{'sufficient on every dataset' if warm.get('sufficient') else 'SHORT on ' + ', '.join(warm.get('insufficient_datasets') or [])}.")
    out.append(f"- Wall time: {run['elapsed_sec']} s.")
    out.append("")

    out.append(render_recommendation(decision, mde))
    return "\n".join(out).rstrip() + "\n"


def report_from_payload(payload: dict) -> str:
    """Render straight from a committed JSON (the ``--render-only`` path)."""
    return render_report(payload)


# ---------------------------------------------------------------------------
# Data acquisition.
# ---------------------------------------------------------------------------

def ensure_min_history(datasets: Sequence[tuple], since: str = HISTORY_SINCE,
                       platform: str = PLATFORM) -> dict:
    """Backfill each dataset to ``since``.

    ``load_cached_data`` fetches ONLY when the cached slice comes back empty,
    so a 2020 request against a 2023-start cache silently returns 2023+ rows and
    never backfills. This calls ``fetch_full_history(..., store=True)``
    explicitly; ``store_ohlcv`` upserts, so re-running is a no-op. A venue that
    never listed a pair fails here and the coverage audit drops its cells — one
    missing symbol never aborts the study.
    """
    from data_fetcher import fetch_full_history
    report = {}
    for symbol, timeframe in datasets:
        key = dataset_key(symbol, timeframe)
        try:
            df = fetch_full_history(symbol, timeframe, since=since,
                                    exchange_id=platform, store=True)
            report[key] = {"ok": True, "bars": int(len(df))}
        except Exception as exc:  # pragma: no cover - network dependent
            report[key] = {"ok": False, "error": f"{type(exc).__name__}: {exc}"}
            print(f"[1422] backfill FAILED for {key}: {exc}", flush=True)
            traceback.print_exc()
    return report


def _parse_datasets(raw: Optional[str]) -> list:
    if not raw:
        return list(DATASETS)
    from eval_windows import parse_dataset_arg
    out = []
    for token in raw.split(","):
        token = token.strip()
        if token:
            out.append(parse_dataset_arg(token))
    return out


def _parse_windows(raw: Optional[str]) -> list:
    if not raw:
        return list(WINDOW_ORDER)
    names = [t.strip() for t in raw.split(",") if t.strip()]
    for name in names:
        if name not in WINDOWS:
            raise SystemExit(f"unknown window {name!r}; known: {sorted(WINDOWS)}")
    return [w for w in WINDOW_ORDER if w in names]


def resolve_primary_config_ids(json_path: str) -> tuple:
    """The per-(family, mode) smallest-raw-p configs in the committed #1410 JSON.

    Returned for the assertion against ``PRIMARY_CONFIG_IDS``: the pinned tuple
    is what the pre-registration promises, and this is what the data actually
    says. A regenerated #1410 JSON that moves the argmin fails loud instead of
    silently swapping the primary set.
    """
    with open(json_path) as fh:
        payload = json.load(fh)
    best = {}
    for cfg in payload.get("configs") or []:
        if cfg.get("p_raw") is None:
            continue
        key = (cfg["family"], cfg["mode"])
        cur = best.get(key)
        cand = (float(cfg["p_raw"]), str(cfg["config_id"]))
        if cur is None or cand < cur[0]:
            best[key] = (cand, cfg["config_id"])
    return tuple(sorted(v[1] for v in best.values()))


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
    p.add_argument("--n-perm-mde", type=int, default=N_PERM_MDE)
    p.add_argument("--seed", type=int, default=SEED)
    p.add_argument("--json-out", default=_DEFAULT_JSON_OUT)
    p.add_argument("--report-out", default=_DEFAULT_REPORT_OUT)
    p.add_argument("--write-report", action="store_true",
                   help="render the Markdown report at the contract path")
    p.add_argument("--no-mirror-check", action="store_true",
                   help="skip the per-leg eval_windows.run_leg identity check")
    p.add_argument("--skip-fetch", action="store_true",
                   help="run on the cache as-is; the coverage audit decides "
                        "which cells exist")
    p.add_argument("--fetch-only", action="store_true",
                   help="backfill history and exit")
    p.add_argument("--render-only", action="store_true",
                   help="re-render the report from an existing --json-out; "
                        "runs no backtests")
    args = p.parse_args(argv)

    # #1424: this study is superseded and may never write the live-evidence
    # contract path again — not from a complete run, not from --render-only,
    # not from an explicit --report-out. Changing the default alone would leave
    # `--report-out .../hurst_gate_calibration.md` as a one-flag revert of the
    # live evidence to a study a better-powered successor replaced.
    if (os.path.abspath(args.report_out)
            == os.path.abspath(os.path.join(_THIS_DIR,
                                            "hurst_gate_calibration.md"))):
        raise SystemExit(
            "[1422] this study is SUPERSEDED and may not write the "
            "live-evidence contract path hurst_gate_calibration.md; "
            "hurst_1424_gate_resolution.py owns it. Its own render belongs at "
            f"{_DEFAULT_REPORT_OUT}.")

    # The committed JSON and the contract report describe the PRE-REGISTERED
    # design. A scoped debug run produces a different study, so it may not
    # occupy either path — the same hazard this PR closed for #1410's report
    # default, approached from the JSON side.
    scope = {
        "only": args.only,
        "datasets": args.datasets,
        "windows": args.windows,
        "hurst_windows": args.hurst_windows,
    }
    scope["complete"] = not any(v for v in scope.values())
    if not scope["complete"]:
        narrowed = ", ".join(f"--{k.replace('_', '-')} {v}"
                             for k, v in scope.items() if k != "complete" and v)
        if os.path.abspath(args.json_out) == os.path.abspath(_DEFAULT_JSON_OUT):
            raise SystemExit(
                f"[1422] refusing to overwrite the committed aggregate "
                f"{_DEFAULT_JSON_OUT} from a scoped run ({narrowed}). Pass an "
                f"explicit --json-out.")
        if os.path.abspath(args.report_out) == os.path.abspath(_DEFAULT_REPORT_OUT):
            raise SystemExit(
                f"[1422] refusing to target the live-evidence contract path "
                f"{_DEFAULT_REPORT_OUT} from a scoped run ({narrowed}). Pass an "
                f"explicit --report-out.")

    if args.render_only:
        with open(args.json_out) as fh:
            payload = json.load(fh)
        is_contract = (os.path.abspath(args.report_out)
                       == os.path.abspath(_DEFAULT_REPORT_OUT))
        if is_contract:
            # Fail closed: a payload with no scope stamp predates this guard and
            # cannot prove it came from a complete run.
            stamped = ((payload.get("run_summary") or {}).get("scope")
                       or {}).get("complete")
            if not stamped:
                raise SystemExit(
                    f"[1422] {args.json_out} is not stamped as a complete run, "
                    f"so it may not be rendered to the contract path "
                    f"{_DEFAULT_REPORT_OUT}.")
            if not args.write_report:
                raise SystemExit(
                    "[1422] writing the contract report needs --write-report, "
                    "on --render-only exactly as on a scoring run.")
        report = report_from_payload(payload)
        with open(args.report_out, "w") as fh:
            fh.write(report)
        print(f"[1422] re-rendered {args.report_out} from {args.json_out}")
        return 0

    datasets = _parse_datasets(args.datasets)
    if args.fetch_only:
        ensure_min_history(datasets)
        print("[1422] backfill complete")
        return 0

    families = FAMILIES
    if args.only:
        wanted = [t.strip() for t in args.only.split(",") if t.strip()]
        for f in wanted:
            if f not in FAMILIES:
                raise SystemExit(f"unknown family {f!r}; known: {list(FAMILIES)}")
        families = tuple(f for f in FAMILIES if f in wanted)
    window_names = _parse_windows(args.windows)
    hurst_windows = (tuple(int(t) for t in args.hurst_windows.split(","))
                     if args.hurst_windows else HURST_WINDOWS)

    # The primary set is pre-registered; verify the committed #1410 evidence
    # still yields exactly it before anything is scored.
    json_1410 = os.path.join(_THIS_DIR, "hurst_1410_gate_calibration.json")
    resolved = resolve_primary_config_ids(json_1410)
    if resolved != tuple(sorted(PRIMARY_CONFIG_IDS)):
        raise SystemExit(
            f"pre-registered primary set {sorted(PRIMARY_CONFIG_IDS)} no longer "
            f"matches the committed #1410 argmin {list(resolved)}. Re-register "
            f"deliberately; never let it drift.")

    started = time.time()
    backfill = {}
    if not args.skip_fetch:
        print(f"[1422] backfilling {len(datasets)} datasets to {HISTORY_SINCE}...")
        backfill = ensure_min_history(datasets)

    from data_fetcher import load_cached_data
    from registry_loader import load_registry
    reg = load_registry("spot")

    print(f"[1422] loading {len(datasets)} datasets from the {PLATFORM} cache...")
    frames = {}
    for symbol, timeframe in datasets:
        try:
            frames[(symbol, timeframe)] = load_cached_data(
                symbol, timeframe, exchange_id=PLATFORM)
        except Exception as exc:  # pragma: no cover - network dependent
            print(f"[1422] load FAILED for {dataset_key(symbol, timeframe)}: {exc}")
            frames[(symbol, timeframe)] = pd.DataFrame()

    coverage = coverage_audit(frames, window_names, hurst_windows)
    print(f"[1422] coverage: {coverage['n_kept']}/{coverage['n_cells']} cells kept, "
          f"{coverage['n_dropped']} dropped")
    for d in coverage["dropped"]:
        print(f"[1422]   dropped {d['dataset']} {d['window']}: {d['reason']}")

    usable_datasets = [ds for ds in datasets
                       if any(coverage["cells"].get(
                           f"{dataset_key(*ds)}|{w}") for w in window_names)]
    if not usable_datasets:
        raise SystemExit("[1422] no dataset carries a scoreable cell; nothing to do")

    scored_windows = [w for w in window_names
                      if any(coverage["cells"].get(f"{dataset_key(*ds)}|{w}")
                             for ds in usable_datasets)]
    first_needed = min(pd.Timestamp(WINDOWS[w][0]) for w in scored_windows)

    warmup = warmup_audit(
        scored_warmup_leads(frames, coverage, scored_windows), hurst_windows)
    if not warmup["sufficient"]:
        print(f"[1422] WARNING: warm-up shortfall on "
              f"{len(warmup['insufficient_datasets'])} dataset(s): "
              f"{', '.join(warmup['insufficient_datasets'])}. H is UNDEFINED on "
              f"their first scored bars, so the NaN bucket carries real trades. "
              f"NaN stays its own bucket (never 0.5) and holds the gate state.")
    else:
        print(f"[1422] warm-up OK: min lead {warmup['min_lead_bars']} bars before "
              f"each dataset's own earliest scored window "
              f"(need {warmup['required_bars']}).")

    print(f"[1422] computing rolling Hurst for {len(usable_datasets)}x"
          f"{len(hurst_windows)} (dataset, window) pairs...")
    hurst: dict = {}
    cache_path = None
    if args.out_dir:
        os.makedirs(args.out_dir, exist_ok=True)
        cache_path = os.path.join(args.out_dir, "hurst_1422_rolling.npz")
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

    jobs = [(ds, hw) for ds in usable_datasets for hw in hurst_windows]
    with ThreadPoolExecutor(max_workers=max(1, args.jobs)) as pool:
        for job, series in pool.map(_hurst_job, jobs):
            hurst[job] = series
    if cache_path:
        arrays = {}
        for ds, hw in jobs:
            key = f"{ds[0]}|{ds[1]}|{hw}"
            arrays[key] = hurst[(ds, hw)].to_numpy(dtype=float)
            arrays[f"meta|{key}"] = cache_meta(frames[ds].index, first_needed)
        np.savez_compressed(cache_path, **arrays)

    print(f"[1422] computing entry-ADX stamps for {len(usable_datasets)} datasets...")
    adx_stamps = {ds: adx_entry_stamp(frames[ds]) for ds in usable_datasets}

    print("[1422] computing symbol daily-return correlations...")
    rho_by_symbol = symbol_return_correlations(
        {ds: frames[ds] for ds in usable_datasets})

    units = [(family, exemplar, symbol, timeframe, wname)
             for family in families
             for exemplar in FAMILY_EXEMPLARS[family]
             for (symbol, timeframe) in usable_datasets
             for wname in scored_windows
             if coverage["cells"].get(f"{dataset_key(symbol, timeframe)}|{wname}")]
    print(f"[1422] scoring {len(units)} legs "
          f"({len(hurst_windows) * 3} gated arms each)...")

    def _leg_job(unit):
        family, exemplar, symbol, timeframe, wname = unit
        by_window = {hw: hurst[((symbol, timeframe), hw)] for hw in hurst_windows}
        return build_leg(reg, family, exemplar, symbol, timeframe, wname,
                         frames[(symbol, timeframe)], by_window,
                         adx_stamps[(symbol, timeframe)],
                         verify_mirror=not args.no_mirror_check)

    with ThreadPoolExecutor(max_workers=max(1, args.jobs)) as pool:
        legs = [lg for lg in pool.map(_leg_job, units) if lg is not None]
    legs.sort(key=lambda lg: (lg["family"], lg["strategy"], lg["dataset"], lg["window"]))

    pooled = {}
    raw_counts = {}
    for family in families:
        rows = [t for lg in legs if lg["family"] == family for t in lg["trades"]]
        raw_counts[family] = len(rows)
        pooled[family] = dedup_entries(rows, WINDOW_ORDER)
    for family in FAMILIES:
        pooled.setdefault(family, [])
        raw_counts.setdefault(family, 0)

    # Structural guarantee, not a promise: no primary trade may come from a
    # cell #1410 scored. Without this the primary hypotheses — chosen by
    # reading #1410's p-values — would be scored on their own selection data.
    for family in FAMILIES:
        for t in pooled[family]:
            if t["cohort"] != COHORT_PRIMARY:
                continue
            key = (dataset_key(t["symbol"], t["timeframe"]), t["window"])
            if key in D_1410:
                raise AssertionError(
                    f"primary cohort leaked a #1410 cell: {key}")

    print("[1422] sweeping configs and running both nulls...")
    configs = build_configs(legs, pooled, hurst_windows, rho_by_symbol,
                            args.n_perm, args.seed)
    configs = [c for c in configs if c["family"] in families]
    apply_bh_by_cohort(configs, alpha=ALPHA)

    print("[1422] measuring detection limits...")
    mde = _measure_detection_limits(pooled, hurst_windows, json_1410,
                                    args.n_perm_mde, args.seed)

    decision = decide_recommendation(configs, mde)

    buckets = {family: {str(hw): bucket_tables(pooled[family], hw)
                        for hw in hurst_windows}
               for family in FAMILIES}

    joint_hw = max(hurst_windows)
    joint = {}
    for family in FAMILIES:
        joint[family] = {
            "table": joint_adx_hurst_table(pooled[family], joint_hw),
            "verdict": joint_separation_verdict(
                pooled[family], joint_hw, n_perm=args.n_perm,
                seed=args.seed),
        }

    n_primary = sum(1 for c in configs if c["cohort"] == COHORT_PRIMARY)
    n_expl = sum(1 for c in configs if c["cohort"] == COHORT_EXPLORATORY)
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
            "primary_config_ids": list(PRIMARY_CONFIG_IDS),
            "min_suppressed_effective": MIN_SUPPRESSED_EFFECTIVE,
            "min_kept_effective": MIN_KEPT_EFFECTIVE,
            "return_tolerance_pp": RETURN_TOLERANCE_PP,
            "return_tolerance_frac": RETURN_TOLERANCE_FRAC,
            "held_out_min_fraction": HELD_OUT_MIN_FRACTION,
            "held_out_min_windows": HELD_OUT_MIN_WINDOWS,
            "alpha": ALPHA,
            "n_perm": args.n_perm,
            "n_perm_mde": args.n_perm_mde,
            "seed": args.seed,
            "min_offset_days": MIN_OFFSET_DAYS,
            "adx_period": ADX_PERIOD,
            "adx_split": ADX_SPLIT,
            "history_since": HISTORY_SINCE,
            "windows": {k: list(WINDOWS[k]) for k in scored_windows},
            "primary_protocol_windows": list(PRIMARY_PROTOCOL_WINDOWS),
            "primary_held_out_windows": list(PRIMARY_HELD_OUT_WINDOWS),
            "exploratory_protocol_windows": list(EXPLORATORY_PROTOCOL_WINDOWS),
            "exploratory_held_out_windows": list(EXPLORATORY_HELD_OUT_WINDOWS),
            "datasets": [dataset_key(s, t) for s, t in usable_datasets],
            "platform": PLATFORM,
            "fee_platform": FEE_PLATFORM,
            "capital": DEFAULT_CAPITAL,
        },
        "run_summary": {
            "scope": scope,
            "legs": len(legs),
            "gated_arms": sum(len(lg["gated"]) for lg in legs),
            "mirror_verified_legs": sum(1 for lg in legs if lg["mirror_verified"]),
            "raw_trades": raw_counts,
            "pooled_trades": {f: len(pooled[f]) for f in FAMILIES},
            "pooled_primary": {
                f: sum(1 for t in pooled[f] if t["cohort"] == COHORT_PRIMARY)
                for f in FAMILIES},
            "pooled_exploratory": {
                f: sum(1 for t in pooled[f] if t["cohort"] == COHORT_EXPLORATORY)
                for f in FAMILIES},
            "n_primary_configs": n_primary,
            "n_exploratory_configs": n_expl,
            "n_primary_significant": sum(
                1 for c in configs
                if c["cohort"] == COHORT_PRIMARY and c.get("bh_reject")),
            "n_exploratory_significant": sum(
                1 for c in configs
                if c["cohort"] == COHORT_EXPLORATORY and c.get("bh_reject")),
            "warmup": warmup,
            "coverage": coverage,
            "backfill": backfill,
            "symbol_correlations": {f"{a}|{b}": v
                                    for (a, b), v in sorted(rho_by_symbol.items())},
            "elapsed_sec": round(time.time() - started, 2),
        },
        "mde": mde,
        "buckets": buckets,
        "joint": joint,
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
    print(f"[1422] wrote {args.json_out}")

    payload_for_report = dict(payload)
    payload_for_report["decision"] = decision
    report = render_report(payload_for_report)
    if args.write_report:
        with open(args.report_out, "w") as fh:
            fh.write(report)
        print(f"[1422] wrote {args.report_out}")
    else:
        print(render_recommendation(decision, mde))
    return 0


def _separation(values: Sequence[float],
                suppressed: Sequence[bool]) -> Optional[float]:
    """Mean(kept) - mean(suppressed), in pp per trade, or None if one-sided.

    Returned in the same orientation as the injected edge in
    `min_detectable_effect`, so a separation and a detection limit measured on
    the same rows are directly comparable.
    """
    if not values or all(suppressed) or not any(suppressed):
        return None
    v = np.asarray(values, dtype=float)
    m = np.asarray(suppressed, dtype=bool)
    return round(float(v[~m].mean() - v[m].mean()), 6)


def _measure_detection_limits(pooled: dict, hurst_windows: Sequence[int],
                              json_1410_path: str, n_perm: int, seed: int) -> dict:
    """Stage A: what each design could have detected.

    Three pools, all under the same injection model and the same rank-1
    Benjamini-Hochberg bar: the #1410 design (its cells, its 30-hypothesis
    denominator), this study's primary cohort (4 hypotheses), and this study's
    exploratory grid (30). Reported beside the separations the buckets actually
    show, so a reader can see whether the null means "no edge" or "no power".
    """
    out: dict = {"by_family_cluster": {}}
    hw = max(hurst_windows)

    def _pool(family: str, cohort: Optional[str], only_1410_cells: bool):
        rows = []
        for t in pooled.get(family) or []:
            if cohort is not None and t["cohort"] != cohort:
                continue
            key = (dataset_key(t["symbol"], t["timeframe"]), t["window"])
            if only_1410_cells and key not in D_1410:
                continue
            rows.append(t)
        return rows

    def _split(rows, family: str):
        # The suppressed side is the FAMILY'S OWN anti-signal bucket, so the
        # injected edge sits exactly where that family's gate claims one lives.
        # A single `h < 0.5` split for both would run the injection backwards
        # for `arms_on_low_h`, and the two families would partly cancel in the
        # pooled row set.
        sense = FAMILY_SENSE[family]
        values, mask, keep = [], [], []
        for t in rows:
            h = (t.get("h") or {}).get(hw)
            if h is None or not math.isfinite(float(h)):
                continue
            keep.append(t)
            values.append(float(t["pnl_pct_net"]))
            mask.append(anti_signal_side(float(h), sense))
        return keep, values, mask

    specs = (
        ("1410", None, True, 30),
        ("primary", COHORT_PRIMARY, False, len(PRIMARY_CONFIG_IDS)),
        ("exploratory", COHORT_EXPLORATORY, False, 30),
    )
    by_pool: dict = {}
    for label, cohort, only_1410, family_size in specs:
        rows_all, vals_all, mask_all, families_all = [], [], [], []
        for family in FAMILIES:
            rows, vals, mask = _split(_pool(family, cohort, only_1410), family)
            rows_all += rows
            vals_all += vals
            mask_all += mask
            families_all += [family] * len(rows)
            if label == "primary":
                # A family-specific limit has its own cluster-usable subset;
                # filter before measuring it so no caller can accidentally
                # pair that limit with a contrast that retained dropped rows.
                fam_idx, _ = usable_cluster_rows(rows)
                fam_rows = [rows[i] for i in fam_idx]
                fam_vals = [vals[i] for i in fam_idx]
                fam_mask = [mask[i] for i in fam_idx]
                fam_mde = min_detectable_effect(
                    fam_rows, fam_vals, fam_mask, family_size, cluster=True,
                    n_perm=n_perm, seed=seed)
                out["by_family_cluster"][family] = fam_mde

        # The cluster null defines this pool's usable sample. Apply that sample
        # ONCE to every number printed beside it: the family separations, both
        # detection limits, and both zero-injection p-values. Letting the free
        # columns or observed contrast retain rows the cluster null discarded
        # would reintroduce the mismatched-sample inference this table exists
        # to prevent.
        idx, _ = usable_cluster_rows(rows_all)
        rows_all = [rows_all[i] for i in idx]
        vals_all = [vals_all[i] for i in idx]
        mask_all = [mask_all[i] for i in idx]
        families_all = [families_all[i] for i in idx]

        pool_obs: dict = {}
        for family in FAMILIES:
            own = [i for i, value in enumerate(families_all)
                   if value == family]
            sep = _separation([vals_all[i] for i in own],
                              [mask_all[i] for i in own])
            if sep is not None:
                pool_obs[f"{family}|{hw}"] = sep
        by_pool[label] = pool_obs
        out[f"pooled_{label}_cluster"] = min_detectable_effect(
            rows_all, vals_all, mask_all, family_size, cluster=True,
            n_perm=n_perm, seed=seed)
        out[f"pooled_{label}_free"] = min_detectable_effect(
            rows_all, vals_all, mask_all, family_size, cluster=False,
            n_perm=n_perm, seed=seed)
        # The UNINJECTED p of the same contrast. Without it a reader cannot tell
        # whether a detection limit below the observed separation means "the
        # split is significant" or "the injection moved the null too" — and the
        # Recommendation leans on exactly that distinction.
        if rows_all and 0 < int(np.sum(mask_all)) < len(mask_all):
            out[f"pooled_{label}_cluster_p0"] = (
                cluster_permutation_pvalue_group_diff(
                    rows_all, vals_all, mask_all, n_perm=n_perm, seed=seed)
                .get("p"))
            out[f"pooled_{label}_free_p0"] = permutation_pvalue_group_diff(
                vals_all, mask_all, n_perm=n_perm, seed=seed)
        else:
            out[f"pooled_{label}_cluster_p0"] = None
            out[f"pooled_{label}_free_p0"] = None

    obs = {}
    for family in FAMILIES:
        rows, vals, mask = _split(_pool(family, None, False), family)
        idx, _ = usable_cluster_rows(rows)
        vals = [vals[i] for i in idx]
        mask = [mask[i] for i in idx]
        sep = _separation(vals, mask)
        if sep is not None:
            obs[f"{family}|{hw}"] = sep
    out["observed_separation_pp"] = obs
    out["observed_separation_pp_by_pool"] = by_pool
    out["json_1410"] = os.path.basename(json_1410_path)
    return out


if __name__ == "__main__":
    raise SystemExit(main())
