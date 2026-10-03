
import numpy as np
import pandas as pd

from shared_strategies.open.conftest import load_module, make_ohlcv

_REGIME_ADAPTIVE = load_module("_regime_adaptive_test", __file__.replace("test_regime_adaptive.py", "regime_adaptive.py"))
regime_adaptive_core = _REGIME_ADAPTIVE.regime_adaptive_core

PIN = dict(period=20, adx_threshold=25.0, return_eff_threshold=0.05,
           range_eff_threshold=0.03, efficiency_threshold=0.5,
           breakout_lookback=20, mr_lookback=20, mr_entry_z=1.5, mr_exit_z=0.0)


def make_range_with_swings(base=100.0, cycles=14, seed=5):
    rng = np.random.RandomState(seed)
    seg = []
    for k in range(cycles):
        seg += list(base + rng.randn(12) * 0.4)
        if k % 2 == 0:
            seg += [base - 3, base - 6, base - 9, base - 11, base - 7, base - 2]
        else:
            seg += [base + 3, base + 6, base + 9, base + 11, base + 7, base + 2]
    return np.array(seg, dtype=float)


def make_clean_uptrend():
    return np.concatenate([
        100 + np.random.RandomState(1).randn(60) * 0.3,
        np.linspace(100, 200, 150),
    ])


def make_clean_downtrend():
    return np.concatenate([
        100 + np.random.RandomState(2).randn(60) * 0.3,
        np.linspace(100, 40, 150),
    ])


def test_ranging_fades_both_sides_when_short_allowed():
    out = regime_adaptive_core(
        make_ohlcv(make_range_with_swings()), allow_short=True, **PIN)
    assert (out["position"] == 1).any()
    assert (out["position"] == -1).any()


def make_bear_with_dips(n_legs=8, seed=7):
    rng = np.random.RandomState(seed)
    seg = []
    level = 200.0
    for _ in range(n_legs):
        seg += list(level + np.cumsum(rng.randn(20) * 0.2) - np.linspace(0, 4, 20))
        level -= 4
        seg += [level - 4, level - 8, level - 12, level - 8, level - 3]
        level -= 2
    return np.array(seg, dtype=float)


