#!/usr/bin/env python3

import argparse
import hashlib
import json
import math
import os
import re
import statistics
import sys
from datetime import datetime, timedelta, timezone

_HERE = os.path.dirname(os.path.abspath(__file__))
if _HERE not in sys.path:
    sys.path.insert(0, _HERE)

import ledger_compare as lc

REPORT_SCHEMA = "go-trader.fee-evidence"
REPORT_SCHEMA_VERSION = 1

OUTLIER_RELATIVE = 0.05
LOG_PRECISION_MAX_BOUND = 0.00005
LOG_PRICE_TOLERANCE = 0.005
LOG_PAIR_WINDOW_SECONDS = 300
FLOAT_BOUND_MARGIN = 1e-9

GROUPS = ("open", "add", "stop_loss", "take_profit_tier", "signal_close", "risk_close",
          "manual", "external", "hedge", "unclassified")
ROLES = ("taker", "maker", "unknown")

CLOSE_REASON_GROUPS = {
    "signal": "signal_close",
    "force_close": "manual",
    "manual_close": "manual",
    "hl_sync_external": "external",
    "hl_sync_external_partial": "external",
    "circuit_breaker": "risk_close",
    "kill_switch": "risk_close",
    "regime_direction_flip": "risk_close",
    "hedge_reduce": "hedge",
    "hedge_close": "hedge",
    "hedge_open_failed_unwind": "hedge",
    "hedge_already_flat": "hedge",
}
CLOSE_REASON_SUFFIXES = ("_corrupt", "_dup_oid")
TP_REASON = re.compile(r"^hl_sync_tp[0-9]+_fill$")
STOP_REASON = re.compile(r"(^|_)(stop_loss|sl)(_|$)")

DETAILS_PREFIX_GROUPS = (
    ("Stop loss close", "stop_loss"),
    ("Paper trailing SL close", "stop_loss"),
    ("Trailing SL close", "stop_loss"),
    ("Liquidation-clamp SL close", "stop_loss"),
    ("Post-TP SL close", "stop_loss"),
    ("Paper SL close", "stop_loss"),
    ("Close long", "signal_close"),
    ("Close short", "signal_close"),
    ("Partial-close long", "signal_close"),
    ("Partial-close short", "signal_close"),
    ("force close", "manual"),
    ("manual close", "manual"),
    ("External close @ mark", "external"),
    ("External partial close @ mark", "external"),
    ("Circuit breaker", "risk_close"),
    ("Regime/direction flip auto-close", "risk_close"),
    ("kill_switch on-chain close", "risk_close"),
    ("On-chain close", "risk_close"),
    ("hedge(", "hedge"),
    ("hedge open failed", "hedge"),
    ("hedge close (already flat", "hedge"),
)
TP_DETAILS = re.compile(r"^TP[0-9]+ fill close")

FILL_LOG_LINE = re.compile(
    r"^\[(?P<ts>\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2})\] \[(?P<strategy>[^\]]+)\] \[[A-Z]+\] "
    r"Live (?P<scale>scale-in )?fill at \$(?P<fill>[0-9]+(?:\.[0-9]+)?) "
    r"qty=(?P<qty>[0-9]+(?:\.[0-9]+)?) \(mid was \$(?P<mid>[0-9]+(?:\.[0-9]+)?)\)\s*$")


class EvidenceInputError(Exception):
    pass


def _sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def _iso(dt: datetime) -> str:
    return dt.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def _parse_utc(text: str, label: str) -> datetime:
    try:
        return lc._utc(text, label).to_pydatetime()
    except lc.LedgerInputError as exc:
        raise EvidenceInputError(str(exc))


def _value(ev: dict, field: str):
    item = ev.get(field) or {}
    if item.get("status") != "available":
        return None
    return item.get("value")


def _stats(samples: list) -> dict:
    if not samples:
        return {"count": 0}
    rates = [s["rate"] for s in samples]
    times = sorted(s["timestamp"] for s in samples)
    out = {
        "count": len(samples),
        "first_timestamp": _iso(times[0]),
        "last_timestamp": _iso(times[-1]),
        "min": min(rates),
        "median": statistics.median(rates),
        "max": max(rates),
    }
    notional = math.fsum(s["notional"] for s in samples)
    if notional > 0:
        out["notional_weighted"] = math.fsum(s["fee"] for s in samples) / notional
    return out


