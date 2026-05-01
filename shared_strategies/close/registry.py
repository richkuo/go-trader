"""Registry for position-aware close strategy evaluators."""

from __future__ import annotations

import os
import sys
from typing import Any, Callable, Dict, Optional, Tuple

_THIS_DIR = os.path.dirname(__file__)
if _THIS_DIR not in sys.path:
    sys.path.insert(0, _THIS_DIR)

from tiered_tp_atr import DEFAULT_TIERS as DEFAULT_ATR_TIERS
from tiered_tp_atr import evaluate as tiered_tp_atr_evaluate
from tiered_tp_atr_live import evaluate as tiered_tp_atr_live_evaluate
from tiered_tp_pct import DEFAULT_TIERS as DEFAULT_PCT_TIERS
from tiered_tp_pct import evaluate as tiered_tp_pct_evaluate
from tp_at_pct import evaluate as tp_at_pct_evaluate

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


def evaluate(name: str, position: dict, market: dict, params: Optional[dict] = None) -> dict:
    if name not in STRATEGIES:
        raise ValueError(f"Unknown close strategy: {name}. Available: {list(STRATEGIES.keys())}")
    entry = STRATEGIES[name]
    merged = {**entry["default_params"], **(params or {})}
    return _normalize_result(name, entry["fn"](position or {}, market or {}, merged))


register(
    "tiered_tp_pct",
    "Tiered take-profit by percentage move from average cost",
    {"tiers": list(DEFAULT_PCT_TIERS)},
)(tiered_tp_pct_evaluate)

register(
    "tiered_tp_atr",
    "Tiered take-profit by ATR multiples from average cost",
    {"tiers": list(DEFAULT_ATR_TIERS)},
)(tiered_tp_atr_evaluate)

register(
    "tiered_tp_atr_live",
    "Tiered take-profit by ATR multiples using live ATR per tick (atr_source: live|entry)",
    {"tiers": list(DEFAULT_ATR_TIERS), "atr_source": "live"},
)(tiered_tp_atr_live_evaluate)

register(
    "tp_at_pct",
    "Position-aware percentage take-profit close",
    {"pct": 0.03},
)(tp_at_pct_evaluate)
