#!/usr/bin/env python3

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
for _p in (_THIS_DIR, _BACKTEST, _ROOT, os.path.join(_ROOT, "shared_tools")):
    if _p not in sys.path:
        sys.path.insert(0, _p)

import numpy as np
import pandas as pd

from eval_windows import (
    DEFAULT_CAPITAL,
    FEE_PLATFORM,
    PLATFORM,
    dataset_key,
)
from regime_stats import benjamini_hochberg

import hurst_1410_gate_calibration as study1410
import hurst_1422_gate_power as study1422
import hurst_1424_gate_resolution as study1424
import hurst_1426_two_sided_sort as study1426

BUCKET_NAN = study1410.BUCKET_NAN
BUCKETS = study1410.BUCKETS
CONFIG_ID_SEP = study1410.CONFIG_ID_SEP
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
STAMP_LEAD_BARS = study1410.STAMP_LEAD_BARS
WARMUP_MARGIN_BARS = study1410.WARMUP_MARGIN_BARS

bucket_label = study1410.bucket_label
cache_entry_is_usable = study1410.cache_entry_is_usable
cache_meta = study1410.cache_meta
chop_loss = study1410.chop_loss
compound_equity = study1410.compound_equity
decision_series = study1410.decision_series
entry_stamp_series = study1410.entry_stamp_series
gate_config_id = study1410.gate_config_id
hysteresis_mask = study1410.hysteresis_mask
required_lead_bars = study1410.required_lead_bars
rolling_hurst = study1410.rolling_hurst
size_config_id = study1410.size_config_id
size_multiplier = study1410.size_multiplier
slice_window = study1410.slice_window
validate_gate_pair = study1410.validate_gate_pair
warmup_audit = study1410.warmup_audit
warmup_lead_bars = study1410.warmup_lead_bars
win_rate = study1410.win_rate

ADX_PERIOD = study1422.ADX_PERIOD
ADX_SPLIT = study1422.ADX_SPLIT
COHORT_EXPLORATORY = study1422.COHORT_EXPLORATORY
COHORT_PRIMARY = study1422.COHORT_PRIMARY
D_1410 = study1422.D_1410
JOINT_ADX_BUCKETS = study1422.JOINT_ADX_BUCKETS
MIN_OFFSET_DAYS = study1422.MIN_OFFSET_DAYS
MIN_WINDOW_BAR_FRACTION = study1422.MIN_WINDOW_BAR_FRACTION

adx_entry_stamp = study1422.adx_entry_stamp
cluster_rotation_offsets = study1422.cluster_rotation_offsets
dedup_entries = study1422.dedup_entries
effective_n = study1422.effective_n
expected_bars = study1422.expected_bars
joint_adx_bucket = study1422.joint_adx_bucket
rotation_shift_counts = study1422.rotation_shift_counts
timeframe_minutes = study1422.timeframe_minutes
usable_cluster_rows = study1422.usable_cluster_rows
_admissible_offsets = study1422._admissible_offsets
_rank1_threshold = study1422._rank1_threshold
_rotate_values = study1422._rotate_values
_separation = study1422._separation

ALPHA = study1424.ALPHA
BITSTAMP_WINDOWS = study1424.BITSTAMP_WINDOWS
COINBASE_WINDOWS = study1424.COINBASE_WINDOWS
CONTINUITY_TARGET = study1424.CONTINUITY_TARGET
DATASETS = study1424.DATASETS
DATASET_HISTORY_SINCE = study1424.DATASET_HISTORY_SINCE
DATASET_WINDOWS = study1424.DATASET_WINDOWS
EXPLORATORY_HELD_OUT_WINDOWS = study1424.EXPLORATORY_HELD_OUT_WINDOWS
EXPLORATORY_PROTOCOL_WINDOWS = study1424.EXPLORATORY_PROTOCOL_WINDOWS
FEASIBILITY_PROBES = study1424.FEASIBILITY_PROBES
FETCH_PAGE_LIMIT = study1424.FETCH_PAGE_LIMIT
HELD_OUT_MIN_FRACTION = study1424.HELD_OUT_MIN_FRACTION
HELD_OUT_MIN_WINDOWS = study1424.HELD_OUT_MIN_WINDOWS
HISTORY_SINCE = study1424.HISTORY_SINCE
HORIZON_HOURS = study1424.HORIZON_HOURS
MDE_EFF_GRID_MAX = study1424.MDE_EFF_GRID_MAX
MDE_EFF_GRID_STEP = study1424.MDE_EFF_GRID_STEP
MDE_EFF_REFINE_STEP = study1424.MDE_EFF_REFINE_STEP
MDE_PP_GRID_MAX = study1424.MDE_PP_GRID_MAX
MDE_PP_GRID_STEP = study1424.MDE_PP_GRID_STEP
MDE_PP_REFINE_STEP = study1424.MDE_PP_REFINE_STEP
MIN_KEPT_EFFECTIVE = study1424.MIN_KEPT_EFFECTIVE
MIN_SUPPRESSED_EFFECTIVE = study1424.MIN_SUPPRESSED_EFFECTIVE
N_PERM = study1424.N_PERM
N_PERM_MDE = study1424.N_PERM_MDE
PRE2020_WINDOWS = study1424.PRE2020_WINDOWS
PRIMARY_HELD_OUT_WINDOWS = study1424.PRIMARY_HELD_OUT_WINDOWS
PRIMARY_PROTOCOL_MIN_WINDOWS = study1424.PRIMARY_PROTOCOL_MIN_WINDOWS
PRIMARY_PROTOCOL_WINDOWS = study1424.PRIMARY_PROTOCOL_WINDOWS
PRIMARY_TARGET = study1424.PRIMARY_TARGET
RETURN_TOLERANCE_FRAC = study1424.RETURN_TOLERANCE_FRAC
RETURN_TOLERANCE_PP = study1424.RETURN_TOLERANCE_PP
WINDOWS = study1424.WINDOWS
WINDOW_ORDER = study1424.WINDOW_ORDER
WINDOW_OWNER = study1424.WINDOW_OWNER

base_asset = study1424.base_asset
build_leg_run_arm = study1424._run_arm
cell_cohort = study1424.cell_cohort
config_verdict = study1424.config_verdict
ensure_min_history = study1424.ensure_min_history
history_since_for = study1424.history_since_for
horizon_bars = study1424.horizon_bars
owned_windows = study1424.owned_windows
protocol_dd_reduction = study1424.protocol_dd_reduction
qualified_symbol = study1424.qualified_symbol
resolve_primary_config_id = study1424.resolve_primary_config_id
scored_warmup_leads = study1424.scored_warmup_leads
signed_efficiency = study1424.signed_efficiency
symbol_return_correlations = study1424.symbol_return_correlations
trade_direction = study1424.trade_direction
_config_shell = study1424._config_shell
_fmt = study1424._fmt
_fmt_family_seps = study1424._fmt_family_seps
_fmt_p = study1424._fmt_p
_fmt_signed = study1424._fmt_signed
_leg_metrics = study1424._leg_metrics
_target_rows = study1424._target_rows
_window_rows_gate = study1424._window_rows_gate
_MIRRORED_LEG_KEYS = study1424._MIRRORED_LEG_KEYS

TWO_SIDED_P_DEFINITION = study1426.TWO_SIDED_P_DEFINITION
doubled_tail_p = study1426.doubled_tail_p
two_sided_cluster_permutation_pvalue_group_diff = \
    study1426.two_sided_cluster_permutation_pvalue_group_diff
two_sided_cluster_permutation_pvalue_weighted = \
    study1426.two_sided_cluster_permutation_pvalue_weighted
two_sided_min_detectable_effect_eff = \
    study1426.two_sided_min_detectable_effect_eff
two_sided_min_detectable_effect_pp = \
    study1426.two_sided_min_detectable_effect_pp
two_sided_permutation_pvalue_group_diff = \
    study1426.two_sided_permutation_pvalue_group_diff
two_sided_permutation_pvalue_weighted = \
    study1426.two_sided_permutation_pvalue_weighted
_largest_magnitude_signed = study1426._largest_magnitude_signed

_JSON_1410 = study1424._JSON_1410
_JSON_1424 = study1424._DEFAULT_JSON_OUT
_JSON_1426 = study1426._DEFAULT_JSON_OUT

SCHEMA_VERSION = 1
ISSUE = 1427
SEED = ISSUE

LEVEL_ORIGIN = 0.5
DELTA_ORIGIN = 0.0
LEVEL_EDGES = (0.45, 0.50, 0.55)

RECENTRING_RULE = (
    "RE-CENTRING RULE, the single design decision this study makes: every "
    "landmark #1410 fixed on the LEVEL of H is reused on the CHANGE in H by "
    "subtracting 0.5. The level study's own midpoint is 0.5 and a difference "
    "has its midpoint at 0, so `x -> x - 0.5` carries the bucket edges "
    "(0.45/0.50/0.55 become -0.05/0.00/+0.05), the three hysteresis "
    "arm/disarm pairs per family and the pinned hypothesis across unchanged "
    "in shape. The issue is right that a difference inherits no landmark from "
    "H's own 0.5 midpoint. It inherits one from the COMMITTED EDGES of the "
    "study that measured the level, which is the only prior on this scale "
    "that exists, and the transform is mechanical rather than chosen. Every "
    "constant below is DERIVED from #1410's and #1424's committed objects at "
    "import time and asserted against them, so a drift there fails loud here "
    "instead of silently re-scoring a different design.")

DELTA_LOOKBACK_DENOMINATOR = 2

DELTA_LOOKBACK_RATIONALE = (
    "LOOKBACK, fixed before the run and never swept: the change is measured "
    "over `W // 2` bars, where `W` is the rolling Hurst window that produced "
    "the two endpoints. Rolling H at bar `i` reads closes `[i-W+1, i]`, so "
    "two H values `L` bars apart share `W - L` of their sample. At `L < W/2` "
    "the shared majority makes the difference mostly estimator noise on a "
    "handful of fresh bars. At `L = W` the endpoints are independent, but the "
    "change they describe finished a full window ago, which is not the "
    "FORMING regime the hypothesis is about. `W/2` is the smallest lookback "
    "at which half the newer estimate's sample is bars the older estimate "
    "never saw, and it is the only ratio that keeps the three Hurst windows "
    "comparable, because each one gets the same overlap fraction rather than "
    "the same bar count. This is ONE RULE producing exactly one change series "
    "per rolling-H series the design already carries, so it adds no swept "
    "dimension and no Benjamini-Hochberg multiplicity, and the confirmatory "
    "hypothesis fixes exactly one lookback. THE COST IS REAL AND IT IS "
    "MEASURED: the warm-up requirement becomes `W + W//2 + margin` rather "
    "than `W + margin`, the coverage audit drops every cell that cannot meet "
    "it, and the dropped cells are listed by name and reason in this report.")

INFERENCE_DIRECTION = "two_sided"
TWO_SIDED = True

INFERENCE_DIRECTION_RATIONALE = (
    "DIRECTION, pre-registered before the run as a constant rather than left "
    "implicit: the test is TWO-SIDED. The natural hypothesis is directional. "
    "A persistence estimate RISING through its midpoint describes a regime "
    "that is forming, which should favour momentum-style entries and "
    "disfavour mean-reversion ones, and a one-sided test of it would be "
    "cheaper in power. It is refused anyway, for a reason already on this "
    "repository's record: the only effect ever MEASURED on these rows, "
    "#1424's confirmatory separation of -0.005 efficiency units, pointed the "
    "way its one-sided design could not detect at any size, and #1426 exists "
    "solely to remove that blind spot. Re-introducing it on a new predictor "
    "would repeat the same mistake on the same tape. THE COST IS REAL: a "
    "two-sided limit can only be at or above its one-sided counterpart at the "
    "same alpha, so this study resolves a strictly larger effect than a "
    "directional one would. The SIGN is carried and reported everywhere, so a "
    "finding still says which way it points.")

PRIOR_EXPOSURE_DISCLOSURE = (
    "PRIOR EXPOSURE, disclosed before the run. The OUTCOME rows are the same "
    "tape #1424 and #1426 scored, and their results are committed. The "
    "PREDICTOR is not: no committed artefact in this repository has ever "
    "computed, bucketed, gated on or reported the change in H, so the "
    "contrast this study tests has not been seen and its sign was not known "
    "when these constants were fixed. A pre-registered confirmatory claim "
    "about the CHANGE is therefore available, and that is what separates this "
    "study from #1426, which had to run exploratory-only because the sign it "
    "tested was what motivated it. What is NOT available is a claim of an "
    "independent sample. The outcomes are shared, so a finding here and "
    "#1424's null on the LEVEL are not two independent looks at the market, "
    "the effective sample size is set by the same calendar clusters, and the "
    "detection limit is of the same order. Read a finding here as evidence "
    "about a NEW predictor on OLD rows.")

CONTRACT_REPORT_BASENAME = "hurst_gate_calibration.md"
CONTRACT_PATH_CLAIMED = False
SIBLING_DEFERRAL = (1428,)
DEFERRING_SIBLINGS = (1426,)

CONTRACT_PATH_STATEMENT = (
    "CONTRACT PATH: this study DEFERS, and it would defer on a positive "
    "finding exactly as on a null. `hurst_gate_calibration.md` is the "
    "live-evidence path cited by `scheduler/hurst_gate.go`, "
    "`docs/ARCHITECTURE.md` and #1412's Stage 0, and it must describe the "
    "quantity the shipped gate actually reads. `scheduler/hurst_gate.go` "
    "reads the LEVEL of H at entry and never its change, so nothing measured "
    "here can calibrate it: a positive result licenses a follow-up DESIGN, "
    "never a threshold. `hurst_1424_gate_resolution.py` keeps the path, its "
    "script and tests are untouched by this work, and this study's `main` "
    "refuses that path unconditionally. #1426 defers it for its own reason, "
    "which is that it is exploratory-only. The supersede clause therefore "
    "passes to #1428, the only remaining study that scores the sizing form "
    "the shipped gate implements, and at most one study may claim it.")

NO_PROMOTION_SENTENCE = (
    "This study ships NO threshold and can recommend NO configuration, "
    "whatever it finds: the quantity it scores is not the quantity "
    "`scheduler/hurst_gate.go` reads, so `decide_recommendation` has no "
    "branch that promotes one and this module defines no configuration "
    "verdict of its own.")

KEY_RISK_PREDICTION = (
    "The pre-registered prediction is INCONCLUSIVE, and the reason is a power "
    "reason rather than a market one. Effective N here is set by independent "
    "CALENDAR CLUSTERS rather than rows, and this study scores the same "
    "calendar #1424 and #1426 scored MINUS whatever the deeper warm-up drops, "
    "so its two-sided detection limit on the confirmatory family can only be "
    "at or above the 0.013 efficiency units #1426 measured. Nothing about a "
    "difference of H gives a prior reason to expect a separation several "
    "times larger than the level's -0.005. The prediction is therefore that "
    "the separation lands below the limit and the validity gate FAILS, which "
    "carries NO bound in either direction. It is a PREDICTION and not a "
    "requirement, and the falsifiable half is the limit: if the measured "
    "limit comes back BELOW 0.013 on these rows, this prediction's stated "
    "mechanism was wrong. The machinery below decides the verdict either way.")

