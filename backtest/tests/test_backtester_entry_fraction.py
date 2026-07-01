"""#980: per-bar ``entry_fraction`` entry sizing.

A strategy-emitted ``entry_fraction`` column in (0, 1] scales the notional
committed at open; the remainder stays as a cash reserve. The column is a
decision input, so it shifts forward one bar with the signal. Absent column,
NaN values, and fraction 1.0 are all byte-identical to today's full-notional
behavior. These tests pin: long/short plain-path math, the open/close engine
path, next-bar-open shift alignment, validation, NaN semantics, and the
reserve-preserving (additive) close/SL fills.
"""
import sys
import pathlib

import numpy as np
import pandas as pd
import pytest

sys.path.insert(0, str(pathlib.Path(__file__).parent.parent.parent / "shared_tools"))
sys.path.insert(0, str(pathlib.Path(__file__).parent.parent))

from backtester import Backtester


def _df(closes, signals, entry_fraction=None, opens=None):
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
    if entry_fraction is not None:
        df["entry_fraction"] = np.asarray(entry_fraction, dtype=float)
    return df


def _run(df, **kw):
    kw.setdefault("commission_pct", 0.0)
    kw.setdefault("slippage_pct", 0.0)
    bt = Backtester(initial_capital=10000.0, **kw)
    return bt.run(df.copy(), strategy_name="x", symbol="BTC/USDT",
                  timeframe="1d", save=False)


# ─── Plain-path mechanics ─────────────────────────────────────────────────────


def test_long_fractional_entry_commits_fraction_and_keeps_reserve():
    # Signal bar 0 fills bar 1 open (100) at fraction 0.5: 5000 invested,
    # 50 shares, 5000 reserve. Close signal bar 2 fills bar 3 open (120):
    # proceeds 6000 land ON TOP of the reserve.
    closes = [100, 100, 120, 120, 120]
    signals = [1, 0, -1, 0, 0]
    res = _run(_df(closes, signals, entry_fraction=[0.5] * 5))
    assert res["total_trades"] == 1
    assert res["trades"][0]["shares"] == pytest.approx(50.0)
    assert res["final_capital"] == pytest.approx(11000.0)


def test_short_fractional_entry_commits_fraction_and_keeps_reserve():
    # direction="short": margin 5000 → 50 shares short at 100, cash
    # 10000 - 5000 + 2*5000 = 15000; cover at 80 costs 4000 → 11000.
    closes = [100, 100, 80, 80, 80]
    signals = [-1, 0, 1, 0, 0]
    res = _run(_df(closes, signals, entry_fraction=[0.5] * 5),
               direction="short")
    assert res["total_trades"] == 1
    assert res["trades"][0]["side"] == "short"
    assert res["trades"][0]["shares"] == pytest.approx(50.0)
    assert res["final_capital"] == pytest.approx(11000.0)


def test_entry_fraction_uses_signal_bar_value_not_fill_bar():
    # The column shifts with the signal: the bar-0 signal fills at bar 1's
    # open using bar 0's fraction (0.25 → 25 shares), never bar 1's (0.75).
    closes = [100, 100, 100, 100]
    signals = [1, 0, -1, 0]
    res = _run(_df(closes, signals, entry_fraction=[0.25, 0.75, 0.75, 0.75]))
    assert res["total_trades"] == 1
    assert res["trades"][0]["shares"] == pytest.approx(25.0)


def test_fraction_one_column_matches_no_column_exactly():
    closes = [100, 105, 98, 110, 104, 120, 115, 108]
    signals = [1, 0, -1, 1, 0, 0, -1, 0]
    base = _run(_df(closes, signals),
                commission_pct=0.001, slippage_pct=0.0005)
    scaled = _run(_df(closes, signals, entry_fraction=[1.0] * 8),
                  commission_pct=0.001, slippage_pct=0.0005)
    assert scaled["final_capital"] == pytest.approx(base["final_capital"])
    assert scaled["total_trades"] == base["total_trades"]


def test_nan_entry_fraction_means_full_notional():
    closes = [100, 100, 120, 120]
    signals = [1, 0, -1, 0]
    base = _run(_df(closes, signals))
    nan_col = _run(_df(closes, signals, entry_fraction=[np.nan] * 4))
    assert nan_col["final_capital"] == pytest.approx(base["final_capital"])
    assert nan_col["trades"][0]["shares"] == pytest.approx(
        base["trades"][0]["shares"])


