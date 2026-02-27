"""
Deribit Options Exchange Adapter — unified interface for options trading.
Uses CCXT deribit with sandbox mode for paper trading.
Supports option chain fetching, Greeks calculation, and paper order execution.
"""

import time
import math
import json
from typing import Optional, Dict, List, Any, Tuple
from datetime import datetime, timedelta
from enum import Enum
from dataclasses import dataclass, field, asdict

import ccxt
import numpy as np

try:
    from scipy.stats import norm
    from scipy.optimize import brentq
    SCIPY_AVAILABLE = True
except ImportError:
    SCIPY_AVAILABLE = False


# ─────────────────────────────────────────────
# Constants
# ─────────────────────────────────────────────

RISK_FREE_RATE = 0.05  # 5% annualized
TRADING_DAYS_PER_YEAR = 365  # crypto is 24/7


# ─────────────────────────────────────────────
# Enums and data classes
# ─────────────────────────────────────────────

class OptionType(str, Enum):
    CALL = "call"
    PUT = "put"


class OptionSide(str, Enum):
    BUY = "buy"
    SELL = "sell"


class OptionOrderStatus(str, Enum):
    PENDING = "pending"
    FILLED = "filled"
    CANCELLED = "cancelled"
    FAILED = "failed"
    EXPIRED = "expired"
    EXERCISED = "exercised"


@dataclass
class Greeks:
    """Option Greeks."""
    delta: float = 0.0
    gamma: float = 0.0
    theta: float = 0.0  # per day
    vega: float = 0.0
    iv: float = 0.0  # implied volatility

    def to_dict(self) -> dict:
        return asdict(self)


@dataclass
class OptionContract:
    """Represents a single option contract."""
    symbol: str
    underlying: str  # BTC, ETH
    strike: float
    expiry: datetime
    option_type: OptionType
    bid: float = 0.0
    ask: float = 0.0
    last: float = 0.0
    volume: float = 0.0
    open_interest: float = 0.0
    greeks: Greeks = field(default_factory=Greeks)
    spot_price: float = 0.0  # underlying spot at time of fetch

    @property
    def mid_price(self) -> float:
        if self.bid > 0 and self.ask > 0:
            return (self.bid + self.ask) / 2
        return self.last or 0.0

    @property
    def dte(self) -> float:
        """Days to expiry."""
        delta = self.expiry - datetime.utcnow()
        return max(delta.total_seconds() / 86400, 0.0)

    @property
    def time_to_expiry(self) -> float:
        """Time to expiry in years."""
        return self.dte / TRADING_DAYS_PER_YEAR

    @property
    def moneyness(self) -> str:
        if self.spot_price <= 0:
            return "unknown"
        if self.option_type == OptionType.CALL:
            if self.strike < self.spot_price * 0.98:
                return "ITM"
            elif self.strike > self.spot_price * 1.02:
                return "OTM"
            return "ATM"
        else:
            if self.strike > self.spot_price * 1.02:
                return "ITM"
            elif self.strike < self.spot_price * 0.98:
                return "OTM"
            return "ATM"

    @property
    def usd_price(self) -> float:
        """Price in USD (Deribit prices options in BTC/ETH)."""
        return self.mid_price * self.spot_price

    def to_dict(self) -> dict:
        return {
            "symbol": self.symbol,
            "underlying": self.underlying,
            "strike": self.strike,
            "expiry": self.expiry.isoformat(),
            "option_type": self.option_type.value,
            "bid": self.bid,
            "ask": self.ask,
            "last": self.last,
            "mid_price": self.mid_price,
            "usd_price": self.usd_price,
            "volume": self.volume,
            "open_interest": self.open_interest,
            "dte": round(self.dte, 1),
            "moneyness": self.moneyness,
            "greeks": self.greeks.to_dict(),
        }


