#!/usr/bin/env python3

import importlib.util
import json
import os
import sys
import traceback
from datetime import datetime, timezone


sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "platforms", "okx"))


def _okx_account_key_module():
    name = "go_trader_okx_account_key"
    mod = sys.modules.get(name)
    if mod is None:
        path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "shared_tools", "okx_account_key.py")
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

    account = _okx_account_key_module().okx_account_key("swap")
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


def _emit_error(message):
    print(json.dumps({
        "positions": [],
        "platform": "okx",
        "timestamp": datetime.now(timezone.utc).isoformat(),
        "error": message,
    }))
    sys.exit(1)


if __name__ == "__main__":
    main()
