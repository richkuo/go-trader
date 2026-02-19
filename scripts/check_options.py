#!/usr/bin/env python3
"""
Stateless options strategy check script.
Evaluates options strategies and outputs JSON to stdout.

Usage: python3 check_options.py <strategy> <underlying>
"""

import math
import sys
import json
import traceback
from datetime import datetime, timezone, timedelta

# Import Deribit utilities for real expiries and live quote (premium + Greeks)
try:
    from deribit_utils import find_closest_expiry, find_closest_strike, get_live_premium, get_live_quote
    USE_REAL_EXPIRIES = True
    USE_LIVE_PREMIUMS = True
except ImportError:
    print("Warning: deribit_utils not found, using synthetic expiries", file=sys.stderr)
    USE_REAL_EXPIRIES = False
    USE_LIVE_PREMIUMS = False


def get_spot_price(underlying):
    """Fetch current spot price for the underlying."""
    try:
        import ccxt
        exchange = ccxt.binanceus({"enableRateLimit": True})
        symbol = f"{underlying}/USDT"
        ticker = exchange.fetch_ticker(symbol)
        return ticker["last"]
    except Exception as e:
        print(f"Spot price fetch failed for {underlying}: {e}", file=sys.stderr)
        return 0


def get_real_expiry(underlying, target_dte):
    """
    Get real Deribit expiry closest to target DTE (within 7-day tolerance).
    Falls back to synthetic expiry if Deribit fetch fails or no expiry is within tolerance.
    """
    if USE_REAL_EXPIRIES:
        result = find_closest_expiry(underlying, target_dte)
        if result:
            expiry_str, actual_dte = result
            return expiry_str, actual_dte
    
    # Fallback to synthetic
    expiry_date = datetime.now(timezone.utc) + timedelta(days=target_dte)
    return expiry_date.strftime("%Y-%m-%d"), target_dte


def get_premium(underlying, option_type, strike, expiry_str, fallback_pct, spot_price):
    """Get live premium from Deribit, falling back to estimated pct of spot if unavailable."""
    if USE_LIVE_PREMIUMS:
        live = get_live_premium(underlying, option_type, strike, expiry_str)
        if live is not None:
            return live, round(live * spot_price, 2)
    return fallback_pct, round(fallback_pct * spot_price, 2)


def _norm_cdf(x):
    """Standard normal CDF (math only, no scipy)."""
    return 0.5 * (1 + math.erf(x / math.sqrt(2)))


def bs_greeks(spot, strike, dte_days, vol_annual, option_type):
    """
    Black-Scholes Greeks for fallback when live Deribit quote is unavailable.
    vol_annual: annualized volatility as decimal (e.g. 0.5 = 50%).
    Returns dict with delta, gamma, theta (per day), vega.
    """
    if dte_days <= 0 or vol_annual <= 0 or spot <= 0:
        return {"delta": 0.0, "gamma": 0.0, "theta": 0.0, "vega": 0.0}
    t = dte_days / 365.0
    sqrt_t = math.sqrt(t)
    d1 = (math.log(spot / strike) + (0.05 + 0.5 * vol_annual ** 2) * t) / (vol_annual * sqrt_t)
    d2 = d1 - vol_annual * sqrt_t
    pdf_d1 = math.exp(-0.5 * d1 ** 2) / math.sqrt(2 * math.pi)
    if option_type.lower() == "call":
        delta = _norm_cdf(d1)
    else:
        delta = _norm_cdf(d1) - 1
    gamma = pdf_d1 / (spot * vol_annual * sqrt_t) if (spot * vol_annual * sqrt_t) > 0 else 0.0
    vega = spot * pdf_d1 * sqrt_t / 100.0  # per 1% vol change
    decay = -(spot * pdf_d1 * vol_annual) / (2 * sqrt_t)
    rterm = 0.05 * strike * math.exp(-0.05 * t)
    if option_type.lower() == "call":
        theta_annual = decay - rterm * _norm_cdf(d2)
    else:
        theta_annual = decay + rterm * _norm_cdf(-d2)
    theta = theta_annual / 365.0  # daily
    return {
        "delta": round(delta, 4),
        "gamma": round(gamma, 6),
        "theta": round(theta, 2),
        "vega": round(vega, 2),
    }


