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
        assert set(built) == {
            "tiered_tp_pct",
            "tiered_tp_atr",
            "tiered_tp_atr_live",
            "tiered_tp_atr_regime",
            "tiered_tp_atr_live_regime",
            "tiered_tp_atr_live_regime_dynamic",
            "trailing_tp_ratchet",
            "trailing_tp_ratchet_regime",
            "time_stop",
            "atr_stop",
            "zscore_target",
            "avwap_stop",
        }


def test_build_close_registry_rejects_unknown_platform(registry):
    with pytest.raises(ValueError, match="Unknown platform"):
        registry.build_close_registry("perps")


def test_evaluate_rejects_unknown_strategy(registry):
    with pytest.raises(ValueError, match="Unknown close strategy"):
        registry.evaluate("missing", {}, {}, {})


def test_tp_at_pct_deprecated_shim(registry):
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
    assert long_hit == {"close_fraction": 1.0, "reason": "tiered_tp_pct:0.03"}
    assert short_hit == {"close_fraction": 1.0, "reason": "tiered_tp_pct:0.03"}


def test_tiered_tp_pct_closes_only_unfilled_tier_amount(registry):
    first = registry.evaluate(
        "tiered_tp_pct",
        {"side": "long", "avg_cost": 100, "current_quantity": 1, "initial_quantity": 1},
        {"mark_price": 102},
        {},
    )
    already_taken = registry.evaluate(
        "tiered_tp_pct",
        {"side": "long", "avg_cost": 100, "current_quantity": 0.5, "initial_quantity": 1},
        {"mark_price": 102},
        {},
    )
    final = registry.evaluate(
        "tiered_tp_pct",
        {"side": "long", "avg_cost": 100, "current_quantity": 0.5, "initial_quantity": 1},
        {"mark_price": 104},
        {},
    )

    assert first == {"close_fraction": 0.5, "reason": "tiered_tp_pct:0.02"}
    assert already_taken == {"close_fraction": 0.0, "reason": "noop:already_taken"}
    assert final == {"close_fraction": 1.0, "reason": "tiered_tp_pct:0.04"}


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
        {"mark_price": 110},
        {},
    )

    assert missing_atr == {"close_fraction": 0.0, "reason": "noop:missing_entry_atr"}
    assert hit == {"close_fraction": 1.0, "reason": "tiered_tp_atr:5"}


_LIVE_POS = {"side": "long", "avg_cost": 100, "current_quantity": 1,
             "initial_quantity": 1, "entry_atr": 2}


@pytest.mark.parametrize("position,market,params,expected", [
    (_LIVE_POS, {"mark_price": 105, "atr": 3}, {},
     {"close_fraction": 0.4, "reason": "tiered_tp_atr_live:live:1.5"}),
    (_LIVE_POS, {"mark_price": 110}, {},
     {"close_fraction": 1.0, "reason": "tiered_tp_atr_live:entry_fallback:5"}),
    (_LIVE_POS, {"mark_price": 103, "atr": 0}, {},
     {"close_fraction": 0.4, "reason": "tiered_tp_atr_live:entry_fallback:1.5"}),
    ({"side": "long", "avg_cost": 100, "current_quantity": 1, "initial_quantity": 1},
     {"mark_price": 104}, {},
     {"close_fraction": 0.0, "reason": "noop:missing_atr"}),
    (_LIVE_POS, {"mark_price": 110, "atr": 10}, {"atr_source": "entry"},
     {"close_fraction": 1.0, "reason": "tiered_tp_atr_live:entry:5"}),
    ({"side": "short", "avg_cost": 100, "current_quantity": 1,
      "initial_quantity": 1, "entry_atr": 5},
     {"mark_price": 90, "atr": 2}, {},
     {"close_fraction": 1.0, "reason": "tiered_tp_atr_live:live:5"}),
])
def test_tiered_tp_atr_live_atr_source_resolution(registry, position, market, params, expected):
    assert registry.evaluate("tiered_tp_atr_live", position, market, params) == expected


