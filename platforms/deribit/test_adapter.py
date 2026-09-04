
import sys
import os
import math
import importlib.util
import pytest
from unittest.mock import MagicMock, patch
from datetime import datetime, timedelta

_adapter_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "adapter.py")
_spec = importlib.util.spec_from_file_location("deribit_adapter", _adapter_path,
    submodule_search_locations=[os.path.dirname(_adapter_path)])
_mod = importlib.util.module_from_spec(_spec)
_shared_tools = os.path.join(os.path.dirname(os.path.abspath(__file__)), '..', '..', 'shared_tools')
if _shared_tools not in sys.path:
    sys.path.insert(0, os.path.abspath(_shared_tools))
_spec.loader.exec_module(_mod)

OptionType = _mod.OptionType
OptionSide = _mod.OptionSide
OptionContract = _mod.OptionContract
OptionPosition = _mod.OptionPosition
bs_price = _mod.bs_price
bs_greeks = _mod.bs_greeks
implied_volatility = _mod.implied_volatility
_norm_cdf = _mod._norm_cdf
_norm_pdf = _mod._norm_pdf
RISK_FREE_RATE = _mod.RISK_FREE_RATE
DeribitOptionsAdapter = _mod.DeribitOptionsAdapter
DeribitExchangeAdapter = _mod.DeribitExchangeAdapter


class TestBlackScholes:
    @pytest.mark.parametrize("option_type", [OptionType.CALL, OptionType.PUT])
    def test_atm_price_positive(self, option_type):
        price = bs_price(100, 100, 0.5, RISK_FREE_RATE, 0.3, option_type)
        assert price > 0

    def test_call_put_parity(self):
        S, K, T, r, sigma = 100, 100, 0.5, RISK_FREE_RATE, 0.3
        call = bs_price(S, K, T, r, sigma, OptionType.CALL)
        put = bs_price(S, K, T, r, sigma, OptionType.PUT)
        parity = S - K * math.exp(-r * T)
        assert abs((call - put) - parity) < 0.01

    def test_deep_itm_call(self):
        price = bs_price(200, 100, 0.5, RISK_FREE_RATE, 0.3, OptionType.CALL)
        assert price > 97

    @pytest.mark.parametrize("spot,option_type,expected", [
        (110, OptionType.CALL, 10),
        (90, OptionType.CALL, 0),
        (90, OptionType.PUT, 10),
        (110, OptionType.PUT, 0),
    ])
    def test_at_expiry_intrinsic(self, spot, option_type, expected):
        assert bs_price(spot, 100, 0, RISK_FREE_RATE, 0.3, option_type) == expected

    def test_zero_vol_call(self):
        price = bs_price(110, 100, 0.5, RISK_FREE_RATE, 0.0, OptionType.CALL)
        assert price == 10


class TestBSGreeks:
    @pytest.mark.parametrize("option_type,low,high", [
        (OptionType.CALL, 0, 1),
        (OptionType.CALL, 0.4, 0.7),
        (OptionType.PUT, -1, 0),
    ])
    def test_atm_delta_bounds(self, option_type, low, high):
        g = bs_greeks(100, 100, 0.5, RISK_FREE_RATE, 0.3, option_type)
        assert low < g.delta < high

    def test_gamma_positive(self):
        g = bs_greeks(100, 100, 0.5, RISK_FREE_RATE, 0.3, OptionType.CALL)
        assert g.gamma > 0

    @pytest.mark.parametrize("spot,expected_delta", [
        (110, 1.0),
        (90, 0.0),
    ])
    def test_expired_call_delta(self, spot, expected_delta):
        g = bs_greeks(spot, 100, 0, RISK_FREE_RATE, 0.3, OptionType.CALL)
        assert g.delta == expected_delta


class TestImpliedVolatility:
    def test_round_trip(self):
        S, K, T, r, sigma = 100, 100, 0.5, RISK_FREE_RATE, 0.3
        price = bs_price(S, K, T, r, sigma, OptionType.CALL)
        iv = implied_volatility(price, S, K, T, r, OptionType.CALL)
        assert abs(iv - sigma) < 0.005

    def test_zero_price(self):
        assert implied_volatility(0, 100, 100, 0.5, RISK_FREE_RATE, OptionType.CALL) == 0.0

    def test_zero_time(self):
        assert implied_volatility(5, 100, 100, 0, RISK_FREE_RATE, OptionType.CALL) == 0.0


