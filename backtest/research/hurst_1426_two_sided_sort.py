import argparse
import json
import math
import os
import sys
import time
from concurrent.futures import ThreadPoolExecutor
from typing import Optional, Sequence
_THIS_DIR = os.path.dirname(os.path.abspath(__file__))
_BACKTEST = os.path.abspath(os.path.join(_THIS_DIR, '..'))
_ROOT = os.path.abspath(os.path.join(_BACKTEST, '..'))
for _p in (_THIS_DIR, _BACKTEST, _ROOT, os.path.join(_ROOT, 'shared_tools')):
    if _p not in sys.path:
        sys.path.insert(0, _p)
import numpy as np
import pandas as pd
from eval_windows import DEFAULT_CAPITAL, FEE_PLATFORM, PLATFORM, dataset_key
from regime_stats import benjamini_hochberg
import hurst_1410_gate_calibration as study1410
import hurst_1422_gate_power as study1422
import hurst_1424_gate_resolution as study1424
bucket_label = study1410.bucket_label
cache_entry_is_usable = study1410.cache_entry_is_usable
cache_meta = study1410.cache_meta
chop_loss = study1410.chop_loss
compound_equity = study1410.compound_equity
decision_series = study1410.decision_series
entry_stamp_series = study1410.entry_stamp_series
hysteresis_mask = study1410.hysteresis_mask
required_lead_bars = study1410.required_lead_bars
rolling_hurst = study1410.rolling_hurst
size_multiplier = study1410.size_multiplier
slice_window = study1410.slice_window
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
SIZING_CLAMP_HI = study1410.SIZING_CLAMP_HI
SIZING_CLAMP_LO = study1410.SIZING_CLAMP_LO
SIZING_GAINS = study1410.SIZING_GAINS
SIZING_NAN_MULTIPLIER = study1410.SIZING_NAN_MULTIPLIER
CONFIG_ID_SEP = study1410.CONFIG_ID_SEP
gate_config_id = study1410.gate_config_id
size_config_id = study1410.size_config_id
adx_entry_stamp = study1422.adx_entry_stamp
anti_signal_side = study1422.anti_signal_side
cluster_rotation_offsets = study1422.cluster_rotation_offsets
dedup_entries = study1422.dedup_entries
effective_n = study1422.effective_n
joint_adx_bucket = study1422.joint_adx_bucket
joint_h_bucket = study1422.joint_h_bucket
rotation_shift_counts = study1422.rotation_shift_counts
timeframe_minutes = study1422.timeframe_minutes
usable_cluster_rows = study1422.usable_cluster_rows
_admissible_offsets = study1422._admissible_offsets
_rank1_threshold = study1422._rank1_threshold
_rotate_values = study1422._rotate_values
_separation = study1422._separation
ADX_PERIOD = study1422.ADX_PERIOD
ADX_SPLIT = study1422.ADX_SPLIT
COHORT_EXPLORATORY = study1422.COHORT_EXPLORATORY
COHORT_PRIMARY = study1422.COHORT_PRIMARY
D_1410 = study1422.D_1410
JOINT_ADX_BUCKETS = study1422.JOINT_ADX_BUCKETS
JOINT_H_BUCKETS = study1422.JOINT_H_BUCKETS
MIN_OFFSET_DAYS = study1422.MIN_OFFSET_DAYS
MIN_WINDOW_BAR_FRACTION = study1422.MIN_WINDOW_BAR_FRACTION
NO_JOINT_SEPARATION = study1422.NO_JOINT_SEPARATION
base_asset = study1424.base_asset
bucket_tables = study1424.bucket_tables
build_leg = study1424.build_leg
cell_cohort = study1424.cell_cohort
config_verdict = study1424.config_verdict
coverage_audit = study1424.coverage_audit
ensure_min_history = study1424.ensure_min_history
history_since_for = study1424.history_since_for
horizon_bars = study1424.horizon_bars
held_out_verdict = study1424.held_out_verdict
joint_adx_hurst_table = study1424.joint_adx_hurst_table
owned_windows = study1424.owned_windows
protocol_dd_reduction = study1424.protocol_dd_reduction
protocol_verdict = study1424.protocol_verdict
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
_render_bucket_table = study1424._render_bucket_table
_render_joint_table = study1424._render_joint_table
_sweep_grid = study1424._sweep_grid
_target_rows = study1424._target_rows
_window_rows_gate = study1424._window_rows_gate
_window_rows_size = study1424._window_rows_size
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
JOINT_ALPHA = study1424.JOINT_ALPHA
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
PRIMARY_CONFIG_ID = study1424.PRIMARY_CONFIG_ID
PRIMARY_CONFIG_IDS = study1424.PRIMARY_CONFIG_IDS
PRIMARY_FAMILY = study1424.PRIMARY_FAMILY
PRIMARY_FAMILY_SIZE = study1424.PRIMARY_FAMILY_SIZE
PRIMARY_HELD_OUT_WINDOWS = study1424.PRIMARY_HELD_OUT_WINDOWS
PRIMARY_PROTOCOL_MIN_WINDOWS = study1424.PRIMARY_PROTOCOL_MIN_WINDOWS
PRIMARY_PROTOCOL_WINDOWS = study1424.PRIMARY_PROTOCOL_WINDOWS
PRIMARY_TARGET = study1424.PRIMARY_TARGET
RETURN_TOLERANCE_FRAC = study1424.RETURN_TOLERANCE_FRAC
RETURN_TOLERANCE_PP = study1424.RETURN_TOLERANCE_PP
WINDOWS = study1424.WINDOWS
WINDOW_ORDER = study1424.WINDOW_ORDER
WINDOW_OWNER = study1424.WINDOW_OWNER
_JSON_1410 = study1424._JSON_1410
_JSON_1424 = study1424._DEFAULT_JSON_OUT
SCHEMA_VERSION = 1
ISSUE = 1426
SEED = ISSUE
COHORT_OPTION = 'exploratory_only_full_pool'
CONTRACT_PATH_CLAIMED = False
SIBLING_DEFERRAL = (1427, 1428)
TWO_SIDED = True
TWO_SIDED_P_DEFINITION = 'p2 = min(1, 2 * min(p_ge, p_le)), each tail carrying the add-one convention over the SAME draws; the smallest reachable p is 2/(draws+1)'
CONTRACT_REPORT_BASENAME = 'hurst_gate_calibration.md'
INTERIM_LOOK_DISCLOSURE = "INTERIM LOOK, disclosed before the run, and it is the worst kind. The SIGN this study was built to test was already visible: #1424's committed run reports a confirmatory separation of -0.005 efficiency units and -0.12 pp of net return on the `momentum` family, measured on the very rows this study re-scores. That look is not merely prior exposure — it is what MOTIVATED the hypothesis that Hurst might sort these trades the other way. No pre-registration can be claimed over rows chosen because of what they already showed. The study therefore takes OPTION 2 from the issue: it is EXPLORATORY-ONLY over the full pool, it states that cost here, and its verdict machinery is structurally unable to recommend anything. A reader should treat even a SIGNIFICANT reversal from this run as hypothesis-generating for a future pre-registered test on tape that does not yet exist, and never as a licence to act."
COHORT_DECISION_STATEMENT = "COHORT DECISION: Option 2 — exploratory-only, full pool. Option 1 (reserve rows #1424 never scored and make those the confirmatory cohort) is not available here. #1424's windows already run open-ended through the tape at run time, so almost no unscored calendar remains; its committed feasibility probes record every other reachable venue as INFEASIBLE (binance-global HTTP 451, Kraken ignores a deep `since`, TopStep exposes no `since` at all, IBKR exposes no OHLCV), so an unscored (venue, window) cell for a scored base asset is the same market's tape — the double count the window-ownership matrix exists to forbid. And effective N is set by independent calendar clusters rather than rows, so any strict subset carries a WORSE detection limit than the 0.013 measured on the full 7,992-row family. Option 1 would buy a pre-registration guaranteed to be inconclusive. THE COST OF OPTION 2 IS REAL AND IT IS ENFORCED: no confirmatory claim is available from this run, no configuration can win, and no threshold ships."
CONTRACT_PATH_STATEMENT = "CONTRACT PATH: this study DEFERS. `hurst_gate_calibration.md` is the live-evidence path cited by `scheduler/hurst_gate.go`, `docs/ARCHITECTURE.md` and #1412's Stage 0, and an exploratory-only study cannot be the evidence behind a shipping gate. `hurst_1424_gate_resolution.py` keeps it, its script and tests are untouched by this work, and this study's `main` refuses that path unconditionally. The supersede clause is shared with #1427 and #1428, and at most one of the three may claim it; #1426 is not that one."
KEY_RISK_PREDICTION = "The two-sided re-test is expected to return INCONCLUSIVE, and the reason matters more than the word. #1424 measured |-0.005| against a row-matched limit of 0.013, and a two-sided limit can only be at or above its one-sided counterpart at the same alpha, so the separation is expected to stay BELOW the limit and the validity gate is expected to FAIL. A failed gate carries NO bound — not in one direction and not in both. What this study buys is therefore narrower than more power and it should not be oversold: it removes the BLIND SPOT that made #1424's observation unusable, so the reversed direction becomes testable at all, and it reports what that test found. A both-directions BOUND is a different outcome with a different name — `resolved_null`, reachable only if the gate passes — and predicting it alongside INCONCLUSIVE would be predicting two things that cannot both happen. It is a PREDICTION and not a requirement: the machinery below decides."
VERDICT_SORT_DETECTED = 'sort_detected'
VERDICT_RESOLVED_NULL = 'resolved_null'
VERDICT_INCONCLUSIVE = 'inconclusive'
VERDICT_LABELS = {VERDICT_SORT_DETECTED: 'HURST SORTS THESE TRADES (EXPLORATORY FINDING)', VERDICT_RESOLVED_NULL: 'NO SORTING IN EITHER DIRECTION AT OR ABOVE THE MEASURED LIMIT', VERDICT_INCONCLUSIVE: 'INCONCLUSIVE'}
EXPLORATORY_ONLY_SENTENCE = 'This study is EXPLORATORY-ONLY (Option 2): the sign it tests was seen before the design was fixed, so no confirmatory claim is available from it, no configuration can win, and no threshold ships.'
MODE_OK = 'ok'
MODE_BELOW_LIMIT = 'below_limit'
MODE_UNRESOLVABLE = 'unresolvable'
MODE_NO_SEPARATION = 'no_separation'
_DEFAULT_JSON_OUT = os.path.join(_THIS_DIR, 'hurst_1426_two_sided_sort.json')
_DEFAULT_REPORT_OUT = os.path.join(_THIS_DIR, 'hurst_1426_two_sided_sort.md')
_CONTRACT_REPORT_OUT = os.path.join(_THIS_DIR, CONTRACT_REPORT_BASENAME)

def doubled_tail_p(n_ge: int, n_le: int, draws: int) -> Optional[float]:
    draws = int(draws)
    if draws <= 0:
        return None
    p_ge = (1.0 + int(n_ge)) / (draws + 1.0)
    p_le = (1.0 + int(n_le)) / (draws + 1.0)
    return round(min(1.0, 2.0 * min(p_ge, p_le)), 6)

