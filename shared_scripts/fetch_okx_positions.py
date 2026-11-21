#!/usr/bin/env python3

import argparse
import importlib.util
import json
import math
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


STRICT_CONTRACT = "okx_swap_positions_strict_v1"
STRICT_POS_SIDES = ("long", "short", "net")
STRICT_MARGIN_MODES = ("cross", "isolated")


def strict_position_rows(raw):
    if not isinstance(raw, list):
        raise ValueError(f"position data is not a list: {raw!r}")
    rows = []
    seen = set()
    for i, p in enumerate(raw):
        if not isinstance(p, dict):
            raise ValueError(f"position row {i} is not an object: {p!r}")
        inst_id = p.get("instId")
        inst_type = p.get("instType")
        pos_side = p.get("posSide")
        margin_mode = p.get("mgnMode")
        pos = p.get("pos")
        if not isinstance(inst_id, str) or not inst_id or not inst_id.endswith("-SWAP"):
            raise ValueError(f"position row {i} has no swap instrument id: {inst_id!r}")
        if inst_type != "SWAP":
            raise ValueError(f"position row {i} ({inst_id}) has instrument type {inst_type!r}")
        if pos_side not in STRICT_POS_SIDES:
            raise ValueError(f"position row {i} ({inst_id}) has position side {pos_side!r}")
        if margin_mode not in STRICT_MARGIN_MODES:
            raise ValueError(f"position row {i} ({inst_id}) has margin mode {margin_mode!r}")
        if not isinstance(pos, str) or not pos.strip():
            raise ValueError(f"position row {i} ({inst_id}) has no position size: {pos!r}")
        try:
            size = float(pos)
        except ValueError as e:
            raise ValueError(f"position row {i} ({inst_id}) size {pos!r} does not parse") from e
        if not math.isfinite(size):
            raise ValueError(f"position row {i} ({inst_id}) size {pos!r} is not finite")
        key = (inst_id, pos_side, margin_mode)
        if key in seen:
            raise ValueError(f"more than one {inst_id} {pos_side} {margin_mode} position row")
        seen.add(key)
        rows.append({
            "inst_id": inst_id,
            "inst_type": inst_type,
            "pos_side": pos_side,
            "margin_mode": margin_mode,
            "pos": pos,
        })
    return rows


def strict_main():
    try:
        from adapter import OKXExchangeAdapter
        adapter = OKXExchangeAdapter()
        if not adapter.is_live:
            _emit_error("OKX adapter not live — set OKX_API_KEY / OKX_API_SECRET / OKX_PASSPHRASE", strict=True)
            return
        account = _okx_account_key_module().okx_account_key("swap")
        if not account:
            _emit_error("no OKX swap account identity (OKX_API_KEY unset)", strict=True)
            return
        rows = strict_position_rows(adapter.fetch_swap_positions_raw())
    except Exception as e:
        traceback.print_exc(file=sys.stderr)
        _emit_error(str(e), strict=True)
        return
    print(json.dumps({
        "contract": STRICT_CONTRACT,
        "account": account,
        "positions": rows,
        "platform": "okx",
        "timestamp": datetime.now(timezone.utc).isoformat(),
    }))


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--strict", action="store_true",
                        help="emit the raw authenticated swap position rows under the strict kill-switch contract")
    args = parser.parse_args()
    if args.strict:
        strict_main()
        return
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


def _emit_error(message, strict=False):
    out = {
        "positions": [],
        "platform": "okx",
        "timestamp": datetime.now(timezone.utc).isoformat(),
        "error": message,
    }
    if strict:
        out["contract"] = STRICT_CONTRACT
    print(json.dumps(out))
    sys.exit(1)


if __name__ == "__main__":
    main()
