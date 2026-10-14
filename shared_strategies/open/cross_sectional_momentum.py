from typing import Any, Dict, Mapping

from basket_allocation import (
    LONG_TAIL_TOP,
    return_rank_preflight,
    return_rank_required_closes,
    trailing_return_rank_targets,
    validate_return_rank_params,
)

XSMOM_NAME = "cross_sectional_momentum"
XSMOM_DEFAULT_PARAMS: Dict[str, Any] = {
    "return_lookback_bars": 24,
    "volatility_lookback_bars": 24,
    "quantile": 0.25,
}


def xsmom_param_validator(params: Mapping[str, Any]) -> None:
    validate_return_rank_params(XSMOM_NAME, params)


def xsmom_required_closes(params: Mapping[str, Any]) -> int:
    return return_rank_required_closes(params)


def xsmom_basket_preflight(params: Mapping[str, Any], allocation_context: Mapping[str, Any]) -> None:
    return_rank_preflight(XSMOM_NAME, params, allocation_context)


def cross_sectional_momentum_core(frames: Mapping[str, Any], eligibility: Mapping[str, Any],
                                  allocation_context: Mapping[str, Any], return_lookback_bars: int = 24,
                                  volatility_lookback_bars: int = 24, quantile: float = 0.25) -> Dict[str, Any]:
    return trailing_return_rank_targets(XSMOM_NAME, frames, allocation_context, return_lookback_bars,
                                        volatility_lookback_bars, quantile, LONG_TAIL_TOP)
