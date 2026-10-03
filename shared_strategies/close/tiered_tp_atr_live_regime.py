
from __future__ import annotations

from _helpers import (
    clamp_fraction,
    current_close_fraction,
    float_from,
    resolve_tp_tier_geometry,
    with_tier_fill_price,
)
from tiered_tp_atr_regime import regime_ladder_for


def evaluate_live_regime_tiers(position: dict, market: dict, params: dict, reason_name: str) -> dict:
    avg_cost = float_from(position, "avg_cost")
    current_quantity = float_from(position, "current_quantity")
    side = str(position.get("side", "") or "").strip().lower()
    mark_price = float_from(market, "mark_price")
    geometry = resolve_tp_tier_geometry(position, market, params, live_atr=True, regime_source="live")
    regime = geometry.regime

    if mark_price <= 0:
        return {"close_fraction": 0.0, "reason": "noop:missing_mark_price"}
    if avg_cost <= 0 or current_quantity <= 0 or side not in ("long", "short"):
        return {"close_fraction": 0.0, "reason": "noop:missing_position"}
    if not regime:
        return {"close_fraction": 0.0, "reason": "noop:missing_regime"}

    if geometry.atr <= 0:
        return {"close_fraction": 0.0, "reason": "noop:missing_atr"}

    ladder = regime_ladder_for(params, regime, geometry.resting_limit)
    if "tiers" not in ladder:
        return {"close_fraction": 0.0, "reason": ladder["reason"]}
    tiers = ladder["tiers"]

    anchor = geometry.anchor
    profit_distance = mark_price - anchor if side == "long" else anchor - mark_price
    atr_profit = profit_distance / geometry.atr
    hit_tiers = [(m, f) for m, f in tiers if atr_profit >= m]
    if not hit_tiers:
        return {"close_fraction": 0.0, "reason": "noop:not_hit"}

    multiple, cumulative_fraction = hit_tiers[-1]
    close_fraction = current_close_fraction(position, clamp_fraction(cumulative_fraction))
    if close_fraction <= 0:
        return {"close_fraction": 0.0, "reason": "noop:already_taken"}
    return with_tier_fill_price(
        {
            "close_fraction": close_fraction,
            "reason": (
                f"{reason_name}:atr={geometry.atr_label}:"
                f"regime={geometry.regime_label}:{regime}:{multiple:g}"
            ),
        },
        position, geometry, hit_tiers, side,
    )


def evaluate(position: dict, market: dict, params: dict) -> dict:
    return evaluate_live_regime_tiers(position, market, params, "tiered_tp_atr_live_regime")
