
import importlib.util
import os

import numpy as np
import pandas as pd

from shared_strategies.open.conftest import load_module, make_ohlcv

_ANCHORED_VWAP_REVERSION = load_module("_anchored_vwap_reversion_test", os.path.join(os.path.dirname(__file__), "anchored_vwap_reversion.py"))
anchored_vwap_reversion_core = _ANCHORED_VWAP_REVERSION.anchored_vwap_reversion_core
_ohlcv = make_ohlcv


_PARAMS = dict(pivot_strength=2, entry_atr_mult=1.0, buffer_atr_mult=0.0,
               confirm_bars=2, atr_period=3)


def _long_stretch_df():
    closes = [110, 108, 106, 104, 102, 100, 101, 102, 101, 100.2, 100.6]
    lows = np.asarray(closes) - 0.5
    lows[9] = 99.0
    return _ohlcv(closes, lows=lows, volume=10.0)


def _short_stretch_df():
    closes = [90, 92, 94, 96, 98, 100, 99, 98, 99, 99.8, 99.4]
    highs = np.asarray(closes) + 0.5
    highs[9] = 102.0
    return _ohlcv(closes, highs=highs, volume=10.0)


def test_long_stretch_fires_once_on_completing_bar():
    out = anchored_vwap_reversion_core(_long_stretch_df(), **_PARAMS)
    sig = out["signal"].to_numpy()
    assert sig[10] == 1
    assert (sig == 1).sum() == 1
    assert (sig == -1).sum() == 0
    avwap = out["avwap"].to_numpy()
    atr = out["atr"].to_numpy()
    low = out["low"].to_numpy()
    close = out["close"].to_numpy()
    lower_band = avwap - _PARAMS["entry_atr_mult"] * atr
    assert low[9] <= lower_band[9]
    assert low[8] > lower_band[8]
    assert close[9] < avwap[9] and close[10] < avwap[10]


def _load_registry():
    here = os.path.dirname(os.path.abspath(__file__))
    spec = importlib.util.spec_from_file_location(
        "_reg_under_test_avwap_reversion", os.path.join(here, "registry.py")
    )
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


