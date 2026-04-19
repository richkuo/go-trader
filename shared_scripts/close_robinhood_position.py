#!/usr/bin/env python3
"""
Robinhood emergency position close script (issue #346).

Submits a market sell for the full on-account quantity of a single crypto
coin via the Robinhood adapter's ``market_sell``. Used by the portfolio
kill switch in the Go scheduler to liquidate live exposure regardless of
which strategy "owns" the position. Mirrors ``close_hyperliquid_position.py``
and ``close_okx_position.py`` so the Go caller's parser contract is
symmetric across platforms.

Scope: crypto only. Robinhood stock options positions have different close
semantics (sell-to-close vs buy-to-close per leg) and no unified adapter
method — they are surfaced as a known gap by the Go kill-switch plan
rather than auto-closed here (see #346 follow-up).

Usage:
    close_robinhood_position.py --symbol=BTC --mode=live

Live mode is required (kill switch is meaningful only against real
positions). Stdout is always a single JSON envelope matching the shape of
``close_hyperliquid_position.py``:
``{"close": {"symbol": ..., "fill": {...}}, "platform": "robinhood",
  "timestamp": ..., "error": "..."}``

The kill switch must NEVER report success unless the on-chain exposure is
actually reduced. Credential errors, missing positions that should exist,
and unexpected SDK responses all exit 1 with a populated ``error`` field
so the Go caller latches the kill switch and retries next cycle.
"""

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

        # Find the on-chain quantity so we can market-sell the full balance.
        # Robinhood crypto is spot (no reduce-only), so the kill switch is
        # explicit: sell everything this account holds for this coin.
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
            # Already flat — treat as success so the kill switch can release
            # the latch when on-chain is genuinely empty (eventual-consistency
            # window between the Go-side fetch and this submit). Set
            # already_flat=True so the Go side routes this through the
            # AlreadyFlat report slice rather than ClosedCoins — operator
            # messaging must distinguish "we sent a close order" from
            # "nothing to close" (#350).
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
        # Adapter returned empty dict — order submit returned nothing. Treat
        # as failure so the kill switch latches; an empty response from a
        # live sell is ambiguous and must not be read as confirmation.
        _emit_error(args.symbol, "empty order response from robin_stocks sell submit")
        return

    # robin_stocks order response: surface best-effort fill telemetry.
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
