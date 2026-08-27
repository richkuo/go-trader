import pandas as pd
import pytest
from backtester import Backtester
TP_5PCT_FULL = [{'name': 'tiered_tp_pct', 'params': {'tp_tiers': [{'profit_pct': 0.05, 'close_fraction': 1.0}]}}]

def _df(opens, highs, lows, closes, signals):
    n = len(closes)
    idx = pd.date_range('2024-01-01', periods=n, freq='D')
    return pd.DataFrame({'open': [float(v) for v in opens], 'high': [float(v) for v in highs], 'low': [float(v) for v in lows], 'close': [float(v) for v in closes], 'signal': [float(v) for v in signals]}, index=idx)

def _run(df, **kw):
    kw.setdefault('initial_capital', 10000.0)
    kw.setdefault('commission_pct', 0.0)
    kw.setdefault('slippage_pct', 0.0)
    bt = Backtester(**kw)
    return bt.run(df.copy(), strategy_name='intrabar-test', save=False)

def _race_long_df():
    return _df(opens=[100, 100, 100, 106, 106], highs=[101, 101, 107, 107, 107], lows=[99, 99, 96, 105, 105], closes=[100, 100, 106, 106, 106], signals=[1, 0, 0, 0, 0])

def test_same_bar_race_long_adverse_first_stops_out_at_trigger():
    res = _run(_race_long_df(), close_strategies=TP_5PCT_FULL, stop_loss_pct=0.03)
    assert res['total_trades'] == 1
    trade = res['trades'][0]
    assert trade['exit_price'] == pytest.approx(97.0, rel=1e-09)
    assert trade['exit_reason'] == 'sl'
    assert trade['exit_date'] == str(_race_long_df().index[2])
    assert res['final_capital'] == pytest.approx(9700.0, rel=1e-09)

def test_same_bar_race_long_legacy_flag_reproduces_tp_credit():
    res = _run(_race_long_df(), close_strategies=TP_5PCT_FULL, stop_loss_pct=0.03, intrabar_resolution='bar_close')
    assert res['total_trades'] == 1
    trade = res['trades'][0]
    assert trade['exit_price'] == pytest.approx(106.0, rel=1e-09)
    assert trade['exit_date'] == str(_race_long_df().index[3])
    assert res['final_capital'] == pytest.approx(10600.0, rel=1e-09)

def test_same_bar_race_short_adverse_first_stops_out_at_trigger():
    df = _df(opens=[100, 100, 100, 94, 94], highs=[101, 101, 104, 95, 95], lows=[99, 99, 93, 93, 93], closes=[100, 100, 94, 94, 94], signals=[-1, 0, 0, 0, 0])
    res = _run(df, close_strategies=TP_5PCT_FULL, stop_loss_pct=0.03, direction='short')
    assert res['total_trades'] == 1
    trade = res['trades'][0]
    assert trade['side'] == 'short'
    assert trade['exit_price'] == pytest.approx(103.0, rel=1e-09)
    assert trade['exit_reason'] == 'sl'
    assert trade['exit_date'] == str(df.index[2])
    assert res['final_capital'] == pytest.approx(9700.0, rel=1e-09)
    legacy = _run(df, close_strategies=TP_5PCT_FULL, stop_loss_pct=0.03, direction='short', intrabar_resolution='bar_close')
    assert legacy['trades'][0]['exit_price'] == pytest.approx(94.0, rel=1e-09)
    assert legacy['final_capital'] == pytest.approx(10600.0, rel=1e-09)

def test_gap_through_sl_long_fills_at_open_not_trigger():
    df = _df(opens=[100, 100, 95, 94, 94], highs=[101, 101, 96, 95, 95], lows=[99, 99, 94, 93, 93], closes=[100, 100, 95, 94, 94], signals=[1, 0, 0, 0, 0])
    res = _run(df, close_strategies=TP_5PCT_FULL, stop_loss_pct=0.03)
    assert res['total_trades'] == 1
    trade = res['trades'][0]
    assert trade['exit_price'] == pytest.approx(95.0, rel=1e-09)
    assert trade['exit_reason'] == 'sl'
    assert trade['exit_date'] == str(df.index[2])

def test_gap_through_sl_short_fills_at_open_not_trigger():
    df = _df(opens=[100, 100, 105, 106, 106], highs=[101, 101, 106, 107, 107], lows=[99, 99, 104, 105, 105], closes=[100, 100, 105, 106, 106], signals=[-1, 0, 0, 0, 0])
    res = _run(df, close_strategies=TP_5PCT_FULL, stop_loss_pct=0.03, direction='short')
    assert res['total_trades'] == 1
    trade = res['trades'][0]
    assert trade['exit_price'] == pytest.approx(105.0, rel=1e-09)
    assert trade['exit_reason'] == 'sl'
    assert trade['exit_date'] == str(df.index[2])

