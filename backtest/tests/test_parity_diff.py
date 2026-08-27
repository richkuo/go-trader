import os
import sys
import numpy as np
import pandas as pd
import pytest
sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..'))
import parity_diff
from parity_diff import LIVE_MIN_CANDLES, ParityConfig, compute_parity_frame, config_from_live_config, extract_fills, main, summarize
from registry_loader import load_registry

def _ohlcv(n: int=300, seed: int=7) -> pd.DataFrame:
    rng = np.random.default_rng(seed)
    drift = np.linspace(0, 12, n)
    wave = 4.0 * np.sin(np.linspace(0, 14, n))
    noise = rng.normal(0, 0.4, n)
    close = 100.0 + drift + wave + noise
    df = pd.DataFrame({'open': close + rng.normal(0, 0.1, n), 'high': close + np.abs(rng.normal(0, 0.5, n)) + 0.2, 'low': close - np.abs(rng.normal(0, 0.5, n)) - 0.2, 'close': close, 'volume': rng.uniform(900, 1100, n)}, index=pd.date_range('2024-01-01', periods=n, freq='1h'))
    return df

def test_window_invariant_strategy_diffs_clean():
    df = _ohlcv(260)
    frame = compute_parity_frame(df, 'sma_crossover', params={'fast_period': 10, 'slow_period': 30}, window=120)
    result = summarize(frame)
    assert result['bars_compared'] > 100
    assert result['clean'], f"window-invariant strategy should not diff: {frame[~frame['match']].head()}"

def test_frame_dependent_strategy_is_caught():
    reg = load_registry('spot')

    def full_frame_mean_strategy(df: pd.DataFrame) -> pd.DataFrame:
        out = df.copy()
        out['signal'] = (out['close'] > out['close'].mean()).astype(int)
        return out
    name = '_parity_diff_test_frame_dependent'
    reg.STRATEGY_REGISTRY[name] = {'fn': full_frame_mean_strategy, 'description': 'test-only frame-dependent strategy', 'default_params': {}}
    try:
        df = _ohlcv(260)
        frame = compute_parity_frame(df, name, window=60)
        result = summarize(frame)
        assert result['mismatches'] > 0, 'frame-dependent strategy must be detected by the parity diff'
        assert 'first_mismatch' in result
    finally:
        del reg.STRATEGY_REGISTRY[name]

def test_regime_labels_diff_clean_per_bar():
    df = _ohlcv(220)
    frame = compute_parity_frame(df, 'sma_crossover', params={'fast_period': 10, 'slow_period': 30}, window=120, regime_enabled=True)
    assert 'bt_regime' in frame.columns and 'live_regime' in frame.columns
    regime_mismatch = frame[frame['bt_regime'] != frame['live_regime']]
    assert regime_mismatch.empty, regime_mismatch.head()

def test_expanding_window_mode():
    df = _ohlcv(120)
    frame = compute_parity_frame(df, 'sma_crossover', params={'fast_period': 5, 'slow_period': 15}, window=None)
    assert len(frame) == len(df) - (LIVE_MIN_CANDLES - 1)
    assert summarize(frame)['clean']

def test_stride_thins_comparison():
    df = _ohlcv(200)
    full = compute_parity_frame(df, 'sma_crossover', params={'fast_period': 5, 'slow_period': 15}, window=60)
    thinned = compute_parity_frame(df, 'sma_crossover', params={'fast_period': 5, 'slow_period': 15}, window=60, stride=5)
    assert len(thinned) == (len(full) + 4) // 5

def test_window_below_live_minimum_rejected():
    df = _ohlcv(100)
    with pytest.raises(ValueError, match='window must be >='):
        compute_parity_frame(df, 'sma_crossover', window=10)

def _trending_ohlcv(n: int=260, seed: int=11) -> pd.DataFrame:
    rng = np.random.default_rng(seed)
    close = 100.0 + np.linspace(0, 80, n) + rng.normal(0, 0.5, n)
    return pd.DataFrame({'open': close + rng.normal(0, 0.1, n), 'high': close + np.abs(rng.normal(0, 0.6, n)) + 0.3, 'low': close - np.abs(rng.normal(0, 0.6, n)) - 0.3, 'close': close, 'volume': rng.uniform(900, 1100, n)}, index=pd.date_range('2024-01-01', periods=n, freq='1h'))

