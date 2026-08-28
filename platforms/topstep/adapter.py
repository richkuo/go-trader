
import os
import sys
import time
from datetime import datetime, timezone, timedelta

API_BASE_URL = "https://api.topstepx.com"

YAHOO_SYMBOL_MAP = {
    "ES": "ES=F",
    "NQ": "NQ=F",
    "MES": "MES=F",
    "MNQ": "MNQ=F",
    "CL": "CL=F",
    "GC": "GC=F",
}

CONTRACT_SPECS = {
    "ES": {"tick_size": 0.25, "tick_value": 12.50, "multiplier": 50, "margin": 15400, "type": "index"},
    "NQ": {"tick_size": 0.25, "tick_value": 5.00, "multiplier": 20, "margin": 21000, "type": "index"},
    "MES": {"tick_size": 0.25, "tick_value": 1.25, "multiplier": 5, "margin": 1540, "type": "index"},
    "MNQ": {"tick_size": 0.25, "tick_value": 0.50, "multiplier": 2, "margin": 2100, "type": "index"},
    "CL": {"tick_size": 0.01, "tick_value": 10.00, "multiplier": 1000, "margin": 7500, "type": "energy"},
    "GC": {"tick_size": 0.10, "tick_value": 10.00, "multiplier": 100, "margin": 11000, "type": "metals"},
}


def _normalize_topstep_fill(f):
    try:
        ts_ms = int(f.get("timestamp", f.get("ts_ms", 0)))
    except (TypeError, ValueError):
        return None
    return {
        "fill_id": str(f.get("id", f.get("fill_id", ""))),
        "ts_ms": ts_ms,
        "symbol": (f.get("symbol") or "").upper(),
        "kind": (f.get("kind") or "").lower(),
        "realized_pnl": float(f.get("realizedPnl", f.get("realized_pnl", 0)) or 0),
        "fee": float(f.get("fee", f.get("commission", 0)) or 0),
    }


