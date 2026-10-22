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
from close_registry_loader import evaluate as close_evaluate
from closed_bar import FRAME_DECISION_TIME_ATTR, declare_frame_cadence, timings_timestamp_kind
from lead_lag_history import LeaderDataUnavailable, cached_leader_params, refuse_leader_input_strategy
from lead_lag_inputs import prepare_historical_inputs, prepare_sealed_inputs, support_bars
from optimizer import DEFAULT_PARAM_RANGES, generate_param_grid, walk_forward_optimize
from registry_loader import load_registry
from strategy_composition import evaluate_open_close, finalize_decision

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


def test_registration_grid_and_validator():
    reg = _reg()
    entry = reg.STRATEGY_REGISTRY[NAME]
    registry = reg._registry
    assert entry["production_refused"] is True and entry["edge_status"] == "no_edge"
    assert NAME not in reg.list_strategies() and NAME not in load_registry("spot").STRATEGY_REGISTRY
    assert NAME in registry.short_entry_strategies() and NAME in registry.DISCOVERY_HIDDEN_STRATEGIES
    grid = generate_param_grid(DEFAULT_PARAM_RANGES[NAME])
    assert len(grid) == 144
    for params in grid:
        registry.validate_params(NAME, {**params, "expected_interval_ms": HOUR})
    for bad in ({"lag_bars": 0}, {"lag_bars": 1.5}, {"lag_bars": True}, {"horizon_bars": 13},
                {"training_bars": 47}, {"training_bars": 241}, {"impulse_z": float("nan")}, {"min_r2": 1.0},
                {"expected_interval_ms": 300_000}, {"signal_mode": "other"}, {"unknown": 1}):
        with pytest.raises(ValueError):
            registry.validate_params(NAME, {**SEED, **bad})


def test_impulse_enters_once_in_its_direction_with_past_only_training_and_mirrors_short():
    for sign, side, want in ((1.0, 1, "entry_long"), (-1.0, -1, "entry_short")):
        follower, leader = _pair(sign=sign)
        out = _run(follower, _ctx(follower, leader))
        entries = out.index[out["signal"] != 0]
        ids = out.loc[entries, "bll_event_id"].tolist()
        assert len(ids) == len(set(ids))
        for j in (150, 200):
            event_ms = T0 + j * HOUR
            hits = out[out["bll_event_open_ms"] == event_ms]
            fired = hits[hits["signal"] != 0]
            assert len(fired) == 1, (sign, j)
            row = fired.iloc[0]
            assert row["signal"] == side and row["bll_reason"] == want
            assert row["bll_event_id"] == f"BTC:1h:{event_ms}:{'up' if side > 0 else 'down'}:lead_lag"
            assert row["bll_train_last_ms"] + (SEED["lag_bars"] + SEED["horizon_bars"] - 1) * HOUR <= row["bll_decision_open_ms"]
            assert row["bll_slope"] > 0 and row["bll_r2"] >= SEED["min_r2"]
            assert side * row["bll_remaining_bps"] > SEED["cost_bps"] + SEED["min_edge_bps"]
            assert row["bll_event_z"] >= SEED["impulse_z"] and len(row["bll_support_sha256"]) == 64
            later = out.index.get_loc(fired.index[0]) + 1
            if out["bll_event_open_ms"].iloc[later] == event_ms:
                assert out["bll_reason"].iloc[later] == "event_consumed" and out["signal"].iloc[later] == 0


def test_every_prefix_tail_and_future_perturbation_keep_earlier_decisions():
    follower, leader = _pair()
    ctx = _ctx(follower, leader)
    full = _run(follower, ctx)
    cols = ["signal", "bll_reason", "bll_event_id"]
    for i in range(S, len(follower)):
        prefix = _run(follower.iloc[:i + 1], ctx)
        assert prefix[cols].iloc[-1].tolist() == full[cols].iloc[i].tolist(), i
        tail = _run(follower.iloc[i - S + 1:i + 1], ctx)
        assert tail[cols].iloc[-1].tolist() == full[cols].iloc[i].tolist(), i
        if i % 37 == 0:
            short = _run(follower.iloc[i - S + 2:i + 1], ctx)
            assert short["bll_reason"].iloc[-1] == "insufficient_history"
    lagged = {"lag_bars": 3, "horizon_bars": 2}
    s3 = support_bars(3, 2, 48)
    full3 = _run(follower, ctx, **lagged)
    fired = full3[full3["signal"] != 0]
    assert (fired["bll_forecast_start_ms"] > fired["bll_decision_open_ms"]).any()
    for i in range(s3, len(follower)):
        tail = _run(follower.iloc[i - s3 + 1:i + 1], ctx, **lagged)
        assert tail[cols + ["bll_forecast_start_ms"]].iloc[-1].fillna(-1).tolist() == \
            full3[cols + ["bll_forecast_start_ms"]].iloc[i].fillna(-1).tolist(), i
    cut = 170
    pf, pl = follower.copy(), leader.copy()
    pf.iloc[cut:, pf.columns.get_loc("close")] *= 1.3
    pl.iloc[cut:, pl.columns.get_loc("close")] *= 0.7
    perturbed = _run(pf, _ctx(pf, pl))
    assert perturbed[cols].iloc[:cut].equals(full[cols].iloc[:cut])