def test_close_evaluator_parity_clean_and_exercised():
    df = _trending_ohlcv(260)
    frame = compute_parity_frame(df, 'sma_crossover', params={'fast_period': 10, 'slow_period': 30}, window=60, close_refs=[{'name': 'tiered_tp_atr', 'params': {}}])
    result = summarize(frame)
    assert result['bars_compared'] > 100
    assert result['clean'], frame[~frame['match']].head()
    assert (frame['live_close_fraction'] > 0).any(), 'tiered_tp_atr never fired — the close-evaluator path is untested'
    assert (frame['bt_close_fraction'] > 0).any()

def test_composed_signal_with_close_refs_diffs_clean():
    df = _trending_ohlcv(220)
    frame = compute_parity_frame(df, 'sma_crossover', params={'fast_period': 5, 'slow_period': 20}, window=60, close_refs=[{'name': 'tiered_tp_atr', 'params': {}}])
    assert (frame['bt_signal'] == frame['live_signal']).all(), frame[frame['bt_signal'] != frame['live_signal']].head()

def test_regime_directional_policy_decision_layer_parity(monkeypatch):
    import regime as regime_mod
    df = _ohlcv(140)
    df['_test_regime'] = 'trending_down'

    def fake_compute_regime(frame, period=14, adx_threshold=20.0):
        out = frame.copy()
        out['regime'] = frame['_test_regime'].values
        out['regime_score'] = 1.0
        out['adx'] = 50.0
        out['plus_di'] = 10.0
        out['minus_di'] = 40.0
        return out

    def fake_prepare_check_regime(window, **kwargs):
        label = str(window['_test_regime'].iloc[-1])
        snap = {'regime': label, 'score': 1.0, 'metrics': {}}
        return (label, label, snap)
    monkeypatch.setattr(regime_mod, 'compute_regime', fake_compute_regime)
    monkeypatch.setattr(parity_diff, 'prepare_check_regime', fake_prepare_check_regime)
    reg = load_registry('futures')
    name = '_parity_regime_directional_always_buy'

    def always_buy(frame: pd.DataFrame) -> pd.DataFrame:
        out = frame.copy()
        out['signal'] = 1
        return out
    reg.STRATEGY_REGISTRY[name] = {'fn': always_buy, 'description': 'test-only always buy', 'default_params': {}}
    try:
        cfg = ParityConfig(strategy_name=name, registry='futures', close_refs=[{'name': 'tiered_tp_pct', 'params': {'tp_tiers': [{'profit_pct': 0.9, 'close_fraction': 1.0}]}}], regime_enabled=True, direction='long', invert_signal=False, regime_directional_policy={'trend_regime': {'trending_up': {'direction': 'long', 'invert_signal': False}, 'trending_down': {'direction': 'short', 'invert_signal': True}, 'ranging': {'direction': 'long', 'invert_signal': False}}})
        frame = compute_parity_frame(df, cfg=cfg, window=60)
        assert summarize(frame)['clean'], frame[~frame['match']].head()
        assert (frame['bt_open_action'] == 'short').any()
        assert (frame['live_open_action'] == 'short').any()
    finally:
        del reg.STRATEGY_REGISTRY[name]

def test_frame_dependent_strategy_caught_with_close_refs_too():
    reg = load_registry('spot')

    def full_frame_mean_strategy(df: pd.DataFrame) -> pd.DataFrame:
        out = df.copy()
        out['signal'] = (out['close'] > out['close'].mean()).astype(int)
        return out
    name = '_parity_diff_test_frame_dependent_close'
    reg.STRATEGY_REGISTRY[name] = {'fn': full_frame_mean_strategy, 'description': 'test-only frame-dependent strategy', 'default_params': {}}
    try:
        df = _ohlcv(260)
        frame = compute_parity_frame(df, name, window=60, close_refs=[{'name': 'tiered_tp_atr', 'params': {}}])
        assert summarize(frame)['mismatches'] > 0
    finally:
        del reg.STRATEGY_REGISTRY[name]

