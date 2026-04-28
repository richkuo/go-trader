"""Tests for futures/strategies.py — all registered futures strategies."""

import importlib.util
import numpy as np
import pandas as pd
import pytest

import sys, os

# Import futures strategies module explicitly to avoid collision with spot/strategies.py
_futures_dir = os.path.dirname(os.path.abspath(__file__))
_spot_dir = os.path.join(_futures_dir, '..', 'spot')
_shared_dir = os.path.join(_futures_dir, '..')

# Add spot and shared to path for indicators/amd_ifvg/chart_patterns imports
sys.path.insert(0, _spot_dir)
sys.path.insert(0, _shared_dir)

# Load the futures strategies module by file path to avoid name collision
_spec = importlib.util.spec_from_file_location(
    "futures_strategies", os.path.join(_futures_dir, "strategies.py"))
_mod = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(_mod)

STRATEGY_REGISTRY = _mod.STRATEGY_REGISTRY
apply_strategy = _mod.apply_strategy
list_strategies = _mod.list_strategies
get_strategy = _mod.get_strategy

# Load conftest helpers via file path so imports work regardless of CWD
_conftest_spec = importlib.util.spec_from_file_location(
    "conftest_helpers", os.path.join(_shared_dir, "conftest.py"))
_conftest_mod = importlib.util.module_from_spec(_conftest_spec)
_conftest_spec.loader.exec_module(_conftest_mod)

make_ohlcv = _conftest_mod.make_ohlcv
make_trending_up = _conftest_mod.make_trending_up
make_trending_down = _conftest_mod.make_trending_down
make_flat = _conftest_mod.make_flat
make_volatile = _conftest_mod.make_volatile


# ─── Registry ───────────────────────────────

class TestFuturesRegistry:
    def test_strategies_registered(self):
        names = list_strategies()
        assert len(names) >= 22
        for expected in ["sma_crossover", "ema_crossover", "bollinger_bands",
                         "volume_weighted", "triple_ema", "triple_ema_bd",
                         "rsi_macd_combo",
                         "momentum", "mean_reversion", "rsi", "macd", "breakout",
                         "stoch_rsi", "supertrend", "squeeze_momentum",
                         "ichimoku_cloud", "atr_breakout", "heikin_ashi_ema",
                         "order_blocks", "parabolic_sar", "delta_neutral_funding"]:
            assert expected in names, f"{expected} not registered"

    def test_get_unknown_strategy_raises(self):
        with pytest.raises(ValueError, match="Unknown strategy"):
            get_strategy("nonexistent_xyz")

    def test_apply_returns_dataframe(self):
        df = make_ohlcv(make_trending_up(100))
        result = apply_strategy("momentum", df)
        assert isinstance(result, pd.DataFrame)
        assert "signal" in result.columns


# ─── Helpers ────────────────────────────────

def _run(name, closes, params=None, volume=None, index=None):
    df = make_ohlcv(closes, volume=volume, index=index)
    return apply_strategy(name, df, params)


def _valid_signals(result):
    signals = result["signal"].dropna()
    assert set(signals.unique()).issubset({-1.0, 0.0, 1.0})


# ─── SMA Crossover ─────────────────────────

class TestSMACrossover:
    def test_buy_signal(self):
        # Trending up should produce buy signal when fast crosses above slow
        closes = list(np.linspace(120, 80, 60)) + list(np.linspace(80, 140, 60))
        result = _run("sma_crossover", closes)
        _valid_signals(result)
        assert "sma_fast" in result.columns
        assert "sma_slow" in result.columns
        assert (result["signal"] == 1).any()

    def test_flat_no_signal(self):
        result = _run("sma_crossover", make_flat(80))
        assert (result["signal"].dropna() == 0).all()


# ─── EMA Crossover ─────────────────────────

class TestEMACrossover:
    def test_buy_signal(self):
        closes = list(np.linspace(120, 80, 50)) + list(np.linspace(80, 140, 50))
        result = _run("ema_crossover", closes)
        _valid_signals(result)
        assert "ema_fast" in result.columns
        assert "ema_slow" in result.columns
        assert (result["signal"] == 1).any()

    def test_flat_no_signal(self):
        result = _run("ema_crossover", make_flat(80))
        assert (result["signal"].dropna() == 0).all()


# ─── Bollinger Bands ───────────────────────

