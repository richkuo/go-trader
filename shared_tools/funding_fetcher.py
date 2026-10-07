
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
    load_funding_first_ts,
    load_funding_rates,
    load_funding_venue_gaps,
    store_funding_fetch,
)

_HOUR_MS = 3_600_000

_EDGE_TOLERANCE_HOURS = 4

_FINALIZE_MARGIN_MS = _HOUR_MS


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


def _event_times(frame) -> np.ndarray:
    if frame is None or frame.empty or "timestamp" not in frame.columns:
        return np.array([], dtype=np.int64)
    return frame["timestamp"].to_numpy(dtype=np.int64)


def _missing_hour_buckets(start_ts: int, end_ts: int, event_ms, known=()) -> list:
    events = np.asarray(event_ms, dtype=np.int64)
    if events.size:
        events = events[(events >= start_ts) & (events <= end_ts)]
    offset = _print_offset_ms(events)
    expected = _expected_print_times(start_ts - 1, end_ts, offset)
    present = {_hour_bucket(int(t)) for t in events.tolist()}
    present.update(int(h) for h in known)
    out = []
    for t in expected.tolist():
        hour = int(t) - offset
        if hour not in present:
            out.append(hour)
    return out


def _hour_finalized(hour_ms: int, now_ms: int) -> bool:
    return hour_ms + _HOUR_MS <= now_ms - _FINALIZE_MARGIN_MS


def _contiguous_hour_runs(hours) -> list:
    runs = []
    for hour in sorted(set(int(h) for h in hours)):
        if runs and hour == runs[-1][-1] + _HOUR_MS:
            runs[-1].append(hour)
        else:
            runs.append([hour])
    return runs


def _confirmed_venue_absent(candidates, records, observed_through_ms,
                            window_start_ms: int, window_end_ms: int,
                            prior_first_ms: Optional[int], now_ms: int) -> list:
    if observed_through_ms is None:
        return []
    times = sorted(int(r["time"]) for r in records)
    real = {_hour_bucket(t) for t in times}
    earliest = times[0] if times else None
    if prior_first_ms is not None:
        earliest = prior_first_ms if earliest is None else min(earliest, int(prior_first_ms))
    out = []
    for hour in sorted(set(int(h) for h in candidates)):
        if hour % _HOUR_MS != 0 or hour in real:
            continue
        if hour < window_start_ms or hour + _HOUR_MS - 1 > window_end_ms:
            continue
        if int(observed_through_ms) < hour + _HOUR_MS:
            continue
        if earliest is None or earliest >= hour:
            continue
        if not _hour_finalized(hour, now_ms):
            continue
        out.append(hour)
    return out


def _strict_window(adapter):
    fn = getattr(adapter, "get_funding_history_window", None)
    return fn if callable(fn) else None


def _strict_download(fn, coin: str, start_ms: int, end_ms: int):
    result = fn(coin, int(start_ms), int(end_ms))
    if not isinstance(result, dict):
        raise ValueError(f"strict funding download returned {type(result).__name__}")
    records = result.get("records")
    if not isinstance(records, list):
        raise ValueError("strict funding download returned no records list")
    observed = result.get("observed_through_ms")
    return records, (int(observed) if observed is not None else None)


def _refill_missing_hours(coin: str, start_ts: int, end_ts: int, exchange: str,
                          adapter, db_kwargs: dict, errors: list, now_ms: int):
    stored = load_funding_rates(exchange, coin, start_ts, end_ts, **db_kwargs)
    marked = load_funding_venue_gaps(exchange, coin, _hour_bucket(start_ts), end_ts, **db_kwargs)
    missing = [h for h in _missing_hour_buckets(start_ts, end_ts, _event_times(stored), marked)
               if _hour_finalized(h, now_ms)]
    if not missing:
        return
    if adapter is None:
        try:
            adapter = _hl_adapter()
        except Exception as exc:
            errors.append(f"{coin} funding refill adapter: {exc}")
            return
    strict = _strict_window(adapter)
    for run in _contiguous_hour_runs(missing):
        ws = run[0]
        we = run[-1] + _HOUR_MS - 1
        try:
            if strict is not None:
                records, observed = _strict_download(strict, coin, ws, we)
            else:
                records = adapter.get_funding_history_range(coin, ws, we) or []
                observed = None
        except Exception as exc:
            errors.append(f"{coin} funding refill {_iso_ms(ws)}..{_iso_ms(we)}: {exc}")
            continue
        absent = []
        if strict is not None:
            absent = _confirmed_venue_absent(
                run, records, observed, ws, we,
                load_funding_first_ts(exchange, coin, **db_kwargs), now_ms)
        if records or absent:
            store_funding_fetch(exchange, coin, records, absent,
                                observed_through_ms=observed, **db_kwargs)


