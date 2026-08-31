
import importlib.util
import os

import numpy as np
import pandas as pd

from shared_strategies.open.conftest import load_module, make_ohlcv

_ANCHORED_VWAP = load_module("_anchored_vwap_test", os.path.join(os.path.dirname(__file__), "anchored_vwap.py"))
_INDICATORS_CORE = load_module("_anchored_vwap_indicators_test", os.path.join(os.path.dirname(__file__), "indicators_core.py"))
anchored_vwap_core = _ANCHORED_VWAP.anchored_vwap_core
_inline_rsi = _INDICATORS_CORE.wilder_rsi
_ohlcv = make_ohlcv


def test_empty_and_short_df_return_zero_signal():
    empty = pd.DataFrame(columns=["open", "high", "low", "close", "volume"])
    out = anchored_vwap_core(empty)
    assert list(out["signal"]) == []
    assert "avwap" in out.columns and "anchor_index" in out.columns and "atr" in out.columns

    short = _ohlcv(np.linspace(100, 101, 6))
    out = anchored_vwap_core(short)
    assert (out["signal"] == 0).all()
    assert (out["anchor_index"] == -1).all()


def test_strict_pivot_and_confirmed_anchor_index():
    closes = [110, 108, 106, 104, 102, 100, 102, 104, 106, 108, 110, 112]
    df = _ohlcv(closes, highs=np.array(closes) + 0.5, lows=np.array(closes) - 0.5)
    out = anchored_vwap_core(df, pivot_strength=2, confirm_bars=2)
    anchor = out["anchor_index"].to_numpy()
    assert (anchor[:7] == -1).all()
    assert (anchor[7:] == 5).all()


def test_flat_top_plateau_is_not_a_pivot():
    closes = [100, 102, 104, 106, 108, 108, 106, 104, 102, 100, 98, 96]
    highs = np.array(closes) + 0.2
    highs[4] = highs[5] = 110.0
    df = _ohlcv(closes, highs=highs, lows=np.array(closes) - 0.5)
    out = anchored_vwap_core(df, pivot_strength=2, confirm_bars=2)
    assert not np.isin(out["anchor_index"].to_numpy(), [4, 5]).any()


def test_avwap_matches_hand_computed_prefix_sum():
    closes = [110, 108, 106, 104, 102, 100, 102, 104, 106, 108, 110, 112]
    highs = np.array(closes) + 0.0
    lows = np.array(closes) + 0.0
    df = _ohlcv(closes, highs=highs, lows=lows, volume=10.0)
    out = anchored_vwap_core(df, pivot_strength=2, confirm_bars=2)
    avwap = out["avwap"].to_numpy()
    for nbar in (7, 8, 9, 10, 11):
        expected = np.mean(closes[5:nbar + 1])
        assert abs(avwap[nbar] - expected) < 1e-9, (nbar, avwap[nbar], expected)
    assert np.isnan(avwap[:7]).all()


def test_avwap_zero_volume_window_falls_back_to_typical():
    closes = [110, 108, 106, 104, 102, 100, 102, 104, 106, 108, 110, 112]
    df = _ohlcv(closes, volume=0.0)
    out = anchored_vwap_core(df, pivot_strength=2, confirm_bars=2)
    avwap = out["avwap"].to_numpy()
    tp = (df["high"] + df["low"] + df["close"]).to_numpy() / 3.0
    for nbar in range(7, 12):
        assert abs(avwap[nbar] - tp[nbar]) < 1e-9


def _long_reclaim_df():
    closes = [110, 108, 106, 104, 102, 100,
              100.5, 100.2, 99.8, 99.5,
              103.5, 104.0, 104.5, 105.0]
    return _ohlcv(closes, volume=10.0)


def test_long_signal_fires_once_on_completing_bar():
    df = _long_reclaim_df()
    out = anchored_vwap_core(df, pivot_strength=2, buffer_atr_mult=0.0, confirm_bars=2, atr_period=3)
    sig = out["signal"].to_numpy()
    longs = np.where(sig == 1)[0]
    assert len(longs) == 1, longs
    b = longs[0]
    win_start = b - 2 + 1
    assert out["close"].to_numpy()[win_start - 1] < out["avwap"].to_numpy()[win_start - 1]


