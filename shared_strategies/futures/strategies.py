"""
Futures strategy engine — strategies optimized for CME futures trading.
Reuses indicator logic from shared_tools and spot strategies where applicable.
Each strategy takes a DataFrame with OHLCV data and returns it with a 'signal' column.
signal: 1 = buy, -1 = sell, 0 = hold
"""

import sys
import os
import json
import numpy as np
import pandas as pd
from typing import Dict, List, Optional

# Add shared_strategies/spot/ to path for indicator functions (sma, ema)
sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'spot'))
# Add shared_strategies/ for amd_ifvg
sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..'))

from indicators import sma, ema
from amd_ifvg import amd_ifvg_core

# ─────────────────────────────────────────────
# Strategy registry
# ─────────────────────────────────────────────

STRATEGY_REGISTRY: Dict[str, dict] = {}


def register_strategy(name: str, description: str, default_params: dict):
    def decorator(fn):
        STRATEGY_REGISTRY[name] = {
            "fn": fn,
            "description": description,
            "default_params": default_params,
        }
        return fn
    return decorator


def get_strategy(name: str) -> dict:
    if name not in STRATEGY_REGISTRY:
        raise ValueError(f"Unknown strategy: {name}. Available: {list(STRATEGY_REGISTRY.keys())}")
    return STRATEGY_REGISTRY[name]


def list_strategies() -> List[str]:
    return list(STRATEGY_REGISTRY.keys())


def apply_strategy(name: str, df: pd.DataFrame, params: Optional[dict] = None) -> pd.DataFrame:
    strat = get_strategy(name)
    p = {**strat["default_params"], **(params or {})}
    return strat["fn"](df, **p)


# ─────────────────────────────────────────────
# Strategy implementations
# ─────────────────────────────────────────────

@register_strategy(
    "momentum",
    "Momentum — trend following on futures using rate of change",
    {"roc_period": 14, "threshold": 3.0}
)
def momentum_strategy(df: pd.DataFrame, roc_period: int = 14, threshold: float = 3.0) -> pd.DataFrame:
    result = df.copy()
    result["roc"] = ((result["close"] - result["close"].shift(roc_period)) / result["close"].shift(roc_period)) * 100
    result["signal"] = 0
    result.loc[(result["roc"] > threshold) & (result["roc"].shift(1) <= threshold), "signal"] = 1
    result.loc[(result["roc"] < -threshold) & (result["roc"].shift(1) >= -threshold), "signal"] = -1
    return result


@register_strategy(
    "mean_reversion",
    "Mean Reversion — range-bound index futures trading",
    {"lookback": 30, "entry_std": 1.5, "exit_std": 0.5}
)
def mean_reversion_strategy(df: pd.DataFrame, lookback: int = 30, entry_std: float = 1.5, exit_std: float = 0.5) -> pd.DataFrame:
    result = df.copy()
    result["rolling_mean"] = result["close"].rolling(window=lookback).mean()
    result["rolling_std"] = result["close"].rolling(window=lookback).std()
    result["z_score"] = (result["close"] - result["rolling_mean"]) / result["rolling_std"]
    result["signal"] = 0
    result.loc[(result["z_score"] > -entry_std) & (result["z_score"].shift(1) <= -entry_std), "signal"] = 1
    result.loc[(result["z_score"] < exit_std) & (result["z_score"].shift(1) >= exit_std), "signal"] = -1
    return result


@register_strategy(
    "rsi",
    "RSI — overbought/oversold signals for futures",
    {"period": 14, "overbought": 70, "oversold": 30}
)
def rsi_strategy(df: pd.DataFrame, period: int = 14, overbought: float = 70, oversold: float = 30) -> pd.DataFrame:
    result = df.copy()
    delta = result["close"].diff()
    gain = delta.clip(lower=0)
    loss = (-delta).clip(lower=0)
    avg_gain = gain.ewm(alpha=1/period, min_periods=period, adjust=False).mean()
    avg_loss = loss.ewm(alpha=1/period, min_periods=period, adjust=False).mean()
    rs = avg_gain / avg_loss
    result["rsi"] = 100 - (100 / (1 + rs))
    result["signal"] = 0
    result.loc[(result["rsi"] > oversold) & (result["rsi"].shift(1) <= oversold), "signal"] = 1
    result.loc[(result["rsi"] < overbought) & (result["rsi"].shift(1) >= overbought), "signal"] = -1
    return result


