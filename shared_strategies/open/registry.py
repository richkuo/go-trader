
import functools
import inspect
import os
import re
import sys
from typing import Any, Dict, List, Optional, Tuple

import numpy as np
import pandas as pd

_THIS_DIR = os.path.dirname(__file__)
for _p in (
    os.path.join(_THIS_DIR, "spot"),
    _THIS_DIR,
):
    if _p not in sys.path:
        sys.path.insert(0, _p)

from indicators import sma, ema
from indicators_core import atr_from_true_range, atr_sma, true_range, wilder_rsi
from amd_ifvg import amd_ifvg_core
from chart_patterns import chart_pattern_core
from liquidity_sweeps import liquidity_sweep_core
from range_scalper import range_scalper_core
from consolidation_range import consolidation_range_core
from atr_band_revert import atr_band_revert_core
from sweep_squeeze_combo import sweep_squeeze_combo_core
from adx_trend import adx_trend_core
from commodity_channel import commodity_channel_trend_core
from bear_pullback_st import bear_pullback_st_core
from donchian_breakout import donchian_breakout_core
from funding_skew import funding_skew_core
from momentum_pro import momentum_pro_core
from mean_reversion_pro import mean_reversion_pro_core
from rsi_bb_combo import rsi_bb_combo_core
from mtf_confluence import mtf_confluence_core
from regime_adaptive import regime_adaptive_core
from regime_adaptive_htf import regime_adaptive_htf_core
from session_breakout import session_breakout_core
from vwap_rejection_st import vwap_rejection_st_core
from vol_momentum import vol_momentum_core
from anchored_vwap import anchored_vwap_core
from anchored_vwap_channel import anchored_vwap_channel_core
from anchored_vwap_reversion import anchored_vwap_reversion_core
from analog_retrieval import analog_retrieval_core
from chaikin_money_flow import chaikin_money_flow_breakout_core
from ease_of_movement import ease_of_movement_breakout_core
from open_interest_breakout import open_interest_breakout_core
from connors_rsi import connors_rsi_reversion_core
from vortex_trend import vortex_trend_core
from relative_vigor_index import relative_vigor_index_core
from awesome_oscillator import awesome_oscillator_core
from chande_momentum import chande_momentum_core
from dual_thrust import dual_thrust_core, validate_dual_thrust_param_dict
from on_balance_volume import on_balance_volume_divergence_core
from williams_r import williams_r_param_validator, williams_r_reversal_core
from money_flow_index import money_flow_index_reversal_core
from aroon_trend import aroon_trend_core, validate_aroon_param_dict
from regression_channel_reversion import regression_channel_reversion_core, validate_rcr_param_dict
from ultimate_oscillator import ultimate_oscillator_core
from fisher_reversal import fisher_reversal_core
from elder_ray_pullback import elder_ray_pullback_core, validate_elder_ray_param_dict
from adaptive_moving_average import adaptive_moving_average_core, ama_param_validator
from schaff_trend_cycle import schaff_trend_cycle_core, stc_param_validator
from trix_momentum import trix_momentum_core, trix_param_validator
from camarilla_pivot_reversal import camarilla_pivot_reversal_core, validate_camarilla_param_dict
from mass_index_reversal import mass_index_reversal_core, mir_param_validator
from calendar_conditioned_entry import calendar_conditioned_entry_core, calendar_param_validator
from stochastic_momentum_index import smi_param_validator, stochastic_momentum_index_core
from elder_force_pullback import efp_param_validator, elder_force_pullback_core
from true_strength_index import true_strength_index_core, tsi_param_validator
from basket_scripted_fixture import basket_scripted_fixture_core
from cross_sectional_momentum import (
    XSMOM_DEFAULT_PARAMS,
    cross_sectional_momentum_core,
    xsmom_basket_preflight,
    xsmom_param_validator,
)
from cross_sectional_reversal import (
    XSREV_DEFAULT_PARAMS,
    cross_sectional_reversal_core,
    xsrev_basket_preflight,
    xsrev_param_validator,
)
from klinger_divergence import klinger_divergence_core, klinger_param_validator
from cumulative_volume_delta_divergence import (
    DEFAULT_PARAMS as DEFAULT_CVD_PARAMS,
    cumulative_volume_delta_divergence_core,
    cvd_param_validator,
)
from macro_event_breakout import (
    DEFAULT_PARAMS as DEFAULT_MACRO_EVENT_PARAMS,
    macro_event_breakout_core,
    macro_param_validator,
)
from perpetual_premium_reversion import (
    DEFAULT_PARAMS as DEFAULT_PREMIUM_PARAMS,
    perpetual_premium_reversion_core,
    premium_param_validator,
)
from bitcoin_lead_lag import (
    DEFAULT_PARAMS as DEFAULT_LEAD_LAG_PARAMS,
    bitcoin_lead_lag_core,
    lead_lag_param_validator,
)
from volume_profile_reclaim import (
    DEFAULT_PARAMS as DEFAULT_VPR_PARAMS,
    volume_profile_reclaim_core,
    vpr_param_validator,
)
from order_book_imbalance import (
    DEFAULT_PARAMS as DEFAULT_OBI_PARAMS,
    obi_param_validator,
    order_book_imbalance_core,
)


VALID_PLATFORMS: Tuple[str, ...] = ("spot", "futures")

STRATEGIES: Dict[str, Dict[str, Any]] = {}

EDGE_STATUS_NO_EDGE = "no_edge"

VALID_EDGE_STATUSES: Tuple[str, ...] = (EDGE_STATUS_NO_EDGE,)

VALID_EDGE_SOURCES: Tuple[str, ...] = (
    "fee_audit_m5",
    "study_fail",
    "study_inconclusive",
    "unvalidated",
)


SINGLE_VALUE_VALIDATED_STRATEGIES: Tuple[str, ...] = ("aroon_trend", "regression_channel_reversion")

BASKET_CONTRACT: Dict[str, str] = {"input": "closed_bar_frames", "output": "signed_weights"}


def basket_rankers() -> frozenset:
    return frozenset(name for name, entry in STRATEGIES.items() if entry.get("basket"))


def get_basket_ranker(name: str) -> Dict[str, Any]:
    entry = STRATEGIES.get(name)
    if entry is None or not entry.get("basket"):
        raise ValueError(f"{name!r} is not a registered basket evaluator; registered: {sorted(basket_rankers())}")
    return entry


def _basket_single_symbol_refusal(name: str):
    def refuse(*_args, **_kwargs):
        raise ValueError(
            f"{name} is a basket evaluator; it ranks a basket universe and cannot run on one symbol")
    refuse.__name__ = f"{name}_single_symbol_refusal"
    return refuse


def _validate_basket(name: str, basket, platforms: Tuple[str, ...], short_entries: bool) -> None:
    if basket is None:
        return
    if not isinstance(basket, dict) or basket != BASKET_CONTRACT:
        raise ValueError(f"{name}: basket must be exactly {BASKET_CONTRACT!r}, got {basket!r}")
    if tuple(platforms) != ("futures",):
        raise ValueError(f"{name}: a basket evaluator registers for platforms ('futures',) only, got {platforms!r}")
    if short_entries:
        raise ValueError(f"{name}: a basket evaluator has no single-symbol entries; declare short_entries=False")


def no_edge_strategies() -> frozenset:
    return frozenset(
        name for name, entry in STRATEGIES.items()
        if entry.get("edge_status") == EDGE_STATUS_NO_EDGE
    )


def production_refused_strategies() -> frozenset:
    return frozenset(
        name for name, entry in STRATEGIES.items()
        if entry.get("production_refused")
    )


def short_entry_strategies() -> frozenset:
    return frozenset(
        name for name, entry in STRATEGIES.items()
        if entry["short_entries"]
    )


def _validate_short_entries(name: str, short_entries, default_params: dict,
                            platforms: Tuple[str, ...],
                            variants: Dict[str, Dict[str, Any]]) -> None:
    if not isinstance(short_entries, bool):
        raise ValueError(
            f"{name}: short_entries must be True or False, got {short_entries!r}"
        )
    if short_entries and "futures" not in platforms:
        raise ValueError(
            f"{name}: short_entries=True requires 'futures' in platforms {platforms}"
        )
    if short_entries:
        return
    shipped = [("default_params", default_params)] + [
        (f"variants[{v!r}].default_params", spec.get("default_params", {}))
        for v, spec in sorted(variants.items())
    ]
    for where, params in shipped:
        if params.get("allow_short"):
            raise ValueError(
                f"{name}: {where} ships allow_short={params['allow_short']!r} "
                f"but short_entries=False; declare short_entries=True"
            )


def _validate_edge_metadata(name: str, edge_status, edge_source, edge_ref) -> None:
    if edge_status is None:
        if edge_source is not None or edge_ref is not None:
            raise ValueError(
                f"{name}: edge_source/edge_ref require edge_status; "
                f"got edge_source={edge_source!r}, edge_ref={edge_ref!r}"
            )
        return
    if edge_status not in VALID_EDGE_STATUSES:
        raise ValueError(
            f"{name}: unknown edge_status {edge_status!r}; expected one of {VALID_EDGE_STATUSES}"
        )
    if edge_source not in VALID_EDGE_SOURCES:
        raise ValueError(
            f"{name}: edge_status={edge_status!r} needs edge_source in "
            f"{VALID_EDGE_SOURCES}, got {edge_source!r}"
        )
    if not isinstance(edge_ref, str) or not edge_ref.strip():
        raise ValueError(
            f"{name}: edge_status={edge_status!r} needs a nonempty edge_ref, got {edge_ref!r}"
        )


_CONSTRAINT_RE = re.compile(
    r"^\s*([A-Za-z_][A-Za-z0-9_]*)\s*(<=|>=|<|>)\s*"
    r"([A-Za-z_][A-Za-z0-9_]*|[-+]?\d+(?:\.\d+)?)\s*$"
)

_CONSTRAINT_OPS = {
    "<": lambda a, b: a < b,
    "<=": lambda a, b: a <= b,
    ">": lambda a, b: a > b,
    ">=": lambda a, b: a >= b,
}


def _parse_constraint(name: str, expr: str) -> Tuple[str, str, Any]:
    m = _CONSTRAINT_RE.match(expr)
    if not m:
        raise ValueError(
            f"{name}: unparseable parameter constraint {expr!r}; expected "
            f"'<param> <op> <param-or-number>' with op in {sorted(_CONSTRAINT_OPS)}"
        )
    lhs, op, rhs = m.groups()
    try:
        rhs = float(rhs)
    except ValueError:
        pass
    return lhs, op, rhs


def _effective_params(fn, default_params: dict, args: tuple, kwargs: dict) -> dict:
    effective = dict(default_params)
    try:
        bound = inspect.signature(fn).bind_partial(*args, **kwargs)
    except TypeError:
        return effective
    for pname, value in bound.arguments.items():
        if pname == "df":
            continue
        kind = inspect.signature(fn).parameters[pname].kind
        if kind is inspect.Parameter.VAR_KEYWORD:
            effective.update(value)
        elif kind is not inspect.Parameter.VAR_POSITIONAL:
            effective[pname] = value
    return effective


def _enforce_parsed_constraints(name: str, effective: dict, parsed: list) -> None:
    for expr, (lhs, op, rhs) in parsed:
        if lhs not in effective:
            continue
        a = effective[lhs]
        b = rhs if isinstance(rhs, float) else effective.get(rhs)
        if a is None or b is None:
            continue
        try:
            ok = _CONSTRAINT_OPS[op](float(a), float(b))
        except (TypeError, ValueError):
            raise ValueError(
                f"{name}: invalid parameters: constraint {expr!r} needs "
                f"numeric values (got {lhs}={a!r}"
                + ("" if isinstance(rhs, float) else f", {rhs}={b!r}")
                + ")"
            )
        if not ok:
            raise ValueError(
                f"{name}: invalid parameters: constraint {expr!r} violated "
                f"({lhs}={a!r}"
                + ("" if isinstance(rhs, float) else f", {rhs}={b!r}")
                + ")"
            )


def _validated(name: str, fn, default_params: dict, constraints: Tuple[str, ...]):
    parsed = [(expr, _parse_constraint(name, expr)) for expr in constraints]

    @functools.wraps(fn)
    def wrapper(*args, **kwargs):
        effective = _effective_params(fn, default_params, args, kwargs)
        _enforce_parsed_constraints(name, effective, parsed)
        return fn(*args, **kwargs)

    return wrapper


def validate_params(name: str, params: dict,
                    default_params: Optional[dict] = None) -> None:
    entry = STRATEGIES.get(name)
    if entry is None:
        raise ValueError(
            f"validate_params: unknown strategy {name!r}; "
            f"registered: {sorted(STRATEGIES)}")
    base = entry["default_params"] if default_params is None else default_params
    effective = {**base, **(params or {})}
    parsed = [(expr, _parse_constraint(name, expr)) for expr in entry["constraints"]]
    _enforce_parsed_constraints(name, effective, parsed)
    validator = entry.get("param_validator")
    if validator is not None:
        validator(effective)


def validate_param_value(name: str, param: str, value) -> None:
    entry = STRATEGIES.get(name)
    if entry is None:
        raise ValueError(
            f"validate_param_value: unknown strategy {name!r}; "
            f"registered: {sorted(STRATEGIES)}")
    single = []
    for expr in entry["constraints"]:
        lhs, op, rhs = _parse_constraint(name, expr)
        if lhs == param and isinstance(rhs, float):
            single.append((expr, (lhs, op, rhs)))
    _enforce_parsed_constraints(name, {param: value}, single)
    if name in SINGLE_VALUE_VALIDATED_STRATEGIES:
        entry["param_validator"]({**entry["default_params"], param: value})


def register(
    name: str,
    description: str,
    default_params: dict,
    platforms: Tuple[str, ...] = ("spot", "futures"),
    variants: Optional[Dict[str, Dict[str, Any]]] = None,
    constraints: Optional[List[str]] = None,
    edge_status: Optional[str] = None,
    edge_source: Optional[str] = None,
    edge_ref: Optional[str] = None,
    *,
    short_entries: bool,
    param_validator=None,
    basket=None,
    production_refused: bool = False,
    basket_preflight=None,
):
    if name in STRATEGIES:
        raise ValueError(f"Strategy '{name}' is already registered")
    _validate_edge_metadata(name, edge_status, edge_source, edge_ref)
    if not isinstance(production_refused, bool):
        raise ValueError(f"{name}: production_refused must be True or False, got {production_refused!r}")
    if production_refused and edge_status != EDGE_STATUS_NO_EDGE:
        raise ValueError(f"{name}: production_refused=True requires edge_status={EDGE_STATUS_NO_EDGE!r}")
    platforms = tuple(platforms)
    _validate_basket(name, basket, platforms, short_entries)
    if not platforms:
        raise ValueError(f"{name}: platforms must be non-empty")
    bad = set(platforms) - set(VALID_PLATFORMS)
    if bad:
        raise ValueError(
            f"{name}: unknown platforms {sorted(bad)}; "
            f"expected subset of {VALID_PLATFORMS}"
        )
    variants = variants or {}
    bad_v = set(variants) - set(platforms)
    if bad_v:
        raise ValueError(
            f"{name}: variants keys {sorted(bad_v)} not in platforms {platforms}"
        )
    _validate_short_entries(name, short_entries, default_params, platforms, variants)
    if param_validator is not None and not callable(param_validator):
        raise ValueError(f"{name}: param_validator must be callable, got {param_validator!r}")
    if basket_preflight is not None and basket is None:
        raise ValueError(f"{name}: basket_preflight applies only to a basket evaluator")
    if basket_preflight is not None and not callable(basket_preflight):
        raise ValueError(f"{name}: basket_preflight must be callable, got {basket_preflight!r}")

    constraint_list = tuple(constraints or ())
    known_params = set(default_params)
    for _variant in variants.values():
        known_params |= set(_variant.get("default_params", {}))
    for _expr in constraint_list:
        _lhs, _op, _rhs = _parse_constraint(name, _expr)
        for _pname in (_lhs, _rhs) if not isinstance(_rhs, float) else (_lhs,):
            if _pname not in known_params:
                raise ValueError(
                    f"{name}: constraint {_expr!r} references unknown "
                    f"parameter {_pname!r} (not in default_params)"
                )

    def decorator(fn):
        wrapped = _validated(name, fn, default_params, constraint_list) if constraint_list else fn
        STRATEGIES[name] = {
            "fn": _basket_single_symbol_refusal(name) if basket else wrapped,
            "basket": dict(basket) if basket else None,
            "basket_fn": fn if basket else None,
            "basket_preflight": basket_preflight,
            "description": description,
            "default_params": dict(default_params),
            "constraints": constraint_list,
            "platforms": platforms,
            "variants": variants,
            "edge_status": edge_status,
            "edge_source": edge_source,
            "edge_ref": edge_ref,
            "short_entries": short_entries,
            "param_validator": param_validator,
            "production_refused": production_refused,
        }
        return fn

    return decorator


def build_registry(platform: str, *, include_hidden: bool = False) -> Dict[str, Dict[str, Any]]:
    if platform not in VALID_PLATFORMS:
        raise ValueError(
            f"Unknown platform {platform!r}; expected one of {VALID_PLATFORMS}"
        )
    order = PLATFORM_ORDER[platform]
    tagged = {n for n, e in STRATEGIES.items() if platform in e["platforms"] and not e.get("basket")}
    expected = set(tagged)
    missing_from_order = expected - set(order)
    if missing_from_order:
        raise RuntimeError(
            f"PLATFORM_ORDER[{platform!r}] is missing {sorted(missing_from_order)}"
        )
    extra_in_order = set(order) - expected
    if extra_in_order:
        raise RuntimeError(
            f"PLATFORM_ORDER[{platform!r}] references strategies not tagged for "
            f"{platform!r}: {sorted(extra_in_order)}"
        )

    out: Dict[str, Dict[str, Any]] = {}
    for name in order:
        if not include_hidden and STRATEGIES[name].get("edge_status") == EDGE_STATUS_NO_EDGE:
            continue
        entry = STRATEGIES[name]
        variant = entry["variants"].get(platform, {})
        out[name] = {
            "fn": entry["fn"],
            "description": variant.get("description", entry["description"]),
            "default_params": {
                **entry["default_params"],
                **variant.get("default_params", {}),
            },
            "constraints": entry["constraints"],
            "edge_status": entry.get("edge_status"),
            "edge_source": entry.get("edge_source"),
            "edge_ref": entry.get("edge_ref"),
            "param_validator": entry.get("param_validator"),
            "production_refused": entry.get("production_refused", False),
        }
    return out


