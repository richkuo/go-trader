"""
Options trading strategies — modular strategy framework for options.
Each strategy evaluates market conditions and returns trade actions.
"""

import sys
import os as _os
sys.path.insert(0, _os.path.join(_os.path.dirname(_os.path.abspath(__file__)), '..', '..', 'platforms', 'deribit'))
sys.path.insert(0, _os.path.dirname(_os.path.abspath(__file__)))

from typing import Dict, Any, List, Optional, Callable
from datetime import datetime, timedelta
from dataclasses import dataclass

import pandas as pd
import numpy as np

from adapter import (
    DeribitOptionsAdapter, OptionContract, OptionPosition, OptionType,
    OptionSide, Greeks
)
from risk import OptionsRiskManager


# ─────────────────────────────────────────────
# Strategy registry
# ─────────────────────────────────────────────

OPTIONS_STRATEGY_REGISTRY: Dict[str, dict] = {}


def register_options_strategy(name: str, description: str, default_params: dict):
    """Decorator to register an options strategy."""
    def decorator(cls):
        OPTIONS_STRATEGY_REGISTRY[name] = {
            "class": cls,
            "description": description,
            "default_params": default_params,
        }
        return cls
    return decorator


def get_options_strategy(name: str) -> dict:
    if name not in OPTIONS_STRATEGY_REGISTRY:
        raise ValueError(
            f"Unknown options strategy: {name}. "
            f"Available: {list(OPTIONS_STRATEGY_REGISTRY.keys())}"
        )
    return OPTIONS_STRATEGY_REGISTRY[name]


def list_options_strategies() -> List[str]:
    return list(OPTIONS_STRATEGY_REGISTRY.keys())


def create_options_strategy(name: str, adapter: DeribitOptionsAdapter,
                             risk_manager: OptionsRiskManager,
                             params: Optional[dict] = None):
    """Instantiate a strategy by name."""
    strat = get_options_strategy(name)
    p = {**strat["default_params"], **(params or {})}
    return strat["class"](adapter, risk_manager, **p)


# ─────────────────────────────────────────────
# Base class
# ─────────────────────────────────────────────

class BaseOptionsStrategy:
    """Base class for options strategies."""

    def __init__(self, adapter: DeribitOptionsAdapter,
                 risk_manager: OptionsRiskManager, **kwargs):
        self.adapter = adapter
        self.risk = risk_manager
        self.params = kwargs
        self.last_action_time: Optional[datetime] = None

    def evaluate(self, underlying: str) -> List[dict]:
        """
        Evaluate strategy and return list of actions.
        Each action: {'type': 'buy_call'|'buy_put'|'sell_call'|'sell_put'|
                       'open_straddle'|'open_strangle'|'close'|'none',
                      'reason': str, ...}
        """
        raise NotImplementedError

    def manage_positions(self, underlying: str) -> List[dict]:
        """Manage existing positions (exits, rolls, etc.)."""
        return []


# ─────────────────────────────────────────────
# Strategy 1: Momentum Options
# ─────────────────────────────────────────────

