import importlib.util
import json
import os
import sys
import time
from decimal import Decimal

EVIDENCE_VERSION = 1
OKX_TERMINAL_STATES = ("filled", "canceled", "mmp_canceled")
OKX_READ_ATTEMPTS = 3
OKX_READ_DELAY_S = 0.25


def _pair_orders():
    name = "go_trader_pair_orders"
    mod = sys.modules.get(name)
    if mod is None:
        path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "pair_orders.py")
        spec = importlib.util.spec_from_file_location(name, path)
        mod = importlib.util.module_from_spec(spec)
        sys.modules[name] = mod
        spec.loader.exec_module(mod)
    return mod


def _evidence(order_ids, accepted_at_ms, executed, fills, complete, detail):
    po = _pair_orders()
    return {
        "v": EVIDENCE_VERSION,
        "venue_order_ids": [str(o) for o in order_ids],
        "accepted_at_ms": int(accepted_at_ms or 0),
        "executed_native_qty": po.decimal_text(executed) if executed is not None else "",
        "fills": fills,
        "complete": bool(complete),
        "detail": "" if complete else (detail or "the venue fill history is incomplete"),
    }


def _failed(order_ids, detail):
    return {
        "v": EVIDENCE_VERSION,
        "venue_order_ids": [str(o) for o in order_ids if o not in (None, "")],
        "accepted_at_ms": 0,
        "executed_native_qty": "",
        "fills": [],
        "complete": False,
        "detail": detail,
    }


def _dedupe(rows, key_field, what):
    by_key = {}
    order = []
    for row in rows:
        key = row.get(key_field)
        if key in (None, ""):
            raise ValueError(f"a {what} row carries no {key_field}")
        key = str(key)
        body = json.dumps(row, sort_keys=True, separators=(",", ":"), default=str)
        if key in by_key:
            if by_key[key][1] != body:
                raise ValueError(f"{what} {key} was returned twice with different bodies")
            continue
        by_key[key] = (row, body)
        order.append(key)
    return [(k, by_key[k][0]) for k in order]


def hyperliquid_close_evidence(oid, total_sz, raw_fills):
    try:
        po = _pair_orders()
        if oid in (None, ""):
            return _failed([], "the close response named no order id")
        executed = po.exact_decimal(str(total_sz), "totalSz")
        rows = [f for f in (raw_fills or []) if isinstance(f, dict) and str(f.get("oid")) == str(oid)]
        fills = []
        for key, f in _dedupe(rows, "tid", "fill"):
            fee_known = f.get("fee") not in (None, "") and bool(str(f.get("feeToken") or "").strip())
            side = {"B": "buy", "A": "sell"}.get(f.get("side"), "")
            fills.append(po.fill_record(key, f.get("time"), f.get("sz"), f.get("px"), f.get("fee"), f.get("feeToken"), fee_known,
                                        order_id=str(oid), side=side))
        fills.sort(key=lambda r: (r["venue_time_ms"], r["fill_id"]))
        qty, _ = po.sum_fills(fills)
        complete = bool(fills) and qty == executed
        detail = ""
        if not fills:
            detail = "the user fill history returned no fill for the close order"
        elif qty != executed:
            detail = f"the fill records sum to {qty}, the venue reports {executed} executed"
        return _evidence([oid], 0, executed, fills, complete, detail)
    except Exception as e:
        return _failed([oid], f"the close fill evidence is unreadable: {e}")


def _okx_order_fills(po, adapter, symbol, oid):
    trades = adapter.fetch_order_trades(symbol, "swap", oid)
    rows = []
    for t in trades or []:
        info = t.get("info") if isinstance(t, dict) else None
        rows.append(info if isinstance(info, dict) else t)
    rows = [r for r in rows if isinstance(r, dict) and str(r.get("ordId") or "") == str(oid)]
    out = []
    for key, row in _dedupe(rows, "billId", "fill"):
        raw_fee = row.get("fee")
        fee_ccy = str(row.get("feeCcy") or "").strip()
        fee_known = raw_fee not in (None, "") and bool(fee_ccy)
        fee = ""
        if fee_known:
            fee = po.decimal_text(-po.exact_decimal(raw_fee, f"fill {key} fee", allow_negative=True))
        when = row.get("fillTime") or row.get("ts")
        out.append(po.fill_record(key, when, row.get("fillSz"), row.get("fillPx"), fee, fee_ccy, fee_known,
                                  order_id=str(oid), side=row.get("side")))
    return out


def okx_close_evidence(adapter, symbol, orders, attempts=OKX_READ_ATTEMPTS, delay_s=OKX_READ_DELAY_S):
    ids = []
    for o in orders or []:
        oid = o.get("id") if isinstance(o, dict) else None
        if oid in (None, ""):
            return _failed(ids, "a close order response named no order id")
        ids.append(str(oid))
    try:
        po = _pair_orders()
        last = ""
        for attempt in range(max(1, int(attempts))):
            fills = []
            executed = Decimal(0)
            accepted = 0
            terminal = True
            for oid in ids:
                info = adapter.fetch_order_info(symbol, "swap", oid)
                if str(info.get("ordId") or "") != oid:
                    raise ValueError(f"the order read for {oid} returned order {info.get('ordId')!r}")
                state = str(info.get("state") or "")
                if state not in OKX_TERMINAL_STATES:
                    terminal = False
                executed += po.exact_decimal(info.get("accFillSz") or "0", "accFillSz")
                c = int(info.get("cTime") or 0)
                if c > 0 and (accepted == 0 or c < accepted):
                    accepted = c
                fills.extend(_okx_order_fills(po, adapter, symbol, oid))
            fills.sort(key=lambda r: (r["venue_time_ms"], r["fill_id"]))
            qty, _ = po.sum_fills(fills)
            if qty > executed:
                return _evidence(ids, accepted, executed, fills, False, f"the fill records sum to {qty}, above the {executed} the venue reports executed")
            if terminal and qty == executed:
                return _evidence(ids, accepted, executed, fills, True, "")
            last = f"after {attempt + 1} reads the orders are {'terminal' if terminal else 'not terminal'} and fill records cover {qty} of {executed} executed"
            if attempt + 1 < attempts:
                time.sleep(delay_s)
        return _evidence(ids, accepted, executed, fills, False, last)
    except Exception as e:
        return _failed(ids, f"the close fill evidence is unreadable: {e}")
