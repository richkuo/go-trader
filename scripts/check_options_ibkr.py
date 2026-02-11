#!/usr/bin/env python3
"""
Stateless IBKR options strategy check script.
Evaluates vol_mean_reversion using CME crypto options contract specs.
Paper trading mode — uses Black-Scholes for premium estimation.

Usage: python3 check_options_ibkr.py <strategy> <underlying> [positions_json]

Key differences from Deribit version:
- Uses CME Micro contract multipliers (BTC=0.1, ETH=0.5)
- Strike prices align to CME intervals ($1000 for BTC, $50 for ETH)
- Premiums are per-contract (multiplier applied)
- Designed for IBKR TWS API integration when going live
"""

import sys
import os
import json
import math
import traceback
from datetime import datetime, timezone, timedelta

# Add parent dir so we can import from options/
sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

from options.ibkr_adapter import (
    IBKRPaperAdapter,
    get_spot_price_ibkr,
    calc_vol_and_iv_rank,
    black_scholes,
    bs_greeks,
)


MAX_POSITIONS_PER_STRATEGY = 4
MIN_SCORE_THRESHOLD = 0.3

adapter = IBKRPaperAdapter()


def evaluate_vol_mean_reversion(underlying, spot_price, hist_vol, iv_rank):
    """
    Volatility mean reversion on CME crypto options.
    High IV → sell strangle (collect premium)
    Low IV → buy straddle (bet on move)
    """
    signal = 0
    actions = []

    # Target ~30 DTE
    expiry_date = datetime.now(timezone.utc) + timedelta(days=30)
    expiry_str = expiry_date.strftime("%Y-%m-%d")
    dte = 30

    # Get CME-aligned strikes
    strike_info = adapter.get_available_strikes(underlying, spot_price)
    interval = strike_info["interval"]
    multiplier = adapter.get_multiplier(underlying)

    if iv_rank > 75:
        # High IV → sell strangle
        signal = -1

        # OTM call: ~10% above spot, rounded to CME interval
        call_strike = math.ceil(spot_price * 1.10 / interval) * interval
        # OTM put: ~10% below spot
        put_strike = math.floor(spot_price * 0.90 / interval) * interval

        call_est = adapter.estimate_premium(underlying, spot_price, call_strike, dte, hist_vol, "call")
        put_est = adapter.estimate_premium(underlying, spot_price, put_strike, dte, hist_vol, "put")

        actions = [
            {
                "action": "sell",
                "option_type": "call",
                "strike": call_strike,
                "expiry": expiry_str,
                "dte": dte,
                "premium": round(call_est["premium_per_unit"] / spot_price, 4),
                "premium_usd": call_est["premium_usd"],
                "multiplier": multiplier,
                "contract_spec": "CME_MICRO",
                "greeks": call_est["greeks"],
            },
            {
                "action": "sell",
                "option_type": "put",
                "strike": put_strike,
                "expiry": expiry_str,
                "dte": dte,
                "premium": round(put_est["premium_per_unit"] / spot_price, 4),
                "premium_usd": put_est["premium_usd"],
                "multiplier": multiplier,
                "contract_spec": "CME_MICRO",
                "greeks": put_est["greeks"],
            },
        ]

    elif iv_rank < 25:
        # Low IV → buy straddle
        signal = 1

        # ATM strike
        strike = round(spot_price / interval) * interval

        call_est = adapter.estimate_premium(underlying, spot_price, strike, dte, hist_vol, "call")
        put_est = adapter.estimate_premium(underlying, spot_price, strike, dte, hist_vol, "put")

        total_cost = call_est["premium_usd"] + put_est["premium_usd"]

        actions = [
            {
                "action": "buy",
                "option_type": "call",
                "strike": strike,
                "expiry": expiry_str,
                "dte": dte,
                "premium": round(call_est["premium_per_unit"] / spot_price, 4),
                "premium_usd": call_est["premium_usd"],
                "multiplier": multiplier,
                "contract_spec": "CME_MICRO",
                "greeks": call_est["greeks"],
            },
            {
                "action": "buy",
                "option_type": "put",
                "strike": strike,
                "expiry": expiry_str,
                "dte": dte,
                "premium": round(put_est["premium_per_unit"] / spot_price, 4),
                "premium_usd": put_est["premium_usd"],
                "multiplier": multiplier,
                "contract_spec": "CME_MICRO",
                "greeks": put_est["greeks"],
            },
        ]

    return signal, actions