def test_compounding_reuses_reserve_plus_proceeds():
    # Round trip 1 at fraction 0.5 (flat price): cash back to 10000. Round
    # trip 2 re-commits half of the FULL 10000 — the reserve rejoined the pool.
    closes = [100, 100, 100, 100, 100, 100, 100]
    signals = [1, 0, -1, 1, 0, -1, 0]
    res = _run(_df(closes, signals, entry_fraction=[0.5] * 7))
    assert res["total_trades"] == 2
    assert res["trades"][1]["shares"] == pytest.approx(50.0)
    assert res["final_capital"] == pytest.approx(10000.0)


# ─── Reserve-preserving close fills ───────────────────────────────────────────


def test_signal_close_preserves_reserve():
    # Regression for the historical ``cash = proceeds - commission``
    # overwrite on the plain-path long close: with a 0.5 entry the 5000
    # reserve must survive the close, not be replaced by the proceeds.
    closes = [100, 100, 90, 90, 90]
    signals = [1, 0, -1, 0, 0]
    res = _run(_df(closes, signals, entry_fraction=[0.5] * 5))
    # 50 shares closed at 90 → 4500 proceeds + 5000 reserve.
    assert res["final_capital"] == pytest.approx(9500.0)


def test_standalone_stop_fill_preserves_reserve():
    # Same regression on the standalone-stop fill: SL at -10% from 100
    # triggers on bar 3's close (85) and fills bar 4's open (85);
    # 50 * 85 = 4250 proceeds + 5000 reserve.
    closes = [100, 100, 95, 85, 85, 85]
    signals = [1, 0, 0, 0, 0, 0]
    res = _run(_df(closes, signals, entry_fraction=[0.5] * 6),
               stop_loss_pct=0.10)
    assert res["total_trades"] == 1
    assert res["final_capital"] == pytest.approx(9250.0)


# ─── Open/close engine path ───────────────────────────────────────────────────


def test_engine_path_long_fractional_entry():
    idx = pd.date_range("2024-01-01", periods=5, freq="D")
    df = pd.DataFrame({
        "open": [100.0] * 5,
        "high": [101.0] * 5,
        "low": [99.0] * 5,
        "close": [100.0] * 5,
        "volume": [1000.0] * 5,
        "open_action": ["none", "long", "none", "none", "none"],
        "close_fraction": [0, 0, 1, 0, 0],
        "entry_fraction": [np.nan, 0.5, np.nan, np.nan, np.nan],
    }, index=idx)
    bt = Backtester(initial_capital=1000, commission_pct=0, slippage_pct=0)
    res = bt.run(df, save=False)
    assert res["total_trades"] == 1
    assert res["trades"][0]["shares"] == pytest.approx(5.0)
    assert res["final_capital"] == pytest.approx(1000.0)


def test_engine_path_short_fractional_entry():
    idx = pd.date_range("2024-01-01", periods=5, freq="D")
    df = pd.DataFrame({
        "open": [100.0, 100.0, 100.0, 80.0, 80.0],
        "high": [101.0] * 5,
        "low": [79.0] * 5,
        "close": [100.0, 100.0, 80.0, 80.0, 80.0],
        "volume": [1000.0] * 5,
        "open_action": ["none", "short", "none", "none", "none"],
        "close_fraction": [0, 0, 1, 0, 0],
        "entry_fraction": [np.nan, 0.5, np.nan, np.nan, np.nan],
    }, index=idx)
    bt = Backtester(initial_capital=1000, commission_pct=0, slippage_pct=0)
    res = bt.run(df, save=False)
    assert res["total_trades"] == 1
    assert res["trades"][0]["side"] == "short"
    assert res["trades"][0]["shares"] == pytest.approx(5.0)
    # 1000 - 500 margin + 2*500 proceeds = 1500; cover at 80 costs 400.
    assert res["final_capital"] == pytest.approx(1100.0)


# ─── Validation ───────────────────────────────────────────────────────────────


def test_zero_entry_fraction_rejected():
    closes = [100, 100, 100]
    signals = [1, 0, 0]
    with pytest.raises(ValueError, match=r"entry_fraction .* \(0, 1\]"):
        _run(_df(closes, signals, entry_fraction=[0.0, 1.0, 1.0]))


def test_out_of_range_entry_fraction_rejected():
    closes = [100, 100, 100]
    signals = [1, 0, 0]
    with pytest.raises(ValueError, match=r"entry_fraction .* \(0, 1\]"):
        _run(_df(closes, signals, entry_fraction=[1.5, 1.0, 1.0]))
