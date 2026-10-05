#!/usr/bin/env python3

import argparse
import copy
import hashlib
import itertools
import json
import math
import os
import sys
import traceback
from typing import Optional

_HERE = os.path.dirname(os.path.abspath(__file__))
_REPO_ROOT = os.path.abspath(os.path.join(_HERE, ".."))
for _path in (_HERE, os.path.join(_REPO_ROOT, "shared_tools")):
    if _path not in sys.path:
        sys.path.insert(0, _path)

import numpy as np
import pandas as pd

import offline_manifest as om
from backtester import (
    COMPARISON_MODE_APPROXIMATE,
    COMPARISON_MODE_STRICT,
    LEDGER_EVENTS_SCHEMA,
    LEDGER_EVENTS_SCHEMA_VERSION,
    STOP_FIELD_KEYS,
    STOP_GEOMETRY_INPUT_KEYS,
    STOP_OWNERS_NEEDING_ATR,
    STOP_OWNERS_NEEDING_LABEL,
    STOP_REGIME_FIELD_KEYS,
    STOP_SCALAR_FIELD_KEYS,
    STOP_UNITS_LIVE_CONFIG,
    Backtester,
    CapabilityContext,
    CloseCapabilityError,
    _apply_direction_invert_value,
    _close_fraction_columns,
    _finite_number,
    _normalize_open_action,
    _open_action_from_signal,
    _thaw_json,
    _unified_close_params,
    decode_close_validation,
    leverage_evidence,
)

REPORT_SCHEMA = "go-trader.ledger-reconciliation-report"
REPORT_SCHEMA_VERSION = 1
INPUT_SCHEMA = "go-trader.ledger-comparison-input"
SUPPORTED_INPUT_VERSIONS = (1, 2)
EXPORT_SCHEMA = "go-trader.booked-ledger"
SUPPORTED_EXPORT_VERSIONS = (1,)
EXIT_STRICT_SUCCESS = 0
EXIT_NOT_STRICT = 1
EXIT_INPUT_ERROR = 2
EXIT_INTERNAL_ERROR = 3

TOLERANCE_UNITS = {
    "time_seconds": "s",
    "price_relative": "fraction",
    "quantity_absolute": "base_asset",
    "money_absolute": "USD",
}
EVIDENCE_KEYS = ("value", "raw_value", "status", "reason", "provenance")
EVIDENCE_STATUSES = ("available", "unavailable", "not_applicable")
EVENT_EVIDENCE_FIELDS = (
    "symbol", "side", "trade_type", "details", "position_id", "exchange_order_id", "fee_source",
    "regime", "quantity", "price", "value", "exchange_fee", "realized_pnl", "is_close", "pnl_gross",
    "manual", "row_net_pnl", "ledger_delta", "event_kind", "close_reason", "close_extent",
    "position_allocation", "entry_atr", "stop_loss_atr_mult", "stop_loss_trigger_px",
    "stop_loss_oid", "tp_oids_json", "tp_tiers_json",
)
EXPORT_SECTIONS = (
    "schema", "schema_version", "inspected_revision", "capture_manifest_sha256", "time_basis",
    "timestamp_meanings", "selection", "capture", "snapshot_files", "current_effective_configuration",
    "events", "wallet_orphan_context",
)
STARTING_RULES = ("attested", "booked_ledger_replay", "signal_replay")
HIGHER_TIMEFRAME_STRATEGIES = frozenset({"mtf_confluence", "regime_adaptive_htf"})
OBSERVATION_VALIDITY_COLUMNS = {"open_interest_breakout": "oib_oi_valid"}
PROTECTION_FIELDS = (
    "stop_loss_pct", "stop_loss_margin_pct", "trailing_stop_pct", "trailing_stop_atr_mult",
    "stop_loss_atr_mult", "stop_loss_atr_mult_regime", "trailing_stop_atr_mult_regime",
)
STOP_ROLE_FIELDS = PROTECTION_FIELDS + ("trailing_stop_min_move_pct", "max_drawdown_pct", "regime_atr_window")
STOP_EVIDENCE_FIELDS = STOP_FIELD_KEYS + STOP_GEOMETRY_INPUT_KEYS
STOP_BASES = ("raw_config", "loader_resolved")
STOP_DEFAULT_KEYS = ("default_stop_loss_atr_mult", "user_close_defaults", "user_regime_atr_defaults",
                     "platform_max_drawdown_pct")
STOP_EVIDENCE_KEYS = ("raw_fields", "resolved_fields", "leverage_origin", "regime_atr_window")
PROVENANCE_STATUSES = ("verified", "unverified")
LEVERAGE_ORIGINS = ("config", "default")
STOP_CENTRAL_CODES = ("UNSUPPORTED_STOP_OWNER", "MISSING_STOP_INPUT", "UNVERIFIED_MARGIN_LEVERAGE")
STOP_UNAPPROXIMABLE_REASONS = ("stop_capability_refused", "stop_evidence_contradictory",
                               "stop_configuration_invalid")
OWNER_INPUT_FIELDS = {
    "trailing_pct": ("trailing_stop_pct", "trailing_stop_min_move_pct"),
    "trailing_atr": ("trailing_stop_atr_mult", "trailing_stop_min_move_pct"),
    "trailing_atr_regime": ("trailing_stop_atr_mult_regime", "regime_atr_window", "trailing_stop_min_move_pct"),
    "unified_regime": ("close_strategy", "regime_atr_window", "trailing_stop_min_move_pct"),
    "fixed_atr": ("stop_loss_atr_mult", "trailing_stop_min_move_pct"),
    "fixed_atr_regime": ("stop_loss_atr_mult_regime", "regime_atr_window", "trailing_stop_min_move_pct"),
    "fixed_pct": ("stop_loss_pct", "trailing_stop_min_move_pct"),
    "margin_pct": ("stop_loss_margin_pct", "leverage", "trailing_stop_min_move_pct"),
    "drawdown_fallback": ("max_drawdown_pct", "trailing_stop_min_move_pct"),
    "none": (),
}
INITIAL_GEOMETRY_STAMP = "initial_entry"
GEOMETRY_STATUSES = ("agreement", "mismatch", "unverified", "unavailable")
PORTFOLIO_CONTROL_FIELDS = (
    "max_drawdown_pct", "circuit_breaker", "cb_drawdown_cooldown_minutes",
    "cb_loss_streak_threshold", "cb_loss_streak_cooldown_minutes",
)
REGIME_FIELDS = (
    "allowed_regimes", "regime_gate_on_failure", "regime_gate_window",
    "regime_directional_window", "regime_directional_policy", "regime_window_divergence",
    "regime_profile_allocation",
)
UNSUPPORTED_FEATURE_FIELDS = {
    "hurst_gate": "hurst_gate",
    "htf_filter": "higher_timeframe",
    "hedge": "hedge",
    "replay_sharing": "replay_mirror",
    "replay_source_id": "replay_mirror",
    "theta_harvest": "options",
    "futures": "futures_contract",
    "margin_per_trade_usd": "sizing",
    "capital_pct": "sizing",
    "risk_per_trade_pct": "sizing",
}
INFORMATIONAL_FIELDS = {
    "id": "identity",
    "storage_strategy_id": "identity",
    "paper_source": "identity",
    "script": "identity",
    "symbol": "identity",
    "timeframe": "identity",
    "capital": "sizing_source_is_verified_starting_cash",
    "initial_capital": "state_field",
    "allow_no_edge": "dispatch_acknowledgement",
    "notify_ratchet_triggers": "notification",
    "llm_entry_analysis": "advisory",
    "interval_seconds": "observation_cadence_within_time_tolerance",
}


class LedgerInputError(ValueError):
    pass


def _sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def _canonical(obj) -> bytes:
    return json.dumps(obj, sort_keys=True, separators=(",", ":"), ensure_ascii=False,
                      allow_nan=False).encode("utf-8")


def booked_sections_sha256(doc: dict) -> str:
    return _sha256(_canonical({"events": doc["events"],
                               "wallet_orphan_context": doc["wallet_orphan_context"]}))


def _reject_constant(name):
    raise ValueError(f"non-finite JSON constant {name}")


def _loads(data: bytes, label: str):
    try:
        return json.loads(data, parse_constant=_reject_constant)
    except ValueError as exc:
        raise LedgerInputError(f"{label} is not valid finite JSON: {exc}")


def _utc(text, label: str) -> pd.Timestamp:
    if not isinstance(text, str) or not text.strip():
        raise LedgerInputError(f"{label} must be a timestamp string")
    try:
        ts = pd.Timestamp(text)
    except (ValueError, TypeError) as exc:
        raise LedgerInputError(f"{label} {text!r} is not a timestamp: {exc}")
    if ts.tz is None:
        raise LedgerInputError(f"{label} {text!r} has no UTC offset")
    return ts.tz_convert("UTC")


def _iso(ts: pd.Timestamp) -> str:
    return ts.tz_convert("UTC").isoformat().replace("+00:00", "Z")


def _is_number(v) -> bool:
    return isinstance(v, (int, float)) and not isinstance(v, bool) and math.isfinite(v)


def _fsum(values) -> float:
    return math.fsum(values)


def _sum_bound(values) -> float:
    values = list(values)
    return len(values) * sys.float_info.epsilon * math.fsum(abs(v) for v in values)


def _validate_evidence(ev, label: str) -> None:
    if not isinstance(ev, dict) or tuple(sorted(ev)) != tuple(sorted(EVIDENCE_KEYS)):
        raise LedgerInputError(f"{label} is not an evidence object with keys {list(EVIDENCE_KEYS)}")
    if ev["status"] not in EVIDENCE_STATUSES:
        raise LedgerInputError(f"{label} has unknown status {ev['status']!r}")
    if ev["status"] == "available" and ev["value"] is None:
        raise LedgerInputError(f"{label} is available without a value")
    if ev["status"] != "available" and ev["value"] is not None:
        raise LedgerInputError(f"{label} is {ev['status']} but carries a value")
    for key in ("value", "raw_value"):
        v = ev[key]
        if isinstance(v, float) and not math.isfinite(v):
            raise LedgerInputError(f"{label}.{key} is not finite")
    if not isinstance(ev["provenance"], list):
        raise LedgerInputError(f"{label}.provenance is not a list")


def validate_export(doc) -> None:
    if not isinstance(doc, dict):
        raise LedgerInputError("export is not a JSON object")
    missing = [k for k in EXPORT_SECTIONS if k not in doc]
    if missing:
        raise LedgerInputError(f"export lacks sections {missing}")
    if doc["schema"] != EXPORT_SCHEMA or doc["schema_version"] not in SUPPORTED_EXPORT_VERSIONS:
        raise LedgerInputError(
            f"export is {doc['schema']!r} version {doc['schema_version']!r}; this comparison reads "
            f"{EXPORT_SCHEMA!r} versions {list(SUPPORTED_EXPORT_VERSIONS)}")
    if doc["time_basis"] != "UTC":
        raise LedgerInputError(f"export time basis is {doc['time_basis']!r}, not UTC")
    sel = doc["selection"]
    if not isinstance(sel, dict) or not all(isinstance(sel.get(k), str) and sel.get(k) for k in
                                            ("partition", "process_strategy_id", "storage_strategy_id",
                                             "source_role", "platform")):
        raise LedgerInputError("export selection is incomplete")
    if not isinstance(doc["events"], list):
        raise LedgerInputError("export events is not a list")
    seen = set()
    for i, ev in enumerate(doc["events"]):
        label = f"events[{i}]"
        if not isinstance(ev, dict):
            raise LedgerInputError(f"{label} is not an object")
        key = ev.get("event_key")
        if not isinstance(key, str) or not key:
            raise LedgerInputError(f"{label} has no event_key")
        if key in seen:
            raise LedgerInputError(f"event_key {key!r} appears twice")
        seen.add(key)
        for field in ("partition", "process_strategy_id", "storage_strategy_id"):
            if ev.get(field) != sel[field]:
                raise LedgerInputError(f"{key} {field} {ev.get(field)!r} disagrees with the selection")
        _utc(ev.get("timestamp"), f"{key}.timestamp")
        for field in EVENT_EVIDENCE_FIELDS:
            _validate_evidence(ev.get(field), f"{key}.{field}")
        for field in ("quantity", "price", "value", "exchange_fee", "realized_pnl", "row_net_pnl", "ledger_delta"):
            v = ev[field]["value"]
            if v is not None and not _is_number(v):
                raise LedgerInputError(f"{key}.{field} value is not a finite number")
        for field in ("exchange_fee", "realized_pnl", "is_close", "pnl_gross", "ledger_delta", "row_net_pnl",
                      "event_kind"):
            if ev[field]["status"] != "available":
                raise LedgerInputError(f"{key}.{field} is required accounting evidence and is {ev[field]['status']}")
    ctx = doc["wallet_orphan_context"]
    if not isinstance(ctx, dict) or not isinstance(ctx.get("records"), list):
        raise LedgerInputError("wallet_orphan_context is malformed")
    for rec in ctx["records"]:
        if not _is_number(rec.get("amount_usd")):
            raise LedgerInputError("wallet_orphan_context amount is not a finite number")


def load_export(path: str):
    with open(path, "rb") as fh:
        data = fh.read()
    doc = _loads(data, f"export {path}")
    validate_export(doc)
    return data, doc


def _resolve_input_path(base_dir: str, rel, label: str) -> str:
    if not isinstance(rel, str) or not rel or os.path.isabs(rel):
        raise LedgerInputError(f"{label} must be a relative path, got {rel!r}")
    path = os.path.normpath(os.path.join(base_dir, rel))
    if not path.startswith(base_dir + os.sep):
        raise LedgerInputError(f"{label} escapes the comparison-input directory: {rel!r}")
    return path


def _require(obj, key, kind, label):
    value = obj.get(key) if isinstance(obj, dict) else None
    if not isinstance(value, kind) or (isinstance(value, bool) and kind is not bool):
        raise LedgerInputError(f"{label}.{key} must be {kind.__name__}")
    return value


def validate_comparison_input(doc, base_dir: str) -> dict:
    if not isinstance(doc, dict):
        raise LedgerInputError("comparison input is not a JSON object")
    version = doc.get("schema_version")
    if doc.get("schema") != INPUT_SCHEMA or isinstance(version, bool) or version not in SUPPORTED_INPUT_VERSIONS:
        raise LedgerInputError(
            f"comparison input must be {INPUT_SCHEMA!r} version {list(SUPPORTED_INPUT_VERSIONS)}")
    exp = doc.get("export")
    for key in ("schema", "capture_manifest_sha256", "booked_sections_sha256"):
        _require(exp, key, str, "export")
    _require(exp, "schema_version", int, "export")
    sel = _require(exp, "selection", dict, "export")
    for key in ("partition", "process_strategy_id"):
        _require(sel, key, str, "export.selection")
    market = _require(doc, "market", dict, "input")
    mref = _require(market, "manifest", dict, "market")
    _require(mref, "sha256", str, "market.manifest")
    manifest_path = _resolve_input_path(base_dir, mref.get("path"), "market.manifest.path")
    _require(market, "dataset", str, "market")
    _require(market, "window", str, "market")
    interval = _require(doc, "interval", dict, "input")
    start = _utc(interval.get("start"), "interval.start")
    end = _utc(interval.get("end"), "interval.end")
    if start >= end:
        raise LedgerInputError("interval.start must precede interval.end")
    if interval.get("start_inclusive") is not True or interval.get("end_inclusive") is not False:
        raise LedgerInputError("interval boundaries must be start_inclusive=true and end_inclusive=false, "
                               "the frozen-window rule this comparison supports")
    tol = _require(doc, "tolerances", dict, "input")
    tolerances = {}
    for key, unit in TOLERANCE_UNITS.items():
        entry = _require(tol, key, dict, "tolerances")
        if entry.get("unit") != unit or not _is_number(entry.get("value")) or entry["value"] < 0:
            raise LedgerInputError(f"tolerances.{key} must be {{value >= 0, unit {unit!r}}}")
        tolerances[key] = float(entry["value"])
    state = _require(doc, "starting_state", dict, "input")
    for key in ("cash_usd", "inventory", "pending_decision"):
        entry = _require(state, key, dict, "starting_state")
        if entry.get("status") not in ("verified", "unverified"):
            raise LedgerInputError(f"starting_state.{key}.status must be verified or unverified")
        if entry.get("rule") not in STARTING_RULES:
            raise LedgerInputError(f"starting_state.{key}.rule must be one of {list(STARTING_RULES)}")
    hist = _require(doc, "historical_configuration", dict, "input")
    timeline = _require(hist, "timeline", list, "historical_configuration")
    for i, seg in enumerate(timeline):
        label = f"historical_configuration.timeline[{i}]"
        _utc(seg.get("effective_from"), f"{label}.effective_from")
        if seg.get("effective_to") is not None:
            _utc(seg.get("effective_to"), f"{label}.effective_to")
        for key in ("strategy", "regime", "portfolio_risk"):
            _require(seg, key, dict, label)
        _validate_strategy_shape(seg["strategy"], f"{label}.strategy")
        if seg.get("status") not in ("verified", "unverified"):
            raise LedgerInputError(f"{label}.status must be verified or unverified")
        if version >= 2:
            _validate_stop_segment(seg, label)
        else:
            extra = sorted(k for k in ("basis", "stop_defaults", "stop_evidence") if k in seg)
            if extra:
                raise LedgerInputError(f"{label} carries version 2 stop evidence {extra} in a version 1 input")
    capev = doc.get("capability_evidence", {})
    if not isinstance(capev, dict):
        raise LedgerInputError("capability_evidence must be an object")
    geometry = doc.get("initial_stop_geometry_evidence")
    if geometry is not None:
        if version < 2:
            raise LedgerInputError("initial_stop_geometry_evidence needs comparison input version 2")
        _validate_geometry_evidence(geometry)
    return {"manifest_path": manifest_path, "start": start, "end": end, "tolerances": tolerances,
            "input_version": version}


