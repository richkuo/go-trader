
import builtins
import importlib.util
import json
import os
import sys
from io import StringIO
from unittest.mock import MagicMock, patch

import pytest


def _run_script(positions_or_exc, is_live=True):
    script_path = os.path.join(os.path.dirname(os.path.abspath(__file__)),
                               "fetch_okx_positions.py")
    spec = importlib.util.spec_from_file_location("fetch_okx_positions", script_path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)

    mock_adapter_cls = MagicMock()
    mock_adapter = MagicMock()
    mock_adapter.is_live = is_live
    mock_adapter_cls.return_value = mock_adapter
    if isinstance(positions_or_exc, Exception):
        mock_adapter.fetch_open_positions.side_effect = positions_or_exc
    else:
        mock_adapter.fetch_open_positions.return_value = positions_or_exc

    captured = StringIO()
    exit_code = {"value": 0}
    original_import = builtins.__import__

    def mock_import(name, *args, **kwargs):
        if name == "adapter":
            fake_mod = MagicMock()
            fake_mod.OKXExchangeAdapter = mock_adapter_cls
            return fake_mod
        return original_import(name, *args, **kwargs)

    def mock_exit(code=0):
        exit_code["value"] = code
        raise SystemExit(code)

    with patch("builtins.__import__", side_effect=mock_import), \
         patch("sys.stdout", captured), \
         patch("sys.argv", ["fetch_okx_positions.py"]), \
         patch.object(mod.sys, "exit", side_effect=mock_exit):
        try:
            mod.main()
        except SystemExit:
            pass

    raw = captured.getvalue().strip()
    parsed = json.loads(raw) if raw else {}
    return parsed, exit_code["value"]


@pytest.mark.parametrize("raw,expected", [
    ([{"symbol": "BTC/USDT:USDT", "contracts": "0.01", "side": "long",
       "entryPrice": "42000.5"}],
     [{"coin": "BTC", "size": 0.01, "side": "long", "entry_price": 42000.5}]),
    ([{"symbol": "ETH/USDT:USDT", "contracts": "0.5", "side": "short",
       "entryPrice": "3000"}],
     [{"coin": "ETH", "size": -0.5, "side": "short", "entry_price": 3000.0}]),
    ([{"symbol": "BTC/USDT:USDT", "contracts": "0", "side": "long"}], []),
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
    ([], False, "OKX_API_KEY"),
    (RuntimeError("OKX 503 Service Unavailable"), True, "OKX 503"),
])
def test_failure_paths_emit_error_envelope(positions_or_exc, is_live, fragment):
    out, code = _run_script(positions_or_exc, is_live=is_live)
    assert code == 1
    assert fragment in out["error"]
    assert out["positions"] == []


if __name__ == "__main__":
    sys.exit(pytest.main([__file__, "-v"]))
