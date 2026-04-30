"""Tiered percentage take-profit close evaluator."""

from __future__ import annotations

from _helpers import clamp_fraction, current_close_fraction, float_from

DEFAULT_TIERS = (
    {"profit_pct": 0.03, "close_fraction": 0.5},
    {"profit_pct": 0.06, "close_fraction": 1.0},
)


def _tiers(raw) -> list[tuple[float, float]]:
    parsed = []
    for tier in raw or DEFAULT_TIERS:
        if isinstance(tier, dict):
            trigger = tier.get("profit_pct", tier.get("pct"))
            fraction = tier.get("close_fraction", tier.get("fraction"))
        else:
            try:
                trigger, fraction = tier
            except (TypeError, ValueError):
                continue
        try:
            trigger = max(float(trigger), 0.0)
        except (TypeError, ValueError):
            continue
        parsed.append((trigger, clamp_fraction(fraction)))
    return sorted(parsed, key=lambda item: item[0])


def evaluate(position: dict, market: dict, params: dict) -> dict:
    avg_cost = float_from(position, "avg_cost")
    current_quantity = float_from(position, "current_quantity")
    side = str(position.get("side", "") or "").strip().lower()
    mark_price = float_from(market, "mark_price")

    if mark_price <= 0:
        return {"close_fraction": 0.0, "reason": "noop:missing_mark_price"}
    if avg_cost <= 0 or current_quantity <= 0 or side not in ("long", "short"):
        return {"close_fraction": 0.0, "reason": "noop:missing_position"}

    pnl_pct = (mark_price - avg_cost) / avg_cost if side == "long" else (avg_cost - mark_price) / avg_cost
    hit_tiers = [(pct, fraction) for pct, fraction in _tiers(params.get("tiers")) if pnl_pct >= pct]
    if not hit_tiers:
        return {"close_fraction": 0.0, "reason": "noop:not_hit"}

    pct, cumulative_fraction = hit_tiers[-1]
    close_fraction = current_close_fraction(position, cumulative_fraction)
    if close_fraction <= 0:
        return {"close_fraction": 0.0, "reason": "noop:already_taken"}
    return {"close_fraction": close_fraction, "reason": f"tiered_tp_pct:{pct:g}"}
