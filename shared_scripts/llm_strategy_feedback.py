#!/usr/bin/env python3

import argparse
import json
import math
import os
import sys

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
sys.path.insert(0, os.path.join(ROOT, "backtest"))

SCHEMA_PATH = os.path.join(os.path.dirname(os.path.abspath(__file__)), "schemas", "trade_feedback_analysis.schema.json")
MAX_REASONS = 50
MAX_REASON_CHARS = 300


class InputError(Exception):
    pass


def _reject_constant(name):
    raise ValueError(f"non-finite number {name} is not allowed")


def _reject_duplicate_keys(pairs):
    out = {}
    for key, value in pairs:
        if key in out:
            raise ValueError(f"duplicate object key {key!r}")
        out[key] = value
    return out


def strict_loads(text):
    return json.loads(text, parse_constant=_reject_constant, object_pairs_hook=_reject_duplicate_keys)


def load_schema():
    with open(SCHEMA_PATH, encoding="utf-8") as fh:
        return json.load(fh)


def _clip(text):
    text = str(text)
    if len(text) > MAX_REASON_CHARS:
        return text[:MAX_REASON_CHARS] + "..."
    return text


def _path(parts):
    out = "result"
    for part in parts:
        out += f"[{part}]" if isinstance(part, int) else f".{part}"
    return out


def _is_number(value):
    return isinstance(value, (int, float)) and not isinstance(value, bool)


def _finite_everywhere(value):
    if isinstance(value, float):
        return math.isfinite(value)
    if isinstance(value, list):
        return all(_finite_everywhere(v) for v in value)
    if isinstance(value, dict):
        return all(_finite_everywhere(v) for v in value.values())
    return True


def bundle_contract(bundle):
    if not isinstance(bundle, dict):
        raise InputError("bundle must be an object")
    selection = bundle.get("selection")
    strategy = bundle.get("strategy")
    if not isinstance(selection, dict) or not isinstance(strategy, dict):
        raise InputError("bundle needs selection and strategy objects")
    rowids = selection.get("feedback_rowids")
    if not isinstance(rowids, list) or not all(isinstance(r, int) and not isinstance(r, bool) and r > 0 for r in rowids):
        raise InputError("bundle selection.feedback_rowids must be a list of positive integers")
    open_ref = strategy.get("open_strategy")
    if not isinstance(open_ref, dict) or not isinstance(open_ref.get("name"), str) or not open_ref["name"].strip():
        raise InputError("bundle strategy.open_strategy.name is required")
    stype = strategy.get("type")
    if not isinstance(stype, str) or not stype.strip():
        raise InputError("bundle strategy.type is required")
    return set(rowids), open_ref["name"].strip(), stype.strip()


def load_open_registry(strategy_type):
    from registry_loader import load_registry, registry_for_strategy_type
    return load_registry(registry_for_strategy_type(strategy_type))


def _value_type_reason(default, value):
    if isinstance(default, bool):
        return None if isinstance(value, bool) else "must be true or false like its default"
    if isinstance(default, int):
        if isinstance(value, bool) or not _is_number(value):
            return "must be a number like its default"
        if isinstance(value, float) and not value.is_integer():
            return "must be a whole number like its default"
        return None
    if isinstance(default, float):
        return None if _is_number(value) else "must be a number like its default"
    if isinstance(default, str):
        return None if isinstance(value, str) else "must be a string like its default"
    return "has a default of an unsupported type"


def _coerce(default, value):
    if isinstance(default, int) and not isinstance(default, bool) and isinstance(value, float):
        return int(value)
    return value


def check_param_grid(proposal, registry, open_name, max_combinations):
    reasons = []
    params = proposal.get("params")
    if not isinstance(params, dict):
        return ["params is not an object"]
    entry = registry.STRATEGY_REGISTRY.get(open_name) if registry is not None else None
    if entry is None:
        return [f"open strategy {open_name!r} is not in the registry"]
    defaults = dict(entry.get("default_params") or {})
    combos = 1
    for name in sorted(params):
        values = params[name]
        if name not in defaults:
            reasons.append(f"unknown parameter {name!r} for {open_name}")
            continue
        if not isinstance(values, list) or not values:
            reasons.append(f"parameter {name!r} needs a non-empty list of values")
            continue
        combos *= len(values)
        for value in values:
            type_reason = _value_type_reason(defaults[name], value)
            if type_reason:
                reasons.append(f"{name}={value!r} {type_reason}")
                continue
            try:
                registry.validate_param_value(open_name, name, _coerce(defaults[name], value))
            except Exception as exc:
                reasons.append(f"{name}={value!r} is rejected: {_clip(exc)}")
    if combos > max_combinations:
        reasons.append(f"the grid has {combos} combinations, above the {max_combinations} limit")
    return reasons