@register(
    "sma_crossover",
    "SMA Crossover \u2014 buy when fast SMA crosses above slow SMA",
    {"fast_period": 20, "slow_period": 50},
    constraints=[
        "fast_period > 0",
        "fast_period < slow_period",
    ],
    edge_status="no_edge",
    edge_source="fee_audit_m5",
    edge_ref="docs/research/fee-audit-m5.md",
    short_entries=False,
)
def sma_crossover_strategy(df: pd.DataFrame, fast_period: int = 20, slow_period: int = 50) -> pd.DataFrame:
    result = df.copy()
    result["sma_fast"] = sma(result["close"], fast_period)
    result["sma_slow"] = sma(result["close"], slow_period)
    result["position"] = np.where(result["sma_fast"] > result["sma_slow"], 1, 0)
    result["signal"] = result["position"].diff()
    return result


@register(
    "ema_crossover",
    "EMA Crossover \u2014 faster response than SMA crossover",
    {"fast_period": 12, "slow_period": 26},
    constraints=[
        "fast_period > 0",
        "fast_period < slow_period",
    ],
    edge_status="no_edge",
    edge_source="fee_audit_m5",
    edge_ref="docs/research/fee-audit-m5.md",
    short_entries=False,
)
def ema_crossover_strategy(df: pd.DataFrame, fast_period: int = 12, slow_period: int = 26) -> pd.DataFrame:
    result = df.copy()
    result["ema_fast"] = ema(result["close"], fast_period)
    result["ema_slow"] = ema(result["close"], slow_period)
    result["position"] = np.where(result["ema_fast"] > result["ema_slow"], 1, 0)
    result["signal"] = result["position"].diff()
    return result


@register(
    "rsi",
    "RSI \u2014 buy at oversold, sell at overbought",
    {"period": 14, "overbought": 70, "oversold": 30},
    variants={
        "futures": {"description": "RSI \u2014 overbought/oversold signals for futures"},
    },
    constraints=[
        "period > 0",
        "oversold < overbought",
    ],
    edge_status="no_edge",
    edge_source="fee_audit_m5",
    edge_ref="docs/research/fee-audit-m5.md",
    short_entries=False,
)
def rsi_strategy(df: pd.DataFrame, period: int = 14, overbought: float = 70, oversold: float = 30) -> pd.DataFrame:
    result = df.copy()
    result["rsi"] = wilder_rsi(result["close"], period)
    result["signal"] = 0
    result.loc[(result["rsi"] > oversold) & (result["rsi"].shift(1) <= oversold), "signal"] = 1
    result.loc[(result["rsi"] < overbought) & (result["rsi"].shift(1) >= overbought), "signal"] = -1
    return result


@register(
    "bollinger_bands",
    "Bollinger Bands \u2014 mean reversion at band touches",
    {"period": 20, "num_std": 2.0},
    constraints=["period > 0"],
    edge_status="no_edge",
    edge_source="fee_audit_m5",
    edge_ref="docs/research/fee-audit-m5.md",
    short_entries=False,
)
def bollinger_strategy(df: pd.DataFrame, period: int = 20, num_std: float = 2.0) -> pd.DataFrame:
    result = df.copy()
    result["bb_middle"] = sma(result["close"], period)
    rolling_std = result["close"].rolling(window=period).std()
    result["bb_upper"] = result["bb_middle"] + (rolling_std * num_std)
    result["bb_lower"] = result["bb_middle"] - (rolling_std * num_std)
    result["signal"] = 0
    result.loc[(result["close"] > result["bb_lower"]) & (result["close"].shift(1) <= result["bb_lower"].shift(1)), "signal"] = 1
    result.loc[(result["close"] < result["bb_upper"]) & (result["close"].shift(1) >= result["bb_upper"].shift(1)), "signal"] = -1
    return result


@register(
    "macd",
    "MACD \u2014 buy/sell on MACD line crossing signal line",
    {"fast_period": 12, "slow_period": 26, "signal_period": 9},
    variants={
        "futures": {"description": "MACD \u2014 momentum crossover for futures"},
    },
    constraints=[
        "fast_period > 0",
        "signal_period > 0",
        "fast_period < slow_period",
    ],
    edge_status="no_edge",
    edge_source="fee_audit_m5",
    edge_ref="docs/research/fee-audit-m5.md",
    short_entries=False,
)
def macd_strategy(df: pd.DataFrame, fast_period: int = 12, slow_period: int = 26, signal_period: int = 9) -> pd.DataFrame:
    result = df.copy()
    ema_fast = ema(result["close"], fast_period)
    ema_slow = ema(result["close"], slow_period)
    result["macd_line"] = ema_fast - ema_slow
    result["macd_signal"] = ema(result["macd_line"], signal_period)
    result["macd_hist"] = result["macd_line"] - result["macd_signal"]
    result["position"] = np.where(result["macd_line"] > result["macd_signal"], 1, 0)
    result["signal"] = result["position"].diff()
    return result


@register(
    "mean_reversion",
    "Mean Reversion \u2014 buy when price is N std below mean, sell when above",
    {"lookback": 30, "entry_std": 1.5, "exit_std": 0.5},
    variants={
        "futures": {"description": "Mean Reversion \u2014 range-bound index futures trading"},
    },
    constraints=[
        "lookback > 0",
        "exit_std < entry_std",
    ],
    edge_status="no_edge",
    edge_source="fee_audit_m5",
    edge_ref="docs/research/fee-audit-m5.md",
    short_entries=False,
)
def mean_reversion_strategy(df: pd.DataFrame, lookback: int = 30, entry_std: float = 1.5, exit_std: float = 0.5) -> pd.DataFrame:
    result = df.copy()
    result["rolling_mean"] = result["close"].rolling(window=lookback).mean()
    result["rolling_std"] = result["close"].rolling(window=lookback).std()
    result["z_score"] = (result["close"] - result["rolling_mean"]) / result["rolling_std"]
    result["signal"] = 0
    result.loc[(result["z_score"] > -entry_std) & (result["z_score"].shift(1) <= -entry_std), "signal"] = 1
    result.loc[(result["z_score"] < exit_std) & (result["z_score"].shift(1) >= exit_std), "signal"] = -1
    return result


@register(
    "momentum",
    "Momentum \u2014 buy on strong upward momentum, sell on reversal",
    {"roc_period": 14, "threshold": 5.0},
    variants={
        "futures": {
            "description": "Momentum \u2014 trend following on futures using rate of change",
            "default_params": {"threshold": 3.0},
        },
    },
    constraints=["roc_period > 0"],
    edge_status="no_edge",
    edge_source="fee_audit_m5",
    edge_ref="docs/research/fee-audit-m5.md",
    short_entries=False,
)
def momentum_strategy(df: pd.DataFrame, roc_period: int = 14, threshold: float = 5.0) -> pd.DataFrame:
    result = df.copy()
    result["roc"] = ((result["close"] - result["close"].shift(roc_period)) / result["close"].shift(roc_period)) * 100
    result["signal"] = 0
    result.loc[(result["roc"] > threshold) & (result["roc"].shift(1) <= threshold), "signal"] = 1
    result.loc[(result["roc"] < -threshold) & (result["roc"].shift(1) >= -threshold), "signal"] = -1
    return result


@register(
    "volume_weighted",
    "Volume-Weighted \u2014 confirms trend with volume analysis",
    {"sma_period": 20, "vol_multiplier": 1.5},
    constraints=["sma_period > 0"],
    edge_status="no_edge",
    edge_source="fee_audit_m5",
    edge_ref="docs/research/fee-audit-m5.md",
    short_entries=False,
)
def volume_weighted_strategy(df: pd.DataFrame, sma_period: int = 20, vol_multiplier: float = 1.5) -> pd.DataFrame:
    result = df.copy()
    result["price_sma"] = sma(result["close"], sma_period)
    result["vol_sma"] = sma(result["volume"], sma_period)
    result["high_volume"] = result["volume"] > (result["vol_sma"] * vol_multiplier)
    result["signal"] = 0
    price_cross_up = (result["close"] > result["price_sma"]) & (result["close"].shift(1) <= result["price_sma"].shift(1))
    result.loc[price_cross_up & result["high_volume"], "signal"] = 1
    price_cross_down = (result["close"] < result["price_sma"]) & (result["close"].shift(1) >= result["price_sma"].shift(1))
    result.loc[price_cross_down & result["high_volume"], "signal"] = -1
    return result


@register(
    "triple_ema",
    "Triple EMA \u2014 trend confirmation using 3 EMAs (short/mid/long)",
    {"short_period": 8, "mid_period": 21, "long_period": 55},
    constraints=[
        "short_period > 0",
        "short_period < mid_period",
        "mid_period < long_period",
    ],
    edge_status="no_edge",
    edge_source="fee_audit_m5",
    edge_ref="docs/research/fee-audit-m5.md",
    short_entries=False,
)
def triple_ema_strategy(df: pd.DataFrame, short_period: int = 8, mid_period: int = 21, long_period: int = 55) -> pd.DataFrame:
    result = df.copy()
    result["ema_short"] = ema(result["close"], short_period)
    result["ema_mid"] = ema(result["close"], mid_period)
    result["ema_long"] = ema(result["close"], long_period)
    bullish = (result["ema_short"] > result["ema_mid"]) & (result["ema_mid"] > result["ema_long"])
    result["position"] = np.where(bullish, 1, 0)
    result["signal"] = result["position"].diff()
    return result


@register(
    "triple_ema_bidir",
    "Triple EMA Bidirectional \u2014 long on bullish stack, short on bearish stack",
    {"short_period": 8, "mid_period": 21, "long_period": 55},
    platforms=("futures",),
    constraints=[
        "short_period > 0",
        "short_period < mid_period",
        "mid_period < long_period",
    ],
    edge_status="no_edge",
    edge_source="fee_audit_m5",
    edge_ref="docs/research/1282-m5-limbo-verdicts.md",
    short_entries=True,
)
def triple_ema_bidir_strategy(df: pd.DataFrame, short_period: int = 8, mid_period: int = 21, long_period: int = 55) -> pd.DataFrame:
    result = df.copy()
    result["ema_short"] = ema(result["close"], short_period)
    result["ema_mid"] = ema(result["close"], mid_period)
    result["ema_long"] = ema(result["close"], long_period)
    bullish = (result["ema_short"] > result["ema_mid"]) & (result["ema_mid"] > result["ema_long"])
    bearish = (result["ema_short"] < result["ema_mid"]) & (result["ema_mid"] < result["ema_long"])
    result["position"] = np.where(bullish, 1, np.where(bearish, -1, 0))
    result["signal"] = result["position"].diff().clip(-1, 1)
    return result


@register(
    "tema_cross",
    "Triple EMA Crossover — EMA fast/mid cross with long EMA trend filter",
    {"short_period": 5, "mid_period": 13, "long_period": 34},
    constraints=[
        "short_period > 0",
        "short_period < mid_period",
        "mid_period < long_period",
    ],
    edge_status="no_edge",
    edge_source="fee_audit_m5",
    edge_ref="docs/research/1282-m5-limbo-verdicts.md",
    short_entries=False,
)
def tema_cross_strategy(df: pd.DataFrame, short_period: int = 5, mid_period: int = 13, long_period: int = 34) -> pd.DataFrame:
    result = df.copy()
    result["ema_short"] = ema(result["close"], short_period)
    result["ema_mid"] = ema(result["close"], mid_period)
    result["ema_long"] = ema(result["close"], long_period)
    uptrend = result["ema_mid"] > result["ema_long"]
    bullish_cross = (result["ema_short"] > result["ema_mid"]) & (
        result["ema_short"].shift(1) <= result["ema_mid"].shift(1)
    )
    bearish_cross = (result["ema_short"] < result["ema_mid"]) & (
        result["ema_short"].shift(1) >= result["ema_mid"].shift(1)
    )
    raw = pd.Series(np.nan, index=result.index)
    raw[uptrend & bullish_cross] = 1
    raw[bearish_cross] = 0
    result["position"] = raw.ffill().fillna(0).astype(int)
    result["signal"] = result["position"].diff().fillna(0).astype(int)
    return result


@register(
    "tema_cross_bd",
    "Triple EMA Crossover Bidirectional — long/short on cross with trend filter",
    {"short_period": 5, "mid_period": 13, "long_period": 34},
    platforms=("futures",),
    constraints=[
        "short_period > 0",
        "short_period < mid_period",
        "mid_period < long_period",
    ],
    edge_status="no_edge",
    edge_source="fee_audit_m5",
    edge_ref="docs/research/1282-m5-limbo-verdicts.md",
    short_entries=True,
)
def tema_cross_bd_strategy(df: pd.DataFrame, short_period: int = 5, mid_period: int = 13, long_period: int = 34) -> pd.DataFrame:
    result = df.copy()
    result["ema_short"] = ema(result["close"], short_period)
    result["ema_mid"] = ema(result["close"], mid_period)
    result["ema_long"] = ema(result["close"], long_period)
    uptrend = result["ema_mid"] > result["ema_long"]
    downtrend = result["ema_mid"] < result["ema_long"]
    bullish_cross = (result["ema_short"] > result["ema_mid"]) & (
        result["ema_short"].shift(1) <= result["ema_mid"].shift(1)
    )
    bearish_cross = (result["ema_short"] < result["ema_mid"]) & (
        result["ema_short"].shift(1) >= result["ema_mid"].shift(1)
    )
    raw = pd.Series(np.nan, index=result.index)
    raw[uptrend & bullish_cross] = 1
    raw[downtrend & bearish_cross] = -1
    raw[(~uptrend) & bullish_cross] = 0
    raw[(~downtrend) & bearish_cross] = 0
    result["position"] = raw.ffill().fillna(0).astype(int)
    result["signal"] = result["position"].diff().fillna(0).clip(-1, 1).astype(int)
    return result


@register(
    "rsi_macd_combo",
    "RSI+MACD Combo \u2014 dual confirmation for higher quality signals",
    {"rsi_period": 14, "rsi_oversold": 35, "rsi_overbought": 65,
     "macd_fast": 12, "macd_slow": 26, "macd_signal": 9,
     "rsi_short_min": 50, "rsi_long_max": 50},
    constraints=[
        "rsi_period > 0",
        "macd_fast > 0",
        "macd_signal > 0",
        "macd_fast < macd_slow",
        "rsi_oversold < rsi_overbought",
    ],
    edge_status="no_edge",
    edge_source="fee_audit_m5",
    edge_ref="docs/research/fee-audit-m5.md",
    short_entries=False,
)
def rsi_macd_combo_strategy(df: pd.DataFrame,
                             rsi_period: int = 14, rsi_oversold: float = 35, rsi_overbought: float = 65,
                             macd_fast: int = 12, macd_slow: int = 26, macd_signal: int = 9,
                             rsi_short_min: float = 50, rsi_long_max: float = 50) -> pd.DataFrame:
    result = df.copy()
    result["rsi"] = wilder_rsi(result["close"], rsi_period)
    ema_fast = ema(result["close"], macd_fast)
    ema_slow = ema(result["close"], macd_slow)
    result["macd_line"] = ema_fast - ema_slow
    result["macd_signal_line"] = ema(result["macd_line"], macd_signal)
    result["signal"] = 0
    macd_bull = (result["macd_line"] > result["macd_signal_line"]) & (result["macd_line"].shift(1) <= result["macd_signal_line"].shift(1))
    rsi_ok = result["rsi"] < rsi_long_max
    result.loc[macd_bull & rsi_ok, "signal"] = 1
    macd_bear = (result["macd_line"] < result["macd_signal_line"]) & (result["macd_line"].shift(1) >= result["macd_signal_line"].shift(1))
    rsi_high = result["rsi"] > rsi_short_min
    result.loc[macd_bear & rsi_high, "signal"] = -1
    return result


@register(
    "stoch_rsi",
    "Stochastic RSI \u2014 earlier momentum signals via stochastic oscillator on RSI",
    {"rsi_period": 14, "stoch_period": 14, "k_smooth": 3, "d_smooth": 3,
     "overbought": 80, "oversold": 20},
    constraints=[
        "rsi_period > 0",
        "stoch_period > 0",
        "k_smooth > 0",
        "d_smooth > 0",
        "oversold < overbought",
    ],
    edge_status="no_edge",
    edge_source="fee_audit_m5",
    edge_ref="docs/research/fee-audit-m5.md",
    short_entries=False,
)
def stoch_rsi_strategy(df: pd.DataFrame,
                       rsi_period: int = 14, stoch_period: int = 14,
                       k_smooth: int = 3, d_smooth: int = 3,
                       overbought: float = 80, oversold: float = 20) -> pd.DataFrame:
    result = df.copy()
    result["rsi"] = wilder_rsi(result["close"], rsi_period)
    rsi_min = result["rsi"].rolling(window=stoch_period).min()
    rsi_max = result["rsi"].rolling(window=stoch_period).max()
    stoch_rsi = (result["rsi"] - rsi_min) / (rsi_max - rsi_min) * 100
    result["stoch_k"] = stoch_rsi.rolling(window=k_smooth).mean()
    result["stoch_d"] = result["stoch_k"].rolling(window=d_smooth).mean()
    result["signal"] = 0
    k_cross_up = (result["stoch_k"] > result["stoch_d"]) & (result["stoch_k"].shift(1) <= result["stoch_d"].shift(1))
    k_cross_down = (result["stoch_k"] < result["stoch_d"]) & (result["stoch_k"].shift(1) >= result["stoch_d"].shift(1))
    result.loc[k_cross_up & (result["stoch_k"] < oversold), "signal"] = 1
    result.loc[k_cross_down & (result["stoch_k"] > overbought), "signal"] = -1
    return result


