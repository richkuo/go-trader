
import sys
import os
import importlib.util
import pytest
from unittest.mock import MagicMock, patch


def _load_hl_adapter(mock_info_cls=None, mock_exchange_cls=None, mock_api_cls=None):
    info_mod = MagicMock()
    exchange_mod = MagicMock()
    api_mod = MagicMock()
    utils_pkg = MagicMock()
    error_mod = MagicMock()
    hl_pkg = MagicMock()

    info_mod.Info = mock_info_cls or MagicMock()
    exchange_mod.Exchange = mock_exchange_cls or MagicMock()
    api_mod.API = mock_api_cls or MagicMock()

    class _StubClientError(Exception):
        def __init__(self, status_code=None, *a, **kw):
            super().__init__(*a, **kw)
            self.status_code = status_code
    error_mod.ClientError = _StubClientError

    saved = {}
    mod_names = (
        "hyperliquid",
        "hyperliquid.info",
        "hyperliquid.exchange",
        "hyperliquid.api",
        "hyperliquid.utils",
        "hyperliquid.utils.error",
    )
    for name in mod_names:
        saved[name] = sys.modules.get(name)

    sys.modules["hyperliquid"] = hl_pkg
    sys.modules["hyperliquid.info"] = info_mod
    sys.modules["hyperliquid.exchange"] = exchange_mod
    sys.modules["hyperliquid.api"] = api_mod
    sys.modules["hyperliquid.utils"] = utils_pkg
    sys.modules["hyperliquid.utils.error"] = error_mod

    try:
        adapter_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "adapter.py")
        spec = importlib.util.spec_from_file_location("hl_adapter", adapter_path)
        mod = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(mod)
    finally:
        for name, orig in saved.items():
            if orig is None:
                sys.modules.pop(name, None)
            else:
                sys.modules[name] = orig

    return mod


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

    def test_get_ohlcv(self, monkeypatch):
        monkeypatch.setenv("GO_TRADER_HL_OHLCV_CACHE", "0")
        adapter, mock_info = self._make_adapter()
        mock_info.candles_snapshot.return_value = [
            {"T": 1700000000000, "o": "100", "h": "110", "l": "90", "c": "105", "v": "50"},
            {"T": 1700003600000, "o": "105", "h": "115", "l": "95", "c": "110", "v": "60"},
        ]
        result = adapter.get_ohlcv("BTC", "1h", 2)
        assert len(result) == 2
        assert result[0] == [1700000000000, 100.0, 110.0, 90.0, 105.0, 50.0]

    def test_get_ohlcv_uses_t_key_fallback(self, monkeypatch):
        monkeypatch.setenv("GO_TRADER_HL_OHLCV_CACHE", "0")
        adapter, mock_info = self._make_adapter()
        mock_info.candles_snapshot.return_value = [
            {"t": 1700000000000, "o": "100", "h": "110", "l": "90", "c": "105", "v": "50"},
        ]
        result = adapter.get_ohlcv("BTC", "1h", 1)
        assert result[0][0] == 1700000000000

    def test_get_ohlcv_widens_window_for_gap_margin(self, monkeypatch):
        monkeypatch.setenv("GO_TRADER_HL_OHLCV_CACHE", "0")
        adapter, mock_info = self._make_adapter()
        mock_info.candles_snapshot.return_value = []
        adapter.get_ohlcv("BTC", "1h", 200)
        _name, interval, start_ms, end_ms = mock_info.candles_snapshot.call_args[0]
        assert interval == "1h"
        assert end_ms - start_ms == 3_600_000 * (200 + 50)

    def test_get_ohlcv_trims_to_limit_when_api_returns_extra(self, monkeypatch):
        monkeypatch.setenv("GO_TRADER_HL_OHLCV_CACHE", "0")
        adapter, mock_info = self._make_adapter()
        mock_info.candles_snapshot.return_value = [
            {"T": 1700000000000 + i * 3_600_000, "o": "100", "h": "110", "l": "90",
             "c": str(100 + i), "v": "50"}
            for i in range(5)
        ]
        result = adapter.get_ohlcv("BTC", "1h", 3)
        assert len(result) == 3
        assert result[0][4] == 102.0
        assert result[-1][4] == 104.0

    def test_get_ohlcv_warns_on_shortfall_below_limit(self, monkeypatch, capsys):
        monkeypatch.setenv("GO_TRADER_HL_OHLCV_CACHE", "0")
        adapter, mock_info = self._make_adapter()
        mock_info.candles_snapshot.return_value = [
            {"T": 1700000000000 + i * 3_600_000, "o": "100", "h": "110", "l": "90",
             "c": str(100 + i), "v": "50"}
            for i in range(3)
        ]
        result = adapter.get_ohlcv("BTC", "1h", 200)
        assert len(result) == 3
        assert mock_info.candles_snapshot.call_count == 3
        err = capsys.readouterr().err
        assert "shortfall" in err
        assert "3 of 200" in err

    def test_get_ohlcv_no_shortfall_warning_when_full(self, monkeypatch, capsys):
        monkeypatch.setenv("GO_TRADER_HL_OHLCV_CACHE", "0")
        adapter, mock_info = self._make_adapter()
        mock_info.candles_snapshot.return_value = [
            {"T": 1700000000000 + i * 3_600_000, "o": "100", "h": "110", "l": "90",
             "c": str(100 + i), "v": "50"}
            for i in range(3)
        ]
        adapter.get_ohlcv("BTC", "1h", 3)
        assert "shortfall" not in capsys.readouterr().err

    @staticmethod
    def _gappy_candles(n):
        return [
            {"T": 1700000000000 + i * 3_600_000, "o": "100", "h": "110",
             "l": "90", "c": str(100 + i), "v": "50"}
            for i in range(n)
        ]

    def test_get_ohlcv_extends_window_until_limit_reached(self, monkeypatch):
        monkeypatch.setenv("GO_TRADER_HL_OHLCV_CACHE", "0")
        adapter, mock_info = self._make_adapter()

        def side_effect(symbol, interval, start_ms, end_ms):
            span = round((end_ms - start_ms) / 3_600_000)
            return self._gappy_candles(int(span * 0.6))

        mock_info.candles_snapshot.side_effect = side_effect
        result = adapter.get_ohlcv("BTC", "1h", 200)
        assert len(result) == 200
        assert mock_info.candles_snapshot.call_count == 2
        spans = [round((c.args[3] - c.args[2]) / 3_600_000)
                 for c in mock_info.candles_snapshot.call_args_list]
        assert spans == [250, 500]

    def test_get_ohlcv_stops_extending_on_history_plateau_and_warns(self, monkeypatch, capsys):
        monkeypatch.setenv("GO_TRADER_HL_OHLCV_CACHE", "0")
        adapter, mock_info = self._make_adapter()
        available = 150

        def side_effect(symbol, interval, start_ms, end_ms):
            span = round((end_ms - start_ms) / 3_600_000)
            return self._gappy_candles(min(int(span * 0.6), available))

        mock_info.candles_snapshot.side_effect = side_effect
        result = adapter.get_ohlcv("BTC", "1h", 200)
        assert mock_info.candles_snapshot.call_count == 3
        assert len(result) == 150
        err = capsys.readouterr().err
        assert "shortfall" in err
        assert "150 of 200" in err

    def test_get_ohlcv_does_not_stop_on_single_interior_gap_widen(self, monkeypatch):
        monkeypatch.setenv("GO_TRADER_HL_OHLCV_CACHE", "0")
        adapter, mock_info = self._make_adapter()
        returns = iter([150, 150, 250])
        mock_info.candles_snapshot.side_effect = (
            lambda *a, **kw: self._gappy_candles(next(returns))
        )
        result = adapter.get_ohlcv("BTC", "1h", 200)
        assert mock_info.candles_snapshot.call_count == 3
        assert len(result) == 200

    def test_get_ohlcv_extend_passes_are_bounded(self, monkeypatch):
        monkeypatch.setenv("GO_TRADER_HL_OHLCV_CACHE", "0")
        adapter, mock_info = self._make_adapter()
        counter = {"n": 1}

        def side_effect(symbol, interval, start_ms, end_ms):
            n = counter["n"]
            counter["n"] += 1
            return self._gappy_candles(n)

        mock_info.candles_snapshot.side_effect = side_effect
        adapter.get_ohlcv("BTC", "1h", 10_000)
        max_passes = type(adapter).get_ohlcv.__globals__["OHLCV_MAX_EXTEND_PASSES"]
        assert mock_info.candles_snapshot.call_count == max_passes

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
        adapter._wallet = MagicMock()
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
        adapter._wallet = MagicMock()
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
        adapter._wallet = MagicMock()
        adapter._exchange = mock_exchange

        result = adapter.market_close("BTC")
        assert result == {"status": "closed"}
        mock_exchange.market_close.assert_called_once_with("BTC", None)

    def test_market_close_partial_size_rounds_to_sz_decimals(self):
        mock_info = MagicMock()
        mock_info.asset_to_sz_decimals = {"ETH": 4}
        mock_info_cls = MagicMock(return_value=mock_info)
        mock_exchange = MagicMock()
        mock_exchange.market_close.return_value = {"status": "closed"}
        mod = _load_hl_adapter(mock_info_cls=mock_info_cls)
        adapter = mod.HyperliquidExchangeAdapter()
        adapter._wallet = MagicMock()
        adapter._exchange = mock_exchange
        adapter._info = mock_info

        adapter.market_close("ETH", 0.2509645272613055)
        mock_exchange.market_close.assert_called_once_with("ETH", 0.251)

    def test_market_close_full_close_passes_none_unchanged(self):
        mock_info = MagicMock()
        mock_info.asset_to_sz_decimals = {"BTC": 5}
        mock_info_cls = MagicMock(return_value=mock_info)
        mock_exchange = MagicMock()
        mock_exchange.market_close.return_value = {"status": "closed"}
        mod = _load_hl_adapter(mock_info_cls=mock_info_cls)
        adapter = mod.HyperliquidExchangeAdapter()
        adapter._wallet = MagicMock()
        adapter._exchange = mock_exchange
        adapter._info = mock_info

        adapter.market_close("BTC")
        mock_exchange.market_close.assert_called_once_with("BTC", None)

    def test_market_close_partial_size_rounded_to_zero_raises(self):
        mock_info = MagicMock()
        mock_info.asset_to_sz_decimals = {"BTC": 0}
        mock_info_cls = MagicMock(return_value=mock_info)
        mock_exchange = MagicMock()
        mod = _load_hl_adapter(mock_info_cls=mock_info_cls)
        adapter = mod.HyperliquidExchangeAdapter()
        adapter._wallet = MagicMock()
        adapter._exchange = mock_exchange
        adapter._info = mock_info

        with pytest.raises(ValueError, match="Size rounded to zero"):
            adapter.market_close("BTC", 0.4)
        mock_exchange.market_close.assert_not_called()


