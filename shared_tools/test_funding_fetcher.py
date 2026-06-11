"""Tests for funding_fetcher.py — funding history cache + bar alignment (#960)."""

import os
import sys
import tempfile

import numpy as np
import pandas as pd

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from funding_fetcher import attach_funding_column, load_cached_funding  # noqa: E402

_HOUR_MS = 3_600_000
_BASE_MS = int(pd.Timestamp("2026-01-01", tz="UTC").timestamp() * 1000)


class StubAdapter:
    """Returns synthetic hourly funding; records calls."""

    def __init__(self, start_ms, hours):
        self.records = [
            {"rate": 1e-5 * ((i % 5) - 2), "time": start_ms + i * _HOUR_MS}
            for i in range(hours)
        ]
        self.calls = 0

    def get_funding_history_range(self, coin, start_ms, end_ms=None):
        self.calls += 1
        return [r for r in self.records
                if r["time"] >= start_ms and (end_ms is None or r["time"] <= end_ms)]


def _tmp_db():
    fd, path = tempfile.mkstemp(suffix=".db")
    os.close(fd)
    os.unlink(path)
    return path


def test_fetch_then_cache_hit():
    db = _tmp_db()
    stub = StubAdapter(_BASE_MS, hours=72)
    first = load_cached_funding("BTC", "2026-01-01", "2026-01-03",
                                adapter=stub, db_path=db)
    assert len(first) > 0
    assert stub.calls == 1
    again = load_cached_funding("BTC", "2026-01-01", "2026-01-03",
                                adapter=stub, db_path=db)
    assert stub.calls == 1, "covered range must be served from cache"
    assert len(again) == len(first)
    os.unlink(db)


def test_uncovered_range_refetches():
    db = _tmp_db()
    stub = StubAdapter(_BASE_MS, hours=24 * 10)
    load_cached_funding("BTC", "2026-01-01", "2026-01-02", adapter=stub, db_path=db)
    assert stub.calls == 1
    wider = load_cached_funding("BTC", "2026-01-01", "2026-01-09",
                                adapter=stub, db_path=db)
    assert stub.calls == 2, "cache end short of requested end must refetch"
    assert int(wider["timestamp"].iloc[-1]) >= _BASE_MS + 8 * 24 * _HOUR_MS
    os.unlink(db)


def test_empty_api_returns_cached_or_empty():
    db = _tmp_db()
    stub = StubAdapter(_BASE_MS, hours=0)
    out = load_cached_funding("NEWCOIN", "2026-01-01", "2026-01-03",
                              adapter=stub, db_path=db)
    assert out.empty
    os.unlink(db)


def _bars(n, freq="1h", tz=None):
    idx = pd.date_range("2026-01-01", periods=n, freq=freq, tz=tz)
    return pd.DataFrame({
        "open": 100.0, "high": 101.0, "low": 99.0,
        "close": 100.0, "volume": 10.0,
    }, index=idx)


def _funding_frame(times_ms, rates):
    df = pd.DataFrame({"timestamp": times_ms, "rate": rates})
    df["datetime"] = pd.to_datetime(df["timestamp"], unit="ms", utc=True)
    return df.set_index("datetime")


def test_attach_backward_only():
    """A bar must get the latest snapshot at or BEFORE its own timestamp."""
    df = _bars(4)
    f = _funding_frame(
        [_BASE_MS + 30 * 60 * 1000],  # 00:30 — between bar0 and bar1
        [7e-5],
    )
    out = attach_funding_column(df, f)
    assert np.isnan(out["funding_rate"].iloc[0])
    assert out["funding_rate"].iloc[1] == 7e-5
    assert out["funding_rate"].iloc[3] == 7e-5


def test_attach_4h_bars_take_latest_hourly():
    """On 4h bars each bar gets the most recent hourly snapshot, not a future
    one: bar at 04:00 sees the 04:00 snapshot, never 05:00."""
    df = _bars(3, freq="4h")
    times = [_BASE_MS + i * _HOUR_MS for i in range(9)]
    rates = [i * 1e-5 for i in range(9)]
    out = attach_funding_column(df, _funding_frame(times, rates))
    assert out["funding_rate"].iloc[0] == 0.0       # 00:00 snapshot
    assert out["funding_rate"].iloc[1] == 4e-5      # 04:00 snapshot
    assert out["funding_rate"].iloc[2] == 8e-5      # 08:00 snapshot


def test_attach_empty_funding_gives_nan():
    out = attach_funding_column(_bars(3), None)
    assert out["funding_rate"].isna().all()
    out2 = attach_funding_column(_bars(3), _funding_frame([], []))
    assert out2["funding_rate"].isna().all()


def test_attach_tz_naive_bars():
    df = _bars(3, tz=None)
    f = _funding_frame([_BASE_MS], [3e-5])
    out = attach_funding_column(df, f)
    assert out["funding_rate"].iloc[0] == 3e-5
