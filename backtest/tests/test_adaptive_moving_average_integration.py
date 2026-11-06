import gzip
import csv
import json
import os
import subprocess
import sys

import numpy as np
import pandas as pd

_HERE = os.path.dirname(os.path.abspath(__file__))
_REPO = os.path.abspath(os.path.join(_HERE, "..", ".."))
_TOOLS = os.path.join(_REPO, "shared_tools")
if _TOOLS not in sys.path:
    sys.path.insert(0, _TOOLS)

from closed_bar import FRAME_TIMESTAMP_KIND_ATTR, declare_frame_cadence
from parity_diff import ParityConfig
from registry_loader import load_registry

NAME = "adaptive_moving_average"
STUDY = os.path.join(_REPO, "backtest", "candidates", "adaptive_moving_average_1641")
DATA = os.path.join(_REPO, "backtest", "candidates", "chande_momentum_1661", "data")
CLOSE = [{"name": "tiered_tp_pct", "params": {}}]
INPUT_COLUMNS = ["open", "high", "low", "close", "volume"]
SEED = {"efficiency_period": 10, "fast_period": 2, "slow_period": 30, "slope_span": 3, "recursion_length": 120}
SMALL = {"efficiency_period": 4, "fast_period": 2, "slow_period": 12, "slope_span": 2, "recursion_length": 20}
DECISION_COLUMNS = ["ama_value", "ama_prev_value", "ama_slope_ref", "ama_slope", "ama_efficiency",
                    "ama_smoothing", "ama_valid", "ama_reason_code", "signal"]


def _reg():
    return load_registry("futures")


def _apply(df, params=None):
    return _reg().apply_strategy(NAME, df, params)


def _need(p):
    return p["recursion_length"] + p["efficiency_period"] + max(p["slope_span"], 1)


def _declare(df, freq="4h", kind="open"):
    return declare_frame_cadence(df, freq, kind)


def _walk(n, seed, start=100.0, freq="4h"):
    rng = np.random.default_rng(seed)
    close = start * np.exp(np.cumsum(rng.normal(0.0, 0.02, n)))
    open_ = np.concatenate([[start], close[:-1]])
    high = np.maximum(open_, close) * (1.0 + rng.uniform(0.001, 0.01, n))
    low = np.minimum(open_, close) * (1.0 - rng.uniform(0.001, 0.01, n))
    volume = rng.uniform(100.0, 1000.0, n)
    df = pd.DataFrame({"open": open_, "high": high, "low": low, "close": close, "volume": volume})
    df.index = pd.date_range("2026-01-01", periods=n, freq=freq)
    return _declare(df, freq)


def _from_closes(closes, freq="4h"):
    c = np.asarray(closes, dtype=float)
    df = pd.DataFrame({"open": c, "high": c + 0.5, "low": c - 0.5, "close": c, "volume": np.full(len(c), 10.0)})
    df.index = pd.date_range("2026-01-01", periods=len(c), freq=freq)
    return _declare(df, freq)


def _frozen(symbol, tf, bars):
    with gzip.open(os.path.join(DATA, f"{symbol}_{tf}_candles.csv.gz"), "rt") as fh:
        rows = list(csv.reader(fh))[1:]
    df = pd.DataFrame(rows, columns=["timestamp"] + INPUT_COLUMNS).astype(
        {"timestamp": "int64", **{c: float for c in INPUT_COLUMNS}})
    df.index = pd.to_datetime(df["timestamp"], unit="ms")
    df = df[INPUT_COLUMNS].iloc[-bars:].copy()
    return _declare(df, tf)


def _ref_ama(c, t, p):
    e, length = p["efficiency_period"], p["recursion_length"]
    fast = 2.0 / (p["fast_period"] + 1.0)
    slow = 2.0 / (p["slow_period"] + 1.0)
    a = c[t - length]
    sc = None
    er = None
    for k in range(t - length + 1, t + 1):
        change = abs(c[k] - c[k - e])
        noise = abs(c[k - e + 1] - c[k - e])
        for i in range(k - e + 2, k + 1):
            noise = noise + abs(c[i] - c[i - 1])
        er = change / noise if noise > 0 else 0.0
        root = er * (fast - slow) + slow
        sc = root * root
        a = a + sc * (c[k] - a)
    return a, er, sc


def _ref_signal(c, t, p):
    s = p["slope_span"]
    a, _, _ = _ref_ama(c, t, p)
    b, _, _ = _ref_ama(c, t - 1, p)
    r, _, _ = _ref_ama(c, t - s, p)
    if c[t] > a and c[t - 1] <= b and a - r > 0:
        return 1, a, b, r
    if c[t] < a and c[t - 1] >= b and a - r < 0:
        return -1, a, b, r
    return 0, a, b, r


