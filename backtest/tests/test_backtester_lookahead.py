import sys
import pathlib
import numpy as np
import pandas as pd
import pytest
sys.path.insert(0, str(pathlib.Path(__file__).parent.parent.parent / 'shared_tools'))
sys.path.insert(0, str(pathlib.Path(__file__).parent.parent))
from backtester import Backtester
_NEVER_FIRES_CLOSE = [{'name': 'tiered_tp_pct', 'params': {'tp_tiers': [{'profit_pct': 0.9, 'close_fraction': 1.0}]}}]
_REGIME_DIRECTIONAL_POLICY = {'trend_regime': {'trending_up': {'direction': 'long', 'invert_signal': False}, 'trending_down': {'direction': 'short', 'invert_signal': True}, 'ranging': {'direction': 'long', 'invert_signal': False}}}

def _step_up_df(n: int=20, jump_bar: int=10, jump_pct: float=0.1) -> pd.DataFrame:
    close = np.full(n, 100.0)
    close[jump_bar:] = 100.0 * (1.0 + jump_pct)
    open_ = close.copy()
    open_[jump_bar] = 100.0
    idx = pd.date_range('2024-01-01', periods=n, freq='D')
    return pd.DataFrame({'open': open_, 'high': np.maximum(open_, close) + 0.01, 'low': np.minimum(open_, close) - 0.01, 'close': close, 'volume': 1000.0}, index=idx)

def test_signal_at_bar_k_fills_at_bar_k_plus_1_open():
    df = _step_up_df(n=20, jump_bar=10, jump_pct=0.1)
    df['signal'] = 0
    df.iloc[9, df.columns.get_loc('signal')] = 1
    bt = Backtester(initial_capital=1000.0, commission_pct=0.0, slippage_pct=0.0)
    result = bt.run(df, save=False)
    assert result['total_trades'] >= 1
    entry_price = result['trades'][0]['entry_price']
    entry_date = pd.Timestamp(result['trades'][0]['entry_date'])
    assert entry_date == df.index[10], f'Entry filled at {entry_date}, expected {df.index[10]} (bar 10 = signal_bar + 1). Shift-by-1 protection in the signal-normalization block of Backtester.run may be broken.'
    assert entry_price == 100.0, f"Expected entry at bar 10's open=100, got {entry_price}"

def test_intra_bar_jump_captured_at_next_bar_open_documents_limit():
    df = _step_up_df(n=20, jump_bar=10, jump_pct=0.2)
    df['signal'] = 0
    df.iloc[9, df.columns.get_loc('signal')] = 1
    df.iloc[15, df.columns.get_loc('signal')] = -1
    bt = Backtester(initial_capital=1000.0, commission_pct=0.0, slippage_pct=0.0)
    result = bt.run(df, save=False)
    final_pct = (result['final_capital'] - 1000.0) / 1000.0 * 100.0
    assert final_pct > 15.0, f"Expected ≥+15% (captures intra-bar jump at bar 10's open), got {final_pct:.2f}%"

def test_regime_gate_uses_prior_bar_regime_not_current():
    df = _step_up_df(n=20, jump_bar=15)
    df['signal'] = 0
    df.iloc[9, df.columns.get_loc('signal')] = 1
    df['regime'] = 'ranging'
    df.iloc[9, df.columns.get_loc('regime')] = 'trending_up'
    bt = Backtester(initial_capital=1000.0, commission_pct=0.0, slippage_pct=0.0, regime_enabled=True, allowed_regimes=['trending_up'])
    result = bt.run(df, save=False)
    assert result['total_trades'] == 1, "Entry should pass: bar 9's regime is 'trending_up' (allowed). If 0 trades, the backtester is reading bar 10's regime (look-ahead) instead of bar 9's. See the regime-shift block in Backtester.run."

def test_regime_gate_blocks_when_prior_bar_regime_disallows():
    df = _step_up_df(n=20, jump_bar=15)
    df['signal'] = 0
    df.iloc[9, df.columns.get_loc('signal')] = 1
    df['regime'] = 'trending_up'
    df.iloc[9, df.columns.get_loc('regime')] = 'ranging'
    bt = Backtester(initial_capital=1000.0, commission_pct=0.0, slippage_pct=0.0, regime_enabled=True, allowed_regimes=['trending_up'])
    result = bt.run(df, save=False)
    assert result['total_trades'] == 0, "Entry should be blocked by bar 9's 'ranging' regime. If a trade fired, the backtester is reading bar 10's regime (look-ahead). See the regime-shift block in Backtester.run."

