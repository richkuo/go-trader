
import hashlib
import json
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
                        db_path: Optional[str] = None,
                        source_box: Optional[dict] = None) -> pd.DataFrame:
    start_ts = _to_utc_ms(start_date)
    end_ts = _to_utc_ms(end_date) if end_date is not None else int(time.time() * 1000)

    db_kwargs = {"db_path": db_path} if db_path else {}
    tol = _EDGE_TOLERANCE_HOURS * _HOUR_MS
    coverage = load_funding_coverage(exchange, coin, **db_kwargs)
    if any(s <= start_ts + tol and e >= end_ts - tol for s, e in coverage):
        if source_box is not None:
            source_box["source"] = "cache"
        return load_funding_rates(exchange, coin, start_ts, end_ts, **db_kwargs)

    if source_box is not None:
        source_box["source"] = "fetched"
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


FUNDING_RATE_STRATEGIES = {"funding_skew", "delta_neutral_funding"}
FUNDING_COLUMNS = (
    "funding_accrual",
    "funding_rate",
    "funding_missing_hours",
    "funding_mode",
    "funding_block_json",
)
FUNDING_MODES = ("charge", "partial", "off")


def _bar_open_ms(index) -> np.ndarray:
    ts = pd.DatetimeIndex(pd.to_datetime(index, utc=True))
    return (ts.as_unit("ns").asi8 // 1_000_000).astype(np.int64)


def _expected_hours(prev_ms: int, cur_ms: int) -> int:
    if cur_ms <= prev_ms:
        return 0
    first = (prev_ms // _HOUR_MS + 1) * _HOUR_MS
    last = (cur_ms // _HOUR_MS) * _HOUR_MS
    if last < first or first > cur_ms:
        return 0
    return int((last - first) // _HOUR_MS) + 1


def _attachment_hour(timestamp_ms: int) -> int:
    """Hour boundary this record fills.

    A print exactly on the hour fills that hour. A print a few milliseconds
    later fills the next hour, which is the bar searchsorted(side="right")
    books it on when bars sit on the hour.
    """
    timestamp_ms = int(timestamp_ms)
    if timestamp_ms % _HOUR_MS == 0:
        return timestamp_ms
    return (timestamp_ms // _HOUR_MS + 1) * _HOUR_MS


def _events_in_span(event_ms: np.ndarray, prev_ms: int, cur_ms: int) -> np.ndarray:
    if event_ms.size == 0 or cur_ms <= prev_ms:
        return event_ms[:0]
    lo = int(np.searchsorted(event_ms, prev_ms, side="right"))
    hi = int(np.searchsorted(event_ms, cur_ms, side="right"))
    return event_ms[lo:hi]


def right_closed_missing_hours(bar_ms: np.ndarray, event_ms: np.ndarray) -> np.ndarray:
    """Hours in (previous bar open, this bar open] with no funding record.

    Matches attach_funding_accrual_column: searchsorted(..., side="right")
    puts a funding time on the bar that contains it and not the next bar.
    The first bar attaches nothing.
    """
    missing = np.zeros(len(bar_ms), dtype=np.int64)
    events = np.unique(np.sort(event_ms.astype(np.int64))) if event_ms.size else event_ms
    for i in range(1, len(bar_ms)):
        prev = int(bar_ms[i - 1])
        cur = int(bar_ms[i])
        expected = _expected_hours(prev, cur)
        if expected == 0:
            continue
        present_hours = set()
        for t in _events_in_span(events, prev, cur):
            hour = _attachment_hour(int(t))
            if prev < hour <= cur:
                present_hours.add(hour)
        missing[i] = max(0, expected - len(present_hours))
    return missing


def _coverage_dict(bar_ms: np.ndarray, event_ms: np.ndarray, missing: np.ndarray) -> dict:
    if len(bar_ms) < 2:
        return {
            "expected_hours": 0,
            "present_hours": 0,
            "missing_hours": 0,
            "max_gap_hours": 0,
            "complete": True,
        }
    expected = 0
    present = 0
    gap = 0
    max_gap = 0
    events = np.unique(np.sort(event_ms.astype(np.int64))) if event_ms.size else event_ms
    for i in range(1, len(bar_ms)):
        prev = int(bar_ms[i - 1])
        cur = int(bar_ms[i])
        exp = _expected_hours(prev, cur)
        expected += exp
        got = set()
        for t in _events_in_span(events, prev, cur):
            hour = _attachment_hour(int(t))
            if prev < hour <= cur:
                got.add(hour)
        present += len(got)
        if exp:
            first = (prev // _HOUR_MS + 1) * _HOUR_MS
            for k in range(exp):
                hour = first + k * _HOUR_MS
                if hour in got:
                    max_gap = max(max_gap, gap)
                    gap = 0
                else:
                    gap += 1
    max_gap = max(max_gap, gap)
    missing_hours = int(missing[1:].sum()) if len(missing) else 0
    return {
        "expected_hours": int(expected),
        "present_hours": int(present),
        "missing_hours": missing_hours,
        "max_gap_hours": int(max_gap),
        "complete": bool(expected == present and missing_hours == 0),
    }


def funding_row_sha256(timestamps, rates) -> str:
    pairs = sorted((int(t), float(r)) for t, r in zip(timestamps, rates))
    payload = "\n".join(f"{t},{r:.12g}" for t, r in pairs).encode()
    return hashlib.sha256(payload).hexdigest()


def _iso_ms(ms: Optional[int]) -> Optional[str]:
    if ms is None:
        return None
    return pd.Timestamp(int(ms), unit="ms", tz="UTC").isoformat().replace("+00:00", "Z")


def _should_price(platform: str, strategy_type: str, strategy_name: str, mode: str) -> bool:
    hl_perps = (str(platform or "").strip().lower() == "hyperliquid"
                and str(strategy_type or "").strip().lower() == "perps")
    if mode == "off":
        return strategy_name == "delta_neutral_funding"
    if mode in ("charge", "partial"):
        return hl_perps or strategy_name == "delta_neutral_funding"
    raise ValueError(f"funding mode must be one of {FUNDING_MODES}, got {mode!r}")


def _stamp_columns(out: pd.DataFrame, mode: str, block: dict,
                   missing: Optional[np.ndarray] = None) -> pd.DataFrame:
    out = out.copy()
    out["funding_mode"] = mode
    out["funding_block_json"] = json.dumps(block, sort_keys=True, default=str)
    if missing is not None:
        out["funding_missing_hours"] = missing.astype(int)
    return out


def rejoin_funding_columns(signals: pd.DataFrame, funded: pd.DataFrame) -> pd.DataFrame:
    if signals is None or funded is None or len(signals) == 0:
        return signals
    missing = [col for col in FUNDING_COLUMNS if col in funded.columns and col not in signals.columns]
    if not missing:
        return signals
    out = signals.copy()
    for col in missing:
        aligned = funded[col].reindex(out.index)
        out[col] = aligned.to_numpy()
    return out


def continuous_history_start(df: pd.DataFrame, event_ms: np.ndarray):
    """First bar whose right-closed hours through the last bar all have records.

    The kept frame still starts on a bar, so that bar itself attaches nothing.
    Coverage required is every hour in (kept start, last bar].
    """
    if df is None or len(df) == 0:
        return None
    bar_ms = _bar_open_ms(df.index)
    missing = right_closed_missing_hours(bar_ms, event_ms)
    k = 0
    while k < len(df) and int(missing[k + 1:].sum()) > 0:
        k += 1
    return df.index[k]


def attach_backtest_funding(df: pd.DataFrame,
                            coin: str,
                            timeframe: Optional[str],
                            platform: str,
                            strategy_type: str,
                            strategy_name: str,
                            mode: str = "charge",
                            since=None,
                            adapter=None,
                            db_path: Optional[str] = None) -> tuple:
    """Attach funding for one non-manifest backtest frame.

    Returns (frame, funding block). Indicator strategies keep funding_rate,
    including an empty column when the fetch returns nothing. Accrual uses the
    existing right-closed sum. A failed fetch is available: false and does not
    write a zero accrual column.
    """
    del timeframe, since  # attachment span is the frame, not the request start
    if mode not in FUNDING_MODES:
        raise ValueError(f"funding mode must be one of {FUNDING_MODES}, got {mode!r}")
    if df is None or len(df) == 0:
        block = {"mode": mode, "available": False, "complete": True,
                 "coverage": _coverage_dict(np.array([]), np.array([]), np.array([])),
                 "source": "unavailable", "sha256": funding_row_sha256([], []),
                 "row_count": 0, "first": None, "last": None}
        return df, block

    keep_rate = strategy_name in FUNDING_RATE_STRATEGIES
    price = _should_price(platform, strategy_type, strategy_name, mode)
    out = df.copy()
    if not price and not keep_rate:
        block = {
            "mode": mode,
            "available": True,
            "complete": True,
            "coverage": _coverage_dict(_bar_open_ms(out.index), np.array([]),
                                       np.zeros(len(out), dtype=np.int64)),
            "source": None,
            "sha256": None,
            "row_count": 0,
            "first": None,
            "last": None,
            "priced": False,
        }
        if mode == "off":
            return _stamp_columns(out, mode, block), block
        return _stamp_columns(out, mode, block, np.zeros(len(out), dtype=np.int64)), block

    bar_ms = _bar_open_ms(out.index)
    first_ms = int(bar_ms[0])
    last_ms = int(bar_ms[-1])
    source_box = {}
    error = None
    loaded = None
    if last_ms > first_ms:
        try:
            loaded = load_cached_funding(
                coin,
                pd.Timestamp(first_ms + 1, unit="ms", tz="UTC"),
                end_date=pd.Timestamp(last_ms, unit="ms", tz="UTC"),
                adapter=adapter,
                db_path=db_path,
                source_box=source_box,
            )
        except Exception as exc:
            error = str(exc)
            loaded = None
    if keep_rate:
        out = attach_funding_column(out, loaded if error is None else None)

    event_ms = np.array([], dtype=np.int64)
    event_rate = np.array([], dtype=float)
    if error is None and loaded is not None and not loaded.empty and "timestamp" in loaded.columns:
        ts = loaded["timestamp"].to_numpy()
        rate = loaded["rate"].to_numpy(dtype=float)
        mask = (ts > first_ms) & (ts <= last_ms)
        event_ms = ts[mask].astype(np.int64)
        event_rate = rate[mask]
    missing = right_closed_missing_hours(bar_ms, event_ms)
    coverage = _coverage_dict(bar_ms, event_ms, missing)
    available = error is None and (not price or event_ms.size > 0 or coverage["expected_hours"] == 0)
    if price and error is None and event_ms.size:
        shaped = pd.DataFrame({"timestamp": event_ms, "rate": event_rate})
        out = attach_funding_accrual_column(out, shaped)
    block = {
        "mode": mode,
        "available": bool(available),
        "complete": bool(coverage["complete"] and available),
        "coverage": coverage,
        "source": "unavailable" if error is not None else (source_box.get("source") or "unavailable"),
        "sha256": funding_row_sha256(event_ms, event_rate),
        "row_count": int(event_ms.size),
        "first": _iso_ms(int(event_ms.min())) if event_ms.size else None,
        "last": _iso_ms(int(event_ms.max())) if event_ms.size else None,
        "priced": bool(price),
    }
    if error is not None:
        block["error"] = error
    return _stamp_columns(out, mode, block, missing), block
