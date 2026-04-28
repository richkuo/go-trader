"""Tests for live execute fee extraction in platform check scripts."""

import importlib.util
import json
import os
import sys
import types


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


def test_robinhood_ignores_execution_notional_without_fee_keys():
    mod = _load_script("check_robinhood.py")
    response = {
        "average_price": "50000",
        "cumulative_quantity": "0.015",
        "executions": [{"total": "750.00", "value": "0.015", "amount": "0.015"}],
    }
    assert mod._extract_fee(response) is None


def test_topstep_extracts_nested_fee_total():
    mod = _load_script("check_topstep.py")
    assert abs(mod._extract_fee({"totalFees": [{"amount": "1.50"}, {"amount": "2.62"}]}) - 4.12) < 1e-12


def test_topstep_ignores_generic_nested_totals_without_fee_context():
    mod = _load_script("check_topstep.py")
    assert mod._extract_fee_value([{"total": "750.00", "value": "0.015"}]) is None


def test_okx_execute_emits_exchange_order_id(monkeypatch, capsys):
    mod = _load_script("check_okx.py")
    adapter_mod = types.ModuleType("adapter")

    class OKXExchangeAdapter:
        def market_open(self, symbol, is_buy, size, inst_type="spot"):
            return {
                "id": "okx-order-123",
                "average": "50000",
                "filled": str(size),
                "fee": {"cost": "0.25"},
            }

    adapter_mod.OKXExchangeAdapter = OKXExchangeAdapter
    monkeypatch.setitem(sys.modules, "adapter", adapter_mod)

    mod.run_execute("BTC", "buy", 0.01, "live", "swap")
    payload = json.loads(capsys.readouterr().out)

    assert payload["execution"]["fill"]["oid"] == "okx-order-123"


def test_robinhood_execute_emits_exchange_order_id(monkeypatch, capsys):
    mod = _load_script("check_robinhood.py")
    adapter_mod = types.ModuleType("adapter")

    class RobinhoodExchangeAdapter:
        def __init__(self, mode="paper"):
            self.mode = mode

        def market_buy(self, symbol, amount_usd):
            return {
                "id": "rh-order-456",
                "average_price": "50000",
                "cumulative_quantity": "0.01",
                "executions": [{"fees": [{"amount": "0.03"}]}],
            }

    adapter_mod.RobinhoodExchangeAdapter = RobinhoodExchangeAdapter
    monkeypatch.setitem(sys.modules, "adapter", adapter_mod)

    mod.run_execute("BTC", "buy", 500, 0, "live")
    payload = json.loads(capsys.readouterr().out)

    assert payload["execution"]["fill"]["oid"] == "rh-order-456"


def test_topstep_execute_emits_exchange_order_id(monkeypatch, capsys):
    mod = _load_script("check_topstep.py")
    adapter_mod = types.ModuleType("adapter")

    class TopStepExchangeAdapter:
        def __init__(self, mode="paper"):
            self.mode = mode

        def market_open(self, symbol, is_buy, contracts):
            return {
                "orderId": "ts-order-789",
                "avgPrice": "5000.25",
                "filledQuantity": contracts,
                "totalFees": [{"amount": "4.12"}],
            }

    adapter_mod.TopStepExchangeAdapter = TopStepExchangeAdapter
    monkeypatch.setitem(sys.modules, "adapter", adapter_mod)

    mod.run_execute("ES", "buy", 2, "live")
    payload = json.loads(capsys.readouterr().out)

    assert payload["execution"]["fill"]["oid"] == "ts-order-789"
