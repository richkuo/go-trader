import os
import sys
from datetime import datetime, timedelta

import numpy as np
import pandas as pd
import pytest

_BACKTEST_DIR = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
if _BACKTEST_DIR not in sys.path:
    sys.path.insert(0, _BACKTEST_DIR)

from backtester import Backtester
from backtest_options import OptionsBacktester
from backtest_theta import ThetaHarvestBacktester


def _df_with_signals(signals):
    n = len(signals)
    idx = pd.date_range("2024-01-01", periods=n, freq="D")
    closes = [100.0 + i for i in range(n)]
    return pd.DataFrame(
        {"open": closes, "close": closes, "signal": signals}, index=idx
    )


def test_signal_out_of_domain_raises(subtests):
    for case_id, signals, match in [
        ("out_of_domain", [0, 1, 2, 0, -1, 0], r"signal column must be in"),
        ("non_integral_float", [0.0, 1.5, 0.0, -1.0, 0.0], r"non-integral values"),
    ]:
        with subtests.test(case=case_id):
            df = _df_with_signals(signals)
            bt = Backtester(initial_capital=1000.0, commission_pct=0.0, slippage_pct=0.0)
            with pytest.raises(ValueError, match=match):
                bt.run(df, save=False)

    for case_id, signals in [
        ("float_signals_from_position_diff", [0.0, 1.0, 0.0, -1.0, 0.0]),
        ("nan_signal_is_hold", [float("nan"), 1, 0, -1, 0]),
    ]:
        with subtests.test(case=case_id):
            df = _df_with_signals(signals)
            bt = Backtester(initial_capital=1000.0, commission_pct=0.0, slippage_pct=0.0)
            result = bt.run(df, save=False)
            assert result["total_trades"] == 1


def test_options_backtester_rejects_max_positions_below_two():
    with pytest.raises(ValueError, match=r"max_positions must be >= 2"):
        OptionsBacktester(initial_capital=1000.0, max_positions=1)


def _synthetic_candles(n_days=200, start_price=20000.0, vol=0.02, seed=11):
    rng = np.random.default_rng(seed)
    closes = [start_price]
    for _ in range(n_days - 1):
        closes.append(closes[-1] * (1 + rng.normal(0, vol)))
    candles = []
    base = datetime(2023, 1, 1)
    for i, c in enumerate(closes):
        ts_ms = int((base + timedelta(days=i)).timestamp() * 1000)
        candles.append([ts_ms, c, c * 1.01, c * 0.99, c, 1000.0])
    return candles


def test_theta_run_leaves_no_open_positions():
    candles = _synthetic_candles(n_days=200, vol=0.01)
    bt = ThetaHarvestBacktester(
        initial_capital=10_000.0,
        max_positions=2,
        profit_target_pct=0,
        stop_loss_pct=0,
        min_dte_close=0,
        label="test",
    )
    bt.run(candles, "BTC")
    assert len(bt.positions) == 0


def _htf_ohlcv_df(n=120, start_price=100.0, drift=0.5, seed=3, freq="D"):
    rng = np.random.default_rng(seed)
    closes = [start_price]
    for _ in range(n - 1):
        closes.append(closes[-1] + drift + rng.normal(0, 0.2))
    idx = pd.date_range("2023-01-01", periods=n, freq=freq, name="datetime")
    return pd.DataFrame(
        {"open": closes, "high": closes, "low": closes,
         "close": closes, "volume": [1.0] * n},
        index=idx,
    )


def test_htf_trend_series_uses_last_closed_htf_bar_no_lookahead(monkeypatch):
    import run_backtest

    n = 60
    closes = [100.0 + i for i in range(n - 1)] + [1.0]
    idx = pd.date_range("2024-01-01", periods=n, freq="D", name="datetime")
    htf_df = pd.DataFrame(
        {"open": closes, "high": closes, "low": closes,
         "close": closes, "volume": [1.0] * n},
        index=idx,
    )
    monkeypatch.setattr(run_backtest, "load_cached_data",
                        lambda *a, **kw: htf_df)

    ema = htf_df["close"].ewm(span=50, adjust=False).mean()
    assert closes[-2] > ema.iloc[-2]
    assert closes[-1] < ema.iloc[-1]

    ltf_index = pd.DatetimeIndex(
        [idx[-1] + pd.Timedelta(hours=h) for h in (0, 6, 12)],
        name="datetime",
    )
    trend = run_backtest._htf_trend_series("BTC/USDT", "4h", ltf_index)
    assert list(trend) == [1, 1, 1], (
        "LTF bars inside the forming HTF candle must see the last CLOSED "
        "candle's trend (1), not the forming candle's final close (-1)")

    early = run_backtest._htf_trend_series(
        "BTC/USDT", "4h",
        pd.DatetimeIndex([idx[0] + pd.Timedelta(hours=6)], name="datetime"))
    assert list(early) == [0]


def test_run_single_backtest_with_htf_filter(monkeypatch):
    import run_backtest

    ltf = _htf_ohlcv_df(n=200, drift=0.3)
    htf = _htf_ohlcv_df(n=60, drift=1.0, freq="7D")
    calls = {"n": 0}

    def fake_load(symbol, timeframe, **kw):
        calls["n"] += 1
        return ltf if calls["n"] == 1 else htf

    monkeypatch.setattr(run_backtest, "load_cached_data", fake_load)

    result = run_backtest.run_single_backtest(
        strategy_name="sma_crossover",
        symbol="BTC/USDT",
        timeframe="1d",
        since="2023-01-01",
        capital=1000.0,
        htf_filter=True,
    )
    assert result is not None
    assert "total_trades" in result
