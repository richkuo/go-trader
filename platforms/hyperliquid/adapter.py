"""
Hyperliquid Perpetuals Exchange Adapter.

Supports paper (simulated fills using live prices, no credentials) and
live (real orders on Hyperliquid mainnet, wallet credentials required) modes.

Environment variables:
    HYPERLIQUID_SECRET_KEY       — private key for live trading (hex string)
    HYPERLIQUID_ACCOUNT_ADDRESS  — account address (inferred from key if omitted)
    HYPERLIQUID_TESTNET=1        — use testnet instead of mainnet
"""

import math
import os
import sys
import time

MAINNET_URL = "https://api.hyperliquid.xyz"
TESTNET_URL = "https://api.hyperliquid-testnet.xyz"

# SDK imports: defer to avoid module-level ImportError when SDK not installed.
# adapter.py is loaded with platforms/hyperliquid/ directly in sys.path (not platforms/),
# so `from hyperliquid.info import Info` resolves to the SDK package (site-packages),
# not this local directory.
try:
    from hyperliquid.info import Info as _HLInfo
    from hyperliquid.exchange import Exchange as _HLExchange
    _SDK_AVAILABLE = True
except ImportError:
    _HLInfo = None
    _HLExchange = None
    _SDK_AVAILABLE = False


def _round_perps_px(px: float, sz_decimals: int) -> float:
    """Round a perps price to HL's tick rules.

    Two constraints apply simultaneously:
      - At most (MAX_DECIMALS - sz_decimals) decimal places, where
        MAX_DECIMALS=6 for perps. Higher-priced assets (BTC sz_decimals=5)
        therefore allow only 1 decimal of price precision.
      - At most 5 significant figures.

    The earlier fixed-6-decimal round was fine for tiny-priced coins but
    routinely produced HL "price has too many decimals" rejections on
    majors, leaving the position unprotected with the SL slot consumed
    (#421 review point 5).
    """
    if px <= 0:
        return px
    px_decimals = max(0, 6 - sz_decimals)
    log = math.floor(math.log10(abs(px)))
    sig_decimals = max(0, 5 - 1 - int(log))
    decimals = min(px_decimals, sig_decimals)
    return round(px, decimals)


