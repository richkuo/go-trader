"""Tests for anchored_vwap_channel.py — dual-AVWAP channel bounce strategy."""

import importlib.util
import os

import numpy as np
import pandas as pd

from anchored_vwap_channel import anchored_vwap_channel_core


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


_PARAMS = dict(pivot_strength=2, buffer_atr_mult=0.0, confirm_bars=2,
               min_width_atr_mult=0.0, atr_period=3)


def _long_bounce_df():
    """High pivot at 2 (conf. 4), low pivot at 6 (conf. 8); bar 10 wicks
    through the support line and closes back above; bar 11 holds -> +1 at 11."""
    closes = [104, 106, 108, 106, 104, 102, 100, 102, 104, 103, 102.5, 103.5]
    lows = np.asarray(closes) - 0.5
    lows[10] = 101.0
    return _ohlcv(closes, lows=lows, volume=10.0)


def _short_rejection_df():
    """Low pivot at 2 (conf. 4), high pivot at 6 (conf. 8); bar 10 wicks
    through the resistance line and closes back below; bar 11 holds -> -1 at 11."""
    closes = [100, 98, 96, 98, 100, 102, 104, 102, 100, 101, 101.5, 100.5]
    highs = np.asarray(closes) + 0.5
    highs[10] = 103.0
    return _ohlcv(closes, highs=highs, volume=10.0)


# --- scaffold + guards -----------------------------------------------------------

def test_empty_and_short_df_return_zero_signal():
    empty = pd.DataFrame(columns=["open", "high", "low", "close", "volume"])
    out = anchored_vwap_channel_core(empty)
    assert list(out["signal"]) == []
    for col in ("avwap_support", "avwap_resistance", "anchor_low_index",
                "anchor_high_index", "atr"):
        assert col in out.columns, col

    short = _ohlcv(np.linspace(100, 101, 6))  # < 2*5+1+2
    out = anchored_vwap_channel_core(short)
    assert (out["signal"] == 0).all()
    assert (out["anchor_low_index"] == -1).all()
    assert (out["anchor_high_index"] == -1).all()


# --- typed pivots + per-type anchor indices --------------------------------------

def test_typed_anchors_confirm_independently():
    df = _long_bounce_df()
    out = anchored_vwap_channel_core(df, **_PARAMS)
    ah = out["anchor_high_index"].to_numpy()
    al = out["anchor_low_index"].to_numpy()
    # High pivot at 2 confirms at 4; low pivot at 6 confirms at 8.
    assert (ah[:4] == -1).all()
    assert (ah[4:10] == 2).all()
    assert (al[:8] == -1).all()
    assert (al[8:] == 6).all()


def test_no_signal_before_both_anchors():
    # Only a swing high forms (monotonic decline afterwards): support never
    # exists, so no signal can fire even on deep touches of the single line.
    closes = [104, 106, 108, 106, 104, 102, 100, 98, 96, 94, 92, 90]
    out = anchored_vwap_channel_core(_ohlcv(closes, volume=10.0), **_PARAMS)
    assert (out["anchor_low_index"] == -1).all()
    assert (out["signal"] == 0).all()


# --- both AVWAPs -----------------------------------------------------------------

def test_avwaps_match_hand_computed_prefix_sums():
    closes = [104, 106, 108, 106, 104, 102, 100, 102, 104, 106, 108, 110]
    flat = np.asarray(closes, dtype=float)   # tp == close when high==low==close
    df = _ohlcv(closes, highs=flat, lows=flat, volume=10.0)
    out = anchored_vwap_channel_core(df, pivot_strength=2, confirm_bars=2, atr_period=3)
    support = out["avwap_support"].to_numpy()
    resistance = out["avwap_resistance"].to_numpy()
    # Resistance anchored at the high pivot (bar 2) from its confirmation (bar 4);
    # support anchored at the low pivot (bar 6) from bar 8.
    assert np.isnan(resistance[:4]).all()
    assert np.isnan(support[:8]).all()
    for nbar in (4, 6, 8, 10, 11):
        expected = np.mean(closes[2:nbar + 1])
        assert abs(resistance[nbar] - expected) < 1e-9, (nbar, resistance[nbar], expected)
    for nbar in (8, 9, 10, 11):
        expected = np.mean(closes[6:nbar + 1])
        assert abs(support[nbar] - expected) < 1e-9, (nbar, support[nbar], expected)


