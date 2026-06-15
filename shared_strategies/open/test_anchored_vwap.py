"""Tests for anchored_vwap.py — Anchored VWAP S/R-flip strategy."""

import importlib.util
import os

import numpy as np
import pandas as pd

from anchored_vwap import anchored_vwap_core


def _hourly_index(n, start="2026-01-01 00:00:00"):
    return pd.date_range(start, periods=n, freq="1h")


def _ohlcv(closes, highs=None, lows=None, opens=None, volume=100.0):
    closes = np.asarray(closes, dtype=float)
    n = len(closes)
    highs = closes + 0.5 if highs is None else np.asarray(highs, dtype=float)
    lows = closes - 0.5 if lows is None else np.asarray(lows, dtype=float)
    opens = closes if opens is None else np.asarray(opens, dtype=float)
    vol = np.full(n, float(volume)) if np.isscalar(volume) else np.asarray(volume, dtype=float)
    return pd.DataFrame(
        {"open": opens, "high": highs, "low": lows, "close": closes, "volume": vol},
        index=_hourly_index(n),
    )


# --- Task 1: scaffold + guards -------------------------------------------------

def test_empty_and_short_df_return_zero_signal():
    empty = pd.DataFrame(columns=["open", "high", "low", "close", "volume"])
    out = anchored_vwap_core(empty)
    assert list(out["signal"]) == []
    assert "avwap" in out.columns and "anchor_index" in out.columns and "atr" in out.columns

    short = _ohlcv(np.linspace(100, 101, 6))  # < 2*5+1+2
    out = anchored_vwap_core(short)
    assert (out["signal"] == 0).all()
    assert (out["anchor_index"] == -1).all()


# --- Task 2: pivots + anchor_index ---------------------------------------------

def test_strict_pivot_and_confirmed_anchor_index():
    closes = [110, 108, 106, 104, 102, 100, 102, 104, 106, 108, 110, 112]
    df = _ohlcv(closes, highs=np.array(closes) + 0.5, lows=np.array(closes) - 0.5)
    out = anchored_vwap_core(df, pivot_strength=2, confirm_bars=2)
    anchor = out["anchor_index"].to_numpy()
    # Trough index 5 is a strict low; confirmed at 5+2=7. Bars 0..6 have no anchor.
    assert (anchor[:7] == -1).all()
    assert (anchor[7:] == 5).all()


def test_flat_top_plateau_is_not_a_pivot():
    closes = [100, 102, 104, 106, 108, 108, 106, 104, 102, 100, 98, 96]
    highs = np.array(closes) + 0.2
    highs[4] = highs[5] = 110.0  # equal plateau highs
    df = _ohlcv(closes, highs=highs, lows=np.array(closes) - 0.5)
    out = anchored_vwap_core(df, pivot_strength=2, confirm_bars=2)
    assert not np.isin(out["anchor_index"].to_numpy(), [4, 5]).any()


# --- Task 3: AVWAP -------------------------------------------------------------

def test_avwap_matches_hand_computed_prefix_sum():
    closes = [110, 108, 106, 104, 102, 100, 102, 104, 106, 108, 110, 112]
    highs = np.array(closes) + 0.0   # tp == close when high==low==close
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


# --- Task 4: trigger -----------------------------------------------------------

def _long_reclaim_df():
    closes = [110, 108, 106, 104, 102, 100,   # trough at idx 5
              100.5, 100.2, 99.8, 99.5,        # chop below the rising AVWAP
              103.5, 104.0, 104.5, 105.0]      # buffered reclaim + hold
    return _ohlcv(closes, volume=10.0)


def test_long_signal_fires_once_on_completing_bar():
    df = _long_reclaim_df()
    out = anchored_vwap_core(df, pivot_strength=2, buffer_atr_mult=0.0, confirm_bars=2, atr_period=3)
    sig = out["signal"].to_numpy()
    longs = np.where(sig == 1)[0]
    assert len(longs) == 1, longs               # fire-once
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


# --- Task 5: registry ----------------------------------------------------------

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
