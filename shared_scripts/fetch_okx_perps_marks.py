#!/usr/bin/env python3
"""
Mark-price fetcher for OKX perpetual swap positions (issue #263).

Called by the Go scheduler to revalue open OKX perps positions at the live
OKX swap mark rather than the BinanceUS spot quote. BinanceUS spot is the
wrong oracle because spot/perps basis divergence (funding, exchange-specific
liquidity) shows up in PortfolioValue as phantom PnL — see issue #263.

Usage: python3 fetch_okx_perps_marks.py BTC ETH SOL

Always outputs a JSON object to stdout:
  {"BTC": 67500.5, "ETH": 3200.1}
Coins that cannot be fetched are omitted so the Go caller can detect misses
and fall back to pos.AvgCost with a [WARN] log — graceful degradation, not a
cycle skip. Matches fetch_futures_marks.py degradation semantics.

Authentication: public swap ticker data does not require credentials. If
OKX_API_KEY / OKX_API_SECRET / OKX_PASSPHRASE are present they will be
loaded by OKXExchangeAdapter but are not needed for this read-only call.
"""

import json
import os
import sys
import traceback


def main():
    coins = sys.argv[1:]
    if not coins:
        print(json.dumps({}))
        return

    try:
        sys.path.insert(
            0,
            os.path.join(os.path.dirname(__file__), "..", "platforms", "okx"),
        )
        from adapter import OKXExchangeAdapter  # type: ignore
    except Exception as e:  # noqa: BLE001
        print(
            f"[WARN][fetch_okx_perps_marks] adapter import failed: {e}",
            file=sys.stderr,
        )
        traceback.print_exc(file=sys.stderr)
        print(json.dumps({}))
        sys.exit(1)

    try:
        adapter = OKXExchangeAdapter()
    except Exception as e:  # noqa: BLE001
        print(
            f"[WARN][fetch_okx_perps_marks] adapter init failed: {e}",
            file=sys.stderr,
        )
        traceback.print_exc(file=sys.stderr)
        print(json.dumps({}))
        sys.exit(1)

    marks: "dict[str, float]" = {}
    for coin in coins:
        try:
            price = adapter.get_perp_price(coin)
            if price and price > 0:
                marks[coin] = float(price)
            else:
                print(
                    f"[WARN][fetch_okx_perps_marks] no price for {coin}",
                    file=sys.stderr,
                )
        except Exception as e:  # noqa: BLE001
            print(
                f"[WARN][fetch_okx_perps_marks] get_perp_price({coin}) failed: {e}",
                file=sys.stderr,
            )
            # Omit failed coins — Go caller falls back to pos.AvgCost.

    print(json.dumps(marks))


if __name__ == "__main__":
    main()
