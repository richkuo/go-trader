"""
Phemex Exchange Adapter — unified interface for spot and perpetual swaps.
Uses CCXT for all API interactions.

Supports paper (public API only, no credentials) and
live (real orders on Phemex, API credentials required) modes.

Environment variables:
    PHEMEX_API_KEY        — API key for live trading
    PHEMEX_API_SECRET     — API secret for live trading
    PHEMEX_SANDBOX=1      — use Phemex testnet environment
"""

import os
import sys
import math
import time
from typing import Tuple

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), '..', '..', 'shared_tools'))

import ccxt


class PhemexExchangeAdapter:
    """
    Exchange adapter for Phemex — spot and perpetual swaps.

    Paper mode:  no credentials needed; uses live Phemex prices for simulation.
    Live mode:   requires PHEMEX_API_KEY, PHEMEX_API_SECRET.
    """

    def __init__(self):
        api_key = os.environ.get("PHEMEX_API_KEY", "")
        api_secret = os.environ.get("PHEMEX_API_SECRET", "")
        sandbox = os.environ.get("PHEMEX_SANDBOX", "") == "1"

        config = {
            "enableRateLimit": True,
        }
        if sandbox:
            config["sandbox"] = True

        self._is_live = bool(api_key and api_secret)
        if self._is_live:
            config["apiKey"] = api_key
            config["secret"] = api_secret

        self._exchange = ccxt.phemex(config)
        self._markets_loaded = False

    @property
    def is_live(self) -> bool:
        """True if API credentials are provided (live mode)."""
        return self._is_live

    @property
    def mode(self) -> str:
        """'live' or 'paper'."""
        return "live" if self.is_live else "paper"

    @property
    def name(self) -> str:
        return "phemex"

    # ─────────────────────────────────────────────
    # Market data
    # ─────────────────────────────────────────────

    def _load_markets(self):
        """Load and cache markets from Phemex."""
        if not self._markets_loaded:
            self._exchange.load_markets()
            self._markets_loaded = True

    def get_spot_price(self, symbol: str) -> float:
        """Get current spot price for a coin (e.g. 'BTC')."""
        self._load_markets()
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
        """Get current last price for a perpetual swap (e.g. 'BTC')."""
        self._load_markets()
        try:
            ticker = self._exchange.fetch_ticker(f"{symbol}/USDT:USDT")
            price = ticker.get("last") or 0
            if price and price > 0:
                return float(price)
        except Exception:
            pass
        return 0.0

    def get_ohlcv(self, symbol: str, interval: str = "1h", limit: int = 100) -> list:
        """
        Fetch OHLCV candles from Phemex.

        interval: "1m", "5m", "15m", "30m", "1h", "2h", "4h", "1d", etc.
        Returns list of [timestamp_ms, open, high, low, close, volume].
        Note: limit must be <= 100 or >= 500 due to Phemex API constraints.
        """
        self._load_markets()
        # Phemex rejects limits in 150-200 range; cap at 100 for safety
        safe_limit = min(limit, 100) if limit < 500 else limit
        pair = f"{symbol}/USDT"
        try:
            candles = self._exchange.fetch_ohlcv(pair, interval, limit=safe_limit)
            return candles  # ccxt already returns [ts, o, h, l, c, v]
        except Exception:
            return []

    def get_ohlcv_closes(self, symbol: str, interval: str = "1h", limit: int = 100) -> list:
        """Fetch OHLCV and return just close prices (for HTF filter compatibility)."""
        candles = self.get_ohlcv(symbol, interval, limit)
        return [c[4] for c in candles] if candles else []

    def get_perp_ohlcv(self, symbol: str, interval: str = "1h", limit: int = 100) -> list:
        """Fetch OHLCV candles for perpetual swap (USDT-margined).
        
        Note: limit must be <= 100 or >= 500 due to Phemex API constraints.
        """
        self._load_markets()
        # Phemex rejects limits in 150-200 range; cap at 100 for safety
        safe_limit = min(limit, 100) if limit < 500 else limit
        pair = f"{symbol}/USDT:USDT"
        try:
            candles = self._exchange.fetch_ohlcv(pair, interval, limit=safe_limit)
            return candles
        except Exception:
            return []

    def get_funding_rate(self, symbol: str) -> float:
        """Get current predicted funding rate for a perpetual swap.

        Returns the raw rate as a float (e.g. 0.0001 = 0.01% per 8h).
        """
        self._load_markets()
        try:
            pair = f"{symbol}/USDT:USDT"
            data = self._exchange.fetch_funding_rate(pair)
            return float(data.get("fundingRate", 0) or 0)
        except Exception:
            return 0.0

    def get_funding_history(self, symbol: str, days: int = 7) -> list:
        """Get historical funding rate snapshots.

        Returns list of {"rate": float, "time": int} dicts, newest last.
        """
        self._load_markets()
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

    # ─────────────────────────────────────────────
    # Order execution (live mode only)
    # ─────────────────────────────────────────────

    def fetch_open_positions(self) -> list:
        """Return every open perpetual swap position on the account.

        Thin wrapper around ccxt's ``fetch_positions`` — exists so shared
        scripts can stay off the private ``_exchange`` attribute (CLAUDE.md
        rule). Raises in paper mode: position queries require auth.
        """
        if not self._is_live:
            raise RuntimeError(
                "fetch_open_positions requires live mode (set PHEMEX_API_KEY, PHEMEX_API_SECRET)"
            )
        return self._exchange.fetch_positions() or []

    def market_open(self, symbol: str, is_buy: bool, size: float, inst_type: str = "spot") -> dict:
        """
        Place a market order.

        inst_type: "spot" for spot trading, "swap" for perpetual swap.
        Only available in live mode; raises RuntimeError in paper mode.
        """
        if not self._is_live:
            raise RuntimeError(
                "market_open requires live mode (set PHEMEX_API_KEY, PHEMEX_API_SECRET)"
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
        """
        Close an open perpetual swap position for a symbol (reduce-only).

        When ``sz`` is None, closes the full on-chain contracts for the
        position (portfolio kill switch / sole-owner circuit breakers).
        When ``sz`` is set, submits a reduce-only market order for that
        contract quantity only — used for shared-wallet per-strategy
        circuit breakers (#360). The caller is responsible for sizing;
        Phemex enforces reduceOnly=True on the order itself so an oversized
        request cannot flip the position.

        Only available in live mode; raises RuntimeError in paper mode.
        """
        if not self._is_live:
            raise RuntimeError(
                "market_close requires live mode (set PHEMEX_API_KEY, PHEMEX_API_SECRET)"
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
        """Return total USDT-denominated account value for shared-wallet
        aggregation (#360 phase 2 — unlocks multi-strategy Phemex portfolio
        value correctness). Sums free + used USDT; callers that need to
        include open-position PnL should rely on ccxt's total field.

        Only available in live mode; raises RuntimeError in paper mode.
        """
        if not self._is_live:
            raise RuntimeError(
                "get_account_balance requires live mode (set PHEMEX_API_KEY, PHEMEX_API_SECRET)"
            )
        bal = self._exchange.fetch_balance()
        total = bal.get("total") or {}
        try:
            return float(total.get("USDT") or 0.0)
        except (TypeError, ValueError):
            return 0.0

    # ─────────────────────────────────────────────
    # Utility methods
    # ─────────────────────────────────────────────

    def floor_size(self, symbol: str, size: float) -> float:
        """Round size down to exchange's precision (for partial closes)."""
        self._load_markets()
        market = self._exchange.market(f"{symbol}/USDT:USDT")
        precision = market.get("precision", {}).get("amount", 8)
        factor = 10 ** precision
        return math.floor(size * factor) / factor

    def round_size(self, symbol: str, size: float) -> float:
        """Round size to exchange's precision."""
        self._load_markets()
        market = self._exchange.market(f"{symbol}/USDT:USDT")
        precision = market.get("precision", {}).get("amount", 8)
        factor = 10 ** precision
        return round(size * factor) / factor
