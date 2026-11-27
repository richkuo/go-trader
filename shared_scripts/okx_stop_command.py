import json
import os
import sys

_ROOT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..")
sys.path.insert(0, os.path.join(_ROOT, "shared_tools"))
sys.path.insert(0, os.path.join(_ROOT, "platforms", "okx"))

from adapter import OKXExchangeAdapter

OKX_STOP_CONTRACT = "okx_stop_v1"


def probe_payload():
    return {"contract": OKX_STOP_CONTRACT, "probe": True}


def _emit(payload):
    print(json.dumps(payload))


def _base(op, ok, reason=""):
    return {"contract": OKX_STOP_CONTRACT, "op": op, "ok": bool(ok), "reason": reason}


def _inst_id(symbol, inst_type):
    if inst_type != "swap":
        raise RuntimeError("OKX exchange stops are swap only")
    coin = str(symbol or "").upper()
    if not coin:
        raise RuntimeError("symbol is required")
    return coin + "-USDT-SWAP"


def run_okx_stop(args):
    op = args.op
    try:
        if op == "public-instrument":
            adapter = OKXExchangeAdapter(public_only=True)
            inst = _inst_id(args.symbol, args.inst_type)
            planned = args.contracts or args.min_contracts or ""
            ref = adapter.public_stop_reference(inst, planned, args.round_entry == "true")
            out = _base(op, True)
            out.update(ref)
            out["fee_source"] = "okx-perps platform taker fee"
            _emit(out)
            return
        adapter = OKXExchangeAdapter()
        inst = _inst_id(args.symbol, args.inst_type)
        if op == "preflight":
            ref = adapter.stop_preflight(inst, args.contracts)
            out = _base(op, True)
            out.update(ref)
            if ref["pos_mode"] != "net_mode" or not ref["flat"] or not ref["cross"] or ref["open_orders"] or ref["open_algos"] or ref["other_positions"]:
                out["ok"] = False
                out["reason"] = "account is not a flat net-mode cross swap book for this instrument"
            _emit(out)
            return
        if op == "position":
            out = _base(op, True)
            out.update(adapter.stop_position(inst))
            _emit(out)
            return
        if op == "arm-reference":
            out = _base(op, True)
            out.update(adapter.arm_reference(inst))
            _emit(out)
            return
        if op == "place":
            placed = adapter.stop_place(inst, args.position_side, args.contracts, args.trigger_px, args.algo_client_id)
            out = _base(op, True)
            out.update(placed)
            _emit(out)
            return
        if op == "read":
            found = adapter.stop_read(inst, args.algo_client_id)
            out = _base(op, True)
            out.update(found)
            _emit(out)
            return
        if op == "amend":
            amended = adapter.stop_amend(inst, args.algo_id, args.algo_client_id, args.trigger_px, args.contracts)
            out = _base(op, True)
            out.update(amended)
            _emit(out)
            return
        if op == "cancel":
            canceled = adapter.stop_cancel(inst, args.algo_id, args.algo_client_id)
            out = _base(op, True)
            out.update(canceled)
            _emit(out)
            return
        if op == "reduce":
            adapter.stop_reduce(inst, args.position_side, args.contracts, args.order_id, args.exp_time_ms)
            _emit(_base(op, True))
            return
        if op == "reduce-fill":
            found = adapter.stop_reduce_fill(inst, args.order_id)
            out = _base(op, True)
            if found.get("absent"):
                out["order_absent"] = True
            else:
                out["fills"] = [found]
            _emit(out)
            return
        if op == "fills":
            out = _base(op, True)
            out["fills"] = adapter.stop_fills(inst, args.order_id)
            try:
                out["child_terminal"] = adapter.stop_child_terminal(inst, args.order_id)
            except Exception:
                out["child_terminal"] = False
            _emit(out)
            return
        raise RuntimeError(f"unknown OKX stop op {op}")
    except Exception as exc:
        _emit(_base(op, False, str(exc)))
        sys.exit(1)