def test_no_signal_before_first_anchor():
    df = _long_reclaim_df()
    out = anchored_vwap_core(df, pivot_strength=2, buffer_atr_mult=0.0, confirm_bars=2, atr_period=3)
    assert (out["signal"].to_numpy()[:7] == 0).all()


def test_short_signal_mirrors():
    closes = [90, 92, 94, 96, 98, 100,
              99.5, 99.8, 100.2, 100.5,
              96.5, 96.0, 95.5, 95.0]
    df = _ohlcv(closes, volume=10.0)
    out = anchored_vwap_core(df, pivot_strength=2, buffer_atr_mult=0.0, confirm_bars=2, atr_period=3)
    sig = out["signal"].to_numpy()
    assert (sig == -1).sum() == 1
    assert (sig == 1).sum() == 0


def test_nan_atr_warmup_yields_no_signal():
    df = _long_reclaim_df()
    out = anchored_vwap_core(df, pivot_strength=2, buffer_atr_mult=0.25, confirm_bars=2, atr_period=99)
    assert (out["signal"] == 0).all()


_GATE_BASE_KW = dict(pivot_strength=2, buffer_atr_mult=0.0, confirm_bars=2, atr_period=3)


def _high_prior_long_reclaim_df():
    closes = [140, 138, 136, 134, 132, 100,
              100.5, 100.2, 99.8, 99.5,
              103.5, 104.0, 104.5, 105.0]
    return _ohlcv(closes, volume=10.0)


def test_inline_rsi_extremes_and_warmup():
    rising = pd.Series(np.linspace(100, 113, 14))
    rsi = _inline_rsi(rising, 3)
    assert rsi.iloc[:3].isna().all()
    assert (rsi.iloc[3:] == 100.0).all()
    falling = pd.Series(np.linspace(113, 100, 14))
    assert (_inline_rsi(falling, 3).iloc[3:] == 0.0).all()


def test_gate_default_off_is_bit_identical():
    df = _long_reclaim_df()
    base = anchored_vwap_core(df, **_GATE_BASE_KW)
    off = anchored_vwap_core(df, **_GATE_BASE_KW, gate_rsi_period=0, gate_ema_period=0)
    assert (base["signal"] == off["signal"]).all()
    assert "gate_rsi" not in off.columns and "gate_ema" not in off.columns


def test_rsi_gate_blocks_long_below_level_and_passes_above():
    df = _long_reclaim_df()
    base = anchored_vwap_core(df, **_GATE_BASE_KW)
    b = int(np.where(base["signal"].to_numpy() == 1)[0][0])
    probe = anchored_vwap_core(df, **_GATE_BASE_KW, gate_rsi_period=6, gate_rsi_level=0.0)
    assert probe["signal"].iloc[b] == 1
    rsi_b = float(probe["gate_rsi"].iloc[b])
    assert not np.isnan(rsi_b)
    blocked = anchored_vwap_core(
        df, **_GATE_BASE_KW, gate_rsi_period=6, gate_rsi_level=rsi_b + 1.0)
    assert blocked["signal"].iloc[b] == 0
    assert (blocked["signal"] == 0).all()
    passed = anchored_vwap_core(
        df, **_GATE_BASE_KW, gate_rsi_period=6, gate_rsi_level=rsi_b - 1.0)
    assert passed["signal"].iloc[b] == 1


def test_rsi_gate_blocks_short_above_level_mirror():
    closes = [90, 92, 94, 96, 98, 100,
              99.5, 99.8, 100.2, 100.5,
              96.5, 96.0, 95.5, 95.0]
    df = _ohlcv(closes, volume=10.0)
    base = anchored_vwap_core(df, **_GATE_BASE_KW)
    b = int(np.where(base["signal"].to_numpy() == -1)[0][0])
    probe = anchored_vwap_core(df, **_GATE_BASE_KW, gate_rsi_period=6, gate_rsi_level=100.0)
    assert probe["signal"].iloc[b] == -1
    rsi_b = float(probe["gate_rsi"].iloc[b])
    blocked = anchored_vwap_core(
        df, **_GATE_BASE_KW, gate_rsi_period=6, gate_rsi_level=rsi_b - 1.0)
    assert blocked["signal"].iloc[b] == 0
    passed = anchored_vwap_core(
        df, **_GATE_BASE_KW, gate_rsi_period=6, gate_rsi_level=rsi_b + 1.0)
    assert passed["signal"].iloc[b] == -1


