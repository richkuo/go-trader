"""
Options Risk Manager — risk management for options portfolios.
Extends risk_manager.py concepts with options-specific rules:
portfolio Greeks tracking, premium limits, delta bounds, margin estimation.
"""

import sys
import os as _os
sys.path.insert(0, _os.path.join(_os.path.dirname(_os.path.abspath(__file__)), '..', '..', 'platforms', 'deribit'))

from typing import Optional, Dict, List
from datetime import datetime, timedelta
from dataclasses import dataclass, asdict

from adapter import (
    DeribitOptionsAdapter, OptionPosition, OptionSide, OptionType, Greeks
)


@dataclass
class OptionsRiskConfig:
    """Options-specific risk configuration."""
    # Premium limits
    max_premium_at_risk_pct: float = 30.0      # Max % of portfolio in long premium
    max_single_trade_premium_pct: float = 5.0  # Max % of portfolio per single option trade
    max_monthly_hedge_cost_pct: float = 2.0    # Max monthly cost for protective puts

    # Position limits
    max_positions: int = 10                     # Max simultaneous option positions
    max_positions_per_underlying: int = 5       # Max per underlying

    # Greeks limits
    max_portfolio_delta: float = 5.0            # Absolute delta limit
    max_portfolio_gamma: float = 2.0            # Absolute gamma limit
    max_portfolio_vega: float = 500.0           # Max vega exposure (USD per 1% IV)
    min_portfolio_delta: float = -5.0           # Negative delta limit

    # Notional exposure
    max_notional_exposure_pct: float = 200.0    # Max notional as % of portfolio
    max_short_notional_pct: float = 100.0       # Max short notional as % of portfolio

    # Loss limits
    max_drawdown_pct: float = 20.0              # Max portfolio drawdown
    daily_loss_limit_pct: float = 5.0           # Daily loss limit
    per_trade_stop_loss_pct: float = 30.0       # Stop loss on premium paid

    # Circuit breakers
    max_consecutive_losses: int = 4
    cooldown_minutes: int = 60

    def to_dict(self) -> dict:
        return asdict(self)