def test_market_atr_wiring_end_to_end(registry):
    import sys
    from pathlib import Path

    import pandas as pd

    sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "shared_tools"))
    from atr import latest_atr

    n = 30
    df = pd.DataFrame({
        "open": [100.0] * n,
        "high": [101.5] * n,
        "low": [98.5] * n,
        "close": [100.0] * n,
        "volume": [1.0] * n,
    })
    atr_value = latest_atr(df)
    assert atr_value > 0, "latest_atr must produce a positive value for live wiring"

    market_ctx = {"mark_price": float(df["close"].iloc[-1])}
    if atr_value > 0:
        market_ctx["atr"] = atr_value

    market_ctx["mark_price"] = 106
    result = registry.evaluate(
        "tiered_tp_atr_live",
        {
            "side": "long",
            "avg_cost": 100,
            "current_quantity": 1,
            "initial_quantity": 1,
            "entry_atr": 99,
        },
        market_ctx,
        {},
    )
    assert result["reason"].startswith("tiered_tp_atr_live:live:"), (
        f"market_ctx['atr'] not flowing through to evaluator: reason={result['reason']!r}"
    )


def _load_helpers():
    path = Path(__file__).resolve().parent / "_helpers.py"
    spec = importlib.util.spec_from_file_location("_close_helpers_under_test", path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def test_tier_list_from_params_canonical_only():
    h = _load_helpers()
    tp = [{"atr_multiple": 2.0, "close_fraction": 1.0}]

    assert h.tier_list_from_params({"tp_tiers": tp}) == tp
    assert h.tier_list_from_params({"tiers": tp}) is None
    assert h.tier_list_from_params({"atr_source": "live"}) is None
    assert h.tier_list_from_params(None) is None


def test_evaluate_reads_tp_tiers(registry):
    position = {
        "avg_cost": 100.0,
        "current_quantity": 1.0,
        "initial_quantity": 1.0,
        "entry_atr": 10.0,
        "side": "long",
    }
    market = {"mark_price": 130.0}
    ladder = [{"atr_multiple": 2.0, "close_fraction": 1.0}]

    hit = registry.evaluate("tiered_tp_atr", position, market, {"tp_tiers": ladder})
    assert hit["close_fraction"] == 1.0
    assert hit["reason"] == "tiered_tp_atr:2"


def test_unified_regime_block_evaluator(registry):
    params = {
        "trend_regime": {
            "trending_up": {"stop_loss_atr": 1.5, "tp_tiers": [
                {"atr_multiple": 2.0, "close_fraction": 0.5},
                {"atr_multiple": 4.0, "close_fraction": 1.0},
            ]},
            "trending_down": {"tp_tiers": [
                {"atr_multiple": 1.8, "close_fraction": 0.5},
                {"atr_multiple": 3.0, "close_fraction": 1.0},
            ]},
            "ranging": {"tp_tiers": [
                {"atr_multiple": 1.0, "close_fraction": 0.5},
                {"atr_multiple": 2.0, "close_fraction": 1.0},
            ]},
        }
    }
    base_pos = {"avg_cost": 100.0, "current_quantity": 1.0,
                "initial_quantity": 1.0, "entry_atr": 10.0, "side": "long"}
    market = {"mark_price": 130.0}

    up = registry.evaluate("tiered_tp_atr_regime", {**base_pos, "regime": "trending_up"}, market, params)
    assert up["close_fraction"] == pytest.approx(0.5), up

    rng = registry.evaluate("tiered_tp_atr_regime", {**base_pos, "regime": "ranging"}, market, params)
    assert rng["close_fraction"] == pytest.approx(1.0), rng

    none = registry.evaluate("tiered_tp_atr_regime", base_pos, market, params)
    assert none["close_fraction"] == 0.0
