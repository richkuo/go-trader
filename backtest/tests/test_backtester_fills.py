import pandas as pd
import pytest

from backtester import Backtester


def _make_df(opens, highs, lows, closes, signals):
    n = len(closes)
    idx = pd.date_range("2024-01-01", periods=n, freq="D")
    return pd.DataFrame(
        {
            "open": opens,
            "high": highs,
            "low": lows,
            "close": closes,
            "signal": signals,
        },
        index=idx,
    )


def test_fill_uses_next_bar_open_not_signal_bar_close():
    opens = [100, 100, 100, 110, 110, 110]
    highs = [101, 101, 101, 111, 111, 111]
    lows = [99, 99, 99, 109, 109, 109]
    closes = [100, 100, 100, 110, 110, 110]
    signals = [0, 0, 1, 0, 0, -1]

    df = _make_df(opens, highs, lows, closes, signals)
    bt = Backtester(initial_capital=1000.0, commission_pct=0.0, slippage_pct=0.0)
    results = bt.run(df, strategy_name="lookahead-check", save=False)

    assert len(results["trades"]) == 1
    trade = results["trades"][0]
    assert trade["entry_price"] == pytest.approx(110.0, rel=1e-9), (
        "Entry must fill at the bar after the signal bar's open (110), "
        "not the signal bar's close (100) — that would be look-ahead bias."
    )
    assert trade["shares"] == pytest.approx(1000.0 / 110.0, rel=1e-9), (
        "Share count must divide available cash by the actual fill price "
        "(110), not the signal-bar close (100)."
    )


def test_exit_uses_next_bar_open_not_signal_bar_close():
    opens = [100, 100, 100, 100, 200, 300]
    highs = [101, 101, 101, 101, 201, 301]
    lows = [99, 99, 99, 99, 199, 299]
    closes = [100, 100, 100, 100, 200, 300]
    signals = [0, 1, 0, -1, 0, 0]

    df = _make_df(opens, highs, lows, closes, signals)
    bt = Backtester(initial_capital=1000.0, commission_pct=0.0, slippage_pct=0.0)
    results = bt.run(df, strategy_name="lookahead-exit", save=False)

    assert len(results["trades"]) == 1
    trade = results["trades"][0]
    assert trade["entry_price"] == pytest.approx(100.0, rel=1e-9)
    assert trade["exit_price"] == pytest.approx(200.0, rel=1e-9), (
        "Exit must fill at the bar after the sell-signal bar's open (200), "
        "not the signal bar's close (100)."
    )
    expected_final_cash = 1000.0 * (200.0 / 100.0)
    assert results["final_capital"] == pytest.approx(expected_final_cash, rel=1e-9), (
        "Final cash should double with the 2x gap-up fill, not stay flat."
    )


def test_falls_back_to_close_when_open_column_missing():
    closes = [100, 100, 100, 110, 110, 110]
    signals = [0, 0, 1, 0, 0, -1]
    idx = pd.date_range("2024-01-01", periods=len(closes), freq="D")
    df = pd.DataFrame({"close": closes, "signal": signals}, index=idx)

    bt = Backtester(initial_capital=1000.0, commission_pct=0.0, slippage_pct=0.0)
    results = bt.run(df, strategy_name="legacy-close-only", save=False)

    assert len(results["trades"]) == 1
    assert results["trades"][0]["entry_price"] == pytest.approx(110.0, rel=1e-9)


def test_signal_on_final_bar_never_fills():
    opens = [100, 100, 100]
    closes = [100, 100, 100]
    signals = [0, 0, 1]
    idx = pd.date_range("2024-01-01", periods=3, freq="D")
    df = pd.DataFrame(
        {"open": opens, "close": closes, "signal": signals}, index=idx
    )

    bt = Backtester(initial_capital=1000.0, commission_pct=0.0, slippage_pct=0.0)
    results = bt.run(df, strategy_name="last-bar-signal", save=False)

    assert results["total_trades"] == 0


def test_signal_on_bar_zero_fills_on_bar_one():
    opens = [100, 200, 200, 200, 200]
    closes = [100, 200, 200, 200, 200]
    signals = [1, 0, 0, 0, -1]
    idx = pd.date_range("2024-01-01", periods=5, freq="D")
    df = pd.DataFrame(
        {"open": opens, "close": closes, "signal": signals}, index=idx
    )

    bt = Backtester(initial_capital=1000.0, commission_pct=0.0, slippage_pct=0.0)
    results = bt.run(df, strategy_name="bar-zero-signal", save=False)

    assert len(results["trades"]) == 1
    assert results["trades"][0]["entry_price"] == pytest.approx(200.0, rel=1e-9)


def test_buy_signal_while_long_is_ignored():
    opens = [100, 100, 100, 100, 100, 100]
    closes = [100, 100, 100, 100, 100, 100]
    signals = [0, 1, 1, 0, 0, -1]
    idx = pd.date_range("2024-01-01", periods=6, freq="D")
    df = pd.DataFrame(
        {"open": opens, "close": closes, "signal": signals}, index=idx
    )

    bt = Backtester(initial_capital=1000.0, commission_pct=0.0, slippage_pct=0.0)
    results = bt.run(df, strategy_name="repeat-buy", save=False)

    assert results["total_trades"] == 1


def test_sell_signal_while_flat_is_ignored():
    opens = [100, 100, 100, 100]
    closes = [100, 100, 100, 100]
    signals = [0, -1, 0, 0]
    idx = pd.date_range("2024-01-01", periods=4, freq="D")
    df = pd.DataFrame(
        {"open": opens, "close": closes, "signal": signals}, index=idx
    )

    bt = Backtester(initial_capital=1000.0, commission_pct=0.0, slippage_pct=0.0)
    results = bt.run(df, strategy_name="sell-while-flat", save=False)

    assert results["total_trades"] == 0
    assert results["final_capital"] == pytest.approx(1000.0, rel=1e-9)
