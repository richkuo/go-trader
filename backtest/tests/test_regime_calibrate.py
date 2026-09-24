import math
import os, sys

import pytest

sys.path.insert(0, os.path.abspath(os.path.join(os.path.dirname(__file__), "..")))
from regime_calibrate import gate_verdict, SIGNIFICANCE_ALPHA, STABILITY_MIN_GAIN


def _report(kw_h, transition_rate, p_value=0.005, target=None):
    r = {"stability": {"transition_rate": transition_rate},
         "h4": {"separation": {"kruskal_h": kw_h},
                "significance": {"p_value": p_value}}}
    if target is not None:
        r["target"] = target
    return r


@pytest.mark.parametrize("hr,md,expected", [
    pytest.param(
        (10.0, 0.40), (9.6, 0.25),
        {"ship": True, "separation_ok": True, "stability_ok": True},
        id="ships_when_stability_better_and_separation_kept"),
    pytest.param(
        (10.0, 0.40), (4.0, 0.20), {"ship": False},
        id="blocks_when_separation_collapses"),
    pytest.param(
        (10.0, 0.40), (10.0, 0.42), {"ship": False},
        id="blocks_when_no_stability_gain"),
    pytest.param(
        (0.1, 0.40, 0.90), (0.1, 0.05, 0.95),
        {"ship": False, "model_separation_real": False,
         "incumbent_trustworthy": False},
        id="blocks_useless_model_when_incumbent_also_useless"),
    pytest.param(
        (2.0, 0.40, 0.20), (0.0, 0.0, 1.0),
        {"ship": False, "stability_ok": True, "model_separation_real": False},
        id="blocks_degenerate_constant_label_model"),
    pytest.param(
        (13.0, 0.40, 0.005), (12.4, 0.25, 0.005),
        {"ship": True, "incumbent_trustworthy": True},
        id="ships_strong_incumbent_with_model_within_tolerance"),
    pytest.param(
        (0.5, 0.40, 0.30), (5.0, 0.20, 0.01),
        {"model_separation_real": True, "incumbent_trustworthy": False,
         "separation_ok": True, "stability_ok": True, "ship": True,
         "gate_semantics": "candidate-self-v2 (#1211)"},
        id="v2_ships_self_significant_noninferior_over_insignificant_incumbent"),
    pytest.param(
        (0.5, 0.40, 0.30), (5.0, 0.20, 0.06),
        {"model_separation_real": False, "separation_ok": False, "ship": False},
        id="v2_blocks_insignificant_candidate"),
    pytest.param(
        (100.0, 0.40, 0.30), (50.0, 0.20, 0.01),
        {"model_separation_real": True, "separation_ok": False, "ship": False},
        id="v2_blocks_self_significant_but_inferior_candidate"),
    pytest.param(
        (5.0, 0.40, 0.30), (5.0, 0.39, 0.01),
        {"separation_ok": True, "stability_ok": False, "ship": False},
        id="v2_blocks_noninferior_but_no_stability_gain"),
    pytest.param(
        (13.0, 0.40, 0.005, "volatility"), (12.4, 0.25, 0.005, "volatility"),
        {"target": "volatility"},
        id="surfaces_target_from_reports"),
    pytest.param(
        (90.0, 0.45, 0.005, "volatility"), (88.0, 0.30, 0.005, "volatility"),
        {"incumbent_trustworthy": True, "separation_ok": True,
         "stability_ok": True, "ship": True},
        id="ships_on_trustworthy_volatility_incumbent"),
    pytest.param(
        (90.0, 0.45, 0.005, "volatility"), (0.0, 0.0, 1.0, "volatility"),
        {"incumbent_trustworthy": True, "stability_ok": True,
         "model_separation_real": False, "separation_ok": False, "ship": False},
        id="engaged_rejects_degenerate_but_perfectly_stable_model"),
    pytest.param(
        (90.0, 0.45, 0.005, "volatility"), (88.0, 0.44, 0.005, "volatility"),
        {"incumbent_trustworthy": True, "separation_ok": True,
         "stability_ok": False, "ship": False},
        id="engaged_blocks_kept_separation_lost_stability_gain"),
    pytest.param(
        (90.0, STABILITY_MIN_GAIN, 0.005, "volatility"),
        (90.0, 0.0, SIGNIFICANCE_ALPHA, "volatility"),
        {"incumbent_trustworthy": True, "model_separation_real": True,
         "separation_ok": True, "stability_ok": True, "ship": True},
        id="engaged_ships_at_inclusive_floor_boundaries"),
    pytest.param(
        (90.0, math.nextafter(STABILITY_MIN_GAIN, 0.0), 0.005, "volatility"),
        (90.0, 0.0, 0.005, "volatility"),
        {"incumbent_trustworthy": True, "separation_ok": True,
         "stability_ok": False, "ship": False},
        id="engaged_blocks_stability_gain_one_ulp_below_floor"),
    pytest.param(
        (90.0, 0.45, 0.005, "volatility"),
        (90.0, 0.0, math.nextafter(SIGNIFICANCE_ALPHA, 1.0), "volatility"),
        {"incumbent_trustworthy": True, "stability_ok": True,
         "model_separation_real": False, "separation_ok": False, "ship": False},
        id="engaged_blocks_model_p_one_ulp_above_alpha"),
])
def test_gate_verdict(hr, md, expected):
    v = gate_verdict(_report(*hr), _report(*md))
    for key, want in expected.items():
        assert v[key] == want, key
