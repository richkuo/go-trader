"""Tests for regime_adaptive.py — composite-regime breakout/mean-reversion switch (#958)."""

import numpy as np
import pandas as pd

from regime_adaptive import regime_adaptive_core

# Exact-index pins below were computed with these explicit parameters; the
# registry/function defaults are tuned independently (walk-forward stability)
# and may drift without invalidating the pinned behavior.
PIN = dict(period=20, adx_threshold=25.0, return_eff_threshold=0.05,
           range_eff_threshold=0.03, efficiency_threshold=0.5,
           breakout_lookback=20, mr_lookback=20, mr_entry_z=1.5, mr_exit_z=0.0)


def make_ohlcv(closes, noise=0.5, volume=100.0):
    closes = np.asarray(closes, dtype=float)
    n = len(closes)
    return pd.DataFrame({
        "open": closes - noise * 0.3,
        "high": closes + noise,
        "low": closes - noise,
        "close": closes,
        "volume": np.full(n, volume),
    }, index=pd.date_range("2026-01-01", periods=n, freq="1h"))


def make_range_with_swings(base=100.0, cycles=14, seed=5):
    """Quiet range punctuated by sharp dips/spikes — the z-recovery cross at the
    end of each swing is the fade trigger; the swing itself reads as a
    trending_*_choppy composite label (big move, low efficiency)."""
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
    """Quiet base then a relentless ramp → trending_up_clean breakout entry."""
    return np.concatenate([
        100 + np.random.RandomState(1).randn(60) * 0.3,
        np.linspace(100, 200, 150),
    ])


def make_clean_downtrend():
    return np.concatenate([
        100 + np.random.RandomState(2).randn(60) * 0.3,
        np.linspace(100, 40, 150),
    ])


def test_columns_present():
    out = regime_adaptive_core(make_ohlcv(make_range_with_swings()), **PIN)
    for col in ("signal", "position", "ra_label", "ra_return_eff",
                "ra_range_eff", "ra_efficiency", "ra_adx", "ra_z"):
        assert col in out.columns


def test_empty_df_is_safe():
    df = pd.DataFrame(columns=["open", "high", "low", "close", "volume"])
    out = regime_adaptive_core(df)
    assert "signal" in out.columns
    assert len(out) == 0


def test_warmup_returns_no_signal():
    out = regime_adaptive_core(make_ohlcv([100.0] * 30))
    assert (out["signal"] == 0).all()
    assert (out["position"] == 0).all()


def test_clean_uptrend_breakout_entry():
    """The ramp must classify trending_up_clean and enter long via the
    prior-extreme breakout — exactly one entry, held through the trend."""
    out = regime_adaptive_core(make_ohlcv(make_clean_uptrend(), noise=0.4), **PIN)
    entries = np.where(out["signal"].values == 1)[0]
    assert entries.tolist() == [68]
    assert out["ra_label"].iloc[68] == "trending_up_clean"
    assert (out["position"].iloc[69:] == 1).all()
    assert not (out["position"] == -1).any()


def test_trend_exit_when_regime_leaves_trend_family():
    """Appending a stall after the ramp must flatten the trend position."""
    closes = np.concatenate([make_clean_uptrend(), np.linspace(200, 195, 40)])
    out = regime_adaptive_core(make_ohlcv(closes, noise=0.4), **PIN)
    pos = out["position"].values
    exit_bars = np.where(np.diff(pos) == -1)[0]
    assert exit_bars.tolist() == [224]
    assert pos[-1] == 0


def test_downtrend_long_only_never_short():
    out = regime_adaptive_core(make_ohlcv(make_clean_downtrend(), noise=0.4), **PIN)
    assert not (out["position"] == -1).any()


def test_downtrend_allow_short_enters_short():
    out = regime_adaptive_core(
        make_ohlcv(make_clean_downtrend(), noise=0.4), allow_short=True, **PIN)
    shorts = np.where(out["signal"].values == -1)[0]
    assert shorts[0] == 32
    assert int((out["position"] == -1).sum()) == 144


def test_ranging_fades_long_only_exact_entries():
    """Long-only fade entries fire on every dip's z-recovery cross; the entry
    bar's composite label is the down-swing's choppy-trend label (the reason
    mr-mode includes the choppy labels — see module docstring)."""
    out = regime_adaptive_core(make_ohlcv(make_range_with_swings()), **PIN)
    entries = np.where(out["signal"].values == 1)[0]
    exits = np.where(out["signal"].values == -1)[0]
    assert entries.tolist() == [52, 88, 124, 160, 196, 232]
    assert exits.tolist() == [54, 90, 126, 162, 198, 234]
    assert out["ra_label"].iloc[52] == "trending_down_choppy"


def test_ranging_fades_both_sides_when_short_allowed():
    out = regime_adaptive_core(
        make_ohlcv(make_range_with_swings()), allow_short=True, **PIN)
    assert (out["position"] == 1).any()
    assert (out["position"] == -1).any()


def test_impossible_efficiency_blocks_breakouts():
    """efficiency_threshold > 1 makes clean-trend unreachable → the ramp
    produces zero positions (choppy-trend bars only allow fades, and a
    monotone ramp never triggers a z-recovery)."""
    out = regime_adaptive_core(
        make_ohlcv(make_clean_uptrend(), noise=0.4), **{**PIN, "efficiency_threshold": 1.01})
    assert (out["position"] == 0).all()


def test_impossible_entry_z_blocks_fades():
    out = regime_adaptive_core(
        make_ohlcv(make_range_with_swings()), **{**PIN, "mr_entry_z": 50.0})
    assert (out["position"] == 0).all()


def test_no_lookahead_future_perturbation():
    """Signals up to bar N must be byte-identical when every row after N is
    rescaled — all inputs are trailing windows."""
    df = make_ohlcv(make_range_with_swings())
    full = regime_adaptive_core(df, allow_short=True, **PIN)
    cut = 150
    pert = df.copy()
    pert.iloc[cut:, :] = pert.iloc[cut:, :] * 1.7
    out = regime_adaptive_core(pert, allow_short=True, **PIN)
    assert (full["signal"].iloc[:cut].values == out["signal"].iloc[:cut].values).all()
    assert (full["position"].iloc[:cut].values == out["position"].iloc[:cut].values).all()