@register_options_strategy(
    "momentum_options",
    "Momentum Options — use ROC momentum to trade calls/puts (30-45 DTE)",
    {"roc_period": 14, "threshold": 5.0, "target_dte": 37,
     "profit_target_pct": 50.0, "stop_loss_pct": 30.0,
     "position_size_pct": 3.0}
)
class MomentumOptionsStrategy(BaseOptionsStrategy):
    """
    When momentum signal = buy → buy ATM/slightly OTM call (30-45 DTE).
    When momentum signal = sell → buy ATM/slightly OTM put (30-45 DTE).
    Exit at 50% profit or 30% loss on premium.
    """

    def _get_momentum_signal(self, underlying: str) -> int:
        """Calculate momentum signal using ROC (same as strategies.py momentum)."""
        try:
            # Fetch OHLCV from the adapter's exchange (Deribit has perpetuals)
            symbol = f"{underlying}/USD:{underlying}-PERPETUAL"
            ohlcv = self.adapter.exchange.fetch_ohlcv(symbol, "4h", limit=100)
            if not ohlcv or len(ohlcv) < 30:
                return 0

            closes = [c[4] for c in ohlcv]
            roc_period = self.params.get("roc_period", 14)
            threshold = self.params.get("threshold", 5.0)

            if len(closes) < roc_period + 2:
                return 0

            # ROC = (close - close[n]) / close[n] * 100
            current_roc = (closes[-1] - closes[-1 - roc_period]) / closes[-1 - roc_period] * 100
            prev_roc = (closes[-2] - closes[-2 - roc_period]) / closes[-2 - roc_period] * 100

            # Buy when ROC crosses above threshold
            if current_roc > threshold and prev_roc <= threshold:
                return 1
            # Sell when ROC crosses below -threshold
            if current_roc < -threshold and prev_roc >= -threshold:
                return -1

            return 0
        except Exception as e:
            return 0

    def evaluate(self, underlying: str) -> List[dict]:
        actions = []
        signal = self._get_momentum_signal(underlying)

        if signal == 0:
            return [{"type": "none", "reason": "No momentum signal"}]

        # Check if we already have a position for this underlying
        existing = [p for p in self.adapter.get_positions().values()
                    if p.underlying == underlying]
        if existing:
            return [{"type": "none", "reason": f"Already have {len(existing)} positions in {underlying}"}]

        target_dte = self.params.get("target_dte", 37)
        size_pct = self.params.get("position_size_pct", 3.0)
        portfolio_value = self.adapter.get_portfolio_value()
        budget = portfolio_value * (size_pct / 100)

        if signal == 1:
            # Buy call
            options = self.adapter.find_options(
                underlying, OptionType.CALL,
                min_dte=25, max_dte=50, moneyness="ATM", max_results=3
            )
            if not options:
                return [{"type": "none", "reason": "No suitable calls found"}]

            contract = options[0]
            contract = self.adapter.enrich_contract(contract)
            est_cost = contract.usd_price
            if est_cost <= 0:
                return [{"type": "none", "reason": "Cannot price contract"}]

            quantity = max(budget / est_cost, 0.1) if est_cost > 0 else 0
            quantity = round(quantity, 2)

            risk_check = self.risk.check_can_trade(
                self.adapter, proposed_premium_usd=est_cost * quantity,
                proposed_side="buy", underlying=underlying
            )
            if not risk_check["allowed"]:
                return [{"type": "none", "reason": f"Risk blocked: {risk_check['reason']}"}]

            actions.append({
                "type": "buy_call",
                "contract": contract,
                "quantity": quantity,
                "reason": f"Momentum BUY signal → call {contract.strike} "
                          f"({contract.dte:.0f} DTE) ~${est_cost * quantity:.0f}",
            })

        elif signal == -1:
            # Buy put
            options = self.adapter.find_options(
                underlying, OptionType.PUT,
                min_dte=25, max_dte=50, moneyness="ATM", max_results=3
            )
            if not options:
                return [{"type": "none", "reason": "No suitable puts found"}]

            contract = options[0]
            contract = self.adapter.enrich_contract(contract)
            est_cost = contract.usd_price
            if est_cost <= 0:
                return [{"type": "none", "reason": "Cannot price contract"}]

            quantity = max(budget / est_cost, 0.1) if est_cost > 0 else 0
            quantity = round(quantity, 2)

            risk_check = self.risk.check_can_trade(
                self.adapter, proposed_premium_usd=est_cost * quantity,
                proposed_side="buy", underlying=underlying
            )
            if not risk_check["allowed"]:
                return [{"type": "none", "reason": f"Risk blocked: {risk_check['reason']}"}]

            actions.append({
                "type": "buy_put",
                "contract": contract,
                "quantity": quantity,
                "reason": f"Momentum SELL signal → put {contract.strike} "
                          f"({contract.dte:.0f} DTE) ~${est_cost * quantity:.0f}",
            })

        return actions

    def manage_positions(self, underlying: str) -> List[dict]:
        """Check for profit target / stop loss exits."""
        actions = []
        profit_target = self.params.get("profit_target_pct", 50.0)
        stop_loss = self.params.get("stop_loss_pct", 30.0)

        for pid, pos in self.adapter.get_positions().items():
            if pos.underlying != underlying:
                continue

            pnl_pct = pos.pnl_pct
            if pos.side == OptionSide.BUY:
                if pnl_pct >= profit_target:
                    actions.append({
                        "type": "close",
                        "position_id": pid,
                        "reason": f"Profit target hit: {pnl_pct:.1f}% >= {profit_target}%",
                    })
                elif pnl_pct <= -stop_loss:
                    actions.append({
                        "type": "close",
                        "position_id": pid,
                        "reason": f"Stop loss hit: {pnl_pct:.1f}% <= -{stop_loss}%",
                    })
                elif pos.dte < 5:
                    actions.append({
                        "type": "close",
                        "position_id": pid,
                        "reason": f"Approaching expiry: {pos.dte:.1f} DTE",
                    })

        return actions


