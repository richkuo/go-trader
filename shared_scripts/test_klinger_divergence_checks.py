import importlib.util
import os
import sys


_NAME = "_strategy_check_contract_for_klinger_divergence"
_SPEC = importlib.util.spec_from_file_location(
    _NAME, os.path.join(os.path.dirname(os.path.abspath(__file__)), "test_strategy_check_contract.py"))
C = importlib.util.module_from_spec(_SPEC)
sys.modules[_NAME] = C
_SPEC.loader.exec_module(C)
CASE = C.CASES["klinger_divergence"]
NAME = CASE.name
PARAMS = CASE.params
H = C.H


def test_closed_bar_entry_reproduces_from_the_emitted_values(subtests):
    for side in [1, -1]:
        with subtests.test(side=side):
            bars = CASE.entry(side)
            boundary = bars[-1]["T"] + 1
            out = C.check(CASE, bars, closed=True, cutoff=boundary)
            assert out["closed_bar_decision"]["held"] is False
            assert out["closed_bar_decision"]["forming_rows_dropped"] == 0
            ind = out["indicators"]
            assert out["signal"] == side == C.klinger_reconstruct(ind)
            assert out["indicator_labels"]["klinger_reason"] == ("entry_long" if side == 1 else "entry_short")
            assert ind["klinger_decision_cutoff_ms"] == float(boundary) and ind["klinger_decision_boundary_ms"] == float(boundary)
            df = C.strategies().apply_strategy(NAME, C.frame_df(C.rows_of(bars[-200:]), "1h", C.NATIVE_CLOSE), PARAMS)
            for col in ("klinger_oscillator", "klinger_signal_line", "klinger_prev_oscillator", "klinger_prev_signal_line",
                        "klinger_cm", "klinger_volume_force", "klinger_valid", "klinger_reason_code"):
                assert ind[col] == float(df[col].iloc[-1])
            side_key = "long" if side == 1 else "short"
            lag = int((ind["klinger_eval_ts_ms"] - ind[f"klinger_{side_key}_confirm_ts_ms"]) / H)
            for expiry, want in ((lag, side), (lag - 1, 0)):
                if expiry < 1:
                    continue
                params = dict(PARAMS, setup_expiry=expiry)
                boundary_out = C.check(CASE, bars, closed=True, cutoff=boundary, strategy_refs=C.refs(CASE, params=params))
                assert boundary_out["signal"] == want == C.klinger_reconstruct(boundary_out["indicators"])
