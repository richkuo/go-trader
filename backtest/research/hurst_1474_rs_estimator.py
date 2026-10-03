#!/usr/bin/env python3

import argparse
import json
import math
import os
import sys
import time
from concurrent.futures import ThreadPoolExecutor
from typing import Callable, Optional, Sequence

_THIS_DIR = os.path.dirname(os.path.abspath(__file__))
_BACKTEST = os.path.abspath(os.path.join(_THIS_DIR, ".."))
_ROOT = os.path.abspath(os.path.join(_BACKTEST, ".."))
for _p in (_THIS_DIR, _BACKTEST, _ROOT, os.path.join(_ROOT, "shared_tools"),
           os.path.join(_ROOT, "shared_strategies", "open")):
    if _p not in sys.path:
        sys.path.insert(0, _p)

import numpy as np
import pandas as pd

from eval_windows import (
    DEFAULT_CAPITAL,
    FEE_PLATFORM,
    dataset_key,
)
from indicators_core import (
    HURST_RS_MIN_POINTS,
    hurst_exponent,
    hurst_rescaled_range,
)

import hurst_1410_gate_calibration as study1410
import hurst_1422_gate_power as study1422
import hurst_1424_gate_resolution as study1424
import hurst_1426_two_sided_sort as study1426

cache_entry_is_usable = study1410.cache_entry_is_usable
cache_meta = study1410.cache_meta
decision_series = study1410.decision_series
entry_stamp_series = study1410.entry_stamp_series
required_lead_bars = study1410.required_lead_bars
rolling_hurst = study1410.rolling_hurst
slice_window = study1410.slice_window
warmup_audit = study1410.warmup_audit

FAMILIES = study1410.FAMILIES
FAMILY_EXEMPLARS = study1410.FAMILY_EXEMPLARS
FAMILY_SENSE = study1410.FAMILY_SENSE
HURST_WINDOWS = study1410.HURST_WINDOWS
SENSE_HIGH = study1410.SENSE_HIGH
SENSE_LOW = study1410.SENSE_LOW
STAMP_LEAD_BARS = study1410.STAMP_LEAD_BARS
WARMUP_MARGIN_BARS = study1410.WARMUP_MARGIN_BARS

anti_signal_side = study1422.anti_signal_side
dedup_entries = study1422.dedup_entries
effective_n = study1422.effective_n
usable_cluster_rows = study1422.usable_cluster_rows
_rank1_threshold = study1422._rank1_threshold
_separation = study1422._separation

COHORT_EXPLORATORY = study1422.COHORT_EXPLORATORY
COHORT_PRIMARY = study1422.COHORT_PRIMARY
MIN_CLUSTER_SPAN_DAYS = study1422.MIN_CLUSTER_SPAN_DAYS
MIN_OFFSET_DAYS = study1422.MIN_OFFSET_DAYS

adx_entry_stamp = study1422.adx_entry_stamp
build_leg = study1424.build_leg
cell_cohort = study1424.cell_cohort
coverage_audit = study1424.coverage_audit
ensure_min_history = study1424.ensure_min_history
qualified_symbol = study1424.qualified_symbol
scored_warmup_leads = study1424.scored_warmup_leads
signed_efficiency = study1424.signed_efficiency
symbol_return_correlations = study1424.symbol_return_correlations
_fmt = study1424._fmt
_fmt_p = study1424._fmt_p
_fmt_signed = study1424._fmt_signed

ALPHA = study1424.ALPHA
DATASETS = study1424.DATASETS
DATASET_WINDOWS = study1424.DATASET_WINDOWS
CONTINUITY_TARGET = study1424.CONTINUITY_TARGET
HORIZON_HOURS = study1424.HORIZON_HOURS
MDE_EFF_GRID_MAX = study1424.MDE_EFF_GRID_MAX
MDE_PP_GRID_MAX = study1424.MDE_PP_GRID_MAX
MIN_KEPT_EFFECTIVE = study1424.MIN_KEPT_EFFECTIVE
MIN_SUPPRESSED_EFFECTIVE = study1424.MIN_SUPPRESSED_EFFECTIVE
N_PERM = study1424.N_PERM
N_PERM_MDE = study1424.N_PERM_MDE
PRIMARY_CONFIG_ID = study1424.PRIMARY_CONFIG_ID
PRIMARY_FAMILY = study1424.PRIMARY_FAMILY
PRIMARY_FAMILY_SIZE = study1424.PRIMARY_FAMILY_SIZE
PRIMARY_TARGET = study1424.PRIMARY_TARGET
WINDOWS = study1424.WINDOWS
WINDOW_ORDER = study1424.WINDOW_ORDER
WINDOW_OWNER = study1424.WINDOW_OWNER

resolve_primary_config_id = study1424.resolve_primary_config_id
_JSON_1410 = study1424._JSON_1410
_JSON_1424 = study1424._DEFAULT_JSON_OUT

doubled_tail_p = study1426.doubled_tail_p
two_sided_cluster_permutation_pvalue_group_diff = \
    study1426.two_sided_cluster_permutation_pvalue_group_diff
two_sided_min_detectable_effect_eff = study1426.two_sided_min_detectable_effect_eff
two_sided_min_detectable_effect_pp = study1426.two_sided_min_detectable_effect_pp
validity_gate = study1426.validity_gate

MODE_BELOW_LIMIT = study1426.MODE_BELOW_LIMIT
MODE_NO_SEPARATION = study1426.MODE_NO_SEPARATION
MODE_OK = study1426.MODE_OK
MODE_UNRESOLVABLE = study1426.MODE_UNRESOLVABLE
TWO_SIDED = study1426.TWO_SIDED
TWO_SIDED_P_DEFINITION = study1426.TWO_SIDED_P_DEFINITION
_JSON_1426 = study1426._DEFAULT_JSON_OUT

SCHEMA_VERSION = 1
ISSUE = 1474
SEED = ISSUE

CONTRACT_REPORT_BASENAME = study1426.CONTRACT_REPORT_BASENAME
CONTRACT_PATH_CLAIMED = False

ESTIMATOR_DFA = "dfa"
ESTIMATOR_RS = "rs_anis_lloyd"
ESTIMATOR_RS_RAW = "rs_raw"
ESTIMATORS = (ESTIMATOR_DFA, ESTIMATOR_RS, ESTIMATOR_RS_RAW)
ESTIMATOR_LABELS = {
    ESTIMATOR_DFA: "DFA, the #1409 SSoT `hurst_exponent`",
    ESTIMATOR_RS: "R/S, Anis-Lloyd corrected, `hurst_rescaled_range`",
    ESTIMATOR_RS_RAW: "R/S, raw slope, `hurst_rescaled_range(corrected=False)`",
}

REFERENCE_WINDOW = 1000
AGREEMENT_WINDOWS = tuple(HURST_WINDOWS) + (REFERENCE_WINDOW,)
PERSISTENT_SIDE_EDGE = 0.5

BIAS_SAMPLE_SIZES = (101, 128, 256, 512, 1000, 2000)
BIAS_DRAWS = 500
BIAS_SIGMA = 0.01
BIAS_BASE_PRICE = 100.0

VERDICT_BOUNDED = "estimator_risk_bounded"
VERDICT_MOVES = "estimator_choice_moves_the_number"
VERDICT_UNRESOLVED = "unresolved"
VERDICT_LABELS = {
    VERDICT_BOUNDED: "THE ESTIMATOR CHOICE DOES NOT MOVE THE #1424 NUMBER",
    VERDICT_MOVES: "THE ESTIMATOR CHOICE MOVES THE #1424 NUMBER",
    VERDICT_UNRESOLVED: "UNRESOLVED",
}

