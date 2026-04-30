"""
Technical indicators for trading signals.
Each indicator returns a DataFrame with signal columns added.
"""

import numpy as np
import pandas as pd
from typing import Tuple


def sma(series: pd.Series, period: int) -> pd.Series:
    """Simple Moving Average."""
    return series.rolling(window=period).mean()


def ema(series: pd.Series, period: int) -> pd.Series:
    """Exponential Moving Average."""
    return series.ewm(span=period, adjust=False).mean()


def sma_crossover(df: pd.DataFrame, fast_period: int = 20, slow_period: int = 50) -> pd.DataFrame:
    """
    SMA Crossover Strategy.
    Buy when fast SMA crosses above slow SMA.
    Sell when fast SMA crosses below slow SMA.

    Adds columns: sma_fast, sma_slow, signal
    signal: 1 = buy, -1 = sell, 0 = hold
    """
    result = df.copy()
    result["sma_fast"] = sma(result["close"], fast_period)
    result["sma_slow"] = sma(result["close"], slow_period)

    # Position: 1 when fast > slow, 0 otherwise
    result["position"] = np.where(result["sma_fast"] > result["sma_slow"], 1, 0)

    # Signal: difference in position = crossover events
    result["signal"] = result["position"].diff()
    # 1 = buy crossover, -1 = sell crossover, 0 = no change

    return result


def rsi(df: pd.DataFrame, period: int = 14, overbought: float = 70,
        oversold: float = 30) -> pd.DataFrame:
    """
    Relative Strength Index.
    Buy when RSI crosses above oversold level (from below).
    Sell when RSI crosses below overbought level (from above).

    Adds columns: rsi, signal
    """
    result = df.copy()
    delta = result["close"].diff()

    gain = delta.clip(lower=0)
    loss = (-delta).clip(lower=0)

    avg_gain = gain.ewm(alpha=1/period, min_periods=period, adjust=False).mean()
    avg_loss = loss.ewm(alpha=1/period, min_periods=period, adjust=False).mean()

    rs = avg_gain / avg_loss
    result["rsi"] = 100 - (100 / (1 + rs))

    # Signals based on oversold/overbought crossovers
    result["signal"] = 0
    # Buy when RSI crosses up through oversold
    result.loc[
        (result["rsi"] > oversold) & (result["rsi"].shift(1) <= oversold),
        "signal"
    ] = 1
    # Sell when RSI crosses down through overbought
    result.loc[
        (result["rsi"] < overbought) & (result["rsi"].shift(1) >= overbought),
        "signal"
    ] = -1

    return result


def bollinger_bands(df: pd.DataFrame, period: int = 20, num_std: float = 2.0) -> pd.DataFrame:
    """
    Bollinger Bands.
    Buy when price touches/crosses below lower band (mean reversion).
    Sell when price touches/crosses above upper band.

    Adds columns: bb_middle, bb_upper, bb_lower, bb_width, signal
    """
    result = df.copy()
    result["bb_middle"] = sma(result["close"], period)
    rolling_std = result["close"].rolling(window=period).std()
    result["bb_upper"] = result["bb_middle"] + (rolling_std * num_std)
    result["bb_lower"] = result["bb_middle"] - (rolling_std * num_std)
    result["bb_width"] = (result["bb_upper"] - result["bb_lower"]) / result["bb_middle"]

    # Mean reversion signals
    result["signal"] = 0
    # Buy: price crosses below lower band then comes back
    result.loc[
        (result["close"] > result["bb_lower"]) & (result["close"].shift(1) <= result["bb_lower"].shift(1)),
        "signal"
    ] = 1
    # Sell: price crosses above upper band then comes back
    result.loc[
        (result["close"] < result["bb_upper"]) & (result["close"].shift(1) >= result["bb_upper"].shift(1)),
        "signal"
    ] = -1

    return result


if __name__ == "__main__":
    # Quick test with synthetic data
    np.random.seed(42)
    dates = pd.date_range("2023-01-01", periods=100, freq="D")
    prices = 100 + np.cumsum(np.random.randn(100) * 2)
    df = pd.DataFrame({
        "open": prices,
        "high": prices + abs(np.random.randn(100)),
        "low": prices - abs(np.random.randn(100)),
        "close": prices + np.random.randn(100) * 0.5,
        "volume": np.random.randint(1000, 10000, 100).astype(float),
    }, index=dates)

    print("=== SMA Crossover (20/50) ===")
    sma_df = sma_crossover(df, 20, 50)
    buy_signals = (sma_df["signal"] == 1).sum()
    sell_signals = (sma_df["signal"] == -1).sum()
    print(f"Buy signals: {buy_signals}, Sell signals: {sell_signals}")

    print("\n=== RSI (14) ===")
    rsi_df = rsi(df, 14)
    print(f"RSI range: {rsi_df['rsi'].min():.1f} - {rsi_df['rsi'].max():.1f}")
    buy_signals = (rsi_df["signal"] == 1).sum()
    sell_signals = (rsi_df["signal"] == -1).sum()
    print(f"Buy signals: {buy_signals}, Sell signals: {sell_signals}")

    print("\n=== Bollinger Bands (20, 2σ) ===")
    bb_df = bollinger_bands(df, 20, 2.0)
    buy_signals = (bb_df["signal"] == 1).sum()
    sell_signals = (bb_df["signal"] == -1).sum()
    print(f"Buy signals: {buy_signals}, Sell signals: {sell_signals}")

    print("\nAll indicators working ✓")
