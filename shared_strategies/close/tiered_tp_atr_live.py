
from __future__ import annotations

from _helpers import (
    current_close_fraction,
    float_from,
    resolve_tp_tier_geometry,
    with_tier_fill_price,
)
from tiered_tp_atr import ladder_for


def evaluate(position: dict, market: dict, params: dict) -> dict:
    avg_cost = float_from(position, "avg_cost")
    current_quantity = float_from(position, "current_quantity")
    side = str(position.get("side", "") or "").strip().lower()
    mark_price = float_from(market, "mark_price")

    if mark_price <= 0:
        return {"close_fraction": 0.0, "reason": "noop:missing_mark_price"}
    if avg_cost <= 0 or current_quantity <= 0 or side not in ("long", "short"):
        return {"close_fraction": 0.0, "reason": "noop:missing_position"}

    geometry = resolve_tp_tier_geometry(position, market, params, live_atr=True)
    if geometry.atr <= 0:
        return {"close_fraction": 0.0, "reason": "noop:missing_atr"}

    tiers = ladder_for(params, geometry.resting_limit)
    if tiers is None:
        return {"close_fraction": 0.0, "reason": "noop:tier_ladder_rejected"}

    anchor = geometry.anchor
    profit_distance = mark_price - anchor if side == "long" else anchor - mark_price
    atr_profit = profit_distance / geometry.atr
    hit_tiers = [
        (multiple, fraction)
        for multiple, fraction in tiers
        if atr_profit >= multiple
    ]
    if not hit_tiers:
        return {"close_fraction": 0.0, "reason": "noop:not_hit"}

    multiple, cumulative_fraction = hit_tiers[-1]
    close_fraction = current_close_fraction(position, cumulative_fraction)
    if close_fraction <= 0:
        return {"close_fraction": 0.0, "reason": "noop:already_taken"}
    return with_tier_fill_price(
        {
            "close_fraction": close_fraction,
            "reason": f"tiered_tp_atr_live:{geometry.atr_label}:{multiple:g}",
        },
        position, geometry, hit_tiers, side,
    )
