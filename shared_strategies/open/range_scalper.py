
import numpy as np
import pandas as pd

from indicators_core import wilder_rsi


def _sma(series: pd.Series, period: int) -> pd.Series:
    return series.rolling(window=period).mean()


def range_scalper_core(df: pd.DataFrame,
                       bb_period: int = 14, bb_std: float = 1.5,
                       bw_threshold: float = 0.008, vol_ratio: float = 0.8,
                       rsi_period: int = 7, rsi_ob: float = 70, rsi_os: float = 30) -> pd.DataFrame:
    result = df.copy()

    result["bb_mid"] = _sma(result["close"], bb_period)
    bb_rolling_std = result["close"].rolling(window=bb_period).std()
    result["bb_upper"] = result["bb_mid"] + (bb_rolling_std * bb_std)
    result["bb_lower"] = result["bb_mid"] - (bb_rolling_std * bb_std)

    safe_mid = result["bb_mid"].replace(0, np.nan)
    result["bb_bandwidth"] = (result["bb_upper"] - result["bb_lower"]) / safe_mid

    result["vol_sma"] = _sma(result["volume"], bb_period)
    result["low_volume"] = result["volume"] < (result["vol_sma"] * vol_ratio)

    result["in_range"] = (result["bb_bandwidth"] < bw_threshold) & result["low_volume"]

    result["rsi"] = wilder_rsi(result["close"], rsi_period)

    result["signal"] = 0
    buy_cross = (result["close"] <= result["bb_lower"]) & (result["close"].shift(1) > result["bb_lower"].shift(1))
    buy_touch = buy_cross & (result["rsi"] < rsi_os) & result["in_range"]
    sell_cross = (result["close"] >= result["bb_upper"]) & (result["close"].shift(1) < result["bb_upper"].shift(1))
    sell_touch = sell_cross & (result["rsi"] > rsi_ob) & result["in_range"]
    result.loc[buy_touch, "signal"] = 1
    result.loc[sell_touch, "signal"] = -1
    return result
