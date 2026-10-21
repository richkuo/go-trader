#!/usr/bin/env python3

import os
import sys
import traceback
from datetime import datetime, timezone


sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "platforms", "robinhood"))
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "shared_tools"))

from stdout_contract import json_stdout


def main():
    with json_stdout() as emit:
        try:
            from adapter import RobinhoodExchangeAdapter
            adapter = RobinhoodExchangeAdapter(mode="live")
            if not adapter.is_live:
                _emit_error(emit, "Robinhood adapter not live — set ROBINHOOD_USERNAME / ROBINHOOD_PASSWORD / ROBINHOOD_TOTP_SECRET")
                return
            raw = adapter.get_crypto_positions_strict()
        except Exception as e:
            traceback.print_exc(file=sys.stderr)
            _emit_error(emit, str(e))
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

        emit({
            "positions": positions,
            "platform": "robinhood",
            "timestamp": datetime.now(timezone.utc).isoformat(),
        })


def _emit_error(emit, message):
    emit({
        "positions": [],
        "platform": "robinhood",
        "timestamp": datetime.now(timezone.utc).isoformat(),
        "error": message,
    })
    sys.exit(1)


if __name__ == "__main__":
    main()
