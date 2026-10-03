#!/usr/bin/env python3

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

import numpy as np
import pandas as pd

from eval_windows import (
    DEFAULT_CAPITAL,
    FEE_PLATFORM,
    dataset_key,
)
from regime_stats import benjamini_hochberg

from hurst_gate import (
    HURST_DEFAULT_SIZE_FLOOR,
    HURST_GATE_MODE_SIZE,
    HURST_SIZE_SPAN,
    HurstGate,
)

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
HURST_WINDOWS = study1410.HURST_WINDOWS
SENSE_HIGH = study1410.SENSE_HIGH
SENSE_LOW = study1410.SENSE_LOW
SIZING_NAN_MULTIPLIER = study1410.SIZING_NAN_MULTIPLIER
WARMUP_MARGIN_BARS = study1410.WARMUP_MARGIN_BARS

bucket_label = study1410.bucket_label
cache_entry_is_usable = study1410.cache_entry_is_usable
cache_meta = study1410.cache_meta
chop_loss = study1410.chop_loss
compound_equity = study1410.compound_equity
decision_series = study1410.decision_series
entry_stamp_series = study1410.entry_stamp_series
mask_entry_signals = study1410.mask_entry_signals
required_lead_bars = study1410.required_lead_bars
rolling_hurst = study1410.rolling_hurst
slice_window = study1410.slice_window
warmup_audit = study1410.warmup_audit

ADX_PERIOD = study1422.ADX_PERIOD
ADX_SPLIT = study1422.ADX_SPLIT
COHORT_EXPLORATORY = study1422.COHORT_EXPLORATORY
COHORT_PRIMARY = study1422.COHORT_PRIMARY
D_1410 = study1422.D_1410
MIN_OFFSET_DAYS = study1422.MIN_OFFSET_DAYS

adx_entry_stamp = study1422.adx_entry_stamp
dedup_entries = study1422.dedup_entries
effective_n = study1422.effective_n
usable_cluster_rows = study1422.usable_cluster_rows
_separation = study1422._separation

ALPHA = study1424.ALPHA
CONTINUITY_TARGET = study1424.CONTINUITY_TARGET
DATASETS = study1424.DATASETS
DATASET_HISTORY_SINCE = study1424.DATASET_HISTORY_SINCE
DATASET_WINDOWS = study1424.DATASET_WINDOWS
EXPLORATORY_HELD_OUT_WINDOWS = study1424.EXPLORATORY_HELD_OUT_WINDOWS
EXPLORATORY_PROTOCOL_MIN_WINDOWS = study1424.EXPLORATORY_PROTOCOL_MIN_WINDOWS
EXPLORATORY_PROTOCOL_WINDOWS = study1424.EXPLORATORY_PROTOCOL_WINDOWS
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
PRIMARY_CONFIG_ID = study1424.PRIMARY_CONFIG_ID
PRIMARY_FAMILY = study1424.PRIMARY_FAMILY
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
cell_cohort = study1424.cell_cohort
coverage_audit = study1424.coverage_audit
held_out_verdict = study1424.held_out_verdict
history_since_for = study1424.history_since_for
horizon_bars = study1424.horizon_bars
owned_windows = study1424.owned_windows
protocol_verdict = study1424.protocol_verdict
qualified_symbol = study1424.qualified_symbol
resolve_primary_config_id = study1424.resolve_primary_config_id
scored_warmup_leads = study1424.scored_warmup_leads
signed_efficiency = study1424.signed_efficiency
symbol_return_correlations = study1424.symbol_return_correlations
trade_direction = study1424.trade_direction
trade_samples_with_side = study1424.trade_samples_with_side
_fmt = study1424._fmt
_fmt_p = study1424._fmt_p
_fmt_signed = study1424._fmt_signed
_leg_metrics = study1424._leg_metrics
_target_rows = study1424._target_rows

TWO_SIDED_P_DEFINITION = study1426.TWO_SIDED_P_DEFINITION
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

_JSON_1410 = study1424._JSON_1410
_JSON_1424 = study1424._DEFAULT_JSON_OUT

_DEFAULT_JSON_OUT = os.path.join(_THIS_DIR, "hurst_1428_sizing_exit.json")
_DEFAULT_REPORT_OUT = os.path.join(_THIS_DIR, "hurst_1428_sizing_exit.md")
CONTRACT_REPORT_BASENAME = "hurst_gate_calibration.md"
_CONTRACT_REPORT_OUT = os.path.join(_THIS_DIR, CONTRACT_REPORT_BASENAME)

SCHEMA_VERSION = 1
ISSUE = 1428
SEED = ISSUE

ARM_SIZING = "sizing"
ARM_EXIT = "exit"
ARMS = (ARM_SIZING, ARM_EXIT)

STUDY_PREMISE = (
    "Every Hurst study on this repository's record asks ADMISSION: should "
    "this trade be allowed to happen. #1410, #1422, #1424, #1426 and #1427 "
    "all score an entry decision. Two questions that framing never reaches "
    "are how BIG the trade should be and when it should END, and this study "
    "asks exactly those two, on the same tape, under the same power "
    "discipline.")

SHIPPED_SIZE_SPAN = HURST_SIZE_SPAN
SHIPPED_SIZE_FLOOR = HURST_DEFAULT_SIZE_FLOOR
SHIPPED_SIZE_CEILING = 1.0
SHIPPED_NAN_MULTIPLIER = 1.0

_SHIPPED_GATE = HurstGate({"enabled": True, "mode": HURST_GATE_MODE_SIZE,
                           "size_floor": SHIPPED_SIZE_FLOOR})


def shipped_size_multiplier(h) -> float:
    return float(_SHIPPED_GATE.size_multiplier(h))


SHIPPED_FORM_STATEMENT = (
    "SHIPPED FORM, the reason this study exists. `scheduler/hurst_gate.go` "
    "implements `mode: size` as "
    "`clamp(|H - 0.5| / 0.15, size_floor, 1.0)` and applies it where the "
    "position size is COMPUTED, and `backtest/hurst_gate.py` mirrors it "
    "bar-for-bar as the parity module the Backtester already shares. #1410 "
    "swept a DIFFERENT curve, `clamp(1 + gain * e, 0, 1.5)`, which can "
    "exceed 1.0 and therefore describes something the shipped code cannot "
    "do. No committed artefact has ever scored the form the code actually "
    "implements, so the shipped multiplier is uncalibrated BY CONSTRUCTION. "
    "This study does not re-implement the form. It CALLS "
    "`hurst_gate.HurstGate.size_multiplier`, the same object the Backtester "
    "and `parity_diff.py` use, so a drift in the shipped form cannot leave "
    "this study scoring a stale curve: the import would move with it, and "
    "the import-time assertions below fail loud if the landmarks change.")

assert shipped_size_multiplier(0.5) == SHIPPED_SIZE_FLOOR
assert shipped_size_multiplier(0.5 + SHIPPED_SIZE_SPAN) == SHIPPED_SIZE_CEILING
assert shipped_size_multiplier(0.5 - SHIPPED_SIZE_SPAN) == SHIPPED_SIZE_CEILING
assert shipped_size_multiplier(1.0) == SHIPPED_SIZE_CEILING
assert shipped_size_multiplier(0.0) == SHIPPED_SIZE_CEILING
assert shipped_size_multiplier(None) == SHIPPED_NAN_MULTIPLIER
assert shipped_size_multiplier(float("nan")) == SHIPPED_NAN_MULTIPLIER
assert SHIPPED_NAN_MULTIPLIER == SIZING_NAN_MULTIPLIER

SIZE_CEILING_BAND = SHIPPED_SIZE_SPAN

SIZING_CONTRAST_STATEMENT = (
    "SIZING VALIDITY CONTRAST, stated per arm because the gate's own "
    "kept-versus-suppressed partition does not survive the move to sizing. "
    "#1424 already defines a size-arm contrast: weighted permutation "
    "p-values, plus a `multiplier < 1.0` versus `multiplier >= 1.0` split "
    "that feeds the effective-N coverage floors. That split is well behaved "
    "for #1410's curve, which can exceed 1.0, so its `>= 1.0` side is a "
    "genuine interval. Under the SHIPPED form the multiplier is CAPPED at "
    "1.0, so `>= 1.0` collapses to the exact-equality set "
    "`|H - 0.5| >= 0.15`, which is the TAIL of the H distribution rather "
    "than an interval around its middle. The contrast is still row-matched "
    "and still signed, and it is the honest one for this form, but its kept "
    "side is small by construction. This study therefore reports the "
    "kept-side effective N beside every sizing verdict and treats a breach "
    "of the 30-effective-trade floor as a POWER statement, never as a null "
    "about the market.")

EXIT_FORM = "persistence_scaled_trailing_stop"
EXIT_MECHANISM = "bucket_conditional_reruns"
EXIT_BASE_TRAIL_ATR_MULT = 2.0
EXIT_TRAIL_GAIN = 0.5
EXIT_NEUTRAL_SCALE = 1.0
EXIT_PRIMARY_TARGET = "pnl_pct_net"
EXIT_CONTINUITY_TARGET = PRIMARY_TARGET
EXIT_PERSISTENT_EDGE = 0.55
EXIT_ANTIPERSISTENT_EDGE = 0.45

