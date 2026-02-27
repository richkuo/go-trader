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
                # Omit failed symbols so Go can detect missing prices

        print(json.dumps(prices))

    except Exception as e:
        traceback.print_exc(file=sys.stderr)
        print(json.dumps({}))
        sys.exit(1)


if __name__ == "__main__":
    main()
