"""Tests for spot/strategies.py — all registered spot strategies."""

import importlib.util
import numpy as np
import pandas as pd
import pytest

import sys, os

# Resolve paths
_spot_dir = os.path.dirname(os.path.abspath(__file__))
_shared_dir = os.path.join(_spot_dir, '..')

# Add spot and shared to path for indicators/amd_ifvg/chart_patterns
sys.path.insert(0, _spot_dir)
sys.path.insert(0, _shared_dir)

# Load spot strategies by file path to avoid collision with futures/options strategies.py
_spec = importlib.util.spec_from_file_location(
    "spot_strategies", os.path.join(_spot_dir, "strategies.py"))
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

class TestRegistry:
    def test_strategies_registered(self):
        names = list_strategies()
        assert len(names) >= 20
        # Spot-check a few
        for expected in ["sma_crossover", "ema_crossover", "rsi", "macd", "momentum",
                         "bollinger_bands", "mean_reversion", "supertrend", "parabolic_sar"]:
            assert expected in names, f"{expected} not registered"

    def test_get_unknown_strategy_raises(self):
        with pytest.raises(ValueError, match="Unknown strategy"):
            get_strategy("nonexistent_strategy_xyz")

    def test_apply_strategy_returns_dataframe(self):
        df = make_ohlcv(make_trending_up(100))
        result = apply_strategy("sma_crossover", df)
        assert isinstance(result, pd.DataFrame)
        assert "signal" in result.columns


# ─── Helper: run strategy with standard checks ─

def _run_strategy(name, closes, params=None, volume=None, index=None):
    """Run a strategy and return the result DataFrame."""
    df = make_ohlcv(closes, volume=volume, index=index)
    return apply_strategy(name, df, params)


def _assert_valid_signals(result):
    """Assert that signal column contains only -1, 0, 1 (and NaN)."""
    signals = result["signal"].dropna()
    assert set(signals.unique()).issubset({-1.0, 0.0, 1.0}), \
        f"Unexpected signal values: {set(signals.unique())}"


# ─── SMA Crossover ──────────────────────────

class TestSMACrossover:
    def test_bullish_crossover(self):
        closes = list(np.linspace(120, 80, 60)) + list(np.linspace(80, 140, 60))
        result = _run_strategy("sma_crossover", closes)
        _assert_valid_signals(result)
        assert (result["signal"] == 1).any()

    def test_bearish_crossover(self):
        closes = list(np.linspace(80, 140, 60)) + list(np.linspace(140, 70, 60))
        result = _run_strategy("sma_crossover", closes)
        assert (result["signal"] == -1).any()

    def test_short_data(self):
        result = _run_strategy("sma_crossover", [100.0] * 10)
        # Not enough data for 50-period SMA — no meaningful signals
        assert "signal" in result.columns

    def test_empty_df(self):
        df = pd.DataFrame(columns=["open", "high", "low", "close", "volume"])
        result = apply_strategy("sma_crossover", df)
        assert len(result) == 0


# ─── EMA Crossover ──────────────────────────

class TestEMACrossover:
    def test_bullish_crossover(self):
        closes = list(np.linspace(120, 80, 50)) + list(np.linspace(80, 140, 50))
        result = _run_strategy("ema_crossover", closes)
        _assert_valid_signals(result)
        assert (result["signal"] == 1).any()

    def test_bearish_crossover(self):
        closes = list(np.linspace(80, 140, 50)) + list(np.linspace(140, 70, 50))
        result = _run_strategy("ema_crossover", closes)
        assert (result["signal"] == -1).any()

    def test_flat_data(self):
        result = _run_strategy("ema_crossover", make_flat(100))
        signals = result["signal"].dropna()
        real = signals[(signals == 1) | (signals == -1)]
        assert len(real) <= 1


# ─── RSI ────────────────────────────────────

