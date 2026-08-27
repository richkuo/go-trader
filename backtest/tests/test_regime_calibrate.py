import math
import os, sys
sys.path.insert(0, os.path.abspath(os.path.join(os.path.dirname(__file__), '..')))
from regime_calibrate import gate_verdict, SIGNIFICANCE_ALPHA, STABILITY_MIN_GAIN

def _report(kw_h, transition_rate, p_value=0.005, target=None):
    r = {'stability': {'transition_rate': transition_rate}, 'h4': {'separation': {'kruskal_h': kw_h}, 'significance': {'p_value': p_value}}}
    if target is not None:
        r['target'] = target
    return r

def test_gate_ships_when_stability_better_and_separation_kept():
    hr = _report(10.0, 0.4)
    md = _report(9.6, 0.25)
    v = gate_verdict(hr, md)
    assert v['ship'] is True and v['separation_ok'] and v['stability_ok']

def test_gate_blocks_when_separation_collapses():
    hr = _report(10.0, 0.4)
    md = _report(4.0, 0.2)
    assert gate_verdict(hr, md)['ship'] is False

def test_gate_blocks_when_no_stability_gain():
    hr = _report(10.0, 0.4)
    md = _report(10.0, 0.42)
    assert gate_verdict(hr, md)['ship'] is False

def test_gate_blocks_useless_model_when_incumbent_also_useless():
    hr = _report(0.1, 0.4, p_value=0.9)
    md = _report(0.1, 0.05, p_value=0.95)
    v = gate_verdict(hr, md)
    assert v['ship'] is False
    assert v['model_separation_real'] is False
    assert v['incumbent_trustworthy'] is False

def test_gate_blocks_degenerate_constant_label_model():
    hr = _report(2.0, 0.4, p_value=0.2)
    md = _report(0.0, 0.0, p_value=1.0)
    v = gate_verdict(hr, md)
    assert v['ship'] is False
    assert v['stability_ok'] is True
    assert v['model_separation_real'] is False

def test_gate_ships_strong_incumbent_with_model_within_tolerance():
    hr = _report(13.0, 0.4, p_value=0.005)
    md = _report(12.4, 0.25, p_value=0.005)
    v = gate_verdict(hr, md)
    assert v['ship'] is True
    assert v['incumbent_trustworthy'] is True

def test_gate_v2_ships_self_significant_noninferior_model_over_insignificant_incumbent():
    hr = _report(0.5, 0.4, p_value=0.3)
    md = _report(5.0, 0.2, p_value=0.01)
    v = gate_verdict(hr, md)
    assert v['model_separation_real'] is True
    assert v['incumbent_trustworthy'] is False
    assert v['separation_ok'] is True and v['stability_ok'] is True
    assert v['ship'] is True
    assert v['gate_semantics'] == 'candidate-self-v2 (#1211)'

def test_gate_v2_blocks_insignificant_candidate_even_if_incumbent_insignificant():
    hr = _report(0.5, 0.4, p_value=0.3)
    md = _report(5.0, 0.2, p_value=0.06)
    v = gate_verdict(hr, md)
    assert v['model_separation_real'] is False
    assert v['separation_ok'] is False
    assert v['ship'] is False

def test_gate_v2_blocks_self_significant_but_inferior_candidate():
    hr = _report(100.0, 0.4, p_value=0.3)
    md = _report(50.0, 0.2, p_value=0.01)
    v = gate_verdict(hr, md)
    assert v['model_separation_real'] is True
    assert v['separation_ok'] is False
    assert v['ship'] is False

def test_gate_v2_blocks_self_significant_noninferior_but_no_stability_gain():
    hr = _report(5.0, 0.4, p_value=0.3)
    md = _report(5.0, 0.39, p_value=0.01)
    v = gate_verdict(hr, md)
    assert v['separation_ok'] is True
    assert v['stability_ok'] is False
    assert v['ship'] is False

def test_gate_surfaces_target_from_reports():
    hr = _report(13.0, 0.4, p_value=0.005, target='volatility')
    md = _report(12.4, 0.25, p_value=0.005, target='volatility')
    assert gate_verdict(hr, md)['target'] == 'volatility'

def test_gate_ships_on_trustworthy_volatility_incumbent():
    hr = _report(90.0, 0.45, p_value=0.005, target='volatility')
    md = _report(88.0, 0.3, p_value=0.005, target='volatility')
    v = gate_verdict(hr, md)
    assert v['incumbent_trustworthy'] is True
    assert v['separation_ok'] and v['stability_ok']
    assert v['ship'] is True

def test_engaged_gate_rejects_degenerate_but_perfectly_stable_model():
    hr = _report(90.0, 0.45, p_value=0.005, target='volatility')
    md = _report(0.0, 0.0, p_value=1.0, target='volatility')
    v = gate_verdict(hr, md)
    assert v['incumbent_trustworthy'] is True
    assert v['stability_ok'] is True
    assert v['model_separation_real'] is False
    assert v['separation_ok'] is False
    assert v['ship'] is False

def test_engaged_gate_blocks_model_that_keeps_separation_but_loses_stability_gain():
    hr = _report(90.0, 0.45, p_value=0.005, target='volatility')
    md = _report(88.0, 0.44, p_value=0.005, target='volatility')
    v = gate_verdict(hr, md)
    assert v['incumbent_trustworthy'] is True
    assert v['separation_ok'] is True
    assert v['stability_ok'] is False
    assert v['ship'] is False

def test_engaged_gate_ships_at_inclusive_floor_boundaries():
    hr = _report(90.0, STABILITY_MIN_GAIN, p_value=0.005, target='volatility')
    md = _report(90.0, 0.0, p_value=SIGNIFICANCE_ALPHA, target='volatility')
    assert hr['stability']['transition_rate'] - md['stability']['transition_rate'] == STABILITY_MIN_GAIN
    v = gate_verdict(hr, md)
    assert v['incumbent_trustworthy'] is True
    assert v['model_separation_real'] is True
    assert v['separation_ok'] is True
    assert v['stability_ok'] is True
    assert v['ship'] is True

def test_engaged_gate_blocks_stability_gain_one_ulp_below_floor():
    hr_tr = math.nextafter(STABILITY_MIN_GAIN, 0.0)
    hr = _report(90.0, hr_tr, p_value=0.005, target='volatility')
    md = _report(90.0, 0.0, p_value=0.005, target='volatility')
    assert hr_tr - 0.0 < STABILITY_MIN_GAIN
    v = gate_verdict(hr, md)
    assert v['incumbent_trustworthy'] is True
    assert v['separation_ok'] is True
    assert v['stability_ok'] is False
    assert v['ship'] is False

def test_engaged_gate_blocks_model_p_one_ulp_above_alpha():
    md_p = math.nextafter(SIGNIFICANCE_ALPHA, 1.0)
    hr = _report(90.0, 0.45, p_value=0.005, target='volatility')
    md = _report(90.0, 0.0, p_value=md_p, target='volatility')
    assert md_p > SIGNIFICANCE_ALPHA
    v = gate_verdict(hr, md)
    assert v['incumbent_trustworthy'] is True
    assert v['stability_ok'] is True
    assert v['model_separation_real'] is False
    assert v['separation_ok'] is False
    assert v['ship'] is False
