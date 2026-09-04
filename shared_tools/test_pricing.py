
import math
import pytest

from shared_tools.conftest import load_module

_PRICING = load_module("_pricing_test", __file__.replace("test_pricing.py", "pricing.py"))
norm_cdf = _PRICING.norm_cdf
norm_pdf = _PRICING.norm_pdf
bs_price = _PRICING.bs_price
bs_greeks = _PRICING.bs_greeks
bs_price_and_greeks = _PRICING.bs_price_and_greeks


@pytest.mark.parametrize("x,expected,tol", [
    (0.0, 0.5, 1e-10),
    (4.0, 0.9999683, 1e-5),
    (-4.0, 1.0 - 0.9999683, 1e-5),
    (1.0, 0.8413, 1e-4),
    (-1.0, 0.1587, 1e-4),
])
def test_norm_cdf_known_values(x, expected, tol):
    assert norm_cdf(x) == pytest.approx(expected, abs=tol)


def test_norm_cdf_symmetry():
    for x in [0.5, 1.0, 2.0, 3.0]:
        assert norm_cdf(x) + norm_cdf(-x) == pytest.approx(1.0, abs=1e-12)


@pytest.mark.parametrize("x,expected,tol", [
    (0.0, 1.0 / math.sqrt(2 * math.pi), 1e-10),
    (1.0, 0.2420, 1e-4),
])
def test_norm_pdf_known_values(x, expected, tol):
    assert norm_pdf(x) == pytest.approx(expected, abs=tol)


def test_norm_pdf_symmetry():
    for x in [0.5, 1.0, 2.0]:
        assert norm_pdf(x) == pytest.approx(norm_pdf(-x), abs=1e-12)


def test_norm_pdf_decreasing():
    assert norm_pdf(0.0) > norm_pdf(1.0) > norm_pdf(2.0) > norm_pdf(3.0)


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

    def test_put_call_parity(self):
        S, K, dte, vol, r = 100, 100, 365, 0.30, 0.05
        call = bs_price(S, K, dte, vol, r, "call")
        put = bs_price(S, K, dte, vol, r, "put")
        T = dte / 365.0
        expected_diff = S - K * math.exp(-r * T)
        assert (call - put) == pytest.approx(expected_diff, abs=1e-8)

    def test_deep_itm_call(self):
        price = bs_price(150, 100, 30, 0.50, 0.05, "call")
        intrinsic = 150 - 100
        assert price >= intrinsic

    def test_deep_otm_call(self):
        price = bs_price(50, 100, 30, 0.20, 0.05, "call")
        assert price < 1.0
        assert price >= 0.0

    @pytest.mark.parametrize("lower,higher", [
        ((100, 100, 30, 0.20, 0.05, "call"), (100, 100, 30, 0.50, 0.05, "call")),
        ((100, 100, 7, 0.30, 0.05, "call"), (100, 100, 90, 0.30, 0.05, "call")),
    ])
    def test_price_monotonic_in_vol_and_dte(self, lower, higher):
        assert bs_price(*higher) > bs_price(*lower)

    def test_btc_atm_call(self):
        price = bs_price(95000, 95000, 30, 0.80, 0.05)
        assert price > 5000
        assert price < 20000


class TestBsGreeks:
    @pytest.mark.parametrize("S,K,dte,vol,option_type,expected,tol", [
        (100, 100, 365, 0.20, "call", 0.6368, 0.02),
        (100, 100, 365, 0.20, "put", -0.3632, 0.02),
        (200, 100, 30, 0.20, "call", 1.0, 0.01),
        (50, 100, 30, 0.20, "call", 0.0, 0.01),
    ])
    def test_delta_known_values(self, S, K, dte, vol, option_type, expected, tol):
        g = bs_greeks(S, K, dte, vol, 0.05, option_type)
        assert g["delta"] == pytest.approx(expected, abs=tol)

    @pytest.mark.parametrize("option_type,low,high", [
        ("call", 0, 1),
        ("put", -1, 0),
    ])
    def test_delta_range(self, option_type, low, high):
        g = bs_greeks(100, 100, 30, 0.30, 0.05, option_type)
        assert low <= g["delta"] <= high

    @pytest.mark.parametrize("greek", ["gamma", "vega"])
    def test_greek_positive(self, greek):
        g = bs_greeks(100, 100, 30, 0.30, 0.05, "call")
        assert g[greek] > 0

    @pytest.mark.parametrize("greek", ["gamma", "vega"])
    def test_greek_same_for_call_and_put(self, greek):
        gc = bs_greeks(100, 100, 30, 0.30, 0.05, "call")
        gp = bs_greeks(100, 100, 30, 0.30, 0.05, "put")
        assert gc[greek] == pytest.approx(gp[greek], abs=1e-6)

    def test_theta_negative_for_long_options(self):
        gc = bs_greeks(100, 100, 30, 0.30, 0.05, "call")
        gp = bs_greeks(100, 100, 30, 0.30, 0.05, "put")
        assert gc["theta"] < 0
        assert gp["theta"] < 0

    @pytest.mark.parametrize("args,keys", [
        ((100, 100, 0, 0.30, 0.05, "call"), ("delta", "gamma", "theta", "vega")),
        ((0, 100, 30, 0.30, 0.05, "call"), ("delta",)),
    ])
    def test_degenerate_inputs_return_zeros(self, args, keys):
        g = bs_greeks(*args)
        for key in keys:
            assert g[key] == 0.0


class TestBsPriceAndGreeks:
    def test_price_matches_standalone(self):
        price_standalone = bs_price(100, 100, 30, 0.30, 0.05, "call")
        price_combined, _ = bs_price_and_greeks(100, 100, 30, 0.30, 0.05, "call")
        assert price_combined == pytest.approx(price_standalone, abs=1e-10)

    def test_greeks_match_standalone(self):
        greeks_standalone = bs_greeks(100, 100, 30, 0.30, 0.05, "call")
        _, greeks_combined = bs_price_and_greeks(100, 100, 30, 0.30, 0.05, "call")
        for key in ("delta", "gamma", "theta", "vega"):
            assert greeks_combined[key] == pytest.approx(greeks_standalone[key], abs=1e-10)

    def test_greeks_dict_keys(self):
        _, greeks = bs_price_and_greeks(100, 100, 30, 0.30, 0.05, "call")
        assert set(greeks.keys()) == {"delta", "gamma", "theta", "vega"}
