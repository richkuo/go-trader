
from __future__ import annotations

from typing import List, Tuple

from _helpers import (
    clamp_fraction,
    current_close_fraction,
    finalize_tp_tiers,
    float_from,
    resolve_tp_tier_geometry,
    tier_list_from_params,
    tier_number,
    with_tier_fill_price,
)
from regime_atr import (
    CANONICAL_TREND_REGIME_LABELS,
    REGIME_TP_TIER_GROUP_DEFAULTS,
    RegimeTierSpec,
    close_params_are_unified_regime,
    parse_regime_tp_tiers,
    regime_close_default_group,
    resolve_regime_tier,
    unified_regime_scalar_params,
)


def _resolve_tier_pairs_for_regime(
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
        return parsed, []

    use_defaults = bool(params.get("use_defaults"))
    raw_tiers = tier_list_from_params(params)
    if use_defaults and raw_tiers is None:
        group = regime_close_default_group(regime)
        ladder = REGIME_TP_TIER_GROUP_DEFAULTS.get(group) if group else None
        if not ladder:
            return [], []
        return sorted(((m, clamp_fraction(f)) for m, f in ladder), key=lambda p: p[0]), []
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
    return resolved, []


def _resolve_tiers_for_regime(
    params: dict, regime: str
) -> Tuple[List[Tuple[float, float]], List[str]]:
    resolved, errs = _resolve_tier_pairs_for_regime(params, regime)
    if resolved:
        resolved[-1] = (resolved[-1][0], 1.0)
    return resolved, errs


def regime_ladder_for(params: dict, regime: str, resting: bool) -> dict:
    if not resting:
        tiers, errs = _resolve_tiers_for_regime(params, regime)
        if errs or not tiers:
            return {"reason": "noop:tier_resolution_failed"}
        return {"tiers": tiers}
    pairs, errs = _resolve_tier_pairs_for_regime(params, regime)
    if errs or not pairs:
        return {"reason": "noop:tier_resolution_failed"}
    if len(pairs) < 2:
        return {"tiers": [(pairs[0][0], 1.0)]}
    tiers = finalize_tp_tiers(pairs)
    if tiers is None:
        return {"reason": "noop:tier_ladder_rejected"}
    return {"tiers": tiers}


def evaluate(position: dict, market: dict, params: dict) -> dict:
    avg_cost = float_from(position, "avg_cost")
    current_quantity = float_from(position, "current_quantity")
    entry_atr = float_from(position, "entry_atr")
    side = str(position.get("side", "") or "").strip().lower()
    mark_price = float_from(market, "mark_price")
    geometry = resolve_tp_tier_geometry(position, market, params, live_atr=False, regime_source="position")
    regime = geometry.regime

    if mark_price <= 0:
        return {"close_fraction": 0.0, "reason": "noop:missing_mark_price"}
    if avg_cost <= 0 or current_quantity <= 0 or side not in ("long", "short"):
        return {"close_fraction": 0.0, "reason": "noop:missing_position"}
    if entry_atr <= 0:
        return {"close_fraction": 0.0, "reason": "noop:missing_entry_atr"}
    if not regime:
        return {"close_fraction": 0.0, "reason": "noop:missing_position_regime"}

    ladder = regime_ladder_for(params, regime, geometry.resting_limit)
    if "tiers" not in ladder:
        return {"close_fraction": 0.0, "reason": ladder["reason"]}
    tiers = ladder["tiers"]

    anchor = geometry.anchor
    profit_distance = mark_price - anchor if side == "long" else anchor - mark_price
    atr_profit = profit_distance / entry_atr
    hit_tiers = [(m, f) for m, f in tiers if atr_profit >= m]
    if not hit_tiers:
        return {"close_fraction": 0.0, "reason": "noop:not_hit"}

    multiple, cumulative_fraction = hit_tiers[-1]
    close_fraction = current_close_fraction(position, cumulative_fraction)
    if close_fraction <= 0:
        return {"close_fraction": 0.0, "reason": "noop:already_taken"}
    return with_tier_fill_price(
        {
            "close_fraction": close_fraction,
            "reason": f"tiered_tp_atr_regime:{regime}:{multiple:g}",
        },
        position, geometry, hit_tiers, side,
    )


def _strict_tier_pairs(raw, ctx_label: str):
    if not isinstance(raw, list):
        return None, [f"{ctx_label}: must be a list, got {type(raw).__name__}"]
    errs: List[str] = []
    pairs: List[Tuple[float, float]] = []
    for idx, tier in enumerate(raw):
        if not isinstance(tier, dict):
            errs.append(f"{ctx_label}[{idx}]: must be an object")
            continue
        mult = tier_number(tier.get("atr_multiple"))
        frac = tier_number(tier.get("close_fraction"))
        mult = 0.0 if mult is None else mult
        frac = 0.0 if frac is None else frac
        if not mult > 0:
            errs.append(f"{ctx_label}[{idx}].atr_multiple: must be > 0")
        if not (0 < frac <= 1):
            errs.append(f"{ctx_label}[{idx}].close_fraction: must be in (0, 1]")
        pairs.append((mult, frac))
    if errs:
        return None, errs
    return sorted(pairs, key=lambda p: p[0]), []


def _keyed_tier_type_errors(raw, ctx_label: str) -> List[str]:
    errs: List[str] = []
    if not isinstance(raw, list):
        return errs
    for idx, tier in enumerate(raw):
        if not isinstance(tier, dict):
            continue
        if "close_fraction" in tier and tier_number(tier["close_fraction"]) is None:
            errs.append(f"{ctx_label}[{idx}].close_fraction: must be a number")
        trend = tier.get("trend_regime")
        if not isinstance(trend, dict):
            continue
        for label in sorted(trend):
            entry = trend[label]
            if not isinstance(entry, dict):
                continue
            for key in ("atr_multiple", "close_fraction"):
                if key in entry and tier_number(entry[key]) is None:
                    errs.append(f"{ctx_label}[{idx}].trend_regime.{label}.{key}: must be a number")
    return errs


def _ladder_order_errors(pairs, ctx_label: str) -> List[str]:
    if len(pairs) < 2:
        return []
    errs: List[str] = []
    for idx in range(1, len(pairs)):
        if pairs[idx][0] < pairs[idx - 1][0]:
            errs.append(
                f"{ctx_label}: tier {idx} atr_multiple {pairs[idx][0]:g} is below tier "
                f"{idx - 1} atr_multiple {pairs[idx - 1][0]:g}"
            )
            continue
        if pairs[idx][1] <= pairs[idx - 1][1]:
            errs.append(
                f"{ctx_label}: tier {idx} close_fraction {pairs[idx][1]:g} must be greater than "
                f"tier {idx - 1} close_fraction {pairs[idx - 1][1]:g}"
            )
    return errs


def resting_ladder_errors(name: str, params: dict, labels=None) -> List[str]:
    labels = tuple(labels or CANONICAL_TREND_REGIME_LABELS)
    name = (name or "").strip().lower()
    params = params or {}
    ctx = f"close_strategy({name}).tp_tiers"
    if name in ("tiered_tp_atr", "tiered_tp_atr_live"):
        raw = tier_list_from_params(params)
        if raw is None:
            return []
        pairs, errs = _strict_tier_pairs(raw, ctx)
        return errs if errs else _ladder_order_errors(pairs, ctx)
    if name not in ("tiered_tp_atr_regime", "tiered_tp_atr_live_regime", "tiered_tp_atr_live_regime_dynamic"):
        return []
    if close_params_are_unified_regime(params):
        trend = params.get("trend_regime")
        errs: List[str] = []
        for label in sorted(trend) if isinstance(trend, dict) else []:
            block = trend.get(label)
            if not isinstance(block, dict) or "tp_tiers" not in block:
                continue
            label_ctx = f"close_strategy({name}).trend_regime.{label}.tp_tiers"
            pairs, parse_errs = _strict_tier_pairs(block["tp_tiers"], label_ctx)
            errs.extend(parse_errs or _ladder_order_errors(pairs, label_ctx))
        return errs
    raw = tier_list_from_params(params)
    if raw is None:
        return []
    type_errs = _keyed_tier_type_errors(raw, ctx)
    if type_errs:
        return type_errs
    specs, parse_errs = parse_regime_tp_tiers(raw, ctx, False, labels)
    if parse_errs:
        return []
    errs = []
    for label in labels:
        pairs = []
        for spec in specs:
            pair = resolve_regime_tier(spec, label)
            if pair is None:
                pairs = []
                break
            pairs.append(pair)
        errs.extend(_ladder_order_errors(pairs, f"{ctx}[regime {label}]"))
    return errs