# ─────────────────────────────────────────────
# Strategy 2: Volatility Mean Reversion
# ─────────────────────────────────────────────

@register_options_strategy(
    "vol_mean_reversion",
    "Volatility Mean Reversion — trade IV rank with straddles/strangles",
    {"high_iv_threshold": 75, "low_iv_threshold": 25,
     "target_dte": 30, "iv_lookback_days": 60,
     "exit_iv_reversion_pct": 50.0, "position_size_pct": 5.0}
)
class VolMeanReversionStrategy(BaseOptionsStrategy):
    """
    High IV rank (>75%) → sell straddles/strangles (premium collection).
    Low IV rank (<25%) → buy straddles (expect vol expansion).
    Delta-neutral entry, exit when IV reverts to median.
    """

    def evaluate(self, underlying: str) -> List[dict]:
        actions = []

        iv_rank = self.adapter.get_iv_rank(underlying,
                                            lookback_days=self.params.get("iv_lookback_days", 60))
        high_thresh = self.params.get("high_iv_threshold", 75)
        low_thresh = self.params.get("low_iv_threshold", 25)
        target_dte = self.params.get("target_dte", 30)
        size_pct = self.params.get("position_size_pct", 5.0)

        # Check existing vol positions
        existing = [p for p in self.adapter.get_positions().values()
                    if p.underlying == underlying and p.leg_group and
                    ("straddle" in p.leg_group or "strangle" in p.leg_group)]
        if existing:
            return [{"type": "none", "reason": f"Already in vol trade for {underlying} (IV rank: {iv_rank:.0f})"}]

        portfolio_value = self.adapter.get_portfolio_value()
        budget = portfolio_value * (size_pct / 100)

        if iv_rank > high_thresh:
            # High IV → sell straddle/strangle
            risk_check = self.risk.check_can_trade(
                self.adapter, proposed_premium_usd=budget,
                proposed_side="sell", underlying=underlying
            )
            if not risk_check["allowed"]:
                return [{"type": "none", "reason": f"Risk blocked: {risk_check['reason']}"}]

            actions.append({
                "type": "sell_strangle",
                "underlying": underlying,
                "target_dte": target_dte,
                "quantity": 1.0,
                "reason": f"IV rank {iv_rank:.0f}% > {high_thresh}% → sell strangle",
            })

        elif iv_rank < low_thresh:
            # Low IV → buy straddle
            risk_check = self.risk.check_can_trade(
                self.adapter, proposed_premium_usd=budget,
                proposed_side="buy", underlying=underlying
            )
            if not risk_check["allowed"]:
                return [{"type": "none", "reason": f"Risk blocked: {risk_check['reason']}"}]

            actions.append({
                "type": "buy_straddle",
                "underlying": underlying,
                "target_dte": target_dte,
                "quantity": 1.0,
                "reason": f"IV rank {iv_rank:.0f}% < {low_thresh}% → buy straddle",
            })

        else:
            return [{"type": "none",
                     "reason": f"IV rank {iv_rank:.0f}% — neutral zone ({low_thresh}-{high_thresh})"}]

        return actions

    def manage_positions(self, underlying: str) -> List[dict]:
        """Exit when IV reverts toward median."""
        actions = []
        reversion_pct = self.params.get("exit_iv_reversion_pct", 50.0)

        for pid, pos in self.adapter.get_positions().items():
            if pos.underlying != underlying or not pos.leg_group:
                continue
            if "straddle" not in pos.leg_group and "strangle" not in pos.leg_group:
                continue

            # Check profit/loss
            pnl_pct = pos.pnl_pct
            if pos.side == OptionSide.SELL and pnl_pct >= 50:
                # Sold premium, captured enough
                actions.append({
                    "type": "close_group",
                    "leg_group": pos.leg_group,
                    "reason": f"Vol sell profit target: {pnl_pct:.1f}%",
                })
            elif pos.side == OptionSide.BUY and pnl_pct >= reversion_pct:
                actions.append({
                    "type": "close_group",
                    "leg_group": pos.leg_group,
                    "reason": f"Vol buy profit target: {pnl_pct:.1f}%",
                })
            elif pnl_pct <= -30:
                actions.append({
                    "type": "close_group",
                    "leg_group": pos.leg_group,
                    "reason": f"Vol trade stop loss: {pnl_pct:.1f}%",
                })
            elif pos.dte < 7:
                actions.append({
                    "type": "close_group",
                    "leg_group": pos.leg_group,
                    "reason": f"Vol trade expiry approaching: {pos.dte:.0f} DTE",
                })

        # Deduplicate by leg_group
        seen = set()
        unique = []
        for a in actions:
            key = a.get("leg_group", a.get("position_id", ""))
            if key not in seen:
                seen.add(key)
                unique.append(a)
        return unique


