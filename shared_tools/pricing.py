
import math
from typing import Optional


def norm_cdf(x: float) -> float:
    return 0.5 * (1 + math.erf(x / math.sqrt(2)))


def norm_pdf(x: float) -> float:
    return math.exp(-0.5 * x * x) / math.sqrt(2 * math.pi)


def bs_price(spot: float, strike: float, dte_days: float, vol: float,
             risk_free: float = 0.05, option_type: str = "call") -> float:
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
    vega = spot * pdf_d1 * sqrt_T / 100.0
    theta = theta_annual / 365.0

    return {
        "delta": round(delta, 4),
        "gamma": round(gamma, 6),
        "theta": round(theta, 2),
        "vega": round(vega, 2),
    }


def bs_price_and_greeks(spot: float, strike: float, dte_days: float, vol: float,
                        risk_free: float = 0.05, option_type: str = "call") -> tuple:
    price = bs_price(spot, strike, dte_days, vol, risk_free, option_type)
    greeks = bs_greeks(spot, strike, dte_days, vol, risk_free, option_type)
    return price, greeks


if __name__ == "__main__":
    spot, strike, dte, vol = 95000, 95000, 30, 0.80
    for opt in ("call", "put"):
        price, greeks = bs_price_and_greeks(spot, strike, dte, vol, option_type=opt)
        print(f"ATM {opt}: ${price:,.0f}  greeks={greeks}")
