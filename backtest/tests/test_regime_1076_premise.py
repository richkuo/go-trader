import os
import sys
import numpy as np
import pandas as pd
import pytest
sys.path.insert(0, os.path.abspath(os.path.join(os.path.dirname(__file__), '..', 'research')))
import regime_1076_directional_premise as premise

def test_bare_symbol_has_no_exchange():
    assert premise.parse_symbol_spec('BTC/USDT') == ('BTC/USDT', None)

def test_ccxt_perp_symbol_splits_on_the_last_at():
    assert premise.parse_symbol_spec('HYPE/USDC:USDC@hyperliquid') == ('HYPE/USDC:USDC', 'hyperliquid')

def test_spec_whitespace_is_trimmed():
    assert premise.parse_symbol_spec('  ETH/USDT @ binanceus ') == ('ETH/USDT', 'binanceus')

@pytest.mark.parametrize('spec', ['', '   ', '@hyperliquid', 'BTC/USDT@', 'BTC/USDT@   '])
def test_malformed_specs_raise(spec):
    with pytest.raises(ValueError):
        premise.parse_symbol_spec(spec)

def test_parse_symbols_arg_mixed_defaults_and_mapped_symbol():
    specs = premise.parse_symbols_arg('BTC/USDT,ETH/USDT,SOL/USDT,HYPE/USDC:USDC@hyperliquid')
    assert specs == (('BTC/USDT', None), ('ETH/USDT', None), ('SOL/USDT', None), ('HYPE/USDC:USDC', 'hyperliquid'))

def test_parse_symbols_arg_skips_blank_entries():
    assert premise.parse_symbols_arg('BTC/USDT, ,ETH/USDT') == (('BTC/USDT', None), ('ETH/USDT', None))

def test_parse_symbols_arg_rejects_an_empty_universe():
    with pytest.raises(ValueError):
        premise.parse_symbols_arg('  ,  ')

def test_normalize_symbol_specs_accepts_both_forms():
    assert premise.normalize_symbol_specs(['BTC/USDT', ('HYPE/USDC:USDC', 'hyperliquid')]) == (('BTC/USDT', None), ('HYPE/USDC:USDC', 'hyperliquid'))

def test_normalize_symbol_specs_does_not_reparse_bare_strings():
    assert premise.normalize_symbol_specs(['A@B']) == (('A@B', None),)

def test_resolve_data_sources_fills_platform_for_unmapped_symbols():
    sources = premise.resolve_data_sources(premise.parse_symbols_arg('BTC/USDT,HYPE/USDC:USDC@hyperliquid'))
    assert sources == {'BTC/USDT': premise.PLATFORM, 'HYPE/USDC:USDC': 'hyperliquid'}

def _frame(start, periods, freq='1h'):
    idx = pd.date_range(start, periods=periods, freq=freq)
    return pd.DataFrame({'open': 1.0, 'high': 1.0, 'low': 1.0, 'close': 1.0, 'volume': 1.0}, index=idx)

def test_clip_window_trims_both_ends_inclusive_of_end():
    df = _frame('2023-12-30', 120)
    out = premise._clip_window(df, '2023-01-01', '2024-01-01')
    assert out.index.min() == pd.Timestamp('2023-12-30')
    assert out.index.max() == pd.Timestamp('2024-01-01 00:00:00')

def test_clip_window_open_ended_when_end_is_none():
    df = _frame('2026-01-01', 48)
    out = premise._clip_window(df, '2026-01-01', None)
    assert len(out) == 48

def test_clip_window_drops_everything_before_the_window():
    df = _frame('2024-11-01', 100)
    out = premise._clip_window(df, '2023-01-01', '2024-01-01')
    assert len(out) == 0

def test_clip_window_passes_empty_frames_through():
    empty = pd.DataFrame(columns=['open', 'high', 'low', 'close', 'volume'])
    assert len(premise._clip_window(empty, '2023-01-01', '2024-01-01')) == 0

def test_clip_window_rejects_a_non_datetime_index():
    df = pd.DataFrame({'close': [1.0, 2.0]})
    with pytest.raises(ValueError):
        premise._clip_window(df, '2023-01-01', '2024-01-01')

def _price_frame(start, periods):
    rng = np.random.default_rng(7)
    idx = pd.date_range(start, periods=periods, freq='1h')
    close = 100.0 * np.exp(np.cumsum(rng.normal(0, 0.002, periods)))
    return pd.DataFrame({'open': close, 'high': close * 1.001, 'low': close * 0.999, 'close': close, 'volume': 1.0}, index=idx)

