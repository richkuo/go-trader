
import os
import sys
import tempfile

import numpy as np
import pandas as pd
import pytest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from funding_fetcher import (
    attach_funding_accrual_column,
    attach_funding_column,
    load_cached_funding,
)

_HOUR_MS = 3_600_000
_BASE_MS = int(pd.Timestamp("2026-01-01", tz="UTC").timestamp() * 1000)


class StubAdapter:

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


def test_cache_hit_survives_elapsed_wallclock():
    db = _tmp_db()
    stub = StubAdapter(_BASE_MS, hours=72)
    load_cached_funding("BTC", "2026-01-01", "2026-01-03", adapter=stub, db_path=db)
    assert stub.calls == 1
    again = load_cached_funding("BTC", "2026-01-01", "2026-01-03",
                                adapter=stub, db_path=db)
    assert stub.calls == 1
    assert not again.empty
    os.unlink(db)


def test_late_listed_coin_cached_after_first_fetch():
    db = _tmp_db()
    listed_at = _BASE_MS + 30 * 24 * _HOUR_MS
    stub = StubAdapter(listed_at, hours=24 * 5)
    first = load_cached_funding("LATECOIN", "2026-01-01", "2026-02-04",
                                adapter=stub, db_path=db)
    assert stub.calls == 1
    assert int(first["timestamp"].iloc[0]) == listed_at
    again = load_cached_funding("LATECOIN", "2026-01-01", "2026-02-04",
                                adapter=stub, db_path=db)
    assert stub.calls == 1, "late-listed coin must not refetch forever"
    assert len(again) == len(first)
    os.unlink(db)


def test_partial_fetch_does_not_claim_tail_coverage():
    db = _tmp_db()
    stub = StubAdapter(_BASE_MS, hours=24)
    load_cached_funding("BTC", "2026-01-01", "2026-01-10", adapter=stub, db_path=db)
    assert stub.calls == 1
    load_cached_funding("BTC", "2026-01-01", "2026-01-10", adapter=stub, db_path=db)
    assert stub.calls == 2, "uncovered tail must refetch"
    load_cached_funding("BTC", "2026-01-01", "2026-01-01 20:00",
                        adapter=stub, db_path=db)
    assert stub.calls == 2
    os.unlink(db)


def test_disjoint_fetches_do_not_poison_middle():
    db = _tmp_db()
    stub = StubAdapter(_BASE_MS, hours=24 * 300)
    load_cached_funding("BTC", "2026-07-20", "2026-07-30", adapter=stub, db_path=db)
    assert stub.calls == 1
    load_cached_funding("BTC", "2026-01-01", "2026-01-10", adapter=stub, db_path=db)
    assert stub.calls == 2
    middle = load_cached_funding("BTC", "2026-03-01", "2026-03-10",
                                 adapter=stub, db_path=db)
    assert stub.calls == 3, "unfetched middle must refetch, not false-cache-hit"
    assert not middle.empty
    assert int(middle["timestamp"].iloc[0]) >= _to_ms("2026-03-01")
    os.unlink(db)


def test_adjacent_fetches_merge_into_one_covered_interval():
    db = _tmp_db()
    stub = StubAdapter(_BASE_MS, hours=24 * 20)
    load_cached_funding("BTC", "2026-01-01", "2026-01-10", adapter=stub, db_path=db)
    load_cached_funding("BTC", "2026-01-10", "2026-01-20", adapter=stub, db_path=db)
    assert stub.calls == 2
    spanning = load_cached_funding("BTC", "2026-01-05", "2026-01-15",
                                   adapter=stub, db_path=db)
    assert stub.calls == 2, "range inside two touching fetches must be a cache hit"
    assert not spanning.empty
    os.unlink(db)


def test_gap_spanning_request_backfills_and_heals_coverage():
    db = _tmp_db()
    stub = StubAdapter(_BASE_MS, hours=24 * 300)
    load_cached_funding("BTC", "2026-01-01", "2026-01-10", adapter=stub, db_path=db)
    load_cached_funding("BTC", "2026-07-20", "2026-07-30", adapter=stub, db_path=db)
    assert stub.calls == 2
    spanning = load_cached_funding("BTC", "2026-01-05", "2026-07-25",
                                   adapter=stub, db_path=db)
    assert stub.calls == 3
    assert not spanning.empty
    again = load_cached_funding("BTC", "2026-01-05", "2026-07-25",
                                adapter=stub, db_path=db)
    assert stub.calls == 3, "backfilled span must now be covered"
    assert len(again) == len(spanning)
    os.unlink(db)


