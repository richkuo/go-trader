import argparse
import bisect
import datetime as _dt
import hashlib
import json
import os
import re
import sys
from typing import Dict, Iterable, List, Optional, Tuple
from zoneinfo import ZoneInfo, ZoneInfoNotFoundError

CALENDAR_SCHEMA = "macro_event_calendar/v1"
QUOTE_SCHEMA = "macro_event_quotes/v1"
IMPORTER = "macro_event_inputs/1654-v1"
CALENDAR_KIND = "macro_calendar"
QUOTE_KIND = "macro_quotes"

EVENT_TYPE_FOMC = "us_fomc_rate_statement"
EVENT_TYPE_CONTROL = "research_control_us_fomc_rate_statement"
EVENT_TYPES = (EVENT_TYPE_FOMC, EVENT_TYPE_CONTROL)
ARTIFACT_KIND_EVENT_TYPES = {
    "official": (EVENT_TYPE_FOMC,),
    "research_control": (EVENT_TYPE_CONTROL,),
    "fixture": EVENT_TYPES,
}
STATUS_SCHEDULED = "scheduled"
STATUSES = (STATUS_SCHEDULED, "postponed", "cancelled")

CALENDAR_TOP_KEYS = ("schema", "kind", "importer", "event_types", "coverage", "sources", "records")
CALENDAR_SOURCE_KEYS = ("source_id", "publisher", "url", "retrieved_ms", "sha256", "published_ms", "note")
CALENDAR_HASH_FIELDS = ("event_id", "event_type", "revision", "status", "scheduled_local", "source_timezone",
                        "scheduled_utc_ms", "known_ms", "publication_evidence", "source_id")
CALENDAR_RECORD_KEYS = CALENDAR_HASH_FIELDS + ("record_sha256",)
CALENDAR_CONTENT_FIELDS = ("event_type", "status", "scheduled_local", "source_timezone", "scheduled_utc_ms")

QUOTE_TOP_KEYS = ("schema", "kind", "importer", "venue", "namespace", "coin", "sources", "records")
QUOTE_SOURCE_KEYS = ("source_id", "publisher", "url", "retrieved_ms", "sha256", "note")
QUOTE_HASH_FIELDS = ("event_ms", "recv_ms", "bid", "ask", "source_id")
QUOTE_RECORD_KEYS = QUOTE_HASH_FIELDS + ("record_sha256",)

CALENDAR_PAYLOAD_KEYS = ("kind", "available", "reason", "calendar_sha256", "schema", "artifact_kind",
                         "event_types", "coverage", "cutoff_ms", "records")
QUOTE_PAYLOAD_KEYS = ("kind", "available", "reason", "quote_sha256", "schema", "artifact_kind", "venue",
                      "namespace", "coin", "cutoff_ms", "window_ms", "records")

VENUE = "hyperliquid"
NAMESPACE = "perps"
MAX_STRING = 256
MAX_URL = 1024
DECIMAL_SCALE = 12
_HEX64 = re.compile(r"^[0-9a-f]{64}$")
_EVENT_ID = re.compile(r"^[A-Za-z0-9_./:-]{1,128}$")
_LOCAL = re.compile(r"^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})$")
_COIN = re.compile(r"^[A-Za-z0-9]{1,16}$")
_REPO = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))


class MacroInputError(ValueError):
    pass


def sha256_bytes(blob: bytes) -> str:
    return hashlib.sha256(blob).hexdigest()


def sha256_file(path: str) -> str:
    with open(path, "rb") as fh:
        return sha256_bytes(fh.read())


def canonical_json_bytes(value) -> bytes:
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=True).encode("ascii")


def record_hash(record: dict, fields: Tuple[str, ...]) -> str:
    return sha256_bytes(canonical_json_bytes([record.get(f) for f in fields]))


def _is_int(value) -> bool:
    return isinstance(value, int) and not isinstance(value, bool)


def _ascii_text(value, what: str, limit: int = MAX_STRING) -> str:
    if not isinstance(value, str) or not value or len(value) > limit:
        raise MacroInputError(f"{what} must be a nonempty string of at most {limit} characters, got {value!r}")
    if any(ord(ch) < 0x20 or ord(ch) > 0x7E for ch in value):
        raise MacroInputError(f"{what} must be printable ASCII, got {value!r}")
    return value


def _pos_int(value, what: str) -> int:
    if not _is_int(value) or value <= 0:
        raise MacroInputError(f"{what} must be a positive integer, got {value!r}")
    return value


def _hex64(value, what: str) -> str:
    if not isinstance(value, str) or not _HEX64.match(value):
        raise MacroInputError(f"{what} must be a lowercase 64-hex sha256, got {value!r}")
    return value


