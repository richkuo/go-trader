import json
from datetime import datetime, timedelta, timezone
from directional_certification import normalize_cert_asset, load_certifications, is_directional_certified, certified_states, backtest_classifier, config_directional_classifier

def test_normalize_cert_asset():
    assert normalize_cert_asset('BTC/USDT') == 'BTC'
    assert normalize_cert_asset('btc') == 'BTC'
    assert normalize_cert_asset('BTC-PERP') == 'BTC'
    assert normalize_cert_asset('ETH/USD') == 'ETH'
    assert normalize_cert_asset('SOL_USDT') == 'SOL'
    assert normalize_cert_asset('  xrp ') == 'XRP'
    assert normalize_cert_asset('') == ''

def test_load_missing_is_failclosed_empty(tmp_path):
    assert load_certifications(str(tmp_path / 'nope.json')) == {}

def test_load_malformed_is_failclosed_empty(tmp_path):
    p = tmp_path / 'cert.json'
    p.write_text('{not json')
    assert load_certifications(str(p)) == {}
    p.write_text(json.dumps({'schema_version': 2, 'certified': []}))
    assert load_certifications(str(p)) == {}

def test_is_directional_certified_active_expired_never(tmp_path):
    now = datetime(2026, 6, 19, tzinfo=timezone.utc)
    future = (now + timedelta(days=2)).isoformat().replace('+00:00', 'Z')
    past = (now - timedelta(days=2)).isoformat().replace('+00:00', 'Z')
    p = tmp_path / 'cert.json'
    p.write_text(json.dumps({'schema_version': 1, 'certified': [{'asset': 'BTC/USDT', 'timeframe': '1h', 'classifier': 'composite', 'expires_at': future, 'states': {'trending_up': 'long'}}, {'asset': 'ETH', 'timeframe': '4h', 'classifier': 'adx', 'expires_at': past, 'states': {'trending_down': 'short'}}]}))
    certs = load_certifications(str(p))
    assert is_directional_certified(certs, 'BTC', '1h', 'composite', now)
    assert not is_directional_certified(certs, 'ETH', '4h', 'adx', now)
    assert not is_directional_certified(certs, 'SOL', '1h', 'composite', now)
    assert not is_directional_certified(certs, 'BTC', '1h', 'adx', now)
    assert not is_directional_certified(certs, 'BTC', '4h', 'composite', now)

def test_backtest_classifier():
    assert backtest_classifier(None) == 'adx'
    assert backtest_classifier({'windows': {}}) == 'composite'

def test_config_directional_classifier_matches_live_resolution():
    assert config_directional_classifier({}, {}) == 'adx'
    assert config_directional_classifier({'windows': {}}, {}) == 'adx'
    windows = {'short': {'classifier': 'adx', 'period': 14}, 'medium': {'classifier': 'composite', 'period': 48}}
    rc = {'enabled': True, 'windows': windows}
    sc_adx = {'regime_directional_window': 'short'}
    assert config_directional_classifier(rc, sc_adx) == 'adx'
    assert backtest_classifier(windows) == 'composite'
    sc_comp = {'regime_directional_window': 'medium'}
    assert config_directional_classifier(rc, sc_comp) == 'composite'
    assert config_directional_classifier(rc, {}) == 'composite'
    assert config_directional_classifier(rc, {'regime_directional_window': 'default'}) == 'composite'
    rc2 = {'windows': {'b_win': {'classifier': 'composite'}, 'a_win': {'classifier': 'adx'}}}
    assert config_directional_classifier(rc2, {}) == 'adx'
    rc3 = {'windows': {'medium': {'period': 48}}}
    assert config_directional_classifier(rc3, {}) == 'adx'

