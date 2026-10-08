import os

import numpy as np
import pandas as pd
import pytest

from shared_tools.conftest import load_module, make_ohlcv
from storage import store_funding_coverage, store_funding_rates

_FUNDING_FETCHER = load_module("_funding_fetcher_test", os.path.join(os.path.dirname(__file__), "funding_fetcher.py"))
attach_funding_accrual_column = _FUNDING_FETCHER.attach_funding_accrual_column
attach_funding_column = _FUNDING_FETCHER.attach_funding_column
load_cached_funding = _FUNDING_FETCHER.load_cached_funding

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


@pytest.fixture
def db_path(tmp_path):
    return str(tmp_path / "funding.db")


def test_partial_fetch_does_not_claim_tail_coverage(db_path):
    db = db_path
    stub = StubAdapter(_BASE_MS, hours=24)
    load_cached_funding("BTC", "2026-01-01", "2026-01-10", adapter=stub, db_path=db)
    assert stub.calls == 1
    load_cached_funding("BTC", "2026-01-01", "2026-01-10", adapter=stub, db_path=db)
    assert stub.calls == 2, "uncovered tail must refetch"
    load_cached_funding("BTC", "2026-01-01", "2026-01-01 20:00",
                        adapter=stub, db_path=db)
    assert stub.calls == 2


def test_disjoint_fetches_do_not_poison_middle(db_path):
    db = db_path
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


def _to_ms(date_str):
    return int(pd.Timestamp(date_str, tz="UTC").timestamp() * 1000)


def _bars(n, freq="1h", tz=None):
    idx = pd.date_range("2026-01-01", periods=n, freq=freq, tz=tz)
    return make_ohlcv(
        [100.0] * n,
        volume=10.0,
        opens=[100.0] * n,
        highs=[101.0] * n,
        lows=[99.0] * n,
        index=idx,
    )


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


def test_accrual_4h_sums_the_interval():
    df = _bars(3, freq="4h")
    times = [_BASE_MS + i * _HOUR_MS for i in range(9)]
    rates = [1e-5] * 9
    out = attach_funding_accrual_column(df, _funding_frame(times, rates))
    acc = out["funding_accrual"].tolist()
    assert acc[0] == 0.0
    assert acc[1] == pytest.approx(4e-5)
    assert acc[2] == pytest.approx(4e-5)


def test_accrual_event_at_bar_is_right_closed():
    df = _bars(3, freq="1h")
    out = attach_funding_accrual_column(
        df, _funding_frame([_BASE_MS + _HOUR_MS], [9e-5]))
    acc = out["funding_accrual"].tolist()
    assert acc[1] == pytest.approx(9e-5)
    assert acc[2] == 0.0


_ADAPTER_MODULES = {}


def _adapter_module():
    if "hl" not in _ADAPTER_MODULES:
        _ADAPTER_MODULES["hl"] = load_module(
            "_hl_adapter_funding_1747_test",
            os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))),
                         "platforms", "hyperliquid", "adapter.py"),
        )
    return _ADAPTER_MODULES["hl"]
_OFFSET_MS = 76


class FakeInfo:

    def __init__(self, times, rate=1.25e-5):
        self.rates = {int(t): rate for t in times}
        self.calls = []
        self.fail = False
        self.descending = False

    def funding_history(self, name, startTime, endTime=None):
        self.calls.append(int(startTime))
        if self.fail:
            raise ConnectionError("venue down")
        rows = [{"coin": name, "fundingRate": str(self.rates[t]), "premium": "0", "time": t}
                for t in sorted(self.rates) if t >= startTime][:500]
        if self.descending:
            rows = rows[::-1]
        return rows


def _real_adapter(info):
    adapter = object.__new__(_adapter_module().HyperliquidExchangeAdapter)
    adapter._info = info
    return adapter


def _hour_prints(hours, skip=()):
    return [_BASE_MS + h * _HOUR_MS + _OFFSET_MS for h in range(hours) if h not in skip]