def test_missing_leader_rows_gaps_and_unmatched_inputs_hold_without_forward_fill():
    follower, leader = _pair()
    full = _run(follower, _ctx(follower, leader))
    entry = int(np.flatnonzero(full["signal"].to_numpy() != 0)[0])
    holed = leader.drop(leader.index[entry - 5])
    out = _run(follower, _ctx(follower, holed))
    assert out["bll_reason"].iloc[entry - 5] == "leader_bar_missing"
    assert out["signal"].iloc[entry] == 0 and out["bll_reason"].iloc[entry] == "insufficient_history"
    for reason, ctx in (("leader_unavailable", None), ("follower_is_leader", _ctx(leader, leader, "BTC/USDT"))):
        held = _run(follower, ctx)
        assert set(held["bll_reason"]) == {reason} and not held["signal"].any()
    dup = pd.concat([leader.iloc[:10], leader.iloc[9:]])
    ctx = _ctx(follower, dup)
    assert ctx["available"] is False and ctx["hold_reason"] == "duplicate_bar"
    rev = leader.iloc[::-1]
    assert _ctx(follower, rev)["hold_reason"] == "reordered_bar"


def _sealed(follower, leader, i, *, rows=S, cutoff_delay=1_000, recv=None, mutate=None):
    def build(df):
        out, timing = [], []
        for k in range(i - rows + 1, i + 2):
            open_ms = int(df["timestamp"].iloc[k])
            closed = k <= i
            r = df.iloc[k]
            out.append([open_ms + HOUR - 1 if closed else open_ms, float(r["open"]), float(r["high"]), float(r["low"]),
                        float(r["close"]), float(r["volume"])])
            timing.append([open_ms, open_ms + HOUR - 1, closed])
        return {"rows": out, "required": len(out), "bars": len(out), "coverage_short": False,
                "first_open_ms": timing[0][0], "last_open_ms": timing[-1][0], "last_close_ms": timing[-1][1],
                "last_recv_at_ms": cutoff if recv is None else recv, "source": "ws", "ready": True,
                "forming_bar_included": True,
                "timing": {"rule": "hyperliquid_native_close", "interval_ms": HOUR, "bars": timing}}
    boundary = int(follower["timestamp"].iloc[i]) + HOUR
    cutoff = boundary + cutoff_delay
    market = {"version": 4, "snapshot_id": f"t/{boundary}", "generation": 1, "sealed_at_ms": cutoff,
              "decision_cutoff_ms": cutoff, "feed_complete": True,
              "frames": {"ETH|1h": build(follower), "BTC|1h": build(leader)}}
    if mutate:
        mutate(market)
    return market


def _sealed_decision(market, side="", ctx_pos=None):
    rows, timings, ctx = prepare_sealed_inputs(market, "ETH", "1h", S)
    reg = _reg()
    if rows is not None:
        df = pd.DataFrame(rows, columns=["timestamp", "open", "high", "low", "close", "volume"])
        df.index = pd.to_datetime(df["timestamp"], unit="ms")
        declare_frame_cadence(df, "1h", timings_timestamp_kind(rows, timings))
        df.attrs[FRAME_DECISION_TIME_ATTR] = {"cutoff_ms": ctx["cutoff_ms"], "boundary_ms": ctx["boundary_ms"]}
    else:
        raw = market["frames"]["ETH|1h"]["rows"]
        df = pd.DataFrame(raw, columns=["timestamp", "open", "high", "low", "close", "volume"])
        df.index = pd.to_datetime(df["timestamp"], unit="ms")
    tp = {"tp_tiers": [{"profit_pct": 0.03, "close_fraction": 1.0}]}
    evaluation = evaluate_open_close(reg.apply_strategy, reg.get_strategy, df, NAME, NAME, ["tiered_tp_pct"], side,
                                     params={**SEED, "leader_context": ctx}, position_ctx=ctx_pos,
                                     close_evaluate=close_evaluate, close_params_by_name={"tiered_tp_pct": tp})
    return ctx, evaluation.open_result_df.iloc[-1], finalize_decision(evaluation, side)