def test_regime_directional_policy_uses_prior_bar_regime_not_current():
    df = _step_up_df(n=20, jump_bar=15)
    df['signal'] = 0
    df.iloc[9, df.columns.get_loc('signal')] = 1
    df['regime'] = 'trending_up'
    df.iloc[9, df.columns.get_loc('regime')] = 'trending_down'
    bt = Backtester(initial_capital=1000.0, commission_pct=0.0, slippage_pct=0.0, close_strategies=_NEVER_FIRES_CLOSE, regime_enabled=True, regime_directional_policy=_REGIME_DIRECTIONAL_POLICY, regime_directional_certified=True)
    result = bt.run(df, save=False)
    assert [t['side'] for t in result['trades']] == ['short'], "Policy resolver must read bar 9's trending_down label for the bar 10 fill. Reading bar 10's trending_up label would open long instead."

def test_forward_peek_signal_documents_caller_responsibility():
    n = 200
    rng = np.random.default_rng(42)
    returns = rng.normal(0.001, 0.02, n)
    close = 100.0 * np.cumprod(1.0 + returns)
    idx = pd.date_range('2024-01-01', periods=n, freq='D')
    df = pd.DataFrame({'open': close, 'high': close * 1.005, 'low': close * 0.995, 'close': close, 'volume': 1000.0}, index=idx)
    df['signal'] = np.where(df['close'].shift(-1) > df['close'], 1, -1).astype(int)
    df.loc[df.index[-1], 'signal'] = 0
    bt = Backtester(initial_capital=1000.0, commission_pct=0.0, slippage_pct=0.0)
    result = bt.run(df, save=False)
    buy_and_hold_pct = (close[-1] - close[0]) / close[0] * 100.0
    final_pct = (result['final_capital'] - 1000.0) / 1000.0 * 100.0
    assert final_pct > buy_and_hold_pct + 20.0, f'Forward-peek signal should inflate returns past buy-and-hold (documented limit). Got {final_pct:.1f}% vs BAH {buy_and_hold_pct:.1f}%. If this assertion fails, the engine has gained forward-peek detection — update the test and the look-ahead contract docstring at the top of backtest/backtester.py.'

def test_shift_moves_signal_by_exactly_one_row():
    df = _step_up_df(n=20, jump_bar=15)
    df['signal'] = 0
    df.iloc[5, df.columns.get_loc('signal')] = 1
    df.iloc[10, df.columns.get_loc('signal')] = -1
    bt = Backtester(initial_capital=1000.0, commission_pct=0.0, slippage_pct=0.0)
    result = bt.run(df, save=False)
    assert result['total_trades'] == 1
    entry_date = pd.Timestamp(result['trades'][0]['entry_date'])
    exit_date = pd.Timestamp(result['trades'][0]['exit_date'])
    assert entry_date == df.index[6], f'Entry should be at bar 6, got {entry_date}'
    assert exit_date == df.index[11], f'Exit should be at bar 11, got {exit_date}'

def test_zscore_target_close_uses_closed_bar_z_and_fills_next_open():
    import pandas as pd
    n = 12
    idx = pd.date_range('2024-01-01', periods=n, freq='h')
    closes = [100.0] * 6 + [100.0, 100.0, 130.0, 130.0, 130.0, 130.0]
    spike_bar = 8
    df = pd.DataFrame({'open': closes, 'high': closes, 'low': closes, 'close': closes, 'open_action': ['long'] + ['none'] * (n - 1)}, index=idx)
    bt = Backtester(initial_capital=1000.0, commission_pct=0.0, slippage_pct=0.0, open_strategy={'name': 'x'}, close_strategies=[{'name': 'zscore_target', 'params': {'lookback': 4, 'z_target': 1.0}}], direction='long')
    result = bt.run(df, strategy_name='x', save=False)
    assert result['total_trades'] == 1
    exit_date = pd.Timestamp(result['trades'][0]['exit_date'])
    assert exit_date > df.index[spike_bar], f'exit {exit_date} must be after the spike bar {df.index[spike_bar]} (next-open fill, not intrabar)'
    assert result['trades'][0]['exit_reason'].startswith('zscore_target:')
sys.path.insert(0, str(pathlib.Path(__file__).parent.parent.parent / 'shared_strategies' / 'open'))
from anchored_vwap import anchored_vwap_core
_AVWAP_PARAMS = dict(pivot_strength=2, confirm_bars=2, atr_period=3)

def _avwap_mixed_fixture() -> pd.DataFrame:
    seg = [110, 108, 106, 104, 102, 100, 100.5, 100.2, 99.8, 99.5, 103.5, 104, 104.5, 105, 105.5, 106, 108, 110, 109.5, 109, 108.5, 104.5, 104, 103.5, 103]
    closes = np.array(seg, dtype=float)
    idx = pd.date_range('2026-01-01', periods=len(closes), freq='1h')
    return pd.DataFrame({'open': closes, 'high': closes + 0.5, 'low': closes - 0.5, 'close': closes, 'volume': np.full(len(closes), 10.0)}, index=idx)

