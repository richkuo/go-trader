"""
Funding Skew — funding-rate crowding entries with price confirmation (#960).

Perpetual funding is a positioning gauge: strongly positive funding means
longs are crowded (longs pay shorts), strongly negative means shorts are
crowded. Extremes mark squeeze fuel — but funding alone is early, so every
entry also requires price to have started moving against the crowd:

    funding z-score <= -z_entry (crowded shorts)  AND close > EMA(confirm_ema)
        → long (short-squeeze continuation)
    funding z-score >= +z_entry (crowded longs)   AND close < EMA(confirm_ema)
        → short (long-squeeze breakdown), gated by ``allow_short``

The z-score is computed over a rolling window of bars against the funding
series itself, so "extreme" adapts to each coin's own funding regime; a
``min_abs_rate`` floor keeps quiet near-zero funding from registering as an
extreme. Exits: the crowding resolves (z returns inside ±z_exit) or the price
confirmation flips (close crosses the EMA against the position).

Funding input — two shapes, column preferred:
  * ``funding_rate`` column on the df (backtest path — attached by
    ``shared_tools/funding_fetcher.attach_funding_column``, merge_asof
    backward so a bar never sees a future snapshot), or
  * ``funding_records`` param: list of ``{"rate": float, "time": int(ms)}``
    (live path — ``check_hyperliquid.py`` fetches the paginated history and
    passes it through; aligned here with the same backward-only rule).
NaN / missing funding bars are fail-safe: no entry can fire and a held
position exits.

Anti-look-ahead: funding alignment is backward-only; z-score, EMA and the
position recursion are trailing. Signal at bar N fills at bar N+1 open.
Emits ``signal`` in {-1, 0, 1} as position transitions (flip clamped like
``tema_cross_bd``).
"""

import numpy as np
import pandas as pd


def _align_funding_records(index: pd.Index, records: list) -> pd.Series:
    """Map {rate, time(ms)} records onto a bar index, backward-only."""
    bar_ts = pd.to_datetime(index)
    try:
        if bar_ts.tz is None:
            bar_ts = bar_ts.tz_localize("UTC")
    except (AttributeError, TypeError):
        return pd.Series(np.nan, index=index)
    # Normalize both keys to ns resolution — pandas >= 2 preserves the source
    # unit (us vs ms) and merge_asof requires identical dtypes.
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
    """Generate funding-crowding signals with price confirmation.

    Parameters
    ----------
    df : DataFrame with open, high, low, close, volume columns; a
        ``funding_rate`` column is used when present (else funding_records)
    funding_window : rolling bars for the funding mean/std behind the z-score
    z_entry : |z| at or beyond which funding counts as a crowding extreme
    z_exit : |z| at or inside which the crowding is considered resolved
    confirm_ema : EMA period for the price-confirmation gate
    min_abs_rate : |funding| floor — below it no extreme is recognized
        (HL hourly funding baseline is ~1e-5/h; a z-spike inside that noise
        band is not a crowd)
    allow_short : permit crowded-long breakdown shorts (perps strategy —
        default True; the long side alone is half the design)
    funding_records : live-path funding history (list of {rate, time(ms)})

    Returns
    -------
    DataFrame with added columns:
        signal       : 1 / -1 / 0 position transitions
        position     : held state per bar (1 long, -1 short, 0 flat)
        funding_rate : per-bar aligned funding (NaN where unknown)
        funding_z    : rolling z-score of funding (NaN during warmup)
    """
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
        # Exits: missing funding (fail-safe), crowding resolved, or the
        # price confirmation flipping against the position.
        if cur == 1 and (not z_ok or zv[i] >= -z_exit or below[i]):
            cur = 0
        elif cur == -1 and (not z_ok or zv[i] <= z_exit or above[i]):
            cur = 0
        # Entries.
        if z_ok and not np.isnan(fv[i]):
            if zv[i] <= -z_entry and fv[i] < -min_abs_rate and above[i]:
                cur = 1
            elif allow_short and zv[i] >= z_entry and fv[i] > min_abs_rate and below[i]:
                cur = -1
        pos[i] = cur

    result["position"] = pos
    # A direct flip yields diff == ±2; clamp so downstream sees {-1, 0, 1}.
    result["signal"] = (
        pd.Series(pos, index=result.index).diff().fillna(0).clip(-1, 1).astype(int)
    )
    return result
