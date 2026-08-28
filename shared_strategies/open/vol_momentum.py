
import numpy as np
import pandas as pd

from indicators_core import atr_sma_series


def vol_momentum_core(
    df: pd.DataFrame,
    mom_window: int = 24,
    atr_period: int = 14,
    entry_threshold: float = 0.30,
    exit_threshold: float = 0.05,
    eff_entry: float = 0.35,
    eff_exit: float = 0.15,
    allow_short: bool = False,
) -> pd.DataFrame:
    result = df.copy()
    n = len(result)

    close = result["close"].astype(float)
    high = result["high"].astype(float)
    low = result["low"].astype(float)

    result["atr"] = atr_sma_series(high, low, close, atr_period)

    net = close - close.shift(mom_window)
    denom = result["atr"] * float(mom_window)
    vol_mom = (net / denom).where(denom > 0, 0.0).fillna(0.0)
    path = close.diff().abs().rolling(window=mom_window).sum()
    efficiency = (net.abs() / path).where(path > 0, 0.0).fillna(0.0)

    result["vol_mom"] = vol_mom
    result["efficiency"] = efficiency

    m = vol_mom.to_numpy()
    e = efficiency.to_numpy()
    pos = np.zeros(n, dtype=np.int64)
    for i in range(1, n):
        cur = pos[i - 1]
        if cur == 1 and (m[i] < exit_threshold or e[i] < eff_exit):
            cur = 0
        elif cur == -1 and (m[i] > -exit_threshold or e[i] < eff_exit):
            cur = 0
        if e[i] >= eff_entry:
            if m[i] > entry_threshold:
                cur = 1
            elif allow_short and m[i] < -entry_threshold:
                cur = -1
        pos[i] = cur

    result["position"] = pos
    result["signal"] = (
        pd.Series(pos, index=result.index).diff().fillna(0).clip(-1, 1).astype(int)
    )
    return result
