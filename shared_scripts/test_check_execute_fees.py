
import importlib.util
import os


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
