"""Tests for regime_adaptive_htf.py — HTF-classified composite-regime fades (#973)."""

import numpy as np
import pandas as pd

from regime_adaptive_htf import (
    _confirm_labels,
    _RANGING_QUIET,
    _RANGING_VOLATILE,
    _TREND_UP_CLEAN,
    _TREND_DOWN_CLEAN,
    _WARMUP,
    regime_adaptive_htf_core,
)

# Exact-index pins below were computed with these explicit parameters; the
# registry/function defaults are tuned independently and may drift without
# invalidating the pinned behavior.
PIN = dict(htf_factor=6, period=14, adx_threshold=20.0,
           return_eff_threshold=0.05, range_eff_threshold=0.03,
           efficiency_threshold=0.5, confirm_buckets=2,
           mr_lookback=20, mr_entry_z=2.0, mr_exit_z=0.0)


def make_ohlcv(closes, noise=0.5, volume=100.0, start="2026-01-01"):
    closes = np.asarray(closes, dtype=float)
    n = len(closes)
    return pd.DataFrame({
        "open": closes - noise * 0.3,
        "high": closes + noise,
        "low": closes - noise,
        "close": closes,
        "volume": np.full(n, volume),
    }, index=pd.date_range(start, periods=n, freq="1h"))


def make_htf_range(base=100.0, cycles=10, seed=5):
    """Long quiet range punctuated by sharp dips that recover — at the 6x HTF
    scale the labels stay in the ranging family (no decisive net move across
    a 14-bucket window) while the dips push the native z below -2 and the
    snap-back fires the recovery cross."""
    rng = np.random.RandomState(seed)
    seg = []
    for _ in range(cycles):
        seg += list(base + rng.randn(30) * 0.4)
        seg += [base - 3, base - 6, base - 9, base - 11, base - 7, base - 2]
    return np.array(seg, dtype=float)


def make_htf_clean_uptrend():
    """Quiet base then a relentless ramp — at the HTF scale the ramp reads
    trending_up_clean (decisive net move, high efficiency, high ADX)."""
    return np.concatenate([
        100 + np.random.RandomState(1).randn(150) * 0.3,
        np.linspace(100, 260, 360),
    ])


# ── Shape / safety ───────────────────────────────────────────────────────────


def test_columns_present():
    out = regime_adaptive_htf_core(make_ohlcv(make_htf_range()), **PIN)
    for col in ("signal", "position", "rah_label", "rah_raw_label",
                "rah_z", "rah_slow_eff"):
        assert col in out.columns


def test_empty_df_is_safe():
    df = pd.DataFrame(columns=["open", "high", "low", "close", "volume"])
    out = regime_adaptive_htf_core(df)
    assert "signal" in out.columns
    assert len(out) == 0


def test_short_inputs_return_flat_never_raise():
    """Frames whose HTF resample yields too few buckets to prime ADX
    (len(htf) <= adx_period) must return a valid all-flat frame, never
    IndexError. Defaults htf_factor=6, period=14 → adx_period=14: the
    boundary sits at 14 buckets ≈ 84 native bars."""
    for n in (1, 5, 14, 83, 84, 85, 90):
        out = regime_adaptive_htf_core(make_ohlcv([100.0 + 0.1 * i for i in range(n)]))
        assert len(out) == n
        assert (out["signal"] == 0).all()
        assert (out["position"] == 0).all()
    # period < 14 ⇒ adx_period == period (cap inactive); htf_factor=2,
    # period=8 → boundary at 16 native bars.
    for n in (15, 16, 17, 40):
        out = regime_adaptive_htf_core(
            make_ohlcv([100.0 + 0.1 * i for i in range(n)]), htf_factor=2, period=8)
        assert len(out) == n
        assert (out["signal"] == 0).all()


def test_range_index_fallback_is_safe():
    """Non-datetime indexes take the positional bucketing fallback."""
    closes = make_htf_range()
    df = pd.DataFrame({
        "open": closes, "high": closes + 0.5, "low": closes - 0.5,
        "close": closes, "volume": np.full(len(closes), 100.0),
    })
    out = regime_adaptive_htf_core(df, **PIN)
    assert len(out) == len(df)
    assert set(out["signal"].unique()) <= {-1, 0, 1}


def test_warmup_returns_no_signal():
    out = regime_adaptive_htf_core(make_ohlcv([100.0] * 120), **PIN)
    assert (out["signal"] == 0).all()
    assert (out["position"] == 0).all()


# ── Confirmation hysteresis (unit) ───────────────────────────────────────────


