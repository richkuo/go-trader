import json
import math
import os
import socket
import sys
import time

_ACTIVE = os.environ.get("QP_STUB") == "1"


def _float_env(name):
    raw = os.environ.get(name, "")
    if raw == "":
        return None
    return float(raw)


def _quote_mode():
    raw = os.environ.get("QP_QUOTE", "")
    if raw == "":
        return "absent", None
    if raw == "error":
        return "error", None
    return "value", float(raw)


def _closes(close, n):
    shape = os.environ.get("QP_SHAPE", "wave")
    if shape == "cross_up":
        return [close * (0.8 - 0.0001 * i) for i in range(n - 1)] + [close]
    if shape == "cross_down":
        return [close * (1.25 + 0.0001 * i) for i in range(n - 1)] + [close]
    return [close * (1.0 + 0.04 * math.sin(i / 4.0) + 0.0005 * (i - n)) for i in range(n - 1)] + [close]


def _candles(close, n, step_ms):
    end_ms = (int(time.time() * 1000) // step_ms) * step_ms
    if os.environ.get("QP_ALL_CLOSED") == "1":
        end_ms -= step_ms
    start_ms = end_ms - (n - 1) * step_ms
    closes = _closes(close, n)
    out = []
    for i, c in enumerate(closes):
        o = closes[i - 1] if i > 0 else c
        hi = max(o, c) * 1.01
        lo = min(o, c) * 0.99
        out.append([start_ms + i * step_ms, o, hi, lo, c, 1000.0 + (i % 17) * 25.0])
    return out


def _step_ms(timeframe):
    units = {"m": 60_000, "h": 3_600_000, "d": 86_400_000}
    tf = str(timeframe or "1h")
    try:
        return int(tf[:-1]) * units[tf[-1]]
    except (KeyError, ValueError):
        return 3_600_000


def _log(record):
    path = os.environ.get("QP_LOG", "")
    if not path:
        return
    with open(path, "a") as f:
        f.write(json.dumps(record) + "\n")


def _daily_mode():
    raw = os.environ.get("QP_DAILY", "")
    if raw == "":
        return "default", None
    if raw in ("error", "429", "503", "timeout"):
        return raw, None
    return "closes", json.loads(raw)


def _daily_rows(closes, step_ms):
    end_ms = (int(time.time() * 1000) // step_ms) * step_ms
    start_ms = end_ms - (len(closes) - 1) * step_ms
    return [[start_ms + i * step_ms, c, c, c, c, 1000.0] for i, c in enumerate(closes)]


def _raise_daily(ccxt, exchange_id, mode):
    url = "https://stub.invalid/" + exchange_id + "/klines?interval=1d"
    if mode == "429":
        raise ccxt.RateLimitExceeded(exchange_id + " GET " + url + " 429 Too Many Requests")
    if mode == "503":
        raise ccxt.ExchangeNotAvailable(exchange_id + " GET " + url + " 503 Service Unavailable")
    if mode == "timeout":
        raise ccxt.RequestTimeout(exchange_id + " GET " + url + " request timed out (10000 ms)")
    raise ccxt.NetworkError("quote precision stub: daily history failure for " + exchange_id)


_OPTION_QUOTES_SERVED = [0]


def _noise():
    if os.environ.get("QP_STDOUT_NOISE") == "1":
        sys.stdout.write("quote precision stub stdout noise\n")
        sys.stdout.flush()


def _option_quote_mode():
    fail_after = os.environ.get("QP_OPTION_QUOTE_FAIL_AFTER", "")
    _OPTION_QUOTES_SERVED[0] += 1
    if fail_after != "" and _OPTION_QUOTES_SERVED[0] > int(fail_after):
        return "error", None
    raw = os.environ.get("QP_OPTION_QUOTE", "")
    if raw == "":
        return "value", 0.05
    if raw in ("error", "429", "503", "missing"):
        return raw, None
    return "value", float(raw)


def _okx_option_markets(spot):
    from datetime import datetime, timedelta, timezone
    now = datetime.now(timezone.utc)
    markets = {}
    for days in (21, 30, 37, 45):
        exp = (now + timedelta(days=days)).replace(hour=8, minute=0, second=0, microsecond=0)
        exp_ms = int(exp.timestamp() * 1000)
        tag = exp.strftime("%y%m%d")
        for i in range(41):
            strike = round(spot * (0.80 + 0.01 * i), 8)
            for option_type, char in (("call", "C"), ("put", "P")):
                sym = "BTC/USD:BTC-%s-%s-%s" % (tag, repr(strike), char)
                markets[sym] = {"symbol": sym, "type": "option", "base": "BTC", "optionType": option_type,
                                "strike": strike, "expiry": exp_ms, "active": True}
    return markets


def _option_greeks():
    raw = os.environ.get("QP_OPTION_GREEKS", "")
    if raw:
        return json.loads(raw)
    return {"delta": 0.25, "gamma": 0.0001, "theta": -1.5, "vega": 2.5}


def _option_ticker(ccxt, exchange_id, symbol):
    mode, value = _option_quote_mode()
    _log({"call": "option_ticker", "exchange": exchange_id, "symbol": symbol, "mode": mode})
    if mode == "429":
        raise ccxt.RateLimitExceeded(exchange_id + " GET https://stub.invalid/ticker 429 Too Many Requests")
    if mode == "503":
        raise ccxt.ExchangeNotAvailable(exchange_id + " GET https://stub.invalid/ticker 503 Service Unavailable")
    if mode == "error":
        raise ccxt.NetworkError("quote precision stub: option ticker failure for " + symbol)
    if mode == "missing":
        return {"symbol": symbol, "last": None, "close": None, "info": {}}
    return {"symbol": symbol, "last": value, "info": _option_greeks()}


def _install_deribit_ticker(requests):
    import requests.adapters
    import requests.models

    real_send = requests.adapters.HTTPAdapter.send

    def send(self, request, *args, **kwargs):
        if "deribit.com/api/v2/public/ticker" not in request.url:
            return real_send(self, request, *args, **kwargs)
        mode, value = _option_quote_mode()
        _log({"call": "deribit_ticker", "url": request.url, "mode": mode})
        if mode == "error":
            raise requests.exceptions.ConnectionError("quote precision stub: deribit ticker failure")
        resp = requests.models.Response()
        resp.status_code = 200
        result = {"underlying_price": 100.0, "greeks": _option_greeks()}
        if mode in ("429", "503"):
            resp.status_code = int(mode)
            body = {"error": {"message": "quote precision stub status " + mode}}
        elif mode == "missing":
            body = {"result": result}
        else:
            result["mark_price"] = value
            body = {"result": result}
        resp._content = json.dumps(body).encode()
        resp.headers["Content-Type"] = "application/json"
        resp.url = request.url
        resp.request = request
        resp.reason = "quote precision stub"
        resp.encoding = "utf-8"
        return resp

    requests.adapters.HTTPAdapter.send = send


def _install_frozen_topstep_clock(iso):
    import datetime as _dt
    import importlib.abc
    import importlib.machinery

    fixed = _dt.datetime.fromisoformat(iso)

    class FrozenDatetime(_dt.datetime):
        @classmethod
        def now(cls, tz=None):
            return fixed.astimezone(tz) if tz is not None else fixed.replace(tzinfo=None)

    suffix = os.path.join("platforms", "topstep", "adapter.py")

    class TopStepClockFinder(importlib.abc.MetaPathFinder):
        def find_spec(self, name, path, target=None):
            if name != "adapter":
                return None
            spec = importlib.machinery.PathFinder.find_spec(name, path)
            if spec is None or not str(spec.origin or "").endswith(suffix):
                return spec
            loader = spec.loader
            original = loader.exec_module

            def exec_module(module):
                original(module)
                module.datetime = FrozenDatetime

            loader.exec_module = exec_module
            return spec

    sys.meta_path.insert(0, TopStepClockFinder())


def _install_okx_venue(ccxt, fill):
    orders = {}

    def order_body(symbol, side, amount, params):
        filled = fill.get("filled")
        if filled is None:
            filled = amount
        return {
            "id": "qp-okx-1",
            "clientOrderId": (params or {}).get("clOrdId", ""),
            "symbol": symbol,
            "side": side,
            "amount": amount,
            "status": fill.get("status", "closed"),
            "filled": filled,
            "average": fill.get("average"),
            "fee": {"cost": fill.get("fee", 0.0), "currency": "USDT"},
        }

    def create_market_order(self, symbol, side, amount, price=None, params=None):
        _log({"call": "create_market_order", "symbol": symbol, "side": side, "amount": amount,
              "params": params or {}, "exp_time": (self.headers or {}).get("expTime", "")})
        if fill.get("create") == "network_error":
            raise ccxt.NetworkError("quote precision stub: order send timed out")
        order = order_body(symbol, side, amount, params)
        orders[order["clientOrderId"]] = order
        return order

    def fetch_order(self, id, symbol=None, params=None):
        cid = (params or {}).get("clOrdId", "")
        _log({"call": "fetch_order", "symbol": symbol, "client_order_id": cid})
        if cid not in orders:
            raise ccxt.OrderNotFound("quote precision stub has no order " + cid)
        return orders[cid]

    ccxt.okx.create_market_order = create_market_order
    ccxt.okx.fetch_order = fetch_order


def _install():
    real_connect = socket.socket.connect

    def blocked_connect(self, address, *args, **kwargs):
        if self.family in (socket.AF_INET, socket.AF_INET6):
            raise OSError("network blocked by the quote precision stub: %r" % (address,))
        return real_connect(self, address, *args, **kwargs)

    socket.socket.connect = blocked_connect

    close = _float_env("QP_CLOSE")
    if close is None:
        close = 100.0
    n_candles = int(os.environ.get("QP_CANDLES") or "300")
    tickers_raw = os.environ.get("QP_TICKERS", "")
    tickers = json.loads(tickers_raw) if tickers_raw else None
    closes_raw = os.environ.get("QP_CLOSES", "")
    closes = json.loads(closes_raw) if closes_raw else {}

    import ccxt
    import pandas as pd
    import requests
    import yfinance

    okx_options = os.environ.get("QP_OKX_OPTIONS", "") in ("1", "empty")

    def fetch_ticker(self, symbol, params=None):
        _log({"call": "fetch_ticker", "exchange": self.id, "symbol": symbol})
        _noise()
        if okx_options and symbol in (getattr(self, "markets", None) or {}) and self.markets[symbol].get("type") == "option":
            return _option_ticker(ccxt, self.id, symbol)
        if tickers is not None:
            if symbol not in tickers:
                raise ccxt.BadSymbol("quote precision stub has no ticker for " + symbol)
            entry = tickers[symbol]
            if isinstance(entry, dict):
                return entry
            return {"symbol": symbol, "last": entry}
        mode, value = _quote_mode()
        if mode == "value":
            return {"symbol": symbol, "last": value}
        raise ccxt.NetworkError("quote precision stub: no quote for " + symbol)

    def fetch_ohlcv(self, symbol, timeframe="1m", since=None, limit=None, params=None):
        _log({"call": "fetch_ohlcv", "exchange": self.id, "symbol": symbol, "timeframe": timeframe})
        _noise()
        if timeframe == "1d":
            mode, daily = _daily_mode()
            if mode == "closes":
                rows = _daily_rows(daily, _step_ms("1d"))
                if limit:
                    rows = rows[-int(limit):]
                return rows
            if mode != "default":
                _raise_daily(ccxt, self.id, mode)
        rows = _candles(float(closes.get(symbol, close)), n_candles, _step_ms(timeframe))
        if limit:
            rows = rows[-int(limit):]
        return rows

    def load_markets(self, reload=False, params=None):
        if okx_options and self.id == "okx":
            spot = _quote_mode()[1] or close
            self.markets = _okx_option_markets(spot) if os.environ.get("QP_OKX_OPTIONS") == "1" else {}
            return self.markets
        return {}

    for cls in (ccxt.Exchange, ccxt.binance, ccxt.binanceus, ccxt.okx, ccxt.luno):
        cls.fetch_ticker = fetch_ticker
        cls.fetch_ohlcv = fetch_ohlcv
        cls.load_markets = load_markets

    okx_fill_raw = os.environ.get("QP_OKX_FILL", "")
    if okx_fill_raw:
        _install_okx_venue(ccxt, json.loads(okx_fill_raw))

    _install_deribit_ticker(requests)

    frozen_now = os.environ.get("QP_NOW", "")
    if frozen_now:
        _install_frozen_topstep_clock(frozen_now)

    def history(self, period=None, interval=None, *args, **kwargs):
        _log({"call": "yahoo_history", "symbol": getattr(self, "ticker", ""), "period": period, "interval": interval})
        if period == "1d" and interval is None:
            mode, value = _quote_mode()
            if mode == "error":
                raise RuntimeError("quote precision stub: yahoo quote failure")
            if mode == "absent":
                return pd.DataFrame(columns=["Open", "High", "Low", "Close", "Volume"])
            index = pd.DatetimeIndex([pd.Timestamp.now(tz="UTC").floor("min")])
            return pd.DataFrame({"Open": [value], "High": [value], "Low": [value], "Close": [value], "Volume": [1.0]}, index=index)
        if interval == "1d":
            mode, daily = _daily_mode()
            if mode == "closes":
                rows = _daily_rows(daily, _step_ms("1d"))
                index = pd.to_datetime([r[0] for r in rows], unit="ms", utc=True)
                return pd.DataFrame({"Open": daily, "High": daily, "Low": daily, "Close": daily, "Volume": [1.0] * len(daily)}, index=index)
            if mode == "429":
                from yfinance.exceptions import YFRateLimitError
                raise YFRateLimitError()
            if mode == "503":
                raise RuntimeError("503 Server Error: Service Unavailable for url: https://stub.invalid/yahoo/chart")
            if mode != "default":
                raise RuntimeError("quote precision stub: yahoo daily history failure")
        step = _step_ms(interval or "1h")
        rows = _candles(float(closes.get(getattr(self, "ticker", ""), close)), n_candles, step)
        index = pd.to_datetime([r[0] for r in rows], unit="ms", utc=True)
        return pd.DataFrame(
            {
                "Open": [r[1] for r in rows],
                "High": [r[2] for r in rows],
                "Low": [r[3] for r in rows],
                "Close": [r[4] for r in rows],
                "Volume": [r[5] for r in rows],
            },
            index=index,
        )

    yfinance.Ticker.history = history
    sys.stderr.write("quote precision stub active\n")
    sys.stderr.flush()


if _ACTIVE:
    _install()
