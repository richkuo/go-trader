#!/usr/bin/env python3
"""
Quick price fetcher for the Go scheduler.
Fetches current prices for given symbols.

Usage: python3 check_price.py BTC/USDT SOL/USDT
"""

import sys
import json
import traceback


def main():
    symbols = sys.argv[1:]
    if not symbols:
        print(json.dumps({}))
        return

    try:
        import ccxt
        exchange = ccxt.binanceus({"enableRateLimit": True})

        prices = {}
        for symbol in symbols:
            try:
                ticker = exchange.fetch_ticker(symbol)
                prices[symbol] = round(ticker["last"], 2)
            except Exception as e:
                print(f"Failed to fetch {symbol}: {e}", file=sys.stderr)
                prices[symbol] = 0

        print(json.dumps(prices))

    except Exception as e:
        traceback.print_exc(file=sys.stderr)
        # Return zeros so Go can still parse
        print(json.dumps({s: 0 for s in symbols}))
        sys.exit(0)


if __name__ == "__main__":
    main()
