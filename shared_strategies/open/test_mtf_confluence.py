"""Tests for mtf_confluence.py — HTF trend gate over LTF pullback entries (#957).

Includes the look-ahead regression class required by the #955 validation bar:
the in-frame HTF resample must never read the in-progress HTF bucket, proven
via prefix-consistency and future-row perturbation.
"""

import numpy as np
import pandas as pd

from mtf_confluence import mtf_confluence_core, _resample_htf


def make_ohlcv(closes, volume=None, noise=1.0, freq="1h", start="2024-01-01"):
    closes = np.asarray(closes, dtype=float)
    n = len(closes)
    if volume is None:
        volume = np.full(n, 100.0)
    idx = pd.date_range(start, periods=n, freq=freq)
    return pd.DataFrame(
        {
            "open": closes - noise * 0.3,
            "high": closes + noise,
            "low": closes - noise,
            "close": closes,
            "volume": np.asarray(volume, dtype=float),
        },
        index=idx,
    )


def build_uptrend_with_pullback(n_trend=900, dip=10.0):
    """A steady 1h uptrend long enough to prime the 4h EMA(50) gate, then a
    pullback that tags the LTF EMA(20), then a resumption bar that breaks the
    prior bar's high."""
    closes = list(np.linspace(100, 400, n_trend))
    base = closes[-1]
    # Pullback: drift down toward the LTF EMA over a few bars.
    closes += [base - 2, base - 5, base - dip]
    # Resumption: strong up bars breaking the prior highs.
    closes += [base - 4, base + 4, base + 10]
    return make_ohlcv(closes)


def build_downtrend_with_rally(n_trend=900, pop=10.0):
    closes = list(np.linspace(400, 100, n_trend))
    base = closes[-1]
    closes += [base + 2, base + 5, base + pop]   # rally into the LTF EMA
    closes += [base + 4, base - 4, base - 10]    # breakdown
    return make_ohlcv(closes)


# ── Basic shape / safety ─────────────────────────────────────────────────────


def test_columns_present():
    out = mtf_confluence_core(build_uptrend_with_pullback())
    for col in ("signal", "position", "htf_trend", "htf_ema_fast",
                "htf_ema_slow", "ltf_ema"):
        assert col in out.columns


def test_empty_df_is_safe():
    df = pd.DataFrame(columns=["open", "high", "low", "close", "volume"])
    out = mtf_confluence_core(df)
    assert "signal" in out.columns
    assert len(out) == 0


def test_warmup_returns_no_signal():
    """Too few bars to prime the HTF slow EMA → neutral everywhere."""
    df = make_ohlcv(np.linspace(100, 200, 120))
    out = mtf_confluence_core(df)
    assert (out["signal"] == 0).all()
    assert (out["htf_trend"] == 0).all()


def test_range_index_fallback_is_safe():
    """Non-datetime index falls back to positional bucketing, no crash."""
    df = make_ohlcv(np.linspace(100, 400, 903)).reset_index(drop=True)
    out = mtf_confluence_core(df)
    assert "signal" in out.columns
    assert out["htf_trend"].iloc[-1] == 1


# ── Signal-value tests ───────────────────────────────────────────────────────


def test_uptrend_pullback_fires_long_at_resumption_bar():
    df = build_uptrend_with_pullback()
    out = mtf_confluence_core(df)
    fired = out.index[out["signal"] == 1]
    assert len(fired) >= 1, "expected a long entry on a resumption bar"
    # The engineered resumption sequence is the last 3 bars; the entry must
    # land there (close reclaims the LTF EMA and breaks the prior high).
    assert fired[-1] >= df.index[-3]
    # And the HTF trend at the entry bar must be up.
    assert (out.loc[fired, "htf_trend"] == 1).all()


def test_long_only_default_suppresses_short_entries():
    df = build_downtrend_with_rally()
    out = mtf_confluence_core(df)  # allow_short defaults to False
    assert (out["position"] >= 0).all()
    assert not (out["signal"] == -1).any() or (out["position"] == 0).all()


def test_downtrend_rally_fires_short_when_allowed():
    df = build_downtrend_with_rally()
    out = mtf_confluence_core(df, allow_short=True)
    fired = out.index[(out["signal"] == -1) & (out["position"] == -1)]
    assert len(fired) >= 1, "expected a short entry on the breakdown bar"
    assert (out.loc[fired, "htf_trend"] == -1).all()


def test_htf_downtrend_blocks_long_pullback_trigger():
    """The same LTF pullback-resumption shape inside an HTF downtrend must not
    produce a long: the HTF gate is the whole point."""
    df = build_downtrend_with_rally()
    out = mtf_confluence_core(df)
    assert not (out["position"] == 1).any()