def _outliers(samples: list, median: float) -> list:
    if not samples or median == 0:
        return []
    return [s for s in samples if abs(s["rate"] - median) > OUTLIER_RELATIVE * abs(median)]


def _close_reason_group(reason: str):
    base = reason
    for suffix in CLOSE_REASON_SUFFIXES:
        if base.endswith(suffix):
            base = base[: -len(suffix)]
    if TP_REASON.match(base):
        return "take_profit_tier"
    if base in CLOSE_REASON_GROUPS:
        return CLOSE_REASON_GROUPS[base]
    if STOP_REASON.search(base):
        return "stop_loss"
    return None


def _details_group(details: str):
    if TP_DETAILS.match(details):
        return "take_profit_tier"
    for prefix, group in DETAILS_PREFIX_GROUPS:
        if details.startswith(prefix):
            return group
    return None


def classify(ev: dict) -> tuple:
    kind = _value(ev, "event_kind")
    if _value(ev, "trade_type") == "hedge":
        return "hedge", "trade_type"
    if _value(ev, "manual") is True:
        return "manual", "manual"
    if kind == "scale_in":
        return "add", "event_kind"
    if kind == "non_close":
        return "open", "event_kind"
    if kind != "close":
        return "unclassified", "event_kind"
    reason = _value(ev, "close_reason")
    if isinstance(reason, str) and reason:
        group = _close_reason_group(reason)
        if group:
            return group, "close_reason"
    details = _value(ev, "details")
    if isinstance(details, str):
        group = _details_group(details)
        if group:
            return group, "details_prefix"
    return "unclassified", "none"


def _decimal_oid(text):
    if isinstance(text, str) and text.isdigit() and text != "0":
        return str(int(text))
    return None


def load_exports(paths: list) -> list:
    out = []
    for path in paths:
        try:
            data, doc = lc.load_export(path)
        except lc.LedgerInputError as exc:
            raise EvidenceInputError(f"export {path}: {exc}")
        except OSError as exc:
            raise EvidenceInputError(f"export {path}: {exc}")
        out.append({"path": path, "sha256": _sha256_bytes(data), "doc": doc})
    return out


ROW_IDENTITY_FIELDS = ("timestamp", "symbol", "side", "quantity", "price", "value", "exchange_fee", "fee_source",
                       "exchange_order_id", "event_kind", "is_close", "manual", "trade_type", "details")


def _row_fingerprint(ev: dict) -> tuple:
    return tuple(ev.get(f) if f == "timestamp" else json.dumps(ev.get(f), sort_keys=True)
                 for f in ROW_IDENTITY_FIELDS)


def select_rows(exports: list) -> tuple:
    candidates = []
    excluded = {}
    seen = {}

    def drop(reason):
        excluded[reason] = excluded.get(reason, 0) + 1

    for exp in exports:
        doc = exp["doc"]
        partition = doc["selection"]["partition"]
        strategy = doc["selection"]["process_strategy_id"]
        for ev in doc["events"]:
            identity = (ev["storage_strategy_id"], ev["source_role"], ev["source_table"], ev["source_row_id"])
            fingerprint = _row_fingerprint(ev)
            if identity in seen:
                prior_key, prior_print = seen[identity]
                if prior_print != fingerprint:
                    raise EvidenceInputError(
                        f"{ev['event_key']} of {ev['storage_strategy_id']} in {exp['path']} repeats {prior_key} "
                        f"from another export with different evidence; pass exports that agree, or one capture")
                drop("duplicate_row")
                continue
            seen[identity] = (ev["event_key"], fingerprint)
            if _value(ev, "event_kind") == "funding":
                drop("funding")
                continue
            if partition != "live":
                drop("partition_not_live")
                continue
            source = _value(ev, "fee_source")
            if source != "userfills":
                if source == "modeled":
                    drop("fee_source_modeled")
                elif source == "reconcile_adjustment":
                    drop("fee_source_reconcile_adjustment")
                elif source in (None, ""):
                    drop("fee_source_empty")
                else:
                    drop("fee_source_other")
                continue
            value = _value(ev, "value")
            fee = _value(ev, "exchange_fee")
            if not lc._is_number(value) or value == 0:
                drop("value_unavailable_or_zero")
                continue
            if not lc._is_number(fee):
                drop("exchange_fee_unavailable")
                continue
            group, basis = classify(ev)
            candidates.append({
                "event_key": ev["event_key"],
                "capture_manifest_sha256": doc["capture_manifest_sha256"],
                "strategy": strategy,
                "timestamp": _parse_utc(ev["timestamp"], f"{ev['event_key']}.timestamp"),
                "group": group,
                "group_basis": basis,
                "symbol": _value(ev, "symbol"),
                "side": _value(ev, "side"),
                "quantity": _value(ev, "quantity"),
                "price": _value(ev, "price"),
                "value": float(value),
                "fee": float(fee),
                "oid": _decimal_oid(_value(ev, "exchange_order_id")),
                "stop_trigger": _value(ev, "stop_loss_trigger_px"),
            })
    return candidates, dict(sorted(excluded.items()))