@register(
    "supertrend",
    "Supertrend \u2014 ATR-based trend following with dynamic support/resistance",
    {"atr_period": 10, "multiplier": 3.0},
    constraints=["atr_period > 0"],
    edge_status="no_edge",
    edge_source="fee_audit_m5",
    edge_ref="docs/research/fee-audit-m5.md",
    short_entries=False,
)
def supertrend_strategy(df: pd.DataFrame, atr_period: int = 10, multiplier: float = 3.0) -> pd.DataFrame:
    result = df.copy()
    atr = atr_sma(result, atr_period, round_large=False)

    hl2 = (result["high"] + result["low"]) / 2
    basic_upper = hl2 + (multiplier * atr)
    basic_lower = hl2 - (multiplier * atr)

    n = len(result)
    final_upper = basic_upper.copy()
    final_lower = basic_lower.copy()
    direction = pd.Series(0, index=result.index, dtype=int)

    atr_valid = atr.notna().to_numpy()
    if not atr_valid.any():
        result["supertrend"] = np.nan
        result["st_direction"] = direction
        result["signal"] = 0
        return result
    start = int(atr_valid.argmax())

    for i in range(start + 1, n):
        if basic_upper.iloc[i] < final_upper.iloc[i-1] or result["close"].iloc[i-1] > final_upper.iloc[i-1]:
            final_upper.iloc[i] = basic_upper.iloc[i]
        else:
            final_upper.iloc[i] = final_upper.iloc[i-1]

        if basic_lower.iloc[i] > final_lower.iloc[i-1] or result["close"].iloc[i-1] < final_lower.iloc[i-1]:
            final_lower.iloc[i] = basic_lower.iloc[i]
        else:
            final_lower.iloc[i] = final_lower.iloc[i-1]

        prev_dir = direction.iloc[i-1]
        if prev_dir <= 0:
            direction.iloc[i] = 1 if result["close"].iloc[i] > final_upper.iloc[i] else -1
        else:
            direction.iloc[i] = -1 if result["close"].iloc[i] < final_lower.iloc[i] else 1

    result["supertrend"] = np.where(direction == 1, final_lower, final_upper)
    result["st_direction"] = direction
    result["signal"] = 0
    dir_series = pd.Series(direction.values, index=result.index)
    result.loc[(dir_series == 1) & (dir_series.shift(1) == -1), "signal"] = 1
    result.loc[(dir_series == -1) & (dir_series.shift(1) == 1), "signal"] = -1
    return result


@register(
    "ichimoku_cloud",
    "Ichimoku Cloud \u2014 trend confirmation via Tenkan/Kijun cross, cloud position, and Chikou span",
    {"tenkan_period": 9, "kijun_period": 26, "senkou_b_period": 52},
    constraints=[
        "tenkan_period > 0",
        "kijun_period > 0",
        "senkou_b_period > 0",
    ],
    edge_status="no_edge",
    edge_source="fee_audit_m5",
    edge_ref="docs/research/fee-audit-m5.md",
    short_entries=False,
)
def ichimoku_cloud_strategy(df: pd.DataFrame, tenkan_period: int = 9, kijun_period: int = 26, senkou_b_period: int = 52) -> pd.DataFrame:
    result = df.copy()
    high, low, close = result["high"], result["low"], result["close"]

    tenkan = (high.rolling(window=tenkan_period).max() + low.rolling(window=tenkan_period).min()) / 2
    kijun = (high.rolling(window=kijun_period).max() + low.rolling(window=kijun_period).min()) / 2
    senkou_a = (tenkan + kijun) / 2
    senkou_b = (high.rolling(window=senkou_b_period).max() + low.rolling(window=senkou_b_period).min()) / 2

    result["tenkan"] = tenkan
    result["kijun"] = kijun
    result["senkou_a"] = senkou_a
    result["senkou_b"] = senkou_b

    cloud_top = np.maximum(senkou_a, senkou_b)
    cloud_bottom = np.minimum(senkou_a, senkou_b)
    above_cloud = close > cloud_top
    below_cloud = close < cloud_bottom
    tk_cross_up = (tenkan > kijun) & (tenkan.shift(1) <= kijun.shift(1))
    tk_cross_down = (tenkan < kijun) & (tenkan.shift(1) >= kijun.shift(1))
    chikou_bull = close > close.shift(kijun_period)
    chikou_bear = close < close.shift(kijun_period)

    result["signal"] = 0
    result.loc[above_cloud & tk_cross_up & chikou_bull, "signal"] = 1
    result.loc[below_cloud & tk_cross_down & chikou_bear, "signal"] = -1
    return result


@register(
    "pairs_spread",
    "Pairs/Spread Trading \u2014 trade z-score of price ratio between two assets (needs 'close_b' column)",
    {"lookback": 30, "entry_z": 2.0, "exit_z": 0.5},
    platforms=("spot",),
    constraints=[
        "lookback > 0",
        "exit_z < entry_z",
    ],
    edge_status="no_edge",
    edge_source="fee_audit_m5",
    edge_ref="docs/research/fee-audit-m5.md",
    short_entries=False,
)
def pairs_spread_strategy(df: pd.DataFrame, lookback: int = 30, entry_z: float = 2.0, exit_z: float = 0.5) -> pd.DataFrame:
    result = df.copy()
    if "close_b" in result.columns:
        result["spread"] = result["close"] / result["close_b"]
    else:
        result["spread"] = result["close"]

    result["spread_mean"] = result["spread"].rolling(window=lookback).mean()
    result["spread_std"] = result["spread"].rolling(window=lookback).std()
    result["z_score"] = (result["spread"] - result["spread_mean"]) / result["spread_std"]
    result["signal"] = 0
    result.loc[(result["z_score"] > -entry_z) & (result["z_score"].shift(1) <= -entry_z), "signal"] = 1
    result.loc[(result["z_score"] < exit_z) & (result["z_score"].shift(1) >= exit_z), "signal"] = -1
    return result


@register(
    "squeeze_momentum",
    "Squeeze Momentum \u2014 BB inside KC detects coiling, trades breakout with momentum confirmation",
    {"bb_period": 20, "bb_std": 2.0, "kc_period": 20, "kc_mult": 1.5, "mom_lookback": 12},
    constraints=[
        "bb_period > 0",
        "kc_period > 0",
        "mom_lookback > 0",
    ],
    edge_status="no_edge",
    edge_source="fee_audit_m5",
    edge_ref="docs/research/fee-audit-m5.md",
    short_entries=False,
)
def squeeze_momentum_strategy(df: pd.DataFrame,
                              bb_period: int = 20, bb_std: float = 2.0,
                              kc_period: int = 20, kc_mult: float = 1.5,
                              mom_lookback: int = 12) -> pd.DataFrame:
    result = df.copy()
    bb_mid = sma(result["close"], bb_period)
    bb_stddev = result["close"].rolling(window=bb_period).std()
    bb_upper = bb_mid + (bb_std * bb_stddev)
    bb_lower = bb_mid - (bb_std * bb_stddev)
    kc_mid = ema(result["close"], kc_period)
    atr = atr_sma(result, kc_period, round_large=False)
    kc_upper = kc_mid + (kc_mult * atr)
    kc_lower = kc_mid - (kc_mult * atr)
    result["squeeze_on"] = (bb_lower > kc_lower) & (bb_upper < kc_upper)
    highest_high = result["high"].rolling(window=kc_period).max()
    lowest_low = result["low"].rolling(window=kc_period).min()
    midline = ((highest_high + lowest_low) / 2 + bb_mid) / 2
    delta = result["close"] - midline
    x = np.arange(mom_lookback, dtype=float)
    x_mean = x.mean()
    x_var = ((x - x_mean) ** 2).sum()
    def _linreg_last(window):
        if len(window) < mom_lookback or np.isnan(window).any():
            return np.nan
        slope = ((x - x_mean) * (window - window.mean())).sum() / x_var
        return slope * (mom_lookback - 1 - x_mean) + window.mean()
    result["squeeze_mom"] = delta.rolling(window=mom_lookback).apply(_linreg_last, raw=True)
    squeeze_fired = (~result["squeeze_on"]) & (result["squeeze_on"].shift(1) == True)
    mom_pos_rising = (result["squeeze_mom"] > 0) & (result["squeeze_mom"] > result["squeeze_mom"].shift(1))
    mom_neg_falling = (result["squeeze_mom"] < 0) & (result["squeeze_mom"] < result["squeeze_mom"].shift(1))
    result["signal"] = 0
    result.loc[squeeze_fired & mom_pos_rising, "signal"] = 1
    result.loc[squeeze_fired & mom_neg_falling, "signal"] = -1
    return result


@register(
    "breakout",
    "Breakout \u2014 trade breakouts from overnight/session range",
    {"lookback": 20, "atr_period": 14, "atr_multiplier": 1.5},
    platforms=("futures",),
    constraints=[
        "lookback > 0",
        "atr_period > 0",
    ],
    short_entries=False,
)
def breakout_strategy(df: pd.DataFrame, lookback: int = 20, atr_period: int = 14, atr_multiplier: float = 1.5) -> pd.DataFrame:
    result = df.copy()
    result["high_roll"] = result["high"].rolling(window=lookback).max()
    result["low_roll"] = result["low"].rolling(window=lookback).min()
    tr = true_range(result)
    result["atr"] = atr_from_true_range(tr, atr_period)
    result["signal"] = 0
    breakout_up = (result["close"] > result["high_roll"].shift(1)) & (tr > result["atr"] * atr_multiplier)
    result.loc[breakout_up & ~breakout_up.shift(1, fill_value=False), "signal"] = 1
    breakout_down = (result["close"] < result["low_roll"].shift(1)) & (tr > result["atr"] * atr_multiplier)
    result.loc[breakout_down & ~breakout_down.shift(1, fill_value=False), "signal"] = -1
    return result


@register(
    "atr_breakout",
    "ATR Breakout \u2014 enter on volatility breakout beyond ATR band",
    {"atr_period": 14, "multiplier": 1.5},
    constraints=["atr_period > 0"],
    edge_status="no_edge",
    edge_source="fee_audit_m5",
    edge_ref="docs/research/fee-audit-m5.md",
    short_entries=False,
)
def atr_breakout_strategy(df: pd.DataFrame, atr_period: int = 14, multiplier: float = 1.5) -> pd.DataFrame:
    result = df.copy()
    result["atr"] = atr_sma(result, atr_period)
    prev_close = result["close"].shift(1)
    upper = prev_close + (multiplier * result["atr"])
    lower = prev_close - (multiplier * result["atr"])
    result["signal"] = 0
    result.loc[(result["close"] > upper) & (result["close"].shift(1) <= upper.shift(1)), "signal"] = 1
    result.loc[(result["close"] < lower) & (result["close"].shift(1) >= lower.shift(1)), "signal"] = -1
    return result


@register(
    "amd_ifvg",
    "AMD+IFVG \u2014 ICT Accumulation-Manipulation-Distribution with Implied Fair Value Gap (15m, session-aware)",
    {
        "asian_start_hour": 20, "asian_end_hour": 0,
        "london_start_hour": 2, "london_end_hour": 5,
        "min_ifvg_pct": 0.05, "sweep_threshold_pct": 0.01,
        "session_tz": "America/New_York",
    },
    edge_status="no_edge",
    edge_source="study_fail",
    edge_ref="docs/research/amd_ifvg_1023.md",
    short_entries=False,
)
def amd_ifvg_strategy(df: pd.DataFrame, **params) -> pd.DataFrame:
    return amd_ifvg_core(df, **params)


@register(
    "heikin_ashi_ema",
    "Heikin Ashi + EMA \u2014 smoothed candles with EMA trend filter; 2 consecutive HA candles + price side of EMA",
    {"ema_period": 21, "confirmation": 2},
    constraints=["ema_period > 0"],
    edge_status="no_edge",
    edge_source="fee_audit_m5",
    edge_ref="docs/research/fee-audit-m5.md",
    short_entries=False,
)
def heikin_ashi_ema_strategy(df: pd.DataFrame, ema_period: int = 21, confirmation: int = 2) -> pd.DataFrame:
    result = df.copy()
    ha_close = (result["open"] + result["high"] + result["low"] + result["close"]) / 4
    ha_open = ha_close.copy()
    for i in range(1, len(result)):
        ha_open.iloc[i] = (ha_open.iloc[i - 1] + ha_close.iloc[i - 1]) / 2
    ha_high = pd.concat([result["high"], ha_open, ha_close], axis=1).max(axis=1)
    ha_low = pd.concat([result["low"], ha_open, ha_close], axis=1).min(axis=1)
    result["ha_open"] = ha_open
    result["ha_close"] = ha_close
    result["ha_high"] = ha_high
    result["ha_low"] = ha_low
    result["ha_ema"] = ema(ha_close, ema_period)
    result["ha_bullish"] = (ha_close > ha_open) & (ha_low == ha_open)
    result["ha_bearish"] = (ha_close < ha_open) & (ha_high == ha_open)
    bull_streak = result["ha_bullish"].rolling(window=confirmation).sum() == confirmation
    bear_streak = result["ha_bearish"].rolling(window=confirmation).sum() == confirmation
    above_ema = ha_close > result["ha_ema"]
    below_ema = ha_close < result["ha_ema"]
    result["signal"] = 0
    buy_cond = bull_streak & above_ema
    sell_cond = bear_streak & below_ema
    result.loc[buy_cond & ~buy_cond.shift(1, fill_value=False), "signal"] = 1
    result.loc[sell_cond & ~sell_cond.shift(1, fill_value=False), "signal"] = -1
    return result


@register(
    "order_blocks",
    "Order Blocks (ICT/SMC) \u2014 institutional supply/demand zones from displacement candles",
    {"atr_period": 14, "displacement_mult": 1.5, "ob_lookback": 20, "max_ob_age": 50},
    constraints=[
        "atr_period > 0",
        "ob_lookback > 0",
    ],
    edge_status="no_edge",
    edge_source="fee_audit_m5",
    edge_ref="docs/research/fee-audit-m5.md",
    short_entries=False,
)
def order_blocks_strategy(df: pd.DataFrame,
                          atr_period: int = 14, displacement_mult: float = 1.5,
                          ob_lookback: int = 20, max_ob_age: int = 50) -> pd.DataFrame:
    result = df.copy()
    close = result["close"].values
    high = result["high"].values
    low = result["low"].values
    opn = result["open"].values
    n = len(result)

    atr = atr_sma(result, atr_period, round_large=False).values

    signal = np.zeros(n, dtype=int)

    active_obs = []

    for i in range(1, n):
        if np.isnan(atr[i]):
            continue

        body = abs(close[i] - opn[i])
        threshold = displacement_mult * atr[i]

        if body > threshold:
            bullish_displacement = close[i] > opn[i]

            for j in range(i - 1, max(i - ob_lookback - 1, 0) - 1, -1):
                if bullish_displacement and close[j] < opn[j]:
                    active_obs.append(("bull", high[j], low[j], i, False))
                    break
                elif not bullish_displacement and close[j] > opn[j]:
                    active_obs.append(("bear", high[j], low[j], i, False))
                    break

        new_obs = []
        for ob_type, ob_high, ob_low, birth, touched in active_obs:
            age = i - birth
            if age > max_ob_age:
                continue

            if ob_type == "bull":
                if close[i] < ob_low:
                    continue
                if low[i] <= ob_high and not touched:
                    signal[i] = 1
                    new_obs.append((ob_type, ob_high, ob_low, birth, True))
                    continue
            else:
                if close[i] > ob_high:
                    continue
                if high[i] >= ob_low and not touched:
                    signal[i] = -1
                    new_obs.append((ob_type, ob_high, ob_low, birth, True))
                    continue

            new_obs.append((ob_type, ob_high, ob_low, birth, touched))
        active_obs = new_obs

    result["signal"] = signal
    return result


@register(
    "vwap_reversion",
    "VWAP Reversion \u2014 buy when price drops below VWAP by N std devs, sell when above",
    {"entry_std": 1.5, "exit_std": 0.2},
    constraints=["exit_std < entry_std"],
    edge_status="no_edge",
    edge_source="fee_audit_m5",
    edge_ref="docs/research/fee-audit-m5.md",
    short_entries=False,
)
def vwap_reversion_strategy(df: pd.DataFrame, entry_std: float = 1.5, exit_std: float = 0.2) -> pd.DataFrame:
    result = df.copy()
    if result.empty:
        result["signal"] = 0
        return result
    if isinstance(result.index, pd.DatetimeIndex):
        day = result.index.date
    else:
        day = pd.to_datetime(result.index).date
    result["_day"] = day
    typical_price = (result["high"] + result["low"] + result["close"]) / 3
    result["_tp_vol"] = typical_price * result["volume"]
    result["_cum_tp_vol"] = result.groupby("_day")["_tp_vol"].cumsum()
    result["_cum_vol"] = result.groupby("_day")["volume"].cumsum()
    result["vwap"] = result["_cum_tp_vol"] / result["_cum_vol"]
    result["vwap_std"] = result.groupby("_day")["close"].transform(
        lambda x: (x - result.loc[x.index, "vwap"]).expanding().std()
    )
    result["vwap_std"] = result["vwap_std"].fillna(0)
    result["signal"] = 0
    lower = result["vwap"] - entry_std * result["vwap_std"]
    upper = result["vwap"] + entry_std * result["vwap_std"]
    buy_cross = (result["close"] < lower) & (result["close"].shift(1) >= lower.shift(1))
    sell_cross = (result["close"] > upper) & (result["close"].shift(1) <= upper.shift(1))
    result.loc[buy_cross, "signal"] = 1
    result.loc[sell_cross, "signal"] = -1
    result.drop(columns=["_day", "_tp_vol", "_cum_tp_vol", "_cum_vol"], inplace=True)
    return result


@register(
    "chart_pattern",
    "Chart Pattern \u2014 detects Double Top/Bottom, H&S, Flags, Triangles with volume confirmation",
    {
        "pivot_lookback": 5, "tolerance": 0.03, "vol_multiplier": 1.5,
        "vol_period": 20,
        "htf_gate_factor": 0, "htf_gate_mode": "veto",
        "htf_gate_ema_fast": 20, "htf_gate_ema_slow": 40,
    },
    constraints=[
        "pivot_lookback > 0",
        "vol_period > 0",
    ],
    short_entries=True,
)
def chart_pattern_strategy(df: pd.DataFrame, **params) -> pd.DataFrame:
    return chart_pattern_core(df, **params)


@register(
    "liquidity_sweeps",
    "Liquidity Sweeps (ICT) \u2014 fades stop-hunt wicks beyond swing highs/lows after price closes back inside range",
    {"swing_lookback": 20, "confirmation": 1},
    constraints=["swing_lookback > 0"],
    short_entries=True,
)
def liquidity_sweeps_strategy(df: pd.DataFrame, **params) -> pd.DataFrame:
    return liquidity_sweep_core(df, **params)