class TestBollingerBands:
    def test_buy_signal(self):
        # Flat → sharp dip → recovery forces price below lower band then cross back up
        closes = list(np.full(30, 100.0)) + list(np.linspace(100, 80, 10)) + list(np.linspace(80, 105, 10))
        result = _run("bollinger_bands", closes)
        assert "bb_middle" in result.columns
        assert "bb_upper" in result.columns
        assert "bb_lower" in result.columns
        _valid_signals(result)
        assert (result["signal"] == 1).any()

    def test_flat_no_signal(self):
        result = _run("bollinger_bands", make_flat(40))
        assert (result["signal"] == 0).all()


# ─── Volume Weighted ──────────────────────

class TestVolumeWeighted:
    def test_buy_signal(self):
        # Down → up crossover with volume spike at the crossover point
        closes = list(np.linspace(120, 80, 30)) + list(np.linspace(80, 130, 30))
        volume = [100.0] * 60
        volume[35] = 500.0  # spike volume at the crossover bar
        volume[36] = 500.0
        result = _run("volume_weighted", closes, volume=volume)
        assert "price_sma" in result.columns
        assert "vol_sma" in result.columns
        _valid_signals(result)
        assert (result["signal"] == 1).any()

    def test_flat_no_signal(self):
        result = _run("volume_weighted", make_flat(40))
        assert (result["signal"] == 0).all()


# ─── Triple EMA ───────────────────────────

class TestTripleEMA:
    def test_buy_signal(self):
        closes = make_trending_up(80)
        result = _run("triple_ema", closes)
        assert "ema_short" in result.columns
        assert "ema_mid" in result.columns
        assert "ema_long" in result.columns
        _valid_signals(result)
        assert (result["signal"] == 1).any()

    def test_flat_no_signal(self):
        result = _run("triple_ema", make_flat(80))
        assert (result["signal"].dropna() == 0).all()


# ─── Triple EMA Bidirectional ─────────────

class TestTripleEMABd:
    def test_uptrend_enters_long(self):
        result = _run("triple_ema_bd", make_trending_up(120))
        assert "ema_short" in result.columns
        _valid_signals(result)
        assert (result["position"] == 1).any()
        assert (result["signal"] == 1).any()

    def test_downtrend_enters_short(self):
        result = _run("triple_ema_bd", make_trending_down(120))
        _valid_signals(result)
        assert (result["position"] == -1).any(), "bearish stack must emit position=-1"
        assert (result["signal"] == -1).any(), "bearish stack must emit short-entry signal"

    def test_flat_no_signal(self):
        result = _run("triple_ema_bd", make_flat(80))
        assert (result["position"].dropna() == 0).all()
        assert (result["signal"].dropna() == 0).all()

    def test_direct_flip_signal_clamped(self):
        # Strong uptrend then strong downtrend — EMAs should flip from bullish to bearish
        # stack with no flat-stack bars in between. The raw diff would be -2 on the flip
        # bar; the strategy must clamp it to -1.
        closes = list(make_trending_up(80, start=100, step=1.0)) + list(
            make_trending_down(80, start=180, step=1.0)
        )
        result = _run("triple_ema_bd", closes)
        _valid_signals(result)
        assert result["signal"].min() >= -1
        assert result["signal"].max() <= 1

    def test_custom_params_applied(self):
        # Override periods — strategy must accept and use them (checked indirectly via
        # ema columns differing from defaults on same input).
        closes = make_trending_up(60)
        default = _run("triple_ema_bd", closes)
        custom = _run("triple_ema_bd", closes,
                      params={"short_period": 3, "mid_period": 10, "long_period": 30})
        assert not default["ema_short"].equals(custom["ema_short"])


# ─── RSI MACD Combo ──────────────────────

