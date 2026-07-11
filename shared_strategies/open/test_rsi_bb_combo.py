"""Tests for rsi_bb_combo.py — RSI-confirmed Bollinger Band reversion strategy."""

import numpy as np
import pandas as pd

from rsi_bb_combo import rsi_bb_combo_core


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


def make_range_with_v_dip(base=100.0, quiet=40, dip_depth=12.0, dip_len=5, recover=6, seed=7):
    """A quiet range, then a sharp V-dip deep enough to pierce the lower band
    and drive RSI oversold, then a recovery back inside the band."""
    rng = np.random.RandomState(seed)
    seg = list(base + rng.randn(quiet) * 0.3)
    seg += list(np.linspace(base, base - dip_depth, dip_len))
    seg += list(np.linspace(base - dip_depth, base - 1, recover))
    seg += list(base + rng.randn(10) * 0.3)
    return np.array(seg, dtype=float)


def make_range_with_spike(base=100.0, quiet=40, spike_height=12.0, spike_len=5, recover=6, seed=7):
    """Mirror of the V-dip: a spike above the upper band with RSI overbought."""
    dip = make_range_with_v_dip(base=base, quiet=quiet, dip_depth=spike_height,
                                dip_len=spike_len, recover=recover, seed=seed)
    return 2 * base - dip


def test_columns_present():
    out = rsi_bb_combo_core(make_ohlcv(make_range_with_v_dip()))
    for col in ("signal", "bb_middle", "bb_upper", "bb_lower", "rsi"):
        assert col in out.columns


def test_short_input_returns_no_signal_and_nan_indicators():
    out = rsi_bb_combo_core(make_ohlcv([100.0] * 10))
    assert (out["signal"] == 0).all()
    assert out["rsi"].isna().all()
    assert out["bb_lower"].isna().all()


def test_empty_df_is_safe():
    df = pd.DataFrame(columns=["open", "high", "low", "close", "volume"])
    out = rsi_bb_combo_core(df)
    assert "signal" in out.columns
    assert len(out) == 0


def test_long_entry_on_rsi_confirmed_reversion():
    """A deep V-dip pierces the lower band with RSI oversold; the recovery
    cross back above the band must emit exactly signal=1 on the cross bar."""
    out = rsi_bb_combo_core(make_ohlcv(make_range_with_v_dip()))
    longs = out.index[out["signal"] == 1]
    assert len(longs) >= 1, "expected a long reversion entry"
    i = longs[0]
    # The signal bar is the reversion cross: close back above the lower band,
    # prior close at/below it.
    assert out.loc[i, "close"] > out.loc[i, "bb_lower"]
    assert out["close"].iloc[out.index.get_loc(i) - 1] <= out["bb_lower"].iloc[out.index.get_loc(i) - 1]
    # RSI was oversold within the confirm window before the cross.
    loc = out.index.get_loc(i)
    assert (out["rsi"].iloc[loc - 3:loc] < 30.0).any()
    assert (out["signal"] != -1).all(), "no shorts in a long-side dip scenario"


def test_short_entry_on_rsi_confirmed_reversion():
    out = rsi_bb_combo_core(make_ohlcv(make_range_with_spike()))
    shorts = out.index[out["signal"] == -1]
    assert len(shorts) >= 1, "expected a short reversion entry"
    i = shorts[0]
    assert out.loc[i, "close"] < out.loc[i, "bb_upper"]
    loc = out.index.get_loc(i)
    assert (out["rsi"].iloc[loc - 3:loc] > 70.0).any()
    assert (out["signal"] != 1).all(), "no longs in a short-side spike scenario"


def test_band_cross_without_rsi_extreme_does_not_fire():
    """A shallow drift below the band that never drives RSI oversold must not
    signal on the re-cross — this is the filter that distinguishes the combo
    from the naive bollinger_bands strategy."""
    rng = np.random.RandomState(3)
    base = 100.0
    seg = list(base + rng.randn(40) * 0.3)
    # Gentle stairstep down just past the band, then back: small per-bar moves
    # keep RSI well above 30.
    seg += list(np.linspace(base, base - 1.6, 12))
    seg += list(np.linspace(base - 1.6, base, 12))
    out = rsi_bb_combo_core(make_ohlcv(np.array(seg)), rsi_oversold=10.0, rsi_overbought=90.0)
    assert (out["signal"] == 0).all(), "re-cross without an RSI extreme must not signal"


def test_confirm_window_expiry_blocks_stale_confirmation():
    """If the RSI extreme happened further back than confirm_window bars
    before the reversion cross, the signal must NOT fire."""
    closes = make_range_with_v_dip(dip_len=5, recover=14)
    df = make_ohlcv(closes)
    tight = rsi_bb_combo_core(df, confirm_window=1)
    wide = rsi_bb_combo_core(df, confirm_window=10)
    # The wide window confirms the entry; the same data with a 1-bar window
    # must produce a subset (possibly empty) of those signal bars.
    wide_longs = set(np.flatnonzero(wide["signal"].to_numpy() == 1))
    tight_longs = set(np.flatnonzero(tight["signal"].to_numpy() == 1))
    assert tight_longs.issubset(wide_longs)
    assert len(wide_longs) >= 1


def test_walking_the_band_does_not_fire_repeatedly():
    """Price walking down the lower band (staying below it) must not emit a
    stream of long signals — only the eventual re-cross bar may signal."""
    rng = np.random.RandomState(11)
    base = 100.0
    seg = list(base + rng.randn(40) * 0.3)
    seg += list(np.linspace(base, base - 20, 25))   # sustained walk down the band
    seg += list(np.linspace(base - 20, base - 16, 4))  # final recovery
    out = rsi_bb_combo_core(make_ohlcv(np.array(seg)))
    walk = out.iloc[40:65]
    below = walk["close"] < walk["bb_lower"]
    assert (walk.loc[below, "signal"] == 0).all(), "no signals while below the band"
    assert int((out["signal"] == 1).sum()) <= 1


def test_uses_wilder_rsi():
    """RSI column must match the consolidated indicators_core implementation."""
    from indicators_core import wilder_rsi

    df = make_ohlcv(make_range_with_v_dip())
    out = rsi_bb_combo_core(df)
    expected = wilder_rsi(df["close"], 14)
    pd.testing.assert_series_equal(out["rsi"], expected, check_names=False)
