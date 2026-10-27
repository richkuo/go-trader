import base64
import hashlib
import json
import math
import re
from fractions import Fraction

EVIDENCE_VERSION = 2

PRICE_RULE_HYPERLIQUID = "hyperliquid_perps_v1"
PRICE_RULE_FIXED_TICK = "fixed_tick_v1"

DERIVATION_HYPERLIQUID = "hyperliquid_info_meta_v1"
DERIVATION_OKX = "okx_public_instrument_v1"
DERIVATIONS = {"hyperliquid": DERIVATION_HYPERLIQUID, "okx": DERIVATION_OKX}

HYPERLIQUID_CURRENCY = "USDC"
HYPERLIQUID_MIN_NOTIONAL = "10"
OKX_CURRENCY = "USDT"
OKX_MIN_NOTIONAL = "0"
USD = "USD"
HL_MAX_SZ_DECIMALS = 12

MAX_ID_LEN = 128
MAX_PROVENANCE_LEN = 256
MAX_DOCUMENT_BYTES = 16 << 20
MAX_ENVELOPE_BYTES = 4 << 20
MAX_BASE64_TEXT = ((MAX_ENVELOPE_BYTES + 2) // 3) * 4

_CANONICAL_DECIMAL = re.compile(r"^-?(0|[1-9][0-9]*)(\.[0-9]*[1-9])?$")
_DECIMAL_INPUT = re.compile(r"^-?[0-9]+(\.[0-9]+)?([eE][-+]?[0-9]{1,4})?$")
_INTEGER = re.compile(r"^-?(0|[1-9][0-9]*)$")
_CURRENCY = re.compile(r"^[A-Z0-9]{2,16}$")
_HEX64 = re.compile(r"^[0-9a-f]{64}$")
_INT64_MAX = (1 << 63) - 1
_UINT64_MAX = (1 << 64) - 1


class PairEvidenceError(ValueError):
    pass


class JSONNumber:
    __slots__ = ("text",)

    def __init__(self, text):
        self.text = text

    def __eq__(self, other):
        return isinstance(other, JSONNumber) and other.text == self.text

    def __hash__(self):
        return hash(self.text)

    def __repr__(self):
        return f"JSONNumber({self.text!r})"


def _reject_constant(name):
    raise PairEvidenceError(f"JSON constant {name} is not allowed")


def _pairs_hook(pairs):
    out = {}
    for key, value in pairs:
        if key in out:
            raise PairEvidenceError(f"duplicate object key {key!r}")
        out[key] = value
    return out


def _check_surrogates(node):
    if isinstance(node, str):
        for ch in node:
            if 0xD800 <= ord(ch) <= 0xDFFF:
                raise PairEvidenceError("JSON string holds a lone surrogate escape")
    elif isinstance(node, list):
        for item in node:
            _check_surrogates(item)
    elif isinstance(node, dict):
        for key, value in node.items():
            _check_surrogates(key)
            _check_surrogates(value)


def loads_strict(data):
    if not isinstance(data, (bytes, bytearray)):
        raise PairEvidenceError("strict JSON input must be bytes")
    try:
        text = bytes(data).decode("utf-8")
    except UnicodeDecodeError as exc:
        raise PairEvidenceError(f"JSON is not valid UTF-8: {exc}") from exc
    try:
        tree = json.loads(
            text,
            object_pairs_hook=_pairs_hook,
            parse_int=JSONNumber,
            parse_float=JSONNumber,
            parse_constant=_reject_constant,
        )
    except json.JSONDecodeError as exc:
        raise PairEvidenceError(f"JSON is unreadable: {exc}") from exc
    _check_surrogates(tree)
    return tree


def _escape(text):
    out = ['"']
    for ch in text:
        code = ord(ch)
        if 0xD800 <= code <= 0xDFFF:
            raise PairEvidenceError("string holds a lone surrogate")
        if ch == '"':
            out.append('\\"')
        elif ch == "\\":
            out.append("\\\\")
        elif ch == "\n":
            out.append("\\n")
        elif ch == "\r":
            out.append("\\r")
        elif ch == "\t":
            out.append("\\t")
        elif code < 0x20 or ch in "<>&":
            out.append("\\u%04x" % code)
        elif code == 0x2028:
            out.append("\\u2028")
        elif code == 0x2029:
            out.append("\\u2029")
        else:
            out.append(ch)
    out.append('"')
    return "".join(out)


def _write(node, out):
    if node is None:
        out.append("null")
    elif node is True:
        out.append("true")
    elif node is False:
        out.append("false")
    elif isinstance(node, int):
        out.append(str(node))
    elif isinstance(node, JSONNumber):
        if not _INTEGER.match(node.text):
            raise PairEvidenceError(f"number {node.text} is not an integer; schema decimals are canonical decimal strings")
        out.append("0" if node.text == "-0" else node.text)
    elif isinstance(node, float):
        raise PairEvidenceError("binary floats are not hashable; schema decimals are canonical decimal strings")
    elif isinstance(node, str):
        out.append(_escape(node))
    elif isinstance(node, (list, tuple)):
        out.append("[")
        for i, item in enumerate(node):
            if i:
                out.append(",")
            _write(item, out)
        out.append("]")
    elif isinstance(node, dict):
        for key in node:
            if not isinstance(key, str):
                raise PairEvidenceError("object keys must be strings")
        out.append("{")
        for i, key in enumerate(sorted(node, key=lambda k: k.encode("utf-8", "surrogatepass"))):
            if i:
                out.append(",")
            out.append(_escape(key))
            out.append(":")
            _write(node[key], out)
        out.append("}")
    else:
        raise PairEvidenceError(f"unsupported JSON value {type(node).__name__}")


def canonical_bytes(obj):
    out = []
    _write(obj, out)
    return "".join(out).encode("utf-8")


def sha256_hex(data):
    return hashlib.sha256(data).hexdigest()


def fraction_decimal(value):
    if value == 0:
        return "0"
    den = value.denominator
    twos = fives = 0
    while den % 2 == 0:
        den //= 2
        twos += 1
    while den % 5 == 0:
        den //= 5
        fives += 1
    if den != 1:
        raise PairEvidenceError(f"{value} has no finite decimal form")
    digits = max(twos, fives)
    scaled = value * (10 ** digits)
    n = abs(scaled.numerator // scaled.denominator)
    sign = "-" if value < 0 else ""
    text = str(n)
    if digits:
        text = text.rjust(digits + 1, "0")
        text = text[:-digits] + "." + text[-digits:]
        text = text.rstrip("0").rstrip(".")
    return sign + text


def parse_decimal_input(text):
    if not isinstance(text, str) or not _DECIMAL_INPUT.match(text):
        raise PairEvidenceError(f"{text!r} is not a decimal number")
    return Fraction(text)


def normalize_decimal(text):
    return fraction_decimal(parse_decimal_input(text))


def is_canonical_decimal(text):
    return isinstance(text, str) and bool(_CANONICAL_DECIMAL.match(text)) and text != "-0"


def decimal_fraction(text, what="decimal"):
    if not is_canonical_decimal(text):
        raise PairEvidenceError(f"{what} {text!r} is not in canonical form")
    return Fraction(text)


def float_decimal(value):
    if isinstance(value, bool) or not isinstance(value, (int, float)) or not math.isfinite(value):
        raise PairEvidenceError(f"value {value!r} is not a finite number")
    return normalize_decimal(repr(float(value)))


KEY_FIELDS = (("partition", "str"), ("provider", "str"), ("account_key", "str"), ("instrument", "str"), ("member_id", "str"))

META_FIELDS = (
    ("native_unit", "str"), ("base_asset", "str"), ("contract_size", "decimal"),
    ("quote_currency", "str"), ("settlement_currency", "str"), ("price_currency", "str"),
    ("tick_step", "decimal"), ("lot_step", "decimal"), ("min_qty", "decimal"), ("min_notional", "decimal"),
    ("trading_status", "str"), ("linear", "bool"), ("max_leverage", "decimal"), ("price_rule", "str"),
    ("sz_decimals", "int"), ("tick_step_text", "str"), ("lot_step_text", "str"), ("fee_currencies", "str_list"),
)

STAMP_FIELDS = (
    ("v", "int"), ("key", KEY_FIELDS), ("evidence_id", "str"), ("evidence_hash", "str"),
    ("observed_ms", "int"), ("first_receipt_ms", "int"), ("valid_from_ms", "int"), ("valid_through_ms", "int"),
    ("provenance", "str"),
)

CAPABILITY_FIELDS = STAMP_FIELDS + (("meta", META_FIELDS), ("metadata_generation", "uint"))
CONVERSION_FIELDS = STAMP_FIELDS + (("currency", "str"), ("rate_usd", "decimal"))
ACCOUNT_FIELDS = STAMP_FIELDS + (
    ("position_mode", "str"), ("margin_mode", "str"), ("leverage", "decimal"), ("collateral_currency", "str"),
    ("equity", "decimal"), ("free_collateral", "decimal"), ("margin_used", "decimal"),
)

SCHEMAS = {"capability": CAPABILITY_FIELDS, "conversion": CONVERSION_FIELDS, "account": ACCOUNT_FIELDS}

DOCUMENT_FIELDS = (("v", "int"), ("capabilities", ("list", CAPABILITY_FIELDS)), ("accounts", ("list", ACCOUNT_FIELDS)), ("conversions", ("list", CONVERSION_FIELDS)))


def _int_value(node, path, unsigned=False):
    if isinstance(node, JSONNumber):
        if not _INTEGER.match(node.text):
            raise PairEvidenceError(f"{path} must be a JSON integer, got {node.text}")
        value = int(node.text)
    elif isinstance(node, int) and not isinstance(node, bool):
        value = node
    else:
        raise PairEvidenceError(f"{path} must be a JSON integer, got {type(node).__name__}")
    if unsigned:
        if value < 0 or value > _UINT64_MAX:
            raise PairEvidenceError(f"{path} integer {value} is out of range")
    elif value < -_INT64_MAX - 1 or value > _INT64_MAX:
        raise PairEvidenceError(f"{path} integer {value} is out of range")
    return value


def _shape(node, schema, path, normalize):
    if isinstance(schema, tuple) and schema and schema[0] == "list":
        if not isinstance(node, list):
            raise PairEvidenceError(f"{path} must be a JSON array")
        return [_shape(item, schema[1], f"{path}[{i}]", normalize) for i, item in enumerate(node)]
    if isinstance(schema, tuple) and schema and schema[0] == "map":
        if not isinstance(node, dict):
            raise PairEvidenceError(f"{path} must be a JSON object")
        return {name: _shape(value, schema[1], f"{path}.{name}", normalize) for name, value in node.items()}
    if isinstance(schema, tuple):
        if not isinstance(node, dict):
            raise PairEvidenceError(f"{path} must be a JSON object")
        names = [name for name, _ in schema]
        for name in names:
            if name not in node:
                raise PairEvidenceError(f"{path}.{name} is missing")
        extra = sorted(k for k in node if k not in names)
        if extra:
            raise PairEvidenceError(f"{path} has unknown field {extra[0]!r}")
        return {name: _shape(node[name], kind, f"{path}.{name}", normalize) for name, kind in schema}
    if schema == "str":
        if not isinstance(node, str):
            raise PairEvidenceError(f"{path} must be a JSON string")
        return node
    if schema == "bool":
        if not isinstance(node, bool):
            raise PairEvidenceError(f"{path} must be a JSON boolean")
        return node
    if schema == "int":
        return _int_value(node, path)
    if schema == "uint":
        return _int_value(node, path, unsigned=True)
    if schema == "nullable_int":
        if node is None:
            return None
        return _int_value(node, path)
    if schema == "bytes":
        if not isinstance(node, str):
            raise PairEvidenceError(f"{path} must be a JSON string of standard padded base64")
        if len(node) > MAX_BASE64_TEXT:
            raise PairEvidenceError(f"{path} base64 text is {len(node)} bytes, above the {MAX_BASE64_TEXT} byte bound")
        try:
            raw = base64.b64decode(node.encode("ascii"), validate=True)
        except (ValueError, UnicodeEncodeError) as exc:
            raise PairEvidenceError(f"{path} is not standard padded base64: {exc}") from exc
        if base64.b64encode(raw).decode("ascii") != node:
            raise PairEvidenceError(f"{path} is not canonical standard padded base64")
        return node
    if schema == "decimal":
        if not isinstance(node, str):
            raise PairEvidenceError(f"{path} must be a decimal string")
        if normalize:
            return normalize_decimal(node)
        if not is_canonical_decimal(node):
            raise PairEvidenceError(f"{path} decimal {node!r} is not in canonical form")
        return node
    if schema == "str_list":
        if not isinstance(node, list) or any(not isinstance(item, str) for item in node):
            raise PairEvidenceError(f"{path} must be a JSON array of strings")
        return sorted(node, key=lambda k: k.encode("utf-8")) if normalize else list(node)
    raise PairEvidenceError(f"{path} has unsupported schema {schema!r}")


def record_kind(record):
    if not isinstance(record, dict):
        raise PairEvidenceError("an evidence record must be an object")
    if "meta" in record:
        return "capability"
    if "rate_usd" in record:
        return "conversion"
    if "position_mode" in record:
        return "account"
    raise PairEvidenceError("the evidence record kind is unknown")


def normalize_record(record, kind=None):
    kind = kind or record_kind(record)
    return _shape(record, SCHEMAS[kind], kind, True)


def evidence_hash(record, kind=None):
    kind = kind or record_kind(record)
    body = dict(record)
    body["evidence_hash"] = ""
    return sha256_hex(canonical_bytes(normalize_record(body, kind)))


def seal_record(record, kind=None):
    kind = kind or record_kind(record)
    out = normalize_record(record, kind)
    out["evidence_hash"] = evidence_hash(out, kind)
    return out


def _key_tuple(key):
    return tuple(key[name].encode("utf-8") for name, _ in KEY_FIELDS)


def _stamp_order(rec):
    return (rec["first_receipt_ms"], rec["observed_ms"], rec["evidence_id"].encode("utf-8"))


def document_bytes(doc):
    norm = _shape(doc, DOCUMENT_FIELDS, "evidence document", True)
    norm["capabilities"].sort(key=lambda r: (_key_tuple(r["key"]),) + _stamp_order(r))
    norm["accounts"].sort(key=lambda r: (_key_tuple(r["key"]),) + _stamp_order(r))
    norm["conversions"].sort(key=lambda r: (_key_tuple(r["key"]), r["currency"].encode("utf-8")) + _stamp_order(r))
    return canonical_bytes(norm)


def _printable(text, limit):
    return isinstance(text, str) and 0 < len(text) <= limit and all(0x20 <= ord(ch) <= 0x7E for ch in text)


def validate_key(key):
    partition = key["partition"]
    if partition not in ("live", "paper") and not (partition.startswith("paper:") and partition[6:].strip() == partition[6:] and partition[6:]):
        raise PairEvidenceError(f"evidence key partition {partition!r} is not a canonical risk partition")
    if key["provider"] not in DERIVATIONS:
        raise PairEvidenceError(f"evidence key provider {key['provider']!r} has no pair venue")
    for name in ("account_key", "instrument", "member_id"):
        value = key[name]
        if not value or value.strip() != value:
            raise PairEvidenceError(f"evidence key {name} {value!r} must be a nonempty trimmed string")


def validate_stamp(rec, kind):
    if rec["v"] != EVIDENCE_VERSION:
        raise PairEvidenceError(f"{kind} evidence version {rec['v']} is not supported (want {EVIDENCE_VERSION})")
    validate_key(rec["key"])
    if not _printable(rec["evidence_id"], MAX_ID_LEN):
        raise PairEvidenceError(f"{kind} evidence_id {rec['evidence_id']!r} must be 1..{MAX_ID_LEN} printable ASCII characters")
    if not _printable(rec["provenance"], MAX_PROVENANCE_LEN):
        raise PairEvidenceError(f"{kind} provenance must be 1..{MAX_PROVENANCE_LEN} printable ASCII characters")
    times = [rec["observed_ms"], rec["first_receipt_ms"], rec["valid_from_ms"], rec["valid_through_ms"]]
    if any(t < 0 for t in times):
        raise PairEvidenceError(f"{kind} evidence {rec['evidence_id']} has a negative time")
    if rec["observed_ms"] > rec["first_receipt_ms"]:
        raise PairEvidenceError(f"{kind} evidence {rec['evidence_id']} was observed after its first receipt")
    if rec["valid_from_ms"] > rec["valid_through_ms"]:
        raise PairEvidenceError(f"{kind} evidence {rec['evidence_id']} has an invalid validity interval")
    if not _HEX64.match(rec["evidence_hash"]) or rec["evidence_hash"] != evidence_hash(rec, kind):
        raise PairEvidenceError(f"{kind} evidence {rec['evidence_id']} evidence_hash does not match its canonical bytes")


def decode_record(data, kind):
    tree = loads_strict(data)
    rec = _shape(tree, SCHEMAS[kind], kind, False)
    if canonical_bytes(rec) != bytes(data):
        raise PairEvidenceError(f"{kind} evidence bytes are not canonical")
    validate_stamp(rec, kind)
    return rec


def decode_document(data, expected_digest=None):
    if len(data) > MAX_DOCUMENT_BYTES:
        raise PairEvidenceError(f"evidence document is {len(data)} bytes, above the {MAX_DOCUMENT_BYTES} byte limit")
    if expected_digest is not None:
        if not isinstance(expected_digest, str) or not _HEX64.match(expected_digest):
            raise PairEvidenceError("the expected document digest must be lowercase SHA-256 hex")
        if sha256_hex(bytes(data)) != expected_digest:
            raise PairEvidenceError("evidence document digest is not the expected digest")
    tree = loads_strict(data)
    if isinstance(tree, dict) and isinstance(tree.get("v"), JSONNumber) and tree["v"].text != str(EVIDENCE_VERSION):
        raise PairEvidenceError(f"evidence document version {tree['v'].text} is not supported (want {EVIDENCE_VERSION})")
    doc = _shape(tree, DOCUMENT_FIELDS, "evidence document", False)
    if doc["v"] != EVIDENCE_VERSION:
        raise PairEvidenceError(f"evidence document version {doc['v']} is not supported (want {EVIDENCE_VERSION})")
    if document_bytes(doc) != bytes(data):
        raise PairEvidenceError("evidence document bytes are not canonical (compact sorted keys, canonical decimals, sorted schedules)")
    seen = set()
    for kind, plural in (("capability", "capabilities"), ("account", "accounts"), ("conversion", "conversions")):
        for rec in doc[plural]:
            validate_stamp(rec, kind)
            ident = (kind, _key_tuple(rec["key"]), rec["evidence_id"])
            if ident in seen:
                raise PairEvidenceError(f"{kind} evidence {rec['evidence_id']} appears twice")
            seen.add(ident)
    last = {}
    for rec in doc["capabilities"]:
        k = _key_tuple(rec["key"])
        prev = last.get(k)
        if prev is not None:
            if rec["metadata_generation"] < prev["metadata_generation"]:
                raise PairEvidenceError(f"capability evidence {rec['evidence_id']} metadata_generation goes backwards")
            if rec["metadata_generation"] == prev["metadata_generation"] and rec["meta"] != prev["meta"]:
                raise PairEvidenceError(f"capability evidence {rec['evidence_id']} changes meta without a new metadata_generation")
        last[k] = rec
    return doc


def _pow10_decimal(n):
    return fraction_decimal(Fraction(10) ** n)


def hyperliquid_tick_step(sz_decimals):
    return _pow10_decimal(-max(0, 6 - sz_decimals))


def validate_wire_meta(provider, meta):
    def bad(msg):
        raise PairEvidenceError(f"{provider} capability meta: {msg}")

    for name in ("contract_size", "tick_step", "lot_step", "max_leverage"):
        if decimal_fraction(meta[name], name) <= 0:
            bad(f"{name} {meta[name]} must be above zero")
    for name in ("min_qty", "min_notional"):
        if decimal_fraction(meta[name], name) < 0:
            bad(f"{name} {meta[name]} must not be negative")
    if not meta["base_asset"].strip() or not meta["trading_status"].strip():
        bad("base_asset and trading_status must be set")
    if meta["linear"] is not True:
        bad("linear must be true")
    fees = meta["fee_currencies"]
    if fees != sorted(fees, key=lambda k: k.encode("utf-8")):
        bad("fee_currencies must be a sorted list")
    if provider == "hyperliquid":
        sz = meta["sz_decimals"]
        if sz < 0 or sz > HL_MAX_SZ_DECIMALS:
            bad(f"sz_decimals {sz} is outside [0, {HL_MAX_SZ_DECIMALS}]")
        lot = _pow10_decimal(-sz)
        want = {
            "native_unit": (meta["native_unit"], "base"),
            "contract_size": (meta["contract_size"], "1"),
            "quote_currency": (meta["quote_currency"], HYPERLIQUID_CURRENCY),
            "settlement_currency": (meta["settlement_currency"], HYPERLIQUID_CURRENCY),
            "price_currency": (meta["price_currency"], HYPERLIQUID_CURRENCY),
            "price_rule": (meta["price_rule"], PRICE_RULE_HYPERLIQUID),
            "lot_step": (meta["lot_step"], lot),
            "min_qty": (meta["min_qty"], lot),
            "tick_step": (meta["tick_step"], hyperliquid_tick_step(sz)),
            "min_notional": (meta["min_notional"], HYPERLIQUID_MIN_NOTIONAL),
            "tick_step_text": (meta["tick_step_text"], ""),
            "lot_step_text": (meta["lot_step_text"], ""),
        }
        for name in sorted(want):
            if want[name][0] != want[name][1]:
                bad(f"{name} is {want[name][0]!r}, the derivation requires {want[name][1]!r}")
        if fees != [HYPERLIQUID_CURRENCY]:
            bad(f"fee_currencies {fees} must be [{HYPERLIQUID_CURRENCY}]")
        if decimal_fraction(meta["max_leverage"]).denominator != 1:
            bad(f"max_leverage {meta['max_leverage']} must be a whole number")
    elif provider == "okx":
        want = {
            "native_unit": (meta["native_unit"], "contract"),
            "quote_currency": (meta["quote_currency"], OKX_CURRENCY),
            "settlement_currency": (meta["settlement_currency"], OKX_CURRENCY),
            "price_currency": (meta["price_currency"], OKX_CURRENCY),
            "price_rule": (meta["price_rule"], PRICE_RULE_FIXED_TICK),
            "tick_step_text": (meta["tick_step_text"], meta["tick_step"]),
            "lot_step_text": (meta["lot_step_text"], meta["lot_step"]),
            "min_notional": (meta["min_notional"], OKX_MIN_NOTIONAL),
        }
        for name in sorted(want):
            if want[name][0] != want[name][1]:
                bad(f"{name} is {want[name][0]!r}, the derivation requires {want[name][1]!r}")
        if meta["sz_decimals"] != 0:
            bad(f"sz_decimals {meta['sz_decimals']} must be 0 (the fixed tick rule uses tick_step_text)")
        if fees != [meta["settlement_currency"]]:
            bad(f"fee_currencies {fees} must be [settlement_currency {meta['settlement_currency']}]")
    else:
        raise PairEvidenceError(f"capability meta provider {provider!r} has no pair venue")


def hyperliquid_wire_meta(instrument, rules, min_notional_usd):
    sz = rules["sz_decimals"]
    lev = rules["max_leverage"]
    if isinstance(sz, bool) or not isinstance(sz, int):
        raise PairEvidenceError(f"Hyperliquid szDecimals {sz!r} is not an integer")
    if isinstance(lev, bool) or not isinstance(lev, int) or lev <= 0:
        raise PairEvidenceError(f"Hyperliquid maxLeverage {lev!r} is not a positive integer")
    if rules["name"] != instrument:
        raise PairEvidenceError(f"Hyperliquid meta entry {rules['name']!r} is not {instrument!r}")
    lot = _pow10_decimal(-sz)
    meta = {
        "native_unit": "base",
        "base_asset": instrument,
        "contract_size": "1",
        "quote_currency": HYPERLIQUID_CURRENCY,
        "settlement_currency": HYPERLIQUID_CURRENCY,
        "price_currency": HYPERLIQUID_CURRENCY,
        "tick_step": hyperliquid_tick_step(sz),
        "lot_step": lot,
        "min_qty": lot,
        "min_notional": float_decimal(min_notional_usd),
        "trading_status": "delisted" if rules["is_delisted"] else "live",
        "linear": True,
        "max_leverage": str(lev),
        "price_rule": PRICE_RULE_HYPERLIQUID,
        "sz_decimals": sz,
        "tick_step_text": "",
        "lot_step_text": "",
        "fee_currencies": [HYPERLIQUID_CURRENCY],
    }
    validate_wire_meta("hyperliquid", meta)
    return meta


def okx_wire_meta(instrument, record):
    if record.get("instId") != instrument or record.get("instType") != "SWAP":
        raise PairEvidenceError(f"OKX instrument record {record.get('instId')!r} {record.get('instType')!r} is not the swap {instrument!r}")
    family = record.get("instFamily") or ""
    parts = family.split("-")
    if len(parts) != 2 or not all(parts):
        raise PairEvidenceError(f"OKX instFamily {family!r} is not <BASE>-<QUOTE>")
    base, quote = parts
    if record.get("ctValCcy") != base:
        raise PairEvidenceError(f"OKX ctValCcy {record.get('ctValCcy')!r} is not the family base {base!r}")
    state = record.get("state") or ""
    if not state:
        raise PairEvidenceError("OKX instrument state is missing")
    settle = record.get("settleCcy") or ""
    tick = normalize_decimal(record.get("tickSz"))
    lot = normalize_decimal(record.get("lotSz"))
    meta = {
        "native_unit": "contract",
        "base_asset": base,
        "contract_size": normalize_decimal(record.get("ctVal")),
        "quote_currency": quote,
        "settlement_currency": settle,
        "price_currency": quote,
        "tick_step": tick,
        "lot_step": lot,
        "min_qty": normalize_decimal(record.get("minSz")),
        "min_notional": OKX_MIN_NOTIONAL,
        "trading_status": "live" if state == "live" else state,
        "linear": record.get("ctType") == "linear",
        "max_leverage": normalize_decimal(record.get("lever")),
        "price_rule": PRICE_RULE_FIXED_TICK,
        "sz_decimals": 0,
        "tick_step_text": tick,
        "lot_step_text": lot,
        "fee_currencies": [settle],
    }
    validate_wire_meta("okx", meta)
    return meta


def expected_wire_meta(member):
    m = member["meta"]
    out = {
        "native_unit": m["native_unit"], "base_asset": m["base_asset"],
        "quote_currency": m["quote_currency"], "settlement_currency": m["settlement_currency"], "price_currency": m["price_currency"],
        "trading_status": m["trading_status"], "linear": m["linear"], "price_rule": m["price_rule"], "sz_decimals": m["sz_decimals"],
        "fee_currencies": sorted(m["fee_currencies"], key=lambda k: k.encode("utf-8")),
    }
    for name in ("contract_size", "tick_step", "lot_step", "min_qty", "min_notional", "max_leverage"):
        out[name] = float_decimal(m[name])
    for name in ("tick_step_text", "lot_step_text"):
        out[name] = normalize_decimal(m[name]) if m[name] else ""
    return out


def capability_matches(member, capability):
    rec = _shape(capability, CAPABILITY_FIELDS, "capability", False)
    validate_stamp(rec, "capability")
    _check_member_key(rec["key"], member, "capability", rec["evidence_id"])
    if rec["provenance"] != DERIVATIONS.get(member["provider"]):
        raise PairEvidenceError(f"capability evidence provenance {rec['provenance']!r} is not the {member['provider']} derivation")
    if rec["metadata_generation"] < 1:
        raise PairEvidenceError("capability evidence metadata_generation must be at least 1")
    validate_wire_meta(member["provider"], rec["meta"])
    want = expected_wire_meta(member)
    for name, _ in META_FIELDS:
        if rec["meta"][name] != want[name]:
            raise PairEvidenceError(f"capability evidence meta.{name} {rec['meta'][name]!r} differs from the frozen contract {want[name]!r}")
    return rec


def required_conversion_currencies(member):
    m = member["meta"]
    out = {m["settlement_currency"], m["price_currency"], *m["fee_currencies"]}
    out.discard(USD)
    return sorted(out, key=lambda k: k.encode("utf-8"))


def _check_member_key(key, member, what, ident):
    for name in ("provider", "account_key", "instrument", "member_id"):
        if key[name] != member[name]:
            raise PairEvidenceError(f"{what} evidence {ident} key {name} {key[name]!r} does not name member {member['member_id']}")


def account_matches(member, account):
    rec = _shape(account, ACCOUNT_FIELDS, "account", False)
    validate_stamp(rec, "account")
    _check_member_key(rec["key"], member, "account", rec["evidence_id"])
    m = member["meta"]
    if rec["position_mode"] != m["position_mode"]:
        raise PairEvidenceError(f"account evidence {rec['evidence_id']} position_mode {rec['position_mode']!r} differs from the required {m['position_mode']!r}")
    if rec["margin_mode"] != m["margin_mode"]:
        raise PairEvidenceError(f"account evidence {rec['evidence_id']} margin_mode {rec['margin_mode']!r} differs from the required {m['margin_mode']!r}")
    lev = decimal_fraction(rec["leverage"], "leverage")
    if lev <= 0:
        raise PairEvidenceError(f"account evidence {rec['evidence_id']} leverage must be above zero")
    if lev != Fraction(float_decimal(member["leverage"])):
        raise PairEvidenceError(f"account evidence {rec['evidence_id']} leverage {rec['leverage']} differs from the frozen selected leverage {member['leverage']!r}")
    if rec["collateral_currency"] != m["settlement_currency"]:
        raise PairEvidenceError(f"account evidence {rec['evidence_id']} collateral_currency differs from the settlement currency")
    decimal_fraction(rec["equity"], "equity")
    for name in ("free_collateral", "margin_used"):
        if decimal_fraction(rec[name], name) < 0:
            raise PairEvidenceError(f"account evidence {rec['evidence_id']} {name} {rec[name]} must not be negative")
    return rec


def conversion_matches(member, conversion):
    rec = _shape(conversion, CONVERSION_FIELDS, "conversion", False)
    validate_stamp(rec, "conversion")
    _check_member_key(rec["key"], member, "conversion", rec["evidence_id"])
    if not _CURRENCY.match(rec["currency"]) or rec["currency"] == USD:
        raise PairEvidenceError(f"conversion evidence {rec['evidence_id']} currency {rec['currency']!r} must be an upper-case currency other than USD")
    if rec["currency"] not in required_conversion_currencies(member):
        raise PairEvidenceError(f"conversion evidence {rec['evidence_id']} currency {rec['currency']} is not a settlement, price or fee currency of member {member['member_id']}")
    if decimal_fraction(rec["rate_usd"], "rate_usd") <= 0:
        raise PairEvidenceError(f"conversion evidence {rec['evidence_id']} rate_usd must be above zero")
    return rec


def eligible_at(rec, ref_ms):
    return rec["observed_ms"] <= ref_ms and rec["first_receipt_ms"] <= ref_ms and rec["valid_from_ms"] <= ref_ms <= rec["valid_through_ms"]


def _log10_floor(px):
    k = 0
    if px >= 1:
        while Fraction(10) ** (k + 1) <= px:
            k += 1
        return k
    while Fraction(10) ** k > px:
        k -= 1
    return k


def _px_decimals(px, sz_decimals):
    return min(max(0, 4 - _log10_floor(px)), max(0, 6 - sz_decimals))


def _floor_to(value, decimals):
    scale = Fraction(10) ** decimals
    scaled = value * scale
    return Fraction(scaled.numerator // scaled.denominator) / scale


def _ceil_to(value, decimals):
    scale = Fraction(10) ** decimals
    scaled = value * scale
    return Fraction(-((-scaled.numerator) // scaled.denominator)) / scale


def _hl_directed(px, sz_decimals, up):
    if px <= 0:
        raise PairEvidenceError("price must be > 0")
    d = _px_decimals(px, sz_decimals)
    out = _ceil_to(px, d) if up else _floor_to(px, d)
    if out <= 0:
        raise PairEvidenceError("price rounds to zero")
    d2 = _px_decimals(out, sz_decimals)
    if d2 < d:
        out = _ceil_to(out, d2) if up else _floor_to(out, d2)
    return out


def round_bound(member, capability, side, bound):
    rec = capability_matches(member, capability)
    if side not in ("buy", "sell"):
        raise PairEvidenceError(f"bound side {side!r} is neither buy nor sell")
    if isinstance(bound, bool) or not isinstance(bound, (int, float)) or not math.isfinite(bound) or bound <= 0:
        raise PairEvidenceError(f"bound {bound!r} must be finite and above zero")
    x = Fraction(float_decimal(bound))
    meta = rec["meta"]

    def rnd(px):
        if meta["price_rule"] == PRICE_RULE_HYPERLIQUID:
            return _hl_directed(px, meta["sz_decimals"], side == "sell")
        if meta["price_rule"] == PRICE_RULE_FIXED_TICK:
            tick = parse_decimal_input(meta["tick_step_text"])
            if tick <= 0:
                raise PairEvidenceError(f"tick_step_text {meta['tick_step_text']!r} is not a positive decimal")
            q = px / tick
            n = q.numerator // q.denominator
            if side == "sell" and Fraction(n) != q:
                n += 1
            return Fraction(n) * tick
        raise PairEvidenceError(f"price rule {meta['price_rule']!r} is not supported")

    r = rnd(x)
    if r <= 0:
        raise PairEvidenceError(f"bound {bound!r} rounds to zero")
    f = r.numerator / r.denominator
    if not math.isfinite(f) or f <= 0:
        raise PairEvidenceError(f"rounded bound {r} has no finite positive float")
    back = Fraction(float_decimal(f))
    if back != r:
        raise PairEvidenceError(f"rounded bound {r} does not survive float projection exactly")
    if rnd(back) != back:
        raise PairEvidenceError(f"projected bound {f!r} is not a legal venue price")
    if (side == "buy" and back > x) or (side == "sell" and back < x):
        raise PairEvidenceError(f"projected bound {f!r} would widen the {side} bound {bound!r}")
    return f
