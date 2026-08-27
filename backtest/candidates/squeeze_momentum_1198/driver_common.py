import statistics
PARAM_SET_ON: dict = {}
PARAM_SET_OFF: dict = {'kc_mult': 100.0}
PARAM_SET_SELECTIVE: dict = {'kc_mult': 1.3, 'mom_lookback': 16}
ADX_SPEC = {'medium': {'classifier': 'adx', 'period': 14, 'adx_threshold': 20.0}}
COMPOSITE_SPEC = {'medium': {'classifier': 'composite', 'period': 14}}
COMP_UP_FAMILY = ['trending_up_clean', 'trending_up_choppy']
COMP_NOT_DOWN = COMP_UP_FAMILY + ['ranging_quiet', 'ranging_volatile', 'ranging_directional']
COMP_NOT_DOWN_CALM = COMP_UP_FAMILY + ['ranging_quiet', 'ranging_directional']

def adx_spec(threshold: float) -> dict:
    return {'medium': {'classifier': 'adx', 'period': 14, 'adx_threshold': float(threshold)}}

def gate_candidate(label: str, allowed: list, spec: dict) -> dict:
    return {'label': label, 'allowed_regimes': list(allowed), 'regime_windows_spec': {k: dict(v) for k, v in spec.items()}}

def profile_candidate(label: str, profiles: dict, window_spec: dict, off_set: dict=PARAM_SET_OFF) -> dict:
    return {'label': label, 'profile_allocation': {'window_spec': dict(window_spec), 'profiles': dict(profiles), 'param_sets': {'on': dict(PARAM_SET_ON), 'off': dict(off_set)}, 'confirm_bars': 2, 'initial_profile': 'on'}}

def build_gate_grid() -> list:
    rows = [{'label': 'baseline'}, gate_candidate('adx_up', ['trending_up'], ADX_SPEC), gate_candidate('adx_not_down', ['trending_up', 'ranging'], ADX_SPEC), gate_candidate('adx_trend_only', ['trending_up', 'trending_down'], ADX_SPEC), gate_candidate('comp_up_family', COMP_UP_FAMILY, COMPOSITE_SPEC), gate_candidate('comp_up_clean', ['trending_up_clean'], COMPOSITE_SPEC), gate_candidate('comp_not_down', COMP_NOT_DOWN, COMPOSITE_SPEC), gate_candidate('comp_not_down_calm', COMP_NOT_DOWN_CALM, COMPOSITE_SPEC), gate_candidate('comp_up_plus_dir_up', COMP_UP_FAMILY + ['ranging_directional_up'], COMPOSITE_SPEC)]
    return rows

def build_gate_threshold_plateau(allowed: list, thresholds=(15.0, 25.0, 30.0)) -> list:
    return [gate_candidate(f'adx_gate_t{t:g}', allowed, adx_spec(t)) for t in thresholds]

def build_composite_period_plateau(allowed: list, periods=(10, 21, 28)) -> list:
    return [gate_candidate(f'comp_gate_p{p}', allowed, {'medium': {'classifier': 'composite', 'period': int(p)}}) for p in periods]

def build_profile_grid() -> list:
    adx_ws = dict(ADX_SPEC['medium'])
    comp_ws = dict(COMPOSITE_SPEC['medium'])
    comp_profiles_bear_off = {'trending_up_clean': 'on', 'trending_up_choppy': 'on', 'ranging_quiet': 'on', 'ranging_volatile': 'on', 'ranging_directional': 'on', 'ranging_directional_up': 'on', 'ranging_directional_down': 'on', 'trending_down_clean': 'off', 'trending_down_choppy': 'off'}
    return [profile_candidate('m4_bear_off', {'trending_up': 'on', 'ranging': 'on', 'trending_down': 'off'}, adx_ws), profile_candidate('m4_trend_only', {'trending_up': 'on', 'ranging': 'off', 'trending_down': 'off'}, adx_ws), profile_candidate('m4_bear_selective', {'trending_up': 'on', 'ranging': 'on', 'trending_down': 'off'}, adx_ws, off_set=PARAM_SET_SELECTIVE), profile_candidate('m4_comp_bear_off', comp_profiles_bear_off, comp_ws)]

def candidate_leg_kwargs(candidate: dict) -> dict:
    return dict(close_strategies=candidate.get('close_strategies'), direction=candidate.get('direction') or 'long', invert_signal=bool(candidate.get('invert_signal')), stop_loss_atr_mult=candidate.get('stop_loss_atr_mult'), trailing_stop_atr_mult=candidate.get('trailing_stop_atr_mult'), profile_allocation=candidate.get('profile_allocation'), allowed_regimes=candidate.get('allowed_regimes'), regime_windows_spec=candidate.get('regime_windows_spec'), regime_directional_policy=candidate.get('regime_directional_policy'))

def summarize_fee_drag(gross_legs, net_legs):
    pairs = [(g, n) for g, n in zip(gross_legs, net_legs) if g is not None and n is not None]
    if not pairs:
        return None
    gross = [g['return_pct'] for g, _ in pairs]
    net = [n['return_pct'] for _, n in pairs]
    trades = sum((n['trades'] for _, n in pairs))
    span_days = sum((float(n.get('span_days') or 0.0) for _, n in pairs))
    mean_gross = statistics.mean(gross)
    mean_net = statistics.mean(net)
    return {'legs': len(pairs), 'mean_gross_return_pct': round(mean_gross, 2), 'mean_net_return_pct': round(mean_net, 2), 'drag_pp': round(mean_gross - mean_net, 2), 'trades': trades, 'trades_per_year': round(trades / (span_days / 365.25), 1) if span_days > 0 else None}
