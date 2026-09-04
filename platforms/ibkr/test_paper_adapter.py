
import sys
import os
import math
import importlib.util
import pytest
from unittest.mock import MagicMock, patch
from datetime import datetime, timezone, timedelta

_pa_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "paper_adapter.py")
_spec = importlib.util.spec_from_file_location("ibkr_paper_adapter", _pa_path)
_mod = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(_mod)

norm_cdf = _mod.norm_cdf
black_scholes = _mod.black_scholes
bs_greeks = _mod.bs_greeks
IBKRPaperAdapter = _mod.IBKRPaperAdapter
get_spot_price_ibkr = _mod.get_spot_price_ibkr
calc_vol_and_iv_rank = _mod.calc_vol_and_iv_rank


class TestBlackScholes:
    @pytest.mark.parametrize("option_type", ["call", "put"])
    def test_atm_price_positive(self, option_type):
        price = black_scholes(100, 100, 30, 0.3, option_type=option_type)
        assert price > 0

    def test_call_put_parity(self):
        S, K, dte, vol, r = 100, 100, 30, 0.3, 0.05
        T = dte / 365.0
        call = black_scholes(S, K, dte, vol, r, "call")
        put = black_scholes(S, K, dte, vol, r, "put")
        parity = S - K * math.exp(-r * T)
        assert abs((call - put) - parity) < 0.01

    def test_at_expiry(self):
        assert black_scholes(110, 100, 0, 0.3) == 10
        assert black_scholes(90, 100, 0, 0.3, option_type="put") == 10

    def test_zero_vol(self):
        price = black_scholes(110, 100, 30, 0.0)
        assert price == 10

    def test_zero_spot(self):
        price = black_scholes(0, 100, 30, 0.3)
        assert price == 0


class TestBSGreeks:
    @pytest.mark.parametrize("option_type,low,high", [
        ("call", 0, 1),
        ("put", -1, 0),
    ])
    def test_delta_range(self, option_type, low, high):
        g = bs_greeks(100, 100, 30, 0.3, option_type=option_type)
        assert low < g["delta"] < high

    @pytest.mark.parametrize("greek", ["gamma", "vega"])
    def test_greek_positive(self, greek):
        g = bs_greeks(100, 100, 30, 0.3, option_type="call")
        assert g[greek] > 0

    def test_expired_returns_zeros(self):
        g = bs_greeks(100, 100, 0, 0.3)
        assert g["delta"] == 0
        assert g["gamma"] == 0


class TestNormCdf:
    @pytest.mark.parametrize("x,low,high", [
        (0, 0.499, 0.501),
        (5, 0.999, 1.001),
        (-5, -0.001, 0.001),
    ])
    def test_bounds(self, x, low, high):
        assert low < norm_cdf(x) < high


class TestIBKRPaperAdapter:
    @pytest.mark.parametrize("symbol,expected", [
        ("BTC", 0.1),
        ("ETH", 0.5),
        ("XYZ", 1.0),
    ])
    def test_multiplier(self, symbol, expected):
        adapter = IBKRPaperAdapter()
        assert adapter.get_multiplier(symbol) == expected

    def test_contract_value(self):
        adapter = IBKRPaperAdapter()
        value = adapter.get_contract_value("BTC", 67000)
        assert value == 6700.0

    @pytest.mark.parametrize("strike,option_type", [
        (70000, "call"),
        (65000, "put"),
    ])
    def test_estimate_premium(self, strike, option_type):
        adapter = IBKRPaperAdapter()
        result = adapter.estimate_premium("BTC", 67000, strike, 30, 0.6, option_type)
        assert result["premium_usd"] > 0
        assert result["multiplier"] == 0.1
        assert "greeks" in result
        assert "delta" in result["greeks"]

    @pytest.mark.parametrize("underlying,spot,interval", [
        ("BTC", 67000, 1000),
        ("ETH", 3500, 50),
    ])
    def test_available_strikes(self, underlying, spot, interval):
        adapter = IBKRPaperAdapter()
        result = adapter.get_available_strikes(underlying, spot)
        assert len(result["strikes"]) > 0
        assert result["underlying"] == underlying
        assert result["interval"] == interval
        assert any(s < spot for s in result["strikes"])
        assert any(s > spot for s in result["strikes"])

    def test_available_expiries(self):
        adapter = IBKRPaperAdapter()
        expiries = adapter.get_available_expiries(days_out=90)
        assert len(expiries) > 0
        for exp in expiries:
            datetime.strptime(exp, "%Y-%m-%d")
        assert expiries == sorted(expiries)


class TestConvenienceFunctions:
    def test_get_spot_price_ibkr(self):
        with patch("ccxt.binanceus") as mock_cls:
            mock_ex = MagicMock()
            mock_ex.fetch_ticker.return_value = {"last": 67000.0}
            mock_cls.return_value = mock_ex
            price = get_spot_price_ibkr("BTC")
            assert price == 67000.0

    def test_get_spot_price_ibkr_failure(self):
        with patch("ccxt.binanceus") as mock_cls:
            mock_ex = MagicMock()
            mock_ex.fetch_ticker.side_effect = Exception("fail")
            mock_cls.return_value = mock_ex
            assert get_spot_price_ibkr("BTC") == 0

    def test_calc_vol_and_iv_rank(self):
        closes = [50000 + i * 100 for i in range(90)]
        candles = [[i * 86400000, c - 50, c + 50, c - 100, c, 1000] for i, c in enumerate(closes)]
        with patch("ccxt.binanceus") as mock_cls:
            mock_ex = MagicMock()
            mock_ex.fetch_ohlcv.return_value = candles
            mock_cls.return_value = mock_ex
            vol, iv_rank = calc_vol_and_iv_rank("BTC")
            assert vol > 0
            assert 0 <= iv_rank <= 100

    def test_calc_vol_and_iv_rank_insufficient(self):
        with patch("ccxt.binanceus") as mock_cls:
            mock_ex = MagicMock()
            mock_ex.fetch_ohlcv.return_value = []
            mock_cls.return_value = mock_ex
            vol, iv_rank = calc_vol_and_iv_rank("BTC")
            assert vol == 0.5
            assert iv_rank == 50.0
