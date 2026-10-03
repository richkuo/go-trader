
import numpy as np
import pandas as pd
import pytest

from shared_strategies.open.conftest import load_module, make_ohlcv as make_frame

_MOMENTUM_PRO = load_module("_momentum_pro_test", __file__.replace("test_momentum_pro.py", "momentum_pro.py"))
momentum_pro_core = _MOMENTUM_PRO.momentum_pro_core


def build_uptrend_with_pullback():
    n = 260
    closes = list(np.linspace(100, 200, n - 6))
    base = closes[-1]
    closes += [base - 4, base - 7, base - 9]
    closes += [base - 4, base + 6, base + 12]
    closes = np.array(closes, dtype=float)
    n = len(closes)
    highs = closes + 1.0
    lows = closes - 1.0
    opens = closes - 0.3
    vol = np.full(n, 100.0)
    vol[-1] = 100.0
    vol[-2] = 500.0
    return make_frame(closes, volume=vol, opens=opens, highs=highs, lows=lows)


def test_uptrend_pullback_fires_long():
    df = build_uptrend_with_pullback()
    out = momentum_pro_core(df, vol_mult=1.2)
    assert (out["signal"] == 1).any(), "expected a long entry on the resumption bar"


def _flat_atr_frame():
    n = 260
    closes = np.full(n, 100.0)
    return make_frame(
        closes, volume=np.full(n, 100.0), opens=closes,
        highs=closes + 1.0, lows=closes - 1.0,
    )


