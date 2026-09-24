
import numpy as np
import pandas as pd

from shared_strategies.open.conftest import load_module, make_ohlcv

_LIQUIDITY_SWEEPS = load_module("_liquidity_sweeps_test", __file__.replace("test_liquidity_sweeps.py", "liquidity_sweeps.py"))
_find_swing_lows = _LIQUIDITY_SWEEPS._find_swing_lows
liquidity_sweep_core = _LIQUIDITY_SWEEPS.liquidity_sweep_core


def _make_ohlcv(highs, lows, closes, opens=None):
    return make_ohlcv(
        closes,
        opens=closes if opens is None else opens,
        highs=highs,
        lows=lows,
    )


def _monotone_uptrend(n: int, start: float = 100.0, slope: float = 0.5) -> tuple:
    closes = start + slope * np.arange(n)
    highs = closes + 0.3
    lows = closes - 0.3
    return highs.astype(float), lows.astype(float), closes.astype(float)


class TestBasic:
    def test_bullish_sweep_after_confirmation(self):
        lookback = 3
        n = 40
        highs, lows, closes = _monotone_uptrend(n)
        lows[10] = 50.0
        closes[10] = 80.0
        lows[25] = 49.0
        closes[25] = 81.0

        df = _make_ohlcv(highs, lows, closes)
        out = liquidity_sweep_core(df, swing_lookback=lookback)
        assert (out["signal"].iloc[25:] == 1).any()


