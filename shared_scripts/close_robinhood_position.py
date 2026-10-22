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
_CLOSE_KIND_EMERGENCY = "emergency"
_SIZING_NUMBER_KEYS = (
    "asked_quantity", "account_quantity", "account_available", "sent_quantity", "min_order_size",
    "quantity_increment", "max_order_size", "residue_quantity",
)
_SIZING_BOOL_KEYS = ("sole_owner", "swept", "capped", "account_short")


class _ArgvError(Exception):
    pass


class _Parser(argparse.ArgumentParser):
    def error(self, message):
        raise _ArgvError(message)


def _finite_or_none(value):
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        return None
    value = float(value)
    return value if math.isfinite(value) else None


def _clean_sizing(sizing):
    if not isinstance(sizing, dict):
        return None
    clean = {key: _finite_or_none(sizing.get(key)) for key in _SIZING_NUMBER_KEYS}
    for key in _SIZING_BOOL_KEYS:
        clean[key] = sizing.get(key) is True
    kind = sizing.get("close_kind")
    clean["close_kind"] = kind if kind in ("whole", "partial", _CLOSE_KIND_EMERGENCY) else None
    return clean


def _parse_args(argv):
    parser = _Parser()
    parser.add_argument("--symbol", required=True)
    parser.add_argument("--mode", default="live")
    parser.add_argument("--quantity", default=None)
    parser.add_argument("--ref-id", default="")
    parser.add_argument("--budget-ms", type=int, default=_DEFAULT_BUDGET_MS)
    parser.add_argument("--probe-only", action="store_true")
    return parser.parse_args(argv)


def _argv_symbol(argv):
    for i, arg in enumerate(argv):
        if arg.startswith("--symbol="):
            return arg.split("=", 1)[1]
        if arg == "--symbol" and i + 1 < len(argv):
            return argv[i + 1]
    return ""


def _parse_quantity(text):
    from adapter import _parse_exact_decimal

    if text is None:
        raise ValueError("--quantity is required: the emergency close sells at most the go-trader book quantity")
    asked = _parse_exact_decimal(text)
    if asked is None or asked <= 0:
        raise ValueError(f"--quantity {text!r} is not a bounded finite positive decimal")
    return asked


def main():
    with json_stdout() as emit:
        argv = sys.argv[1:]
        try:
            args = _parse_args(argv)
        except _ArgvError as e:
            _emit(emit, _argv_symbol(argv), {"outcome": "not_sent", "detail": f"invalid arguments: {e}"})
            return
        if args.probe_only:
            sys.exit(0)

        try:
            asked = _parse_quantity(args.quantity)
        except Exception as e:
            _emit(emit, args.symbol, {"outcome": "not_sent", "detail": str(e)})
            return

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
            holding = adapter.holding_quantities(args.symbol, deadline)
            held, available = holding["quantity"], holding["available"]
        except Exception as e:
            traceback.print_exc(file=sys.stderr)
            _emit(emit, args.symbol, {"outcome": "not_sent", "detail": str(e)})
            return

        if held == 0:
            _emit(emit, args.symbol, {"outcome": "already_flat", "detail": ""}, held_qty=0.0, available_qty=0.0)
            return

        if not args.ref_id:
            _emit(emit, args.symbol, {"outcome": "not_sent", "detail": f"--ref-id is required to sell the held {args.symbol}"}, held_qty=held, available_qty=available)
            return

        try:
            result = adapter.place_market_order(
                args.symbol, "sell", ref_id=args.ref_id, deadline=deadline, quantity=float(asked),
                sell_policy={"asked": asked, "close_kind": _CLOSE_KIND_EMERGENCY, "sole_owner": False},
            )
        except Exception as e:
            traceback.print_exc(file=sys.stderr)
            result = {"outcome": "not_sent", "detail": str(e), "ref_id": args.ref_id}
        if result.get("detail"):
            print(f"[robinhood] close {result.get('outcome')}: {result['detail']}", file=sys.stderr)
        _emit(emit, args.symbol, result, held_qty=held, available_qty=available)


def _emit(emit, symbol, result, held_qty=None, available_qty=None):
    outcome = result.get("outcome") or "unknown"
    close = {"symbol": symbol}
    if held_qty is not None:
        close["held_qty"] = _finite_or_none(float(held_qty))
    if available_qty is not None:
        close["available_qty"] = _finite_or_none(float(available_qty))
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
    sizing = _clean_sizing(result.get("sizing"))
    if sizing is not None:
        payload["sizing"] = sizing
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