def _seed(db, times, cov_end_ms, rate=1.25e-5):
    store_funding_rates([{"time": t, "rate": rate} for t in times], "hyperliquid", "BTC", db_path=db)
    store_funding_coverage("hyperliquid", "BTC", _BASE_MS, cov_end_ms, db_path=db)


def _rows(db, sql, params=()):
    import sqlite3
    conn = sqlite3.connect(db)
    try:
        return conn.execute(sql, params).fetchall()
    finally:
        conn.close()


def _markers(db):
    return [r[0] for r in _rows(db, "SELECT timestamp FROM funding_venue_gaps ORDER BY timestamp")]


def _rates_in_hour(db, hour):
    start = _BASE_MS + hour * _HOUR_MS
    return _rows(db, "SELECT timestamp FROM funding_rates WHERE timestamp >= ? AND timestamp < ?",
                 (start, start + _HOUR_MS))


def _load(db, adapter, end_hours=40, box=None):
    return load_cached_funding("BTC", pd.Timestamp(_BASE_MS, unit="ms", tz="UTC"),
                               pd.Timestamp(_BASE_MS + end_hours * _HOUR_MS, unit="ms", tz="UTC"),
                               adapter=adapter, db_path=db, source_box=box)


def test_clean_cache_hit_builds_no_adapter(db_path, monkeypatch):
    _seed(db_path, _hour_prints(48), _BASE_MS + 47 * _HOUR_MS + _OFFSET_MS)

    def _no_adapter():
        raise AssertionError("adapter built on a clean cache hit")

    monkeypatch.setattr(_FUNDING_FETCHER, "_hl_adapter", _no_adapter)
    box = {}
    out = _load(db_path, None, box=box)
    assert box["source"] == "cache"
    assert "fetch_error" not in box
    assert len(out) == 40


def test_interior_missing_hour_downloads_once_and_stores_the_rate(db_path):
    _seed(db_path, _hour_prints(48, skip={10}), _BASE_MS + 47 * _HOUR_MS + _OFFSET_MS)
    info = FakeInfo(_hour_prints(48))
    box = {}
    out = _load(db_path, _real_adapter(info), box=box)
    assert info.calls == [_BASE_MS + 10 * _HOUR_MS]
    assert box["source"] == "cache"
    assert len(_rates_in_hour(db_path, 10)) == 1
    assert len(out) == 40
    assert _markers(db_path) == []
    _load(db_path, _real_adapter(info))
    assert len(info.calls) == 1


def test_venue_absent_hour_writes_a_marker_and_no_rate(db_path):
    _seed(db_path, _hour_prints(48, skip={10}), _BASE_MS + 47 * _HOUR_MS + _OFFSET_MS)
    info = FakeInfo(_hour_prints(48, skip={10}))
    box = {}
    _load(db_path, _real_adapter(info), box=box)
    assert info.calls == [_BASE_MS + 10 * _HOUR_MS]
    assert _markers(db_path) == [_BASE_MS + 10 * _HOUR_MS]
    assert _rates_in_hour(db_path, 10) == []
    assert box["venue_absent_hours"] == [_BASE_MS + 10 * _HOUR_MS]
    assert "fetch_error" not in box
    _load(db_path, _real_adapter(info))
    assert len(info.calls) == 1


def test_failed_download_writes_nothing_and_reports_the_error(db_path):
    cov_end = _BASE_MS + 47 * _HOUR_MS + _OFFSET_MS
    _seed(db_path, _hour_prints(48, skip={10}), cov_end)
    info = FakeInfo(_hour_prints(48))
    info.fail = True
    box = {}
    out = _load(db_path, _real_adapter(info), box=box)
    assert len(info.calls) == 1
    assert "venue down" in box["fetch_error"]
    assert _rates_in_hour(db_path, 10) == []
    assert _markers(db_path) == []
    assert box["venue_absent_hours"] == []
    assert len(out) == 39
    assert _rows(db_path, "SELECT start_ts, end_ts FROM funding_coverage") == [(_BASE_MS, cov_end)]


