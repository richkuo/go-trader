"""Trailing-TP ratchet close evaluator (#844).

A trailing ATR stop where each cleared TP tier (a) tightens the trailing-stop
ATR multiple and (b) optionally closes ``close_fraction`` of the position.
``close_fraction == 0`` (the default) is a *trail-only rung* — no close, it just
ratchets the trail tighter. The "pure trailing" strategy is the special case
where every tier sets ``close_fraction: 0``.

Responsibility split (see #844):

* This Python evaluator owns ONLY the per-tier partial-close fraction — used by
  the paper scheduler and the backtester. It returns the cumulative close
  fraction reached by the highest cleared tier, deduped against prior fills via
  ``current_close_fraction``.
* The **Go scheduler** owns the on-chain trailing stop. It independently
  resolves each cleared tier's tightened ATR multiple from entry-ATR profit
  distance and stamps ``Position.PostTPTrailingATRMult`` (monotonically), after
  which the existing trailing-stop walker cancel+replaces the on-chain SL. The
  **backtester** mirrors that trail in its ratchet step. So the
  ``trailing_mult_after`` / ``tp_atr_fraction`` per-tier fields are consumed by
  the Go/backtester trail machinery, NOT here.

Tier resolution is FROZEN at open: the regime form keys ``tp_tiers`` on the
position's open regime (``position["regime"]``); the plain form is a bare list.
Tier triggers use **entry ATR** (frozen at open) so Go, Python and the
backtester agree on which tier has cleared.

Per-tier shape: ``{atr_multiple, close_fraction?, trailing_mult_after | tp_atr_fraction}``.
``close_fraction`` is a cumulative target (fraction of the initial position to
have closed once that rung clears); ``0`` (the default) is a trail-only rung.
"""

from __future__ import annotations

from typing import List, Optional, Tuple

from _helpers import clamp_fraction, current_close_fraction, float_from


def ratchet_tiers_for_regime(params: dict, regime: str):
    """Return the raw tier list for ``regime`` (frozen at open), or ``None``.

    ``tp_tiers`` is either a bare list (plain ``trailing_tp_ratchet``) or a
    mapping of regime label -> list (``trailing_tp_ratchet_regime``). Returns
    ``None`` when no tier list resolves (e.g. the position's regime is absent
    from a regime-keyed table).
    """
    if not isinstance(params, dict):
        return None
    raw = params.get("tp_tiers")
    if isinstance(raw, dict):
        if not regime:
            return None
        tiers = raw.get(regime)
        return tiers if isinstance(tiers, list) else None
    if isinstance(raw, list):
        return raw
    return None


def parse_ratchet_tiers(raw) -> List[Tuple[float, float]]:
    """Parse ``[(atr_multiple, cumulative_close_fraction)]`` sorted ascending.

    Per-tier ``close_fraction`` defaults to 0.0 (a trail-only rung). The trail
    fields (``trailing_mult_after`` / ``tp_atr_fraction``) are ignored here —
    they drive the Go/backtester trailing stop, not the close fraction.
    """
    parsed: List[Tuple[float, float]] = []
    for tier in raw or ():
        if not isinstance(tier, dict):
            continue
        try:
            mult = float(tier.get("atr_multiple"))
        except (TypeError, ValueError):
            continue
        if mult <= 0:
            continue
        parsed.append((mult, clamp_fraction(tier.get("close_fraction", 0.0))))
    parsed.sort(key=lambda t: t[0])
    return parsed


def resolve_trail_tiers(raw):
    """Backtester helper: [(atr_multiple, abs_trail_mult)] sorted ascending.

    abs_trail_mult is ``trailing_mult_after`` (absolute) or
    ``tp_atr_fraction * atr_multiple`` (relative). Tiers without a positive
    resolved trail mult are dropped. Mirrors the Go resolvedTrailMult().
    """
    out = []
    for tier in raw or ():
        if not isinstance(tier, dict):
            continue
        try:
            mult = float(tier.get("atr_multiple"))
        except (TypeError, ValueError):
            continue
        if mult <= 0:
            continue
        trail = 0.0
        if tier.get("trailing_mult_after") is not None:
            try:
                trail = float(tier.get("trailing_mult_after"))
            except (TypeError, ValueError):
                trail = 0.0
        elif tier.get("tp_atr_fraction") is not None:
            try:
                trail = float(tier.get("tp_atr_fraction")) * mult
            except (TypeError, ValueError):
                trail = 0.0
        if trail <= 0:
            continue
        out.append((mult, trail))
    out.sort(key=lambda t: t[0])
    return out


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

    raw = ratchet_tiers_for_regime(params, regime)
    if raw is None:
        return {"close_fraction": 0.0, "reason": "noop:no_tiers_for_regime"}
    tiers = parse_ratchet_tiers(raw)
    if not tiers:
        return {"close_fraction": 0.0, "reason": "noop:no_tiers"}

    profit_distance = mark_price - avg_cost if side == "long" else avg_cost - mark_price
    atr_profit = profit_distance / entry_atr
    cleared = [(m, f) for m, f in tiers if atr_profit >= m]
    if not cleared:
        return {"close_fraction": 0.0, "reason": "noop:not_hit"}

    # Cumulative close target = the largest cumulative fraction among cleared
    # rungs. Trail-only rungs (close_fraction 0) contribute nothing, so a
    # trail-only rung sitting above a scale-out rung never un-closes the
    # position; the highest cleared multiple still drives the trail tightening.
    highest_mult = cleared[-1][0]
    target_fraction = max(f for _m, f in cleared)
    label = regime or "default"
    close_fraction = current_close_fraction(position, target_fraction)
    if close_fraction <= 0:
        # Trail-only rung (or fraction already taken): no partial close this
        # tick. The Go scheduler / backtester still ratchets the trail tighter.
        return {"close_fraction": 0.0, "reason": f"noop:trail_only:{label}:{highest_mult:g}"}
    return {
        "close_fraction": close_fraction,
        "reason": f"trailing_tp_ratchet:{label}:{highest_mult:g}",
    }
