import sys
import pathlib
import numpy as np
import pandas as pd
import pytest
sys.path.insert(0, str(pathlib.Path(__file__).parent.parent.parent / 'shared_tools'))
sys.path.insert(0, str(pathlib.Path(__file__).parent.parent))
from backtester import Backtester
from regime import compute_regime, latest_regime, ensure_regime_columns

def _uptrend_df(n: int=100) -> pd.DataFrame:
    close = np.linspace(100.0, 200.0, n)
    idx = pd.date_range('2024-01-01', periods=n, freq='D')
    return pd.DataFrame({'open': close, 'high': close + 0.5, 'low': close - 0.5, 'close': close, 'volume': 1000.0}, index=idx)

def _ranging_df(n: int=100) -> pd.DataFrame:
    close = np.full(n, 100.0)
    idx = pd.date_range('2024-01-01', periods=n, freq='D')
    return pd.DataFrame({'open': close, 'high': close + 0.05, 'low': close - 0.05, 'close': close, 'volume': 1000.0}, index=idx)

def _signal_df(df: pd.DataFrame, *, buy_at: int=30) -> pd.DataFrame:
    out = df.copy()
    out['signal'] = 0
    out.iloc[buy_at, out.columns.get_loc('signal')] = 1
    return out

def test_ensure_regime_columns_adds_regime_column():
    df = _uptrend_df()
    out = ensure_regime_columns(df)
    assert 'regime' in out.columns
    assert 'regime_score' in out.columns

def test_ensure_regime_columns_uptrend_labels_trending_up():
    df = _uptrend_df(n=100)
    out = ensure_regime_columns(df, period=14, adx_threshold=20.0)
    assert out['regime'].iloc[-1] == 'trending_up'

def test_ensure_regime_columns_ranging_labels_ranging():
    df = _ranging_df(n=100)
    out = ensure_regime_columns(df, period=14, adx_threshold=20.0)
    assert out['regime'].iloc[-1] == 'ranging'

def test_ensure_regime_columns_composite_labels():
    df = _uptrend_df(n=120)
    out = ensure_regime_columns(df, period=50, classifier='composite', thresholds={'return_eff': 0.02, 'range_eff': 0.02, 'adx': 15})
    label = out['regime'].iloc[-1]
    assert label.startswith('trending_up')

def test_backtester_accepts_regime_params():
    bt = Backtester(initial_capital=1000, commission_pct=0, slippage_pct=0, regime_enabled=True, regime_period=14, regime_adx_threshold=20.0, allowed_regimes=['trending_up'])
    assert bt.regime_enabled is True
    assert bt.regime_period == 14
    assert bt.regime_adx_threshold == 20.0
    assert bt.allowed_regimes == ['trending_up']

def test_backtester_regime_defaults():
    bt = Backtester(initial_capital=1000, commission_pct=0, slippage_pct=0)
    assert bt.regime_enabled is False
    assert bt.allowed_regimes == []

def test_regime_gate_allows_entry_when_regime_matches():
    df = _uptrend_df(n=100)
    ensure_regime_columns(df, period=14, adx_threshold=20.0)
    df_sig = _signal_df(df, buy_at=50)
    bt = Backtester(initial_capital=1000, commission_pct=0, slippage_pct=0, regime_enabled=True, allowed_regimes=['trending_up'])
    result = bt.run(df_sig, save=False)
    assert result['total_trades'] >= 1, 'Expected at least one trade when regime matches'

def test_regime_gate_blocks_entry_when_regime_mismatches():
    df = _uptrend_df(n=100)
    ensure_regime_columns(df, period=14, adx_threshold=20.0)
    df_sig = _signal_df(df, buy_at=50)
    bt = Backtester(initial_capital=1000, commission_pct=0, slippage_pct=0, regime_enabled=True, allowed_regimes=['ranging'])
    result = bt.run(df_sig, save=False)
    assert result['total_trades'] == 0, 'Expected no trades when regime gate blocks entry'

def test_regime_gate_does_not_close_open_position():
    df = _uptrend_df(n=100)
    ensure_regime_columns(df, period=14, adx_threshold=20.0)
    df_sig = df.copy()
    df_sig['signal'] = 0
    df_sig.iloc[50, df_sig.columns.get_loc('signal')] = 1
    df_sig['regime'] = 'trending_up'
    df_sig.iloc[52:, df_sig.columns.get_loc('regime')] = 'ranging'
    bt = Backtester(initial_capital=1000, commission_pct=0, slippage_pct=0, regime_enabled=True, allowed_regimes=['trending_up'])
    result = bt.run(df_sig, save=False)
    assert result['total_trades'] == 1
    assert result['trades'][0]['exit_price'] > 0

def test_regime_enabled_empty_allowed_allows_all():
    df = _uptrend_df(n=100)
    ensure_regime_columns(df, period=14, adx_threshold=20.0)
    df_sig = _signal_df(df, buy_at=50)
    bt = Backtester(initial_capital=1000, commission_pct=0, slippage_pct=0, regime_enabled=True, allowed_regimes=[])
    result = bt.run(df_sig, save=False)
    assert result['total_trades'] >= 1

def test_parity_latest_regime_matches_compute_regime_last_bar():
    df = _uptrend_df(n=100)
    live_result = latest_regime(df, period=14, adx_threshold=20.0)
    backtest_series = compute_regime(df, period=14, adx_threshold=20.0)
    backtest_last = backtest_series.iloc[-1]
    assert live_result['regime'] == backtest_last['regime'], f"Parity violation: live={live_result['regime']}, backtest last bar={backtest_last['regime']}"
    assert abs(live_result['score'] - float(backtest_last['regime_score'])) < 1e-09, 'Score mismatch between live and backtest'

def test_parity_ranging_df():
    df = _ranging_df(n=100)
    live = latest_regime(df, period=14, adx_threshold=20.0)
    bt_last = compute_regime(df, period=14, adx_threshold=20.0).iloc[-1]
    assert live['regime'] == bt_last['regime']

def test_regime_disabled_does_not_block_entries():
    df = _ranging_df(n=100)
    df_sig = _signal_df(df, buy_at=50)
    bt = Backtester(initial_capital=1000, commission_pct=0, slippage_pct=0, regime_enabled=False, allowed_regimes=['trending_up'])
    result = bt.run(df_sig, save=False)
    assert result['total_trades'] >= 1, 'Disabled regime gate must not block entries'
