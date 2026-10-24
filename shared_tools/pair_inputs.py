import base64
import re
from fractions import Fraction

from pair_evidence import (
    ACCOUNT_FIELDS,
    CAPABILITY_FIELDS,
    CONVERSION_FIELDS,
    KEY_FIELDS,
    MAX_ENVELOPE_BYTES,
    JSONNumber,
    PairEvidenceError,
    _shape,
    account_matches,
    canonical_bytes,
    capability_matches,
    conversion_matches,
    decimal_fraction,
    eligible_at,
    float_decimal,
    fraction_decimal,
    is_canonical_decimal,
    loads_strict,
    required_conversion_currencies,
    sha256_hex,
)

OBSERVATION_VERSION = 2
ENVELOPE_VERSION = 2

SOURCE_SEALED = "sealed_feed"
SOURCE_ARCHIVE = "archive"
SOURCE_PUBLIC = "public_snapshot"

CONTRACT_HL_SEALED = "hyperliquid_ws_l2Book"
CONTRACT_HL_ARCHIVE = "hyperliquid_archive_l2_v1"
CONTRACT_HL_PUBLIC = "hyperliquid_public_l2_snapshot_v1"
CONTRACT_OKX_PUBLIC = "okx_public_books_snapshot_v1"

FEED_CONTRACT = "hyperliquid-l2book-native-slow/1635-v1"
FEED_TIME_SEMANTICS = "source_event_ms+first_receipt_ms/1635-v1"
FEED_IDENTITY_SEMANTICS = "sha256-lp(host,namespace,coin,event_ms,levels)/1635-v1"
ARCHIVE_PAYLOAD_TIME_BASIS = "archiver_receipt_ns_floor_ms/1635-v1"
TIME_SEMANTICS_HL_PUBLIC = "hyperliquid_l2_time_ms+reader_receipt_ms/1871-v1"
TIME_SEMANTICS_OKX_PUBLIC = "okx_books_ts_ms+reader_receipt_ms/1871-v1"
TIME_SEMANTICS_ARCHIVE = "archiver_receipt_ms+modeled_delivery_ms/1871-v1"
IDENTITY_SEMANTICS_PUBLIC = "sha256(canonical_payload)/1871-v1"

HL_PUBLIC_HOST = "https://api.hyperliquid.xyz"
OKX_PUBLIC_HOST = "https://www.okx.com"

COVERAGE_COMPLETE = "complete"
COVERAGE_INCOMPLETE = "incomplete"
GAP_SILENCE = "silence"

MAX_LEVELS = 20
MAX_OBSERVATIONS = 4096
MAX_WINDOW_MS = 900_000
MAX_MARKERS = 64
MAX_FUTURE_SKEW_MS = 250
MAX_LAG_MS = 5_000
MAX_GAP_MS = 10_000
MAX_ORDERS = 4_294_967_295
DECIMAL_MAX_LEN = 32
DECIMAL_MAX_FRAC = 18
MAX_INSTANCE_LEN = 128
MAX_REASON_LEN = 240
SNAPSHOT_ID_MAX_LEN = 256
FEED_STATUS_MAX_LEN = 64
MARKER_REASONS = frozenset({"epoch_start", "subscribed", "subscription_failed", "disconnected", "invalid_frame", "conflict",
                            "out_of_order", "clock_regression", "silence", "evidence_loss", "depth_raised"})

PUBLIC_LEVEL_BYTES = 160
WINDOW_LEVEL_BYTES = 96
WINDOW_RECORD_BYTES = 256
WINDOW_MARKER_BYTES = 1792
WINDOW_HEADER_BYTES = 2048
PUBLIC_HEADER_BYTES = 512

INT64_MAX = (1 << 63) - 1
INT64_MIN = -(1 << 63)

_HEX64 = re.compile(r"^[0-9a-f]{64}$")
_DECIMAL_TEXT = re.compile(r"^[0-9]+(\.[0-9]+)?$")
_ORDER_COUNT = re.compile(r"^(0|[1-9][0-9]{0,9})$")
_VENUE_MS = re.compile(r"^[1-9][0-9]{0,18}$")
_INTEGER = re.compile(r"^-?(0|[1-9][0-9]*)$")

SOURCE_SPECS = {
    ("hyperliquid", SOURCE_SEALED): (CONTRACT_HL_SEALED, FEED_TIME_SEMANTICS, FEED_IDENTITY_SEMANTICS, False),
    ("hyperliquid", SOURCE_ARCHIVE): (CONTRACT_HL_ARCHIVE, TIME_SEMANTICS_ARCHIVE, FEED_IDENTITY_SEMANTICS, False),
    ("hyperliquid", SOURCE_PUBLIC): (CONTRACT_HL_PUBLIC, TIME_SEMANTICS_HL_PUBLIC, IDENTITY_SEMANTICS_PUBLIC, True),
    ("okx", SOURCE_PUBLIC): (CONTRACT_OKX_PUBLIC, TIME_SEMANTICS_OKX_PUBLIC, IDENTITY_SEMANTICS_PUBLIC, True),
}

