import re
from fractions import Fraction

from pair_evidence import (
    CONVERSION_FIELDS,
    KEY_FIELDS,
    MAX_DOCUMENT_BYTES,
    MAX_ID_LEN,
    MAX_PROVENANCE_LEN,
    JSONNumber,
    PairEvidenceError,
    _shape,
    canonical_bytes,
    decimal_fraction,
    fraction_decimal,
    loads_strict,
    sha256_hex,
    validate_key,
    validate_stamp,
)

DOCUMENT_VERSION = 1
RECORD_VERSION = 1
BATCH_VERSION = 1
EVENT_VERSION = 2
COVERAGE_VERSION = 1
MAX_RECORDS = 1 << 16
MAX_INTERVALS = 1 << 12

RULE_HYPERLIQUID = "hyperliquid_hourly_oracle_v1"
RULE_OKX = "okx_swap_realized_mark_v1"

RULES = {
    RULE_HYPERLIQUID: {"provider": "hyperliquid", "currency": "USDC", "price_kind": "oracle", "assessment_window_ms": 0},
    RULE_OKX: {"provider": "okx", "currency": "USDT", "price_kind": "mark", "assessment_window_ms": 60000},
}

_HEX64 = re.compile(r"^[0-9a-f]{64}$")
_OKX_SWAP = re.compile(r"^[A-Z0-9]+-USDT-SWAP$")

INTERVAL_FIELDS = (("from_ms", "int"), ("through_ms", "int"))
GAP_FIELDS = (("from_ms", "int"), ("through_ms", "int"), ("reason", "str"))

RATE_FIELDS = (
    ("v", "int"), ("key", KEY_FIELDS), ("event_id", "str"), ("rule_version", "str"), ("event_ms", "int"),
    ("visible_ms", "int"), ("rate", "decimal"), ("provenance", "str"), ("record_hash", "str"),
)
PRICE_FIELDS = (
    ("v", "int"), ("key", KEY_FIELDS), ("evidence_id", "str"), ("price_kind", "str"), ("at_ms", "int"),
    ("visible_ms", "int"), ("price", "decimal"), ("provenance", "str"), ("record_hash", "str"),
)
COVERAGE_RECORD_FIELDS = (
    ("v", "int"), ("key", KEY_FIELDS), ("evidence_id", "str"), ("requested_from_ms", "int"), ("requested_through_ms", "int"),
    ("proven", ("list", INTERVAL_FIELDS)), ("gaps", ("list", GAP_FIELDS)), ("visible_ms", "int"), ("provenance", "str"), ("record_hash", "str"),
)
DOCUMENT_FIELDS = (
    ("v", "int"), ("source_id", "str"), ("rates", ("list", RATE_FIELDS)), ("prices", ("list", PRICE_FIELDS)),
    ("coverage", ("list", COVERAGE_RECORD_FIELDS)),
)
BATCH_FIELDS = (
    ("v", "int"), ("source_id", "str"), ("seq", "int"), ("prev_hash", "str"), ("rates", ("list", RATE_FIELDS)),
    ("prices", ("list", PRICE_FIELDS)), ("coverage", ("list", COVERAGE_RECORD_FIELDS)), ("batch_hash", "str"),
)
EVENT_COVERAGE_FIELDS = (
    ("v", "int"), ("key", KEY_FIELDS), ("requested_from_ms", "int"), ("requested_through_ms", "int"),
    ("proven", ("list", INTERVAL_FIELDS)), ("gaps", ("list", GAP_FIELDS)), ("inventory_through_ms", "int"),
    ("source_kind", "str"), ("source_id", "str"), ("cutoff_ms", "int"),
)
EVENT_FIELDS = (
    ("v", "int"), ("key", KEY_FIELDS), ("event_id", "str"), ("rule_version", "str"), ("event_ms", "int"),
    ("visible_ms", "int"), ("signed_base_qty", "decimal"), ("native_mark", "decimal"), ("rate", "decimal"),
    ("currency", "str"), ("native_amount", "decimal"), ("usd_amount", "decimal"), ("conversion", CONVERSION_FIELDS),
    ("coverage", EVENT_COVERAGE_FIELDS), ("source_hash", "str"), ("price_hash", "str"), ("evidence_hash", "str"),
)

