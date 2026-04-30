"""Unified strategy registry — single source of truth for spot + futures.

Each strategy is registered once via ``@register`` with a ``platforms`` tuple
and, when the spot and futures flavors differ, a ``variants`` dict carrying
per-platform ``description`` / ``default_params`` overrides.

``shared_strategies/spot/strategies.py`` and ``shared_strategies/futures/strategies.py``
are thin shims that call ``build_registry("spot")`` / ``build_registry("futures")``
to materialize a platform-filtered view with the same shape as the legacy
``STRATEGY_REGISTRY`` dict.

Per-platform ordering is explicit in ``PLATFORM_ORDER`` at the bottom of this
file — it must match the legacy registration order in each shim so
``--list-json`` output stays byte-identical.
"""

import os
import sys
from typing import Any, Dict, List, Optional, Tuple

import numpy as np
import pandas as pd

# Strategy core modules live at several paths; wire them all up so
# ``from indicators import sma, ema`` etc. keep working regardless of which
# shim loaded this module.
_THIS_DIR = os.path.dirname(__file__)
for _p in (
    os.path.join(_THIS_DIR, "spot"),  # indicators.py
    _THIS_DIR,                         # amd_ifvg, chart_patterns, …
):
    if _p not in sys.path:
        sys.path.insert(0, _p)

from indicators import sma, ema
from amd_ifvg import amd_ifvg_core
from chart_patterns import chart_pattern_core
from liquidity_sweeps import liquidity_sweep_core
from range_scalper import range_scalper_core
from sweep_squeeze_combo import sweep_squeeze_combo_core
from adx_trend import adx_trend_core
from donchian_breakout import donchian_breakout_core
from session_breakout import session_breakout_core


VALID_PLATFORMS: Tuple[str, ...] = ("spot", "futures")

# name -> {fn, description, default_params, platforms, variants}
STRATEGIES: Dict[str, Dict[str, Any]] = {}


def register(
    name: str,
    description: str,
    default_params: dict,
    platforms: Tuple[str, ...] = ("spot", "futures"),
    variants: Optional[Dict[str, Dict[str, Any]]] = None,
):
    """Register a strategy once.

    ``variants`` maps platform -> {"description": ..., "default_params": {...}}
    for per-platform overrides. Variant ``default_params`` is merged on top of
    the base ``default_params`` (variant wins on key collision).
    """
    if name in STRATEGIES:
        raise ValueError(f"Strategy '{name}' is already registered")
    platforms = tuple(platforms)
    if not platforms:
        raise ValueError(f"{name}: platforms must be non-empty")
    bad = set(platforms) - set(VALID_PLATFORMS)
    if bad:
        raise ValueError(
            f"{name}: unknown platforms {sorted(bad)}; "
            f"expected subset of {VALID_PLATFORMS}"
        )
    variants = variants or {}
    bad_v = set(variants) - set(platforms)
    if bad_v:
        raise ValueError(
            f"{name}: variants keys {sorted(bad_v)} not in platforms {platforms}"
        )

    def decorator(fn):
        STRATEGIES[name] = {
            "fn": fn,
            "description": description,
            "default_params": dict(default_params),
            "platforms": platforms,
            "variants": variants,
        }
        return fn

    return decorator


def build_registry(platform: str) -> Dict[str, Dict[str, Any]]:
    """Return a fresh ``{name: {fn, description, default_params}}`` dict
    filtered to ``platform`` and in the order declared in ``PLATFORM_ORDER``.

    Variant overrides are applied so callers see the platform-specific
    description and merged defaults.
    """
    if platform not in VALID_PLATFORMS:
        raise ValueError(
            f"Unknown platform {platform!r}; expected one of {VALID_PLATFORMS}"
        )
    order = PLATFORM_ORDER[platform]
    expected = {n for n, e in STRATEGIES.items() if platform in e["platforms"]}
    missing_from_order = expected - set(order)
    if missing_from_order:
        raise RuntimeError(
            f"PLATFORM_ORDER[{platform!r}] is missing {sorted(missing_from_order)}"
        )
    extra_in_order = set(order) - expected
    if extra_in_order:
        raise RuntimeError(
            f"PLATFORM_ORDER[{platform!r}] references strategies not tagged for "
            f"{platform!r}: {sorted(extra_in_order)}"
        )

    out: Dict[str, Dict[str, Any]] = {}
    for name in order:
        entry = STRATEGIES[name]
        variant = entry["variants"].get(platform, {})
        out[name] = {
            "fn": entry["fn"],
            "description": variant.get("description", entry["description"]),
            "default_params": {
                **entry["default_params"],
                **variant.get("default_params", {}),
            },
        }
    return out


# ─────────────────────────────────────────────
# Strategy implementations
# ─────────────────────────────────────────────


