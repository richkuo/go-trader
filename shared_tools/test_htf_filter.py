
import numpy as np
import pandas as pd
import pytest

from shared_tools.conftest import load_module

_HTF_FILTER = load_module("_htf_filter_test", __file__.replace("test_htf_filter.py", "htf_filter.py"))
get_default_htf = _HTF_FILTER.get_default_htf
htf_trend_filter = _HTF_FILTER.htf_trend_filter
apply_htf_filter = _HTF_FILTER.apply_htf_filter
_compute_ema = _HTF_FILTER._compute_ema


@pytest.mark.parametrize("timeframe,expected", [
    ("1m", "15m"),
    ("5m", "1h"),
    ("15m", "1h"),
    ("30m", "4h"),
    ("1h", "4h"),
    ("4h", "1d"),
    ("1d", "1w"),
    ("1w", "1M"),
    ("bogus", "4h"),
])
def test_get_default_htf(timeframe, expected):
    assert get_default_htf(timeframe) == expected


class TestComputeEma:
    def test_constant_values(self):
        values = np.array([50.0] * 20)
        ema = _compute_ema(values, 10)
        np.testing.assert_allclose(ema, 50.0, atol=1e-10)

    def test_seeds_with_first_value_and_preserves_length(self):
        values = np.array([10.0, 20.0, 30.0, 40.0, 50.0])
        ema = _compute_ema(values, 3)
        assert ema[0] == 10.0
        assert len(ema) == len(values)

    @pytest.mark.parametrize("values,lags", [
        (np.arange(1.0, 21.0), True),
        (np.arange(20.0, 0.0, -1.0), False),
    ])
    def test_ema_lags_the_trend(self, values, lags):
        ema = _compute_ema(values, 5)
        assert bool(ema[-1] < values[-1]) is lags


def _make_fetch_fn(closes):
    def fetch_fn(symbol, timeframe, limit):
        df = pd.DataFrame({"close": closes[:limit]})
        return df
    return fetch_fn


class TestHtfTrendFilter:
    @pytest.mark.parametrize("closes,kwargs,expected_trend,expected_tf", [
        (list(np.linspace(50, 100, 80)), {}, 1, "4h"),
        (list(np.linspace(100, 50, 80)), {}, -1, "4h"),
        (list(np.linspace(50, 100, 80)), {"htf": "1d"}, 1, "1d"),
        (list(np.linspace(50, 100, 30)), {"ema_period": 10}, 1, "4h"),
    ])
    def test_trend_direction(self, closes, kwargs, expected_trend, expected_tf):
        result = htf_trend_filter("BTC/USDT", "1h", _make_fetch_fn(closes), **kwargs)
        assert result["htf_trend"] == expected_trend
        assert result["htf_timeframe"] == expected_tf
        assert result["htf_close"] > 0
        assert result["htf_ema"] > 0

    def test_insufficient_data_returns_neutral(self):
        closes = list(range(10))
        result = htf_trend_filter("BTC/USDT", "1h", _make_fetch_fn(closes))
        assert result["htf_trend"] == 0
        assert result["htf_ema"] == 0.0
        assert result["htf_close"] == 0.0

    def test_none_data_returns_neutral(self):
        def fetch_fn(symbol, timeframe, limit):
            return None
        result = htf_trend_filter("BTC/USDT", "1h", fetch_fn)
        assert result["htf_trend"] == 0

    def test_fetch_exception_returns_neutral(self):
        def fetch_fn(symbol, timeframe, limit):
            raise ConnectionError("API down")
        result = htf_trend_filter("BTC/USDT", "1h", fetch_fn)
        assert result["htf_trend"] == 0

    def test_flat_data_returns_neutral(self):
        closes = [100.0] * 80
        result = htf_trend_filter("BTC/USDT", "1h", _make_fetch_fn(closes))
        assert result["htf_trend"] == 0


@pytest.mark.parametrize("signal,trend,expected", [
    (1, 1, 1),
    (-1, -1, -1),
    (1, -1, 0),
    (-1, 1, 0),
    (1, 0, 1),
    (-1, 0, -1),
    (0, 1, 0),
    (0, -1, 0),
    (0, 0, 0),
])
def test_apply_htf_filter(signal, trend, expected):
    assert apply_htf_filter(signal, trend) == expected
