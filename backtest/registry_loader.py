"""Load the spot/ or futures/ strategy registry by platform name.

Both ``shared_strategies/open/spot/strategies.py`` and
``shared_strategies/open/futures/strategies.py`` expose a module-level
``STRATEGY_REGISTRY`` dict and ``apply_strategy`` / ``list_strategies``
helpers. We load each via ``importlib.util`` under a unique module name so
the two registries can coexist in the same process without clobbering each
other on ``sys.modules['strategies']``.
"""
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


def _ensure_import_paths() -> None:
    # Strategy modules resolve ``indicators``, ``amd_ifvg``, etc. via sys.path.
    for p in (_SPOT_DIR, _SHARED_DIR, _TOOLS_DIR):
        if p not in sys.path:
            sys.path.insert(0, p)


def load_registry(platform: str = "spot") -> ModuleType:
    """Return the strategy module for ``platform`` (``'spot'`` or ``'futures'``)."""
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
    # An accidentally-empty registry (e.g. all @register_strategy decorators
    # removed) otherwise surfaces as "Unknown strategy <name>" at the caller,
    # indistinguishable from a typo.
    registry = getattr(mod, "STRATEGY_REGISTRY", None)
    if not registry:
        raise RuntimeError(
            f"{path} loaded but STRATEGY_REGISTRY is missing or empty"
        )
    _cached[key] = mod
    return mod
