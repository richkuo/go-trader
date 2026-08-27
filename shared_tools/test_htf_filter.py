import numpy as np
import pandas as pd
import pytest
from htf_filter import get_default_htf, htf_trend_filter, apply_htf_filter, _compute_ema

class TestGetDefaultHtf:

    def test_known_mappings(self):
        assert get_default_htf('1m') == '15m'
        assert get_default_htf('5m') == '1h'
        assert get_default_htf('15m') == '1h'
        assert get_default_htf('30m') == '4h'
        assert get_default_htf('1h') == '4h'
        assert get_default_htf('4h') == '1d'

    def test_daily_maps_to_weekly(self):
        assert get_default_htf('1d') == '1w'

    def test_weekly_maps_to_monthly(self):
        assert get_default_htf('1w') == '1M'

    def test_unknown_timeframe_returns_4h(self):
        assert get_default_htf('bogus') == '4h'

class TestComputeEma:

    def test_constant_values(self):
        values = np.array([50.0] * 20)
        ema = _compute_ema(values, 10)
        np.testing.assert_allclose(ema, 50.0, atol=1e-10)

    def test_first_value_equals_input(self):
        values = np.array([10.0, 20.0, 30.0, 40.0, 50.0])
        ema = _compute_ema(values, 3)
        assert ema[0] == 10.0

    def test_trending_up(self):
        values = np.arange(1.0, 21.0)
        ema = _compute_ema(values, 5)
        assert ema[-1] < values[-1]

    def test_trending_down(self):
        values = np.arange(20.0, 0.0, -1.0)
        ema = _compute_ema(values, 5)
        assert ema[-1] > values[-1]

    def test_length_matches_input(self):
        values = np.array([1.0, 2.0, 3.0, 4.0, 5.0])
        ema = _compute_ema(values, 3)
        assert len(ema) == len(values)

def _make_fetch_fn(closes):

    def fetch_fn(symbol, timeframe, limit):
        df = pd.DataFrame({'close': closes[:limit]})
        return df
    return fetch_fn

class TestHtfTrendFilter:

    def test_bullish_trend(self):
        closes = list(np.linspace(50, 100, 80))
        result = htf_trend_filter('BTC/USDT', '1h', _make_fetch_fn(closes))
        assert result['htf_trend'] == 1
        assert result['htf_timeframe'] == '4h'
        assert result['htf_close'] > 0
        assert result['htf_ema'] > 0

    def test_bearish_trend(self):
        closes = list(np.linspace(100, 50, 80))
        result = htf_trend_filter('BTC/USDT', '1h', _make_fetch_fn(closes))
        assert result['htf_trend'] == -1

    def test_custom_htf_override(self):
        closes = list(np.linspace(50, 100, 80))
        result = htf_trend_filter('BTC/USDT', '1h', _make_fetch_fn(closes), htf='1d')
        assert result['htf_timeframe'] == '1d'

    def test_insufficient_data_returns_neutral(self):
        closes = list(range(10))
        result = htf_trend_filter('BTC/USDT', '1h', _make_fetch_fn(closes))
        assert result['htf_trend'] == 0
        assert result['htf_ema'] == 0.0
        assert result['htf_close'] == 0.0

    def test_none_data_returns_neutral(self):

        def fetch_fn(symbol, timeframe, limit):
            return None
        result = htf_trend_filter('BTC/USDT', '1h', fetch_fn)
        assert result['htf_trend'] == 0

    def test_fetch_exception_returns_neutral(self):

        def fetch_fn(symbol, timeframe, limit):
            raise ConnectionError('API down')
        result = htf_trend_filter('BTC/USDT', '1h', fetch_fn)
        assert result['htf_trend'] == 0

    def test_flat_data_returns_neutral(self):
        closes = [100.0] * 80
        result = htf_trend_filter('BTC/USDT', '1h', _make_fetch_fn(closes))
        assert result['htf_trend'] == 0

    def test_custom_ema_period(self):
        closes = list(np.linspace(50, 100, 30))
        result = htf_trend_filter('BTC/USDT', '1h', _make_fetch_fn(closes), ema_period=10)
        assert result['htf_trend'] == 1

class TestApplyHtfFilter:

    def test_buy_bull_aligned(self):
        assert apply_htf_filter(1, 1) == 1

    def test_sell_bear_aligned(self):
        assert apply_htf_filter(-1, -1) == -1

    def test_buy_bear_filtered(self):
        assert apply_htf_filter(1, -1) == 0

    def test_sell_bull_filtered(self):
        assert apply_htf_filter(-1, 1) == 0

    def test_neutral_trend_passes_signal(self):
        assert apply_htf_filter(1, 0) == 1
        assert apply_htf_filter(-1, 0) == -1

    def test_no_signal_stays_hold(self):
        assert apply_htf_filter(0, 1) == 0
        assert apply_htf_filter(0, -1) == 0
        assert apply_htf_filter(0, 0) == 0
