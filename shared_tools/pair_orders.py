import importlib.util
import os
import re
import sys
import time
from decimal import Decimal, InvalidOperation

_HERE = os.path.dirname(os.path.abspath(__file__))

ACTION_REQUEST_VERSION = 2
OUTCOME_VERSION = 1
POSITION_REQUEST_VERSION = 1
POSITION_OUTPUT_VERSION = 1

PURPOSE_KIND = {"pair_entry": "increase", "pair_exit": "reduction", "pair_protective": "reduction"}

_REQUIRED = {
    "v": "int",
    "pair_id": "str",
    "partition": "str",
    "member_id": "str",
    "provider": "str",
    "account_key": "str",
    "instrument": "str",
    "client_order_id": "str",
    "purpose": "str",
    "order_side": "str",
    "position_side": "str",
    "base_qty": "num",
    "native_qty": "num",
    "contract_size": "num",
    "price_bound": "num",
    "accept_deadline_ms": "int",
    "origin_slot_ms": "int",
    "reduce_only": "bool",
    "contract_fingerprint": "str",
}
_OPTIONAL = {
    "quote_event_id": "str",
    "conversions": "list",
    "capability_hash": "str",
    "evidence_hash": "str",
    "reference_max_age_ms": "int",
    "reference_receipt_ms": "int",
    "reference_px": "str",
    "native_booked": "bool",
}
REFERENCE_FIELDS = ("reference_receipt_ms", "reference_max_age_ms", "reference_px")
_POSITION_FIELDS = {"v": "int", "provider": "str", "account_key": "str", "instrument": "str", "member_id": "str", "partition": "str"}

HL_ACCOUNT = re.compile(r"^hyperliquid:(mainnet|testnet):0x[0-9a-f]{40}$")
OKX_ACCOUNT = re.compile(r"^okx:swap:(live|sandbox):key-[0-9a-f]{16}$")
HL_CLOID = re.compile(r"^0x[0-9a-f]{32}$")
OKX_CLOID = re.compile(r"^[a-z0-9]{1,32}$")
HL_COIN = re.compile(r"^[A-Za-z0-9]{1,20}$")
OKX_INST = re.compile(r"^([A-Z0-9]{1,16})-USDT-SWAP$")
_FINGERPRINT = re.compile(r"^[0-9a-f]{64}$")
_INTEGER = re.compile(r"^-?(0|[1-9][0-9]*)$")


class PairOrderError(ValueError):
    pass


def _codec():
    name = "go_trader_pair_orders_codec"
    mod = sys.modules.get(name)
    if mod is not None:
        return mod
    spec = importlib.util.spec_from_file_location(name, os.path.join(_HERE, "pair_evidence.py"))
    mod = importlib.util.module_from_spec(spec)
    sys.modules[name] = mod
    spec.loader.exec_module(mod)
    return mod


def okx_account_key(inst_type):
    import hashlib
    api_key = os.environ.get("OKX_API_KEY", "").strip()
    if not api_key:
        return ""
    network = "sandbox" if os.environ.get("OKX_SANDBOX", "") == "1" else "live"
    fingerprint = hashlib.sha256(("go-trader/okx-api-key/v1:" + api_key).encode()).hexdigest()[:16]
    return f"okx:{inst_type}:{network}:key-{fingerprint}"


def now_ms():
    return int(time.time() * 1000)


def decimal_text(value):
    if not isinstance(value, Decimal):
        value = Decimal(str(value))
    if not value.is_finite():
        raise PairOrderError(f"decimal {value!r} is not finite")
    if value == 0:
        return "0"
    text = format(value.normalize(), "f")
    if "." in text:
        text = text.rstrip("0").rstrip(".")
    return text


def exact_decimal(raw, field, positive=False, allow_negative=False):
    if isinstance(raw, bool) or raw is None:
        raise PairOrderError(f"{field} is missing or not a decimal")
    try:
        value = Decimal(str(raw).strip())
    except (InvalidOperation, ValueError):
        raise PairOrderError(f"{field} {raw!r} is not a decimal")
    if not value.is_finite():
        raise PairOrderError(f"{field} {raw!r} is not finite")
    if positive and value <= 0:
        raise PairOrderError(f"{field} {raw!r} is not above zero")
    if not allow_negative and value < 0:
        raise PairOrderError(f"{field} {raw!r} is negative")
    return value


