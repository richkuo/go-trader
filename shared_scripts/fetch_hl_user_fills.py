#!/usr/bin/env python3

import argparse
import json
import os
import sys
import time
import traceback


sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "platforms", "hyperliquid"))


PAGE_LIMIT_HARD = 200


def _safe_int(v):
    try:
        return int(v)
    except (TypeError, ValueError):
        return 0


def _safe_float(v):
    try:
        return float(v)
    except (TypeError, ValueError):
        return 0.0


def _safe_coin(v):
    if not isinstance(v, str):
        return ""
    return v.strip().upper()


def _emit(payload: dict, exit_code: int = 0):
    print(json.dumps(payload))
    sys.exit(exit_code)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--since-ms", type=int, required=True)
    parser.add_argument("--end-ms", type=int, default=0,
                        help="upper bound (ms epoch); defaults to now")
    args = parser.parse_args()

    since_ms = args.since_ms
    end_ms = args.end_ms or int(time.time() * 1000)

    if since_ms <= 0:
        _emit({
            "by_oid": {},
            "fill_count": 0,
            "page_count": 0,
            "account_address": "",
            "error": "--since-ms must be > 0",
        }, exit_code=1)

    try:
        from adapter import HyperliquidExchangeAdapter
        adapter = HyperliquidExchangeAdapter()
    except Exception as e:
        traceback.print_exc(file=sys.stderr)
        _emit({
            "by_oid": {},
            "fill_count": 0,
            "page_count": 0,
            "account_address": "",
            "error": f"failed to init HL adapter: {e}",
        }, exit_code=1)

    addr = adapter._account_address
    if not addr:
        _emit({
            "by_oid": {},
            "fill_count": 0,
            "page_count": 0,
            "account_address": "",
            "error": "HYPERLIQUID_ACCOUNT_ADDRESS not set (and no HYPERLIQUID_SECRET_KEY to derive it)",
        }, exit_code=1)

    by_oid: dict = {}
    fill_count = 0
    page_count = 0
    cursor_ms = since_ms
    seen_first_ts_at_cursor = set()

    while page_count < PAGE_LIMIT_HARD:
        page_count += 1
        try:
            page = adapter._info.user_fills_by_time(addr, cursor_ms, end_ms)
        except Exception as e:
            traceback.print_exc(file=sys.stderr)
            _emit({
                "by_oid": by_oid,
                "fill_count": fill_count,
                "page_count": page_count,
                "account_address": addr,
                "error": f"user_fills_by_time failed at page {page_count}: {e}",
            }, exit_code=1)

        if not isinstance(page, list) or not page:
            break

        next_cursor = cursor_ms
        new_in_page = 0
        page_rows = []

        for f in page:
            if not isinstance(f, dict):
                continue
            oid = _safe_int(f.get("oid"))
            ts = _safe_int(f.get("time"))
            tid = f.get("tid")
            dedup_key = (ts, oid, tid if tid is not None else (
                _safe_float(f.get("sz")), _safe_float(f.get("px"))))
            if ts == cursor_ms and dedup_key in seen_first_ts_at_cursor:
                continue
            if ts == cursor_ms:
                seen_first_ts_at_cursor.add(dedup_key)
            page_rows.append((ts, dedup_key))

            fee = _safe_float(f.get("fee"))
            closed_pnl = _safe_float(f.get("closedPnl"))
            if oid > 0:
                key = str(oid)
                entry = by_oid.get(key)
                coin = _safe_coin(f.get("coin"))
                if entry is None:
                    entry = {"coin": coin, "fee": 0.0, "closed_pnl": 0.0, "count": 0,
                             "qty": 0.0, "_px_num": 0.0,
                             "first_time_ms": ts if ts > 0 else 0,
                             "last_time_ms": ts if ts > 0 else 0}
                    by_oid[key] = entry
                elif coin and entry.get("coin") and coin != entry.get("coin"):
                    entry["coin"] = ""
                elif coin and not entry.get("coin"):
                    entry["coin"] = coin
                entry["fee"] += fee
                entry["closed_pnl"] += closed_pnl
                entry["count"] += 1
                if ts > 0:
                    first = _safe_int(entry.get("first_time_ms"))
                    last = _safe_int(entry.get("last_time_ms"))
                    entry["first_time_ms"] = ts if first <= 0 else min(first, ts)
                    entry["last_time_ms"] = max(last, ts)
                sz = _safe_float(f.get("sz"))
                entry["qty"] += sz
                entry["_px_num"] += sz * _safe_float(f.get("px"))
            fill_count += 1
            new_in_page += 1
            if ts > next_cursor:
                next_cursor = ts

        if new_in_page == 0:
            break

        if next_cursor > cursor_ms:
            seen_first_ts_at_cursor = {dk for (ts, dk) in page_rows if ts == next_cursor}
            cursor_ms = next_cursor
        else:
            cursor_ms = next_cursor + 1
            seen_first_ts_at_cursor = set()

        if cursor_ms > end_ms:
            break

    for entry in by_oid.values():
        num = entry.pop("_px_num", 0.0)
        qty = entry.get("qty", 0.0)
        entry["px"] = (num / qty) if qty > 0 else 0.0

    _emit({
        "by_oid": by_oid,
        "fill_count": fill_count,
        "page_count": page_count,
        "account_address": addr,
        "error": "",
    }, exit_code=0)


if __name__ == "__main__":
    main()
