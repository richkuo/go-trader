"""Trailing-stop ratchet close evaluator (#844).

A trailing ATR stop where each cleared TP tier (a) tightens the trailing ATR
multiple and (b) optionally scales out a fraction at that tier's price. A tier
with ``close_fraction == 0`` is a pure trail-tightener; the position's remainder
exits via the trailing stop once price reverses. The "pure trailing" strategy is
just the special case where every tier sets ``close_fraction: 0``.

Frozen-at-open: the tier table is resolved once from ``position["regime"]`` (the
label stamped on the Go-side Position at entry) and held for the life of the
trade — it does not re-resolve if the regime flips mid-position. Two registered
names share this evaluator:

  - ``trailing_tp_ratchet``        — ``tp_tiers`` is a plain list (regime ignored,
                                      or selected via a ``"default"`` key)
  - ``trailing_tp_ratchet_regime`` — ``tp_tiers`` is a ``{regime_label: [tiers]}`` map

Each tier is ``{atr_multiple, close_fraction, trailing_mult_after | tp_atr_fraction}``
where ``close_fraction`` is the *cumulative* target closed fraction at that tier
(same semantics as ``tiered_tp_atr``; the double-close guard converts it to the
incremental fraction-of-current-quantity). The new trail distance is either
``trailing_mult_after`` (absolute ATR multiple) or ``tp_atr_fraction`` (relative:
``fraction × tier.atr_multiple``) — mutually exclusive per tier.

Output adds ``post_tp_trailing_atr_mult``: the tightest trailing ATR multiple
among the cleared tiers. The Go runtime stamps it onto
``Position.PostTPTrailingATRMult`` (monotonically tightening only) and the
trailing-stop walker takes over at that distance. Unlike ``tiered_tp_atr``, the
final tier's ``close_fraction`` is NOT coerced to 1.0 — the remainder is meant
to ride the trail to exit.
"""

from __future__ import annotations

from typing import List, Optional, Tuple

from _helpers import (
    current_close_fraction,
    float_from,
    tier_list_from_params,
)


def _resolve_tier_list(raw, regime: str):
    """Select the concrete tier list for this position.

    ``raw`` is either a plain list (``trailing_tp_ratchet``) or a regime-keyed
    map (``trailing_tp_ratchet_regime``). For the map form, select the list under
    the position's regime label, falling back to a ``"default"`` key.
    """
    if isinstance(raw, dict):
        if regime and isinstance(raw.get(regime), list):
            return raw.get(regime)
        return raw.get("default")
    return raw


def _resolve_tier_trail(tier: dict, trigger: float) -> Optional[float]:
    """Resolve a tier's new trailing ATR multiple.

    ``trailing_mult_after`` (absolute) takes precedence over ``tp_atr_fraction``
    (relative to the firing tier). Returns None when the tier specifies neither
    (the trail is left unchanged at that tier) or the value is non-positive.
    """
    mult_after = tier.get("trailing_mult_after")
    if mult_after is not None:
        try:
            value = float(mult_after)
        except (TypeError, ValueError):
            return None
        return value if value > 0 else None
    tp_frac = tier.get("tp_atr_fraction")
    if tp_frac is not None and trigger > 0:
        try:
            frac = float(tp_frac)
        except (TypeError, ValueError):
            return None
        return frac * trigger if frac > 0 else None
    return None


def _parse_tiers(raw) -> List[Tuple[float, float, Optional[float]]]:
    """Parse a tier list into sorted ``(atr_multiple, close_fraction, trail)``
    tuples. Malformed tiers are skipped; the list is sorted by ascending
    ``atr_multiple``."""
    parsed: List[Tuple[float, float, Optional[float]]] = []
    for tier in raw or []:
        if not isinstance(tier, dict):
            continue
        trigger_raw = tier.get("atr_multiple", tier.get("atr", tier.get("multiple")))
        try:
            trigger = float(trigger_raw)
        except (TypeError, ValueError):
            continue
        if trigger < 0:
            continue
        frac_raw = tier.get("close_fraction", tier.get("fraction"))
        try:
            frac = float(frac_raw) if frac_raw is not None else 0.0
        except (TypeError, ValueError):
            frac = 0.0
        frac = min(max(frac, 0.0), 1.0)
        parsed.append((trigger, frac, _resolve_tier_trail(tier, trigger)))
    parsed.sort(key=lambda item: item[0])
    return parsed


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

    tiers = _parse_tiers(_resolve_tier_list(tier_list_from_params(params), regime))
    if not tiers:
        return {"close_fraction": 0.0, "reason": "noop:no_tiers"}

    # Profit distance in ATR units, using the frozen entry ATR (tier triggers
    # never re-scale with live ATR — matches tiered_tp_atr.py:51-53).
    profit_distance = mark_price - avg_cost if side == "long" else avg_cost - mark_price
    atr_profit = profit_distance / entry_atr
    cleared = [(m, f, t) for (m, f, t) in tiers if atr_profit >= m]
    if not cleared:
        return {"close_fraction": 0.0, "reason": "noop:not_hit"}

    multiple, cumulative_fraction, _ = cleared[-1]
    # Tightest trail among the cleared tiers — Go monotonic-guards the stamp so a
    # mis-ordered table (looser trail on a higher tier) can never loosen.
    trail_candidates = [t for (_, _, t) in cleared if t is not None and t > 0]
    new_trail = min(trail_candidates) if trail_candidates else None

    close_fraction = current_close_fraction(position, cumulative_fraction)
    result = {
        "close_fraction": close_fraction,
        "reason": f"trailing_tp_ratchet:{regime or 'default'}:{multiple:g}",
    }
    if new_trail is not None:
        result["post_tp_trailing_atr_mult"] = new_trail
    return result
