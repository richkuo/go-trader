
import contextlib
import os
import signal
import sys
import math
import re
import time
import uuid
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

_ORDER_CONNECT_TIMEOUT_S = 5.0
_ORDER_READ_TIMEOUT_S = 10.0
_LOGIN_WATCHDOG_CAP_S = 15.0
_CANCEL_RESERVE_S = 12.0
_CANCEL_RESERVE_SHARE = 0.4
_FINAL_MARGIN_S = 1.0
_POLL_INTERVAL_START_S = 0.5
_POLL_INTERVAL_MAX_S = 2.0
_POLL_INTERVAL_GROWTH = 1.5
_ORDER_PAGE_CAP = 20
_SEND_ATTEMPTS = 3
_QTY_TOLERANCE = 1e-9
_SETTLE_WINDOW_MS = 10 * 60 * 1000

_FILLED_STATES = frozenset({"filled"})
_CANCELED_STATES = frozenset({"canceled", "cancelled"})
_REFUSED_STATES = frozenset({"rejected", "failed"})
_REFUSAL_STATUSES = frozenset({400, 401, 403, 404, 422})
_REFUSAL_KEYS = ("detail", "non_field_errors")

OUTCOME_FILLED = "filled"
OUTCOME_PARTIALLY_FILLED = "partially_filled"
OUTCOME_NO_FILL = "no_fill"
OUTCOME_REJECTED = "rejected"
OUTCOME_NOT_SENT = "not_sent"
OUTCOME_NOT_FOUND = "not_found"
OUTCOME_OPEN = "open"
OUTCOME_UNKNOWN = "unknown"
OUTCOME_INCONSISTENT = "inconsistent"
CONFIRMED_FILL_OUTCOMES = frozenset({OUTCOME_FILLED, OUTCOME_PARTIALLY_FILLED})

_EVIDENCE_KEYS = (
    "id", "ref_id", "state", "side", "currency_pair_id", "account_id",
    "quantity", "price", "cumulative_quantity", "average_price", "created_at",
)

_FEE_CONTAINER_KEYS = (
    "fee",
    "fees",
    "commission",
    "commission_paid",
    "total_fee",
    "totalFee",
    "estimated_fee",
    "regulatory_fee",
)

_FEE_VALUE_KEYS = ("cost", "amount", "total", "value")


class _HoldingsReadError(RuntimeError):
    pass


class _BudgetExpired(BaseException):
    pass


class _OrderPrepError(RuntimeError):
    pass


class _OrderEvidenceError(RuntimeError):
    pass


@contextlib.contextmanager
def _watchdog(seconds, what):
    if seconds <= 0:
        raise _BudgetExpired(f"{what}: the time budget is exhausted")

    def _expire(signum, frame):
        raise _BudgetExpired(f"{what}: the time budget of {seconds:.1f}s expired")

    previous = signal.signal(signal.SIGALRM, _expire)
    signal.setitimer(signal.ITIMER_REAL, seconds)
    try:
        yield
    finally:
        signal.setitimer(signal.ITIMER_REAL, 0)
        signal.signal(signal.SIGALRM, previous)


def _norm_state(value):
    if not isinstance(value, str):
        return ""
    return value.strip().lower()


def _evidence_value(value):
    if value is None:
        return None
    if isinstance(value, bool):
        return str(value).lower()
    if isinstance(value, (int, float)):
        if isinstance(value, float) and not math.isfinite(value):
            return None
        return str(value)
    if isinstance(value, str):
        return value
    return None


def _order_evidence(order):
    if not isinstance(order, dict):
        return {}
    return {key: _evidence_value(order.get(key)) for key in _EVIDENCE_KEYS if key in order}


def _float_or_none(value):
    if value is None:
        return None
    try:
        return float(value)
    except (TypeError, ValueError):
        return None


def _extract_fee_value(value, fee_context=False):
    if isinstance(value, dict):
        keys = _FEE_VALUE_KEYS if fee_context else _FEE_CONTAINER_KEYS
        for key in keys:
            fee = _extract_fee_value(value.get(key), fee_context=True)
            if fee is not None:
                return fee
        return None
    if isinstance(value, list):
        total = 0.0
        found = False
        for item in value:
            fee = _extract_fee_value(item, fee_context=fee_context)
            if fee is not None:
                total += fee
                found = True
        return total if found else None
    if fee_context:
        return _float_or_none(value)
    return None


