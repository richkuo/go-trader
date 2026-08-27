import numpy as np
import pandas as pd
import pytest
from session_breakout import session_breakout_core

def _make_df(bars):
    idx = pd.DatetimeIndex([b[0] for b in bars])
    return pd.DataFrame({'open': [b[1] for b in bars], 'high': [b[2] for b in bars], 'low': [b[3] for b in bars], 'close': [b[4] for b in bars], 'volume': [b[5] for b in bars]}, index=idx)

def test_no_datetime_index_returns_zero_signal():
    df = pd.DataFrame({'open': [100.0] * 10, 'high': [101.0] * 10, 'low': [99.0] * 10, 'close': [100.0] * 10, 'volume': [100.0] * 10})
    result = session_breakout_core(df)
    assert (result['signal'] == 0).all()

def test_empty_df_returns_empty():
    df = pd.DataFrame(columns=['open', 'high', 'low', 'close', 'volume'])
    result = session_breakout_core(df)
    assert 'signal' in result.columns
    assert len(result) == 0

def test_bullish_breakout_requires_volume_spike():
    bars = []
    base = pd.Timestamp('2024-01-01 00:00:00', tz=None)
    for h in range(8):
        bars.append((base + pd.Timedelta(hours=h), 100, 105, 99, 102, 100.0))
    for h in range(8, 24):
        bars.append((base + pd.Timedelta(hours=h), 102, 103, 101, 102, 100.0))
    day2 = base + pd.Timedelta(days=1)
    for h in range(8):
        bars.append((day2 + pd.Timedelta(hours=h), 102, 104, 101, 103, 100.0))
    bars.append((day2 + pd.Timedelta(hours=9), 103, 108, 103, 107, 500.0))
    df = _make_df(bars)
    result = session_breakout_core(df, session='asian', lookback=1, volume_threshold=1.5)
    assert result['signal'].iloc[-1] == 1

def test_breakout_suppressed_without_volume():
    bars = []
    base = pd.Timestamp('2024-01-01 00:00:00')
    for h in range(8):
        bars.append((base + pd.Timedelta(hours=h), 100, 105, 99, 102, 100.0))
    for h in range(8, 24):
        bars.append((base + pd.Timedelta(hours=h), 102, 103, 101, 102, 100.0))
    day2 = base + pd.Timedelta(days=1)
    for h in range(8):
        bars.append((day2 + pd.Timedelta(hours=h), 102, 104, 101, 103, 100.0))
    bars.append((day2 + pd.Timedelta(hours=9), 103, 108, 103, 107, 100.0))
    df = _make_df(bars)
    result = session_breakout_core(df, session='asian', lookback=1, volume_threshold=1.5)
    assert result['signal'].iloc[-1] == 0

def test_bearish_breakout_emits_short_signal():
    bars = []
    base = pd.Timestamp('2024-01-01 00:00:00')
    for h in range(8):
        bars.append((base + pd.Timedelta(hours=h), 102, 105, 100, 103, 100.0))
    for h in range(8, 24):
        bars.append((base + pd.Timedelta(hours=h), 102, 103, 101, 102, 100.0))
    day2 = base + pd.Timedelta(days=1)
    for h in range(8):
        bars.append((day2 + pd.Timedelta(hours=h), 101, 103, 100, 101, 100.0))
    bars.append((day2 + pd.Timedelta(hours=9), 100, 100, 95, 96, 500.0))
    df = _make_df(bars)
    result = session_breakout_core(df, session='asian', lookback=1, volume_threshold=1.5)
    assert result['signal'].iloc[-1] == -1

def test_intra_session_bar_does_not_signal():
    bars = []
    base = pd.Timestamp('2024-01-01 00:00:00')
    for h in range(8):
        bars.append((base + pd.Timedelta(hours=h), 100, 105, 99, 102, 100.0))
    for h in range(8, 24):
        bars.append((base + pd.Timedelta(hours=h), 102, 103, 101, 102, 100.0))
    day2 = base + pd.Timedelta(days=1)
    bars.append((day2 + pd.Timedelta(hours=3), 103, 108, 103, 107, 500.0))
    df = _make_df(bars)
    result = session_breakout_core(df, session='asian', lookback=1, volume_threshold=1.5)
    assert result['signal'].iloc[-1] == 0

def test_signal_not_repeated_across_consecutive_breakout_bars():
    bars = []
    base = pd.Timestamp('2024-01-01 00:00:00')
    for h in range(8):
        bars.append((base + pd.Timedelta(hours=h), 100, 105, 99, 102, 100.0))
    for h in range(8, 24):
        bars.append((base + pd.Timedelta(hours=h), 102, 103, 101, 102, 100.0))
    day2 = base + pd.Timedelta(days=1)
    for h in range(8):
        bars.append((day2 + pd.Timedelta(hours=h), 102, 104, 101, 103, 100.0))
    bars.append((day2 + pd.Timedelta(hours=9), 103, 108, 103, 107, 500.0))
    bars.append((day2 + pd.Timedelta(hours=10), 107, 109, 106, 108, 500.0))
    df = _make_df(bars)
    result = session_breakout_core(df, session='asian', lookback=1, volume_threshold=1.5)
    signals = result['signal'].iloc[-2:].tolist()
    assert signals == [1, 0]

def test_unknown_session_raises():
    rng = np.random.RandomState(0)
    n = 96
    idx = pd.date_range('2024-01-01', periods=n, freq='h')
    closes = 100 + rng.randn(n).cumsum() * 0.1
    df = pd.DataFrame({'open': closes, 'high': closes + 0.5, 'low': closes - 0.5, 'close': closes, 'volume': np.full(n, 100.0)}, index=idx)
    with pytest.raises(ValueError, match='Unknown session'):
        session_breakout_core(df, session='not_a_real_session')
