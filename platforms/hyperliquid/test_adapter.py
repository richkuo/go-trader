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
        mock_exchange.market_close.assert_called_once_with("BTC", None)

    def test_market_close_partial_size_passes_sz(self):
        mock_info = MagicMock()
        mock_info_cls = MagicMock(return_value=mock_info)
        mock_exchange = MagicMock()
        mock_exchange.market_close.return_value = {"status": "closed"}
        mod = _load_hl_adapter(mock_info_cls=mock_info_cls)
        adapter = mod.HyperliquidExchangeAdapter()
        adapter._exchange = mock_exchange

        adapter.market_close("ETH", 0.25)
        mock_exchange.market_close.assert_called_once_with("ETH", 0.25)


# ─── Stop Loss / Trigger Orders (#412 / #421) ──────

class TestStopLossPlacement:
    """Coverage for place_stop_loss + cancel_trigger_order added in #412 and
    refined for tick-size rules in #421 review point 5."""

    def _live_adapter(self, sz_decimals=None):
        mock_info = MagicMock()
        mock_info.asset_to_sz_decimals = sz_decimals or {"BTC": 5, "ETH": 4}
        mock_info_cls = MagicMock(return_value=mock_info)
        mod = _load_hl_adapter(mock_info_cls=mock_info_cls)
        adapter = mod.HyperliquidExchangeAdapter()
        mock_exchange = MagicMock()
        adapter._exchange = mock_exchange
        adapter._info = mock_info
        return adapter, mock_exchange, mod

    def test_place_stop_loss_paper_mode_raises(self):
        mock_info = MagicMock()
        mock_info_cls = MagicMock(return_value=mock_info)
        mod = _load_hl_adapter(mock_info_cls=mock_info_cls)
        adapter = mod.HyperliquidExchangeAdapter()
        with pytest.raises(RuntimeError, match="live mode"):
            adapter.place_stop_loss("BTC", 0.01, 60000, False)

    def test_place_stop_loss_long_uses_sell_with_lower_limit(self):
        adapter, ex, _ = self._live_adapter()
        ex.order.return_value = {"status": "ok"}
        adapter.place_stop_loss("ETH", 0.5, 3000.0, is_buy=False, limit_slippage_pct=5.0)
        args, kwargs = ex.order.call_args
        sym, is_buy, sz, limit_px, order_type = args
        assert sym == "ETH"
        assert is_buy is False
        # limit_px is below trigger_px for a sell-stop
        assert limit_px < 3000.0
        assert kwargs == {"reduce_only": True}
        assert order_type["trigger"]["tpsl"] == "sl"
        assert order_type["trigger"]["isMarket"] is True

    def test_place_stop_loss_short_uses_buy_with_higher_limit(self):
        adapter, ex, _ = self._live_adapter()
        ex.order.return_value = {"status": "ok"}
        adapter.place_stop_loss("ETH", 0.5, 3000.0, is_buy=True, limit_slippage_pct=5.0)
        _, _, _, limit_px, _ = ex.order.call_args.args
        # limit_px is above trigger_px for a buy-stop
        assert limit_px > 3000.0

    def test_place_stop_loss_size_rounds_to_zero_raises(self):
        adapter, _, _ = self._live_adapter(sz_decimals={"BTC": 0})
        with pytest.raises(ValueError, match="rounded to zero"):
            adapter.place_stop_loss("BTC", 0.4, 60000, False)

    def test_place_stop_loss_invalid_trigger_px_raises(self):
        adapter, _, _ = self._live_adapter()
        with pytest.raises(ValueError, match="trigger_px must be > 0"):
            adapter.place_stop_loss("BTC", 0.01, 0, False)

    def test_place_stop_loss_high_priced_asset_uses_per_asset_px_decimals(self):
        # BTC has sz_decimals=5 → px_decimals = 6 - 5 = 1. Fixed-6-decimal
        # rounding (the pre-#421 behavior) would produce e.g. 60000.000000;
        # the new logic must round to ≤1 decimal, capped at 5 sig figs.
        adapter, ex, mod = self._live_adapter(sz_decimals={"BTC": 5})
        ex.order.return_value = {"status": "ok"}
        # Use a trigger_px that would produce extra decimals after the
        # slip-band multiplication. trigger_px = 63123.456 → after rounding
        # to px_decimals=1 we expect 63123.5 (but capped at 5 sig figs to 63120).
        adapter.place_stop_loss("BTC", 0.001, 63123.456, is_buy=False)
        _, _, _, limit_px, order_type = ex.order.call_args.args
        # 5-sig-fig rounding for ~63000 → tens place (63120 or 63130).
        assert limit_px == round(limit_px, 0) or limit_px == round(limit_px, -1)
        assert order_type["trigger"]["triggerPx"] == round(order_type["trigger"]["triggerPx"], 0) or \
               order_type["trigger"]["triggerPx"] == round(order_type["trigger"]["triggerPx"], -1)

    def test_round_perps_px_high_price(self):
        # Direct unit test of the helper. BTC at $63500 with sz_decimals=5
        # → px_decimals=1, but 5 sig fig cap takes priority and rounds to
        # tens place.
        from importlib import reload  # noqa: F401
        mod = _load_hl_adapter(mock_info_cls=MagicMock(return_value=MagicMock()))
        # 63123.456 with 5 sig figs → 63123 (decimals=0)
        assert mod._round_perps_px(63123.456, sz_decimals=5) == 63123
        # 0.123456 with 5 sig figs and sz_decimals=2 → px_decimals=4, sig_decimals=5
        # → use min(4, 5) = 4 decimals → 0.1235
        assert mod._round_perps_px(0.123456, sz_decimals=2) == 0.1235
        # Edge case: zero / negative passes through unchanged.
        assert mod._round_perps_px(0, sz_decimals=5) == 0
        assert mod._round_perps_px(-1.5, sz_decimals=5) == -1.5

    def test_round_perps_trigger_px_matches_internal_helper(self):
        # Public wrapper must produce the same value place_stop_loss would
        # submit, so callers can record the post-rounding price for PnL.
        adapter, _, mod = self._live_adapter(sz_decimals={"BTC": 5})
        assert adapter.round_perps_trigger_px("BTC", 63123.456) == mod._round_perps_px(63123.456, 5)
        # Idempotent — rounding a rounded value returns the same value.
        rounded = adapter.round_perps_trigger_px("BTC", 63123.456)
        assert adapter.round_perps_trigger_px("BTC", rounded) == rounded

    def test_cancel_trigger_order_paper_mode_raises(self):
        mock_info = MagicMock()
        mock_info_cls = MagicMock(return_value=mock_info)
        mod = _load_hl_adapter(mock_info_cls=mock_info_cls)
        adapter = mod.HyperliquidExchangeAdapter()
        with pytest.raises(RuntimeError, match="live mode"):
            adapter.cancel_trigger_order("BTC", 12345)

    def test_cancel_trigger_order_passes_int_oid(self):
        adapter, ex, _ = self._live_adapter()
        ex.cancel.return_value = {"status": "ok"}
        adapter.cancel_trigger_order("BTC", "12345")
        ex.cancel.assert_called_once_with("BTC", 12345)
