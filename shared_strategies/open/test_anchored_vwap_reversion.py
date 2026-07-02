"""Tests for anchored_vwap_reversion.py — single-AVWAP stretch-fade strategy."""

import importlib.util
import os

import numpy as np
import pandas as pd

from anchored_vwap_reversion import anchored_vwap_reversion_core


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


_PARAMS = dict(pivot_strength=2, entry_atr_mult=1.0, buffer_atr_mult=0.0,
               confirm_bars=2, atr_period=3)


def _long_stretch_df():
    """Swing low at 5 (conf. 7); bar 9 wicks through the lower ATR band and
    closes back inside it — below the line; bar 10 holds in the zone -> +1 at 10."""
    closes = [110, 108, 106, 104, 102, 100, 101, 102, 101, 100.2, 100.6]
    lows = np.asarray(closes) - 0.5
    lows[9] = 99.0
    return _ohlcv(closes, lows=lows, volume=10.0)


def _short_stretch_df():
    """Swing high at 5 (conf. 7); bar 9 wicks through the upper ATR band and
    closes back inside it — above the line; bar 10 holds in the zone -> -1 at 10."""
    closes = [90, 92, 94, 96, 98, 100, 99, 98, 99, 99.8, 99.4]
    highs = np.asarray(closes) + 0.5
    highs[9] = 102.0
    return _ohlcv(closes, highs=highs, volume=10.0)


# --- scaffold + guards -----------------------------------------------------------

def test_empty_and_short_df_return_zero_signal():
    empty = pd.DataFrame(columns=["open", "high", "low", "close", "volume"])
    out = anchored_vwap_reversion_core(empty)
    assert list(out["signal"]) == []
    for col in ("avwap", "anchor_index", "atr"):
        assert col in out.columns, col

    short = _ohlcv(np.linspace(100, 101, 6))  # < 2*5+1+2
    out = anchored_vwap_reversion_core(short)
    assert (out["signal"] == 0).all()
    assert (out["anchor_index"] == -1).all()


# --- anchor index ----------------------------------------------------------------

def test_anchor_confirms_at_pivot_plus_strength():
    df = _long_stretch_df()
    out = anchored_vwap_reversion_core(df, **_PARAMS)
    anchor = out["anchor_index"].to_numpy()
    # Low pivot at 5 confirms at 7; the bar-7 swing high (strict max of highs
    # 5..9) re-anchors from its confirmation at 9. Bar 9's hand-set wick low
    # would need bar 11 to confirm and the fixture ends at 10.
    assert (anchor[:7] == -1).all()
    assert anchor[7] == 5 and anchor[8] == 5
    assert (anchor[9:] == 7).all()


def test_no_signal_before_first_anchor():
    # Monotonic decline: equal-step lows tie under the strict pivot rule, so
    # nothing confirms and no stretch may fire however deep price falls.
    closes = [110, 108, 106, 104, 102, 100, 98, 96, 94, 92, 90, 88]
    out = anchored_vwap_reversion_core(_ohlcv(closes, volume=10.0), **_PARAMS)
    assert (out["anchor_index"] == -1).all()
    assert (out["signal"] == 0).all()


# --- AVWAP -----------------------------------------------------------------------

def test_avwap_matches_hand_computed_prefix_sum():
    closes = [104, 106, 108, 106, 104, 102, 100, 102, 104, 106, 108, 110]
    flat = np.asarray(closes, dtype=float)   # tp == close when high==low==close
    df = _ohlcv(closes, highs=flat, lows=flat, volume=10.0)
    out = anchored_vwap_reversion_core(df, pivot_strength=2, confirm_bars=2, atr_period=3)
    avwap = out["avwap"].to_numpy()
    # High pivot at 2 confirms at 4; low pivot at 6 re-anchors from bar 8.
    assert np.isnan(avwap[:4]).all()
    for nbar in (4, 5, 6, 7):
        expected = np.mean(closes[2:nbar + 1])
        assert abs(avwap[nbar] - expected) < 1e-9, (nbar, avwap[nbar], expected)
    for nbar in (8, 9, 10, 11):
        expected = np.mean(closes[6:nbar + 1])
        assert abs(avwap[nbar] - expected) < 1e-9, (nbar, avwap[nbar], expected)


def test_avwap_zero_volume_window_falls_back_to_typical():
    closes = [104, 106, 108, 106, 104, 102, 100, 102, 104, 106, 108, 110]
    df = _ohlcv(closes, volume=0.0)
    out = anchored_vwap_reversion_core(df, pivot_strength=2, confirm_bars=2, atr_period=3)
    tp = (df["high"] + df["low"] + df["close"]).to_numpy() / 3.0
    avwap = out["avwap"].to_numpy()
    for nbar in range(4, 12):
        assert abs(avwap[nbar] - tp[nbar]) < 1e-9


# --- triggers --------------------------------------------------------------------

