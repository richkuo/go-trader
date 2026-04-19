"""Tests for close_okx_position.py — adapter response parsing for the
portfolio kill switch (#345).

Pattern mirrors test_close_hyperliquid_position.py: load the script as a
module, mock the `adapter` import, run main() with --symbol=... --mode=live,
capture stdout + exit code.

These tests pin the contract for every branch of the adapter response
parser. A regression that treats an ambiguous response as success would
silently clear virtual state while on-chain exposure remained — the
exact #345 failure mode. Every path that means "close was NOT confirmed"
must exit 1 with a populated error field so the Go caller latches the
kill switch.
"""

import builtins
import importlib.util
import json
import os
import sys
from io import StringIO
from unittest.mock import MagicMock, patch

import pytest


def _run_script(adapter_response_or_exc, argv, is_live=True):
    """Helper: invoke close_okx_position.main() with a mocked adapter.

    adapter_response_or_exc may be either a dict/value (returned by
    adapter.market_close) or an Exception subclass (raised by
    market_close). argv is the list passed to main() as sys.argv
    (excluding the program name).

    is_live controls the mocked adapter's is_live property — False
    verifies the script rejects paper/unauthenticated adapters.

    Returns (parsed_stdout_json, exit_code).
    """
    script_path = os.path.join(os.path.dirname(os.path.abspath(__file__)),
                               "close_okx_position.py")
    spec = importlib.util.spec_from_file_location("close_okx_position", script_path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)

    mock_adapter_cls = MagicMock()
    mock_adapter = MagicMock()
    mock_adapter.is_live = is_live
    mock_adapter_cls.return_value = mock_adapter
    if isinstance(adapter_response_or_exc, Exception):
        mock_adapter.market_close.side_effect = adapter_response_or_exc
    else:
        mock_adapter.market_close.return_value = adapter_response_or_exc

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
         patch("sys.argv", ["close_okx_position.py"] + argv), \
         patch.object(mod.sys, "exit", side_effect=mock_exit):
        try:
            mod.main()
        except SystemExit:
            pass

    raw = captured.getvalue().strip()
    parsed = json.loads(raw) if raw else {}
    return parsed, exit_code["value"]


class TestPaperModeRejected:
    """--mode=live is required for kill-switch invocation."""

    def test_paper_mode_exits_nonzero(self):
        out, code = _run_script({"id": "xx"}, ["--symbol=BTC", "--mode=paper"])
        assert code == 1
        assert "--mode=live required" in out["error"]
        assert out["close"] is None

    def test_default_mode_is_live(self):
        """No --mode flag → default live. Must go through to the adapter."""
        out, code = _run_script({"id": "abc", "average": 42000, "filled": 0.01},
                                ["--symbol=BTC"])
        assert code == 0, out
        assert "error" not in out


class TestNonLiveAdapterRejected:
    """Adapter reporting is_live=False (missing credentials) must fail fast —
    silently calling market_close on a paper adapter would raise
    RuntimeError but the script's try/except would surface it as a generic
    error. The explicit is_live check produces a clearer operator message."""

    def test_non_live_adapter_exits_nonzero(self):
        out, code = _run_script({}, ["--symbol=BTC", "--mode=live"], is_live=False)
        assert code == 1
        assert "OKX_API_KEY" in out["error"]
        assert out["close"]["fill"] == {}


class TestSuccessFill:
    """Normal close response → success path with fill telemetry."""

    def test_full_fill_fields(self):
        response = {
            "id": "12345abc",
            "average": 42000.5,
            "filled": 0.01,
            "fee": {"cost": 0.25, "currency": "USDT"},
        }
        out, code = _run_script(response, ["--symbol=BTC", "--mode=live"])
        assert code == 0
        assert out["close"]["symbol"] == "BTC"
        fill = out["close"]["fill"]
        assert fill["avg_px"] == 42000.5
        assert fill["total_sz"] == 0.01
        assert fill["oid"] == "12345abc"
        assert fill["fee"] == 0.25

    def test_minimal_fill_fields(self):
        """Older/degenerate ccxt responses may omit optional fields. Must
        still succeed — we don't have a way to re-fetch telemetry, and the
        kill switch only cares about the binary close-submitted signal."""
        out, code = _run_script({"id": "x"}, ["--symbol=BTC", "--mode=live"])
        assert code == 0
        assert out["close"]["symbol"] == "BTC"
        assert out["close"]["fill"].get("oid") == "x"


class TestAlreadyFlat:
    """Empty dict == adapter found no position. Must be success with empty
    fill so the kill switch can release the latch during the eventual-
    consistency window between Go-side fetch and this submit."""

    def test_empty_response_is_success(self):
        out, code = _run_script({}, ["--symbol=BTC", "--mode=live"])
        assert code == 0, out
        assert out["close"]["symbol"] == "BTC"
        assert out["close"]["fill"] == {}
        assert "error" not in out


class TestFailurePaths:
    """Every path that means "close was NOT confirmed" must emit error +
    exit 1 so the Go caller latches the kill switch for retry."""

    def test_adapter_raises(self):
        out, code = _run_script(RuntimeError("OKX auth failed"),
                                ["--symbol=BTC", "--mode=live"])
        assert code == 1
        assert "OKX auth" in out["error"]
        assert out["close"]["fill"] == {}

    def test_non_dict_response(self):
        """Defensive: unexpected ccxt response shape (list, None, string)
        must not be treated as success — we don't know what it means."""
        out, code = _run_script("unexpected string", ["--symbol=BTC", "--mode=live"])
        assert code == 1
        assert "unexpected adapter response type" in out["error"]


if __name__ == "__main__":
    sys.exit(pytest.main([__file__, "-v"]))
