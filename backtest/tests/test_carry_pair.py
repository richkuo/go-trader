
from __future__ import annotations

import os
import sys

import numpy as np
import pandas as pd
import pytest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from backtest_carry_pair import (
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
from eval_windows import LIQUIDATED_DDADJ_FLOOR
from backtester import LIQUIDATED_METRIC_FLOOR


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
    return [-1] * n



def test_funding_booked_on_perp_leg_only() -> None:
    n = 50
    a = 0.0001
    df = _make_df([100.0] * n, _open_hold(n), accrual=[a] * n)
    bt = CarryPairBacktester(base_notional=1000.0, leverage=3.0,
                             perp_fee_pct=0.0, spot_fee_pct=0.0, bar_hours=1.0)
    res = bt.run(df)
    assert res.pairs_opened == 1
    funded_bars = n - 2
    assert res.bars_funded == funded_bars
    assert res.funding_pnl == pytest.approx(1000.0 * a * funded_bars)
    df_no_fund = _make_df([100.0] * n, _open_hold(n))
    assert bt.run(df_no_fund).funding_pnl == 0.0


def test_funding_booked_exactly_over_held_interval_nonconstant() -> None:
    n = 10
    prices = [100.0] * n
    sig = [-1, -1, -1, -1, -1, 1, 0, 0, 0, 0]
    accrual = [0.0] * n
    accrual[1] = -999.0
    for j in range(2, 7):
        accrual[j] = j * 0.0001
    accrual[7] = -999.0
    df = _make_df(prices, sig, accrual=accrual)
    bt = CarryPairBacktester(base_notional=750.0, leverage=3.0,
                             perp_fee_pct=0.0, spot_fee_pct=0.0)
    res = bt.run(df)
    assert res.pairs_opened == 1
    assert res.episodes[0].exit_reason == "exit_signal"
    assert res.episodes[0].exit_bar == 6
    expected = 750.0 * sum(j * 0.0001 for j in range(2, 7))
    assert res.funding_pnl == pytest.approx(expected)
    assert res.bars_funded == 5


def test_funding_roundtrip_books_each_held_interval() -> None:
    n = 16
    sig = [-1, 0, 0, 1, 0, 0, 0, 0, -1, 0, 0, 1, 0, 0, 0, 0]
    accrual = [0.001] * n
    df = _make_df([100.0] * n, sig, accrual=accrual)
    bt = CarryPairBacktester(base_notional=750.0, leverage=3.0,
                             perp_fee_pct=0.0, spot_fee_pct=0.0)
    res = bt.run(df)
    assert res.pairs_opened == 2
    assert res.bars_funded == 6
    assert res.funding_pnl == pytest.approx(750.0 * 0.001 * 6)


def test_exit_bar_funding_valued_at_close_not_open() -> None:
    idx = pd.date_range("2024-01-01", periods=6, freq="1h")
    df = pd.DataFrame({
        "open":   [100.0, 100.0, 100.0, 100.0, 100.0, 100.0],
        "high":   [300.0] * 6,
        "low":    [100.0] * 6,
        "close":  [100.0, 100.0, 100.0, 100.0, 300.0, 100.0],
        "volume": [1.0] * 6,
        "signal": [-1, 0, 0, 1, 0, 0],
        "funding_accrual": [0.0, 0.0, 0.0, 0.0, 0.001, 0.0],
    }, index=idx)
    bt = CarryPairBacktester(base_notional=1000.0, leverage=1.0,
                             maintenance_margin=0.0, perp_fee_pct=0.0,
                             spot_fee_pct=0.0)
    res = bt.run(df)
    ep = res.episodes[0]
    assert ep.exit_reason == "exit_signal"
    assert ep.exit_bar == 4
    assert res.bars_funded == 1
    assert res.funding_pnl == pytest.approx(3.0)


def test_hedge_cancels_price_pnl() -> None:
    prices = [100.0, 100.0] + [100.0 + i for i in range(48)]
    df = _make_df(prices, _open_hold(len(prices)))
    bt = CarryPairBacktester(base_notional=1000.0, leverage=1.0,
                             maintenance_margin=0.0,
                             perp_fee_pct=0.0, spot_fee_pct=0.0)
    res = bt.run(df)
    assert res.pairs_opened == 1
    assert res.price_pnl == pytest.approx(0.0, abs=1e-6)


def test_fees_charged_on_both_legs_both_fills() -> None:
    n = 20
    sig = [-1] + [0] * 8 + [1] + [0] * (n - 10)
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
    n = 10
    prices = [100.0] * n
    df = _make_df(prices, [-1] * n)
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
    n = 30
    sig = [-1] + [0] * 4 + [1] + [0] * 4 + [-1] + [0] * (n - 11)
    df = _make_df([100.0] * n, sig)
    bt = CarryPairBacktester(base_notional=1000.0, leverage=3.0,
                             perp_fee_pct=0.0, spot_fee_pct=0.0)
    res = bt.run(df)
    assert res.pairs_opened == 2
    assert res.episodes[0].exit_reason == "exit_signal"


def test_perp_liquidation_caps_loss_and_credits_spot() -> None:
    n = 20
    prices = [100.0, 100.0] + [135.0] * (n - 2)
    df = _make_df(prices, _open_hold(n))
    bt = CarryPairBacktester(base_notional=1000.0, leverage=3.0,
                             maintenance_margin=0.02,
                             perp_fee_pct=0.0, spot_fee_pct=0.0)
    res = bt.run(df)
    assert res.perp_liquidations >= 1
    ep = [e for e in res.episodes if e.exit_reason == "liquidation"][0]
    margin = 1000.0 / 3.0
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



def test_delta_drift_pct_math() -> None:
    assert delta_drift_pct(10.0, 100.0, 10.0, 100.0) == pytest.approx(0.0)
    assert delta_drift_pct(10.0, 100.0, 9.5, 100.0) == pytest.approx(5.0)
    assert delta_drift_pct(10.0, 100.0, 8.0, 100.0) > delta_drift_pct(10.0, 100.0, 9.0, 100.0)
    assert delta_drift_pct(0.0, 100.0, 10.0, 100.0) == 0.0


def test_rebalance_spot_qty_restores_parity() -> None:
    target, _ = rebalance_spot_qty(qty_perp=10.0, mark_perp=110.0, mark_spot=100.0)
    assert target == pytest.approx(11.0)
    assert delta_drift_pct(10.0, 110.0, target, 100.0) == pytest.approx(0.0)


def test_basis_drift_triggers_one_rebalance() -> None:
    n = 12
    spot = [100.0] * n
    perp = [100.0, 100.0] + [106.0] * (n - 2)
    df = _make_df(spot, _open_hold(n), perp_prices=perp)
    bt = CarryPairBacktester(base_notional=1000.0, leverage=50.0,
                             maintenance_margin=0.0, drift_threshold=2.0,
                             perp_fee_pct=0.0, spot_fee_pct=0.00045)
    res = bt.run(df)
    assert res.rebalances >= 1
    assert res.fees > 1000.0 * 0.00045 * 1


def test_below_threshold_never_rebalances() -> None:
    n = 12
    spot = [100.0] * n
    perp = [100.0, 100.0] + [100.5] * (n - 2)
    df = _make_df(spot, _open_hold(n), perp_prices=perp)
    bt = CarryPairBacktester(base_notional=1000.0, leverage=50.0,
                             maintenance_margin=0.0, drift_threshold=2.0,
                             perp_fee_pct=0.0, spot_fee_pct=0.0)
    res = bt.run(df)
    assert res.rebalances == 0


def test_single_series_never_drifts() -> None:
    prices = [100.0 + i for i in range(30)]
    df = _make_df(prices, _open_hold(len(prices)))
    bt = CarryPairBacktester(base_notional=1000.0, leverage=50.0,
                             maintenance_margin=0.0, drift_threshold=2.0)
    res = bt.run(df)
    assert res.rebalances == 0


def test_rebalance_preserves_spot_leg_pnl_across_sell_rebalances() -> None:
    spot = [100.0, 100.0, 200.0, 250.0, 250.0]
    perp = [100.0] * 5
    sig = [-1, 0, 0, 0, 0]
    df = _make_df(spot, sig, perp_prices=perp)
    bt = CarryPairBacktester(initial_capital=100_000.0, base_notional=1000.0,
                             leverage=3.0, drift_threshold=2.0,
                             perp_fee_pct=0.0, spot_fee_pct=0.0)
    res = bt.run(df)
    ep = res.episodes[0]
    assert ep.rebalances == 2
    assert ep.realized_spot_pnl == pytest.approx(650.0)
    assert ep.price_pnl == pytest.approx(1250.0)


def test_rebalance_that_increases_qty_prices_new_units_from_purchase() -> None:
    spot = [100.0, 100.0, 50.0, 100.0, 100.0]
    perp = [100.0] * 5
    sig = [-1, 0, 0, 0, 0]
    df = _make_df(spot, sig, perp_prices=perp)
    bt = CarryPairBacktester(initial_capital=100_000.0, base_notional=1000.0,
                             leverage=3.0, drift_threshold=2.0,
                             perp_fee_pct=0.0, spot_fee_pct=0.0)
    res = bt.run(df)
    ep = res.episodes[0]
    assert ep.rebalances == 2
    assert ep.price_pnl == pytest.approx(500.0)


def test_rebalance_pnl_survives_into_signal_close() -> None:
    spot = [100.0, 100.0, 200.0, 200.0, 200.0, 200.0]
    perp = [100.0] * 6
    sig = [-1, 0, 0, 1, 0, 0]
    df = _make_df(spot, sig, perp_prices=perp)
    bt = CarryPairBacktester(initial_capital=100_000.0, base_notional=1000.0,
                             leverage=3.0, drift_threshold=2.0,
                             perp_fee_pct=0.0, spot_fee_pct=0.0)
    res = bt.run(df)
    ep = res.episodes[0]
    assert ep.exit_reason == "exit_signal"
    assert ep.rebalances == 1
    assert ep.realized_spot_pnl == pytest.approx(500.0)
    assert ep.price_pnl == pytest.approx(1000.0)



def test_leg_from_results_liquidated_floors() -> None:
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
    assert leg["funding_share"] == pytest.approx(0.75)
    assert leg["ddadj"] == pytest.approx(2.0, abs=1e-6)


def test_aggregate_legs_degenerate_rule() -> None:
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
    assert aggregate_legs({"A": None})["datasets"] == 0


def _summary(mean_return, traded=2, datasets=2, liquidated=0, degenerate=False):
    return {"mean_return_pct": mean_return, "traded_datasets": traded,
            "datasets": datasets, "liquidated_legs": liquidated,
            "degenerate": degenerate}


def test_carry_verdict_matrix() -> None:
    assert carry_verdict({"is": _summary(0.0, traded=0)}) == "no_trades"
    assert carry_verdict({"is": _summary(5.0, degenerate=True)}) == "no_trades"
    assert carry_verdict({"a": _summary(-1.0), "b": _summary(-2.0)}) == "deprecate"
    assert carry_verdict({"a": _summary(3.0), "b": _summary(1.0)}) == "healthy"
    assert carry_verdict({"a": _summary(3.0, liquidated=1),
                          "b": _summary(1.0)}) == "marginal"
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
    assert bar_hours_from_index(pd.Index([1])) == 1.0
