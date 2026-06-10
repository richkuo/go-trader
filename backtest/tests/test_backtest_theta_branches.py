"""Branch coverage for ThetaHarvestBacktester early-exit + metrics (issue #944).

Before this, the only theta test was ``test_theta_force_close_emits_trade_log_entries``
(end-of-run force-close). The three early-exit branches in ``_check_early_exit``
— profit target, stop loss, DTE floor — and the ``_report`` metrics block had
no coverage. These tests drive ``_check_early_exit`` directly with constructed
``OptionPosition`` objects so each branch is hit deterministically (no reliance
on synthetic price paths landing in a particular regime), and exercise
``_report`` with a known equity curve to pin the metrics arithmetic.
"""
import os
import sys
from datetime import datetime, timedelta

import pytest

_BACKTEST_DIR = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
if _BACKTEST_DIR not in sys.path:
    sys.path.insert(0, _BACKTEST_DIR)

from backtest_options import OptionPosition  # noqa: E402
from backtest_theta import ThetaHarvestBacktester  # noqa: E402


_HIST_VOL = 0.6
_DATE = "2023-06-01"


def _bt(**kw) -> ThetaHarvestBacktester:
    base = dict(
        initial_capital=10_000.0, max_positions=2,
        profit_target_pct=0, stop_loss_pct=0, min_dte_close=0, label="t",
    )
    base.update(kw)
    return ThetaHarvestBacktester(**base)


# ─── Profit-target (theta harvest) branch ────────────────────────────────────


def test_profit_target_branch_closes_and_logs_theta_harvest():
    """A sold deep-OTM call near expiry buys back for ~0 → ~100% profit, which
    clears a 60% profit target. The early-close branch must fire, count an
    ``early_close`` win, and append a ``theta_harvest`` trade-log entry."""
    bt = _bt(profit_target_pct=60)
    # strike 60k vs spot 20k, 2 days left → buyback ≈ 0 (confirmed via BS probe).
    pos = OptionPosition("call", "sell", 60_000.0, expiry_idx=2,
                         premium=0.005, premium_usd=100.0, opened_idx=0)
    closed = bt._check_early_exit(pos, spot=20_000.0, current_idx=0,
                                  hist_vol=_HIST_VOL, date=_DATE)
    assert closed is True
    assert bt.early_closes == 1
    assert bt.stop_losses == 0 and bt.dte_closes == 0
    assert bt.total_trades == 1 and bt.winning_trades == 1
    entry = bt.trade_log[-1]
    assert entry["event"] == "theta_harvest"
    assert entry["action"] == "close_sell"
    assert entry["reason"].startswith("profit_target")
    assert entry["profit_pct"] >= 60


# ─── Stop-loss branch ────────────────────────────────────────────────────────


def test_stop_loss_branch_closes_and_logs_stop_loss():
    """A sold deep-ITM call buys back far above the premium collected → a large
    loss that clears the 150% stop. The stop branch must fire (the profit-target
    branch is checked first but a negative profit can't satisfy it), count a
    loss, and log a ``stop_loss`` event."""
    bt = _bt(profit_target_pct=60, stop_loss_pct=150)
    # strike 12k vs spot 20k, 20 days left → buyback ≈ 8033 ≫ premium 200.
    pos = OptionPosition("call", "sell", 12_000.0, expiry_idx=20,
                         premium=0.01, premium_usd=200.0, opened_idx=0)
    closed = bt._check_early_exit(pos, spot=20_000.0, current_idx=0,
                                  hist_vol=_HIST_VOL, date=_DATE)
    assert closed is True
    assert bt.stop_losses == 1
    assert bt.early_closes == 0 and bt.dte_closes == 0
    assert bt.total_trades == 1 and bt.losing_trades == 1
    entry = bt.trade_log[-1]
    assert entry["event"] == "stop_loss"
    assert entry["reason"].startswith("stop_loss")
    assert entry["loss_pct"] >= 150
    assert entry["pnl"] < 0


# ─── DTE-floor branch ────────────────────────────────────────────────────────


def test_dte_floor_branch_closes_and_logs_dte_close():
    """With profit-target and stop disabled, a position inside the DTE floor
    must close via the DTE branch only."""
    bt = _bt(profit_target_pct=0, stop_loss_pct=0, min_dte_close=5)
    # 2 days left ≤ 5-day floor.
    pos = OptionPosition("call", "sell", 25_000.0, expiry_idx=2,
                         premium=0.005, premium_usd=100.0, opened_idx=0)
    closed = bt._check_early_exit(pos, spot=20_000.0, current_idx=0,
                                  hist_vol=_HIST_VOL, date=_DATE)
    assert closed is True
    assert bt.dte_closes == 1
    assert bt.early_closes == 0 and bt.stop_losses == 0
    assert bt.total_trades == 1
    assert bt.winning_trades + bt.losing_trades == 1
    entry = bt.trade_log[-1]
    assert entry["event"] == "dte_close"
    assert entry["reason"].startswith("dte_floor")
    assert entry["days_left"] == 2


