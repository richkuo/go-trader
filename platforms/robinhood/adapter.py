"""
Robinhood Exchange Adapter.

Supports crypto spot trading and US stock options.
Paper mode: yfinance for OHLCV/prices, Black-Scholes for options pricing.
Live mode: robin_stocks for prices/chains/orders, TOTP MFA authentication.

Environment variables:
    ROBINHOOD_USERNAME     — Robinhood account email/username
    ROBINHOOD_PASSWORD     — Robinhood account password
    ROBINHOOD_TOTP_SECRET  — TOTP secret for MFA (base32 string from authenticator setup)
"""

import os
import sys
import math
from datetime import datetime, timezone, timedelta
from typing import Tuple

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), '..', '..', 'shared_tools'))

# Yahoo Finance crypto symbol mapping (paper mode OHLCV + fallback prices)
YAHOO_CRYPTO_MAP = {
    "BTC": "BTC-USD",
    "ETH": "ETH-USD",
    "SOL": "SOL-USD",
    "DOGE": "DOGE-USD",
    "AVAX": "AVAX-USD",
    "LINK": "LINK-USD",
    "ADA": "ADA-USD",
    "DOT": "DOT-USD",
    "MATIC": "MATIC-USD",
    "SHIB": "SHIB-USD",
}

# Standard equity options strike intervals
STRIKE_INTERVALS = [
    (100, 1),    # under $100: $1 increments
    (500, 5),    # $100-$500: $5 increments
    (float('inf'), 10),  # over $500: $10 increments
]


def _get_strike_interval(price: float) -> float:
    """Return standard equity options strike interval for given price."""
    for threshold, interval in STRIKE_INTERVALS:
        if price < threshold:
            return interval
    return 10


