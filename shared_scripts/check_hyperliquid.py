#!/usr/bin/env python3
"""
Hyperliquid perps strategy check script.
Fetches OHLCV from Hyperliquid, runs strategy, outputs JSON to stdout, exits.

Signal check mode (paper or live):
    check_hyperliquid.py <strategy> <symbol> <timeframe> [--mode=paper|live]

Execution mode (live only, called by Go as phase 2):
    check_hyperliquid.py --execute --symbol=BTC --side=buy|sell --size=0.01 [--mode=live]
"""

import sys
import os
import json
import traceback
from datetime import datetime, timezone

# Add paths: platforms/hyperliquid/ directly (avoids naming conflict with hyperliquid SDK),
# shared_strategies/spot/ for apply_strategy, shared_tools/ for utilities.
sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'platforms', 'hyperliquid'))
sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'shared_strategies', 'spot'))
sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'shared_tools'))


def _make_dataframe(candles):
    """Convert raw OHLCV list to pandas DataFrame compatible with strategy functions."""
    import pandas as pd
    df = pd.DataFrame(candles, columns=["timestamp", "open", "high", "low", "close", "volume"])
    df["datetime"] = pd.to_datetime(df["timestamp"], unit="ms", utc=True)
    df = df.set_index("datetime")
    df.sort_index(inplace=True)
    return df


def run_signal_check(strategy_name, symbol, timeframe, mode):
    """Run strategy signal check using Hyperliquid OHLCV data."""
    try:
        from adapter import HyperliquidExchangeAdapter
        from strategies import apply_strategy, get_strategy

        # Verify strategy exists early
        get_strategy(strategy_name)

        adapter = HyperliquidExchangeAdapter()

        print(f"Fetching {symbol} {timeframe} from Hyperliquid ({mode})...", file=sys.stderr)
        candles = adapter.get_ohlcv(symbol, interval=timeframe, limit=200)

        if not candles or len(candles) < 30:
            print(json.dumps({
                "strategy": strategy_name,
                "symbol": symbol,
                "timeframe": timeframe,
                "signal": 0,
                "price": 0,
                "indicators": {},
                "mode": mode,
                "platform": "hyperliquid",
                "timestamp": datetime.now(timezone.utc).isoformat(),
                "error": f"Insufficient data: {len(candles) if candles else 0} candles",
            }))
            sys.exit(1)

        df = _make_dataframe(candles)
        result_df = apply_strategy(strategy_name, df)

        last = result_df.iloc[-1]
        signal = int(last.get("signal", 0))
        if signal > 0:
            signal = 1
        elif signal < 0:
            signal = -1
        else:
            signal = 0

        price = float(last["close"])

        # Freshen price with live mid if available
        try:
            mid = adapter.get_spot_price(symbol)
            if mid > 0:
                price = mid
        except Exception:
            pass

        indicators = {}
        skip_cols = {
            "open", "high", "low", "close", "volume",
            "timestamp", "signal", "position", "datetime",
        }
        for col in result_df.columns:
            if col in skip_cols:
                continue
            val = last.get(col)
            if val is not None:
                try:
                    indicators[col] = round(float(val), 6)
                except (ValueError, TypeError):
                    pass

        print(json.dumps({
            "strategy": strategy_name,
            "symbol": symbol,
            "timeframe": timeframe,
            "signal": signal,
            "price": round(price, 2),
            "indicators": indicators,
            "mode": mode,
            "platform": "hyperliquid",
            "timestamp": datetime.now(timezone.utc).isoformat(),
        }))

    except Exception as e:
        traceback.print_exc(file=sys.stderr)
        print(json.dumps({
            "strategy": strategy_name,
            "symbol": symbol,
            "timeframe": timeframe,
            "signal": 0,
            "price": 0,
            "indicators": {},
            "mode": mode,
            "platform": "hyperliquid",
            "timestamp": datetime.now(timezone.utc).isoformat(),
            "error": str(e),
        }))
        sys.exit(1)


def run_execute(symbol, side, size, mode):
    """Place a live market order on Hyperliquid."""
    if mode != "live":
        print(json.dumps({"error": "--execute requires --mode=live"}))
        sys.exit(1)

    try:
        from adapter import HyperliquidExchangeAdapter
        adapter = HyperliquidExchangeAdapter()

        is_buy = side.lower() == "buy"
        result = adapter.market_open(symbol, is_buy, size)

        # Extract fill info from SDK response structure:
        # {"status": "ok", "response": {"type": "order", "data": {"statuses": [...]}}}
        fill = {}
        try:
            statuses = result.get("response", {}).get("data", {}).get("statuses", [])
            if statuses:
                filled = statuses[0].get("filled", {})
                fill = {
                    "avg_px": float(filled.get("avgPx", 0) or 0),
                    "total_sz": float(filled.get("totalSz", 0) or 0),
                }
        except Exception:
            pass

        print(json.dumps({
            "execution": {
                "action": "buy" if is_buy else "sell",
                "symbol": symbol,
                "size": size,
                "fill": fill,
            },
            "platform": "hyperliquid",
            "timestamp": datetime.now(timezone.utc).isoformat(),
        }))

    except Exception as e:
        traceback.print_exc(file=sys.stderr)
        print(json.dumps({
            "execution": None,
            "platform": "hyperliquid",
            "timestamp": datetime.now(timezone.utc).isoformat(),
            "error": str(e),
        }))
        sys.exit(1)


def main():
    if "--execute" in sys.argv:
        # Execute mode: --execute --symbol=BTC --side=buy|sell --size=0.01 [--mode=live]
        import argparse
        parser = argparse.ArgumentParser()
        parser.add_argument("--execute", action="store_true")
        parser.add_argument("--symbol", required=True)
        parser.add_argument("--side", required=True, choices=["buy", "sell"])
        parser.add_argument("--size", type=float, required=True)
        parser.add_argument("--mode", default="live")
        args = parser.parse_args()
        run_execute(args.symbol, args.side, args.size, args.mode)
    else:
        # Signal check mode: <strategy> <symbol> <timeframe> [--mode=paper|live]
        import argparse
        parser = argparse.ArgumentParser()
        parser.add_argument("strategy")
        parser.add_argument("symbol")
        parser.add_argument("timeframe")
        parser.add_argument("--mode", default="paper")
        args = parser.parse_args()
        run_signal_check(args.strategy, args.symbol, args.timeframe, args.mode)


if __name__ == "__main__":
    main()
