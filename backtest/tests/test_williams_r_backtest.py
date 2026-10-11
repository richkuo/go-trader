import json
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
from regime_label_columns import (attach_regime_label_columns, feature_label_kwargs,
                                  resolve_feature_windows)

NAME = "williams_r_reversal"
PARAMS = {"lookback": 3, "oversold": -80.0, "overbought": -20.0}
TP = [{"name": "tiered_tp_atr", "params": {"tp_tiers": [{"atr_multiple": 2.0, "close_fraction": 1.0}]}}]
STUDY_DIR = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "candidates", "williams_r_reversal_1650"))
STUDY_MANIFEST = os.path.join(STUDY_DIR, "study_manifest.json")
STUDY_SPEC = os.path.join(STUDY_DIR, "candidate_spec.json")


def _frame(rows, start="2026-01-01"):
    df = pd.DataFrame(rows, columns=["open", "high", "low", "close"], dtype=float)
    df["volume"] = 1.0
    df.index = pd.date_range(start, periods=len(df), freq="4h")
    return df


def _bar(o, c, pad=0.0):
    return (o, max(o, c) + pad, min(o, c) - pad, c)


FLAT = [(100.0, 101.0, 99.0, 100.0)] * 20


def _signals(df, params=PARAMS):
    return ensure_atr_indicator(load_registry("futures").apply_strategy(NAME, df, params))


def _bt(**kw):
    base = dict(initial_capital=1000.0, platform="hyperliquid",
                open_strategy={"name": NAME, "params": PARAMS},
                close_strategies=TP, stop_loss_atr_mult=1.0, direction="both",
                comparison_mode="strict")
    base.update(kw)
    return Backtester(**base)


def _run(sig, **kw):
    return _bt(**kw).run(sig, strategy_name=NAME, symbol="BTC/USDT", timeframe="4h", save=False)


def _market(n=420, seed=1650):
    rng = np.random.RandomState(seed)
    t = np.arange(n)
    close = np.round(1000 + 60 * np.sin(t / 18.0) + np.cumsum(rng.randn(n) * 5.0), 2)
    open_ = np.concatenate([[close[0]], close[:-1]])
    span = np.round(np.abs(rng.randn(n)) * 6.0, 2) + 1.0
    df = pd.DataFrame({"open": open_, "high": np.maximum(open_, close) + span,
                       "low": np.minimum(open_, close) - span, "close": close, "volume": 1.0})
    df.index = pd.date_range("2025-01-01", periods=n, freq="4h")
    return df


def test_long_entry_fills_next_bar_open_and_the_take_profit_owns_the_exit():
    rows = FLAT + [_bar(100, 99), _bar(99, 98), _bar(98, 97), _bar(97, 98.5)]
    rows += [_bar(98.5 + i * 1.5, 100 + i * 1.5) for i in range(8)]
    df = _frame(rows)
    sig = _signals(df)
    decision = sig.index.get_loc(df.index[23])
    assert sig["signal"].iloc[decision] == 1
    assert sig["wr_prev"].iloc[decision] <= -80.0 < sig["wr"].iloc[decision]
    assert (sig["signal"].iloc[:decision] == 0).all()
    res = _run(sig)
    assert len(res["trades"]) == 1
    t = res["trades"][0]
    assert t["side"] == "long"
    assert t["entry_date"] == str(df.index[decision + 1])
    assert t["entry_price"] == pytest.approx(df["open"].iloc[decision + 1] * 1.0005)
    assert t["exit_reason"].startswith("tiered_tp_atr")
    assert t["exit_price"] == pytest.approx(t["entry_price"] + 2.0 * t["entry_atr"])
    assert res["close_validation"]["incomplete_parity"] is False


def test_short_mirror_and_an_open_position_blocks_new_entries():
    rows = FLAT + [_bar(100, 101), _bar(101, 102), _bar(102, 103), _bar(103, 101.5)]
    rows += [_bar(101.5, 100.0), _bar(100.0, 99.0), _bar(99.0, 98.0), _bar(98.0, 99.5)]
    rows += [_bar(99.5, 99.4)] * 6
    df = _frame(rows)
    sig = _signals(df)
    assert sig["signal"].iloc[23] == -1
    assert sig["wr_prev"].iloc[23] >= -20.0 > sig["wr"].iloc[23]
    long_bar = 27
    assert sig["signal"].iloc[long_bar] == 1
    res = _run(sig, close_strategies=[{"name": "tiered_tp_atr",
                                       "params": {"tp_tiers": [{"atr_multiple": 50.0, "close_fraction": 1.0}]}}],
               stop_loss_atr_mult=20.0)
    assert len(res["trades"]) == 1
    t = res["trades"][0]
    assert t["side"] == "short"
    assert t["entry_date"] == str(df.index[24])
    assert pd.Timestamp(t["exit_date"]) == df.index[-1]


def test_invalid_entry_data_keeps_take_profit_and_stop_processing():
    rows = FLAT + [_bar(100, 99), _bar(99, 98), _bar(98, 97), _bar(97, 98.5)]
    rows += [_bar(98.5 + i * 1.5, 100 + i * 1.5) for i in range(8)]
    df = _frame(rows)
    blank = df.copy()
    blank.iloc[24:, blank.columns.get_loc("high")] = np.nan
    sig = _signals(blank)
    sig["high"] = df["high"]
    sig["atr"] = ensure_atr_indicator(df.copy())["atr"]
    assert (sig["wr_reason"].iloc[24:] == "nonfinite_input").all()
    assert (sig["signal"].iloc[24:] == 0).all()
    res = _run(sig)
    assert len(res["trades"]) == 1
    assert res["trades"][0]["exit_reason"].startswith("tiered_tp_atr")

    crash = FLAT + [_bar(100, 99), _bar(99, 98), _bar(98, 97), _bar(97, 98.5)]
    crash += [(98.0, 98.2, 80.0, 81.0)] * 4
    cdf = _frame(crash)
    cblank = cdf.copy()
    cblank.iloc[24:, cblank.columns.get_loc("close")] = 0.0
    sig2 = _signals(cblank)
    sig2["close"] = cdf["close"]
    sig2["atr"] = ensure_atr_indicator(cdf.copy())["atr"]
    assert (sig2["wr_reason"].iloc[24:] == "nonpositive_price").all()
    res2 = _run(sig2)
    assert res2["trades"][0]["exit_reason"] == "sl"


