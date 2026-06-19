# backtest/tests/test_regime_calibrate.py
import os, sys
sys.path.insert(0, os.path.abspath(os.path.join(os.path.dirname(__file__), "..")))
from regime_calibrate import gate_verdict


def _report(kw_h, transition_rate):
    return {"stability": {"transition_rate": transition_rate},
            "h4": {"separation": {"kruskal_h": kw_h}}}


def test_gate_ships_when_stability_better_and_separation_kept():
    hr = _report(10.0, 0.40)
    md = _report(9.6, 0.25)  # sep within 5% tolerance, whipsaw down
    v = gate_verdict(hr, md)
    assert v["ship"] is True and v["separation_ok"] and v["stability_ok"]


def test_gate_blocks_when_separation_collapses():
    hr = _report(10.0, 0.40)
    md = _report(4.0, 0.20)  # separation lost
    assert gate_verdict(hr, md)["ship"] is False


def test_gate_blocks_when_no_stability_gain():
    hr = _report(10.0, 0.40)
    md = _report(10.0, 0.42)  # whipsaw not improved
    assert gate_verdict(hr, md)["ship"] is False
