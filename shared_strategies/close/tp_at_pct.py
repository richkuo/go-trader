"""Close a position when mark price reaches a profit percentage."""

from __future__ import annotations


def _float_from(mapping: dict, key: str) -> float:
    try:
        return float(mapping.get(key, 0) or 0)
    except (TypeError, ValueError):
        return 0.0


def evaluate(position: dict, market: dict, params: dict) -> dict:
    avg_cost = _float_from(position, "avg_cost")
    current_quantity = _float_from(position, "current_quantity")
    side = str(position.get("side", "") or "").strip().lower()
    mark_price = _float_from(market, "mark_price")

    if mark_price <= 0:
        return {"close_fraction": 0.0, "reason": "noop:missing_mark_price"}
    if avg_cost <= 0 or current_quantity <= 0 or side not in ("long", "short"):
        return {"close_fraction": 0.0, "reason": "noop:missing_position"}

    try:
        threshold = max(float(params.get("pct", 0.03)), 0.0)
    except (TypeError, ValueError):
        threshold = 0.0

    if side == "long":
        pnl_pct = (mark_price - avg_cost) / avg_cost
    else:
        pnl_pct = (avg_cost - mark_price) / avg_cost
    if pnl_pct >= threshold:
        return {"close_fraction": 1.0, "reason": "tp_at_pct:hit"}
    return {"close_fraction": 0.0, "reason": "noop:not_hit"}
