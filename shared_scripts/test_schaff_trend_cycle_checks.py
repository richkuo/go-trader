import importlib.util
import os
import sys

import pytest

_NAME = "_strategy_check_contract_for_schaff_trend_cycle"
_SPEC = importlib.util.spec_from_file_location(
    _NAME, os.path.join(os.path.dirname(os.path.abspath(__file__)), "test_strategy_check_contract.py"))
C = importlib.util.module_from_spec(_SPEC)
sys.modules[_NAME] = C
_SPEC.loader.exec_module(C)
CASE = C.CASES["schaff_trend_cycle"]
NAME = CASE.name
PARAMS = CASE.params


@pytest.mark.parametrize("side", [1, -1])
def test_closed_bar_entry_reproduces_from_the_emitted_values(side):
    bars = CASE.entry(side)
    boundary = bars[-1]["T"] + 1
    out = C.check(CASE, bars, closed=True, cutoff=boundary)
    assert out["closed_bar_decision"]["held"] is False
    assert out["closed_bar_decision"]["forming_rows_dropped"] == 0
    ind = out["indicators"]
    assert out["signal"] == side == C.stc_reconstruct(ind)
    assert out["indicator_labels"]["stc_reason"] == ("entry_long" if side == 1 else "entry_short")
    assert ind["stc_decision_cutoff_ms"] == float(boundary) and ind["stc_decision_boundary_ms"] == float(boundary)
    df = C.strategies().apply_strategy(NAME, C.frame_df(C.rows_of(bars[-200:]), "1h", C.NATIVE_CLOSE), PARAMS)
    for col in ("stc_value", "stc_prev_value", "stc_macd", "stc_prev_macd", "stc_valid", "stc_reason_code"):
        assert ind[col] == float(df[col].iloc[-1])
    key = "lower_threshold" if side == 1 else "upper_threshold"
    at_prev = dict(PARAMS, **{key: ind["stc_prev_value"]})
    at_cur = dict(PARAMS, **{key: ind["stc_value"]})
    for params, want in ((at_prev, side), (at_cur, 0)):
        boundary_out = C.check(CASE, bars, closed=True, cutoff=boundary, strategy_refs=C.refs(CASE, params=params))
        assert boundary_out["signal"] == want == C.stc_reconstruct(boundary_out["indicators"])