NATIVE_LEVEL_FIELDS = (("px", "str"), ("qty", "str"))
BASE_LEVEL_FIELDS = (("px", "str"), ("base_qty", "str"))
GAP_FIELDS = (("from_ms", "int"), ("through_ms", "int"), ("reason", "str"))
COVERAGE_FIELDS = (
    ("source_contract", "str"), ("time_semantics", "str"), ("identity_semantics", "str"), ("from_ms", "int"), ("through_ms", "int"),
    ("required_depth", "int"), ("required_window_ms", "int"), ("required_capacity", "int"), ("prefix_complete", "bool"),
    ("status", "str"), ("gaps", ("list", GAP_FIELDS)),
)
OBSERVATION_FIELDS = (
    ("v", "int"), ("key", KEY_FIELDS), ("source", "str"), ("provenance", "str"), ("venue_time_ms", "nullable_int"),
    ("first_receipt_ms", "int"), ("modeled_delivery_ms", "int"), ("source_cutoff_ms", "int"), ("snapshot_id", "str"),
    ("generation", "uint"), ("coverage", COVERAGE_FIELDS), ("native_bids", ("list", NATIVE_LEVEL_FIELDS)),
    ("native_asks", ("list", NATIVE_LEVEL_FIELDS)), ("bids", ("list", BASE_LEVEL_FIELDS)), ("asks", ("list", BASE_LEVEL_FIELDS)),
    ("native_unit", "str"), ("contract_size", "str"), ("price_currency", "str"), ("normalization_id", "str"),
    ("payload", "bytes"), ("payload_hash", "str"),
)
MEMBER_FIELDS = (("key", KEY_FIELDS), ("observation", OBSERVATION_FIELDS), ("capability", CAPABILITY_FIELDS),
                 ("conversions", ("list", CONVERSION_FIELDS)), ("account", ACCOUNT_FIELDS))
ENVELOPE_FIELDS = (
    ("v", "int"), ("partition", "str"), ("pair_id", "str"), ("contract_fingerprint", "str"), ("slot_ms", "int"), ("start_ms", "int"),
    ("deadline_ms", "int"), ("cutoff_ms", "int"), ("snapshot_id", "str"), ("generation", "uint"), ("members", ("list", MEMBER_FIELDS)),
)


def _fail(msg):
    raise PairEvidenceError(msg)


def _add_ms(a, b):
    out = a + b
    if out > INT64_MAX or out < INT64_MIN:
        return None
    return out


def age_within(reference_ms, receipt_ms, limit_ms):
    return receipt_ms <= reference_ms and limit_ms >= 0 and reference_ms - receipt_ms <= limit_ms


def skew_within(a_ms, b_ms, limit_ms):
    return abs(a_ms - b_ms) <= limit_ms


def _kind(node):
    if node is None:
        return "null"
    if isinstance(node, bool):
        return "boolean"
    if isinstance(node, str):
        return "string"
    if isinstance(node, (int, JSONNumber)):
        return "number"
    if isinstance(node, list):
        return "array"
    if isinstance(node, dict):
        return "object"
    return type(node).__name__


def _obj(node, path, required, optional=()):
    if not isinstance(node, dict):
        _fail(f"{path} must be a JSON object, got {_kind(node)}")
    for k in required:
        if k not in node:
            _fail(f"{path}.{k} is missing")
    allowed = set(required) | set(optional)
    extra = sorted(k for k in node if k not in allowed)
    if extra:
        _fail(f"{path} has unknown field {extra[0]!r}")
    return node


def _str(node, path):
    if not isinstance(node, str):
        _fail(f"{path} must be a JSON string, got {_kind(node)}")
    return node


def _int(node, path, lo, hi):
    if isinstance(node, JSONNumber):
        if not _INTEGER.match(node.text):
            _fail(f"{path} must be a JSON integer, got {node.text}")
        value = int(node.text)
    elif isinstance(node, int) and not isinstance(node, bool):
        value = node
    else:
        _fail(f"{path} must be a JSON integer, got {_kind(node)}")
    if value < INT64_MIN or value > INT64_MAX or value < lo or value > hi:
        _fail(f"{path} {value} is outside [{lo}, {hi}]")
    return value


def _bool(node, path):
    if not isinstance(node, bool):
        _fail(f"{path} must be a JSON boolean, got {_kind(node)}")
    return node


def _arr(node, path, max_len):
    if not isinstance(node, list):
        _fail(f"{path} must be a JSON array, got {_kind(node)}")
    if len(node) > max_len:
        _fail(f"{path} holds {len(node)} entries, above {max_len}")
    return node


