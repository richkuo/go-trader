
import sys
import os
import importlib.util
import json
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


class TestMarketCloseSized:
    @staticmethod
    def _adapter(live, mids):
        mock_info = MagicMock()
        mock_info.asset_to_sz_decimals = {"ETH": 4}
        if isinstance(mids, Exception):
            mock_info.all_mids.side_effect = mids
        else:
            mock_info.all_mids.return_value = mids
        mock_info_cls = MagicMock(return_value=mock_info)
        mock_exchange = MagicMock()
        mock_exchange.order.return_value = {"status": "ok"}
        mod = _load_hl_adapter(mock_info_cls=mock_info_cls)
        adapter = mod.HyperliquidExchangeAdapter()
        adapter._info = mock_info
        if live:
            adapter._wallet = MagicMock()
            adapter._exchange = mock_exchange
        return adapter, mock_info, mock_exchange

    @pytest.mark.parametrize("live,is_buy,size,px,reduce_only,want", [
        (True, False, 0.12349, 2970.0, True, 0.1234),
        (True, True, 0.12349, 3030.0, True, 0.1234),
        (True, False, 0.5, 2970.0, False, 0.5),
        (True, True, 0.99999, 3030.0, False, 0.9999),
        (True, False, 0.00009, 2970.0, True, ValueError),
        (True, False, 0.0, 2970.0, True, ValueError),
        (True, False, 0.5, 0.0, True, ValueError),
        (True, False, 0.5, float("nan"), True, ValueError),
        (False, False, 0.5, 2970.0, True, RuntimeError),
    ])
    def test_market_close_sized(self, live, is_buy, size, px, reduce_only, want):
        adapter, mock_info, mock_exchange = self._adapter(live, {"ETH": "3000"})

        if isinstance(want, type):
            with pytest.raises(want):
                adapter.market_close_sized("ETH", is_buy, size, px, reduce_only=reduce_only)
            mock_exchange.order.assert_not_called()
            return
        adapter.market_close_sized("ETH", is_buy, size, px, reduce_only=reduce_only)
        assert mock_exchange.order.call_args.args == ("ETH", is_buy, want, px, {"limit": {"tif": "Ioc"}})
        assert mock_exchange.order.call_args.kwargs == {"reduce_only": reduce_only}
        mock_info.all_mids.assert_not_called()
        mock_exchange.market_open.assert_not_called()
        mock_exchange.market_close.assert_not_called()

    @pytest.mark.parametrize("is_buy,mids,want", [
        (False, {"ETH": "3000"}, 2970.0),
        (True, {"ETH": "3000"}, 3030.0),
        (False, {}, ValueError),
        (False, {"ETH": "0"}, ValueError),
        (False, {"ETH": "nan"}, ValueError),
        (False, ConnectionError("info down"), ConnectionError),
    ])
    def test_sized_close_price(self, is_buy, mids, want):
        adapter, _, mock_exchange = self._adapter(True, mids)

        if isinstance(want, type):
            with pytest.raises(want):
                adapter.sized_close_price("ETH", is_buy)
        else:
            assert adapter.sized_close_price("ETH", is_buy) == want
        mock_exchange.order.assert_not_called()



class TestOrderExecution:

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

    def test_place_stop_loss_floors_to_the_lot(self):
        adapter, ex, _ = self._live_adapter(sz_decimals={"ETH": 2})
        ex.order.return_value = {"status": "ok"}
        adapter.place_stop_loss("ETH", 0.127, 3000.0, is_buy=False)
        assert ex.order.call_args.args[2] == 0.12

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


class TestLookupFillFeeByOID:
    def _make_adapter(self):
        mock_info = MagicMock()
        mock_info_cls = MagicMock(return_value=mock_info)
        mod = _load_hl_adapter(mock_info_cls=mock_info_cls)
        adapter = mod.HyperliquidExchangeAdapter()
        adapter._account_address = "0xABC123"
        return adapter, mock_info

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


class TestFillsSummaryByOID:

    def _make_adapter(self):
        mock_info = MagicMock()
        mock_info_cls = MagicMock(return_value=mock_info)
        mod = _load_hl_adapter(mock_info_cls=mock_info_cls)
        adapter = mod.HyperliquidExchangeAdapter()
        adapter._account_address = "0xABC123"
        return adapter, mock_info

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


@pytest.mark.parametrize("symbol,strict,rounded", [
    ("BTC", 5, 5),
    ("UNLISTED", None, 3),
])
def test_lot_size_decimals_is_strict_while_sz_decimals_keeps_the_default(symbol, strict, rounded):
    mod = _load_hl_adapter()
    adapter = mod.HyperliquidExchangeAdapter()
    adapter._info = MagicMock()
    adapter._info.asset_to_sz_decimals = {"BTC": 5}
    refreshed = MagicMock()
    refreshed.asset_to_sz_decimals = {"BTC": 5}
    adapter._build_info = lambda base_url, allow_cache: refreshed

    assert adapter.lot_size_decimals(symbol) == strict
    assert adapter._sz_decimals(symbol) == rounded