def test_config_mode_builds_parity_config(tmp_path):
    import json as _json
    cfg_path = tmp_path / 'config.json'
    cfg_path.write_text(_json.dumps({'config_version': 15, 'regime': {'enabled': True, 'period': 10, 'adx_threshold': 25}, 'strategies': [{'id': 'hl-sma-btc', 'type': 'perps', 'script': 'shared_scripts/check_hyperliquid.py', 'args': ['sma_crossover', 'BTC/USDT', '4h'], 'open_strategy': {'name': 'sma_crossover', 'params': {'fast_period': 5, 'slow_period': 20}}, 'close_strategy': {'name': 'tiered_tp_atr', 'params': {}}}]}))
    cfg = config_from_live_config(str(cfg_path), 'hl-sma-btc')
    assert cfg.strategy_name == 'sma_crossover'
    assert cfg.params == {'fast_period': 5, 'slow_period': 20}
    assert cfg.registry == 'futures'
    assert cfg.platform == 'hyperliquid'
    assert cfg.symbol == 'BTC/USDT' and cfg.timeframe == '4h'
    assert cfg.close_refs == [{'name': 'tiered_tp_atr', 'params': {}}]
    assert cfg.regime_enabled and cfg.regime_period == 10
    assert cfg.regime_adx_threshold == 25.0
    frame = compute_parity_frame(_trending_ohlcv(200), cfg=cfg, window=60)
    assert summarize(frame)['bars_compared'] > 50

def test_config_mode_unknown_strategy_id_raises(tmp_path):
    import json as _json
    cfg_path = tmp_path / 'config.json'
    cfg_path.write_text(_json.dumps({'config_version': 15, 'strategies': []}))
    with pytest.raises(ValueError):
        config_from_live_config(str(cfg_path), 'missing-id')

def test_extract_fills_reports_entry_and_exit_legs():
    df = _trending_ohlcv(220)
    cfg = ParityConfig(strategy_name='sma_crossover', params={'fast_period': 5, 'slow_period': 20})
    fills = extract_fills(df, cfg)
    assert fills, 'trending data + sma crossover must produce fills'
    entries = [f for f in fills if f['event'] == 'entry']
    exits = [f for f in fills if f['event'] == 'exit']
    assert entries and all((f['fill_px'] > 0 and f['fee'] >= 0 for f in entries))
    assert all(('pnl' in f for f in exits))

def test_backtest_effective_columns_are_prior_bar_inputs():
    df = _ohlcv(200)
    frame = compute_parity_frame(df, 'sma_crossover', params={'fast_period': 5, 'slow_period': 15}, window=60)
    assert 'backtest_effective_signal' in frame.columns
    got = frame['backtest_effective_signal'].iloc[1:].tolist()
    want = frame['bt_signal'].iloc[:-1].tolist()
    assert got == want

def _live_config_json(stype: str='perps') -> dict:
    return {'config_version': 15, 'strategies': [{'id': 'test-strat', 'type': stype, 'script': 'shared_scripts/check_hyperliquid.py', 'args': ['sma_crossover', 'BTC/USDT', '1h'], 'open_strategy': {'name': 'sma_crossover', 'params': {'fast_period': 5, 'slow_period': 20}}}]}

@pytest.mark.parametrize('stype,want_platform', [('perps', 'hyperliquid'), ('manual', 'hyperliquid'), ('futures', 'binanceus'), ('spot', 'binanceus')])
def test_config_mode_platform_autodetect(tmp_path, stype, want_platform):
    import json as _json
    cfg_path = tmp_path / 'config.json'
    cfg_path.write_text(_json.dumps(_live_config_json(stype)))
    cfg = config_from_live_config(str(cfg_path), 'test-strat', platform='')
    assert cfg.platform == want_platform

def test_config_mode_explicit_platform_overrides_autodetect(tmp_path):
    import json as _json
    cfg_path = tmp_path / 'config.json'
    cfg_path.write_text(_json.dumps(_live_config_json('spot')))
    cfg = config_from_live_config(str(cfg_path), 'test-strat', platform='hyperliquid')
    assert cfg.platform == 'hyperliquid'

def test_main_config_mode_fills_use_autodetected_platform(tmp_path, monkeypatch):
    import json as _json
    import data_fetcher
    cfg_path = tmp_path / 'config.json'
    cfg_path.write_text(_json.dumps(_live_config_json('perps')))
    monkeypatch.setattr(data_fetcher, 'load_cached_data', lambda *a, **k: _trending_ohlcv(200))
    captured = {}

    def spy_extract_fills(df, cfg):
        captured['platform'] = cfg.platform
        return []
    monkeypatch.setattr(parity_diff, 'extract_fills', spy_extract_fills)
    rc = main(['--config', str(cfg_path), '--strategy-id', 'test-strat', '--fills', '--window', '60'])
    assert rc in (0, 1)
    assert captured['platform'] == 'hyperliquid'

