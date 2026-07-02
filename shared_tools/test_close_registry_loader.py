"""Tests for close_registry_loader.py's --list-json catalog surface (#1203)."""

import json
import subprocess
import sys
from pathlib import Path

from close_registry_loader import list_strategies, list_strategies_detailed

_LOADER_PATH = Path(__file__).resolve().parent / "close_registry_loader.py"


def test_list_strategies_detailed_matches_list_strategies():
    detailed = list_strategies_detailed()
    assert {e["name"] for e in detailed} == set(list_strategies())


def test_list_strategies_detailed_sorted_by_name():
    names = [e["name"] for e in list_strategies_detailed()]
    assert names == sorted(names)


def test_list_strategies_detailed_shape():
    detailed = list_strategies_detailed()
    assert detailed, "expected at least one registered close evaluator"
    for entry in detailed:
        assert set(entry.keys()) == {"name", "description", "default_params", "platforms"}
        assert isinstance(entry["name"], str) and entry["name"]
        assert isinstance(entry["description"], str) and entry["description"]
        assert isinstance(entry["default_params"], dict)
        assert isinstance(entry["platforms"], list) and entry["platforms"]


def test_list_strategies_detailed_spot_check_known_evaluators():
    by_name = {e["name"]: e for e in list_strategies_detailed()}
    for name in ("tiered_tp_atr_live", "trailing_tp_ratchet", "avwap_stop"):
        assert name in by_name
    assert by_name["avwap_stop"]["default_params"] == {"buffer_atr_mult": 0.25, "atr_source": "live"}


def test_cli_list_json_emits_same_shape_as_python_call():
    """Subprocess contract: --list-json prints JSON to stdout on success."""
    result = subprocess.run(
        [sys.executable, str(_LOADER_PATH), "--list-json"],
        capture_output=True,
        text=True,
        check=True,
    )
    dumped = json.loads(result.stdout)
    assert dumped == list_strategies_detailed()


def test_cli_without_list_json_flag_errors_with_json_on_stdout():
    """Subprocess contract: scripts emit JSON to stdout even on error, exit 1."""
    result = subprocess.run(
        [sys.executable, str(_LOADER_PATH)],
        capture_output=True,
        text=True,
    )
    assert result.returncode == 1
    payload = json.loads(result.stdout)
    assert "error" in payload
