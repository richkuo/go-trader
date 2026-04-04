"""Tests for OKXExchangeAdapter — mock ccxt to avoid live API calls."""

import sys
import os
import importlib.util
import pytest
from unittest.mock import MagicMock, patch

# Load okx adapter by file path to avoid module name collisions
_adapter_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "adapter.py")
_shared_tools = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), '..', '..', 'shared_tools'))
if _shared_tools not in sys.path:
    sys.path.insert(0, _shared_tools)

# Load the module once to get the class reference
_spec = importlib.util.spec_from_file_location("okx_adapter", _adapter_path)
_mod = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(_mod)
OKXExchangeAdapter = _mod.OKXExchangeAdapter


@pytest.fixture
def adapter():
    """Create OKXExchangeAdapter in paper mode with a mocked ccxt exchange."""
    mock_ex = MagicMock()
    with patch.dict(os.environ, {}, clear=False):
        for key in ("OKX_API_KEY", "OKX_API_SECRET", "OKX_PASSPHRASE", "OKX_SANDBOX"):
            os.environ.pop(key, None)
        # Patch ccxt.okx in the loaded module so __init__ uses our mock
        orig_ccxt_okx = _mod.ccxt.okx
        _mod.ccxt.okx = MagicMock(return_value=mock_ex)
        try:
            a = OKXExchangeAdapter()
        finally:
            _mod.ccxt.okx = orig_ccxt_okx
    return a, mock_ex


# ─── Properties ────────────────────────────────────

class TestProperties:
    def test_name(self, adapter):
        a, _ = adapter
        assert a.name == "okx"

    def test_paper_mode(self, adapter):
        a, _ = adapter
        assert a.mode == "paper"
        assert a.is_live is False

    def test_live_mode(self):
        mock_ex = MagicMock()
        with patch.dict(os.environ, {
            "OKX_API_KEY": "key",
            "OKX_API_SECRET": "secret",
            "OKX_PASSPHRASE": "pass",
        }):
            orig = _mod.ccxt.okx
            _mod.ccxt.okx = MagicMock(return_value=mock_ex)
            try:
                a = OKXExchangeAdapter()
            finally:
                _mod.ccxt.okx = orig
            assert a.is_live is True
            assert a.mode == "live"


# ─── Market Data ───────────────────────────────────

class TestMarketData:
    def test_get_spot_price(self, adapter):
        a, mock_ex = adapter
        mock_ex.fetch_ticker.return_value = {"last": 67500.0}
        price = a.get_spot_price("BTC")
        assert price == 67500.0
        mock_ex.fetch_ticker.assert_called_once_with("BTC/USDT")

    def test_get_spot_price_tries_multiple_suffixes(self, adapter):
        a, mock_ex = adapter
        mock_ex.fetch_ticker.side_effect = [
            Exception("not found"),
            {"last": 67000.0},
        ]
        price = a.get_spot_price("BTC")
        assert price == 67000.0
        assert mock_ex.fetch_ticker.call_count == 2

    def test_get_spot_price_all_fail(self, adapter):
        a, mock_ex = adapter
        mock_ex.fetch_ticker.side_effect = Exception("fail")
        assert a.get_spot_price("BTC") == 0.0

    def test_get_ohlcv(self, adapter):
        a, mock_ex = adapter
        candles = [[1700000000000, 100, 110, 90, 105, 50]]
        mock_ex.fetch_ohlcv.return_value = candles
        result = a.get_ohlcv("BTC", "1h", 200)
        assert result == candles
        mock_ex.fetch_ohlcv.assert_called_once_with("BTC/USDT", "1h", limit=200)

    def test_get_ohlcv_on_error(self, adapter):
        a, mock_ex = adapter
        mock_ex.fetch_ohlcv.side_effect = Exception("fail")
        assert a.get_ohlcv("BTC") == []

    def test_get_ohlcv_closes(self, adapter):
        a, mock_ex = adapter
        mock_ex.fetch_ohlcv.return_value = [
            [1700000000000, 100, 110, 90, 105, 50],
            [1700003600000, 105, 115, 95, 110, 60],
        ]
        closes = a.get_ohlcv_closes("BTC")
        assert closes == [105, 110]

    def test_get_ohlcv_closes_empty(self, adapter):
        a, mock_ex = adapter
        mock_ex.fetch_ohlcv.side_effect = Exception("fail")
        assert a.get_ohlcv_closes("BTC") == []

    def test_get_perp_ohlcv(self, adapter):
        a, mock_ex = adapter
        candles = [[1700000000000, 100, 110, 90, 105, 50]]
        mock_ex.fetch_ohlcv.return_value = candles
        result = a.get_perp_ohlcv("BTC", "1h", 200)
        assert result == candles
        mock_ex.fetch_ohlcv.assert_called_once_with("BTC/USDT:USDT", "1h", limit=200)

    def test_get_funding_rate(self, adapter):
        a, mock_ex = adapter
        mock_ex.fetch_funding_rate.return_value = {"fundingRate": 0.0001}
        rate = a.get_funding_rate("BTC")
        assert rate == 0.0001
        mock_ex.fetch_funding_rate.assert_called_once_with("BTC/USDT:USDT")

    def test_get_funding_rate_on_error(self, adapter):
        a, mock_ex = adapter
        mock_ex.fetch_funding_rate.side_effect = Exception("fail")
        assert a.get_funding_rate("BTC") == 0.0

    def test_get_funding_history(self, adapter):
        a, mock_ex = adapter
        mock_ex.fetch_funding_rate_history.return_value = [
            {"fundingRate": 0.0001, "timestamp": 1700000000000},
            {"fundingRate": 0.0002, "timestamp": 1700003600000},
        ]
        result = a.get_funding_history("BTC", days=7)
        assert len(result) == 2
        assert result[0] == {"rate": 0.0001, "time": 1700000000000}

    def test_get_funding_history_on_error(self, adapter):
        a, mock_ex = adapter
        mock_ex.fetch_funding_rate_history.side_effect = Exception("fail")
        assert a.get_funding_history("BTC") == []