def test_gap_through_tp_fills_at_open_both_modes():
    df = _df(opens=[100, 100, 100, 110, 110], highs=[101, 101, 107, 111, 111], lows=[99, 99, 99, 109, 109], closes=[100, 100, 106, 110, 110], signals=[1, 0, 0, 0, 0])
    for mode in ('ohlc_walk', 'bar_close'):
        res = _run(df, close_strategies=TP_5PCT_FULL, intrabar_resolution=mode)
        assert res['total_trades'] == 1, mode
        trade = res['trades'][0]
        assert trade['exit_price'] == pytest.approx(110.0, rel=1e-09), mode
        assert trade['exit_date'] == str(df.index[3]), mode

def test_plain_path_stop_fills_at_trigger_same_bar():
    df = _df(opens=[100, 100, 100, 105, 115, 120], highs=[101, 101, 101, 110, 120, 121], lows=[99, 99, 96, 104, 114, 119], closes=[100, 100, 98, 105, 115, 120], signals=[1, 0, 0, 0, 0, 0])
    res = _run(df, stop_loss_pct=0.03)
    assert res['total_trades'] == 1
    trade = res['trades'][0]
    assert trade['exit_price'] == pytest.approx(97.0, rel=1e-09)
    assert trade['exit_reason'] == 'signal_sl'
    assert trade['exit_date'] == str(df.index[2])
    assert res['final_capital'] == pytest.approx(9700.0, rel=1e-09)
    legacy = _run(df, stop_loss_pct=0.03, intrabar_resolution='bar_close')
    assert legacy['total_trades'] == 1
    assert legacy['trades'][0]['exit_reason'] == 'end_of_data'
    assert legacy['final_capital'] == pytest.approx(12000.0, rel=1e-09)

def test_partial_tp_at_open_then_intrabar_stop_same_bar():
    close_refs = [{'name': 'tiered_tp_pct', 'params': {'tp_tiers': [{'profit_pct': 0.05, 'close_fraction': 0.5}]}}]
    df = _df(opens=[100, 100, 100, 106, 106], highs=[101, 101, 107, 107, 107], lows=[99, 99, 99, 96, 96], closes=[100, 100, 106, 106, 106], signals=[1, 0, 0, 0, 0])
    res = _run(df, close_strategies=close_refs, stop_loss_pct=0.03)
    assert res['total_trades'] == 2
    tp_leg, sl_leg = res['trades']
    assert tp_leg['exit_price'] == pytest.approx(106.0, rel=1e-09)
    assert tp_leg['exit_date'] == str(df.index[3])
    assert sl_leg['exit_price'] == pytest.approx(97.0, rel=1e-09)
    assert sl_leg['exit_reason'] == 'sl'
    assert sl_leg['exit_date'] == str(df.index[3])
    assert res['final_capital'] == pytest.approx(50 * 106.0 + 50 * 97.0, rel=1e-09)

def test_no_race_run_identical_across_modes():
    df = _df(opens=[100, 100, 101, 102, 106, 106], highs=[101, 101, 103, 107, 107, 107], lows=[99, 99, 100, 101, 105, 105], closes=[100, 100, 102, 106, 106, 106], signals=[1, 0, 0, 0, 0, 0])
    walk = _run(df, close_strategies=TP_5PCT_FULL, stop_loss_pct=0.05)
    legacy = _run(df, close_strategies=TP_5PCT_FULL, stop_loss_pct=0.05, intrabar_resolution='bar_close')
    assert walk == legacy

def test_entry_bar_trailing_seed_is_not_pierce_eligible():
    df = _df(opens=[100, 100, 110, 110, 110], highs=[101, 110, 111, 111, 111], lows=[99, 99, 109, 109, 109], closes=[100, 110, 110, 110, 110], signals=[1, 0, 0, 0, 0])
    df['atr'] = 2.0
    res = _run(df, trailing_stop_atr_mult=1.0)
    assert res['total_trades'] == 1
    trade = res['trades'][0]
    assert trade['exit_reason'] == 'end_of_data'
    assert res['final_capital'] > 10500.0

def test_carried_trailing_trigger_is_pierce_eligible_next_bar():
    df = _df(opens=[100, 100, 110, 110, 112, 112], highs=[101, 110, 111, 111, 112, 112], lows=[99, 99, 109, 107, 111, 111], closes=[100, 110, 110, 109, 112, 112], signals=[1, 0, 0, 0, 0, 0])
    df['atr'] = 2.0
    res = _run(df, trailing_stop_atr_mult=1.0)
    assert res['total_trades'] == 1
    trade = res['trades'][0]
    assert trade['exit_price'] == pytest.approx(108.0, rel=1e-09)
    assert trade['exit_reason'] == 'signal_sl'
    assert trade['exit_date'] == str(df.index[3])

