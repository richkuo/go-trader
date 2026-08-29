
import sys
import os
import math
from typing import Any, Optional, Tuple

sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'shared_tools'))
_REPO_ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), '..'))
if _REPO_ROOT not in sys.path:
    sys.path.insert(0, _REPO_ROOT)

import numpy as np
import pandas as pd

from storage import store_backtest_result
from atr import standard_atr

_close_registry = None

_ensure_regime_fn = None
_regime_allows_entry_fn = None

_post_tp_sl_module = None
_trailing_ratchet_module = None


def _load_regime():
    global _ensure_regime_fn, _regime_allows_entry_fn
    if _ensure_regime_fn is None:
        from regime import ensure_regime_columns as _ensure_regime_columns
        from regime import regime_label_allows_entry as _allows_entry
        _ensure_regime_fn = _ensure_regime_columns
        _regime_allows_entry_fn = _allows_entry
    return _ensure_regime_fn


def _regime_allows_entry(allowed, bar_regime: str, on_failure: str = "open") -> bool:
    if _regime_allows_entry_fn is None:
        _load_regime()
    if _regime_allows_entry_fn is None:
        if not allowed:
            return True
        if not bar_regime:
            return on_failure != "closed"
        return bar_regime in allowed
    return _regime_allows_entry_fn(allowed, bar_regime, on_failure)


def _regime_primary_labels(spec: Optional[dict]) -> Optional[tuple]:
    if not spec:
        return None
    from regime import (
        valid_labels_for_classifier,
        REGIME_PRIMARY_WINDOW_KEY,
        CLASSIFIER_ADX,
    )
    primary_key = (
        REGIME_PRIMARY_WINDOW_KEY
        if REGIME_PRIMARY_WINDOW_KEY in spec
        else sorted(spec.keys())[0]
    )
    classifier = str(spec[primary_key].get("classifier") or CLASSIFIER_ADX).strip().lower()
    if classifier == CLASSIFIER_ADX:
        return None
    return tuple(sorted(valid_labels_for_classifier(classifier)))


def _normalize_regime_directional_policy(policy: Optional[dict]) -> Optional[dict]:
    if not policy:
        return None
    if not isinstance(policy, dict):
        raise ValueError("regime_directional_policy must be an object")
    raw = policy.get("trend_regime")
    if not isinstance(raw, dict):
        raise ValueError(
            "regime_directional_policy must contain a trend_regime object"
        )
    parsed: dict[str, dict[str, object]] = {}
    for label, entry in raw.items():
        if not isinstance(entry, dict):
            raise ValueError(
                f"regime_directional_policy.{label}: must be an object"
            )
        direction = entry.get("direction")
        if not isinstance(direction, str):
            raise ValueError(
                f"regime_directional_policy.{label}.direction: must be a string"
            )
        if direction not in ("long", "short", "both"):
            raise ValueError(
                f"regime_directional_policy.{label}.direction: must be "
                f"'long', 'short', or 'both'"
            )
        invert = entry.get("invert_signal", False)
        if not isinstance(invert, bool):
            raise ValueError(
                f"regime_directional_policy.{label}.invert_signal: "
                f"must be a boolean"
            )
        for key in entry:
            if key not in ("direction", "invert_signal"):
                raise ValueError(
                    f"regime_directional_policy.{label}: unknown key {key!r}"
                )
        parsed[str(label).strip()] = {
            "direction": direction,
            "invert_signal": invert,
        }
    return parsed or None


def _gate_directional_policy_by_states(
    policy: Optional[dict], cert_states: Optional[dict],
) -> Optional[dict]:
    if not policy:
        return policy
    from regime import RANGING_DIRECTIONAL_BARE, RANGING_DIRECTIONAL_SUBS
    bare_entry = policy.get(RANGING_DIRECTIONAL_BARE)
    if isinstance(bare_entry, dict):
        expanded = dict(policy)
        for sub in sorted(RANGING_DIRECTIONAL_SUBS):
            expanded.setdefault(sub, bare_entry)
        policy = expanded
    if cert_states is None:
        return policy
    gated = {}
    for label, entry in policy.items():
        cert_dir = str(cert_states.get(label) or "").strip().lower()
        if not cert_dir:
            continue
        direction = str((entry or {}).get("direction") or "").strip().lower()
        if direction != "both" and direction != cert_dir:
            continue
        gated[label] = entry
    return gated


def _resolve_regime_directional_entry(
    policy: Optional[dict],
    current_regime: str,
    position_regime: str = "",
    position_qty: float = 0.0,
) -> Optional[dict]:
    if not policy:
        return None
    regime = str(current_regime or "").strip()
    if position_qty > 0 and str(position_regime or "").strip():
        regime = str(position_regime or "").strip()
    entry = policy.get(regime)
    return dict(entry) if isinstance(entry, dict) else None


def _apply_direction_invert_value(
    signal: int,
    uses_open_close: bool,
    direction: Optional[str],
    invert_signal: bool,
) -> int:
    sig = int(signal)
    if invert_signal and sig != 0:
        sig = -sig
    d = (direction or "").strip().lower()
    if uses_open_close and d in ("long", "short"):
        if d == "long" and sig < 0:
            return 0
        if d == "short" and sig > 0:
            return 0
    return sig


def _signal_from_open_action(action: str) -> int:
    action = str(action or "").strip().lower()
    if action == "long":
        return 1
    if action == "short":
        return -1
    return 0


def _load_post_tp_sl():
    global _post_tp_sl_module
    if _post_tp_sl_module is not None:
        return _post_tp_sl_module
    import importlib.util
    name = "_go_trader_post_tp_sl"
    path = os.path.abspath(os.path.join(
        os.path.dirname(__file__), "..", "shared_strategies", "close", "post_tp_sl.py",
    ))
    spec = importlib.util.spec_from_file_location(name, path)
    mod = importlib.util.module_from_spec(spec)
    sys.modules[name] = mod
    spec.loader.exec_module(mod)
    _post_tp_sl_module = mod
    return mod


def _load_trailing_ratchet():
    global _trailing_ratchet_module
    if _trailing_ratchet_module is not None:
        return _trailing_ratchet_module
    _ensure_close_strategies_path()
    import importlib.util
    name = "_go_trader_trailing_ratchet"
    path = os.path.abspath(os.path.join(
        os.path.dirname(__file__), "..", "shared_strategies", "close", "trailing_tp_ratchet.py",
    ))
    spec = importlib.util.spec_from_file_location(name, path)
    mod = importlib.util.module_from_spec(spec)
    sys.modules[name] = mod
    spec.loader.exec_module(mod)
    _trailing_ratchet_module = mod
    return mod


def _load_close_registry():
    global _close_registry
    if _close_registry is None:
        from close_registry_loader import evaluate as _evaluate, list_strategies as _list
        _close_registry = (_evaluate, _list)
    return _close_registry


_CLOSE_STRATEGIES_DIR = os.path.abspath(
    os.path.join(os.path.dirname(__file__), "..", "shared_strategies", "close")
)


def _ensure_close_strategies_path() -> None:
    if _CLOSE_STRATEGIES_DIR not in sys.path:
        sys.path.insert(0, _CLOSE_STRATEGIES_DIR)


def _rewrite_deprecated_close_ref(name: str, params: dict) -> tuple[str, dict]:
    if name != "tp_at_pct":
        return name, dict(params or {})
    pct = 0.03
    if params and params.get("pct") is not None:
        try:
            pct = max(float(params.get("pct", 0.03)), 0.0)
        except (TypeError, ValueError):
            pct = 0.03
    out = {
        "tp_tiers": [{"profit_pct": pct, "close_fraction": 1.0}],
    }
    if params and "sl_after" in params:
        out["sl_after"] = params["sl_after"]
    return "tiered_tp_pct", out


TIMEFRAME_PERIODS_PER_YEAR = {
    "1m":  365 * 24 * 60,
    "5m":  365 * 24 * 12,
    "15m": 365 * 24 * 4,
    "30m": 365 * 24 * 2,
    "1h":  365 * 24,
    "2h":  365 * 12,
    "4h":  365 * 6,
    "6h":  365 * 4,
    "8h":  365 * 3,
    "12h": 365 * 2,
    "1d":  365,
    "1w":  52,
    "1M":  12,
}


def periods_per_year(timeframe: str) -> int:
    return TIMEFRAME_PERIODS_PER_YEAR.get(timeframe, 365)


LIQUIDATED_METRIC_FLOOR = 100.0


PLATFORM_FEE_PCT = {
    "binanceus":   0.001,
    "hyperliquid": 0.00045,
    "robinhood":   0.0,
    "luno":        0.01,
    "okx":         0.001,
    "okx-perps":   0.0005,
}

HYPERLIQUID_MAKER_FEE_PCT = 0.00015


def fee_pct_for_platform(platform: str) -> float:
    return PLATFORM_FEE_PCT.get(platform, PLATFORM_FEE_PCT["binanceus"])


def _open_action_from_signal(signal: int) -> str:
    if signal > 0:
        return "long"
    if signal < 0:
        return "short"
    return "none"


def _parse_profile_allocation(alloc: Optional[dict]) -> Optional[dict]:
    if not alloc:
        return None
    profiles = dict(alloc.get("profiles") or {})
    param_sets = dict(alloc.get("param_sets") or {})
    confirm_bars = int(alloc.get("confirm_bars") or 0)
    initial_profile = str(alloc.get("initial_profile") or "").strip()
    if len(param_sets) != 2:
        raise ValueError(
            f"regime_profile_allocation.param_sets must define exactly 2 "
            f"profiles (the M4 two-profile model), got {len(param_sets)}"
        )
    if confirm_bars < 1:
        raise ValueError("regime_profile_allocation.confirm_bars must be >= 1")
    if initial_profile not in param_sets:
        raise ValueError(
            f"regime_profile_allocation.initial_profile={initial_profile!r} "
            f"is not a param_sets profile {sorted(param_sets)}"
        )
    for lbl, prof in profiles.items():
        if prof not in param_sets:
            raise ValueError(
                f"regime_profile_allocation.profiles[{lbl!r}]={prof!r} is not "
                f"a param_sets profile {sorted(param_sets)}"
            )
    return {
        "profiles": profiles,
        "param_sets": param_sets,
        "confirm_bars": confirm_bars,
        "initial_profile": initial_profile,
        "names": sorted(param_sets),
    }


class _ProfileSwitcher:

    def __init__(self, alloc: dict):
        self._profiles = alloc["profiles"]
        self._confirm_bars = alloc["confirm_bars"]
        self.active = alloc["initial_profile"]
        self._pending = ""
        self._seen = 0

    def step(self, label: str, flat: bool) -> str:
        desired = self._profiles.get((label or "").strip(), "")
        if desired == "":
            return self.active
        if desired == self.active:
            self._pending = ""
            self._seen = 0
            return self.active
        if self._pending == desired:
            self._seen += 1
        else:
            self._pending = desired
            self._seen = 1
        if flat and self._seen >= self._confirm_bars:
            self.active = desired
            self._pending = ""
            self._seen = 0
        return self.active


def _close_refs_use_regime_tiered_tp(refs: list[dict]) -> bool:
    for ref in refs:
        n = (ref.get("name") or "").strip().lower()
        if n in ("tiered_tp_atr_regime", "tiered_tp_atr_live_regime"):
            return True
    return False


def _normalize_open_action(value) -> str:
    action = str(value or "none").strip().lower()
    if action not in {"long", "short", "none"}:
        raise ValueError(
            "open_action column must contain only 'long', 'short', or 'none' "
            f"(got {value!r})"
        )
    return action


def _close_fraction_columns(df: pd.DataFrame) -> list[str]:
    return [
        c for c in df.columns
        if c == "close_fraction" or str(c).startswith("close_fraction:")
    ]


def _max_close_fraction_series(df: pd.DataFrame) -> pd.Series:
    cols = _close_fraction_columns(df)
    if not cols:
        return pd.Series(0.0, index=df.index)
    fractions = df[cols].fillna(0).astype(float)
    bad = (fractions < 0) | (fractions > 1)
    if bad.any().any():
        values = sorted(set(fractions[bad].stack().tolist()))
        raise ValueError(f"close_fraction values must be in [0, 1] — got {values}")
    return fractions.max(axis=1)


def _validated_entry_fraction_series(df: pd.DataFrame) -> pd.Series:
    vals = df["entry_fraction"].astype(float)
    bad = vals[vals.notna() & ((vals <= 0) | (vals > 1))]
    if not bad.empty:
        values = sorted(set(bad.tolist()))
        raise ValueError(
            f"entry_fraction values must be in (0, 1] — got {values}"
        )
    return vals


class _ScaleInState:

    __slots__ = ("risk_anchor_price", "scale_in_count", "last_add_price",
                 "added_notional_usd", "base_open_notional")

    def __init__(self):
        self.reset()

    def reset(self) -> None:
        self.risk_anchor_price = 0.0
        self.scale_in_count = 0
        self.last_add_price = 0.0
        self.added_notional_usd = 0.0
        self.base_open_notional = 0.0

    def geom_cost(self, avg_cost: float) -> float:
        if self.risk_anchor_price > 0:
            return self.risk_anchor_price
        return avg_cost