# ─────────────────────────────────────────────
# Strategy 3: Protective Puts
# ─────────────────────────────────────────────

@register_options_strategy(
    "protective_puts",
    "Protective Puts — hedge spot holdings with OTM puts",
    {"otm_pct": 12.0, "target_dte": 45, "roll_dte": 10,
     "max_hedge_cost_pct": 2.0, "spot_holding_usd": 5000.0}
)
class ProtectivePutsStrategy(BaseOptionsStrategy):
    """
    Buy OTM puts (10-15% OTM, 30-60 DTE) to hedge spot.
    Roll before expiry. Limit hedge cost to 2% of portfolio/month.
    """

    def evaluate(self, underlying: str) -> List[dict]:
        actions = []
        otm_pct = self.params.get("otm_pct", 12.0)
        target_dte = self.params.get("target_dte", 45)
        spot_holding = self.params.get("spot_holding_usd", 5000.0)

        # Check if we already have protective puts
        existing = [p for p in self.adapter.get_positions().values()
                    if p.underlying == underlying and
                    p.option_type == OptionType.PUT and
                    p.side == OptionSide.BUY]
        if existing:
            return [{"type": "none", "reason": f"Already have protective puts for {underlying}"}]

        # Find OTM puts
        spot = self.adapter.get_spot_price(underlying)
        target_strike = spot * (1 - otm_pct / 100)

        options = self.adapter.find_options(
            underlying, OptionType.PUT,
            min_dte=25, max_dte=65, moneyness="OTM", max_results=10
        )
        if not options:
            return [{"type": "none", "reason": "No suitable puts found"}]

        # Pick closest to target strike and DTE
        best = min(options, key=lambda c: abs(c.strike - target_strike) + abs(c.dte - target_dte))
        best = self.adapter.enrich_contract(best)

        est_cost = best.usd_price
        if est_cost <= 0:
            return [{"type": "none", "reason": "Cannot price protective put"}]

        # Scale quantity to cover holding
        quantity = max(spot_holding / spot, 0.01)
        total_cost = est_cost * quantity

        # Check hedge budget
        portfolio_value = self.adapter.get_portfolio_value()
        if not self.risk.check_hedge_budget(total_cost, portfolio_value):
            return [{"type": "none",
                     "reason": f"Hedge budget exceeded (${self.risk.monthly_hedge_spend:.0f} + ${total_cost:.0f})"}]

        risk_check = self.risk.check_can_trade(
            self.adapter, proposed_premium_usd=total_cost,
            proposed_side="buy", underlying=underlying
        )
        if not risk_check["allowed"]:
            return [{"type": "none", "reason": f"Risk blocked: {risk_check['reason']}"}]

        actions.append({
            "type": "buy_put",
            "contract": best,
            "quantity": round(quantity, 4),
            "is_hedge": True,
            "reason": f"Protective put: {best.strike:.0f} strike "
                      f"({best.dte:.0f} DTE, {otm_pct}% OTM) ~${total_cost:.0f}",
        })
        return actions

    def manage_positions(self, underlying: str) -> List[dict]:
        """Roll puts before expiry."""
        actions = []
        roll_dte = self.params.get("roll_dte", 10)

        for pid, pos in self.adapter.get_positions().items():
            if (pos.underlying == underlying and
                    pos.option_type == OptionType.PUT and
                    pos.side == OptionSide.BUY and
                    pos.dte < roll_dte):
                actions.append({
                    "type": "roll",
                    "position_id": pid,
                    "reason": f"Rolling protective put: {pos.dte:.0f} DTE < {roll_dte}",
                })

        return actions


