#!/usr/bin/env python3

import importlib.util
import json
import os
import sys
import traceback
from datetime import datetime, timezone


sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "platforms", "okx"))


def _pair_orders():
    name = "go_trader_pair_orders"
    mod = sys.modules.get(name)
    if mod is None:
        path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "shared_tools", "pair_orders.py")
        spec = importlib.util.spec_from_file_location(name, path)
        mod = importlib.util.module_from_spec(spec)
        sys.modules[name] = mod
        spec.loader.exec_module(mod)
    return mod


def _float_field(value):
    try:
        return float(value or 0)
    except (TypeError, ValueError):
        return 0.0


def main():
    try:
        from adapter import OKXExchangeAdapter
        adapter = OKXExchangeAdapter()
        if not adapter.is_live:
            _emit_error("OKX adapter not live — set OKX_API_KEY / OKX_API_SECRET / OKX_PASSPHRASE")
            return
        raw = adapter.fetch_open_positions()
    except Exception as e:
        traceback.print_exc(file=sys.stderr)
        _emit_error(str(e))
        return

    account = _pair_orders().okx_account_key("swap")
    positions = []
    for p in raw or []:
        try:
            contracts = float(p.get("contracts") or 0)
        except (TypeError, ValueError):
            continue
        if contracts == 0:
            continue
        symbol = p.get("symbol") or ""
        coin = symbol.split("/", 1)[0] if "/" in symbol else symbol
        if not coin:
            continue
        side = (p.get("side") or "").lower()
        signed_size = -contracts if side == "short" else contracts
        info = p.get("info") if isinstance(p.get("info"), dict) else {}
        positions.append({
            "coin": coin,
            "size": signed_size,
            "entry_price": _float_field(p.get("entryPrice")),
            "side": side,
            "unrealized_pnl": _float_field(p.get("unrealizedPnl")),
            "inst_id": str(info.get("instId") or ""),
            "contracts": contracts,
            "contract_size": _float_field(p.get("contractSize")),
            "margin_mode": str(p.get("marginMode") or info.get("mgnMode") or ""),
            "account": account,
        })

    print(json.dumps({
        "positions": positions,
        "platform": "okx",
        "timestamp": datetime.now(timezone.utc).isoformat(),
    }))


def run_pair_position(raw, probe_only):
    po = _pair_orders()
    try:
        req = po.parse_position_request(raw, "okx")
    except po.PairOrderError as e:
        po.finish({"platform": "okx", "error": str(e)}, 1)
        return
    if probe_only:
        sys.exit(0)
    try:
        from adapter import OKXExchangeAdapter
        adapter = OKXExchangeAdapter()
        account = po.okx_account_key("swap")
        if account != req["account_key"]:
            raise ValueError(f"the credentials answer for account {account!r}, the request is bound to {req['account_key']!r}")
        inst = adapter.pair_instrument(req["instrument"])
        pos = adapter.fetch_pair_position(req["instrument"])
        observed = po.now_ms()
        qty = po.decimal_text(po.exact_decimal(pos["pos"], "pos", allow_negative=True))
        size = po.decimal_text(po.exact_decimal(inst["ctVal"], "ctVal", positive=True))
    except Exception as e:
        traceback.print_exc(file=sys.stderr)
        po.finish({"platform": "okx", "error": f"strict pair position read failed: {e}"}, 1)
        return
    po.finish({"platform": "okx", "account": account, "pair_position": {
        "v": po.POSITION_OUTPUT_VERSION, "account_key": account, "instrument": req["instrument"], "native_unit": "contract",
        "contract_size": size, "signed_native_qty": qty, "position_mode": pos["pos_side"], "margin_mode": pos["margin_mode"],
        "found": bool(pos["found"]), "observed_ms": observed}})


def _emit_error(message):
    print(json.dumps({
        "positions": [],
        "platform": "okx",
        "timestamp": datetime.now(timezone.utc).isoformat(),
        "error": message,
    }))
    sys.exit(1)


if __name__ == "__main__":
    pair = [a for a in sys.argv[1:] if a.startswith("--pair-position-json")]
    if pair:
        import argparse
        parser = argparse.ArgumentParser()
        parser.add_argument("--pair-position-json", required=True)
        parser.add_argument("--mode", default="live", choices=["live"])
        parser.add_argument("--probe-only", action="store_true",
                            help="Startup compatibility probe (#1877): parse the strict request and exit 0 before any adapter or venue call.")
        args = parser.parse_args()
        run_pair_position(args.pair_position_json, args.probe_only)
    else:
        main()