def _strict_keys(obj, keys: Tuple[str, ...], what: str) -> dict:
    if not isinstance(obj, dict):
        raise MacroInputError(f"{what} must be an object")
    missing = [k for k in keys if k not in obj]
    extra = sorted(set(obj) - set(keys))
    if missing or extra:
        raise MacroInputError(f"{what} keys differ from the contract (missing {missing}, unknown {extra})")
    return obj


def canonical_decimal(raw, what: str) -> str:
    if not isinstance(raw, str) or not raw:
        raise MacroInputError(f"{what} must be a decimal string, got {raw!r}")
    whole, dot, frac = raw.partition(".")
    if not whole or (dot and not frac) or not (whole + frac).isdigit() or not (whole + frac).isascii():
        raise MacroInputError(f"{what} {raw!r} is not a plain nonnegative decimal")
    if len(whole) > 1 and whole.startswith("0"):
        raise MacroInputError(f"{what} {raw!r} has a leading zero")
    if dot and frac.endswith("0"):
        raise MacroInputError(f"{what} {raw!r} has a trailing zero")
    if len(frac) > DECIMAL_SCALE or len(whole) > 15:
        raise MacroInputError(f"{what} {raw!r} exceeds the {DECIMAL_SCALE}-digit scale or 15 integer digits")
    return raw


def normalize_decimal(raw, what: str) -> str:
    if isinstance(raw, bool) or not isinstance(raw, (str, int, float)):
        raise MacroInputError(f"{what} must be a decimal, got {raw!r}")
    text = format(raw, "f") if isinstance(raw, float) else str(raw).strip()
    if text.startswith("+") or text.startswith("-"):
        raise MacroInputError(f"{what} {raw!r} must be a plain nonnegative decimal")
    whole, dot, frac = text.partition(".")
    whole = whole.lstrip("0") or "0"
    frac = frac.rstrip("0")
    return canonical_decimal(whole + ("." + frac if frac else ""), what)


def decimal_units(text: str) -> int:
    whole, _, frac = text.partition(".")
    return int(whole) * 10 ** DECIMAL_SCALE + int((frac + "0" * DECIMAL_SCALE)[:DECIMAL_SCALE] or "0")


def local_to_utc_ms(local: str, tz_name: str) -> int:
    m = _LOCAL.match(local or "")
    if not m:
        raise MacroInputError(f"scheduled_local {local!r} must be YYYY-MM-DDTHH:MM:SS")
    try:
        zone = ZoneInfo(tz_name)
    except (ZoneInfoNotFoundError, ValueError) as exc:
        raise MacroInputError(f"source_timezone {tz_name!r} is not a known IANA zone: {exc}")
    try:
        naive = _dt.datetime(*(int(g) for g in m.groups()))
    except ValueError as exc:
        raise MacroInputError(f"scheduled_local {local!r} is not a valid time: {exc}")
    early, late = naive.replace(tzinfo=zone, fold=0), naive.replace(tzinfo=zone, fold=1)
    back = early.astimezone(_dt.timezone.utc).astimezone(zone).replace(tzinfo=None)
    if back != naive:
        raise MacroInputError(f"scheduled_local {local!r} does not exist in {tz_name}")
    if early.utcoffset() != late.utcoffset():
        raise MacroInputError(f"scheduled_local {local!r} is ambiguous in {tz_name}")
    return int(early.astimezone(_dt.timezone.utc).timestamp() * 1000)


def _validate_sources(sources, keys: Tuple[str, ...], what: str) -> Dict[str, dict]:
    if not isinstance(sources, list) or not sources:
        raise MacroInputError(f"{what} sources must be a nonempty list")
    out = {}
    prev = None
    for i, src in enumerate(sources):
        _strict_keys(src, keys, f"{what} source {i}")
        sid = _ascii_text(src["source_id"], f"{what} source {i} source_id", 128)
        if prev is not None and sid <= prev:
            raise MacroInputError(f"{what} sources are not strictly ordered by source_id at {sid!r}")
        prev = sid
        _ascii_text(src["publisher"], f"{what} source {sid} publisher")
        _ascii_text(src["url"], f"{what} source {sid} url", MAX_URL)
        _pos_int(src["retrieved_ms"], f"{what} source {sid} retrieved_ms")
        _hex64(src["sha256"], f"{what} source {sid} sha256")
        if "published_ms" in keys and src["published_ms"] is not None:
            _pos_int(src["published_ms"], f"{what} source {sid} published_ms")
        _ascii_text(src["note"], f"{what} source {sid} note", 2048)
        out[sid] = src
    return out