NON_GOALS = (
    "NON-GOALS, fixed before the run and enforced by the acceptance criteria. "
    "This study adds NO `metrics[\"hurst_rs\"]` key to the live payload; "
    "`shared_tools/regime.py`, `scheduler/hurst_gate.go` and "
    "`config.example.json` are untouched; no threshold and no estimator swap "
    "is recommended. `hurst_exponent` stays the #1409 single source of truth "
    "for every live and backtest path and is byte-identical after this work, "
    "and `hurst_rescaled_range` is a SECOND estimator that only this research "
    "harness reads. A follow-up issue may promote R/S to the live payload "
    "only if the agreement section shows a material, signed difference on the "
    "confirmatory family.")

CONTRACT_PATH_STATEMENT = (
    "CONTRACT PATH: this study DEFERS. `hurst_gate_calibration.md` is the "
    "live-evidence path cited by `scheduler/hurst_gate.go` and #1412's Stage "
    "0. An estimator comparison decides nothing about a shipping gate: it "
    "re-scores the SAME pinned hypothesis under a second measuring "
    "instrument, and its whole purpose is to say how much the instrument "
    "moves the number. `hurst_1424_gate_resolution.py` keeps the path, and "
    "this study's `main` refuses it unconditionally.")

REFERENCE_WINDOW_STATEMENT = (
    f"The {REFERENCE_WINDOW}-bar reference window is a RESEARCH window and it "
    f"cannot run live as things stand. `backtest/hurst_gate.py`'s "
    f"`hurst_live_frame_bars` fetches `max(200, 2*maxPeriod-1+10)` bars, "
    f"which is 200 bars at every regime period this repo configures, so a "
    f"{REFERENCE_WINDOW}-bar rolling estimate is undefined on every live "
    f"cycle. It is reported here only to show where the two estimators "
    f"converge once the sample is long enough. Reading it as a live option "
    f"would need a deeper fetch that no issue has proposed.")

ESTIMATOR_1409_CLAIM = (
    "#1409 chose DFA over classic rescaled range with the note \"R/S is too "
    "noisy at the window lengths this system uses\", and that claim was never "
    "measured on this repo's data. The bias section is that measurement: it "
    "reports the centre and the spread of every estimator on a memoryless "
    "series at each window length, so the claim is answered with a number "
    "instead of being restated.")

KEY_RISK_PREDICTION = (
    "The gate-separation section is expected to stay INCONCLUSIVE under every "
    "estimator, and the reason is inherited rather than new. #1424 measured "
    "-0.005098 efficiency units against a row-matched limit of 0.013, and "
    "#1426 re-measured -0.004617 against the same 0.013 two-sided. A second "
    "estimator changes the measuring instrument and it does not change the "
    "pool, the effective N, or the calendar-cluster structure that sets the "
    "limit. What the section buys is a bound on ESTIMATOR RISK: if R/S moves "
    "the separation by less than the detection limit, the #1424 verdict was "
    "not an artefact of the estimator choice, and that is a statement #1424 "
    "could not make about itself. A move at or above the limit is the "
    "opposite finding, and it licenses a follow-up rather than a threshold. "
    "It is a PREDICTION and not a requirement: the machinery below decides.")

CONSTANT_OFFSET_STATEMENT = (
    "The Anis-Lloyd correction is a CONSTANT SHIFT at a fixed window. The "
    "corrected estimate fits the same log-log regression against "
    "`log(R/S) - log(E[R/S])`, the block grid depends only on the window "
    "length, and least squares is linear, so `H_corrected = H_raw - c(W) + "
    "0.5` for one constant `c(W)` per window. Raw and corrected R/S therefore "
    "carry IDENTICAL Spearman correlation against DFA and identical row "
    "ordering, and they differ only in WHERE the 0.5 edge falls. That edge is "
    "exactly what the gate reads, so the two are reported as separate "
    "estimators in the separation section rather than folded together.")

_DEFAULT_JSON_OUT = os.path.join(_THIS_DIR, "hurst_1474_rs_estimator.json")
_DEFAULT_REPORT_OUT = os.path.join(_THIS_DIR, "hurst_1474_rs_estimator.md")
_CONTRACT_REPORT_OUT = os.path.join(_THIS_DIR, CONTRACT_REPORT_BASENAME)


def estimator_fn(estimator: str) -> Callable[[pd.Series], float]:
    if estimator == ESTIMATOR_DFA:
        return hurst_exponent
    if estimator == ESTIMATOR_RS:
        return lambda close: hurst_rescaled_range(close, corrected=True)
    if estimator == ESTIMATOR_RS_RAW:
        return lambda close: hurst_rescaled_range(close, corrected=False)
    raise ValueError(f"unknown estimator {estimator!r}; known: {list(ESTIMATORS)}")


def rolling_estimator(close: pd.Series, window: int, estimator: str,
                      first_needed: Optional[pd.Timestamp] = None) -> pd.Series:
    if window < 2:
        raise ValueError(f"hurst window must be >= 2, got {window}")
    if estimator == ESTIMATOR_DFA:
        return rolling_hurst(close, window, first_needed=first_needed)
    fn = estimator_fn(estimator)
    prices = close.astype(float)
    n = len(prices)
    values = np.full(n, np.nan, dtype=float)
    start = window - 1
    if first_needed is not None:
        pos = int(prices.index.searchsorted(pd.Timestamp(first_needed)))
        start = max(start, pos - STAMP_LEAD_BARS)
    for i in range(start, n):
        values[i] = fn(prices.iloc[i - window + 1: i + 1])
    return pd.Series(values, index=prices.index,
                     name=f"hurst_{estimator}_{window}")


def _finite_pairs(a: pd.Series, b: pd.Series) -> tuple:
    left = a.to_numpy(dtype=float)
    right = b.to_numpy(dtype=float)
    keep = np.isfinite(left) & np.isfinite(right)
    return left[keep], right[keep]


def _pearson(a: np.ndarray, b: np.ndarray) -> Optional[float]:
    if a.size < 3 or float(np.std(a)) == 0.0 or float(np.std(b)) == 0.0:
        return None
    return round(float(np.corrcoef(a, b)[0, 1]), 6)


def _average_ranks(values: np.ndarray) -> np.ndarray:
    order = np.argsort(values, kind="mergesort")
    ranks = np.empty(values.size, dtype=float)
    ranks[order] = np.arange(1, values.size + 1, dtype=float)
    ordered = values[order]
    start = 0
    for i in range(1, values.size + 1):
        if i == values.size or ordered[i] != ordered[start]:
            if i - start > 1:
                ranks[order[start:i]] = float(np.mean(ranks[order[start:i]]))
            start = i
    return ranks


def _spearman(a: np.ndarray, b: np.ndarray) -> Optional[float]:
    if a.size < 3:
        return None
    return _pearson(_average_ranks(a), _average_ranks(b))