_SCALE_IN_CFG_KEYS = (
    "max_adds", "max_added_notional_usd", "add_spacing_atr", "add_notional_usd",
)


def _normalize_scale_in_cfg(scale_in: Optional[dict]) -> dict:
    cfg = {"max_adds": 0, "max_added_notional_usd": 0.0,
           "add_spacing_atr": 0.0, "add_notional_usd": 0.0}
    if not scale_in:
        return cfg
    if not isinstance(scale_in, dict):
        raise ValueError(
            f"scale_in must be a dict of {list(_SCALE_IN_CFG_KEYS)}, "
            f"got {type(scale_in).__name__}"
        )
    unknown = sorted(set(scale_in) - set(_SCALE_IN_CFG_KEYS))
    if unknown:
        raise ValueError(
            f"scale_in has unknown key(s) {unknown}; "
            f"supported: {list(_SCALE_IN_CFG_KEYS)}"
        )
    max_adds = scale_in.get("max_adds", 0) or 0
    if int(max_adds) != max_adds or int(max_adds) < 0:
        raise ValueError(f"scale_in.max_adds must be an int >= 0, got {max_adds!r}")
    cfg["max_adds"] = int(max_adds)
    for key in ("max_added_notional_usd", "add_notional_usd"):
        val = float(scale_in.get(key, 0) or 0)
        if val < 0:
            raise ValueError(f"scale_in.{key} must be >= 0, got {val}")
        cfg[key] = val
    cfg["add_spacing_atr"] = float(scale_in.get("add_spacing_atr", 0) or 0)
    return cfg


def _scale_in_decision(scale_cfg: dict, side: str, quantity: float,
                       avg_cost: float, entry_atr: float, scale_in_count: int,
                       added_notional_usd: float, last_add_price: float,
                       signal: int, price: float,
                       default_open_notional: float) -> Tuple[float, bool, str]:
    if price <= 0:
        return 0.0, False, "no price for scale-in"
    if not ((signal == 1 and side == "long" and quantity > 0)
            or (signal == -1 and side == "short" and quantity > 0)):
        return 0.0, False, "not a same-direction add"

    max_adds = int(scale_cfg.get("max_adds", 0) or 0)
    if max_adds > 0 and scale_in_count >= max_adds:
        return 0.0, False, "scale-in max_adds reached"

    add_notional = default_open_notional
    if (scale_cfg.get("add_notional_usd", 0) or 0) > 0:
        add_notional = float(scale_cfg["add_notional_usd"])
    if add_notional <= 0:
        return 0.0, False, "scale-in add notional resolves to zero"
    max_added = float(scale_cfg.get("max_added_notional_usd", 0) or 0)
    if max_added > 0 and added_notional_usd + add_notional > max_added + 1e-9:
        return 0.0, False, "scale-in max_added_notional_usd reached"

    spacing = float(scale_cfg.get("add_spacing_atr", 0) or 0)
    if spacing != 0:
        if entry_atr <= 0:
            return 0.0, False, "scale-in spacing requires a positive EntryATR"
        last_add = last_add_price
        if last_add <= 0:
            last_add = avg_cost
        direction = -1.0 if side == "short" else 1.0
        favorable_move = (price - last_add) * direction
        needed = spacing * entry_atr
        if spacing > 0:
            if favorable_move + 1e-9 < needed:
                return 0.0, False, "scale-in spacing (add-to-winners) not reached"
        else:
            if -favorable_move + 1e-9 < -needed:
                return 0.0, False, "scale-in spacing (average-down) not reached"

    return add_notional / price, True, ""


def _ungated_leg_notional(leg_notional: float, hurst_size_mult: float) -> float:
    if hurst_size_mult > 0:
        return leg_notional / hurst_size_mult
    return leg_notional


class Trade:
    def __init__(self, entry_date, entry_price, side="long"):
        self.entry_date = entry_date
        self.entry_price = entry_price
        self.side = side
        self.exit_date = None
        self.exit_price = None
        self.pnl = 0.0
        self.pnl_pct = 0.0
        self.shares = 0.0
        self.bars_held = 0
        self.mfe_pct = 0.0
        self.mae_pct = 0.0
        self.bars_to_mfe = 0
        self.bars_to_mae = 0
        self.entry_atr = 0.0
        self.entry_fee = 0.0
        self.exit_fee = 0.0
        self.exit_reason = ""
        self.scale_in_adds = 0

    def close(self, exit_date, exit_price):
        self.exit_date = exit_date
        self.exit_price = exit_price
        if self.side == "long":
            self.pnl_pct = (exit_price - self.entry_price) / self.entry_price
        else:
            self.pnl_pct = (self.entry_price - exit_price) / self.entry_price
        self.pnl = self.shares * self.entry_price * self.pnl_pct

    def to_dict(self):
        return {
            "entry_date": str(self.entry_date),
            "exit_date": str(self.exit_date),
            "entry_price": self.entry_price,
            "exit_price": self.exit_price,
            "side": self.side,
            "shares": self.shares,
            "pnl": round(self.pnl, 2),
            "pnl_pct": round(self.pnl_pct * 100, 2),
            "bars_held": self.bars_held,
            "mfe_pct": round(self.mfe_pct * 100, 4),
            "mae_pct": round(self.mae_pct * 100, 4),
            "bars_to_mfe": self.bars_to_mfe,
            "bars_to_mae": self.bars_to_mae,
            "entry_atr": round(self.entry_atr, 6),
            "entry_fee": round(self.entry_fee, 6),
            "exit_fee": round(self.exit_fee, 6),
            "exit_reason": self.exit_reason,
            "scale_in_adds": self.scale_in_adds,
        }


class _HoldTracker:

    __slots__ = ("bars", "high", "low", "high_bar", "low_bar",
                 "entry_fee", "entry_fee_netted", "entry_price", "side")

    def __init__(self):
        self.open(0.0, "long", 0.0)

    def open(self, entry_price: float, side: str, entry_fee: float) -> None:
        self.bars = 0
        self.high = entry_price
        self.low = entry_price
        self.high_bar = 0
        self.low_bar = 0
        self.entry_fee = entry_fee
        self.entry_fee_netted = 0.0
        self.entry_price = entry_price
        self.side = side

    def step(self, high: float, low: float) -> None:
        self.bars += 1
        if high > self.high:
            self.high = high
            self.high_bar = self.bars
        if low < self.low:
            self.low = low
            self.low_bar = self.bars

    def metrics(self):
        e = self.entry_price
        if e <= 0:
            return 0.0, 0.0, 0, 0
        if self.side == "long":
            return (self.high - e) / e, (self.low - e) / e, self.high_bar, self.low_bar
        return (e - self.low) / e, (e - self.high) / e, self.low_bar, self.high_bar


def _stamp_hold(trade, hold: "_HoldTracker", *, entry_atr: float,
                exit_fee: float, reason: str, qty_frac: float = 1.0,
                true_up_entry_fee: bool = False) -> None:
    mfe, mae, b_mfe, b_mae = hold.metrics()
    trade.bars_held = hold.bars
    trade.mfe_pct = mfe
    trade.mae_pct = mae
    trade.bars_to_mfe = b_mfe
    trade.bars_to_mae = b_mae
    trade.entry_atr = entry_atr
    if true_up_entry_fee:
        trade.entry_fee = max(0.0, hold.entry_fee - hold.entry_fee_netted)
    else:
        trade.entry_fee = hold.entry_fee * qty_frac
    hold.entry_fee_netted += trade.entry_fee
    trade.exit_fee = exit_fee
    trade.exit_reason = reason
    trade.pnl -= trade.entry_fee + trade.exit_fee


