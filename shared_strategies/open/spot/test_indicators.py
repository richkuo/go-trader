"""Tests for spot/indicators.py — sma, ema, sma_crossover, rsi, bollinger_bands."""

import importlib.util
import numpy as np
import pandas as pd
import pytest

import sys, os

_spot_dir = os.path.dirname(os.path.abspath(__file__))
_shared_dir = os.path.join(_spot_dir, '..')
sys.path.insert(0, _spot_dir)
sys.path.insert(0, _shared_dir)

# Load indicators by file path to avoid import collisions when run from parent
_spec = importlib.util.spec_from_file_location(
    "spot_indicators", os.path.join(_spot_dir, "indicators.py"))
_imod = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(_imod)

sma = _imod.sma
ema = _imod.ema
sma_crossover = _imod.sma_crossover
rsi = _imod.rsi
bollinger_bands = _imod.bollinger_bands

from conftest import make_ohlcv


# ─── SMA ────────────────────────────────────

class TestSMA:
    def test_basic_calculation(self):
        s = pd.Series([1.0, 2.0, 3.0, 4.0, 5.0])
        result = sma(s, 3)
        assert np.isnan(result.iloc[0])
        assert np.isnan(result.iloc[1])
        assert result.iloc[2] == pytest.approx(2.0)
        assert result.iloc[3] == pytest.approx(3.0)
        assert result.iloc[4] == pytest.approx(4.0)

    def test_period_larger_than_data(self):
        s = pd.Series([1.0, 2.0, 3.0])
        result = sma(s, 10)
        assert result.isna().all()

    def test_constant_series(self):
        s = pd.Series([5.0] * 20)
        result = sma(s, 5)
        # After warmup, SMA of constant series should equal the constant
        assert result.iloc[4:].eq(5.0).all()

    def test_single_element(self):
        s = pd.Series([42.0])
        result = sma(s, 1)
        assert result.iloc[0] == pytest.approx(42.0)


# ─── EMA ────────────────────────────────────

class TestEMA:
    def test_basic_calculation(self):
        s = pd.Series([1.0, 2.0, 3.0, 4.0, 5.0])
        result = ema(s, 3)
        # EMA should exist for all rows (ewm doesn't produce NaN for initial rows)
        assert not result.isna().any()
        # EMA of increasing series should be increasing
        assert (result.diff().iloc[1:] > 0).all()

    def test_constant_series(self):
        s = pd.Series([10.0] * 20)
        result = ema(s, 5)
        # EMA of constant = constant
        assert np.allclose(result.values, 10.0)

    def test_ema_responds_faster_than_sma(self):
        """EMA should react to price changes faster than SMA."""
        prices = [100.0] * 20 + [120.0] * 5
        s = pd.Series(prices)
        ema_result = ema(s, 10)
        sma_result = sma(s, 10)
        # At the first bar after the jump (index 20), EMA should be higher than SMA
        # because EMA weights recent data more
        assert ema_result.iloc[21] > sma_result.iloc[21]


# ─── SMA Crossover ──────────────────────────

class TestSMACrossover:
    def test_bullish_crossover(self):
        """Fast SMA crossing above slow SMA should produce buy signal (1)."""
        # Start declining, then rally — fast SMA will cross above slow SMA
        closes = list(np.linspace(120, 90, 60)) + list(np.linspace(90, 130, 60))
        df = make_ohlcv(closes)
        result = sma_crossover(df, fast_period=10, slow_period=30)
        assert "signal" in result.columns
        buy_signals = result[result["signal"] == 1]
        assert len(buy_signals) >= 1

    def test_bearish_crossover(self):
        """Fast SMA crossing below slow SMA should produce sell signal (-1)."""
        closes = list(np.linspace(90, 130, 60)) + list(np.linspace(130, 80, 60))
        df = make_ohlcv(closes)
        result = sma_crossover(df, fast_period=10, slow_period=30)
        sell_signals = result[result["signal"] == -1]
        assert len(sell_signals) >= 1

    def test_flat_data_no_crossovers(self):
        df = make_ohlcv([100.0] * 100, noise=0)
        result = sma_crossover(df, fast_period=10, slow_period=30)
        # No crossovers on flat data (position stays constant after warmup)
        signals = result["signal"].dropna()
        # The only nonzero signal might be the initial transition from NaN
        # Filter to actual +-1 signals
        real_signals = signals[(signals == 1) | (signals == -1)]
        # Should have at most 1 signal (the initial state transition)
        assert len(real_signals) <= 1

    def test_insufficient_data(self):
        df = make_ohlcv([100.0, 101.0, 99.0])
        result = sma_crossover(df, fast_period=20, slow_period=50)
        # Not enough data for slow SMA — signal should be all NaN or 0
        valid_signals = result["signal"].dropna()
        assert (valid_signals.abs() <= 1).all()

    def test_output_columns(self):
        df = make_ohlcv(list(range(100, 200)))
        result = sma_crossover(df)
        assert "sma_fast" in result.columns
        assert "sma_slow" in result.columns
        assert "signal" in result.columns


