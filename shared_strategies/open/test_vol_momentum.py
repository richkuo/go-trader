
import numpy as np
import pandas as pd

from shared_strategies.open.conftest import load_module, make_ohlcv

_VOL_MOMENTUM = load_module("_vol_momentum_test", __file__.replace("test_vol_momentum.py", "vol_momentum.py"))
vol_momentum_core = _VOL_MOMENTUM.vol_momentum_core


def make_uptrend_with_plateau():
    return np.concatenate([
        100 + np.random.RandomState(1).randn(40) * 0.3,
        np.linspace(100, 180, 100),
        180 + np.random.RandomState(3).randn(40) * 0.3,
    ])


def make_downtrend_with_plateau():
    return np.concatenate([
        100 + np.random.RandomState(2).randn(40) * 0.3,
        np.linspace(100, 50, 100),
        50 + np.random.RandomState(4).randn(40) * 0.3,
    ])


def make_round_trip_chop():
    return 100 + np.tile([0, 2, 4, 2, 0, -2, -4, -2], 38)[:300] * 1.5


def test_efficiency_collapse_exits_position():
    out = vol_momentum_core(
        make_ohlcv(make_uptrend_with_plateau(), noise=0.4),
        exit_threshold=-10.0, eff_exit=0.15)
    assert int(out["position"].iloc[-1]) == 0
    assert (out["position"] == 1).any()


