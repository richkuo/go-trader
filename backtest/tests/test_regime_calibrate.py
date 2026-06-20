# backtest/tests/test_regime_calibrate.py
import math
import os, sys
sys.path.insert(0, os.path.abspath(os.path.join(os.path.dirname(__file__), "..")))
from regime_calibrate import gate_verdict, SIGNIFICANCE_ALPHA, STABILITY_MIN_GAIN


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


# --- #1078 engaged-gate separation floor (bot review #1079) ------------------------------
# These pin the auto-protective behavior the volatility re-target newly unlocks: now that the
# incumbent CAN be trustworthy, a model must still independently clear the block-shuffle
# separation floor and must not ship on the stability arm alone. Invariant: incumbent
# trustworthy (significant) => ship requires (md_p <= SIGNIFICANCE_ALPHA) AND a stability gain
# of at least STABILITY_MIN_GAIN, both independently.

def test_engaged_gate_rejects_degenerate_but_perfectly_stable_model():
    # (1) Strong, significant vol incumbent (engaged, not abstaining) + a constant-label model
    # (kruskal_h~0, transition_rate=0): the stability arm is maximally green, but separation is
    # nil and not significant -> must NOT ship. This is the high-value path the PR smoke showed
    # and the floor that stops a bad model shipping now that incumbent_trustworthy can be true.
    hr = _report(90.0, 0.45, p_value=0.005, target="volatility")
    md = _report(0.0, 0.0, p_value=1.0, target="volatility")  # perfect stability, zero separation
    v = gate_verdict(hr, md)
    assert v["incumbent_trustworthy"] is True
    assert v["abstained"] is False           # engaged, NOT the weak-incumbent abstain path
    assert v["stability_ok"] is True          # the trap: stability looks perfect
    assert v["model_separation_real"] is False
    assert v["separation_ok"] is False
    assert v["ship"] is False


def test_engaged_gate_blocks_model_that_keeps_separation_but_loses_stability_gain():
    # (2) Inverse of (1): engaged gate + a model that keeps the separation (within tolerance,
    # significant) but whose stability gain is below STABILITY_MIN_GAIN -> must NOT ship. Pins
    # that the two arms are independent; a strong separator cannot ship without the whipsaw win.
    hr = _report(90.0, 0.45, p_value=0.005, target="volatility")
    md = _report(88.0, 0.44, p_value=0.005, target="volatility")  # gain 0.01 < STABILITY_MIN_GAIN
    v = gate_verdict(hr, md)
    assert v["incumbent_trustworthy"] is True
    assert v["abstained"] is False
    assert v["separation_ok"] is True
    assert v["stability_ok"] is False
    assert v["ship"] is False


def test_engaged_gate_ships_at_inclusive_floor_boundaries():
    # (3) Boundary: model exactly AT both floors — md_p == SIGNIFICANCE_ALPHA and stability gain
    # == STABILITY_MIN_GAIN — must still ship, pinning that BOTH comparisons are inclusive
    # (<= / >=). The gain must land bit-exactly on the floor or the test fails to distinguish
    # >= from >: `0.45 - (0.45 - 0.02)` is 0.020000000000000018 in IEEE-754 double (one ulp ABOVE
    # 0.02), which passes a strict `>` too, so a `>=`->`>` regression would slip through (bot
    # review #1079). `STABILITY_MIN_GAIN - 0.0` is exactly the double 0.02, so a strict-bound
    # regression on EITHER arm turns this red. incumbent_trustworthy keys off hr_p (not the
    # transition-rate magnitude), so the small hr_tr keeps the engaged (non-abstaining) path.
    hr = _report(90.0, STABILITY_MIN_GAIN, p_value=0.005, target="volatility")
    md = _report(90.0, 0.0, p_value=SIGNIFICANCE_ALPHA, target="volatility")
    assert (hr["stability"]["transition_rate"] - md["stability"]["transition_rate"]) == STABILITY_MIN_GAIN
    v = gate_verdict(hr, md)
    assert v["incumbent_trustworthy"] is True
    assert v["abstained"] is False
    assert v["model_separation_real"] is True   # md_p == alpha is inside the floor
    assert v["separation_ok"] is True
    assert v["stability_ok"] is True            # gain == STABILITY_MIN_GAIN is inside the floor
    assert v["ship"] is True


def test_engaged_gate_blocks_stability_gain_one_ulp_below_floor():
    # (4) Must-survive inverse of the boundary: a stability gain one ULP below STABILITY_MIN_GAIN
    # must block, isolating the stability arm (separation kept within tolerance + significant).
    # Constructed exactly: hr_tr = nextafter(floor, 0), md_tr = 0.0, so the gain (subtracting 0.0
    # is exact) is exactly one ulp under the floor — `>=` correctly rejects it.
    hr_tr = math.nextafter(STABILITY_MIN_GAIN, 0.0)
    hr = _report(90.0, hr_tr, p_value=0.005, target="volatility")
    md = _report(90.0, 0.0, p_value=0.005, target="volatility")
    assert (hr_tr - 0.0) < STABILITY_MIN_GAIN
    v = gate_verdict(hr, md)
    assert v["incumbent_trustworthy"] is True
    assert v["abstained"] is False
    assert v["separation_ok"] is True           # separation arm green — only stability blocks
    assert v["stability_ok"] is False
    assert v["ship"] is False


def test_engaged_gate_blocks_model_p_one_ulp_above_alpha():
    # (5) Symmetric must-survive on the significance floor: a model p one ULP above
    # SIGNIFICANCE_ALPHA is NOT real separation and must block, isolating the separation arm
    # (stability gain large + green). Pins that model_separation_real uses an inclusive `<=`.
    md_p = math.nextafter(SIGNIFICANCE_ALPHA, 1.0)
    hr = _report(90.0, 0.45, p_value=0.005, target="volatility")
    md = _report(90.0, 0.0, p_value=md_p, target="volatility")
    assert md_p > SIGNIFICANCE_ALPHA
    v = gate_verdict(hr, md)
    assert v["incumbent_trustworthy"] is True
    assert v["abstained"] is False
    assert v["stability_ok"] is True            # stability arm green — only separation blocks
    assert v["model_separation_real"] is False
    assert v["separation_ok"] is False
    assert v["ship"] is False