EXIT_FORM_STATEMENT = (
    "EXIT FORM, ONE rule pre-registered before the run and never swept, "
    "because power is the binding constraint on this tape and a second form "
    "would double the hypothesis family for nothing. A trade's trailing "
    "stop distance is scaled by the PERSISTENCE side its entry bar sits on: "
    "the persistent side widens the trail by "
    f"{EXIT_TRAIL_GAIN:g}, the anti-persistent side tightens it by the same "
    "amount, the two middle buckets and the NaN bucket keep the base "
    "distance exactly. The landmarks are #1410's own committed bucket "
    f"edges ({EXIT_ANTIPERSISTENT_EDGE:g} and {EXIT_PERSISTENT_EDGE:g}), not "
    "new constants, and 'persistent side' is read through the family's "
    "SENSE so a mean-reversion exemplar orients the same way its entry gate "
    "does. The base distance is "
    f"{EXIT_BASE_TRAIL_ATR_MULT:g} x ATR, the value this repository already "
    "uses as its manual-strategy stop default and the same multiple "
    "`EXEMPLAR_CLOSE_OVERRIDES` pins for `atr_band_revert`. THE HYPOTHESIS "
    "IS THAT PERSISTENCE PREDICTS HOW FAR A MOVE RUNS, so a persistent "
    "entry should be given more room before the trail takes it out.")

EXIT_MECHANISM_STATEMENT = (
    "EXIT MECHANISM, named because the Backtester does not expose a "
    "per-trade trailing distance and the issue's 'real Backtester re-runs' "
    "criterion constrains the mechanism without picking it. `Backtester` "
    "takes ONE scalar `trailing_stop_atr_mult` per run, so a "
    "persistence-conditional trail cannot be expressed inside a single run. "
    "This study uses BUCKET-CONDITIONAL RE-RUNS: for each persistence "
    "bucket it masks entry signals down to that bucket exactly as the gate "
    "arm masks them, and runs the Backtester twice on the masked series - "
    "once at the base distance and once at that bucket's scaled distance. "
    "Every leg on both sides is a real `Backtester.run`, and the two sides "
    "share their entry masking, so the contrast isolates the EXIT change "
    "rather than the masking. A research-local close hook was refused: it "
    "would score a code path the live system does not have.")

EXIT_CONTRAST_STATEMENT = (
    "EXIT VALIDITY CONTRAST, stated per arm because an exit change has no "
    "kept-versus-suppressed partition at all: it modifies EVERY trade's "
    "exit rather than removing some trades. The contrast is therefore "
    "PAIRED and row-matched by construction. A bucket's masked baseline run "
    "and its masked scaled run admit the SAME entries, so each entry bar "
    "carries both outcomes, and the row's value is the signed difference "
    "`scaled - baseline` on the arm's primary target. Rows in the neutral "
    "and NaN buckets take the base distance on both sides, so their "
    "difference is exactly 0 by construction and they form the control "
    "side. The published separation is "
    "`mean(difference | scaled bucket) - mean(difference | neutral bucket)`, "
    "which is signed, row-matched, and reduces to the mean paired effect. "
    "The cluster-rotation null rotates the bucket labels over the calendar "
    "exactly as #1422 rotates the gate's, so correlated concurrent trades "
    "still move together. An entry that appears on only one side of a pair "
    "is DROPPED and counted, never imputed.")

INFERENCE_DIRECTION = "two_sided"
TWO_SIDED = True

INFERENCE_DIRECTION_RATIONALE = (
    "DIRECTION, pre-registered as a constant: the test is TWO-SIDED on both "
    "arms. Both hypotheses are naturally directional - a bigger size on a "
    "more persistent entry should pay, and a wider trail on a persistent "
    "entry should pay - and a one-sided test of either would be cheaper in "
    "power. It is refused for the reason already on this repository's "
    "record: the only effect ever MEASURED on these rows, #1424's "
    "confirmatory separation of -0.005 efficiency units, pointed the way "
    "its one-sided design could not detect at any size. #1426 exists solely "
    "to remove that blind spot and #1427 kept it removed. Re-introducing it "
    "here would repeat the same mistake on the same tape. THE COST IS REAL: "
    "a two-sided limit can only be at or above its one-sided counterpart at "
    "the same alpha. The SIGN is carried and reported everywhere.")

PRIOR_EXPOSURE_DISCLOSURE = (
    "PRIOR EXPOSURE, disclosed before the run. The OUTCOME rows are the "
    "same tape #1424, #1426 and #1427 scored, and their results are "
    "committed. Neither PREDICTOR is: no committed artefact in this "
    "repository has ever scored the shipped sizing form, and none has ever "
    "tested Hurst on the exit side at all, so neither contrast has been "
    "seen and neither sign was known when these constants were fixed. A "
    "pre-registered confirmatory claim is therefore available on both arms. "
    "What is NOT available is a claim of an independent sample: the "
    "outcomes are shared, the effective sample size is set by the same "
    "calendar clusters, and the detection limit is of the same order as "
    "#1424's. Read a finding here as evidence about NEW quantities on OLD "
    "rows.")

CONTRACT_PATH_CLAIM_RULE = (
    "CONTRACT PATH, resolved MECHANICALLY from this run's verdict rather "
    "than asserted up front. `hurst_gate_calibration.md` is the "
    "live-evidence path cited by `scheduler/hurst_gate.go` and #1412's "
    "Stage 0. #1426 (PR #1452) DEFERS it because it is exploratory-only, "
    "and #1427 (PR #1473) DEFERS it because the shipped gate reads the "
    "LEVEL of H and never its change, so its committed artefact passes the "
    "supersede clause to this study alone. This study is the only remaining "
    "one that scores a quantity the shipped code implements, so the clause "
    "is available to it. It is claimed ONLY on a confirmatory result, which "
    "is the precedent #1426 set and the maintainer restated on the issue: "
    "an exploratory or inconclusive run must not move the live evidence, "
    "whatever it finds. `claims_contract_path` below reads the decision "
    "object, so no human judgement sits between the verdict and the claim.")

NO_PROMOTION_SENTENCE = (
    "This study ships NO threshold and recommends NO configuration. Both "
    "arms measure whether a quantity SORTS outcomes; neither searches a "
    "threshold, and `decide_recommendation` has no branch that promotes "
    "one. A positive finding licenses a follow-up DESIGN issue, never a "
    "shipped constant from this run.")

KEY_RISK_PREDICTION = (
    "The pre-registered prediction is INCONCLUSIVE on both arms, and the "
    "reason is a power reason rather than a market one. Effective N here is "
    "set by independent CALENDAR CLUSTERS rather than rows, and this study "
    "scores the same calendar #1424 scored, so its two-sided detection "
    "limit can only be at or above the 0.013 efficiency units #1426 "
    "measured. The sizing arm carries a second, sharper power risk that is "
    "specific to the shipped form: its kept side is the tail "
    f"`|H - 0.5| >= {SIZE_CEILING_BAND:g}`, so the kept-side effective N may "
    "fall under the 30-trade floor on its own. The falsifiable half is the "
    "limit: if a measured limit comes back BELOW 0.013 on these rows, this "
    "prediction's stated mechanism was wrong. The machinery decides the "
    "verdict either way.")

VERDICT_INCONCLUSIVE = "inconclusive"
VERDICT_RESOLVED_NULL = "resolved_null"
VERDICT_SORTS = "sorts"

MODE_OK = "resolvable"
MODE_BELOW_LIMIT = "below_limit"
MODE_NO_SEPARATION = "no_separation"
MODE_UNRESOLVABLE = "unresolvable"
MODE_FLOOR_BREACH = "floor_breach"

ARM_TARGETS = {
    ARM_SIZING: {"primary": PRIMARY_TARGET, "continuity": CONTINUITY_TARGET},
    ARM_EXIT: {"primary": EXIT_PRIMARY_TARGET,
               "continuity": EXIT_CONTINUITY_TARGET},
}

ARM_TARGET_STATEMENT = (
    "TARGETS. The sizing arm keeps #1424's pairing unchanged - signed "
    "fixed-horizon Kaufman efficiency over "
    f"{HORIZON_HOURS}h as the primary, net return as continuity - because a "
    "size change does not move when a trade ends, so a fixed-horizon "
    "outcome stays well defined on its rows. The exit arm SWAPS them: its "
    "whole purpose is to change when a trade ends, which makes a "
    "FIXED-horizon statistic a poor primary, so net return on the ACTUAL "
    "holding period is primary there and efficiency is retained as "
    "continuity. The swap is a property of the arm and is pre-registered "
    "here, not chosen after the numbers were seen.")

