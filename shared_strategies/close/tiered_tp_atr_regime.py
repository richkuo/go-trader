"""Regime-aware tiered ATR take-profit (frozen at open) — #733.

Multipliers are resolved once at position open via ``position["regime"]``
(the regime stamped on the Go-side Position) and frozen for the lifetime
of the position. Compatible with HL on-chain reduce-only TP placement
because the tier prices are determined when the order is armed.

For per-bar re-resolution see :mod:`tiered_tp_atr_live_regime`.
"""

from __future__ import annotations

from typing import List, Tuple

from _helpers import (
    clamp_fraction,
    current_close_fraction,
    float_from,
    tier_list_from_params,
)
from regime_atr import (
    RegimeTierSpec,
    parse_regime_tp_tiers,
    resolve_regime_tier,
)


def _resolve_tiers_for_regime(
    params: dict, regime: str
) -> Tuple[List[Tuple[float, float]], List[str]]:
    """Walk the configured tier specs and return concrete
    [(atr_multiple, cumulative_close_fraction)] for the given regime label.

    Returns (tiers, errors). Errors are returned as strings so the caller
    can surface them — the live runtime should never see errors here (the
    Go config loader validates at startup), but tests and the backtester
    rely on this helper to mirror parser semantics.
    """
    use_defaults = bool(params.get("use_defaults"))
    raw_tiers = tier_list_from_params(params)
    specs, errs = parse_regime_tp_tiers(raw_tiers, "tiered_tp_atr_regime", use_defaults)
    if errs:
        return [], errs

    resolved: List[Tuple[float, float]] = []
    for idx, spec in enumerate(specs):
        pair = resolve_regime_tier(spec, regime)
        if pair is None:
            return [], [
                f"tiered_tp_atr_regime.tiers[{idx}]: regime {regime!r} resolved "
                "to no atr/close_fraction (config validation should have caught this)"
            ]
        atr, frac = pair
        resolved.append((atr, clamp_fraction(frac)))

    resolved.sort(key=lambda p: p[0])
    if resolved:
        # Final tier always 1.0 — matches live strategyTPTiers contract.
        atr, _ = resolved[-1]
        resolved[-1] = (atr, 1.0)
    return resolved, []


def evaluate(position: dict, market: dict, params: dict) -> dict:
    avg_cost = float_from(position, "avg_cost")
    current_quantity = float_from(position, "current_quantity")
    entry_atr = float_from(position, "entry_atr")
    side = str(position.get("side", "") or "").strip().lower()
    regime = str(position.get("regime", "") or "").strip()
    mark_price = float_from(market, "mark_price")

    if mark_price <= 0:
        return {"close_fraction": 0.0, "reason": "noop:missing_mark_price"}
    if avg_cost <= 0 or current_quantity <= 0 or side not in ("long", "short"):
        return {"close_fraction": 0.0, "reason": "noop:missing_position"}
    if entry_atr <= 0:
        return {"close_fraction": 0.0, "reason": "noop:missing_entry_atr"}
    if not regime:
        return {"close_fraction": 0.0, "reason": "noop:missing_position_regime"}

    tiers, errs = _resolve_tiers_for_regime(params, regime)
    if errs or not tiers:
        return {"close_fraction": 0.0, "reason": "noop:tier_resolution_failed"}

    profit_distance = mark_price - avg_cost if side == "long" else avg_cost - mark_price
    atr_profit = profit_distance / entry_atr
    hit_tiers = [(m, f) for m, f in tiers if atr_profit >= m]
    if not hit_tiers:
        return {"close_fraction": 0.0, "reason": "noop:not_hit"}

    multiple, cumulative_fraction = hit_tiers[-1]
    close_fraction = current_close_fraction(position, cumulative_fraction)
    if close_fraction <= 0:
        return {"close_fraction": 0.0, "reason": "noop:already_taken"}
    return {
        "close_fraction": close_fraction,
        "reason": f"tiered_tp_atr_regime:{regime}:{multiple:g}",
    }
