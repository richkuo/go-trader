"""AVWAP loss-of-line close evaluator (#1196).

Exits the whole position when the mark breaches the anchored VWAP by
``buffer_atr_mult`` ATR units on the losing side (long: below the line,
short: above it). The line is the *live re-anchored* AVWAP — the same
``avwap`` column the AVWAP-family open strategies compute — injected into
``market["avwap"]`` by the composition layer (live) and the backtester, both
reading the open strategy's own result so entry and exit track one line.

Anchor semantics (#1196 spec-gate 1): live re-anchor, not frozen-at-entry.
A frozen anchor cannot be recomputed once the anchor bar ages out of the
bounded OHLCV fetch window, and would track a different line than the open
strategy's own view; re-anchoring keeps exit and entry on the same line.

``buffer_atr_mult == 0`` exits exactly at a line touch and needs no ATR.
``atr_source`` selects the buffer's ATR: ``live`` (the current bar's
``market["atr"]``, the default — the buffer scales with current volatility
like the re-anchored line itself) or ``entry`` (frozen ``Position.EntryATR``).
Missing avwap/ATR context fails safe (no-op) — the engine stop-loss still
protects the position.
"""

from __future__ import annotations

from _helpers import float_from


def evaluate(position: dict, market: dict, params: dict) -> dict:
    avg_cost = float_from(position, "avg_cost")
    current_quantity = float_from(position, "current_quantity")
    side = str(position.get("side", "") or "").strip().lower()
    if avg_cost <= 0 or current_quantity <= 0 or side not in ("long", "short"):
        return {"close_fraction": 0.0, "reason": "noop:missing_position"}
    mark_price = float_from(market, "mark_price")
    if mark_price <= 0:
        return {"close_fraction": 0.0, "reason": "noop:missing_mark_price"}
    avwap = float_from(market, "avwap")
    if avwap <= 0:
        return {"close_fraction": 0.0, "reason": "noop:missing_avwap"}

    try:
        buffer_atr_mult = float(params.get("buffer_atr_mult", 0.25) or 0.0)
    except (TypeError, ValueError):
        buffer_atr_mult = 0.25
    if buffer_atr_mult < 0:
        buffer_atr_mult = 0.0

    buffer = 0.0
    if buffer_atr_mult > 0:
        atr_source = str(params.get("atr_source", "live") or "live").strip().lower()
        if atr_source == "entry":
            atr_ref = float_from(position, "entry_atr")
            missing_reason = "noop:missing_entry_atr"
        else:
            atr_ref = float_from(market, "atr")
            missing_reason = "noop:missing_live_atr"
        if atr_ref <= 0:
            return {"close_fraction": 0.0, "reason": missing_reason}
        buffer = buffer_atr_mult * atr_ref

    if side == "long":
        hit = mark_price <= avwap - buffer
    else:
        hit = mark_price >= avwap + buffer
    if hit:
        return {
            "close_fraction": 1.0,
            "reason": f"avwap_stop:line_lost:{buffer_atr_mult:g}",
        }
    return {"close_fraction": 0.0, "reason": "noop:holding_line"}
