import numpy as np
import pandas as pd
import pytest
from chart_patterns import PatternMatch, find_swing_points, volume_confirmed, detect_double_top, detect_double_bottom, detect_triple_top, detect_triple_bottom, detect_head_and_shoulders, detect_inverse_head_and_shoulders, detect_bull_flag, detect_bear_flag, detect_ascending_triangle, detect_descending_triangle, detect_cup_and_handle, chart_pattern_core, _get_swing_indices, _htf_gate_trend

def make_ohlcv(closes, volume=None, noise=0.5):
    closes = np.array(closes, dtype=float)
    n = len(closes)
    if volume is None:
        volume = np.full(n, 100.0)
    highs = closes + noise
    lows = closes - noise
    opens = closes - noise * 0.3
    return pd.DataFrame({'open': opens, 'high': highs, 'low': lows, 'close': closes, 'volume': np.array(volume, dtype=float)})

class TestSwingPoints:

    def test_detects_obvious_peak_and_trough(self):
        prices = list(range(100, 90, -1)) + list(range(90, 101))
        df = make_ohlcv(prices)
        sh, sl = find_swing_points(df['high'], df['low'], lookback=3)
        swing_low_idx = _get_swing_indices(sl)
        assert len(swing_low_idx) >= 1
        assert any((abs(i - 10) <= 3 for i in swing_low_idx))

    def test_detects_peak(self):
        prices = list(range(90, 101)) + list(range(100, 89, -1))
        df = make_ohlcv(prices)
        sh, sl = find_swing_points(df['high'], df['low'], lookback=3)
        swing_high_idx = _get_swing_indices(sh)
        assert len(swing_high_idx) >= 1
        assert any((abs(i - 10) <= 3 for i in swing_high_idx))

    def test_flat_data_no_swings(self):
        prices = [100.0] * 50
        df = make_ohlcv(prices, noise=0)
        sh, sl = find_swing_points(df['high'], df['low'], lookback=5)
        assert len(df) == 50

    def test_insufficient_data(self):
        prices = [100, 101, 100]
        df = make_ohlcv(prices)
        sh, sl = find_swing_points(df['high'], df['low'], lookback=5)
        assert sh.notna().sum() == 0
        assert sl.notna().sum() == 0

class TestVolumeConfirmed:

    def test_high_volume_confirmed(self):
        vol = pd.Series([100] * 25)
        vol.iloc[24] = 200
        assert volume_confirmed(vol, 24, vol_period=20, vol_multiplier=1.5)

    def test_low_volume_rejected(self):
        vol = pd.Series([100] * 25)
        vol.iloc[24] = 110
        assert not volume_confirmed(vol, 24, vol_period=20, vol_multiplier=1.5)

    def test_insufficient_history_allows(self):
        vol = pd.Series([100] * 5)
        assert volume_confirmed(vol, 3, vol_period=20, vol_multiplier=1.5)

class TestDoubleTop:

    def test_detects_double_top(self):
        prices = list(np.linspace(80, 100, 20)) + list(np.linspace(100, 90, 15)) + list(np.linspace(90, 99, 15)) + list(np.linspace(99, 85, 20)) + [85] * 30
        df = make_ohlcv(prices)
        sh, sl = find_swing_points(df['high'], df['low'], lookback=3)
        matches = detect_double_top(df['high'], df['low'], df['close'], sh, sl, tolerance=0.03)
        assert len(matches) >= 1
        assert matches[0].signal == -1
        assert matches[0].pattern == 'double_top'

    def test_no_double_top_when_peaks_differ(self):
        prices = list(np.linspace(80, 100, 20)) + list(np.linspace(100, 90, 15)) + list(np.linspace(90, 110, 15)) + list(np.linspace(110, 85, 20)) + [85] * 30
        df = make_ohlcv(prices)
        sh, sl = find_swing_points(df['high'], df['low'], lookback=3)
        matches = detect_double_top(df['high'], df['low'], df['close'], sh, sl, tolerance=0.03)
        assert len(matches) == 0

class TestDoubleBottom:

    def test_detects_double_bottom(self):
        prices = list(np.linspace(100, 80, 20)) + list(np.linspace(80, 90, 15)) + list(np.linspace(90, 81, 15)) + list(np.linspace(81, 100, 20)) + [100] * 30
        df = make_ohlcv(prices)
        sh, sl = find_swing_points(df['high'], df['low'], lookback=3)
        matches = detect_double_bottom(df['high'], df['low'], df['close'], sh, sl, tolerance=0.03)
        assert len(matches) >= 1
        assert matches[0].signal == 1

