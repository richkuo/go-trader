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

import numpy as np
import pandas as pd

from storage import (
    load_funding_coverage,
    load_funding_rates,
    store_funding_coverage,
    store_funding_rates,
)

_HOUR_MS = 3_600_000

# Refetch when recorded coverage doesn't reach within this many hours of the
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


def _to_utc_ms(value) -> int:
    """Date string / Timestamp / datetime → Unix ms (naive read as UTC)."""
    ts = pd.Timestamp(value)
    ts = ts.tz_localize("UTC") if ts.tz is None else ts.tz_convert("UTC")
    return int(ts.timestamp() * 1000)


def load_cached_funding(coin: str,
                        start_date,
                        end_date=None,
                        exchange: str = "hyperliquid",
                        adapter=None,
                        db_path: Optional[str] = None) -> pd.DataFrame:
    """Load hourly funding for ``coin`` (e.g. ``"BTC"``) covering
    [start_date, end_date]; fetch from Hyperliquid and cache on a miss.
    Dates accept strings or Timestamps; ``end_date=None`` means now (callers
    with a known window end — e.g. a backtest's last bar — should pass it so
    a repeat run is a cache hit regardless of elapsed wall-clock time).

    Cache hits are decided by the ``funding_coverage`` ledger (the range
    already fetched from the API), not by the stored rates: a coin listed
    mid-range legitimately has no rates near the range start, and only the
    coverage row distinguishes "nothing exists to fetch" from "never fetched".

    Returns DataFrame(timestamp, rate) with a UTC DatetimeIndex (may be empty
    when the API has no data for the range — e.g. a coin listed later).
    """
    start_ts = _to_utc_ms(start_date)
    end_ts = _to_utc_ms(end_date) if end_date is not None else int(time.time() * 1000)

    db_kwargs = {"db_path": db_path} if db_path else {}
    tol = _EDGE_TOLERANCE_HOURS * _HOUR_MS
    coverage = load_funding_coverage(exchange, coin, **db_kwargs)
    if coverage and coverage[0] <= start_ts + tol and coverage[1] >= end_ts - tol:
        return load_funding_rates(exchange, coin, start_ts, end_ts, **db_kwargs)

    if adapter is None:
        adapter = _hl_adapter()
    records = adapter.get_funding_history_range(coin, start_ts, end_ts)
    if records:
        store_funding_rates(records, exchange, coin, **db_kwargs)
        # Coverage start is the requested start even when the first record is
        # later (coin listed mid-range — nothing earlier exists). Coverage end
        # claims the requested end only when records actually reach near it;
        # a pagination that died early must not mark the tail as covered.
        last_t = int(records[-1]["time"])
        covered_end = end_ts if last_t >= end_ts - tol else last_t
        store_funding_coverage(exchange, coin, start_ts, covered_end, **db_kwargs)
        return load_funding_rates(exchange, coin, start_ts, end_ts, **db_kwargs)
    # API returned nothing (range before listing, or a transient failure —
    # indistinguishable here, so record no coverage and retry next run).
    return load_funding_rates(exchange, coin, start_ts, end_ts, **db_kwargs)


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


def attach_funding_accrual_column(df: pd.DataFrame, funding: pd.DataFrame) -> pd.DataFrame:
    """Attach a ``funding_accrual`` column: the TOTAL funding rate accrued over
    each bar's holding interval — the sum of the hourly funding snapshots in
    ``(previous_bar, this_bar]`` (#988).

    This is distinct from ``attach_funding_column``: that returns a
    point-in-time snapshot used as a *signal* input (the current rate level);
    this returns the per-bar *carry* to BOOK against a held position. It is
    timeframe-correct — a 4h bar sums the ~4 hourly funding events inside it
    where the snapshot would capture only one. Purely backward-looking (a bar's
    accrual covers the closed interval ending at the bar), preserving the
    look-ahead invariant. The first bar (and any bar before the first funding
    event) accrues 0.0.
    """
    out = df.copy()
    if funding is None or funding.empty or len(out) == 0:
        out["funding_accrual"] = 0.0
        return out

    bar_ts = pd.to_datetime(out.index)
    if bar_ts.tz is None:
        bar_ts = bar_ts.tz_localize("UTC")
    bar_ts = bar_ts.tz_convert("UTC")

    f_ts = pd.to_datetime(funding["timestamp"], unit="ms", utc=True)
    order = np.argsort(f_ts.values)
    ev_t = f_ts.values[order]
    ev_cum = np.cumsum(funding["rate"].astype(float).values[order])

    bt = bar_ts.values
    # Count of funding events at or before each bar (right-closed interval),
    # then the cumulative funding up to that point.
    pos = np.searchsorted(ev_t, bt, side="right")
    cum_at_bar = np.where(pos > 0, ev_cum[np.clip(pos - 1, 0, len(ev_cum) - 1)], 0.0)

    accrual = np.zeros(len(bt), dtype=float)
    accrual[1:] = cum_at_bar[1:] - cum_at_bar[:-1]
    out["funding_accrual"] = accrual
    return out
