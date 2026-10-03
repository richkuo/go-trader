
import numpy as np
import pandas as pd

from shared_strategies.open.conftest import load_module, make_ohlcv

_RSI_BB = load_module("_rsi_bb_combo_test", __file__.replace("test_rsi_bb_combo.py", "rsi_bb_combo.py"))
rsi_bb_combo_core = _RSI_BB.rsi_bb_combo_core


def make_range_with_v_dip(base=100.0, quiet=40, dip_depth=12.0, dip_len=5, recover=6, seed=7):
    rng = np.random.RandomState(seed)
    seg = list(base + rng.randn(quiet) * 0.3)
    seg += list(np.linspace(base, base - dip_depth, dip_len))
    seg += list(np.linspace(base - dip_depth, base - 1, recover))
    seg += list(base + rng.randn(10) * 0.3)
    return np.array(seg, dtype=float)


def make_range_with_spike(base=100.0, quiet=40, spike_height=12.0, spike_len=5, recover=6, seed=7):
    dip = make_range_with_v_dip(base=base, quiet=quiet, dip_depth=spike_height,
                                dip_len=spike_len, recover=recover, seed=seed)
    return 2 * base - dip


def test_short_entry_on_rsi_confirmed_reversion():
    out = rsi_bb_combo_core(make_ohlcv(make_range_with_spike()))
    shorts = out.index[out["signal"] == -1]
    assert len(shorts) >= 1, "expected a short reversion entry"
    i = shorts[0]
    assert out.loc[i, "close"] < out.loc[i, "bb_upper"]
    loc = out.index.get_loc(i)
    assert (out["rsi"].iloc[loc - 3:loc] > 70.0).any()
    assert (out["signal"] != 1).all(), "no longs in a short-side spike scenario"