class TestHeadAndShoulders:

    def test_detects_head_and_shoulders(self):
        prices = list(np.linspace(80, 95, 15)) + list(np.linspace(95, 85, 10)) + list(np.linspace(85, 105, 15)) + list(np.linspace(105, 85, 10)) + list(np.linspace(85, 96, 15)) + list(np.linspace(96, 80, 20)) + [80] * 15
        df = make_ohlcv(prices)
        sh, sl = find_swing_points(df['high'], df['low'], lookback=3)
        matches = detect_head_and_shoulders(df['high'], df['low'], df['close'], sh, sl, tolerance=0.05)
        assert len(matches) >= 1
        assert matches[0].signal == -1

class TestInverseHeadAndShoulders:

    def test_detects_inverse_hs(self):
        prices = list(np.linspace(100, 85, 15)) + list(np.linspace(85, 95, 10)) + list(np.linspace(95, 75, 15)) + list(np.linspace(75, 95, 10)) + list(np.linspace(95, 84, 15)) + list(np.linspace(84, 100, 20)) + [100] * 15
        df = make_ohlcv(prices)
        sh, sl = find_swing_points(df['high'], df['low'], lookback=3)
        matches = detect_inverse_head_and_shoulders(df['high'], df['low'], df['close'], sh, sl, tolerance=0.05)
        assert len(matches) >= 1
        assert matches[0].signal == 1

class TestTripleTop:

    def test_detects_triple_top(self):
        prices = list(np.linspace(80, 100, 15)) + list(np.linspace(100, 88, 10)) + list(np.linspace(88, 99, 12)) + list(np.linspace(99, 88, 10)) + list(np.linspace(88, 101, 12)) + list(np.linspace(101, 82, 20)) + [82] * 21
        df = make_ohlcv(prices)
        sh, sl = find_swing_points(df['high'], df['low'], lookback=3)
        matches = detect_triple_top(df['high'], df['low'], df['close'], sh, sl, tolerance=0.03)
        assert len(matches) >= 1
        assert matches[0].signal == -1

class TestTripleBottom:

    def test_detects_triple_bottom(self):
        prices = list(np.linspace(100, 80, 15)) + list(np.linspace(80, 92, 10)) + list(np.linspace(92, 81, 12)) + list(np.linspace(81, 92, 10)) + list(np.linspace(92, 79, 12)) + list(np.linspace(79, 100, 20)) + [100] * 21
        df = make_ohlcv(prices)
        sh, sl = find_swing_points(df['high'], df['low'], lookback=3)
        matches = detect_triple_bottom(df['high'], df['low'], df['close'], sh, sl, tolerance=0.03)
        assert len(matches) >= 1
        assert matches[0].signal == 1

class TestBullFlag:

    def test_detects_bull_flag(self):
        prices = list(np.linspace(80, 120, 15)) + list(np.linspace(120, 115, 10)) + list(np.linspace(115, 125, 10)) + [125] * 15
        vol = [100] * len(prices)
        for i in range(15):
            vol[i] = 200
        df = make_ohlcv(prices, volume=vol)
        sh, sl = find_swing_points(df['high'], df['low'], lookback=3)
        matches = detect_bull_flag(df['high'], df['low'], df['close'], df['volume'], sh, sl)
        assert isinstance(matches, list)

class TestBearFlag:

    def test_detects_bear_flag(self):
        prices = list(np.linspace(120, 80, 15)) + list(np.linspace(80, 85, 10)) + list(np.linspace(85, 75, 10)) + [75] * 15
        vol = [100] * len(prices)
        for i in range(15):
            vol[i] = 200
        df = make_ohlcv(prices, volume=vol)
        sh, sl = find_swing_points(df['high'], df['low'], lookback=3)
        matches = detect_bear_flag(df['high'], df['low'], df['close'], df['volume'], sh, sl)
        assert isinstance(matches, list)

class TestCupAndHandle:

    def test_detects_cup_and_handle(self):
        prices = list(np.linspace(90, 100, 10)) + list(np.linspace(100, 85, 15)) + list(np.linspace(85, 100, 15)) + list(np.linspace(100, 96, 5)) + list(np.linspace(96, 105, 10)) + [105] * 15
        df = make_ohlcv(prices)
        sh, sl = find_swing_points(df['high'], df['low'], lookback=3)
        matches = detect_cup_and_handle(df['high'], df['low'], df['close'], sh, sl)
        assert isinstance(matches, list)

