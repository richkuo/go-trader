import importlib.util
import os
import sys

import pytest

_NAME = "_strategy_check_contract_for_true_strength_index"
_SPEC = importlib.util.spec_from_file_location(
    _NAME, os.path.join(os.path.dirname(os.path.abspath(__file__)), "test_strategy_check_contract.py"))
C = importlib.util.module_from_spec(_SPEC)
sys.modules[_NAME] = C
_SPEC.loader.exec_module(C)
CASE = C.CASES["true_strength_index"]
PARAMS = CASE.params
BOUNDARY = {"slow_period": 33, "fast_period": 13, "signal_period": 9, "recursion_length": 198}
H = C.H


@pytest.mark.parametrize("rows,closed_reason", [(40, "insufficient_history"), (41, "entry_long")])
def test_selected_closed_history_needs_w_plus_one_bars(rows, closed_reason):
    params = {**PARAMS, "recursion_length": 40}
    bars = C.tsi_bars(rows + 1, H, C.TSI_LONG_SETUP + [406.5, 410.0])
    out = C.check(CASE, bars, closed=True, strategy_refs=C.refs(CASE, params=params))
    assert out["closed_bar_decision"]["forming_rows_dropped"] == 1
    assert out["indicator_labels"]["tsi_reason"] == closed_reason
    assert out["signal"] == (1 if closed_reason == "entry_long" else 0)


@pytest.mark.parametrize("limit,closed_reason", [(200, "entry_long"), (199, "entry_long"),
                                                 (198, "insufficient_history")])
def test_the_ordinary_200_row_window_carries_the_largest_support(limit, closed_reason):
    bars = C.tsi_bars(260, H, C.TSI_LONG_SETUP + [406.5, 410.0])
    out = C.check(CASE, bars, closed=True, limit=limit, strategy_refs=C.refs(CASE, params=BOUNDARY))
    assert out["closed_bar_decision"]["forming_rows_dropped"] == 1
    assert out["closed_bar_decision"]["rows"] == limit
    assert out["indicator_labels"]["tsi_reason"] == closed_reason
    assert out["indicators"]["tsi_required_history"] == 199.0
    assert out["signal"] == (1 if closed_reason == "entry_long" else 0)


def test_received_order_defects_survive_normalization_and_hold_the_entry():
    bars = C.tsi_long()
    rows = C.rows_of(bars)
    rows[-6], rows[-7] = rows[-7], rows[-6]
    strategy_refs = C.refs(CASE, closes=[CASE.tp])
    out = C.check(CASE, bars, closed=False, extra=C.HELD_LONG, timing=False, rows=rows, strategy_refs=strategy_refs)
    assert out["indicator_labels"]["tsi_reason"] == "timestamp_order"
    assert out["indicators"]["tsi_valid"] == 0.0
    assert out["open_action"] == "none" and out["close_fraction"] == 1.0 and out["signal"] == -1
    flat = C.check(CASE, bars, closed=False, timing=False, rows=rows, strategy_refs=strategy_refs)
    assert flat["signal"] == 0 and flat["indicator_labels"]["tsi_reason"] == "timestamp_order"
    clean = C.check(CASE, bars, closed=False, timing=False, strategy_refs=strategy_refs)
    assert clean["signal"] == 1
