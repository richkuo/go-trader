#!/usr/bin/env python3
"""
TopStep live account-balance fetcher (issue #1106 — exchange-sourced cash-flow
journal, shadow phase / Phase 4 of #1100).

Emits the USD account equity (settled cash + unrealized PnL) for the configured
TopStep account, plus the unrealized-PnL component from the SAME read, so the Go
cash-flow journal reconciles a coherent equity/uPnL snapshot (no intra-cycle
jitter). Mirrors fetch_okx_balance.py.

Requires TOPSTEP_API_KEY / TOPSTEP_API_SECRET / TOPSTEP_ACCOUNT_ID. Output:
``{"balance": 1234.56, "unrealized_pnl": 5.0, "platform": "topstep",
  "timestamp": ..., "error": "..."}``
where ``balance`` is the USD equity and ``unrealized_pnl`` is equity − cashBalance.
"""

import json
import os
import sys
import traceback
from datetime import datetime, timezone


sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "platforms", "topstep"))


def main():
    try:
        from adapter import TopStepExchangeAdapter
        adapter = TopStepExchangeAdapter(mode="live")
        if not adapter.is_live:
            _emit_error("TopStep adapter not live — set TOPSTEP_API_KEY / TOPSTEP_API_SECRET / TOPSTEP_ACCOUNT_ID")
            return
        equity, upnl = adapter.get_account_equity_and_upnl()
        balance = float(equity or 0.0)
        unrealized_pnl = float(upnl or 0.0)
    except Exception as e:
        traceback.print_exc(file=sys.stderr)
        _emit_error(str(e))
        return

    print(json.dumps({
        "balance": balance,
        "unrealized_pnl": unrealized_pnl,
        "platform": "topstep",
        "timestamp": datetime.now(timezone.utc).isoformat(),
    }))


def _emit_error(message):
    print(json.dumps({
        "balance": 0.0,
        "unrealized_pnl": 0.0,
        "platform": "topstep",
        "timestamp": datetime.now(timezone.utc).isoformat(),
        "error": message,
    }))
    sys.exit(1)


if __name__ == "__main__":
    main()