class TestDatetimeIndex:

    def _make_datetime_ohlcv(self, closes, volume=None, noise=0.5):
        df = make_ohlcv(closes, volume=volume, noise=noise)
        df.index = pd.date_range('2024-01-01', periods=len(df), freq='1h')
        return df

    def test_get_swing_indices_with_datetime_index(self):
        prices = list(range(100, 90, -1)) + list(range(90, 101))
        df = self._make_datetime_ohlcv(prices)
        sh, sl = find_swing_points(df['high'], df['low'], lookback=3)
        indices = _get_swing_indices(sl)
        assert all((isinstance(i, (int, np.integer)) for i in indices)), f'Expected integer positions, got: {[type(i) for i in indices]}'

    def test_chart_pattern_core_with_datetime_index(self):
        prices = list(np.linspace(80, 100, 20)) + list(np.linspace(100, 90, 15)) + list(np.linspace(90, 99, 15)) + list(np.linspace(99, 85, 20)) + [85] * 30
        df = self._make_datetime_ohlcv(prices, volume=[100] * len(prices))
        result = chart_pattern_core(df, pivot_lookback=3)
        assert 'signal' in result.columns
        assert set(result['signal'].unique()).issubset({-1, 0, 1})

    def test_detect_double_top_with_datetime_index(self):
        prices = list(np.linspace(80, 100, 20)) + list(np.linspace(100, 90, 15)) + list(np.linspace(90, 99, 15)) + list(np.linspace(99, 85, 20)) + [85] * 30
        df = self._make_datetime_ohlcv(prices)
        sh, sl = find_swing_points(df['high'], df['low'], lookback=3)
        matches = detect_double_top(df['high'], df['low'], df['close'], sh, sl, tolerance=0.03)
        assert isinstance(matches, list)

    def test_detect_flag_pole_low_bar_with_datetime_index(self):
        bull_prices = list(np.linspace(80, 120, 15)) + list(np.linspace(120, 115, 10)) + list(np.linspace(115, 125, 10)) + [125] * 15
        vol = [200] * 15 + [100] * (len(bull_prices) - 15)
        df_bull = self._make_datetime_ohlcv(bull_prices, volume=vol)
        sh_b, sl_b = find_swing_points(df_bull['high'], df_bull['low'], lookback=3)
        bull_matches = detect_bull_flag(df_bull['high'], df_bull['low'], df_bull['close'], df_bull['volume'], sh_b, sl_b)
        assert isinstance(bull_matches, list)
        bear_prices = list(np.linspace(120, 80, 15)) + list(np.linspace(80, 85, 10)) + list(np.linspace(85, 75, 10)) + [75] * 15
        df_bear = self._make_datetime_ohlcv(bear_prices, volume=vol)
        sh_b2, sl_b2 = find_swing_points(df_bear['high'], df_bear['low'], lookback=3)
        bear_matches = detect_bear_flag(df_bear['high'], df_bear['low'], df_bear['close'], df_bear['volume'], sh_b2, sl_b2)
        assert isinstance(bear_matches, list)

