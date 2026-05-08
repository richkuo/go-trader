"""Tests for bear_pullback_st.py — Bear Pullback Short strategy."""

import numpy as np
import pandas as pd

from bear_pullback_st import bear_pullback_st_core


def make_ohlcv(closes, noise=0.5):
    closes = np.array(closes, dtype=float)
    n = len(closes)
    return pd.DataFrame({
        "open": closes - noise * 0.3,
        "high": closes + noise,
        "low": closes - noise,
        "close": closes,
        "volume": np.full(n, 100.0),
    })


def _bear_setup_with_rally_and_rejection():
    """Build a long downtrend, a rally bouncing above EMA20/50, then a rejection.

    The downtrend must be long enough for EMA200 to be above EMA50, ADX > 20,
    and the rally must lift RSI into the 55–65 zone before the trigger bar.
    """
    rng = np.random.default_rng(42)
    # 230 bars of downtrend from 200 → 110 with mild noise so ADX trends up.
    down = np.linspace(200.0, 110.0, 230) + rng.normal(0, 0.4, 230)
    # 10-bar rally up to 130 — overshoots the recent EMA20/50.
    rally = np.linspace(110.0, 132.0, 10)
    # Rejection: 4 bars closing back below the rally peak with progressively
    # lower lows so the trigger fires.
    reject = [128.0, 124.0, 119.0, 113.0]
    closes = np.concatenate([down, rally, reject])
    return make_ohlcv(closes)


def test_emits_short_on_failed_rally_in_bear_trend():
    df = _bear_setup_with_rally_and_rejection()
    result = bear_pullback_st_core(df)
    assert (result["signal"] == -1).any(), (
        "Expected at least one short signal on rejection bars after a bear-trend rally"
    )
    # Signals should be confined to the post-rally rejection window (last 5 bars).
    last_signals = result["signal"].iloc[-5:]
    assert (last_signals == -1).any(), (
        f"Short signal should land in the rejection window, got {last_signals.tolist()}"
    )


def test_no_long_signals_emitted():
    """Strategy is short-only — signal must never be +1."""
    df = _bear_setup_with_rally_and_rejection()
    result = bear_pullback_st_core(df)
    assert not (result["signal"] == 1).any(), "Strategy should never emit long signals"
    assert set(result["signal"].unique()).issubset({-1, 0})


def test_bullish_regime_blocks_shorts():
    """In an uptrend (EMA50 > EMA200) the regime filter must veto every short."""
    rng = np.random.default_rng(0)
    closes = np.linspace(100.0, 200.0, 250) + rng.normal(0, 0.4, 250)
    df = make_ohlcv(closes)
    result = bear_pullback_st_core(df)
    assert (result["signal"] == 0).all(), "Bullish regime must produce no short signals"


def test_short_data_returns_zero_signal_without_crash():
    df = make_ohlcv([100.0] * 50)
    result = bear_pullback_st_core(df)
    assert "signal" in result.columns
    assert (result["signal"] == 0).all()
    # Indicator columns are still attached so downstream consumers can inspect them.
    for col in ("ema_short", "ema_mid", "ema_long", "adx", "rsi"):
        assert col in result.columns


def test_signal_values_valid():
    df = _bear_setup_with_rally_and_rejection()
    result = bear_pullback_st_core(df)
    assert set(result["signal"].unique()).issubset({-1, 0, 1})


def test_indicator_columns_exposed():
    df = _bear_setup_with_rally_and_rejection()
    result = bear_pullback_st_core(df)
    for col in ("ema_short", "ema_mid", "ema_long", "adx", "rsi"):
        assert col in result.columns, f"{col} column missing from output"