def test_non_ascending_page_raises_and_writes_nothing(db_path):
    _seed(db_path, _hour_prints(48, skip={10}), _BASE_MS + 47 * _HOUR_MS + _OFFSET_MS)
    info = FakeInfo(_hour_prints(48, skip={10}))
    info.descending = True
    with pytest.raises(ValueError):
        _real_adapter(info).get_funding_history_window("BTC", _BASE_MS, _BASE_MS + 20 * _HOUR_MS)
    box = {}
    _load(db_path, _real_adapter(info), box=box)
    assert "not ascending" in box["fetch_error"] or "before cursor" in box["fetch_error"]
    assert _markers(db_path) == []
    assert _rates_in_hour(db_path, 10) == []


def test_stored_rate_deletes_the_marker(db_path):
    _seed(db_path, _hour_prints(48, skip={10}), _BASE_MS + 47 * _HOUR_MS + _OFFSET_MS)
    _load(db_path, _real_adapter(FakeInfo(_hour_prints(48, skip={10}))))
    assert _markers(db_path) == [_BASE_MS + 10 * _HOUR_MS]
    store_funding_rates([{"time": _BASE_MS + 10 * _HOUR_MS + _OFFSET_MS, "rate": 2e-5}],
                        "hyperliquid", "BTC", db_path=db_path)
    assert _markers(db_path) == []
    assert len(_rates_in_hour(db_path, 10)) == 1


def test_fetch_path_response_with_the_print_deletes_the_marker(db_path):
    _seed(db_path, _hour_prints(48, skip={10}), _BASE_MS + 47 * _HOUR_MS + _OFFSET_MS)
    _load(db_path, _real_adapter(FakeInfo(_hour_prints(48, skip={10}))))
    assert _markers(db_path) == [_BASE_MS + 10 * _HOUR_MS]
    info = FakeInfo(_hour_prints(100))
    box = {}
    _load(db_path, _real_adapter(info), end_hours=80, box=box)
    assert box["source"] == "fetched"
    assert info.calls[0] == _BASE_MS
    assert _markers(db_path) == []
    assert len(_rates_in_hour(db_path, 10)) == 1


def _charge_block(db, adapter, end_hours=40):
    frame = _bars(end_hours + 1, tz="UTC")
    _out, block = _FUNDING_FETCHER.attach_backtest_funding(
        frame, "BTC", "1h", "hyperliquid", "perps", "sma_crossover",
        mode="charge", adapter=adapter, db_path=db)
    return block


def test_pre_listing_hours_inside_coverage_make_no_call(db_path):
    _seed(db_path, _hour_prints(41)[5:], _BASE_MS + 40 * _HOUR_MS)
    info = FakeInfo(_hour_prints(41)[5:])
    _load(db_path, _real_adapter(info))
    box = {}
    _load(db_path, _real_adapter(info), box=box)
    assert info.calls == []
    assert "fetch_error" not in box
    assert _markers(db_path) == []
    block = _charge_block(db_path, _real_adapter(info))
    assert info.calls == []
    assert block["coverage"]["missing_hours"] == 5
    assert block["complete"] is False
    for hour in range(5):
        assert _rates_in_hour(db_path, hour) == []


def test_pre_listing_hours_before_coverage_download_once(db_path):
    store_funding_rates([{"time": t, "rate": 1.25e-5} for t in _hour_prints(41)[5:]],
                        "hyperliquid", "BTC", db_path=db_path)
    store_funding_coverage("hyperliquid", "BTC", _BASE_MS + 2 * _HOUR_MS,
                           _BASE_MS + 40 * _HOUR_MS, db_path=db_path)
    info = FakeInfo(_hour_prints(41)[5:])
    box = {}
    _load(db_path, _real_adapter(info), box=box)
    assert info.calls == [_BASE_MS]
    assert "fetch_error" not in box
    assert _markers(db_path) == []
    assert _rows(db_path, "SELECT start_ts, end_ts FROM funding_coverage ORDER BY start_ts") == [
        (_BASE_MS, _BASE_MS + 2 * _HOUR_MS - 1),
        (_BASE_MS + 2 * _HOUR_MS, _BASE_MS + 40 * _HOUR_MS)]
    _load(db_path, _real_adapter(info))
    assert info.calls == [_BASE_MS]


