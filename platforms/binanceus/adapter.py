
import sys
import os as _os
from typing import Optional, Tuple

sys.path.insert(0, _os.path.join(_os.path.dirname(_os.path.abspath(__file__)), '..', '..', 'shared_tools'))

import egress_policy
import option_data


def _get_ccxt_exchange():
    import ccxt
    return egress_policy.pin_ccxt(ccxt.binanceus({"enableRateLimit": True}))


class BinanceUSExchangeAdapter:

    @property
    def name(self) -> str:
        return "binanceus"

    def get_spot_price(self, underlying: str) -> float:
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
        try:
            exchange = _get_ccxt_exchange()
            ohlcv = exchange.fetch_ohlcv(underlying + "/USDT", "1d", limit=90)
        except Exception as e:
            raise option_data.unavailable("volatility unavailable: daily history read failed", e) from e
        return option_data.realized_vol_metrics(option_data.daily_closes(ohlcv), 365)

    def get_real_expiry(self, underlying: str, target_dte: int) -> Tuple[str, int]:
        raise NotImplementedError("BinanceUS does not support options")

    def get_real_strike(self, underlying: str, expiry: str,
                        option_type: str, target_strike: float) -> float:
        raise NotImplementedError("BinanceUS does not support options")

    def get_premium_and_greeks(self, underlying: str, option_type: str,
                                strike: float, expiry: str, dte: float,
                                spot: float, vol: Optional[float] = None) -> Tuple[float, float, dict]:
        raise NotImplementedError("BinanceUS does not support options")
