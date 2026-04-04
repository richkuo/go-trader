"""Tests for IBKR paper_adapter.py — Black-Scholes, contracts, and IBKRPaperAdapter."""

import sys
import os
import math
import importlib.util
import pytest
from unittest.mock import MagicMock, patch
from datetime import datetime, timezone, timedelta

# Load ibkr paper_adapter by file path to avoid module name collisions
_pa_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "paper_adapter.py")
_spec = importlib.util.spec_from_file_location("ibkr_paper_adapter", _pa_path)
_mod = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(_mod)

norm_cdf = _mod.norm_cdf
black_scholes = _mod.black_scholes
bs_greeks = _mod.bs_greeks
IBKRPaperAdapter = _mod.IBKRPaperAdapter
IBKRConnection = _mod.IBKRConnection
get_spot_price_ibkr = _mod.get_spot_price_ibkr
calc_vol_and_iv_rank = _mod.calc_vol_and_iv_rank


# ─── Black-Scholes ─────────────────────────────────

class TestBlackScholes:
    def test_call_price_positive(self):
        price = black_scholes(100, 100, 30, 0.3, option_type="call")
        assert price > 0

    def test_put_price_positive(self):
        price = black_scholes(100, 100, 30, 0.3, option_type="put")
        assert price > 0

    def test_call_put_parity(self):
        S, K, dte, vol, r = 100, 100, 30, 0.3, 0.05
        T = dte / 365.0
        call = black_scholes(S, K, dte, vol, r, "call")
        put = black_scholes(S, K, dte, vol, r, "put")
        parity = S - K * math.exp(-r * T)
        assert abs((call - put) - parity) < 0.01

    def test_at_expiry(self):
        assert black_scholes(110, 100, 0, 0.3) == 10  # intrinsic for call
        assert black_scholes(90, 100, 0, 0.3, option_type="put") == 10

    def test_zero_vol(self):
        price = black_scholes(110, 100, 30, 0.0)
        assert price == 10  # intrinsic

    def test_zero_spot(self):
        price = black_scholes(0, 100, 30, 0.3)
        assert price == 0


class TestBSGreeks:
    def test_call_delta_range(self):
        g = bs_greeks(100, 100, 30, 0.3, option_type="call")
        assert 0 < g["delta"] < 1

    def test_put_delta_range(self):
        g = bs_greeks(100, 100, 30, 0.3, option_type="put")
        assert -1 < g["delta"] < 0

    def test_gamma_positive(self):
        g = bs_greeks(100, 100, 30, 0.3, option_type="call")
        assert g["gamma"] > 0

    def test_vega_positive(self):
        g = bs_greeks(100, 100, 30, 0.3, option_type="call")
        assert g["vega"] > 0

    def test_expired_returns_zeros(self):
        g = bs_greeks(100, 100, 0, 0.3)
        assert g["delta"] == 0
        assert g["gamma"] == 0


class TestNormCdf:
    def test_at_zero(self):
        assert abs(norm_cdf(0) - 0.5) < 0.001

    def test_large_positive(self):
        assert norm_cdf(5) > 0.999

    def test_large_negative(self):
        assert norm_cdf(-5) < 0.001


# ─── IBKRPaperAdapter ──────────────────────────────

class TestIBKRPaperAdapter:
    def test_multiplier_btc(self):
        adapter = IBKRPaperAdapter()
        assert adapter.get_multiplier("BTC") == 0.1

    def test_multiplier_eth(self):
        adapter = IBKRPaperAdapter()
        assert adapter.get_multiplier("ETH") == 0.5

    def test_multiplier_unknown(self):
        adapter = IBKRPaperAdapter()
        assert adapter.get_multiplier("XYZ") == 1.0

    def test_contract_value(self):
        adapter = IBKRPaperAdapter()
        value = adapter.get_contract_value("BTC", 67000)
        assert value == 6700.0  # 67000 * 0.1

    def test_estimate_premium_call(self):
        adapter = IBKRPaperAdapter()
        result = adapter.estimate_premium("BTC", 67000, 70000, 30, 0.6, "call")
        assert result["premium_usd"] > 0
        assert result["multiplier"] == 0.1
        assert "greeks" in result
        assert "delta" in result["greeks"]

    def test_estimate_premium_put(self):
        adapter = IBKRPaperAdapter()
        result = adapter.estimate_premium("BTC", 67000, 65000, 30, 0.6, "put")
        assert result["premium_usd"] > 0

    def test_available_strikes_btc(self):
        adapter = IBKRPaperAdapter()
        result = adapter.get_available_strikes("BTC", 67000)
        assert len(result["strikes"]) > 0
        assert result["underlying"] == "BTC"
        assert result["interval"] == 1000
        # Strikes should be around spot
        assert any(s < 67000 for s in result["strikes"])
        assert any(s > 67000 for s in result["strikes"])

    def test_available_strikes_eth(self):
        adapter = IBKRPaperAdapter()
        result = adapter.get_available_strikes("ETH", 3500)
        assert result["interval"] == 50

    def test_available_expiries(self):
        adapter = IBKRPaperAdapter()
        expiries = adapter.get_available_expiries(days_out=90)
        assert len(expiries) > 0
        # All should be valid dates
        for exp in expiries:
            datetime.strptime(exp, "%Y-%m-%d")
        # Should be sorted
        assert expiries == sorted(expiries)


# ─── IBKRConnection ────────────────────────────────

class TestIBKRConnection:
    def test_init(self):
        conn = IBKRConnection(host="127.0.0.1", port=4002, client_id=1)
        assert conn.host == "127.0.0.1"
        assert conn.port == 4002
        assert conn.is_connected() is False


# ─── Convenience Functions ─────────────────────────

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
