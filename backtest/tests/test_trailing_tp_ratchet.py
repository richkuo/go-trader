"""End-to-end backtester parity for the trailing_tp_ratchet close (#844).

Each cleared TP tier tightens the trailing ATR multiple (and optionally scales
out); the remainder exits via the trailing stop once price reverses. ATR is a
fixed 10 and entry is $100 throughout, so trail triggers are easy to reason
about: for a long, trigger = high_water_mark - trail_mult * 10.
"""
from __future__ import annotations

import pandas as pd

from backtester import Backtester


def _df_open_then_hold(opens, closes, atrs):
    n = len(closes)
    idx = pd.date_range("2024-01-01", periods=n, freq="D")
    return pd.DataFrame(
        {
            "open": opens,
            "close": closes,
            "atr": atrs,
            "open_action": ["long"] + ["none"] * (n - 1),
        },
        index=idx,
    )


def _bt(tiers, trailing_mult=3.0):
    return Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        platform="hyperliquid", strategy_type="perps",
        trailing_stop_atr_mult=trailing_mult,
        close_strategies=[{"name": "trailing_tp_ratchet", "params": {"tp_tiers": tiers}}],
    )


def test_ratchet_scale_out_then_trail_exit():
    # tier1 @1.5×ATR: trail-only → tighten 3.0→2.0. tier2 @3.0×ATR: close 30%,
    # tighten →1.0. Remainder rides the 1.0×ATR trail (hwm 140 → trigger 130).
    df = _df_open_then_hold(
        opens=[100, 100, 116, 131, 140, 140, 140, 130],
        closes=[100, 115, 131, 140, 140, 140, 130, 130],
        atrs=[10] * 8,
    )
    bt = _bt([
        {"atr_multiple": 1.5, "close_fraction": 0.0, "trailing_mult_after": 2.0},
        {"atr_multiple": 3.0, "close_fraction": 0.3, "trailing_mult_after": 1.0},
    ])
    trades = bt.run(df, save=False)["trades"]
    prices = [(t["side"], t["exit_price"]) for t in trades]
    assert ("long", 131.0) in prices, prices   # 30% scale-out at tier2
    assert ("long", 130.0) in prices, prices   # remainder on the tightened trail
    assert len(trades) == 2, prices
    # Remainder is the larger leg (0.7 of initial vs 0.3 scaled out).
    by_exit = {t["exit_price"]: t["shares"] for t in trades}
    assert by_exit[130.0] > by_exit[131.0]


def test_pure_trailing_all_zero_single_exit():
    # Every tier close_fraction 0 → no scale-out; the trail tightens 3.0→2.0→1.0
    # and the whole position exits on one trailing-stop fire.
    df = _df_open_then_hold(
        opens=[100, 100, 121, 130, 120],
        closes=[100, 110, 121, 130, 120],
        atrs=[10] * 5,
    )
    bt = _bt([
        {"atr_multiple": 1.0, "close_fraction": 0.0, "trailing_mult_after": 2.0},
        {"atr_multiple": 2.0, "close_fraction": 0.0, "trailing_mult_after": 1.0},
    ])
    trades = bt.run(df, save=False)["trades"]
    assert len(trades) == 1, [(t["side"], t["exit_price"]) for t in trades]
    assert trades[0]["exit_price"] == 120.0  # hwm 130 - 1.0×ATR


def test_tp_atr_fraction_trail_form():
    # tp_atr_fraction 0.5 at a 2.0×ATR tier → trail = 1.0×ATR. Distinguishes the
    # relative form from the absolute one: a wrong reading (0.5×ATR) would exit
    # at 125, the correct 1.0×ATR exits at 120 (hwm 130 - 1.0×ATR).
    df = _df_open_then_hold(
        opens=[100, 100, 121, 130, 130, 120],
        closes=[100, 121, 121, 130, 120, 120],
        atrs=[10] * 6,
    )
    bt = _bt([{"atr_multiple": 2.0, "close_fraction": 0.3, "tp_atr_fraction": 0.5}])
    trades = bt.run(df, save=False)["trades"]
    prices = [(t["side"], t["exit_price"]) for t in trades]
    assert ("long", 121.0) in prices, prices  # 30% scale-out
    assert ("long", 120.0) in prices, prices  # remainder at 1.0×ATR trail