class TestRSIMACDCombo:
    def test_buy_signal(self):
        closes = list(np.linspace(120, 80, 50)) + list(np.linspace(80, 140, 50))
        result = _run("rsi_macd_combo", closes)
        assert "rsi" in result.columns
        assert "macd_line" in result.columns
        assert "macd_signal_line" in result.columns
        _valid_signals(result)
        assert (result["signal"] == 1).any() or (result["signal"] == -1).any()

    def test_default_gate_preserves_legacy_behavior(self):
        # The default rsi_short_min / rsi_long_max values (50) must match the
        # pre-PR hard-coded gates. A straight down-then-up series should still
        # produce at least one -1 signal with defaults.
        closes = list(np.linspace(80, 140, 60)) + list(np.linspace(140, 80, 60))
        result = _run("rsi_macd_combo", closes)
        _valid_signals(result)
        assert (result["signal"] == -1).any()

    def test_loosened_short_gate_catches_more_shorts(self):
        # Series: rally → sharp drop (first MACD bearish cross at high RSI) → small
        # bounce → extended drop (second bearish cross, now at low RSI). Default
        # gate (rsi_short_min=50) blocks the second cross; permissive gate catches both.
        closes = (list(np.linspace(80, 160, 50)) +
                  list(np.linspace(160, 100, 20)) +
                  list(np.linspace(100, 115, 15)) +
                  list(np.linspace(115, 60, 40)))
        strict = _run("rsi_macd_combo", closes)
        loose = _run("rsi_macd_combo", closes, params={"rsi_short_min": 0})
        _valid_signals(strict)
        _valid_signals(loose)
        assert (loose["signal"] == -1).sum() > (strict["signal"] == -1).sum(), \
            "loosening rsi_short_min must allow more short signals"

    def test_loosened_long_gate_catches_more_longs(self):
        # Symmetric pattern: crash → sharp rally (first cross at low RSI) → dip →
        # extended rally (second bullish cross at higher RSI). Permissive gate
        # (rsi_long_max=100) should catch at least as many longs as default.
        closes = (list(np.linspace(140, 70, 50)) +
                  list(np.linspace(70, 130, 20)) +
                  list(np.linspace(130, 115, 15)) +
                  list(np.linspace(115, 180, 40)))
        strict = _run("rsi_macd_combo", closes)
        loose = _run("rsi_macd_combo", closes, params={"rsi_long_max": 100})
        _valid_signals(strict)
        _valid_signals(loose)
        assert (loose["signal"] == 1).sum() >= (strict["signal"] == 1).sum()

    def test_params_forwarded_via_apply_strategy(self):
        # Sanity: the new params propagate through apply_strategy without error.
        closes = make_trending_down(80)
        result = _run("rsi_macd_combo", closes,
                      params={"rsi_short_min": 30, "rsi_long_max": 70})
        assert "rsi" in result.columns
        _valid_signals(result)

    def test_flat_no_signal(self):
        result = _run("rsi_macd_combo", make_flat(80))
        assert (result["signal"] == 0).all()


# ─── Momentum ───────────────────────────────

class TestMomentum:
    def test_buy_signal(self):
        closes = list(np.linspace(100, 100, 30)) + list(np.linspace(100, 120, 20))
        result = _run("momentum", closes, {"roc_period": 14, "threshold": 3.0})
        _valid_signals(result)
        assert "roc" in result.columns
        assert (result["signal"] == 1).any()

    def test_sell_signal(self):
        closes = list(np.linspace(100, 100, 30)) + list(np.linspace(100, 80, 20))
        result = _run("momentum", closes, {"roc_period": 14, "threshold": 3.0})
        _valid_signals(result)
        assert (result["signal"] == -1).any()

    def test_flat_no_signal(self):
        result = _run("momentum", make_flat(60))
        assert (result["signal"] == 0).all()


# ─── Mean Reversion ─────────────────────────

class TestMeanReversion:
    def test_buy_on_dip(self):
        closes = (
            list(np.linspace(100, 100, 40)) +
            list(np.linspace(100, 80, 10)) +
            list(np.linspace(80, 95, 20))
        )
        result = _run("mean_reversion", closes)
        _valid_signals(result)
        assert "z_score" in result.columns

    def test_flat_no_signal(self):
        result = _run("mean_reversion", make_flat(60))
        assert (result["signal"] == 0).all()


# ─── RSI ────────────────────────────────────

class TestRSI:
    def test_produces_rsi_column(self):
        closes = make_volatile(80, amplitude=10)
        result = _run("rsi", closes)
        assert "rsi" in result.columns
        valid = result["rsi"].dropna()
        assert (valid >= 0).all() and (valid <= 100).all()

    def test_flat_no_signal(self):
        result = _run("rsi", make_flat(50))
        assert (result["signal"] == 0).all()


# ─── MACD ───────────────────────────────────

class TestMACD:
    def test_bullish_cross(self):
        closes = list(np.linspace(120, 80, 50)) + list(np.linspace(80, 140, 50))
        result = _run("macd", closes)
        _valid_signals(result)
        assert (result["signal"] == 1).any()
        assert "macd_line" in result.columns

    def test_bearish_cross(self):
        closes = list(np.linspace(80, 140, 50)) + list(np.linspace(140, 70, 50))
        result = _run("macd", closes)
        assert (result["signal"] == -1).any()


# ─── Breakout ───────────────────────────────

