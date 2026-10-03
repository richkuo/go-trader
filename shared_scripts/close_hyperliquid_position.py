#!/usr/bin/env python3

import argparse
import json
import math
import os
import sys
import time
import traceback
from datetime import datetime, timezone


sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "platforms", "hyperliquid"))
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "shared_tools"))

from hl_user_fills import apply_user_fills_lookup


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--symbol", required=True)
    parser.add_argument("--mode", default="live")
    parser.add_argument(
        "--sz",
        type=float,
        default=None,
        help="partial close size in coin units (omit for full position)",
    )
    parser.add_argument(
        "--cancel-stop-loss-oid",
        type=int,
        action="append",
        default=[],
        help="cancel this trigger OID; repeat for shared-coin triggers (#421)",
    )
    parser.add_argument(
        "--cancel-protection-after-close",
        action="store_true",
        help="cancel trigger OIDs only after the close fill covers the requested size",
    )
    parser.add_argument(
        "--side",
        default="",
        help="sized close order side (buy or sell); required with --close-mode",
    )
    parser.add_argument(
        "--close-mode",
        default="",
        help="sized close mode (reduce_only or cross); sends an IOC order of --side, lot-floored",
    )
    parser.add_argument(
        "--cancel-min-fill",
        type=float,
        default=None,
        help="with --cancel-protection-after-close, cancel only once the fill reaches this size",
    )
    parser.add_argument(
        "--probe-only",
        action="store_true",
        help="startup compatibility probe: validate the argv shape and exit 0 without trading",
    )
    args = parser.parse_args()
    if args.probe_only:
        sys.exit(0)

    if args.mode != "live":
        print(json.dumps({
            "close": None,
            "platform": "hyperliquid",
            "timestamp": datetime.now(timezone.utc).isoformat(),
            "error": "--mode=live required for emergency close",
        }))
        sys.exit(1)

    if args.close_mode:
        run_sized_close(args)
        return

    cancel_err = ""
    cancel_succeeded = False
    cancel_succeeded_oids = []
    cancel_failed_oids = []

    try:
        from adapter import HyperliquidExchangeAdapter
        adapter = HyperliquidExchangeAdapter()
        if not args.cancel_protection_after_close:
            cancel_err, cancel_succeeded, cancel_succeeded_oids, cancel_failed_oids = _cancel_trigger_orders(
                adapter, args.symbol, args.cancel_stop_loss_oid
            )
        fills_since_ms = int(time.time() * 1000) - 10_000
        result = adapter.market_close(args.symbol, args.sz)
    except Exception as e:
        traceback.print_exc(file=sys.stderr)
        _emit_error(args.symbol, str(e), cancel_err=cancel_err, cancel_succeeded=cancel_succeeded,
                    cancel_succeeded_oids=cancel_succeeded_oids, cancel_failed_oids=cancel_failed_oids)
        return


    if not isinstance(result, dict):
        _emit_error(args.symbol, f"unexpected SDK response type {type(result).__name__}: {result!r}",
                    cancel_err=cancel_err, cancel_succeeded=cancel_succeeded,
                    cancel_succeeded_oids=cancel_succeeded_oids, cancel_failed_oids=cancel_failed_oids)
        return

    outer_status = result.get("status")
    if outer_status not in (None, "ok"):
        _emit_error(args.symbol, f"sdk status={outer_status!r}: {result}",
                    cancel_err=cancel_err, cancel_succeeded=cancel_succeeded,
                    cancel_succeeded_oids=cancel_succeeded_oids, cancel_failed_oids=cancel_failed_oids)
        return

    statuses = result.get("response", {}).get("data", {}).get("statuses", [])

    if not statuses:
        _emit_success(args.symbol, fill={}, already_flat=True,
                      cancel_err=cancel_err, cancel_succeeded=cancel_succeeded,
                      cancel_succeeded_oids=cancel_succeeded_oids, cancel_failed_oids=cancel_failed_oids)
        return

    first = statuses[0]

    if "error" in first:
        _emit_error(args.symbol, f"per-status error: {first['error']}",
                    cancel_err=cancel_err, cancel_succeeded=cancel_succeeded,
                    cancel_succeeded_oids=cancel_succeeded_oids, cancel_failed_oids=cancel_failed_oids)
        return

    if "filled" not in first:
        _emit_error(args.symbol, f"close not filled (status keys={list(first.keys())}): {first}",
                    cancel_err=cancel_err, cancel_succeeded=cancel_succeeded,
                    cancel_succeeded_oids=cancel_succeeded_oids, cancel_failed_oids=cancel_failed_oids)
        return

    filled = first["filled"]
    fill = {
        "avg_px": float(filled.get("avgPx", 0) or 0),
        "total_sz": float(filled.get("totalSz", 0) or 0),
    }
    if args.cancel_protection_after_close and _fill_covers_requested_size(fill["total_sz"], args.sz, args.cancel_min_fill):
        cancel_err, cancel_succeeded, cancel_succeeded_oids, cancel_failed_oids = _cancel_trigger_orders(
            adapter, args.symbol, args.cancel_stop_loss_oid
        )
    oid = filled.get("oid")
    if oid is not None:
        fill["oid"] = int(oid)
    fee = filled.get("fee")
    if fee is not None:
        fill["fee"] = float(fee)

    if fill.get("oid"):
        try:
            lookup = adapter.lookup_fill_fee_by_oid(fill["oid"], fills_since_ms)
            if not lookup:
                print(f"[WARN] userFills lookup returned no fills for oid={fill['oid']}", file=sys.stderr)
            elif not apply_user_fills_lookup(fill, lookup):
                print(f"[WARN] userFills lookup returned malformed fill data for oid={fill['oid']}", file=sys.stderr)
        except Exception as fe:
            print(f"[WARN] userFills lookup failed for oid={fill['oid']}: {fe}", file=sys.stderr)
    _emit_success(args.symbol, fill, cancel_err=cancel_err, cancel_succeeded=cancel_succeeded,
                  cancel_succeeded_oids=cancel_succeeded_oids, cancel_failed_oids=cancel_failed_oids)


