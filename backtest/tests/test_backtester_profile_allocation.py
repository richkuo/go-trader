
import sys
import pathlib

import numpy as np
import pandas as pd
import pytest

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


def test_switcher_flat_switch_after_confirm():
    sw = _ProfileSwitcher(_parse_profile_allocation(_alloc(confirm_bars=2)))
    assert sw.step("up", flat=True) == "fade"
    assert sw.step("up", flat=True) == "trend"
    assert sw.active == "trend"


def test_switcher_open_freezes_then_commits_on_first_flat():
    sw = _ProfileSwitcher(_parse_profile_allocation(_alloc(confirm_bars=2)))
    for _ in range(4):
        assert sw.step("up", flat=False) == "fade"
    assert sw.active == "fade"
    assert sw.step("up", flat=True) == "trend"


def test_switcher_empty_label_freezes():
    sw = _ProfileSwitcher(_parse_profile_allocation(_alloc(confirm_bars=2)))
    sw.step("up", flat=True)
    sw.step("", flat=True)
    assert sw.active == "fade"
    assert sw.step("up", flat=True) == "trend"


def test_switcher_desired_equals_active_resets_pending():
    sw = _ProfileSwitcher(_parse_profile_allocation(_alloc(confirm_bars=3)))
    sw.step("up", flat=True)
    sw.step("down", flat=True)
    assert sw.step("up", flat=True) == "fade"
    assert sw.step("up", flat=True) == "fade"
    assert sw.step("up", flat=True) == "trend"


def test_parse_rejects_wrong_profile_count():
    bad = _alloc()
    bad["param_sets"] = {"a": {}, "b": {}, "c": {}}
    with pytest.raises(ValueError):
        _parse_profile_allocation(bad)


def _flat_df(n=40):
    close = np.full(n, 100.0)
    idx = pd.date_range("2024-01-01", periods=n, freq="D")
    return pd.DataFrame(
        {"open": close, "high": close + 0.01, "low": close - 0.01,
         "close": close, "volume": 1000.0},
        index=idx,
    )


def test_engine_selects_active_profile_signal():
    df = _flat_df(40)
    sig_trend = pd.Series(0, index=df.index)
    sig_trend.iloc[5] = 1
    sig_fade = pd.Series(0, index=df.index)
    df["signal__trend"] = sig_trend.values
    df["signal__fade"] = sig_fade.values
    df["_profile_label"] = "up"

    bt = Backtester(initial_capital=1000.0, commission_pct=0.0, slippage_pct=0.0,
                    profile_allocation=_alloc(confirm_bars=2))
    result = bt.run(df, save=False)
    assert result["total_trades"] >= 1, "trend profile buy should have opened a trade"


def test_engine_no_switch_when_label_stays_fade():
    df = _flat_df(40)
    sig_trend = pd.Series(0, index=df.index)
    sig_trend.iloc[5] = 1
    df["signal__trend"] = sig_trend.values
    df["signal__fade"] = pd.Series(0, index=df.index).values
    df["_profile_label"] = "down"

    bt = Backtester(initial_capital=1000.0, commission_pct=0.0, slippage_pct=0.0,
                    profile_allocation=_alloc(confirm_bars=2))
    result = bt.run(df, save=False)
    assert result["total_trades"] == 0, "fade profile should never fire the trend buy"


def test_engine_requires_profile_columns():
    df = _flat_df(10)
    df["_profile_label"] = "up"
    bt = Backtester(initial_capital=1000.0, profile_allocation=_alloc())
    with pytest.raises(ValueError):
        bt.run(df, save=False)