@register(
    "sma_crossover",
    "SMA Crossover \u2014 buy when fast SMA crosses above slow SMA",
    {"fast_period": 20, "slow_period": 50},
)
def sma_crossover_strategy(df: pd.DataFrame, fast_period: int = 20, slow_period: int = 50) -> pd.DataFrame:
    result = df.copy()
    result["sma_fast"] = sma(result["close"], fast_period)
    result["sma_slow"] = sma(result["close"], slow_period)
    result["position"] = np.where(result["sma_fast"] > result["sma_slow"], 1, 0)
    result["signal"] = result["position"].diff()
    return result


@register(
    "ema_crossover",
    "EMA Crossover \u2014 faster response than SMA crossover",
    {"fast_period": 12, "slow_period": 26},
)
def ema_crossover_strategy(df: pd.DataFrame, fast_period: int = 12, slow_period: int = 26) -> pd.DataFrame:
    result = df.copy()
    result["ema_fast"] = ema(result["close"], fast_period)
    result["ema_slow"] = ema(result["close"], slow_period)
    result["position"] = np.where(result["ema_fast"] > result["ema_slow"], 1, 0)
    result["signal"] = result["position"].diff()
    return result


@register(
    "rsi",
    "RSI \u2014 buy at oversold, sell at overbought",
    {"period": 14, "overbought": 70, "oversold": 30},
    variants={
        "futures": {"description": "RSI \u2014 overbought/oversold signals for futures"},
    },
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


@register(
    "bollinger_bands",
    "Bollinger Bands \u2014 mean reversion at band touches",
    {"period": 20, "num_std": 2.0},
)
def bollinger_strategy(df: pd.DataFrame, period: int = 20, num_std: float = 2.0) -> pd.DataFrame:
    result = df.copy()
    result["bb_middle"] = sma(result["close"], period)
    rolling_std = result["close"].rolling(window=period).std()
    result["bb_upper"] = result["bb_middle"] + (rolling_std * num_std)
    result["bb_lower"] = result["bb_middle"] - (rolling_std * num_std)
    result["signal"] = 0
    result.loc[(result["close"] > result["bb_lower"]) & (result["close"].shift(1) <= result["bb_lower"].shift(1)), "signal"] = 1
    result.loc[(result["close"] < result["bb_upper"]) & (result["close"].shift(1) >= result["bb_upper"].shift(1)), "signal"] = -1
    return result


@register(
    "macd",
    "MACD \u2014 buy/sell on MACD line crossing signal line",
    {"fast_period": 12, "slow_period": 26, "signal_period": 9},
    variants={
        "futures": {"description": "MACD \u2014 momentum crossover for futures"},
    },
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


@register(
    "mean_reversion",
    "Mean Reversion \u2014 buy when price is N std below mean, sell when above",
    {"lookback": 30, "entry_std": 1.5, "exit_std": 0.5},
    variants={
        "futures": {"description": "Mean Reversion \u2014 range-bound index futures trading"},
    },
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


@register(
    "momentum",
    "Momentum \u2014 buy on strong upward momentum, sell on reversal",
    {"roc_period": 14, "threshold": 5.0},
    variants={
        "futures": {
            "description": "Momentum \u2014 trend following on futures using rate of change",
            "default_params": {"threshold": 3.0},
        },
    },
)
def momentum_strategy(df: pd.DataFrame, roc_period: int = 14, threshold: float = 5.0) -> pd.DataFrame:
    result = df.copy()
    result["roc"] = ((result["close"] - result["close"].shift(roc_period)) / result["close"].shift(roc_period)) * 100
    result["signal"] = 0
    result.loc[(result["roc"] > threshold) & (result["roc"].shift(1) <= threshold), "signal"] = 1
    result.loc[(result["roc"] < -threshold) & (result["roc"].shift(1) >= -threshold), "signal"] = -1
    return result


@register(
    "volume_weighted",
    "Volume-Weighted \u2014 confirms trend with volume analysis",
    {"sma_period": 20, "vol_multiplier": 1.5},
)
def volume_weighted_strategy(df: pd.DataFrame, sma_period: int = 20, vol_multiplier: float = 1.5) -> pd.DataFrame:
    result = df.copy()
    result["price_sma"] = sma(result["close"], sma_period)
    result["vol_sma"] = sma(result["volume"], sma_period)
    result["high_volume"] = result["volume"] > (result["vol_sma"] * vol_multiplier)
    result["signal"] = 0
    price_cross_up = (result["close"] > result["price_sma"]) & (result["close"].shift(1) <= result["price_sma"].shift(1))
    result.loc[price_cross_up & result["high_volume"], "signal"] = 1
    price_cross_down = (result["close"] < result["price_sma"]) & (result["close"].shift(1) >= result["price_sma"].shift(1))
    result.loc[price_cross_down & result["high_volume"], "signal"] = -1
    return result


@register(
    "triple_ema",
    "Triple EMA \u2014 trend confirmation using 3 EMAs (short/mid/long)",
    {"short_period": 8, "mid_period": 21, "long_period": 55},
)
def triple_ema_strategy(df: pd.DataFrame, short_period: int = 8, mid_period: int = 21, long_period: int = 55) -> pd.DataFrame:
    result = df.copy()
    result["ema_short"] = ema(result["close"], short_period)
    result["ema_mid"] = ema(result["close"], mid_period)
    result["ema_long"] = ema(result["close"], long_period)
    bullish = (result["ema_short"] > result["ema_mid"]) & (result["ema_mid"] > result["ema_long"])
    result["position"] = np.where(bullish, 1, 0)
    result["signal"] = result["position"].diff()
    return result


@register(
    "triple_ema_bidir",
    "Triple EMA Bidirectional \u2014 long on bullish stack, short on bearish stack",
    {"short_period": 8, "mid_period": 21, "long_period": 55},
    platforms=("futures",),
)
def triple_ema_bidir_strategy(df: pd.DataFrame, short_period: int = 8, mid_period: int = 21, long_period: int = 55) -> pd.DataFrame:
    result = df.copy()
    result["ema_short"] = ema(result["close"], short_period)
    result["ema_mid"] = ema(result["close"], mid_period)
    result["ema_long"] = ema(result["close"], long_period)
    bullish = (result["ema_short"] > result["ema_mid"]) & (result["ema_mid"] > result["ema_long"])
    bearish = (result["ema_short"] < result["ema_mid"]) & (result["ema_mid"] < result["ema_long"])
    result["position"] = np.where(bullish, 1, np.where(bearish, -1, 0))
    # A direct bullish→bearish flip yields diff == -2; clamp so downstream sees {-1, 0, 1}.
    result["signal"] = result["position"].diff().clip(-1, 1)
    return result


@register(
    "rsi_macd_combo",
    "RSI+MACD Combo \u2014 dual confirmation for higher quality signals",
    {"rsi_period": 14, "rsi_oversold": 35, "rsi_overbought": 65,
     "macd_fast": 12, "macd_slow": 26, "macd_signal": 9,
     "rsi_short_min": 50, "rsi_long_max": 50},
)
def rsi_macd_combo_strategy(df: pd.DataFrame,
                             rsi_period: int = 14, rsi_oversold: float = 35, rsi_overbought: float = 65,
                             macd_fast: int = 12, macd_slow: int = 26, macd_signal: int = 9,
                             rsi_short_min: float = 50, rsi_long_max: float = 50) -> pd.DataFrame:
    result = df.copy()
    delta = result["close"].diff()
    gain = delta.clip(lower=0)
    loss = (-delta).clip(lower=0)
    avg_gain = gain.ewm(alpha=1/rsi_period, min_periods=rsi_period, adjust=False).mean()
    avg_loss = loss.ewm(alpha=1/rsi_period, min_periods=rsi_period, adjust=False).mean()
    rs = avg_gain / avg_loss
    result["rsi"] = 100 - (100 / (1 + rs))
    ema_fast = ema(result["close"], macd_fast)
    ema_slow = ema(result["close"], macd_slow)
    result["macd_line"] = ema_fast - ema_slow
    result["macd_signal_line"] = ema(result["macd_line"], macd_signal)
    result["signal"] = 0
    # Buy: MACD bullish cross AND RSI below rsi_long_max (default 50 = not already overbought).
    # Lower rsi_long_max to require a more oversold RSI before longing.
    macd_bull = (result["macd_line"] > result["macd_signal_line"]) & (result["macd_line"].shift(1) <= result["macd_signal_line"].shift(1))
    rsi_ok = result["rsi"] < rsi_long_max
    result.loc[macd_bull & rsi_ok, "signal"] = 1
    # Sell: MACD bearish cross AND RSI above rsi_short_min (default 50 = not already oversold).
    # Lower rsi_short_min to allow shorts deeper into a downtrend.
    macd_bear = (result["macd_line"] < result["macd_signal_line"]) & (result["macd_line"].shift(1) >= result["macd_signal_line"].shift(1))
    rsi_high = result["rsi"] > rsi_short_min
    result.loc[macd_bear & rsi_high, "signal"] = -1
    return result


@register(
    "stoch_rsi",
    "Stochastic RSI \u2014 earlier momentum signals via stochastic oscillator on RSI",
    {"rsi_period": 14, "stoch_period": 14, "k_smooth": 3, "d_smooth": 3,
     "overbought": 80, "oversold": 20},
)
def stoch_rsi_strategy(df: pd.DataFrame,
                       rsi_period: int = 14, stoch_period: int = 14,
                       k_smooth: int = 3, d_smooth: int = 3,
                       overbought: float = 80, oversold: float = 20) -> pd.DataFrame:
    result = df.copy()
    delta = result["close"].diff()
    gain = delta.clip(lower=0)
    loss = (-delta).clip(lower=0)
    avg_gain = gain.ewm(alpha=1/rsi_period, min_periods=rsi_period, adjust=False).mean()
    avg_loss = loss.ewm(alpha=1/rsi_period, min_periods=rsi_period, adjust=False).mean()
    rs = avg_gain / avg_loss
    result["rsi"] = 100 - (100 / (1 + rs))
    rsi_min = result["rsi"].rolling(window=stoch_period).min()
    rsi_max = result["rsi"].rolling(window=stoch_period).max()
    stoch_rsi = (result["rsi"] - rsi_min) / (rsi_max - rsi_min) * 100
    result["stoch_k"] = stoch_rsi.rolling(window=k_smooth).mean()
    result["stoch_d"] = result["stoch_k"].rolling(window=d_smooth).mean()
    result["signal"] = 0
    k_cross_up = (result["stoch_k"] > result["stoch_d"]) & (result["stoch_k"].shift(1) <= result["stoch_d"].shift(1))
    k_cross_down = (result["stoch_k"] < result["stoch_d"]) & (result["stoch_k"].shift(1) >= result["stoch_d"].shift(1))
    result.loc[k_cross_up & (result["stoch_k"] < oversold), "signal"] = 1
    result.loc[k_cross_down & (result["stoch_k"] > overbought), "signal"] = -1
    return result


@register(
    "supertrend",
    "Supertrend \u2014 ATR-based trend following with dynamic support/resistance",
    {"atr_period": 10, "multiplier": 3.0},
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


@register(
    "ichimoku_cloud",
    "Ichimoku Cloud \u2014 trend confirmation via Tenkan/Kijun cross, cloud position, and Chikou span",
    {"tenkan_period": 9, "kijun_period": 26, "senkou_b_period": 52},
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


@register(
    "pairs_spread",
    "Pairs/Spread Trading \u2014 trade z-score of price ratio between two assets (needs 'close_b' column)",
    {"lookback": 30, "entry_z": 2.0, "exit_z": 0.5},
    platforms=("spot",),
)
def pairs_spread_strategy(df: pd.DataFrame, lookback: int = 30, entry_z: float = 2.0, exit_z: float = 0.5) -> pd.DataFrame:
    """
    Stat arb / pairs trading on spread. Requires 'close_b' column for the second asset.
    If 'close_b' is not present, uses close price ratio to its own rolling mean (self-mean-reversion).
    """
    result = df.copy()
    if "close_b" in result.columns:
        result["spread"] = result["close"] / result["close_b"]
    else:
        result["spread"] = result["close"]

    result["spread_mean"] = result["spread"].rolling(window=lookback).mean()
    result["spread_std"] = result["spread"].rolling(window=lookback).std()
    result["z_score"] = (result["spread"] - result["spread_mean"]) / result["spread_std"]
    result["signal"] = 0
    result.loc[(result["z_score"] > -entry_z) & (result["z_score"].shift(1) <= -entry_z), "signal"] = 1
    result.loc[(result["z_score"] < exit_z) & (result["z_score"].shift(1) >= exit_z), "signal"] = -1
    return result


@register(
    "squeeze_momentum",
    "Squeeze Momentum \u2014 BB inside KC detects coiling, trades breakout with momentum confirmation",
    {"bb_period": 20, "bb_std": 2.0, "kc_period": 20, "kc_mult": 1.5, "mom_lookback": 12},
)
def squeeze_momentum_strategy(df: pd.DataFrame,
                              bb_period: int = 20, bb_std: float = 2.0,
                              kc_period: int = 20, kc_mult: float = 1.5,
                              mom_lookback: int = 12) -> pd.DataFrame:
    result = df.copy()
    bb_mid = sma(result["close"], bb_period)
    bb_stddev = result["close"].rolling(window=bb_period).std()
    bb_upper = bb_mid + (bb_std * bb_stddev)
    bb_lower = bb_mid - (bb_std * bb_stddev)
    kc_mid = ema(result["close"], kc_period)
    tr = pd.concat([
        result["high"] - result["low"],
        (result["high"] - result["close"].shift(1)).abs(),
        (result["low"] - result["close"].shift(1)).abs(),
    ], axis=1).max(axis=1)
    atr = tr.rolling(window=kc_period).mean()
    kc_upper = kc_mid + (kc_mult * atr)
    kc_lower = kc_mid - (kc_mult * atr)
    result["squeeze_on"] = (bb_lower > kc_lower) & (bb_upper < kc_upper)
    highest_high = result["high"].rolling(window=kc_period).max()
    lowest_low = result["low"].rolling(window=kc_period).min()
    midline = ((highest_high + lowest_low) / 2 + bb_mid) / 2
    delta = result["close"] - midline
    x = np.arange(mom_lookback, dtype=float)
    x_mean = x.mean()
    x_var = ((x - x_mean) ** 2).sum()
    def _linreg_last(window):
        if len(window) < mom_lookback or np.isnan(window).any():
            return np.nan
        slope = ((x - x_mean) * (window - window.mean())).sum() / x_var
        return slope * (mom_lookback - 1 - x_mean) + window.mean()
    result["squeeze_mom"] = delta.rolling(window=mom_lookback).apply(_linreg_last, raw=True)
    squeeze_fired = (~result["squeeze_on"]) & (result["squeeze_on"].shift(1) == True)
    mom_pos_rising = (result["squeeze_mom"] > 0) & (result["squeeze_mom"] > result["squeeze_mom"].shift(1))
    mom_neg_falling = (result["squeeze_mom"] < 0) & (result["squeeze_mom"] < result["squeeze_mom"].shift(1))
    result["signal"] = 0
    result.loc[squeeze_fired & mom_pos_rising, "signal"] = 1
    result.loc[squeeze_fired & mom_neg_falling, "signal"] = -1
    return result


@register(
    "breakout",
    "Breakout \u2014 trade breakouts from overnight/session range",
    {"lookback": 20, "atr_period": 14, "atr_multiplier": 1.5},
    platforms=("futures",),
)
def breakout_strategy(df: pd.DataFrame, lookback: int = 20, atr_period: int = 14, atr_multiplier: float = 1.5) -> pd.DataFrame:
    result = df.copy()
    result["high_roll"] = result["high"].rolling(window=lookback).max()
    result["low_roll"] = result["low"].rolling(window=lookback).min()
    tr = pd.concat([
        result["high"] - result["low"],
        (result["high"] - result["close"].shift(1)).abs(),
        (result["low"] - result["close"].shift(1)).abs(),
    ], axis=1).max(axis=1)
    result["atr"] = tr.rolling(window=atr_period).mean()
    result["signal"] = 0
    breakout_up = (result["close"] > result["high_roll"].shift(1)) & (tr > result["atr"] * atr_multiplier)
    result.loc[breakout_up & ~breakout_up.shift(1, fill_value=False), "signal"] = 1
    breakout_down = (result["close"] < result["low_roll"].shift(1)) & (tr > result["atr"] * atr_multiplier)
    result.loc[breakout_down & ~breakout_down.shift(1, fill_value=False), "signal"] = -1
    return result


@register(
    "atr_breakout",
    "ATR Breakout \u2014 enter on volatility breakout beyond ATR band",
    {"atr_period": 14, "multiplier": 1.5},
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


@register(
    "amd_ifvg",
    "AMD+IFVG \u2014 ICT Accumulation-Manipulation-Distribution with Implied Fair Value Gap (15m, session-aware)",
    {
        "asian_start_hour": 0, "asian_end_hour": 8,
        "london_start_hour": 8, "london_end_hour": 12,
        "min_ifvg_pct": 0.05, "sweep_threshold_pct": 0.01,
    },
)
def amd_ifvg_strategy(df: pd.DataFrame, **params) -> pd.DataFrame:
    return amd_ifvg_core(df, **params)


@register(
    "heikin_ashi_ema",
    "Heikin Ashi + EMA \u2014 smoothed candles with EMA trend filter; 2 consecutive HA candles + price side of EMA",
    {"ema_period": 21, "confirmation": 2},
)
def heikin_ashi_ema_strategy(df: pd.DataFrame, ema_period: int = 21, confirmation: int = 2) -> pd.DataFrame:
    result = df.copy()
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
    result["ha_bullish"] = (ha_close > ha_open) & (ha_low == ha_open)
    result["ha_bearish"] = (ha_close < ha_open) & (ha_high == ha_open)
    bull_streak = result["ha_bullish"].rolling(window=confirmation).sum() == confirmation
    bear_streak = result["ha_bearish"].rolling(window=confirmation).sum() == confirmation
    above_ema = ha_close > result["ha_ema"]
    below_ema = ha_close < result["ha_ema"]
    result["signal"] = 0
    buy_cond = bull_streak & above_ema
    sell_cond = bear_streak & below_ema
    result.loc[buy_cond & ~buy_cond.shift(1, fill_value=False), "signal"] = 1
    result.loc[sell_cond & ~sell_cond.shift(1, fill_value=False), "signal"] = -1
    return result


@register(
    "order_blocks",
    "Order Blocks (ICT/SMC) \u2014 institutional supply/demand zones from displacement candles",
    {"atr_period": 14, "displacement_mult": 1.5, "ob_lookback": 20, "max_ob_age": 50},
)
def order_blocks_strategy(df: pd.DataFrame,
                          atr_period: int = 14, displacement_mult: float = 1.5,
                          ob_lookback: int = 20, max_ob_age: int = 50) -> pd.DataFrame:
    result = df.copy()
    close = result["close"].values
    high = result["high"].values
    low = result["low"].values
    opn = result["open"].values
    n = len(result)

    tr = pd.concat([
        result["high"] - result["low"],
        (result["high"] - result["close"].shift(1)).abs(),
        (result["low"] - result["close"].shift(1)).abs(),
    ], axis=1).max(axis=1)
    atr = tr.rolling(window=atr_period).mean().values

    signal = np.zeros(n, dtype=int)

    # Track active order blocks as tuples: (type, ob_high, ob_low, birth_idx, touched)
    active_obs = []

    for i in range(1, n):
        if np.isnan(atr[i]):
            continue

        body = abs(close[i] - opn[i])
        threshold = displacement_mult * atr[i]

        if body > threshold:
            bullish_displacement = close[i] > opn[i]

            for j in range(i - 1, max(i - ob_lookback - 1, 0) - 1, -1):
                if bullish_displacement and close[j] < opn[j]:
                    active_obs.append(("bull", high[j], low[j], i, False))
                    break
                elif not bullish_displacement and close[j] > opn[j]:
                    active_obs.append(("bear", high[j], low[j], i, False))
                    break

        new_obs = []
        for ob_type, ob_high, ob_low, birth, touched in active_obs:
            age = i - birth
            if age > max_ob_age:
                continue

            if ob_type == "bull":
                if close[i] < ob_low:
                    continue
                if low[i] <= ob_high and not touched:
                    signal[i] = 1
                    new_obs.append((ob_type, ob_high, ob_low, birth, True))
                    continue
            else:
                if close[i] > ob_high:
                    continue
                if high[i] >= ob_low and not touched:
                    signal[i] = -1
                    new_obs.append((ob_type, ob_high, ob_low, birth, True))
                    continue

            new_obs.append((ob_type, ob_high, ob_low, birth, touched))
        active_obs = new_obs

    result["signal"] = signal
    return result


@register(
    "vwap_reversion",
    "VWAP Reversion \u2014 buy when price drops below VWAP by N std devs, sell when above",
    {"entry_std": 1.5, "exit_std": 0.2},
)
def vwap_reversion_strategy(df: pd.DataFrame, entry_std: float = 1.5, exit_std: float = 0.2) -> pd.DataFrame:
    result = df.copy()
    if result.empty:
        result["signal"] = 0
        return result
    if isinstance(result.index, pd.DatetimeIndex):
        day = result.index.date
    else:
        day = pd.to_datetime(result.index).date
    result["_day"] = day
    typical_price = (result["high"] + result["low"] + result["close"]) / 3
    result["_tp_vol"] = typical_price * result["volume"]
    result["_cum_tp_vol"] = result.groupby("_day")["_tp_vol"].cumsum()
    result["_cum_vol"] = result.groupby("_day")["volume"].cumsum()
    result["vwap"] = result["_cum_tp_vol"] / result["_cum_vol"]
    result["vwap_std"] = result.groupby("_day")["close"].transform(
        lambda x: (x - result.loc[x.index, "vwap"]).expanding().std()
    )
    result["vwap_std"] = result["vwap_std"].fillna(0)
    result["signal"] = 0
    lower = result["vwap"] - entry_std * result["vwap_std"]
    upper = result["vwap"] + entry_std * result["vwap_std"]
    buy_cross = (result["close"] < lower) & (result["close"].shift(1) >= lower.shift(1))
    sell_cross = (result["close"] > upper) & (result["close"].shift(1) <= upper.shift(1))
    result.loc[buy_cross, "signal"] = 1
    result.loc[sell_cross, "signal"] = -1
    result.drop(columns=["_day", "_tp_vol", "_cum_tp_vol", "_cum_vol"], inplace=True)
    return result


@register(
    "chart_pattern",
    "Chart Pattern \u2014 detects Double Top/Bottom, H&S, Flags, Triangles with volume confirmation",
    {"pivot_lookback": 5, "tolerance": 0.03, "vol_multiplier": 1.5, "vol_period": 20},
)
def chart_pattern_strategy(df: pd.DataFrame, **params) -> pd.DataFrame:
    return chart_pattern_core(df, **params)


@register(
    "liquidity_sweeps",
    "Liquidity Sweeps (ICT) \u2014 fades stop-hunt wicks beyond swing highs/lows after price closes back inside range",
    {"swing_lookback": 20, "confirmation": 1},
)
def liquidity_sweeps_strategy(df: pd.DataFrame, **params) -> pd.DataFrame:
    return liquidity_sweep_core(df, **params)


@register(
    "parabolic_sar",
    "Parabolic SAR \u2014 trend-following stop and reverse with accelerating trailing stop",
    {"iaf": 0.02, "af_step": 0.02, "max_af": 0.2},
)
def parabolic_sar_strategy(df: pd.DataFrame, iaf: float = 0.02, af_step: float = 0.02, max_af: float = 0.2) -> pd.DataFrame:
    result = df.copy()
    high = result["high"].values
    low = result["low"].values
    close = result["close"].values
    n = len(close)
    sar = np.zeros(n)
    trend = np.zeros(n, dtype=int)  # 1 = uptrend, -1 = downtrend
    af = np.zeros(n)
    ep = np.zeros(n)

    if n < 2:
        result["sar"] = np.nan
        result["signal"] = 0
        return result

    trend[0] = 1  # neutral default; avoids look-ahead bias from peeking at close[1] (#104)
    if trend[0] == 1:
        sar[0] = low[0]
        ep[0] = high[0]
    else:
        sar[0] = high[0]
        ep[0] = low[0]
    af[0] = iaf

    for i in range(1, n):
        prev_sar = sar[i - 1]
        prev_af = af[i - 1]
        prev_ep = ep[i - 1]
        prev_trend = trend[i - 1]

        new_sar = prev_sar + prev_af * (prev_ep - prev_sar)

        if prev_trend == 1:
            new_sar = min(new_sar, low[i - 1])
            if i >= 2:
                new_sar = min(new_sar, low[i - 2])
        else:
            new_sar = max(new_sar, high[i - 1])
            if i >= 2:
                new_sar = max(new_sar, high[i - 2])

        if prev_trend == 1 and low[i] < new_sar:
            trend[i] = -1
            sar[i] = prev_ep
            ep[i] = low[i]
            af[i] = iaf
        elif prev_trend == -1 and high[i] > new_sar:
            trend[i] = 1
            sar[i] = prev_ep
            ep[i] = high[i]
            af[i] = iaf
        else:
            trend[i] = prev_trend
            sar[i] = new_sar
            if prev_trend == 1:
                ep[i] = max(prev_ep, high[i])
            else:
                ep[i] = min(prev_ep, low[i])
            if ep[i] != prev_ep:
                af[i] = min(prev_af + af_step, max_af)
            else:
                af[i] = prev_af

    result["sar"] = sar
    result["signal"] = 0
    trend_series = pd.Series(trend, index=result.index)
    buy = (trend_series == 1) & (trend_series.shift(1) == -1)
    sell = (trend_series == -1) & (trend_series.shift(1) == 1)
    result.loc[buy, "signal"] = 1
    result.loc[sell, "signal"] = -1
    return result


@register(
    "range_scalper",
    "Range Scalper \u2014 detects low-volatility consolidation via Bollinger bandwidth + volume, then mean-reverts at band touches",
    {"bb_period": 14, "bb_std": 1.5, "bw_threshold": 0.008, "vol_ratio": 0.8, "rsi_period": 7, "rsi_ob": 70, "rsi_os": 30},
)
def range_scalper_strategy(df: pd.DataFrame, **params) -> pd.DataFrame:
    return range_scalper_core(df, **params)


@register(
    "sweep_squeeze_combo",
    "Sweep Squeeze Combo \u2014 2-of-3 consensus (liquidity sweeps + squeeze momentum + stochastic RSI) for high-conviction reversals",
    {"swing_lookback": 10, "min_agree": 2},
)
def sweep_squeeze_combo_strategy(df: pd.DataFrame, **params) -> pd.DataFrame:
    return sweep_squeeze_combo_core(df, **params)


@register(
    "adx_trend",
    "ADX Trend Rider \u2014 enters on DI crossovers when ADX confirms strong trend (>25)",
    {"adx_period": 14, "adx_threshold": 25},
)
def adx_trend_strategy(df: pd.DataFrame, **params) -> pd.DataFrame:
    return adx_trend_core(df, **params)


@register(
    "delta_neutral_funding",
    "Delta-Neutral Funding \u2014 enter when 7d avg funding rate exceeds threshold, exit when below",
    {"entry_threshold": 0.0001, "exit_threshold": 0.00005, "drift_threshold": 2.0,
     "current_funding_rate": 0.0, "avg_funding_rate_7d": 0.0},
    platforms=("futures",),
)
def delta_neutral_funding_strategy(df: pd.DataFrame,
                                   entry_threshold: float = 0.0001,
                                   exit_threshold: float = 0.00005,
                                   drift_threshold: float = 2.0,
                                   current_funding_rate: float = 0.0,
                                   avg_funding_rate_7d: float = 0.0) -> pd.DataFrame:
    result = df.copy()
    avg = avg_funding_rate_7d
    result["funding_rate"] = current_funding_rate
    result["avg_funding_7d"] = avg
    result["funding_apy"] = avg * 3 * 365 * 100
    result["delta_drift_pct"] = 0.0
    result["rebalance_needed"] = 0.0
    result["signal"] = 0
    if avg == 0.0:
        return result
    # Positive avg funding = longs pay shorts → SHORT perp to collect (#102)
    if avg > entry_threshold:
        result.iloc[-1, result.columns.get_loc("signal")] = -1  # enter short
    elif avg < exit_threshold:
        result.iloc[-1, result.columns.get_loc("signal")] = 1   # exit short
    return result


@register(
    "donchian_breakout",
    "Donchian Channel Breakout \u2014 turtle-trading style entry on new high/low channel breakouts",
    {"entry_period": 20, "exit_period": 10},
)
def donchian_breakout_strategy(df: pd.DataFrame, **params) -> pd.DataFrame:
    return donchian_breakout_core(df, **params)


@register(
    "session_breakout",
    "Session Breakout — break of prior session (Asian/US open/US close) high/low with volume confirmation",
    {
        "session": "asian", "lookback": 1, "volume_threshold": 1.5,
        "vol_period": 20, "atr_period": 14, "atr_multiplier": 0.0,
    },
    platforms=("futures",),
)
def session_breakout_strategy(df: pd.DataFrame, **params) -> pd.DataFrame:
    return session_breakout_core(df, **params)


def _position_float(params: dict, key: str) -> float:
    try:
        return float(params.get(key, 0) or 0)
    except (TypeError, ValueError):
        return 0.0


def _position_close_frame(df: pd.DataFrame, close_fraction: float, reason: str) -> pd.DataFrame:
    result = df.tail(1).copy()
    if result.empty:
        result = pd.DataFrame({"close": [0.0]})
    result["signal"] = 0
    result["close_fraction"] = close_fraction
    result["reason"] = reason
    return result


@register(
    "tp_at_pct",
    "Position-aware test close — close when mark reaches a profit percentage from avg cost",
    {"pct": 0.03},
)
def tp_at_pct_strategy(df: pd.DataFrame, pct: float = 0.03, **params) -> pd.DataFrame:
    """Reference close evaluator for position-aware wiring tests (#496).

    Entry ATR is available only when the entry strategy's result includes an
    ``atr`` column; check scripts copy that last-row value into ``entry_atr``.
    ATR-dependent close evaluators should return a no-op reason when it is 0.
    """
    avg_cost = _position_float(params, "avg_cost")
    current_quantity = _position_float(params, "current_quantity")
    initial_quantity = _position_float(params, "initial_quantity")
    entry_atr = _position_float(params, "entry_atr")
    side = str(params.get("side", "") or "").strip().lower()
    _ = (initial_quantity, entry_atr)  # Read for the canonical wrapper shape; unused by this simple TP.

    if df.empty or "close" not in df.columns:
        return _position_close_frame(df, 0.0, "noop:missing_mark_price")
    if avg_cost <= 0 or current_quantity <= 0 or side not in ("long", "short"):
        return _position_close_frame(df, 0.0, "noop:missing_position")
    try:
        mark_price = float(df["close"].iloc[-1])
    except (TypeError, ValueError):
        return _position_close_frame(df, 0.0, "noop:missing_mark_price")
    if mark_price <= 0:
        return _position_close_frame(df, 0.0, "noop:missing_mark_price")

    try:
        threshold = max(float(pct), 0.0)
    except (TypeError, ValueError):
        threshold = 0.0
    if side == "long":
        pnl_pct = (mark_price - avg_cost) / avg_cost
    else:
        pnl_pct = (avg_cost - mark_price) / avg_cost
    if pnl_pct >= threshold:
        return _position_close_frame(df, 1.0, "tp_at_pct:hit")
    return _position_close_frame(df, 0.0, "noop:not_hit")


# ─────────────────────────────────────────────
# Per-platform display order.
# These lists MUST match the legacy registration order in each shim so
# ``--list-json`` output stays byte-identical (agent tooling depends on it).
# ─────────────────────────────────────────────

PLATFORM_ORDER: Dict[str, List[str]] = {
    "spot": [
        "sma_crossover", "ema_crossover", "rsi", "bollinger_bands", "macd",
        "mean_reversion", "momentum", "volume_weighted", "triple_ema",
        "rsi_macd_combo", "stoch_rsi", "supertrend", "ichimoku_cloud",
        "pairs_spread", "squeeze_momentum", "atr_breakout", "amd_ifvg",
        "heikin_ashi_ema", "order_blocks", "vwap_reversion", "chart_pattern",
        "liquidity_sweeps", "parabolic_sar", "range_scalper",
        "sweep_squeeze_combo", "adx_trend", "donchian_breakout",
        "tp_at_pct",
    ],
    "futures": [
        "sma_crossover", "ema_crossover", "bollinger_bands", "volume_weighted",
        "triple_ema", "triple_ema_bidir", "rsi_macd_combo", "momentum",
        "mean_reversion", "rsi", "macd", "breakout", "stoch_rsi", "supertrend",
        "squeeze_momentum", "ichimoku_cloud", "atr_breakout", "amd_ifvg",
        "heikin_ashi_ema", "order_blocks", "vwap_reversion", "chart_pattern",
        "liquidity_sweeps", "parabolic_sar", "range_scalper",
        "sweep_squeeze_combo", "adx_trend", "delta_neutral_funding",
        "donchian_breakout", "session_breakout", "tp_at_pct",
    ],
}
