
import numpy as np
import pandas as pd

from shared_strategies.open.conftest import load_module, make_ohlcv

_REGIME_ADAPTIVE_HTF = load_module("_regime_adaptive_htf_test", __file__.replace("test_regime_adaptive_htf.py", "regime_adaptive_htf.py"))
_confirm_labels = _REGIME_ADAPTIVE_HTF._confirm_labels
_RANGING_QUIET = _REGIME_ADAPTIVE_HTF._RANGING_QUIET
_RANGING_VOLATILE = _REGIME_ADAPTIVE_HTF._RANGING_VOLATILE
_TREND_UP_CLEAN = _REGIME_ADAPTIVE_HTF._TREND_UP_CLEAN
_TREND_DOWN_CLEAN = _REGIME_ADAPTIVE_HTF._TREND_DOWN_CLEAN
_WARMUP = _REGIME_ADAPTIVE_HTF._WARMUP
regime_adaptive_htf_core = _REGIME_ADAPTIVE_HTF.regime_adaptive_htf_core

PIN = dict(htf_factor=6, period=14, adx_threshold=20.0,
           return_eff_threshold=0.05, range_eff_threshold=0.03,
           efficiency_threshold=0.5, confirm_buckets=2,
           mr_lookback=20, mr_entry_z=2.0, mr_exit_z=0.0)


def make_htf_range(base=100.0, cycles=10, seed=5):
    rng = np.random.RandomState(seed)
    seg = []
    for _ in range(cycles):
        seg += list(base + rng.randn(30) * 0.4)
        seg += [base - 3, base - 6, base - 9, base - 11, base - 7, base - 2]
    return np.array(seg, dtype=float)


def make_htf_clean_uptrend():
    return np.concatenate([
        100 + np.random.RandomState(1).randn(150) * 0.3,
        np.linspace(100, 260, 360),
    ])


PINNED_RANGE_ENTRIES = [106, 133, 142]
PINNED_RANGE_EXITS = [108, 134, 144]


def test_allow_short_fades_spikes():
    closes = 200.0 - (make_htf_range() - 100.0)
    df = make_ohlcv(closes)
    base = regime_adaptive_htf_core(df, **PIN)
    shorted = regime_adaptive_htf_core(df, allow_short=True, **PIN)
    assert not (base["position"] == -1).any()
    assert (shorted["position"] == -1).any()


def make_bear_with_dips(n_legs=10, seed=7):
    rng = np.random.RandomState(seed)
    seg = []
    level = 300.0
    for _ in range(n_legs):
        seg += list(level + np.cumsum(rng.randn(30) * 0.2) - np.linspace(0, 5, 30))
        level -= 5
        seg += [level - 4, level - 8, level - 12, level - 8, level - 3]
        level -= 2
    return np.array(seg, dtype=float)


