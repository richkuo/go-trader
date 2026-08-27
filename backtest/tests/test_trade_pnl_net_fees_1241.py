import pandas as pd
import pytest
from backtester import Backtester
COMMISSION = 0.001
INITIAL_CAPITAL = 1000.0

def _df_signals(opens, signals):
    n = len(opens)
    idx = pd.date_range('2024-01-01', periods=n, freq='D')
    return pd.DataFrame({'open': opens, 'close': opens, 'signal': signals}, index=idx)

def _df_open_then_hold(opens, closes, atrs, side='long'):
    n = len(closes)
    idx = pd.date_range('2024-01-01', periods=n, freq='D')
    open_actions = [side] + ['none'] * (n - 1)
    return pd.DataFrame({'open': opens, 'close': closes, 'open_action': open_actions, 'atr': atrs}, index=idx)

def test_plain_close_gross_winner_flips_to_net_loss():
    df = _df_signals(opens=[100.0, 100.0, 100.0, 100.0, 100.15, 100.15], signals=[0, 1, 0, -1, 0, 0])
    bt = Backtester(initial_capital=INITIAL_CAPITAL, commission_pct=COMMISSION, slippage_pct=0.0)
    result = bt.run(df, save=False)
    assert result['total_trades'] == 1
    trade = result['trades'][0]
    assert trade['entry_fee'] > 0.0
    assert trade['exit_fee'] > 0.0
    gross = trade['pnl'] + trade['entry_fee'] + trade['exit_fee']
    assert gross > 0.0
    assert trade['pnl'] < 0.0
    assert result['win_rate'] == 0.0
    assert result['profit_factor'] == 0.0

def test_partial_close_prorated_entry_fees_sum_and_net():
    df = _df_open_then_hold(opens=[100, 100, 100, 110, 120], closes=[100, 100, 110, 120, 120], atrs=[10, 10, 10, 10, 10])
    bt = Backtester(initial_capital=INITIAL_CAPITAL, commission_pct=COMMISSION, slippage_pct=0.0, close_strategies=[{'name': 'tiered_tp_atr', 'params': {'tp_tiers': [{'atr_multiple': 1.0, 'close_fraction': 0.5}, {'atr_multiple': 2.0, 'close_fraction': 1.0}]}}])
    result = bt.run(df, save=False)
    assert result['total_trades'] == 2
    leg0, leg1 = (result['trades'][0], result['trades'][1])
    total_entry_fee = INITIAL_CAPITAL * COMMISSION
    assert leg0['entry_fee'] + leg1['entry_fee'] == pytest.approx(total_entry_fee)
    for leg in (leg0, leg1):
        entry_px, exit_px, shares = (leg['entry_price'], leg['exit_price'], leg['shares'])
        gross = shares * (exit_px - entry_px)
        expected_exit_fee = shares * exit_px * COMMISSION
        assert leg['exit_fee'] == pytest.approx(expected_exit_fee)
        expected_net = gross - leg['entry_fee'] - leg['exit_fee']
        assert leg['pnl'] == pytest.approx(expected_net, abs=0.01)
    net_total = leg0['pnl'] + leg1['pnl']
    gross_total = leg0['shares'] * (leg0['exit_price'] - leg0['entry_price']) + leg1['shares'] * (leg1['exit_price'] - leg1['entry_price'])
    assert net_total < gross_total

def test_partial_then_end_of_data_entry_fees_sum_to_whole():
    df = _df_open_then_hold(opens=[100, 100, 100, 110, 110], closes=[100, 100, 110, 110, 110], atrs=[10, 10, 10, 10, 10])
    bt = Backtester(initial_capital=INITIAL_CAPITAL, commission_pct=COMMISSION, slippage_pct=0.0, close_strategies=[{'name': 'tiered_tp_atr', 'params': {'tp_tiers': [{'atr_multiple': 1.0, 'close_fraction': 0.5}, {'atr_multiple': 5.0, 'close_fraction': 1.0}]}}])
    result = bt.run(df, save=False)
    assert result['total_trades'] == 2
    leg0, leg1 = (result['trades'][0], result['trades'][1])
    assert leg1['exit_reason'] == 'end_of_data'
    total_entry_fee = INITIAL_CAPITAL * COMMISSION
    assert leg0['entry_fee'] + leg1['entry_fee'] == pytest.approx(total_entry_fee)
    assert leg1['entry_fee'] == pytest.approx(total_entry_fee * 0.5)
    for leg in (leg0, leg1):
        entry_px, exit_px, shares = (leg['entry_price'], leg['exit_price'], leg['shares'])
        gross = shares * (exit_px - entry_px)
        expected_net = gross - leg['entry_fee'] - leg['exit_fee']
        assert leg['pnl'] == pytest.approx(expected_net, abs=0.01)

def test_avg_loss_uses_net_convention_not_gross_pnl_pct():
    df = _df_signals(opens=[100.0, 100.0, 100.0, 100.0, 100.15, 100.15], signals=[0, 1, 0, -1, 0, 0])
    bt = Backtester(initial_capital=INITIAL_CAPITAL, commission_pct=COMMISSION, slippage_pct=0.0)
    result = bt.run(df, save=False)
    assert result['trades'][0]['pnl'] < 0.0
    assert result['trades'][0]['pnl_pct'] > 0.0
    assert result['avg_loss_pct'] < 0.0
    assert result['avg_win_pct'] == 0.0

def test_short_partial_then_end_of_data_entry_fees_sum_to_whole():
    df = _df_open_then_hold(opens=[100, 100, 100, 90, 90], closes=[100, 100, 90, 90, 90], atrs=[10, 10, 10, 10, 10], side='short')
    bt = Backtester(initial_capital=INITIAL_CAPITAL, commission_pct=COMMISSION, slippage_pct=0.0, close_strategies=[{'name': 'tiered_tp_atr', 'params': {'tp_tiers': [{'atr_multiple': 1.0, 'close_fraction': 0.5}, {'atr_multiple': 5.0, 'close_fraction': 1.0}]}}])
    result = bt.run(df, save=False)
    assert result['total_trades'] == 2
    leg0, leg1 = (result['trades'][0], result['trades'][1])
    assert leg0['side'] == 'short'
    assert leg1['side'] == 'short'
    assert leg1['exit_reason'] == 'end_of_data'
    total_entry_fee = INITIAL_CAPITAL * COMMISSION
    assert leg0['entry_fee'] + leg1['entry_fee'] == pytest.approx(total_entry_fee)
    assert leg1['entry_fee'] == pytest.approx(total_entry_fee * 0.5)
    for leg in (leg0, leg1):
        entry_px, exit_px, shares = (leg['entry_price'], leg['exit_price'], leg['shares'])
        gross = shares * (entry_px - exit_px)
        expected_net = gross - leg['entry_fee'] - leg['exit_fee']
        assert leg['pnl'] == pytest.approx(expected_net, abs=0.01)