def get_premium_and_greeks(underlying, option_type, strike, expiry_str, dte, fallback_pct, spot_price, vol_annual=None):
    """
    Get premium (pct in underlying terms, USD) and Greeks in one go.
    Uses live Deribit quote when available; otherwise fallback premium + BS-estimated Greeks.
    vol_annual: optional annualized vol for BS fallback (decimal); default 0.5 if None.
    """
    if USE_LIVE_PREMIUMS:
        quote = get_live_quote(underlying, option_type, strike, expiry_str)
        if quote:
            mark = quote["mark_price"]
            premium_usd = round(mark * spot_price, 2)
            return mark, premium_usd, quote["greeks"]
    # Live quote unavailable — use fallback premium and BS Greeks.
    # Skip the second API call inside get_premium() by computing fallback directly.
    vol = vol_annual if vol_annual is not None and vol_annual > 0 else 0.5
    greeks = bs_greeks(spot_price, strike, dte, vol, option_type)
    return fallback_pct, round(fallback_pct * spot_price, 2), greeks


def get_real_strike(underlying, expiry_str, option_type, target_strike):
    """
    Get real Deribit strike closest to target.
    Falls back to rounded target if Deribit fetch fails.
    """
    if USE_REAL_EXPIRIES:
        result = find_closest_strike(underlying, expiry_str, option_type, target_strike)
        if result:
            return result
    
    # Fallback: round to nearest 1000 for BTC, 50 for ETH
    if underlying == "BTC":
        return round(target_strike, -3)
    else:
        return round(target_strike / 50) * 50


def parse_positions_context(raw_positions):
    """
    Split the combined position list sent by Go into option and spot position lists.
    Spot entries carry position_type="spot"; option entries have no position_type field.
    """
    option_positions = []
    spot_positions = []
    for p in (raw_positions or []):
        if isinstance(p, dict) and p.get("position_type") == "spot":
            spot_positions.append(p)
        elif isinstance(p, dict):
            option_positions.append(p)
    return option_positions, spot_positions


def compute_iv_rank(returns, window=14):
    """
    Compute IV rank using a rolling historical-volatility approach.
    Standard formula: (current_HV - period_min_HV) / (period_max_HV - period_min_HV) * 100
    Falls back to ratio method when insufficient data for rolling windows.
    """
    if not returns or len(returns) < 2:
        return 50.0

    w = min(window, len(returns))

    def hv_annualised(r):
        n = len(r)
        if n < 1:
            return 0.0
        mean = sum(r) / n
        variance = sum((x - mean) ** 2 for x in r) / n
        return math.sqrt(variance) * math.sqrt(365) * 100

    recent_hv = hv_annualised(returns[-w:])

    if len(returns) >= 2 * w:
        hvs = [hv_annualised(returns[i:i + w]) for i in range(len(returns) - w + 1)]
        hv_min = min(hvs)
        hv_max = max(hvs)
        if hv_max > hv_min:
            rank = (recent_hv - hv_min) / (hv_max - hv_min) * 100
            return round(min(max(rank, 0.0), 100.0), 1)

    # Fallback: ratio vs full-period HV
    hist_hv = hv_annualised(returns)
    return round(min(max((recent_hv / max(hist_hv, 0.001)) * 50, 0.0), 100.0), 1)


# ─────────────────────────────────────────────
# Shared evaluation helpers
# ─────────────────────────────────────────────

def _fetch_ohlcv_closes(underlying, timeframe, limit, min_len):
    """
    Fetch OHLCV from Binance US and return a list of closing prices.
    Returns None when data is missing or shorter than min_len.
    Raises ccxt or network exceptions for the caller's except block to catch.
    """
    import ccxt
    exchange = ccxt.binanceus({"enableRateLimit": True})
    ohlcv = exchange.fetch_ohlcv(f"{underlying}/USDT", timeframe, limit=limit)
    if not ohlcv or len(ohlcv) < min_len:
        return None
    return [c[4] for c in ohlcv]


def _price_returns(closes):
    """Compute simple percentage returns from a list of closing prices."""
    return [(closes[i] - closes[i - 1]) / closes[i - 1] for i in range(1, len(closes))]


def _vol_metrics(returns, window=14, safe_window=True):
    """
    Compute annualised volatility fraction and IV rank from a return series.

    safe_window: when True (default), guards the window count against return
      lists shorter than `window`.  Pass False for vol_mean_reversion, which
      intentionally uses a fixed /14 divisor matching its original formula.

    Returns (vol_annual, iv_rank).
    """
    divisor = max(len(returns[-window:]), 1) if safe_window else window
    vol_annual = math.sqrt(sum(r ** 2 for r in returns[-window:]) / divisor) * math.sqrt(365)
    iv_rank = compute_iv_rank(returns)
    return vol_annual, iv_rank


