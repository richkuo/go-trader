"""Tests for fetch_hl_user_fills.py — userFills paging + OID aggregation
used by `go-trader backfill hl-fees` (issue #589).

Pattern mirrors test_close_hyperliquid_position.py: load the script as a
module, mock the HL adapter, run main() with --since-ms, capture stdout +
exit code, assert on the JSON envelope.
"""

import builtins
import importlib.util
import json
import os
import sys
from io import StringIO
from unittest.mock import MagicMock, patch

import pytest


def _run_script(pages_or_exc, argv, account_address="0xabc"):
    """Helper: invoke fetch_hl_user_fills.main() with a mocked adapter.

    pages_or_exc is either:
      - a list of pages (each page = list of fill dicts) returned by
        successive user_fills_by_time calls, OR
      - an Exception instance to raise on the first call.

    Returns (parsed_stdout_json, exit_code).
    """
    script_path = os.path.join(os.path.dirname(os.path.abspath(__file__)),
                               "fetch_hl_user_fills.py")
    spec = importlib.util.spec_from_file_location("fetch_hl_user_fills", script_path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)

    mock_adapter = MagicMock()
    mock_adapter._account_address = account_address
    mock_adapter._info = MagicMock()
    if isinstance(pages_or_exc, Exception):
        mock_adapter._info.user_fills_by_time.side_effect = pages_or_exc
    else:
        mock_adapter._info.user_fills_by_time.side_effect = pages_or_exc

    mock_adapter_cls = MagicMock(return_value=mock_adapter)

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
         patch("sys.argv", ["fetch_hl_user_fills.py"] + argv), \
         patch.object(mod.sys, "exit", side_effect=mock_exit):
        try:
            mod.main()
        except SystemExit:
            pass

    raw = captured.getvalue().strip()
    parsed = json.loads(raw) if raw else {}
    return parsed, exit_code["value"]


class TestSinglePage:
    def test_aggregates_per_oid(self):
        # Two fills, different OIDs.
        page = [
            {"oid": 111, "coin": "ETH", "time": 1000, "fee": "0.40", "closedPnl": "0", "tid": 1},
            {"oid": 222, "coin": "BTC", "time": 1100, "fee": "0.30", "closedPnl": "9.7", "tid": 2},
        ]
        # Adapter returns the page once, then [] to terminate the loop.
        out, code = _run_script([page, []], ["--since-ms=500"])
        assert code == 0, out
        assert out["error"] == ""
        assert out["fill_count"] == 2
        assert out["by_oid"]["111"]["fee"] == pytest.approx(0.40)
        assert out["by_oid"]["222"]["fee"] == pytest.approx(0.30)
        assert out["by_oid"]["222"]["closed_pnl"] == pytest.approx(9.7)
        assert out["by_oid"]["111"]["count"] == 1
        assert out["by_oid"]["111"]["coin"] == "ETH"
        assert out["by_oid"]["111"]["first_time_ms"] == 1000
        assert out["by_oid"]["222"]["last_time_ms"] == 1100

    def test_partial_fills_same_oid_summed(self):
        # One OID, two partial-fill rows — script must sum fee + closed_pnl.
        page = [
            {"oid": 111, "coin": "eth", "time": 1000, "fee": "0.20", "closedPnl": "5.0", "tid": 1},
            {"oid": 111, "coin": "ETH", "time": 1200, "fee": "0.20", "closedPnl": "4.7", "tid": 2},
        ]
        out, code = _run_script([page, []], ["--since-ms=500"])
        assert code == 0, out
        entry = out["by_oid"]["111"]
        assert entry["fee"] == pytest.approx(0.40)
        assert entry["closed_pnl"] == pytest.approx(9.7)
        assert entry["count"] == 2
        assert entry["coin"] == "ETH"
        assert entry["first_time_ms"] == 1000
        assert entry["last_time_ms"] == 1200

    def test_conflicting_coin_metadata_fails_closed(self):
        page = [
            {"oid": 111, "coin": "ETH", "time": 1000, "fee": "0.20", "closedPnl": "5.0", "tid": 1},
            {"oid": 111, "coin": "BTC", "time": 1200, "fee": "0.20", "closedPnl": "4.7", "tid": 2},
        ]
        out, code = _run_script([page, []], ["--since-ms=500"])
        assert code == 0, out
        assert out["by_oid"]["111"]["coin"] == ""


class TestMultiPage:
    def test_advances_cursor_to_last_fill_time(self):
        page1 = [
            {"oid": 111, "time": 1000, "fee": "0.10", "closedPnl": "0", "tid": 1},
            {"oid": 222, "time": 2000, "fee": "0.20", "closedPnl": "0", "tid": 2},
        ]
        page2 = [
            # Same time as page1's last fill, different tid — must NOT be deduped
            # (different leg of a different OID at the same ms boundary).
            {"oid": 333, "time": 2000, "fee": "0.30", "closedPnl": "0", "tid": 3},
            {"oid": 444, "time": 3000, "fee": "0.40", "closedPnl": "0", "tid": 4},
        ]
        out, code = _run_script([page1, page2, []], ["--since-ms=500"])
        assert code == 0, out
        assert set(out["by_oid"].keys()) == {"111", "222", "333", "444"}
        assert out["page_count"] >= 2

    def test_dedups_boundary_row_seen_in_prior_page(self):
        # The same fill (same tid + same time as the cursor) reappears on
        # the next page — must NOT be double-counted.
        page1 = [
            {"oid": 111, "time": 1000, "fee": "0.10", "closedPnl": "0", "tid": 1},
            {"oid": 222, "time": 2000, "fee": "0.20", "closedPnl": "0", "tid": 2},
        ]
        page2 = [
            {"oid": 222, "time": 2000, "fee": "0.20", "closedPnl": "0", "tid": 2},  # dup
            {"oid": 333, "time": 3000, "fee": "0.30", "closedPnl": "0", "tid": 3},
        ]
        out, code = _run_script([page1, page2, []], ["--since-ms=500"])
        assert code == 0, out
        # OID 222 should have count=1, not 2.
        assert out["by_oid"]["222"]["count"] == 1
        assert out["by_oid"]["222"]["fee"] == pytest.approx(0.20)


class TestErrorPaths:
    def test_missing_account_address_errors_cleanly(self):
        out, code = _run_script([], ["--since-ms=500"], account_address="")
        assert code == 1
        assert "HYPERLIQUID_ACCOUNT_ADDRESS" in out["error"]
        assert out["by_oid"] == {}

    def test_invalid_since_ms_errors(self):
        out, code = _run_script([], ["--since-ms=0"], account_address="0xabc")
        assert code == 1
        assert "since-ms" in out["error"]

    def test_user_fills_exception_surfaces(self):
        out, code = _run_script(RuntimeError("indexer down"),
                                ["--since-ms=500"], account_address="0xabc")
        assert code == 1
        assert "user_fills_by_time" in out["error"]
        assert "indexer down" in out["error"]


class TestNoFills:
    def test_empty_first_page_terminates(self):
        out, code = _run_script([[]], ["--since-ms=500"])
        assert code == 0
        assert out["fill_count"] == 0
        assert out["by_oid"] == {}
        assert out["page_count"] == 1
