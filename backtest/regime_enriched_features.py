"""Enriched feature matrix for the #1095 unsupervised vol-regime bake-off.

The #1080 bake-off fit every candidate on exactly the hand-rule's own four inputs
(``return_eff, range_eff, efficiency, adx`` from ``shared_tools/regime.composite_feature_matrix``).
Clustering can only discover structure present in its inputs, so fitting on the rule's own
features leaves little room to out-separate it. This module builds an ENRICHED matrix — the four
canonical features (kept FIRST, in their canonical order) plus signals the hand-rule ignores:

  * ``funding_rate``    — point-in-time Hyperliquid funding snapshot (``funding_fetcher``, #960
                          backward ``merge_asof``; a future snapshot never reaches a past bar).
  * ``volume_z``        — trailing rolling z-score of bar volume (turnover surprise vs the last
                          ``vol_window`` bars, inclusive of the current bar — same causal window
                          convention as the canonical features).
  * ``htf_range_eff``   — a coarser-timeframe ``range_eff`` aligned so a base bar only ever sees a
                          higher-timeframe bar that has already CLOSED at or before the base bar's
                          open (strictly causal; conservatively staler than the canonical row,
                          never leaking).

Offline / research only. Live ``check_regime.py`` still builds only the canonical four-column
matrix, so an enriched model is NOT drop-in for the live classifier — that delta is recorded for
#1074 (see ``LIVE_WIRING_DELTA``), not solved here.

DESIGN INVARIANT — canonical-first column order. ``map_latent_to_names`` names each state from the
four canonical columns only; the extra signals shape cluster geometry, not the 7-label vocabulary.
Keeping the canonical block first means ``canonical_indices=(0, 1, 2, 3)`` holds for every subset.
``regime_hmm.forward_filter_labels`` is purely positional, so the SAME column order used at fit
must be used at decode — ``assert_feature_contract`` / ``decode_with_model`` enforce that.
"""
from __future__ import annotations
import os
import sys

_THIS_DIR = os.path.dirname(os.path.abspath(__file__))
for _p in (_THIS_DIR, os.path.abspath(os.path.join(_THIS_DIR, "..")),
           os.path.abspath(os.path.join(_THIS_DIR, "..", "shared_tools"))):
    if _p not in sys.path:
        sys.path.insert(0, _p)

import numpy as np
import pandas as pd

from regime import composite_feature_matrix, _DEFAULT_COMPOSITE_THRESHOLDS

# Canonical four, in the order composite_feature_matrix emits them. The naming path
# (map_composite_label) consumes these by position; they MUST stay first and in this order.
CANONICAL_COLUMNS = ["return_eff", "range_eff", "efficiency", "adx"]
# Extra signals the hand-rule does not use. Appended AFTER the canonical block.
ENRICHED_EXTRA_COLUMNS = ["funding_rate", "volume_z", "htf_range_eff"]
ENRICHED_COLUMNS = CANONICAL_COLUMNS + ENRICHED_EXTRA_COLUMNS
# Position of (return_eff, range_eff, efficiency, adx) inside any canonical-first matrix.
CANONICAL_INDICES = (0, 1, 2, 3)

# Recorded for #1074. An enriched model decodes from this column set on-cycle; live
# check_regime.py builds only CANONICAL_COLUMNS, so live wiring would need to feed the same
# enriched matrix (funding fetch + volume z-score + HTF resample) every cycle. Not solved here.
LIVE_WIRING_DELTA = (
    "Enriched models decode from ENRICHED_COLUMNS; live check_regime.py builds only "
    "CANONICAL_COLUMNS. Live wiring (#1074) must feed funding_rate + volume_z + htf_range_eff "
    "on-cycle in this exact order, or forward_filter_labels reads garbage."
)


def _infer_base_delta(index: pd.DatetimeIndex) -> pd.Timedelta:
    """Median spacing of the bar index — the base timeframe as a Timedelta."""
    if len(index) < 2:
        raise ValueError("need >= 2 bars to infer the base timeframe")
    diffs = pd.Series(index).diff().dropna()
    med = diffs.median()
    if not (pd.notna(med) and med > pd.Timedelta(0)):
        raise ValueError("could not infer a positive base timeframe from the index")
    return med


def _volume_z_column(df: pd.DataFrame, window: int) -> np.ndarray:
    """Trailing rolling z-score of volume over `window` bars, inclusive of the current bar.

    Causal: the window ends at bar i and bar i's own volume is known at its close — the same
    convention the canonical features use (their window i-period+1..i also includes bar i).
    Warmup (fewer than `window` observations) is NaN. Zero-variance windows -> 0.0 (no surprise).
    """
    vol = df["volume"].astype(float)
    roll = vol.rolling(window=window, min_periods=window)
    mean = roll.mean()
    std = roll.std(ddof=0)
    z = (vol - mean) / std
    z = z.where(std > 1e-12, 0.0)     # zero-variance window: defined surprise of 0.0, not inf/NaN
    z = z.where(mean.notna(), np.nan)  # warmup (< window observations) stays NaN
    return z.to_numpy(dtype=float)


