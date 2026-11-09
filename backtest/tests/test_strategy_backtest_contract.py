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
    FRAME_TIMESTAMP_KIND_ATTR,
    TIMESTAMP_KIND_NATIVE_CLOSE,
    TIMESTAMP_KIND_OPEN,
)
from eval_windows import expand_sweep
from optimizer import DEFAULT_PARAM_RANGES, generate_param_grid
from parity_diff import ParityConfig, compute_parity_frame, summarize
from registry_loader import load_registry
from strategy_composition import evaluate_open_close, finalize_decision

from . import test_adaptive_moving_average_integration as ama
from . import test_aroon_trend_backtest as aroon
from . import test_awesome_oscillator_backtest as ao
from . import test_bitcoin_lead_lag_integration as blag
from . import test_calendar_conditioned_entry_integration as cce
from . import test_camarilla_pivot_reversal_integration as cpr
from . import test_chande_momentum_backtest as cmo
from . import test_connors_rsi_backtest as crsi
from . import test_cumulative_volume_delta_divergence_integration as cvd
from . import test_sweep_absorption_ifvg_integration as sai
from . import test_dual_thrust_backtest as dt
from . import test_elder_force_pullback_integration as efp
from . import test_elder_ray_pullback_integration as erp
from . import test_fisher_reversal_integration as fis
from . import test_klinger_divergence_integration as kl
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


COVERED_PLATFORM_SETS = (("futures",), ("spot", "futures"))


