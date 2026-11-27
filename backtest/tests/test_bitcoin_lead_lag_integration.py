import json
import os
import subprocess
import sys

import numpy as np
import pandas as pd
import pytest

_HERE = os.path.dirname(os.path.abspath(__file__))
_REPO = os.path.abspath(os.path.join(_HERE, "..", ".."))
for _p in (os.path.join(_REPO, "shared_tools"), os.path.join(_REPO, "backtest")):
    if _p not in sys.path:
        sys.path.insert(0, _p)

from atr import ensure_atr_indicator
from backtester import Backtester
from closed_bar import declare_frame_cadence
from lead_lag_history import LeaderDataUnavailable, cached_leader_params, refuse_leader_input_strategy
from lead_lag_inputs import prepare_historical_inputs, support_bars
from optimizer import walk_forward_optimize
from registry_loader import load_registry

NAME = "bitcoin_lead_lag"
HOUR = 3_600_000
T0 = int(pd.Timestamp("2026-01-01T00:00:00Z").value // 1_000_000)
SEED = {"lag_bars": 1, "horizon_bars": 2, "training_bars": 48, "impulse_z": 2.0, "min_r2": 0.01,
        "min_edge_bps": 0.0, "cost_bps": 10.0, "max_data_age_ms": 300_000}
S = support_bars(1, 2, 48)


def _reg():
    return load_registry("futures")


def _frame(returns, start=T0):
    close = 100.0 * np.exp(np.cumsum(returns))
    open_ = np.concatenate([[close[0]], close[:-1]])
    idx = pd.to_datetime(start + np.arange(len(returns)) * HOUR, unit="ms")
    df = pd.DataFrame({"open": open_, "high": np.maximum(open_, close) * 1.002,
                       "low": np.minimum(open_, close) * 0.998, "close": close, "volume": 10.0}, index=idx)
    df.index.name = "datetime"
    df["timestamp"] = start + np.arange(len(returns)) * HOUR
    return declare_frame_cadence(df, "1h", "open")


def _pair(n=260, seed=7, impulses=(150, 200), sign=1.0, response=0.6):
    rng = np.random.default_rng(seed)
    rb = rng.normal(0.0, 0.004, n)
    for j in impulses:
        rb[j] = sign * 0.03
    rf = np.concatenate([[0.0], response * rb[:-1]]) + rng.normal(0.0, 0.0015, n)
    return _frame(rf), _frame(rb)


def _ctx(follower, leader, symbol="ETH/USDT"):
    return prepare_historical_inputs(follower, leader, symbol, "1h")


def _run(follower, ctx, **over):
    return _reg().apply_strategy(NAME, follower, {**SEED, **over, "leader_context": ctx})


def test_simulator_fills_next_open_both_sides_and_the_close_owner_exits():
    rng = np.random.default_rng(11)
    n = 1400
    rb = rng.normal(0.0, 0.004, n)
    for j in range(120, n - 10, 23):
        rb[j] = 0.03 if (j // 23) % 2 else -0.03
    rf = np.concatenate([[0.0], 0.6 * rb[:-1]]) + rng.normal(0.0, 0.0015, n)
    follower, leader = _frame(rf), _frame(rb)
    sig = ensure_atr_indicator(_run(follower, _ctx(follower, leader)))
    assert (sig["signal"] == 1).any() and (sig["signal"] == -1).any()
    bt = Backtester(initial_capital=1000.0, platform="hyperliquid",
                    open_strategy={"name": NAME, "params": dict(SEED)},
                    close_strategies=[{"name": "tiered_tp_atr", "params": {"tp_tiers": [{"atr_multiple": 1.0, "close_fraction": 1.0}]}}],
                    stop_loss_atr_mult=1.0, direction="both")
    res = bt.run(sig, strategy_name=NAME, symbol="ETH/USDT", timeframe="1h", save=False)
    trades = res["trades"]
    assert {t["side"] for t in trades} == {"long", "short"}
    for t in trades:
        pos = sig.index.get_loc(pd.Timestamp(t["entry_date"]))
        assert int(sig["signal"].iloc[pos - 1]) == (1 if t["side"] == "long" else -1)
        assert t["entry_price"] == pytest.approx(follower["open"].iloc[pos], rel=0.01)


def test_cached_and_manifest_attachment_and_unsupported_paths(monkeypatch):
    import storage
    import eval_windows
    follower, leader = _pair(n=400)
    frames = {"ETH/USDT": follower, "BTC/USDT": leader}

    def fake_load(exchange, symbol, timeframe, start_ts=None, end_ts=None, db_path=None):
        df = frames[symbol].drop(columns=["timestamp"]).copy()
        df["timestamp"] = frames[symbol]["timestamp"]
        if start_ts is not None:
            df = df[df["timestamp"] >= start_ts]
        if end_ts is not None:
            df = df[df["timestamp"] <= end_ts]
        return df
    monkeypatch.setattr(storage, "load_ohlcv", fake_load)
    params, info = cached_leader_params(follower, "ETH/USDT", "1h", follower.index[0])
    assert info["available"] and info["coverage"]["leader_rows_matched"] == len(follower)
    assert info["source"]["symbol"] == "BTC/USDT"
    full = _run(follower, _ctx(follower, leader))
    via = _reg().apply_strategy(NAME, follower, {**SEED, **params})
    assert via["signal"].tolist() == full["signal"].tolist()
    frames["BTC/USDT"] = leader.iloc[:-30]
    with pytest.raises(LeaderDataUnavailable, match="no usable row for 30 of 400 follower bars"):
        cached_leader_params(follower, "ETH/USDT", "1h", follower.index[0])
    shared_gap = follower.drop(follower.index[200:209])
    frames["BTC/USDT"] = leader.drop(leader.index[200:209])
    params, info = cached_leader_params(shared_gap, "ETH/USDT", "1h", follower.index[0])
    assert info["coverage"]["leader_rows_matched"] == len(shared_gap)
    frames["BTC/USDT"] = leader.iloc[:0]
    with pytest.raises(LeaderDataUnavailable):
        cached_leader_params(follower, "ETH/USDT", "1h", follower.index[0])

    import offline_manifest as om
    manifest = om.load_manifest(os.path.join(_REPO, "backtest", "candidates", "bitcoin_lead_lag_1640_manifest_4h.json"))
    leg = eval_windows.run_leg(_reg(), NAME, {**SEED, "training_bars": 120}, "ETH", "4h", (None, None),
                               close_strategies=[{"name": "tiered_tp_atr", "params": {"tp_tiers": [{"atr_multiple": 1.0, "close_fraction": 1.0}]}}],
                               direction="both", stop_loss_atr_mult=1.0,
                               manifest_ctx={"manifest": manifest, "window": "train", "cost_multiplier": 1.0})
    leader_info = leg["manifest"]["leader"]
    assert leader_info["available"] and leader_info["source"]["dataset"] == "BTC 4h"
    cov = leader_info["coverage"]
    assert cov["leader_rows_matched"] == cov["follower_rows"] == manifest["warmup_bars"] + leg["manifest"]["candle_coverage"]["present_bars"]
    assert not leg.get("funding_incomplete")

    for consumer in ("walk-forward", "exit policy"):
        with pytest.raises(ValueError, match="pinned BTC leader series"):
            refuse_leader_input_strategy(NAME, consumer)
    with pytest.raises(ValueError, match="pinned BTC leader series"):
        walk_forward_optimize(follower, NAME, {"lag_bars": [1]}, n_splits=2, timeframe="1h", registry="futures")
    proc = subprocess.run([sys.executable, os.path.join(_REPO, "backtest", "run_backtest.py"), "--mode", "optimize",
                           "--strategy", NAME, "--registry", "futures", "--symbol", "ETH/USDT", "--timeframe", "1h"],
                          capture_output=True, text=True, cwd=_REPO, timeout=240)
    assert proc.returncode == 1 and "pinned BTC leader series" in proc.stdout


def test_production_refusal_keeps_the_candidate_out_of_every_check_before_input_work():
    from strategy_composition import GATE_MODE_PAPER, Acknowledgement, GateMode, admit_configured_strategies
    from close_registry_loader import get_strategy as get_close
    reg = _reg()
    with pytest.raises(ValueError, match=f"open strategy '{NAME}' is production_refused"):
        admit_configured_strategies(NAME, None, None, GateMode(GATE_MODE_PAPER), Acknowledgement(True),
                                    reg.get_strategy, get_close)
    discovery = json.loads(subprocess.run(
        [sys.executable, os.path.join(_REPO, "shared_strategies", "open", "futures", "strategies.py"), "--list-json"],
        capture_output=True, text=True, check=True, cwd=_REPO, timeout=120).stdout)
    assert NAME not in {row["id"] for row in discovery}
