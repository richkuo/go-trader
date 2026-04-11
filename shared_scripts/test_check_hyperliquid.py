"""Tests for check_hyperliquid.py — specifically the fill extraction logic."""

import sys
import os
import json
import importlib.util
from unittest.mock import MagicMock, patch
from io import StringIO

import pytest


def _load_check_module():
    """Load check_hyperliquid.py as a module."""
    script_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "check_hyperliquid.py")
    spec = importlib.util.spec_from_file_location("check_hyperliquid", script_path)
    mod = importlib.util.module_from_spec(spec)
    # Don't execute top-level code that sets up sys.path — we'll mock the adapter
    return mod, spec


class TestFillExtraction:
    """Test that run_execute extracts oid and fee from Hyperliquid SDK responses."""

    def _run_execute_with_mock_response(self, sdk_response):
        """Helper: mock the adapter and capture JSON output from run_execute."""
        mod, spec = _load_check_module()
        spec.loader.exec_module(mod)

        mock_adapter_cls = MagicMock()
        mock_adapter = MagicMock()
        mock_adapter_cls.return_value = mock_adapter
        mock_adapter.market_open.return_value = sdk_response

        captured = StringIO()
        with patch.dict(sys.modules, {}):
            with patch.object(mod, "__builtins__", mod.__builtins__):
                # Patch the import inside run_execute
                import builtins
                original_import = builtins.__import__

                def mock_import(name, *args, **kwargs):
                    if name == "adapter":
                        fake_mod = MagicMock()
                        fake_mod.HyperliquidExchangeAdapter = mock_adapter_cls
                        return fake_mod
                    return original_import(name, *args, **kwargs)

                with patch("builtins.__import__", side_effect=mock_import):
                    with patch("sys.stdout", captured):
                        mod.run_execute("BTC", "buy", 0.01, "live")

        return json.loads(captured.getvalue())

    def test_fill_with_oid_and_fee(self):
        """SDK response includes oid and fee — both should appear in output."""
        sdk_response = {
            "status": "ok",
            "response": {
                "type": "order",
                "data": {
                    "statuses": [
                        {
                            "filled": {
                                "avgPx": "55000.5",
                                "totalSz": "0.01",
                                "oid": 1234567890,
                                "fee": "0.35",
                            }
                        }
                    ]
                },
            },
        }
        result = self._run_execute_with_mock_response(sdk_response)
        fill = result["execution"]["fill"]
        assert fill["avg_px"] == 55000.5
        assert fill["total_sz"] == 0.01
        assert fill["oid"] == 1234567890
        assert fill["fee"] == 0.35

    def test_fill_with_oid_no_fee(self):
        """SDK response has oid but no fee — fee should be absent."""
        sdk_response = {
            "status": "ok",
            "response": {
                "type": "order",
                "data": {
                    "statuses": [
                        {
                            "filled": {
                                "avgPx": "2100.0",
                                "totalSz": "0.5",
                                "oid": 9876543210,
                            }
                        }
                    ]
                },
            },
        }
        result = self._run_execute_with_mock_response(sdk_response)
        fill = result["execution"]["fill"]
        assert fill["oid"] == 9876543210
        assert "fee" not in fill

    def test_fill_without_oid(self):
        """SDK response has no oid — backwards compatible with old responses."""
        sdk_response = {
            "status": "ok",
            "response": {
                "type": "order",
                "data": {
                    "statuses": [
                        {
                            "filled": {
                                "avgPx": "50000",
                                "totalSz": "0.1",
                            }
                        }
                    ]
                },
            },
        }
        result = self._run_execute_with_mock_response(sdk_response)
        fill = result["execution"]["fill"]
        assert fill["avg_px"] == 50000.0
        assert fill["total_sz"] == 0.1
        assert "oid" not in fill
        assert "fee" not in fill

    def test_fill_empty_statuses(self):
        """SDK response with empty statuses — fill should be empty dict."""
        sdk_response = {
            "status": "ok",
            "response": {"type": "order", "data": {"statuses": []}},
        }
        result = self._run_execute_with_mock_response(sdk_response)
        assert result["execution"]["fill"] == {}
