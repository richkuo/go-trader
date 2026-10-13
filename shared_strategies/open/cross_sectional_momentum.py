import math
from typing import Any, Dict, Mapping

from basket_allocation import BasketAllocationHold, allocate_long_short

XSMOM_DEFAULT_PARAMS: Dict[str, Any] = {
    "return_lookback_bars": 24,
    "volatility_lookback_bars": 24,
    "quantile": 0.25,
}
XSMOM_MAX_LOOKBACK = 4999
XSMOM_PRICE_COLUMNS = ("open", "high", "low", "close")


def _strict_int(params: Mapping[str, Any], key: str, low: int, high: int) -> int:
    value = params.get(key)
    if isinstance(value, bool) or not isinstance(value, int):
        raise ValueError(f"cross_sectional_momentum: {key} must be an integer, got {value!r}")
    if not low <= value <= high:
        raise ValueError(f"cross_sectional_momentum: {key} must be in [{low}, {high}], got {value}")
    return value


def xsmom_param_validator(params: Mapping[str, Any]) -> None:
    _strict_int(params, "return_lookback_bars", 1, XSMOM_MAX_LOOKBACK)
    _strict_int(params, "volatility_lookback_bars", 2, XSMOM_MAX_LOOKBACK)
    q = params.get("quantile")
    if isinstance(q, bool) or not isinstance(q, (int, float)) or not math.isfinite(float(q)):
        raise ValueError(f"cross_sectional_momentum: quantile must be a finite number, got {q!r}")
    if not 0 < float(q) < 0.5:
        raise ValueError(f"cross_sectional_momentum: quantile must be above 0 and below 0.5, got {q!r}")


def xsmom_required_closes(params: Mapping[str, Any]) -> int:
    return max(int(params["return_lookback_bars"]), int(params["volatility_lookback_bars"])) + 1


def xsmom_basket_preflight(params: Mapping[str, Any], allocation_context: Mapping[str, Any]) -> None:
    xsmom_param_validator(params)
    need = xsmom_required_closes(params)
    lookback = allocation_context["lookback_bars"]
    if lookback < need:
        raise ValueError(
            f"lookback_bars {lookback} cannot supply return_lookback_bars {params['return_lookback_bars']} and "
            f"volatility_lookback_bars {params['volatility_lookback_bars']}: the ranker needs {need} closed bars")


def _member_signal(sym: str, frame, return_bars: int, vol_bars: int, need: int):
    prices = frame[list(XSMOM_PRICE_COLUMNS)].to_numpy(dtype=float)
    if len(prices) < need:
        raise BasketAllocationHold(
            f"{sym}: the frame carries {len(prices)} closed bars, cross_sectional_momentum needs {need}")
    for row in prices:
        for px in row:
            if not math.isfinite(px):
                raise BasketAllocationHold(f"{sym}: the frame carries a non-finite price")
            if px <= 0:
                raise BasketAllocationHold(f"{sym}: the frame carries a non-positive price")
    closes = [float(c) for c in prices[:, 3]]
    score = closes[-1] / closes[-1 - return_bars] - 1.0
    if not math.isfinite(score):
        raise BasketAllocationHold(f"{sym}: the trailing return is not finite")
    logs = [math.log(c) for c in closes[-vol_bars - 1:]]
    returns = [logs[i] - logs[i - 1] for i in range(1, len(logs))]
    mean = math.fsum(returns) / vol_bars
    variance = math.fsum((r - mean) ** 2 for r in returns) / vol_bars
    vol = math.sqrt(variance)
    if not math.isfinite(vol) or vol <= 0:
        raise BasketAllocationHold(f"{sym}: the trailing volatility {vol!r} is zero or not finite")
    return score, vol


def cross_sectional_momentum_core(frames: Mapping[str, Any], eligibility: Mapping[str, Any],
                                  allocation_context: Mapping[str, Any], return_lookback_bars: int = 24,
                                  volatility_lookback_bars: int = 24, quantile: float = 0.25) -> Dict[str, Any]:
    params = {"return_lookback_bars": return_lookback_bars, "volatility_lookback_bars": volatility_lookback_bars,
              "quantile": quantile}
    xsmom_param_validator(params)
    need = xsmom_required_closes(params)
    try:
        scores: Dict[str, float] = {}
        vols: Dict[str, float] = {}
        for sym in sorted(frames):
            scores[sym], vols[sym] = _member_signal(sym, frames[sym], return_lookback_bars,
                                                    volatility_lookback_bars, need)
        weights = allocate_long_short(scores, vols, quantile, allocation_context)
    except BasketAllocationHold as e:
        return {"hold": str(e)}
    return {"scores": scores, "weights": weights, "exclusions": {}}