def _validate_provenance(entry, label: str, presence: bool) -> None:
    keys = ("status", "source", "present", "value") if presence else ("status", "source", "value")
    if not isinstance(entry, dict) or set(entry) != set(keys):
        raise LedgerInputError(f"{label} must be an object with keys {list(keys)}")
    if entry["status"] not in PROVENANCE_STATUSES:
        raise LedgerInputError(f"{label}.status must be one of {list(PROVENANCE_STATUSES)}")
    source = entry["source"]
    if source is not None and not isinstance(source, str):
        raise LedgerInputError(f"{label}.source must be a string or null")
    if entry["status"] == "verified" and not (isinstance(source, str) and source.strip()):
        raise LedgerInputError(f"{label} is verified without a source")
    if presence:
        if not isinstance(entry["present"], bool):
            raise LedgerInputError(f"{label}.present must be a boolean")
        if not entry["present"] and entry["value"] is not None:
            raise LedgerInputError(f"{label} is absent but carries a value")
        if entry["present"] and entry["value"] is None:
            raise LedgerInputError(f"{label} is present without a value; an absent field has present=false")


def _validate_stop_segment(seg: dict, label: str) -> None:
    if seg.get("basis") not in STOP_BASES:
        raise LedgerInputError(f"{label}.basis must be one of {list(STOP_BASES)}")
    defaults = _require(seg, "stop_defaults", dict, label)
    if set(defaults) != set(STOP_DEFAULT_KEYS):
        raise LedgerInputError(f"{label}.stop_defaults must have exactly the keys {list(STOP_DEFAULT_KEYS)}")
    for key in STOP_DEFAULT_KEYS:
        _validate_provenance(defaults[key], f"{label}.stop_defaults.{key}", True)
    for key in ("user_close_defaults", "user_regime_atr_defaults"):
        value = defaults[key]["value"]
        if value is not None and not isinstance(value, dict):
            raise LedgerInputError(f"{label}.stop_defaults.{key}.value must be an object or null")
    evidence = _require(seg, "stop_evidence", dict, label)
    unknown = sorted(set(evidence) - set(STOP_EVIDENCE_KEYS))
    if unknown:
        raise LedgerInputError(f"{label}.stop_evidence has unknown keys {unknown}")
    raw = _require(evidence, "raw_fields", dict, f"{label}.stop_evidence")
    for key, entry in raw.items():
        if key not in STOP_EVIDENCE_FIELDS:
            raise LedgerInputError(f"{label}.stop_evidence.raw_fields has unknown field {key!r}")
        _validate_provenance(entry, f"{label}.stop_evidence.raw_fields.{key}", True)
    if seg["basis"] == "loader_resolved":
        resolved = _require(evidence, "resolved_fields", dict, f"{label}.stop_evidence")
        for key, entry in resolved.items():
            if key not in STOP_EVIDENCE_FIELDS:
                raise LedgerInputError(f"{label}.stop_evidence.resolved_fields has unknown field {key!r}")
            _validate_provenance(entry, f"{label}.stop_evidence.resolved_fields.{key}", False)
    elif "resolved_fields" in evidence:
        raise LedgerInputError(f"{label}.stop_evidence.resolved_fields applies only to basis loader_resolved")
    origin = evidence.get("leverage_origin")
    _validate_provenance(origin, f"{label}.stop_evidence.leverage_origin", False)
    if origin["value"] not in LEVERAGE_ORIGINS + (None,) or (
            origin["status"] == "verified" and origin["value"] not in LEVERAGE_ORIGINS):
        raise LedgerInputError(
            f"{label}.stop_evidence.leverage_origin.value must be one of {list(LEVERAGE_ORIGINS)}")
    window = evidence.get("regime_atr_window")
    _validate_provenance(window, f"{label}.stop_evidence.regime_atr_window", True)
    if window["value"] is not None and not isinstance(window["value"], str):
        raise LedgerInputError(f"{label}.stop_evidence.regime_atr_window.value must be a string or null")


def _validate_geometry_evidence(geometry) -> None:
    if not isinstance(geometry, dict):
        raise LedgerInputError("initial_stop_geometry_evidence must be an object keyed by booked event_key")
    for key, entry in geometry.items():
        label = f"initial_stop_geometry_evidence[{key!r}]"
        if not isinstance(entry, dict) or set(entry) != {"status", "source", "stamp"}:
            raise LedgerInputError(f"{label} must be an object with keys ['status', 'source', 'stamp']")
        if entry["status"] not in PROVENANCE_STATUSES:
            raise LedgerInputError(f"{label}.status must be one of {list(PROVENANCE_STATUSES)}")
        if entry["source"] is not None and not isinstance(entry["source"], str):
            raise LedgerInputError(f"{label}.source must be a string or null")
        if entry["status"] == "verified" and not (isinstance(entry["source"], str) and entry["source"].strip()):
            raise LedgerInputError(f"{label} is verified without a source")
        if entry["stamp"] != INITIAL_GEOMETRY_STAMP:
            raise LedgerInputError(f"{label}.stamp must be {INITIAL_GEOMETRY_STAMP!r}")


def _validate_strategy_shape(strategy: dict, label: str) -> None:
    args = strategy.get("args")
    if args is not None and not (isinstance(args, list) and all(isinstance(a, str) for a in args)):
        raise LedgerInputError(f"{label}.args must be a list of strings")
    open_ref = strategy.get("open_strategy")
    if open_ref is not None:
        if not isinstance(open_ref, dict):
            raise LedgerInputError(f"{label}.open_strategy must be an object")
        name = open_ref.get("name")
        if name is not None and not isinstance(name, str):
            raise LedgerInputError(f"{label}.open_strategy.name must be a string")
        params = open_ref.get("params")
        if params is not None and not isinstance(params, dict):
            raise LedgerInputError(f"{label}.open_strategy.params must be an object")
    for key in ("type", "platform", "symbol", "direction", "atr_method"):
        value = strategy.get(key)
        if value is not None and not isinstance(value, str):
            raise LedgerInputError(f"{label}.{key} must be a string")


def _evidence_verified(entry: Optional[dict]) -> bool:
    return (isinstance(entry, dict) and entry.get("status") == "verified"
            and isinstance(entry.get("evidence"), str) and entry["evidence"].strip() != ""
            and isinstance(entry.get("rule"), str) and entry["rule"].strip() != "")


def _ev(event: dict, field: str):
    item = event[field]
    return item["value"] if item["status"] == "available" else None


def _side_from_action(action: Optional[str], is_close: bool) -> Optional[str]:
    a = (action or "").strip().lower()
    if a == "buy":
        return "short" if is_close else "long"
    if a == "sell":
        return "long" if is_close else "short"
    return None


def normalize_booked(doc: dict, start: pd.Timestamp, end: pd.Timestamp, coin: str,
                     not_comparable_reason: Optional[str]) -> dict:
    dispositions = {}
    positions = {}
    funding_in, funding_out = [], []
    unresolved = []
    outside = []
    not_comparable = []
    records = []
    for ev in doc["events"]:
        ts = _utc(ev["timestamp"], ev["event_key"])
        rel = "in" if start <= ts < end else ("before" if ts < start else "after")
        rec = {
            "event_key": ev["event_key"],
            "timestamp": _iso(ts),
            "ts": ts,
            "relation": rel,
            "kind": _ev(ev, "event_kind"),
            "symbol": _ev(ev, "symbol"),
            "action": _ev(ev, "side"),
            "position_id": _ev(ev, "position_id"),
            "quantity": _ev(ev, "quantity"),
            "price": _ev(ev, "price"),
            "fee": _ev(ev, "exchange_fee"),
            "realized_pnl": _ev(ev, "realized_pnl"),
            "pnl_gross": _ev(ev, "pnl_gross"),
            "is_close": _ev(ev, "is_close"),
            "row_net_pnl": _ev(ev, "row_net_pnl"),
            "ledger_delta": _ev(ev, "ledger_delta"),
            "close_reason": _ev(ev, "close_reason"),
            "close_extent": _ev(ev, "close_extent"),
            "fee_source": _ev(ev, "fee_source"),
            "_stop_trigger": ev["stop_loss_trigger_px"],
            "_entry_atr": ev["entry_atr"],
        }
        records.append(rec)
    records.sort(key=lambda r: (r["ts"], r["event_key"]))
    field_evidence = {}
    for ev in doc["events"]:
        for field in EVENT_EVIDENCE_FIELDS:
            item = ev[field]
            label = item["status"] if item["reason"] is None else f"{item['status']}:{item['reason']}"
            counts = field_evidence.setdefault(field, {})
            counts[label] = counts.get(label, 0) + 1
    for rec in records:
        key = rec["event_key"]
        if not_comparable_reason is not None:
            dispositions[key] = "not_comparable"
            not_comparable.append({"event_key": key, "reason": not_comparable_reason})
            continue
        if rec["kind"] == "funding":
            if rec["relation"] == "in":
                dispositions[key] = "strategy_funding"
                funding_in.append(rec)
            else:
                dispositions[key] = "outside_interval"
                funding_out.append(rec)
                outside.append({"event_key": key, "relation": rec["relation"], "position_id": None,
                                "kind": "funding"})
            continue
        reason = None
        if rec["kind"] not in ("non_close", "scale_in", "close"):
            reason = f"event_kind {rec['kind']!r} is not a position fill"
        elif not rec["position_id"]:
            reason = "no recorded position_id; position allocation is unresolved"
        elif rec["symbol"] != coin:
            reason = f"symbol {rec['symbol']!r} is not the configured coin {coin!r}"
        elif not (_is_number(rec["quantity"]) and rec["quantity"] > 0 and _is_number(rec["price"]) and rec["price"] > 0):
            reason = "quantity or price is missing or not positive"
        elif _side_from_action(rec["action"], rec["kind"] == "close") is None:
            reason = f"action side {rec['action']!r} is not buy or sell"
        if reason is not None:
            if rec["relation"] == "in":
                dispositions[key] = "unresolved_identity"
                unresolved.append({"event_key": key, "reason": reason, "timestamp": rec["timestamp"]})
            else:
                dispositions[key] = "outside_interval"
                outside.append({"event_key": key, "relation": rec["relation"], "position_id": rec["position_id"],
                                "kind": rec["kind"], "unresolved_reason": reason})
            continue
        pos = positions.setdefault(rec["position_id"], {"position_id": rec["position_id"], "events": []})
        pos["events"].append(rec)
    out_positions = []
    for pid in sorted(positions, key=lambda p: (positions[p]["events"][0]["ts"], p)):
        pos = positions[pid]
        evs = pos["events"]
        open_sides = {_side_from_action(e["action"], False) for e in evs if e["kind"] in ("non_close", "scale_in")}
        close_sides = {_side_from_action(e["action"], True) for e in evs if e["kind"] == "close"}
        sides = open_sides | close_sides
        if len(sides) != 1:
            direction, evidence = None, "contradictory"
        elif open_sides and close_sides:
            direction, evidence = next(iter(sides)), "opening_and_closing_sides_agree"
        elif open_sides:
            direction, evidence = next(iter(sides)), "opening_side_only"
        else:
            direction, evidence = None, "closing_side_only"
        before = [e for e in evs if e["relation"] == "before"]
        inside = [e for e in evs if e["relation"] == "in"]
        after = [e for e in evs if e["relation"] == "after"]
        start_inv = _fsum(e["quantity"] for e in before if e["kind"] != "close") - \
            _fsum(e["quantity"] for e in before if e["kind"] == "close")
        opened = _fsum(e["quantity"] for e in inside if e["kind"] != "close")
        closed = _fsum(e["quantity"] for e in inside if e["kind"] == "close")
        p = {
            "position_id": pid,
            "ownership": {"partition": doc["selection"]["partition"],
                          "process_strategy_id": doc["selection"]["process_strategy_id"],
                          "symbol": coin, "side": direction},
            "direction_evidence": evidence,
            "event_keys": [e["event_key"] for e in evs],
            "in_interval_event_keys": [e["event_key"] for e in inside],
            "crosses_interval_start": bool(before) and bool(inside or after),
            "crosses_interval_end": bool(after) and bool(inside or before),
            "start_inventory": start_inv,
            "opened_qty": opened,
            "closed_qty": closed,
            "residual_qty": start_inv + opened - closed,
            "entry_fees": _fsum(e["fee"] for e in inside if e["kind"] != "close"),
            "exit_fees": _fsum(e["fee"] for e in inside if e["kind"] == "close"),
            "ledger_delta": _fsum(e["ledger_delta"] for e in inside),
            "row_net_pnl": _fsum(e["row_net_pnl"] for e in inside),
            "legacy_rows": [e["event_key"] for e in inside if not e["pnl_gross"]],
            "_events": evs,
        }
        if not inside:
            for e in evs:
                dispositions[e["event_key"]] = "outside_interval"
                outside.append({"event_key": e["event_key"], "relation": e["relation"], "position_id": pid,
                                "kind": e["kind"]})
            p["status"] = "outside_interval"
        elif direction is None:
            for e in inside:
                dispositions[e["event_key"]] = "unresolved_identity"
                unresolved.append({"event_key": e["event_key"], "timestamp": e["timestamp"],
                                   "reason": f"position {pid} direction is {evidence}"})
            for e in before + after:
                dispositions[e["event_key"]] = "outside_interval"
                outside.append({"event_key": e["event_key"], "relation": e["relation"], "position_id": pid,
                                "kind": e["kind"]})
            p["status"] = "unresolved_identity"
        else:
            for e in before + after:
                dispositions[e["event_key"]] = "outside_interval"
                outside.append({"event_key": e["event_key"], "relation": e["relation"], "position_id": pid,
                                "kind": e["kind"]})
            p["status"] = "comparable"
        out_positions.append(p)
    return {
        "records": records,
        "positions": out_positions,
        "funding_in": funding_in,
        "funding_out": funding_out,
        "unresolved": unresolved,
        "outside": outside,
        "not_comparable": not_comparable,
        "dispositions": dispositions,
        "field_evidence": field_evidence,
    }