class TestStopLossPlacement:

    def _live_adapter(self, sz_decimals=None):
        mock_info = MagicMock()
        mock_info.asset_to_sz_decimals = sz_decimals or {"BTC": 5, "ETH": 4}
        mock_info_cls = MagicMock(return_value=mock_info)
        mod = _load_hl_adapter(mock_info_cls=mock_info_cls)
        adapter = mod.HyperliquidExchangeAdapter()
        mock_exchange = MagicMock()
        adapter._wallet = MagicMock()
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
        assert limit_px < 3000.0
        assert kwargs == {"reduce_only": True}
        assert order_type["trigger"]["tpsl"] == "sl"
        assert order_type["trigger"]["isMarket"] is True

    def test_place_stop_loss_short_uses_buy_with_higher_limit(self):
        adapter, ex, _ = self._live_adapter()
        ex.order.return_value = {"status": "ok"}
        adapter.place_stop_loss("ETH", 0.5, 3000.0, is_buy=True, limit_slippage_pct=5.0)
        _, _, _, limit_px, _ = ex.order.call_args.args
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
        adapter, ex, mod = self._live_adapter(sz_decimals={"BTC": 5})
        ex.order.return_value = {"status": "ok"}
        adapter.place_stop_loss("BTC", 0.001, 63123.456, is_buy=False)
        _, _, _, limit_px, order_type = ex.order.call_args.args
        assert limit_px == round(limit_px, 0) or limit_px == round(limit_px, -1)
        assert order_type["trigger"]["triggerPx"] == round(order_type["trigger"]["triggerPx"], 0) or \
               order_type["trigger"]["triggerPx"] == round(order_type["trigger"]["triggerPx"], -1)

    def test_round_perps_px_high_price(self):
        from importlib import reload
        mod = _load_hl_adapter(mock_info_cls=MagicMock(return_value=MagicMock()))
        assert mod._round_perps_px(63123.456, sz_decimals=5) == 63123
        assert mod._round_perps_px(0.123456, sz_decimals=2) == 0.1235
        assert mod._round_perps_px(0, sz_decimals=5) == 0
        assert mod._round_perps_px(-1.5, sz_decimals=5) == -1.5

    def test_round_perps_trigger_px_matches_internal_helper(self):
        adapter, _, mod = self._live_adapter(sz_decimals={"BTC": 5})
        assert adapter.round_perps_trigger_px("BTC", 63123.456) == mod._round_perps_px(63123.456, 5)
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

    def test_place_take_profit_limit_uses_reduce_only_limit(self):
        adapter, ex, _ = self._live_adapter(sz_decimals={"ETH": 4})
        ex.order.return_value = {"status": "ok"}
        adapter.place_take_profit_limit("ETH", 0.123456, 3100.0, is_buy=False)
        sym, is_buy, sz, limit_px, order_type = ex.order.call_args.args
        assert sym == "ETH"
        assert is_buy is False
        assert sz == 0.1234
        assert limit_px == 3100.0
        assert order_type == {"limit": {"tif": "Gtc"}}
        assert ex.order.call_args.kwargs == {"reduce_only": True}

    def test_open_order_oids_filters_by_symbol(self):
        adapter, _, _ = self._live_adapter()
        adapter._account_address = "0xabc"
        adapter._info.open_orders.return_value = [
            {"coin": "ETH", "oid": 111},
            {"coin": "BTC", "oid": 222},
            {"coin": "ETH", "oid": "333"},
        ]
        assert adapter.open_order_oids("ETH") == {111, 333}


