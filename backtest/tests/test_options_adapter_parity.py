import math
import os
import re
import sys
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



def test_btc_strikes_round_to_1000():
    assert adapter_strike("BTC", 67234) == 67000
    assert adapter_strike("BTC", 67501) == 68000
    assert adapter_strike("BTC", 67500) == 68000


def test_eth_strikes_round_to_50():
    assert adapter_strike("ETH", 3412) == 3400
    assert adapter_strike("ETH", 3426) == 3450
    assert adapter_strike("ETH", 3425) == 3400


def test_unknown_underlying_uses_50_default():
    assert adapter_strike("SOL", 150) == 150
    assert adapter_strike("SOL", 173) == 150
    assert adapter_strike("DOGE", 0.2134) == 0.0


def test_strike_grid_matches_adapter_source():
    text = DERIBIT_ADAPTER.read_text()
    match = re.search(
        r"def get_real_strike\([^)]*\)[^:]*:.*?return round\(target_strike / (\d+)\) \* (\d+)",
        text,
        re.DOTALL,
    )
    assert match, (
        "get_real_strike fallback no longer matches "
        "'return round(target_strike / N) * N' — update scraper regex"
    )
    default_step = int(match.group(1))
    assert default_step == int(DEFAULT_STRIKE_STEP), (
        f"Adapter uses ${default_step} default strike step, backtest uses "
        f"${DEFAULT_STRIKE_STEP}"
    )

    btc_match = re.search(
        r'underlying\.upper\(\) == ["\']BTC["\']:\s*(?:\n\s*)?return round\(target_strike, (-?\d+)\)',
        text,
    )
    assert btc_match, (
        "get_real_strike BTC branch no longer matches "
        "'round(target_strike, -N)' — update scraper regex"
    )
    btc_digits = int(btc_match.group(1))
    assert 10 ** (-btc_digits) == int(ADAPTER_STRIKE_STEP["BTC"]), (
        f"Adapter rounds BTC to 10^{-btc_digits}, backtest uses "
        f"{ADAPTER_STRIKE_STEP['BTC']}"
    )



@pytest.mark.parametrize(
    "spot,strike,dte,vol,option_type",
    [
        (67000, 68000, 30, 0.80, "call"),
        (67000, 66000, 30, 0.80, "put"),
        (67000, 67000, 7,  0.60, "call"),
        (3400,  3500,  14, 0.70, "call"),
        (3400,  3300,  45, 0.70, "put"),
    ],
)
def test_backtest_bs_matches_shared_pricing(spot, strike, dte, vol, option_type):
    got = black_scholes_price(spot, strike, dte, vol, option_type=option_type)
    expected = bs_price(spot, strike, dte, vol, option_type=option_type)
    assert got == pytest.approx(expected, rel=1e-12), (
        f"Drift between backtest BS and shared_tools BS on "
        f"({spot}, {strike}, {dte}d, {vol}, {option_type})"
    )


def test_greeks_populated_on_bs_call():
    price, greeks = bs_price_and_greeks(67000, 67000, 30, 0.80, option_type="call")
    assert price > 0
    assert 0.4 < greeks["delta"] < 0.7, greeks
    assert greeks["gamma"] > 0
    assert greeks["vega"] > 0
    assert greeks["theta"] < 0


def test_greeks_populated_on_bs_put():
    price, greeks = bs_price_and_greeks(67000, 67000, 30, 0.80, option_type="put")
    assert price > 0
    assert -0.6 < greeks["delta"] < -0.3, greeks
    assert greeks["gamma"] > 0
    assert greeks["theta"] < 0



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
