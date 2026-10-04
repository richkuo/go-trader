import numpy as np
import pandas as pd
import pytest

from backtester import Backtester
from registry_loader import load_registry
from atr import ensure_atr_indicator
from parity_diff import ParityConfig, compute_parity_frame, summarize

NAME = "connors_rsi_reversion"
CLOSE = [{"name": "time_stop", "params": {"max_bars": 6}}]


def _market(n=420, seed=1645):
    rng = np.random.RandomState(seed)
    t = np.arange(n)
    close = 100.0 * np.exp(0.08 * np.sin(t / 25.0) + np.cumsum(rng.randn(n) * 0.012))
    open_ = np.concatenate([[close[0]], close[:-1]])
    span = np.abs(rng.randn(n)) * 0.004 * close + 0.01
    df = pd.DataFrame({"open": open_, "high": np.maximum(open_, close) + span,
                       "low": np.minimum(open_, close) - span, "close": close,
                       "volume": 50 + np.abs(rng.randn(n)) * 40})
    df.index = pd.date_range("2025-01-01", periods=n, freq="4h")
    return df


def _signals(df, params=None):
    return ensure_atr_indicator(load_registry("futures").apply_strategy(NAME, df, params))


def _bt(**kw):
    base = dict(initial_capital=1000.0, platform="hyperliquid",
                open_strategy={"name": NAME, "params": {}},
                close_strategies=CLOSE, stop_loss_atr_mult=1.0, direction="both",
                comparison_mode="approximate")
    base.update(kw)
    return Backtester(**base)


def test_entries_fill_next_bar_open_both_sides_and_time_stop_owns_exits():
    df = _market()
    sig = _signals(df)
    res = _bt().run(sig, strategy_name=NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    trades = res["trades"]
    assert {t["side"] for t in trades} == {"long", "short"}
    pos = {ts: i for i, ts in enumerate(df.index)}
    for t in trades:
        i = pos[pd.Timestamp(t["entry_date"])]
        expected_side = 1 if t["side"] == "long" else -1
        assert int(sig["signal"].iloc[i - 1]) == expected_side
        slip = 1.0005 if t["side"] == "long" else 0.9995
        assert t["entry_price"] == pytest.approx(df["open"].iloc[i] * slip)
        assert t["exit_reason"].startswith("time_stop:") or t["exit_reason"] in {"sl", "end_of_data"}
    for a, b in zip(trades, trades[1:]):
        assert pd.Timestamp(b["entry_date"]) >= pd.Timestamp(a["exit_date"])


def _parity_cfg(**kw):
    base = dict(strategy_name=NAME, registry="futures", platform="hyperliquid",
                symbol="BTC", timeframe="4h", close_refs=CLOSE, direction="both",
                comparison_mode="approximate")
    base.update(kw)
    return ParityConfig(**base)


@pytest.mark.parametrize("window", [200, None])
def test_non_batched_parity_runner_is_clean_on_bounded_window_and_every_prefix(window):
    df = _market(360, seed=7)
    frame = compute_parity_frame(df, cfg=_parity_cfg(), window=window)
    result = summarize(frame)
    assert result["bars_compared"] > 0
    assert result["mismatches"] == 0, frame[~frame["match"]].head()
    assert result["close_parity"] == "incomplete" and not result["clean"]
    assert (frame["live_signal"] != 0).any()


def test_batched_parity_mode_refuses_the_research_entry():
    with pytest.raises(ValueError, match="backtest_only"):
        compute_parity_frame(_market(260), cfg=_parity_cfg(batched=True), window=200)