def test_delisted_tail_settles_after_one_download(db_path):
    _seed(db_path, _hour_prints(35), _BASE_MS + 38 * _HOUR_MS)
    info = FakeInfo(_hour_prints(35))
    box = {}
    _load(db_path, _real_adapter(info), box=box)
    assert info.calls == [_BASE_MS + 38 * _HOUR_MS]
    assert "fetch_error" not in box
    assert _rows(db_path, "SELECT start_ts, end_ts FROM funding_coverage") == [
        (_BASE_MS, _BASE_MS + 40 * _HOUR_MS - 1)]
    _load(db_path, _real_adapter(info))
    assert len(info.calls) == 1
    block = _charge_block(db_path, _real_adapter(info))
    assert len(info.calls) == 1
    assert block["coverage"]["missing_hours"] == 5
    assert block["complete"] is False
    assert _markers(db_path) == []
    for hour in range(35, 40):
        assert _rates_in_hour(db_path, hour) == []


def test_unfinalized_hour_is_not_downloaded_or_marked(db_path):
    now_hour = (int(pd.Timestamp.now(tz="UTC").timestamp() * 1000) // _HOUR_MS) * _HOUR_MS
    start = now_hour - 30 * _HOUR_MS
    prints = [start + h * _HOUR_MS + _OFFSET_MS for h in range(29)]
    store_funding_rates([{"time": t, "rate": 1e-5} for t in prints], "hyperliquid", "BTC",
                        db_path=db_path)
    store_funding_coverage("hyperliquid", "BTC", start, now_hour + _HOUR_MS, db_path=db_path)
    info = FakeInfo(prints)
    load_cached_funding("BTC", pd.Timestamp(start, unit="ms", tz="UTC"),
                        pd.Timestamp(now_hour, unit="ms", tz="UTC"),
                        adapter=_real_adapter(info), db_path=db_path)
    assert info.calls == []
    assert _markers(db_path) == []


def test_fresh_fetch_records_the_interior_gap_with_rates_and_coverage(db_path):
    info = FakeInfo(_hour_prints(48, skip={10}))
    box = {}
    out = _load(db_path, _real_adapter(info), box=box)
    assert box["source"] == "fetched"
    assert info.calls == [_BASE_MS]
    assert len(out) == 39
    assert _markers(db_path) == [_BASE_MS + 10 * _HOUR_MS]
    assert box["venue_absent_hours"] == [_BASE_MS + 10 * _HOUR_MS]
    assert _rows(db_path, "SELECT start_ts, end_ts FROM funding_coverage") == [
        (_BASE_MS, _BASE_MS + 40 * _HOUR_MS)]


def test_fresh_fetch_store_failure_rolls_back_rates_markers_and_coverage(db_path):
    import sqlite3
    _FUNDING_FETCHER.load_funding_coverage("hyperliquid", "BTC", db_path=db_path)
    conn = sqlite3.connect(db_path)
    conn.execute("CREATE TRIGGER fail_gap BEFORE INSERT ON funding_venue_gaps"
                 " BEGIN SELECT RAISE(ABORT, 'gap insert refused'); END")
    conn.commit()
    conn.close()
    info = FakeInfo(_hour_prints(48, skip={10}))
    with pytest.raises(sqlite3.DatabaseError):
        _load(db_path, _real_adapter(info))
    assert _rows(db_path, "SELECT COUNT(*) FROM funding_rates") == [(0,)]
    assert _rows(db_path, "SELECT COUNT(*) FROM funding_coverage") == [(0,)]
    assert _markers(db_path) == []
