import json
import os
import sys
import numpy as np
import pandas as pd
import pytest
sys.path.insert(0, os.path.abspath(os.path.join(os.path.dirname(__file__), '..', 'research')))
import hurst_1426_two_sided_sort as study
import hurst_1424_gate_resolution as study1424
import hurst_1422_gate_power as study1422
import hurst_1410_gate_calibration as study1410
_MR = 'mean_reversion'
CONTRACT = os.path.join(os.path.dirname(study._DEFAULT_REPORT_OUT), 'hurst_gate_calibration.md')

def _trade(symbol='BTC/USDT', timeframe='1h', window='2021', day=0, pnl=1.0, eff=0.1, hold_days=1, h=None, adx=None, cohort=None, exchange='binanceus', armed=None):
    entry = pd.Timestamp('2021-01-01') + pd.Timedelta(days=day)
    return {'strategy': 'momentum', 'exchange': exchange, 'symbol': symbol, 'base_symbol': symbol.split('@', 1)[0], 'timeframe': timeframe, 'window': window, 'cohort': cohort or study.cell_cohort(exchange, symbol.split('@', 1)[0], timeframe, window), 'entry_date': str(entry), 'entry_ns': int(entry.value), 'exit_ns': int((entry + pd.Timedelta(days=hold_days)).value), 'pnl_pct_net': float(pnl), 'efficiency': None if eff is None else float(eff), 'adx': adx, 'h': {512: h, 128: h}, 'armed': dict(armed or {})}

def _rotatable_pool(n=60):
    return [_trade(day=i * 5) for i in range(n)] + [_trade(symbol='ETH/USDT', day=i * 5) for i in range(n)]

def _random_mask(n, seed=7):
    return list(np.random.default_rng(seed).random(n) < 0.5)

def test_doubled_tail_formula_is_the_pre_registered_one():
    assert study.doubled_tail_p(0, 999, 999) == pytest.approx(2.0 / 1000.0, abs=1e-06)
    assert study.doubled_tail_p(999, 0, 999) == pytest.approx(2.0 / 1000.0, abs=1e-06)
    assert study.doubled_tail_p(500, 500, 1000) == 1.0
    assert study.doubled_tail_p(20, 80, 100) == pytest.approx(2.0 * 21.0 / 101.0, abs=1e-06)

def test_doubled_tail_is_capped_at_one():
    assert study.doubled_tail_p(400, 400, 500) == 1.0

def test_doubled_tail_is_untestable_without_draws():
    assert study.doubled_tail_p(0, 0, 0) is None
    assert study.doubled_tail_p(3, 4, -1) is None

def test_the_smallest_reachable_p_is_two_over_draws_plus_one():
    assert study.doubled_tail_p(0, 1999, 1999) == pytest.approx(2.0 / 2000.0)

def test_both_tails_are_counted_over_surviving_draws_not_the_request():
    trades = _rotatable_pool(40)
    mask = _random_mask(80)
    values = [1.0 if m else 0.0 for m in mask]
    out = study.two_sided_cluster_permutation_pvalue_group_diff(trades, values, mask, n_perm=300, seed=1426)
    assert out['n_draws'] > 0
    assert out['p'] == pytest.approx(study.doubled_tail_p(0, out['n_draws'], out['n_draws']), abs=0.05)

def _reversed_pool():
    trades = _rotatable_pool(60)
    mask = _random_mask(120)
    values = [1.0 if m else 0.0 for m in mask]
    return (trades, values, mask)

def test_a_reversed_effect_is_detected_by_the_two_sided_cluster_null():
    trades, values, mask = _reversed_pool()
    one = study1422.cluster_permutation_pvalue_group_diff(trades, values, mask, n_perm=2000, seed=1426)['p']
    two = study.two_sided_cluster_permutation_pvalue_group_diff(trades, values, mask, n_perm=2000, seed=1426)['p']
    assert one > 0.9
    assert two <= study.ALPHA

def test_a_reversed_effect_is_detected_by_the_two_sided_free_shuffle():
    _trades, values, mask = _reversed_pool()
    assert study1410.permutation_pvalue_group_diff(values, mask, n_perm=2000, seed=1426) > 0.9
    assert study.two_sided_permutation_pvalue_group_diff(values, mask, n_perm=2000, seed=1426) <= study.ALPHA

def test_a_forward_effect_is_still_detected():
    trades = _rotatable_pool(60)
    mask = _random_mask(120)
    values = [0.0 if m else 1.0 for m in mask]
    assert study.two_sided_cluster_permutation_pvalue_group_diff(trades, values, mask, n_perm=2000, seed=1426)['p'] <= study.ALPHA
    assert study.two_sided_permutation_pvalue_group_diff(values, mask, n_perm=2000, seed=1426) <= study.ALPHA

def test_null_data_is_not_flagged_by_either_two_sided_function():
    trades = _rotatable_pool(60)
    mask = _random_mask(120)
    values = list(np.random.default_rng(99).normal(0.0, 1.0, size=120))
    assert study.two_sided_permutation_pvalue_group_diff(values, mask, n_perm=2000, seed=1426) > study.ALPHA
    assert study.two_sided_cluster_permutation_pvalue_group_diff(trades, values, mask, n_perm=2000, seed=1426)['p'] > study.ALPHA

def test_a_reversed_sizing_pairing_is_detected():
    trades = _rotatable_pool(60)
    rng = np.random.default_rng(3)
    rets = list(rng.normal(0.0, 1.0, size=120))
    mults = [2.0 if r < 0 else 0.5 for r in rets]
    assert study1410.permutation_pvalue_weighted(rets, mults, n_perm=2000, seed=1426) > 0.9
    assert study.two_sided_permutation_pvalue_weighted(rets, mults, n_perm=2000, seed=1426) <= study.ALPHA
    assert study.two_sided_cluster_permutation_pvalue_weighted(trades, rets, mults, n_perm=2000, seed=1426)['p'] <= study.ALPHA