def validate(bundle, result, schema, registry_loader=load_open_registry):
    from jsonschema import Draft202012Validator

    rowids, open_name, strategy_type = bundle_contract(bundle)
    limits = schema.get("x-go-trader-limits") or {}
    max_bytes = int(limits.get("max_result_bytes", 524288))
    max_combinations = int(limits.get("max_grid_combinations", 4096))
    reasons = []
    proposals_out = []

    if isinstance(result, str):
        if len(result.encode("utf-8")) > max_bytes:
            return {"outcome": "invalid", "reasons": [f"the result is larger than {max_bytes} bytes"], "proposals": []}
        try:
            result = strict_loads(result)
        except ValueError as exc:
            return {"outcome": "invalid", "reasons": [f"the result is not valid JSON: {_clip(exc)}"], "proposals": []}
    encoded = json.dumps(result, separators=(",", ":"), ensure_ascii=False, allow_nan=True)
    if len(encoded.encode("utf-8")) > max_bytes:
        return {"outcome": "invalid", "reasons": [f"the result is larger than {max_bytes} bytes"], "proposals": []}
    if not _finite_everywhere(result):
        return {"outcome": "invalid", "reasons": ["the result contains a non-finite number"], "proposals": []}

    validator = Draft202012Validator(schema)
    errors = sorted(validator.iter_errors(result), key=lambda e: (list(map(str, e.absolute_path)), e.message))
    for err in errors:
        reasons.append(f"{_path(err.absolute_path)}: {_clip(err.message)}")

    if isinstance(result, dict):
        findings = result.get("findings")
        if isinstance(findings, list):
            for i, finding in enumerate(findings):
                if not isinstance(finding, dict):
                    continue
                rid = finding.get("feedback_rowid")
                if isinstance(rid, int) and not isinstance(rid, bool) and rid not in rowids:
                    reasons.append(f"result.findings[{i}].feedback_rowid {rid} is not an included mark")
        proposals = result.get("proposals")
        if isinstance(proposals, list):
            registry = None
            if any(isinstance(p, dict) and p.get("kind") == "param_grid" for p in proposals):
                try:
                    registry = registry_loader(strategy_type)
                except Exception as exc:
                    raise InputError(f"cannot load the {strategy_type} open-strategy registry: {_clip(exc)}")
            for i, proposal in enumerate(proposals):
                if not isinstance(proposal, dict):
                    proposals_out.append({"index": i, "kind": "", "status": "invalid", "reasons": ["proposal is not an object"]})
                    continue
                kind = proposal.get("kind")
                if kind == "param_grid":
                    p_reasons = check_param_grid(proposal, registry, open_name, max_combinations)
                elif kind == "note":
                    p_reasons = []
                else:
                    p_reasons = [f"unknown proposal kind {kind!r}"]
                schema_prefix = f"result.proposals[{i}]"
                p_reasons = [r for r in reasons if r.startswith(schema_prefix + ".") or r.startswith(schema_prefix + ":")] + p_reasons
                proposals_out.append({"index": i, "kind": kind if isinstance(kind, str) else "", "status": "invalid" if p_reasons else "valid", "reasons": p_reasons[:MAX_REASONS]})
                for r in p_reasons:
                    if r not in reasons:
                        reasons.append(f"result.proposals[{i}]: {r}")

    outcome = "invalid" if reasons else "ok"
    return {"outcome": outcome, "reasons": reasons[:MAX_REASONS], "proposals": proposals_out}


def main(argv=None):
    parser = argparse.ArgumentParser(description="Validate one trade feedback analysis result against the shared contract.")
    parser.add_argument("--validate-only", action="store_true", required=True)
    parser.parse_args(argv)
    try:
        raw = sys.stdin.read()
        try:
            envelope = strict_loads(raw)
        except ValueError as exc:
            raise InputError(f"standard input is not valid JSON: {_clip(exc)}")
        if not isinstance(envelope, dict) or set(envelope) != {"bundle", "result"}:
            raise InputError("standard input must be an object with exactly bundle and result")
        out = validate(envelope["bundle"], envelope["result"], load_schema())
    except InputError as exc:
        print(json.dumps({"error": str(exc)}))
        return 1
    except Exception as exc:
        print(json.dumps({"error": f"validator failure: {_clip(type(exc).__name__ + ': ' + str(exc))}"}))
        return 1
    print(json.dumps(out, sort_keys=True))
    return 0


if __name__ == "__main__":
    sys.exit(main())