class TestBreakout:
    def test_upside_breakout(self):
        # Range-bound then break above
        closes = list(np.linspace(100, 105, 30)) + [120, 122, 125, 128, 130]
        result = _run("breakout", closes)
        _valid_signals(result)
        assert "high_roll" in result.columns
        assert "atr" in result.columns

    def test_flat_no_breakout(self):
        result = _run("breakout", make_flat(40))
        assert (result["signal"] == 0).all()


# ─── Stochastic RSI ─────────────────────────

class TestStochRSI:
    def test_columns(self):
        closes = make_volatile(80, amplitude=10)
        result = _run("stoch_rsi", closes)
        assert "stoch_k" in result.columns
        assert "stoch_d" in result.columns
        _valid_signals(result)

    def test_flat_data(self):
        result = _run("stoch_rsi", make_flat(80))
        assert "signal" in result.columns


# ─── Supertrend ─────────────────────────────

class TestSupertrend:
    def test_output_columns(self):
        closes = list(np.linspace(120, 80, 40)) + list(np.linspace(80, 150, 60))
        result = _run("supertrend", closes)
        _valid_signals(result)
        assert "supertrend" in result.columns
        assert "st_direction" in result.columns

    def test_direction_computed(self):
        closes = make_trending_down(100, start=200, step=1.0)
        result = _run("supertrend", closes)
        dirs = result["st_direction"]
        assert set(dirs.unique()).issubset({-1, 0, 1})


# ─── Squeeze Momentum ──────────────────────

class TestSqueezeMomentum:
    def test_columns(self):
        closes = make_volatile(100, amplitude=10)
        result = _run("squeeze_momentum", closes)
        assert "squeeze_on" in result.columns
        assert "squeeze_mom" in result.columns
        _valid_signals(result)

    def test_flat_no_signal(self):
        result = _run("squeeze_momentum", make_flat(60))
        assert (result["signal"] == 0).all()


# ─── Ichimoku Cloud ─────────────────────────

class TestIchimokuCloud:
    def test_columns(self):
        closes = make_trending_up(120)
        result = _run("ichimoku_cloud", closes)
        for col in ["tenkan", "kijun", "senkou_a", "senkou_b"]:
            assert col in result.columns

    def test_short_data(self):
        result = _run("ichimoku_cloud", [100.0] * 20)
        assert (result["signal"] == 0).all()


# ─── ATR Breakout ───────────────────────────

class TestATRBreakout:
    def test_upside_breakout(self):
        closes = list(np.linspace(100, 100, 30)) + list(np.linspace(100, 130, 10))
        result = _run("atr_breakout", closes, {"atr_period": 14, "multiplier": 1.0})
        _valid_signals(result)

    def test_flat_no_breakout(self):
        result = _run("atr_breakout", make_flat(50))
        assert (result["signal"] == 0).all()


# ─── Heikin Ashi EMA ────────────────────────

class TestHeikinAshiEMA:
    def test_columns(self):
        closes = make_trending_up(80)
        result = _run("heikin_ashi_ema", closes)
        for col in ["ha_open", "ha_close", "ha_high", "ha_low", "ha_ema"]:
            assert col in result.columns
        _valid_signals(result)


# ─── Order Blocks ───────────────────────────

class TestOrderBlocks:
    def test_flat_no_signal(self):
        result = _run("order_blocks", make_flat(80))
        assert (result["signal"] == 0).all()

    def test_no_crash_volatile(self):
        closes = make_volatile(100, amplitude=15)
        result = _run("order_blocks", closes)
        _valid_signals(result)


# ─── VWAP Reversion ─────────────────────────

class TestVWAPReversion:
    def test_with_datetime_index(self):
        n = 100
        closes = make_volatile(n, center=100, amplitude=8)
        idx = pd.date_range("2024-01-01", periods=n, freq="h")
        result = _run("vwap_reversion", closes, index=idx)
        assert "vwap" in result.columns
        _valid_signals(result)


# ─── Parabolic SAR ──────────────────────────

class TestParabolicSAR:
    def test_buy_signal(self):
        closes = list(np.linspace(120, 80, 40)) + list(np.linspace(80, 140, 60))
        result = _run("parabolic_sar", closes)
        _valid_signals(result)
        assert "sar" in result.columns
        assert (result["signal"] == 1).any()

    def test_sell_signal(self):
        closes = list(np.linspace(80, 140, 40)) + list(np.linspace(140, 70, 60))
        result = _run("parabolic_sar", closes)
        assert (result["signal"] == -1).any()

    def test_single_bar(self):
        result = _run("parabolic_sar", [100.0])
        assert result["sar"].isna().all()
        assert (result["signal"] == 0).all()


