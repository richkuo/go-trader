import contextlib
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


def _ref_fisher(mid, t, lookback):
    x = 0.0
    f = 0.0
    for k in range(t - HORIZON + 1, t + 1):
        window = mid[k - lookback + 1:k + 1]
        lo, hi = min(window), max(window)
        n = 2 * (mid[k] - lo) / (hi - lo) - 1
        x = 0.33 * n + 0.67 * x
        x = min(max(x, -1 + 1e-6), 1 - 1e-6)
        f = 0.5 * math.log((1 + x) / (1 - x)) + 0.5 * f
    return f


def _ref_signal(a, b, c, extreme):
    if a > b and b <= c and b < -extreme:
        return 1
    if a < b and b >= c and b > extreme:
        return -1
    return 0


def test_values_match_an_independent_reference_and_the_truth_table():
    df = _walk(420, 7)
    mid = ((df["high"] + df["low"]) / 2.0).tolist()
    for params in [{"lookback": 9, "extreme": 0.5}, {"lookback": 2, "extreme": 0.75}, {"lookback": 32, "extreme": 1.0}]:
        out = _apply(df, params)
        need = params["lookback"] + HORIZON + 1
        assert (out["fisher_valid"].iloc[:need - 1] == 0.0).all()
        assert (out["fisher_reason"].iloc[:need - 1] == "insufficient_history").all()
        assert out["fisher_valid"].iloc[need - 1] == 1.0
        seen = set()
        for t in range(need - 1, len(df)):
            a = _ref_fisher(mid, t, params["lookback"])
            b = _ref_fisher(mid, t - 1, params["lookback"])
            c = _ref_fisher(mid, t - 2, params["lookback"])
            row = out.iloc[t]
            assert (row["fisher_value"], row["fisher_trigger"], row["fisher_prev_trigger"]) == (a, b, c)
            sig = _ref_signal(a, b, c, params["extreme"])
            assert int(row["signal"]) == sig
            assert row["fisher_reason_code"] == {1: 1.0, -1: -1.0, 0: 0.0}[sig]
            seen.add(sig)
        assert seen == {-1, 0, 1}


def _check_equal_previous_comparison_qualifies_and_equal_current_does_not():
    falling = list(np.linspace(300.0, 100.0, 150))
    df = _from_mids(falling + [falling[-1] + 30.0])
    out = _apply(df, {"lookback": 9, "extreme": 0.5})
    last, prev = out.iloc[-1], out.iloc[-2]
    assert last["fisher_trigger"] == last["fisher_prev_trigger"] < -0.5
    assert last["fisher_value"] > last["fisher_trigger"]
    assert int(last["signal"]) == 1 and last["fisher_reason"] == "entry_long"
    assert prev["fisher_value"] == prev["fisher_trigger"] == prev["fisher_prev_trigger"]
    assert int(prev["signal"]) == 0 and prev["fisher_reason"] == "no_cross"

    rising = list(np.linspace(100.0, 300.0, 150))
    out = _apply(_from_mids(rising + [rising[-1] - 30.0]), {"lookback": 9, "extreme": 0.5})
    last = out.iloc[-1]
    assert last["fisher_trigger"] == last["fisher_prev_trigger"] > 0.5
    assert int(last["signal"]) == -1 and last["fisher_reason"] == "entry_short"


def _check_trigger_exactly_at_the_extreme_is_not_an_entry():
    df = _walk(600, 11)
    out = _apply(df, {"lookback": 9, "extreme": 0.5})
    for side in (1, -1):
        with _case(side):
            _trigger_exactly_at_the_extreme_is_not_an_entry(df, out, side)


def _trigger_exactly_at_the_extreme_is_not_an_entry(df, out, side):
    rows = [i for i in np.flatnonzero(out["signal"].to_numpy() == side)
            if 0.5 < abs(out["fisher_trigger"].iloc[i]) < 3.0]
    assert rows
    i = int(rows[0])
    edge = abs(float(out["fisher_trigger"].iloc[i]))
    at = _apply(df, {"lookback": 9, "extreme": edge})
    inside = _apply(df, {"lookback": 9, "extreme": float(np.nextafter(edge, 0.0))})
    assert int(at["signal"].iloc[i]) == 0 and at["fisher_reason"].iloc[i] == "no_cross"
    assert int(inside["signal"].iloc[i]) == side


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


def _check_invalid_entry_data_is_a_reasoned_numeric_hold_that_preserves_the_frame():
    for i, (mutate, reason) in enumerate(INVALID_ENTRY_DATA):
        with _case(f"{i}-{reason}"):
            _holds_at_end(mutate(_walk(160, 3)), P, reason)


