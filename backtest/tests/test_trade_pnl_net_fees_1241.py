"""
Regression tests for issue #1241 — per-trade ``Trade.pnl`` must be net of BOTH
the entry and the exit commission, at every close site.

Before the fix only the engine/registry close path deducted the *exit* fee
from ``pnl`` (and never the entry fee); the plain signal / stop-loss close
paths deducted *neither* — so those legs reported PnL gross of both fees.
That inflated win-rate and profit_factor relative to live's net-PnL
convention (``tradeNetPnL``). ``_stamp_hold`` is now the single netting
chokepoint: it deducts the pro-rated entry fee and the exit fee from the gross
``pnl`` that ``Trade.close()`` set, so every close site nets identically.

Two regressions:

1. ``test_plain_close_gross_winner_flips_to_net_loss`` — a marginal gross
   winner on the plain signal path whose small positive move is swamped by the
   two fees; once netted it classifies as a LOSS, moving both win-rate and
   profit_factor. This is the leg class that previously netted NOTHING, so it
   fails hard against pre-fix code (it would classify as a win there).
2. ``test_partial_close_prorated_entry_fees_sum_and_net`` — a tiered partial
   then full close on the engine path; each leg's pro-rated entry fee is its
   share of the single entry commission (the legs' fees sum to the whole), and
   each leg's ``pnl`` equals gross minus its own entry+exit fee (proving the
   engine path nets the exit fee exactly once — no double-deduct).
"""
import pandas as pd
import pytest

from backtester import Backtester


COMMISSION = 0.001
INITIAL_CAPITAL = 1000.0


def _df_signals(opens, signals):
    """Build a plain signal df with explicit per-bar open prices.

    A signal at bar N fills at bar N+1's open (look-ahead-safe).
    """
    n = len(opens)
    idx = pd.date_range("2024-01-01", periods=n, freq="D")
    return pd.DataFrame({"open": opens, "close": opens, "signal": signals}, index=idx)


def _df_open_then_hold(opens, closes, atrs, side="long"):
    n = len(closes)
    idx = pd.date_range("2024-01-01", periods=n, freq="D")
    open_actions = [side] + ["none"] * (n - 1)
    return pd.DataFrame(
        {"open": opens, "close": closes, "open_action": open_actions, "atr": atrs},
        index=idx,
    )


def test_plain_close_gross_winner_flips_to_net_loss():
    # signal=1 at bar 1 -> open long at bar 2 open ($100.00).
    # signal=-1 at bar 3 -> close at bar 4 open ($100.15).
    # Gross move +$0.15/share is smaller than entry+exit fees (~$0.20/share at
    # 0.1% a side on ~$100), so the net trade is a LOSS despite a gross gain.
    df = _df_signals(
        opens=[100.0, 100.0, 100.0, 100.0, 100.15, 100.15],
        signals=[0, 1, 0, -1, 0, 0],
    )
    bt = Backtester(
        initial_capital=INITIAL_CAPITAL,
        commission_pct=COMMISSION,
        slippage_pct=0.0,
    )
    result = bt.run(df, save=False)

    assert result["total_trades"] == 1
    trade = result["trades"][0]

    assert trade["entry_fee"] > 0.0
    assert trade["exit_fee"] > 0.0

    # Gross pnl (what pre-fix code reported on this leg) was a WIN...
    gross = trade["pnl"] + trade["entry_fee"] + trade["exit_fee"]
    assert gross > 0.0

    # ...but the net pnl is a LOSS once both fees are deducted.
    assert trade["pnl"] < 0.0

    # Classification flips accordingly: pre-fix win_rate would be 100 / PF inf.
    assert result["win_rate"] == 0.0
    assert result["profit_factor"] == 0.0


