#!/usr/bin/env python3
"""
Page through Hyperliquid userFills history and emit an OID-keyed map of real
exchange fees and closed PnL.

Used by `go-trader backfill hl-fees` (issue #589) to replay live fills against
the trades table and rewrite `exchange_fee` / `realized_pnl` for rows that were
written before #587 — when HL's order placement response did not surface the
real fee.

Args:
    --since-ms <int>: lower bound (ms epoch) for the userFills query
    [--end-ms <int>]: upper bound (defaults to "now")

Stdout (always JSON):
    {
        "by_oid": {"<oid>": {"fee": float, "closed_pnl": float, "count": int}, ...},
        "fill_count": int,
        "page_count": int,
        "account_address": "0x...",
        "error": ""
    }

Exits 1 on error (stdout still contains a JSON envelope with "error" set).
"""

import argparse
import json
import os
import sys
import time
import traceback


sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "platforms", "hyperliquid"))


# HL's user_fills_by_time returns at most ~2000 rows per call; we page forward
# from the last fill's `time` field. PAGE_LIMIT_HARD caps total requests so a
# pathological response (e.g. a stuck cursor) can't loop forever.
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

    # Aggregate across pages: a single market order can fragment into multiple
    # partial fills, all sharing the same OID — sum fee + closedPnl across
    # those rows so the OID-keyed map gives the true total.
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

        # Track which fills we already consumed at the exact cursor_ms
        # boundary so a partial fill landing on the same ms as the cursor
        # isn't double counted on the next loop (HL's API is inclusive on
        # the lower bound). When we advance the cursor below, we re-seed
        # this set with the rows from this page that landed exactly on the
        # new cursor — those will reappear in the next response and need
        # to be skipped.
        next_cursor = cursor_ms
        new_in_page = 0
        page_rows = []  # parsed (ts, dedup_key) pairs for boundary re-seeding

        for f in page:
            if not isinstance(f, dict):
                continue
            oid = _safe_int(f.get("oid"))
            ts = _safe_int(f.get("time"))
            tid = f.get("tid")
            # Dedup key — within one ms bucket, HL's tid (trade id) uniquely
            # identifies a leg; fall back to (oid, sz, px) when tid is absent.
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
                if entry is None:
                    entry = {"fee": 0.0, "closed_pnl": 0.0, "count": 0}
                    by_oid[key] = entry
                entry["fee"] += fee
                entry["closed_pnl"] += closed_pnl
                entry["count"] += 1
            fill_count += 1
            new_in_page += 1
            if ts > next_cursor:
                next_cursor = ts

        # No new rows means we're done (cursor stuck on rows we've already
        # consumed at this ms).
        if new_in_page == 0:
            break

        # Advance cursor. If next_cursor moved forward, re-seed the dedup
        # set with the boundary rows from this page that share the new
        # cursor's timestamp — HL's user_fills_by_time is inclusive on the
        # lower bound, so those rows will reappear in the next response.
        if next_cursor > cursor_ms:
            seen_first_ts_at_cursor = {dk for (ts, dk) in page_rows if ts == next_cursor}
            cursor_ms = next_cursor
        else:
            # HL returned a full page all at the same ms — push past it to
            # avoid an infinite loop.
            cursor_ms = next_cursor + 1
            seen_first_ts_at_cursor = set()

        if cursor_ms > end_ms:
            break

    _emit({
        "by_oid": by_oid,
        "fill_count": fill_count,
        "page_count": page_count,
        "account_address": addr,
        "error": "",
    }, exit_code=0)


if __name__ == "__main__":
    main()