DEGENERATE_LIMIT_DISCLOSURE = (
    "DEGENERATE DETECTION LIMIT, disclosed rather than quoted as "
    "corroboration. The injection search walks a grid upward from 0 and stops "
    "at the first point that clears the Benjamini-Hochberg bar, so it returns "
    "0.000 whenever the ZERO-injection contrast ALREADY clears it. A limit of "
    "0.000 is therefore a RESTATEMENT of the rejection and not an independent "
    "measurement of this design's resolution, and the validity gate passes "
    "TRIVIALLY on it. That gate exists to tell a null that means something "
    "about the market from a null that only means the design is blind; on a "
    "REJECTION it adds nothing, and this report says so rather than printing "
    "the 0.000 as a second piece of evidence. The nearest NON-degenerate "
    "resolution figures this run does measure are the pooled limits and the "
    "continuity target's limit, and both are reported beside it.")

CONSTRUCTION_CAVEAT = (
    "CONSTRUCTION CAVEAT on any positive result, stated because it is the "
    "first thing a reader should attack. The PRIMARY TARGET is signed "
    "fixed-horizon efficiency: net displacement divided by summed absolute "
    "path over the next 96 hours, signed by the trade's own side. Its "
    "MAGNITUDE is a measure of forward TRENDINESS. The predictor is the "
    "change in a TRAILING persistence estimate. Trendiness is autocorrelated, "
    "so a rising trailing persistence that predicts a higher forward "
    "efficiency has a route from predictor to target running through the "
    "shared construction rather than through any tradeable edge. The "
    "CONTINUITY target, net return, does not share that construction, and it "
    "is the check. This study reports both side by side and never collapses "
    "them: a separation on the primary target while net return stays inside "
    "its own detection limit is exactly the pattern this caveat predicts, and "
    "it is evidence about a MECHANISM rather than about money.")

VERDICT_CHANGE_SORTS = "change_sorts"
VERDICT_RESOLVED_NULL = "resolved_null"
VERDICT_INCONCLUSIVE = "inconclusive"
VERDICT_LABELS = {
    VERDICT_CHANGE_SORTS: "THE CHANGE IN HURST SORTS THESE TRADES",
    VERDICT_RESOLVED_NULL: "NO SORTING IN EITHER DIRECTION AT OR ABOVE THE "
                           "MEASURED LIMIT",
    VERDICT_INCONCLUSIVE: "INCONCLUSIVE",
}

MODE_OK = study1426.MODE_OK
MODE_BELOW_LIMIT = study1426.MODE_BELOW_LIMIT
MODE_UNRESOLVABLE = study1426.MODE_UNRESOLVABLE
MODE_NO_SEPARATION = study1426.MODE_NO_SEPARATION

STAGE0_EXCLUSION = (
    "This study renders a joint ADX x change-in-H table for description only "
    "and takes NO Stage 0 verdict from it. #1412's Stage 0 question is "
    "defined on the LEVEL of H, #1424 and #1426 discharged it there, and "
    "re-answering it against a different predictor would put a verdict on the "
    "record that is not comparable with the one already there. No Stage 0 "
    "verdict function is called from this module and a test pins that.")

_DEFAULT_JSON_OUT = os.path.join(_THIS_DIR, "hurst_1427_change_sort.json")
_DEFAULT_REPORT_OUT = os.path.join(_THIS_DIR, "hurst_1427_change_sort.md")
_CONTRACT_REPORT_OUT = os.path.join(_THIS_DIR, CONTRACT_REPORT_BASENAME)


def _delta_edge_label(value) -> str:
    return f"{float(value):+.2f}"


def _delta_bucket_names(edges: Sequence[float]) -> tuple:
    names = [f"<{_delta_edge_label(edges[0])}"]
    for lo, hi in zip(edges, edges[1:]):
        names.append(f"{_delta_edge_label(lo)}..{_delta_edge_label(hi)}")
    names.append(f">={_delta_edge_label(edges[-1])}")
    return tuple(names)


def recentre(level_value) -> float:
    return round(float(level_value) - LEVEL_ORIGIN, 6)


DELTA_EDGES = tuple(recentre(e) for e in LEVEL_EDGES)
DELTA_BUCKETS = _delta_bucket_names(DELTA_EDGES) + (BUCKET_NAN,)
DELTA_GATE_PAIRS = {
    family: tuple((recentre(arm), recentre(disarm))
                  for arm, disarm in GATE_PAIRS[family])
    for family in FAMILIES
}
JOINT_DELTA_BUCKETS = (
    f"<{_delta_edge_label(DELTA_EDGES[0])}",
    f"{_delta_edge_label(DELTA_EDGES[0])}..{_delta_edge_label(DELTA_EDGES[-1])}",
    f">{_delta_edge_label(DELTA_EDGES[-1])}",
    BUCKET_NAN,
)

BUCKET_SEPARATOR_NOTE = (
    "The change buckets use `..` where #1410's level buckets use a hyphen, "
    "because a difference can be negative and `-0.05--0.00` is ambiguous. The "
    "edges themselves are #1410's, re-centred; only the rendering differs.")


def _assert_level_landmarks_are_1410s() -> None:
    probes = [LEVEL_EDGES[0] - 0.05] + list(LEVEL_EDGES)
    labels = tuple(bucket_label(p) for p in probes)
    if labels != tuple(BUCKETS[:-1]):
        raise AssertionError(
            f"#1410's bucket_label no longer cuts at {LEVEL_EDGES}: probing "
            f"{probes} gave {labels}, expected {tuple(BUCKETS[:-1])}. The "
            f"re-centring rule is derived from those edges; re-derive it "
            f"deliberately before trusting this study.")
    for edge in LEVEL_EDGES:
        if bucket_label(edge - 1e-9) == bucket_label(edge):
            raise AssertionError(
                f"#1410's bucket_label has no half-open boundary at {edge}; "
                f"the re-centring rule assumes one")
    if len(DELTA_BUCKETS) != len(BUCKETS):
        raise AssertionError(
            f"re-centred buckets {DELTA_BUCKETS} do not mirror {BUCKETS}")
    for family in FAMILIES:
        if len(DELTA_GATE_PAIRS[family]) != len(GATE_PAIRS[family]):
            raise AssertionError(
                f"re-centred gate pairs for {family} do not mirror "
                f"{GATE_PAIRS[family]}")
        for arm, disarm in DELTA_GATE_PAIRS[family]:
            validate_gate_pair(arm, disarm, FAMILY_SENSE[family])


def _recentre_gate_config_id(config_id: str) -> str:
    parts = str(config_id).split(CONFIG_ID_SEP)
    if len(parts) != 5 or parts[1] != "gate":
        raise ValueError(
            f"{config_id!r} is not a gate config id this study can re-centre")
    family = parts[0]
    hurst_window = int(parts[2][1:])
    arm = recentre(float(parts[3][len("arm"):]))
    disarm = recentre(float(parts[4][len("dis"):]))
    return gate_config_id(family, hurst_window, arm, disarm)


LEVEL_PRIMARY_CONFIG_ID = study1424.PRIMARY_CONFIG_ID
PRIMARY_CONFIG_ID = _recentre_gate_config_id(LEVEL_PRIMARY_CONFIG_ID)
PRIMARY_CONFIG_IDS = (PRIMARY_CONFIG_ID,)
PRIMARY_FAMILY_SIZE = len(PRIMARY_CONFIG_IDS)
PRIMARY_FAMILY = PRIMARY_CONFIG_ID.split(CONFIG_ID_SEP)[0]
PRIMARY_HURST_WINDOW = int(PRIMARY_CONFIG_ID.split(CONFIG_ID_SEP)[2][1:])
PRIMARY_ARM = float(PRIMARY_CONFIG_ID.split(CONFIG_ID_SEP)[3][len("arm"):])
PRIMARY_DISARM = float(PRIMARY_CONFIG_ID.split(CONFIG_ID_SEP)[4][len("dis"):])