class TestLookupFillFeeByOID:
    def _make_adapter(self):
        mock_info = MagicMock()
        mock_info_cls = MagicMock(return_value=mock_info)
        mod = _load_hl_adapter(mock_info_cls=mock_info_cls)
        adapter = mod.HyperliquidExchangeAdapter()
        adapter._account_address = "0xABC123"
        return adapter, mock_info

    def test_returns_empty_when_no_address(self):
        mock_info = MagicMock()
        mock_info_cls = MagicMock(return_value=mock_info)
        mod = _load_hl_adapter(mock_info_cls=mock_info_cls)
        adapter = mod.HyperliquidExchangeAdapter()
        adapter._account_address = ""
        assert adapter.lookup_fill_fee_by_oid(123, since_ms=0) == {}
        mock_info.user_fills_by_time.assert_not_called()

    def test_aggregates_fee_and_pnl_across_partial_fills(self):
        adapter, mock_info = self._make_adapter()
        mock_info.user_fills_by_time.return_value = [
            {"oid": 100, "fee": "0.50", "closedPnl": "1.25"},
            {"oid": 100, "fee": "0.30", "closedPnl": "0.75"},
            {"oid": 999, "fee": "5.00", "closedPnl": "10.00"},
        ]
        result = adapter.lookup_fill_fee_by_oid(100, since_ms=1000)
        assert result["fee"] == pytest.approx(0.80)
        assert result["closed_pnl"] == pytest.approx(2.00)
        assert result["count"] == 2

    def test_handles_string_oid_in_response(self):
        adapter, mock_info = self._make_adapter()
        mock_info.user_fills_by_time.return_value = [
            {"oid": "100", "fee": "0.42", "closedPnl": "0"},
        ]
        result = adapter.lookup_fill_fee_by_oid(100, since_ms=1000)
        assert result["fee"] == pytest.approx(0.42)
        assert result["count"] == 1

    def test_retries_until_indexer_catches_up(self, monkeypatch):
        adapter, mock_info = self._make_adapter()
        mock_info.user_fills_by_time.side_effect = [
            [],
            [{"oid": 999, "fee": "1", "closedPnl": "0"}],
            [{"oid": 100, "fee": "0.65", "closedPnl": "0"}],
        ]
        sleeps = []
        monkeypatch.setattr("time.sleep", lambda s: sleeps.append(s))
        result = adapter.lookup_fill_fee_by_oid(100, since_ms=1000, max_retries=4, retry_delay_s=0.1)
        assert result["fee"] == pytest.approx(0.65)
        assert mock_info.user_fills_by_time.call_count == 3
        assert sleeps == [0.1, 0.1]

    def test_returns_empty_after_max_retries_exhausted(self, monkeypatch):
        adapter, mock_info = self._make_adapter()
        mock_info.user_fills_by_time.return_value = []
        monkeypatch.setattr("time.sleep", lambda s: None)
        result = adapter.lookup_fill_fee_by_oid(100, since_ms=1000, max_retries=3, retry_delay_s=0.0)
        assert result == {}
        assert mock_info.user_fills_by_time.call_count == 3

    def test_swallows_sdk_exceptions_and_retries(self, monkeypatch):
        adapter, mock_info = self._make_adapter()
        mock_info.user_fills_by_time.side_effect = [
            Exception("network blip"),
            [{"oid": 100, "fee": "0.10", "closedPnl": "0"}],
        ]
        monkeypatch.setattr("time.sleep", lambda s: None)
        result = adapter.lookup_fill_fee_by_oid(100, since_ms=1000, max_retries=4, retry_delay_s=0.0)
        assert result["fee"] == pytest.approx(0.10)

    def test_treats_non_list_response_as_no_match(self, monkeypatch):
        adapter, mock_info = self._make_adapter()
        mock_info.user_fills_by_time.return_value = {"unexpected": "shape"}
        monkeypatch.setattr("time.sleep", lambda s: None)
        result = adapter.lookup_fill_fee_by_oid(100, since_ms=1000, max_retries=2, retry_delay_s=0.0)
        assert result == {}


