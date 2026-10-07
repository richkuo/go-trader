#!/usr/bin/env python3

import argparse
import calendar
import datetime
import hashlib
import json
import os
import sys
from decimal import Decimal, ROUND_HALF_EVEN

_HERE = os.path.dirname(os.path.abspath(__file__))
if _HERE not in sys.path:
    sys.path.insert(0, _HERE)

import offline_manifest as om

SCHEMA = "go-trader.resting-tp-fill-study"
SCHEMA_VERSION = 1
CAPTURE_SCHEMA = "go-trader.resting-tp-capture"
TICK_CAP = 10000
BUCKETS = ("0", "1", "2-4", "5-9", "10+")
TERMINAL_STATUSES = frozenset({
    "filled", "canceled", "cancelled", "rejected", "expired", "margincanceled", "triggered",
})
CANCEL_STATUSES = frozenset({"canceled", "cancelled", "margincanceled"})


def status_key(status) -> str:
    return str(status or "").lower()


def is_terminal_status(status) -> bool:
    key = status_key(status)
    if key in TERMINAL_STATUSES:
        return True
    return (
        key.endswith("canceled") or key.endswith("cancelled")
        or key.endswith("rejected") or key.endswith("cancel")
    )


def is_cancel_status(status) -> bool:
    key = status_key(status)
    if key in CANCEL_STATUSES:
        return True
    return key.endswith("canceled") or key.endswith("cancelled") or key.endswith("cancel")
RATE_CLASSES = frozenset({
    "full_fill", "partial_fill", "no_fill_touch", "no_fill_trade_through", "not_reached",
})


class StudyError(Exception):
    pass


