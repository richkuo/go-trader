
import builtins
import contextlib
import importlib.util
import json
import os
import sys
from io import StringIO
from unittest.mock import MagicMock, patch

import pytest


@contextlib.contextmanager
def _case(*label):
    try:
        yield
    except (Exception, pytest.fail.Exception) as exc:
        raise AssertionError(f"case {label!r} failed: {exc}") from exc


def _run_script(pages_or_exc, argv, account_address="0xabc"):
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
    def test_fills_aggregate_per_oid(self, subtests):
        with subtests.test(case="aggregates_per_oid"):
            page = [
                {"oid": 111, "coin": "ETH", "time": 1000, "fee": "0.40", "closedPnl": "0", "tid": 1},
                {"oid": 222, "coin": "BTC", "time": 1100, "fee": "0.30", "closedPnl": "9.7", "tid": 2},
            ]
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

        with subtests.test(case="partial_fills_same_oid_summed"):
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

        with subtests.test(case="conflicting_coin_metadata_fails_closed"):
            page = [
                {"oid": 111, "coin": "ETH", "time": 1000, "fee": "0.20", "closedPnl": "5.0", "tid": 1},
                {"oid": 111, "coin": "BTC", "time": 1200, "fee": "0.20", "closedPnl": "4.7", "tid": 2},
            ]
            out, code = _run_script([page, []], ["--since-ms=500"])
            assert code == 0, out
            assert out["by_oid"]["111"]["coin"] == ""


class TestMultiPage:
    def test_pages_advance_dedup_and_terminate(self, subtests):
        with subtests.test(case="advances_cursor_to_last_fill_time"):
            page1 = [
                {"oid": 111, "time": 1000, "fee": "0.10", "closedPnl": "0", "tid": 1},
                {"oid": 222, "time": 2000, "fee": "0.20", "closedPnl": "0", "tid": 2},
            ]
            page2 = [
                {"oid": 333, "time": 2000, "fee": "0.30", "closedPnl": "0", "tid": 3},
                {"oid": 444, "time": 3000, "fee": "0.40", "closedPnl": "0", "tid": 4},
            ]
            out, code = _run_script([page1, page2, []], ["--since-ms=500"])
            assert code == 0, out
            assert set(out["by_oid"].keys()) == {"111", "222", "333", "444"}
            assert out["page_count"] >= 2

        with subtests.test(case="dedups_boundary_row_seen_in_prior_page"):
            page1 = [
                {"oid": 111, "time": 1000, "fee": "0.10", "closedPnl": "0", "tid": 1},
                {"oid": 222, "time": 2000, "fee": "0.20", "closedPnl": "0", "tid": 2},
            ]
            page2 = [
                {"oid": 222, "time": 2000, "fee": "0.20", "closedPnl": "0", "tid": 2},
                {"oid": 333, "time": 3000, "fee": "0.30", "closedPnl": "0", "tid": 3},
            ]
            out, code = _run_script([page1, page2, []], ["--since-ms=500"])
            assert code == 0, out
            assert out["by_oid"]["222"]["count"] == 1
            assert out["by_oid"]["222"]["fee"] == pytest.approx(0.20)

        with subtests.test(case="empty_first_page_terminates"):
            out, code = _run_script([[]], ["--since-ms=500"])
            assert code == 0
            assert out["fill_count"] == 0
            assert out["by_oid"] == {}
            assert out["page_count"] == 1


def test_error_paths_exit_one_with_json_envelope():
    cases = [
        ([], ["--since-ms=500"], "", ["HYPERLIQUID_ACCOUNT_ADDRESS"]),
        ([], ["--since-ms=0"], "0xabc", ["since-ms"]),
        (RuntimeError("indexer down"), ["--since-ms=500"], "0xabc",
         ["user_fills_by_time", "indexer down"]),
    ]
    for pages_or_exc, argv, account_address, fragments in cases:
        with _case(argv, account_address, fragments):
            out, code = _run_script(pages_or_exc, argv, account_address=account_address)
            assert code == 1
            for fragment in fragments:
                assert fragment in out["error"]
            assert out["by_oid"] == {}
