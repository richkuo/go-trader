
from __future__ import annotations

import numpy as np
import pandas as pd

from indicators_core import atr_sma

FEATURE_COLUMNS = ("ret_eff", "mom_atr", "atr_pct", "vol_ratio", "trend_atr")


def encode_features(
    df: pd.DataFrame,
    feat_window: int = 20,
    atr_period: int = 14,
    vol_baseline: int = 100,
) -> pd.DataFrame:
    close = df["close"].astype(float)
    atr = atr_sma(df, atr_period)

    net_move = close - close.shift(feat_window)
    path = close.diff().abs().rolling(window=feat_window).sum()
    ret_eff = (net_move / path).where(path > 0, 0.0)

    mom_atr = net_move / (atr * np.sqrt(feat_window))

    atr_pct = atr / close
    vol_ratio = atr_pct / atr_pct.rolling(window=vol_baseline).mean()

    ema_fast = close.ewm(span=feat_window, adjust=False).mean()
    ema_slow = close.ewm(span=4 * feat_window, adjust=False).mean()
    trend_atr = (ema_fast - ema_slow) / atr

    feats = pd.DataFrame(
        {
            "ret_eff": ret_eff,
            "mom_atr": mom_atr,
            "atr_pct": atr_pct,
            "vol_ratio": vol_ratio,
            "trend_atr": trend_atr,
        },
        index=df.index,
    )
    return feats.replace([np.inf, -np.inf], np.nan)


def forward_returns(close: pd.Series, horizon: int) -> pd.Series:
    close = close.astype(float)
    return close.shift(-horizon) / close - 1.0


def retrieve_neighbors(
    index_matrix: np.ndarray, query: np.ndarray, k: int
) -> np.ndarray:
    if index_matrix.shape[0] == 0 or k <= 0:
        return np.empty(0, dtype=int)
    dist = np.sqrt(((index_matrix - query) ** 2).sum(axis=1))
    order = np.argsort(dist, kind="stable")
    return order[: min(k, len(order))]


def analog_retrieval_core(
    df: pd.DataFrame,
    feat_window: int = 20,
    atr_period: int = 14,
    vol_baseline: int = 100,
    horizon: int = 12,
    k_neighbors: int = 25,
    min_index: int = 200,
    max_index: int = 5000,
    min_t_stat: float = 2.0,
    min_edge_atr: float = 0.25,
) -> pd.DataFrame:
    result = df.copy()
    n = len(result)
    result["signal"] = 0
    result["analog_mean_fwd"] = np.nan
    result["analog_t_stat"] = np.nan
    result["analog_k"] = 0
    result["atr"] = atr_sma(result, atr_period)
    if n == 0:
        return result

    feats = encode_features(
        result, feat_window=feat_window, atr_period=atr_period,
        vol_baseline=vol_baseline,
    )
    fwd = forward_returns(result["close"], horizon).to_numpy()
    fmat = feats.to_numpy(dtype=float)
    feat_ok = ~np.isnan(fmat).any(axis=1)

    exp_mean = feats.expanding(min_periods=2).mean().to_numpy(dtype=float)
    exp_std = feats.expanding(min_periods=2).std().to_numpy(dtype=float)

    signal = result["signal"].to_numpy(copy=True)
    mean_col = result["analog_mean_fwd"].to_numpy(copy=True)
    tstat_col = result["analog_t_stat"].to_numpy(copy=True)
    k_col = result["analog_k"].to_numpy(copy=True)
    atr_pct_all = fmat[:, FEATURE_COLUMNS.index("atr_pct")]

    for t in range(n):
        if not feat_ok[t]:
            continue
        elig_end = t - horizon + 1
        if elig_end <= 0:
            continue
        elig = np.flatnonzero(feat_ok[:elig_end])
        if len(elig) < max(min_index, 1):
            continue
        if max_index and len(elig) > max_index:
            elig = elig[-max_index:]

        mu = exp_mean[t]
        sd = exp_std[t]
        if np.isnan(mu).any() or np.isnan(sd).any():
            continue
        sd = np.where(sd > 0, sd, 1.0)

        zindex = (fmat[elig] - mu) / sd
        zquery = (fmat[t] - mu) / sd
        nbr = retrieve_neighbors(zindex, zquery, k_neighbors)
        if len(nbr) == 0:
            continue
        nbr_fwd = fwd[elig[nbr]]
        nbr_fwd = nbr_fwd[~np.isnan(nbr_fwd)]
        k = len(nbr_fwd)
        if k < 2:
            continue

        m = float(nbr_fwd.mean())
        s = float(nbr_fwd.std(ddof=1))
        t_stat = np.inf * np.sign(m) if s == 0 else m / (s / np.sqrt(k))
        mean_col[t] = m
        tstat_col[t] = t_stat
        k_col[t] = k

        edge_floor = min_edge_atr * atr_pct_all[t] * np.sqrt(horizon)
        if abs(m) >= edge_floor and abs(t_stat) >= min_t_stat:
            signal[t] = 1 if m > 0 else -1

    result["signal"] = signal
    result["analog_mean_fwd"] = mean_col
    result["analog_t_stat"] = tstat_col
    result["analog_k"] = k_col
    return result