def _build_action(action, option_type, strike, expiry_str, dte,
                  premium_pct, premium_usd, greeks, **extra):
    """Construct a standard option action dict, merging any strategy-specific extra fields."""
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
                expiry_str, dte, fallback_pct, spot_price, vol_annual, **extra):
    """
    Look up the nearest real strike, fetch premium + Greeks, and return an action dict.
    Passes **extra straight through to _build_action (e.g. quantity=2, wheel_phase=1).
    """
    strike = get_real_strike(underlying, expiry_str, option_type, target_strike)
    prem_pct, prem_usd, greeks = get_premium_and_greeks(
        underlying, option_type, strike, expiry_str, dte, fallback_pct, spot_price, vol_annual
    )
    return _build_action(action, option_type, strike, expiry_str, dte,
                         prem_pct, prem_usd, greeks, **extra)


# ─────────────────────────────────────────────
# Strategy evaluators
# ─────────────────────────────────────────────

def evaluate_momentum_options(underlying, spot_price, existing_positions=None):
    """
    Momentum-based options strategy.
    Uses ROC on 4h candles to determine direction, suggests calls/puts.
    """
    try:
        closes = _fetch_ohlcv_closes(underlying, "4h", 100, 30)
        if closes is None:
            return 0, [], 0

        roc_period = 14
        threshold = 5.0
        current_roc = (closes[-1] - closes[-1 - roc_period]) / closes[-1 - roc_period] * 100
        prev_roc = (closes[-2] - closes[-2 - roc_period]) / closes[-2 - roc_period] * 100

        signal = 0
        if current_roc > threshold and prev_roc <= threshold:
            signal = 1
        elif current_roc < -threshold and prev_roc >= -threshold:
            signal = -1

        returns = _price_returns(closes)
        vol_annual, iv_rank = _vol_metrics(returns)

        actions = []
        if signal != 0:
            expiry_str, dte = get_real_expiry(underlying, 37)
            if signal == 1:
                actions.append(_option_leg(
                    underlying, "buy", "call", spot_price * 1.02,
                    expiry_str, dte, 0.045, spot_price, vol_annual,
                ))
            else:
                actions.append(_option_leg(
                    underlying, "buy", "put", spot_price * 0.98,
                    expiry_str, dte, 0.040, spot_price, vol_annual,
                ))

        return signal, actions, round(iv_rank, 1)

    except Exception as e:
        print(f"Momentum options evaluation failed: {e}", file=sys.stderr)
        traceback.print_exc(file=sys.stderr)
        return 0, [], 0


def evaluate_vol_mean_reversion(underlying, spot_price, existing_positions=None):
    """
    Volatility mean reversion strategy.
    High IV → sell premium, Low IV → buy straddles.
    """
    try:
        closes = _fetch_ohlcv_closes(underlying, "1d", 90, 30)
        if closes is None:
            return 0, [], 0

        returns = _price_returns(closes)
        # safe_window=False preserves the original /14 fixed divisor for this strategy.
        vol_annual, iv_rank = _vol_metrics(returns, safe_window=False)

        signal = 0
        actions = []
        expiry_str, dte = get_real_expiry(underlying, 30)

        if iv_rank > 75:
            # High IV → sell strangle
            signal = -1
            actions = [
                _option_leg(underlying, "sell", "call", spot_price * 1.10,
                            expiry_str, dte, 0.025, spot_price, vol_annual),
                _option_leg(underlying, "sell", "put", spot_price * 0.90,
                            expiry_str, dte, 0.020, spot_price, vol_annual),
            ]
        elif iv_rank < 25:
            # Low IV → buy straddle (both legs share the same ATM strike)
            signal = 1
            atm_strike = get_real_strike(underlying, expiry_str, "call", spot_price)
            call_pct, call_usd, call_greeks = get_premium_and_greeks(
                underlying, "call", atm_strike, expiry_str, dte, 0.035, spot_price, vol_annual
            )
            put_pct, put_usd, put_greeks = get_premium_and_greeks(
                underlying, "put", atm_strike, expiry_str, dte, 0.030, spot_price, vol_annual
            )
            actions = [
                _build_action("buy", "call", atm_strike, expiry_str, dte,
                              call_pct, call_usd, call_greeks),
                _build_action("buy", "put", atm_strike, expiry_str, dte,
                              put_pct, put_usd, put_greeks),
            ]

        return signal, actions, round(iv_rank, 1)

    except Exception as e:
        print(f"Vol mean reversion evaluation failed: {e}", file=sys.stderr)
        traceback.print_exc(file=sys.stderr)
        return 0, [], 0


