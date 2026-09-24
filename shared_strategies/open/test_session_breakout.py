
import numpy as np
import pandas as pd
import pytest

from shared_strategies.open.conftest import load_module

_SESSION_BREAKOUT = load_module("_session_breakout_test", __file__.replace("test_session_breakout.py", "session_breakout.py"))
session_breakout_core = _SESSION_BREAKOUT.session_breakout_core


def _make_df(bars):
    idx = pd.DatetimeIndex([b[0] for b in bars])
    return pd.DataFrame(
        {
            "open":   [b[1] for b in bars],
            "high":   [b[2] for b in bars],
            "low":    [b[3] for b in bars],
            "close":  [b[4] for b in bars],
            "volume": [b[5] for b in bars],
        },
        index=idx,
    )


def test_bearish_breakout_emits_short_signal():
    bars = []
    base = pd.Timestamp("2024-01-01 00:00:00")
    for h in range(8):
        bars.append((base + pd.Timedelta(hours=h), 102, 105, 100, 103, 100.0))
    for h in range(8, 24):
        bars.append((base + pd.Timedelta(hours=h), 102, 103, 101, 102, 100.0))
    day2 = base + pd.Timedelta(days=1)
    for h in range(8):
        bars.append((day2 + pd.Timedelta(hours=h), 101, 103, 100, 101, 100.0))
    bars.append((day2 + pd.Timedelta(hours=9), 100, 100, 95, 96, 500.0))

    df = _make_df(bars)
    result = session_breakout_core(df, session="asian", lookback=1, volume_threshold=1.5)
    assert result["signal"].iloc[-1] == -1


