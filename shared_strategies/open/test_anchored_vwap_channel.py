
import importlib.util
import os

import numpy as np
import pandas as pd

from shared_strategies.open.conftest import load_module, make_ohlcv

_ANCHORED_VWAP_CHANNEL = load_module("_anchored_vwap_channel_test", os.path.join(os.path.dirname(__file__), "anchored_vwap_channel.py"))
anchored_vwap_channel_core = _ANCHORED_VWAP_CHANNEL.anchored_vwap_channel_core
_ohlcv = make_ohlcv


_PARAMS = dict(pivot_strength=2, buffer_atr_mult=0.0, confirm_bars=2,
               min_width_atr_mult=0.0, atr_period=3)


def _long_bounce_df():
    closes = [104, 106, 108, 106, 104, 102, 100, 102, 104, 103, 102.5, 103.5]
    lows = np.asarray(closes) - 0.5
    lows[10] = 101.0
    return _ohlcv(closes, lows=lows, volume=10.0)


def _short_rejection_df():
    closes = [100, 98, 96, 98, 100, 102, 104, 102, 100, 101, 101.5, 100.5]
    highs = np.asarray(closes) + 0.5
    highs[10] = 103.0
    return _ohlcv(closes, highs=highs, volume=10.0)


def test_long_bounce_fires_once_on_completing_bar():
    out = anchored_vwap_channel_core(_long_bounce_df(), **_PARAMS)
    sig = out["signal"].to_numpy()
    assert sig[11] == 1
    assert (sig == 1).sum() == 1
    assert (sig == -1).sum() == 0
    support = out["avwap_support"].to_numpy()
    low = out["low"].to_numpy()
    assert low[10] <= support[10]
    assert low[9] > support[9]


def _load_registry():
    here = os.path.dirname(os.path.abspath(__file__))
    spec = importlib.util.spec_from_file_location(
        "_reg_under_test_avwap_channel", os.path.join(here, "registry.py")
    )
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


