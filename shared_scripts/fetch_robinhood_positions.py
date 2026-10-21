#!/usr/bin/env python3

import math
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

        if not isinstance(raw, list):
            _emit_error(emit, f"Robinhood holdings read returned {type(raw).__name__}, expected list")
            return

        positions = []
        for index, p in enumerate(raw):
            if not isinstance(p, dict):
                _emit_error(emit, f"Robinhood holding {index} is not an object")
                return
            qty = _finite_number(p.get("quantity"))
            if qty is None or qty < 0:
                _emit_error(emit, f"Robinhood holding {index} has a malformed, non-finite or negative quantity")
                return
            if qty == 0:
                continue
            raw_symbol = p.get("symbol")
            symbol = raw_symbol.strip().upper() if isinstance(raw_symbol, str) else ""
            if not symbol:
                _emit_error(emit, f"Robinhood holding {index} has no symbol")
                return
            avg_price = _finite_number(p.get("avg_price"))
            if avg_price is None or avg_price < 0:
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


def _finite_number(value):
    if isinstance(value, bool):
        return None
    try:
        number = float(value)
    except (TypeError, ValueError):
        return None
    if not math.isfinite(number):
        return None
    return number


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