class TestOptionContract:
    def test_mid_price(self):
        c = OptionContract(
            symbol="BTC-100000-C",
            underlying="BTC",
            strike=100000,
            expiry=datetime.utcnow() + timedelta(days=30),
            option_type=OptionType.CALL,
            bid=0.05,
            ask=0.06,
            spot_price=67000,
        )
        assert c.mid_price == pytest.approx(0.055)

    def test_usd_price(self):
        c = OptionContract(
            symbol="BTC-100000-C",
            underlying="BTC",
            strike=100000,
            expiry=datetime.utcnow() + timedelta(days=30),
            option_type=OptionType.CALL,
            bid=0.05,
            ask=0.06,
            spot_price=67000,
        )
        assert c.usd_price == pytest.approx(0.055 * 67000)

    def test_dte_positive(self):
        c = OptionContract(
            symbol="BTC-100000-C",
            underlying="BTC",
            strike=100000,
            expiry=datetime.utcnow() + timedelta(days=30),
            option_type=OptionType.CALL,
        )
        assert c.dte > 29

    @pytest.mark.parametrize("strike,expected", [
        (60000, "ITM"),
        (75000, "OTM"),
        (67000, "ATM"),
    ])
    def test_moneyness_call(self, strike, expected):
        c = OptionContract(
            symbol="X",
            underlying="BTC",
            strike=strike,
            expiry=datetime.utcnow() + timedelta(days=30),
            option_type=OptionType.CALL,
            spot_price=67000,
        )
        assert c.moneyness == expected


class TestDeribitOptionsAdapter:
    def test_portfolio_value_no_positions(self):
        with patch("ccxt.deribit"):
            adapter = DeribitOptionsAdapter(initial_balance_usd=5000)
            assert adapter.get_portfolio_value() == 5000

    def test_portfolio_greeks_empty(self):
        with patch("ccxt.deribit"):
            adapter = DeribitOptionsAdapter()
            g = adapter.get_portfolio_greeks()
            assert g.delta == 0
            assert g.gamma == 0
            assert g.theta == 0
            assert g.vega == 0

    def test_next_order_id(self):
        with patch("ccxt.deribit"):
            adapter = DeribitOptionsAdapter()
            id1 = adapter._next_order_id()
            id2 = adapter._next_order_id()
            assert id1 == "opt_1"
            assert id2 == "opt_2"

    def test_close_nonexistent_position(self):
        with patch("ccxt.deribit"):
            adapter = DeribitOptionsAdapter()
            assert adapter.close_position("nonexistent") is None


class TestDeribitExchangeAdapter:
    def test_get_spot_price_success(self):
        adapter = DeribitExchangeAdapter()
        with patch("ccxt.binanceus") as mock_cls:
            mock_ex = MagicMock()
            mock_ex.fetch_ticker.return_value = {"last": 67500.0}
            mock_cls.return_value = mock_ex
            price = adapter.get_spot_price("BTC")
            assert price == 67500.0

    def test_get_spot_price_failure(self):
        adapter = DeribitExchangeAdapter()
        with patch("ccxt.binanceus") as mock_cls:
            mock_ex = MagicMock()
            mock_ex.fetch_ticker.side_effect = Exception("fail")
            mock_cls.return_value = mock_ex
            assert adapter.get_spot_price("BTC") == 0.0

    def test_get_real_expiry_fallback(self):
        adapter = DeribitExchangeAdapter()
        with patch.dict(sys.modules, {"utils": None}):
            expiry, dte = adapter.get_real_expiry("BTC", 30)
            assert dte == 30
            from datetime import datetime
            datetime.strptime(expiry, "%Y-%m-%d")

    @pytest.mark.parametrize("underlying,spot,expected", [
        ("BTC", 67500, 68000),
        ("ETH", 3475, 3500),
    ])
    def test_get_real_strike_fallback(self, underlying, spot, expected):
        adapter = DeribitExchangeAdapter()
        with patch.dict(sys.modules, {"utils": None}):
            strike = adapter.get_real_strike(underlying, "2026-05-01", "call", spot)
            assert strike == expected

    def test_get_premium_and_greeks_bs_fallback(self):
        adapter = DeribitExchangeAdapter()
        with patch.dict(sys.modules, {"utils": None}):
            pct, usd, greeks = adapter.get_premium_and_greeks(
                "BTC", "call", 70000, "2026-05-01", 30, 67000, 0.6
            )
            assert usd > 0
            assert "delta" in greeks

    def test_get_vol_metrics(self):
        adapter = DeribitExchangeAdapter()
        closes = [50000 + i * 100 for i in range(90)]
        candles = [[i * 86400000, c - 50, c + 50, c - 100, c, 1000] for i, c in enumerate(closes)]
        with patch("ccxt.binanceus") as mock_cls:
            mock_ex = MagicMock()
            mock_ex.fetch_ohlcv.return_value = candles
            mock_cls.return_value = mock_ex
            vol, iv_rank = adapter.get_vol_metrics("BTC")
            assert vol > 0
            assert 0 <= iv_rank <= 100

    def test_get_vol_metrics_insufficient(self):
        adapter = DeribitExchangeAdapter()
        with patch("ccxt.binanceus") as mock_cls:
            mock_ex = MagicMock()
            mock_ex.fetch_ohlcv.return_value = []
            mock_cls.return_value = mock_ex
            vol, iv_rank = adapter.get_vol_metrics("BTC")
            assert vol == 0.60
            assert iv_rank == 50.0
