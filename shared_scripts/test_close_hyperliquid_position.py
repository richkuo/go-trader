"""Tests for close_hyperliquid_position.py — SDK response parsing for the
portfolio kill switch (#341).

The kill switch is the last line of defense. Any response shape that the
script treats as success — when the position actually is NOT closed on-chain
— silently clears virtual state while exposure persists (the original #341
failure mode shifted into the Python layer). These tests pin the contract
for every branch of the SDK response parser.

Pattern mirrors test_check_hyperliquid.py: load the script as a module, mock
the `adapter` import, run main() with --symbol=... --mode=live, capture
stdout + exit code.
"""

import builtins
import importlib.util
import json
import os
import sys
from io import StringIO
from unittest.mock import MagicMock, patch

import pytest


def _run_script(sdk_response_or_exc, argv):
    """Helper: invoke close_hyperliquid_position.main() with a mocked adapter.

    sdk_response_or_exc may be either a dict (returned by adapter.market_close)
    or an Exception subclass (raised by market_close). argv is the list
    passed to main() as sys.argv (excluding the program name).

    Returns (parsed_stdout_json, exit_code). exit_code is 0 on clean return
    or whatever value sys.exit was called with.
    """
    script_path = os.path.join(os.path.dirname(os.path.abspath(__file__)),
                               "close_hyperliquid_position.py")
    spec = importlib.util.spec_from_file_location("close_hyperliquid_position", script_path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)

    mock_adapter_cls = MagicMock()
    mock_adapter = MagicMock()
    mock_adapter_cls.return_value = mock_adapter
    if isinstance(sdk_response_or_exc, Exception):
        mock_adapter.market_close.side_effect = sdk_response_or_exc
    else:
        mock_adapter.market_close.return_value = sdk_response_or_exc

    captured = StringIO()
    exit_code = {"value": 0}

    original_import = builtins.__import__

    def mock_import(name, *args, **kwargs):
        if name == "adapter":
            fake_mod = MagicMock()
            fake_mod.HyperliquidExchangeAdapter = mock_adapter_cls
            return fake_mod
        return original_import(name, *args, **kwargs)

    def mock_exit(code=0):
        exit_code["value"] = code
        raise SystemExit(code)

    with patch("builtins.__import__", side_effect=mock_import), \
         patch("sys.stdout", captured), \
         patch("sys.argv", ["close_hyperliquid_position.py"] + argv), \
         patch.object(mod.sys, "exit", side_effect=mock_exit):
        try:
            mod.main()
        except SystemExit:
            pass

    raw = captured.getvalue().strip()
    parsed = json.loads(raw) if raw else {}
    return parsed, exit_code["value"]


class TestPaperModeRejected:
    """--mode=live is required for kill-switch invocation. Any other value
    must refuse to place an order — a regression here could arm the kill
    switch against a paper SDK stub with unpredictable consequences."""

    def test_paper_mode_exits_nonzero(self):
        out, code = _run_script({}, ["--symbol=ETH", "--mode=paper"])
        assert code == 1
        assert "--mode=live required" in out["error"]
        assert out["close"] is None

    def test_default_mode_is_live(self):
        """No --mode flag → default live. Must go through to the adapter."""
        sdk_response = {
            "status": "ok",
            "response": {"type": "order", "data": {"statuses": [
                {"filled": {"avgPx": "3000", "totalSz": "0.5"}}
            ]}},
        }
        out, code = _run_script(sdk_response, ["--symbol=ETH"])
        assert code == 0, out
        assert "error" not in out


class TestSuccessFill:
    """statuses[0] has `filled` → success path."""

    def test_filled_with_all_fields(self):
        sdk_response = {
            "status": "ok",
            "response": {"type": "order", "data": {"statuses": [
                {"filled": {"avgPx": "3000.5", "totalSz": "0.517",
                            "oid": 9999999, "fee": "1.25"}}
            ]}},
        }
        out, code = _run_script(sdk_response, ["--symbol=ETH", "--mode=live"])
        assert code == 0
        assert out["close"]["symbol"] == "ETH"
        fill = out["close"]["fill"]
        assert fill["avg_px"] == 3000.5
        assert fill["total_sz"] == 0.517
        assert fill["oid"] == 9999999
        # Fee extraction is required for #341 close-accounting parity with
        # the execute path — regression would silently drop exchange fees.
        assert fill["fee"] == 1.25

    def test_filled_missing_optional_fields(self):
        """Older SDK responses / short paths may omit oid and fee."""
        sdk_response = {
            "status": "ok",
            "response": {"type": "order", "data": {"statuses": [
                {"filled": {"avgPx": "50000", "totalSz": "0.01"}}
            ]}},
        }
        out, code = _run_script(sdk_response, ["--symbol=BTC", "--mode=live"])
        assert code == 0
        fill = out["close"]["fill"]
        assert fill["avg_px"] == 50000
        assert fill["total_sz"] == 0.01
        assert "oid" not in fill
        assert "fee" not in fill