@register_strategy(
    "macd",
    "MACD — momentum crossover for futures",
    {"fast_period": 12, "slow_period": 26, "signal_period": 9}
)
def macd_strategy(df: pd.DataFrame, fast_period: int = 12, slow_period: int = 26, signal_period: int = 9) -> pd.DataFrame:
    result = df.copy()
    ema_fast = ema(result["close"], fast_period)
    ema_slow = ema(result["close"], slow_period)
    result["macd_line"] = ema_fast - ema_slow
    result["macd_signal"] = ema(result["macd_line"], signal_period)
    result["macd_hist"] = result["macd_line"] - result["macd_signal"]
    result["position"] = np.where(result["macd_line"] > result["macd_signal"], 1, 0)
    result["signal"] = result["position"].diff()
    return result


@register_strategy(
    "breakout",
    "Breakout — trade breakouts from overnight/session range",
    {"lookback": 20, "atr_period": 14, "atr_multiplier": 1.5}
)
def breakout_strategy(df: pd.DataFrame, lookback: int = 20, atr_period: int = 14, atr_multiplier: float = 1.5) -> pd.DataFrame:
    result = df.copy()
    result["high_roll"] = result["high"].rolling(window=lookback).max()
    result["low_roll"] = result["low"].rolling(window=lookback).min()
    # ATR for confirmation
    tr = pd.concat([
        result["high"] - result["low"],
        (result["high"] - result["close"].shift(1)).abs(),
        (result["low"] - result["close"].shift(1)).abs(),
    ], axis=1).max(axis=1)
    result["atr"] = tr.rolling(window=atr_period).mean()
    result["signal"] = 0
    # Breakout above range high with ATR confirmation
    breakout_up = (result["close"] > result["high_roll"].shift(1)) & (tr > result["atr"] * atr_multiplier)
    result.loc[breakout_up & ~breakout_up.shift(1, fill_value=False), "signal"] = 1
    # Breakdown below range low with ATR confirmation
    breakout_down = (result["close"] < result["low_roll"].shift(1)) & (tr > result["atr"] * atr_multiplier)
    result.loc[breakout_down & ~breakout_down.shift(1, fill_value=False), "signal"] = -1
    return result


@register_strategy(
    "stoch_rsi",
    "Stochastic RSI — earlier momentum signals via stochastic oscillator on RSI",
    {"rsi_period": 14, "stoch_period": 14, "k_smooth": 3, "d_smooth": 3,
     "overbought": 80, "oversold": 20}
)
def stoch_rsi_strategy(df: pd.DataFrame,
                       rsi_period: int = 14, stoch_period: int = 14,
                       k_smooth: int = 3, d_smooth: int = 3,
                       overbought: float = 80, oversold: float = 20) -> pd.DataFrame:
    result = df.copy()
    # RSI
    delta = result["close"].diff()
    gain = delta.clip(lower=0)
    loss = (-delta).clip(lower=0)
    avg_gain = gain.ewm(alpha=1/rsi_period, min_periods=rsi_period, adjust=False).mean()
    avg_loss = loss.ewm(alpha=1/rsi_period, min_periods=rsi_period, adjust=False).mean()
    rs = avg_gain / avg_loss
    result["rsi"] = 100 - (100 / (1 + rs))
    # Stochastic RSI
    rsi_min = result["rsi"].rolling(window=stoch_period).min()
    rsi_max = result["rsi"].rolling(window=stoch_period).max()
    stoch_rsi = (result["rsi"] - rsi_min) / (rsi_max - rsi_min) * 100
    result["stoch_k"] = stoch_rsi.rolling(window=k_smooth).mean()
    result["stoch_d"] = result["stoch_k"].rolling(window=d_smooth).mean()
    # Signals: %K crosses %D in oversold/overbought zones
    result["signal"] = 0
    k_cross_up = (result["stoch_k"] > result["stoch_d"]) & (result["stoch_k"].shift(1) <= result["stoch_d"].shift(1))
    k_cross_down = (result["stoch_k"] < result["stoch_d"]) & (result["stoch_k"].shift(1) >= result["stoch_d"].shift(1))
    result.loc[k_cross_up & (result["stoch_k"] < oversold), "signal"] = 1
    result.loc[k_cross_down & (result["stoch_k"] > overbought), "signal"] = -1
    return result


