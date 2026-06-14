"""#988: funding-carry booking in the engine.

A ``funding_accrual`` column (total funding rate over each bar, attached for
carry strategies like delta_neutral_funding) is booked each bar against the
position carried into the bar:

    funding_cash = -position * mark * accrual      (position signed: + long, - short)

so a SHORT receives funding when the rate is positive and a LONG pays it. These
tests pin the sign, magnitude, the one-bar open lag, NaN handling, and that
strategies without the column are untouched.
"""
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


# ─── Short collects positive funding ──────────────────────────────────────────


def test_short_collects_positive_funding_on_flat_price():
    # Short opens at bar1 (signal at bar0 fills bar1 open), held to end of data.
    # Price flat → zero price PnL → the entire return is collected funding.
    # Funding accrues on the carried-in position: bars 2..5 (4 bars), the open
    # bar (1) accrues nothing. notional=10000, accrual=1e-3 → +10/bar → +40.
    df = _df([-1, 0, 0, 0, 0, 0], accrual=[1e-3] * 6)
    res = _run(df, direction="short")
    assert res["total_funding_pnl"] == pytest.approx(40.0)
    assert res["final_capital"] == pytest.approx(10040.0)


def test_short_pays_negative_funding():
    # Negative funding = shorts pay longs → the short bleeds carry.
    df = _df([-1, 0, 0, 0, 0, 0], accrual=[-1e-3] * 6)
    res = _run(df, direction="short")
    assert res["total_funding_pnl"] == pytest.approx(-40.0)
    assert res["final_capital"] == pytest.approx(9960.0)


# ─── Long is the mirror ───────────────────────────────────────────────────────


def test_long_pays_positive_funding():
    # Long pays funding when the rate is positive → flat-price return is the
    # negated carry of the short case.
    df = _df([1, 0, 0, 0, 0, 0], accrual=[1e-3] * 6)
    res = _run(df, direction=None)
    assert res["total_funding_pnl"] == pytest.approx(-40.0)
    assert res["final_capital"] == pytest.approx(9960.0)


# ─── Lag, NaN, opt-in ─────────────────────────────────────────────────────────


def test_open_bar_accrues_nothing_one_interval_one_charge():
    # The open bar accrues nothing (position not yet carried in); each carried
    # interval is one charge. Open at bar1 (signal at bar0), close at bar2
    # (signal +1 at bar1) → held over exactly one interval (1,2] → one charge.
    df = _df([-1, 1, 0, 0, 0, 0], accrual=[1e-3] * 6)
    res = _run(df, direction="short")
    assert res["total_trades"] == 1
    assert res["total_funding_pnl"] == pytest.approx(10.0)


def test_charge_count_equals_intervals_held():
    # Open bar1, close bar3 (signal +1 at bar2) → intervals (1,2] and (2,3] →
    # two charges. Pins that the close bar still accrues (closed at its open,
    # after the top-of-bar booking on the carried-in position).
    df = _df([-1, 0, 1, 0, 0, 0], accrual=[1e-3] * 6)
    res = _run(df, direction="short")
    assert res["total_trades"] == 1
    assert res["total_funding_pnl"] == pytest.approx(20.0)


def test_nan_accrual_is_ignored():
    # A NaN funding bar must not corrupt cash (treated as 0 carry).
    df = _df([-1, 0, 0, 0, 0, 0], accrual=[1e-3, 1e-3, np.nan, 1e-3, 1e-3, 1e-3])
    res = _run(df, direction="short")
    # bars 2..5 carry funding; bar2 is NaN→0, bars 3,4,5 = +10 each → +30.
    assert res["total_funding_pnl"] == pytest.approx(30.0)
    assert np.isfinite(res["final_capital"])


def test_no_funding_column_books_nothing():
    df = _df([-1, 0, 0, 1, 0, 0])  # no funding_accrual column
    res = _run(df, direction="short")
    assert res["total_funding_pnl"] == 0.0


def test_funding_not_booked_while_flat():
    # All-flat signals → never in a position → no funding booked despite a
    # populated accrual column.
    df = _df([0, 0, 0, 0, 0, 0], accrual=[1e-3] * 6)
    res = _run(df, direction="short")
    assert res["total_funding_pnl"] == 0.0
    assert res["final_capital"] == pytest.approx(10000.0)
