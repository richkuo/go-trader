
from __future__ import annotations

from tiered_tp_atr_live_regime import evaluate_live_regime_tiers


def evaluate(position: dict, market: dict, params: dict) -> dict:
    return evaluate_live_regime_tiers(position, market, params, "tiered_tp_atr_live_regime_dynamic")
