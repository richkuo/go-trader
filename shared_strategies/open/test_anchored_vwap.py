
import importlib.util
import os

import numpy as np
import pandas as pd

from shared_strategies.open.conftest import load_module, make_ohlcv

_ANCHORED_VWAP = load_module("_anchored_vwap_test", os.path.join(os.path.dirname(__file__), "anchored_vwap.py"))
anchored_vwap_core = _ANCHORED_VWAP.anchored_vwap_core
_ohlcv = make_ohlcv


def _long_reclaim_df():
    closes = [110, 108, 106, 104, 102, 100,
              100.5, 100.2, 99.8, 99.5,
              103.5, 104.0, 104.5, 105.0]
    return _ohlcv(closes, volume=10.0)


def test_long_signal_fires_once_on_completing_bar():
    df = _long_reclaim_df()
    out = anchored_vwap_core(df, pivot_strength=2, buffer_atr_mult=0.0, confirm_bars=2, atr_period=3)
    sig = out["signal"].to_numpy()
    longs = np.where(sig == 1)[0]
    assert len(longs) == 1, longs
    b = longs[0]
    win_start = b - 2 + 1
    assert out["close"].to_numpy()[win_start - 1] < out["avwap"].to_numpy()[win_start - 1]


_GATE_BASE_KW = dict(pivot_strength=2, buffer_atr_mult=0.0, confirm_bars=2, atr_period=3)


def _high_prior_long_reclaim_df():
    closes = [140, 138, 136, 134, 132, 100,
              100.5, 100.2, 99.8, 99.5,
              103.5, 104.0, 104.5, 105.0]
    return _ohlcv(closes, volume=10.0)


def _load_registry():
    here = os.path.dirname(os.path.abspath(__file__))
    spec = importlib.util.spec_from_file_location(
        "_reg_under_test_avwap", os.path.join(here, "registry.py")
    )
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


