"""Tests for mean_reversion_pro.py — trend-filtered mean-reversion strategy."""

import numpy as np
import pandas as pd

from mean_reversion_pro import mean_reversion_pro_core


def make_ohlcv(closes, noise=0.5, volume=100.0):
    closes = np.asarray(closes, dtype=float)
    n = len(closes)
    return pd.DataFrame({
        "open": closes - noise * 0.3,
        "high": closes + noise,
        "low": closes - noise,
        "close": closes,
        "volume": np.full(n, volume),
    })


def make_choppy_with_extremes(base=100.0, cycles=14, seed=5):
    """Low-ADX chop with sharp V-dips and spikes that drive RSI to true
    oversold/overbought extremes — a realistic ranging market (a smooth sine
    reads as high-ADX alternating trends, which the no-trend gate rejects)."""
    rng = np.random.RandomState(seed)
    seg = []
    for k in range(cycles):
        seg += list(base + rng.randn(12) * 0.4)              # quiet range
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
    """A clean low-ADX oscillation should produce both long and short
    reversion entries."""
    out = mean_reversion_pro_core(make_ohlcv(make_choppy_with_extremes()), entry_std=1.5)
    assert (out["signal"] == 1).any(), "expected at least one long reversion"
    assert (out["signal"] == -1).any(), "expected at least one short reversion"


def test_strong_trend_blocks_entries():
    """A strong, steady trend (high ADX) must be filtered out — the whole
    point of the no-trend gate (no falling-knife fades)."""
    closes = np.linspace(100, 300, 400)  # relentless uptrend → high ADX
    out = mean_reversion_pro_core(make_ohlcv(closes, noise=0.2))
    assert (out["signal"] == 0).all()


def test_adx_max_is_respected():
    """With adx_max = 0, the no-trend gate can never open → no entries."""
    out = mean_reversion_pro_core(make_ohlcv(make_choppy_with_extremes()), adx_max=0.0)
    assert (out["signal"] == 0).all()


def test_rsi_confirmation_required():
    """With impossible RSI thresholds (oversold below 0, overbought above 100),
    the oscillator confirmation can never be satisfied → no entries."""
    out = mean_reversion_pro_core(
        make_ohlcv(make_choppy_with_extremes()),
        rsi_oversold=-1.0,
        rsi_overbought=101.0,
    )
    assert (out["signal"] == 0).all()


# ─── #981 additional entry triggers (default-off) ───────────────────────────


def test_extra_triggers_default_off_bit_identical():
    """touch_entry=0, turn_entry=0 must be bit-identical to the pre-#981
    strategy — the registry default changes nothing."""
    df = make_ohlcv(make_choppy_with_extremes())
    base = mean_reversion_pro_core(df, entry_std=1.5)
    off = mean_reversion_pro_core(df, entry_std=1.5, touch_entry=0, turn_entry=0)
    assert (base["signal"] == off["signal"]).all()
    for col in ("z_score", "adx", "rsi"):
        assert base[col].equals(off[col])


def test_touch_entry_adds_setups():
    """touch_entry=1 must add signal bars the reversion-cross trigger misses
    (that is the frequency mechanism) without removing any base signal.

    The fixture's V-dips put pierce-bar RSI at ~31-34, so thresholds of
    35/65 give the touch trigger genuine current-bar RSI evidence there —
    the same thresholds on both runs keep the comparison apples-to-apples."""
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
    """On every bar where the base strategy fires, the triggers-on run must
    emit the same direction (the extra triggers are additive OR, never a
    rewrite of the base decision)."""
    df = make_ohlcv(make_choppy_with_extremes())
    base = mean_reversion_pro_core(df, entry_std=1.5)
    both = mean_reversion_pro_core(df, entry_std=1.5, touch_entry=1, turn_entry=1)
    fired = base["signal"].values != 0
    assert (base["signal"].values[fired] == both["signal"].values[fired]).all()


def test_extra_triggers_still_blocked_by_strong_trend():
    """The regime filter is the strategy's edge (#981's hard requirement):
    a relentless high-ADX trend must produce zero entries even with both
    extra triggers enabled."""
    closes = np.linspace(100, 300, 400)
    out = mean_reversion_pro_core(
        make_ohlcv(closes, noise=0.2), touch_entry=1, turn_entry=1
    )
    assert (out["signal"] == 0).all()


def test_extra_triggers_respect_adx_max_zero():
    """adx_max=0 closes the no-trend gate for the extra triggers too."""
    out = mean_reversion_pro_core(
        make_ohlcv(make_choppy_with_extremes()),
        adx_max=0.0, touch_entry=1, turn_entry=1,
    )
    assert (out["signal"] == 0).all()


def test_extra_triggers_require_rsi_evidence():
    """Impossible RSI thresholds must silence the extra triggers as well —
    they add setups behind the same oscillator evidence, not around it."""
    out = mean_reversion_pro_core(
        make_ohlcv(make_choppy_with_extremes()),
        rsi_oversold=-1.0, rsi_overbought=101.0,
        touch_entry=1, turn_entry=1,
    )
    assert (out["signal"] == 0).all()


def test_extra_triggers_fire_both_sides():
    """The mirrored short-side triggers must fire on the fixture's spikes,
    not just the long side on its dips."""
    out = mean_reversion_pro_core(
        make_ohlcv(make_choppy_with_extremes()),
        entry_std=1.5, touch_entry=1, turn_entry=1,
    )
    assert (out["signal"] == 1).any()
    assert (out["signal"] == -1).any()


def test_extra_triggers_prefix_stable():
    """No look-ahead: for every bar where the triggers-on run emits a signal,
    running on the prefix df[:k+1] must emit the same signal at bar k."""
    df = make_ohlcv(make_choppy_with_extremes())
    kwargs = dict(entry_std=1.5, touch_entry=1, turn_entry=1)
    full = mean_reversion_pro_core(df, **kwargs)
    signal_bars = list(np.where(full["signal"].values != 0)[0])
    assert len(signal_bars) >= 1  # fixture sanity
    for k in signal_bars:
        partial = mean_reversion_pro_core(df.iloc[: k + 1], **kwargs)
        assert partial["signal"].iloc[k] == full["signal"].iloc[k], (
            f"signal at bar {k} flipped under truncation: "
            f"full={full['signal'].iloc[k]} truncated={partial['signal'].iloc[k]}"
        )
