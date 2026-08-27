import importlib.util
import os
_DRIVER_PATH = os.path.join(os.path.dirname(__file__), '..', 'candidates', 'breakout_1165', 'live_label_fidelity.py')
_spec = importlib.util.spec_from_file_location('breakout_1165_live_label_fidelity', _DRIVER_PATH)
llf = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(llf)

def _drift(n, agreement, transitions=None):
    return {'n': n, 'agreement': agreement, 'disagreements': sum((transitions or {}).values()), 'transitions': dict(transitions or {})}

def test_gate_membership_flips_counts_both_directions():
    drift = _drift(100, 0.97, {'trending_up_clean->trending_up_choppy': 2, 'trending_up_choppy->trending_up_clean': 1, 'ranging_quiet->ranging_volatile': 4})
    assert llf.gate_membership_flips(drift) == 3

def test_gate_membership_flips_ignores_non_gate_disagreements():
    drift = _drift(50, 0.9, {'trending_down_clean->ranging_quiet': 5})
    assert llf.gate_membership_flips(drift) == 0

def test_row_verdict_fails_closed_below_min_bars():
    v = llf.row_verdict(_drift(3, 1.0), agreement_threshold=0.95, min_agreement_bars=30)
    assert not v['passes']
    assert any(('insufficient comparable bars' in r for r in v['blocking_reasons']))

def test_row_verdict_blocks_below_agreement_threshold():
    v = llf.row_verdict(_drift(100, 0.9, {'trending_up_clean->ranging_quiet': 10}), agreement_threshold=0.95, min_agreement_bars=30)
    assert not v['passes']
    assert any(('label agreement' in r for r in v['blocking_reasons']))

def test_row_verdict_passes_and_derives_gate_agreement():
    v = llf.row_verdict(_drift(100, 0.98, {'trending_up_clean->trending_up_choppy': 1, 'ranging_quiet->ranging_volatile': 1}), agreement_threshold=0.95, min_agreement_bars=30)
    assert v['passes']
    assert v['gate_membership_flips'] == 1
    assert v['gate_membership_agreement'] == 0.99
    assert v['gate_membership_agreement'] >= v['agreement']

def test_overall_verdict_blocks_on_missing_rows():
    row = {'symbol': 'BTC/USDT', 'timeframe': '1h', 'window': 'oos', 'verdict': {'passes': True, 'blocking_reasons': []}}
    v = llf.overall_verdict([row], expected_rows=12)
    assert not v['promote']
    assert any(('1/12' in r for r in v['blocking_reasons']))

def test_overall_verdict_blocks_when_any_row_blocks():
    ok = {'symbol': 'BTC/USDT', 'timeframe': '1h', 'window': 'is', 'verdict': {'passes': True, 'blocking_reasons': []}}
    bad = {'symbol': 'SOL/USDT', 'timeframe': '4h', 'window': 'oos', 'verdict': {'passes': False, 'blocking_reasons': ['label agreement 0.9000 < threshold']}}
    v = llf.overall_verdict([ok, bad], expected_rows=2)
    assert not v['promote']
    assert any(('SOL/USDT 4h oos' in r for r in v['blocking_reasons']))

def test_overall_verdict_promotes_when_all_rows_pass():
    rows = [{'symbol': s, 'timeframe': tf, 'window': w, 'verdict': {'passes': True, 'blocking_reasons': []}} for s in ('BTC/USDT',) for tf in ('1h', '4h') for w in ('is', 'oos')]
    v = llf.overall_verdict(rows, expected_rows=4)
    assert v['promote']
    assert v['blocking_reasons'] == []