def test_partial_close_prorated_entry_fees_sum_and_net():
    # ATR=10 throughout; two tiers close 50% at 1xATR then 100% at 2xATR.
    # Entry at bar 1 open ($100). Bar 2 close=$110 -> tier 1 (close half at bar
    # 3 open=$110). Bar 3 close=$120 -> tier 2 (close rest at bar 4 open=$120).
    df = _df_open_then_hold(
        opens=[100, 100, 100, 110, 120],
        closes=[100, 100, 110, 120, 120],
        atrs=[10, 10, 10, 10, 10],
    )
    bt = Backtester(
        initial_capital=INITIAL_CAPITAL,
        commission_pct=COMMISSION,
        slippage_pct=0.0,
        close_strategies=[
            {"name": "tiered_tp_atr", "params": {"tp_tiers": [
                {"atr_multiple": 1.0, "close_fraction": 0.5},
                {"atr_multiple": 2.0, "close_fraction": 1.0},
            ]}},
        ],
    )
    result = bt.run(df, save=False)

    assert result["total_trades"] == 2
    leg0, leg1 = result["trades"][0], result["trades"][1]

    # The single entry commission is initial_capital * commission_pct; the two
    # legs' pro-rated entry fees sum back to that whole (each leg gets its
    # share of the one entry fee, fractions summing to 1).
    total_entry_fee = INITIAL_CAPITAL * COMMISSION
    assert leg0["entry_fee"] + leg1["entry_fee"] == pytest.approx(total_entry_fee)

    for leg in (leg0, leg1):
        entry_px, exit_px, shares = leg["entry_price"], leg["exit_price"], leg["shares"]
        gross = shares * (exit_px - entry_px)
        expected_exit_fee = shares * exit_px * COMMISSION
        assert leg["exit_fee"] == pytest.approx(expected_exit_fee)
        # Net pnl = gross minus this leg's own entry AND exit fee -- the exit
        # fee is netted exactly once (the engine path no longer double-deducts).
        expected_net = gross - leg["entry_fee"] - leg["exit_fee"]
        assert leg["pnl"] == pytest.approx(expected_net, abs=0.01)

    # Both legs are still gross winners here; net PnL stays positive but is
    # strictly less than gross by the summed fees.
    net_total = leg0["pnl"] + leg1["pnl"]
    gross_total = (leg0["shares"] * (leg0["exit_price"] - leg0["entry_price"])
                   + leg1["shares"] * (leg1["exit_price"] - leg1["entry_price"]))
    assert net_total < gross_total


def test_partial_then_end_of_data_entry_fees_sum_to_whole():
    # Review #1250 finding: a tiered partial close (engine path) followed by an
    # end-of-data close must NOT double-count the entry fee. One tier closes
    # 50% at 1×ATR; the remainder rides to the end of the data and is force-
    # closed there. The two legs' pro-rated entry fees must sum to the single
    # entry commission (fractions 0.5 + 0.5 = 1), and the remainder leg's pnl
    # must net exactly one pro-rated entry fee, not the full one.
    df = _df_open_then_hold(
        opens=[100, 100, 100, 110, 110],
        closes=[100, 100, 110, 110, 110],
        atrs=[10, 10, 10, 10, 10],
    )
    bt = Backtester(
        initial_capital=INITIAL_CAPITAL,
        commission_pct=COMMISSION,
        slippage_pct=0.0,
        close_strategies=[
            {"name": "tiered_tp_atr", "params": {"tp_tiers": [
                {"atr_multiple": 1.0, "close_fraction": 0.5},
                # Second tier at 5×ATR never triggers → remainder hits EOD.
                {"atr_multiple": 5.0, "close_fraction": 1.0},
            ]}},
        ],
    )
    result = bt.run(df, save=False)

    assert result["total_trades"] == 2
    leg0, leg1 = result["trades"][0], result["trades"][1]
    assert leg1["exit_reason"] == "end_of_data"

    total_entry_fee = INITIAL_CAPITAL * COMMISSION
    # The whole entry fee is split across the two legs exactly once.
    assert leg0["entry_fee"] + leg1["entry_fee"] == pytest.approx(total_entry_fee)
    # Each ~half the position → ~half the entry fee each; the EOD remainder leg
    # must NOT carry the full entry fee.
    assert leg1["entry_fee"] == pytest.approx(total_entry_fee * 0.5)

    for leg in (leg0, leg1):
        entry_px, exit_px, shares = leg["entry_price"], leg["exit_price"], leg["shares"]
        gross = shares * (exit_px - entry_px)
        expected_net = gross - leg["entry_fee"] - leg["exit_fee"]
        assert leg["pnl"] == pytest.approx(expected_net, abs=0.01)


