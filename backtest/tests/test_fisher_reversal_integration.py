import contextlib
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

import offline_manifest as om
from eval_windows import expand_sweep
from optimizer import DEFAULT_PARAM_RANGES
from parity_diff import ParityConfig
from registry_loader import load_registry

NAME = "fisher_reversal"
MANIFEST = os.path.join(_REPO, "backtest", "candidates", "fisher_reversal_1646_manifest.json")
SEED = os.path.join(_REPO, "backtest", "candidates", "fisher_reversal_1646", "candidate_seed.json")
CLOSE = [{"name": "tiered_tp_pct", "params": {}}]
INPUT_COLUMNS = ["open", "high", "low", "close", "volume"]
HORIZON = 64
DOMAIN_ENDPOINTS = [
    {"lookback": 2, "extreme": 0.5},
    {"lookback": 2, "extreme": 3.0},
    {"lookback": 32, "extreme": 0.5},
    {"lookback": 32, "extreme": 3.0},
]


def _reg():
    return load_registry("futures")


@contextlib.contextmanager
def _case(label):
    try:
        yield
    except AssertionError as exc:
        raise AssertionError(f"case {label}: {exc}") from exc
    except pytest.fail.Exception as exc:
        pytest.fail(f"case {label}: {exc}")
    except Exception as exc:
        exc.add_note(f"case {label}")
        raise


def _apply(df, params=None):
    return _reg().apply_strategy(NAME, df, params)


def _walk(n, seed, start=100.0, freq="4h"):
    rng = np.random.default_rng(seed)
    close = start * np.exp(np.cumsum(rng.normal(0.0, 0.02, n)))
    open_ = np.concatenate([[start], close[:-1]])
    high = np.maximum(open_, close) * (1.0 + rng.uniform(0.001, 0.01, n))
    low = np.minimum(open_, close) * (1.0 - rng.uniform(0.001, 0.01, n))
    volume = rng.uniform(100.0, 1000.0, n)
    df = pd.DataFrame({"open": open_, "high": high, "low": low, "close": close, "volume": volume})
    df.index = pd.date_range("2026-01-01", periods=n, freq=freq)
    return df


def _from_mids(mids):
    mids = np.asarray(mids, dtype=float)
    df = pd.DataFrame({"open": mids, "high": mids + 0.5, "low": mids - 0.5, "close": mids,
                       "volume": np.full(len(mids), 10.0)})
    df.index = pd.date_range("2026-01-01", periods=len(mids), freq="4h")
    return df


def _ref_signal(a, b, c, extreme):
    if a > b and b <= c and b < -extreme:
        return 1
    if a < b and b >= c and b > extreme:
        return -1
    return 0


def _holds_at_end(df, params, reason):
    before = df.copy()
    out = _apply(df, params)
    pd.testing.assert_frame_equal(df, before)
    for col in df.columns:
        pd.testing.assert_series_equal(out[col], before[col])
    assert int(out["signal"].iloc[-1]) == 0
    assert out["fisher_valid"].iloc[-1] == 0.0
    assert out["fisher_reason"].iloc[-1] == reason, out["fisher_reason"].iloc[-1]
    numeric = out.drop(columns=["fisher_reason"]).filter(like="fisher_")
    assert np.isfinite(numeric.to_numpy(dtype=float)).all()
    return out


P = {"lookback": 5, "extreme": 0.5}


def _shift_index(df, pos, delta):
    idx = list(df.index)
    idx[pos] = idx[pos] + delta
    return df.set_axis(pd.DatetimeIndex(idx))


INVALID_ENTRY_DATA = [
    (lambda df: df.drop(columns=["volume"]), "missing_columns"),
    (lambda df: df.drop(columns=["open", "high"]), "missing_columns"),
    (lambda df: df.reset_index(drop=True), "missing_timestamps"),
    (lambda df: df.set_axis(pd.DatetimeIndex(list(df.index[:-1]) + [pd.NaT])), "missing_timestamps"),
    (lambda df: df.assign(high=np.r_[df["high"].to_numpy()[:-1], np.nan]), "nonfinite_input"),
    (lambda df: df.assign(volume=np.r_[df["volume"].to_numpy()[:-1], np.inf]), "nonfinite_input"),
    (lambda df: df.assign(low=np.r_[df["low"].to_numpy()[:-1], 0.0]), "nonpositive_price"),
    (lambda df: df.assign(open=np.r_[df["open"].to_numpy()[:-1], df["high"].iloc[-1] * 2]), "invalid_candle_geometry"),
    (lambda df: df.assign(close=np.r_[df["close"].to_numpy()[:-1], df["low"].iloc[-1] / 2]), "invalid_candle_geometry"),
    (lambda df: df.assign(high=np.r_[df["high"].to_numpy()[:-1], df["low"].iloc[-1] / 2]), "invalid_candle_geometry"),
    (lambda df: df.assign(volume=np.r_[df["volume"].to_numpy()[:-1], -1.0]), "negative_volume"),
    (lambda df: df.set_axis(pd.DatetimeIndex(list(df.index[:-1]) + [df.index[-2]])), "timestamp_order"),
    (lambda df: df.set_axis(pd.DatetimeIndex(list(df.index[:-1]) + [df.index[-3]])), "timestamp_order"),
    (lambda df: _shift_index(df, len(df) - 1, pd.Timedelta(hours=4)), "irregular_cadence"),
    (lambda df: _shift_index(df, len(df) - 30, pd.Timedelta(hours=1)), "irregular_cadence"),
    (lambda df: df.assign(open=100.0, high=101.0, low=99.0, close=100.0), "flat_range"),
    (lambda df: df.iloc[-(P["lookback"] + HORIZON):], "insufficient_history"),
]


def _frozen(key, bars):
    manifest = om.load_manifest(MANIFEST)
    candles = om.load_candles(om.dataset_by_key(manifest, key))
    return candles[INPUT_COLUMNS].iloc[-bars:]


def _grid_and_endpoints():
    specs = [(k, list(v)) for k, v in DEFAULT_PARAM_RANGES[NAME].items()]
    return [p for _, p in expand_sweep({"lookback": 9, "extreme": 1.5}, specs)] + DOMAIN_ENDPOINTS


DECISION_COLUMNS = ["fisher_value", "fisher_trigger", "fisher_prev_trigger", "fisher_valid",
                    "fisher_reason_code", "signal"]


def _entry_frame(side):
    falling = list(np.linspace(300.0, 100.0, 150))
    rising = list(np.linspace(100.0, 300.0, 150))
    return _from_mids(falling + [falling[-1] + 30.0]) if side == "long" else _from_mids(rising + [rising[-1] - 30.0])


def _cfg(params, **kw):
    base = dict(strategy_name=NAME, params=params, registry="futures", platform="hyperliquid",
                symbol="BTC", timeframe="4h")
    base.update(kw)
    return ParityConfig(**base)
