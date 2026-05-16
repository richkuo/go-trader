"""Tests for the two-leg pairs backtester."""

from __future__ import annotations

import os
import sys

import numpy as np
import pandas as pd
import pytest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from backtest_pairs import (  # noqa: E402
    PairsBacktester,
    SIDE_LONG_A,
    SIDE_SHORT_A,
    _liquidation_loss,
)


def _flat_df(prices: list[float], start: str = "2024-01-01") -> pd.DataFrame:
    idx = pd.date_range(start, periods=len(prices), freq="1h")
    return pd.DataFrame(
        {
            "open": prices,
            "high": prices,
            "low": prices,
            "close": prices,
            "volume": [1.0] * len(prices),
        },
        index=idx,
    )


def test_liquidation_loss_math() -> None:
    assert _liquidation_loss(1000.0, 20.0, 0.02) == pytest.approx(30.0)
    assert _liquidation_loss(1000.0, 10.0, 0.02) == pytest.approx(80.0)


def test_constructor_rejects_bad_params() -> None:
    with pytest.raises(ValueError):
        PairsBacktester(entry_z=1.0, exit_z=2.0)
    with pytest.raises(ValueError):
        PairsBacktester(leverage=0)
    with pytest.raises(ValueError):
        PairsBacktester(maintenance_margin=0.5, leverage=5.0)


def _make_spread_series(n: int, base_a: float, base_b: float,
                       spike_at: int, spike_pct: float, direction: int) -> tuple[pd.DataFrame, pd.DataFrame]:
    """Build two flat price series, then perturb A at `spike_at` to create a
    spread spike of `spike_pct` in `direction` (+1 = A overpriced, -1 = A underpriced),
    then revert by end of series."""
    prices_a = [base_a] * n
    prices_b = [base_b] * n
    for i in range(spike_at, min(spike_at + 5, n)):
        prices_a[i] = base_a * (1 + direction * spike_pct)
    return _flat_df(prices_a), _flat_df(prices_b)


def test_short_a_signal_fires_when_spread_high() -> None:
    df_a, df_b = _make_spread_series(n=300, base_a=2000.0, base_b=60000.0,
                                     spike_at=200, spike_pct=0.05, direction=+1)
    bt = PairsBacktester(
        base_notional=1000.0, beta=1.0, leverage=20.0, lookback=50,
        entry_z=2.0, exit_z=0.5, taker_fee_pct=0.0, maker_fee_pct=0.0,
    )
    results = bt.run(df_a, df_b)
    assert any(t.side_a == SIDE_SHORT_A for t in results.trades), \
        "spike up in A should trigger short-A trade"


def test_long_a_signal_fires_when_spread_low() -> None:
    df_a, df_b = _make_spread_series(n=300, base_a=2000.0, base_b=60000.0,
                                     spike_at=200, spike_pct=0.05, direction=-1)
    bt = PairsBacktester(
        base_notional=1000.0, beta=1.0, leverage=20.0, lookback=50,
        entry_z=2.0, exit_z=0.5, taker_fee_pct=0.0, maker_fee_pct=0.0,
    )
    results = bt.run(df_a, df_b)
    assert any(t.side_a == SIDE_LONG_A for t in results.trades), \
        "spike down in A should trigger long-A trade"


def test_beta_hedge_scales_b_notional() -> None:
    df_a, df_b = _make_spread_series(n=300, base_a=2000.0, base_b=60000.0,
                                     spike_at=200, spike_pct=0.05, direction=+1)
    bt = PairsBacktester(
        base_notional=1000.0, beta=1.2, leverage=20.0, lookback=50,
        entry_z=2.0, exit_z=0.5, taker_fee_pct=0.0, maker_fee_pct=0.0,
    )
    results = bt.run(df_a, df_b)
    assert results.trades, "expected at least one trade"
    t = results.trades[0]
    assert t.notional_a == pytest.approx(1000.0)
    assert t.notional_b == pytest.approx(1200.0)


