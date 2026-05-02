"""Regression test for #536: stale `last` row hides injected `atr` indicator.

Verifies that capturing `last = result_df.iloc[-1]` AFTER `ensure_atr_indicator`
includes the injected `atr` column, whereas capturing before does not.
"""
import sys
import os

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "shared_tools"))

import pandas as pd
from atr import ensure_atr_indicator


def _make_df(n=20):
    return pd.DataFrame(
        {
            "open": [1.0] * n,
            "high": [1.1] * n,
            "low": [0.9] * n,
            "close": [1.0] * n,
            "volume": [1.0] * n,
        }
    )


def test_stale_last_missing_atr():
    """Captures before ensure — atr absent (the bug pattern)."""
    df = _make_df()
    stale_last = df.iloc[-1]
    ensure_atr_indicator(df)
    assert "atr" not in stale_last.index


def test_fresh_last_has_atr():
    """Captures after ensure — atr present (the fix pattern)."""
    df = _make_df()
    ensure_atr_indicator(df)
    fresh_last = df.iloc[-1]
    assert "atr" in fresh_last.index
    import math
    assert math.isfinite(float(fresh_last["atr"]))


def test_noop_when_atr_already_present():
    """Recapturing after ensure is safe when atr is already a column."""
    df = _make_df()
    df["atr"] = 0.05
    original_atr = df.iloc[-1]["atr"]
    ensure_atr_indicator(df)
    fresh_last = df.iloc[-1]
    assert fresh_last["atr"] == original_atr
