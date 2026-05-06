"""Tests for check_hyperliquid.py — specifically the fill extraction logic."""

import sys
import os
import json
import importlib.util
from unittest.mock import MagicMock, patch
from io import StringIO

import pytest


_UNSET = object()


def _load_check_module():
    """Load check_hyperliquid.py as a module."""
    script_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "check_hyperliquid.py")
    spec = importlib.util.spec_from_file_location("check_hyperliquid", script_path)
    mod = importlib.util.module_from_spec(spec)
    # Don't execute top-level code that sets up sys.path — we'll mock the adapter
    return mod, spec


class TestFillExtraction:
    """Test that run_execute extracts oid and fee from Hyperliquid SDK responses."""

    def _run_execute_with_mock_response(self, sdk_response, lookup_result=_UNSET):
        """Helper: mock the adapter and capture JSON output from run_execute."""
        mod, spec = _load_check_module()
        spec.loader.exec_module(mod)

        mock_adapter_cls = MagicMock()
        mock_adapter = MagicMock()
        mock_adapter_cls.return_value = mock_adapter
        mock_adapter.market_open.return_value = sdk_response
        if lookup_result is not _UNSET:
            mock_adapter.lookup_fill_fee_by_oid.return_value = lookup_result

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

    def test_fill_uses_numeric_lookup_result(self):
        """userFills lookup fee + closed PnL should be copied only as numbers."""
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
        result = self._run_execute_with_mock_response(
            sdk_response,
            lookup_result={"fee": "0.42", "closed_pnl": "3.14"},
        )
        fill = result["execution"]["fill"]
        assert fill["fee"] == 0.42
        assert fill["closed_pnl"] == 3.14

    def test_fill_ignores_truthy_non_mapping_lookup_result(self):
        """A bare MagicMock lookup result must not leak into JSON output."""
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
        result = self._run_execute_with_mock_response(sdk_response, lookup_result=MagicMock())
        fill = result["execution"]["fill"]
        assert fill["oid"] == 9876543210
        assert "fee" not in fill
        assert "closed_pnl" not in fill

    def test_fill_ignores_malformed_lookup_values(self):
        """Truthy dicts with non-numeric payloads are ignored."""
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
        result = self._run_execute_with_mock_response(
            sdk_response,
            lookup_result={"fee": MagicMock(), "closed_pnl": MagicMock()},
        )
        fill = result["execution"]["fill"]
        assert fill["oid"] == 9876543210
        assert "fee" not in fill
        assert "closed_pnl" not in fill

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


