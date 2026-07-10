"""Regression tests for #1271 — intra-bar OHLC-walk SL/TP race resolution.

The engine's default ``intrabar_resolution="ohlc_walk"`` resolves the
engine-tracked stop-loss trigger against each bar's full range: a bar whose
range touches the armed trigger stops the position out ON that bar — at the
trigger price, or at the bar's open when the bar gaps through the trigger —
and wins adverse-move-first over any same-bar close-evaluator exit (the TP a
bar-close mark would have credited). ``intrabar_resolution="bar_close"``
restores the pre-#1271 legacy semantics (SL hit detected on the close only,
filled at the next bar's open) so documented baselines stay reproducible.

Scenarios pinned here (issue #1271 acceptance criteria):
  - same-bar SL+TP races, long and short, where adverse-first flips the
    outcome vs bar-close resolution;
  - gap-through opens (beyond SL, beyond TP) filling at the open;
  - stop fills priced at the trigger level in the non-gap case;
  - no-race runs byte-identical across both modes;
  - the close-anchored trailing seed is not pierce-eligible on its entry bar.
"""
import pandas as pd
import pytest

from backtester import Backtester


TP_5PCT_FULL = [{
    "name": "tiered_tp_pct",
    "params": {"tp_tiers": [{"profit_pct": 0.05, "close_fraction": 1.0}]},
}]


def _df(opens, highs, lows, closes, signals):
    n = len(closes)
    idx = pd.date_range("2024-01-01", periods=n, freq="D")
    return pd.DataFrame(
        {
            "open": [float(v) for v in opens],
            "high": [float(v) for v in highs],
            "low": [float(v) for v in lows],
            "close": [float(v) for v in closes],
            "signal": [float(v) for v in signals],
        },
        index=idx,
    )


def _run(df, **kw):
    kw.setdefault("initial_capital", 10000.0)
    kw.setdefault("commission_pct", 0.0)
    kw.setdefault("slippage_pct", 0.0)
    bt = Backtester(**kw)
    return bt.run(df.copy(), strategy_name="intrabar-test", save=False)


def _race_long_df():
    """Long entry at bar 1 open=100 (SL trigger 97, TP level 105); bar 2
    sweeps both levels (low 96 pierces the stop, close 106 confirms the TP)."""
    return _df(
        opens=[100, 100, 100, 106, 106],
        highs=[101, 101, 107, 107, 107],
        lows=[99, 99, 96, 105, 105],
        closes=[100, 100, 106, 106, 106],
        signals=[1, 0, 0, 0, 0],
    )


def test_same_bar_race_long_adverse_first_stops_out_at_trigger():
    """Default (ohlc_walk): the bar that sweeps both SL and TP stops out at
    the trigger price ON that bar — the same-bar TP the close confirms must
    NOT be credited (adverse-move-first)."""
    res = _run(_race_long_df(), close_strategies=TP_5PCT_FULL,
               stop_loss_pct=0.03)
    assert res["total_trades"] == 1
    trade = res["trades"][0]
    assert trade["exit_price"] == pytest.approx(97.0, rel=1e-9)
    assert trade["exit_reason"] == "sl"
    assert trade["exit_date"] == str(_race_long_df().index[2])
    assert res["final_capital"] == pytest.approx(9700.0, rel=1e-9)


def test_same_bar_race_long_legacy_flag_reproduces_tp_credit():
    """bar_close (legacy) mode reproduces the pre-#1271 outcome on the same
    frame: the SL pierce is invisible at the close, the TP is credited and
    fills at the next bar's open."""
    res = _run(_race_long_df(), close_strategies=TP_5PCT_FULL,
               stop_loss_pct=0.03, intrabar_resolution="bar_close")
    assert res["total_trades"] == 1
    trade = res["trades"][0]
    assert trade["exit_price"] == pytest.approx(106.0, rel=1e-9)
    assert trade["exit_date"] == str(_race_long_df().index[3])
    assert res["final_capital"] == pytest.approx(10600.0, rel=1e-9)


