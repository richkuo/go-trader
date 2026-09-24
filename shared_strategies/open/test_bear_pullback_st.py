
import numpy as np

from shared_strategies.open.conftest import load_module, make_ohlcv

_BEAR_PULLBACK = load_module("_bear_pullback_st_test", __file__.replace("test_bear_pullback_st.py", "bear_pullback_st.py"))
bear_pullback_st_core = _BEAR_PULLBACK.bear_pullback_st_core


def _bear_setup_with_rally_and_rejection():
    rng = np.random.default_rng(42)
    down = np.linspace(200.0, 110.0, 230) + rng.normal(0, 0.4, 230)
    rally = np.linspace(110.0, 132.0, 10)
    reject = [128.0, 124.0, 119.0, 113.0]
    closes = np.concatenate([down, rally, reject])
    return make_ohlcv(closes)


def test_emits_short_on_failed_rally_in_bear_trend():
    df = _bear_setup_with_rally_and_rejection()
    result = bear_pullback_st_core(df)
    assert (result["signal"] == -1).any(), (
        "Expected at least one short signal on rejection bars after a bear-trend rally"
    )
    last_signals = result["signal"].iloc[-5:]
    assert (last_signals == -1).any(), (
        f"Short signal should land in the rejection window, got {last_signals.tolist()}"
    )


def test_no_long_signals_emitted():
    df = _bear_setup_with_rally_and_rejection()
    result = bear_pullback_st_core(df)
    assert not (result["signal"] == 1).any(), "Strategy should never emit long signals"
    assert set(result["signal"].unique()).issubset({-1, 0})


def test_bullish_regime_blocks_shorts():
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
    for col in ("ema_short", "ema_mid", "ema_long", "adx", "rsi"):
        assert col in result.columns


def test_shallow_rally_below_rsi_zone_blocks_short():
    rng = np.random.default_rng(7)
    down = np.linspace(200.0, 110.0, 230) + rng.normal(0, 0.4, 230)
    rally = np.linspace(110.0, 112.0, 6)
    reject = [111.5, 110.8, 109.5, 108.0]
    closes = np.concatenate([down, rally, reject])
    df = make_ohlcv(closes)
    result = bear_pullback_st_core(df)
    last = result.iloc[-1]
    assert last["ema_mid"] < last["ema_long"], "Setup must produce a bearish regime"
    assert last["adx"] > 20.0, "Setup must produce a strong trend"
    rsi_tail = result["rsi"].iloc[-20:]
    assert not ((rsi_tail >= 55.0) & (rsi_tail <= 65.0)).any(), (
        f"Shallow-rally setup unexpectedly hit the RSI zone: {rsi_tail.tolist()}"
    )
    assert (result["signal"] == 0).all(), (
        "Shallow rally must not produce any short signals"
    )


def test_buffer_rejects_wick_only_touch():
    rng = np.random.default_rng(13)
    down = np.linspace(200.0, 110.0, 230) + rng.normal(0, 0.05, 230)
    rally = np.linspace(110.0, 113.0, 6)
    reject = [111.5, 110.8, 109.5, 108.0]
    closes = np.concatenate([down, rally, reject])
    df = make_ohlcv(closes, noise=0.05)
    result = bear_pullback_st_core(df, pullback_touch_buffer_pct=0.01)
    assert (result["signal"] == 0).all(), (
        "Wick-only touch (no buffer overshoot) should not produce a short signal"
    )