class TestUpdateStopLoss:
    """#501: trailing stops reuse cancel_trigger_order + place_stop_loss without
    submitting a market order."""

    def _run_update(self, side="long", place_response=None, cancel_side_effect=None):
        mod, spec = _load_check_module()
        spec.loader.exec_module(mod)

        mock_adapter_cls = MagicMock()
        mock_adapter = MagicMock()
        mock_adapter_cls.return_value = mock_adapter
        mock_adapter.round_perps_trigger_px.side_effect = lambda _symbol, px: round(px, 2)
        if cancel_side_effect is not None:
            mock_adapter.cancel_trigger_order.side_effect = cancel_side_effect
        mock_adapter.place_stop_loss.return_value = place_response or {
            "response": {"type": "order", "data": {"statuses": [
                {"resting": {"oid": 22222}}
            ]}}
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
                mod.run_update_stop_loss("ETH", side, 0.5, 3104.123, "live", cancel_oid=11111)
        return json.loads(captured.getvalue()), mock_adapter

    def test_cancel_then_place_long_stop(self):
        out, adapter = self._run_update(side="long")
        adapter.cancel_trigger_order.assert_called_once_with("ETH", 11111)
        adapter.place_stop_loss.assert_called_once_with("ETH", 0.5, 3104.12, False)
        method_names = [call[0] for call in adapter.method_calls]
        assert method_names.index("cancel_trigger_order") < method_names.index("place_stop_loss")
        assert out["cancel_stop_loss_succeeded"] is True
        assert out["stop_loss_oid"] == 22222
        assert out["stop_loss_trigger_px"] == 3104.12

    def test_short_stop_places_buy_trigger(self):
        out, adapter = self._run_update(side="short")
        adapter.place_stop_loss.assert_called_once_with("ETH", 0.5, 3104.12, True)
        assert out["stop_loss_oid"] == 22222


class TestCloseFullPosition:
    """#592: final-tier TP close uses market_close(sz=None) instead of market_open."""

    def _run_close_full(self, market_close_response=None):
        mod, spec = _load_check_module()
        spec.loader.exec_module(mod)

        mock_adapter_cls = MagicMock()
        mock_adapter = MagicMock()
        mock_adapter_cls.return_value = mock_adapter
        # Return {} so `if lookup:` is falsy and we skip fee overwrite (#585 path)
        mock_adapter.lookup_fill_fee_by_oid.return_value = {}
        mock_adapter.market_close.return_value = market_close_response or {
            "status": "ok",
            "response": {
                "type": "order",
                "data": {
                    "statuses": [
                        {
                            "filled": {
                                "avgPx": "3000.5",
                                "totalSz": "0.211",
                                "oid": 888,
                            }
                        }
                    ]
                },
            },
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
                mod.run_execute("ETH", "sell", 0.0, "live", close_full_position=True)

        return json.loads(captured.getvalue()), mock_adapter

    def test_uses_market_close_not_market_open(self):
        """close_full_position=True must call market_close(sz=None), not market_open."""
        _, adapter = self._run_close_full()
        adapter.market_close.assert_called_once_with("ETH", sz=None)
        adapter.market_open.assert_not_called()

    def test_output_shape_matches_sized_close(self):
        """JSON output shape must be identical to a sized close so Go consumer works unchanged."""
        out, _ = self._run_close_full()
        assert "execution" in out
        fill = out["execution"]["fill"]
        assert fill["avg_px"] == 3000.5
        assert fill["total_sz"] == 0.211
        assert fill["oid"] == 888

    def test_dust_scenario_closes_full_residual(self):
        """Regression for #592: TP2 after a 0.421 ETH position that was half-closed to 0.211.
        The close_full_position path closes the entire residual, not just 0.210."""
        _, adapter = self._run_close_full(market_close_response={
            "status": "ok",
            "response": {
                "type": "order",
                "data": {
                    "statuses": [
                        {
                            "filled": {
                                "avgPx": "3100.0",
                                "totalSz": "0.211",  # full residual, not 0.210
                                "oid": 999,
                            }
                        }
                    ]
                },
            },
        })
        # market_close called with sz=None — HL determines the size, not the caller
        adapter.market_close.assert_called_once_with("ETH", sz=None)


class TestSyncProtection:
    """#601 / #604 review #1: run_sync_protection branches for OID present /
    gone-but-cancelled / gone-but-filled. The over-close hazard arises when a
    TP OID dropped from open_orders because it actually filled (not because
    it was cancelled), and the script blindly re-places at the same price
    sized off the stale virtual qty."""

    def _run_sync(
        self,
        *,
        size=1.0,
        avg_cost=2000.0,
        entry_atr=20.0,
        side="long",
        sl_oid=0,
        tp1_oid=0,
        tp2_oid=0,
        tp_tiers=None,
        tp_oids=None,
        open_oids=None,
        fill_lookup_by_oid=None,
        place_responses=None,
    ):
        mod, spec = _load_check_module()
        spec.loader.exec_module(mod)

        mock_adapter_cls = MagicMock()
        mock_adapter = MagicMock()
        mock_adapter_cls.return_value = mock_adapter
        mock_adapter.open_order_oids.return_value = (
            set() if open_oids is None else set(open_oids)
        )
        mock_adapter.round_perps_trigger_px.side_effect = lambda _sym, px: round(px, 4)

        fills = fill_lookup_by_oid or {}

        def lookup_side_effect(oid, *args, **kwargs):
            return fills.get(int(oid), {})

        mock_adapter.lookup_fill_fee_by_oid.side_effect = lookup_side_effect

        responses = place_responses or {}

        def stop_loss_side_effect(*args, **kwargs):
            return responses.get("sl", {"status": "ok", "response": {"type": "order", "data": {"statuses": [{"resting": {"oid": 9000}}]}}})

        def tp_side_effect(symbol, sz, px, is_buy):
            # Return distinct OIDs for TP1 vs TP2 by detecting which call this
            # is via a counter on the side_effect itself.
            count = mock_adapter.place_take_profit_limit.call_count
            key = "tp1" if count == 1 else "tp2"
            return responses.get(key, {
                "status": "ok",
                "response": {"type": "order", "data": {"statuses": [{"resting": {"oid": 9100 + count}}]}}
            })

        mock_adapter.place_stop_loss.side_effect = stop_loss_side_effect
        mock_adapter.place_take_profit_limit.side_effect = tp_side_effect

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
                mod.run_sync_protection(
                    "ETH",
                    side,
                    size,
                    avg_cost,
                    entry_atr,
                    "live",
                    stop_loss_atr_mult=1.0,
                    tp1_atr_mult=1.0,
                    tp1_fraction=0.5,
                    tp2_atr_mult=2.0,
                    stop_loss_oid=sl_oid,
                    tp1_oid=tp1_oid,
                    tp2_oid=tp2_oid,
                    tp_tiers=tp_tiers,
                    tp_oids=tp_oids,
                )
        return json.loads(captured.getvalue()), mock_adapter

    def test_existing_oid_still_open_returns_same_oid(self):
        """OID still in open_orders → echo it back, do NOT call place_take_profit_limit."""
        out, adapter = self._run_sync(
            tp1_oid=200,
            tp2_oid=300,
            sl_oid=100,
            open_oids={100, 200, 300},
        )
        assert out["tp1_oid"] == 200
        assert out["tp2_oid"] == 300
        assert out["stop_loss_oid"] == 100
        adapter.place_take_profit_limit.assert_not_called()
        adapter.place_stop_loss.assert_not_called()
        adapter.lookup_fill_fee_by_oid.assert_not_called()

    def test_missing_oid_with_no_fill_places_replacement(self):
        """OID gone from open_orders AND not in userFills → cancelled, place new."""
        out, adapter = self._run_sync(
            tp1_oid=200,
            tp2_oid=300,
            open_oids=set(),  # empty — TP OIDs gone
            fill_lookup_by_oid={},  # no fills for any OID
        )
        # New OIDs surfaced
        assert "tp1_oid" in out
        assert "tp2_oid" in out
        # userFills was consulted to make sure the OID hadn't filled
        assert adapter.lookup_fill_fee_by_oid.called
        # New TPs placed
        assert adapter.place_take_profit_limit.call_count == 2
        # Filled-externally flag NOT set
        assert not out.get("tp1_filled_externally")
        assert not out.get("tp2_filled_externally")

    def test_missing_oid_with_fill_marks_externally_filled(self):
        """OID gone AND userFills shows a fill → filled externally; do NOT re-place. (#604 review #1)"""
        out, adapter = self._run_sync(
            tp1_oid=200,
            tp2_oid=300,
            open_oids=set(),
            fill_lookup_by_oid={
                200: {"fee": 0.05, "closed_pnl": 25.0, "count": 1},
                # TP2 still missing (cancelled, not filled)
            },
        )
        assert out.get("tp1_filled_externally") is True
        assert "tp1_fill" in out
        assert out["tp1_fill"]["fee"] == 0.05
        # TP2 should be placed since no fill found
        assert not out.get("tp2_filled_externally")
        # Only ONE place_take_profit_limit call (for TP2), because TP1 was filled.
        assert adapter.place_take_profit_limit.call_count == 1

    def test_three_tiers_places_incremental_sizes(self):
        """#612: N-tier protection sizes each order from cumulative fractions."""
        out, adapter = self._run_sync(
            size=10.0,
            tp_tiers=[
                {"atr_multiple": 1.0, "close_fraction": 0.5},
                {"atr_multiple": 2.0, "close_fraction": 0.8},
                {"atr_multiple": 3.0, "close_fraction": 1.0},
            ],
            tp_oids=[0, 0, 0],
            open_oids=set(),
        )
        assert len(out["tp_oids"]) == 3
        assert out["tp_pxs"] == [2020.0, 2040.0, 2060.0]
        sizes = [call.args[1] for call in adapter.place_take_profit_limit.call_args_list]
        assert sizes == pytest.approx([5.0, 3.0, 2.0])
        assert adapter.place_take_profit_limit.call_count == 3

    def test_three_tiers_detects_middle_oid_filled_externally(self):
        """#612: filled-externally detection is indexed, not hardcoded to TP1/TP2."""
        out, adapter = self._run_sync(
            tp_tiers=[
                {"atr_multiple": 1.0, "close_fraction": 0.5},
                {"atr_multiple": 2.0, "close_fraction": 0.8},
                {"atr_multiple": 3.0, "close_fraction": 1.0},
            ],
            tp_oids=[100, 200, 300],
            open_oids={100, 300},
            fill_lookup_by_oid={200: {"fee": 0.01, "closed_pnl": 7.0, "count": 1}},
        )
        assert out["tp_oids"] == [100, 0, 300]
        assert out["tp_filled_externally"] == [False, True, False]
        assert out["tp_fills"][1]["closed_pnl"] == 7.0
        adapter.place_take_profit_limit.assert_not_called()

    def test_open_orders_fetch_failure_defers_replacement(self):
        """open_order_oids() raise → leave existing OIDs alone, do not re-place
        (would double-up the protection). The script returns the failure
        marker so the Go side knows to retry next cycle."""
        mod, spec = _load_check_module()
        spec.loader.exec_module(mod)

        mock_adapter_cls = MagicMock()
        mock_adapter = MagicMock()
        mock_adapter_cls.return_value = mock_adapter
        mock_adapter.open_order_oids.side_effect = RuntimeError("indexer down")
        mock_adapter.round_perps_trigger_px.side_effect = lambda _sym, px: round(px, 4)

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
                mod.run_sync_protection(
                    "ETH", "long", 1.0, 2000.0, 20.0, "live",
                    stop_loss_atr_mult=1.0, tp1_atr_mult=1.0, tp1_fraction=0.5, tp2_atr_mult=2.0,
                    stop_loss_oid=100, tp1_oid=200, tp2_oid=300,
                )
        out = json.loads(captured.getvalue())
        assert out["open_order_check_error"] == "indexer down"
        # No re-placements issued — existing OIDs are left alone.
        mock_adapter.place_take_profit_limit.assert_not_called()
        mock_adapter.place_stop_loss.assert_not_called()