def booked_rates(rows: list) -> dict:
    by_group = {g: [] for g in GROUPS}
    rebates = []
    for row in rows:
        sample = {
            "event_key": row["event_key"],
            "timestamp": row["timestamp"],
            "rate": row["fee"] / abs(row["value"]),
            "fee": row["fee"],
            "notional": abs(row["value"]),
        }
        by_group[row["group"]].append(sample)
        if row["fee"] < 0:
            rebates.append({"event_key": row["event_key"], "group": row["group"], "rate": sample["rate"]})
    groups = {}
    outliers = []
    for g in GROUPS:
        samples = by_group[g]
        stats = _stats(samples)
        groups[g] = stats
        if samples:
            for s in _outliers(samples, stats["median"]):
                outliers.append({"event_key": s["event_key"], "group": g, "rate": s["rate"],
                                 "group_median": stats["median"]})
    trigger_basis = sorted(r["event_key"] for r in rows
                           if r["group"] == "stop_loss" and lc._is_number(r["stop_trigger"])
                           and lc._is_number(r["price"]) and r["price"] == r["stop_trigger"])
    return {
        "rate_definition": "exchange_fee / abs(value) per booked row",
        "groups": groups,
        "rebates": sorted(rebates, key=lambda x: x["event_key"]),
        "outliers": sorted(outliers, key=lambda x: (x["group"], x["event_key"])),
        "outlier_rule": f"rate differs from its group median by more than {OUTLIER_RELATIVE:g} relative",
        "unclassified_event_keys": sorted(r["event_key"] for r in rows if r["group"] == "unclassified"),
        "stop_rows_value_basis_trigger_price": trigger_basis,
    }


def _num(text, label):
    try:
        v = float(text)
    except (TypeError, ValueError):
        raise EvidenceInputError(f"{label} {text!r} is not a number")
    if not math.isfinite(v):
        raise EvidenceInputError(f"{label} is not finite")
    return v


def load_user_fills(paths: list) -> tuple:
    fills = {}
    inputs = []
    for path in paths:
        try:
            with open(path, "rb") as fh:
                data = fh.read()
        except OSError as exc:
            raise EvidenceInputError(f"user fills {path}: {exc}")
        try:
            arr = json.loads(data, parse_constant=lc._reject_constant)
        except ValueError as exc:
            raise EvidenceInputError(f"user fills {path} is not valid finite JSON: {exc}")
        if not isinstance(arr, list):
            raise EvidenceInputError(f"user fills {path} is not a JSON array (raw userFillsByTime response)")
        for i, f in enumerate(arr):
            label = f"{path}[{i}]"
            if not isinstance(f, dict):
                raise EvidenceInputError(f"{label} is not an object")
            for key in ("coin", "px", "sz", "fee", "oid", "time"):
                if key not in f:
                    raise EvidenceInputError(f"{label} lacks {key!r}")
            oid = str(f["oid"])
            if not oid.isdigit():
                raise EvidenceInputError(f"{label} oid {f['oid']!r} is not a decimal order id")
            if isinstance(f["time"], bool) or not isinstance(f["time"], int):
                raise EvidenceInputError(f"{label} time is not integer milliseconds")
            px = _num(f["px"], f"{label}.px")
            sz = _num(f["sz"], f"{label}.sz")
            fee = _num(f["fee"], f"{label}.fee")
            if px <= 0 or sz <= 0:
                raise EvidenceInputError(f"{label} has a non-positive px or sz")
            crossed = f.get("crossed")
            role = "taker" if crossed is True else "maker" if crossed is False else "unknown"
            tid = f.get("tid")
            key = ("tid", str(tid)) if tid is not None else ("fill", oid, f["time"], f["px"], f["sz"])
            rec = {
                "coin": f["coin"], "oid": str(int(oid)), "px": px, "sz": sz, "fee": fee,
                "fee_token": f.get("feeToken"), "role": role,
                "builder_fee": _num(f["builderFee"], f"{label}.builderFee") if "builderFee" in f else 0.0,
                "timestamp": datetime.fromtimestamp(f["time"] / 1000.0, tz=timezone.utc),
            }
            prior = fills.get(key)
            if prior is not None and (prior["oid"], prior["px"], prior["sz"], prior["fee"]) != \
                    (rec["oid"], rec["px"], rec["sz"], rec["fee"]):
                raise EvidenceInputError(f"{label} repeats fill {key!r} with different values")
            fills[key] = rec
        inputs.append({"file": os.path.basename(path), "sha256": _sha256_bytes(data), "fills": len(arr)})
    return list(fills.values()), inputs


