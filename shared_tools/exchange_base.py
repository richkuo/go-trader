"""
ExchangeAdapter protocol — unified interface for all trading platforms.
Concrete adapters live in platforms/<name>/adapter.py.
"""

from typing import Protocol, Tuple


class ExchangeAdapter(Protocol):
    """
    Protocol (structural interface) that all platform adapters must implement.
    Enables shared strategy code to work across Deribit, IBKR, Binance US, etc.
    """

    @property
    def name(self) -> str:
        """Platform name, e.g. 'deribit', 'ibkr', 'binanceus'."""
        ...

    def get_spot_price(self, underlying: str) -> float:
        """Return current USD spot price for underlying (e.g. 'BTC')."""
        ...

    def get_real_expiry(self, underlying: str, target_dte: int) -> Tuple[str, int]:
        """
        Return the closest available expiry to target_dte.

        Returns:
            (expiry_str: "YYYY-MM-DD", actual_dte: int)
        """
        ...

    def get_real_strike(self, underlying: str, expiry: str,
                        option_type: str, target_strike: float) -> float:
        """
        Return the closest available strike to target_strike for the given expiry.

        Args:
            underlying: e.g. "BTC"
            expiry: "YYYY-MM-DD"
            option_type: "call" or "put"
            target_strike: ideal strike in USD

        Returns:
            Actual available strike in USD
        """
        ...

    def get_premium_and_greeks(self, underlying: str, option_type: str,
                                strike: float, expiry: str,
                                dte: float, spot: float, vol: float) -> Tuple[float, float, dict]:
        """
        Return option premium and Greeks.

        Args:
            underlying: e.g. "BTC"
            option_type: "call" or "put"
            strike: strike price in USD
            expiry: "YYYY-MM-DD"
            dte: days to expiration
            spot: current spot price
            vol: annualized implied volatility (decimal)

        Returns:
            (premium_pct: fraction of spot, premium_usd: USD value, greeks: dict)
        """
        ...

    def get_vol_metrics(self, underlying: str) -> Tuple[float, float]:
        """
        Return current volatility metrics for the underlying.

        Returns:
            (vol_annual: annualized vol as decimal, iv_rank: 0-100)
        """
        ...
