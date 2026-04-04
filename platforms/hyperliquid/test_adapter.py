"""Tests for HyperliquidExchangeAdapter — mock SDK to avoid live API calls."""

import sys
import os
import importlib.util
import pytest
from unittest.mock import MagicMock, patch


def _load_hl_adapter(mock_info_cls=None, mock_exchange_cls=None):
    """Load the hyperliquid adapter with mocked SDK modules.

    We inject fake hyperliquid.info.Info and hyperliquid.exchange.Exchange
    before loading the adapter module, so it picks up _SDK_AVAILABLE = True.
    """
    info_mod = MagicMock()
    exchange_mod = MagicMock()
    hl_pkg = MagicMock()

    info_mod.Info = mock_info_cls or MagicMock()
    exchange_mod.Exchange = mock_exchange_cls or MagicMock()

    saved = {}
    for name in ("hyperliquid", "hyperliquid.info", "hyperliquid.exchange"):
        saved[name] = sys.modules.get(name)

    sys.modules["hyperliquid"] = hl_pkg
    sys.modules["hyperliquid.info"] = info_mod
    sys.modules["hyperliquid.exchange"] = exchange_mod

    try:
        adapter_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "adapter.py")
        spec = importlib.util.spec_from_file_location("hl_adapter", adapter_path)
        mod = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(mod)
    finally:
        # Restore original modules even if loading fails
        for name, orig in saved.items():
            if orig is None:
                sys.modules.pop(name, None)
            else:
                sys.modules[name] = orig

    return mod


# ─── Properties ────────────────────────────────────

class TestProperties:
    def test_name(self):
        mock_info = MagicMock()
        mock_info_cls = MagicMock(return_value=mock_info)
        mod = _load_hl_adapter(mock_info_cls=mock_info_cls)
        adapter = mod.HyperliquidExchangeAdapter()
        assert adapter.name == "hyperliquid"

    def test_paper_mode_when_no_secret(self):
        mock_info = MagicMock()
        mock_info_cls = MagicMock(return_value=mock_info)
        with patch.dict(os.environ, {}, clear=False):
            os.environ.pop("HYPERLIQUID_SECRET_KEY", None)
            mod = _load_hl_adapter(mock_info_cls=mock_info_cls)
            adapter = mod.HyperliquidExchangeAdapter()
            assert adapter.mode == "paper"
            assert adapter.is_live is False

    def test_sdk_not_available_raises(self):
        mock_info_cls = MagicMock()
        mod = _load_hl_adapter(mock_info_cls=mock_info_cls)
        mod._SDK_AVAILABLE = False
        with pytest.raises(ImportError, match="hyperliquid-python-sdk"):
            mod.HyperliquidExchangeAdapter()


# ─── Market Data ───────────────────────────────────

class TestMarketData:
    def _make_adapter(self):
        mock_info = MagicMock()
        mock_info_cls = MagicMock(return_value=mock_info)
        mod = _load_hl_adapter(mock_info_cls=mock_info_cls)
        adapter = mod.HyperliquidExchangeAdapter()
        return adapter, mock_info

    def test_get_spot_price_found(self):
        adapter, mock_info = self._make_adapter()
        mock_info.all_mids.return_value = {"BTC": "67500.50"}
        assert adapter.get_spot_price("BTC") == 67500.50

    def test_get_spot_price_fallback_perp_suffix(self):
        adapter, mock_info = self._make_adapter()
        mock_info.all_mids.return_value = {"BTC-PERP": "67000.00"}
        assert adapter.get_spot_price("BTC") == 67000.00

    def test_get_spot_price_not_found(self):
        adapter, mock_info = self._make_adapter()
        mock_info.all_mids.return_value = {}
        assert adapter.get_spot_price("XYZ") == 0.0

    def test_get_ohlcv(self):
        adapter, mock_info = self._make_adapter()
        mock_info.candles_snapshot.return_value = [
            {"T": 1700000000000, "o": "100", "h": "110", "l": "90", "c": "105", "v": "50"},
            {"T": 1700003600000, "o": "105", "h": "115", "l": "95", "c": "110", "v": "60"},
        ]
        result = adapter.get_ohlcv("BTC", "1h", 2)
        assert len(result) == 2
        assert result[0] == [1700000000000, 100.0, 110.0, 90.0, 105.0, 50.0]

    def test_get_ohlcv_uses_t_key_fallback(self):
        adapter, mock_info = self._make_adapter()
        mock_info.candles_snapshot.return_value = [
            {"t": 1700000000000, "o": "100", "h": "110", "l": "90", "c": "105", "v": "50"},
        ]
        result = adapter.get_ohlcv("BTC", "1h", 1)
        assert result[0][0] == 1700000000000

    def test_get_funding_rate_found(self):
        adapter, mock_info = self._make_adapter()
        mock_info.meta_and_asset_ctxs.return_value = [
            {"universe": [{"name": "BTC"}, {"name": "ETH"}]},
            [{"funding": "0.0001"}, {"funding": "0.0002"}],
        ]
        rate = adapter.get_funding_rate("BTC")
        assert rate == 0.0001

    def test_get_funding_rate_not_found(self):
        adapter, mock_info = self._make_adapter()
        mock_info.meta_and_asset_ctxs.return_value = [
            {"universe": [{"name": "ETH"}]},
            [{"funding": "0.0002"}],
        ]
        assert adapter.get_funding_rate("BTC") == 0.0

    def test_get_funding_rate_on_error(self):
        adapter, mock_info = self._make_adapter()
        mock_info.meta_and_asset_ctxs.side_effect = Exception("API down")
        assert adapter.get_funding_rate("BTC") == 0.0

    def test_get_funding_history(self):
        adapter, mock_info = self._make_adapter()
        mock_info.funding_history.return_value = [
            {"fundingRate": "0.0001", "time": 1700000000000},
            {"fundingRate": "0.0002", "time": 1700003600000},
        ]
        result = adapter.get_funding_history("BTC", days=7)
        assert len(result) == 2
        assert result[0] == {"rate": 0.0001, "time": 1700000000000}

    def test_get_funding_history_on_error(self):
        adapter, mock_info = self._make_adapter()
        mock_info.funding_history.side_effect = Exception("fail")
        assert adapter.get_funding_history("BTC") == []