def test_confirm_labels_requires_streak():
    raw = np.array([_RANGING_QUIET] * 3 + [_TREND_UP_CLEAN] + [_RANGING_QUIET] * 3)
    conf = _confirm_labels(raw, 2)
    # The single-bucket trend blip never confirms; the post-blip quiet bucket
    # restarts the streak and re-confirms one bucket later.
    assert conf.tolist() == [0, _RANGING_QUIET, _RANGING_QUIET, _RANGING_QUIET,
                             _RANGING_QUIET, _RANGING_QUIET, _RANGING_QUIET]


def test_confirm_labels_switches_after_n():
    raw = np.array([_RANGING_QUIET] * 4 + [_TREND_UP_CLEAN] * 3)
    conf = _confirm_labels(raw, 3)
    assert conf.tolist() == [0, 0, _RANGING_QUIET, _RANGING_QUIET,
                             _RANGING_QUIET, _RANGING_QUIET, _TREND_UP_CLEAN]


def test_confirm_labels_warmup_resets_streak():
    raw = np.array([_RANGING_QUIET, _WARMUP, _RANGING_QUIET, _RANGING_QUIET])
    conf = _confirm_labels(raw, 2)
    # The warmup gap breaks the streak; confirmation lands only after two
    # consecutive post-gap buckets, and warmup holds the prior confirmed label.
    assert conf.tolist() == [0, 0, 0, _RANGING_QUIET]


def test_confirm_one_is_identity():
    raw = np.array([_RANGING_QUIET, _TREND_UP_CLEAN, _TREND_DOWN_CLEAN])
    assert _confirm_labels(raw, 1).tolist() == raw.tolist()


# ── Entry / exit semantics ───────────────────────────────────────────────────

# Pinned by running make_htf_range through the core once with PIN params.
PINNED_RANGE_ENTRIES = [106, 133, 142]
PINNED_RANGE_EXITS = [108, 134, 144]


def test_ranging_fades_exact_entries():
    """Fades fire on the z-recovery cross inside confirmed HTF ranging labels
    and exit at the mean."""
    out = regime_adaptive_htf_core(make_ohlcv(make_htf_range()), **PIN)
    entries = np.where(out["signal"].values == 1)[0]
    exits = np.where(out["signal"].values == -1)[0]
    assert len(entries) > 0
    assert entries.tolist() == PINNED_RANGE_ENTRIES
    assert exits.tolist() == PINNED_RANGE_EXITS
    fade_labels = {"ranging_quiet", "ranging_volatile"}
    for i in entries:
        assert out["rah_label"].iloc[i] in fade_labels


def test_fade_only_default_never_holds_through_clean_trend():
    """trend_entry='off' (default): a clean HTF uptrend produces no entries."""
    out = regime_adaptive_htf_core(make_ohlcv(make_htf_clean_uptrend(), noise=0.4), **PIN)
    labels = set(out["rah_label"].unique())
    assert "trending_up_clean" in labels  # the frame does classify as a clean trend
    assert (out["position"] == 0).all()


def test_trend_breakout_mode_enters_clean_trend():
    out = regime_adaptive_htf_core(
        make_ohlcv(make_htf_clean_uptrend(), noise=0.4),
        trend_entry="breakout", breakout_lookback=10, **PIN)
    entries = np.where(out["signal"].values == 1)[0]
    assert len(entries) > 0
    assert out["rah_label"].iloc[entries[0]] == "trending_up_clean"
    # Held through the trend: position stays long once entered.
    assert (out["position"].iloc[entries[0] + 1:] == 1).all()
    # The ramp's slow drift agrees at the entry bar (the gate was satisfied,
    # not bypassed).
    assert out["rah_slow_eff"].iloc[entries[0]] >= 0.05


def test_trend_drift_confirm_blocks_without_agreeing_drift():
    """The gate is the fade veto's mirror: an impossible threshold blocks all
    clean-trend entries on the same frame where the default fires."""
    df = make_ohlcv(make_htf_clean_uptrend(), noise=0.4)
    base = regime_adaptive_htf_core(df, trend_entry="breakout",
                                    breakout_lookback=10, **PIN)
    gated = regime_adaptive_htf_core(df, trend_entry="breakout",
                                     breakout_lookback=10,
                                     trend_drift_confirm=99.0, **PIN)
    assert (base["signal"] == 1).any()
    assert (gated["position"] == 0).all()
    # Fades are untouched by the trend gate: a ranging frame is byte-identical
    # across trend_drift_confirm values.
    rdf = make_ohlcv(make_htf_range())
    a = regime_adaptive_htf_core(rdf, trend_drift_confirm=0.05, **PIN)
    b = regime_adaptive_htf_core(rdf, trend_drift_confirm=99.0, **PIN)
    assert (a["signal"].values == b["signal"].values).all()