def test_anchored_vwap_no_lookahead():
    df = _avwap_mixed_fixture()
    cut = 20

    def real(d):
        return anchored_vwap_core(d, **_AVWAP_PARAMS)['signal'].to_numpy()

    def forward_peeking(d):
        s = anchored_vwap_core(d, **_AVWAP_PARAMS)['signal'].to_numpy().copy()
        if len(s) > 1:
            s[:-1] = s[1:]
        return s
    full = real(df)
    assert (full != 0).any(), 'fixture is vacuous — no signal to guard'
    assert (full == 1).any() and (full == -1).any(), 'expected a +1 and a -1'
    trunc = real(df.iloc[:cut])
    assert np.array_equal(full[:cut], trunc), 'signals < cut must not depend on future bars'
    bf = forward_peeking(df)
    bt = forward_peeking(df.iloc[:cut])
    assert not np.array_equal(bf[:cut], bt), 'forward-peeking variant should break truncation-invariance — the test is not sensitive to look-ahead'
from anchored_vwap_channel import anchored_vwap_channel_core
_AVWAP_CHANNEL_PARAMS = dict(pivot_strength=2, buffer_atr_mult=0.0, confirm_bars=2, min_width_atr_mult=0.0, atr_period=3)

def _avwap_channel_mixed_fixture() -> pd.DataFrame:
    closes = np.array([104, 106, 108, 106, 104, 102, 100, 102, 104, 103, 102.5, 103.5, 104.5, 106.0, 104.8, 104.2, 104.6, 104.0], dtype=float)
    lows = closes - 0.5
    lows[10] = 101.0
    highs = closes + 0.5
    highs[16] = 105.8
    idx = pd.date_range('2026-01-01', periods=len(closes), freq='1h')
    return pd.DataFrame({'open': closes, 'high': highs, 'low': lows, 'close': closes, 'volume': np.full(len(closes), 10.0)}, index=idx)

def test_anchored_vwap_channel_no_lookahead():
    df = _avwap_channel_mixed_fixture()
    cut = 17

    def real(d):
        return anchored_vwap_channel_core(d, **_AVWAP_CHANNEL_PARAMS)['signal'].to_numpy()

    def forward_peeking(d):
        s = anchored_vwap_channel_core(d, **_AVWAP_CHANNEL_PARAMS)['signal'].to_numpy().copy()
        if len(s) > 1:
            s[:-1] = s[1:]
        return s
    full = real(df)
    assert (full != 0).any(), 'fixture is vacuous — no signal to guard'
    assert (full == 1).any() and (full == -1).any(), 'expected a +1 and a -1'
    trunc = real(df.iloc[:cut])
    assert np.array_equal(full[:cut], trunc), 'signals < cut must not depend on future bars'
    for c in range(3, len(df)):
        assert np.array_equal(full[:c], real(df.iloc[:c])), f'prefix changed at cut {c}'
    bf = forward_peeking(df)
    bt = forward_peeking(df.iloc[:cut])
    assert not np.array_equal(bf[:cut], bt), 'forward-peeking variant should break truncation-invariance — the test is not sensitive to look-ahead'
from anchored_vwap_reversion import anchored_vwap_reversion_core
_AVWAP_REVERSION_PARAMS = dict(pivot_strength=2, entry_atr_mult=1.0, buffer_atr_mult=0.0, confirm_bars=2, atr_period=3)

def _avwap_reversion_mixed_fixture() -> pd.DataFrame:
    closes = np.array([110, 108, 106, 104, 102, 100, 101, 102, 101, 100.2, 100.6, 103, 105, 107, 106, 105, 106.5, 106.3], dtype=float)
    lows = closes - 0.5
    lows[9] = 99.0
    highs = closes + 0.5
    highs[16] = 108.5
    idx = pd.date_range('2026-01-01', periods=len(closes), freq='1h')
    return pd.DataFrame({'open': closes, 'high': highs, 'low': lows, 'close': closes, 'volume': np.full(len(closes), 10.0)}, index=idx)

def test_anchored_vwap_reversion_no_lookahead():
    df = _avwap_reversion_mixed_fixture()
    cut = 17

    def real(d):
        return anchored_vwap_reversion_core(d, **_AVWAP_REVERSION_PARAMS)['signal'].to_numpy()

    def forward_peeking(d):
        s = anchored_vwap_reversion_core(d, **_AVWAP_REVERSION_PARAMS)['signal'].to_numpy().copy()
        if len(s) > 1:
            s[:-1] = s[1:]
        return s
    full = real(df)
    assert (full != 0).any(), 'fixture is vacuous — no signal to guard'
    assert (full == 1).any() and (full == -1).any(), 'expected a +1 and a -1'
    trunc = real(df.iloc[:cut])
    assert np.array_equal(full[:cut], trunc), 'signals < cut must not depend on future bars'
    for c in range(3, len(df)):
        assert np.array_equal(full[:c], real(df.iloc[:c])), f'prefix changed at cut {c}'
    bf = forward_peeking(df)
    bt = forward_peeking(df.iloc[:cut])
    assert not np.array_equal(bf[:cut], bt), 'forward-peeking variant should break truncation-invariance — the test is not sensitive to look-ahead'

