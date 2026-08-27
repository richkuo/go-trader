#!/usr/bin/env python3

import argparse
import json
import os
import sys
import traceback
from datetime import datetime, timezone


sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "platforms", "robinhood"))


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--symbol", required=True)
    parser.add_argument("--mode", default="live")
    args = parser.parse_args()

    if args.mode != "live":
        print(json.dumps({
            "close": None,
            "platform": "robinhood",
            "timestamp": datetime.now(timezone.utc).isoformat(),
            "error": "--mode=live required for emergency close",
        }))
        sys.exit(1)

    try:
        from adapter import RobinhoodExchangeAdapter
        adapter = RobinhoodExchangeAdapter(mode="live")
        if not adapter.is_live:
            _emit_error(args.symbol, "Robinhood adapter not live — set ROBINHOOD_USERNAME / ROBINHOOD_PASSWORD / ROBINHOOD_TOTP_SECRET")
            return

        positions = adapter.get_crypto_positions()
        qty = 0.0
        for pos in positions or []:
            if (pos.get("symbol") or "").upper() == args.symbol.upper():
                try:
                    qty = float(pos.get("quantity") or 0)
                except (TypeError, ValueError):
                    qty = 0.0
                break

        if qty <= 0:
            _emit_success(args.symbol, fill={}, already_flat=True)
            return

        result = adapter.market_sell(args.symbol, qty)
    except Exception as e:
        traceback.print_exc(file=sys.stderr)
        _emit_error(args.symbol, str(e))
        return

    if not isinstance(result, dict):
        _emit_error(args.symbol, f"unexpected adapter response type {type(result).__name__}: {result!r}")
        return

    if not result:
        _emit_error(args.symbol, "empty order response from robin_stocks sell submit")
        return

    fill = {}
    try:
        filled_qty = result.get("quantity") or result.get("cumulative_quantity")
        if filled_qty is not None:
            fill["total_sz"] = float(filled_qty)
    except (TypeError, ValueError):
        pass
    try:
        avg = result.get("average_price") or result.get("price")
        if avg is not None:
            fill["avg_px"] = float(avg)
    except (TypeError, ValueError):
        pass
    oid = result.get("id")
    if oid is not None:
        fill["oid"] = str(oid)

    _emit_success(args.symbol, fill)


def _emit_success(symbol, fill, already_flat=False):
    close = {"symbol": symbol, "fill": fill}
    if already_flat:
        close["already_flat"] = True
    print(json.dumps({
        "close": close,
        "platform": "robinhood",
        "timestamp": datetime.now(timezone.utc).isoformat(),
    }))


def _emit_error(symbol, message):
    print(json.dumps({
        "close": {"symbol": symbol, "fill": {}},
        "platform": "robinhood",
        "timestamp": datetime.now(timezone.utc).isoformat(),
        "error": message,
    }))
    sys.exit(1)


if __name__ == "__main__":
    main()
