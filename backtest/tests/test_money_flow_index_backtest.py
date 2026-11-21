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
from atr import ensure_atr_indicator
from backtester import Backtester
from eval_windows import expand_sweep
from optimizer import DEFAULT_PARAM_RANGES
from parity_diff import ParityConfig
from registry_loader import load_registry

NAME = "money_flow_index_reversal"
CANDIDATE_DIR = os.path.join(_REPO, "backtest", "candidates", "money_flow_index_reversal_1658")
MANIFEST = os.path.join(CANDIDATE_DIR, "study_manifest.json")
CLOSE = [{"name": "tiered_tp_atr", "params": {"tp_tiers": [{"atr_multiple": 2.0, "close_fraction": 1.0}]}}]
INPUT_COLUMNS = ["open", "high", "low", "close", "volume"]


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


def _apply(df, params):
    return _reg().apply_strategy(NAME, df, params)


def _flat_frame(rows):
    df = pd.DataFrame(rows, columns=["close", "volume"], dtype=float)
    df["open"] = df["close"]
    df["high"] = df["close"]
    df["low"] = df["close"]
    df = df[INPUT_COLUMNS]
    df.index = pd.date_range("2026-01-01", periods=len(df), freq="4h")
    return df


def _walk(n, seed, start=100.0):
    rng = np.random.default_rng(seed)
    close = start * np.exp(np.cumsum(rng.normal(0.0, 0.02, n)))
    open_ = np.concatenate([[start], close[:-1]])
    high = np.maximum(open_, close) * (1.0 + rng.uniform(0.001, 0.01, n))
    low = np.minimum(open_, close) * (1.0 - rng.uniform(0.001, 0.01, n))
    volume = rng.uniform(100.0, 1000.0, n)
    df = pd.DataFrame({"open": open_, "high": high, "low": low, "close": close, "volume": volume})
    df.index = pd.date_range("2026-01-01", periods=n, freq="4h")
    return df


def _holds(df, params, reason):
    before = df.copy()
    out = _apply(df, params)
    pd.testing.assert_frame_equal(df, before)
    for col in df.columns:
        pd.testing.assert_series_equal(out[col], before[col])
    assert (out["signal"] == 0).all()
    assert not out["mfi_valid"].any()
    assert out["mfi_reason"].iloc[-1].startswith(reason), out["mfi_reason"].iloc[-1]
    return out


P2 = {"lookback": 2, "oversold": 20.0, "overbought": 80.0}


INVALID_ENTRY_DATA = [
    (lambda df: df.drop(columns=["volume"]), "missing_columns:volume"),
    (lambda df: df.drop(columns=["high", "volume"]), "missing_columns:high,volume"),
    (lambda df: df.assign(volume=np.nan), "nonfinite_input"),
    (lambda df: df.assign(volume=np.inf), "nonfinite_input"),
    (lambda df: df.assign(high=np.nan), "nonfinite_input"),
    (lambda df: df.assign(volume=-1.0), "negative_volume"),
    (lambda df: df.assign(low=0.0), "nonpositive_price"),
    (lambda df: df.assign(high=df["low"] * 0.5), "inconsistent_hlc"),
    (lambda df: df.assign(volume=0.0), "zero_total_flow"),
    (lambda df: df.assign(open=50.0, high=50.0, low=50.0, close=50.0), "zero_total_flow"),
    (lambda df: df.assign(volume=1e308), "arithmetic_overflow"),
    (lambda df: df.set_axis(df.index[::-1]), "timestamp_order"),
    (lambda df: df.iloc[:3], "insufficient_history"),
]


def _check_invalid_entry_data_is_a_reasoned_hold_that_preserves_the_frame():
    for i, (mutate, reason) in enumerate(INVALID_ENTRY_DATA):
        with _case(f"{i}-{reason}"):
            df = mutate(_flat_frame([(10, 1), (11, 1), (12, 1), (11, 1), (10, 1), (11, 1)]))
            _holds(df, P2, reason)


def _check_window_total_overflow_holds_while_each_flow_is_finite():
    df = _flat_frame([(10, 1), (11, 1e307), (12, 1e307), (13, 1e307)])
    out = _apply(df, P2)
    assert np.isfinite(out["mfi_raw_flow"].iloc[1:]).all()
    assert out["mfi_reason"].iloc[-1] == "arithmetic_overflow"
    assert (out["signal"] == 0).all()


def _check_a_bad_bar_invalidates_its_dependent_windows_and_recovery_needs_full_history():
    df = _walk(40, 3)
    bad = 20
    df.iloc[bad, df.columns.get_loc("volume")] = np.nan
    out = _apply(df, {"lookback": 5, "oversold": 30.0, "overbought": 70.0})
    reasons = out["mfi_reason"]
    assert reasons.iloc[bad] == "nonfinite_input"
    span = range(bad, bad + 5 + 2)
    assert all(reasons.iloc[i] == "nonfinite_input" for i in span)
    assert not out["mfi_valid"].iloc[bad:bad + 7].any()
    assert out["mfi_valid"].iloc[bad + 7]
    clean = _apply(df.iloc[bad + 1:], {"lookback": 5, "oversold": 30.0, "overbought": 70.0})
    np.testing.assert_array_equal(out["mfi_value"].iloc[bad + 7:].to_numpy(),
                                  clean["mfi_value"].iloc[6:].to_numpy())


def _check_supported_bounds_decide_inside_the_200_bar_check_window():
    df = _walk(260, 11)
    for params in ({"lookback": 2, "oversold": 0.0, "overbought": 100.0},
                   {"lookback": 198, "oversold": 20.0, "overbought": 80.0}):
        with _case(params):
            _supported_bounds_decide_inside_the_200_bar_check_window(df, params)