@register(
    "parabolic_sar",
    "Parabolic SAR \u2014 trend-following stop and reverse with accelerating trailing stop",
    {"iaf": 0.02, "af_step": 0.02, "max_af": 0.2},
    edge_status="no_edge",
    edge_source="fee_audit_m5",
    edge_ref="docs/research/fee-audit-m5.md",
    short_entries=False,
)
def parabolic_sar_strategy(df: pd.DataFrame, iaf: float = 0.02, af_step: float = 0.02, max_af: float = 0.2) -> pd.DataFrame:
    result = df.copy()
    high = result["high"].values
    low = result["low"].values
    close = result["close"].values
    n = len(close)
    sar = np.zeros(n)
    trend = np.zeros(n, dtype=int)
    af = np.zeros(n)
    ep = np.zeros(n)

    if n < 2:
        result["sar"] = np.nan
        result["signal"] = 0
        return result

    trend[0] = 1
    if trend[0] == 1:
        sar[0] = low[0]
        ep[0] = high[0]
    else:
        sar[0] = high[0]
        ep[0] = low[0]
    af[0] = iaf

    for i in range(1, n):
        prev_sar = sar[i - 1]
        prev_af = af[i - 1]
        prev_ep = ep[i - 1]
        prev_trend = trend[i - 1]

        new_sar = prev_sar + prev_af * (prev_ep - prev_sar)

        if prev_trend == 1:
            new_sar = min(new_sar, low[i - 1])
            if i >= 2:
                new_sar = min(new_sar, low[i - 2])
        else:
            new_sar = max(new_sar, high[i - 1])
            if i >= 2:
                new_sar = max(new_sar, high[i - 2])

        if prev_trend == 1 and low[i] < new_sar:
            trend[i] = -1
            sar[i] = prev_ep
            ep[i] = low[i]
            af[i] = iaf
        elif prev_trend == -1 and high[i] > new_sar:
            trend[i] = 1
            sar[i] = prev_ep
            ep[i] = high[i]
            af[i] = iaf
        else:
            trend[i] = prev_trend
            sar[i] = new_sar
            if prev_trend == 1:
                ep[i] = max(prev_ep, high[i])
            else:
                ep[i] = min(prev_ep, low[i])
            if ep[i] != prev_ep:
                af[i] = min(prev_af + af_step, max_af)
            else:
                af[i] = prev_af

    result["sar"] = sar
    result["signal"] = 0
    trend_series = pd.Series(trend, index=result.index)
    buy = (trend_series == 1) & (trend_series.shift(1) == -1)
    sell = (trend_series == -1) & (trend_series.shift(1) == 1)
    result.loc[buy, "signal"] = 1
    result.loc[sell, "signal"] = -1
    return result


@register(
    "range_scalper",
    "Range Scalper \u2014 detects low-volatility consolidation via Bollinger bandwidth + volume, then mean-reverts at band touches",
    {"bb_period": 14, "bb_std": 1.5, "bw_threshold": 0.008, "vol_ratio": 0.8, "rsi_period": 7, "rsi_ob": 70, "rsi_os": 30},
    constraints=[
        "bb_period > 0",
        "rsi_period > 0",
        "rsi_os < rsi_ob",
    ],
    edge_status="no_edge",
    edge_source="study_fail",
    edge_ref="docs/research/range_scalper_987.md",
    short_entries=False,
)
def range_scalper_strategy(df: pd.DataFrame, **params) -> pd.DataFrame:
    return range_scalper_core(df, **params)


@register(
    "sweep_squeeze_combo",
    "Sweep Squeeze Combo \u2014 2-of-3 consensus (liquidity sweeps + squeeze momentum + stochastic RSI) for high-conviction reversals",
    {"swing_lookback": 10, "min_agree": 2},
    constraints=["swing_lookback > 0"],
    edge_status="no_edge",
    edge_source="fee_audit_m5",
    edge_ref="docs/research/fee-audit-m5.md",
    short_entries=False,
)
def sweep_squeeze_combo_strategy(df: pd.DataFrame, **params) -> pd.DataFrame:
    return sweep_squeeze_combo_core(df, **params)


@register(
    "adx_trend",
    "ADX Trend Rider \u2014 enters on DI crossovers when ADX confirms strong trend (>25)",
    {"adx_period": 14, "adx_threshold": 25},
    constraints=["adx_period > 0"],
    edge_status="no_edge",
    edge_source="fee_audit_m5",
    edge_ref="docs/research/fee-audit-m5.md",
    short_entries=False,
)
def adx_trend_strategy(df: pd.DataFrame, **params) -> pd.DataFrame:
    return adx_trend_core(df, **params)


@register(
    "commodity_channel_trend",
    "RESEARCH, no-edge (#1656) \u2014 Commodity Channel Index trend entries: long when the index (typical price minus its lookback mean, over 0.015 times the window's mean absolute deviation) crosses strictly above +threshold while the close is strictly above the trend_period simple moving average; short mirrors below -threshold under the average. Entry-only; pair with one explicit close strategy and one stop owner. Paper evaluation needs explicit paper mode; live use needs allow_no_edge: true",
    {"lookback": 20, "threshold": 100.0, "trend_period": 50},
    platforms=("futures",),
    constraints=[
        "lookback >= 2",
        "lookback <= 100",
        "threshold > 0",
        "trend_period >= 2",
        "trend_period <= 150",
    ],
    edge_status="no_edge",
    edge_source="study_fail",
    edge_ref="backtest/candidates/commodity_channel_trend_1656/REPORT.md",
    short_entries=True,
)
def commodity_channel_trend_strategy(df: pd.DataFrame, **params) -> pd.DataFrame:
    return commodity_channel_trend_core(df, **params)


_DELTA_FUNDING_WINDOW_DAYS = 7.0


def _funding_window_bars(index: pd.Index) -> int:
    bar_hours = 1.0
    if isinstance(index, pd.DatetimeIndex) and len(index) >= 2:
        deltas = index.to_series().diff().dropna()
        if not deltas.empty:
            secs = deltas.median().total_seconds()
            if secs and secs > 0:
                bar_hours = secs / 3600.0
    return max(1, int(round(_DELTA_FUNDING_WINDOW_DAYS * 24.0 / bar_hours)))


@register(
    "delta_neutral_funding",
    "Delta-Neutral Funding \u2014 enter when 7d avg funding rate exceeds threshold, exit when below",
    {"entry_threshold": 0.0001, "exit_threshold": 0.00005, "drift_threshold": 2.0,
     "current_funding_rate": 0.0, "avg_funding_rate_7d": 0.0},
    platforms=("futures",),
    short_entries=False,
)
def delta_neutral_funding_strategy(df: pd.DataFrame,
                                   entry_threshold: float = 0.0001,
                                   exit_threshold: float = 0.00005,
                                   drift_threshold: float = 2.0,
                                   current_funding_rate: float = 0.0,
                                   avg_funding_rate_7d: float = 0.0) -> pd.DataFrame:
    result = df.copy()
    result["delta_drift_pct"] = 0.0
    result["rebalance_needed"] = 0.0

    has_series = "funding_rate" in df.columns and pd.to_numeric(
        df["funding_rate"], errors="coerce").notna().any()
    if has_series:
        funding = pd.to_numeric(df["funding_rate"], errors="coerce")
        window_bars = _funding_window_bars(result.index)
        avg = funding.rolling(window_bars, min_periods=window_bars).mean()
        result["funding_rate"] = funding
        result["avg_funding_7d"] = avg
        result["funding_apy"] = avg * 24 * 365 * 100
        sig = pd.Series(0, index=result.index, dtype=int)
        sig[avg > entry_threshold] = -1
        sig[avg < exit_threshold] = 1
        sig[avg.isna()] = 0
        result["signal"] = sig.values
        return result

    avg = avg_funding_rate_7d
    result["funding_rate"] = current_funding_rate
    result["avg_funding_7d"] = avg
    result["funding_apy"] = avg * 3 * 365 * 100
    result["signal"] = 0
    if avg == 0.0:
        return result
    if avg > entry_threshold:
        result.iloc[-1, result.columns.get_loc("signal")] = -1
    elif avg < exit_threshold:
        result.iloc[-1, result.columns.get_loc("signal")] = 1
    return result


@register(
    "funding_skew",
    "Funding Skew \u2014 funding-rate crowding extremes (rolling z-score) with EMA price confirmation: long crowded-short squeezes, short crowded-long breakdowns; flat when funding is unavailable",
    {
        "funding_window": 168,
        "z_entry": 2.0,
        "z_exit": 0.5,
        "confirm_ema": 40,
        "min_abs_rate": 0.00001,
        "allow_short": True,
    },
    platforms=("futures",),
    constraints=[
        "funding_window > 0",
        "z_exit < z_entry",
    ],
    edge_status="no_edge",
    edge_source="fee_audit_m5",
    edge_ref="docs/research/1282-m5-limbo-verdicts.md",
    short_entries=True,
)
def funding_skew_strategy(df: pd.DataFrame, **params) -> pd.DataFrame:
    return funding_skew_core(df, **params)


@register(
    "donchian_breakout",
    "Donchian Channel Breakout \u2014 turtle-trading style entry on new high/low channel breakouts",
    {"entry_period": 20, "exit_period": 10},
    constraints=[
        "entry_period > 0",
        "exit_period > 0",
    ],
    edge_status="no_edge",
    edge_source="study_fail",
    edge_ref="docs/research/985-donchian-regime-gate-m1.md",
    short_entries=True,
)
def donchian_breakout_strategy(df: pd.DataFrame, **params) -> pd.DataFrame:
    return donchian_breakout_core(df, **params)


@register(
    "bear_pullback_st",
    "Bear Pullback Short — short bear-market rallies into EMA20/50 resistance with RSI rebound + ADX trend filter",
    {
        "ema_short": 20, "ema_mid": 50, "ema_long": 200,
        "adx_period": 14, "adx_threshold": 20.0,
        "rsi_period": 14, "rsi_lower": 55.0, "rsi_upper": 65.0,
        "pullback_window": 5, "pullback_touch_buffer_pct": 0.001,
    },
    platforms=("futures",),
    constraints=[
        "ema_short > 0",
        "adx_period > 0",
        "rsi_period > 0",
        "pullback_window > 0",
        "ema_short < ema_mid",
        "ema_mid < ema_long",
        "rsi_lower < rsi_upper",
    ],
    short_entries=True,
)
def bear_pullback_st_strategy(df: pd.DataFrame, **params) -> pd.DataFrame:
    return bear_pullback_st_core(df, **params)


@register(
    "session_breakout",
    "Session Breakout — break of prior session (Asian/US open/US close) high/low with volume confirmation",
    {
        "session": "asian", "lookback": 1, "volume_threshold": 1.5,
        "vol_period": 20, "atr_period": 14, "atr_multiplier": 0.0,
    },
    platforms=("futures",),
    constraints=[
        "lookback > 0",
        "vol_period > 0",
        "atr_period > 0",
    ],
    edge_status="no_edge",
    edge_source="study_fail",
    edge_ref="docs/research/1031-session-breakout-short-m1.md",
    short_entries=True,
)
def session_breakout_strategy(df: pd.DataFrame, **params) -> pd.DataFrame:
    return session_breakout_core(df, **params)


@register(
    "vwap_rejection_st",
    "VWAP Rejection Short — short weak rallies into session VWAP / EMA20 / EMA50 with RSI sub-50 + bearish EMA50<EMA200 regime",
    {
        "ema_short": 20, "ema_mid": 50, "ema_long": 200,
        "rsi_period": 14, "rsi_max_reclaim": 50.0,
        "rally_window": 5, "rally_touch_buffer_pct": 0.001,
    },
    platforms=("futures",),
    constraints=[
        "ema_short > 0",
        "rsi_period > 0",
        "rally_window > 0",
        "ema_short < ema_mid",
        "ema_mid < ema_long",
    ],
    short_entries=True,
)
def vwap_rejection_st_strategy(df: pd.DataFrame, **params) -> pd.DataFrame:
    return vwap_rejection_st_core(df, **params)


@register(
    "anchored_vwap",
    "Anchored VWAP — single VWAP anchored to the last confirmed swing pivot as dynamic S/R; long on a buffered reclaim above, short on a buffered breakdown below",
    {
        "pivot_strength": 5,
        "buffer_atr_mult": 0.25,
        "confirm_bars": 2,
        "atr_period": 14,
        "gate_rsi_period": 0, "gate_rsi_level": 50.0,
        "gate_ema_period": 0,
    },
    constraints=[
        "pivot_strength > 0",
        "atr_period > 0",
    ],
    short_entries=True,
)
def anchored_vwap_strategy(df: pd.DataFrame, **params) -> pd.DataFrame:
    return anchored_vwap_core(df, **params)


@register(
    "anchored_vwap_channel",
    "Anchored VWAP Channel — dual VWAPs anchored to the last confirmed swing low (support) and swing high (resistance); long a buffered bounce off the lower line, short a buffered rejection off the upper",
    {
        "pivot_strength": 5,
        "buffer_atr_mult": 0.25,
        "confirm_bars": 2,
        "min_width_atr_mult": 1.5,
        "atr_period": 14,
    },
    constraints=[
        "pivot_strength > 0",
        "atr_period > 0",
    ],
    short_entries=True,
)
def anchored_vwap_channel_strategy(df: pd.DataFrame, **params) -> pd.DataFrame:
    return anchored_vwap_channel_core(df, **params)


@register(
    "anchored_vwap_reversion",
    "Anchored VWAP Reversion — fades an ATR-measured stretch beyond the pivot-anchored VWAP; long a buffered snap-back from below the band, short the mirror above",
    {
        "pivot_strength": 5,
        "entry_atr_mult": 1.5,
        "buffer_atr_mult": 0.25,
        "confirm_bars": 2,
        "atr_period": 14,
    },
    constraints=[
        "pivot_strength > 0",
        "atr_period > 0",
    ],
    short_entries=True,
)
def anchored_vwap_reversion_strategy(df: pd.DataFrame, **params) -> pd.DataFrame:
    return anchored_vwap_reversion_core(df, **params)


@register(
    "analog_retrieval",
    "RESEARCH, no-edge (#1138) — k-NN analog retrieval: matches the current bar's scale-free state vector (return efficiency, ATR-normalized momentum, ATR%, vol regime, trend) against strictly-prior windows with realized forward returns and votes them into a t-stat- and ATR-edge-gated direction signal. Paper evaluation needs explicit paper mode; live use needs allow_no_edge: true",
    {
        "feat_window": 20,
        "atr_period": 14,
        "vol_baseline": 100,
        "horizon": 12,
        "k_neighbors": 25,
        "min_index": 200,
        "max_index": 5000,
        "min_t_stat": 2.0,
        "min_edge_atr": 0.25,
    },
    constraints=[
        "feat_window > 0",
        "atr_period > 0",
        "horizon > 0",
        "k_neighbors > 0",
    ],
    edge_status="no_edge",
    edge_source="study_fail",
    edge_ref="docs/research/1138-analog-retrieval-m1.md",
    short_entries=False,
)
def analog_retrieval_strategy(df: pd.DataFrame, **params) -> pd.DataFrame:
    return analog_retrieval_core(df, **params)


@register(
    "chaikin_money_flow_breakout",
    "RESEARCH, no-edge (#1649) — Chaikin Money Flow breakout confirmation: long on the first close strictly above the prior breakout_window high while candle-estimated money flow (close-location value times volume over flow_window bars, not measured aggressor flow) is strictly above flow_threshold; short mirrors on the prior low with flow below -flow_threshold. Entry-only; pair with one explicit close strategy and one stop owner. Paper evaluation needs explicit paper mode; live use needs allow_no_edge: true",
    {"flow_window": 20, "breakout_window": 20, "flow_threshold": 0.05},
    platforms=("futures",),
    constraints=[
        "flow_window >= 2",
        "flow_window <= 100",
        "breakout_window >= 2",
        "breakout_window <= 100",
        "flow_threshold >= 0",
        "flow_threshold < 1",
    ],
    edge_status="no_edge",
    edge_source="study_fail",
    edge_ref="backtest/candidates/chaikin_money_flow_1649/REPORT.md",
    short_entries=True,
)
def chaikin_money_flow_breakout_strategy(df: pd.DataFrame, **params) -> pd.DataFrame:
    return chaikin_money_flow_breakout_core(df, **params)


@register(
    "open_interest_breakout",
    "RESEARCH, no-edge (#1637) \u2014 open-interest breakout confirmation: long on the first closed bar whose close is strictly above the prior price_lookback high while Hyperliquid open interest in base-coin units rose by strictly more than oi_change_threshold over oi_lookback bars; short mirrors below the prior low with the same rising-interest rule. Open interest comes only from supplied observations (open_interest_observations), each endpoint the latest sample at or before the bar close and no older than max_observation_age_ms; missing, stale, gapped or thin data gives a reasoned hold. Entry-only; pair with one explicit close strategy and one stop owner. Paper evaluation needs explicit paper mode; live use needs allow_no_edge: true",
    {
        "price_lookback": 20,
        "oi_lookback": 4,
        "oi_change_threshold": 0.002,
        "max_observation_age_ms": 120000,
        "observation_cadence_ms": 60000,
        "min_coverage": 0.95,
        "max_gap_ms": 300000,
    },
    platforms=("futures",),
    constraints=[
        "price_lookback >= 2",
        "price_lookback <= 200",
        "oi_lookback >= 1",
        "oi_lookback <= 96",
        "oi_change_threshold >= 0",
        "oi_change_threshold < 1",
        "max_observation_age_ms >= 1000",
        "max_observation_age_ms <= max_gap_ms",
        "observation_cadence_ms >= 1000",
        "observation_cadence_ms <= max_gap_ms",
        "min_coverage > 0",
        "min_coverage <= 1",
        "max_gap_ms <= 86400000",
    ],
    edge_status="no_edge",
    edge_source="study_inconclusive",
    edge_ref="backtest/candidates/open_interest_breakout_1637/REPORT.md",
    short_entries=True,
)
def open_interest_breakout_strategy(df: pd.DataFrame, **params) -> pd.DataFrame:
    return open_interest_breakout_core(df, **params)


@register(
    "connors_rsi_reversion",
    "RESEARCH, no-edge (#1645) — Connors RSI pullback: composite of a bounded Wilder RSI of closes (price_period), a bounded Wilder RSI of the consecutive up/down streak (streak_period, streak capped at 20 bars) and the mid-rank percentile of the latest one-bar return among the previous rank_window returns; long when the composite recovers to or above oversold from strictly below, short when it falls to or below overbought from strictly above. Every decision reads a fixed trailing span (at most 153 bars). Entry-only; pair with one explicit close strategy and one stop owner. Paper evaluation needs explicit paper mode; live use needs allow_no_edge: true",
    {"price_period": 3, "streak_period": 2, "rank_window": 100, "oversold": 10.0, "overbought": 90.0},
    platforms=("futures",),
    constraints=[
        "price_period >= 2",
        "price_period <= 10",
        "streak_period >= 2",
        "streak_period <= 10",
        "rank_window >= 20",
        "rank_window <= 150",
        "oversold > 0",
        "overbought < 100",
        "oversold < overbought",
    ],
    edge_status="no_edge",
    edge_source="study_fail",
    edge_ref="backtest/candidates/connors_rsi_reversion_1645/REPORT.md",
    short_entries=True,
)
def connors_rsi_reversion_strategy(df: pd.DataFrame, **params) -> pd.DataFrame:
    return connors_rsi_reversion_core(df, **params)


