import csv
import gzip
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

from closed_bar import (
    declare_frame_cadence,
)
from parity_diff import ParityConfig

NAME = "true_strength_index"
STUDY = os.path.join(_REPO, "backtest", "candidates", "true_strength_index_1667")
DATA = os.path.join(_REPO, "backtest", "candidates", "chande_momentum_1661", "data")
INPUT_COLUMNS = ["open", "high", "low", "close", "volume"]
SEED = {"slow_period": 25, "fast_period": 13, "signal_period": 7, "recursion_length": 180}
SMALL = {"slow_period": 3, "fast_period": 2, "signal_period": 2, "recursion_length": 18}
REL = 1e-10
DECISION_COLUMNS = ["tsi_value", "tsi_prev_value", "tsi_signal_line", "tsi_prev_signal_line", "tsi_diff",
                    "tsi_prev_diff", "tsi_slow_change", "tsi_double_change", "tsi_slow_abs", "tsi_double_abs",
                    "tsi_signal_seed_change", "tsi_valid", "tsi_value_valid", "tsi_reason_code", "signal"]


def _close_refs():
    with open(os.path.join(STUDY, "protocol.json")) as fh:
        return json.load(fh)["close_strategies"]


def _need(p):
    return p["recursion_length"] + 1


def _declare(df, freq="4h", kind="open"):
    return declare_frame_cadence(df, freq, kind)


def _walk(n, seed, start=100.0, freq="4h", vol=0.02):
    rng = np.random.default_rng(seed)
    close = start * np.exp(np.cumsum(rng.normal(0.0, vol, n)))
    open_ = np.concatenate([[start], close[:-1]])
    high = np.maximum(open_, close) * (1.0 + rng.uniform(0.001, 0.01, n))
    low = np.minimum(open_, close) * (1.0 - rng.uniform(0.001, 0.01, n))
    volume = rng.uniform(100.0, 1000.0, n)
    df = pd.DataFrame({"open": open_, "high": high, "low": low, "close": close, "volume": volume})
    df.index = pd.date_range("2026-01-01", periods=n, freq=freq)
    return _declare(df, freq)


def _from_closes(closes, freq="4h"):
    c = np.asarray(closes, dtype=float)
    df = pd.DataFrame({"open": c, "high": c * 1.001, "low": c * 0.999, "close": c, "volume": np.full(len(c), 10.0)})
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


def _ref_endpoint(c, t, p):
    a = 2.0 / (p["slow_period"] + 1.0)
    b = 2.0 / (p["fast_period"] + 1.0)
    g = 2.0 / (p["signal_period"] + 1.0)
    length = p["recursion_length"]
    window = [float(x) for x in c[t - length + 1:t + 1]]
    changes = [window[i] - window[i - 1] for i in range(1, len(window))]
    slow = fast = changes[0]
    slow_abs = fast_abs = abs(changes[0])
    line = None
    seed_at = None
    tsi = None
    for j, d in enumerate(changes):
        if j > 0:
            slow = a * d + (1.0 - a) * slow
            fast = b * slow + (1.0 - b) * fast
            slow_abs = a * abs(d) + (1.0 - a) * slow_abs
            fast_abs = b * slow_abs + (1.0 - b) * fast_abs
        tsi = 100.0 * (fast / fast_abs) if fast_abs > 0 else None
        if line is None:
            if tsi is not None:
                line, seed_at = tsi, j
        else:
            line = g * tsi + (1.0 - g) * line
    return {"tsi": tsi, "line": line, "slow": slow, "fast": fast, "slow_abs": slow_abs, "fast_abs": fast_abs,
            "seed_at": seed_at, "first": changes[0]}


def _ref_signal(c, t, p):
    cur = _ref_endpoint(c, t, p)
    prev = _ref_endpoint(c, t - 1, p)
    diff, pdiff = cur["tsi"] - cur["line"], prev["tsi"] - prev["line"]
    if diff > 0 and pdiff <= 0 and cur["tsi"] > 0:
        return 1, cur, prev
    if diff < 0 and pdiff >= 0 and cur["tsi"] < 0:
        return -1, cur, prev
    return 0, cur, prev


REFERENCE_PARAMS = [SMALL, dict(SMALL, signal_period=5), dict(SMALL, slow_period=4, fast_period=3,
                                                               recursion_length=24)]


