"""Tests for fetch_okx_bills.py — OKX account-bills fetcher feeding the #1105
exchange-sourced cash-flow journal (shadow phase / Phase 3a of #1100).

The journal reconstructs the wallet TOTAL from this feed, so the script must
relay the adapter's bills + cap flag truthfully and emit a JSON envelope on
every path (success and error), exit 1 on error — the Go-side parser reads the
envelope regardless of exit code.
"""

import builtins
import importlib.util
import json
import os
import sys
from io import StringIO
from unittest.mock import MagicMock, patch

import pytest


def _run_script(bills_or_exc, capped=False, is_live=True, argv=None):
    """Invoke fetch_okx_bills.main() with a mocked adapter.

    bills_or_exc may be a list of bill dicts (returned by
    adapter.get_account_bills as (bills, capped)) or an Exception. Returns
    (parsed_stdout_json, exit_code).
    """
    script_path = os.path.join(os.path.dirname(os.path.abspath(__file__)),
                               "fetch_okx_bills.py")
    spec = importlib.util.spec_from_file_location("fetch_okx_bills", script_path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)

    mock_adapter_cls = MagicMock()
    mock_adapter = MagicMock()
    mock_adapter.is_live = is_live
    mock_adapter_cls.return_value = mock_adapter
    if isinstance(bills_or_exc, Exception):
        mock_adapter.get_account_bills.side_effect = bills_or_exc
    else:
        mock_adapter.get_account_bills.return_value = (bills_or_exc, capped)

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
         patch("sys.argv", argv or ["fetch_okx_bills.py", "--since-ms=123"]), \
         patch.object(mod.sys, "exit", side_effect=mock_exit):
        try:
            mod.main()
        except SystemExit:
            pass

    raw = captured.getvalue().strip()
    parsed = json.loads(raw) if raw else {}
    return parsed, exit_code["value"], mock_adapter


class TestSuccess:
    def test_relays_bills(self):
        bills = [
            {"bill_id": "b1", "ts_ms": 100, "ccy": "USDT", "type": "2", "bal_chg": 19.7},
            {"bill_id": "b2", "ts_ms": 200, "ccy": "USDT", "type": "8", "bal_chg": -1.0},
        ]
        out, code, _ = _run_script(bills)
        assert code == 0
        assert out["bills"] == bills
        assert out["capped"] is False
        assert "error" not in out

    def test_passes_since_ms_to_adapter(self):
        out, code, adapter = _run_script([], argv=["fetch_okx_bills.py", "--since-ms=987654321"])
        assert code == 0
        adapter.get_account_bills.assert_called_once_with(since_ms=987654321)

    def test_capped_flag_relayed(self):
        out, code, _ = _run_script([{"bill_id": "b", "ts_ms": 1, "bal_chg": 1.0}], capped=True)
        assert code == 0
        assert out["capped"] is True

    def test_empty_bills(self):
        out, code, _ = _run_script([])
        assert code == 0
        assert out["bills"] == []
        assert out["capped"] is False
        assert "error" not in out


class TestFailurePaths:
    def test_non_live_adapter(self):
        out, code, _ = _run_script([], is_live=False)
        assert code == 1
        assert "OKX_API_KEY" in out["error"]
        assert out["bills"] == []

    def test_adapter_raises(self):
        out, code, _ = _run_script(RuntimeError("OKX 503 Service Unavailable"))
        assert code == 1
        assert "OKX 503" in out["error"]
        assert out["bills"] == []
        assert out["capped"] is False


class TestArgParsing:
    def test_since_ms_defaults_to_zero(self):
        mod_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "fetch_okx_bills.py")
        spec = importlib.util.spec_from_file_location("fetch_okx_bills_args", mod_path)
        mod = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(mod)
        assert mod._parse_args([]).since_ms == 0
        assert mod._parse_args(["--since-ms=42"]).since_ms == 42


if __name__ == "__main__":
    sys.exit(pytest.main([__file__, "-v"]))