def _cancel_trigger_orders(adapter, symbol, cancel_oids):
    cancel_errors = []
    cancel_succeeded_oids = []
    cancel_failed_oids = []
    for oid in cancel_oids:
        if oid <= 0:
            continue
        try:
            rejected = _cancel_rejection(adapter.cancel_trigger_order(symbol, oid))
            if rejected:
                cancel_failed_oids.append(oid)
                cancel_errors.append(f"{oid}: {rejected}")
                print(f"[WARN] cancel_trigger_order({symbol}, {oid}) rejected: {rejected}", file=sys.stderr)
                continue
            cancel_succeeded_oids.append(oid)
        except Exception as ce:
            cancel_failed_oids.append(oid)
            cancel_errors.append(f"{oid}: {ce}")
            print(f"[WARN] cancel_trigger_order({symbol}, {oid}) failed: {ce}", file=sys.stderr)
    return "; ".join(cancel_errors), bool(cancel_succeeded_oids), cancel_succeeded_oids, cancel_failed_oids


def _cancel_rejection(response):
    if not isinstance(response, dict):
        return ""
    if response.get("status") not in (None, "ok"):
        return str(response)
    data = response.get("response", {})
    data = data.get("data", {}) if isinstance(data, dict) else {}
    statuses = data.get("statuses") if isinstance(data, dict) else None
    for st in statuses or []:
        if isinstance(st, dict) and "error" in st:
            return str(st["error"])
    return ""


def _fill_covers_requested_size(total_sz, requested_sz, min_fill=None):
    if not _finite_positive(total_sz):
        return False
    if min_fill is not None:
        return _finite_positive(min_fill) and total_sz >= min_fill - 1e-9
    if requested_sz is None:
        return True
    return total_sz >= requested_sz - 1e-9


def _finite_positive(value):
    try:
        v = float(value)
    except (TypeError, ValueError):
        return False
    return math.isfinite(v) and v > 0


SIZED_CLOSE_MODES = ("reduce_only", "cross")


