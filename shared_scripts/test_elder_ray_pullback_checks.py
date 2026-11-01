import importlib.util
import os
import sys

import pytest

_NAME = "_strategy_check_contract_for_elder_ray_pullback"
_SPEC = importlib.util.spec_from_file_location(
    _NAME, os.path.join(os.path.dirname(os.path.abspath(__file__)), "test_strategy_check_contract.py"))
C = importlib.util.module_from_spec(_SPEC)
sys.modules[_NAME] = C
_SPEC.loader.exec_module(C)
CASE = C.CASES["elder_ray_pullback"]


@pytest.mark.parametrize("bars_count", [40, 75])
def test_warmup_holds_the_entry_and_keeps_protection(bars_count):
    bars = C.erp_bars(bars_count, C.H)
    strategy_refs = C.refs(CASE, closes=[CASE.tp])
    out = C.check(CASE, bars, closed=False, extra=C.HELD_LONG, strategy_refs=strategy_refs)
    assert out["indicators"]["erp_reason_code"] == 10.0 and out["indicators"]["erp_valid"] == 0.0
    assert out["open_action"] == "none" and out["close_fraction"] == 1.0 and out["signal"] == -1
    flat = C.check(CASE, bars, closed=False, strategy_refs=strategy_refs)
    assert flat["signal"] == 0