@register(
    "vortex_trend",
    "RESEARCH, no-edge (#1647) \u2014 Vortex trend crossover: VI+ and VI- are the period sums of |high - previous low| and |low - previous high| divided by the period sum of true range; long when VI+ minus VI- rises strictly above min_separation from at or below it on the previous bar, short on the mirror below -min_separation. Entry-only; pair with one explicit close strategy and one stop owner. Paper evaluation needs explicit paper mode; live use needs allow_no_edge: true",
    {"period": 14, "min_separation": 0.0},
    platforms=("futures",),
    constraints=[
        "period >= 2",
        "period <= 100",
        "min_separation >= 0",
        "min_separation < 1",
    ],
    edge_status="no_edge",
    edge_source="study_fail",
    edge_ref="backtest/candidates/vortex_trend_1647/REPORT.md",
    short_entries=True,
)
def vortex_trend_strategy(df: pd.DataFrame, **params) -> pd.DataFrame:
    return vortex_trend_core(df, **params)


@register(
    "relative_vigor_index",
    "RESEARCH, no-edge (#1666) \u2014 Relative Vigor Index crossover: the period-bar mean of 1-2-2-1 smoothed close-minus-open over the mean of smoothed high-minus-low, with a 1-2-2-1 smoothed signal line; long on a strict upward cross of the signal line while the index is above zero, short on the mirror below zero (zero_line_filter). Any bad candle, timestamp defect or cadence gap inside the window holds. Entry-only; pair with one explicit close strategy and one stop owner. Paper evaluation needs explicit paper mode; live use needs allow_no_edge: true",
    {"period": 10, "zero_line_filter": True},
    platforms=("futures",),
    constraints=[
        "period >= 2",
        "period <= 100",
    ],
    edge_status="no_edge",
    edge_source="study_fail",
    edge_ref="backtest/candidates/relative_vigor_index_1666/REPORT.md",
    short_entries=True,
)
def relative_vigor_index_strategy(df: pd.DataFrame, **params) -> pd.DataFrame:
    return relative_vigor_index_core(df, **params)


@register(
    "awesome_oscillator",
    "RESEARCH, no-edge (#1660) \u2014 Awesome Oscillator zero cross: the mean candle midpoint over fast_period bars minus the mean over slow_period bars; long when it turns positive after a negative value, short when it turns negative after a positive value, crossing at most 7 exact-zero bars (8-bar predecessor horizon). Entry-only; pair with one explicit close strategy and one stop owner. Paper evaluation needs explicit paper mode; live use needs allow_no_edge: true",
    {"fast_period": 5, "slow_period": 34},
    platforms=("futures",),
    constraints=[
        "fast_period >= 1",
        "fast_period < slow_period",
        "slow_period <= 100",
    ],
    edge_status="no_edge",
    edge_source="study_fail",
    edge_ref="backtest/candidates/awesome_oscillator_1660/REPORT.md",
    short_entries=True,
)
def awesome_oscillator_strategy(df: pd.DataFrame, fast_period: int = 5, slow_period: int = 34) -> pd.DataFrame:
    return awesome_oscillator_core(df, fast_period=fast_period, slow_period=slow_period)


@register(
    "chande_momentum",
    "RESEARCH, no-edge (#1661) \u2014 Chande Momentum Oscillator threshold cross: 100 x (sum of up close changes - sum of down close changes) / (their total) over period unsmoothed changes, not Wilder-smoothed RSI; long when it rises strictly above threshold from at or below it, short when it falls strictly below -threshold from at or above it. Both adjacent windows (period + 2 closes) must hold finite positive closes spaced exactly expected_interval_ms apart, else the entry holds with a reason; a flat window reads 0 and fires nothing. Entry-only; pair with one explicit close strategy and one stop owner. Paper evaluation needs explicit paper mode; live use needs allow_no_edge: true",
    {"period": 20, "threshold": 25.0, "expected_interval_ms": 3600000},
    platforms=("futures",),
    constraints=[
        "period >= 1",
        "period <= 198",
        "threshold > 0",
        "threshold < 100",
        "expected_interval_ms >= 1",
    ],
    edge_status="no_edge",
    edge_source="study_fail",
    edge_ref="backtest/candidates/chande_momentum_1661/REPORT.md",
    short_entries=True,
)
def chande_momentum_strategy(df: pd.DataFrame, period: int = 20, threshold: float = 25.0,
                             expected_interval_ms: int = 3600000) -> pd.DataFrame:
    return chande_momentum_core(df, period=period, threshold=threshold,
                                expected_interval_ms=expected_interval_ms)


@register(
    "dual_thrust",
    "RESEARCH, no-edge (#1642) \u2014 Dual Thrust session breakout: at each UTC session boundary (session_start_hour) it freezes range = max(HH - LC, HC - LL) over the immediately preceding lookback_sessions complete daily sessions (highest high, lowest daily close, highest daily close, lowest low) and anchors on the current session's opening candle open; long on the first closed bar strictly above anchor + k_long x range, short on the first closed bar strictly below anchor - k_short x range, at most one raw signal per side per session, reconstructed from the supplied closed bars each evaluation. Every candle of the prior sessions and of the current session through the decision bar must be present, on the expected_interval_ms grid and well formed, else the entry holds with a reason; (lookback_sessions + 1) sessions must fit the 200-bar check window. Entry-only; pair with one explicit close strategy and one stop owner. Paper evaluation needs explicit paper mode; live use needs allow_no_edge: true",
    {"lookback_sessions": 4, "k_long": 0.5, "k_short": 0.5, "session_start_hour": 0,
     "expected_interval_ms": 3600000},
    platforms=("futures",),
    constraints=[
        "lookback_sessions >= 1",
        "lookback_sessions <= 199",
        "k_long > 0",
        "k_long <= 2",
        "k_short > 0",
        "k_short <= 2",
        "session_start_hour >= 0",
        "session_start_hour <= 23",
        "expected_interval_ms >= 1",
        "expected_interval_ms <= 86400000",
    ],
    edge_status="no_edge",
    edge_source="study_fail",
    edge_ref="backtest/candidates/dual_thrust_1642/REPORT.md",
    short_entries=True,
    param_validator=validate_dual_thrust_param_dict,
)
def dual_thrust_strategy(df: pd.DataFrame, lookback_sessions: int = 4, k_long: float = 0.5,
                         k_short: float = 0.5, session_start_hour: int = 0,
                         expected_interval_ms: int = 3600000) -> pd.DataFrame:
    return dual_thrust_core(df, lookback_sessions=lookback_sessions, k_long=k_long, k_short=k_short,
                            session_start_hour=session_start_hour,
                            expected_interval_ms=expected_interval_ms)


@register(
    "williams_r_reversal",
    "RESEARCH, no-edge (#1650) \u2014 Williams %R reversal: %R = -100 x (highest high - close) / (highest high - lowest low) over the lookback bars ending at the decision bar; long when the previous %R is at or below oversold and the current %R is strictly above it, short when the previous %R is at or above overbought and the current %R is strictly below it. Needs lookback + 1 valid consecutive candles; any bad candle, timestamp defect or zero-range window holds. Entry-only; pair with one explicit close strategy and one stop owner. Paper evaluation needs explicit paper mode; live use needs allow_no_edge: true",
    {"lookback": 14, "oversold": -80.0, "overbought": -20.0},
    platforms=("futures",),
    constraints=[
        "lookback >= 2",
        "lookback <= 100",
        "oversold >= -100",
        "overbought <= 0",
        "oversold < overbought",
    ],
    edge_status="no_edge",
    edge_source="study_fail",
    edge_ref="backtest/candidates/williams_r_reversal_1650/REPORT.md",
    short_entries=True,
    param_validator=williams_r_param_validator,
)
def williams_r_reversal_strategy(df: pd.DataFrame, lookback: int = 14, oversold: float = -80.0,
                                 overbought: float = -20.0) -> pd.DataFrame:
    return williams_r_reversal_core(df, lookback=lookback, oversold=oversold, overbought=overbought)


@register(
    "on_balance_volume_divergence",
    "RESEARCH, no-edge (#1657) \u2014 On Balance Volume divergence: signed volume is +volume after a higher close, -volume after a lower close and 0 after an equal close; a strict price pivot (left_span/right_span) is usable only once its right-side bars close; each newly confirmed low pairs with the nearest earlier confirmed low min_separation..max_separation bars back, and a strictly lower low whose signed-volume sum over the pair interval, divided by the absolute-volume sum over the same interval, exceeds volume_threshold arms a long; the mirror high arms a short. The setup enters once on the first later close strictly beyond the reclaim level (highest high, or lowest low, from the second pivot through its confirmation bar) within setup_expiry bars; a newer pivot replaces it and simultaneous opposite events hold. Every decision reads exactly its trailing left_span + max_separation + right_span + setup_expiry + 1 bars (at most 200); bad prices, bad volume, timestamp defects and cadence gaps inside that span hold. volume_test=false is the price-pivot-only ablation. Entry-only; pair with one explicit close strategy and one stop owner. Paper evaluation needs explicit paper mode; live use needs allow_no_edge: true",
    {
        "left_span": 3, "right_span": 3, "min_separation": 5, "max_separation": 40,
        "volume_threshold": 0.10, "setup_expiry": 12, "volume_test": True,
    },
    platforms=("futures",),
    constraints=[
        "left_span >= 1",
        "left_span <= 10",
        "right_span >= 1",
        "right_span <= 10",
        "min_separation >= 1",
        "max_separation <= 100",
        "min_separation <= max_separation",
        "volume_threshold >= 0",
        "volume_threshold <= 1",
        "setup_expiry >= 1",
        "setup_expiry <= 50",
    ],
    edge_status="no_edge",
    edge_source="study_inconclusive",
    edge_ref="backtest/candidates/on_balance_volume_divergence_1657/REPORT.md",
    short_entries=True,
)
def on_balance_volume_divergence_strategy(df: pd.DataFrame, **params) -> pd.DataFrame:
    return on_balance_volume_divergence_core(df, **params)


@register(
    "money_flow_index_reversal",
    "RESEARCH, no-edge (#1658) \u2014 Money Flow Index reversal: typical price times volume, split into positive and negative flow by the change in typical price, summed over lookback bars into a 0-100 oscillator; long when it rises strictly above oversold from at or below it, short when it falls strictly below overbought from at or above it. Missing or invalid volume holds; volume is never invented. Entry-only; pair with one explicit close strategy and one stop owner. Paper evaluation needs explicit paper mode; live use needs allow_no_edge: true",
    {"lookback": 14, "oversold": 20.0, "overbought": 80.0},
    platforms=("futures",),
    constraints=[
        "lookback >= 2",
        "lookback <= 198",
        "oversold >= 0",
        "oversold < overbought",
        "overbought <= 100",
    ],
    edge_status="no_edge",
    edge_source="study_fail",
    edge_ref="backtest/candidates/money_flow_index_reversal_1658/REPORT.md",
    short_entries=True,
)
def money_flow_index_reversal_strategy(df: pd.DataFrame, **params) -> pd.DataFrame:
    return money_flow_index_reversal_core(df, **params)


@register(
    "ultimate_oscillator",
    "RESEARCH, no-edge (#1664) \u2014 Ultimate Oscillator confirmation of Donchian breakouts: buying pressure is close minus min(low, previous close), true range is max(high, previous close) minus that minimum, and the oscillator is 100 x (4 x short + 2 x mid + long window averages of summed pressure over summed range) / 7; long only when the close first breaks strictly above the prior breakout_lookback high and the oscillator on that same closed bar is strictly above bullish_threshold, short on the mirror below the prior low and bearish_threshold. A failed confirmation holds and no later bar confirms it. Any bad candle, timestamp defect, cadence gap or zero range inside the trailing dependency span holds. Entry-only; pair with one explicit close strategy and one stop owner. Paper evaluation needs explicit paper mode; live use needs allow_no_edge: true",
    {
        "short_window": 7,
        "mid_window": 14,
        "long_window": 28,
        "bullish_threshold": 70.0,
        "bearish_threshold": 30.0,
        "breakout_lookback": 20,
    },
    platforms=("futures",),
    constraints=[
        "short_window >= 1",
        "short_window < mid_window",
        "mid_window < long_window",
        "long_window <= 198",
        "breakout_lookback >= 1",
        "breakout_lookback <= 197",
        "bearish_threshold > 0",
        "bearish_threshold < bullish_threshold",
        "bullish_threshold < 100",
    ],
    edge_status="no_edge",
    edge_source="study_inconclusive",
    edge_ref="backtest/candidates/ultimate_oscillator_1664/REPORT.md",
    short_entries=True,
)
def ultimate_oscillator_strategy(df: pd.DataFrame, short_window: int = 7, mid_window: int = 14,
                                 long_window: int = 28, bullish_threshold: float = 70.0,
                                 bearish_threshold: float = 30.0,
                                 breakout_lookback: int = 20) -> pd.DataFrame:
    return ultimate_oscillator_core(
        df, short_window=short_window, mid_window=mid_window, long_window=long_window,
        bullish_threshold=bullish_threshold, bearish_threshold=bearish_threshold,
        breakout_lookback=breakout_lookback)


@register(
    "schaff_trend_cycle",
    "RESEARCH, no-edge (#1651) \u2014 Schaff Trend Cycle entries: for every timestamp the fast and slow close EMAs (weight 2 / (period + 1)) restart from the first close of exactly 192 trailing bars, MACD = fast EMA - slow EMA drops its first slow_period - 1 samples, each complete cycle_length window of MACD is normalized to 0-100 and smoothed recursively by smoothing (seeded with its first value), and the smoothed series is normalized and smoothed again into the 0-100 oscillator. Long when the previous oscillator is at or below lower_threshold, the current one is strictly above it and the current MACD endpoint is strictly above the previous one; short is the mirror at upper_threshold with a falling MACD. Needs 193 valid consecutive closed bars and declared bar cadence; any bad candle, timestamp defect, cadence gap or zero normalization range holds, and a check-time frame whose last bar is not the latest closed bar holds. Entry-only; pair with one explicit close strategy and one stop owner. Paper evaluation needs explicit paper mode; live use needs allow_no_edge: true",
    {
        "fast_period": 23,
        "slow_period": 50,
        "cycle_length": 10,
        "smoothing": 0.5,
        "lower_threshold": 25.0,
        "upper_threshold": 75.0,
    },
    platforms=("futures",),
    constraints=[
        "fast_period >= 2",
        "fast_period <= 32",
        "slow_period >= 3",
        "slow_period <= 64",
        "fast_period < slow_period",
        "cycle_length >= 2",
        "cycle_length <= 16",
        "smoothing >= 0.25",
        "smoothing <= 1",
        "lower_threshold > 0",
        "upper_threshold < 100",
        "lower_threshold < upper_threshold",
    ],
    edge_status="no_edge",
    edge_source="study_fail",
    edge_ref="backtest/candidates/schaff_trend_cycle_1651/REPORT.md",
    short_entries=True,
    param_validator=stc_param_validator,
)
def schaff_trend_cycle_strategy(df: pd.DataFrame, fast_period: int = 23, slow_period: int = 50,
                                cycle_length: int = 10, smoothing: float = 0.5,
                                lower_threshold: float = 25.0, upper_threshold: float = 75.0) -> pd.DataFrame:
    return schaff_trend_cycle_core(
        df, fast_period=fast_period, slow_period=slow_period, cycle_length=cycle_length,
        smoothing=smoothing, lower_threshold=lower_threshold, upper_threshold=upper_threshold)


@register(
    "trix_momentum",
    "RESEARCH, no-edge (#1659) \u2014 triple-smoothed momentum (TRIX) crossover entries: for every endpoint the three chained close EMAs (weight 2 / (period + 1)) restart from the close recursion_length bars back and apply exactly recursion_length updates; the oscillator is 100 times the one-bar fractional change of the third EMA and the signal line is an EMA (weight 2 / (signal_period + 1)) of that local oscillator seeded with its first value. The previous endpoint uses its own trailing run, so a decision reads recursion_length + 2 bars (at most 199). Long when oscillator minus signal line is strictly positive, was at or below zero on the previous bar, and the oscillator is strictly positive; short is the exact mirror. Needs declared bar cadence and recursion_length >= 6 * period; bad candles, timestamp defects, cadence gaps and invalid derived values inside the window hold. Entry-only; pair with one explicit close strategy and one stop owner. Paper evaluation needs explicit paper mode; live use needs allow_no_edge: true",
    {"period": 18, "signal_period": 9, "recursion_length": 120},
    platforms=("futures",),
    constraints=[
        "period >= 1",
        "period <= 32",
        "signal_period >= 1",
        "signal_period < recursion_length",
        "recursion_length >= 6",
        "recursion_length <= 197",
    ],
    edge_status="no_edge",
    edge_source="study_fail",
    edge_ref="backtest/candidates/trix_momentum_1659/REPORT.md",
    short_entries=True,
    param_validator=trix_param_validator,
)
def trix_momentum_strategy(df: pd.DataFrame, period: int = 18, signal_period: int = 9,
                           recursion_length: int = 120) -> pd.DataFrame:
    return trix_momentum_core(df, period=period, signal_period=signal_period, recursion_length=recursion_length)


