
import sys
import os as _os
from typing import Optional, Tuple

sys.path.insert(0, _os.path.join(_os.path.dirname(_os.path.abspath(__file__)), '..', '..', 'shared_tools'))

import egress_policy
import option_data

_LUNO_QUOTE_CURRENCIES = ["ZAR", "GBP", "EUR", "MYR", "NGN"]


def _get_ccxt_exchange():
    import ccxt
    api_key = _os.environ.get("LUNO_API_KEY", "")
    api_secret = _os.environ.get("LUNO_API_SECRET", "")
    params = {"enableRateLimit": True}
    if api_key and api_secret:
        params["apiKey"] = api_key
        params["secret"] = api_secret
    return egress_policy.pin_ccxt(ccxt.luno(params))


class LunoExchangeAdapter:

    @property
    def name(self) -> str:
        return "luno"

    def get_spot_price(self, underlying: str) -> float:
        exchange = _get_ccxt_exchange()
        for quote in _LUNO_QUOTE_CURRENCIES:
            try:
                ticker = exchange.fetch_ticker(underlying + "/" + quote)
                price = ticker.get("last") or 0
                if price and price > 0:
                    return float(price)
            except Exception:
                continue
        return 0.0

    def get_vol_metrics(self, underlying: str) -> Tuple[float, float]:
        try:
            exchange = _get_ccxt_exchange()
        except Exception as e:
            raise option_data.unavailable("volatility unavailable: daily history read failed", e) from e
        ohlcv = None
        last_error = None
        for quote in _LUNO_QUOTE_CURRENCIES:
            try:
                ohlcv = exchange.fetch_ohlcv(underlying + "/" + quote, "1d", limit=90)
            except Exception as e:
                last_error = e
                continue
            if ohlcv and len(ohlcv) >= option_data.VOL_WINDOW + 1:
                break
        if not ohlcv and last_error is not None:
            raise option_data.unavailable("volatility unavailable: daily history read failed", last_error) from last_error
        return option_data.realized_vol_metrics(option_data.daily_closes(ohlcv or []), 365)

    def get_real_expiry(self, underlying: str, target_dte: int) -> Tuple[str, int]:
        raise NotImplementedError("Luno does not support options")

    def get_real_strike(self, underlying: str, expiry: str,
                        option_type: str, target_strike: float) -> float:
        raise NotImplementedError("Luno does not support options")

    def get_premium_and_greeks(self, underlying: str, option_type: str,
                                strike: float, expiry: str, dte: float,
                                spot: float, vol: Optional[float] = None) -> Tuple[float, float, dict]:
        raise NotImplementedError("Luno does not support options")