# ─────────────────────────────────────────────
# Strategy 4: Covered Calls
# ─────────────────────────────────────────────

@register_options_strategy(
    "covered_calls",
    "Covered Calls — sell OTM calls against spot holdings for income",
    {"otm_pct": 12.0, "target_dte": 21, "roll_dte": 5,
     "itm_roll_threshold_pct": 2.0, "spot_holding_usd": 5000.0}
)
class CoveredCallsStrategy(BaseOptionsStrategy):
    """
    Sell OTM calls (10-15% OTM, 14-30 DTE) against spot holdings.
    Roll if approaching ITM. Target: 2-4% premium/month.
    """

    def evaluate(self, underlying: str) -> List[dict]:
        actions = []
        otm_pct = self.params.get("otm_pct", 12.0)
        target_dte = self.params.get("target_dte", 21)
        spot_holding = self.params.get("spot_holding_usd", 5000.0)

        # Check if we already have covered calls
        existing = [p for p in self.adapter.get_positions().values()
                    if p.underlying == underlying and
                    p.option_type == OptionType.CALL and
                    p.side == OptionSide.SELL]
        if existing:
            return [{"type": "none", "reason": f"Already have covered calls for {underlying}"}]

        spot = self.adapter.get_spot_price(underlying)
        target_strike = spot * (1 + otm_pct / 100)

        options = self.adapter.find_options(
            underlying, OptionType.CALL,
            min_dte=10, max_dte=35, moneyness="OTM", max_results=10
        )
        if not options:
            return [{"type": "none", "reason": "No suitable calls found"}]

        best = min(options, key=lambda c: abs(c.strike - target_strike) + abs(c.dte - target_dte))
        best = self.adapter.enrich_contract(best)

        est_premium = best.usd_price
        if est_premium <= 0:
            return [{"type": "none", "reason": "Cannot price covered call"}]

        # Quantity to cover spot holding
        quantity = max(spot_holding / spot, 0.01)

        risk_check = self.risk.check_can_trade(
            self.adapter, proposed_premium_usd=est_premium * quantity,
            proposed_side="sell", underlying=underlying
        )
        if not risk_check["allowed"]:
            return [{"type": "none", "reason": f"Risk blocked: {risk_check['reason']}"}]

        monthly_yield = (est_premium / spot) * (30 / best.dte) * 100 if best.dte > 0 else 0
        actions.append({
            "type": "sell_call",
            "contract": best,
            "quantity": round(quantity, 4),
            "reason": f"Covered call: {best.strike:.0f} strike "
                      f"({best.dte:.0f} DTE, {otm_pct}% OTM) "
                      f"~{monthly_yield:.1f}%/month yield",
        })
        return actions

    def manage_positions(self, underlying: str) -> List[dict]:
        """Roll if approaching ITM or expiry."""
        actions = []
        roll_dte = self.params.get("roll_dte", 5)
        itm_threshold = self.params.get("itm_roll_threshold_pct", 2.0)

        for pid, pos in self.adapter.get_positions().items():
            if (pos.underlying != underlying or
                    pos.option_type != OptionType.CALL or
                    pos.side != OptionSide.SELL):
                continue

            spot = pos.current_spot or self.adapter.get_spot_price(pos.underlying)

            # Roll if approaching ITM
            distance_pct = ((pos.strike - spot) / spot) * 100
            if distance_pct < itm_threshold:
                actions.append({
                    "type": "roll",
                    "position_id": pid,
                    "reason": f"Roll covered call: spot within {distance_pct:.1f}% of strike",
                })
            elif pos.dte < roll_dte:
                actions.append({
                    "type": "roll",
                    "position_id": pid,
                    "reason": f"Roll covered call: {pos.dte:.0f} DTE < {roll_dte}",
                })

        return actions


if __name__ == "__main__":
    import json
    if "--list-json" in sys.argv:
        print(json.dumps([{"id": name, "description": OPTIONS_STRATEGY_REGISTRY[name]["description"]} for name in list_options_strategies()]))
    else:
        print(f"Registered options strategies: {list_options_strategies()}")
        for name in list_options_strategies():
            s = OPTIONS_STRATEGY_REGISTRY[name]
            print(f"  {name}: {s['description']}")
            print(f"    Defaults: {s['default_params']}")