@register_strategy(
    "supertrend",
    "Supertrend — ATR-based trend following with dynamic support/resistance",
    {"atr_period": 10, "multiplier": 3.0}
)
def supertrend_strategy(df: pd.DataFrame, atr_period: int = 10, multiplier: float = 3.0) -> pd.DataFrame:
    result = df.copy()
    tr = pd.concat([
        result["high"] - result["low"],
        (result["high"] - result["close"].shift(1)).abs(),
        (result["low"] - result["close"].shift(1)).abs(),
    ], axis=1).max(axis=1)
    atr = tr.rolling(window=atr_period).mean()

    hl2 = (result["high"] + result["low"]) / 2
    basic_upper = hl2 + (multiplier * atr)
    basic_lower = hl2 - (multiplier * atr)

    n = len(result)
    final_upper = basic_upper.copy()
    final_lower = basic_lower.copy()
    direction = pd.Series(0, index=result.index, dtype=int)

    for i in range(1, n):
        if basic_upper.iloc[i] < final_upper.iloc[i-1] or result["close"].iloc[i-1] > final_upper.iloc[i-1]:
            final_upper.iloc[i] = basic_upper.iloc[i]
        else:
            final_upper.iloc[i] = final_upper.iloc[i-1]

        if basic_lower.iloc[i] > final_lower.iloc[i-1] or result["close"].iloc[i-1] < final_lower.iloc[i-1]:
            final_lower.iloc[i] = basic_lower.iloc[i]
        else:
            final_lower.iloc[i] = final_lower.iloc[i-1]

        prev_dir = direction.iloc[i-1]
        if prev_dir <= 0:
            direction.iloc[i] = 1 if result["close"].iloc[i] > final_upper.iloc[i] else -1
        else:
            direction.iloc[i] = -1 if result["close"].iloc[i] < final_lower.iloc[i] else 1

    result["supertrend"] = np.where(direction == 1, final_lower, final_upper)
    result["st_direction"] = direction
    result["signal"] = 0
    dir_series = pd.Series(direction.values, index=result.index)
    result.loc[(dir_series == 1) & (dir_series.shift(1) == -1), "signal"] = 1
    result.loc[(dir_series == -1) & (dir_series.shift(1) == 1), "signal"] = -1
    return result


@register_strategy(
    "ichimoku_cloud",
    "Ichimoku Cloud — trend confirmation via Tenkan/Kijun cross, cloud position, and Chikou span",
    {"tenkan_period": 9, "kijun_period": 26, "senkou_b_period": 52}
)
def ichimoku_cloud_strategy(df: pd.DataFrame, tenkan_period: int = 9, kijun_period: int = 26, senkou_b_period: int = 52) -> pd.DataFrame:
    result = df.copy()
    high, low, close = result["high"], result["low"], result["close"]

    tenkan = (high.rolling(window=tenkan_period).max() + low.rolling(window=tenkan_period).min()) / 2
    kijun = (high.rolling(window=kijun_period).max() + low.rolling(window=kijun_period).min()) / 2
    senkou_a = (tenkan + kijun) / 2
    senkou_b = (high.rolling(window=senkou_b_period).max() + low.rolling(window=senkou_b_period).min()) / 2

    result["tenkan"] = tenkan
    result["kijun"] = kijun
    result["senkou_a"] = senkou_a
    result["senkou_b"] = senkou_b

    cloud_top = np.maximum(senkou_a, senkou_b)
    cloud_bottom = np.minimum(senkou_a, senkou_b)
    above_cloud = close > cloud_top
    below_cloud = close < cloud_bottom
    tk_cross_up = (tenkan > kijun) & (tenkan.shift(1) <= kijun.shift(1))
    tk_cross_down = (tenkan < kijun) & (tenkan.shift(1) >= kijun.shift(1))
    chikou_bull = close > close.shift(kijun_period)
    chikou_bear = close < close.shift(kijun_period)

    result["signal"] = 0
    result.loc[above_cloud & tk_cross_up & chikou_bull, "signal"] = 1
    result.loc[below_cloud & tk_cross_down & chikou_bear, "signal"] = -1
    return result