def test_rsi_gate_warmup_nan_fails_open():
    df = _long_reclaim_df()
    base = anchored_vwap_core(df, **_GATE_BASE_KW)
    b = int(np.where(base["signal"].to_numpy() == 1)[0][0])
    gated = anchored_vwap_core(
        df, **_GATE_BASE_KW, gate_rsi_period=13, gate_rsi_level=100.0)
    assert np.isnan(gated["gate_rsi"].iloc[b])
    assert (gated["signal"] == base["signal"]).all()


def test_ema_gate_blocks_counter_trend_long():
    df = _high_prior_long_reclaim_df()
    base = anchored_vwap_core(df, **_GATE_BASE_KW)
    longs = np.where(base["signal"].to_numpy() == 1)[0]
    assert len(longs) == 1
    b = int(longs[0])
    gated = anchored_vwap_core(df, **_GATE_BASE_KW, gate_ema_period=10)
    assert b >= 10
    assert float(gated["gate_ema"].iloc[b]) > float(df["close"].iloc[b])
    assert (gated["signal"] == 0).all()


def test_ema_gate_passes_aligned_long_and_short():
    long_df = _high_prior_long_reclaim_df()
    base = anchored_vwap_core(long_df, **_GATE_BASE_KW)
    b = int(np.where(base["signal"].to_numpy() == 1)[0][0])
    gated = anchored_vwap_core(long_df, **_GATE_BASE_KW, gate_ema_period=3)
    assert float(gated["gate_ema"].iloc[b]) < float(long_df["close"].iloc[b])
    assert gated["signal"].iloc[b] == 1
    short_df = _ohlcv([90, 92, 94, 96, 98, 100,
                       99.5, 99.8, 100.2, 100.5,
                       96.5, 96.0, 95.5, 95.0], volume=10.0)
    sbase = anchored_vwap_core(short_df, **_GATE_BASE_KW)
    sb = int(np.where(sbase["signal"].to_numpy() == -1)[0][0])
    sgated = anchored_vwap_core(short_df, **_GATE_BASE_KW, gate_ema_period=3)
    assert float(sgated["gate_ema"].iloc[sb]) > float(short_df["close"].iloc[sb])
    assert sgated["signal"].iloc[sb] == -1


def test_ema_gate_warmup_fails_open():
    df = _high_prior_long_reclaim_df()
    base = anchored_vwap_core(df, **_GATE_BASE_KW)
    b = int(np.where(base["signal"].to_numpy() == 1)[0][0])
    gated = anchored_vwap_core(df, **_GATE_BASE_KW, gate_ema_period=12)
    assert b < 12
    assert (gated["signal"] == base["signal"]).all()


def test_gated_signal_independent_of_future_bars():
    df = _high_prior_long_reclaim_df()
    kw = dict(_GATE_BASE_KW, gate_rsi_period=6, gate_rsi_level=50.0, gate_ema_period=3)
    full = anchored_vwap_core(df, **kw)
    for k in range(8, len(df)):
        partial = anchored_vwap_core(df.iloc[:k + 1], **kw)
        assert (
            partial["signal"].to_numpy()
            == full["signal"].to_numpy()[:k + 1]
        ).all(), k


def _load_registry():
    here = os.path.dirname(os.path.abspath(__file__))
    spec = importlib.util.spec_from_file_location(
        "_reg_under_test_avwap", os.path.join(here, "registry.py")
    )
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def test_registered_for_spot_and_futures():
    reg = _load_registry()
    for platform in ("spot", "futures"):
        assert "anchored_vwap" in reg.build_registry(platform), platform
        assert "anchored_vwap" in reg.PLATFORM_ORDER[platform], platform


def test_registered_fn_applies_via_registry():
    reg = _load_registry()
    entry = reg.STRATEGIES["anchored_vwap"]
    df = _long_reclaim_df()
    out = entry["fn"](df, **entry["default_params"])
    assert "signal" in out.columns
