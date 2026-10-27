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
RESEARCH_DOCUMENT_VERSION = 2
RESEARCH_BATCH_VERSION = 2
RESEARCH_EVENT_VERSION = 3
RESEARCH_BINDING_VERSION = 2
OBSERVATION_VERSION = 2
POLICY_VERSION = 1
MAX_RECORDS = 1 << 16
MAX_INTERVALS = 1 << 12

RULE_HYPERLIQUID = "hyperliquid_hourly_oracle_v1"
RULE_OKX = "okx_swap_realized_mark_v1"
RULE_HL_RECEIPT_RESEARCH = "hyperliquid_hourly_oracle_receipt_research_v1"
PRICE_ORACLE_RECEIPT = "oracle_receipt_research"
OBSERVATION_TIME_BASIS = "local_request_receipt"
ORACLE_PROVENANCE = "hyperliquid_info_metaAndAssetCtxs_oraclePx_receipt_research_v1@https://api.hyperliquid.xyz"

RULES = {
    RULE_HYPERLIQUID: {"provider": "hyperliquid", "currency": "USDC", "price_kind": "oracle", "assessment_window_ms": 0},
    RULE_OKX: {"provider": "okx", "currency": "USDT", "price_kind": "mark", "assessment_window_ms": 60000},
}
RESEARCH_RULES = {
    RULE_HL_RECEIPT_RESEARCH: {
        "rule_version": RULE_HL_RECEIPT_RESEARCH,
        "provider": "hyperliquid",
        "currency": "USDC",
        "rate_field": "fundingHistory.fundingRate",
        "price_kind": PRICE_ORACLE_RECEIPT,
        "formula": "native_amount = -(signed_base_qty * native_mark * rate)",
        "same_time": "inventory strictly before event_ms; a fill at event_ms is excluded",
        "assessment_window_ms": 0,
        "schedule": "each venue settlement record is one event; research oracle selection floors event_ms to the UTC hour within the frozen maximum print offset",
        "rounding": "none: the exact decimal product is kept",
        "evidence": ["research receipt-time observation; timing bounds do not establish venue settlement equality or oracle-update age"],
    },
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

POLICY_FIELDS = (
    ("version", "int"), ("member_rules", ("map", "str")), ("window_ms", "int"),
    ("max_print_offset_ms", "int"), ("prepare_lead_ms", "int"), ("from_ms", "int"), ("drain_through_ms", "int"),
)
OBSERVATION_FIELDS = (
    ("v", "int"), ("key", KEY_FIELDS), ("evidence_id", "str"), ("attempt_id", "str"),
    ("policy_digest", "str"), ("price_kind", "str"), ("target_event_ms", "int"),
    ("request_ms", "int"), ("receipt_ms", "int"), ("at_ms", "int"), ("visible_ms", "int"),
    ("time_basis", "str"), ("price", "decimal"), ("provenance", "str"), ("record_hash", "str"),
)
DOCUMENT_V2_FIELDS = (
    ("v", "int"), ("source_id", "str"), ("policy", POLICY_FIELDS), ("policy_digest", "str"),
    ("rates", ("list", RATE_FIELDS)), ("prices", ("list", PRICE_FIELDS)),
    ("observations", ("list", OBSERVATION_FIELDS)), ("coverage", ("list", COVERAGE_RECORD_FIELDS)),
)
BATCH_V2_FIELDS = (
    ("v", "int"), ("source_id", "str"), ("seq", "int"), ("prev_hash", "str"),
    ("policy_digest", "str"), ("rates", ("list", RATE_FIELDS)), ("prices", ("list", PRICE_FIELDS)),
    ("observations", ("list", OBSERVATION_FIELDS)), ("coverage", ("list", COVERAGE_RECORD_FIELDS)),
    ("batch_hash", "str"),
)
EVENT_V3_FIELDS = EVENT_FIELDS[:-1] + (
    ("policy_digest", "str"), ("observation_evidence_id", "str"),
    ("observation_record_hash", "str"), ("observation_target_event_ms", "int"),
    ("observation_request_ms", "int"), ("observation_receipt_ms", "int"),
    ("observation_time_basis", "str"), ("evidence_hash", "str"),
)
BINDING_FIELDS = (
    ("v", "int"), ("contract_fingerprint", "str"), ("members", ("list", KEY_FIELDS)),
    ("source_kind", "str"), ("source_id", "str"), ("digest", "str"), ("anchor_ms", "int"),
    ("rules_digest", "str"), ("rules", ("list", "str")),
)
BINDING_V2_FIELDS = BINDING_FIELDS + (("policy", POLICY_FIELDS), ("policy_digest", "str"))

SCHEMAS = {"rate": RATE_FIELDS, "price": PRICE_FIELDS, "coverage": COVERAGE_RECORD_FIELDS, "observation": OBSERVATION_FIELDS}


def _printable(text, limit):
    return isinstance(text, str) and 0 < len(text) <= limit and all(0x20 <= ord(ch) <= 0x7E for ch in text)


def _key_tuple(key):
    return tuple(key[name].encode("utf-8") for name, _ in KEY_FIELDS)


def event_id(provider, instrument, event_ms):
    return f"{provider}:{instrument}:{int(event_ms)}"


def policy_digest(policy):
    norm = _shape(policy, POLICY_FIELDS, "funding policy", True)
    return sha256_hex(canonical_bytes(norm))


def validate_policy(policy):
    p = _shape(policy, POLICY_FIELDS, "funding policy", False)
    if p["version"] != POLICY_VERSION or p["window_ms"] != 5000 or p["max_print_offset_ms"] != 60000 or p["prepare_lead_ms"] != 30000:
        raise PairEvidenceError("funding policy must be version 1 with window_ms 5000, max_print_offset_ms 60000 and prepare_lead_ms 30000")
    if p["from_ms"] <= 0 or p["drain_through_ms"] < p["from_ms"]:
        raise PairEvidenceError("funding policy bounds are invalid")
    if not p["member_rules"]:
        raise PairEvidenceError("funding policy member_rules is empty")
    for member_id, version in p["member_rules"].items():
        if not _printable(member_id, MAX_ID_LEN):
            raise PairEvidenceError(f"funding policy member id {member_id!r} is invalid")
        rule = RESEARCH_RULES.get(version)
        if rule is None:
            rule = RULES.get(version)
        if rule is None:
            raise PairEvidenceError(f"funding policy member {member_id} selects unsupported rule {version!r}")
    return p


def research_target(event_ms, policy):
    p = validate_policy(policy)
    if isinstance(event_ms, bool) or not isinstance(event_ms, int) or event_ms <= 0:
        return None
    target = event_ms - event_ms % 3600000
    if target <= 0 or not 0 <= event_ms - target <= p["max_print_offset_ms"]:
        return None
    if target < p["from_ms"] or target > p["drain_through_ms"]:
        return None
    return target


def oracle_attempt_id(key, target_event_ms, digest):
    body = {"key": key, "target_event_ms": target_event_ms, "policy_digest": digest}
    return "hyperliquid-oracle-attempt:" + sha256_hex(canonical_bytes(body))


def oracle_evidence_id(key, target_event_ms, receipt_ms):
    return f"hyperliquid-oracle:{key['instrument']}:{target_event_ms}:{receipt_ms}"


def validate_observation(observation, policy):
    o = _shape(observation, OBSERVATION_FIELDS, "funding oracle observation", False)
    p = validate_policy(policy)
    what = f"funding oracle observation {o['evidence_id']}"
    if o["v"] != OBSERVATION_VERSION or o["price_kind"] != PRICE_ORACLE_RECEIPT or o["time_basis"] != OBSERVATION_TIME_BASIS or o["provenance"] != ORACLE_PROVENANCE:
        raise PairEvidenceError(f"{what} has an unsupported version, price kind, time basis or provenance")
    validate_key(o["key"])
    digest = policy_digest(p)
    if o["policy_digest"] != digest:
        raise PairEvidenceError(f"{what} policy digest does not match the admitted policy")
    if o["attempt_id"] != oracle_attempt_id(o["key"], o["target_event_ms"], digest):
        raise PairEvidenceError(f"{what} attempt identity is invalid")
    target = o["target_event_ms"]
    if target <= 0 or target % 3600000 or target < p["from_ms"] or target > p["drain_through_ms"]:
        raise PairEvidenceError(f"{what} target is outside the policy hour grid")
    deadline = min(target + p["window_ms"], p["drain_through_ms"])
    if o["request_ms"] < target or o["receipt_ms"] < o["request_ms"] or o["receipt_ms"] > deadline or o["at_ms"] != o["receipt_ms"] or o["visible_ms"] != o["receipt_ms"]:
        raise PairEvidenceError(f"{what} local request/receipt times are outside [{target}, {deadline}]")
    if o["evidence_id"] != oracle_evidence_id(o["key"], target, o["receipt_ms"]) or not _printable(o["evidence_id"], MAX_ID_LEN):
        raise PairEvidenceError(f"{what} evidence identity is invalid")
    if decimal_fraction(o["price"], "price") <= 0:
        raise PairEvidenceError(f"{what} price must be above zero")
    if not _HEX64.match(o["record_hash"]) or o["record_hash"] != record_hash(o, "observation"):
        raise PairEvidenceError(f"{what} record_hash does not match its canonical bytes")
    return o


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


def check_records(rates, prices, coverage, prior=None, observations=None, policy=None):
    observations = observations or []
    if len(rates) > MAX_RECORDS or len(prices) > MAX_RECORDS or len(coverage) > MAX_RECORDS or len(observations) > MAX_RECORDS:
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
    oseen = set()
    for o in observations:
        if policy is None:
            raise PairEvidenceError("funding observations require an admitted policy")
        validate_observation(o, policy)
        ident = (_key_tuple(o["key"]), o["evidence_id"])
        if ident in oseen:
            raise PairEvidenceError(f"funding observation {o['evidence_id']} appears twice")
        oseen.add(ident)
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
    if "observations" in doc:
        doc["observations"].sort(key=lambda o: (_key_tuple(o["key"]), o["target_event_ms"], o["receipt_ms"], o["evidence_id"].encode("utf-8"), o["record_hash"].encode("utf-8")))


def _version(tree, what):
    if not isinstance(tree, dict):
        raise PairEvidenceError(f"{what} must be an object")
    value = tree.get("v")
    if isinstance(value, JSONNumber):
        if not re.fullmatch(r"-?(0|[1-9][0-9]*)", value.text):
            raise PairEvidenceError(f"{what} v must be an integer")
        return int(value.text)
    if isinstance(value, bool) or not isinstance(value, int):
        raise PairEvidenceError(f"{what} v must be an integer")
    return value


def document_bytes(doc):
    version = _version(doc, "funding document")
    schema = DOCUMENT_FIELDS if version == DOCUMENT_VERSION else DOCUMENT_V2_FIELDS if version == RESEARCH_DOCUMENT_VERSION else None
    if schema is None:
        raise PairEvidenceError(f"funding document version {version} is not supported")
    norm = _shape(doc, schema, "funding document", True)
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
    version = _version(tree, "funding document")
    schema = DOCUMENT_FIELDS if version == DOCUMENT_VERSION else DOCUMENT_V2_FIELDS if version == RESEARCH_DOCUMENT_VERSION else None
    if schema is None:
        raise PairEvidenceError(f"funding document version {version} is not supported")
    doc = _shape(tree, schema, "funding document", False)
    if not _printable(doc["source_id"], MAX_ID_LEN):
        raise PairEvidenceError(f"funding document source_id must be 1..{MAX_ID_LEN} printable ASCII characters")
    if document_bytes(doc) != bytes(data):
        raise PairEvidenceError("funding document bytes are not canonical (compact sorted keys, canonical decimals, records sorted by key and time)")
    policy = None
    observations = None
    if version == RESEARCH_DOCUMENT_VERSION:
        policy = validate_policy(doc["policy"])
        if doc["policy_digest"] != policy_digest(policy):
            raise PairEvidenceError("funding document policy digest is invalid")
        observations = doc["observations"]
    check_records(doc["rates"], doc["prices"], doc["coverage"], observations=observations, policy=policy)
    return doc


def _batch_schema(version):
    if version == BATCH_VERSION:
        return BATCH_FIELDS
    if version == RESEARCH_BATCH_VERSION:
        return BATCH_V2_FIELDS
    raise PairEvidenceError(f"funding batch version {version} is not supported")


def batch_hash(batch):
    norm = _shape(batch, _batch_schema(_version(batch, "funding batch")), "funding batch", True)
    _sort_lists(norm)
    norm["batch_hash"] = ""
    return sha256_hex(canonical_bytes(norm))


def seal_batch(batch):
    norm = _shape(batch, _batch_schema(_version(batch, "funding batch")), "funding batch", True)
    _sort_lists(norm)
    norm["batch_hash"] = batch_hash(norm)
    return norm


def batch_bytes(batch):
    norm = _shape(batch, _batch_schema(_version(batch, "funding batch")), "funding batch", True)
    _sort_lists(norm)
    return canonical_bytes(norm)


def decode_batch(data):
    tree = loads_strict(data)
    batch = _shape(tree, _batch_schema(_version(tree, "funding batch")), "funding batch", False)
    if batch_bytes(batch) != bytes(data):
        raise PairEvidenceError("funding batch bytes are not canonical")
    if not _printable(batch["source_id"], MAX_ID_LEN) or batch["seq"] < 1 or (batch["seq"] == 1) != (batch["prev_hash"] == "") or (batch["prev_hash"] and not _HEX64.match(batch["prev_hash"])):
        raise PairEvidenceError("funding batch source, sequence or previous hash is invalid")
    if not _HEX64.match(batch["batch_hash"]) or batch["batch_hash"] != batch_hash(batch):
        raise PairEvidenceError("funding batch hash does not match its canonical bytes")
    if batch["v"] == RESEARCH_BATCH_VERSION and not _HEX64.match(batch["policy_digest"]):
        raise PairEvidenceError("funding batch policy digest is invalid")
    return batch


def decode_binding(data):
    tree = loads_strict(data)
    version = _version(tree, "funding binding")
    schema = BINDING_FIELDS if version == 1 else BINDING_V2_FIELDS if version == RESEARCH_BINDING_VERSION else None
    if schema is None:
        raise PairEvidenceError(f"funding binding version {version} is not supported")
    binding = _shape(tree, schema, "funding binding", False)
    if canonical_bytes(binding) != bytes(data):
        raise PairEvidenceError("funding binding bytes are not canonical")
    if version == RESEARCH_BINDING_VERSION:
        policy = validate_policy(binding["policy"])
        if binding["policy_digest"] != policy_digest(policy):
            raise PairEvidenceError("funding binding policy digest is invalid")
    return binding


def funding_amounts(qty, price, rate, rate_usd):
    native = -(Fraction(qty) * Fraction(price) * Fraction(rate))
    usd = native * Fraction(rate_usd)
    return fraction_decimal(native), fraction_decimal(usd)


def event_hash(event):
    version = _version(event, "funding event")
    schema = EVENT_FIELDS if version == EVENT_VERSION else EVENT_V3_FIELDS if version == RESEARCH_EVENT_VERSION else None
    if schema is None:
        raise PairEvidenceError(f"funding event version {version} is not supported")
    norm = _shape(event, schema, "funding event", True)
    norm["evidence_hash"] = ""
    return sha256_hex(canonical_bytes(norm))


def decode_event(data):
    tree = loads_strict(data)
    version = _version(tree, "funding event")
    schema = EVENT_FIELDS if version == EVENT_VERSION else EVENT_V3_FIELDS if version == RESEARCH_EVENT_VERSION else None
    if schema is None:
        raise PairEvidenceError(f"funding event version {version} is not supported")
    ev = _shape(tree, schema, "funding event", False)
    if canonical_bytes(ev) != bytes(data):
        raise PairEvidenceError("funding event bytes are not canonical")
    what = f"funding event {ev['event_id']}"
    validate_key(ev["key"])
    if version == RESEARCH_EVENT_VERSION:
        if ev["rule_version"] != RULE_HL_RECEIPT_RESEARCH or not _HEX64.match(ev["policy_digest"]) or not _printable(ev["observation_evidence_id"], MAX_ID_LEN) or not _HEX64.match(ev["observation_record_hash"]) or ev["observation_time_basis"] != OBSERVATION_TIME_BASIS:
            raise PairEvidenceError(f"{what} has an invalid research identity")
        if (ev["price_hash"] != ev["observation_record_hash"] or ev["observation_target_event_ms"] <= 0 or ev["observation_target_event_ms"] % 3600000
                or ev["observation_receipt_ms"] < ev["observation_request_ms"] or ev["observation_request_ms"] < ev["observation_target_event_ms"]
                or ev["observation_receipt_ms"] > ev["visible_ms"]):
            raise PairEvidenceError(f"{what} has invalid observation evidence")
    if ev["event_id"] != event_id(ev["key"]["provider"], ev["key"]["instrument"], ev["event_ms"]) or ev["event_ms"] <= 0:
        raise PairEvidenceError(f"{what} event_id is not the venue identity")
    if ev["visible_ms"] < ev["event_ms"]:
        raise PairEvidenceError(f"{what} visible_ms is before event_ms")
    rule = RESEARCH_RULES.get(ev["rule_version"]) if version == RESEARCH_EVENT_VERSION else RULES.get(ev["rule_version"])
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


def hyperliquid_window_records(key, records, from_ms, through_ms, visible_ms, provenance, evidence_id, rule_version=RULE_HYPERLIQUID):
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
        rec = {"v": RECORD_VERSION, "key": dict(key), "event_id": event_id("hyperliquid", key["instrument"], t), "rule_version": rule_version,
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
    "BATCH_VERSION", "COVERAGE_VERSION", "DOCUMENT_VERSION", "EVENT_VERSION", "RECORD_VERSION", "RULES", "RESEARCH_RULES",
    "RULE_HYPERLIQUID", "RULE_OKX", "RULE_HL_RECEIPT_RESEARCH", "OBSERVATION_VERSION", "POLICY_VERSION", "PRICE_ORACLE_RECEIPT",
    "OBSERVATION_TIME_BASIS", "ORACLE_PROVENANCE", "POLICY_FIELDS", "OBSERVATION_FIELDS", "DOCUMENT_V2_FIELDS", "BATCH_V2_FIELDS",
    "EVENT_V3_FIELDS", "BINDING_V2_FIELDS", "JSONNumber", "batch_bytes", "batch_hash", "check_records", "decode_batch", "decode_binding",
    "decode_document", "decode_event", "document_bytes", "event_hash", "event_id", "funding_amounts", "hyperliquid_window_records",
    "mark_price_record", "okx_range_records", "oracle_attempt_id", "oracle_evidence_id", "policy_digest", "record_hash", "research_target",
    "seal", "seal_batch", "source_hash", "validate_observation", "validate_policy",
]