@dataclass
class OptionPosition:
    """Tracks an open options position."""
    id: str
    symbol: str
    underlying: str
    strike: float
    expiry: datetime
    option_type: OptionType
    side: OptionSide
    quantity: float
    entry_price: float  # in underlying currency (BTC/ETH)
    entry_price_usd: float
    entry_time: datetime
    entry_spot: float  # spot price at entry
    current_price: float = 0.0
    current_spot: float = 0.0
    greeks: Greeks = field(default_factory=Greeks)
    leg_group: Optional[str] = None  # for multi-leg strategies

    @property
    def usd_value(self) -> float:
        return self.current_price * self.current_spot * self.quantity

    @property
    def entry_usd_value(self) -> float:
        return self.entry_price_usd * self.quantity

    @property
    def pnl_usd(self) -> float:
        current = self.current_price * self.current_spot * self.quantity
        entry = self.entry_price * self.entry_spot * self.quantity
        if self.side == OptionSide.BUY:
            return current - entry
        else:
            return entry - current

    @property
    def pnl_pct(self) -> float:
        entry = self.entry_price * self.entry_spot * self.quantity
        if entry == 0:
            return 0.0
        return (self.pnl_usd / entry) * 100

    @property
    def dte(self) -> float:
        delta = self.expiry - datetime.utcnow()
        return max(delta.total_seconds() / 86400, 0.0)

    @property
    def is_expired(self) -> bool:
        return datetime.utcnow() >= self.expiry

    def to_dict(self) -> dict:
        return {
            "id": self.id,
            "symbol": self.symbol,
            "underlying": self.underlying,
            "strike": self.strike,
            "expiry": self.expiry.isoformat(),
            "option_type": self.option_type.value,
            "side": self.side.value,
            "quantity": self.quantity,
            "entry_price": self.entry_price,
            "entry_price_usd": self.entry_price_usd,
            "current_price": self.current_price,
            "pnl_usd": round(self.pnl_usd, 2),
            "pnl_pct": round(self.pnl_pct, 2),
            "dte": round(self.dte, 1),
            "greeks": self.greeks.to_dict(),
            "leg_group": self.leg_group,
        }


# ─────────────────────────────────────────────
# Black-Scholes pricing
# ─────────────────────────────────────────────

def _norm_cdf(x: float) -> float:
    """Standard normal CDF (fallback when scipy not available)."""
    if SCIPY_AVAILABLE:
        return float(norm.cdf(x))
    return 0.5 * (1.0 + math.erf(x / math.sqrt(2.0)))


def _norm_pdf(x: float) -> float:
    """Standard normal PDF."""
    if SCIPY_AVAILABLE:
        return float(norm.pdf(x))
    return math.exp(-0.5 * x * x) / math.sqrt(2.0 * math.pi)


def bs_price(S: float, K: float, T: float, r: float, sigma: float,
             option_type: OptionType) -> float:
    """
    Black-Scholes option price.
    S: spot price, K: strike, T: time to expiry (years),
    r: risk-free rate, sigma: volatility
    """
    if T <= 0 or sigma <= 0:
        # At expiry
        if option_type == OptionType.CALL:
            return max(S - K, 0)
        else:
            return max(K - S, 0)

    d1 = (math.log(S / K) + (r + 0.5 * sigma**2) * T) / (sigma * math.sqrt(T))
    d2 = d1 - sigma * math.sqrt(T)

    if option_type == OptionType.CALL:
        return S * _norm_cdf(d1) - K * math.exp(-r * T) * _norm_cdf(d2)
    else:
        return K * math.exp(-r * T) * _norm_cdf(-d2) - S * _norm_cdf(-d1)


def bs_greeks(S: float, K: float, T: float, r: float, sigma: float,
              option_type: OptionType) -> Greeks:
    """Calculate Black-Scholes Greeks."""
    if T <= 0 or sigma <= 0:
        intrinsic = max(S - K, 0) if option_type == OptionType.CALL else max(K - S, 0)
        delta = 1.0 if intrinsic > 0 and option_type == OptionType.CALL else \
                -1.0 if intrinsic > 0 and option_type == OptionType.PUT else 0.0
        return Greeks(delta=delta, gamma=0, theta=0, vega=0, iv=sigma)

    sqrt_T = math.sqrt(T)
    d1 = (math.log(S / K) + (r + 0.5 * sigma**2) * T) / (sigma * sqrt_T)
    d2 = d1 - sigma * sqrt_T

    pdf_d1 = _norm_pdf(d1)

    # Delta
    if option_type == OptionType.CALL:
        delta = _norm_cdf(d1)
    else:
        delta = _norm_cdf(d1) - 1.0

    # Gamma (same for calls and puts)
    gamma = pdf_d1 / (S * sigma * sqrt_T)

    # Theta (per day)
    theta_term1 = -(S * pdf_d1 * sigma) / (2 * sqrt_T)
    if option_type == OptionType.CALL:
        theta = (theta_term1 - r * K * math.exp(-r * T) * _norm_cdf(d2)) / TRADING_DAYS_PER_YEAR
    else:
        theta = (theta_term1 + r * K * math.exp(-r * T) * _norm_cdf(-d2)) / TRADING_DAYS_PER_YEAR

    # Vega (per 1% vol change)
    vega = S * sqrt_T * pdf_d1 / 100

    return Greeks(delta=delta, gamma=gamma, theta=theta, vega=vega, iv=sigma)


