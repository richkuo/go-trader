"""Tests for the hedged funding-carry pair backtester (#1326).

Synthetic frames only — no DB/network access. Engine invariants exercise the
two-leg accounting; the pure layer (drift/rebalance helpers, leg mapping,
aggregation, verdict) is tested without the engine.
"""

from __future__ import annotations

import os
import sys

import numpy as np
import pandas as pd
import pytest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from backtest_carry_pair import (  # noqa: E402
    CarryPairBacktester,
    CarryResults,
    aggregate_legs,
    bar_hours_from_index,
    carry_verdict,
    delta_drift_pct,
    leg_from_carry_results,
    liquidation_loss,
    rebalance_spot_qty,
)
from eval_windows import LIQUIDATED_DDADJ_FLOOR  # noqa: E402
from backtester import LIQUIDATED_METRIC_FLOOR  # noqa: E402


def _make_df(prices: list[float], signals: list[int],
             accrual: list[float] | None = None,
             perp_prices: list[float] | None = None,
             start: str = "2024-01-01", freq: str = "1h") -> pd.DataFrame:
    n = len(prices)
    idx = pd.date_range(start, periods=n, freq=freq)
    data = {
        "open": prices,
        "high": prices,
        "low": prices,
        "close": prices,
        "volume": [1.0] * n,
        "signal": signals,
    }
    if accrual is not None:
        data["funding_accrual"] = accrual
    if perp_prices is not None:
        data["perp_open"] = perp_prices
        data["perp_close"] = perp_prices
    return pd.DataFrame(data, index=idx)


def _open_hold(n: int) -> list[int]:
    """Signal series that opens at bar 1 and holds short to the end."""
    return [-1] * n


# ---------------------------------------------------------------------------
# Engine invariants.
# ---------------------------------------------------------------------------

def test_funding_booked_on_perp_leg_only() -> None:
    """Flat prices, constant accrual a/bar → funding booked EXACTLY over the
    held interval [entry_bar+1, last_bar], and entirely from the perp leg.

    Opens at bar 1 (fill), holds to the end_of_data close at bar n-1, so the
    funded bars are [2, n-1] = n-2 bars (the entry bar itself is a pre-hold
    interval and is not funded)."""
    n = 50
    a = 0.0001
    df = _make_df([100.0] * n, _open_hold(n), accrual=[a] * n)
    bt = CarryPairBacktester(base_notional=1000.0, leverage=3.0,
                             perp_fee_pct=0.0, spot_fee_pct=0.0, bar_hours=1.0)
    res = bt.run(df)
    assert res.pairs_opened == 1
    funded_bars = n - 2  # [entry_bar+1 .. n-1] = [2 .. 49]
    assert res.bars_funded == funded_bars
    assert res.funding_pnl == pytest.approx(1000.0 * a * funded_bars)
    # No accrual column would mean zero funding — the spot leg never contributes.
    df_no_fund = _make_df([100.0] * n, _open_hold(n))
    assert bt.run(df_no_fund).funding_pnl == 0.0


def test_funding_booked_exactly_over_held_interval_nonconstant() -> None:
    """Non-constant accrual pins the exact booked bars (no ±1 tolerance): a
    pair opened at bar 1 and closed (signal) at bar 6 funds ONLY bars [2, 6].
    Sentinel −999 accruals on the pre-entry bar (entry_bar) and the post-exit
    bar must be excluded — a one-bar window shift would book the wrong sign."""
    n = 10
    prices = [100.0] * n
    # signal: open at bar 1 (sig[0]=-1), hold, close at bar 6 (sig[5]=1).
    sig = [-1, -1, -1, -1, -1, 1, 0, 0, 0, 0]
    accrual = [0.0] * n
    accrual[1] = -999.0                 # entry_bar (pre-hold) — must be excluded
    for j in range(2, 7):               # held bars [2..6] — must be included
        accrual[j] = j * 0.0001
    accrual[7] = -999.0                 # first post-exit bar — must be excluded
    df = _make_df(prices, sig, accrual=accrual)
    bt = CarryPairBacktester(base_notional=750.0, leverage=3.0,
                             perp_fee_pct=0.0, spot_fee_pct=0.0)
    res = bt.run(df)
    assert res.pairs_opened == 1
    assert res.episodes[0].exit_reason == "exit_signal"
    assert res.episodes[0].exit_bar == 6
    # qty_perp × mark (flat 100) = base notional 750 on every held bar.
    expected = 750.0 * sum(j * 0.0001 for j in range(2, 7))
    assert res.funding_pnl == pytest.approx(expected)
    assert res.bars_funded == 5  # bars 2,3,4,5,6 — never the −999 sentinels


