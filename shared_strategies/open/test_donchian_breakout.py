
import numpy as np
import pandas as pd
import pytest

from donchian_breakout import donchian_breakout_core


def make_ohlcv(closes, volume=None, noise=0.5):
    closes = np.array(closes, dtype=float)
    n = len(closes)
    if volume is None:
        volume = np.full(n, 100.0)
    highs = closes + noise
    lows = closes - noise
    opens = closes - noise * 0.3
    return pd.DataFrame({
        "open": opens,
        "high": highs,
        "low": lows,
        "close": closes,
        "volume": np.array(volume, dtype=float),
    })


def test_breakout_above_channel_generates_buy():
    prices = [100] * 30 + list(np.linspace(100, 120, 20))
    df = make_ohlcv(prices)
    result = donchian_breakout_core(df)
    assert (result["signal"] == 1).any(), "Expected at least one buy signal on upward breakout"


def test_breakdown_below_channel_generates_sell():
    prices = [100] * 30 + list(np.linspace(100, 80, 20))
    df = make_ohlcv(prices)
    result = donchian_breakout_core(df)
    assert (result["signal"] == -1).any(), "Expected at least one sell signal on downward breakdown"


def test_flat_market_no_signals():
    prices = [100.0] * 50
    df = make_ohlcv(prices, noise=0)
    result = donchian_breakout_core(df)
    assert (result["signal"] == 0).all(), "Flat market should produce no breakout signals"


def test_short_data_no_crash():
    df = make_ohlcv([100, 101, 102, 103, 104])
    result = donchian_breakout_core(df)
    assert "signal" in result.columns
    assert len(result) == 5
    assert (result["signal"] == 0).all(), "Short data should produce no signals"


def test_signal_values_valid():
    prices = list(np.linspace(80, 120, 30)) + list(np.linspace(120, 80, 30))
    df = make_ohlcv(prices)
    result = donchian_breakout_core(df)
    assert set(result["signal"].unique()).issubset({-1, 0, 1})


def test_no_lookahead_bias():
    prices = [100] * 25 + [110]
    df = make_ohlcv(prices)
    result = donchian_breakout_core(df)
    assert result["signal"].iloc[25] == 1, (
        "Breakout candle should fire buy signal (channel based on prior bars)"
    )
    assert (result["signal"].iloc[:25] == 0).all(), (
        "No signals expected in the flat region before breakout"
    )


def test_channels_exposed():
    df = make_ohlcv([100] * 30)
    result = donchian_breakout_core(df)
    assert "donchian_upper" in result.columns
    assert "donchian_lower" in result.columns
    assert "donchian_exit_upper" in result.columns
    assert "donchian_exit_lower" in result.columns
    assert (result["signal"] == 0).all(), "Flat data should produce no breakout signals"
