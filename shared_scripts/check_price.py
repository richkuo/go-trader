#!/usr/bin/env python3

import sys
import json
import math
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
                last = ticker["last"]
                if isinstance(last, bool) or not isinstance(last, (int, float)):
                    raise TypeError(f"ticker last is not a number: {last!r}")
                if not math.isfinite(last):
                    raise ValueError(f"ticker last is not finite: {last!r}")
                prices[symbol] = float(last)
            except Exception as e:
                print(f"Failed to fetch {symbol}: {e}", file=sys.stderr)

        print(json.dumps(prices))

    except Exception as e:
        traceback.print_exc(file=sys.stderr)
        print(json.dumps({}))
        sys.exit(1)


if __name__ == "__main__":
    main()
