import os

import numpy as np
import pandas as pd
import pytest

from shared_tools.conftest import load_module, make_ohlcv

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
