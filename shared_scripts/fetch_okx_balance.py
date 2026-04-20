#!/usr/bin/env python3
"""
OKX live account-balance fetcher (issue #360 phase 2 of #357).

Emits the total USDT-denominated account value for shared-wallet portfolio
aggregation. Used by ``defaultSharedWalletBalance`` in the Go scheduler so
multi-strategy OKX deployments don't double-count capital.

Scope: unified USDT total balance (free + used) via the adapter. Callers
that need open-position PnL should upgrade the adapter's aggregation — for
now, unrealized PnL is reflected via ``fetch_positions`` and revalued at
mark prices upstream in the scheduler.

Requires OKX_API_KEY / OKX_API_SECRET / OKX_PASSPHRASE. Output:
``{"balance": 1234.56, "platform": "okx", "timestamp": ..., "error": "..."}``
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
        balance = float(adapter.get_account_balance() or 0.0)
    except Exception as e:
        traceback.print_exc(file=sys.stderr)
        _emit_error(str(e))
        return

    print(json.dumps({
        "balance": balance,
        "platform": "okx",
        "timestamp": datetime.now(timezone.utc).isoformat(),
    }))


def _emit_error(message):
    print(json.dumps({
        "balance": 0.0,
        "platform": "okx",
        "timestamp": datetime.now(timezone.utc).isoformat(),
        "error": message,
    }))
    sys.exit(1)


if __name__ == "__main__":
    main()