class TopStepExchangeAdapter:

    def __init__(self, mode="paper"):
        self._mode = mode
        self._api_key = os.environ.get("TOPSTEP_API_KEY", "")
        self._api_secret = os.environ.get("TOPSTEP_API_SECRET", "")
        self._account_id = os.environ.get("TOPSTEP_ACCOUNT_ID", "")
        self._session = None

        if mode == "live":
            if not self._api_key or not self._api_secret or not self._account_id:
                raise RuntimeError(
                    "Live mode requires TOPSTEP_API_KEY, TOPSTEP_API_SECRET, "
                    "and TOPSTEP_ACCOUNT_ID environment variables"
                )
            try:
                import requests
                self._session = requests.Session()
                self._session.headers.update({
                    "X-API-Key": self._api_key,
                    "X-API-Secret": self._api_secret,
                    "Content-Type": "application/json",
                })
            except ImportError:
                raise ImportError("requests package required for live mode. Run: uv sync")

    @property
    def is_live(self) -> bool:
        return self._mode == "live" and self._session is not None

    @property
    def mode(self) -> str:
        return self._mode

    @property
    def name(self) -> str:
        return "topstep"


    def get_contract_spec(self, symbol: str) -> dict:
        spec = CONTRACT_SPECS.get(symbol)
        if spec is None:
            raise ValueError(f"Unknown symbol: {symbol}. Supported: {list(CONTRACT_SPECS.keys())}")
        return dict(spec)


    def get_price(self, symbol: str) -> float:
        if not self.is_live:
            return self._get_yahoo_price(symbol)
        try:
            resp = self._session.get(
                f"{API_BASE_URL}/v1/market/quote",
                params={"symbol": symbol, "accountId": self._account_id},
                timeout=10,
            )
            resp.raise_for_status()
            data = resp.json()
            return float(data.get("lastPrice", 0))
        except Exception as e:
            print(f"[topstep] get_price error: {e}", file=sys.stderr)
            return 0.0

    def get_ohlcv(self, symbol: str, interval: str = "1h", limit: int = 200) -> list:
        if not self.is_live:
            return self._get_yahoo_ohlcv(symbol, interval, limit)
        try:
            resp = self._session.get(
                f"{API_BASE_URL}/v1/market/candles",
                params={
                    "symbol": symbol,
                    "interval": interval,
                    "limit": limit,
                    "accountId": self._account_id,
                },
                timeout=15,
            )
            resp.raise_for_status()
            candles = resp.json().get("candles", [])
            result = []
            for c in candles:
                result.append([
                    int(c.get("timestamp", 0)),
                    float(c.get("open", 0)),
                    float(c.get("high", 0)),
                    float(c.get("low", 0)),
                    float(c.get("close", 0)),
                    float(c.get("volume", 0)),
                ])
            return result
        except Exception as e:
            print(f"[topstep] get_ohlcv error: {e}", file=sys.stderr)
            return []


    def get_open_positions(self) -> list:
        if not self.is_live:
            return []
        try:
            return self._fetch_open_positions()
        except Exception as e:
            print(f"[topstep] get_open_positions error: {e}", file=sys.stderr)
            return []

    def get_open_positions_raise(self) -> list:
        if self._mode != "live":
            raise RuntimeError("TopStep adapter not in live mode")
        if self._session is None:
            raise RuntimeError(
                "TopStep live session not initialized — missing "
                "TOPSTEP_API_KEY / TOPSTEP_API_SECRET / TOPSTEP_ACCOUNT_ID"
            )
        return self._fetch_open_positions()

    def _fetch_open_positions(self) -> list:
        resp = self._session.get(
            f"{API_BASE_URL}/v1/account/positions",
            params={"accountId": self._account_id},
            timeout=10,
        )
        resp.raise_for_status()
        positions = []
        for pos in resp.json().get("positions", []):
            qty = int(pos.get("quantity", 0))
            if qty == 0:
                continue
            positions.append({
                "symbol": pos.get("symbol", ""),
                "quantity": qty,
                "avg_price": float(pos.get("avgPrice", 0)),
                "side": "long" if qty > 0 else "short",
                "unrealized_pnl": float(pos.get("unrealizedPnl", 0)),
            })
        return positions

    def get_account_equity_and_upnl(self):
        if not self.is_live:
            raise RuntimeError("get_account_equity_and_upnl requires live mode")
        resp = self._session.get(
            f"{API_BASE_URL}/v1/account/balance",
            params={"accountId": self._account_id},
            timeout=10,
        )
        resp.raise_for_status()
        data = resp.json()
        if "equity" not in data:
            raise ValueError("TopStep /v1/account/balance response missing 'equity' field")
        equity = float(data.get("equity", 0))
        cash = float(data.get("cashBalance", data.get("balance", equity)))
        return equity, equity - cash

    def get_account_fills(self, since_ms=0, page_limit=100, max_fills=10000):
        if not self.is_live:
            raise RuntimeError("get_account_fills requires live mode")
        collected = {}
        cursor = int(since_ms or 0)
        capped = False
        for _ in range(max(1, max_fills // max(1, page_limit)) + 2):
            resp = self._session.get(
                f"{API_BASE_URL}/v1/account/fills",
                params={
                    "accountId": self._account_id,
                    "sinceMs": cursor,
                    "limit": int(page_limit),
                },
                timeout=15,
            )
            resp.raise_for_status()
            page = resp.json().get("fills", []) or []
            if not page:
                break
            before = len(collected)
            page_last_ts = cursor
            for f in page:
                fill = _normalize_topstep_fill(f)
                if fill is None:
                    continue
                if fill["ts_ms"] > page_last_ts:
                    page_last_ts = fill["ts_ms"]
                key = fill["fill_id"] or f"{fill['kind']}:{fill['ts_ms']}:{fill['symbol']}"
                collected[key] = fill
            added = len(collected) - before
            if len(collected) >= max_fills:
                capped = True
                break
            if len(page) < int(page_limit):
                break
            if page_last_ts <= cursor and added == 0:
                capped = True
                break
            cursor = page_last_ts
        else:
            capped = True
        fills = sorted(collected.values(), key=lambda x: x["ts_ms"])
        if len(fills) > max_fills:
            fills = fills[:max_fills]
        return fills, capped


    def _get_yahoo_price(self, symbol: str) -> float:
        yahoo_sym = YAHOO_SYMBOL_MAP.get(symbol)
        if not yahoo_sym:
            return 0.0
        try:
            import yfinance as yf
            ticker = yf.Ticker(yahoo_sym)
            hist = ticker.history(period="1d")
            if hist.empty:
                return 0.0
            return float(hist["Close"].iloc[-1])
        except ImportError:
            print("[topstep] yfinance not installed — paper mode has no price data. Run: uv add yfinance", file=sys.stderr)
            return 0.0
        except Exception as e:
            print(f"[topstep] yahoo price error for {symbol}: {e}", file=sys.stderr)
            return 0.0

    def _get_yahoo_ohlcv(self, symbol: str, interval: str = "1h", limit: int = 200) -> list:
        yahoo_sym = YAHOO_SYMBOL_MAP.get(symbol)
        if not yahoo_sym:
            return []
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
            print("[topstep] yfinance not installed — paper mode has no OHLCV data. Run: uv add yfinance", file=sys.stderr)
            return []
        except Exception as e:
            print(f"[topstep] yahoo ohlcv error for {symbol}: {e}", file=sys.stderr)
            return []


    def market_open(self, symbol: str, is_buy: bool, contracts: int) -> dict:
        if not self.is_live:
            raise RuntimeError("market_open requires live mode")
        contracts = int(contracts)
        if contracts <= 0:
            raise ValueError("contracts must be > 0")
        resp = self._session.post(
            f"{API_BASE_URL}/v1/order/market",
            json={
                "accountId": self._account_id,
                "symbol": symbol,
                "side": "buy" if is_buy else "sell",
                "quantity": contracts,
            },
            timeout=10,
        )
        resp.raise_for_status()
        return resp.json()

    def market_close(self, symbol: str) -> dict:
        if not self.is_live:
            raise RuntimeError("market_close requires live mode")
        resp = self._session.post(
            f"{API_BASE_URL}/v1/order/close",
            json={
                "accountId": self._account_id,
                "symbol": symbol,
            },
            timeout=10,
        )
        resp.raise_for_status()
        return resp.json()


    def is_market_open(self) -> bool:
        try:
            from zoneinfo import ZoneInfo
        except ImportError:
            from backports.zoneinfo import ZoneInfo

        now = datetime.now(ZoneInfo("America/New_York"))
        weekday = now.weekday()
        hour = now.hour
        minute = now.minute
        current_minutes = hour * 60 + minute

        maintenance_start = 17 * 60
        maintenance_end = 18 * 60
        if maintenance_start <= current_minutes < maintenance_end:
            return False

        if weekday == 5:
            return False

        if weekday == 6:
            return current_minutes >= maintenance_end

        if weekday == 4:
            return current_minutes < maintenance_start

        return True
