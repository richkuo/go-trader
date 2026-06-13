"""#989: short/flat plain signal path — the mirror of the structural
long/flat path, engaged by ``direction="short"`` with no close evaluator.

signal=-1 OPENS a short, signal=+1 CLOSES it (live open-as-close semantics on
a short-only strategy). These tests pin the mechanics: entry/exit fills, PnL
sign, fees, next-bar-open alignment, standalone stops mirrored above the
entry, regime gating, invert_signal composition, and the guards that keep
direction="both" and walk-forward long-seeding off this path.
"""
import sys
import pathlib

import numpy as np
import pandas as pd
import pytest

sys.path.insert(0, str(pathlib.Path(__file__).parent.parent.parent / "shared_tools"))
sys.path.insert(0, str(pathlib.Path(__file__).parent.parent))

from backtester import Backtester


def _df(closes, signals, opens=None, atr=None):
    closes = np.asarray(closes, dtype=float)
    n = len(closes)
    opens = closes if opens is None else np.asarray(opens, dtype=float)
    idx = pd.date_range("2024-01-01", periods=n, freq="D")
    df = pd.DataFrame(
        {
            "open": opens,
            "high": np.maximum(opens, closes) + 0.5,
            "low": np.minimum(opens, closes) - 0.5,
            "close": closes,
            "volume": np.full(n, 1000.0),
            "signal": np.asarray(signals, dtype=float),
        },
        index=idx,
    )
    if atr is not None:
        df["atr"] = float(atr)
    return df


def _run(df, **kw):
    kw.setdefault("direction", "short")
    kw.setdefault("commission_pct", 0.0)
    kw.setdefault("slippage_pct", 0.0)
    bt = Backtester(initial_capital=10000.0, **kw)
    return bt.run(df.copy(), strategy_name="x", symbol="BTC/USDT",
                  timeframe="1d", save=False)


# ─── Round-trip mechanics ─────────────────────────────────────────────────────


def test_short_round_trip_profits_when_price_falls():
    # Signal at bar 0 fills at bar 1 open (100); buy-back signal at bar 3
    # fills at bar 4 open (80). 20% favourable move on a short.
    closes = [100, 100, 90, 80, 80, 80]
    signals = [-1, 0, 0, 1, 0, 0]
    res = _run(_df(closes, signals))
    assert res["total_trades"] == 1
    t = res["trades"][0]
    assert t["side"] == "short"
    assert t["entry_price"] == pytest.approx(100.0)
    assert t["exit_price"] == pytest.approx(80.0)
    assert t["pnl"] > 0
    assert res["final_capital"] == pytest.approx(12000.0)


def test_short_round_trip_loses_when_price_rises():
    closes = [100, 100, 110, 120, 120, 120]
    signals = [-1, 0, 0, 1, 0, 0]
    res = _run(_df(closes, signals))
    assert res["total_trades"] == 1
    assert res["trades"][0]["pnl"] < 0
    assert res["final_capital"] == pytest.approx(8000.0)


def test_short_entry_fills_next_bar_open_not_signal_bar():
    # Signal on bar 1 (close=100) must fill at bar 2's DISTINCT open (95) —
    # the N-close → N+1-open convention, no look-ahead.
    closes = [100, 100, 100, 90, 90]
    opens = [100, 100, 95, 90, 90]
    signals = [0, -1, 0, 0, 1]
    res = _run(_df(closes, signals, opens=opens))
    assert res["total_trades"] == 1
    assert res["trades"][0]["entry_price"] == pytest.approx(95.0)


def test_commission_and_slippage_charged_on_both_legs():
    closes = [100, 100, 100, 100, 100]
    signals = [-1, 0, 1, 0, 0]
    free = _run(_df(closes, signals))
    costly = _run(_df(closes, signals), commission_pct=0.001,
                  slippage_pct=0.0005)
    assert free["final_capital"] == pytest.approx(10000.0)
    # Flat prices: every dollar lost is friction, on entry AND exit.
    assert costly["final_capital"] < free["final_capital"] - 25.0


def test_long_signal_while_flat_opens_nothing_in_short_mode():
    # +1 while flat is a no-op (it only CLOSES a short); -1 while short is a
    # hold. End-of-data flush closes the one short opened by the first -1.
    closes = [100, 100, 100, 100, 100]
    signals = [1, 0, -1, -1, 0]
    res = _run(_df(closes, signals))
    assert [t["side"] for t in res["trades"]] == ["short"]
    assert res["trades"][0]["exit_reason"] == "end_of_data"


def test_invert_signal_composes_with_short_mode():
    # invert flips first: a raw +1 becomes -1 and opens the short; the raw
    # -1 becomes +1 and closes it. Mirrors the live order (invert then gate).
    closes = [100, 100, 90, 80, 80]
    signals = [1, 0, -1, 0, 0]
    res = _run(_df(closes, signals), invert_signal=True)
    assert res["total_trades"] == 1
    t = res["trades"][0]
    assert t["side"] == "short"
    assert t["pnl"] > 0