def test_the_two_sided_functions_keep_the_untestable_semantics():
    assert study.two_sided_permutation_pvalue_group_diff([], [], n_perm=10) is None
    assert study.two_sided_permutation_pvalue_group_diff([1.0, 2.0], [True, True], n_perm=10) is None
    assert study.two_sided_permutation_pvalue_weighted([1.0, 2.0], [1.0, 1.0], n_perm=10) is None
    out = study.two_sided_cluster_permutation_pvalue_group_diff([], [], [], n_perm=10)
    assert out['p'] is None and out['reason']

def test_a_pool_too_short_to_rotate_is_untestable_not_insignificant():
    trades = [_trade(day=i) for i in range(6)]
    out = study.two_sided_cluster_permutation_pvalue_group_diff(trades, [1.0] * 6, [True, False] * 3, n_perm=100, seed=1426)
    assert out['p'] is None
    assert 'calendar time' in out['reason']

def test_the_cluster_null_reuses_1422s_rotation_internals():
    assert study.cluster_rotation_offsets is study1422.cluster_rotation_offsets
    assert study.rotation_shift_counts is study1422.rotation_shift_counts
    assert study._rotate_values is study1422._rotate_values
    assert study._admissible_offsets is study1422._admissible_offsets
    assert study.usable_cluster_rows is study1422.usable_cluster_rows
_ONE_SIDED = ((study1410, 'permutation_pvalue_group_diff'), (study1410, 'permutation_pvalue_weighted'), (study1422, 'cluster_permutation_pvalue_group_diff'), (study1422, 'cluster_permutation_pvalue_weighted'), (study1424, 'permutation_pvalue_group_diff'), (study1424, 'permutation_pvalue_weighted'), (study1424, 'cluster_permutation_pvalue_group_diff'), (study1424, 'cluster_permutation_pvalue_weighted'), (study1424, 'min_detectable_effect_on_grid'), (study1424, 'min_detectable_effect_eff'), (study1424, 'min_detectable_effect_pp'))

@pytest.fixture()
def one_sided_is_a_landmine(monkeypatch):

    def _boom(*_a, **_kw):
        raise AssertionError('a one-sided p-value function was reached from the two-sided confirmatory path')
    for module, name in _ONE_SIDED:
        monkeypatch.setattr(module, name, _boom, raising=False)
    return _boom
_SWEEP_N_PERM = int(np.ceil(2.0 / (study.ALPHA / 30.0)))

def _pooled_for_sweep():
    cid = study.PRIMARY_CONFIG_ID
    rows = []
    mask = _random_mask(80, seed=11)
    for i in range(40):
        rows.append(_trade(day=i * 5, h=0.6 if mask[i] else 0.4, eff=1.0 if mask[i] else -1.0, armed={cid: bool(mask[i])}))
        rows.append(_trade(symbol='ETH/USDT', day=i * 5, h=0.6 if mask[40 + i] else 0.4, eff=1.0 if mask[40 + i] else -1.0, armed={cid: bool(mask[40 + i])}))
    return {study.PRIMARY_FAMILY: rows, _MR: []}

def test_build_configs_never_reaches_a_one_sided_p(one_sided_is_a_landmine):
    pooled = _pooled_for_sweep()
    cfgs = study.build_configs([], pooled, [512], {}, n_perm=50, seed=study.SEED)
    assert cfgs
    assert any((c['p_cluster'] is not None or c['p_raw'] is not None for c in cfgs))

def test_measure_detection_limits_never_reaches_a_one_sided_p(one_sided_is_a_landmine):
    study.measure_detection_limits(_pooled_for_sweep(), [512], n_perm=_SWEEP_N_PERM, seed=study.SEED)

def test_the_two_sided_mde_never_reaches_a_one_sided_p(one_sided_is_a_landmine):
    trades = _rotatable_pool(30)
    mask = _random_mask(60, seed=5)
    values = list(np.random.default_rng(2).normal(0.0, 0.3, size=60))
    study.two_sided_min_detectable_effect_on_grid(trades, values, mask, 1, grid_step=0.5, grid_max=0.5, refine_step=0.5, cluster=False, n_perm=200)

def test_stage_0_is_the_deliberately_inherited_one_sided_exception():
    assert study.joint_separation_verdict.__doc__
    assert 'ONE-SIDED' in study.joint_separation_verdict.__doc__
    assert '#1412' in study.joint_separation_verdict.__doc__

def test_stage_0_is_never_called_from_the_confirmatory_path(monkeypatch):

    def _boom(*_a, **_kw):
        raise AssertionError('Stage 0 was reached from the confirmatory path')
    monkeypatch.setattr(study, 'joint_separation_verdict', _boom)
    monkeypatch.setattr(study1422, 'joint_separation_verdict', _boom)
    pooled = _pooled_for_sweep()
    cfgs = study.build_configs([], pooled, [512], {}, n_perm=50, seed=study.SEED)
    mde = study.measure_detection_limits(pooled, [512], n_perm=_SWEEP_N_PERM, seed=study.SEED)
    study.validity_gate(mde)
    study.decide_recommendation(cfgs, mde)

