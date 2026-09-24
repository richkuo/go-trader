
import numpy as np
import pytest

from shared_strategies.open.conftest import load_module, make_ohlcv

_ADX_TREND = load_module("_adx_trend_test", __file__.replace("test_adx_trend.py", "adx_trend.py"))
adx_trend_core = _ADX_TREND.adx_trend_core
_compute_adx_components = _ADX_TREND._compute_adx_components


def test_strong_uptrend_generates_buy():
    prices = list(np.linspace(150, 100, 50)) + list(np.linspace(100, 200, 100))
    df = make_ohlcv(prices, noise=1.0)
    result = adx_trend_core(df)
    assert (result["signal"] == 1).any(), "Expected at least one buy signal on DI crossover into uptrend"


