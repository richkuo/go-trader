"""
Risk management rules engine.
Enforces position limits, exposure limits, daily loss limits, and drawdown kill switch.
"""

import json
import os
from typing import Optional, Dict, List
from datetime import datetime, timedelta
from dataclasses import dataclass, field, asdict


@dataclass
class RiskConfig:
    """Risk management configuration."""
    # Position limits
    max_position_size_pct: float = 20.0       # Max % of portfolio per position
    max_position_size_usd: float = 5000.0     # Hard USD cap per position
    max_num_positions: int = 5                  # Max simultaneous positions

    # Exposure limits
    max_total_exposure_pct: float = 80.0       # Max % of portfolio in positions
    max_single_asset_pct: float = 30.0          # Max % in one asset

    # Loss limits
    daily_loss_limit_pct: float = 5.0          # Max daily loss % — stops trading
    max_drawdown_pct: float = 15.0              # Max drawdown from peak — kills all trading
    per_trade_stop_loss_pct: float = 3.0        # Default stop loss per trade

    # Circuit breakers
    max_consecutive_losses: int = 5             # Pause after N consecutive losses
    cooldown_after_circuit_break_min: int = 60  # Cooldown period in minutes

    def to_dict(self) -> dict:
        return asdict(self)


class RiskManager:
    """
    Risk management engine that checks all rules before allowing trades.
    """

    def __init__(self, config: Optional[RiskConfig] = None):
        self.config = config or RiskConfig()
        self.peak_portfolio_value = 0.0
        self.daily_start_value = 0.0
        self.daily_pnl = 0.0
        self.consecutive_losses = 0
        self.circuit_break_active = False
        self.circuit_break_until: Optional[datetime] = None
        self.trade_log: List[dict] = []
        self._day_str = ""

    def reset_daily(self, portfolio_value: float):
        """Reset daily tracking. Call at start of each trading day."""
        today = datetime.utcnow().strftime("%Y-%m-%d")
        if today != self._day_str:
            self._day_str = today
            self.daily_start_value = portfolio_value
            self.daily_pnl = 0.0
            if portfolio_value > self.peak_portfolio_value:
                self.peak_portfolio_value = portfolio_value

    def update_peak(self, portfolio_value: float):
        """Update peak portfolio value for drawdown tracking."""
        if portfolio_value > self.peak_portfolio_value:
            self.peak_portfolio_value = portfolio_value

    def record_trade_result(self, pnl: float):
        """Record a trade PnL for consecutive loss tracking."""
        self.daily_pnl += pnl
        if pnl < 0:
            self.consecutive_losses += 1
        else:
            self.consecutive_losses = 0
        self.trade_log.append({
            "pnl": pnl,
            "timestamp": datetime.utcnow().isoformat(),
            "consecutive_losses": self.consecutive_losses,
        })

    def check_can_trade(self, portfolio_value: float, proposed_trade_usd: float = 0,
                         symbol: str = "", current_positions: Optional[Dict[str, float]] = None) -> dict:
        """
        Check if a proposed trade passes all risk rules.

        Returns:
            dict with 'allowed' (bool) and 'reason' (str) if blocked.
        """
        self.reset_daily(portfolio_value)

        # Check circuit breaker cooldown
        if self.circuit_break_active:
            if self.circuit_break_until and datetime.utcnow() < self.circuit_break_until:
                remaining = (self.circuit_break_until - datetime.utcnow()).seconds // 60
                return {"allowed": False, "reason": f"Circuit breaker active. Cooldown: {remaining}min remaining"}
            else:
                self.circuit_break_active = False
                self.consecutive_losses = 0

        # Check consecutive losses
        if self.consecutive_losses >= self.config.max_consecutive_losses:
            self._trigger_circuit_break()
            return {"allowed": False,
                    "reason": f"Circuit breaker: {self.consecutive_losses} consecutive losses"}

        # Check daily loss limit
        if self.daily_start_value > 0:
            daily_loss_pct = (self.daily_pnl / self.daily_start_value) * 100
            if daily_loss_pct <= -self.config.daily_loss_limit_pct:
                self._trigger_circuit_break()
                return {"allowed": False,
                        "reason": f"Daily loss limit hit: {daily_loss_pct:.2f}% (limit: -{self.config.daily_loss_limit_pct}%)"}

        # Check max drawdown kill switch
        if self.peak_portfolio_value > 0:
            drawdown_pct = ((portfolio_value - self.peak_portfolio_value) / self.peak_portfolio_value) * 100
            if drawdown_pct <= -self.config.max_drawdown_pct:
                self._trigger_circuit_break()
                return {"allowed": False,
                        "reason": f"KILL SWITCH: Max drawdown {drawdown_pct:.2f}% (limit: -{self.config.max_drawdown_pct}%)"}

        # Check position size limits
        if proposed_trade_usd > 0:
            # Percentage limit
            max_by_pct = portfolio_value * (self.config.max_position_size_pct / 100)
            max_allowed = min(max_by_pct, self.config.max_position_size_usd)
            if proposed_trade_usd > max_allowed:
                return {"allowed": False,
                        "reason": f"Position too large: ${proposed_trade_usd:,.2f} > limit ${max_allowed:,.2f}"}

        # Check number of positions
        positions = current_positions or {}
        active_positions = {k: v for k, v in positions.items() if v > 0}
        if len(active_positions) >= self.config.max_num_positions:
            if symbol and symbol.split("/")[0] not in active_positions:
                return {"allowed": False,
                        "reason": f"Max positions reached: {len(active_positions)}/{self.config.max_num_positions}"}

        # Check total exposure
        if positions and portfolio_value > 0:
            total_exposure = sum(positions.values())
            exposure_pct = (total_exposure / portfolio_value) * 100
            if exposure_pct + (proposed_trade_usd / portfolio_value * 100) > self.config.max_total_exposure_pct:
                return {"allowed": False,
                        "reason": f"Total exposure would exceed {self.config.max_total_exposure_pct}%"}

        return {"allowed": True, "reason": "OK"}

    def calculate_position_size(self, portfolio_value: float, entry_price: float,
                                 stop_loss_price: Optional[float] = None) -> float:
        """
        Calculate appropriate position size based on risk rules.
        Uses fixed fractional or stop-loss based sizing.

        Returns:
            Position size in quote currency (e.g., USDT amount)
        """
        # Max by percentage
        max_by_pct = portfolio_value * (self.config.max_position_size_pct / 100)
        max_allowed = min(max_by_pct, self.config.max_position_size_usd)

        if stop_loss_price and entry_price > 0:
            # Risk-based sizing: risk X% of portfolio per trade
            risk_per_trade = portfolio_value * (self.config.per_trade_stop_loss_pct / 100)
            price_risk = abs(entry_price - stop_loss_price) / entry_price
            if price_risk > 0:
                size = risk_per_trade / price_risk
                return min(size, max_allowed)

        return max_allowed

    def get_stop_loss_price(self, entry_price: float, side: str = "long") -> float:
        """Calculate default stop loss price."""
        stop_pct = self.config.per_trade_stop_loss_pct / 100
        if side == "long":
            return entry_price * (1 - stop_pct)
        else:
            return entry_price * (1 + stop_pct)

    def _trigger_circuit_break(self):
        """Activate circuit breaker with cooldown."""
        self.circuit_break_active = True
        self.circuit_break_until = datetime.utcnow() + timedelta(
            minutes=self.config.cooldown_after_circuit_break_min
        )
        print(f"⚠️  CIRCUIT BREAKER TRIGGERED — Trading paused until {self.circuit_break_until.strftime('%H:%M UTC')}")

    def get_status(self) -> dict:
        """Get current risk management status."""
        return {
            "circuit_break_active": self.circuit_break_active,
            "consecutive_losses": self.consecutive_losses,
            "daily_pnl": round(self.daily_pnl, 2),
            "peak_portfolio": round(self.peak_portfolio_value, 2),
            "config": self.config.to_dict(),
        }

    def format_status(self) -> str:
        """Human-readable risk status."""
        s = self.get_status()
        lines = [
            f"\n{'─'*50}",
            f"  RISK MANAGER STATUS",
            f"{'─'*50}",
            f"  Circuit Breaker:    {'🔴 ACTIVE' if s['circuit_break_active'] else '🟢 OK'}",
            f"  Consecutive Losses: {s['consecutive_losses']}/{self.config.max_consecutive_losses}",
            f"  Daily PnL:          ${s['daily_pnl']:+,.2f}",
            f"  Peak Portfolio:     ${s['peak_portfolio']:,.2f}",
            f"  Max Position Size:  {self.config.max_position_size_pct}% / ${self.config.max_position_size_usd:,.0f}",
            f"  Daily Loss Limit:   -{self.config.daily_loss_limit_pct}%",
            f"  Max Drawdown:       -{self.config.max_drawdown_pct}%",
            f"{'─'*50}",
        ]
        return "\n".join(lines)


if __name__ == "__main__":
    rm = RiskManager()
    print(rm.format_status())

    # Test checks
    result = rm.check_can_trade(10000, proposed_trade_usd=1500)
    print(f"Trade $1500 on $10k portfolio: {result}")

    result = rm.check_can_trade(10000, proposed_trade_usd=6000)
    print(f"Trade $6000 on $10k portfolio: {result}")

    # Simulate losses
    for i in range(5):
        rm.record_trade_result(-100)
    result = rm.check_can_trade(9500)
    print(f"After 5 losses: {result}")