def canonical_book_decimal(raw, path):
    if not isinstance(raw, str) or not 0 < len(raw.encode("utf-8")) <= DECIMAL_MAX_LEN:
        _fail(f"{path}: decimal is empty or longer than {DECIMAL_MAX_LEN}")
    int_part, dot, frac = raw.partition(".")
    if int_part == "" or (dot and frac == "") or "." in frac:
        _fail(f"{path}: decimal {raw!r} is malformed")
    if any(c != "." and not ("0" <= c <= "9") for c in raw):
        _fail(f"{path}: decimal {raw!r} is not a plain positive ASCII decimal")
    int_part = int_part.lstrip("0") or "0"
    frac = frac.rstrip("0")
    if len(frac) > DECIMAL_MAX_FRAC:
        _fail(f"{path}: decimal {raw!r} has more than {DECIMAL_MAX_FRAC} fractional digits")
    out = int_part + ("." + frac if frac else "")
    if out == "0":
        _fail(f"{path}: decimal {raw!r} is not positive")
    return out


def _level(px_raw, sz_raw, path, canonical_only=False):
    px = canonical_book_decimal(px_raw, path + " px")
    sz = canonical_book_decimal(sz_raw, path + " sz")
    if canonical_only and (px != px_raw or sz != sz_raw):
        _fail(f"{path} decimal is not canonical")
    return (Fraction(px), Fraction(sz), {"px": px, "qty": sz})


def _check_sides(bids, asks, depth, path):
    if len(bids) < depth or len(asks) < depth:
        _fail(f"{path} holds {len(bids)} bids and {len(asks)} asks, below the required depth {depth}")
    for i in range(1, len(bids)):
        if bids[i][0] >= bids[i - 1][0]:
            _fail(f"{path} bids are not strictly descending at {i}")
    for i in range(1, len(asks)):
        if asks[i][0] <= asks[i - 1][0]:
            _fail(f"{path} asks are not strictly ascending at {i}")
    if bids[0][0] >= asks[0][0]:
        _fail(f"{path} is crossed or locked")
    return [dict(b[2]) for b in bids[:depth]], [dict(a[2]) for a in asks[:depth]]


def _point_coverage(spec, need, receipt_ms):
    return {"source_contract": spec[0], "time_semantics": spec[1], "identity_semantics": spec[2], "from_ms": receipt_ms,
            "through_ms": receipt_ms, "required_depth": need["depth"], "required_window_ms": 0, "required_capacity": 1,
            "prefix_complete": True, "status": COVERAGE_COMPLETE, "gaps": []}


def _check_venue_time(venue_ms, receipt_ms, path):
    if venue_ms - receipt_ms > MAX_FUTURE_SKEW_MS:
        _fail(f"{path} venue time {venue_ms} is more than {MAX_FUTURE_SKEW_MS}ms after its receipt {receipt_ms}")
    if receipt_ms - venue_ms > MAX_LAG_MS:
        _fail(f"{path} venue time {venue_ms} lags its receipt {receipt_ms} by more than {MAX_LAG_MS}ms")


def _derive_hl_public(spec, member, need, tree, receipt_ms):
    root = _obj(tree, "payload", ("request", "response"))
    req = _obj(root["request"], "payload.request", ("coin", "type"))
    if req["coin"] != member["instrument"] or req["type"] != "l2Book":
        _fail("payload.request does not name the member instrument l2Book")
    resp = _obj(root["response"], "payload.response", ("coin", "levels", "time"))
    if _str(resp["coin"], "payload.response.coin") != member["instrument"]:
        _fail("payload.response.coin is not the member instrument")
    venue = _int(resp["time"], "payload.response.time", 1, INT64_MAX)
    sides = _arr(resp["levels"], "payload.response.levels", 2)
    if len(sides) != 2:
        _fail(f"payload.response.levels holds {len(sides)} sides, want 2")
    parsed = [[], []]
    for s, side in enumerate(sides):
        path = f"payload.response.levels[{s}]"
        for i, lv in enumerate(_arr(side, path, MAX_LEVELS)):
            lp = f"{path}[{i}]"
            o = _obj(lv, lp, ("n", "px", "sz"))
            _int(o["n"], lp + ".n", 1, MAX_ORDERS)
            parsed[s].append(_level(_str(o["px"], lp + ".px"), _str(o["sz"], lp + ".sz"), lp))
    bids, asks = _check_sides(parsed[0], parsed[1], need["depth"], "payload book")
    _check_venue_time(venue, receipt_ms, "payload")
    return {"provenance": spec[0] + "@" + HL_PUBLIC_HOST, "venue_time_ms": venue, "first_receipt_ms": receipt_ms,
            "source_cutoff_ms": receipt_ms, "snapshot_id": "", "coverage": _point_coverage(spec, need, receipt_ms), "bids": bids, "asks": asks}