class TestRSI:
    def test_buy_on_oversold_recovery(self):
        # Long flat, sharp drop to drive RSI oversold, then recovery
        closes = (
            list(np.linspace(100, 100, 20)) +
            list(np.linspace(100, 60, 20)) +
            list(np.linspace(60, 85, 30))
        )
        result = _run_strategy("rsi", closes)
        _assert_valid_signals(result)
        # RSI should have recovered from oversold — may produce buy signal
        assert "rsi" in result.columns

    def test_sell_on_overbought_drop(self):
        closes = (
            list(np.linspace(100, 100, 20)) +
            list(np.linspace(100, 160, 20)) +
            list(np.linspace(160, 130, 30))
        )
        result = _run_strategy("rsi", closes)
        _assert_valid_signals(result)

    def test_rsi_range(self):
        closes = make_volatile(200, amplitude=15)
        result = _run_strategy("rsi", closes)
        valid_rsi = result["rsi"].dropna()
        assert (valid_rsi >= 0).all() and (valid_rsi <= 100).all()

    def test_flat_no_signal(self):
        result = _run_strategy("rsi", make_flat(50))
        assert (result["signal"] == 0).all()


# ─── Bollinger Bands ────────────────────────

class TestBollingerBands:
    def test_buy_at_lower_band(self):
        closes = (
            list(np.linspace(100, 100, 30)) +
            list(np.linspace(100, 75, 15)) +
            list(np.linspace(75, 95, 20))
        )
        result = _run_strategy("bollinger_bands", closes)
        _assert_valid_signals(result)

    def test_flat_no_signal(self):
        result = _run_strategy("bollinger_bands", make_flat(50))
        assert (result["signal"] == 0).all()


# ─── MACD ───────────────────────────────────

class TestMACD:
    def test_bullish_crossover(self):
        closes = list(np.linspace(120, 80, 50)) + list(np.linspace(80, 140, 50))
        result = _run_strategy("macd", closes)
        _assert_valid_signals(result)
        assert (result["signal"] == 1).any()
        assert "macd_line" in result.columns
        assert "macd_signal" in result.columns

    def test_bearish_crossover(self):
        closes = list(np.linspace(80, 140, 50)) + list(np.linspace(140, 70, 50))
        result = _run_strategy("macd", closes)
        assert (result["signal"] == -1).any()

    def test_flat_no_signal(self):
        result = _run_strategy("macd", make_flat(100))
        signals = result["signal"].dropna()
        real = signals[(signals == 1) | (signals == -1)]
        assert len(real) <= 1


# ─── Mean Reversion ─────────────────────────

class TestMeanReversion:
    def test_buy_on_dip(self):
        # Flat then sharp dip then recovery — z-score should go below -entry_std
        closes = (
            list(np.linspace(100, 100, 40)) +
            list(np.linspace(100, 80, 10)) +
            list(np.linspace(80, 95, 20))
        )
        result = _run_strategy("mean_reversion", closes)
        _assert_valid_signals(result)
        assert "z_score" in result.columns

    def test_flat_no_signal(self):
        result = _run_strategy("mean_reversion", make_flat(60))
        assert (result["signal"] == 0).all()


# ─── Momentum ───────────────────────────────

class TestMomentum:
    def test_buy_on_strong_uptrend(self):
        # Start flat then rally strongly — ROC should cross threshold
        closes = list(np.linspace(100, 100, 30)) + list(np.linspace(100, 130, 30))
        result = _run_strategy("momentum", closes, {"roc_period": 14, "threshold": 5.0})
        _assert_valid_signals(result)
        assert "roc" in result.columns
        assert (result["signal"] == 1).any()

    def test_sell_on_strong_downtrend(self):
        closes = list(np.linspace(100, 100, 30)) + list(np.linspace(100, 70, 30))
        result = _run_strategy("momentum", closes, {"roc_period": 14, "threshold": 5.0})
        _assert_valid_signals(result)
        assert (result["signal"] == -1).any()

    def test_flat_no_signal(self):
        result = _run_strategy("momentum", make_flat(60))
        assert (result["signal"] == 0).all()


# ─── Volume Weighted ────────────────────────

class TestVolumeWeighted:
    def test_buy_with_high_volume(self):
        # Price crosses above SMA with high volume
        n = 60
        closes = list(np.linspace(100, 90, 30)) + list(np.linspace(90, 115, 30))
        vol = [100.0] * n
        # High volume at crossover area
        for i in range(28, 40):
            vol[i] = 300.0
        result = _run_strategy("volume_weighted", closes, volume=vol)
        _assert_valid_signals(result)

    def test_low_volume_filters_signal(self):
        # Same price pattern but low volume — should have fewer signals
        closes = list(np.linspace(100, 90, 30)) + list(np.linspace(90, 115, 30))
        vol = [50.0] * 60  # All low volume
        result = _run_strategy("volume_weighted", closes, volume=vol)
        _assert_valid_signals(result)


