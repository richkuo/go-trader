"""Close setup-based positions at stop, breakeven, or R-multiple target."""

from __future__ import annotations

from _helpers import float_from


def _positive_param(params: dict, key: str, default: float) -> float:
    try:
        value = float(params.get(key, default))
    except (TypeError, ValueError):
        return default
    return value if value > 0 else default


def _reason(name: str, suffix: str) -> dict:
    return {"close_fraction": 1.0, "reason": f"{name}:{suffix}"}


def evaluate(position: dict, market: dict, params: dict) -> dict:
    """Evaluate a setup stop, target, and breakeven rule.

    The entry strategy must stamp ``setup_stop`` or ``three_candle_stop`` onto
    the position. The backtester also passes bar high/low and high/low-water
    fields so the breakeven condition can persist after price first reaches
    ``breakeven_r``.
    """
    avg_cost = float_from(position, "avg_cost")
    current_quantity = float_from(position, "current_quantity")
    side = str(position.get("side", "") or "").strip().lower()
    stop = float_from(position, "setup_stop") or float_from(position, "three_candle_stop")

    if avg_cost <= 0 or current_quantity <= 0 or side not in ("long", "short"):
        return {"close_fraction": 0.0, "reason": "noop:missing_position"}
    if stop <= 0:
        return {"close_fraction": 0.0, "reason": "noop:missing_setup_stop"}

    target_r = _positive_param(params, "target_r", 3.0)
    breakeven_r = _positive_param(params, "breakeven_r", 2.0)
    priority = str(params.get("same_bar_priority", "stop") or "stop").strip().lower()

    mark_price = float_from(market, "mark_price")
    bar_high = float_from(market, "bar_high") or mark_price
    bar_low = float_from(market, "bar_low") or mark_price
    high_water = float_from(position, "high_water") or max(mark_price, bar_high)
    low_water = float_from(position, "low_water") or min(mark_price, bar_low)

    if side == "long":
        risk = avg_cost - stop
        if risk <= 0:
            return {"close_fraction": 0.0, "reason": "noop:invalid_setup_stop"}
        target = avg_cost + target_r * risk
        breakeven_active = high_water >= avg_cost + breakeven_r * risk
        active_stop = max(stop, avg_cost) if breakeven_active else stop
        stop_hit = bar_low <= active_stop
        target_hit = bar_high >= target
    else:
        risk = stop - avg_cost
        if risk <= 0:
            return {"close_fraction": 0.0, "reason": "noop:invalid_setup_stop"}
        target = avg_cost - target_r * risk
        breakeven_active = low_water <= avg_cost - breakeven_r * risk
        active_stop = min(stop, avg_cost) if breakeven_active else stop
        stop_hit = bar_high >= active_stop
        target_hit = bar_low <= target

    if stop_hit and target_hit:
        if priority == "target":
            return _reason("setup_rr", "target")
        return _reason("setup_rr", "breakeven" if breakeven_active else "stop")
    if stop_hit:
        return _reason("setup_rr", "breakeven" if breakeven_active else "stop")
    if target_hit:
        return _reason("setup_rr", "target")
    return {"close_fraction": 0.0, "reason": "noop:not_hit"}