def _typed(node, kind, field):
    codec = _codec()
    if kind == "str":
        if not isinstance(node, str) or not node or node != node.strip():
            raise PairOrderError(f"{field} must be a nonempty trimmed JSON string")
        return node
    if kind == "bool":
        if not isinstance(node, bool):
            raise PairOrderError(f"{field} must be a JSON boolean")
        return node
    if kind == "int":
        if not isinstance(node, codec.JSONNumber) or not _INTEGER.match(node.text):
            raise PairOrderError(f"{field} must be a JSON integer")
        return int(node.text)
    if kind == "num":
        if not isinstance(node, codec.JSONNumber):
            raise PairOrderError(f"{field} must be a JSON number")
        return exact_decimal(node.text, field, allow_negative=True)
    if kind == "list":
        if not isinstance(node, list):
            raise PairOrderError(f"{field} must be a JSON array")
        return node
    if kind == "obj":
        if not isinstance(node, dict):
            raise PairOrderError(f"{field} must be a JSON object")
        return node
    raise PairOrderError(f"{field} has no type rule")


def _strict_object(raw, required, optional, what):
    codec = _codec()
    data = raw.encode("utf-8") if isinstance(raw, str) else raw
    try:
        tree = codec.loads_strict(data)
    except codec.PairEvidenceError as exc:
        raise PairOrderError(f"the {what} is not strict JSON: {exc}")
    if not isinstance(tree, dict):
        raise PairOrderError(f"the {what} must be a JSON object")
    unknown = sorted(k for k in tree if k not in required and k not in optional)
    if unknown:
        raise PairOrderError(f"the {what} carries unknown fields {unknown}")
    missing = sorted(k for k in required if k not in tree)
    if missing:
        raise PairOrderError(f"the {what} is missing fields {missing}")
    out = {}
    for k, kind in list(required.items()) + list(optional.items()):
        if k in tree:
            out[k] = _typed(tree[k], kind, k)
    return out