@register(
    "stochastic_momentum_index",
    "RESEARCH, no-edge (#1663) \u2014 Stochastic Momentum Index pullback entries: for each bar the highest high and lowest low of the range_period bars ending there give the range (difference) and the relative value (close minus their midpoint). For every endpoint t all five recursions restart at bar t - recursion_length: the relative value and the range are each smoothed twice by an exponential average of smoothing_period (weight 2 / (period + 1)), both stages seeded with that bar's own relative value or range; the oscillator is 200 x smoothed relative / smoothed range, and its signal line is an exponential average of signal_period seeded with the seed-bar oscillator; exactly recursion_length updates run through t. Endpoint t - 1 is replayed independently from its own seed. Long when the oscillator rises from at or below oversold to strictly above it and oscillator minus signal line rises from at or below zero to strictly above zero on the same bar; short is the mirror at overbought. Needs recursion_length + range_period + 1 valid consecutive closed bars (recursion_length at least 8 x the larger smoothing or signal period, recursion_length + range_period at most 198) and declared bar cadence; any bad candle, timestamp defect, cadence gap or non-positive smoothed range holds, and a check-time frame whose last bar is not the latest closed bar holds. Entry-only; pair with one explicit close strategy and one stop owner. Paper evaluation needs explicit paper mode; live use needs allow_no_edge: true",
    {
        "range_period": 10,
        "smoothing_period": 3,
        "signal_period": 3,
        "recursion_length": 60,
        "oversold": -40.0,
        "overbought": 40.0,
    },
    platforms=("futures",),
    constraints=[
        "range_period >= 1",
        "smoothing_period >= 1",
        "signal_period >= 1",
        "recursion_length >= 8",
        "oversold > -100",
        "overbought < 100",
        "oversold < overbought",
    ],
    edge_status="no_edge",
    edge_source="study_inconclusive",
    edge_ref="backtest/candidates/stochastic_momentum_index_1663/REPORT.md",
    short_entries=True,
    param_validator=smi_param_validator,
)
def stochastic_momentum_index_strategy(df: pd.DataFrame, range_period: int = 10, smoothing_period: int = 3,
                                       signal_period: int = 3, recursion_length: int = 60,
                                       oversold: float = -40.0, overbought: float = 40.0) -> pd.DataFrame:
    return stochastic_momentum_index_core(
        df, range_period=range_period, smoothing_period=smoothing_period, signal_period=signal_period,
        recursion_length=recursion_length, oversold=oversold, overbought=overbought)


@register(
    "camarilla_pivot_reversal",
    "RESEARCH, no-edge (#1669) \u2014 Camarilla pivot reversal: the previous complete UTC day (00:00 to 24:00) gives high H, low L and final close C, frozen for the current UTC day as S3 = C - 1.1 x (H - L) / 4, R3 = C + 1.1 x (H - L) / 4, S4 = C - 1.1 x (H - L) / 2 and R4 = C + 1.1 x (H - L) / 2. Long when the preceding closed candle closes at or below S3 and the current closed candle closes strictly above S3 and strictly below R3 with both lows strictly above S4; short when the preceding candle closes at or above R3 and the current candle closes strictly below R3 and strictly above S3 with both highs strictly below R4. Both candles are consecutive bars of the current UTC day; a jump across the opposite level holds. Every bar of the prior day must be present, on the expected_interval_ms grid (15m, 30m, 1h, 2h, 4h, 8h or 12h) and well formed, and the frame must declare its bar interval and timestamp kind, else the entry holds with a reason; levels never carry over a missing day. A check-time frame whose last bar is not the latest closed bar holds. Entry-only; pair with one explicit close strategy and one stop owner. Paper evaluation needs explicit paper mode; live use needs allow_no_edge: true",
    {"expected_interval_ms": 3600000},
    platforms=("futures",),
    constraints=[
        "expected_interval_ms >= 900000",
        "expected_interval_ms <= 43200000",
    ],
    edge_status="no_edge",
    edge_source="study_fail",
    edge_ref="backtest/candidates/camarilla_pivot_reversal_1669/REPORT.md",
    short_entries=True,
    param_validator=validate_camarilla_param_dict,
)
def camarilla_pivot_reversal_strategy(df: pd.DataFrame, expected_interval_ms: int = 3600000) -> pd.DataFrame:
    return camarilla_pivot_reversal_core(df, expected_interval_ms=expected_interval_ms)


@register(
    "mass_index_reversal",
    "RESEARCH, no-edge (#1670) \u2014 Mass Index reversal: for every bar the candle range (high - low) is smoothed by two exponential averages of period ema_period (weight 2 / (period + 1)), both restarted from the range recursion_length bars back and updated exactly recursion_length times, and the Mass Index is the sum of sum_period ratios of the first average to the second. A strict rise through upper_threshold arms a volatility bulge with its direction frozen from the trend slope (close SMA of trend_period minus its value slope_span bars earlier): a rising slope allows a later short, a falling slope a later long, a zero slope arms nothing. The first strict fall below lower_threshold within max_bulge_age bars completes the bulge; on that bar a long needs the close strictly above the previous confirm_lookback highs and a short strictly below their lows, else the bulge is consumed without an entry. A newer rise replaces an older bulge. bulge_filter 0 is the comparison mode that tests the same confirmation on every bar with the side from the current slope. A decision needs max(max_bulge_age + recursion_length + sum_period + 1, max_bulge_age + trend_period + slope_span, confirm_lookback + 1) clean rows (at most 199) with declared bar cadence; bad candles, timestamp defects, cadence gaps and a non-positive second average hold with a reason. Entry-only; pair with one explicit close strategy and one stop owner. Paper evaluation needs explicit paper mode; live use needs allow_no_edge: true",
    {
        "ema_period": 9,
        "sum_period": 25,
        "upper_threshold": 27.0,
        "lower_threshold": 26.5,
        "trend_period": 50,
        "slope_span": 5,
        "confirm_lookback": 5,
        "max_bulge_age": 50,
        "recursion_length": 72,
        "bulge_filter": 1,
    },
    platforms=("futures",),
    constraints=[
        "ema_period >= 2",
        "sum_period >= 1",
        "trend_period >= 2",
        "slope_span >= 1",
        "confirm_lookback >= 1",
        "max_bulge_age >= 1",
        "recursion_length >= 16",
        "lower_threshold > 0",
        "lower_threshold < upper_threshold",
        "bulge_filter >= 0",
        "bulge_filter <= 1",
    ],
    edge_status="no_edge",
    edge_source="study_inconclusive",
    edge_ref="backtest/candidates/mass_index_reversal_1670/REPORT.md",
    short_entries=True,
    param_validator=mir_param_validator,
)
def mass_index_reversal_strategy(df: pd.DataFrame, ema_period: int = 9, sum_period: int = 25,
                                 upper_threshold: float = 27.0, lower_threshold: float = 26.5,
                                 trend_period: int = 50, slope_span: int = 5, confirm_lookback: int = 5,
                                 max_bulge_age: int = 50, recursion_length: int = 72,
                                 bulge_filter: int = 1) -> pd.DataFrame:
    return mass_index_reversal_core(
        df, ema_period=ema_period, sum_period=sum_period, upper_threshold=upper_threshold,
        lower_threshold=lower_threshold, trend_period=trend_period, slope_span=slope_span,
        confirm_lookback=confirm_lookback, max_bulge_age=max_bulge_age, recursion_length=recursion_length,
        bulge_filter=bulge_filter)


@register(
    "calendar_conditioned_entry",
    "RESEARCH, no-edge (#1653) \u2014 calendar-conditioned entries: at each scheduled refit (every refit_bars bars on a grid anchored at 1970-01-05T00:00:00Z UTC) a table of UTC weekday/hour cells is fitted from the trailing training_bars bars, using only completed labels whose exit open is present at the refit: label = open(entry + holding_bars) / open(entry) - 1, the same nominal entry and exit opens as time_stop(max_bars=holding_bars). Each cell averages its labels per UTC week, takes a Newey-West standard error over the weekly series (lag ceil(holding_bars x interval / 1 week), at least 1) and a conservative effective sample; the long edge is mean - cost_hurdle, the short edge is -mean - cost_hurdle, and a direction qualifies when its lower bound at a one-sided Bonferroni level (1 - confidence) / 336 is strictly positive with at least min_effective_obs effective weeks. A closed bar enters when the next bar's cell (from expected_interval_ms) qualifies in the last scheduled table; the table stays fixed until the next refit. Missing, duplicate, descending, misaligned or invalid bars inside the required history, or undeclared timing, hold with a reason. Entry-only; pair with time_stop and one stop owner. Paper evaluation needs explicit paper mode; live use needs allow_no_edge: true",
    {"training_bars": 4368, "holding_bars": 4, "refit_bars": 168, "min_effective_obs": 8, "confidence": 0.9,
     "cost_hurdle": 0.002, "expected_interval_ms": 3600000},
    platforms=("futures",),
    constraints=[
        "training_bars >= 2",
        "holding_bars >= 1",
        "refit_bars >= 1",
        "min_effective_obs >= 2",
        "holding_bars < training_bars",
        "refit_bars <= training_bars",
        "confidence > 0",
        "confidence < 1",
        "cost_hurdle >= 0",
        "expected_interval_ms >= 60000",
        "expected_interval_ms <= 86400000",
    ],
    edge_status="no_edge",
    edge_source="study_inconclusive",
    edge_ref="backtest/candidates/calendar_conditioned_entry_1653/REPORT.md",
    short_entries=True,
    param_validator=calendar_param_validator,
)
def calendar_conditioned_entry_strategy(df: pd.DataFrame, training_bars: int = 4368, holding_bars: int = 4,
                                        refit_bars: int = 168, min_effective_obs: int = 8,
                                        confidence: float = 0.9, cost_hurdle: float = 0.002,
                                        expected_interval_ms: int = 3600000) -> pd.DataFrame:
    return calendar_conditioned_entry_core(
        df, training_bars=training_bars, holding_bars=holding_bars, refit_bars=refit_bars,
        min_effective_obs=min_effective_obs, confidence=confidence, cost_hurdle=cost_hurdle,
        expected_interval_ms=expected_interval_ms)


@register(
    "elder_force_pullback",
    "RESEARCH, no-edge (#1668) \u2014 Elder Force Index pullback: raw force is the close minus the previous close, multiplied by the candle volume (volume_weighting 1) or by one (volume_weighting 0, the ablation). The smoothed force at every bar is one exponential recursion with weight 2 / (force_period + 1) over exactly recursion_length raw-force values ending at that bar, seeded with the oldest of them, so each value reads only the trailing recursion_length + 1 closes. Uptrend: close strictly above the trend_period close SMA and that SMA strictly above its value slope_span bars earlier; downtrend reverses both. Long when the smoothed force crosses strictly from below zero on the preceding candle to above zero on the current candle while the uptrend holds on both candles; short is the mirror in a downtrend. Exact zero never triggers, and both crossing candles need positive supplied volume in both variants. recursion_length must be at least 10 x force_period and at most 197, and a decision needs max(recursion_length + 2, trend_period + slope_span + 1) clean rows (at most 199) with declared bar cadence; bad candles, timestamp defects, cadence gaps and derived overflow hold with a reason. Entry-only; pair with one explicit close strategy and one stop owner. Paper evaluation needs explicit paper mode; live use needs allow_no_edge: true",
    {
        "force_period": 13,
        "trend_period": 50,
        "slope_span": 5,
        "recursion_length": 150,
        "volume_weighting": 1,
    },
    platforms=("futures",),
    constraints=[
        "force_period >= 2",
        "trend_period >= 2",
        "slope_span >= 1",
        "recursion_length >= 20",
        "recursion_length <= 197",
        "volume_weighting >= 0",
        "volume_weighting <= 1",
    ],
    edge_status="no_edge",
    edge_source="study_fail",
    edge_ref="backtest/candidates/elder_force_pullback_1668/REPORT.md",
    short_entries=True,
    param_validator=efp_param_validator,
)
def elder_force_pullback_strategy(df: pd.DataFrame, force_period: int = 13, trend_period: int = 50,
                                  slope_span: int = 5, recursion_length: int = 150,
                                  volume_weighting: int = 1) -> pd.DataFrame:
    return elder_force_pullback_core(
        df, force_period=force_period, trend_period=trend_period, slope_span=slope_span,
        recursion_length=recursion_length, volume_weighting=volume_weighting)


@register(
    "true_strength_index",
    "RESEARCH, no-edge (#1667) \u2014 True Strength Index (TSI) trend entries: for every endpoint the evaluator reads exactly recursion_length closes and their recursion_length - 1 successive changes; the signed changes and their absolute values are each smoothed by a slow EMA (weight 2 / (slow_period + 1)) and then a fast EMA (weight 2 / (fast_period + 1)), both stages seeded from the first change of that window. TSI is 100 times the double-smoothed signed change divided by the double-smoothed absolute change; the signal line is an EMA (weight 2 / (signal_period + 1)) of TSI, unset until the first finite TSI of the window and seeded with it. The previous endpoint uses its own window, so a decision reads recursion_length + 1 bars (at most 199). Long when TSI minus signal line is strictly positive, was at or below zero on the previous bar, and TSI is strictly positive; short is the exact mirror. Needs declared bar cadence and recursion_length >= 6 * slow_period; bad candles, timestamp defects, cadence gaps, a zero denominator and invalid derived values inside the window hold. Entry-only; pair with one explicit close strategy and one stop owner. Paper evaluation needs explicit paper mode; live use needs allow_no_edge: true",
    {"slow_period": 25, "fast_period": 13, "signal_period": 7, "recursion_length": 180},
    platforms=("futures",),
    constraints=[
        "slow_period >= 2",
        "slow_period <= 33",
        "fast_period >= 2",
        "fast_period < slow_period",
        "signal_period >= 2",
        "signal_period < recursion_length",
        "recursion_length >= 18",
        "recursion_length <= 198",
    ],
    edge_status="no_edge",
    edge_source="study_fail",
    edge_ref="backtest/candidates/true_strength_index_1667/REPORT.md",
    short_entries=True,
    param_validator=tsi_param_validator,
)
def true_strength_index_strategy(df: pd.DataFrame, slow_period: int = 25, fast_period: int = 13,
                                 signal_period: int = 7, recursion_length: int = 180) -> pd.DataFrame:
    return true_strength_index_core(df, slow_period=slow_period, fast_period=fast_period,
                                    signal_period=signal_period, recursion_length=recursion_length)


@register(
    "klinger_divergence",
    "RESEARCH, no-edge (#1662) \u2014 Klinger Oscillator divergence entries: every oscillator endpoint t is replayed from exactly replay_bars + 1 trailing closed bars (rows t-replay_bars .. t); the replay seeds trend -1, cm = dm (high - low) and the seed volume force, then trend is +1 when high + low + close exceeds the previous sum (else -1), cm adds dm while the trend repeats and restarts at previous dm + dm on a change, volume force = volume * 2 * (dm / cm - 1) * trend * 100, the oscillator is its fast_period EMA minus its slow_period EMA (both seeded at the seed force) and the signal line is a signal_period EMA of the oscillator (seeded at the seed oscillator). A strict price pivot (left_span/right_span) is usable once its right-side bars close; each newly confirmed low pairs with the nearest earlier confirmed low min_separation..max_separation bars back, and a strictly lower low with a strictly higher oscillator arms a long (the mirror high arms a short). The setup enters once on the first strict oscillator cross of its signal line in its direction on bars confirmation+1 .. confirmation+setup_expiry; a newer same-side pivot replaces it. divergence_test=false is the signal-cross-only ablation. Every decision reads exactly replay_bars + left_span + max_separation + right_span + setup_expiry + 2 bars (at most 200); bad prices or volume, timestamp defects, cadence gaps, zero cm and a check-time frame whose last bar is not the latest closed bar hold. Entry-only; pair with one explicit close strategy and one stop owner. Paper evaluation needs explicit paper mode; live use needs allow_no_edge: true",
    {
        "fast_period": 34,
        "slow_period": 55,
        "signal_period": 13,
        "replay_bars": 112,
        "left_span": 3,
        "right_span": 3,
        "min_separation": 5,
        "max_separation": 40,
        "setup_expiry": 12,
        "divergence_test": True,
    },
    platforms=("futures",),
    constraints=[
        "fast_period >= 2",
        "fast_period <= 120",
        "slow_period >= 3",
        "slow_period <= 150",
        "signal_period >= 2",
        "signal_period <= 150",
        "replay_bars >= 4",
        "replay_bars <= 190",
        "fast_period < slow_period",
        "slow_period < replay_bars",
        "signal_period < replay_bars",
        "left_span >= 1",
        "left_span <= 10",
        "right_span >= 1",
        "right_span <= 10",
        "min_separation >= 1",
        "max_separation <= 100",
        "min_separation <= max_separation",
        "setup_expiry >= 1",
        "setup_expiry <= 50",
    ],
    edge_status="no_edge",
    edge_source="study_fail",
    edge_ref="backtest/candidates/klinger_divergence_1662/REPORT.md",
    short_entries=True,
    param_validator=klinger_param_validator,
)
def klinger_divergence_strategy(df: pd.DataFrame, fast_period: int = 34, slow_period: int = 55,
                                signal_period: int = 13, replay_bars: int = 112, left_span: int = 3,
                                right_span: int = 3, min_separation: int = 5, max_separation: int = 40,
                                setup_expiry: int = 12, divergence_test: bool = True) -> pd.DataFrame:
    return klinger_divergence_core(
        df, fast_period=fast_period, slow_period=slow_period, signal_period=signal_period,
        replay_bars=replay_bars, left_span=left_span, right_span=right_span, min_separation=min_separation,
        max_separation=max_separation, setup_expiry=setup_expiry, divergence_test=divergence_test)


@register(
    "fisher_reversal",
    "RESEARCH, no-edge (#1646) \u2014 Fisher Transform reversal: the candle midpoint is normalized into [-1, 1] against its inclusive lookback-bar range, smoothed by x = 0.33 * normalized + 0.67 * previous x (clamped 1e-6 inside +/-1) and transformed by F = 0.5 * ln((1 + x) / (1 - x)) + 0.5 * previous F, both recurrences restarted from zero over exactly 64 bars for every timestamp (lookback + 65 bars of history); long when F crosses above its one-bar-lagged trigger after the trigger is below -extreme, short is the mirror above +extreme. Bad, gapped or flat input holds the entry. Entry-only; pair with one explicit close strategy and one stop owner. Paper evaluation needs explicit paper mode; live use needs allow_no_edge: true",
    {"lookback": 9, "extreme": 1.5},
    platforms=("futures",),
    constraints=[
        "lookback >= 2",
        "lookback <= 32",
        "extreme >= 0.5",
        "extreme <= 3.0",
    ],
    edge_status="no_edge",
    edge_source="study_inconclusive",
    edge_ref="backtest/candidates/fisher_reversal_1646/REPORT.md",
    short_entries=True,
)
def fisher_reversal_strategy(df: pd.DataFrame, lookback: int = 9, extreme: float = 1.5) -> pd.DataFrame:
    return fisher_reversal_core(df, lookback=lookback, extreme=extreme)