def load_cached_funding(coin: str,
                        start_date,
                        end_date=None,
                        exchange: str = "hyperliquid",
                        adapter=None,
                        db_path: Optional[str] = None,
                        source_box: Optional[dict] = None) -> pd.DataFrame:
    now_ms = int(time.time() * 1000)
    start_ts = _to_utc_ms(start_date)
    end_ts = _to_utc_ms(end_date) if end_date is not None else now_ms

    db_kwargs = {"db_path": db_path} if db_path else {}
    tol = _EDGE_TOLERANCE_HOURS * _HOUR_MS
    errors = []
    coverage = load_funding_coverage(exchange, coin, **db_kwargs)
    if any(s <= start_ts + tol and e >= end_ts - tol for s, e in coverage):
        if source_box is not None:
            source_box["source"] = "cache"
        _refill_missing_hours(coin, start_ts, end_ts, exchange, adapter,
                              db_kwargs, errors, now_ms)
    else:
        if source_box is not None:
            source_box["source"] = "fetched"
        if adapter is None:
            adapter = _hl_adapter()
        strict = _strict_window(adapter)
        if strict is None:
            records = adapter.get_funding_history_range(coin, start_ts, end_ts)
            if records:
                last_t = int(records[-1]["time"])
                covered_end = end_ts if last_t >= end_ts - tol else last_t
                store_funding_fetch(exchange, coin, records, [],
                                    coverage=(start_ts, covered_end), **db_kwargs)
        else:
            try:
                records, observed = _strict_download(strict, coin, start_ts, end_ts)
            except Exception as exc:
                errors.append(f"{coin} funding fetch {_iso_ms(start_ts)}..{_iso_ms(end_ts)}: {exc}")
                records, observed = [], None
            if records:
                last_t = int(records[-1]["time"])
                covered_end = end_ts if last_t >= end_ts - tol else last_t
                times = np.array([int(r["time"]) for r in records], dtype=np.int64)
                absent = _confirmed_venue_absent(
                    _missing_hour_buckets(start_ts, end_ts, times),
                    records, observed, start_ts, end_ts,
                    load_funding_first_ts(exchange, coin, **db_kwargs), now_ms)
                store_funding_fetch(exchange, coin, records, absent,
                                    coverage=(start_ts, covered_end),
                                    observed_through_ms=observed, **db_kwargs)
    if source_box is not None:
        source_box["venue_absent_hours"] = load_funding_venue_gaps(
            exchange, coin, _hour_bucket(start_ts), end_ts, **db_kwargs)
        if errors:
            source_box["fetch_error"] = "; ".join(errors)
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


