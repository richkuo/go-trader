import contextlib
import json
import os
import subprocess
import sys
from dataclasses import dataclass, fields
from typing import Any, Callable, Optional

import numpy as np
import pandas as pd
import pytest

_HERE = os.path.dirname(os.path.abspath(__file__))
_REPO = os.path.abspath(os.path.join(_HERE, "..", ".."))
_TOOLS = os.path.join(_REPO, "shared_tools")
if _TOOLS not in sys.path:
    sys.path.insert(0, _TOOLS)

import offline_manifest as om
from atr import ensure_atr_indicator
from backtester import Backtester, validate_close_capabilities
from close_registry_loader import evaluate as close_evaluate
from closed_bar import (
    FRAME_DECISION_TIME_ATTR,
    FRAME_RECEIVED_ORDER_DEFECTS_ATTR,
    FRAME_TIMESTAMP_KIND_ATTR,
    TIMESTAMP_KIND_NATIVE_CLOSE,
    TIMESTAMP_KIND_OPEN,
)
from eval_windows import expand_sweep
from optimizer import DEFAULT_PARAM_RANGES, generate_param_grid
from parity_diff import ParityConfig, compute_parity_frame, summarize
from registry_loader import load_registry
from strategy_composition import (
    StrategyReference,
    evaluate_open_close,
    finalize_decision,
    preflight_reference_params,
)

from . import test_adaptive_moving_average_integration as ama
from . import test_aroon_trend_backtest as aroon
from . import test_awesome_oscillator_backtest as ao
from . import test_bitcoin_lead_lag_integration as blag
from . import test_calendar_conditioned_entry_integration as cce
from . import test_camarilla_pivot_reversal_integration as cpr
from . import test_chande_momentum_backtest as cmo
from . import test_connors_rsi_backtest as crsi
from . import test_cumulative_volume_delta_divergence_integration as cvd
from . import test_dual_thrust_backtest as dt
from . import test_ease_of_movement_backtest as eom
from . import test_elder_force_pullback_integration as efp
from . import test_elder_ray_pullback_integration as erp
from . import test_fisher_reversal_integration as fis
from . import test_klinger_divergence_integration as kl
from . import test_macro_event_breakout_integration as meb
from . import test_mass_index_reversal_integration as mir
from . import test_money_flow_index_backtest as mfi
from . import test_on_balance_volume_backtest as obv
from . import test_regression_channel_reversion_backtest as rcr
from . import test_relative_vigor_index_backtest as rvi
from . import test_schaff_trend_cycle_integration as stc
from . import test_stochastic_momentum_index_integration as smi
from . import test_trix_momentum_integration as trix
from . import test_true_strength_index_integration as tsi
from . import test_ultimate_oscillator_backtest as uo
from . import test_volume_profile_reclaim_integration as vpr
from . import test_vortex_trend_backtest as vtx
from . import test_williams_r_backtest as wr

EDGE_SOURCES = ("unvalidated", "study_fail", "study_inconclusive")
OTHER_PRODUCTION_REFUSED = frozenset({"inventory_market_making"})
REPLACED_PER_STRATEGY_TESTS = (
    "test_batched_explicit_paper_parity_is_clean",
    "test_batched_explicit_paper_parity_matches_solo",
    "test_batched_explicit_paper_slots_match_solo",
    "test_batched_parity_admits_explicit_paper_and_matches_solo",
    "test_batched_parity_mode_admits_explicit_paper_and_matches_solo",
    "test_check_time_frames_hold_unless_the_last_bar_is_the_latest_closed_bar",
    "test_close_and_stop_owners_pass_strict_capability_validation",
    "test_close_owner_passes_strict_capability_validation",
    "test_composed_parity_covers_both_sides_with_the_close_owner",
    "test_composed_parity_covers_flat_long_and_short_contexts",
    "test_composer_emits_both_directions_and_holds_entries_while_a_position_is_open",
    "test_decision_parity_covers_both_entries_with_the_close_and_stop_owner",
    "test_decision_parity_with_the_close_and_stop_owner",
    "test_discovery_captures_are_byte_identical_and_live_output_hides_the_strategy",
    "test_entry_data_hold_keeps_the_close_owner_executable",
    "test_entry_holds_keep_the_close_owner_executable",
    "test_entry_parity_on_frozen_candles",
    "test_frozen_manifest_frame_full_series_prefix_and_bounded_window_agree",
    "test_invalid_entry_data_keeps_close_and_stop_management",
    "test_invalid_entry_data_keeps_close_and_stop_processing",
    "test_invalid_entry_frame_keeps_the_close_owner_executable",
    "test_invalid_entry_frame_keeps_the_explicit_close_owner_executable",
    "test_invalid_entry_volume_keeps_close_and_stop_processing",
    "test_invalid_parameters_are_configuration_errors",
    "test_invalid_parameters_are_explicit_errors",
    "test_invalid_parameters_are_explicit_errors_before_evaluation",
    "test_invalid_parameters_are_refused_before_evaluation",
    "test_invalid_parameters_are_refused_by_every_registry_entry_point",
    "test_invalid_parameters_are_refused_by_the_registry_and_the_evaluator",
    "test_invalid_parameters_are_rejected",
    "test_invalid_parameters_fail_validation",
    "test_invalid_parameters_raise_through_the_registry",
    "test_long_entry_fills_next_bar_open_and_time_stop_owns_exit",
    "test_non_batched_decision_parity_is_clean",
    "test_non_batched_parity_clean_on_frozen_hyperliquid_candles",
    "test_non_batched_parity_frame_matches_on_the_bounded_check_window",
    "test_non_batched_parity_is_clean",
    "test_non_batched_parity_matches_full_series",
    "test_non_batched_parity_on_frozen_hyperliquid_frame_is_clean",
    "test_non_batched_parity_runner_is_clean_on_bounded_window_and_every_prefix",
    "test_non_batched_parity_with_close_owner_and_both_directions",
    "test_parity_frame_matches_on_every_compared_bar",
    "test_raw_entry_parity_on_frozen_candles",
    "test_registered_futures_only_hidden_and_grid_fits_the_closed_window",
    "test_registered_futures_only_hidden_and_grid_fits_the_live_window",
    "test_registration_grid_and_validator",
    "test_registration_grid_validator_and_hiding",
    "test_registration_is_futures_only_bidirectional_hidden_no_edge_with_a_valid_grid",
    "test_registration_is_futures_only_bidirectional_hidden_no_edge_with_an_empty_grid",
    "test_registration_is_futures_only_bidirectional_hidden_production_refused_with_a_valid_grid",
    "test_registration_is_futures_only_bidirectional_no_edge_and_hidden",
    "test_registration_is_futures_only_bidirectional_no_edge_hidden_and_seeded",
    "test_registration_metadata_grid_and_hidden_discovery",
    "test_registration_metadata_grid_and_mirrored_validator",
    "test_short_mirror_and_no_reversal_while_open",
    "test_simulator_fills_next_open_with_explicit_exits_partials_and_no_reversal_or_scale_in",
    "test_simulator_fills_next_open_with_one_close_and_one_stop_and_no_reversal_or_scale_in",
    "test_simulator_keeps_closing_and_stopping_after_entry_inputs_fail",
    "test_strict_decision_parity_for_the_frozen_close_and_stop",
)
DEFAULT_WINDOW = "default"
SLIP = 1.0005


@contextlib.contextmanager
def _case(label):
    try:
        yield
    except AssertionError as exc:
        raise AssertionError(f"case {label}: {exc}") from exc
    except pytest.fail.Exception as exc:
        pytest.fail(f"case {label}: {exc}")
    except Exception as exc:
        exc.add_note(f"case {label}")
        raise


def _fut():
    return load_registry("futures")


def _apply(name, df, params):
    return _fut().apply_strategy(name, df, params)


def _covered_kind():
    strategies = _fut()._registry.STRATEGIES
    return frozenset(
        name for name, entry in strategies.items()
        if entry["platforms"] == ("futures",) and entry["short_entries"] is True
        and not entry["quote"]
        and entry["edge_status"] == "no_edge"
        and str(entry["edge_ref"] or "").startswith("backtest/candidates/")
    )


def _copy_refs(refs):
    return json.loads(json.dumps(refs))


def _decision_time(df, cutoff, boundary):
    out = df.copy()
    out.attrs = dict(df.attrs)
    out.attrs[FRAME_DECISION_TIME_ATTR] = {} if cutoff is None else {"cutoff_ms": cutoff, "boundary_ms": boundary}
    return out


def _undeclared(df):
    out = df.copy()
    out.attrs = {}
    return out


def _tail_keeping_attrs(df, rows):
    tail = df.iloc[-rows:].copy()
    tail.attrs = dict(df.attrs)
    return tail


def _manifest_window(manifest_path, key, rows):
    manifest = om.load_manifest(manifest_path)
    dataset = om.dataset_by_key(manifest, key)
    frame, _, coverage = om.window_frame(manifest, dataset, "test")
    assert coverage["complete"]
    return frame.iloc[:rows]


@dataclass(frozen=True)
class NotCovered:
    reason: str


NO_EARLIER_TEST = NotCovered("no per-strategy test of this contract existed before the contract suite")
SIDE_INPUT = NotCovered("entry evidence is a sealed side input; its own file covers this path")


@dataclass(frozen=True)
class Registration:
    study: str
    validator: bool
    production_refused: bool = False
    defaults: tuple = ()
    grid: Optional[Callable[[], dict]] = None
    grid_keys: Optional[frozenset] = None
    grid_fixed: Optional[dict] = None
    seed_in_grid: Optional[dict] = None
    combos: Optional[tuple] = None
    grid_values_validate: bool = False
    discovery: Optional[str] = None


@dataclass(frozen=True)
class Parity:
    label: str
    frame: Callable[[], pd.DataFrame]
    cfgs: tuple
    windows: tuple
    checks: tuple


@dataclass(frozen=True)
class Composer:
    params: dict
    close_refs: list
    long_frame: Callable[[], pd.DataFrame]
    short_frame: Callable[[], pd.DataFrame]
    entry_atr: float


@dataclass(frozen=True)
class EntryHold:
    params: dict
    close_refs: list
    base: Callable[[], pd.DataFrame]
    mutations: dict
    valid_col: str
    partial: Optional[tuple]


@dataclass(frozen=True)
class SimFills:
    params: dict
    close_refs: list
    frame: Callable[[], pd.DataFrame]
    timeframe: str
    min_signals: int
    required_reasons: frozenset
    partial_legs: bool
    skipped_signals: bool
    ignored_count: bool = False


@dataclass(frozen=True)
class SimAfterFailure:
    params: dict
    close_refs: list
    frame: Callable[[], pd.DataFrame]
    timeframe: str
    pick: tuple
    corrupt: Callable[[pd.DataFrame, int], pd.DataFrame]
    restore_open: bool = False


@dataclass(frozen=True)
class CheckTime:
    kinds: tuple
    interval: int
    decision: Callable[[str], tuple]
    apply: Callable[[pd.DataFrame], pd.DataFrame]
    runtime: Callable[..., pd.DataFrame]
    reason_col: str
    value_col: str
    records: Optional[str] = None


@dataclass(frozen=True)
class Refusal:
    kind: str
    base: Optional[dict] = None
    errors: tuple = (ValueError,)
    needle: bool = False
    frame: Optional[Callable[[], pd.DataFrame]] = None
    core: Optional[Callable[..., Any]] = None
    skip_key: Optional[str] = None
    match: Optional[str] = None


@dataclass(frozen=True)
class InvalidParams:
    items: tuple
    refusals: tuple


@dataclass(frozen=True)
class TimeStopFills:
    long: Callable[[], tuple]
    long_bar: int
    sole_long_signal: bool
    short: Callable[[], tuple]
    short_bar: int
    reverse_bar: int
    exit_bar: Optional[int]
    single_trade: bool


@dataclass(frozen=True)
class BadEntryPath:
    label: str
    run: Callable[[], dict]
    holds: tuple
    exit: tuple
    signal_hold: bool = True
    marks: tuple = ()
    side: Optional[str] = None


@dataclass(frozen=True)
class Case:
    registration: Any
    capability: Any
    parity: Any
    composer: Any
    entry_hold: Any
    sim_fills: Any
    sim_after_failure: Any
    check_time: Any
    invalid_params: Any
    time_stop_fills: Any
    bad_entry: Any


FAMILIES = tuple(f.name for f in fields(Case))


def _uncovered(*families, reason=NO_EARLIER_TEST):
    return {family: reason for family in families}


def _cases_with(family):
    return [(name, getattr(case, family)) for name, case in sorted(CASES.items())
            if not isinstance(getattr(case, family), NotCovered)]


def _open_both(f):
    return bool((f["live_open_action"] == "long").any() and (f["live_open_action"] == "short").any())


def _close_both(f):
    closing = f[f["live_close_fraction"] > 0]
    return bool((closing["live_signal"] == -1).any() and (closing["live_signal"] == 1).any())


PARITY_CHECKS = {
    "compared": lambda s, f, a: s["bars_compared"] >= a,
    "rows": lambda s, f, a: len(f) >= a,
    "rows_input_minus": lambda s, f, a: len(f) == a,
    "clean": lambda s, f, a: s["clean"] is True,
    "not_clean": lambda s, f, a: not s["clean"],
    "batch_clean": lambda s, f, a: bool(s["batch_clean"]),
    "no_mismatches": lambda s, f, a: s["mismatches"] == 0,
    "no_batch_mismatches": lambda s, f, a: s["batch_mismatches"] == 0,
    "close_parity": lambda s, f, a: s["close_parity"] == a,
    "close_parity_not": lambda s, f, a: s["close_parity"] != a,
    "eligible": lambda s, f, a: s["close_validation"]["close_eligibility"] == "eligible",
    "all_match": lambda s, f, a: bool(f["match"].all()),
    "opens_both": lambda s, f, a: _open_both(f),
    "open_set_covers_both": lambda s, f, a: set(f["live_open_action"]) >= {"long", "short"},
    "open_set_is_both": lambda s, f, a: set(f.loc[f["live_open_action"] != "none", "live_open_action"]) == {
        "long", "short"},
    "opens_any": lambda s, f, a: bool((f["live_open_action"] != "none").any()),
    "closes_both": lambda s, f, a: _close_both(f),
    "live_signals": lambda s, f, a: int((f["live_signal"] != 0).sum()) > 0,
    "live_signals_both": lambda s, f, a: bool((f["live_signal"] == 1).any() and (f["live_signal"] == -1).any()),
    "bt_signals": lambda s, f, a: int((f["bt_signal"] != 0).sum()) > 0,
    "batch_signals": lambda s, f, a: int((f["batch_signal"] != 0).sum()) > 0,
}