def booked_start_state(norm: dict) -> dict:
    before = [r for r in norm["records"] if r["relation"] == "before"]
    unresolved_before = [o for o in norm["outside"] if o["relation"] == "before" and o.get("unresolved_reason")]
    inventory = 0.0
    for p in norm["positions"]:
        if p["start_inventory"]:
            sign = 1.0 if p["ownership"]["side"] == "long" else (-1.0 if p["ownership"]["side"] == "short" else None)
            if sign is None:
                inventory = None
                break
            inventory += sign * p["start_inventory"]
    return {
        "ledger_delta_before_start": _fsum(r["ledger_delta"] for r in before),
        "inventory_before_start": None if unresolved_before else inventory,
        "unresolved_before_start": [o["event_key"] for o in unresolved_before],
    }


def verify_starting_state(spec: dict, booked_start: dict, pending_recomputed: Optional[str],
                          tolerances: dict) -> dict:
    out = {}
    money_tol = tolerances["money_absolute"]
    qty_tol = tolerances["quantity_absolute"]

    def entry(name, value_ok, recomputed=None, note=None):
        src = spec[name]
        declared_verified = _evidence_verified(src)
        status = "verified" if declared_verified and value_ok else "unverified"
        res = {
            "value": src.get("value", src.get("quantity")),
            "declared_status": src.get("status"),
            "status": status,
            "rule": src.get("rule"),
            "evidence": src.get("evidence"),
            "verification": ("recomputed" if src.get("rule") in ("booked_ledger_replay", "signal_replay")
                             else "attested_by_comparison_input"),
        }
        if recomputed is not None:
            res["recomputed"] = recomputed
        if note:
            res["note"] = note
        return res

    cash = spec["cash_usd"]
    cash_ok = _is_number(cash.get("value"))
    recomputed = None
    note = None
    if cash.get("rule") == "booked_ledger_replay":
        inputs = cash.get("rule_inputs") or {}
        base = inputs.get("base_value")
        if not (_is_number(base) and isinstance(inputs.get("base_evidence"), str) and inputs["base_evidence"].strip()):
            cash_ok = False
            note = "booked_ledger_replay needs rule_inputs.base_value and rule_inputs.base_evidence"
        else:
            recomputed = base + booked_start["ledger_delta_before_start"]
            cash_ok = cash_ok and abs(recomputed - cash["value"]) <= money_tol
            if not cash_ok:
                note = "declared starting cash differs from the booked-ledger replay"
    elif cash.get("rule") != "attested":
        cash_ok = False
        note = f"rule {cash.get('rule')!r} cannot verify starting cash"
    out["cash_usd"] = entry("cash_usd", cash_ok, recomputed, note)

    inv = spec["inventory"]
    inv_value = inv.get("quantity")
    inv_ok = _is_number(inv_value)
    recomputed = booked_start["inventory_before_start"]
    note = None
    if inv.get("rule") not in ("booked_ledger_replay", "attested"):
        inv_ok = False
        note = f"rule {inv.get('rule')!r} cannot verify starting inventory"
    elif recomputed is None:
        inv_ok = False
        note = ("booked events before the interval have unresolved position identity; the booked ledger "
                "cannot confirm the starting inventory")
    else:
        inv_ok = inv_ok and abs(recomputed - inv_value) <= qty_tol
        if not inv_ok:
            note = "declared starting inventory differs from the booked-ledger replay"
    out["inventory"] = entry("inventory", inv_ok, recomputed, note)
    out["inventory"]["value"] = inv_value
    if inv.get("rule") == "attested":
        out["inventory"]["verification"] = "attested_and_checked_against_booked_ledger_replay"

    pend = spec["pending_decision"]
    pend_ok = isinstance(pend.get("value"), str)
    note = None
    if pend.get("rule") == "signal_replay":
        if pending_recomputed is None:
            pend_ok = False
            note = "the boundary decision could not be recomputed from frozen candles"
        else:
            pend_ok = pend_ok and pend["value"] == pending_recomputed
            if not pend_ok:
                note = "declared pending decision differs from the strategy signal on the last warm-up bar"
    elif pend.get("rule") != "attested":
        pend_ok = False
        note = f"rule {pend.get('rule')!r} cannot verify the pending decision"
    out["pending_decision"] = entry("pending_decision", pend_ok, pending_recomputed, note)
    return out


def select_historical_configuration(timeline: list, start: pd.Timestamp, end: pd.Timestamp) -> dict:
    covering = []
    overlapping = []
    for i, seg in enumerate(timeline):
        f = _utc(seg["effective_from"], "effective_from")
        t = _utc(seg["effective_to"], "effective_to") if seg.get("effective_to") is not None else None
        overlaps = f < end and (t is None or t > start)
        if overlaps:
            overlapping.append(i)
            if f <= start and (t is None or t >= end):
                covering.append(i)
    if len(overlapping) > 1:
        return {"status": "refused", "reason": "configuration_transition_unsupported",
                "segments": overlapping,
                "detail": "the historical configuration changes inside the comparison interval; the "
                          "simulator cannot switch configuration without resetting state"}
    if not covering:
        return {"status": "unverified", "reason": "historical_configuration_missing",
                "segments": overlapping,
                "detail": "no historical configuration segment covers the whole comparison interval"}
    seg = timeline[covering[0]]
    return {"status": "verified" if _evidence_verified(seg) else "unverified",
            "reason": None if _evidence_verified(seg) else "historical_configuration_unverified",
            "segment_index": covering[0], "segment": seg}


def _active(value) -> bool:
    if value is None or value is False:
        return False
    if isinstance(value, (int, float)) and not isinstance(value, bool):
        return value != 0
    if isinstance(value, (str, list, dict)):
        return len(value) > 0
    return True


def _same_value(a, b) -> bool:
    if _is_number(a) and _is_number(b):
        return float(a) == float(b)
    if isinstance(a, dict) and isinstance(b, dict):
        return set(a) == set(b) and all(_same_value(a[k], b[k]) for k in a)
    if isinstance(a, list) and isinstance(b, list):
        return len(a) == len(b) and all(_same_value(x, y) for x, y in zip(a, b))
    return type(a) is type(b) and a == b


def _provenance_verified(entry) -> bool:
    return (isinstance(entry, dict) and entry.get("status") == "verified"
            and isinstance(entry.get("source"), str) and entry["source"].strip() != "")


def _use_defaults_only(block) -> bool:
    return isinstance(block, dict) and block.get("use_defaults") is True and block.get("trend_regime") is None


def _jsonable_stop_inputs(kwargs: dict) -> dict:
    return {k: _thaw_json(kwargs.get(k)) for k in STOP_EVIDENCE_FIELDS}


def _stop_refusal(reason: str, field: str, detail: str, approximable: bool, **extra) -> dict:
    out = {"reason": reason, "field": field, "detail": detail, "approximable": approximable}
    out.update(extra)
    return out


def _raw_config_document(seg: dict, with_defaults: bool) -> dict:
    cfg = {"regime": copy.deepcopy(seg.get("regime") or {}),
           "strategies": [copy.deepcopy(seg.get("strategy") or {})]}
    if not with_defaults:
        return cfg
    defaults = seg["stop_defaults"]

    def present(key):
        entry = defaults[key]
        return entry["present"], copy.deepcopy(entry["value"])

    ok, value = present("default_stop_loss_atr_mult")
    if ok:
        cfg["default_stop_loss_atr_mult"] = value
    user = {}
    ok, value = present("user_close_defaults")
    if ok:
        user["close"] = value
    ok, value = present("user_regime_atr_defaults")
    if ok:
        user["regime_atr"] = value
    if user:
        cfg["user_defaults"] = user
    ok, value = present("platform_max_drawdown_pct")
    if ok:
        cfg["platforms"] = {"hyperliquid": {"risk": {"max_drawdown_pct": value}}}
    return cfg


def _raw_stop_contradictions(strategy: dict, evidence: dict) -> list:
    out = []
    for key, entry in sorted(evidence["raw_fields"].items()):
        declared = strategy.get(key)
        if entry["present"] != (declared is not None) or (
                entry["present"] and not _same_value(entry["value"], declared)):
            out.append((key, f"raw evidence {{present: {entry['present']}, value: {entry['value']!r}}} "
                             f"disagrees with the raw strategy value {declared!r}"))
    window = evidence["regime_atr_window"]
    declared = strategy.get("regime_atr_window")
    if window["present"] != (declared is not None) or (
            window["present"] and not _same_value(window["value"], declared)):
        out.append(("regime_atr_window", f"window evidence {window['value']!r} disagrees with the raw "
                                         f"strategy value {declared!r}"))
    origin = evidence["leverage_origin"]
    if origin["status"] == "verified":
        expected = "config" if (_finite_number(strategy.get("leverage")) or 0) > 0 else "default"
        if origin["value"] != expected:
            out.append(("leverage", f"leverage origin {origin['value']!r} disagrees with the raw leverage "
                                    f"{strategy.get('leverage')!r}"))
    return out


def _loader_stop_contradictions(strategy: dict, evidence: dict) -> list:
    out = []
    raw, resolved = evidence["raw_fields"], evidence["resolved_fields"]
    raw_owner_present = any(raw.get(k, {}).get("present") for k in STOP_FIELD_KEYS)
    close_name = str((strategy.get("close_strategy") or {}).get("name") or "").strip().lower()
    defaultable = {"stop_loss_atr_mult": not raw_owner_present,
                   "trailing_stop_atr_mult_regime": close_name == "trailing_tp_ratchet_regime" and not raw_owner_present}
    for key, entry in sorted(resolved.items()):
        declared = strategy.get(key)
        if declared is not None and not _same_value(entry["value"], declared):
            out.append((key, f"resolved evidence {entry['value']!r} disagrees with the loader-resolved "
                             f"strategy value {declared!r}"))
    for key in STOP_EVIDENCE_FIELDS:
        rv, sv = raw.get(key), resolved.get(key)
        if rv is None or sv is None:
            continue
        value = sv["value"]
        if key in STOP_SCALAR_FIELD_KEYS or key == "trailing_stop_min_move_pct":
            if rv["present"] and not _same_value(rv["value"], value):
                out.append((key, f"explicit raw value {rv['value']!r} differs from the resolved value {value!r}; "
                                 "the loader never rewrites an explicit stop field"))
            elif not rv["present"] and value is not None and not defaultable.get(key):
                out.append((key, f"resolved value {value!r} has no raw field and no loader default produces it"))
        elif key in STOP_REGIME_FIELD_KEYS:
            if rv["present"] and not _use_defaults_only(rv["value"]) and not _same_value(rv["value"], value):
                out.append((key, "explicit raw regime block differs from the resolved block"))
            elif not rv["present"] and value is not None and not defaultable.get(key):
                out.append((key, "resolved regime block has no raw field and no loader default produces it"))
        elif rv["present"] and (_finite_number(rv["value"]) or 0) > 0 and not _same_value(rv["value"], value):
            out.append((key, f"explicit raw value {rv['value']!r} differs from the resolved value {value!r}"))
    origin = evidence["leverage_origin"]
    raw_lev = raw.get("leverage")
    if origin["status"] == "verified" and _provenance_verified(raw_lev):
        explicit = raw_lev["present"] and (_finite_number(raw_lev["value"]) or 0) > 0
        if (origin["value"] == "config") != explicit:
            out.append(("leverage", f"leverage origin {origin['value']!r} disagrees with the raw leverage "
                                    f"evidence {raw_lev['value']!r}"))
    window = evidence["regime_atr_window"]
    declared = strategy.get("regime_atr_window")
    if declared is not None and not (window["present"] and _same_value(window["value"], declared)):
        out.append(("regime_atr_window", f"window evidence {window['value']!r} disagrees with the strategy "
                                         f"value {declared!r}"))
    return out


def _default_requirements(seg: dict, close_refs: list) -> list:
    from run_backtest import _USER_CLOSE_DEFAULTS_SUPPORTED
    strategy = seg.get("strategy") or {}
    needed = []
    scalar = any(strategy.get(k) is not None for k in STOP_SCALAR_FIELD_KEYS)
    block = any(isinstance(strategy.get(k), dict) and strategy.get(k) for k in STOP_REGIME_FIELD_KEYS)
    if not scalar and not block and _unified_close_params(close_refs) is None:
        needed.append("default_stop_loss_atr_mult")
    for ref in close_refs:
        name = str(ref.get("name") or "").strip().lower()
        if name in _USER_CLOSE_DEFAULTS_SUPPORTED and (
                (ref.get("params") or {}).get("tp_tiers") is None or name == "trailing_tp_ratchet_regime"):
            needed.append("user_close_defaults")
            break
    if any(_use_defaults_only(strategy.get(k)) for k in STOP_REGIME_FIELD_KEYS):
        needed.append("user_regime_atr_defaults")
    return needed