def sha256_file(path: str) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as fh:
        for chunk in iter(lambda: fh.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def canonical(obj) -> str:
    return json.dumps(obj, sort_keys=True, separators=(",", ":"), ensure_ascii=True)


def dump(obj) -> str:
    return json.dumps(obj, sort_keys=True, indent=2) + "\n"


def sha256_text(text: str) -> str:
    return hashlib.sha256(text.encode("utf-8")).hexdigest()


def evidence(node):
    if isinstance(node, dict) and node.get("status") == "available":
        return node.get("value")
    return None


def parse_time_ms(text):
    if not isinstance(text, str) or len(text) != 20 or not text.endswith("Z"):
        raise StudyError(f"ledger timestamp must be whole-second UTC, got {text!r}")
    if text[4] != "-" or text[7] != "-" or text[10] != "T" or text[13] != ":" or text[16] != ":":
        raise StudyError(f"ledger timestamp must be whole-second UTC, got {text!r}")
    dt = datetime.datetime(
        int(text[0:4]), int(text[5:7]), int(text[8:10]),
        int(text[11:13]), int(text[14:16]), int(text[17:19]))
    return calendar.timegm(dt.timetuple()) * 1000


def dec(value, label):
    try:
        out = Decimal(str(value))
    except Exception as exc:
        raise StudyError(f"{label} is not a decimal: {value!r}") from exc
    if not out.is_finite():
        raise StudyError(f"{label} is not finite: {value!r}")
    return out


def floor_log10(px: Decimal) -> int:
    sign, digits, exp = px.normalize().as_tuple()
    if sign or not digits:
        raise StudyError("price must be positive")
    return exp + len(digits) - 1


def perps_decimals(px: Decimal, sz_decimals: int) -> int:
    sig = max(0, 4 - floor_log10(px))
    px_dec = max(0, 6 - sz_decimals)
    return min(px_dec, sig)


def on_tick_grid(px: Decimal, sz_decimals: int) -> bool:
    places = perps_decimals(px, sz_decimals)
    quantum = Decimal(1).scaleb(-places)
    rounded = px.quantize(quantum, rounding=ROUND_HALF_EVEN)
    return rounded == px


def step_at(px: Decimal, sz_decimals: int) -> Decimal:
    return Decimal(1).scaleb(-perps_decimals(px, sz_decimals))


def ticks_through(extreme: Decimal, limit: Decimal, side: str, sz_decimals: int) -> int:
    if side == "long":
        if extreme < limit:
            return 0
        cursor = limit
        ticks = 0
        while ticks < TICK_CAP:
            nxt = cursor + step_at(cursor, sz_decimals)
            if extreme + Decimal(0) < nxt:
                break
            cursor = nxt
            ticks += 1
        return ticks
    if extreme > limit:
        return 0
    cursor = limit
    ticks = 0
    while ticks < TICK_CAP:
        nxt = cursor - step_at(cursor - step_at(cursor, sz_decimals), sz_decimals)
        if nxt >= cursor:
            nxt = cursor - step_at(cursor, sz_decimals)
        if extreme > nxt:
            break
        cursor = nxt
        ticks += 1
    return ticks


def bps_through(extreme: Decimal, limit: Decimal, side: str):
    if limit <= 0:
        return Decimal(0)
    if side == "long":
        raw = (extreme - limit) / limit * Decimal(10000)
    else:
        raw = (limit - extreme) / limit * Decimal(10000)
    if raw < 0:
        return Decimal(0)
    return raw


def bucket_name(n: int) -> str:
    if n <= 0:
        return "0"
    if n == 1:
        return "1"
    if n <= 4:
        return "2-4"
    if n <= 9:
        return "5-9"
    return "10+"


def book_side(text):
    raw = str(text or "").lower()
    if raw in ("buy", "long", "b"):
        return "long"
    if raw in ("sell", "short", "a"):
        return "short"
    return None


def venue_order_side(text):
    raw = str(text or "")
    if raw in ("A", "sell", "Sell"):
        return "sell"
    if raw in ("B", "buy", "Buy"):
        return "buy"
    return None


def take_profit_venue_side(position_side):
    return "sell" if position_side == "long" else "buy"


def unwrap_status(doc):
    if not isinstance(doc, dict):
        return None
    outer = doc.get("order")
    if isinstance(outer, dict) and isinstance(outer.get("order"), dict):
        return {
            "order": outer["order"],
            "status": outer.get("status"),
            "status_time": outer.get("statusTimestamp"),
        }
    if isinstance(outer, dict) and "oid" in outer:
        return {
            "order": outer,
            "status": doc.get("status"),
            "status_time": doc.get("statusTimestamp"),
        }
    return None


def is_reduce_only_gtc(order):
    flag = order.get("reduceOnly")
    if flag is not True and str(flag).lower() != "true":
        return False
    tif = str(order.get("tif") or "")
    if tif.lower() == "gtc":
        return True
    kind = order.get("orderType")
    if isinstance(kind, dict):
        limit = kind.get("limit")
        if isinstance(limit, dict) and str(limit.get("tif") or "").lower() == "gtc":
            return True
    return False


def load_exports(paths):
    positions = []
    hashes = []
    for path in paths:
        digest = sha256_file(path)
        hashes.append(digest)
        with open(path) as fh:
            doc = json.load(fh)
        if not isinstance(doc, dict) or doc.get("schema") != "go-trader.booked-ledger":
            raise StudyError(f"{path}: schema must be go-trader.booked-ledger")
        grouped = {}
        for event in doc.get("events") or []:
            if not isinstance(event, dict):
                continue
            strategy = str(event.get("process_strategy_id") or "")
            position_id = evidence(event.get("position_id"))
            if not strategy or not position_id:
                continue
            key = (strategy, str(position_id))
            slot = grouped.setdefault(key, {
                "strategy": strategy,
                "position_id": str(position_id),
                "coin": None,
                "side": None,
                "open_ms": None,
                "close_ms": None,
                "tp_oids": [],
                "stop_oid": None,
            })
            symbol = evidence(event.get("symbol"))
            if isinstance(symbol, str) and symbol.strip():
                slot["coin"] = symbol.split("/")[0].strip()
            is_close = evidence(event.get("is_close"))
            stamp = parse_time_ms(event.get("timestamp"))
            if is_close is True:
                slot["close_ms"] = stamp if slot["close_ms"] is None else min(slot["close_ms"], stamp)
            elif is_close is False:
                slot["open_ms"] = stamp if slot["open_ms"] is None else min(slot["open_ms"], stamp)
                side = book_side(evidence(event.get("side")))
                if side:
                    slot["side"] = side
            raw_tps = evidence(event.get("tp_oids_json"))
            if isinstance(raw_tps, str) and raw_tps.strip():
                try:
                    parsed = json.loads(raw_tps)
                except json.JSONDecodeError as exc:
                    raise StudyError(f"tp_oids_json is not json: {raw_tps!r}") from exc
                if isinstance(parsed, list):
                    for item in parsed:
                        try:
                            oid = int(item)
                        except (TypeError, ValueError) as exc:
                            raise StudyError(f"tp oid is not an integer: {item!r}") from exc
                        if oid > 0 and oid not in slot["tp_oids"]:
                            slot["tp_oids"].append(oid)
            raw_stop = evidence(event.get("stop_loss_oid"))
            if isinstance(raw_stop, str) and raw_stop.strip():
                try:
                    stop_oid = int(raw_stop)
                except (TypeError, ValueError) as exc:
                    raise StudyError(f"stop oid is not an integer: {raw_stop!r}") from exc
                if stop_oid > 0:
                    slot["stop_oid"] = stop_oid
        for slot in grouped.values():
            if slot["coin"] and slot["side"] and slot["open_ms"] is not None:
                positions.append(slot)
    positions.sort(key=lambda row: (row["strategy"], row["coin"], row["open_ms"], row["position_id"]))
    return positions, hashes


def load_capture(path):
    bundle_path = os.path.join(path, "bundle.json")
    digest = sha256_file(bundle_path)
    with open(bundle_path) as fh:
        bundle = json.load(fh)
    if not isinstance(bundle, dict) or bundle.get("schema") != CAPTURE_SCHEMA:
        raise StudyError("capture bundle schema must be go-trader.resting-tp-capture")
    if bundle.get("schema_version") != 1:
        raise StudyError("capture bundle schema_version must be 1")
    log_name = "request" + "s.jsonl"
    log_path = os.path.join(path, log_name)
    rows = []
    with open(log_path) as fh:
        for line in fh:
            if line.strip():
                rows.append(json.loads(line))
    loaded = []
    for row in rows:
        rel = row["response_file"]
        full = os.path.join(path, rel)
        raw = open(full, "rb").read()
        if sha256_file(full) != row["response_sha256"]:
            raise StudyError(f"capture response hash mismatch: {rel}")
        payload = json.loads(raw.decode("utf-8")) if raw else None
        loaded.append({"meta": row, "payload": payload, "raw": raw})
    return bundle, digest, loaded


def load_market(manifest_path, manifest_sha, window_name):
    actual = sha256_file(manifest_path)
    if actual != manifest_sha:
        raise StudyError(
            f"manifest sha256 {actual} != bound {manifest_sha}")
    manifest = om.load_manifest(manifest_path, verify=True)
    if window_name not in manifest["windows"]:
        raise StudyError(f"unknown window {window_name!r}")
    by_coin = {}
    for dataset in manifest["datasets"]:
        frame = om.load_candles(dataset)
        by_coin[dataset["coin"]] = {
            "dataset": dataset,
            "frame": frame,
            "size_decimals": int(dataset["size_decimals"]),
        }
    return manifest, by_coin


def fill_identity(row):
    if not isinstance(row, dict):
        return None
    tid = row.get("tid")
    if tid is not None:
        return ("tid", str(tid))
    return ("row", row.get("time"), row.get("oid"), row.get("sz"), row.get("px"))


def dedupe_fills(rows):
    seen = set()
    out = []
    for row in rows:
        key = fill_identity(row)
        if key is None or key in seen:
            continue
        seen.add(key)
        out.append(row)
    return out


def index_responses(loaded):
    statuses = {}
    historical = []
    fills = []
    trades = {}
    candles = {}
    for item in loaded:
        meta = item["meta"]
        kind = meta.get("type")
        payload = item["payload"]
        if kind == "orderStatus":
            body = meta.get("body") or {}
            oid = body.get("oid")
            if oid is not None:
                statuses[int(oid)] = unwrap_status(payload)
        elif kind == "historicalOrders" and isinstance(payload, list):
            historical.extend(payload)
        elif kind == "userFillsByTime" and isinstance(payload, list):
            fills.extend(payload)
        elif kind == "recentTrades":
            coin = meta.get("coin")
            trades.setdefault(coin, [])
            if isinstance(payload, list):
                trades[coin].extend(payload)
        elif kind == "candleSnapshot":
            coin = meta.get("coin")
            candles.setdefault(coin, [])
            if isinstance(payload, list):
                candles[coin].extend(payload)
    return statuses, historical, dedupe_fills(fills), trades, candles


def fills_for(oid, fills):
    rows = []
    for row in fills:
        if not isinstance(row, dict):
            continue
        try:
            if int(row.get("oid")) != int(oid):
                continue
        except (TypeError, ValueError):
            continue
        rows.append(row)
    rows.sort(key=lambda row: (int(row.get("time") or 0), str(row.get("tid") or "")))
    return rows


def fill_summary(rows):
    qty = Decimal(0)
    notion = Decimal(0)
    fee = Decimal(0)
    times = []
    for row in rows:
        sz = dec(row.get("sz"), "fill sz")
        px = dec(row.get("px"), "fill px")
        qty += sz
        notion += sz * px
        if row.get("fee") is not None:
            fee += dec(row.get("fee"), "fill fee")
        if row.get("time") is not None:
            times.append(int(row["time"]))
    vwap = (notion / qty) if qty > 0 else None
    rate = (fee / notion) if notion > 0 else None
    return {
        "count": len(rows),
        "qty": qty,
        "vwap": vwap,
        "fee": fee,
        "fee_rate": rate,
        "first_ms": min(times) if times else None,
        "last_ms": max(times) if times else None,
    }


def ranges_overlap(a_open, a_close, b_open, b_close):
    a_end = a_close if a_close is not None else 2**62
    b_end = b_close if b_close is not None else 2**62
    return a_open < b_end and b_open < a_end


def attribute(oid, coin, venue_side, placement, positions, export_claimed):
    owners = []
    for pos in positions:
        claimed = export_claimed and oid in pos["tp_oids"]
        discovered = (not export_claimed and coin == pos["coin"] and placement is not None
                      and ranges_overlap(pos["open_ms"], pos["close_ms"], placement, placement + 1)
                      and venue_side == take_profit_venue_side(pos["side"]))
        if claimed or discovered:
            owners.append(pos)
    strategies = sorted({pos["strategy"] for pos in owners})
    if len(strategies) != 1:
        return None, "shared_coin_ambiguous" if len(strategies) > 1 else "unattributed"
    if len(owners) != 1:
        return None, "shared_coin_ambiguous"
    return owners[0], None


def order_record(oid, status_doc, fill_rows):
    order = status_doc["order"] if status_doc else None
    status = str((status_doc or {}).get("status") or "")
    status_time = (status_doc or {}).get("status_time")
    placement = None
    limit_px = None
    orig = None
    coin = None
    venue_side = None
    if isinstance(order, dict):
        if order.get("timestamp") is not None:
            placement = int(order["timestamp"])
        if order.get("limitPx") is not None:
            limit_px = dec(order["limitPx"], "limitPx")
        raw_sz = order.get("origSz", order.get("sz"))
        if raw_sz is not None:
            orig = dec(raw_sz, "origSz")
        if order.get("coin"):
            coin = str(order["coin"])
        venue_side = venue_order_side(order.get("side"))
    summary = fill_summary(fill_rows)
    terminal_ms = None
    if is_terminal_status(status) and isinstance(status_time, int):
        terminal_ms = status_time
    if summary["qty"] > 0 and orig is not None and summary["qty"] >= orig and summary["last_ms"] is not None:
        fill_end = summary["last_ms"]
        terminal_ms = fill_end if terminal_ms is None else min(terminal_ms, fill_end)
    missing = []
    if placement is None:
        missing.append("placement")
    if terminal_ms is None:
        missing.append("terminal")
    if orig is None:
        missing.append("requested_quantity")
    if limit_px is None:
        missing.append("limit_price")
    return {
        "oid": int(oid),
        "coin": coin,
        "venue_side": venue_side,
        "placement_ms": placement,
        "terminal_ms": terminal_ms,
        "status": status.lower(),
        "limit_px": limit_px,
        "orig": orig,
        "fills": summary,
        "missing": missing,
        "trigger_px": dec(order["triggerPx"], "triggerPx") if isinstance(order, dict) and order.get("triggerPx") is not None else None,
    }


def build_orders(positions, statuses, historical, fills):
    export_oids = []
    seen = set()
    for pos in positions:
        for oid in pos["tp_oids"]:
            if oid not in seen:
                seen.add(oid)
                export_oids.append(oid)
        if pos["stop_oid"] and pos["stop_oid"] not in seen:
            seen.add(pos["stop_oid"])
    discovered = []
    for doc in historical:
        parsed = unwrap_status(doc)
        if not parsed or not isinstance(parsed.get("order"), dict):
            continue
        order = parsed["order"]
        if not is_reduce_only_gtc(order):
            continue
        try:
            oid = int(order.get("oid"))
        except (TypeError, ValueError):
            continue
        if oid in seen:
            continue
        seen.add(oid)
        discovered.append(oid)
        statuses.setdefault(oid, parsed)
    records = []
    for oid in export_oids + discovered:
        if oid in {pos["stop_oid"] for pos in positions}:
            continue
        rec = order_record(oid, statuses.get(oid), fills_for(oid, fills))
        rec["from_export"] = oid in set(export_oids)
        records.append(rec)
    return records, statuses


def fill_terminated(rec) -> bool:
    if rec.get("status") == "filled":
        return True
    orig = rec.get("orig")
    fills = rec.get("fills") or {}
    qty = fills.get("qty") or Decimal(0)
    return orig is not None and qty >= orig and isinstance(fills.get("first_ms"), int)


def score_end_ms(rec):
    if fill_terminated(rec):
        first = (rec.get("fills") or {}).get("first_ms")
        if isinstance(first, int):
            return first
    return rec.get("terminal_ms")


def lifetime_inside(start, end, windows) -> bool:
    if not windows:
        return False
    for lo, hi in windows:
        if not isinstance(lo, int) or not isinstance(hi, int):
            return False
        if start < lo or end > hi:
            return False
    return True


def bars_for(frame, step, placement, end, other_times, include_end_bar=False):
    times = [int(v) for v in frame["timestamp"].tolist()]
    rows = {int(frame["timestamp"].iloc[i]): frame.iloc[i] for i in range(len(frame))}
    if not times:
        return [], [], True
    origin = times[0]
    last_end = times[-1] + step
    missing = placement < origin or end > last_end
    interior = []
    boundary = []
    cursor = origin
    while cursor < end:
        intersects = cursor < end and cursor + step > placement
        if intersects:
            if cursor not in rows:
                missing = True
            else:
                row = rows[cursor]
                item = {
                    "open_ms": cursor,
                    "high": dec(row["high"], "high"),
                    "low": dec(row["low"], "low"),
                    "close": dec(row["close"], "close"),
                }
                overlaps_placement = cursor < placement < cursor + step
                overlaps_end = cursor < end < cursor + step
                overlaps_other = any(cursor < stamp < cursor + step for stamp in other_times)
                score_end_bar = (
                    include_end_bar and overlaps_end
                    and not overlaps_placement and not overlaps_other
                )
                contained = cursor >= placement and cursor + step <= end
                if score_end_bar or (
                    contained and not overlaps_placement and not overlaps_end and not overlaps_other
                ):
                    interior.append(item)
                elif overlaps_placement or overlaps_end or overlaps_other:
                    boundary.append(item)
        cursor += step
    return interior, boundary, missing


def extreme(bar, side):
    return bar["high"] if side == "long" else bar["low"]


def reached(bar, limit, side):
    if side == "long":
        return bar["high"] >= limit
    return bar["low"] <= limit


def close_crossed(bar, limit, side):
    if side == "long":
        return bar["close"] >= limit
    return bar["close"] <= limit


def classify_order(rec, position, frame, step, uncovered, sz_decimals, other_times, windows):
    if position is None:
        return "shared_coin_ambiguous" if rec.get("attr_reason") == "shared_coin_ambiguous" else rec.get("attr_reason")
    if rec["missing"]:
        return "lifetime_unknown"
    if rec["venue_side"] != take_profit_venue_side(position["side"]):
        return "side_mismatch"
    start = rec["placement_ms"]
    end = rec["terminal_ms"]
    if uncovered and ranges_overlap(start, end, uncovered[0], uncovered[1]):
        return "acquisition_incomplete"
    if not lifetime_inside(start, end, windows):
        return "acquisition_incomplete"
    filled = rec["fills"]["qty"]
    orig = rec["orig"]
    if rec["status"] == "filled" and filled < orig:
        return "acquisition_incomplete"
    if frame is None:
        return "candle_coverage_missing"
    if not on_tick_grid(rec["limit_px"], sz_decimals):
        return "limit_off_grid"
    interior, boundary, missing = bars_for(
        frame, step, start, score_end_ms(rec), other_times,
        include_end_bar=fill_terminated(rec))
    rec["interior"] = interior
    rec["boundary"] = boundary
    if missing:
        return "candle_coverage_missing"
    if not interior:
        return "boundary_unknown"
    side = position["side"]
    limit = rec["limit_px"]
    max_ticks = 0
    max_bps = Decimal(0)
    close_hit = False
    touch = False
    for bar in interior:
        ext = extreme(bar, side)
        ticks = ticks_through(ext, limit, side, sz_decimals)
        max_ticks = max(max_ticks, ticks)
        max_bps = max(max_bps, bps_through(ext, limit, side))
        touch = touch or reached(bar, limit, side)
        close_hit = close_hit or close_crossed(bar, limit, side)
    rec["max_ticks"] = max_ticks
    rec["max_bps"] = max_bps
    rec["close_hit"] = close_hit
    rec["touch"] = touch
    filled = rec["fills"]["qty"]
    orig = rec["orig"]
    if filled <= 0:
        if max_ticks >= 1:
            return "no_fill_trade_through"
        if touch:
            return "no_fill_touch"
        return "not_reached"
    if filled < orig:
        return "partial_fill"
    return "full_fill"


def rule_predicts(rec, name, k=None):
    if name == "close":
        return bool(rec.get("close_hit"))
    if name == "touch":
        return bool(rec.get("touch"))
    return int(rec.get("max_ticks") or 0) >= int(k)


def blank_counts():
    return {
        "agreements": 0,
        "false_fills": 0,
        "missed_fills": 0,
        "quantity_errors": 0,
        "tick_buckets": {name: 0 for name in BUCKETS},
        "bps_buckets": {name: 0 for name in BUCKETS},
    }


def mark_unmeasured(block):
    out = {
        "agreements": block["agreements"],
        "false_fills": block["false_fills"],
        "missed_fills": block["missed_fills"],
        "quantity_errors": block["quantity_errors"],
    }
    for key in ("tick_buckets", "bps_buckets"):
        rendered = {}
        for name in BUCKETS:
            rendered[name] = block[key][name] if block[key][name] else "unmeasured"
        out[key] = rendered
    return out


def score_rules(population, max_k):
    names = ["close", "touch"] + [f"trade_through_{k}" for k in range(1, max_k + 1)]
    sides = ("long", "short")
    tables = {name: {side: blank_counts() for side in sides} for name in names}
    for rec in population:
        side = rec["side"]
        tick_b = bucket_name(int(rec.get("max_ticks") or 0))
        bps_b = bucket_name(int(rec.get("max_bps") or 0))
        venue_full = rec["class_name"] == "full_fill"
        venue_partial = rec["class_name"] == "partial_fill"
        venue_none = rec["class_name"] in ("no_fill_touch", "no_fill_trade_through", "not_reached")
        for name in names:
            k = None
            kind = name
            if name.startswith("trade_through_"):
                kind = "trade_through"
                k = int(name.split("_")[-1])
            predicts = rule_predicts(rec, kind, k)
            block = tables[name][side]
            block["tick_buckets"][tick_b] += 1
            block["bps_buckets"][bps_b] += 1
            if predicts and venue_partial:
                block["quantity_errors"] += 1
            elif predicts and venue_none:
                block["false_fills"] += 1
            elif (not predicts) and (venue_full or venue_partial):
                block["missed_fills"] += 1
            elif predicts and venue_full:
                block["agreements"] += 1
            elif (not predicts) and venue_none:
                block["agreements"] += 1
    return {name: {side: mark_unmeasured(tables[name][side]) for side in sides} for name in names}


def same_bar_race(rec, trigger):
    if trigger is None:
        return None
    side = rec["side"]
    limit = rec["limit_px"]
    for bar in rec.get("interior") or []:
        if side == "long":
            if bar["high"] >= limit and bar["low"] <= trigger:
                return True
        elif bar["low"] <= limit and bar["high"] >= trigger:
            return True
    return False


def minute_opens(since_ms, end_ms):
    if not isinstance(since_ms, int) or not isinstance(end_ms, int) or end_ms <= since_ms:
        return []
    return list(range(int(since_ms), int(end_ms), 60_000))


def candle_basis(manifest_coins, candles, fills, since_ms, end_ms, step_ms, trades=None):
    del trades
    failures = []
    expected = minute_opens(since_ms, end_ms)
    if not expected:
        failures.append({"reason": "window_missing"})
    fills_by_coin = {}
    for row in fills or []:
        if isinstance(row, dict) and row.get("coin"):
            fills_by_coin.setdefault(str(row["coin"]), []).append(row)
    for coin in sorted(manifest_coins):
        frame = manifest_coins[coin]["frame"]
        by_open = {}
        for candle in candles.get(coin) or []:
            if isinstance(candle, dict) and candle.get("t") is not None:
                by_open[int(candle["t"])] = candle
        missing = [open_ms for open_ms in expected if open_ms not in by_open]
        if missing:
            failures.append({
                "coin": coin,
                "missing_bars": len(missing),
                "reason": "snapshot_incomplete",
            })
            continue
        times = [int(v) for v in frame["timestamp"].tolist()]
        frame_rows = {int(frame["timestamp"].iloc[i]): frame.iloc[i] for i in range(len(frame))}
        for open_ms in times:
            if open_ms < since_ms or open_ms + step_ms > end_ms:
                continue
            highs = []
            lows = []
            covered = True
            cursor = open_ms
            while cursor < open_ms + step_ms:
                candle = by_open.get(cursor)
                if candle is None:
                    covered = False
                    break
                highs.append(dec(candle.get("h"), "candle high"))
                lows.append(dec(candle.get("l"), "candle low"))
                cursor += 60_000
            if not covered or not highs:
                failures.append({"coin": coin, "open_ms": open_ms, "reason": "snapshot_incomplete"})
                continue
            bar = frame_rows.get(open_ms)
            if bar is None:
                failures.append({"coin": coin, "open_ms": open_ms, "reason": "manifest_bar_missing"})
                continue
            if max(highs) != dec(bar["high"], "manifest high") or min(lows) != dec(bar["low"], "manifest low"):
                failures.append({"coin": coin, "open_ms": open_ms, "reason": "aggregate_mismatch"})
        for row in fills_by_coin.get(coin) or []:
            if row.get("time") is None or row.get("px") is None:
                continue
            stamp = int(row["time"])
            if stamp < since_ms or stamp >= end_ms:
                continue
            minute = (stamp // 60_000) * 60_000
            candle = by_open.get(minute)
            if candle is None:
                failures.append({"coin": coin, "open_ms": minute, "reason": "fill_without_candle"})
                continue
            px = dec(row.get("px"), "fill px")
            if px < dec(candle.get("l"), "candle low") or px > dec(candle.get("h"), "candle high"):
                failures.append({"coin": coin, "open_ms": minute, "reason": "fill_outside_candle"})
    if not manifest_coins and not failures:
        failures.append({"reason": "no_manifest_candles"})
    status = "confirmed" if not failures else "unconfirmed"
    return status, sha256_text(canonical(failures))


def cancel_source(statuses, tp_oids):
    cancelled = []
    for oid in sorted(tp_oids):
        doc = statuses.get(oid)
        if not doc:
            continue
        status = str(doc.get("status") or "")
        if is_cancel_status(status):
            cancelled.append(doc.get("status_time"))
    if not cancelled:
        return "unconfirmed", sha256_text(canonical({"cancelled": 0}))
    if any(isinstance(stamp, int) for stamp in cancelled):
        return "confirmed", sha256_text(canonical({"cancelled_with_time": sum(isinstance(s, int) for s in cancelled)}))
    return "unconfirmed", sha256_text(canonical({"cancelled_without_time": len(cancelled)}))


def fee_text(rate):
    if rate is None:
        return None
    return format(rate, "f")


def public_order(ordinal, rec, class_name):
    return {
        "class_name": class_name if class_name in RATE_CLASSES or class_name == "lifetime_unknown" else None,
        "coin": rec.get("coin"),
        "exclusion": None if class_name in RATE_CLASSES or class_name == "lifetime_unknown" else class_name,
        "fee_rate": fee_text(rec["fills"]["fee_rate"]),
        "fill_count": rec["fills"]["count"],
        "filled_qty": format(rec["fills"]["qty"], "f"),
        "limit_px": format(rec["limit_px"], "f") if rec.get("limit_px") is not None else None,
        "order_ordinal": ordinal,
        "outcome": class_name,
        "side": rec.get("side"),
    }


def render_markdown(report):
    lines = [
        "# Resting take-profit fill study",
        "",
        "Read-only measurement for issue 1727. The capture tool and this study stay outside the scheduler. The study reads a frozen ledger export, a capture bundle, and a hash-checked candle manifest. It submits no order and writes no config or state.",
        "",
        "Placement time, terminal time, requested size and limit price come only from venue order records. Export timestamps only attribute an order to a position. A missing venue field is `lifetime_unknown`. Tier prices, manual additions and replacement orders are not rebuilt from current configuration.",
        "",
        "A bar that overlaps placement, cancellation or replacement is unknown. The bar that holds a venue fill is scored with that fill. A rule that predicts a full quantity against a venue partial fill is a quantity error. An unconfirmed candle basis or cancel-time source is a blocker. Raising k does not repair unknown placement timing.",
        "",
        "The frozen manifest interval is 5m because the manifest verifier has no 1m interval. Live capture still asks for 1m candles. Venue field shapes were not confirmed on the venue in this run.",
        "",
        "## Sample",
        "",
        "```json",
        dump(report["sample"]).rstrip(),
        "```",
        "",
        "## Blockers",
        "",
        "```json",
        dump(report["blockers"]).rstrip(),
        "```",
        "",
        "## Fee rates on take-profit fills",
        "",
        "These rates are evidence for issue 1726. This study changes no fee.",
        "",
        "```json",
        dump(report["fee_rates"]).rstrip(),
        "```",
        "",
    ]
    return "\n".join(lines)


def study(exports, capture_dir, manifest_path, manifest_sha, window_name, production_capture):
    positions, export_hashes = load_exports(exports)
    bundle, capture_sha, loaded = load_capture(capture_dir)
    manifest, by_coin = load_market(manifest_path, manifest_sha, window_name)
    statuses, historical, fills, trades, candles = index_responses(loaded)
    records, statuses = build_orders(positions, statuses, historical, fills)
    fills_meta = bundle.get("completeness", {}).get("userFillsByTime") or {}
    uncovered = None
    if not fills_meta.get("complete"):
        start = fills_meta.get("uncovered_from_ms")
        end = fills_meta.get("uncovered_to_ms")
        if isinstance(start, int) and isinstance(end, int):
            uncovered = (start, end)
        else:
            uncovered = (0, 2**62)
    step = int(manifest["interval_ms"])
    capture_inputs = bundle.get("inputs") or {}
    window_spec = manifest["windows"][window_name]
    windows = [
        (capture_inputs.get("since_ms"), capture_inputs.get("end_ms")),
        (parse_time_ms(window_spec["start"]), parse_time_ms(window_spec["end"])),
    ]
    export_oid_set = {oid for pos in positions for oid in pos["tp_oids"]}
    times_by_coin = {}
    for rec in records:
        coin = rec.get("coin") or ""
        for stamp in (rec.get("placement_ms"), rec.get("terminal_ms")):
            if isinstance(stamp, int):
                times_by_coin.setdefault(coin, []).append((rec["oid"], stamp))
    prepared = []
    for rec in records:
        owner, reason = attribute(
            rec["oid"], rec.get("coin"), rec.get("venue_side"), rec.get("placement_ms"),
            positions, rec["oid"] in export_oid_set)
        rec["attr_reason"] = reason
        rec["side"] = owner["side"] if owner else None
        rec["strategy"] = owner["strategy"] if owner else None
        market = by_coin.get(rec.get("coin") or "")
        frame = market["frame"] if market else None
        sz_decimals = market["size_decimals"] if market else None
        if reason:
            class_name = reason
        else:
            other_times = [
                stamp for oid, stamp in times_by_coin.get(rec.get("coin") or "", [])
                if oid != rec["oid"]
            ]
            class_name = classify_order(
                rec, owner, frame, step, uncovered, sz_decimals, other_times, windows)
        rec["class_name"] = class_name
        rec["position"] = owner
        prepared.append(rec)
    prepared.sort(key=lambda rec: (
        rec.get("coin") or "",
        rec.get("placement_ms") if rec.get("placement_ms") is not None else -1,
        rec["oid"],
    ))
    for ordinal, rec in enumerate(prepared, start=1):
        rec["ordinal"] = ordinal
    population = [rec for rec in prepared if rec["class_name"] in RATE_CLASSES]
    max_k = max([1] + [int(rec.get("max_ticks") or 0) for rec in population])
    rules = score_rules(population, max_k)
    races = {"same_bar_stop_and_tier": 0, "unknown": 0}
    for rec in population:
        owner = rec["position"]
        if not owner or not owner.get("stop_oid"):
            continue
        stop = statuses.get(owner["stop_oid"])
        trigger = None
        if stop and isinstance(stop.get("order"), dict) and stop["order"].get("triggerPx") is not None:
            trigger = dec(stop["order"]["triggerPx"], "stop trigger")
        elif stop and stop.get("status_time") is not None and isinstance(stop.get("order"), dict):
            trigger = None
        hit = same_bar_race(rec, trigger)
        if trigger is None:
            races["unknown"] += 1
        elif hit:
            races["same_bar_stop_and_tier"] += 1
    basis, basis_hash = candle_basis(
        by_coin, candles, fills, capture_inputs.get("since_ms"), capture_inputs.get("end_ms"),
        step, trades)
    tp_oids = [rec["oid"] for rec in prepared]
    cancel, cancel_hash = cancel_source(statuses, tp_oids)
    blockers = []
    if basis != "confirmed":
        blockers.append({
            "evidence_sha256": basis_hash,
            "fact": "candle_price_basis",
            "status": "unconfirmed",
        })
    if cancel != "confirmed":
        blockers.append({
            "evidence_sha256": cancel_hash,
            "fact": "cancel_time_source",
            "status": "unconfirmed",
        })
    if production_capture != "done":
        blockers.append({
            "fact": "production_capture",
            "reason": "production host, live account address, and venue network were not available",
            "status": "pending",
        })
    counts = {}
    side_counts = {}
    exclusions = []
    for rec in prepared:
        name = rec["class_name"]
        if name in RATE_CLASSES or name == "lifetime_unknown":
            counts[name] = counts.get(name, 0) + 1
            side = rec.get("side") or "unknown"
            side_counts.setdefault(side, {})
            side_counts[side][name] = side_counts[side].get(name, 0) + 1
        else:
            exclusions.append({"order_ordinal": rec["ordinal"], "reason": name})
    orders = [public_order(rec["ordinal"], rec, rec["class_name"]) for rec in prepared]
    fee_rates = [
        {"fee_rate": row["fee_rate"], "order_ordinal": row["order_ordinal"]}
        for row in orders if row["fee_rate"] is not None
    ]
    report = {
        "blockers": blockers,
        "cancel_time_source": cancel,
        "candle_price_basis": basis,
        "fee_rates": fee_rates,
        "inputs": {
            "capture_bundle_sha256": capture_sha,
            "export_sha256": export_hashes,
            "manifest_sha256": manifest_sha,
            "window": window_name,
        },
        "orders": orders,
        "production_capture": "pending" if production_capture != "done" else "done",
        "races": races,
        "rules": rules,
        "sample": {
            "exclusions": exclusions,
            "orders_per_class": counts,
            "orders_per_class_by_side": side_counts,
            "strategies": sorted({pos["strategy"] for pos in positions}),
            "windows": [
                {
                    "close_ms": pos["close_ms"],
                    "coin": pos["coin"],
                    "open_ms": pos["open_ms"],
                    "side": pos["side"],
                    "strategy": pos["strategy"],
                }
                for pos in positions
            ],
        },
        "schema": SCHEMA,
        "schema_version": SCHEMA_VERSION,
    }
    return report


def main(argv):
    parser = argparse.ArgumentParser(description="Offline resting take-profit fill study.")
    parser.add_argument("--export", action="append", required=True)
    parser.add_argument("--capture", required=True)
    parser.add_argument("--manifest", required=True)
    parser.add_argument("--manifest-sha256", required=True)
    parser.add_argument("--window", required=True)
    parser.add_argument("--out", required=True)
    parser.add_argument("--summary", default="")
    parser.add_argument("--summary-md", default="")
    parser.add_argument("--production-capture", choices=("pending", "done"), default="pending")
    args = parser.parse_args(argv)
    report = study(
        args.export, args.capture, args.manifest, args.manifest_sha256,
        args.window, args.production_capture)
    text = dump(report)
    with open(args.out, "w") as fh:
        fh.write(text)
    if args.summary:
        summary = {
            "blockers": report["blockers"],
            "fee_rates": report["fee_rates"],
            "inputs": report["inputs"],
            "orders": [
                {
                    "fee_rate": row["fee_rate"],
                    "order_ordinal": row["order_ordinal"],
                    "outcome": row["outcome"],
                    "side": row["side"],
                }
                for row in report["orders"]
            ],
            "production_capture": report["production_capture"],
            "races": report["races"],
            "rules": report["rules"],
            "sample": report["sample"],
            "schema": report["schema"],
            "schema_version": report["schema_version"],
        }
        with open(args.summary, "w") as fh:
            fh.write(dump(summary))
    if args.summary_md:
        with open(args.summary_md, "w") as fh:
            fh.write(render_markdown(report))


if __name__ == "__main__":
    try:
        main(sys.argv[1:])
    except (StudyError, om.ManifestError) as exc:
        print(str(exc), file=sys.stderr)
        sys.exit(1)