SCHEMAS = {"rate": RATE_FIELDS, "price": PRICE_FIELDS, "coverage": COVERAGE_RECORD_FIELDS}


def _printable(text, limit):
    return isinstance(text, str) and 0 < len(text) <= limit and all(0x20 <= ord(ch) <= 0x7E for ch in text)


def _key_tuple(key):
    return tuple(key[name].encode("utf-8") for name, _ in KEY_FIELDS)


def event_id(provider, instrument, event_ms):
    return f"{provider}:{instrument}:{int(event_ms)}"


def record_hash(record, kind):
    body = _shape(record, SCHEMAS[kind], kind, True)
    body["record_hash"] = ""
    return sha256_hex(canonical_bytes(body))


def seal(record, kind):
    out = _shape(record, SCHEMAS[kind], kind, True)
    out["record_hash"] = record_hash(out, kind)
    return out


def source_hash(rate):
    body = {name: rate[name] for name in ("key", "event_id", "rule_version", "event_ms", "rate")}
    return sha256_hex(canonical_bytes(body))


def _normalize(intervals):
    xs = sorted((iv for iv in intervals if iv[0] < iv[1]), key=lambda iv: iv[0])
    out = []
    for lo, hi in xs:
        if out and lo <= out[-1][1]:
            out[-1][1] = max(out[-1][1], hi)
        else:
            out.append([lo, hi])
    return out


def _intersect(a, b):
    out = []
    for x in a:
        for y in b:
            lo, hi = max(x[0], y[0]), min(x[1], y[1])
            if lo < hi:
                out.append([lo, hi])
    return _normalize(out)


def _subtract(a, b):
    cur = _normalize(a)
    for y in _normalize(b):
        nxt = []
        for x in cur:
            if y[1] <= x[0] or y[0] >= x[1]:
                nxt.append(x)
                continue
            if y[0] > x[0]:
                nxt.append([x[0], y[0]])
            if y[1] < x[1]:
                nxt.append([y[1], x[1]])
        cur = nxt
    return cur


def _intervals_valid(items, lo, hi, what):
    if len(items) > MAX_INTERVALS:
        raise PairEvidenceError(f"{what}: {len(items)} intervals exceed the {MAX_INTERVALS} bound")
    prev = lo
    for i, (a, b) in enumerate(items):
        if a >= b:
            raise PairEvidenceError(f"{what}: interval {i} ({a}, {b}] is empty")
        if a < prev or b > hi:
            raise PairEvidenceError(f"{what}: interval {i} ({a}, {b}] is out of order or outside ({lo}, {hi}]")
        prev = b


def validate_rate(r):
    what = f"funding rate {r['event_id']}"
    if r["v"] != RECORD_VERSION:
        raise PairEvidenceError(f"{what} version {r['v']} is not supported (want {RECORD_VERSION})")
    validate_key(r["key"])
    if r["event_ms"] <= 0 or r["visible_ms"] < r["event_ms"]:
        raise PairEvidenceError(f"{what} needs event_ms above zero and visible_ms at or after it")
    want = event_id(r["key"]["provider"], r["key"]["instrument"], r["event_ms"])
    if r["event_id"] != want:
        raise PairEvidenceError(f"{what} event_id must be the venue identity {want}")
    if not _printable(r["rule_version"], MAX_ID_LEN):
        raise PairEvidenceError(f"{what} rule_version must be 1..{MAX_ID_LEN} printable ASCII characters")
    decimal_fraction(r["rate"], "rate")
    if not _printable(r["provenance"], MAX_PROVENANCE_LEN):
        raise PairEvidenceError(f"{what} provenance must be 1..{MAX_PROVENANCE_LEN} printable ASCII characters")
    if not _HEX64.match(r["record_hash"]) or r["record_hash"] != record_hash(r, "rate"):
        raise PairEvidenceError(f"{what} record_hash does not match its canonical bytes")