def validate_calendar_record(rec, i, event_types, sources) -> dict:
    what = f"calendar record {i}"
    _strict_keys(rec, CALENDAR_RECORD_KEYS, what)
    if not isinstance(rec["event_id"], str) or not _EVENT_ID.match(rec["event_id"]):
        raise MacroInputError(f"{what} event_id {rec['event_id']!r} is malformed")
    if rec["event_type"] not in event_types:
        raise MacroInputError(f"{what} event_type {rec['event_type']!r} is not declared in {list(event_types)}")
    _pos_int(rec["revision"], f"{what} revision")
    if rec["status"] not in STATUSES:
        raise MacroInputError(f"{what} status {rec['status']!r} is not one of {STATUSES}")
    utc = local_to_utc_ms(rec["scheduled_local"], rec["source_timezone"])
    if rec["scheduled_utc_ms"] != utc:
        raise MacroInputError(f"{what} scheduled_utc_ms {rec['scheduled_utc_ms']!r} is not the UTC conversion "
                              f"{utc} of {rec['scheduled_local']} {rec['source_timezone']}")
    if rec["known_ms"] is not None:
        _pos_int(rec["known_ms"], f"{what} known_ms")
    _ascii_text(rec["publication_evidence"], f"{what} publication_evidence", 512)
    if rec["source_id"] not in sources:
        raise MacroInputError(f"{what} source_id {rec['source_id']!r} names no artifact source")
    _hex64(rec["record_sha256"], f"{what} record_sha256")
    if record_hash(rec, CALENDAR_HASH_FIELDS) != rec["record_sha256"]:
        raise MacroInputError(f"{what} ({rec['event_id']} r{rec['revision']}) record_sha256 does not match its content")
    return rec


def calendar_sort_key(rec: dict):
    known = rec["known_ms"]
    return (rec["event_id"], rec["revision"], 1 if known is None else 0, known or 0, rec["record_sha256"])


def validate_calendar_artifact(doc) -> dict:
    _strict_keys(doc, CALENDAR_TOP_KEYS, "calendar artifact")
    if doc["schema"] != CALENDAR_SCHEMA:
        raise MacroInputError(f"calendar schema {doc['schema']!r} is not {CALENDAR_SCHEMA!r}")
    kind = doc["kind"]
    if kind not in ARTIFACT_KIND_EVENT_TYPES:
        raise MacroInputError(f"calendar kind {kind!r} is not one of {sorted(ARTIFACT_KIND_EVENT_TYPES)}")
    if doc["importer"] != IMPORTER:
        raise MacroInputError(f"calendar importer {doc['importer']!r} is not {IMPORTER!r}")
    types = doc["event_types"]
    if (not isinstance(types, list) or not types or len(set(types)) != len(types) or types != sorted(types)
            or any(t not in ARTIFACT_KIND_EVENT_TYPES[kind] for t in types)):
        raise MacroInputError(f"calendar event_types {types!r} must be a sorted unique subset of "
                              f"{list(ARTIFACT_KIND_EVENT_TYPES[kind])} for kind {kind!r}")
    cov = doc["coverage"]
    if not isinstance(cov, list):
        raise MacroInputError("calendar coverage must be a list")
    prev = None
    for i, c in enumerate(cov):
        _strict_keys(c, ("event_type", "start_ms", "end_ms"), f"calendar coverage {i}")
        if c["event_type"] not in types:
            raise MacroInputError(f"calendar coverage {i} event_type {c['event_type']!r} is not declared")
        _pos_int(c["start_ms"], f"calendar coverage {i} start_ms")
        _pos_int(c["end_ms"], f"calendar coverage {i} end_ms")
        if c["end_ms"] <= c["start_ms"]:
            raise MacroInputError(f"calendar coverage {i} is empty or reversed")
        key = (c["event_type"], c["start_ms"])
        if prev is not None and (key <= prev[0] or (c["event_type"] == prev[0][0] and c["start_ms"] < prev[1])):
            raise MacroInputError(f"calendar coverage {i} is out of order or overlaps")
        prev = (key, c["end_ms"])
    sources = _validate_sources(doc["sources"], CALENDAR_SOURCE_KEYS, "calendar")
    records = doc["records"]
    if not isinstance(records, list):
        raise MacroInputError("calendar records must be a list")
    prev_key = None
    event_type_of = {}
    for i, rec in enumerate(records):
        validate_calendar_record(rec, i, types, sources)
        key = calendar_sort_key(rec)
        if prev_key is not None:
            if key == prev_key or key[4] == prev_key[4]:
                raise MacroInputError(f"calendar record {i} duplicates an earlier record")
            if key < prev_key:
                raise MacroInputError(f"calendar record {i} is out of canonical order (event_id, revision, known_ms, hash)")
        prev_key = key
        if event_type_of.setdefault(rec["event_id"], rec["event_type"]) != rec["event_type"]:
            raise MacroInputError(f"calendar event {rec['event_id']} changes event_type between records")
    return doc