def resolve_historical_stops(seg: dict, input_version: int, comparison_mode: str) -> dict:
    from run_backtest import (_atr_window_evidence, _resolve_regime_windows_spec,
                              live_stop_engine_inputs, live_stop_preflight, resolve_raw_config_stops,
                              strategy_close_refs)
    strategy = seg.get("strategy") or {}
    regime = seg.get("regime") or {}
    basis = seg.get("basis") if input_version >= 2 else None
    verdict = {"applicable": True, "input_version": input_version, "basis": basis, "status": "refused",
               "owner": None, "refusals": [], "kwargs": None, "close_refs": None, "needs_labels": False,
               "labels": None, "required_inputs": [], "resolved_live_units": None, "capability_context": None}
    stype = str(strategy.get("type") or "")
    platform = str(strategy.get("platform") or "").strip().lower()
    if stype != "perps" or platform != "hyperliquid":
        verdict.update(applicable=False, status="not_applicable")
        return verdict
    label = f"historical strategy {strategy.get('id')!r}"
    refusals = verdict["refusals"]
    try:
        if basis == "loader_resolved":
            evidence = seg["stop_evidence"]
            for key, why in _loader_stop_contradictions(strategy, evidence):
                refusals.append(_stop_refusal("stop_evidence_contradictory", key, why, False))
            resolved = {}
            for key in STOP_EVIDENCE_FIELDS:
                entry = evidence["resolved_fields"].get(key)
                resolved[key] = copy.deepcopy(entry["value"] if entry is not None else strategy.get(key))
            origin = evidence["leverage_origin"]
            lev = resolved.get("leverage")
            if _provenance_verified(origin) and origin["value"] == "config":
                lev_ev = leverage_evidence(lev, "config", True)
            elif _provenance_verified(origin):
                lev_ev = {"status": "unverified", "source": "default", "value": _finite_number(lev)}
            else:
                lev_ev = {"status": "unverified", "source": "historical_unverified", "value": _finite_number(lev)}
            raw_fields = {k: {"present": e["present"], "value": copy.deepcopy(e["value"])}
                          for k, e in sorted(evidence["raw_fields"].items())}
            window_value = evidence["regime_atr_window"]["value"]
            kwargs, context = live_stop_engine_inputs(
                resolved, raw_fields=raw_fields, source=STOP_UNITS_LIVE_CONFIG, leverage=lev_ev,
                extra_evidence={"atr_regime_window": _atr_window_evidence(
                    {"regime_atr_window": window_value}, regime)})
            kwargs["stop_platform"] = "hyperliquid"
            close_refs = strategy_close_refs(strategy, label)
        else:
            if basis == "raw_config":
                evidence = seg["stop_evidence"]
                for key, why in _raw_stop_contradictions(strategy, evidence):
                    refusals.append(_stop_refusal("stop_evidence_contradictory", key, why, False))
            stops = resolve_raw_config_stops(_raw_config_document(seg, basis == "raw_config"),
                                             strategy.get("id"), label)
            kwargs, context, close_refs = stops["stop_kwargs"], stops["stop_context"], stops["close_refs"]
            lev_ev = dict(context.input_evidence.get("leverage") or {})
            if basis == "raw_config" and lev_ev.get("status") == "verified" and not _provenance_verified(
                    seg["stop_evidence"]["raw_fields"].get("leverage")):
                lev_ev = {"status": "unverified", "source": "historical_unverified", "value": lev_ev.get("value")}
                merged = _thaw_json(context.input_evidence)
                merged["leverage"] = lev_ev
                context = CapabilityContext(raw_fields=_thaw_json(context.raw_fields), input_evidence=merged)
        spec = _resolve_regime_windows_spec(regime)
        try:
            validation = live_stop_preflight(
                kwargs, context, platform="hyperliquid", strategy_type="perps", close_refs=close_refs,
                regime_windows_spec=spec, risk_per_trade_pct=strategy.get("risk_per_trade_pct"),
                comparison_mode=comparison_mode)
            preflight, central = validation.capability_context, []
        except CloseCapabilityError as exc:
            preflight = exc.validation.capability_context
            central = [r for r in exc.reasons if r["reason_code"] in STOP_CENTRAL_CODES]
    except (ValueError, TypeError) as exc:
        refusals.append(_stop_refusal("stop_configuration_invalid", "stop_configuration",
                                      f"the historical stop configuration cannot be resolved: {exc}", False))
        return verdict
    if preflight is None or preflight.errors or preflight.resolved_stop_owner is None:
        refusals.append(_stop_refusal("stop_configuration_invalid", "capability_context",
                                      "the central stop context could not be built", False))
        return verdict
    owner = preflight.resolved_stop_owner["name"]
    verdict.update(owner=owner, close_refs=close_refs,
                   resolved_live_units=_thaw_json(context.input_evidence["resolved_live_units"]["value"]),
                   capability_context=preflight.to_dict())
    for rec in central:
        refusals.append(_stop_refusal("stop_capability_refused", rec["feature"],
                                      rec["details"].get("message") or rec["reason_code"], False,
                                      reason_code=rec["reason_code"]))
    needs_labels = owner in STOP_OWNERS_NEEDING_LABEL
    verdict["needs_labels"] = needs_labels
    if needs_labels:
        verdict["labels"] = {"enabled": bool(regime.get("enabled")),
                             "period": int(regime.get("period", 14) or 14),
                             "adx_threshold": float(regime.get("adx_threshold", 20.0) or 20.0),
                             "windows_spec": spec, "timeframe": regime.get("timeframe")}
        if not regime.get("enabled"):
            refusals.append(_stop_refusal(
                "stop_capability_refused", owner,
                f"{owner} resolves its stop from a regime label, but the historical regime configuration is "
                "disabled; the comparison cannot establish the label live used", False,
                reason_code="MISSING_STOP_INPUT"))
    required = []
    if input_version < 2:
        required = ["basis"] + [f for f in OWNER_INPUT_FIELDS.get(owner, ()) if f != "trailing_stop_min_move_pct"]
        for field in required:
            refusals.append(_stop_refusal(
                "stop_inputs_unverified", field,
                f"comparison input version 1 has no raw or loader-resolved basis and no stop provenance, so it "
                f"cannot rule out an explicit stop, an implicit ATR default, a close-owned stop or the drawdown "
                f"fallback; read as raw configuration the owner is {owner!r}", True))
    else:
        evidence = seg["stop_evidence"]
        loader = basis == "loader_resolved"
        unverified = []

        def need(field, entry):
            required.append(field)
            if not _provenance_verified(entry):
                unverified.append(field)

        for key in STOP_FIELD_KEYS:
            need(key, evidence["raw_fields"].get(key))
            if loader:
                need(key, evidence["resolved_fields"].get(key))
        for field in OWNER_INPUT_FIELDS.get(owner, ()):
            if field in ("close_strategy", "leverage") or field in STOP_FIELD_KEYS:
                continue
            if field == "regime_atr_window":
                need(field, evidence["regime_atr_window"])
            elif loader:
                need(field, evidence["resolved_fields"].get(field))
            else:
                entry = evidence["raw_fields"].get(field)
                need(field, entry)
                if field == "max_drawdown_pct" and not (
                        isinstance(entry, dict) and entry["present"] and (_finite_number(entry["value"]) or 0) != 0):
                    need("platform_max_drawdown_pct", seg["stop_defaults"]["platform_max_drawdown_pct"])
        if not loader:
            for key in _default_requirements(seg, strategy_close_refs(strategy, label)):
                need(key, seg["stop_defaults"][key])
        for field in sorted(set(unverified)):
            refusals.append(_stop_refusal(
                "stop_inputs_unverified", field,
                f"the {basis} segment has no verified provenance for {field}, an input of the {owner!r} stop "
                "owner or of its precedence", True))
    verdict["required_inputs"] = sorted(set(required))
    kwargs["capability_context"] = preflight
    verdict["kwargs"] = kwargs
    verdict["status"] = "modeled" if not refusals else "refused"
    return verdict


def _stop_rows(verdict: dict, strategy: dict) -> list:
    named = {r["field"] for r in verdict["refusals"]}
    rows = []
    for field in STOP_ROLE_FIELDS:
        value = strategy.get(field)
        present = value is not None
        if not verdict["applicable"]:
            decision = "informational" if present else "inactive"
            reason = "no Hyperliquid perps stop model for this owner"
        elif field in named or (verdict["status"] != "modeled" and present):
            decision = "refused"
            reason = "the stop verdict refused this input; see eligibility.refusals"
        elif field == "regime_atr_window" and not verdict["needs_labels"]:
            decision = "informational" if present else "inactive"
            reason = "no regime-owned stop reads the regime ATR window"
        elif present or field in OWNER_INPUT_FIELDS.get(verdict["owner"], ()):
            decision = "modeled"
            reason = (f"verified historical stop input; the common engine arms owner {verdict['owner']!r} "
                      "with the central capability context")
        else:
            decision = "inactive"
            reason = "not configured and not an input of the resolved owner"
        rows.append({"field": field, "category": "protection", "present": field in strategy, "value": value,
                     "active": decision not in ("inactive", "informational"), "decision": decision,
                     "reason": reason, "approximable": False, "stop_owner": verdict["owner"]})
    return rows


def capability_matrix(seg: dict, capability_evidence: dict, market: dict, stop_verdict: dict) -> list:
    strategy = seg.get("strategy") or {}
    regime = seg.get("regime") or {}
    risk = seg.get("portfolio_risk") or {}
    controls_verified = _evidence_verified(capability_evidence.get("portfolio_controls"))
    rows = []

    def row(field, category, value, decision, reason, approximable=False):
        if field.startswith("regime."):
            present = field[len("regime."):] in regime
        elif field.startswith("portfolio_risk."):
            present = field[len("portfolio_risk."):] in risk
        else:
            present = field in strategy
        rows.append({"field": field, "category": category, "present": present,
                     "value": value, "active": decision not in ("inactive", "informational"),
                     "decision": decision, "reason": reason, "approximable": approximable})

    handled = set()
    stype = strategy.get("type")
    handled.add("type")
    if stype == "manual":
        row("type", "signal_model", stype, "refused",
            "manual owner: the simulator has no signal model for operator actions")
    elif stype != "perps":
        row("type", "signal_model", stype, "refused", f"strategy type {stype!r} is not Hyperliquid perps")
    else:
        row("type", "signal_model", stype, "modeled", "Hyperliquid perps through the open/close engine")
    handled.add("platform")
    platform = str(strategy.get("platform") or "").lower()
    row("platform", "venue", strategy.get("platform"), "modeled" if platform == "hyperliquid" else "refused",
        "Hyperliquid fees, lot size and minimum notional from the frozen market manifest"
        if platform == "hyperliquid" else "only Hyperliquid is supported")
    handled.add("args")
    args = strategy.get("args") or []
    open_ref = strategy.get("open_strategy") or {}
    handled.add("open_strategy")
    if stype == "perps":
        coin = args[1] if len(args) > 1 else None
        tf = args[2] if len(args) > 2 else None
        ok = coin == market["coin"] and tf == market["interval"]
        row("args", "identity", args, "modeled" if ok else "refused",
            "coin and timeframe match the frozen dataset" if ok else
            f"args coin/timeframe {coin!r}/{tf!r} disagree with dataset {market['coin']!r}/{market['interval']!r}")
        name = open_ref.get("name") or (args[0] if args else None)
        if args and open_ref.get("name") and open_ref.get("name") != args[0]:
            row("open_strategy", "signal_model", open_ref, "refused", "open_strategy name disagrees with args[0]")
        elif name in HIGHER_TIMEFRAME_STRATEGIES:
            row("open_strategy", "higher_timeframe", open_ref, "refused",
                f"{name} reads higher-timeframe candles that the frozen comparison does not prepare")
        elif name in market["funding_input_strategies"]:
            row("open_strategy", "observation", open_ref, "refused",
                f"{name} reads funding as an entry input; frozen funding attaches only as a cost")
        elif name not in market["registry"]:
            row("open_strategy", "signal_model", open_ref, "refused", f"{name!r} is not in the futures registry")
        else:
            needs_oi = name in market["observation_input_strategies"]
            row("open_strategy", "observation" if needs_oi else "signal_model", open_ref, "modeled",
                "registry strategy over frozen candles" + (" and the recorded open-interest series" if needs_oi else ""))
    else:
        row("args", "identity", args or None, "informational", "no signal model for this owner")
        row("open_strategy", "signal_model", open_ref or None, "informational", "no signal model for this owner")
    handled.add("close_strategy")
    row("close_strategy", "exit_model", strategy.get("close_strategy"),
        "modeled" if strategy.get("close_strategy") else ("refused" if stype == "perps" else "informational"),
        "validated by the central close-capability contract" if strategy.get("close_strategy") else
        "the execution-spec path needs the open/close engine; the plain signal path has no lot- or minimum-aware fill sites",
        approximable=not strategy.get("close_strategy"))
    for field in ("direction", "invert_signal", "allow_shorts", "atr_method"):
        handled.add(field)
        row(field, "signal_model", strategy.get(field), "modeled" if strategy.get(field) is not None else "inactive",
            "simulator input")
    for field in ("leverage", "sizing_leverage"):
        handled.add(field)
        v = strategy.get(field)
        if v is None or (_is_number(v) and v <= 1):
            row(field, "margin_liquidation" if field == "leverage" else "sizing", v, "inactive",
                "leverage at or below 1 has no margin, liquidation or sizing effect")
        else:
            row(field, "margin_liquidation" if field == "leverage" else "sizing", v, "refused",
                "the simulator does not model leverage, margin or liquidation")
    handled.add("margin_mode")
    lev = strategy.get("leverage")
    row("margin_mode", "margin_liquidation", strategy.get("margin_mode"),
        "inactive" if lev is None or (_is_number(lev) and lev <= 1) else "refused",
        "margin mode matters only with leverage above 1")
    handled.update(STOP_ROLE_FIELDS)
    rows.extend(_stop_rows(stop_verdict, strategy))
    handled.update(("allow_scale_in", "scale_in"))
    if strategy.get("allow_scale_in"):
        row("allow_scale_in", "execution_cost", True, "refused",
            "the execution-spec path does not model scale-in adds", approximable=True)
        row("scale_in", "execution_cost", strategy.get("scale_in"), "refused",
            "scale-in sizing runs only under flat costs", approximable=True)
    else:
        row("allow_scale_in", "execution_cost", strategy.get("allow_scale_in"), "inactive", "no scale-in")
        row("scale_in", "execution_cost", strategy.get("scale_in"),
            "refused" if _active(strategy.get("scale_in")) else "inactive",
            "a scale_in block without allow_scale_in" if _active(strategy.get("scale_in")) else "no scale-in")
    for field in PORTFOLIO_CONTROL_FIELDS:
        handled.add(field)
        v = strategy.get(field)
        if not _active(v):
            row(field, "portfolio_controls", v, "inactive", "control not configured")
        else:
            row(field, "portfolio_controls", v, "requires_evidence_verified" if controls_verified else "refused",
                "capability_evidence.portfolio_controls verifies the control did not act in the interval"
                if controls_verified else "an unmodeled control may have acted; no verified non-intervention evidence",
                approximable=True)
    handled.add("paused")
    row("paused", "portfolio_controls", strategy.get("paused"), "refused" if strategy.get("paused") else "inactive",
        "a paused strategy holds position-increasing signals")
    for field in REGIME_FIELDS:
        handled.add(field)
        v = strategy.get(field)
        row(field, "regime", v, "refused" if _active(v) else "inactive",
            "regime gating is not modeled by this comparison" if _active(v) else "not configured")
    for field, category in UNSUPPORTED_FEATURE_FIELDS.items():
        handled.add(field)
        v = strategy.get(field)
        enabled = v.get("enabled", True) if isinstance(v, dict) and field == "hurst_gate" else _active(v)
        row(field, category, v, "refused" if enabled and _active(v) else "inactive",
            f"{category} is not modeled" if enabled and _active(v) else "not configured")
    for field, category in INFORMATIONAL_FIELDS.items():
        handled.add(field)
        if field in strategy:
            row(field, category, strategy.get(field), "informational", "no simulated behavior")
    for field in sorted(set(strategy) - handled):
        v = strategy[field]
        row(field, "unknown", v, "refused" if _active(v) else "inactive",
            "unknown active field: unknown behavior prevents strict eligibility" if _active(v) else "unknown field, inactive")
    needs_labels = bool(stop_verdict.get("needs_labels"))
    if not regime.get("enabled"):
        row("regime.enabled", "regime", regime.get("enabled"), "inactive", "regime disabled")
    elif needs_labels:
        row("regime.enabled", "regime", True, "modeled",
            "label input only: the primary-window classification supplies the regime-owned stop label; "
            "no entry gating is added")
    else:
        row("regime.enabled", "regime", True, "refused",
            "regime classification is modeled only as the label input of a regime-owned stop")
    timeframe = str(regime.get("timeframe") or "").strip().lower()
    if timeframe:
        differs = timeframe != str(market.get("interval") or "").strip().lower()
        row("regime.timeframe", "regime", regime.get("timeframe"),
            "refused" if differs and needs_labels else "informational",
            "regime labels from another timeframe are not prepared by the frozen comparison"
            if differs and needs_labels else "labels come from the frozen dataset interval")
    for field in sorted(risk):
        v = risk[field]
        if field == "warn_threshold_pct":
            row(f"portfolio_risk.{field}", "notification", v, "informational", "warning threshold only")
        elif not _active(v):
            row(f"portfolio_risk.{field}", "portfolio_controls", v, "inactive", "control not configured")
        else:
            row(f"portfolio_risk.{field}", "portfolio_controls", v,
                "requires_evidence_verified" if controls_verified else "refused",
                "capability_evidence.portfolio_controls verifies the control did not act in the interval"
                if controls_verified else "an unmodeled portfolio control may have acted; no verified non-intervention evidence",
                approximable=True)
    return rows


def _signal_actions(frame: pd.DataFrame, direction: str, invert: bool) -> pd.Series:
    if "open_action" in frame.columns:
        raw = frame["open_action"].map(_normalize_open_action)
        sig = raw.map(lambda a: 1 if a == "long" else (-1 if a == "short" else 0))
    else:
        sig = frame["signal"].fillna(0).astype(float).round().astype(int)
    return sig.map(lambda s: _open_action_from_signal(
        _apply_direction_invert_value(int(s), True, direction, invert)))


def _signal_columns(frame: pd.DataFrame) -> list:
    cols = [c for c in ("signal", "open_action", "entry_fraction") if c in frame.columns]
    return cols + _close_fraction_columns(frame)


def _frames_equal(a: pd.DataFrame, b: pd.DataFrame, cols: list) -> bool:
    for c in cols:
        x, y = a[c], b[c]
        if not (pd.api.types.is_numeric_dtype(x) and pd.api.types.is_numeric_dtype(y)):
            if not x.astype(str).equals(y.astype(str)):
                return False
        elif not np.array_equal(x.to_numpy(dtype=float), y.to_numpy(dtype=float), equal_nan=True):
            return False
    return True


