"""Load the close strategy registry without relying on ambiguous sys.path imports."""

from __future__ import annotations

import importlib.util
import os
from types import ModuleType
from typing import Optional

_ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
_CLOSE_REGISTRY_PATH = os.path.join(_ROOT, "shared_strategies", "close", "registry.py")
_cached: Optional[ModuleType] = None


def _load_registry() -> ModuleType:
    global _cached
    if _cached is not None:
        return _cached
    spec = importlib.util.spec_from_file_location("_go_trader_close_registry", _CLOSE_REGISTRY_PATH)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    _cached = mod
    return mod


def evaluate(name: str, position: dict, market: dict, params: Optional[dict] = None) -> dict:
    return _load_registry().evaluate(name, position, market, params)


def build_close_registry(platform: str):
    return _load_registry().build_close_registry(platform)


def get_strategy(name: str) -> dict:
    registry = _load_registry().STRATEGIES
    if name not in registry:
        raise ValueError(f"Unknown close strategy: {name}. Available: {list(registry.keys())}")
    return registry[name]


def list_strategies() -> list[str]:
    return list(_load_registry().STRATEGIES.keys())


def list_strategies_detailed() -> list[dict]:
    """Full catalog for operator-facing surfaces (#1203): one dict per
    registered evaluator with its description, default params, and supported
    platforms. Sorted by name so output is deterministic across runs."""
    registry = _load_registry().STRATEGIES
    return [
        {
            "name": name,
            "description": registry[name]["description"],
            "default_params": registry[name]["default_params"],
            "platforms": list(registry[name]["platforms"]),
        }
        for name in sorted(registry.keys())
    ]


if __name__ == "__main__":
    import json
    import sys

    if "--list-json" in sys.argv:
        try:
            print(json.dumps(list_strategies_detailed()))
        except Exception as exc:  # subprocess contract: JSON to stdout even on error
            print(json.dumps({"error": str(exc)}))
            sys.exit(1)
    else:
        print(json.dumps({"error": "usage: close_registry_loader.py --list-json"}))
        sys.exit(1)