def parse_request(raw, provider, route="query"):
    req = _strict_object(raw, _REQUIRED, _OPTIONAL, "pair action request")
    if req["v"] != ACTION_REQUEST_VERSION:
        raise PairOrderError(f"request version {req['v']} is not {ACTION_REQUEST_VERSION}")
    if req["provider"] != provider:
        raise PairOrderError(f"the request names provider {req['provider']!r}, this route serves {provider!r}")
    kind = PURPOSE_KIND.get(req["purpose"])
    if kind is None:
        raise PairOrderError(f"purpose {req['purpose']!r} has no pair order route; native closes never use it")
    if req.get("native_booked"):
        raise PairOrderError("a native-booked request never uses the pair order route")
    if req["reduce_only"] != (kind == "reduction"):
        raise PairOrderError(f"purpose {req['purpose']} needs reduce_only={kind == 'reduction'}")
    side, pos = req["order_side"], req["position_side"]
    if side not in ("buy", "sell") or pos not in ("long", "short"):
        raise PairOrderError(f"order side {side!r} and position side {pos!r} are not buy/sell and long/short")
    if kind == "increase" and (side == "buy") != (pos == "long"):
        raise PairOrderError(f"an increase of a {pos} position cannot be a {side}")
    if kind == "reduction" and (side == "sell") != (pos == "long"):
        raise PairOrderError(f"a reduction of a {pos} position cannot be a {side}")
    for field in ("base_qty", "native_qty", "contract_size", "price_bound"):
        if req[field] <= 0:
            raise PairOrderError(f"{field} {req[field]} is not above zero")
    if req["native_qty"] * req["contract_size"] != req["base_qty"]:
        raise PairOrderError(f"base_qty {req['base_qty']} is not native_qty {req['native_qty']} times contract_size {req['contract_size']}")
    if req["accept_deadline_ms"] <= 0:
        raise PairOrderError("accept_deadline_ms must be above zero")
    if not _FINGERPRINT.match(req["contract_fingerprint"]):
        raise PairOrderError("contract_fingerprint is not a lowercase sha-256 digest")
    evidence = [k for k in ("quote_event_id", "capability_hash", "evidence_hash") if req.get(k)]
    if kind == "increase" and len(evidence) != 3:
        raise PairOrderError("an increase request needs its quote event, capability hash and evidence hash")
    if kind == "reduction" and evidence:
        raise PairOrderError(f"a reduction request carries increase evidence {evidence}")
    _check_reference_authority(req, kind, route)
    if provider == "hyperliquid":
        if req["contract_size"] != 1 or req["native_qty"] != req["base_qty"]:
            raise PairOrderError("a hyperliquid request has contract size 1 and native quantity equal to base quantity")
        if not HL_COIN.match(req["instrument"]):
            raise PairOrderError(f"instrument {req['instrument']!r} is not a hyperliquid coin")
        if not HL_ACCOUNT.match(req["account_key"]):
            raise PairOrderError(f"account {req['account_key']!r} is not hyperliquid:<network>:<address>")
        if not HL_CLOID.match(req["client_order_id"]):
            raise PairOrderError(f"client order id {req['client_order_id']!r} is not 0x plus 32 lowercase hex")
    elif provider == "okx":
        if not OKX_INST.match(req["instrument"]):
            raise PairOrderError(f"instrument {req['instrument']!r} is not <UNDERLYING>-USDT-SWAP")
        if not OKX_ACCOUNT.match(req["account_key"]):
            raise PairOrderError(f"account {req['account_key']!r} is not okx:swap:<network>:key-<16 hex>")
        if not OKX_CLOID.match(req["client_order_id"]):
            raise PairOrderError(f"client order id {req['client_order_id']!r} is not 1 to 32 lowercase letters or digits")
    else:
        raise PairOrderError(f"provider {provider!r} has no pair order route")
    return req


def _check_reference_authority(req, kind, route):
    present = [k for k in REFERENCE_FIELDS if k in req]
    if kind == "increase":
        if present:
            raise PairOrderError(f"an increase binds its quote event and carries no reduction reference {present}")
        return
    if not present:
        if route == "order":
            raise PairOrderError("a reduction order needs its saved reference authority (reference_receipt_ms, reference_max_age_ms, reference_px); an older request stays queryable but never authorizes a send")
        return
    if len(present) != len(REFERENCE_FIELDS):
        raise PairOrderError(f"the reduction reference authority is incomplete: {sorted(present)}")
    if req["reference_receipt_ms"] <= 0 or req["reference_max_age_ms"] <= 0:
        raise PairOrderError("reference_receipt_ms and reference_max_age_ms must be above zero")
    text = req["reference_px"]
    px = exact_decimal(text, "reference_px", positive=True)
    if decimal_text(px) != text:
        raise PairOrderError(f"reference_px {text!r} is not a canonical decimal")
    req["reference_px"] = px
    if req["order_side"] == "sell" and req["price_bound"] > px:
        raise PairOrderError(f"the sell bound {req['price_bound']} is above the saved reference best bid {text}, so it does not cross")
    if req["order_side"] == "buy" and req["price_bound"] < px:
        raise PairOrderError(f"the buy bound {req['price_bound']} is below the saved reference best ask {text}, so it does not cross")


def reference_refusal(req, now):
    if PURPOSE_KIND.get(req["purpose"]) != "reduction":
        return ""
    if "reference_receipt_ms" not in req:
        return "the reduction request has no saved reference authority; nothing is sent"
    receipt, limit = req["reference_receipt_ms"], req["reference_max_age_ms"]
    if receipt > now:
        return f"the saved reference receipt {receipt} is after the child time {now}; nothing is sent"
    if now - receipt > limit:
        return f"the saved reference received at {receipt} is older than reference_max_age_ms {limit} at the child time {now}; nothing is sent"
    return ""