# ─── Delta Neutral Funding ──────────────────

class TestDeltaNeutralFunding:
    def test_entry_on_high_funding(self):
        result = _run("delta_neutral_funding", make_flat(20),
                       {"avg_funding_rate_7d": 0.0005, "entry_threshold": 0.0001})
        # Positive funding → SHORT perp to collect (#102)
        assert result["signal"].iloc[-1] == -1

    def test_exit_on_low_funding(self):
        result = _run("delta_neutral_funding", make_flat(20),
                       {"avg_funding_rate_7d": 0.00003, "exit_threshold": 0.00005})
        assert result["signal"].iloc[-1] == 1

    def test_zero_funding_no_signal(self):
        result = _run("delta_neutral_funding", make_flat(20),
                       {"avg_funding_rate_7d": 0.0})
        assert (result["signal"] == 0).all()


# ─── Chart Pattern (wrapper) ────────────────

class TestChartPattern:
    def test_returns_signal(self):
        closes = list(np.linspace(90, 110, 50)) + list(np.linspace(110, 90, 50))
        result = _run("chart_pattern", closes)
        assert "signal" in result.columns
        _valid_signals(result)


# ─── Liquidity Sweeps (wrapper) ─────────────

class TestLiquiditySweeps:
    def test_returns_signal(self):
        closes = list(np.linspace(90, 110, 50)) + list(np.linspace(110, 90, 50))
        result = _run("liquidity_sweeps", closes)
        assert "signal" in result.columns
        _valid_signals(result)


# ─── AMD+IFVG ──────────────────────────────

class TestAMDIFVG:
    def test_returns_signal_column(self):
        """AMD+IFVG should return signal column with datetime index."""
        n = 96  # 24 hours of 15-min candles
        idx = pd.date_range("2024-01-01", periods=n, freq="15min")
        closes = make_volatile(n, center=100, amplitude=5)
        result = _run("amd_ifvg", closes, index=idx)
        assert "signal" in result.columns
        _valid_signals(result)
        assert "asian_high" in result.columns
        assert "asian_low" in result.columns

    def test_short_data_no_signal(self):
        """Less than 3 bars should return all zeros."""
        idx = pd.date_range("2024-01-01", periods=2, freq="15min")
        result = _run("amd_ifvg", [100.0, 101.0], index=idx)
        assert (result["signal"] == 0).all()

    def test_no_crash_flat(self):
        """Flat data with datetime index should not crash."""
        n = 96
        idx = pd.date_range("2024-01-01", periods=n, freq="15min")
        result = _run("amd_ifvg", make_flat(n), index=idx)
        assert "signal" in result.columns


# ─── Edge Cases ─────────────────────────────

class TestEdgeCases:
    @pytest.mark.parametrize("name", [
        "sma_crossover", "ema_crossover", "bollinger_bands",
        "volume_weighted", "triple_ema", "triple_ema_bd", "rsi_macd_combo",
        "momentum", "mean_reversion", "rsi", "macd", "breakout",
        "stoch_rsi", "supertrend", "squeeze_momentum",
        "atr_breakout", "heikin_ashi_ema", "parabolic_sar",
        "ichimoku_cloud", "order_blocks", "delta_neutral_funding",
        "vwap_reversion", "chart_pattern", "liquidity_sweeps", "amd_ifvg",
    ])
    def test_empty_dataframe(self, name):
        df = pd.DataFrame(columns=["open", "high", "low", "close", "volume"])
        result = apply_strategy(name, df)
        assert len(result) == 0

    @pytest.mark.parametrize("name", [
        "sma_crossover", "ema_crossover", "bollinger_bands",
        "volume_weighted", "triple_ema", "triple_ema_bd", "rsi_macd_combo",
        "momentum", "mean_reversion", "rsi", "macd", "breakout",
        "stoch_rsi", "atr_breakout", "heikin_ashi_ema",
        "supertrend", "squeeze_momentum", "ichimoku_cloud",
        "order_blocks", "delta_neutral_funding",
        "chart_pattern", "liquidity_sweeps", "parabolic_sar", "amd_ifvg",
    ])
    def test_single_row(self, name):
        df = make_ohlcv([100.0])
        result = apply_strategy(name, df)
        assert len(result) == 1
        assert "signal" in result.columns
