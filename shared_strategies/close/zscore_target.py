"""Z-score target close evaluator (#997 M3 — fade/stretch exit tuning).

Default-off mean-reversion exit: closes the whole position when price has
stretched to ``z_target`` standard deviations in the position's favour
(long → close at high z, short → close at low z). Targets fade-style entries
whose returns bleed when the snap-back exit is left to the open signal — the
classic "took profit too late on the reversion" giveback.

``lookback == 0`` (the registry default) is an explicit no-op. The rolling
z-score itself is computed upstream by the backtester (same closed-bar →
next-open-fill contract as ATR) and handed in via ``market["zscore"]``; the
``lookback`` param here only declares the window so the engine knows to compute
it. It is NOT supplied by the live check scripts yet — live wiring is deferred
(see #997); until then a live config referencing ``zscore_target`` fails safe
with ``noop:missing_zscore``.
"""

from __future__ import annotations

from _helpers import float_from


def evaluate(position: dict, market: dict, params: dict) -> dict:
    try:
        lookback = int(params.get("lookback", 0) or 0)
    except (TypeError, ValueError):
        lookback = 0
    try:
        z_target = float(params.get("z_target", 0.0) or 0.0)
    except (TypeError, ValueError):
        z_target = 0.0
    if lookback <= 0 or z_target <= 0:
        return {"close_fraction": 0.0, "reason": "noop:disabled"}

    current_quantity = float_from(position, "current_quantity")
    side = str(position.get("side", "") or "").strip().lower()
    if current_quantity <= 0 or side not in ("long", "short"):
        return {"close_fraction": 0.0, "reason": "noop:missing_position"}

    if "zscore" not in market or market.get("zscore") is None:
        return {"close_fraction": 0.0, "reason": "noop:missing_zscore"}
    try:
        zscore = float(market.get("zscore"))
    except (TypeError, ValueError):
        return {"close_fraction": 0.0, "reason": "noop:missing_zscore"}
    if zscore != zscore:  # NaN warmup bar
        return {"close_fraction": 0.0, "reason": "noop:missing_zscore"}

    if side == "long":
        hit = zscore >= z_target
    else:
        hit = zscore <= -z_target
    if hit:
        return {"close_fraction": 1.0, "reason": f"zscore_target:{z_target:g}"}
    return {"close_fraction": 0.0, "reason": "noop:not_hit"}
