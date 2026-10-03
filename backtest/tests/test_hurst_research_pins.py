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

_MR = "mean_reversion"


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


def test_the_shipped_multiplier_is_capped_floored_and_one_at_undefined_h():
    study = _study("hurst_1428_sizing_exit")
    span = study.SHIPPED_SIZE_SPAN
    for i in range(0, 201):
        m = study.shipped_size_multiplier(i / 200.0)
        assert study.SHIPPED_SIZE_FLOOR <= m <= study.SHIPPED_SIZE_CEILING
    assert study.shipped_size_multiplier(0.5 + span) == 1.0
    assert study.shipped_size_multiplier(0.5 - span) == 1.0
    assert study.shipped_size_multiplier(0.5 + span / 2) == pytest.approx(0.5)
    for missing in (None, float("nan"), float("inf")):
        assert study.shipped_size_multiplier(missing) == 1.0


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


def _mde(mom_limit, mom_sep, p0=0.9):
    study = _study("hurst_1427_change_sort")
    return {
        "by_family_cluster": {study.PRIMARY_FAMILY: mom_limit, _MR: 0.05},
        "by_family_separation": {study.PRIMARY_FAMILY: mom_sep, _MR: 0.02},
        "by_family_n": {study.PRIMARY_FAMILY: 8000, _MR: 20000},
        "by_family_cluster_p0": {study.PRIMARY_FAMILY: p0, _MR: 0.8},
        "observed_separation_by_pool": {
            "primary": {
                f"{study.PRIMARY_FAMILY}|512": mom_sep,
                f"{_MR}|512": 0.02,
            }
        },
        "pooled_primary_cluster": 0.01,
        "pooled_primary_free": 0.009,
        "pooled_primary_n": 28000,
        "pooled_primary_cluster_p0": 0.5,
        "pooled_primary_free_p0": 0.5,
        "pooled_primary_cluster_return": 1.0,
        "pooled_primary_cluster_return_p0": 0.6,
    }


def _passing_cfg():
    study = _study("hurst_1427_change_sort")
    windows = {
        w: {
            "n_legs": 1,
            "dd_delta": -1.0,
            "chop_delta": -1.0,
            "ret_gated": 10.0,
            "ret_ungated": 10.0,
        }
        for w in study.WINDOW_ORDER
    }
    return {
        "config_id": study.PRIMARY_CONFIG_ID,
        "cohort": study.COHORT_PRIMARY,
        "family": study.PRIMARY_FAMILY,
        "mode": "gate",
        "hurst_window": 512,
        "lookback_bars": 256,
        "arm": study.PRIMARY_ARM,
        "disarm": study.PRIMARY_DISARM,
        "gain": None,
        "protocol_windows": list(study.PRIMARY_PROTOCOL_WINDOWS),
        "protocol_min_windows": study.PRIMARY_PROTOCOL_MIN_WINDOWS,
        "held_out_windows": list(study.PRIMARY_HELD_OUT_WINDOWS),
        "windows": windows,
        "p_raw": 0.001,
        "p_cluster": 0.001,
        "p_cluster_return": 0.001,
        "separation": 0.09,
        "separation_return": 1.0,
        "bh_reject": True,
        "n_pooled_trades": 500,
        "n_missing_target": 0,
        "n_suppressed": 100,
        "n_kept": 400,
        "n_pooled_effective": 200.0,
        "n_suppressed_effective": 60.0,
        "n_kept_effective": 140.0,
        "cluster_excluded_datasets": [],
        "cluster_excluded_trades": 0,
        "cluster_offset_range": [30, 300],
        "cluster_distinct_offsets": 271,
        "cluster_reason": None,
    }


def test_a_zero_limit_is_flagged_as_degenerate_everywhere():
    study = _study("hurst_1427_change_sort")
    assert study.limit_is_degenerate(0.0) is True
    assert study.limit_is_degenerate(0.001) is False
    assert study.limit_is_degenerate(None) is False
    for limit, sep in ((0.0, 0.01), (0.0, None), (0.05, 0.09), (None, 0.09)):
        gate = study.validity_gate(_mde(limit, sep))
        assert "limit_is_degenerate" in gate
        assert gate["limit_is_degenerate"] is (limit == 0.0)


def test_a_degenerate_limit_passes_the_gate_but_corroborates_nothing():
    study = _study("hurst_1427_change_sort")
    gate = study.validity_gate(_mde(0.0, 0.0099))
    assert gate["passed"] is True
    assert gate["limit_is_degenerate"] is True
    decision = study.decide_recommendation(
        [_passing_cfg()], _mde(0.0, 0.0099, p0=0.011)
    )
    assert decision["verdict"] == study.VERDICT_CHANGE_SORTS
    text = decision["justification"]
    assert "PASSES TRIVIALLY" in text
    assert "corroborates NOTHING" in text


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


@pytest.mark.parametrize("name", STUDY_NAMES)
def test_only_the_owner_defaults_to_the_live_evidence_contract_path(name):
    study = _study(name)
    is_owner = name == CONTRACT_OWNER
    assert (
        os.path.abspath(study._DEFAULT_REPORT_OUT) == os.path.abspath(CONTRACT)
    ) is is_owner
    assert os.path.basename(study._DEFAULT_JSON_OUT) == f"{name}.json"


@pytest.mark.parametrize(
    "name,needs_json_out",
    [
        ("hurst_1422_gate_power", True),
        ("hurst_1426_two_sided_sort", False),
        ("hurst_1427_change_sort", False),
        ("hurst_1428_sizing_exit", False),
        ("hurst_1474_rs_estimator", False),
    ],
)
def test_a_deferring_study_refuses_the_contract_path_even_when_asked(
    name, needs_json_out, tmp_path
):
    study = _study(name)
    before = os.path.exists(CONTRACT) and open(CONTRACT).read()
    argv = ["--report-out", CONTRACT]
    if needs_json_out:
        argv += ["--json-out", str(tmp_path / "scoped.json")]
    with pytest.raises(SystemExit) as exc:
        study.main(argv)
    message = str(exc.value)
    assert f"[{name.split('_')[1]}]" in message, message
    assert CONTRACT_BASENAME in message, message
    assert f"{CONTRACT_OWNER}.py" in message, message
    assert (os.path.exists(CONTRACT) and open(CONTRACT).read()) == before


@pytest.mark.parametrize("name", STUDY_NAMES)
def test_no_study_names_a_live_state_or_config_artifact(name):
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


@pytest.mark.parametrize("name", STUDY_NAMES)
def test_no_study_writes_outside_its_own_two_artifacts(name):
    code = _code_lines(_study(name))
    writes = [ln for ln in code.split("\n") if "open(" in ln and '"w"' in ln]
    assert writes, "the study must write its own report and JSON"
    for line in writes:
        assert "args.json_out" in line or "args.report_out" in line, line
