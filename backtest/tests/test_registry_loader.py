"""Regression tests: the spot and futures strategy registries must both be
loadable and coexist in the same process (sys.modules['strategies'] clobber
is the failure mode this guards against)."""
import os

import pytest

from registry_loader import load_registry, registry_for_strategy_type
from optimizer import DEFAULT_PARAM_RANGES


def test_load_spot_registry():
    mod = load_registry("spot")
    assert "sma_crossover" in mod.STRATEGY_REGISTRY
    assert "pairs_spread" in mod.STRATEGY_REGISTRY
    # spot registry must NOT carry futures-only strategies
    assert "delta_neutral_funding" not in mod.STRATEGY_REGISTRY
    assert "breakout" not in mod.STRATEGY_REGISTRY


def test_load_futures_registry():
    mod = load_registry("futures")
    assert "sma_crossover" in mod.STRATEGY_REGISTRY
    assert "delta_neutral_funding" in mod.STRATEGY_REGISTRY
    assert "breakout" in mod.STRATEGY_REGISTRY
    # futures registry must NOT carry spot-only strategies
    assert "pairs_spread" not in mod.STRATEGY_REGISTRY


def test_both_registries_coexist():
    """Importing both must not overwrite the other's STRATEGY_REGISTRY —
    regression against sys.modules['strategies'] being clobbered."""
    spot = load_registry("spot")
    fut = load_registry("futures")
    assert spot is not fut
    assert spot.STRATEGY_REGISTRY is not fut.STRATEGY_REGISTRY
    assert "pairs_spread" in spot.STRATEGY_REGISTRY
    assert "breakout" in fut.STRATEGY_REGISTRY


def test_unknown_platform_rejected():
    with pytest.raises(ValueError, match="Unknown platform"):
        load_registry("options")


@pytest.mark.parametrize("strategy_type,expected", [
    ("spot", "spot"),
    ("options", "spot"),
    ("perps", "futures"),
    ("futures", "futures"),
    ("manual", "futures"),
    (" PERPS ", "futures"),
])
def test_registry_for_strategy_type(strategy_type, expected):
    assert registry_for_strategy_type(strategy_type) == expected


def test_param_ranges_cover_every_registered_strategy():
    spot_ids = set(load_registry("spot").STRATEGY_REGISTRY.keys())
    fut_ids = set(load_registry("futures").STRATEGY_REGISTRY.keys())
    missing = (spot_ids | fut_ids) - set(DEFAULT_PARAM_RANGES.keys())
    assert not missing, (
        f"Strategies without DEFAULT_PARAM_RANGES — walk-forward will fall "
        f"back to a single-point grid: {sorted(missing)}"
    )


def test_empty_registry_raises():
    """If the strategy file loaded but produced an empty STRATEGY_REGISTRY
    (e.g. all decorators accidentally removed), every caller would see
    'Unknown strategy' indistinguishable from a typo — raise instead."""
    import tempfile
    import importlib

    import registry_loader

    with tempfile.TemporaryDirectory() as tmp:
        empty_dir = os.path.join(tmp, "empty")
        os.makedirs(empty_dir)
        with open(os.path.join(empty_dir, "strategies.py"), "w") as f:
            f.write("STRATEGY_REGISTRY = {}\n")

        orig_dirs = registry_loader._PLATFORM_DIRS.copy()
        orig_cached = registry_loader._cached.copy()
        try:
            registry_loader._PLATFORM_DIRS["_empty"] = empty_dir
            registry_loader._cached.pop("_empty", None)
            with pytest.raises(RuntimeError, match="STRATEGY_REGISTRY is missing or empty"):
                registry_loader.load_registry("_empty")
        finally:
            registry_loader._PLATFORM_DIRS.clear()
            registry_loader._PLATFORM_DIRS.update(orig_dirs)
            registry_loader._cached.clear()
            registry_loader._cached.update(orig_cached)
