"""Backtester regime-profile allocation (#998) parity tests.

These exercise the in-engine switch replay: per-profile ``signal__<p>`` columns,
the ``_profile_label`` shift, and the flat-only / confirm_bars hysteresis state
machine that mirrors the Go ``resolveRegimeProfile``.
"""

import sys
import pathlib

import numpy as np
import pandas as pd

sys.path.insert(0, str(pathlib.Path(__file__).parent.parent.parent / "shared_tools"))
sys.path.insert(0, str(pathlib.Path(__file__).parent.parent))

from backtester import Backtester, _ProfileSwitcher, _parse_profile_allocation


def _alloc(confirm_bars=2):
    return {
        "profiles": {"up": "trend", "down": "fade"},
        "param_sets": {"trend": {"k": 1}, "fade": {"k": 0}},
        "confirm_bars": confirm_bars,
        "initial_profile": "fade",
    }


# ─── State machine (mirrors Go resolveRegimeProfile) ─────────────────────────

def test_switcher_flat_switch_after_confirm():
    sw = _ProfileSwitcher(_parse_profile_allocation(_alloc(confirm_bars=2)))
    assert sw.step("up", flat=True) == "fade"   # bar 1: pending, not confirmed
    assert sw.step("up", flat=True) == "trend"  # bar 2: confirmed → switch
    assert sw.active == "trend"


def test_switcher_open_freezes_then_commits_on_first_flat():
    sw = _ProfileSwitcher(_parse_profile_allocation(_alloc(confirm_bars=2)))
    # Position open the whole time; counter accrues but no commit.
    for _ in range(4):
        assert sw.step("up", flat=False) == "fade"
    assert sw.active == "fade"
    # First flat bar commits immediately (counter already satisfied).
    assert sw.step("up", flat=True) == "trend"


def test_switcher_empty_label_freezes():
    sw = _ProfileSwitcher(_parse_profile_allocation(_alloc(confirm_bars=2)))
    sw.step("up", flat=True)  # pending=trend seen=1
    sw.step("", flat=True)    # freeze — no change
    assert sw.active == "fade"
    # The frozen counter resumes; one more "up" confirms.
    assert sw.step("up", flat=True) == "trend"


def test_switcher_desired_equals_active_resets_pending():
    sw = _ProfileSwitcher(_parse_profile_allocation(_alloc(confirm_bars=3)))
    sw.step("up", flat=True)            # pending=trend seen=1
    sw.step("down", flat=True)          # desired==active(fade) → reset
    # Now it takes a full confirm window again.
    assert sw.step("up", flat=True) == "fade"
    assert sw.step("up", flat=True) == "fade"
    assert sw.step("up", flat=True) == "trend"


def test_parse_rejects_wrong_profile_count():
    bad = _alloc()
    bad["param_sets"] = {"a": {}, "b": {}, "c": {}}
    try:
        _parse_profile_allocation(bad)
        assert False, "expected ValueError"
    except ValueError as e:
        assert "exactly 2" in str(e)


# ─── End-to-end engine replay ────────────────────────────────────────────────

def _flat_df(n=40):
    close = np.full(n, 100.0)
    idx = pd.date_range("2024-01-01", periods=n, freq="D")
    return pd.DataFrame(
        {"open": close, "high": close + 0.01, "low": close - 0.01,
         "close": close, "volume": 1000.0},
        index=idx,
    )


def test_engine_selects_active_profile_signal():
    """Profile 'trend' fires a buy; profile 'fade' never does. With the label
    held at 'up' and confirm_bars=2, the engine switches to 'trend' and the
    trend profile's buy opens a position; the fade profile alone would not."""
    df = _flat_df(40)
    # trend profile buys on bar 5; fade profile is always flat.
    sig_trend = pd.Series(0, index=df.index)
    sig_trend.iloc[5] = 1
    sig_fade = pd.Series(0, index=df.index)
    df["signal__trend"] = sig_trend.values
    df["signal__fade"] = sig_fade.values
    df["_profile_label"] = "up"  # sustained trending regime

    bt = Backtester(initial_capital=1000.0, commission_pct=0.0, slippage_pct=0.0,
                    profile_allocation=_alloc(confirm_bars=2))
    result = bt.run(df, save=False)
    assert result["total_trades"] >= 1, "trend profile buy should have opened a trade"


def test_engine_no_switch_when_label_stays_fade():
    """With the label held at 'down' (→ fade), the engine never switches to
    'trend', so the trend profile's buy is never read and no trade opens."""
    df = _flat_df(40)
    sig_trend = pd.Series(0, index=df.index)
    sig_trend.iloc[5] = 1
    df["signal__trend"] = sig_trend.values
    df["signal__fade"] = pd.Series(0, index=df.index).values
    df["_profile_label"] = "down"  # fade regime throughout

    bt = Backtester(initial_capital=1000.0, commission_pct=0.0, slippage_pct=0.0,
                    profile_allocation=_alloc(confirm_bars=2))
    result = bt.run(df, save=False)
    assert result["total_trades"] == 0, "fade profile should never fire the trend buy"


def test_engine_requires_profile_columns():
    df = _flat_df(10)
    df["_profile_label"] = "up"
    bt = Backtester(initial_capital=1000.0, profile_allocation=_alloc())
    try:
        bt.run(df, save=False)
        assert False, "expected ValueError for missing signal columns"
    except ValueError as e:
        assert "missing signal columns" in str(e)
