
import numpy as np
import pandas as pd

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


def test_columns_present():
    out = mean_reversion_pro_core(make_ohlcv(make_choppy_with_extremes()))
    for col in ("signal", "z_score", "adx", "rsi"):
        assert col in out.columns


def test_warmup_returns_no_signal():
    out = mean_reversion_pro_core(make_ohlcv([100.0] * 30))
    assert (out["signal"] == 0).all()


def test_empty_df_is_safe():
    df = pd.DataFrame(columns=["open", "high", "low", "close", "volume"])
    out = mean_reversion_pro_core(df)
    assert "signal" in out.columns
    assert len(out) == 0


def test_oscillating_range_fires_both_sides():
    out = mean_reversion_pro_core(make_ohlcv(make_choppy_with_extremes()), entry_std=1.5)
    assert (out["signal"] == 1).any(), "expected at least one long reversion"
    assert (out["signal"] == -1).any(), "expected at least one short reversion"


def test_strong_trend_blocks_entries():
    closes = np.linspace(100, 300, 400)
    out = mean_reversion_pro_core(make_ohlcv(closes, noise=0.2))
    assert (out["signal"] == 0).all()


def test_adx_max_is_respected():
    out = mean_reversion_pro_core(make_ohlcv(make_choppy_with_extremes()), adx_max=0.0)
    assert (out["signal"] == 0).all()


def test_rsi_confirmation_required():
    out = mean_reversion_pro_core(
        make_ohlcv(make_choppy_with_extremes()),
        rsi_oversold=-1.0,
        rsi_overbought=101.0,
    )
    assert (out["signal"] == 0).all()


def test_extra_triggers_default_off_bit_identical():
    df = make_ohlcv(make_choppy_with_extremes())
    base = mean_reversion_pro_core(df, entry_std=1.5)
    off = mean_reversion_pro_core(df, entry_std=1.5, touch_entry=0, turn_entry=0)
    assert (base["signal"] == off["signal"]).all()
    for col in ("z_score", "adx", "rsi"):
        assert base[col].equals(off[col])


def test_touch_entry_adds_setups():
    df = make_ohlcv(make_choppy_with_extremes())
    kwargs = dict(entry_std=1.5, rsi_oversold=35.0, rsi_overbought=65.0)
    base = mean_reversion_pro_core(df, **kwargs)
    touch = mean_reversion_pro_core(df, touch_entry=1, **kwargs)
    base_bars = set(np.where(base["signal"].values != 0)[0])
    touch_bars = set(np.where(touch["signal"].values != 0)[0])
    assert base_bars <= touch_bars, "touch_entry removed a base signal"
    assert len(touch_bars) > len(base_bars), "touch_entry added no setups"


def test_turn_entry_adds_setups():
    df = make_ohlcv(make_choppy_with_extremes())
    base = mean_reversion_pro_core(df, entry_std=1.5)
    turn = mean_reversion_pro_core(df, entry_std=1.5, turn_entry=1)
    base_bars = set(np.where(base["signal"].values != 0)[0])
    turn_bars = set(np.where(turn["signal"].values != 0)[0])
    assert base_bars <= turn_bars, "turn_entry removed a base signal"
    assert len(turn_bars) > len(base_bars), "turn_entry added no setups"


def test_extra_triggers_preserve_base_signal_values():
    df = make_ohlcv(make_choppy_with_extremes())
    base = mean_reversion_pro_core(df, entry_std=1.5)
    both = mean_reversion_pro_core(df, entry_std=1.5, touch_entry=1, turn_entry=1)
    fired = base["signal"].values != 0
    assert (base["signal"].values[fired] == both["signal"].values[fired]).all()


def test_extra_triggers_still_blocked_by_strong_trend():
    closes = np.linspace(100, 300, 400)
    out = mean_reversion_pro_core(
        make_ohlcv(closes, noise=0.2), touch_entry=1, turn_entry=1
    )
    assert (out["signal"] == 0).all()


def test_extra_triggers_respect_adx_max_zero():
    out = mean_reversion_pro_core(
        make_ohlcv(make_choppy_with_extremes()),
        adx_max=0.0, touch_entry=1, turn_entry=1,
    )
    assert (out["signal"] == 0).all()


def test_extra_triggers_require_rsi_evidence():
    out = mean_reversion_pro_core(
        make_ohlcv(make_choppy_with_extremes()),
        rsi_oversold=-1.0, rsi_overbought=101.0,
        touch_entry=1, turn_entry=1,
    )
    assert (out["signal"] == 0).all()


def test_extra_triggers_fire_both_sides():
    out = mean_reversion_pro_core(
        make_ohlcv(make_choppy_with_extremes()),
        entry_std=1.5, touch_entry=1, turn_entry=1,
    )
    assert (out["signal"] == 1).any()
    assert (out["signal"] == -1).any()


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
