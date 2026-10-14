from typing import Any, Dict, Mapping

from basket_allocation import (
    LONG_TAIL_BOTTOM,
    return_rank_preflight,
    trailing_return_rank_targets,
    validate_return_rank_params,
)

XSREV_NAME = "cross_sectional_reversal"
XSREV_DEFAULT_PARAMS: Dict[str, Any] = {
    "return_lookback_bars": 24,
    "volatility_lookback_bars": 24,
    "quantile": 0.25,
}


def xsrev_param_validator(params: Mapping[str, Any]) -> None:
    validate_return_rank_params(XSREV_NAME, params)


def xsrev_basket_preflight(params: Mapping[str, Any], allocation_context: Mapping[str, Any]) -> None:
    return_rank_preflight(XSREV_NAME, params, allocation_context)


def cross_sectional_reversal_core(frames: Mapping[str, Any], eligibility: Mapping[str, Any],
                                  allocation_context: Mapping[str, Any], return_lookback_bars: int = 24,
                                  volatility_lookback_bars: int = 24, quantile: float = 0.25) -> Dict[str, Any]:
    return trailing_return_rank_targets(XSREV_NAME, frames, allocation_context, return_lookback_bars,
                                        volatility_lookback_bars, quantile, LONG_TAIL_BOTTOM)