def _derive_okx_public(spec, member, need, tree, receipt_ms):
    root = _obj(tree, "payload", ("request", "response"))
    req = _obj(root["request"], "payload.request", ("instId", "sz"))
    if req["instId"] != member["instrument"]:
        _fail("payload.request.instId is not the member instrument")
    if req["sz"] != str(need["depth"]):
        _fail("payload.request.sz is not the required depth")
    resp = _obj(root["response"], "payload.response", ("code", "data", "msg"))
    if resp["code"] != "0":
        _fail("payload.response.code is not \"0\"")
    _str(resp["msg"], "payload.response.msg")
    data = _arr(resp["data"], "payload.response.data", 1)
    if len(data) != 1:
        _fail(f"payload.response.data holds {len(data)} books, want 1")
    book = _obj(data[0], "payload.response.data[0]", ("asks", "bids", "ts"))
    ts = _str(book["ts"], "payload.response.data[0].ts")
    if not _VENUE_MS.match(ts) or int(ts) > INT64_MAX:
        _fail(f"payload.response.data[0].ts {ts!r} is not a positive millisecond integer")
    venue = int(ts)
    parsed = [[], []]
    for s, name in enumerate(("bids", "asks")):
        path = "payload.response.data[0]." + name
        for i, lv in enumerate(_arr(book[name], path, MAX_LEVELS)):
            lp = f"{path}[{i}]"
            parts = _arr(lv, lp, 4)
            if len(parts) != 4:
                _fail(f"{lp} holds {len(parts)} fields, want [px, sz, liquidated, orders]")
            text = [_str(parts[j], f"{lp}[{j}]") for j in range(4)]
            if len(text[2].encode("utf-8")) > DECIMAL_MAX_LEN or not _DECIMAL_TEXT.match(text[2]):
                _fail(f"{lp} liquidated field {text[2]!r} is not a plain decimal")
            if not _ORDER_COUNT.match(text[3]):
                _fail(f"{lp} order count {text[3]!r} is not a plain integer")
            parsed[s].append(_level(text[0], text[1], lp))
    bids, asks = _check_sides(parsed[0], parsed[1], need["depth"], "payload book")
    _check_venue_time(venue, receipt_ms, "payload")
    return {"provenance": spec[0] + "@" + OKX_PUBLIC_HOST, "venue_time_ms": venue, "first_receipt_ms": receipt_ms,
            "source_cutoff_ms": receipt_ms, "snapshot_id": "", "coverage": _point_coverage(spec, need, receipt_ms), "bids": bids, "asks": asks}


WINDOW_FIELDS = ("available", "capacity", "coin", "contract", "cutoff_ms", "depth", "identity_semantics", "kind", "markers",
                 "max_future_skew_ms", "max_gap_ms", "max_lag_ms", "observations", "owner_instance", "prefix_complete",
                 "retained_from_ms", "snapshot_id", "source", "time_semantics", "window_ms")


def _printable(text, limit):
    return isinstance(text, str) and 0 < len(text) <= limit and all(0x20 <= ord(ch) <= 0x7E for ch in text)


def _valid_book_text(text, limit):
    return len(text.encode("utf-8")) <= limit and all(0x20 <= ord(ch) <= 0x7E for ch in text)


def _shift(value, delay, path):
    out = _add_ms(value, delay)
    if out is None:
        _fail(f"{path} {value} plus the modeled delivery {delay}ms overflows")
    return out


