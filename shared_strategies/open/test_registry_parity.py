import importlib.util
import os
import pytest
_HERE = os.path.dirname(os.path.abspath(__file__))
_SHARED_DIR = _HERE
_SPOT_DIR = os.path.join(_HERE, 'spot')

def _load(name: str, path: str):
    spec = importlib.util.spec_from_file_location(name, path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod

@pytest.fixture(scope='module')
def registry():
    return _load('_registry_under_test', os.path.join(_SHARED_DIR, 'registry.py'))

@pytest.fixture(scope='module')
def spot_shim():
    return _load('_spot_shim_under_test', os.path.join(_SPOT_DIR, 'strategies.py'))

@pytest.fixture(scope='module')
def futures_shim():
    return _load('_futures_shim_under_test', os.path.join(_HERE, 'futures', 'strategies.py'))

@pytest.fixture(scope='module')
def conftest_helpers():
    return _load('_conftest_helpers_parity', os.path.join(_HERE, 'conftest.py'))

def test_platforms_non_empty_and_valid(registry):
    valid = set(registry.VALID_PLATFORMS)
    for name, entry in registry.STRATEGIES.items():
        platforms = entry['platforms']
        assert platforms, f'{name}: platforms tuple is empty'
        bad = set(platforms) - valid
        assert not bad, f'{name}: unknown platforms {sorted(bad)}'

def test_variants_subset_of_platforms(registry):
    for name, entry in registry.STRATEGIES.items():
        bad = set(entry['variants']) - set(entry['platforms'])
        assert not bad, f"{name}: variants reference platforms {sorted(bad)} outside its platforms tuple {entry['platforms']}"

def test_no_duplicate_registration(registry):
    with pytest.raises(ValueError, match='already registered'):

        @registry.register('sma_crossover', 'dup', {})
        def _():
            return None

def test_platform_order_matches_platform_tags(registry):
    for platform in registry.VALID_PLATFORMS:
        tagged = {n for n, e in registry.STRATEGIES.items() if platform in e['platforms']}
        order = set(registry.PLATFORM_ORDER[platform])
        assert tagged == order, f'PLATFORM_ORDER[{platform!r}] mismatch: tagged={sorted(tagged)}, order={sorted(order)}'

def test_hidden_strategies_stay_registered_but_leave_discovery(registry):
    for platform in registry.VALID_PLATFORMS:
        visible = registry.build_registry(platform)
        full = registry.build_registry(platform, include_hidden=True)
        for name in registry.DISCOVERY_HIDDEN_STRATEGIES:
            if platform in registry.STRATEGIES[name]['platforms']:
                assert name in full
                assert name not in visible

def test_build_registry_rejects_unknown_platform(registry):
    with pytest.raises(ValueError, match='Unknown platform'):
        registry.build_registry('options')

def _skip_funding(name: str) -> bool:
    return False

def _apply_each(shim, helpers):
    import pandas as pd
    idx = pd.date_range('2024-01-01', periods=200, freq='15min')
    df = helpers.make_ohlcv(helpers.make_trending_up(200), index=idx)
    for name in shim.list_strategies():
        result = shim.apply_strategy(name, df)
        assert 'signal' in result.columns, f"{name}: missing 'signal' column"

def test_spot_shim_applies_every_registered_strategy(spot_shim, conftest_helpers):
    _apply_each(spot_shim, conftest_helpers)

def test_futures_shim_applies_every_registered_strategy(futures_shim, conftest_helpers):
    _apply_each(futures_shim, conftest_helpers)

def test_shims_produce_independent_registries(spot_shim, futures_shim):
    assert spot_shim.STRATEGY_REGISTRY is not futures_shim.STRATEGY_REGISTRY
    assert 'tp_at_pct' not in spot_shim.STRATEGY_REGISTRY
    assert 'tp_at_pct' not in futures_shim.STRATEGY_REGISTRY
    assert 'pairs_spread' in spot_shim.STRATEGY_REGISTRY
    assert 'pairs_spread' not in futures_shim.STRATEGY_REGISTRY
    assert 'breakout' not in spot_shim.STRATEGY_REGISTRY
    assert 'breakout' in futures_shim.STRATEGY_REGISTRY
    assert 'delta_neutral_funding' not in spot_shim.STRATEGY_REGISTRY
    assert 'delta_neutral_funding' in futures_shim.STRATEGY_REGISTRY
    assert 'triple_ema_bidir' not in spot_shim.STRATEGY_REGISTRY
    assert 'triple_ema_bidir' in futures_shim.STRATEGY_REGISTRY

def test_deprecated_range_scalper_hidden_but_loadable(spot_shim, futures_shim, conftest_helpers):
    for shim in (spot_shim, futures_shim):
        assert 'range_scalper' not in shim.list_strategies()
        assert 'range_scalper' in shim.STRATEGY_REGISTRY
        df = conftest_helpers.make_ohlcv(conftest_helpers.make_trending_up(80))
        result = shim.apply_strategy('range_scalper', df)
        assert 'signal' in result.columns

def test_deprecated_session_breakout_hidden_but_loadable(spot_shim, futures_shim, conftest_helpers):
    import pandas as pd
    assert 'session_breakout' not in futures_shim.list_strategies()
    assert 'session_breakout' in futures_shim.STRATEGY_REGISTRY
    assert 'session_breakout' not in spot_shim.list_strategies()
    assert 'session_breakout' not in spot_shim.STRATEGY_REGISTRY
    idx = pd.date_range('2024-01-01', periods=200, freq='15min')
    df = conftest_helpers.make_ohlcv(conftest_helpers.make_trending_up(200), index=idx)
    result = futures_shim.apply_strategy('session_breakout', df)
    assert 'signal' in result.columns

def test_deprecated_vol_momentum_hidden_but_loadable(spot_shim, futures_shim, conftest_helpers):
    for shim in (spot_shim, futures_shim):
        assert 'vol_momentum' not in shim.list_strategies()
        assert 'vol_momentum' in shim.STRATEGY_REGISTRY
        df = conftest_helpers.make_ohlcv(conftest_helpers.make_trending_up(80))
        result = shim.apply_strategy('vol_momentum', df)
        assert 'signal' in result.columns
    assert spot_shim.STRATEGY_REGISTRY['vol_momentum']['default_params']['allow_short'] is False
    assert futures_shim.STRATEGY_REGISTRY['vol_momentum']['default_params']['allow_short'] is True

def test_deprecated_donchian_breakout_hidden_but_loadable(spot_shim, futures_shim, conftest_helpers):
    for shim in (spot_shim, futures_shim):
        assert 'donchian_breakout' not in shim.list_strategies()
        assert 'donchian_breakout' in shim.STRATEGY_REGISTRY
        df = conftest_helpers.make_ohlcv(conftest_helpers.make_trending_up(80))
        result = shim.apply_strategy('donchian_breakout', df)
        assert 'signal' in result.columns

def test_deprecated_amd_ifvg_hidden_but_loadable(spot_shim, futures_shim, conftest_helpers):
    import pandas as pd
    idx = pd.date_range('2024-01-01', periods=200, freq='15min')
    df = conftest_helpers.make_ohlcv(conftest_helpers.make_trending_up(200), index=idx)
    for shim in (spot_shim, futures_shim):
        assert 'amd_ifvg' not in shim.list_strategies()
        assert 'amd_ifvg' in shim.STRATEGY_REGISTRY
        result = shim.apply_strategy('amd_ifvg', df)
        assert 'signal' in result.columns
    p = spot_shim.STRATEGY_REGISTRY['amd_ifvg']['default_params']
    assert p['session_tz'] == 'America/New_York'
    assert (p['asian_start_hour'], p['asian_end_hour']) == (20, 0)
    assert (p['london_start_hour'], p['london_end_hour']) == (2, 5)

def test_momentum_variant_overrides_threshold(spot_shim, futures_shim):
    assert spot_shim.STRATEGY_REGISTRY['momentum']['default_params']['threshold'] == 5.0
    assert futures_shim.STRATEGY_REGISTRY['momentum']['default_params']['threshold'] == 3.0

def test_variant_descriptions_land_on_the_right_platform(spot_shim, futures_shim):
    assert 'buy at oversold' in spot_shim.STRATEGY_REGISTRY['rsi']['description']
    assert 'for futures' in futures_shim.STRATEGY_REGISTRY['rsi']['description']

def test_backtest_only_analog_retrieval_hidden_but_loadable(spot_shim, futures_shim, conftest_helpers):
    for shim in (spot_shim, futures_shim):
        assert 'analog_retrieval' not in shim.list_strategies()
        assert 'analog_retrieval' in shim.STRATEGY_REGISTRY
        assert shim.STRATEGY_REGISTRY['analog_retrieval']['backtest_only'] is True
        df = conftest_helpers.make_ohlcv(conftest_helpers.make_trending_up(80))
        result = shim.apply_strategy('analog_retrieval', df)
        assert 'signal' in result.columns
        assert (result['signal'] == 0).all()

def test_backtest_only_flag_defaults_false_for_all_other_strategies(registry):
    for platform in registry.VALID_PLATFORMS:
        full = registry.build_registry(platform, include_hidden=True)
        for name, entry in full.items():
            assert entry['backtest_only'] is (name == 'analog_retrieval'), name
M5_DEPRECATE_VERDICT_NAMES = frozenset({'adx_trend', 'amd_ifvg', 'atr_breakout', 'bollinger_bands', 'consolidation_range', 'ema_crossover', 'funding_skew', 'heikin_ashi_ema', 'ichimoku_cloud', 'macd', 'mean_reversion', 'momentum', 'mtf_confluence', 'order_blocks', 'pairs_spread', 'parabolic_sar', 'range_scalper', 'regime_adaptive', 'rsi', 'rsi_macd_combo', 'sma_crossover', 'squeeze_momentum', 'stoch_rsi', 'supertrend', 'sweep_squeeze_combo', 'tema_cross', 'tema_cross_bd', 'triple_ema', 'triple_ema_bidir', 'vol_momentum', 'volume_weighted', 'vwap_reversion'})

def test_m5_deprecated_roster_matches_fee_audit_verdicts(registry):
    assert registry.M5_DEPRECATED_EDGE_STRATEGIES == M5_DEPRECATE_VERDICT_NAMES
    unknown = registry.M5_DEPRECATED_EDGE_STRATEGIES - set(registry.STRATEGIES)
    assert not unknown, f'quarantined names not registered: {sorted(unknown)}'
    assert registry.M5_DEPRECATED_EDGE_STRATEGIES <= registry.DISCOVERY_HIDDEN_STRATEGIES

def test_edge_status_flag_matches_quarantine_roster(registry):
    for name, entry in registry.STRATEGIES.items():
        expected = 'deprecated_m5' if name in registry.M5_DEPRECATED_EDGE_STRATEGIES else None
        assert entry['edge_status'] == expected, name
    for platform in registry.VALID_PLATFORMS:
        full = registry.build_registry(platform, include_hidden=True)
        for name, entry in full.items():
            expected = 'deprecated_m5' if name in registry.M5_DEPRECATED_EDGE_STRATEGIES else None
            assert entry['edge_status'] == expected, name

def test_m5_deprecated_strategies_hidden_but_loadable(spot_shim, futures_shim, conftest_helpers):
    for shim in (spot_shim, futures_shim):
        listed = set(shim.list_strategies())
        for name in shim.STRATEGY_REGISTRY:
            if shim.STRATEGY_REGISTRY[name].get('edge_status') == 'deprecated_m5':
                assert name not in listed, name
        assert 'macd' in shim.STRATEGY_REGISTRY
        assert shim.STRATEGY_REGISTRY['macd']['edge_status'] == 'deprecated_m5'
        df = conftest_helpers.make_ohlcv(conftest_helpers.make_trending_up(80))
        result = shim.apply_strategy('macd', df)
        assert 'signal' in result.columns
