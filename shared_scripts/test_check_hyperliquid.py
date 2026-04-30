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


class TestMarginMode:
    """#486: run_execute calls update_leverage with isolated/cross before placing
    the market order. Failure of update_leverage must abort the order (fail closed)
    so a bad config can't silently land in the wrong margin mode."""

    def _run_execute_with_margin(self, margin_mode, leverage, update_leverage_side_effect=None):
        mod, spec = _load_check_module()
        spec.loader.exec_module(mod)

        mock_adapter_cls = MagicMock()
        mock_adapter = MagicMock()
        mock_adapter_cls.return_value = mock_adapter
        if update_leverage_side_effect is not None:
            mock_adapter.update_leverage.side_effect = update_leverage_side_effect
        mock_adapter.market_open.return_value = {
            "status": "ok",
            "response": {"type": "order", "data": {"statuses": [
                {"filled": {"avgPx": "50000", "totalSz": "0.01"}}
            ]}},
        }

        captured = StringIO()
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
                exit_code = 0
                try:
                    mod.run_execute("BTC", "buy", 0.01, "live",
                                    margin_mode=margin_mode, leverage=leverage)
                except SystemExit as e:
                    exit_code = e.code
        return json.loads(captured.getvalue()), mock_adapter, exit_code

    def test_isolated_calls_update_leverage_with_is_cross_false(self):
        result, adapter, exit_code = self._run_execute_with_margin("isolated", 5)
        assert exit_code == 0
        adapter.update_leverage.assert_called_once_with(5, "BTC", is_cross=False)
        adapter.market_open.assert_called_once()
        assert result["execution"]["action"] == "buy"

    def test_cross_calls_update_leverage_with_is_cross_true(self):
        result, adapter, exit_code = self._run_execute_with_margin("cross", 3)
        assert exit_code == 0
        adapter.update_leverage.assert_called_once_with(3, "BTC", is_cross=True)
        adapter.market_open.assert_called_once()

    def test_no_margin_mode_skips_update_leverage(self):
        result, adapter, exit_code = self._run_execute_with_margin("", 0)
        assert exit_code == 0
        adapter.update_leverage.assert_not_called()
        adapter.market_open.assert_called_once()

    def test_invalid_margin_mode_fails_closed(self):
        result, adapter, exit_code = self._run_execute_with_margin("portfolio", 5)
        assert exit_code == 1
        adapter.update_leverage.assert_not_called()
        adapter.market_open.assert_not_called()
        assert "invalid margin_mode" in result.get("error", "")

    def test_zero_leverage_with_mode_fails_closed(self):
        result, adapter, exit_code = self._run_execute_with_margin("isolated", 0)
        assert exit_code == 1
        adapter.update_leverage.assert_not_called()
        adapter.market_open.assert_not_called()
        assert "leverage" in result.get("error", "").lower()

    def test_update_leverage_failure_aborts_order(self):
        result, adapter, exit_code = self._run_execute_with_margin(
            "isolated", 5,
            update_leverage_side_effect=RuntimeError("HL rejected: position open"),
        )
        assert exit_code == 1
        adapter.update_leverage.assert_called_once()
        adapter.market_open.assert_not_called()
        assert "update_leverage failed" in result.get("error", "")
        assert "position open" in result.get("error", "")


