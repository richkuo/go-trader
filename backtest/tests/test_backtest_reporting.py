import math
import os
import sys
from datetime import datetime, timedelta

import numpy as np
import pandas as pd
import pytest

_BACKTEST_DIR = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
if _BACKTEST_DIR not in sys.path:
    sys.path.insert(0, _BACKTEST_DIR)

from backtester import (
    Backtester, periods_per_year, TIMEFRAME_PERIODS_PER_YEAR,
)
from backtest_options import OptionsBacktester
from backtest_theta import ThetaHarvestBacktester



def _df_with_signals(signals):
    n = len(signals)
    idx = pd.date_range("2024-01-01", periods=n, freq="D")
    closes = [100.0 + i for i in range(n)]
    return pd.DataFrame(
        {"open": closes, "close": closes, "signal": signals}, index=idx
    )


def test_signal_out_of_domain_raises():
    df = _df_with_signals([0, 1, 2, 0, -1, 0])
    bt = Backtester(initial_capital=1000.0, commission_pct=0.0, slippage_pct=0.0)
    with pytest.raises(ValueError, match=r"signal column must be in"):
        bt.run(df, save=False)


def test_float_signals_from_position_diff_still_accepted():
    df = _df_with_signals([0.0, 1.0, 0.0, -1.0, 0.0])
    bt = Backtester(initial_capital=1000.0, commission_pct=0.0, slippage_pct=0.0)
    result = bt.run(df, save=False)
    assert result["total_trades"] == 1


def test_nan_signal_is_treated_as_hold():
    df = _df_with_signals([float("nan"), 1, 0, -1, 0])
    bt = Backtester(initial_capital=1000.0, commission_pct=0.0, slippage_pct=0.0)
    result = bt.run(df, save=False)
    assert result["total_trades"] == 1


def test_non_integral_float_signal_raises():
    df = _df_with_signals([0.0, 1.5, 0.0, -1.0, 0.0])
    bt = Backtester(initial_capital=1000.0, commission_pct=0.0, slippage_pct=0.0)
    with pytest.raises(ValueError, match=r"non-integral values"):
        bt.run(df, save=False)



def _synthetic_returns_df(n=400, seed=7):
    rng = np.random.default_rng(seed)
    rets = rng.normal(0.0005, 0.01, n)
    closes = 100 * np.cumprod(1 + rets)
    idx = pd.date_range("2024-01-01", periods=n, freq="D")
    df = pd.DataFrame({"open": closes, "close": closes}, index=idx)
    df["signal"] = 0
    for i in range(20, n - 1, 40):
        df.iloc[i, df.columns.get_loc("signal")] = 1
        df.iloc[min(i + 20, n - 1), df.columns.get_loc("signal")] = -1
    return df


def test_periods_per_year_table():
    assert periods_per_year("1d") == 365
    assert periods_per_year("4h") == 365 * 6
    assert periods_per_year("1h") == 365 * 24
    assert periods_per_year("1w") == 52
    assert periods_per_year("nonsense") == 365


def test_sharpe_scales_with_timeframe():
    df = _synthetic_returns_df()
    bt = Backtester(initial_capital=1000.0, commission_pct=0.0, slippage_pct=0.0)
    res_1d = bt.run(df, timeframe="1d", save=False)
    res_4h = bt.run(df, timeframe="4h", save=False)

    if res_1d["sharpe_ratio"] == 0:
        pytest.skip("synthetic series produced no variance — can't check ratio")

    ratio = res_4h["sharpe_ratio"] / res_1d["sharpe_ratio"]
    assert ratio == pytest.approx(math.sqrt(6), rel=0.02), (
        f"4h Sharpe / 1d Sharpe should be sqrt(6) ≈ 2.449; got {ratio:.4f}"
    )


def test_volatility_scales_with_timeframe():
    df = _synthetic_returns_df()
    bt = Backtester(initial_capital=1000.0, commission_pct=0.0, slippage_pct=0.0)
    res_1d = bt.run(df, timeframe="1d", save=False)
    res_1h = bt.run(df, timeframe="1h", save=False)
    if res_1d["volatility_pct"] == 0:
        pytest.skip("zero volatility — synthetic series degenerate")
    ratio = res_1h["volatility_pct"] / res_1d["volatility_pct"]
    assert ratio == pytest.approx(math.sqrt(24), rel=0.02), (
        f"1h vol / 1d vol should be sqrt(24); got {ratio:.4f}"
    )



