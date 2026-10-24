#!/usr/bin/env python3
import sys

sys.dont_write_bytecode = True

import importlib.util
import json
import os
import traceback

ROOT = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
REQUEST_VERSION = 2
RESULT_VERSION = 2
MAX_REASON_BYTES = 1024
_PAIR_REGISTRY = ("go_trader_pair_registry", os.path.join("shared_strategies", "open", "pair_registry.py"))
_CODEC = ("pair_evidence", os.path.join("shared_tools", "pair_evidence.py"))


def _load(module_name, rel):
    path = os.path.join(ROOT, rel)
    loaded = sys.modules.get(module_name)
    if loaded is not None:
        origin = getattr(loaded, "__file__", None) or ""
        if os.path.realpath(origin) != os.path.realpath(path):
            raise ImportError(f"module {module_name} is loaded from {origin!r}, not {path!r}")
        return loaded
    spec = importlib.util.spec_from_file_location(module_name, path)
    module = importlib.util.module_from_spec(spec)
    sys.modules[module_name] = module
    try:
        spec.loader.exec_module(module)
    except BaseException:
        sys.modules.pop(module_name, None)
        raise
    return module


def _clip(text):
    raw = text.encode("utf-8")
    if len(raw) <= MAX_REASON_BYTES:
        return text
    return raw[:MAX_REASON_BYTES].decode("utf-8", "ignore")


def _identity(codec, request):
    out = {"v": RESULT_VERSION, "pair_id": "", "slot_ms": 0, "contract_fingerprint": "", "input_fingerprint": ""}
    if not isinstance(request, dict):
        return out
    for k in ("pair_id", "contract_fingerprint", "input_fingerprint"):
        if isinstance(request.get(k), str):
            out[k] = request[k]
    slot = request.get("slot_ms")
    if isinstance(slot, codec.JSONNumber):
        try:
            value = int(slot.text)
        except ValueError:
            value = None
        if value is not None and str(value) == slot.text and 0 <= value < (1 << 63):
            out["slot_ms"] = value
    return out


def _invalid(codec, request, reason):
    out = _identity(codec, request)
    policy = request.get("policy") if isinstance(request, dict) and isinstance(request.get("policy"), str) else None
    out.update({"status": "invalid", "error": _clip(reason), "decision": {"policy": policy, "stage": "request", "reason": _clip(reason)}})
    return out


def run(data):
    pairs = _load(*_PAIR_REGISTRY)
    codec = _load(*_CODEC)
    try:
        request = codec.loads_strict(data)
    except codec.PairEvidenceError as exc:
        return _invalid(codec, None, f"the policy request is not strict JSON: {exc}")
    if not isinstance(request, dict):
        return _invalid(codec, None, "the policy request is not a JSON object")
    version = request.get("v")
    if not (isinstance(version, codec.JSONNumber) and version.text == str(REQUEST_VERSION)):
        shown = version.text if isinstance(version, codec.JSONNumber) else type(version).__name__
        return _invalid(codec, request, f"request version {shown} is not {REQUEST_VERSION}")
    policy = request.get("policy")
    known = pairs.pair_policies()
    if not isinstance(policy, str) or policy not in known:
        return _invalid(codec, request, f"policy {policy!r} is not a known pair policy (known: {', '.join(known)})")
    return pairs.get_pair_evaluator(policy)(request)


def _emit(obj):
    text = json.dumps(obj, sort_keys=True, separators=(",", ":"), allow_nan=False)
    sys.stdout.write(text + "\n")
    sys.stdout.flush()


def main(argv):
    probe = False
    for arg in argv:
        if arg == "--probe-only":
            probe = True
        else:
            sys.stderr.write(f"run_pair_policy.py: unknown argument {arg!r}\n")
            return 2
    try:
        if probe:
            pairs = _load(*_PAIR_REGISTRY)
            known = list(pairs.pair_policies())
            for name in known:
                pairs.get_pair_evaluator(name)
            _emit({"ok": True, "probe": True, "policies": known})
            return 0
        _emit(run(sys.stdin.buffer.read()))
        return 0
    except Exception:
        traceback.print_exc(file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