def parse_position_request(raw, provider):
    req = _strict_object(raw, _POSITION_FIELDS, {}, "pair position request")
    if req["v"] != POSITION_REQUEST_VERSION:
        raise PairOrderError(f"position request version {req['v']} is not {POSITION_REQUEST_VERSION}")
    if req["provider"] != provider:
        raise PairOrderError(f"the position request names provider {req['provider']!r}, this route serves {provider!r}")
    if provider == "hyperliquid":
        if not HL_COIN.match(req["instrument"]) or not HL_ACCOUNT.match(req["account_key"]):
            raise PairOrderError("the hyperliquid position request needs a coin and a hyperliquid:<network>:<address> account")
    elif provider == "okx":
        if not OKX_INST.match(req["instrument"]) or not OKX_ACCOUNT.match(req["account_key"]):
            raise PairOrderError("the okx position request needs <UNDERLYING>-USDT-SWAP and an okx:swap:<network>:key-<16 hex> account")
    else:
        raise PairOrderError(f"provider {provider!r} has no pair position route")
    return req


def request_echo(req):
    return {
        "client_order_id": req["client_order_id"],
        "account_key": req["account_key"],
        "instrument": req["instrument"],
        "order_side": req["order_side"],
        "purpose": req["purpose"],
        "reduce_only": req["reduce_only"],
        "native_qty": decimal_text(req["native_qty"]),
        "price_bound": decimal_text(req["price_bound"]),
        "accept_deadline_ms": req["accept_deadline_ms"],
    }


def outcome(mode, status, detail, req=None, **fields):
    out = {
        "v": OUTCOME_VERSION,
        "mode": mode,
        "status": status,
        "detail": detail,
        "observed_at_ms": now_ms(),
        "terminal": False,
        "accepted_at_ms": 0,
        "cum_native_qty": "0",
        "cum_native_notional": "0",
        "fills": [],
        "fill_coverage": {"complete": False, "reason": "", "from_ms": 0, "through_ms": 0, "pages": 0},
        "venue": {},
    }
    if req is not None:
        out["request"] = request_echo(req)
    out.update(fields)
    return out


def never_sent(mode, detail, req=None):
    return outcome(mode, "never_sent", detail, req, terminal=True)


def fill_record(fill_id, venue_time_ms, native_qty, native_px, fee_amount, fee_currency, fee_known, order_id="", side=""):
    qty = exact_decimal(native_qty, f"fill {fill_id} size", positive=True)
    px = exact_decimal(native_px, f"fill {fill_id} price", positive=True)
    if not isinstance(fill_id, str) or not fill_id or len(fill_id) > 128 or not fill_id.isprintable():
        raise PairOrderError(f"fill id {fill_id!r} is not printable text of at most 128 characters")
    try:
        t = int(venue_time_ms)
    except (TypeError, ValueError):
        raise PairOrderError(f"fill {fill_id} has no venue time")
    if t <= 0:
        raise PairOrderError(f"fill {fill_id} has no venue time")
    rec = {
        "fill_id": fill_id,
        "venue_time_ms": t,
        "native_qty": decimal_text(qty),
        "native_px": decimal_text(px),
        "native_notional": decimal_text(qty * px),
        "fee_known": bool(fee_known),
        "fee_amount": "",
        "fee_currency": "",
        "order_id": str(order_id or ""),
        "side": str(side or ""),
    }
    if fee_known:
        rec["fee_amount"] = decimal_text(exact_decimal(fee_amount, f"fill {fill_id} fee", allow_negative=True))
        rec["fee_currency"] = str(fee_currency).strip().upper()
        if not rec["fee_currency"]:
            rec["fee_known"] = False
            rec["fee_amount"] = ""
    return rec


def sum_fills(fills):
    qty = Decimal(0)
    notional = Decimal(0)
    for f in fills:
        qty += Decimal(f["native_qty"])
        notional += Decimal(f["native_notional"])
    return qty, notional


def finish(out_doc, code=0):
    import json
    print(json.dumps(out_doc, sort_keys=True))
    sys.stdout.flush()
    if code:
        sys.exit(code)
