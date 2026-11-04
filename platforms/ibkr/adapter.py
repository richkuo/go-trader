
import sys
import os as _os
from datetime import datetime, timezone, timedelta
from typing import Optional, Tuple

sys.path.insert(0, _os.path.join(_os.path.dirname(_os.path.abspath(__file__)), '..', '..', 'shared_tools'))

import egress_policy
import option_data
from pricing import bs_price_and_greeks

CME_SPECS = {
    "BTC": {"interval": 1000, "multiplier": 0.1},
    "ETH": {"interval": 50,   "multiplier": 0.5},
}
DEFAULT_SPECS = {"interval": 100, "multiplier": 1.0}


def _get_spot_price(underlying: str) -> float:
    import ccxt
    exchange = egress_policy.pin_ccxt(ccxt.binanceus({"enableRateLimit": True}))
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
    try:
        import ccxt
        exchange = egress_policy.pin_ccxt(ccxt.binanceus({"enableRateLimit": True}))
        ohlcv = exchange.fetch_ohlcv(underlying + "/USDT", "1d", limit=90)
    except Exception as e:
        raise option_data.unavailable("volatility unavailable: daily history read failed", e) from e
    return option_data.realized_vol_metrics(option_data.daily_closes(ohlcv), 365)


class IBKRExchangeAdapter:

    @property
    def name(self) -> str:
        return "ibkr"

    def get_spot_price(self, underlying: str) -> float:
        return _get_spot_price(underlying)

    def get_vol_metrics(self, underlying: str) -> Tuple[float, float]:
        return _calc_vol_and_iv_rank(underlying)

    def get_real_expiry(self, underlying: str, target_dte: int) -> Tuple[str, int]:
        expiry_dt = datetime.now(timezone.utc) + timedelta(days=target_dte)
        return expiry_dt.strftime("%Y-%m-%d"), target_dte

    def get_real_strike(self, underlying: str, expiry: str,
                        option_type: str, target_strike: float) -> float:
        specs = CME_SPECS.get(underlying.upper(), DEFAULT_SPECS)
        interval = specs["interval"]
        return round(target_strike / interval) * interval

    def get_premium_and_greeks(self, underlying: str, option_type: str,
                                strike: float, expiry: str, dte: float,
                                spot: float, vol: Optional[float] = None) -> Tuple[float, float, dict]:
        spot, strike, dte, vol = option_data.require_model_inputs(spot, strike, dte, vol)
        price_usd, greeks = bs_price_and_greeks(spot, strike, dte, vol, option_type=option_type)
        mark_pct = price_usd / spot
        return round(mark_pct, 6), round(price_usd, 2), greeks

    def get_multiplier(self, underlying: str) -> float:
        return CME_SPECS.get(underlying.upper(), DEFAULT_SPECS)["multiplier"]

    def get_strike_interval(self, underlying: str) -> float:
        return CME_SPECS.get(underlying.upper(), DEFAULT_SPECS)["interval"]
