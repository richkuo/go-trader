"""
Regression tests for issue #303 H1 — backtest must be able to load either
the spot or the futures strategy registry, and the two registries must be
able to coexist in one process.
"""
import pytest

from registry_loader import load_registry
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


def test_param_ranges_cover_every_registered_strategy():
    """Every strategy in either registry should have a DEFAULT_PARAM_RANGES
    entry. Missing entries fall back to default_params (single-point grid),
    but the ideal state is coverage — a gap means optimize mode silently
    degrades to no tuning."""
    spot_ids = set(load_registry("spot").STRATEGY_REGISTRY.keys())
    fut_ids = set(load_registry("futures").STRATEGY_REGISTRY.keys())
    all_ids = spot_ids | fut_ids
    missing = all_ids - set(DEFAULT_PARAM_RANGES.keys())
    assert not missing, (
        f"Strategies without DEFAULT_PARAM_RANGES — walk-forward will fall "
        f"back to a single-point grid: {sorted(missing)}"
    )