def _hour_bucket(timestamp_ms: int) -> int:
    """Floor a print to the hour it settles.

    A print exactly on the hour fills that hour. A print a few milliseconds
    later fills the same hour. The bar that receives it is the bar whose
    span contains the print, which attach_funding_accrual_column selects
    with searchsorted(..., side="right").
    """
    return (int(timestamp_ms) // _HOUR_MS) * _HOUR_MS


def _print_offset_ms(event_ms: np.ndarray) -> int:
    """Millisecond offset shared by the prints in this window.

    An empty window uses the hour boundary. A mixed window uses the most
    common offset, and the smaller offset when two offsets tie, so a missing
    hour is placed where this series' prints sit.
    """
    if event_ms.size == 0:
        return 0
    offsets = event_ms.astype(np.int64) % np.int64(_HOUR_MS)
    values, counts = np.unique(offsets, return_counts=True)
    best = int(counts.max())
    return int(values[counts == best].min())


def _expected_print_times(first_ms: int, last_ms: int, offset_ms: int) -> np.ndarray:
    """Print times of each hour whose settlement falls in (first, last]."""
    if last_ms <= first_ms:
        return np.array([], dtype=np.int64)
    start = int(first_ms) - int(offset_ms) + 1
    hour = ((start + _HOUR_MS - 1) // _HOUR_MS) * _HOUR_MS
    times = []
    while hour + offset_ms <= last_ms:
        times.append(hour + offset_ms)
        hour += _HOUR_MS
    if not times:
        return np.array([], dtype=np.int64)
    return np.asarray(times, dtype=np.int64)


def _hour_coverage(bar_ms: np.ndarray, event_ms: np.ndarray, absent_hours=()):
    missing, coverage, _absent_used = _hour_coverage_detail(bar_ms, event_ms, absent_hours)
    return missing, coverage


def _hour_coverage_detail(bar_ms: np.ndarray, event_ms: np.ndarray, absent_hours=()):
    """Per-bar missing hours and the window coverage.

    Each hour is present when any in-window print floors to it. The hour is
    counted on the bar that would be charged for a print at that hour plus
    this series' offset, the same bar searchsorted(side="right") charges.
    """
    bars = np.asarray(bar_ms, dtype=np.int64)
    missing = np.zeros(len(bars), dtype=np.int64)
    empty = {
        "expected_hours": 0,
        "present_hours": 0,
        "missing_hours": 0,
        "max_gap_hours": 0,
        "venue_absent_hours": 0,
        "complete": True,
    }
    if len(bars) < 2:
        return missing, empty, []
    first = int(bars[0])
    last = int(bars[-1])
    events = np.asarray(event_ms, dtype=np.int64) if np.size(event_ms) else np.array([], dtype=np.int64)
    if events.size:
        events = events[(events > first) & (events <= last)]
    offset = _print_offset_ms(events)
    expected_times = _expected_print_times(first, last, offset)
    expected_hours = expected_times - np.int64(offset) if expected_times.size else expected_times
    real = {_hour_bucket(int(t)) for t in events}
    present = real | {int(h) for h in absent_hours}
    if expected_times.size:
        idx = np.searchsorted(bars, expected_times, side="left")
        ok = (idx > 0) & (idx < len(bars))
        idx = idx[ok]
        times = expected_times[ok]
        hours = expected_hours[ok]
        if idx.size:
            prev = bars[idx - 1]
            cur = bars[idx]
            inside = (prev < times) & (times <= cur)
            idx = idx[inside]
            hours = hours[inside]
        for bar_i, hour in zip(idx.tolist(), hours.tolist()):
            if int(hour) not in present:
                missing[int(bar_i)] += 1
    gap = 0
    max_gap = 0
    present_expected = 0
    for hour in expected_hours.tolist():
        if int(hour) in present:
            present_expected += 1
            max_gap = max(max_gap, gap)
            gap = 0
        else:
            gap += 1
    max_gap = max(max_gap, gap)
    expected_n = int(expected_hours.size)
    missing_hours = int(missing.sum())
    absent_used = sorted(int(h) for h in expected_hours.tolist()
                         if int(h) in present and int(h) not in real)
    return missing, {
        "expected_hours": expected_n,
        "present_hours": int(present_expected),
        "missing_hours": missing_hours,
        "max_gap_hours": int(max_gap),
        "venue_absent_hours": len(absent_used),
        "complete": bool(expected_n == present_expected and missing_hours == 0),
    }, absent_used


def right_closed_missing_hours(bar_ms: np.ndarray, event_ms: np.ndarray,
                               absent_hours=()) -> np.ndarray:
    """Missing funding hours on the bar the accrual would charge.

    The first bar attaches nothing. A bar's count is the number of hours
    whose print searchsorted(side="right") would charge on that bar and
    that have no record.
    """
    missing, _coverage = _hour_coverage(bar_ms, event_ms, absent_hours)
    return missing


def _coverage_dict(bar_ms: np.ndarray, event_ms: np.ndarray, missing: np.ndarray,
                   absent_hours=()) -> dict:
    del missing  # the coverage is recomputed with the missing hours
    _missing, coverage = _hour_coverage(bar_ms, event_ms, absent_hours)
    return coverage


def funding_row_sha256(timestamps, rates) -> str:
    pairs = sorted((int(t), float(r)) for t, r in zip(timestamps, rates))
    payload = "\n".join(f"{t},{r:.12g}" for t, r in pairs).encode()
    return hashlib.sha256(payload).hexdigest()


def venue_absent_sha256(hours) -> str:
    payload = "\n".join(str(int(h)) for h in sorted(int(h) for h in hours)).encode()
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


def continuous_history_start(df: pd.DataFrame, event_ms: np.ndarray, absent_hours=()):
    """First bar whose right-closed hours through the last bar all have records.

    The kept frame still starts on a bar, so that bar itself attaches nothing.
    Coverage required is every hour in (kept start, last bar].
    """
    if df is None or len(df) == 0:
        return None
    bar_ms = _bar_open_ms(df.index)
    missing = right_closed_missing_hours(bar_ms, event_ms, absent_hours)
    nz = np.flatnonzero(missing)
    if nz.size == 0:
        return df.index[0]
    return df.index[int(nz[-1])]


def _stored_funding_mode(mode: str, price: bool) -> str:
    """A saved mode says charge only when this frame charged funding.

    ``off`` stays ``off``. A charge or partial request that does not price
    the frame is ``not_priced``.
    """
    if price or mode == "off":
        return mode
    return "not_priced"


_UNPRICED_COVERAGE = {
    "expected_hours": 0,
    "present_hours": 0,
    "missing_hours": 0,
    "max_gap_hours": 0,
    "venue_absent_hours": 0,
    "complete": True,
}


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
    stored_mode = _stored_funding_mode(mode, price)
    if not price and not keep_rate:
        block = {
            "mode": stored_mode,
            "available": True,
            "complete": True,
            "coverage": dict(_UNPRICED_COVERAGE),
            "source": None,
            "sha256": None,
            "row_count": 0,
            "first": None,
            "last": None,
            "priced": False,
        }
        if stored_mode == "off":
            return _stamp_columns(out, stored_mode, block), block
        return _stamp_columns(out, stored_mode, block,
                              np.zeros(len(out), dtype=np.int64)), block

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
    if not price:
        # The rate column is an entry input. This frame charges no funding,
        # so missing hours stay zero and the saved mode is not charge.
        missing = np.zeros(len(out), dtype=np.int64)
        block = {
            "mode": stored_mode,
            "available": error is None,
            "complete": True,
            "coverage": dict(_UNPRICED_COVERAGE),
            "source": "unavailable" if error is not None else (source_box.get("source") or "unavailable"),
            "sha256": funding_row_sha256([], []),
            "row_count": 0,
            "first": None,
            "last": None,
            "priced": False,
        }
        if error is not None:
            block["error"] = error
        if source_box.get("fetch_error"):
            block["fetch_error"] = source_box["fetch_error"]
        if stored_mode == "off":
            return _stamp_columns(out, stored_mode, block), block
        return _stamp_columns(out, stored_mode, block, missing), block
    absent = tuple(source_box.get("venue_absent_hours") or ()) if error is None else ()
    missing, coverage, absent_used = _hour_coverage_detail(bar_ms, event_ms, absent)
    available = error is None and (event_ms.size > 0
                                   or coverage["venue_absent_hours"] > 0
                                   or coverage["expected_hours"] == 0)
    if error is None and event_ms.size:
        shaped = pd.DataFrame({"timestamp": event_ms, "rate": event_rate})
        out = attach_funding_accrual_column(out, shaped)
    block = {
        "mode": stored_mode,
        "available": bool(available),
        "complete": bool(coverage["complete"] and available),
        "coverage": coverage,
        "source": "unavailable" if error is not None else (source_box.get("source") or "unavailable"),
        "sha256": funding_row_sha256(event_ms, event_rate),
        "venue_absent_sha256": venue_absent_sha256(absent_used),
        "row_count": int(event_ms.size),
        "first": _iso_ms(int(event_ms.min())) if event_ms.size else None,
        "last": _iso_ms(int(event_ms.max())) if event_ms.size else None,
        "priced": True,
    }
    if error is not None:
        block["error"] = error
    if source_box.get("fetch_error"):
        block["fetch_error"] = source_box["fetch_error"]
    return _stamp_columns(out, stored_mode, block, missing), block
