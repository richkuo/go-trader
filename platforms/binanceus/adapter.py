"""
BinanceUS ExchangeAdapter — thin ccxt wrapper for spot trading only.
Options methods raise NotImplementedError (BinanceUS is spot-only).
"""

import sys
import os as _os
import math
from typing import Tuple

sys.path.insert(0, _os.path.join(_os.path.dirname(_os.path.abspath(__file__)), '..', '..', 'shared_tools'))


def _get_ccxt_exchange():
    import ccxt
    return ccxt.binanceus({"enableRateLimit": True})


class BinanceUSExchangeAdapter:
    """
    ExchangeAdapter for BinanceUS — spot trading only.
    Provides spot price and vol metrics; options methods are not supported.
    """

    @property
    def name(self) -> str:
        return "binanceus"

    def get_spot_price(self, underlying: str) -> float:
        """Fetch current spot price for underlying via BinanceUS."""
        exchange = _get_ccxt_exchange()
        for suffix in ("/USDT", "/USD", "/USDC"):
            try:
                ticker = exchange.fetch_ticker(underlying + suffix)
                price = ticker.get("last") or 0
                if price and price > 0:
                    return float(price)
            except Exception:
                continue
        return 0.0

    def get_vol_metrics(self, underlying: str) -> Tuple[float, float]:
        """Compute 14-day historical vol and IV rank from daily OHLCV."""
        try:
            exchange = _get_ccxt_exchange()
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

    def get_real_expiry(self, underlying: str, target_dte: int) -> Tuple[str, int]:
        raise NotImplementedError("BinanceUS does not support options")

    def get_real_strike(self, underlying: str, expiry: str,
                        option_type: str, target_strike: float) -> float:
        raise NotImplementedError("BinanceUS does not support options")

    def get_premium_and_greeks(self, underlying: str, option_type: str,
                                strike: float, expiry: str, dte: float,
                                spot: float, vol: float) -> Tuple[float, float, dict]:
        raise NotImplementedError("BinanceUS does not support options")
