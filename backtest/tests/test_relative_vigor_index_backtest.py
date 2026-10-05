import os

import numpy as np
import pandas as pd
import pytest

import offline_manifest as om
from atr import ensure_atr_indicator
from backtester import Backtester
from eval_windows import run_leg
from parity_diff import ParityConfig, compute_parity_frame, summarize
from registry_loader import load_registry

NAME = "relative_vigor_index"
PARAMS = {"period": 3, "zero_line_filter": True}
TIME_STOP = [{"name": "time_stop", "params": {"max_bars": 3}}]
STUDY_MANIFEST = os.path.abspath(os.path.join(
    os.path.dirname(__file__), "..", "candidates", "relative_vigor_index_1666", "study_manifest.json"))


def _frame(rows):
    df = pd.DataFrame(rows, columns=["open", "high", "low", "close"], dtype=float)
    df["volume"] = 1.0
    df.index = pd.date_range("2026-01-01", periods=len(df), freq="4h")
    return df


def _doji(n, px=100.0):
    return [(px, px + 1.0, px - 1.0, px)] * n


def _bar(o, c):
    return (o, max(o, c) + 1.0, min(o, c) - 1.0, c)


def _signals(df, params=PARAMS):
    out = load_registry("futures").apply_strategy(NAME, df, params)
    return ensure_atr_indicator(out)


def _bt(**kw):
    base = dict(initial_capital=1000.0, platform="hyperliquid",
                open_strategy={"name": NAME, "params": PARAMS},
                close_strategies=TIME_STOP, stop_loss_atr_mult=1.0, direction="both",
                comparison_mode="approximate")
    base.update(kw)
    return Backtester(**base)


def _market(n=420, seed=1666):
    rng = np.random.RandomState(seed)
    t = np.arange(n)
    close = np.round(1000 + 40 * np.sin(t / 25.0) + np.cumsum(rng.randn(n) * 4.0))
    open_ = np.concatenate([[close[0]], close[:-1]]) + np.round(rng.randn(n) * 2.0)
    span = np.round(np.abs(rng.randn(n)) * 5.0) + 1.0
    df = pd.DataFrame({"open": open_, "high": np.maximum(open_, close) + span,
                       "low": np.minimum(open_, close) - span, "close": close, "volume": 1.0})
    df.index = pd.date_range("2025-01-01", periods=n, freq="4h")
    return df


def test_long_entry_fills_next_bar_open_and_time_stop_owns_exit():
    rows = _doji(20) + [_bar(100.0, 104.0)] + [_bar(104.0, 104.5)] * 8
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
    ablation = {"period": 3, "zero_line_filter": False}
    rows = _doji(20) + [_bar(100.0, 96.0), _bar(96.0, 110.0)] + _doji(10, 110.0)
    df = _frame(rows)
    sig = _signals(df, ablation)
    assert sig["signal"].iloc[20] == -1
    assert sig["signal"].iloc[21] == 1
    res = _bt(open_strategy={"name": NAME, "params": ablation},
              stop_loss_atr_mult=None, stop_loss_pct=0.5,
              close_strategies=[{"name": "time_stop", "params": {"max_bars": 6}}]).run(
        sig, strategy_name=NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    first = res["trades"][0]
    assert first["side"] == "short"
    assert first["entry_date"] == str(df.index[21])
    assert first["exit_reason"].startswith("time_stop:")
    assert first["exit_date"] == str(df.index[27])
    assert all(t["side"] == "short" and pd.Timestamp(t["entry_date"]) >= pd.Timestamp(first["exit_date"])
               for t in res["trades"][1:])


def test_invalid_entry_data_keeps_close_and_stop_processing():
    rows = _doji(20) + [_bar(100.0, 104.0)] + [_bar(104.0, 104.5)] * 6
    df = _frame(rows)
    blank = df.copy()
    blank.iloc[21:, blank.columns.get_loc("open")] = np.nan
    sig = _signals(blank)
    sig["open"] = df["open"]
    assert (sig["rvi_reason"].iloc[21:] == "nonfinite_input").all()
    assert (sig["signal"].iloc[21:] == 0).all()
    res = _bt().run(sig, strategy_name=NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    assert len(res["trades"]) == 1
    assert res["trades"][0]["exit_reason"].startswith("time_stop:")

    crash = _doji(20) + [_bar(100.0, 104.0)] + [(103.0, 103.5, 80.0, 81.0)] * 4
    cdf = _frame(crash)
    cdf = cdf.drop(cdf.index[22])
    sig2 = _signals(cdf)
    assert (sig2["rvi_reason"].iloc[22:] == "cadence_gap").all()
    res2 = _bt(close_strategies=[{"name": "time_stop", "params": {"max_bars": 50}}]).run(
        sig2, strategy_name=NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    assert res2["trades"][0]["exit_reason"] == "sl"


@pytest.mark.parametrize("window", [None, 200])
def test_non_batched_parity_matches_full_series(window):
    df = _market()
    cfg = ParityConfig(strategy_name=NAME, params={"period": 10, "zero_line_filter": True},
                       registry="futures", close_refs=[{"name": "time_stop", "params": {"max_bars": 20}}],
                       direction="both", comparison_mode="approximate")
    frame = compute_parity_frame(df, cfg=cfg, window=window)
    assert len(frame) > 0
    assert bool(frame["match"].all())
    assert summarize(frame)["close_parity"] == "incomplete" and not summarize(frame)["clean"]
    assert (frame["live_signal"] != 0).sum() > 0


def test_batched_parity_admits_explicit_paper_and_matches_solo():
    cfg = ParityConfig(strategy_name=NAME, params={"period": 10, "zero_line_filter": True},
                       registry="futures", batched=True, symbol="BTC", timeframe="4h")
    frame = compute_parity_frame(_market(240), cfg=cfg, window=200)
    assert len(frame) > 0
    assert bool(frame["match"].all())


def test_frozen_manifest_frame_full_series_prefix_and_bounded_window_agree():
    manifest = om.load_manifest(STUDY_MANIFEST)
    dataset = om.dataset_by_key(manifest, "BTC 4h")
    frame, _, coverage = om.window_frame(manifest, dataset, "test")
    assert coverage["complete"]
    sub = frame.iloc[:420]
    cfg = ParityConfig(strategy_name=NAME, params={"period": 10, "zero_line_filter": True},
                       registry="futures", close_refs=[{"name": "time_stop", "params": {"max_bars": 20}}],
                       direction="both", comparison_mode="approximate")
    for window in (None, 200):
        parity = compute_parity_frame(sub, cfg=cfg, window=window)
        assert bool(parity["match"].all())
        assert summarize(parity)["close_parity"] == "incomplete" and not summarize(parity)["clean"]


def test_manifest_leg_books_funding_and_execution_spec():
    manifest = om.load_manifest(STUDY_MANIFEST)
    reg = load_registry("futures")
    leg = run_leg(reg, NAME, {"period": 10, "zero_line_filter": True}, "BTC", "4h", ("", ""),
                  capital=1000.0, close_strategies=[{"name": "time_stop", "params": {"max_bars": 20}}],
                  direction="both", stop_loss_atr_mult=1.0,
                  manifest_ctx={"manifest": manifest, "window": "train"},
                  comparison_mode="approximate")
    info = leg["manifest"]
    assert info["cost_model"] == "execution_spec"
    assert info["funding_coverage"]["available"] is True
    assert info["candle_coverage"]["complete"] is True
    ex = leg["execution"]
    assert ex["positions"] > 0
    assert ex["long_positions"] > 0 and ex["short_positions"] > 0
    assert ex["funding_pnl_usd"] != 0.0