def sized_close_args_error(args):
    if args.close_mode not in SIZED_CLOSE_MODES:
        return f"invalid --close-mode {args.close_mode!r}, expected one of {', '.join(SIZED_CLOSE_MODES)}"
    if args.side not in ("buy", "sell"):
        return f"--close-mode requires --side=buy or --side=sell, got {args.side!r}"
    if not _finite_positive(args.sz):
        return "--close-mode requires --sz > 0"
    cancel_oids = [oid for oid in args.cancel_stop_loss_oid if oid > 0]
    if cancel_oids and not args.cancel_protection_after_close:
        return "--close-mode cancels protection only after the fill (--cancel-protection-after-close)"
    if cancel_oids and not _finite_positive(args.cancel_min_fill):
        return "--close-mode with trigger cancels requires --cancel-min-fill > 0"
    return ""


def _extract_sized_close_fill(result):
    if not isinstance(result, dict):
        return None, f"exchange returned no usable order response: {result!r}", "unknown"
    if result.get("status") != "ok":
        return None, f"exchange rejected order: {result}", "rejected"
    response = result.get("response")
    data = response.get("data") if isinstance(response, dict) else None
    statuses = data.get("statuses") if isinstance(data, dict) else None
    if not isinstance(statuses, list) or not statuses:
        return None, "exchange returned no order status", "unknown"
    status = statuses[0]
    if not isinstance(status, dict):
        return None, "exchange returned a malformed order status", "unknown"
    if "error" in status:
        return None, f"exchange rejected order: {status['error']}", "rejected"
    filled = status.get("filled")
    if not isinstance(filled, dict):
        return None, f"exchange returned no filled status (status keys={list(status.keys())})", "unknown"
    raw_avg_px = filled.get("avgPx")
    raw_total_sz = filled.get("totalSz")
    try:
        avg_px = float(raw_avg_px)
        total_sz = float(raw_total_sz)
    except (TypeError, ValueError):
        return None, f"exchange returned malformed fill values (avgPx={raw_avg_px!r}, totalSz={raw_total_sz!r})", "unknown"
    if not math.isfinite(avg_px) or not math.isfinite(total_sz):
        return None, f"exchange returned malformed fill values (avgPx={raw_avg_px!r}, totalSz={raw_total_sz!r})", "unknown"
    if total_sz == 0:
        return None, "exchange returned no confirmed fill (sz=0)", "rejected"
    if avg_px <= 0 or total_sz < 0:
        return None, f"exchange returned no confirmed fill (sz={total_sz:.8f} px={avg_px:.8f})", "unknown"
    fill = {"avg_px": avg_px, "total_sz": total_sz}
    oid = filled.get("oid")
    if oid is not None:
        try:
            fill["oid"] = int(oid)
        except (TypeError, ValueError):
            print(f"[WARN] ignoring malformed fill oid={oid!r}", file=sys.stderr)
    fee = filled.get("fee")
    if fee is not None:
        try:
            parsed_fee = float(fee)
            if math.isfinite(parsed_fee):
                fill["fee"] = parsed_fee
        except (TypeError, ValueError):
            print(f"[WARN] ignoring malformed fill fee={fee!r}", file=sys.stderr)
    return fill, "", "filled"


