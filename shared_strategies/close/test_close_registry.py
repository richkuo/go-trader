import importlib.util
from pathlib import Path

import pytest


def _load_close_registry():
    path = Path(__file__).resolve().parent / "registry.py"
    spec = importlib.util.spec_from_file_location("_close_registry_under_test", path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


@pytest.fixture(scope="module")
def registry():
    return _load_close_registry()


def test_build_close_registry_filters_valid_platforms(registry):
    assert tuple(registry.VALID_PLATFORMS) == ("spot", "futures", "options")
    for platform in registry.VALID_PLATFORMS:
        built = registry.build_close_registry(platform)
        assert set(built) == {"tiered_tp_pct", "tiered_tp_atr", "tp_at_pct"}


def test_build_close_registry_rejects_unknown_platform(registry):
    with pytest.raises(ValueError, match="Unknown platform"):
        registry.build_close_registry("perps")


def test_evaluate_rejects_unknown_strategy(registry):
    with pytest.raises(ValueError, match="Unknown close strategy"):
        registry.evaluate("missing", {}, {}, {})


def test_tp_at_pct_hits_for_long_and_short(registry):
    long_hit = registry.evaluate(
        "tp_at_pct",
        {"side": "long", "avg_cost": 100, "current_quantity": 1},
        {"mark_price": 103},
        {"pct": 0.03},
    )
    short_hit = registry.evaluate(
        "tp_at_pct",
        {"side": "short", "avg_cost": 100, "current_quantity": 1},
        {"mark_price": 97},
        {"pct": 0.03},
    )
    assert long_hit == {"close_fraction": 1.0, "reason": "tp_at_pct:hit"}
    assert short_hit == {"close_fraction": 1.0, "reason": "tp_at_pct:hit"}


def test_tiered_tp_pct_closes_only_unfilled_tier_amount(registry):
    first = registry.evaluate(
        "tiered_tp_pct",
        {"side": "long", "avg_cost": 100, "current_quantity": 1, "initial_quantity": 1},
        {"mark_price": 103},
        {},
    )
    already_taken = registry.evaluate(
        "tiered_tp_pct",
        {"side": "long", "avg_cost": 100, "current_quantity": 0.5, "initial_quantity": 1},
        {"mark_price": 103},
        {},
    )
    final = registry.evaluate(
        "tiered_tp_pct",
        {"side": "long", "avg_cost": 100, "current_quantity": 0.5, "initial_quantity": 1},
        {"mark_price": 106},
        {},
    )

    assert first == {"close_fraction": 0.5, "reason": "tiered_tp_pct:0.03"}
    assert already_taken == {"close_fraction": 0.0, "reason": "noop:already_taken"}
    assert final == {"close_fraction": 1.0, "reason": "tiered_tp_pct:0.06"}


def test_tiered_tp_atr_uses_entry_atr_multiple(registry):
    missing_atr = registry.evaluate(
        "tiered_tp_atr",
        {"side": "long", "avg_cost": 100, "current_quantity": 1, "initial_quantity": 1},
        {"mark_price": 103},
        {},
    )
    hit = registry.evaluate(
        "tiered_tp_atr",
        {"side": "long", "avg_cost": 100, "current_quantity": 1, "initial_quantity": 1, "entry_atr": 2},
        {"mark_price": 104},
        {},
    )

    assert missing_atr == {"close_fraction": 0.0, "reason": "noop:missing_entry_atr"}
    assert hit == {"close_fraction": 1.0, "reason": "tiered_tp_atr:2"}