def test_exit_emitted_when_htf_trend_flips():
    """After a long entry, rolling the trend over hard must flatten the
    position via signal=-1 once the HTF view registers the flip."""
    up = build_uptrend_with_pullback()
    base = up["close"].iloc[-1]
    n_down = 240
    down_closes = np.linspace(base, base - 250, n_down)
    down = make_ohlcv(
        down_closes, start=up.index[-1] + pd.Timedelta(hours=1))
    df = pd.concat([up, down])
    out = mtf_confluence_core(df)
    assert (out["signal"] == 1).any(), "needs the long entry first"
    entry_ts = out.index[out["signal"] == 1][-1]
    after = out.loc[entry_ts:]
    exits = after.index[after["signal"] == -1]
    assert len(exits) >= 1, "expected an exit when the HTF trend flipped"
    assert out["position"].iloc[-1] <= 0
    # Exit reason is the gate, not a short trigger: position goes to 0 or -1
    # only via the clamped transition.
    assert set(out["signal"].unique()) <= {-1, 0, 1}


def test_flat_market_no_signal():
    n = 900
    closes = 100.0 + np.random.RandomState(0).randn(n) * 0.05
    df = make_ohlcv(closes, noise=0.2)
    out = mtf_confluence_core(df)
    assert (out["signal"] == 0).all()


# ── HTF resample mechanics ───────────────────────────────────────────────────


def test_resample_buckets_are_epoch_aligned_4h():
    df = make_ohlcv(np.linspace(100, 110, 50), start="2024-01-01 03:00")
    htf, visible_at = _resample_htf(df, 4)
    # 1h bars from 03:00 → first bucket label is the epoch-aligned 00:00.
    assert htf.index[0] == pd.Timestamp("2024-01-01 00:00")
    assert (htf.index.hour % 4 == 0).all()
    # Bucket [00:00, 04:00) is visible at the native bar labeled 03:00.
    assert visible_at[0] == pd.Timestamp("2024-01-01 03:00")


def test_incomplete_trailing_bucket_never_visible():
    """A frame ending mid-bucket must not expose that bucket's aggregates:
    its visible-at label lies beyond the end of the native index."""
    # 1h bars 00:00..21:00 → last 4h bucket [20:00, 00:00) holds only 2 bars.
    df = make_ohlcv(np.linspace(100, 110, 22))
    htf, visible_at = _resample_htf(df, 4)
    assert visible_at[-1] > df.index[-1]
    proj = pd.Series(htf["close"].to_numpy(), index=visible_at).reindex(
        df.index, method="ffill")
    # The projected HTF close at the final native bar is the last COMPLETE
    # bucket's close (the 19:00 bar), not the in-progress bucket's.
    assert proj.iloc[-1] == df["close"].iloc[19]


# ── Look-ahead regression (the #955 bar) ─────────────────────────────────────


def test_prefix_consistency_no_lookahead():
    """Gold standard: signals over df[:k] must equal the first k signals over
    the full df, for cutoffs landing at every offset within an HTF bucket.
    Any read of the in-progress HTF bucket (or any other future leak) breaks
    this for some k."""
    df = build_uptrend_with_pullback()
    full = mtf_confluence_core(df)["signal"]
    n = len(df)
    for k in range(n - 9, n):  # covers all 4 intra-bucket offsets, twice
        prefix = mtf_confluence_core(df.iloc[:k])["signal"]
        pd.testing.assert_series_equal(
            prefix, full.iloc[:k], check_names=False, obj=f"prefix k={k}"
        )


def test_future_rows_do_not_change_past_signals():
    """Perturb rows after a cutoff (including bars inside the then-current
    HTF bucket) wildly; signals at and before the cutoff must be unchanged."""
    df = build_uptrend_with_pullback()
    n = len(df)
    cut = n - 6
    base = mtf_confluence_core(df)["signal"].iloc[:cut]

    crashed = df.copy()
    crashed.loc[crashed.index[cut:], ["open", "high", "low", "close"]] = 1.0
    out_crashed = mtf_confluence_core(crashed)["signal"].iloc[:cut]
    pd.testing.assert_series_equal(base, out_crashed, check_names=False)

    mooned = df.copy()
    mooned.loc[mooned.index[cut:], ["open", "high", "low", "close"]] *= 10.0
    out_mooned = mtf_confluence_core(mooned)["signal"].iloc[:cut]
    pd.testing.assert_series_equal(base, out_mooned, check_names=False)


def test_htf_trend_lags_not_leads_the_bucket():
    """At a native bar mid-bucket, htf_trend must reflect only buckets that
    already closed: rewriting the remainder of the current bucket cannot
    change htf_trend at that bar."""
    df = build_uptrend_with_pullback()
    out = mtf_confluence_core(df)
    # Pick a bar that is NOT the last bar of its 4h bucket (hour % 4 != 3).
    mid_bucket = [ts for ts in df.index[-30:] if ts.hour % 4 != 3][0]
    mutated = df.copy()
    later = mutated.index > mid_bucket
    mutated.loc[later, ["open", "high", "low", "close"]] = 1.0
    out_mut = mtf_confluence_core(mutated)
    assert out.loc[mid_bucket, "htf_trend"] == out_mut.loc[mid_bucket, "htf_trend"]
