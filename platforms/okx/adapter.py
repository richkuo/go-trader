"""
OKX Exchange Adapter — unified interface for spot, perpetual swaps, and options.
Uses CCXT for all API interactions.

Supports paper (public API only, no credentials) and
live (real orders on OKX, API credentials required) modes.

Environment variables:
    OKX_API_KEY        — API key for live trading
    OKX_API_SECRET     — API secret for live trading
    OKX_PASSPHRASE     — API passphrase for live trading
    OKX_SANDBOX=1      — use OKX demo trading environment
"""

import os
import sys
import math
import time
from typing import Tuple

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), '..', '..', 'shared_tools'))

import ccxt


class OKXExchangeAdapter:
    """
    Exchange adapter for OKX — spot, perpetual swaps, and options.

    Paper mode:  no credentials needed; uses live OKX prices for simulation.
    Live mode:   requires OKX_API_KEY, OKX_API_SECRET, OKX_PASSPHRASE.
    """

    def __init__(self):
        api_key = os.environ.get("OKX_API_KEY", "")
        api_secret = os.environ.get("OKX_API_SECRET", "")
        passphrase = os.environ.get("OKX_PASSPHRASE", "")
        sandbox = os.environ.get("OKX_SANDBOX", "") == "1"

        config = {
            "enableRateLimit": True,
        }
        if sandbox:
            config["sandbox"] = True

        self._is_live = bool(api_key and api_secret and passphrase)
        if self._is_live:
            config["apiKey"] = api_key
            config["secret"] = api_secret
            config["password"] = passphrase

        self._exchange = ccxt.okx(config)
        self._markets_loaded = False

    @property
    def is_live(self) -> bool:
        """True if API credentials are provided (live mode)."""
        return self._is_live

    @property
    def mode(self) -> str:
        """'live' or 'paper'."""
        return "live" if self.is_live else "paper"

    @property
    def name(self) -> str:
        return "okx"

    # ─────────────────────────────────────────────
    # Market data
    # ─────────────────────────────────────────────

    def _load_markets(self):
        """Load and cache markets from OKX."""
        if not self._markets_loaded:
            self._exchange.load_markets()
            self._markets_loaded = True

    def get_spot_price(self, symbol: str) -> float:
        """Get current spot price for a coin (e.g. 'BTC')."""
        for suffix in ("/USDT", "/USD", "/USDC"):
            try:
                ticker = self._exchange.fetch_ticker(symbol + suffix)
                price = ticker.get("last") or 0
                if price and price > 0:
                    return float(price)
            except Exception:
                continue
        return 0.0

    def get_ohlcv(self, symbol: str, interval: str = "1h", limit: int = 200) -> list:
        """
        Fetch OHLCV candles from OKX.

        interval: "1m", "5m", "15m", "30m", "1h", "2h", "4h", "1d", etc.
        Returns list of [timestamp_ms, open, high, low, close, volume].
        """
        pair = f"{symbol}/USDT"
        try:
            candles = self._exchange.fetch_ohlcv(pair, interval, limit=limit)
            return candles  # ccxt already returns [ts, o, h, l, c, v]
        except Exception:
            return []

    def get_ohlcv_closes(self, symbol: str, interval: str = "1h", limit: int = 200) -> list:
        """Fetch OHLCV and return just close prices (for HTF filter compatibility)."""
        candles = self.get_ohlcv(symbol, interval, limit)
        return [c[4] for c in candles] if candles else []

    def get_perp_ohlcv(self, symbol: str, interval: str = "1h", limit: int = 200) -> list:
        """Fetch OHLCV candles for perpetual swap (USDT-margined)."""
        pair = f"{symbol}/USDT:USDT"
        try:
            candles = self._exchange.fetch_ohlcv(pair, interval, limit=limit)
            return candles
        except Exception:
            return []

    def get_funding_rate(self, symbol: str) -> float:
        """Get current predicted funding rate for a perpetual swap.

        Returns the raw rate as a float (e.g. 0.0001 = 0.01% per 8h).
        """
        try:
            pair = f"{symbol}/USDT:USDT"
            data = self._exchange.fetch_funding_rate(pair)
            return float(data.get("fundingRate", 0) or 0)
        except Exception:
            return 0.0

    def get_funding_history(self, symbol: str, days: int = 7) -> list:
        """Get historical funding rate snapshots.

        Returns list of {"rate": float, "time": int} dicts, newest last.
        """
        try:
            pair = f"{symbol}/USDT:USDT"
            since = int((time.time() - days * 86400) * 1000)
            records = self._exchange.fetch_funding_rate_history(pair, since=since)
            return [
                {"rate": float(r.get("fundingRate", 0) or 0), "time": int(r.get("timestamp", 0))}
                for r in records
            ]
        except Exception:
            return []

    # ─────────────────────────────────────────────
    # Order execution (live mode only)
    # ─────────────────────────────────────────────

    def market_open(self, symbol: str, is_buy: bool, size: float, inst_type: str = "spot") -> dict:
        """
        Place a market order.

        inst_type: "spot" for spot trading, "swap" for perpetual swap.
        Only available in live mode; raises RuntimeError in paper mode.
        """
        if not self._is_live:
            raise RuntimeError(
                "market_open requires live mode (set OKX_API_KEY, OKX_API_SECRET, OKX_PASSPHRASE)"
            )
        side = "buy" if is_buy else "sell"
        if inst_type == "swap":
            pair = f"{symbol}/USDT:USDT"
            params = {"tdMode": "cross"}
        else:
            pair = f"{symbol}/USDT"
            params = {"tdMode": "cash"}
        return self._exchange.create_market_order(pair, side, size, params=params)

    def market_close(self, symbol: str) -> dict:
        """
        Close all open perpetual swap positions for a symbol.
        Only available in live mode; raises RuntimeError in paper mode.
        """
        if not self._is_live:
            raise RuntimeError(
                "market_close requires live mode (set OKX_API_KEY, OKX_API_SECRET, OKX_PASSPHRASE)"
            )
        pair = f"{symbol}/USDT:USDT"
        positions = self._exchange.fetch_positions([pair])
        for pos in positions:
            contracts = float(pos.get("contracts", 0) or 0)
            if contracts > 0:
                pos_side = pos.get("side", "")
                close_side = "sell" if pos_side == "long" else "buy"
                return self._exchange.create_market_order(
                    pair, close_side, contracts,
                    params={"tdMode": "cross", "reduceOnly": True}
                )
        return {}

    # ─────────────────────────────────────────────
    # Options Protocol methods
    # ─────────────────────────────────────────────

    def get_vol_metrics(self, underlying: str) -> Tuple[float, float]:
        """Compute 14-day historical vol and IV rank from daily OHLCV."""
        try:
            ohlcv = self._exchange.fetch_ohlcv(underlying + "/USDT", "1d", limit=90)
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
        """Return options expiry closest to target_dte.

        Returns (expiry_str: "YYYY-MM-DD", actual_dte: int).
        """
        self._load_markets()
        from datetime import datetime, timezone
        now = datetime.now(timezone.utc)

        expiries = set()
        for market in self._exchange.markets.values():
            if (market.get("type") == "option"
                    and market.get("base", "").upper() == underlying.upper()
                    and market.get("active", True)):
                exp = market.get("expiry")
                if exp:
                    expiries.add(int(exp))

        if not expiries:
            # Fallback: synthetic expiry
            from datetime import timedelta
            syn = now + timedelta(days=target_dte)
            return syn.strftime("%Y-%m-%d"), target_dte

        best_exp = None
        best_diff = float("inf")
        for exp_ts in expiries:
            exp_dt = datetime.fromtimestamp(exp_ts / 1000, tz=timezone.utc)
            dte = (exp_dt - now).days
            if dte < 0:
                continue
            diff = abs(dte - target_dte)
            if diff < best_diff:
                best_diff = diff
                best_exp = exp_dt
                best_dte = dte

        if best_exp is None:
            from datetime import timedelta
            syn = now + timedelta(days=target_dte)
            return syn.strftime("%Y-%m-%d"), target_dte

        return best_exp.strftime("%Y-%m-%d"), best_dte

    def get_real_strike(self, underlying: str, expiry: str,
                        option_type: str, target_strike: float) -> float:
        """Return strike closest to target_strike for given underlying/expiry/type."""
        self._load_markets()
        from datetime import datetime, timezone

        exp_dt = datetime.strptime(expiry, "%Y-%m-%d").replace(tzinfo=timezone.utc)
        exp_start = int(exp_dt.timestamp() * 1000)
        exp_end = exp_start + 86400 * 1000  # within same day

        strikes = []
        for market in self._exchange.markets.values():
            if (market.get("type") == "option"
                    and market.get("base", "").upper() == underlying.upper()
                    and market.get("optionType") == option_type
                    and market.get("active", True)):
                mkt_exp = market.get("expiry")
                if mkt_exp and exp_start <= int(mkt_exp) < exp_end:
                    strike = market.get("strike")
                    if strike:
                        strikes.append(float(strike))

        if not strikes:
            # Fallback: round to nearest 1000 for BTC, 100 for ETH
            if underlying.upper() == "BTC":
                return round(target_strike / 1000) * 1000
            elif underlying.upper() == "ETH":
                return round(target_strike / 100) * 100
            return round(target_strike / 50) * 50

        return min(strikes, key=lambda s: abs(s - target_strike))

    def get_premium_and_greeks(self, underlying: str, option_type: str,
                                strike: float, expiry: str, dte: float,
                                spot: float, vol: float) -> Tuple[float, float, dict]:
        """Estimate premium and Greeks.

        Returns (premium_pct, premium_usd, greeks_dict).
        Tries live OKX quote first, falls back to Black-Scholes.
        """
        # Try live quote
        try:
            self._load_markets()
            from datetime import datetime, timezone
            exp_dt = datetime.strptime(expiry, "%Y-%m-%d").replace(tzinfo=timezone.utc)
            exp_start = int(exp_dt.timestamp() * 1000)
            exp_end = exp_start + 86400 * 1000

            opt_char = "C" if option_type == "call" else "P"
            for sym, market in self._exchange.markets.items():
                if (market.get("type") == "option"
                        and market.get("base", "").upper() == underlying.upper()
                        and market.get("optionType") == option_type
                        and market.get("strike") == strike
                        and market.get("active", True)):
                    mkt_exp = market.get("expiry")
                    if mkt_exp and exp_start <= int(mkt_exp) < exp_end:
                        ticker = self._exchange.fetch_ticker(sym)
                        mark = ticker.get("last") or ticker.get("close") or 0
                        if mark and mark > 0:
                            premium_usd = float(mark) * spot  # OKX options priced in base currency
                            premium_pct = float(mark)
                            greeks = {
                                "delta": ticker.get("info", {}).get("delta", 0),
                                "gamma": ticker.get("info", {}).get("gamma", 0),
                                "theta": ticker.get("info", {}).get("theta", 0),
                                "vega": ticker.get("info", {}).get("vega", 0),
                            }
                            # Convert to floats
                            greeks = {k: float(v or 0) for k, v in greeks.items()}
                            return premium_pct, premium_usd, greeks
        except Exception:
            pass

        # Fallback: Black-Scholes
        try:
            from pricing import bs_price_and_greeks
            premium_usd, greeks = bs_price_and_greeks(spot, strike, dte, vol, option_type)
            premium_pct = premium_usd / spot if spot > 0 else 0
            return round(premium_pct, 6), round(premium_usd, 2), greeks
        except Exception:
            return 0.0, 0.0, {"delta": 0, "gamma": 0, "theta": 0, "vega": 0}