def _derive_window(spec, member, need, tree):
    root = _obj(tree, "payload", WINDOW_FIELDS, ("feed_status", "reason"))
    archive = spec[0] == CONTRACT_HL_ARCHIVE
    want_time = ARCHIVE_PAYLOAD_TIME_BASIS if archive else FEED_TIME_SEMANTICS
    for k, want in (("kind", "l2_book"), ("coin", member["instrument"]), ("source", CONTRACT_HL_SEALED), ("contract", FEED_CONTRACT),
                    ("time_semantics", want_time), ("identity_semantics", FEED_IDENTITY_SEMANTICS)):
        if _str(root[k], "payload." + k) != want:
            _fail(f"payload.{k} {root[k]!r} is not {want!r}")
    for k, want in (("max_future_skew_ms", MAX_FUTURE_SKEW_MS), ("max_lag_ms", MAX_LAG_MS), ("max_gap_ms", MAX_GAP_MS),
                    ("window_ms", need["window_ms"]), ("capacity", need["capacity"]), ("depth", need["depth"])):
        if _int(root[k], "payload." + k, INT64_MIN, INT64_MAX) != want:
            _fail(f"payload.{k} is not the required {want}")
    owner = _str(root["owner_instance"], "payload.owner_instance")
    snapshot_id = _str(root["snapshot_id"], "payload.snapshot_id")
    if "feed_status" in root and len(_str(root["feed_status"], "payload.feed_status").encode("utf-8")) > FEED_STATUS_MAX_LEN:
        _fail(f"payload.feed_status is longer than {FEED_STATUS_MAX_LEN} bytes")
    available = _bool(root["available"], "payload.available")
    if "reason" in root:
        reason = _str(root["reason"], "payload.reason")
        if not available:
            _fail(f"the book section is unavailable: {reason}")
        _fail(f"an available book section carries a refusal reason {reason!r}")
    if not available:
        _fail("the book section is unavailable")
    if owner == "" or not _valid_book_text(owner, MAX_INSTANCE_LEN):
        _fail(f"payload.owner_instance {owner!r} is not a bounded owner identity")
    if archive:
        if snapshot_id != "":
            _fail(f"an archive section carries snapshot_id {snapshot_id!r}")
    elif not _printable(snapshot_id, SNAPSHOT_ID_MAX_LEN):
        _fail(f"a sealed section needs a printable snapshot_id of at most {SNAPSHOT_ID_MAX_LEN} bytes")
    cutoff = _int(root["cutoff_ms"], "payload.cutoff_ms", 1, INT64_MAX)
    retained = _int(root["retained_from_ms"], "payload.retained_from_ms", 0, INT64_MAX)
    prefix = _bool(root["prefix_complete"], "payload.prefix_complete")
    start = cutoff - need["window_ms"]
    raw_obs = _arr(root["observations"], "payload.observations", need["capacity"])
    if not raw_obs:
        _fail("the book section holds no observation in its window")
    obs = []
    ids = set()
    for i, raw in enumerate(raw_obs):
        p = f"payload.observations[{i}]"
        o = _obj(raw, p, ("ask_depth", "asks", "bid_depth", "bids", "event_ms", "identity", "recv_ms", "seq", "session"))
        seq = _int(o["seq"], p + ".seq", 1, INT64_MAX)
        event = _int(o["event_ms"], p + ".event_ms", 1, INT64_MAX)
        recv = _int(o["recv_ms"], p + ".recv_ms", 1, INT64_MAX)
        _int(o["session"], p + ".session", 0, INT64_MAX)
        identity = _str(o["identity"], p + ".identity")
        if not _HEX64.match(identity) or identity in ids:
            _fail(f"{p}.identity is not a unique 64-character lowercase hex digest")
        ids.add(identity)
        depths = [_int(o["bid_depth"], p + ".bid_depth", 1, MAX_LEVELS), _int(o["ask_depth"], p + ".ask_depth", 1, MAX_LEVELS)]
        sides = [[], []]
        for s, name in enumerate(("bids", "asks")):
            levels = _arr(o[name], f"{p}.{name}", need["depth"])
            if len(levels) > depths[s] or not levels:
                _fail(f"{p}.{name} holds {len(levels)} levels, outside [1, its declared depth {depths[s]}]")
            for j, lv in enumerate(levels):
                lp = f"{p}.{name}[{j}]"
                parts = _arr(lv, lp, 3)
                if len(parts) != 3:
                    _fail(f"{lp} is not [px, sz, n]")
                px_raw = _str(parts[0], lp + " px")
                sz_raw = _str(parts[1], lp + " sz")
                _int(parts[2], lp + " n", 1, MAX_ORDERS)
                sides[s].append(_level(px_raw, sz_raw, lp, canonical_only=True))
        if recv < start or recv > cutoff:
            _fail(f"{p} receipt {recv} lies outside the window [{start}, {cutoff}]")
        _check_venue_time(event, recv, p)
        if obs and not (seq > obs[-1]["seq"] and event > obs[-1]["event"] and recv >= obs[-1]["recv"]):
            _fail(f"{p} is out of sequence, event or receipt order")
        obs.append({"seq": seq, "event": event, "recv": recv, "bids": sides[0], "asks": sides[1]})
    gaps = []
    last_seq = 0
    for i, raw in enumerate(_arr(root["markers"], "payload.markers", MAX_MARKERS)):
        p = f"payload.markers[{i}]"
        mk = _obj(raw, p, ("at_ms", "reason", "seq", "session"), ("detail",))
        seq = _int(mk["seq"], p + ".seq", 1, INT64_MAX)
        if i and seq <= last_seq:
            _fail(f"{p} is out of sequence")
        last_seq = seq
        at = _int(mk["at_ms"], p + ".at_ms", 1, INT64_MAX)
        if at < start or at > cutoff:
            _fail(f"{p} lies outside the window")
        _int(mk["session"], p + ".session", 0, INT64_MAX)
        reason = _str(mk["reason"], p + ".reason")
        if reason not in MARKER_REASONS:
            _fail(f"{p} reason {reason!r} is not a contract marker")
        if "detail" in mk:
            detail = _str(mk["detail"], p + ".detail")
            if detail == "" or len(detail.encode("utf-8")) > MAX_REASON_LEN:
                _fail(f"{p} detail is empty or longer than {MAX_REASON_LEN} bytes")
        gaps.append({"from_ms": at, "through_ms": at, "reason": reason})
    prev = start
    for o in obs:
        if o["recv"] - prev > MAX_GAP_MS:
            gaps.append({"from_ms": prev, "through_ms": o["recv"], "reason": GAP_SILENCE})
        prev = o["recv"]
    if cutoff - prev > MAX_GAP_MS:
        gaps.append({"from_ms": prev, "through_ms": cutoff, "reason": GAP_SILENCE})
    gaps.sort(key=lambda g: (g["from_ms"], g["through_ms"], g["reason"].encode("utf-8")))
    last = obs[-1]
    bids, asks = _check_sides(last["bids"], last["asks"], need["depth"], "payload latest observation")
    delay = member["modeled_delivery_ms"] if archive else 0
    receipt = _shift(last["recv"], delay, "receipt")
    shifted = [{"from_ms": _shift(g["from_ms"], delay, "gap start"), "through_ms": _shift(g["through_ms"], delay, "gap end"), "reason": g["reason"]}
               for g in gaps]
    status = COVERAGE_COMPLETE if prefix and not shifted else COVERAGE_INCOMPLETE
    cov = {"source_contract": spec[0], "time_semantics": spec[1], "identity_semantics": spec[2],
           "from_ms": _shift(retained, delay, "coverage start"), "through_ms": _shift(cutoff, delay, "coverage end"),
           "required_depth": need["depth"], "required_window_ms": need["window_ms"], "required_capacity": need["capacity"],
           "prefix_complete": prefix, "status": status, "gaps": shifted}
    return {"provenance": spec[0] + "@" + owner, "venue_time_ms": last["event"], "first_receipt_ms": receipt, "source_cutoff_ms": cutoff,
            "snapshot_id": snapshot_id, "coverage": cov, "bids": bids, "asks": asks}