def test_sl_after_bump_bar_suppresses_pierce_until_next_bar():
    idx = pd.date_range('2024-01-01', periods=5, freq='D')
    df = pd.DataFrame({'open': [100.0, 100.0, 100.0, 110.0, 105.0], 'high': [100.0, 101.0, 110.0, 111.0, 106.0], 'low': [100.0, 99.0, 100.0, 99.0, 99.5], 'close': [100.0, 100.0, 110.0, 108.0, 105.0], 'open_action': ['long', 'none', 'none', 'none', 'none'], 'atr': [10.0] * 5}, index=idx)
    bt = Backtester(initial_capital=1000.0, commission_pct=0.0, slippage_pct=0.0, platform='hyperliquid', strategy_type='perps', stop_loss_atr_mult=1.0, close_strategies=[{'name': 'tiered_tp_atr', 'params': {'sl_after': 'breakeven', 'tp_tiers': [{'atr_multiple': 1.0, 'close_fraction': 0.5}, {'atr_multiple': 2.0, 'close_fraction': 1.0}]}}])
    res = bt.run(df, save=False)
    assert res['total_trades'] == 2
    tp_leg, sl_leg = res['trades']
    assert tp_leg['exit_price'] == pytest.approx(110.0, rel=1e-09)
    assert tp_leg['exit_date'] == str(idx[3])
    assert sl_leg['exit_price'] == pytest.approx(100.0, rel=1e-09)
    assert sl_leg['exit_reason'] == 'sl'
    assert sl_leg['exit_date'] == str(idx[4])
    assert res['final_capital'] == pytest.approx(5 * 110.0 + 5 * 100.0, rel=1e-09)

def test_invalid_intrabar_resolution_rejected():
    with pytest.raises(ValueError, match='intrabar_resolution'):
        Backtester(initial_capital=1000.0, intrabar_resolution='hlc_walk')
FEE = 0.001
FULL_ENTRY_FEE = 10.0

def test_intrabar_stop_full_position_charges_full_entry_fee():
    df = _df(opens=[100, 100, 100, 100], highs=[101, 101, 101, 101], lows=[99, 99, 96, 96], closes=[100, 100, 96, 96], signals=[1, 0, 0, 0])
    res = _run(df, close_strategies=TP_5PCT_FULL, stop_loss_pct=0.03, commission_pct=FEE)
    assert res['total_trades'] == 1
    trade = res['trades'][0]
    assert trade['exit_reason'] == 'sl'
    assert trade['entry_fee'] == pytest.approx(FULL_ENTRY_FEE, rel=1e-09)

def test_partial_tp_then_intrabar_stop_entry_fees_sum_to_one_fee():
    close_refs = [{'name': 'tiered_tp_pct', 'params': {'tp_tiers': [{'profit_pct': 0.05, 'close_fraction': 0.5}]}}]
    df = _df(opens=[100, 100, 100, 106, 106], highs=[101, 101, 107, 107, 107], lows=[99, 99, 99, 96, 96], closes=[100, 100, 106, 106, 106], signals=[1, 0, 0, 0, 0])
    res = _run(df, close_strategies=close_refs, stop_loss_pct=0.03, commission_pct=FEE)
    assert res['total_trades'] == 2
    tp_leg, sl_leg = res['trades']
    assert sl_leg['exit_reason'] == 'sl'
    assert tp_leg['entry_fee'] == pytest.approx(FULL_ENTRY_FEE / 2, rel=1e-09)
    assert sl_leg['entry_fee'] == pytest.approx(FULL_ENTRY_FEE / 2, rel=1e-09)

def test_three_way_split_entry_fees_prorate_by_initial_quantity():
    close_refs = [{'name': 'tiered_tp_pct', 'params': {'tp_tiers': [{'profit_pct': 0.05, 'close_fraction': 0.25}, {'profit_pct': 0.1, 'close_fraction': 0.25}]}}]
    df = _df(opens=[100, 100, 100, 105, 110, 110], highs=[101, 101, 106, 111, 111, 111], lows=[99, 99, 99, 104, 96, 96], closes=[100, 100, 105, 110, 110, 110], signals=[1, 0, 0, 0, 0, 0])
    res = _run(df, close_strategies=close_refs, stop_loss_pct=0.03, commission_pct=FEE)
    assert res['total_trades'] == 3
    trades = res['trades']
    assert trades[-1]['exit_reason'] == 'sl'
    initial_shares = sum((t['shares'] for t in trades))
    assert initial_shares == pytest.approx((10000.0 - FULL_ENTRY_FEE) / 100.0, rel=1e-09)
    for t in trades:
        assert t['entry_fee'] == pytest.approx(FULL_ENTRY_FEE * t['shares'] / initial_shares, rel=1e-06)
    assert sum((t['entry_fee'] for t in trades)) == pytest.approx(FULL_ENTRY_FEE, rel=1e-09)
