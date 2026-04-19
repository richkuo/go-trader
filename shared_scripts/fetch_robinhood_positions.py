#!/usr/bin/env python3
"""
Robinhood live positions fetcher (issue #346).

Fetches every open Robinhood crypto position on the account and emits a
JSON list to stdout. Used by the portfolio kill switch in the Go scheduler
to decide which coins need market closes — mirrors
``fetch_okx_positions.py``. Requires authenticated session
(robin_stocks + TOTP MFA); there is no public unauthenticated endpoint.

Scope: crypto only. Stock options are NOT surfaced — the kill switch has
no automated options close path (different buy-to-close / sell-to-close
semantics, see ``close_robinhood_position.py``).

Usage:
    fetch_robinhood_positions.py

Requires ROBINHOOD_USERNAME / ROBINHOOD_PASSWORD / ROBINHOOD_TOTP_SECRET.
Output:
``{"positions": [{"coin": "BTC", "size": 0.01, "avg_price": 42000.0}, ...],
  "platform": "robinhood", "timestamp": ...}``
Size is unsigned (Robinhood crypto is spot — no short positions).
"""

import json
import os
import sys
import traceback
from datetime import datetime, timezone


sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "platforms", "robinhood"))


def main():
    try:
        from adapter import RobinhoodExchangeAdapter
        adapter = RobinhoodExchangeAdapter(mode="live")
        if not adapter.is_live:
            _emit_error("Robinhood adapter not live — set ROBINHOOD_USERNAME / ROBINHOOD_PASSWORD / ROBINHOOD_TOTP_SECRET")
            return
        # Strict variant propagates exceptions instead of silently returning
        # [] — required by the kill switch so a Robinhood outage latches
        # rather than clears virtual state while live exposure remains
        # (#346 review).
        raw = adapter.get_crypto_positions_strict()
    except Exception as e:
        traceback.print_exc(file=sys.stderr)
        _emit_error(str(e))
        return

    positions = []
    for p in raw or []:
        symbol = (p.get("symbol") or "").upper()
        if not symbol:
            continue
        try:
            qty = float(p.get("quantity") or 0)
        except (TypeError, ValueError):
            continue
        if qty <= 0:
            continue
        try:
            avg_price = float(p.get("avg_price") or 0)
        except (TypeError, ValueError):
            avg_price = 0.0
        positions.append({
            "coin": symbol,
            "size": qty,
            "avg_price": avg_price,
        })

    print(json.dumps({
        "positions": positions,
        "platform": "robinhood",
        "timestamp": datetime.now(timezone.utc).isoformat(),
    }))


def _emit_error(message):
    print(json.dumps({
        "positions": [],
        "platform": "robinhood",
        "timestamp": datetime.now(timezone.utc).isoformat(),
        "error": message,
    }))
    sys.exit(1)


if __name__ == "__main__":
    main()
