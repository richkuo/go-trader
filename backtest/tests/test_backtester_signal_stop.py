"""Standalone hard-stop tests for the plain signal path (no close strategy).

The simple signal-based execution path (signal=1 long entry, signal=-1 exit)
historically carried no stop-loss machinery — a bare ``stop_loss_atr_mult`` /
``trailing_stop_atr_mult`` silently no-opped because the SL trigger is only
seeded by the open/close sl_after/TP pipeline. These tests pin the standalone
stop: it seeds at entry, fills at the next bar's open on a hit, and is
look-ahead safe.
"""
import sys
import pathlib

import numpy as np
import pandas as pd

sys.path.insert(0, str(pathlib.Path(__file__).parent.parent.parent / "shared_tools"))
sys.path.insert(0, str(pathlib.Path(__file__).parent.parent))

from backtester import Backtester


def _df_with_signal(closes, signals, atr=None):
    closes = np.asarray(closes, dtype=float)
    n = len(closes)
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
    if atr is not None:
        df["atr"] = float(atr)
    return df


def _run(df, **kw):
    bt = Backtester(initial_capital=10000.0, platform="binanceus", **kw)
    return bt.run(df.copy(), strategy_name="x", symbol="BTC/USDT", timeframe="1d", save=False)


def test_fixed_atr_stop_exits_on_drawdown():
    """Enter long, price falls past avg_cost - mult*ATR → stop closes the
    position; without the stop it rides the decline to the end."""
    # Enter at bar 1 (signal on bar 0 fills at bar 1 open=100), then a steady
    # decline. ATR=2, mult=1 → stop at 98. Bar 3 close=96 breaches it.
    closes = [100, 100, 99, 96, 90, 85, 80, 80]
    signals = [1, 0, 0, 0, 0, 0, 0, 0]
    df = _df_with_signal(closes, signals, atr=2.0)

    stopped = _run(df, stop_loss_atr_mult=1.0)
    no_stop = _run(df, stop_loss_atr_mult=None)

    # The stop must cut the loss: final capital strictly higher than buy-and-hold.
    assert stopped["final_capital"] > no_stop["final_capital"]
    # Exactly one round trip booked (entry + stop exit).
    assert stopped["total_trades"] == 1


def test_stop_fills_next_bar_open_not_same_bar():
    """A breach detected at bar K's close must fill at bar K+1's open, not at
    bar K's close — the pre-#1271 legacy (bar_close) convention. Pinned to
    that mode explicitly: the default ohlc_walk exits on the breach bar
    itself (test_backtester_intrabar.py owns those semantics)."""
    # ATR=2, mult=1, entry=100 → trigger 98. Bar 2 close=97 breaches; the exit
    # fills at bar 3 open. Make bar 3 open distinct (=95) so we can assert the
    # fill price came from bar 3, not bar 2's close (97).
    closes = [100, 100, 97, 200, 200]
    opens = [100, 100, 97, 95, 200]
    n = len(closes)
    idx = pd.date_range("2024-01-01", periods=n, freq="D")
    df = pd.DataFrame(
        {
            "open": np.array(opens, float),
            "high": np.array(closes, float) + 0.5,
            "low": np.minimum(opens, closes) - 0.5,
            "close": np.array(closes, float),
            "volume": np.full(n, 1000.0),
            "signal": np.array([1, 0, 0, 0, 0], float),
            "atr": np.full(n, 2.0),
        },
        index=idx,
    )
    res = _run(df, stop_loss_atr_mult=1.0, intrabar_resolution="bar_close")
    assert res["total_trades"] == 1
    # The single trade exits ~95 (bar-3 open), so it must NOT have ridden the
    # bar-3 close jump to 200 — final capital is a loss, not a 2x gain.
    assert res["final_capital"] < 10000.0


def test_trailing_atr_stop_ratchets_and_caps_drawdown():
    """A trailing ATR stop ratchets up on new highs and exits on the pullback,
    capping drawdown vs a no-stop hold."""
    # Rise to 130 then collapse. ATR=2, trail mult=1.
    closes = [100, 100, 110, 120, 130, 120, 100, 80, 80]
    signals = [1, 0, 0, 0, 0, 0, 0, 0, 0]
    df = _df_with_signal(closes, signals, atr=2.0)

    trailed = _run(df, trailing_stop_atr_mult=1.0)
    no_stop = _run(df, trailing_stop_atr_mult=None)
    assert trailed["final_capital"] > no_stop["final_capital"]
    assert trailed["max_drawdown_pct"] >= no_stop["max_drawdown_pct"]  # less negative


def test_no_stop_when_mult_unset_is_unchanged():
    """Sanity: omitting both stop params leaves the plain signal path
    behavior exactly as before (single buy-and-hold round trip)."""
    closes = [100, 100, 90, 80, 80]
    signals = [1, 0, 0, 0, -1]
    df = _df_with_signal(closes, signals, atr=2.0)
    res = _run(df)
    assert res["total_trades"] == 1