def evaluate_protective_puts(underlying, spot_price, existing_positions=None):
    """
    Protective puts — buy OTM puts to hedge spot holdings.
    Buys 10-15% OTM puts, 30-60 DTE, limits hedge cost to 2% of capital/month.
    """
    if existing_positions is None:
        existing_positions = []
    try:
        closes = _fetch_ohlcv_closes(underlying, "1d", 30, 10)
        if closes is None:
            return 0, [], 0

        returns = _price_returns(closes)
        vol_annual, iv_rank = _vol_metrics(returns)

        # Only buy if not already holding a protective put
        has_protective_put = any(
            p.get("option_type") == "put" and p.get("action") == "buy"
            for p in existing_positions
        )
        if has_protective_put:
            return 0, [], round(iv_rank, 1)

        signal = 1
        expiry_str, dte = get_real_expiry(underlying, 45)
        actions = [
            _option_leg(underlying, "buy", "put", spot_price * 0.88,
                        expiry_str, dte, 0.015, spot_price, vol_annual),
        ]

        return signal, actions, round(iv_rank, 1)

    except Exception as e:
        print(f"Protective puts evaluation failed: {e}", file=sys.stderr)
        return 0, [], 0


def evaluate_covered_calls(underlying, spot_price, existing_positions=None):
    """
    Covered calls — sell OTM calls for income on holdings.
    Sells 10-15% OTM calls, 14-30 DTE, targets 2-4% premium/month.
    """
    if existing_positions is None:
        existing_positions = []
    try:
        closes = _fetch_ohlcv_closes(underlying, "1d", 30, 10)
        if closes is None:
            return 0, [], 0

        returns = _price_returns(closes)
        vol_annual, iv_rank = _vol_metrics(returns)

        # Only sell if not already holding a covered call
        has_covered_call = any(
            p.get("option_type") == "call" and p.get("action") == "sell"
            for p in existing_positions
        )
        if has_covered_call:
            return 0, [], round(iv_rank, 1)

        # Sell covered calls — better when IV is higher
        signal = -1
        expiry_str, dte = get_real_expiry(underlying, 21)
        actions = [
            _option_leg(underlying, "sell", "call", spot_price * 1.12,
                        expiry_str, dte, 0.020, spot_price, vol_annual),
        ]

        return signal, actions, round(iv_rank, 1)

    except Exception as e:
        print(f"Covered calls evaluation failed: {e}", file=sys.stderr)
        return 0, [], 0


def evaluate_wheel(underlying, spot_price, existing_positions=None, spot_positions=None):
    """
    Wheel strategy — full lifecycle:
    Phase 1 (no spot holdings): Sell cash-secured OTM puts to collect premium.
    Phase 2 (spot holdings from assignment): Sell OTM covered calls against the holding.
    Transitions back to Phase 1 once calls expire or are called away.
    """
    if existing_positions is None:
        existing_positions = []
    if spot_positions is None:
        spot_positions = []
    try:
        closes = _fetch_ohlcv_closes(underlying, "1d", 30, 10)
        if closes is None:
            return 0, [], 0

        returns = _price_returns(closes)
        vol_annual, iv_rank = _vol_metrics(returns)

        # Detect whether we have spot holdings from a prior put assignment.
        has_assigned_spot = any(
            p.get("symbol", "").upper() == underlying.upper()
            and p.get("side") == "long"
            and p.get("quantity", 0) > 0
            for p in spot_positions
        )

        if has_assigned_spot:
            # Phase 2: sell covered call against the spot position.
            has_active_call = any(
                p.get("option_type") == "call" and p.get("action") == "sell"
                for p in existing_positions
            )
            if has_active_call:
                return 0, [], round(iv_rank, 1)

            expiry_str, dte = get_real_expiry(underlying, 21)
            actions = [
                _option_leg(underlying, "sell", "call", spot_price * 1.10,
                            expiry_str, dte, 0.020, spot_price, vol_annual,
                            wheel_phase=2),
            ]
            return -1, actions, round(iv_rank, 1)

        else:
            # Phase 1: sell cash-secured put.
            has_wheel_put = any(
                p.get("option_type") == "put" and p.get("action") == "sell"
                for p in existing_positions
            )
            if has_wheel_put:
                return 0, [], round(iv_rank, 1)

            expiry_str, dte = get_real_expiry(underlying, 37)
            actions = [
                _option_leg(underlying, "sell", "put", spot_price * 0.94,
                            expiry_str, dte, 0.020, spot_price, vol_annual,
                            wheel_phase=1),
            ]
            return -1, actions, round(iv_rank, 1)

    except Exception as e:
        print(f"Wheel evaluation failed: {e}", file=sys.stderr)
        return 0, [], 0


