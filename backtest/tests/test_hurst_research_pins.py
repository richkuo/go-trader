import ast
import importlib
import json
import os
import sys

import pytest

_HERE = os.path.dirname(__file__)
_REPO = os.path.abspath(os.path.join(_HERE, "..", ".."))
_RESEARCH = os.path.abspath(os.path.join(_HERE, "..", "research"))

sys.path.insert(0, _RESEARCH)
sys.path.insert(0, os.path.abspath(os.path.join(_HERE, "..")))

import hurst_gate as parity

STUDY_NAMES = (
    "hurst_1410_gate_calibration",
    "hurst_1422_gate_power",
    "hurst_1424_gate_resolution",
    "hurst_1426_two_sided_sort",
    "hurst_1427_change_sort",
    "hurst_1428_sizing_exit",
    "hurst_1474_rs_estimator",
)

CONTRACT_BASENAME = "hurst_gate_calibration.md"
CONTRACT = os.path.join(_RESEARCH, CONTRACT_BASENAME)
CONTRACT_OWNER = "hurst_1424_gate_resolution"

GO_GATE = os.path.join(_REPO, "scheduler", "hurst_gate.go")

LIVE_MODULES = {"regime", "regime_unified", "shared_tools.regime"}
LIVE_ARTIFACTS = ("hurst_gate_state", "config.example.json")


def _study(name):
    return importlib.import_module(name)


def _source(module):
    return open(module.__file__).read()


def _code_lines(module):
    lines = []
    for line in _source(module).split("\n"):
        stripped = line.strip()
        if stripped.startswith(('"', "'", 'f"', "f'", "#")):
            continue
        lines.append(line)
    return "\n".join(lines)


def _imported_roots(module):
    roots = set()
    for node in ast.walk(ast.parse(_source(module))):
        if isinstance(node, ast.Import):
            roots.update(a.name for a in node.names)
        elif isinstance(node, ast.ImportFrom) and node.module and node.level == 0:
            roots.add(node.module)
    return roots


# --- the shipped gate the runtime actually reads -----------------------------


def test_the_sizing_study_scores_the_parity_module_not_a_copy():
    study = _study("hurst_1428_sizing_exit")
    gate = parity.HurstGate(
        {
            "enabled": True,
            "mode": parity.HURST_GATE_MODE_SIZE,
            "size_floor": study.SHIPPED_SIZE_FLOOR,
        }
    )
    for i in range(0, 201):
        h = i / 200.0
        assert study.shipped_size_multiplier(h) == gate.size_multiplier(h)


def test_the_go_gate_still_implements_the_form_the_parity_module_scores():
    study = _study("hurst_1428_sizing_exit")
    src = open(GO_GATE).read()
    assert f"hurstSizeSpan = {study.SHIPPED_SIZE_SPAN}" in src
    assert f"hurstDefaultSizeFloor = {study.SHIPPED_SIZE_FLOOR}" in src
    body = src.split("func hurstSizeMultiplier(", 1)[1].split("\n}", 1)[0]
    assert "math.Abs(h-0.5) / hurstSizeSpan" in body
    assert "m > 1.0" in body and "m = 1.0" in body
    assert "m < floor" in body and "m = floor" in body
    assert "return 1.0" in body


def test_the_reference_window_exceeds_the_live_fetch_depth():
    study = _study("hurst_1474_rs_estimator")
    assert study.REFERENCE_WINDOW not in study.HURST_WINDOWS
    assert parity.hurst_live_frame_bars(None) < study.REFERENCE_WINDOW
    assert (
        parity.hurst_live_frame_bars({"trend": {"period": 200}})
        < study.REFERENCE_WINDOW
    )


# --- the degenerate-limit verdict pin ---------------------------------------


def test_the_committed_run_never_oversells_a_degenerate_limit():
    study = _study("hurst_1427_change_sort")
    with open(study._DEFAULT_JSON_OUT) as fh:
        payload = json.load(fh)
    gate = payload["decision"]["validity_gate"]
    text = payload["decision"]["justification"]
    assert "limit_is_degenerate" in gate
    if gate["limit_is_degenerate"]:
        assert gate["limit"] == 0.0
        assert "PASSES TRIVIALLY" in text
        assert "corroborates NOTHING" in text


# --- report-only: no study may write a live path ----------------------------


def test_only_the_owner_defaults_to_the_live_evidence_contract_path(subtests):
    for i, name in enumerate(STUDY_NAMES):
        with subtests.test(case=i, name=name):
            study = _study(name)
            is_owner = name == CONTRACT_OWNER
            assert (
                os.path.abspath(study._DEFAULT_REPORT_OUT) == os.path.abspath(CONTRACT)
            ) is is_owner
            assert os.path.basename(study._DEFAULT_JSON_OUT) == f"{name}.json"


_A_DEFERRING_STUDY_REFUSES_THE_CONTRACT_PATH_EVEN_WHEN_ASKED_CASES = [
        ("hurst_1422_gate_power", True),
        ("hurst_1426_two_sided_sort", False),
        ("hurst_1427_change_sort", False),
        ("hurst_1428_sizing_exit", False),
        ("hurst_1474_rs_estimator", False),
    ]


def test_a_deferring_study_refuses_the_contract_path_even_when_asked(tmp_path, subtests):
    for i, (name, needs_json_out) in enumerate(_A_DEFERRING_STUDY_REFUSES_THE_CONTRACT_PATH_EVEN_WHEN_ASKED_CASES):
        with subtests.test(case=i, name=name, needs_json_out=needs_json_out):
            case_dir = tmp_path / f"case{i}"
            case_dir.mkdir()
            study = _study(name)
            before = os.path.exists(CONTRACT) and open(CONTRACT).read()
            argv = ["--report-out", CONTRACT]
            if needs_json_out:
                argv += ["--json-out", str(case_dir / "scoped.json")]
            with pytest.raises(SystemExit) as exc:
                study.main(argv)
            message = str(exc.value)
            assert f"[{name.split('_')[1]}]" in message, message
            assert CONTRACT_BASENAME in message, message
            assert f"{CONTRACT_OWNER}.py" in message, message
            assert (os.path.exists(CONTRACT) and open(CONTRACT).read()) == before


def test_no_study_names_a_live_state_or_config_artifact(subtests):
    for i, name in enumerate(STUDY_NAMES):
        with subtests.test(case=i, name=name):
            code = _code_lines(_study(name))
            for banned in LIVE_ARTIFACTS:
                assert banned not in code, banned


def test_the_estimator_study_reaches_no_live_regime_module():
    study = _study("hurst_1474_rs_estimator")
    assert not (_imported_roots(study) & LIVE_MODULES)
    assert study.CONTRACT_PATH_CLAIMED is False
    for needle in (
        "hurst_rs",
        "shared_tools/regime.py",
        "scheduler/hurst_gate.go",
        "config.example.json",
    ):
        assert needle in study.NON_GOALS
