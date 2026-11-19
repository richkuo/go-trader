from dataclasses import asdict, dataclass, field
from datetime import datetime, timezone
from enum import Enum
from typing import Any, Dict, List, Optional, Protocol, runtime_checkable

TRADING_DAYS_PER_YEAR = 365


class OptionType(str, Enum):
    CALL = "call"
    PUT = "put"


class OptionSide(str, Enum):
    BUY = "buy"
    SELL = "sell"


@dataclass
class Greeks:
    delta: float = 0.0
    gamma: float = 0.0
    theta: float = 0.0
    vega: float = 0.0
    iv: float = 0.0

    def to_dict(self) -> dict:
        return asdict(self)


@dataclass
class OptionContract:
    symbol: str
    underlying: str
    strike: float
    expiry: datetime
    option_type: OptionType
    bid: float = 0.0
    ask: float = 0.0
    last: float = 0.0
    volume: float = 0.0
    open_interest: float = 0.0
    greeks: Greeks = field(default_factory=Greeks)
    spot_price: float = 0.0

    @property
    def mid_price(self) -> float:
        if self.bid > 0 and self.ask > 0:
            return (self.bid + self.ask) / 2
        return self.last or 0.0

    @property
    def dte(self) -> float:
        now = datetime.now(timezone.utc) if self.expiry.tzinfo else datetime.utcnow()
        delta = self.expiry - now
        return max(delta.total_seconds() / 86400, 0.0)

    @property
    def time_to_expiry(self) -> float:
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
    id: str
    symbol: str
    underlying: str
    strike: float
    expiry: datetime
    option_type: OptionType
    side: OptionSide
    quantity: float
    entry_price: float
    entry_price_usd: float
    entry_time: datetime
    entry_spot: float
    current_price: float = 0.0
    current_spot: float = 0.0
    greeks: Greeks = field(default_factory=Greeks)
    leg_group: Optional[str] = None

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
        now = datetime.now(timezone.utc) if self.expiry.tzinfo else datetime.utcnow()
        delta = self.expiry - now
        return max(delta.total_seconds() / 86400, 0.0)

    @property
    def is_expired(self) -> bool:
        now = datetime.now(timezone.utc) if self.expiry.tzinfo else datetime.utcnow()
        return now >= self.expiry

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


@runtime_checkable
class PaperOptionsBook(Protocol):

    def get_spot_price(self, underlying: str) -> float: ...

    def fetch_ohlcv(self, underlying: str, timeframe: str, limit: int) -> List[list]: ...

    def find_options(self, underlying: str, option_type: OptionType, **criteria: Any) -> List[OptionContract]: ...

    def enrich_contract(self, contract: OptionContract) -> OptionContract: ...

    def get_iv_rank(self, underlying: str, lookback_days: int = 60) -> float: ...

    def get_positions(self) -> Dict[str, OptionPosition]: ...

    def get_open_position_count(self) -> int: ...

    def get_portfolio_value(self) -> float: ...

    def get_portfolio_greeks(self) -> Greeks: ...

    def get_premium_at_risk(self) -> float: ...