def test_timestamp_column_is_accepted_without_a_datetime_index():
    df = _walk(160, 3)
    plain = df.reset_index(drop=True)
    plain["timestamp"] = (df.index.asi8 // 1_000_000).astype("int64")
    a = _apply(df, P)
    b = _apply(plain, P)
    np.testing.assert_array_equal(a["fisher_value"].to_numpy(), b["fisher_value"].to_numpy())
    np.testing.assert_array_equal(a["signal"].to_numpy(), b["signal"].to_numpy())


def _check_a_defect_holds_only_inside_its_span_and_recovery_matches_a_clean_frame():
    df = _walk(400, 5)
    bad = 200
    need = P["lookback"] + HORIZON + 1
    corrupt = df.copy()
    corrupt.iloc[bad, corrupt.columns.get_loc("volume")] = np.nan
    out = _apply(corrupt, P)
    clean = _apply(df, P)
    assert (out["fisher_reason"].iloc[bad:bad + need] == "nonfinite_input").all()
    assert (out["fisher_valid"].iloc[bad:bad + need] == 0.0).all()
    assert (out["signal"].iloc[bad:bad + need] == 0).all()
    pd.testing.assert_frame_equal(out.iloc[:bad], clean.iloc[:bad])
    pd.testing.assert_frame_equal(out.iloc[bad + need:], clean.iloc[bad + need:])
    gapped = df.drop(df.index[bad])
    out = _apply(gapped, P)
    assert (out["fisher_reason"].iloc[bad:bad + need - 1] == "irregular_cadence").all()
    assert out["fisher_valid"].iloc[bad + need - 1] == 1.0


def test_integral_float_lookback_is_accepted():
    df = _walk(120, 1)
    pd.testing.assert_frame_equal(_apply(df, {"lookback": 9.0, "extreme": 1.5}), _apply(df, {"lookback": 9, "extreme": 1.5}))


def _frozen(key, bars):
    manifest = om.load_manifest(MANIFEST)
    candles = om.load_candles(om.dataset_by_key(manifest, key))
    return candles[INPUT_COLUMNS].iloc[-bars:]


def _grid_and_endpoints():
    specs = [(k, list(v)) for k, v in DEFAULT_PARAM_RANGES[NAME].items()]
    return [p for _, p in expand_sweep({"lookback": 9, "extreme": 1.5}, specs)] + DOMAIN_ENDPOINTS


DECISION_COLUMNS = ["fisher_value", "fisher_trigger", "fisher_prev_trigger", "fisher_valid",
                    "fisher_reason_code", "signal"]


def test_full_series_prefix_exact_span_and_200_row_tail_agree_for_every_permitted_extreme():
    df = _frozen("ETH 4h", 500)
    for params in _grid_and_endpoints():
        need = params["lookback"] + HORIZON + 1
        assert need <= 97
        full = _apply(df, params)
        valid_ends = [int(i) + 1 for i in np.flatnonzero(full["fisher_valid"].to_numpy() == 1.0) if i >= 200]
        ends = sorted({valid_ends[0], valid_ends[len(valid_ends) // 2], valid_ends[-1], 300, 500})
        for end in ends:
            frames = (df.iloc[:end], df.iloc[end - 200:end], df.iloc[end - need:end])
            for part in frames:
                got = _apply(part, params)[DECISION_COLUMNS].iloc[-1]
                pd.testing.assert_series_equal(got, full[DECISION_COLUMNS].iloc[end - 1], check_names=False)
            short = _apply(df.iloc[end - need + 1:end], params)
            assert short["fisher_reason"].iloc[-1] == "insufficient_history"


def _entry_frame(side):
    falling = list(np.linspace(300.0, 100.0, 150))
    rising = list(np.linspace(100.0, 300.0, 150))
    return _from_mids(falling + [falling[-1] + 30.0]) if side == "long" else _from_mids(rising + [rising[-1] - 30.0])


def _cfg(params, **kw):
    base = dict(strategy_name=NAME, params=params, registry="futures", platform="hyperliquid",
                symbol="BTC", timeframe="4h")
    base.update(kw)
    return ParityConfig(**base)


def test_entry_rule_boundaries_at_equal_triggers_and_the_extreme(subtests):
    for check in (
            _check_equal_previous_comparison_qualifies_and_equal_current_does_not,
            _check_trigger_exactly_at_the_extreme_is_not_an_entry,
    ):
        with subtests.test(case=check.__name__.removeprefix("_check_")):
            check()


def test_input_defects_hold_with_a_reason_inside_their_span(subtests):
    for check in (
            _check_invalid_entry_data_is_a_reasoned_numeric_hold_that_preserves_the_frame,
            _check_a_defect_holds_only_inside_its_span_and_recovery_matches_a_clean_frame,
    ):
        with subtests.test(case=check.__name__.removeprefix("_check_")):
            check()
