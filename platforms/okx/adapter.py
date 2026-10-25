
import os
import re
import sys
import math
import time
from typing import Tuple

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), '..', '..', 'shared_tools'))

import ccxt
import egress_policy


def _bill_float(value) -> float:
    try:
        return float(value)
    except (TypeError, ValueError):
        return 0.0


def _okx_usdt_cash_balance(info):
    try:
        details = ((info or {}).get("data") or [{}])[0].get("details") or []
    except (AttributeError, IndexError, TypeError):
        return None
    for d in details:
        try:
            if str(d.get("ccy") or "").upper() == "USDT":
                return float(d.get("cashBal"))
        except (TypeError, ValueError):
            return None
    return None


def _normalize_okx_bill(entry: dict) -> dict:
    info = entry.get("info") or {}
    ts = info.get("ts")
    if ts in (None, ""):
        ts = entry.get("timestamp")
    return {
        "bill_id": str(info.get("billId") or entry.get("id") or ""),
        "ts_ms": int(_bill_float(ts)),
        "ccy": str(info.get("ccy") or ""),
        "type": str(info.get("type") or ""),
        "sub_type": str(info.get("subType") or ""),
        "bal_chg": _bill_float(info.get("balChg")),
        "pnl": _bill_float(info.get("pnl")),
        "fee": _bill_float(info.get("fee")),
        "inst_id": str(info.get("instId") or ""),
        "trade_id": str(info.get("tradeId") or ""),
    }


