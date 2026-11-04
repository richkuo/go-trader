import importlib.util
import math
import os
import sys
import types
from pathlib import Path

import pytest

from backtest_options import (
    ADAPTER_STRIKE_STEP,
    DEFAULT_STRIKE_STEP,
    adapter_strike,
    black_scholes_price,
)

REPO_ROOT = Path(__file__).resolve().parents[2]
SHARED_TOOLS = str(REPO_ROOT / "shared_tools")
if SHARED_TOOLS not in sys.path:
    sys.path.insert(0, SHARED_TOOLS)

from pricing import bs_price, bs_price_and_greeks


DERIBIT_ADAPTER = REPO_ROOT / "platforms" / "deribit" / "adapter.py"


def _deribit_exchange_adapter():
    spec = importlib.util.spec_from_file_location("_bt_options_deribit_adapter_parity", DERIBIT_ADAPTER)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod.DeribitExchangeAdapter


def test_strikes_round_to_the_underlying_grid_like_the_adapter_fallback(monkeypatch, subtests):
    grid = [
        ("BTC", 67234, 67000),
        ("BTC", 67501, 68000),
        ("BTC", 67500, 68000),
        ("ETH", 3412, 3400),
        ("ETH", 3426, 3450),
        ("ETH", 3425, 3400),
        ("SOL", 150, 150),
        ("SOL", 173, 150),
        ("DOGE", 0.2134, 0.0),
    ]
    with subtests.test("test_strikes_round_to_the_underlying_grid"):
        for underlying, target, expected in grid:
            assert adapter_strike(underlying, target) == expected, \
                f"test_strikes_round_to_the_underlying_grid[{underlying}-{target}-{expected}]"
    with subtests.test("test_strikes_round_to_the_underlying_grid_like_the_adapter_fallback"):
        with monkeypatch.context() as mp:
            no_chain = types.ModuleType("utils")
            no_chain.find_closest_strike = lambda *args, **kwargs: None
            mp.setitem(sys.modules, "utils", no_chain)
            adapter_cls = _deribit_exchange_adapter()
            for underlying, target, expected in grid:
                label = f"test_strikes_round_to_the_underlying_grid[{underlying}-{target}-{expected}]"
                live = adapter_cls.get_real_strike(None, underlying, "", "call", target)
                assert live == expected, (label, live)
            for underlying, step in [("BTC", ADAPTER_STRIKE_STEP["BTC"]), ("ETH", DEFAULT_STRIKE_STEP),
                                     ("SOL", DEFAULT_STRIKE_STEP)]:
                for target in (step * 7, step * 7 + step * 0.49, step * 7 + step * 0.51, step * 7.5, step * 8.5):
                    label = f"adapter_fallback_parity[{underlying}-{target}]"
                    assert adapter_cls.get_real_strike(None, underlying, "", "call", target) == \
                        adapter_strike(underlying, target), label


def test_backtest_bs_matches_shared_pricing_and_greeks(subtests):
    with subtests.test(case="backtest_bs_matches_shared_pricing"):
        _assert_backtest_bs_matches_shared_pricing()
    with subtests.test(case="greeks_populated_on_bs_price"):
        _assert_greeks_populated_on_bs_price()


def _assert_backtest_bs_matches_shared_pricing():
    for spot, strike, dte, vol, option_type in [
        (67000, 68000, 30, 0.80, "call"),
        (67000, 66000, 30, 0.80, "put"),
        (67000, 67000, 7,  0.60, "call"),
        (3400,  3500,  14, 0.70, "call"),
        (3400,  3300,  45, 0.70, "put"),
    ]:
        got = black_scholes_price(spot, strike, dte, vol, option_type=option_type)
        expected = bs_price(spot, strike, dte, vol, option_type=option_type)
        assert got == pytest.approx(expected, rel=1e-12), (
            f"test_backtest_bs_matches_shared_pricing: drift between backtest BS and shared_tools BS on "
            f"({spot}, {strike}, {dte}d, {vol}, {option_type})"
        )


def _assert_greeks_populated_on_bs_price():
    for option_type, delta_lo, delta_hi in [
        ("call", 0.4, 0.7),
        ("put", -0.6, -0.3),
    ]:
        case = f"test_greeks_populated_on_bs_price[{option_type}-{delta_lo}-{delta_hi}]"
        price, greeks = bs_price_and_greeks(67000, 67000, 30, 0.80,
                                            option_type=option_type)
        assert price > 0, case
        assert delta_lo < greeks["delta"] < delta_hi, (case, greeks)
        assert greeks["gamma"] > 0, case
        assert greeks["vega"] > 0, case
        assert greeks["theta"] < 0, case


def _deterministic_candles(underlying: str = "BTC", start_price: float = 50000.0,
                            n: int = 200) -> list:
    candles = []
    price = start_price
    for i in range(n):
        sign = 1 if (i % 2 == 0) else -1
        step = 0.002 if i < 150 else 0.05
        price *= math.exp(sign * step)
        ts = (1704067200 + i * 86400) * 1000
        candles.append([ts, price, price, price, price, 0.0])
    return candles


def test_trade_log_entries_carry_delta_and_on_grid_strikes():
    from backtest_options import OptionsBacktester

    candles = _deterministic_candles()
    bt = OptionsBacktester(initial_capital=10_000, max_positions=2, check_interval=1)
    bt.run_vol_mean_reversion(candles, "BTC")

    opens = [t for t in bt.trade_log if t["event"] == "open"]
    assert opens, (
        "Deterministic high-vol tail must produce at least one entry; if "
        "this fails, the iv_rank thresholds or vol-window sizes changed"
    )
    for t in opens:
        assert "delta" in t, f"trade log entry missing delta: {t}"
        assert t["strike"] % ADAPTER_STRIKE_STEP["BTC"] == 0, (
            f"BTC strike {t['strike']} is off the $1000 grid"
        )
