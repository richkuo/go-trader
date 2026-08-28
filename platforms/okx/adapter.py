
import os
import sys
import math
import time
from typing import Tuple

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), '..', '..', 'shared_tools'))

import ccxt


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
        return self._is_live

    @property
    def mode(self) -> str:
        return "live" if self.is_live else "paper"

    @property
    def name(self) -> str:
        return "okx"


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


    def fetch_open_positions(self) -> list:
        if not self._is_live:
            raise RuntimeError(
                "fetch_open_positions requires live mode (set OKX_API_KEY, OKX_API_SECRET, OKX_PASSPHRASE)"
            )
        return self._exchange.fetch_positions() or []

    def market_open(self, symbol: str, is_buy: bool, size: float, inst_type: str = "spot") -> dict:
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

    def market_close(self, symbol: str, sz: float | None = None) -> dict:
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
        return results[0] if results else {}

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