def derive(spec, member, need, payload, reader_receipt_ms):
    tree = loads_strict(payload)
    if canonical_bytes(tree) != bytes(payload):
        _fail("payload bytes are not canonical")
    if spec[0] == CONTRACT_HL_PUBLIC:
        return _derive_hl_public(spec, member, need, tree, reader_receipt_ms)
    if spec[0] == CONTRACT_OKX_PUBLIC:
        return _derive_okx_public(spec, member, need, tree, reader_receipt_ms)
    if spec[0] in (CONTRACT_HL_SEALED, CONTRACT_HL_ARCHIVE):
        return _derive_window(spec, member, need, tree)
    _fail(f"source contract {spec[0]!r} has no derivation")


def _escaped_len(text):
    return 6 * len(text.encode("utf-8")) + 2


def payload_max_bytes(spec, member, need):
    inst = _escaped_len(member["instrument"])
    if spec[3]:
        return PUBLIC_HEADER_BYTES + 2 * inst + 2 * need["depth"] * PUBLIC_LEVEL_BYTES
    record = WINDOW_RECORD_BYTES + 2 * need["depth"] * WINDOW_LEVEL_BYTES
    return (WINDOW_HEADER_BYTES + inst + _escaped_len("x" * SNAPSHOT_ID_MAX_LEN) + _escaped_len("x" * MAX_INSTANCE_LEN)
            + need["capacity"] * record + MAX_MARKERS * WINDOW_MARKER_BYTES)


def _member_key(partition, member):
    return {"partition": partition, "provider": member["provider"], "account_key": member["account_key"],
            "instrument": member["instrument"], "member_id": member["member_id"]}


def _key_tuple(key):
    return tuple(key[name].encode("utf-8") for name, _ in KEY_FIELDS)


def _check_observation_source(obs, member, key, need):
    spec = SOURCE_SPECS.get((member["provider"], member["source"]))
    if spec is None:
        _fail(f"member {member['member_id']} source {member['source']!r} has no source contract")
    if obs["v"] != OBSERVATION_VERSION:
        _fail(f"observation version {obs['v']} is not supported")
    if obs["key"] != key:
        _fail("observation key is not the member key")
    if obs["source"] != member["source"]:
        _fail("observation source is not the frozen source")
    if obs["modeled_delivery_ms"] != member["modeled_delivery_ms"]:
        _fail("observation modeled delivery is not the frozen value")
    if obs["venue_time_ms"] is None:
        _fail(f"source contract {spec[0]} supplies a venue time, but the observation venue time is null")
    payload = base64.b64decode(obs["payload"].encode("ascii"), validate=True)
    if not payload or len(payload) > payload_max_bytes(spec, member, need):
        _fail(f"observation payload is {len(payload)} bytes, outside its bound")
    if obs["payload_hash"] != sha256_hex(payload):
        _fail("observation payload_hash does not match its payload bytes")
    d = derive(spec, member, need, payload, obs["first_receipt_ms"])
    if d["provenance"] != obs["provenance"]:
        _fail("observation provenance is not the payload provenance")
    for k in ("venue_time_ms", "first_receipt_ms", "source_cutoff_ms", "snapshot_id"):
        if obs[k] != d[k]:
            _fail(f"observation {k} disagrees with the payload")
    if obs["coverage"] != d["coverage"]:
        _fail("observation coverage disagrees with the payload coverage")
    if obs["native_bids"] != d["bids"] or obs["native_asks"] != d["asks"]:
        _fail("observation native levels disagree with the payload levels")
    if obs["coverage"]["status"] != COVERAGE_COMPLETE:
        _fail(f"{spec[0]} coverage is {obs['coverage']['status']}; a failed or partial book is never repaired")
    if member["source"] != SOURCE_SEALED and obs["generation"] != 0:
        _fail(f"a {member['source']} observation carries a snapshot generation")
    return spec