class TestLimitOpen:

    def _make_adapter(self, sz_decimals=None):
        mock_info = MagicMock()
        mock_info.asset_to_sz_decimals = sz_decimals or {"BTC": 4, "ETH": 4}
        mock_info_cls = MagicMock(return_value=mock_info)
        mock_exchange = MagicMock()
        mock_exchange.order.return_value = {"status": "ok"}
        mod = _load_hl_adapter(mock_info_cls=mock_info_cls)
        adapter = mod.HyperliquidExchangeAdapter()
        adapter._wallet = MagicMock()
        adapter._exchange = mock_exchange
        adapter._info = mock_info
        return adapter, mock_exchange

    def test_paper_mode_raises(self):
        mod = _load_hl_adapter()
        adapter = mod.HyperliquidExchangeAdapter()
        adapter._exchange = None
        with pytest.raises(RuntimeError, match="limit_open requires live mode"):
            adapter.limit_open("BTC", True, 0.01, 58000)

    def test_alo_post_only_non_reduce_only(self):
        adapter, mock_exchange = self._make_adapter()
        adapter.limit_open("ETH", True, 0.5, 3000.0)
        args, kwargs = mock_exchange.order.call_args
        assert args[0] == "ETH"
        assert args[1] is True
        assert args[2] == 0.5
        assert args[4] == {"limit": {"tif": "Alo"}}
        assert kwargs.get("reduce_only") is False

    def test_gtc_tif_passthrough(self):
        adapter, mock_exchange = self._make_adapter()
        adapter.limit_open("ETH", False, 0.5, 3000.0, tif="Gtc")
        _, _, _, _, order_type = mock_exchange.order.call_args[0]
        assert order_type == {"limit": {"tif": "Gtc"}}

    def test_invalid_limit_px_raises(self):
        adapter, _ = self._make_adapter()
        with pytest.raises(ValueError, match="limit_px must be > 0"):
            adapter.limit_open("ETH", True, 0.5, 0)

    def test_bad_tif_raises(self):
        adapter, _ = self._make_adapter()
        with pytest.raises(ValueError, match="unsupported tif"):
            adapter.limit_open("ETH", True, 0.5, 3000.0, tif="Fok")

    def test_size_rounded_to_zero_raises(self):
        adapter, _ = self._make_adapter(sz_decimals={"BTC": 0})
        with pytest.raises(ValueError, match="Size rounded to zero"):
            adapter.limit_open("BTC", True, 0.4, 58000)