def _covered_kind():
    strategies = _fut()._registry.STRATEGIES
    return frozenset(
        name for name, entry in strategies.items()
        if entry["platforms"] in COVERED_PLATFORM_SETS and entry["short_entries"] is True
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
    platforms: tuple = ("futures",)
    catalog: Optional[str] = None


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
class SpotRoute:
    params: dict
    close_refs: list
    long_frame: Callable[[], pd.DataFrame]
    short_frame: Callable[[], pd.DataFrame]
    walk: Callable[[], pd.DataFrame]
    defect_frame: Callable[[], pd.DataFrame]
    entry_atr: float
    reason_col: str
    suppressed_reason: str


FUTURES_ONLY = NotCovered("futures-only registration: no spot evaluator exists")


@dataclass(frozen=True)
class Case:
    registration: Any
    capability: Any
    parity: Any
    composer: Any
    sim_fills: Any
    check_time: Any
    time_stop_fills: Any
    spot_route: Any = FUTURES_ONLY


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
    "no_short_opens": lambda s, f, a: not bool((f["live_open_action"] == "short").any()
                                               or (f["bt_open_action"] == "short").any()),
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


def _seed_json(path):
    with open(path) as fh:
        return json.load(fh)


def _protocol(study_dir):
    with open(os.path.join(study_dir, "protocol.json")) as fh:
        return json.load(fh)


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


def _time_stop_bt(factory, max_bars, **kw):
    return factory(close_strategies=[{"name": "time_stop", "params": {"max_bars": max_bars}}], **kw)


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


def _cct_frozen_btc(bars):
    manifest = om.load_manifest(CCT_MANIFEST)
    candles = om.load_candles(om.dataset_by_key(manifest, "BTC 4h"))
    return candles[["open", "high", "low", "close", "volume"]].iloc[-bars:]


def _cct_cfg(**kw):
    base = dict(strategy_name=CCT_NAME, registry="futures", platform="hyperliquid", symbol="BTC", timeframe="4h")
    base.update(kw)
    return ParityConfig(**base)


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


VSB_NAME = "volume_spike_breakout"
VSB_STUDY = os.path.join(_REPO, "backtest", "candidates", "volume_spike_breakout_2008")
VSB_DEFAULTS = {"lookback": 20, "vol_period": 20, "vol_multiplier": 2.0, "min_body_frac": 0.0}
VSB_SMALL = {"lookback": 5, "vol_period": 5, "vol_multiplier": 2.0, "min_body_frac": 0.0}
VSB_MAX = {"lookback": 100, "vol_period": 100, "vol_multiplier": 1.5, "min_body_frac": 0.25}
VSB_CLOSE = [{"name": "tiered_tp_pct", "params": {"tp_tiers": [{"profit_pct": 0.02, "close_fraction": 1.0}]}}]


def _vsb_walk(n, seed=11, freq="4h", spike_every=7):
    rng = np.random.default_rng(seed)
    close = 100.0 * np.exp(np.cumsum(rng.normal(0.0, 0.012, n)))
    open_ = np.concatenate([[100.0], close[:-1]])
    wick = rng.uniform(0.0005, 0.008, (n, 2))
    volume = rng.uniform(50.0, 150.0, n)
    volume[::spike_every] *= rng.uniform(2.5, 5.0, len(volume[::spike_every]))
    df = pd.DataFrame({"open": open_, "high": np.maximum(open_, close) * (1 + wick[:, 0]),
                       "low": np.minimum(open_, close) * (1 - wick[:, 1]), "close": close, "volume": volume})
    df.index = pd.date_range("2026-01-01", periods=n, freq=freq)
    return df


def _vsb_event(side, n=30, tail=0):
    rows = [(100.0, 100.5, 99.5, 100.0, 10.0)] * (n - 1)
    px = 100.0 + 8.0 * side
    rows.append((100.0, max(100.0, px) + 0.5, min(100.0, px) - 0.5, px, 50.0))
    rows += [(px, px + 0.5, px - 0.5, px, 10.0)] * tail
    df = pd.DataFrame(rows, columns=["open", "high", "low", "close", "volume"], dtype=float)
    df.index = pd.date_range("2026-01-01", periods=len(df), freq="4h")
    return df


def _vsb_defect():
    df = _vsb_event(1)
    df.iloc[-1, df.columns.get_loc("volume")] = 0.0
    return df


def _vsb_cfg(registry, params=VSB_SMALL, **kw):
    fut = registry == "futures"
    base = dict(strategy_name=VSB_NAME, params=dict(params), registry=registry,
                platform="hyperliquid" if fut else "binanceus", symbol="BTC" if fut else "BTC/USDT",
                timeframe="4h", close_refs=_copy_refs(VSB_CLOSE), direction="both" if fut else None,
                stop_kwargs={"stop_loss_atr_mult": 1.0}, comparison_mode="strict",
                strategy_type="perps" if fut else "spot")
    base.update(kw)
    return ParityConfig(**base)


def _vsb_bt(**kw):
    base = dict(initial_capital=1000.0, platform="hyperliquid",
                open_strategy={"name": VSB_NAME, "params": dict(VSB_SMALL)},
                close_strategies=[{"name": "time_stop", "params": {"max_bars": 3}}],
                stop_loss_atr_mult=1.0, direction="both", comparison_mode="approximate")
    base.update(kw)
    return Backtester(**base)


def _vsb_signals(df, params=VSB_SMALL):
    return ensure_atr_indicator(_apply(VSB_NAME, df, params))


def _vsb_long():
    df = _vsb_event(1, n=21, tail=8)
    return df, _vsb_signals(df), _vsb_bt()


def _vsb_short():
    df = _vsb_event(-1, n=21, tail=2)
    rows = [df, pd.DataFrame([[92.0, 120.5, 91.5, 120.0, 400.0]], columns=df.columns),
            pd.DataFrame([[120.0, 120.5, 119.5, 120.0, 10.0]] * 6, columns=df.columns)]
    out = pd.concat(rows, ignore_index=True)
    out.index = pd.date_range("2026-01-01", periods=len(out), freq="4h")
    return out, _vsb_signals(out), _time_stop_bt(_vsb_bt, 6, stop_loss_atr_mult=None, stop_loss_pct=0.5)


WINDOWS = (200,)
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
            Parity("raw_entry", lambda: ama._frozen("BTC", "4h", 420), _cfgs(ama._cfg, [ama.SEED]),
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
        sim_fills=SimFills(ama.SMALL, ama.CLOSE, lambda: ama._walk(900, 5), "4h", 0,
                           frozenset({"sl", "tiered_tp_pct"}), True, True),
        **_uncovered("check_time", "time_stop_fills"),
    ),
    "aroon_trend": Case(
        registration=Registration(study="aroon_trend_1643", validator=True),
        parity=(
            Parity("strict_owner", lambda: aroon._market(420),
                   (("kwargs", lambda: dict(strategy_name=aroon.NAME, params={"period": 14, "strength": 50.0},
                                            registry="futures", close_refs=aroon.TP, comparison_mode="strict")),),
                   WINDOWS, (("close_parity", "unverified"), ("all_match", None), ("open_set_covers_both", None))),
            Parity("batched", lambda: aroon._market(420),
                   _one(lambda: ParityConfig(strategy_name=aroon.NAME, params={"period": 14, "strength": 50.0},
                                             registry="futures", batched=True, symbol="BTC", timeframe="4h")),
                   (200,), (("no_batch_mismatches", None), ("all_match", None), ("open_set_covers_both", None))),
        ),
        **_uncovered("capability", "composer", "sim_fills", "check_time", "time_stop_fills"),
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
        **_uncovered("capability", "composer", "sim_fills", "check_time"),
    ),
    "bitcoin_lead_lag": Case(
        registration=Registration(study="bitcoin_lead_lag_1640", validator=True, production_refused=True,
                                  combos=("grid", None, 144, True, {"expected_interval_ms": blag.HOUR})),
        **_uncovered("capability", "parity", "composer", "sim_fills", "check_time", "time_stop_fills",
                     reason=SIDE_INPUT),
    ),
    "calendar_conditioned_entry": Case(
        registration=Registration(study="calendar_conditioned_entry_1653", validator=True),
        check_time=CheckTime((TIMESTAMP_KIND_OPEN,), cce.H1, _calendar_check_frame, lambda df: cce._apply(df),
                             lambda df, kind, c, b: _decision_time(df, c, b), "cce_reason", "cce_gross_mean"),
        **_uncovered("capability", "parity", "composer", "sim_fills", "time_stop_fills"),
    ),
    "camarilla_pivot_reversal": Case(
        registration=Registration(study="camarilla_pivot_reversal_1669", validator=True,
                                  defaults=(lambda: {"expected_interval_ms": cpr.H},), grid=lambda: {}),
        parity=(
            Parity("owner_batched", lambda: cpr._walk(24 * 18, "1h", seed=41, sigma=0.012),
                   _one(lambda: cpr._cfg(batched=True)), WINDOWS, OWNER_BATCHED_PARITY),
        ),
        composer=Composer({"expected_interval_ms": cpr.H}, cpr.CLOSE,
                          lambda: cpr._day_frame("1h", [100.0, 94.0, 95.0]).iloc[:27].copy(),
                          lambda: cpr._day_frame("1h", [100.0, 106.0, 105.0]).iloc[:27].copy(), 1.0),
        sim_fills=SimFills({"expected_interval_ms": cpr.H}, cpr.CLOSE,
                           lambda: cpr._walk(24 * 60, "1h", seed=31, sigma=0.008), "1h", 6,
                           frozenset({"sl"}), False, True, ignored_count=True),
        check_time=CheckTime((TIMESTAMP_KIND_OPEN, TIMESTAMP_KIND_NATIVE_CLOSE), cpr.H, _camarilla_check_frame,
                             lambda df: cpr._apply(df), lambda df, kind, c, b: cpr._runtime(df, c, b),
                             "cpr_reason", "cpr_s3", records="cpr"),
        **_uncovered("capability", "time_stop_fills"),
    ),
    "chaikin_money_flow_breakout": Case(
        registration=Registration(study="chaikin_money_flow_1649", validator=False),
        time_stop_fills=TimeStopFills(_cmf_long, 20, False, _cmf_short, 20, 23, 27, True),
        **_uncovered("capability", "parity", "composer", "sim_fills", "check_time"),
    ),
    "chande_momentum": Case(
        registration=Registration(study="chande_momentum_1661", validator=False),
        parity=(
            Parity("owner", lambda: cmo._market(), _one(lambda: cmo._parity_cfg()), WINDOWS,
                   (("compared", 1), ("no_mismatches", None), ("clean", None), ("close_parity", "unverified"),
                    ("live_signals_both", None))),
            Parity("batched", lambda: cmo._market(300), _one(lambda: cmo._parity_cfg(batched=True)), WINDOWS,
                   (("compared", 1), ("no_mismatches", None), ("no_batch_mismatches", None), ("clean", None))),
        ),
        **_uncovered("capability", "composer", "sim_fills", "check_time", "time_stop_fills"),
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
        **_uncovered("capability", "composer", "sim_fills", "check_time"),
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
        **_uncovered("capability", "composer", "sim_fills", "check_time", "time_stop_fills"),
    ),
    "cumulative_volume_delta_divergence": Case(
        registration=Registration(study="cumulative_volume_delta_divergence_1638", validator=True,
                                  production_refused=True,
                                  combos=("grid", None, 216, True, {"expected_interval_ms": cvd.BAR})),
        **_uncovered("capability", "parity", "composer", "sim_fills", "check_time", "time_stop_fills",
                     reason=SIDE_INPUT),
    ),
    "sweep_absorption_ifvg": Case(
        registration=Registration(study="sweep_absorption_ifvg_2010", validator=True, production_refused=True,
                                  defaults=(lambda: sai.DEFAULT_PARAMS,),
                                  combos=("grid", None, 72, True, {"expected_interval_ms": 300_000},
                                          ({"expected_interval_ms": 900_000}, {"absorption_filter": False})),
                                  discovery=sai.STUDY_DIR, catalog=os.path.join(sai.STUDY_DIR, "evidence")),
        **_uncovered("capability", "parity", "composer", "sim_fills", "check_time", "time_stop_fills",
                     reason=SIDE_INPUT),
    ),
    "dual_thrust": Case(
        registration=Registration(study="dual_thrust_1642", validator=True),
        parity=(
            Parity("1h", lambda: dt._walk(24 * 14, freq="1h", seed=31, step=2.0), _one(lambda: dt._parity_cfg("1h")),
                   WINDOWS, (("compared", 1), ("no_mismatches", None), ("clean", None),
                             ("live_signals_both", None))),
            Parity("4h_batched", lambda: dt._walk(6 * 45, freq="4h", seed=37, step=2.0),
                   _one(lambda: dt._parity_cfg("4h", batched=True)), WINDOWS,
                   (("compared", 1), ("no_mismatches", None), ("no_batch_mismatches", None), ("clean", None))),
        ),
        **_uncovered("capability", "composer", "sim_fills", "check_time", "time_stop_fills"),
    ),
    "ease_of_movement_breakout": Case(
        registration=Registration(study="ease_of_movement_1671", validator=False),
        **_uncovered("capability", "parity", "composer", "sim_fills", "check_time", "time_stop_fills"),
    ),
    "elder_force_pullback": Case(
        registration=Registration(study="elder_force_pullback_1668", validator=True, defaults=(lambda: efp.SEED,),
                                  grid_fixed={"volume_weighting": [1]}, seed_in_grid=efp.SEED,
                                  combos=("grid", None, 54, True, {}, ({"volume_weighting": 0},))),
        capability=efp.CLOSE,
        parity=(
            Parity("owner_batched", lambda: efp._walk(420, seed=41), _one(lambda: efp._cfg(batched=True)), WINDOWS,
                   OWNER_BATCHED_PARITY),
        ),
        composer=Composer(efp.SEED, efp.CLOSE, lambda: efp._frame(efp._pullback(1)).iloc[:efp.CROSS_AT + 1].copy(),
                          lambda: efp._frame(efp._pullback(-1)).iloc[:efp.CROSS_AT + 1].copy(), 1.0),
        sim_fills=SimFills(efp.SEED, efp.CLOSE, lambda: efp._walk(3000, seed=31), "1h", 3,
                           frozenset({"tiered_tp_pct"}), False, False),
        **_uncovered("check_time", "time_stop_fills"),
    ),
    "elder_ray_pullback": Case(
        registration=Registration(study="elder_ray_pullback_1648", validator=True,
                                  defaults=(lambda: _seed_json(erp.SEED)["params"], lambda: erp.DEFAULTS),
                                  grid=lambda: _seed_json(erp.SEED)["optimizer_grid"], discovery=erp.STUDY_DIR),
        parity=(
            Parity("entry", lambda: erp._frozen("BTC 4h", 320),
                   _cfgs(erp._cfg, [erp.DEFAULTS]), WINDOWS,
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
        sim_fills=SimFills(erp.DEFAULTS, erp.CLOSE, lambda: erp._walk(900, 5), "4h", 0,
                           frozenset({"sl", "tiered_tp_pct"}), True, False),
        **_uncovered("capability", "check_time", "time_stop_fills"),
    ),
    "fisher_reversal": Case(
        registration=Registration(study="fisher_reversal_1646", validator=False,
                                  defaults=(lambda: _seed_json(fis.SEED)["params"],
                                            lambda: {"lookback": 9, "extreme": 1.5}),
                                  grid=lambda: _seed_json(fis.SEED)["optimizer_grid"]),
        parity=(
            Parity("entry", lambda: fis._frozen("BTC 4h", 320),
                   _cfgs(fis._cfg, [{"lookback": 9, "extreme": 1.5}]), WINDOWS, ENTRY_PARITY),
            Parity("composed", lambda: fis._frozen("SOL 4h", 520),
                   _one(lambda: fis._cfg({"lookback": 5, "extreme": 0.75}, close_refs=_copy_refs(fis.CLOSE),
                                         direction="both")), WINDOWS, COMPOSED_PARITY),
            Parity("batched", lambda: fis._frozen("BTC 4h", 300),
                   _one(lambda: fis._cfg({"lookback": 5, "extreme": 0.75}, close_refs=_copy_refs(fis.CLOSE),
                                         direction="both", batched=True)), WINDOWS, BATCHED_PARITY),
        ),
        composer=Composer({"lookback": 9, "extreme": 0.5}, fis.CLOSE, lambda: fis._entry_frame("long"),
                          lambda: fis._entry_frame("short"), 50.0),
        sim_fills=SimFills({"lookback": 9, "extreme": 0.75}, fis.CLOSE, lambda: fis._walk(900, 5), "4h", 0,
                           frozenset({"sl", "tiered_tp_pct"}), True, False),
        **_uncovered("capability", "check_time", "time_stop_fills"),
    ),
    "klinger_divergence": Case(
        registration=Registration(study="klinger_divergence_1662", validator=True,
                                  defaults=(lambda: _seed_json(kl.SEED)["params"], lambda: kl.DEFAULTS),
                                  grid=lambda: _seed_json(kl.SEED)["optimizer_grid"],
                                  combos=("sweep", lambda: kl.DEFAULTS, 768, True, {})),
        capability=kl.CLOSE,
        parity=(
            Parity("entry", lambda: kl._frozen("BTC 4h", 460),
                   _cfgs(kl._cfg, [kl.DEFAULTS]), WINDOWS, ENTRY_PARITY),
            Parity("composed", lambda: kl._frozen("SOL 4h", 560),
                   _one(lambda: kl._cfg(kl.FAST, close_refs=_copy_refs(kl.CLOSE), direction="both")),
                   WINDOWS, COMPOSED_PARITY),
            Parity("batched", lambda: kl._frozen("BTC 4h", 380),
                   _one(lambda: kl._cfg(kl.FAST, close_refs=_copy_refs(kl.CLOSE), direction="both", batched=True)),
                   WINDOWS, BATCHED_PARITY + (("opens_any", None),)),
        ),
        check_time=CheckTime((TIMESTAMP_KIND_OPEN, TIMESTAMP_KIND_NATIVE_CLOSE), kl.FOUR_H,
                             _first_long_check_frame(kl, 1500), lambda df: kl._apply(df, kl.DEFAULTS), kl._runtime,
                             "klinger_reason", "klinger_oscillator"),
        **_uncovered("composer", "sim_fills", "time_stop_fills"),
    ),
    "macro_event_breakout": Case(
        registration=Registration(study="macro_event_breakout_1654", validator=True, production_refused=True,
                                  combos=("grid", None, 12, True, {})),
        **_uncovered("capability", "parity", "composer", "sim_fills", "check_time", "time_stop_fills",
                     reason=SIDE_INPUT),
    ),
    "mass_index_reversal": Case(
        registration=Registration(study="mass_index_reversal_1670", validator=True, defaults=(lambda: mir.SEED,),
                                  grid_fixed={"bulge_filter": [1]}, seed_in_grid=mir.SEED,
                                  combos=("grid", None, 324, True, {})),
        capability=mir.CLOSE,
        parity=(
            Parity("owner_batched", lambda: mir._walk(720, seed=41), _one(lambda: mir._cfg(batched=True)), WINDOWS,
                   OWNER_BATCHED_PARITY),
        ),
        composer=Composer(mir.SEED, mir.CLOSE,
                          lambda: mir._frame(*mir._short_entry_mid(-0.05)).iloc[:mir.DOWN_AT + 1].copy(),
                          lambda: mir._frame(*mir._short_entry_mid(0.05)).iloc[:mir.DOWN_AT + 1].copy(), 1.0),
        sim_fills=SimFills(mir.SEED, mir.CLOSE, lambda: mir._walk(2400, seed=31), "1h", 3, frozenset(), False, False),
        **_uncovered("check_time", "time_stop_fills"),
    ),
    "money_flow_index_reversal": Case(
        registration=Registration(study="money_flow_index_reversal_1658", validator=False,
                                  defaults=(lambda: {"lookback": 14, "oversold": 20.0, "overbought": 80.0},),
                                  grid_keys=frozenset({"lookback", "oversold", "overbought"})),
        parity=(
            Parity("entry", lambda: mfi._frozen("BTC 4h", 320),
                   _cfgs(mfi._cfg, [{"lookback": 14, "oversold": 20.0, "overbought": 80.0}]), WINDOWS, ENTRY_PARITY),
            Parity("composed", lambda: mfi._frozen("SOL 4h", 420),
                   _one(lambda: mfi._cfg({"lookback": 7, "oversold": 30.0, "overbought": 70.0},
                                         close_refs=_copy_refs(mfi.CLOSE), direction="both")), WINDOWS,
                   COMPOSED_PARITY),
            Parity("batched", lambda: mfi._frozen("BTC 4h", 260),
                   _one(lambda: mfi._cfg({"lookback": 7, "oversold": 30.0, "overbought": 70.0},
                                         close_refs=_copy_refs(mfi.CLOSE), direction="both", batched=True)),
                   WINDOWS, BATCHED_PARITY),
        ),
        **_uncovered("capability", "composer", "sim_fills", "check_time", "time_stop_fills"),
    ),
    "on_balance_volume_divergence": Case(
        registration=Registration(study="on_balance_volume_divergence_1657", validator=False),
        parity=(
            Parity("strict_owner", lambda: obv._market(520),
                   _one(lambda: ParityConfig(strategy_name=obv.NAME, params={}, registry="futures",
                                             close_refs=obv.TP_CLOSE, direction="both", comparison_mode="strict")),
                   WINDOWS, (("rows", 1), ("all_match", None), ("open_set_is_both", None))),
            Parity("batched", lambda: obv._market(240),
                   _one(lambda: ParityConfig(strategy_name=obv.NAME, params={}, registry="futures", batched=True,
                                             symbol="BTC", timeframe="4h")),
                   (200,), (("rows", 1), ("all_match", None), ("batch_signals", None))),
        ),
        **_uncovered("capability", "composer", "sim_fills", "check_time", "time_stop_fills"),
    ),
    "open_interest_breakout": Case(
        registration=Registration(study="open_interest_breakout_1637", validator=False),
        **_uncovered("capability", "parity", "composer", "sim_fills", "check_time", "time_stop_fills"),
    ),
    "order_book_imbalance": Case(
        registration=Registration(study="order_book_imbalance_1635", validator=True, production_refused=True),
        **_uncovered("capability", "parity", "composer", "sim_fills", "check_time", "time_stop_fills",
                     reason=SIDE_INPUT),
    ),
    "perpetual_premium_reversion": Case(
        registration=Registration(study="perpetual_premium_reversion_1639", validator=True, production_refused=True,
                                  combos=("grid", None, 12, True, {})),
        **_uncovered("capability", "parity", "composer", "sim_fills", "check_time", "time_stop_fills",
                     reason=SIDE_INPUT),
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
                   WINDOWS, (("rows", 101), ("all_match", None), ("open_set_covers_both", None))),
            Parity("batched", lambda: rcr._market(420),
                   _one(lambda: ParityConfig(strategy_name=rcr.NAME, params=dict(rcr.SEED, fit_window=20,
                                                                                 slope_ceiling=1.0),
                                             registry="futures", batched=True, symbol="BTC", timeframe="4h")),
                   (200,), (("no_batch_mismatches", None), ("rows", 101), ("all_match", None),
                            ("open_set_covers_both", None))),
        ),
        **_uncovered("capability", "composer", "sim_fills", "check_time", "time_stop_fills"),
    ),
    "relative_vigor_index": Case(
        registration=Registration(study="relative_vigor_index_1666", validator=False),
        parity=(
            Parity("time_stop_owner", lambda: rvi._market(),
                   _one(lambda: ParityConfig(strategy_name=rvi.NAME, params={"period": 10, "zero_line_filter": True},
                                             registry="futures",
                                             close_refs=[{"name": "time_stop", "params": {"max_bars": 20}}],
                                             direction="both", comparison_mode="approximate")),
                   WINDOWS, (("rows", 1), ("all_match", None), ("close_parity", "incomplete"),
                             ("not_clean", None), ("live_signals", None))),
            Parity("batched", lambda: rvi._market(240),
                   _one(lambda: ParityConfig(strategy_name=rvi.NAME, params={"period": 10, "zero_line_filter": True},
                                             registry="futures", batched=True, symbol="BTC", timeframe="4h")),
                   (200,), (("rows", 1), ("all_match", None))),
        ),
        time_stop_fills=TimeStopFills(_rvi_long, 20, False, _rvi_short, 20, 21, 27, False),
        **_uncovered("capability", "composer", "sim_fills", "check_time"),
    ),
    "schaff_trend_cycle": Case(
        registration=Registration(study="schaff_trend_cycle_1651", validator=True,
                                  defaults=(lambda: _seed_json(stc.SEED)["params"], lambda: stc.DEFAULTS),
                                  grid=lambda: _seed_json(stc.SEED)["optimizer_grid"],
                                  combos=("sweep", lambda: stc.DEFAULTS, None, True, {})),
        parity=(
            Parity("entry", lambda: stc._frozen("BTC 4h", 420), _cfgs(stc._cfg, [stc.DEFAULTS]),
                   WINDOWS, ENTRY_PARITY),
            Parity("composed", lambda: stc._frozen("SOL 4h", 560),
                   _one(lambda: stc._cfg(dict(stc.DEFAULTS, fast_period=12, slow_period=26, cycle_length=8),
                                         close_refs=_copy_refs(stc.CLOSE), direction="both")),
                   WINDOWS, COMPOSED_PARITY),
            Parity("batched", lambda: stc._frozen("BTC 4h", 360),
                   _one(lambda: stc._cfg(dict(stc.DEFAULTS, fast_period=12, slow_period=26, cycle_length=8),
                                         close_refs=_copy_refs(stc.CLOSE), direction="both", batched=True)),
                   WINDOWS, BATCHED_PARITY + (("opens_any", None),)),
        ),
        composer=Composer(stc.DEFAULTS, stc.CLOSE, lambda: stc._entry_frame("long"),
                          lambda: stc._entry_frame("short"), 50.0),
        check_time=CheckTime((TIMESTAMP_KIND_OPEN, TIMESTAMP_KIND_NATIVE_CLOSE), stc.FOUR_H,
                             _first_long_check_frame(stc, 900), lambda df: stc._apply(df, stc.DEFAULTS), stc._runtime,
                             "stc_reason", "stc_value"),
        **_uncovered("capability", "sim_fills", "time_stop_fills"),
    ),
    "stochastic_momentum_index": Case(
        registration=Registration(study="stochastic_momentum_index_1663", validator=True,
                                  defaults=(lambda: _seed_json(smi.SEED)["params"], lambda: smi.DEFAULTS),
                                  grid=lambda: _seed_json(smi.SEED)["optimizer_grid"],
                                  combos=("sweep", lambda: smi.DEFAULTS, 729, True, {})),
        parity=(
            Parity("entry", lambda: smi._frozen("BTC 4h", 420), _cfgs(smi._cfg, [smi.DEFAULTS]),
                   WINDOWS, ENTRY_PARITY),
            Parity("composed", lambda: smi._frozen("SOL 4h", 560),
                   _one(lambda: smi._cfg(smi.DEFAULTS, close_refs=_copy_refs(smi.CLOSE), direction="both")),
                   WINDOWS, COMPOSED_PARITY),
            Parity("batched", lambda: smi._frozen("BTC 4h", 360),
                   _one(lambda: smi._cfg(smi.DEFAULTS, close_refs=_copy_refs(smi.CLOSE), direction="both",
                                         batched=True)), WINDOWS, BATCHED_PARITY + (("opens_any", None),)),
        ),
        composer=Composer(smi.DEFAULTS, smi.CLOSE, lambda: smi._entry_frame("long"),
                          lambda: smi._entry_frame("short"), 50.0),
        check_time=CheckTime((TIMESTAMP_KIND_OPEN, TIMESTAMP_KIND_NATIVE_CLOSE), smi.FOUR_H,
                             _first_long_check_frame(smi, 900), lambda df: smi._apply(df, smi.DEFAULTS), smi._runtime,
                             "smi_reason", "smi_value"),
        **_uncovered("capability", "sim_fills", "time_stop_fills"),
    ),
    "trix_momentum": Case(
        registration=Registration(study="trix_momentum_1659", validator=True,
                                  defaults=(lambda: trix.SEED, lambda: _protocol(trix.STUDY)["seed_params"]),
                                  grid=lambda: _protocol(trix.STUDY)["grids"][trix.NAME],
                                  combos=("sweep", lambda: trix.SEED, 18, True, {}), discovery=trix.STUDY),
        capability=trix._close_refs(),
        parity=(
            Parity("raw_entry", lambda: trix._frozen("BTC", "4h", 420), _cfgs(trix._cfg, [trix.SEED]),
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
        **_uncovered("sim_fills", "check_time", "time_stop_fills"),
    ),
    "true_strength_index": Case(
        registration=Registration(study="true_strength_index_1667", validator=True,
                                  defaults=(lambda: tsi.SEED, lambda: _protocol(tsi.STUDY)["seed_params"]),
                                  grid=lambda: _protocol(tsi.STUDY)["grids"][tsi.NAME],
                                  combos=("sweep", lambda: tsi.SEED, 54, True, {}), discovery=tsi.STUDY),
        capability=tsi._close_refs(),
        parity=(
            Parity("raw_entry_seed", lambda: tsi._frozen("BTC", "4h", 420), _cfgs(tsi._cfg, [tsi.SEED]),
                   WINDOWS, ENTRY_PARITY),
            Parity("composed", lambda: tsi._frozen("SOL", "4h", 620),
                   _one(lambda: tsi._cfg(tsi.SMALL, close_refs=tsi._close_refs(), direction="both")),
                   WINDOWS, COMPOSED_PARITY),
            Parity("batched", lambda: tsi._frozen("BTC", "4h", 320),
                   _one(lambda: tsi._cfg(tsi.SMALL, close_refs=tsi._close_refs(), direction="both", batched=True)),
                   WINDOWS, BATCHED_PARITY),
        ),
        composer=Composer(tsi.SMALL, tsi._close_refs(), lambda: tsi._from_closes([100.0] * 20 + [99.0, 104.0]),
                          lambda: tsi._from_closes([100.0] * 20 + [101.0, 96.0]), 50.0),
        sim_fills=SimFills(tsi.SMALL, tsi._close_refs(), lambda: tsi._walk(1500, 5), "4h", 0,
                           frozenset({"sl", "tiered_tp_pct"}), True, True),
        **_uncovered("check_time", "time_stop_fills"),
    ),
    "ultimate_oscillator": Case(
        registration=Registration(study="ultimate_oscillator_1664", validator=False),
        parity=(
            Parity("strict_owner_batched", lambda: uo._market(420, 7),
                   _one(lambda: ParityConfig(strategy_name=uo.NAME, registry="futures", platform="hyperliquid",
                                             close_refs=uo.CLOSE, direction="both", batched=True,
                                             stop_kwargs={"stop_loss_atr_mult": 1.0})),
                   (200,), (("compared", 1), ("no_mismatches", None), ("clean", None), ("no_batch_mismatches", None),
                            ("opens_any", None))),
        ),
        **_uncovered("capability", "composer", "sim_fills", "check_time", "time_stop_fills"),
    ),
    "volume_profile_reclaim": Case(
        registration=Registration(study="volume_profile_reclaim_1665", validator=True, production_refused=True,
                                  combos=("grid", None, 9, True, {"expected_interval_ms": vpr.BAR})),
        **_uncovered("capability", "parity", "composer", "sim_fills", "check_time", "time_stop_fills",
                     reason=SIDE_INPUT),
    ),
    "vortex_trend": Case(
        registration=Registration(study="vortex_trend_1647", validator=False),
        parity=(
            Parity("time_stop_owner", lambda: vtx._market(),
                   _one(lambda: dict(strategy_name=vtx.NAME, params=None, registry="futures",
                                     close_refs=[{"name": "time_stop", "params": {"max_bars": 20}}],
                                     comparison_mode="approximate")),
                   (DEFAULT_WINDOW,), (("rows_input_minus", 199), ("all_match", None), ("close_parity", "incomplete"),
                                       ("not_clean", None), ("bt_signals", None), ("open_set_covers_both", None))),
        ),
        time_stop_fills=TimeStopFills(_vtx_long, 20, False, _vtx_short, 20, 23, 27, True),
        **_uncovered("capability", "composer", "sim_fills", "check_time"),
    ),
    "volume_spike_breakout": Case(
        registration=Registration(study="volume_spike_breakout_2008", validator=True,
                                  platforms=("spot", "futures"),
                                  defaults=(lambda: VSB_DEFAULTS,
                                            lambda: _seed_json(os.path.join(VSB_STUDY, "candidate_seed.json"))["params"]),
                                  grid=lambda: {"lookback": [10, 20, 40, 80, 100], "vol_period": [10, 20, 40, 80, 100],
                                                "vol_multiplier": [1.0, 1.5, 2.0, 3.0], "min_body_frac": [0.0, 0.25, 0.5]},
                                  combos=("grid", None, 300, True, {}), grid_values_validate=True,
                                  discovery=VSB_STUDY, catalog=os.path.join(VSB_STUDY, "evidence")),
        capability=VSB_CLOSE,
        parity=(
            Parity("futures_owner", lambda: _vsb_walk(420), _one(lambda: _vsb_cfg("futures")), (200, None),
                   OWNER_PARITY),
            Parity("spot_owner", lambda: _vsb_walk(420), _one(lambda: _vsb_cfg("spot")), (200, None),
                   (("compared", 1), ("no_mismatches", None), ("clean", None), ("eligible", None),
                    ("opens_any", None), ("no_short_opens", None))),
            Parity("max_windows", lambda: _vsb_walk(420, seed=5, spike_every=3),
                   (("futures", lambda: _vsb_cfg("futures", VSB_MAX)), ("spot", lambda: _vsb_cfg("spot", VSB_MAX))),
                   (200, None), (("compared", 1), ("no_mismatches", None), ("clean", None), ("opens_any", None))),
            Parity("batched", lambda: _vsb_walk(300), _one(lambda: _vsb_cfg("futures", batched=True)), (200,),
                   OWNER_BATCHED_PARITY),
        ),
        composer=Composer(VSB_SMALL, VSB_CLOSE, lambda: _vsb_event(1), lambda: _vsb_event(-1), 1.0),
        sim_fills=SimFills(VSB_SMALL, VSB_CLOSE, lambda: _vsb_walk(900, seed=3, spike_every=3), "4h", 3,
                           frozenset({"tiered_tp_pct"}), False, True),
        time_stop_fills=TimeStopFills(_vsb_long, 20, True, _vsb_short, 20, 23, 27, True),
        spot_route=SpotRoute(VSB_SMALL, VSB_CLOSE, lambda: _vsb_event(1), lambda: _vsb_event(-1),
                             lambda: _vsb_walk(900, seed=3), _vsb_defect, 1.0, "vsb_reason", "short_suppressed_spot"),
        **_uncovered("check_time", reason=NotCovered(
            "the evaluator reads no decision-time attribute: closed-bar selection is the check scripts' "
            "closed_bar_decisions selector, covered by the check contract")),
    ),
    "williams_r_reversal": Case(
        registration=Registration(study="williams_r_reversal_1650", validator=True),
        parity=(
            Parity("strict_owner", lambda: wr._market(420),
                   _one(lambda: ParityConfig(strategy_name=wr.NAME,
                                             params={"lookback": 14, "oversold": -80.0, "overbought": -20.0},
                                             registry="futures", platform="hyperliquid", timeframe="4h",
                                             close_refs=wr.TP, direction="both", comparison_mode="strict")),
                   WINDOWS, (("rows", 1), ("all_match", None), ("live_signals", None),
                             ("close_parity_not", "incomplete"))),
            Parity("batched", lambda: wr._market(240),
                   _one(lambda: ParityConfig(strategy_name=wr.NAME,
                                             params={"lookback": 14, "oversold": -80.0, "overbought": -20.0},
                                             registry="futures", platform="hyperliquid", batched=True, symbol="BTC",
                                             timeframe="4h")),
                   (200,), (("rows", 1), ("all_match", None), ("no_batch_mismatches", None))),
        ),
        **_uncovered("capability", "composer", "sim_fills", "check_time", "time_stop_fills"),
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
    assert FAMILIES == ("registration", "capability", "parity", "composer", "sim_fills", "check_time",
                        "time_stop_fills", "spot_route")
    for name, case in sorted(CASES.items()):
        for family in FAMILIES:
            value = getattr(case, family)
            with _case(f"{name}-{family}"):
                assert value is not None
                if isinstance(value, NotCovered):
                    assert isinstance(value.reason, str) and value.reason.strip()
    assert all(not isinstance(case.registration, NotCovered) for case in CASES.values())
    for name, case in sorted(CASES.items()):
        with _case(name):
            assert isinstance(case.spot_route, NotCovered) is ("spot" not in case.registration.platforms)


def test_production_refused_set_is_the_side_input_cases_plus_the_non_candle_capabilities():
    refused = _fut()._registry.production_refused_strategies()
    assert refused == frozenset({"cumulative_volume_delta_divergence", "bitcoin_lead_lag", "macro_event_breakout",
                                 "volume_profile_reclaim", "perpetual_premium_reversion", "order_book_imbalance",
                                 "inventory_market_making", "sweep_absorption_ifvg"})
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


def _catalog_adds_only(name, platforms, evidence):
    for platform in ("spot", "futures"):
        live = json.loads(subprocess.run([sys.executable, os.path.join(_REPO, "shared_tools", "strategy_catalog.py"),
                                          "--list-json", "--platform", platform],
                                         capture_output=True, text=True, cwd=_REPO, check=True).stdout)
        captures = {}
        for phase in ("before", "after"):
            with open(os.path.join(evidence, f"catalog_{phase}_{platform}.json")) as fh:
                captures[phase] = {(e["kind"], e["id"]): e for e in json.load(fh)["entries"]}
        before, after = captures["before"], captures["after"]
        added = sorted(set(after) - set(before))
        assert added == ([("open", name)] if platform in platforms else []), (platform, added)
        assert not set(before) - set(after)
        assert all(after[key] == before[key] for key in before), platform
        live_rows = {(e["kind"], e["id"]): e for e in live["entries"]}
        if platform in platforms:
            assert live_rows[("open", name)] == after[("open", name)], platform


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
    assert raw["short_entries"] is True and raw["platforms"] == spec.platforms
    if spec.validator:
        assert callable(raw["param_validator"])
    else:
        assert raw["param_validator"] is None
    assert entry["production_refused"] is spec.production_refused
    assert (name in registry.production_refused_strategies()) is spec.production_refused
    assert name in registry.short_entry_strategies()
    assert name in registry.DISCOVERY_HIDDEN_STRATEGIES
    assert name not in fut.list_strategies()
    spot = load_registry("spot")
    if "spot" in spec.platforms:
        assert name not in spot.list_strategies()
        assert spot.STRATEGY_REGISTRY[name]["default_params"] == entry["default_params"]
        assert spot.STRATEGY_REGISTRY[name]["fn"] is not entry["fn"]
        assert registry.PLATFORM_ORDER["spot"].count(name) == 1
    else:
        assert name not in spot.STRATEGY_REGISTRY
        assert name not in registry.PLATFORM_ORDER["spot"]
    assert registry.PLATFORM_ORDER["futures"].count(name) == 1
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
    if spec.catalog is not None:
        _catalog_adds_only(name, spec.platforms, spec.catalog)


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


def _spot_compose(name, params, close_refs, df, side="", ctx=None):
    reg = load_registry("spot")
    evaluation = evaluate_open_close(
        reg.apply_strategy, reg.get_strategy, df, name, name, [c["name"] for c in close_refs], side,
        params=dict(params), position_ctx=ctx, close_evaluate=close_evaluate,
        close_params_by_name={c["name"]: c["params"] for c in close_refs})
    return evaluation, finalize_decision(evaluation, side)


@pytest.mark.parametrize("name", _names("spot_route"))
def test_spot_route_contract(name):
    spec = CASES[name].spot_route
    spot = load_registry("spot")
    with _case("evaluator"):
        long_df, short_df = spec.long_frame(), spec.short_frame()
        assert int(spot.apply_strategy(name, long_df, dict(spec.params))["signal"].iloc[-1]) == 1
        fut_short = _apply(name, short_df, dict(spec.params)).iloc[-1]
        spot_short = spot.apply_strategy(name, short_df, dict(spec.params)).iloc[-1]
        assert int(fut_short["signal"]) == -1 and int(spot_short["signal"]) == 0
        assert spot_short[spec.reason_col] == spec.suppressed_reason
        walk = spec.walk()
        fut_sig = _apply(name, walk, dict(spec.params))["signal"].to_numpy()
        spot_out = spot.apply_strategy(name, walk, dict(spec.params))
        spot_sig = spot_out["signal"].to_numpy()
        assert (fut_sig == 1).any() and (fut_sig == -1).any()
        assert (spot_sig >= 0).all() and (spot_sig == np.maximum(fut_sig, 0)).all()
        assert (spot_out[spec.reason_col].to_numpy()[fut_sig == -1] == spec.suppressed_reason).all()
    with _case("composer"):
        _, decision = _spot_compose(name, spec.params, spec.close_refs, long_df)
        assert (decision["open_action"], decision["signal"]) == ("long", 1)
        _, decision = _spot_compose(name, spec.params, spec.close_refs, short_df)
        assert (decision["open_action"], decision["signal"]) == ("none", 0)
        mark = float(long_df["close"].iloc[-1])
        ctx = {"side": "long", "avg_cost": mark, "entry_atr": spec.entry_atr, "current_quantity": 1.0,
               "initial_quantity": 1.0}
        _, held = _spot_compose(name, spec.params, spec.close_refs, long_df, "long", ctx)
        assert held["signal"] == 0 and held["close_fraction"] == 0.0
    with _case("entry_defect_keeps_close_owner"):
        defect = spec.defect_frame()
        out = spot.apply_strategy(name, defect, dict(spec.params)).iloc[-1]
        assert int(out["signal"]) == 0 and not bool(out[spec.reason_col.replace("_reason", "_valid")])
        mark = float(defect["close"].iloc[-1])
        for reg_compose, side in ((_spot_compose, "long"), (_compose, "long"), (_compose, "short")):
            cost = mark / 1.05 if side == "long" else mark * 1.05
            ctx = {"side": side, "avg_cost": cost, "entry_atr": spec.entry_atr, "current_quantity": 1.0,
                   "initial_quantity": 1.0}
            evaluation, held = reg_compose(name, spec.params, spec.close_refs, defect, side, ctx)
            assert evaluation.open_signal == 0 and held["open_action"] == "none"
            assert held["close_fraction"] == 1.0 and held["close_strategy"] == spec.close_refs[0]["name"]
    with _case("simulator"):
        sig = ensure_atr_indicator(spot.apply_strategy(name, walk, dict(spec.params)))
        bt = Backtester(initial_capital=1000.0, platform="binanceus", open_strategy={"name": name,
                        "params": dict(spec.params)}, close_strategies=_copy_refs(spec.close_refs),
                        stop_loss_atr_mult=1.0, strategy_type="spot")
        trades = bt.run(sig, strategy_name=name, symbol="BTC/USDT", timeframe="4h", save=False)["trades"]
        assert trades and {t["side"] for t in trades} == {"long"}
        for t in trades:
            pos = sig.index.get_loc(pd.Timestamp(t["entry_date"]))
            assert pos > 0 and int(sig["signal"].iloc[pos - 1]) == 1
            assert t["entry_price"] == pytest.approx(float(walk["open"].iloc[pos]) * (1 + bt.slippage_pct), rel=1e-12)


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