def prepare_market(manifest_path: str, manifest_sha: str, dataset_key: str, window_name: str,
                   start: pd.Timestamp, end: pd.Timestamp) -> dict:
    out = {"status": "verified", "refusals": [], "checks": {}}
    try:
        with open(manifest_path, "rb") as fh:
            raw = fh.read()
    except OSError as exc:
        return {"status": "refused", "refusals": [{"reason": "market_manifest_unreadable", "detail": str(exc)}],
                "checks": {}}
    out["manifest_sha256"] = _sha256(raw)
    if out["manifest_sha256"] != manifest_sha:
        out["status"] = "refused"
        out["refusals"].append({"reason": "market_manifest_hash_mismatch",
                                "detail": f"manifest sha256 {out['manifest_sha256']} != bound {manifest_sha}"})
        return out
    try:
        manifest = om.load_manifest(manifest_path, verify=True)
        dataset = om.dataset_by_key(manifest, dataset_key)
        frame, win, cov = om.window_frame(manifest, dataset, window_name)
    except om.ManifestError as exc:
        out["status"] = "refused"
        out["refusals"].append({"reason": "market_input_integrity", "detail": str(exc)})
        return out
    out.update({"manifest": manifest, "dataset": dataset, "frame": frame, "window": win,
                "candle_coverage": cov})
    out["provenance"] = {
        "manifest_sha256": out["manifest_sha256"],
        "provenance": manifest["provenance"],
        "dataset": dataset["key"],
        "candles_sha256": dataset["candles"]["sha256"],
        "funding_sha256": (dataset["funding"] or {}).get("sha256"),
        "open_interest_sha256": (dataset.get("open_interest") or {}).get("sha256"),
        "venue_meta_sha256": manifest["venue_reference"]["meta"]["sha256"],
    }
    step = manifest["interval_ms"]
    win_start = pd.Timestamp(win["start_ms"], unit="ms", tz="UTC")
    win_end = pd.Timestamp(win["end_ms"], unit="ms", tz="UTC")
    checks = out["checks"]
    checks["window_matches_interval"] = {"ok": bool(win_start == start and win_end == end),
                                         "window_start": _iso(win_start), "window_end": _iso(win_end)}
    ts = frame["timestamp"].to_numpy(dtype=np.int64)
    expected_total = manifest["warmup_bars"] + (win["end_ms"] - win["start_ms"]) // step
    grid_ok = False
    if len(ts) == expected_total and len(ts) > manifest["warmup_bars"]:
        grid_ok = (bool((np.diff(ts) == step).all()) and int(ts[-1]) == win["end_ms"] - step
                   and int(ts[manifest["warmup_bars"]]) == win["start_ms"])
    checks["candle_grid"] = {
        "ok": bool(grid_ok and cov["complete"] and cov["warmup_missing_bars"] == 0),
        "bars": int(len(ts)), "expected_bars": int(expected_total), "coverage": cov,
        "rule": "warm-up plus scoring bars form one exact interval grid with no gap at the warm-up join or either edge",
    }
    o, h, l, c, v = (frame[k].to_numpy(dtype=float) for k in ("open", "high", "low", "close", "volume"))
    finite = bool(np.isfinite(o).all() and np.isfinite(h).all() and np.isfinite(l).all()
                  and np.isfinite(c).all() and np.isfinite(v).all())
    valid = finite and bool((o > 0).all() and (c > 0).all() and (l > 0).all()
                            and (h >= np.maximum(o, c)).all() and (l <= np.minimum(o, c)).all() and (v >= 0).all())
    checks["candle_values"] = {"ok": valid, "rule": "finite positive OHLC with high/low bracketing open/close, volume >= 0"}
    funding = om.load_funding(dataset)
    attach_start = win["start_ms"] - step + om.HOUR_MS
    attach_end = win["end_ms"] - step + om.HOUR_MS
    fcov = om.funding_coverage(funding, attach_start, attach_end)
    checks["funding"] = {"ok": bool(fcov["complete"]), "coverage": fcov,
                         "attachment_span": [_iso(pd.Timestamp(attach_start, unit="ms", tz="UTC")),
                                             _iso(pd.Timestamp(attach_end, unit="ms", tz="UTC"))],
                         "rule": "hourly funding is present for every hour the simulator attaches to a scored bar"}
    for name, chk in checks.items():
        if not chk["ok"]:
            out["status"] = "refused"
            out["refusals"].append({"reason": f"market_{name}_incomplete", "detail": chk.get("rule")})
    return out


def _with_stop_inputs(frame: pd.DataFrame, atr: bool, atr_method: str, labels: Optional[dict]) -> pd.DataFrame:
    from atr import ensure_atr_indicator
    frame = frame.copy()
    if atr:
        frame = ensure_atr_indicator(frame, method=atr_method)
    if labels is not None:
        from regime import ensure_regime_columns
        ensure_regime_columns(frame, period=labels["period"], adx_threshold=labels["adx_threshold"],
                              windows_spec=labels["windows_spec"])
    return frame


def market_strategy_checks(market: dict, name: str, params: dict, direction: str, invert: bool,
                           needs_oi: bool, has_close: bool, atr_method: str = "simple",
                           stop_needs_atr: bool = False, labels: Optional[dict] = None) -> dict:
    from registry_loader import load_registry
    reg = load_registry("futures")
    manifest, dataset, win = market["manifest"], market["dataset"], market["window"]
    frame, _ = om.attach_funding_cost(market["frame"], dataset, win)
    obs_params = {}
    out = {"refusals": [], "checks": {}}
    oi_cov = None
    if needs_oi:
        oi, oi_cov = om.attach_open_interest(manifest, dataset, win)
        if oi is not None:
            obs_params = {"open_interest_observations": oi}
    p = {**(params or {}), **obs_params}
    signals = reg.apply_strategy(name, frame, p)
    if needs_oi:
        column = OBSERVATION_VALIDITY_COLUMNS.get(name)
        scored = om.slice_window(signals, win)
        if column is None or column not in scored.columns:
            invalid = None
        else:
            invalid = [_iso(pd.Timestamp(int(t), unit="ms", tz="UTC"))
                       for t, ok in zip(scored["timestamp"], scored[column]) if not bool(ok)]
        complete = invalid == []
        out["checks"]["observations"] = {
            "ok": complete, "coverage": oi_cov, "validity_column": column,
            "invalid_decision_bars": invalid if invalid is None else invalid[:50],
            "invalid_decision_bar_count": None if invalid is None else len(invalid),
            "rule": "every scored decision bar has fresh observations with enough lookback coverage and no "
                    "gap, as the strategy's own per-bar observation check reports",
        }
        if not complete:
            out["refusals"].append({"reason": "market_observations_incomplete",
                                    "detail": "recorded observations are stale, gapped or missing at a decision cutoff"
                                    if invalid else "the strategy reports no per-bar observation validity"})
    with_atr = has_close or stop_needs_atr
    signals = _with_stop_inputs(signals, with_atr, atr_method, labels)
    warm = manifest["warmup_bars"]
    trim = warm // 3
    cols = _signal_columns(signals)
    if stop_needs_atr:
        cols.append("atr")
    if labels is not None:
        cols.append("regime")
    stable = True
    if trim > 0:
        trimmed = _with_stop_inputs(reg.apply_strategy(name, frame.iloc[trim:].copy(), p), with_atr,
                                    atr_method, labels)
        scored_full = om.slice_window(signals, win)
        scored_trim = om.slice_window(trimmed, win)
        stable = len(scored_full) == len(scored_trim) and _frames_equal(scored_full, scored_trim, cols)
    out["checks"]["indicator_history"] = {
        "ok": bool(stable), "trimmed_warmup_bars": trim, "compared_columns": cols,
        "rule": "scored-window decisions, and the ATR and regime labels a stop owner reads, are identical "
                "when the first third of warm-up is removed",
    }
    if not stable:
        out["refusals"].append({"reason": "market_indicator_history_insufficient",
                                "detail": "scored decisions depend on how much warm-up history is supplied"})
    pre = signals.loc[signals["timestamp"] < win["start_ms"]]
    pending = None
    if len(pre):
        pending = str(_signal_actions(pre.iloc[[-1]], direction, invert).iloc[0])
    out["pending_decision"] = pending
    out["signals"] = signals
    return out


def run_simulation(market: dict, signals: pd.DataFrame, plan: dict) -> dict:
    manifest, dataset, win = market["manifest"], market["dataset"], market["window"]
    spec = om.execution_spec(manifest, dataset)
    scored = om.slice_window(signals, win)
    kwargs = dict(
        initial_capital=plan["initial_cash"], platform="hyperliquid", strategy_type="perps",
        open_strategy={"name": plan["open_name"], "params": dict(plan["params"])},
        close_strategies=plan["close_refs"], direction=plan["direction"],
        invert_signal=plan["invert_signal"], atr_method=plan["atr_method"],
        comparison_mode=plan["comparison_mode"],
    )
    if plan["execution_spec"]:
        kwargs["execution_spec"] = spec
        cost_model = {"kind": "market_manifest_execution_spec", "spec": spec}
    else:
        kwargs["commission_pct"] = spec["taker_fee_pct"]
        kwargs["slippage_pct"] = spec["half_spread_pct"] + spec["slippage_pct"]
        cost_model = {"kind": "flat_taker_fee_and_adverse_price",
                      "commission_pct": kwargs["commission_pct"], "slippage_pct": kwargs["slippage_pct"]}
    stop = plan.get("stop") or {}
    if stop.get("kwargs"):
        kwargs.update(stop["kwargs"])
        if stop.get("needs_labels"):
            labels = stop["labels"]
            kwargs.update(regime_enabled=True, regime_period=labels["period"],
                          regime_adx_threshold=labels["adx_threshold"],
                          regime_windows_spec=labels["windows_spec"])
    if plan.get("allow_scale_in"):
        kwargs["allow_scale_in"] = True
        kwargs["scale_in"] = plan.get("scale_in")
    stop_events: list = []
    bt = Backtester(**kwargs)
    res = bt.run(scored, strategy_name=plan["open_name"], symbol=dataset["symbol"],
                 timeframe=manifest["interval"], params=plan["params"], save=False,
                 indicator_frame=signals, record_events=True, stop_observer=stop_events.append)
    env = res["ledger_events"]
    if env.get("schema") != LEDGER_EVENTS_SCHEMA or env.get("schema_version") != LEDGER_EVENTS_SCHEMA_VERSION:
        raise RuntimeError("simulator event envelope has an unexpected schema")
    return {"envelope": env, "close_validation": res.get("close_validation"),
            "execution": res.get("execution"), "cost_model": cost_model,
            "interval_ms": manifest["interval_ms"], "stop_events": stop_events,
            "stop_owner": bt._stop_owner,
            "stop_warmup_skipped_entries": res.get("stop_warmup_skipped_entries", 0)}


def normalize_simulated(env: dict) -> dict:
    positions = {}
    funding = []
    synthetic = []
    dispositions = {}
    for e in env["events"]:
        if e["kind"] == "funding":
            funding.append(e)
            dispositions[e["event_id"]] = "strategy_funding"
        elif e["kind"] in ("terminal_liquidation", "seed_inventory"):
            synthetic.append(e)
            dispositions[e["event_id"]] = "synthetic"
        else:
            p = positions.setdefault(e["position_local_id"], {"position_local_id": e["position_local_id"],
                                                               "side": e["side"], "_events": []})
            p["_events"].append(e)
    end = env["interval_end"] or {}
    out = []
    for pid in sorted(positions):
        p = positions[pid]
        evs = p["_events"]
        opens = [e for e in evs if e["kind"] in ("open", "scale_in")]
        closes = [e for e in evs if e["kind"] == "close"]
        residual = abs(end.get("position_qty") or 0.0) if end.get("position_local_id") == pid else 0.0
        p.update({
            "event_ids": [e["event_id"] for e in evs],
            "opened_qty": _fsum(e["quantity"] for e in opens),
            "closed_qty": _fsum(e["quantity"] for e in closes),
            "residual_qty": residual,
            "entry_fees": _fsum(e["fee_charged"] for e in opens),
            "exit_fees": _fsum(e["fee_charged"] for e in closes),
            "gross_realized": _fsum(e["gross_realized"] or 0.0 for e in closes),
            "net": _fsum(e["gross_realized"] or 0.0 for e in closes) - _fsum(e["fee_charged"] for e in evs),
            "entry_fee_allocated": _fsum(e["entry_fee_allocated"] or 0.0 for e in closes),
            "entry_fee_outstanding": (end.get("entry_fee_outstanding") or 0.0) if end.get("position_local_id") == pid else 0.0,
        })
        out.append(p)
    return {"positions": out, "funding": funding, "synthetic": synthetic, "dispositions": dispositions,
            "interval_end": end}


def _time_window(sim_event: dict, step_s: float, tol_s: float):
    bar = _utc(sim_event["bar_timestamp"], "simulated bar")
    width = step_s if sim_event["timing"] == "intrabar_trigger_fill" else 0.0
    return bar, width, tol_s


def _component_compat(b: dict, s: dict, step_s: float, tol: dict) -> dict:
    bar, width, tol_s = _time_window(s, step_s, tol["time_seconds"])
    dt = (b["ts"] - bar).total_seconds()
    time_ok = -tol_s <= dt <= width + tol_s
    rel = abs(b["price"] - s["effective_price"]) / s["effective_price"] if s["effective_price"] else math.inf
    price_ok = rel <= tol["price_relative"]
    qty_delta = b["quantity"] - s["quantity"]
    qty_ok = abs(qty_delta) <= tol["quantity_absolute"]
    fee_delta = b["fee"] - s["fee_charged"]
    return {
        "booked_event_key": b["event_key"], "simulated_event_id": s["event_id"],
        "booked_timestamp": b["timestamp"], "simulated_bar_timestamp": s["bar_timestamp"],
        "simulated_timing": s["timing"], "time_delta_seconds": dt, "time_ok": time_ok,
        "price_booked": b["price"], "price_simulated": s["effective_price"], "price_relative_delta": rel,
        "price_ok": price_ok, "quantity_delta": qty_delta, "quantity_ok": qty_ok,
        "fee_booked": b["fee"], "fee_simulated": s["fee_charged"], "fee_delta": fee_delta,
        "fee_ok": abs(fee_delta) <= tol["money_absolute"],
        "within_tolerance": bool(time_ok and price_ok and qty_ok and abs(fee_delta) <= tol["money_absolute"]),
    }


def _booked_opening(bp: dict) -> Optional[dict]:
    opens = [e for e in bp["_events"] if e["relation"] == "in" and e["kind"] != "close"]
    return opens[0] if opens else None


def match_positions(booked: dict, simulated: dict, step_s: float, tol: dict) -> dict:
    bps = [p for p in booked["positions"] if p["status"] == "comparable"]
    sps = simulated["positions"]
    edges = {}
    weak = []
    for bi, bp in enumerate(bps):
        b_open = _booked_opening(bp)
        for si, sp in enumerate(sps):
            if bp["ownership"]["side"] != sp["side"] or b_open is None:
                continue
            s_open = next(e for e in sp["_events"] if e["kind"] in ("open", "scale_in"))
            comp = _component_compat(b_open, s_open, step_s, tol)
            if comp["time_ok"] and comp["price_ok"] and comp["quantity_ok"]:
                edges[(bi, si)] = comp
            elif comp["time_ok"] or comp["price_ok"]:
                weak.append({"booked_position_id": bp["position_id"], "simulated_position_id": sp["position_local_id"],
                             "evidence": comp, "support": "weak"})
    adj_b, adj_s = {}, {}
    for (bi, si) in edges:
        adj_b.setdefault(bi, set()).add(si)
        adj_s.setdefault(si, set()).add(bi)
    seen_b, seen_s = set(), set()
    assigned, ambiguous = [], []
    for start in sorted(adj_b):
        if start in seen_b:
            continue
        comp_b, comp_s, stack = set(), set(), [("b", start)]
        while stack:
            side, n = stack.pop()
            if side == "b" and n not in comp_b:
                comp_b.add(n)
                stack.extend(("s", m) for m in adj_b.get(n, ()))
            elif side == "s" and n not in comp_s:
                comp_s.add(n)
                stack.extend(("b", m) for m in adj_s.get(n, ()))
        seen_b |= comp_b
        seen_s |= comp_s
        comp_edges = sorted(e for e in edges if e[0] in comp_b)
        if len(comp_edges) > 16:
            ambiguous.append({"booked": sorted(comp_b), "simulated": sorted(comp_s), "reason": "component_too_large",
                              "edges": comp_edges})
            continue
        best, best_sets = 0, []
        for r in range(min(len(comp_b), len(comp_s)), 0, -1):
            for combo in itertools.combinations(comp_edges, r):
                if len({e[0] for e in combo}) == r and len({e[1] for e in combo}) == r:
                    best_sets.append(combo)
            if best_sets:
                best = r
                break
        if len(best_sets) == 1:
            assigned.extend(best_sets[0])
        else:
            ambiguous.append({"booked": sorted(comp_b), "simulated": sorted(comp_s),
                              "reason": "equally_supported_assignments", "maximum_matching_size": best,
                              "alternatives": len(best_sets), "edges": comp_edges})
    return {"bps": bps, "sps": sps, "edges": edges, "assigned": assigned, "ambiguous": ambiguous, "weak": weak}