def implied_volatility(market_price: float, S: float, K: float, T: float,
                        r: float, option_type: OptionType,
                        tol: float = 1e-6, max_iter: int = 100) -> float:
    """Calculate implied volatility using Brent's method or bisection."""
    if market_price <= 0 or T <= 0:
        return 0.0

    # Intrinsic value check
    if option_type == OptionType.CALL:
        intrinsic = max(S - K * math.exp(-r * T), 0)
    else:
        intrinsic = max(K * math.exp(-r * T) - S, 0)

    if market_price < intrinsic:
        return 0.0

    def objective(sigma):
        return bs_price(S, K, T, r, sigma, option_type) - market_price

    if SCIPY_AVAILABLE:
        try:
            return float(brentq(objective, 0.01, 10.0, xtol=tol, maxiter=max_iter))
        except (ValueError, RuntimeError):
            pass

    # Fallback: bisection
    low, high = 0.01, 10.0
    for _ in range(max_iter):
        mid = (low + high) / 2
        price = bs_price(S, K, T, r, mid, option_type)
        if abs(price - market_price) < tol:
            return mid
        if price > market_price:
            high = mid
        else:
            low = mid
    return (low + high) / 2


# ─────────────────────────────────────────────
# Deribit Options Adapter
# ─────────────────────────────────────────────

