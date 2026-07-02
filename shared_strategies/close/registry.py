"""Registry for position-aware close strategy evaluators."""

from __future__ import annotations

import os
import sys
from typing import Any, Callable, Dict, Optional, Tuple

_THIS_DIR = os.path.dirname(__file__)
if _THIS_DIR not in sys.path:
    sys.path.insert(0, _THIS_DIR)

from _helpers import warn_deprecated_close_key
from tiered_tp_atr import DEFAULT_TIERS as DEFAULT_ATR_TIERS
from tiered_tp_atr import evaluate as tiered_tp_atr_evaluate
from tiered_tp_atr_live import evaluate as tiered_tp_atr_live_evaluate
from tiered_tp_atr_regime import evaluate as tiered_tp_atr_regime_evaluate
from tiered_tp_atr_live_regime import evaluate as tiered_tp_atr_live_regime_evaluate
from tiered_tp_atr_live_regime_dynamic import evaluate as tiered_tp_atr_live_regime_dynamic_evaluate
from trailing_tp_ratchet import DEFAULT_RATCHET_TIERS
from trailing_tp_ratchet import evaluate_regime as trailing_tp_ratchet_regime_evaluate
from trailing_tp_ratchet import evaluate_scalar as trailing_tp_ratchet_evaluate
from tiered_tp_pct import DEFAULT_TIERS as DEFAULT_PCT_TIERS
from tiered_tp_pct import evaluate as tiered_tp_pct_evaluate
from time_stop import evaluate as time_stop_evaluate
from atr_stop import evaluate as atr_stop_evaluate
from zscore_target import evaluate as zscore_target_evaluate
from avwap_stop import evaluate as avwap_stop_evaluate

VALID_PLATFORMS: Tuple[str, ...] = ("spot", "futures", "options")

# name -> {fn, description, default_params, platforms}
STRATEGIES: Dict[str, Dict[str, Any]] = {}


def register(
    name: str,
    description: str,
    default_params: dict,
    platforms: Tuple[str, ...] = VALID_PLATFORMS,
) -> Callable:
    if name in STRATEGIES:
        raise ValueError(f"Close strategy '{name}' is already registered")
    platforms = tuple(platforms)
    if not platforms:
        raise ValueError(f"{name}: platforms must be non-empty")
    bad = set(platforms) - set(VALID_PLATFORMS)
    if bad:
        raise ValueError(
            f"{name}: unknown platforms {sorted(bad)}; "
            f"expected subset of {VALID_PLATFORMS}"
        )

    def decorator(fn):
        STRATEGIES[name] = {
            "fn": fn,
            "description": description,
            "default_params": dict(default_params),
            "platforms": platforms,
        }
        return fn

    return decorator


def build_close_registry(platform: str) -> Dict[str, Dict[str, Any]]:
    if platform not in VALID_PLATFORMS:
        raise ValueError(
            f"Unknown platform {platform!r}; expected one of {VALID_PLATFORMS}"
        )
    return {
        name: {
            "fn": entry["fn"],
            "description": entry["description"],
            "default_params": dict(entry["default_params"]),
        }
        for name, entry in STRATEGIES.items()
        if platform in entry["platforms"]
    }


def _normalize_result(name: str, result: Optional[dict]) -> dict:
    result = result or {}
    try:
        close_fraction = float(result.get("close_fraction", 0) or 0)
    except (TypeError, ValueError):
        close_fraction = 0.0
    close_fraction = min(max(close_fraction, 0.0), 1.0)
    reason = str(result.get("reason") or f"{name}:no_reason")
    return {"close_fraction": close_fraction, "reason": reason}


def _rewrite_deprecated_close(name: str, params: Optional[dict]) -> tuple[str, dict]:
    """One-window shim: tp_at_pct → single-tier tiered_tp_pct (#841)."""
    if name != "tp_at_pct":
        return name, dict(params or {})
    warn_deprecated_close_key("tp_at_pct", "tiered_tp_pct")
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