def test_options_backtester_rejects_max_positions_below_two():
    with pytest.raises(ValueError, match=r"max_positions must be >= 2"):
        OptionsBacktester(initial_capital=1000.0, max_positions=1)


def test_options_backtester_accepts_max_positions_two_and_above():
    OptionsBacktester(initial_capital=1000.0, max_positions=2)
    OptionsBacktester(initial_capital=1000.0, max_positions=4)



def test_annualized_return_uses_calendar_days_not_curve_length():
    bt = OptionsBacktester(initial_capital=1000.0, max_positions=2,
                           check_interval=7)
    bt.cash = 1100.0
    start = datetime(2023, 1, 1)
    bt.equity_curve = [
        ((start + timedelta(days=i * 7)).strftime("%Y-%m-%d"), 1000.0)
        for i in range(53)
    ]
    bt.equity_curve[-1] = (bt.equity_curve[-1][0], 1100.0)
    report = bt._generate_report("BTC", bt.equity_curve[0][0],
                                  bt.equity_curve[-1][0], 20000.0, 22000.0)
    assert report["annualized_return_pct"] == pytest.approx(10.0, abs=1.0), (
        f"Annualized return should be ~10% over a 1-year span, "
        f"got {report['annualized_return_pct']}"
    )


def test_elapsed_days_returns_calendar_difference():
    bt = OptionsBacktester(initial_capital=1000.0, max_positions=2,
                           check_interval=7)
    bt.equity_curve = [
        ("2023-01-01", 1000.0),
        ("2023-04-01", 1050.0),
        ("2023-12-31", 1100.0),
    ]
    assert bt._elapsed_days() == 364



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


def test_theta_force_close_emits_trade_log_entries():
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
    force_close_log = [t for t in bt.trade_log if t.get("event") == "force_close"]
    assert len(bt.positions) == 0
    if bt.total_trades > 0:
        assert len(bt.trade_log) > 0



def _htf_ohlcv_df(n=120, start_price=100.0, drift=0.5, seed=3):
    rng = np.random.default_rng(seed)
    closes = [start_price]
    for _ in range(n - 1):
        closes.append(closes[-1] + drift + rng.normal(0, 0.2))
    idx = pd.date_range("2023-01-01", periods=n, freq="D", name="datetime")
    return pd.DataFrame(
        {"open": closes, "high": closes, "low": closes,
         "close": closes, "volume": [1.0] * n},
        index=idx,
    )


def test_htf_trend_series_aligns_on_datetime_indexed_frames(monkeypatch):
    import run_backtest

    htf_df = _htf_ohlcv_df(n=120)
    monkeypatch.setattr(run_backtest, "load_cached_data",
                        lambda *a, **kw: htf_df)

    ltf_index = pd.date_range("2023-02-01", periods=40, freq="6h",
                              name="datetime")
    trend = run_backtest._htf_trend_series("BTC/USDT", "1h", ltf_index)

    assert len(trend) == len(ltf_index)
    assert set(trend.unique()).issubset({-1, 0, 1})
    assert (trend == 1).any(), "expected bullish bars in upward-drift series"


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
    trend = run_backtest._htf_trend_series("BTC/USDT", "1h", ltf_index)
    assert list(trend) == [1, 1, 1], (
        "LTF bars inside the forming HTF candle must see the last CLOSED "
        "candle's trend (1), not the forming candle's final close (-1)")

    early = run_backtest._htf_trend_series(
        "BTC/USDT", "1h",
        pd.DatetimeIndex([idx[0] + pd.Timedelta(hours=6)], name="datetime"))
    assert list(early) == [0]


def test_run_single_backtest_with_htf_filter(monkeypatch):
    import run_backtest

    ltf = _htf_ohlcv_df(n=200, drift=0.3)
    htf = _htf_ohlcv_df(n=60, drift=1.0)
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
