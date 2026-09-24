import os

import pytest

from registry_loader import load_registry, registry_for_strategy_type
from optimizer import DEFAULT_PARAM_RANGES


@pytest.mark.parametrize("platform,present,absent", [
    ("spot", ["sma_crossover", "pairs_spread"], ["delta_neutral_funding", "breakout"]),
    ("futures", ["sma_crossover", "delta_neutral_funding", "breakout"], ["pairs_spread"]),
])
def test_load_registry_membership(platform, present, absent):
    mod = load_registry(platform)
    for name in present:
        assert name in mod.STRATEGY_REGISTRY
    for name in absent:
        assert name not in mod.STRATEGY_REGISTRY


def test_both_registries_coexist():
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
