
import contextlib
import os
import sys
import math
import re
import time
from datetime import datetime, timezone, timedelta
from typing import Tuple
from urllib.parse import urlsplit

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), '..', '..', 'shared_tools'))

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

STRIKE_INTERVALS = [
    (100, 1),
    (500, 5),
    (float('inf'), 10),
]

_HOLDINGS_ERROR_PREFIX = "Robinhood holdings read failed:"
_HOLDINGS_CONNECT_TIMEOUT_S = 5.0
_HOLDINGS_READ_TIMEOUT_S = 10.0
_HOLDINGS_TOTAL_DEADLINE_S = 15.0
_HOLDINGS_PAGE_CAP = 50


class _HoldingsReadError(RuntimeError):
    pass


_DECIMAL_TEXT = re.compile(r"[+-]?(?:\d+(?:\.\d*)?|\.\d+)(?:[eE][+-]?\d+)?")


def _parse_decimal(value):
    if isinstance(value, bool):
        return None
    if isinstance(value, (int, float)):
        number = float(value)
    elif isinstance(value, str):
        text = value.strip()
        if not _DECIMAL_TEXT.fullmatch(text):
            return None
        number = float(text)
    else:
        return None
    if not math.isfinite(number):
        return None
    return number


def _holding_avg_price(row, quantity):
    cost_bases = row.get("cost_bases")
    if not isinstance(cost_bases, list) or not cost_bases:
        return 0.0
    first = cost_bases[0]
    if not isinstance(first, dict):
        return 0.0
    basis = _parse_decimal(first.get("direct_cost_basis"))
    if basis is None or basis < 0:
        return 0.0
    avg_price = basis / quantity
    if not math.isfinite(avg_price):
        return 0.0
    return avg_price


def _parse_holding_row(row, page, index):
    where = f"page {page} row {index}"
    if not isinstance(row, dict):
        raise _HoldingsReadError(f"{where}: row is not an object")
    if "quantity" not in row:
        raise _HoldingsReadError(f"{where}: quantity is missing")
    quantity = _parse_decimal(row["quantity"])
    if quantity is None:
        raise _HoldingsReadError(f"{where}: quantity is not a finite number")
    if quantity < 0:
        raise _HoldingsReadError(f"{where}: quantity is negative")
    if quantity == 0:
        return None
    currency = row.get("currency")
    code = currency.get("code") if isinstance(currency, dict) else None
    if not isinstance(code, str) or not code.strip():
        raise _HoldingsReadError(f"{where}: held row has no currency code")
    return {
        "symbol": code.strip().upper(),
        "quantity": quantity,
        "avg_price": _holding_avg_price(row, quantity),
    }


def _get_strike_interval(price: float) -> float:
    for threshold, interval in STRIKE_INTERVALS:
        if price < threshold:
            return interval
    return 10


@contextlib.contextmanager
def robinhood_library():
    with contextlib.redirect_stdout(sys.stderr):
        import robin_stocks.robinhood as rh
        rh.helper.set_output(sys.stderr)
        yield rh