def test_values_match_an_independent_scalar_reference_and_both_entry_directions():
    df = _walk(420, 7)
    c = df["close"].tolist()
    seen = set()
    for p in (SMALL, dict(SMALL, slope_span=1, efficiency_period=1), dict(SMALL, fast_period=1, slow_period=2),
              dict(SEED, recursion_length=60)):
        out = _apply(df, p)
        need = _need(p)
        assert (out["ama_valid"].iloc[:need - 1] == 0.0).all()
        assert (out["ama_reason"].iloc[:need - 1] == "insufficient_history").all()
        assert out["ama_valid"].iloc[need - 1] == 1.0
        for t in range(need - 1, len(df)):
            sig, a, b, r = _ref_signal(c, t, p)
            _, er, sc = _ref_ama(c, t, p)
            row = out.iloc[t]
            assert (row["ama_value"], row["ama_prev_value"], row["ama_slope_ref"]) == (a, b, r)
            assert (row["ama_efficiency"], row["ama_smoothing"]) == (er, sc)
            assert row["ama_slope"] == a - r
            assert row["ama_seed_close"] == c[t - p["recursion_length"]]
            assert int(row["signal"]) == sig
            assert row["ama_reason_code"] == {1: 1.0, -1: -1.0}.get(sig, row["ama_reason_code"])
            if sig == 0:
                assert row["ama_reason"] in ("no_cross", "slope_disagrees")
            seen.add(sig)
    assert seen == {-1, 0, 1}


def test_timestamp_column_and_native_close_frames_decide_the_same_way():
    df = _walk(260, 3)
    plain = df.reset_index(drop=True)
    plain["timestamp"] = df.index.as_unit("ms").asi8.astype("int64")
    plain.attrs = dict(df.attrs)
    a = _apply(df, SMALL)
    b = _apply(plain, SMALL)
    np.testing.assert_array_equal(a["signal"].to_numpy(), b["signal"].to_numpy())
    np.testing.assert_array_equal(a["ama_value"].to_numpy(), b["ama_value"].to_numpy())
    native = df.set_axis(df.index + pd.Timedelta(hours=4) - pd.Timedelta(milliseconds=1))
    _declare(native, "4h", "native_close_inclusive")
    c = _apply(native, SMALL)
    np.testing.assert_array_equal(a["signal"].to_numpy(), c["signal"].to_numpy())
    np.testing.assert_array_equal(a["ama_eval_ts_ms"].to_numpy(), c["ama_eval_ts_ms"].to_numpy())
    assert a["ama_eval_ts_ms"].iloc[-1] == df.index[-1].value // 1_000_000
    native.attrs.pop(FRAME_TIMESTAMP_KIND_ATTR)
    d = _apply(native, SMALL)
    np.testing.assert_array_equal(a["signal"].to_numpy(), d["signal"].to_numpy())
    assert d["ama_eval_ts_ms"].isna().all()


def _check_rows_outside_the_dependency_window_cannot_change_a_decision():
    df = _frozen("ETH", "4h", 400)
    p = SEED
    need = _need(p)
    t = 380
    full = _apply(df, p)
    early = df.copy()
    early.iloc[: t - need + 1, early.columns.get_loc("close")] *= 1.5
    early.iloc[: t - need + 1, early.columns.get_loc("high")] *= 1.5
    future = df.copy()
    future.iloc[t + 1:, future.columns.get_loc("volume")] = np.nan
    for mutated in (early, future):
        got = _apply(mutated, p)
        pd.testing.assert_series_equal(got[DECISION_COLUMNS].iloc[t], full[DECISION_COLUMNS].iloc[t],
                                       check_names=False)


def _cfg(params, **kw):
    base = dict(strategy_name=NAME, params=params, registry="futures", platform="hyperliquid",
                symbol="BTC", timeframe="4h")
    base.update(kw)
    return ParityConfig(**base)