class DeribitOptionsAdapter:
    """
    Options exchange adapter using Deribit sandbox via CCXT.
    Paper trading with real market data.
    """

    def __init__(self, api_key: Optional[str] = None, api_secret: Optional[str] = None,
                 sandbox: bool = True, initial_balance_usd: float = 10000.0):
        self.sandbox = sandbox
        self.initial_balance_usd = initial_balance_usd

        config = {
            "sandbox": sandbox,
            "enableRateLimit": True,
        }
        if api_key and api_secret:
            config["apiKey"] = api_key
            config["secret"] = api_secret

        self.exchange = ccxt.deribit(config)

        # Paper trading state
        self._cash_usd = initial_balance_usd
        self._positions: Dict[str, OptionPosition] = {}
        self._trades: List[dict] = []
        self._order_counter = 0
        self._iv_history: Dict[str, List[Tuple[datetime, float]]] = {}

        # Market data cache
        self._markets_loaded = False
        self._option_markets: Dict[str, dict] = {}
        self._spot_cache: Dict[str, Tuple[float, float]] = {}  # symbol -> (price, timestamp)
        self._spot_cache_ttl = 30  # seconds

    @property
    def mode_str(self) -> str:
        return "SANDBOX" if self.sandbox else "LIVE"

    # ─────────────────────────────────────────
    # Market data
    # ─────────────────────────────────────────

    def load_markets(self, force: bool = False):
        """Load and cache option markets from Deribit."""
        if self._markets_loaded and not force:
            return
        markets = self.exchange.load_markets()
        self._option_markets = {
            k: v for k, v in markets.items()
            if v.get("type") == "option" and v.get("active", True)
        }
        self._markets_loaded = True

    def get_spot_price(self, underlying: str) -> float:
        """Get current spot price for underlying (BTC, ETH)."""
        cache_key = underlying
        now = time.time()
        if cache_key in self._spot_cache:
            price, ts = self._spot_cache[cache_key]
            if now - ts < self._spot_cache_ttl:
                return price

        symbol = f"{underlying}/USD:{underlying}"
        try:
            ticker = self.exchange.fetch_ticker(symbol)
            price = ticker["last"]
        except Exception:
            # Fallback: try the perpetual
            try:
                symbol = f"{underlying}/USD:{underlying}-PERPETUAL"
                ticker = self.exchange.fetch_ticker(symbol)
                price = ticker["last"]
            except Exception:
                # Last resort: use index
                symbol = f"{underlying}/USDT"
                ticker = self.exchange.fetch_ticker(f"{underlying}/USD")
                price = ticker["last"]

        self._spot_cache[cache_key] = (price, now)
        return price

    def get_option_chain(self, underlying: str = "BTC",
                          min_dte: float = 0, max_dte: float = 365,
                          max_entries: int = 500) -> List[OptionContract]:
        """
        Fetch option chain for an underlying.
        Returns list of OptionContract objects grouped by expiry and strike.
        """
        self.load_markets()
        spot = self.get_spot_price(underlying)

        chain = []
        count = 0
        now = datetime.utcnow()

        for symbol, market in self._option_markets.items():
            if count >= max_entries:
                break

            # Filter by underlying
            base = market.get("base", "")
            if not base.startswith(underlying):
                continue

            info = market.get("info", {})
            strike = market.get("strike")
            option_type_raw = market.get("optionType")
            expiry_ts = market.get("expiry")

            if not all([strike, option_type_raw, expiry_ts]):
                continue

            option_type_str = str(option_type_raw).lower()
            if option_type_str not in ("call", "put"):
                continue

            expiry = datetime.utcfromtimestamp(expiry_ts / 1000) if expiry_ts > 1e10 else datetime.utcfromtimestamp(expiry_ts)
            dte = (expiry - now).total_seconds() / 86400

            if dte < min_dte or dte > max_dte:
                continue

            opt_type = OptionType.CALL if option_type_str == "call" else OptionType.PUT

            contract = OptionContract(
                symbol=symbol,
                underlying=underlying,
                strike=float(strike),
                expiry=expiry,
                option_type=opt_type,
                spot_price=spot,
            )
            chain.append(contract)
            count += 1

        return chain

    def get_option_ticker(self, symbol: str) -> dict:
        """Fetch live ticker for a specific option."""
        return self.exchange.fetch_ticker(symbol)

    def enrich_contract(self, contract: OptionContract) -> OptionContract:
        """Fetch live pricing and calculate Greeks for a contract."""
        try:
            ticker = self.get_option_ticker(contract.symbol)
            contract.bid = ticker.get("bid") or 0.0
            contract.ask = ticker.get("ask") or 0.0
            contract.last = ticker.get("last") or 0.0
            contract.volume = ticker.get("baseVolume") or 0.0
            contract.open_interest = ticker.get("info", {}).get("open_interest", 0)
            contract.spot_price = self.get_spot_price(contract.underlying)

            # Calculate IV and Greeks
            mid = contract.mid_price
            if mid > 0 and contract.spot_price > 0 and contract.time_to_expiry > 0:
                # Deribit prices in underlying, BS expects USD
                market_price_usd = mid * contract.spot_price
                iv = implied_volatility(
                    market_price_usd, contract.spot_price, contract.strike,
                    contract.time_to_expiry, RISK_FREE_RATE, contract.option_type
                )
                contract.greeks = bs_greeks(
                    contract.spot_price, contract.strike,
                    contract.time_to_expiry, RISK_FREE_RATE, iv,
                    contract.option_type
                )

                # Track IV history
                key = f"{contract.underlying}_{contract.strike}_{contract.option_type.value}"
                if key not in self._iv_history:
                    self._iv_history[key] = []
                self._iv_history[key].append((datetime.utcnow(), iv))
                # Keep last 90 days
                cutoff = datetime.utcnow() - timedelta(days=90)
                self._iv_history[key] = [
                    (t, v) for t, v in self._iv_history[key] if t > cutoff
                ]

        except Exception as e:
            pass  # Ticker fetch can fail for illiquid options

        return contract

    def find_options(self, underlying: str, option_type: OptionType,
                     min_dte: float = 7, max_dte: float = 60,
                     moneyness: str = "ATM",
                     max_results: int = 10) -> List[OptionContract]:
        """
        Find options matching criteria.
        moneyness: 'ATM', 'OTM', 'ITM', or 'any'
        """
        chain = self.get_option_chain(underlying, min_dte=min_dte, max_dte=max_dte)
        filtered = [c for c in chain if c.option_type == option_type]

        spot = self.get_spot_price(underlying)

        if moneyness == "ATM":
            # Sort by distance from spot
            filtered.sort(key=lambda c: abs(c.strike - spot))
        elif moneyness == "OTM":
            if option_type == OptionType.CALL:
                filtered = [c for c in filtered if c.strike > spot]
                filtered.sort(key=lambda c: c.strike)
            else:
                filtered = [c for c in filtered if c.strike < spot]
                filtered.sort(key=lambda c: -c.strike)
        elif moneyness == "ITM":
            if option_type == OptionType.CALL:
                filtered = [c for c in filtered if c.strike < spot]
                filtered.sort(key=lambda c: -c.strike)
            else:
                filtered = [c for c in filtered if c.strike > spot]
                filtered.sort(key=lambda c: c.strike)

        return filtered[:max_results]

    def get_atm_iv(self, underlying: str, dte_target: float = 30) -> float:
        """Get ATM implied volatility for an underlying at target DTE."""
        calls = self.find_options(underlying, OptionType.CALL,
                                   min_dte=dte_target - 10, max_dte=dte_target + 10,
                                   moneyness="ATM", max_results=3)
        if not calls:
            return 0.0

        for c in calls:
            c = self.enrich_contract(c)
            if c.greeks.iv > 0:
                return c.greeks.iv
        return 0.0

    def get_iv_rank(self, underlying: str, lookback_days: int = 60) -> float:
        """
        Calculate IV rank: percentile of current IV over lookback period.
        Returns 0-100.
        """
        current_iv = self.get_atm_iv(underlying)
        if current_iv <= 0:
            return 50.0  # neutral default

        # Collect IV history across all ATM-ish options
        all_ivs = []
        for key, history in self._iv_history.items():
            if key.startswith(underlying):
                for ts, iv in history:
                    if (datetime.utcnow() - ts).days <= lookback_days:
                        all_ivs.append(iv)

        if len(all_ivs) < 5:
            return 50.0

        below = sum(1 for iv in all_ivs if iv < current_iv)
        return (below / len(all_ivs)) * 100

    # ─────────────────────────────────────────
    # Paper trading
    # ─────────────────────────────────────────

    def _next_order_id(self) -> str:
        self._order_counter += 1
        return f"opt_{self._order_counter}"

    def buy_option(self, contract: OptionContract, quantity: float = 1.0,
                   leg_group: Optional[str] = None) -> Optional[OptionPosition]:
        """Buy an option (paper mode)."""
        contract = self.enrich_contract(contract)
        price = contract.ask if contract.ask > 0 else contract.mid_price
        if price <= 0:
            return None

        # Cost in USD
        cost_usd = price * contract.spot_price * quantity
        commission = cost_usd * 0.0003  # Deribit taker fee ~0.03%

        if cost_usd + commission > self._cash_usd:
            return None

        self._cash_usd -= (cost_usd + commission)

        pos = OptionPosition(
            id=self._next_order_id(),
            symbol=contract.symbol,
            underlying=contract.underlying,
            strike=contract.strike,
            expiry=contract.expiry,
            option_type=contract.option_type,
            side=OptionSide.BUY,
            quantity=quantity,
            entry_price=price,
            entry_price_usd=price * contract.spot_price,
            entry_time=datetime.utcnow(),
            entry_spot=contract.spot_price,
            current_price=price,
            current_spot=contract.spot_price,
            greeks=contract.greeks,
            leg_group=leg_group,
        )
        self._positions[pos.id] = pos
        self._trades.append({
            "action": "BUY",
            "position_id": pos.id,
            "symbol": contract.symbol,
            "option_type": contract.option_type.value,
            "strike": contract.strike,
            "price": price,
            "price_usd": cost_usd,
            "quantity": quantity,
            "commission": commission,
            "timestamp": datetime.utcnow().isoformat(),
        })
        return pos

    def sell_option(self, contract: OptionContract, quantity: float = 1.0,
                    leg_group: Optional[str] = None) -> Optional[OptionPosition]:
        """Sell (write) an option (paper mode). Collects premium."""
        contract = self.enrich_contract(contract)
        price = contract.bid if contract.bid > 0 else contract.mid_price
        if price <= 0:
            return None

        # Premium received in USD
        premium_usd = price * contract.spot_price * quantity
        commission = premium_usd * 0.0003

        self._cash_usd += (premium_usd - commission)

        pos = OptionPosition(
            id=self._next_order_id(),
            symbol=contract.symbol,
            underlying=contract.underlying,
            strike=contract.strike,
            expiry=contract.expiry,
            option_type=contract.option_type,
            side=OptionSide.SELL,
            quantity=quantity,
            entry_price=price,
            entry_price_usd=price * contract.spot_price,
            entry_time=datetime.utcnow(),
            entry_spot=contract.spot_price,
            current_price=price,
            current_spot=contract.spot_price,
            greeks=contract.greeks,
            leg_group=leg_group,
        )
        self._positions[pos.id] = pos
        self._trades.append({
            "action": "SELL",
            "position_id": pos.id,
            "symbol": contract.symbol,
            "option_type": contract.option_type.value,
            "strike": contract.strike,
            "price": price,
            "price_usd": premium_usd,
            "quantity": quantity,
            "commission": commission,
            "timestamp": datetime.utcnow().isoformat(),
        })
        return pos

    def close_position(self, position_id: str) -> Optional[dict]:
        """Close an open position at current market price."""
        pos = self._positions.get(position_id)
        if not pos:
            return None

        try:
            ticker = self.get_option_ticker(pos.symbol)
            spot = self.get_spot_price(pos.underlying)
        except Exception:
            return None

        if pos.side == OptionSide.BUY:
            # Selling to close — use bid
            close_price = ticker.get("bid") or ticker.get("last") or 0
            proceeds_usd = close_price * spot * pos.quantity
            commission = proceeds_usd * 0.0003
            self._cash_usd += (proceeds_usd - commission)
        else:
            # Buying to close — use ask
            close_price = ticker.get("ask") or ticker.get("last") or 0
            cost_usd = close_price * spot * pos.quantity
            commission = cost_usd * 0.0003
            self._cash_usd -= (cost_usd + commission)

        pnl = pos.pnl_usd
        trade = {
            "action": "CLOSE",
            "position_id": position_id,
            "symbol": pos.symbol,
            "close_price": close_price,
            "pnl_usd": pnl,
            "commission": commission,
            "timestamp": datetime.utcnow().isoformat(),
        }
        self._trades.append(trade)
        del self._positions[position_id]
        return trade

    def close_leg_group(self, leg_group: str) -> List[dict]:
        """Close all positions in a leg group (for spreads)."""
        results = []
        ids_to_close = [pid for pid, p in self._positions.items() if p.leg_group == leg_group]
        for pid in ids_to_close:
            result = self.close_position(pid)
            if result:
                results.append(result)
        return results

    def handle_expiries(self):
        """Handle expired options: exercise ITM, expire OTM."""
        expired_ids = [pid for pid, p in self._positions.items() if p.is_expired]

        for pid in expired_ids:
            pos = self._positions[pid]
            spot = self.get_spot_price(pos.underlying)

            # Calculate intrinsic value
            if pos.option_type == OptionType.CALL:
                intrinsic = max(spot - pos.strike, 0)
            else:
                intrinsic = max(pos.strike - spot, 0)

            if intrinsic > 0:
                # ITM — exercise
                settlement_usd = intrinsic * pos.quantity
                if pos.side == OptionSide.BUY:
                    self._cash_usd += settlement_usd
                else:
                    self._cash_usd -= settlement_usd

                self._trades.append({
                    "action": "EXERCISED",
                    "position_id": pid,
                    "symbol": pos.symbol,
                    "settlement_usd": settlement_usd,
                    "intrinsic": intrinsic,
                    "timestamp": datetime.utcnow().isoformat(),
                })
            else:
                # OTM — expires worthless
                self._trades.append({
                    "action": "EXPIRED",
                    "position_id": pid,
                    "symbol": pos.symbol,
                    "timestamp": datetime.utcnow().isoformat(),
                })

            del self._positions[pid]

    def update_positions(self):
        """Update current prices and Greeks for all open positions."""
        for pos in self._positions.values():
            try:
                ticker = self.get_option_ticker(pos.symbol)
                pos.current_price = ticker.get("last") or ticker.get("bid") or 0
                pos.current_spot = self.get_spot_price(pos.underlying)

                if pos.current_price > 0 and pos.current_spot > 0:
                    T = max((pos.expiry - datetime.utcnow()).total_seconds() / (86400 * TRADING_DAYS_PER_YEAR), 0)
                    market_usd = pos.current_price * pos.current_spot
                    if T > 0:
                        iv = implied_volatility(
                            market_usd, pos.current_spot, pos.strike,
                            T, RISK_FREE_RATE, pos.option_type
                        )
                        pos.greeks = bs_greeks(
                            pos.current_spot, pos.strike, T,
                            RISK_FREE_RATE, iv, pos.option_type
                        )
            except Exception:
                pass

    # ─────────────────────────────────────────
    # Multi-leg strategies
    # ─────────────────────────────────────────

    def open_spread(self, buy_contract: OptionContract, sell_contract: OptionContract,
                    quantity: float = 1.0, name: str = "spread") -> Optional[str]:
        """Open a two-leg spread (buy one, sell one)."""
        group = f"{name}_{self._order_counter + 1}"
        long = self.buy_option(buy_contract, quantity, leg_group=group)
        short = self.sell_option(sell_contract, quantity, leg_group=group)
        if long and short:
            return group
        return None

    def open_straddle(self, underlying: str, dte_target: float = 30,
                      side: OptionSide = OptionSide.BUY,
                      quantity: float = 1.0) -> Optional[str]:
        """Open a straddle (same strike call+put)."""
        calls = self.find_options(underlying, OptionType.CALL,
                                   min_dte=dte_target - 7, max_dte=dte_target + 7,
                                   moneyness="ATM", max_results=1)
        puts = self.find_options(underlying, OptionType.PUT,
                                  min_dte=dte_target - 7, max_dte=dte_target + 7,
                                  moneyness="ATM", max_results=1)
        if not calls or not puts:
            return None

        group = f"straddle_{self._order_counter + 1}"
        fn = self.buy_option if side == OptionSide.BUY else self.sell_option
        leg1 = fn(calls[0], quantity, leg_group=group)
        leg2 = fn(puts[0], quantity, leg_group=group)
        if leg1 and leg2:
            return group
        return None

    def open_strangle(self, underlying: str, dte_target: float = 30,
                      otm_pct: float = 0.05,
                      side: OptionSide = OptionSide.BUY,
                      quantity: float = 1.0) -> Optional[str]:
        """Open a strangle (OTM call + OTM put)."""
        calls = self.find_options(underlying, OptionType.CALL,
                                   min_dte=dte_target - 7, max_dte=dte_target + 7,
                                   moneyness="OTM", max_results=5)
        puts = self.find_options(underlying, OptionType.PUT,
                                  min_dte=dte_target - 7, max_dte=dte_target + 7,
                                  moneyness="OTM", max_results=5)
        if not calls or not puts:
            return None

        spot = self.get_spot_price(underlying)
        # Pick strikes ~otm_pct away
        call_contract = min(calls, key=lambda c: abs(c.strike - spot * (1 + otm_pct)))
        put_contract = min(puts, key=lambda c: abs(c.strike - spot * (1 - otm_pct)))

        group = f"strangle_{self._order_counter + 1}"
        fn = self.buy_option if side == OptionSide.BUY else self.sell_option
        leg1 = fn(call_contract, quantity, leg_group=group)
        leg2 = fn(put_contract, quantity, leg_group=group)
        if leg1 and leg2:
            return group
        return None

    # ─────────────────────────────────────────
    # Portfolio
    # ─────────────────────────────────────────

    def get_cash(self) -> float:
        return self._cash_usd

    def get_positions(self) -> Dict[str, OptionPosition]:
        return dict(self._positions)

    def get_open_position_count(self) -> int:
        return len(self._positions)

    def get_portfolio_value(self) -> float:
        """Total portfolio value: cash + positions mark-to-market."""
        total = self._cash_usd
        for pos in self._positions.values():
            if pos.side == OptionSide.BUY:
                total += pos.current_price * pos.current_spot * pos.quantity
            else:
                # Short: liability = current value
                total -= pos.current_price * pos.current_spot * pos.quantity
        return total

    def get_portfolio_greeks(self) -> Greeks:
        """Aggregate portfolio Greeks."""
        net = Greeks()
        for pos in self._positions.values():
            sign = 1.0 if pos.side == OptionSide.BUY else -1.0
            net.delta += pos.greeks.delta * pos.quantity * sign
            net.gamma += pos.greeks.gamma * pos.quantity * sign
            net.theta += pos.greeks.theta * pos.quantity * sign
            net.vega += pos.greeks.vega * pos.quantity * sign
        return net

    def get_trade_history(self) -> List[dict]:
        return list(self._trades)

    def get_premium_at_risk(self) -> float:
        """Total premium at risk (long positions only)."""
        total = 0.0
        for pos in self._positions.values():
            if pos.side == OptionSide.BUY:
                total += pos.entry_price_usd * pos.quantity
        return total