DEGENERATE_LIMIT_DISCLOSURE = (
    "A detection limit of 0.000 is DEGENERATE, not excellent. The injection "
    "search returns it when the zero-injection contrast already clears the "
    "significance bar, so the smallest grid step is 'detectable' only "
    "because the un-injected data was already significant. A validity gate "
    "that passes against a 0.000 limit therefore certifies nothing about "
    "the effect SIZE, and this study labels every such pass explicitly "
    "rather than reading it as a resolved measurement. #1427 hit exactly "
    "this case and its artefact says so; the same disclosure is inherited "
    "here unchanged.")


def _sense_signed_h(h, sense: str) -> Optional[float]:
    if h is None:
        return None
    h = float(h)
    if not math.isfinite(h):
        return None
    return h if sense == SENSE_HIGH else (1.0 - h)


def exit_bucket(h, sense: str) -> str:
    signed = _sense_signed_h(h, sense)
    if signed is None:
        return BUCKET_NAN
    if signed >= EXIT_PERSISTENT_EDGE:
        return "persistent"
    if signed < EXIT_ANTIPERSISTENT_EDGE:
        return "anti_persistent"
    return "neutral"


EXIT_BUCKETS = ("persistent", "neutral", "anti_persistent", BUCKET_NAN)
EXIT_SCALED_BUCKETS = ("persistent", "anti_persistent")

EXIT_BUCKET_SCALES = {
    "persistent": EXIT_NEUTRAL_SCALE + EXIT_TRAIL_GAIN,
    "neutral": EXIT_NEUTRAL_SCALE,
    "anti_persistent": EXIT_NEUTRAL_SCALE - EXIT_TRAIL_GAIN,
    BUCKET_NAN: EXIT_NEUTRAL_SCALE,
}

assert set(EXIT_BUCKET_SCALES) == set(EXIT_BUCKETS)
assert all(EXIT_BUCKET_SCALES[b] != EXIT_NEUTRAL_SCALE
           for b in EXIT_SCALED_BUCKETS)
assert all(EXIT_BUCKET_SCALES[b] == EXIT_NEUTRAL_SCALE
           for b in EXIT_BUCKETS if b not in EXIT_SCALED_BUCKETS)
assert EXIT_BUCKET_SCALES["anti_persistent"] > 0


def exit_trail_mult(bucket: str) -> float:
    return round(EXIT_BASE_TRAIL_ATR_MULT * EXIT_BUCKET_SCALES[bucket], 6)


def sizing_config_id(family: str, hw: int) -> str:
    return CONFIG_ID_SEP.join([family, ARM_SIZING, f"W{int(hw)}", "shipped"])


def exit_config_id(family: str, hw: int) -> str:
    return CONFIG_ID_SEP.join(
        [family, ARM_EXIT, f"W{int(hw)}", f"g{EXIT_TRAIL_GAIN:g}"])


EXIT_HURST_WINDOW = 512

assert f"W{EXIT_HURST_WINDOW}" in PRIMARY_CONFIG_ID, (
    "the exit arm pins the rolling-Hurst window #1424's committed primary "
    "hypothesis uses; a drift there must be re-registered deliberately")
assert EXIT_HURST_WINDOW in HURST_WINDOWS

EXIT_WINDOW_STATEMENT = (
    f"EXIT ARM WINDOW. The exit arm scores exactly ONE rolling-Hurst window, "
    f"W{EXIT_HURST_WINDOW}, the window #1424's committed primary hypothesis "
    f"`{PRIMARY_CONFIG_ID}` already pins. Sweeping all three would triple "
    "this arm's hypothesis family and its Backtester cost for a design "
    "question the study is not asking, and the pin is inherited rather than "
    "chosen here. It is asserted against #1424's committed config id at "
    "import time, so a drift there fails loud instead of silently scoring a "
    "different window.")


def arm_primary_value(trade: dict, arm: str):
    if arm == ARM_SIZING:
        return trade.get("efficiency")
    return trade.get("delta_primary")


def arm_continuity_value(trade: dict, arm: str):
    if arm == ARM_SIZING:
        return trade.get("pnl_pct_net")
    return trade.get("delta_continuity")


def _run_exit_arm(reg, name: str, symbol: str, timeframe: str,
                  df: pd.DataFrame, armed, overrides: dict,
                  trail_mult: Optional[float]) -> Optional[dict]:
    from atr import ensure_atr_indicator
    from backtester import Backtester
    from eval_windows import leg_from_results
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
    if close_strategies or trail_mult is not None:
        df_signals = ensure_atr_indicator(df_signals)

    bt = Backtester(
        initial_capital=DEFAULT_CAPITAL, platform=FEE_PLATFORM,
        open_strategy={"name": name, "params": dict(strat_params or {})},
        close_strategies=close_strategies,
        direction=None, invert_signal=False,
        stop_loss_atr_mult=overrides.get("stop_loss_atr_mult"),
        trailing_stop_atr_mult=trail_mult,
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
    leg["trade_samples"] = trade_samples_with_side(results)
    return leg


def _exit_ns(sample: dict) -> Optional[int]:
    try:
        return int(pd.Timestamp(sample["exit_date"]).value)
    except (ValueError, TypeError, KeyError):
        return None


PAIRED_EXIT_NS_RULE = (
    "A paired row has TWO exits, one per side, and the cluster model needs "
    "one holding interval per row. The row takes the LATER of the two, so an "
    "overlap between two rows is never understated and the effective-N "
    "correction can only be conservative. A row whose exit is unreadable on "
    "either side carries `exit_ns=None` and is dropped by `effective_n` "
    "exactly as #1422 drops such a row, rather than being imputed.")


def pair_exit_rows(base_leg: Optional[dict], scaled_leg: Optional[dict],
                   closes, key_pos: dict, k_bars: int) -> tuple:
    if base_leg is None or scaled_leg is None:
        return [], 0
    base_by_key = {}
    for s in base_leg.get("trade_samples") or []:
        base_by_key.setdefault(str(s["entry_date"]), s)
    scaled_by_key = {}
    for s in scaled_leg.get("trade_samples") or []:
        scaled_by_key.setdefault(str(s["entry_date"]), s)

    rows = []
    n_unpaired = 0
    for key in sorted(set(base_by_key) | set(scaled_by_key)):
        b = base_by_key.get(key)
        c = scaled_by_key.get(key)
        if b is None or c is None:
            n_unpaired += 1
            continue
        pos = key_pos.get(key)
        if pos is None:
            raise AssertionError(
                f"paired entry_date {key!r} is not a bar of the scored slice")
        direction = trade_direction(b.get("side"))
        eff = signed_efficiency(closes, pos, k_bars, direction)
        exits = [_exit_ns(s) for s in (b, c)]
        exits = [v for v in exits if v is not None]
        rows.append({
            "entry_date": key,
            "entry_ns": int(pd.Timestamp(key).value),
            "pos": pos,
            "side": b.get("side"),
            "efficiency": None if eff is None else round(float(eff), 6),
            "base_exit_ns": _exit_ns(b),
            "scaled_exit_ns": _exit_ns(c),
            "exit_ns": (max(exits) if exits else None),
            "base_pnl_pct_net": float(b["pnl_pct_net"]),
            "scaled_pnl_pct_net": float(c["pnl_pct_net"]),
            "delta_primary": round(float(c["pnl_pct_net"])
                                   - float(b["pnl_pct_net"]), 6),
        })
    return rows, n_unpaired


def build_leg(reg, family: str, exemplar: str, dataset: tuple,
              window_name: str, full: pd.DataFrame, hurst_by_window: dict,
              adx_stamp: pd.Series, run_exit: bool = True) -> Optional[dict]:
    exchange_id, symbol, timeframe = dataset
    qsym = qualified_symbol(exchange_id, symbol)
    window = WINDOWS[window_name]
    overrides = EXEMPLAR_CLOSE_OVERRIDES.get(exemplar, {})
    df = slice_window(full, window)
    if df.empty:
        return None
    sense = FAMILY_SENSE[family]

    ungated = study1424._run_arm(reg, exemplar, symbol, timeframe, df, None,
                                 overrides)
    if ungated is None:
        return None

    index_keys = [str(ts) for ts in df.index]
    key_pos = {k: i for i, k in enumerate(index_keys)}
    closes = df["close"].to_numpy(dtype=float)
    k_bars = horizon_bars(timeframe)

    stamps = {}
    decisions = {}
    for hw, rolling in hurst_by_window.items():
        stamps[hw] = entry_stamp_series(rolling).reindex(
            df.index).to_numpy(dtype=float)
        decisions[hw] = decision_series(rolling).reindex(
            df.index).to_numpy(dtype=float)
    adx_vals = adx_stamp.reindex(df.index).to_numpy(dtype=float)

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
        eff = signed_efficiency(closes, pos, k_bars,
                                trade_direction(sample.get("side")))
        if eff is None:
            n_horizon_excluded += 1
        h_by_hw = {hw: (None if not math.isfinite(stamps[hw][pos])
                        else float(stamps[hw][pos])) for hw in hurst_by_window}
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
            "exit_ns": _exit_ns(sample),
            "pnl_pct_net": float(sample["pnl_pct_net"]),
            "efficiency": None if eff is None else round(float(eff), 6),
            "adx": (None if not math.isfinite(adx_vals[pos])
                    else float(adx_vals[pos])),
            "h": h_by_hw,
            "size_mult": {hw: shipped_size_multiplier(h_by_hw[hw])
                          for hw in hurst_by_window},
        })

    exit_rows = []
    exit_meta = {"n_unpaired": 0, "buckets": {}, "ran": False}
    if run_exit and EXIT_HURST_WINDOW in hurst_by_window:
        exit_meta["ran"] = True
        decision_vals = decisions[EXIT_HURST_WINDOW]
        stamp_vals = stamps[EXIT_HURST_WINDOW]
        bucket_at_bar = np.array(
            [exit_bucket(None if not math.isfinite(v) else float(v), sense)
             for v in decision_vals], dtype=object)
        base_trail = float(EXIT_BASE_TRAIL_ATR_MULT)
        for bucket in EXIT_BUCKETS:
            mask = (bucket_at_bar == bucket)
            if not mask.any():
                exit_meta["buckets"][bucket] = {"n_bars": 0, "n_rows": 0}
                continue
            base_leg = _run_exit_arm(reg, exemplar, symbol, timeframe, df,
                                     mask, overrides, base_trail)
            if EXIT_BUCKET_SCALES[bucket] == EXIT_NEUTRAL_SCALE:
                scaled_leg = base_leg
            else:
                scaled_leg = _run_exit_arm(reg, exemplar, symbol, timeframe,
                                           df, mask, overrides,
                                           exit_trail_mult(bucket))
            rows, n_unpaired = pair_exit_rows(base_leg, scaled_leg, closes,
                                              key_pos, k_bars)
            exit_meta["n_unpaired"] += n_unpaired
            exit_meta["buckets"][bucket] = {
                "n_bars": int(mask.sum()), "n_rows": len(rows),
                "trail_atr_mult": exit_trail_mult(bucket),
                "scale": EXIT_BUCKET_SCALES[bucket],
            }
            for row in rows:
                pos = row["pos"]
                h_val = (None if not math.isfinite(stamp_vals[pos])
                         else float(stamp_vals[pos]))
                exit_rows.append({
                    "strategy": exemplar,
                    "exchange": exchange_id,
                    "symbol": qsym,
                    "base_symbol": symbol,
                    "timeframe": timeframe,
                    "window": window_name,
                    "cohort": cohort,
                    "entry_date": row["entry_date"],
                    "entry_ns": row["entry_ns"],
                    "exit_ns": row["exit_ns"],
                    "base_exit_ns": row["base_exit_ns"],
                    "scaled_exit_ns": row["scaled_exit_ns"],
                    "bucket": bucket,
                    "scaled": bucket in EXIT_SCALED_BUCKETS,
                    "trail_atr_mult": exit_trail_mult(bucket),
                    "h": {EXIT_HURST_WINDOW: h_val},
                    "efficiency": row["efficiency"],
                    "base_pnl_pct_net": row["base_pnl_pct_net"],
                    "scaled_pnl_pct_net": row["scaled_pnl_pct_net"],
                    "pnl_pct_net": row["scaled_pnl_pct_net"],
                    "delta_primary": row["delta_primary"],
                })

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
        "ungated": _leg_metrics(ungated),
        "trades": trades,
        "exit_rows": exit_rows,
        "exit_meta": exit_meta,
    }