class TestPeerLeverageSkip:
    """#491: when a peer strategy has already opened the same coin, HL has
    (margin_mode, leverage) pinned to the existing on-chain position. A fresh
    update_leverage call would fail, so run_execute queries get_position_leverage
    and skips the call when state already matches. LoadConfig validates that
    peers agree on (margin_mode, leverage), so a match is the expected case
    when peers share a coin."""

    def _run_execute_with_existing_pos(self, margin_mode, leverage, current_state):
        mod, spec = _load_check_module()
        spec.loader.exec_module(mod)

        mock_adapter_cls = MagicMock()
        mock_adapter = MagicMock()
        mock_adapter_cls.return_value = mock_adapter
        mock_adapter.get_position_leverage.return_value = current_state
        mock_adapter.market_open.return_value = {
            "status": "ok",
            "response": {"type": "order", "data": {"statuses": [
                {"filled": {"avgPx": "50000", "totalSz": "0.01"}}
            ]}},
        }

        captured = StringIO()
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
                exit_code = 0
                try:
                    mod.run_execute("ETH", "buy", 0.5, "live",
                                    margin_mode=margin_mode, leverage=leverage)
                except SystemExit as e:
                    exit_code = e.code
        return json.loads(captured.getvalue()), mock_adapter, exit_code

    def test_skips_update_leverage_when_state_matches(self):
        result, adapter, exit_code = self._run_execute_with_existing_pos(
            "isolated", 5, {"margin_mode": "isolated", "leverage": 5})
        assert exit_code == 0
        adapter.update_leverage.assert_not_called()
        adapter.market_open.assert_called_once()
        assert result["execution"]["action"] == "buy"

    def test_calls_update_leverage_when_mode_mismatches(self):
        result, adapter, exit_code = self._run_execute_with_existing_pos(
            "isolated", 5, {"margin_mode": "cross", "leverage": 5})
        adapter.update_leverage.assert_called_once_with(5, "ETH", is_cross=False)

    def test_calls_update_leverage_when_leverage_mismatches(self):
        result, adapter, exit_code = self._run_execute_with_existing_pos(
            "isolated", 5, {"margin_mode": "isolated", "leverage": 3})
        adapter.update_leverage.assert_called_once_with(5, "ETH", is_cross=False)

    def test_calls_update_leverage_when_no_existing_position(self):
        # get_position_leverage returns None when HL has no open position
        # for the coin — then update_leverage is safe to call (HL only
        # rejects mode changes on an OPEN position).
        result, adapter, exit_code = self._run_execute_with_existing_pos(
            "isolated", 5, None)
        adapter.update_leverage.assert_called_once_with(5, "ETH", is_cross=False)

    def test_state_fetch_failure_falls_back_to_calling_update_leverage(self):
        # If get_position_leverage raises, fall back to calling
        # update_leverage so the existing fail-closed safety net catches a
        # genuine mismatch — never silently skip without confirmation.
        mod, spec = _load_check_module()
        spec.loader.exec_module(mod)

        mock_adapter_cls = MagicMock()
        mock_adapter = MagicMock()
        mock_adapter_cls.return_value = mock_adapter
        mock_adapter.get_position_leverage.side_effect = RuntimeError("info endpoint timeout")
        mock_adapter.market_open.return_value = {
            "status": "ok",
            "response": {"type": "order", "data": {"statuses": [
                {"filled": {"avgPx": "50000", "totalSz": "0.01"}}
            ]}},
        }

        captured = StringIO()
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
                exit_code = 0
                try:
                    mod.run_execute("ETH", "buy", 0.5, "live",
                                    margin_mode="isolated", leverage=5)
                except SystemExit as e:
                    exit_code = e.code

        assert exit_code == 0
        mock_adapter.update_leverage.assert_called_once_with(5, "ETH", is_cross=False)


class TestClassifySLResponse:
    """Unit coverage for _classify_sl_response added in #421. The classifier
    is the load-bearing piece that distinguishes a resting SL from an instant
    fill or rejection — getting it wrong means either virtual state thinks
    the position is open when it's flat, or the scheduler treats a happy
    instant fill as a placement error."""

    def _classify(self, response):
        mod, spec = _load_check_module()
        spec.loader.exec_module(mod)
        return mod._classify_sl_response(response)

    def test_resting(self):
        kind, oid = self._classify({
            "response": {"type": "order", "data": {"statuses": [
                {"resting": {"oid": 12345}}
            ]}}
        })
        assert kind == "resting"
        assert oid == 12345

    def test_resting_missing_oid_returns_zero(self):
        kind, oid = self._classify({
            "response": {"type": "order", "data": {"statuses": [
                {"resting": {}}
            ]}}
        })
        assert kind == "resting"
        assert oid == 0

    def test_filled_immediate_with_oid(self):
        kind, oid = self._classify({
            "response": {"type": "order", "data": {"statuses": [
                {"filled": {"oid": 67890, "avgPx": "3000"}}
            ]}}
        })
        assert kind == "filled"
        assert oid == 67890

    def test_filled_immediate_without_oid(self):
        kind, oid = self._classify({
            "response": {"type": "order", "data": {"statuses": [
                {"filled": {}}
            ]}}
        })
        assert kind == "filled"
        assert oid == 0

    def test_per_status_error(self):
        kind, payload = self._classify({
            "response": {"type": "order", "data": {"statuses": [
                {"error": "Too many open trigger orders"}
            ]}}
        })
        assert kind == "error"
        assert "Too many" in payload

    def test_missing_when_no_statuses(self):
        kind, payload = self._classify({
            "response": {"type": "order", "data": {"statuses": []}}
        })
        assert kind == "missing"
        assert payload is None

    def test_missing_when_completely_malformed(self):
        kind, payload = self._classify({})
        assert kind == "missing"
        assert payload is None

    def test_missing_when_status_is_not_dict(self):
        kind, payload = self._classify({
            "response": {"type": "order", "data": {"statuses": ["not a dict"]}}
        })
        assert kind == "missing"
        assert payload is None
