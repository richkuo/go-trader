import numpy as np
import pandas as pd
import pytest

from backtester import Backtester
from registry_loader import load_registry
from atr import ensure_atr_indicator

NAME = "chaikin_money_flow_breakout"
PARAMS = {"flow_window": 3, "breakout_window": 3, "flow_threshold": 0.2}


def _frame(rows):
    df = pd.DataFrame(rows, columns=["open", "high", "low", "close", "volume"], dtype=float)
    df.index = pd.date_range("2026-01-01", periods=len(df), freq="4h")
    return df


def _flat(n, px=100.0):
    return [(px, px + 1.0, px - 1.0, px, 1.0)] * n


def _signals(df):
    reg = load_registry("futures")
    out = reg.apply_strategy(NAME, df, PARAMS)
    return ensure_atr_indicator(out)


def _bt(**kw):
    base = dict(initial_capital=1000.0, platform="hyperliquid",
                open_strategy={"name": NAME, "params": PARAMS},
                close_strategies=[{"name": "time_stop", "params": {"max_bars": 3}}],
                stop_loss_atr_mult=1.0, direction="both", comparison_mode="approximate")
    base.update(kw)
    return Backtester(**base)


def test_long_entry_fills_next_bar_open_and_time_stop_owns_exit():
    rows = _flat(20) + [(100.0, 110.0, 100.0, 110.0, 8.0)]
    rows += [(111.0, 112.5, 110.5, 112.0, 1.0)] * 8
    df = _frame(rows)
    sig = _signals(df)
    assert sig["signal"].iloc[20] == 1
    res = _bt().run(sig, strategy_name=NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    trades = res["trades"]
    assert len(trades) == 1
    t = trades[0]
    assert t["side"] == "long"
    assert t["entry_date"] == str(df.index[21])
    assert t["entry_price"] == pytest.approx(df["open"].iloc[21] * 1.0005)
    assert t["exit_reason"].startswith("time_stop:")


def test_short_mirror_and_no_reversal_while_open():
    rows = _flat(20) + [(100.0, 100.0, 90.0, 90.0, 8.0)]
    rows += [(89.5, 90.0, 88.0, 89.0, 1.0)] * 2
    rows += [(89.0, 120.0, 88.0, 120.0, 30.0)]
    rows += [(119.0, 121.0, 118.0, 119.5, 1.0)] * 6
    df = _frame(rows)
    sig = _signals(df)
    assert sig["signal"].iloc[20] == -1
    assert sig["signal"].iloc[23] == 1
    res = _bt(stop_loss_atr_mult=None, stop_loss_pct=0.5,
              close_strategies=[{"name": "time_stop", "params": {"max_bars": 6}}]).run(
        sig, strategy_name=NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    assert len(res["trades"]) == 1
    t = res["trades"][0]
    assert t["side"] == "short"
    assert t["entry_date"] == str(df.index[21])
    assert t["exit_reason"].startswith("time_stop:")
    assert t["exit_date"] == str(df.index[27])


def test_invalid_entry_volume_keeps_close_and_stop_processing():
    rows = _flat(20) + [(100.0, 110.0, 100.0, 110.0, 8.0)]
    rows += [(111.0, 112.5, 110.5, 112.0, float("nan"))] * 6
    df = _frame(rows)
    sig = _signals(df)
    assert (sig["cmf_reason"].iloc[21:] == "nonfinite_input").all()
    assert (sig["signal"].iloc[21:] == 0).all()
    res = _bt().run(sig, strategy_name=NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    assert len(res["trades"]) == 1
    assert res["trades"][0]["exit_reason"].startswith("time_stop:")

    crash = _flat(20) + [(100.0, 110.0, 100.0, 110.0, 8.0)]
    crash += [(109.0, 109.5, 80.0, 81.0, float("nan"))] * 4
    sig2 = _signals(_frame(crash))
    res2 = _bt(close_strategies=[{"name": "time_stop", "params": {"max_bars": 50}}]).run(
        sig2, strategy_name=NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    assert res2["trades"][0]["exit_reason"] == "sl"
