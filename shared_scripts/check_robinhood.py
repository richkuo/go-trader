#!/usr/bin/env python3
"""
Robinhood crypto strategy check script.
Fetches OHLCV via yfinance, runs strategy, outputs JSON to stdout, exits.

Signal check mode (paper or live):
    check_robinhood.py <strategy> <symbol> <timeframe> [--mode=paper|live]

Execution mode (live only, called by Go as phase 2):
    check_robinhood.py --execute --symbol=BTC --side=buy --amount_usd=950 [--mode=live]
    check_robinhood.py --execute --symbol=BTC --side=sell --quantity=0.01 [--mode=live]
"""

import sys
import os
import json
import traceback
from datetime import datetime, timezone

# Add paths: platforms/robinhood/ for adapter, shared_strategies/spot/ for apply_strategy,
# shared_tools/ for utilities.
sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'platforms', 'robinhood'))
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


def run_signal_check(strategy_name, symbol, timeframe, mode, htf_filter_enabled=False, strategy_params=None):
    """Run strategy signal check using yfinance OHLCV data."""
    try:
        from adapter import RobinhoodExchangeAdapter
        from strategies import apply_strategy, get_strategy

        # Verify strategy exists early
        get_strategy(strategy_name)

        adapter = RobinhoodExchangeAdapter(mode=mode)

        print(f"Fetching {symbol} {timeframe} from Robinhood/yfinance ({mode})...", file=sys.stderr)
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
                "platform": "robinhood",
                "timestamp": datetime.now(timezone.utc).isoformat(),
                "error": f"Insufficient data: {len(candles) if candles else 0} candles",
            }))
            sys.exit(1)

        df = _make_dataframe(candles)
        result_df = apply_strategy(strategy_name, df, strategy_params)

        last = result_df.iloc[-1]
        signal = int(last.get("signal", 0))
        if signal > 0:
            signal = 1
        elif signal < 0:
            signal = -1
        else:
            signal = 0

        price = float(last["close"])

        # Apply HTF trend filter if enabled (skip for funding-rate strategies — #103)
        htf_info = {}
        if htf_filter_enabled and strategy_name != "delta_neutral_funding":
            from htf_filter import htf_trend_filter, apply_htf_filter

            def _fetch_htf(sym, tf, limit):
                candles = adapter.get_ohlcv(sym, interval=tf, limit=limit)
                return _make_dataframe(candles) if candles else None

            htf_info = htf_trend_filter(symbol, timeframe, _fetch_htf)
            original_signal = signal
            signal = apply_htf_filter(signal, htf_info.get("htf_trend", 0))
            if signal != original_signal:
                print(f"HTF filter: {original_signal} → {signal} (HTF trend={htf_info.get('htf_trend')})", file=sys.stderr)

        # Freshen price with live quote if available
        try:
            live_price = adapter.get_price(symbol)
            if live_price > 0:
                price = live_price
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

        # Merge HTF indicators
        if htf_info:
            for k, v in htf_info.items():
                if isinstance(v, (int, float)):
                    indicators[k] = v

        print(json.dumps({
            "strategy": strategy_name,
            "symbol": symbol,
            "timeframe": timeframe,
            "signal": signal,
            "price": round(price, 2),
            "indicators": indicators,
            "mode": mode,
            "platform": "robinhood",
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
            "platform": "robinhood",
            "timestamp": datetime.now(timezone.utc).isoformat(),
            "error": str(e),
        }))
        sys.exit(1)


def run_execute(symbol, side, amount_usd, quantity, mode):
    """Place a live crypto order on Robinhood."""
    if mode != "live":
        print(json.dumps({"error": "--execute requires --mode=live"}))
        sys.exit(1)

    try:
        from adapter import RobinhoodExchangeAdapter
        adapter = RobinhoodExchangeAdapter(mode="live")

        is_buy = side.lower() == "buy"

        if is_buy:
            result = adapter.market_buy(symbol, amount_usd)
        else:
            result = adapter.market_sell(symbol, quantity)

        # Extract fill info from robin_stocks response
        fill = {}
        try:
            if result:
                avg_px = float(result.get("average_price", 0) or 0)
                filled_qty = float(result.get("cumulative_quantity", 0) or 0)
                if avg_px > 0:
                    fill = {"avg_px": avg_px, "quantity": filled_qty}
        except Exception:
            pass

        execution = {
            "action": "buy" if is_buy else "sell",
            "symbol": symbol,
            "fill": fill,
        }
        if is_buy:
            execution["amount_usd"] = amount_usd
        else:
            execution["quantity"] = quantity

        print(json.dumps({
            "execution": execution,
            "platform": "robinhood",
            "timestamp": datetime.now(timezone.utc).isoformat(),
        }))

    except Exception as e:
        traceback.print_exc(file=sys.stderr)
        print(json.dumps({
            "execution": None,
            "platform": "robinhood",
            "timestamp": datetime.now(timezone.utc).isoformat(),
            "error": str(e),
        }))
        sys.exit(1)


def main():
    if "--execute" in sys.argv:
        import argparse
        parser = argparse.ArgumentParser()
        parser.add_argument("--execute", action="store_true")
        parser.add_argument("--symbol", required=True)
        parser.add_argument("--side", required=True, choices=["buy", "sell"])
        parser.add_argument("--amount_usd", type=float, default=0)
        parser.add_argument("--quantity", type=float, default=0)
        parser.add_argument("--mode", default="live")
        args = parser.parse_args()
        run_execute(args.symbol, args.side, args.amount_usd, args.quantity, args.mode)
    else:
        import argparse
        parser = argparse.ArgumentParser()
        parser.add_argument("strategy")
        parser.add_argument("symbol")
        parser.add_argument("timeframe")
        parser.add_argument("--mode", default="paper")
        parser.add_argument("--htf-filter", action="store_true", default=False)
        parser.add_argument("--params", default=None)
        args = parser.parse_args()
        params_parsed = json.loads(args.params) if args.params else None
        run_signal_check(args.strategy, args.symbol, args.timeframe, args.mode, args.htf_filter, params_parsed)


if __name__ == "__main__":
    main()
