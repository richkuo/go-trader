#!/usr/bin/env python3
"""
TopStep live trade-fills fetcher (issue #1106 — exchange-sourced cash-flow
journal, shadow phase / Phase 4 of #1100).

Emits every settled TopStep trade fill since a cursor timestamp. A TopStep fill
carries a GROSS realized PnL (0 for entry fills) and a separately-reported
commission; the Go cash-flow journal computes each fill's settled-cash delta as
realized-PnL-gross minus commission (mirroring Hyperliquid's userFills, NOT OKX's
pre-netted balChg). This single feed is the settled-cash-flow source the journal
reconstructs the wallet equity from.

Thin pump: all mapping / summing / fail-closed logic lives in tested Go
(topstep_cashflow_journal.go); this script only pages fills via the adapter and
shapes them to JSON.

Usage:
    fetch_topstep_fills.py --since-ms <int>

Requires TOPSTEP_API_KEY / TOPSTEP_API_SECRET / TOPSTEP_ACCOUNT_ID. Output:
``{"fills": [{"fill_id","ts_ms","symbol","kind","realized_pnl","fee"}, ...],
  "capped": false, "platform": "topstep", "timestamp": ...}``
Fills are oldest-first; ``capped`` is true when the adapter hit its page limit
before exhausting the feed (the Go journal treats that cycle as not-usable).
"""

import argparse
import json
import os
import sys
import traceback
from datetime import datetime, timezone


sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "platforms", "topstep"))


def _parse_args(argv=None):
    parser = argparse.ArgumentParser(description="Fetch TopStep trade fills since a cursor.")
    parser.add_argument(
        "--since-ms",
        type=int,
        default=0,
        help="Only return fills settled at or after this epoch-millisecond cursor.",
    )
    return parser.parse_args(argv)


def main(argv=None):
    args = _parse_args(argv)
    try:
        from adapter import TopStepExchangeAdapter
        adapter = TopStepExchangeAdapter(mode="live")
        if not adapter.is_live:
            _emit_error("TopStep adapter not live — set TOPSTEP_API_KEY / TOPSTEP_API_SECRET / TOPSTEP_ACCOUNT_ID")
            return
        fills, capped = adapter.get_account_fills(since_ms=args.since_ms)
    except Exception as e:
        traceback.print_exc(file=sys.stderr)
        _emit_error(str(e))
        return

    print(json.dumps({
        "fills": fills or [],
        "capped": bool(capped),
        "platform": "topstep",
        "timestamp": datetime.now(timezone.utc).isoformat(),
    }))


def _emit_error(message):
    print(json.dumps({
        "fills": [],
        "capped": False,
        "platform": "topstep",
        "timestamp": datetime.now(timezone.utc).isoformat(),
        "error": message,
    }))
    sys.exit(1)


if __name__ == "__main__":
    main()