def test_same_bar_race_short_adverse_first_stops_out_at_trigger():
    """Short mirror: trigger 103 above the 100 entry, TP level 95 below; the
    race bar's high pierces the stop while its close confirms the TP —
    adverse-first buys back at the trigger."""
    df = _df(
        opens=[100, 100, 100, 94, 94],
        highs=[101, 101, 104, 95, 95],
        lows=[99, 99, 93, 93, 93],
        closes=[100, 100, 94, 94, 94],
        signals=[-1, 0, 0, 0, 0],
    )
    res = _run(df, close_strategies=TP_5PCT_FULL, stop_loss_pct=0.03,
               direction="short")
    assert res["total_trades"] == 1
    trade = res["trades"][0]
    assert trade["side"] == "short"
    assert trade["exit_price"] == pytest.approx(103.0, rel=1e-9)
    assert trade["exit_reason"] == "sl"
    assert trade["exit_date"] == str(df.index[2])
    assert res["final_capital"] == pytest.approx(9700.0, rel=1e-9)

    legacy = _run(df, close_strategies=TP_5PCT_FULL, stop_loss_pct=0.03,
                  direction="short", intrabar_resolution="bar_close")
    assert legacy["trades"][0]["exit_price"] == pytest.approx(94.0, rel=1e-9)
    assert legacy["final_capital"] == pytest.approx(10600.0, rel=1e-9)


def test_gap_through_sl_long_fills_at_open_not_trigger():
    """A bar that OPENS beyond the stop fills at the open price (the trigger
    price no longer exists in the market), on that bar."""
    df = _df(
        opens=[100, 100, 95, 94, 94],
        highs=[101, 101, 96, 95, 95],
        lows=[99, 99, 94, 93, 93],
        closes=[100, 100, 95, 94, 94],
        signals=[1, 0, 0, 0, 0],
    )
    res = _run(df, close_strategies=TP_5PCT_FULL, stop_loss_pct=0.03)
    assert res["total_trades"] == 1
    trade = res["trades"][0]
    assert trade["exit_price"] == pytest.approx(95.0, rel=1e-9)
    assert trade["exit_reason"] == "sl"
    assert trade["exit_date"] == str(df.index[2])


def test_gap_through_sl_short_fills_at_open_not_trigger():
    """Short mirror of the gap rule: the bar opens above the buy-back
    trigger (103) — fill at the open (105)."""
    df = _df(
        opens=[100, 100, 105, 106, 106],
        highs=[101, 101, 106, 107, 107],
        lows=[99, 99, 104, 105, 105],
        closes=[100, 100, 105, 106, 106],
        signals=[-1, 0, 0, 0, 0],
    )
    res = _run(df, close_strategies=TP_5PCT_FULL, stop_loss_pct=0.03,
               direction="short")
    assert res["total_trades"] == 1
    trade = res["trades"][0]
    assert trade["exit_price"] == pytest.approx(105.0, rel=1e-9)
    assert trade["exit_reason"] == "sl"
    assert trade["exit_date"] == str(df.index[2])


def test_gap_through_tp_fills_at_open_both_modes():
    """A TP decided at bar N's close fills at bar N+1's open even when that
    open gaps beyond the TP level — in both modes (evaluator closes keep the
    N-close -> N+1-open fill contract)."""
    df = _df(
        opens=[100, 100, 100, 110, 110],
        highs=[101, 101, 107, 111, 111],
        lows=[99, 99, 99, 109, 109],
        closes=[100, 100, 106, 110, 110],
        signals=[1, 0, 0, 0, 0],
    )
    for mode in ("ohlc_walk", "bar_close"):
        res = _run(df, close_strategies=TP_5PCT_FULL,
                   intrabar_resolution=mode)
        assert res["total_trades"] == 1, mode
        trade = res["trades"][0]
        assert trade["exit_price"] == pytest.approx(110.0, rel=1e-9), mode
        assert trade["exit_date"] == str(df.index[3]), mode