def run_sized_close(args):
    symbol = args.symbol
    arg_err = sized_close_args_error(args)
    if arg_err:
        _emit_error(symbol, arg_err, order_outcome="not_sent")
        return
    is_buy = args.side == "buy"
    submitted_sz = 0.0
    try:
        from adapter import HyperliquidExchangeAdapter
        adapter = HyperliquidExchangeAdapter()
        submitted_sz = adapter.floor_size(symbol, args.sz)
        if not _finite_positive(submitted_sz):
            _emit_error(symbol, f"sized close {args.sz} for {symbol} floors to zero lots; no order sent and no protection cancelled",
                        order_outcome="not_sent")
            return
        px = adapter.sized_close_price(symbol, is_buy)
    except Exception as e:
        traceback.print_exc(file=sys.stderr)
        _emit_error(symbol, f"sized close {symbol} preflight failed ({e}); no order sent and no protection cancelled",
                    order_outcome="not_sent")
        return

    fills_since_ms = int(time.time() * 1000) - 10_000
    try:
        result = adapter.market_close_sized(symbol, is_buy, submitted_sz, px, reduce_only=(args.close_mode == "reduce_only"))
    except Exception as e:
        traceback.print_exc(file=sys.stderr)
        _emit_error(symbol, str(e), order_outcome="unknown", submitted_sz=submitted_sz)
        return

    fill, fill_error, order_outcome = _extract_sized_close_fill(result)
    if fill_error:
        _emit_error(symbol, fill_error, order_outcome=order_outcome, submitted_sz=submitted_sz)
        return

    cancel_err = ""
    cancel_succeeded = False
    cancel_succeeded_oids = []
    cancel_failed_oids = []
    if _fill_covers_requested_size(fill["total_sz"], submitted_sz, args.cancel_min_fill):
        cancel_err, cancel_succeeded, cancel_succeeded_oids, cancel_failed_oids = _cancel_trigger_orders(
            adapter, symbol, args.cancel_stop_loss_oid
        )

    if fill.get("oid"):
        try:
            lookup = adapter.lookup_fill_fee_by_oid(fill["oid"], fills_since_ms)
            if not lookup:
                print(f"[WARN] userFills lookup returned no fills for oid={fill['oid']}", file=sys.stderr)
            elif not apply_user_fills_lookup(fill, lookup):
                print(f"[WARN] userFills lookup returned malformed fill data for oid={fill['oid']}", file=sys.stderr)
        except Exception as fe:
            print(f"[WARN] userFills lookup failed for oid={fill['oid']}: {fe}", file=sys.stderr)
    _emit_success(symbol, fill, cancel_err=cancel_err, cancel_succeeded=cancel_succeeded,
                  cancel_succeeded_oids=cancel_succeeded_oids, cancel_failed_oids=cancel_failed_oids,
                  order_outcome="filled", submitted_sz=submitted_sz)


def _emit_success(symbol, fill, already_flat=False, cancel_err="", cancel_succeeded=False,
                  cancel_succeeded_oids=None, cancel_failed_oids=None, order_outcome="", submitted_sz=None):
    close = {"symbol": symbol, "fill": fill}
    if already_flat:
        close["already_flat"] = True
    if submitted_sz is not None:
        close["submitted_sz"] = submitted_sz
    out = {
        "close": close,
        "platform": "hyperliquid",
        "timestamp": datetime.now(timezone.utc).isoformat(),
    }
    if order_outcome:
        out["order_outcome"] = order_outcome
    if cancel_err:
        out["cancel_stop_loss_error"] = cancel_err
    if cancel_succeeded:
        out["cancel_stop_loss_succeeded"] = True
    if cancel_succeeded_oids:
        out["cancel_stop_loss_succeeded_oids"] = cancel_succeeded_oids
    if cancel_failed_oids:
        out["cancel_stop_loss_failed_oids"] = cancel_failed_oids
    print(json.dumps(out))


def _emit_error(symbol, message, cancel_err="", cancel_succeeded=False,
                cancel_succeeded_oids=None, cancel_failed_oids=None, order_outcome="", submitted_sz=None):
    close = {"symbol": symbol, "fill": {}}
    if submitted_sz is not None:
        close["submitted_sz"] = submitted_sz
    out = {
        "close": close,
        "platform": "hyperliquid",
        "timestamp": datetime.now(timezone.utc).isoformat(),
        "error": message,
    }
    if order_outcome:
        out["order_outcome"] = order_outcome
    if cancel_err:
        out["cancel_stop_loss_error"] = cancel_err
    if cancel_succeeded:
        out["cancel_stop_loss_succeeded"] = True
    if cancel_succeeded_oids:
        out["cancel_stop_loss_succeeded_oids"] = cancel_succeeded_oids
    if cancel_failed_oids:
        out["cancel_stop_loss_failed_oids"] = cancel_failed_oids
    print(json.dumps(out))
    sys.exit(1)


if __name__ == "__main__":
    main()
