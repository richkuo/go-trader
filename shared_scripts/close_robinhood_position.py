#!/usr/bin/env python3

import argparse
import math
import os
import sys
import time
import traceback
from datetime import datetime, timezone

_STARTED = time.monotonic()

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "platforms", "robinhood"))
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "shared_tools"))

from stdout_contract import json_stdout

_BUDGET_MIN_MS = 2000
_BUDGET_MAX_MS = 120000
_DEFAULT_BUDGET_MS = 50000
_CONFIRMED_FILL_OUTCOMES = ("filled", "partially_filled")


def _finite_or_none(value):
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        return None
    value = float(value)
    return value if math.isfinite(value) else None


def main():
    with json_stdout() as emit:
        parser = argparse.ArgumentParser()
        parser.add_argument("--symbol", required=True)
        parser.add_argument("--mode", default="live")
        parser.add_argument("--ref-id", default="")
        parser.add_argument("--budget-ms", type=int, default=_DEFAULT_BUDGET_MS)
        parser.add_argument("--probe-only", action="store_true")
        args = parser.parse_args()
        if args.probe_only:
            sys.exit(0)

        if args.mode != "live":
            _emit(emit, args.symbol, {"outcome": "not_sent", "detail": "--mode=live required for emergency close"})
            return

        if args.budget_ms < _BUDGET_MIN_MS or args.budget_ms > _BUDGET_MAX_MS:
            _emit(emit, args.symbol, {"outcome": "not_sent", "detail": f"--budget-ms must be from {_BUDGET_MIN_MS} to {_BUDGET_MAX_MS}"})
            return
        deadline = _STARTED + args.budget_ms / 1000.0

        try:
            from adapter import RobinhoodExchangeAdapter
            adapter = RobinhoodExchangeAdapter(mode="live", login_deadline=deadline)
            if not adapter.is_live:
                raise RuntimeError("Robinhood adapter not live — set ROBINHOOD_USERNAME / ROBINHOOD_PASSWORD / ROBINHOOD_TOTP_SECRET")

            positions = adapter.get_crypto_positions_strict(deadline=deadline)
            if not isinstance(positions, list):
                raise RuntimeError(f"Robinhood holdings read returned {type(positions).__name__}, expected list")
            qty = 0.0
            for pos in positions:
                if not isinstance(pos, dict):
                    raise RuntimeError("Robinhood holdings read returned a non-object row")
                if (pos.get("symbol") or "").strip().upper() == args.symbol.strip().upper():
                    raw_qty = pos.get("quantity")
                    if isinstance(raw_qty, bool):
                        raise RuntimeError(f"Robinhood holding for {args.symbol} has a malformed quantity")
                    try:
                        qty = float(raw_qty)
                    except (TypeError, ValueError) as e:
                        raise RuntimeError(f"Robinhood holding for {args.symbol} has a malformed quantity") from e
                    if not math.isfinite(qty) or qty < 0:
                        raise RuntimeError(f"Robinhood holding for {args.symbol} has a non-finite or negative quantity")
                    break
        except Exception as e:
            traceback.print_exc(file=sys.stderr)
            _emit(emit, args.symbol, {"outcome": "not_sent", "detail": str(e)})
            return

        if qty == 0:
            _emit(emit, args.symbol, {"outcome": "already_flat", "detail": ""}, held_qty=0.0)
            return

        if not args.ref_id:
            _emit(emit, args.symbol, {"outcome": "not_sent", "detail": f"--ref-id is required to sell the held {args.symbol}"}, held_qty=qty)
            return

        try:
            result = adapter.place_market_order(args.symbol, "sell", ref_id=args.ref_id, deadline=deadline, quantity=qty)
        except Exception as e:
            traceback.print_exc(file=sys.stderr)
            result = {"outcome": "not_sent", "detail": str(e), "ref_id": args.ref_id}
        if result.get("detail"):
            print(f"[robinhood] close {result.get('outcome')}: {result['detail']}", file=sys.stderr)
        _emit(emit, args.symbol, result, held_qty=qty)


def _emit(emit, symbol, result, held_qty=None):
    outcome = result.get("outcome") or "unknown"
    close = {"symbol": symbol}
    if held_qty is not None:
        close["held_qty"] = _finite_or_none(held_qty)
    payload = {
        "close": close,
        "order_outcome": outcome,
        "order": result.get("order") or {},
        "ref_id": result.get("ref_id"),
        "requested_quantity": _finite_or_none(result.get("requested_quantity")),
        "account_id": result.get("account_id"),
        "cancel": result.get("cancel") or {"requested": False, "acknowledged": False},
        "platform": "robinhood",
        "timestamp": datetime.now(timezone.utc).isoformat(),
    }
    fill = result.get("fill")
    if outcome == "already_flat":
        close["already_flat"] = True
        close["fill"] = {}
    elif outcome in _CONFIRMED_FILL_OUTCOMES and isinstance(fill, dict):
        clean = {
            "avg_px": _finite_or_none(fill.get("avg_px")),
            "total_sz": _finite_or_none(fill.get("quantity")),
            "oid": fill.get("oid"),
        }
        fee = _finite_or_none(fill.get("fee"))
        if fee is not None:
            clean["fee"] = fee
        close["fill"] = clean
    else:
        payload["error"] = result.get("detail") or f"Robinhood close outcome {outcome}"
    emit(payload)
    if outcome != "already_flat" and outcome not in _CONFIRMED_FILL_OUTCOMES:
        sys.exit(1)


if __name__ == "__main__":
    main()
