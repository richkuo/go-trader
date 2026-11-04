import json
import os
import subprocess
import sys

import numpy as np
import pandas as pd

import offline_manifest as om
from atr import ensure_atr_indicator
from backtester import Backtester
from eval_windows import run_leg
from parity_diff import ParityConfig
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


def _swap_high_low(df, start):
    hi, lo = df.columns.get_loc("high"), df.columns.get_loc("low")
    high = df.iloc[start:, hi].copy()
    df.iloc[start:, hi] = df.iloc[start:, lo].to_numpy()
    df.iloc[start:, lo] = high.to_numpy()
    return df


def _frozen_frame(rows=520):
    manifest = om.load_manifest(MANIFEST)
    dataset = om.dataset_by_key(manifest, "BTC 4h")
    frame, _, _ = om.window_frame(manifest, dataset, "test")
    return frame.iloc[-rows:].copy()


def _parity_cfg(**kw):
    return ParityConfig(strategy_name=NAME, registry="futures", platform="hyperliquid",
                        close_refs=[TIME_STOP], comparison_mode="approximate", **kw)


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
