
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


def _load_robinhood_adapter():
    adapter_path = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "platforms", "robinhood", "adapter.py")
    spec = importlib.util.spec_from_file_location("robinhood_adapter_fee_test", adapter_path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def test_robinhood_extracts_nested_execution_fee_and_ignores_notional_without_fee_keys():
    mod = _load_robinhood_adapter()
    response = {
        "average_price": "50000",
        "cumulative_quantity": "0.01",
        "executions": [{"fees": [{"amount": "0.03"}, {"amount": "0.02"}]}],
    }
    assert abs(mod.extract_order_fee(response) - 0.05) < 1e-12, "test_robinhood_extracts_nested_execution_fee"
    response = {
        "average_price": "50000",
        "cumulative_quantity": "0.015",
        "executions": [{"total": "750.00", "value": "0.015", "amount": "0.015"}],
    }
    assert mod.extract_order_fee(response) is None, "test_robinhood_ignores_execution_notional_without_fee_keys"


def test_robinhood_execute_emits_exchange_order_id(monkeypatch, capsys):
    mod = _load_script("check_robinhood.py")
    adapter_mod = types.ModuleType("adapter")

    class RobinhoodExchangeAdapter:
        def __init__(self, mode="paper", login_deadline=None):
            self.mode = mode
            self.is_live = True

        def place_market_order(self, symbol, side, *, ref_id, deadline, quantity=None, amount_usd=None, expected_account_id=None):
            return {
                "outcome": "filled",
                "order": {"id": "rh-order-456", "state": "filled"},
                "ref_id": ref_id,
                "fill": {"avg_px": 50000.0, "quantity": 0.01, "oid": "rh-order-456", "fee": 0.03},
            }

    adapter_mod.RobinhoodExchangeAdapter = RobinhoodExchangeAdapter
    monkeypatch.setitem(sys.modules, "adapter", adapter_mod)

    mod.run_execute("BTC", "buy", 500, 0, "live", "11111111-1111-4111-8111-111111111111", 5000)
    payload = json.loads(capsys.readouterr().out)

    assert payload["execution"]["fill"]["oid"] == "rh-order-456"
    assert payload["order_outcome"] == "filled"