class TestFillsSummaryByOID:

    def _make_adapter(self):
        mock_info = MagicMock()
        mock_info_cls = MagicMock(return_value=mock_info)
        mod = _load_hl_adapter(mock_info_cls=mock_info_cls)
        adapter = mod.HyperliquidExchangeAdapter()
        adapter._account_address = "0xABC123"
        return adapter, mock_info

    def test_no_address_returns_empty(self):
        mod = _load_hl_adapter()
        adapter = mod.HyperliquidExchangeAdapter()
        adapter._account_address = ""
        assert adapter.fills_summary_by_oid(100, since_ms=0) == {}

    def test_sums_size_and_size_weighted_vwap(self):
        adapter, mock_info = self._make_adapter()
        mock_info.user_fills_by_time.return_value = [
            {"oid": 100, "sz": "0.4", "px": "2000", "fee": "0.20"},
            {"oid": 100, "sz": "0.6", "px": "2010", "fee": "0.30"},
            {"oid": 999, "sz": "5", "px": "1", "fee": "9"},
        ]
        out = adapter.fills_summary_by_oid(100, since_ms=1000)
        assert out["filled_size"] == pytest.approx(1.0)
        assert out["fee"] == pytest.approx(0.50)
        assert out["count"] == 2
        assert out["avg_px"] == pytest.approx(2006.0)

    def test_no_match_returns_empty(self, monkeypatch):
        adapter, mock_info = self._make_adapter()
        mock_info.user_fills_by_time.return_value = [{"oid": 7, "sz": "1", "px": "1", "fee": "0"}]
        monkeypatch.setattr("time.sleep", lambda s: None)
        assert adapter.fills_summary_by_oid(100, since_ms=1000, max_retries=2, retry_delay_s=0.0) == {}

    def test_zero_oid_returns_empty(self):
        adapter, mock_info = self._make_adapter()
        assert adapter.fills_summary_by_oid(0, since_ms=1000) == {}
        mock_info.user_fills_by_time.assert_not_called()