# ─── No-exit / guard paths ───────────────────────────────────────────────────


def test_no_branch_fires_when_within_bounds_leaves_position_open():
    """Modest profit below target, no stop, no DTE floor → position stays open
    and no counters move."""
    bt = _bt(profit_target_pct=90, stop_loss_pct=300, min_dte_close=0)
    # ATM-ish call with time left → buyback meaningfully > 0 but < entry premium.
    pos = OptionPosition("call", "sell", 21_000.0, expiry_idx=30,
                         premium=0.02, premium_usd=2_000.0, opened_idx=0)
    closed = bt._check_early_exit(pos, spot=20_000.0, current_idx=0,
                                  hist_vol=_HIST_VOL, date=_DATE)
    assert closed is False
    assert bt.total_trades == 0 and not bt.trade_log


def test_bought_option_is_never_early_closed():
    """``_check_early_exit`` only manages sold premium — a bought leg returns
    False immediately regardless of price."""
    bt = _bt(profit_target_pct=10, stop_loss_pct=10, min_dte_close=100)
    pos = OptionPosition("call", "buy", 60_000.0, expiry_idx=2,
                         premium=0.005, premium_usd=100.0, opened_idx=0)
    closed = bt._check_early_exit(pos, spot=20_000.0, current_idx=0,
                                  hist_vol=_HIST_VOL, date=_DATE)
    assert closed is False
    assert bt.total_trades == 0


def test_zero_premium_position_is_skipped():
    """A degenerate sold leg with zero entry premium can't compute a profit
    percentage → guarded out, returns False."""
    bt = _bt(profit_target_pct=60, stop_loss_pct=150, min_dte_close=5)
    pos = OptionPosition("call", "sell", 60_000.0, expiry_idx=2,
                         premium=0.0, premium_usd=0.0, opened_idx=0)
    closed = bt._check_early_exit(pos, spot=20_000.0, current_idx=0,
                                  hist_vol=_HIST_VOL, date=_DATE)
    assert closed is False
    assert bt.total_trades == 0


# ─── Metrics block (_report) ─────────────────────────────────────────────────


def test_report_metrics_block_computes_expected_fields():
    """Pin the ``_report`` arithmetic against a known equity curve and tallies.
    Daily sampling (one equity point per calendar day) makes ``days`` the curve
    length — the constraint documented in backtest_theta.py's metrics block."""
    bt = _bt(initial_capital=10_000.0, profit_target_pct=60,
             stop_loss_pct=150, min_dte_close=2)
    bt.cash = 11_000.0  # +10% final
    bt.total_trades = 4
    bt.winning_trades = 3
    bt.losing_trades = 1
    bt.early_closes = 2
    bt.stop_losses = 1
    bt.dte_closes = 1
    bt.total_premium_collected = 500.0

    start = datetime(2023, 1, 1)
    n = 366  # one point per day across a ~1-year span
    bt.equity_curve = [
        ((start + timedelta(days=i)).strftime("%Y-%m-%d"),
         10_000.0 + (1_000.0 * i / (n - 1)))
        for i in range(n)
    ]
    start_date = bt.equity_curve[0][0]
    end_date = bt.equity_curve[-1][0]

    report = bt._report("BTC", start_date, end_date, 20_000.0, 22_000.0)

    assert report["label"] == "t"
    assert report["underlying"] == "BTC"
    assert report["days"] == n  # daily-sampling: curve length == day count
    assert report["initial_capital"] == 10_000.0
    assert report["final_value"] == pytest.approx(11_000.0)
    assert report["total_return_pct"] == pytest.approx(10.0)
    assert report["buy_hold_return_pct"] == pytest.approx(10.0)
    assert report["annualized_return_pct"] == pytest.approx(10.0, abs=0.5)
    assert report["win_rate_pct"] == pytest.approx(75.0)
    assert report["total_trades"] == 4
    assert report["winning_trades"] == 3
    assert report["losing_trades"] == 1
    assert report["early_closes"] == 2
    assert report["stop_losses"] == 1
    assert report["dte_closes"] == 1
    assert report["total_premium_collected"] == pytest.approx(500.0)
    assert report["max_drawdown_pct"] == pytest.approx(0.0)  # monotonic curve
    # Daily Sharpe: positive on a monotone-up curve, and finite.
    assert isinstance(report["sharpe_ratio"], float)
    assert report["sharpe_ratio"] >= 0
    # Config echo-back preserved.
    assert report["profit_target_pct"] == 60
    assert report["stop_loss_pct"] == 150
    assert report["min_dte_close"] == 2


def test_report_zero_trades_yields_zero_win_rate():
    """No trades → win-rate divisor guard returns 0, not a ZeroDivisionError."""
    bt = _bt()
    bt.cash = 10_000.0
    bt.equity_curve = [("2023-01-01", 10_000.0), ("2023-01-02", 10_000.0)]
    report = bt._report("BTC", "2023-01-01", "2023-01-02", 20_000.0, 20_000.0)
    assert report["win_rate_pct"] == 0
    assert report["total_trades"] == 0