def test_avg_loss_uses_net_convention_not_gross_pnl_pct():
    # Review #1250 optional finding: a gross-winner/net-loser (test 1's trade
    # class) is bucketed as a loss on net pnl, so avg_loss must reflect a
    # NEGATIVE net return, not the trade's POSITIVE gross pnl_pct.
    df = _df_signals(
        opens=[100.0, 100.0, 100.0, 100.0, 100.15, 100.15],
        signals=[0, 1, 0, -1, 0, 0],
    )
    bt = Backtester(
        initial_capital=INITIAL_CAPITAL,
        commission_pct=COMMISSION,
        slippage_pct=0.0,
    )
    result = bt.run(df, save=False)

    # The sole trade is a net loss; its gross pnl_pct is positive.
    assert result["trades"][0]["pnl"] < 0.0
    assert result["trades"][0]["pnl_pct"] > 0.0
    # avg_loss must be net-negative, not pulled positive by the gross figure.
    assert result["avg_loss_pct"] < 0.0
    assert result["avg_win_pct"] == 0.0


def test_short_partial_then_end_of_data_entry_fees_sum_to_whole():
    # Review #1250 (round 2): drive the position < 0 engine partial-close
    # branch through the end-of-data pro-rating so short sign-handling
    # (positive shares / initial_quantity against a negative position) is
    # verified in CI. Short entry at bar 1 open ($100); price FALLS to $90
    # (1×ATR profit) → tier 1 closes 50% at bar 3 open=$90; the 5×ATR tier
    # never triggers so the remainder rides to end-of-data at the final close.
    df = _df_open_then_hold(
        opens=[100, 100, 100, 90, 90],
        closes=[100, 100, 90, 90, 90],
        atrs=[10, 10, 10, 10, 10],
        side="short",
    )
    bt = Backtester(
        initial_capital=INITIAL_CAPITAL,
        commission_pct=COMMISSION,
        slippage_pct=0.0,
        close_strategies=[
            {"name": "tiered_tp_atr", "params": {"tp_tiers": [
                {"atr_multiple": 1.0, "close_fraction": 0.5},
                {"atr_multiple": 5.0, "close_fraction": 1.0},
            ]}},
        ],
    )
    result = bt.run(df, save=False)

    assert result["total_trades"] == 2
    leg0, leg1 = result["trades"][0], result["trades"][1]
    assert leg0["side"] == "short"
    assert leg1["side"] == "short"
    assert leg1["exit_reason"] == "end_of_data"

    total_entry_fee = INITIAL_CAPITAL * COMMISSION
    # Entry fee split across the two short legs exactly once (0.5 + 0.5 = 1).
    assert leg0["entry_fee"] + leg1["entry_fee"] == pytest.approx(total_entry_fee)
    assert leg1["entry_fee"] == pytest.approx(total_entry_fee * 0.5)

    for leg in (leg0, leg1):
        entry_px, exit_px, shares = leg["entry_price"], leg["exit_price"], leg["shares"]
        # Short gross = shares × (entry − exit).
        gross = shares * (entry_px - exit_px)
        expected_net = gross - leg["entry_fee"] - leg["exit_fee"]
        assert leg["pnl"] == pytest.approx(expected_net, abs=0.01)