def _mde_inputs(n=120, seed=1426):
    rng = np.random.default_rng(seed)
    trades = _rotatable_pool(n // 2)
    mask = _random_mask(n, seed=seed)
    values = list(rng.normal(0.0, 0.3, size=n))
    return (trades, values, mask)

def _single_direction_limit(values, mask, sign, *, grid_step=0.05, grid_max=1.0, n_perm=1000):
    vals = np.asarray(values, dtype=float)
    m = np.asarray(mask, dtype=bool)
    bar = study._rank1_threshold(1, study.ALPHA)
    for i in range(int(round(grid_max / grid_step)) + 1):
        d = round(i * grid_step, 9)
        shifted = vals - np.where(m, sign * d, 0.0)
        p = study.two_sided_permutation_pvalue_group_diff(shifted, m, n_perm=n_perm, seed=study.SEED)
        if p is not None and p <= bar:
            return d
    return None

def test_the_two_sided_limit_is_the_max_over_the_two_directions():
    trades, values, mask = _mde_inputs()
    both = study.two_sided_min_detectable_effect_on_grid(trades, values, mask, 1, grid_step=0.05, grid_max=1.0, refine_step=0.05, cluster=False, n_perm=1000, seed=study.SEED)
    up = _single_direction_limit(values, mask, +1.0)
    down = _single_direction_limit(values, mask, -1.0)
    assert None not in (both, up, down)
    assert both == max(up, down)

def test_an_injection_of_the_published_limit_is_detected_pointing_either_way():
    trades, values, mask = _mde_inputs()
    limit = study.two_sided_min_detectable_effect_on_grid(trades, values, mask, 1, grid_step=0.05, grid_max=1.0, refine_step=0.05, cluster=False, n_perm=1000, seed=study.SEED)
    assert limit is not None
    vals = np.asarray(values, dtype=float)
    m = np.asarray(mask, dtype=bool)
    for sign in (+1.0, -1.0):
        shifted = vals - np.where(m, sign * limit, 0.0)
        p = study.two_sided_permutation_pvalue_group_diff(shifted, m, n_perm=1000, seed=study.SEED)
        assert p is not None and p <= study.ALPHA, (sign, p)

def test_the_resolvability_floor_is_two_over_n_plus_one():
    trades, values, mask = _mde_inputs(n=60)
    bar = study._rank1_threshold(1, study.ALPHA)
    with pytest.raises(ValueError) as exc:
        study.two_sided_min_detectable_effect_on_grid(trades, values, mask, 1, grid_step=0.5, grid_max=0.5, refine_step=0.5, cluster=False, n_perm=38)
    assert 'cannot resolve' in str(exc.value)
    assert 'TWO-SIDED' in str(exc.value)
    assert 1.0 / 39.0 <= bar < 2.0 / 39.0
    study.two_sided_min_detectable_effect_on_grid(trades, values, mask, 1, grid_step=0.5, grid_max=0.5, refine_step=0.5, cluster=False, n_perm=39)

def test_the_mde_returns_none_when_no_grid_point_clears_the_bar():
    trades, values, mask = _mde_inputs(n=60)
    out = study.two_sided_min_detectable_effect_on_grid(trades, values, mask, 1, grid_step=0.0001, grid_max=0.0001, refine_step=0.0001, cluster=False, n_perm=200)
    assert out is None or out <= 0.0001

def test_the_eff_and_pp_wrappers_use_1424s_grids_verbatim():
    calls = []

    def _spy(*_args, **kwargs):
        calls.append((kwargs['grid_step'], kwargs['grid_max'], kwargs['refine_step']))
        return 0.0
    original = study.two_sided_min_detectable_effect_on_grid
    study.two_sided_min_detectable_effect_on_grid = _spy
    try:
        study.two_sided_min_detectable_effect_eff([], [], [], 1)
        study.two_sided_min_detectable_effect_pp([], [], [], 1)
    finally:
        study.two_sided_min_detectable_effect_on_grid = original
    assert calls == [(study1424.MDE_EFF_GRID_STEP, study1424.MDE_EFF_GRID_MAX, study1424.MDE_EFF_REFINE_STEP), (study1424.MDE_PP_GRID_STEP, study1424.MDE_PP_GRID_MAX, study1424.MDE_PP_REFINE_STEP)]

def test_the_grids_are_inherited_rather_than_restated():
    assert study.MDE_EFF_GRID_STEP == study1424.MDE_EFF_GRID_STEP
    assert study.MDE_EFF_GRID_MAX == study1424.MDE_EFF_GRID_MAX
    assert study.MDE_EFF_REFINE_STEP == study1424.MDE_EFF_REFINE_STEP
    assert study.MDE_PP_GRID_STEP == study1422.MDE_GRID_STEP
    assert study.MDE_PP_GRID_MAX == study1422.MDE_GRID_MAX
    assert study.MDE_PP_REFINE_STEP == study1422.MDE_REFINE_STEP

def test_detection_limits_report_a_row_matched_zero_injection_p():
    mde = study.measure_detection_limits(_pooled_for_sweep(), [512], n_perm=_SWEEP_N_PERM, seed=study.SEED)
    assert study.PRIMARY_FAMILY in mde['by_family_cluster_p0']
    assert study.PRIMARY_FAMILY in mde['by_family_n']
    assert mde['two_sided'] is True

def _mde(mom_limit=0.05, mom_sep=0.09, mr_limit=0.05, mr_sep=0.02, p0=0.4, pooled=None, **extra):
    out = {'by_family_cluster': {study.PRIMARY_FAMILY: mom_limit, _MR: mr_limit}, 'by_family_separation': {study.PRIMARY_FAMILY: mom_sep, _MR: mr_sep}, 'by_family_cluster_p0': {study.PRIMARY_FAMILY: p0, _MR: 0.6}, 'by_family_n': {study.PRIMARY_FAMILY: 100, _MR: 100}, 'pooled_primary_cluster': 0.001 if pooled is None else pooled, 'observed_separation_by_pool': {'primary': {f'{study.PRIMARY_FAMILY}|512': mom_sep, f'{_MR}|512': mr_sep}}}
    out.update(extra)
    return out

def test_the_gate_passes_on_a_positive_separation_above_the_limit():
    gate = study.validity_gate(_mde(mom_limit=0.05, mom_sep=0.09))
    assert gate['passed'] is True
    assert gate['mode'] == study.MODE_OK
    assert gate['family'] == study.PRIMARY_FAMILY

def test_the_gate_passes_on_a_NEGATIVE_separation_above_the_limit():
    gate = study.validity_gate(_mde(mom_limit=0.01, mom_sep=-0.3))
    assert gate['passed'] is True
    assert gate['mode'] == study.MODE_OK
    assert study1424.validity_gate(_mde(mom_limit=0.01, mom_sep=-0.3))['passed'] is False

def test_the_gate_preserves_the_sign_it_passed_on():
    gate = study.validity_gate(_mde(mom_limit=0.01, mom_sep=-0.3))
    assert gate['largest_separation'] == pytest.approx(-0.3)

def test_the_gate_fails_below_the_limit_in_either_direction():
    assert study.validity_gate(_mde(mom_limit=0.2, mom_sep=0.09))['passed'] is False
    assert study.validity_gate(_mde(mom_limit=0.2, mom_sep=-0.09))['passed'] is False

def test_the_gate_fails_closed_on_an_unreachable_limit():
    gate = study.validity_gate(_mde(mom_limit=None, mom_sep=-0.3))
    assert gate['passed'] is False
    assert gate['mode'] == study.MODE_UNRESOLVABLE
    assert 'either direction' in gate['reason']

def test_the_gate_fails_closed_with_no_separation_at_all():
    gate = study.validity_gate({'by_family_cluster': {study.PRIMARY_FAMILY: 0.05}, 'by_family_separation': {}})
    assert gate['passed'] is False
    assert gate['mode'] == study.MODE_NO_SEPARATION

def test_the_gate_reads_the_confirmatory_familys_own_rows_not_the_pool():
    gate = study.validity_gate(_mde(mom_limit=0.05, mom_sep=0.009, mr_limit=0.05, mr_sep=0.008, pooled=0.004))
    assert gate['passed'] is False
    assert gate['limit'] == pytest.approx(0.05)

def test_the_gate_never_borrows_the_other_familys_limit():
    gate = study.validity_gate(_mde(mom_limit=None, mom_sep=-0.3, mr_limit=0.001, mr_sep=0.5))
    assert gate['passed'] is False
    assert gate['limit'] is None

def test_the_gate_is_never_read_against_a_mismatched_pool():
    mde = _mde(mom_limit=0.2, mom_sep=0.09)
    mde['observed_separation_by_pool']['exploratory'] = {f'{study.PRIMARY_FAMILY}|512': 0.9}
    assert study.validity_gate(mde)['passed'] is False

def test_the_gate_ignores_the_other_familys_larger_separation():
    gate = study.validity_gate(_mde(mom_limit=0.2, mom_sep=0.05, mr_limit=0.001, mr_sep=-0.9))
    assert gate['passed'] is False
    assert gate['largest_separation'] == pytest.approx(0.05)

def test_there_is_no_reversed_mode_in_a_symmetric_study():
    assert not hasattr(study, 'MODE_REVERSED')
    for sep in (-0.3, 0.3):
        assert study.validity_gate(_mde(mom_limit=0.01, mom_sep=sep))['mode'] == study.MODE_OK

def test_the_gate_marks_itself_two_sided():
    assert study.validity_gate(_mde())['two_sided'] is True

def _passing_cfg(**over):
    cfg = {'config_id': study.PRIMARY_CONFIG_ID, 'cohort': study.COHORT_PRIMARY, 'family': study.PRIMARY_FAMILY, 'mode': 'gate', 'hurst_window': 512, 'n_pooled_trades': 400, 'n_pooled_effective': 60.0, 'n_suppressed_effective': 100.0, 'n_kept_effective': 100.0, 'n_suppressed': 200, 'n_kept': 200, 'n_missing_target': 0, 'separation': -0.005, 'separation_return': -0.12, 'p_raw': 0.001, 'p_cluster': 0.001, 'p_cluster_return': 0.01, 'bh_reject': True, 'cluster_excluded_datasets': [], 'cluster_excluded_trades': 0, 'protocol_windows': list(study.PRIMARY_PROTOCOL_WINDOWS), 'protocol_min_windows': study.PRIMARY_PROTOCOL_MIN_WINDOWS, 'held_out_windows': list(study.PRIMARY_HELD_OUT_WINDOWS), 'windows': {w: {'n_legs': 1, 'dd_delta': -1.0, 'chop_delta': -1.0, 'ret_gated': 10.0, 'ret_ungated': 10.0} for w in study.WINDOW_ORDER}}
    cfg.update(over)
    return cfg

def test_this_module_defines_no_configuration_verdict():
    assert not hasattr(study, 'VERDICT_CONFIG')
    assert 'config' not in study.VERDICT_LABELS

def test_a_config_that_passes_1424s_rule_still_wins_nothing():
    cfg = _passing_cfg()
    assert study1424.config_verdict(cfg)[0] is True
    decision = study.decide_recommendation([cfg], _mde(p0=0.001, mom_limit=0.01, mom_sep=-0.3))
    assert all((v['winner'] is None for v in decision['families'].values()))
    assert decision['families'][study.PRIMARY_FAMILY]['n_passing'] == 1

def test_no_verdict_can_name_a_winner_whatever_the_inputs():
    for p0, limit, sep in ((0.001, 0.01, -0.3), (0.9, 0.01, -0.3), (0.9, 0.5, -0.005), (0.001, 0.5, 0.9)):
        decision = study.decide_recommendation([_passing_cfg()], _mde(p0=p0, mom_limit=limit, mom_sep=sep))
        assert all((v['winner'] is None for v in decision['families'].values())), (p0, limit, sep)

def test_every_verdict_carries_the_exploratory_only_sentence():
    seen = set()
    for p0, limit, sep in ((0.001, 0.01, -0.3), (0.9, 0.01, -0.3), (0.9, 0.5, -0.005)):
        decision = study.decide_recommendation([_passing_cfg()], _mde(p0=p0, mom_limit=limit, mom_sep=sep))
        seen.add(decision['verdict'])
        assert study.EXPLORATORY_ONLY_SENTENCE in decision['justification']
    assert seen == {study.VERDICT_SORT_DETECTED, study.VERDICT_RESOLVED_NULL, study.VERDICT_INCONCLUSIVE}

def test_sort_detected_names_the_direction_from_the_sign():
    down = study.decide_recommendation([], _mde(p0=0.001, mom_limit=0.01, mom_sep=-0.3))
    assert down['verdict'] == study.VERDICT_SORT_DETECTED
    assert 'SUPPRESSED trades did better' in down['justification']
    assert 'would have HURT' in down['justification']
    up = study.decide_recommendation([], _mde(p0=0.001, mom_limit=0.01, mom_sep=0.3))
    assert up['verdict'] == study.VERDICT_SORT_DETECTED
    assert 'KEPT trades did better' in up['justification']

def test_a_resolved_null_claims_a_bound_in_both_directions():
    decision = study.decide_recommendation([], _mde(p0=0.9, mom_limit=0.01, mom_sep=-0.3))
    assert decision['verdict'] == study.VERDICT_RESOLVED_NULL
    assert decision['key_risk_held'] is True
    assert 'EITHER DIRECTION' in decision['justification']

def test_an_underpowered_null_claims_no_bound_and_stays_inconclusive():
    decision = study.decide_recommendation([], _mde(p0=0.9, mom_limit=0.5, mom_sep=-0.005))
    assert decision['verdict'] == study.VERDICT_INCONCLUSIVE
    assert decision['key_risk_held'] is False
    assert 'BELOW the limit' in decision['justification']

def test_an_untestable_confirmatory_p_is_not_significance():
    decision = study.decide_recommendation([], _mde(p0=None, mom_limit=0.5, mom_sep=-0.005))
    assert decision['significant'] is False
    assert decision['verdict'] == study.VERDICT_INCONCLUSIVE
    assert 'untestable' in decision['justification']

def test_the_confirmatory_p_is_the_row_matched_one_not_the_pool():
    mde = _mde(p0=0.9)
    mde['pooled_primary_cluster_p0'] = 0.001
    assert study.confirmatory_p(mde) == pytest.approx(0.9)
    assert study.decide_recommendation([], mde)['significant'] is False

def test_the_confirmatory_bar_is_alpha_for_a_family_of_one():
    decision = study.decide_recommendation([], _mde())
    assert decision['confirmatory_bar'] == pytest.approx(study.ALPHA)
    assert study.PRIMARY_FAMILY_SIZE == 1

def test_the_decision_payload_never_names_a_winner():
    decision = study.decide_recommendation([_passing_cfg()], _mde(p0=0.001, mom_limit=0.01, mom_sep=-0.3))
    payload = study.decision_payload(decision)
    assert all((v['winner'] is None for v in payload['families'].values()))
    assert payload['cohort_option'] == study.COHORT_OPTION

def test_the_cohort_option_is_declared_as_data():
    assert study.COHORT_OPTION == 'exploratory_only_full_pool'
    assert study.CONTRACT_PATH_CLAIMED is False
    assert study.SIBLING_DEFERRAL == (1427, 1428)
    assert study.TWO_SIDED is True
_FAILING_MDE = _mde(mom_limit=0.02, mom_sep=-0.005, p0=0.71, pooled_primary_cluster=0.008, pooled_primary_free=0.006, pooled_primary_cluster_return=1.2, pooled_primary_n=28998, pooled_primary_cluster_p0=0.9, pooled_primary_free_p0=0.9, pooled_primary_cluster_return_p0=1.0)

def _render_payload(decision=None, mde=None, configs=None):
    mde = dict(mde or _FAILING_MDE)
    mde.setdefault('observed_separation_pp_by_pool', {'primary': {f'{study.PRIMARY_FAMILY}|512': -0.12}})
    mde.setdefault('by_family_cluster_return', {study.PRIMARY_FAMILY: 1.4, _MR: 0.9})
    mde.setdefault('by_family_separation_return', {study.PRIMARY_FAMILY: -0.12, _MR: -0.23})
    mde.setdefault('by_family_cluster_return_p0', {study.PRIMARY_FAMILY: 0.85, _MR: 0.9})
    cfgs = list(configs if configs is not None else [_passing_cfg(bh_reject=False, p_cluster=0.71, p_raw=0.7, p_cluster_return=0.85)])
    decision = decision or study.decide_recommendation(cfgs, mde)
    return {'schema_version': study.SCHEMA_VERSION, 'issue': study.ISSUE, 'pre_registered': {'hurst_windows': [512], 'windows': {w: list(study.WINDOWS[w]) for w in study.WINDOW_ORDER}, 'datasets': ['BTC/USDT 1h'], 'fee_platform': study.FEE_PLATFORM, 'n_perm': study.N_PERM, 'n_perm_mde': study.N_PERM_MDE, 'seed': study.SEED, 'feasibility_probes': [dict(p) for p in study.FEASIBILITY_PROBES]}, 'run_summary': {'scope': {'complete': True, 'pre_registered_inference': True}, 'legs': 1, 'gated_arms': 9, 'mirror_verified_legs': 1, 'pooled_trades': {f: 1 for f in study.FAMILIES}, 'pooled_primary': {f: 1 for f in study.FAMILIES}, 'pooled_exploratory': {f: 0 for f in study.FAMILIES}, 'n_primary_configs': 1, 'n_exploratory_configs': 30, 'n_primary_significant': 0, 'n_exploratory_significant': 0, 'warmup': {'required_bars': 522, 'min_lead_bars': 900, 'sufficient': True, 'insufficient_datasets': [], 'lead_bars': {}}, 'coverage': {'n_kept': 1, 'n_cells': 1, 'n_dropped': 0, 'n_unowned': 3, 'required_lead_bars': 522, 'min_window_bar_fraction': 0.8, 'reference_last_bar': '2026-01-01', 'dropped': []}, 'symbol_correlations': {}, 'elapsed_sec': 1.0}, 'mde': mde, 'buckets': {f: {'512': study.bucket_tables([], 512)} for f in study.FAMILIES}, 'joint': {f: {'table': {}, 'verdict': {'separated': False, 'reason': 'test'}} for f in study.FAMILIES}, 'configs': cfgs, 'legs': [], 'decision': decision}

def test_report_renders_and_ends_with_the_recommendation():
    text = study.report_from_payload(_render_payload())
    body, _, tail = text.rpartition('## Recommendation')
    assert body and '## Recommendation' not in tail

def test_report_states_the_cohort_decision_and_the_interim_look_verbatim():
    text = study.report_from_payload(_render_payload())
    assert study.COHORT_DECISION_STATEMENT in text
    assert study.INTERIM_LOOK_DISCLOSURE in text
    assert study.KEY_RISK_PREDICTION in text

def test_report_names_the_contract_path_deferral_and_both_siblings():
    text = study.report_from_payload(_render_payload())
    assert study.CONTRACT_PATH_STATEMENT in text
    assert '#1427' in text and '#1428' in text

def test_report_prints_the_two_sided_p_definition():
    text = study.report_from_payload(_render_payload())
    assert 'p2   = min(1, 2 * min(p_ge, p_le))' in text
    assert '2/(draws+1)' in text

def test_report_discloses_the_stage_0_one_sided_exception():
    text = study.report_from_payload(_render_payload())
    assert 'ONE DELIBERATE EXCEPTION' in text
    assert 'Stage 0' in text

def test_report_says_no_configuration_can_be_recommended():
    text = study.report_from_payload(_render_payload())
    assert 'No configuration is recommended, and none could be.' in text
    assert 'none (structurally)' in text
    assert 'DEFAULT-OFF' in text

def test_report_prints_the_validity_gate_outcome_both_ways():
    failing = study.report_from_payload(_render_payload())
    assert '**Outcome: FAILED**' in failing
    passing = study.report_from_payload(_render_payload(mde=_mde(mom_limit=0.001, mom_sep=-0.3, p0=0.9)))
    assert '**Outcome: PASSED**' in passing

def test_report_states_what_the_study_cannot_say():
    text = study.report_from_payload(_render_payload())
    assert '## What this study cannot say' in text
    assert 'cannot CONFIRM anything' in text

def test_largest_magnitude_signed_prefers_size_and_keeps_the_sign():
    assert study._largest_magnitude_signed({'a|512': 0.1, 'b|512': -0.4}) == pytest.approx(-0.4)
    assert study._largest_magnitude_signed({'a|512': 0.4, 'b|512': -0.1}) == pytest.approx(0.4)
    assert study._largest_magnitude_signed({}) is None
    assert study._largest_magnitude_signed({'a|512': None}) is None
    assert study._largest_magnitude_signed({'a|512': 0.3, 'b|512': -0.3}) == pytest.approx(0.3)

def test_a_missing_separation_renders_as_a_dash_never_as_zero():
    assert study._fmt_signed(None) == '-'
    assert study._fmt_signed(0.0) == '+0.00'

def test_the_report_names_the_numbers_the_gate_actually_reads():
    text = study.report_from_payload(_render_payload())
    assert 'neither of the pooled rows above' in text
    assert 'row-matched rule' in text

def test_the_report_explains_why_a_magnitude_comparison_is_legitimate_here():
    text = study.report_from_payload(_render_payload())
    assert 'That is not a relaxation' in text
    assert 'must restore the signed comparison in the same change' in text

def test_the_pre_registered_draw_count_resolves_the_binding_bar():
    binding = study.ALPHA / 30.0
    confirmatory = study.ALPHA / study.PRIMARY_FAMILY_SIZE
    assert binding < confirmatory
    assert 2.0 / (study.N_PERM_MDE + 1.0) <= binding, 'the pre-registered detection-limit draw count must resolve the tightest bar in the run'

def test_the_report_names_the_binding_bar_rather_than_the_easier_one():
    text = study.report_from_payload(_render_payload())
    assert 'BINDING constraint' in text
    assert f'{study.ALPHA / 30.0:.5f}' in text

def test_the_prediction_does_not_promise_a_bound_it_cannot_deliver():
    text = study.KEY_RISK_PREDICTION
    assert 'INCONCLUSIVE' in text
    assert 'NO bound' in text
    assert 'resolved_null' in text
    assert 'BLIND SPOT' in text

def test_only_a_passing_gate_ever_claims_a_bound():
    bound = study.decide_recommendation([], _mde(p0=0.9, mom_limit=0.01, mom_sep=-0.3))
    assert bound['verdict'] == study.VERDICT_RESOLVED_NULL
    assert bound['validity_gate']['passed'] is True
    no_bound = study.decide_recommendation([], _mde(p0=0.9, mom_limit=0.5, mom_sep=-0.005))
    assert no_bound['verdict'] == study.VERDICT_INCONCLUSIVE
    assert no_bound['validity_gate']['passed'] is False
    assert 'EITHER DIRECTION' not in no_bound['justification']
    assert 'bounds any sorting effect' in no_bound['justification'] or 'no bound' in no_bound['justification']

def test_the_recommendation_only_claims_the_bound_when_the_gate_passed():
    passing = study.report_from_payload(_render_payload(mde=_mde(mom_limit=0.001, mom_sep=-0.3, p0=0.9)))
    assert 'bounds any sorting effect in BOTH directions' in passing
    failing = study.report_from_payload(_render_payload())
    assert 'bounds any sorting effect in BOTH directions' not in failing
    assert 'carries no bound' in failing

def test_1426_does_not_default_to_the_contract_path():
    assert os.path.basename(study._DEFAULT_REPORT_OUT) == 'hurst_1426_two_sided_sort.md'
    assert os.path.basename(study._DEFAULT_JSON_OUT) == 'hurst_1426_two_sided_sort.json'

def test_1424_still_owns_the_contract_path():
    assert os.path.basename(study1424._DEFAULT_REPORT_OUT) == 'hurst_gate_calibration.md'

def test_1426_may_not_write_the_contract_path_even_when_asked(tmp_path):
    with pytest.raises(SystemExit) as exc:
        study.main(['--report-out', CONTRACT, '--json-out', str(tmp_path / 'scoped.json')])
    assert 'DEFERS' in str(exc.value)
    assert '1424' in str(exc.value)

def test_the_contract_refusal_survives_render_only(tmp_path):
    path = tmp_path / 'complete.json'
    payload = _render_payload()
    payload['decision'] = study.decision_payload(payload['decision'])
    path.write_text(json.dumps(payload))
    with pytest.raises(SystemExit) as exc:
        study.main(['--render-only', '--json-out', str(path), '--report-out', CONTRACT, '--write-report'])
    assert 'DEFERS' in str(exc.value)

def test_the_contract_refusal_is_checked_before_every_other_refusal(tmp_path):
    with pytest.raises(SystemExit) as exc:
        study.main(['--only', 'momentum', '--report-out', CONTRACT, '--json-out', str(tmp_path / 'scoped.json')])
    assert 'DEFERS' in str(exc.value)

def test_scoped_run_may_not_overwrite_the_committed_json():
    with pytest.raises(SystemExit) as exc:
        study.main(['--only', 'momentum'])
    assert 'committed aggregate' in str(exc.value)

@pytest.mark.parametrize('flag,value', [('--only', 'momentum'), ('--datasets', 'BTC/USDT:1h'), ('--windows', '2017'), ('--hurst-windows', '128')])
def test_every_scoping_flag_protects_the_committed_report(tmp_path, flag, value):
    with pytest.raises(SystemExit) as exc:
        study.main([flag, value, '--json-out', str(tmp_path / 'scoped.json')])
    assert 'committed report' in str(exc.value)

@pytest.mark.parametrize('argv,needle', [(['--n-perm-mde', '200'], '--n-perm-mde 200'), (['--n-perm', '200'], '--n-perm 200'), (['--seed', '7'], '--seed 7'), (['--no-mirror-check'], '--no-mirror-check')])
def test_a_deviating_run_may_not_write_the_committed_artifacts(tmp_path, argv, needle):
    with pytest.raises(SystemExit) as exc:
        study.main(argv)
    assert 'committed aggregate' in str(exc.value)
    assert needle in str(exc.value)
    with pytest.raises(SystemExit) as exc:
        study.main(argv + ['--json-out', str(tmp_path / 'debug.json')])
    assert 'committed report' in str(exc.value)

class _Args:

    def __init__(self, **kw):
        self.n_perm = kw.get('n_perm', study.N_PERM)
        self.n_perm_mde = kw.get('n_perm_mde', study.N_PERM_MDE)
        self.seed = kw.get('seed', study.SEED)
        self.no_mirror_check = kw.get('no_mirror_check', False)

def test_stating_the_pre_registered_settings_explicitly_is_not_a_deviation():
    assert study.inference_deviations(_Args()) == []
    assert study.inference_deviations(_Args(seed=study.SEED, n_perm=study.N_PERM, n_perm_mde=study.N_PERM_MDE)) == []

@pytest.mark.parametrize('kw,needle', [({'n_perm_mde': study.N_PERM_MDE - 1}, '--n-perm-mde'), ({'n_perm': 200}, '--n-perm '), ({'seed': study.SEED + 1}, '--seed'), ({'no_mirror_check': True}, '--no-mirror-check')])
def test_every_inference_deviation_is_named(kw, needle):
    found = study.inference_deviations(_Args(**kw))
    assert len(found) == 1
    assert needle in found[0]

def test_render_only_refuses_an_unstamped_payload_on_the_committed_report(tmp_path):
    payload = _render_payload()
    payload['decision'] = study.decision_payload(payload['decision'])
    payload['run_summary']['scope'] = {}
    path = tmp_path / 'unstamped.json'
    path.write_text(json.dumps(payload))
    with pytest.raises(SystemExit) as exc:
        study.main(['--render-only', '--json-out', str(path), '--write-report'])
    assert 'not stamped as a complete run' in str(exc.value)

def test_render_only_refuses_a_payload_not_stamped_pre_registered(tmp_path):
    payload = _render_payload()
    payload['decision'] = study.decision_payload(payload['decision'])
    payload['run_summary']['scope'] = {'complete': True}
    path = tmp_path / 'deviating.json'
    path.write_text(json.dumps(payload))
    with pytest.raises(SystemExit) as exc:
        study.main(['--render-only', '--json-out', str(path), '--write-report'])
    assert 'pre-registered inference' in str(exc.value)

def test_render_only_to_the_committed_report_needs_write_report(tmp_path):
    payload = _render_payload()
    payload['decision'] = study.decision_payload(payload['decision'])
    path = tmp_path / 'complete.json'
    path.write_text(json.dumps(payload))
    with pytest.raises(SystemExit) as exc:
        study.main(['--render-only', '--json-out', str(path)])
    assert 'needs --write-report' in str(exc.value)

def test_render_only_writes_a_non_committed_path_freely(tmp_path):
    payload = _render_payload()
    payload['decision'] = study.decision_payload(payload['decision'])
    path = tmp_path / 'p.json'
    path.write_text(json.dumps(payload))
    out = tmp_path / 'r.md'
    assert study.main(['--render-only', '--json-out', str(path), '--report-out', str(out)]) == 0
    assert out.exists()

def test_fetch_only_may_be_scoped_to_the_venues_that_need_it(monkeypatch):
    seen = {}

    def _fake(datasets):
        seen['datasets'] = list(datasets)
        return {}
    monkeypatch.setattr(study, 'ensure_min_history', _fake)
    assert study.main(['--fetch-only', '--datasets', 'bitstamp=BTC/USD:1h']) == 0
    assert seen['datasets'] == [('bitstamp', 'BTC/USD', '1h')]

def test_the_registry_row_records_the_deferral_and_the_two_sidedness():
    path = os.path.join(os.path.dirname(study.__file__), '..', '..', 'docs', 'backtesting-registry.md')
    with open(os.path.abspath(path)) as fh:
        rows = [ln for ln in fh if 'hurst_1426_two_sided_sort.py' in ln]
    assert len(rows) == 1
    row = rows[0]
    assert 'TWO-SIDED' in row
    assert 'DEFERS' in row and 'hurst_gate_calibration.md' in row
    assert '#1427' in row and '#1428' in row

def test_the_estimator_is_the_1409_ssot_and_is_never_reimplemented():
    assert study.rolling_hurst is study1410.rolling_hurst
    source = open(study.__file__).read()
    assert 'def hurst_exponent' not in source

def test_look_ahead_shifts_are_the_inherited_ones():
    series = pd.Series(np.arange(10.0))
    assert study.decision_series(series).equals(series.shift(1))
    assert study.entry_stamp_series(series).equals(series.shift(2))

def test_nan_is_its_own_bucket_for_both_h_and_adx():
    assert study.bucket_label(None) == study.BUCKET_NAN
    assert study.bucket_label(float('nan')) == study.BUCKET_NAN
    assert study.joint_h_bucket(None) == study.BUCKET_NAN
    assert study.joint_adx_bucket(None) == study.BUCKET_NAN

def test_the_design_is_inherited_from_1424_rather_than_restated():
    assert study.WINDOWS == study1424.WINDOWS
    assert study.WINDOW_ORDER == study1424.WINDOW_ORDER
    assert study.DATASETS == study1424.DATASETS
    assert study.DATASET_WINDOWS == study1424.DATASET_WINDOWS
    assert study.WINDOW_OWNER == study1424.WINDOW_OWNER
    assert study.PRIMARY_CONFIG_ID == study1424.PRIMARY_CONFIG_ID
    assert study.PRIMARY_FAMILY == study1424.PRIMARY_FAMILY
    assert study.PRIMARY_TARGET == study1424.PRIMARY_TARGET
    assert study.HORIZON_HOURS == study1424.HORIZON_HOURS

def test_the_acceptance_rule_and_the_targets_are_1424s_objects():
    assert study.config_verdict is study1424.config_verdict
    assert study.signed_efficiency is study1424.signed_efficiency
    assert study.build_leg is study1424.build_leg
    assert study.cell_cohort is study1424.cell_cohort
    assert study.bucket_tables is study1424.bucket_tables

def test_the_pinned_hypothesis_is_still_the_committed_1410_argmin():
    assert study.resolve_primary_config_id(study._JSON_1410) == study.PRIMARY_CONFIG_ID

def test_the_seed_is_the_issue_number():
    assert study.SEED == study.ISSUE == 1426

def _committed():
    with open(study._DEFAULT_JSON_OUT) as fh:
        return json.load(fh)

def test_the_committed_decision_is_what_the_current_rule_produces():
    payload = _committed()
    fresh = study.decision_payload(study.decide_recommendation(payload['configs'], payload['mde']))
    assert payload['decision'] == fresh

def test_the_committed_report_is_what_the_committed_json_renders():
    payload = _committed()
    with open(study._DEFAULT_REPORT_OUT) as fh:
        assert study.report_from_payload(payload) == fh.read()

def test_the_committed_run_is_complete_and_pre_registered():
    scope = _committed()['run_summary']['scope']
    assert scope['complete'] is True
    assert scope['pre_registered_inference'] is True

def test_the_committed_run_declares_option_2_and_defers_the_contract_path():
    pre = _committed()['pre_registered']
    assert pre['cohort_option'] == 'exploratory_only_full_pool'
    assert pre['contract_path_claimed'] is False
    assert pre['sibling_deferral'] == [1427, 1428]
    assert pre['two_sided'] is True
    assert pre['interim_look_disclosure'] == study.INTERIM_LOOK_DISCLOSURE

def test_the_committed_run_recommends_nothing():
    decision = _committed()['decision']
    assert all((v['winner'] is None for v in decision['families'].values()))
    assert study.EXPLORATORY_ONLY_SENTENCE in decision['justification']

def test_the_committed_gate_is_two_sided_and_row_matched():
    payload = _committed()
    gate = payload['decision']['validity_gate']
    assert gate['family'] == study.PRIMARY_FAMILY
    assert gate['two_sided'] is True
    assert gate['n_rows'] == payload['mde']['by_family_n'][study.PRIMARY_FAMILY]
    assert gate['limit'] == payload['mde']['by_family_cluster'][study.PRIMARY_FAMILY]
    assert gate['largest_separation'] == pytest.approx(payload['mde']['by_family_separation'][study.PRIMARY_FAMILY])

def test_the_committed_limit_is_not_below_1424s_one_sided_limit():
    payload = _committed()
    mine = payload['mde']['by_family_cluster'][study.PRIMARY_FAMILY]
    with open(study1424._DEFAULT_JSON_OUT) as fh:
        theirs = json.load(fh)['mde']['by_family_cluster'][study.PRIMARY_FAMILY]
    if mine is None or theirs is None:
        pytest.skip('one of the two limits is unresolvable on its grid')
    assert mine >= theirs

def test_the_committed_report_never_licenses_a_threshold():
    with open(study._DEFAULT_REPORT_OUT) as fh:
        text = fh.read()
    assert 'DEFAULT-OFF' in text
    assert 'No configuration is recommended, and none could be.' in text
    assert study.CONTRACT_PATH_STATEMENT in text
