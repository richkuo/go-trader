import json
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
    FRAME_DECISION_TIME_ATTR,
    TIMESTAMP_KIND_OPEN,
    bind_fixed_interval_params,
    declare_frame_cadence,
)
from parity_diff import ParityConfig
from registry_loader import load_registry

NAME = "camarilla_pivot_reversal"
DAY = 86_400_000
H = 3_600_000
SUPPORTED = {"15m": 900_000, "30m": 1_800_000, "1h": 3_600_000, "2h": 7_200_000, "4h": 14_400_000,
             "8h": 28_800_000, "12h": 43_200_000}
CLOSE = [{"name": "tiered_tp_pct", "params": {"tp_tiers": [{"profit_pct": 0.03, "close_fraction": 1.0}]}}]
def _reg():
    return load_registry("futures")


def _apply(df, interval=H):
    return _reg().apply_strategy(NAME, df, {"expected_interval_ms": interval})


def _bar(o, c, h=None, lo=None):
    return (float(o), float(max(o, c) + 0.1 if h is None else h), float(min(o, c) - 0.1 if lo is None else lo),
            float(c))


def _prior_day(per, high=110.0, low=90.0, close=100.0):
    bars = [_bar(100.0, 100.0) for _ in range(per)]
    bars[0] = (100.0, high, 99.9, 100.0)
    bars[-1] = (100.0, max(100.0, close) + 0.1, low, close)
    return bars


def _session(closes, start=100.0):
    out, prev = [], start
    for c in closes:
        out.append(_bar(prev, c))
        prev = c
    return out


def _frame(bars, tf="1h", start="2026-03-02", kind=TIMESTAMP_KIND_OPEN):
    step = SUPPORTED[tf]
    idx = pd.date_range(start, periods=len(bars), freq=pd.Timedelta(milliseconds=step))
    o, h, lo, c = (list(x) for x in zip(*bars))
    df = pd.DataFrame({"open": o, "high": h, "low": lo, "close": c, "volume": 1000.0}, index=idx)
    df.index.name = "datetime"
    return declare_frame_cadence(df, tf, kind)


def _day_frame(tf, current, prior=None):
    per = DAY // SUPPORTED[tf]
    bars = _prior_day(per) if prior is None else prior
    current = current + [100.0] * (per - len(current))
    return _frame(bars + _session(current), tf)


def _walk(n, tf, seed, start="2026-03-01", sigma=None):
    rng = np.random.default_rng(seed)
    step = SUPPORTED[tf]
    sig = sigma if sigma is not None else 0.006 * (step / H) ** 0.5
    close = 100.0 * np.exp(np.cumsum(rng.normal(0.0, sig, n)))
    open_ = np.concatenate([[100.0], close[:-1]])
    wick = np.abs(rng.normal(0.0, sig / 2, n))
    df = pd.DataFrame({"open": open_, "high": np.maximum(open_, close) * (1 + wick),
                       "low": np.minimum(open_, close) * (1 - wick), "close": close,
                       "volume": rng.uniform(100.0, 1000.0, n)},
                      index=pd.date_range(start, periods=n, freq=pd.Timedelta(milliseconds=step)))
    df.index.name = "datetime"
    return declare_frame_cadence(df, tf, TIMESTAMP_KIND_OPEN)


def _reference(df, interval):
    per = DAY // interval
    ms = np.asarray([int(t.value // 1_000_000) for t in df.index], dtype=np.int64)
    days = ms // DAY
    rows = {int(t): i for i, t in enumerate(ms)}
    out = []
    for i in range(len(df)):
        expect = {"signal": 0, "valid": False}
        if i >= 1 and ms[i] - ms[i - 1] == interval and days[i] == days[i - 1]:
            start = (days[i] - 1) * DAY
            idx = [rows.get(start + k * interval) for k in range(per)]
            if all(j is not None for j in idx):
                prior = df.iloc[idx]
                hi, lo, cl = float(prior["high"].max()), float(prior["low"].min()), float(prior["close"].iloc[-1])
                rng = hi - lo
                if rng > 0:
                    s3, r3 = cl - 1.1 * rng / 4, cl + 1.1 * rng / 4
                    s4, r4 = cl - 1.1 * rng / 2, cl + 1.1 * rng / 2
                    pc, c = float(df["close"].iloc[i - 1]), float(df["close"].iloc[i])
                    pl, low = float(df["low"].iloc[i - 1]), float(df["low"].iloc[i])
                    ph, high = float(df["high"].iloc[i - 1]), float(df["high"].iloc[i])
                    sig = 0
                    if pc <= s3 < c < r3 and pl > s4 and low > s4:
                        sig = 1
                    elif pc >= r3 > c > s3 and ph < r4 and high < r4:
                        sig = -1
                    expect = {"signal": sig, "valid": True, "s3": s3, "r3": r3, "s4": s4, "r4": r4, "H": hi,
                              "L": lo, "C": cl}
        out.append(expect)
    return out


def _runtime(df, cutoff, boundary):
    out = df.copy()
    out.attrs = dict(df.attrs)
    out.attrs[FRAME_DECISION_TIME_ATTR] = {} if cutoff is None else {"cutoff_ms": cutoff, "boundary_ms": boundary}
    return out


def test_research_binding_refuses_a_cadence_or_timeframe_mismatch_before_loading_candles(monkeypatch):
    import data_fetcher

    def refuse(*_a, **_k):
        raise AssertionError("candles were loaded before the parameters were validated")

    monkeypatch.setattr(data_fetcher, "load_cached_data", refuse)
    reg = _reg()
    window = ("2026-01-01", "2026-02-01")
    with pytest.raises(ValueError, match="does not match timeframe"):
        eval_windows.run_leg(reg, NAME, {"expected_interval_ms": H}, "BTC/USDT", "4h", window)
    with pytest.raises(ValueError, match="expected_interval_ms >= 900000"):
        eval_windows.run_leg(reg, NAME, None, "BTC/USDT", "5m", window)
    with pytest.raises(ValueError, match="not a supported interval"):
        eval_windows.run_leg(reg, NAME, None, "BTC/USDT", "6h", window)
    with pytest.raises(ValueError, match="fixed-duration timeframe"):
        eval_windows.run_leg(reg, NAME, None, "BTC/USDT", "1w", window)
    assert bind_fixed_interval_params({"expected_interval_ms": H}, None, "4h") == {"expected_interval_ms": 4 * H}


def _cfg(**kw):
    base = dict(strategy_name=NAME, params={"expected_interval_ms": H}, registry="futures", platform="hyperliquid",
                symbol="BTC", timeframe="1h", close_refs=json.loads(json.dumps(CLOSE)), direction="both",
                stop_kwargs={"stop_loss_atr_mult": 1.0}, comparison_mode="strict")
    base.update(kw)
    return ParityConfig(**base)
