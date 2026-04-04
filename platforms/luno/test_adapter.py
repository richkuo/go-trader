"""Tests for LunoExchangeAdapter — mock ccxt to avoid live API calls."""

import sys
import os
import importlib.util
import pytest
from unittest.mock import MagicMock, patch

# Load luno adapter by file path to avoid module name collisions
_adapter_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "adapter.py")
_shared_tools = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), '..', '..', 'shared_tools'))
if _shared_tools not in sys.path:
    sys.path.insert(0, _shared_tools)
_spec = importlib.util.spec_from_file_location("luno_adapter", _adapter_path)
_mod = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(_mod)

LunoExchangeAdapter = _mod.LunoExchangeAdapter


@pytest.fixture
def mock_exchange():
    """Provide a mock ccxt exchange and patch it into the adapter module."""
    mock_ex = MagicMock()
    original = _mod._get_ccxt_exchange
    _mod._get_ccxt_exchange = lambda: mock_ex
    yield mock_ex
    _mod._get_ccxt_exchange = original


# ─── Properties ────────────────────────────────────

class TestProperties:
    def test_name(self):
        adapter = LunoExchangeAdapter()
        assert adapter.name == "luno"


# ─── Spot Price ────────────────────────────────────

class TestSpotPrice:
    def test_get_spot_price_zar(self, mock_exchange):
        adapter = LunoExchangeAdapter()
        mock_exchange.fetch_ticker.return_value = {"last": 1200000.0}
        price = adapter.get_spot_price("BTC")
        assert price == 1200000.0
        mock_exchange.fetch_ticker.assert_called_once_with("BTC/ZAR")

    def test_get_spot_price_fallback_gbp(self, mock_exchange):
        adapter = LunoExchangeAdapter()
        mock_exchange.fetch_ticker.side_effect = [
            Exception("not found"),
            {"last": 53000.0},
        ]
        price = adapter.get_spot_price("BTC")
        assert price == 53000.0
        assert mock_exchange.fetch_ticker.call_count == 2

    def test_get_spot_price_all_fail(self, mock_exchange):
        adapter = LunoExchangeAdapter()
        mock_exchange.fetch_ticker.side_effect = Exception("fail")
        assert adapter.get_spot_price("BTC") == 0.0


# ─── Vol Metrics ───────────────────────────────────

class TestVolMetrics:
    def test_get_vol_metrics(self, mock_exchange):
        adapter = LunoExchangeAdapter()
        # Use volatile data: alternating +-5% swings so vol is non-trivial
        import math
        base = 1200000
        closes = [base * (1 + 0.05 * ((-1) ** i)) for i in range(90)]
        candles = [[i * 86400000, c * 0.99, c * 1.01, c * 0.98, c, 100] for i, c in enumerate(closes)]
        mock_exchange.fetch_ohlcv.return_value = candles
        vol, iv_rank = adapter.get_vol_metrics("BTC")
        assert vol > 0
        assert 0 <= iv_rank <= 100

    def test_get_vol_metrics_insufficient(self, mock_exchange):
        adapter = LunoExchangeAdapter()
        mock_exchange.fetch_ohlcv.return_value = []
        vol, iv_rank = adapter.get_vol_metrics("BTC")
        assert vol == 0.60
        assert iv_rank == 50.0

    def test_get_vol_metrics_error(self, mock_exchange):
        adapter = LunoExchangeAdapter()
        mock_exchange.fetch_ohlcv.side_effect = Exception("fail")
        vol, iv_rank = adapter.get_vol_metrics("BTC")
        assert vol == 0.60
        assert iv_rank == 50.0

    def test_get_vol_metrics_tries_multiple_quotes(self, mock_exchange):
        adapter = LunoExchangeAdapter()
        base = 1200000
        closes = [base * (1 + 0.05 * ((-1) ** i)) for i in range(90)]
        candles = [[i * 86400000, c * 0.99, c * 1.01, c * 0.98, c, 100] for i, c in enumerate(closes)]
        # First quote (ZAR) fails, second (GBP) succeeds
        mock_exchange.fetch_ohlcv.side_effect = [
            Exception("not found"),
            candles,
        ]
        vol, iv_rank = adapter.get_vol_metrics("BTC")
        assert vol > 0


# ─── Options Not Supported ─────────────────────────

class TestOptionsNotSupported:
    def test_get_real_expiry_raises(self):
        adapter = LunoExchangeAdapter()
        with pytest.raises(NotImplementedError, match="not support options"):
            adapter.get_real_expiry("BTC", 30)

    def test_get_real_strike_raises(self):
        adapter = LunoExchangeAdapter()
        with pytest.raises(NotImplementedError, match="not support options"):
            adapter.get_real_strike("BTC", "2026-05-01", "call", 70000)

    def test_get_premium_and_greeks_raises(self):
        adapter = LunoExchangeAdapter()
        with pytest.raises(NotImplementedError, match="not support options"):
            adapter.get_premium_and_greeks("BTC", "call", 70000, "2026-05-01", 30, 67000, 0.6)
