import json
import os
import sys
import types
import numpy as np
import pandas as pd
import pytest
_BT_DIR = os.path.abspath(os.path.join(os.path.dirname(__file__), '..'))
if _BT_DIR not in sys.path:
    sys.path.insert(0, _BT_DIR)
import eval_windows as ew
from backtester import Backtester, format_results

def _metrics(equities, trades=None, timeframe='1d', initial_capital=1000.0):
    idx = pd.date_range('2024-01-01', periods=len(equities), freq='D')
    equity_df = pd.DataFrame({'equity': np.asarray(equities, dtype=float)}, index=idx)
    df = pd.DataFrame({'close': np.full(len(equities), 100.0)}, index=idx)
    bt = Backtester(initial_capital=initial_capital)
    return bt._calculate_metrics(equity_df, trades or [], df, timeframe=timeframe)

def _trade(pnl, pnl_pct=0.0, shares=1.0, entry_price=100.0):
    return types.SimpleNamespace(pnl=pnl, pnl_pct=pnl_pct, shares=shares, entry_price=entry_price)

def _ann_factor(timeframe='1d'):
    return np.sqrt(365)

def test_sortino_zero_losing_bars_is_none_not_zero():
    m = _metrics([1000, 1100, 1200, 1300, 1400])
    assert m['liquidated'] is False
    assert m['sortino_ratio'] is None
    assert m['sharpe_ratio'] > 0

def test_sortino_one_losing_bar_is_finite_canonical_value():
    equities = [1000, 1100, 1050, 1200, 1300]
    m = _metrics(equities)
    rets = pd.Series(equities, dtype=float).pct_change().dropna()
    downside_dev = float(np.sqrt((rets.clip(upper=0.0) ** 2).mean()))
    expected = rets.mean() / downside_dev * _ann_factor()
    assert m['sortino_ratio'] is not None
    assert m['sortino_ratio'] == pytest.approx(round(expected, 3))
    assert m['sortino_ratio'] != 0.0

def test_sortino_all_loss_is_finite_negative():
    equities = [1000, 950, 900, 850, 800]
    m = _metrics(equities)
    rets = pd.Series(equities, dtype=float).pct_change().dropna()
    downside_dev = float(np.sqrt((rets.clip(upper=0.0) ** 2).mean()))
    expected = rets.mean() / downside_dev * _ann_factor()
    assert m['liquidated'] is False
    assert m['sortino_ratio'] is not None
    assert m['sortino_ratio'] < 0
    assert m['sortino_ratio'] == pytest.approx(round(expected, 3))

def test_sortino_liquidated_forces_floor_not_none():
    m = _metrics([1000, -500, 800, 900])
    assert m['liquidated'] is True
    assert m['sortino_ratio'] is not None
    assert m['sortino_ratio'] < 0

def test_sortino_uptrend_with_no_trades_still_none():
    m = _metrics([1000, 1010, 1020], trades=[])
    assert m['sortino_ratio'] is None
    assert m['profit_factor'] == 0

def test_profit_factor_all_win_is_none():
    trades = [_trade(50.0, 0.05), _trade(30.0, 0.03)]
    m = _metrics([1000, 1050, 1080], trades=trades)
    assert m['profit_factor'] is None

def test_profit_factor_mixed_is_finite():
    trades = [_trade(80.0, 0.08), _trade(-20.0, -0.02)]
    m = _metrics([1000, 1080, 1060], trades=trades)
    assert m['profit_factor'] == pytest.approx(round(80.0 / 20.0, 3))

def test_profit_factor_none_never_serializes_as_infinity():
    trades = [_trade(50.0, 0.05), _trade(30.0, 0.03)]
    m = _metrics([1000, 1050, 1080], trades=trades)
    dumped = json.dumps(m)
    assert 'Infinity' not in dumped
    assert '-Infinity' not in dumped
    assert 'NaN' not in dumped
    reparsed = json.loads(dumped)
    assert reparsed['profit_factor'] is None
    assert reparsed['sortino_ratio'] is None
    json.dumps(m, allow_nan=False)

def test_format_results_tolerates_none_metrics():
    trades = [_trade(50.0, 0.05)]
    m = _metrics([1000, 1050], trades=trades)
    results = {'strategy_name': 'x', 'symbol': 'BTC/USDT', 'timeframe': '1d', 'start_date': '2024-01-01T00:00:00', 'end_date': '2024-01-02T00:00:00', 'initial_capital': 1000.0, 'final_capital': 1050.0, **m}
    text = format_results(results)
    assert 'n/a' in text

class _FakeRegistry:
    STRATEGY_REGISTRY = {'noop': {'default_params': {}, 'description': 't'}}

    @staticmethod
    def list_strategies():
        return ['noop']

    @staticmethod
    def apply_strategy(name, df, params):
        out = df.copy()
        out['signal'] = np.zeros(len(out), dtype=int)
        return out

def _master_frame():
    idx = pd.date_range('2023-12-31 00:00', '2024-01-02 00:00', freq='1h')
    base = 100.0 + np.arange(len(idx))
    return pd.DataFrame({'open': base, 'high': base * 1.01, 'low': base * 0.99, 'close': base, 'volume': np.full(len(idx), 1000.0)}, index=idx)

def _inclusive_loader(master):

    def _load(symbol, timeframe, start_date=None, end_date=None, **kw):
        df = master
        if start_date is not None:
            df = df[df.index >= pd.Timestamp(start_date)]
        if end_date is not None:
            df = df[df.index <= pd.Timestamp(end_date)]
        return df.copy()
    return _load

def _captured_index(monkeypatch, window):
    import data_fetcher
    monkeypatch.setattr(data_fetcher, 'load_cached_data', _inclusive_loader(_master_frame()), raising=True)
    seen = {}
    orig = _FakeRegistry.apply_strategy
    reg = _FakeRegistry()

    def _spy(name, df, params):
        seen['index'] = df.index
        return orig(name, df, params)
    monkeypatch.setattr(reg, 'apply_strategy', _spy)
    ew.run_leg(reg, 'noop', None, 'BTC/USDT', '1h', window)
    return seen['index']

def test_adjacent_windows_share_no_bar(monkeypatch):
    boundary = pd.Timestamp('2024-01-01 00:00')
    idx_2023 = _captured_index(monkeypatch, ('2023-12-31', '2024-01-01'))
    idx_2024 = _captured_index(monkeypatch, ('2024-01-01', '2024-01-02'))
    assert set(idx_2023) & set(idx_2024) == set()
    assert boundary not in idx_2023
    assert boundary in idx_2024
    assert idx_2023.max() < boundary

def test_open_ended_window_keeps_last_bar(monkeypatch):
    idx = _captured_index(monkeypatch, ('2023-12-31', None))
    assert idx.max() == pd.Timestamp('2024-01-02 00:00')
