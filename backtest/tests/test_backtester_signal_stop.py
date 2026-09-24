import sys
import pathlib

import numpy as np
import pandas as pd

sys.path.insert(0, str(pathlib.Path(__file__).parent.parent.parent / "shared_tools"))
sys.path.insert(0, str(pathlib.Path(__file__).parent.parent))

from backtester import Backtester


def _df_with_signal(closes, signals, atr=None):
    closes = np.asarray(closes, dtype=float)
    n = len(closes)
    idx = pd.date_range("2024-01-01", periods=n, freq="D")
    df = pd.DataFrame(
        {
            "open": closes,
            "high": closes + 0.5,
            "low": closes - 0.5,
            "close": closes,
            "volume": np.full(n, 1000.0),
            "signal": np.asarray(signals, dtype=float),
        },
        index=idx,
    )
    if atr is not None:
        df["atr"] = float(atr)
    return df


def _run(df, **kw):
    bt = Backtester(initial_capital=10000.0, platform="binanceus", **kw)
    return bt.run(df.copy(), strategy_name="x", symbol="BTC/USDT", timeframe="1d", save=False)


def test_fixed_atr_stop_exits_on_drawdown():
    closes = [100, 100, 99, 96, 90, 85, 80, 80]
    signals = [1, 0, 0, 0, 0, 0, 0, 0]
    df = _df_with_signal(closes, signals, atr=2.0)

    stopped = _run(df, stop_loss_atr_mult=1.0)
    no_stop = _run(df, stop_loss_atr_mult=None)

    assert stopped["final_capital"] > no_stop["final_capital"]
    assert stopped["total_trades"] == 1


def test_stop_fills_next_bar_open_not_same_bar():
    closes = [100, 100, 97, 200, 200]
    opens = [100, 100, 97, 95, 200]
    n = len(closes)
    idx = pd.date_range("2024-01-01", periods=n, freq="D")
    df = pd.DataFrame(
        {
            "open": np.array(opens, float),
            "high": np.array(closes, float) + 0.5,
            "low": np.minimum(opens, closes) - 0.5,
            "close": np.array(closes, float),
            "volume": np.full(n, 1000.0),
            "signal": np.array([1, 0, 0, 0, 0], float),
            "atr": np.full(n, 2.0),
        },
        index=idx,
    )
    res = _run(df, stop_loss_atr_mult=1.0, intrabar_resolution="bar_close")
    assert res["total_trades"] == 1
    assert res["final_capital"] < 10000.0


def test_trailing_atr_stop_ratchets_and_caps_drawdown():
    closes = [100, 100, 110, 120, 130, 120, 100, 80, 80]
    signals = [1, 0, 0, 0, 0, 0, 0, 0, 0]
    df = _df_with_signal(closes, signals, atr=2.0)

    trailed = _run(df, trailing_stop_atr_mult=1.0)
    no_stop = _run(df, trailing_stop_atr_mult=None)
    assert trailed["final_capital"] > no_stop["final_capital"]
    assert trailed["max_drawdown_pct"] >= no_stop["max_drawdown_pct"]


def test_no_stop_when_mult_unset_is_unchanged():
    closes = [100, 100, 90, 80, 80]
    signals = [1, 0, 0, 0, -1]
    df = _df_with_signal(closes, signals, atr=2.0)
    res = _run(df)
    assert res["total_trades"] == 1
