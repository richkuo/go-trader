"""Tests for the consolidation_range open strategy."""

import os
import sys

import numpy as np
import pandas as pd

sys.path.insert(0, os.path.dirname(__file__))

from consolidation_range import consolidation_range_core


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
    df.iloc[-1, df.columns.get_loc("close")] = 99.2  # bottom 10% of the box
    r = consolidation_range_core(df, box_width_pct=0.05, min_bars=16, edge_entry_frac=0.2)
    assert r["signal"].iloc[-1] == 1


def test_short_at_top_edge():
    df = _box()
    df.iloc[-1, df.columns.get_loc("close")] = 100.8  # top 10%
    r = consolidation_range_core(df, box_width_pct=0.05, min_bars=16, edge_entry_frac=0.2)
    assert r["signal"].iloc[-1] == -1


def test_hold_in_middle():
    df = _box()  # close == mid
    r = consolidation_range_core(df, box_width_pct=0.05, min_bars=16, edge_entry_frac=0.2)
    assert r["signal"].iloc[-1] == 0


def test_no_signal_when_not_a_range():
    # Wide trending series -> box_width never within 5%, no entries.
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