@register_strategy(
    "atr_breakout",
    "ATR Breakout — enter on volatility breakout beyond ATR band",
    {"atr_period": 14, "multiplier": 1.5}
)
def atr_breakout_strategy(df: pd.DataFrame, atr_period: int = 14, multiplier: float = 1.5) -> pd.DataFrame:
    result = df.copy()
    tr = pd.concat([
        result["high"] - result["low"],
        (result["high"] - result["close"].shift(1)).abs(),
        (result["low"] - result["close"].shift(1)).abs(),
    ], axis=1).max(axis=1)
    result["atr"] = tr.rolling(window=atr_period).mean()
    prev_close = result["close"].shift(1)
    upper = prev_close + (multiplier * result["atr"])
    lower = prev_close - (multiplier * result["atr"])
    result["signal"] = 0
    result.loc[(result["close"] > upper) & (result["close"].shift(1) <= upper.shift(1)), "signal"] = 1
    result.loc[(result["close"] < lower) & (result["close"].shift(1) >= lower.shift(1)), "signal"] = -1
    return result


@register_strategy(
    "amd_ifvg",
    "AMD+IFVG — ICT Accumulation-Manipulation-Distribution with Implied Fair Value Gap (15m, session-aware)",
    {
        "asian_start_hour": 0, "asian_end_hour": 8,
        "london_start_hour": 8, "london_end_hour": 12,
        "min_ifvg_pct": 0.05, "sweep_threshold_pct": 0.01,
    }
)
def amd_ifvg_strategy(df: pd.DataFrame, **params) -> pd.DataFrame:
    return amd_ifvg_core(df, **params)


@register_strategy(
    "heikin_ashi_ema",
    "Heikin Ashi + EMA — smoothed candles with EMA trend filter; 2 consecutive HA candles + price side of EMA",
    {"ema_period": 21, "confirmation": 2}
)
def heikin_ashi_ema_strategy(df: pd.DataFrame, ema_period: int = 21, confirmation: int = 2) -> pd.DataFrame:
    result = df.copy()
    # Compute Heikin Ashi candles
    ha_close = (result["open"] + result["high"] + result["low"] + result["close"]) / 4
    ha_open = ha_close.copy()
    for i in range(1, len(result)):
        ha_open.iloc[i] = (ha_open.iloc[i - 1] + ha_close.iloc[i - 1]) / 2
    ha_high = pd.concat([result["high"], ha_open, ha_close], axis=1).max(axis=1)
    ha_low = pd.concat([result["low"], ha_open, ha_close], axis=1).min(axis=1)
    result["ha_open"] = ha_open
    result["ha_close"] = ha_close
    result["ha_high"] = ha_high
    result["ha_low"] = ha_low
    result["ha_ema"] = ema(ha_close, ema_period)
    # Bullish HA: green candle (ha_close > ha_open) with no lower wick (ha_low == ha_open)
    result["ha_bullish"] = (ha_close > ha_open) & (ha_low == ha_open)
    # Bearish HA: red candle (ha_close < ha_open) with no upper wick (ha_high == ha_open)
    result["ha_bearish"] = (ha_close < ha_open) & (ha_high == ha_open)
    # Require `confirmation` consecutive bullish/bearish candles
    bull_streak = result["ha_bullish"].rolling(window=confirmation).sum() == confirmation
    bear_streak = result["ha_bearish"].rolling(window=confirmation).sum() == confirmation
    above_ema = ha_close > result["ha_ema"]
    below_ema = ha_close < result["ha_ema"]
    result["signal"] = 0
    # BUY: confirmation consecutive bullish HA candles + price above EMA
    buy_cond = bull_streak & above_ema
    sell_cond = bear_streak & below_ema
    result.loc[buy_cond & ~buy_cond.shift(1, fill_value=False), "signal"] = 1
    result.loc[sell_cond & ~sell_cond.shift(1, fill_value=False), "signal"] = -1
    return result


if __name__ == "__main__":
    if "--list-json" in sys.argv:
        print(json.dumps([{"id": name, "description": STRATEGY_REGISTRY[name]["description"]} for name in list_strategies()]))
    else:
        print(f"Registered strategies: {list_strategies()}")
        for name in list_strategies():
            s = STRATEGY_REGISTRY[name]
            print(f"  {name}: {s['description']}")
            print(f"    Defaults: {s['default_params']}")
