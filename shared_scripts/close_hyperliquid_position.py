#!/usr/bin/env python3
"""
Hyperliquid emergency position close script (issue #341).

Submits a reduce-only market close for a single coin via the HL SDK's
`market_close`. Used by the portfolio kill switch in the Go scheduler to
liquidate on-chain exposure regardless of which strategy "owns" the
position — including shared coins where per-strategy reconciliation
deliberately does not overwrite virtual quantities (#258), so virtual
state can diverge from the on-chain net.

Usage:
    close_hyperliquid_position.py --symbol=ETH --mode=live

Live mode is required (kill switch is meaningful only against real
positions). Stdout is always a single JSON envelope: `{"close": ..., "platform": ...,
"timestamp": ..., "error": "..."}`. The Go caller (`RunHyperliquidClose`)
prefers the JSON `error` field over the exit code, but exit 1 is also set
on every error path so a malformed-JSON crash still surfaces as failure.
"""

import argparse
import json
import os
import sys
import traceback
from datetime import datetime, timezone


sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "platforms", "hyperliquid"))


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--symbol", required=True)
    parser.add_argument("--mode", default="live")
    args = parser.parse_args()

    if args.mode != "live":
        print(json.dumps({
            "close": None,
            "platform": "hyperliquid",
            "timestamp": datetime.now(timezone.utc).isoformat(),
            "error": "--mode=live required for emergency close",
        }))
        sys.exit(1)

    try:
        from adapter import HyperliquidExchangeAdapter
        adapter = HyperliquidExchangeAdapter()
        result = adapter.market_close(args.symbol)
    except Exception as e:
        traceback.print_exc(file=sys.stderr)
        _emit_error(args.symbol, str(e))
        return

    # SDK reduce-only close response shape mirrors market_open:
    # {"status": "ok", "response": {"type": "order", "data": {"statuses": [...]}}}
    # The kill switch must NEVER report success unless the order actually
    # filled on-chain — silently treating a "resting" or per-status "error"
    # entry as success would clear virtual state while exposure remains
    # (the original #341 failure mode shifted into the Python layer).

    if not isinstance(result, dict):
        _emit_error(args.symbol, f"unexpected SDK response type {type(result).__name__}: {result!r}")
        return

    # Outer status must be "ok" or absent — anything else is an SDK rejection.
    outer_status = result.get("status")
    if outer_status not in (None, "ok"):
        _emit_error(args.symbol, f"sdk status={outer_status!r}: {result}")
        return

    statuses = result.get("response", {}).get("data", {}).get("statuses", [])

    # Empty statuses == HL had nothing to close (already flat). Treat as success
    # with empty fill so the kill switch can release the latch when on-chain is
    # genuinely flat — this complements the szi==0 filter in fetchHyperliquidState
    # for the eventual-consistency window where on-chain just-flattened between
    # the Go-side fetch and our submit.
    if not statuses:
        # Set already_flat=True so the Go side routes this through the
        # AlreadyFlat report slice rather than ClosedCoins — operator
        # messaging must distinguish "we sent a close order" from
        # "nothing to close" (#350).
        _emit_success(args.symbol, fill={}, already_flat=True)
        return

    first = statuses[0]

    # Per-status error (e.g. "order has zero size", "no position", rate limit).
    # Surface so the kill switch latches and retries next cycle.
    if "error" in first:
        _emit_error(args.symbol, f"per-status error: {first['error']}")
        return

    # "resting" means a limit order is sitting on the book — for market_close
    # this should never happen (market orders fill or fail), but guard anyway.
    # Not "filled" => not closed => kill switch must NOT release the latch.
    if "filled" not in first:
        _emit_error(args.symbol, f"close not filled (status keys={list(first.keys())}): {first}")
        return

    filled = first["filled"]
    fill = {
        "avg_px": float(filled.get("avgPx", 0) or 0),
        "total_sz": float(filled.get("totalSz", 0) or 0),
    }
    oid = filled.get("oid")
    if oid is not None:
        fill["oid"] = int(oid)
    fee = filled.get("fee")
    if fee is not None:
        fill["fee"] = float(fee)
    _emit_success(args.symbol, fill)


def _emit_success(symbol, fill, already_flat=False):
    close = {"symbol": symbol, "fill": fill}
    if already_flat:
        close["already_flat"] = True
    print(json.dumps({
        "close": close,
        "platform": "hyperliquid",
        "timestamp": datetime.now(timezone.utc).isoformat(),
    }))


def _emit_error(symbol, message):
    print(json.dumps({
        "close": {"symbol": symbol, "fill": {}},
        "platform": "hyperliquid",
        "timestamp": datetime.now(timezone.utc).isoformat(),
        "error": message,
    }))
    sys.exit(1)


if __name__ == "__main__":
    main()