def venue_join(rows: list, fills: list, provided: bool) -> dict:
    if not provided:
        return {"status": "unavailable", "reason": "no_user_fills"}
    usable = []
    excluded = {}
    for f in fills:
        if f["fee_token"] not in (None, "USDC"):
            excluded["fee_token_not_usdc"] = excluded.get("fee_token_not_usdc", 0) + 1
            continue
        usable.append(f)
    by_oid = {}
    for f in usable:
        by_oid.setdefault(f["oid"], []).append(f)
    rows_by_oid = {}
    for r in rows:
        if r["oid"]:
            rows_by_oid.setdefault(r["oid"], []).append(r)
    for oid, rs in rows_by_oid.items():
        for f in by_oid.get(oid, []):
            for r in rs:
                if r["symbol"] != f["coin"]:
                    raise EvidenceInputError(
                        f"fill coin {f['coin']!r} for order {oid} disagrees with {r['event_key']} symbol {r['symbol']!r}")

    def sample(f):
        notional = f["sz"] * f["px"]
        return {"timestamp": f["timestamp"], "rate": f["fee"] / notional, "fee": f["fee"], "notional": notional}

    all_by_role = {role: [sample(f) for f in usable if f["role"] == role] for role in ROLES}
    group_samples = {}
    conflicting = 0
    joined_fills = 0
    for oid, fs in by_oid.items():
        rs = rows_by_oid.get(oid)
        if not rs:
            continue
        groups = {r["group"] for r in rs}
        if len(groups) != 1:
            conflicting += len(fs)
            continue
        g = groups.pop()
        joined_fills += len(fs)
        for f in fs:
            group_samples.setdefault(g, {}).setdefault(f["role"], []).append(sample(f))
    by_group = {}
    for g in GROUPS:
        roles = group_samples.get(g, {})
        if not roles:
            continue
        by_group[g] = {
            "role_counts": {role: len(roles.get(role, [])) for role in ROLES},
            "by_role": {role: _stats(roles[role]) for role in ROLES if roles.get(role)},
        }
    rows_with_oid = [r for r in rows if r["oid"]]
    return {
        "status": "available",
        "rate_definition": "fee / (sz * px) per venue fill; role from the fill's crossed flag "
                           "(true = taker, false = maker, absent = unknown)",
        "fills_usable": len(usable),
        "fills_excluded": dict(sorted(excluded.items())),
        "fills_with_builder_fee": sum(1 for f in usable if f["builder_fee"] != 0),
        "all_fills_by_role": {role: _stats(all_by_role[role]) for role in ROLES},
        "joined_fills": joined_fills,
        "joined_fills_conflicting_group": conflicting,
        "fills_not_joined": len(usable) - joined_fills - conflicting,
        "rows_with_order_id": len(rows_with_oid),
        "rows_joined": sum(1 for r in rows_with_oid if r["oid"] in by_oid),
        "by_group": by_group,
    }


def _adverse(side: str, ref: float, px: float):
    if side == "sell":
        return (ref - px) / ref
    if side == "buy":
        return (px - ref) / ref
    return None