def test_long_stretch_fires_once_on_completing_bar():
    out = anchored_vwap_reversion_core(_long_stretch_df(), **_PARAMS)
    sig = out["signal"].to_numpy()
    assert sig[10] == 1
    assert (sig == 1).sum() == 1
    assert (sig == -1).sum() == 0
    # The trigger bar (9) wicked through the band the prior bar (8) did not touch,
    # and both window closes held below the line (the target not yet reached).
    avwap = out["avwap"].to_numpy()
    atr = out["atr"].to_numpy()
    low = out["low"].to_numpy()
    close = out["close"].to_numpy()
    lower_band = avwap - _PARAMS["entry_atr_mult"] * atr
    assert low[9] <= lower_band[9]
    assert low[8] > lower_band[8]
    assert close[9] < avwap[9] and close[10] < avwap[10]


def test_short_stretch_mirrors():
    out = anchored_vwap_reversion_core(_short_stretch_df(), **_PARAMS)
    sig = out["signal"].to_numpy()
    assert sig[10] == -1
    assert (sig == -1).sum() == 1
    assert (sig == 1).sum() == 0
    avwap = out["avwap"].to_numpy()
    atr = out["atr"].to_numpy()
    high = out["high"].to_numpy()
    close = out["close"].to_numpy()
    upper_band = avwap + _PARAMS["entry_atr_mult"] * atr
    assert high[9] >= upper_band[9]
    assert high[8] < upper_band[8]
    assert close[9] > avwap[9] and close[10] > avwap[10]


def test_no_snap_back_keeps_falling_knife_unfaded():
    """The trigger bar closes BELOW the band (stretch without recovery): the
    snap-back clause alone must hold the signal at 0 — the falling-knife guard."""
    closes = [110, 108, 106, 104, 102, 100, 101, 102, 101, 97.5, 97.9]
    lows = np.asarray(closes) - 0.5
    lows[9] = 97.0
    df = _ohlcv(closes, lows=lows, volume=10.0)
    out = anchored_vwap_reversion_core(df, **_PARAMS)
    # Non-vacuity: the touch clause genuinely holds at the would-be trigger bar...
    avwap = out["avwap"].to_numpy()
    atr = out["atr"].to_numpy()
    lower_band = avwap - _PARAMS["entry_atr_mult"] * atr
    assert df["low"].to_numpy()[9] <= lower_band[9]
    # ...but the close never recovered back inside the band.
    assert df["close"].to_numpy()[9] < lower_band[9]
    assert (out["signal"] == 0).all()


def test_line_reclaim_during_hold_kills_the_fire():
    """The hold bar closes back ABOVE the line: the reversion opportunity is
    gone (flip territory), so the zone-hold clause must hold the signal at 0."""
    closes = [110, 108, 106, 104, 102, 100, 101, 102, 101, 100.2, 101.5]
    lows = np.asarray(closes) - 0.5
    lows[9] = 99.0
    df = _ohlcv(closes, lows=lows, volume=10.0)
    out = anchored_vwap_reversion_core(df, **_PARAMS)
    # Non-vacuity: identical trigger-bar geometry to the firing fixture...
    avwap = out["avwap"].to_numpy()
    atr = out["atr"].to_numpy()
    lower_band = avwap - _PARAMS["entry_atr_mult"] * atr
    close = df["close"].to_numpy()
    assert df["low"].to_numpy()[9] <= lower_band[9]
    assert close[9] >= lower_band[9]
    assert close[9] < avwap[9]
    # ...but bar 10 closed above the live line.
    assert close[10] >= avwap[10]
    assert (out["signal"] == 0).all()


def test_buffer_blocks_shallow_snap_back():
    # Same stretch geometry, but demand a recovery margin the trigger bar's
    # close cannot meet -> no signal anywhere.
    out = anchored_vwap_reversion_core(
        _long_stretch_df(), pivot_strength=2, entry_atr_mult=1.0,
        buffer_atr_mult=5.0, confirm_bars=2, atr_period=3)
    assert (out["signal"] == 0).all()


def test_nan_atr_warmup_yields_no_signal():
    out = anchored_vwap_reversion_core(
        _long_stretch_df(), pivot_strength=2, entry_atr_mult=1.0,
        buffer_atr_mult=0.0, confirm_bars=2, atr_period=99)
    assert (out["signal"] == 0).all()


# --- registry --------------------------------------------------------------------

def _load_registry():
    here = os.path.dirname(os.path.abspath(__file__))
    spec = importlib.util.spec_from_file_location(
        "_reg_under_test_avwap_reversion", os.path.join(here, "registry.py")
    )
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def test_registered_for_spot_and_futures():
    reg = _load_registry()
    for platform in ("spot", "futures"):
        assert "anchored_vwap_reversion" in reg.build_registry(platform), platform
        assert "anchored_vwap_reversion" in reg.PLATFORM_ORDER[platform], platform


def test_registered_fn_applies_via_registry():
    reg = _load_registry()
    entry = reg.STRATEGIES["anchored_vwap_reversion"]
    df = _long_stretch_df()
    out = entry["fn"](df, **entry["default_params"])
    assert "signal" in out.columns