class HyperliquidExchangeAdapter:
    """
    Exchange adapter for Hyperliquid perpetual futures.

    Paper mode:  no credentials needed; uses live Hyperliquid prices for simulation.
    Live mode:   requires HYPERLIQUID_SECRET_KEY; places real market orders.
    """

    def __init__(self):
        if not _SDK_AVAILABLE:
            raise ImportError(
                "hyperliquid-python-sdk not installed. Run: uv sync"
            )

        secret = os.environ.get("HYPERLIQUID_SECRET_KEY", "")
        addr = os.environ.get("HYPERLIQUID_ACCOUNT_ADDRESS", "")
        testnet = os.environ.get("HYPERLIQUID_TESTNET", "") == "1"
        base_url = TESTNET_URL if testnet else MAINNET_URL

        self._info = _HLInfo(base_url=base_url, skip_ws=True)
        self._account_address = addr
        self._exchange = None

        if secret:
            try:
                import eth_account
                wallet = eth_account.Account.from_key(secret)
                account_addr = addr or wallet.address
                self._account_address = account_addr
                self._exchange = _HLExchange(
                    wallet, base_url=base_url, account_address=account_addr
                )
            except Exception as e:
                raise RuntimeError(
                    f"Failed to initialize Hyperliquid Exchange client: {e}"
                )

    @property
    def is_live(self) -> bool:
        """True if Exchange client is initialized (live mode)."""
        return self._exchange is not None

    @property
    def mode(self) -> str:
        """'live' or 'paper'."""
        return "live" if self.is_live else "paper"

    @property
    def name(self) -> str:
        return "hyperliquid"

    # ─────────────────────────────────────────────
    # Market data
    # ─────────────────────────────────────────────

    def get_spot_price(self, symbol: str) -> float:
        """Get current mid price for a coin (e.g. 'BTC')."""
        mids = self._info.all_mids()
        raw = mids.get(symbol, mids.get(symbol + "-PERP", "0"))
        return float(raw or 0)

    def get_ohlcv(self, symbol: str, interval: str = "1h", limit: int = 200) -> list:
        """
        Fetch OHLCV candles from Hyperliquid.

        interval: "1m", "3m", "5m", "15m", "30m", "1h", "2h", "4h", "8h", "12h", "1d"
        Returns list of [timestamp_ms, open, high, low, close, volume].
        """
        interval_ms_map = {
            "1m": 60_000, "3m": 180_000, "5m": 300_000, "15m": 900_000,
            "30m": 1_800_000, "1h": 3_600_000, "2h": 7_200_000,
            "4h": 14_400_000, "8h": 28_800_000, "12h": 43_200_000,
            "1d": 86_400_000, "3d": 259_200_000, "1w": 604_800_000,
        }
        interval_ms = interval_ms_map.get(interval, 3_600_000)
        end_ms = int(time.time() * 1000)
        start_ms = end_ms - interval_ms * limit

        candles = self._info.candles_snapshot(symbol, interval, start_ms, end_ms)
        result = []
        for c in candles:
            # T = close time, t = open time; use T as the candle timestamp
            result.append([
                int(c.get("T", c.get("t", 0))),
                float(c["o"]),
                float(c["h"]),
                float(c["l"]),
                float(c["c"]),
                float(c["v"]),
            ])
        return result

    def get_funding_rate(self, symbol: str) -> float:
        """Get current predicted funding rate for a coin (e.g. 'BTC').

        Returns the raw rate as a float (e.g. 0.0001 = 0.01% per 8h).
        Returns 0.0 if the symbol is not found or on error.
        """
        try:
            data = self._info.meta_and_asset_ctxs()
            universe = data[0]["universe"]
            asset_ctxs = data[1]
            for i, asset in enumerate(universe):
                if asset["name"] == symbol:
                    return float(asset_ctxs[i].get("funding", 0))
            return 0.0
        except Exception:
            return 0.0

    def get_funding_history(self, symbol: str, days: int = 7) -> list:
        """Get historical funding rate snapshots for a coin.

        Args:
            symbol: Coin name (e.g. 'BTC').
            days: Number of days of history to fetch (default 7).

        Returns list of {"rate": float, "time": int} dicts, newest last.
        """
        try:
            start_time = int(time.time() * 1000) - days * 86400 * 1000
            records = self._info.funding_history(symbol, start_time)
            return [
                {"rate": float(r["fundingRate"]), "time": int(r["time"])}
                for r in records
            ]
        except Exception:
            return []

    # ─────────────────────────────────────────────
    # Account data (requires HYPERLIQUID_ACCOUNT_ADDRESS)
    # ─────────────────────────────────────────────

    def get_open_positions(self) -> list:
        """
        Get open perp positions for the configured account.
        Returns list of dicts: {coin, size, entry_price, unrealized_pnl}.
        """
        if not self._account_address:
            return []
        try:
            user_state = self._info.user_state(self._account_address)
            positions = []
            for asset_pos in user_state.get("assetPositions", []):
                pos = asset_pos.get("position", {})
                szi = float(pos.get("szi", 0))
                if szi == 0:
                    continue
                positions.append({
                    "coin": pos.get("coin", ""),
                    "size": szi,
                    "entry_price": float(pos.get("entryPx", 0) or 0),
                    "unrealized_pnl": float(pos.get("unrealizedPnl", 0) or 0),
                })
            return positions
        except Exception:
            return []

    # ─────────────────────────────────────────────
    # Order execution (live mode only)
    # ─────────────────────────────────────────────

    def market_open(self, symbol: str, is_buy: bool, size: float) -> dict:
        """
        Place a market order to open/add to a position.
        Only available in live mode; raises RuntimeError in paper mode.
        Returns raw SDK response dict.
        """
        if not self._exchange:
            raise RuntimeError(
                "market_open requires live mode (set HYPERLIQUID_SECRET_KEY)"
            )
        # Round to asset's tick precision to avoid float_to_wire rounding error
        if symbol not in self._info.asset_to_sz_decimals:
            print(f"[WARN] sz_decimals not found for {symbol}, defaulting to 3", file=sys.stderr)
        sz_decimals = self._info.asset_to_sz_decimals.get(symbol, 3)
        size = round(size, sz_decimals)
        if size <= 0:
            raise ValueError(f"Size rounded to zero for {symbol} (sz_decimals={sz_decimals})")
        return self._exchange.market_open(symbol, is_buy, size, None, 0.01)

    def market_close(self, symbol: str, sz: float | None = None) -> dict:
        """
        Close an open perp position for a symbol (reduce-only).

        When ``sz`` is None, closes the full on-chain position (SDK default).
        When ``sz`` is set, submits a reduce-only market order for that coin
        quantity only — used for shared-wallet per-strategy circuit breakers
        (#356).

        Only available in live mode; raises RuntimeError in paper mode.
        Returns raw SDK response dict.
        """
        if not self._exchange:
            raise RuntimeError(
                "market_close requires live mode (set HYPERLIQUID_SECRET_KEY)"
            )
        return self._exchange.market_close(symbol, sz)

    def place_stop_loss(
        self,
        symbol: str,
        sz: float,
        trigger_px: float,
        is_buy: bool,
        limit_slippage_pct: float = 5.0,
    ) -> dict:
        """Place a reduce-only stop-loss trigger order (#412).

        ``is_buy`` is the direction of the triggered order itself — a long
        position's stop-loss is a SELL (is_buy=False); a short's is a BUY
        (is_buy=True). HL requires a ``limit_px`` as the worst acceptable
        fill price; a market-trigger uses a wide band off the trigger
        (default 5%) so slippage around the stop doesn't reject the fill.

        HL enforces a 10 trigger-orders-per-day cap per account (resets 00:00
        UTC). Cap exhaustion surfaces as an order-status error string (e.g.
        "Too many open trigger orders") in the SDK response; the scheduler
        detects it via isHLTriggerCapRejection and escalates to CRITICAL +
        notifier — no proactive client-side counter is required.
        """
        if not self._exchange:
            raise RuntimeError(
                "place_stop_loss requires live mode (set HYPERLIQUID_SECRET_KEY)"
            )
        if symbol not in self._info.asset_to_sz_decimals:
            print(f"[WARN] sz_decimals not found for {symbol}, defaulting to 3", file=sys.stderr)
        sz_decimals = self._info.asset_to_sz_decimals.get(symbol, 3)
        sz = round(sz, sz_decimals)
        if sz <= 0:
            raise ValueError(f"Size rounded to zero for {symbol} (sz_decimals={sz_decimals})")
        if trigger_px <= 0:
            raise ValueError(f"trigger_px must be > 0, got {trigger_px}")

        slip = max(limit_slippage_pct, 0.0) / 100.0
        if is_buy:
            limit_px = trigger_px * (1.0 + slip)
        else:
            limit_px = trigger_px * (1.0 - slip)
        # HL perps: prices use at most (MAX_DECIMALS - sz_decimals) decimals
        # AND at most 5 significant figures. Fixed-6-decimal rounding here
        # was rejected by HL on high-priced assets like BTC (sz_decimals=5,
        # so px_decimals=1) — the trigger sat resting only because the order
        # got rejected (#421 review point 5).
        limit_px = _round_perps_px(limit_px, sz_decimals)
        trigger_px = _round_perps_px(trigger_px, sz_decimals)

        order_type = {"trigger": {"triggerPx": trigger_px, "isMarket": True, "tpsl": "sl"}}
        return self._exchange.order(
            symbol, is_buy, sz, limit_px, order_type, reduce_only=True
        )

    def cancel_trigger_order(self, symbol: str, oid: int) -> dict:
        """Cancel a resting trigger order by OID (#412)."""
        if not self._exchange:
            raise RuntimeError(
                "cancel_trigger_order requires live mode (set HYPERLIQUID_SECRET_KEY)"
            )
        return self._exchange.cancel(symbol, int(oid))