def delta_lookback_bars(hurst_window) -> int:
    return max(1, int(hurst_window) // DELTA_LOOKBACK_DENOMINATOR)


PRIMARY_LOOKBACK_BARS = delta_lookback_bars(PRIMARY_HURST_WINDOW)

PRIMARY_HYPOTHESIS_STATEMENT = (
    f"PINNED HYPOTHESIS, one and only one: `{PRIMARY_CONFIG_ID}`. It is the "
    f"change in the {PRIMARY_HURST_WINDOW}-bar rolling Hurst exponent over "
    f"{PRIMARY_LOOKBACK_BARS} bars, gated with hysteresis that arms at or "
    f"above {PRIMARY_ARM:+g} and disarms below {PRIMARY_DISARM:+g}, on the "
    f"`{PRIMARY_FAMILY}` family. No sweep on the change series precedes it "
    f"and nothing in this study's data chose it: it is "
    f"`{LEVEL_PRIMARY_CONFIG_ID}`, #1424's own pinned hypothesis, put through "
    f"the re-centring rule. #1424 derived that pin mechanically as the argmin "
    f"raw p over the committed #1410 grid and re-derives it at run time with "
    f"a hard assert; this study re-derives it the same way and then "
    f"re-centres it. The Benjamini-Hochberg denominator for the confirmatory "
    f"family is therefore {PRIMARY_FAMILY_SIZE} and its rank-1 bar is alpha "
    f"itself.")


def _assert_pin_is_recentred_1424() -> None:
    if (PRIMARY_ARM, PRIMARY_DISARM) not in DELTA_GATE_PAIRS[PRIMARY_FAMILY]:
        raise AssertionError(
            f"the re-centred pin {PRIMARY_CONFIG_ID!r} carries "
            f"{(PRIMARY_ARM, PRIMARY_DISARM)}, which is not one of this "
            f"study's swept pairs {DELTA_GATE_PAIRS[PRIMARY_FAMILY]}")
    if PRIMARY_HURST_WINDOW not in HURST_WINDOWS:
        raise AssertionError(
            f"the re-centred pin names window {PRIMARY_HURST_WINDOW}, which "
            f"is not one of {HURST_WINDOWS}")
    if PRIMARY_FAMILY not in FAMILIES:
        raise AssertionError(
            f"the re-centred pin names family {PRIMARY_FAMILY!r}, which is "
            f"not one of {FAMILIES}")


_assert_level_landmarks_are_1410s()
_assert_pin_is_recentred_1424()


anti_signal_side = study1422.anti_signal_side
joint_h_bucket = study1422.joint_h_bucket
JOINT_H_BUCKETS = study1422.JOINT_H_BUCKETS

_LEVEL_TO_DELTA_BUCKET = dict(zip(BUCKETS, DELTA_BUCKETS))
_LEVEL_TO_JOINT_DELTA_BUCKET = dict(zip(JOINT_H_BUCKETS, JOINT_DELTA_BUCKETS))

if _LEVEL_TO_DELTA_BUCKET.get(BUCKET_NAN) != BUCKET_NAN:
    raise AssertionError(
        "the re-centred bucket map must carry NaN through unchanged")
if _LEVEL_TO_JOINT_DELTA_BUCKET.get(BUCKET_NAN) != BUCKET_NAN:
    raise AssertionError(
        "the re-centred joint bucket map must carry NaN through unchanged")


def delta_bucket_label(dh) -> str:
    if dh is None:
        return BUCKET_NAN
    value = float(dh)
    if not math.isfinite(value):
        return BUCKET_NAN
    return _LEVEL_TO_DELTA_BUCKET[bucket_label(value + LEVEL_ORIGIN)]


def joint_delta_bucket(dh) -> str:
    if dh is None:
        return BUCKET_NAN
    value = float(dh)
    if not math.isfinite(value):
        return BUCKET_NAN
    return _LEVEL_TO_JOINT_DELTA_BUCKET[joint_h_bucket(value + LEVEL_ORIGIN)]


def delta_size_multiplier(dh, sense: str, gain: float) -> float:
    if dh is None:
        return size_multiplier(None, sense, gain)
    value = float(dh)
    if not math.isfinite(value):
        return size_multiplier(value, sense, gain)
    return size_multiplier(value + LEVEL_ORIGIN, sense, gain)


def delta_anti_signal_side(dh, sense: str) -> bool:
    return anti_signal_side(float(dh) + LEVEL_ORIGIN, sense)


def delta_required_lead_bars(hurst_window: int) -> int:
    return required_lead_bars(hurst_window) + delta_lookback_bars(hurst_window)


def delta_warmup_audit(leads: dict, hurst_windows: Sequence[int]) -> dict:
    hurst_only = warmup_audit(leads, hurst_windows)
    required = max((delta_required_lead_bars(hw) for hw in hurst_windows),
                   default=0)
    short = sorted(k for k, v in leads.items() if int(v) < required)
    return {
        "required_bars": required,
        "hurst_only_required_bars": hurst_only["required_bars"],
        "lookback_denominator": DELTA_LOOKBACK_DENOMINATOR,
        "warmup_margin_bars": WARMUP_MARGIN_BARS,
        "components": {
            str(hw): {
                "hurst_window": int(hw),
                "lookback_bars": delta_lookback_bars(hw),
                "margin_bars": WARMUP_MARGIN_BARS,
                "required_bars": delta_required_lead_bars(hw),
            }
            for hw in sorted(int(h) for h in hurst_windows)
        },
        "lead_bars": {k: int(leads[k]) for k in sorted(leads)},
        "min_lead_bars": min((int(v) for v in leads.values()), default=0),
        "sufficient": not short,
        "insufficient_datasets": short,
        "hurst_only_sufficient": hurst_only["sufficient"],
        "hurst_only_insufficient_datasets": hurst_only["insufficient_datasets"],
    }


def delta_first_needed(index, first_needed, lookback: int):
    idx = pd.Index(index)
    if len(idx) == 0:
        return pd.Timestamp(first_needed)
    pos = int(idx.searchsorted(pd.Timestamp(first_needed), side="left"))
    pos = max(0, min(len(idx) - 1, pos - int(lookback)))
    return pd.Timestamp(idx[pos])


def rolling_hurst_for_delta(close: pd.Series, window: int, lookback: int,
                            first_needed: Optional[pd.Timestamp] = None
                            ) -> pd.Series:
    if int(lookback) < 1:
        raise ValueError(f"delta lookback must be >= 1, got {lookback}")
    if first_needed is None:
        return rolling_hurst(close, window, first_needed=None)
    return rolling_hurst(
        close, window,
        first_needed=delta_first_needed(close.index, first_needed, lookback))


def delta_hurst_series(rolling: pd.Series, lookback: int) -> pd.Series:
    if int(lookback) < 1:
        raise ValueError(f"delta lookback must be >= 1, got {lookback}")
    out = rolling.astype(float) - rolling.astype(float).shift(int(lookback))
    return out.rename(f"dhurst_{lookback}")


def coverage_audit(frames: dict, window_names: Sequence[str],
                   hurst_windows: Sequence[int]) -> dict:
    need_lead = max((delta_required_lead_bars(hw) for hw in hurst_windows),
                    default=0)
    hurst_only_lead = max((required_lead_bars(hw) for hw in hurst_windows),
                          default=0)
    last_bars = [f.index[-1] for f in frames.values()
                 if f is not None and not f.empty]
    reference_last = max(last_bars) if last_bars else None
    cells = {}
    dropped = []
    n_unowned = 0
    for dataset, frame in sorted(frames.items()):
        exchange_id, symbol, timeframe = dataset
        key = dataset_key(qualified_symbol(exchange_id, symbol), timeframe)
        own = owned_windows(dataset, window_names)
        n_unowned += len(window_names) - len(own)
        for wname in own:
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
                    reason = (
                        f"lead {lead} bars before {start} < required "
                        f"{need_lead} (Hurst window {hurst_only_lead} + "
                        f"lookback {need_lead - hurst_only_lead})")
                elif in_window <= 0:
                    ok = False
                    reason = f"no bars inside [{start}, {end or 'latest'})"
                elif want > 0 and in_window < MIN_WINDOW_BAR_FRACTION * want:
                    ok = False
                    reason = (f"only {in_window} of ~{want} expected bars "
                              f"inside [{start}, {end or 'latest'}) "
                              f"({in_window / want:.0%} < "
                              f"{MIN_WINDOW_BAR_FRACTION:.0%}) - data gap")
            cells[f"{key}|{wname}"] = ok
            if not ok:
                dropped.append({"dataset": key, "window": wname,
                                "reason": reason})
    return {
        "required_lead_bars": need_lead,
        "hurst_only_required_lead_bars": hurst_only_lead,
        "lookback_lead_bars": need_lead - hurst_only_lead,
        "min_window_bar_fraction": MIN_WINDOW_BAR_FRACTION,
        "reference_last_bar": (None if reference_last is None
                               else str(reference_last)),
        "n_cells": len(cells),
        "n_kept": int(sum(1 for v in cells.values() if v)),
        "n_dropped": len(dropped),
        "n_unowned": n_unowned,
        "cells": cells,
        "dropped": dropped,
    }


def build_leg(reg, family: str, exemplar: str, dataset: tuple,
              window_name: str, full: pd.DataFrame, delta_by_window: dict,
              adx_stamp: pd.Series, verify_mirror: bool = True
              ) -> Optional[dict]:
    from eval_windows import run_leg

    exchange_id, symbol, timeframe = dataset
    qsym = qualified_symbol(exchange_id, symbol)
    window = WINDOWS[window_name]
    overrides = EXEMPLAR_CLOSE_OVERRIDES.get(exemplar, {})
    df = slice_window(full, window)
    if df.empty:
        return None
    sense = FAMILY_SENSE[family]

    ungated = build_leg_run_arm(reg, exemplar, symbol, timeframe, df, None,
                                overrides)
    if ungated is None:
        return None

    mirror_ok = None
    if verify_mirror:
        reference = run_leg(
            reg, exemplar, None, symbol, timeframe, window,
            capital=DEFAULT_CAPITAL,
            close_strategies=overrides.get("close_strategies"),
            stop_loss_atr_mult=overrides.get("stop_loss_atr_mult"),
            trailing_stop_atr_mult=overrides.get("trailing_stop_atr_mult"),
            keep_trades=True,
            exchange_id=exchange_id)
        mirror_ok = reference is not None and all(
            reference.get(k) == ungated.get(k) for k in _MIRRORED_LEG_KEYS)
        if not mirror_ok:
            raise AssertionError(
                f"gated-arm mirror diverged from eval_windows.run_leg for "
                f"{exemplar} {qsym} {timeframe} {window_name}: "
                f"{ {k: (reference or {}).get(k) for k in _MIRRORED_LEG_KEYS} }"
                f" vs { {k: ungated.get(k) for k in _MIRRORED_LEG_KEYS} }")

    index_keys = [str(ts) for ts in df.index]
    key_pos = {k: i for i, k in enumerate(index_keys)}
    closes = df["close"].to_numpy(dtype=float)
    k_bars = horizon_bars(timeframe)

    stamps = {}
    decisions = {}
    for hw, delta in delta_by_window.items():
        stamps[hw] = entry_stamp_series(delta).reindex(
            df.index).to_numpy(dtype=float)
        decisions[hw] = decision_series(delta).reindex(
            df.index).to_numpy(dtype=float)
    adx_vals = adx_stamp.reindex(df.index).to_numpy(dtype=float)

    armed_signal_bar = {}
    armed_fill_bar = {}
    for hw in delta_by_window:
        for arm, disarm in DELTA_GATE_PAIRS[family]:
            cid = gate_config_id(family, hw, arm, disarm)
            mask = hysteresis_mask(decisions[hw], arm, disarm, sense)
            armed_signal_bar[cid] = mask
            shifted = np.empty_like(mask)
            shifted[0] = bool(GATE_INITIAL_ARMED)
            shifted[1:] = mask[:-1]
            armed_fill_bar[cid] = shifted

    cohort = cell_cohort(exchange_id, symbol, timeframe, window_name)
    trades = []
    n_horizon_excluded = 0
    for sample in ungated.get("trade_samples") or []:
        key = str(sample["entry_date"])
        pos = key_pos.get(key)
        if pos is None:
            raise AssertionError(
                f"trade entry_date {key!r} is not a bar of the {window_name} "
                f"slice for {exemplar} {qsym} {timeframe}")
        try:
            exit_ns = int(pd.Timestamp(sample["exit_date"]).value)
        except (ValueError, TypeError):
            exit_ns = None
        eff = signed_efficiency(closes, pos, k_bars,
                                trade_direction(sample.get("side")))
        if eff is None:
            n_horizon_excluded += 1
        trades.append({
            "strategy": exemplar,
            "exchange": exchange_id,
            "symbol": qsym,
            "base_symbol": symbol,
            "timeframe": timeframe,
            "window": window_name,
            "cohort": cohort,
            "entry_date": key,
            "entry_ns": int(pd.Timestamp(key).value),
            "exit_ns": exit_ns,
            "pnl_pct_net": float(sample["pnl_pct_net"]),
            "efficiency": None if eff is None else round(float(eff), 6),
            "adx": (None if not math.isfinite(adx_vals[pos])
                    else float(adx_vals[pos])),
            "dh": {hw: (None if not math.isfinite(stamps[hw][pos])
                        else float(stamps[hw][pos]))
                   for hw in delta_by_window},
            "armed": {cid: bool(armed_fill_bar[cid][pos])
                      for cid in armed_fill_bar},
        })

    gated = {}
    for cid, mask in armed_signal_bar.items():
        arm_leg = build_leg_run_arm(reg, exemplar, symbol, timeframe, df, mask,
                                    overrides)
        gated[cid] = _leg_metrics(arm_leg)

    return {
        "family": family,
        "strategy": exemplar,
        "exchange": exchange_id,
        "symbol": qsym,
        "base_symbol": symbol,
        "timeframe": timeframe,
        "dataset": dataset_key(qsym, timeframe),
        "window": window_name,
        "cohort": cohort,
        "bars": int(len(df)),
        "horizon_bars": k_bars,
        "n_horizon_excluded": n_horizon_excluded,
        "mirror_verified": mirror_ok,
        "ungated": _leg_metrics(ungated),
        "gated": gated,
        "trades": trades,
    }


def bucket_tables(trades: Sequence[dict], hurst_window: int) -> dict:
    by_bucket = {b: {"ret": [], "eff": []} for b in DELTA_BUCKETS}
    for t in trades:
        row = by_bucket[delta_bucket_label((t.get("dh") or {}).get(hurst_window))]
        row["ret"].append(t["pnl_pct_net"])
        if t.get("efficiency") is not None:
            row["eff"].append(float(t["efficiency"]))
    out = {}
    for bucket, vals in by_bucket.items():
        rets = vals["ret"]
        effs = vals["eff"]
        total_return, max_dd = compound_equity(rets)
        out[bucket] = {
            "trades": len(rets),
            "win_rate_pct": win_rate(rets),
            "mean_pnl_pct_net": round(float(np.mean(rets)), 6) if rets else None,
            "median_pnl_pct_net": (round(float(np.median(rets)), 6)
                                   if rets else None),
            "compounded_return_pct": total_return,
            "trade_seq_max_dd_pct": max_dd,
            "chop_loss_pct": chop_loss(rets),
            "efficiency_trades": len(effs),
            "mean_efficiency": round(float(np.mean(effs)), 6) if effs else None,
        }
    return out


def joint_adx_delta_table(trades: Sequence[dict], hurst_window: int) -> dict:
    cells = {}
    for a in JOINT_ADX_BUCKETS:
        for d in JOINT_DELTA_BUCKETS:
            cells[f"{a}|{d}"] = []
    for t in trades:
        a = joint_adx_bucket(t.get("adx"))
        d = joint_delta_bucket((t.get("dh") or {}).get(hurst_window))
        cells[f"{a}|{d}"].append(float(t["pnl_pct_net"]))
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


def _window_rows_size(legs: Sequence[dict], family: str, cohort: str,
                      hurst_window: int, gain: float) -> dict:
    sense = FAMILY_SENSE[family]
    rows = {}
    own = [lg for lg in legs
           if lg["family"] == family and lg["cohort"] == cohort]
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
            mults = [delta_size_multiplier((t.get("dh") or {}).get(hurst_window),
                                           sense, gain)
                     for t in lg["trades"]]
            base_ret, base_dd = compound_equity(rets)
            sized_ret, sized_dd = compound_equity(rets, mults)
            dd_deltas.append(abs(sized_dd) - abs(base_dd))
            chop_deltas.append(
                chop_loss([m * r for m, r in zip(mults, rets)])
                - chop_loss(rets))
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


def _sweep_grid(cohort: str, hurst_windows: Sequence[int]) -> list:
    grid = []
    for family in FAMILIES:
        for hw in hurst_windows:
            for arm, disarm in DELTA_GATE_PAIRS[family]:
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


def build_configs(legs: Sequence[dict], pooled: dict,
                  hurst_windows: Sequence[int], rho_by_symbol: dict,
                  n_perm: int, seed: int) -> list:
    configs = []
    for cohort in (COHORT_PRIMARY, COHORT_EXPLORATORY):
        for family, mode, hw, arm, disarm, gain in _sweep_grid(cohort,
                                                               hurst_windows):
            sense = FAMILY_SENSE[family]
            trades = [t for t in (pooled.get(family) or [])
                      if t["cohort"] == cohort]
            cfg = _config_shell(family, cohort, mode, hw, arm, disarm, gain)
            cid = cfg["config_id"]
            cfg["lookback_bars"] = delta_lookback_bars(hw)
            if mode == "gate":
                sub = [t for t in trades if cid in t["armed"]]
            else:
                sub = list(trades)
            sub, n_missing_target = _target_rows(sub)
            idx, excluded = usable_cluster_rows(sub)
            n_excluded = len(sub) - len(idx)
            sub = [sub[i] for i in idx]
            values = [float(t["efficiency"]) for t in sub]
            returns = [float(t["pnl_pct_net"]) for t in sub]
            if mode == "gate":
                suppressed = [not t["armed"][cid] for t in sub]
                cfg["p_raw"] = two_sided_permutation_pvalue_group_diff(
                    values, suppressed, n_perm=n_perm, seed=seed)
                cluster = two_sided_cluster_permutation_pvalue_group_diff(
                    sub, values, suppressed, n_perm=n_perm, seed=seed)
                cfg["p_raw_return"] = two_sided_permutation_pvalue_group_diff(
                    returns, suppressed, n_perm=n_perm, seed=seed)
                cfg["p_cluster_return"] = \
                    two_sided_cluster_permutation_pvalue_group_diff(
                        sub, returns, suppressed, n_perm=n_perm,
                        seed=seed).get("p")
                sup_rows = [t for t, s in zip(sub, suppressed) if s]
                kept_rows = [t for t, s in zip(sub, suppressed) if not s]
                cfg["separation"] = _separation(values, suppressed)
                cfg["separation_return"] = _separation(returns, suppressed)
                cfg["windows"] = _window_rows_gate(legs, family, cohort, cid)
            else:
                mults = [delta_size_multiplier((t.get("dh") or {}).get(hw),
                                               sense, gain)
                         for t in sub]
                cfg["p_raw"] = two_sided_permutation_pvalue_weighted(
                    values, mults, n_perm=n_perm, seed=seed)
                cluster = two_sided_cluster_permutation_pvalue_weighted(
                    sub, values, mults, n_perm=n_perm, seed=seed)
                cfg["p_raw_return"] = two_sided_permutation_pvalue_weighted(
                    returns, mults, n_perm=n_perm, seed=seed)
                cfg["p_cluster_return"] = \
                    two_sided_cluster_permutation_pvalue_weighted(
                        sub, returns, mults, n_perm=n_perm, seed=seed).get("p")
                sup_rows = [t for t, m in zip(sub, mults) if m < 1.0]
                kept_rows = [t for t, m in zip(sub, mults) if m >= 1.0]
                down = [m < 1.0 for m in mults]
                cfg["separation"] = _separation(values, down)
                cfg["separation_return"] = _separation(returns, down)
                cfg["windows"] = _window_rows_size(legs, family, cohort, hw,
                                                   gain)
            cfg["p_cluster"] = cluster.get("p")
            cfg["cluster_draws"] = cluster.get("n_draws")
            cfg["cluster_excluded_datasets"] = excluded
            cfg["cluster_excluded_trades"] = n_excluded
            cfg["cluster_offset_range"] = cluster.get("offset_range")
            cfg["cluster_distinct_offsets"] = cluster.get("n_distinct_offsets")
            cfg["cluster_reason"] = cluster.get("reason")
            cfg["n_pooled_trades"] = len(sub)
            cfg["n_missing_target"] = n_missing_target
            cfg["n_suppressed"] = len(sup_rows)
            cfg["n_kept"] = len(kept_rows)
            cfg["n_pooled_effective"] = effective_n(sub, rho_by_symbol)
            cfg["n_suppressed_effective"] = effective_n(sup_rows, rho_by_symbol)
            cfg["n_kept_effective"] = effective_n(kept_rows, rho_by_symbol)
            configs.append(cfg)
    return configs


def apply_bh_by_cohort(configs: Sequence[dict], alpha: float = ALPHA) -> None:
    for cohort in (COHORT_PRIMARY, COHORT_EXPLORATORY):
        own = [c for c in configs if c.get("cohort") == cohort]
        for cfg in own:
            cfg["bh_reject"] = False
        testable = [c for c in own if c.get("p_cluster") is not None]
        if not testable:
            continue
        flags = benjamini_hochberg([c["p_cluster"] for c in testable],
                                   alpha=alpha, family_size=len(own))
        for cfg, flag in zip(testable, flags):
            cfg["bh_reject"] = bool(flag)


def measure_detection_limits(pooled: dict, hurst_windows: Sequence[int],
                             n_perm: int, seed: int) -> dict:
    out: dict = {"by_family_cluster": {}, "by_family_cluster_return": {},
                 "by_family_separation": {}, "by_family_separation_return": {},
                 "by_family_cluster_p0": {}, "by_family_cluster_return_p0": {},
                 "by_family_n": {}}
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
        sense = FAMILY_SENSE[family]
        keep, values, returns, mask = [], [], [], []
        for t in rows:
            dh = (t.get("dh") or {}).get(hw)
            if dh is None or not math.isfinite(float(dh)):
                continue
            if t.get("efficiency") is None:
                continue
            keep.append(t)
            values.append(float(t["efficiency"]))
            returns.append(float(t["pnl_pct_net"]))
            mask.append(delta_anti_signal_side(float(dh), sense))
        return keep, values, returns, mask

    specs = (
        ("1410", None, True, 30),
        ("primary", COHORT_PRIMARY, False, PRIMARY_FAMILY_SIZE),
        ("exploratory", COHORT_EXPLORATORY, False, 30),
    )
    by_pool: dict = {}
    by_pool_return: dict = {}
    for label, cohort, only_1410, family_size in specs:
        rows_all, vals_all, rets_all, mask_all, fams_all = [], [], [], [], []
        for family in FAMILIES:
            rows, vals, rets, mask = _split(_pool(family, cohort, only_1410),
                                            family)
            rows_all += rows
            vals_all += vals
            rets_all += rets
            mask_all += mask
            fams_all += [family] * len(rows)
            if label == "primary":
                fam_idx, _ = usable_cluster_rows(rows)
                fam_rows = [rows[i] for i in fam_idx]
                fam_vals = [vals[i] for i in fam_idx]
                fam_rets = [rets[i] for i in fam_idx]
                fam_mask = [mask[i] for i in fam_idx]
                out["by_family_cluster"][family] = \
                    two_sided_min_detectable_effect_eff(
                        fam_rows, fam_vals, fam_mask, family_size, cluster=True,
                        n_perm=n_perm, seed=seed)
                out["by_family_cluster_return"][family] = \
                    two_sided_min_detectable_effect_pp(
                        fam_rows, fam_rets, fam_mask, family_size, cluster=True,
                        n_perm=n_perm, seed=seed)
                out["by_family_separation"][family] = _separation(fam_vals,
                                                                 fam_mask)
                out["by_family_separation_return"][family] = _separation(
                    fam_rets, fam_mask)
                out["by_family_n"][family] = len(fam_rows)
                if fam_rows and 0 < int(np.sum(fam_mask)) < len(fam_mask):
                    out["by_family_cluster_p0"][family] = (
                        two_sided_cluster_permutation_pvalue_group_diff(
                            fam_rows, fam_vals, fam_mask, n_perm=n_perm,
                            seed=seed).get("p"))
                    out["by_family_cluster_return_p0"][family] = (
                        two_sided_cluster_permutation_pvalue_group_diff(
                            fam_rows, fam_rets, fam_mask, n_perm=n_perm,
                            seed=seed).get("p"))
                else:
                    out["by_family_cluster_p0"][family] = None
                    out["by_family_cluster_return_p0"][family] = None

        idx, _ = usable_cluster_rows(rows_all)
        rows_all = [rows_all[i] for i in idx]
        vals_all = [vals_all[i] for i in idx]
        rets_all = [rets_all[i] for i in idx]
        mask_all = [mask_all[i] for i in idx]
        fams_all = [fams_all[i] for i in idx]

        pool_obs: dict = {}
        pool_obs_ret: dict = {}
        for family in FAMILIES:
            own = [i for i, value in enumerate(fams_all) if value == family]
            sep = _separation([vals_all[i] for i in own],
                              [mask_all[i] for i in own])
            if sep is not None:
                pool_obs[f"{family}|{hw}"] = sep
            sep_ret = _separation([rets_all[i] for i in own],
                                  [mask_all[i] for i in own])
            if sep_ret is not None:
                pool_obs_ret[f"{family}|{hw}"] = sep_ret
        by_pool[label] = pool_obs
        by_pool_return[label] = pool_obs_ret
        out[f"pooled_{label}_cluster"] = two_sided_min_detectable_effect_eff(
            rows_all, vals_all, mask_all, family_size, cluster=True,
            n_perm=n_perm, seed=seed)
        out[f"pooled_{label}_free"] = two_sided_min_detectable_effect_eff(
            rows_all, vals_all, mask_all, family_size, cluster=False,
            n_perm=n_perm, seed=seed)
        out[f"pooled_{label}_cluster_return"] = \
            two_sided_min_detectable_effect_pp(
                rows_all, rets_all, mask_all, family_size, cluster=True,
                n_perm=n_perm, seed=seed)
        out[f"pooled_{label}_n"] = len(rows_all)
        if rows_all and 0 < int(np.sum(mask_all)) < len(mask_all):
            out[f"pooled_{label}_cluster_p0"] = (
                two_sided_cluster_permutation_pvalue_group_diff(
                    rows_all, vals_all, mask_all, n_perm=n_perm, seed=seed)
                .get("p"))
            out[f"pooled_{label}_free_p0"] = \
                two_sided_permutation_pvalue_group_diff(
                    vals_all, mask_all, n_perm=n_perm, seed=seed)
            out[f"pooled_{label}_cluster_return_p0"] = (
                two_sided_cluster_permutation_pvalue_group_diff(
                    rows_all, rets_all, mask_all, n_perm=n_perm, seed=seed)
                .get("p"))
        else:
            out[f"pooled_{label}_cluster_p0"] = None
            out[f"pooled_{label}_free_p0"] = None
            out[f"pooled_{label}_cluster_return_p0"] = None

    out["observed_separation_by_pool"] = by_pool
    out["observed_separation_pp_by_pool"] = by_pool_return
    out["primary_target"] = PRIMARY_TARGET
    out["continuity_target"] = CONTINUITY_TARGET
    out["horizon_hours"] = HORIZON_HOURS
    out["two_sided"] = TWO_SIDED
    out["predictor"] = "delta_hurst"
    out["lookback_bars"] = delta_lookback_bars(hw)
    return out


def limit_is_degenerate(limit) -> bool:
    return limit is not None and float(limit) == 0.0


def validity_gate(mde: dict) -> dict:
    family = PRIMARY_FAMILY
    limit = (mde.get("by_family_cluster") or {}).get(family)
    sep = (mde.get("by_family_separation") or {}).get(family)
    base = {"family": family,
            "n_rows": (mde.get("by_family_n") or {}).get(family),
            "limit_is_degenerate": limit_is_degenerate(limit)}
    if sep is None:
        return dict(base, passed=False, limit=limit, largest_separation=None,
                    mode=MODE_NO_SEPARATION, two_sided=True,
                    reason=(f"the confirmatory family (`{family}`) carries no "
                            f"measurable separation on the change in H"))
    sep = round(float(sep), 6)
    if limit is None:
        return dict(base, passed=False, limit=None, largest_separation=sep,
                    mode=MODE_UNRESOLVABLE, two_sided=True,
                    reason=(f"the confirmatory family (`{family}`) has a "
                            f"two-sided detection limit above "
                            f"{MDE_EFF_GRID_MAX:g} efficiency units, so no "
                            f"effect on the injection grid is resolvable in "
                            f"either direction"))
    passed = bool(abs(sep) >= float(limit))
    return dict(base, passed=passed, limit=round(float(limit), 6),
                largest_separation=sep, reason="", two_sided=True,
                mode=MODE_OK if passed else MODE_BELOW_LIMIT)


def continuity_clause(mde: dict) -> str:
    sep = (mde.get("by_family_separation_return") or {}).get(PRIMARY_FAMILY)
    lim = (mde.get("by_family_cluster_return") or {}).get(PRIMARY_FAMILY)
    p = (mde.get("by_family_cluster_return_p0") or {}).get(PRIMARY_FAMILY)
    if sep is None:
        return ("On the CONTINUITY target those rows carry no measurable "
                "separation, so this run says nothing at all about the "
                "economics.")
    head = (f"On the CONTINUITY target the SAME rows separate by "
            f"{_fmt_signed(sep, 3)} pp of net return (two-sided cluster "
            f"p={_fmt_p(p)})")
    if lim is None:
        return (head + f", and its detection limit sits above "
                       f"{MDE_PP_GRID_MAX:g} pp, so the economic size is "
                       f"UNESTIMATED.")
    if abs(float(sep)) >= float(lim):
        return (head + f" against a measured limit of {_fmt(lim)} pp, which "
                       f"its magnitude clears.")
    return (head + f" against a measured limit of {_fmt(lim)} pp, BELOW that "
                   f"limit, so the economic size is UNESTIMATED. A separation "
                   f"on the bounded-variance primary target is evidence about "
                   f"the MECHANISM and never on its own a licence to ship a "
                   f"gate.")


def predecessor_reference() -> dict:
    out = {}
    for tag, path in (("1424", _JSON_1424), ("1426", _JSON_1426)):
        try:
            with open(path) as fh:
                mde = json.load(fh).get("mde") or {}
        except (OSError, ValueError):
            continue
        out[f"{tag}_family_limit"] = (
            mde.get("by_family_cluster") or {}).get(PRIMARY_FAMILY)
        out[f"{tag}_family_separation"] = (
            mde.get("by_family_separation") or {}).get(PRIMARY_FAMILY)
        out[f"{tag}_pooled_primary_limit"] = mde.get("pooled_primary_cluster")
    return out


def prediction_audit(mde: dict, predecessor: Optional[dict] = None) -> str:
    predecessor = predecessor or {}
    limit = (mde.get("by_family_cluster") or {}).get(PRIMARY_FAMILY)
    prior = predecessor.get("1426_family_limit")
    parts = []
    if limit_is_degenerate(limit):
        parts.append(
            "The confirmatory family's own limit came back 0.000, the "
            "DEGENERATE value the search returns when the zero-injection "
            "contrast already clears the bar, so the prediction's falsifiable "
            "clause cannot be read off it: a degenerate figure is neither "
            "above nor below anything.")
    elif limit is None:
        parts.append(
            f"The confirmatory family's limit came back above "
            f"{MDE_EFF_GRID_MAX:g} efficiency units, so no comparison with "
            f"the predicted floor is available.")
    elif prior is not None:
        parts.append(
            f"The confirmatory family's limit came back {float(limit):.3f} "
            f"against the {float(prior):g} #1426 measured, which the "
            f"prediction said it could not fall below: it landed "
            + ("AT OR ABOVE, as predicted."
               if float(limit) >= float(prior) else "BELOW, so the "
               "prediction's stated mechanism was WRONG."))
    pooled = mde.get("pooled_primary_cluster")
    prior_pooled = predecessor.get("1426_pooled_primary_limit")
    if pooled is not None and prior_pooled is not None:
        parts.append(
            f"The nearest NON-degenerate comparison this run offers is the "
            f"POOLED primary limit: {float(pooled):.3f} here against "
            f"{float(prior_pooled):.3f} in #1426 on the same calendar. "
            + ("It came in BELOW, so the prediction's stated mechanism - same "
               "tape minus whatever the deeper warm-up drops, therefore no "
               "more resolution - was WRONG. It ignored that this study also "
               "changes the SPLIT, from the LEVEL of H to the SIGN of its "
               "change, and a differently balanced split resolves a different "
               "effect size on the same rows."
               if float(pooled) < float(prior_pooled) else
               "It came in at or above, which is what the prediction's "
               "mechanism said would happen."))
    return " ".join(parts)


def _direction_phrase(separation: float) -> str:
    return ("the KEPT trades did better, so entries taken while the change in "
            "H points the hypothesis' way outperformed"
            if float(separation) >= 0 else
            "the SUPPRESSED trades did better, the OPPOSITE of the "
            "hypothesis, so a gate built on the change in H would have HURT")


def confirmatory_p(mde: dict) -> Optional[float]:
    return (mde.get("by_family_cluster_p0") or {}).get(PRIMARY_FAMILY)


def decide_recommendation(configs: Sequence[dict], mde: dict) -> dict:
    gate = validity_gate(mde)
    p_conf = confirmatory_p(mde)
    bar = _rank1_threshold(PRIMARY_FAMILY_SIZE, ALPHA)
    significant = p_conf is not None and float(p_conf) <= bar

    primary = [c for c in configs if c.get("cohort") == COHORT_PRIMARY]
    families = {}
    for family in FAMILIES:
        own = [c for c in primary if c.get("family") == family]
        families[family] = {
            "winner": None,
            "n_tested": len(own),
            "n_passing": sum(1 for c in own if config_verdict(c)[0]),
        }

    sep = gate.get("largest_separation")
    p_text = "untestable" if p_conf is None else f"{float(p_conf):.4f}"
    head = (f"On the confirmatory family (`{gate.get('family', PRIMARY_FAMILY)}"
            f"`, {gate.get('n_rows') or 0} row-matched rows) the TWO-SIDED "
            f"zero-injection cluster p on the change in H is {p_text} against "
            f"a rank-1 Benjamini-Hochberg bar of {bar:g}")

    if significant:
        verdict = VERDICT_CHANGE_SORTS
        detail = (
            f"{head}, so the contrast is REJECTED in a test that could have "
            f"rejected it either way. The CHANGE in the Hurst exponent at "
            f"entry SORTS these trades: the separation is "
            f"{_fmt_signed(sep, 3)} efficiency units, which means "
            f"{_direction_phrase(sep if sep is not None else 0.0)}.")
        if gate["passed"] and gate.get("limit_is_degenerate"):
            detail += (
                " The validity gate PASSES TRIVIALLY here and corroborates "
                "NOTHING: its limit is 0.000 only because the zero-injection "
                "contrast already cleared the bar, so the number restates the "
                "rejection instead of measuring this design's resolution. The "
                "effect SIZE is therefore unestimated on the primary target.")
        elif gate["passed"]:
            detail += (
                f" The validity gate also PASSED on a NON-degenerate limit: "
                f"{gate['limit']:.3f} sits at or below the magnitude of that "
                f"separation, so the effect is one this design can resolve "
                f"rather than one it merely reached significance on.")
        else:
            detail += (
                " The validity gate did NOT pass, so the effect sits below "
                "the size this design resolves. The verdict on the magnitude "
                "stays a POWER statement: treat the significance as fragile "
                "and the effect size as unestimated.")
    elif gate["passed"]:
        verdict = VERDICT_RESOLVED_NULL
        detail = (
            f"{head}, so the contrast is NOT rejected. The validity gate "
            f"PASSED: the two-sided limit on those same rows is "
            f"{gate['limit']:.3f} efficiency units and their separation is "
            f"{_fmt_signed(sep, 3)}, whose magnitude the design can resolve. "
            f"This is a statement about the market and not about the test: "
            f"the change in Hurst at entry does not sort these trades IN "
            f"EITHER DIRECTION at an effect size this design can see.")
    else:
        verdict = VERDICT_INCONCLUSIVE
        if gate["reason"]:
            detail = (f"{head}. The validity gate FAILED: {gate['reason']}, "
                      f"so the run carries no bound in either direction and "
                      f"the verdict is a POWER statement, not a statement "
                      f"about the market.")
        else:
            detail = (
                f"{head}. The validity gate FAILED: the two-sided limit on "
                f"those rows is {gate['limit']:.3f} efficiency units while "
                f"they separate by only {_fmt_signed(sep, 3)}, whose "
                f"magnitude sits BELOW the limit. Nothing that small is "
                f"visible to this design, so the run bounds any sorting "
                f"effect from above at {gate['limit']:.3f} efficiency units "
                f"and says nothing at all about anything smaller. The verdict "
                f"is a POWER statement, not a statement about the market.")

    detail += " " + continuity_clause(mde)

    n_untestable = sum(1 for c in primary if c.get("p_cluster") is None)
    n_significant = sum(1 for c in primary if c.get("bh_reject"))
    tested = sum(v["n_tested"] for v in families.values())

    return {
        "verdict": verdict,
        "families": families,
        "validity_gate": gate,
        "confirmatory_p": p_conf,
        "confirmatory_bar": bar,
        "significant": bool(significant),
        "key_risk_held": bool(gate["passed"]),
        "predictor": "delta_hurst",
        "inference_direction": INFERENCE_DIRECTION,
        "contract_path_claimed": CONTRACT_PATH_CLAIMED,
        "justification": (
            f"{detail} {NO_PROMOTION_SENTENCE} Across the {tested} "
            f"primary-cohort "
            f"{'configuration' if tested == 1 else 'configurations'} swept, "
            f"{n_significant} reached Benjamini-Hochberg significance on the "
            f"two-sided cluster permutation and {n_untestable} were "
            f"untestable; neither count can promote a configuration, because "
            f"this study has no branch that promotes one."
        ).strip(),
    }


def decision_payload(decision: dict) -> dict:
    return {
        "verdict": decision["verdict"],
        "justification": decision["justification"],
        "validity_gate": decision["validity_gate"],
        "confirmatory_p": decision["confirmatory_p"],
        "confirmatory_bar": decision["confirmatory_bar"],
        "significant": decision["significant"],
        "key_risk_held": decision["key_risk_held"],
        "predictor": decision["predictor"],
        "inference_direction": decision["inference_direction"],
        "contract_path_claimed": decision["contract_path_claimed"],
        "families": {f: {"n_tested": d["n_tested"], "n_passing": d["n_passing"],
                         "winner": ((d["winner"] or {}).get("config_id")
                                    if isinstance(d["winner"], dict)
                                    else d["winner"])}
                     for f, d in decision["families"].items()},
    }


_NAN_POLICY_NOTE = (
    "An undefined change is UNKNOWN, never 0. It is its own bucket, it "
    "neither arms nor disarms the gate so the gate state HOLDS across it, and "
    "it gives a size multiplier of exactly 1. A change is undefined until "
    "BOTH endpoints exist, so NaN propagates from either endpoint and is "
    "never filled.")


def render_nan_bucket_note(warmup) -> str:
    if not warmup:
        return ("The `NaN` bucket's contents depend on warm-up depth, and "
                "this run recorded no warm-up audit, so whether the change in "
                "H was defined on every scored bar is NOT attested here. "
                "Re-run the study to record it. " + _NAN_POLICY_NOTE)
    required = warmup["required_bars"]
    hurst_only = warmup.get("hurst_only_required_bars", required)
    lookback_part = int(required) - int(hurst_only)
    if warmup["sufficient"]:
        return (f"The `NaN` bucket is EMPTY here because of the harness, not "
                f"the estimator, and this run MEASURED the condition that "
                f"makes it so. A change at a scored bar needs {required} bars "
                f"of history before the earliest window start - {hurst_only} "
                f"for the rolling Hurst window and its margin, plus "
                f"{lookback_part} more for the lookback that the difference "
                f"adds on top. The thinnest dataset in this run carried "
                f"{warmup['min_lead_bars']}, so the change is defined on "
                f"every scored bar. On a thinner cache the run prints a "
                f"warm-up warning and this bucket carries real trades. "
                + _NAN_POLICY_NOTE)
    short = ", ".join(f"`{d}`" for d in warmup["insufficient_datasets"])
    return (f"The `NaN` bucket is POPULATED here. A change at a scored bar "
            f"needs {required} bars of history before the earliest window "
            f"start - {hurst_only} for the rolling Hurst window and its "
            f"margin, plus {lookback_part} more for the lookback the "
            f"difference adds on top - and "
            f"{len(warmup['insufficient_datasets'])} dataset(s) carried less "
            f"({short}; thinnest {warmup['min_lead_bars']} bars), so the "
            f"first bars of the earliest window are unscored. Read the `NaN` "
            f"rows below as a warm-up artefact of this cache, not as an "
            f"estimator refusal. " + _NAN_POLICY_NOTE)


def _render_bucket_table(table: dict) -> list:
    lines = [
        "| Bucket (change in H) | Trades | Win rate | Mean net % | "
        "Median net % | Mean efficiency | Eff. rows | Compounded % | "
        "Trade-seq max DD % | Chop loss |",
        "|----------------------|-------:|---------:|-----------:|"
        "-------------:|----------------:|----------:|-------------:|"
        "-------------------:|----------:|",
    ]
    for bucket in DELTA_BUCKETS:
        row = table.get(bucket) or {}
        lines.append(
            f"| `{bucket}` | {row.get('trades', 0)} | "
            f"{_fmt(row.get('win_rate_pct'), 1, '%')} | "
            f"{_fmt(row.get('mean_pnl_pct_net'))} | "
            f"{_fmt(row.get('median_pnl_pct_net'))} | "
            f"{_fmt(row.get('mean_efficiency'), 4)} | "
            f"{row.get('efficiency_trades', 0)} | "
            f"{_fmt(row.get('compounded_return_pct'))} | "
            f"{_fmt(row.get('trade_seq_max_dd_pct'))} | "
            f"{_fmt(row.get('chop_loss_pct'))} |")
    lines.append("")
    return lines


def _render_joint_table(table: dict) -> list:
    lines = [
        "| ADX | Change in H | Trades | Win rate | Mean net % | Compounded % "
        "| Trade-seq max DD % |",
        "|-----|-------------|-------:|---------:|-----------:|-------------:|"
        "-------------------:|",
    ]
    for a in JOINT_ADX_BUCKETS:
        for d in JOINT_DELTA_BUCKETS:
            row = table.get(f"{a}|{d}") or {}
            lines.append(
                f"| `{a}` | `{d}` | {row.get('trades', 0)} | "
                f"{_fmt(row.get('win_rate_pct'), 1, '%')} | "
                f"{_fmt(row.get('mean_pnl_pct_net'))} | "
                f"{_fmt(row.get('compounded_return_pct'))} | "
                f"{_fmt(row.get('trade_seq_max_dd_pct'))} |")
    lines.append("")
    return lines


def _render_gate_sentence(gate: dict) -> str:
    if gate.get("limit") is None or gate.get("largest_separation") is None:
        return (gate.get("reason") or "the gate could not be evaluated") + "."
    if gate.get("reason"):
        return gate["reason"] + "."
    relation = "at or below" if gate["passed"] else "ABOVE"
    tail = (" That 0.000 is DEGENERATE: the injection search stops at its "
            "first grid point because the zero-injection contrast already "
            "clears the bar, so the gate passes trivially and corroborates "
            "nothing."
            if gate.get("limit_is_degenerate") else "")
    return (f"on the confirmatory family "
            f"(`{gate.get('family', PRIMARY_FAMILY)}`) the two-sided detection "
            f"limit is {gate['limit']:.3f} efficiency units, {relation} the "
            f"magnitude of the {gate['largest_separation']:+.3f} those SAME "
            f"rows separate by." + tail)


def _render_config_table(cfgs: Sequence[dict], protocol: Sequence[str]) -> list:
    head = ("| Config | Mode | W | Lookback | Pooled N (eff) | sup/kept eff | "
            "separation (eff, signed) | 2-sided free p | 2-sided cluster p | "
            "2-sided cluster p (net ret) | BH sig |")
    sep = ("|--------|------|--:|---------:|----------------|--------------|"
           "------------------------:|---------------:|-----------------:|"
           "----------------------------:|:------:|")
    for w in protocol:
        head += f" {w} dd | {w} chop | {w} ret (arm/base) |"
        sep += "------:|--------:|-------------------|"
    head += " #1424 rule |"
    sep += "------------|"
    lines = [head, sep]
    for cfg in cfgs:
        row = (f"| `{cfg['config_id']}` | {cfg['mode']} | "
               f"{cfg['hurst_window']} | "
               f"{cfg.get('lookback_bars', delta_lookback_bars(cfg['hurst_window']))} | "
               f"{cfg['n_pooled_trades']} ({_fmt(cfg['n_pooled_effective'], 1)}) | "
               f"{_fmt(cfg['n_suppressed_effective'], 1)}/"
               f"{_fmt(cfg['n_kept_effective'], 1)} | "
               f"{_fmt_signed(cfg.get('separation'), 4)} | "
               f"{_fmt_p(cfg['p_raw'])} | {_fmt_p(cfg['p_cluster'])} | "
               f"{_fmt_p(cfg.get('p_cluster_return'))} | "
               f"{'yes' if cfg.get('bh_reject') else 'no'} |")
        for w in protocol:
            r = cfg["windows"].get(w) or {}
            if not r.get("n_legs"):
                row += " - | - | - |"
            else:
                row += (f" {_fmt(r['dd_delta'])} | {_fmt(r['chop_delta'])} | "
                        f"{_fmt(r['ret_gated'])} / {_fmt(r['ret_ungated'])} |")
        ok, reasons = config_verdict(cfg)
        row += f" {'would pass' if ok else '; '.join(reasons)} |"
        lines.append(row)
    lines.append("")
    return lines


def render_recommendation(decision: dict, mde: dict,
                          configs: Sequence[dict] = (),
                          predecessor: Optional[dict] = None) -> str:
    gate = decision.get("validity_gate") or validity_gate(mde)
    lines = ["## Recommendation", ""]
    lines.append(VERDICT_LABELS.get(decision["verdict"],
                                    decision["verdict"].upper()))
    lines.append("")
    lines.append(decision["justification"])
    lines.append("")
    lines.append("### The pre-registered prediction, and what happened")
    lines.append("")
    lines.append(f"> {KEY_RISK_PREDICTION}")
    lines.append("")
    if decision["verdict"] == VERDICT_CHANGE_SORTS:
        lines.append(
            "The prediction did NOT hold, and that is the interesting "
            "outcome. A two-sided test rejected the contrast on a predictor "
            "none of #1410, #1422, #1424 or #1426 ever measured. Read what "
            "that is and what it is not. It IS a pre-registered rejection: "
            "one hypothesis fixed before the run, a rank-1 bar of alpha, a "
            "cluster-rotation null, and a sign carried through. It is NOT an "
            "effect size, because the gate's limit is degenerate on a "
            "rejection and cannot bound the magnitude. It is NOT an economic "
            "result, because the continuity target is scored on the same rows "
            "and reported beside it. And it is NOT a second independent look "
            "at the market: the OUTCOMES are the rows #1424 and #1426 already "
            "scored, so only the predictor is new. What it licenses is a "
            "FOLLOW-UP DESIGN on tape this study did not score. It licenses "
            "no threshold: `scheduler/hurst_gate.go` reads the LEVEL of H and "
            "never its change.")
    elif decision.get("key_risk_held"):
        lines.append(
            "The prediction HELD in its useful half, and the run bought more "
            "than it predicted. The contrast is not rejected AND the validity "
            "gate passed, so the run bounds any sorting effect from the "
            "change in H in BOTH directions at the measured limit. The "
            "question this study was opened to ask is therefore closed at "
            "this resolution rather than left open.")
    else:
        lines.append(
            "The prediction HELD. The contrast is not rejected, and the "
            "validity gate did not pass either, so the run carries no bound "
            "in either direction. The change in H is neither shown to sort "
            "these trades nor shown not to. The binding constraint is the "
            "calendar: effective N here counts independent clusters rather "
            "than rows, this study scores the same calendar as #1424 and "
            "#1426 minus what the deeper warm-up drops, and the venues this "
            "repository can reach are already exhausted.")
    audit = prediction_audit(mde, predecessor)
    if audit:
        lines.append("")
        lines.append(audit)
    lines.append("")
    lines.append("**No configuration is recommended, and none could be.** "
                 + NO_PROMOTION_SENTENCE)
    lines.append("")
    lines.append("| Family | Primary configs tested | Would pass #1424's rule "
                 "| Recommended |")
    lines.append("|--------|----------------------:|------------------------:|"
                 ":-----------:|")
    for family in FAMILIES:
        entry = decision["families"][family]
        lines.append(f"| `{family}` | {entry['n_tested']} | "
                     f"{entry['n_passing']} | none (structurally) |")
    lines.append("")
    lines.append(
        "#1411's `hurst_gate` stays DEFAULT-OFF with no recommended "
        "thresholds, and it is untouched by this work. `config.example.json` "
        "carries no `hurst_gate` block and nothing here adds one.")
    lines.append("")
    lines.append(CONTRACT_PATH_STATEMENT)
    return "\n".join(lines).rstrip() + "\n"


def render_report(payload: dict) -> str:
    pre = payload["pre_registered"]
    run = payload["run_summary"]
    cfgs = payload["configs"]
    mde = payload.get("mde") or {}
    decision = payload["decision"]
    hurst_windows = pre["hurst_windows"]
    gate = decision.get("validity_gate") or validity_gate(mde)

    out = []
    out.append("# Does the CHANGE in the Hurst exponent sort entry outcomes? "
               "(#1427)")
    out.append("")
    out.append(
        "Report-only research. Nothing here is wired to the scheduler, to "
        "config, or to any live path. This file is NOT the live-evidence "
        "contract path: `hurst_gate_calibration.md` stays with the #1424 "
        "resolution study, and this script refuses to write it.")
    out.append("")
    out.append(
        "#1410, #1422, #1424 and #1426 all bucket the LEVEL of the Hurst "
        "exponent at entry. On the best-powered confirmatory rows that level "
        "did not sort trades: #1424 measured a separation of -0.005 "
        "efficiency units against a limit of 0.013, and #1426 re-tested the "
        "same contrast two-sided and stayed inconclusive. Nothing has tested "
        "whether the CHANGE in H carries information. Those are different "
        "claims. \"This market is trending\" is a statement about a level; "
        "\"this market is becoming trending\" is a statement about a "
        "derivative, and a persistence estimate rising through its midpoint "
        "describes a regime that is FORMING rather than one already priced. "
        "This study measures that derivative under #1424's power discipline.")
    out.append("")
    out.append(
        f"Generated by `backtest/research/hurst_1427_change_sort.py` (schema "
        f"{payload['schema_version']}). Every number below is rendered from "
        f"`hurst_1427_change_sort.json`, produced by the same run; "
        f"`--render-only` reproduces this file byte for byte from it.")
    out.append("")

    out.append("## Verdict at a glance")
    out.append("")
    out.append(f"- Predictor: **the change in rolling H over `W // "
               f"{DELTA_LOOKBACK_DENOMINATOR}` bars**, derived from the same "
               f"rolling H #1424 computes. Confirmatory hypothesis: "
               f"`{PRIMARY_CONFIG_ID}` (lookback "
               f"{pre['primary_lookback_bars']} bars).")
    out.append(f"- Inference direction, pre-registered: "
               f"**{INFERENCE_DIRECTION.replace('_', '-')}**.")
    out.append(f"- Confirmatory two-sided cluster p on `{PRIMARY_FAMILY}`: "
               f"**{_fmt_p(decision.get('confirmatory_p'))}** against a "
               f"Benjamini-Hochberg rank-1 bar of "
               f"{decision.get('confirmatory_bar', ALPHA):g}.")
    out.append(f"- Separation on those same rows: "
               f"**{_fmt_signed(gate.get('largest_separation'), 4)}** "
               f"efficiency units - " + (
                   _direction_phrase(gate["largest_separation"])
                   if gate.get("largest_separation") is not None
                   else "no measurable separation") + ".")
    out.append(f"- Measured two-sided detection limit on those same rows: "
               f"**{'> ' + f'{MDE_EFF_GRID_MAX:g}' if gate.get('limit') is None else _fmt(gate.get('limit'), 3)}** "
               f"efficiency units"
               + (" - DEGENERATE, see below: the search stops at its first "
                  "grid point when the zero-injection contrast already clears "
                  "the bar, so this figure restates the rejection and "
                  "measures no resolution."
                  if gate.get("limit_is_degenerate") else "."))
    out.append(f"- Validity gate: **{'PASSED' if gate['passed'] else 'FAILED'}"
               f"** - " + _render_gate_sentence(gate))
    out.append(f"- Verdict: **{VERDICT_LABELS.get(decision['verdict'], decision['verdict'].upper())}**.")
    out.append(f"- Contract path: **deferred**; it stays with #1424 and the "
               f"supersede clause passes to "
               f"{', '.join('#' + str(n) for n in SIBLING_DEFERRAL)}.")
    out.append("")

    out.append("## The design decisions this study had to make, and when")
    out.append("")
    out.append(
        "The issue names three parameters a change study invents that a level "
        "study inherits: the lookback, the bucket edges, and the direction of "
        "the inference. All three are fixed BEFORE the run, as named "
        "constants at the top of the script, and all three are rendered here "
        "from that run's JSON rather than described from memory.")
    out.append("")
    out.append("### The re-centring rule")
    out.append("")
    out.append(RECENTRING_RULE)
    out.append("")
    out.append(BUCKET_SEPARATOR_NOTE)
    out.append("")
    out.append("| Landmark | #1410, on the level | This study, on the change |")
    out.append("|----------|---------------------|---------------------------|")
    out.append(
        f"| Bucket edges | {', '.join(f'{e:g}' for e in pre['level_edges'])} | "
        f"{', '.join(_delta_edge_label(e) for e in pre['delta_edges'])} |")
    for family in FAMILIES:
        level_pairs = "; ".join(
            f"arm {a:g} / disarm {d:g}" for a, d in GATE_PAIRS[family])
        delta_pairs = "; ".join(
            f"arm {_delta_edge_label(a)} / disarm {_delta_edge_label(d)}"
            for a, d in DELTA_GATE_PAIRS[family])
        out.append(f"| `{family}` gate pairs | {level_pairs} | {delta_pairs} |")
    out.append(f"| Pinned hypothesis | `{LEVEL_PRIMARY_CONFIG_ID}` | "
               f"`{PRIMARY_CONFIG_ID}` |")
    out.append("")
    out.append("### The lookback")
    out.append("")
    out.append(DELTA_LOOKBACK_RATIONALE)
    out.append("")
    out.append("### The direction of the inference")
    out.append("")
    out.append(INFERENCE_DIRECTION_RATIONALE)
    out.append("")
    out.append("### Prior exposure, disclosed")
    out.append("")
    out.append(PRIOR_EXPOSURE_DISCLOSURE)
    out.append("")
    out.append(PRIMARY_HYPOTHESIS_STATEMENT)
    out.append("")

    out.append("## Two-sided inference")
    out.append("")
    out.append(
        "Every p-value on the confirmatory path - both cohorts, both targets, "
        "the gate arm and the sizing arm, free-shuffle and cluster-rotation - "
        "is the DOUBLED ONE-TAILED permutation p #1426 defines, imported from "
        "that module rather than restated:")
    out.append("")
    out.append("```")
    out.append("p_ge = (1 + #{stat >= obs}) / (draws + 1)")
    out.append("p_le = (1 + #{stat <= obs}) / (draws + 1)")
    out.append("p2   = min(1, 2 * min(p_ge, p_le))")
    out.append("```")
    out.append("")
    out.append(
        f"Both tails are counted over the draws the null ACTUALLY produced, "
        f"never over the requested count, so the smallest reachable p is "
        f"`2/(draws+1)`. At {pre['n_perm_mde']} detection-limit draws that "
        f"floor is {2.0 / (float(pre['n_perm_mde']) + 1.0):.5f}, and the "
        f"binding bar it has to clear is the 30-hypothesis grid's rank-1 "
        f"threshold {ALPHA / 30.0:.5f} rather than the confirmatory family's "
        f"own {ALPHA / max(1, PRIMARY_FAMILY_SIZE):g}. The DETECTION LIMIT "
        f"injects both directions at every grid point and scores the point by "
        f"`max(p2(+d), p2(-d))`, so the published limit is the smallest "
        f"effect this design would catch WHICHEVER WAY it points. The grids "
        f"are #1424's verbatim ({MDE_EFF_GRID_STEP:g} to "
        f"{MDE_EFF_GRID_MAX:g} with a {MDE_EFF_REFINE_STEP:g} refinement on "
        f"the primary target; {MDE_PP_GRID_STEP:g} to {MDE_PP_GRID_MAX:g} pp "
        f"on the continuity target), so the limits stay directly comparable "
        f"with the level studies' and the only thing a reader has to account "
        f"for is the predictor.")
    out.append("")
    out.append(STAGE0_EXCLUSION)
    out.append("")

    out.append("### The validity gate")
    out.append("")
    out.append(
        f"This study is VALID only when the measured two-sided cluster-null "
        f"detection limit on the CONFIRMATORY family (`{PRIMARY_FAMILY}`, the "
        f"family the single pinned hypothesis belongs to) falls at or below "
        f"the MAGNITUDE of that same family's observed separation, both in "
        f"efficiency units and both measured on the IDENTICAL rows. Otherwise "
        f"the verdict is a statement about the design and says so.")
    out.append("")
    out.append(
        "The gate reads a MAGNITUDE, which is legitimate only because the "
        "null underneath it is symmetric and the injection is applied in both "
        "directions with the harder of the two taken as the limit. A future "
        "edit that makes any p on this path one-sided again must restore a "
        "SIGNED comparison in the same change. The gate refuses the POOLED "
        "limit for the same reason #1424 and #1426 do: that number spans both "
        "families and would resolve a smaller effect purely by holding more "
        "trades, so pairing it with one family's separation biases the gate "
        "toward passing. Separations are read POOL-MATCHED and SIGNED.")
    out.append("")
    out.append(f"**Outcome: {'PASSED' if gate['passed'] else 'FAILED'}** - "
               + _render_gate_sentence(gate))
    out.append("")

    out.append("## Pre-registered design")
    out.append("")
    out.append(
        "Inherited from #1424 unless a section above names it: the datasets, "
        "the windows and their ownership matrix, the cohorts, the estimator, "
        "the targets, the cluster-rotation null, effective N, the coverage "
        "floors and the acceptance rule. These constants are IMPORTED from "
        "that module rather than restated, so a drift there fails loud here "
        "instead of silently re-scoring different cells.")
    out.append("")
    out.append(
        "- Estimator: `hurst_exponent` from "
        "`shared_strategies/open/indicators_core.py` (#1409 SSoT). Never "
        "reimplemented here, and the change series is a DIFFERENCE of its "
        "output rather than a second estimator.")
    out.append(
        f"- Change series: `dH(i) = H(i) - H(i - L)` with `L = W // "
        f"{DELTA_LOOKBACK_DENOMINATOR}`, giving "
        + ", ".join(f"L={pre['lookback_bars'][str(hw)]} at W={hw}"
                    for hw in hurst_windows) + ".")
    out.append(
        "- NaN policy: an undefined change is its OWN bucket, never 0. It "
        "propagates from either endpoint, is never filled, neither arms nor "
        "disarms the gate, and gives a size multiplier of exactly 1. ADX "
        "keeps its own NaN bucket unchanged.")
    out.append(f"- Hurst window lengths: "
               f"{', '.join(str(h) for h in hurst_windows)} bars.")
    out.append(f"- Buckets on the change at entry: "
               f"{', '.join('`' + b + '`' for b in pre['delta_buckets'])}.")
    out.append(
        f"- PRIMARY TARGET: `{PRIMARY_TARGET}` over a fixed {HORIZON_HOURS}"
        f"-hour horizon, bounded in `[-1, 1]`. CONTINUITY TARGET: "
        f"`{CONTINUITY_TARGET}`, scored on the SAME rows so the two describe "
        f"one pool.")
    out.append("- Windows: " + "; ".join(
        f"`{k}` {v[0]}..{v[1] or 'latest'}"
        for k, v in pre["windows"].items()) + ".")
    out.append(f"- Datasets ({len(pre['datasets'])}): "
               f"{', '.join('`' + d + '`' for d in pre['datasets'])}.")
    out.append(f"- Fee model: `{pre['fee_platform']}` on every venue, "
               f"decoupled from the data-source exchange exactly as in #1424.")
    out.append(
        f"- Volume floors on EFFECTIVE N: >= {MIN_SUPPRESSED_EFFECTIVE:g} "
        f"suppressed and >= {MIN_KEPT_EFFECTIVE:g} kept.")
    out.append(
        f"- Inference: {pre['n_perm']} draws, seed {pre['seed']}, minimum "
        f"rotation offset {MIN_OFFSET_DAYS} days; {pre['n_perm_mde']} draws "
        f"for the detection limits. Benjamini-Hochberg at alpha={ALPHA}, "
        f"applied SEPARATELY to the primary and exploratory cohorts.")
    out.append(f"- Joint table: ADX period {ADX_PERIOD} (Wilder), split at "
               f"{ADX_SPLIT:g}. Descriptive only, no verdict.")
    out.append("")

    out.append("### Look-ahead invariant on the derived series")
    out.append("")
    out.append(
        f"The change series inherits H's convention EXACTLY, and adds nothing "
        f"of its own. Rolling H at bar `i` uses closes `[i-W+1, i]`. The "
        f"change at bar `i` is `H(i) - H(i-L)`, so it reads no bar later than "
        f"`i` and its oldest input is bar `i-L-W+1`. The DECISION series is "
        f"the change shifted ONE bar, so a signal at bar N reads a change "
        f"through N-1; the backtester fills a bar-N signal at N+1's open, so "
        f"the FILL-BAR stamp is the change shifted TWICE. Entry ADX uses the "
        f"same two shifts. The primary target reads bars AFTER the fill, "
        f"which is what an outcome does, and it enters no decision anywhere. "
        f"The rolling H feeding the difference is computed from "
        f"`L + {STAMP_LEAD_BARS}` bars before each dataset's earliest scored "
        f"window rather than {STAMP_LEAD_BARS}, so the two shifts and the "
        f"lookback all resolve on real bars instead of silently producing "
        f"NaN.")
    out.append("")

    out.append("## Coverage, warm-up, and what the lookback costs")
    out.append("")
    cov = run.get("coverage") or {}
    warm = run.get("warmup") or {}
    out.append(
        f"{cov.get('n_kept', 0)} of {cov.get('n_cells', 0)} OWNED (dataset, "
        f"window) cells carried enough history to score: "
        f"{cov.get('required_lead_bars', 0)} bars of lead before the window "
        f"start, and at least "
        f"{float(cov.get('min_window_bar_fraction') or 0) * 100:.0f}% of the "
        f"bars a complete cache would hold inside the window. An open-ended "
        f"window is closed at ONE run-level reference bar "
        f"(`{cov.get('reference_last_bar') or '-'}`). {cov.get('n_dropped', 0)}"
        f" owned cells were DROPPED"
        + (", every one listed below with its reason"
           if cov.get("dropped") else "")
        + f"; a further {cov.get('n_unowned', 0)} (dataset, window) pairs were "
          f"never in the design because another venue owns that calendar "
          f"span.")
    out.append("")
    out.append(
        f"**The lead requirement is where this study pays for its predictor.** "
        f"A level study needs "
        f"{cov.get('hurst_only_required_lead_bars', 0)} bars; a change study "
        f"needs {cov.get('required_lead_bars', 0)}, because the difference "
        f"adds its lookback on top of the Hurst window before either endpoint "
        f"exists. The extra {cov.get('lookback_lead_bars', 0)} bars are the "
        f"whole cost, and the dropped-cell table below is where it lands.")
    out.append("")
    out.append("| Hurst window | Lookback | Margin | Required lead |")
    out.append("|-------------:|---------:|-------:|--------------:|")
    for key in sorted((warm.get("components") or {}), key=lambda k: int(k)):
        comp = warm["components"][key]
        out.append(f"| {comp['hurst_window']} | {comp['lookback_bars']} | "
                   f"{comp['margin_bars']} | {comp['required_bars']} |")
    out.append("")
    dropped = cov.get("dropped") or []
    if dropped:
        out.append("| Dataset | Window | Why dropped |")
        out.append("|---------|--------|-------------|")
        for d in dropped:
            out.append(f"| `{d['dataset']}` | `{d['window']}` | "
                       f"{d['reason']} |")
        out.append("")
    else:
        out.append("No owned cells were dropped.")
        out.append("")
    out.append(
        f"Warm-up measured against each dataset's own earliest scored window: "
        f"minimum lead {warm.get('min_lead_bars', 0)} bars against a "
        f"requirement of {warm.get('required_bars', 0)} "
        f"({warm.get('hurst_only_required_bars', 0)} for the Hurst window and "
        f"its margin, "
        f"{int(warm.get('required_bars', 0)) - int(warm.get('hurst_only_required_bars', 0))} "
        f"more for the lookback) - "
        + ("sufficient on every dataset."
           if warm.get("sufficient")
           else "SHORT on " + ", ".join(f"`{d}`" for d in
                                        warm.get("insufficient_datasets") or [])
           + ".")
        + (" The same datasets would have been sufficient for a LEVEL study."
           if warm.get("hurst_only_sufficient") and not warm.get("sufficient")
           else ""))
    out.append("")
    out.append(
        "Effective N is `N^2 / sum_ij rho_ij`, with `rho_ij` the symbol-level "
        "daily-return correlation when two trades' holding periods OVERLAP "
        "and 0 when they do not; same-asset pairs take 1.0 whatever the quote "
        "currency or venue, and correlations are clipped to `[0, 1]` so "
        "anti-correlation can never manufacture power.")
    out.append("")
    ex_names = sorted({d for c in cfgs
                       for d in (c.get("cluster_excluded_datasets") or [])})
    ex_rows = max((int(c.get("cluster_excluded_trades") or 0) for c in cfgs),
                  default=0)
    if ex_names:
        out.append(
            f"Datasets too short to host a calendar rotation, and therefore "
            f"dropped from the contrast, the counts and the effective-N "
            f"floors alike (up to {ex_rows} rows on a single config): "
            + ", ".join(f"`{d}`" for d in ex_names) + ".")
    else:
        out.append(
            "Every dataset spans enough calendar time to host a rotation, so "
            "no rows were dropped from any cluster contrast.")
    out.append("")
    excluded_target = max((int(c.get("n_missing_target") or 0) for c in cfgs),
                          default=0)
    out.append(
        f"Horizon truncation: up to {excluded_target} rows on a single config "
        f"had fewer than the horizon's bars left in their window slice and "
        f"were excluded from BOTH targets, so the primary and continuity "
        f"columns always describe one pool.")
    out.append("")

    out.append("## Measured detection limit, in BOTH directions")
    out.append("")
    out.append(
        "The smallest per-trade effect each pool could have detected "
        "WHICHEVER WAY IT POINTS, under the two-sided cluster null at the "
        "rank-1 Benjamini-Hochberg threshold. The separation column is that "
        "same contrast measured on the SAME rows, split by the SIGN of the "
        "change in H rather than by the level. `Resolvable?` compares the "
        "MAGNITUDE of the separation with the limit.")
    out.append("")
    out.append(
        "**Separations carry their SIGN, and the sign decides what a finding "
        "MEANS.** A separation is `mean(kept) - mean(suppressed)`. POSITIVE "
        "means the trades taken while the change pointed the hypothesis' way "
        "did better. NEGATIVE means the SUPPRESSED trades did better, so a "
        "gate built on the change would have HURT - under a symmetric test "
        "that is a detectable finding rather than a blind spot.")
    out.append("")
    by_pool = mde.get("observed_separation_by_pool") or {}
    out.append("| Pool | Rows | BH denominator | 2-sided cluster MDE (eff) | "
               "2-sided free MDE (eff) | Largest separation ON THAT POOL "
               "(eff) | By family (signed) | Resolvable? | 2-sided cluster p "
               "at zero injection |")
    out.append("|------|-----:|---------------:|--------------------------:|"
               "-----------------------:|"
               "--------------------------------------:|"
               "--------------------|:-----------:|"
               "--------------------------------:|")
    for key, label, denom in (
            ("1410", "#1410 design (its 30-hypothesis grid)", 30),
            ("primary", "this study, primary cohort", PRIMARY_FAMILY_SIZE),
            ("exploratory", "this study, exploratory grid", 30)):
        c = mde.get(f"pooled_{key}_cluster")
        f = mde.get(f"pooled_{key}_free")
        seps = by_pool.get(key) or {}
        largest = _largest_magnitude_signed(seps)
        if largest is None or c is None:
            resolvable = "-"
        else:
            resolvable = "yes" if abs(largest) >= c else "NO"
        out.append(
            f"| {label} | {mde.get(f'pooled_{key}_n', 0)} | {denom} | "
            f"{'> ' + f'{MDE_EFF_GRID_MAX:g}' if c is None else _fmt(c, 3)} | "
            f"{'> ' + f'{MDE_EFF_GRID_MAX:g}' if f is None else _fmt(f, 3)} | "
            f"{_fmt_signed(largest, 3)} | {_fmt_family_seps(seps, 3)} | "
            f"{resolvable} | "
            f"{_fmt_p(mde.get(f'pooled_{key}_cluster_p0'))} |")
    out.append("")
    out.append(
        "The same three pools on the CONTINUITY target (percentage points of "
        "net return), on #1422's grid so the studies stay comparable:")
    out.append("")
    by_pool_pp = mde.get("observed_separation_pp_by_pool") or {}
    out.append("| Pool | 2-sided cluster MDE (pp/trade) | Largest separation "
               "ON THAT POOL (pp/trade) | By family (signed) | Resolvable? | "
               "2-sided cluster p at zero injection |")
    out.append("|------|-------------------------------:|"
               "------------------------------------------:|"
               "--------------------|:-----------:|"
               "--------------------------------:|")
    for key, label in (("1410", "#1410 design (its 30-hypothesis grid)"),
                       ("primary", "this study, primary cohort"),
                       ("exploratory", "this study, exploratory grid")):
        c = mde.get(f"pooled_{key}_cluster_return")
        seps = by_pool_pp.get(key) or {}
        largest = _largest_magnitude_signed(seps)
        if largest is None or c is None:
            resolvable = "-"
        else:
            resolvable = "yes" if abs(largest) >= c else "NO"
        out.append(f"| {label} | "
                   f"{'> ' + f'{MDE_PP_GRID_MAX:g}' if c is None else _fmt(c)} "
                   f"| {_fmt_signed(largest)} | {_fmt_family_seps(seps)} | "
                   f"{resolvable} | "
                   f"{_fmt_p(mde.get(f'pooled_{key}_cluster_return_p0'))} |")
    out.append("")
    out.append(
        f"**The numbers the validity gate and the verdict actually read** are "
        f"neither of the pooled rows above. Both are evaluated on the "
        f"CONFIRMATORY family (`{PRIMARY_FAMILY}`) alone, on that family's "
        f"own rows. The pooled primary limit spans BOTH families and would "
        f"resolve a smaller effect purely because it holds more trades, so "
        f"reading it against one family's separation would make the gate "
        f"easier to pass than its own row-matched rule allows.")
    out.append("")
    fam_lim = mde.get("by_family_cluster") or {}
    fam_sep = mde.get("by_family_separation") or {}
    fam_lim_ret = mde.get("by_family_cluster_return") or {}
    fam_sep_ret = mde.get("by_family_separation_return") or {}
    fam_p0 = mde.get("by_family_cluster_p0") or {}
    fam_n = mde.get("by_family_n") or {}
    out.append("| Family | Rows | 2-sided cluster MDE (eff) | Separation "
               "(eff, signed) | 2-sided p at zero injection | 2-sided cluster "
               "MDE (pp) | Separation (pp, signed) | Reads the gate? |")
    out.append("|--------|-----:|--------------------------:|"
               "-------------------------:|----------------------------:|"
               "-------------------------:|------------------------:|"
               ":---------------:|")
    for family in FAMILIES:
        lim = fam_lim.get(family)
        lim_ret = fam_lim_ret.get(family)
        out.append(
            f"| `{family}` | {fam_n.get(family, 0)} | "
            f"{'> ' + f'{MDE_EFF_GRID_MAX:g}' if lim is None else _fmt(lim, 3)} | "
            f"{_fmt_signed(fam_sep.get(family), 3)} | "
            f"{_fmt_p(fam_p0.get(family))} | "
            f"{'> ' + f'{MDE_PP_GRID_MAX:g}' if lim_ret is None else _fmt(lim_ret)} | "
            f"{_fmt_signed(fam_sep_ret.get(family))} | "
            f"{'YES' if family == PRIMARY_FAMILY else 'no'} |")
    out.append("")

    out.append("## Part A - outcomes bucketed by the CHANGE in H at entry")
    out.append("")
    out.append(
        "Ungated legs only, pooled per family across datasets and windows and "
        "deduplicated on `(strategy, symbol, timeframe, entry_date)` with the "
        "symbol venue-qualified. Drawdown here is TRADE-GRANULAR (the "
        "compounded trade sequence), not the bar-level engine drawdown used "
        "in Part B. `Mean efficiency` is the PRIMARY TARGET, the column the "
        "validity gate adjudicates.")
    out.append("")
    out.append(render_nan_bucket_note(run.get("warmup")))
    out.append("")
    for family in FAMILIES:
        out.append(f"### {family}")
        out.append("")
        for hw in hurst_windows:
            out.append(f"**Hurst window {hw} bars, lookback "
                       f"{pre['lookback_bars'][str(hw)]} bars**")
            out.append("")
            out.extend(_render_bucket_table(
                (payload["buckets"].get(family) or {}).get(str(hw)) or {}))

    out.append("## Part B / C - the pinned hypothesis")
    out.append("")
    out.append(
        "`gate` rows are real Backtester re-runs with entry signals masked "
        "while the gate is disarmed (closes never masked); their drawdowns "
        "are bar-level. `size` rows re-compound the same ungated trade "
        "sequence with the size multiplier; their drawdowns are "
        "trade-granular. Never compare a `gate` drawdown to a `size` "
        "drawdown. `dd` and `chop` are MAGNITUDE deltas (arm minus ungated) "
        "averaged over that window's legs, so negative means improvement. "
        "Every p column is TWO-SIDED. The `#1424 rule` column shows whether "
        "the config would have passed that study's acceptance rule; it "
        "promotes nothing here.")
    out.append("")
    primary = [c for c in cfgs if c["cohort"] == COHORT_PRIMARY]
    out.extend(_render_config_table(primary, PRIMARY_PROTOCOL_WINDOWS))

    out.append("## Part B / C - exploratory grid (#1410's shape, re-centred)")
    out.append("")
    out.append(
        "The same 30-cell grid #1410 swept on the level, re-centred onto the "
        "change, reported for completeness under its OWN Benjamini-Hochberg "
        "correction. It is exploratory and promotes nothing.")
    out.append("")
    exploratory = [c for c in cfgs if c["cohort"] == COHORT_EXPLORATORY]
    out.extend(_render_config_table(exploratory, EXPLORATORY_PROTOCOL_WINDOWS))

    out.append("## Part D - joint ADX x change-in-H buckets (descriptive)")
    out.append("")
    out.append(STAGE0_EXCLUSION)
    out.append("")
    joint = payload.get("joint") or {}
    joint_hw = max(hurst_windows)
    out.append(f"Scored at Hurst window {joint_hw} bars, lookback "
               f"{pre['lookback_bars'][str(joint_hw)]} bars.")
    out.append("")
    for family in FAMILIES:
        entry = joint.get(family) or {}
        out.append(f"### {family}")
        out.append("")
        if not entry:
            out.append("No trades pooled for this family.")
            out.append("")
            continue
        out.extend(_render_joint_table(entry.get("table") or {}))

    out.append("## What a positive result here would and would not mean")
    out.append("")
    out.append(CONSTRUCTION_CAVEAT)
    out.append("")
    out.append(DEGENERATE_LIMIT_DISCLOSURE)
    out.append("")
    out.append("**The two targets on the confirmatory family, side by side.** "
               + continuity_clause(mde))
    out.append("")

    out.append("## What this study cannot say")
    out.append("")
    out.append(
        "1. It cannot calibrate `scheduler/hurst_gate.go`. That gate reads "
        "the LEVEL of H and never its change, so no threshold measured here "
        "fits it, and a positive result licenses a follow-up DESIGN rather "
        "than a configuration.")
    out.append(
        "2. It cannot claim an independent sample. The outcomes are the same "
        "rows #1424 and #1426 scored, so this is a new predictor on old tape "
        "and its detection limit is of the same order as theirs.")
    out.append(
        "3. It cannot re-open the LEVEL question, and it does not try. #1424 "
        "owns that evidence and this run leaves its script, its tests and its "
        "report untouched.")
    out.append(
        "4. It cannot re-answer #1412's Stage 0. That question is defined on "
        "the level, and the joint table above carries no verdict.")
    out.append(
        "5. It cannot resolve an effect below the measured limit. When the "
        "separation sits under it, the verdict is a statement about this "
        "design's resolution and the report says so in those words.")
    out.append(
        "6. It cannot size an effect it rejects. The validity gate's limit "
        "goes DEGENERATE on a rejection, so a passing gate there measures "
        "nothing; the magnitude stays unestimated until the continuity target "
        "clears its own limit.")
    out.append("")
    out.append(
        "What it CAN say, and what no study before it could: whether the "
        "DIRECTION a persistence estimate is moving at entry separates "
        "outcomes at all, measured in both directions with the limit "
        "published beside the separation.")
    out.append("")

    out.append("## Run summary")
    out.append("")
    out.append(f"- Legs scored: {run['legs']} ungated + {run['gated_arms']} "
               f"gated arms.")
    out.append(f"- Harness identity: {run['mirror_verified_legs']} of "
               f"{run['legs']} ungated legs reproduced "
               f"`eval_windows.run_leg` exactly.")
    out.append("- Pooled deduplicated trades: " + "; ".join(
        f"{f} {run['pooled_trades'][f]} "
        f"(primary {run['pooled_primary'][f]}, exploratory "
        f"{run['pooled_exploratory'][f]})" for f in FAMILIES) + ".")
    out.append(f"- Hypotheses: {run['n_primary_configs']} primary, "
               f"{run['n_exploratory_configs']} exploratory; "
               f"Benjamini-Hochberg-significant on the two-sided cluster p: "
               f"{run['n_primary_significant']} primary, "
               f"{run['n_exploratory_significant']} exploratory.")
    out.append(f"- Warm-up lead before each dataset's own earliest scored "
               f"window: min {warm.get('min_lead_bars', 0)} bars, required "
               f"{warm.get('required_bars', 0)} - "
               f"{'sufficient on every dataset' if warm.get('sufficient') else 'SHORT on ' + ', '.join(warm.get('insufficient_datasets') or [])}.")
    out.append(f"- Wall time: {run['elapsed_sec']} s.")
    out.append("")

    out.append(render_recommendation(
        decision, mde, cfgs, pre.get("predecessor_reference")))
    return "\n".join(out).rstrip() + "\n"


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
                   help=f"comma-separated families to run "
                        f"({', '.join(FAMILIES)})")
    p.add_argument("--windows", default=None,
                   help="comma-separated window names")
    p.add_argument("--datasets", default=None,
                   help="comma-separated [EXCHANGE=]SYMBOL:TIMEFRAME")
    p.add_argument("--hurst-windows", default=None,
                   help="comma-separated rolling Hurst window lengths")
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
            f"[1427] this study scores the CHANGE in H, which "
            f"scheduler/hurst_gate.go never reads, so it DEFERS the "
            f"live-evidence contract path {CONTRACT_REPORT_BASENAME}; "
            f"hurst_1424_gate_resolution.py owns it. Its own render belongs "
            f"at {_DEFAULT_REPORT_OUT}. The supersede clause passes to "
            f"{' and '.join('#' + str(n) for n in SIBLING_DEFERRAL)}.")

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
                f"[1427] refusing to overwrite the committed aggregate "
                f"{_DEFAULT_JSON_OUT} from {kind} ({narrowed}). Pass an "
                f"explicit --json-out.")
        if (os.path.abspath(args.report_out)
                == os.path.abspath(_DEFAULT_REPORT_OUT)):
            raise SystemExit(
                f"[1427] refusing to target the committed report "
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
                    f"[1427] {args.json_out} is not stamped as a complete "
                    f"run, so it may not be rendered to the committed report "
                    f"{_DEFAULT_REPORT_OUT}.")
            if not stamp.get("pre_registered_inference"):
                raise SystemExit(
                    f"[1427] {args.json_out} is not stamped as having run the "
                    f"pre-registered inference settings and verification, so "
                    f"it may not be rendered to the committed report "
                    f"{_DEFAULT_REPORT_OUT}.")
            if not args.write_report:
                raise SystemExit(
                    "[1427] writing the committed report needs "
                    "--write-report, on --render-only exactly as on a scoring "
                    "run.")
        report = report_from_payload(payload)
        with open(args.report_out, "w") as fh:
            fh.write(report)
        print(f"[1427] re-rendered {args.report_out} from {args.json_out}")
        return 0

    datasets = _parse_datasets(args.datasets)
    if args.fetch_only:
        ensure_min_history(datasets)
        print("[1427] backfill complete")
        return 0

    families = FAMILIES
    if args.only:
        wanted = [t.strip() for t in args.only.split(",") if t.strip()]
        for f in wanted:
            if f not in FAMILIES:
                raise SystemExit(
                    f"unknown family {f!r}; known: {list(FAMILIES)}")
        families = tuple(f for f in FAMILIES if f in wanted)
    window_names = _parse_windows(args.windows)
    hurst_windows = (tuple(int(t) for t in args.hurst_windows.split(","))
                     if args.hurst_windows else HURST_WINDOWS)

    resolved = resolve_primary_config_id(_JSON_1410)
    if resolved != LEVEL_PRIMARY_CONFIG_ID:
        raise SystemExit(
            f"#1424's pinned hypothesis {LEVEL_PRIMARY_CONFIG_ID!r} no longer "
            f"matches the committed #1410 argmin {resolved!r}. This study's "
            f"pin is that one re-centred, so re-pin deliberately; never let "
            f"it drift.")

    started = time.time()
    backfill = {}
    if not args.skip_fetch:
        print(f"[1427] backfilling {len(datasets)} datasets...")
        backfill = ensure_min_history(datasets)

    from data_fetcher import load_cached_data
    from registry_loader import load_registry
    reg = load_registry("spot")

    print(f"[1427] loading {len(datasets)} datasets from the venue caches...")
    frames = {}
    for dataset in datasets:
        exchange_id, symbol, timeframe = dataset
        try:
            frames[dataset] = load_cached_data(symbol, timeframe,
                                               exchange_id=exchange_id)
        except Exception as exc:
            print(f"[1427] load FAILED for {exchange_id} "
                  f"{dataset_key(symbol, timeframe)}: {exc}")
            frames[dataset] = pd.DataFrame()

    coverage = coverage_audit(frames, window_names, hurst_windows)
    print(f"[1427] coverage: {coverage['n_kept']}/{coverage['n_cells']} owned "
          f"cells kept, {coverage['n_dropped']} dropped, "
          f"{coverage['n_unowned']} not owned "
          f"(lead {coverage['required_lead_bars']} bars = "
          f"{coverage['hurst_only_required_lead_bars']} Hurst + "
          f"{coverage['lookback_lead_bars']} lookback)")
    for d in coverage["dropped"]:
        print(f"[1427]   dropped {d['dataset']} {d['window']}: {d['reason']}")

    def _cell_ok(dataset, window):
        exchange_id, symbol, timeframe = dataset
        key = dataset_key(qualified_symbol(exchange_id, symbol), timeframe)
        return bool(coverage["cells"].get(f"{key}|{window}"))

    usable_datasets = [ds for ds in datasets
                       if any(_cell_ok(ds, w) for w in window_names)]
    if not usable_datasets:
        raise SystemExit("[1427] no dataset carries a scoreable cell; nothing "
                         "to do")

    scored_windows = [w for w in window_names
                      if any(_cell_ok(ds, w) for ds in usable_datasets)]
    first_needed_by_ds = {}
    for ds in usable_datasets:
        own = [w for w in scored_windows if _cell_ok(ds, w)]
        first_needed_by_ds[ds] = min(pd.Timestamp(WINDOWS[w][0]) for w in own)

    warmup = delta_warmup_audit(
        scored_warmup_leads(frames, coverage, scored_windows), hurst_windows)
    if not warmup["sufficient"]:
        print(f"[1427] WARNING: warm-up shortfall on "
              f"{len(warmup['insufficient_datasets'])} dataset(s): "
              f"{', '.join(warmup['insufficient_datasets'])}. The change in H "
              f"is UNDEFINED on their first scored bars, so the NaN bucket "
              f"carries real trades. NaN stays its own bucket (never 0) and "
              f"holds the gate state.")
    else:
        print(f"[1427] warm-up OK: min lead {warmup['min_lead_bars']} bars "
              f"before each dataset's own earliest scored window "
              f"(need {warmup['required_bars']} = "
              f"{warmup['hurst_only_required_bars']} Hurst + "
              f"{warmup['required_bars'] - warmup['hurst_only_required_bars']} "
              f"lookback).")

    print(f"[1427] computing rolling Hurst and its change for "
          f"{len(usable_datasets)}x{len(hurst_windows)} (dataset, window) "
          f"pairs...")
    delta: dict = {}
    cache_path = None
    if args.out_dir:
        os.makedirs(args.out_dir, exist_ok=True)
        cache_path = os.path.join(args.out_dir, "hurst_1427_rolling.npz")
    cached = {}
    if cache_path and os.path.exists(cache_path):
        with np.load(cache_path, allow_pickle=False) as z:
            cached = {k: z[k] for k in z.files}

    def _hurst_key(dataset, hw):
        exchange_id, symbol, timeframe = dataset
        return (f"{exchange_id}|{symbol}|{timeframe}|{hw}|"
                f"L{delta_lookback_bars(hw)}")

    def _lead_stamp(dataset, hw):
        return delta_first_needed(frames[dataset].index,
                                  first_needed_by_ds[dataset],
                                  delta_lookback_bars(hw))

    def _hurst_job(job):
        dataset, hw = job
        key = _hurst_key(dataset, hw)
        frame = frames[dataset]
        needed = _lead_stamp(dataset, hw)
        if key in cached and cache_entry_is_usable(
                cached.get(f"meta|{key}"), frame.index, needed):
            return job, pd.Series(cached[key], index=frame.index)
        return job, rolling_hurst_for_delta(frame["close"], hw,
                                            delta_lookback_bars(hw),
                                            first_needed=first_needed_by_ds[dataset])

    jobs = [(ds, hw) for ds in usable_datasets for hw in hurst_windows]
    rolling: dict = {}
    with ThreadPoolExecutor(max_workers=max(1, args.jobs)) as pool:
        for job, series in pool.map(_hurst_job, jobs):
            rolling[job] = series
            delta[job] = delta_hurst_series(series,
                                            delta_lookback_bars(job[1]))
    if cache_path:
        arrays = {}
        for ds, hw in jobs:
            key = _hurst_key(ds, hw)
            arrays[key] = rolling[(ds, hw)].to_numpy(dtype=float)
            arrays[f"meta|{key}"] = cache_meta(frames[ds].index,
                                               _lead_stamp(ds, hw))
        np.savez_compressed(cache_path, **arrays)

    print(f"[1427] computing entry-ADX stamps for {len(usable_datasets)} "
          f"datasets...")
    adx_stamps = {ds: adx_entry_stamp(frames[ds]) for ds in usable_datasets}

    print("[1427] computing symbol daily-return correlations...")
    rho_by_symbol = symbol_return_correlations(
        {ds: frames[ds] for ds in usable_datasets})

    units = [(family, exemplar, ds, wname)
             for family in families
             for exemplar in FAMILY_EXEMPLARS[family]
             for ds in usable_datasets
             for wname in scored_windows
             if _cell_ok(ds, wname)]
    print(f"[1427] scoring {len(units)} legs "
          f"({len(hurst_windows) * 3} gated arms each)...")

    def _leg_job(unit):
        family, exemplar, ds, wname = unit
        by_window = {hw: delta[(ds, hw)] for hw in hurst_windows}
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
        rows = [t for lg in legs if lg["family"] == family
                for t in lg["trades"]]
        raw_counts[family] = len(rows)
        pooled[family] = dedup_entries(rows, WINDOW_ORDER)
    for family in FAMILIES:
        pooled.setdefault(family, [])
        raw_counts.setdefault(family, 0)

    for family in FAMILIES:
        for t in pooled[family]:
            if t["cohort"] != COHORT_PRIMARY:
                continue
            key = (dataset_key(t["symbol"], t["timeframe"]), t["window"])
            if key in D_1410:
                raise AssertionError(
                    f"primary cohort leaked a #1410 cell: {key}")

    print("[1427] sweeping configs and running BOTH two-sided nulls on both "
          "targets...")
    configs = build_configs(legs, pooled, hurst_windows, rho_by_symbol,
                            args.n_perm, args.seed)
    configs = [c for c in configs if c["family"] in families]
    apply_bh_by_cohort(configs, alpha=ALPHA)

    print("[1427] measuring two-sided detection limits...")
    mde = measure_detection_limits(pooled, hurst_windows, args.n_perm_mde,
                                   args.seed)

    predecessor = predecessor_reference()
    decision = decide_recommendation(configs, mde)

    buckets = {family: {str(hw): bucket_tables(pooled[family], hw)
                        for hw in hurst_windows}
               for family in FAMILIES}

    joint_hw = max(hurst_windows)
    joint = {family: {"table": joint_adx_delta_table(pooled[family], joint_hw)}
             for family in FAMILIES}

    n_primary = sum(1 for c in configs if c["cohort"] == COHORT_PRIMARY)
    n_expl = sum(1 for c in configs if c["cohort"] == COHORT_EXPLORATORY)
    payload = {
        "schema_version": SCHEMA_VERSION,
        "issue": ISSUE,
        "pre_registered": {
            "families": {f: list(FAMILY_EXEMPLARS[f]) for f in FAMILIES},
            "family_sense": dict(FAMILY_SENSE),
            "exemplar_close_overrides": EXEMPLAR_CLOSE_OVERRIDES,
            "predictor": "delta_hurst",
            "predecessor_reference": predecessor,
            "degenerate_limit_disclosure": DEGENERATE_LIMIT_DISCLOSURE,
            "construction_caveat": CONSTRUCTION_CAVEAT,
            "recentring_rule": RECENTRING_RULE,
            "bucket_separator_note": BUCKET_SEPARATOR_NOTE,
            "level_origin": LEVEL_ORIGIN,
            "delta_origin": DELTA_ORIGIN,
            "level_edges": list(LEVEL_EDGES),
            "delta_edges": list(DELTA_EDGES),
            "level_buckets": list(BUCKETS),
            "delta_buckets": list(DELTA_BUCKETS),
            "joint_delta_buckets": list(JOINT_DELTA_BUCKETS),
            "level_gate_pairs": {f: [list(p) for p in GATE_PAIRS[f]]
                                 for f in FAMILIES},
            "gate_pairs": {f: [list(p) for p in DELTA_GATE_PAIRS[f]]
                           for f in FAMILIES},
            "gate_initial_armed": GATE_INITIAL_ARMED,
            "lookback_denominator": DELTA_LOOKBACK_DENOMINATOR,
            "lookback_rationale": DELTA_LOOKBACK_RATIONALE,
            "lookback_bars": {str(hw): delta_lookback_bars(hw)
                              for hw in hurst_windows},
            "primary_lookback_bars": PRIMARY_LOOKBACK_BARS,
            "hurst_windows": list(hurst_windows),
            "sizing": {"gains": list(SIZING_GAINS),
                       "clamp_lo": SIZING_CLAMP_LO,
                       "clamp_hi": SIZING_CLAMP_HI,
                       "nan_multiplier": SIZING_NAN_MULTIPLIER},
            "inference_direction": INFERENCE_DIRECTION,
            "inference_direction_rationale": INFERENCE_DIRECTION_RATIONALE,
            "two_sided": TWO_SIDED,
            "two_sided_p_definition": TWO_SIDED_P_DEFINITION,
            "prior_exposure_disclosure": PRIOR_EXPOSURE_DISCLOSURE,
            "contract_path_claimed": CONTRACT_PATH_CLAIMED,
            "contract_path_statement": CONTRACT_PATH_STATEMENT,
            "no_promotion_sentence": NO_PROMOTION_SENTENCE,
            "sibling_deferral": list(SIBLING_DEFERRAL),
            "deferring_siblings": list(DEFERRING_SIBLINGS),
            "stage0_exclusion": STAGE0_EXCLUSION,
            "level_primary_config_id": LEVEL_PRIMARY_CONFIG_ID,
            "primary_config_id": PRIMARY_CONFIG_ID,
            "primary_config_ids": list(PRIMARY_CONFIG_IDS),
            "primary_family_size": PRIMARY_FAMILY_SIZE,
            "primary_hypothesis_statement": PRIMARY_HYPOTHESIS_STATEMENT,
            "primary_target": PRIMARY_TARGET,
            "continuity_target": CONTINUITY_TARGET,
            "horizon_hours": HORIZON_HOURS,
            "key_risk_prediction": KEY_RISK_PREDICTION,
            "feasibility_probes": [dict(pr) for pr in FEASIBILITY_PROBES],
            "window_owner": dict(WINDOW_OWNER),
            "dataset_windows": {
                f"{ex}={dataset_key(sym, tf)}": list(ws)
                for (ex, sym, tf), ws in sorted(DATASET_WINDOWS.items())},
            "history_since": dict(HISTORY_SINCE),
            "dataset_history_since": {
                f"{ex}={dataset_key(sym, tf)}": since
                for (ex, sym, tf), since in sorted(
                    DATASET_HISTORY_SINCE.items())},
            "fetch_page_limit": dict(FETCH_PAGE_LIMIT),
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
            "stamp_lead_bars": STAMP_LEAD_BARS,
            "warmup_margin_bars": WARMUP_MARGIN_BARS,
            "mde_eff_grid": [MDE_EFF_GRID_STEP, MDE_EFF_GRID_MAX,
                             MDE_EFF_REFINE_STEP],
            "mde_pp_grid": [MDE_PP_GRID_STEP, MDE_PP_GRID_MAX,
                            MDE_PP_REFINE_STEP],
            "windows": {k: list(WINDOWS[k]) for k in scored_windows},
            "primary_protocol_windows": list(PRIMARY_PROTOCOL_WINDOWS),
            "primary_protocol_min_windows": PRIMARY_PROTOCOL_MIN_WINDOWS,
            "primary_held_out_windows": list(PRIMARY_HELD_OUT_WINDOWS),
            "exploratory_protocol_windows": list(EXPLORATORY_PROTOCOL_WINDOWS),
            "exploratory_held_out_windows": list(EXPLORATORY_HELD_OUT_WINDOWS),
            "datasets": [dataset_key(qualified_symbol(ex, sym), tf)
                         for (ex, sym, tf) in usable_datasets],
            "fee_platform": FEE_PLATFORM,
            "capital": DEFAULT_CAPITAL,
        },
        "run_summary": {
            "scope": scope,
            "legs": len(legs),
            "gated_arms": sum(len(lg["gated"]) for lg in legs),
            "mirror_verified_legs": sum(1 for lg in legs
                                        if lg["mirror_verified"]),
            "raw_trades": raw_counts,
            "pooled_trades": {f: len(pooled[f]) for f in FAMILIES},
            "pooled_primary": {
                f: sum(1 for t in pooled[f] if t["cohort"] == COHORT_PRIMARY)
                for f in FAMILIES},
            "pooled_exploratory": {
                f: sum(1 for t in pooled[f]
                       if t["cohort"] == COHORT_EXPLORATORY)
                for f in FAMILIES},
            "pooled_with_target": {
                f: sum(1 for t in pooled[f]
                       if t.get("efficiency") is not None)
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
            "symbol_correlations": {
                f"{a}|{b}": v for (a, b), v in sorted(rho_by_symbol.items())},
            "elapsed_sec": round(time.time() - started, 2),
        },
        "mde": mde,
        "buckets": buckets,
        "joint": joint,
        "configs": configs,
        "legs": [{k: v for k, v in lg.items() if k != "trades"}
                 for lg in legs],
        "decision": decision_payload(decision),
    }

    with open(args.json_out, "w") as fh:
        json.dump(payload, fh, indent=2, sort_keys=False)
        fh.write("\n")
    print(f"[1427] wrote {args.json_out}")

    payload_for_report = dict(payload)
    payload_for_report["decision"] = decision
    report = render_report(payload_for_report)
    if args.write_report:
        with open(args.report_out, "w") as fh:
            fh.write(report)
        print(f"[1427] wrote {args.report_out}")
    else:
        print(render_recommendation(
            decision, mde, configs, predecessor))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