def _htf_range_eff_column(df: pd.DataFrame, period: int, thresholds: dict,
                          htf_multiple: int) -> np.ndarray:
    """Higher-timeframe range_eff aligned causally onto the base index.

    Resample the base frame to `htf_multiple` x the base timeframe (OHLC + volume agg), compute
    the canonical composite range_eff on the coarse frame, then assign each base bar the most
    recent HTF bar that has already CLOSED at or before the base bar's OPEN time. A coarse bar
    labelled T_open closes at T_open + htf_delta; matching on close <= base_open guarantees the
    base bar never sees an in-progress HTF bar (no look-ahead). Warmup / no closed HTF bar -> NaN.
    """
    if htf_multiple < 2:
        raise ValueError("htf_multiple must be >= 2 (a strictly coarser timeframe)")
    if not isinstance(df.index, pd.DatetimeIndex):
        raise TypeError("htf_range_eff needs a DatetimeIndex (timestamped bars)")
    base_delta = _infer_base_delta(df.index)
    htf_delta = base_delta * htf_multiple
    agg = {"open": "first", "high": "max", "low": "min", "close": "last", "volume": "sum"}
    htf = df.resample(htf_delta, closed="left", label="left").agg(agg).dropna(subset=["close"])
    if len(htf) <= period:
        return np.full(len(df), np.nan, dtype=float)
    htf_feat = composite_feature_matrix(htf, period, thresholds)["range_eff"]
    # Re-key to the HTF bar's CLOSE time; only closed bars may inform a base bar.
    right = pd.DataFrame({
        "ts": (htf_feat.index + htf_delta),
        "htf_range_eff": htf_feat.to_numpy(dtype=float),
    }).dropna(subset=["htf_range_eff"]).sort_values("ts").reset_index(drop=True)
    if right.empty:
        return np.full(len(df), np.nan, dtype=float)
    left = pd.DataFrame({"ts": pd.DatetimeIndex(df.index)}).reset_index(drop=True)
    merged = pd.merge_asof(left, right, on="ts", direction="backward")
    return merged["htf_range_eff"].to_numpy(dtype=float)


def enriched_feature_matrix(df: pd.DataFrame, period: int, thresholds: dict | None = None, *,
                            funding: pd.DataFrame | None = None, vol_window: int | None = None,
                            htf_multiple: int = 4,
                            columns: list[str] | None = None) -> pd.DataFrame:
    """Per-bar enriched feature matrix (canonical four FIRST, then the enabled extras).

    All joins are causal — canonical window ends at the bar, funding/HTF use backward merges,
    volume z-score uses a trailing window. Warmup and atr<=0 bars are NaN (dropped downstream by
    the model's NaN mask). `funding` is the DataFrame(timestamp, rate) from
    ``funding_fetcher.load_cached_funding``; when None/empty the funding column is all-NaN (the
    model mask then drops every row, so a funding-bearing subset is effectively unavailable — the
    bake-off harness detects and reports that rather than fitting on nothing).

    `columns` selects a subset for ablations; the result preserves ENRICHED_COLUMNS order so the
    canonical block stays first (canonical_indices == (0, 1, 2, 3)). Unknown names raise.
    """
    if thresholds is None:
        thresholds = dict(_DEFAULT_COMPOSITE_THRESHOLDS)
    if columns is None:
        columns = list(ENRICHED_COLUMNS)
    unknown = [c for c in columns if c not in ENRICHED_COLUMNS]
    if unknown:
        raise ValueError(f"unknown enriched columns {unknown}; known: {ENRICHED_COLUMNS}")
    # Re-order to the canonical-first global order regardless of caller order, so the canonical
    # block always leads (canonical_indices == (0, 1, 2, 3)) and the naming path stays positional.
    columns = [c for c in ENRICHED_COLUMNS if c in columns]

    vol_window = int(vol_window) if vol_window else int(period)
    out = pd.DataFrame(index=df.index)
    canon = composite_feature_matrix(df, period, thresholds)
    for col in CANONICAL_COLUMNS:
        out[col] = canon[col].to_numpy(dtype=float)
    if "funding_rate" in columns:
        from funding_fetcher import attach_funding_column
        out["funding_rate"] = attach_funding_column(df, funding)["funding_rate"].to_numpy(dtype=float)
    if "volume_z" in columns:
        out["volume_z"] = _volume_z_column(df, vol_window)
    if "htf_range_eff" in columns:
        out["htf_range_eff"] = _htf_range_eff_column(df, period, thresholds, htf_multiple)
    return out[columns]


def canonical_indices_for(columns: list[str]) -> tuple[int, int, int, int]:
    """Positions of (return_eff, range_eff, efficiency, adx) within `columns`. Requires all four
    canonical columns present (naming needs them); raises otherwise."""
    cols = list(columns)
    missing = [c for c in CANONICAL_COLUMNS if c not in cols]
    if missing:
        raise ValueError(f"naming needs all canonical columns; missing {missing}")
    return tuple(cols.index(c) for c in CANONICAL_COLUMNS)  # type: ignore[return-value]


def assert_feature_contract(model: dict, columns: list[str]) -> None:
    """Fail loudly when a decode matrix's columns don't match the model's fit columns exactly
    (same names, same order). forward_filter_labels is positional, so a mismatch silently decodes
    garbage — this turns it into an error at the boundary."""
    fit_cols = list(model.get("features", []))
    if list(columns) != fit_cols:
        raise ValueError(
            f"feature-order contract violated: model fit on {fit_cols} but decode matrix has "
            f"{list(columns)} — forward_filter_labels needs identical column order")


def decode_with_model(matrix: pd.DataFrame, model: dict):
    """Decode causal labels for an enriched matrix, enforcing the column-order contract first."""
    from regime_hmm import forward_filter_labels
    assert_feature_contract(model, list(matrix.columns))
    return forward_filter_labels(matrix.to_numpy(dtype=float), model)