def evaluate(name: str, position: dict, market: dict, params: Optional[dict] = None) -> dict:
    name, params = _rewrite_deprecated_close(name, params)
    if name not in STRATEGIES:
        raise ValueError(f"Unknown close strategy: {name}. Available: {list(STRATEGIES.keys())}")
    entry = STRATEGIES[name]
    merged = {**entry["default_params"], **(params or {})}
    return _normalize_result(name, entry["fn"](position or {}, market or {}, merged))


register(
    "tiered_tp_pct",
    "Tiered take-profit by percentage move from average cost",
    {"tp_tiers": list(DEFAULT_PCT_TIERS)},
)(tiered_tp_pct_evaluate)

register(
    "tiered_tp_atr",
    "Tiered take-profit by ATR multiples from average cost",
    {"tp_tiers": list(DEFAULT_ATR_TIERS)},
)(tiered_tp_atr_evaluate)

register(
    "tiered_tp_atr_live",
    "Tiered take-profit by ATR multiples using live ATR per tick (atr_source: live|entry)",
    {"tp_tiers": list(DEFAULT_ATR_TIERS), "atr_source": "live"},
)(tiered_tp_atr_live_evaluate)

register(
    "tiered_tp_atr_regime",
    "Regime-aware tiered TP — ATR multiples resolved at open via Position.Regime (#733)",
    # default_params intentionally empty: the Go config loader expands
    # `use_defaults: true` into a concrete tier list before evaluator
    # invocation, and any other shape must be operator-supplied.
    {},
)(tiered_tp_atr_regime_evaluate)

register(
    "tiered_tp_atr_live_regime",
    "Regime-aware tiered TP — live ATR + per-tick regime classification (#733)",
    {"atr_source": "live"},
)(tiered_tp_atr_live_regime_evaluate)

register(
    "tiered_tp_atr_live_regime_dynamic",
    "Unified per-regime TP/SL — live ATR-regime re-resolution (#843; HL live uses on-chain sync)",
    {"atr_source": "live", "regime_confirm_cycles": 2},
)(tiered_tp_atr_live_regime_dynamic_evaluate)

register(
    "trailing_tp_ratchet",
    "Tiered trail ratchet — tightens trailing_stop_atr_mult at each ATR tier (close_fraction may be 0)",
    {"tp_tiers": [dict(t) for t in DEFAULT_RATCHET_TIERS]},
)(trailing_tp_ratchet_evaluate)

register(
    "trailing_tp_ratchet_regime",
    "Regime-keyed tiered trail ratchet — frozen at open via Position.Regime (#844)",
    {},
)(trailing_tp_ratchet_regime_evaluate)

# #997 M3 exit-quality knobs — default-off; backtest-wired, live wiring deferred.
register(
    "time_stop",
    "Holding-time cap — full close after max_bars held (default-off; needs bars_held context)",
    {"max_bars": 0},
)(time_stop_evaluate)

register(
    "atr_stop",
    "Standalone ATR stop — full close at atr_mult ATR against avg_cost (default-off; atr_source: entry|live)",
    {"atr_mult": 0.0, "atr_source": "entry"},
)(atr_stop_evaluate)

register(
    "zscore_target",
    "Z-score target exit — full close when price stretches z_target sigma in favour (default-off; needs zscore context)",
    {"lookback": 0, "z_target": 0.0},
)(zscore_target_evaluate)

# #1196 AVWAP loss-of-line exit. Reads market["avwap"] — the live re-anchored
# line injected from the AVWAP-family open strategy's own `avwap` column by
# evaluate_open_close (live) and the backtester; fails safe (no-op) without it.
register(
    "avwap_stop",
    "AVWAP loss-of-line exit — full close when the mark breaches the anchored VWAP by buffer_atr_mult ATR (needs market avwap context; atr_source: live|entry)",
    {"buffer_atr_mult": 0.25, "atr_source": "live"},
)(avwap_stop_evaluate)