def test_regime_gate_blocks_short_entries():
    closes = [100, 100, 90, 80, 80]
    signals = [-1, 0, 0, 0, 0]
    df = _df(closes, signals)
    df["regime"] = "bullish"
    res = _run(df, regime_enabled=True, allowed_regimes=["bearish"])
    assert res["total_trades"] == 0
    assert res["final_capital"] == pytest.approx(10000.0)


def test_end_of_data_flush_closes_open_short():
    closes = [100, 100, 90, 85, 80]
    signals = [-1, 0, 0, 0, 0]
    res = _run(_df(closes, signals))
    assert res["total_trades"] == 1
    t = res["trades"][0]
    assert t["exit_reason"] == "end_of_data"
    assert t["pnl"] > 0  # closed at the final bar's lower close


# ─── Standalone stops, mirrored above the entry ───────────────────────────────


def test_fixed_atr_stop_buys_back_on_adverse_rally():
    # Entry 100, ATR=2, mult=1 → trigger 102. Bar 2 close=103 breaches;
    # buy-back fills at bar 3 open. Without the stop the short rides to 130.
    closes = [100, 100, 103, 110, 120, 130, 130]
    signals = [-1, 0, 0, 0, 0, 0, 0]
    df = _df(closes, signals, atr=2.0)
    stopped = _run(df, stop_loss_atr_mult=1.0)
    no_stop = _run(df)
    assert stopped["total_trades"] == 1
    assert stopped["trades"][0]["exit_reason"] == "signal_sl"
    assert stopped["final_capital"] > no_stop["final_capital"]


def test_fixed_atr_stop_fills_next_bar_open():
    # Breach at bar 2's close (103 > 102) fills at bar 3's distinct open
    # (104), not at bar 2's close — and not at bar 3's close spike (200).
    closes = [100, 100, 103, 200, 200]
    opens = [100, 100, 103, 104, 200]
    signals = [-1, 0, 0, 0, 0]
    df = _df(closes, signals, opens=opens, atr=2.0)
    res = _run(df, stop_loss_atr_mult=1.0)
    assert res["total_trades"] == 1
    assert res["trades"][0]["exit_price"] == pytest.approx(104.0)


def test_trailing_atr_stop_tightens_down_and_exits_on_bounce():
    # Price falls to 70 then bounces. ATR=2, trail mult=1: the trigger walks
    # down with the low-water mark and the bounce closes the short, beating
    # the no-stop hold that gives the move back.
    closes = [100, 100, 90, 80, 70, 80, 95, 100, 100]
    signals = [-1, 0, 0, 0, 0, 0, 0, 0, 0]
    df = _df(closes, signals, atr=2.0)
    trailed = _run(df, trailing_stop_atr_mult=1.0)
    no_stop = _run(df)
    assert trailed["total_trades"] == 1
    assert trailed["trades"][0]["exit_reason"] == "signal_sl"
    assert trailed["final_capital"] > no_stop["final_capital"]


def test_trailing_stop_never_loosens_on_adverse_move():
    # After the low-water mark is set, a higher close must not push the
    # trigger back up: the bar-4 bounce to 75 breaches the bar-3 trigger
    # (70 + 2 = 72) even though 75's own candidate would be higher.
    closes = [100, 100, 85, 70, 75, 75]
    signals = [-1, 0, 0, 0, 0, 0]
    df = _df(closes, signals, atr=2.0)
    res = _run(df, trailing_stop_atr_mult=1.0)
    assert res["total_trades"] == 1
    assert res["trades"][0]["exit_reason"] == "signal_sl"
    # Buy-back fills at bar 5's open (75): profit vs the 100 entry.
    assert res["trades"][0]["pnl"] > 0


def test_fixed_pct_stop_mirrors_above_entry():
    # stop_loss_pct=0.05 on a 100 entry → trigger 105. Bar 2 close=106
    # breaches; without the stop the short rides to 130.
    closes = [100, 100, 106, 115, 130, 130]
    signals = [-1, 0, 0, 0, 0, 0]
    df = _df(closes, signals)
    stopped = _run(df, stop_loss_pct=0.05)
    no_stop = _run(df)
    assert stopped["total_trades"] == 1
    assert stopped["final_capital"] > no_stop["final_capital"]


# ─── Guards ──────────────────────────────────────────────────────────────────


def test_starting_long_rejected_on_short_path():
    closes = [100, 100, 100]
    signals = [0, 0, 0]
    bt = Backtester(initial_capital=10000.0, direction="short",
                    commission_pct=0.0, slippage_pct=0.0)
    with pytest.raises(ValueError, match="starting_long"):
        bt.run(_df(closes, signals), save=False,
               starting_long={"entry_price": 100.0})


