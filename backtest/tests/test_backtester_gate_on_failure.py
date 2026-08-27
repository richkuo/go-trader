import json
import sys
import pathlib
import numpy as np
import pandas as pd
import pytest
sys.path.insert(0, str(pathlib.Path(__file__).parent.parent.parent / 'shared_tools'))
sys.path.insert(0, str(pathlib.Path(__file__).parent.parent))
from backtester import Backtester, _regime_allows_entry
import run_backtest

def _df_with_signal(n: int=100, buy_at: int=50, sell_at: int=None) -> pd.DataFrame:
    close = np.linspace(100.0, 200.0, n)
    idx = pd.date_range('2024-01-01', periods=n, freq='D')
    df = pd.DataFrame({'open': close, 'high': close + 0.5, 'low': close - 0.5, 'close': close, 'volume': 1000.0}, index=idx)
    df['signal'] = 0
    df.iloc[buy_at, df.columns.get_loc('signal')] = 1
    if sell_at is not None:
        df.iloc[sell_at, df.columns.get_loc('signal')] = -1
    return df

def _bt(policy=None, allowed=('trending_up',)):
    kwargs = dict(initial_capital=1000, commission_pct=0, slippage_pct=0, regime_enabled=True, allowed_regimes=list(allowed))
    if policy is not None:
        kwargs['regime_gate_on_failure'] = policy
    return Backtester(**kwargs)

def test_all_empty_regime_bars_admit_entry_under_default_open():
    df = _df_with_signal()
    df['regime'] = ''
    result = _bt().run(df, save=False)
    assert result['total_trades'] >= 1, 'default (fail-open) must admit the entry on empty regime bars'

def test_all_empty_regime_bars_block_entry_under_closed():
    df = _df_with_signal()
    df['regime'] = ''
    result = _bt('closed').run(df, save=False)
    assert result['total_trades'] == 0, 'fail-closed must hold entries while the regime label is empty'

def test_mid_series_gap_at_decision_bar_respects_policy():
    df = _df_with_signal(buy_at=50)
    df['regime'] = 'trending_up'
    df.iloc[50, df.columns.get_loc('regime')] = ''
    assert _bt('open').run(df, save=False)['total_trades'] >= 1
    assert _bt('closed').run(df, save=False)['total_trades'] == 0

def test_known_label_decided_by_membership_under_both_policies():
    df = _df_with_signal()
    df['regime'] = 'trending_up'
    assert _bt('closed').run(df, save=False)['total_trades'] >= 1, 'a matching label must admit the entry under fail-closed'
    df['regime'] = 'ranging'
    assert _bt('open').run(df, save=False)['total_trades'] == 0, 'a mismatching label must block under fail-open too'

def test_closes_never_gated_under_closed():
    df = _df_with_signal(buy_at=30, sell_at=60)
    df['regime'] = ''
    df.iloc[30, df.columns.get_loc('regime')] = 'trending_up'
    res_open = _bt('open').run(df, save=False)
    res_closed = _bt('closed').run(df, save=False)
    assert res_closed['total_trades'] == res_open['total_trades'] >= 1
    assert [t['exit_date'] for t in res_closed['trades']] == [t['exit_date'] for t in res_open['trades']]

def test_default_open_is_byte_identical_to_omitted():
    df = _df_with_signal()
    df['regime'] = ''
    res_omitted = _bt().run(df, save=False)
    res_open = _bt('open').run(df, save=False)
    assert res_omitted['total_trades'] == res_open['total_trades']
    assert res_omitted['total_return_pct'] == res_open['total_return_pct']

def test_unknown_policy_rejected_at_construction():
    with pytest.raises(ValueError, match='regime_gate_on_failure'):
        _bt('fail-closed')