if __name__ == "__main__":
    adapter = DeribitOptionsAdapter(sandbox=True)
    print(f"Mode: {adapter.mode_str}")
    print(f"Cash: ${adapter.get_cash():,.2f}")

    try:
        spot = adapter.get_spot_price("BTC")
        print(f"BTC spot: ${spot:,.2f}")

        chain = adapter.get_option_chain("BTC", min_dte=7, max_dte=90)
        print(f"Option chain entries: {len(chain)}")

        if chain:
            sample = chain[0]
            print(f"Sample: {sample.symbol} | Strike: {sample.strike} | "
                  f"Type: {sample.option_type.value} | DTE: {sample.dte:.0f}")
    except Exception as e:
        print(f"Connection test: {e}")


# ─────────────────────────────────────────────
# ExchangeAdapter implementation for check_options.py
# ─────────────────────────────────────────────

import sys as _sys
import os as _os
_sys.path.insert(0, _os.path.join(_os.path.dirname(_os.path.abspath(__file__)), '..', '..', 'shared_tools'))

try:
    from pricing import bs_price_and_greeks as _bs_price_and_greeks
except ImportError:
    _bs_price_and_greeks = None


class DeribitExchangeAdapter:
    """
    Lightweight ExchangeAdapter for use by shared_scripts/check_options.py.
    Wraps platforms/deribit/utils.py for live expiry/strike/quote lookups.
    Falls back to Black-Scholes when live Deribit data is unavailable.
    """

    @property
    def name(self) -> str:
        return "deribit"

    def get_spot_price(self, underlying: str) -> float:
        """Fetch spot price from Binance US via ccxt."""
        try:
            exchange = ccxt.binanceus({"enableRateLimit": True})
            ticker = exchange.fetch_ticker(f"{underlying}/USDT")
            return float(ticker.get("last") or 0)
        except Exception:
            return 0.0

    def get_vol_metrics(self, underlying: str) -> Tuple[float, float]:
        """Compute annualized vol and IV rank from daily OHLCV."""
        import math as _math
        try:
            exchange = ccxt.binanceus({"enableRateLimit": True})
            ohlcv = exchange.fetch_ohlcv(f"{underlying}/USDT", "1d", limit=90)
            if not ohlcv or len(ohlcv) < 15:
                return 0.60, 50.0
            closes = [c[4] for c in ohlcv]
            returns = [_math.log(closes[i] / closes[i - 1]) for i in range(1, len(closes))]
            if len(returns) < 14:
                return 0.60, 50.0
            w = 14
            mean = sum(returns[-w:]) / w
            variance = sum((r - mean) ** 2 for r in returns[-w:]) / w
            vol = _math.sqrt(variance) * _math.sqrt(365)
            hvs = []
            for i in range(len(returns) - w + 1):
                chunk = returns[i:i + w]
                m = sum(chunk) / w
                v = sum((r - m) ** 2 for r in chunk) / w
                hvs.append(_math.sqrt(v) * _math.sqrt(365) * 100)
            current_hv = vol * 100
            hv_min, hv_max = min(hvs), max(hvs)
            iv_rank = (current_hv - hv_min) / (hv_max - hv_min) * 100 if hv_max > hv_min else 50.0
            return round(vol, 4), round(min(max(iv_rank, 0.0), 100.0), 1)
        except Exception:
            return 0.60, 50.0

    def get_real_expiry(self, underlying: str, target_dte: int) -> Tuple[str, int]:
        """Return closest available Deribit expiry to target_dte."""
        try:
            from utils import find_closest_expiry
            result = find_closest_expiry(underlying, target_dte)
            if result:
                return result
        except Exception:
            pass
        from datetime import datetime, timezone, timedelta
        expiry_dt = datetime.now(timezone.utc) + timedelta(days=target_dte)
        return expiry_dt.strftime("%Y-%m-%d"), target_dte

    def get_real_strike(self, underlying: str, expiry: str,
                        option_type: str, target_strike: float) -> float:
        """Return closest available Deribit strike to target_strike."""
        try:
            from utils import find_closest_strike
            result = find_closest_strike(underlying, expiry, option_type, target_strike)
            if result:
                return result
        except Exception:
            pass
        # Fallback: round to nearest 1000 (BTC) or 50 (ETH)
        if underlying.upper() == "BTC":
            return round(target_strike, -3)
        return round(target_strike / 50) * 50

    def get_premium_and_greeks(self, underlying: str, option_type: str,
                                strike: float, expiry: str, dte: float,
                                spot: float, vol: float) -> Tuple[float, float, dict]:
        """Get live quote from Deribit; fall back to Black-Scholes."""
        try:
            from utils import get_live_quote
            quote = get_live_quote(underlying, option_type, strike, expiry)
            if quote:
                mark = quote["mark_price"]
                return mark, round(mark * spot, 2), quote["greeks"]
        except Exception:
            pass
        # Fallback: Black-Scholes
        if _bs_price_and_greeks is not None and vol > 0:
            price_usd, greeks = _bs_price_and_greeks(spot, strike, dte, vol, option_type=option_type)
            mark_pct = (price_usd / spot) if spot > 0 else 0.0
            return round(mark_pct, 6), round(price_usd, 2), greeks
        fallback_pct = 0.05
        return fallback_pct, round(fallback_pct * spot, 2), {"delta": 0.5, "gamma": 0.0, "theta": 0.0, "vega": 0.0}
