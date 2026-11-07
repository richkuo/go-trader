import importlib.abc
import importlib.machinery
import importlib.util
import json
import os
import socket
import sys

_FIXTURE_PATH = os.environ.get("CANDLE_RANGE_FIXTURE", "")
_HTF_SITE = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "htf_offline", "sitecustomize.py")


def _load():
    with open(_FIXTURE_PATH) as fh:
        return json.load(fh)


def _log(kind, entry):
    data = _load()
    path = data.get("log")
    if path:
        with open(path, "a") as fh:
            fh.write(json.dumps([kind] + list(entry)) + "\n")


def _block_network():
    real_connect = socket.socket.connect

    def blocked_connect(self, address, *args, **kwargs):
        if self.family in (socket.AF_INET, socket.AF_INET6):
            raise OSError("network blocked by the candle range fixture: %r" % (address,))
        return real_connect(self, address, *args, **kwargs)

    socket.socket.connect = blocked_connect


def _patch_ccxt(module):
    def fetch_ohlcv(self, symbol, timeframe="1m", since=None, limit=None, params=None):
        _log("ccxt", [self.id, symbol, timeframe, since, limit])
        data = _load()
        rows = [list(r) for r in data.get("ccxt", {}).get(symbol + "|" + timeframe, [])]
        if data.get("ccxt_stall"):
            return rows[:2]
        cap = data.get("ccxt_page_cap")
        if since is not None:
            rows = [r for r in rows if r[0] >= since]
            n = limit or len(rows)
            if cap:
                n = min(n, cap)
            return rows[:n]
        return rows[-limit:] if limit else rows

    for name in ("binanceus",):
        getattr(module, name).fetch_ohlcv = fetch_ohlcv


class _FakeInfo:
    def __init__(self, *args, **kwargs):
        pass

    def candles_snapshot(self, name, interval, startTime, endTime):
        _log("hl", [name, interval, startTime, endTime])
        data = _load()
        rows = [dict(r) for r in data.get("hl", [])]
        if data.get("hl_stall"):
            return rows[:2]
        rows = [r for r in rows if startTime <= r["t"] <= endTime]
        cap = data.get("hl_page_cap")
        return rows[:cap] if cap else rows


def _patch_hl_info(module):
    module.Info = _FakeInfo


def _patch_adapter(module):
    if hasattr(module, "_fetch_raw_meta"):
        module._fetch_raw_meta = lambda base_url: ({"tokens": [], "universe": []}, {"universe": []})


def _patch_yfinance(module):
    spec = importlib.util.spec_from_file_location("htf_offline_site", _HTF_SITE)
    htf = importlib.util.module_from_spec(spec)
    os.environ.setdefault("HTF_OFFLINE_FIXTURE", "")
    spec.loader.exec_module(htf)
    htf._load = _load
    htf._patch_yfinance(module)


_PATCHES = {
    "ccxt": _patch_ccxt,
    "hyperliquid.info": _patch_hl_info,
    "adapter": _patch_adapter,
    "yfinance": _patch_yfinance,
}


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
