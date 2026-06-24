"""
OKX Exchange Adapter — unified interface for spot, perpetual swaps, and options.
Uses CCXT for all API interactions.

Supports paper (public API only, no credentials) and
live (real orders on OKX, API credentials required) modes.

Environment variables:
    OKX_API_KEY        — API key for live trading
    OKX_API_SECRET     — API secret for live trading
    OKX_PASSPHRASE     — API passphrase for live trading
    OKX_SANDBOX=1      — use OKX demo trading environment
"""

import os
import sys
import math
import time
from typing import Tuple

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), '..', '..', 'shared_tools'))

import ccxt


def _bill_float(value) -> float:
    """Parse an OKX bill numeric field; missing/blank/garbage → 0.0."""
    try:
        return float(value)
    except (TypeError, ValueError):
        return 0.0


def _okx_usdt_cash_balance(info):
    """Pull the USDT ``cashBal`` from a raw OKX fetch_balance ``info`` payload
    (``data[0].details[]``). Returns a float, or None when the field is absent
    or unparseable. Pure — unit-tested without a live exchange."""
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
    """Normalize one ccxt fetch_ledger entry into the #1105 journal bill shape.

    Pulls the raw OKX bill from ``entry["info"]`` (authoritative field names —
    billId/balChg/etc.) and falls back to the ccxt-unified ``timestamp`` for the
    millisecond time when the raw ``ts`` is absent. Pure — unit-tested without a
    live exchange.
    """
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
    """
    Exchange adapter for OKX — spot, perpetual swaps, and options.

    Paper mode:  no credentials needed; uses live OKX prices for simulation.
    Live mode:   requires OKX_API_KEY, OKX_API_SECRET, OKX_PASSPHRASE.
    """

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
        """True if API credentials are provided (live mode)."""
        return self._is_live

    @property
    def mode(self) -> str:
        """'live' or 'paper'."""
        return "live" if self.is_live else "paper"

    @property
    def name(self) -> str:
        return "okx"

    # ─────────────────────────────────────────────
    # Market data
    # ─────────────────────────────────────────────

    def _load_markets(self):
        """Load and cache markets from OKX."""
        if not self._markets_loaded:
            self._exchange.load_markets()
            self._markets_loaded = True

    def get_spot_price(self, symbol: str) -> float:
        """Get current spot price for a coin (e.g. 'BTC')."""
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
        try:
            ticker = self._exchange.fetch_ticker(f"{symbol}/USDT:USDT")
            price = ticker.get("last") or 0
            if price and price > 0:
                return float(price)
        except Exception:
            pass
        return 0.0

    def get_ohlcv(self, symbol: str, interval: str = "1h", limit: int = 200) -> list:
        """
        Fetch OHLCV candles from OKX.

        interval: "1m", "5m", "15m", "30m", "1h", "2h", "4h", "1d", etc.
        Returns list of [timestamp_ms, open, high, low, close, volume].
        """
        pair = f"{symbol}/USDT"
        try:
            candles = self._exchange.fetch_ohlcv(pair, interval, limit=limit)
            return candles  # ccxt already returns [ts, o, h, l, c, v]
        except Exception:
            return []

    def get_ohlcv_closes(self, symbol: str, interval: str = "1h", limit: int = 200) -> list:
        """Fetch OHLCV and return just close prices (for HTF filter compatibility)."""
        candles = self.get_ohlcv(symbol, interval, limit)
        return [c[4] for c in candles] if candles else []

    def get_perp_ohlcv(self, symbol: str, interval: str = "1h", limit: int = 200) -> list:
        """Fetch OHLCV candles for perpetual swap (USDT-margined)."""
        pair = f"{symbol}/USDT:USDT"
        try:
            candles = self._exchange.fetch_ohlcv(pair, interval, limit=limit)
            return candles
        except Exception:
            return []

    def get_funding_rate(self, symbol: str) -> float:
        """Get current predicted funding rate for a perpetual swap.

        Returns the raw rate as a float (e.g. 0.0001 = 0.01% per 8h).
        """
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
                "fetch_open_positions requires live mode (set OKX_API_KEY, OKX_API_SECRET, OKX_PASSPHRASE)"
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
        """
        Close an open perpetual swap position for a symbol (reduce-only).

        When ``sz`` is None, closes the full on-chain contracts for the
        position (portfolio kill switch / sole-owner circuit breakers).
        When ``sz`` is set, submits a reduce-only market order for that
        contract quantity only — used for shared-wallet per-strategy
        circuit breakers (#360). The caller is responsible for sizing;
        OKX enforces reduceOnly=True on the order itself so an oversized
        request cannot flip the position.

        Only available in live mode; raises RuntimeError in paper mode.
        """
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
        """Return the total USDT-denominated account VALUE for shared-wallet
        aggregation (#360 phase 2 — unlocks multi-strategy OKX portfolio value
        correctness).

        Returns ccxt's ``balance["total"]["USDT"]``, which ccxt's OKX driver
        maps from the account's equity field (``eq``) — i.e. cash balance PLUS
        unrealized P&L on open positions for the unified/cross margin account.

        #918 RELIES on this being equity-inclusive: the shared-wallet
        reconciler computes ``base = accountBalance - Σ unrealizedPnL`` and
        redistributes the base by capital weight, attributing each position's
        unrealized P&L to its owning strategy. If this value ever excluded
        unrealized P&L the per-member equity rows would be skewed by the
        owners' P&L (the member SUM still reconciles to this number, so the
        drift alarm stays correct, but the split would misreport per strategy).
        Verify against a live OKX account if the ccxt mapping changes.

        Only available in live mode; raises RuntimeError in paper mode.
        """
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
        """Return a COHERENT ``(equity, unrealized_pnl)`` snapshot from ONE
        ``fetch_balance`` call, for the #1105 cash-flow journal.

        ``equity`` is the USDT account value (ccxt ``total["USDT"]`` == OKX
        ``eq`` — the same field ``get_account_balance`` returns and the #918
        capital-weight split reconciles). ``unrealized_pnl`` is derived as
        ``eq - cashBal`` for USDT from the SAME response, so eq and uPnL are one
        atomic snapshot: in the journal's equity equation the uPnL term then
        cancels exactly against ``eq``, leaving residual drift = settled-cash
        reconstruction error only. Pairing eq with a uPnL sampled from a SEPARATE
        ``fetch_positions`` call (a different instant) would inject intra-cycle
        uPnL jitter — dollars on a leveraged position — into the shadow drift,
        the very signal Phase 3b must evaluate.

        Falls back to uPnL ``0.0`` when the USDT ``cashBal`` is absent/unparseable
        (the shadow drift then conservatively carries uPnL — visible, never a
        false "clean"). Only available in live mode; raises in paper mode.
        """
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
        """Fetch OKX account bills (settled cash-flow events) since ``since_ms``
        for the #1105 exchange-sourced cash-flow journal (shadow phase).

        Every OKX balance change is an account bill carrying ``balChg`` (the
        signed change to the settled cash balance), so this single feed is the
        COMPLETE settled-cash-flow source — trade PnL, fees, funding, transfers,
        deposits and withdrawals are all bills. ``balChg`` already nets every
        component, so the consumer needs no per-fill fee arithmetic; ``pnl`` /
        ``fee`` are returned as attribution metadata only.

        Returns ``(bills, capped)`` where ``bills`` is oldest-first, each a dict:
        ``{"bill_id","ts_ms","ccy","type","sub_type","bal_chg","pnl","fee",
        "inst_id","trade_id"}`` sourced from the raw OKX bill
        (ccxt ``fetch_ledger`` entry ``info``). ``capped`` is True when the
        ``max_bills`` safety cap was hit before the feed was exhausted — the Go
        journal then treats that cycle as not-usable but still advances its
        cursor past the contiguous oldest prefix.

        Horizon: ccxt ``fetch_ledger`` reads OKX ``/account/bills`` (~7 days). In
        steady per-cycle operation the window is ample; after a multi-day outage
        older bills fall outside it and surface as journal drift in the shadow
        log (visible to the operator before any Phase-3b alarm flip).

        Ordering assumption: this pages FORWARD by timestamp and relies on ccxt's
        unified ``fetch_ledger`` returning entries ascending-from-``since`` (ccxt
        ``parse_ledger`` sorts by timestamp). That contract — and that the
        millisecond ``ts`` matches the live feed — is offline-unverifiable; confirm
        the settled-Σ tracks eq to ~0 over a multi-cycle live window before any
        Phase-3b alarm flip.

        Only available in live mode; raises RuntimeError in paper mode.
        """
        if not self._is_live:
            raise RuntimeError(
                "get_account_bills requires live mode (set OKX_API_KEY, OKX_API_SECRET, OKX_PASSPHRASE)"
            )
        collected = {}
        cursor = int(since_ms or 0)
        capped = False
        # Page FORWARD from the cursor, advancing the `since` watermark with a
        # one-millisecond OVERLAP (cursor = page's last ts, NOT last_ts + 1) so a
        # bill that shares the boundary millisecond but fell beyond the page cut
        # is re-fetched on the next page — the `collected` dedup absorbs the
        # re-read. Advancing past last_ts would permanently skip such a same-ms
        # bill (a standing offset in the settled sum, not a transient miss). The
        # page-loop bound caps total iterations.
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
                break  # short page → feed exhausted
            page_last_ts = max((int(e.get("timestamp") or 0) for e in page), default=cursor)
            if page_last_ts <= cursor and added == 0:
                # A FULL page that neither advanced the timestamp nor yielded any
                # new bill: a single-millisecond block larger than page_limit, which
                # timestamp paging cannot step through without dropping bills. Fail
                # closed — the Go journal treats `capped` as not-usable for the
                # cycle rather than silently advancing past the undelivered tail.
                capped = True
                break
            cursor = page_last_ts  # overlap on the boundary ms (dedup absorbs it)
        bills = sorted(collected.values(), key=lambda b: b["ts_ms"])
        if len(bills) > max_bills:
            bills = bills[:max_bills]
        return bills, capped

    # ─────────────────────────────────────────────
    # Options Protocol methods
    # ─────────────────────────────────────────────

    def get_vol_metrics(self, underlying: str) -> Tuple[float, float]:
        """Compute 14-day historical vol and IV rank from daily OHLCV."""
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
        """Return options expiry closest to target_dte.

        Returns (expiry_str: "YYYY-MM-DD", actual_dte: int).
        """
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
            # Fallback: synthetic expiry
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
        """Return strike closest to target_strike for given underlying/expiry/type."""
        self._load_markets()
        from datetime import datetime, timezone

        exp_dt = datetime.strptime(expiry, "%Y-%m-%d").replace(tzinfo=timezone.utc)
        exp_start = int(exp_dt.timestamp() * 1000)
        exp_end = exp_start + 86400 * 1000  # within same day

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
            # Fallback: round to nearest 1000 for BTC, 100 for ETH
            if underlying.upper() == "BTC":
                return round(target_strike / 1000) * 1000
            elif underlying.upper() == "ETH":
                return round(target_strike / 100) * 100
            return round(target_strike / 50) * 50

        return min(strikes, key=lambda s: abs(s - target_strike))

    def get_premium_and_greeks(self, underlying: str, option_type: str,
                                strike: float, expiry: str, dte: float,
                                spot: float, vol: float) -> Tuple[float, float, dict]:
        """Estimate premium and Greeks.

        Returns (premium_pct, premium_usd, greeks_dict).
        Tries live OKX quote first, falls back to Black-Scholes.
        """
        # Try live quote
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
                            premium_usd = float(mark) * spot  # OKX options priced in base currency
                            premium_pct = float(mark)
                            greeks = {
                                "delta": ticker.get("info", {}).get("delta", 0),
                                "gamma": ticker.get("info", {}).get("gamma", 0),
                                "theta": ticker.get("info", {}).get("theta", 0),
                                "vega": ticker.get("info", {}).get("vega", 0),
                            }
                            # Convert to floats
                            greeks = {k: float(v or 0) for k, v in greeks.items()}
                            return premium_pct, premium_usd, greeks
        except Exception:
            pass

        # Fallback: Black-Scholes
        try:
            from pricing import bs_price_and_greeks
            premium_usd, greeks = bs_price_and_greeks(spot, strike, dte, vol, option_type)
            premium_pct = premium_usd / spot if spot > 0 else 0
            return round(premium_pct, 6), round(premium_usd, 2), greeks
        except Exception:
            return 0.0, 0.0, {"delta": 0, "gamma": 0, "theta": 0, "vega": 0}