def agreement_stats(reference: pd.Series, candidate: pd.Series) -> dict:
    ref, cand = _finite_pairs(reference, candidate)
    n = int(ref.size)
    n_ref_finite = int(np.sum(np.isfinite(reference.to_numpy(dtype=float))))
    n_cand_finite = int(np.sum(np.isfinite(candidate.to_numpy(dtype=float))))
    out = {
        "n_rows": n,
        "n_reference_only": n_ref_finite - n,
        "n_candidate_only": n_cand_finite - n,
        "pearson": None,
        "spearman": None,
        "mean_signed_difference": None,
        "mean_absolute_difference": None,
        "side_disagreement_share": None,
        "reference_persistent_share": None,
        "candidate_persistent_share": None,
    }
    if n == 0:
        return out
    diff = cand - ref
    out["pearson"] = _pearson(ref, cand)
    out["spearman"] = _spearman(ref, cand)
    out["mean_signed_difference"] = round(float(np.mean(diff)), 6)
    out["mean_absolute_difference"] = round(float(np.mean(np.abs(diff))), 6)
    ref_side = ref >= PERSISTENT_SIDE_EDGE
    cand_side = cand >= PERSISTENT_SIDE_EDGE
    out["side_disagreement_share"] = round(float(np.mean(ref_side != cand_side)), 6)
    out["reference_persistent_share"] = round(float(np.mean(ref_side)), 6)
    out["candidate_persistent_share"] = round(float(np.mean(cand_side)), 6)
    return out


def _concat(series_by_key: dict, keys: Sequence[str]) -> pd.Series:
    parts = [series_by_key[k].to_numpy(dtype=float) for k in keys]
    if not parts:
        return pd.Series([], dtype=float)
    return pd.Series(np.concatenate(parts))


def agreement_section(rolling: dict, datasets: Sequence[tuple],
                      windows: Sequence[int]) -> dict:
    out: dict = {
        "by_window": {},
        "windows": [int(w) for w in windows],
        "reference_window": REFERENCE_WINDOW,
        "live_windows": [int(w) for w in HURST_WINDOWS],
        "persistent_side_edge": PERSISTENT_SIDE_EDGE,
        "reference_window_statement": REFERENCE_WINDOW_STATEMENT,
        "constant_offset_statement": CONSTANT_OFFSET_STATEMENT,
    }
    candidates = [est for est in ESTIMATORS if est != ESTIMATOR_DFA]
    for hw in windows:
        per_dataset: dict = {}
        reference: dict = {}
        candidate: dict = {est: {} for est in candidates}
        for ds in datasets:
            key = dataset_key(qualified_symbol(ds[0], ds[1]), ds[2])
            dfa = rolling.get((ds, int(hw), ESTIMATOR_DFA))
            if dfa is None:
                continue
            reference[key] = dfa
            entry = {}
            for est in candidates:
                cand = rolling.get((ds, int(hw), est))
                if cand is None:
                    continue
                candidate[est][key] = cand
                entry[est] = agreement_stats(dfa, cand)
            if entry:
                per_dataset[key] = entry
        pooled = {}
        for est in candidates:
            keys = [k for k in reference if k in candidate[est]]
            pooled[est] = agreement_stats(_concat(reference, keys),
                                          _concat(candidate[est], keys))
        out["by_window"][str(int(hw))] = {
            "pooled": pooled,
            "by_dataset": {k: per_dataset[k] for k in sorted(per_dataset)},
            "n_datasets": len(per_dataset),
        }
    return out


def _random_walk_prices(n: int, seed: int) -> pd.Series:
    rng = np.random.default_rng(seed)
    return pd.Series(BIAS_BASE_PRICE
                     * np.exp(np.cumsum(rng.normal(0.0, BIAS_SIGMA, int(n)))))


def bias_draw_seed(n: int, draw: int) -> int:
    return int(SEED) * 1_000_000 + int(n) * 1_000 + int(draw)


def _summarize(values: Sequence[float]) -> dict:
    arr = np.asarray(values, dtype=float)
    finite = arr[np.isfinite(arr)]
    out = {"n_draws": int(arr.size), "n_defined": int(finite.size),
           "mean": None, "median": None, "q25": None, "q75": None,
           "iqr": None, "sd": None, "bias": None}
    if finite.size == 0:
        return out
    out["mean"] = round(float(np.mean(finite)), 6)
    out["median"] = round(float(np.median(finite)), 6)
    out["q25"] = round(float(np.percentile(finite, 25)), 6)
    out["q75"] = round(float(np.percentile(finite, 75)), 6)
    out["iqr"] = round(float(out["q75"]) - float(out["q25"]), 6)
    out["sd"] = round(float(np.std(finite)), 6)
    out["bias"] = round(float(out["mean"]) - PERSISTENT_SIDE_EDGE, 6)
    return out


def bias_section(sample_sizes: Sequence[int] = BIAS_SAMPLE_SIZES,
                 draws: int = BIAS_DRAWS, jobs: int = 4) -> dict:
    fns = {est: estimator_fn(est) for est in ESTIMATORS}

    def _one(n: int) -> tuple:
        collected = {est: [] for est in ESTIMATORS}
        for draw in range(int(draws)):
            close = _random_walk_prices(n, bias_draw_seed(n, draw))
            for est, fn in fns.items():
                collected[est].append(fn(close))
        return int(n), {est: _summarize(vals) for est, vals in collected.items()}

    rows = {}
    with ThreadPoolExecutor(max_workers=max(1, int(jobs))) as pool:
        for n, summary in pool.map(_one, list(sample_sizes)):
            rows[str(n)] = summary
    return {
        "generator": ("independent Gaussian log-return random walks, sigma="
                      f"{BIAS_SIGMA}, base price {BIAS_BASE_PRICE}, one numpy "
                      "default_rng per draw seeded from the issue number, so "
                      "every cell is reproducible"),
        "seed_rule": "SEED * 1_000_000 + n * 1_000 + draw",
        "sample_sizes": [int(n) for n in sample_sizes],
        "draws": int(draws),
        "estimators": list(ESTIMATORS),
        "min_points": int(HURST_RS_MIN_POINTS),
        "by_n": rows,
        "claim_under_test": ESTIMATOR_1409_CLAIM,
    }


def stamp_rows(rows: Sequence[dict], stamps: dict,
               windows: Sequence[int] = AGREEMENT_WINDOWS) -> None:
    for trade in rows:
        dataset = (trade["exchange"], trade["base_symbol"], trade["timeframe"])
        entry = pd.Timestamp(trade["entry_date"])
        by_estimator = {}
        for est in ESTIMATORS:
            per_window = {}
            for hw in windows:
                series = stamps.get((dataset, int(hw), est))
                value = None
                if series is not None:
                    try:
                        raw = float(series.loc[entry])
                    except KeyError:
                        raw = float("nan")
                    if math.isfinite(raw):
                        value = round(raw, 6)
                per_window[str(int(hw))] = value
            by_estimator[est] = per_window
        trade["h_by_estimator"] = by_estimator


def matched_rows(family_rows: Sequence[dict], hurst_window: int,
                 estimators: Sequence[str]) -> tuple:
    kept = []
    dropped_target = 0
    dropped_undefined = 0
    for trade in family_rows:
        if trade.get("cohort") != COHORT_PRIMARY:
            continue
        if trade.get("efficiency") is None:
            dropped_target += 1
            continue
        stamps = trade.get("h_by_estimator") or {}
        values = [(stamps.get(est) or {}).get(str(int(hurst_window)))
                  for est in estimators]
        if any(v is None or not math.isfinite(float(v)) for v in values):
            dropped_undefined += 1
            continue
        kept.append(trade)
    idx, excluded = usable_cluster_rows(kept)
    rows = [kept[i] for i in idx]
    audit = {
        "estimators": list(estimators),
        "n_matched": len(kept),
        "n_scored": len(rows),
        "n_dropped_no_target": dropped_target,
        "n_dropped_undefined_h": dropped_undefined,
        "n_dropped_short_cluster": len(kept) - len(rows),
        "excluded_datasets": excluded,
    }
    return audit, rows