def test_main_zero_bars_compared_is_data_error(monkeypatch):
    import data_fetcher
    monkeypatch.setattr(data_fetcher, 'load_cached_data', lambda *a, **k: _ohlcv(40))
    rc = main(['--strategy', 'sma_crossover', '--window', '200'])
    assert rc == 2

def test_out_of_contract_signal_rejected_loudly():
    reg = load_registry('spot')

    def fractional_signal_strategy(df: pd.DataFrame) -> pd.DataFrame:
        out = df.copy()
        sma = out['close'].rolling(10).mean()
        out['signal'] = np.where(out['close'] > sma, 0.5, -0.5)
        return out
    name = '_parity_diff_test_fractional_signal'
    reg.STRATEGY_REGISTRY[name] = {'fn': fractional_signal_strategy, 'description': 'test-only fractional-signal strategy', 'default_params': {}}
    try:
        df = _ohlcv(200)
        with pytest.raises(ValueError, match='signal must be in'):
            compute_parity_frame(df, name, window=60)
    finally:
        del reg.STRATEGY_REGISTRY[name]

def test_normalize_signal_contract():
    assert parity_diff._normalize_signal(np.float64(1.0)) == 1
    assert parity_diff._normalize_signal(-1) == -1
    assert parity_diff._normalize_signal(0.0) == 0
    assert parity_diff._normalize_signal(float('nan')) == 0
    assert parity_diff._normalize_signal(None) == 0
    for bad in (0.5, -0.5, 2, -3):
        with pytest.raises(ValueError, match='signal must be in'):
            parity_diff._normalize_signal(bad)

def test_position_context_entry_regime_is_decision_bar_label():
    from atr import ensure_atr_indicator
    n = 40
    df = _ohlcv(n)
    bt = pd.DataFrame({'signal': [0] * n, 'open_action': ['none'] * n, 'close_fraction': [0.0] * n}, index=df.index)
    bt.iloc[9, bt.columns.get_loc('open_action')] = 'long'
    atr_full = ensure_atr_indicator(df.copy())['atr']
    regime_full = pd.Series([f'label{i}' for i in range(n)], index=df.index)
    contexts, _ = parity_diff._simulate_position_contexts(bt, df, atr_full, regime_full)
    assert contexts[9] is None
    assert contexts[10] is not None
    assert contexts[10]['regime'] == 'label9', "entry regime must be the decision bar's (9) label, not the fill bar's (10)"

def test_bt_close_evaluator_uses_engine_dict_shape(monkeypatch):
    from atr import ensure_atr_indicator
    captured = {}

    def fake_evaluate(name, position, market, params):
        captured['position'] = position
        captured['market'] = market
        return {'close_fraction': 0.0}
    monkeypatch.setattr(parity_diff, 'close_evaluate', fake_evaluate)
    df = _trending_ohlcv(80)
    atr_full = ensure_atr_indicator(df.copy())['atr']
    cfg = ParityConfig(strategy_name='sma_crossover', close_refs=[{'name': 'tiered_tp_atr', 'params': {}}])
    ctx = {'side': 'long', 'avg_cost': 100.0, 'current_quantity': 1.0, 'initial_quantity': 1.0}
    parity_diff._bt_close_evaluator_fraction(cfg, 60, df, atr_full, None, ctx)
    assert captured['market']['regime'] == ''
    assert captured['position']['regime'] == ''
    assert isinstance(captured['position']['entry_atr'], float)
    assert captured['position']['entry_atr'] == 0.0

def test_non_registry_close_ref_rejected_like_engine():
    df = _trending_ohlcv(120)
    with pytest.raises(ValueError, match='Unknown close strategy'):
        compute_parity_frame(df, 'sma_crossover', params={'fast_period': 5, 'slow_period': 20}, window=60, close_refs=[{'name': 'rsi_oversold', 'params': {}}])
    from backtester import Backtester
    with pytest.raises(ValueError, match='Unknown close strategy'):
        Backtester(initial_capital=1000.0, open_strategy={'name': 'sma_crossover', 'params': {}}, close_strategies=[{'name': 'rsi_oversold', 'params': {}}])