def _to_ms(date_str):
    return int(pd.Timestamp(date_str, tz="UTC").timestamp() * 1000)


def test_timestamp_end_date_accepted():
    db = _tmp_db()
    stub = StubAdapter(_BASE_MS, hours=72)
    naive_end = pd.Timestamp("2026-01-03")
    aware_end = pd.Timestamp("2026-01-03", tz="UTC")
    out = load_cached_funding("BTC", "2026-01-01", naive_end,
                              adapter=stub, db_path=db)
    assert stub.calls == 1 and not out.empty
    out2 = load_cached_funding("BTC", "2026-01-01", aware_end,
                               adapter=stub, db_path=db)
    assert stub.calls == 1 and len(out2) == len(out)
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
    df = _bars(4)
    f = _funding_frame(
        [_BASE_MS + 30 * 60 * 1000],
        [7e-5],
    )
    out = attach_funding_column(df, f)
    assert np.isnan(out["funding_rate"].iloc[0])
    assert out["funding_rate"].iloc[1] == 7e-5
    assert out["funding_rate"].iloc[3] == 7e-5


def test_attach_4h_bars_take_latest_hourly():
    df = _bars(3, freq="4h")
    times = [_BASE_MS + i * _HOUR_MS for i in range(9)]
    rates = [i * 1e-5 for i in range(9)]
    out = attach_funding_column(df, _funding_frame(times, rates))
    assert out["funding_rate"].iloc[0] == 0.0
    assert out["funding_rate"].iloc[1] == 4e-5
    assert out["funding_rate"].iloc[2] == 8e-5


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


def test_accrual_1h_is_per_bar_event():
    df = _bars(4, freq="1h")
    rates = [1e-5, 2e-5, 3e-5, 4e-5]
    times = [_BASE_MS + i * _HOUR_MS for i in range(4)]
    out = attach_funding_accrual_column(df, _funding_frame(times, rates))
    acc = out["funding_accrual"].tolist()
    assert acc[0] == 0.0
    assert acc[1] == pytest.approx(2e-5)
    assert acc[2] == pytest.approx(3e-5)
    assert acc[3] == pytest.approx(4e-5)


def test_accrual_4h_sums_the_interval():
    df = _bars(3, freq="4h")
    times = [_BASE_MS + i * _HOUR_MS for i in range(9)]
    rates = [1e-5] * 9
    out = attach_funding_accrual_column(df, _funding_frame(times, rates))
    acc = out["funding_accrual"].tolist()
    assert acc[0] == 0.0
    assert acc[1] == pytest.approx(4e-5)
    assert acc[2] == pytest.approx(4e-5)


def test_accrual_total_equals_held_funding():
    df = _bars(5, freq="1h")
    times = [_BASE_MS + i * _HOUR_MS for i in range(5)]
    rates = [1e-5, 2e-5, 3e-5, 4e-5, 5e-5]
    out = attach_funding_accrual_column(df, _funding_frame(times, rates))
    assert out["funding_accrual"].sum() == pytest.approx(2e-5 + 3e-5 + 4e-5 + 5e-5)


def test_accrual_empty_funding_is_zero():
    assert (attach_funding_accrual_column(_bars(3), None)["funding_accrual"] == 0.0).all()
    empty = attach_funding_accrual_column(_bars(3), _funding_frame([], []))
    assert (empty["funding_accrual"] == 0.0).all()


def test_accrual_event_at_bar_is_right_closed():
    df = _bars(3, freq="1h")
    out = attach_funding_accrual_column(
        df, _funding_frame([_BASE_MS + _HOUR_MS], [9e-5]))
    acc = out["funding_accrual"].tolist()
    assert acc[1] == pytest.approx(9e-5)
    assert acc[2] == 0.0