def stop_slippage(rows: list, fills: list, provided: bool) -> dict:
    by_oid = {}
    for f in fills:
        if f["fee_token"] in (None, "USDC"):
            by_oid.setdefault(f["oid"], []).append(f)
    venue = []
    differs = []
    at_trigger = 0
    no_trigger = 0
    no_venue_fill = 0
    for r in rows:
        if r["group"] != "stop_loss":
            continue
        trig = r["stop_trigger"]
        if not lc._is_number(trig) or trig <= 0:
            no_trigger += 1
            continue
        fs = by_oid.get(r["oid"]) if r["oid"] else None
        if fs:
            sz = math.fsum(f["sz"] for f in fs)
            vwap = math.fsum(f["sz"] * f["px"] for f in fs) / sz
            slip = _adverse(r["side"], trig, vwap)
            if slip is None:
                no_trigger += 1
                continue
            venue.append({"event_key": r["event_key"], "timestamp": r["timestamp"], "rate": slip,
                          "fee": 0.0, "notional": 0.0, "fills": len(fs)})
            continue
        if provided and r["oid"]:
            no_venue_fill += 1
        if lc._is_number(r["price"]) and r["price"] != trig:
            slip = _adverse(r["side"], trig, r["price"])
            differs.append({"event_key": r["event_key"], "export_price_slippage": slip,
                            "provenance": "unknown: may be a backfilled venue average or a modeled price"})
        else:
            at_trigger += 1
    stats = _stats(venue)
    stats.pop("notional_weighted", None)
    return {
        "definition": "side-signed (venue VWAP - stop_loss_trigger_px) / stop_loss_trigger_px; positive is adverse",
        "status": "available" if venue else "unavailable",
        "reason": None if venue else ("no_user_fills" if not provided else "no_stop_row_joined_to_a_venue_fill"),
        "venue_joined": stats,
        "venue_joined_samples": [{"event_key": v["event_key"], "slippage": v["rate"], "fills": v["fills"]}
                                 for v in sorted(venue, key=lambda x: x["event_key"])],
        "export_price_differs": sorted(differs, key=lambda x: x["event_key"]),
        "booked_at_trigger_no_venue_evidence": at_trigger,
        "stop_rows_without_trigger": no_trigger,
        "stop_rows_with_order_id_not_in_user_fills": no_venue_fill,
    }


def _parse_fill_log_arg(arg: str) -> tuple:
    if "=" in arg:
        strategy, path = arg.split("=", 1)
        if strategy and path:
            return strategy, path
    return None, arg


def market_fill_slippage(rows: list, log_args: list) -> dict:
    if not log_args:
        return {"status": "unavailable", "reason": "no_log_extract"}
    lines = []
    inputs = []
    for arg in log_args:
        forced, path = _parse_fill_log_arg(arg)
        try:
            with open(path, "rb") as fh:
                data = fh.read()
        except OSError as exc:
            raise EvidenceInputError(f"fill log {path}: {exc}")
        parsed = 0
        for raw in data.decode("utf-8", errors="replace").splitlines():
            m = FILL_LOG_LINE.match(raw.strip())
            if not m:
                continue
            strategy = m.group("strategy")
            if forced is not None and strategy != forced:
                raise EvidenceInputError(f"fill log {path} line names strategy {strategy!r}, expected {forced!r}")
            lines.append({
                "strategy": strategy,
                "timestamp": datetime.strptime(m.group("ts"), "%Y-%m-%d %H:%M:%S").replace(tzinfo=timezone.utc),
                "fill": float(m.group("fill")), "qty": float(m.group("qty")), "mid": float(m.group("mid")),
                "scale_in": m.group("scale") is not None,
            })
            parsed += 1
        inputs.append({"file": os.path.basename(path), "sha256": _sha256_bytes(data), "lines_parsed": parsed})
    window = timedelta(seconds=LOG_PAIR_WINDOW_SECONDS)
    samples = []
    unpaired = 0
    ambiguous = 0
    imprecise = 0
    used = set()
    for ln in lines:
        if ln["mid"] <= 0:
            unpaired += 1
            continue
        if ln["mid"] <= LOG_PRICE_TOLERANCE:
            imprecise += 1
            continue
        matches = [r for r in rows
                   if r["strategy"] == ln["strategy"]
                   and (r["group"] == "add") == ln["scale_in"]
                   and lc._is_number(r["quantity"]) and round(r["quantity"], 6) == round(ln["qty"], 6)
                   and lc._is_number(r["price"]) and abs(r["price"] - ln["fill"]) <= LOG_PRICE_TOLERANCE + 1e-12
                   and abs(r["timestamp"] - ln["timestamp"]) <= window]
        if not matches:
            unpaired += 1
            continue
        if len(matches) > 1 or matches[0]["event_key"] in used:
            ambiguous += 1
            continue
        row = matches[0]
        used.add(row["event_key"])
        bound = (LOG_PRICE_TOLERANCE * row["price"] / (ln["mid"] * (ln["mid"] - LOG_PRICE_TOLERANCE))
                 * (1 + FLOAT_BOUND_MARGIN))
        if bound > LOG_PRECISION_MAX_BOUND:
            imprecise += 1
            continue
        slip = _adverse(row["side"], ln["mid"], row["price"])
        if slip is None:
            unpaired += 1
            continue
        samples.append({"event_key": row["event_key"], "timestamp": row["timestamp"], "rate": slip,
                        "fee": 0.0, "notional": 0.0, "rounding_bound": bound, "group": row["group"]})
    stats = _stats(samples)
    stats.pop("notional_weighted", None)
    return {
        "status": "available" if samples else "unavailable",
        "reason": None if samples else "no_paired_line_within_precision",
        "label": "reference price as logged",
        "definition": "side-signed (booked fill price - logged reference) / logged reference; positive is adverse",
        "inputs": inputs,
        "lines_parsed": len(lines),
        "paired": stats,
        "samples": [{"event_key": s["event_key"], "group": s["group"], "slippage": s["rate"],
                     "rounding_bound": s["rounding_bound"]} for s in sorted(samples, key=lambda x: x["event_key"])],
        "excluded": {"unpaired": unpaired, "ambiguous": ambiguous, "log_precision_insufficient": imprecise},
        "precision_rule": f"the logged reference uses 2 decimals and the fill price is the exact booked price; "
                          f"rounding_bound = 0.005 * price / (reference * (reference - 0.005)), widened by "
                          f"{FLOAT_BOUND_MARGIN:g} relative for float error, and a sample is "
                          f"kept only when it is <= {LOG_PRECISION_MAX_BOUND:g}",
    }


