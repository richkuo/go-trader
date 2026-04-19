"""Tests for fetch_topstep_positions.py — live-account position fetcher
used by the portfolio kill switch (#347).

Critical invariant: auth / network failures on TopStepX MUST produce an
error envelope (exit 1, "error" key populated, positions empty). If the
script silently reports an empty position list on a transient 5xx or an
expired token, the kill switch would clear virtual state while live CME
exposure survived — the exact #341/#342 bug class this feature is meant
to close, just shifted into the fetch path.
"""

import builtins
import importlib.util
import json
import os
import sys
from io import StringIO
from unittest.mock import MagicMock, patch

import pytest


def _run_script(positions_or_exc, is_live=True, use_raise=True):
    """Invoke fetch_topstep_positions.main() with a mocked adapter.

    positions_or_exc may be either a list (returned by
    adapter.get_open_positions_raise) or an Exception. Returns
    (parsed_stdout_json, exit_code).
    """
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


class TestSuccess:
    def test_long_position(self):
        out, code = _run_script([
            {"symbol": "ES", "quantity": 2, "avg_price": 5000.0, "side": "long"},
        ])
        assert code == 0
        assert len(out["positions"]) == 1
        p = out["positions"][0]
        assert p["coin"] == "ES"
        assert p["size"] == 2
        assert p["avg_price"] == 5000.0
        assert p["side"] == "long"
        assert "error" not in out

    def test_short_position_size_is_negative(self):
        """Short positions must carry a negative signed size so the Go
        kill switch parser can infer direction identically to HL/OKX."""
        out, code = _run_script([
            {"symbol": "NQ", "quantity": -1, "avg_price": 18000.0, "side": "short"},
        ])
        assert code == 0
        assert out["positions"][0]["size"] == -1
        assert out["positions"][0]["side"] == "short"

    def test_zero_size_filtered(self):
        out, code = _run_script([
            {"symbol": "ES", "quantity": 0, "avg_price": 5000.0, "side": "long"},
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
        assert "TOPSTEP_API_KEY" in out["error"]

    def test_exchange_raises_401(self):
        """Auth failure (rotated/revoked token) must surface as an error
        envelope, not an empty success. Regression guard for the review
        comment on PR #351: the old code called the soft-fail
        get_open_positions() which swallowed every exception."""
        out, code = _run_script(RuntimeError("401 Unauthorized"))
        assert code == 1
        assert "401" in out["error"]
        assert out["positions"] == []

    def test_exchange_raises_5xx(self):
        out, code = _run_script(RuntimeError("TopStepX 503 Service Unavailable"))
        assert code == 1
        assert "503" in out["error"]
        assert out["positions"] == []

    def test_network_error(self):
        out, code = _run_script(ConnectionError("DNS resolution failed"))
        assert code == 1
        assert "DNS" in out["error"]
        assert out["positions"] == []

    def test_uses_raise_variant_not_soft_fail(self):
        """The script MUST call get_open_positions_raise (re-raises on
        failure) rather than get_open_positions (returns []). If a future
        refactor switches back to the soft-fail method, this test will
        catch it because the raise-variant mock will be untouched and
        the exception never fires."""
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
