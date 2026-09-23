
from __future__ import annotations

import math
import sys
from collections import namedtuple

_deprecated_close_keys_warned: set[str] = set()

FLOAT_NOISE_RELATIVE_TOLERANCE = 1e-9


def warn_deprecated_close_key(old: str, canonical: str) -> None:
    token = f"{old}->{canonical}"
    if token in _deprecated_close_keys_warned:
        return
    _deprecated_close_keys_warned.add(token)
    print(
        f"[DEPRECATED] close config key {old!r} is deprecated; use {canonical!r} (#841)",
        file=sys.stderr,
    )


def tier_list_from_params(params: dict):
    if not isinstance(params, dict):
        return None
    return params.get("tp_tiers")


def float_from(mapping: dict, key: str) -> float:
    try:
        return float(mapping.get(key, 0) or 0)
    except (TypeError, ValueError):
        return 0.0


def clamp_fraction(value) -> float:
    try:
        fraction = float(value)
    except (TypeError, ValueError):
        return 0.0
    return min(max(fraction, 0.0), 1.0)


def current_close_fraction(position: dict, target_closed_fraction: float) -> float:
    current_qty = float_from(position, "current_quantity")
    initial_qty = float_from(position, "initial_quantity") or current_qty
    if current_qty <= 0 or initial_qty <= 0:
        return 0.0
    already_closed_qty = max(initial_qty - current_qty, 0.0)
    target_closed_qty = initial_qty * clamp_fraction(target_closed_fraction)
    qty_to_close = min(max(target_closed_qty - already_closed_qty, 0.0), current_qty)
    if qty_to_close <= initial_qty * FLOAT_NOISE_RELATIVE_TOLERANCE:
        return 0.0
    if qty_to_close >= current_qty * (1.0 - FLOAT_NOISE_RELATIVE_TOLERANCE):
        return 1.0
    return clamp_fraction(qty_to_close / current_qty)


TP_MODEL_RESTING_LIMIT = "resting_limit"


def resting_limit_model(position: dict) -> bool:
    if not isinstance(position, dict):
        return False
    return str(position.get("tp_model", "") or "").strip().lower() == TP_MODEL_RESTING_LIMIT


def geometry_anchor(position: dict) -> float:
    anchor = float_from(position, "risk_anchor_price")
    if anchor > 0:
        return anchor
    return float_from(position, "avg_cost")


def parse_resting_tp_tiers(raw) -> list[tuple[float, float]]:
    if not isinstance(raw, (list, tuple)):
        return []
    parsed = []
    for tier in raw:
        if not isinstance(tier, dict):
            continue
        try:
            multiple = float(tier.get("atr_multiple"))
            fraction = float(tier.get("close_fraction"))
        except (TypeError, ValueError):
            continue
        if not (math.isfinite(multiple) and math.isfinite(fraction)):
            continue
        if multiple <= 0 or fraction <= 0:
            continue
        parsed.append((multiple, min(fraction, 1.0)))
    parsed.sort(key=lambda item: item[0])
    return parsed


def finalize_tp_tiers(pairs):
    tiers = [(float(multiple), float(fraction)) for multiple, fraction in pairs]
    if len(tiers) < 2:
        return tiers
    previous = 0.0
    for multiple, fraction in tiers:
        if multiple <= 0 or fraction <= previous:
            return None
        previous = fraction
    tiers[-1] = (tiers[-1][0], 1.0)
    return tiers


TierGeometry = namedtuple(
    "TierGeometry",
    ("anchor", "atr", "atr_label", "regime", "regime_label", "resting_limit"),
)


def _normalized_atr_source(params: dict) -> str:
    source = str((params or {}).get("atr_source", "live") or "live").strip().lower()
    return source if source in ("live", "entry") else "live"


def _market_atr(market: dict) -> float:
    live_atr = float_from(market, "atr")
    if live_atr <= 0:
        live_atr = float_from(market, "live_atr")
    return live_atr


def _resolve_tier_atr(position: dict, market: dict, params: dict, live_atr: bool, resting: bool) -> tuple[float, str]:
    entry_atr = float_from(position, "entry_atr")
    if not live_atr:
        return entry_atr, "entry"
    atr_source = _normalized_atr_source(params)
    if resting:
        if entry_atr > 0:
            return entry_atr, "entry"
        if atr_source == "entry":
            return 0.0, "entry"
        market_atr = _market_atr(market)
        if market_atr > 0:
            return market_atr, "live"
        return 0.0, "missing"
    if atr_source == "entry":
        return entry_atr, "entry"
    market_atr = _market_atr(market)
    if market_atr > 0:
        return market_atr, "live"
    if entry_atr > 0:
        return entry_atr, "entry_fallback"
    return 0.0, "missing"


def _resolve_tier_regime(position: dict, market: dict, regime_source: str, resting: bool) -> tuple[str, str]:
    position_regime = str((position or {}).get("regime", "") or "").strip()
    if regime_source == "position":
        return position_regime, "frozen" if position_regime else ""
    if regime_source != "live":
        return "", ""
    market_regime = str((market or {}).get("regime", "") or "").strip()
    if resting:
        if position_regime:
            return position_regime, "frozen"
        return market_regime, "live" if market_regime else ""
    if market_regime:
        return market_regime, "live"
    return position_regime, "frozen" if position_regime else ""


def resolve_tp_tier_geometry(position: dict, market: dict, params: dict, *, live_atr: bool, regime_source: str = "") -> TierGeometry:
    resting = resting_limit_model(position)
    atr, atr_label = _resolve_tier_atr(position, market, params, live_atr, resting)
    regime, regime_label = _resolve_tier_regime(position, market, regime_source, resting)
    return TierGeometry(
        anchor=geometry_anchor(position),
        atr=atr,
        atr_label=atr_label,
        regime=regime,
        regime_label=regime_label,
        resting_limit=resting,
    )


def tier_price(side: str, anchor: float, atr: float, multiple: float) -> float:
    offset = multiple * atr
    return anchor - offset if side == "short" else anchor + offset


def resting_tier_fill_price(position: dict, hit_tiers, side: str, anchor: float, atr: float) -> float:
    if not hit_tiers or anchor <= 0 or atr <= 0:
        return 0.0
    current_qty = float_from(position, "current_quantity")
    initial_qty = float_from(position, "initial_quantity") or current_qty
    if current_qty <= 0 or initial_qty <= 0:
        return 0.0
    already_closed = max(initial_qty - current_qty, 0.0)
    weighted = 0.0
    filled = 0.0
    previous_target = 0.0
    for multiple, cumulative in hit_tiers:
        low = max(previous_target * initial_qty, already_closed)
        high = min(clamp_fraction(cumulative) * initial_qty, already_closed + current_qty)
        previous_target = clamp_fraction(cumulative)
        if high <= low:
            continue
        weighted += (high - low) * tier_price(side, anchor, atr, multiple)
        filled += high - low
    if filled <= 0:
        multiple = hit_tiers[-1][0]
        return tier_price(side, anchor, atr, multiple)
    return weighted / filled


def with_tier_fill_price(result: dict, position: dict, geometry: TierGeometry, hit_tiers, side: str) -> dict:
    if not geometry.resting_limit or result.get("close_fraction", 0.0) <= 0:
        return result
    price = resting_tier_fill_price(position, hit_tiers, side, geometry.anchor, geometry.atr)
    if price > 0 and math.isfinite(price):
        result["tier_fill_price"] = price
    return result
