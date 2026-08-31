
import os
import sys

import numpy as np
import pandas as pd

from shared_strategies.open.conftest import load_module

_CONSOLIDATION_RANGE = load_module("_consolidation_range_test", __file__.replace("test_consolidation_range.py", "consolidation_range.py"))
consolidation_range_core = _CONSOLIDATION_RANGE.consolidation_range_core


def _box(n=40, close_level=100.0, top=101.0, bottom=99.0):
    idx = pd.date_range("2024-01-01", periods=n, freq="4h")
    c = np.full(n, close_level)
    return pd.DataFrame(
        {"open": c, "high": np.full(n, top), "low": np.full(n, bottom),
         "close": c, "volume": [1.0] * n},
        index=idx,
    )


def test_long_at_bottom_edge():
    df = _box()
    df.iloc[-1, df.columns.get_loc("close")] = 99.2
    r = consolidation_range_core(df, box_width_pct=0.05, min_bars=16, edge_entry_frac=0.2)
    assert r["signal"].iloc[-1] == 1


def test_short_at_top_edge():
    df = _box()
    df.iloc[-1, df.columns.get_loc("close")] = 100.8
    r = consolidation_range_core(df, box_width_pct=0.05, min_bars=16, edge_entry_frac=0.2)
    assert r["signal"].iloc[-1] == -1


def test_hold_in_middle():
    df = _box()
    r = consolidation_range_core(df, box_width_pct=0.05, min_bars=16, edge_entry_frac=0.2)
    assert r["signal"].iloc[-1] == 0


def test_no_signal_when_not_a_range():
    n = 40
    closes = np.linspace(100, 160, n)
    idx = pd.date_range("2024-01-01", periods=n, freq="4h")
    df = pd.DataFrame({"open": closes, "high": closes + 1, "low": closes - 1,
                       "close": closes, "volume": [1.0] * n}, index=idx)
    r = consolidation_range_core(df, box_width_pct=0.05, min_bars=16, edge_entry_frac=0.2)
    assert (r["signal"] == 0).all()


def test_box_columns_exposed():
    df = _box()
    r = consolidation_range_core(df)
    for col in ["box_top", "box_bottom", "box_mid", "in_range"]:
        assert col in r.columns
