import importlib.util
import os
import sys
from types import ModuleType

_ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
_OPEN_DIR = os.path.join(_ROOT, "shared_strategies", "open")
_SPOT_DIR = os.path.join(_OPEN_DIR, "spot")
_FUTURES_DIR = os.path.join(_OPEN_DIR, "futures")
_SHARED_DIR = _OPEN_DIR
_TOOLS_DIR = os.path.join(_ROOT, "shared_tools")

_PLATFORM_DIRS = {"spot": _SPOT_DIR, "futures": _FUTURES_DIR}
_cached: dict = {}


def registry_for_strategy_type(strategy_type: str) -> str:
    key = str(strategy_type or "").strip().lower()
    return "futures" if key in ("perps", "futures", "manual") else "spot"


def _ensure_import_paths() -> None:
    for p in (_SPOT_DIR, _SHARED_DIR, _TOOLS_DIR):
        if p not in sys.path:
            sys.path.insert(0, p)


def load_registry(platform: str = "spot") -> ModuleType:
    key = platform.lower()
    if key not in _PLATFORM_DIRS:
        raise ValueError(
            f"Unknown platform '{platform}' — expected 'spot' or 'futures'"
        )
    if key in _cached:
        return _cached[key]
    _ensure_import_paths()
    path = os.path.join(_PLATFORM_DIRS[key], "strategies.py")
    spec = importlib.util.spec_from_file_location(
        f"_backtest_{key}_strategies", path
    )
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    registry = getattr(mod, "STRATEGY_REGISTRY", None)
    if not registry:
        raise RuntimeError(
            f"{path} loaded but STRATEGY_REGISTRY is missing or empty"
        )
    _cached[key] = mod
    return mod
