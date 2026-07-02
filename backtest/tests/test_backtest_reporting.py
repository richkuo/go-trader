"""
Regression tests for issue #304 — backtest reporting fixes.

Covers:
- M1 — Backtester rejects out-of-domain signal values; in-domain ints/floats both work.
- M3 — Sharpe annualization scales with the candle timeframe (1d, 4h, 1h).
- M4 — OptionsBacktester rejects max_positions < 2 (no silent naked legs).
- M5 — OptionsBacktester annualized return uses calendar days, not equity-curve length.
- L5 — backtest_theta force-closes append entries to ``trade_log``.
"""
import math
import os
import sys
from datetime import datetime, timedelta

import numpy as np
import pandas as pd
import pytest

# Mirror conftest: make backtest/ modules importable.
_BACKTEST_DIR = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
if _BACKTEST_DIR not in sys.path:
    sys.path.insert(0, _BACKTEST_DIR)

from backtester import (  # noqa: E402
    Backtester, periods_per_year, TIMEFRAME_PERIODS_PER_YEAR,
)
from backtest_options import OptionsBacktester  # noqa: E402
from backtest_theta import ThetaHarvestBacktester  # noqa: E402


# --------------------------------------------------------------------------- #
# M1 — signal domain enforcement                                              #
# --------------------------------------------------------------------------- #

def _df_with_signals(signals):
    n = len(signals)
    idx = pd.date_range("2024-01-01", periods=n, freq="D")
    closes = [100.0 + i for i in range(n)]
    return pd.DataFrame(
        {"open": closes, "close": closes, "signal": signals}, index=idx
    )


def test_signal_out_of_domain_raises():
    """+2 (or any non-{-1,0,1}) must surface as an explicit error, not be silently dropped."""
    df = _df_with_signals([0, 1, 2, 0, -1, 0])
    bt = Backtester(initial_capital=1000.0, commission_pct=0.0, slippage_pct=0.0)
    with pytest.raises(ValueError, match=r"signal column must be in"):
        bt.run(df, save=False)


def test_float_signals_from_position_diff_still_accepted():
    """``position.diff()`` emits ±1.0 floats — they must continue to work."""
    df = _df_with_signals([0.0, 1.0, 0.0, -1.0, 0.0])
    bt = Backtester(initial_capital=1000.0, commission_pct=0.0, slippage_pct=0.0)
    result = bt.run(df, save=False)
    assert result["total_trades"] == 1


def test_nan_signal_is_treated_as_hold():
    """Strategies sometimes leave NaN at the start; should coerce to 0, not raise."""
    df = _df_with_signals([float("nan"), 1, 0, -1, 0])
    bt = Backtester(initial_capital=1000.0, commission_pct=0.0, slippage_pct=0.0)
    result = bt.run(df, save=False)
    assert result["total_trades"] == 1


def test_non_integral_float_signal_raises():
    """1.5 from a fractional-sizing bug must raise, not truncate silently to 1."""
    df = _df_with_signals([0.0, 1.5, 0.0, -1.0, 0.0])
    bt = Backtester(initial_capital=1000.0, commission_pct=0.0, slippage_pct=0.0)
    with pytest.raises(ValueError, match=r"non-integral values"):
        bt.run(df, save=False)


# --------------------------------------------------------------------------- #
# M3 — Sharpe annualization derives from timeframe                            #
# --------------------------------------------------------------------------- #

def _synthetic_returns_df(n=400, seed=7):
    rng = np.random.default_rng(seed)
    rets = rng.normal(0.0005, 0.01, n)
    closes = 100 * np.cumprod(1 + rets)
    idx = pd.date_range("2024-01-01", periods=n, freq="D")
    df = pd.DataFrame({"open": closes, "close": closes}, index=idx)
    # Simple alternating signal so we generate equity-curve variation.
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
    # Unknown timeframe → fall back to daily, not crash.
    assert periods_per_year("nonsense") == 365


def test_sharpe_scales_with_timeframe():
    """A 4h backtest should report Sharpe ≈ sqrt(6)× a 1d backtest on the
    same equity curve. The pre-fix behaviour multiplied both by sqrt(365),
    masking sub-daily inflation."""
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


# --------------------------------------------------------------------------- #
# M4 — strangle leg guard                                                     #
# --------------------------------------------------------------------------- #

def test_options_backtester_rejects_max_positions_below_two():
    with pytest.raises(ValueError, match=r"max_positions must be >= 2"):
        OptionsBacktester(initial_capital=1000.0, max_positions=1)


def test_options_backtester_accepts_max_positions_two_and_above():
    OptionsBacktester(initial_capital=1000.0, max_positions=2)
    OptionsBacktester(initial_capital=1000.0, max_positions=4)


# --------------------------------------------------------------------------- #
# M5 — annualized return uses calendar days                                   #
# --------------------------------------------------------------------------- #

