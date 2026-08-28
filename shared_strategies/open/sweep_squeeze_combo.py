
import numpy as np
import pandas as pd

from indicators_core import atr_sma, wilder_rsi

from liquidity_sweeps import liquidity_sweep_core


def _sma(series: pd.Series, period: int) -> pd.Series:
    return series.rolling(window=period).mean()


def _ema(series: pd.Series, period: int) -> pd.Series:
    return series.ewm(span=period, adjust=False).mean()


def _squeeze_signals(
    df: pd.DataFrame,
    bb_period: int = 20,
    bb_std: float = 2.0,
    kc_period: int = 20,
    kc_mult: float = 1.5,
    mom_lookback: int = 12,
) -> pd.Series:
    result = df.copy()
    bb_mid = _sma(result["close"], bb_period)
    bb_stddev = result["close"].rolling(window=bb_period).std()
    bb_upper = bb_mid + (bb_std * bb_stddev)
    bb_lower = bb_mid - (bb_std * bb_stddev)

    kc_mid = _ema(result["close"], kc_period)
    atr = atr_sma(result, kc_period, round_large=False)
    kc_upper = kc_mid + (kc_mult * atr)
    kc_lower = kc_mid - (kc_mult * atr)

    squeeze_on = (bb_lower > kc_lower) & (bb_upper < kc_upper)

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

    squeeze_mom = delta.rolling(window=mom_lookback).apply(_linreg_last, raw=True)

    squeeze_fired = (~squeeze_on) & (squeeze_on.shift(1) == True)
    mom_pos_rising = (squeeze_mom > 0) & (squeeze_mom > squeeze_mom.shift(1))
    mom_neg_falling = (squeeze_mom < 0) & (squeeze_mom < squeeze_mom.shift(1))

    signal = pd.Series(0, index=df.index)
    signal.loc[squeeze_fired & mom_pos_rising] = 1
    signal.loc[squeeze_fired & mom_neg_falling] = -1
    return signal


def _stoch_rsi_signals(
    df: pd.DataFrame,
    rsi_period: int = 14,
    stoch_period: int = 14,
    k_smooth: int = 3,
    d_smooth: int = 3,
    overbought: float = 80,
    oversold: float = 20,
) -> pd.Series:
    close = df["close"]
    rsi = wilder_rsi(close, rsi_period)

    rsi_min = rsi.rolling(window=stoch_period).min()
    rsi_max = rsi.rolling(window=stoch_period).max()
    stoch_rsi = (rsi - rsi_min) / (rsi_max - rsi_min) * 100
    stoch_k = stoch_rsi.rolling(window=k_smooth).mean()
    stoch_d = stoch_k.rolling(window=d_smooth).mean()

    k_cross_up = (stoch_k > stoch_d) & (stoch_k.shift(1) <= stoch_d.shift(1))
    k_cross_down = (stoch_k < stoch_d) & (stoch_k.shift(1) >= stoch_d.shift(1))

    signal = pd.Series(0, index=df.index)
    signal.loc[k_cross_up & (stoch_k < oversold)] = 1
    signal.loc[k_cross_down & (stoch_k > overbought)] = -1
    return signal


def sweep_squeeze_combo_core(
    df: pd.DataFrame,
    swing_lookback: int = 10,
    confirmation: int = 1,
    bb_period: int = 20,
    bb_std: float = 2.0,
    kc_period: int = 20,
    kc_mult: float = 1.5,
    mom_lookback: int = 12,
    rsi_period: int = 14,
    stoch_period: int = 14,
    k_smooth: int = 3,
    d_smooth: int = 3,
    overbought: float = 80,
    oversold: float = 20,
    min_agree: int = 2,
) -> pd.DataFrame:
    result = df.copy()

    ls_result = liquidity_sweep_core(df, swing_lookback=swing_lookback, confirmation=confirmation)
    ls_signal = ls_result["signal"]

    sq_signal = _squeeze_signals(df, bb_period, bb_std, kc_period, kc_mult, mom_lookback)

    sr_signal = _stoch_rsi_signals(df, rsi_period, stoch_period, k_smooth, d_smooth, overbought, oversold)

    buy_votes = (ls_signal == 1).astype(int) + (sq_signal == 1).astype(int) + (sr_signal == 1).astype(int)
    sell_votes = (ls_signal == -1).astype(int) + (sq_signal == -1).astype(int) + (sr_signal == -1).astype(int)

    result["signal"] = 0
    result.loc[buy_votes >= min_agree, "signal"] = 1
    result.loc[sell_votes >= min_agree, "signal"] = -1

    result["ls_signal"] = ls_signal.values
    result["sq_signal"] = sq_signal.values
    result["sr_signal"] = sr_signal.values
    result["buy_votes"] = buy_votes.values
    result["sell_votes"] = sell_votes.values

    return result
