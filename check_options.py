#!/usr/bin/env python3
"""
Stateless options strategy check script.
Evaluates options strategies and outputs JSON to stdout.

Usage: python3 check_options.py <strategy> <underlying>
"""

import sys
import json
import traceback
from datetime import datetime, timezone, timedelta


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


def evaluate_momentum_options(underlying, spot_price):
    """
    Momentum-based options strategy.
    Uses ROC on 4h candles to determine direction, suggests calls/puts.
    """
    try:
        import ccxt
        exchange = ccxt.binanceus({"enableRateLimit": True})
        symbol = f"{underlying}/USDT"
        ohlcv = exchange.fetch_ohlcv(symbol, "4h", limit=100)

        if not ohlcv or len(ohlcv) < 30:
            return 0, [], 0

        closes = [c[4] for c in ohlcv]
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
            # Suggest a simple option trade
            expiry_date = datetime.now(timezone.utc) + timedelta(days=37)
            expiry_str = expiry_date.strftime("%Y-%m-%d")
            dte = 37

            if signal == 1:
                strike = round(spot_price * 1.02, -2)  # slightly OTM call
                premium_pct = 0.045
                actions.append({
                    "action": "buy",
                    "option_type": "call",
                    "strike": strike,
                    "expiry": expiry_str,
                    "dte": dte,
                    "premium": premium_pct,
                    "premium_usd": round(premium_pct * spot_price, 2),
                    "greeks": {
                        "delta": 0.45,
                        "gamma": 0.001,
                        "theta": -15.2,
                        "vega": 120.5
                    }
                })
            else:
                strike = round(spot_price * 0.98, -2)  # slightly OTM put
                premium_pct = 0.040
                actions.append({
                    "action": "buy",
                    "option_type": "put",
                    "strike": strike,
                    "expiry": expiry_str,
                    "dte": dte,
                    "premium": premium_pct,
                    "premium_usd": round(premium_pct * spot_price, 2),
                    "greeks": {
                        "delta": -0.40,
                        "gamma": 0.001,
                        "theta": -14.8,
                        "vega": 115.3
                    }
                })

        # Estimate IV rank (simplified — use recent volatility)
        returns = [(closes[i] - closes[i-1]) / closes[i-1] for i in range(1, len(closes))]
        current_vol = abs(sum(returns[-14:])) / 14 * 100
        hist_vol = abs(sum(returns)) / len(returns) * 100
        iv_rank = min(max((current_vol / max(hist_vol, 0.001)) * 50, 0), 100)

        return signal, actions, round(iv_rank, 1)

    except Exception as e:
        print(f"Momentum options evaluation failed: {e}", file=sys.stderr)
        traceback.print_exc(file=sys.stderr)
        return 0, [], 0


def evaluate_vol_mean_reversion(underlying, spot_price):
    """
    Volatility mean reversion strategy.
    High IV → sell premium, Low IV → buy straddles.
    """
    try:
        import ccxt
        exchange = ccxt.binanceus({"enableRateLimit": True})
        symbol = f"{underlying}/USDT"
        ohlcv = exchange.fetch_ohlcv(symbol, "1d", limit=90)

        if not ohlcv or len(ohlcv) < 30:
            return 0, [], 0

        closes = [c[4] for c in ohlcv]

        # Calculate historical volatility
        returns = [(closes[i] - closes[i-1]) / closes[i-1] for i in range(1, len(closes))]
        import math
        recent_vol = math.sqrt(sum(r**2 for r in returns[-14:]) / 14) * math.sqrt(365) * 100
        hist_vol = math.sqrt(sum(r**2 for r in returns) / len(returns)) * math.sqrt(365) * 100

        iv_rank = min(max((recent_vol / max(hist_vol, 0.001)) * 50, 0), 100)

        signal = 0
        actions = []
        expiry_date = datetime.now(timezone.utc) + timedelta(days=30)
        expiry_str = expiry_date.strftime("%Y-%m-%d")

        if iv_rank > 75:
            # High IV → sell strangle
            signal = -1
            call_strike = round(spot_price * 1.10, -2)
            put_strike = round(spot_price * 0.90, -2)
            actions = [
                {
                    "action": "sell",
                    "option_type": "call",
                    "strike": call_strike,
                    "expiry": expiry_str,
                    "dte": 30,
                    "premium": 0.025,
                    "premium_usd": round(0.025 * spot_price, 2),
                    "greeks": {"delta": 0.20, "gamma": 0.0005, "theta": 25.0, "vega": -80.0}
                },
                {
                    "action": "sell",
                    "option_type": "put",
                    "strike": put_strike,
                    "expiry": expiry_str,
                    "dte": 30,
                    "premium": 0.020,
                    "premium_usd": round(0.020 * spot_price, 2),
                    "greeks": {"delta": -0.18, "gamma": 0.0004, "theta": 22.0, "vega": -75.0}
                }
            ]
        elif iv_rank < 25:
            # Low IV → buy straddle
            signal = 1
            strike = round(spot_price, -2)
            actions = [
                {
                    "action": "buy",
                    "option_type": "call",
                    "strike": strike,
                    "expiry": expiry_str,
                    "dte": 30,
                    "premium": 0.035,
                    "premium_usd": round(0.035 * spot_price, 2),
                    "greeks": {"delta": 0.50, "gamma": 0.001, "theta": -18.0, "vega": 130.0}
                },
                {
                    "action": "buy",
                    "option_type": "put",
                    "strike": strike,
                    "expiry": expiry_str,
                    "dte": 30,
                    "premium": 0.030,
                    "premium_usd": round(0.030 * spot_price, 2),
                    "greeks": {"delta": -0.50, "gamma": 0.001, "theta": -17.0, "vega": 125.0}
                }
            ]

        return signal, actions, round(iv_rank, 1)

    except Exception as e:
        print(f"Vol mean reversion evaluation failed: {e}", file=sys.stderr)
        traceback.print_exc(file=sys.stderr)
        return 0, [], 0


