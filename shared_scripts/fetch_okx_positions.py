#!/usr/bin/env python3
"""
OKX live positions fetcher (issue #345).

Fetches every open OKX perpetual swap position on the account and emits a
JSON list to stdout. Used by the portfolio kill switch in the Go scheduler
to decide which coins need reduce-only closes — mirrors
``fetchHyperliquidState`` but via Python subprocess since OKX position
queries require authenticated API access (no public endpoint equivalent).

Scope: swap (perps) only. OKX spot balances are NOT surfaced — the kill
switch has no automated spot close path (see ``close_okx_position.py``).

Usage:
    fetch_okx_positions.py

Requires OKX_API_KEY / OKX_API_SECRET / OKX_PASSPHRASE. Output:
``{"positions": [{"coin": "BTC", "size": 0.334, "entry_price": 42000.5,
  "side": "long"}, ...], "platform": "okx", "timestamp": ...}``
Size is signed (positive = long, negative = short) to mirror HLPosition.
"""

import json
import os
import sys
import traceback
from datetime import datetime, timezone


sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "platforms", "okx"))


def main():
    try:
        from adapter import OKXExchangeAdapter
        adapter = OKXExchangeAdapter()
        if not adapter.is_live:
            _emit_error("OKX adapter not live — set OKX_API_KEY / OKX_API_SECRET / OKX_PASSPHRASE")
            return
        raw = adapter._exchange.fetch_positions()
    except Exception as e:
        traceback.print_exc(file=sys.stderr)
        _emit_error(str(e))
        return

    positions = []
    for p in raw or []:
        # ccxt unified position shape: {symbol, contracts, side, entryPrice, ...}
        try:
            contracts = float(p.get("contracts") or 0)
        except (TypeError, ValueError):
            continue
        if contracts == 0:
            continue
        symbol = p.get("symbol") or ""
        # OKX swap symbols look like "BTC/USDT:USDT" — strip to coin.
        coin = symbol.split("/", 1)[0] if "/" in symbol else symbol
        if not coin:
            continue
        side = (p.get("side") or "").lower()
        # Encode side into signed size so Go side can mirror HLPosition semantics.
        signed_size = -contracts if side == "short" else contracts
        entry_price = 0.0
        try:
            entry_price = float(p.get("entryPrice") or 0)
        except (TypeError, ValueError):
            pass
        positions.append({
            "coin": coin,
            "size": signed_size,
            "entry_price": entry_price,
            "side": side,
        })

    print(json.dumps({
        "positions": positions,
        "platform": "okx",
        "timestamp": datetime.now(timezone.utc).isoformat(),
    }))


def _emit_error(message):
    print(json.dumps({
        "positions": [],
        "platform": "okx",
        "timestamp": datetime.now(timezone.utc).isoformat(),
        "error": message,
    }))
    sys.exit(1)


if __name__ == "__main__":
    main()
