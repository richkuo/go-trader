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


@pytest.mark.parametrize(
    "signals,accrual,direction,funding,capital,trades",
    [
        ([-1, 0, 0, 0, 0, 0], [1e-3] * 6, "short", 40.0, 10040.0, None),
        ([-1, 0, 0, 0, 0, 0], [-1e-3] * 6, "short", -40.0, 9960.0, None),
        ([1, 0, 0, 0, 0, 0], [1e-3] * 6, None, -40.0, 9960.0, None),
        ([-1, 1, 0, 0, 0, 0], [1e-3] * 6, "short", 10.0, None, 1),
        ([-1, 0, 1, 0, 0, 0], [1e-3] * 6, "short", 20.0, None, 1),
        ([-1, 0, 0, 0, 0, 0],
         [1e-3, 1e-3, np.nan, 1e-3, 1e-3, 1e-3], "short", 30.0, None, None),
        ([-1, 0, 0, 1, 0, 0], None, "short", 0.0, None, None),
        ([0, 0, 0, 0, 0, 0], [1e-3] * 6, "short", 0.0, 10000.0, None),
    ],
    ids=[
        "short_collects_positive",
        "short_pays_negative",
        "long_pays_positive",
        "open_bar_accrues_nothing",
        "charge_count_equals_intervals_held",
        "nan_accrual_ignored",
        "no_funding_column",
        "not_booked_while_flat",
    ],
)
def test_funding_accrual_books_each_held_interval(
    signals, accrual, direction, funding, capital, trades
):
    df = _df(signals, accrual=accrual)
    res = _run(df, direction=direction)
    assert res["total_funding_pnl"] == pytest.approx(funding)
    assert np.isfinite(res["final_capital"])
    if capital is not None:
        assert res["final_capital"] == pytest.approx(capital)
    if trades is not None:
        assert res["total_trades"] == trades
