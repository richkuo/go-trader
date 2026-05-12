"""Regime-aware tiered ATR take-profit (re-resolved per tick) — #733.

Multipliers AND ATR are re-resolved every cycle:
  - regime comes from ``market["regime"]`` (the live classifier output),
  - ATR comes from ``market["atr"]`` (with fallback to ``position["entry_atr"]``).

Virtual-only — HL on-chain reduce-only TP placement uses the frozen variant
(``tiered_tp_atr_regime``) because on-chain prices can't follow a moving
regime classification.
"""

from __future__ import annotations

from _helpers import clamp_fraction, current_close_fraction, float_from
from tiered_tp_atr_regime import _resolve_tiers_for_regime


def _resolve_atr(market: dict, position: dict, atr_source: str):
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

    # Live regime preferred; fall back to the frozen position regime so a
    # regime-classifier outage doesn't disarm the evaluator mid-position.
    regime = str(market.get("regime", "") or "").strip()
    if not regime:
        regime = str(position.get("regime", "") or "").strip()

    if mark_price <= 0:
        return {"close_fraction": 0.0, "reason": "noop:missing_mark_price"}
    if avg_cost <= 0 or current_quantity <= 0 or side not in ("long", "short"):
        return {"close_fraction": 0.0, "reason": "noop:missing_position"}
    if not regime:
        return {"close_fraction": 0.0, "reason": "noop:missing_regime"}

    atr_value, atr_label = _resolve_atr(market, position, atr_source)
    if atr_value <= 0:
        return {"close_fraction": 0.0, "reason": "noop:missing_atr"}

    tiers, errs = _resolve_tiers_for_regime(params, regime)
    if errs or not tiers:
        return {"close_fraction": 0.0, "reason": "noop:tier_resolution_failed"}

    profit_distance = mark_price - avg_cost if side == "long" else avg_cost - mark_price
    atr_profit = profit_distance / atr_value
    hit_tiers = [(m, f) for m, f in tiers if atr_profit >= m]
    if not hit_tiers:
        return {"close_fraction": 0.0, "reason": "noop:not_hit"}

    multiple, cumulative_fraction = hit_tiers[-1]
    close_fraction = current_close_fraction(position, clamp_fraction(cumulative_fraction))
    if close_fraction <= 0:
        return {"close_fraction": 0.0, "reason": "noop:already_taken"}
    return {
        "close_fraction": close_fraction,
        "reason": f"tiered_tp_atr_live_regime:{atr_label}:{regime}:{multiple:g}",
    }
