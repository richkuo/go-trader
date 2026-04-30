"""Spot strategy shim \u2014 platform-filtered view of shared_strategies.registry.

All strategy implementations live in ``shared_strategies/open/registry.py``; this
file exposes the subset tagged ``platforms=("spot", ...)`` with any spot-specific
``variants`` applied. ``check_strategy.py`` and the spot backtester path import
from this module via sys.path insertion (``from strategies import ...``), so
the surface (``STRATEGY_REGISTRY`` dict, ``apply_strategy``, ``list_strategies``,
``get_strategy``) must stay stable.
"""

import importlib.util
import json
import os
import sys
from typing import Dict, List, Optional

import pandas as pd

_TOOLS_DIR = os.path.join(os.path.dirname(__file__), "..", "..", "..", "shared_tools")
if _TOOLS_DIR not in sys.path:
    sys.path.insert(0, _TOOLS_DIR)

from strategy_composition import strip_unsupported_position_context


def _load_registry_module():
    """Load ``shared_strategies/open/registry.py`` as an isolated module.

    We use ``spec_from_file_location`` instead of a package import so this
    shim works whether callers put ``shared_strategies/open/spot/`` or the repo
    root on sys.path \u2014 and so spot and futures shims each get a fresh
    registry instance (the ``test_both_registries_coexist`` contract).
    """
    registry_path = os.path.join(os.path.dirname(__file__), "..", "registry.py")
    spec = importlib.util.spec_from_file_location("_strategy_registry_spot", registry_path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


_registry = _load_registry_module()

STRATEGY_REGISTRY: Dict[str, dict] = _registry.build_registry("spot")


def get_strategy(name: str) -> dict:
    if name not in STRATEGY_REGISTRY:
        raise ValueError(f"Unknown strategy: {name}. Available: {list(STRATEGY_REGISTRY.keys())}")
    return STRATEGY_REGISTRY[name]


def list_strategies() -> List[str]:
    return list(STRATEGY_REGISTRY.keys())


def apply_strategy(name: str, df: pd.DataFrame, params: Optional[dict] = None) -> pd.DataFrame:
    """Apply a named strategy with optional parameter overrides."""
    strat = get_strategy(name)
    p = {**strat["default_params"], **(params or {})}
    p = strip_unsupported_position_context(strat["fn"], p)
    return strat["fn"](df, **p)


if __name__ == "__main__":
    if "--list-json" in sys.argv:
        print(json.dumps([{"id": name, "description": STRATEGY_REGISTRY[name]["description"]} for name in list_strategies()]))
    else:
        print(f"Registered strategies: {list_strategies()}")
        for name in list_strategies():
            s = STRATEGY_REGISTRY[name]
            print(f"  {name}: {s['description']}")
            print(f"    Defaults: {s['default_params']}")
