
import sys
import os
import importlib.util
import pytest
from unittest.mock import MagicMock, patch

_adapter_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "adapter.py")
_shared_tools = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), '..', '..', 'shared_tools'))
if _shared_tools not in sys.path:
    sys.path.insert(0, _shared_tools)


def _load_rh_module():
    spec = importlib.util.spec_from_file_location("rh_adapter", _adapter_path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


_mod = _load_rh_module()
RobinhoodExchangeAdapter = _mod.RobinhoodExchangeAdapter
_get_strike_interval = _mod._get_strike_interval


@pytest.fixture
def paper_adapter():
    with patch.dict(os.environ, {}, clear=False):
        for key in ("ROBINHOOD_USERNAME", "ROBINHOOD_PASSWORD", "ROBINHOOD_TOTP_SECRET"):
            os.environ.pop(key, None)
        return RobinhoodExchangeAdapter(mode="paper")


class TestProperties:
    def test_live_mode_no_creds_raises(self):
        with patch.dict(os.environ, {}, clear=False):
            for key in ("ROBINHOOD_USERNAME", "ROBINHOOD_PASSWORD", "ROBINHOOD_TOTP_SECRET"):
                os.environ.pop(key, None)
            with pytest.raises(RuntimeError, match="Live mode requires"):
                RobinhoodExchangeAdapter(mode="live")


class TestSymbolResolution:
    @pytest.mark.parametrize("symbol,expected", [
        ("BTC", "BTC-USD"),
        ("ETH", "ETH-USD"),
        ("SPY", "SPY"),
        ("AAPL", "AAPL"),
    ])
    def test_resolve_yahoo_symbol(self, paper_adapter, symbol, expected):
        assert paper_adapter._resolve_yahoo_symbol(symbol) == expected


class TestMarketData:
    def test_get_spot_price_alias(self, paper_adapter):
        with patch.object(paper_adapter, "get_price", return_value=42000.0):
            assert paper_adapter.get_spot_price("BTC") == 42000.0

    def test_get_ohlcv_delegates_to_yahoo(self, paper_adapter):
        with patch.object(paper_adapter, "_get_yahoo_ohlcv", return_value=[[0, 1, 2, 3, 4, 5]]) as mock:
            result = paper_adapter.get_ohlcv("BTC", "1h", 100)
            mock.assert_called_once_with("BTC", "1h", 100)
            assert result == [[0, 1, 2, 3, 4, 5]]

    def test_get_ohlcv_closes(self, paper_adapter):
        candles = [
            [0, 100, 110, 90, 105, 50],
            [1, 105, 115, 95, 110, 60],
        ]
        with patch.object(paper_adapter, "_get_yahoo_ohlcv", return_value=candles):
            closes = paper_adapter.get_ohlcv_closes("BTC", "4h", 100, min_len=1)
            assert closes == [105, 110]

    def test_get_ohlcv_closes_insufficient(self, paper_adapter):
        with patch.object(paper_adapter, "_get_yahoo_ohlcv", return_value=[]):
            assert paper_adapter.get_ohlcv_closes("BTC", "4h", 100) is None


class TestStrikeInterval:
    @pytest.mark.parametrize("price,expected", [
        (50, 1),
        (200, 5),
        (600, 10),
    ])
    def test_interval_by_price_band(self, price, expected):
        assert _get_strike_interval(price) == expected


class TestOptionsProtocol:
    def test_get_real_expiry_paper(self, paper_adapter):
        expiry, dte = paper_adapter.get_real_expiry("SPY", 30)
        assert dte == 30
        from datetime import datetime
        datetime.strptime(expiry, "%Y-%m-%d")

    def test_get_real_strike_paper_stock(self, paper_adapter):
        strike = paper_adapter.get_real_strike("SPY", "2026-05-01", "call", 453.0)
        assert strike == 455.0

    def test_get_real_strike_paper_low_price(self, paper_adapter):
        strike = paper_adapter.get_real_strike("XYZ", "2026-05-01", "call", 42.3)
        assert strike == 42.0

    def test_get_premium_and_greeks_paper_bs(self, paper_adapter):
        pct, usd, greeks = paper_adapter.get_premium_and_greeks(
            "SPY", "call", 450, "2026-05-01", 30, 445, 0.20
        )
        assert usd > 0
        assert "delta" in greeks
        assert usd >= pct * 445 * 90


class TestOrderExecution:
    @pytest.mark.parametrize("method,args", [
        ("market_buy", ("BTC", 1000)),
        ("market_sell", ("BTC", 0.5)),
    ])
    def test_market_order_paper_raises(self, paper_adapter, method, args):
        with pytest.raises(RuntimeError, match="live mode"):
            getattr(paper_adapter, method)(*args)

    def test_get_crypto_positions_not_logged_in(self, paper_adapter):
        assert paper_adapter.get_crypto_positions() == []
