
import os
import sys

import numpy as np
import pandas as pd

from shared_strategies.open.conftest import load_module

_ATR_BAND_REVERT = load_module("_atr_band_revert_test", __file__.replace("test_atr_band_revert.py", "atr_band_revert.py"))
atr_band_revert_core = _ATR_BAND_REVERT.atr_band_revert_core


def _box(n=60, level=100.0, top=101.0, bottom=99.0):
    idx = pd.date_range("2024-01-01", periods=n, freq="1h")
    c = np.full(n, level)
    return pd.DataFrame(
        {"open": c, "high": np.full(n, top), "low": np.full(n, bottom),
         "close": c, "volume": [1.0] * n},
        index=idx,
    )


def test_long_entry_below_lower_band():
    df = _box()
    df.iloc[-1, df.columns.get_loc("close")] = 90.0
    df.iloc[-1, df.columns.get_loc("low")] = 89.5
    r = atr_band_revert_core(df, period=20, atr_period=14, k_entry=1.5)
    assert r["signal"].iloc[-1] == 1