def validate_price(p):
    what = f"funding price {p['evidence_id']}"
    if p["v"] != RECORD_VERSION:
        raise PairEvidenceError(f"{what} version {p['v']} is not supported (want {RECORD_VERSION})")
    validate_key(p["key"])
    if not _printable(p["evidence_id"], MAX_ID_LEN):
        raise PairEvidenceError(f"funding price evidence_id {p['evidence_id']!r} must be 1..{MAX_ID_LEN} printable ASCII characters")
    if p["price_kind"] not in ("mark", "oracle"):
        raise PairEvidenceError(f"{what} price_kind {p['price_kind']!r} is not mark or oracle")
    if p["at_ms"] <= 0 or p["visible_ms"] < p["at_ms"]:
        raise PairEvidenceError(f"{what} needs at_ms above zero and visible_ms at or after it")
    if decimal_fraction(p["price"], "price") <= 0:
        raise PairEvidenceError(f"{what} price must be above zero")
    if not _printable(p["provenance"], MAX_PROVENANCE_LEN):
        raise PairEvidenceError(f"{what} provenance must be 1..{MAX_PROVENANCE_LEN} printable ASCII characters")
    if not _HEX64.match(p["record_hash"]) or p["record_hash"] != record_hash(p, "price"):
        raise PairEvidenceError(f"{what} record_hash does not match its canonical bytes")


def validate_coverage(c):
    what = f"funding coverage {c['evidence_id']}"
    if c["v"] != RECORD_VERSION:
        raise PairEvidenceError(f"{what} version {c['v']} is not supported (want {RECORD_VERSION})")
    validate_key(c["key"])
    if not _printable(c["evidence_id"], MAX_ID_LEN):
        raise PairEvidenceError(f"funding coverage evidence_id {c['evidence_id']!r} must be 1..{MAX_ID_LEN} printable ASCII characters")
    lo, hi = c["requested_from_ms"], c["requested_through_ms"]
    if lo < 0 or lo >= hi:
        raise PairEvidenceError(f"{what} requested window ({lo}, {hi}] is empty or negative")
    if c["visible_ms"] < 0:
        raise PairEvidenceError(f"{what} visible_ms is negative")
    proven = [(iv["from_ms"], iv["through_ms"]) for iv in c["proven"]]
    gaps = [(g["from_ms"], g["through_ms"]) for g in c["gaps"]]
    _intervals_valid(proven, lo, hi, f"{what} proven")
    for g in c["gaps"]:
        if not _printable(g["reason"], MAX_ID_LEN):
            raise PairEvidenceError(f"{what} gap reason must be 1..{MAX_ID_LEN} printable ASCII characters")
    _intervals_valid(gaps, lo, hi, f"{what} gaps")
    if _intersect([list(x) for x in proven], [list(x) for x in gaps]):
        raise PairEvidenceError(f"{what} declares an interval both proven and a gap")
    if not _printable(c["provenance"], MAX_PROVENANCE_LEN):
        raise PairEvidenceError(f"{what} provenance must be 1..{MAX_PROVENANCE_LEN} printable ASCII characters")
    if not _HEX64.match(c["record_hash"]) or c["record_hash"] != record_hash(c, "coverage"):
        raise PairEvidenceError(f"{what} record_hash does not match its canonical bytes")


def check_records(rates, prices, coverage, prior=None):
    if len(rates) > MAX_RECORDS or len(prices) > MAX_RECORDS or len(coverage) > MAX_RECORDS:
        raise PairEvidenceError(f"funding input holds more than {MAX_RECORDS} records of one kind")
    seen = dict(prior or {})
    fresh = set()
    for r in rates:
        validate_rate(r)
        ident = (_key_tuple(r["key"]), r["event_id"], r["rule_version"])
        if ident in fresh:
            raise PairEvidenceError(f"funding rate {r['event_id']} appears twice in one input")
        fresh.add(ident)
        sh = source_hash(r)
        if ident in seen and seen[ident] != sh:
            raise PairEvidenceError(f"funding rate {r['event_id']} conflicts with earlier evidence for the same event identity")
        seen[ident] = sh
    pseen = set()
    for p in prices:
        validate_price(p)
        ident = (_key_tuple(p["key"]), p["evidence_id"])
        if ident in pseen:
            raise PairEvidenceError(f"funding price {p['evidence_id']} appears twice")
        pseen.add(ident)
    cseen = set()
    for c in coverage:
        validate_coverage(c)
        ident = (_key_tuple(c["key"]), c["evidence_id"])
        if ident in cseen:
            raise PairEvidenceError(f"funding coverage {c['evidence_id']} appears twice")
        cseen.add(ident)
        for r in rates:
            if r["key"] != c["key"] or r["visible_ms"] <= c["visible_ms"]:
                continue
            for iv in c["proven"]:
                if iv["from_ms"] < r["event_ms"] <= iv["through_ms"]:
                    raise PairEvidenceError(f"funding coverage {c['evidence_id']} proves ({iv['from_ms']}, {iv['through_ms']}] complete at {c['visible_ms']}, but rate {r['event_id']} inside it is first visible at {r['visible_ms']}")
    return seen


