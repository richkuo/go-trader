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
        }


def test_build_close_registry_rejects_unknown_platform(registry):
    with pytest.raises(ValueError, match="Unknown platform"):
        registry.build_close_registry("perps")


def test_evaluate_rejects_unknown_strategy(registry):
    with pytest.raises(ValueError, match="Unknown close strategy"):
        registry.evaluate("missing", {}, {}, {})


def test_tp_at_pct_deprecated_shim(registry):
    """#841: tp_at_pct is rewritten to single-tier tiered_tp_pct via read shim."""
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


def test_tiered_tp_atr_live_uses_market_atr(registry):
    # Live ATR (3.0) means 104 mark = 1.33 ATR profit, hits the 1.0x tier (50%).
    live_hit = registry.evaluate(
        "tiered_tp_atr_live",
        {"side": "long", "avg_cost": 100, "current_quantity": 1, "initial_quantity": 1, "entry_atr": 2},
        {"mark_price": 104, "atr": 3},
        {},
    )
    assert live_hit == {"close_fraction": 0.5, "reason": "tiered_tp_atr_live:live:1"}


def test_tiered_tp_atr_live_falls_back_to_entry_atr(registry):
    # Live ATR missing -> falls back to entry_atr (2.0); 104 mark = 2 ATR -> 1.0x and 2.0x both hit.
    fallback = registry.evaluate(
        "tiered_tp_atr_live",
        {"side": "long", "avg_cost": 100, "current_quantity": 1, "initial_quantity": 1, "entry_atr": 2},
        {"mark_price": 104},
        {},
    )
    assert fallback == {"close_fraction": 1.0, "reason": "tiered_tp_atr_live:entry_fallback:2"}


def test_tiered_tp_atr_live_zero_live_atr_falls_back(registry):
    # atr=0 should be treated as missing and fall back to entry_atr.
    result = registry.evaluate(
        "tiered_tp_atr_live",
        {"side": "long", "avg_cost": 100, "current_quantity": 1, "initial_quantity": 1, "entry_atr": 2},
        {"mark_price": 103, "atr": 0},
        {},
    )
    assert result["reason"].startswith("tiered_tp_atr_live:entry_fallback")


def test_tiered_tp_atr_live_missing_all_atr_noop(registry):
    result = registry.evaluate(
        "tiered_tp_atr_live",
        {"side": "long", "avg_cost": 100, "current_quantity": 1, "initial_quantity": 1},
        {"mark_price": 104},
        {},
    )
    assert result == {"close_fraction": 0.0, "reason": "noop:missing_atr"}


def test_tiered_tp_atr_live_entry_source_ignores_market_atr(registry):
    # atr_source=entry must use entry_atr even when market.atr is present.
    result = registry.evaluate(
        "tiered_tp_atr_live",
        {"side": "long", "avg_cost": 100, "current_quantity": 1, "initial_quantity": 1, "entry_atr": 2},
        {"mark_price": 104, "atr": 10},
        {"atr_source": "entry"},
    )
    assert result == {"close_fraction": 1.0, "reason": "tiered_tp_atr_live:entry:2"}


def test_tiered_tp_atr_live_short_side(registry):
    result = registry.evaluate(
        "tiered_tp_atr_live",
        {"side": "short", "avg_cost": 100, "current_quantity": 1, "initial_quantity": 1, "entry_atr": 5},
        {"mark_price": 96, "atr": 2},
        {},
    )
    # profit_distance = 4, atr=2 -> 2.0 atr_profit -> hits both tiers, full close.
    assert result == {"close_fraction": 1.0, "reason": "tiered_tp_atr_live:live:2"}


def test_market_atr_wiring_end_to_end(registry):
    """End-to-end: latest_atr(df) → market_ctx["atr"] → tiered_tp_atr_live evaluator.

    This mirrors the wiring in shared_scripts/check_*.py: the check script computes
    ATR from OHLCV via latest_atr(df) and stuffs the value into market_ctx["atr"]
    before calling evaluate_open_close. The evaluator must see the live value
    (reason starts with `live:`) rather than falling back to entry_atr.
    """
    import sys
    from pathlib import Path

    import pandas as pd

    sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "shared_tools"))
    from atr import latest_atr  # type: ignore

    # Build a 30-bar OHLCV frame with a stable ~$3 ATR.
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

    # Mark moves $4 above avg_cost; with live ATR=$3 that's 1.33 ATR profit
    # → hits 1.0x tier (50%). Reason must reflect `live` source, not `entry_fallback`.
    market_ctx["mark_price"] = 104  # mark moved up after market_ctx was built
    result = registry.evaluate(
        "tiered_tp_atr_live",
        {
            "side": "long",
            "avg_cost": 100,
            "current_quantity": 1,
            "initial_quantity": 1,
            "entry_atr": 99,  # garbage entry value to detect fallback
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
    """#841 v15: only tp_tiers is accepted after alias reads were dropped."""
    h = _load_helpers()
    tp = [{"atr_multiple": 2.0, "close_fraction": 1.0}]

    assert h.tier_list_from_params({"tp_tiers": tp}) == tp
    assert h.tier_list_from_params({"tiers": tp}) is None
    assert h.tier_list_from_params({"atr_source": "live"}) is None
    assert h.tier_list_from_params(None) is None


def test_evaluate_reads_tp_tiers(registry):
    """#841: tiered_tp_atr reads the canonical tp_tiers ladder."""
    position = {
        "avg_cost": 100.0,
        "current_quantity": 1.0,
        "initial_quantity": 1.0,
        "entry_atr": 10.0,
        "side": "long",
    }
    market = {"mark_price": 130.0}  # +3 ATR → clears a 2x tier
    ladder = [{"atr_multiple": 2.0, "close_fraction": 1.0}]

    hit = registry.evaluate("tiered_tp_atr", position, market, {"tp_tiers": ladder})
    assert hit["close_fraction"] == 1.0
    assert hit["reason"] == "tiered_tp_atr:2"


def test_unified_regime_block_evaluator(registry):
    """#841 2b: the regime evaluator resolves a unified per-regime block via
    select-then-scalar — each regime's own ladder drives close_fraction."""
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
    market = {"mark_price": 130.0}  # +3 ATR

    # trending_up: 3 ATR clears the 2x tier only → close 50%.
    up = registry.evaluate("tiered_tp_atr_regime", {**base_pos, "regime": "trending_up"}, market, params)
    assert up["close_fraction"] == pytest.approx(0.5), up

    # ranging: 3 ATR clears both 1x and 2x (final) → close 100%.
    rng = registry.evaluate("tiered_tp_atr_regime", {**base_pos, "regime": "ranging"}, market, params)
    assert rng["close_fraction"] == pytest.approx(1.0), rng

    # Missing regime → no close.
    none = registry.evaluate("tiered_tp_atr_regime", base_pos, market, params)
    assert none["close_fraction"] == 0.0