class TestFundingHistoryRange:

    _HOUR = 3_600_000

    def _paged_info(self, total_hours, page_size=500, base_ms=1_700_000_000_000):
        all_records = [
            {"fundingRate": str((i % 7 - 3) * 1e-5), "time": base_ms + i * self._HOUR}
            for i in range(total_hours)
        ]

        def funding_history(symbol, start_time):
            eligible = [r for r in all_records if r["time"] >= start_time]
            return eligible[:page_size]

        mock_info = MagicMock()
        mock_info.funding_history.side_effect = funding_history
        return mock_info, all_records, base_ms

    def _adapter(self, mock_info):
        mod = _load_hl_adapter(mock_info_cls=MagicMock(return_value=mock_info))
        return mod.HyperliquidExchangeAdapter()

    def test_paginates_past_single_page_cap(self):
        mock_info, all_records, base = self._paged_info(total_hours=1200)
        adapter = self._adapter(mock_info)
        out = adapter.get_funding_history_range("BTC", base, base + 1199 * self._HOUR)
        assert len(out) == 1200
        assert out[0]["time"] == base
        assert out[-1]["time"] == base + 1199 * self._HOUR
        assert mock_info.funding_history.call_count >= 3

    def test_single_page_range(self):
        mock_info, _, base = self._paged_info(total_hours=100)
        adapter = self._adapter(mock_info)
        out = adapter.get_funding_history_range("BTC", base)
        assert len(out) == 100
        times = [r["time"] for r in out]
        assert times == sorted(times)
        assert len(set(times)) == len(times), "must dedupe on time"

    def test_end_ms_bound_respected(self):
        mock_info, _, base = self._paged_info(total_hours=1200)
        adapter = self._adapter(mock_info)
        out = adapter.get_funding_history_range("BTC", base, base + 49 * self._HOUR)
        assert len(out) == 50
        assert out[-1]["time"] == base + 49 * self._HOUR

    def test_api_error_returns_empty(self):
        mock_info = MagicMock()
        mock_info.funding_history.side_effect = Exception("boom")
        adapter = self._adapter(mock_info)
        assert adapter.get_funding_history_range("BTC", 0) == []

    def test_no_progress_terminates(self):
        mock_info, all_records, base = self._paged_info(total_hours=10)
        mock_info.funding_history.side_effect = lambda s, t: all_records[:5]
        adapter = self._adapter(mock_info)
        out = adapter.get_funding_history_range("BTC", base, base + 100 * self._HOUR)
        assert len(out) == 5


