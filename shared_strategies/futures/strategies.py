"""Futures strategy shim \u2014 platform-filtered view of shared_strategies.registry.

All strategy implementations live in ``shared_strategies/registry.py``; this
file exposes the subset tagged ``platforms=("futures", ...)`` with any
futures-specific ``variants`` applied. ``check_hyperliquid.py``,
``check_topstep.py``, ``check_robinhood.py``, and ``check_okx.py`` (swap mode)
import from this module via sys.path insertion (``from strategies import ...``),
so the surface (``STRATEGY_REGISTRY``, ``apply_strategy``, ``list_strategies``,
``get_strategy``) must stay stable.
"""

import importlib.util
import inspect
import json
import os
import sys
from typing import Dict, List, Optional

import pandas as pd


def _load_registry_module():
    """Load ``shared_strategies/registry.py`` as an isolated module \u2014 see
    spot/strategies.py for rationale (independent modules per shim)."""
    registry_path = os.path.join(os.path.dirname(__file__), "..", "registry.py")
    spec = importlib.util.spec_from_file_location("_strategy_registry_futures", registry_path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


_registry = _load_registry_module()

STRATEGY_REGISTRY: Dict[str, dict] = _registry.build_registry("futures")
POSITION_CONTEXT_PARAM_KEYS = {"side", "avg_cost", "current_quantity", "initial_quantity", "entry_atr"}


def get_strategy(name: str) -> dict:
    if name not in STRATEGY_REGISTRY:
        raise ValueError(f"Unknown strategy: {name}. Available: {list(STRATEGY_REGISTRY.keys())}")
    return STRATEGY_REGISTRY[name]


def list_strategies() -> List[str]:
    return list(STRATEGY_REGISTRY.keys())


def apply_strategy(name: str, df: pd.DataFrame, params: Optional[dict] = None) -> pd.DataFrame:
    strat = get_strategy(name)
    p = {**strat["default_params"], **(params or {})}
    p = _strip_unsupported_position_context(strat["fn"], p)
    return strat["fn"](df, **p)


def _strip_unsupported_position_context(fn, params: dict) -> dict:
    if not params:
        return params
    sig = inspect.signature(fn)
    if any(p.kind == inspect.Parameter.VAR_KEYWORD for p in sig.parameters.values()):
        return params
    accepted = {
        name for name, p in sig.parameters.items()
        if name != "df" and p.kind in (
            inspect.Parameter.POSITIONAL_OR_KEYWORD,
            inspect.Parameter.KEYWORD_ONLY,
        )
    }
    return {
        key: value for key, value in params.items()
        if key in accepted or key not in POSITION_CONTEXT_PARAM_KEYS
    }


if __name__ == "__main__":
    if "--list-json" in sys.argv:
        print(json.dumps([{"id": name, "description": STRATEGY_REGISTRY[name]["description"]} for name in list_strategies()]))
    else:
        print(f"Registered strategies: {list_strategies()}")
        for name in list_strategies():
            s = STRATEGY_REGISTRY[name]
            print(f"  {name}: {s['description']}")
            print(f"    Defaults: {s['default_params']}")