def test_plain_path_stop_fills_at_trigger_same_bar():
    """Plain signal path (no close evaluator): a low that pierces the stop
    without the close confirming it stops out at the trigger price ON that
    bar under the default; legacy mode never exits and rides to end-of-data."""
    df = _df(
        opens=[100, 100, 100, 105, 115, 120],
        highs=[101, 101, 101, 110, 120, 121],
        lows=[99, 99, 96, 104, 114, 119],
        closes=[100, 100, 98, 105, 115, 120],
        signals=[1, 0, 0, 0, 0, 0],
    )
    res = _run(df, stop_loss_pct=0.03)
    assert res["total_trades"] == 1
    trade = res["trades"][0]
    assert trade["exit_price"] == pytest.approx(97.0, rel=1e-9)
    assert trade["exit_reason"] == "signal_sl"
    assert trade["exit_date"] == str(df.index[2])
    assert res["final_capital"] == pytest.approx(9700.0, rel=1e-9)

    legacy = _run(df, stop_loss_pct=0.03, intrabar_resolution="bar_close")
    assert legacy["total_trades"] == 1
    assert legacy["trades"][0]["exit_reason"] == "end_of_data"
    assert legacy["final_capital"] == pytest.approx(12000.0, rel=1e-9)


def test_partial_tp_at_open_then_intrabar_stop_same_bar():
    """A pending 50% TP leg fills at the bar's open (O comes first in the
    walk); the same bar's low then pierces the stop and the REMAINDER exits
    at the trigger — two legs booked on the same bar."""
    close_refs = [{
        "name": "tiered_tp_pct",
        "params": {"tp_tiers": [{"profit_pct": 0.05, "close_fraction": 0.5}]},
    }]
    df = _df(
        opens=[100, 100, 100, 106, 106],
        highs=[101, 101, 107, 107, 107],
        lows=[99, 99, 99, 96, 96],
        closes=[100, 100, 106, 106, 106],
        signals=[1, 0, 0, 0, 0],
    )
    res = _run(df, close_strategies=close_refs, stop_loss_pct=0.03)
    assert res["total_trades"] == 2
    tp_leg, sl_leg = res["trades"]
    assert tp_leg["exit_price"] == pytest.approx(106.0, rel=1e-9)
    assert tp_leg["exit_date"] == str(df.index[3])
    assert sl_leg["exit_price"] == pytest.approx(97.0, rel=1e-9)
    assert sl_leg["exit_reason"] == "sl"
    assert sl_leg["exit_date"] == str(df.index[3])
    assert res["final_capital"] == pytest.approx(
        50 * 106.0 + 50 * 97.0, rel=1e-9,
    )


def test_no_race_run_identical_across_modes():
    """A run whose bars never touch the armed trigger produces identical
    results under both modes — the walk only changes trigger-touch bars."""
    df = _df(
        opens=[100, 100, 101, 102, 106, 106],
        highs=[101, 101, 103, 107, 107, 107],
        lows=[99, 99, 100, 101, 105, 105],
        closes=[100, 100, 102, 106, 106, 106],
        signals=[1, 0, 0, 0, 0, 0],
    )
    walk = _run(df, close_strategies=TP_5PCT_FULL, stop_loss_pct=0.05)
    legacy = _run(df, close_strategies=TP_5PCT_FULL, stop_loss_pct=0.05,
                  intrabar_resolution="bar_close")
    assert walk == legacy


def test_entry_bar_trailing_seed_is_not_pierce_eligible():
    """The trailing seed is close-anchored (docstring: trailing triggers seed
    on the bar after open) — it is not an armed level DURING the entry bar,
    so the walk must not pierce-check it there: a rally bar whose
    close-anchored trigger (108) sits above the whole session would otherwise
    'gap-fill' a phantom stop at the 100 open."""
    df = _df(
        opens=[100, 100, 110, 110, 110],
        highs=[101, 110, 111, 111, 111],
        lows=[99, 99, 109, 109, 109],
        closes=[100, 110, 110, 110, 110],
        signals=[1, 0, 0, 0, 0],
    )
    df["atr"] = 2.0
    res = _run(df, trailing_stop_atr_mult=1.0)
    assert res["total_trades"] == 1
    trade = res["trades"][0]
    assert trade["exit_reason"] == "end_of_data"
    assert res["final_capital"] > 10500.0


