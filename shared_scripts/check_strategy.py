#!/usr/bin/env python3
"""
Stateless spot strategy check script.
Fetches data, runs strategy, outputs JSON to stdout, exits.

Usage: python3 check_strategy.py <strategy> <symbol> <timeframe> [symbol_b]

  symbol_b  Optional second asset symbol for pairs_spread (e.g. ETH/USDT).
            When provided, close prices of symbol_b are merged into the
            dataframe as the 'close_b' column so the strategy runs proper
            stat-arb.  Without it, pairs_spread degrades to self-mean-reversion.
"""

import sys
import os
import json
import traceback
from datetime import datetime, timezone

# Add parent dirs to path so we can import from strategies/ and core/
sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'shared_strategies', 'spot'))
sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'shared_tools'))


def main():
    if len(sys.argv) < 4:
        print(json.dumps({
            "error": f"Usage: {sys.argv[0]} <strategy> <symbol> <timeframe> [symbol_b]"
        }))
        sys.exit(1)

    strategy_name = sys.argv[1]
    symbol = sys.argv[2]
    timeframe = sys.argv[3]
    symbol_b = sys.argv[4] if len(sys.argv) >= 5 else None

    try:
        from strategies import apply_strategy, get_strategy
        from data_fetcher import fetch_ohlcv

        # Verify strategy exists
        get_strategy(strategy_name)

        # Warn when pairs_spread will degrade due to missing secondary symbol
        if strategy_name == "pairs_spread" and not symbol_b:
            print(
                "Warning: pairs_spread requires a secondary symbol (symbol_b); "
                "degrading to self-mean-reversion. Pass a 4th argument to enable "
                "proper stat-arb (e.g. ETH/USDT for a BTC/USDT primary).",
                file=sys.stderr,
            )

        # Fetch primary data
        print(f"Fetching {symbol} {timeframe}...", file=sys.stderr)
        df = fetch_ohlcv(symbol=symbol, timeframe=timeframe, limit=200, store=False)

        # Fetch and merge secondary data for pairs strategies
        if strategy_name == "pairs_spread" and symbol_b:
            print(f"Fetching secondary {symbol_b} {timeframe}...", file=sys.stderr)
            df_b = fetch_ohlcv(symbol=symbol_b, timeframe=timeframe, limit=200, store=False)
            if df_b.empty:
                print(json.dumps({
                    "strategy": strategy_name,
                    "symbol": symbol,
                    "timeframe": timeframe,
                    "signal": 0,
                    "price": 0,
                    "indicators": {},
                    "timestamp": datetime.now(timezone.utc).isoformat(),
                    "error": f"No data returned for secondary symbol {symbol_b}",
                }))
                sys.exit(1)
            # Inner join on datetime index so both assets have the same timestamps
            df = df.join(df_b[["close"]].rename(columns={"close": "close_b"}), how="inner")
            print(f"Merged pair: {len(df)} aligned candles ({symbol} / {symbol_b})", file=sys.stderr)

        if df.empty or len(df) < 30:
            print(json.dumps({
                "strategy": strategy_name,
                "symbol": symbol,
                "timeframe": timeframe,
                "signal": 0,
                "price": 0,
                "indicators": {},
                "timestamp": datetime.now(timezone.utc).isoformat(),
                "error": f"Insufficient data: {len(df)} candles"
            }))
            return

        # Run the strategy
        result_df = apply_strategy(strategy_name, df)

        # Get the last row's signal
        last = result_df.iloc[-1]
        signal = int(last.get("signal", 0))
        # Clamp to -1, 0, 1
        if signal > 0:
            signal = 1
        elif signal < 0:
            signal = -1
        else:
            signal = 0

        price = float(last["close"])

        # Collect relevant indicators
        indicators = {}
        indicator_cols = [c for c in result_df.columns
                         if c not in ("open", "high", "low", "close", "close_b", "volume",
                                      "timestamp", "signal", "position", "datetime")]
        for col in indicator_cols:
            val = last.get(col)
            if val is not None:
                try:
                    indicators[col] = round(float(val), 6)
                except (ValueError, TypeError):
                    pass

        output = {
            "strategy": strategy_name,
            "symbol": symbol,
            "timeframe": timeframe,
            "signal": signal,
            "price": round(price, 2),
            "indicators": indicators,
            "timestamp": datetime.now(timezone.utc).isoformat()
        }
        print(json.dumps(output))

    except Exception as e:
        traceback.print_exc(file=sys.stderr)
        print(json.dumps({
            "strategy": strategy_name,
            "symbol": symbol,
            "timeframe": timeframe,
            "signal": 0,
            "price": 0,
            "indicators": {},
            "timestamp": datetime.now(timezone.utc).isoformat(),
            "error": str(e)
        }))
        sys.exit(1)  # Exit 1; Go will still parse the JSON error field


if __name__ == "__main__":
    main()