def evaluate_butterfly(underlying, spot_price, existing_positions=None):
    """
    Butterfly spread — neutral strategy that profits from low volatility.
    Structure: Buy 1 ITM, Sell 2 ATM, Buy 1 OTM (calls or puts).
    
    Max profit when price stays at middle strike at expiry.
    Limited risk = net debit paid.
    
    Best when expecting price to trade in a range (low volatility).
    """
    try:
        closes = _fetch_ohlcv_closes(underlying, "1d", 30, 10)
        if closes is None:
            return 0, [], 0

        returns = _price_returns(closes)
        vol_annual, iv_rank = _vol_metrics(returns)

        # Only trade butterfly when volatility is moderate (not too high, not too low)
        # High IV = expensive to buy wings, Low IV = not enough premium
        if iv_rank < 30 or iv_rank > 70:
            return 0, [], round(iv_rank, 1)

        # Butterfly setup: ±5% wing width, 30 DTE
        signal = 1  # Neutral (buying butterfly)
        expiry_str, dte = get_real_expiry(underlying, 30)

        actions = [
            _option_leg(underlying, "buy", "call", spot_price * 0.95,
                        expiry_str, dte, 0.055, spot_price, vol_annual),
            _option_leg(underlying, "sell", "call", spot_price,
                        expiry_str, dte, 0.035, spot_price, vol_annual,
                        quantity=2),
            _option_leg(underlying, "buy", "call", spot_price * 1.05,
                        expiry_str, dte, 0.015, spot_price, vol_annual),
        ]

        return signal, actions, round(iv_rank, 1)

    except Exception as e:
        print(f"Butterfly evaluation failed: {e}", file=sys.stderr)
        return 0, [], 0


STRATEGY_MAP = {
    "momentum_options": evaluate_momentum_options,
    "vol_mean_reversion": evaluate_vol_mean_reversion,
    "protective_puts": evaluate_protective_puts,
    "covered_calls": evaluate_covered_calls,
    "wheel": evaluate_wheel,
    "butterfly": evaluate_butterfly,
}


MAX_POSITIONS_PER_STRATEGY = 4
MIN_SCORE_THRESHOLD = 0.3


def score_new_trade(proposed_action, existing_positions, spot_price):
    """
    Score a proposed trade against existing positions.
    Returns a score from 0.0 (don't trade) to 1.0+ (great trade).

    Factors:
    - Strike distance from existing positions (farther = more diversified)
    - Expiry spread (different expiries = better)
    - Greek concentration (adding to existing skew = bad)
    - Premium efficiency (more premium for same risk = good)
    """
    if not existing_positions:
        return 1.0, "first position"

    score = 0.5  # base score for having room
    reasons = []

    p_strike = proposed_action.get("strike", 0)
    p_expiry = proposed_action.get("expiry", "")
    p_type = proposed_action.get("option_type", "")
    p_delta = proposed_action.get("greeks", {}).get("delta", 0)

    # 1. Strike distance bonus (0 to +0.4)
    same_type_positions = [p for p in existing_positions if p.get("option_type") == p_type]
    if same_type_positions and spot_price > 0:
        min_strike_dist = min(
            abs(p_strike - p["strike"]) / spot_price
            for p in same_type_positions
        )
        if min_strike_dist > 0.10:  # >10% apart
            score += 0.4
            reasons.append(f"strike distance {min_strike_dist:.1%}")
        elif min_strike_dist > 0.05:  # 5-10% apart
            score += 0.2
            reasons.append(f"moderate strike distance {min_strike_dist:.1%}")
        else:  # <5% apart — basically same strike
            score -= 0.3
            reasons.append(f"overlapping strikes {min_strike_dist:.1%}")

    # 2. Expiry spread bonus (0 to +0.3)
    existing_expiries = set(p.get("expiry", "") for p in existing_positions)
    if p_expiry not in existing_expiries:
        score += 0.3
        reasons.append("different expiry")
    else:
        score -= 0.1
        reasons.append("same expiry")

    # 3. Greek concentration penalty (0 to -0.3)
    net_delta = sum(p.get("delta", 0) for p in existing_positions)
    # If adding this trade pushes delta further from zero, penalize
    new_net_delta = net_delta + p_delta
    if abs(new_net_delta) > abs(net_delta) and abs(new_net_delta) > 0.5:
        score -= 0.3
        reasons.append(f"delta concentration {new_net_delta:+.2f}")
    elif abs(new_net_delta) < abs(net_delta):
        score += 0.2
        reasons.append(f"delta balancing {new_net_delta:+.2f}")

    # 4. Premium efficiency (+0.1 if collecting more per unit risk)
    if proposed_action.get("action") == "sell":
        avg_existing_premium = 0
        sell_positions = [p for p in existing_positions if p.get("action") == "sell"]
        if sell_positions:
            avg_existing_premium = sum(p.get("entry_premium_usd", 0) for p in sell_positions) / len(sell_positions)
            if proposed_action.get("premium_usd", 0) > avg_existing_premium * 1.1:
                score += 0.1
                reasons.append("better premium")

    return round(score, 2), "; ".join(reasons) if reasons else "default"