def _sort_lists(doc):
    doc["rates"].sort(key=lambda r: (_key_tuple(r["key"]), r["event_ms"], r["rule_version"].encode("utf-8"), r["visible_ms"]))
    doc["prices"].sort(key=lambda p: (_key_tuple(p["key"]), p["at_ms"], p["price_kind"].encode("utf-8"), p["visible_ms"], p["evidence_id"].encode("utf-8")))
    doc["coverage"].sort(key=lambda c: (_key_tuple(c["key"]), c["visible_ms"], c["evidence_id"].encode("utf-8")))


def document_bytes(doc):
    norm = _shape(doc, DOCUMENT_FIELDS, "funding document", True)
    _sort_lists(norm)
    return canonical_bytes(norm)


def decode_document(data, expected_digest=None):
    if len(data) > MAX_DOCUMENT_BYTES:
        raise PairEvidenceError(f"funding document is {len(data)} bytes, above the {MAX_DOCUMENT_BYTES} byte limit")
    if expected_digest is not None:
        if not isinstance(expected_digest, str) or not _HEX64.match(expected_digest):
            raise PairEvidenceError("the expected funding document digest must be lowercase SHA-256 hex")
        if sha256_hex(bytes(data)) != expected_digest:
            raise PairEvidenceError("funding document digest is not the expected digest")
    tree = loads_strict(data)
    doc = _shape(tree, DOCUMENT_FIELDS, "funding document", False)
    if doc["v"] != DOCUMENT_VERSION:
        raise PairEvidenceError(f"funding document version {doc['v']} is not supported (want {DOCUMENT_VERSION})")
    if not _printable(doc["source_id"], MAX_ID_LEN):
        raise PairEvidenceError(f"funding document source_id must be 1..{MAX_ID_LEN} printable ASCII characters")
    if document_bytes(doc) != bytes(data):
        raise PairEvidenceError("funding document bytes are not canonical (compact sorted keys, canonical decimals, records sorted by key and time)")
    check_records(doc["rates"], doc["prices"], doc["coverage"])
    return doc


def batch_hash(batch):
    norm = _shape(batch, BATCH_FIELDS, "funding batch", True)
    _sort_lists(norm)
    norm["batch_hash"] = ""
    return sha256_hex(canonical_bytes(norm))


def seal_batch(batch):
    norm = _shape(batch, BATCH_FIELDS, "funding batch", True)
    _sort_lists(norm)
    norm["batch_hash"] = batch_hash(norm)
    return norm


def batch_bytes(batch):
    norm = _shape(batch, BATCH_FIELDS, "funding batch", True)
    _sort_lists(norm)
    return canonical_bytes(norm)


def funding_amounts(qty, price, rate, rate_usd):
    native = -(Fraction(qty) * Fraction(price) * Fraction(rate))
    usd = native * Fraction(rate_usd)
    return fraction_decimal(native), fraction_decimal(usd)


def event_hash(event):
    norm = _shape(event, EVENT_FIELDS, "funding event", True)
    norm["evidence_hash"] = ""
    return sha256_hex(canonical_bytes(norm))


