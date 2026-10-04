import os

import numpy as np
import pandas as pd
import pytest

import offline_manifest as om
from atr import ensure_atr_indicator
from backtester import Backtester
from parity_diff import ParityConfig, compute_parity_frame, summarize
from registry_loader import load_registry

NAME = "commodity_channel_trend"
BASE = [(101.0, 99.0, 100.0) if i % 2 == 0 else (102.0, 100.0, 101.0) for i in range(20)]
UP = [(102.0, 100.5, 101.5), (112.0, 102.0, 110.0)]
DOWN = [(100.5, 99.0, 99.5), (99.0, 89.0, 91.0)]
PARAMS = {"lookback": 5, "threshold": 120.0, "trend_period": 3}
CLOSE = [{"name": "time_stop", "params": {"max_bars": 20}}]
MANIFEST = os.path.abspath(os.path.join(
    os.path.dirname(__file__), "..", "candidates", "commodity_channel_trend_1656",
    "study_manifest.json"))


def _frame(rows):
    df = pd.DataFrame(rows, columns=["high", "low", "close"], dtype=float)
    df.insert(0, "open", df["close"])
    df["volume"] = 1.0
    df.index = pd.date_range("2026-01-01", periods=len(df), freq="4h")
    return df


def _signals(df, params=PARAMS):
    out = load_registry("futures").apply_strategy(NAME, df, params)
    return ensure_atr_indicator(out)


def _bt(**kw):
    base = dict(initial_capital=1000.0, platform="hyperliquid",
                open_strategy={"name": NAME, "params": PARAMS},
                close_strategies=[{"name": "time_stop", "params": {"max_bars": 3}}],
                stop_loss_atr_mult=1.0, direction="both")
    base.update(kw)
    return Backtester(**base)


def _hold(px, n):
    return [(px + 1.0, px - 1.0, px)] * n


def test_long_entry_fills_next_bar_open_and_time_stop_owns_exit():
    df = _frame(BASE + UP + _hold(110.5, 8))
    sig = _signals(df)
    entry_bar = len(BASE) + 1
    assert sig["signal"].iloc[entry_bar] == 1
    assert (sig["signal"].drop(sig.index[entry_bar]) == 0).all()
    res = _bt().run(sig, strategy_name=NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    trades = res["trades"]
    assert len(trades) == 1
    t = trades[0]
    assert t["side"] == "long"
    assert t["entry_date"] == str(df.index[entry_bar + 1])
    assert t["entry_price"] == pytest.approx(df["open"].iloc[entry_bar + 1] * 1.0005)
    assert t["exit_reason"].startswith("time_stop:")


def test_short_mirror_and_no_reversal_while_open():
    swing = [(97.0, 95.0, 96.0), (98.0, 96.0, 97.0)] * 3
    rally = [(97.5, 96.0, 97.0), (107.5, 97.5, 105.5)]
    rows = BASE + DOWN + _hold(91.0, 8) + swing + rally + _hold(106.0, 6)
    df = _frame(rows)
    sig = _signals(df)
    short_bar = len(BASE) + 1
    long_bar = short_bar + 8 + len(swing) + 2
    assert sig["signal"].iloc[short_bar] == -1
    assert sig["signal"].iloc[long_bar] == 1
    res = _bt(stop_loss_atr_mult=None, stop_loss_pct=0.5,
              close_strategies=[{"name": "time_stop", "params": {"max_bars": 40}}]).run(
        sig, strategy_name=NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    assert len(res["trades"]) == 1
    t = res["trades"][0]
    assert t["side"] == "short"
    assert t["entry_date"] == str(df.index[short_bar + 1])


def _signals_with_bad_entry_data(df, bad_rows):
    corrupt = df.copy()
    corrupt.loc[corrupt.index[-bad_rows:], "high"] = float("nan")
    sig = _signals(corrupt)
    sig[["open", "high", "low", "close"]] = df[["open", "high", "low", "close"]]
    return ensure_atr_indicator(sig.drop(columns=["atr"]))


def test_invalid_entry_data_keeps_close_and_stop_processing():
    df = _frame(BASE + UP + _hold(110.5, 6))
    sig = _signals_with_bad_entry_data(df, 6)
    assert (sig["cct_reason"].iloc[-6:] == "nonfinite_input").all()
    assert (sig["signal"].iloc[-6:] == 0).all()
    assert sig["signal"].iloc[len(BASE) + 1] == 1
    res = _bt().run(sig, strategy_name=NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    assert len(res["trades"]) == 1
    assert res["trades"][0]["exit_reason"].startswith("time_stop:")

    crash = _frame(BASE + UP + _hold(110.5, 1) + [(109.5, 80.0, 81.0)] * 4)
    sig2 = _signals_with_bad_entry_data(crash, 4)
    assert (sig2["cct_valid"].iloc[-4:] == False).all()
    res2 = _bt(close_strategies=[{"name": "time_stop", "params": {"max_bars": 50}}]).run(
        sig2, strategy_name=NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    assert res2["trades"][0]["exit_reason"] == "sl"


def _frozen_btc(bars):
    manifest = om.load_manifest(MANIFEST)
    candles = om.load_candles(om.dataset_by_key(manifest, "BTC 4h"))
    return candles[["open", "high", "low", "close", "volume"]].iloc[-bars:]


def _cfg(**kw):
    base = dict(strategy_name=NAME, registry="futures", platform="hyperliquid",
                symbol="BTC", timeframe="4h")
    base.update(kw)
    return ParityConfig(**base)


@pytest.mark.parametrize("window", [200, None])
def test_non_batched_parity_clean_on_frozen_hyperliquid_candles(window):
    df = _frozen_btc(700)
    frame = compute_parity_frame(df, cfg=_cfg(), window=window)
    result = summarize(frame)
    assert result["bars_compared"] >= 500
    assert result["clean"], frame[~frame["match"]].head()
    assert (frame["live_signal"] != 0).sum() > 0


@pytest.mark.parametrize("window", [200, None])
def test_non_batched_parity_with_close_owner_and_both_directions(window):
    df = _frozen_btc(600)
    cfg = _cfg(close_refs=[dict(c, params=dict(c["params"])) for c in CLOSE], direction="both")
    frame = compute_parity_frame(df, cfg=cfg, window=window)
    result = summarize(frame)
    assert result["bars_compared"] >= 400
    assert result["clean"], frame[~frame["match"]].head()
    assert (frame["live_open_action"] == "long").any()
    assert (frame["live_open_action"] == "short").any()


def test_batched_parity_mode_refuses_research_entry():
    df = _frozen_btc(260)
    with pytest.raises(ValueError, match="backtest_only"):
        compute_parity_frame(df, cfg=_cfg(batched=True), window=200)
