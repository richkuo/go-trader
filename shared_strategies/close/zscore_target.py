
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
    if zscore != zscore:
        return {"close_fraction": 0.0, "reason": "noop:missing_zscore"}

    if side == "long":
        hit = zscore >= z_target
    else:
        hit = zscore <= -z_target
    if hit:
        return {"close_fraction": 1.0, "reason": f"zscore_target:{z_target:g}"}
    return {"close_fraction": 0.0, "reason": "noop:not_hit"}