class OptionsRiskManager:
    """
    Risk management engine for options portfolios.
    Tracks Greeks, enforces premium limits, and manages exposure.
    """

    def __init__(self, config: Optional[OptionsRiskConfig] = None):
        self.config = config or OptionsRiskConfig()
        self.peak_portfolio_value = 0.0
        self.daily_start_value = 0.0
        self.daily_pnl = 0.0
        self.consecutive_losses = 0
        self.circuit_break_active = False
        self.circuit_break_until: Optional[datetime] = None
        self.monthly_hedge_spend = 0.0
        self.monthly_hedge_reset: Optional[datetime] = None
        self._day_str = ""
        self.trade_log: List[dict] = []

    def reset_daily(self, portfolio_value: float):
        """Reset daily tracking."""
        today = datetime.utcnow().strftime("%Y-%m-%d")
        if today != self._day_str:
            self._day_str = today
            self.daily_start_value = portfolio_value
            self.daily_pnl = 0.0

    def update_peak(self, portfolio_value: float):
        """Update peak portfolio value."""
        if portfolio_value > self.peak_portfolio_value:
            self.peak_portfolio_value = portfolio_value

    def reset_monthly_hedge(self):
        """Reset monthly hedge spend tracker."""
        now = datetime.utcnow()
        if self.monthly_hedge_reset is None or (now - self.monthly_hedge_reset).days >= 30:
            self.monthly_hedge_spend = 0.0
            self.monthly_hedge_reset = now

    def record_trade_result(self, pnl: float):
        """Record a trade PnL."""
        self.daily_pnl += pnl
        if pnl < 0:
            self.consecutive_losses += 1
        else:
            self.consecutive_losses = 0
        self.trade_log.append({
            "pnl": pnl,
            "timestamp": datetime.utcnow().isoformat(),
        })

    def check_can_trade(self, adapter: DeribitOptionsAdapter,
                         proposed_premium_usd: float = 0,
                         proposed_side: str = "buy",
                         underlying: str = "") -> dict:
        """
        Check if a proposed options trade passes all risk rules.

        Returns:
            dict with 'allowed' (bool) and 'reason' (str) if blocked.
        """
        portfolio_value = adapter.get_portfolio_value()
        self.reset_daily(portfolio_value)

        # Circuit breaker
        if self.circuit_break_active:
            if self.circuit_break_until and datetime.utcnow() < self.circuit_break_until:
                remaining = (self.circuit_break_until - datetime.utcnow()).seconds // 60
                return {"allowed": False, "reason": f"Circuit breaker active ({remaining}min remaining)"}
            self.circuit_break_active = False
            self.consecutive_losses = 0

        # Consecutive losses
        if self.consecutive_losses >= self.config.max_consecutive_losses:
            self._trigger_circuit_break()
            return {"allowed": False,
                    "reason": f"Circuit breaker: {self.consecutive_losses} consecutive losses"}

        # Daily loss limit
        if self.daily_start_value > 0:
            daily_pct = (self.daily_pnl / self.daily_start_value) * 100
            if daily_pct <= -self.config.daily_loss_limit_pct:
                return {"allowed": False,
                        "reason": f"Daily loss limit: {daily_pct:.1f}%"}

        # Max drawdown
        if self.peak_portfolio_value > 0:
            dd_pct = ((portfolio_value - self.peak_portfolio_value) / self.peak_portfolio_value) * 100
            if dd_pct <= -self.config.max_drawdown_pct:
                return {"allowed": False,
                        "reason": f"Max drawdown hit: {dd_pct:.1f}%"}

        # Position count
        positions = adapter.get_positions()
        if len(positions) >= self.config.max_positions:
            return {"allowed": False,
                    "reason": f"Max positions ({self.config.max_positions}) reached"}

        # Per-underlying limit
        if underlying:
            underlying_count = sum(1 for p in positions.values() if p.underlying == underlying)
            if underlying_count >= self.config.max_positions_per_underlying:
                return {"allowed": False,
                        "reason": f"Max positions for {underlying} ({self.config.max_positions_per_underlying}) reached"}

        # Single trade premium limit
        if proposed_premium_usd > 0 and portfolio_value > 0:
            trade_pct = (proposed_premium_usd / portfolio_value) * 100
            if trade_pct > self.config.max_single_trade_premium_pct:
                return {"allowed": False,
                        "reason": f"Trade premium {trade_pct:.1f}% > limit {self.config.max_single_trade_premium_pct}%"}

        # Total premium at risk
        if proposed_side == "buy" and portfolio_value > 0:
            current_premium = adapter.get_premium_at_risk()
            new_total = current_premium + proposed_premium_usd
            pct = (new_total / portfolio_value) * 100
            if pct > self.config.max_premium_at_risk_pct:
                return {"allowed": False,
                        "reason": f"Premium at risk would be {pct:.1f}% > limit {self.config.max_premium_at_risk_pct}%"}

        return {"allowed": True, "reason": "OK"}

    def check_greeks_limits(self, adapter: DeribitOptionsAdapter) -> dict:
        """Check if portfolio Greeks are within limits."""
        greeks = adapter.get_portfolio_greeks()
        violations = []

        if greeks.delta > self.config.max_portfolio_delta:
            violations.append(f"Delta {greeks.delta:.2f} > max {self.config.max_portfolio_delta}")
        if greeks.delta < self.config.min_portfolio_delta:
            violations.append(f"Delta {greeks.delta:.2f} < min {self.config.min_portfolio_delta}")
        if abs(greeks.gamma) > self.config.max_portfolio_gamma:
            violations.append(f"|Gamma| {abs(greeks.gamma):.4f} > max {self.config.max_portfolio_gamma}")
        if abs(greeks.vega) > self.config.max_portfolio_vega:
            violations.append(f"|Vega| {abs(greeks.vega):.2f} > max {self.config.max_portfolio_vega}")

        return {
            "within_limits": len(violations) == 0,
            "violations": violations,
            "greeks": greeks.to_dict(),
        }

    def check_hedge_budget(self, cost_usd: float, portfolio_value: float) -> bool:
        """Check if hedge cost is within monthly budget."""
        self.reset_monthly_hedge()
        max_spend = portfolio_value * (self.config.max_monthly_hedge_cost_pct / 100)
        return (self.monthly_hedge_spend + cost_usd) <= max_spend

    def record_hedge_spend(self, cost_usd: float):
        """Record hedge premium spent."""
        self.reset_monthly_hedge()
        self.monthly_hedge_spend += cost_usd

    def estimate_margin(self, adapter: DeribitOptionsAdapter) -> dict:
        """Estimate margin requirements for short positions."""
        positions = adapter.get_positions()
        total_margin = 0.0

        for pos in positions.values():
            if pos.side != OptionSide.SELL:
                continue
            spot = pos.current_spot or pos.entry_spot
            if pos.option_type == OptionType.CALL:
                otm_amount = max(spot - pos.strike, 0)
            else:
                otm_amount = max(pos.strike - spot, 0)
            premium_margin = (pos.current_price * spot + otm_amount) * pos.quantity
            min_margin = 0.10 * spot * pos.quantity
            total_margin += max(premium_margin, min_margin)

        portfolio_value = adapter.get_portfolio_value()
        return {
            "estimated_margin": round(total_margin, 2),
            "margin_pct": round((total_margin / portfolio_value * 100) if portfolio_value > 0 else 0, 1),
            "portfolio_value": round(portfolio_value, 2),
        }

    def max_loss_scenario(self, adapter: DeribitOptionsAdapter,
                           spot_move_pct: float = 20.0) -> dict:
        """Estimate max loss under a spot price move scenario."""
        positions = adapter.get_positions()
        scenarios = {}

        for direction, mult in [("up", 1 + spot_move_pct / 100), ("down", 1 - spot_move_pct / 100)]:
            total_pnl = 0.0
            for pos in positions.values():
                spot = pos.current_spot or pos.entry_spot
                new_spot = spot * mult
                if pos.option_type == OptionType.CALL:
                    new_value = max(new_spot - pos.strike, 0)
                else:
                    new_value = max(pos.strike - new_spot, 0)
                current_value = pos.current_price * spot
                pnl = (new_value - current_value) * pos.quantity
                if pos.side == OptionSide.SELL:
                    pnl = -pnl
                total_pnl += pnl
            scenarios[direction] = round(total_pnl, 2)

        return {
            "spot_move_pct": spot_move_pct,
            "pnl_if_up": scenarios["up"],
            "pnl_if_down": scenarios["down"],
            "worst_case": min(scenarios["up"], scenarios["down"]),
        }

    def _trigger_circuit_break(self):
        """Activate circuit breaker."""
        self.circuit_break_active = True
        self.circuit_break_until = datetime.utcnow() + timedelta(minutes=self.config.cooldown_minutes)

    def format_status(self, adapter: DeribitOptionsAdapter) -> str:
        """Human-readable risk status."""
        portfolio_value = adapter.get_portfolio_value()
        greeks = adapter.get_portfolio_greeks()
        premium = adapter.get_premium_at_risk()
        margin = self.estimate_margin(adapter)
        dd_pct = 0.0
        if self.peak_portfolio_value > 0:
            dd_pct = ((portfolio_value - self.peak_portfolio_value) / self.peak_portfolio_value) * 100
        lines = [
            f"\n{'─'*55}",
            f"  OPTIONS RISK MANAGER STATUS",
            f"{'─'*55}",
            f"  Consecutive Losses: {self.consecutive_losses}/{self.config.max_consecutive_losses}",
            f"  Daily PnL:          ${self.daily_pnl:+,.2f}",
            f"  Drawdown:           {dd_pct:.1f}% (max: -{self.config.max_drawdown_pct}%)",
            f"  Positions:          {adapter.get_open_position_count()}/{self.config.max_positions}",
            f"{'─'*55}",
        ]
        return "\n".join(lines)