def test_funding_roundtrip_books_each_held_interval() -> None:
    """Two open→close cycles fund only their own held intervals; the flat gaps
    between them (position closed) accrue nothing to the strategy."""
    n = 16
    # Cycle 1: open bar 1, close bar 4. Cycle 2: open bar 9, close bar 12.
    sig = [-1, 0, 0, 1, 0, 0, 0, 0, -1, 0, 0, 1, 0, 0, 0, 0]
    accrual = [0.001] * n  # constant, so the answer is just (held-bar count)
    df = _make_df([100.0] * n, sig, accrual=accrual)
    bt = CarryPairBacktester(base_notional=750.0, leverage=3.0,
                             perp_fee_pct=0.0, spot_fee_pct=0.0)
    res = bt.run(df)
    assert res.pairs_opened == 2
    # Cycle 1 funds [2,4] = 3 bars; cycle 2 funds [10,12] = 3 bars → 6 total.
    assert res.bars_funded == 6
    assert res.funding_pnl == pytest.approx(750.0 * 0.001 * 6)


def test_hedge_cancels_price_pnl() -> None:
    """Single-series hedge: perp short loss exactly offsets spot long gain, so
    price PnL nets ~0 regardless of the price path."""
    prices = [100.0, 100.0] + [100.0 + i for i in range(48)]  # ramp up ~48%
    df = _make_df(prices, _open_hold(len(prices)))
    # leverage 1× / 0% MMR → 100% liquidation threshold, so the 48% ramp never
    # liquidates and the single episode's legs cancel exactly.
    bt = CarryPairBacktester(base_notional=1000.0, leverage=1.0,
                             maintenance_margin=0.0,
                             perp_fee_pct=0.0, spot_fee_pct=0.0)
    res = bt.run(df)
    assert res.pairs_opened == 1
    assert res.price_pnl == pytest.approx(0.0, abs=1e-6)


def test_fees_charged_on_both_legs_both_fills() -> None:
    """One round trip on flat prices = (perp_fee + spot_fee) × notional × 2
    (open on base notional + close on the equal exit notional)."""
    n = 20
    sig = [-1] + [0] * 8 + [1] + [0] * (n - 10)  # open bar1, close after bar 9
    df = _make_df([100.0] * n, sig)
    perp_fee, spot_fee = 0.00045, 0.00030
    bt = CarryPairBacktester(base_notional=1000.0, leverage=3.0,
                             perp_fee_pct=perp_fee, spot_fee_pct=spot_fee)
    res = bt.run(df)
    closed = [e for e in res.episodes if e.exit_reason == "exit_signal"]
    assert closed, "expected a signal-driven close"
    expected = (1000.0 * perp_fee + 1000.0 * spot_fee) * 2
    assert closed[0].fees == pytest.approx(expected, rel=1e-6)


def test_no_lookahead_fills_at_next_bar_open() -> None:
    """A signal at bar N fills at bar N+1's OPEN, not bar N's close."""
    n = 10
    prices = [100.0] * n
    df = _make_df(prices, [-1] * n)
    # Make bar 1's open distinct so a close-fill (100) vs open-fill (105) differ.
    df.iloc[1, df.columns.get_loc("open")] = 105.0
    bt = CarryPairBacktester(base_notional=1000.0, leverage=3.0,
                             perp_fee_pct=0.0, spot_fee_pct=0.0)
    res = bt.run(df)
    assert res.episodes
    ep = res.episodes[0]
    assert ep.entry_bar == 1
    assert ep.entry_perp == pytest.approx(105.0)
    assert ep.entry_spot == pytest.approx(105.0)


def test_exit_signal_closes_and_reopens() -> None:
    """signal +1 closes the pair; a later -1 opens a fresh one."""
    n = 30
    sig = [-1] + [0] * 4 + [1] + [0] * 4 + [-1] + [0] * (n - 11)
    df = _make_df([100.0] * n, sig)
    bt = CarryPairBacktester(base_notional=1000.0, leverage=3.0,
                             perp_fee_pct=0.0, spot_fee_pct=0.0)
    res = bt.run(df)
    assert res.pairs_opened == 2
    assert res.episodes[0].exit_reason == "exit_signal"