def decode_event(data):
    tree = loads_strict(data)
    ev = _shape(tree, EVENT_FIELDS, "funding event", False)
    if canonical_bytes(ev) != bytes(data):
        raise PairEvidenceError("funding event bytes are not canonical")
    what = f"funding event {ev['event_id']}"
    if ev["v"] != EVENT_VERSION:
        raise PairEvidenceError(f"{what} version {ev['v']} is not supported (want {EVENT_VERSION})")
    validate_key(ev["key"])
    if ev["event_id"] != event_id(ev["key"]["provider"], ev["key"]["instrument"], ev["event_ms"]) or ev["event_ms"] <= 0:
        raise PairEvidenceError(f"{what} event_id is not the venue identity")
    if ev["visible_ms"] < ev["event_ms"]:
        raise PairEvidenceError(f"{what} visible_ms is before event_ms")
    rule = RULES.get(ev["rule_version"])
    if rule is None or rule["provider"] != ev["key"]["provider"]:
        raise PairEvidenceError(f"{what} rule {ev['rule_version']!r} is not the supported rule for {ev['key']['provider']}")
    if ev["currency"] != rule["currency"] or ev["conversion"]["currency"] != ev["currency"]:
        raise PairEvidenceError(f"{what} currency and conversion must both be the rule currency {rule['currency']}")
    qty = decimal_fraction(ev["signed_base_qty"], "signed_base_qty")
    if qty == 0:
        raise PairEvidenceError(f"{what} signed_base_qty is zero")
    if decimal_fraction(ev["native_mark"], "native_mark") <= 0 or decimal_fraction(ev["conversion"]["rate_usd"], "rate_usd") <= 0:
        raise PairEvidenceError(f"{what} price and conversion must be above zero")
    native, usd = funding_amounts(ev["signed_base_qty"], ev["native_mark"], ev["rate"], ev["conversion"]["rate_usd"])
    if ev["native_amount"] != native or ev["usd_amount"] != usd:
        raise PairEvidenceError(f"{what} amounts {ev['native_amount']}/{ev['usd_amount']} are not the rule product {native}/{usd}")
    validate_stamp(ev["conversion"], "conversion")
    if ev["conversion"]["key"] != ev["key"]:
        raise PairEvidenceError(f"{what} conversion key is not the member key")
    if not (ev["conversion"]["valid_from_ms"] <= ev["event_ms"] <= ev["conversion"]["valid_through_ms"]):
        raise PairEvidenceError(f"{what} conversion is not valid at event time")
    cov = ev["coverage"]
    if cov["v"] != COVERAGE_VERSION or cov["key"] != ev["key"] or cov["requested_through_ms"] != ev["event_ms"] or cov["requested_from_ms"] >= cov["requested_through_ms"]:
        raise PairEvidenceError(f"{what} coverage must name the member key with a requested window ending at the event")
    if cov["gaps"]:
        raise PairEvidenceError(f"{what} coverage still holds gaps")
    want = [[cov["requested_from_ms"], cov["requested_through_ms"]]]
    if _subtract(want, [[iv["from_ms"], iv["through_ms"]] for iv in cov["proven"]]):
        raise PairEvidenceError(f"{what} proven coverage does not span the requested window")
    if cov["inventory_through_ms"] < max(ev["event_ms"], ev["event_ms"] + rule["assessment_window_ms"] - 1):
        raise PairEvidenceError(f"{what} inventory is not complete through the event and its assessment window")
    if cov["cutoff_ms"] < ev["visible_ms"]:
        raise PairEvidenceError(f"{what} cutoff is before its inputs were visible")
    if not _HEX64.match(ev["source_hash"]) or not _HEX64.match(ev["price_hash"]):
        raise PairEvidenceError(f"{what} needs its source and price record hashes")
    if not _HEX64.match(ev["evidence_hash"]) or ev["evidence_hash"] != event_hash(ev):
        raise PairEvidenceError(f"{what} evidence_hash does not match its canonical bytes")
    return ev


