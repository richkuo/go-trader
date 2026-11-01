import importlib.util
import os
import sys


_NAME = "_strategy_check_contract_for_stochastic_momentum_index"
_SPEC = importlib.util.spec_from_file_location(
    _NAME, os.path.join(os.path.dirname(os.path.abspath(__file__)), "test_strategy_check_contract.py"))
C = importlib.util.module_from_spec(_SPEC)
sys.modules[_NAME] = C
_SPEC.loader.exec_module(C)
CASE = C.CASES["stochastic_momentum_index"]
NAME = CASE.name
PARAMS = CASE.params


def test_closed_bar_entry_reproduces_from_the_emitted_full_precision_values(subtests):
    for side in [1, -1]:
        with subtests.test(side=side):
            bars = CASE.entry(side)
            boundary = bars[-1]["T"] + 1
            out = C.check(CASE, bars, closed=True, cutoff=boundary)
            assert out["closed_bar_decision"]["held"] is False
            assert out["closed_bar_decision"]["forming_rows_dropped"] == 0
            ind = out["indicators"]
            assert out["signal"] == side == C.smi_reconstruct(ind)
            assert out["indicator_labels"]["smi_reason"] == ("entry_long" if side == 1 else "entry_short")
            assert ind["smi_decision_cutoff_ms"] == float(boundary) and ind["smi_decision_boundary_ms"] == float(boundary)
            df = C.strategies().apply_strategy(NAME, C.frame_df(C.rows_of(bars[-200:]), "1h", C.NATIVE_CLOSE), PARAMS)
            for col in ("smi_value", "smi_prev_value", "smi_signal_line", "smi_prev_signal_line", "smi_diff",
                        "smi_prev_diff", "smi_valid", "smi_reason_code"):
                assert ind[col] == float(df[col].iloc[-1])
            key = "oversold" if side == 1 else "overbought"
            other = {"overbought": 99.0} if side == 1 else {"oversold": -99.0}
            at_prev = dict(PARAMS, **other, **{key: ind["smi_prev_value"]})
            at_cur = dict(PARAMS, **other, **{key: ind["smi_value"]})
            for params, want in ((at_prev, side), (at_cur, 0)):
                boundary_out = C.check(CASE, bars, closed=True, cutoff=boundary, strategy_refs=C.refs(CASE, params=params))
                assert boundary_out["signal"] == want == C.smi_reconstruct(boundary_out["indicators"])