class TestLazyExchangeInit:
    def _sample_meta(self):
        return (
            {"universe": [{"index": 0, "name": "USDC/USDC", "tokens": [0, 0]}],
             "tokens": [{"name": "USDC", "szDecimals": 0}]},
            {"universe": [{"name": "BTC", "szDecimals": 5}]},
        )

    def _live_adapter_mod(self, monkeypatch, mock_exchange_cls=None):
        spot_meta, meta = self._sample_meta()
        mock_info = MagicMock()
        mock_info.asset_to_sz_decimals = {"BTC": 5}
        mock_info.candles_snapshot.return_value = []
        mock_info_cls = MagicMock(return_value=mock_info)
        exchange_calls = {"n": 0}
        mock_exchange = MagicMock()
        mock_exchange.market_open.return_value = {"status": "ok"}

        def exchange_factory(*args, **kwargs):
            exchange_calls["n"] += 1
            exchange_calls["kwargs"] = kwargs
            return mock_exchange

        mock_exchange_cls = mock_exchange_cls or MagicMock(side_effect=exchange_factory)
        mod = _load_hl_adapter(mock_info_cls=mock_info_cls, mock_exchange_cls=mock_exchange_cls)
        monkeypatch.setenv("HYPERLIQUID_SECRET_KEY", "0x" + "11" * 32)
        monkeypatch.setattr(mod, "_load_meta_cache", lambda *a, **kw: (spot_meta, meta))

        fake_wallet = MagicMock()
        fake_account_mod = MagicMock()
        fake_account_mod.Account.from_key.return_value = fake_wallet
        monkeypatch.setitem(sys.modules, "eth_account", fake_account_mod)

        adapter = mod.HyperliquidExchangeAdapter()
        return mod, adapter, mock_info, mock_exchange, exchange_calls

    def test_init_defers_exchange_when_secret_set(self, monkeypatch):
        mod, adapter, _, _, exchange_calls = self._live_adapter_mod(monkeypatch)
        assert adapter._exchange is None
        assert adapter.is_live is True
        assert exchange_calls["n"] == 0

    def test_get_ohlcv_works_without_exchange(self, monkeypatch):
        _, adapter, mock_info, _, exchange_calls = self._live_adapter_mod(monkeypatch)
        adapter.get_ohlcv("BTC", interval="1h", limit=10)
        mock_info.candles_snapshot.assert_called()
        assert exchange_calls["n"] == 0

    def test_market_open_lazy_inits_exchange_with_cached_meta(self, monkeypatch):
        _, adapter, mock_info, mock_exchange, exchange_calls = self._live_adapter_mod(monkeypatch)
        assert adapter._cached_meta is not None
        adapter._info = mock_info
        adapter.market_open("BTC", True, 0.5)
        assert exchange_calls["n"] == 1
        assert exchange_calls["kwargs"].get("meta", {})["universe"][0]["name"] == "BTC"
        mock_exchange.market_open.assert_called_once()

    def test_exchange_init_429_does_not_fail_adapter_init(self, monkeypatch):
        err = RuntimeError("(429, None, 'null', None)")
        mod, adapter, mock_info, _, _ = self._live_adapter_mod(
            monkeypatch,
            mock_exchange_cls=MagicMock(side_effect=err),
        )
        adapter._info = mock_info
        assert adapter._exchange is None
        with pytest.raises(RuntimeError, match="Failed to initialize Hyperliquid Exchange client"):
            adapter.market_open("BTC", True, 0.5)

    def test_exchange_init_backoff_suppresses_rapid_retries(self, monkeypatch):
        err = RuntimeError("(429, None, 'null', None)")
        mod, adapter, mock_info, _, exchange_calls = self._live_adapter_mod(monkeypatch)

        def exchange_factory(*args, **kwargs):
            exchange_calls["n"] += 1
            raise err

        monkeypatch.setattr(mod, "_HLExchange", MagicMock(side_effect=exchange_factory))
        now = [1_700_000_000.0]
        monkeypatch.setattr(mod.time, "time", lambda: now[0])
        adapter._info = mock_info

        with pytest.raises(RuntimeError, match="Failed to initialize Hyperliquid Exchange client"):
            adapter.market_open("BTC", True, 0.5)
        assert exchange_calls["n"] == 1

        now[0] += 5
        with pytest.raises(RuntimeError, match="Failed to initialize Hyperliquid Exchange client"):
            adapter.market_open("BTC", True, 0.5)
        assert exchange_calls["n"] == 1, "backoff must suppress SDK constructor within 30s"

    def test_exchange_init_retries_after_backoff_expires(self, monkeypatch):
        err = RuntimeError("(429, None, 'null', None)")
        mod, adapter, mock_info, _, exchange_calls = self._live_adapter_mod(monkeypatch)

        def exchange_factory(*args, **kwargs):
            exchange_calls["n"] += 1
            raise err

        monkeypatch.setattr(mod, "_HLExchange", MagicMock(side_effect=exchange_factory))
        now = [1_700_000_000.0]
        monkeypatch.setattr(mod.time, "time", lambda: now[0])
        adapter._info = mock_info

        with pytest.raises(RuntimeError, match="Failed to initialize Hyperliquid Exchange client"):
            adapter.market_open("BTC", True, 0.5)
        assert exchange_calls["n"] == 1

        now[0] += mod._EXCHANGE_INIT_BACKOFF_S + 1
        with pytest.raises(RuntimeError, match="Failed to initialize Hyperliquid Exchange client"):
            adapter.market_open("BTC", True, 0.5)
        assert exchange_calls["n"] == 2, "constructor must retry after backoff window"

    def test_exchange_init_success_after_backoff_uses_cached_exchange(self, monkeypatch):
        mod, adapter, mock_info, mock_exchange, exchange_calls = self._live_adapter_mod(monkeypatch)
        err = RuntimeError("(429, None, 'null', None)")

        def exchange_factory(*args, **kwargs):
            exchange_calls["n"] += 1
            if exchange_calls["n"] == 1:
                raise err
            return mock_exchange

        adapter._exchange = None
        monkeypatch.setattr(
            mod,
            "_HLExchange",
            MagicMock(side_effect=exchange_factory),
        )
        now = [1_700_000_000.0]
        monkeypatch.setattr(mod.time, "time", lambda: now[0])
        adapter._info = mock_info

        with pytest.raises(RuntimeError, match="Failed to initialize Hyperliquid Exchange client"):
            adapter.market_open("BTC", True, 0.5)
        assert exchange_calls["n"] == 1

        now[0] += mod._EXCHANGE_INIT_BACKOFF_S + 1
        adapter.market_open("BTC", True, 0.5)
        assert exchange_calls["n"] == 2
        assert adapter._exchange is mock_exchange

        adapter.market_open("BTC", True, 0.5)
        assert exchange_calls["n"] == 2, "cached exchange must not re-init"
        assert mock_exchange.market_open.call_count == 2

    def test_concurrent_lazy_init_calls_exchange_once(self, monkeypatch):
        import threading

        _, adapter, mock_info, _, exchange_calls = self._live_adapter_mod(monkeypatch)
        adapter._info = mock_info
        barrier = threading.Barrier(2)
        errors = []

        def worker():
            try:
                barrier.wait(timeout=5)
                adapter.market_open("BTC", True, 0.5)
            except Exception as exc:
                errors.append(exc)

        t1 = threading.Thread(target=worker)
        t2 = threading.Thread(target=worker)
        t1.start()
        t2.start()
        t1.join(timeout=10)
        t2.join(timeout=10)
        assert not errors
        assert exchange_calls["n"] == 1


def _sdk_info(asset_to_sz_decimals_by_index, name_to_coin=None, coin_to_asset=None, name_to_asset_fn=None):
    mock_info = MagicMock()
    mock_info.asset_to_sz_decimals = asset_to_sz_decimals_by_index
    if name_to_coin is None:
        name_to_coin = {sym: sym for sym in coin_to_asset.keys()} if coin_to_asset else {}
    mock_info.name_to_coin = name_to_coin
    mock_info.coin_to_asset = coin_to_asset or {}
    if name_to_asset_fn is not None:
        mock_info.name_to_asset = name_to_asset_fn
    else:
        def _default_name_to_asset(name):
            return coin_to_asset.get(name)
        mock_info.name_to_asset = _default_name_to_asset
    return mock_info