def okx_range_records(key, result, visible_ms, provenance, evidence_id):
    if key["provider"] != "okx" or not _OKX_SWAP.match(key["instrument"]):
        raise PairEvidenceError("an OKX range result needs an OKX member key with a <UNDERLYING>-USDT-SWAP instrument")
    if result.get("inst_id") != key["instrument"]:
        raise PairEvidenceError(f"the OKX range result is for {result.get('inst_id')!r}, the member trades {key['instrument']}")
    req = result["requested"]
    rates = []
    for ev in result["events"]:
        if ev["event_ms"] > visible_ms:
            raise PairEvidenceError(f"funding event {ev['event_ms']} is after its visibility {visible_ms}")
        rec = {"v": RECORD_VERSION, "key": dict(key), "event_id": event_id("okx", key["instrument"], ev["event_ms"]), "rule_version": RULE_OKX,
               "event_ms": ev["event_ms"], "visible_ms": visible_ms, "rate": ev["realized_rate"], "provenance": provenance, "record_hash": ""}
        rates.append(seal(rec, "rate"))
    cov = {"v": RECORD_VERSION, "key": dict(key), "evidence_id": evidence_id, "requested_from_ms": req["from_ms"], "requested_through_ms": req["through_ms"],
           "proven": [dict(iv) for iv in result["proven"]], "gaps": [dict(g) for g in result["gaps"]], "visible_ms": visible_ms,
           "provenance": provenance, "record_hash": ""}
    return rates, seal(cov, "coverage")


def hyperliquid_window_records(key, records, from_ms, through_ms, visible_ms, provenance, evidence_id):
    if key["provider"] != "hyperliquid":
        raise PairEvidenceError("a Hyperliquid funding window needs a Hyperliquid member key")
    if isinstance(from_ms, bool) or isinstance(through_ms, bool) or not isinstance(from_ms, int) or not isinstance(through_ms, int) or from_ms < 0 or through_ms <= from_ms:
        raise PairEvidenceError(f"the Hyperliquid funding window ({from_ms}, {through_ms}] is empty or negative")
    rates = []
    prev = None
    for r in records:
        t = r["time"]
        if isinstance(t, bool) or not isinstance(t, int) or t <= from_ms or t > through_ms:
            raise PairEvidenceError(f"Hyperliquid funding record {t!r} is outside ({from_ms}, {through_ms}]")
        if prev is not None and t <= prev:
            raise PairEvidenceError("Hyperliquid funding records are not strictly ascending")
        if t > visible_ms:
            raise PairEvidenceError(f"funding event {t} is after its visibility {visible_ms}")
        prev = t
        rec = {"v": RECORD_VERSION, "key": dict(key), "event_id": event_id("hyperliquid", key["instrument"], t), "rule_version": RULE_HYPERLIQUID,
               "event_ms": t, "visible_ms": visible_ms, "rate": r["rate"], "provenance": provenance, "record_hash": ""}
        rates.append(seal(rec, "rate"))
    proven, gaps = [], []
    through = from_ms
    if rates:
        through = min(prev, through_ms)
        proven.append({"from_ms": from_ms, "through_ms": through})
    if through < through_ms:
        gaps.append({"from_ms": through, "through_ms": through_ms, "reason": "after_last_record"})
    cov = {"v": RECORD_VERSION, "key": dict(key), "evidence_id": evidence_id, "requested_from_ms": from_ms, "requested_through_ms": through_ms,
           "proven": proven, "gaps": gaps, "visible_ms": visible_ms, "provenance": provenance, "record_hash": ""}
    return rates, seal(cov, "coverage")


def mark_price_record(key, at_ms, price, visible_ms, provenance, evidence_id):
    if at_ms > visible_ms:
        raise PairEvidenceError(f"mark price at {at_ms} is after its visibility {visible_ms}")
    rec = {"v": RECORD_VERSION, "key": dict(key), "evidence_id": evidence_id, "price_kind": "mark", "at_ms": at_ms, "visible_ms": visible_ms,
           "price": price, "provenance": provenance, "record_hash": ""}
    return seal(rec, "price")


__all__ = [
    "BATCH_VERSION", "COVERAGE_VERSION", "DOCUMENT_VERSION", "EVENT_VERSION", "RECORD_VERSION", "RULES", "RULE_HYPERLIQUID", "RULE_OKX",
    "JSONNumber", "batch_bytes", "batch_hash", "check_records", "decode_document", "decode_event", "document_bytes", "event_hash",
    "event_id", "funding_amounts", "hyperliquid_window_records", "mark_price_record", "okx_range_records", "record_hash", "seal", "seal_batch",
    "source_hash",
]