def test_carried_trailing_trigger_is_pierce_eligible_next_bar():
    """From the bar after open the walked trailing trigger is a real carried
    level: a later bar whose low touches it stops out at the trigger price."""
    df = _df(
        opens=[100, 100, 110, 110, 112, 112],
        highs=[101, 110, 111, 111, 112, 112],
        lows=[99, 99, 109, 107, 111, 111],
        closes=[100, 110, 110, 109, 112, 112],
        signals=[1, 0, 0, 0, 0, 0],
    )
    df["atr"] = 2.0
    # Entry bar 1 (fill 100): trigger seeds close-anchored at 110-2=108 and
    # becomes pierce-eligible from bar 2. Bar 3's low (107) pierces 108.
    res = _run(df, trailing_stop_atr_mult=1.0)
    assert res["total_trades"] == 1
    trade = res["trades"][0]
    assert trade["exit_price"] == pytest.approx(108.0, rel=1e-9)
    assert trade["exit_reason"] == "signal_sl"
    assert trade["exit_date"] == str(df.index[3])


def test_sl_after_bump_bar_suppresses_pierce_until_next_bar():
    """#715 x #1271: on the bar where a TP partial fill bumps the SL (live's
    fresh SL OID lands mid-cycle), the intra-bar pierce check is suppressed —
    a low that pierces the just-bumped breakeven trigger on the bump bar must
    NOT exit; the next bar's touch exits at the bumped trigger price."""
    idx = pd.date_range("2024-01-01", periods=5, freq="D")
    df = pd.DataFrame(
        {
            "open": [100.0, 100.0, 100.0, 110.0, 105.0],
            "high": [100.0, 101.0, 110.0, 111.0, 106.0],
            "low": [100.0, 99.0, 100.0, 99.0, 99.5],
            "close": [100.0, 100.0, 110.0, 108.0, 105.0],
            "open_action": ["long", "none", "none", "none", "none"],
            "atr": [10.0] * 5,
        },
        index=idx,
    )
    bt = Backtester(
        initial_capital=1000.0, commission_pct=0.0, slippage_pct=0.0,
        platform="hyperliquid", strategy_type="perps",
        stop_loss_atr_mult=1.0,
        close_strategies=[{
            "name": "tiered_tp_atr",
            "params": {
                "sl_after": "breakeven",
                "tp_tiers": [
                    {"atr_multiple": 1.0, "close_fraction": 0.5},
                    {"atr_multiple": 2.0, "close_fraction": 1.0},
                ],
            },
        }],
    )
    res = bt.run(df, save=False)
    # Leg 1: TP1 partial at bar 3's open (110) — the bump bar, whose low (99)
    # pierces the fresh breakeven trigger (100) but is suppressed. Leg 2: the
    # remainder exits at the bumped trigger on bar 4 (low 99.5 touches 100).
    assert res["total_trades"] == 2
    tp_leg, sl_leg = res["trades"]
    assert tp_leg["exit_price"] == pytest.approx(110.0, rel=1e-9)
    assert tp_leg["exit_date"] == str(idx[3])
    assert sl_leg["exit_price"] == pytest.approx(100.0, rel=1e-9)
    assert sl_leg["exit_reason"] == "sl"
    assert sl_leg["exit_date"] == str(idx[4])
    assert res["final_capital"] == pytest.approx(5 * 110.0 + 5 * 100.0, rel=1e-9)


def test_invalid_intrabar_resolution_rejected():
    with pytest.raises(ValueError, match="intrabar_resolution"):
        Backtester(initial_capital=1000.0, intrabar_resolution="hlc_walk")


# ---------------------------------------------------------------------------
# #1241 entry-fee proration under intra-bar stops (review on PR #1292).
# Invariant: the entry commission of a position closed across any number of
# legs (partial TPs + an intrabar stop remainder) sums to exactly one full
# position's entry fee — with initial_quantity, never the live position, as
# the proration denominator.
# ---------------------------------------------------------------------------

