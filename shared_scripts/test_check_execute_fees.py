"""Tests for live execute fee extraction in platform check scripts."""

import importlib.util
import os


def _load_script(filename):
    script_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), filename)
    module_name = filename.replace(".py", "").replace("-", "_") + "_fee_test"
    spec = importlib.util.spec_from_file_location(module_name, script_path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def test_okx_extracts_ccxt_fee_cost():
    mod = _load_script("check_okx.py")
    assert mod._extract_fee({"fee": {"cost": "0.25", "currency": "USDT"}}) == 0.25


def test_robinhood_extracts_nested_execution_fee():
    mod = _load_script("check_robinhood.py")
    response = {
        "average_price": "50000",
        "cumulative_quantity": "0.01",
        "executions": [{"fees": [{"amount": "0.03"}, {"amount": "0.02"}]}],
    }
    assert abs(mod._extract_fee(response) - 0.05) < 1e-12


def test_topstep_extracts_nested_fee_total():
    mod = _load_script("check_topstep.py")
    assert abs(mod._extract_fee({"totalFees": [{"amount": "1.50"}, {"amount": "2.62"}]}) - 4.12) < 1e-12
