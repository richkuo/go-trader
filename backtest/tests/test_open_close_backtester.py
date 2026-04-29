import pandas as pd

from backtester import Backtester


def test_backtester_open_close_columns_close_before_open():
    idx = pd.date_range("2024-01-01", periods=5, freq="D")
    df = pd.DataFrame({
        "open": [100, 100, 110, 120, 120],
        "close": [100, 110, 120, 120, 120],
        "open_action": ["none", "long", "short", "none", "none"],
        "close_fraction": [0, 0, 1, 0, 0],
    }, index=idx)

    bt = Backtester(initial_capital=1000, commission_pct=0, slippage_pct=0)
    result = bt.run(df, save=False)

    assert result["total_trades"] == 1
    assert result["trades"][0]["entry_date"] == str(idx[2])
    assert result["trades"][0]["exit_date"] == str(idx[3])
    assert result["final_capital"] == 1090.91


def test_backtester_close_fraction_columns_use_max_wins():
    idx = pd.date_range("2024-01-01", periods=5, freq="D")
    df = pd.DataFrame({
        "open": [100, 100, 100, 100, 100],
        "close": [100, 100, 100, 100, 100],
        "open_action": ["none", "long", "none", "none", "none"],
        "close_fraction:slow": [0, 0, 0.25, 0, 0],
        "close_fraction:fast": [0, 0, 0.5, 0, 0],
    }, index=idx)

    bt = Backtester(initial_capital=1000, commission_pct=0, slippage_pct=0)
    result = bt.run(df, save=False)

    assert result["total_trades"] == 2
    assert result["trades"][0]["shares"] == 5.0
    assert result["trades"][1]["shares"] == 5.0