class Backtester:

    def __init__(self, initial_capital: float = 1000.0,
                 commission_pct: Optional[float] = None,
                 slippage_pct: float = 0.0005,
                 platform: str = "binanceus",
                 open_strategy: Optional[dict] = None,
                 close_strategies: Optional[list[dict]] = None,
                 regime_enabled: bool = False,
                 regime_period: int = 14,
                 regime_adx_threshold: float = 20.0,
                 regime_windows_spec: Optional[dict] = None,
                 hurst_gate: Optional[dict] = None,
                 allowed_regimes: Optional[list[str]] = None,
                 regime_gate_on_failure: str = "open",
                 stop_loss_atr_mult: Optional[float] = None,
                 stop_loss_pct: Optional[float] = None,
                 stop_loss_margin_pct: Optional[float] = None,
                 trailing_stop_atr_mult: Optional[float] = None,
                 trailing_stop_pct: Optional[float] = None,
                 stop_loss_atr_regime: Optional[dict] = None,
                 trail_stop_atr_regime: Optional[dict] = None,
                 strategy_type: str = "perps",
                 direction: Optional[str] = None,
                 invert_signal: bool = False,
                 regime_directional_policy: Optional[dict] = None,
                 regime_directional_certified: bool = False,
                 regime_directional_certified_states: Optional[dict] = None,
                 regime_timeframe: Optional[str] = None,
                 profile_allocation: Optional[dict] = None,
                 intrabar_resolution: str = "ohlc_walk",
                 risk_per_trade_pct: Optional[float] = None,
                 allow_scale_in: bool = False,
                 scale_in: Optional[dict] = None,
                 atr_method: str = "simple"):
        self.initial_capital = initial_capital
        self.platform = platform
        self.intrabar_resolution = str(intrabar_resolution or "").strip().lower()
        if self.intrabar_resolution not in ("ohlc_walk", "bar_close"):
            raise ValueError(
                f"intrabar_resolution must be 'ohlc_walk' or 'bar_close', "
                f"got {intrabar_resolution!r}"
            )
        self.commission_pct = (
            commission_pct if commission_pct is not None
            else fee_pct_for_platform(platform)
        )
        self.slippage_pct = slippage_pct
        self.open_strategy = dict(open_strategy or {})
        self._close_refs: list[dict] = []
        for ref in close_strategies or []:
            if not isinstance(ref, dict):
                raise ValueError(
                    f"close_strategies entries must be dicts of shape "
                    f"{{'name': str, 'params': dict}}, got {type(ref).__name__}"
                )
            name = (ref.get("name") or "").strip()
            if not name:
                raise ValueError(f"close_strategies ref missing 'name': {ref}")
            params = dict(ref.get("params") or {})
            name, params = _rewrite_deprecated_close_ref(name, params)
            self._close_refs.append({
                "name": name,
                "params": params,
            })
        self.close_strategies = [r["name"] for r in self._close_refs]
        self.close_params = {r["name"]: r["params"] for r in self._close_refs}
        self.regime_enabled = regime_enabled
        self.regime_timeframe = str(regime_timeframe or "").strip() or None
        self.regime_period = regime_period
        self.regime_adx_threshold = regime_adx_threshold
        self.regime_windows_spec = dict(regime_windows_spec) if regime_windows_spec else None
        self._regime_primary_labels = _regime_primary_labels(self.regime_windows_spec)
        self.hurst_gate = dict(hurst_gate) if hurst_gate else None
        self.allowed_regimes = list(allowed_regimes or [])
        _norm_gate = str(regime_gate_on_failure or "").strip().lower() or "open"
        if _norm_gate not in ("open", "closed"):
            raise ValueError(
                f"regime_gate_on_failure must be 'open' or 'closed', "
                f"got {regime_gate_on_failure!r}"
            )
        self.regime_gate_on_failure = _norm_gate
        _norm_atr_method = str(atr_method or "").strip().lower() or "simple"
        if _norm_atr_method not in ("simple", "wilder"):
            raise ValueError(
                f"atr_method must be 'simple' or 'wilder', got {atr_method!r}"
            )
        self.atr_method = _norm_atr_method
        self.stop_loss_atr_mult = stop_loss_atr_mult
        self.stop_loss_pct = stop_loss_pct
        self.stop_loss_margin_pct = stop_loss_margin_pct
        self.trailing_stop_atr_mult = trailing_stop_atr_mult
        self.trailing_stop_pct = trailing_stop_pct
        self.strategy_type = strategy_type
        self.direction = (str(direction).strip().lower() if direction else None)
        self.invert_signal = bool(invert_signal)
        self.regime_directional_policy = _normalize_regime_directional_policy(
            regime_directional_policy,
        )
        if self.regime_directional_policy is not None:
            if regime_directional_certified_states is not None:
                cert_states = regime_directional_certified_states
            elif bool(regime_directional_certified):
                cert_states = None
            else:
                cert_states = {}
            self.regime_directional_policy = _gate_directional_policy_by_states(
                self.regime_directional_policy, cert_states,
            )
            if not self.regime_directional_policy:
                print("[#1085] regime_directional_policy present but NOT certified "
                      "(or no state survives the per-state sign gate) for this "
                      "(asset,timeframe,classifier) — DEFAULT-OFF in backtest "
                      "(base direction), mirroring live (#1076 negative result).",
                      file=sys.stderr)
                self.regime_directional_policy = None
        if self.regime_directional_policy is not None and not self.regime_enabled:
            raise ValueError(
                "regime_directional_policy requires regime_enabled=True"
            )
        self._profile_alloc = _parse_profile_allocation(profile_allocation)
        self.stop_loss_atr_regime = (
            dict(stop_loss_atr_regime) if stop_loss_atr_regime else None
        )
        self.trail_stop_atr_regime = (
            dict(trail_stop_atr_regime) if trail_stop_atr_regime else None
        )
        self._stop_loss_regime_block = None
        self._trailing_stop_regime_block = None
        self._uses_regime_tiered_close = _close_refs_use_regime_tiered_tp(
            self._close_refs,
        )
        self._unified_close_params: Optional[dict] = None
        self._unified_scalar_params = None
        self._uses_trailing_ratchet_close = any(
            (r.get("name") or "").strip().lower()
            in ("trailing_tp_ratchet", "trailing_tp_ratchet_regime")
            for r in self._close_refs
        )
        _zscore_refs = [
            r for r in self._close_refs
            if (r.get("name") or "").strip().lower() == "zscore_target"
        ]
        if len(_zscore_refs) > 1:
            raise ValueError(
                "duplicate zscore_target close refs are not supported "
                "(close params are keyed by name; the second would silently "
                "override the first's lookback)"
            )
        self._zscore_lookback = 0
        if _zscore_refs:
            try:
                self._zscore_lookback = int(
                    (_zscore_refs[0].get("params") or {}).get("lookback", 0) or 0
                )
            except (TypeError, ValueError):
                self._zscore_lookback = 0
        self._ratchet_mod = None
        self._ratchet_ref: Optional[dict] = None
        self._ratchet_tiers_run: list = []
        if self._uses_trailing_ratchet_close:
            self._ratchet_mod = _load_trailing_ratchet()
            for ref in self._close_refs:
                n = (ref.get("name") or "").strip().lower()
                if n in ("trailing_tp_ratchet", "trailing_tp_ratchet_regime"):
                    self._ratchet_ref = ref
                    break
            _regime_ratchet = (
                (self._ratchet_ref or {}).get("name") or ""
            ).strip().lower() == "trailing_tp_ratchet_regime"
            if _regime_ratchet:
                if self.trail_stop_atr_regime is None:
                    raise ValueError(
                        "trailing_tp_ratchet_regime requires trail_stop_atr_regime"
                    )
            elif (
                self.trailing_stop_atr_mult is None
                or self.trailing_stop_atr_mult <= 0
            ):
                raise ValueError(
                    "trailing_tp_ratchet requires trailing_stop_atr_mult > 0"
                )
            if self.trailing_stop_pct is not None and self.trailing_stop_pct > 0:
                raise ValueError(
                    "trailing_tp_ratchet* cannot combine with trailing_stop_pct"
                )
        _needs_regime_atr = (
            self.stop_loss_atr_regime is not None
            or self.trail_stop_atr_regime is not None
            or self._uses_regime_tiered_close
        )
        if _needs_regime_atr:
            _ensure_close_strategies_path()
            from regime_atr import (
                SURFACE_STOP_LOSS,
                SURFACE_TRAILING,
                close_params_are_unified_regime,
                parse_regime_atr_block,
                resolve_regime_atr,
                unified_regime_scalar_params,
                validate_unified_regime_close,
            )

            self._unified_close_params = None
            self._unified_scalar_params = unified_regime_scalar_params
            for _ref in self._close_refs:
                _n = (_ref.get("name") or "").strip().lower()
                if _n not in ("tiered_tp_atr_regime", "tiered_tp_atr_live_regime"):
                    continue
                _params = _ref.get("params") or {}
                if close_params_are_unified_regime(_params):
                    self._unified_close_params = dict(_params)
                break
            if self._unified_close_params is not None:
                _unified_errs = validate_unified_regime_close(
                    self._unified_close_params,
                    labels=self._regime_primary_labels,
                )
                if _unified_errs:
                    raise ValueError(
                        "Invalid unified per-regime close block: "
                        + "; ".join(_unified_errs)
                    )
                _sole_owner_conflicts = [
                    ("stop_loss_atr_mult", self.stop_loss_atr_mult),
                    ("stop_loss_pct", self.stop_loss_pct),
                    ("stop_loss_margin_pct", self.stop_loss_margin_pct),
                    ("trailing_stop_atr_mult", self.trailing_stop_atr_mult),
                    ("trailing_stop_pct", self.trailing_stop_pct),
                ]
                for _field, _val in _sole_owner_conflicts:
                    if _val is not None and _val > 0:
                        raise ValueError(
                            f"{_field} is not allowed alongside a unified "
                            "per-regime close — the close owns the SL via "
                            "per-regime stop_loss_atr"
                        )
                if self.stop_loss_atr_regime is not None or (
                    self.trail_stop_atr_regime is not None
                ):
                    raise ValueError(
                        "stop_loss_atr_regime/trail_stop_atr_regime are not "
                        "allowed alongside a unified per-regime close — the "
                        "close owns the SL via per-regime stop_loss_atr"
                    )

            regime_errs: list[str] = []
            if self.stop_loss_atr_regime is not None:
                blk, errs = parse_regime_atr_block(
                    self.stop_loss_atr_regime,
                    "stop_loss_atr_regime",
                    SURFACE_STOP_LOSS,
                    labels=self._regime_primary_labels,
                )
                regime_errs.extend(errs)
                self._stop_loss_regime_block = blk
            if self.trail_stop_atr_regime is not None:
                blk, errs = parse_regime_atr_block(
                    self.trail_stop_atr_regime,
                    "trail_stop_atr_regime",
                    SURFACE_TRAILING,
                    labels=self._regime_primary_labels,
                )
                regime_errs.extend(errs)
                self._trailing_stop_regime_block = blk
            if regime_errs:
                raise ValueError(
                    "Invalid regime ATR stop configuration: " + "; ".join(regime_errs)
                )

            def _active_regime_sl(blk) -> bool:
                return blk is not None and not blk.is_zero()

            if _active_regime_sl(self._stop_loss_regime_block):
                if (
                    self.stop_loss_atr_mult is not None
                    and self.stop_loss_atr_mult > 0
                ):
                    raise ValueError(
                        "stop_loss_atr_regime is mutually exclusive with "
                        "stop_loss_atr_mult"
                    )
                if self.stop_loss_pct is not None and self.stop_loss_pct > 0:
                    raise ValueError(
                        "stop_loss_atr_regime is mutually exclusive with "
                        "stop_loss_pct"
                    )
                if (
                    self.stop_loss_margin_pct is not None
                    and self.stop_loss_margin_pct > 0
                ):
                    raise ValueError(
                        "stop_loss_atr_regime is mutually exclusive with "
                        "stop_loss_margin_pct"
                    )
                if self.trailing_stop_pct is not None and self.trailing_stop_pct > 0:
                    raise ValueError(
                        "stop_loss_atr_regime is mutually exclusive with "
                        "trailing_stop_pct"
                    )
                if (
                    self.trailing_stop_atr_mult is not None
                    and self.trailing_stop_atr_mult > 0
                ):
                    raise ValueError(
                        "stop_loss_atr_regime is mutually exclusive with "
                        "trailing_stop_atr_mult"
                    )
                if _active_regime_sl(self._trailing_stop_regime_block):
                    raise ValueError(
                        "stop_loss_atr_regime is mutually exclusive with "
                        "trail_stop_atr_regime"
                    )

            if _active_regime_sl(self._trailing_stop_regime_block):
                if (
                    self.trailing_stop_atr_mult is not None
                    and self.trailing_stop_atr_mult > 0
                ):
                    raise ValueError(
                        "trail_stop_atr_regime is mutually exclusive with "
                        "trailing_stop_atr_mult"
                    )
                if self.trailing_stop_pct is not None and self.trailing_stop_pct > 0:
                    raise ValueError(
                        "trail_stop_atr_regime is mutually exclusive with "
                        "trailing_stop_pct"
                    )
                if self.stop_loss_pct is not None and self.stop_loss_pct > 0:
                    raise ValueError(
                        "trail_stop_atr_regime is mutually exclusive with "
                        "stop_loss_pct"
                    )
                if (
                    self.stop_loss_margin_pct is not None
                    and self.stop_loss_margin_pct > 0
                ):
                    raise ValueError(
                        "trail_stop_atr_regime is mutually exclusive with "
                        "stop_loss_margin_pct"
                    )
                if (
                    self.stop_loss_atr_mult is not None
                    and self.stop_loss_atr_mult > 0
                ):
                    raise ValueError(
                        "trail_stop_atr_regime is mutually exclusive with "
                        "stop_loss_atr_mult"
                    )
            self._resolve_regime_atr = resolve_regime_atr
        else:
            self._resolve_regime_atr = None
            _evaluate, list_strategies = _load_close_registry()
            available = set(list_strategies())
            for name in self.close_strategies:
                if name not in available:
                    raise ValueError(
                        f"Unknown close strategy: {name}. "
                        f"Available: {sorted(available)}"
                    )

        self._sl_mod = _load_post_tp_sl()
        _tier_vocab_errs = self._sl_mod.validate_regime_tiered_tp_labels(
            self._close_refs, labels=self._regime_primary_labels,
        )
        if _tier_vocab_errs:
            raise ValueError(
                "Invalid regime tiered-TP configuration: " + "; ".join(_tier_vocab_errs)
            )
        self._sl_after_rules_static, _sl_parse_errs = (
            self._sl_mod.parse_strategy_tp_sl_after_rules(
                self._close_refs, labels=self._regime_primary_labels)
        )
        self._tp_tier_thresholds_static = self._sl_mod.parse_tp_tier_close_fractions(
            self._close_refs,
        )
        self._active_sl_after_rules = self._sl_after_rules_static
        self._run_tp_tier_thresholds = list(self._tp_tier_thresholds_static)
        self._run_stop_loss_atr_mult: Optional[float] = None
        self._run_trailing_stop_atr_mult: Optional[float] = None
        self._run_position_regime = ""
        any_sl_after_key = False
        for ref in self._close_refs:
            params = ref.get("params") or {}
            if "sl_after" in params:
                any_sl_after_key = True
                break
            tiers_raw = params.get("tp_tiers", params.get("tiers"))
            if isinstance(tiers_raw, list) and any(
                isinstance(t, dict) and "sl_after" in t for t in tiers_raw
            ):
                any_sl_after_key = True
                break
        self._any_sl_after_key = any_sl_after_key
        self._sl_after_pipeline_enabled = (
            self._sl_after_rules_static.has_any() or any_sl_after_key
        )
        if (
            self._sl_after_rules_static.has_any()
            or _sl_parse_errs
            or any_sl_after_key
        ):
            errs = self._sl_mod.validate_post_tp_stop_loss_rules(
                self._close_refs,
                stop_loss_atr_mult=self.stop_loss_atr_mult,
                stop_loss_pct=self.stop_loss_pct,
                stop_loss_margin_pct=self.stop_loss_margin_pct,
                trailing_stop_atr_mult=self.trailing_stop_atr_mult,
                trailing_stop_pct=self.trailing_stop_pct,
                stop_loss_atr_regime=self.stop_loss_atr_regime,
                strategy_type=self.strategy_type,
                labels=self._regime_primary_labels,
            )
            if errs:
                raise ValueError(
                    "Invalid sl_after configuration: " + "; ".join(errs)
                )
            if self._sl_after_rules_static.has_any():
                regime_rules = []
                if self._sl_after_rules_static.default.has_regime():
                    regime_rules.append("strategy-level default")
                for idx, r in enumerate(self._sl_after_rules_static.per_tier):
                    if r.has_regime():
                        regime_rules.append(f"tier[{idx}]")
                if regime_rules:
                    raise ValueError(
                        "Invalid sl_after configuration: regime-aware "
                        "trend_regime block is HL-live-only in this release "
                        "(backtester parity deferred — see #736). Found on: "
                        + ", ".join(regime_rules)
                        + ". Use the scalar atr_mult / trail_from_here.atr_mult "
                        "form for backtesting."
                    )
                has_atr_sl = (
                    (
                        self.stop_loss_atr_mult is not None
                        and self.stop_loss_atr_mult > 0
                    )
                    or (
                        self._stop_loss_regime_block is not None
                        and not self._stop_loss_regime_block.is_zero()
                    )
                )
                has_pct_sl = (
                    self.stop_loss_pct is not None and self.stop_loss_pct > 0
                )
                has_margin_sl = (
                    self.stop_loss_margin_pct is not None
                    and self.stop_loss_margin_pct > 0
                )
                if has_margin_sl and not (has_atr_sl or has_pct_sl):
                    raise ValueError(
                        "Invalid sl_after configuration: "
                        "stop_loss_margin_pct cannot be the sole fixed SL "
                        "in backtests — the backtester does not model "
                        "leverage, so the pre-TP SL would never fire and "
                        "the post-TP bump would diverge from live. Use "
                        "stop_loss_atr_mult or stop_loss_pct."
                    )

        self.risk_per_trade_pct: Optional[float] = None
        if risk_per_trade_pct is not None:
            pct = float(risk_per_trade_pct)
            if not (0 < pct <= 10):
                raise ValueError(
                    f"risk_per_trade_pct must be in (0, 10], got {pct}"
                )
            if self._unified_close_params is not None:
                raise ValueError(
                    "risk_per_trade_pct cannot size from the unified "
                    "per-regime close block — its SL resolves per-regime "
                    "after open, so the stop distance is unknowable at "
                    "sizing time (#1268; live rejects this at config load)"
                )
            if self.stop_loss_atr_regime or self.trail_stop_atr_regime:
                raise ValueError(
                    "risk_per_trade_pct cannot size from a regime-resolved "
                    "stop owner (stop_loss_atr_regime / "
                    "trail_stop_atr_regime) — the SL resolves from the "
                    "regime stamped after open (#1268; live rejects this at "
                    "config load)"
                )
            has_atr_owner = (
                (self.trailing_stop_atr_mult or 0) > 0
                or (self.stop_loss_atr_mult or 0) > 0
            )
            has_pct_owner = (
                (self.trailing_stop_pct or 0) > 0
                or (self.stop_loss_pct or 0) > 0
            )
            if not (has_atr_owner or has_pct_owner):
                if (self.stop_loss_margin_pct or 0) > 0:
                    raise ValueError(
                        "risk_per_trade_pct cannot size from a "
                        "stop_loss_margin_pct-only stop in backtests — the "
                        "backtester does not model leverage, so the price "
                        "distance cannot be derived. Use stop_loss_atr_mult, "
                        "trailing_stop_atr_mult, stop_loss_pct, or "
                        "trailing_stop_pct."
                    )
                raise ValueError(
                    "risk_per_trade_pct requires an explicit stop owner "
                    "(stop_loss_atr_mult, trailing_stop_atr_mult, "
                    "stop_loss_pct, or trailing_stop_pct) to derive the "
                    "stop distance from (#1268)"
                )
            self.risk_per_trade_pct = pct
        self._risk_cap_warned = False
        self._risk_skip_warned = False

        self.allow_scale_in = bool(allow_scale_in)
        if scale_in and not self.allow_scale_in:
            raise ValueError(
                "scale_in block is set but allow_scale_in is false — enable "
                "allow_scale_in or remove the block (the live daemon rejects "
                "this config at startup)"
            )
        self._scale_in_cfg = _normalize_scale_in_cfg(scale_in)
        if self.allow_scale_in and self.risk_per_trade_pct is not None:
            raise ValueError(
                "allow_scale_in is mutually exclusive with risk_per_trade_pct "
                "(#1268: add legs re-size off the frozen SL geometry, breaking "
                "the constant-dollar-risk invariant; the live daemon rejects "
                "this config at startup)"
            )

    def _apply_direction_invert(self, sig_int: pd.Series,
                                uses_open_close: bool) -> pd.Series:
        return sig_int.map(
            lambda s: _apply_direction_invert_value(
                int(s), uses_open_close, self.direction, self.invert_signal,
            )
        ).astype(int)

    def _effective_directional_entry(
        self, current_regime: str, position_regime: str, position_qty: float,
    ) -> tuple[str, bool]:
        entry = _resolve_regime_directional_entry(
            self.regime_directional_policy,
            current_regime,
            position_regime,
            abs(position_qty),
        )
        if entry is None:
            return self.direction or "", self.invert_signal
        return str(entry["direction"]), bool(entry["invert_signal"])

    def _normalize_profile_signals(self, df: pd.DataFrame, uses_open_close: bool) -> None:
        for p in self._profile_alloc["names"]:
            col = "signal__" + p
            sig_raw = df[col].fillna(0).astype(float)
            non_integral = sig_raw[sig_raw != sig_raw.round()]
            if not non_integral.empty:
                raise ValueError(
                    f"{col} must be in {{-1, 0, 1}} — got non-integral values "
                    f"{sorted(set(non_integral.unique().tolist()))}"
                )
            sig_int = sig_raw.astype(int)
            bad = sig_int[~sig_int.isin([-1, 0, 1])]
            if not bad.empty:
                raise ValueError(
                    f"{col} must be in {{-1, 0, 1}} — got unexpected values "
                    f"{sorted(bad.unique().tolist())}"
                )
            if self.regime_directional_policy is None:
                sig_int = self._apply_direction_invert(sig_int, uses_open_close)
            if uses_open_close:
                df["_open_action__" + p] = (
                    sig_int.map(_open_action_from_signal).shift(1).fillna("none")
                )
            df[col] = sig_int.shift(1).fillna(0).astype(int)
        df["signal"] = 0
        if uses_open_close:
            df["_open_action"] = "none"
            df["_close_fraction"] = _max_close_fraction_series(df).shift(1).fillna(0.0)
        df["_profile_label"] = df["_profile_label"].shift(1).fillna("")

    def run(self, df: pd.DataFrame, strategy_name: str = "Unknown",
            symbol: str = "BTC/USDT", timeframe: str = "1d",
            params: Optional[dict] = None, save: bool = True,
            starting_long: Optional[dict] = None) -> dict:
        uses_open_close = (
            "open_action" in df.columns
            or bool(_close_fraction_columns(df))
            or bool(self.close_strategies)
        )
        if self.direction == "both" and not uses_open_close:
            raise ValueError(
                "direction='both' requires a close evaluator (open/close "
                "engine path) — the plain single-leg path cannot open one "
                "side and close the other, so the run would silently score "
                "long/flat. Backtest each leg separately with "
                "direction='long' / direction='short'."
            )
        if self.regime_directional_policy is not None and not uses_open_close:
            both_labels = sorted(
                label for label, entry in self.regime_directional_policy.items()
                if entry.get("direction") == "both"
            )
            if both_labels:
                raise ValueError(
                    "regime_directional_policy direction='both' requires a "
                    "close evaluator on the plain signal path; labels with "
                    f"both: {both_labels}"
                )
        plain_short = (not uses_open_close) and self.direction == "short"
        if plain_short and starting_long:
            raise ValueError(
                "starting_long cannot seed a direction='short' plain-path "
                "run — the short/flat path never emits a long close, so the "
                "seeded long would be carried untouched to end-of-data."
            )
        has_profile_alloc = self._profile_alloc is not None
        if has_profile_alloc:
            if "_profile_label" not in df.columns:
                raise ValueError(
                    "regime_profile_allocation backtest requires a '_profile_label' column"
                )
            missing = [
                p for p in self._profile_alloc["names"]
                if ("signal__" + p) not in df.columns
            ]
            if missing:
                raise ValueError(
                    f"regime_profile_allocation backtest is missing signal columns "
                    f"for profiles {missing} (expected 'signal__<profile>')"
                )
        if "signal" not in df.columns and not uses_open_close and not has_profile_alloc:
            raise ValueError("DataFrame must have a 'signal' column or open_action/close_fraction columns")

        df = df.copy()
        if has_profile_alloc:
            self._normalize_profile_signals(df, uses_open_close)
        elif "signal" in df.columns:
            sig_raw = df["signal"].fillna(0).astype(float)
            non_integral = sig_raw[sig_raw != sig_raw.round()]
            if not non_integral.empty:
                raise ValueError(
                    f"signal column must be in {{-1, 0, 1}} — got "
                    f"non-integral values {sorted(set(non_integral.unique().tolist()))}"
                )
            sig_int = sig_raw.astype(int)
            bad = sig_int[~sig_int.isin([-1, 0, 1])]
            if not bad.empty:
                raise ValueError(
                    f"signal column must be in {{-1, 0, 1}} — got "
                    f"unexpected values {sorted(bad.unique().tolist())}"
                )
            if self.regime_directional_policy is None:
                sig_int = self._apply_direction_invert(sig_int, uses_open_close)
            signal_for_open = sig_int
            df["signal"] = sig_int.shift(1).fillna(0).astype(int)
        else:
            signal_for_open = pd.Series(0, index=df.index)
            df["signal"] = 0

        if uses_open_close and not has_profile_alloc:
            if "open_action" in df.columns:
                open_actions = df["open_action"].map(_normalize_open_action)
            else:
                open_actions = signal_for_open.map(_open_action_from_signal)
            df["_open_action"] = open_actions.shift(1).fillna("none")
            df["_close_fraction"] = _max_close_fraction_series(df).shift(1).fillna(0.0)

        if "entry_fraction" in df.columns:
            df["_entry_fraction"] = (
                _validated_entry_fraction_series(df).shift(1).fillna(1.0)
            )

        if self.regime_enabled and "regime" not in df.columns:
            ensure_regime = _load_regime()
            ensure_regime(
                df,
                period=self.regime_period,
                adx_threshold=self.regime_adx_threshold,
                windows_spec=self.regime_windows_spec,
            )

        if "regime" in df.columns:
            df["_regime_bar_close"] = df["regime"].copy()

        if self.regime_enabled and "regime" in df.columns:
            df["regime"] = df["regime"].shift(1).fillna("")

        hurst_runner = None
        if self.hurst_gate and self.hurst_gate.get("enabled"):
            from hurst_gate import HurstGate, hurst_live_frame_bars, rolling_hurst

            hurst_runner = HurstGate(self.hurst_gate)
            frame_bars = hurst_live_frame_bars(
                self.regime_windows_spec, self.regime_period
            )
            df["_hurst"] = rolling_hurst(df["close"], frame_bars).shift(1)

        has_open = "open" in df.columns

        def _entry_stamp(row) -> str:
            if self.regime_enabled:
                return str(row.get("regime", "") or "").strip()
            return str(row.get("_regime_bar_close", "") or "").strip()

        def _bar_close_regime(row) -> str:
            return str(row.get("_regime_bar_close", "") or "").strip()

        cash = self.initial_capital
        position = 0.0
        trades = []
        current_trade = None
        equity_curve = []

        avg_cost = 0.0
        initial_quantity = 0.0
        entry_atr_value = 0.0
        pending_close_fraction = 0.0
        pending_close_reason = ""
        hold = _HoldTracker()
        scale = _ScaleInState()
        scale_in_adds_total = 0
        scale_in_added_notional_total = 0.0

        sl_trigger_px = 0.0
        sl_tiers_processed = 0
        post_tp_trail_mult: Optional[float] = None
        sl_high_water_px = 0.0

        pending_signal_sl_close = False
        walk_mode = self.intrabar_resolution == "ohlc_walk"
        sl_pierce_armed = False
        self._active_sl_after_rules = self._sl_after_rules_static
        self._run_tp_tier_thresholds = list(self._tp_tier_thresholds_static)
        self._run_stop_loss_atr_mult: Optional[float] = None
        self._run_trailing_stop_atr_mult: Optional[float] = None
        self._run_position_regime = ""
        sl_after_active = self._sl_after_pipeline_enabled
        trailing_ratchet_active = self._uses_trailing_ratchet_close

        zscore_series = None
        if self._zscore_lookback > 0 and "close" in df.columns:
            lb = self._zscore_lookback
            closes = df["close"].astype(float)
            roll = closes.rolling(lb)
            std = roll.std(ddof=0)
            zscore_series = (closes - roll.mean()) / std.replace(0.0, float("nan"))

        avwap_series = df["avwap"] if "avwap" in df.columns else None
        if self._close_names_include_avwap_stop():
            avwap_usable = avwap_series is not None and bool(
                (pd.to_numeric(avwap_series, errors="coerce") > 0).any()
            )
            if not avwap_usable:
                from strategy_composition import warn_avwap_stop_missing_context
                warn_avwap_stop_missing_context()

        atr_series = df["atr"] if "atr" in df.columns else None
        if atr_series is None and (
            (self.stop_loss_atr_mult is not None and self.stop_loss_atr_mult > 0)
            or (self.trailing_stop_atr_mult is not None and self.trailing_stop_atr_mult > 0)
        ):
            atr_series = standard_atr(df, method=self.atr_method)

        def _initial_trail_trigger(side: str, mark: float, entry_atr: float,
                                    trail_mult: float) -> float:
            if mark <= 0 or entry_atr <= 0 or trail_mult <= 0:
                return 0.0
            if side == "long":
                return mark - trail_mult * entry_atr
            if side == "short":
                return mark + trail_mult * entry_atr
            return 0.0

        def stamp_open_from_label(stamp: str) -> None:
            lab = (stamp or "").strip()
            self._run_position_regime = lab
            if self._uses_regime_tiered_close:
                rules_rt, _ = self._sl_mod.parse_strategy_tp_sl_after_rules(
                    self._close_refs, regime=lab,
                    labels=self._regime_primary_labels,
                )
                self._active_sl_after_rules = rules_rt
                self._run_tp_tier_thresholds = self._sl_mod.parse_tp_tier_close_fractions(
                    self._close_refs, regime=lab,
                )
            else:
                self._active_sl_after_rules = self._sl_after_rules_static
                self._run_tp_tier_thresholds = list(self._tp_tier_thresholds_static)

            self._ratchet_tiers_run = []
            if self._uses_trailing_ratchet_close and self._ratchet_mod and self._ratchet_ref:
                regime_table = (
                    (self._ratchet_ref.get("name") or "").strip().lower()
                    == "trailing_tp_ratchet_regime"
                )
                tiers, terr = self._ratchet_mod.resolve_tiers_for_regime(
                    self._ratchet_ref.get("params") or {},
                    lab,
                    regime_table=regime_table,
                )
                if terr:
                    raise ValueError(
                        "trailing_tp_ratchet tier resolution failed: "
                        + "; ".join(terr)
                    )
                self._ratchet_tiers_run = tiers

            self._run_stop_loss_atr_mult = None
            self._run_trailing_stop_atr_mult = None
            if self._resolve_regime_atr is not None and lab:
                if (
                    self._stop_loss_regime_block is not None
                    and not self._stop_loss_regime_block.is_zero()
                ):
                    self._run_stop_loss_atr_mult = self._resolve_regime_atr(
                        self._stop_loss_regime_block, lab,
                    )
                if (
                    self._trailing_stop_regime_block is not None
                    and not self._trailing_stop_regime_block.is_zero()
                ):
                    self._run_trailing_stop_atr_mult = self._resolve_regime_atr(
                        self._trailing_stop_regime_block, lab,
                    )
            if (
                self._run_stop_loss_atr_mult is None
                and self._unified_close_params is not None
                and self._unified_scalar_params is not None
                and lab
            ):
                _, _usl = self._unified_scalar_params(
                    self._unified_close_params, lab
                )
                if _usl and _usl > 0:
                    self._run_stop_loss_atr_mult = float(_usl)
            if self._run_stop_loss_atr_mult is None:
                if (
                    self.stop_loss_atr_mult is not None
                    and self.stop_loss_atr_mult > 0
                ):
                    self._run_stop_loss_atr_mult = self.stop_loss_atr_mult
            if self._run_trailing_stop_atr_mult is None:
                if (
                    self.trailing_stop_atr_mult is not None
                    and self.trailing_stop_atr_mult > 0
                ):
                    self._run_trailing_stop_atr_mult = self.trailing_stop_atr_mult

        if starting_long:
            effective_entry = starting_long["entry_price"]
            entry_commission = self.initial_capital * self.commission_pct
            available = self.initial_capital - entry_commission
            position = available / effective_entry
            cash = 0.0
            current_trade = Trade(
                starting_long.get("entry_date", df.index[0]),
                effective_entry, "long",
            )
            current_trade.shares = position
            avg_cost = effective_entry
            initial_quantity = position
            scale.reset()
            scale.base_open_notional = position * effective_entry
            hold.open(effective_entry, "long", entry_commission)
            seed_atr = starting_long.get("entry_atr", 0.0)
            try:
                seed_atr = float(seed_atr or 0.0)
            except (TypeError, ValueError):
                seed_atr = 0.0
            if seed_atr > 0 and seed_atr <= 0.5 * effective_entry:
                entry_atr_value = seed_atr
            stamp = str(starting_long.get("entry_regime", "") or "").strip()
            if not stamp:
                stamp = _entry_stamp(df.iloc[0])
            stamp_open_from_label(stamp)
            seed_hwm = starting_long.get("high_water", 0.0)
            try:
                seed_hwm = float(seed_hwm or 0.0)
            except (TypeError, ValueError):
                seed_hwm = 0.0
            hwm_anchor = max(effective_entry, seed_hwm)
            if sl_after_active and self._run_tp_tier_thresholds:
                sl_trigger_px = self._initial_sl_trigger(
                    "long", avg_cost, entry_atr_value,
                )
                sl_high_water_px = 0.0
            elif trailing_ratchet_active and self._run_trailing_stop_atr_mult:
                sl_trigger_px = _initial_trail_trigger(
                    "long", hwm_anchor, entry_atr_value,
                    self._run_trailing_stop_atr_mult,
                )
                sl_high_water_px = hwm_anchor
            else:
                sl_trigger_px = self._initial_sl_trigger(
                    "long", avg_cost, entry_atr_value,
                )
                if sl_trigger_px <= 0 and self._run_trailing_stop_atr_mult:
                    sl_trigger_px = _initial_trail_trigger(
                        "long", hwm_anchor, entry_atr_value,
                        self._run_trailing_stop_atr_mult,
                    )
                sl_high_water_px = hwm_anchor
            sl_tiers_processed = 0
            post_tp_trail_mult = None
            sl_pierce_armed = True

        profile_switcher = (
            _ProfileSwitcher(self._profile_alloc) if has_profile_alloc else None
        )
        active_profile = ""

        book_funding = "funding_accrual" in df.columns
        total_funding_pnl = 0.0

        has_entry_fraction = "_entry_fraction" in df.columns

        risk_mode = (self.risk_per_trade_pct or 0) > 0
        if risk_mode and has_entry_fraction:
            raise ValueError(
                "risk_per_trade_pct is mutually exclusive with a "
                "strategy-emitted entry_fraction column (#1268) — the live "
                "sizer has no entry_fraction input, so composing them would "
                "diverge from the live sizing formula"
            )
        risk_skipped_entries = 0

        if self.allow_scale_in and has_entry_fraction:
            raise ValueError(
                "allow_scale_in is mutually exclusive with a strategy-emitted "
                "entry_fraction column (#1276) — the live per-add sizing "
                "(the fresh-open notional) has no entry_fraction input, so "
                "composing them would diverge from the live add-sizing rule"
            )
        prev_close_arr = (
            df["close"].shift(1).to_numpy(dtype=float)
            if self.allow_scale_in else None
        )

        def _try_scale_in_add(i: int, side: str, fill_price: float) -> bool:
            nonlocal position, cash, avg_cost, initial_quantity
            nonlocal scale_in_adds_total, scale_in_added_notional_total
            if hurst_blocked:
                return False
            decision_price = float(prev_close_arr[i])
            if not (decision_price > 0):
                return False
            default_notional = scale.base_open_notional
            add_qty, ok, _reason = _scale_in_decision(
                self._scale_in_cfg, side, abs(position), avg_cost,
                entry_atr_value, scale.scale_in_count,
                scale.added_notional_usd, scale.last_add_price,
                1 if side == "long" else -1, decision_price, default_notional,
            )
            if not ok or add_qty <= 0:
                return False
            add_qty *= hurst_size_mult
            if add_qty <= 0:
                return False
            if side == "long":
                eff = fill_price * (1 + self.slippage_pct)
            else:
                eff = fill_price * (1 - self.slippage_pct)
            notional = add_qty * eff
            commission = notional * self.commission_pct
            if side == "long":
                cash -= notional + commission
                position += add_qty
            else:
                cash += notional - commission
                position -= add_qty
            if scale.risk_anchor_price <= 0:
                scale.risk_anchor_price = avg_cost
            old_qty = abs(position) - add_qty
            new_qty = old_qty + add_qty
            avg_cost = (old_qty * avg_cost + add_qty * eff) / new_qty
            initial_quantity += add_qty
            scale.scale_in_count += 1
            scale.last_add_price = eff
            scale.added_notional_usd += notional
            scale_in_adds_total += 1
            scale_in_added_notional_total += notional
            if current_trade is not None:
                current_trade.entry_price = avg_cost
                current_trade.shares += add_qty
                current_trade.scale_in_adds = scale.scale_in_count
            hold.entry_fee += commission
            return True

        for i, (idx, row) in enumerate(df.iterrows()):
            fill_price = row["open"] if has_open else row["close"]
            mark_price = row["close"]
            signal = row["signal"]
            entry_fraction = (
                float(row["_entry_fraction"]) if has_entry_fraction else 1.0
            )
            risk_entry_blocked = False
            if risk_mode:
                risk_fraction = self._risk_entry_fraction(
                    atr_series, idx, fill_price,
                )
                if risk_fraction is None:
                    risk_entry_blocked = True
                    entry_wanted = (
                        str(row.get("_open_action", "none")) in ("long", "short")
                        if uses_open_close
                        else int(signal) != 0
                    )
                    if position == 0 and entry_wanted:
                        risk_skipped_entries += 1
                        if not self._risk_skip_warned:
                            self._risk_skip_warned = True
                            print(
                                f"[#1268] risk_per_trade_pct: entry skipped at "
                                f"{idx} — stop distance unresolvable (no usable "
                                f"ATR at the signal bar); fail-closed, matching "
                                f"live. Further skips counted silently.",
                                file=sys.stderr,
                            )
                else:
                    entry_fraction = risk_fraction
            if profile_switcher is not None:
                active_profile = profile_switcher.step(
                    str(row.get("_profile_label", "") or ""), position == 0
                )
                signal = row["signal__" + active_profile]

            bar_regime = str(row.get("regime", "")) if self.regime_enabled else ""
            effective_direction = self.direction or ""
            effective_invert = self.invert_signal
            plain_short_for_bar = plain_short
            if self.regime_directional_policy is not None:
                effective_direction, effective_invert = self._effective_directional_entry(
                    bar_regime,
                    self._run_position_regime,
                    abs(position),
                )
                if not uses_open_close:
                    signal = _apply_direction_invert_value(
                        int(signal),
                        uses_open_close=False,
                        direction=effective_direction,
                        invert_signal=effective_invert,
                    )
                    plain_short_for_bar = effective_direction == "short"

            sl_after_just_applied = False

            if book_funding and position != 0:
                accrual = row.get("funding_accrual", 0.0)
                accrual = float(accrual) if accrual == accrual else 0.0
                if accrual != 0.0:
                    funding_cash = -position * mark_price * accrual
                    cash += funding_cash
                    total_funding_pnl += funding_cash

            equity = cash + position * mark_price
            equity_curve.append({"date": idx, "equity": equity})

            regime_blocked = (
                self.regime_enabled
                and bool(self.allowed_regimes)
                and not _regime_allows_entry(
                    self.allowed_regimes, bar_regime, self.regime_gate_on_failure
                )
            )

            hurst_size_mult = 1.0
            hurst_blocked = False
            if hurst_runner is not None:
                hurst_h = row.get("_hurst")
                hurst_blocked, hurst_size_mult = hurst_runner.step(
                    hurst_h, position == 0
                )
                if hurst_blocked:
                    regime_blocked = True
                if hurst_size_mult != 1.0:
                    entry_fraction *= hurst_size_mult

            if uses_open_close:
                col_close_fraction = float(row.get("_close_fraction", 0.0))
                if col_close_fraction >= pending_close_fraction:
                    close_fraction = col_close_fraction
                    close_reason = "column_close_fraction" if col_close_fraction > 0 else ""
                else:
                    close_fraction = pending_close_fraction
                    close_reason = pending_close_reason
                pending_close_fraction = 0.0
                pending_close_reason = ""
                if profile_switcher is not None:
                    open_action = row.get("_open_action__" + active_profile, "none")
                else:
                    open_action = row.get("_open_action", "none")
                raw_open_signal = (
                    _signal_from_open_action(open_action)
                    if self.regime_directional_policy is not None
                    else 0
                )

                if close_fraction > 0 and position != 0:
                    qty_to_close = abs(position) * min(close_fraction, 1.0)
                    if position > 0:
                        effective_price = fill_price * (1 - self.slippage_pct)
                        proceeds = qty_to_close * effective_price
                        commission = proceeds * self.commission_pct
                        cash += proceeds - commission
                        position -= qty_to_close
                    else:
                        effective_price = fill_price * (1 + self.slippage_pct)
                        cost = qty_to_close * effective_price
                        commission = cost * self.commission_pct
                        cash -= cost + commission
                        position += qty_to_close

                    if current_trade:
                        closed = Trade(current_trade.entry_date, current_trade.entry_price, current_trade.side)
                        closed.shares = qty_to_close
                        closed.close(idx, effective_price)
                        qty_frac = (qty_to_close / initial_quantity) if initial_quantity > 0 else 1.0
                        _stamp_hold(closed, hold, entry_atr=entry_atr_value,
                                    exit_fee=commission,
                                    reason=close_reason or "close_strategy",
                                    qty_frac=qty_frac,
                                    true_up_entry_fee=(
                                        scale.scale_in_count > 0
                                        and abs(position) <= 1e-12
                                    ))
                        closed.scale_in_adds = scale.scale_in_count
                        trades.append(closed)
                        current_trade.shares -= qty_to_close
                        if current_trade.shares <= 1e-12:
                            current_trade = None

                    if abs(position) <= 1e-12:
                        position = 0.0
                        avg_cost = 0.0
                        initial_quantity = 0.0
                        entry_atr_value = 0.0
                        scale.reset()
                        sl_trigger_px = 0.0
                        sl_tiers_processed = 0
                        post_tp_trail_mult = None
                        sl_high_water_px = 0.0
                        sl_after_just_applied = False
                        self._active_sl_after_rules = self._sl_after_rules_static
                        self._run_tp_tier_thresholds = list(
                            self._tp_tier_thresholds_static,
                        )
                        self._run_stop_loss_atr_mult = None
                        self._run_trailing_stop_atr_mult = None
                        self._run_position_regime = ""
                    elif sl_after_active and self._run_tp_tier_thresholds:
                        side_now = "long" if position > 0 else "short"
                        prev_trigger = sl_trigger_px
                        prev_post_tp_trail = post_tp_trail_mult
                        sl_trigger_px, sl_tiers_processed, post_tp_trail_mult, \
                            sl_high_water_px = self._maybe_apply_sl_after(
                                side=side_now,
                                avg_cost=scale.geom_cost(avg_cost),
                                entry_atr=entry_atr_value,
                                position_qty=abs(position),
                                initial_qty=initial_quantity,
                                mark_price=mark_price,
                                fill_price=fill_price,
                                sl_trigger_px=sl_trigger_px,
                                sl_tiers_processed=sl_tiers_processed,
                                post_tp_trail_mult=post_tp_trail_mult,
                                sl_high_water_px=sl_high_water_px,
                            )
                        if (
                            sl_trigger_px != prev_trigger
                            or post_tp_trail_mult != prev_post_tp_trail
                        ):
                            sl_after_just_applied = True

                if self.regime_directional_policy is not None:
                    entry_direction, entry_invert = self._effective_directional_entry(
                        bar_regime,
                        self._run_position_regime,
                        abs(position),
                    )
                    open_action = _open_action_from_signal(
                        _apply_direction_invert_value(
                            raw_open_signal,
                            uses_open_close=True,
                            direction=entry_direction,
                            invert_signal=entry_invert,
                        )
                    )

                if open_action == "long" and position == 0 and cash > 0 and not regime_blocked and not risk_entry_blocked:
                    effective_price = fill_price * (1 + self.slippage_pct)
                    invest = cash * entry_fraction
                    commission = invest * self.commission_pct
                    available = invest - commission
                    shares = available / effective_price
                    position = shares
                    cash -= invest

                    current_trade = Trade(idx, effective_price, "long")
                    current_trade.shares = shares
                    avg_cost = effective_price
                    initial_quantity = shares
                    entry_atr_value = self._stamp_entry_atr(atr_series, idx, effective_price)
                    hold.open(effective_price, "long", commission)
                    scale.reset()
                    scale.base_open_notional = _ungated_leg_notional(
                        shares * effective_price, hurst_size_mult,
                    )
                    stamp_open_from_label(_entry_stamp(row))
                    if sl_after_active and self._run_tp_tier_thresholds:
                        sl_trigger_px = self._initial_sl_trigger(
                            "long", avg_cost, entry_atr_value,
                        )
                        sl_pierce_armed = sl_trigger_px > 0
                        sl_tiers_processed = 0
                        post_tp_trail_mult = None
                        sl_high_water_px = 0.0
                    elif trailing_ratchet_active and self._run_trailing_stop_atr_mult:
                        sl_trigger_px = _initial_trail_trigger(
                            "long", mark_price, entry_atr_value,
                            self._run_trailing_stop_atr_mult,
                        )
                        sl_pierce_armed = False
                        sl_tiers_processed = 0
                        post_tp_trail_mult = None
                        sl_high_water_px = mark_price
                    else:
                        sl_trigger_px = self._initial_sl_trigger(
                            "long", avg_cost, entry_atr_value,
                        )
                        sl_pierce_armed = sl_trigger_px > 0
                        if sl_trigger_px <= 0 and self._run_trailing_stop_atr_mult:
                            sl_trigger_px = _initial_trail_trigger(
                                "long", mark_price, entry_atr_value,
                                self._run_trailing_stop_atr_mult,
                            )
                            sl_pierce_armed = False
                        sl_tiers_processed = 0
                        post_tp_trail_mult = None
                        sl_high_water_px = mark_price
                elif open_action == "short" and position == 0 and cash > 0 and not regime_blocked and not risk_entry_blocked:
                    effective_price = fill_price * (1 - self.slippage_pct)
                    margin = cash * entry_fraction
                    commission = margin * self.commission_pct
                    notional = margin - commission
                    shares = notional / effective_price
                    cash += 2 * notional - margin
                    position = -shares

                    current_trade = Trade(idx, effective_price, "short")
                    current_trade.shares = shares
                    avg_cost = effective_price
                    initial_quantity = shares
                    entry_atr_value = self._stamp_entry_atr(atr_series, idx, effective_price)
                    hold.open(effective_price, "short", commission)
                    scale.reset()
                    scale.base_open_notional = _ungated_leg_notional(
                        shares * effective_price, hurst_size_mult,
                    )
                    stamp_open_from_label(_entry_stamp(row))
                    if sl_after_active and self._run_tp_tier_thresholds:
                        sl_trigger_px = self._initial_sl_trigger(
                            "short", avg_cost, entry_atr_value,
                        )
                        sl_pierce_armed = sl_trigger_px > 0
                        sl_tiers_processed = 0
                        post_tp_trail_mult = None
                        sl_high_water_px = 0.0
                    elif trailing_ratchet_active and self._run_trailing_stop_atr_mult:
                        sl_trigger_px = _initial_trail_trigger(
                            "short", mark_price, entry_atr_value,
                            self._run_trailing_stop_atr_mult,
                        )
                        sl_pierce_armed = False
                        sl_tiers_processed = 0
                        post_tp_trail_mult = None
                        sl_high_water_px = mark_price
                    else:
                        sl_trigger_px = self._initial_sl_trigger(
                            "short", avg_cost, entry_atr_value,
                        )
                        sl_pierce_armed = sl_trigger_px > 0
                        if sl_trigger_px <= 0 and self._run_trailing_stop_atr_mult:
                            sl_trigger_px = _initial_trail_trigger(
                                "short", mark_price, entry_atr_value,
                                self._run_trailing_stop_atr_mult,
                            )
                            sl_pierce_armed = False
                        sl_tiers_processed = 0
                        post_tp_trail_mult = None
                        sl_high_water_px = mark_price
                elif (
                    self.allow_scale_in
                    and open_action == "long"
                    and position > 0
                ):
                    _try_scale_in_add(i, "long", fill_price)
                elif (
                    self.allow_scale_in
                    and open_action == "short"
                    and position < 0
                ):
                    _try_scale_in_add(i, "short", fill_price)

                if position != 0:
                    hold.step(
                        float(row.get("high", mark_price) or mark_price),
                        float(row.get("low", mark_price) or mark_price),
                    )

                if (
                    walk_mode
                    and position != 0
                    and sl_pierce_armed
                    and sl_trigger_px > 0
                    and not sl_after_just_applied
                    and avg_cost > 0
                ):
                    side_now = "long" if position > 0 else "short"
                    raw_fill = self._intrabar_sl_fill(
                        side_now,
                        float(row["open"]) if has_open else mark_price,
                        float(row.get("high", mark_price) or mark_price),
                        float(row.get("low", mark_price) or mark_price),
                        sl_trigger_px,
                    )
                    if raw_fill is not None:
                        qty_to_close = abs(position)
                        if position > 0:
                            effective_price = raw_fill * (1 - self.slippage_pct)
                            proceeds = qty_to_close * effective_price
                            commission = proceeds * self.commission_pct
                            cash += proceeds - commission
                        else:
                            effective_price = raw_fill * (1 + self.slippage_pct)
                            cost = qty_to_close * effective_price
                            commission = cost * self.commission_pct
                            cash -= cost + commission
                        position = 0.0
                        if current_trade:
                            closed = Trade(
                                current_trade.entry_date,
                                current_trade.entry_price,
                                current_trade.side,
                            )
                            closed.shares = qty_to_close
                            closed.close(idx, effective_price)
                            qty_frac = (
                                qty_to_close / initial_quantity
                                if initial_quantity > 0 else 1.0
                            )
                            _stamp_hold(closed, hold,
                                        entry_atr=entry_atr_value,
                                        exit_fee=commission, reason="sl",
                                        qty_frac=qty_frac,
                                        true_up_entry_fee=(
                                            scale.scale_in_count > 0
                                        ))
                            closed.scale_in_adds = scale.scale_in_count
                            trades.append(closed)
                            current_trade = None
                        avg_cost = 0.0
                        initial_quantity = 0.0
                        entry_atr_value = 0.0
                        scale.reset()
                        sl_trigger_px = 0.0
                        sl_tiers_processed = 0
                        post_tp_trail_mult = None
                        sl_high_water_px = 0.0
                        sl_pierce_armed = False
                        self._active_sl_after_rules = self._sl_after_rules_static
                        self._run_tp_tier_thresholds = list(
                            self._tp_tier_thresholds_static,
                        )
                        self._run_stop_loss_atr_mult = None
                        self._run_trailing_stop_atr_mult = None
                        self._run_position_regime = ""

                if self.close_strategies and position != 0 and avg_cost > 0:
                    pending_close_fraction, pending_close_reason = self._evaluate_close_strategies(
                        position, scale.geom_cost(avg_cost), initial_quantity,
                        entry_atr_value,
                        mark_price, atr_series, idx,
                        position_regime=self._run_position_regime,
                        market_regime=_bar_close_regime(row),
                        bars_held=hold.bars,
                        zscore_series=zscore_series,
                        avwap_series=avwap_series,
                    )
                    if (
                        trailing_ratchet_active
                        and self._ratchet_mod
                        and self._ratchet_tiers_run
                        and position != 0
                        and entry_atr_value > 0
                    ):
                        side_now = "long" if position > 0 else "short"
                        base_trail = self._run_trailing_stop_atr_mult or 0.0
                        sl_tiers_processed, post_tp_trail_mult = (
                            self._ratchet_mod.maybe_apply_mark_ratchet(
                                self._ratchet_tiers_run,
                                watermark=sl_tiers_processed,
                                mark_price=mark_price,
                                avg_cost=scale.geom_cost(avg_cost),
                                entry_atr=entry_atr_value,
                                side=side_now,
                                post_tp_trail_mult=post_tp_trail_mult,
                                trailing_stop_atr_mult=base_trail,
                            )
                        )

                scalar_stop_active = (
                    (self._run_stop_loss_atr_mult or 0) > 0
                    or (self._run_trailing_stop_atr_mult or 0) > 0
                    or (self.stop_loss_pct or 0) > 0
                )
                if (
                    (sl_after_active or trailing_ratchet_active
                     or scalar_stop_active)
                    and not sl_after_just_applied
                    and position != 0
                    and avg_cost > 0
                ):
                    side_now = "long" if position > 0 else "short"
                    trail_mult = post_tp_trail_mult
                    if trail_mult is None or trail_mult <= 0:
                        trail_mult = self._run_trailing_stop_atr_mult
                    if (
                        trail_mult is not None
                        and trail_mult > 0
                        and entry_atr_value > 0
                    ):
                        sl_trigger_px, sl_high_water_px = self._walk_trail(
                            side=side_now,
                            mark_price=mark_price,
                            entry_atr=entry_atr_value,
                            trail_mult=trail_mult,
                            sl_trigger_px=sl_trigger_px,
                            sl_high_water_px=sl_high_water_px,
                        )
                    if not walk_mode and sl_trigger_px > 0 and self._sl_hit(
                        side_now, mark_price, sl_trigger_px,
                    ):
                        pending_close_fraction = 1.0
                        pending_close_reason = "sl"
                if position != 0:
                    sl_pierce_armed = True
                continue

            if pending_signal_sl_close and position > 0:
                effective_price = fill_price * (1 - self.slippage_pct)
                proceeds = position * effective_price
                commission = proceeds * self.commission_pct
                cash += proceeds - commission
                position = 0.0
                if current_trade:
                    current_trade.close(idx, effective_price)
                    _stamp_hold(current_trade, hold, entry_atr=entry_atr_value,
                                exit_fee=commission, reason="signal_sl")
                    current_trade.scale_in_adds = scale.scale_in_count
                    trades.append(current_trade)
                    current_trade = None
                pending_signal_sl_close = False
                sl_trigger_px = 0.0
                avg_cost = 0.0
                entry_atr_value = 0.0
                sl_high_water_px = 0.0
                scale.reset()
                self._run_position_regime = ""
                continue

            if pending_signal_sl_close and position < 0:
                effective_price = fill_price * (1 + self.slippage_pct)
                cost = abs(position) * effective_price
                commission = cost * self.commission_pct
                cash -= cost + commission
                position = 0.0
                if current_trade:
                    current_trade.close(idx, effective_price)
                    _stamp_hold(current_trade, hold, entry_atr=entry_atr_value,
                                exit_fee=commission, reason="signal_sl")
                    current_trade.scale_in_adds = scale.scale_in_count
                    trades.append(current_trade)
                    current_trade = None
                pending_signal_sl_close = False
                sl_trigger_px = 0.0
                avg_cost = 0.0
                entry_atr_value = 0.0
                sl_high_water_px = 0.0
                scale.reset()
                self._run_position_regime = ""
                continue

            if plain_short_for_bar and signal == -1 and position == 0 and cash > 0 and not regime_blocked and not risk_entry_blocked:
                effective_price = fill_price * (1 - self.slippage_pct)
                margin = cash * entry_fraction
                commission = margin * self.commission_pct
                notional = margin - commission
                shares = notional / effective_price
                cash += 2 * notional - margin
                position = -shares

                current_trade = Trade(idx, effective_price, "short")
                current_trade.shares = shares
                scale.reset()
                scale.base_open_notional = _ungated_leg_notional(
                    shares * effective_price, hurst_size_mult,
                )

                avg_cost = effective_price
                entry_atr_value = self._stamp_entry_atr(atr_series, idx, effective_price)
                hold.open(effective_price, "short", commission)
                stamp_open_from_label(_entry_stamp(row))
                sl_trigger_px = 0.0
                sl_high_water_px = mark_price
                sl_pierce_armed = False
                if (
                    self.stop_loss_atr_mult is not None
                    and self.stop_loss_atr_mult > 0
                    and entry_atr_value > 0
                ):
                    sl_trigger_px = avg_cost + self.stop_loss_atr_mult * entry_atr_value
                    sl_pierce_armed = True
                elif (
                    self.trailing_stop_atr_mult is not None
                    and self.trailing_stop_atr_mult > 0
                    and entry_atr_value > 0
                ):
                    sl_trigger_px = mark_price + self.trailing_stop_atr_mult * entry_atr_value
                elif self.stop_loss_pct is not None and self.stop_loss_pct > 0:
                    sl_trigger_px = avg_cost * (1 + self.stop_loss_pct)
                    sl_pierce_armed = True

            elif plain_short_for_bar and signal == 1 and position < 0:
                effective_price = fill_price * (1 + self.slippage_pct)
                cost = abs(position) * effective_price
                commission = cost * self.commission_pct
                cash -= cost + commission
                position = 0.0

                if current_trade:
                    current_trade.close(idx, effective_price)
                    _stamp_hold(current_trade, hold, entry_atr=entry_atr_value,
                                exit_fee=commission, reason="signal")
                    current_trade.scale_in_adds = scale.scale_in_count
                    trades.append(current_trade)
                    current_trade = None
                sl_trigger_px = 0.0
                avg_cost = 0.0
                entry_atr_value = 0.0
                sl_high_water_px = 0.0
                scale.reset()
                self._run_position_regime = ""

            elif not plain_short_for_bar and signal == 1 and position == 0 and cash > 0 and not regime_blocked and not risk_entry_blocked:
                effective_price = fill_price * (1 + self.slippage_pct)
                invest = cash * entry_fraction
                commission = invest * self.commission_pct
                available = invest - commission
                shares = available / effective_price
                position = shares
                cash -= invest

                current_trade = Trade(idx, effective_price, "long")
                current_trade.shares = shares
                scale.reset()
                scale.base_open_notional = _ungated_leg_notional(
                    shares * effective_price, hurst_size_mult,
                )

                avg_cost = effective_price
                entry_atr_value = self._stamp_entry_atr(atr_series, idx, effective_price)
                hold.open(effective_price, "long", commission)
                stamp_open_from_label(_entry_stamp(row))
                sl_trigger_px = 0.0
                sl_high_water_px = mark_price
                sl_pierce_armed = False
                if (
                    self.stop_loss_atr_mult is not None
                    and self.stop_loss_atr_mult > 0
                    and entry_atr_value > 0
                ):
                    sl_trigger_px = avg_cost - self.stop_loss_atr_mult * entry_atr_value
                    sl_pierce_armed = True
                elif (
                    self.trailing_stop_atr_mult is not None
                    and self.trailing_stop_atr_mult > 0
                    and entry_atr_value > 0
                ):
                    sl_trigger_px = mark_price - self.trailing_stop_atr_mult * entry_atr_value
                elif self.stop_loss_pct is not None and self.stop_loss_pct > 0:
                    sl_trigger_px = avg_cost * (1 - self.stop_loss_pct)
                    sl_pierce_armed = True

            elif signal == -1 and position > 0:
                effective_price = fill_price * (1 - self.slippage_pct)
                proceeds = position * effective_price
                commission = proceeds * self.commission_pct
                cash += proceeds - commission
                position = 0.0

                if current_trade:
                    current_trade.close(idx, effective_price)
                    _stamp_hold(current_trade, hold, entry_atr=entry_atr_value,
                                exit_fee=commission, reason="signal")
                    current_trade.scale_in_adds = scale.scale_in_count
                    trades.append(current_trade)
                    current_trade = None
                sl_trigger_px = 0.0
                avg_cost = 0.0
                entry_atr_value = 0.0
                sl_high_water_px = 0.0
                scale.reset()
                self._run_position_regime = ""

            elif (
                self.allow_scale_in
                and not plain_short_for_bar
                and signal == 1
                and position > 0
            ):
                _try_scale_in_add(i, "long", fill_price)
            elif (
                self.allow_scale_in
                and plain_short_for_bar
                and signal == -1
                and position < 0
            ):
                _try_scale_in_add(i, "short", fill_price)

            if position != 0:
                hold.step(
                    float(row.get("high", mark_price) or mark_price),
                    float(row.get("low", mark_price) or mark_price),
                )

            if (
                walk_mode
                and position != 0
                and sl_pierce_armed
                and sl_trigger_px > 0
            ):
                side_now = "long" if position > 0 else "short"
                raw_fill = self._intrabar_sl_fill(
                    side_now,
                    float(row["open"]) if has_open else mark_price,
                    float(row.get("high", mark_price) or mark_price),
                    float(row.get("low", mark_price) or mark_price),
                    sl_trigger_px,
                )
                if raw_fill is not None:
                    if position > 0:
                        effective_price = raw_fill * (1 - self.slippage_pct)
                        proceeds = position * effective_price
                        commission = proceeds * self.commission_pct
                        cash += proceeds - commission
                    else:
                        effective_price = raw_fill * (1 + self.slippage_pct)
                        cost = abs(position) * effective_price
                        commission = cost * self.commission_pct
                        cash -= cost + commission
                    position = 0.0
                    if current_trade:
                        current_trade.close(idx, effective_price)
                        _stamp_hold(current_trade, hold,
                                    entry_atr=entry_atr_value,
                                    exit_fee=commission, reason="signal_sl")
                        current_trade.scale_in_adds = scale.scale_in_count
                        trades.append(current_trade)
                        current_trade = None
                    sl_trigger_px = 0.0
                    avg_cost = 0.0
                    entry_atr_value = 0.0
                    sl_high_water_px = 0.0
                    sl_pierce_armed = False
                    scale.reset()
                    self._run_position_regime = ""

            if position > 0 and sl_trigger_px > 0:
                if (
                    self.trailing_stop_atr_mult is not None
                    and self.trailing_stop_atr_mult > 0
                    and entry_atr_value > 0
                ):
                    if mark_price > sl_high_water_px:
                        sl_high_water_px = mark_price
                    candidate = sl_high_water_px - self.trailing_stop_atr_mult * entry_atr_value
                    if candidate > sl_trigger_px:
                        sl_trigger_px = candidate
                if not walk_mode and self._sl_hit("long", mark_price, sl_trigger_px):
                    pending_signal_sl_close = True
            elif position < 0 and sl_trigger_px > 0:
                if (
                    self.trailing_stop_atr_mult is not None
                    and self.trailing_stop_atr_mult > 0
                    and entry_atr_value > 0
                ):
                    if mark_price < sl_high_water_px:
                        sl_high_water_px = mark_price
                    candidate = sl_high_water_px + self.trailing_stop_atr_mult * entry_atr_value
                    if candidate < sl_trigger_px:
                        sl_trigger_px = candidate
                if not walk_mode and self._sl_hit("short", mark_price, sl_trigger_px):
                    pending_signal_sl_close = True

            if position != 0:
                sl_pierce_armed = True

        if position != 0:
            if position > 0:
                final_price = df["close"].iloc[-1] * (1 - self.slippage_pct)
                proceeds = position * final_price
                commission = proceeds * self.commission_pct
                cash += proceeds - commission
            else:
                final_price = df["close"].iloc[-1] * (1 + self.slippage_pct)
                cost = abs(position) * final_price
                commission = cost * self.commission_pct
                cash -= cost + commission
            position = 0.0

            if current_trade:
                current_trade.close(df.index[-1], final_price)
                eod_qty_frac = (
                    current_trade.shares / initial_quantity
                    if initial_quantity > 0 else 1.0
                )
                _stamp_hold(current_trade, hold, entry_atr=entry_atr_value,
                            exit_fee=commission, reason="end_of_data",
                            qty_frac=eod_qty_frac,
                            true_up_entry_fee=scale.scale_in_count > 0)
                current_trade.scale_in_adds = scale.scale_in_count
                trades.append(current_trade)

        final_equity = cash
        equity_df = pd.DataFrame(equity_curve).set_index("date")

        metrics = self._calculate_metrics(equity_df, trades, df, timeframe)
        open_ref = dict(self.open_strategy) if self.open_strategy else {}
        if not open_ref.get("name") and strategy_name:
            open_ref["name"] = strategy_name
        if "params" not in open_ref and params:
            open_ref["params"] = dict(params)
        metrics.update({
            "strategy_name": open_ref.get("name") or strategy_name,
            "symbol": symbol,
            "timeframe": timeframe,
            "start_date": str(df.index[0]),
            "end_date": str(df.index[-1]),
            "initial_capital": self.initial_capital,
            "final_capital": round(final_equity, 2),
            "total_funding_pnl": round(total_funding_pnl, 4),
            "params": open_ref.get("params") or params or {},
            "open_strategy": open_ref,
            "close_strategies": [dict(r) for r in self._close_refs],
            "trades": [t.to_dict() for t in trades],
        })
        if risk_mode:
            metrics["risk_per_trade_pct"] = self.risk_per_trade_pct
            metrics["risk_sizing_skipped_entries"] = risk_skipped_entries
        if self.allow_scale_in:
            metrics["scale_in_adds"] = scale_in_adds_total
            metrics["scale_in_added_notional_usd"] = round(
                scale_in_added_notional_total, 6,
            )

        if save:
            store_backtest_result(metrics)

        return metrics

    def _risk_entry_fraction(self, atr_series: Optional[pd.Series], idx,
                             price: float) -> Optional[float]:
        pct = float(self.risk_per_trade_pct or 0)
        if pct <= 0 or price <= 0:
            return None
        dist = None
        atr_mult = 0.0
        if (self.trailing_stop_atr_mult or 0) > 0:
            atr_mult = float(self.trailing_stop_atr_mult)
        elif (self.stop_loss_atr_mult or 0) > 0:
            atr_mult = float(self.stop_loss_atr_mult)
        if atr_mult > 0:
            atr = self._stamp_entry_atr(atr_series, idx, price)
            if atr <= 0:
                return None
            dist = atr_mult * atr
        elif (self.trailing_stop_pct or 0) > 0:
            dist = price * float(self.trailing_stop_pct)
        elif (self.stop_loss_pct or 0) > 0:
            dist = price * float(self.stop_loss_pct)
        if dist is None or dist <= 0:
            return None
        fraction = (pct / 100.0) * price / dist
        if fraction > 1.0:
            if not self._risk_cap_warned:
                self._risk_cap_warned = True
                print(
                    f"[#1268] risk_per_trade_pct: risk-derived notional "
                    f"exceeds available cash at {idx} (fraction "
                    f"{fraction:.2f} capped at 1.0) — the backtester models "
                    f"no leverage, so live would size up to cash × "
                    f"exchange_leverage here. Further caps applied silently.",
                    file=sys.stderr,
                )
            fraction = 1.0
        return fraction

    def _stamp_entry_atr(self, atr_series: Optional[pd.Series], idx,
                         entry_price: float) -> float:
        if atr_series is None or entry_price <= 0:
            return 0.0
        try:
            pos = int(atr_series.index.get_loc(idx))
        except (KeyError, TypeError, ValueError):
            return 0.0
        if pos < 1:
            return 0.0
        try:
            value = float(atr_series.iloc[pos - 1])
        except (TypeError, ValueError):
            return 0.0
        if not (value > 0):
            return 0.0
        if value > 0.5 * entry_price:
            return 0.0
        return value

    def _close_names_include_avwap_stop(self) -> bool:
        from strategy_composition import close_names_include_avwap_stop
        return close_names_include_avwap_stop(self.close_strategies)

    def _evaluate_close_strategies(self, position: float, avg_cost: float,
                                   initial_quantity: float,
                                   entry_atr_value: float,
                                   mark_price: float,
                                   atr_series: Optional[pd.Series],
                                   idx,
                                   *,
                                   position_regime: str = "",
                                   market_regime: str = "",
                                   bars_held: int = 0,
                                   zscore_series: Optional[pd.Series] = None,
                                   avwap_series: Optional[pd.Series] = None
                                   ) -> Tuple[float, str]:
        evaluate, _list_strategies = _load_close_registry()
        side = "long" if position > 0 else "short"
        position_dict = {
            "side": side,
            "avg_cost": float(avg_cost),
            "current_quantity": float(abs(position)),
            "initial_quantity": float(initial_quantity or abs(position)),
            "entry_atr": float(entry_atr_value),
            "regime": str(position_regime or ""),
            "bars_held": int(bars_held),
        }
        market_dict = {
            "mark_price": float(mark_price),
            "regime": str(market_regime or ""),
        }
        if atr_series is not None:
            try:
                live_atr = float(atr_series.loc[idx])
            except (KeyError, TypeError, ValueError):
                live_atr = 0.0
            if live_atr > 0:
                market_dict["atr"] = live_atr

        if zscore_series is not None:
            try:
                z = float(zscore_series.loc[idx])
            except (KeyError, TypeError, ValueError):
                z = float("nan")
            if z == z:
                market_dict["zscore"] = z

        if avwap_series is not None:
            try:
                avwap_value = float(avwap_series.loc[idx])
            except (KeyError, TypeError, ValueError):
                avwap_value = float("nan")
            if avwap_value == avwap_value and avwap_value > 0:
                market_dict["avwap"] = avwap_value

        best = 0.0
        best_reason = ""
        for name in self.close_strategies:
            params = self.close_params.get(name)
            result = evaluate(name, position_dict, market_dict, params)
            fraction = float(result.get("close_fraction", 0.0) or 0.0)
            if fraction > best:
                best = fraction
                best_reason = str(result.get("reason") or name)
                if best >= 1.0:
                    return 1.0, best_reason
        return min(max(best, 0.0), 1.0), best_reason

    def _initial_sl_trigger(self, side: str, avg_cost: float,
                            entry_atr: float) -> float:
        if avg_cost <= 0 or side not in ("long", "short"):
            return 0.0
        if (
            self._run_stop_loss_atr_mult is not None
            and self._run_stop_loss_atr_mult > 0
            and entry_atr > 0
        ):
            distance = self._run_stop_loss_atr_mult * entry_atr
            return avg_cost - distance if side == "long" else avg_cost + distance
        if self.stop_loss_pct is not None and self.stop_loss_pct > 0:
            return (
                avg_cost * (1 - self.stop_loss_pct)
                if side == "long"
                else avg_cost * (1 + self.stop_loss_pct)
            )
        return 0.0

    @staticmethod
    def _intrabar_sl_fill(side: str, open_px: float, high_px: float,
                          low_px: float, trigger_px: float) -> Optional[float]:
        if trigger_px <= 0:
            return None
        if side == "long":
            if open_px > 0 and open_px <= trigger_px:
                return open_px
            if low_px > 0 and low_px <= trigger_px:
                return trigger_px
        elif side == "short":
            if open_px >= trigger_px:
                return open_px
            if high_px >= trigger_px:
                return trigger_px
        return None

    @staticmethod
    def _sl_hit(side: str, mark_price: float, trigger_px: float) -> bool:
        if trigger_px <= 0 or mark_price <= 0:
            return False
        if side == "long":
            return mark_price <= trigger_px
        if side == "short":
            return mark_price >= trigger_px
        return False

    @staticmethod
    def _walk_trail(side: str, mark_price: float, entry_atr: float,
                    trail_mult: float, sl_trigger_px: float,
                    sl_high_water_px: float) -> Tuple[float, float]:
        if mark_price <= 0 or entry_atr <= 0 or trail_mult <= 0:
            return sl_trigger_px, sl_high_water_px
        new_trigger = sl_trigger_px
        new_hwm = sl_high_water_px
        if side == "long":
            if mark_price > new_hwm:
                new_hwm = mark_price
            candidate = new_hwm - trail_mult * entry_atr
            if candidate > new_trigger:
                new_trigger = candidate
        elif side == "short":
            if new_hwm <= 0 or mark_price < new_hwm:
                new_hwm = mark_price
            candidate = new_hwm + trail_mult * entry_atr
            if new_trigger <= 0 or candidate < new_trigger:
                new_trigger = candidate
        return new_trigger, new_hwm

    def _maybe_apply_sl_after(
        self, *, side: str, avg_cost: float, entry_atr: float,
        position_qty: float, initial_qty: float, mark_price: float,
        fill_price: float, sl_trigger_px: float, sl_tiers_processed: int,
        post_tp_trail_mult: Optional[float], sl_high_water_px: float,
    ) -> Tuple[float, int, Optional[float], float]:
        if initial_qty <= 0 or position_qty <= 0:
            return sl_trigger_px, sl_tiers_processed, post_tp_trail_mult, sl_high_water_px
        closed_ratio = 1.0 - (position_qty / initial_qty)
        if closed_ratio <= 0:
            return sl_trigger_px, sl_tiers_processed, post_tp_trail_mult, sl_high_water_px
        highest = self._sl_mod.find_highest_cleared_tier(
            self._run_tp_tier_thresholds, closed_ratio, sl_tiers_processed,
        )
        if highest < 0:
            return sl_trigger_px, sl_tiers_processed, post_tp_trail_mult, sl_high_water_px
        raw_rule = self._active_sl_after_rules.for_tier(highest)
        if raw_rule.is_empty():
            return sl_trigger_px, highest + 1, post_tp_trail_mult, sl_high_water_px
        if sl_trigger_px <= 0:
            return sl_trigger_px, sl_tiers_processed, post_tp_trail_mult, sl_high_water_px
        tier_multiple = self._active_sl_after_rules.tier_multiple(highest)
        rule = raw_rule.resolve_for_regime(self._run_position_regime, tier_multiple)
        if rule is None:
            return sl_trigger_px, sl_tiers_processed, post_tp_trail_mult, sl_high_water_px
        seed_mark = fill_price if fill_price > 0 else mark_price
        new_trigger, _mode, ok = self._sl_mod.compute_post_tp_stop_loss_trigger(
            rule, side, avg_cost, entry_atr, seed_mark,
        )
        if not ok:
            return sl_trigger_px, sl_tiers_processed, post_tp_trail_mult, sl_high_water_px
        new_post_tp_trail = post_tp_trail_mult
        new_hwm = sl_high_water_px
        if rule.kind == "trail_from_here":
            new_post_tp_trail = rule.trail_atr_mult
            new_hwm = seed_mark
        return new_trigger, highest + 1, new_post_tp_trail, new_hwm

    def _calculate_metrics(self, equity_df: pd.DataFrame, trades: list,
                           df: pd.DataFrame, timeframe: str = "1d") -> dict:
        equity = equity_df["equity"]
        ann_factor = math.sqrt(periods_per_year(timeframe))

        liquidated = bool((equity <= 0).any())
        if liquidated:
            bust_pos = int(np.argmax(equity.values <= 0))
            equity = equity.copy()
            equity.iloc[bust_pos:] = 0.0

        total_return = (equity.iloc[-1] - self.initial_capital) / self.initial_capital

        days = (df.index[-1] - df.index[0]).days
        years = max(days / 365.25, 0.01)
        annual_return = (1 + total_return) ** (1 / years) - 1 if total_return > -1 else -1

        daily_returns = equity.pct_change().dropna()

        if len(daily_returns) > 1 and daily_returns.std() > 0:
            sharpe = (daily_returns.mean() / daily_returns.std()) * ann_factor
        else:
            sharpe = 0.0

        if len(daily_returns) > 0:
            neg = daily_returns.clip(upper=0.0)
            downside_dev = float(np.sqrt((neg**2).mean()))
        else:
            downside_dev = 0.0
        if downside_dev > 0:
            sortino = (daily_returns.mean() / downside_dev) * ann_factor
        else:
            sortino = None

        cummax_raw = equity.cummax()
        cummax = cummax_raw.where(cummax_raw >= self.initial_capital, self.initial_capital)
        drawdown = (equity - cummax) / cummax
        max_drawdown = drawdown.min()

        total_trades = len(trades)
        if total_trades > 0:
            winning = [t for t in trades if t.pnl > 0]
            losing = [t for t in trades if t.pnl <= 0]
            win_rate = len(winning) / total_trades

            gross_profit = sum(t.pnl for t in winning) if winning else 0
            gross_loss = abs(sum(t.pnl for t in losing)) if losing else 0
            profit_factor = gross_profit / gross_loss if gross_loss > 0 else None

            def _net_pnl_pct(t):
                notional = t.shares * t.entry_price
                return (t.pnl / notional) if notional > 0 else 0.0
            avg_win = np.mean([_net_pnl_pct(t) for t in winning]) if winning else 0
            avg_loss = np.mean([_net_pnl_pct(t) for t in losing]) if losing else 0
        else:
            win_rate = 0
            profit_factor = 0
            avg_win = 0
            avg_loss = 0

        volatility = daily_returns.std() * ann_factor if len(daily_returns) > 1 else 0

        calmar = annual_return / abs(max_drawdown) if max_drawdown != 0 else 0

        if liquidated:
            sharpe = sortino = -LIQUIDATED_METRIC_FLOOR
            volatility = LIQUIDATED_METRIC_FLOOR

        return {
            "total_return_pct": round(total_return * 100, 2),
            "annual_return_pct": round(annual_return * 100, 2),
            "sharpe_ratio": round(sharpe, 3),
            "sortino_ratio": round(sortino, 3) if sortino is not None else None,
            "max_drawdown_pct": round(max_drawdown * 100, 2),
            "calmar_ratio": round(calmar, 3),
            "volatility_pct": round(volatility * 100, 2),
            "win_rate": round(win_rate * 100, 2),
            "profit_factor": round(profit_factor, 3) if profit_factor is not None else None,
            "total_trades": total_trades,
            "avg_win_pct": round(avg_win * 100, 2),
            "avg_loss_pct": round(avg_loss * 100, 2),
            "liquidated": liquidated,
        }