def validate_quote_record(rec, i, sources) -> dict:
    what = f"quote record {i}"
    _strict_keys(rec, QUOTE_RECORD_KEYS, what)
    _pos_int(rec["event_ms"], f"{what} event_ms")
    _pos_int(rec["recv_ms"], f"{what} recv_ms")
    if rec["recv_ms"] < rec["event_ms"]:
        raise MacroInputError(f"{what} was received at {rec['recv_ms']} before its event time {rec['event_ms']}")
    bid = decimal_units(canonical_decimal(rec["bid"], f"{what} bid"))
    ask = decimal_units(canonical_decimal(rec["ask"], f"{what} ask"))
    if bid <= 0 or ask < bid:
        raise MacroInputError(f"{what} needs 0 < bid <= ask, got bid {rec['bid']} ask {rec['ask']}")
    if rec["source_id"] not in sources:
        raise MacroInputError(f"{what} source_id {rec['source_id']!r} names no artifact source")
    _hex64(rec["record_sha256"], f"{what} record_sha256")
    if record_hash(rec, QUOTE_HASH_FIELDS) != rec["record_sha256"]:
        raise MacroInputError(f"{what} record_sha256 does not match its content")
    return rec


def quote_sort_key(rec: dict):
    return (rec["event_ms"], rec["recv_ms"], rec["record_sha256"])


def validate_quote_artifact(doc, venue: Optional[str] = None, namespace: Optional[str] = None,
                            coin: Optional[str] = None) -> dict:
    _strict_keys(doc, QUOTE_TOP_KEYS, "quote artifact")
    if doc["schema"] != QUOTE_SCHEMA:
        raise MacroInputError(f"quote schema {doc['schema']!r} is not {QUOTE_SCHEMA!r}")
    if doc["kind"] not in ("official", "fixture", "recorded"):
        raise MacroInputError(f"quote kind {doc['kind']!r} is not one of ['fixture', 'official', 'recorded']")
    if doc["importer"] != IMPORTER:
        raise MacroInputError(f"quote importer {doc['importer']!r} is not {IMPORTER!r}")
    if doc["venue"] != VENUE or doc["namespace"] != NAMESPACE:
        raise MacroInputError(f"quote artifact venue/namespace {doc['venue']!r}/{doc['namespace']!r} is not {VENUE}/{NAMESPACE}")
    if not isinstance(doc["coin"], str) or not _COIN.match(doc["coin"]):
        raise MacroInputError(f"quote artifact coin {doc['coin']!r} is malformed")
    for want, got, label in ((venue, doc["venue"], "venue"), (namespace, doc["namespace"], "namespace"),
                             (coin, doc["coin"], "coin")):
        if want is not None and want != got:
            raise MacroInputError(f"quote artifact {label} {got!r} is not the requested {want!r}")
    sources = _validate_sources(doc["sources"], QUOTE_SOURCE_KEYS, "quote")
    records = doc["records"]
    if not isinstance(records, list):
        raise MacroInputError("quote records must be a list")
    prev = None
    seen = set()
    for i, rec in enumerate(records):
        validate_quote_record(rec, i, sources)
        key = quote_sort_key(rec)
        if rec["record_sha256"] in seen:
            raise MacroInputError(f"quote record {i} duplicates an earlier record")
        seen.add(rec["record_sha256"])
        if prev is not None and key < prev:
            raise MacroInputError(f"quote record {i} is out of canonical order (event_ms, recv_ms, hash)")
        prev = key
    return doc


def resolve_artifact_path(path: str, base_dir: Optional[str] = None) -> str:
    if not isinstance(path, str) or not path.strip():
        raise MacroInputError("artifact path is empty")
    if os.path.isabs(path):
        return path
    return os.path.abspath(os.path.join(base_dir or _REPO, path))


def read_artifact(path: str, expected_sha256: str) -> Tuple[dict, str]:
    _hex64(expected_sha256, "artifact sha256")
    try:
        with open(path, "rb") as fh:
            blob = fh.read()
    except OSError as exc:
        raise MacroInputError(f"artifact {path} is unreadable: {exc}")
    digest = sha256_bytes(blob)
    if digest != expected_sha256:
        raise MacroInputError(f"artifact {path} has sha256 {digest}, the configuration pins {expected_sha256}")
    try:
        doc = json.loads(blob.decode("ascii"))
    except (UnicodeDecodeError, ValueError) as exc:
        raise MacroInputError(f"artifact {path} is not ASCII JSON: {exc}")
    return doc, digest


def load_calendar_artifact(path: str, expected_sha256: str) -> Tuple[dict, str]:
    doc, digest = read_artifact(path, expected_sha256)
    return validate_calendar_artifact(doc), digest