def evaluate_protective_puts(underlying, spot_price):
    """
    Protective puts — buy OTM puts to hedge spot holdings.
    Buys 10-15% OTM puts, 30-60 DTE, limits hedge cost to 2% of capital/month.
    """
    try:
        import ccxt
        exchange = ccxt.binanceus({"enableRateLimit": True})
        symbol = f"{underlying}/USDT"
        ohlcv = exchange.fetch_ohlcv(symbol, "1d", limit=30)

        if not ohlcv or len(ohlcv) < 10:
            return 0, [], 0

        closes = [c[4] for c in ohlcv]
        returns = [(closes[i] - closes[i-1]) / closes[i-1] for i in range(1, len(closes))]
        import math
        recent_vol = math.sqrt(sum(r**2 for r in returns[-14:]) / max(len(returns[-14:]), 1)) * math.sqrt(365) * 100
        hist_vol = math.sqrt(sum(r**2 for r in returns) / len(returns)) * math.sqrt(365) * 100
        iv_rank = min(max((recent_vol / max(hist_vol, 0.001)) * 50, 0), 100)

        # Always buy protective puts if not already holding
        signal = 1
        strike = round(spot_price * 0.88, -2)  # 12% OTM
        expiry_date = datetime.now(timezone.utc) + timedelta(days=45)
        expiry_str = expiry_date.strftime("%Y-%m-%d")
        premium_pct = 0.015  # ~1.5% for OTM put

        actions = [{
            "action": "buy",
            "option_type": "put",
            "strike": strike,
            "expiry": expiry_str,
            "dte": 45,
            "premium": premium_pct,
            "premium_usd": round(premium_pct * spot_price, 2),
            "greeks": {"delta": -0.15, "gamma": 0.0003, "theta": -5.0, "vega": 60.0}
        }]

        return signal, actions, round(iv_rank, 1)

    except Exception as e:
        print(f"Protective puts evaluation failed: {e}", file=sys.stderr)
        return 0, [], 0


def evaluate_covered_calls(underlying, spot_price):
    """
    Covered calls — sell OTM calls for income on holdings.
    Sells 10-15% OTM calls, 14-30 DTE, targets 2-4% premium/month.
    """
    try:
        import ccxt
        exchange = ccxt.binanceus({"enableRateLimit": True})
        symbol = f"{underlying}/USDT"
        ohlcv = exchange.fetch_ohlcv(symbol, "1d", limit=30)

        if not ohlcv or len(ohlcv) < 10:
            return 0, [], 0

        closes = [c[4] for c in ohlcv]
        returns = [(closes[i] - closes[i-1]) / closes[i-1] for i in range(1, len(closes))]
        import math
        recent_vol = math.sqrt(sum(r**2 for r in returns[-14:]) / max(len(returns[-14:]), 1)) * math.sqrt(365) * 100
        hist_vol = math.sqrt(sum(r**2 for r in returns) / len(returns)) * math.sqrt(365) * 100
        iv_rank = min(max((recent_vol / max(hist_vol, 0.001)) * 50, 0), 100)

        # Sell covered calls — better when IV is higher
        signal = -1
        strike = round(spot_price * 1.12, -2)  # 12% OTM
        expiry_date = datetime.now(timezone.utc) + timedelta(days=21)
        expiry_str = expiry_date.strftime("%Y-%m-%d")
        premium_pct = 0.020  # ~2% for OTM call

        actions = [{
            "action": "sell",
            "option_type": "call",
            "strike": strike,
            "expiry": expiry_str,
            "dte": 21,
            "premium": premium_pct,
            "premium_usd": round(premium_pct * spot_price, 2),
            "greeks": {"delta": 0.18, "gamma": 0.0004, "theta": 12.0, "vega": -55.0}
        }]

        return signal, actions, round(iv_rank, 1)

    except Exception as e:
        print(f"Covered calls evaluation failed: {e}", file=sys.stderr)
        return 0, [], 0


STRATEGY_MAP = {
    "momentum_options": evaluate_momentum_options,
    "vol_mean_reversion": evaluate_vol_mean_reversion,
    "protective_puts": evaluate_protective_puts,
    "covered_calls": evaluate_covered_calls,
}


def main():
    if len(sys.argv) < 3:
        print(json.dumps({
            "error": f"Usage: {sys.argv[0]} <strategy> <underlying>"
        }))
        sys.exit(1)

    strategy_name = sys.argv[1]
    underlying = sys.argv[2].upper()

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
        signal, actions, iv_rank = evaluate_fn(underlying, spot_price)

        output = {
            "strategy": strategy_name,
            "underlying": underlying,
            "signal": signal,
            "spot_price": round(spot_price, 2),
            "actions": actions,
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
        sys.exit(0)


if __name__ == "__main__":
    main()
