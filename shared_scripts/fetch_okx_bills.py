#!/usr/bin/env python3
"""
OKX live account-bills fetcher (issue #1105 — exchange-sourced cash-flow journal,
shadow phase / Phase 3a of #1100).

Emits every OKX account bill settled since a cursor timestamp. An OKX account
bill is the venue's record of a single settled-cash movement — trade PnL, fee,
funding, transfer, deposit, withdrawal — each carrying ``balChg`` (the signed
change to the settled cash balance). This single feed is therefore the COMPLETE
settled-cash-flow source the Go cash-flow journal reconstructs the wallet total
from (mirroring the HL journal, but single-stream because ``balChg`` already
nets every component — no per-fill fee arithmetic).

Thin pump: all mapping / summing / fail-closed logic lives in tested Go
(okx_cashflow_journal.go); this script only pages OKX bills via the adapter and
shapes them to JSON.

Usage:
    fetch_okx_bills.py --since-ms <int>

Requires OKX_API_KEY / OKX_API_SECRET / OKX_PASSPHRASE. Output:
``{"bills": [{"bill_id","ts_ms","ccy","type","sub_type","bal_chg","pnl","fee",
  "inst_id","trade_id"}, ...], "capped": false, "platform": "okx",
  "timestamp": ...}``
Bills are oldest-first; ``capped`` is true when the adapter hit its safety page
cap before exhausting the feed (the Go journal treats that cycle as not-usable).
"""

import argparse
import json
import os
import sys
import traceback
from datetime import datetime, timezone


sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "platforms", "okx"))


def _parse_args(argv=None):
    parser = argparse.ArgumentParser(description="Fetch OKX account bills since a cursor.")
    parser.add_argument(
        "--since-ms",
        type=int,
        default=0,
        help="Only return bills settled at or after this epoch-millisecond cursor.",
    )
    return parser.parse_args(argv)


def main(argv=None):
    args = _parse_args(argv)
    try:
        from adapter import OKXExchangeAdapter
        adapter = OKXExchangeAdapter()
        if not adapter.is_live:
            _emit_error("OKX adapter not live — set OKX_API_KEY / OKX_API_SECRET / OKX_PASSPHRASE")
            return
        bills, capped = adapter.get_account_bills(since_ms=args.since_ms)
    except Exception as e:
        traceback.print_exc(file=sys.stderr)
        _emit_error(str(e))
        return

    print(json.dumps({
        "bills": bills or [],
        "capped": bool(capped),
        "platform": "okx",
        "timestamp": datetime.now(timezone.utc).isoformat(),
    }))


def _emit_error(message):
    print(json.dumps({
        "bills": [],
        "capped": False,
        "platform": "okx",
        "timestamp": datetime.now(timezone.utc).isoformat(),
        "error": message,
    }))
    sys.exit(1)


if __name__ == "__main__":
    main()