def estimator_measurement(rows: Sequence[dict], hurst_window: int,
                          estimator: str, family: str, rho_by_symbol: dict,
                          n_perm: int, n_perm_mde: int, seed: int) -> dict:
    sense = FAMILY_SENSE[family]
    values, returns, mask = [], [], []
    for trade in rows:
        h = float(trade["h_by_estimator"][estimator][str(int(hurst_window))])
        values.append(float(trade["efficiency"]))
        returns.append(float(trade["pnl_pct_net"]))
        mask.append(anti_signal_side(h, sense))
    n_suppressed = int(np.sum(mask))
    out = {
        "estimator": estimator,
        "hurst_window": int(hurst_window),
        "family": family,
        "sense": sense,
        "n_rows": len(rows),
        "n_suppressed": n_suppressed,
        "n_kept": len(rows) - n_suppressed,
        "effective_n_suppressed": None,
        "effective_n_kept": None,
        "suppressed_floor_met": None,
        "kept_floor_met": None,
        "separation": None,
        "separation_return": None,
        "limit": None,
        "limit_return": None,
        "p": None,
        "p_return": None,
        "gate": None,
        "reason": "",
    }
    if not rows or n_suppressed in (0, len(rows)):
        out["reason"] = "no testable contrast"
        return out
    suppressed_rows = [t for t, m in zip(rows, mask) if m]
    kept_rows = [t for t, m in zip(rows, mask) if not m]
    out["effective_n_suppressed"] = effective_n(suppressed_rows, rho_by_symbol)
    out["effective_n_kept"] = effective_n(kept_rows, rho_by_symbol)
    out["suppressed_floor_met"] = bool(
        out["effective_n_suppressed"] >= MIN_SUPPRESSED_EFFECTIVE)
    out["kept_floor_met"] = bool(out["effective_n_kept"] >= MIN_KEPT_EFFECTIVE)
    out["separation"] = _separation(values, mask)
    out["separation_return"] = _separation(returns, mask)
    out["limit"] = two_sided_min_detectable_effect_eff(
        rows, values, mask, PRIMARY_FAMILY_SIZE, cluster=True,
        n_perm=n_perm_mde, seed=seed)
    out["limit_return"] = two_sided_min_detectable_effect_pp(
        rows, returns, mask, PRIMARY_FAMILY_SIZE, cluster=True,
        n_perm=n_perm_mde, seed=seed)
    out["p"] = two_sided_cluster_permutation_pvalue_group_diff(
        rows, values, mask, n_perm=n_perm, seed=seed).get("p")
    out["p_return"] = two_sided_cluster_permutation_pvalue_group_diff(
        rows, returns, mask, n_perm=n_perm, seed=seed).get("p")
    out["gate"] = validity_gate({
        "by_family_cluster": {family: out["limit"]},
        "by_family_separation": {family: out["separation"]},
        "by_family_n": {family: out["n_rows"]},
    })
    return out


def separation_section(pooled: dict, hurst_windows: Sequence[int],
                       rho_by_symbol: dict, n_perm: int, n_perm_mde: int,
                       seed: int) -> dict:
    family = PRIMARY_FAMILY
    family_rows = pooled.get(family) or []
    by_window = {}
    for hw in hurst_windows:
        audit, rows = matched_rows(family_rows, hw, ESTIMATORS)
        measurements = {
            est: estimator_measurement(rows, hw, est, family, rho_by_symbol,
                                       n_perm, n_perm_mde, seed)
            for est in ESTIMATORS
        }
        dfa_audit, dfa_rows = matched_rows(family_rows, hw, (ESTIMATOR_DFA,))
        unmatched = estimator_measurement(dfa_rows, hw, ESTIMATOR_DFA, family,
                                          rho_by_symbol, n_perm, n_perm_mde,
                                          seed)
        base = measurements[ESTIMATOR_DFA].get("separation")
        deltas = {}
        for est, measurement in measurements.items():
            if est == ESTIMATOR_DFA:
                continue
            if base is None or measurement.get("separation") is None:
                deltas[est] = None
                continue
            deltas[est] = round(float(measurement["separation"]) - float(base), 6)
        by_window[str(int(hw))] = {
            "row_matching": audit,
            "measurements": measurements,
            "separation_delta_vs_dfa": deltas,
            "dfa_unmatched": {"row_matching": dfa_audit,
                              "measurement": unmatched},
        }
    return {
        "family": family,
        "sense": FAMILY_SENSE[family],
        "pinned_config_id": PRIMARY_CONFIG_ID,
        "primary_target": PRIMARY_TARGET,
        "continuity_target": CONTINUITY_TARGET,
        "horizon_hours": HORIZON_HOURS,
        "hurst_windows": [int(hw) for hw in hurst_windows],
        "two_sided": TWO_SIDED,
        "two_sided_p_definition": TWO_SIDED_P_DEFINITION,
        "confirmatory_bar": _rank1_threshold(PRIMARY_FAMILY_SIZE, ALPHA),
        "min_kept_effective": MIN_KEPT_EFFECTIVE,
        "min_suppressed_effective": MIN_SUPPRESSED_EFFECTIVE,
        "by_window": by_window,
    }


def estimator_risk_verdict(separation: dict) -> dict:
    pinned = str(int(max(HURST_WINDOWS)))
    window = (separation.get("by_window") or {}).get(pinned) or {}
    measurements = window.get("measurements") or {}
    dfa = measurements.get(ESTIMATOR_DFA) or {}
    rs = measurements.get(ESTIMATOR_RS) or {}
    limit = dfa.get("limit")
    delta = (window.get("separation_delta_vs_dfa") or {}).get(ESTIMATOR_RS)
    out = {
        "hurst_window": int(pinned),
        "dfa_separation": dfa.get("separation"),
        "rs_separation": rs.get("separation"),
        "dfa_limit": limit,
        "rs_limit": rs.get("limit"),
        "separation_delta": delta,
        "bounded": False,
        "verdict": VERDICT_UNRESOLVED,
        "statement": "",
    }
    if delta is None or limit is None:
        out["statement"] = (
            "The estimator move is UNRESOLVED. Either the row-matched "
            "separation or the detection limit is unavailable at the pinned "
            "window, so this run bounds nothing about estimator risk, and no "
            "follow-up is licensed in either direction.")
        return out
    out["bounded"] = bool(abs(float(delta)) < float(limit))
    out["verdict"] = VERDICT_BOUNDED if out["bounded"] else VERDICT_MOVES
    if out["bounded"]:
        out["statement"] = (
            f"The estimator move is BOUNDED. A swap from DFA to Anis-Lloyd "
            f"R/S at W{pinned} moves the row-matched separation by "
            f"{abs(float(delta)):.6f} efficiency units, which is BELOW the "
            f"{float(limit):.3f} detection limit the same rows carry, so on "
            f"this pool the #1424 verdict is not an artefact of the estimator "
            f"choice. This bounds the INSTRUMENT and it says nothing about "
            f"the market: both estimators sit under the same limit, so "
            f"neither resolves an effect and no threshold ships.")
    else:
        out["statement"] = (
            f"The estimator move REACHES the limit. A swap from DFA to "
            f"Anis-Lloyd R/S at W{pinned} moves the row-matched separation by "
            f"{abs(float(delta)):.6f} efficiency units against a "
            f"{float(limit):.3f} detection limit, so the estimator choice is "
            f"a real term in the #1424 measurement, and a follow-up issue "
            f"should re-open it. It licenses NO threshold: a moved separation "
            f"is still read against its own limit before anything is claimed.")
    return out


