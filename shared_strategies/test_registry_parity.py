"""Parity tests for ``shared_strategies/registry.py``.

These guard the invariants that make the unified registry safe:

* Every strategy advertises at least one valid platform.
* ``variants`` overrides only reference platforms the strategy is tagged for.
* No name is registered twice.
* Every strategy exposed by a shim applies cleanly on synthetic OHLCV data
  and returns a DataFrame with a ``signal`` column.
"""

import importlib.util
import os

import pytest

_HERE = os.path.dirname(os.path.abspath(__file__))
_SHARED_DIR = _HERE
_SPOT_DIR = os.path.join(_HERE, "spot")


def _load(name: str, path: str):
    spec = importlib.util.spec_from_file_location(name, path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


@pytest.fixture(scope="module")
def registry():
    return _load("_registry_under_test", os.path.join(_SHARED_DIR, "registry.py"))


@pytest.fixture(scope="module")
def spot_shim():
    return _load("_spot_shim_under_test", os.path.join(_SPOT_DIR, "strategies.py"))


@pytest.fixture(scope="module")
def futures_shim():
    return _load(
        "_futures_shim_under_test",
        os.path.join(_HERE, "futures", "strategies.py"),
    )


@pytest.fixture(scope="module")
def conftest_helpers():
    return _load("_conftest_helpers_parity", os.path.join(_HERE, "conftest.py"))


def test_platforms_non_empty_and_valid(registry):
    valid = set(registry.VALID_PLATFORMS)
    for name, entry in registry.STRATEGIES.items():
        platforms = entry["platforms"]
        assert platforms, f"{name}: platforms tuple is empty"
        bad = set(platforms) - valid
        assert not bad, f"{name}: unknown platforms {sorted(bad)}"


def test_variants_subset_of_platforms(registry):
    for name, entry in registry.STRATEGIES.items():
        bad = set(entry["variants"]) - set(entry["platforms"])
        assert not bad, (
            f"{name}: variants reference platforms {sorted(bad)} "
            f"outside its platforms tuple {entry['platforms']}"
        )


def test_no_duplicate_registration(registry):
    # ``register`` raises if the same name is registered twice; this test
    # guards against a future refactor relaxing that check.
    with pytest.raises(ValueError, match="already registered"):
        @registry.register("sma_crossover", "dup", {})
        def _():  # pragma: no cover
            return None


def test_platform_order_matches_platform_tags(registry):
    for platform in registry.VALID_PLATFORMS:
        tagged = {n for n, e in registry.STRATEGIES.items() if platform in e["platforms"]}
        order = set(registry.PLATFORM_ORDER[platform])
        assert tagged == order, (
            f"PLATFORM_ORDER[{platform!r}] mismatch: "
            f"tagged={sorted(tagged)}, order={sorted(order)}"
        )


def test_build_registry_rejects_unknown_platform(registry):
    with pytest.raises(ValueError, match="Unknown platform"):
        registry.build_registry("options")


def _skip_funding(name: str) -> bool:
    # delta_neutral_funding requires funding-rate params injected at runtime
    # (check_hyperliquid / check_okx do this) and returns zeros otherwise,
    # which is exactly what we'd want this smoke test to exercise. Keep it in.
    return False


def _apply_each(shim, helpers):
    import pandas as pd
    # Some strategies (amd_ifvg, vwap_reversion) need a DatetimeIndex for
    # session/day bucketing \u2014 supply one at 15-minute granularity so every
    # strategy lands in a valid code path.
    idx = pd.date_range("2024-01-01", periods=200, freq="15min")
    df = helpers.make_ohlcv(helpers.make_trending_up(200), index=idx)
    for name in shim.list_strategies():
        result = shim.apply_strategy(name, df)
        assert "signal" in result.columns, f"{name}: missing 'signal' column"


def test_spot_shim_applies_every_registered_strategy(spot_shim, conftest_helpers):
    _apply_each(spot_shim, conftest_helpers)


def test_futures_shim_applies_every_registered_strategy(futures_shim, conftest_helpers):
    _apply_each(futures_shim, conftest_helpers)


def test_shims_produce_independent_registries(spot_shim, futures_shim):
    assert spot_shim.STRATEGY_REGISTRY is not futures_shim.STRATEGY_REGISTRY
    # Spot-only vs futures-only
    assert "pairs_spread" in spot_shim.STRATEGY_REGISTRY
    assert "pairs_spread" not in futures_shim.STRATEGY_REGISTRY
    assert "breakout" not in spot_shim.STRATEGY_REGISTRY
    assert "breakout" in futures_shim.STRATEGY_REGISTRY
    assert "delta_neutral_funding" not in spot_shim.STRATEGY_REGISTRY
    assert "delta_neutral_funding" in futures_shim.STRATEGY_REGISTRY
    assert "triple_ema_bidir" not in spot_shim.STRATEGY_REGISTRY
    assert "triple_ema_bidir" in futures_shim.STRATEGY_REGISTRY


def test_momentum_variant_overrides_threshold(spot_shim, futures_shim):
    # The only non-description default_params override; spot uses 5.0, futures 3.0.
    assert spot_shim.STRATEGY_REGISTRY["momentum"]["default_params"]["threshold"] == 5.0
    assert futures_shim.STRATEGY_REGISTRY["momentum"]["default_params"]["threshold"] == 3.0


def test_variant_descriptions_land_on_the_right_platform(spot_shim, futures_shim):
    # Sanity spot-check a variant-bearing strategy description.
    assert "buy at oversold" in spot_shim.STRATEGY_REGISTRY["rsi"]["description"]
    assert "for futures" in futures_shim.STRATEGY_REGISTRY["rsi"]["description"]