class TestSzDecimalsSdkShape:

    @pytest.mark.parametrize(
        "symbol,sz_by_index,coin_to_asset,expected",
        [
            ("HYPE", {151: 2, 0: 5}, {"HYPE": 151, "BTC": 0}, 2),
            ("BTC", {151: 2, 0: 5}, {"HYPE": 151, "BTC": 0}, 5),
            ("ETH", {1: 4}, {"ETH": 1}, 4),
        ],
    )
    def test_returns_correct_decimals_via_name_to_asset(self, symbol, sz_by_index, coin_to_asset, expected):
        mock_info = _sdk_info(
            asset_to_sz_decimals_by_index=sz_by_index,
            coin_to_asset=coin_to_asset,
        )
        mod = _load_hl_adapter()
        adapter = mod.HyperliquidExchangeAdapter()
        adapter._info = mock_info
        assert adapter._sz_decimals(symbol) == expected

    def test_missing_asset_to_sz_decimals_falls_back_to_default_3(self):
        mock_info = _sdk_info(
            asset_to_sz_decimals_by_index={151: 2},
            coin_to_asset={"HYPE": 151},
        )
        del mock_info.asset_to_sz_decimals
        mod = _load_hl_adapter()
        adapter = mod.HyperliquidExchangeAdapter()
        adapter._info = mock_info
        adapter._sz_decimals_misses.add("HYPE")
        assert adapter._sz_decimals("HYPE") == 3

    def test_resolved_index_absent_from_sz_map_returns_none(self):
        mock_info = _sdk_info(
            asset_to_sz_decimals_by_index={0: 5},
            coin_to_asset={"HYPE": 151},
        )
        mod = _load_hl_adapter()
        assert mod.HyperliquidExchangeAdapter._resolve_sz_decimals(mock_info, "HYPE") is None

    def test_market_open_rounds_hype_size_to_2_decimals_not_3(self):
        mock_info = _sdk_info(
            asset_to_sz_decimals_by_index={151: 2},
            coin_to_asset={"HYPE": 151},
        )
        mock_exchange = MagicMock()
        mock_exchange.market_open.return_value = {"status": "ok"}
        mod = _load_hl_adapter()
        adapter = mod.HyperliquidExchangeAdapter()
        adapter._wallet = MagicMock()
        adapter._exchange = mock_exchange
        adapter._info = mock_info

        adapter.market_open("HYPE", True, 6.137982)
        mock_exchange.market_open.assert_called_once_with("HYPE", True, 6.14, None, 0.01)

    def test_market_close_rounds_hype_size_to_2_decimals(self):
        mock_info = _sdk_info(
            asset_to_sz_decimals_by_index={151: 2},
            coin_to_asset={"HYPE": 151},
        )
        mock_exchange = MagicMock()
        mock_exchange.market_close.return_value = {"status": "closed"}
        mod = _load_hl_adapter()
        adapter = mod.HyperliquidExchangeAdapter()
        adapter._wallet = MagicMock()
        adapter._exchange = mock_exchange
        adapter._info = mock_info

        adapter.market_close("HYPE", 6.137982)
        mock_exchange.market_close.assert_called_once_with("HYPE", 6.14)

    def test_unknown_symbol_falls_through_to_default_3(self):
        mock_info = _sdk_info(
            asset_to_sz_decimals_by_index={0: 5},
            coin_to_asset={"BTC": 0},
        )
        mod = _load_hl_adapter()
        adapter = mod.HyperliquidExchangeAdapter()
        adapter._info = mock_info
        assert adapter._sz_decimals("NOPE") == 3

    def test_name_to_asset_raising_falls_back_to_coin_to_asset(self):
        def _raising(name):
            raise RuntimeError("sdk quirk")

        mock_info = _sdk_info(
            asset_to_sz_decimals_by_index={42: 2},
            coin_to_asset={"HYPE": 42},
            name_to_asset_fn=_raising,
        )
        mod = _load_hl_adapter()
        adapter = mod.HyperliquidExchangeAdapter()
        adapter._info = mock_info
        assert adapter._sz_decimals("HYPE") == 2

    def test_legacy_symbol_keyed_mock_still_works(self):
        mock_info = MagicMock()
        mock_info.asset_to_sz_decimals = {"BTC": 5}
        mock_info.name_to_coin = {}
        mock_info.coin_to_asset = {}
        mod = _load_hl_adapter()
        adapter = mod.HyperliquidExchangeAdapter()
        adapter._info = mock_info
        assert adapter._sz_decimals("BTC") == 5