def load_quote_artifact(path: str, expected_sha256: str, coin: Optional[str] = None) -> Tuple[dict, str]:
    doc, digest = read_artifact(path, expected_sha256)
    return validate_quote_artifact(doc, VENUE, NAMESPACE, coin), digest


def unavailable_calendar(reason: str, sha: str = "", cutoff_ms: Optional[int] = None) -> dict:
    return {"kind": CALENDAR_KIND, "available": False, "reason": reason, "calendar_sha256": sha, "schema": "",
            "artifact_kind": "", "event_types": [], "coverage": [], "cutoff_ms": cutoff_ms, "records": []}


def unavailable_quotes(reason: str, coin: str = "", sha: str = "", cutoff_ms: Optional[int] = None,
                       window_ms: Optional[int] = None) -> dict:
    return {"kind": QUOTE_KIND, "available": False, "reason": reason, "quote_sha256": sha, "schema": "",
            "artifact_kind": "", "venue": VENUE, "namespace": NAMESPACE, "coin": coin, "cutoff_ms": cutoff_ms,
            "window_ms": window_ms, "records": []}


def calendar_payload(doc: dict, sha: str, cutoff_ms: Optional[int] = None) -> dict:
    records = [dict(r) for r in doc["records"]
               if r["known_ms"] is not None and (cutoff_ms is None or r["known_ms"] <= cutoff_ms)]
    return {"kind": CALENDAR_KIND, "available": True, "reason": "", "calendar_sha256": sha,
            "schema": doc["schema"], "artifact_kind": doc["kind"], "event_types": list(doc["event_types"]),
            "coverage": [dict(c) for c in doc["coverage"]], "cutoff_ms": cutoff_ms, "records": records}


def quote_payload(doc: dict, sha: str, cutoff_ms: Optional[int] = None, window_ms: Optional[int] = None) -> dict:
    start = None if cutoff_ms is None or window_ms is None else cutoff_ms - window_ms
    records = [dict(r) for r in doc["records"]
               if (cutoff_ms is None or r["recv_ms"] <= cutoff_ms) and (start is None or r["recv_ms"] >= start)]
    return {"kind": QUOTE_KIND, "available": True, "reason": "", "quote_sha256": sha, "schema": doc["schema"],
            "artifact_kind": doc["kind"], "venue": doc["venue"], "namespace": doc["namespace"], "coin": doc["coin"],
            "cutoff_ms": cutoff_ms, "window_ms": window_ms, "records": records}


class CalendarView:
    def __init__(self, error: Optional[str] = None, detail: str = ""):
        self.error = error
        self.detail = detail
        self.sha = ""
        self.cutoff = None
        self.coverage: Dict[str, List[Tuple[int, int]]] = {}
        self.by_event: Dict[str, List[dict]] = {}
        self.kind = ""

    def covered(self, event_type: str, start_ms: int, end_ms: int) -> bool:
        return any(s <= start_ms and end_ms <= e for s, e in self.coverage.get(event_type, ()))

    def selected(self, event_id: str, at_ms: int):
        known = [r for r in self.by_event.get(event_id, ()) if r["known_ms"] is not None and r["known_ms"] <= at_ms]
        if not known:
            return None
        top = max(r["revision"] for r in known)
        same = {r["record_sha256"]: r for r in known if r["revision"] == top}
        if len(same) > 1:
            return "conflict"
        return next(iter(same.values()))


