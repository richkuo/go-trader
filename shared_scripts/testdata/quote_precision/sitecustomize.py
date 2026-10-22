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
    import yfinance

    def fetch_ticker(self, symbol, params=None):
        _log({"call": "fetch_ticker", "exchange": self.id, "symbol": symbol})
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
        rows = _candles(float(closes.get(symbol, close)), n_candles, _step_ms(timeframe))
        if limit:
            rows = rows[-int(limit):]
        return rows

    def load_markets(self, reload=False, params=None):
        return {}

    for cls in (ccxt.Exchange, ccxt.binance, ccxt.binanceus, ccxt.okx):
        cls.fetch_ticker = fetch_ticker
        cls.fetch_ohlcv = fetch_ohlcv
        cls.load_markets = load_markets

    okx_fill_raw = os.environ.get("QP_OKX_FILL", "")
    if okx_fill_raw:
        _install_okx_venue(ccxt, json.loads(okx_fill_raw))

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