def _fmt_opt(value, spec: str = ".3f", none_text: str = "n/a") -> str:
    if value is None:
        return none_text
    return format(value, spec)


def format_results(results: dict) -> str:
    lines = [
        f"\n{'='*60}",
        f"  BACKTEST RESULTS: {results['strategy_name']}",
        f"{'='*60}",
        f"  Symbol:          {results['symbol']}",
        f"  Timeframe:       {results['timeframe']}",
        f"  Period:          {results['start_date'][:10]} → {results['end_date'][:10]}",
        f"  Initial Capital: ${results['initial_capital']:,.2f}",
        f"  Final Capital:   ${results['final_capital']:,.2f}",
    ]
    if results.get("liquidated"):
        lines.append(
            "  *** LIQUIDATED: equity hit 0 — metrics floored at the bust bar ***"
        )
    lines += [
        f"{'─'*60}",
        f"  RETURNS",
        f"    Total Return:    {results['total_return_pct']:+.2f}%",
        f"    Annual Return:   {results['annual_return_pct']:+.2f}%",
        f"    Volatility:      {results.get('volatility_pct', 0):.2f}%",
        f"{'─'*60}",
        f"  RISK METRICS",
        f"    Sharpe Ratio:    {results['sharpe_ratio']:.3f}",
        f"    Sortino Ratio:   {_fmt_opt(results['sortino_ratio'])}",
        f"    Max Drawdown:    {results['max_drawdown_pct']:.2f}%",
        f"    Calmar Ratio:    {results.get('calmar_ratio', 0):.3f}",
        f"{'─'*60}",
        f"  TRADE STATS",
        f"    Total Trades:    {results['total_trades']}",
        f"    Win Rate:        {results['win_rate']:.1f}%",
        f"    Profit Factor:   {_fmt_opt(results['profit_factor'])}",
        f"    Avg Win:         {results.get('avg_win_pct', 0):+.2f}%",
        f"    Avg Loss:        {results.get('avg_loss_pct', 0):+.2f}%",
        f"{'='*60}",
    ]
    return "\n".join(lines)


if __name__ == "__main__":
    np.random.seed(42)
    dates = pd.date_range("2023-01-01", periods=200, freq="D")
    prices = 100 + np.cumsum(np.random.randn(200) * 2)
    df = pd.DataFrame({
        "close": prices,
    }, index=dates)

    df["signal"] = 0
    df.iloc[10, df.columns.get_loc("signal")] = 1
    df.iloc[30, df.columns.get_loc("signal")] = -1
    df.iloc[50, df.columns.get_loc("signal")] = 1
    df.iloc[80, df.columns.get_loc("signal")] = -1

    bt = Backtester(initial_capital=1000)
    results = bt.run(df, strategy_name="Test", save=False)
    print(format_results(results))
