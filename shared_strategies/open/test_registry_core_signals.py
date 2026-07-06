"""Concrete signal-value tests for registry.py strategies with no dedicated test file.

Issue #1237: these 13 strategies were previously exercised only by the
shape-only shim sweep in test_registry_parity.py (asserting a "signal"
column exists). Each test here asserts actual signal values — buy/sell
fires on a scenario constructed for it, and a flat market stays silent —
per the repo guardrail "strategy tests must assert actual signal values".

All strategies are loaded through the open registry so what is tested is
exactly what discoverStrategies / apply_strategy dispatches in production.
"""

import importlib.util
import os

import numpy as np
import pandas as pd
import pytest

from conftest import make_flat, make_ohlcv

_HERE = os.path.dirname(os.path.abspath(__file__))


def _load_registry():
    spec = importlib.util.spec_from_file_location(
        "_registry_core_signals", os.path.join(_HERE, "registry.py")
    )
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


@pytest.fixture(scope="module")
def registry():
    return _load_registry()


_FN = {"bollinger_bands": "bollinger_strategy"}


def _apply(registry, name, df, **params):
    fn = getattr(registry, _FN.get(name, name + "_strategy"))
    return fn(df, **params)


def _flat_df(n=200, price=100.0):
    return make_ohlcv(make_flat(n, price), noise=0)


def _dip_and_recover(n_flat=30, depth=20.0, dip_len=20, recover_len=30, start=100.0):
    """Flat, sell-off, then recovery — drives RSI/z-score oversold then back up."""
    flat = [start] * n_flat
    dip = list(np.linspace(start, start - depth, dip_len))
    recover = list(np.linspace(start - depth, start + depth / 2, recover_len))
    return np.array(flat + dip + recover)


def _rally_and_fade(n_flat=30, height=20.0, rally_len=20, fade_len=30, start=100.0):
    flat = [start] * n_flat
    rally = list(np.linspace(start, start + height, rally_len))
    fade = list(np.linspace(start + height, start - height / 2, fade_len))
    return np.array(flat + rally + fade)


def _down_then_up(down_n=60, up_n=80, start=150.0, low=100.0, high=200.0):
    return np.concatenate([np.linspace(start, low, down_n), np.linspace(low, high, up_n)])


def _up_then_down(up_n=60, down_n=80, start=100.0, high=150.0, low=60.0):
    return np.concatenate([np.linspace(start, high, up_n), np.linspace(high, low, down_n)])


# ─── ema_crossover ───────────────────────────────────────────────────────────


def test_ema_crossover_buy_on_bullish_cross(registry):
    df = make_ohlcv(_down_then_up())
    out = _apply(registry, "ema_crossover", df)
    buys = out.index[out["signal"] == 1]
    assert len(buys) >= 1, "Expected a buy when fast EMA crosses above slow EMA"
    i = buys[0]
    assert out.loc[i, "ema_fast"] > out.loc[i, "ema_slow"]
    prev = out.index.get_loc(i) - 1
    assert out.iloc[prev]["ema_fast"] <= out.iloc[prev]["ema_slow"], (
        "Buy must land exactly on the crossover bar"
    )


def test_ema_crossover_sell_on_bearish_cross(registry):
    df = make_ohlcv(_up_then_down())
    out = _apply(registry, "ema_crossover", df)
    assert (out["signal"] == -1).any(), "Expected a sell when fast EMA crosses below slow EMA"


def test_ema_crossover_flat_market_silent(registry):
    out = _apply(registry, "ema_crossover", _flat_df())
    assert (out["signal"].fillna(0) == 0).all()


# ─── macd ────────────────────────────────────────────────────────────────────


def test_macd_buy_on_bullish_cross(registry):
    df = make_ohlcv(_down_then_up())
    out = _apply(registry, "macd", df)
    buys = out.index[out["signal"] == 1]
    assert len(buys) >= 1, "Expected a buy on MACD line crossing above signal line"
    i = buys[0]
    assert out.loc[i, "macd_line"] > out.loc[i, "macd_signal"]


