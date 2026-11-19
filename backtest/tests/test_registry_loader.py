

from registry_loader import load_registry
from optimizer import DEFAULT_PARAM_RANGES


def test_param_ranges_cover_every_registered_strategy():
    spot_ids = set(load_registry("spot").STRATEGY_REGISTRY.keys())
    fut_ids = set(load_registry("futures").STRATEGY_REGISTRY.keys())
    missing = (spot_ids | fut_ids) - set(DEFAULT_PARAM_RANGES.keys())
    assert not missing, (
        f"Strategies without DEFAULT_PARAM_RANGES — walk-forward will fall "
        f"back to a single-point grid: {sorted(missing)}"
    )