def test_trend_drift_confirm_inactive_when_veto_disabled():
    """slow_trend_lookback=0 disables the drift series; the trend gate must
    pass (not block) so short frames / veto-off configs still trend-enter."""
    df = make_ohlcv(make_htf_clean_uptrend(), noise=0.4)
    out = regime_adaptive_htf_core(df, trend_entry="breakout",
                                   breakout_lookback=10,
                                   slow_trend_lookback=0,
                                   trend_drift_confirm=99.0, **PIN)
    assert (out["signal"] == 1).any()


def test_trend_transition_mode_enters_at_flip():
    """trend_entry='transition': entry fires within transition_window native
    bars of the confirmed label flipping into trending_up_clean."""
    out = regime_adaptive_htf_core(
        make_ohlcv(make_htf_clean_uptrend(), noise=0.4),
        trend_entry="transition", transition_window=6, **PIN)
    entries = np.where(out["signal"].values == 1)[0]
    assert len(entries) > 0
    first = entries[0]
    labels = out["rah_label"].values
    assert labels[first] == "trending_up_clean"
    # The confirmed label flipped into the clean trend within the window.
    flip = first
    while flip > 0 and labels[flip - 1] == "trending_up_clean":
        flip -= 1
    assert first - flip < 6


def test_fade_labels_all_mr_widens_the_gate():
    """A sloppy persistent downtrend reads trending_down_choppy at the HTF
    scale: the ranging-only default takes none of its snap-back fades while
    the #967 all_mr mapping takes them (veto disabled — the bear drift would
    otherwise block them, which is the veto test's job)."""
    df = make_ohlcv(make_bear_with_dips(), noise=0.4)
    narrow = regime_adaptive_htf_core(df, fade_labels="ranging",
                                      slow_trend_lookback=0, **PIN)
    wide = regime_adaptive_htf_core(df, fade_labels="all_mr",
                                    slow_trend_lookback=0, **PIN)
    assert int((narrow["signal"] == 1).sum()) == 0
    wide_entries = np.where(wide["signal"].values == 1)[0]
    assert len(wide_entries) > 0
    choppy = {"trending_up_choppy", "trending_down_choppy"}
    assert all(wide["rah_label"].iloc[i] in choppy for i in wide_entries)


def test_long_only_default_never_short():
    out = regime_adaptive_htf_core(
        make_ohlcv(-make_htf_range() + 300), **PIN)
    assert not (out["position"] == -1).any()


def test_allow_short_fades_spikes():
    """Mirrored range (spikes up instead of dips): shorts fade the spikes
    only when allow_short=True."""
    closes = 200.0 - (make_htf_range() - 100.0)  # dips become spikes
    df = make_ohlcv(closes)
    base = regime_adaptive_htf_core(df, **PIN)
    shorted = regime_adaptive_htf_core(df, allow_short=True, **PIN)
    assert not (base["position"] == -1).any()
    assert (shorted["position"] == -1).any()


def test_impossible_entry_z_blocks_fades():
    out = regime_adaptive_htf_core(
        make_ohlcv(make_htf_range()), **{**PIN, "mr_entry_z": 50.0})
    assert (out["position"] == 0).all()


# ── Slow-trend veto (#967 semantics carried over) ───────────────────────────


def make_bear_with_dips(n_legs=10, seed=7):
    """Persistent downtrend with sharp dips that snap back enough to fire the
    long-fade z-recovery — the slow drift is decisively negative, so the veto
    must block surviving fades whose label is in the fade family."""
    rng = np.random.RandomState(seed)
    seg = []
    level = 300.0
    for _ in range(n_legs):
        seg += list(level + np.cumsum(rng.randn(30) * 0.2) - np.linspace(0, 5, 30))
        level -= 5
        seg += [level - 4, level - 8, level - 12, level - 8, level - 3]
        level -= 2
    return np.array(seg, dtype=float)


def test_slow_trend_veto_blocks_bear_market_fades():
    df = make_ohlcv(make_bear_with_dips(), noise=0.4)
    base = regime_adaptive_htf_core(df, slow_trend_lookback=0, fade_labels="all_mr", **PIN)
    vetoed = regime_adaptive_htf_core(
        df, slow_trend_lookback=60, slow_veto_threshold=0.05, fade_labels="all_mr", **PIN)
    base_fades = int((base["signal"] == 1).sum())
    vetoed_fades = int((vetoed["signal"] == 1).sum())
    assert base_fades > 0
    assert vetoed_fades < base_fades
    surviving = np.where(vetoed["signal"].values == 1)[0]
    for i in surviving:
        assert not (vetoed["rah_slow_eff"].iloc[i] <= -0.05)


