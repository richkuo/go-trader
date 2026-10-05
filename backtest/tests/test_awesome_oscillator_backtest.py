import json
import os
import subprocess
import sys

import numpy as np
import pandas as pd
import pytest

import offline_manifest as om
from atr import ensure_atr_indicator
from backtester import Backtester
from eval_windows import run_leg
from parity_diff import ParityConfig, compute_parity_frame, summarize
from registry_loader import load_registry

NAME = "awesome_oscillator"
PARAMS = {"fast_period": 1, "slow_period": 2}
REPO = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
MANIFEST = os.path.join(REPO, "backtest", "candidates", "awesome_oscillator_1660", "study_manifest.json")
TIME_STOP = {"name": "time_stop", "params": {"max_bars": 20}}


def _frame(mids, jump_rows=None):
    mids = np.asarray(mids, dtype=float)
    df = pd.DataFrame({"open": mids, "high": mids + 1.0, "low": mids - 1.0, "close": mids,
                       "volume": np.ones(len(mids))})
    for i, row in (jump_rows or {}).items():
        df.iloc[i] = row
    df.index = pd.date_range("2026-01-01", periods=len(df), freq="4h")
    return df


def _signals(df):
    out = load_registry("futures").apply_strategy(NAME, df, PARAMS)
    return ensure_atr_indicator(out)


def _bt(**kw):
    base = dict(initial_capital=1000.0, platform="hyperliquid",
                open_strategy={"name": NAME, "params": PARAMS},
                close_strategies=[{"name": "time_stop", "params": {"max_bars": 3}}],
                stop_loss_atr_mult=1.0, direction="both", comparison_mode="approximate")
    base.update(kw)
    return Backtester(**base)


def test_long_entry_fills_next_bar_open_and_time_stop_owns_exit():
    mids = [100.0] * 20 + [99.0, 101.0] + [101.5] * 8
    df = _frame(mids)
    sig = _signals(df)
    assert sig["signal"].iloc[21] == 1
    assert (sig["signal"].drop(sig.index[21]) == 0).all()
    res = _bt().run(sig, strategy_name=NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    trades = res["trades"]
    assert len(trades) == 1
    t = trades[0]
    assert t["side"] == "long"
    assert t["entry_date"] == str(df.index[22])
    assert t["entry_price"] == pytest.approx(df["open"].iloc[22] * 1.0005)
    assert t["exit_reason"].startswith("time_stop:")


def test_short_mirror_and_no_reversal_while_open():
    mids = [100.0] * 20 + [101.0, 99.0, 98.0, 97.0, 99.0] + [99.5] * 8
    df = _frame(mids)
    sig = _signals(df)
    assert sig["signal"].iloc[21] == -1
    assert sig["signal"].iloc[24] == 1
    res = _bt(stop_loss_atr_mult=None, stop_loss_pct=0.5,
              close_strategies=[{"name": "time_stop", "params": {"max_bars": 6}}]).run(
        sig, strategy_name=NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    assert len(res["trades"]) == 1
    t = res["trades"][0]
    assert t["side"] == "short"
    assert t["entry_date"] == str(df.index[22])
    assert t["exit_reason"].startswith("time_stop:")
    assert t["exit_date"] == str(df.index[28])


def _swap_high_low(df, start):
    hi, lo = df.columns.get_loc("high"), df.columns.get_loc("low")
    high = df.iloc[start:, hi].copy()
    df.iloc[start:, hi] = df.iloc[start:, lo].to_numpy()
    df.iloc[start:, lo] = high.to_numpy()
    return df


def test_invalid_entry_data_keeps_close_and_stop_processing():
    df = _swap_high_low(_frame([100.0] * 20 + [99.0, 101.0] + [101.5] * 6), 22)
    sig = _signals(df)
    assert sig["signal"].iloc[21] == 1
    assert (sig["ao_reason"].iloc[22:] == "inconsistent_hl").all()
    assert (sig["signal"].iloc[22:] == 0).all()
    res = _bt().run(sig, strategy_name=NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    assert len(res["trades"]) == 1
    assert res["trades"][0]["exit_reason"].startswith("time_stop:")

    crash = _swap_high_low(_frame([100.0] * 20 + [99.0, 101.0, 101.5] + [80.0] * 4), 22)
    sig2 = _signals(crash)
    assert (sig2["signal"].iloc[22:] == 0).all()
    res2 = _bt(close_strategies=[{"name": "time_stop", "params": {"max_bars": 50}}]).run(
        sig2, strategy_name=NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    assert res2["trades"][0]["exit_reason"] == "sl"


def _frozen_frame(rows=520):
    manifest = om.load_manifest(MANIFEST)
    dataset = om.dataset_by_key(manifest, "BTC 4h")
    frame, _, _ = om.window_frame(manifest, dataset, "test")
    return frame.iloc[-rows:].copy()


def _parity_cfg(**kw):
    return ParityConfig(strategy_name=NAME, registry="futures", platform="hyperliquid",
                        close_refs=[TIME_STOP], comparison_mode="approximate", **kw)


@pytest.mark.parametrize("window", [200, None])
def test_non_batched_parity_on_frozen_hyperliquid_frame_is_clean(window):
    df = _frozen_frame()
    full = load_registry("futures").apply_strategy(NAME, df, None)
    assert (full["signal"] != 0).sum() >= 10
    frame = compute_parity_frame(df, cfg=_parity_cfg(), window=window)
    result = summarize(frame)
    assert result["bars_compared"] >= 300
    assert result["mismatches"] == 0
    assert result["close_parity"] == "incomplete" and not result["clean"]
    assert (frame["live_signal"] != 0).sum() > 0


def test_batched_parity_admits_explicit_paper_and_matches_solo():
    df = _frozen_frame(260)
    frame = compute_parity_frame(df, cfg=_parity_cfg(batched=True), window=200)
    result = summarize(frame)
    assert result["bars_compared"] > 0
    assert result["mismatches"] == 0 and result["clean"]


def test_production_solo_check_subprocess_refuses_unacknowledged_live_before_any_exchange_call():
    env = {k: v for k, v in os.environ.items() if not k.startswith("HYPERLIQUID")}
    proc = subprocess.run(
        [sys.executable, os.path.join(REPO, "shared_scripts", "check_hyperliquid.py"),
         NAME, "BTC", "4h", "--mode=live"],
        cwd=REPO, capture_output=True, text=True, timeout=120, env=env)
    assert proc.returncode == 1
    payload = json.loads(proc.stdout.strip().splitlines()[-1])
    assert payload["strategy"] == NAME
    assert payload["signal"] == 0
    assert "allow_no_edge" in payload["error"] and "explicit live mode" in payload["error"]


def test_manifest_leg_books_funding_and_execution_spec_for_the_candidate():
    manifest = om.load_manifest(MANIFEST)
    reg = load_registry("futures")
    leg = run_leg(reg, NAME, None, "BTC", "4h", ("2025-09-01", "2026-09-01"),
                  close_strategies=[TIME_STOP], direction="both", stop_loss_atr_mult=1.0,
                  manifest_ctx={"manifest": manifest, "window": "test"},
                  comparison_mode="approximate")
    assert leg["manifest"]["cost_model"] == "execution_spec"
    assert leg["manifest"]["candle_coverage"]["complete"]
    assert leg["manifest"]["funding_coverage"]["complete"]
    ex = leg["execution"]
    assert ex["long_positions"] > 0 and ex["short_positions"] > 0
    assert ex["funding_pnl_usd"] != 0.0
    assert ex["fees_usd"] > 0