class RobinhoodExchangeAdapter:
    """
    Exchange adapter for Robinhood crypto + stock options trading.

    Paper mode:  no credentials needed; uses yfinance for OHLCV and price data,
                 Black-Scholes for options pricing.
    Live mode:   requires ROBINHOOD_USERNAME, ROBINHOOD_PASSWORD, ROBINHOOD_TOTP_SECRET;
                 uses robin_stocks for live prices, options chains, and order execution.
    """

    def __init__(self, mode="paper"):
        self._mode = mode
        self._logged_in = False

        if mode == "live":
            self._login()
        else:
            # Paper mode: attempt login for live prices, but don't fail if missing
            try:
                self._login()
            except Exception:
                pass

    def _login(self):
        """Authenticate with Robinhood via robin_stocks + TOTP."""
        username = os.environ.get("ROBINHOOD_USERNAME", "")
        password = os.environ.get("ROBINHOOD_PASSWORD", "")
        totp_secret = os.environ.get("ROBINHOOD_TOTP_SECRET", "")

        if not username or not password or not totp_secret:
            if self._mode == "live":
                raise RuntimeError(
                    "Live mode requires ROBINHOOD_USERNAME, ROBINHOOD_PASSWORD, "
                    "and ROBINHOOD_TOTP_SECRET environment variables"
                )
            return

        import robin_stocks.robinhood as rh
        import pyotp

        totp = pyotp.TOTP(totp_secret).now()
        rh.login(username, password, mfa_code=totp)
        self._logged_in = True

    @property
    def is_live(self) -> bool:
        return self._mode == "live" and self._logged_in

    @property
    def mode(self) -> str:
        return self._mode

    @property
    def name(self) -> str:
        return "robinhood"

    # ─────────────────────────────────────────────
    # Market data (crypto + stocks)
    # ─────────────────────────────────────────────

    def _resolve_yahoo_symbol(self, symbol: str) -> str:
        """Resolve symbol to yfinance ticker. Crypto uses map, stocks pass through."""
        return YAHOO_CRYPTO_MAP.get(symbol.upper(), symbol.upper())

    def get_price(self, symbol: str) -> float:
        """Get current price. Uses robin_stocks if logged in, else yfinance."""
        if self._logged_in:
            try:
                import robin_stocks.robinhood as rh
                # Try crypto first
                if symbol.upper() in YAHOO_CRYPTO_MAP:
                    quote = rh.crypto.get_crypto_quote(symbol)
                    if quote and quote.get("mark_price"):
                        return float(quote["mark_price"])
                else:
                    # Stock quote
                    prices = rh.stocks.get_latest_price(symbol)
                    if prices and prices[0]:
                        return float(prices[0])
            except Exception as e:
                print(f"[robinhood] robin_stocks price error for {symbol}: {e}", file=sys.stderr)
        return self._get_yahoo_price(symbol)

    def get_spot_price(self, symbol: str) -> float:
        """Alias for get_price (protocol compatibility)."""
        return self.get_price(symbol)

    def get_ohlcv(self, symbol: str, interval: str = "1h", limit: int = 200) -> list:
        """
        Fetch OHLCV candles via yfinance.

        Returns list of [timestamp_ms, open, high, low, close, volume].
        """
        return self._get_yahoo_ohlcv(symbol, interval, limit)

    def get_ohlcv_closes(self, symbol: str, timeframe: str = "4h", limit: int = 100, min_len: int = 30) -> list:
        """Fetch closing prices via yfinance. Used by check_options.py momentum strategy."""
        candles = self._get_yahoo_ohlcv(symbol, timeframe, limit)
        if not candles or len(candles) < min_len:
            return None
        return [c[4] for c in candles]

    # ─────────────────────────────────────────────
    # Options Protocol methods
    # ─────────────────────────────────────────────

    def get_vol_metrics(self, underlying: str) -> Tuple[float, float]:
        """Compute 14-day historical vol and IV rank from yfinance OHLCV data."""
        try:
            import yfinance as yf
            yahoo_sym = self._resolve_yahoo_symbol(underlying)
            ticker = yf.Ticker(yahoo_sym)
            hist = ticker.history(period="90d", interval="1d")
            if hist.empty or len(hist) < 15:
                return 0.30, 50.0
            closes = hist["Close"].tolist()
            returns = [math.log(closes[i] / closes[i - 1]) for i in range(1, len(closes)) if closes[i - 1] > 0]
            if len(returns) < 14:
                return 0.30, 50.0

            w = 14
            mean = sum(returns[-w:]) / w
            variance = sum((r - mean) ** 2 for r in returns[-w:]) / w
            vol = math.sqrt(variance) * math.sqrt(252)  # 252 trading days for stocks

            # IV rank (rolling HV comparison)
            hvs = []
            for i in range(len(returns) - w + 1):
                chunk = returns[i:i + w]
                m = sum(chunk) / w
                v = sum((r - m) ** 2 for r in chunk) / w
                hvs.append(math.sqrt(v) * math.sqrt(252) * 100)
            current_hv = vol * 100
            hv_min, hv_max = min(hvs), max(hvs)
            if hv_max > hv_min:
                iv_rank = (current_hv - hv_min) / (hv_max - hv_min) * 100
                iv_rank = round(min(max(iv_rank, 0.0), 100.0), 1)
            else:
                iv_rank = 50.0

            return round(vol, 4), iv_rank
        except Exception as e:
            print(f"[robinhood] vol_metrics error for {underlying}: {e}", file=sys.stderr)
            return 0.30, 50.0

    def get_real_expiry(self, underlying: str, target_dte: int) -> Tuple[str, int]:
        """Return options expiry closest to target_dte.

        Live mode: fetches real expiry dates from Robinhood.
        Paper mode: synthetic expiry (today + target_dte).
        """
        if self._logged_in:
            try:
                import robin_stocks.robinhood as rh
                chain = rh.options.get_chains(underlying)
                if chain and chain.get("expiration_dates"):
                    today = datetime.now(timezone.utc).date()
                    target_date = today + timedelta(days=target_dte)
                    best_expiry = None
                    best_diff = float('inf')
                    for exp_str in chain["expiration_dates"]:
                        exp_date = datetime.strptime(exp_str, "%Y-%m-%d").date()
                        diff = abs((exp_date - target_date).days)
                        if diff < best_diff:
                            best_diff = diff
                            best_expiry = exp_str
                    if best_expiry:
                        actual_dte = (datetime.strptime(best_expiry, "%Y-%m-%d").date() - today).days
                        return best_expiry, max(actual_dte, 1)
            except Exception as e:
                print(f"[robinhood] get_real_expiry error: {e}", file=sys.stderr)

        # Paper mode fallback: synthetic expiry
        expiry_dt = datetime.now(timezone.utc) + timedelta(days=target_dte)
        return expiry_dt.strftime("%Y-%m-%d"), target_dte

    def get_real_strike(self, underlying: str, expiry: str,
                        option_type: str, target_strike: float) -> float:
        """Return strike closest to target.

        Live mode: fetches real strikes from Robinhood.
        Paper mode: rounds to standard equity options interval.
        """
        if self._logged_in:
            try:
                import robin_stocks.robinhood as rh
                options = rh.options.find_tradable_options(
                    underlying, expirationDate=expiry,
                    optionType=option_type
                )
                if options:
                    best_strike = None
                    best_diff = float('inf')
                    for opt in options:
                        strike = float(opt.get("strike_price", 0))
                        if strike > 0:
                            diff = abs(strike - target_strike)
                            if diff < best_diff:
                                best_diff = diff
                                best_strike = strike
                    if best_strike is not None:
                        return best_strike
            except Exception as e:
                print(f"[robinhood] get_real_strike error: {e}", file=sys.stderr)

        # Paper mode fallback: round to standard interval
        interval = _get_strike_interval(target_strike)
        return round(target_strike / interval) * interval

    def get_premium_and_greeks(self, underlying: str, option_type: str,
                                strike: float, expiry: str, dte: float,
                                spot: float, vol: float) -> Tuple[float, float, dict]:
        """Estimate premium and greeks.

        Live mode: fetches real market data from Robinhood.
        Paper mode: Black-Scholes via shared_tools/pricing.py.
        """
        if self._logged_in:
            try:
                import robin_stocks.robinhood as rh
                options = rh.options.find_tradable_options(
                    underlying, expirationDate=expiry,
                    strikePrice=str(strike), optionType=option_type
                )
                if options:
                    opt_id = options[0].get("id") or options[0].get("url", "").split("/")[-2]
                    if opt_id:
                        market_data = rh.options.get_option_market_data_by_id(opt_id)
                        if market_data and isinstance(market_data, list) and market_data[0]:
                            md = market_data[0]
                            mark = float(md.get("adjusted_mark_price", 0) or 0)
                            greeks = {
                                "delta": float(md.get("delta", 0) or 0),
                                "gamma": float(md.get("gamma", 0) or 0),
                                "theta": float(md.get("theta", 0) or 0),
                                "vega": float(md.get("vega", 0) or 0),
                            }
                            # mark is per-share price, multiply by 100 for per-contract
                            premium_usd = mark * 100
                            mark_pct = (mark / spot) if spot > 0 else 0.0
                            return round(mark_pct, 6), round(premium_usd, 2), greeks
            except Exception as e:
                print(f"[robinhood] get_premium_and_greeks error: {e}", file=sys.stderr)

        # Paper mode fallback: Black-Scholes
        from pricing import bs_price_and_greeks
        if vol <= 0:
            vol = 0.30  # stock default
        price_usd, greeks = bs_price_and_greeks(spot, strike, dte, vol, option_type=option_type)
        # price_usd is per-share; multiply by 100 for per-contract
        premium_usd = price_usd * 100
        mark_pct = (price_usd / spot) if spot > 0 else 0.0
        return round(mark_pct, 6), round(premium_usd, 2), greeks

    # ─────────────────────────────────────────────
    # Yahoo Finance helpers
    # ─────────────────────────────────────────────

    def _get_yahoo_price(self, symbol: str) -> float:
        """Fetch current price via yfinance. Works for both crypto and stocks."""
        yahoo_sym = self._resolve_yahoo_symbol(symbol)
        try:
            import yfinance as yf
            ticker = yf.Ticker(yahoo_sym)
            hist = ticker.history(period="1d")
            if hist.empty:
                return 0.0
            return float(hist["Close"].iloc[-1])
        except ImportError:
            print("[robinhood] yfinance not installed. Run: uv add yfinance", file=sys.stderr)
            return 0.0
        except Exception as e:
            print(f"[robinhood] yahoo price error for {symbol}: {e}", file=sys.stderr)
            return 0.0

    def _get_yahoo_ohlcv(self, symbol: str, interval: str = "1h", limit: int = 200) -> list:
        """Fetch OHLCV via yfinance. Works for both crypto and stocks."""
        yahoo_sym = self._resolve_yahoo_symbol(symbol)
        try:
            import yfinance as yf
            yf_interval = interval
            if "m" in interval:
                period = "5d"
            elif interval in ("1h", "60m"):
                period = "30d"
            else:
                period = "1y"
            ticker = yf.Ticker(yahoo_sym)
            hist = ticker.history(period=period, interval=yf_interval)
            if hist.empty:
                return []
            result = []
            for idx, row in hist.iterrows():
                ts_ms = int(idx.timestamp() * 1000)
                result.append([
                    ts_ms,
                    float(row["Open"]),
                    float(row["High"]),
                    float(row["Low"]),
                    float(row["Close"]),
                    float(row.get("Volume", 0)),
                ])
            return result[-limit:]
        except ImportError:
            print("[robinhood] yfinance not installed. Run: uv add yfinance", file=sys.stderr)
            return []
        except Exception as e:
            print(f"[robinhood] yahoo ohlcv error for {symbol}: {e}", file=sys.stderr)
            return []

    # ─────────────────────────────────────────────
    # Order execution (live mode only)
    # ─────────────────────────────────────────────

    def market_buy(self, symbol: str, amount_usd: float) -> dict:
        """
        Buy crypto by USD amount. Live mode only.
        Returns robin_stocks order response dict.
        """
        if not self.is_live:
            raise RuntimeError("market_buy requires live mode")
        import robin_stocks.robinhood as rh
        result = rh.orders.order_buy_crypto_by_price(symbol, amount_usd)
        return result or {}

    def market_sell(self, symbol: str, quantity: float) -> dict:
        """
        Sell crypto by quantity. Live mode only.
        Returns robin_stocks order response dict.
        """
        if not self.is_live:
            raise RuntimeError("market_sell requires live mode")
        import robin_stocks.robinhood as rh
        result = rh.orders.order_sell_crypto_by_quantity(symbol, quantity)
        return result or {}

    def get_crypto_positions(self) -> list:
        """Get current crypto positions from Robinhood.

        Best-effort variant: swallows exceptions and returns []. Suitable for
        signal/strategy paths where a transient Robinhood outage shouldn't
        crash the cycle. **Do not use for kill-switch flat-confirmation** —
        a silent [] there would clear virtual state while live exposure
        remained (see get_crypto_positions_strict, #346 review).
        """
        if not self._logged_in:
            return []
        try:
            return self.get_crypto_positions_strict()
        except Exception as e:
            print(f"[robinhood] get_crypto_positions error: {e}", file=sys.stderr)
            return []

    def get_crypto_positions_strict(self) -> list:
        """Strict variant of get_crypto_positions used by the kill-switch
        position fetcher (#346). Propagates every error rather than masking
        it as an empty list — the caller (fetch_robinhood_positions.py)
        relies on exceptions to emit a JSON error envelope so the Go-side
        kill switch latches instead of clearing virtual state on a silent
        empty.

        Raises RuntimeError if the adapter never authenticated.
        """
        if not self._logged_in:
            raise RuntimeError(
                "Robinhood adapter not logged in — cannot fetch crypto positions"
            )
        import robin_stocks.robinhood as rh
        positions = rh.crypto.get_crypto_positions()
        result = []
        for pos in positions:
            qty = float(pos.get("quantity", 0) or 0)
            if qty <= 0:
                continue
            currency = pos.get("currency", {})
            symbol = currency.get("code", "")
            cost_basis = float(pos.get("cost_bases", [{}])[0].get("direct_cost_basis", 0) or 0)
            avg_price = cost_basis / qty if qty > 0 else 0
            result.append({
                "symbol": symbol,
                "quantity": qty,
                "avg_price": avg_price,
            })
        return result
