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
