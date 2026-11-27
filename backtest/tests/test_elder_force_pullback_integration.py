import importlib.util
import json
import math
import os
import sys

import numpy as np
import pandas as pd
import pytest

_HERE = os.path.dirname(os.path.abspath(__file__))
_REPO = os.path.abspath(os.path.join(_HERE, "..", ".."))
_TOOLS = os.path.join(_REPO, "shared_tools")
if _TOOLS not in sys.path:
    sys.path.insert(0, _TOOLS)

import eval_windows
from closed_bar import (
    TIMESTAMP_KIND_OPEN,
    declare_frame_cadence,
)
from optimizer import DEFAULT_PARAM_RANGES
from parity_diff import ParityConfig
from registry_loader import load_registry
from strategy_composition import (
    StrategyReference,
    preflight_reference_params,
)

NAME = "elder_force_pullback"
STUDY_DIR = os.path.join(_REPO, "backtest", "candidates", "elder_force_pullback_1668")
SEED = {"force_period": 13, "trend_period": 50, "slope_span": 5, "recursion_length": 150, "volume_weighting": 1}
TIERS = [{"profit_pct": 0.01, "close_fraction": 0.25}, {"profit_pct": 0.02, "close_fraction": 0.5},
         {"profit_pct": 0.03, "close_fraction": 0.75}, {"profit_pct": 0.04, "close_fraction": 1.0}]
CLOSE = [{"name": "tiered_tp_pct", "params": {"tp_tiers": TIERS}}]
CROSS_AT = 211
FIRST_PULLBACK = 200
def _reg():
    return load_registry("futures")


def _frame(close, volume=None, start="2026-01-01", kind=TIMESTAMP_KIND_OPEN):
    close = np.asarray(close, dtype=float)
    open_ = np.concatenate(([close[0]], close[:-1]))
    vol = np.full(len(close), 100.0) if volume is None else np.asarray(volume, dtype=float)
    df = pd.DataFrame({"open": open_, "high": np.maximum(open_, close) + 0.5,
                       "low": np.minimum(open_, close) - 0.5, "close": close, "volume": vol},
                      index=pd.date_range(start, periods=len(close), freq="1h"))
    df.index.name = "datetime"
    return declare_frame_cadence(df, "1h", kind)


def _pullback(sign=1, n=260):
    step = np.full(n, 0.5)
    step[FIRST_PULLBACK:FIRST_PULLBACK + 5] = -2.0
    step[0] = 0.0
    return 100.0 + np.cumsum(step) if sign > 0 else 400.0 - np.cumsum(step)