def test_perp_liquidation_caps_loss_and_credits_spot() -> None:
    """A large upward move liquidates the short perp (isolated margin), but the
    perp loss is capped at the posted margin and the spot leg's offsetting gain
    is still credited in full."""
    n = 20
    # Flat, then a 35% jump — past the 3× / 2% MMR liquidation threshold (31.3%).
    prices = [100.0, 100.0] + [135.0] * (n - 2)
    df = _make_df(prices, _open_hold(n))
    bt = CarryPairBacktester(base_notional=1000.0, leverage=3.0,
                             maintenance_margin=0.02,
                             perp_fee_pct=0.0, spot_fee_pct=0.0)
    res = bt.run(df)
    assert res.perp_liquidations >= 1
    ep = [e for e in res.episodes if e.exit_reason == "liquidation"][0]
    margin = 1000.0 / 3.0
    # Reconstruct perp vs spot components: capped perp loss ≤ margin, spot gain
    # (≈ +$350 on a 35% move) still lifts the episode's net price PnL positive.
    assert ep.price_pnl > 0.0
    assert ep.price_pnl <= 1000.0 * 0.35 - margin + 1e-6


def test_constructor_rejects_bad_params() -> None:
    with pytest.raises(ValueError):
        CarryPairBacktester(initial_capital=0)
    with pytest.raises(ValueError):
        CarryPairBacktester(base_notional=-1)
    with pytest.raises(ValueError):
        CarryPairBacktester(leverage=0)
    with pytest.raises(ValueError):
        CarryPairBacktester(leverage=5.0, maintenance_margin=0.5)
    with pytest.raises(ValueError):
        CarryPairBacktester(entry_threshold=0.0001, exit_threshold=0.0002)
    with pytest.raises(ValueError):
        CarryPairBacktester(drift_threshold=0)
    with pytest.raises(ValueError):
        CarryPairBacktester(bar_hours=0)


def test_run_rejects_missing_columns() -> None:
    bt = CarryPairBacktester()
    with pytest.raises(ValueError):
        bt.run(pd.DataFrame({"open": [1.0], "close": [1.0]}))


# ---------------------------------------------------------------------------
# Drift + rebalancing (pure helpers + engine in basis mode).
# ---------------------------------------------------------------------------

def test_delta_drift_pct_math() -> None:
    # Equal notionals → zero drift.
    assert delta_drift_pct(10.0, 100.0, 10.0, 100.0) == pytest.approx(0.0)
    # Spot notional 5% below perp notional → 5% drift.
    assert delta_drift_pct(10.0, 100.0, 9.5, 100.0) == pytest.approx(5.0)
    # Monotone in divergence.
    assert delta_drift_pct(10.0, 100.0, 8.0, 100.0) > delta_drift_pct(10.0, 100.0, 9.0, 100.0)
    # Degenerate perp notional → 0, never a divide-by-zero.
    assert delta_drift_pct(0.0, 100.0, 10.0, 100.0) == 0.0


def test_rebalance_spot_qty_restores_parity() -> None:
    target, _ = rebalance_spot_qty(qty_perp=10.0, mark_perp=110.0, mark_spot=100.0)
    # Perp notional 1100 / spot mark 100 → 11 spot units.
    assert target == pytest.approx(11.0)
    assert delta_drift_pct(10.0, 110.0, target, 100.0) == pytest.approx(0.0)


def test_basis_drift_triggers_one_rebalance() -> None:
    """In basis mode the perp leg marks on a diverging series, so the notionals
    drift; once past drift_threshold the spot leg is rebalanced to parity."""
    n = 12
    spot = [100.0] * n
    perp = [100.0, 100.0] + [106.0] * (n - 2)  # +6% basis jump → ~5.7% drift
    df = _make_df(spot, _open_hold(n), perp_prices=perp)
    bt = CarryPairBacktester(base_notional=1000.0, leverage=50.0,
                             maintenance_margin=0.0, drift_threshold=2.0,
                             perp_fee_pct=0.0, spot_fee_pct=0.00045)
    res = bt.run(df)
    assert res.rebalances >= 1
    # A rebalance costs a spot-leg fee.
    assert res.fees > 1000.0 * 0.00045 * 1  # more than just the open spot fee


def test_below_threshold_never_rebalances() -> None:
    n = 12
    spot = [100.0] * n
    perp = [100.0, 100.0] + [100.5] * (n - 2)  # +0.5% basis → drift < 2%
    df = _make_df(spot, _open_hold(n), perp_prices=perp)
    bt = CarryPairBacktester(base_notional=1000.0, leverage=50.0,
                             maintenance_margin=0.0, drift_threshold=2.0,
                             perp_fee_pct=0.0, spot_fee_pct=0.0)
    res = bt.run(df)
    assert res.rebalances == 0


