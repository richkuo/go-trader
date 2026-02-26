"""
Black-Scholes option pricing — single implementation used across all platforms.
Replaces duplicate BS code in scripts/check_options.py and options/ibkr_adapter.py.
"""

import math
from typing import Optional


def norm_cdf(x: float) -> float:
    """Standard normal CDF."""
    return 0.5 * (1 + math.erf(x / math.sqrt(2)))


def norm_pdf(x: float) -> float:
    """Standard normal PDF."""
    return math.exp(-0.5 * x * x) / math.sqrt(2 * math.pi)


def bs_price(spot: float, strike: float, dte_days: float, vol: float,
             risk_free: float = 0.05, option_type: str = "call") -> float:
    """
    Black-Scholes option price.

    Args:
        spot: Underlying spot price (USD)
        strike: Option strike price (USD)
        dte_days: Days to expiration
        vol: Annualized implied volatility (decimal, e.g. 0.8 = 80%)
        risk_free: Annual risk-free rate (decimal)
        option_type: "call" or "put"

    Returns:
        Option price in USD
    """
    if dte_days <= 0 or vol <= 0 or spot <= 0:
        if option_type == "call":
            return max(spot - strike, 0.0)
        return max(strike - spot, 0.0)

    T = dte_days / 365.0
    sqrt_T = math.sqrt(T)
    d1 = (math.log(spot / strike) + (risk_free + 0.5 * vol ** 2) * T) / (vol * sqrt_T)
    d2 = d1 - vol * sqrt_T

    if option_type == "call":
        return spot * norm_cdf(d1) - strike * math.exp(-risk_free * T) * norm_cdf(d2)
    return strike * math.exp(-risk_free * T) * norm_cdf(-d2) - spot * norm_cdf(-d1)


def bs_greeks(spot: float, strike: float, dte_days: float, vol: float,
              risk_free: float = 0.05, option_type: str = "call") -> dict:
    """
    Black-Scholes Greeks.

    Args:
        spot: Underlying spot price (USD)
        strike: Option strike price (USD)
        dte_days: Days to expiration
        vol: Annualized implied volatility (decimal)
        risk_free: Annual risk-free rate (decimal)
        option_type: "call" or "put"

    Returns:
        Dict with delta, gamma, theta (per day, USD), vega (per 1% vol change, USD)
    """
    if dte_days <= 0 or vol <= 0 or spot <= 0:
        return {"delta": 0.0, "gamma": 0.0, "theta": 0.0, "vega": 0.0}

    T = dte_days / 365.0
    sqrt_T = math.sqrt(T)
    d1 = (math.log(spot / strike) + (risk_free + 0.5 * vol ** 2) * T) / (vol * sqrt_T)
    d2 = d1 - vol * sqrt_T
    pdf_d1 = norm_pdf(d1)

    if option_type == "call":
        delta = norm_cdf(d1)
        theta_annual = (
            -(spot * pdf_d1 * vol) / (2 * sqrt_T)
            - risk_free * strike * math.exp(-risk_free * T) * norm_cdf(d2)
        )
    else:
        delta = norm_cdf(d1) - 1
        theta_annual = (
            -(spot * pdf_d1 * vol) / (2 * sqrt_T)
            + risk_free * strike * math.exp(-risk_free * T) * norm_cdf(-d2)
        )

    gamma = pdf_d1 / (spot * vol * sqrt_T) if (spot * vol * sqrt_T) > 0 else 0.0
    vega = spot * pdf_d1 * sqrt_T / 100.0  # per 1% vol change
    theta = theta_annual / 365.0           # daily

    return {
        "delta": round(delta, 4),
        "gamma": round(gamma, 6),
        "theta": round(theta, 2),
        "vega": round(vega, 2),
    }


def bs_price_and_greeks(spot: float, strike: float, dte_days: float, vol: float,
                        risk_free: float = 0.05, option_type: str = "call") -> tuple:
    """
    Compute BS price and Greeks in one call.

    Returns:
        (price_usd, greeks_dict)
    """
    price = bs_price(spot, strike, dte_days, vol, risk_free, option_type)
    greeks = bs_greeks(spot, strike, dte_days, vol, risk_free, option_type)
    return price, greeks


if __name__ == "__main__":
    # Quick sanity check
    spot, strike, dte, vol = 95000, 95000, 30, 0.80
    for opt in ("call", "put"):
        price, greeks = bs_price_and_greeks(spot, strike, dte, vol, option_type=opt)
        print(f"ATM {opt}: ${price:,.0f}  greeks={greeks}")