def _ama_core(df, **p):
    from adaptive_moving_average import adaptive_moving_average_core
    return adaptive_moving_average_core(df, **p)


def _trix_core(df, **p):
    from trix_momentum import trix_momentum_core
    return trix_momentum_core(df, **p)


def _tsi_core(df, **p):
    from true_strength_index import true_strength_index_core
    return true_strength_index_core(df, **p)


def _seed_json(path):
    with open(path) as fh:
        return json.load(fh)


def _protocol(study_dir):
    with open(os.path.join(study_dir, "protocol.json")) as fh:
        return json.load(fh)


def _set_last(df, col, value):
    out = df.copy()
    out.iloc[-1, out.columns.get_loc(col)] = value
    return out


def _volume_nan_from(df, first):
    out = df.copy()
    out.iloc[first + 1:, out.columns.get_loc("volume")] = np.nan
    return out


def _received_order_defects_from(df, first):
    out = df.copy()
    out.attrs = dict(df.attrs)
    out.attrs[FRAME_RECEIVED_ORDER_DEFECTS_ATTR] = [int(t.value // 1_000_000) for t in df.index[first + 1:]]
    return out


def _open_above_high_from(df, first):
    out = df.copy()
    out.iloc[first + 1:, out.columns.get_loc("open")] = out["high"].iloc[first + 1:] * 2
    return out


def _shifted_index_from(module):
    def corrupt(df, first):
        later = df.index[first + 1:]
        shift = later + pd.to_timedelta(np.arange(1, len(later) + 1), unit="min")
        return module._with_index(df.copy(), list(df.index[:first + 1]) + list(shift))
    return corrupt


def _camarilla_check_frame(kind):
    df = cpr._day_frame("1h", [100.0, 94.0, 95.0]).iloc[:27].copy()
    if kind == TIMESTAMP_KIND_NATIVE_CLOSE:
        df.index = df.index + pd.Timedelta(milliseconds=cpr.H - 1)
        df.attrs[FRAME_TIMESTAMP_KIND_ATTR] = kind
    hist = cpr._apply(df)
    return df, hist, len(df) - 1


def _first_long_check_frame(module, n):
    def build(kind):
        base = module._walk(n, 11)
        hist = module._apply(base, module.DEFAULTS)
        i = module._first(hist, 1)
        df = base.iloc[:i + 1].copy()
        if kind == TIMESTAMP_KIND_NATIVE_CLOSE:
            df.index = df.index + pd.Timedelta(hours=4) - pd.Timedelta(milliseconds=1)
        return df, hist, i
    return build


def _calendar_check_frame(kind):
    df = cce._calendar()
    full = cce._apply(df)
    i = int(np.flatnonzero(full["signal"].to_numpy() == 1)[-1])
    frame = df.iloc[:i + 1].copy()
    frame.attrs = dict(df.attrs)
    return frame, full, i


def _aroon_tp():
    rows = aroon._spike_up(aroon._trend(30))
    entry_px = rows[-1][3]
    rows += [(entry_px, entry_px + 0.5, entry_px - 0.5, float("nan"), 1.0)]
    rows += [(entry_px + 1, entry_px * 1.05, entry_px, entry_px * 1.04, 1.0)] * 3
    sig = ensure_atr_indicator(aroon._apply(aroon._frame(rows), {"period": 10, "strength": 70.0}))
    res = aroon._bt(stop_loss_atr_mult=None, stop_loss_pct=50.0).run(
        sig, strategy_name=aroon.NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    return {"sig": sig, "res": res, "hold": 31}


def _aroon_crash():
    crash = aroon._spike_up(aroon._trend(30))
    px = crash[-1][3]
    crash += [(px, px + 0.2, px - 0.2, px, 1.0)]
    crash += [(px - 1, px - 0.5, px * 0.5, px * 0.55, 1.0)]
    crash += [(px * 0.55, px * 0.56, px * 0.5, float("nan"), 1.0)] * 3
    sig = ensure_atr_indicator(aroon._apply(aroon._frame(crash), {"period": 10, "strength": 70.0}))
    res = aroon._bt().run(sig, strategy_name=aroon.NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    return {"sig": sig, "res": res, "hold": None}


def _time_stop_bt(factory, max_bars, **kw):
    return factory(close_strategies=[{"name": "time_stop", "params": {"max_bars": max_bars}}], **kw)


def _ao_tp():
    df = ao._swap_high_low(ao._frame([100.0] * 20 + [99.0, 101.0] + [101.5] * 6), 22)
    sig = ao._signals(df)
    res = ao._bt().run(sig, strategy_name=ao.NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    return {"sig": sig, "res": res, "hold": 22}


def _ao_crash():
    crash = ao._swap_high_low(ao._frame([100.0] * 20 + [99.0, 101.0, 101.5] + [80.0] * 4), 22)
    sig = ao._signals(crash)
    res = _time_stop_bt(ao._bt, 50).run(sig, strategy_name=ao.NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    return {"sig": sig, "res": res, "hold": 22}


CCT_NAME = "commodity_channel_trend"
CCT_BASE = [(101.0, 99.0, 100.0) if i % 2 == 0 else (102.0, 100.0, 101.0) for i in range(20)]
CCT_UP = [(102.0, 100.5, 101.5), (112.0, 102.0, 110.0)]
CCT_DOWN = [(100.5, 99.0, 99.5), (99.0, 89.0, 91.0)]
CCT_PARAMS = {"lookback": 5, "threshold": 120.0, "trend_period": 3}
CCT_CLOSE = [{"name": "time_stop", "params": {"max_bars": 20}}]
CCT_MANIFEST = os.path.join(_REPO, "backtest", "candidates", "commodity_channel_trend_1656", "study_manifest.json")


def _cct_frame(rows):
    df = pd.DataFrame(rows, columns=["high", "low", "close"], dtype=float)
    df.insert(0, "open", df["close"])
    df["volume"] = 1.0
    df.index = pd.date_range("2026-01-01", periods=len(df), freq="4h")
    return df


def _cct_signals(df, params=CCT_PARAMS):
    return ensure_atr_indicator(load_registry("futures").apply_strategy(CCT_NAME, df, params))


def _cct_bt(**kw):
    base = dict(initial_capital=1000.0, platform="hyperliquid",
                open_strategy={"name": CCT_NAME, "params": CCT_PARAMS},
                close_strategies=[{"name": "time_stop", "params": {"max_bars": 3}}],
                stop_loss_atr_mult=1.0, direction="both", comparison_mode="approximate")
    base.update(kw)
    return Backtester(**base)


def _cct_hold(px, n):
    return [(px + 1.0, px - 1.0, px)] * n


def _cct_bad_entry_signals(df, bad_rows):
    corrupt = df.copy()
    corrupt.loc[corrupt.index[-bad_rows:], "high"] = float("nan")
    sig = _cct_signals(corrupt)
    sig[["open", "high", "low", "close"]] = df[["open", "high", "low", "close"]]
    return ensure_atr_indicator(sig.drop(columns=["atr"]))


def _cct_frozen_btc(bars):
    manifest = om.load_manifest(CCT_MANIFEST)
    candles = om.load_candles(om.dataset_by_key(manifest, "BTC 4h"))
    return candles[["open", "high", "low", "close", "volume"]].iloc[-bars:]


def _cct_cfg(**kw):
    base = dict(strategy_name=CCT_NAME, registry="futures", platform="hyperliquid", symbol="BTC", timeframe="4h")
    base.update(kw)
    return ParityConfig(**base)


def _cct_tp():
    sig = _cct_bad_entry_signals(_cct_frame(CCT_BASE + CCT_UP + _cct_hold(110.5, 6)), 6)
    res = _cct_bt().run(sig, strategy_name=CCT_NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    return {"sig": sig, "res": res, "hold": len(sig) - 6}


def _cct_crash():
    crash = _cct_frame(CCT_BASE + CCT_UP + _cct_hold(110.5, 1) + [(109.5, 80.0, 81.0)] * 4)
    sig = _cct_bad_entry_signals(crash, 4)
    res = _time_stop_bt(_cct_bt, 50).run(sig, strategy_name=CCT_NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    return {"sig": sig, "res": res, "hold": len(sig) - 4}


CMF_NAME = "chaikin_money_flow_breakout"
CMF_PARAMS = {"flow_window": 3, "breakout_window": 3, "flow_threshold": 0.2}


def _cmf_frame(rows):
    df = pd.DataFrame(rows, columns=["open", "high", "low", "close", "volume"], dtype=float)
    df.index = pd.date_range("2026-01-01", periods=len(df), freq="4h")
    return df


def _cmf_flat(n, px=100.0):
    return [(px, px + 1.0, px - 1.0, px, 1.0)] * n


def _cmf_signals(df):
    return ensure_atr_indicator(load_registry("futures").apply_strategy(CMF_NAME, df, CMF_PARAMS))


def _cmf_bt(**kw):
    base = dict(initial_capital=1000.0, platform="hyperliquid",
                open_strategy={"name": CMF_NAME, "params": CMF_PARAMS},
                close_strategies=[{"name": "time_stop", "params": {"max_bars": 3}}],
                stop_loss_atr_mult=1.0, direction="both", comparison_mode="approximate")
    base.update(kw)
    return Backtester(**base)


def _cmf_tp():
    rows = _cmf_flat(20) + [(100.0, 110.0, 100.0, 110.0, 8.0)]
    rows += [(111.0, 112.5, 110.5, 112.0, float("nan"))] * 6
    sig = _cmf_signals(_cmf_frame(rows))
    res = _cmf_bt().run(sig, strategy_name=CMF_NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    return {"sig": sig, "res": res, "hold": 21}


def _cmf_crash():
    crash = _cmf_flat(20) + [(100.0, 110.0, 100.0, 110.0, 8.0)]
    crash += [(109.0, 109.5, 80.0, 81.0, float("nan"))] * 4
    sig = _cmf_signals(_cmf_frame(crash))
    res = _time_stop_bt(_cmf_bt, 50).run(sig, strategy_name=CMF_NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    return {"sig": sig, "res": res, "hold": None}


def _obv_tp():
    df = obv._frame(obv._long_closes())
    blank = df.copy()
    blank.iloc[obv.ENTRY_ROW + 1:, blank.columns.get_loc("volume")] = np.nan
    sig = ensure_atr_indicator(obv._run(blank))
    res = obv._bt().run(sig, strategy_name=obv.NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    return {"sig": sig, "res": res, "hold": obv.ENTRY_ROW + 1}


def _obv_crash():
    cdf = obv._frame(obv._long_closes(tail=[100.5, 90.0, 80.0, 79.0, 78.0]))
    cdf.iloc[obv.ENTRY_ROW + 2:, cdf.columns.get_loc("volume")] = -1.0
    sig = ensure_atr_indicator(obv._run(cdf))
    res = obv._bt().run(sig, strategy_name=obv.NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    return {"sig": sig, "res": res, "hold": obv.ENTRY_ROW + 2}


def _rcr_tp():
    rows = rcr._dip(rcr._symmetric(4), 3.0) + [100.0]
    rows += [float("nan")]
    rows += [104.0, 104.5, 105.0]
    df = rcr._frame(rows)
    df["high"] = df["high"].fillna(100.6)
    df["low"] = df["low"].fillna(99.6)
    df["open"] = df["open"].fillna(100.1)
    df.iloc[len(rows) - 3:, df.columns.get_loc("high")] = 106.0
    sig = ensure_atr_indicator(rcr._apply(df, rcr.SMALL))
    res = rcr._bt(stop_loss_atr_mult=None, stop_loss_pct=50.0).run(
        sig, strategy_name=rcr.NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    return {"sig": sig, "res": res, "hold": len(rows) - 4}


def _rcr_crash():
    crash = rcr._dip(rcr._symmetric(4), 3.0) + [100.0, 100.0, 60.0, float("nan"), float("nan")]
    cdf = rcr._frame(crash)
    cdf.iloc[-3:, cdf.columns.get_loc("low")] = 55.0
    cdf.iloc[-3:, cdf.columns.get_loc("high")] = 101.0
    cdf.iloc[-3:, cdf.columns.get_loc("open")] = 99.0
    sig = ensure_atr_indicator(rcr._apply(cdf, rcr.SMALL))
    res = rcr._bt().run(sig, strategy_name=rcr.NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    return {"sig": sig, "res": res, "hold": None}


def _rvi_tp():
    rows = rvi._doji(20) + [rvi._bar(100.0, 104.0)] + [rvi._bar(104.0, 104.5)] * 6
    df = rvi._frame(rows)
    blank = df.copy()
    blank.iloc[21:, blank.columns.get_loc("open")] = np.nan
    sig = rvi._signals(blank)
    sig["open"] = df["open"]
    res = rvi._bt().run(sig, strategy_name=rvi.NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    return {"sig": sig, "res": res, "hold": 21}


def _rvi_crash():
    crash = rvi._doji(20) + [rvi._bar(100.0, 104.0)] + [(103.0, 103.5, 80.0, 81.0)] * 4
    cdf = rvi._frame(crash)
    cdf = cdf.drop(cdf.index[22])
    sig = rvi._signals(cdf)
    res = _time_stop_bt(rvi._bt, 50).run(sig, strategy_name=rvi.NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    return {"sig": sig, "res": res, "hold": 22}


def _vtx_tp():
    rows = vtx._flat(20) + [(100.0, 103.0, 100.0, 102.0, 1.0)]
    rows += [(102.5, 103.5, 102.0, float("nan"), 1.0)] * 6
    sig = vtx._signals(vtx._frame(rows))
    res = vtx._bt().run(sig, strategy_name=vtx.NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    return {"sig": sig, "res": res, "hold": 21}


def _vtx_crash():
    crash = vtx._flat(20) + [(100.0, 103.0, 100.0, 102.0, 1.0)]
    crash += [(101.0, 101.5, 80.0, 81.0, 1.0)]
    crash += [(81.0, 81.5, 79.0, float("nan"), 1.0)] * 4
    sig = vtx._signals(vtx._frame(crash))
    res = _time_stop_bt(vtx._bt, 50).run(sig, strategy_name=vtx.NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    return {"sig": sig, "res": res, "hold": None}


def _cmo_gap(path):
    def run():
        rise = [100.0] * 30 + [100.5, 101.0, 101.4, 101.8]
        tail = [102.0, 103.5, 104.5, 105.5, 106.0] if path == "up" else [101.0, 99.0, 97.0, 95.0, 93.0]
        df = cmo._frame(rise + tail)
        df = df.drop(df.index[len(rise)])
        sig, res = cmo._run_bt(df)
        return {"sig": sig, "res": res, "hold": len(rise), "after": df.index[len(rise) - 1]}
    return run


def _dt_gap(path):
    def run():
        head = dt._base() + dt._session([101.0, 102.0, 103.5], per=3)
        tail = ([104.0, 105.5, 106.5, 107.5, 108.5, 109.0] if path == "up"
                else [102.0, 99.0, 97.0, 95.0, 93.0, 92.0])
        df = dt._frame(head + dt._session(tail, per=len(tail), anchor=103.5))
        df = df.drop(df.index[len(head)])
        sig, res = dt._run_bt(df)
        return {"sig": sig, "res": res, "hold": len(head), "after": df.index[len(head) - 1]}
    return run


def _ao_long():
    df = ao._frame([100.0] * 20 + [99.0, 101.0] + [101.5] * 8)
    return df, ao._signals(df), ao._bt()


def _ao_short():
    df = ao._frame([100.0] * 20 + [101.0, 99.0, 98.0, 97.0, 99.0] + [99.5] * 8)
    return df, ao._signals(df), _time_stop_bt(ao._bt, 6, stop_loss_atr_mult=None, stop_loss_pct=0.5)


def _cmf_long():
    rows = _cmf_flat(20) + [(100.0, 110.0, 100.0, 110.0, 8.0)]
    rows += [(111.0, 112.5, 110.5, 112.0, 1.0)] * 8
    df = _cmf_frame(rows)
    return df, _cmf_signals(df), _cmf_bt()


def _cmf_short():
    rows = _cmf_flat(20) + [(100.0, 100.0, 90.0, 90.0, 8.0)]
    rows += [(89.5, 90.0, 88.0, 89.0, 1.0)] * 2
    rows += [(89.0, 120.0, 88.0, 120.0, 30.0)]
    rows += [(119.0, 121.0, 118.0, 119.5, 1.0)] * 6
    df = _cmf_frame(rows)
    return df, _cmf_signals(df), _time_stop_bt(_cmf_bt, 6, stop_loss_atr_mult=None, stop_loss_pct=0.5)


def _cct_long():
    df = _cct_frame(CCT_BASE + CCT_UP + _cct_hold(110.5, 8))
    return df, _cct_signals(df), _cct_bt()


CCT_SWING = [(97.0, 95.0, 96.0), (98.0, 96.0, 97.0)] * 3


def _cct_short():
    rally = [(97.5, 96.0, 97.0), (107.5, 97.5, 105.5)]
    df = _cct_frame(CCT_BASE + CCT_DOWN + _cct_hold(91.0, 8) + CCT_SWING + rally + _cct_hold(106.0, 6))
    return df, _cct_signals(df), _time_stop_bt(_cct_bt, 40, stop_loss_atr_mult=None, stop_loss_pct=0.5)


def _rvi_long():
    df = rvi._frame(rvi._doji(20) + [rvi._bar(100.0, 104.0)] + [rvi._bar(104.0, 104.5)] * 8)
    return df, rvi._signals(df), rvi._bt()


def _rvi_short():
    ablation = {"period": 3, "zero_line_filter": False}
    df = rvi._frame(rvi._doji(20) + [rvi._bar(100.0, 96.0), rvi._bar(96.0, 110.0)] + rvi._doji(10, 110.0))
    bt = _time_stop_bt(rvi._bt, 6, open_strategy={"name": rvi.NAME, "params": ablation},
                       stop_loss_atr_mult=None, stop_loss_pct=0.5)
    return df, rvi._signals(df, ablation), bt


def _vtx_long():
    rows = vtx._flat(20) + [(100.0, 103.0, 100.0, 102.0, 1.0)]
    rows += [(102.5, 103.5, 102.0, 103.0, 1.0)] * 8
    df = vtx._frame(rows)
    return df, vtx._signals(df), vtx._bt()


def _vtx_short():
    rows = vtx._flat(20) + [(100.0, 100.0, 97.0, 98.0, 1.0)]
    rows += [(97.5, 98.0, 96.0, 97.0, 1.0)] * 2
    rows += [(97.0, 112.0, 96.5, 111.0, 1.0)]
    rows += [(111.0, 112.0, 110.0, 111.5, 1.0)] * 6
    df = vtx._frame(rows)
    return df, vtx._signals(df), _time_stop_bt(vtx._bt, 6, stop_loss_atr_mult=None, stop_loss_pct=0.5)


TIME_STOP_EXIT = (("single_prefix", "time_stop:"),)
SL_FIRST = (("first_exact", "sl"),)
WINDOWS = (200, None)
ENTRY_PARITY = (("compared", 1), ("clean", None))
COMPOSED_PARITY = (("no_mismatches", None), ("clean", None), ("opens_both", None), ("closes_both", None))
BATCHED_PARITY = (("compared", 1), ("clean", None), ("batch_clean", None))
OWNER_PARITY = (("compared", 1), ("no_mismatches", None), ("clean", None), ("eligible", None), ("opens_both", None))
OWNER_BATCHED_PARITY = OWNER_PARITY + (("no_batch_mismatches", None),)


def _p(params):
    return tuple((json.dumps(p, sort_keys=True), p) for p in params)


def _cfgs(builder, params):
    return tuple((label, (lambda p=p: builder(p))) for label, p in _p(params))


def _one(builder):
    return (("cfg", builder),)


CASES = {
    "adaptive_moving_average": Case(
        registration=Registration(
            study="adaptive_moving_average_1641", validator=True,
            defaults=(lambda: ama.SEED, lambda: _protocol(ama.STUDY)["seed_params"]),
            grid=lambda: _protocol(ama.STUDY)["grids"][ama.NAME],
            combos=("sweep", lambda: ama.SEED, 162, False, {}), discovery=ama.STUDY),
        capability=ama.CLOSE,
        parity=(
            Parity("raw_entry", lambda: ama._frozen("BTC", "4h", 420), _cfgs(ama._cfg, [ama.SEED, ama.BOUNDARY]),
                   WINDOWS, ENTRY_PARITY),
            Parity("composed", lambda: ama._frozen("SOL", "4h", 520),
                   _one(lambda: ama._cfg(ama.SMALL, close_refs=_copy_refs(ama.CLOSE), direction="both")),
                   WINDOWS, COMPOSED_PARITY),
            Parity("batched", lambda: ama._frozen("BTC", "4h", 320),
                   _one(lambda: ama._cfg(ama.SMALL, close_refs=_copy_refs(ama.CLOSE), direction="both", batched=True)),
                   WINDOWS, BATCHED_PARITY),
        ),
        composer=Composer(ama.SMALL, ama.CLOSE, lambda: ama._from_closes([100.0] * 80 + [101.0]),
                          lambda: ama._from_closes([100.0] * 80 + [99.0]), 50.0),
        entry_hold=EntryHold(ama.SMALL, ama.CLOSE, lambda: ama._walk(200, 9), {
            "volume": lambda df: (ama._set_last(df, "volume", -5.0), None),
            "geometry": lambda df: (ama._set_last(df, "open", df["high"].iloc[-1] * 2), None),
            "cadence": lambda df: (ama._with_index(df, list(df.index[:-1]) + [df.index[-1] + pd.Timedelta(hours=2)]),
                                   None),
            "insufficient": lambda df: (df.iloc[-(ama._need(ama.SMALL) - 1):], None),
        }, "ama_valid", (1.015, 0.25, None)),
        sim_fills=SimFills(ama.SMALL, ama.CLOSE, lambda: ama._walk(900, 5), "4h", 0,
                           frozenset({"sl", "tiered_tp_pct"}), True, True),
        sim_after_failure=SimAfterFailure(ama.SMALL, ama.CLOSE, lambda: ama._walk(600, 5), "4h",
                                          ("signal_after", 100), _volume_nan_from),
        invalid_params=InvalidParams(tuple(ama.INVALID_PARAMS), (
            Refusal("registry", base=ama.SEED),
            Refusal("apply", base=ama.SEED, frame=lambda: ama._walk(120, 1)),
            Refusal("core", base=ama.SEED, frame=lambda: ama._walk(120, 1), core=_ama_core),
        )),
        **_uncovered("check_time", "time_stop_fills", "bad_entry"),
    ),
    "aroon_trend": Case(
        registration=Registration(study="aroon_trend_1643", validator=True),
        parity=(
            Parity("strict_owner", lambda: aroon._market(420),
                   (("kwargs", lambda: dict(strategy_name=aroon.NAME, params={"period": 14, "strength": 50.0},
                                            registry="futures", close_refs=aroon.TP, comparison_mode="strict")),),
                   (None,), (("close_parity", "unverified"), ("all_match", None), ("open_set_covers_both", None))),
            Parity("batched", lambda: aroon._market(420),
                   _one(lambda: ParityConfig(strategy_name=aroon.NAME, params={"period": 14, "strength": 50.0},
                                             registry="futures", batched=True, symbol="BTC", timeframe="4h")),
                   (200,), (("no_batch_mismatches", None), ("all_match", None), ("open_set_covers_both", None))),
        ),
        invalid_params=InvalidParams(tuple(aroon.INVALID_PARAMS), (
            Refusal("apply", needle=True, frame=lambda: aroon._market(40)),
            Refusal("public", needle=True),
            Refusal("value", needle=True),
        )),
        bad_entry=(
            BadEntryPath("take_profit", _aroon_tp, (("aroon_reason", "nonfinite_input"),),
                         (("single_prefix", "tiered_tp_pct"),)),
            BadEntryPath("crash", _aroon_crash, (), SL_FIRST, signal_hold=False),
        ),
        **_uncovered("capability", "composer", "entry_hold", "sim_fills", "sim_after_failure", "check_time",
                     "time_stop_fills"),
    ),
    "awesome_oscillator": Case(
        registration=Registration(study="awesome_oscillator_1660", validator=False),
        parity=(
            Parity("time_stop_owner", lambda: ao._frozen_frame(), _one(lambda: ao._parity_cfg()), WINDOWS,
                   (("input_signals", 10), ("compared", 300), ("no_mismatches", None),
                    ("close_parity", "incomplete"), ("not_clean", None), ("live_signals", None))),
            Parity("batched", lambda: ao._frozen_frame(260), _one(lambda: ao._parity_cfg(batched=True)), (200,),
                   (("compared", 1), ("no_mismatches", None), ("close_parity", "incomplete"), ("not_clean", None))),
        ),
        time_stop_fills=TimeStopFills(_ao_long, 21, True, _ao_short, 21, 24, 28, True),
        bad_entry=(
            BadEntryPath("time_stop", _ao_tp, (("ao_reason", "inconsistent_hl"),), TIME_STOP_EXIT,
                         marks=((21, 1),)),
            BadEntryPath("crash", _ao_crash, (), SL_FIRST),
        ),
        **_uncovered("capability", "composer", "entry_hold", "sim_fills", "sim_after_failure", "check_time",
                     "invalid_params"),
    ),
    "bitcoin_lead_lag": Case(
        registration=Registration(study="bitcoin_lead_lag_1640", validator=True, production_refused=True,
                                  combos=("grid", None, 144, True, {"expected_interval_ms": blag.HOUR})),
        invalid_params=InvalidParams((
            {"lag_bars": 0}, {"lag_bars": 1.5}, {"lag_bars": True}, {"horizon_bars": 13},
            {"training_bars": 47}, {"training_bars": 241}, {"impulse_z": float("nan")}, {"min_r2": 1.0},
            {"expected_interval_ms": 300_000}, {"signal_mode": "other"}, {"unknown": 1}),
            (Refusal("registry", base=blag.SEED),)),
        **_uncovered("capability", "parity", "composer", "entry_hold", "sim_fills", "sim_after_failure",
                     "check_time", "time_stop_fills", "bad_entry", reason=SIDE_INPUT),
    ),
    "calendar_conditioned_entry": Case(
        registration=Registration(study="calendar_conditioned_entry_1653", validator=True),
        check_time=CheckTime((TIMESTAMP_KIND_OPEN,), cce.H1, _calendar_check_frame, lambda df: cce._apply(df),
                             lambda df, kind, c, b: _decision_time(df, c, b), "cce_reason", "cce_gross_mean"),
        invalid_params=InvalidParams(tuple(cce.INVALID_PARAMS), (
            Refusal("public"),
            Refusal("preflight", match=f"open strategy '{cce.NAME}' has invalid parameters"),
            Refusal("apply", base=cce.P, errors=(ValueError, TypeError), frame=lambda: cce._calendar(n=50)),
        )),
        **_uncovered("capability", "parity", "composer", "entry_hold", "sim_fills", "sim_after_failure",
                     "time_stop_fills", "bad_entry"),
    ),
    "camarilla_pivot_reversal": Case(
        registration=Registration(study="camarilla_pivot_reversal_1669", validator=True,
                                  defaults=(lambda: {"expected_interval_ms": cpr.H},), grid=lambda: {}),
        parity=(
            Parity("owner", lambda: cpr._walk(24 * 24, "1h", seed=41, sigma=0.012),
                   _one(lambda: cpr._cfg(batched=False)), WINDOWS, OWNER_PARITY),
            Parity("owner_batched", lambda: cpr._walk(24 * 24, "1h", seed=41, sigma=0.012),
                   _one(lambda: cpr._cfg(batched=True)), WINDOWS, OWNER_BATCHED_PARITY),
        ),
        composer=Composer({"expected_interval_ms": cpr.H}, cpr.CLOSE,
                          lambda: cpr._day_frame("1h", [100.0, 94.0, 95.0]).iloc[:27].copy(),
                          lambda: cpr._day_frame("1h", [100.0, 106.0, 105.0]).iloc[:27].copy(), 1.0),
        entry_hold=EntryHold({"expected_interval_ms": cpr.H}, cpr.CLOSE,
                             lambda: cpr._day_frame("1h", [100.0, 94.0, 95.0]).iloc[:27].copy(), {
            "prior_gap": lambda df: (cpr._drop(df, 5), None),
            "nan_close": lambda df: (cpr._set(df, 25, "close", np.nan), None),
            "undeclared": lambda df: (_undeclared(df), None),
            "stale": lambda df: (cpr._runtime(df, int(df.index[-1].value // 1_000_000) + 3 * cpr.H,
                                              int(df.index[-1].value // 1_000_000) + cpr.H), None),
            "unavailable": lambda df: (None, df),
        }, "cpr_valid", None),
        sim_fills=SimFills({"expected_interval_ms": cpr.H}, cpr.CLOSE,
                           lambda: cpr._walk(24 * 60, "1h", seed=31, sigma=0.008), "1h", 6,
                           frozenset({"sl"}), False, True, ignored_count=True),
        sim_after_failure=SimAfterFailure({"expected_interval_ms": cpr.H}, cpr.CLOSE,
                                          lambda: cpr._walk(24 * 40, "1h", seed=31, sigma=0.008), "1h",
                                          ("entry_after", 60), _received_order_defects_from),
        check_time=CheckTime((TIMESTAMP_KIND_OPEN, TIMESTAMP_KIND_NATIVE_CLOSE), cpr.H, _camarilla_check_frame,
                             lambda df: cpr._apply(df), lambda df, kind, c, b: cpr._runtime(df, c, b),
                             "cpr_reason", "cpr_s3", records="cpr"),
        **_uncovered("capability", "invalid_params", "time_stop_fills", "bad_entry"),
    ),
    "chaikin_money_flow_breakout": Case(
        registration=Registration(study="chaikin_money_flow_1649", validator=False),
        time_stop_fills=TimeStopFills(_cmf_long, 20, False, _cmf_short, 20, 23, 27, True),
        bad_entry=(
            BadEntryPath("time_stop", _cmf_tp, (("cmf_reason", "nonfinite_input"),), TIME_STOP_EXIT),
            BadEntryPath("crash", _cmf_crash, (), SL_FIRST, signal_hold=False),
        ),
        **_uncovered("capability", "parity", "composer", "entry_hold", "sim_fills", "sim_after_failure",
                     "check_time", "invalid_params"),
    ),
    "chande_momentum": Case(
        registration=Registration(study="chande_momentum_1661", validator=False),
        parity=(
            Parity("owner", lambda: cmo._market(), _one(lambda: cmo._parity_cfg()), (None, 200),
                   (("compared", 1), ("no_mismatches", None), ("clean", None), ("close_parity", "unverified"),
                    ("live_signals_both", None))),
            Parity("batched", lambda: cmo._market(300), _one(lambda: cmo._parity_cfg(batched=True)), (None, 200),
                   (("compared", 1), ("no_mismatches", None), ("no_batch_mismatches", None), ("clean", None))),
        ),
        invalid_params=InvalidParams(tuple(cmo.INVALID_PARAMS), (
            Refusal("apply", base=cmo.PARAMS, frame=lambda: cmo._frame(cmo._walk(40))),
        )),
        bad_entry=(
            BadEntryPath("up_take_profit", _cmo_gap("up"), (("cmo_reason", "cadence_gap"),),
                         (("after", None), ("first_contains", "tp"))),
            BadEntryPath("down_stop", _cmo_gap("down"), (("cmo_reason", "cadence_gap"),),
                         (("after", None), ("first_exact", "sl"))),
        ),
        **_uncovered("capability", "composer", "entry_hold", "sim_fills", "sim_after_failure", "check_time",
                     "time_stop_fills"),
    ),
    "commodity_channel_trend": Case(
        registration=Registration(study="commodity_channel_trend_1656", validator=False),
        parity=(
            Parity("entry", lambda: _cct_frozen_btc(700), _one(lambda: _cct_cfg()), WINDOWS,
                   (("compared", 500), ("clean", None), ("live_signals", None))),
            Parity("time_stop_owner", lambda: _cct_frozen_btc(600),
                   _one(lambda: _cct_cfg(close_refs=[dict(c, params=dict(c["params"])) for c in CCT_CLOSE],
                                         direction="both", comparison_mode="approximate")),
                   WINDOWS, (("compared", 400), ("no_mismatches", None), ("close_parity", "incomplete"),
                             ("not_clean", None), ("opens_both", None))),
            Parity("batched", lambda: _cct_frozen_btc(260), _one(lambda: _cct_cfg(batched=True)), (200,),
                   (("compared", 1), ("clean", None))),
        ),
        time_stop_fills=TimeStopFills(_cct_long, len(CCT_BASE) + 1, True, _cct_short, len(CCT_BASE) + 1,
                                      len(CCT_BASE) + 1 + 8 + len(CCT_SWING) + 2, None, True),
        bad_entry=(
            BadEntryPath("time_stop", _cct_tp, (("cct_reason", "nonfinite_input"),), TIME_STOP_EXIT,
                         marks=((len(CCT_BASE) + 1, 1),)),
            BadEntryPath("crash", _cct_crash, (("cct_valid", False),), SL_FIRST, signal_hold=False),
        ),
        **_uncovered("capability", "composer", "entry_hold", "sim_fills", "sim_after_failure", "check_time",
                     "invalid_params"),
    ),
    "connors_rsi_reversion": Case(
        registration=Registration(study="connors_rsi_reversion_1645", validator=False),
        parity=(
            Parity("time_stop_owner", lambda: crsi._market(360, seed=7), _one(lambda: crsi._parity_cfg()), WINDOWS,
                   (("compared", 1), ("no_mismatches", None), ("close_parity", "incomplete"), ("not_clean", None),
                    ("live_signals", None))),
            Parity("batched", lambda: crsi._market(260), _one(lambda: crsi._parity_cfg(batched=True)), (200,),
                   (("compared", 1), ("no_mismatches", None), ("close_parity", "incomplete"), ("not_clean", None))),
        ),
        **_uncovered("capability", "composer", "entry_hold", "sim_fills", "sim_after_failure", "check_time",
                     "invalid_params", "time_stop_fills", "bad_entry"),
    ),
    "cumulative_volume_delta_divergence": Case(
        registration=Registration(study="cumulative_volume_delta_divergence_1638", validator=True,
                                  production_refused=True,
                                  combos=("grid", None, 216, True, {"expected_interval_ms": cvd.BAR})),
        **_uncovered("capability", "parity", "composer", "entry_hold", "sim_fills", "sim_after_failure",
                     "check_time", "invalid_params", "time_stop_fills", "bad_entry", reason=SIDE_INPUT),
    ),
    "dual_thrust": Case(
        registration=Registration(study="dual_thrust_1642", validator=True),
        parity=(
            Parity("1h", lambda: dt._walk(24 * 14, freq="1h", seed=31, step=2.0), _one(lambda: dt._parity_cfg("1h")),
                   (None, 200), (("compared", 1), ("no_mismatches", None), ("clean", None),
                                 ("live_signals_both", None))),
            Parity("4h", lambda: dt._walk(6 * 50, freq="4h", seed=31, step=2.0), _one(lambda: dt._parity_cfg("4h")),
                   (None, 200), (("compared", 1), ("no_mismatches", None), ("clean", None),
                                 ("live_signals_both", None))),
            Parity("1h_batched", lambda: dt._walk(24 * 11, freq="1h", seed=37, step=2.0),
                   _one(lambda: dt._parity_cfg("1h", batched=True)), (None, 200),
                   (("compared", 1), ("no_mismatches", None), ("no_batch_mismatches", None), ("clean", None))),
            Parity("4h_batched", lambda: dt._walk(6 * 45, freq="4h", seed=37, step=2.0),
                   _one(lambda: dt._parity_cfg("4h", batched=True)), (None, 200),
                   (("compared", 1), ("no_mismatches", None), ("no_batch_mismatches", None), ("clean", None))),
        ),
        invalid_params=InvalidParams(tuple(dt.INVALID_PARAMS), (
            Refusal("apply", base=dt.P, frame=lambda: dt._walk(24 * 3)),
            Refusal("public", base=dt.P),
        )),
        bad_entry=(
            BadEntryPath("up_take_profit", _dt_gap("up"), (("dt_reason", "cadence_gap"),),
                         (("after", None), ("first_contains", "tp")), side="long"),
            BadEntryPath("down_stop", _dt_gap("down"), (("dt_reason", "cadence_gap"),),
                         (("after", None), ("first_exact", "sl")), side="long"),
        ),
        **_uncovered("capability", "composer", "entry_hold", "sim_fills", "sim_after_failure", "check_time",
                     "time_stop_fills"),
    ),
    "ease_of_movement_breakout": Case(
        registration=Registration(study="ease_of_movement_1671", validator=False),
        invalid_params=InvalidParams(tuple(eom.INVALID_PARAMS), (
            Refusal("apply", base=eom.PARAMS, frame=lambda: eom._frame(eom._flat(30))),
        )),
        **_uncovered("capability", "parity", "composer", "entry_hold", "sim_fills", "sim_after_failure",
                     "check_time", "time_stop_fills", "bad_entry"),
    ),
    "elder_force_pullback": Case(
        registration=Registration(study="elder_force_pullback_1668", validator=True, defaults=(lambda: efp.SEED,),
                                  grid_fixed={"volume_weighting": [1]}, seed_in_grid=efp.SEED,
                                  combos=("grid", None, 54, True, {}, ({"volume_weighting": 0},))),
        capability=efp.CLOSE,
        parity=(
            Parity("owner", lambda: efp._walk(700, seed=41), _one(lambda: efp._cfg(batched=False)), WINDOWS,
                   OWNER_PARITY),
            Parity("owner_batched", lambda: efp._walk(700, seed=41), _one(lambda: efp._cfg(batched=True)), WINDOWS,
                   OWNER_BATCHED_PARITY),
        ),
        composer=Composer(efp.SEED, efp.CLOSE, lambda: efp._frame(efp._pullback(1)).iloc[:efp.CROSS_AT + 1].copy(),
                          lambda: efp._frame(efp._pullback(-1)).iloc[:efp.CROSS_AT + 1].copy(), 1.0),
        entry_hold=EntryHold(efp.SEED, efp.CLOSE,
                             lambda: efp._frame(efp._pullback(1)).iloc[:efp.CROSS_AT + 1].copy(), {
            "gap": lambda df: (efp._with_index(df, list(df.index[:-3]) + list(df.index[-3:] + pd.Timedelta(hours=1))),
                               None),
            "nan_close": lambda df: (efp._set(df, len(df) - 4, "close", np.nan), None),
            "undeclared": lambda df: (_undeclared(df), None),
            "short_history": lambda df: (_tail_keeping_attrs(df, efp.NEED - 1), None),
            "unavailable": lambda df: (None, df),
        }, "efp_valid", None),
        sim_fills=SimFills(efp.SEED, efp.CLOSE, lambda: efp._walk(3000, seed=31), "1h", 3,
                           frozenset({"tiered_tp_pct"}), False, False),
        invalid_params=InvalidParams(tuple(efp.INVALID_PARAMS), (
            Refusal("registry", needle=True),
            Refusal("apply", base=efp.SEED, frame=lambda: efp._frame(efp._pullback()), skip_key="unknown_knob"),
        )),
        **_uncovered("sim_after_failure", "check_time", "time_stop_fills", "bad_entry"),
    ),
    "elder_ray_pullback": Case(
        registration=Registration(study="elder_ray_pullback_1648", validator=True,
                                  defaults=(lambda: _seed_json(erp.SEED)["params"], lambda: erp.DEFAULTS),
                                  grid=lambda: _seed_json(erp.SEED)["optimizer_grid"], discovery=erp.STUDY_DIR),
        parity=(
            Parity("entry", lambda: erp._frozen("BTC 4h", 320),
                   _cfgs(erp._cfg, [erp.DOMAIN_ENDPOINTS[0], erp.DOMAIN_ENDPOINTS[-1], erp.DEFAULTS]), WINDOWS,
                   ENTRY_PARITY),
            Parity("composed", lambda: erp._frozen("SOL 4h", 520),
                   _one(lambda: erp._cfg(erp.DEFAULTS, close_refs=_copy_refs(erp.CLOSE), direction="both")),
                   WINDOWS, COMPOSED_PARITY),
            Parity("batched", lambda: erp._frozen("BTC 4h", 300),
                   _one(lambda: erp._cfg(erp.DEFAULTS, close_refs=_copy_refs(erp.CLOSE), direction="both",
                                         batched=True)), WINDOWS, BATCHED_PARITY),
        ),
        composer=Composer(erp.DEFAULTS, erp.CLOSE, lambda: erp._trend_frame(154, [(150, 3)]),
                          lambda: erp._trend_frame(154, [(150, 3)], rising=False), 50.0),
        entry_hold=EntryHold(erp.P, erp.CLOSE, lambda: erp._walk(200, 9), {
            "volume": lambda df: (df.assign(volume=np.r_[df["volume"].to_numpy()[:-1], -5.0]), None),
            "geometry": lambda df: (df.assign(open=np.r_[df["open"].to_numpy()[:-1], df["high"].iloc[-1] * 2]), None),
            "cadence": lambda df: (erp._shift_index(df, len(df) - 1, pd.Timedelta(hours=2)), None),
            "warmup": lambda df: (df.iloc[-40:], None),
            "nonfinite": lambda df: (df.assign(volume=np.r_[df["volume"].to_numpy()[:-5], np.nan,
                                                            df["volume"].to_numpy()[-4:]]), None),
        }, "erp_valid", (1.015, 0.25, None)),
        sim_fills=SimFills(erp.DEFAULTS, erp.CLOSE, lambda: erp._walk(900, 5), "4h", 0,
                           frozenset({"sl", "tiered_tp_pct"}), True, False),
        sim_after_failure=SimAfterFailure(erp.DEFAULTS, erp.CLOSE, lambda: erp._walk(600, 5), "4h",
                                          ("signal_after", 120), _volume_nan_from),
        invalid_params=InvalidParams(tuple(erp.INVALID_PARAMS), (
            Refusal("registry", base=erp.DEFAULTS, errors=(ValueError, TypeError)),
            Refusal("apply", base=erp.DEFAULTS, errors=(ValueError, TypeError), frame=lambda: erp._walk(120, 1)),
        )),
        **_uncovered("capability", "check_time", "time_stop_fills", "bad_entry"),
    ),
    "fisher_reversal": Case(
        registration=Registration(study="fisher_reversal_1646", validator=False,
                                  defaults=(lambda: _seed_json(fis.SEED)["params"],
                                            lambda: {"lookback": 9, "extreme": 1.5}),
                                  grid=lambda: _seed_json(fis.SEED)["optimizer_grid"]),
        parity=(
            Parity("entry", lambda: fis._frozen("BTC 4h", 320),
                   _cfgs(fis._cfg, fis.DOMAIN_ENDPOINTS + [{"lookback": 9, "extreme": 1.5}]), WINDOWS, ENTRY_PARITY),
            Parity("composed", lambda: fis._frozen("SOL 4h", 520),
                   _one(lambda: fis._cfg({"lookback": 5, "extreme": 0.75}, close_refs=_copy_refs(fis.CLOSE),
                                         direction="both")), WINDOWS, COMPOSED_PARITY),
            Parity("batched", lambda: fis._frozen("BTC 4h", 300),
                   _one(lambda: fis._cfg({"lookback": 5, "extreme": 0.75}, close_refs=_copy_refs(fis.CLOSE),
                                         direction="both", batched=True)), WINDOWS, BATCHED_PARITY),
        ),
        composer=Composer({"lookback": 9, "extreme": 0.5}, fis.CLOSE, lambda: fis._entry_frame("long"),
                          lambda: fis._entry_frame("short"), 50.0),
        entry_hold=EntryHold(fis.P, fis.CLOSE, lambda: fis._walk(200, 9), {
            "volume": lambda df: (df.assign(volume=np.r_[df["volume"].to_numpy()[:-1], -5.0]), None),
            "geometry": lambda df: (df.assign(open=np.r_[df["open"].to_numpy()[:-1], df["high"].iloc[-1] * 2]), None),
            "cadence": lambda df: (fis._shift_index(df, len(df) - 1, pd.Timedelta(hours=2)), None),
        }, "fisher_valid", (1.015, 0.25, None)),
        sim_fills=SimFills({"lookback": 9, "extreme": 0.75}, fis.CLOSE, lambda: fis._walk(900, 5), "4h", 0,
                           frozenset({"sl", "tiered_tp_pct"}), True, False),
        sim_after_failure=SimAfterFailure({"lookback": 9, "extreme": 0.75}, fis.CLOSE, lambda: fis._walk(600, 5),
                                          "4h", ("signal_after", 100), _volume_nan_from),
        invalid_params=InvalidParams(tuple(fis.INVALID_PARAMS), (
            Refusal("apply", base={"lookback": 9, "extreme": 1.5}, frame=lambda: fis._walk(120, 1)),
        )),
        **_uncovered("capability", "check_time", "time_stop_fills", "bad_entry"),
    ),
    "klinger_divergence": Case(
        registration=Registration(study="klinger_divergence_1662", validator=True,
                                  defaults=(lambda: _seed_json(kl.SEED)["params"], lambda: kl.DEFAULTS),
                                  grid=lambda: _seed_json(kl.SEED)["optimizer_grid"],
                                  combos=("sweep", lambda: kl.DEFAULTS, 768, True, {})),
        capability=kl.CLOSE,
        parity=(
            Parity("entry", lambda: kl._frozen("BTC 4h", 460),
                   _cfgs(kl._cfg, [kl.DEFAULTS, kl.FAST, kl.DOMAIN_ENDPOINTS[3]]), WINDOWS, ENTRY_PARITY),
            Parity("composed", lambda: kl._frozen("SOL 4h", 900),
                   _one(lambda: kl._cfg(kl.FAST, close_refs=_copy_refs(kl.CLOSE), direction="both")),
                   WINDOWS, COMPOSED_PARITY),
            Parity("batched", lambda: kl._frozen("BTC 4h", 520),
                   _one(lambda: kl._cfg(kl.FAST, close_refs=_copy_refs(kl.CLOSE), direction="both", batched=True)),
                   WINDOWS, BATCHED_PARITY + (("opens_any", None),)),
        ),
        entry_hold=EntryHold(kl.DEFAULTS, kl.CLOSE, lambda: kl._walk(400, 9), {
            "volume": lambda df: (df.assign(volume=np.r_[df["volume"].to_numpy()[:-1], -5.0]), None),
            "geometry": lambda df: (df.assign(open=np.r_[df["open"].to_numpy()[:-1], df["high"].iloc[-1] * 2]), None),
            "cadence": lambda df: (kl._shift_index(df, len(df) - 1, pd.Timedelta(hours=2)), None),
            "stale": lambda df: (kl._runtime(df, TIMESTAMP_KIND_OPEN,
                                             int(df.index[-1].value // 1_000_000) + 3 * kl.FOUR_H,
                                             int(df.index[-1].value // 1_000_000) + kl.FOUR_H), None),
            "zero_cm": lambda df: (kl._flat_pair(df, len(df) - 2), None),
        }, "klinger_valid", (1.015, 0.0, 0)),
        check_time=CheckTime((TIMESTAMP_KIND_OPEN, TIMESTAMP_KIND_NATIVE_CLOSE), kl.FOUR_H,
                             _first_long_check_frame(kl, 1500), lambda df: kl._apply(df, kl.DEFAULTS), kl._runtime,
                             "klinger_reason", "klinger_oscillator"),
        invalid_params=InvalidParams(tuple(kl.INVALID_PARAMS), (
            Refusal("apply", base=kl.DEFAULTS, frame=lambda: kl._walk(300, 1)),
            Refusal("registry", base=kl.DEFAULTS),
        )),
        **_uncovered("composer", "sim_fills", "sim_after_failure", "time_stop_fills", "bad_entry"),
    ),
    "macro_event_breakout": Case(
        registration=Registration(study="macro_event_breakout_1654", validator=True, production_refused=True,
                                  combos=("grid", None, 12, True, {})),
        invalid_params=InvalidParams((
            {"confirm_bars": 7}, {"range_bars": 0}, {"max_spread_frac": 0.0}, {"confirm_bars": 3, "post_window_bars": 2},
            {"spread_gate": "yes"}, {"event_type": "cpi"}, {"calendar_path": "x.json"},
            {"calendar_sha256": "ABC"}, {"max_quote_age_ms": meb.H + 1}, {"expected_interval_ms": 300_000},
            {"unknown_key": 1}), (Refusal("registry"),)),
        **_uncovered("capability", "parity", "composer", "entry_hold", "sim_fills", "sim_after_failure",
                     "check_time", "time_stop_fills", "bad_entry", reason=SIDE_INPUT),
    ),
    "mass_index_reversal": Case(
        registration=Registration(study="mass_index_reversal_1670", validator=True, defaults=(lambda: mir.SEED,),
                                  grid_fixed={"bulge_filter": [1]}, seed_in_grid=mir.SEED,
                                  combos=("grid", None, 324, True, {})),
        capability=mir.CLOSE,
        parity=(
            Parity("owner", lambda: mir._walk(720, seed=41), _one(lambda: mir._cfg(batched=False)), WINDOWS,
                   OWNER_PARITY),
            Parity("owner_batched", lambda: mir._walk(720, seed=41), _one(lambda: mir._cfg(batched=True)), WINDOWS,
                   OWNER_BATCHED_PARITY),
        ),
        composer=Composer(mir.SEED, mir.CLOSE,
                          lambda: mir._frame(*mir._short_entry_mid(-0.05)).iloc[:mir.DOWN_AT + 1].copy(),
                          lambda: mir._frame(*mir._short_entry_mid(0.05)).iloc[:mir.DOWN_AT + 1].copy(), 1.0),
        entry_hold=EntryHold(mir.SEED, mir.CLOSE,
                             lambda: mir._frame(*mir._short_entry_mid(0.05)).iloc[:mir.DOWN_AT + 1].copy(), {
            "gap": lambda df: (mir._with_index(df, list(df.index[:-3]) + list(df.index[-3:] + pd.Timedelta(hours=1))),
                               None),
            "nan_close": lambda df: (mir._set(df, len(df) - 4, "close", np.nan), None),
            "undeclared": lambda df: (_undeclared(df), None),
            "short_history": lambda df: (_tail_keeping_attrs(df, mir.NEED - 1), None),
            "unavailable": lambda df: (None, df),
        }, "mir_valid", None),
        sim_fills=SimFills(mir.SEED, mir.CLOSE, lambda: mir._walk(2400, seed=31), "1h", 3, frozenset(), False, False),
        invalid_params=InvalidParams(tuple(mir.INVALID_PARAMS), (
            Refusal("registry", needle=True),
            Refusal("apply", base=mir.SEED, frame=lambda: mir._frame(*mir._burst()), skip_key="unknown_knob"),
        )),
        **_uncovered("sim_after_failure", "check_time", "time_stop_fills", "bad_entry"),
    ),
    "money_flow_index_reversal": Case(
        registration=Registration(study="money_flow_index_reversal_1658", validator=False,
                                  defaults=(lambda: {"lookback": 14, "oversold": 20.0, "overbought": 80.0},),
                                  grid_keys=frozenset({"lookback", "oversold", "overbought"})),
        parity=(
            Parity("entry", lambda: mfi._frozen("BTC 4h", 320), _cfgs(mfi._cfg, mfi.PARITY_PARAMS), WINDOWS,
                   ENTRY_PARITY),
            Parity("composed", lambda: mfi._frozen("SOL 4h", 420),
                   _one(lambda: mfi._cfg({"lookback": 7, "oversold": 30.0, "overbought": 70.0},
                                         close_refs=_copy_refs(mfi.CLOSE), direction="both")), WINDOWS,
                   COMPOSED_PARITY),
            Parity("batched", lambda: mfi._frozen("BTC 4h", 260),
                   _one(lambda: mfi._cfg({"lookback": 7, "oversold": 30.0, "overbought": 70.0},
                                         close_refs=_copy_refs(mfi.CLOSE), direction="both", batched=True)),
                   WINDOWS, BATCHED_PARITY),
        ),
        invalid_params=InvalidParams(tuple(mfi.INVALID_PARAMS), (
            Refusal("apply", base={"lookback": 14, "oversold": 20.0, "overbought": 80.0},
                    frame=lambda: mfi._walk(30, 1)),
        )),
        **_uncovered("capability", "composer", "entry_hold", "sim_fills", "sim_after_failure", "check_time",
                     "time_stop_fills", "bad_entry"),
    ),
    "on_balance_volume_divergence": Case(
        registration=Registration(study="on_balance_volume_divergence_1657", validator=False),
        parity=(
            Parity("strict_owner", lambda: obv._market(520),
                   _one(lambda: ParityConfig(strategy_name=obv.NAME, params={}, registry="futures",
                                             close_refs=obv.TP_CLOSE, direction="both", comparison_mode="strict")),
                   (None, 200), (("rows", 1), ("all_match", None), ("open_set_is_both", None))),
            Parity("batched", lambda: obv._market(240),
                   _one(lambda: ParityConfig(strategy_name=obv.NAME, params={}, registry="futures", batched=True,
                                             symbol="BTC", timeframe="4h")),
                   (200,), (("rows", 1), ("all_match", None), ("batch_signals", None))),
            Parity("frozen_manifest", lambda: _manifest_window(obv.STUDY_MANIFEST, "BTC 4h", 420),
                   _one(lambda: ParityConfig(strategy_name=obv.NAME, params={}, registry="futures",
                                             close_refs=obv.TP_CLOSE, direction="both", comparison_mode="strict")),
                   (None, 200), (("all_match", None), ("clean", None))),
        ),
        invalid_params=InvalidParams(tuple(obv.INVALID_PARAMS), (
            Refusal("apply", base=obv.PARAMS, needle=True, frame=lambda: obv._frame(obv._long_closes())),
        )),
        bad_entry=(
            BadEntryPath("take_profit", _obv_tp, (("obvd_reason", "invalid_volume"),),
                         (("single_exact", "tiered_tp_pct:0.02"),)),
            BadEntryPath("crash", _obv_crash, (("obvd_reason", "invalid_volume"),), SL_FIRST, signal_hold=False),
        ),
        **_uncovered("capability", "composer", "entry_hold", "sim_fills", "sim_after_failure", "check_time",
                     "time_stop_fills"),
    ),
    "open_interest_breakout": Case(
        registration=Registration(study="open_interest_breakout_1637", validator=False),
        **_uncovered("capability", "parity", "composer", "entry_hold", "sim_fills", "sim_after_failure",
                     "check_time", "invalid_params", "time_stop_fills", "bad_entry"),
    ),
    "order_book_imbalance": Case(
        registration=Registration(study="order_book_imbalance_1635", validator=True, production_refused=True),
        **_uncovered("capability", "parity", "composer", "entry_hold", "sim_fills", "sim_after_failure",
                     "check_time", "invalid_params", "time_stop_fills", "bad_entry", reason=SIDE_INPUT),
    ),
    "perpetual_premium_reversion": Case(
        registration=Registration(study="perpetual_premium_reversion_1639", validator=True, production_refused=True,
                                  combos=("grid", None, 12, True, {})),
        **_uncovered("capability", "parity", "composer", "entry_hold", "sim_fills", "sim_after_failure",
                     "check_time", "invalid_params", "time_stop_fills", "bad_entry", reason=SIDE_INPUT),
    ),
    "regression_channel_reversion": Case(
        registration=Registration(study="regression_channel_reversion_1644", validator=True,
                                  defaults=(lambda: rcr.SEED,), grid_keys=frozenset(rcr.SEED),
                                  grid_values_validate=True),
        parity=(
            Parity("strict_owner", lambda: rcr._market(420),
                   (("kwargs", lambda: dict(strategy_name=rcr.NAME, params=dict(rcr.SEED, fit_window=20,
                                                                                slope_ceiling=1.0),
                                            registry="futures", close_refs=rcr.TP, comparison_mode="strict")),),
                   (None, 200), (("rows", 101), ("all_match", None), ("open_set_covers_both", None))),
            Parity("batched", lambda: rcr._market(420),
                   _one(lambda: ParityConfig(strategy_name=rcr.NAME, params=dict(rcr.SEED, fit_window=20,
                                                                                 slope_ceiling=1.0),
                                             registry="futures", batched=True, symbol="BTC", timeframe="4h")),
                   (200,), (("no_batch_mismatches", None), ("rows", 101), ("all_match", None),
                            ("open_set_covers_both", None))),
        ),
        invalid_params=InvalidParams(tuple(rcr.INVALID_PARAMS), (
            Refusal("apply", needle=True, frame=lambda: rcr._market(60)),
            Refusal("public", needle=True),
            Refusal("value", needle=True),
        )),
        bad_entry=(
            BadEntryPath("take_profit", _rcr_tp, (("rcr_reason", "nonfinite_input"),),
                         (("single_prefix", "tiered_tp_pct"),)),
            BadEntryPath("crash", _rcr_crash, (), SL_FIRST, signal_hold=False),
        ),
        **_uncovered("capability", "composer", "entry_hold", "sim_fills", "sim_after_failure", "check_time",
                     "time_stop_fills"),
    ),
    "relative_vigor_index": Case(
        registration=Registration(study="relative_vigor_index_1666", validator=False),
        parity=(
            Parity("time_stop_owner", lambda: rvi._market(),
                   _one(lambda: ParityConfig(strategy_name=rvi.NAME, params={"period": 10, "zero_line_filter": True},
                                             registry="futures",
                                             close_refs=[{"name": "time_stop", "params": {"max_bars": 20}}],
                                             direction="both", comparison_mode="approximate")),
                   (None, 200), (("rows", 1), ("all_match", None), ("close_parity", "incomplete"),
                                 ("not_clean", None), ("live_signals", None))),
            Parity("batched", lambda: rvi._market(240),
                   _one(lambda: ParityConfig(strategy_name=rvi.NAME, params={"period": 10, "zero_line_filter": True},
                                             registry="futures", batched=True, symbol="BTC", timeframe="4h")),
                   (200,), (("rows", 1), ("all_match", None))),
            Parity("frozen_manifest", lambda: _manifest_window(rvi.STUDY_MANIFEST, "BTC 4h", 420),
                   _one(lambda: ParityConfig(strategy_name=rvi.NAME, params={"period": 10, "zero_line_filter": True},
                                             registry="futures",
                                             close_refs=[{"name": "time_stop", "params": {"max_bars": 20}}],
                                             direction="both", comparison_mode="approximate")),
                   (None, 200), (("all_match", None), ("close_parity", "incomplete"), ("not_clean", None))),
        ),
        time_stop_fills=TimeStopFills(_rvi_long, 20, False, _rvi_short, 20, 21, 27, False),
        bad_entry=(
            BadEntryPath("time_stop", _rvi_tp, (("rvi_reason", "nonfinite_input"),), TIME_STOP_EXIT),
            BadEntryPath("crash", _rvi_crash, (("rvi_reason", "cadence_gap"),), SL_FIRST, signal_hold=False),
        ),
        **_uncovered("capability", "composer", "entry_hold", "sim_fills", "sim_after_failure", "check_time",
                     "invalid_params"),
    ),
    "schaff_trend_cycle": Case(
        registration=Registration(study="schaff_trend_cycle_1651", validator=True,
                                  defaults=(lambda: _seed_json(stc.SEED)["params"], lambda: stc.DEFAULTS),
                                  grid=lambda: _seed_json(stc.SEED)["optimizer_grid"],
                                  combos=("sweep", lambda: stc.DEFAULTS, None, True, {})),
        parity=(
            Parity("entry", lambda: stc._frozen("BTC 4h", 420), _cfgs(stc._cfg, stc.DOMAIN_ENDPOINTS + [stc.DEFAULTS]),
                   WINDOWS, ENTRY_PARITY),
            Parity("composed", lambda: stc._frozen("SOL 4h", 900),
                   _one(lambda: stc._cfg(dict(stc.DEFAULTS, fast_period=12, slow_period=26, cycle_length=8),
                                         close_refs=_copy_refs(stc.CLOSE), direction="both")),
                   WINDOWS, COMPOSED_PARITY),
            Parity("batched", lambda: stc._frozen("BTC 4h", 480),
                   _one(lambda: stc._cfg(dict(stc.DEFAULTS, fast_period=12, slow_period=26, cycle_length=8),
                                         close_refs=_copy_refs(stc.CLOSE), direction="both", batched=True)),
                   WINDOWS, BATCHED_PARITY + (("opens_any", None),)),
        ),
        composer=Composer(stc.DEFAULTS, stc.CLOSE, lambda: stc._entry_frame("long"),
                          lambda: stc._entry_frame("short"), 50.0),
        entry_hold=EntryHold(stc.DEFAULTS, stc.CLOSE, lambda: stc._walk(400, 9), {
            "volume": lambda df: (df.assign(volume=np.r_[df["volume"].to_numpy()[:-1], -5.0]), None),
            "geometry": lambda df: (df.assign(open=np.r_[df["open"].to_numpy()[:-1], df["high"].iloc[-1] * 2]), None),
            "cadence": lambda df: (stc._shift_index(df, len(df) - 1, pd.Timedelta(hours=2)), None),
            "stale": lambda df: (stc._runtime(df, TIMESTAMP_KIND_OPEN,
                                              int(df.index[-1].value // 1_000_000) + 3 * stc.FOUR_H,
                                              int(df.index[-1].value // 1_000_000) + stc.FOUR_H), None),
        }, "stc_valid", (1.015, 0.25, -1)),
        sim_after_failure=SimAfterFailure(stc.DEFAULTS, stc.CLOSE, lambda: stc._walk(1200, 5), "4h",
                                          ("signal_after", 250), _volume_nan_from),
        check_time=CheckTime((TIMESTAMP_KIND_OPEN, TIMESTAMP_KIND_NATIVE_CLOSE), stc.FOUR_H,
                             _first_long_check_frame(stc, 900), lambda df: stc._apply(df, stc.DEFAULTS), stc._runtime,
                             "stc_reason", "stc_value"),
        invalid_params=InvalidParams(tuple(stc.INVALID_PARAMS), (
            Refusal("apply", base=stc.DEFAULTS, frame=lambda: stc._walk(300, 1)),
            Refusal("registry", base=stc.DEFAULTS),
        )),
        **_uncovered("capability", "sim_fills", "time_stop_fills", "bad_entry"),
    ),
    "stochastic_momentum_index": Case(
        registration=Registration(study="stochastic_momentum_index_1663", validator=True,
                                  defaults=(lambda: _seed_json(smi.SEED)["params"], lambda: smi.DEFAULTS),
                                  grid=lambda: _seed_json(smi.SEED)["optimizer_grid"],
                                  combos=("sweep", lambda: smi.DEFAULTS, 729, True, {})),
        parity=(
            Parity("entry", lambda: smi._frozen("BTC 4h", 420), _cfgs(smi._cfg, smi.DOMAIN_ENDPOINTS + [smi.DEFAULTS]),
                   WINDOWS, ENTRY_PARITY),
            Parity("composed", lambda: smi._frozen("SOL 4h", 900),
                   _one(lambda: smi._cfg(smi.DEFAULTS, close_refs=_copy_refs(smi.CLOSE), direction="both")),
                   WINDOWS, COMPOSED_PARITY),
            Parity("batched", lambda: smi._frozen("BTC 4h", 480),
                   _one(lambda: smi._cfg(smi.DEFAULTS, close_refs=_copy_refs(smi.CLOSE), direction="both",
                                         batched=True)), WINDOWS, BATCHED_PARITY + (("opens_any", None),)),
        ),
        composer=Composer(smi.DEFAULTS, smi.CLOSE, lambda: smi._entry_frame("long"),
                          lambda: smi._entry_frame("short"), 50.0),
        entry_hold=EntryHold(smi.DEFAULTS, smi.CLOSE, lambda: smi._walk(300, 9), {
            "geometry": lambda df: (smi._set_last(df, "open", df["high"].iloc[-1] * 2), None),
            "cadence": lambda df: (smi._shift_index(df, len(df) - 1, pd.Timedelta(hours=2)), None),
            "stale": lambda df: (smi._runtime(df, TIMESTAMP_KIND_OPEN,
                                              int(df.index[-1].value // 1_000_000) + 3 * smi.FOUR_H,
                                              int(df.index[-1].value // 1_000_000) + smi.FOUR_H), None),
            "insufficient": lambda df: (df.iloc[-(smi.NEED - 1):], None),
            "zero_range": lambda df: (df.assign(open=100.0, high=100.0, low=100.0, close=100.0), None),
        }, "smi_valid", (1.01, 0.0, 0)),
        sim_after_failure=SimAfterFailure(smi.DEFAULTS, smi.CLOSE, lambda: smi._walk(1200, 5), "4h",
                                          ("entry_after", 250), _open_above_high_from, restore_open=True),
        check_time=CheckTime((TIMESTAMP_KIND_OPEN, TIMESTAMP_KIND_NATIVE_CLOSE), smi.FOUR_H,
                             _first_long_check_frame(smi, 900), lambda df: smi._apply(df, smi.DEFAULTS), smi._runtime,
                             "smi_reason", "smi_value"),
        invalid_params=InvalidParams(tuple(smi.INVALID_PARAMS), (
            Refusal("apply", base=smi.DEFAULTS, frame=lambda: smi._walk(300, 1)),
            Refusal("registry", base=smi.DEFAULTS),
        )),
        **_uncovered("capability", "sim_fills", "time_stop_fills", "bad_entry"),
    ),
    "trix_momentum": Case(
        registration=Registration(study="trix_momentum_1659", validator=True,
                                  defaults=(lambda: trix.SEED, lambda: _protocol(trix.STUDY)["seed_params"]),
                                  grid=lambda: _protocol(trix.STUDY)["grids"][trix.NAME],
                                  combos=("sweep", lambda: trix.SEED, 18, True, {}), discovery=trix.STUDY),
        capability=trix._close_refs(),
        parity=(
            Parity("raw_entry", lambda: trix._frozen("BTC", "4h", 420), _cfgs(trix._cfg, [trix.SEED, trix.BOUNDARY]),
                   WINDOWS, ENTRY_PARITY),
            Parity("composed", lambda: trix._frozen("SOL", "4h", 620),
                   _one(lambda: trix._cfg(trix.SMALL, close_refs=trix._close_refs(), direction="both")),
                   WINDOWS, COMPOSED_PARITY),
            Parity("batched", lambda: trix._frozen("BTC", "4h", 320),
                   _one(lambda: trix._cfg(trix.SMALL, close_refs=trix._close_refs(), direction="both", batched=True)),
                   WINDOWS, BATCHED_PARITY),
        ),
        composer=Composer(trix.TINY, trix._close_refs(), lambda: trix._from_closes([100.0] * 20 + [101.0]),
                          lambda: trix._from_closes([100.0] * 20 + [99.0]), 50.0),
        entry_hold=EntryHold(trix.SMALL, trix._close_refs(), lambda: trix._walk(200, 9), {
            "price": lambda df: (trix._set_at(df, -5, "close", np.nan), None),
            "geometry": lambda df: (trix._set_last(df, "open", df["high"].iloc[-1] * 2), None),
            "cadence": lambda df: (trix._with_index(df, list(df.index[:-1]) + [df.index[-1] + pd.Timedelta(hours=2)]),
                                   None),
            "insufficient": lambda df: (df.iloc[-(trix._need(trix.SMALL) - 1):], None),
            "order": lambda df: (trix._with_index(df, list(df.index[:-1]) + [df.index[-3]]), None),
        }, "trix_valid", (1.015, 0.25, None)),
        sim_after_failure=SimAfterFailure(trix.SMALL, trix._close_refs(), lambda: trix._walk(800, 5), "4h",
                                          ("signal_after", 100), _shifted_index_from(trix)),
        invalid_params=InvalidParams(tuple(trix.INVALID_PARAMS), (
            Refusal("registry", base=trix.SEED),
            Refusal("apply", base=trix.SEED, frame=lambda: trix._walk(120, 1)),
            Refusal("core", base=trix.SEED, frame=lambda: trix._walk(120, 1), core=_trix_core),
        )),
        **_uncovered("sim_fills", "check_time", "time_stop_fills", "bad_entry"),
    ),
    "true_strength_index": Case(
        registration=Registration(study="true_strength_index_1667", validator=True,
                                  defaults=(lambda: tsi.SEED, lambda: _protocol(tsi.STUDY)["seed_params"]),
                                  grid=lambda: _protocol(tsi.STUDY)["grids"][tsi.NAME],
                                  combos=("sweep", lambda: tsi.SEED, 54, True, {}), discovery=tsi.STUDY),
        capability=tsi._close_refs(),
        parity=(
            Parity("raw_entry_seed", lambda: tsi._frozen("BTC", "4h", 420), _cfgs(tsi._cfg, [tsi.SEED]),
                   (200, tsi._need(tsi.SEED), None), ENTRY_PARITY),
            Parity("raw_entry_boundary", lambda: tsi._frozen("BTC", "4h", 420), _cfgs(tsi._cfg, [tsi.BOUNDARY]),
                   (200, tsi._need(tsi.BOUNDARY), None), ENTRY_PARITY),
            Parity("composed", lambda: tsi._frozen("SOL", "4h", 620),
                   _one(lambda: tsi._cfg(tsi.SMALL, close_refs=tsi._close_refs(), direction="both")),
                   WINDOWS, COMPOSED_PARITY),
            Parity("batched", lambda: tsi._frozen("BTC", "4h", 320),
                   _one(lambda: tsi._cfg(tsi.SMALL, close_refs=tsi._close_refs(), direction="both", batched=True)),
                   WINDOWS, BATCHED_PARITY),
        ),
        composer=Composer(tsi.SMALL, tsi._close_refs(), lambda: tsi._from_closes([100.0] * 20 + [99.0, 104.0]),
                          lambda: tsi._from_closes([100.0] * 20 + [101.0, 96.0]), 50.0),
        entry_hold=EntryHold(tsi.SMALL, tsi._close_refs(), lambda: tsi._walk(200, 9), {
            "price": lambda df: (tsi._set_at(df, -5, "close", np.nan), None),
            "geometry": lambda df: (tsi._set_last(df, "open", df["high"].iloc[-1] * 2), None),
            "cadence": lambda df: (tsi._with_index(df, list(df.index[:-1]) + [df.index[-1] + pd.Timedelta(hours=2)]),
                                   None),
            "insufficient": lambda df: (df.iloc[-(tsi._need(tsi.SMALL) - 1):], None),
            "order": lambda df: (tsi._with_index(df, list(df.index[:-1]) + [df.index[-3]]), None),
            "denominator": lambda df: (tsi._from_closes([200.0] * 60), None),
        }, "tsi_valid", (1.015, 0.25, None)),
        sim_fills=SimFills(tsi.SMALL, tsi._close_refs(), lambda: tsi._walk(1500, 5), "4h", 0,
                           frozenset({"sl", "tiered_tp_pct"}), True, True),
        sim_after_failure=SimAfterFailure(tsi.SMALL, tsi._close_refs(), lambda: tsi._walk(800, 5), "4h",
                                          ("signal_after", 100), _shifted_index_from(tsi)),
        invalid_params=InvalidParams(tuple(tsi.INVALID_PARAMS), (
            Refusal("registry", base=tsi.SEED),
            Refusal("apply", base=tsi.SEED, frame=lambda: tsi._walk(120, 1)),
            Refusal("core", base=tsi.SEED, frame=lambda: tsi._walk(120, 1), core=_tsi_core),
        )),
        **_uncovered("check_time", "time_stop_fills", "bad_entry"),
    ),
    "ultimate_oscillator": Case(
        registration=Registration(study="ultimate_oscillator_1664", validator=False),
        parity=(
            Parity("strict_owner", lambda: uo._market(420, 7),
                   _one(lambda: ParityConfig(strategy_name=uo.NAME, registry="futures", platform="hyperliquid",
                                             close_refs=uo.CLOSE, direction="both", batched=False,
                                             stop_kwargs={"stop_loss_atr_mult": 1.0})),
                   (None, 200), (("compared", 1), ("no_mismatches", None), ("clean", None), ("opens_any", None))),
            Parity("strict_owner_batched", lambda: uo._market(420, 7),
                   _one(lambda: ParityConfig(strategy_name=uo.NAME, registry="futures", platform="hyperliquid",
                                             close_refs=uo.CLOSE, direction="both", batched=True,
                                             stop_kwargs={"stop_loss_atr_mult": 1.0})),
                   (200,), (("compared", 1), ("no_mismatches", None), ("clean", None), ("no_batch_mismatches", None),
                            ("opens_any", None))),
        ),
        **_uncovered("capability", "composer", "entry_hold", "sim_fills", "sim_after_failure", "check_time",
                     "invalid_params", "time_stop_fills", "bad_entry"),
    ),
    "volume_profile_reclaim": Case(
        registration=Registration(study="volume_profile_reclaim_1665", validator=True, production_refused=True,
                                  combos=("grid", None, 9, True, {"expected_interval_ms": vpr.BAR})),
        invalid_params=InvalidParams((
            {"profile_bins": 1}, {"profile_bins": 513}, {"profile_bins": 2.5}, {"profile_bins": True},
            {"value_area_fraction": 0.0}, {"value_area_fraction": 1.0}, {"value_area_fraction": float("nan")},
            {"session_bounds": 1}, {"expected_interval_ms": 60_000}, {"expected_interval_ms": 14_400_000},
            {"bins": 64}), (Refusal("registry", base=vpr.SEED),)),
        **_uncovered("capability", "parity", "composer", "entry_hold", "sim_fills", "sim_after_failure",
                     "check_time", "time_stop_fills", "bad_entry", reason=SIDE_INPUT),
    ),
    "vortex_trend": Case(
        registration=Registration(study="vortex_trend_1647", validator=False),
        parity=(
            Parity("time_stop_owner", lambda: vtx._market(),
                   tuple((json.dumps(p, sort_keys=True), (lambda p=p: dict(
                       strategy_name=vtx.NAME, params=p, registry="futures",
                       close_refs=[{"name": "time_stop", "params": {"max_bars": 20}}],
                       comparison_mode="approximate")))
                         for p in (None, {"period": 28, "min_separation": 0.05})),
                   (DEFAULT_WINDOW,), (("rows_input_minus", 199), ("all_match", None), ("close_parity", "incomplete"),
                                       ("not_clean", None), ("bt_signals", None), ("open_set_covers_both", None))),
        ),
        time_stop_fills=TimeStopFills(_vtx_long, 20, False, _vtx_short, 20, 23, 27, True),
        bad_entry=(
            BadEntryPath("time_stop", _vtx_tp, (("vortex_reason", "nonfinite_input"),), TIME_STOP_EXIT),
            BadEntryPath("crash", _vtx_crash, (), SL_FIRST, signal_hold=False),
        ),
        **_uncovered("capability", "composer", "entry_hold", "sim_fills", "sim_after_failure", "check_time",
                     "invalid_params"),
    ),
    "williams_r_reversal": Case(
        registration=Registration(study="williams_r_reversal_1650", validator=True),
        parity=(
            Parity("strict_owner", lambda: wr._market(420),
                   _one(lambda: ParityConfig(strategy_name=wr.NAME,
                                             params={"lookback": 14, "oversold": -80.0, "overbought": -20.0},
                                             registry="futures", platform="hyperliquid", timeframe="4h",
                                             close_refs=wr.TP, direction="both", comparison_mode="strict")),
                   (None, 200), (("rows", 1), ("all_match", None), ("live_signals", None),
                                 ("close_parity_not", "incomplete"))),
            Parity("batched", lambda: wr._market(240),
                   tuple((f"lookback={lookback}", (lambda lookback=lookback: ParityConfig(
                       strategy_name=wr.NAME, params={"lookback": lookback, "oversold": -80.0, "overbought": -20.0},
                       registry="futures", platform="hyperliquid", batched=True, symbol="BTC", timeframe="4h")))
                         for lookback in (2, 14, 100)),
                   (200,), (("rows", 1), ("all_match", None), ("no_batch_mismatches", None))),
            Parity("frozen_manifest", lambda: _manifest_window(wr.STUDY_MANIFEST, "BTC 4h", 420),
                   _one(lambda: ParityConfig(strategy_name=wr.NAME,
                                             params={"lookback": 14, "oversold": -80.0, "overbought": -20.0},
                                             registry="futures", platform="hyperliquid", timeframe="4h",
                                             close_refs=wr.TP, direction="both", comparison_mode="strict")),
                   (None, 200), (("all_match", None),)),
        ),
        **_uncovered("capability", "composer", "entry_hold", "sim_fills", "sim_after_failure", "check_time",
                     "invalid_params", "time_stop_fills", "bad_entry"),
    ),
}


def _compose(name, params, close_refs, df, side="", ctx=None, protection_df=None):
    reg = _fut()
    evaluation = evaluate_open_close(
        reg.apply_strategy, reg.get_strategy, df, name, name, [c["name"] for c in close_refs], side,
        params=dict(params), position_ctx=ctx, close_evaluate=close_evaluate,
        close_params_by_name={c["name"]: c["params"] for c in close_refs}, protection_df=protection_df)
    return evaluation, finalize_decision(evaluation, side)


def _bt(name, params, close_refs, **kw):
    base = dict(initial_capital=1000.0, platform="hyperliquid", open_strategy={"name": name, "params": dict(params)},
                close_strategies=close_refs, stop_loss_atr_mult=1.0, direction="both")
    base.update(kw)
    return Backtester(**base)


def _names(family):
    return [name for name, _ in _cases_with(family)]


def test_case_table_matches_the_registry():
    covered = _covered_kind()
    assert sorted(set(CASES) - covered) == []
    assert sorted(covered - set(CASES)) == []


def test_every_family_is_configured_or_explicitly_not_covered():
    assert FAMILIES == ("registration", "capability", "parity", "composer", "entry_hold", "sim_fills",
                        "sim_after_failure", "check_time", "invalid_params", "time_stop_fills", "bad_entry")
    for name, case in sorted(CASES.items()):
        for family in FAMILIES:
            value = getattr(case, family)
            with _case(f"{name}-{family}"):
                assert value is not None
                if isinstance(value, NotCovered):
                    assert isinstance(value.reason, str) and value.reason.strip()
    assert all(not isinstance(case.registration, NotCovered) for case in CASES.values())


def test_production_refused_set_is_the_side_input_cases_plus_the_non_candle_capabilities():
    refused = _fut()._registry.production_refused_strategies()
    assert refused == frozenset({"cumulative_volume_delta_divergence", "bitcoin_lead_lag", "macro_event_breakout",
                                 "volume_profile_reclaim", "perpetual_premium_reversion", "order_book_imbalance",
                                 "inventory_market_making"})
    table = {name for name, case in CASES.items() if case.registration.production_refused}
    assert refused == frozenset(table) | OTHER_PRODUCTION_REFUSED


def _discovery_is_unchanged_and_hides(name, study):
    for platform in ("spot", "futures"):
        out = subprocess.run([sys.executable, os.path.join(_REPO, "shared_strategies", "open", platform,
                                                          "strategies.py"), "--list-json"],
                             capture_output=True, text=True, cwd=_REPO, check=True).stdout
        captures = []
        for phase in ("before", "after"):
            with open(os.path.join(study, f"discovery_{phase}_{platform}.json"), "rb") as fh:
                captures.append(fh.read())
        assert captures[0] == captures[1]
        assert json.loads(captures[0]) == json.loads(captures[1])
        assert name.encode() not in captures[1]
        assert name not in json.dumps(json.loads(captures[1]))
        assert name not in out


@pytest.mark.parametrize("name", sorted(CASES))
def test_registration_contract(name):
    spec = CASES[name].registration
    fut = _fut()
    registry = fut._registry
    entry = fut.STRATEGY_REGISTRY[name]
    raw = registry.STRATEGIES[name]
    for expected in spec.defaults:
        assert entry["default_params"] == expected()
    assert entry["edge_status"] == "no_edge"
    assert entry["edge_source"] in EDGE_SOURCES
    assert entry["edge_ref"] == f"backtest/candidates/{spec.study}/REPORT.md"
    assert os.path.isfile(os.path.join(_REPO, entry["edge_ref"]))
    assert raw["short_entries"] is True and raw["platforms"] == ("futures",)
    if spec.validator:
        assert callable(raw["param_validator"])
    else:
        assert raw["param_validator"] is None
    assert entry["production_refused"] is spec.production_refused
    assert (name in registry.production_refused_strategies()) is spec.production_refused
    assert name in registry.short_entry_strategies()
    assert name in registry.DISCOVERY_HIDDEN_STRATEGIES
    assert name not in fut.list_strategies()
    assert name not in load_registry("spot").STRATEGY_REGISTRY
    assert registry.PLATFORM_ORDER["futures"].count(name) == 1
    assert name not in registry.PLATFORM_ORDER["spot"]
    if spec.grid is not None:
        assert DEFAULT_PARAM_RANGES[name] == spec.grid()
    if spec.grid_keys is not None:
        assert set(DEFAULT_PARAM_RANGES[name]) == set(spec.grid_keys)
    for key, values in (spec.grid_fixed or {}).items():
        assert DEFAULT_PARAM_RANGES[name][key] == values
    for key, value in (spec.seed_in_grid or {}).items():
        assert value in DEFAULT_PARAM_RANGES[name][key], key
    if spec.combos is not None:
        style, seed, count, validate, extra = spec.combos[:5]
        also = spec.combos[5] if len(spec.combos) > 5 else ()
        grid = DEFAULT_PARAM_RANGES[name]
        if style == "sweep":
            combos = [p for _, p in expand_sweep(dict(seed()), [(k, list(v)) for k, v in grid.items()])]
        else:
            combos = generate_param_grid(grid)
        if count is not None:
            assert len(combos) == count
        if validate:
            for params in combos:
                registry.validate_params(name, {**params, **extra})
                for override in also:
                    registry.validate_params(name, dict(params, **override))
    if spec.grid_values_validate:
        for key, values in DEFAULT_PARAM_RANGES[name].items():
            for value in values:
                fut.validate_param_value(name, key, value)
    if spec.discovery is not None:
        _discovery_is_unchanged_and_hides(name, spec.discovery)


@pytest.mark.parametrize("name", _names("capability"))
def test_close_owner_capability_contract(name):
    report = validate_close_capabilities(close_refs=CASES[name].capability, comparison_mode=None,
                                         platform="hyperliquid", strategy_type="perps", phase="preflight").to_dict()
    assert report["close_eligibility"] == "eligible" and not report["approximations"] and not report["refusals"]
    assert report["incomplete_parity"] is False


def _parity_params():
    out = []
    for name in _names("parity"):
        for scenario in CASES[name].parity:
            for index, (cfg_label, _) in enumerate(scenario.cfgs):
                for window in scenario.windows:
                    out.append(pytest.param(name, scenario.label, index, window,
                                            id=f"{name}-{scenario.label}-{cfg_label}-window={window}"))
    return out


def test_parity_scenarios_are_declared_once_per_strategy():
    for name in _names("parity"):
        scenarios = CASES[name].parity
        with _case(name):
            assert scenarios and len({scenario.label for scenario in scenarios}) == len(scenarios)
            for scenario in scenarios:
                assert scenario.cfgs and scenario.windows and scenario.checks
                assert len({label for label, _ in scenario.cfgs}) == len(scenario.cfgs)
                assert len(set(scenario.windows)) == len(scenario.windows)


@pytest.mark.parametrize("name,label,cfg_index,window", _parity_params())
def test_parity_contract(name, label, cfg_index, window):
    scenario = next(s for s in CASES[name].parity if s.label == label)
    _, build = scenario.cfgs[cfg_index]
    df = scenario.frame()
    cfg = build()
    kw = {} if window == DEFAULT_WINDOW else {"window": window}
    if isinstance(cfg, dict):
        frame = compute_parity_frame(df, **cfg, **kw)
    else:
        frame = compute_parity_frame(df, cfg=cfg, **kw)
    summary = summarize(frame)
    for check, arg in scenario.checks:
        if check == "input_signals":
            ok = int((_apply(name, scenario.frame(), None)["signal"] != 0).sum()) >= arg
        elif check == "rows_input_minus":
            ok = len(frame) == len(df) - arg
        else:
            ok = PARITY_CHECKS[check](summary, frame, arg)
        assert ok, (check, arg, frame[~frame["match"]].head() if "match" in frame else summary)


@pytest.mark.parametrize("name", _names("composer"))
def test_composer_contract(name):
    spec = CASES[name].composer
    long_df, short_df = spec.long_frame(), spec.short_frame()
    _, decision = _compose(name, spec.params, spec.close_refs, long_df)
    assert (decision["open_action"], decision["signal"]) == ("long", 1)
    _, decision = _compose(name, spec.params, spec.close_refs, short_df)
    assert (decision["open_action"], decision["signal"]) == ("short", -1)
    for label, df in (("long_frame", long_df), ("short_frame", short_df)):
        mark = float(df["close"].iloc[-1])
        for side in ("long", "short"):
            with _case(f"{label}-{side}"):
                ctx = {"side": side, "avg_cost": mark, "entry_atr": spec.entry_atr, "current_quantity": 1.0,
                       "initial_quantity": 1.0}
                _, held = _compose(name, spec.params, spec.close_refs, df, side, ctx)
                assert held["signal"] == 0 and held["close_fraction"] == 0.0


@pytest.mark.parametrize("name", _names("entry_hold"))
def test_entry_hold_keeps_the_close_owner_contract(name):
    spec = CASES[name].entry_hold
    close_name = spec.close_refs[0]["name"]
    for label, mutate in spec.mutations.items():
        with _case(label):
            df, protection = mutate(spec.base())
            frame = df if df is not None else protection
            mark = float(frame["close"].dropna().iloc[-1])
            ctx = {"side": "long", "avg_cost": mark / 1.05, "entry_atr": 1.0, "current_quantity": 1.0,
                   "initial_quantity": 1.0}
            evaluation, decision = _compose(name, spec.params, spec.close_refs, df, "long", ctx, protection)
            if df is not None:
                assert evaluation.open_result_df[spec.valid_col].iloc[-1] == 0.0
                assert int(evaluation.open_result_df["signal"].iloc[-1]) == 0
            assert decision["open_action"] == "none"
            assert decision["close_strategy"] == close_name and decision["close_fraction"] == 1.0
            assert decision["signal"] == -1
            _, decision = _compose(name, spec.params, spec.close_refs, df, "short",
                                   dict(ctx, side="short", avg_cost=mark * 1.05), protection)
            assert decision["open_action"] == "none" and decision["close_fraction"] == 1.0
            assert decision["signal"] == 1
            if spec.partial is not None:
                ratio, fraction, signal = spec.partial
                _, decision = _compose(name, spec.params, spec.close_refs, df, "long",
                                       dict(ctx, avg_cost=mark / ratio), protection)
                assert decision["close_fraction"] == fraction
                if signal is not None:
                    assert decision["signal"] == signal
            _, flat = _compose(name, spec.params, spec.close_refs, df, "", None, protection)
            assert flat["signal"] == 0 and flat["open_action"] == "none"


@pytest.mark.parametrize("name", _names("sim_fills"))
def test_simulator_fill_contract(name):
    spec = CASES[name].sim_fills
    df = spec.frame()
    sig = ensure_atr_indicator(_apply(name, df, dict(spec.params)))
    if spec.min_signals:
        assert (sig["signal"] == 1).sum() >= spec.min_signals and (sig["signal"] == -1).sum() >= spec.min_signals
    bt = _bt(name, spec.params, spec.close_refs)
    res = bt.run(sig, strategy_name=name, symbol="BTC/USDT", timeframe=spec.timeframe, save=False)
    trades = res["trades"]
    close_name = spec.close_refs[0]["name"]
    assert {t["side"] for t in trades} == {"long", "short"}
    reasons = {t["exit_reason"].split(":")[0] for t in trades}
    assert reasons <= {"sl", close_name, "end_of_data"} and spec.required_reasons <= reasons
    by_entry = {}
    for t in trades:
        by_entry.setdefault(t["entry_date"], []).append(t)
    spans = []
    for entry_date, legs in by_entry.items():
        assert len({leg["side"] for leg in legs}) == 1
        pos = sig.index.get_loc(pd.Timestamp(entry_date))
        want = 1 if legs[0]["side"] == "long" else -1
        assert pos > 0 and int(sig["signal"].iloc[pos - 1]) == want
        opened = float(df["open"].iloc[pos])
        assert legs[0]["entry_price"] == pytest.approx(opened * (1 + want * bt.slippage_pct), rel=1e-12)
        spans.append((pd.Timestamp(entry_date), max(pd.Timestamp(leg["exit_date"]) for leg in legs)))
    spans.sort()
    for (_, prev_exit), (next_entry, _) in zip(spans, spans[1:]):
        assert next_entry >= prev_exit
    if spec.partial_legs:
        assert any(len(legs) > 1 for legs in by_entry.values())
    if spec.skipped_signals:
        skipped = [i for i in np.flatnonzero(sig["signal"].to_numpy() != 0)
                   if not any(pd.Timestamp(e) == sig.index[min(i + 1, len(sig) - 1)] for e in by_entry)]
        assert skipped
    if spec.ignored_count:
        assert int((sig["signal"] != 0).sum()) - len(trades) > 0


@pytest.mark.parametrize("name", _names("sim_after_failure"))
def test_simulator_after_entry_failure_contract(name):
    spec = CASES[name].sim_after_failure
    close_name = spec.close_refs[0]["name"]
    df = spec.frame()
    sig = ensure_atr_indicator(_apply(name, df, dict(spec.params)))
    kind, floor = spec.pick
    if kind == "signal_after":
        first = next(int(i) for i in np.flatnonzero(sig["signal"].to_numpy() != 0)
                     if i > floor and np.isfinite(sig["atr"].iloc[i]))
    else:
        clean = _bt(name, spec.params, spec.close_refs).run(sig, strategy_name=name, symbol="BTC/USDT",
                                                            timeframe=spec.timeframe, save=False)
        first = min(int(sig.index.get_loc(pd.Timestamp(t["entry_date"]))) for t in clean["trades"]
                    if sig.index.get_loc(pd.Timestamp(t["entry_date"])) > floor) - 1
    bad = _apply(name, spec.corrupt(df, first), dict(spec.params))
    assert (bad["signal"].iloc[:first + 1].to_numpy() == sig["signal"].iloc[:first + 1].to_numpy()).all()
    assert (bad["signal"].iloc[first + 1:] == 0).all()
    if spec.restore_open:
        bad = bad.assign(open=df["open"])
    bad = ensure_atr_indicator(bad)
    res = _bt(name, spec.params, spec.close_refs).run(bad, strategy_name=name, symbol="BTC/USDT",
                                                      timeframe=spec.timeframe, save=False)
    last_entry = max(pd.Timestamp(t["entry_date"]) for t in res["trades"])
    assert last_entry == bad.index[first + 1]
    final = [t for t in res["trades"] if pd.Timestamp(t["entry_date"]) == last_entry]
    assert {t["exit_reason"].split(":")[0] for t in final} <= {"sl", close_name}


@pytest.mark.parametrize("name", _names("check_time"))
def test_closed_bar_check_time_contract(name):
    spec = CASES[name].check_time
    step = spec.interval
    for kind in spec.kinds:
        with _case(kind):
            df, ref, i = spec.decision(kind)
            assert int(ref["signal"].iloc[i]) == 1
            last = int(df.index[-1].value // 1_000_000)
            boundary = (last - step + 1 if kind == TIMESTAMP_KIND_NATIVE_CLOSE else last) + step
            cases = [
                (boundary, boundary, 1, "entry_long"),
                (boundary + step - 1, boundary, 1, "entry_long"),
                (boundary + step, boundary, 0, "decision_bar_stale"),
                (boundary - 1, boundary, 0, "decision_bar_future"),
                (boundary + 5, boundary - step, 0, "decision_boundary_mismatch"),
                (boundary, boundary - step, 0, "decision_boundary_mismatch"),
                (None, None, 0, "decision_time_missing"),
            ]
            for cutoff, declared, signal, reason in cases:
                out = spec.apply(spec.runtime(df, kind, cutoff, declared))
                assert int(out["signal"].iloc[-1]) == signal, (cutoff, declared)
                assert out[spec.reason_col].iloc[-1] == reason, (cutoff, declared)
                assert out[spec.value_col].iloc[-1] == ref[spec.value_col].iloc[i]
                assert (out["signal"].iloc[:-1].to_numpy() == ref["signal"].iloc[:i].to_numpy()).all()
            if spec.records is not None:
                recorded = spec.apply(spec.runtime(df, kind, boundary, boundary))
                assert recorded[f"{spec.records}_decision_cutoff_ms"].iloc[-1] == boundary
                assert recorded[f"{spec.records}_decision_boundary_ms"].iloc[-1] == boundary
            malformed = spec.runtime(df, kind, boundary, boundary)
            malformed.attrs[FRAME_DECISION_TIME_ATTR] = {"cutoff_ms": float(boundary), "boundary_ms": True}
            assert spec.apply(malformed)[spec.reason_col].iloc[-1] == "decision_time_missing"


def _refuse(name, refusal, params, override):
    fut = _fut()
    if refusal.kind == "registry":
        return fut._registry.validate_params(name, params)
    if refusal.kind == "public":
        return fut.validate_params(name, params)
    if refusal.kind == "value":
        (key, value), = override.items()
        return fut.validate_param_value(name, key, value)
    if refusal.kind == "apply":
        return fut.apply_strategy(name, refusal.frame(), params)
    if refusal.kind == "core":
        return refusal.core(refusal.frame(), **params)
    if refusal.kind == "preflight":
        return preflight_reference_params([StrategyReference("open", name)], params, None, fut.get_strategy,
                                          fut.validate_params)
    raise AssertionError(f"unknown refusal kind {refusal.kind!r}")


@pytest.mark.parametrize("name", _names("invalid_params"))
def test_invalid_parameters_contract(name):
    spec = CASES[name].invalid_params
    assert spec.items and spec.refusals
    for index, item in enumerate(spec.items):
        override, needle = item if isinstance(item, tuple) else (item, None)
        with _case(f"{index}-{override}"):
            for refusal in spec.refusals:
                if refusal.skip_key is not None and refusal.skip_key in override:
                    continue
                params = dict(override) if refusal.base is None else {**refusal.base, **override}
                match = refusal.match if refusal.match is not None else (needle if refusal.needle else None)
                with pytest.raises(refusal.errors, match=match):
                    _refuse(name, refusal, params, override)


@pytest.mark.parametrize("name", _names("time_stop_fills"))
def test_time_stop_fill_contract(name):
    spec = CASES[name].time_stop_fills
    with _case("long"):
        df, sig, bt = spec.long()
        assert sig["signal"].iloc[spec.long_bar] == 1
        if spec.sole_long_signal:
            assert (sig["signal"].drop(sig.index[spec.long_bar]) == 0).all()
        trades = bt.run(sig, strategy_name=name, symbol="BTC/USDT", timeframe="4h", save=False)["trades"]
        assert len(trades) == 1
        t = trades[0]
        assert t["side"] == "long"
        assert t["entry_date"] == str(df.index[spec.long_bar + 1])
        assert t["entry_price"] == pytest.approx(df["open"].iloc[spec.long_bar + 1] * SLIP)
        assert t["exit_reason"].startswith("time_stop:")
    with _case("short"):
        df, sig, bt = spec.short()
        assert sig["signal"].iloc[spec.short_bar] == -1
        assert sig["signal"].iloc[spec.reverse_bar] == 1
        trades = bt.run(sig, strategy_name=name, symbol="BTC/USDT", timeframe="4h", save=False)["trades"]
        if spec.single_trade:
            assert len(trades) == 1
        first = trades[0]
        assert first["side"] == "short"
        assert first["entry_date"] == str(df.index[spec.short_bar + 1])
        if spec.exit_bar is not None:
            assert first["exit_reason"].startswith("time_stop:")
            assert first["exit_date"] == str(df.index[spec.exit_bar])
        assert all(t["side"] == "short" and pd.Timestamp(t["entry_date"]) >= pd.Timestamp(first["exit_date"])
                   for t in trades[1:])


def _bad_entry_path(path):
    out = path.run()
    sig, trades = out["sig"], out["res"]["trades"]
    if out["hold"] is not None:
        held = sig.iloc[out["hold"]:]
        if path.signal_hold:
            assert (held["signal"] == 0).all()
        for col, value in path.holds:
            assert (held[col] == value).all(), col
    else:
        assert not path.holds and not path.signal_hold
    for row, value in path.marks:
        assert sig["signal"].iloc[row] == value
    first = trades[0]
    if path.side is not None:
        assert first["side"] == path.side
    for kind, value in path.exit:
        if kind == "single_prefix":
            assert len(trades) == 1 and first["exit_reason"].startswith(value)
        elif kind == "single_exact":
            assert len(trades) == 1 and first["exit_reason"] == value
        elif kind == "first_exact":
            assert first["exit_reason"] == value
        elif kind == "first_contains":
            assert value in first["exit_reason"]
        elif kind == "after":
            assert pd.Timestamp(first["exit_date"]) > out["after"]
        else:
            raise AssertionError(f"unknown exit check {kind!r}")


@pytest.mark.parametrize("name", _names("bad_entry"))
def test_bad_entry_data_keeps_close_and_stop_contract(name):
    paths = CASES[name].bad_entry
    assert paths and len({path.label for path in paths}) == len(paths)
    for path in paths:
        with _case(path.label):
            _bad_entry_path(path)
