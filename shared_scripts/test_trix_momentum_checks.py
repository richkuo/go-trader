import importlib.util
import os
import sys

import pytest

_NAME = "_strategy_check_contract_for_trix_momentum"
_SPEC = importlib.util.spec_from_file_location(
    _NAME, os.path.join(os.path.dirname(os.path.abspath(__file__)), "test_strategy_check_contract.py"))
C = importlib.util.module_from_spec(_SPEC)
sys.modules[_NAME] = C
_SPEC.loader.exec_module(C)
CASE = C.CASES["trix_momentum"]
PARAMS = CASE.params
H = C.H


@pytest.mark.parametrize("rows,closed_reason", [(41, "insufficient_history"), (42, "entry_long")])
def test_selected_closed_history_needs_l_plus_two_bars(rows, closed_reason):
    params = {**PARAMS, "recursion_length": 40}
    bars = C.flat_bars(rows + 1, H, forming_jump=None)
    bars[-2] = dict(bars[-2], h=409.0, c=408.0)
    out = C.check(CASE, bars, closed=True, strategy_refs=C.refs(CASE, params=params))
    assert out["closed_bar_decision"]["forming_rows_dropped"] == 1
    assert out["indicator_labels"]["trix_reason"] == closed_reason
    assert out["signal"] == (1 if closed_reason == "entry_long" else 0)


@pytest.mark.parametrize("limit,closed_reason", [(200, "entry_long"), (198, "insufficient_history")])
def test_the_ordinary_200_row_window_carries_the_largest_dependency(limit, closed_reason):
    params = {"period": 32, "signal_period": 9, "recursion_length": 197}
    bars = C.flat_bars(260, H, forming_jump=None)
    bars[-2] = dict(bars[-2], h=409.0, c=408.0)
    out = C.check(CASE, bars, closed=True, limit=limit, strategy_refs=C.refs(CASE, params=params))
    assert out["closed_bar_decision"]["forming_rows_dropped"] == 1
    assert out["closed_bar_decision"]["rows"] == limit
    assert out["indicator_labels"]["trix_reason"] == closed_reason
    assert out["indicators"]["trix_required_history"] == 199.0
    assert out["signal"] == (1 if closed_reason == "entry_long" else 0)
