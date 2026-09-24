import pytest

from shared_tools.conftest import load_module

_PRICING = load_module("_pricing_test", __file__.replace("test_pricing.py", "pricing.py"))
bs_price = _PRICING.bs_price


class TestBsPrice:
    @pytest.mark.parametrize("S,K,dte,vol,option_type,expected,tol", [
        (100, 100, 365, 0.20, "call", 10.45, 0.1),
        (100, 100, 365, 0.20, "put", 5.57, 0.1),
        (110, 100, 0, 0.30, "call", 10.0, 1e-10),
        (110, 100, 0, 0.30, "put", 0.0, 1e-10),
        (90, 100, 0, 0.30, "put", 10.0, 1e-10),
        (110, 100, 30, 0, "call", 10.0, 1e-10),
        (90, 100, 30, 0, "call", 0.0, 1e-10),
    ])
    def test_known_values(self, S, K, dte, vol, option_type, expected, tol):
        price = bs_price(S, K, dte, vol, risk_free=0.05, option_type=option_type)
        assert price == pytest.approx(expected, abs=tol)