def test_direction_both_rejected_on_plain_path():
    # PR #1004 review: "both" is unmodelable on the single-leg path (one
    # signal cannot open one side and close the other). The loaders reject it,
    # but API callers can construct Backtester directly — run() must refuse
    # rather than silently score a long/flat run as "both".
    closes = [100, 100, 110, 120, 120]
    signals = [1, 0, 0, -1, 0]
    with pytest.raises(ValueError, match="direction='both'"):
        _run(_df(closes, signals), direction="both")


def test_direction_long_default_is_unchanged():
    # The long/flat path must be byte-identical with and without an explicit
    # direction="long" (regression guard for the plain_short branch).
    closes = [100, 100, 110, 120, 120]
    signals = [1, 0, 0, -1, 0]
    base = _run(_df(closes, signals), direction=None)
    explicit = _run(_df(closes, signals), direction="long")
    assert base["final_capital"] == explicit["final_capital"]
    assert [t["side"] for t in base["trades"]] == ["long"]
    assert [t["side"] for t in explicit["trades"]] == ["long"]


# ─── Bust-account entry guard (PR #1004 review) ──────────────────────────────
# Invariant: an opened position's sign always matches its booked trade side.
# A short losing >100% leaves flat-state cash <= 0; opening from it would
# compute negative shares — a phantom opposite-side position with inverted
# PnL. Entries must skip instead, on BOTH paths and BOTH sides.


def test_no_short_reentry_after_blowup_plain_path():
    # Short 100 → buy back 250 leaves cash at -5000. The later -1 must NOT
    # open: a phantom long would profit from the 250→50 crash (final > -5000),
    # a sign-flipped "short" booking would corrupt PnL. Cash stays put.
    closes = [100, 100, 250, 250, 100, 50, 50]
    signals = [-1, 0, 1, -1, 0, 0, 0]
    res = _run(_df(closes, signals))
    assert res["total_trades"] == 1
    assert res["trades"][0]["pnl"] < 0
    assert res["final_capital"] == pytest.approx(-5000.0)


def test_consecutive_reentry_attempts_after_blowup_do_not_compound():
    closes = [100, 100, 250, 250, 100, 80, 50]
    signals = [-1, 0, 1, -1, -1, -1, 0]
    res = _run(_df(closes, signals))
    assert res["total_trades"] == 1
    assert res["final_capital"] == pytest.approx(-5000.0)


def test_cash_zero_boundary_books_no_phantom_trade():
    # Exact-wipeout buy-back (100 → 200, fee-free) leaves cash == 0.0; the
    # next -1 must not open a zero-share trade (dangling current_trade).
    closes = [100, 100, 200, 200, 200]
    signals = [-1, 0, 1, -1, 0]
    res = _run(_df(closes, signals))
    assert res["total_trades"] == 1
    assert len(res["trades"]) == 1
    assert res["final_capital"] == pytest.approx(0.0)


def test_no_short_reentry_after_blowup_engine_path():
    # Same invariant via the open/close engine path: time_stop force-closes
    # the short at 300 (-100% and change, cash -10000); the later -1
    # open_action must skip, not book a sign-flipped entry that would
    # "profit" from the 300→100 fall.
    closes = [100, 100, 200, 300, 300, 100, 100]
    signals = [-1, 0, 0, 0, -1, 0, 0]
    df = _df(closes, signals, atr=2.0)
    res = _run(df, close_strategies=[
        {"name": "time_stop", "params": {"max_bars": 2}},
    ])
    assert res["total_trades"] == 1
    assert res["trades"][0]["side"] == "short"
    assert res["final_capital"] == pytest.approx(-10000.0)


def test_no_long_open_after_short_blowup_engine_path_both():
    # Inverse side, same state: with direction="both" a +1 after the blowup
    # would open a LONG from negative cash (negative shares booked as
    # "long"). The guard must cover the long open block too.
    closes = [100, 100, 200, 300, 300, 300, 300]
    signals = [-1, 0, 0, 0, 1, 0, 0]
    df = _df(closes, signals, atr=2.0)
    res = _run(df, direction="both", close_strategies=[
        {"name": "time_stop", "params": {"max_bars": 2}},
    ])
    assert res["total_trades"] == 1
    assert res["trades"][0]["side"] == "short"
    assert res["final_capital"] == pytest.approx(-10000.0)


def test_short_mode_with_close_refs_uses_engine_path_not_plain():
    # With a close evaluator the engine path owns execution: direction=short
    # masks long opens, raw -1 opens the short, and the evaluator closes it.
    # The plain short/flat interpretation (+1 closes) must NOT apply: the
    # later +1 is masked, so the position rides to end-of-data.
    closes = [100, 100, 90, 80, 80]
    signals = [-1, 0, 1, 0, 0]
    df = _df(closes, signals, atr=2.0)
    never_fires = [{"name": "tiered_tp_pct", "params": {"tp_tiers": [
        {"profit_pct": 0.9, "close_fraction": 1.0},
    ]}}]
    res = _run(df, close_strategies=never_fires)
    assert [t["side"] for t in res["trades"]] == ["short"]
    assert res["trades"][0]["exit_reason"] == "end_of_data"