def _fmt_share(value) -> str:
    return "-" if value is None else f"{float(value) * 100.0:.2f}%"


def _render_agreement(agreement: dict) -> list:
    lines = ["## 1. Agreement between the two estimators", "",
             agreement["constant_offset_statement"], "",
             agreement["reference_window_statement"], "",
             (f"Rows are the bars on which BOTH estimators are defined. The "
              f"side share counts the bars where the two fall on opposite "
              f"sides of {agreement['persistent_side_edge']}, the edge the "
              f"gate's own `anti_signal_side` reads. The signed difference is "
              f"`candidate - DFA`."), ""]
    for hw in agreement["windows"]:
        block = agreement["by_window"].get(str(int(hw))) or {}
        pooled = block.get("pooled") or {}
        marker = (" (research-only reference window)"
                  if int(hw) == agreement["reference_window"] else "")
        lines.append(f"### W{hw}{marker}")
        lines.append("")
        lines.append("| Estimator | Rows | Pearson | Spearman | Mean signed "
                     "diff | Mean abs diff | Opposite side of 0.5 |")
        lines.append("|---|---:|---:|---:|---:|---:|---:|")
        for est in ESTIMATORS:
            if est == ESTIMATOR_DFA:
                continue
            stats = pooled.get(est) or {}
            lines.append(
                f"| {ESTIMATOR_LABELS[est]} | {stats.get('n_rows', 0)} | "
                f"{_fmt(stats.get('pearson'), 4)} | "
                f"{_fmt(stats.get('spearman'), 4)} | "
                f"{_fmt_signed(stats.get('mean_signed_difference'), 4)} | "
                f"{_fmt(stats.get('mean_absolute_difference'), 4)} | "
                f"{_fmt_share(stats.get('side_disagreement_share'))} |")
        lines.append("")
        rs_stats = pooled.get(ESTIMATOR_RS) or {}
        lines.append(
            f"Persistent-side share at W{hw}: DFA "
            f"{_fmt_share(rs_stats.get('reference_persistent_share'))}, "
            f"Anis-Lloyd R/S "
            f"{_fmt_share(rs_stats.get('candidate_persistent_share'))} over "
            f"{rs_stats.get('n_rows', 0)} shared bars across "
            f"{block.get('n_datasets', 0)} datasets.")
        lines.append("")
    return lines


def _render_bias(bias: dict) -> list:
    lines = ["## 2. Bias and spread on a memoryless series", "",
             bias["claim_under_test"], "",
             (f"{bias['draws']} draws per sample size, {bias['generator']}. "
              f"Seed rule `{bias['seed_rule']}`. `n` counts PRICE points, so "
              f"an estimator sees `n - 1` log returns and needs at least "
              f"`min_points` = {bias['min_points']} of them."), ""]
    lines.append("| n | Estimator | Defined | Mean | Bias vs 0.5 | Q25 | Q75 | "
                 "IQR | SD |")
    lines.append("|---:|---|---:|---:|---:|---:|---:|---:|---:|")
    for n in bias["sample_sizes"]:
        row = bias["by_n"].get(str(int(n))) or {}
        for est in ESTIMATORS:
            stats = row.get(est) or {}
            lines.append(
                f"| {n} | {ESTIMATOR_LABELS[est]} | "
                f"{stats.get('n_defined', 0)}/{stats.get('n_draws', 0)} | "
                f"{_fmt(stats.get('mean'), 4)} | "
                f"{_fmt_signed(stats.get('bias'), 4)} | "
                f"{_fmt(stats.get('q25'), 4)} | {_fmt(stats.get('q75'), 4)} | "
                f"{_fmt(stats.get('iqr'), 4)} | {_fmt(stats.get('sd'), 4)} |")
    lines.append("")
    return lines


def _render_measurement_row(measurement: dict, label: str) -> str:
    gate = measurement.get("gate") or {}
    return (
        f"| {label} | {measurement.get('n_rows', 0)} | "
        f"{measurement.get('n_kept', 0)} | "
        f"{measurement.get('n_suppressed', 0)} | "
        f"{_fmt(measurement.get('effective_n_kept'), 1)} | "
        f"{_fmt(measurement.get('effective_n_suppressed'), 1)} | "
        f"{_fmt_signed(measurement.get('separation'), 6)} | "
        f"{_fmt(measurement.get('limit'), 3)} | "
        f"{_fmt_p(measurement.get('p'))} | "
        f"{gate.get('mode') or '-'} |")


def _render_separation(separation: dict, verdict: dict) -> list:
    lines = ["## 3. Gate separation under each estimator", "",
             (f"The pinned #1424 confirmatory hypothesis is "
              f"`{separation['pinned_config_id']}` on the "
              f"`{separation['family']}` family, sense "
              f"`{separation['sense']}`. Every row below is ROW-MATCHED: a "
              f"trade is scored only where EVERY estimator is defined at that "
              f"window, so the three estimators split the identical row set "
              f"and only the partition changes. Separations are signed and "
              f"read against each estimator's OWN row-matched limit, never "
              f"against a pooled one and never through `abs()`."), "",
             (f"Inference is two-sided, inherited from #1426: "
              f"{separation['two_sided_p_definition']}. The confirmatory bar "
              f"is alpha for a family of {PRIMARY_FAMILY_SIZE}, that is "
              f"{separation['confirmatory_bar']:g}. The primary target is "
              f"`{separation['primary_target']}` over "
              f"{separation['horizon_hours']}h and the continuity target is "
              f"`{separation['continuity_target']}`."), ""]
    for hw in separation["hurst_windows"]:
        block = separation["by_window"].get(str(int(hw))) or {}
        audit = block.get("row_matching") or {}
        lines.append(f"### W{hw}")
        lines.append("")
        lines.append(
            f"Row matching: {audit.get('n_scored', 0)} scored rows from "
            f"{audit.get('n_matched', 0)} matched "
            f"({audit.get('n_dropped_undefined_h', 0)} dropped because at "
            f"least one estimator was undefined, "
            f"{audit.get('n_dropped_no_target', 0)} for a missing target, "
            f"{audit.get('n_dropped_short_cluster', 0)} for a calendar "
            f"cluster too short to rotate).")
        lines.append("")
        lines.append("| Estimator | Rows | Kept | Suppressed | Eff kept | Eff "
                     "suppressed | Separation | Limit | Two-sided p | Gate |")
        lines.append("|---|---:|---:|---:|---:|---:|---:|---:|---:|---|")
        for est in ESTIMATORS:
            measurement = (block.get("measurements") or {}).get(est) or {}
            lines.append(_render_measurement_row(measurement,
                                                 ESTIMATOR_LABELS[est]))
        unmatched = (block.get("dfa_unmatched") or {}).get("measurement") or {}
        lines.append(_render_measurement_row(
            unmatched, "DFA on its OWN rows (not row-matched, #1426 row set)"))
        lines.append("")
        deltas = block.get("separation_delta_vs_dfa") or {}
        for est in ESTIMATORS:
            if est == ESTIMATOR_DFA:
                continue
            lines.append(f"- Separation move versus DFA, {ESTIMATOR_LABELS[est]}: "
                         f"{_fmt_signed(deltas.get(est), 6)} efficiency units.")
        lines.append("")
        lines.append("Continuity target, net return in percentage points:")
        lines.append("")
        lines.append("| Estimator | Separation (pp) | Limit (pp) | Two-sided p |")
        lines.append("|---|---:|---:|---:|")
        for est in ESTIMATORS:
            measurement = (block.get("measurements") or {}).get(est) or {}
            lines.append(
                f"| {ESTIMATOR_LABELS[est]} | "
                f"{_fmt_signed(measurement.get('separation_return'), 6)} | "
                f"{_fmt(measurement.get('limit_return'), 3)} | "
                f"{_fmt_p(measurement.get('p_return'))} |")
        lines.append("")
    lines.append("### Verdict on estimator risk")
    lines.append("")
    lines.append(f"**{VERDICT_LABELS[verdict['verdict']]}**")
    lines.append("")
    lines.append(verdict["statement"])
    lines.append("")
    return lines