def reconcile_pair(bp: dict, sp: dict, step_s: float, tol: dict) -> dict:
    b_in = [e for e in bp["_events"] if e["relation"] == "in"]
    out = {"booked_position_id": bp["position_id"], "simulated_position_id": sp["position_local_id"],
           "components": [], "unmatched_booked_components": [], "unmatched_simulated_components": []}
    for kinds_b, kinds_s in ((("non_close", "scale_in"), ("open", "scale_in")), (("close",), ("close",))):
        bs = [e for e in b_in if e["kind"] in kinds_b]
        ss = [e for e in sp["_events"] if e["kind"] in kinds_s]
        for b, s in zip(bs, ss):
            out["components"].append(_component_compat(b, s, step_s, tol))
        out["unmatched_booked_components"].extend(b["event_key"] for b in bs[len(ss):])
        out["unmatched_simulated_components"].extend(s["event_id"] for s in ss[len(bs):])
    b_open_vwap = (_fsum(e["quantity"] * e["price"] for e in b_in if e["kind"] != "close") / bp["opened_qty"]
                   if bp["opened_qty"] else None)
    s_opens = [e for e in sp["_events"] if e["kind"] in ("open", "scale_in")]
    s_open_vwap = (_fsum(e["quantity"] * e["effective_price"] for e in s_opens) / sp["opened_qty"]
                   if sp["opened_qty"] else None)
    deltas = {
        "opened_qty": bp["opened_qty"] - sp["opened_qty"],
        "closed_qty": bp["closed_qty"] - sp["closed_qty"],
        "residual_qty": bp["residual_qty"] - sp["residual_qty"],
        "entry_fees": bp["entry_fees"] - sp["entry_fees"],
        "exit_fees": bp["exit_fees"] - sp["exit_fees"],
        "net": bp["ledger_delta"] - sp["net"],
        "entry_vwap_relative": (abs(b_open_vwap - s_open_vwap) / s_open_vwap
                                if b_open_vwap is not None and s_open_vwap else None),
    }
    qty_tol, money_tol = tol["quantity_absolute"], tol["money_absolute"]
    ok = (all(c["within_tolerance"] for c in out["components"])
          and not out["unmatched_booked_components"] and not out["unmatched_simulated_components"]
          and abs(deltas["opened_qty"]) <= qty_tol and abs(deltas["closed_qty"]) <= qty_tol
          and abs(deltas["residual_qty"]) <= qty_tol and abs(deltas["entry_fees"]) <= money_tol
          and abs(deltas["exit_fees"]) <= money_tol and abs(deltas["net"]) <= money_tol)
    out.update({
        "booked": {k: bp[k] for k in ("opened_qty", "closed_qty", "residual_qty", "entry_fees", "exit_fees",
                                      "ledger_delta", "row_net_pnl", "legacy_rows", "crosses_interval_end",
                                      "crosses_interval_start", "direction_evidence")},
        "simulated": {k: sp[k] for k in ("opened_qty", "closed_qty", "residual_qty", "entry_fees", "exit_fees",
                                         "gross_realized", "net", "entry_fee_allocated", "entry_fee_outstanding")},
        "deltas": deltas,
        "within_tolerance": bool(ok),
        "cause": None,
    })
    return out


def _arm_timestamp(value) -> Optional[str]:
    try:
        ts = pd.Timestamp(value)
    except (ValueError, TypeError):
        return None
    ts = ts.tz_localize("UTC") if ts.tz is None else ts.tz_convert("UTC")
    return _iso(ts)


def map_first_arms(simulated: dict, stop_events: list) -> dict:
    arms = [e for e in stop_events if e.get("event") == "arm"]
    opens = []
    for sp in simulated["positions"]:
        first = sp["_events"][0] if sp["_events"] else None
        if first is not None and first["kind"] == "open":
            opens.append((sp["position_local_id"], first))
    opens.sort(key=lambda o: o[1]["seq"])
    keys = {}
    for _, ev in opens:
        k = (ev["bar_timestamp"], ev["side"])
        keys[k] = keys.get(k, 0) + 1
    mapped, unmapped = {}, {}
    if len(arms) != len(opens):
        for pid, _ in opens:
            unmapped[pid] = (f"{len(arms)} first-arm events for {len(opens)} simulated openings; the arm "
                             "sequence cannot be mapped one to one")
        return {"mapped": mapped, "unmapped": unmapped, "arm_events": len(arms)}
    for (pid, ev), arm in zip(opens, arms):
        if keys[(ev["bar_timestamp"], ev["side"])] > 1:
            unmapped[pid] = "more than one simulated opening shares this bar and side"
        elif _arm_timestamp(arm.get("date")) != ev["bar_timestamp"] or arm.get("side") != ev["side"]:
            unmapped[pid] = "the arm in sequence does not have the opening's bar time and side"
        else:
            mapped[pid] = arm
    return {"mapped": mapped, "unmapped": unmapped, "arm_events": len(arms)}


def _stamp_value(item: dict):
    return item["value"] if item["status"] == "available" else None


def compare_initial_stop_geometry(pairs: list, arm_map: dict, attestations: dict, tol: dict) -> dict:
    results = []
    used = set()
    for bp, sp in pairs:
        opening = _booked_opening(bp)
        initial = (opening is not None and opening["kind"] == "non_close"
                   and not any(e["relation"] == "before" for e in bp["_events"]))
        keys = {e["event_key"] for e in bp["_events"]}
        attested = sorted(k for k in attestations if k in keys and _provenance_verified(attestations[k]))
        used.update(k for k in attestations if k in keys)
        b_trigger = _stamp_value(opening["_stop_trigger"]) if opening is not None else None
        later = [e["event_key"] for e in bp["_events"]
                 if e is not opening and _stamp_value(e["_stop_trigger"]) is not None]
        arm = arm_map["mapped"].get(sp["position_local_id"])
        res = {
            "booked_position_id": bp["position_id"], "simulated_position_id": sp["position_local_id"],
            "booked_event_key": opening["event_key"] if opening is not None else None,
            "booked_trigger": b_trigger,
            "booked_entry_atr": _stamp_value(opening["_entry_atr"]) if opening is not None else None,
            "simulated_trigger": arm.get("trigger") if arm else None,
            "simulated_entry_atr": arm.get("entry_atr") if arm else None,
            "simulated_geometry": arm.get("geometry") if arm else None,
            "simulated_owner": arm.get("owner") if arm else None,
            "later_stamps": later, "attested_event_keys": attested, "relative_delta": None,
        }
        foreign = [k for k in attested if opening is None or k != opening["event_key"]]
        if not initial:
            if b_trigger is not None or later or attested:
                res.update(status="unverified", reason="the booked opening inside the interval is not the "
                                                       "position's initial entry; a later stamp cannot establish "
                                                       "the first arm")
            else:
                res.update(status="unavailable", reason="no initial-entry row inside the interval")
        elif foreign:
            res.update(status="unverified", reason=f"attested rows {foreign} are not the initial-entry row; a close "
                                                   "or scale-in stamp can carry later geometry")
        elif b_trigger is None:
            res.update(status="unavailable", reason="the initial-entry row has no recorded stop trigger")
        elif not attested:
            res.update(status="unverified", reason="the initial-entry stop trigger has no verified "
                                                   "initial_stop_geometry_evidence entry")
        elif arm is None:
            res.update(status="unverified", reason=arm_map["unmapped"].get(
                sp["position_local_id"], "no simulated first arm maps to this position"))
        else:
            s_trigger = float(arm.get("trigger") or 0.0)
            rel = abs(b_trigger - s_trigger) / s_trigger if s_trigger > 0 else math.inf
            ok = rel <= tol["price_relative"]
            res.update(status="agreement" if ok else "mismatch",
                       relative_delta=rel if math.isfinite(rel) else None,
                       reason=None if ok else "booked initial stop trigger differs from the simulated first arm "
                                              "beyond the price tolerance")
        results.append(res)
    counts = {status: sum(1 for r in results if r["status"] == status) for status in GEOMETRY_STATUSES}
    return {
        "rule": "the booked initial-entry stop_loss_trigger_px, with verified initial-entry provenance, is "
                "compared with the simulated first arm mapped by sequence, bar time and side; missing geometry "
                "is unavailable and is never agreement",
        "positions": results, "counts": counts,
        "unused_attestations": sorted(k for k in attestations if k not in used),
        "arm_mapping": {"arm_events": arm_map["arm_events"], "unmapped": arm_map["unmapped"]},
    }


BOOKED_SECTION_DISPOSITIONS = {
    "matched_components": ("matched",),
    "matched_pair_unmatched_components": ("unmatched",),
    "ambiguous": ("ambiguous",),
    "unmatched_booked": ("unmatched", "not_simulated"),
    "strategy_funding": ("strategy_funding",),
    "unresolved": ("unresolved_identity",),
    "outside_interval": ("outside_interval",),
    "not_comparable": ("not_comparable",),
}


def _expected_row_accounting(r: dict) -> tuple:
    fee, rp = r["fee"], r["realized_pnl"]
    if r["pnl_gross"]:
        return rp - fee, rp - fee
    if r["is_close"]:
        return rp, rp
    return -fee, rp


def conservation(booked_doc: dict, norm: dict, sim: Optional[dict], sim_env: Optional[dict],
                 booked_dispositions: dict, sim_dispositions: dict, tol: dict, booked_sections: dict,
                 matched: list) -> dict:
    checks = []
    informational = []

    def check(name, ok, **detail):
        checks.append({"check": name, "ok": bool(ok), **detail})

    records = {r["event_key"]: r for r in norm["records"]}
    all_keys = [e["event_key"] for e in booked_doc["events"]]
    check("booked_each_event_one_disposition", set(booked_dispositions) == set(all_keys)
          and len(booked_dispositions) == len(all_keys), events=len(all_keys),
          dispositions=len(booked_dispositions))

    bad_rows = []
    for r in norm["records"]:
        delta, net = _expected_row_accounting(r)
        b = 4 * sys.float_info.epsilon * max(1.0, abs(r["realized_pnl"]) + abs(r["fee"]))
        if abs(r["ledger_delta"] - delta) > b or abs(r["row_net_pnl"] - net) > b:
            bad_rows.append({"event_key": r["event_key"], "ledger_delta": r["ledger_delta"],
                             "expected_ledger_delta": delta, "row_net_pnl": r["row_net_pnl"],
                             "expected_row_net_pnl": net})
    check("booked_row_accounting_identity", not bad_rows, rows=len(norm["records"]),
          mismatched_count=len(bad_rows), mismatched=bad_rows[:50],
          rule="each row's ledger_delta and row_net_pnl equal the exporter formulas recomputed from "
               "realized_pnl, exchange_fee, is_close and pnl_gross")

    listed = {}
    disagree = []
    unknown = []
    for section in sorted(booked_sections):
        for k in booked_sections[section]:
            listed[k] = listed.get(k, 0) + 1
            if k not in records:
                unknown.append({"section": section, "event_key": k})
            elif booked_dispositions.get(k) not in BOOKED_SECTION_DISPOSITIONS[section]:
                disagree.append({"section": section, "event_key": k,
                                 "disposition": booked_dispositions.get(k)})
    missing = sorted(set(all_keys) - set(listed))
    duplicated = sorted(k for k, n in listed.items() if n > 1)
    check("booked_report_sections_partition_events", not (missing or duplicated or unknown or disagree),
          missing=missing, duplicated=duplicated, unknown=unknown, disposition_disagreements=disagree,
          rule="every booked event is listed in exactly one report section, and that section agrees with "
               "its disposition")
    total_fees = [r["fee"] for r in norm["records"]]
    total_deltas = [r["ledger_delta"] for r in norm["records"]]
    section_fees = [records[k]["fee"] for keys in booked_sections.values() for k in keys if k in records]
    section_deltas = [records[k]["ledger_delta"] for keys in booked_sections.values() for k in keys if k in records]
    bound = _sum_bound(total_fees + section_fees) + _sum_bound(total_deltas + section_deltas) + 1e-12
    by_section = {sec: {"fees": _fsum(records[k]["fee"] for k in keys if k in records),
                        "ledger_delta": _fsum(records[k]["ledger_delta"] for k in keys if k in records)}
                  for sec, keys in sorted(booked_sections.items())}
    check("booked_fees_counted_once", abs(_fsum(total_fees) - _fsum(section_fees)) <= bound,
          total=_fsum(total_fees), report_sections_total=_fsum(section_fees), by_section=by_section,
          numerical_bound=bound)
    check("booked_ledger_delta_conserved", abs(_fsum(total_deltas) - _fsum(section_deltas)) <= bound,
          total=_fsum(total_deltas), report_sections_total=_fsum(section_deltas), by_section=by_section,
          numerical_bound=bound)

    for pair in matched:
        keys = [c["booked_event_key"] for c in pair["components"]] + list(pair["unmatched_booked_components"])
        fees = [records[k]["fee"] for k in keys]
        deltas = [records[k]["ledger_delta"] for k in keys]
        pos_fees = pair["booked"]["entry_fees"] + pair["booked"]["exit_fees"]
        pos_delta = pair["booked"]["ledger_delta"]
        b = _sum_bound(fees + [pos_fees]) + _sum_bound(deltas + [pos_delta]) + 1e-12
        check(f"booked_matched_pair_components:{pair['booked_position_id']}",
              len(keys) == len(set(keys)) and abs(_fsum(fees) - pos_fees) <= b and abs(_fsum(deltas) - pos_delta) <= b,
              components_fees=_fsum(fees), position_fees=pos_fees, components_ledger_delta=_fsum(deltas),
              position_ledger_delta=pos_delta, numerical_bound=b,
              rule="the pair's compared and leftover booked components add up to the position's in-interval totals")

    qty_tol = tol["quantity_absolute"]
    for p in norm["positions"]:
        running, low = 0.0, 0.0
        for _, group in itertools.groupby(p["_events"], key=lambda e: e["ts"]):
            group = list(group)
            running += (_fsum(e["quantity"] for e in group if e["kind"] != "close")
                        - _fsum(e["quantity"] for e in group if e["kind"] == "close"))
            low = min(low, running)
        check(f"booked_position_inventory_never_negative:{p['position_id']}",
              low >= -qty_tol and p["residual_qty"] >= -qty_tol, min_running_inventory=low,
              residual_at_interval_end=p["residual_qty"], lifetime_residual=running,
              rule="in record-time order (same-time rows applied together), a position never closes more "
                   "than it has opened")

    funding_total = _fsum(r["realized_pnl"] for r in norm["funding_in"])
    informational.append({"total": "booked_strategy_funding_unallocated", "strategy_funding": funding_total,
                          "allocated_to_positions": 0.0, "unallocated": funding_total,
                          "rows_with_position_id": sum(1 for r in norm["funding_in"] if r["position_id"]),
                          "rule": "strategy funding is compared only as a strategy total; this is a total, "
                                  "not a check"})
    wallet = booked_doc["wallet_orphan_context"]
    in_totals = any(rec.get("event_key") in booked_dispositions for rec in wallet["records"])
    check("wallet_orphan_excluded_from_strategy_totals", not in_totals,
          wallet_orphan_records=len(wallet["records"]),
          wallet_orphan_total=_fsum(r["amount_usd"] for r in wallet["records"]))
    if sim_env is not None:
        evs = sim_env["events"]
        cash, qty = sim_env["initial_cash"], 0.0
        breaks = 0
        for e in evs:
            if abs(e["cash_before"] - cash) > 1e-9 * max(1.0, abs(cash)) or abs(e["qty_before"] - qty) > 1e-12:
                breaks += 1
            cash, qty = e["cash_after"], e["qty_after"]
        check("simulated_cash_and_quantity_continuity", breaks == 0, breaks=breaks)
        pre = [e for e in evs if e["kind"] != "terminal_liquidation"]
        end = sim_env["interval_end"]
        gross = [e["gross_realized"] or 0.0 for e in pre]
        fees_s = [e["fee_charged"] for e in pre]
        fund = [e["funding_cash"] or 0.0 for e in pre]
        lhs = sim_env["initial_cash"] + _fsum(gross) - _fsum(fees_s) + _fsum(fund)
        rhs = end["cash"] + end["position_qty"] * (end["avg_cost"] or 0.0)
        b = _sum_bound(gross + fees_s + fund) + 1e-9 * max(1.0, abs(lhs))
        check("simulated_cash_identity", abs(lhs - rhs) <= b, initial_plus_realized_minus_fees_plus_funding=lhs,
              cash_plus_inventory_at_cost=rhs, numerical_bound=b)
        charged = _fsum(e["fee_charged"] for e in pre if e["kind"] in ("open", "scale_in", "seed_inventory"))
        allocated = _fsum(e["entry_fee_allocated"] or 0.0 for e in pre)
        outstanding = end["entry_fee_outstanding"]
        check("simulated_entry_fees_charged_once", abs(charged - allocated - outstanding) <= 1e-9 * max(1.0, charged),
              charged=charged, allocated=allocated, outstanding=outstanding)
        sim_ids = [e["event_id"] for e in evs]
        check("simulated_each_event_one_disposition", set(sim_dispositions) == set(sim_ids)
              and len(sim_dispositions) == len(sim_ids), events=len(sim_ids), dispositions=len(sim_dispositions))
        opened = _fsum(e["quantity"] for e in pre if e["kind"] in ("open", "scale_in"))
        closed = _fsum(e["quantity"] for e in pre if e["kind"] == "close")
        check("simulated_quantity_conserved", abs(opened - closed - abs(end["position_qty"])) <= 1e-9,
              opened=opened, closed=closed, residual=abs(end["position_qty"]))
    return {"checks": checks, "all_passed": all(c["ok"] for c in checks), "informational_totals": informational}


