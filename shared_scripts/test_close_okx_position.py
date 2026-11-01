
import builtins
import importlib.util
import json
import os
import sys
from io import StringIO
from unittest.mock import MagicMock, patch

import pytest


def _run_script(adapter_response_or_exc, argv, is_live=True):
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
        mock_adapter.market_close_orders.side_effect = adapter_response_or_exc
    else:
        mock_adapter.market_close_orders.return_value = [adapter_response_or_exc] if adapter_response_or_exc else []

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


class TestNonLiveRejected:

    def test_paper_mode_or_non_live_adapter_exits_nonzero(self, subtests):
        with subtests.test(case="paper_mode"):
            out, code = _run_script({"id": "xx"}, ["--symbol=BTC", "--mode=paper"])
            assert code == 1
            assert "--mode=live required" in out["error"]
            assert out["close"] is None

        with subtests.test(case="non_live_adapter"):
            out, code = _run_script({}, ["--symbol=BTC", "--mode=live"], is_live=False)
            assert code == 1
            assert "OKX_API_KEY" in out["error"]
            assert out["close"]["fill"] == {}


class TestSuccessFill:

    def test_filled_close_reports_the_fill(self, subtests):
        with subtests.test(case="default_mode_is_live"):
            out, code = _run_script({"id": "abc", "average": 42000, "filled": 0.01},
                                    ["--symbol=BTC"])
            assert code == 0, out
            assert "error" not in out

        with subtests.test(case="full_fill_fields"):
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

        with subtests.test(case="minimal_fill_fields"):
            out, code = _run_script({"id": "x"}, ["--symbol=BTC", "--mode=live"])
            assert code == 0
            assert out["close"]["symbol"] == "BTC"
            assert out["close"]["fill"].get("oid") == "x"


class TestAlreadyFlat:

    def test_empty_response_is_success(self):
        out, code = _run_script({}, ["--symbol=BTC", "--mode=live"])
        assert code == 0, out
        assert out["close"]["symbol"] == "BTC"
        assert out["close"]["fill"] == {}
        assert "error" not in out
        assert out["close"]["already_flat"] is True


class TestFailurePaths:

    def test_failed_closes_exit_one_with_the_reason(self, subtests):
        with subtests.test(case="adapter_raises"):
            out, code = _run_script(RuntimeError("OKX auth failed"),
                                    ["--symbol=BTC", "--mode=live"])
            assert code == 1
            assert "OKX auth" in out["error"]
            assert out["close"]["fill"] == {}

        with subtests.test(case="non_dict_response"):
            out, code = _run_script("unexpected string", ["--symbol=BTC", "--mode=live"])
            assert code == 1
            assert "unexpected adapter response type" in out["error"]


if __name__ == "__main__":
    sys.exit(pytest.main([__file__, "-v"]))
