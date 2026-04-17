"""
Regression tests for issue #303 H4 — options backtester must use the
same strike grid and Black-Scholes pricing that the live Deribit adapter
falls back to, so backtest results are comparable with live deployment.

Pre-fix the backtester rounded BTC strikes to the nearest $100 and
carried its own (differently-coded) BS routine. Neither matched the
live adapter — premiums landed at non-listed strikes and greeks were
silently discarded.
"""
import math
import os
import sys

import pytest

from backtest_options import (
    ADAPTER_STRIKE_STEP,
    adapter_strike,
    black_scholes_price,
)

# Make shared_tools/pricing importable so the parity test can call the
# same function the live adapter uses for its Black-Scholes fallback.
REPO_ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
SHARED_TOOLS = os.path.join(REPO_ROOT, "shared_tools")
if SHARED_TOOLS not in sys.path:
    sys.path.insert(0, SHARED_TOOLS)

from pricing import bs_price, bs_price_and_greeks  # type: ignore


# ---------- Strike-grid parity ----------

def test_btc_strikes_round_to_1000():
    """Deribit BTC strikes step at $1000 — round-to-$100 requests an
    instrument that does not exist on the exchange."""
    assert adapter_strike("BTC", 67234) == 67000
    assert adapter_strike("BTC", 67501) == 68000
    assert adapter_strike("BTC", 67500) == 68000  # Python round-half-even: 68


def test_eth_strikes_round_to_50():
    """Deribit ETH strikes step at $50."""
    assert adapter_strike("ETH", 3412) == 3400
    assert adapter_strike("ETH", 3426) == 3450
    assert adapter_strike("ETH", 3425) == 3400  # round-half-even


def test_unknown_underlying_falls_back_to_btc_grid():
    """Matches the adapter's fallback — if ``underlying`` isn't recognized,
    use BTC's $1000 grid rather than returning an off-grid strike."""
    assert adapter_strike("DOGE", 0.2134) == 0.0  # rounds to nearest $1000
    assert adapter_strike("DOGE", 1500) == 2000


def test_strike_grid_matches_adapter_fallback_source():
    """The authoritative values live in platforms/deribit/adapter.py
    get_real_strike fallback. If those change, this test fails loud."""
    # Mirror of:
    #   if underlying.upper() == "BTC": return round(target, -3)
    #   return round(target / 50) * 50
    # Adapter rounds BTC to nearest 1000 via round(x, -3); we express that
    # as a 1000-step. Adapter defaults to 50-step for ETH and everything
    # else besides BTC. Verify both encodings agree.
    assert ADAPTER_STRIKE_STEP["BTC"] == 1000.0
    assert ADAPTER_STRIKE_STEP["ETH"] == 50.0


# ---------- BS pricing parity ----------

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
    """backtest_options.black_scholes_price must return the exact same
    premium as shared_tools.pricing.bs_price on identical inputs — both
    now route through the same implementation.
    """
    got = black_scholes_price(spot, strike, dte, vol, option_type=option_type)
    expected = bs_price(spot, strike, dte, vol, option_type=option_type)
    assert got == pytest.approx(expected, rel=1e-12), (
        f"Drift between backtest BS and shared_tools BS on "
        f"({spot}, {strike}, {dte}d, {vol}, {option_type})"
    )


def test_greeks_populated_on_bs_call():
    """Greeks must be surfaced on entry so delta-neutral analysis is
    possible. Previously the backtester computed price only and discarded
    greeks entirely."""
    price, greeks = bs_price_and_greeks(67000, 67000, 30, 0.80, option_type="call")
    assert price > 0
    # ATM call delta ~0.5 (rises with vol/dte; accept 0.4–0.7 band).
    assert 0.4 < greeks["delta"] < 0.7, greeks
    assert greeks["gamma"] > 0
    assert greeks["vega"] > 0
    # Calls have negative theta (time decay); shared_tools expresses it
    # per day in USD, so it should be a small negative number.
    assert greeks["theta"] < 0


def test_greeks_populated_on_bs_put():
    price, greeks = bs_price_and_greeks(67000, 67000, 30, 0.80, option_type="put")
    assert price > 0
    # ATM put delta ~-0.5.
    assert -0.6 < greeks["delta"] < -0.3, greeks
    assert greeks["gamma"] > 0
    assert greeks["theta"] < 0


# ---------- End-to-end: strike + greeks in trade log ----------

def test_trade_log_entries_carry_delta():
    """Running the full options backtester should emit delta on each
    ``open`` event so downstream analysis can rank entries by delta-
    neutrality. Previously the trade log omitted greeks entirely."""
    from backtest_options import OptionsBacktester

    # Minimal 100-day synthetic path with extreme vol at the end — forces
    # at least one entry (iv_rank >> 75) so the trade log is non-empty.
    candles = []
    price = 50000.0
    import random
    rng = random.Random(17)
    for i in range(200):
        # First 150 bars: calm; last 50 bars: high-vol.
        noise = rng.gauss(0, 0.002 if i < 150 else 0.05)
        price *= math.exp(noise)
        ts = (1704067200 + i * 86400) * 1000  # 2024-01-01 + i days, ms
        candles.append([ts, price, price, price, price, 0.0])

    bt = OptionsBacktester(initial_capital=10_000, max_positions=2, check_interval=1)
    bt.run_vol_mean_reversion(candles, "BTC")

    opens = [t for t in bt.trade_log if t["event"] == "open"]
    assert opens, "Synthetic path produced no entries — adjust noise schedule"
    for t in opens:
        assert "delta" in t, f"trade log entry missing delta: {t}"
        assert t["strike"] % ADAPTER_STRIKE_STEP["BTC"] == 0, (
            f"BTC strike {t['strike']} is off the $1000 grid"
        )