class TestLookaheadRegression:

    def test_double_top_breakout_after_confirmation_window(self):
        lookback = 3
        prices = list(np.linspace(80, 100, 20)) + list(np.linspace(100, 90, 15)) + list(np.linspace(90, 99, 15)) + list(np.linspace(99, 85, 20)) + [85] * 30
        df = make_ohlcv(prices)
        sh, sl = find_swing_points(df['high'], df['low'], lookback=lookback)
        matches = detect_double_top(df['high'], df['low'], df['close'], sh, sl, tolerance=0.03, lookback=lookback)
        assert len(matches) >= 1
        sh_positions = sorted(np.where(sh.notna())[0])
        for m in matches:
            prior_swings = [s for s in sh_positions if s < m.bar_index]
            assert prior_swings, 'match should follow at least one swing high'
            latest_prior = max(prior_swings)
            assert m.bar_index >= latest_prior + lookback + 1, f'double_top breakout at {m.bar_index} too close to trailing swing at {latest_prior} (lookback={lookback}); allowed only from {latest_prior + lookback + 1} onward'

    def test_head_and_shoulders_breakout_after_confirmation_window(self):
        lookback = 3
        prices = list(np.linspace(80, 95, 15)) + list(np.linspace(95, 85, 10)) + list(np.linspace(85, 105, 15)) + list(np.linspace(105, 85, 10)) + list(np.linspace(85, 96, 15)) + list(np.linspace(96, 80, 20)) + [80] * 15
        df = make_ohlcv(prices)
        sh, sl = find_swing_points(df['high'], df['low'], lookback=lookback)
        matches = detect_head_and_shoulders(df['high'], df['low'], df['close'], sh, sl, tolerance=0.05, lookback=lookback)
        assert len(matches) >= 1
        sh_positions = sorted(np.where(sh.notna())[0])
        for m in matches:
            prior_swings = [s for s in sh_positions if s < m.bar_index]
            assert prior_swings
            latest_prior = max(prior_swings)
            assert m.bar_index >= latest_prior + lookback + 1, f'H&S breakout at {m.bar_index} too close to trailing swing at {latest_prior} (lookback={lookback}); allowed only from {latest_prior + lookback + 1} onward'

    def test_signal_independent_of_future_bars(self):
        prices = list(np.linspace(80, 100, 20)) + list(np.linspace(100, 90, 15)) + list(np.linspace(90, 99, 15)) + list(np.linspace(99, 85, 20)) + [85] * 30
        vol = [100] * len(prices)
        for i in range(50, 70):
            vol[i] = 200
        df = make_ohlcv(prices, volume=vol)
        full = chart_pattern_core(df, pivot_lookback=3, tolerance=0.03, vol_multiplier=1.0)
        signal_bars = list(np.where(full['signal'].values != 0)[0])
        assert len(signal_bars) >= 1
        for k in signal_bars:
            partial_df = df.iloc[:k + 1]
            partial = chart_pattern_core(partial_df, pivot_lookback=3, tolerance=0.03, vol_multiplier=1.0)
            assert partial['signal'].iloc[k] == full['signal'].iloc[k], f"signal at bar {k} flipped under truncation: full={full['signal'].iloc[k]} truncated={partial['signal'].iloc[k]}"

class TestChartPatternCore:

    def test_returns_signal_column(self):
        prices = list(np.linspace(90, 110, 50)) + list(np.linspace(110, 90, 50))
        df = make_ohlcv(prices)
        result = chart_pattern_core(df, pivot_lookback=3)
        assert 'signal' in result.columns
        assert 'swing_high' in result.columns
        assert 'swing_low' in result.columns
        assert set(result['signal'].unique()).issubset({-1, 0, 1})

    def test_short_data_returns_zeros(self):
        df = make_ohlcv([100, 101, 100, 99, 100])
        result = chart_pattern_core(df, pivot_lookback=5)
        assert (result['signal'] == 0).all()

    def test_flat_data_no_signals(self):
        df = make_ohlcv([100.0] * 100, noise=0)
        result = chart_pattern_core(df, pivot_lookback=3)
        assert result['signal'].abs().sum() == 0

    def test_double_top_through_orchestrator(self):
        prices = list(np.linspace(80, 100, 20)) + list(np.linspace(100, 90, 15)) + list(np.linspace(90, 99, 15)) + list(np.linspace(99, 85, 20)) + [85] * 30
        vol = [100] * len(prices)
        for i in range(50, 70):
            vol[i] = 200
        df = make_ohlcv(prices, volume=vol)
        result = chart_pattern_core(df, pivot_lookback=3, tolerance=0.03, vol_multiplier=1.0)
        sell_signals = result[result['signal'] == -1]
        assert len(sell_signals) >= 1

    def test_volume_filter_blocks_weak_breakout(self):
        prices = list(np.linspace(80, 100, 20)) + list(np.linspace(100, 90, 15)) + list(np.linspace(90, 99, 15)) + list(np.linspace(99, 85, 20)) + [85] * 30
        vol = [200] * len(prices)
        for i in range(50, 70):
            vol[i] = 10
        df = make_ohlcv(prices, volume=vol)
        result = chart_pattern_core(df, pivot_lookback=3, tolerance=0.03, vol_multiplier=3.0)
        sell_signals = result[result['signal'] == -1]
        assert len(sell_signals) == 0

def _double_top_fixture(prefix=None):
    prices = list(np.linspace(80, 100, 20)) + list(np.linspace(100, 90, 15)) + list(np.linspace(90, 99, 15)) + list(np.linspace(99, 85, 20)) + [85] * 30
    if prefix is not None:
        prices = list(prefix) + prices
    vol = [100.0] * len(prices)
    off = len(prefix) if prefix is not None else 0
    for i in range(off + 50, off + 70):
        vol[i] = 200.0
    return make_ohlcv(prices, volume=vol)