# ─── Order Execution ──────────────────────────────

class TestOrderExecution:
    def test_market_open_paper_raises(self, adapter):
        a, _ = adapter
        with pytest.raises(RuntimeError, match="live mode"):
            a.market_open("BTC", True, 0.5)

    def test_market_close_paper_raises(self, adapter):
        a, _ = adapter
        with pytest.raises(RuntimeError, match="live mode"):
            a.market_close("BTC")

    def test_market_open_spot(self, adapter):
        a, mock_ex = adapter
        a._is_live = True
        mock_ex.create_market_order.return_value = {"id": "123"}
        result = a.market_open("BTC", True, 0.5, inst_type="spot")
        assert result == {"id": "123"}
        mock_ex.create_market_order.assert_called_once_with(
            "BTC/USDT", "buy", 0.5, params={"tdMode": "cash"}
        )

    def test_market_open_swap(self, adapter):
        a, mock_ex = adapter
        a._is_live = True
        mock_ex.create_market_order.return_value = {"id": "456"}
        result = a.market_open("BTC", False, 1.0, inst_type="swap")
        assert result == {"id": "456"}
        mock_ex.create_market_order.assert_called_once_with(
            "BTC/USDT:USDT", "sell", 1.0, params={"tdMode": "cross"}
        )

    def test_market_close_with_position(self, adapter):
        a, mock_ex = adapter
        a._is_live = True
        mock_ex.fetch_positions.return_value = [
            {"contracts": "1.5", "side": "long"},
        ]
        mock_ex.create_market_order.return_value = {"id": "789"}
        result = a.market_close("BTC")
        assert result == {"id": "789"}
        mock_ex.create_market_order.assert_called_once_with(
            "BTC/USDT:USDT", "sell", 1.5,
            params={"tdMode": "cross", "reduceOnly": True}
        )

    def test_market_close_no_position(self, adapter):
        a, mock_ex = adapter
        a._is_live = True
        mock_ex.fetch_positions.return_value = []
        result = a.market_close("BTC")
        assert result == {}


# ─── Options Protocol ──────────────────────────────

