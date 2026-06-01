"""Shared helpers for position-aware close evaluators."""

from __future__ import annotations

import sys

_deprecated_close_keys_warned: set[str] = set()


def warn_deprecated_close_key(old: str, canonical: str) -> None:
    """Emit a one-shot stderr deprecation notice for a legacy close-config key (#841)."""
    token = f"{old}->{canonical}"
    if token in _deprecated_close_keys_warned:
        return
    _deprecated_close_keys_warned.add(token)
    print(
        f"[DEPRECATED] close config key {old!r} is deprecated; use {canonical!r} (#841)",
        file=sys.stderr,
    )


def tier_list_from_params(params: dict):
    """Return the take-profit tier list from close params, preferring the
    canonical ``tp_tiers`` key over the deprecated ``tiers`` alias (#841)."""
    if not isinstance(params, dict):
        return None
    if "tp_tiers" in params:
        return params.get("tp_tiers")
    if "tiers" in params:
        warn_deprecated_close_key("tiers", "tp_tiers")
        return params.get("tiers")
    return None


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
    if qty_to_close <= 0:
        return 0.0
    return clamp_fraction(qty_to_close / current_qty)