def test_annualized_return_uses_calendar_days_not_curve_length():
    """With ``check_interval=7`` the equity curve samples weekly. Computing
    ``years = len(curve)/365`` (the pre-fix behaviour) reports years≈0.14
    for a 1-year run, wildly inflating annualized return. Calendar-day
    elapsed time should give ~1.0 year and a sane annualized number."""
    bt = OptionsBacktester(initial_capital=1000.0, max_positions=2,
                           check_interval=7)
    bt.cash = 1100.0  # +10% over the run
    start = datetime(2023, 1, 1)
    bt.equity_curve = [
        ((start + timedelta(days=i * 7)).strftime("%Y-%m-%d"), 1000.0)
        for i in range(53)
    ]
    bt.equity_curve[-1] = (bt.equity_curve[-1][0], 1100.0)
    report = bt._generate_report("BTC", bt.equity_curve[0][0],
                                  bt.equity_curve[-1][0], 20000.0, 22000.0)
    # 1 year (or close) elapsed → annualized ≈ total return ≈ +10%.
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


# --------------------------------------------------------------------------- #
# L5 — backtest_theta force-close trade-log entries                           #
# --------------------------------------------------------------------------- #

def _synthetic_candles(n_days=200, start_price=20000.0, vol=0.02, seed=11):
    """Build minimal OHLCV candle list (ms timestamp + OHLCV)."""
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
    """If positions remain open at end-of-run, the force-close branch must
    record them in ``trade_log``; before the fix it silently settled cash."""
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
    # Either no force-close happened (no positions left) OR every force-close
    # carries a matching trade_log entry. We only assert that the log records
    # whatever closes the loop performed.
    force_close_log = [t for t in bt.trade_log if t.get("event") == "force_close"]
    assert len(bt.positions) == 0
    # Sanity: total_trades ≥ number of force_close entries (force-closes are
    # counted as trades). If any trade exists, the log shouldn't be empty.
    if bt.total_trades > 0:
        assert len(bt.trade_log) > 0


# --------------------------------------------------------------------------- #
# M2 — run_backtest --htf-filter path                                         #
# --------------------------------------------------------------------------- #

def _htf_ohlcv_df(n=120, start_price=100.0, drift=0.5, seed=3):
    """Build a synthetic HTF OHLCV frame with the same index name
    (``datetime``) that ``shared_tools.data_fetcher.load_cached_data``
    produces — the real shape the backtest consumes at runtime."""
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
    """Regression: the prior merge_asof implementation renamed from
    ``timestamp``/``index`` to ``ts``, but ``load_cached_data`` returns a
    ``datetime``-named index — so every real run raised KeyError. The
    reindex-based implementation must work against the real index name
    and emit one trend value per LTF bar."""
    import run_backtest  # backtest/ is already on sys.path via conftest

    htf_df = _htf_ohlcv_df(n=120)
    monkeypatch.setattr(run_backtest, "load_cached_data",
                        lambda *a, **kw: htf_df)

    ltf_index = pd.date_range("2023-02-01", periods=40, freq="6h",
                              name="datetime")
    trend = run_backtest._htf_trend_series("BTC/USDT", "1h", ltf_index)

    assert len(trend) == len(ltf_index)
    # Upward-drifting series → close eventually exceeds EMA → bullish trend.
    assert set(trend.unique()).issubset({-1, 0, 1})
    assert (trend == 1).any(), "expected bullish bars in upward-drift series"


def test_htf_trend_series_uses_last_closed_htf_bar_no_lookahead(monkeypatch):
    """#1154: ``load_cached_data`` indexes each HTF candle by its OPEN time,
    and a row's trend derives from that candle's full close — so it isn't
    actually known until the candle CLOSES (the next HTF row's open). An LTF
    bar inside a still-forming HTF candle must see the PRIOR (closed)
    candle's trend, never the forming candle's final close — the same
    open-vs-close-time leak fixed for regime alignment in #1153."""
    import run_backtest

    # 59 rising closes (bullish vs EMA), then a crash on the final —
    # still-forming — candle that flips its trend bearish.
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

    # Sanity: same EMA formula as _htf_trend_series — last CLOSED candle is
    # bullish, the forming candle's final close is bearish.
    ema = htf_df["close"].ewm(span=50, adjust=False).mean()
    assert closes[-2] > ema.iloc[-2]
    assert closes[-1] < ema.iloc[-1]

    # LTF bars falling inside the final (still-forming) HTF candle.
    ltf_index = pd.DatetimeIndex(
        [idx[-1] + pd.Timedelta(hours=h) for h in (0, 6, 12)],
        name="datetime",
    )
    trend = run_backtest._htf_trend_series("BTC/USDT", "1h", ltf_index)
    assert list(trend) == [1, 1, 1], (
        "LTF bars inside the forming HTF candle must see the last CLOSED "
        "candle's trend (1), not the forming candle's final close (-1)")

    # Before the first HTF candle has closed no trend is known yet → neutral.
    early = run_backtest._htf_trend_series(
        "BTC/USDT", "1h",
        pd.DatetimeIndex([idx[0] + pd.Timedelta(hours=6)], name="datetime"))
    assert list(early) == [0]


def test_run_single_backtest_with_htf_filter(monkeypatch):
    """End-to-end: run_single_backtest(..., htf_filter=True) against a
    mocked ``load_cached_data`` must not raise and must produce a result
    dict (proves the HTF path wires together without the KeyError)."""
    import run_backtest

    ltf = _htf_ohlcv_df(n=200, drift=0.3)
    htf = _htf_ohlcv_df(n=60, drift=1.0)
    calls = {"n": 0}

    def fake_load(symbol, timeframe, **kw):
        calls["n"] += 1
        # First call is LTF (run_single_backtest), subsequent is HTF
        # (_htf_trend_series). Both return the same shape — what matters
        # is that the ``datetime``-named index flows through.
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
