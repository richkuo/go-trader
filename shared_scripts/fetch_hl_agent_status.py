#!/usr/bin/env python3

import argparse
import json
import os
import re
import sys

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "platforms", "hyperliquid"))

from adapter import hyperliquid_info_api, hyperliquid_signer

ADDRESS_RE = re.compile(r"^0x[0-9a-fA-F]{40}$")
LONG_HEX_RE = re.compile(r"(?i)(0x)?[0-9a-f]{64,}")
LONG_DIGITS_RE = re.compile(r"\d{60,}")
ERROR_LIMIT = 300


def _network():
    return "testnet" if os.environ.get("HYPERLIQUID_TESTNET", "") == "1" else "mainnet"


def _doc(**fields):
    doc = {
        "signer_address": "",
        "master_address": "",
        "network": _network(),
        "role": None,
        "role_user": None,
        "valid_until_ms": None,
        "agent_name": None,
        "listed_in_extra_agents": False,
        "error": "",
    }
    doc.update(fields)
    return doc


def _secret_forms(secret):
    forms = set()
    raw = (secret or "").strip()
    if not raw:
        return forms
    body = raw[2:] if raw[:2].lower() == "0x" else raw
    for value in (raw, body):
        if len(value) >= 8:
            forms.update({value, value.lower(), value.upper()})
    try:
        forms.add(str(int(body, 16)))
    except ValueError:
        pass
    return {f for f in forms if len(f) >= 8}


def _redact(text, secret):
    out = str(text)
    for form in sorted(_secret_forms(secret), key=len, reverse=True):
        out = out.replace(form, "[redacted]")
    out = LONG_HEX_RE.sub("[redacted]", out)
    out = LONG_DIGITS_RE.sub("[redacted]", out)
    if len(out) > ERROR_LIMIT:
        out = out[:ERROR_LIMIT]
    return out


def _emit(doc, secret=""):
    if doc.get("error"):
        doc["error"] = _redact(doc["error"], secret)
    print(json.dumps(doc))
    sys.exit(1 if doc.get("error") else 0)


def _post(api, payload, secret, label):
    try:
        return api.post("/info", payload), ""
    except Exception as exc:
        return None, _redact(f"{label} request failed: {type(exc).__name__}: {exc}", secret)


def _reply_error(reply):
    if isinstance(reply, dict) and isinstance(reply.get("error"), str):
        return reply["error"]
    return ""


def _valid_until(value):
    if isinstance(value, bool):
        return None
    if isinstance(value, int):
        return value if value > 0 else None
    if isinstance(value, str) and value.isdigit():
        parsed = int(value)
        return parsed if parsed > 0 else None
    return None


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--timeout", type=float, default=6.0)
    parser.add_argument("--probe-only", action="store_true")
    args = parser.parse_args()

    if args.probe_only:
        _emit(_doc())

    if not args.timeout or args.timeout <= 0:
        _emit(_doc(error="--timeout must be > 0"))

    secret = os.environ.get("HYPERLIQUID_SECRET_KEY", "").strip()
    if not secret:
        _emit(_doc(error="HYPERLIQUID_SECRET_KEY is not set"))

    configured = os.environ.get("HYPERLIQUID_ACCOUNT_ADDRESS", "").strip()
    if configured and not ADDRESS_RE.match(configured):
        _emit(_doc(error="HYPERLIQUID_ACCOUNT_ADDRESS is not 0x followed by 40 hex characters"), secret)

    try:
        wallet, master = hyperliquid_signer(secret, configured)
        signer = str(wallet.address)
    except BaseException:
        _emit(_doc(error="HYPERLIQUID_SECRET_KEY is not a valid private key"), secret)

    base = {"signer_address": signer, "master_address": str(master)}

    try:
        api = hyperliquid_info_api(args.timeout)
    except Exception as exc:
        _emit(_doc(**base, error=f"info client unavailable: {type(exc).__name__}: {exc}"), secret)

    reply, err = _post(api, {"type": "userRole", "user": signer}, secret, "userRole")
    if err:
        _emit(_doc(**base, error=err), secret)
    if not isinstance(reply, dict):
        _emit(_doc(**base, error=f"userRole reply is not an object: {type(reply).__name__}"), secret)
    if _reply_error(reply):
        _emit(_doc(**base, error=f"userRole reply error: {_reply_error(reply)}"), secret)
    role = reply.get("role")
    if not isinstance(role, str) or not role:
        _emit(_doc(**base, error="userRole reply has no role"), secret)

    if role != "agent":
        _emit(_doc(**base, role=role), secret)

    data = reply.get("data")
    role_user = data.get("user") if isinstance(data, dict) else None
    if not isinstance(role_user, str) or not ADDRESS_RE.match(role_user):
        _emit(_doc(**base, role=role, error="userRole agent reply has no master address"), secret)

    agents, err = _post(api, {"type": "extraAgents", "user": role_user}, secret, "extraAgents")
    if err:
        _emit(_doc(**base, role=role, role_user=role_user, error=err), secret)
    if _reply_error(agents):
        _emit(_doc(**base, role=role, role_user=role_user, error=f"extraAgents reply error: {_reply_error(agents)}"), secret)
    if not isinstance(agents, list):
        _emit(_doc(**base, role=role, role_user=role_user, error=f"extraAgents reply is not a list: {type(agents).__name__}"), secret)

    entry = None
    for item in agents:
        if isinstance(item, dict) and isinstance(item.get("address"), str) and item["address"].lower() == signer.lower():
            entry = item
            break
    if entry is None:
        _emit(_doc(**base, role=role, role_user=role_user), secret)

    valid_until = _valid_until(entry.get("validUntil"))
    if valid_until is None:
        _emit(_doc(**base, role=role, role_user=role_user, listed_in_extra_agents=True, error="extraAgents entry has no positive validUntil"), secret)
    name = entry.get("name")
    _emit(_doc(
        **base,
        role=role,
        role_user=role_user,
        valid_until_ms=valid_until,
        agent_name=name if isinstance(name, str) else "",
        listed_in_extra_agents=True,
    ), secret)


if __name__ == "__main__":
    try:
        main()
    except SystemExit:
        raise
    except BaseException as exc:
        _emit(_doc(error=f"unexpected failure: {type(exc).__name__}: {exc}"), os.environ.get("HYPERLIQUID_SECRET_KEY", ""))