# ─── Account Data ──────────────────────────────────

class TestAccountData:
    def _make_adapter_with_address(self):
        mock_info = MagicMock()
        mock_info_cls = MagicMock(return_value=mock_info)
        mod = _load_hl_adapter(mock_info_cls=mock_info_cls)
        adapter = mod.HyperliquidExchangeAdapter()
        adapter._account_address = "0xABC123"
        return adapter, mock_info

    def test_get_open_positions(self):
        adapter, mock_info = self._make_adapter_with_address()
        mock_info.user_state.return_value = {
            "assetPositions": [
                {"position": {"coin": "BTC", "szi": "0.5", "entryPx": "67000", "unrealizedPnl": "100.50"}},
                {"position": {"coin": "ETH", "szi": "0", "entryPx": "3500", "unrealizedPnl": "0"}},
            ]
        }
        positions = adapter.get_open_positions()
        assert len(positions) == 1
        assert positions[0]["coin"] == "BTC"
        assert positions[0]["size"] == 0.5
        assert positions[0]["entry_price"] == 67000.0

    def test_get_open_positions_no_address(self):
        mock_info = MagicMock()
        mock_info_cls = MagicMock(return_value=mock_info)
        mod = _load_hl_adapter(mock_info_cls=mock_info_cls)
        adapter = mod.HyperliquidExchangeAdapter()
        adapter._account_address = ""
        assert adapter.get_open_positions() == []

    def test_get_open_positions_on_error(self):
        adapter, mock_info = self._make_adapter_with_address()
        mock_info.user_state.side_effect = Exception("fail")
        assert adapter.get_open_positions() == []


# ─── Order Execution ──────────────────────────────

class TestOrderExecution:
    def test_market_open_paper_mode_raises(self):
        mock_info = MagicMock()
        mock_info_cls = MagicMock(return_value=mock_info)
        mod = _load_hl_adapter(mock_info_cls=mock_info_cls)
        adapter = mod.HyperliquidExchangeAdapter()
        assert adapter._exchange is None
        with pytest.raises(RuntimeError, match="live mode"):
            adapter.market_open("BTC", True, 0.5)

    def test_market_close_paper_mode_raises(self):
        mock_info = MagicMock()
        mock_info_cls = MagicMock(return_value=mock_info)
        mod = _load_hl_adapter(mock_info_cls=mock_info_cls)
        adapter = mod.HyperliquidExchangeAdapter()
        with pytest.raises(RuntimeError, match="live mode"):
            adapter.market_close("BTC")

    def test_market_open_live_mode(self):
        mock_info = MagicMock()
        mock_info.asset_to_sz_decimals = {"BTC": 4}
        mock_info_cls = MagicMock(return_value=mock_info)
        mock_exchange = MagicMock()
        mock_exchange.market_open.return_value = {"status": "ok"}
        mod = _load_hl_adapter(mock_info_cls=mock_info_cls)
        adapter = mod.HyperliquidExchangeAdapter()
        # Simulate live mode
        adapter._exchange = mock_exchange
        adapter._info = mock_info

        result = adapter.market_open("BTC", True, 0.5)
        assert result == {"status": "ok"}
        mock_exchange.market_open.assert_called_once_with("BTC", True, 0.5, None, 0.01)

    def test_market_open_size_rounded_to_zero_raises(self):
        mock_info = MagicMock()
        mock_info.asset_to_sz_decimals = {"BTC": 0}
        mock_info_cls = MagicMock(return_value=mock_info)
        mock_exchange = MagicMock()
        mod = _load_hl_adapter(mock_info_cls=mock_info_cls)
        adapter = mod.HyperliquidExchangeAdapter()
        adapter._exchange = mock_exchange
        adapter._info = mock_info

        with pytest.raises(ValueError, match="Size rounded to zero"):
            adapter.market_open("BTC", True, 0.4)

    def test_market_close_live_mode(self):
        mock_info = MagicMock()
        mock_info_cls = MagicMock(return_value=mock_info)
        mock_exchange = MagicMock()
        mock_exchange.market_close.return_value = {"status": "closed"}
        mod = _load_hl_adapter(mock_info_cls=mock_info_cls)
        adapter = mod.HyperliquidExchangeAdapter()
        adapter._exchange = mock_exchange

        result = adapter.market_close("BTC")
        assert result == {"status": "closed"}
        mock_exchange.market_close.assert_called_once_with("BTC")