def test_macd_sell_on_bearish_cross(registry):
    df = make_ohlcv(_up_then_down())
    out = _apply(registry, "macd", df)
    sells = out.index[out["signal"] == -1]
    assert len(sells) >= 1, "Expected a sell on MACD line crossing below signal line"
    i = sells[0]
    assert out.loc[i, "macd_line"] < out.loc[i, "macd_signal"]


def test_macd_flat_market_silent(registry):
    out = _apply(registry, "macd", _flat_df())
    assert (out["signal"].fillna(0) == 0).all()


# ─── bollinger_bands ─────────────────────────────────────────────────────────


def test_bollinger_buy_on_reentry_from_below_lower_band(registry):
    df = make_ohlcv(_dip_and_recover())
    out = _apply(registry, "bollinger_bands", df)
    buys = out.index[out["signal"] == 1]
    assert len(buys) >= 1, "Expected a buy when close re-crosses above the lower band"
    i = buys[0]
    assert out.loc[i, "close"] > out.loc[i, "bb_lower"]


def test_bollinger_sell_on_reentry_from_above_upper_band(registry):
    df = make_ohlcv(_rally_and_fade())
    out = _apply(registry, "bollinger_bands", df)
    sells = out.index[out["signal"] == -1]
    assert len(sells) >= 1, "Expected a sell when close re-crosses below the upper band"
    i = sells[0]
    assert out.loc[i, "close"] < out.loc[i, "bb_upper"]


# ─── volume_weighted ─────────────────────────────────────────────────────────


def test_volume_weighted_buy_needs_price_cross_and_volume_spike(registry):
    prices = [100.0] * 40 + [104.0] * 10
    vol = [100.0] * 40 + [400.0] * 10  # spike on the breakout bars
    df = make_ohlcv(prices, volume=vol, noise=0.1)
    out = _apply(registry, "volume_weighted", df)
    assert out["signal"].iloc[40] == 1, (
        "Cross above price SMA on high volume must fire a buy on the cross bar"
    )
    # Same price path on flat volume must NOT fire.
    df_quiet = make_ohlcv(prices, noise=0.1)
    out_quiet = _apply(registry, "volume_weighted", df_quiet)
    assert (out_quiet["signal"] == 0).all(), "No volume confirmation → no signal"


def test_volume_weighted_sell_on_breakdown_with_volume(registry):
    prices = [100.0] * 40 + [96.0] * 10
    vol = [100.0] * 40 + [400.0] * 10
    df = make_ohlcv(prices, volume=vol, noise=0.1)
    out = _apply(registry, "volume_weighted", df)
    assert out["signal"].iloc[40] == -1


# ─── rsi_macd_combo ──────────────────────────────────────────────────────────


def test_rsi_macd_combo_buy_requires_macd_cross_with_low_rsi(registry):
    df = make_ohlcv(_dip_and_recover(n_flat=40, depth=30.0, dip_len=30, recover_len=40))
    out = _apply(registry, "rsi_macd_combo", df)
    buys = out.index[out["signal"] == 1]
    assert len(buys) >= 1, "Expected a buy: MACD bullish cross while RSI < rsi_long_max"
    i = buys[0]
    assert out.loc[i, "macd_line"] > out.loc[i, "macd_signal_line"]
    assert out.loc[i, "rsi"] < 50


def test_rsi_macd_combo_sell_requires_macd_cross_with_high_rsi(registry):
    df = make_ohlcv(_rally_and_fade(n_flat=40, height=30.0, rally_len=30, fade_len=40))
    out = _apply(registry, "rsi_macd_combo", df)
    sells = out.index[out["signal"] == -1]
    assert len(sells) >= 1, "Expected a sell: MACD bearish cross while RSI > rsi_short_min"
    i = sells[0]
    assert out.loc[i, "macd_line"] < out.loc[i, "macd_signal_line"]
    assert out.loc[i, "rsi"] > 50


def test_rsi_macd_combo_flat_market_silent(registry):
    out = _apply(registry, "rsi_macd_combo", _flat_df())
    assert (out["signal"].fillna(0) == 0).all()


# ─── stoch_rsi ───────────────────────────────────────────────────────────────


def _stoch_rsi_cycle_df():
    # A clean price cycle swings the RSI (and thus StochRSI) through both
    # extremes repeatedly, producing %K/%D crosses inside the OB/OS zones.
    t = np.arange(250)
    return make_ohlcv(100 + 4 * np.sin(2 * np.pi * t / 20), noise=0.05)