@register(
    "elder_ray_pullback",
    "RESEARCH, no-edge (#1648) \u2014 Elder Ray pullback: a finite exponential moving average of close (weight 2/(ema_period+1), seeded at the oldest close of each timestamp's own trailing 64 closes), slope = average minus the average slope_span bars earlier, bull power = high minus average and bear power = low minus average. A long setup arms on the first bar with slope strictly positive and bear power strictly negative after a bar where that pair was false; it enters once when bear power returns to zero or above while slope stays strictly positive, at most pullback_window bars after arming. Short is the mirror with negative slope and bull power. A zero or opposite slope clears the setup and recovery consumes it. Every decision reads exactly its trailing 64 + slope_span + pullback_window + 1 rows (at most 89); bad candles, timestamp defects and cadence gaps inside that span hold. Entry-only; pair with one explicit close strategy and one stop owner. Paper evaluation needs explicit paper mode; live use needs allow_no_edge: true",
    {"ema_period": 13, "slope_span": 3, "pullback_window": 8},
    platforms=("futures",),
    constraints=[
        "ema_period >= 2",
        "ema_period <= 32",
        "slope_span >= 1",
        "slope_span <= 8",
        "pullback_window >= 1",
        "pullback_window <= 16",
    ],
    edge_status="no_edge",
    edge_source="study_fail",
    edge_ref="backtest/candidates/elder_ray_pullback_1648/REPORT.md",
    short_entries=True,
    param_validator=validate_elder_ray_param_dict,
)
def elder_ray_pullback_strategy(df: pd.DataFrame, ema_period: int = 13, slope_span: int = 3,
                                pullback_window: int = 8) -> pd.DataFrame:
    return elder_ray_pullback_core(df, ema_period=ema_period, slope_span=slope_span,
                                   pullback_window=pullback_window)


@register(
    "adaptive_moving_average",
    "RESEARCH, no-edge (#1641) \u2014 Kaufman adaptive moving average trend entries: efficiency over efficiency_period bars is the absolute close change divided by the sum of absolute one-bar changes (0 when that sum is 0); the smoothing constant is (efficiency * (2/(fast_period+1) - 2/(slow_period+1)) + 2/(slow_period+1)) squared; every average restarts from the close recursion_length bars back and applies exactly recursion_length updates, so a decision reads recursion_length + efficiency_period + max(slope_span, 1) bars (at most 199). Long when the close crosses strictly above the average from at or below it and the average rose over slope_span bars; short is the exact mirror. Needs declared bar cadence; bad candles, timestamp defects and cadence gaps inside the window hold. Entry-only; pair with one explicit close strategy and one stop owner. Paper evaluation needs explicit paper mode; live use needs allow_no_edge: true",
    {"efficiency_period": 10, "fast_period": 2, "slow_period": 30, "slope_span": 3, "recursion_length": 120},
    platforms=("futures",),
    constraints=[
        "efficiency_period >= 1",
        "efficiency_period <= 64",
        "fast_period >= 1",
        "fast_period <= 64",
        "slow_period >= 2",
        "slow_period <= 256",
        "fast_period < slow_period",
        "slope_span >= 1",
        "slope_span <= 64",
        "recursion_length >= 1",
        "recursion_length <= 197",
    ],
    edge_status="no_edge",
    edge_source="study_fail",
    edge_ref="backtest/candidates/adaptive_moving_average_1641/REPORT.md",
    short_entries=True,
    param_validator=ama_param_validator,
)
def adaptive_moving_average_strategy(df: pd.DataFrame, efficiency_period: int = 10, fast_period: int = 2,
                                     slow_period: int = 30, slope_span: int = 3,
                                     recursion_length: int = 120) -> pd.DataFrame:
    return adaptive_moving_average_core(df, efficiency_period=efficiency_period, fast_period=fast_period,
                                        slow_period=slow_period, slope_span=slope_span,
                                        recursion_length=recursion_length)


@register(
    "ease_of_movement_breakout",
    "RESEARCH, no-edge (#1671) \u2014 Ease of Movement breakout confirmation: one-bar Ease of Movement is the change in candle midpoint times the current high-minus-low range divided by current base volume, averaged over eom_period bars; long on the first close strictly above the prior breakout_window high while the average is strictly positive, short on the mirror below the prior low with a strictly negative average. Needs declared bar cadence; zero or negative volume, bad candles, timestamp defects and cadence gaps inside the window hold. Entry-only; pair with one explicit close strategy and one stop owner. Paper evaluation needs explicit paper mode; live use needs allow_no_edge: true",
    {"eom_period": 14, "breakout_window": 20},
    platforms=("futures",),
    constraints=[
        "eom_period >= 2",
        "eom_period <= 100",
        "breakout_window >= 2",
        "breakout_window <= 100",
    ],
    edge_status="no_edge",
    edge_source="study_fail",
    edge_ref="backtest/candidates/ease_of_movement_1671/REPORT.md",
    short_entries=True,
)
def ease_of_movement_breakout_strategy(df: pd.DataFrame, eom_period: int = 14,
                                       breakout_window: int = 20) -> pd.DataFrame:
    return ease_of_movement_breakout_core(df, eom_period=eom_period, breakout_window=breakout_window)


@register(
    "aroon_trend",
    "RESEARCH, no-edge (#1643) — Aroon trend crossover: over the trailing period+1 closed bars (decision bar included), Aroon Up = 100*(period - bars since the highest high)/period and Aroon Down = 100*(period - bars since the lowest low)/period, ties picking the most recent bar; long on the bar Up rises strictly above Down from at or below it while Up is strictly above strength, short on the mirror with Down. Needs period+2 valid rows; bad, flat or short windows hold with a reason code. Entry-only; pair with one explicit close strategy and one stop owner. Paper evaluation needs explicit paper mode; live use needs allow_no_edge: true",
    {"period": 25, "strength": 70.0},
    platforms=("futures",),
    constraints=[
        "period >= 2",
        "period <= 198",
        "strength >= 0",
        "strength <= 100",
    ],
    edge_status="no_edge",
    edge_source="study_inconclusive",
    edge_ref="backtest/candidates/aroon_trend_1643/REPORT.md",
    short_entries=True,
    param_validator=validate_aroon_param_dict,
)
def aroon_trend_strategy(df: pd.DataFrame, period: int = 25, strength: float = 70.0) -> pd.DataFrame:
    return aroon_trend_core(df, period=period, strength=strength)


@register(
    "regression_channel_reversion",
    "RESEARCH, no-edge (#1644) \u2014 Regression-channel reversion: at decision bar N a least-squares line is fitted to the fit_window closes strictly before N, frozen with its population residual deviation and extended to N; the bands are that forecast plus or minus band_multiplier times the deviation. Long when the previous close was at or below its own frozen lower band and the current close is strictly above the current lower band and at or below the current upper band; short mirrors at the upper band. Entries also need the normalized slope (absolute fitted change over the window divided by the window mean) at or below slope_ceiling and the residual deviation at or above min_residual_std (quote-price units). Needs fit_window+2 rows with declared bar cadence; bad prices, timestamp defects, cadence gaps and degenerate fits hold with a reason code. Entry-only; pair with one explicit close strategy and one stop owner. Paper evaluation needs explicit paper mode; live use needs allow_no_edge: true",
    {"fit_window": 50, "band_multiplier": 2.0, "slope_ceiling": 0.10, "min_residual_std": 0.0},
    platforms=("futures",),
    constraints=[
        "fit_window >= 2",
        "fit_window <= 197",
        "band_multiplier > 0",
        "slope_ceiling >= 0",
        "min_residual_std >= 0",
    ],
    edge_status="no_edge",
    edge_source="study_fail",
    edge_ref="backtest/candidates/regression_channel_reversion_1644/REPORT.md",
    short_entries=True,
    param_validator=validate_rcr_param_dict,
)
def regression_channel_reversion_strategy(df: pd.DataFrame, fit_window: int = 50, band_multiplier: float = 2.0,
                                          slope_ceiling: float = 0.10,
                                          min_residual_std: float = 0.0) -> pd.DataFrame:
    return regression_channel_reversion_core(df, fit_window=fit_window, band_multiplier=band_multiplier,
                                             slope_ceiling=slope_ceiling, min_residual_std=min_residual_std)


@register(
    "momentum_pro",
    "Momentum Pro — trend-pullback entries in a stacked-EMA trend, ADX-confirmed, on a volume-backed resumption",
    {
        "ema_fast": 20, "ema_mid": 50, "ema_long": 200,
        "adx_period": 14, "adx_threshold": 20.0,
        "pullback_window": 6, "pullback_touch_buffer_pct": 0.0,
        "vol_period": 20, "vol_mult": 1.2,
    },
    constraints=[
        "ema_fast > 0",
        "adx_period > 0",
        "vol_period > 0",
        "pullback_window > 0",
        "ema_fast < ema_mid",
        "ema_mid < ema_long",
    ],
    short_entries=True,
)
def momentum_pro_strategy(df: pd.DataFrame, **params) -> pd.DataFrame:
    return momentum_pro_core(df, **params)


@register(
    "mean_reversion_pro",
    "Mean Reversion Pro — z-score reversion gated by a no-trend ADX ceiling + RSI extreme confirmation",
    {
        "lookback": 30, "entry_std": 2.0,
        "adx_period": 14, "adx_max": 25.0,
        "rsi_period": 14, "rsi_oversold": 30.0, "rsi_overbought": 70.0,
        "confirm_window": 3,
        "touch_entry": 0, "turn_entry": 0,
    },
    constraints=[
        "lookback > 0",
        "adx_period > 0",
        "rsi_period > 0",
        "rsi_oversold < rsi_overbought",
    ],
    short_entries=True,
)
def mean_reversion_pro_strategy(df: pd.DataFrame, **params) -> pd.DataFrame:
    return mean_reversion_pro_core(df, **params)


@register(
    "rsi_bb_combo",
    "RSI+BB Combo — Bollinger Band mean reversion confirmed by RSI extremes. No inline trend filter BY DESIGN — run behind the composite ranging regime gate (init wires it by default) or it fades trends. NOTE: default params LOSE on BTC 1h (-47% gated w/ tiered TP + 2xATR SL); BTC 4h gated showed edge (+27%, PF 1.42, MaxDD -16%) but the strategy FAILED M1 incumbent-relative validation in both plain and gated shapes — mean_reversion_pro dominates head-to-head (docs/research/1329-rsi-bb-combo-m1.md); NOT a promotion candidate, tune per-market before any use",
    {
        "bb_period": 20, "bb_std": 2.0,
        "rsi_period": 14, "rsi_oversold": 30.0, "rsi_overbought": 70.0,
        "confirm_window": 3,
    },
    constraints=[
        "bb_period > 0",
        "rsi_period > 0",
        "confirm_window > 0",
        "rsi_oversold < rsi_overbought",
    ],
    short_entries=True,
)
def rsi_bb_combo_strategy(df: pd.DataFrame, **params) -> pd.DataFrame:
    return rsi_bb_combo_core(df, **params)


@register(
    "consolidation_range",
    "Consolidation Range — enters at the edges of a consolidation box (long near the bottom, short near the top); exit via a trailing ATR stop. NOTE: default params LOSE in run_backtest.py (~-40% to -47% on BTC 4h, see docs/research/consolidation-findings.md) — ship as a tunable baseline, adjust box width / stop / trail per market before live use",
    {"box_width_pct": 0.05, "min_bars": 16, "edge_entry_frac": 0.2},
    platforms=("futures",),
    constraints=["min_bars > 0"],
    edge_status="no_edge",
    edge_source="fee_audit_m5",
    edge_ref="docs/research/1282-m5-limbo-verdicts.md",
    short_entries=True,
)
def consolidation_range_strategy(df: pd.DataFrame, **params) -> pd.DataFrame:
    return consolidation_range_core(df, **params)


@register(
    "atr_band_revert",
    "ATR Band Reversion — ranging-market mean reversion: fade ATR-scaled bands around an SMA (long below mid-k*ATR; short above mid+k*ATR on futures). Entries only — pair with allowed_regimes=ranging and tiered_tp_atr / stop_loss_atr_mult for the take-profit-at-mid and range-break exit (see atr_band_revert.py)",
    {"period": 20, "atr_period": 14, "k_entry": 1.5, "allow_short": False},
    variants={
        "futures": {
            "description": "ATR Band Reversion — bidirectional ranging mean reversion: fade ATR-scaled bands around an SMA (long below mid-k*ATR, short above mid+k*ATR). Entries only — pair with allowed_regimes=ranging and tiered_tp_atr / stop_loss_atr_mult for exit",
            "default_params": {"allow_short": True},
        },
    },
    constraints=[
        "period > 0",
        "atr_period > 0",
    ],
    short_entries=True,
)
def atr_band_revert_strategy(df: pd.DataFrame, **params) -> pd.DataFrame:
    return atr_band_revert_core(df, **params)


@register(
    "mtf_confluence",
    "MTF Confluence — higher-timeframe EMA trend gate (resampled in-frame, no extra data) over native-frame pullback resumption entries; exits when the HTF trend flips",
    {
        "htf_factor": 4, "htf_ema_fast": 20, "htf_ema_slow": 40,
        "htf_sep_pct": 0.001, "ltf_ema": 20, "pullback_window": 6,
        "pullback_touch_buffer_pct": 0.0, "allow_short": False,
    },
    variants={
        "futures": {
            "description": "MTF Confluence — bidirectional: HTF EMA trend gate over native-frame pullback resumption entries; shorts mirror the logic in HTF downtrends",
            "default_params": {"allow_short": True},
        },
    },
    constraints=[
        "htf_factor > 0",
        "htf_ema_fast > 0",
        "ltf_ema > 0",
        "pullback_window > 0",
        "htf_ema_fast < htf_ema_slow",
    ],
    edge_status="no_edge",
    edge_source="fee_audit_m5",
    edge_ref="docs/research/fee-audit-m5.md",
    short_entries=True,
)
def mtf_confluence_strategy(df: pd.DataFrame, **params) -> pd.DataFrame:
    return mtf_confluence_core(df, **params)


@register(
    "vol_momentum",
    "Vol Momentum — volatility-targeted time-series momentum: ATR-normalized N-bar net move with Kaufman efficiency-ratio trend confirmation; hysteresis exit on momentum decay or efficiency collapse",
    {
        "mom_window": 24, "atr_period": 14,
        "entry_threshold": 0.30, "exit_threshold": 0.05,
        "eff_entry": 0.35, "eff_exit": 0.15,
        "allow_short": False,
    },
    variants={
        "futures": {
            "description": "Vol Momentum — volatility-targeted time-series momentum, bidirectional: long/short on ATR-normalized momentum with Kaufman efficiency confirmation",
            "default_params": {"allow_short": True},
        },
    },
    constraints=[
        "mom_window > 0",
        "atr_period > 0",
    ],
    edge_status="no_edge",
    edge_source="study_fail",
    edge_ref="docs/research/vol_momentum_1021.md",
    short_entries=True,
)
def vol_momentum_strategy(df: pd.DataFrame, **params) -> pd.DataFrame:
    return vol_momentum_core(df, **params)


@register(
    "regime_adaptive",
    "Regime Adaptive — per-bar composite regime metrics (return/range/Kaufman efficiency + ADX) switch between breakout entries in clean trends and mean-reversion fades in ranges; flat in chop",
    {
        "period": 20,
        "adx_threshold": 20.0,
        "return_eff_threshold": 0.05,
        "range_eff_threshold": 0.03,
        "efficiency_threshold": 0.5,
        "breakout_lookback": 10,
        "mr_lookback": 20,
        "mr_entry_z": 2.0,
        "mr_exit_z": 0.0,
        "slow_trend_lookback": 100,
        "slow_veto_threshold": 0.05,
        "allow_short": False,
    },
    variants={
        "futures": {"default_params": {"allow_short": True}},
    },
    constraints=[
        "period > 0",
        "breakout_lookback > 0",
        "mr_lookback > 0",
    ],
    edge_status="no_edge",
    edge_source="fee_audit_m5",
    edge_ref="docs/research/1282-m5-limbo-verdicts.md",
    short_entries=True,
)
def regime_adaptive_strategy(df: pd.DataFrame, **params) -> pd.DataFrame:
    return regime_adaptive_core(df, **params)


@register(
    "regime_adaptive_htf",
    "Regime Adaptive HTF — selective z-score fades gated by composite regime labels (return/range/Kaufman efficiency + ADX) classified on higher-timeframe buckets with confirmation hysteresis; optional clean-trend entry modes; flat in chop and directional grinds",
    {
        "htf_factor": 6,
        "period": 14,
        "adx_threshold": 20.0,
        "return_eff_threshold": 0.05,
        "range_eff_threshold": 0.03,
        "efficiency_threshold": 0.5,
        "confirm_buckets": 2,
        "trend_entry": "off",
        "trend_drift_confirm": 0.10,
        "transition_window": 6,
        "pullback_z": 1.0,
        "fade_labels": "ranging",
        "breakout_lookback": 10,
        "mr_lookback": 20,
        "mr_entry_z": 2.0,
        "mr_exit_z": 0.0,
        "slow_trend_lookback": 100,
        "slow_veto_threshold": 0.05,
        "allow_short": False,
    },
    constraints=[
        "htf_factor > 0",
        "period > 0",
        "breakout_lookback > 0",
        "mr_lookback > 0",
    ],
    short_entries=False,
)
def regime_adaptive_htf_strategy(df: pd.DataFrame, **params) -> pd.DataFrame:
    return regime_adaptive_htf_core(df, **params)


@register(
    "basket_scripted_fixture",
    "Basket scripted fixture — integration-test basket evaluator that returns operator-scripted signed weights for the coordinator contract; it ranks nothing and has no edge",
    {"weights": {}, "scores": {}, "hold": "", "sleep_seconds": 0, "raw": None},
    platforms=("futures",),
    edge_status="no_edge",
    edge_source="unvalidated",
    edge_ref="SKILL.md",
    short_entries=False,
    basket=dict(BASKET_CONTRACT),
)
def basket_scripted_fixture_strategy(frames, eligibility, allocation_context, **params):
    return basket_scripted_fixture_core(frames, eligibility, allocation_context, **params)


