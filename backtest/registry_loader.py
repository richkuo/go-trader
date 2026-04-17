"""Load the spot/ or futures/ strategy registry by platform name.

Both ``shared_strategies/spot/strategies.py`` and
``shared_strategies/futures/strategies.py`` expose a module-level
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
_SPOT_DIR = os.path.join(_ROOT, "shared_strategies", "spot")
_FUTURES_DIR = os.path.join(_ROOT, "shared_strategies", "futures")
_SHARED_DIR = os.path.join(_ROOT, "shared_strategies")
_TOOLS_DIR = os.path.join(_ROOT, "shared_tools")

_PLATFORM_DIRS = {"spot": _SPOT_DIR, "futures": _FUTURES_DIR}
_cached: dict = {}


def _ensure_import_paths() -> None:
    # Strategy modules resolve ``indicators``, ``amd_ifvg``, etc. via sys.path.
    # Inject once per process so both registries can import their deps.
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
    _cached[key] = mod
    return mod