class TestAlreadyFlat:
    """Empty statuses (HL had nothing to close) — success with empty fill.
    This complements the Go-side szi==0 upstream filter for the eventual-
    consistency window: if on-chain flattens between Go's fetch and our
    submit, the SDK returns an empty statuses list and we must NOT treat
    it as an error (which would latch the kill switch forever)."""

    def test_empty_statuses_is_success(self):
        sdk_response = {"status": "ok", "response": {"type": "order", "data": {"statuses": []}}}
        out, code = _run_script(sdk_response, ["--symbol=ETH", "--mode=live"])
        assert code == 0, out
        assert out["close"]["symbol"] == "ETH"
        assert out["close"]["fill"] == {}
        assert "error" not in out
        # already_flat must be set so the Go side routes this through
        # AlreadyFlat instead of ClosedCoins (#350) — operator messaging
        # must distinguish "we sent a close order" from "nothing to close".
        assert out["close"]["already_flat"] is True

    def test_no_response_field(self):
        """Some SDK paths omit response entirely for a flat account — handled
        the same as empty statuses."""
        sdk_response = {"status": "ok"}
        out, code = _run_script(sdk_response, ["--symbol=ETH", "--mode=live"])
        assert code == 0
        assert out["close"]["already_flat"] is True


class TestFailurePaths:
    """Every path that means "close was NOT confirmed" must emit error + exit 1
    so the Go caller latches the kill switch for retry."""

    def test_outer_status_not_ok(self):
        sdk_response = {"status": "err", "response": {"msg": "nonce too low"}}
        out, code = _run_script(sdk_response, ["--symbol=ETH", "--mode=live"])
        assert code == 1
        assert "sdk status='err'" in out["error"]

    def test_per_status_error(self):
        """Outer status='ok' but inner status has an error — kill switch must
        NOT report success. This was the bot review's #1 finding."""
        sdk_response = {
            "status": "ok",
            "response": {"type": "order", "data": {"statuses": [
                {"error": "Order reduce-only would not reduce position"}
            ]}},
        }
        out, code = _run_script(sdk_response, ["--symbol=ETH", "--mode=live"])
        assert code == 1
        assert "per-status error" in out["error"]
        assert "reduce-only would not reduce" in out["error"]

    def test_per_status_resting(self):
        """A market_close should never produce a resting order — but if the
        SDK ever returns one, treat it as failure: resting means not filled
        means not closed on-chain."""
        sdk_response = {
            "status": "ok",
            "response": {"type": "order", "data": {"statuses": [
                {"resting": {"oid": 12345}}
            ]}},
        }
        out, code = _run_script(sdk_response, ["--symbol=ETH", "--mode=live"])
        assert code == 1
        assert "close not filled" in out["error"]
        assert "resting" in out["error"]

    def test_adapter_raises(self):
        """Adapter-level exception (network error, credentials, etc.) —
        script must exit 1 with the error surfaced in the envelope."""
        out, code = _run_script(RuntimeError("HYPERLIQUID_SECRET_KEY not set"),
                                ["--symbol=ETH", "--mode=live"])
        assert code == 1
        assert "HYPERLIQUID_SECRET_KEY" in out["error"]
        assert out["close"]["fill"] == {}

    def test_non_dict_response(self):
        """Defensive: if the SDK ever returns a non-dict (older versions did),
        don't crash with an opaque parse error — surface a clear error."""
        out, code = _run_script("unexpected string response",
                                ["--symbol=ETH", "--mode=live"])
        assert code == 1
        assert "unexpected SDK response type" in out["error"]


