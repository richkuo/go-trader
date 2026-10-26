import importlib.abc
import importlib.machinery
import json
import math
import os
import socket
import sys
import time

_FIXTURE = os.environ.get("PAIR_PAPER_FIXTURE", "")
_BLOCK_ALL = os.environ.get("PAIR_PAPER_BLOCK_NETWORK", "") == "1"
_HOUR = 3_600_000


def _load():
    with open(_FIXTURE) as fh:
        return json.load(fh)


def _log(kind, what):
    path = os.environ.get("PAIR_PAPER_CALL_LOG", "")
    if path:
        with open(path, "a") as fh:
            fh.write(json.dumps([kind, what, int(time.time() * 1000)]) + "\n")


def _blocked(self, address, *args, **kwargs):
    if self.family in (socket.AF_INET, socket.AF_INET6):
        _log("connect", repr(address))
        raise OSError("network blocked by the pair paper fixture: %r" % (address,))
    return _real_connect(self, address, *args, **kwargs)


_real_connect = socket.socket.connect
socket.socket.connect = _blocked
socket.socket.connect_ex = _blocked


def _converged(fx, now):
    return now >= fx["converge_ms"]


def _ladder(start, step, sz, depth):
    return [("%.1f" % (start + step * i), sz) for i in range(depth)]


def _hl_book(fx, now):
    if _converged(fx, now):
        return _ladder(60000, -1, "0.5", 20), _ladder(60002, 1, "0.5", 20)
    return _ladder(59998, -1, "0.5", 20), _ladder(60000, 1, "0.5", 20)


def _okx_book(fx, now, depth):
    if _converged(fx, now):
        return _ladder(59999.9, -0.1, "30", depth), _ladder(60000, 0.1, "30", depth)
    return _ladder(62000, -0.1, "30", depth), _ladder(62000.1, 0.1, "30", depth)


def _price(open_ms):
    i = open_ms // _HOUR
    return 60000 * (1 + 0.003 * math.sin(i / 4.0))