def parse_event_records(payload) -> CalendarView:
    if payload is None:
        return CalendarView("calendar_unavailable", "no event_records were supplied")
    if not isinstance(payload, dict):
        return CalendarView("calendar_malformed", "event_records must be an object")
    if payload.get("kind") != CALENDAR_KIND:
        return CalendarView("calendar_malformed", f"event_records kind {payload.get('kind')!r} is not {CALENDAR_KIND!r}")
    if not payload.get("available"):
        return CalendarView("calendar_unavailable", str(payload.get("reason") or "event_records marked unavailable"))
    try:
        _strict_keys(payload, CALENDAR_PAYLOAD_KEYS, "event_records")
        if payload["schema"] != CALENDAR_SCHEMA:
            raise MacroInputError(f"schema {payload['schema']!r} is not {CALENDAR_SCHEMA!r}")
        kind = payload["artifact_kind"]
        if kind not in ARTIFACT_KIND_EVENT_TYPES:
            raise MacroInputError(f"artifact_kind {kind!r} is unknown")
        sha = _hex64(payload["calendar_sha256"], "calendar_sha256")
        cutoff = payload["cutoff_ms"]
        if cutoff is not None:
            _pos_int(cutoff, "cutoff_ms")
        types = payload["event_types"]
        if not isinstance(types, list) or any(t not in ARTIFACT_KIND_EVENT_TYPES[kind] for t in types):
            raise MacroInputError(f"event_types {types!r} do not fit kind {kind!r}")
        view = CalendarView()
        view.sha, view.cutoff, view.kind = sha, cutoff, kind
        for i, c in enumerate(payload["coverage"] if isinstance(payload["coverage"], list) else [None]):
            _strict_keys(c, ("event_type", "start_ms", "end_ms"), f"coverage {i}")
            if c["event_type"] not in types or not _is_int(c["start_ms"]) or not _is_int(c["end_ms"]) \
                    or c["end_ms"] <= c["start_ms"]:
                raise MacroInputError(f"coverage {i} is malformed")
            view.coverage.setdefault(c["event_type"], []).append((c["start_ms"], c["end_ms"]))
        if not isinstance(payload["records"], list):
            raise MacroInputError("records must be a list")
        pseudo_sources = {r.get("source_id"): None for r in payload["records"] if isinstance(r, dict)}
        prev = None
        for i, rec in enumerate(payload["records"]):
            validate_calendar_record(rec, i, types, pseudo_sources)
            if rec["known_ms"] is None:
                raise MacroInputError(f"record {i} has no known_ms; only deadline-eligible records belong in a payload")
            if cutoff is not None and rec["known_ms"] > cutoff:
                raise MacroInputError(f"record {i} became known at {rec['known_ms']}, after the cutoff {cutoff}")
            key = calendar_sort_key(rec)
            if prev is not None and (key <= prev or key[4] == prev[4]):
                raise MacroInputError(f"record {i} is duplicated or out of canonical order")
            prev = key
            group = view.by_event.setdefault(rec["event_id"], [])
            if group and group[0]["event_type"] != rec["event_type"]:
                raise MacroInputError(f"event {rec['event_id']} changes event_type between records")
            group.append(rec)
        return view
    except MacroInputError as exc:
        return CalendarView("calendar_malformed", str(exc))


class QuoteView:
    def __init__(self, error: Optional[str] = None, detail: str = ""):
        self.error = error
        self.detail = detail
        self.sha = ""
        self.coin = ""
        self.cutoff = None
        self.records: List[dict] = []
        self.events: List[int] = []

    def select(self, at_ms: int, max_age_ms: int):
        floor = at_ms - max_age_ms
        best = None
        found = {}
        i = bisect.bisect_right(self.events, at_ms) - 1
        while i >= 0:
            rec = self.records[i]
            if rec["event_ms"] < floor or (best is not None and rec["event_ms"] < best):
                break
            if rec["recv_ms"] <= at_ms:
                best = rec["event_ms"]
                found[rec["record_sha256"]] = rec
            i -= 1
        if not found:
            return "quote_missing", None
        if len(found) > 1:
            return "quote_conflict", sorted(found.values(), key=lambda r: r["record_sha256"])[0]
        return "ok", next(iter(found.values()))


def parse_spread_records(payload, coin: Optional[str] = None) -> QuoteView:
    if payload is None:
        return QuoteView("quotes_unavailable", "no spread_records were supplied")
    if not isinstance(payload, dict):
        return QuoteView("quotes_malformed", "spread_records must be an object")
    if payload.get("kind") != QUOTE_KIND:
        return QuoteView("quotes_malformed", f"spread_records kind {payload.get('kind')!r} is not {QUOTE_KIND!r}")
    if not payload.get("available"):
        return QuoteView("quotes_unavailable", str(payload.get("reason") or "spread_records marked unavailable"))
    try:
        _strict_keys(payload, QUOTE_PAYLOAD_KEYS, "spread_records")
        if payload["schema"] != QUOTE_SCHEMA:
            raise MacroInputError(f"schema {payload['schema']!r} is not {QUOTE_SCHEMA!r}")
        if payload["venue"] != VENUE or payload["namespace"] != NAMESPACE:
            raise MacroInputError(f"venue/namespace {payload['venue']!r}/{payload['namespace']!r} is not {VENUE}/{NAMESPACE}")
        if coin is not None and payload["coin"] != coin:
            raise MacroInputError(f"quotes are for {payload['coin']!r}, the strategy trades {coin!r}")
        view = QuoteView()
        view.sha = _hex64(payload["quote_sha256"], "quote_sha256")
        view.coin = payload["coin"]
        cutoff = payload["cutoff_ms"]
        window = payload["window_ms"]
        if cutoff is not None:
            _pos_int(cutoff, "cutoff_ms")
        if window is not None:
            _pos_int(window, "window_ms")
        view.cutoff = cutoff
        if not isinstance(payload["records"], list):
            raise MacroInputError("records must be a list")
        pseudo_sources = {r.get("source_id"): None for r in payload["records"] if isinstance(r, dict)}
        prev = None
        for i, rec in enumerate(payload["records"]):
            validate_quote_record(rec, i, pseudo_sources)
            if cutoff is not None and rec["recv_ms"] > cutoff:
                raise MacroInputError(f"record {i} was received at {rec['recv_ms']}, after the cutoff {cutoff}")
            if cutoff is not None and window is not None and rec["recv_ms"] < cutoff - window:
                raise MacroInputError(f"record {i} was received before the payload window")
            key = quote_sort_key(rec)
            if prev is not None and key <= prev:
                raise MacroInputError(f"record {i} is duplicated or out of canonical order")
            prev = key
            view.records.append(rec)
            view.events.append(rec["event_ms"])
        return view
    except MacroInputError as exc:
        return QuoteView("quotes_malformed", str(exc))


