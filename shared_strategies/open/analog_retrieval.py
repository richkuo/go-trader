"""Analog-retrieval forecasting strategy (#1138) — OFFLINE RESEARCH ONLY.

Do NOT wire this strategy to live until it clears parity and Sharpe checks —
and even then, promotion to live requires explicit human sign-off (#1138's
human promotion gate). It is registered ``backtest_only=True`` and every live
check script refuses to evaluate it; it exists solely for
``backtest/run_backtest.py`` and the M1 harness (``backtest/eval_windows.py``).

Idea (transferable, LLM-free core of arXiv 2502.05878): at each bar, encode
the market state into a small scale-free feature vector, retrieve the K most
similar *prior* states whose forward return is already realized, and vote
their forward returns into a direction signal gated by the retrieved sample's
dispersion (a t-statistic) and by minimum edge measured in ATR units.

Leakage invariant (the crux — see the issue's "Critical correctness
requirement"): the analog index at bar t may only contain bars j with
``j + horizon <= t`` — the neighbor's forward-return window [j+1, j+horizon]
must have fully closed by t. Feature normalization uses expanding
(prefix-only) statistics for the same reason. The Backtester's output-column
``shift(1)`` invariants cannot guard strategy-internal look-ahead, so this
module structurally slices every per-bar computation to the eligible prefix,
and ``test_analog_retrieval.py`` carries a prefix-consistency regression test
(the analog of ``backtest/tests/test_backtester_lookahead.py``).

Complexity note: retrieval is O(n · min(n, max_index) · d) — fine for research
backtests (tens of thousands of bars), another reason this must never run on a
live check path.

ATR comes from the shared open-tree module ``indicators_core`` (#1281) —
importable at module load without shared_tools on sys.path.
"""

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
    """Per-bar scale-free state vector from indicators the repo already uses.

    Every column at row t is a function of bars <= t only (backward-looking
    rolling/EMA ops) — the encoder itself can never leak.

    Columns
    -------
    ret_eff   : signed return efficiency over ``feat_window`` — net move
                divided by the path length (sum of |close diffs|), in [-1, 1].
                The regime classifier's return_eff concept, kept signed.
    mom_atr   : momentum normalized by volatility —
                (close_t - close_{t-W}) / (ATR_t * sqrt(W)).
    atr_pct   : ATR / close (per-bar volatility as a fraction of price).
    vol_ratio : atr_pct over its ``vol_baseline``-bar rolling mean — the
                volatility-regime feature (>1 hot, <1 quiet).
    trend_atr : (EMA(W) - EMA(4W)) / ATR — trend state in ATR units.
    """
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
    """Realized forward pct return over ``horizon`` bars: close[t+h]/close[t]-1.

    Row j reads ``horizon`` bars of FUTURE data relative to j — it is only
    knowable at bar j + horizon, so callers must never read row j before the
    eligibility cut ``j + horizon <= t`` (enforced in analog_retrieval_core).
    """
    close = close.astype(float)
    return close.shift(-horizon) / close - 1.0


def retrieve_neighbors(
    index_matrix: np.ndarray, query: np.ndarray, k: int
) -> np.ndarray:
    """Row indices of the ``k`` nearest rows of ``index_matrix`` to ``query``.

    Euclidean distance on already-normalized features; stable argsort so ties
    break by row order deterministically (prefix-stability for the leakage
    regression test).
    """
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
    """k-NN analog-retrieval direction signals over a walk-forward index.

    Parameters
    ----------
    df : OHLCV DataFrame (open, high, low, close, volume).
    feat_window : lookback for return efficiency / momentum / fast EMA.
    atr_period : lookback for the inline ATR.
    vol_baseline : lookback for the volatility-regime baseline.
    horizon : forward-return horizon in bars. Also the leakage cut — bar j
        enters the index only once j + horizon <= t.
    k_neighbors : neighbors retrieved per bar.
    min_index : minimum eligible index size before any signal may fire.
    max_index : cap on index size — only the most recent ``max_index``
        eligible bars are searched (0 = unbounded). Bounds compute and lets
        the analog set adapt to regime drift.
    min_t_stat : dispersion gate — |mean| / (std / sqrt(k)) of the neighbors'
        forward returns must reach this.
    min_edge_atr : edge gate — |mean forward return| must reach
        min_edge_atr * atr_pct_t * sqrt(horizon) (a fraction of the typical
        horizon-length ATR move at current volatility).

    Returns
    -------
    DataFrame with added columns:
        signal          : +1 long, -1 short, 0 no edge
        analog_mean_fwd : mean forward return of the retrieved neighbors (NaN
                          when no retrieval ran)
        analog_t_stat   : the dispersion statistic (NaN when no retrieval ran)
        analog_k        : neighbors actually used (0 when no retrieval ran)
        atr             : inline ATR
    """
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

    # Expanding (prefix-only) normalization stats: mean/std at row t summarize
    # feature rows <= t, so normalizing with row t's stats cannot leak.
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
        # LEAKAGE GUARD: only bars j <= t - horizon have a forward return
        # realized by t. Everything below reads the [0, elig_end) prefix only.
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