def _normalized_levels(levels, contract_size):
    out = []
    for lv in levels:
        if not is_canonical_decimal(lv["qty"]) or Fraction(lv["qty"]) <= 0:
            _fail(f"native quantity {lv['qty']!r} is not a positive canonical decimal")
        if not is_canonical_decimal(lv["px"]) or Fraction(lv["px"]) <= 0:
            _fail(f"native price {lv['px']!r} is not a positive canonical decimal")
        out.append({"px": lv["px"], "base_qty": fraction_decimal(Fraction(lv["qty"]) * contract_size)})
    return out


def _book_need(raw):
    need = {k: _int(raw.get(k), "book_need." + k, INT64_MIN, INT64_MAX) for k in ("depth", "window_ms", "capacity")}
    if not 0 < need["window_ms"] <= MAX_WINDOW_MS or not 0 < need["capacity"] <= MAX_OBSERVATIONS or not 0 < need["depth"] <= MAX_LEVELS:
        _fail(f"book need {need} is outside its bounds")
    return need


def check_envelope(expect, raw):
    limit = expect["limits"]["envelope_max_bytes"]
    if not isinstance(limit, int) or limit <= 0 or limit > MAX_ENVELOPE_BYTES:
        limit = MAX_ENVELOPE_BYTES
    if len(raw) > limit:
        _fail(f"quote envelope is {len(raw)} bytes, above the {limit} byte limit")
    tree = loads_strict(raw)
    if isinstance(tree, dict) and isinstance(tree.get("v"), JSONNumber) and tree["v"].text != str(ENVELOPE_VERSION):
        _fail(f"quote envelope version {tree['v'].text} is not supported (want {ENVELOPE_VERSION})")
    env = _shape(tree, ENVELOPE_FIELDS, "quote envelope", False)
    if canonical_bytes(env) != bytes(raw):
        _fail("quote envelope bytes are not canonical")
    if env["v"] != ENVELOPE_VERSION:
        _fail(f"quote envelope version {env['v']} is not supported")
    if env["partition"] != expect["partition"] or env["pair_id"] != expect["pair_id"] or env["contract_fingerprint"] != expect["contract_fingerprint"]:
        _fail("envelope identity is not the pair")
    need = expect["book_need"]
    limits = expect["limits"]
    deadline = _add_ms(env["start_ms"], limits["collection_timeout_ms"])
    if env["slot_ms"] <= 0 or env["start_ms"] < env["slot_ms"]:
        _fail("envelope slot is not positive or is after the collection start")
    if deadline is None or env["deadline_ms"] != deadline:
        _fail("envelope deadline is not the collection start plus the collection timeout")
    if env["cutoff_ms"] < env["start_ms"] or not env["cutoff_ms"] < env["deadline_ms"]:
        _fail("envelope cutoff is not in [start, deadline)")
    members = sorted(expect["members"], key=lambda m: _key_tuple(_member_key(expect["partition"], m)))
    if len(env["members"]) != len(members) or not members:
        _fail(f"envelope holds {len(env['members'])} members, the pair has {len(members)}")
    sealed = False
    receipts = []
    for i, m in enumerate(members):
        em = env["members"][i]
        key = _member_key(expect["partition"], m)
        if em["key"] != key:
            _fail(f"envelope member {i} key is not {key} (members must be complete and sorted by full key)")
        _check_observation_source(em["observation"], m, key, need)
        obs = em["observation"]
        if m["source"] == SOURCE_SEALED:
            sealed = True
            if obs["snapshot_id"] == "" or obs["snapshot_id"] != env["snapshot_id"] or obs["generation"] != env["generation"]:
                _fail(f"member {m['member_id']} sealed observation snapshot is not the captured snapshot")
        elif obs["snapshot_id"] != "":
            _fail(f"member {m['member_id']} {m['source']} observation carries a snapshot_id")
        if not age_within(env["cutoff_ms"], obs["first_receipt_ms"], limits["quote_age_ms"]):
            _fail(f"member {m['member_id']} receipt is after the cutoff or older than quote_age_ms")
        receipts.append(obs["first_receipt_ms"])
        cap = capability_matches(m, em["capability"])
        if cap["key"] != key or not eligible_at(cap, env["cutoff_ms"]):
            _fail(f"member {m['member_id']} capability is not bound to the member and eligible at the cutoff")
        size = decimal_fraction(cap["meta"]["contract_size"], "contract_size")
        if size <= 0:
            _fail("contract_size must be above zero")
        norm_id = cap["meta"]["native_unit"] + "*" + cap["meta"]["contract_size"] + "@" + cap["evidence_id"]
        if (obs["native_unit"], obs["contract_size"], obs["price_currency"], obs["normalization_id"]) != (
                cap["meta"]["native_unit"], cap["meta"]["contract_size"], cap["meta"]["price_currency"], norm_id):
            _fail(f"member {m['member_id']} normalization is not the capability normalization")
        if obs["bids"] != _normalized_levels(obs["native_bids"], size) or obs["asks"] != _normalized_levels(obs["native_asks"], size):
            _fail(f"member {m['member_id']} base levels disagree with the native levels times the verified contract size")
        acct = account_matches(m, em["account"])
        if acct["key"] != key or not eligible_at(acct, env["cutoff_ms"]):
            _fail(f"member {m['member_id']} account is not bound to the member and eligible at the cutoff")
        required = required_conversion_currencies(m)
        if len(em["conversions"]) != len(required):
            _fail(f"member {m['member_id']} carries {len(em['conversions'])} conversions, it needs exactly {required}")
        for j, conv in enumerate(em["conversions"]):
            if conv["currency"] != required[j]:
                _fail(f"member {m['member_id']} conversion {j} is {conv['currency']}, want {required[j]}")
            rec = conversion_matches(m, conv)
            if rec["key"] != key or not eligible_at(rec, env["cutoff_ms"]):
                _fail(f"member {m['member_id']} conversion is not bound to the member and eligible at the cutoff")
    if not sealed and (env["snapshot_id"] != "" or env["generation"] != 0):
        _fail("an envelope with no sealed member carries a snapshot")
    for r in receipts[1:]:
        if not skew_within(receipts[0], r, limits["receive_skew_ms"]):
            _fail("member receipts differ by more than receive_skew_ms")
    return env