def two_sided_permutation_pvalue_group_diff(values: Sequence[float], suppressed: Sequence[bool], n_perm: int=N_PERM, seed: int=SEED) -> Optional[float]:
    vals = np.asarray(values, dtype=float)
    mask = np.asarray(suppressed, dtype=bool)
    if vals.shape != mask.shape:
        raise ValueError('values/suppressed length mismatch')
    n = vals.size
    k = int(mask.sum())
    if n == 0 or k == 0 or k == n:
        return None
    total = float(vals.sum())
    sup_sum = float(vals[mask].sum())
    observed = (total - sup_sum) / (n - k) - sup_sum / k
    rng = np.random.default_rng(seed)
    ge = 0
    le = 0
    remaining = int(n_perm)
    chunk_size = max(1, min(remaining, max(1, 5000000 // max(n, 1))))
    while remaining > 0:
        take = min(chunk_size, remaining)
        keys = rng.random((take, n))
        picks = np.argpartition(keys, k - 1, axis=1)[:, :k]
        sums = vals[picks].sum(axis=1)
        stats = (total - sums) / (n - k) - sums / k
        ge += int(np.count_nonzero(stats >= observed))
        le += int(np.count_nonzero(stats <= observed))
        remaining -= take
    return doubled_tail_p(ge, le, int(n_perm))

def two_sided_permutation_pvalue_weighted(returns: Sequence[float], multipliers: Sequence[float], n_perm: int=N_PERM, seed: int=SEED) -> Optional[float]:
    rets = np.asarray(returns, dtype=float)
    mults = np.asarray(multipliers, dtype=float)
    if rets.shape != mults.shape:
        raise ValueError('returns/multipliers length mismatch')
    n = rets.size
    if n == 0 or float(np.ptp(mults)) == 0.0:
        return None
    observed = float(np.mean(rets * mults))
    rng = np.random.default_rng(seed)
    ge = 0
    le = 0
    remaining = int(n_perm)
    chunk_size = max(1, min(remaining, max(1, 5000000 // max(n, 1))))
    tile = np.broadcast_to(mults, (chunk_size, n))
    while remaining > 0:
        take = min(chunk_size, remaining)
        shuffled = rng.permuted(np.array(tile[:take], copy=True), axis=1)
        stats = (shuffled * rets).mean(axis=1)
        ge += int(np.count_nonzero(stats >= observed))
        le += int(np.count_nonzero(stats <= observed))
        remaining -= take
    return doubled_tail_p(ge, le, int(n_perm))

def two_sided_cluster_permutation_pvalue_group_diff(trades: Sequence[dict], values: Sequence[float], suppressed: Sequence[bool], n_perm: int=N_PERM, seed: int=SEED) -> dict:
    vals = np.asarray(values, dtype=float)
    mask = np.asarray(suppressed, dtype=bool)
    if vals.shape != mask.shape or len(trades) != vals.size:
        raise ValueError('trades/values/suppressed length mismatch')
    n_in = vals.size
    if n_in == 0:
        return {'p': None, 'n_draws': 0, 'excluded_datasets': [], 'n_scored': 0, 'n_excluded_trades': 0, 'offset_range': None, 'reason': 'no testable contrast'}
    idx, excluded = usable_cluster_rows(trades)
    n_excluded = n_in - len(idx)
    if not idx:
        return {'p': None, 'n_draws': 0, 'excluded_datasets': excluded, 'n_scored': 0, 'n_excluded_trades': n_excluded, 'offset_range': None, 'reason': 'no dataset spans enough calendar time to rotate'}
    trades = [trades[i] for i in idx]
    vals = vals[idx]
    mask = mask[idx]
    n = vals.size
    k = int(mask.sum())
    if k == 0 or k == n:
        return {'p': None, 'n_draws': 0, 'excluded_datasets': excluded, 'n_scored': n, 'n_excluded_trades': n_excluded, 'offset_range': None, 'reason': 'no testable contrast'}
    clusters = cluster_rotation_offsets(trades)
    bounds = _admissible_offsets(clusters)
    if not bounds:
        return {'p': None, 'n_draws': 0, 'excluded_datasets': excluded, 'n_scored': n, 'n_excluded_trades': n_excluded, 'offset_range': None, 'reason': 'no dataset spans enough calendar time to rotate'}
    lo, hi = bounds
    observed = float(vals[~mask].mean() - vals[mask].mean())
    rng = np.random.default_rng(seed)
    offsets = rng.integers(lo, hi + 1, size=int(n_perm))
    ge = 0
    le = 0
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
        if stat <= observed:
            le += 1
    if draws == 0:
        return {'p': None, 'n_draws': 0, 'excluded_datasets': excluded, 'n_scored': n, 'n_excluded_trades': n_excluded, 'offset_range': [int(lo), int(hi)], 'reason': 'every rotation collapsed the split'}
    return {'p': doubled_tail_p(ge, le, draws), 'n_draws': draws, 'excluded_datasets': excluded, 'n_scored': n, 'n_excluded_trades': n_excluded, 'n_distinct_offsets': int(hi) - int(lo) + 1, 'offset_range': [int(lo), int(hi)]}

def two_sided_cluster_permutation_pvalue_weighted(trades: Sequence[dict], returns: Sequence[float], multipliers: Sequence[float], n_perm: int=N_PERM, seed: int=SEED) -> dict:
    rets = np.asarray(returns, dtype=float)
    mults = np.asarray(multipliers, dtype=float)
    if rets.shape != mults.shape or len(trades) != rets.size:
        raise ValueError('trades/returns/multipliers length mismatch')
    n_in = rets.size
    if n_in == 0:
        return {'p': None, 'n_draws': 0, 'excluded_datasets': [], 'n_scored': 0, 'n_excluded_trades': 0, 'offset_range': None, 'reason': 'multipliers carry no variation'}
    idx, excluded = usable_cluster_rows(trades)
    n_excluded = n_in - len(idx)
    if not idx:
        return {'p': None, 'n_draws': 0, 'excluded_datasets': excluded, 'n_scored': 0, 'n_excluded_trades': n_excluded, 'offset_range': None, 'reason': 'no dataset spans enough calendar time to rotate'}
    trades = [trades[i] for i in idx]
    rets = rets[idx]
    mults = mults[idx]
    if float(np.ptp(mults)) == 0.0:
        return {'p': None, 'n_draws': 0, 'excluded_datasets': excluded, 'n_scored': rets.size, 'n_excluded_trades': n_excluded, 'offset_range': None, 'reason': 'multipliers carry no variation'}
    clusters = cluster_rotation_offsets(trades)
    bounds = _admissible_offsets(clusters)
    if not bounds:
        return {'p': None, 'n_draws': 0, 'excluded_datasets': excluded, 'n_scored': rets.size, 'n_excluded_trades': n_excluded, 'offset_range': None, 'reason': 'no dataset spans enough calendar time to rotate'}
    lo, hi = bounds
    observed = float(np.mean(rets * mults))
    rng = np.random.default_rng(seed)
    offsets = rng.integers(lo, hi + 1, size=int(n_perm))
    ge = 0
    le = 0
    draws = 0
    for off in offsets:
        shifts = rotation_shift_counts(clusters, int(off))
        rot = _rotate_values(mults, clusters, shifts)
        draws += 1
        stat = float(np.mean(rets * rot))
        if stat >= observed:
            ge += 1
        if stat <= observed:
            le += 1
    if draws == 0:
        return {'p': None, 'n_draws': 0, 'excluded_datasets': excluded, 'n_scored': rets.size, 'n_excluded_trades': n_excluded, 'offset_range': [int(lo), int(hi)], 'reason': 'no valid draw'}
    return {'p': doubled_tail_p(ge, le, draws), 'n_draws': draws, 'excluded_datasets': excluded, 'n_scored': rets.size, 'n_excluded_trades': n_excluded, 'n_distinct_offsets': int(hi) - int(lo) + 1, 'offset_range': [int(lo), int(hi)]}

def two_sided_min_detectable_effect_on_grid(trades: Sequence[dict], values: Sequence[float], suppressed: Sequence[bool], family_size: int, *, grid_step: float, grid_max: float, refine_step: float, cluster: bool=True, n_perm: int=N_PERM_MDE, seed: int=SEED, alpha: float=ALPHA) -> Optional[float]:
    vals = np.asarray(values, dtype=float)
    mask = np.asarray(suppressed, dtype=bool)
    bar = _rank1_threshold(family_size, alpha)
    floor = 2.0 / (float(n_perm) + 1.0)
    if floor > bar:
        raise ValueError(f'n_perm={n_perm} cannot resolve the rank-1 Benjamini-Hochberg bar {bar:g} for a family of {family_size} under the TWO-SIDED doubled-tail convention (smallest reachable p is {floor:g}); raise --n-perm-mde to at least {int(math.ceil(2.0 / bar))}')
    if vals.size == 0 or int(mask.sum()) in (0, vals.size):
        return None

    def _p_one_direction(d: float) -> Optional[float]:
        shifted = vals - np.where(mask, float(d), 0.0)
        if cluster:
            return two_sided_cluster_permutation_pvalue_group_diff(trades, shifted, mask, n_perm=n_perm, seed=seed).get('p')
        return two_sided_permutation_pvalue_group_diff(shifted, mask, n_perm=n_perm, seed=seed)

    def _p_at(d: float) -> Optional[float]:
        up = _p_one_direction(float(d))
        down = _p_one_direction(-float(d))
        if up is None or down is None:
            return None
        return max(float(up), float(down))
    coarse = None
    steps = int(round(float(grid_max) / float(grid_step)))
    for i in range(steps + 1):
        d = round(i * float(grid_step), 9)
        p = _p_at(d)
        if p is not None and p <= bar:
            coarse = d
            break
    if coarse is None:
        return None
    if coarse == 0.0:
        return 0.0
    lo = max(0.0, coarse - float(grid_step))
    fine = coarse
    n_fine = int(round(float(grid_step) / float(refine_step)))
    for i in range(n_fine + 1):
        d = round(lo + i * float(refine_step), 9)
        p = _p_at(d)
        if p is not None and p <= bar:
            fine = d
            break
    return round(float(fine), 6)

def two_sided_min_detectable_effect_eff(trades, values, suppressed, family_size, **kw):
    return two_sided_min_detectable_effect_on_grid(trades, values, suppressed, family_size, grid_step=MDE_EFF_GRID_STEP, grid_max=MDE_EFF_GRID_MAX, refine_step=MDE_EFF_REFINE_STEP, **kw)

def two_sided_min_detectable_effect_pp(trades, values, suppressed, family_size, **kw):
    return two_sided_min_detectable_effect_on_grid(trades, values, suppressed, family_size, grid_step=MDE_PP_GRID_STEP, grid_max=MDE_PP_GRID_MAX, refine_step=MDE_PP_REFINE_STEP, **kw)

def measure_detection_limits(pooled: dict, hurst_windows: Sequence[int], n_perm: int, seed: int) -> dict:
    out: dict = {'by_family_cluster': {}, 'by_family_cluster_return': {}, 'by_family_separation': {}, 'by_family_separation_return': {}, 'by_family_cluster_p0': {}, 'by_family_cluster_return_p0': {}, 'by_family_n': {}}
    hw = max(hurst_windows)

    def _pool(family: str, cohort: Optional[str], only_1410_cells: bool):
        rows = []
        for t in pooled.get(family) or []:
            if cohort is not None and t['cohort'] != cohort:
                continue
            key = (dataset_key(t['symbol'], t['timeframe']), t['window'])
            if only_1410_cells and key not in D_1410:
                continue
            rows.append(t)
        return rows

    def _split(rows, family: str):
        sense = FAMILY_SENSE[family]
        keep, values, returns, mask = ([], [], [], [])
        for t in rows:
            h = (t.get('h') or {}).get(hw)
            if h is None or not math.isfinite(float(h)):
                continue
            if t.get('efficiency') is None:
                continue
            keep.append(t)
            values.append(float(t['efficiency']))
            returns.append(float(t['pnl_pct_net']))
            mask.append(anti_signal_side(float(h), sense))
        return (keep, values, returns, mask)
    specs = (('1410', None, True, 30), ('primary', COHORT_PRIMARY, False, PRIMARY_FAMILY_SIZE), ('exploratory', COHORT_EXPLORATORY, False, 30))
    by_pool: dict = {}
    by_pool_return: dict = {}
    for label, cohort, only_1410, family_size in specs:
        rows_all, vals_all, rets_all, mask_all, fams_all = ([], [], [], [], [])
        for family in FAMILIES:
            rows, vals, rets, mask = _split(_pool(family, cohort, only_1410), family)
            rows_all += rows
            vals_all += vals
            rets_all += rets
            mask_all += mask
            fams_all += [family] * len(rows)
            if label == 'primary':
                fam_idx, _ = usable_cluster_rows(rows)
                fam_rows = [rows[i] for i in fam_idx]
                fam_vals = [vals[i] for i in fam_idx]
                fam_rets = [rets[i] for i in fam_idx]
                fam_mask = [mask[i] for i in fam_idx]
                out['by_family_cluster'][family] = two_sided_min_detectable_effect_eff(fam_rows, fam_vals, fam_mask, family_size, cluster=True, n_perm=n_perm, seed=seed)
                out['by_family_cluster_return'][family] = two_sided_min_detectable_effect_pp(fam_rows, fam_rets, fam_mask, family_size, cluster=True, n_perm=n_perm, seed=seed)
                out['by_family_separation'][family] = _separation(fam_vals, fam_mask)
                out['by_family_separation_return'][family] = _separation(fam_rets, fam_mask)
                out['by_family_n'][family] = len(fam_rows)
                if fam_rows and 0 < int(np.sum(fam_mask)) < len(fam_mask):
                    out['by_family_cluster_p0'][family] = two_sided_cluster_permutation_pvalue_group_diff(fam_rows, fam_vals, fam_mask, n_perm=n_perm, seed=seed).get('p')
                    out['by_family_cluster_return_p0'][family] = two_sided_cluster_permutation_pvalue_group_diff(fam_rows, fam_rets, fam_mask, n_perm=n_perm, seed=seed).get('p')
                else:
                    out['by_family_cluster_p0'][family] = None
                    out['by_family_cluster_return_p0'][family] = None
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
            sep = _separation([vals_all[i] for i in own], [mask_all[i] for i in own])
            if sep is not None:
                pool_obs[f'{family}|{hw}'] = sep
            sep_ret = _separation([rets_all[i] for i in own], [mask_all[i] for i in own])
            if sep_ret is not None:
                pool_obs_ret[f'{family}|{hw}'] = sep_ret
        by_pool[label] = pool_obs
        by_pool_return[label] = pool_obs_ret
        out[f'pooled_{label}_cluster'] = two_sided_min_detectable_effect_eff(rows_all, vals_all, mask_all, family_size, cluster=True, n_perm=n_perm, seed=seed)
        out[f'pooled_{label}_free'] = two_sided_min_detectable_effect_eff(rows_all, vals_all, mask_all, family_size, cluster=False, n_perm=n_perm, seed=seed)
        out[f'pooled_{label}_cluster_return'] = two_sided_min_detectable_effect_pp(rows_all, rets_all, mask_all, family_size, cluster=True, n_perm=n_perm, seed=seed)
        out[f'pooled_{label}_n'] = len(rows_all)
        if rows_all and 0 < int(np.sum(mask_all)) < len(mask_all):
            out[f'pooled_{label}_cluster_p0'] = two_sided_cluster_permutation_pvalue_group_diff(rows_all, vals_all, mask_all, n_perm=n_perm, seed=seed).get('p')
            out[f'pooled_{label}_free_p0'] = two_sided_permutation_pvalue_group_diff(vals_all, mask_all, n_perm=n_perm, seed=seed)
            out[f'pooled_{label}_cluster_return_p0'] = two_sided_cluster_permutation_pvalue_group_diff(rows_all, rets_all, mask_all, n_perm=n_perm, seed=seed).get('p')
        else:
            out[f'pooled_{label}_cluster_p0'] = None
            out[f'pooled_{label}_free_p0'] = None
            out[f'pooled_{label}_cluster_return_p0'] = None
    out['observed_separation_by_pool'] = by_pool
    out['observed_separation_pp_by_pool'] = by_pool_return
    out['primary_target'] = PRIMARY_TARGET
    out['continuity_target'] = CONTINUITY_TARGET
    out['horizon_hours'] = HORIZON_HOURS
    out['two_sided'] = TWO_SIDED
    return out

def validity_gate(mde: dict) -> dict:
    family = PRIMARY_FAMILY
    limit = (mde.get('by_family_cluster') or {}).get(family)
    sep = (mde.get('by_family_separation') or {}).get(family)
    base = {'family': family, 'n_rows': (mde.get('by_family_n') or {}).get(family)}
    if sep is None:
        return dict(base, passed=False, limit=limit, largest_separation=None, mode=MODE_NO_SEPARATION, two_sided=True, reason=f'the confirmatory family (`{family}`) carries no measurable separation')
    sep = round(float(sep), 6)
    if limit is None:
        return dict(base, passed=False, limit=None, largest_separation=sep, mode=MODE_UNRESOLVABLE, two_sided=True, reason=f'the confirmatory family (`{family}`) has a two-sided detection limit above {MDE_EFF_GRID_MAX:g} efficiency units, so no effect on the injection grid is resolvable in either direction')
    passed = bool(abs(sep) >= float(limit))
    return dict(base, passed=passed, limit=round(float(limit), 6), largest_separation=sep, reason='', two_sided=True, mode=MODE_OK if passed else MODE_BELOW_LIMIT)

def _direction_phrase(separation: float) -> str:
    return "the KEPT trades did better, the direction the gate's own hypothesis claims" if float(separation) >= 0 else "the SUPPRESSED trades did better, the OPPOSITE of the gate's own hypothesis — a gate built on it would have HURT"

def confirmatory_p(mde: dict) -> Optional[float]:
    return (mde.get('by_family_cluster_p0') or {}).get(PRIMARY_FAMILY)

def decide_recommendation(configs: Sequence[dict], mde: dict) -> dict:
    gate = validity_gate(mde)
    p_conf = confirmatory_p(mde)
    bar = _rank1_threshold(PRIMARY_FAMILY_SIZE, ALPHA)
    significant = p_conf is not None and float(p_conf) <= bar
    primary = [c for c in configs if c.get('cohort') == COHORT_PRIMARY]
    families = {}
    for family in FAMILIES:
        own = [c for c in primary if c.get('family') == family]
        n_passing = sum((1 for c in own if config_verdict(c)[0]))
        families[family] = {'winner': None, 'n_tested': len(own), 'n_passing': n_passing}
    sep = gate.get('largest_separation')
    p_text = 'untestable' if p_conf is None else f'{float(p_conf):.4f}'
    head = f"On the confirmatory family (`{gate.get('family', PRIMARY_FAMILY)}`, {gate.get('n_rows') or 0} row-matched rows) the TWO-SIDED zero-injection cluster p is {p_text} against a rank-1 Benjamini-Hochberg bar of {bar:g}"
    if significant:
        verdict = VERDICT_SORT_DETECTED
        detail = f'{head}, so the contrast is REJECTED in a test that could have rejected it either way. Hurst at entry SORTS these trades: the separation is {_fmt_signed(sep, 3)} efficiency units, which means {_direction_phrase(sep if sep is not None else 0.0)}.'
        if gate['passed']:
            detail += f" The validity gate also PASSED — the two-sided limit of {gate['limit']:.3f} sits at or below the magnitude of that separation — so the effect is one this design can resolve rather than one it merely reached significance on."
        else:
            detail += ' The validity gate did NOT pass, so the effect sits below the size this design resolves; treat the significance as fragile and the magnitude as unestimated.'
    elif gate['passed']:
        verdict = VERDICT_RESOLVED_NULL
        detail = f"{head}, so the contrast is NOT rejected. The validity gate PASSED: the two-sided limit on those same rows is {gate['limit']:.3f} efficiency units and their separation is {_fmt_signed(sep, 3)}, whose magnitude the design can resolve. So this is a statement about the data and not about the test: Hurst at entry does not sort these trades IN EITHER DIRECTION at an effect size this design can see."
    else:
        verdict = VERDICT_INCONCLUSIVE
        if gate['reason']:
            detail = f"{head}. The validity gate FAILED: {gate['reason']}, so the run carries no bound in either direction."
        else:
            detail = f"{head}. The validity gate FAILED: the two-sided limit on those rows is {gate['limit']:.3f} efficiency units while they separate by only {_fmt_signed(sep, 3)}, whose magnitude sits BELOW the limit. Nothing that small is visible to this design, so the run bounds any sorting effect — in EITHER direction, which is what a symmetric test buys and what #1424's one-sided run could not offer — from above at {gate['limit']:.3f} efficiency units, and says nothing about anything smaller."
    n_untestable = sum((1 for c in primary if c.get('p_cluster') is None))
    n_significant = sum((1 for c in primary if c.get('bh_reject')))
    tested = sum((v['n_tested'] for v in families.values()))
    return {'verdict': verdict, 'families': families, 'validity_gate': gate, 'confirmatory_p': p_conf, 'confirmatory_bar': bar, 'significant': bool(significant), 'key_risk_held': bool(gate['passed']), 'cohort_option': COHORT_OPTION, 'justification': f"{detail} {EXPLORATORY_ONLY_SENTENCE} Across the {tested} primary-cohort {('configuration' if tested == 1 else 'configurations')} swept, {n_significant} reached Benjamini-Hochberg significance on the two-sided cluster permutation and {n_untestable} were untestable; neither count can promote a configuration, because this study has no branch that promotes one.".strip()}

def decision_payload(decision: dict) -> dict:
    return {'verdict': decision['verdict'], 'justification': decision['justification'], 'validity_gate': decision['validity_gate'], 'confirmatory_p': decision['confirmatory_p'], 'confirmatory_bar': decision['confirmatory_bar'], 'significant': decision['significant'], 'key_risk_held': decision['key_risk_held'], 'cohort_option': decision['cohort_option'], 'families': {f: {'n_tested': d['n_tested'], 'n_passing': d['n_passing'], 'winner': (d['winner'] or {}).get('config_id') if isinstance(d['winner'], dict) else d['winner']} for f, d in decision['families'].items()}}

def build_configs(legs: Sequence[dict], pooled: dict, hurst_windows: Sequence[int], rho_by_symbol: dict, n_perm: int, seed: int) -> list:
    configs = []
    for cohort in (COHORT_PRIMARY, COHORT_EXPLORATORY):
        for family, mode, hw, arm, disarm, gain in _sweep_grid(cohort, hurst_windows):
            sense = FAMILY_SENSE[family]
            trades = [t for t in pooled.get(family) or [] if t['cohort'] == cohort]
            cfg = _config_shell(family, cohort, mode, hw, arm, disarm, gain)
            cid = cfg['config_id']
            if mode == 'gate':
                sub = [t for t in trades if cid in t['armed']]
            else:
                sub = list(trades)
            sub, n_missing_target = _target_rows(sub)
            idx, excluded = usable_cluster_rows(sub)
            n_excluded = len(sub) - len(idx)
            sub = [sub[i] for i in idx]
            values = [float(t['efficiency']) for t in sub]
            returns = [float(t['pnl_pct_net']) for t in sub]
            if mode == 'gate':
                suppressed = [not t['armed'][cid] for t in sub]
                cfg['p_raw'] = two_sided_permutation_pvalue_group_diff(values, suppressed, n_perm=n_perm, seed=seed)
                cluster = two_sided_cluster_permutation_pvalue_group_diff(sub, values, suppressed, n_perm=n_perm, seed=seed)
                cfg['p_raw_return'] = two_sided_permutation_pvalue_group_diff(returns, suppressed, n_perm=n_perm, seed=seed)
                cfg['p_cluster_return'] = two_sided_cluster_permutation_pvalue_group_diff(sub, returns, suppressed, n_perm=n_perm, seed=seed).get('p')
                sup_rows = [t for t, s in zip(sub, suppressed) if s]
                kept_rows = [t for t, s in zip(sub, suppressed) if not s]
                cfg['separation'] = _separation(values, suppressed)
                cfg['separation_return'] = _separation(returns, suppressed)
                cfg['windows'] = _window_rows_gate(legs, family, cohort, cid)
            else:
                mults = [size_multiplier((t.get('h') or {}).get(hw), sense, gain) for t in sub]
                cfg['p_raw'] = two_sided_permutation_pvalue_weighted(values, mults, n_perm=n_perm, seed=seed)
                cluster = two_sided_cluster_permutation_pvalue_weighted(sub, values, mults, n_perm=n_perm, seed=seed)
                cfg['p_raw_return'] = two_sided_permutation_pvalue_weighted(returns, mults, n_perm=n_perm, seed=seed)
                cfg['p_cluster_return'] = two_sided_cluster_permutation_pvalue_weighted(sub, returns, mults, n_perm=n_perm, seed=seed).get('p')
                sup_rows = [t for t, m in zip(sub, mults) if m < 1.0]
                kept_rows = [t for t, m in zip(sub, mults) if m >= 1.0]
                down = [m < 1.0 for m in mults]
                cfg['separation'] = _separation(values, down)
                cfg['separation_return'] = _separation(returns, down)
                cfg['windows'] = _window_rows_size(legs, family, cohort, hw, gain)
            cfg['p_cluster'] = cluster.get('p')
            cfg['cluster_draws'] = cluster.get('n_draws')
            cfg['cluster_excluded_datasets'] = excluded
            cfg['cluster_excluded_trades'] = n_excluded
            cfg['cluster_offset_range'] = cluster.get('offset_range')
            cfg['cluster_distinct_offsets'] = cluster.get('n_distinct_offsets')
            cfg['cluster_reason'] = cluster.get('reason')
            cfg['n_pooled_trades'] = len(sub)
            cfg['n_missing_target'] = n_missing_target
            cfg['n_suppressed'] = len(sup_rows)
            cfg['n_kept'] = len(kept_rows)
            cfg['n_pooled_effective'] = effective_n(sub, rho_by_symbol)
            cfg['n_suppressed_effective'] = effective_n(sup_rows, rho_by_symbol)
            cfg['n_kept_effective'] = effective_n(kept_rows, rho_by_symbol)
            configs.append(cfg)
    return configs

def apply_bh_by_cohort(configs: Sequence[dict], alpha: float=ALPHA) -> None:
    for cohort in (COHORT_PRIMARY, COHORT_EXPLORATORY):
        own = [c for c in configs if c.get('cohort') == cohort]
        for cfg in own:
            cfg['bh_reject'] = False
        testable = [c for c in own if c.get('p_cluster') is not None]
        if not testable:
            continue
        flags = benjamini_hochberg([c['p_cluster'] for c in testable], alpha=alpha, family_size=len(own))
        for cfg, flag in zip(testable, flags):
            cfg['bh_reject'] = bool(flag)

def joint_separation_verdict(trades: Sequence[dict], hurst_window: int, n_perm: int=N_PERM, seed: int=SEED) -> dict:
    return study1422.joint_separation_verdict(trades, hurst_window, n_perm=n_perm, seed=seed)

def _largest_magnitude_signed(seps: dict):
    values = [float(v) for v in (seps or {}).values() if v is not None]
    if not values:
        return None
    return max(values, key=lambda v: (abs(v), v))

def _render_gate_sentence(gate: dict) -> str:
    if gate.get('limit') is None or gate.get('largest_separation') is None:
        return gate.get('reason') or 'the gate could not be evaluated.'
    if gate.get('reason'):
        return gate['reason'] + '.'
    relation = 'at or below' if gate['passed'] else 'ABOVE'
    return f"on the confirmatory family (`{gate.get('family', PRIMARY_FAMILY)}`) the two-sided detection limit is {gate['limit']:.3f} efficiency units, {relation} the magnitude of the {gate['largest_separation']:+.3f} those SAME rows separate by."

def _render_config_table(cfgs: Sequence[dict], protocol: Sequence[str]) -> list:
    head = '| Config | Mode | W | Pooled N (eff) | sup/kept eff | separation (eff, signed) | 2-sided free p | 2-sided cluster p | 2-sided cluster p (net ret) | BH sig |'
    sep = '|--------|------|--:|----------------|--------------|----------------------------:|---------------:|-----------------:|----------------------------:|:------:|'
    for w in protocol:
        head += f' {w} dd | {w} chop | {w} ret (arm/base) |'
        sep += '------:|--------:|-------------------|'
    head += ' #1424 rule |'
    sep += '------------|'
    lines = [head, sep]
    for cfg in cfgs:
        row = f"| `{cfg['config_id']}` | {cfg['mode']} | {cfg['hurst_window']} | {cfg['n_pooled_trades']} ({_fmt(cfg['n_pooled_effective'], 1)}) | {_fmt(cfg['n_suppressed_effective'], 1)}/{_fmt(cfg['n_kept_effective'], 1)} | {_fmt_signed(cfg.get('separation'), 4)} | {_fmt_p(cfg['p_raw'])} | {_fmt_p(cfg['p_cluster'])} | {_fmt_p(cfg.get('p_cluster_return'))} | {('yes' if cfg.get('bh_reject') else 'no')} |"
        for w in protocol:
            r = cfg['windows'].get(w) or {}
            if not r.get('n_legs'):
                row += ' - | - | - |'
            else:
                row += f" {_fmt(r['dd_delta'])} | {_fmt(r['chop_delta'])} | {_fmt(r['ret_gated'])} / {_fmt(r['ret_ungated'])} |"
        ok, reasons = config_verdict(cfg)
        row += f" {('would pass' if ok else '; '.join(reasons))} |"
        lines.append(row)
    lines.append('')
    return lines

def render_recommendation(decision: dict, mde: dict, configs: Sequence[dict]=()) -> str:
    gate = decision.get('validity_gate') or validity_gate(mde)
    lines = ['## Recommendation', '']
    lines.append(VERDICT_LABELS.get(decision['verdict'], decision['verdict'].upper()))
    lines.append('')
    lines.append(decision['justification'])
    lines.append('')
    lines.append('### The pre-registered prediction, and what happened')
    lines.append('')
    lines.append(f'> {KEY_RISK_PREDICTION}')
    lines.append('')
    if decision['verdict'] == VERDICT_SORT_DETECTED:
        lines.append('The prediction did NOT hold, and that is the interesting outcome. A two-sided test rejected the contrast, so Hurst at entry sorts these trades by an amount this design registers — a result the one-sided predecessors could not have produced whichever way the data pointed. Under Option 2 it is a HYPOTHESIS, not a finding to act on: the sign was known before the design was fixed, so the next step is a pre-registered test on tape this study did not score, and there is none available today.')
    elif decision.get('key_risk_held'):
        lines.append("The prediction HELD in its useful half. The contrast is not rejected AND the validity gate passed, so the run bounds any sorting effect in BOTH directions at the measured limit. That is the concrete thing #1426 was built to produce: #1424's observation was unusable because its design could not speak to the direction the data pointed, and this run replaces it with a symmetric bound. The bound is economically negligible at this resolution.")
    else:
        lines.append("The prediction is UNRESOLVED. The contrast is not rejected, but the validity gate did not pass either, so the run carries no bound — in either direction. A symmetric test removes the BLIND SPOT that made #1424's observation unusable; it does not manufacture power. Anyone reopening this question needs a design that resolves a smaller effect, and the calendar and the venues this repository can reach are already exhausted.")
    lines.append('')
    lines.append('**No configuration is recommended, and none could be.** ' + EXPLORATORY_ONLY_SENTENCE + " `decide_recommendation` has no branch that promotes a configuration and this module defines no configuration verdict at all; the per-family table below reports how many configs WOULD have passed #1424's acceptance rule purely as a description of the data.")
    lines.append('')
    lines.append("| Family | Primary configs tested | Would pass #1424's rule | Recommended |")
    lines.append('|--------|----------------------:|------------------------:|:-----------:|')
    for family in FAMILIES:
        entry = decision['families'][family]
        lines.append(f"| `{family}` | {entry['n_tested']} | {entry['n_passing']} | none (structurally) |")
    lines.append('')
    lines.append("#1411's `hurst_gate` stays DEFAULT-OFF with no recommended thresholds. Nothing in this report licenses shipping one, and `config.example.json` carries no `hurst_gate` block.")
    lines.append('')
    lines.append(CONTRACT_PATH_STATEMENT)
    return '\n'.join(lines).rstrip() + '\n'

def render_report(payload: dict) -> str:
    pre = payload['pre_registered']
    run = payload['run_summary']
    cfgs = payload['configs']
    mde = payload.get('mde') or {}
    decision = payload['decision']
    hurst_windows = pre['hurst_windows']
    gate = decision.get('validity_gate') or validity_gate(mde)
    out = []
    out.append('# Hurst entry hypothesis, tested in BOTH directions (#1426)')
    out.append('')
    out.append('Report-only research. Nothing here is wired to the scheduler, to config, or to any live path. This file is NOT the live-evidence contract path: `hurst_gate_calibration.md` stays with the #1424 resolution study, and this script refuses to write it.')
    out.append('')
    out.append('#1410, #1422 and #1424 all tested ONE hypothesis — that trades taken at high Hurst persistence beat trades taken at low persistence — and all three tested it ONE-SIDED. #1424 measured the consequence: on its confirmatory family the row-matched detection limit is 0.013 efficiency units while those same rows separate by **-0.005**. The sign is the finding. `mean(kept) - mean(suppressed)` going NEGATIVE means the trades the gate would have SUPPRESSED did BETTER, and that is the one direction a one-sided design cannot detect at any magnitude. The observation was therefore neither a rejection nor a confirmation. This study re-runs the same machinery with the inference made SYMMETRIC, so a reversed effect becomes a finding instead of a blind spot.')
    out.append('')
    out.append(f"Generated by `backtest/research/hurst_1426_two_sided_sort.py` (schema {payload['schema_version']}). Every number below is rendered from `hurst_1426_two_sided_sort.json`, produced by the same run.")
    out.append('')
    out.append('## Verdict at a glance')
    out.append('')
    out.append(f'- Cohort decision: **Option 2 — exploratory-only, full pool**. No confirmatory claim is available from this run.')
    out.append(f"- Confirmatory two-sided cluster p on `{PRIMARY_FAMILY}`: **{_fmt_p(decision.get('confirmatory_p'))}** against a Benjamini-Hochberg rank-1 bar of {decision.get('confirmatory_bar', ALPHA):g}.")
    out.append(f"- Separation on those same rows: **{_fmt_signed(gate.get('largest_separation'), 4)}** efficiency units — " + (_direction_phrase(gate['largest_separation']) if gate.get('largest_separation') is not None else 'no measurable separation') + '.')
    out.append(f"- Validity gate: **{('PASSED' if gate['passed'] else 'FAILED')}** — " + _render_gate_sentence(gate))
    out.append(f"- Verdict: **{VERDICT_LABELS.get(decision['verdict'], decision['verdict'].upper())}**.")
    out.append(f"- Contract path: **deferred to #1424**; siblings {', '.join(('#' + str(n) for n in SIBLING_DEFERRAL))} may still claim it.")
    out.append('')
    out.append('## The cohort decision, and its cost')
    out.append('')
    out.append(COHORT_DECISION_STATEMENT)
    out.append('')
    out.append('### Interim look, disclosed')
    out.append('')
    out.append(INTERIM_LOOK_DISCLOSURE)
    out.append('')
    out.append('## Two-sided inference')
    out.append('')
    out.append('Every p-value on the confirmatory path — both cohorts, both targets, the gate arm and the sizing arm, free-shuffle and cluster-rotation — is a DOUBLED ONE-TAILED permutation p:')
    out.append('')
    out.append('```')
    out.append('p_ge = (1 + #{stat >= obs}) / (draws + 1)')
    out.append('p_le = (1 + #{stat <= obs}) / (draws + 1)')
    out.append('p2   = min(1, 2 * min(p_ge, p_le))')
    out.append('```')
    out.append('')
    out.append('The doubled tail is used rather than the `|stat| >= |obs|` form because neither statistic is guaranteed symmetric under its own null: the group difference is not, under unequal group sizes, and the weighted (sizing) statistic is not centred at zero at all. Doubling the smaller tail is valid-conservative for both, uniformly. Both tails are counted over the draws the null ACTUALLY produced, never over the requested count — a cluster rotation discards draws that collapse the split.')
    out.append('')
    out.append(f"""The smallest reachable p is therefore `2/(draws+1)`, and the detection-limit search checks THAT floor rather than `1/(n+1)`: a run with too few draws would find every effect undetectable and publish "no power" when the truth is "no permutations". It RAISES instead. At {pre['n_perm_mde']} detection-limit draws the floor is {2.0 / (float(pre['n_perm_mde']) + 1.0):.5f}. The bar it has to clear is not the confirmatory one: the #1410 and exploratory pools are scored against a family of 30, whose rank-1 bar is {ALPHA / 30.0:.5f}, and that is the BINDING constraint on this run's draw count. The confirmatory family's own bar is {ALPHA / max(1, PRIMARY_FAMILY_SIZE):g} and is far easier to resolve. Doubling the tail doubles the required draws against #1424's one-sided floor, which is why a draw count that sufficed there can fail loudly here.""")
    out.append('')
    out.append(f"The DETECTION LIMIT injects both directions at every grid point — the suppressed values shifted down by `d`, then up by `d` — and scores the point by `max(p2(+d), p2(-d))`. The published limit is therefore the smallest effect this design would catch WHICHEVER WAY it points. Taking the minimum instead would publish the easier direction as the test's resolution, which is the one-sided error in a new costume. The grids are #1424's verbatim ({MDE_EFF_GRID_STEP:g} to {MDE_EFF_GRID_MAX:g} with a {MDE_EFF_REFINE_STEP:g} refinement on the primary target; #1422's {MDE_PP_GRID_STEP:g}-to-{MDE_PP_GRID_MAX:g} pp grid on the continuity target), so the limits stay directly comparable and the only thing a reader has to account for is the symmetry.")
    out.append('')
    out.append('ONE DELIBERATE EXCEPTION, disclosed rather than buried: the joint ADX x Hurst **Stage 0** verdict in Part D is inherited from #1422 VERBATIM and stays ONE-SIDED on net return. #1424 pinned it that way so the verdict recorded against #1412 stays comparable across studies. It sits outside the confirmatory path — nothing in the config sweep, the detection limits, the validity gate or the verdict calls it — and a test pins that.')
    out.append('')
    out.append('### The validity gate')
    out.append('')
    out.append(f"This study is VALID only when the measured two-sided cluster-null detection limit on the CONFIRMATORY family (`{PRIMARY_FAMILY}`, the family the single pinned hypothesis belongs to) falls at or below the MAGNITUDE of that same family's observed separation, both in efficiency units and both measured on the IDENTICAL rows.")
    out.append('')
    out.append("The gate reads a MAGNITUDE here, and #1424's read a SIGNED value. That is not a relaxation: #1424's nulls and its injection were one-sided in the `mean(kept) - mean(suppressed)` orientation, so a negative separation pointed the way its design could not detect at any size and no limit spoke to it. Here the null is two-sided and the injection is applied in both directions with the limit taken as the harder of the two, so the limit describes an effect detectable whichever way it points. The magnitude comparison is legitimate ONLY because of that, and a future edit that makes any p on this path one-sided again must restore the signed comparison in the same change.")
    out.append('')
    out.append("The SIGN is still carried and still reported everywhere, because it is what says WHICH way a finding points. And the gate still refuses the POOLED limit: that number spans both families and would resolve a smaller effect purely by holding more trades, so pairing it with one family's separation is the whole-study-versus-sub-cohort mismatch the pool-matched tables exist to prevent, and it biases the gate toward passing.")
    out.append('')
    out.append(f"**Outcome: {('PASSED' if gate['passed'] else 'FAILED')}** — " + _render_gate_sentence(gate))
    out.append('')
    out.append('## Pre-registered design')
    out.append('')
    out.append('Inherited from #1424 unless the two-sided section above names it. These constants are imported from that module rather than restated, so a drift there fails loud here instead of silently rescoring different cells.')
    out.append('')
    out.append('- Estimator: `hurst_exponent` from `shared_strategies/open/indicators_core.py` (#1409 SSoT). Never reimplemented here.')
    out.append('- NaN policy: NaN is its OWN bucket for BOTH H and ADX, never coerced to 0.5 / 25. It neither arms nor disarms the gate (state holds) and gives a size multiplier of exactly 1.')
    out.append(f"- Hurst window lengths: {', '.join((str(h) for h in hurst_windows))} bars.")
    out.append(f"- Buckets on H at entry: {', '.join(('`' + b + '`' for b in BUCKETS))}.")
    out.append(f'- PRIMARY TARGET: `{PRIMARY_TARGET}` over a fixed {HORIZON_HOURS}-hour horizon, bounded in `[-1, 1]`. CONTINUITY TARGET: `{CONTINUITY_TARGET}`, scored on the SAME rows so the two describe one pool.')
    out.append(f'- PINNED HYPOTHESIS: `{PRIMARY_CONFIG_ID}`, inherited from #1424, which derived it mechanically as the smallest raw p over all 30 configs in the committed `hurst_1410_gate_calibration.json` and re-derives it at run time with a hard assert. The Benjamini-Hochberg denominator for the confirmatory family is {PRIMARY_FAMILY_SIZE}. Under Option 2 this is a PINNED hypothesis and not a pre-registered one: the sign it is tested against was already visible.')
    out.append('- Windows: ' + '; '.join((f"`{k}` {v[0]}..{v[1] or 'latest'}" for k, v in pre['windows'].items())) + '.')
    out.append(f"- Datasets ({len(pre['datasets'])}): {', '.join(('`' + d + '`' for d in pre['datasets']))}.")
    out.append(f"- Fee model: `{pre['fee_platform']}` on every venue, decoupled from the data-source exchange exactly as in #1424.")
    out.append(f'- Volume floors on EFFECTIVE N: >= {MIN_SUPPRESSED_EFFECTIVE:g} suppressed and >= {MIN_KEPT_EFFECTIVE:g} kept.')
    out.append(f"- Inference: {pre['n_perm']} draws, seed {pre['seed']}, minimum rotation offset {MIN_OFFSET_DAYS} days; {pre['n_perm_mde']} draws for the detection limits. Benjamini-Hochberg at alpha={ALPHA}, applied SEPARATELY to the primary and exploratory cohorts.")
    out.append(f'- Joint table: ADX period {ADX_PERIOD} (Wilder), split at {ADX_SPLIT:g}, scored ONE-SIDED on net return — the disclosed exception above.')
    out.append('')
    out.append('### Look-ahead invariant')
    out.append('')
    out.append("Unchanged, and this study touches none of it. Rolling H at bar `i` uses closes `[i-W+1, i]`. The DECISION series is that shifted one bar, so a signal at bar N reads H through N-1; the backtester fills a bar-N signal at N+1's open, so the FILL-BAR stamp is the bar-close series shifted twice. Entry ADX uses the SAME two shifts, with warm-up bars masked to NaN. The primary target reads bars AFTER the fill, which is what an outcome does; it enters no decision anywhere.")
    out.append('')
    out.append('## Coverage and effective sample size')
    out.append('')
    cov = run.get('coverage') or {}
    out.append(f"{cov.get('n_kept', 0)} of {cov.get('n_cells', 0)} OWNED (dataset, window) cells carried enough history to score: {cov.get('required_lead_bars', 0)} bars of lead before the window start, and at least {float(cov.get('min_window_bar_fraction') or 0) * 100:.0f}% of the bars a complete cache would hold inside the window. An open-ended window is closed at ONE run-level reference bar (`{cov.get('reference_last_bar') or '-'}`). {cov.get('n_dropped', 0)} owned cells were DROPPED, listed below; a further {cov.get('n_unowned', 0)} (dataset, window) pairs were never in the design because another venue owns that calendar span.")
    out.append('')
    dropped = cov.get('dropped') or []
    if dropped:
        out.append('| Dataset | Window | Why dropped |')
        out.append('|---------|--------|-------------|')
        for d in dropped:
            out.append(f"| `{d['dataset']}` | `{d['window']}` | {d['reason']} |")
        out.append('')
    else:
        out.append('No owned cells were dropped.')
        out.append('')
    out.append("Effective N is `N^2 / sum_ij rho_ij`, with `rho_ij` the symbol-level daily-return correlation when two trades' holding periods OVERLAP and 0 when they do not; same-asset pairs take 1.0 whatever the quote currency or venue, and correlations are clipped to `[0, 1]` so anti-correlation can never manufacture power.")
    out.append('')
    ex_names = sorted({d for c in cfgs for d in c.get('cluster_excluded_datasets') or []})
    ex_rows = max((int(c.get('cluster_excluded_trades') or 0) for c in cfgs), default=0)
    if ex_names:
        out.append(f'Datasets too short to host a calendar rotation, and therefore dropped from the contrast, the counts and the effective-N floors alike (up to {ex_rows} rows on a single config): ' + ', '.join((f'`{d}`' for d in ex_names)) + '.')
    else:
        out.append('Every dataset spans enough calendar time to host a rotation, so no rows were dropped from any cluster contrast.')
    out.append('')
    excluded_target = max((int(c.get('n_missing_target') or 0) for c in cfgs), default=0)
    out.append(f"Horizon truncation: up to {excluded_target} rows on a single config had fewer than the horizon's bars left in their window slice and were excluded from BOTH targets, so the primary and continuity columns always describe one pool.")
    out.append('')
    out.append('## Measured detection limit, in BOTH directions')
    out.append('')
    out.append('The smallest per-trade effect each pool could have detected WHICHEVER WAY IT POINTS, under the two-sided cluster null at the rank-1 Benjamini-Hochberg threshold. The separation column is that same contrast measured on the SAME rows. `Resolvable?` compares the MAGNITUDE of the separation with the limit — which is a legitimate comparison here and was not in #1424, for the reason the validity-gate section gives.')
    out.append('')
    out.append("**Separations still carry their SIGN, and the sign still decides what a finding MEANS.** A separation is `mean(kept) - mean(suppressed)`. A POSITIVE value means the kept trades did better, the direction the gate's hypothesis claims. A NEGATIVE value means the SUPPRESSED trades did better — under this study's symmetric test that is a detectable finding rather than a blind spot, and it says a gate built on the hypothesis would have HURT. The `Largest separation` column reports the largest MAGNITUDE with its own sign attached, so neither the size nor the direction is hidden.")
    out.append('')
    by_pool = mde.get('observed_separation_by_pool') or {}
    out.append('| Pool | Rows | BH denominator | 2-sided cluster MDE (eff) | 2-sided free MDE (eff) | Largest separation ON THAT POOL (eff) | By family (signed) | Resolvable? | 2-sided cluster p at zero injection |')
    out.append('|------|-----:|---------------:|--------------------------:|-----------------------:|--------------------------------------:|--------------------|:-----------:|--------------------------------:|')
    for key, label, denom in (('1410', '#1410 design (its 30-hypothesis grid)', 30), ('primary', 'this study, primary cohort', PRIMARY_FAMILY_SIZE), ('exploratory', 'this study, exploratory grid', 30)):
        c = mde.get(f'pooled_{key}_cluster')
        f = mde.get(f'pooled_{key}_free')
        seps = by_pool.get(key) or {}
        largest = _largest_magnitude_signed(seps)
        if largest is None or c is None:
            resolvable = '-'
        else:
            resolvable = 'yes' if abs(largest) >= c else 'NO'
        out.append(f"| {label} | {mde.get(f'pooled_{key}_n', 0)} | {denom} | {('> ' + f'{MDE_EFF_GRID_MAX:g}' if c is None else _fmt(c, 3))} | {('> ' + f'{MDE_EFF_GRID_MAX:g}' if f is None else _fmt(f, 3))} | {_fmt_signed(largest, 3)} | {_fmt_family_seps(seps, 3)} | {resolvable} | {_fmt_p(mde.get(f'pooled_{key}_cluster_p0'))} |")
    out.append('')
    out.append("The same three pools on the CONTINUITY target (percentage points of net return), on #1422's grid so the studies stay comparable:")
    out.append('')
    by_pool_pp = mde.get('observed_separation_pp_by_pool') or {}
    out.append('| Pool | 2-sided cluster MDE (pp/trade) | Largest separation ON THAT POOL (pp/trade) | By family (signed) | Resolvable? | 2-sided cluster p at zero injection |')
    out.append('|------|-------------------------------:|------------------------------------------:|--------------------|:-----------:|--------------------------------:|')
    for key, label in (('1410', '#1410 design (its 30-hypothesis grid)'), ('primary', 'this study, primary cohort'), ('exploratory', 'this study, exploratory grid')):
        c = mde.get(f'pooled_{key}_cluster_return')
        seps = by_pool_pp.get(key) or {}
        largest = _largest_magnitude_signed(seps)
        if largest is None or c is None:
            resolvable = '-'
        else:
            resolvable = 'yes' if abs(largest) >= c else 'NO'
        out.append(f"| {label} | {('> ' + f'{MDE_PP_GRID_MAX:g}' if c is None else _fmt(c))} | {_fmt_signed(largest)} | {_fmt_family_seps(seps)} | {resolvable} | {_fmt_p(mde.get(f'pooled_{key}_cluster_return_p0'))} |")
    out.append('')
    out.append(f"**The numbers the validity gate and the verdict actually read** are neither of the pooled rows above. Both are evaluated on the CONFIRMATORY family (`{PRIMARY_FAMILY}`) alone, on that family's own rows. The pooled primary limit spans BOTH families and would resolve a smaller effect purely because it holds more trades, so reading it against one family's separation would make the gate easier to pass than its own row-matched rule allows.")
    out.append('')
    fam_lim = mde.get('by_family_cluster') or {}
    fam_sep = mde.get('by_family_separation') or {}
    fam_lim_ret = mde.get('by_family_cluster_return') or {}
    fam_sep_ret = mde.get('by_family_separation_return') or {}
    fam_p0 = mde.get('by_family_cluster_p0') or {}
    fam_n = mde.get('by_family_n') or {}
    out.append('| Family | Rows | 2-sided cluster MDE (eff) | Separation (eff, signed) | 2-sided p at zero injection | 2-sided cluster MDE (pp) | Separation (pp, signed) | Reads the gate? |')
    out.append('|--------|-----:|--------------------------:|-------------------------:|----------------------------:|-------------------------:|------------------------:|:---------------:|')
    for family in FAMILIES:
        lim = fam_lim.get(family)
        lim_ret = fam_lim_ret.get(family)
        out.append(f"| `{family}` | {fam_n.get(family, 0)} | {('> ' + f'{MDE_EFF_GRID_MAX:g}' if lim is None else _fmt(lim, 3))} | {_fmt_signed(fam_sep.get(family), 3)} | {_fmt_p(fam_p0.get(family))} | {('> ' + f'{MDE_PP_GRID_MAX:g}' if lim_ret is None else _fmt(lim_ret))} | {_fmt_signed(fam_sep_ret.get(family))} | {('YES' if family == PRIMARY_FAMILY else 'no')} |")
    out.append('')
    out.append('## Part A - outcomes bucketed by H at entry')
    out.append('')
    out.append('Ungated legs only, pooled per family across datasets and windows and deduplicated on `(strategy, symbol, timeframe, entry_date)` with the symbol venue-qualified. Drawdown here is TRADE-GRANULAR (the compounded trade sequence), not the bar-level engine drawdown used in Part B. `Mean efficiency` is the PRIMARY TARGET — the column the validity gate adjudicates.')
    out.append('')
    out.append(study1410.render_nan_bucket_note(run.get('warmup')))
    out.append('')
    for family in FAMILIES:
        out.append(f'### {family}')
        out.append('')
        for hw in hurst_windows:
            out.append(f'**Hurst window {hw} bars**')
            out.append('')
            out.extend(_render_bucket_table((payload['buckets'].get(family) or {}).get(str(hw)) or {}))
    out.append('## Part B / C - the pinned hypothesis')
    out.append('')
    out.append("`gate` rows are real Backtester re-runs with entry signals masked while the gate is disarmed (closes never masked); their drawdowns are bar-level. `size` rows re-compound the same ungated trade sequence with the size multiplier; their drawdowns are trade-granular. Never compare a `gate` drawdown to a `size` drawdown. `dd` and `chop` are MAGNITUDE deltas (arm minus ungated) averaged over that window's legs — negative means improvement. Every p column is TWO-SIDED. The `#1424 rule` column shows whether the config would have passed that study's acceptance rule; under Option 2 it cannot win here either way.")
    out.append('')
    primary = [c for c in cfgs if c['cohort'] == COHORT_PRIMARY]
    out.extend(_render_config_table(primary, PRIMARY_PROTOCOL_WINDOWS))
    out.append("## Part B / C - exploratory grid (#1410's configs, expanded data)")
    out.append('')
    out.append('Reported for completeness under its OWN Benjamini-Hochberg correction. Under Option 2 the whole study is exploratory, so this grid differs from the one above only in its denominator and its hypothesis count.')
    out.append('')
    exploratory = [c for c in cfgs if c['cohort'] == COHORT_EXPLORATORY]
    out.extend(_render_config_table(exploratory, EXPLORATORY_PROTOCOL_WINDOWS))
    out.append('## Part D - joint ADX x Hurst buckets (#1412 Stage 0)')
    out.append('')
    out.append("The question #1412's Stage 0 asks: do joint buckets separate beyond what either metric alone predicts — specifically, is high-ADX + low-Hurst materially worse for momentum-style entries than high-ADX alone? Scored ONE-SIDED on NET RETURN, inherited from #1422 verbatim. This is the study's single disclosed exception to two-sidedness, kept so the verdict recorded against #1412 stays comparable with the one already on the record. It sits outside the confirmatory path.")
    out.append('')
    joint = payload.get('joint') or {}
    for family in FAMILIES:
        entry = joint.get(family) or {}
        out.append(f'### {family}')
        out.append('')
        if not entry:
            out.append('No trades pooled for this family.')
            out.append('')
            continue
        out.extend(_render_joint_table(entry.get('table') or {}))
        v = entry.get('verdict') or {}
        if v.get('separated'):
            out.append(f"**Separation found.** Within `ADX >= {ADX_SPLIT:g}`, `H < 0.45` entries underperform `H >= 0.45` entries by {_fmt(v.get('delta_mean_pp'))} pp per trade (one-sided cluster p={_fmt_p(v.get('p_cluster'))}, Bonferroni bar {JOINT_ALPHA:g}; detection limit {_fmt(v.get('mde_pp'))} pp).")
        else:
            out.append(f"**{NO_JOINT_SEPARATION}** — {v.get('reason') or 'no contrast'}.")
        out.append('')
    all_sep = [(joint.get(f) or {}).get('verdict', {}).get('separated') for f in FAMILIES]
    if any(all_sep):
        out.append("At least one family separates on the joint buckets on this run. That is Stage 0's own one-sided question and it is reported here for continuity; it is not this study's verdict.")
    else:
        out.append(f'`{NO_JOINT_SEPARATION}` on every family, re-discharging Stage 0 on this pool exactly as #1424 did. #1412 stays closed.')
    out.append('')
    out.append('## What this study cannot say')
    out.append('')
    out.append('Under Option 2 the honest boundary has to be printed, not implied:')
    out.append('')
    out.append('1. It cannot CONFIRM anything. The sign was seen before the design was fixed, and it is what motivated the hypothesis. Even a significant result is hypothesis-generating.')
    out.append('2. It cannot recommend a threshold, a gate or a size rule. No code path here promotes a configuration.')
    out.append("3. It cannot claim more independent information than #1424 had. The pool is the same tape, so the LIMITS are of the same order — a two-sided test costs power against a one-sided one at the same alpha, so this study's limit can only be at or above #1424's.")
    out.append('4. It does not supersede #1424 as the live evidence, and it never writes the contract path.')
    out.append('')
    out.append('What it CAN say, and what #1424 could not: whether the observed contrast is distinguishable from chance IN EITHER DIRECTION, and — when the validity gate passes — a bound that holds both ways.')
    out.append('')
    out.append('## Run summary')
    out.append('')
    out.append(f"- Legs scored: {run['legs']} ungated + {run['gated_arms']} gated arms.")
    out.append(f"- Harness identity: {run['mirror_verified_legs']} of {run['legs']} ungated legs reproduced `eval_windows.run_leg` exactly.")
    out.append('- Pooled deduplicated trades: ' + '; '.join((f"{f} {run['pooled_trades'][f]} (primary {run['pooled_primary'][f]}, exploratory {run['pooled_exploratory'][f]})" for f in FAMILIES)) + '.')
    out.append(f"- Hypotheses: {run['n_primary_configs']} primary, {run['n_exploratory_configs']} exploratory; Benjamini-Hochberg-significant on the two-sided cluster p: {run['n_primary_significant']} primary, {run['n_exploratory_significant']} exploratory.")
    warm = run.get('warmup') or {}
    out.append(f"- Warm-up lead before each dataset's own earliest scored window: min {warm.get('min_lead_bars', 0)} bars, required {warm.get('required_bars', 0)} — {('sufficient on every dataset' if warm.get('sufficient') else 'SHORT on ' + ', '.join(warm.get('insufficient_datasets') or []))}.")
    out.append(f"- Wall time: {run['elapsed_sec']} s.")
    out.append('')
    out.append(render_recommendation(decision, mde, cfgs))
    return '\n'.join(out).rstrip() + '\n'

def report_from_payload(payload: dict) -> str:
    return render_report(payload)

def _parse_datasets(raw: Optional[str]) -> list:
    return study1424._parse_datasets(raw)

def _parse_windows(raw: Optional[str]) -> list:
    return study1424._parse_windows(raw)

def inference_deviations(args) -> list:
    out = []
    if args.n_perm != N_PERM:
        out.append(f'--n-perm {args.n_perm} (pre-registered {N_PERM})')
    if args.n_perm_mde != N_PERM_MDE:
        out.append(f'--n-perm-mde {args.n_perm_mde} (pre-registered {N_PERM_MDE})')
    if args.seed != SEED:
        out.append(f'--seed {args.seed} (pre-registered {SEED})')
    if args.no_mirror_check:
        out.append('--no-mirror-check (the pre-registered design verifies every leg against eval_windows.run_leg)')
    return out

def main(argv: Optional[Sequence[str]]=None) -> int:
    p = argparse.ArgumentParser()
    p.add_argument('--jobs', type=int, default=4, help='worker threads')
    p.add_argument('--out-dir', default=None, help='optional dir for the rolling-Hurst npz cache')
    p.add_argument('--only', default=None, help=f"comma-separated families to run ({', '.join(FAMILIES)})")
    p.add_argument('--windows', default=None, help='comma-separated window names')
    p.add_argument('--datasets', default=None, help='comma-separated [EXCHANGE=]SYMBOL:TIMEFRAME')
    p.add_argument('--hurst-windows', default=None, help='comma-separated rolling Hurst window lengths')
    p.add_argument('--n-perm', type=int, default=N_PERM)
    p.add_argument('--n-perm-mde', type=int, default=N_PERM_MDE)
    p.add_argument('--seed', type=int, default=SEED)
    p.add_argument('--json-out', default=_DEFAULT_JSON_OUT)
    p.add_argument('--report-out', default=_DEFAULT_REPORT_OUT)
    p.add_argument('--write-report', action='store_true', help='render the Markdown report')
    p.add_argument('--no-mirror-check', action='store_true', help='skip the per-leg eval_windows.run_leg identity check')
    p.add_argument('--skip-fetch', action='store_true', help='run on the cache as-is; the coverage audit decides which cells exist')
    p.add_argument('--fetch-only', action='store_true', help='backfill history and exit')
    p.add_argument('--render-only', action='store_true', help='re-render the report from an existing --json-out; runs no backtests')
    args = p.parse_args(argv)
    if os.path.abspath(args.report_out) == os.path.abspath(_CONTRACT_REPORT_OUT):
        raise SystemExit(f"[1426] this study is EXPLORATORY-ONLY (Option 2) and DEFERS the live-evidence contract path {CONTRACT_REPORT_BASENAME}; hurst_1424_gate_resolution.py owns it. Its own render belongs at {_DEFAULT_REPORT_OUT}. The supersede clause remains available to {' and '.join(('#' + str(n) for n in SIBLING_DEFERRAL))}.")
    scope = {'only': args.only, 'datasets': args.datasets, 'windows': args.windows, 'hurst_windows': args.hurst_windows}
    scope['complete'] = not any((v for v in scope.values()))
    deviations = inference_deviations(args)
    scope['pre_registered_inference'] = not deviations
    if (not scope['complete'] or deviations) and (not args.fetch_only):
        narrowed = ', '.join([f"--{k.replace('_', '-')} {v}" for k, v in scope.items() if k not in ('complete', 'pre_registered_inference') and v] + deviations)
        kind = 'a scoped run' if not scope['complete'] else 'a run that deviates from the pre-registered design'
        if os.path.abspath(args.json_out) == os.path.abspath(_DEFAULT_JSON_OUT):
            raise SystemExit(f'[1426] refusing to overwrite the committed aggregate {_DEFAULT_JSON_OUT} from {kind} ({narrowed}). Pass an explicit --json-out.')
        if os.path.abspath(args.report_out) == os.path.abspath(_DEFAULT_REPORT_OUT):
            raise SystemExit(f'[1426] refusing to target the committed report {_DEFAULT_REPORT_OUT} from {kind} ({narrowed}). Pass an explicit --report-out.')
    if args.render_only:
        with open(args.json_out) as fh:
            payload = json.load(fh)
        is_committed = os.path.abspath(args.report_out) == os.path.abspath(_DEFAULT_REPORT_OUT)
        if is_committed:
            stamp = (payload.get('run_summary') or {}).get('scope') or {}
            if not stamp.get('complete'):
                raise SystemExit(f'[1426] {args.json_out} is not stamped as a complete run, so it may not be rendered to the committed report {_DEFAULT_REPORT_OUT}.')
            if not stamp.get('pre_registered_inference'):
                raise SystemExit(f'[1426] {args.json_out} is not stamped as having run the pre-registered inference settings and verification, so it may not be rendered to the committed report {_DEFAULT_REPORT_OUT}.')
            if not args.write_report:
                raise SystemExit('[1426] writing the committed report needs --write-report, on --render-only exactly as on a scoring run.')
        report = report_from_payload(payload)
        with open(args.report_out, 'w') as fh:
            fh.write(report)
        print(f'[1426] re-rendered {args.report_out} from {args.json_out}')
        return 0
    datasets = _parse_datasets(args.datasets)
    if args.fetch_only:
        ensure_min_history(datasets)
        print('[1426] backfill complete')
        return 0
    families = FAMILIES
    if args.only:
        wanted = [t.strip() for t in args.only.split(',') if t.strip()]
        for f in wanted:
            if f not in FAMILIES:
                raise SystemExit(f'unknown family {f!r}; known: {list(FAMILIES)}')
        families = tuple((f for f in FAMILIES if f in wanted))
    window_names = _parse_windows(args.windows)
    hurst_windows = tuple((int(t) for t in args.hurst_windows.split(','))) if args.hurst_windows else HURST_WINDOWS
    resolved = resolve_primary_config_id(_JSON_1410)
    if resolved != PRIMARY_CONFIG_ID:
        raise SystemExit(f'pinned hypothesis {PRIMARY_CONFIG_ID!r} no longer matches the committed #1410 argmin {resolved!r}. Re-pin deliberately; never let it drift.')
    started = time.time()
    backfill = {}
    if not args.skip_fetch:
        print(f'[1426] backfilling {len(datasets)} datasets...')
        backfill = ensure_min_history(datasets)
    from data_fetcher import load_cached_data
    from registry_loader import load_registry
    reg = load_registry('spot')
    print(f'[1426] loading {len(datasets)} datasets from the venue caches...')
    frames = {}
    for dataset in datasets:
        exchange_id, symbol, timeframe = dataset
        try:
            frames[dataset] = load_cached_data(symbol, timeframe, exchange_id=exchange_id)
        except Exception as exc:
            print(f'[1426] load FAILED for {exchange_id} {dataset_key(symbol, timeframe)}: {exc}')
            frames[dataset] = pd.DataFrame()
    coverage = coverage_audit(frames, window_names, hurst_windows)
    print(f"[1426] coverage: {coverage['n_kept']}/{coverage['n_cells']} owned cells kept, {coverage['n_dropped']} dropped, {coverage['n_unowned']} not owned")
    for d in coverage['dropped']:
        print(f"[1426]   dropped {d['dataset']} {d['window']}: {d['reason']}")

    def _cell_ok(dataset, window):
        exchange_id, symbol, timeframe = dataset
        key = dataset_key(qualified_symbol(exchange_id, symbol), timeframe)
        return bool(coverage['cells'].get(f'{key}|{window}'))
    usable_datasets = [ds for ds in datasets if any((_cell_ok(ds, w) for w in window_names))]
    if not usable_datasets:
        raise SystemExit('[1426] no dataset carries a scoreable cell; nothing to do')
    scored_windows = [w for w in window_names if any((_cell_ok(ds, w) for ds in usable_datasets))]
    first_needed_by_ds = {}
    for ds in usable_datasets:
        own = [w for w in scored_windows if _cell_ok(ds, w)]
        first_needed_by_ds[ds] = min((pd.Timestamp(WINDOWS[w][0]) for w in own))
    warmup = warmup_audit(scored_warmup_leads(frames, coverage, scored_windows), hurst_windows)
    if not warmup['sufficient']:
        print(f"[1426] WARNING: warm-up shortfall on {len(warmup['insufficient_datasets'])} dataset(s): {', '.join(warmup['insufficient_datasets'])}. H is UNDEFINED on their first scored bars, so the NaN bucket carries real trades. NaN stays its own bucket (never 0.5) and holds the gate state.")
    else:
        print(f"[1426] warm-up OK: min lead {warmup['min_lead_bars']} bars before each dataset's own earliest scored window (need {warmup['required_bars']}).")
    print(f'[1426] computing rolling Hurst for {len(usable_datasets)}x{len(hurst_windows)} (dataset, window) pairs...')
    hurst: dict = {}
    cache_path = None
    if args.out_dir:
        os.makedirs(args.out_dir, exist_ok=True)
        cache_path = os.path.join(args.out_dir, 'hurst_1426_rolling.npz')
    cached = {}
    if cache_path and os.path.exists(cache_path):
        with np.load(cache_path, allow_pickle=False) as z:
            cached = {k: z[k] for k in z.files}

    def _hurst_key(dataset, hw):
        exchange_id, symbol, timeframe = dataset
        return f'{exchange_id}|{symbol}|{timeframe}|{hw}'

    def _hurst_job(job):
        dataset, hw = job
        key = _hurst_key(dataset, hw)
        frame = frames[dataset]
        first_needed = first_needed_by_ds[dataset]
        if key in cached and cache_entry_is_usable(cached.get(f'meta|{key}'), frame.index, first_needed):
            return (job, pd.Series(cached[key], index=frame.index))
        return (job, rolling_hurst(frame['close'], hw, first_needed=first_needed))
    jobs = [(ds, hw) for ds in usable_datasets for hw in hurst_windows]
    with ThreadPoolExecutor(max_workers=max(1, args.jobs)) as pool:
        for job, series in pool.map(_hurst_job, jobs):
            hurst[job] = series
    if cache_path:
        arrays = {}
        for ds, hw in jobs:
            key = _hurst_key(ds, hw)
            arrays[key] = hurst[ds, hw].to_numpy(dtype=float)
            arrays[f'meta|{key}'] = cache_meta(frames[ds].index, first_needed_by_ds[ds])
        np.savez_compressed(cache_path, **arrays)
    print(f'[1426] computing entry-ADX stamps for {len(usable_datasets)} datasets...')
    adx_stamps = {ds: adx_entry_stamp(frames[ds]) for ds in usable_datasets}
    print('[1426] computing symbol daily-return correlations...')
    rho_by_symbol = symbol_return_correlations({ds: frames[ds] for ds in usable_datasets})
    units = [(family, exemplar, ds, wname) for family in families for exemplar in FAMILY_EXEMPLARS[family] for ds in usable_datasets for wname in scored_windows if _cell_ok(ds, wname)]
    print(f'[1426] scoring {len(units)} legs ({len(hurst_windows) * 3} gated arms each)...')

    def _leg_job(unit):
        family, exemplar, ds, wname = unit
        by_window = {hw: hurst[ds, hw] for hw in hurst_windows}
        return build_leg(reg, family, exemplar, ds, wname, frames[ds], by_window, adx_stamps[ds], verify_mirror=not args.no_mirror_check)
    with ThreadPoolExecutor(max_workers=max(1, args.jobs)) as pool:
        legs = [lg for lg in pool.map(_leg_job, units) if lg is not None]
    legs.sort(key=lambda lg: (lg['family'], lg['strategy'], lg['dataset'], lg['window']))
    pooled = {}
    raw_counts = {}
    for family in families:
        rows = [t for lg in legs if lg['family'] == family for t in lg['trades']]
        raw_counts[family] = len(rows)
        pooled[family] = dedup_entries(rows, WINDOW_ORDER)
    for family in FAMILIES:
        pooled.setdefault(family, [])
        raw_counts.setdefault(family, 0)
    for family in FAMILIES:
        for t in pooled[family]:
            if t['cohort'] != COHORT_PRIMARY:
                continue
            key = (dataset_key(t['symbol'], t['timeframe']), t['window'])
            if key in D_1410:
                raise AssertionError(f'primary cohort leaked a #1410 cell: {key}')
    print('[1426] sweeping configs and running BOTH two-sided nulls on both targets...')
    configs = build_configs(legs, pooled, hurst_windows, rho_by_symbol, args.n_perm, args.seed)
    configs = [c for c in configs if c['family'] in families]
    apply_bh_by_cohort(configs, alpha=ALPHA)
    print('[1426] measuring two-sided detection limits...')
    mde = measure_detection_limits(pooled, hurst_windows, args.n_perm_mde, args.seed)
    decision = decide_recommendation(configs, mde)
    buckets = {family: {str(hw): bucket_tables(pooled[family], hw) for hw in hurst_windows} for family in FAMILIES}
    joint_hw = max(hurst_windows)
    joint = {}
    for family in FAMILIES:
        joint[family] = {'table': joint_adx_hurst_table(pooled[family], joint_hw), 'verdict': joint_separation_verdict(pooled[family], joint_hw, n_perm=args.n_perm, seed=args.seed)}
    n_primary = sum((1 for c in configs if c['cohort'] == COHORT_PRIMARY))
    n_expl = sum((1 for c in configs if c['cohort'] == COHORT_EXPLORATORY))
    payload = {'schema_version': SCHEMA_VERSION, 'issue': ISSUE, 'pre_registered': {'families': {f: list(FAMILY_EXEMPLARS[f]) for f in FAMILIES}, 'family_sense': dict(FAMILY_SENSE), 'exemplar_close_overrides': EXEMPLAR_CLOSE_OVERRIDES, 'buckets': list(BUCKETS), 'hurst_windows': list(hurst_windows), 'gate_pairs': {f: [list(p) for p in GATE_PAIRS[f]] for f in FAMILIES}, 'gate_initial_armed': GATE_INITIAL_ARMED, 'sizing': {'gains': list(SIZING_GAINS), 'clamp_lo': SIZING_CLAMP_LO, 'clamp_hi': SIZING_CLAMP_HI, 'nan_multiplier': SIZING_NAN_MULTIPLIER}, 'two_sided': TWO_SIDED, 'two_sided_p_definition': TWO_SIDED_P_DEFINITION, 'cohort_option': COHORT_OPTION, 'cohort_decision_statement': COHORT_DECISION_STATEMENT, 'contract_path_claimed': CONTRACT_PATH_CLAIMED, 'contract_path_statement': CONTRACT_PATH_STATEMENT, 'sibling_deferral': list(SIBLING_DEFERRAL), 'stage0_one_sided_exception': 'the joint ADX x Hurst Stage 0 verdict is inherited from #1422 one-sided on net return, so the verdict recorded against #1412 stays comparable; it sits outside the confirmatory path', 'primary_config_id': PRIMARY_CONFIG_ID, 'primary_config_ids': list(PRIMARY_CONFIG_IDS), 'primary_family_size': PRIMARY_FAMILY_SIZE, 'primary_target': PRIMARY_TARGET, 'continuity_target': CONTINUITY_TARGET, 'horizon_hours': HORIZON_HOURS, 'interim_look_disclosure': INTERIM_LOOK_DISCLOSURE, 'key_risk_prediction': KEY_RISK_PREDICTION, 'feasibility_probes': [dict(pr) for pr in FEASIBILITY_PROBES], 'window_owner': dict(WINDOW_OWNER), 'dataset_windows': {f'{ex}={dataset_key(sym, tf)}': list(ws) for (ex, sym, tf), ws in sorted(DATASET_WINDOWS.items())}, 'history_since': dict(HISTORY_SINCE), 'dataset_history_since': {f'{ex}={dataset_key(sym, tf)}': since for (ex, sym, tf), since in sorted(DATASET_HISTORY_SINCE.items())}, 'fetch_page_limit': dict(FETCH_PAGE_LIMIT), 'min_suppressed_effective': MIN_SUPPRESSED_EFFECTIVE, 'min_kept_effective': MIN_KEPT_EFFECTIVE, 'return_tolerance_pp': RETURN_TOLERANCE_PP, 'return_tolerance_frac': RETURN_TOLERANCE_FRAC, 'held_out_min_fraction': HELD_OUT_MIN_FRACTION, 'held_out_min_windows': HELD_OUT_MIN_WINDOWS, 'alpha': ALPHA, 'n_perm': args.n_perm, 'n_perm_mde': args.n_perm_mde, 'seed': args.seed, 'min_offset_days': MIN_OFFSET_DAYS, 'adx_period': ADX_PERIOD, 'adx_split': ADX_SPLIT, 'mde_eff_grid': [MDE_EFF_GRID_STEP, MDE_EFF_GRID_MAX, MDE_EFF_REFINE_STEP], 'mde_pp_grid': [MDE_PP_GRID_STEP, MDE_PP_GRID_MAX, MDE_PP_REFINE_STEP], 'windows': {k: list(WINDOWS[k]) for k in scored_windows}, 'primary_protocol_windows': list(PRIMARY_PROTOCOL_WINDOWS), 'primary_protocol_min_windows': PRIMARY_PROTOCOL_MIN_WINDOWS, 'primary_held_out_windows': list(PRIMARY_HELD_OUT_WINDOWS), 'exploratory_protocol_windows': list(EXPLORATORY_PROTOCOL_WINDOWS), 'exploratory_held_out_windows': list(EXPLORATORY_HELD_OUT_WINDOWS), 'datasets': [dataset_key(qualified_symbol(ex, sym), tf) for ex, sym, tf in usable_datasets], 'fee_platform': FEE_PLATFORM, 'capital': DEFAULT_CAPITAL}, 'run_summary': {'scope': scope, 'legs': len(legs), 'gated_arms': sum((len(lg['gated']) for lg in legs)), 'mirror_verified_legs': sum((1 for lg in legs if lg['mirror_verified'])), 'raw_trades': raw_counts, 'pooled_trades': {f: len(pooled[f]) for f in FAMILIES}, 'pooled_primary': {f: sum((1 for t in pooled[f] if t['cohort'] == COHORT_PRIMARY)) for f in FAMILIES}, 'pooled_exploratory': {f: sum((1 for t in pooled[f] if t['cohort'] == COHORT_EXPLORATORY)) for f in FAMILIES}, 'pooled_with_target': {f: sum((1 for t in pooled[f] if t.get('efficiency') is not None)) for f in FAMILIES}, 'n_primary_configs': n_primary, 'n_exploratory_configs': n_expl, 'n_primary_significant': sum((1 for c in configs if c['cohort'] == COHORT_PRIMARY and c.get('bh_reject'))), 'n_exploratory_significant': sum((1 for c in configs if c['cohort'] == COHORT_EXPLORATORY and c.get('bh_reject'))), 'warmup': warmup, 'coverage': coverage, 'backfill': backfill, 'symbol_correlations': {f'{a}|{b}': v for (a, b), v in sorted(rho_by_symbol.items())}, 'elapsed_sec': round(time.time() - started, 2)}, 'mde': mde, 'buckets': buckets, 'joint': joint, 'configs': configs, 'legs': [{k: v for k, v in lg.items() if k != 'trades'} for lg in legs], 'decision': decision_payload(decision)}
    with open(args.json_out, 'w') as fh:
        json.dump(payload, fh, indent=2, sort_keys=False)
        fh.write('\n')
    print(f'[1426] wrote {args.json_out}')
    payload_for_report = dict(payload)
    payload_for_report['decision'] = decision
    report = render_report(payload_for_report)
    if args.write_report:
        with open(args.report_out, 'w') as fh:
            fh.write(report)
        print(f'[1426] wrote {args.report_out}')
    else:
        print(render_recommendation(decision, mde, configs))
    return 0
if __name__ == '__main__':
    raise SystemExit(main())