def spread_fraction(rec: dict) -> float:
    bid = decimal_units(rec["bid"])
    ask = decimal_units(rec["ask"])
    return (ask - bid) / ((ask + bid) / 2.0)


def build_calendar_record(row: dict) -> dict:
    rec = {
        "event_id": row["event_id"],
        "event_type": row["event_type"],
        "revision": row["revision"],
        "status": row["status"],
        "scheduled_local": row["scheduled_local"],
        "source_timezone": row["source_timezone"],
        "scheduled_utc_ms": row.get("scheduled_utc_ms"),
        "known_ms": row.get("known_ms"),
        "publication_evidence": row["publication_evidence"],
        "source_id": row["source_id"],
    }
    utc = local_to_utc_ms(rec["scheduled_local"], rec["source_timezone"])
    if rec["scheduled_utc_ms"] is None:
        rec["scheduled_utc_ms"] = utc
    elif rec["scheduled_utc_ms"] != utc:
        raise MacroInputError(f"{rec['event_id']}: declared UTC {rec['scheduled_utc_ms']} differs from the conversion {utc}")
    rec["record_sha256"] = record_hash(rec, CALENDAR_HASH_FIELDS)
    return rec


def import_calendar(rows: Iterable[dict], sources: List[dict], coverage: List[dict], kind: str,
                    event_types: List[str]) -> Tuple[dict, dict]:
    built = [build_calendar_record(r) for r in rows]
    stats = {"input_rows": len(built), "identical_duplicates": 0, "conflicting_versions": 0}
    by_version: Dict[Tuple[str, int], List[dict]] = {}
    for rec in built:
        by_version.setdefault((rec["event_id"], rec["revision"]), []).append(rec)
    kept = []
    for key in sorted(by_version):
        group = sorted(by_version[key], key=calendar_sort_key)
        content: Dict[tuple, dict] = {}
        for rec in group:
            ck = tuple(rec[f] for f in CALENDAR_CONTENT_FIELDS)
            if ck in content:
                stats["identical_duplicates"] += 1
                continue
            content[ck] = rec
        if len(content) > 1:
            stats["conflicting_versions"] += 1
        kept.extend(content.values())
    kept.sort(key=calendar_sort_key)
    doc = {
        "schema": CALENDAR_SCHEMA, "kind": kind, "importer": IMPORTER,
        "event_types": sorted(event_types),
        "coverage": sorted((dict(c) for c in coverage), key=lambda c: (c["event_type"], c["start_ms"])),
        "sources": sorted((dict(s) for s in sources), key=lambda s: s["source_id"]),
        "records": kept,
    }
    validate_calendar_artifact(doc)
    return doc, stats


def build_quote_record(row: dict) -> dict:
    rec = {
        "event_ms": row["event_ms"], "recv_ms": row["recv_ms"],
        "bid": normalize_decimal(row["bid"], "bid"), "ask": normalize_decimal(row["ask"], "ask"),
        "source_id": row["source_id"],
    }
    rec["record_sha256"] = record_hash(rec, QUOTE_HASH_FIELDS)
    return rec


def import_quotes(rows: Iterable[dict], sources: List[dict], coin: str, kind: str) -> Tuple[dict, dict]:
    built = [build_quote_record(r) for r in rows]
    stats = {"input_rows": len(built), "identical_duplicates": 0, "conflicting_event_times": 0}
    by_event: Dict[int, List[dict]] = {}
    for rec in built:
        by_event.setdefault(rec["event_ms"], []).append(rec)
    kept = []
    for ev in sorted(by_event):
        group = sorted(by_event[ev], key=quote_sort_key)
        content: Dict[tuple, dict] = {}
        for rec in group:
            ck = (rec["bid"], rec["ask"])
            if ck in content:
                stats["identical_duplicates"] += 1
                continue
            content[ck] = rec
        if len(content) > 1:
            stats["conflicting_event_times"] += 1
        kept.extend(content.values())
    kept.sort(key=quote_sort_key)
    doc = {"schema": QUOTE_SCHEMA, "kind": kind, "importer": IMPORTER, "venue": VENUE, "namespace": NAMESPACE,
           "coin": coin, "sources": sorted((dict(s) for s in sources), key=lambda s: s["source_id"]),
           "records": kept}
    validate_quote_artifact(doc)
    return doc, stats


