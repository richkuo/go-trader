"""
Funding-rate history for backtests (#960).

Hyperliquid is the funding source (hourly snapshots via the public
``funding_history`` API, paginated by the adapter's
``get_funding_history_range``); prices in backtests may come from a different
exchange (binanceus by default) — funding applies to the HL perp of the same
coin, which is the venue the live strategy trades.

Cached in the same SQLite database as OHLCV (``shared_tools/trading_bot.db``,
``funding_rates`` table) so repeat backtests don't refetch.
"""

import os
import sys
import time
from typing import Optional

import pandas as pd

from storage import load_funding_rates, store_funding_rates

_HOUR_MS = 3_600_000

# Refetch when the cache doesn't reach within this many hours of the
# requested range edges (funding is hourly; allow a small ragged edge).
_EDGE_TOLERANCE_HOURS = 4


def _hl_adapter():
    """Import the Hyperliquid adapter (sys.path dance per project convention —
    the HL SDK has a module-name clash, so the platform dir must be inserted
    before importing)."""
    here = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    hl_dir = os.path.join(here, "platforms", "hyperliquid")
    if hl_dir not in sys.path:
        sys.path.insert(0, hl_dir)
    from adapter import HyperliquidExchangeAdapter
    return HyperliquidExchangeAdapter()


def load_cached_funding(coin: str,
                        start_date: str,
                        end_date: Optional[str] = None,
                        exchange: str = "hyperliquid",
                        adapter=None,
                        db_path: Optional[str] = None) -> pd.DataFrame:
    """Load hourly funding for ``coin`` (e.g. ``"BTC"``) covering
    [start_date, end_date]; fetch from Hyperliquid and cache on a miss.

    Returns DataFrame(timestamp, rate) with a UTC DatetimeIndex (may be empty
    when the API has no data for the range — e.g. a coin listed later).
    """
    start_ts = int(pd.Timestamp(start_date, tz="UTC").timestamp() * 1000)
    end_ts = (int(pd.Timestamp(end_date, tz="UTC").timestamp() * 1000)
              if end_date else int(time.time() * 1000))

    db_kwargs = {"db_path": db_path} if db_path else {}
    cached = load_funding_rates(exchange, coin, start_ts, end_ts, **db_kwargs)
    tol = _EDGE_TOLERANCE_HOURS * _HOUR_MS
    if not cached.empty:
        covers_start = int(cached["timestamp"].iloc[0]) <= start_ts + tol
        covers_end = int(cached["timestamp"].iloc[-1]) >= end_ts - tol
        if covers_start and covers_end:
            return cached

    if adapter is None:
        adapter = _hl_adapter()
    records = adapter.get_funding_history_range(coin, start_ts, end_ts)
    if records:
        store_funding_rates(records, exchange, coin, **db_kwargs)
        return load_funding_rates(exchange, coin, start_ts, end_ts, **db_kwargs)
    # API returned nothing new — serve whatever the cache has (possibly empty).
    return cached


def attach_funding_column(df: pd.DataFrame, funding: pd.DataFrame) -> pd.DataFrame:
    """Attach a ``funding_rate`` column to an OHLCV frame.

    Each bar gets the most recent funding snapshot at or BEFORE the bar's
    timestamp (merge_asof backward) — never a future snapshot, preserving the
    look-ahead invariant. Bars before the first snapshot get NaN (strategies
    treat NaN funding as no-entry).
    """
    out = df.copy()
    if funding is None or funding.empty or len(out) == 0:
        out["funding_rate"] = float("nan")
        return out

    bar_ts = pd.to_datetime(out.index)
    if bar_ts.tz is None:
        bar_ts = bar_ts.tz_localize("UTC")
    # ns-normalize both keys: pandas >= 2 preserves source units (us vs ms)
    # and merge_asof requires identical dtypes.
    left = pd.DataFrame({"ts": bar_ts.tz_convert("UTC").astype("datetime64[ns, UTC]")})
    right = pd.DataFrame({
        "ts": pd.to_datetime(funding["timestamp"], unit="ms", utc=True)
              .astype("datetime64[ns, UTC]"),
        "funding_rate": funding["rate"].astype(float).values,
    }).sort_values("ts")
    merged = pd.merge_asof(left, right, on="ts", direction="backward")
    out["funding_rate"] = merged["funding_rate"].values
    return out
