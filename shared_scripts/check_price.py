#!/usr/bin/env python3

import os
import sys
import json
import traceback

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "shared_tools"))

import egress_policy


def main():
    symbols = sys.argv[1:]
    if not symbols:
        print(json.dumps({}))
        return

    try:
        import ccxt
        exchange = egress_policy.pin_ccxt(ccxt.binanceus({"enableRateLimit": True}))

        prices = {}
        for symbol in symbols:
            try:
                ticker = exchange.fetch_ticker(symbol)
                prices[symbol] = round(ticker["last"], 2)
            except Exception as e:
                print(f"Failed to fetch {symbol}: {e}", file=sys.stderr)

        print(json.dumps(prices))

    except Exception as e:
        traceback.print_exc(file=sys.stderr)
        print(json.dumps({}))
        sys.exit(1)


if __name__ == "__main__":
    main()
