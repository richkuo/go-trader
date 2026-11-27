
import json
import subprocess
import sys
from pathlib import Path

from shared_tools.conftest import load_module

_CLOSE_REGISTRY_LOADER = load_module("_close_registry_loader_test", Path(__file__).with_name("close_registry_loader.py"))
list_strategies_detailed = _CLOSE_REGISTRY_LOADER.list_strategies_detailed

_LOADER_PATH = Path(__file__).resolve().parent / "close_registry_loader.py"


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