def test_slow_trend_veto_inactive_in_flat_range():
    """Drift-free range: veto on vs off must be byte-identical."""
    df = make_ohlcv(make_htf_range())
    off = regime_adaptive_htf_core(df, slow_trend_lookback=0, **PIN)
    on = regime_adaptive_htf_core(df, slow_trend_lookback=100,
                                  slow_veto_threshold=0.05, **PIN)
    assert (off["signal"].values == on["signal"].values).all()
    assert (off["position"].values == on["position"].values).all()


# ── Registration pins ────────────────────────────────────────────────────────


def test_registered_long_only_on_both_platforms():
    """Bidirectional was benchmarked below the bar (OOS -1.68 vs -0.32) and
    deliberately not registered: both platform builds must ship
    allow_short=False with no futures override. Pins the docstring/registry
    agreement — re-adding a futures allow_short variant must consciously
    update all three (registry, docstring, init.go) together."""
    # Load the OPEN registry by path — a bare `import registry` resolves to
    # whichever registry.py (open or close) hit sys.modules first, which
    # varies with test collection order (flaky under pytest-xdist).
    import importlib.util
    import os

    path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "registry.py")
    spec = importlib.util.spec_from_file_location("_open_registry_1304", path)
    open_registry = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(open_registry)
    entry = open_registry.STRATEGIES["regime_adaptive_htf"]
    assert entry["default_params"]["allow_short"] is False
    assert entry["variants"] == {}
    for platform in ("spot", "futures"):
        built = open_registry.build_registry(platform)["regime_adaptive_htf"]
        assert built["default_params"]["allow_short"] is False


# ── Look-ahead regression (the #955 bar) ─────────────────────────────────────


def test_prefix_consistency_no_lookahead():
    """Gold standard: signals over df[:k] must equal the first k signals over
    the full df, for cutoffs landing at every offset within an HTF bucket.
    Any read of the in-progress HTF bucket (or any other future leak) breaks
    this for some k."""
    df = make_ohlcv(make_htf_range())
    full = regime_adaptive_htf_core(df, **PIN)["signal"]
    n = len(df)
    for k in range(n - 13, n):  # covers all 6 intra-bucket offsets, twice
        prefix = regime_adaptive_htf_core(df.iloc[:k], **PIN)["signal"]
        pd.testing.assert_series_equal(
            prefix, full.iloc[:k], check_names=False, obj=f"prefix k={k}"
        )


def test_future_rows_do_not_change_past_signals():
    """Perturb rows after a cutoff (including bars inside the then-current
    HTF bucket) wildly; signals at and before the cutoff must be unchanged."""
    df = make_ohlcv(make_htf_range())
    n = len(df)
    cut = n - 8
    base = regime_adaptive_htf_core(df, allow_short=True, **PIN)["signal"].iloc[:cut]

    crashed = df.copy()
    crashed.loc[crashed.index[cut:], ["open", "high", "low", "close"]] = 1.0
    out_crashed = regime_adaptive_htf_core(crashed, allow_short=True, **PIN)["signal"].iloc[:cut]
    pd.testing.assert_series_equal(base, out_crashed, check_names=False)

    mooned = df.copy()
    mooned.loc[mooned.index[cut:], ["open", "high", "low", "close"]] *= 10.0
    out_mooned = regime_adaptive_htf_core(mooned, allow_short=True, **PIN)["signal"].iloc[:cut]
    pd.testing.assert_series_equal(base, out_mooned, check_names=False)


def test_label_lags_not_leads_the_bucket():
    """At a native bar mid-bucket, rah_label must reflect only buckets that
    already closed: rewriting the remainder of the current bucket cannot
    change the label at that bar."""
    df = make_ohlcv(make_htf_range())
    out = regime_adaptive_htf_core(df, **PIN)
    mid_bucket = [ts for ts in df.index[-40:] if ts.hour % 6 != 5][0]
    mutated = df.copy()
    later = mutated.index > mid_bucket
    mutated.loc[later, ["open", "high", "low", "close"]] = 1.0
    out_mut = regime_adaptive_htf_core(mutated, **PIN)
    assert out.loc[mid_bucket, "rah_label"] == out_mut.loc[mid_bucket, "rah_label"]

