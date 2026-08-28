
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

_EDGE_TOLERANCE_HOURS = 4


def _hl_adapter():
    here = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    hl_dir = os.path.join(here, "platforms", "hyperliquid")
    if hl_dir not in sys.path:
        sys.path.insert(0, hl_dir)
    from adapter import HyperliquidExchangeAdapter
    return HyperliquidExchangeAdapter()


def _to_utc_ms(value) -> int:
    ts = pd.Timestamp(value)
    ts = ts.tz_localize("UTC") if ts.tz is None else ts.tz_convert("UTC")
    return int(ts.timestamp() * 1000)


def load_cached_funding(coin: str,
                        start_date,
                        end_date=None,
                        exchange: str = "hyperliquid",
                        adapter=None,
                        db_path: Optional[str] = None) -> pd.DataFrame:
    start_ts = _to_utc_ms(start_date)
    end_ts = _to_utc_ms(end_date) if end_date is not None else int(time.time() * 1000)

    db_kwargs = {"db_path": db_path} if db_path else {}
    tol = _EDGE_TOLERANCE_HOURS * _HOUR_MS
    coverage = load_funding_coverage(exchange, coin, **db_kwargs)
    if any(s <= start_ts + tol and e >= end_ts - tol for s, e in coverage):
        return load_funding_rates(exchange, coin, start_ts, end_ts, **db_kwargs)

    if adapter is None:
        adapter = _hl_adapter()
    records = adapter.get_funding_history_range(coin, start_ts, end_ts)
    if records:
        store_funding_rates(records, exchange, coin, **db_kwargs)
        last_t = int(records[-1]["time"])
        covered_end = end_ts if last_t >= end_ts - tol else last_t
        store_funding_coverage(exchange, coin, start_ts, covered_end, **db_kwargs)
        return load_funding_rates(exchange, coin, start_ts, end_ts, **db_kwargs)
    return load_funding_rates(exchange, coin, start_ts, end_ts, **db_kwargs)


def attach_funding_column(df: pd.DataFrame, funding: pd.DataFrame) -> pd.DataFrame:
    out = df.copy()
    if funding is None or funding.empty or len(out) == 0:
        out["funding_rate"] = float("nan")
        return out

    bar_ts = pd.to_datetime(out.index)
    if bar_ts.tz is None:
        bar_ts = bar_ts.tz_localize("UTC")
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
    pos = np.searchsorted(ev_t, bt, side="right")
    cum_at_bar = np.where(pos > 0, ev_cum[np.clip(pos - 1, 0, len(ev_cum) - 1)], 0.0)

    accrual = np.zeros(len(bt), dtype=float)
    accrual[1:] = cum_at_bar[1:] - cum_at_bar[:-1]
    out["funding_accrual"] = accrual
    return out