def extract_order_fee(order):
    if not isinstance(order, dict):
        return None
    for key in _FEE_CONTAINER_KEYS:
        fee = _extract_fee_value(order.get(key), fee_context=True)
        if fee is not None:
            return fee if math.isfinite(fee) and fee >= 0 else None
    fee = _extract_fee_value(order.get("executions"))
    if fee is None or not math.isfinite(fee) or fee < 0:
        return None
    return fee


def _is_pre_send_failure(exc):
    import requests
    from urllib3.exceptions import NewConnectionError

    if isinstance(exc, requests.exceptions.ConnectTimeout):
        return True
    if not isinstance(exc, requests.exceptions.ConnectionError):
        return False
    seen = set()
    stack = [exc]
    while stack:
        item = stack.pop()
        if item is None or id(item) in seen:
            continue
        seen.add(id(item))
        if isinstance(item, NewConnectionError):
            return True
        if isinstance(item, BaseException):
            stack.extend(item.args)
            stack.append(item.__cause__)
            stack.append(item.__context__)
            stack.append(getattr(item, "reason", None))
    return False


def _parse_created_at_ms(value):
    if not isinstance(value, str) or not value.strip():
        return None
    text = value.strip()
    if text.endswith("Z") or text.endswith("z"):
        text = text[:-1] + "+00:00"
    try:
        stamp = datetime.fromisoformat(text)
    except ValueError:
        return None
    if stamp.tzinfo is None:
        return None
    return int(stamp.timestamp() * 1000)


def _relative_close(a, b):
    scale = max(abs(a), abs(b), 1e-18)
    return abs(a - b) / scale <= _QTY_TOLERANCE


