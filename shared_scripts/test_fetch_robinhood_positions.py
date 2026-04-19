"""Tests for fetch_robinhood_positions.py — live-account position fetcher
used by the portfolio kill switch (#346).

Regression target: the #346 review caught that the adapter's
``get_crypto_positions()`` swallowed exceptions and returned [], which
the Go-side parser would then read as "no positions" → ConfirmedFlat=True
→ virtual state cleared while live exposure remained. The script now
calls ``get_crypto_positions_strict()`` so any adapter failure surfaces
as a JSON error envelope and the kill switch latches.
"""

import builtins
import importlib.util
import json
import os
import sys
from io import StringIO
from unittest.mock import MagicMock, patch

import pytest


def _run_script(positions_or_exc, is_live=True):
    """Invoke fetch_robinhood_positions.main() with a mocked adapter.

    positions_or_exc may be a list (returned by
    adapter.get_crypto_positions_strict) or an Exception. Returns
    (parsed_stdout_json, exit_code).
    """
    script_path = os.path.join(os.path.dirname(os.path.abspath(__file__)),
                               "fetch_robinhood_positions.py")
    spec = importlib.util.spec_from_file_location("fetch_robinhood_positions", script_path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)

    mock_adapter_cls = MagicMock()
    mock_adapter = MagicMock()
    mock_adapter.is_live = is_live
    mock_adapter_cls.return_value = mock_adapter
    if isinstance(positions_or_exc, Exception):
        mock_adapter.get_crypto_positions_strict.side_effect = positions_or_exc
    else:
        mock_adapter.get_crypto_positions_strict.return_value = positions_or_exc

    captured = StringIO()
    exit_code = {"value": 0}
    original_import = builtins.__import__

    def mock_import(name, *args, **kwargs):
        if name == "adapter":
            fake_mod = MagicMock()
            fake_mod.RobinhoodExchangeAdapter = mock_adapter_cls
            return fake_mod
        return original_import(name, *args, **kwargs)

    def mock_exit(code=0):
        exit_code["value"] = code
        raise SystemExit(code)

    with patch("builtins.__import__", side_effect=mock_import), \
         patch("sys.stdout", captured), \
         patch("sys.argv", ["fetch_robinhood_positions.py"]), \
         patch.object(mod.sys, "exit", side_effect=mock_exit):
        try:
            mod.main()
        except SystemExit:
            pass

    raw = captured.getvalue().strip()
    parsed = json.loads(raw) if raw else {}
    return parsed, exit_code["value"]


class TestSuccess:
    def test_single_position(self):
        out, code = _run_script([
            {"symbol": "BTC", "quantity": 0.01, "avg_price": 42000.0},
        ])
        assert code == 0
        assert len(out["positions"]) == 1
        p = out["positions"][0]
        assert p["coin"] == "BTC"
        assert p["size"] == 0.01
        assert p["avg_price"] == 42000.0
        assert "error" not in out

    def test_zero_quantity_filtered(self):
        out, code = _run_script([
            {"symbol": "BTC", "quantity": 0, "avg_price": 0},
        ])
        assert code == 0
        assert out["positions"] == []

    def test_empty_positions(self):
        out, code = _run_script([])
        assert code == 0
        assert out["positions"] == []
        assert "error" not in out


class TestFailurePaths:
    def test_non_live_adapter(self):
        out, code = _run_script([], is_live=False)
        assert code == 1
        assert "ROBINHOOD" in out["error"]

    def test_strict_raises_propagates_as_error_envelope(self):
        """Regression for #346 review: if the adapter call fails, the
        script MUST emit error envelope + exit 1 — NOT a clean
        {positions: []}. Otherwise the Go-side kill switch reads no
        positions, sets ConfirmedFlat=true, and clears virtual state
        while live exposure remains.
        """
        out, code = _run_script(RuntimeError("Robinhood 503 Service Unavailable"))
        assert code == 1
        assert "503" in out["error"]
        assert out["positions"] == []

    def test_not_logged_in_raises(self):
        out, code = _run_script(
            RuntimeError("Robinhood adapter not logged in — cannot fetch crypto positions")
        )
        assert code == 1
        assert "not logged in" in out["error"]


if __name__ == "__main__":
    sys.exit(pytest.main([__file__, "-v"]))
