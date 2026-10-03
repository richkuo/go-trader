
import numpy as np
import pytest

from shared_strategies.open.conftest import load_module, make_ohlcv

_MEAN_REVERSION = load_module("_mean_reversion_pro_test", __file__.replace("test_mean_reversion_pro.py", "mean_reversion_pro.py"))
mean_reversion_pro_core = _MEAN_REVERSION.mean_reversion_pro_core


def make_choppy_with_extremes(base=100.0, cycles=14, seed=5):
    rng = np.random.RandomState(seed)
    seg = []
    for k in range(cycles):
        seg += list(base + rng.randn(12) * 0.4)
        if k % 2 == 0:
            seg += [base - 3, base - 6, base - 9, base - 11, base - 7, base - 2]
        else:
            seg += [base + 3, base + 6, base + 9, base + 11, base + 7, base + 2]
    return np.array(seg, dtype=float)


_TREND_CLOSES = np.linspace(100, 300, 400)

_EXTRA = dict(touch_entry=1, turn_entry=1)

_BLOCKED_CASES = [
    ("strong_trend", "trend", dict()),
    ("adx_max_zero", "chop", dict(adx_max=0.0)),
    ("rsi_gate", "chop", dict(rsi_oversold=-1.0, rsi_overbought=101.0)),
    ("strong_trend_extra", "trend", _EXTRA),
    ("adx_max_zero_extra", "chop", dict(adx_max=0.0, **_EXTRA)),
    ("rsi_gate_extra", "chop",
     dict(rsi_oversold=-1.0, rsi_overbought=101.0, **_EXTRA)),
]


def _frame(kind):
    if kind == "trend":
        return make_ohlcv(_TREND_CLOSES, noise=0.2)
    return make_ohlcv(make_choppy_with_extremes())


@pytest.mark.parametrize("kwargs", [
    dict(entry_std=1.5),
    dict(entry_std=1.5, touch_entry=1, turn_entry=1),
], ids=["base", "extra_triggers"])
def test_oscillating_range_fires_both_sides(kwargs):
    out = mean_reversion_pro_core(make_ohlcv(make_choppy_with_extremes()), **kwargs)
    assert (out["signal"] == 1).any(), "expected at least one long reversion"
    assert (out["signal"] == -1).any(), "expected at least one short reversion"


def test_extra_triggers_prefix_stable():
    df = make_ohlcv(make_choppy_with_extremes())
    kwargs = dict(entry_std=1.5, touch_entry=1, turn_entry=1)
    full = mean_reversion_pro_core(df, **kwargs)
    signal_bars = list(np.where(full["signal"].values != 0)[0])
    assert len(signal_bars) >= 1
    for k in signal_bars:
        partial = mean_reversion_pro_core(df.iloc[: k + 1], **kwargs)
        assert partial["signal"].iloc[k] == full["signal"].iloc[k], (
            f"signal at bar {k} flipped under truncation: "
            f"full={full['signal'].iloc[k]} truncated={partial['signal'].iloc[k]}"
        )