def render_report(payload: dict) -> str:
    pre = payload["pre_registered"]
    run = payload["run_summary"]
    verdict = payload["verdict"]
    lines = [
        f"# Hurst estimator comparison: rescaled range against DFA (#{ISSUE})",
        "",
        (f"Report-only estimator comparison over the #1424 pool. It adds a "
         f"classic rescaled-range (R/S) estimator beside the #1409 DFA single "
         f"source of truth and asks three questions: how closely the two "
         f"agree per row, how each is biased and how wide each is on a "
         f"memoryless series at every window length, and whether the "
         f"estimator choice moves the #1424 gate separation at all."),
        "",
        pre["non_goals"],
        "",
        pre["contract_path_statement"],
        "",
        "## Pre-registered key risk",
        "",
        pre["key_risk_prediction"],
        "",
        "## Run summary",
        "",
        f"- Legs scored: {run['legs']}",
        f"- Datasets: {len(pre['datasets'])}",
        f"- Windows scored: {', '.join(pre['windows'])}",
        f"- Rolling Hurst windows: "
        f"{', '.join(str(w) for w in pre['agreement_windows'])} "
        f"(live: {', '.join(str(w) for w in pre['live_windows'])})",
        f"- Estimators: {', '.join(ESTIMATORS)}",
        f"- Pooled `{PRIMARY_FAMILY}` trades: "
        f"{run['pooled_trades'].get(PRIMARY_FAMILY, 0)} "
        f"({run['pooled_primary'].get(PRIMARY_FAMILY, 0)} primary cohort)",
        f"- Permutation draws: {pre['n_perm']} for p, {pre['n_perm_mde']} for "
        f"the detection limit; seed {pre['seed']}",
        f"- Elapsed: {run['elapsed_sec']}s",
        f"- History backfill: "
        f"{'SKIPPED, scored on the venue caches as they stood' if run.get('skip_fetch') else 'ran before scoring'}",
        "",
    ]
    warm = run.get("warmup_reference") or {}
    if warm and not warm.get("sufficient", True):
        lines += [
            (f"WARM-UP SHORTFALL at the {REFERENCE_WINDOW}-bar reference "
             f"window on {len(warm.get('insufficient_datasets') or [])} "
             f"dataset(s): "
             f"{', '.join(warm.get('insufficient_datasets') or []) or '-'}. "
             f"The reference estimate is UNDEFINED on their earliest scored "
             f"bars, those bars simply drop out of the agreement rows, and "
             f"the live windows are unaffected because coverage was audited "
             f"against them."),
            "",
        ]
    lines += _render_agreement(payload["agreement"])
    lines += _render_bias(payload["bias"])
    lines += _render_separation(payload["separation"], verdict)
    lines += [
        "## What this study cannot say",
        "",
        ("It cannot recommend an estimator for the live path. It scores ONE "
         "pinned hypothesis on ONE pool whose sign was already visible before "
         "this design was fixed, it ships no threshold, and it leaves "
         "`hurst_exponent` as the single source of truth every live and "
         "backtest path reads. A bounded estimator move is a statement about "
         "the instrument on these rows and it is not evidence that the gate "
         "works. An estimator move at or above the limit licenses a "
         "follow-up ISSUE and never a configuration change."),
        "",
    ]
    return "\n".join(lines) + "\n"


def report_from_payload(payload: dict) -> str:
    return render_report(payload)


def _parse_datasets(raw: Optional[str]) -> list:
    return study1424._parse_datasets(raw)


def _parse_windows(raw: Optional[str]) -> list:
    return study1424._parse_windows(raw)


def inference_deviations(args) -> list:
    out = []
    if args.n_perm != N_PERM:
        out.append(f"--n-perm {args.n_perm} (pre-registered {N_PERM})")
    if args.n_perm_mde != N_PERM_MDE:
        out.append(f"--n-perm-mde {args.n_perm_mde} "
                   f"(pre-registered {N_PERM_MDE})")
    if args.seed != SEED:
        out.append(f"--seed {args.seed} (pre-registered {SEED})")
    if args.bias_draws != BIAS_DRAWS:
        out.append(f"--bias-draws {args.bias_draws} "
                   f"(pre-registered {BIAS_DRAWS})")
    if args.no_mirror_check:
        out.append("--no-mirror-check (the pre-registered design verifies "
                   "every leg against eval_windows.run_leg)")
    return out


