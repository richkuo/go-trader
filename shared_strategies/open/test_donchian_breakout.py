
import numpy as np
import pytest

from shared_strategies.open.conftest import load_module, make_ohlcv

_DONCHIAN = load_module("_donchian_breakout_test", __file__.replace("test_donchian_breakout.py", "donchian_breakout.py"))
donchian_breakout_core = _DONCHIAN.donchian_breakout_core


def test_breakout_above_channel_generates_buy():
    prices = [100] * 30 + list(np.linspace(100, 120, 20))
    df = make_ohlcv(prices)
    result = donchian_breakout_core(df)
    assert (result["signal"] == 1).any(), "Expected at least one buy signal on upward breakout"