def test_entry_atr_stamped_from_bar_before_fill():
    idx = pd.date_range('2024-01-01', periods=5, freq='D')
    df = pd.DataFrame({'open': [100, 100, 100, 110, 110], 'close': [100, 100, 110, 110, 110], 'atr': [5, 20, 20, 20, 20], 'open_action': ['long', 'none', 'none', 'none', 'none']}, index=idx)
    bt = Backtester(initial_capital=1000, commission_pct=0, slippage_pct=0, close_strategies=[{'name': 'tiered_tp_atr', 'params': {'tp_tiers': [{'atr_multiple': 1.0, 'close_fraction': 1.0}]}}])
    result = bt.run(df, save=False)
    assert result['total_trades'] == 1
    assert result['trades'][0]['exit_price'] == 110.0
    assert result['trades'][0]['exit_date'] == str(idx[3])

def test_entry_atr_stamp_no_prior_bar_returns_zero():
    bt = Backtester(initial_capital=1000, commission_pct=0, slippage_pct=0)
    idx = pd.date_range('2024-01-01', periods=3, freq='D')
    atr = pd.Series([5.0, 6.0, 7.0], index=idx)
    assert bt._stamp_entry_atr(atr, idx[0], 100.0) == 0.0
    assert bt._stamp_entry_atr(atr, idx[2], 100.0) == 6.0

def test_scale_in_add_fills_at_next_bar_open():
    idx = pd.date_range('2024-01-01', periods=6, freq='D')
    df = pd.DataFrame({'open': [100.0, 100.0, 100.0, 108.0, 108.0, 108.0], 'high': [100.5, 100.5, 108.0, 108.5, 108.5, 108.5], 'low': [99.5, 99.5, 99.5, 107.5, 107.5, 107.5], 'close': [100.0, 100.0, 108.0, 108.0, 108.0, 108.0], 'signal': [1, 0, 1, 0, -1, 0]}, index=idx)
    bt = Backtester(initial_capital=10000, commission_pct=0, slippage_pct=0, allow_scale_in=True)
    result = bt.run(df, save=False)
    assert result['scale_in_adds'] == 1
    trade, = result['trades']
    add_qty = 10000.0 / 108.0
    blend = (100.0 * 100.0 + add_qty * 108.0) / (100.0 + add_qty)
    assert trade['shares'] == pytest.approx(100.0 + add_qty)
    assert trade['entry_price'] == pytest.approx(blend)

def test_scale_in_decision_ignores_fill_bar_range():

    def _res(fill_bar_close, fill_bar_high):
        idx = pd.date_range('2024-01-01', periods=6, freq='D')
        df = pd.DataFrame({'open': [100.0, 100.0, 100.0, 108.0, 108.0, 108.0], 'high': [100.5, 100.5, 108.0, fill_bar_high, 130.0, 130.0], 'low': [99.5, 99.5, 99.5, 107.5, 107.5, 107.5], 'close': [100.0, 100.0, 108.0, fill_bar_close, 120.0, 120.0], 'signal': [1, 0, 1, 0, -1, 0]}, index=idx)
        bt = Backtester(initial_capital=10000, commission_pct=0, slippage_pct=0, allow_scale_in=True, scale_in={'add_spacing_atr': 0.0})
        return bt.run(df, save=False)
    base = _res(108.0, 108.5)
    perturbed = _res(125.0, 126.0)
    assert base['scale_in_adds'] == perturbed['scale_in_adds'] == 1
    assert base['trades'][0]['shares'] == pytest.approx(perturbed['trades'][0]['shares'])

def test_scale_in_spacing_gate_reads_signal_bar_close():
    idx = pd.date_range('2024-01-01', periods=6, freq='D')
    df = pd.DataFrame({'open': [100.0, 100.0, 100.0, 104.0, 104.0, 104.0], 'high': [100.5, 100.5, 102.0, 104.5, 104.5, 104.5], 'low': [99.5, 99.5, 99.5, 103.5, 103.5, 103.5], 'close': [100.0, 100.0, 101.9, 104.0, 104.0, 104.0], 'atr': [2.0] * 6, 'signal': [1, 0, 1, 0, 0, 0]}, index=idx)
    bt = Backtester(initial_capital=10000, commission_pct=0, slippage_pct=0, allow_scale_in=True, scale_in={'add_spacing_atr': 1.0})
    result = bt.run(df, save=False)
    assert result['scale_in_adds'] == 0
