"""Tests for IBKRExchangeAdapter — mock ccxt to avoid live API calls."""

import sys
import os
import math
import importlib.util
import pytest
from unittest.mock import MagicMock, patch
from datetime import datetime, timezone

# Load ibkr adapter by file path to avoid module name collisions
_adapter_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "adapter.py")
_shared_tools = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), '..', '..', 'shared_tools'))
if _shared_tools not in sys.path:
    sys.path.insert(0, _shared_tools)
_spec = importlib.util.spec_from_file_location("ibkr_adapter", _adapter_path)
_mod = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(_mod)

IBKRExchangeAdapter = _mod.IBKRExchangeAdapter
CME_SPECS = _mod.CME_SPECS
DEFAULT_SPECS = _mod.DEFAULT_SPECS


# ─── Properties ────────────────────────────────────

class TestProperties:
    def test_name(self):
        adapter = IBKRExchangeAdapter()
        assert adapter.name == "ibkr"


# ─── Spot Price ────────────────────────────────────

class TestSpotPrice:
    def test_get_spot_price(self):
        adapter = IBKRExchangeAdapter()
        with patch("ccxt.binanceus") as mock_cls:
            mock_ex = MagicMock()
            mock_ex.fetch_ticker.return_value = {"last": 67500.0}
            mock_cls.return_value = mock_ex
            price = adapter.get_spot_price("BTC")
            assert price == 67500.0

    def test_get_spot_price_fallback_usd(self):
        adapter = IBKRExchangeAdapter()
        with patch("ccxt.binanceus") as mock_cls:
            mock_ex = MagicMock()
            mock_ex.fetch_ticker.side_effect = [
                Exception("not found"),
                {"last": 67000.0},
            ]
            mock_cls.return_value = mock_ex
            price = adapter.get_spot_price("BTC")
            assert price == 67000.0

    def test_get_spot_price_all_fail(self):
        adapter = IBKRExchangeAdapter()
        with patch("ccxt.binanceus") as mock_cls:
            mock_ex = MagicMock()
            mock_ex.fetch_ticker.side_effect = Exception("fail")
            mock_cls.return_value = mock_ex
            assert adapter.get_spot_price("BTC") == 0.0


# ─── Vol Metrics ───────────────────────────────────

class TestVolMetrics:
    def test_get_vol_metrics(self):
        adapter = IBKRExchangeAdapter()
        closes = [50000 + i * 100 for i in range(90)]
        candles = [[i * 86400000, c - 50, c + 50, c - 100, c, 1000] for i, c in enumerate(closes)]
        with patch("ccxt.binanceus") as mock_cls:
            mock_ex = MagicMock()
            mock_ex.fetch_ohlcv.return_value = candles
            mock_cls.return_value = mock_ex
            vol, iv_rank = adapter.get_vol_metrics("BTC")
            assert vol > 0
            assert 0 <= iv_rank <= 100

    def test_get_vol_metrics_insufficient_data(self):
        adapter = IBKRExchangeAdapter()
        with patch("ccxt.binanceus") as mock_cls:
            mock_ex = MagicMock()
            mock_ex.fetch_ohlcv.return_value = []
            mock_cls.return_value = mock_ex
            vol, iv_rank = adapter.get_vol_metrics("BTC")
            assert vol == 0.60
            assert iv_rank == 50.0


# ─── Options Protocol ──────────────────────────────

class TestOptionsProtocol:
    def test_get_real_expiry(self):
        adapter = IBKRExchangeAdapter()
        expiry, dte = adapter.get_real_expiry("BTC", 30)
        assert dte == 30
        datetime.strptime(expiry, "%Y-%m-%d")

    def test_get_real_strike_btc(self):
        adapter = IBKRExchangeAdapter()
        strike = adapter.get_real_strike("BTC", "2026-05-01", "call", 67500)
        assert strike == 68000  # round to nearest 1000

    def test_get_real_strike_eth(self):
        adapter = IBKRExchangeAdapter()
        strike = adapter.get_real_strike("ETH", "2026-05-01", "call", 3475)
        assert strike == 3500  # round to nearest 50

    def test_get_real_strike_default(self):
        adapter = IBKRExchangeAdapter()
        strike = adapter.get_real_strike("SOL", "2026-05-01", "call", 160)
        assert strike == 200  # round to nearest 100

    def test_get_premium_and_greeks(self):
        adapter = IBKRExchangeAdapter()
        pct, usd, greeks = adapter.get_premium_and_greeks(
            "BTC", "call", 70000, "2026-05-01", 30, 67000, 0.6
        )
        assert usd > 0
        assert pct > 0
        assert "delta" in greeks
        assert "gamma" in greeks
        assert 0 < greeks["delta"] < 1

    def test_get_premium_and_greeks_default_vol(self):
        adapter = IBKRExchangeAdapter()
        pct, usd, greeks = adapter.get_premium_and_greeks(
            "BTC", "call", 70000, "2026-05-01", 30, 67000, 0
        )
        # vol=0 should default to 0.80
        assert usd > 0

    def test_get_multiplier(self):
        adapter = IBKRExchangeAdapter()
        assert adapter.get_multiplier("BTC") == 0.1
        assert adapter.get_multiplier("ETH") == 0.5

    def test_get_strike_interval(self):
        adapter = IBKRExchangeAdapter()
        assert adapter.get_strike_interval("BTC") == 1000
        assert adapter.get_strike_interval("ETH") == 50