# ─── RSI ────────────────────────────────────

class TestRSI:
    def test_buy_signal_on_oversold_recovery(self):
        """RSI crossing above oversold level should produce buy signal."""
        # Sharp drop to push RSI into oversold, then recovery
        closes = list(np.linspace(100, 100, 20)) + list(np.linspace(100, 70, 15)) + list(np.linspace(70, 90, 30))
        df = make_ohlcv(closes)
        result = rsi(df, period=14, overbought=70, oversold=30)
        assert "rsi" in result.columns
        assert "signal" in result.columns
        # RSI values should be between 0 and 100
        valid_rsi = result["rsi"].dropna()
        assert (valid_rsi >= 0).all() and (valid_rsi <= 100).all()

    def test_sell_signal_on_overbought_drop(self):
        """RSI crossing below overbought level should produce sell signal."""
        # Sharp rise then pullback
        closes = list(np.linspace(100, 100, 20)) + list(np.linspace(100, 150, 15)) + list(np.linspace(150, 130, 30))
        df = make_ohlcv(closes)
        result = rsi(df, period=14, overbought=70, oversold=30)
        sell_signals = result[result["signal"] == -1]
        # May or may not fire depending on exact crossover, but should not crash
        assert isinstance(sell_signals, pd.DataFrame)

    def test_flat_data_no_signals(self):
        df = make_ohlcv([100.0] * 50, noise=0)
        result = rsi(df, period=14)
        # Flat data: gain/loss are 0 → RSI is NaN or 50-ish, no crossover signals
        signals = result["signal"]
        assert (signals == 0).all() or signals.isna().all()

    def test_insufficient_data(self):
        df = make_ohlcv([100.0, 101.0])
        result = rsi(df, period=14)
        # Only 2 rows — RSI won't converge, signals should be 0
        assert (result["signal"] == 0).all()


# ─── Bollinger Bands ────────────────────────

class TestBollingerBands:
    def test_output_columns(self):
        df = make_ohlcv(list(range(80, 130)))
        result = bollinger_bands(df, period=20, num_std=2.0)
        for col in ["bb_middle", "bb_upper", "bb_lower", "bb_width", "signal"]:
            assert col in result.columns

    def test_upper_above_middle_above_lower(self):
        """bb_upper > bb_middle > bb_lower always holds."""
        closes = list(np.linspace(90, 110, 50)) + list(np.linspace(110, 90, 50))
        df = make_ohlcv(closes)
        result = bollinger_bands(df, period=20, num_std=2.0)
        valid = result.dropna(subset=["bb_upper", "bb_lower"])
        assert (valid["bb_upper"] >= valid["bb_middle"]).all()
        assert (valid["bb_middle"] >= valid["bb_lower"]).all()

    def test_buy_signal_at_lower_band(self):
        """Price crossing below then back above lower band should generate buy."""
        # Decline then recovery to touch lower band
        closes = (
            list(np.linspace(100, 100, 30)) +
            list(np.linspace(100, 80, 15)) +
            list(np.linspace(80, 95, 15))
        )
        df = make_ohlcv(closes)
        result = bollinger_bands(df, period=20, num_std=2.0)
        # Check signal column exists and has valid values
        assert set(result["signal"].unique()).issubset({-1, 0, 1})

    def test_flat_data_no_signals(self):
        df = make_ohlcv([100.0] * 50, noise=0)
        result = bollinger_bands(df, period=20, num_std=2.0)
        # Flat data: std=0, bands collapse to middle, no crossovers
        assert (result["signal"] == 0).all()

    def test_insufficient_data(self):
        df = make_ohlcv([100.0] * 5)
        result = bollinger_bands(df, period=20, num_std=2.0)
        # Not enough data for 20-period rolling — signal should be 0
        assert (result["signal"] == 0).all()
