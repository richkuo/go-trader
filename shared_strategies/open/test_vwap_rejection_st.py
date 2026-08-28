
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
    rng = np.random.default_rng(42)
    down = np.linspace(200.0, 110.0, 230) + rng.normal(0, 0.4, 230)
    rally = np.linspace(110.0, 130.0, 5)
    reject_closes = [120.0, 113.0, 109.0, 106.0]
    closes = np.concatenate([down, rally, reject_closes])
    n = len(closes)
    idx = _hourly_index(n)
    df = make_ohlcv_from_closes(closes, idx, noise=0.4)
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
    df = _bear_setup_with_rally_and_rejection()
    result = vwap_rejection_st_core(df)
    assert not (result["signal"] == 1).any(), "Strategy should never emit long signals"
    assert set(result["signal"].unique()).issubset({-1, 0})


def test_bullish_regime_blocks_shorts():
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
    df = _bear_setup_with_rally_and_rejection()
    result = vwap_rejection_st_core(df, rsi_max_reclaim=0.0)
    assert (result["signal"] == 0).all(), (
        "RSI reclaim gate at 0 must veto every short"
    )


def test_buffer_rejects_wick_only_touch():
    df = _bear_setup_with_rally_and_rejection()
    result = vwap_rejection_st_core(df, rally_touch_buffer_pct=0.5)
    assert (result["signal"] == 0).all(), (
        "Buffer gate at 50% must veto every short — no high realistically overshoots that much"
    )


def test_signal_values_valid():
    df = _bear_setup_with_rally_and_rejection()
    result = vwap_rejection_st_core(df)
    assert set(result["signal"].unique()).issubset({-1, 0, 1})


def test_trigger_closes_below_every_rally_magnet():
    df = _bear_setup_with_rally_and_rejection()
    result = vwap_rejection_st_core(df)
    fired = result[result["signal"] == -1]
    assert len(fired) > 0, "Setup should produce at least one short signal"
    for ts, row in fired.iterrows():
        assert row["close"] < row["vwap"], (
            f"{ts}: close {row['close']} not below VWAP {row['vwap']}"
        )
        assert row["close"] < row["ema_short"], (
            f"{ts}: close {row['close']} not below EMA(short) {row['ema_short']}"
        )
        assert row["close"] < row["ema_mid"], (
            f"{ts}: close {row['close']} not below EMA(mid) {row['ema_mid']}"
        )