def main(argv: Optional[Sequence[str]] = None) -> int:
    p = argparse.ArgumentParser()
    p.add_argument("--jobs", type=int, default=4, help="worker threads")
    p.add_argument("--out-dir", default=None,
                   help="optional dir for the rolling-Hurst npz cache")
    p.add_argument("--only", default=None,
                   help=f"comma-separated families to run ({', '.join(FAMILIES)})")
    p.add_argument("--windows", default=None, help="comma-separated window names")
    p.add_argument("--datasets", default=None,
                   help="comma-separated [EXCHANGE=]SYMBOL:TIMEFRAME")
    p.add_argument("--hurst-windows", default=None,
                   help="comma-separated rolling Hurst window lengths")
    p.add_argument("--bias-draws", type=int, default=BIAS_DRAWS)
    p.add_argument("--n-perm", type=int, default=N_PERM)
    p.add_argument("--n-perm-mde", type=int, default=N_PERM_MDE)
    p.add_argument("--seed", type=int, default=SEED)
    p.add_argument("--json-out", default=_DEFAULT_JSON_OUT)
    p.add_argument("--report-out", default=_DEFAULT_REPORT_OUT)
    p.add_argument("--write-report", action="store_true",
                   help="render the Markdown report")
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

    if (os.path.abspath(args.report_out)
            == os.path.abspath(_CONTRACT_REPORT_OUT)):
        raise SystemExit(
            f"[{ISSUE}] this study is an ESTIMATOR COMPARISON and DEFERS the "
            f"live-evidence contract path {CONTRACT_REPORT_BASENAME}; "
            f"hurst_1424_gate_resolution.py owns it. Its own render belongs "
            f"at {_DEFAULT_REPORT_OUT}.")

    scope = {
        "only": args.only,
        "datasets": args.datasets,
        "windows": args.windows,
        "hurst_windows": args.hurst_windows,
    }
    scope["complete"] = not any(v for v in scope.values())
    deviations = inference_deviations(args)
    scope["pre_registered_inference"] = not deviations
    if (not scope["complete"] or deviations) and not args.fetch_only:
        narrowed = ", ".join(
            [f"--{k.replace('_', '-')} {v}" for k, v in scope.items()
             if k not in ("complete", "pre_registered_inference") and v]
            + deviations)
        kind = ("a scoped run" if not scope["complete"]
                else "a run that deviates from the pre-registered design")
        if os.path.abspath(args.json_out) == os.path.abspath(_DEFAULT_JSON_OUT):
            raise SystemExit(
                f"[{ISSUE}] refusing to overwrite the committed aggregate "
                f"{_DEFAULT_JSON_OUT} from {kind} ({narrowed}). Pass an "
                f"explicit --json-out.")
        if os.path.abspath(args.report_out) == os.path.abspath(_DEFAULT_REPORT_OUT):
            raise SystemExit(
                f"[{ISSUE}] refusing to target the committed report "
                f"{_DEFAULT_REPORT_OUT} from {kind} ({narrowed}). Pass an "
                f"explicit --report-out.")

    if args.render_only:
        with open(args.json_out) as fh:
            payload = json.load(fh)
        is_committed = (os.path.abspath(args.report_out)
                        == os.path.abspath(_DEFAULT_REPORT_OUT))
        if is_committed:
            stamp = ((payload.get("run_summary") or {}).get("scope") or {})
            if not stamp.get("complete"):
                raise SystemExit(
                    f"[{ISSUE}] {args.json_out} is not stamped as a complete "
                    f"run, so it may not be rendered to the committed report "
                    f"{_DEFAULT_REPORT_OUT}.")
            if not stamp.get("pre_registered_inference"):
                raise SystemExit(
                    f"[{ISSUE}] {args.json_out} is not stamped as having run "
                    f"the pre-registered inference settings and verification, "
                    f"so it may not be rendered to the committed report "
                    f"{_DEFAULT_REPORT_OUT}.")
            if not args.write_report:
                raise SystemExit(
                    f"[{ISSUE}] writing the committed report needs "
                    f"--write-report, on --render-only exactly as on a "
                    f"scoring run.")
        report = report_from_payload(payload)
        with open(args.report_out, "w") as fh:
            fh.write(report)
        print(f"[{ISSUE}] re-rendered {args.report_out} from {args.json_out}")
        return 0

    datasets = _parse_datasets(args.datasets)
    if args.fetch_only:
        ensure_min_history(datasets)
        print(f"[{ISSUE}] backfill complete")
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
                     if args.hurst_windows else tuple(HURST_WINDOWS))
    agreement_windows = tuple(dict.fromkeys(
        tuple(hurst_windows) + (REFERENCE_WINDOW,)))

    resolved = resolve_primary_config_id(_JSON_1410)
    if resolved != PRIMARY_CONFIG_ID:
        raise SystemExit(
            f"pinned hypothesis {PRIMARY_CONFIG_ID!r} no longer matches the "
            f"committed #1410 argmin {resolved!r}. Re-pin deliberately; never "
            f"let it drift.")

    started = time.time()
    backfill = {}
    if not args.skip_fetch:
        print(f"[{ISSUE}] backfilling {len(datasets)} datasets...")
        backfill = ensure_min_history(datasets)

    from data_fetcher import load_cached_data
    from registry_loader import load_registry
    reg = load_registry("spot")

    print(f"[{ISSUE}] loading {len(datasets)} datasets from the venue caches...")
    frames = {}
    for dataset in datasets:
        exchange_id, symbol, timeframe = dataset
        try:
            frames[dataset] = load_cached_data(symbol, timeframe,
                                               exchange_id=exchange_id)
        except Exception as exc:
            print(f"[{ISSUE}] load FAILED for {exchange_id} "
                  f"{dataset_key(symbol, timeframe)}: {exc}")
            frames[dataset] = pd.DataFrame()

    coverage = coverage_audit(frames, window_names, hurst_windows)
    print(f"[{ISSUE}] coverage: {coverage['n_kept']}/{coverage['n_cells']} owned "
          f"cells kept, {coverage['n_dropped']} dropped, "
          f"{coverage['n_unowned']} not owned")
    for d in coverage["dropped"]:
        print(f"[{ISSUE}]   dropped {d['dataset']} {d['window']}: {d['reason']}")

    def _cell_ok(dataset, window):
        exchange_id, symbol, timeframe = dataset
        key = dataset_key(qualified_symbol(exchange_id, symbol), timeframe)
        return bool(coverage["cells"].get(f"{key}|{window}"))

    usable_datasets = [ds for ds in datasets
                       if any(_cell_ok(ds, w) for w in window_names)]
    if not usable_datasets:
        raise SystemExit(f"[{ISSUE}] no dataset carries a scoreable cell; "
                         f"nothing to do")

    scored_windows = [w for w in window_names
                      if any(_cell_ok(ds, w) for ds in usable_datasets)]
    first_needed_by_ds = {}
    for ds in usable_datasets:
        own = [w for w in scored_windows if _cell_ok(ds, w)]
        first_needed_by_ds[ds] = min(pd.Timestamp(WINDOWS[w][0]) for w in own)

    leads = scored_warmup_leads(frames, coverage, scored_windows)
    warmup = warmup_audit(leads, hurst_windows)
    warmup_reference = warmup_audit(leads, agreement_windows)
    if not warmup["sufficient"]:
        print(f"[{ISSUE}] WARNING: warm-up shortfall on "
              f"{len(warmup['insufficient_datasets'])} dataset(s): "
              f"{', '.join(warmup['insufficient_datasets'])}. H is UNDEFINED "
              f"on their first scored bars.")
    else:
        print(f"[{ISSUE}] warm-up OK at the live windows: min lead "
              f"{warmup['min_lead_bars']} bars "
              f"(need {warmup['required_bars']}).")
    if not warmup_reference["sufficient"]:
        print(f"[{ISSUE}] reference window {REFERENCE_WINDOW}: warm-up "
              f"shortfall on {len(warmup_reference['insufficient_datasets'])} "
              f"dataset(s); those bars drop out of the agreement rows only.")

    jobs = [(ds, hw, est) for ds in usable_datasets
            for hw in agreement_windows for est in ESTIMATORS]
    print(f"[{ISSUE}] computing {len(jobs)} rolling (dataset, window, "
          f"estimator) series...")
    cache_path = None
    if args.out_dir:
        os.makedirs(args.out_dir, exist_ok=True)
        cache_path = os.path.join(args.out_dir, f"hurst_{ISSUE}_rolling.npz")
    cached = {}
    if cache_path and os.path.exists(cache_path):
        with np.load(cache_path, allow_pickle=False) as z:
            cached = {k: z[k] for k in z.files}

    def _rolling_key(dataset, hw, estimator):
        exchange_id, symbol, timeframe = dataset
        return f"{exchange_id}|{symbol}|{timeframe}|{hw}|{estimator}"

    def _rolling_job(job):
        dataset, hw, estimator = job
        key = _rolling_key(dataset, hw, estimator)
        frame = frames[dataset]
        first_needed = first_needed_by_ds[dataset]
        if key in cached and cache_entry_is_usable(
                cached.get(f"meta|{key}"), frame.index, first_needed):
            return job, pd.Series(cached[key], index=frame.index)
        return job, rolling_estimator(frame["close"], int(hw), estimator,
                                      first_needed=first_needed)

    rolling: dict = {}
    with ThreadPoolExecutor(max_workers=max(1, args.jobs)) as pool:
        for job, series in pool.map(_rolling_job, jobs):
            rolling[job] = series
    if cache_path:
        arrays = {}
        for job in jobs:
            key = _rolling_key(*job)
            arrays[key] = rolling[job].to_numpy(dtype=float)
            arrays[f"meta|{key}"] = cache_meta(frames[job[0]].index,
                                               first_needed_by_ds[job[0]])
        np.savez_compressed(cache_path, **arrays)

    print(f"[{ISSUE}] computing entry-ADX stamps for {len(usable_datasets)} "
          f"datasets...")
    adx_stamps = {ds: adx_entry_stamp(frames[ds]) for ds in usable_datasets}

    print(f"[{ISSUE}] computing symbol daily-return correlations...")
    rho_by_symbol = symbol_return_correlations(
        {ds: frames[ds] for ds in usable_datasets})

    units = [(family, exemplar, ds, wname)
             for family in families
             for exemplar in FAMILY_EXEMPLARS[family]
             for ds in usable_datasets
             for wname in scored_windows
             if _cell_ok(ds, wname)]
    print(f"[{ISSUE}] scoring {len(units)} legs on #1424's own DFA arms...")

    def _leg_job(unit):
        family, exemplar, ds, wname = unit
        by_window = {hw: rolling[(ds, hw, ESTIMATOR_DFA)] for hw in hurst_windows}
        return build_leg(reg, family, exemplar, ds, wname, frames[ds],
                         by_window, adx_stamps[ds],
                         verify_mirror=not args.no_mirror_check)

    with ThreadPoolExecutor(max_workers=max(1, args.jobs)) as pool:
        legs = [lg for lg in pool.map(_leg_job, units) if lg is not None]
    legs.sort(key=lambda lg: (lg["family"], lg["strategy"], lg["dataset"],
                              lg["window"]))

    pooled = {}
    raw_counts = {}
    for family in families:
        rows = [t for lg in legs if lg["family"] == family for t in lg["trades"]]
        raw_counts[family] = len(rows)
        pooled[family] = dedup_entries(rows, WINDOW_ORDER)
    for family in FAMILIES:
        pooled.setdefault(family, [])
        raw_counts.setdefault(family, 0)

    stamps = {(ds, int(hw), est): entry_stamp_series(rolling[(ds, hw, est)])
              for ds in usable_datasets for hw in agreement_windows
              for est in ESTIMATORS}
    print(f"[{ISSUE}] stamping every pooled row with all "
          f"{len(ESTIMATORS)} estimators at "
          f"{len(agreement_windows)} windows...")
    for family in FAMILIES:
        stamp_rows(pooled[family], stamps, agreement_windows)

    print(f"[{ISSUE}] measuring per-row agreement...")
    agreement = agreement_section(rolling, usable_datasets, agreement_windows)

    print(f"[{ISSUE}] measuring estimator bias on synthetic random walks...")
    bias = bias_section(BIAS_SAMPLE_SIZES, args.bias_draws, args.jobs)

    print(f"[{ISSUE}] re-scoring the pinned #1424 hypothesis under every "
          f"estimator...")
    separation = separation_section(pooled, hurst_windows, rho_by_symbol,
                                    args.n_perm, args.n_perm_mde, args.seed)
    verdict = estimator_risk_verdict(separation)

    payload = {
        "schema_version": SCHEMA_VERSION,
        "issue": ISSUE,
        "pre_registered": {
            "families": {f: list(FAMILY_EXEMPLARS[f]) for f in FAMILIES},
            "family_sense": dict(FAMILY_SENSE),
            "estimators": list(ESTIMATORS),
            "estimator_labels": dict(ESTIMATOR_LABELS),
            "hurst_windows": [int(hw) for hw in hurst_windows],
            "live_windows": [int(hw) for hw in HURST_WINDOWS],
            "agreement_windows": [int(hw) for hw in agreement_windows],
            "reference_window": REFERENCE_WINDOW,
            "reference_window_statement": REFERENCE_WINDOW_STATEMENT,
            "constant_offset_statement": CONSTANT_OFFSET_STATEMENT,
            "persistent_side_edge": PERSISTENT_SIDE_EDGE,
            "bias_sample_sizes": [int(n) for n in BIAS_SAMPLE_SIZES],
            "bias_draws": int(args.bias_draws),
            "bias_sigma": BIAS_SIGMA,
            "non_goals": NON_GOALS,
            "contract_path_claimed": CONTRACT_PATH_CLAIMED,
            "contract_path_statement": CONTRACT_PATH_STATEMENT,
            "estimator_1409_claim": ESTIMATOR_1409_CLAIM,
            "key_risk_prediction": KEY_RISK_PREDICTION,
            "two_sided": TWO_SIDED,
            "two_sided_p_definition": TWO_SIDED_P_DEFINITION,
            "primary_config_id": PRIMARY_CONFIG_ID,
            "primary_family": PRIMARY_FAMILY,
            "primary_family_size": PRIMARY_FAMILY_SIZE,
            "primary_target": PRIMARY_TARGET,
            "continuity_target": CONTINUITY_TARGET,
            "horizon_hours": HORIZON_HOURS,
            "alpha": ALPHA,
            "n_perm": args.n_perm,
            "n_perm_mde": args.n_perm_mde,
            "seed": args.seed,
            "min_kept_effective": MIN_KEPT_EFFECTIVE,
            "min_suppressed_effective": MIN_SUPPRESSED_EFFECTIVE,
            "window_owner": dict(WINDOW_OWNER),
            "windows": list(scored_windows),
            "datasets": [dataset_key(qualified_symbol(ex, sym), tf)
                         for (ex, sym, tf) in usable_datasets],
            "fee_platform": FEE_PLATFORM,
            "capital": DEFAULT_CAPITAL,
        },
        "run_summary": {
            "scope": scope,
            "skip_fetch": bool(args.skip_fetch),
            "jobs": int(args.jobs),
            "legs": len(legs),
            "mirror_verified_legs": sum(1 for lg in legs if lg["mirror_verified"]),
            "raw_trades": raw_counts,
            "pooled_trades": {f: len(pooled[f]) for f in FAMILIES},
            "pooled_primary": {
                f: sum(1 for t in pooled[f] if t["cohort"] == COHORT_PRIMARY)
                for f in FAMILIES},
            "pooled_exploratory": {
                f: sum(1 for t in pooled[f] if t["cohort"] == COHORT_EXPLORATORY)
                for f in FAMILIES},
            "warmup": warmup,
            "warmup_reference": warmup_reference,
            "coverage": coverage,
            "backfill": backfill,
            "symbol_correlations": {f"{a}|{b}": v
                                    for (a, b), v in sorted(rho_by_symbol.items())},
            "elapsed_sec": round(time.time() - started, 2),
        },
        "agreement": agreement,
        "bias": bias,
        "separation": separation,
        "verdict": verdict,
        "legs": [{k: v for k, v in lg.items() if k != "trades"} for lg in legs],
    }

    with open(args.json_out, "w") as fh:
        json.dump(payload, fh, indent=2, sort_keys=False)
        fh.write("\n")
    print(f"[{ISSUE}] wrote {args.json_out}")

    report = render_report(payload)
    if args.write_report:
        with open(args.report_out, "w") as fh:
            fh.write(report)
        print(f"[{ISSUE}] wrote {args.report_out}")
    else:
        print(verdict["statement"])
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