def test_load_clips_a_fetch_fallback_frame_to_the_requested_window(monkeypatch):
    start, end = premise.WINDOWS['2023']
    full = _price_frame('2023-11-01', 24 * 400)
    assert full.index.max() > pd.Timestamp(end)
    monkeypatch.setattr(premise, 'load_cached_data', lambda *a, **k: full.copy())
    d = premise._load('HYPE/USDC:USDC', '1h', '2023', 'composite', dict(premise._DEFAULT_COMPOSITE_THRESHOLDS), exchange='hyperliquid')
    assert d is not None
    in_window = int(((full.index >= pd.Timestamp(start)) & (full.index <= pd.Timestamp(end))).sum())
    assert in_window < len(full)
    assert d['n_bars'] <= in_window
    assert len(d['close']) == in_window

def test_load_passes_the_mapped_exchange_to_the_loader(monkeypatch):
    seen = {}

    def fake(symbol, timeframe, exchange_id=None, start_date=None, end_date=None):
        seen['exchange_id'] = exchange_id
        return _price_frame('2025-01-01', 24 * 120)
    monkeypatch.setattr(premise, 'load_cached_data', fake)
    premise._load('HYPE/USDC:USDC', '1h', '2025H1', 'composite', dict(premise._DEFAULT_COMPOSITE_THRESHOLDS), exchange='hyperliquid')
    assert seen['exchange_id'] == 'hyperliquid'

def test_load_defaults_to_platform_when_no_exchange_is_mapped(monkeypatch):
    seen = {}

    def fake(symbol, timeframe, exchange_id=None, start_date=None, end_date=None):
        seen['exchange_id'] = exchange_id
        return _price_frame('2025-01-01', 24 * 120)
    monkeypatch.setattr(premise, 'load_cached_data', fake)
    premise._load('BTC/USDT', '1h', '2025H1', 'composite', dict(premise._DEFAULT_COMPOSITE_THRESHOLDS))
    assert seen['exchange_id'] == premise.PLATFORM

def test_load_returns_none_for_a_window_the_asset_predates(monkeypatch):
    monkeypatch.setattr(premise, 'load_cached_data', lambda *a, **k: _price_frame('2024-11-01', 24 * 300))
    assert premise._load('HYPE/USDC:USDC', '1h', '2023', 'composite', dict(premise._DEFAULT_COMPOSITE_THRESHOLDS), exchange='hyperliquid') is None

def _row(symbol='HYPE/USDC:USDC', timeframe='1h', window='oos', classifier='composite', source='hyperliquid', n_bars=1200):
    return {'classifier': classifier, 'symbol': symbol, 'source': source, 'n_bars': n_bars, 'timeframe': timeframe, 'window': window, 'horizon': 4, 'state': 'trending_up', 'gap': 0.01, 'mean_fwd': 0.01, 'p_value': 0.5, 'fdr_reject': False, 'policy_dir': 1, 'sign_aligned': True, 'candidate_edge': False}

def test_coverage_table_counts_rows_and_bars_per_classifier():
    rows = [_row(), _row(), _row(classifier='adx', n_bars=1234)]
    cov = premise.coverage_table(rows)
    assert len(cov) == 1
    e = cov[0]
    assert e['symbol'] == 'HYPE/USDC:USDC'
    assert e['source'] == 'hyperliquid'
    assert e['rows'] == 3
    assert e['bars'] == {'composite': 1200, 'adx': 1234}

def test_coverage_table_is_sorted_and_splits_windows():
    rows = [_row(window='oos'), _row(window='is'), _row(symbol='BTC/USDT', source='binanceus')]
    cov = premise.coverage_table(rows)
    assert [(e['symbol'], e['window']) for e in cov] == [('BTC/USDT', 'oos'), ('HYPE/USDC:USDC', 'is'), ('HYPE/USDC:USDC', 'oos')]

def test_coverage_table_omits_windows_that_contributed_nothing():
    cov = premise.coverage_table([_row(window='oos')])
    assert [e['window'] for e in cov] == ['oos']

def test_parse_symbols_arg_rejects_a_repeated_symbol():
    with pytest.raises(ValueError, match='duplicate symbol'):
        premise.parse_symbols_arg('BTC/USDT,ETH/USDT,BTC/USDT@kraken')

def test_parse_symbols_arg_rejects_an_exact_repeat():
    with pytest.raises(ValueError, match='duplicate symbol'):
        premise.parse_symbols_arg('BTC/USDT,BTC/USDT')

def test_parse_symbols_arg_allows_distinct_symbols_on_one_venue():
    assert premise.parse_symbols_arg('BTC/USDT@kraken,ETH/USDT@kraken') == (('BTC/USDT', 'kraken'), ('ETH/USDT', 'kraken'))