def test_zero_volume_window_falls_back_to_typical():
    closes = [104, 106, 108, 106, 104, 102, 100, 102, 104, 106, 108, 110]
    df = _ohlcv(closes, volume=0.0)
    out = anchored_vwap_channel_core(df, pivot_strength=2, confirm_bars=2, atr_period=3)
    tp = (df["high"] + df["low"] + df["close"]).to_numpy() / 3.0
    support = out["avwap_support"].to_numpy()
    resistance = out["avwap_resistance"].to_numpy()
    for nbar in range(8, 12):
        assert abs(support[nbar] - tp[nbar]) < 1e-9
        assert abs(resistance[nbar] - tp[nbar]) < 1e-9


# --- triggers --------------------------------------------------------------------

def test_long_bounce_fires_once_on_completing_bar():
    out = anchored_vwap_channel_core(_long_bounce_df(), **_PARAMS)
    sig = out["signal"].to_numpy()
    assert sig[11] == 1
    assert (sig == 1).sum() == 1
    assert (sig == -1).sum() == 0
    # The touch bar (10) dipped through the line the prior bar (9) did not touch.
    support = out["avwap_support"].to_numpy()
    low = out["low"].to_numpy()
    assert low[10] <= support[10]
    assert low[9] > support[9]


def test_short_rejection_mirrors():
    out = anchored_vwap_channel_core(_short_rejection_df(), **_PARAMS)
    sig = out["signal"].to_numpy()
    assert sig[11] == -1
    assert (sig == -1).sum() == 1
    assert (sig == 1).sum() == 0
    resistance = out["avwap_resistance"].to_numpy()
    high = out["high"].to_numpy()
    assert high[10] >= resistance[10]
    assert high[9] < resistance[9]


def test_buffer_blocks_shallow_reclaim():
    # Same bounce geometry, but demand a reclaim margin the touch bar's close
    # cannot meet -> no signal anywhere.
    out = anchored_vwap_channel_core(
        _long_bounce_df(), pivot_strength=2, buffer_atr_mult=5.0,
        confirm_bars=2, min_width_atr_mult=0.0, atr_period=3)
    assert (out["signal"] == 0).all()


def test_nan_atr_warmup_yields_no_signal():
    out = anchored_vwap_channel_core(
        _long_bounce_df(), pivot_strength=2, buffer_atr_mult=0.0,
        confirm_bars=2, min_width_atr_mult=0.0, atr_period=99)
    assert (out["signal"] == 0).all()


# --- channel-validity gates ------------------------------------------------------

def test_inverted_channel_blocks_would_be_bounce():
    """Rally after the V: the low-anchored line climbs above the high-anchored
    line. A deep wick then satisfies every LONG clause except channel order —
    the inversion gate alone must hold the signal at 0."""
    closes = [104, 106, 108, 106, 104, 102, 100, 104, 108, 112, 116, 120, 118, 119]
    lows = np.asarray(closes) - 0.5
    lows[12] = 108.0  # would-be touch of the support line
    df = _ohlcv(closes, lows=lows, volume=10.0)
    out = anchored_vwap_channel_core(df, **_PARAMS)
    support = out["avwap_support"].to_numpy()
    resistance = out["avwap_resistance"].to_numpy()
    close = out["close"].to_numpy()
    low = out["low"].to_numpy()
    b = 12  # trigger bar for a window completing at 13
    # Non-vacuity: the long clauses genuinely hold at b...
    assert low[b] <= support[b]
    assert close[b] >= support[b]
    assert close[13] >= support[13]
    assert low[b - 1] > support[b - 1]
    # ...but the channel is inverted there, so nothing may fire.
    assert support[b] >= resistance[b]
    assert (out["signal"] == 0).all()


def test_min_width_gate_blocks_thin_channel():
    # The exact fixture that fires +1 under min_width_atr_mult=0 stays silent
    # when the required width exceeds what the channel offers.
    out = anchored_vwap_channel_core(
        _long_bounce_df(), pivot_strength=2, buffer_atr_mult=0.0,
        confirm_bars=2, min_width_atr_mult=50.0, atr_period=3)
    assert (out["signal"] == 0).all()


# --- registry --------------------------------------------------------------------

def _load_registry():
    here = os.path.dirname(os.path.abspath(__file__))
    spec = importlib.util.spec_from_file_location(
        "_reg_under_test_avwap_channel", os.path.join(here, "registry.py")
    )
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def test_registered_for_spot_and_futures():
    reg = _load_registry()
    for platform in ("spot", "futures"):
        assert "anchored_vwap_channel" in reg.build_registry(platform), platform
        assert "anchored_vwap_channel" in reg.PLATFORM_ORDER[platform], platform


def test_registered_fn_applies_via_registry():
    reg = _load_registry()
    entry = reg.STRATEGIES["anchored_vwap_channel"]
    df = _long_bounce_df()
    out = entry["fn"](df, **entry["default_params"])
    assert "signal" in out.columns