def test_certified_states_active_expired_never(tmp_path):
    now = datetime(2026, 6, 19, tzinfo=timezone.utc)
    future = (now + timedelta(days=2)).isoformat().replace('+00:00', 'Z')
    past = (now - timedelta(days=2)).isoformat().replace('+00:00', 'Z')
    p = tmp_path / 'cert.json'
    p.write_text(json.dumps({'schema_version': 1, 'certified': [{'asset': 'BTC/USDT', 'timeframe': '1h', 'classifier': 'composite', 'expires_at': future, 'states': {'trending_up': 'long', 'trending_down': 'short'}}, {'asset': 'ETH', 'timeframe': '4h', 'classifier': 'adx', 'expires_at': past, 'states': {'trending_down': 'short'}}]}))
    certs = load_certifications(str(p))
    assert certified_states(certs, 'BTC', '1h', 'composite', now) == {'trending_up': 'long', 'trending_down': 'short'}
    assert certified_states(certs, 'ETH', '4h', 'adx', now) is None
    assert certified_states(certs, 'SOL', '1h', 'composite', now) is None

def test_gate_directional_policy_by_states_per_state_sign():
    from backtester import _gate_directional_policy_by_states
    policy = {'trending_up': {'direction': 'short', 'invert_signal': True}, 'trending_down': {'direction': 'short'}, 'ranging': {'direction': 'long'}}
    certs = {'trending_up': 'long', 'trending_down': 'short', 'ranging': 'long'}
    gated = _gate_directional_policy_by_states(policy, certs)
    assert 'trending_up' not in gated
    assert gated['trending_down']['direction'] == 'short'
    assert gated['ranging']['direction'] == 'long'
    pol_both = {'trending_up': {'direction': 'both'}}
    assert _gate_directional_policy_by_states(pol_both, {'trending_up': 'long'}) == pol_both
    assert _gate_directional_policy_by_states(policy, {'trending_up': 'short'}) == {'trending_up': {'direction': 'short', 'invert_signal': True}}
    assert _gate_directional_policy_by_states(policy, {}) == {}
    assert _gate_directional_policy_by_states(policy, None) == policy

def test_repo_artifact_is_empty_and_valid():
    import os
    here = os.path.dirname(os.path.abspath(__file__))
    artifact = os.path.join(here, '..', 'research', 'regime_directional_certifications.json')
    certs = load_certifications(artifact)
    assert certs == {}, 'the shipped artifact must certify nothing (#1076)'

def test_gate_expands_bare_policy_onto_certified_subs():
    from backtester import _gate_directional_policy_by_states
    bare_only = {'ranging_directional': {'direction': 'long'}}
    gated = _gate_directional_policy_by_states(bare_only, {'ranging_directional_up': 'long'})
    assert gated == {'ranging_directional_up': {'direction': 'long'}}
    assert _gate_directional_policy_by_states(bare_only, {'ranging_directional_up': 'short'}) == {}
    mixed = {'ranging_directional': {'direction': 'long'}, 'ranging_directional_up': {'direction': 'both'}}
    gated = _gate_directional_policy_by_states(mixed, {'ranging_directional_up': 'short'})
    assert gated == {'ranging_directional_up': {'direction': 'both'}}

def test_resolve_directional_entry_is_exact_match_only():
    from backtester import _resolve_regime_directional_entry
    bare_only = {'ranging_directional': {'direction': 'short'}}
    assert _resolve_regime_directional_entry(bare_only, 'ranging_directional_down') is None
    assert _resolve_regime_directional_entry(bare_only, 'trending_up') is None
    assert _resolve_regime_directional_entry(bare_only, 'ranging_directional') == {'direction': 'short'}

def test_gate_then_resolve_matches_live_cert_semantics():
    from backtester import _gate_directional_policy_by_states, _resolve_regime_directional_entry
    bare_policy = {'ranging_directional': {'direction': 'long'}}
    gated = _gate_directional_policy_by_states(bare_policy, {'ranging_directional': 'long'})
    assert _resolve_regime_directional_entry(gated, 'ranging_directional_down') is None
    assert _resolve_regime_directional_entry(gated, 'ranging_directional') == {'direction': 'long'}
    gated = _gate_directional_policy_by_states(bare_policy, {'ranging_directional_up': 'long'})
    assert _resolve_regime_directional_entry(gated, 'ranging_directional_up') == {'direction': 'long'}
    gated = _gate_directional_policy_by_states(bare_policy, {'ranging_directional_up': 'short'})
    assert _resolve_regime_directional_entry(gated, 'ranging_directional_up') is None