def test_range_gate_blocks_entries_only_and_reads_the_prior_bar_label():
    manifest = om.load_manifest(STUDY_MANIFEST)
    frame, _, _ = om.window_frame(manifest, om.dataset_by_key(manifest, "ETH 4h"), "test")
    df = frame.iloc[:900].copy()
    spec = json.load(open(STUDY_SPEC))
    gate = spec["range_regime_gate"]
    plan = resolve_feature_windows({}, {"enabled": True, "period": gate["period"],
                                        "adx_threshold": gate["adx_threshold"],
                                        "timeframe": gate["timeframe"]})
    params = {"lookback": 14, "oversold": -80.0, "overbought": -20.0}
    sig = _signals(df, params)
    open_ungated = _run(sig.copy(), open_strategy={"name": NAME, "params": params})
    labeled, label_cols = attach_regime_label_columns(sig.copy(), plan)
    gated = _bt(open_strategy={"name": NAME, "params": params}, regime_enabled=True,
                allowed_regimes=list(gate["allowed_labels"]), regime_gate_on_failure=gate["on_failure"],
                regime_label_columns=label_cols, **feature_label_kwargs(plan)).run(
        labeled, strategy_name=NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    labels = labeled["regime"]
    assert (labels.iloc[:199] == "").all()
    assert set(labels.iloc[199:]) <= {"ranging", "trending_up", "trending_down"}
    entries = [labeled.index.get_loc(pd.Timestamp(t["entry_date"])) for t in gated["trades"]]
    assert entries
    assert all(labels.iloc[i - 1] == "ranging" for i in entries)
    assert all(i - 1 >= 199 for i in entries)
    decided = [i for i in range(200, len(labeled) - 1) if labeled["signal"].iloc[i] != 0]
    assert any(labels.iloc[i] != "ranging" for i in decided)
    assert len(gated["trades"]) < len(open_ungated["trades"])
    exits = [labeled.index.get_loc(pd.Timestamp(t["exit_date"])) for t in gated["trades"]]
    assert any(labels.iloc[i] != "ranging" for i in exits)


@pytest.mark.parametrize("window", [None, 200])
def test_non_batched_parity_matches_full_series(window):
    df = _market(420)
    cfg = ParityConfig(strategy_name=NAME, params={"lookback": 14, "oversold": -80.0, "overbought": -20.0},
                       registry="futures", platform="hyperliquid", timeframe="4h", close_refs=TP,
                       direction="both", comparison_mode="strict")
    frame = compute_parity_frame(df, cfg=cfg, window=window)
    assert len(frame) > 0
    assert bool(frame["match"].all())
    assert (frame["live_signal"] != 0).sum() > 0
    assert summarize(frame)["close_parity"] != "incomplete"


@pytest.mark.parametrize("lookback", [2, 14, 100])
def test_batched_parity_admits_explicit_paper_and_matches_solo(lookback):
    cfg = ParityConfig(strategy_name=NAME, params={"lookback": lookback, "oversold": -80.0, "overbought": -20.0},
                       registry="futures", platform="hyperliquid", batched=True, symbol="BTC", timeframe="4h")
    frame = compute_parity_frame(_market(240), cfg=cfg, window=200)
    assert len(frame) > 0
    assert bool(frame["match"].all())
    assert summarize(frame)["batch_mismatches"] == 0


def test_frozen_manifest_frame_full_series_prefix_and_bounded_window_agree():
    manifest = om.load_manifest(STUDY_MANIFEST)
    dataset = om.dataset_by_key(manifest, "BTC 4h")
    frame, _, coverage = om.window_frame(manifest, dataset, "test")
    assert coverage["complete"]
    sub = frame.iloc[:420]
    cfg = ParityConfig(strategy_name=NAME, params={"lookback": 14, "oversold": -80.0, "overbought": -20.0},
                       registry="futures", platform="hyperliquid", timeframe="4h", close_refs=TP,
                       direction="both", comparison_mode="strict")
    for window in (None, 200):
        parity = compute_parity_frame(sub, cfg=cfg, window=window)
        assert bool(parity["match"].all())


def test_manifest_leg_books_funding_execution_spec_and_both_sides():
    manifest = om.load_manifest(STUDY_MANIFEST)
    reg = load_registry("futures")
    leg = run_leg(reg, NAME, {"lookback": 14, "oversold": -80.0, "overbought": -20.0}, "BTC", "4h", ("", ""),
                  capital=1000.0, close_strategies=TP, direction="both", stop_loss_atr_mult=1.0,
                  manifest_ctx={"manifest": manifest, "window": "train"}, comparison_mode="strict")
    info = leg["manifest"]
    assert info["cost_model"] == "execution_spec"
    assert info["funding_coverage"]["available"] is True
    assert info["funding_coverage"]["complete"] is True
    assert info["candle_coverage"]["complete"] is True
    ex = leg["execution"]
    assert ex["long_positions"] > 0 and ex["short_positions"] > 0
    assert ex["funding_pnl_usd"] != 0.0
    assert leg["close_validation"]["incomplete_parity"] is False