def test_stoch_rsi_buy_on_k_cross_up_in_oversold(registry):
    out = _apply(registry, "stoch_rsi", _stoch_rsi_cycle_df())
    buys = out.index[out["signal"] == 1]
    assert len(buys) >= 1, "Expected a buy on %K crossing above %D below the oversold line"
    for i in buys:
        assert out.loc[i, "stoch_k"] < 20
        assert out.loc[i, "stoch_k"] > out.loc[i, "stoch_d"]


def test_stoch_rsi_sell_on_k_cross_down_in_overbought(registry):
    out = _apply(registry, "stoch_rsi", _stoch_rsi_cycle_df())
    sells = out.index[out["signal"] == -1]
    assert len(sells) >= 1, "Expected a sell on %K crossing below %D above the overbought line"
    for i in sells:
        assert out.loc[i, "stoch_k"] > 80
        assert out.loc[i, "stoch_k"] < out.loc[i, "stoch_d"]


# ─── atr_breakout ────────────────────────────────────────────────────────────


def test_atr_breakout_buy_on_upside_gap(registry):
    prices = [100.0] * 40 + [104.0] + [104.0] * 5  # jump ≫ 1.5×ATR of the quiet range
    df = make_ohlcv(prices, noise=0.2)
    out = _apply(registry, "atr_breakout", df)
    assert out["signal"].iloc[40] == 1, "Close beyond prev_close + 1.5×ATR must fire a buy"
    assert (out["signal"].iloc[:40] == 0).all(), "Quiet range must stay silent"


def test_atr_breakout_sell_on_downside_gap(registry):
    prices = [100.0] * 40 + [96.0] + [96.0] * 5
    df = make_ohlcv(prices, noise=0.2)
    out = _apply(registry, "atr_breakout", df)
    assert out["signal"].iloc[40] == -1, "Close beyond prev_close - 1.5×ATR must fire a sell"


# ─── heikin_ashi_ema ─────────────────────────────────────────────────────────


def test_heikin_ashi_ema_buy_in_clean_uptrend(registry):
    # Strong monotone up-move: HA candles turn solid bullish (no lower wick)
    # and HA close sits above the EMA.
    closes = np.linspace(100, 200, 120)
    df = pd.DataFrame({
        "open": closes - 1.0,
        "high": closes + 0.1,
        "low": closes - 1.1,
        "close": closes,
        "volume": np.full(120, 100.0),
    })
    out = _apply(registry, "heikin_ashi_ema", df)
    buys = out.index[out["signal"] == 1]
    assert len(buys) >= 1, "Expected a buy after 2 consecutive bullish HA candles above EMA"
    i = buys[0]
    assert out.loc[i, "ha_close"] > out.loc[i, "ha_ema"]


def test_heikin_ashi_ema_sell_in_clean_downtrend(registry):
    closes = np.linspace(200, 100, 120)
    df = pd.DataFrame({
        "open": closes + 1.0,
        "high": closes + 1.1,
        "low": closes - 0.1,
        "close": closes,
        "volume": np.full(120, 100.0),
    })
    out = _apply(registry, "heikin_ashi_ema", df)
    sells = out.index[out["signal"] == -1]
    assert len(sells) >= 1, "Expected a sell after 2 consecutive bearish HA candles below EMA"
    i = sells[0]
    assert out.loc[i, "ha_close"] < out.loc[i, "ha_ema"]


# ─── ichimoku_cloud ──────────────────────────────────────────────────────────


def test_ichimoku_buy_on_tk_cross_above_cloud(registry):
    # Long basing period then a strong sustained rally: price rises above the
    # (lagging) cloud, Tenkan crosses Kijun upward, Chikou above price 26 back.
    prices = np.concatenate([
        np.linspace(110, 100, 80),          # gentle downtrend (bearish TK stack)
        np.linspace(100, 180, 120),         # strong rally
    ])
    df = make_ohlcv(prices, noise=0.5)
    out = _apply(registry, "ichimoku_cloud", df)
    buys = out.index[out["signal"] == 1]
    assert len(buys) >= 1, "Expected a buy: TK cross up, close above cloud, Chikou bullish"
    i = buys[0]
    assert out.loc[i, "tenkan"] > out.loc[i, "kijun"]
    assert out.loc[i, "close"] > max(out.loc[i, "senkou_a"], out.loc[i, "senkou_b"])


