#!/usr/bin/env python3

import argparse
import json
import os
import sys
import traceback
from datetime import datetime, timezone


sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "platforms", "topstep"))


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--symbol", required=True)
    parser.add_argument("--mode", default="live")
    args = parser.parse_args()

    if args.mode != "live":
        print(json.dumps({
            "close": None,
            "platform": "topstep",
            "timestamp": datetime.now(timezone.utc).isoformat(),
            "error": "--mode=live required for emergency close",
        }))
        sys.exit(1)

    try:
        from adapter import TopStepExchangeAdapter
        adapter = TopStepExchangeAdapter(mode="live")
        if not adapter.is_live:
            _emit_error(args.symbol, "TopStep adapter not live — set TOPSTEP_API_KEY / TOPSTEP_API_SECRET / TOPSTEP_ACCOUNT_ID")
            return

        result = adapter.market_close(args.symbol)
    except Exception as e:
        traceback.print_exc(file=sys.stderr)
        _emit_error(args.symbol, str(e))
        return

    if not isinstance(result, dict):
        _emit_error(args.symbol, f"unexpected adapter response type {type(result).__name__}: {result!r}")
        return

    status = (result.get("status") or "").lower()
    if status and status not in ("ok", "filled", "accepted"):
        _emit_error(args.symbol, f"venue status={result.get('status')!r}: {result}")
        return
    if not result and status == "":
        _emit_error(args.symbol, "empty order response from TopStepX market_close")
        return

    fill = {}
    try:
        filled_qty = result.get("filledQuantity") or result.get("quantity")
        if filled_qty is not None:
            fill["total_contracts"] = int(filled_qty)
    except (TypeError, ValueError):
        pass
    try:
        avg = result.get("avgPrice") or result.get("averagePrice")
        if avg is not None:
            fill["avg_px"] = float(avg)
    except (TypeError, ValueError):
        pass
    oid = result.get("orderId") or result.get("id")
    if oid is not None:
        fill["oid"] = str(oid)

    _emit_success(args.symbol, fill)


def _emit_success(symbol, fill):
    print(json.dumps({
        "close": {"symbol": symbol, "fill": fill},
        "platform": "topstep",
        "timestamp": datetime.now(timezone.utc).isoformat(),
    }))


def _emit_error(symbol, message):
    print(json.dumps({
        "close": {"symbol": symbol, "fill": {}},
        "platform": "topstep",
        "timestamp": datetime.now(timezone.utc).isoformat(),
        "error": message,
    }))
    sys.exit(1)


if __name__ == "__main__":
    main()