def build_report(export_paths: list, fill_paths: list, log_args: list) -> dict:
    if not export_paths:
        raise EvidenceInputError("at least one --export is required")
    exports = load_exports(export_paths)
    rows, excluded = select_rows(exports)
    fills, fill_inputs = load_user_fills(fill_paths) if fill_paths else ([], [])
    return {
        "schema": REPORT_SCHEMA,
        "schema_version": REPORT_SCHEMA_VERSION,
        "suggest_only": True,
        "inputs": {
            "exports": [{
                "file": os.path.basename(e["path"]),
                "sha256": e["sha256"],
                "schema_version": e["doc"]["schema_version"],
                "capture_manifest_sha256": e["doc"]["capture_manifest_sha256"],
                "capture_started_at": e["doc"]["capture"].get("started_at"),
                "capture_completed_at": e["doc"]["capture"].get("completed_at"),
                "inspected_revision": e["doc"]["inspected_revision"],
                "partition": e["doc"]["selection"]["partition"],
                "process_strategy_id": e["doc"]["selection"]["process_strategy_id"],
                "events": len(e["doc"]["events"]),
            } for e in exports],
            "user_fills": fill_inputs,
        },
        "rows": {"candidates": len(rows), "excluded": excluded,
                 "selection_rule": "live partition, fee_source userfills, value available and nonzero, not funding"},
        "booked_rates": booked_rates(rows),
        "venue_rates": venue_join(rows, fills, bool(fill_paths)),
        "stop_slippage": stop_slippage(rows, fills, bool(fill_paths)),
        "market_fill_slippage": market_fill_slippage(rows, log_args),
    }


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(
        description="Offline, suggest-only Hyperliquid fee and slippage evidence from booked-ledger exports.")
    ap.add_argument("--export", action="append", default=[], required=True,
                    help="go-trader export ledger JSON file (repeatable)")
    ap.add_argument("--user-fills", action="append", default=[],
                    help="raw Hyperliquid userFillsByTime JSON array (repeatable)")
    ap.add_argument("--fill-log", action="append", default=[],
                    help="log extract with 'Live fill at' lines, optionally <strategy_id>=<path> (repeatable)")
    ap.add_argument("--output", help="write the report here instead of stdout")
    args = ap.parse_args(argv)
    try:
        report = build_report(args.export, args.user_fills, args.fill_log)
    except EvidenceInputError as exc:
        print(json.dumps({"error": str(exc)}), file=sys.stdout)
        print(f"fee_evidence: {exc}", file=sys.stderr)
        return 1
    text = json.dumps(report, indent=2, sort_keys=True, allow_nan=False) + "\n"
    if args.output:
        with open(args.output, "w") as fh:
            fh.write(text)
    else:
        sys.stdout.write(text)
    return 0


if __name__ == "__main__":
    sys.exit(main())
