"""Tiered ATR-multiple take-profit close evaluator using live ATR per tick."""

from __future__ import annotations

from _helpers import current_close_fraction, float_from
from tiered_tp_atr import _tiers


def _resolve_atr(market: dict, position: dict, atr_source: str) -> tuple[float, str]:
    """Return (atr, source_label). Falls back to entry_atr when live ATR is unusable."""
    entry_atr = float_from(position, "entry_atr")
    if atr_source == "entry":
        return entry_atr, "entry"

    live_atr = float_from(market, "atr")
    if live_atr <= 0:
        live_atr = float_from(market, "live_atr")
    if live_atr > 0:
        return live_atr, "live"
    if entry_atr > 0:
        return entry_atr, "entry_fallback"
    return 0.0, "missing"


def evaluate(position: dict, market: dict, params: dict) -> dict:
    avg_cost = float_from(position, "avg_cost")
    current_quantity = float_from(position, "current_quantity")
    side = str(position.get("side", "") or "").strip().lower()
    mark_price = float_from(market, "mark_price")
    atr_source = str(params.get("atr_source", "live") or "live").strip().lower()
    if atr_source not in ("live", "entry"):
        atr_source = "live"

    if mark_price <= 0:
        return {"close_fraction": 0.0, "reason": "noop:missing_mark_price"}
    if avg_cost <= 0 or current_quantity <= 0 or side not in ("long", "short"):
        return {"close_fraction": 0.0, "reason": "noop:missing_position"}

    atr_value, atr_label = _resolve_atr(market, position, atr_source)
    if atr_value <= 0:
        return {"close_fraction": 0.0, "reason": "noop:missing_atr"}

    profit_distance = mark_price - avg_cost if side == "long" else avg_cost - mark_price
    atr_profit = profit_distance / atr_value
    hit_tiers = [
        (multiple, fraction)
        for multiple, fraction in _tiers(params.get("tiers"))
        if atr_profit >= multiple
    ]
    if not hit_tiers:
        return {"close_fraction": 0.0, "reason": "noop:not_hit"}

    multiple, cumulative_fraction = hit_tiers[-1]
    close_fraction = current_close_fraction(position, cumulative_fraction)
    if close_fraction <= 0:
        return {"close_fraction": 0.0, "reason": "noop:already_taken"}
    return {
        "close_fraction": close_fraction,
        "reason": f"tiered_tp_atr_live:{atr_label}:{multiple:g}",
    }