def _decision_class(rows: list) -> tuple:
    refusals = [r for r in rows if r["decision"] == "refused"]
    return refusals, [r for r in rows if r["decision"] == "requires_evidence_verified"]


def compare(export_path: str, input_path: str, mode: str = COMPARISON_MODE_STRICT) -> dict:
    if mode not in (COMPARISON_MODE_STRICT, COMPARISON_MODE_APPROXIMATE):
        raise LedgerInputError(f"mode must be strict or approximate, got {mode!r}")
    export_bytes, doc = load_export(export_path)
    input_path = os.path.abspath(input_path)
    with open(input_path, "rb") as fh:
        input_bytes = fh.read()
    cin = _loads(input_bytes, f"comparison input {input_path}")
    parsed = validate_comparison_input(cin, os.path.dirname(input_path))
    start, end, tol = parsed["start"], parsed["end"], parsed["tolerances"]
    refusals, unverified, approximations = [], [], []
    if mode == COMPARISON_MODE_APPROXIMATE:
        approximations.append({"feature": "comparison_mode", "category": "mode",
                               "assumption": "approximate research mode was requested explicitly",
                               "effect": "parity is incomplete by definition; the result is never strict success"})

    binding = {
        "schema": cin["export"]["schema"] == doc["schema"] and cin["export"]["schema_version"] == doc["schema_version"],
        "capture_manifest_sha256": cin["export"]["capture_manifest_sha256"] == doc["capture_manifest_sha256"],
        "booked_sections_sha256": cin["export"]["booked_sections_sha256"] == booked_sections_sha256(doc),
        "selection": (cin["export"]["selection"]["partition"] == doc["selection"]["partition"]
                      and cin["export"]["selection"]["process_strategy_id"] == doc["selection"]["process_strategy_id"]),
    }
    for k, ok in sorted(binding.items()):
        if not ok:
            refusals.append({"reason": "input_binding", "field": k,
                             "detail": f"the comparison input is not bound to this export ({k})"})

    hist = select_historical_configuration(cin["historical_configuration"]["timeline"], start, end)
    if hist["status"] == "refused":
        refusals.append({"reason": hist["reason"], "detail": hist["detail"]})
    elif hist["status"] == "unverified":
        unverified.append({"input": "historical_configuration", "reason": hist["reason"]})
    seg = hist.get("segment") or {}
    strategy = seg.get("strategy") or {}

    market_spec = cin["market"]
    market = prepare_market(parsed["manifest_path"], market_spec["manifest"]["sha256"], market_spec["dataset"],
                            market_spec["window"], start, end)
    refusals.extend(market["refusals"])
    market_ctx = {"coin": None, "interval": None, "registry": set(), "funding_input_strategies": set(),
                  "observation_input_strategies": set()}
    if "dataset" in market:
        from registry_loader import load_registry
        from run_backtest import FUNDING_COLUMN_STRATEGIES, OBSERVATION_INPUT_STRATEGIES
        market_ctx = {"coin": market["dataset"]["coin"], "interval": market["manifest"]["interval"],
                      "registry": set(load_registry("futures").STRATEGY_REGISTRY),
                      "funding_input_strategies": set(FUNDING_COLUMN_STRATEGIES),
                      "observation_input_strategies": set(OBSERVATION_INPUT_STRATEGIES)}
    args = strategy.get("args") or []
    if strategy.get("type") == "manual":
        coin = strategy.get("symbol")
    elif len(args) > 1:
        coin = args[1]
    else:
        coin = market_ctx["coin"]
    input_version = parsed["input_version"]
    stop_verdict = (resolve_historical_stops(seg, input_version, mode) if seg else
                    {"applicable": False, "status": "not_applicable", "refusals": [], "owner": None,
                     "needs_labels": False, "kwargs": None, "labels": None, "required_inputs": [],
                     "input_version": input_version, "basis": None, "resolved_live_units": None,
                     "capability_context": None, "close_refs": None})
    matrix = capability_matrix(seg, cin.get("capability_evidence", {}), market_ctx, stop_verdict) if seg else []
    cap_refusals, cap_evidence = _decision_class(matrix)
    manual = strategy.get("type") == "manual"
    approx_ok = mode == COMPARISON_MODE_APPROXIMATE
    for r in cap_refusals:
        if r["category"] == "protection":
            continue
        if approx_ok and r["approximable"]:
            approximations.append({"feature": r["field"], "category": r["category"],
                                   "assumption": r["reason"], "effect": "simulated with an unverified substitute"})
        else:
            refusals.append({"reason": f"capability_{r['category']}", "field": r["field"], "detail": r["reason"]})
    for r in stop_verdict["refusals"]:
        if approx_ok and r["approximable"]:
            approximations.append({"feature": r["field"], "category": "protection",
                                   "assumption": f"{r['reason']}: {r['detail']}",
                                   "effect": "simulated with the declared, unverified stop input"})
        else:
            refusals.append({k: v for k, v in r.items() if k != "approximable"})

    booked = normalize_booked(doc, start, end, coin or "",
                              "manual owner: no simulator signal model; events are not comparable" if manual else None)
    booked_start = booked_start_state(booked)

    strategy_checks = None
    pending = None
    has_close = bool(strategy.get("close_strategy"))
    direction = strategy.get("direction") or ("both" if strategy.get("allow_shorts") else "long")
    invert = bool(strategy.get("invert_signal"))
    open_ref = strategy.get("open_strategy") or {}
    open_name = open_ref.get("name") or ((strategy.get("args") or [None])[0])
    signal_refused = any(r["field"] in ("type", "platform", "args", "open_strategy") for r in cap_refusals)
    if "dataset" in market and not manual and not signal_refused and open_name:
        try:
            strategy_checks = market_strategy_checks(
                market, open_name, open_ref.get("params") or {}, direction, invert,
                open_name in market_ctx["observation_input_strategies"], has_close,
                atr_method=strategy.get("atr_method") or "simple",
                stop_needs_atr=stop_verdict.get("owner") in STOP_OWNERS_NEEDING_ATR,
                labels=stop_verdict.get("labels") if stop_verdict.get("needs_labels") else None)
        except ValueError as exc:
            refusals.append({"reason": "strategy_rejected_inputs", "detail": f"{open_name}: {exc}"})
        else:
            refusals.extend(strategy_checks["refusals"])
            pending = strategy_checks["pending_decision"]
        if pending not in (None, "none"):
            refusals.append({"reason": "boundary_pending_decision_unmodeled",
                             "detail": f"the strategy emitted {pending!r} on the last warm-up bar; the simulator "
                                       "drops a decision made before the first scored bar"})

    state = verify_starting_state(cin["starting_state"], booked_start, pending, tol)
    for name, res in state.items():
        if res["status"] != "verified":
            unverified.append({"input": f"starting_state.{name}", "reason": res.get("note") or "not verified"})
    inv_value = state["inventory"]["value"]
    replay_inv = booked_start["inventory_before_start"]
    if ((_is_number(inv_value) and abs(inv_value) > tol["quantity_absolute"])
            or (replay_inv is not None and abs(replay_inv) > tol["quantity_absolute"])):
        refusals.append({"reason": "seeded_inventory_unsupported",
                         "detail": "a non-flat starting inventory (declared or replayed from the booked ledger) cannot "
                                   "be reproduced; execution-spec runs refuse seeded inventory",
                         "declared": inv_value, "booked_replay": replay_inv})

    close_validation = None
    sim = None
    sim_status = {"status": "not_run", "reasons": []}
    can_sim = (not manual and strategy and "dataset" in market and not signal_refused
               and _is_number(cin["starting_state"]["cash_usd"].get("value")) and strategy_checks is not None)
    if mode == COMPARISON_MODE_STRICT and refusals:
        can_sim = False
        sim_status["reasons"].append("strict comparison refused before simulation; no substitute is simulated")
    elif approx_ok and any(r["reason"] in ("input_binding", "seeded_inventory_unsupported",
                                           "configuration_transition_unsupported", "market_input_integrity",
                                           "market_manifest_hash_mismatch") + STOP_UNAPPROXIMABLE_REASONS
                           or r["reason"].startswith("capability_")
                           for r in refusals):
        can_sim = False
        sim_status["reasons"].append("an unapproximable refusal blocks simulation")
    if manual:
        sim_status["reasons"].append("manual owner: no signal model")
    if can_sim:
        use_spec = has_close and not strategy.get("allow_scale_in")
        if not use_spec:
            approximations.append({"feature": "execution_cost", "category": "execution_cost",
                                   "assumption": "flat taker fee and combined spread/slippage from the manifest replace "
                                                 "the lot- and minimum-aware execution spec",
                                   "effect": "fills are not lot-floored or minimum-checked"})
        plan = {
            "open_name": open_name, "params": dict(open_ref.get("params") or {}),
            "close_refs": ((copy.deepcopy(stop_verdict["close_refs"]) if stop_verdict.get("close_refs") is not None
                            else [strategy["close_strategy"]]) if has_close else None),
            "direction": direction, "invert_signal": invert,
            "atr_method": strategy.get("atr_method") or "simple",
            "comparison_mode": mode, "initial_cash": float(cin["starting_state"]["cash_usd"]["value"]),
            "execution_spec": use_spec,
            "stop": stop_verdict,
        }
        if approx_ok:
            if strategy.get("allow_scale_in"):
                plan["allow_scale_in"] = True
                plan["scale_in"] = strategy.get("scale_in")
        try:
            sim = run_simulation(market, strategy_checks["signals"], plan)
            close_validation = decode_close_validation(sim["close_validation"])
            sim_status = {"status": "run", "reasons": [], "cost_model": sim["cost_model"],
                          "execution": sim["execution"]}
        except CloseCapabilityError as exc:
            close_validation = decode_close_validation(exc.validation.to_dict())
            refusals.append({"reason": "close_validation_refused", "detail": str(exc)})
            sim_status = {"status": "refused", "reasons": [str(exc)]}
        except ValueError as exc:
            refusals.append({"reason": "simulator_refused", "detail": str(exc)})
            sim_status = {"status": "refused", "reasons": [str(exc)]}
    close_ok = (close_validation is not None and close_validation.get("decode_status") == "ok"
                and close_validation.get("mode") == COMPARISON_MODE_STRICT
                and close_validation.get("close_eligibility") == "eligible"
                and not close_validation.get("approximations") and not close_validation.get("refusals"))
    if close_validation is not None and close_validation.get("approximations"):
        for a in close_validation["approximations"]:
            approximations.append({"feature": a.get("feature"), "category": "close_validation",
                                   "assumption": a.get("reason_code"), "effect": "approximate close evaluation"})

    simulated = normalize_simulated(sim["envelope"]) if sim else None
    step_s = (sim["interval_ms"] / 1000.0) if sim else 0.0
    matched, ambiguous, unmatched_b, unmatched_s, weak = [], [], [], [], []
    geometry_pairs = []
    booked_disp = dict(booked["dispositions"])
    sim_disp = dict(simulated["dispositions"]) if simulated else {}
    if simulated is not None:
        m = match_positions(booked, simulated, step_s, tol)
        weak = m["weak"]
        assigned_b = {bi for bi, _ in m["assigned"]}
        assigned_s = {si for _, si in m["assigned"]}
        amb_b = {bi for a in m["ambiguous"] for bi in a["booked"]}
        amb_s = {si for a in m["ambiguous"] for si in a["simulated"]}
        for bi, si in m["assigned"]:
            bp, sp = m["bps"][bi], m["sps"][si]
            pair = reconcile_pair(bp, sp, step_s, tol)
            matched.append(pair)
            geometry_pairs.append((bp, sp, pair))
            comp_b = {c["booked_event_key"] for c in pair["components"]}
            comp_s = {c["simulated_event_id"] for c in pair["components"]}
            for k in bp["in_interval_event_keys"]:
                booked_disp[k] = "matched" if k in comp_b else "unmatched"
            for e in sp["_events"]:
                sim_disp[e["event_id"]] = "matched" if e["event_id"] in comp_s else "unmatched"
        for a in m["ambiguous"]:
            ambiguous.append({
                "booked_position_ids": [m["bps"][i]["position_id"] for i in a["booked"]],
                "simulated_position_ids": [m["sps"][i]["position_local_id"] for i in a["simulated"]],
                "reason": a["reason"], "alternatives": a.get("alternatives"),
                "candidates": [{"booked_position_id": m["bps"][bi]["position_id"],
                                "simulated_position_id": m["sps"][si]["position_local_id"],
                                "evidence": m["edges"][(bi, si)]} for bi, si in a["edges"]],
            })
            for i in a["booked"]:
                for k in m["bps"][i]["in_interval_event_keys"]:
                    booked_disp[k] = "ambiguous"
            for i in a["simulated"]:
                for e in m["sps"][i]["_events"]:
                    sim_disp[e["event_id"]] = "ambiguous"
        for bi, bp in enumerate(m["bps"]):
            if bi not in assigned_b and bi not in amb_b:
                unmatched_b.append({"booked_position_id": bp["position_id"], "event_keys": bp["in_interval_event_keys"],
                                    "opened_qty": bp["opened_qty"], "closed_qty": bp["closed_qty"],
                                    "residual_qty": bp["residual_qty"], "ledger_delta": bp["ledger_delta"],
                                    "weak_candidates": [w for w in weak if w["booked_position_id"] == bp["position_id"]],
                                    "cause": None})
                for k in bp["in_interval_event_keys"]:
                    booked_disp[k] = "unmatched"
        for si, sp in enumerate(m["sps"]):
            if si not in assigned_s and si not in amb_s:
                unmatched_s.append({"simulated_position_id": sp["position_local_id"], "event_ids": sp["event_ids"],
                                    "opened_qty": sp["opened_qty"], "closed_qty": sp["closed_qty"],
                                    "residual_qty": sp["residual_qty"], "net": sp["net"],
                                    "weak_candidates": [w for w in weak if w["simulated_position_id"] == sp["position_local_id"]],
                                    "cause": None})
                for e in sp["_events"]:
                    sim_disp[e["event_id"]] = "unmatched"
    else:
        for p in booked["positions"]:
            if p["status"] == "comparable":
                for k in p["in_interval_event_keys"]:
                    booked_disp[k] = "not_simulated"
                unmatched_b.append({"booked_position_id": p["position_id"], "event_keys": p["in_interval_event_keys"],
                                    "opened_qty": p["opened_qty"], "closed_qty": p["closed_qty"],
                                    "residual_qty": p["residual_qty"], "ledger_delta": p["ledger_delta"],
                                    "weak_candidates": [], "cause": "simulation_not_run"})

    initial_geometry = None
    if simulated is not None:
        arm_map = map_first_arms(simulated, sim["stop_events"])
        initial_geometry = compare_initial_stop_geometry(
            [(bp, sp) for bp, sp, _ in geometry_pairs], arm_map,
            cin.get("initial_stop_geometry_evidence") or {}, tol)
        for (_, _, pair), res in zip(geometry_pairs, initial_geometry["positions"]):
            pair["initial_stop_geometry"] = res
            if res["status"] == "mismatch":
                pair["within_tolerance"] = False
            elif res["status"] == "unverified":
                unverified.append({"input": "initial_stop_geometry", "reason": res["reason"],
                                   "booked_position_id": res["booked_position_id"]})

    pos_by_id = {p["position_id"]: p for p in booked["positions"]}
    booked_sections = {
        "matched_components": [c["booked_event_key"] for p in matched for c in p["components"]],
        "matched_pair_unmatched_components": [k for p in matched for k in p["unmatched_booked_components"]],
        "ambiguous": [k for a in ambiguous for pid in a["booked_position_ids"]
                      for k in pos_by_id[pid]["in_interval_event_keys"]],
        "unmatched_booked": [k for u in unmatched_b for k in u["event_keys"]],
        "strategy_funding": [r["event_key"] for r in booked["funding_in"]],
        "unresolved": [u["event_key"] for u in booked["unresolved"]],
        "outside_interval": [o["event_key"] for o in booked["outside"]],
        "not_comparable": [n["event_key"] for n in booked["not_comparable"]],
    }
    cons = conservation(doc, booked, simulated, sim["envelope"] if sim else None, booked_disp, sim_disp, tol,
                        booked_sections, matched)

    in_records = [r for r in booked["records"] if r["relation"] == "in"]
    booked_funding = _fsum(r["realized_pnl"] for r in booked["funding_in"])
    booked_net = _fsum(r["ledger_delta"] for r in in_records)
    end_inv = None
    if not booked["unresolved"] and not manual:
        end_inv = 0.0
        for p in booked["positions"]:
            if p["status"] not in ("comparable", "outside_interval") or not p["residual_qty"]:
                continue
            side = p["ownership"]["side"]
            if side not in ("long", "short"):
                if abs(p["residual_qty"]) > tol["quantity_absolute"]:
                    end_inv = None
                    break
                continue
            end_inv += p["residual_qty"] * (1.0 if side == "long" else -1.0)
    strategy_totals = {
        "booked": {"funding": booked_funding, "ledger_delta": booked_net, "fees": _fsum(r["fee"] for r in in_records),
                   "end_inventory_signed": end_inv, "events_in_interval": len(in_records)},
    }
    totals_ok = False
    if simulated is not None:
        env = sim["envelope"]
        pre = [e for e in env["events"] if e["kind"] != "terminal_liquidation"]
        sim_funding = _fsum(e["funding_cash"] or 0.0 for e in pre)
        sim_net = _fsum(e["gross_realized"] or 0.0 for e in pre) - _fsum(e["fee_charged"] for e in pre) + sim_funding
        sim_inv = env["interval_end"]["position_qty"]
        strategy_totals["simulated"] = {"funding": sim_funding, "net": sim_net,
                                        "fees": _fsum(e["fee_charged"] for e in pre),
                                        "end_inventory_signed": sim_inv}
        strategy_totals["deltas"] = {
            "funding": booked_funding - sim_funding,
            "net": booked_net - sim_net,
            "end_inventory_signed": (end_inv - sim_inv) if end_inv is not None else None,
        }
        totals_ok = (abs(strategy_totals["deltas"]["funding"]) <= tol["money_absolute"]
                     and abs(strategy_totals["deltas"]["net"]) <= tol["money_absolute"]
                     and end_inv is not None
                     and abs(strategy_totals["deltas"]["end_inventory_signed"]) <= tol["quantity_absolute"])
        strategy_totals["within_tolerance"] = bool(totals_ok)

    in_interval_out = [o for o in booked["outside"] if o["relation"] == "in"]
    strict_checks = {
        "mode_strict": mode == COMPARISON_MODE_STRICT,
        "no_refusals": not refusals,
        "inputs_verified": not unverified,
        "no_approximations": not approximations,
        "close_validation_strict_eligible": close_ok,
        "simulation_ran": simulated is not None,
        "conservation_passed": cons["all_passed"],
        "no_ambiguous": not ambiguous,
        "no_unmatched_booked": not unmatched_b,
        "no_unmatched_simulated": not unmatched_s,
        "no_unresolved_in_interval": not booked["unresolved"] and not in_interval_out,
        "matched_within_tolerance": all(p["within_tolerance"] for p in matched),
        "strategy_totals_within_tolerance": bool(totals_ok),
        "initial_stop_geometry_consistent": initial_geometry is None or all(
            r["status"] in ("agreement", "unavailable") for r in initial_geometry["positions"]),
    }
    strict_success = all(strict_checks.values())
    if strict_success:
        outcome = "strict_success"
    elif mode == COMPARISON_MODE_APPROXIMATE:
        outcome = "incomplete"
    elif refusals:
        outcome = "refused"
    elif unverified:
        outcome = "unverified"
    else:
        outcome = "mismatch"

    present = doc["current_effective_configuration"]
    differences = []
    if seg:
        for key in sorted(set(present.get("strategy") or {}) | set(strategy)):
            if (present.get("strategy") or {}).get(key) != strategy.get(key):
                differences.append({"field": f"strategy.{key}", "present": (present.get("strategy") or {}).get(key),
                                    "historical": strategy.get(key)})
    report = {
        "schema": REPORT_SCHEMA,
        "schema_version": REPORT_SCHEMA_VERSION,
        "mode": mode,
        "outcome": outcome,
        "strict_success": strict_success,
        "exit_code": EXIT_STRICT_SUCCESS if strict_success else EXIT_NOT_STRICT,
        "strict_checks": strict_checks,
        "provenance": {
            "export_sha256": _sha256(export_bytes),
            "export_schema": doc["schema"],
            "export_schema_version": doc["schema_version"],
            "capture_manifest_sha256": doc["capture_manifest_sha256"],
            "booked_sections_sha256": booked_sections_sha256(doc),
            "inspected_revision": doc["inspected_revision"],
            "capture": doc["capture"],
            "snapshot_files": doc["snapshot_files"],
            "selection": doc["selection"],
            "comparison_input_sha256": _sha256(input_bytes),
            "comparison_input_schema": [INPUT_SCHEMA, input_version],
            "input_binding": binding,
            "market": market.get("provenance"),
            "simulator_events_schema": [LEDGER_EVENTS_SCHEMA, LEDGER_EVENTS_SCHEMA_VERSION],
        },
        "timestamps": {
            "time_basis": "UTC",
            "interval": {"start": _iso(start), "end": _iso(end), "start_inclusive": True, "end_inclusive": False},
            "booked_meanings": doc["timestamp_meanings"],
            "booked_event_timestamp_meaning": "ledger_record_time; exchange fill time is not established",
            "simulated_meanings": sim["envelope"]["timing_meanings"] if sim else None,
            "matching_rule": "a booked record time matches a bar-open fill within +/- time tolerance of the bar open, "
                             "and an intrabar fill within the bar plus the tolerance",
        },
        "tolerances": {k: {"value": v, "unit": TOLERANCE_UNITS[k]} for k, v in tol.items()},
        "configuration": {
            "present": {"basis": present.get("basis"), "config_version": present.get("config_version"),
                        "source": "export.current_effective_configuration",
                        "note": "current-at-capture evidence only; never used as historical configuration",
                        "strategy": present.get("strategy"), "regime": present.get("regime"),
                        "portfolio_risk": present.get("portfolio_risk")},
            "historical": {"source": "comparison_input.historical_configuration", "selection": {
                k: v for k, v in hist.items() if k != "segment"}, "segment": seg or None},
            "present_vs_historical_differences": differences,
        },
        "starting_state": state,
        "booked_start_replay": booked_start,
        "eligibility": {
            "refusals": refusals,
            "unverified": unverified,
            "approximations": approximations,
            "capability_matrix": matrix,
            "capability_evidence_used": [r["field"] for r in cap_evidence],
            "close_validation": close_validation,
            "market": {"status": market["status"], "checks": market.get("checks"),
                       "strategy_checks": (strategy_checks or {}).get("checks"),
                       "pending_decision_recomputed": pending},
        },
        "booked": {
            "events_total": len(doc["events"]),
            "events_in_interval": len(in_records),
            "positions": [{k: v for k, v in p.items() if not k.startswith("_")} for p in booked["positions"]],
            "strategy_funding": {"in_interval_total": booked_funding,
                                 "event_keys": [r["event_key"] for r in booked["funding_in"]],
                                 "allocation": "unallocated", "allocated_to_positions": 0.0},
            "unresolved": booked["unresolved"],
            "outside_interval": booked["outside"],
            "not_comparable": booked["not_comparable"],
            "dispositions": booked_disp,
            "field_evidence": booked["field_evidence"],
        },
        "simulated": None if simulated is None else {
            "events_total": len(sim["envelope"]["events"]),
            "positions": [{k: v for k, v in p.items() if not k.startswith("_")} for p in simulated["positions"]],
            "funding_total": _fsum(e["funding_cash"] or 0.0 for e in simulated["funding"]),
            "funding_allocation": "per simulated position; compared only as a strategy total",
            "interval_end": simulated["interval_end"],
            "synthetic_terminal_events": [e["event_id"] for e in simulated["synthetic"]],
            "dispositions": sim_disp,
        },
        "simulation": sim_status,
        "stops": {
            "input_version": input_version,
            "basis": stop_verdict.get("basis"),
            "applicable": stop_verdict["applicable"],
            "status": stop_verdict["status"],
            "owner": stop_verdict.get("owner"),
            "required_inputs": stop_verdict.get("required_inputs"),
            "refusals": [{k: v for k, v in r.items()} for r in stop_verdict["refusals"]],
            "resolved_live_units": stop_verdict.get("resolved_live_units"),
            "engine_inputs": (_jsonable_stop_inputs(stop_verdict["kwargs"])
                              if stop_verdict.get("kwargs") else None),
            "capability_context": stop_verdict.get("capability_context"),
            "labels": stop_verdict.get("labels") if stop_verdict.get("needs_labels") else None,
            "simulated_owner": sim["stop_owner"] if sim else None,
            "warmup_skipped_entries": sim["stop_warmup_skipped_entries"] if sim else None,
            "arm_events": ([{k: _json_safe(v) for k, v in e.items()} for e in sim["stop_events"]
                            if e.get("event") == "arm"] if sim else None),
        },
        "initial_stop_geometry": initial_geometry,
        "matching": {
            "matched": matched,
            "ambiguous": ambiguous,
            "unmatched_booked": unmatched_b,
            "unmatched_simulated": unmatched_s,
            "weak_candidates": weak,
        },
        "strategy_totals": strategy_totals,
        "costs": {
            "actual_recorded": {"fees": strategy_totals["booked"]["fees"], "funding": booked_funding,
                                "source": "booked exchange_fee and funding rows"},
            "modeled": None if simulated is None else {
                "fees": strategy_totals["simulated"]["fees"], "funding": strategy_totals["simulated"]["funding"],
                "source": sim_status.get("cost_model")},
        },
        "wallet_orphan_context": {
            "status": doc["wallet_orphan_context"]["status"],
            "records": len(doc["wallet_orphan_context"]["records"]),
            "total_usd": _fsum(r["amount_usd"] for r in doc["wallet_orphan_context"]["records"]),
            "included_in_strategy_totals": False,
        },
        "conservation": cons,
    }
    return report