def test_entry_atr_plausibility_guard_matches_engine():
    from atr import ensure_atr_indicator
    n = 40
    rng = np.random.default_rng(3)
    close = 100.0 + rng.normal(0, 0.2, n)
    df = pd.DataFrame({'open': close, 'high': close + 90.0, 'low': close - 90.0, 'close': close, 'volume': [1000.0] * n}, index=pd.date_range('2024-01-01', periods=n, freq='1h'))
    bt = pd.DataFrame({'signal': [0] * n, 'open_action': ['none'] * n, 'close_fraction': [0.0] * n}, index=df.index)
    bt.iloc[19, bt.columns.get_loc('open_action')] = 'long'
    atr_full = ensure_atr_indicator(df.copy())['atr']
    assert float(atr_full.iloc[20]) > 0.5 * float(df['close'].iloc[20])
    contexts, _ = parity_diff._simulate_position_contexts(bt, df, atr_full, None)
    assert contexts[20] is not None
    assert 'entry_atr' not in contexts[20]

def test_position_context_avg_cost_is_fill_bar_open_with_slippage():
    from atr import ensure_atr_indicator
    n = 40
    df = _ohlcv(n)
    atr_full = ensure_atr_indicator(df.copy())['atr']
    slip = parity_diff._ENGINE_SLIPPAGE_PCT
    assert slip == pytest.approx(0.0005)
    for action, sign in (('long', 1), ('short', -1)):
        bt = pd.DataFrame({'signal': [0] * n, 'open_action': ['none'] * n, 'close_fraction': [0.0] * n}, index=df.index)
        bt.iloc[19, bt.columns.get_loc('open_action')] = action
        contexts, _ = parity_diff._simulate_position_contexts(bt, df, atr_full, None)
        expected = float(df['open'].iloc[20]) * (1 + sign * slip)
        assert contexts[20]['avg_cost'] == pytest.approx(expected), action
        assert contexts[20]['avg_cost'] != pytest.approx(float(df['close'].iloc[20]))

def test_registry_close_advances_quantity_ladder():
    from atr import ensure_atr_indicator
    n = 60
    close = np.array([100.0] * 30 + [104.0] * 10 + [108.0] * 20)
    df = pd.DataFrame({'open': close, 'high': close + 1.0, 'low': close - 1.0, 'close': close, 'volume': [1000.0] * n}, index=pd.date_range('2024-01-01', periods=n, freq='1h'))
    bt = pd.DataFrame({'signal': [0] * n, 'open_action': ['none'] * n, 'close_fraction': [0.0] * n}, index=df.index)
    bt.iloc[19, bt.columns.get_loc('open_action')] = 'long'
    atr_full = ensure_atr_indicator(df.copy())['atr']
    cfg = ParityConfig(strategy_name='sma_crossover', close_refs=[{'name': 'tiered_tp_atr', 'params': {}}])
    contexts, fracs = parity_diff._simulate_position_contexts(bt, df, atr_full, None, cfg)
    assert fracs[30] == pytest.approx(0.4)
    assert contexts[31]['current_quantity'] == pytest.approx(0.6)
    assert fracs[31] == pytest.approx(0.0)
    assert fracs[40] == pytest.approx(2.0 / 3.0)
    assert contexts[41]['current_quantity'] == pytest.approx(0.2)
    assert fracs[41] == pytest.approx(0.0)

def test_config_mode_injects_user_close_defaults(tmp_path):
    import json as _json
    cfg_path = tmp_path / 'config.json'
    ladder = [{'atr_multiple': 1.0, 'close_fraction': 0.5}, {'atr_multiple': 2.0, 'close_fraction': 1.0}]
    cfg_path.write_text(_json.dumps({'config_version': 16, 'user_defaults': {'close': {'tiered_tp_atr': {'tp_tiers': ladder}}}, 'strategies': [{'id': 'hl-sma-btc', 'type': 'perps', 'script': 'shared_scripts/check_hyperliquid.py', 'args': ['sma_crossover', 'BTC/USDT', '4h'], 'open_strategy': {'name': 'sma_crossover', 'params': {}}, 'close_strategy': {'name': 'tiered_tp_atr', 'params': {'use_defaults': True}}}]}))
    cfg = config_from_live_config(str(cfg_path), 'hl-sma-btc')
    assert cfg.close_refs[0]['params'].get('tp_tiers') == ladder

