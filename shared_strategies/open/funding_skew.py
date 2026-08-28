
import numpy as np
import pandas as pd


def _align_funding_records(index: pd.Index, records: list) -> pd.Series:
    bar_ts = pd.to_datetime(index)
    try:
        if bar_ts.tz is None:
            bar_ts = bar_ts.tz_localize("UTC")
    except (AttributeError, TypeError):
        return pd.Series(np.nan, index=index)
    left = pd.DataFrame({"ts": bar_ts.tz_convert("UTC").astype("datetime64[ns, UTC]")})
    right = pd.DataFrame({
        "ts": pd.to_datetime([int(r["time"]) for r in records], unit="ms", utc=True)
              .astype("datetime64[ns, UTC]"),
        "rate": [float(r["rate"]) for r in records],
    }).sort_values("ts")
    merged = pd.merge_asof(left, right, on="ts", direction="backward")
    return pd.Series(merged["rate"].values, index=index)


def funding_skew_core(
    df: pd.DataFrame,
    funding_window: int = 168,
    z_entry: float = 2.0,
    z_exit: float = 0.5,
    confirm_ema: int = 40,
    min_abs_rate: float = 0.00001,
    allow_short: bool = True,
    funding_records: list = None,
) -> pd.DataFrame:
    result = df.copy()
    n = len(result)
    result["signal"] = 0
    result["position"] = 0
    if "funding_rate" not in result.columns:
        if funding_records:
            result["funding_rate"] = _align_funding_records(result.index, funding_records)
        else:
            result["funding_rate"] = np.nan
    result["funding_z"] = np.nan
    if n == 0:
        return result

    funding = result["funding_rate"].astype(float)
    mean = funding.rolling(window=funding_window).mean()
    std = funding.rolling(window=funding_window).std()
    z = ((funding - mean) / std).where(std > 0)
    result["funding_z"] = z

    close = result["close"].astype(float)
    ema = close.ewm(span=confirm_ema, adjust=False).mean()
    above = (close > ema).to_numpy()
    below = (close < ema).to_numpy()

    zv = z.to_numpy()
    fv = funding.to_numpy()
    pos = np.zeros(n, dtype=np.int64)
    for i in range(1, n):
        cur = pos[i - 1]
        z_ok = not np.isnan(zv[i])
        if cur == 1 and (not z_ok or zv[i] >= -z_exit or below[i]):
            cur = 0
        elif cur == -1 and (not z_ok or zv[i] <= z_exit or above[i]):
            cur = 0
        if z_ok and not np.isnan(fv[i]):
            if zv[i] <= -z_entry and fv[i] < -min_abs_rate and above[i]:
                cur = 1
            elif allow_short and zv[i] >= z_entry and fv[i] > min_abs_rate and below[i]:
                cur = -1
        pos[i] = cur

    result["position"] = pos
    result["signal"] = (
        pd.Series(pos, index=result.index).diff().fillna(0).clip(-1, 1).astype(int)
    )
    return result
