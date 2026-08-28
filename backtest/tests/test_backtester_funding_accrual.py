import sys
import pathlib

import numpy as np
import pandas as pd
import pytest

sys.path.insert(0, str(pathlib.Path(__file__).parent.parent.parent / "shared_tools"))
sys.path.insert(0, str(pathlib.Path(__file__).parent.parent))

from backtester import Backtester


def _df(signals, accrual=None, price=100.0, n=6):
    closes = np.full(n, float(price))
    idx = pd.date_range("2024-01-01", periods=n, freq="D")
    df = pd.DataFrame(
        {
            "open": closes,
            "high": closes + 0.5,
            "low": closes - 0.5,
            "close": closes,
            "volume": np.full(n, 1000.0),
            "signal": np.asarray(signals, dtype=float),
        },
        index=idx,
    )
    if accrual is not None:
        df["funding_accrual"] = np.asarray(accrual, dtype=float)
    return df


def _run(df, **kw):
    kw.setdefault("commission_pct", 0.0)
    kw.setdefault("slippage_pct", 0.0)
    bt = Backtester(initial_capital=10000.0, **kw)
    return bt.run(df.copy(), strategy_name="x", symbol="BTC/USDT",
                  timeframe="1d", save=False)


def test_short_collects_positive_funding_on_flat_price():
    df = _df([-1, 0, 0, 0, 0, 0], accrual=[1e-3] * 6)
    res = _run(df, direction="short")
    assert res["total_funding_pnl"] == pytest.approx(40.0)
    assert res["final_capital"] == pytest.approx(10040.0)


def test_short_pays_negative_funding():
    df = _df([-1, 0, 0, 0, 0, 0], accrual=[-1e-3] * 6)
    res = _run(df, direction="short")
    assert res["total_funding_pnl"] == pytest.approx(-40.0)
    assert res["final_capital"] == pytest.approx(9960.0)


def test_long_pays_positive_funding():
    df = _df([1, 0, 0, 0, 0, 0], accrual=[1e-3] * 6)
    res = _run(df, direction=None)
    assert res["total_funding_pnl"] == pytest.approx(-40.0)
    assert res["final_capital"] == pytest.approx(9960.0)


def test_open_bar_accrues_nothing_one_interval_one_charge():
    df = _df([-1, 1, 0, 0, 0, 0], accrual=[1e-3] * 6)
    res = _run(df, direction="short")
    assert res["total_trades"] == 1
    assert res["total_funding_pnl"] == pytest.approx(10.0)


def test_charge_count_equals_intervals_held():
    df = _df([-1, 0, 1, 0, 0, 0], accrual=[1e-3] * 6)
    res = _run(df, direction="short")
    assert res["total_trades"] == 1
    assert res["total_funding_pnl"] == pytest.approx(20.0)


def test_nan_accrual_is_ignored():
    df = _df([-1, 0, 0, 0, 0, 0], accrual=[1e-3, 1e-3, np.nan, 1e-3, 1e-3, 1e-3])
    res = _run(df, direction="short")
    assert res["total_funding_pnl"] == pytest.approx(30.0)
    assert np.isfinite(res["final_capital"])


def test_no_funding_column_books_nothing():
    df = _df([-1, 0, 0, 1, 0, 0])
    res = _run(df, direction="short")
    assert res["total_funding_pnl"] == 0.0


def test_funding_not_booked_while_flat():
    df = _df([0, 0, 0, 0, 0, 0], accrual=[1e-3] * 6)
    res = _run(df, direction="short")
    assert res["total_funding_pnl"] == 0.0
    assert res["final_capital"] == pytest.approx(10000.0)
