#!/usr/bin/env python3
"""
OKX emergency position close script (issue #345).

Submits a reduce-only market close for a single coin via the OKX adapter's
``market_close``. Used by the portfolio kill switch in the Go scheduler to
liquidate on-chain swap (perps) exposure regardless of which strategy
"owns" the position.

Scope: swap (perps) only. OKX spot strategies are NOT closed by this script
— spot "positions" are just balances, have no reduce-only semantics, and a
blind sell-all would trash unrelated holdings on the same account. See #345
for the spot follow-up.

Usage:
    close_okx_position.py --symbol=BTC --mode=live

Live mode is required (kill switch is meaningful only against real
positions). Stdout is always a single JSON envelope matching the shape of
``close_hyperliquid_position.py`` so the Go caller's parser contract is
symmetric across platforms:
``{"close": {"symbol": ..., "fill": {...}}, "platform": "okx",
  "timestamp": ..., "error": "..."}``

The kill switch must NEVER report success unless the on-chain exposure is
actually reduced. Credential errors, network failures, and unexpected SDK
responses all exit 1 with a populated ``error`` field so the Go caller
latches the kill switch and retries next cycle.
"""

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
        result = adapter.market_close(args.symbol)
    except Exception as e:
        traceback.print_exc(file=sys.stderr)
        _emit_error(args.symbol, str(e))
        return

    # adapter.market_close returns:
    #   - {} when fetch_positions returned no open position (already flat)
    #   - the ccxt order dict from the first reduce-only close submit
    # Any other shape is unexpected and must be treated as failure so the
    # kill switch latches for a retry rather than clearing virtual state.
    if not isinstance(result, dict):
        _emit_error(args.symbol, f"unexpected adapter response type {type(result).__name__}: {result!r}")
        return

    if not result:
        # Empty dict — adapter found no open position to close. Treat as
        # already-flat success so the kill switch can release the latch when
        # on-chain is genuinely flat (eventual-consistency window between the
        # Go-side fetch and this submit). Set already_flat=True so the Go
        # side routes this through the AlreadyFlat report slice rather than
        # ClosedCoins — operator messaging must distinguish "we sent a
        # close order" from "nothing to close" (#350).
        _emit_success(args.symbol, fill={}, already_flat=True)
        return

    # ccxt unified order response: extract best-effort fill telemetry. Missing
    # fields are OK — the Go caller only treats non-nil error as failure.
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
