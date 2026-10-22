
import json
import subprocess
import sys
from pathlib import Path

from shared_tools.conftest import load_module

_CLOSE_REGISTRY_LOADER = load_module("_close_registry_loader_test", Path(__file__).with_name("close_registry_loader.py"))
list_strategies = _CLOSE_REGISTRY_LOADER.list_strategies
list_strategies_detailed = _CLOSE_REGISTRY_LOADER.list_strategies_detailed

_LOADER_PATH = Path(__file__).resolve().parent / "close_registry_loader.py"


def test_list_strategies_detailed_matches_names_sorted_shape_and_known_evaluators():
    detailed = list_strategies_detailed()
    assert {e["name"] for e in detailed} == set(list_strategies()), "matches_list_strategies"

    names = [e["name"] for e in list_strategies_detailed()]
    assert names == sorted(names), "sorted_by_name"

    detailed = list_strategies_detailed()
    assert detailed, "shape: expected at least one registered close evaluator"
    for entry in detailed:
        assert set(entry.keys()) == {"name", "description", "default_params", "platforms"}, ("shape", entry)
        assert isinstance(entry["name"], str) and entry["name"], ("shape", entry)
        assert isinstance(entry["description"], str) and entry["description"], ("shape", entry)
        assert isinstance(entry["default_params"], dict), ("shape", entry)
        assert isinstance(entry["platforms"], list) and entry["platforms"], ("shape", entry)

    by_name = {e["name"]: e for e in list_strategies_detailed()}
    for name in ("tiered_tp_atr_live", "trailing_tp_ratchet", "avwap_stop"):
        assert name in by_name, ("spot_check_known_evaluators", name)
    assert by_name["avwap_stop"]["default_params"] == {"buffer_atr_mult": 0.25, "atr_source": "live"}, (
        "spot_check_known_evaluators: avwap_stop default_params")


def test_cli_list_json_emits_same_shape_as_python_call():
    result = subprocess.run(
        [sys.executable, str(_LOADER_PATH), "--list-json"],
        capture_output=True,
        text=True,
        check=True,
    )
    dumped = json.loads(result.stdout)
    assert dumped == list_strategies_detailed()


def test_cli_without_list_json_flag_errors_with_json_on_stdout():
    result = subprocess.run(
        [sys.executable, str(_LOADER_PATH)],
        capture_output=True,
        text=True,
    )
    assert result.returncode == 1
    payload = json.loads(result.stdout)
    assert "error" in payload
