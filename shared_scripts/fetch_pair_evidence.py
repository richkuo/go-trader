#!/usr/bin/env python3
import argparse
import importlib.util
import json
import os
import sys
import time

ROOT = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
CLEARED_ENV = ("HYPERLIQUID_SECRET_KEY", "HYPERLIQUID_ACCOUNT_ADDRESS", "OKX_API_KEY", "OKX_API_SECRET", "OKX_PASSPHRASE")
ADAPTERS = {
    "hyperliquid": ("go_trader_pair_evidence_hyperliquid_adapter", os.path.join(ROOT, "platforms", "hyperliquid", "adapter.py")),
    "okx": ("go_trader_pair_evidence_okx_adapter", os.path.join(ROOT, "platforms", "okx", "adapter.py")),
}


class _ArgumentError(Exception):
    pass


class _Parser(argparse.ArgumentParser):
    def error(self, message):
        raise _ArgumentError(message)


def _emit(payload, code):
    sys.stdout.write(json.dumps(payload, sort_keys=True) + "\n")
    sys.stdout.flush()
    return code


def _redact(text):
    policy = sys.modules.get("egress_policy")
    if policy is not None:
        try:
            return policy.redact(text)
        except Exception:
            return "public capability read failed (detail withheld)"
    return text


def _load_module(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    sys.modules[name] = module
    spec.loader.exec_module(module)
    return module


def _read(provider, instrument):
    for key in CLEARED_ENV:
        os.environ.pop(key, None)
    sys.dont_write_bytecode = True
    tools = os.path.join(ROOT, "shared_tools")
    if tools not in sys.path:
        sys.path.insert(0, tools)
    evidence = _load_module("go_trader_pair_evidence_codec", os.path.join(tools, "pair_evidence.py"))
    name, path = ADAPTERS[provider]
    adapter_module = _load_module(name, path)
    if provider == "hyperliquid":
        adapter = adapter_module.HyperliquidExchangeAdapter(public_only=True)
        rules = adapter.public_instrument_rules(instrument)
        observed_ms = int(time.time() * 1000)
        meta = evidence.hyperliquid_wire_meta(instrument, rules, adapter_module.MIN_ORDER_NOTIONAL_USD)
    else:
        adapter = adapter_module.OKXExchangeAdapter(public_only=True)
        record = adapter.public_instrument_rules(instrument)
        observed_ms = int(time.time() * 1000)
        meta = evidence.okx_wire_meta(instrument, record)
    return {
        "ok": True,
        "provider": provider,
        "instrument": instrument,
        "provenance": evidence.DERIVATIONS[provider],
        "observed_ms": observed_ms,
        "meta": meta,
    }


def main(argv):
    parser = _Parser(prog="fetch_pair_evidence.py", add_help=False)
    parser.add_argument("--provider", required=True, choices=sorted(ADAPTERS))
    parser.add_argument("--instrument", required=True)
    parser.add_argument("--probe-only", action="store_true")
    try:
        args = parser.parse_args(argv)
    except _ArgumentError as exc:
        return _emit({"ok": False, "error": f"invalid arguments: {exc}"}, 1)
    if not args.instrument.strip() or args.instrument.strip() != args.instrument:
        return _emit({"ok": False, "error": "--instrument must be a nonempty trimmed native instrument"}, 1)
    if args.probe_only:
        return _emit({"ok": True, "probe": True}, 0)
    try:
        result = _read(args.provider, args.instrument)
    except Exception as exc:
        return _emit({"ok": False, "error": _redact(f"{type(exc).__name__}: {exc}")}, 1)
    return _emit(result, 0)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