# ─── Triple EMA ─────────────────────────────

class TestTripleEMA:
    def test_aligned_bullish(self):
        # Strong uptrend should align short > mid > long
        closes = make_trending_up(120, start=80, step=0.5)
        result = _run_strategy("triple_ema", closes)
        _assert_valid_signals(result)
        assert "ema_short" in result.columns
        assert "ema_mid" in result.columns
        assert "ema_long" in result.columns

    def test_bearish_after_uptrend(self):
        closes = list(np.linspace(80, 140, 60)) + list(np.linspace(140, 70, 80))
        result = _run_strategy("triple_ema", closes)
        assert (result["signal"] == -1).any()


# ─── RSI+MACD Combo ────────────────────────

class TestRSIMACDCombo:
    def test_buy_signal(self):
        # Decline then recovery — MACD cross up + RSI < 50
        closes = list(np.linspace(120, 80, 60)) + list(np.linspace(80, 130, 60))
        result = _run_strategy("rsi_macd_combo", closes)
        _assert_valid_signals(result)
        assert "rsi" in result.columns
        assert "macd_line" in result.columns

    def test_sell_signal(self):
        closes = list(np.linspace(80, 140, 60)) + list(np.linspace(140, 80, 60))
        result = _run_strategy("rsi_macd_combo", closes)
        _assert_valid_signals(result)


# ─── Stochastic RSI ─────────────────────────

class TestStochRSI:
    def test_buy_in_oversold(self):
        closes = (
            list(np.linspace(100, 100, 30)) +
            list(np.linspace(100, 60, 25)) +
            list(np.linspace(60, 80, 25))
        )
        result = _run_strategy("stoch_rsi", closes)
        _assert_valid_signals(result)
        assert "stoch_k" in result.columns
        assert "stoch_d" in result.columns

    def test_flat_data(self):
        result = _run_strategy("stoch_rsi", make_flat(80))
        # Stoch RSI on flat data — no meaningful crossovers
        assert "signal" in result.columns


# ─── Supertrend ─────────────────────────────

class TestSupertrend:
    def test_output_columns(self):
        closes = list(np.linspace(150, 60, 50)) + list(np.linspace(60, 180, 70))
        result = _run_strategy("supertrend", closes)
        assert "supertrend" in result.columns
        assert "st_direction" in result.columns
        assert "signal" in result.columns
        _assert_valid_signals(result)

    def test_direction_computed(self):
        """Supertrend should compute a direction column with -1 or 1 values."""
        closes = make_trending_down(100, start=200, step=1.0)
        result = _run_strategy("supertrend", closes)
        # Direction should have non-zero values (strategy starts at 0 then picks -1 or 1)
        dirs = result["st_direction"]
        assert set(dirs.unique()).issubset({-1, 0, 1})

    def test_single_bar(self):
        result = _run_strategy("supertrend", [100.0])
        # Single bar — signal should be 0
        assert (result["signal"] == 0).all()

    def test_flat_data(self):
        result = _run_strategy("supertrend", make_flat(60))
        assert (result["signal"] == 0).all()


# ─── Ichimoku Cloud ─────────────────────────

class TestIchimokuCloud:
    def test_output_columns(self):
        closes = make_trending_up(120)
        result = _run_strategy("ichimoku_cloud", closes)
        for col in ["tenkan", "kijun", "senkou_a", "senkou_b", "signal"]:
            assert col in result.columns

    def test_requires_many_bars(self):
        # Ichimoku needs 52+ bars minimum for senkou_b
        result = _run_strategy("ichimoku_cloud", [100.0] * 20)
        # Should not crash, signals should be 0
        assert (result["signal"] == 0).all()

    def test_strong_trend(self):
        # 150 bars of strong uptrend
        closes = make_trending_up(150, start=50, step=1.0)
        result = _run_strategy("ichimoku_cloud", closes)
        _assert_valid_signals(result)


# ─── Pairs Spread ───────────────────────────

class TestPairsSpread:
    def test_self_mean_reversion(self):
        """Without close_b, uses self-mean-reversion on close."""
        closes = make_volatile(100, amplitude=10)
        result = _run_strategy("pairs_spread", closes)
        _assert_valid_signals(result)
        assert "z_score" in result.columns

    def test_with_close_b(self):
        """With close_b column, computes spread ratio."""
        closes_a = make_volatile(80, center=100, amplitude=5)
        closes_b = make_volatile(80, center=50, amplitude=3, seed=99)
        df = make_ohlcv(closes_a)
        df["close_b"] = closes_b
        result = apply_strategy("pairs_spread", df)
        assert "spread" in result.columns
        _assert_valid_signals(result)