_CACHE_SCRIPT = r"""
import json, sys
sys.path.insert(0, sys.argv[2])
import run_study as rs
proto = rs.load_protocol()
_, source = rs.verify_frozen(proto)
drop = json.loads(sys.argv[3])
rs.build_cache(sys.argv[1], proto, source, drop_funding={k: tuple(v) for k, v in drop.items()} or None)
rs.forbid_network()
import pandas as pd
import eval_windows as ew
from data_fetcher import load_cached_data
from registry_loader import load_registry
reg = load_registry("futures")
case = sys.argv[4]
out = {}
if case == "warmup":
    window = ("2025-03-01", "2025-05-01")
    params = dict(proto["seed_params"])
    close = proto["close_strategies"]
    warm = ew.run_leg(reg, "adaptive_moving_average", params, "BTC/USDT", "4h", window, close_strategies=close,
                      direction="both", stop_loss_atr_mult=1.0, warmup_bars=300, keep_positions=True)
    cold = ew.run_leg(reg, "adaptive_moving_average", params, "BTC/USDT", "4h", window, close_strategies=close,
                      direction="both", stop_loss_atr_mult=1.0, keep_positions=True)
    full = load_cached_data("BTC/USDT", "4h", start_date="2024-06-01", end_date=window[1])
    full = full[full.index < pd.Timestamp(window[1])]
    sig = reg.apply_strategy("adaptive_moving_average", full, params)
    inwin = sig[sig.index >= pd.Timestamp(window[0])]
    out["warm"] = {k: warm[k] for k in ("warmup", "trades", "funding_incomplete") if k in warm}
    out["warm_entries"] = [p["entry_date"] for p in warm["positions"]]
    out["cold_entries"] = [p["entry_date"] for p in cold["positions"]]
    out["full_signal_bars"] = [str(t) for t in inwin.index[inwin["signal"].to_numpy() != 0]]
    out["first_window_bar"] = str(inwin.index[0])
    out["warm_funding_expected_hours"] = warm["funding"]["coverage"]["expected_hours"]
    out["cold_funding_expected_hours"] = cold["funding"]["coverage"]["expected_hours"]
else:
    proto["symbols"] = ["BTC/USDT"]
    proto["windows"]["probe"] = ["2025-03-01", "2025-04-01"]
    proto["held_out_windows"] = ["probe"]
    runner = rs.Runner(proto)
    sel = {"ema_crossover": {"fast_period": 12, "slow_period": 26},
           "vol_momentum": {"mom_window": 24, "entry_threshold": 0.3, "eff_entry": 0.35}}
    held = {"probe": runner.evaluate("adaptive_moving_average", proto["seed_params"], "1h", "probe")}
    stress = {"probe": runner.evaluate("adaptive_moving_average", proto["seed_params"], "1h", "probe",
                                       slippage_mult=2.0)}
    comps = {c: {"probe": runner.evaluate(c, sel[c], "1h", "probe")} for c in proto["comparators"]}
    out["verdict"] = rs.group_verdict(proto, held, stress, comps)
    out["legs"] = {"candidate": held["probe"]["legs"], "comparators": {c: comps[c]["probe"]["legs"] for c in comps},
                   "bars": held["probe"]["bars"]}
print(json.dumps(out, default=str))
"""


def _cache_case(tmp_path, case, drop=None):
    proc = subprocess.run(
        [sys.executable, "-c", _CACHE_SCRIPT, str(tmp_path / "cache.db"), STUDY, json.dumps(drop or {}), case],
        capture_output=True, text=True, cwd=os.path.join(_REPO, "backtest"), timeout=1800,
        env={**os.environ, "PYTHONPATH": os.pathsep.join([os.path.join(_REPO, "backtest"), _TOOLS])})
    assert proc.returncode == 0, proc.stderr[-4000:]
    return json.loads(proc.stdout.strip().splitlines()[-1])


def test_cached_m1_warmup_scores_only_the_window_with_warmed_indicators(tmp_path):
    out = _cache_case(tmp_path, "warmup")
    warm = out["warm"]
    assert warm["warmup"] == {"bars": 300, "first_bar": "2025-01-10 00:00:00", "scored_first_bar": "2025-03-01 00:00:00"}
    assert out["first_window_bar"] == "2025-03-01 00:00:00"
    assert min(out["warm_entries"]) >= "2025-03-01"
    expected = [str(pd.Timestamp(b) + pd.Timedelta(hours=4)) for b in out["full_signal_bars"]]
    assert out["warm_entries"] and set(out["warm_entries"]) <= set(expected)
    assert min(out["cold_entries"]) > min(out["warm_entries"])
    assert out["warm_funding_expected_hours"] == out["cold_funding_expected_hours"]


def test_missing_held_hour_funding_makes_every_arm_unscorable_and_the_verdict_inconclusive(tmp_path):
    lo = int(pd.Timestamp("2025-03-01").timestamp() * 1000)
    hi = int(pd.Timestamp("2025-04-01").timestamp() * 1000)
    out = _cache_case(tmp_path, "funding", {"BTC": [lo, hi]})
    verdict = out["verdict"]
    assert verdict["verdict"] == "inconclusive"
    bad = verdict["checks"]["unscorable_windows"]["probe"]
    for label in ("candidate BTC/USDT 1h", "candidate x2 slippage BTC/USDT 1h", "ema_crossover BTC/USDT 1h",
                  "vol_momentum BTC/USDT 1h", "incumbent median BTC/USDT 1h"):
        assert label in bad, bad
    assert verdict["checks"]["scorable_windows"] == []
    assert out["legs"]["candidate"]["BTC/USDT 1h"]["funding"]["unpriced_held_hours"] > 0
    funding = out["legs"]["candidate"]["BTC/USDT 1h"]["funding"]
    assert funding["priced_hours"] == funding["present_hours"] - funding["venue_absent_hours"]
    assert 0 <= funding["priced_hours"] < funding["expected_hours"]
    assert funding["coverage_complete"] is False


def test_decisions_depend_only_on_rows_inside_the_dependency_window(subtests):
    for check in (
            _check_rows_outside_the_dependency_window_cannot_change_a_decision,
    ):
        with subtests.test(case=check.__name__.removeprefix("_check_")):
            check()
