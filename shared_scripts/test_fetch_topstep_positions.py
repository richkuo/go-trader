
import builtins
import importlib.util
import json
import os
import sys
from io import StringIO
from unittest.mock import MagicMock, patch

import pytest


def _run_script(positions_or_exc, is_live=True, use_raise=True):
    script_path = os.path.join(os.path.dirname(os.path.abspath(__file__)),
                               "fetch_topstep_positions.py")
    spec = importlib.util.spec_from_file_location("fetch_topstep_positions", script_path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)

    mock_adapter_cls = MagicMock()
    mock_adapter = MagicMock()
    mock_adapter.is_live = is_live
    mock_adapter_cls.return_value = mock_adapter
    target = mock_adapter.get_open_positions_raise if use_raise else mock_adapter.get_open_positions
    if isinstance(positions_or_exc, Exception):
        target.side_effect = positions_or_exc
    else:
        target.return_value = positions_or_exc

    captured = StringIO()
    exit_code = {"value": 0}
    original_import = builtins.__import__

    def mock_import(name, *args, **kwargs):
        if name == "adapter":
            fake_mod = MagicMock()
            fake_mod.TopStepExchangeAdapter = mock_adapter_cls
            return fake_mod
        return original_import(name, *args, **kwargs)

    def mock_exit(code=0):
        exit_code["value"] = code
        raise SystemExit(code)

    with patch("builtins.__import__", side_effect=mock_import), \
         patch("sys.stdout", captured), \
         patch("sys.argv", ["fetch_topstep_positions.py"]), \
         patch.object(mod.sys, "exit", side_effect=mock_exit):
        try:
            mod.main()
        except SystemExit:
            pass

    raw = captured.getvalue().strip()
    parsed = json.loads(raw) if raw else {}
    return parsed, exit_code["value"]


@pytest.mark.parametrize("raw,expected", [
    ([{"symbol": "ES", "quantity": 2, "avg_price": 5000.0, "side": "long"}],
     [{"coin": "ES", "size": 2, "avg_price": 5000.0, "side": "long"}]),
    ([{"symbol": "NQ", "quantity": -1, "avg_price": 18000.0, "side": "short"}],
     [{"coin": "NQ", "size": -1, "avg_price": 18000.0, "side": "short"}]),
    ([{"symbol": "ES", "quantity": 0, "avg_price": 5000.0, "side": "long"}], []),
    ([], []),
])
def test_positions_are_normalized(raw, expected):
    out, code = _run_script(raw)
    assert code == 0
    assert len(out["positions"]) == len(expected)
    for got, want in zip(out["positions"], expected):
        for key, value in want.items():
            assert got[key] == value
    assert "error" not in out


@pytest.mark.parametrize("positions_or_exc,is_live,fragment", [
    ([], False, "TOPSTEP_API_KEY"),
    (RuntimeError("401 Unauthorized"), True, "401"),
    (RuntimeError("TopStepX 503 Service Unavailable"), True, "503"),
    (ConnectionError("DNS resolution failed"), True, "DNS"),
])
def test_failure_paths_emit_error_envelope(positions_or_exc, is_live, fragment):
    out, code = _run_script(positions_or_exc, is_live=is_live)
    assert code == 1
    assert fragment in out["error"]
    assert out["positions"] == []


def test_uses_raise_variant_not_soft_fail():
    script_path = os.path.join(os.path.dirname(os.path.abspath(__file__)),
                               "fetch_topstep_positions.py")
    spec = importlib.util.spec_from_file_location("fetch_topstep_positions", script_path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)

    mock_adapter_cls = MagicMock()
    mock_adapter = MagicMock()
    mock_adapter.is_live = True
    mock_adapter_cls.return_value = mock_adapter
    mock_adapter.get_open_positions_raise.return_value = []
    mock_adapter.get_open_positions.side_effect = AssertionError(
        "fetch_topstep_positions must use get_open_positions_raise, not get_open_positions"
    )

    original_import = builtins.__import__

    def mock_import(name, *args, **kwargs):
        if name == "adapter":
            fake_mod = MagicMock()
            fake_mod.TopStepExchangeAdapter = mock_adapter_cls
            return fake_mod
        return original_import(name, *args, **kwargs)

    captured = StringIO()
    with patch("builtins.__import__", side_effect=mock_import), \
         patch("sys.stdout", captured), \
         patch("sys.argv", ["fetch_topstep_positions.py"]):
        mod.main()

    assert mock_adapter.get_open_positions_raise.called
    assert not mock_adapter.get_open_positions.called


if __name__ == "__main__":
    sys.exit(pytest.main([__file__, "-v"]))