def _supported_bounds_decide_inside_the_200_bar_check_window(df, params):
    full = _apply(df, params)
    tail = _apply(df.iloc[-200:], params)
    assert bool(tail["mfi_valid"].iloc[-1])
    assert tail["mfi_value"].iloc[-1] == full["mfi_value"].iloc[-1]
    assert tail["signal"].iloc[-1] == full["signal"].iloc[-1]
    short = _apply(df.iloc[-(params["lookback"] + 1):], params)
    assert short["mfi_reason"].iloc[-1] == "insufficient_history"


def _bt(**kw):
    base = dict(initial_capital=1000.0, platform="hyperliquid",
                open_strategy={"name": NAME, "params": kw.pop("params")},
                close_strategies=CLOSE, stop_loss_atr_mult=1.0, direction="both")
    base.update(kw)
    return Backtester(**base)


def test_simulator_fills_next_bar_open_with_explicit_exit_and_no_reversal_or_scale_in():
    params = {"lookback": 7, "oversold": 30.0, "overbought": 70.0}
    df = _walk(600, 5)
    sig = ensure_atr_indicator(_apply(df, params))
    res = _bt(params=params).run(sig, strategy_name=NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    trades = res["trades"]
    assert {t["side"] for t in trades} == {"long", "short"}
    signal_at = sig["signal"]
    last_exit = None
    for t in trades:
        entry = pd.Timestamp(t["entry_date"])
        pos = sig.index.get_loc(entry)
        assert signal_at.iloc[pos - 1] == (1 if t["side"] == "long" else -1)
        assert t["entry_price"] == pytest.approx(df["open"].iloc[pos], rel=0.01)
        assert t["exit_reason"].split(":")[0] in ("sl", "tiered_tp_atr", "end_of_data"), t["exit_reason"]
        if last_exit is not None:
            assert entry >= last_exit
        last_exit = pd.Timestamp(t["exit_date"])
    assert len({t["entry_date"] for t in trades}) == len(trades)


def test_simulator_closes_and_stops_when_entry_inputs_fail():
    params = {"lookback": 7, "oversold": 30.0, "overbought": 70.0}
    df = _walk(400, 5)
    sig = ensure_atr_indicator(_apply(df, params))
    first = next(int(i) for i in np.flatnonzero(sig["signal"].to_numpy() != 0)
                 if i > 30 and np.isfinite(sig["atr"].iloc[i]))
    corrupt = df.copy()
    corrupt.iloc[first + 1:, corrupt.columns.get_loc("volume")] = np.nan
    bad = ensure_atr_indicator(_apply(corrupt, params))
    assert (bad["signal"].iloc[:first + 1] == sig["signal"].iloc[:first + 1]).all()
    assert (bad["signal"].iloc[first + 1:] == 0).all()
    assert (bad["mfi_reason"].iloc[first + 1:] == "nonfinite_input").all()
    res = _bt(params=params).run(bad, strategy_name=NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    entries = [pd.Timestamp(t["entry_date"]) for t in res["trades"]]
    assert max(entries) == bad.index[first + 1]
    t = res["trades"][-1]
    assert pd.Timestamp(t["entry_date"]) == bad.index[first + 1]
    assert t["exit_reason"].split(":")[0] in ("sl", "tiered_tp_atr")


def _frozen(key, bars):
    manifest = om.load_manifest(MANIFEST)
    candles = om.load_candles(om.dataset_by_key(manifest, key))
    return candles[INPUT_COLUMNS].iloc[-bars:]


def _check_full_series_prefix_and_bounded_values_are_bit_identical_across_the_grid():
    df = _frozen("ETH 4h", 460)
    specs = [(k, list(v)) for k, v in DEFAULT_PARAM_RANGES[NAME].items()]
    for _, params in expand_sweep({"lookback": 14, "oversold": 20.0, "overbought": 80.0}, specs):
        full = _apply(df, params)
        for end in (230, 345, 460):
            tail = _apply(df.iloc[end - 200:end], params)
            prefix = _apply(df.iloc[:end], params)
            for frame in (tail, prefix):
                assert frame["mfi_value"].iloc[-1] == full["mfi_value"].iloc[end - 1]
                assert frame["mfi_positive_total"].iloc[-1] == full["mfi_positive_total"].iloc[end - 1]
                assert frame["signal"].iloc[-1] == full["signal"].iloc[end - 1]


def _cfg(params, **kw):
    base = dict(strategy_name=NAME, params=params, registry="futures", platform="hyperliquid",
                symbol="BTC", timeframe="4h")
    base.update(kw)
    return ParityConfig(**base)


def test_input_defects_hold_with_a_reason_inside_their_dependent_windows(subtests):
    for check in (
            _check_invalid_entry_data_is_a_reasoned_hold_that_preserves_the_frame,
            _check_window_total_overflow_holds_while_each_flow_is_finite,
            _check_a_bad_bar_invalidates_its_dependent_windows_and_recovery_needs_full_history,
    ):
        with subtests.test(case=check.__name__.removeprefix("_check_")):
            check()


def test_decisions_depend_only_on_the_bounded_window_across_the_grid(subtests):
    for check in (
            _check_supported_bounds_decide_inside_the_200_bar_check_window,
            _check_full_series_prefix_and_bounded_values_are_bit_identical_across_the_grid,
    ):
        with subtests.test(case=check.__name__.removeprefix("_check_")):
            check()
