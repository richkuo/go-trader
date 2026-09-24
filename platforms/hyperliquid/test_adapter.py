
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