def _bar(open_ms, ivl):
    px = _price(open_ms)
    return px, px * 1.004, px * 0.996, px * 1.001, 100 + (open_ms // ivl) % 50


def _ivl(text):
    n, unit = int(text[:-1]), text[-1].lower()
    return n * {"m": 60_000, "h": _HOUR, "d": 86_400_000}[unit]


_META = {"universe": [{"name": "BTC", "szDecimals": 3, "maxLeverage": 20}]}
_SPOT = {"tokens": [{"name": "USDC", "szDecimals": 8, "weiDecimals": 8, "index": 0, "tokenId": "0x0", "isCanonical": True},
                    {"name": "PURR", "szDecimals": 0, "weiDecimals": 5, "index": 1, "tokenId": "0x1", "isCanonical": True}],
         "universe": [{"tokens": [1, 0], "name": "PURR/USDC", "index": 0, "isCanonical": True}]}


def _hl_post(self, path, payload=None):
    fx = _load()
    payload = payload or {}
    t = payload.get("type")
    now = int(time.time() * 1000)
    _log("hl", t)
    if fx.get("fail_hl") and t == fx["fail_hl"]:
        raise RuntimeError("fixture failure of hyperliquid " + t)
    if path != "/info":
        raise RuntimeError("the pair paper fixture serves only /info, not " + str(path))
    if t == "meta":
        return _META
    if t == "spotMeta":
        return _SPOT
    if t == "l2Book" and payload.get("coin") == "BTC":
        bids, asks = _hl_book(fx, now)
        return {"coin": "BTC", "time": now - 30, "levels": [[{"px": p, "sz": s, "n": 2} for p, s in bids], [{"px": p, "sz": s, "n": 2} for p, s in asks]]}
    if t == "candleSnapshot":
        req = payload.get("req") or {}
        ivl = _ivl(req["interval"])
        start, end = int(req["startTime"]), int(req["endTime"])
        out = []
        o = start - start % ivl
        while o <= end:
            px, hi, lo, cl, vol = _bar(o, ivl)
            out.append({"t": o, "T": o + ivl - 1, "s": req["coin"], "i": req["interval"], "o": "%.2f" % px, "h": "%.2f" % hi, "l": "%.2f" % lo,
                        "c": "%.2f" % cl, "v": "%.3f" % vol, "n": 10})
            o += ivl
        return out
    if t == "fundingHistory":
        start = int(payload["startTime"])
        end = int(payload.get("endTime") or now)
        rows = [[int(r[0]), r[1]] for r in fx.get("hl_funding", [])]
        if fx.get("follow_funding"):
            rows.append([end, "0.0000125"])
        out = []
        for tm, rate in sorted(rows):
            if start <= tm <= end:
                out.append({"coin": payload["coin"], "fundingRate": rate, "premium": "0.0001", "time": tm})
        return out
    raise RuntimeError("the pair paper fixture has no hyperliquid answer for " + str(t))


def _query(url):
    out = {}
    if "?" in url:
        for part in url.split("?", 1)[1].split("&"):
            k, _, v = part.partition("=")
            out[k] = v
    return out


def _okx_fetch(self, url, method="GET", headers=None, body=None):
    fx = _load()
    now = int(time.time() * 1000)
    path = url.split("?")[0]
    q = _query(url)
    _log("okx", path.split("/api/v5/")[-1])
    if fx.get("fail_okx") and fx["fail_okx"] in path:
        raise RuntimeError("fixture failure of okx " + path)
    if "/api/v5/market/books" in path and q.get("instId") == "BTC-USDT-SWAP":
        sz = int(q.get("sz", "5"))
        bids, asks = _okx_book(fx, now, sz)
        return {"code": "0", "msg": "", "data": [{"asks": [[p, s, "0", "2"] for p, s in asks], "bids": [[p, s, "0", "2"] for p, s in bids], "ts": str(now - 20)}]}
    if "/api/v5/public/instruments" in path:
        return {"code": "0", "msg": "", "data": [{"instId": "BTC-USDT-SWAP", "instType": "SWAP", "instFamily": "BTC-USDT", "ctValCcy": "BTC", "settleCcy": "USDT",
                                                  "ctVal": "0.01", "ctType": "linear", "tickSz": "0.1", "lotSz": "1", "minSz": "1", "lever": "20", "state": "live"}]}
    if "/api/v5/market/candles" in path:
        ivl = {"1m": 60_000, "5m": 300_000, "15m": 900_000, "1H": _HOUR, "4H": 4 * _HOUR, "1D": 86_400_000}[q["bar"]]
        limit = int(q.get("limit", "100"))
        cur = now - now % ivl
        rows = []
        for i in range(limit):
            o = cur - i * ivl
            px, hi, lo, cl, vol = _bar(o, ivl)
            rows.append([str(o), "%.1f" % px, "%.1f" % hi, "%.1f" % lo, "%.1f" % cl, "%.3f" % vol, "0", "0", "0" if i == 0 else "1"])
        return {"code": "0", "msg": "", "data": rows}
    if "/api/v5/public/funding-rate-history" in path:
        after = int(q["after"])
        limit = int(q.get("limit", "100"))
        rows = [[int(r[0]), r[1]] for r in fx.get("okx_funding", [])]
        if fx.get("follow_funding"):
            rows.append([after - 1, "0.0001"])
        rows = sorted([r for r in rows if r[0] < after], reverse=True)[:limit]
        return {"code": "0", "msg": "", "data": [{"instId": "BTC-USDT-SWAP", "fundingTime": str(tm), "realizedRate": rate, "fundingRate": rate, "method": "current_period",
                                                  "formulaType": "withRate"} for tm, rate in rows]}
    if "/api/v5/market/history-mark-price-candles" in path:
        after = int(q["after"])
        at = after - 1
        if at % 60_000:
            return {"code": "0", "msg": "", "data": []}
        px = _price(at)
        return {"code": "0", "msg": "", "data": [[str(at), "%.1f" % px, "%.1f" % px, "%.1f" % px, "%.1f" % px, "1"]]}
    raise RuntimeError("the pair paper fixture has no okx answer for " + path)


def _patch_hl(module):
    module.API.post = _hl_post


def _patch_ccxt(module):
    module.okx.fetch = _okx_fetch
    module.okx.load_markets = lambda self, *a, **k: (_ for _ in ()).throw(RuntimeError("okx market load is forbidden in the pair paper fixture"))


_PATCHES = {"hyperliquid.api": _patch_hl, "ccxt": _patch_ccxt}


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


if _FIXTURE and not _BLOCK_ALL:
    sys.meta_path.insert(0, _FixtureFinder())