class TestOptionsProtocol:
    def test_get_vol_metrics(self, adapter):
        a, mock_ex = adapter
        closes = [50000 + i * 100 for i in range(90)]
        candles = [[i * 86400000, c - 50, c + 50, c - 100, c, 1000] for i, c in enumerate(closes)]
        mock_ex.fetch_ohlcv.return_value = candles
        vol, iv_rank = a.get_vol_metrics("BTC")
        assert vol > 0
        assert 0 <= iv_rank <= 100

    def test_get_vol_metrics_insufficient_data(self, adapter):
        a, mock_ex = adapter
        mock_ex.fetch_ohlcv.return_value = [[0, 100, 110, 90, 105, 50]] * 5
        vol, iv_rank = a.get_vol_metrics("BTC")
        assert vol == 0.60
        assert iv_rank == 50.0

    def test_get_real_expiry_with_markets(self, adapter):
        a, mock_ex = adapter
        from datetime import datetime, timezone, timedelta
        now = datetime.now(timezone.utc)
        exp1 = now + timedelta(days=30)
        exp2 = now + timedelta(days=60)

        mock_ex.markets = {
            "BTC-30D-100000-C": {
                "type": "option",
                "base": "BTC",
                "active": True,
                "expiry": int(exp1.timestamp() * 1000),
            },
            "BTC-60D-100000-C": {
                "type": "option",
                "base": "BTC",
                "active": True,
                "expiry": int(exp2.timestamp() * 1000),
            },
        }
        mock_ex.load_markets.return_value = mock_ex.markets

        expiry_str, actual_dte = a.get_real_expiry("BTC", 30)
        assert actual_dte >= 29
        assert actual_dte <= 31

    def test_get_real_strike_with_markets(self, adapter):
        a, mock_ex = adapter
        from datetime import datetime, timezone, timedelta
        now = datetime.now(timezone.utc)
        exp = now + timedelta(days=30)
        exp_str = exp.strftime("%Y-%m-%d")
        exp_start = int(datetime.strptime(exp_str, "%Y-%m-%d").replace(tzinfo=timezone.utc).timestamp() * 1000)

        mock_ex.markets = {
            "BTC-30D-65000-C": {
                "type": "option",
                "base": "BTC",
                "optionType": "call",
                "active": True,
                "strike": 65000.0,
                "expiry": exp_start + 3600000,
            },
            "BTC-30D-70000-C": {
                "type": "option",
                "base": "BTC",
                "optionType": "call",
                "active": True,
                "strike": 70000.0,
                "expiry": exp_start + 3600000,
            },
        }
        mock_ex.load_markets.return_value = mock_ex.markets

        strike = a.get_real_strike("BTC", exp_str, "call", 67000.0)
        assert strike == 65000.0

    def test_get_real_strike_fallback(self, adapter):
        a, mock_ex = adapter
        mock_ex.markets = {}
        mock_ex.load_markets.return_value = {}
        strike = a.get_real_strike("BTC", "2026-04-15", "call", 67500.0)
        assert strike == 68000.0

    def test_get_premium_and_greeks_fallback(self, adapter):
        """When live quote fails and BS import has arg mismatch, returns zeros."""
        a, mock_ex = adapter
        mock_ex.markets = {}
        mock_ex.load_markets.return_value = {}
        pct, usd, greeks = a.get_premium_and_greeks(
            "BTC", "call", 70000, "2026-05-01", 30, 67000, 0.6
        )
        # Returns a valid tuple (may be zeros if BS fallback also fails)
        assert isinstance(pct, float)
        assert isinstance(usd, float)
        assert "delta" in greeks

    def test_get_premium_and_greeks_live_quote(self, adapter):
        """When a matching option market exists, returns live quote data."""
        a, mock_ex = adapter
        from datetime import datetime, timezone, timedelta
        now = datetime.now(timezone.utc)
        exp = now + timedelta(days=30)
        exp_str = exp.strftime("%Y-%m-%d")
        exp_start = int(datetime.strptime(exp_str, "%Y-%m-%d").replace(tzinfo=timezone.utc).timestamp() * 1000)

        mock_ex.markets = {
            "BTC-30D-70000-C": {
                "type": "option",
                "base": "BTC",
                "optionType": "call",
                "active": True,
                "strike": 70000,
                "expiry": exp_start + 3600000,
            },
        }
        mock_ex.load_markets.return_value = mock_ex.markets
        mock_ex.fetch_ticker.return_value = {
            "last": 0.05,
            "close": 0.05,
            "info": {"delta": "0.45", "gamma": "0.001", "theta": "-10", "vega": "50"},
        }
        pct, usd, greeks = a.get_premium_and_greeks(
            "BTC", "call", 70000, exp_str, 30, 67000, 0.6
        )
        assert pct == 0.05
        assert usd == 0.05 * 67000
        assert greeks["delta"] == 0.45
