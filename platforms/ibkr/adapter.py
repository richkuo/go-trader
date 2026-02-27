"""
IBKR ExchangeAdapter — implements ExchangeAdapter for CME crypto options.
Uses Black-Scholes for premium estimation (paper trading / fallback).
Live trading requires IBKRPaperAdapter from paper_adapter.py and TWS connection.
"""

import sys
import os as _os
import math
from datetime import datetime, timezone, timedelta
from typing import Tuple

sys.path.insert(0, _os.path.join(_os.path.dirname(_os.path.abspath(__file__)), '..', '..', 'shared_tools'))

from pricing import bs_price_and_greeks

# CME contract specs: interval = minimum strike increment, multiplier = contract size
CME_SPECS = {
    "BTC": {"interval": 1000, "multiplier": 0.1},  # Micro Bitcoin
    "ETH": {"interval": 50,   "multiplier": 0.5},  # Micro Ether
}
DEFAULT_SPECS = {"interval": 100, "multiplier": 1.0}


def _get_spot_price(underlying: str) -> float:
    """Fetch spot price via ccxt Binance US."""
    import ccxt
    exchange = ccxt.binanceus({"enableRateLimit": True})
    for suffix in ("/USDT", "/USD"):
        try:
            ticker = exchange.fetch_ticker(underlying + suffix)
            price = ticker.get("last") or 0
            if price and price > 0:
                return float(price)
        except Exception:
            continue
    return 0.0


def _calc_vol_and_iv_rank(underlying: str) -> Tuple[float, float]:
    """Compute historical vol and IV rank from OHLCV data."""
    try:
        import ccxt
        exchange = ccxt.binanceus({"enableRateLimit": True})
        ohlcv = exchange.fetch_ohlcv(underlying + "/USDT", "1d", limit=90)
        if not ohlcv or len(ohlcv) < 15:
            return 0.60, 50.0
        closes = [c[4] for c in ohlcv]
        returns = [math.log(closes[i] / closes[i - 1]) for i in range(1, len(closes))]
        if len(returns) < 14:
            return 0.60, 50.0
        w = 14
        mean = sum(returns[-w:]) / w
        variance = sum((r - mean) ** 2 for r in returns[-w:]) / w
        vol = math.sqrt(variance) * math.sqrt(365)

        # IV rank (rolling HV comparison)
        hvs = []
        for i in range(len(returns) - w + 1):
            chunk = returns[i:i + w]
            m = sum(chunk) / w
            v = sum((r - m) ** 2 for r in chunk) / w
            hvs.append(math.sqrt(v) * math.sqrt(365) * 100)
        current_hv = vol * 100
        hv_min, hv_max = min(hvs), max(hvs)
        if hv_max > hv_min:
            iv_rank = (current_hv - hv_min) / (hv_max - hv_min) * 100
            iv_rank = round(min(max(iv_rank, 0.0), 100.0), 1)
        else:
            iv_rank = 50.0

        return round(vol, 4), iv_rank
    except Exception:
        return 0.60, 50.0


class IBKRExchangeAdapter:
    """
    ExchangeAdapter for IBKR/CME crypto options.
    Uses CME-aligned strikes and Black-Scholes for all premium estimates.
    """

    @property
    def name(self) -> str:
        return "ibkr"

    def get_spot_price(self, underlying: str) -> float:
        return _get_spot_price(underlying)

    def get_vol_metrics(self, underlying: str) -> Tuple[float, float]:
        return _calc_vol_and_iv_rank(underlying)

    def get_real_expiry(self, underlying: str, target_dte: int) -> Tuple[str, int]:
        """Return synthetic expiry at exactly target_dte days from now."""
        expiry_dt = datetime.now(timezone.utc) + timedelta(days=target_dte)
        return expiry_dt.strftime("%Y-%m-%d"), target_dte

    def get_real_strike(self, underlying: str, expiry: str,
                        option_type: str, target_strike: float) -> float:
        """Return CME-aligned strike closest to target."""
        specs = CME_SPECS.get(underlying.upper(), DEFAULT_SPECS)
        interval = specs["interval"]
        return round(target_strike / interval) * interval

    def get_premium_and_greeks(self, underlying: str, option_type: str,
                                strike: float, expiry: str, dte: float,
                                spot: float, vol: float) -> Tuple[float, float, dict]:
        """Estimate premium using Black-Scholes. Returns (mark_pct, premium_usd, greeks)."""
        if vol <= 0:
            vol = 0.80  # crypto default
        price_usd, greeks = bs_price_and_greeks(spot, strike, dte, vol, option_type=option_type)
        mark_pct = (price_usd / spot) if spot > 0 else 0.0
        return round(mark_pct, 6), round(price_usd, 2), greeks

    def get_multiplier(self, underlying: str) -> float:
        return CME_SPECS.get(underlying.upper(), DEFAULT_SPECS)["multiplier"]

    def get_strike_interval(self, underlying: str) -> float:
        return CME_SPECS.get(underlying.upper(), DEFAULT_SPECS)["interval"]
