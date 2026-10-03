
from typing import Protocol, Tuple


class ExchangeAdapter(Protocol):

    @property
    def name(self) -> str:
        ...

    def get_spot_price(self, underlying: str) -> float:
        ...

    def get_real_expiry(self, underlying: str, target_dte: int) -> Tuple[str, int]:
        ...

    def get_real_strike(self, underlying: str, expiry: str,
                        option_type: str, target_strike: float) -> float:
        ...

    def get_premium_and_greeks(self, underlying: str, option_type: str,
                                strike: float, expiry: str,
                                dte: float, spot: float, vol: float) -> Tuple[float, float, dict]:
        ...

    def get_vol_metrics(self, underlying: str) -> Tuple[float, float]:
        ...
