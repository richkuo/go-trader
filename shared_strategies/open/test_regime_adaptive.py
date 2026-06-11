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


def test_short_inputs_return_flat_never_raise():
    """Frames too short to prime ADX (n <= adx_period) must return a valid
    all-flat frame, never IndexError — a newly-listed symbol with few candles
    is a no-signal frame, not a script failure."""
    # Default period=20 caps adx_period at 14: cover n below, at, and just
    # past the boundary, plus the still-all-zero ADX warmup band.
    for n in (1, 5, 13, 14, 15, 28):
        out = regime_adaptive_core(make_ohlcv([100.0 + 0.1 * i for i in range(n)]))
        assert len(out) == n
        assert (out["signal"] == 0).all()
        assert (out["position"] == 0).all()
    # period < 14 ⇒ adx_period == period (cap inactive): boundary at n == period.
    for n in (8, 9, 16):
        out = regime_adaptive_core(
            make_ohlcv([100.0 + 0.1 * i for i in range(n)]), period=8)
        assert len(out) == n
        assert (out["signal"] == 0).all()


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


def make_bear_with_dips(n_legs=8, seed=7):
    """Persistent downtrend with periodic sharp dips that snap back enough to
    fire the z-recovery long-fade trigger — the #967 OOS bleed pattern. The
    slow drift is decisively negative, so the veto must block these fades."""
    rng = np.random.RandomState(seed)
    seg = []
    level = 200.0
    for _ in range(n_legs):
        seg += list(level + np.cumsum(rng.randn(20) * 0.2) - np.linspace(0, 4, 20))
        level -= 4
        seg += [level - 4, level - 8, level - 12, level - 8, level - 3]
        level -= 2
    return np.array(seg, dtype=float)


def test_slow_trend_veto_blocks_bear_market_fades():
    """In a persistent bear, long fades fire with the veto disabled and are
    blocked once the slow drift opposes them (the #967 follow-up)."""
    df = make_ohlcv(make_bear_with_dips(), noise=0.4)
    base = regime_adaptive_core(df, slow_trend_lookback=0, **PIN)
    vetoed = regime_adaptive_core(
        df, slow_trend_lookback=60, slow_veto_threshold=0.05, **PIN)
    base_fades = int((base["signal"] == 1).sum())
    vetoed_fades = int((vetoed["signal"] == 1).sum())
    assert base_fades > 0
    assert vetoed_fades < base_fades
    # Any surviving long FADE (mr-family label at entry; clean-trend breakouts
    # are deliberately not vetoed) must not have fired against a vetoing drift.
    mr_labels = {"ranging_quiet", "ranging_volatile",
                 "trending_up_choppy", "trending_down_choppy"}
    surviving = np.where(vetoed["signal"].values == 1)[0]
    for i in surviving:
        if vetoed["ra_label"].iloc[i] in mr_labels:
            assert not (vetoed["ra_slow_eff"].iloc[i] <= -0.05)


def test_slow_trend_veto_inverse_blocks_short_fades_in_bull():
    """Inverse scenario: in a persistent bull with spikes, short fades are
    blocked by a positive slow drift when shorts are allowed."""
    closes = make_bear_with_dips()[::-1].copy()
    df = make_ohlcv(closes, noise=0.4)
    base = regime_adaptive_core(df, slow_trend_lookback=0, allow_short=True, **PIN)
    vetoed = regime_adaptive_core(
        df, slow_trend_lookback=60, slow_veto_threshold=0.05, allow_short=True, **PIN)
    mr_labels = {"ranging_quiet", "ranging_volatile",
                 "trending_up_choppy", "trending_down_choppy"}
    surviving = np.where(vetoed["signal"].values == -1)[0]
    for i in surviving:
        # Short FADE entries only — long exits and clean-trend breakout
        # shorts (not vetoed by design) are excluded.
        if vetoed["position"].iloc[i] == -1 and vetoed["ra_label"].iloc[i] in mr_labels:
            assert not (vetoed["ra_slow_eff"].iloc[i] >= 0.05)
    assert int((vetoed["position"] == -1).sum()) <= int((base["position"] == -1).sum())


def test_slow_trend_veto_inactive_in_flat_range():
    """In a drift-free range the veto must never fire — fade behavior is
    byte-identical with the veto enabled vs disabled (boundary: a fade buys
    below the mean by construction, so a price-vs-mean gate would kill all
    fades; the drift gate must not)."""
    df = make_ohlcv(make_range_with_swings())
    off = regime_adaptive_core(df, slow_trend_lookback=0, **PIN)
    on = regime_adaptive_core(df, slow_trend_lookback=100, slow_veto_threshold=0.05, **PIN)
    assert (off["signal"].values == on["signal"].values).all()
    assert (off["position"].values == on["position"].values).all()


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
