"""
Regression tests for issue #303 H2 — Backtester commission rate must be
platform-aware and match ``scheduler/fees.go:CalculatePlatformSpotFee``.
A strategy that clears fees on Robinhood (0%) must not be taxed at
BinanceUS's 0.1% in the backtest, and vice versa.

Fee constants here mirror scheduler/fees.go — update both files together
if the live rate table changes.
"""
import re
from pathlib import Path

import pandas as pd
import pytest

from backtester import Backtester, PLATFORM_FEE_PCT, fee_pct_for_platform


FEES_GO = Path(__file__).resolve().parents[2] / "scheduler" / "fees.go"

# Mirror the rate table from fees.go for the parity test. If these drift from
# the Go source the scraper below catches it.
EXPECTED_RATES = {
    "binanceus":   0.001,
    "hyperliquid": 0.00035,
    "robinhood":   0.0,
    "luno":        0.01,
    "okx":         0.001,
    "okx-perps":   0.0005,
}


def _scrape_fees_go_constants() -> dict:
    """Pull the const block from scheduler/fees.go and parse the rate
    constants we mirror in PLATFORM_FEE_PCT. Guards against the Python table
    silently drifting from the Go source."""
    text = FEES_GO.read_text()
    const_pattern = re.compile(
        r"^\s*(BinanceSpotFeePct|HyperliquidTakerFeePct|LunoTakerFeePct|"
        r"OKXSpotTakerFeePct|OKXPerpsTakerFeePct)\s*=\s*([0-9.]+)",
        re.MULTILINE,
    )
    return {m.group(1): float(m.group(2)) for m in const_pattern.finditer(text)}


def test_platform_fee_table_matches_expected():
    assert PLATFORM_FEE_PCT == EXPECTED_RATES, (
        "PLATFORM_FEE_PCT drifted from the inline expectation — update both "
        "the table and this test together, and keep them in sync with fees.go"
    )


def test_platform_fee_table_matches_fees_go():
    go_rates = _scrape_fees_go_constants()
    # Only check constants we actually mirror — not all of fees.go is spot.
    assert go_rates["BinanceSpotFeePct"] == PLATFORM_FEE_PCT["binanceus"]
    assert go_rates["HyperliquidTakerFeePct"] == PLATFORM_FEE_PCT["hyperliquid"]
    assert go_rates["LunoTakerFeePct"] == PLATFORM_FEE_PCT["luno"]
    assert go_rates["OKXSpotTakerFeePct"] == PLATFORM_FEE_PCT["okx"]
    assert go_rates["OKXPerpsTakerFeePct"] == PLATFORM_FEE_PCT["okx-perps"]


def test_unknown_platform_falls_back_to_binanceus():
    """Matches the ``default:`` branch in CalculatePlatformSpotFee."""
    assert fee_pct_for_platform("mystery-exchange") == PLATFORM_FEE_PCT["binanceus"]


def test_backtester_uses_platform_fee_by_default():
    bt = Backtester(platform="hyperliquid")
    assert bt.commission_pct == pytest.approx(0.00035)

    bt = Backtester(platform="robinhood")
    assert bt.commission_pct == 0.0

    bt = Backtester(platform="luno")
    assert bt.commission_pct == pytest.approx(0.01)


def test_explicit_commission_overrides_platform():
    bt = Backtester(platform="hyperliquid", commission_pct=0.002)
    assert bt.commission_pct == pytest.approx(0.002), (
        "Explicit commission_pct must win over the platform default — "
        "tests rely on the override to pin zero-fee scenarios."
    )


def _one_trade_df():
    # Buy on bar 1 (fills at bar 2 open), sell on bar 3 (fills at bar 4 open).
    # Flat price so P&L isolates fee impact.
    idx = pd.date_range("2024-01-01", periods=6, freq="D")
    return pd.DataFrame(
        {
            "open":   [100.0] * 6,
            "high":   [100.0] * 6,
            "low":    [100.0] * 6,
            "close":  [100.0] * 6,
            "signal": [0, 1, 0, -1, 0, 0],
        },
        index=idx,
    )


@pytest.mark.parametrize(
    "platform,expected_rate",
    [
        ("binanceus",   0.001),
        ("hyperliquid", 0.00035),
        ("robinhood",   0.0),
        ("luno",        0.01),
        ("okx",         0.001),
        ("okx-perps",   0.0005),
    ],
)
def test_fee_actually_deducted_end_to_end(platform, expected_rate):
    """On a flat-price round trip, final_capital reflects two fee charges
    plus the symmetric slippage band. Compute the expected final_capital
    from the fee/slippage model and pin it."""
    df = _one_trade_df()
    capital = 1000.0
    slippage = 0.0005

    bt = Backtester(
        initial_capital=capital, slippage_pct=slippage, platform=platform
    )
    results = bt.run(df, strategy_name="fee-probe", save=False)

    # Model: buy at open*(1+slip), sell at open*(1-slip), fee pct on each leg.
    buy_fill = 100.0 * (1 + slippage)
    sell_fill = 100.0 * (1 - slippage)
    cash_after_buy_fee = capital * (1 - expected_rate)
    shares = cash_after_buy_fee / buy_fill
    proceeds = shares * sell_fill
    expected_final = proceeds * (1 - expected_rate)

    assert results["final_capital"] == pytest.approx(expected_final, abs=0.01), (
        f"Fee for platform '{platform}' did not match — expected rate "
        f"{expected_rate:.5f} producing final_capital {expected_final:.2f}"
    )