@register(
    "cross_sectional_momentum",
    "Cross-sectional momentum — basket evaluator that ranks the eligible members by trailing closed-bar return, "
    "longs the top quantile and shorts the bottom quantile with inverse-volatility weights capped per coin; "
    "unvalidated research candidate, no edge evidence",
    dict(XSMOM_DEFAULT_PARAMS),
    platforms=("futures",),
    constraints=[
        "return_lookback_bars >= 1",
        "volatility_lookback_bars >= 2",
        "quantile > 0",
        "quantile < 0.5",
    ],
    edge_status="no_edge",
    edge_source="study_fail",
    edge_ref="backtest/candidates/cross_sectional_momentum_1787/REPORT.md",
    short_entries=False,
    param_validator=xsmom_param_validator,
    basket=dict(BASKET_CONTRACT),
    basket_preflight=xsmom_basket_preflight,
)
def cross_sectional_momentum_strategy(frames, eligibility, allocation_context, **params):
    return cross_sectional_momentum_core(frames, eligibility, allocation_context, **params)


@register(
    "cross_sectional_reversal",
    "Cross-sectional reversal — basket evaluator that ranks the eligible members by trailing closed-bar return, "
    "longs the bottom quantile and shorts the top quantile with inverse-volatility weights capped per coin; "
    "unvalidated research candidate, no edge evidence",
    dict(XSREV_DEFAULT_PARAMS),
    platforms=("futures",),
    constraints=[
        "return_lookback_bars >= 1",
        "volatility_lookback_bars >= 2",
        "quantile > 0",
        "quantile < 0.5",
    ],
    edge_status="no_edge",
    edge_source="study_fail",
    edge_ref="backtest/candidates/cross_sectional_reversal_1793/REPORT.md",
    short_entries=False,
    param_validator=xsrev_param_validator,
    basket=dict(BASKET_CONTRACT),
    basket_preflight=xsrev_basket_preflight,
)
def cross_sectional_reversal_strategy(frames, eligibility, allocation_context, **params):
    return cross_sectional_reversal_core(frames, eligibility, allocation_context, **params)


@register(
    "hold",
    "Hold — always returns signal=0; used internally by type=manual strategies for the close-evaluator loop (#569)",
    {},
    platforms=("spot", "futures"),
    short_entries=False,
)
def hold_strategy(df: pd.DataFrame) -> pd.DataFrame:
    result = df.copy()
    result["signal"] = 0
    return result


@register(
    "cumulative_volume_delta_divergence",
    "RESEARCH, production-refused (#1638) \u2014 cumulative volume delta divergence on closed candles: signed executed volume (aggressor buys minus sells, base units) from supplied Hyperliquid trade-flow aggregates (trade_flow_observations) accumulates from each UTC session start (session_hours); strict price pivots (left_span/right_span) become usable only at their confirmation bar; a lower pivot low whose pivot-to-pivot delta change, divided by the observed total volume after the earlier pivot through the later pivot, is strictly above delta_threshold arms a long, and the mirror higher high with a falling delta arms a short; a later close strictly beyond the confirmation bar high (long) or low (short) within setup_expiry bars enters once. Every used bar needs complete, unrevoked trade intervals known by the decision deadline whose total equals the candle volume, else the entry holds with a reason. delta_confirmation=false is the price-only baseline with identical data eligibility. Entry-only; research use only: every production config, mode and allow_no_edge acknowledgement is refused until a separate reviewed promotion",
    dict(DEFAULT_CVD_PARAMS),
    platforms=("futures",),
    constraints=[
        "left_span >= 1",
        "left_span <= 10",
        "right_span >= 1",
        "right_span <= 10",
        "min_separation >= 1",
        "max_separation <= 288",
        "min_separation <= max_separation",
        "delta_threshold >= 0",
        "delta_threshold < 1",
        "setup_expiry >= 1",
        "setup_expiry <= 96",
        "session_hours >= 1",
        "session_hours <= 24",
        "expected_interval_ms >= 60000",
        "expected_interval_ms <= 3600000",
    ],
    edge_status="no_edge",
    edge_source="study_inconclusive",
    edge_ref="backtest/candidates/cumulative_volume_delta_divergence_1638/REPORT.md",
    short_entries=True,
    param_validator=cvd_param_validator,
    production_refused=True,
)
def cumulative_volume_delta_divergence_strategy(df: pd.DataFrame, **params) -> pd.DataFrame:
    return cumulative_volume_delta_divergence_core(df, **params)


@register(
    "macro_event_breakout",
    "RESEARCH, production-refused (#1654) \u2014 scheduled US central-bank rate statement breakout on closed candles: for each statement in a hash-pinned, point-in-time calendar artifact (event_records, only versions known by each decision deadline and published before the release), the range_bars completed candles that close at or before the scheduled release freeze a high/low range; a long needs confirm_bars consecutive closes strictly above it, and a short the mirror below, inside the post_window_bars window that starts at the first bar opening at or after the release; one entry per event, cooldown_bars between entries; with spread_gate the decision also needs a supplied bid/ask quote (spread_records) received by the bar close, at most max_quote_age_ms old, whose (ask - bid) / midpoint is within max_spread_frac. Postponed, cancelled, conflicting or revised schedules, missing coverage and missing or invalid quotes hold the entry with a reason. Entry-only; exits belong to time_stop with max_bars = holding_bars. Research use only: every production config, mode and allow_no_edge acknowledgement is refused until a separate reviewed promotion",
    dict(DEFAULT_MACRO_EVENT_PARAMS),
    platforms=("futures",),
    constraints=[
        "range_bars >= 1",
        "range_bars <= 48",
        "post_window_bars >= 1",
        "post_window_bars <= 48",
        "confirm_bars >= 1",
        "confirm_bars <= 6",
        "confirm_bars <= post_window_bars",
        "max_spread_frac > 0",
        "max_spread_frac <= 0.01",
        "max_quote_age_ms >= 1000",
        "max_quote_age_ms <= expected_interval_ms",
        "cooldown_bars >= 0",
        "cooldown_bars <= 96",
        "holding_bars >= 1",
        "holding_bars <= 240",
        "expected_interval_ms >= 900000",
        "expected_interval_ms <= 3600000",
    ],
    edge_status="no_edge",
    edge_source="study_inconclusive",
    edge_ref="backtest/candidates/macro_event_breakout_1654/REPORT.md",
    short_entries=True,
    param_validator=macro_param_validator,
    production_refused=True,
)
def macro_event_breakout_strategy(df: pd.DataFrame, **params) -> pd.DataFrame:
    return macro_event_breakout_core(df, **params)


@register(
    "bitcoin_lead_lag",
    "RESEARCH, production-refused, frozen study FAIL (#1640: net negative on the 1h held-out window at base and stress cost) \u2014 Bitcoin lead-lag entries on closed bars: the BTC perpetual on the follower's own timeframe is the leader; a trailing intercept-and-slope regression of the follower's log return over horizon_bars, starting lag_bars after a BTC bar, on that BTC bar's log return is refitted at every decision from training_bars past pairs whose forecast window closed at or before the decision. A BTC return at least impulse_z standard deviations of the training BTC returns is an impulse event; at the first decision in its forecast window where the slope and both training-half slopes are positive, R-squared is at least min_r2 and the signed forecast minus the follower move already completed exceeds cost_bps + min_edge_bps, it enters once in the impulse direction (mirrored shorts); later decisions, restarts and stop-outs never re-enter from the same impulse. Leader rows come only from a supplied leader_context (same sealed deadline and closed-bar cutoff, or a pinned historical series); missing, stale, duplicate, reordered or unmatched leader data holds the entry with a reason. signal_mode follower_momentum and btc_trend_filter are the frozen study baselines. Entry-only; research use only: every production config, mode and allow_no_edge acknowledgement is refused until a separate reviewed promotion",
    dict(DEFAULT_LEAD_LAG_PARAMS),
    platforms=("futures",),
    constraints=[
        "lag_bars >= 1",
        "lag_bars <= 6",
        "horizon_bars >= 1",
        "horizon_bars <= 12",
        "training_bars >= 48",
        "training_bars <= 720",
        "impulse_z >= 0.5",
        "impulse_z <= 6",
        "min_r2 >= 0",
        "min_r2 < 1",
        "min_edge_bps >= 0",
        "min_edge_bps <= 500",
        "cost_bps >= 0",
        "cost_bps <= 500",
        "max_data_age_ms >= 60000",
        "max_data_age_ms <= 86400000",
        "expected_interval_ms >= 900000",
        "expected_interval_ms <= 14400000",
    ],
    edge_status="no_edge",
    edge_source="study_fail",
    edge_ref="backtest/candidates/bitcoin_lead_lag_1640/REPORT.md",
    short_entries=True,
    param_validator=lead_lag_param_validator,
    production_refused=True,
)
def bitcoin_lead_lag_strategy(df: pd.DataFrame, **params) -> pd.DataFrame:
    return bitcoin_lead_lag_core(df, **params)


@register(
    "volume_profile_reclaim",
    "RESEARCH, production-refused, frozen study INCONCLUSIVE (#1665: the only executed-trade recording holds no complete UTC session) \u2014 volume-profile value-area reclaim on closed candles: the exact executed-trade profile of the previous complete UTC day (base-asset size per canonical price from supplied Hyperliquid trades, volume_profile_observations) is frozen for the whole following day; equal-width bins over that session's price range (profile_bins, the session maximum in the last bin, half-open elsewhere) pick the point of control (lower bin on a tie) and expand to the adjacent bin with more volume (lower side on a tie) until value_area_fraction of session volume is included. A long needs the previous closed candle of the same session to close below the value-area low and the current consecutive closed candle to close strictly inside the value area; a short is the mirror from above the value-area high. Both decision deadlines must see the same complete, unrevoked profile; boundary touches, opposite-boundary overshoots, the first candle of a session, gaps and a missing, pending, revoked or malformed profile hold the entry with a reason. session_bounds=true is the ablation that uses the session low and high instead of the value area. Entry-only; research use only: every production config, mode and allow_no_edge acknowledgement is refused until a separate reviewed promotion",
    dict(DEFAULT_VPR_PARAMS),
    platforms=("futures",),
    constraints=[
        "profile_bins >= 2",
        "profile_bins <= 512",
        "value_area_fraction > 0",
        "value_area_fraction < 1",
        "expected_interval_ms >= 300000",
        "expected_interval_ms <= 3600000",
    ],
    edge_status="no_edge",
    edge_source="study_inconclusive",
    edge_ref="backtest/candidates/volume_profile_reclaim_1665/REPORT.md",
    short_entries=True,
    param_validator=vpr_param_validator,
    production_refused=True,
)
def volume_profile_reclaim_strategy(df: pd.DataFrame, **params) -> pd.DataFrame:
    return volume_profile_reclaim_core(df, **params)


@register(
    "perpetual_premium_reversion",
    "RESEARCH, production-refused, frozen study INCONCLUSIVE (#1639: 9 held-out event-replay positions in a 10.8 h recording, under the 30-position floor; net negative on both windows) \u2014 perpetual premium mean reversion on closed bars: premium is the executable bid/ask midpoint divided by the time-aligned oracle price minus one, from supplied synchronized Hyperliquid quote/oracle samples on a fixed 60 s grid (premium_observations; each sample holds the latest bbo and asset context received at or before its grid time). At each bar close the mean and population deviation come from the lookback_samples samples strictly before the current sample; a positive premium whose z-score turn_span samples earlier was strictly above entry_z and whose distance to that mean then fell strictly at every step while staying above it enters short, and the negative mirror enters long. Every sample in the window must be sampled, unrevoked by the decision time, in one socket session, with quote age, observed oracle receipt age and quote/context skew within bounds, spread at most max_spread_bps and premium and mark dislocation within bounds; the variance must be strictly above variance_floor, else the entry holds with a reason. Oracle freshness is receipt age only; the venue context has no source time. Entry-only; research use only: every production config, mode and allow_no_edge acknowledgement is refused until a separate reviewed promotion",
    dict(DEFAULT_PREMIUM_PARAMS),
    platforms=("futures",),
    constraints=[
        "lookback_samples >= 10",
        "lookback_samples <= 720",
        "entry_z > 0",
        "entry_z <= 10",
        "turn_span >= 1",
        "turn_span <= 5",
        "turn_span < lookback_samples",
        "variance_floor > 0",
        "variance_floor <= 0.0001",
        "max_spread_bps > 0",
        "max_spread_bps <= 1000",
        "max_quote_age_ms >= 100",
        "max_quote_age_ms <= 60000",
        "max_oracle_age_ms >= 100",
        "max_oracle_age_ms <= 60000",
        "max_skew_ms >= 0",
        "max_skew_ms <= 60000",
        "max_abs_premium > 0",
        "max_abs_premium <= 0.5",
        "max_mark_dislocation > 0",
        "max_mark_dislocation <= 0.5",
        "expected_interval_ms >= 60000",
        "expected_interval_ms <= 3600000",
    ],
    edge_status="no_edge",
    edge_source="study_inconclusive",
    edge_ref="backtest/candidates/perpetual_premium_reversion_1639/REPORT.md",
    short_entries=True,
    param_validator=premium_param_validator,
    production_refused=True,
)
def perpetual_premium_reversion_strategy(df: pd.DataFrame, **params) -> pd.DataFrame:
    return perpetual_premium_reversion_core(df, **params)


@register(
    "order_book_imbalance",
    "RESEARCH, production-refused, frozen study pending (#1635) \u2014 order-book imbalance on supplied Hyperliquid l2Book snapshots (l2_book_observations, the Delivery 1 l2_book contract): for the first depth_levels levels per side, B = sum(bid px x sz), A = sum(ask px x sz), I = (B - A) / (B + A) and spread_bps = 10000 x (ask - bid) / mid, all in exact rational arithmetic. A book qualifies long when I > entry_threshold and short when I < -entry_threshold (strict; equality is neutral) with spread_bps <= max_spread_bps and B + A >= min_combined_depth_usd. An episode is a same-direction run of distinct qualifying books after a retained boundary (a reset marker, a non-qualifying book or the last book of the opposite run) inside the 60000 ms evidence window; the confirmation_observations-th book is the crossing, and its sole decision deadline is the next UTC whole second at or above max(event, receipt). The entry fires only at that deadline, only if the run is still active and the crossing book's event and receipt are each at most max_snapshot_age_ms old; later deadlines return consumed, a missing boundary returns evidence_truncated, and missing or malformed books hold with a reason. Entry-only; the closed-bar branch is unsupported; research use only: every production config, mode and allow_no_edge acknowledgement is refused until a separate reviewed promotion",
    dict(DEFAULT_OBI_PARAMS),
    platforms=("futures",),
    constraints=[
        "depth_levels >= 1",
        "depth_levels <= 20",
        "entry_threshold > 0",
        "entry_threshold < 1",
        "confirmation_observations >= 2",
        "confirmation_observations <= 5",
        "max_spread_bps > 0",
        "max_spread_bps <= 100",
        "min_combined_depth_usd > 0",
        "min_combined_depth_usd <= 1000000000",
        "max_snapshot_age_ms >= 250",
        "max_snapshot_age_ms <= 5000",
    ],
    edge_status="no_edge",
    edge_source="study_inconclusive",
    edge_ref="backtest/candidates/order_book_imbalance_1635/REPORT.md",
    short_entries=True,
    param_validator=obi_param_validator,
    production_refused=True,
)
def order_book_imbalance_strategy(df: pd.DataFrame, **params) -> pd.DataFrame:
    return order_book_imbalance_core(df, **params)


PLATFORM_ORDER: Dict[str, List[str]] = {
    "spot": [
        "sma_crossover", "ema_crossover", "rsi", "bollinger_bands", "macd",
        "mean_reversion", "momentum", "volume_weighted", "triple_ema",
        "rsi_macd_combo", "stoch_rsi", "supertrend", "ichimoku_cloud",
        "pairs_spread", "squeeze_momentum", "atr_breakout", "amd_ifvg",
        "heikin_ashi_ema", "order_blocks", "vwap_reversion", "anchored_vwap",
        "anchored_vwap_channel", "anchored_vwap_reversion", "chart_pattern",
        "liquidity_sweeps", "parabolic_sar", "range_scalper",
        "sweep_squeeze_combo", "adx_trend", "donchian_breakout", "tema_cross",
        "momentum_pro", "mean_reversion_pro", "rsi_bb_combo", "atr_band_revert", "mtf_confluence",
        "vol_momentum", "regime_adaptive", "regime_adaptive_htf",
        "analog_retrieval",
        "hold",
    ],
    "futures": [
        "sma_crossover", "ema_crossover", "bollinger_bands", "volume_weighted",
        "triple_ema", "triple_ema_bidir", "tema_cross", "tema_cross_bd", "rsi_macd_combo", "momentum",
        "mean_reversion", "rsi", "macd", "breakout", "stoch_rsi", "supertrend",
        "squeeze_momentum", "ichimoku_cloud", "atr_breakout", "amd_ifvg",
        "heikin_ashi_ema", "order_blocks", "vwap_reversion", "anchored_vwap",
        "anchored_vwap_channel", "anchored_vwap_reversion", "chart_pattern",
        "liquidity_sweeps", "parabolic_sar", "range_scalper",
        "sweep_squeeze_combo", "adx_trend", "delta_neutral_funding",
        "commodity_channel_trend",
        "funding_skew", "donchian_breakout", "session_breakout", "bear_pullback_st",
        "vwap_rejection_st", "momentum_pro", "mean_reversion_pro", "rsi_bb_combo",
        "consolidation_range", "atr_band_revert", "mtf_confluence", "vol_momentum",
        "regime_adaptive", "regime_adaptive_htf", "analog_retrieval",
        "chaikin_money_flow_breakout", "open_interest_breakout", "connors_rsi_reversion",
        "vortex_trend", "relative_vigor_index", "awesome_oscillator", "williams_r_reversal",
        "on_balance_volume_divergence", "chande_momentum",
        "money_flow_index_reversal",
        "aroon_trend",
        "regression_channel_reversion",
        "ease_of_movement_breakout",
        "fisher_reversal",
        "ultimate_oscillator",
        "adaptive_moving_average",
        "dual_thrust",
        "elder_ray_pullback",
        "schaff_trend_cycle",
        "trix_momentum",
        "camarilla_pivot_reversal",
        "mass_index_reversal",
        "calendar_conditioned_entry",
        "stochastic_momentum_index",
        "elder_force_pullback",
        "true_strength_index",
        "klinger_divergence",
        "cumulative_volume_delta_divergence",
        "macro_event_breakout",
        "bitcoin_lead_lag",
        "volume_profile_reclaim",
        "perpetual_premium_reversion",


@register(
        "order_book_imbalance",
        "hold",
    ],
}

DISCOVERY_HIDDEN_STRATEGIES = no_edge_strategies()