def _evaluate_terminal(order, requested_quantity):
    state = _norm_state(order.get("state"))
    cumulative = _parse_decimal(order.get("cumulative_quantity"))
    raw_average = order.get("average_price")
    average = None if raw_average is None else _parse_decimal(raw_average)
    terminal = state in _FILLED_STATES or state in _CANCELED_STATES or state in _REFUSED_STATES
    if not terminal:
        return None, ""
    if cumulative is None or cumulative < 0:
        return OUTCOME_UNKNOWN, f"order {state} with an unreadable cumulative_quantity {order.get('cumulative_quantity')!r}"
    if raw_average is not None and average is None:
        return OUTCOME_UNKNOWN, f"order {state} with an unreadable average_price {raw_average!r}"
    if requested_quantity is not None and requested_quantity > 0 and cumulative > requested_quantity * (1 + _QTY_TOLERANCE):
        return OUTCOME_INCONSISTENT, f"order {state} reports cumulative_quantity {cumulative} above the requested {requested_quantity}"
    if state in _FILLED_STATES:
        if cumulative > 0 and average is not None and average > 0:
            return OUTCOME_FILLED, ""
        return OUTCOME_UNKNOWN, f"order filled without a positive cumulative_quantity and average_price ({cumulative}, {raw_average!r})"
    if state in _CANCELED_STATES:
        if cumulative == 0:
            return OUTCOME_NO_FILL, "order canceled with no fill"
        if average is not None and average > 0:
            return OUTCOME_PARTIALLY_FILLED, f"order canceled after a partial fill of {cumulative}"
        return OUTCOME_UNKNOWN, f"order canceled after a fill of {cumulative} with no usable average_price"
    if cumulative == 0:
        return OUTCOME_REJECTED, f"order {state} with no fill"
    return OUTCOME_UNKNOWN, f"order {state} after a fill of {cumulative}"


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

    def __init__(self, mode="paper", login_deadline=None):
        self._mode = mode
        self._logged_in = False
        self._login_deadline = login_deadline

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
        if self._login_deadline is None:
            with robinhood_library() as rh:
                rh.login(username, password, mfa_code=totp)
        else:
            remaining = self._login_deadline - time.monotonic()
            try:
                with _watchdog(min(_LOGIN_WATCHDOG_CAP_S, remaining), "Robinhood login"):
                    with robinhood_library() as rh:
                        rh.login(username, password, mfa_code=totp)
            except _BudgetExpired as e:
                raise RuntimeError(str(e)) from None
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


    def _logged_in_session(self, error_cls, not_logged_in="Robinhood adapter not logged in"):
        if not self._logged_in:
            raise error_cls(not_logged_in)
        with robinhood_library() as rh:
            logged_in = rh.helper.LOGGED_IN
            session = rh.helper.SESSION
            urls = rh.urls
        if logged_in is not True:
            raise error_cls("robin_stocks session not logged in")
        authorization = session.headers.get("Authorization")
        if not isinstance(authorization, str) or not authorization.strip():
            raise error_cls("robin_stocks session has no Authorization header")
        return session, urls

    def _order_request(self, session, method, url, deadline, json_body=None):
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise _BudgetExpired(f"{method} {url}: the time budget is exhausted")
        kwargs = {
            "timeout": (min(_ORDER_CONNECT_TIMEOUT_S, remaining), min(_ORDER_READ_TIMEOUT_S, remaining)),
            "allow_redirects": False,
        }
        if json_body is not None:
            kwargs["json"] = json_body
            kwargs["headers"] = {"Content-Type": "application/json"}
        return session.request(method, url, **kwargs)

    def _read_json_dict(self, session, url, deadline, what):
        import requests

        try:
            res = self._order_request(session, "GET", url, deadline)
        except requests.exceptions.RequestException as e:
            raise _OrderPrepError(f"{what}: request failed ({type(e).__name__})") from e
        if res.status_code != 200:
            raise _OrderPrepError(f"{what}: HTTP status {res.status_code}")
        try:
            body = res.json()
        except ValueError as e:
            raise _OrderPrepError(f"{what}: response is not valid JSON") from e
        if not isinstance(body, dict):
            raise _OrderPrepError(f"{what}: response is not a JSON object")
        return body

    def _read_pages(self, session, first_url, deadline, what):
        host = urlsplit(first_url).netloc
        requested = set()
        url = first_url
        page = 0
        while url:
            page += 1
            if page > _ORDER_PAGE_CAP:
                raise _OrderPrepError(f"{what}: more than {_ORDER_PAGE_CAP} pages")
            if url in requested:
                raise _OrderPrepError(f"{what}: page {page} repeats a next link")
            requested.add(url)
            body = self._read_json_dict(session, url, deadline, f"{what} page {page}")
            rows = body.get("results")
            if not isinstance(rows, list):
                raise _OrderPrepError(f"{what} page {page}: response has no results list")
            if "next" not in body:
                raise _OrderPrepError(f"{what} page {page}: response has no next key")
            next_url = body["next"]
            if next_url is None or next_url == "":
                next_url = None
            elif not isinstance(next_url, str):
                raise _OrderPrepError(f"{what} page {page}: next link is not a string")
            else:
                parts = urlsplit(next_url)
                if parts.scheme != "https" or parts.netloc != host:
                    raise _OrderPrepError(f"{what} page {page}: next link is not https on {host}")
            yield page, rows, next_url
            url = next_url

    def _currency_pair_id(self, session, urls, symbol, deadline):
        want = symbol.strip().upper()
        matches = []
        for _, rows, _ in self._read_pages(session, urls.crypto_currency_pairs_url(), deadline, "currency pairs"):
            for row in rows:
                if not isinstance(row, dict):
                    continue
                asset = row.get("asset_currency")
                code = asset.get("code") if isinstance(asset, dict) else None
                if isinstance(code, str) and code.strip().upper() == want:
                    matches.append(row)
        if len(matches) > 1:
            usd = []
            for row in matches:
                quote = row.get("quote_currency")
                code = quote.get("code") if isinstance(quote, dict) else None
                if isinstance(code, str) and code.strip().upper() == "USD":
                    usd.append(row)
            matches = usd
        if len(matches) != 1:
            raise _OrderPrepError(f"currency pairs: {len(matches)} tradable pairs match {want}, expected exactly one")
        pair_id = matches[0].get("id")
        if not isinstance(pair_id, str) or not pair_id.strip():
            raise _OrderPrepError(f"currency pairs: the {want} pair has no id")
        return pair_id.strip()

    def _crypto_account_id(self, session, urls, deadline, expected_account_id):
        body = self._read_json_dict(session, urls.crypto_account_url(), deadline, "crypto account")
        rows = body.get("results")
        if not isinstance(rows, list):
            raise _OrderPrepError("crypto account: response has no results list")
        ids = []
        for row in rows:
            if isinstance(row, dict) and isinstance(row.get("id"), str) and row["id"].strip():
                ids.append(row["id"].strip())
        if len(ids) != 1 or len(rows) != 1:
            raise _OrderPrepError(f"crypto account: {len(rows)} account rows, expected exactly one with an id")
        if expected_account_id and ids[0] != expected_account_id:
            raise _OrderPrepError("crypto account: the account id differs from the attempt's account")
        return ids[0]

    def _quote_price(self, session, urls, pair_id, side, deadline):
        body = self._read_json_dict(session, urls.crypto_quote_url(pair_id), deadline, "crypto quote")
        key = "ask_price" if side == "buy" else "bid_price"
        price = _parse_decimal(body.get(key))
        if price is None or price <= 0:
            raise _OrderPrepError(f"crypto quote: {key} {body.get(key)!r} is not a finite positive number")
        return price

    def _poll_order(self, session, urls, order_id, ref_id, side, deadline):
        import requests

        try:
            res = self._order_request(session, "GET", urls.crypto_orders_url(order_id), deadline)
        except requests.exceptions.RequestException as e:
            return None, f"order read failed ({type(e).__name__})"
        if res.status_code != 200:
            return None, f"order read returned HTTP status {res.status_code}"
        try:
            body = res.json()
        except ValueError:
            return None, "order read returned invalid JSON"
        if not isinstance(body, dict):
            return None, "order read did not return a JSON object"
        if body.get("id") != order_id:
            raise _OrderEvidenceError(f"order read returned id {body.get('id')!r}, expected {order_id!r}")
        if "ref_id" in body and body.get("ref_id") != ref_id:
            raise _OrderEvidenceError(f"order read returned ref_id {body.get('ref_id')!r}, expected {ref_id!r}")
        if "side" in body and _norm_state(body.get("side")) != side:
            raise _OrderEvidenceError(f"order read returned side {body.get('side')!r}, expected {side!r}")
        return body, ""

    def _follow_order(self, session, urls, order_id, ref_id, side, deadline, requested_quantity, result, cancel_open=True, first=None):
        last = first
        last_error = ""
        if first is not None:
            outcome, detail = _evaluate_terminal(first, requested_quantity)
            if outcome is not None:
                return self._finish(result, outcome, detail, first)
        now = time.monotonic()
        reserve = min(_CANCEL_RESERVE_S, max(deadline - now, 0.0) * _CANCEL_RESERVE_SHARE) if cancel_open else _FINAL_MARGIN_S
        poll_deadline = deadline - reserve
        interval = _POLL_INTERVAL_START_S
        while time.monotonic() < poll_deadline:
            body, error = self._poll_order(session, urls, order_id, ref_id, side, poll_deadline)
            if body is None:
                last_error = error
            else:
                last, last_error = body, ""
                result["order"] = _order_evidence(body)
                outcome, detail = _evaluate_terminal(body, requested_quantity)
                if outcome is not None:
                    return self._finish(result, outcome, detail, body)
            if time.monotonic() + interval >= poll_deadline:
                break
            time.sleep(interval)
            interval = min(interval * _POLL_INTERVAL_GROWTH, _POLL_INTERVAL_MAX_S)
        if not cancel_open:
            if last is None or last_error:
                return self._finish(result, OUTCOME_UNKNOWN, f"order {order_id} state is unreadable: {last_error}", last)
            return self._finish(result, OUTCOME_OPEN, f"order {order_id} is still {_norm_state(last.get('state')) or 'in an unknown state'}", last)
        result["cancel"]["requested"] = True
        try:
            res = self._order_request(session, "POST", urls.crypto_cancel_url(order_id), deadline - _FINAL_MARGIN_S)
            result["cancel"]["acknowledged"] = 200 <= res.status_code < 300
            if not result["cancel"]["acknowledged"]:
                result["cancel"]["detail"] = f"HTTP status {res.status_code}"
        except Exception as e:
            result["cancel"]["detail"] = f"cancel request failed ({type(e).__name__})"
        final_deadline = deadline - _FINAL_MARGIN_S
        interval = _POLL_INTERVAL_START_S
        while time.monotonic() < final_deadline:
            body, error = self._poll_order(session, urls, order_id, ref_id, side, final_deadline)
            if body is None:
                last_error = error
            else:
                last, last_error = body, ""
                result["order"] = _order_evidence(body)
                outcome, detail = _evaluate_terminal(body, requested_quantity)
                if outcome is not None:
                    return self._finish(result, outcome, detail, body)
            if time.monotonic() + interval >= final_deadline:
                break
            time.sleep(interval)
            interval = min(interval * _POLL_INTERVAL_GROWTH, _POLL_INTERVAL_MAX_S)
        if last is None or last_error:
            return self._finish(result, OUTCOME_UNKNOWN, f"order {order_id} state is unreadable after the cancel request: {last_error}", last)
        return self._finish(result, OUTCOME_OPEN, f"order {order_id} is still {_norm_state(last.get('state')) or 'in an unknown state'} after the cancel request; cancel completion is unconfirmed", last)

    def _finish(self, result, outcome, detail, order):
        result["outcome"] = outcome
        result["detail"] = detail
        if isinstance(order, dict):
            result["order"] = _order_evidence(order)
        if outcome in CONFIRMED_FILL_OUTCOMES and isinstance(order, dict):
            fill = {
                "avg_px": _parse_decimal(order.get("average_price")),
                "quantity": _parse_decimal(order.get("cumulative_quantity")),
                "oid": str(order.get("id")),
            }
            fee = extract_order_fee(order)
            if fee is not None:
                fill["fee"] = fee
            result["fill"] = fill
        return result

    def _new_result(self, ref_id):
        return {
            "outcome": OUTCOME_UNKNOWN,
            "detail": "",
            "order": {},
            "ref_id": ref_id,
            "requested_quantity": None,
            "account_id": None,
            "currency_pair_id": None,
            "cancel": {"requested": False, "acknowledged": False},
            "post_attempts": 0,
        }

    def place_market_order(self, symbol, side, *, ref_id, deadline, quantity=None, amount_usd=None, expected_account_id=None):
        side = (side or "").strip().lower()
        if side not in ("buy", "sell"):
            raise ValueError(f"side must be buy or sell, got {side!r}")
        try:
            ref_id = str(uuid.UUID(str(ref_id)))
        except ValueError:
            raise ValueError(f"ref_id {ref_id!r} is not a UUID") from None
        size = quantity if side == "sell" else amount_usd
        if isinstance(size, bool) or not isinstance(size, (int, float)) or not math.isfinite(size) or size <= 0:
            raise ValueError(f"order size {size!r} is not a finite positive number")
        if not self.is_live:
            raise RuntimeError("place_market_order requires live mode")
        result = self._new_result(ref_id)
        result["_send_started"] = False
        try:
            with _watchdog(deadline - time.monotonic() - 0.25, "Robinhood order"):
                self._place(symbol, side, ref_id, deadline, size, expected_account_id, result)
        except _BudgetExpired as e:
            if result["_send_started"]:
                self._finish_clean(result, OUTCOME_UNKNOWN, f"{e}; the order was sent and its outcome is unknown")
            else:
                self._finish_clean(result, OUTCOME_NOT_SENT, f"{e}; no order was sent")
        except _OrderEvidenceError as e:
            self._finish_clean(result, OUTCOME_UNKNOWN, str(e))
        except Exception as e:
            if result["_send_started"]:
                self._finish_clean(result, OUTCOME_UNKNOWN, f"unexpected {type(e).__name__} after the order was sent: {e}")
            else:
                self._finish_clean(result, OUTCOME_NOT_SENT, str(e))
        result.pop("_send_started", None)
        return result

    def _finish_clean(self, result, outcome, detail):
        result["outcome"] = outcome
        result["detail"] = detail
        result.pop("fill", None)
        return result

    def _place(self, symbol, side, ref_id, deadline, size, expected_account_id, result):
        import requests

        session, urls = self._logged_in_session(_OrderPrepError)
        pair_id = self._currency_pair_id(session, urls, symbol, deadline)
        result["currency_pair_id"] = pair_id
        account_id = self._crypto_account_id(session, urls, deadline, expected_account_id)
        result["account_id"] = account_id
        with robinhood_library() as rh:
            round_price = rh.helper.round_price
        price = round_price(self._quote_price(session, urls, pair_id, side, deadline))
        if side == "sell":
            order_quantity = size
        else:
            order_quantity = round_price(size / price)
        if not math.isfinite(order_quantity) or order_quantity <= 0:
            raise _OrderPrepError(f"order quantity {order_quantity!r} is not a finite positive number")
        result["requested_quantity"] = order_quantity
        payload = {
            "account_id": account_id,
            "currency_pair_id": pair_id,
            "price": price,
            "quantity": order_quantity,
            "ref_id": ref_id,
            "side": side,
            "time_in_force": "gtc",
            "type": "market",
        }
        response = None
        last_pre_send = None
        while result["post_attempts"] < _SEND_ATTEMPTS:
            if deadline - time.monotonic() <= 0:
                break
            result["post_attempts"] += 1
            result["_send_started"] = True
            try:
                response = self._order_request(session, "POST", urls.order_crypto_url(), deadline, json_body=payload)
                break
            except requests.exceptions.RequestException as e:
                if _is_pre_send_failure(e):
                    last_pre_send = e
                    result["_send_started"] = False
                    continue
                return self._finish_clean(result, OUTCOME_UNKNOWN, f"order submit failed after the request was sent ({type(e).__name__}); the outcome is unknown")
        if response is None:
            detail = f"every order submit failed before the request was sent ({type(last_pre_send).__name__ if last_pre_send else 'no budget'})"
            return self._finish_clean(result, OUTCOME_NOT_SENT, detail)
        try:
            body = response.json()
        except ValueError:
            body = None
        status = response.status_code
        if 200 <= status < 300:
            if not isinstance(body, dict):
                return self._finish_clean(result, OUTCOME_UNKNOWN, f"order submit returned HTTP {status} without a JSON object; the outcome is unknown")
            order_id = body.get("id")
            if not isinstance(order_id, str) or not order_id.strip():
                result["order"] = _order_evidence(body)
                return self._finish_clean(result, OUTCOME_UNKNOWN, f"order submit returned HTTP {status} with no order id; the outcome is unknown")
            for key, want in (("ref_id", ref_id), ("currency_pair_id", pair_id)):
                if key in body and body.get(key) != want:
                    result["order"] = _order_evidence(body)
                    return self._finish_clean(result, OUTCOME_UNKNOWN, f"order submit returned {key} {body.get(key)!r}, expected {want!r}")
            if "side" in body and _norm_state(body.get("side")) != side:
                result["order"] = _order_evidence(body)
                return self._finish_clean(result, OUTCOME_UNKNOWN, f"order submit returned side {body.get('side')!r}, expected {side!r}")
            result["order"] = _order_evidence(body)
            return self._follow_order(session, urls, order_id, ref_id, side, deadline, order_quantity, result, cancel_open=True, first=body)
        if status in _REFUSAL_STATUSES and isinstance(body, dict) and not body.get("id") and any(body.get(k) for k in _REFUSAL_KEYS):
            reason = "; ".join(f"{k}={body[k]!r}" for k in _REFUSAL_KEYS if body.get(k))
            result["order"] = _order_evidence(body)
            return self._finish_clean(result, OUTCOME_REJECTED, f"Robinhood refused the order (HTTP {status}): {reason}")
        return self._finish_clean(result, OUTCOME_UNKNOWN, f"order submit returned HTTP {status}; the outcome is unknown")

    def resolve_order(self, symbol, side, *, ref_id, deadline, submitted_after_ms, submitted_before_ms,
                      order_id=None, requested_quantity=None, expected_account_id=None, cancel_open=False, now_ms=None,
                      verify_fields=False):
        side = (side or "").strip().lower()
        if side not in ("buy", "sell"):
            raise ValueError(f"side must be buy or sell, got {side!r}")
        try:
            ref_id = str(uuid.UUID(str(ref_id)))
        except ValueError:
            raise ValueError(f"ref_id {ref_id!r} is not a UUID") from None
        if requested_quantity is not None and (not math.isfinite(requested_quantity) or requested_quantity <= 0):
            raise ValueError(f"requested quantity {requested_quantity!r} is not a finite positive number")
        if not self.is_live:
            raise RuntimeError("resolve_order requires live mode")
        result = self._new_result(ref_id)
        result["requested_quantity"] = requested_quantity
        try:
            with _watchdog(deadline - time.monotonic() - 0.25, "Robinhood order resolution"):
                self._resolve(symbol, side, ref_id, deadline, submitted_after_ms, submitted_before_ms,
                              order_id, requested_quantity, expected_account_id, cancel_open, now_ms, result,
                              verify_fields)
        except _BudgetExpired as e:
            self._finish_clean(result, OUTCOME_UNKNOWN, f"{e}; the order outcome is still unknown")
        except (_OrderPrepError, _OrderEvidenceError) as e:
            self._finish_clean(result, OUTCOME_UNKNOWN, str(e))
        except Exception as e:
            self._finish_clean(result, OUTCOME_UNKNOWN, f"unexpected {type(e).__name__}: {e}")
        return result

    def _resolve(self, symbol, side, ref_id, deadline, submitted_after_ms, submitted_before_ms,
                 order_id, requested_quantity, expected_account_id, cancel_open, now_ms, result, verify_fields=False):
        session, urls = self._logged_in_session(_OrderPrepError)
        account_id = self._crypto_account_id(session, urls, deadline, expected_account_id)
        result["account_id"] = account_id
        if order_id:
            body, error = self._poll_order(session, urls, order_id, ref_id, side, deadline)
            if body is None:
                return self._finish_clean(result, OUTCOME_UNKNOWN, f"order {order_id}: {error}")
            if verify_fields and body.get("ref_id") != ref_id:
                pair_id = self._currency_pair_id(session, urls, symbol, deadline)
                result["currency_pair_id"] = pair_id
                self._verify_order_fields(body, pair_id, side, requested_quantity, submitted_after_ms, submitted_before_ms)
            return self._resolve_found(session, urls, order_id, ref_id, side, deadline, requested_quantity, account_id, cancel_open, body, result)
        pair_id = self._currency_pair_id(session, urls, symbol, deadline)
        result["currency_pair_id"] = pair_id
        window_start = submitted_after_ms - _SETTLE_WINDOW_MS
        window_end = submitted_before_ms + _SETTLE_WINDOW_MS
        by_ref = []
        by_fields = []
        complete = False
        previous_created = None
        ordered = True
        window_rows_complete = True
        saw_older = False
        try:
            for _, rows, next_url in self._read_pages(session, urls.crypto_orders_url(), deadline, "crypto orders"):
                for row in rows:
                    if not isinstance(row, dict):
                        window_rows_complete = False
                        continue
                    created = _parse_created_at_ms(row.get("created_at"))
                    if created is None:
                        ordered = False
                        window_rows_complete = False
                    else:
                        if previous_created is not None and created > previous_created:
                            ordered = False
                        previous_created = created
                        if created < window_start:
                            saw_older = True
                    if row.get("ref_id") == ref_id:
                        by_ref.append(row)
                        continue
                    if "ref_id" in row and row.get("ref_id"):
                        continue
                    if created is None or created < window_start or created > window_end:
                        continue
                    row_account = row.get("account_id")
                    if row_account is not None and row_account != account_id:
                        continue
                    if row.get("currency_pair_id") is None or "side" not in row or row.get("quantity") is None:
                        window_rows_complete = False
                        continue
                    if row.get("currency_pair_id") != pair_id or _norm_state(row.get("side")) != side:
                        continue
                    qty = _parse_decimal(row.get("quantity"))
                    if qty is None or requested_quantity is None:
                        window_rows_complete = False
                        continue
                    if _relative_close(qty, requested_quantity):
                        by_fields.append(row)
                if next_url is None:
                    complete = True
                elif ordered and saw_older:
                    complete = True
                if complete:
                    break
        except _OrderPrepError as e:
            return self._finish_clean(result, OUTCOME_UNKNOWN, f"order list read failed: {e}")
        matches = by_ref if by_ref else by_fields
        if len(matches) > 1:
            return self._finish_clean(result, OUTCOME_UNKNOWN, f"{len(matches)} orders match the attempt; the order is ambiguous")
        if len(matches) == 1:
            match = matches[0]
            found_id = match.get("id")
            if not isinstance(found_id, str) or not found_id.strip():
                return self._finish_clean(result, OUTCOME_UNKNOWN, "the matching order has no id")
            body, error = self._poll_order(session, urls, found_id, ref_id, side, deadline)
            if body is None:
                return self._finish_clean(result, OUTCOME_UNKNOWN, f"order {found_id}: {error}")
            return self._resolve_found(session, urls, found_id, ref_id, side, deadline, requested_quantity, account_id, cancel_open, body, result)
        current_ms = now_ms if now_ms is not None else int(time.time() * 1000)
        if not complete:
            return self._finish_clean(result, OUTCOME_UNKNOWN, "no matching order on the pages read, and the order list was not read to the settle window")
        if not window_rows_complete:
            return self._finish_clean(result, OUTCOME_UNKNOWN, "no matching order, but some order rows in the settle window are unreadable")
        if current_ms < submitted_before_ms + _SETTLE_WINDOW_MS:
            return self._finish_clean(result, OUTCOME_UNKNOWN, "no matching order yet; the settle window has not passed")
        return self._finish_clean(result, OUTCOME_NOT_FOUND, "no order with this ref_id or matching fields exists after the settle window")

    def _resolve_found(self, session, urls, order_id, ref_id, side, deadline, requested_quantity, account_id, cancel_open, body, result):
        self._check_account(body, account_id)
        result["order"] = _order_evidence(body)
        outcome, detail = _evaluate_terminal(body, requested_quantity)
        if outcome is not None:
            return self._finish(result, outcome, detail, body)
        if not cancel_open:
            return self._finish(result, OUTCOME_OPEN, f"order {order_id} is still {_norm_state(body.get('state')) or 'in an unknown state'}", body)
        return self._follow_order(session, urls, order_id, ref_id, side, deadline, requested_quantity, result,
                                  cancel_open=True, first=body)

    def _verify_order_fields(self, body, pair_id, side, requested_quantity, submitted_after_ms, submitted_before_ms):
        if body.get("currency_pair_id") != pair_id:
            raise _OrderEvidenceError(f"order pair {body.get('currency_pair_id')!r} is not {pair_id!r}")
        if _norm_state(body.get("side")) != side:
            raise _OrderEvidenceError(f"order side {body.get('side')!r} is not {side!r}")
        qty = _parse_decimal(body.get("quantity"))
        if requested_quantity is None or qty is None or not _relative_close(qty, requested_quantity):
            raise _OrderEvidenceError(f"order quantity {body.get('quantity')!r} does not match the attempt's {requested_quantity!r}")
        created = _parse_created_at_ms(body.get("created_at"))
        if created is None or created < submitted_after_ms - _SETTLE_WINDOW_MS or created > submitted_before_ms + _SETTLE_WINDOW_MS:
            raise _OrderEvidenceError(f"order created_at {body.get('created_at')!r} is outside the attempt's submit window")

    def _check_account(self, body, account_id):
        row_account = body.get("account_id")
        if row_account is not None and row_account != account_id:
            raise _OrderEvidenceError(f"order belongs to account {row_account!r}, not the logged-in crypto account")

    def get_crypto_positions_strict(self, deadline=None) -> list:
        try:
            return self._read_crypto_holdings(deadline)
        except _HoldingsReadError as e:
            raise RuntimeError(f"{_HOLDINGS_ERROR_PREFIX} {e}") from e
        except Exception as e:
            raise RuntimeError(f"{_HOLDINGS_ERROR_PREFIX} unexpected {type(e).__name__}") from e

    def _read_crypto_holdings(self, deadline=None) -> list:
        import requests

        session, urls = self._logged_in_session(
            _HoldingsReadError, "Robinhood adapter not logged in — cannot fetch crypto positions")
        first_url = urls.crypto_holdings_url()

        holdings_host = urlsplit(first_url).netloc
        own_deadline = time.monotonic() + _HOLDINGS_TOTAL_DEADLINE_S
        deadline = own_deadline if deadline is None else min(own_deadline, deadline)
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
