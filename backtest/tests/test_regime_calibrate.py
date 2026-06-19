# backtest/tests/test_regime_calibrate.py
import os, sys
sys.path.insert(0, os.path.abspath(os.path.join(os.path.dirname(__file__), "..")))
from regime_calibrate import gate_verdict


def _report(kw_h, transition_rate, p_value=0.005, target=None):
    # p_value defaults to "significant" so existing separation/stability cases are
    # exercised in isolation; cases that probe the significance floor set it explicitly.
    r = {"stability": {"transition_rate": transition_rate},
         "h4": {"separation": {"kruskal_h": kw_h},
                "significance": {"p_value": p_value}}}
    if target is not None:
        r["target"] = target
    return r


def test_gate_ships_when_stability_better_and_separation_kept():
    hr = _report(10.0, 0.40)
    md = _report(9.6, 0.25)  # sep within 5% tolerance, whipsaw down, both significant
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


# --- absolute separation floor (bot review #1071) -------------------------------------

def test_gate_blocks_useless_model_when_incumbent_also_useless():
    # (a) hr_h ~ 0 and md_h ~ 0: relative tolerance passes (0.1 >= 0.1*0.95) but the model
    # separates nothing (p high) -> must NOT ship, and must abstain on the weak incumbent.
    hr = _report(0.1, 0.40, p_value=0.90)
    md = _report(0.1, 0.05, p_value=0.95)
    v = gate_verdict(hr, md)
    assert v["ship"] is False
    assert v["model_separation_real"] is False
    assert v["abstained"] is True


def test_gate_blocks_degenerate_constant_label_model():
    # (b) degenerate constant-label model (transition_rate=0, kruskal_h=0) vs a weak
    # incumbent: stability arm is maximally green, but separation is nil -> must NOT ship.
    hr = _report(2.0, 0.40, p_value=0.20)   # incumbent not significant
    md = _report(0.0, 0.0, p_value=1.0)     # constant label: no separation, no flips
    v = gate_verdict(hr, md)
    assert v["ship"] is False
    assert v["stability_ok"] is True        # the trap: stability looks perfect
    assert v["model_separation_real"] is False
    assert v["abstained"] is True


def test_gate_ships_strong_incumbent_with_model_within_tolerance():
    # (c) no regression: strong, significant incumbent and a model within tolerance and
    # itself significant must still ship.
    hr = _report(13.0, 0.40, p_value=0.005)
    md = _report(12.4, 0.25, p_value=0.005)
    v = gate_verdict(hr, md)
    assert v["ship"] is True
    assert v["abstained"] is False


def test_gate_abstains_when_incumbent_not_significant():
    # A strong, significant model cannot ship off a window where the incumbent baseline
    # shows no significant separation — there is nothing trustworthy to validate against.
    hr = _report(0.5, 0.40, p_value=0.30)   # incumbent separation not significant
    md = _report(5.0, 0.20, p_value=0.01)   # model strong and significant
    v = gate_verdict(hr, md)
    assert v["model_separation_real"] is True
    assert v["incumbent_trustworthy"] is False
    assert v["abstained"] is True
    assert v["ship"] is False


# --- #1078: gate re-founded on the forward-VOLATILITY target -----------------------------

def test_gate_surfaces_target_from_reports():
    # The verdict must echo the forward target the reports were scored on, so a volatility
    # result can never be misread as a directional (return) one.
    hr = _report(13.0, 0.40, p_value=0.005, target="volatility")
    md = _report(12.4, 0.25, p_value=0.005, target="volatility")
    assert gate_verdict(hr, md)["target"] == "volatility"


def test_gate_ships_on_trustworthy_volatility_incumbent():
    # The crux of #1078: on the forward-VOLATILITY axis the hand-rule IS significant
    # (PR #1077: 4/5 windows p <= 0.01), so a stability-improved model that keeps the
    # separation now ships honestly — exactly the path that was impossible on the return
    # target, where the incumbent was never significant (#1073) and the gate always abstained.
    hr = _report(90.0, 0.45, p_value=0.005, target="volatility")  # strong, real vol incumbent
    md = _report(88.0, 0.30, p_value=0.005, target="volatility")  # within tol, whipsaw down
    v = gate_verdict(hr, md)
    assert v["incumbent_trustworthy"] is True
    assert v["abstained"] is False
    assert v["separation_ok"] and v["stability_ok"]
    assert v["ship"] is True