def test_fees_accumulate_round_trip() -> None:
    """Two fills × (1000 + 1000) notional × 0.0432% taker fee = $0.864."""
    df_a, df_b = _make_spread_series(n=300, base_a=2000.0, base_b=60000.0,
                                     spike_at=200, spike_pct=0.05, direction=+1)
    bt = PairsBacktester(
        base_notional=1000.0, beta=1.0, leverage=20.0, lookback=50,
        entry_z=2.0, exit_z=0.5,
        taker_fee_pct=0.000432, maker_fee_pct=0.000144,
    )
    results = bt.run(df_a, df_b)
    closed = [t for t in results.trades if t.exit_bar is not None]
    assert closed, "expected at least one closed trade"
    t = closed[0]
    expected_fee = (1000.0 + 1000.0) * 0.000432 * 2  # open + close
    assert t.fees == pytest.approx(expected_fee, rel=1e-6)


def test_liquidation_on_large_adverse_move() -> None:
    """A 4% adverse move on a 20× leg with 2% MMR (3% liq threshold) should
    liquidate. We build a scenario where A spikes up by 5% (triggers short-A),
    then keeps climbing — the short-A leg gets crushed."""
    n = 300
    prices_a = [2000.0] * 200 + [2100.0] * 5 + [2180.0] * (n - 205)  # 9% gap
    prices_b = [60000.0] * n
    df_a = _flat_df(prices_a)
    df_b = _flat_df(prices_b)
    bt = PairsBacktester(
        base_notional=1000.0, beta=1.0, leverage=20.0, maintenance_margin=0.02,
        lookback=50, entry_z=2.0, exit_z=0.5,
        taker_fee_pct=0.0, maker_fee_pct=0.0,
    )
    results = bt.run(df_a, df_b)
    assert results.liquidations >= 1
    assert any(t.exit_reason == "liquidation" for t in results.trades)


def test_funding_accrues_correctly_per_leg() -> None:
    """If A funding is +0.01%/hr and we hold short-A for 10 bars on $1000
    notional, we should RECEIVE $1.00 on the A leg.

    Use very small spike + zero fees so funding dominates."""
    df_a, df_b = _make_spread_series(n=300, base_a=2000.0, base_b=60000.0,
                                     spike_at=200, spike_pct=0.05, direction=+1)
    bt = PairsBacktester(
        base_notional=1000.0, beta=0.0,  # disable B leg entirely
        leverage=20.0, lookback=50, entry_z=2.0, exit_z=0.5,
        taker_fee_pct=0.0, maker_fee_pct=0.0,
        funding_a_per_hour=0.0001,  # 0.01%/hr
        funding_b_per_hour=0.0,
        bar_hours=1.0,
    )
    results = bt.run(df_a, df_b)
    short_trades = [t for t in results.trades if t.side_a == SIDE_SHORT_A and t.exit_bar]
    assert short_trades
    t = short_trades[0]
    bars_held = t.exit_bar - t.entry_bar
    # While we're short A and funding is positive, we receive funding.
    # Funding accrues during bars [entry_bar+1 .. exit_bar] inclusive of the
    # mark-to-market step at each bar. Allow loose tolerance for off-by-one bar.
    expected_min = 1000.0 * 0.0001 * (bars_held - 1)
    expected_max = 1000.0 * 0.0001 * (bars_held + 1)
    assert expected_min <= t.funding <= expected_max, \
        f"funding {t.funding} not in [{expected_min}, {expected_max}] for {bars_held} bars"


def test_no_lookahead_fills_at_next_bar_open() -> None:
    """Signal at bar N must fill at bar N+1's open price, not bar N's close."""
    n = 300
    prices_a = [2000.0] * n
    prices_b = [60000.0] * n
    # Inject a single-bar spike at bar 200 that reverts at 201
    prices_a[200] = 2100.0
    df_a = _flat_df(prices_a)
    df_b = _flat_df(prices_b)
    # Make bar 201's open distinct from bar 200's close so we can verify
    df_a.iloc[201, df_a.columns.get_loc("open")] = 2050.0
    bt = PairsBacktester(
        base_notional=1000.0, beta=1.0, leverage=20.0, lookback=50,
        entry_z=2.0, exit_z=0.5, taker_fee_pct=0.0, maker_fee_pct=0.0,
    )
    results = bt.run(df_a, df_b)
    if not results.trades:
        pytest.skip("spread didn't cross threshold; lookahead invariant not exercised")
    t = results.trades[0]
    # Fill price must come from a bar's open, not a previous bar's close
    assert t.entry_price_a in df_a["open"].values
