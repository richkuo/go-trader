import importlib.util
import json
import os
import subprocess
import sys

import pytest

HERE = os.path.dirname(os.path.abspath(__file__))
SCRIPT = os.path.join(HERE, "llm_strategy_feedback.py")
SCHEMA = os.path.join(HERE, "schemas", "trade_feedback_analysis.schema.json")


def _load():
    spec = importlib.util.spec_from_file_location("llm_strategy_feedback_under_test", SCRIPT)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


@pytest.fixture(scope="module")
def mod():
    return _load()


BUNDLE = {
    "bundle_version": 1,
    "selection": {"feedback_rowids": [3, 7]},
    "strategy": {"type": "perps", "open_strategy": {"name": "momentum"}},
}


def _result(**over):
    out = {
        "summary": "entries were late",
        "findings": [{"feedback_rowid": 3, "text": "late entry"}],
        "proposals": [
            {"kind": "param_grid", "params": {"roc_period": [10, 12], "threshold": [2.5, 3]}, "rationale": "faster"},
            {"kind": "note", "text": "watch funding"},
        ],
        "confidence": "medium",
    }
    out.update(over)
    return out


def _run(payload):
    proc = subprocess.run([sys.executable, SCRIPT, "--validate-only"], input=payload, capture_output=True, text=True, timeout=120)
    return proc.returncode, json.loads(proc.stdout)


def test_valid_result_passes_through_the_real_script():
    code, out = _run(json.dumps({"bundle": BUNDLE, "result": _result()}))
    assert code == 0
    assert out["outcome"] == "ok"
    assert [p["status"] for p in out["proposals"]] == ["valid", "valid"]


def test_each_contract_breach_makes_the_whole_result_invalid(mod):
    schema = mod.load_schema()
    long_grid = {f"p{i}": [1] for i in range(17)}
    cases = {
        "unknown_top_field": _result(extra=1),
        "unknown_finding_field": _result(findings=[{"feedback_rowid": 3, "text": "t", "x": 1}]),
        "bad_confidence": _result(confidence="certain"),
        "summary_too_long": _result(summary="x" * 4001),
        "too_many_findings": _result(findings=[{"feedback_rowid": 3, "text": "t"}] * 101),
        "uncited_row": _result(findings=[{"feedback_rowid": 99, "text": "t"}]),
        "bool_row_id": _result(findings=[{"feedback_rowid": True, "text": "t"}]),
        "empty_grid": _result(proposals=[{"kind": "param_grid", "params": {}, "rationale": "r"}]),
        "empty_values": _result(proposals=[{"kind": "param_grid", "params": {"roc_period": []}, "rationale": "r"}]),
        "too_many_params": _result(proposals=[{"kind": "param_grid", "params": long_grid, "rationale": "r"}]),
        "unknown_param": _result(proposals=[{"kind": "param_grid", "params": {"nope": [1]}, "rationale": "r"}]),
        "rejected_value": _result(proposals=[{"kind": "param_grid", "params": {"roc_period": [0]}, "rationale": "r"}]),
        "fractional_int": _result(proposals=[{"kind": "param_grid", "params": {"roc_period": [10.5]}, "rationale": "r"}]),
        "bool_for_number": _result(proposals=[{"kind": "param_grid", "params": {"threshold": [True]}, "rationale": "r"}]),
        "null_value": _result(proposals=[{"kind": "param_grid", "params": {"threshold": [None]}, "rationale": "r"}]),
        "note_with_params": _result(proposals=[{"kind": "note", "text": "n", "params": {"roc_period": [1]}}]),
        "unknown_kind": _result(proposals=[{"kind": "apply_now", "text": "n"}]),
        "not_an_object": [1, 2],
    }
    for name, result in cases.items():
        out = mod.validate(BUNDLE, result, schema)
        assert out["outcome"] == "invalid", name
        assert out["reasons"], name


def test_grid_product_limit_and_size_limit(mod):
    schema = dict(mod.load_schema())
    schema["x-go-trader-limits"] = {"max_grid_combinations": 4, "max_result_bytes": 600}
    wide = _result(proposals=[{"kind": "param_grid", "params": {"roc_period": [5, 6, 7], "threshold": [1, 2]}, "rationale": "r"}])
    out = mod.validate(BUNDLE, wide, schema)
    assert out["outcome"] == "invalid"
    assert out["proposals"][0]["status"] == "invalid"
    big = _result(summary="y" * 700)
    assert mod.validate(BUNDLE, big, schema)["outcome"] == "invalid"


def test_text_results_are_parsed_strictly(mod):
    schema = mod.load_schema()
    assert mod.validate(BUNDLE, json.dumps(_result()), schema)["outcome"] == "ok"
    for text in ["not json", '{"summary": NaN}', '{"summary": "a", "summary": "b"}', json.dumps(_result()).replace("2.5", "Infinity")]:
        assert mod.validate(BUNDLE, text, schema)["outcome"] == "invalid", text


def test_a_mixed_result_keeps_per_proposal_status(mod):
    result = _result(proposals=[
        {"kind": "param_grid", "params": {"roc_period": [10]}, "rationale": "ok"},
        {"kind": "param_grid", "params": {"roc_period": [-1]}, "rationale": "bad"},
        {"kind": "note", "text": "n"},
    ])
    out = mod.validate(BUNDLE, result, mod.load_schema())
    assert out["outcome"] == "invalid"
    assert [p["status"] for p in out["proposals"]] == ["valid", "invalid", "valid"]


def test_operational_failures_exit_one_with_json():
    broken = [
        "not json",
        json.dumps({"bundle": BUNDLE}),
        json.dumps({"bundle": BUNDLE, "result": _result(), "extra": 1}),
        json.dumps({"bundle": {"selection": {}}, "result": _result()}),
        json.dumps({"bundle": {**BUNDLE, "selection": {"feedback_rowids": [0]}}, "result": _result()}),
        json.dumps({"bundle": {**BUNDLE, "strategy": {"type": "perps", "open_strategy": {"name": "no_such_strategy"}}}, "result": _result()}),
    ]
    codes = []
    for payload in broken:
        code, out = _run(payload)
        codes.append((code, "error" in out))
    assert codes[:5] == [(1, True)] * 5
    assert codes[5] == (0, False)


def test_unknown_open_strategy_invalidates_grids(mod):
    bundle = {**BUNDLE, "strategy": {"type": "perps", "open_strategy": {"name": "no_such_strategy"}}}
    out = mod.validate(bundle, _result(), mod.load_schema())
    assert out["outcome"] == "invalid"
    assert out["proposals"][0]["status"] == "invalid"
    assert out["proposals"][1]["status"] == "valid"


def test_committed_schema_is_a_valid_draft_2020_12_schema():
    from jsonschema import Draft202012Validator
    with open(SCHEMA, encoding="utf-8") as fh:
        schema = json.load(fh)
    Draft202012Validator.check_schema(schema)
    assert schema["x-go-trader-limits"] == {"max_grid_combinations": 4096, "max_result_bytes": 524288}