def _set_at(df, pos, col, value):
    out = df.copy()
    out.iloc[pos, out.columns.get_loc(col)] = value
    return out


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
proto["symbols"] = ["BTC/USDT"]
proto["windows"]["probe"] = ["2025-03-01", "2025-04-01"]
proto["held_out_windows"] = ["probe"]
proto["arms"] = {a: proto["arms"][a] for a in proto["verdict_rules"]["required_arms"]}
runner = rs.Runner(proto)
reg = runner.reg.STRATEGY_REGISTRY
sel = {a: ({k: proto["seed_params"][k] for k in proto["seed_params"]} if a == proto["candidate_arm"] else
           {k: reg[s["name"]]["default_params"][k] for k in proto["grids"][s["name"]]})
       for a, s in proto["arms"].items()}
g = {"selections": {a: {"how": "selected", "params": p} for a, p in sel.items()}}
g["held_out"] = {a: {"probe": runner.evaluate(a, sel[a], "1h", "probe")} for a in proto["arms"]}
g["held_out_x2_slippage"] = {a: {"probe": runner.evaluate(a, sel[a], "1h", "probe", slippage_mult=2.0)}
                             for a in proto["stress_arms"]}
out = {"verdict": rs.group_verdict(proto, g, True, True),
       "legs": {a: g["held_out"][a]["probe"]["legs"] for a in proto["arms"]}}
print(json.dumps(out, default=str))
"""


def _cache_case(tmp_path, drop=None):
    proc = subprocess.run(
        [sys.executable, "-c", _CACHE_SCRIPT, str(tmp_path / "cache.db"), STUDY, json.dumps(drop or {})],
        capture_output=True, text=True, cwd=os.path.join(_REPO, "backtest"), timeout=1800,
        env={**os.environ, "PYTHONPATH": os.pathsep.join([os.path.join(_REPO, "backtest"), _TOOLS])})
    assert proc.returncode == 0, proc.stderr[-4000:]
    return json.loads(proc.stdout.strip().splitlines()[-1])


def _missing_held_hour_funding_makes_every_required_arm_unscorable_and_the_verdict_inconclusive(tmp_path):
    lo = int(pd.Timestamp("2025-03-01").timestamp() * 1000)
    hi = int(pd.Timestamp("2025-04-01").timestamp() * 1000)
    out = _cache_case(tmp_path, {"BTC": [lo, hi]})
    verdict = out["verdict"]
    assert verdict["verdict"] == "inconclusive"
    assert verdict["gates"]["all_windows_scorable"] is False
    bad = verdict["checks"]["unscorable_windows"]["probe"]
    for label in ("candidate BTC/USDT 1h", "candidate x2 slippage BTC/USDT 1h", "momentum_pro BTC/USDT 1h",
                  "vol_momentum BTC/USDT 1h", "trix_momentum BTC/USDT 1h", "incumbent median BTC/USDT 1h"):
        assert label in bad, bad
    funding = out["legs"]["candidate"]["BTC/USDT 1h"]["funding"]
    assert funding["unpriced_held_hours"] > 0 and funding["mode"] == "charge"
    assert 0 <= funding["priced_hours"] < funding["expected_hours"]
    assert funding["coverage_complete"] is False


def _complete_funding_gives_scorable_bidirectional_legs_with_next_open_fills(tmp_path):
    out = _cache_case(tmp_path)
    verdict = out["verdict"]
    assert verdict["gates"]["all_windows_scorable"] is True
    for arm, legs in out["legs"].items():
        leg = legs["BTC/USDT 1h"]
        assert leg["funding"]["mode"] == "charge" and leg["funding_incomplete"] is False
        assert leg["warmup"]["bars"] == 300
        assert leg["next_bar_fills"]["mismatches"] == 0
    assert verdict["checks"]["long_positions"]["candidate"] > 0
    assert verdict["checks"]["short_positions"]["candidate"] > 0


FUNDING_CASES = [
    ("missing_held_hour_funding_is_inconclusive",
     _missing_held_hour_funding_makes_every_required_arm_unscorable_and_the_verdict_inconclusive),
    ("complete_funding_is_scorable_with_next_open_fills",
     _complete_funding_gives_scorable_bidirectional_legs_with_next_open_fills),
]


def test_study_funding_coverage_decides_whether_every_leg_is_scorable(tmp_path, subtests):
    for label, run in FUNDING_CASES:
        with subtests.test(case=label):
            case_dir = tmp_path / label
            case_dir.mkdir()
            run(case_dir)
