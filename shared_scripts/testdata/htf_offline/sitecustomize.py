import importlib.abc
import importlib.machinery
import json
import os
import socket
import sys
import time

_FIXTURE_PATH = os.environ.get("HTF_OFFLINE_FIXTURE", "")


def _load():
    with open(_FIXTURE_PATH) as fh:
        return json.load(fh)


def _rows(raw):
    out = []
    for row in raw:
        out.append([int(row[0])] + [float(v) for v in row[1:6]])
    return out


def _block_network():
    real_connect = socket.socket.connect

    def blocked_connect(self, address, *args, **kwargs):
        if self.family in (socket.AF_INET, socket.AF_INET6):
            raise OSError("network blocked by the htf offline fixture: %r" % (address,))
        return real_connect(self, address, *args, **kwargs)

    socket.socket.connect = blocked_connect


def _patch_ccxt(module):
    def fetch_ohlcv(self, symbol, timeframe="1m", since=None, limit=None, params=None):
        data = _load()
        if data.get("ccxt_log"):
            with open(data["ccxt_log"], "a") as fh:
                fh.write(json.dumps([symbol, timeframe, since, limit]) + "\n")
        if data.get("ccxt_delay_s"):
            time.sleep(float(data["ccxt_delay_s"]))
        value = data.get("ccxt", {}).get(symbol + "|" + timeframe, [])
        if value == "network_error":
            raise module.NetworkError("htf offline fixture: %s %s unreachable" % (symbol, timeframe))
        if isinstance(value, dict):
            raise module.NetworkError(str(value.get("error", "htf offline fixture error")))
        rows = _rows(value)
        if since is not None:
            rows = [r for r in rows if r[0] >= since]
            return rows[:limit] if limit else rows
        return rows[-limit:] if limit else rows

    def fetch_ticker(self, symbol, params=None):
        px = _load().get("tickers", {}).get(symbol)
        if px is None:
            raise module.BadSymbol("htf offline fixture has no ticker for %s" % symbol)
        return {"symbol": symbol, "last": float(px), "close": float(px)}

    def load_markets(self, reload=False, params=None):
        return {}

    for name in ("binanceus", "okx"):
        cls = getattr(module, name)
        cls.fetch_ohlcv = fetch_ohlcv
        cls.fetch_ticker = fetch_ticker
        cls.load_markets = load_markets


def _patch_yfinance(module):
    import pandas as pd

    def history(self, period="1mo", interval="1d", *args, **kwargs):
        data = _load()
        sym = getattr(self, "ticker", None) or getattr(self, "symbol", "")
        if period == "1d" and interval == "1d" and sym in data.get("yahoo_price", {}):
            px = float(data["yahoo_price"][sym])
            idx = pd.DatetimeIndex([pd.Timestamp("2026-10-01", tz="UTC")])
            return pd.DataFrame({"Open": [px], "High": [px], "Low": [px], "Close": [px], "Volume": [0.0]}, index=idx)
        value = data.get("yahoo", {}).get(sym + "|" + interval, [])
        if value == "network_error":
            raise ConnectionError("htf offline fixture: %s %s unreachable" % (sym, interval))
        if isinstance(value, dict):
            raise ConnectionError(str(value.get("error", "htf offline fixture error")))
        rows = _rows(value)
        idx = pd.DatetimeIndex([pd.Timestamp(r[0], unit="ms", tz="UTC") for r in rows])
        return pd.DataFrame({
            "Open": [r[1] for r in rows],
            "High": [r[2] for r in rows],
            "Low": [r[3] for r in rows],
            "Close": [r[4] for r in rows],
            "Volume": [r[5] for r in rows],
        }, index=idx)

    module.Ticker.history = history


def _patch_adapter(module):
    cls = getattr(module, "TopStepExchangeAdapter", None)
    data = _load()
    if cls is not None and "topstep_market_open" in data:
        is_open = bool(data["topstep_market_open"])
        cls.is_market_open = lambda self: is_open


_PATCHES = {"ccxt": _patch_ccxt, "yfinance": _patch_yfinance, "adapter": _patch_adapter}


class _FixtureFinder(importlib.abc.MetaPathFinder):

    def find_spec(self, name, path, target=None):
        patch = _PATCHES.get(name)
        if patch is None:
            return None
        spec = importlib.machinery.PathFinder.find_spec(name, path)
        if spec is None or spec.loader is None:
            return None
        original = spec.loader.exec_module

        def exec_module(module):
            original(module)
            patch(module)

        spec.loader.exec_module = exec_module
        return spec


if _FIXTURE_PATH:
    _block_network()
    sys.meta_path.insert(0, _FixtureFinder())