def test_sealed_payload_decisions_match_history_and_every_defect_holds_the_entry_only():
    follower, leader = _pair()
    full = _run(follower, _ctx(follower, leader))
    entry = int(np.flatnonzero(full["signal"].to_numpy() != 0)[0])
    for i in (entry, entry + 1, entry + 7):
        ctx, last, decision = _sealed_decision(_sealed(follower, leader, i))
        assert ctx["available"] and last["bll_event_id"] == full["bll_event_id"].iloc[i]
        assert decision["open_action"] == {1: "long", -1: "short", 0: "none"}[int(full["signal"].iloc[i])]
        assert ctx["boundary_ms"] == int(follower["timestamp"].iloc[i]) + HOUR

    def drop_timing(m):
        m["frames"]["BTC|1h"].pop("timing")

    def duplicate(m):
        f = m["frames"]["BTC|1h"]
        f["rows"][5], f["timing"]["bars"][5] = list(f["rows"][4]), list(f["timing"]["bars"][4])

    def reorder(m):
        f = m["frames"]["BTC|1h"]
        f["rows"][5], f["rows"][6] = f["rows"][6], f["rows"][5]
        f["timing"]["bars"][5], f["timing"]["bars"][6] = f["timing"]["bars"][6], f["timing"]["bars"][5]

    def not_ready(m):
        m["frames"]["BTC|1h"]["ready"] = False

    def stale(m):
        m["frames"]["BTC|1h"]["stale"] = True

    def missing(m):
        m["frames"].pop("BTC|1h")

    def malformed(m):
        m["frames"]["BTC|1h"]["rows"][3] = [1, 2, 3]

    def shorter(m):
        f = m["frames"]["BTC|1h"]
        f["rows"], f["timing"]["bars"] = f["rows"][3:], f["timing"]["bars"][3:]

    def lagging(m):
        f = m["frames"]["BTC|1h"]
        f["rows"], f["timing"]["bars"] = f["rows"][:-2], f["timing"]["bars"][:-2]

    def nonfinite(m):
        m["frames"]["BTC|1h"]["rows"][10][4] = float("nan")

    def gap(m):
        for key in ("ETH|1h", "BTC|1h"):
            f = m["frames"][key]
            del f["rows"][10]
            del f["timing"]["bars"][10]

    cases = {"timing_invalid": drop_timing, "duplicate_bar": duplicate, "reordered_bar": reorder,
             "frame_not_ready": not_ready, "frame_stale": stale, "frame_missing": missing,
             "payload_invalid": malformed, "insufficient_history": shorter, "boundary_mismatch": lagging,
             "invalid_row": nonfinite, "bar_gap": gap}
    pos = {"side": "long", "avg_cost": float(follower["close"].iloc[entry]) / 1.05, "entry_atr": 1.0,
           "current_quantity": 1.0, "initial_quantity": 1.0}
    for want, mutate in cases.items():
        rows = S + 1 if want in ("boundary_mismatch", "bar_gap") else S
        ctx, last, decision = _sealed_decision(_sealed(follower, leader, entry, rows=rows, mutate=mutate))
        assert ctx["available"] is False and ctx["hold_reason"] == want, (want, ctx)
        assert last["bll_reason"] == "leader_unavailable" and decision["open_action"] == "none"
        _, _, held = _sealed_decision(_sealed(follower, leader, entry, rows=rows, mutate=mutate), "long", pos)
        assert held["close_strategy"] == "tiered_tp_pct" and held["close_fraction"] == 1.0 and held["signal"] == -1

    for kwargs, want in (({"cutoff_delay": 400_000}, "event_stale"), ({"recv": 10**15}, "receipt_future"),
                         ({"recv": 1}, "receipt_stale")):
        ctx, last, decision = _sealed_decision(_sealed(follower, leader, entry, **kwargs))
        assert ctx["available"] and last["bll_reason"] == want and decision["open_action"] == "none"
        assert full["signal"].iloc[entry] != 0
    a = _sealed_decision(_sealed(follower, leader, entry))[1]
    b = _sealed_decision(_sealed(follower, leader, entry))[1]
    assert a[["signal", "bll_reason", "bll_event_id", "bll_support_sha256"]].tolist() == \
        b[["signal", "bll_reason", "bll_event_id", "bll_support_sha256"]].tolist()


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