def test_regime_allows_entry_empty_label_parity_matrix():
    gate = ['trending_up']
    assert _regime_allows_entry([], '', 'open')
    assert _regime_allows_entry([], '', 'closed')
    assert _regime_allows_entry(gate, '', 'open')
    assert not _regime_allows_entry(gate, '', 'closed')
    assert _regime_allows_entry(gate, 'trending_up', 'closed')
    assert not _regime_allows_entry(gate, 'ranging', 'open')
    assert _regime_allows_entry(['ranging_directional'], 'ranging_directional_up', 'closed')

def _write_config(tmp_path, strategy_extra=None, regime_extra=None):
    strategy = {'id': 'hl-x', 'type': 'perps', 'open_strategy': {'name': 'tema_cross_bd'}, 'allowed_regimes': ['trending_up']}
    strategy.update(strategy_extra or {})
    regime = {'enabled': True, 'period': 14, 'adx_threshold': 20.0}
    regime.update(regime_extra or {})
    cfg = {'config_version': 15, 'strategies': [strategy], 'regime': regime}
    p = tmp_path / 'config.json'
    p.write_text(json.dumps(cfg))
    return str(p)

def test_load_strategy_config_defaults_gate_policy_open(tmp_path):
    path = _write_config(tmp_path)
    kwargs = run_backtest.load_strategy_config(path, 'hl-x')
    assert kwargs['regime_gate_on_failure'] == 'open'

def test_load_strategy_config_reads_strategy_gate_policy(tmp_path):
    path = _write_config(tmp_path, strategy_extra={'regime_gate_on_failure': 'closed'})
    kwargs = run_backtest.load_strategy_config(path, 'hl-x')
    assert kwargs['regime_gate_on_failure'] == 'closed'

def test_load_strategy_config_strategy_overrides_global_gate_policy(tmp_path):
    path = _write_config(tmp_path, strategy_extra={'regime_gate_on_failure': 'open'}, regime_extra={'gate_on_failure': 'closed'})
    kwargs = run_backtest.load_strategy_config(path, 'hl-x')
    assert kwargs['regime_gate_on_failure'] == 'open'

def test_load_strategy_config_global_gate_policy_applies(tmp_path):
    path = _write_config(tmp_path, regime_extra={'gate_on_failure': 'closed'})
    kwargs = run_backtest.load_strategy_config(path, 'hl-x')
    assert kwargs['regime_gate_on_failure'] == 'closed'

def test_load_strategy_config_rejects_unknown_gate_policy(tmp_path):
    path = _write_config(tmp_path, strategy_extra={'regime_gate_on_failure': 'fail-closed'})
    with pytest.raises(ValueError, match='regime_gate_on_failure'):
        run_backtest.load_strategy_config(path, 'hl-x')

def test_load_strategy_config_rejects_unknown_global_gate_policy(tmp_path):
    path = _write_config(tmp_path, regime_extra={'gate_on_failure': 'garbage'})
    with pytest.raises(ValueError, match='regime.gate_on_failure'):
        run_backtest.load_strategy_config(path, 'hl-x')

def test_load_strategy_config_rejects_unknown_global_gate_policy_even_with_valid_strategy_override(tmp_path):
    path = _write_config(tmp_path, strategy_extra={'regime_gate_on_failure': 'closed'}, regime_extra={'gate_on_failure': 'garbage'})
    with pytest.raises(ValueError, match='regime.gate_on_failure'):
        run_backtest.load_strategy_config(path, 'hl-x')

def test_backtester_accepts_load_strategy_config_kwargs(tmp_path):
    path = _write_config(tmp_path, strategy_extra={'regime_gate_on_failure': 'closed'})
    kwargs = run_backtest.load_strategy_config(path, 'hl-x')
    open_name = kwargs.pop('open_strategy')['name']
    close_refs = kwargs.pop('close_strategies')
    bt = Backtester(initial_capital=1000, open_strategy={'name': open_name, 'params': {}}, close_strategies=close_refs, **{k: v for k, v in kwargs.items() if k in ('regime_enabled', 'regime_period', 'regime_adx_threshold', 'allowed_regimes', 'regime_gate_on_failure')})
    assert bt.regime_gate_on_failure == 'closed'