FEE = 0.001            # 10 bps entry commission on a 10_000 full-cash entry
FULL_ENTRY_FEE = 10.0  # 10_000 * FEE (invest = all flat-state cash)


def test_intrabar_stop_full_position_charges_full_entry_fee():
    """A single full-position intrabar stop (no prior partial) charges exactly
    one entry fee — qty_frac must resolve to 1.0, not a prorated fraction."""
    df = _df(
        opens=[100, 100, 100, 100],
        highs=[101, 101, 101, 101],
        lows=[99, 99, 96, 96],
        closes=[100, 100, 96, 96],
        signals=[1, 0, 0, 0],
    )
    res = _run(df, close_strategies=TP_5PCT_FULL, stop_loss_pct=0.03,
               commission_pct=FEE)
    assert res["total_trades"] == 1
    trade = res["trades"][0]
    assert trade["exit_reason"] == "sl"
    assert trade["entry_fee"] == pytest.approx(FULL_ENTRY_FEE, rel=1e-9)


def test_partial_tp_then_intrabar_stop_entry_fees_sum_to_one_fee():
    """The 50% TP leg and the same-bar intrabar-stop remainder each carry half
    the entry fee; together they sum to exactly one full entry fee."""
    close_refs = [{
        "name": "tiered_tp_pct",
        "params": {"tp_tiers": [{"profit_pct": 0.05, "close_fraction": 0.5}]},
    }]
    df = _df(
        opens=[100, 100, 100, 106, 106],
        highs=[101, 101, 107, 107, 107],
        lows=[99, 99, 99, 96, 96],
        closes=[100, 100, 106, 106, 106],
        signals=[1, 0, 0, 0, 0],
    )
    res = _run(df, close_strategies=close_refs, stop_loss_pct=0.03,
               commission_pct=FEE)
    assert res["total_trades"] == 2
    tp_leg, sl_leg = res["trades"]
    assert sl_leg["exit_reason"] == "sl"
    assert tp_leg["entry_fee"] == pytest.approx(FULL_ENTRY_FEE / 2, rel=1e-9)
    assert sl_leg["entry_fee"] == pytest.approx(FULL_ENTRY_FEE / 2, rel=1e-9)


def test_three_way_split_entry_fees_prorate_by_initial_quantity():
    """Two partial TP tiers then an intrabar stop on the remainder: each leg's
    entry fee equals full_fee * leg_shares / initial_shares (denominator is
    the ORIGINAL position size, not the shrinking live position), and the
    three legs sum to exactly one full entry fee."""
    close_refs = [{
        "name": "tiered_tp_pct",
        "params": {"tp_tiers": [
            {"profit_pct": 0.05, "close_fraction": 0.25},
            {"profit_pct": 0.10, "close_fraction": 0.25},
        ]},
    }]
    df = _df(
        opens=[100, 100, 100, 105, 110, 110],
        highs=[101, 101, 106, 111, 111, 111],
        lows=[99, 99, 99, 104, 96, 96],
        closes=[100, 100, 105, 110, 110, 110],
        signals=[1, 0, 0, 0, 0, 0],
    )
    res = _run(df, close_strategies=close_refs, stop_loss_pct=0.03,
               commission_pct=FEE)
    assert res["total_trades"] == 3
    trades = res["trades"]
    assert trades[-1]["exit_reason"] == "sl"
    initial_shares = sum(t["shares"] for t in trades)
    # Full position closed across the three legs.
    assert initial_shares == pytest.approx((10000.0 - FULL_ENTRY_FEE) / 100.0,
                                           rel=1e-9)
    for t in trades:
        assert t["entry_fee"] == pytest.approx(
            FULL_ENTRY_FEE * t["shares"] / initial_shares, rel=1e-6)
    assert sum(t["entry_fee"] for t in trades) == pytest.approx(
        FULL_ENTRY_FEE, rel=1e-9)