def artifact_bytes(doc: dict) -> bytes:
    return canonical_json_bytes(doc) + b"\n"


def write_artifact(path: str, doc: dict) -> str:
    blob = artifact_bytes(doc)
    tmp = path + ".tmp"
    with open(tmp, "wb") as fh:
        fh.write(blob)
    os.replace(tmp, path)
    return sha256_bytes(blob)


def calendar_coverage(doc: dict, event_type: str, start_ms: int, end_ms: int) -> dict:
    view = parse_event_records(calendar_payload(doc, "0" * 64))
    events = {}
    for rec in doc["records"]:
        if rec["event_type"] != event_type:
            continue
        events.setdefault(rec["event_id"], []).append(rec)
    rows = []
    for event_id in sorted(events):
        latest = max(events[event_id], key=lambda r: (r["revision"], r["known_ms"] or 0))
        release = latest["scheduled_utc_ms"]
        if not (start_ms <= release < end_ms):
            continue
        at_release = view.selected(event_id, release)
        if at_release is None:
            state = "unknown_publication" if all(r["known_ms"] is None for r in events[event_id]) else "not_known_before_release"
        elif at_release == "conflict":
            state = "conflict"
        elif at_release["status"] != STATUS_SCHEDULED:
            state = at_release["status"]
        elif at_release["scheduled_utc_ms"] != release:
            state = "revised"
        else:
            state = "eligible"
        rows.append({"event_id": event_id, "release_ms": release, "state": state,
                     "revisions": len(events[event_id])})
    states: Dict[str, int] = {}
    for r in rows:
        states[r["state"]] = states.get(r["state"], 0) + 1
    covered = view.covered(event_type, start_ms, end_ms)
    return {"event_type": event_type, "window": [start_ms, end_ms], "calendar_covers_window": covered,
            "events": len(rows), "eligible_events": states.get("eligible", 0),
            "states": dict(sorted(states.items())), "rows": rows}


def research_inputs(params: dict, coin: str, base_dir: Optional[str] = None) -> Tuple[dict, dict, dict]:
    cal_path, cal_sha = str(params.get("calendar_path") or ""), str(params.get("calendar_sha256") or "")
    quote_path, quote_sha = str(params.get("quote_path") or ""), str(params.get("quote_sha256") or "")
    info = {"calendar_path": cal_path, "calendar_sha256": cal_sha, "quote_path": quote_path,
            "quote_sha256": quote_sha}
    if cal_path or cal_sha:
        doc, sha = load_calendar_artifact(resolve_artifact_path(cal_path, base_dir), cal_sha)
        events = calendar_payload(doc, sha)
        info["calendar_records"] = len(doc["records"])
        info["calendar_eligible_records"] = len(events["records"])
    else:
        events = unavailable_calendar("no calendar artifact is configured (calendar_path, calendar_sha256)")
    if quote_path or quote_sha:
        doc, sha = load_quote_artifact(resolve_artifact_path(quote_path, base_dir), quote_sha, coin)
        quotes = quote_payload(doc, sha)
        info["quote_records"] = len(doc["records"])
    else:
        quotes = unavailable_quotes("no quote artifact is configured (quote_path, quote_sha256); "
                                    "no historical bid/ask record backs the spread gate", coin)
    return events, quotes, info


def _cli(argv=None) -> int:
    ap = argparse.ArgumentParser(description="Validate macro-event calendar and quote artifacts (#1654).")
    sub = ap.add_subparsers(dest="cmd", required=True)
    v = sub.add_parser("verify", help="check an artifact's digest and contract")
    v.add_argument("--calendar")
    v.add_argument("--quotes")
    v.add_argument("--sha256", required=True)
    v.add_argument("--coin")
    args = ap.parse_args(argv)
    try:
        if bool(args.calendar) == bool(args.quotes):
            raise MacroInputError("pass exactly one of --calendar or --quotes")
        if args.calendar:
            doc, sha = load_calendar_artifact(args.calendar, args.sha256)
            print(json.dumps({"ok": True, "sha256": sha, "kind": doc["kind"], "records": len(doc["records"])}))
        else:
            doc, sha = load_quote_artifact(args.quotes, args.sha256, args.coin)
            print(json.dumps({"ok": True, "sha256": sha, "coin": doc["coin"], "records": len(doc["records"])}))
        return 0
    except MacroInputError as exc:
        print(json.dumps({"ok": False, "error": str(exc)}))
        return 1


if __name__ == "__main__":
    sys.exit(_cli())