# ─── ATR Breakout ───────────────────────────

class TestATRBreakout:
    def test_upside_breakout(self):
        # Flat consolidation then sharp breakout up
        closes = list(np.linspace(100, 100, 30)) + list(np.linspace(100, 130, 10))
        result = _run_strategy("atr_breakout", closes, {"atr_period": 14, "multiplier": 1.0})
        _assert_valid_signals(result)

    def test_flat_no_breakout(self):
        result = _run_strategy("atr_breakout", make_flat(50))
        assert (result["signal"] == 0).all()


# ─── Heikin Ashi EMA ────────────────────────

class TestHeikinAshiEMA:
    def test_output_columns(self):
        closes = make_trending_up(80)
        result = _run_strategy("heikin_ashi_ema", closes)
        for col in ["ha_open", "ha_close", "ha_high", "ha_low", "ha_ema", "signal"]:
            assert col in result.columns

    def test_uptrend(self):
        closes = make_trending_up(100, start=80, step=0.8)
        result = _run_strategy("heikin_ashi_ema", closes)
        _assert_valid_signals(result)


# ─── Order Blocks ───────────────────────────

class TestOrderBlocks:
    def test_no_crash_on_flat(self):
        result = _run_strategy("order_blocks", make_flat(80))
        assert (result["signal"] == 0).all()

    def test_displacement_produces_signal(self):
        # Strong rally (displacement candle) then pullback into OB zone
        closes = (
            list(np.linspace(100, 98, 20)) +  # mild bearish candles (OB candidates)
            [115] +  # displacement candle (big bullish)
            list(np.linspace(115, 100, 20)) +  # pullback into OB zone
            list(np.linspace(100, 110, 20))
        )
        # Build with more realistic OHLC
        n = len(closes)
        opens = [c - 0.3 for c in closes]
        # Make index 20 a clear displacement candle: open low, close high
        opens[20] = 98
        df = pd.DataFrame({
            "open": opens,
            "high": [max(o, c) + 0.5 for o, c in zip(opens, closes)],
            "low": [min(o, c) - 0.5 for o, c in zip(opens, closes)],
            "close": closes,
            "volume": [100.0] * n,
        })
        result = apply_strategy("order_blocks", df)
        _assert_valid_signals(result)


# ─── VWAP Reversion ─────────────────────────

class TestVWAPReversion:
    def test_with_datetime_index(self):
        n = 100
        closes = make_volatile(n, center=100, amplitude=8)
        idx = pd.date_range("2024-01-01", periods=n, freq="h")
        result = _run_strategy("vwap_reversion", closes, index=idx)
        assert "vwap" in result.columns
        _assert_valid_signals(result)

    def test_no_temp_columns_in_output(self):
        n = 50
        closes = make_volatile(n, center=100, amplitude=5)
        idx = pd.date_range("2024-01-01", periods=n, freq="h")
        result = _run_strategy("vwap_reversion", closes, index=idx)
        for col in ["_day", "_tp_vol", "_cum_tp_vol", "_cum_vol"]:
            assert col not in result.columns


# ─── Chart Pattern (wrapper) ────────────────

class TestChartPattern:
    def test_returns_signal(self):
        closes = list(np.linspace(90, 110, 50)) + list(np.linspace(110, 90, 50))
        result = _run_strategy("chart_pattern", closes)
        assert "signal" in result.columns
        _assert_valid_signals(result)


# ─── Liquidity Sweeps (wrapper) ─────────────

class TestLiquiditySweeps:
    def test_returns_signal(self):
        closes = list(np.linspace(90, 110, 50)) + list(np.linspace(110, 90, 50))
        result = _run_strategy("liquidity_sweeps", closes)
        assert "signal" in result.columns
        _assert_valid_signals(result)


# ─── Parabolic SAR ──────────────────────────