def test_single_series_never_drifts() -> None:
    """The default single-series hedge tracks perfectly → zero rebalances."""
    prices = [100.0 + i for i in range(30)]
    df = _make_df(prices, _open_hold(len(prices)))
    bt = CarryPairBacktester(base_notional=1000.0, leverage=50.0,
                             maintenance_margin=0.0, drift_threshold=2.0)
    res = bt.run(df)
    assert res.rebalances == 0


# ---------------------------------------------------------------------------
# Pure aggregation / scoring layer.
# ---------------------------------------------------------------------------

def test_leg_from_results_liquidated_floors() -> None:
    """A liquidated account floors Sharpe and DDadj at the shared constants."""
    res = CarryResults(total_return_pct=-100.0, max_drawdown_pct=-100.0,
                       sharpe=5.0, account_liquidated=True, pairs_opened=1)
    leg = leg_from_carry_results(res)
    assert leg["liquidated"] is True
    assert leg["sharpe"] == -LIQUIDATED_METRIC_FLOOR
    assert leg["ddadj"] == LIQUIDATED_DDADJ_FLOOR


def test_leg_from_results_funding_share() -> None:
    res = CarryResults(total_return_pct=2.0, max_drawdown_pct=-1.0, sharpe=1.5,
                       price_pnl=0.0, funding_pnl=30.0, fees=10.0, pairs_opened=3)
    leg = leg_from_carry_results(res)
    # funding / (|price| + |funding| + fees) = 30 / (0 + 30 + 10) = 0.75
    assert leg["funding_share"] == pytest.approx(0.75)
    assert leg["ddadj"] == pytest.approx(2.0, abs=1e-6)  # +2% / |-1%| dd


def test_aggregate_legs_degenerate_rule() -> None:
    # 1 of 3 traded → majority-must-trade fails → degenerate.
    legs = {
        "A": {"return_pct": 1.0, "sharpe": 1.0, "ddadj": 1.0, "trades": 2,
              "funding_pnl": 5.0, "price_pnl": 0.0, "fees": 1.0, "rebalances": 0,
              "liquidated": False},
        "B": {"return_pct": 0.0, "sharpe": 0.0, "ddadj": 0.0, "trades": 0,
              "funding_pnl": 0.0, "price_pnl": 0.0, "fees": 0.0, "rebalances": 0,
              "liquidated": False},
        "C": {"return_pct": 0.0, "sharpe": 0.0, "ddadj": 0.0, "trades": 0,
              "funding_pnl": 0.0, "price_pnl": 0.0, "fees": 0.0, "rebalances": 0,
              "liquidated": False},
    }
    s = aggregate_legs(legs)
    assert s["traded_datasets"] == 1
    assert s["degenerate"] is True
    # None legs are dropped from the means.
    assert aggregate_legs({"A": None})["datasets"] == 0


def _summary(mean_return, traded=2, datasets=2, liquidated=0, degenerate=False):
    return {"mean_return_pct": mean_return, "traded_datasets": traded,
            "datasets": datasets, "liquidated_legs": liquidated,
            "degenerate": degenerate}


def test_carry_verdict_matrix() -> None:
    # No traded, non-degenerate window anywhere → no_trades.
    assert carry_verdict({"is": _summary(0.0, traded=0)}) == "no_trades"
    assert carry_verdict({"is": _summary(5.0, degenerate=True)}) == "no_trades"
    # Net carry ≤ 0 across traded windows → deprecate.
    assert carry_verdict({"a": _summary(-1.0), "b": _summary(-2.0)}) == "deprecate"
    # Net positive, majority positive, no liquidation → healthy.
    assert carry_verdict({"a": _summary(3.0), "b": _summary(1.0)}) == "healthy"
    # Net positive but a liquidation appeared → marginal.
    assert carry_verdict({"a": _summary(3.0, liquidated=1),
                          "b": _summary(1.0)}) == "marginal"
    # Net positive but minority of windows positive → marginal.
    assert carry_verdict({"a": _summary(10.0), "b": _summary(-1.0),
                          "c": _summary(-1.0)}) == "marginal"


def test_liquidation_loss_math() -> None:
    assert liquidation_loss(1000.0, 3.0, 0.02) == pytest.approx(1000.0 * (1 / 3 - 0.02))
    assert liquidation_loss(1000.0, 10.0, 0.02) == pytest.approx(80.0)


def test_bar_hours_from_index() -> None:
    idx1 = pd.date_range("2024-01-01", periods=5, freq="1h")
    idx4 = pd.date_range("2024-01-01", periods=5, freq="4h")
    assert bar_hours_from_index(idx1) == pytest.approx(1.0)
    assert bar_hours_from_index(idx4) == pytest.approx(4.0)
    assert bar_hours_from_index(pd.Index([1])) == 1.0  # non-datetime fallback