class OKXExchangeAdapter:

    def __init__(self, public_only: bool = False):
        self._public_only = bool(public_only)
        api_key = "" if self._public_only else os.environ.get("OKX_API_KEY", "")
        api_secret = "" if self._public_only else os.environ.get("OKX_API_SECRET", "")
        passphrase = "" if self._public_only else os.environ.get("OKX_PASSPHRASE", "")
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

        self._exchange = egress_policy.pin_ccxt(ccxt.okx(config))
        self._markets_loaded = False

    @property
    def is_live(self) -> bool:
        return self._is_live

    @property
    def mode(self) -> str:
        return "live" if self.is_live else "paper"

    @property
    def name(self) -> str:
        return "okx"


    PUBLIC_INSTRUMENT_FIELDS = ("instId", "instType", "instFamily", "ctValCcy", "settleCcy", "ctVal", "ctType", "tickSz", "lotSz", "minSz", "lever", "state")

    def public_instrument_rules(self, inst_id: str) -> dict:
        if not self._public_only:
            raise RuntimeError("public instrument rules need the public-only reader")
        resp = self._exchange.publicGetPublicInstruments({"instType": "SWAP", "instId": inst_id})
        if not isinstance(resp, dict) or str(resp.get("code")) != "0":
            msg = resp.get("msg") if isinstance(resp, dict) else resp
            raise RuntimeError(f"OKX public instruments refused {inst_id}: {msg!r}")
        data = resp.get("data")
        if not isinstance(data, list) or len(data) != 1 or not isinstance(data[0], dict):
            raise RuntimeError(f"OKX public instruments returned {len(data) if isinstance(data, list) else 'no'} records for {inst_id}")
        record = data[0]
        out = {}
        for field in self.PUBLIC_INSTRUMENT_FIELDS:
            value = record.get(field)
            if not isinstance(value, str):
                raise RuntimeError(f"OKX instrument {inst_id} field {field} is not a string: {value!r}")
            out[field] = value
        if out["instId"] != inst_id:
            raise RuntimeError(f"OKX public instruments returned {out['instId']} for {inst_id}")
        return out

    def public_host(self) -> str:
        rest = ((self._exchange.urls or {}).get("api") or {}).get("rest")
        if not isinstance(rest, str):
            raise RuntimeError("OKX public REST host is unknown")
        return rest.replace("{hostname}", str(getattr(self._exchange, "hostname", "")))

    def public_order_book(self, inst_id: str, depth: int) -> dict:
        if not self._public_only:
            raise RuntimeError("public depth reads need the public-only reader")
        if isinstance(depth, bool) or not isinstance(depth, int) or not 1 <= depth <= 20:
            raise RuntimeError(f"OKX book depth {depth!r} is outside [1, 20]")
        request = {"instId": inst_id, "sz": str(depth)}
        resp = self._exchange.publicGetMarketBooks(dict(request))
        if not isinstance(resp, dict) or str(resp.get("code")) != "0" or not isinstance(resp.get("code"), str):
            msg = resp.get("msg") if isinstance(resp, dict) else resp
            raise RuntimeError(f"OKX public books refused {inst_id}: {msg!r}")
        msg = resp.get("msg", "")
        if not isinstance(msg, str):
            raise RuntimeError("OKX public books msg is not text")
        data = resp.get("data")
        if not isinstance(data, list) or len(data) != 1 or not isinstance(data[0], dict):
            raise RuntimeError(f"OKX public books returned {len(data) if isinstance(data, list) else 'no'} books for {inst_id}")
        book = data[0]
        out = {}
        for name in ("asks", "bids"):
            side = book.get(name)
            if not isinstance(side, list):
                raise RuntimeError(f"OKX public books {name} is not a list")
            levels = []
            for level in side:
                if not isinstance(level, list) or len(level) != 4 or not all(isinstance(x, str) for x in level):
                    raise RuntimeError(f"OKX public books level {level!r} is not four text fields")
                levels.append(list(level))
            out[name] = levels
        ts = book.get("ts")
        if not isinstance(ts, str):
            raise RuntimeError("OKX public books ts is not text")
        out["ts"] = ts
        return {"request": request, "response": {"code": "0", "data": [out], "msg": msg}}

    def _load_markets(self):
        if not self._markets_loaded:
            self._exchange.load_markets()
            self._markets_loaded = True

    def get_spot_price(self, symbol: str) -> float:
        for suffix in ("/USDT", "/USD", "/USDC"):
            try:
                ticker = self._exchange.fetch_ticker(symbol + suffix)
                price = ticker.get("last") or 0
                if price and price > 0:
                    return float(price)
            except Exception:
                continue
        return 0.0

    def get_perp_price(self, symbol: str) -> float:
        try:
            ticker = self._exchange.fetch_ticker(f"{symbol}/USDT:USDT")
            price = ticker.get("last") or 0
            if price and price > 0:
                return float(price)
        except Exception:
            pass
        return 0.0

    def get_ohlcv(self, symbol: str, interval: str = "1h", limit: int = 200) -> list:
        pair = f"{symbol}/USDT"
        try:
            candles = self._exchange.fetch_ohlcv(pair, interval, limit=limit)
            return candles
        except Exception:
            return []

    OHLCV_TIMESTAMP_KIND = "open"

    def fetch_candles(self, symbol: str, interval: str, limit: int, inst_type: str = "spot") -> list:
        pair = f"{symbol}/USDT:USDT" if inst_type == "swap" else f"{symbol}/USDT"
        candles = self._exchange.fetch_ohlcv(pair, interval, limit=limit)
        return [list(c) for c in (candles or [])]

    def get_ohlcv_closes(self, symbol: str, interval: str = "1h", limit: int = 200) -> list:
        candles = self.get_ohlcv(symbol, interval, limit)
        return [c[4] for c in candles] if candles else []

    def get_perp_ohlcv(self, symbol: str, interval: str = "1h", limit: int = 200) -> list:
        pair = f"{symbol}/USDT:USDT"
        try:
            candles = self._exchange.fetch_ohlcv(pair, interval, limit=limit)
            return candles
        except Exception:
            return []

    def get_funding_rate(self, symbol: str) -> float:
        try:
            pair = f"{symbol}/USDT:USDT"
            data = self._exchange.fetch_funding_rate(pair)
            return float(data.get("fundingRate", 0) or 0)
        except Exception:
            return 0.0

    def get_funding_history(self, symbol: str, days: int = 7) -> list:
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

    FUNDING_PAGE_MAX = 400
    FUNDING_REQUESTS_MAX = 64
    FUNDING_INST = re.compile(r"^[A-Z0-9]+-USDT-SWAP$")
    FUNDING_DECIMAL = re.compile(r"^-?[0-9]+(\.[0-9]+)?([eE][-+]?[0-9]{1,4})?$")

    @classmethod
    def funding_inst_id(cls, symbol: str) -> str:
        if not isinstance(symbol, str) or not symbol.strip() or symbol != symbol.strip():
            raise ValueError(f"OKX funding symbol {symbol!r} must be a trimmed coin or a <COIN>-USDT-SWAP instrument")
        inst = symbol if symbol.endswith("-USDT-SWAP") else f"{symbol.upper()}-USDT-SWAP"
        if not cls.FUNDING_INST.match(inst):
            raise ValueError(f"OKX funding instrument {inst!r} is not a <COIN>-USDT-SWAP perpetual")
        return inst

    def get_funding_history_range(self, symbol: str, start_ms: int, end_ms: int, page_limit: int = 100,
                                  max_requests: int = 16, deadline_ms: int = 30000) -> dict:
        for name, value in (("start_ms", start_ms), ("end_ms", end_ms), ("page_limit", page_limit),
                            ("max_requests", max_requests), ("deadline_ms", deadline_ms)):
            if isinstance(value, bool) or not isinstance(value, int):
                raise ValueError(f"OKX funding range {name} must be an integer, got {value!r}")
        if start_ms < 0 or start_ms >= end_ms:
            raise ValueError(f"OKX funding range ({start_ms}, {end_ms}] is empty or negative")
        if not 1 <= page_limit <= self.FUNDING_PAGE_MAX:
            raise ValueError(f"OKX funding page_limit {page_limit} is outside [1, {self.FUNDING_PAGE_MAX}]")
        if not 1 <= max_requests <= self.FUNDING_REQUESTS_MAX:
            raise ValueError(f"OKX funding max_requests {max_requests} is outside [1, {self.FUNDING_REQUESTS_MAX}]")
        if deadline_ms <= 0:
            raise ValueError("OKX funding deadline_ms must be above zero")
        inst = self.funding_inst_id(symbol)
        stop_at = time.monotonic() + deadline_ms / 1000.0
        events = {}
        cursor = end_ms + 1
        requests = 0
        reached_start = False
        stop_reason = ""
        error = ""
        while True:
            if requests >= max_requests:
                stop_reason = "request_limit"
                break
            if time.monotonic() >= stop_at:
                stop_reason = "deadline"
                break
            requests += 1
            try:
                resp = self._exchange.publicGetPublicFundingRateHistory({"instId": inst, "after": str(cursor), "limit": str(page_limit)})
            except ccxt.NetworkError as exc:
                stop_reason, error = "transport_error", f"{type(exc).__name__}: {exc}"
                break
            except Exception as exc:
                stop_reason, error = "venue_error", f"{type(exc).__name__}: {exc}"
                break
            if not isinstance(resp, dict) or not isinstance(resp.get("code"), str) or resp.get("code") != "0":
                msg = resp.get("msg") if isinstance(resp, dict) else resp
                stop_reason, error = "venue_error", f"OKX funding history refused {inst}: {msg!r}"
                break
            data = resp.get("data")
            if not isinstance(data, list):
                stop_reason, error = "malformed_response", "OKX funding history data is not a list"
                break
            if len(data) > page_limit:
                stop_reason, error = "truncation", f"OKX funding history returned {len(data)} rows for a page limit of {page_limit}"
                break
            if not data:
                stop_reason = "history_exhausted"
                break
            page_min = None
            bad = ""
            for row in data:
                if not isinstance(row, dict):
                    bad = "a funding history row is not an object"
                    break
                if row.get("instId") != inst:
                    bad = f"a funding history row is for {row.get('instId')!r}, not {inst}"
                    break
                ft, rate = row.get("fundingTime"), row.get("realizedRate")
                if not isinstance(ft, str) or not ft.isdigit() or not isinstance(rate, str) or not self.FUNDING_DECIMAL.match(rate):
                    bad = f"a funding history row has fundingTime {ft!r} and realizedRate {rate!r}"
                    break
                t = int(ft)
                if t >= cursor:
                    bad = f"funding time {t} is not earlier than the cursor {cursor}; the cursor made no progress"
                    break
                if t in events and events[t]["realized_rate"] != rate:
                    bad = f"funding time {t} carries conflicting realized rates {events[t]['realized_rate']!r} and {rate!r}"
                    break
                if start_ms < t <= end_ms:
                    events[t] = {"event_ms": t, "inst_id": inst, "realized_rate": rate,
                                 "funding_rate": row.get("fundingRate") if isinstance(row.get("fundingRate"), str) else "",
                                 "method": row.get("method") if isinstance(row.get("method"), str) else "",
                                 "formula_type": row.get("formulaType") if isinstance(row.get("formulaType"), str) else ""}
                if t <= start_ms:
                    reached_start = True
                page_min = t if page_min is None else min(page_min, t)
            if bad:
                stop_reason, error = "malformed_response", bad
                break
            if reached_start:
                break
            if len(data) < page_limit:
                stop_reason = "history_exhausted"
                break
            cursor = page_min
        ordered = [] if error else [events[t] for t in sorted(events)]
        proven, gaps = [], []
        if error:
            gaps.append({"from_ms": start_ms, "through_ms": end_ms, "reason": stop_reason})
        elif not ordered:
            gaps.append({"from_ms": start_ms, "through_ms": end_ms, "reason": "after_last_record" if reached_start else stop_reason})
        else:
            newest = ordered[-1]["event_ms"]
            lower = start_ms if reached_start else ordered[0]["event_ms"] - 1
            proven.append({"from_ms": lower, "through_ms": newest})
            if lower > start_ms:
                gaps.append({"from_ms": start_ms, "through_ms": lower, "reason": stop_reason})
            if newest < end_ms:
                gaps.append({"from_ms": newest, "through_ms": end_ms, "reason": "after_last_record"})
        return {
            "provider": "okx",
            "inst_id": inst,
            "rate_field": "realizedRate",
            "requested": {"from_ms": start_ms, "through_ms": end_ms},
            "events": ordered,
            "proven": proven,
            "gaps": gaps,
            "complete": bool(proven) and not gaps,
            "requests": requests,
            "stop_reason": stop_reason,
            "error": error,
        }

    def fetch_open_positions(self) -> list:
        if not self._is_live:
            raise RuntimeError(
                "fetch_open_positions requires live mode (set OKX_API_KEY, OKX_API_SECRET, OKX_PASSPHRASE)"
            )
        return self._exchange.fetch_positions() or []

    @staticmethod
    def order_pair(symbol: str, inst_type: str = "spot") -> str:
        if inst_type == "swap":
            return f"{symbol}/USDT:USDT"
        return f"{symbol}/USDT"

    def market_open(self, symbol: str, is_buy: bool, size: float, inst_type: str = "spot",
                    client_order_id: str | None = None, reduce_only: bool = False,
                    exp_time_ms: int | None = None) -> dict:
        if not self._is_live:
            raise RuntimeError(
                "market_open requires live mode (set OKX_API_KEY, OKX_API_SECRET, OKX_PASSPHRASE)"
            )
        side = "buy" if is_buy else "sell"
        pair = self.order_pair(symbol, inst_type)
        if inst_type == "swap":
            params = {"tdMode": "cross"}
            if reduce_only:
                params["reduceOnly"] = True
        else:
            params = {"tdMode": "cash"}
        if client_order_id:
            params["clOrdId"] = client_order_id
        if not exp_time_ms:
            return self._exchange.create_market_order(pair, side, size, params=params)
        self._load_markets()
        saved = self._exchange.headers
        self._exchange.headers = {**(saved or {}), "expTime": str(int(exp_time_ms))}
        try:
            return self._exchange.create_market_order(pair, side, size, params=params)
        finally:
            self._exchange.headers = saved

    PAIR_SWAP_SUFFIX = "-USDT-SWAP"

    def _require_live(self, caller: str):
        if not self._is_live:
            raise RuntimeError(f"{caller} requires live mode (set OKX_API_KEY, OKX_API_SECRET, OKX_PASSPHRASE)")

    @classmethod
    def pair_symbol(cls, inst_id: str) -> str:
        if not isinstance(inst_id, str) or not inst_id.endswith(cls.PAIR_SWAP_SUFFIX) or len(inst_id) <= len(cls.PAIR_SWAP_SUFFIX):
            raise ValueError(f"instrument {inst_id!r} is not <UNDERLYING>-USDT-SWAP")
        return cls.order_pair(inst_id[:-len(cls.PAIR_SWAP_SUFFIX)], "swap")

    def pair_instrument(self, inst_id: str) -> dict:
        resp = self._exchange.publicGetPublicInstruments({"instType": "SWAP", "instId": inst_id})
        if not isinstance(resp, dict) or str(resp.get("code")) != "0":
            raise RuntimeError(f"OKX public instruments refused {inst_id}: {resp!r}")
        data = resp.get("data")
        if not isinstance(data, list) or len(data) != 1 or not isinstance(data[0], dict):
            raise RuntimeError(f"OKX public instruments returned no single record for {inst_id}")
        record = data[0]
        out = {}
        for field in ("instId", "ctVal", "tickSz", "lotSz", "minSz", "state", "settleCcy"):
            value = record.get(field)
            if not isinstance(value, str) or not value:
                raise RuntimeError(f"OKX instrument {inst_id} field {field} is not text: {value!r}")
            out[field] = value
        if out["instId"] != inst_id:
            raise RuntimeError(f"OKX public instruments returned {out['instId']} for {inst_id}")
        return out

    def pair_order_check(self, inst_id: str, native_text: str, price_text: str) -> dict:
        self._require_live("pair_order_check")
        pair = self.pair_symbol(inst_id)
        inst = self.pair_instrument(inst_id)
        self._load_markets()
        market = self._exchange.market(pair)
        if not isinstance(market, dict) or market.get("id") != inst_id:
            raise RuntimeError(f"the SDK market for {pair} is not {inst_id}")
        return {
            "inst_id": inst_id,
            "contract_size": inst["ctVal"],
            "tick": inst["tickSz"],
            "lot": inst["lotSz"],
            "min": inst["minSz"],
            "state": inst["state"],
            "settle": inst["settleCcy"],
            "amount_text": str(self._exchange.amount_to_precision(pair, float(native_text))),
            "price_text": str(self._exchange.price_to_precision(pair, float(price_text))),
        }

    def pair_limit_ioc(self, inst_id: str, side: str, native_text: str, price_text: str, client_order_id: str,
                       reduce_only: bool, exp_time_ms: int) -> dict:
        self._require_live("pair_limit_ioc")
        if side not in ("buy", "sell"):
            raise ValueError(f"side {side!r} is neither buy nor sell")
        pair = self.pair_symbol(inst_id)
        params = {"tdMode": "cross", "clOrdId": client_order_id, "reduceOnly": bool(reduce_only), "timeInForce": "IOC"}
        self._load_markets()
        saved = self._exchange.headers
        self._exchange.headers = {**(saved or {}), "expTime": str(int(exp_time_ms))}
        try:
            return self._exchange.create_order(pair, "limit", side, float(native_text), float(price_text), params=params)
        finally:
            self._exchange.headers = saved

    def fetch_pair_order(self, inst_id: str, client_order_id: str) -> dict:
        self._require_live("fetch_pair_order")
        order = self._exchange.fetch_order(None, self.pair_symbol(inst_id), params={"clOrdId": client_order_id})
        info = order.get("info") if isinstance(order, dict) else None
        if not isinstance(info, dict):
            raise RuntimeError(f"the order read for {client_order_id} returned no venue record: {order!r}")
        return info

    def fetch_pair_position(self, inst_id: str) -> dict:
        self._require_live("fetch_pair_position")
        self.pair_symbol(inst_id)
        resp = self._exchange.privateGetAccountPositions({"instType": "SWAP", "instId": inst_id})
        if not isinstance(resp, dict) or str(resp.get("code")) != "0" or not isinstance(resp.get("code"), str):
            raise RuntimeError(f"OKX positions read failed: {resp!r}")
        data = resp.get("data")
        if not isinstance(data, list):
            raise RuntimeError(f"OKX positions read returned no list: {resp!r}")
        found = None
        for i, row in enumerate(data):
            if not isinstance(row, dict):
                raise RuntimeError(f"OKX position row {i} is not an object, so no instrument can be proven flat")
            row_inst = row.get("instId")
            if not isinstance(row_inst, str) or not row_inst:
                raise RuntimeError(f"OKX position row {i} names no instrument, so no instrument can be proven flat")
            if row_inst != inst_id:
                raise RuntimeError(f"OKX position row {i} names {row_inst}, outside the requested {inst_id}")
            if found is not None:
                raise RuntimeError(f"the OKX position list holds {inst_id} twice")
            pos_side = row.get("posSide")
            if pos_side != "net":
                raise RuntimeError(f"OKX position {inst_id} has posSide {pos_side!r}; the pair needs net position mode")
            raw = row.get("pos")
            if not isinstance(raw, str) or not raw.strip():
                raise RuntimeError(f"OKX position {inst_id} carries no pos text: {raw!r}")
            mgn = row.get("mgnMode")
            if mgn not in ("cross", "isolated"):
                raise RuntimeError(f"OKX position {inst_id} carries margin mode {mgn!r}")
            found = {"pos": raw.strip(), "pos_side": "net", "margin_mode": mgn, "found": True}
        if found is None:
            return {"pos": "0", "pos_side": "net", "margin_mode": "", "found": False}
        return found

    def fetch_order_by_client_id(self, symbol: str, inst_type: str, client_order_id: str) -> dict:
        if not self._is_live:
            raise RuntimeError(
                "fetch_order_by_client_id requires live mode (set OKX_API_KEY, OKX_API_SECRET, OKX_PASSPHRASE)"
            )
        return self._exchange.fetch_order(None, self.order_pair(symbol, inst_type), params={"clOrdId": client_order_id})

    def fetch_fills_page(self, inst_type: str, begin_ms: int, end_ms: int, limit: int, after: str = "", history: bool = False) -> list:
        if not self._is_live:
            raise RuntimeError(
                "fetch_fills_page requires live mode (set OKX_API_KEY, OKX_API_SECRET, OKX_PASSPHRASE)"
            )
        params = {
            "instType": "SPOT" if inst_type == "spot" else "SWAP",
            "begin": str(int(begin_ms)),
            "end": str(int(end_ms)),
            "limit": str(int(limit)),
        }
        if after:
            params["after"] = str(after)
        if history:
            resp = self._exchange.privateGetTradeFillsHistory(params)
        else:
            resp = self._exchange.privateGetTradeFills(params)
        if not isinstance(resp, dict) or str(resp.get("code")) != "0":
            raise RuntimeError(f"OKX fills query failed: {resp!r}")
        data = resp.get("data")
        if not isinstance(data, list):
            raise RuntimeError(f"OKX fills query returned no list: {resp!r}")
        return data

    def fetch_order_trades(self, symbol: str, inst_type: str, order_id: str) -> list:
        if not self._is_live:
            raise RuntimeError(
                "fetch_order_trades requires live mode (set OKX_API_KEY, OKX_API_SECRET, OKX_PASSPHRASE)"
            )
        return self._exchange.fetch_order_trades(order_id, self.order_pair(symbol, inst_type))

    def fetch_order_info(self, symbol: str, inst_type: str, order_id: str) -> dict:
        if not self._is_live:
            raise RuntimeError(
                "fetch_order_info requires live mode (set OKX_API_KEY, OKX_API_SECRET, OKX_PASSPHRASE)"
            )
        order = self._exchange.fetch_order(order_id, self.order_pair(symbol, inst_type))
        info = order.get("info") if isinstance(order, dict) else None
        if not isinstance(info, dict):
            raise RuntimeError(f"the order read for {order_id} returned no venue record: {order!r}")
        return info

    def market_close(self, symbol: str, sz: float | None = None) -> dict:
        orders = self.market_close_orders(symbol, sz)
        return orders[0] if orders else {}

    def market_close_orders(self, symbol: str, sz: float | None = None) -> list:
        if not self._is_live:
            raise RuntimeError(
                "market_close requires live mode (set OKX_API_KEY, OKX_API_SECRET, OKX_PASSPHRASE)"
            )
        pair = f"{symbol}/USDT:USDT"
        positions = self._exchange.fetch_positions([pair])
        results = []
        for pos in positions:
            contracts = float(pos.get("contracts", 0) or 0)
            if contracts > 0:
                pos_side = pos.get("side", "")
                close_side = "sell" if pos_side == "long" else "buy"
                close_sz = contracts
                if sz is not None:
                    if sz <= 0:
                        continue
                    close_sz = min(float(sz), contracts)
                    if close_sz <= 0:
                        continue
                results.append(self._exchange.create_market_order(
                    pair, close_side, close_sz,
                    params={"tdMode": "cross", "reduceOnly": True}
                ))
        return results

    def get_account_balance(self) -> float:
        if not self._is_live:
            raise RuntimeError(
                "get_account_balance requires live mode (set OKX_API_KEY, OKX_API_SECRET, OKX_PASSPHRASE)"
            )
        bal = self._exchange.fetch_balance()
        total = bal.get("total") or {}
        try:
            return float(total.get("USDT") or 0.0)
        except (TypeError, ValueError):
            return 0.0

    def get_account_equity_and_upnl(self) -> Tuple[float, float]:
        if not self._is_live:
            raise RuntimeError(
                "get_account_equity_and_upnl requires live mode (set OKX_API_KEY, OKX_API_SECRET, OKX_PASSPHRASE)"
            )
        bal = self._exchange.fetch_balance()
        total = bal.get("total") or {}
        try:
            eq = float(total.get("USDT") or 0.0)
        except (TypeError, ValueError):
            eq = 0.0
        cash_bal = _okx_usdt_cash_balance(bal.get("info"))
        upnl = (eq - cash_bal) if cash_bal is not None else 0.0
        return eq, upnl

    def get_account_bills(self, since_ms: int = 0, page_limit: int = 100,
                          max_bills: int = 10000) -> Tuple[list, bool]:
        if not self._is_live:
            raise RuntimeError(
                "get_account_bills requires live mode (set OKX_API_KEY, OKX_API_SECRET, OKX_PASSPHRASE)"
            )
        collected = {}
        cursor = int(since_ms or 0)
        capped = False
        for _ in range(max(1, max_bills // max(1, page_limit)) + 2):
            page = self._exchange.fetch_ledger(code=None, since=cursor, limit=page_limit) or []
            if not page:
                break
            before = len(collected)
            for entry in page:
                bill = _normalize_okx_bill(entry)
                key = bill["bill_id"] or f"{bill['type']}:{bill['ts_ms']}:{bill['trade_id']}"
                collected[key] = bill
            added = len(collected) - before
            if len(collected) >= max_bills:
                capped = True
                break
            if len(page) < page_limit:
                break
            page_last_ts = max((int(e.get("timestamp") or 0) for e in page), default=cursor)
            if page_last_ts <= cursor and added == 0:
                capped = True
                break
            cursor = page_last_ts
        else:
            capped = True
        bills = sorted(collected.values(), key=lambda b: b["ts_ms"])
        if len(bills) > max_bills:
            bills = bills[:max_bills]
        return bills, capped


    def get_vol_metrics(self, underlying: str) -> Tuple[float, float]:
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
            from datetime import timedelta
            syn = now + timedelta(days=target_dte)
            return syn.strftime("%Y-%m-%d"), target_dte

        best_exp = None
        best_diff = float("inf")
        best_dte = 0
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
        self._load_markets()
        from datetime import datetime, timezone

        exp_dt = datetime.strptime(expiry, "%Y-%m-%d").replace(tzinfo=timezone.utc)
        exp_start = int(exp_dt.timestamp() * 1000)
        exp_end = exp_start + 86400 * 1000

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
            if underlying.upper() == "BTC":
                return round(target_strike / 1000) * 1000
            elif underlying.upper() == "ETH":
                return round(target_strike / 100) * 100
            return round(target_strike / 50) * 50

        return min(strikes, key=lambda s: abs(s - target_strike))

    def get_premium_and_greeks(self, underlying: str, option_type: str,
                                strike: float, expiry: str, dte: float,
                                spot: float, vol: float) -> Tuple[float, float, dict]:
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
                        and float(market.get("strike") or 0) == strike
                        and market.get("active", True)):
                    mkt_exp = market.get("expiry")
                    if mkt_exp and exp_start <= int(mkt_exp) < exp_end:
                        ticker = self._exchange.fetch_ticker(sym)
                        mark = ticker.get("last") or ticker.get("close") or 0
                        if mark and mark > 0:
                            premium_usd = float(mark) * spot
                            premium_pct = float(mark)
                            greeks = {
                                "delta": ticker.get("info", {}).get("delta", 0),
                                "gamma": ticker.get("info", {}).get("gamma", 0),
                                "theta": ticker.get("info", {}).get("theta", 0),
                                "vega": ticker.get("info", {}).get("vega", 0),
                            }
                            greeks = {k: float(v or 0) for k, v in greeks.items()}
                            return premium_pct, premium_usd, greeks
        except Exception:
            pass

        try:
            from pricing import bs_price_and_greeks
            premium_usd, greeks = bs_price_and_greeks(spot, strike, dte, vol, option_type)
            premium_pct = premium_usd / spot if spot > 0 else 0
            return round(premium_pct, 6), round(premium_usd, 2), greeks
        except Exception:
            return 0.0, 0.0, {"delta": 0, "gamma": 0, "theta": 0, "vega": 0}