def _batched_cfg(**overrides) -> ParityConfig:
    cfg = ParityConfig(strategy_name='breakout', registry='futures', platform='hyperliquid', symbol='BTC', timeframe='1h', batched=True)
    for key, value in overrides.items():
        setattr(cfg, key, value)
    return cfg

def test_batched_dimension_reports_zero_diff():
    frame = compute_parity_frame(_ohlcv(140), cfg=_batched_cfg(), window=60, stride=5)
    result = summarize(frame)
    assert result['bars_compared'] > 0
    assert result['batch_mismatches'] == 0, frame[frame['solo_signal'] != frame['batch_signal']].head()
    assert result['batch_clean']
    for column in ('solo_signal', 'batch_signal', 'solo_open_action', 'batch_open_action', 'solo_close_fraction', 'batch_close_fraction'):
        assert column in frame.columns

def test_batched_dimension_zero_diff_with_close_refs_and_regime():
    cfg = _batched_cfg(close_refs=[{'name': 'tiered_tp_pct', 'params': {'tp_tiers': [{'profit_pct': 0.9, 'close_fraction': 1.0}]}}], regime_enabled=True)
    frame = compute_parity_frame(_ohlcv(140), cfg=cfg, window=60, stride=5)
    result = summarize(frame)
    assert result['bars_compared'] > 0
    assert result['batch_mismatches'] == 0, frame[~frame['match']].head()

def test_batched_slot_reaches_the_close_evaluators_and_params():
    from parity_diff import _hl_batch_slot, _load_hl_batch_module
    cfg = _batched_cfg(params={'lookback': 11}, close_refs=[{'name': 'tiered_tp_pct', 'params': {'tp_tiers': [{'profit_pct': 0.5, 'close_fraction': 1.0}]}}])
    mod = _load_hl_batch_module()
    slot = _hl_batch_slot(mod, cfg, 'batched', 'long', {'side': 'long'})
    assert slot['open_strategy'] == 'breakout'
    assert slot['close_strategies'] == 'tiered_tp_pct'
    assert slot['params'] == {'lookback': 11}
    assert slot['close_params_by_name']['tiered_tp_pct']['tp_tiers']

def test_batched_close_fraction_can_be_non_zero():
    from parity_diff import _batched_bar_decisions
    df = _ohlcv(140)
    window = df.iloc[:80]
    mark = float(window['close'].iloc[-1])
    cfg = _batched_cfg(close_refs=[{'name': 'tiered_tp_pct', 'params': {'tp_tiers': [{'profit_pct': 0.02, 'close_fraction': 1.0}]}}])
    ctx = {'side': 'long', 'avg_cost': mark * 0.9, 'current_quantity': 1.0, 'initial_quantity': 1.0, 'entry_atr': 1.0}
    solo, batched = _batched_bar_decisions(window, cfg, position_side='long', position_ctx=ctx)
    assert batched['close_fraction'] > 0.0
    assert solo['close_fraction'] == batched['close_fraction']

def test_batched_dimension_is_off_by_default():
    frame = compute_parity_frame(_ohlcv(140), cfg=_batched_cfg(batched=False), window=60, stride=5)
    for column in ('solo_signal', 'batch_signal', 'batch_close_fraction'):
        assert column not in frame.columns
    result = summarize(frame)
    assert 'batch_mismatches' not in result
    assert 'batch_clean' not in result

def test_batched_dimension_catches_a_divergent_slot(monkeypatch):
    cfg = _batched_cfg()
    real = parity_diff._batched_bar_decisions

    def diverging(window, config, position_side='', position_ctx=None):
        solo, batched = real(window, config, position_side, position_ctx)
        batched = dict(batched)
        batched['signal'] = solo['signal'] + 1
        return (solo, batched)
    monkeypatch.setattr(parity_diff, '_batched_bar_decisions', diverging)
    frame = compute_parity_frame(_ohlcv(140), cfg=cfg, window=60, stride=5)
    result = summarize(frame)
    assert result['batch_mismatches'] == result['bars_compared']
    assert not result['clean']