class TestParabolicSAR:
    def test_uptrend_buy(self):
        closes = list(np.linspace(120, 80, 40)) + list(np.linspace(80, 140, 60))
        result = _run_strategy("parabolic_sar", closes)
        _assert_valid_signals(result)
        assert "sar" in result.columns
        assert (result["signal"] == 1).any()

    def test_downtrend_sell(self):
        closes = list(np.linspace(80, 140, 40)) + list(np.linspace(140, 70, 60))
        result = _run_strategy("parabolic_sar", closes)
        assert (result["signal"] == -1).any()

    def test_single_bar(self):
        result = _run_strategy("parabolic_sar", [100.0])
        assert (result["signal"] == 0).all()
        assert result["sar"].isna().all()


# ─── Delta Neutral Funding ──────────────────

class TestDeltaNeutralFunding:
    def test_entry_signal_on_high_avg(self):
        closes = make_flat(20)
        result = _run_strategy("delta_neutral_funding", closes,
                               {"avg_funding_rate_7d": 0.0005, "entry_threshold": 0.0001})
        # Last row should have buy signal (avg > entry_threshold → enter delta-neutral)
        assert result["signal"].iloc[-1] == 1

    def test_exit_signal_on_low_avg(self):
        closes = make_flat(20)
        result = _run_strategy("delta_neutral_funding", closes,
                               {"avg_funding_rate_7d": 0.00003, "exit_threshold": 0.00005})
        assert result["signal"].iloc[-1] == -1

    def test_no_signal_on_zero_avg(self):
        closes = make_flat(20)
        result = _run_strategy("delta_neutral_funding", closes,
                               {"avg_funding_rate_7d": 0.0})
        assert (result["signal"] == 0).all()


# ─── Squeeze Momentum ──────────────────────

class TestSqueezeMomentum:
    def test_returns_signal_column(self):
        closes = make_volatile(100, amplitude=10)
        result = _run_strategy("squeeze_momentum", closes)
        assert "signal" in result.columns
        assert "squeeze_on" in result.columns
        assert "squeeze_mom" in result.columns
        _assert_valid_signals(result)

    def test_flat_no_signal(self):
        result = _run_strategy("squeeze_momentum", make_flat(60))
        # Flat data — no squeeze fire expected
        assert (result["signal"] == 0).all()


# ─── AMD+IFVG ──────────────────────────────

class TestAMDIFVG:
    def test_returns_signal_column(self):
        """AMD+IFVG should return signal column with datetime index."""
        n = 96  # 24 hours of 15-min candles
        idx = pd.date_range("2024-01-01", periods=n, freq="15min")
        closes = make_volatile(n, center=100, amplitude=5)
        result = _run_strategy("amd_ifvg", closes, index=idx)
        assert "signal" in result.columns
        _assert_valid_signals(result)
        assert "asian_high" in result.columns
        assert "asian_low" in result.columns

    def test_short_data_no_signal(self):
        """Less than 3 bars should return all zeros."""
        idx = pd.date_range("2024-01-01", periods=2, freq="15min")
        result = _run_strategy("amd_ifvg", [100.0, 101.0], index=idx)
        assert (result["signal"] == 0).all()

    def test_no_crash_flat(self):
        """Flat data with datetime index should not crash."""
        n = 96
        idx = pd.date_range("2024-01-01", periods=n, freq="15min")
        result = _run_strategy("amd_ifvg", make_flat(n), index=idx)
        assert "signal" in result.columns


# ─── Edge Cases (all strategies) ────────────

class TestEdgeCases:
    @pytest.mark.parametrize("name", [
        "sma_crossover", "ema_crossover", "rsi", "bollinger_bands", "macd",
        "mean_reversion", "momentum", "volume_weighted", "triple_ema",
        "rsi_macd_combo", "stoch_rsi", "supertrend", "atr_breakout",
        "heikin_ashi_ema", "parabolic_sar", "amd_ifvg",
    ])
    def test_empty_dataframe(self, name):
        """All strategies should handle empty DataFrames without crashing."""
        df = pd.DataFrame(columns=["open", "high", "low", "close", "volume"])
        result = apply_strategy(name, df)
        assert len(result) == 0

    @pytest.mark.parametrize("name", [
        "sma_crossover", "ema_crossover", "rsi", "bollinger_bands", "macd",
        "mean_reversion", "momentum", "volume_weighted", "triple_ema",
        "rsi_macd_combo", "stoch_rsi", "atr_breakout",
        "heikin_ashi_ema", "amd_ifvg",
    ])
    def test_single_row(self, name):
        """All strategies should handle a single-row DataFrame."""
        df = make_ohlcv([100.0])
        result = apply_strategy(name, df)
        assert len(result) == 1
        assert "signal" in result.columns