def score_new_trade(proposed_action, existing_positions, spot_price):
    """Score a proposed trade against existing positions."""
    if not existing_positions:
        return 1.0, "first position"

    score = 0.5
    reasons = []

    p_strike = proposed_action.get("strike", 0)
    p_expiry = proposed_action.get("expiry", "")
    p_type = proposed_action.get("option_type", "")
    p_delta = proposed_action.get("greeks", {}).get("delta", 0)

    # Strike distance
    same_type = [p for p in existing_positions if p.get("option_type") == p_type]
    if same_type and spot_price > 0:
        min_dist = min(abs(p_strike - p["strike"]) / spot_price for p in same_type)
        if min_dist > 0.10:
            score += 0.4
            reasons.append(f"strike distance {min_dist:.1%}")
        elif min_dist > 0.05:
            score += 0.2
            reasons.append(f"moderate distance {min_dist:.1%}")
        else:
            score -= 0.3
            reasons.append(f"overlapping {min_dist:.1%}")

    # Expiry spread
    existing_expiries = set(p.get("expiry", "") for p in existing_positions)
    if p_expiry not in existing_expiries:
        score += 0.3
        reasons.append("different expiry")
    else:
        score -= 0.1
        reasons.append("same expiry")

    # Delta balance
    net_delta = sum(p.get("delta", 0) for p in existing_positions)
    new_net = net_delta + p_delta
    if abs(new_net) > abs(net_delta) and abs(new_net) > 0.5:
        score -= 0.3
        reasons.append(f"delta concentration {new_net:+.2f}")
    elif abs(new_net) < abs(net_delta):
        score += 0.2
        reasons.append(f"delta balancing {new_net:+.2f}")

    return round(score, 2), "; ".join(reasons) if reasons else "default"


STRATEGY_MAP = {
    "vol_mean_reversion": evaluate_vol_mean_reversion,
}


def main():
    if len(sys.argv) < 3:
        print(json.dumps({"error": f"Usage: {sys.argv[0]} <strategy> <underlying> [positions_json]"}))
        sys.exit(1)

    strategy_name = sys.argv[1]
    underlying = sys.argv[2].upper()

    existing_positions = []
    if len(sys.argv) > 3:
        try:
            existing_positions = json.loads(sys.argv[3])
        except (json.JSONDecodeError, ValueError):
            pass

    # Hard cap
    if len(existing_positions) >= MAX_POSITIONS_PER_STRATEGY:
        print(json.dumps({
            "strategy": strategy_name,
            "underlying": underlying,
            "signal": 0,
            "spot_price": 0,
            "actions": [],
            "iv_rank": 0,
            "exchange": "IBKR_CME",
            "timestamp": datetime.now(timezone.utc).isoformat(),
            "skip_reason": f"Max positions ({len(existing_positions)}/{MAX_POSITIONS_PER_STRATEGY})",
        }))
        return

    if strategy_name not in STRATEGY_MAP:
        print(json.dumps({
            "strategy": strategy_name,
            "underlying": underlying,
            "signal": 0,
            "spot_price": 0,
            "actions": [],
            "iv_rank": 0,
            "exchange": "IBKR_CME",
            "timestamp": datetime.now(timezone.utc).isoformat(),
            "error": f"Unknown strategy: {strategy_name}",
        }))
        return

    try:
        spot_price = get_spot_price_ibkr(underlying)
        if spot_price <= 0:
            print(json.dumps({
                "strategy": strategy_name,
                "underlying": underlying,
                "signal": 0,
                "spot_price": 0,
                "actions": [],
                "iv_rank": 0,
                "exchange": "IBKR_CME",
                "timestamp": datetime.now(timezone.utc).isoformat(),
                "error": "Could not fetch spot price",
            }))
            return

        hist_vol, iv_rank = calc_vol_and_iv_rank(underlying)

        evaluate_fn = STRATEGY_MAP[strategy_name]
        signal, actions = evaluate_fn(underlying, spot_price, hist_vol, iv_rank)

        # Score actions
        scored_actions = []
        for action in actions:
            score, reason = score_new_trade(action, existing_positions, spot_price)
            action["score"] = score
            action["score_reason"] = reason
            if score >= MIN_SCORE_THRESHOLD:
                scored_actions.append(action)
            else:
                print(f"Skipping {action.get('action')} {action.get('option_type')} "
                      f"strike={action.get('strike')}: score={score} ({reason})",
                      file=sys.stderr)

        if actions and not scored_actions:
            signal = 0

        output = {
            "strategy": strategy_name,
            "underlying": underlying,
            "signal": signal,
            "spot_price": round(spot_price, 2),
            "actions": scored_actions,
            "iv_rank": iv_rank,
            "hist_vol_pct": round(hist_vol * 100, 1),
            "exchange": "IBKR_CME",
            "contract_type": "CME_MICRO",
            "multiplier": adapter.get_multiplier(underlying),
            "timestamp": datetime.now(timezone.utc).isoformat(),
        }
        print(json.dumps(output))

    except Exception as e:
        traceback.print_exc(file=sys.stderr)
        print(json.dumps({
            "strategy": strategy_name,
            "underlying": underlying,
            "signal": 0,
            "spot_price": 0,
            "actions": [],
            "iv_rank": 0,
            "exchange": "IBKR_CME",
            "timestamp": datetime.now(timezone.utc).isoformat(),
            "error": str(e),
        }))


if __name__ == "__main__":
    main()