def test_ichimoku_sell_on_tk_cross_below_cloud(registry):
    prices = np.concatenate([
        np.linspace(100, 110, 80),
        np.linspace(110, 30, 120),
    ])
    df = make_ohlcv(prices, noise=0.5)
    out = _apply(registry, "ichimoku_cloud", df)
    sells = out.index[out["signal"] == -1]
    assert len(sells) >= 1, "Expected a sell: TK cross down, close below cloud, Chikou bearish"
    i = sells[0]
    assert out.loc[i, "tenkan"] < out.loc[i, "kijun"]
    assert out.loc[i, "close"] < min(out.loc[i, "senkou_a"], out.loc[i, "senkou_b"])


# ─── squeeze_momentum ────────────────────────────────────────────────────────


def _squeeze_then_breakout(direction=1):
    """Tight low-vol coil (BB inside KC) followed by an expansion move."""
    rng = np.random.RandomState(7)
    coil = 100.0 + rng.randn(80) * 0.05                     # very tight range
    burst = 100.0 + direction * np.linspace(0.5, 25.0, 40)  # vol expansion
    closes = np.concatenate([coil, burst])
    n = len(closes)
    # Wide highs/lows during the coil keep the ATR (KC width) large relative
    # to the tiny close-stddev (BB width) → squeeze_on during the coil.
    noise = np.concatenate([np.full(80, 2.0), np.full(40, 2.0)])
    return pd.DataFrame({
        "open": closes,
        "high": closes + noise,
        "low": closes - noise,
        "close": closes,
        "volume": np.full(n, 100.0),
    })


def test_squeeze_momentum_buy_on_upside_release(registry):
    df = _squeeze_then_breakout(direction=1)
    out = _apply(registry, "squeeze_momentum", df)
    assert out["squeeze_on"].iloc[30:80].any(), "Coil must register as a squeeze"
    buys = out.index[out["signal"] == 1]
    assert len(buys) >= 1, "Expected a buy when the squeeze releases with rising momentum"
    i = buys[0]
    assert out.loc[i, "squeeze_mom"] > 0


def test_squeeze_momentum_sell_on_downside_release(registry):
    df = _squeeze_then_breakout(direction=-1)
    out = _apply(registry, "squeeze_momentum", df)
    sells = out.index[out["signal"] == -1]
    assert len(sells) >= 1, "Expected a sell when the squeeze releases with falling momentum"
    i = sells[0]
    assert out.loc[i, "squeeze_mom"] < 0


# ─── order_blocks ────────────────────────────────────────────────────────────


def test_order_blocks_buy_on_retest_of_bullish_ob(registry):
    """Down candle, bullish displacement, then a retest of the OB zone → buy."""
    n = 60
    opens = np.full(n, 100.0)
    closes = np.full(n, 100.0)
    highs = np.full(n, 100.6)
    lows = np.full(n, 99.4)
    # Bar 30: last down candle (the order block: high 100.6, low 99.4)
    opens[30], closes[30] = 100.5, 99.5
    # Bar 31: bullish displacement (body ≫ 1.5×ATR of the quiet range),
    # gapped above the OB zone so the displacement bar does not touch its own OB
    opens[31], closes[31], highs[31], lows[31] = 100.7, 106.0, 106.2, 100.65
    # Bars 32-35: hold above the zone
    for j in range(32, 36):
        opens[j] = closes[j] = 106.0
        highs[j], lows[j] = 106.4, 105.6
    # Bar 36: retest dips into the OB zone (low <= ob_high=100.6) and holds
    opens[36], closes[36], highs[36], lows[36] = 106.0, 105.0, 106.0, 100.4
    for j in range(37, n):
        opens[j] = closes[j] = 105.0
        highs[j], lows[j] = 105.4, 104.6
    df = pd.DataFrame({
        "open": opens, "high": highs, "low": lows, "close": closes,
        "volume": np.full(n, 100.0),
    })
    out = _apply(registry, "order_blocks", df)
    assert out["signal"].iloc[36] == 1, "Retest of the bullish order block must fire a buy"
    assert (out["signal"].iloc[:36] == 0).all(), "No signal before the retest"


