"""Tests for the atr_band_revert open strategy — ATR-band mean reversion.

Entry-only core: long when close sinks to/below ``mid - k_entry*ATR``, short
(when ``allow_short``) when close stretches to/above ``mid + k_entry*ATR``.
Exit (TP at mid / opposite band / split) is owned by the close+stop machinery
and is config, not code — see the module docstring.
"""

import os
import sys

import numpy as np
import pandas as pd

sys.path.insert(0, os.path.dirname(__file__))

from atr_band_revert import atr_band_revert_core


def _box(n=60, level=100.0, top=101.0, bottom=99.0):
    """A flat range around ``level`` so mid≈level and ATR is stable."""
    idx = pd.date_range("2024-01-01", periods=n, freq="1h")
    c = np.full(n, level)
    return pd.DataFrame(
        {"open": c, "high": np.full(n, top), "low": np.full(n, bottom),
         "close": c, "volume": [1.0] * n},
        index=idx,
    )


def test_columns_exposed():
    r = atr_band_revert_core(_box())
    for col in ("signal", "atr", "band_mid", "band_lower", "band_upper"):
        assert col in r.columns


def test_long_entry_below_lower_band():
    df = _box()
    # Drive the last close well beneath the lower band.
    df.iloc[-1, df.columns.get_loc("close")] = 90.0
    df.iloc[-1, df.columns.get_loc("low")] = 89.5
    r = atr_band_revert_core(df, period=20, atr_period=14, k_entry=1.5)
    assert r["signal"].iloc[-1] == 1


def test_short_entry_above_upper_band_when_allowed():
    df = _box()
    df.iloc[-1, df.columns.get_loc("close")] = 110.0
    df.iloc[-1, df.columns.get_loc("high")] = 110.5
    r = atr_band_revert_core(df, period=20, atr_period=14, k_entry=1.5, allow_short=True)
    assert r["signal"].iloc[-1] == -1


def test_short_suppressed_when_allow_short_false():
    df = _box()
    df.iloc[-1, df.columns.get_loc("close")] = 110.0
    df.iloc[-1, df.columns.get_loc("high")] = 110.5
    r = atr_band_revert_core(df, period=20, atr_period=14, k_entry=1.5, allow_short=False)
    assert r["signal"].iloc[-1] == 0


def test_hold_inside_bands():
    # Flat box: every close sits at mid, never reaching a band.
    r = atr_band_revert_core(_box(), period=20, atr_period=14, k_entry=1.5, allow_short=True)
    assert r["signal"].iloc[-1] == 0


def test_no_signal_during_warmup():
    # Before the SMA/ATR windows fill, bands are NaN -> no spurious entries.
    r = atr_band_revert_core(_box(), period=20, atr_period=14)
    warm = r.iloc[:19]
    assert (warm["signal"] == 0).all()


def test_long_invariant_holds_everywhere():
    # Property: any bar at/below the lower band is a long; any non-band bar is flat.
    rng = np.random.RandomState(7)
    n = 200
    closes = 100.0 + np.cumsum(rng.randn(n) * 0.5)
    idx = pd.date_range("2024-01-01", periods=n, freq="1h")
    df = pd.DataFrame({"open": closes, "high": closes + 1.0, "low": closes - 1.0,
                       "close": closes, "volume": [1.0] * n}, index=idx)
    r = atr_band_revert_core(df, period=20, atr_period=14, k_entry=1.5, allow_short=False)
    valid = r["band_lower"].notna()
    below = valid & (r["close"] <= r["band_lower"])
    assert (r.loc[below, "signal"] == 1).all()
    assert (r.loc[valid & (r["close"] > r["band_lower"]), "signal"] == 0).all()