def _json_safe(value):
    if isinstance(value, float) and not math.isfinite(value):
        return None
    if isinstance(value, (np.floating,)):
        return float(value) if math.isfinite(float(value)) else None
    if isinstance(value, (np.integer,)):
        return int(value)
    return value


def _json_default(obj):
    if isinstance(obj, (np.integer,)):
        return int(obj)
    if isinstance(obj, (np.floating,)):
        return float(obj)
    if isinstance(obj, (set, frozenset)):
        return sorted(obj)
    if isinstance(obj, pd.Timestamp):
        return _iso(obj)
    raise TypeError(f"{type(obj).__name__} is not JSON serializable")


def write_report(report: dict, path: str) -> None:
    if os.path.lexists(path):
        raise LedgerInputError(f"output {path} already exists; refusing to replace it")
    text = json.dumps(report, indent=2, sort_keys=True, default=_json_default, allow_nan=False) + "\n"
    tmp = f"{path}.{os.getpid()}.tmp"
    with open(tmp, "x") as fh:
        fh.write(text)
    try:
        os.link(tmp, path)
    finally:
        os.unlink(tmp)


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(
        description="Compare one booked-ledger export with the Backtester over frozen inputs (issue 1686). "
                    "Research-only: reads frozen files, writes one report, changes no live state.")
    parser.add_argument("--export", required=True, help="go-trader.booked-ledger export JSON")
    parser.add_argument("--comparison-input", required=True, help="go-trader.ledger-comparison-input JSON")
    parser.add_argument("--mode", choices=[COMPARISON_MODE_STRICT, COMPARISON_MODE_APPROXIMATE],
                        default=COMPARISON_MODE_STRICT,
                        help="strict (default) refuses unmodeled inputs; approximate is research-only and never "
                             "reports strict success")
    parser.add_argument("--output", required=True, help="new report JSON path (never replaced)")
    args = parser.parse_args(argv)
    try:
        report = compare(args.export, args.comparison_input, args.mode)
        write_report(report, args.output)
    except (LedgerInputError, OSError) as exc:
        print(f"ledger_compare: {exc}", file=sys.stderr)
        return EXIT_INPUT_ERROR
    except Exception as exc:
        traceback.print_exc(file=sys.stderr)
        print(f"ledger_compare: internal error, no report written: {type(exc).__name__}: {exc}", file=sys.stderr)
        return EXIT_INTERNAL_ERROR
    print(f"ledger_compare: outcome={report['outcome']} strict_success={report['strict_success']} "
          f"matched={len(report['matching']['matched'])} ambiguous={len(report['matching']['ambiguous'])} "
          f"unmatched_booked={len(report['matching']['unmatched_booked'])} "
          f"unmatched_simulated={len(report['matching']['unmatched_simulated'])} "
          f"refusals={len(report['eligibility']['refusals'])} report={args.output}")
    return report["exit_code"]


if __name__ == "__main__":
    sys.exit(main())