def _plain(node):
    if isinstance(node, JSONNumber):
        return int(node.text) if _INTEGER.match(node.text) else float(node.text)
    if isinstance(node, list):
        return [_plain(x) for x in node]
    if isinstance(node, dict):
        return {k: _plain(v) for k, v in node.items()}
    return node


def expectation(request):
    if not isinstance(request, dict):
        _fail("the policy request is not an object")
    req = _plain({k: v for k, v in request.items() if k != "envelope"})
    for k in ("partition", "pair_id", "contract_fingerprint", "input_fingerprint"):
        _str(req.get(k), k)
    if not _HEX64.match(req["input_fingerprint"]):
        _fail("input_fingerprint is not lowercase SHA-256 hex")
    limits = req.get("limits")
    if not isinstance(limits, dict):
        _fail("limits is not an object")
    for k in ("quote_age_ms", "collection_timeout_ms", "receive_skew_ms", "envelope_max_bytes"):
        _int(limits.get(k), "limits." + k, 1, INT64_MAX)
    members = []
    for i, m in enumerate(_arr(req.get("members"), "members", 16)):
        if not isinstance(m, dict):
            _fail(f"members[{i}] is not an object")
        for k in ("member_id", "provider", "account_key", "instrument", "source", "source_contract"):
            _str(m.get(k), f"members[{i}].{k}")
        spec = SOURCE_SPECS.get((m["provider"], m["source"]))
        if spec is None or m["source_contract"] != spec[0]:
            _fail(f"members[{i}] source {m['source']!r} and source_contract {m['source_contract']!r} are not a compatible pair")
        _int(m.get("modeled_delivery_ms"), f"members[{i}].modeled_delivery_ms", 0, INT64_MAX)
        lev = m.get("leverage")
        if isinstance(lev, bool) or not isinstance(lev, (int, float)):
            _fail(f"members[{i}].leverage is not a number")
        m["leverage"] = float(lev)
        if not isinstance(m.get("meta"), dict):
            _fail(f"members[{i}].meta is not an object")
        members.append(m)
    return {"partition": req["partition"], "pair_id": req["pair_id"], "contract_fingerprint": req["contract_fingerprint"],
            "limits": limits, "book_need": _book_need(req.get("book_need") or {}), "members": members,
            "slot_ms": _int(req.get("slot_ms"), "slot_ms", 1, INT64_MAX), "cutoff_ms": _int(req.get("cutoff_ms"), "cutoff_ms", 1, INT64_MAX),
            "input_fingerprint": req["input_fingerprint"]}


def verify_envelope(request: dict) -> dict:
    expect = expectation(request)
    if "envelope" not in request:
        _fail("the policy request carries no envelope")
    raw = canonical_bytes(request["envelope"])
    if sha256_hex(raw) != expect["input_fingerprint"]:
        _fail("the embedded envelope's SHA-256 is not the request input_fingerprint")
    env = check_envelope(expect, raw)
    if env["slot_ms"] != expect["slot_ms"] or env["cutoff_ms"] != expect["cutoff_ms"]:
        _fail("the envelope slot or cutoff is not the request slot or cutoff")
    return env


def bound_crosses(envelope, member_id, side, bound):
    if isinstance(bound, bool) or not isinstance(bound, (int, float)) or bound != bound or bound in (float("inf"), float("-inf")) or bound <= 0:
        return False
    for m in envelope["members"]:
        if m["key"]["member_id"] != member_id:
            continue
        b = Fraction(float_decimal(bound))
        obs = m["observation"]
        if side == "buy" and obs["native_asks"]:
            return b >= Fraction(obs["native_asks"][0]["px"])
        if side == "sell" and obs["native_bids"]:
            return b <= Fraction(obs["native_bids"][0]["px"])
        return False
    return False
