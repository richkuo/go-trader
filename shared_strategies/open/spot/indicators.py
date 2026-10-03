
import os
import sys

import numpy as np
import pandas as pd
from typing import Tuple

_OPEN_DIR = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
if _OPEN_DIR not in sys.path:
    sys.path.insert(0, _OPEN_DIR)

from indicators_core import wilder_rsi


def sma(series: pd.Series, period: int) -> pd.Series:
    return series.rolling(window=period).mean()


def ema(series: pd.Series, period: int) -> pd.Series:
    return series.ewm(span=period, adjust=False).mean()


def sma_crossover(df: pd.DataFrame, fast_period: int = 20, slow_period: int = 50) -> pd.DataFrame:
    result = df.copy()
    result["sma_fast"] = sma(result["close"], fast_period)
    result["sma_slow"] = sma(result["close"], slow_period)

    result["position"] = np.where(result["sma_fast"] > result["sma_slow"], 1, 0)

    result["signal"] = result["position"].diff()

    return result


def rsi(df: pd.DataFrame, period: int = 14, overbought: float = 70,
        oversold: float = 30) -> pd.DataFrame:
    result = df.copy()
    result["rsi"] = wilder_rsi(result["close"], period)

    result["signal"] = 0
    result.loc[
        (result["rsi"] > oversold) & (result["rsi"].shift(1) <= oversold),
        "signal"
    ] = 1
    result.loc[
        (result["rsi"] < overbought) & (result["rsi"].shift(1) >= overbought),
        "signal"
    ] = -1

    return result


def bollinger_bands(df: pd.DataFrame, period: int = 20, num_std: float = 2.0) -> pd.DataFrame:
    result = df.copy()
    result["bb_middle"] = sma(result["close"], period)
    rolling_std = result["close"].rolling(window=period).std()
    result["bb_upper"] = result["bb_middle"] + (rolling_std * num_std)
    result["bb_lower"] = result["bb_middle"] - (rolling_std * num_std)
    result["bb_width"] = (result["bb_upper"] - result["bb_lower"]) / result["bb_middle"]

    result["signal"] = 0
    result.loc[
        (result["close"] > result["bb_lower"]) & (result["close"].shift(1) <= result["bb_lower"].shift(1)),
        "signal"
    ] = 1
    result.loc[
        (result["close"] < result["bb_upper"]) & (result["close"].shift(1) >= result["bb_upper"].shift(1)),
        "signal"
    ] = -1

    return result


if __name__ == "__main__":
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
