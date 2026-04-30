"""Tiered ATR-multiple take-profit close evaluator."""

from __future__ import annotations

DEFAULT_TIERS = (
    {"atr_multiple": 1.0, "close_fraction": 0.5},
    {"atr_multiple": 2.0, "close_fraction": 1.0},
)


def _float_from(mapping: dict, key: str) -> float:
    try:
        return float(mapping.get(key, 0) or 0)
    except (TypeError, ValueError):
        return 0.0


def _clamp_fraction(value) -> float:
    try:
        fraction = float(value)
    except (TypeError, ValueError):
        return 0.0
    return min(max(fraction, 0.0), 1.0)


def _tiers(raw) -> list[tuple[float, float]]:
    parsed = []
    for tier in raw or DEFAULT_TIERS:
        if isinstance(tier, dict):
            trigger = tier.get("atr_multiple", tier.get("multiple"))
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
        parsed.append((trigger, _clamp_fraction(fraction)))
    return sorted(parsed, key=lambda item: item[0])


def _current_close_fraction(position: dict, target_closed_fraction: float) -> float:
    current_qty = _float_from(position, "current_quantity")
    initial_qty = _float_from(position, "initial_quantity") or current_qty
    if current_qty <= 0 or initial_qty <= 0:
        return 0.0
    already_closed_qty = max(initial_qty - current_qty, 0.0)
    target_closed_qty = initial_qty * _clamp_fraction(target_closed_fraction)
    qty_to_close = min(max(target_closed_qty - already_closed_qty, 0.0), current_qty)
    if qty_to_close <= 0:
        return 0.0
    return _clamp_fraction(qty_to_close / current_qty)


def evaluate(position: dict, market: dict, params: dict) -> dict:
    avg_cost = _float_from(position, "avg_cost")
    current_quantity = _float_from(position, "current_quantity")
    entry_atr = _float_from(position, "entry_atr")
    side = str(position.get("side", "") or "").strip().lower()
    mark_price = _float_from(market, "mark_price")

    if mark_price <= 0:
        return {"close_fraction": 0.0, "reason": "noop:missing_mark_price"}
    if avg_cost <= 0 or current_quantity <= 0 or side not in ("long", "short"):
        return {"close_fraction": 0.0, "reason": "noop:missing_position"}
    if entry_atr <= 0:
        return {"close_fraction": 0.0, "reason": "noop:missing_entry_atr"}

    profit_distance = mark_price - avg_cost if side == "long" else avg_cost - mark_price
    atr_profit = profit_distance / entry_atr
    hit_tiers = [(multiple, fraction) for multiple, fraction in _tiers(params.get("tiers")) if atr_profit >= multiple]
    if not hit_tiers:
        return {"close_fraction": 0.0, "reason": "noop:not_hit"}

    multiple, cumulative_fraction = hit_tiers[-1]
    close_fraction = _current_close_fraction(position, cumulative_fraction)
    if close_fraction <= 0:
        return {"close_fraction": 0.0, "reason": "noop:already_taken"}
    return {"close_fraction": close_fraction, "reason": f"tiered_tp_atr:{multiple:g}"}