class RobinhoodExchangeAdapter:

    def __init__(self, mode="paper"):
        self._mode = mode
        self._logged_in = False

        if mode == "live":
            self._login()
        else:
            try:
                self._login()
            except Exception:
                pass

    def _login(self):
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

        import pyotp

        totp = pyotp.TOTP(totp_secret).now()
        with robinhood_library() as rh:
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


    def _resolve_yahoo_symbol(self, symbol: str) -> str:
        return YAHOO_CRYPTO_MAP.get(symbol.upper(), symbol.upper())

    def get_price(self, symbol: str) -> float:
        if self._logged_in:
            try:
                with robinhood_library() as rh:
                    if symbol.upper() in YAHOO_CRYPTO_MAP:
                        quote = rh.crypto.get_crypto_quote(symbol)
                        if quote and quote.get("mark_price"):
                            return float(quote["mark_price"])
                    else:
                        prices = rh.stocks.get_latest_price(symbol)
                        if prices and prices[0]:
                            return float(prices[0])
            except Exception as e:
                print(f"[robinhood] robin_stocks price error for {symbol}: {e}", file=sys.stderr)
        return self._get_yahoo_price(symbol)

    def get_spot_price(self, symbol: str) -> float:
        return self.get_price(symbol)

    def get_ohlcv(self, symbol: str, interval: str = "1h", limit: int = 200) -> list:
        return self._get_yahoo_ohlcv(symbol, interval, limit)

    def get_ohlcv_closes(self, symbol: str, timeframe: str = "4h", limit: int = 100, min_len: int = 30) -> list:
        candles = self._get_yahoo_ohlcv(symbol, timeframe, limit)
        if not candles or len(candles) < min_len:
            return None
        return [c[4] for c in candles]


    def get_vol_metrics(self, underlying: str) -> Tuple[float, float]:
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
            vol = math.sqrt(variance) * math.sqrt(252)

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
        if self._logged_in:
            try:
                with robinhood_library() as rh:
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

        expiry_dt = datetime.now(timezone.utc) + timedelta(days=target_dte)
        return expiry_dt.strftime("%Y-%m-%d"), target_dte

    def get_real_strike(self, underlying: str, expiry: str,
                        option_type: str, target_strike: float) -> float:
        if self._logged_in:
            try:
                with robinhood_library() as rh:
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

        interval = _get_strike_interval(target_strike)
        return round(target_strike / interval) * interval

    def get_premium_and_greeks(self, underlying: str, option_type: str,
                                strike: float, expiry: str, dte: float,
                                spot: float, vol: float) -> Tuple[float, float, dict]:
        if self._logged_in:
            try:
                with robinhood_library() as rh:
                    options = rh.options.find_tradable_options(
                        underlying, expirationDate=expiry,
                        strikePrice=str(strike), optionType=option_type
                    )
                if options:
                    opt_id = options[0].get("id") or options[0].get("url", "").split("/")[-2]
                    if opt_id:
                        with robinhood_library() as rh:
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
                            premium_usd = mark * 100
                            mark_pct = (mark / spot) if spot > 0 else 0.0
                            return round(mark_pct, 6), round(premium_usd, 2), greeks
            except Exception as e:
                print(f"[robinhood] get_premium_and_greeks error: {e}", file=sys.stderr)

        from pricing import bs_price_and_greeks
        if vol <= 0:
            vol = 0.30
        price_usd, greeks = bs_price_and_greeks(spot, strike, dte, vol, option_type=option_type)
        premium_usd = price_usd * 100
        mark_pct = (price_usd / spot) if spot > 0 else 0.0
        return round(mark_pct, 6), round(premium_usd, 2), greeks


    def _get_yahoo_price(self, symbol: str) -> float:
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


    def market_buy(self, symbol: str, amount_usd: float) -> dict:
        if not self.is_live:
            raise RuntimeError("market_buy requires live mode")
        with robinhood_library() as rh:
            result = rh.orders.order_buy_crypto_by_price(symbol, amount_usd)
        return result or {}

    def market_sell(self, symbol: str, quantity: float) -> dict:
        if not self.is_live:
            raise RuntimeError("market_sell requires live mode")
        with robinhood_library() as rh:
            result = rh.orders.order_sell_crypto_by_quantity(symbol, quantity)
        return result or {}

    def get_crypto_positions_strict(self) -> list:
        try:
            return self._read_crypto_holdings()
        except _HoldingsReadError as e:
            raise RuntimeError(f"{_HOLDINGS_ERROR_PREFIX} {e}") from e
        except Exception as e:
            raise RuntimeError(f"{_HOLDINGS_ERROR_PREFIX} unexpected {type(e).__name__}") from e

    def _read_crypto_holdings(self) -> list:
        if not self._logged_in:
            raise _HoldingsReadError(
                "Robinhood adapter not logged in — cannot fetch crypto positions"
            )
        import requests

        with robinhood_library() as rh:
            logged_in = rh.helper.LOGGED_IN
            session = rh.helper.SESSION
            first_url = rh.urls.crypto_holdings_url()

        if logged_in is not True:
            raise _HoldingsReadError("robin_stocks session not logged in")
        authorization = session.headers.get("Authorization")
        if not isinstance(authorization, str) or not authorization.strip():
            raise _HoldingsReadError("robin_stocks session has no Authorization header")

        holdings_host = urlsplit(first_url).netloc
        deadline = time.monotonic() + _HOLDINGS_TOTAL_DEADLINE_S
        requested = set()
        result = []
        seen_symbols = set()
        url = first_url
        page = 0
        while url:
            page += 1
            if page > _HOLDINGS_PAGE_CAP:
                raise _HoldingsReadError(f"more than {_HOLDINGS_PAGE_CAP} holdings pages")
            if url in requested:
                raise _HoldingsReadError(f"page {page}: repeated next link")
            requested.add(url)
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise _HoldingsReadError(f"page {page}: read deadline of {_HOLDINGS_TOTAL_DEADLINE_S}s exceeded")
            timeout = (
                min(_HOLDINGS_CONNECT_TIMEOUT_S, remaining),
                min(_HOLDINGS_READ_TIMEOUT_S, remaining),
            )
            try:
                res = session.get(url, timeout=timeout, allow_redirects=False)
            except requests.exceptions.RequestException as e:
                raise _HoldingsReadError(f"page {page}: request failed ({type(e).__name__})") from e
            if res.status_code != 200:
                raise _HoldingsReadError(f"page {page}: HTTP status {res.status_code}")
            try:
                body = res.json()
            except ValueError as e:
                raise _HoldingsReadError(f"page {page}: response is not valid JSON") from e
            if not isinstance(body, dict):
                raise _HoldingsReadError(f"page {page}: response is not a JSON object")
            rows = body.get("results")
            if not isinstance(rows, list):
                raise _HoldingsReadError(f"page {page}: response has no results list")
            if "next" not in body:
                raise _HoldingsReadError(f"page {page}: response has no next key")
            next_url = body["next"]
            if next_url is None or next_url == "":
                next_url = None
            elif not isinstance(next_url, str):
                raise _HoldingsReadError(f"page {page}: next link is not a string")
            else:
                parts = urlsplit(next_url)
                if parts.scheme != "https" or parts.netloc != holdings_host:
                    raise _HoldingsReadError(f"page {page}: next link is not https on {holdings_host}")
            for index, row in enumerate(rows):
                parsed = _parse_holding_row(row, page, index)
                if parsed is None:
                    continue
                if parsed["symbol"] in seen_symbols:
                    raise _HoldingsReadError(
                        f"page {page} row {index}: duplicate holding for {parsed['symbol']}"
                    )
                seen_symbols.add(parsed["symbol"])
                result.append(parsed)
            url = next_url
        return result