def _cohort_legs(legs: Sequence[dict], family: str, cohort: str) -> list:
    return [lg for lg in legs
            if lg["family"] == family and lg["cohort"] == cohort]


def _window_rows_sizing(legs: Sequence[dict], family: str, cohort: str,
                        hurst_window: int) -> dict:
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
            mults = [shipped_size_multiplier((t.get("h") or {}).get(hurst_window))
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


def _window_rows_exit(legs: Sequence[dict], family: str, cohort: str) -> dict:
    rows = {}
    own = _cohort_legs(legs, family, cohort)
    for wname in WINDOW_ORDER:
        cells = [lg for lg in own if lg["window"] == wname]
        if not cells:
            rows[wname] = {"n_legs": 0}
            continue
        dd_deltas, chop_deltas, ret_g, ret_u = [], [], [], []
        n_used = 0
        n_rows = 0
        for lg in cells:
            pairs = lg.get("exit_rows") or []
            if not pairs:
                continue
            pairs = sorted(pairs, key=lambda r: r["entry_ns"])
            base = [r["base_pnl_pct_net"] for r in pairs]
            scaled = [r["scaled_pnl_pct_net"] for r in pairs]
            base_ret, base_dd = compound_equity(base)
            scaled_ret, scaled_dd = compound_equity(scaled)
            dd_deltas.append(abs(scaled_dd) - abs(base_dd))
            chop_deltas.append(chop_loss(scaled) - chop_loss(base))
            ret_g.append(scaled_ret)
            ret_u.append(base_ret)
            n_used += 1
            n_rows += len(pairs)
        if not n_used:
            rows[wname] = {"n_legs": 0}
            continue
        rows[wname] = {
            "n_legs": n_used,
            "dd_delta": round(float(np.mean(dd_deltas)), 6),
            "chop_delta": round(float(np.mean(chop_deltas)), 6),
            "ret_gated": round(float(np.mean(ret_g)), 6),
            "ret_ungated": round(float(np.mean(ret_u)), 6),
            "trades_gated": n_rows,
            "trades_ungated": n_rows,
        }
    return rows


def _config_shell(family: str, cohort: str, arm: str, hw: int) -> dict:
    cid = (sizing_config_id(family, hw) if arm == ARM_SIZING
           else exit_config_id(family, hw))
    if cohort == COHORT_PRIMARY:
        protocol = PRIMARY_PROTOCOL_WINDOWS
        protocol_min = PRIMARY_PROTOCOL_MIN_WINDOWS
        held_out = PRIMARY_HELD_OUT_WINDOWS
    else:
        protocol = EXPLORATORY_PROTOCOL_WINDOWS
        protocol_min = EXPLORATORY_PROTOCOL_MIN_WINDOWS
        held_out = EXPLORATORY_HELD_OUT_WINDOWS
    return {
        "config_id": cid,
        "cohort": cohort,
        "family": family,
        "arm": arm,
        "sense": FAMILY_SENSE[family],
        "hurst_window": int(hw),
        "primary_target": ARM_TARGETS[arm]["primary"],
        "continuity_target": ARM_TARGETS[arm]["continuity"],
        "protocol_windows": list(protocol),
        "protocol_min_windows": protocol_min,
        "held_out_windows": list(held_out),
    }


def _sweep_grid(hurst_windows: Sequence[int]) -> list:
    grid = []
    for family in FAMILIES:
        for hw in hurst_windows:
            grid.append((family, ARM_SIZING, int(hw)))
        grid.append((family, ARM_EXIT, EXIT_HURST_WINDOW))
    return grid


FAMILY_SIZE_BY_ARM = {
    ARM_SIZING: len(FAMILIES) * len(HURST_WINDOWS),
    ARM_EXIT: len(FAMILIES),
}


def sizing_rows(trades: Sequence[dict], hurst_window: int) -> tuple:
    keep, values, returns, mults, suppressed = [], [], [], [], []
    for t in trades:
        if t.get("efficiency") is None:
            continue
        m = float((t.get("size_mult") or {}).get(hurst_window,
                                                 shipped_size_multiplier(
                                                     (t.get("h") or {}).get(hurst_window))))
        keep.append(t)
        values.append(float(t["efficiency"]))
        returns.append(float(t["pnl_pct_net"]))
        mults.append(m)
        suppressed.append(m < SHIPPED_SIZE_CEILING)
    return keep, values, returns, mults, suppressed


def exit_contrast_rows(rows: Sequence[dict]) -> tuple:
    keep, values, returns, suppressed = [], [], [], []
    for r in rows:
        if r.get("delta_primary") is None:
            continue
        keep.append(r)
        values.append(float(r["delta_primary"]))
        returns.append(float(r["delta_primary"]))
        suppressed.append(not bool(r.get("scaled")))
    return keep, values, returns, suppressed


def build_configs(legs: Sequence[dict], pooled: dict, pooled_exit: dict,
                  hurst_windows: Sequence[int], rho_by_symbol: dict,
                  n_perm: int, seed: int) -> list:
    configs = []
    for cohort in (COHORT_PRIMARY, COHORT_EXPLORATORY):
        for family, arm, hw in _sweep_grid(hurst_windows):
            cfg = _config_shell(family, cohort, arm, hw)
            if arm == ARM_SIZING:
                trades = [t for t in (pooled.get(family) or [])
                          if t["cohort"] == cohort]
                sub, values, returns, weights, suppressed = sizing_rows(
                    trades, hw)
            else:
                trades = [t for t in (pooled_exit.get(family) or [])
                          if t["cohort"] == cohort]
                sub, values, returns, suppressed = exit_contrast_rows(trades)
                weights = None

            idx, excluded = usable_cluster_rows(sub)
            n_excluded = len(sub) - len(idx)
            sub = [sub[i] for i in idx]
            values = [values[i] for i in idx]
            returns = [returns[i] for i in idx]
            suppressed = [suppressed[i] for i in idx]
            if weights is not None:
                weights = [weights[i] for i in idx]

            if arm == ARM_SIZING:
                cfg["p_raw"] = two_sided_permutation_pvalue_weighted(
                    values, weights, n_perm=n_perm, seed=seed)
                cluster = two_sided_cluster_permutation_pvalue_weighted(
                    sub, values, weights, n_perm=n_perm, seed=seed)
                cfg["p_raw_return"] = two_sided_permutation_pvalue_weighted(
                    returns, weights, n_perm=n_perm, seed=seed)
                cfg["p_cluster_return"] = (
                    two_sided_cluster_permutation_pvalue_weighted(
                        sub, returns, weights, n_perm=n_perm, seed=seed).get("p"))
                sup_rows = [t for t, s in zip(sub, suppressed) if s]
                kept_rows = [t for t, s in zip(sub, suppressed) if not s]
                cfg["windows"] = _window_rows_sizing(legs, family, cohort, hw)
            else:
                cfg["p_raw"] = two_sided_permutation_pvalue_group_diff(
                    values, suppressed, n_perm=n_perm, seed=seed)
                cluster = two_sided_cluster_permutation_pvalue_group_diff(
                    sub, values, suppressed, n_perm=n_perm, seed=seed)
                cfg["p_raw_return"] = cfg["p_raw"]
                cfg["p_cluster_return"] = cluster.get("p")
                sup_rows = [t for t, s in zip(sub, suppressed) if s]
                kept_rows = [t for t, s in zip(sub, suppressed) if not s]
                cfg["windows"] = _window_rows_exit(legs, family, cohort)

            cfg["separation"] = _separation(values, suppressed)
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
    for cohort in (COHORT_PRIMARY, COHORT_EXPLORATORY):
        for arm in ARMS:
            own = [c for c in configs
                   if c.get("cohort") == cohort and c.get("arm") == arm]
            for cfg in own:
                cfg["bh_reject"] = False
            testable = [c for c in own if c.get("p_cluster") is not None]
            if not testable:
                continue
            flags = benjamini_hochberg([c["p_cluster"] for c in testable],
                                       alpha=alpha, family_size=len(own))
            for cfg, flag in zip(testable, flags):
                cfg["bh_reject"] = bool(flag)


def measure_detection_limits(pooled: dict, pooled_exit: dict,
                             n_perm: int, seed: int) -> dict:
    out: dict = {
        "by_arm": {},
        "primary_targets": {a: ARM_TARGETS[a]["primary"] for a in ARMS},
        "continuity_targets": {a: ARM_TARGETS[a]["continuity"] for a in ARMS},
        "horizon_hours": HORIZON_HOURS,
        "sizing_hurst_window": EXIT_HURST_WINDOW,
        "exit_hurst_window": EXIT_HURST_WINDOW,
        "two_sided": TWO_SIDED,
    }
    for arm in ARMS:
        per_family = {"cluster": {}, "separation": {}, "n": {},
                      "n_kept_effective": {}, "n_suppressed_effective": {}}
        family_size = FAMILY_SIZE_BY_ARM[arm]
        for family in FAMILIES:
            if arm == ARM_SIZING:
                rows = [t for t in (pooled.get(family) or [])
                        if t["cohort"] == COHORT_PRIMARY]
                sub, values, _returns, _w, suppressed = sizing_rows(
                    rows, EXIT_HURST_WINDOW)
            else:
                rows = [t for t in (pooled_exit.get(family) or [])
                        if t["cohort"] == COHORT_PRIMARY]
                sub, values, _returns, suppressed = exit_contrast_rows(rows)
            idx, _excluded = usable_cluster_rows(sub)
            sub = [sub[i] for i in idx]
            values = [values[i] for i in idx]
            suppressed = [suppressed[i] for i in idx]
            if arm == ARM_SIZING:
                limit = two_sided_min_detectable_effect_eff(
                    sub, values, suppressed, family_size, cluster=True,
                    n_perm=n_perm, seed=seed)
            else:
                limit = two_sided_min_detectable_effect_pp(
                    sub, values, suppressed, family_size, cluster=True,
                    n_perm=n_perm, seed=seed)
            per_family["cluster"][family] = limit
            per_family["separation"][family] = _separation(values, suppressed)
            per_family["n"][family] = len(sub)
        per_family["family_size"] = family_size
        out["by_arm"][arm] = per_family

    for arm in ARMS:
        out["by_arm"][arm]["units"] = (
            "efficiency units" if arm == ARM_SIZING
            else "pp of net return, as a paired difference")
    return out


def _kept_effective_for(configs: Sequence[dict], arm: str,
                        family: str) -> Optional[float]:
    own = [c for c in configs
           if c["arm"] == arm and c["family"] == family
           and c["cohort"] == COHORT_PRIMARY
           and int(c["hurst_window"]) == EXIT_HURST_WINDOW]
    if not own:
        return None
    return own[0].get("n_kept_effective")


def validity_gate(arm: str, mde: dict,
                  configs: Sequence[dict] = ()) -> dict:
    family = PRIMARY_FAMILY
    per = (mde.get("by_arm") or {}).get(arm) or {}
    limit = (per.get("cluster") or {}).get(family)
    sep = (per.get("separation") or {}).get(family)
    kept_eff = _kept_effective_for(configs, arm, family)
    base = {
        "arm": arm,
        "family": family,
        "n_rows": (per.get("n") or {}).get(family),
        "units": per.get("units"),
        "contrast": (SIZING_CONTRAST_STATEMENT if arm == ARM_SIZING
                     else EXIT_CONTRAST_STATEMENT),
        "kept_effective": kept_eff,
        "kept_floor": MIN_KEPT_EFFECTIVE,
    }
    if kept_eff is not None and float(kept_eff) < MIN_KEPT_EFFECTIVE:
        return dict(base, passed=False, limit=limit,
                    largest_separation=(None if sep is None
                                        else round(float(sep), 6)),
                    mode=MODE_FLOOR_BREACH,
                    reason=(
                        f"the confirmatory family (`{family}`) carries only "
                        f"{float(kept_eff):.1f} effective KEPT rows against a "
                        f"floor of {MIN_KEPT_EFFECTIVE:g}"
                        + (f", which is what the shipped form's 1.0 cap does "
                           f"to this contrast: its kept side is the tail "
                           f"`|H - 0.5| >= {SIZE_CEILING_BAND:g}` rather than "
                           f"an interval" if arm == ARM_SIZING else "")))
    if sep is None:
        return dict(base, passed=False, limit=limit, largest_separation=None,
                    mode=MODE_NO_SEPARATION,
                    reason=(f"the confirmatory family (`{family}`) carries no "
                            f"measurable separation on this arm's contrast"))
    sep = round(float(sep), 6)
    if limit is None:
        grid_max = (MDE_EFF_GRID_MAX if arm == ARM_SIZING else MDE_PP_GRID_MAX)
        return dict(base, passed=False, limit=None, largest_separation=sep,
                    mode=MODE_UNRESOLVABLE,
                    reason=(f"the confirmatory family (`{family}`) has a "
                            f"detection limit above {grid_max:g} "
                            f"{per.get('units')}, so no effect on the "
                            f"injection grid is resolvable"))
    limit = round(float(limit), 6)
    passed = bool(abs(sep) >= limit)
    return dict(base, passed=passed, limit=limit, largest_separation=sep,
                degenerate=bool(limit == 0.0), reason="",
                mode=MODE_OK if passed else MODE_BELOW_LIMIT)


def config_verdict(cfg: dict) -> tuple:
    reasons = []
    if float(cfg.get("n_suppressed_effective") or 0.0) < MIN_SUPPRESSED_EFFECTIVE:
        reasons.append(
            f"only {_fmt(cfg.get('n_suppressed_effective'), 1)} effective "
            f"suppressed rows (floor {MIN_SUPPRESSED_EFFECTIVE:g})")
    if float(cfg.get("n_kept_effective") or 0.0) < MIN_KEPT_EFFECTIVE:
        reasons.append(
            f"only {_fmt(cfg.get('n_kept_effective'), 1)} effective kept rows "
            f"(floor {MIN_KEPT_EFFECTIVE:g})")
    if cfg.get("p_cluster") is None:
        reasons.append("cluster permutation p on the primary target is "
                       f"untestable ({cfg.get('cluster_reason') or 'no draws'})")
    elif not cfg.get("bh_reject"):
        reasons.append(
            f"not significant after Benjamini-Hochberg on the primary "
            f"target's cluster p (cluster p={cfg.get('p_cluster')})")
    windows = cfg.get("windows") or {}
    ok_proto, _holding, _with_legs, proto_reasons = protocol_verdict(
        windows, cfg.get("protocol_windows") or (),
        int(cfg.get("protocol_min_windows") or 0))
    if not ok_proto:
        reasons.extend(proto_reasons)
    ok, non_deg, n_with = held_out_verdict(
        windows, cfg.get("held_out_windows") or ())
    if not ok:
        reasons.append(
            f"drawdown holds on only {non_deg}/{n_with} held-out windows with "
            f"legs (need {HELD_OUT_MIN_FRACTION:.2f} of them, min "
            f"{HELD_OUT_MIN_WINDOWS} windows)")
    return (not reasons), reasons


def decide_recommendation(configs: Sequence[dict], mde: dict) -> dict:
    arms = {}
    for arm in ARMS:
        gate = validity_gate(arm, mde, configs)
        primary = [c for c in configs
                   if c.get("cohort") == COHORT_PRIMARY and c.get("arm") == arm]
        sorts = [c for c in primary if c.get("bh_reject")]
        n_untestable = sum(1 for c in primary if c.get("p_cluster") is None)
        best = None
        for cfg in primary:
            p = cfg.get("p_cluster")
            if p is not None and (best is None or p < best[0]):
                best = (p, cfg["config_id"], cfg.get("separation"))
        if sorts:
            verdict = VERDICT_SORTS
        elif gate["passed"]:
            verdict = VERDICT_RESOLVED_NULL
        else:
            verdict = VERDICT_INCONCLUSIVE
        arms[arm] = {
            "verdict": verdict,
            "validity_gate": gate,
            "n_tested": len(primary),
            "n_significant": len(sorts),
            "n_untestable": n_untestable,
            "significant_config_ids": sorted(c["config_id"] for c in sorts),
            "best": (None if best is None else
                     {"p_cluster": best[0], "config_id": best[1],
                      "separation": best[2]}),
            "verdict_is_confirmatory": bool(
                verdict == VERDICT_SORTS and gate["passed"]
                and not gate.get("degenerate")),
        }

    confirmatory = [a for a in ARMS if arms[a]["verdict_is_confirmatory"]]
    if any(arms[a]["verdict"] == VERDICT_SORTS for a in ARMS):
        headline = VERDICT_SORTS
    elif all(arms[a]["validity_gate"]["passed"] for a in ARMS):
        headline = VERDICT_RESOLVED_NULL
    else:
        headline = VERDICT_INCONCLUSIVE

    return {
        "verdict": headline,
        "arms": arms,
        "confirmatory_arms": confirmatory,
        "claims_contract_path": bool(confirmatory),
        "no_promotion": NO_PROMOTION_SENTENCE,
        "key_risk_held": bool(
            all(arms[a]["verdict"] != VERDICT_SORTS for a in ARMS)),
    }


def claims_contract_path(decision: dict) -> bool:
    return bool((decision or {}).get("claims_contract_path"))


def decision_payload(decision: dict) -> dict:
    return {
        "verdict": decision["verdict"],
        "confirmatory_arms": list(decision.get("confirmatory_arms") or ()),
        "claims_contract_path": claims_contract_path(decision),
        "arms": {
            arm: {
                "verdict": row["verdict"],
                "n_significant": row["n_significant"],
                "n_tested": row["n_tested"],
                "significant_config_ids": list(row["significant_config_ids"]),
                "validity_gate": {
                    "passed": row["validity_gate"]["passed"],
                    "mode": row["validity_gate"]["mode"],
                    "limit": row["validity_gate"]["limit"],
                    "largest_separation":
                        row["validity_gate"]["largest_separation"],
                    "kept_effective": row["validity_gate"]["kept_effective"],
                },
            }
            for arm, row in sorted((decision.get("arms") or {}).items())
        },
    }


def _render_config_table(cfgs: Sequence[dict]) -> list:
    head = ("| Config | Arm | Rows (eff.) | kept/suppressed eff. | cluster p "
            "| BH | Separation | Verdict |")
    out = [head, "|" + "---|" * 8]
    for cfg in sorted(cfgs, key=lambda c: c["config_id"]):
        ok, reasons = config_verdict(cfg)
        out.append(
            f"| `{cfg['config_id']}` | {cfg['arm']} | "
            f"{cfg['n_pooled_trades']} ({_fmt(cfg.get('n_pooled_effective'), 1)}) | "
            f"{_fmt(cfg.get('n_kept_effective'), 1)}/"
            f"{_fmt(cfg.get('n_suppressed_effective'), 1)} | "
            f"{_fmt_p(cfg.get('p_cluster'))} | "
            f"{'reject' if cfg.get('bh_reject') else 'no'} | "
            f"{_fmt_signed(cfg.get('separation'), 6)} | "
            f"{'PASSES' if ok else '; '.join(reasons)} |")
    return out


def _render_gate(gate: dict) -> str:
    if gate["mode"] == MODE_OK:
        degenerate = (" The limit is 0.000, which is DEGENERATE: the "
                      "injection search returns it when the un-injected "
                      "contrast already clears the bar, so the effect SIZE "
                      "stays unestimated." if gate.get("degenerate") else "")
        return (f"PASSED on `{gate['family']}`: the detection limit is "
                f"{gate['limit']:.6f} {gate['units']} and those SAME rows "
                f"separate by {gate['largest_separation']:+.6f}, so an effect "
                f"of this size IS resolvable here.{degenerate}")
    if gate["reason"]:
        return f"FAILED on `{gate['family']}`: {gate['reason']}."
    return (f"FAILED on `{gate['family']}`: the detection limit is "
            f"{gate['limit']:.6f} {gate['units']} while those SAME rows "
            f"separate by only {gate['largest_separation']:+.6f}, BELOW the "
            f"limit. Nothing at or below that limit is VISIBLE to this "
            f"design, so this null bounds the effect from above and says "
            f"nothing either way about a smaller one.")


def render_report(payload: dict) -> str:
    pre = payload["pre_registered"]
    run = payload["run_summary"]
    cfgs = payload["configs"]
    mde = payload["detection_limits"]
    decision = payload["decision"]

    out = [f"# Hurst as a SIZING and EXIT input (#{payload['issue']})", ""]
    out.append(STUDY_PREMISE)
    out.append("")
    out.append(f"Report-only. {NO_PROMOTION_SENTENCE}")
    out.append("")

    out.append("## Verdict")
    out.append("")
    out.append(f"Headline: **{decision['verdict']}**.")
    out.append("")
    for arm in ARMS:
        row = decision["arms"][arm]
        out.append(f"### {arm} arm")
        out.append("")
        out.append(f"- Verdict: **{row['verdict']}** over "
                   f"{row['n_tested']} primary hypothes"
                   f"{'is' if row['n_tested'] == 1 else 'es'}; "
                   f"{row['n_significant']} reached Benjamini-Hochberg "
                   f"significance at alpha={pre['alpha']}, "
                   f"{row['n_untestable']} were untestable.")
        if row["best"]:
            out.append(f"- Best cluster p: {row['best']['p_cluster']:.6f} "
                       f"(`{row['best']['config_id']}`), separation "
                       f"{_fmt_signed(row['best']['separation'], 6)}.")
        out.append(f"- Validity gate: {_render_gate(row['validity_gate'])}")
        out.append(f"- Kept-side effective N: "
                   f"{_fmt(row['validity_gate'].get('kept_effective'), 1)} "
                   f"against a floor of {MIN_KEPT_EFFECTIVE:g}.")
        out.append("")

    out.append("## Contract path")
    out.append("")
    out.append(CONTRACT_PATH_CLAIM_RULE)
    out.append("")
    if claims_contract_path(decision):
        out.append(
            f"This run CLAIMS `{CONTRACT_REPORT_BASENAME}`: the "
            f"{', '.join(decision['confirmatory_arms'])} arm(s) returned a "
            f"confirmatory result whose validity gate passed on a "
            f"non-degenerate limit. #1426 and #1427 both defer.")
    else:
        out.append(
            f"This run DEFERS `{CONTRACT_REPORT_BASENAME}` to "
            f"`hurst_1424_gate_resolution.py`: no arm returned a "
            f"confirmatory result whose validity gate passed on a "
            f"non-degenerate limit, and the maintainer precedent is that "
            f"only a confirmatory result may move the live evidence. #1426 "
            f"and #1427 defer for their own reasons.")
    out.append("")

    out.append("## Pre-registered design")
    out.append("")
    for text in (SHIPPED_FORM_STATEMENT, SIZING_CONTRAST_STATEMENT,
                 EXIT_FORM_STATEMENT, EXIT_MECHANISM_STATEMENT,
                 EXIT_CONTRAST_STATEMENT, PAIRED_EXIT_NS_RULE,
                 EXIT_WINDOW_STATEMENT,
                 ARM_TARGET_STATEMENT, INFERENCE_DIRECTION_RATIONALE,
                 TWO_SIDED_P_DEFINITION, PRIOR_EXPOSURE_DISCLOSURE,
                 KEY_RISK_PREDICTION, DEGENERATE_LIMIT_DISCLOSURE):
        out.append(text)
        out.append("")

    out.append("### Exit trail ladder")
    out.append("")
    out.append("| Bucket | Scale | Trail (x ATR) | In contrast |")
    out.append("|---|---|---|---|")
    for bucket in EXIT_BUCKETS:
        out.append(f"| `{bucket}` | {EXIT_BUCKET_SCALES[bucket]:g} | "
                   f"{exit_trail_mult(bucket):g} | "
                   f"{'scaled' if bucket in EXIT_SCALED_BUCKETS else 'control'} |")
    out.append("")

    out.append("## Detection limits")
    out.append("")
    out.append("| Arm | Family | Rows | Limit | Separation | Units |")
    out.append("|---|---|---|---|---|---|")
    for arm in ARMS:
        per = mde["by_arm"][arm]
        for family in FAMILIES:
            out.append(
                f"| {arm} | `{family}` | {per['n'].get(family)} | "
                f"{_fmt(per['cluster'].get(family), 6)} | "
                f"{_fmt_signed(per['separation'].get(family), 6)} | "
                f"{per['units']} |")
    out.append("")

    for cohort in (COHORT_PRIMARY, COHORT_EXPLORATORY):
        own = [c for c in cfgs if c["cohort"] == cohort]
        if not own:
            continue
        out.append(f"## Configurations - {cohort} cohort")
        out.append("")
        out.extend(_render_config_table(own))
        out.append("")

    out.append("## Run")
    out.append("")
    out.append(f"- Legs: {run['legs']}; sizing rows: {run['sizing_rows']}; "
               f"exit paired rows: {run['exit_rows']}; unpaired exit entries "
               f"dropped: {run['exit_unpaired']}.")
    out.append(f"- Datasets: {len(pre['datasets'])}; windows: "
               f"{len(pre['windows'])}; permutations: {pre['n_perm']} "
               f"(limits {pre['n_perm_mde']}); seed {pre['seed']}.")
    out.append(f"- Scope complete: {run['scope']['complete']}; "
               f"pre-registered inference: "
               f"{run['scope']['pre_registered_inference']}.")
    out.append(f"- Warm-up: "
               f"{'sufficient on every dataset' if run['warmup'].get('sufficient') else 'SHORT on ' + ', '.join(run['warmup'].get('insufficient_datasets') or [])}.")
    out.append(f"- Wall time: {run['elapsed_sec']} s.")
    out.append("")
    return "\n".join(out).rstrip() + "\n"


def report_from_payload(payload: dict) -> str:
    return render_report(payload)


def ensure_min_history(datasets: Sequence[tuple]) -> dict:
    return study1424.ensure_min_history(datasets)


def inference_deviations(args) -> list:
    dev = []
    if int(args.n_perm) != N_PERM:
        dev.append(f"--n-perm {args.n_perm}")
    if int(args.n_perm_mde) != N_PERM_MDE:
        dev.append(f"--n-perm-mde {args.n_perm_mde}")
    if int(args.seed) != SEED:
        dev.append(f"--seed {args.seed}")
    if args.no_exit_arm:
        dev.append("--no-exit-arm")
    return dev


def main(argv: Optional[Sequence[str]] = None) -> int:
    p = argparse.ArgumentParser()
    p.add_argument("--jobs", type=int, default=4, help="worker threads")
    p.add_argument("--out-dir", default=None,
                   help="optional dir for the rolling-Hurst npz cache")
    p.add_argument("--only", default=None,
                   help=f"comma-separated families ({', '.join(FAMILIES)})")
    p.add_argument("--windows", default=None, help="comma-separated windows")
    p.add_argument("--datasets", default=None,
                   help="comma-separated [EXCHANGE=]SYMBOL:TIMEFRAME")
    p.add_argument("--hurst-windows", default=None,
                   help="comma-separated rolling Hurst window lengths")
    p.add_argument("--n-perm", type=int, default=N_PERM)
    p.add_argument("--n-perm-mde", type=int, default=N_PERM_MDE)
    p.add_argument("--seed", type=int, default=SEED)
    p.add_argument("--json-out", default=_DEFAULT_JSON_OUT)
    p.add_argument("--report-out", default=_DEFAULT_REPORT_OUT)
    p.add_argument("--no-exit-arm", action="store_true",
                   help="score the sizing arm only; deviates from the "
                        "pre-registered design")
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
            f"[1428] this study never writes the live-evidence contract path "
            f"{CONTRACT_REPORT_BASENAME} from the command line. Whether it "
            f"CLAIMS that path is decided mechanically by this run's verdict "
            f"and recorded in `decision.claims_contract_path`; the handover "
            f"itself is a reviewed change to "
            f"`hurst_1424_gate_resolution.py` and its tests, never a "
            f"`--report-out` flag. Its own render belongs at "
            f"{_DEFAULT_REPORT_OUT}.")

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
                f"[1428] refusing to overwrite the committed aggregate "
                f"{_DEFAULT_JSON_OUT} from {kind} ({narrowed}). Pass an "
                f"explicit --json-out.")
        if os.path.abspath(args.report_out) == os.path.abspath(_DEFAULT_REPORT_OUT):
            raise SystemExit(
                f"[1428] refusing to overwrite the committed report "
                f"{_DEFAULT_REPORT_OUT} from {kind} ({narrowed}). Pass an "
                f"explicit --report-out.")

    if args.render_only:
        with open(args.json_out) as fh:
            payload = json.load(fh)
        report = report_from_payload(payload)
        with open(args.report_out, "w") as fh:
            fh.write(report)
        print(f"[1428] re-rendered {args.report_out} from {args.json_out}")
        return 0

    datasets = study1424._parse_datasets(args.datasets)
    if args.fetch_only:
        ensure_min_history(datasets)
        print("[1428] backfill complete")
        return 0

    families = FAMILIES
    if args.only:
        wanted = [t.strip() for t in args.only.split(",") if t.strip()]
        for f in wanted:
            if f not in FAMILIES:
                raise SystemExit(f"unknown family {f!r}; known: {list(FAMILIES)}")
        families = tuple(f for f in FAMILIES if f in wanted)
    window_names = study1424._parse_windows(args.windows)
    hurst_windows = (tuple(int(t) for t in args.hurst_windows.split(","))
                     if args.hurst_windows else HURST_WINDOWS)

    resolved = resolve_primary_config_id(_JSON_1410)
    if resolved != PRIMARY_CONFIG_ID:
        raise SystemExit(
            f"#1424's pre-registered primary hypothesis {PRIMARY_CONFIG_ID!r} "
            f"no longer matches the committed #1410 argmin {resolved!r}. This "
            f"study inherits that pin for its exit-arm window; re-register "
            f"deliberately, never let it drift.")

    started = time.time()
    backfill = {}
    if not args.skip_fetch:
        print(f"[1428] backfilling {len(datasets)} datasets...")
        backfill = ensure_min_history(datasets)

    from data_fetcher import load_cached_data
    from registry_loader import load_registry
    reg = load_registry("spot")

    print(f"[1428] loading {len(datasets)} datasets from the venue caches...")
    frames = {}
    for dataset in datasets:
        exchange_id, symbol, timeframe = dataset
        try:
            frames[dataset] = load_cached_data(symbol, timeframe,
                                               exchange_id=exchange_id)
        except Exception as exc:
            print(f"[1428] load FAILED for {exchange_id} "
                  f"{dataset_key(symbol, timeframe)}: {exc}")
            frames[dataset] = pd.DataFrame()

    coverage = coverage_audit(frames, window_names, hurst_windows)
    print(f"[1428] coverage: {coverage['n_kept']}/{coverage['n_cells']} owned "
          f"cells kept, {coverage['n_dropped']} dropped, "
          f"{coverage['n_unowned']} not owned")
    for d in coverage["dropped"]:
        print(f"[1428]   dropped {d['dataset']} {d['window']}: {d['reason']}")

    def _cell_ok(dataset, window):
        exchange_id, symbol, timeframe = dataset
        key = dataset_key(qualified_symbol(exchange_id, symbol), timeframe)
        return bool(coverage["cells"].get(f"{key}|{window}"))

    usable_datasets = [ds for ds in datasets
                       if any(_cell_ok(ds, w) for w in window_names)]
    if not usable_datasets:
        raise SystemExit("[1428] no dataset carries a scoreable cell")

    scored_windows = [w for w in window_names
                      if any(_cell_ok(ds, w) for ds in usable_datasets)]
    first_needed_by_ds = {}
    for ds in usable_datasets:
        own = [w for w in scored_windows if _cell_ok(ds, w)]
        first_needed_by_ds[ds] = min(pd.Timestamp(WINDOWS[w][0]) for w in own)

    warmup = warmup_audit(
        scored_warmup_leads(frames, coverage, scored_windows), hurst_windows)
    if not warmup["sufficient"]:
        print(f"[1428] WARNING: warm-up shortfall on "
              f"{len(warmup['insufficient_datasets'])} dataset(s). H is "
              f"UNDEFINED on their first scored bars; NaN stays its own "
              f"bucket, sizes at exactly 1.0 and takes the base trail.")

    print(f"[1428] computing rolling Hurst for {len(usable_datasets)}x"
          f"{len(hurst_windows)} (dataset, window) pairs...")
    hurst: dict = {}
    cache_path = None
    if args.out_dir:
        os.makedirs(args.out_dir, exist_ok=True)
        cache_path = os.path.join(args.out_dir, "hurst_1428_rolling.npz")
    cached = {}
    if cache_path and os.path.exists(cache_path):
        with np.load(cache_path, allow_pickle=False) as z:
            cached = {k: z[k] for k in z.files}

    def _hurst_key(dataset, hw):
        exchange_id, symbol, timeframe = dataset
        return f"{exchange_id}|{symbol}|{timeframe}|{hw}"

    def _hurst_job(job):
        dataset, hw = job
        key = _hurst_key(dataset, hw)
        frame = frames[dataset]
        first_needed = first_needed_by_ds[dataset]
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
            key = _hurst_key(ds, hw)
            arrays[key] = hurst[(ds, hw)].to_numpy(dtype=float)
            arrays[f"meta|{key}"] = cache_meta(frames[ds].index,
                                               first_needed_by_ds[ds])
        np.savez_compressed(cache_path, **arrays)

    print(f"[1428] computing entry-ADX stamps for {len(usable_datasets)} "
          f"datasets...")
    adx_stamps = {ds: adx_entry_stamp(frames[ds]) for ds in usable_datasets}

    print("[1428] computing symbol daily-return correlations...")
    rho_by_symbol = symbol_return_correlations(
        {ds: frames[ds] for ds in usable_datasets})

    units = [(family, exemplar, ds, wname)
             for family in families
             for exemplar in FAMILY_EXEMPLARS[family]
             for ds in usable_datasets
             for wname in scored_windows
             if _cell_ok(ds, wname)]
    run_exit = (not args.no_exit_arm) and EXIT_HURST_WINDOW in hurst_windows
    print(f"[1428] scoring {len(units)} legs "
          f"({'sizing + exit' if run_exit else 'sizing only'})...")

    def _leg_job(unit):
        family, exemplar, ds, wname = unit
        by_window = {hw: hurst[(ds, hw)] for hw in hurst_windows}
        return build_leg(reg, family, exemplar, ds, wname, frames[ds],
                         by_window, adx_stamps[ds], run_exit=run_exit)

    with ThreadPoolExecutor(max_workers=max(1, args.jobs)) as pool:
        legs = [lg for lg in pool.map(_leg_job, units) if lg is not None]
    legs.sort(key=lambda lg: (lg["family"], lg["strategy"], lg["dataset"],
                              lg["window"]))

    pooled = {}
    pooled_exit = {}
    for family in FAMILIES:
        rows = [t for lg in legs if lg["family"] == family
                for t in lg["trades"]]
        pooled[family] = dedup_entries(rows, WINDOW_ORDER)
        erows = [t for lg in legs if lg["family"] == family
                 for t in lg["exit_rows"]]
        pooled_exit[family] = dedup_entries(erows, WINDOW_ORDER)

    for family in FAMILIES:
        for t in pooled[family]:
            if t["cohort"] != COHORT_PRIMARY:
                continue
            key = (dataset_key(t["symbol"], t["timeframe"]), t["window"])
            if key in D_1410:
                raise AssertionError(f"primary cohort leaked a #1410 cell: {key}")

    print("[1428] sweeping configs and running the cluster nulls...")
    configs = build_configs(legs, pooled, pooled_exit, hurst_windows,
                            rho_by_symbol, args.n_perm, args.seed)
    configs = [c for c in configs if c["family"] in families]
    apply_bh_by_cohort(configs, alpha=ALPHA)

    print("[1428] measuring detection limits...")
    mde = measure_detection_limits(pooled, pooled_exit, args.n_perm_mde,
                                   args.seed)

    decision = decide_recommendation(configs, mde)

    payload = {
        "schema_version": SCHEMA_VERSION,
        "issue": ISSUE,
        "pre_registered": {
            "study_premise": STUDY_PREMISE,
            "arms": list(ARMS),
            "arm_targets": {a: dict(ARM_TARGETS[a]) for a in ARMS},
            "arm_target_statement": ARM_TARGET_STATEMENT,
            "shipped_form_statement": SHIPPED_FORM_STATEMENT,
            "shipped_size_span": SHIPPED_SIZE_SPAN,
            "shipped_size_floor": SHIPPED_SIZE_FLOOR,
            "shipped_size_ceiling": SHIPPED_SIZE_CEILING,
            "shipped_nan_multiplier": SHIPPED_NAN_MULTIPLIER,
            "sizing_contrast_statement": SIZING_CONTRAST_STATEMENT,
            "size_ceiling_band": SIZE_CEILING_BAND,
            "exit_form": EXIT_FORM,
            "exit_form_statement": EXIT_FORM_STATEMENT,
            "exit_mechanism": EXIT_MECHANISM,
            "exit_mechanism_statement": EXIT_MECHANISM_STATEMENT,
            "exit_contrast_statement": EXIT_CONTRAST_STATEMENT,
            "paired_exit_ns_rule": PAIRED_EXIT_NS_RULE,
            "exit_window_statement": EXIT_WINDOW_STATEMENT,
            "exit_base_trail_atr_mult": EXIT_BASE_TRAIL_ATR_MULT,
            "exit_trail_gain": EXIT_TRAIL_GAIN,
            "exit_bucket_scales": dict(EXIT_BUCKET_SCALES),
            "exit_trail_by_bucket": {b: exit_trail_mult(b)
                                     for b in EXIT_BUCKETS},
            "exit_scaled_buckets": list(EXIT_SCALED_BUCKETS),
            "exit_persistent_edge": EXIT_PERSISTENT_EDGE,
            "exit_antipersistent_edge": EXIT_ANTIPERSISTENT_EDGE,
            "exit_hurst_window": EXIT_HURST_WINDOW,
            "inference_direction": INFERENCE_DIRECTION,
            "inference_direction_rationale": INFERENCE_DIRECTION_RATIONALE,
            "two_sided_p_definition": TWO_SIDED_P_DEFINITION,
            "prior_exposure_disclosure": PRIOR_EXPOSURE_DISCLOSURE,
            "contract_path_claim_rule": CONTRACT_PATH_CLAIM_RULE,
            "contract_report_basename": CONTRACT_REPORT_BASENAME,
            "no_promotion_sentence": NO_PROMOTION_SENTENCE,
            "key_risk_prediction": KEY_RISK_PREDICTION,
            "degenerate_limit_disclosure": DEGENERATE_LIMIT_DISCLOSURE,
            "inherits_primary_config_id": PRIMARY_CONFIG_ID,
            "primary_family": PRIMARY_FAMILY,
            "families": {f: list(FAMILY_EXEMPLARS[f]) for f in FAMILIES},
            "family_sense": dict(FAMILY_SENSE),
            "exemplar_close_overrides": EXEMPLAR_CLOSE_OVERRIDES,
            "hurst_windows": list(hurst_windows),
            "family_size_by_arm": dict(FAMILY_SIZE_BY_ARM),
            "horizon_hours": HORIZON_HOURS,
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
            "windows": {k: list(WINDOWS[k]) for k in scored_windows},
            "window_owner": dict(WINDOW_OWNER),
            "primary_protocol_windows": list(PRIMARY_PROTOCOL_WINDOWS),
            "primary_protocol_min_windows": PRIMARY_PROTOCOL_MIN_WINDOWS,
            "primary_held_out_windows": list(PRIMARY_HELD_OUT_WINDOWS),
            "exploratory_protocol_windows": list(EXPLORATORY_PROTOCOL_WINDOWS),
            "exploratory_held_out_windows": list(EXPLORATORY_HELD_OUT_WINDOWS),
            "datasets": [dataset_key(qualified_symbol(ex, sym), tf)
                         for (ex, sym, tf) in usable_datasets],
            "history_since": dict(HISTORY_SINCE),
            "fetch_page_limit": dict(FETCH_PAGE_LIMIT),
            "fee_platform": FEE_PLATFORM,
            "capital": DEFAULT_CAPITAL,
        },
        "run_summary": {
            "scope": scope,
            "legs": len(legs),
            "sizing_rows": sum(len(pooled[f]) for f in FAMILIES),
            "exit_rows": sum(len(pooled_exit[f]) for f in FAMILIES),
            "exit_unpaired": sum(lg["exit_meta"]["n_unpaired"] for lg in legs),
            "exit_arm_ran": run_exit,
            "coverage": coverage,
            "warmup": warmup,
            "backfill": backfill,
            "elapsed_sec": round(time.time() - started, 1),
        },
        "configs": configs,
        "detection_limits": mde,
        "decision": decision,
        "decision_payload": decision_payload(decision),
    }

    with open(args.json_out, "w") as fh:
        json.dump(payload, fh, indent=2, sort_keys=True, default=str)
    with open(args.report_out, "w") as fh:
        fh.write(render_report(payload))
    print(f"[1428] wrote {args.json_out} and {args.report_out}")
    print(f"[1428] verdict: {decision['verdict']}; claims contract path: "
          f"{claims_contract_path(decision)}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
