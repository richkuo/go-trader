
from __future__ import annotations

from typing import List, Tuple

from _helpers import (
    clamp_fraction,
    current_close_fraction,
    float_from,
    tier_list_from_params,
)
from regime_atr import (
    REGIME_TP_TIER_GROUP_DEFAULTS,
    RegimeTierSpec,
    close_params_are_unified_regime,
    parse_regime_tp_tiers,
    regime_close_default_group,
    resolve_regime_tier,
    unified_regime_scalar_params,
)


def _resolve_tiers_for_regime(
    params: dict, regime: str
) -> Tuple[List[Tuple[float, float]], List[str]]:
    if close_params_are_unified_regime(params):
        scalar, _ = unified_regime_scalar_params(params, regime)
        if scalar is None:
            return [], []
        parsed: List[Tuple[float, float]] = []
        for t in scalar.get("tp_tiers", []) or []:
            if not isinstance(t, dict):
                continue
            try:
                mult = float(t.get("atr_multiple"))
                frac = float(t.get("close_fraction"))
            except (TypeError, ValueError):
                continue
            if mult <= 0 or frac <= 0:
                continue
            parsed.append((mult, max(min(frac, 1.0), 0.0)))
        parsed.sort(key=lambda p: p[0])
        if parsed:
            parsed[-1] = (parsed[-1][0], 1.0)
        return parsed, []

    use_defaults = bool(params.get("use_defaults"))
    raw_tiers = tier_list_from_params(params)
    if use_defaults and raw_tiers is None:
        group = regime_close_default_group(regime)
        ladder = REGIME_TP_TIER_GROUP_DEFAULTS.get(group) if group else None
        if not ladder:
            return [], []
        resolved = sorted(((m, clamp_fraction(f)) for m, f in ladder), key=lambda p: p[0])
        resolved[-1] = (resolved[-1][0], 1.0)
        return resolved, []
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