def test_order_blocks_sell_on_retest_of_bearish_ob(registry):
    n = 60
    opens = np.full(n, 100.0)
    closes = np.full(n, 100.0)
    highs = np.full(n, 100.6)
    lows = np.full(n, 99.4)
    opens[30], closes[30] = 99.5, 100.5           # last up candle = bearish OB
    opens[31], closes[31], highs[31], lows[31] = 99.3, 94.0, 99.35, 93.8
    for j in range(32, 36):
        opens[j] = closes[j] = 94.0
        highs[j], lows[j] = 94.4, 93.6
    opens[36], closes[36], highs[36], lows[36] = 94.0, 95.0, 99.6, 94.0  # retest high >= ob_low
    for j in range(37, n):
        opens[j] = closes[j] = 95.0
        highs[j], lows[j] = 95.4, 94.6
    df = pd.DataFrame({
        "open": opens, "high": highs, "low": lows, "close": closes,
        "volume": np.full(n, 100.0),
    })
    out = _apply(registry, "order_blocks", df)
    assert out["signal"].iloc[36] == -1, "Retest of the bearish order block must fire a sell"


# ─── parabolic_sar ───────────────────────────────────────────────────────────


def test_parabolic_sar_reversals_fire_on_trend_flips(registry):
    prices = _up_then_down(up_n=80, down_n=80, start=100.0, high=180.0, low=60.0)
    df = make_ohlcv(prices, noise=1.0)
    out = _apply(registry, "parabolic_sar", df)
    sells = out.index[out["signal"] == -1]
    assert len(sells) >= 1, "SAR must flip short when the downtrend pierces the trailing stop"
    # The first reversal must come after the peak (bar 80), not during the rally.
    assert out.index.get_loc(sells[0]) > 60
    # And a fresh uptrend after the trough flips it back long.
    prices2 = np.concatenate([prices, np.linspace(60.0, 140.0, 80)])
    out2 = _apply(registry, "parabolic_sar", make_ohlcv(prices2, noise=1.0))
    buys2 = out2.index[out2["signal"] == 1]
    assert len(buys2) >= 1, "SAR must flip long again in the recovery leg"
    assert out2.index.get_loc(buys2[0]) > 100


def test_parabolic_sar_no_reversal_in_monotone_trend(registry):
    prices = np.linspace(100, 200, 120)
    df = make_ohlcv(prices, noise=0.2)
    out = _apply(registry, "parabolic_sar", df)
    assert (out["signal"] != -1).all(), "A clean monotone uptrend must never flip short"


# ─── sweep_squeeze_combo ─────────────────────────────────────────────────────


def test_sweep_squeeze_combo_flat_market_silent(registry):
    out = _apply(registry, "sweep_squeeze_combo", _flat_df(250))
    assert (out["signal"].fillna(0) == 0).all()


def test_sweep_squeeze_combo_consensus_gates_signal(registry):
    """min_agree=3 must be at least as strict as min_agree=1 on the same data."""
    rng = np.random.RandomState(3)
    prices = 100 + 10 * np.sin(np.linspace(0, 12 * np.pi, 400)) + rng.randn(400) * 0.8
    idx = pd.date_range("2024-01-01", periods=400, freq="15min")
    df = make_ohlcv(prices, noise=1.0, index=idx)
    loose = _apply(registry, "sweep_squeeze_combo", df, min_agree=1)
    strict = _apply(registry, "sweep_squeeze_combo", df, min_agree=3)
    n_loose = int((loose["signal"] != 0).sum())
    n_strict = int((strict["signal"] != 0).sum())
    assert n_loose >= 1, "min_agree=1 should surface at least one component signal here"
    assert n_strict <= n_loose, "Raising the consensus bar must never add signals"
    # Every strict signal must also be a loose signal with the same direction.
    fired = strict["signal"] != 0
    assert (strict.loc[fired, "signal"] == loose.loc[fired, "signal"]).all()
