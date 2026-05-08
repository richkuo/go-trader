"""Tests for vwap_rejection_st.py — VWAP Rejection Short strategy."""

import numpy as np
import pandas as pd

from vwap_rejection_st import vwap_rejection_st_core


def make_ohlc(opens, highs, lows, closes, index, volume=100.0):
    n = len(closes)
    return pd.DataFrame(
        {
            "open": np.asarray(opens, dtype=float),
            "high": np.asarray(highs, dtype=float),
            "low": np.asarray(lows, dtype=float),
            "close": np.asarray(closes, dtype=float),
            "volume": np.full(n, float(volume)),
        },
        index=index,
    )


def make_ohlcv_from_closes(closes, index, noise=0.5):
    closes = np.asarray(closes, dtype=float)
    n = len(closes)
    opens = closes - noise * 0.3
    highs = closes + noise
    lows = closes - noise
    return make_ohlc(opens, highs, lows, closes, index)


def _hourly_index(n: int, start: str = "2026-01-01 00:00:00") -> pd.DatetimeIndex:
    return pd.date_range(start, periods=n, freq="1h")


def _bear_setup_with_rally_and_rejection():
    """Long multi-day hourly downtrend, then a same-day rally + rejection.

    The rally is *contained inside the final session* so the daily-anchored
    VWAP starts fresh near the rally's lows and gets pierced by the rally
    high — this is the level the strategy expects price to reject from.
    """
    rng = np.random.default_rng(42)
    # 230 hourly bars (~10 days) trending 200 → 110 — long enough for
    # EMA200 to sit well above EMA50.
    down = np.linspace(200.0, 110.0, 230) + rng.normal(0, 0.4, 230)
    # 5-bar rally up to ~130 — overshoots EMA20/50 and pierces same-day VWAP.
    rally = np.linspace(110.0, 130.0, 5)
    # Rejection bars — engineered as red candles closing back below VWAP/EMA20.
    reject_closes = [120.0, 113.0, 109.0, 106.0]
    closes = np.concatenate([down, rally, reject_closes])
    n = len(closes)
    idx = _hourly_index(n)
    df = make_ohlcv_from_closes(closes, idx, noise=0.4)
    # Force the last 4 bars into clean red candles (open above close) so the
    # rejection-trigger gate (close < open) actually fires.
    for i, close_px in enumerate(reject_closes, start=len(down) + len(rally)):
        prev_close = closes[i - 1]
        df.iat[i, df.columns.get_loc("open")] = prev_close + 0.5
        df.iat[i, df.columns.get_loc("high")] = prev_close + 1.0
        df.iat[i, df.columns.get_loc("low")] = close_px - 1.0
        df.iat[i, df.columns.get_loc("close")] = close_px
    return df


def test_emits_short_on_vwap_rejection_in_bear_trend():
    df = _bear_setup_with_rally_and_rejection()
    result = vwap_rejection_st_core(df)
    assert (result["signal"] == -1).any(), (
        "Expected at least one short signal on rejection bars after a VWAP/EMA rally"
    )
    last_signals = result["signal"].iloc[-5:]
    assert (last_signals == -1).any(), (
        f"Short signal should land in the rejection window, got {last_signals.tolist()}"
    )


def test_no_long_signals_emitted():
    """Strategy is short-only — signal must never be +1."""
    df = _bear_setup_with_rally_and_rejection()
    result = vwap_rejection_st_core(df)
    assert not (result["signal"] == 1).any(), "Strategy should never emit long signals"
    assert set(result["signal"].unique()).issubset({-1, 0})


def test_bullish_regime_blocks_shorts():
    """In an uptrend (EMA50 > EMA200) the regime filter must veto every short."""
    rng = np.random.default_rng(0)
    closes = np.linspace(100.0, 200.0, 250) + rng.normal(0, 0.4, 250)
    idx = _hourly_index(len(closes))
    df = make_ohlcv_from_closes(closes, idx)
    result = vwap_rejection_st_core(df)
    assert (result["signal"] == 0).all(), "Bullish regime must produce no short signals"


def test_short_data_returns_zero_signal_without_crash():
    idx = _hourly_index(50)
    df = make_ohlcv_from_closes([100.0] * 50, idx)
    result = vwap_rejection_st_core(df)
    assert "signal" in result.columns
    assert (result["signal"] == 0).all()
    for col in ("ema_short", "ema_mid", "ema_long", "vwap", "rsi"):
        assert col in result.columns


def test_indicator_columns_exposed():
    df = _bear_setup_with_rally_and_rejection()
    result = vwap_rejection_st_core(df)
    for col in ("ema_short", "ema_mid", "ema_long", "vwap", "rsi"):
        assert col in result.columns, f"{col} column missing from output"


def test_rsi_reclaim_above_50_blocks_short():
    """If RSI is forced above the reclaim cap, every trigger must be vetoed.

    The RSI gate is the momentum filter — even with a textbook rally + red
    rejection candle, RSI > rsi_max_reclaim says momentum has flipped and the
    short setup is no longer valid.
    """
    df = _bear_setup_with_rally_and_rejection()
    # Drop the cap to a value the trigger-bar RSI cannot satisfy.
    result = vwap_rejection_st_core(df, rsi_max_reclaim=0.0)
    assert (result["signal"] == 0).all(), (
        "RSI reclaim gate at 0 must veto every short"
    )


def test_buffer_rejects_wick_only_touch():
    """A high that merely *grazes* the resistance levels without exceeding the
    buffer must not count as a rally touch.

    Construct a tame setup where the rally bar's high sits very close to —
    but never meaningfully above — VWAP/EMA(short). With a fat 50% buffer
    nothing realistic can satisfy the touch gate, so no short signal can fire.
    """
    df = _bear_setup_with_rally_and_rejection()
    # 50% buffer is far wider than any high-vs-EMA gap in the synthetic setup,
    # so every touch is filtered out — proves the buffer gate is wired up.
    result = vwap_rejection_st_core(df, rally_touch_buffer_pct=0.5)
    assert (result["signal"] == 0).all(), (
        "Buffer gate at 50% must veto every short — no high realistically overshoots that much"
    )


def test_signal_values_valid():
    df = _bear_setup_with_rally_and_rejection()
    result = vwap_rejection_st_core(df)
    assert set(result["signal"].unique()).issubset({-1, 0, 1})