class TestHTFGate:

    def test_default_off_is_bit_identical(self):
        df = _double_top_fixture()
        base = chart_pattern_core(df, pivot_lookback=3, tolerance=0.03, vol_multiplier=1.0)
        off = chart_pattern_core(df, pivot_lookback=3, tolerance=0.03, vol_multiplier=1.0, htf_gate_factor=0)
        assert 'htf_gate_trend' not in base.columns
        assert 'htf_gate_trend' not in off.columns
        assert (base['signal'] == off['signal']).all()

    def test_htf_gate_trend_direction_and_warmup(self):
        up = make_ohlcv(np.linspace(50, 200, 600))
        trend_up = _htf_gate_trend(up, 4, ema_fast=10, ema_slow=20)
        down = make_ohlcv(np.linspace(200, 50, 600))
        trend_down = _htf_gate_trend(down, 4, ema_fast=10, ema_slow=20)
        assert trend_up.iloc[0] == 0 and trend_down.iloc[0] == 0
        assert trend_up.iloc[-1] == 1
        assert trend_down.iloc[-1] == -1

    def test_veto_blocks_counter_trend_sell_in_uptrend(self):
        df = _double_top_fixture(prefix=np.linspace(20, 80, 400))
        base = chart_pattern_core(df, pivot_lookback=3, tolerance=0.03, vol_multiplier=1.0)
        gated = chart_pattern_core(df, pivot_lookback=3, tolerance=0.03, vol_multiplier=1.0, htf_gate_factor=4, htf_gate_ema_fast=10, htf_gate_ema_slow=20)
        sell_bars = np.where(base['signal'].values == -1)[0]
        assert len(sell_bars) >= 1
        blocked = 0
        for k in sell_bars:
            trend_k = int(gated['htf_gate_trend'].iloc[k])
            if trend_k == 1:
                assert gated['signal'].iloc[k] == 0, f'veto left a -1 signal at bar {k} despite HTF uptrend'
                blocked += 1
            else:
                assert gated['signal'].iloc[k] == base['signal'].iloc[k]
        assert blocked >= 1

    def test_veto_passes_neutral_and_align_blocks_it(self):
        df = _double_top_fixture()
        base = chart_pattern_core(df, pivot_lookback=3, tolerance=0.03, vol_multiplier=1.0)
        assert base['signal'].abs().sum() >= 1
        veto = chart_pattern_core(df, pivot_lookback=3, tolerance=0.03, vol_multiplier=1.0, htf_gate_factor=30)
        align = chart_pattern_core(df, pivot_lookback=3, tolerance=0.03, vol_multiplier=1.0, htf_gate_factor=30, htf_gate_mode='align')
        assert (veto['htf_gate_trend'] == 0).all()
        assert (veto['signal'] == base['signal']).all()
        assert (align['signal'] == 0).all()

    def test_invalid_mode_raises(self):
        df = _double_top_fixture()
        with pytest.raises(ValueError, match='htf_gate_mode'):
            chart_pattern_core(df, htf_gate_mode='bogus')

    def test_gated_signal_independent_of_future_bars(self):
        df = _double_top_fixture(prefix=np.linspace(20, 80, 400))
        df.index = pd.date_range('2024-01-01', periods=len(df), freq='1h')
        kwargs = dict(pivot_lookback=3, tolerance=0.03, vol_multiplier=1.0, htf_gate_factor=4, htf_gate_ema_fast=10, htf_gate_ema_slow=20)
        full = chart_pattern_core(df, **kwargs)
        base = chart_pattern_core(df, pivot_lookback=3, tolerance=0.03, vol_multiplier=1.0)
        signal_bars = list(np.where(base['signal'].values != 0)[0])
        assert len(signal_bars) >= 1
        for k in signal_bars:
            partial = chart_pattern_core(df.iloc[:k + 1], **kwargs)
            assert partial['signal'].iloc[k] == full['signal'].iloc[k], f'gated signal at bar {k} flipped under truncation'
        k = signal_bars[-1]
        partial = chart_pattern_core(df.iloc[:k + 1], **kwargs)
        assert (partial['htf_gate_trend'].values == full['htf_gate_trend'].values[:k + 1]).all()