def _walk(n, seed):
    rs = np.random.default_rng(seed)
    t = np.arange(n)
    drift = np.where((t // 150) % 2 == 0, 0.3, -0.3)
    close = 300.0 + np.cumsum(drift + rs.normal(0.0, 1.0, n))
    return _frame(close, rs.uniform(100.0, 1000.0, n))


def _reference(df, params):
    close = [float(x) for x in df["close"]]
    volume = [float(x) for x in df["volume"]]
    n = len(close)
    p, trend, span, length = (params["force_period"], params["trend_period"], params["slope_span"],
                              params["recursion_length"])
    weighted = params["volume_weighting"] == 1
    a = 2.0 / (p + 1.0)
    raw = [math.nan] + [(close[i] - close[i - 1]) * (volume[i] if weighted else 1.0) for i in range(1, n)]
    force = [math.nan] * n
    for t in range(length, n):
        e = raw[t - length + 1]
        for k in range(t - length + 2, t + 1):
            e = a * raw[k] + (1.0 - a) * e
        force[t] = e
    sma = [math.nan] * n
    for t in range(trend - 1, n):
        total = close[t - trend + 1]
        for j in range(t - trend + 2, t + 1):
            total = total + close[j]
        sma[t] = total / float(trend)
    need = max(length + 2, trend + span + 1)
    rows = []
    for t in range(n):
        valid = t >= need - 1
        sig = 0
        if valid:
            up = all(close[x] > sma[x] > sma[x - span] for x in (t - 1, t))
            down = all(close[x] < sma[x] < sma[x - span] for x in (t - 1, t))
            vol_ok = volume[t] > 0 and volume[t - 1] > 0
            if force[t - 1] < 0 < force[t] and up and vol_ok:
                sig = 1
            elif force[t - 1] > 0 > force[t] and down and vol_ok:
                sig = -1
        rows.append({"valid": valid, "signal": sig, "force": force[t], "sma": sma[t], "raw": raw[t]})
    return rows


def _check_check_preflight_refuses_invalid_open_and_close_fallback_parameters():
    reg = _reg()
    refs = [StrategyReference("open", "breakout"), StrategyReference("close", NAME)]
    preflight_reference_params(refs, {}, {NAME: {}}, reg.get_strategy, reg.validate_params, timeframe="1h")
    with pytest.raises(ValueError) as exc:
        preflight_reference_params(refs, {}, {NAME: {"recursion_length": 40}}, reg.get_strategy,
                                   reg.validate_params, timeframe="1h")
    assert str(exc.value).startswith(f"close strategy '{NAME}' has invalid parameters")
    open_refs = [StrategyReference("open", NAME)]
    with pytest.raises(ValueError, match=f"open strategy '{NAME}' has invalid parameters"):
        preflight_reference_params(open_refs, {"volume_weighting": True}, {}, reg.get_strategy, reg.validate_params)


def _check_research_legs_refuse_invalid_parameters_before_loading_candles(monkeypatch):
    import data_fetcher

    def refuse(*_a, **_k):
        raise AssertionError("candles were loaded before the parameters were validated")

    monkeypatch.setattr(data_fetcher, "load_cached_data", refuse)
    with pytest.raises(ValueError, match="recursion_length"):
        eval_windows.run_leg(_reg(), NAME, {"recursion_length": 40}, "BTC/USDT", "4h", ("2026-01-01", "2026-02-01"))


def test_invalid_parameters_are_refused_before_evaluation_or_candle_load(subtests, monkeypatch):
    for label, check in (
        ("check_preflight_refuses_invalid_open_and_close_fallback_parameters", _check_check_preflight_refuses_invalid_open_and_close_fallback_parameters),
        ("research_legs_refuse_invalid_parameters_before_loading_candles", lambda: _check_research_legs_refuse_invalid_parameters_before_loading_candles(monkeypatch)),
    ):
        with subtests.test(case=label):
            check()


def _cfg(**kw):
    base = dict(strategy_name=NAME, params=dict(SEED), registry="futures", platform="hyperliquid", symbol="BTC",
                timeframe="1h", close_refs=json.loads(json.dumps(CLOSE)), direction="both",
                stop_kwargs={"stop_loss_atr_mult": 1.0}, comparison_mode="strict")
    base.update(kw)
    return ParityConfig(**base)


def _load(name):
    path = os.path.join(STUDY_DIR, f"{name}.py")
    spec = importlib.util.spec_from_file_location(f"_efp_1668_{name}", path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def _study():
    return _load("run_study")


def test_study_protocol_lock_grid_and_verdict_rules():
    study = _study()
    seed = json.load(open(os.path.join(STUDY_DIR, "candidate_seed.json")))
    assert seed["params"] == SEED and seed["optimizer_grid"] == {k: list(v) for k, v in DEFAULT_PARAM_RANGES[NAME].items()}
    assert seed["close_strategies"] == CLOSE and seed["incumbent_contract"]["close_strategies"] == CLOSE
    assert study.verify_lock()["candidate_seed_sha256"] == study._sha(study.SEED_PATH)
    assert study.check_grid(_reg())["all_valid"]
    assert study.ABLATION == "elder_force_pullback_vw0" and study.paired_params(study.ABLATION, SEED)[
        "volume_weighting"] == 0
    assert set(study.COMPARATORS) == {"elder_force_pullback_vw0", "momentum_pro", "elder_ray_pullback"}

    def arm(ip, sharpe=1.0, ddadj=1.0, net=10.0, longs=5, shorts=5):
        block = {"independent_positions": ip, "mean_sharpe": sharpe, "mean_ddadj": ddadj, "net_pnl_usd": net,
                 "long_positions": longs, "short_positions": shorts, "coverage_complete": True}
        return {"selected": {"base": block, "stress": dict(block)}}

    gate = {"complete": True, "refused": []}
    held = {n: arm(40, sharpe=0.5, ddadj=0.5) for n in study.ARMS}
    held[study.CANDIDATE] = arm(40)
    assert study.verdict(gate, {}, held)["outcome"] == "pass"
    assert study.verdict(gate, {}, held)["edge_source"] == "unvalidated"
    held[study.CANDIDATE] = arm(40, sharpe=0.4)
    assert study.verdict(gate, {}, held)["edge_source"] == "study_fail"
    held[study.CANDIDATE] = arm(20)
    out = study.verdict(gate, {}, held)
    assert out["outcome"] == "inconclusive" and out["edge_source"] == "study_inconclusive"
    held[study.CANDIDATE] = arm(40, shorts=0)
    assert study.verdict(gate, {}, held)["outcome"] == "inconclusive"
    held[study.CANDIDATE] = arm(40)
    held["momentum_pro"] = {"selected": None}
    assert study.verdict(gate, {}, held)["outcome"] == "inconclusive"
    assert study.verdict({"complete": False, "refused": [{"dataset": "BTC 4h", "window": "test"}]}, {}, {})[
        "outcome"] == "inconclusive"
