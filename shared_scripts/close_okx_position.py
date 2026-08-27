#!/usr/bin/env python3

import argparse
import json
import os
import sys
import traceback
from datetime import datetime, timezone


sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "platforms", "okx"))


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--symbol", required=True)
    parser.add_argument("--mode", default="live")
    parser.add_argument(
        "--sz",
        type=float,
        default=None,
        help="partial close size in contract units (omit for full position)",
    )
    args = parser.parse_args()

    if args.mode != "live":
        print(json.dumps({
            "close": None,
            "platform": "okx",
            "timestamp": datetime.now(timezone.utc).isoformat(),
            "error": "--mode=live required for emergency close",
        }))
        sys.exit(1)

    try:
        from adapter import OKXExchangeAdapter
        adapter = OKXExchangeAdapter()
        if not adapter.is_live:
            _emit_error(args.symbol, "OKX adapter not live — set OKX_API_KEY / OKX_API_SECRET / OKX_PASSPHRASE")
            return
        result = adapter.market_close(args.symbol, args.sz)
    except Exception as e:
        traceback.print_exc(file=sys.stderr)
        _emit_error(args.symbol, str(e))
        return

    if not isinstance(result, dict):
        _emit_error(args.symbol, f"unexpected adapter response type {type(result).__name__}: {result!r}")
        return

    if not result:
        _emit_success(args.symbol, fill={}, already_flat=True)
        return

    fill = {}
    avg = result.get("average") or result.get("price")
    filled = result.get("filled") or result.get("amount")
    try:
        if avg is not None:
            fill["avg_px"] = float(avg or 0)
        if filled is not None:
            fill["total_sz"] = float(filled or 0)
    except (TypeError, ValueError):
        pass
    oid = result.get("id")
    if oid is not None:
        fill["oid"] = str(oid)
    fee_info = result.get("fee") or {}
    if isinstance(fee_info, dict) and fee_info.get("cost") is not None:
        try:
            fill["fee"] = float(fee_info["cost"])
        except (TypeError, ValueError):
            pass

    _emit_success(args.symbol, fill)


def _emit_success(symbol, fill, already_flat=False):
    close = {"symbol": symbol, "fill": fill}
    if already_flat:
        close["already_flat"] = True
    print(json.dumps({
        "close": close,
        "platform": "okx",
        "timestamp": datetime.now(timezone.utc).isoformat(),
    }))


def _emit_error(symbol, message):
    print(json.dumps({
        "close": {"symbol": symbol, "fill": {}},
        "platform": "okx",
        "timestamp": datetime.now(timezone.utc).isoformat(),
        "error": message,
    }))
    sys.exit(1)


if __name__ == "__main__":
    main()
