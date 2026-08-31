
import numpy as np
import pandas as pd

from shared_strategies.open.conftest import load_module, make_ohlcv

_MTF_CONFLUENCE = load_module("_mtf_confluence_test", __file__.replace("test_mtf_confluence.py", "mtf_confluence.py"))
mtf_confluence_core = _MTF_CONFLUENCE.mtf_confluence_core
_resample_htf = _MTF_CONFLUENCE._resample_htf


def build_uptrend_with_pullback(n_trend=900, dip=10.0):
    closes = list(np.linspace(100, 400, n_trend))
    base = closes[-1]
    closes += [base - 2, base - 5, base - dip]
    closes += [base - 4, base + 4, base + 10]
    return make_ohlcv(closes)


def build_downtrend_with_rally(n_trend=900, pop=10.0):
    closes = list(np.linspace(400, 100, n_trend))
    base = closes[-1]
    closes += [base + 2, base + 5, base + pop]
    closes += [base + 4, base - 4, base - 10]
    return make_ohlcv(closes)


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
    df = make_ohlcv(np.linspace(100, 200, 120))
    out = mtf_confluence_core(df)
    assert (out["signal"] == 0).all()
    assert (out["htf_trend"] == 0).all()


def test_range_index_fallback_is_safe():
    df = make_ohlcv(np.linspace(100, 400, 903)).reset_index(drop=True)
    out = mtf_confluence_core(df)
    assert "signal" in out.columns
    assert out["htf_trend"].iloc[-1] == 1


def test_uptrend_pullback_fires_long_at_resumption_bar():
    df = build_uptrend_with_pullback()
    out = mtf_confluence_core(df)
    fired = out.index[out["signal"] == 1]
    assert len(fired) >= 1, "expected a long entry on a resumption bar"
    assert fired[-1] >= df.index[-3]
    assert (out.loc[fired, "htf_trend"] == 1).all()


def test_long_only_default_suppresses_short_entries():
    df = build_downtrend_with_rally()
    out = mtf_confluence_core(df)
    assert (out["position"] >= 0).all()
    assert not (out["signal"] == -1).any() or (out["position"] == 0).all()


def test_downtrend_rally_fires_short_when_allowed():
    df = build_downtrend_with_rally()
    out = mtf_confluence_core(df, allow_short=True)
    fired = out.index[(out["signal"] == -1) & (out["position"] == -1)]
    assert len(fired) >= 1, "expected a short entry on the breakdown bar"
    assert (out.loc[fired, "htf_trend"] == -1).all()


def test_htf_downtrend_blocks_long_pullback_trigger():
    df = build_downtrend_with_rally()
    out = mtf_confluence_core(df)
    assert not (out["position"] == 1).any()


def test_exit_emitted_when_htf_trend_flips():
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
    assert set(out["signal"].unique()) <= {-1, 0, 1}


def test_flat_market_no_signal():
    n = 900
    closes = 100.0 + np.random.RandomState(0).randn(n) * 0.05
    df = make_ohlcv(closes, noise=0.2)
    out = mtf_confluence_core(df)
    assert (out["signal"] == 0).all()


def test_resample_buckets_are_epoch_aligned_4h():
    df = make_ohlcv(np.linspace(100, 110, 50), start="2024-01-01 03:00")
    htf, visible_at = _resample_htf(df, 4)
    assert htf.index[0] == pd.Timestamp("2024-01-01 00:00")
    assert (htf.index.hour % 4 == 0).all()
    assert visible_at[0] == pd.Timestamp("2024-01-01 03:00")


def test_incomplete_trailing_bucket_never_visible():
    df = make_ohlcv(np.linspace(100, 110, 22))
    htf, visible_at = _resample_htf(df, 4)
    assert visible_at[-1] > df.index[-1]
    proj = pd.Series(htf["close"].to_numpy(), index=visible_at).reindex(
        df.index, method="ffill")
    assert proj.iloc[-1] == df["close"].iloc[19]


def test_prefix_consistency_no_lookahead():
    df = build_uptrend_with_pullback()
    full = mtf_confluence_core(df)["signal"]
    n = len(df)
    for k in range(n - 9, n):
        prefix = mtf_confluence_core(df.iloc[:k])["signal"]
        pd.testing.assert_series_equal(
            prefix, full.iloc[:k], check_names=False, obj=f"prefix k={k}"
        )


def test_future_rows_do_not_change_past_signals():
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
    df = build_uptrend_with_pullback()
    out = mtf_confluence_core(df)
    mid_bucket = [ts for ts in df.index[-30:] if ts.hour % 4 != 3][0]
    mutated = df.copy()
    later = mutated.index > mid_bucket
    mutated.loc[later, ["open", "high", "low", "close"]] = 1.0
    out_mut = mtf_confluence_core(mutated)
    assert out.loc[mid_bucket, "htf_trend"] == out_mut.loc[mid_bucket, "htf_trend"]