def main():
    if len(sys.argv) < 3:
        print(json.dumps({
            "error": f"Usage: {sys.argv[0]} <strategy> <underlying> [positions_json]"
        }))
        sys.exit(1)

    strategy_name = sys.argv[1]
    underlying = sys.argv[2].upper()

    # Parse existing positions from Go scheduler.
    # Prefer stdin (avoids /proc/pid/cmdline leakage); fall back to argv[3] for manual testing.
    raw_positions = []
    if len(sys.argv) > 3:
        try:
            raw_positions = json.loads(sys.argv[3])
        except (json.JSONDecodeError, ValueError):
            pass
    elif not sys.stdin.isatty():
        try:
            stdin_data = sys.stdin.read().strip()
            if stdin_data:
                raw_positions = json.loads(stdin_data)
        except (json.JSONDecodeError, ValueError):
            pass

    # Split combined Go payload into option and spot positions.
    # Cap check counts only option positions; spot holdings don't consume strategy slots.
    existing_positions, spot_positions = parse_positions_context(raw_positions)

    # Hard cap check (option positions only)
    if len(existing_positions) >= MAX_POSITIONS_PER_STRATEGY:
        print(json.dumps({
            "strategy": strategy_name,
            "underlying": underlying,
            "signal": 0,
            "spot_price": 0,
            "actions": [],
            "iv_rank": 0,
            "timestamp": datetime.now(timezone.utc).isoformat(),
            "skip_reason": f"Max positions reached ({len(existing_positions)}/{MAX_POSITIONS_PER_STRATEGY})"
        }))
        return

    try:
        if strategy_name not in STRATEGY_MAP:
            print(json.dumps({
                "strategy": strategy_name,
                "underlying": underlying,
                "signal": 0,
                "spot_price": 0,
                "actions": [],
                "iv_rank": 0,
                "timestamp": datetime.now(timezone.utc).isoformat(),
                "error": f"Unknown strategy: {strategy_name}. Available: {list(STRATEGY_MAP.keys())}"
            }))
            return

        spot_price = get_spot_price(underlying)
        if spot_price <= 0:
            print(json.dumps({
                "strategy": strategy_name,
                "underlying": underlying,
                "signal": 0,
                "spot_price": 0,
                "actions": [],
                "iv_rank": 0,
                "timestamp": datetime.now(timezone.utc).isoformat(),
                "error": "Could not fetch spot price"
            }))
            return

        evaluate_fn = STRATEGY_MAP[strategy_name]
        # Wheel receives spot_positions to detect phase (put-sell vs covered-call).
        if strategy_name == "wheel":
            signal, actions, iv_rank = evaluate_fn(underlying, spot_price, existing_positions, spot_positions)
        else:
            signal, actions, iv_rank = evaluate_fn(underlying, spot_price, existing_positions)

        # Score each proposed action against option positions only
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

        # If all actions were filtered out, signal becomes hold
        if actions and not scored_actions:
            signal = 0

        output = {
            "strategy": strategy_name,
            "underlying": underlying,
            "signal": signal,
            "spot_price": round(spot_price, 2),
            "actions": scored_actions,
            "iv_rank": iv_rank,
            "timestamp": datetime.now(timezone.utc).isoformat()
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
            "timestamp": datetime.now(timezone.utc).isoformat(),
            "error": str(e)
        }))
        sys.exit(1)  # Exit 1; Go will still parse the JSON error field


if __name__ == "__main__":
    main()