def _run_script_with_cancel(sdk_response, cancel_response, argv):
    """Variant of _run_script that also stubs adapter.cancel_trigger_order.
    cancel_response is either a dict (returned) or an Exception (raised)."""
    script_path = os.path.join(os.path.dirname(os.path.abspath(__file__)),
                               "close_hyperliquid_position.py")
    spec = importlib.util.spec_from_file_location("close_hyperliquid_position", script_path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)

    mock_adapter_cls = MagicMock()
    mock_adapter = MagicMock()
    mock_adapter_cls.return_value = mock_adapter
    mock_adapter.market_close.return_value = sdk_response
    if isinstance(cancel_response, Exception):
        mock_adapter.cancel_trigger_order.side_effect = cancel_response
    else:
        mock_adapter.cancel_trigger_order.return_value = cancel_response

    captured = StringIO()
    exit_code = {"value": 0}

    original_import = builtins.__import__

    def mock_import(name, *args, **kwargs):
        if name == "adapter":
            fake_mod = MagicMock()
            fake_mod.HyperliquidExchangeAdapter = mock_adapter_cls
            return fake_mod
        return original_import(name, *args, **kwargs)

    def mock_exit(code=0):
        exit_code["value"] = code
        raise SystemExit(code)

    with patch("builtins.__import__", side_effect=mock_import), \
         patch("sys.stdout", captured), \
         patch("sys.argv", ["close_hyperliquid_position.py"] + argv), \
         patch.object(mod.sys, "exit", side_effect=mock_exit):
        try:
            mod.main()
        except SystemExit:
            pass

    raw = captured.getvalue().strip()
    parsed = json.loads(raw) if raw else {}
    return parsed, exit_code["value"], mock_adapter


class TestCancelStopLossOID:
    """#421 review point 1: per-strategy CB / portfolio-kill close paths must
    cancel the resting SL trigger before flattening so HL's 10/day account-wide
    trigger-order cap doesn't fill up with orphans."""

    def _filled_response(self, sym="ETH"):
        return {
            "status": "ok",
            "response": {"type": "order", "data": {"statuses": [
                {"filled": {"avgPx": "3000", "totalSz": "1.0", "oid": 999}}
            ]}},
        }

    def test_no_cancel_when_oid_zero(self):
        """Default behavior preserved: omitting --cancel-stop-loss-oid (or
        passing 0) must not call cancel_trigger_order."""
        out, code, adapter = _run_script_with_cancel(
            self._filled_response(), {"status": "ok"},
            ["--symbol=ETH", "--mode=live"])
        assert code == 0
        adapter.cancel_trigger_order.assert_not_called()
        assert "cancel_stop_loss_succeeded" not in out
        assert "cancel_stop_loss_error" not in out

    def test_cancel_succeeded_surfaces_in_envelope(self):
        out, code, adapter = _run_script_with_cancel(
            self._filled_response(), {"status": "ok"},
            ["--symbol=ETH", "--mode=live", "--cancel-stop-loss-oid=12345"])
        assert code == 0
        adapter.cancel_trigger_order.assert_called_once_with("ETH", 12345)
        assert out.get("cancel_stop_loss_succeeded") is True

    def test_cancel_failure_is_non_fatal(self):
        """Cancel may fail because the SL already triggered — close should
        still proceed and the failure is surfaced for the Go side to log."""
        out, code, adapter = _run_script_with_cancel(
            self._filled_response(), RuntimeError("order not found"),
            ["--symbol=ETH", "--mode=live", "--cancel-stop-loss-oid=999"])
        assert code == 0  # close still succeeded
        assert "order not found" in out.get("cancel_stop_loss_error", "")
        assert "cancel_stop_loss_succeeded" not in out

    def test_cancel_state_propagates_through_close_failure(self):
        """If close fails after cancel succeeds, the envelope must still
        report cancel_stop_loss_succeeded so the Go side can clear the
        dead OID from pos.StopLossOID — same contract as the execute path
        from #421's CancelStopLossSucceeded field."""
        sdk_response = {
            "status": "ok",
            "response": {"type": "order", "data": {"statuses": [
                {"error": "no position to close"}
            ]}},
        }
        out, code, adapter = _run_script_with_cancel(
            sdk_response, {"status": "ok"},
            ["--symbol=ETH", "--mode=live", "--cancel-stop-loss-oid=12345"])
        assert code == 1
        assert "per-status error" in out["error"]
        assert out.get("cancel_stop_loss_succeeded") is True


if __name__ == "__main__":
    sys.exit(pytest.main([__file__, "-v"]))
