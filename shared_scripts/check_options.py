#!/usr/bin/env python3
"""
Unified options strategy check script.
Evaluates options strategies using a platform adapter.

Usage: python3 check_options.py <strategy> <underlying> [--platform=deribit|ibkr]
"""

import sys
import os
import json
import math
import traceback
import importlib.util
from datetime import datetime, timezone

# ── path setup ───────────────────────────────────────────────────────────────
_THIS_DIR = os.path.dirname(os.path.abspath(__file__))
_REPO_ROOT = os.path.dirname(_THIS_DIR)
sys.path.insert(0, _REPO_ROOT)

MAX_POSITIONS_PER_STRATEGY = 4
MIN_SCORE_THRESHOLD = 0.3


# ── adapter loader ───────────────────────────────────────────────────────────

def _load_adapter(platform: str):
    """Dynamically load ExchangeAdapter from platforms/<platform>/adapter.py."""
    adapter_path = os.path.join(_REPO_ROOT, "platforms", platform, "adapter.py")
    if not os.path.exists(adapter_path):
        raise ImportError(f"No adapter found for platform '{platform}' at {adapter_path}")
    platform_dir = os.path.dirname(adapter_path)
    if platform_dir not in sys.path:
        sys.path.insert(0, platform_dir)
    spec = importlib.util.spec_from_file_location(f"{platform}_adapter", adapter_path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    for name in dir(module):
        if name.endswith("ExchangeAdapter"):
            return getattr(module, name)()
    raise AttributeError(f"No ExchangeAdapter class found in {adapter_path}")


# ── shared helpers ────────────────────────────────────────────────────────────

def _fetch_ohlcv_closes(underlying, timeframe, limit, min_len):
    """Fetch OHLCV from BinanceUS and return closing prices, or None if insufficient data."""
    import ccxt
    exchange = ccxt.binanceus({"enableRateLimit": True})
    ohlcv = exchange.fetch_ohlcv(f"{underlying}/USDT", timeframe, limit=limit)
    if not ohlcv or len(ohlcv) < min_len:
        return None
    return [c[4] for c in ohlcv]


def _platform_extra(adapter, underlying: str) -> dict:
    """Return platform-specific action fields (multiplier, contract_spec) if applicable."""
    multiplier_fn = getattr(adapter, 'get_multiplier', None)
    if multiplier_fn is not None:
        return {'multiplier': multiplier_fn(underlying), 'contract_spec': 'CME_MICRO'}
    return {}


def _build_action(action, option_type, strike, expiry_str, dte,
                  premium_pct, premium_usd, greeks, **extra) -> dict:
    d = {
        "action": action,
        "option_type": option_type,
        "strike": strike,
        "expiry": expiry_str,
        "dte": dte,
        "premium": premium_pct,
        "premium_usd": premium_usd,
        "greeks": greeks,
    }
    d.update(extra)
    return d


def _option_leg(underlying, action, option_type, target_strike,
                expiry_str, dte, fallback_pct, spot, vol, adapter, **extra) -> dict:
    """Look up the nearest real strike, fetch premium + Greeks, and return an action dict."""
    strike = adapter.get_real_strike(underlying, expiry_str, option_type, target_strike)
    prem_pct, prem_usd, greeks = adapter.get_premium_and_greeks(
        underlying, option_type, strike, expiry_str, dte, spot, vol
    )
    platform_fields = _platform_extra(adapter, underlying)
    return _build_action(action, option_type, strike, expiry_str, dte,
                         prem_pct, prem_usd, greeks, **platform_fields, **extra)


def parse_positions_context(raw_positions):
    """Split combined Go position list into option and spot position lists."""
    option_positions, spot_positions = [], []
    for p in (raw_positions or []):
        if isinstance(p, dict) and p.get("position_type") == "spot":
            spot_positions.append(p)
        elif isinstance(p, dict):
            option_positions.append(p)
    return option_positions, spot_positions


def score_new_trade(proposed_action, existing_positions, spot_price):
    """Score a proposed trade against existing positions. Returns (score, reason)."""
    if not existing_positions:
        return 1.0, "first position"

    score = 0.5
    reasons = []

    p_strike = proposed_action.get("strike", 0)
    p_expiry = proposed_action.get("expiry", "")
    p_type = proposed_action.get("option_type", "")
    p_delta = proposed_action.get("greeks", {}).get("delta", 0)

    same_type = [p for p in existing_positions if p.get("option_type") == p_type]
    if same_type and spot_price > 0:
        min_dist = min(abs(p_strike - p["strike"]) / spot_price for p in same_type)
        if min_dist > 0.10:
            score += 0.4
            reasons.append(f"strike distance {min_dist:.1%}")
        elif min_dist > 0.05:
            score += 0.2
            reasons.append(f"moderate strike distance {min_dist:.1%}")
        else:
            score -= 0.3
            reasons.append(f"overlapping strikes {min_dist:.1%}")

    existing_expiries = set(p.get("expiry", "") for p in existing_positions)
    if p_expiry not in existing_expiries:
        score += 0.3
        reasons.append("different expiry")
    else:
        score -= 0.1
        reasons.append("same expiry")

    net_delta = sum(p.get("delta", 0) for p in existing_positions)
    new_net = net_delta + p_delta
    if abs(new_net) > abs(net_delta) and abs(new_net) > 0.5:
        score -= 0.3
        reasons.append(f"delta concentration {new_net:+.2f}")
    elif abs(new_net) < abs(net_delta):
        score += 0.2
        reasons.append(f"delta balancing {new_net:+.2f}")

    if proposed_action.get("action") == "sell":
        sell_positions = [p for p in existing_positions if p.get("action") == "sell"]
        if sell_positions:
            avg_premium = sum(p.get("entry_premium_usd", 0) for p in sell_positions) / len(sell_positions)
            if proposed_action.get("premium_usd", 0) > avg_premium * 1.1:
                score += 0.1
                reasons.append("better premium")

    return round(score, 2), "; ".join(reasons) if reasons else "default"


# ── strategy evaluators ────────────────────────────────────────────────────────
# Signature: (underlying, spot_price, vol_annual, iv_rank, existing, spot_pos, adapter)
# Returns: (signal, actions, iv_rank)

def evaluate_momentum_options(underlying, spot_price, vol_annual, iv_rank,
                               existing_positions, spot_positions, adapter):
    try:
        closes = _fetch_ohlcv_closes(underlying, "4h", 100, 30)
        if closes is None:
            return 0, [], iv_rank

        roc_period = 14
        threshold = 5.0
        current_roc = (closes[-1] - closes[-1 - roc_period]) / closes[-1 - roc_period] * 100
        prev_roc = (closes[-2] - closes[-2 - roc_period]) / closes[-2 - roc_period] * 100

        signal = 0
        if current_roc > threshold and prev_roc <= threshold:
            signal = 1
        elif current_roc < -threshold and prev_roc >= -threshold:
            signal = -1

        actions = []
        if signal != 0:
            expiry_str, dte = adapter.get_real_expiry(underlying, 37)
            if signal == 1:
                actions.append(_option_leg(
                    underlying, "buy", "call", spot_price * 1.02,
                    expiry_str, dte, 0.045, spot_price, vol_annual, adapter,
                ))
            else:
                actions.append(_option_leg(
                    underlying, "buy", "put", spot_price * 0.98,
                    expiry_str, dte, 0.040, spot_price, vol_annual, adapter,
                ))

        return signal, actions, iv_rank

    except Exception as e:
        print(f"Momentum options evaluation failed: {e}", file=sys.stderr)
        traceback.print_exc(file=sys.stderr)
        return 0, [], iv_rank


def evaluate_vol_mean_reversion(underlying, spot_price, vol_annual, iv_rank,
                                 existing_positions, spot_positions, adapter):
    try:
        signal = 0
        actions = []
        expiry_str, dte = adapter.get_real_expiry(underlying, 30)
        pf = _platform_extra(adapter, underlying)

        if iv_rank > 75:
            signal = -1
            actions = [
                _option_leg(underlying, "sell", "call", spot_price * 1.10,
                            expiry_str, dte, 0.025, spot_price, vol_annual, adapter),
                _option_leg(underlying, "sell", "put", spot_price * 0.90,
                            expiry_str, dte, 0.020, spot_price, vol_annual, adapter),
            ]
        elif iv_rank < 25:
            signal = 1
            atm_strike = adapter.get_real_strike(underlying, expiry_str, "call", spot_price)
            call_pct, call_usd, call_greeks = adapter.get_premium_and_greeks(
                underlying, "call", atm_strike, expiry_str, dte, spot_price, vol_annual
            )
            put_pct, put_usd, put_greeks = adapter.get_premium_and_greeks(
                underlying, "put", atm_strike, expiry_str, dte, spot_price, vol_annual
            )
            actions = [
                _build_action("buy", "call", atm_strike, expiry_str, dte,
                              call_pct, call_usd, call_greeks, **pf),
                _build_action("buy", "put", atm_strike, expiry_str, dte,
                              put_pct, put_usd, put_greeks, **pf),
            ]

        return signal, actions, iv_rank

    except Exception as e:
        print(f"Vol mean reversion evaluation failed: {e}", file=sys.stderr)
        traceback.print_exc(file=sys.stderr)
        return 0, [], iv_rank


def evaluate_protective_puts(underlying, spot_price, vol_annual, iv_rank,
                              existing_positions, spot_positions, adapter):
    try:
        has_protective_put = any(
            p.get("option_type") == "put" and p.get("action") == "buy"
            for p in existing_positions
        )
        if has_protective_put:
            return 0, [], iv_rank

        expiry_str, dte = adapter.get_real_expiry(underlying, 45)
        actions = [
            _option_leg(underlying, "buy", "put", spot_price * 0.88,
                        expiry_str, dte, 0.015, spot_price, vol_annual, adapter),
        ]
        return 1, actions, iv_rank

    except Exception as e:
        print(f"Protective puts evaluation failed: {e}", file=sys.stderr)
        return 0, [], iv_rank


def evaluate_covered_calls(underlying, spot_price, vol_annual, iv_rank,
                            existing_positions, spot_positions, adapter):
    try:
        has_covered_call = any(
            p.get("option_type") == "call" and p.get("action") == "sell"
            for p in existing_positions
        )
        if has_covered_call:
            return 0, [], iv_rank

        expiry_str, dte = adapter.get_real_expiry(underlying, 21)
        actions = [
            _option_leg(underlying, "sell", "call", spot_price * 1.12,
                        expiry_str, dte, 0.020, spot_price, vol_annual, adapter),
        ]
        return -1, actions, iv_rank

    except Exception as e:
        print(f"Covered calls evaluation failed: {e}", file=sys.stderr)
        return 0, [], iv_rank


def evaluate_wheel(underlying, spot_price, vol_annual, iv_rank,
                   existing_positions, spot_positions, adapter):
    try:
        has_assigned_spot = any(
            p.get("symbol", "").upper() == underlying.upper()
            and p.get("side") == "long"
            and p.get("quantity", 0) > 0
            for p in spot_positions
        )

        if has_assigned_spot:
            has_active_call = any(
                p.get("option_type") == "call" and p.get("action") == "sell"
                for p in existing_positions
            )
            if has_active_call:
                return 0, [], iv_rank
            expiry_str, dte = adapter.get_real_expiry(underlying, 21)
            actions = [
                _option_leg(underlying, "sell", "call", spot_price * 1.10,
                            expiry_str, dte, 0.020, spot_price, vol_annual, adapter,
                            wheel_phase=2),
            ]
            return -1, actions, iv_rank
        else:
            has_wheel_put = any(
                p.get("option_type") == "put" and p.get("action") == "sell"
                for p in existing_positions
            )
            if has_wheel_put:
                return 0, [], iv_rank
            expiry_str, dte = adapter.get_real_expiry(underlying, 37)
            actions = [
                _option_leg(underlying, "sell", "put", spot_price * 0.94,
                            expiry_str, dte, 0.020, spot_price, vol_annual, adapter,
                            wheel_phase=1),
            ]
            return -1, actions, iv_rank

    except Exception as e:
        print(f"Wheel evaluation failed: {e}", file=sys.stderr)
        return 0, [], iv_rank


def evaluate_butterfly(underlying, spot_price, vol_annual, iv_rank,
                        existing_positions, spot_positions, adapter):
    try:
        if iv_rank < 30 or iv_rank > 70:
            return 0, [], iv_rank

        expiry_str, dte = adapter.get_real_expiry(underlying, 30)
        actions = [
            _option_leg(underlying, "buy", "call", spot_price * 0.95,
                        expiry_str, dte, 0.055, spot_price, vol_annual, adapter),
            _option_leg(underlying, "sell", "call", spot_price,
                        expiry_str, dte, 0.035, spot_price, vol_annual, adapter,
                        quantity=2),
            _option_leg(underlying, "buy", "call", spot_price * 1.05,
                        expiry_str, dte, 0.015, spot_price, vol_annual, adapter),
        ]
        return 1, actions, iv_rank

    except Exception as e:
        print(f"Butterfly evaluation failed: {e}", file=sys.stderr)
        return 0, [], iv_rank


STRATEGY_MAP = {
    "momentum_options": evaluate_momentum_options,
    "vol_mean_reversion": evaluate_vol_mean_reversion,
    "protective_puts": evaluate_protective_puts,
    "covered_calls": evaluate_covered_calls,
    "wheel": evaluate_wheel,
    "butterfly": evaluate_butterfly,
}


# ── main ──────────────────────────────────────────────────────────────────────

def main():
    # Parse args: strip --platform= before positional parsing
    args = sys.argv[1:]
    platform = "deribit"
    remaining = []
    for arg in args:
        if arg.startswith("--platform="):
            platform = arg.split("=", 1)[1]
        elif arg.startswith("--platform"):
            pass  # bare flag without value, ignore
        else:
            remaining.append(arg)

    if len(remaining) < 2:
        print(json.dumps({
            "error": f"Usage: {sys.argv[0]} <strategy> <underlying> [--platform=deribit|ibkr]"
        }))
        sys.exit(1)

    strategy_name = remaining[0]
    underlying = remaining[1].upper()

    # Parse existing positions from stdin or argv[2]
    raw_positions = []
    if len(remaining) > 2:
        try:
            raw_positions = json.loads(remaining[2])
        except (json.JSONDecodeError, ValueError):
            pass
    elif not sys.stdin.isatty():
        try:
            stdin_data = sys.stdin.read().strip()
            if stdin_data:
                raw_positions = json.loads(stdin_data)
        except (json.JSONDecodeError, ValueError):
            pass

    existing_positions, spot_positions = parse_positions_context(raw_positions)

    if len(existing_positions) >= MAX_POSITIONS_PER_STRATEGY:
        print(json.dumps({
            "strategy": strategy_name,
            "underlying": underlying,
            "signal": 0,
            "spot_price": 0,
            "actions": [],
            "iv_rank": 0,
            "platform": platform,
            "timestamp": datetime.now(timezone.utc).isoformat(),
            "skip_reason": f"Max positions reached ({len(existing_positions)}/{MAX_POSITIONS_PER_STRATEGY})"
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
            "platform": platform,
            "timestamp": datetime.now(timezone.utc).isoformat(),
            "error": f"Unknown strategy: {strategy_name}. Available: {list(STRATEGY_MAP.keys())}"
        }))
        return

    try:
        adapter = _load_adapter(platform)

        spot_price = adapter.get_spot_price(underlying)
        if spot_price <= 0:
            print(json.dumps({
                "strategy": strategy_name,
                "underlying": underlying,
                "signal": 0,
                "spot_price": 0,
                "actions": [],
                "iv_rank": 0,
                "platform": platform,
                "timestamp": datetime.now(timezone.utc).isoformat(),
                "error": "Could not fetch spot price"
            }))
            return

        vol_annual, iv_rank = adapter.get_vol_metrics(underlying)

        evaluate_fn = STRATEGY_MAP[strategy_name]
        signal, actions, iv_rank = evaluate_fn(
            underlying, spot_price, vol_annual, iv_rank,
            existing_positions, spot_positions, adapter
        )

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
            "iv_rank": round(iv_rank, 1),
            "platform": platform,
            "timestamp": datetime.now(timezone.utc).isoformat(),
        }
        # Include multiplier in top-level output for IBKR (informational)
        multiplier_fn = getattr(adapter, 'get_multiplier', None)
        if multiplier_fn is not None:
            output["multiplier"] = multiplier_fn(underlying)
            output["contract_type"] = "CME_MICRO"

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
            "platform": platform,
            "timestamp": datetime.now(timezone.utc).isoformat(),
            "error": str(e)
        }))
        sys.exit(1)


if __name__ == "__main__":
    main()
