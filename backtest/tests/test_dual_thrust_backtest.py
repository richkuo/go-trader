import json
import math
import os
import subprocess
import sys

import numpy as np
import pandas as pd
import pytest

_REPO = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
if os.path.join(_REPO, "shared_tools") not in sys.path:
    sys.path.insert(0, os.path.join(_REPO, "shared_tools"))

from atr import ensure_atr_indicator
from backtester import Backtester
from close_registry_loader import evaluate as close_evaluate
from closed_bar import declare_frame_cadence
from parity_diff import _ENGINE_SLIPPAGE_PCT, ParityConfig, compute_parity_frame, summarize
from registry_loader import load_registry
from strategy_composition import evaluate_open_close, finalize_decision

NAME = "dual_thrust"
H = 3_600_000
DAY = 86_400_000
T0 = 1_767_225_600_000
P = {"lookback_sessions": 2, "k_long": 0.5, "k_short": 0.5, "session_start_hour": 0, "expected_interval_ms": H}
TP = {"name": "tiered_tp_pct", "params": {"tp_tiers": [{"profit_pct": 0.03, "close_fraction": 1.0}]}}
COLS = ["signal", "dt_reason", "dt_valid", "dt_range", "dt_anchor", "dt_upper", "dt_lower", "dt_hh", "dt_lc",
        "dt_hc", "dt_ll", "dt_long_consumed", "dt_short_consumed", "dt_eval_ts_ms", "dt_session_open_ms"]
CHECK_HL = os.path.join(_REPO, "shared_scripts", "check_hyperliquid.py")
CREDENTIAL_PREFIXES = ("HYPERLIQUID", "OKX", "ROBINHOOD", "TOPSTEP", "PROJECTX")


def _reg():
    return load_registry("futures")


def _apply(df, **kw):
    return _reg().apply_strategy(NAME, df, dict(P, **kw))


def _frame(bars, freq="1h", start="2026-01-01", kind="open"):
    df = pd.DataFrame(bars, columns=["open", "high", "low", "close"], dtype=float)
    df["volume"] = 1.0
    df.index = pd.date_range(start, periods=len(df), freq={"30m": "30min"}.get(freq, freq))
    return declare_frame_cadence(df, freq, kind)


def _bar(o, c, pad=0.1):
    return (o, max(o, c) + pad, min(o, c) - pad, c)


def _base(per=24):
    day1 = [(100.0, 100.5, 99.5, 100.0)] * per
    day1[2] = (100.0, 105.0, 99.5, 100.0)
    day1[-1] = (100.0, 101.0, 99.9, 101.0)
    day2 = [(100.0, 100.5, 99.5, 100.0)] * per
    day2[1] = (100.0, 100.4, 96.0, 100.0)
    day2[-1] = (100.0, 100.2, 99.0, 99.0)
    return day1 + day2


def _session(closes, per=24, anchor=100.0, fill=100.0):
    rows, o = [], anchor
    for c in closes:
        rows.append(_bar(o, c))
        o = c
    while len(rows) < per:
        rows.append(_bar(o, fill))
        o = fill
    return rows


def _walk(n, seed=1642, freq="1h", start="2026-01-01", step=1.2):
    rng = np.random.RandomState(seed)
    c = np.round(100 + 4 * np.sin(np.arange(n) / 17.0) + np.cumsum(rng.randn(n) * step), 2)
    o = np.concatenate([[c[0]], c[:-1]])
    hi = np.maximum(o, c) + np.round(np.abs(rng.randn(n)) * 0.6, 2)
    lo = np.minimum(o, c) - np.round(np.abs(rng.randn(n)) * 0.6, 2)
    return _frame(list(zip(o, hi, lo, c)), freq=freq, start=start)


def _reference(df, n, kl, ks, hour, interval):
    per = DAY // interval
    ms = np.array([t.value // 1_000_000 for t in df.index])
    sess = (ms - hour * H) // DAY
    daily = {}
    for s in np.unique(sess):
        rows = df[sess == s]
        if len(rows) == per:
            daily[s] = (rows["high"].max(), rows["low"].min(), rows["close"].iloc[-1])
    sig = np.zeros(len(df), dtype=int)
    vals = {}
    fired = set()
    for i in range(len(df)):
        s = sess[i]
        prior = [s - j for j in range(1, n + 1)]
        first = int(np.flatnonzero(sess == s)[0])
        if not all(p in daily for p in prior) or (ms[first] - hour * H) % DAY:
            continue
        hh = max(daily[p][0] for p in prior)
        ll = min(daily[p][1] for p in prior)
        hc = max(daily[p][2] for p in prior)
        lc = min(daily[p][2] for p in prior)
        rng = max(hh - lc, hc - ll)
        anchor = df["open"].iloc[first]
        vals[i] = (hh, lc, hc, ll, rng, anchor)
        close = df["close"].iloc[i]
        if close > anchor + kl * rng and (s, 1) not in fired:
            sig[i] = 1
            fired.add((s, 1))
        elif close < anchor - ks * rng and (s, -1) not in fired:
            sig[i] = -1
            fired.add((s, -1))
    return sig, vals


def _same(a, b):
    for col in COLS:
        x, y = a[col], b[col]
        if isinstance(x, float) and isinstance(y, float) and math.isnan(x) and math.isnan(y):
            continue
        assert x == y, (col, x, y)


@pytest.mark.parametrize("freq,interval,n,kl,ks,hour,days", [
    ("1h", H, 3, 0.3, 0.4, 0, 20),
    ("1h", H, 2, 0.25, 0.25, 7, 20),
    ("1h", H, 7, 0.5, 0.25, 23, 40),
    ("4h", 4 * H, 4, 0.5, 0.5, 0, 50),
    ("4h", 4 * H, 2, 0.2, 0.3, 8, 50),
])
def test_registry_matches_an_independent_daily_session_calculation(freq, interval, n, kl, ks, hour, days):
    per = DAY // interval
    df = _walk(days * per, freq=freq, seed=hour + n)
    params = dict(lookback_sessions=n, k_long=kl, k_short=ks, session_start_hour=hour, expected_interval_ms=interval)
    out = _apply(df, **params)
    sig, vals = _reference(df, n, kl, ks, hour, interval)
    assert np.array_equal(out["signal"].to_numpy(), sig)
    assert (sig == 1).sum() >= 2 and (sig == -1).sum() >= 2
    valid = out["dt_valid"].to_numpy()
    assert sorted(np.flatnonzero(valid)) == sorted(vals)
    for i, (hh, lc, hc, ll, rng, anchor) in vals.items():
        row = out.iloc[i]
        assert (row["dt_hh"], row["dt_lc"], row["dt_hc"], row["dt_ll"]) == (hh, lc, hc, ll)
        assert row["dt_range"] == rng and row["dt_anchor"] == anchor
        assert row["dt_upper"] == anchor + kl * rng and row["dt_lower"] == anchor - ks * rng
    ms = np.array([t.value // 1_000_000 for t in df.index])
    first_session = int(np.flatnonzero((ms - hour * H) % DAY == 0)[0])
    assert not valid[:first_session + n * per].any() and valid[first_session + n * per]


def test_rising_session_fires_one_long_and_records_its_evaluated_inputs():
    df = _frame(_base() + _session([101.0, 102.0, 103.5, 104.0, 102.0, 104.5]))
    out = _apply(df)
    assert list(np.flatnonzero(out["signal"].to_numpy() == 1)) == [50]
    assert not (out["signal"] == -1).any()
    row = out.iloc[50]
    assert row["dt_reason"] == "entry_long" and row["dt_reason_code"] == 1 and bool(row["dt_valid"])
    assert (row["dt_hh"], row["dt_lc"], row["dt_hc"], row["dt_ll"]) == (105.0, 99.0, 101.0, 96.0)
    assert row["dt_range"] == 6.0 and row["dt_anchor"] == 100.0
    assert row["dt_upper"] == 103.0 and row["dt_lower"] == 97.0
    assert row["dt_bar_in_session"] == 2.0 and row["dt_session_open_ms"] == T0 + 2 * DAY
    assert row["dt_eval_ts_ms"] == T0 + 2 * DAY + 2 * H and row["dt_close"] == 103.5
    assert (row["dt_lookback_sessions"], row["dt_k_long"], row["dt_k_short"]) == (2.0, 0.5, 0.5)
    assert (row["dt_session_start_hour"], row["dt_interval_ms"]) == (0.0, float(H))
    assert list(out["dt_reason"].iloc[51:54]) == ["long_consumed", "inside_range", "long_consumed"]
    assert (out["dt_reason"].iloc[:48] == "insufficient_history").all()
    assert out["dt_valid"].iloc[48:].all()


def test_falling_session_fires_one_short():
    df = _frame(_base() + _session([99.0, 98.0, 96.5, 96.0, 98.0, 95.0]))
    out = _apply(df)
    assert list(np.flatnonzero(out["signal"].to_numpy() == -1)) == [50]
    assert not (out["signal"] == 1).any()
    assert out["dt_reason"].iloc[50] == "entry_short" and out["dt_reason_code"].iloc[50] == -1
    assert list(out["dt_reason"].iloc[53:55]) == ["short_consumed", "inside_range"]


def test_both_sides_fire_once_in_one_session_and_rearm_at_the_next_session():
    today = _session([103.5, 100.0, 96.5, 104.0, 96.0, 103.9, 96.9])
    tomorrow = _session([110.0, 80.0, 112.0])
    df = _frame(_base() + today + tomorrow)
    out = _apply(df)
    sig = out["signal"].to_numpy()
    assert list(np.flatnonzero(sig == 1)) == [48, 72]
    assert list(np.flatnonzero(sig == -1)) == [50, 73]
    assert list(out["dt_reason"].iloc[51:55]) == ["long_consumed", "short_consumed", "long_consumed",
                                                  "short_consumed"]
    assert out["dt_long_consumed"].iloc[71] == 1.0 and out["dt_long_consumed"].iloc[72] == 1.0
    assert out["dt_short_consumed"].iloc[72] == 0.0 and out["dt_short_consumed"].iloc[73] == 1.0
    assert out["dt_session_open_ms"].iloc[72] == T0 + 3 * DAY
    assert out["dt_anchor"].iloc[72] == 100.0 and out["dt_range"].iloc[72] != out["dt_range"].iloc[48]
    ref, _ = _reference(df, 2, 0.5, 0.5, 0, H)
    assert np.array_equal(sig, ref)


def test_threshold_equality_never_fires_and_a_strict_cross_does():
    out = _apply(_frame(_base() + _session([103.0, 97.0, 103.0, 103.01, 96.99])))
    assert list(out["dt_reason"].iloc[48:51]) == ["threshold_equal"] * 3
    assert (out["signal"].iloc[48:51] == 0).all()
    assert out["signal"].iloc[51] == 1 and out["signal"].iloc[52] == -1


def test_the_opening_candle_itself_can_fire():
    out = _apply(_frame(_base() + _session([103.5])))
    assert out["signal"].iloc[48] == 1 and out["dt_bar_in_session"].iloc[48] == 0.0


def test_constant_prices_hold_with_zero_range():
    out = _apply(_frame([(50.0, 50.0, 50.0, 50.0)] * 96))
    assert (out["dt_reason"].iloc[48:] == "zero_range").all()
    assert not out["dt_valid"].any() and (out["signal"] == 0).all()
    assert (out["dt_range"].iloc[48:] == 0.0).all()


def test_overflowing_thresholds_hold_as_nonfinite():
    bars = _base()
    bars[3] = (1e308, 1.7e308, 1e308, 1e308)
    out = _apply(_frame(bars + _session([101.0, 102.0]), freq="1h"), k_long=2.0)
    assert (out["dt_reason"].iloc[48:72] == "nonfinite_derived").all()
    assert (out["signal"] == 0).all()


def _days(n_days, per=24):
    bars = _base(per)
    while len(bars) < n_days * per:
        bars += _session([101.0, 99.0, 103.5, 96.5], per=per)
    return bars


def _mutate(kind, df, j):
    df = df.copy()
    idx = list(df.index)
    if kind == "missing_timestamp":
        idx[j] = pd.NaT
    elif kind == "duplicate_timestamp":
        idx[j] = idx[j - 1]
    elif kind == "descending_timestamp":
        idx[j] = idx[j - 1] - pd.Timedelta(minutes=60)
    elif kind == "grid_misaligned":
        idx[j] = idx[j] + pd.Timedelta(minutes=30)
    elif kind == "nonfinite_price":
        df.iloc[j, df.columns.get_loc("close")] = np.nan
    elif kind == "infinite_price":
        df.iloc[j, df.columns.get_loc("high")] = np.inf
    elif kind == "nonpositive_price":
        df.iloc[j, df.columns.get_loc("low")] = 0.0
    elif kind == "inconsistent_candle":
        df.iloc[j, df.columns.get_loc("high")] = df["close"].iloc[j] - 0.5
    df.index = pd.DatetimeIndex(idx)
    return df


IN_PLACE = {
    "missing_timestamp": "missing_timestamp",
    "duplicate_timestamp": "duplicate_timestamp",
    "descending_timestamp": "descending_timestamp",
    "grid_misaligned": "grid_misaligned",
    "nonfinite_price": "nonfinite_price",
    "infinite_price": "nonfinite_price",
    "nonpositive_price": "nonpositive_price",
    "inconsistent_candle": "inconsistent_candle",
}


@pytest.mark.parametrize("kind", sorted(IN_PLACE))
def test_each_defect_holds_its_session_and_the_next_n_sessions_then_recovers(kind):
    clean = _frame(_days(8))
    j = 3 * 24 + 10
    df = _mutate(kind, clean, j)
    out = _apply(df)
    ref = _apply(clean)
    assert out["dt_reason"].iloc[j] == IN_PLACE[kind]
    recover = (3 + 2 + 1) * 24
    blocked = out.iloc[j:recover]
    assert not blocked["dt_valid"].any() and (blocked["signal"] == 0).all()
    assert set(blocked["dt_reason"]) <= {IN_PLACE[kind], "cadence_gap", "descending_timestamp", "missing_timestamp",
                                         "duplicate_timestamp"}
    for i in list(range(48, j)) + list(range(recover, len(df))):
        _same(out.iloc[i], ref.iloc[i])
    assert (ref["signal"].iloc[j:recover] != 0).any()


@pytest.mark.parametrize("drop,first_bad,recover,reason", [
    ("session_open", 3 * 24, 6 * 24, "cadence_gap"),
    ("interior", 3 * 24 + 10, 6 * 24, "cadence_gap"),
    ("whole_day", 3 * 24, 6 * 24, "cadence_gap"),
])
def test_missing_candles_never_become_valid_sessions(drop, first_bad, recover, reason):
    clean = _frame(_days(8))
    if drop == "session_open":
        removed = [3 * 24]
    elif drop == "interior":
        removed = [3 * 24 + 10]
    else:
        removed = list(range(3 * 24, 4 * 24))
    df = clean.drop(clean.index[removed])
    out = _apply(df).reindex(clean.index)
    ref = _apply(clean)
    kept = out.dropna(subset=["dt_reason"])
    assert kept["dt_reason"].loc[clean.index[first_bad + (len(removed) if drop != "interior" else 1)]] == reason
    window = kept[(kept.index >= clean.index[first_bad]) & (kept.index < clean.index[recover])]
    assert not window["dt_valid"].astype(bool).any() and (window["signal"] == 0).all()
    for i in range(recover, len(clean)):
        _same(out.iloc[i], ref.iloc[i])
    for i in range(48, first_bad):
        _same(out.iloc[i], ref.iloc[i])


def test_an_incomplete_leading_session_is_never_a_prior_session():
    full = _frame(_days(6))
    df = full.iloc[5:]
    out = _apply(df)
    first_valid = out.index[out["dt_valid"].to_numpy()][0]
    assert first_valid == full.index[3 * 24]
    assert (out["dt_reason"].loc[:full.index[3 * 24 - 1]] == "insufficient_history").all()
    ref = _apply(full)
    for ts in out.index[out.index >= first_valid]:
        _same(out.loc[ts], ref.loc[ts])


def test_timezone_column_and_native_close_inputs_resolve_the_same_opening_times():
    df = _walk(24 * 12, seed=5)
    base = _apply(df)
    tz = declare_frame_cadence(df.tz_localize("UTC").tz_convert("America/New_York"), "1h", "open")
    col = df.reset_index(drop=True)
    col["timestamp"] = df.index.as_unit("ms").asi8
    col = declare_frame_cadence(col, "1h", "open")
    native_idx = df.index + pd.Timedelta(milliseconds=H - 1)
    native = df.copy()
    native.index = native_idx
    native = declare_frame_cadence(native, "1h", "native_close_inclusive")
    undeclared_native = df.copy()
    undeclared_native.index = native_idx.tz_localize("UTC")
    undeclared_native.attrs = {}
    undeclared_open = df.tz_localize("UTC")
    undeclared_open.attrs = {}
    for other in (tz, col, native, undeclared_native, undeclared_open):
        got = _apply(other)
        for c in ("signal", "dt_valid", "dt_eval_ts_ms", "dt_upper", "dt_lower", "dt_reason"):
            assert np.array_equal(base[c].to_numpy(), got[c].to_numpy(),
                                  equal_nan=base[c].dtype.kind == "f"), c
    assert (base["signal"] != 0).any()


def test_undeclared_or_contradictory_timing_holds_every_row():
    df = _walk(24 * 6)
    naive = df.copy()
    naive.attrs = {}
    assert (_apply(naive)["dt_reason"] == "undeclared_timing").all()
    four_hour = declare_frame_cadence(df.copy(), "4h", "open")
    assert (_apply(four_hour)["dt_reason"] == "timing_contradiction").all()
    weird = df.copy()
    weird.attrs["bar_timestamp_kind"] = "close"
    assert (_apply(weird)["dt_reason"] == "timing_contradiction").all()
    for held in (_apply(naive), _apply(four_hour), _apply(weird)):
        assert (held["signal"] == 0).all() and not held["dt_valid"].any()
    no_close = df.drop(columns=["close"])
    assert (_apply(no_close)["dt_reason"] == "missing_columns").all()
    no_ts = df.reset_index(drop=True)
    assert (_apply(no_ts)["dt_reason"] == "missing_timestamps").all()
    empty = _apply(df.iloc[:0])
    assert len(empty) == 0 and "dt_reason" in empty.columns


@pytest.mark.parametrize("bad", [
    {"lookback_sessions": 0}, {"lookback_sessions": 200}, {"lookback_sessions": 2.5},
    {"lookback_sessions": True}, {"lookback_sessions": "2"}, {"lookback_sessions": float("nan")},
    {"k_long": 0}, {"k_long": -0.5}, {"k_long": 2.01}, {"k_long": float("inf")}, {"k_long": True},
    {"k_long": "0.5"}, {"k_short": 0}, {"k_short": float("nan")}, {"k_short": None},
    {"session_start_hour": -1}, {"session_start_hour": 24}, {"session_start_hour": 1.5},
    {"session_start_hour": False},
    {"expected_interval_ms": 0}, {"expected_interval_ms": 25_200_000}, {"expected_interval_ms": 1.5},
    {"expected_interval_ms": True}, {"expected_interval_ms": None},
    {"expected_interval_ms": 4 * H, "session_start_hour": 1},
    {"expected_interval_ms": 1_800_000, "lookback_sessions": 4},
    {"lookback_sessions": 8},
    {"expected_interval_ms": 4 * H, "lookback_sessions": 33},
])
def test_invalid_parameters_are_configuration_errors(bad):
    df = _walk(24 * 3)
    with pytest.raises(ValueError):
        _apply(df, **bad)
    with pytest.raises(ValueError):
        _reg().validate_params(NAME, dict(P, **bad))


@pytest.mark.parametrize("good", [
    {"lookback_sessions": 7}, {"expected_interval_ms": 4 * H, "lookback_sessions": 32},
    {"expected_interval_ms": 4 * H, "session_start_hour": 8}, {"expected_interval_ms": 1_800_000,
                                                               "lookback_sessions": 3},
    {"expected_interval_ms": DAY, "lookback_sessions": 199}, {"session_start_hour": 23, "k_long": 2.0},
    {"lookback_sessions": 4.0, "k_short": 1},
])
def test_boundary_parameters_are_accepted(good):
    _reg().validate_params(NAME, dict(P, **good))


def test_every_optimizer_grid_point_fits_the_window_at_1h_and_4h_and_30m_points_are_excluded():
    from optimizer import DEFAULT_PARAM_RANGES, generate_param_grid
    grid = generate_param_grid(DEFAULT_PARAM_RANGES[NAME])
    assert len(grid) == 80
    for interval in (H, 4 * H):
        for p in grid:
            _reg().validate_params(NAME, dict(p, expected_interval_ms=interval))
    refused = []
    for p in grid:
        try:
            _reg().validate_params(NAME, dict(p, expected_interval_ms=1_800_000))
        except ValueError:
            refused.append(p["lookback_sessions"])
    assert sorted(set(refused)) == [4, 5, 7]


def test_walk_forward_scoring_skips_combinations_the_validator_refuses(capsys):
    from optimizer import walk_forward_optimize
    df = _walk(48 * 60, freq="30m", seed=11)
    result = walk_forward_optimize(df, NAME, {"lookback_sessions": [3, 5], "k_long": [0.5], "k_short": [0.5],
                                              "session_start_hour": [0]},
                                   n_splits=2, timeframe="30m", registry="futures", platform="hyperliquid",
                                   verbose=True, close_strategies=[TP], stop_loss_atr_mult=1.0, direction="both")
    out = capsys.readouterr().out
    assert "lookback_sessions': 5" in out and "exceeds the 200-bar check window" in out
    picked = [w["best_params"]["lookback_sessions"] for w in result["window_results"] if w.get("best_params")]
    assert picked and set(picked) == {3}


def _defective(n_days=10, per=24, freq="1h"):
    df = _walk(n_days * per, seed=17, freq=freq)
    df.iloc[3 * per + 4, df.columns.get_loc("close")] = np.nan
    idx = list(df.index)
    idx[5 * per + 2] = idx[5 * per + 1]
    idx[7 * per + 9] = pd.NaT
    df.index = pd.DatetimeIndex(idx)
    return df.drop(df.index[8 * per + 5])


@pytest.mark.parametrize("freq,params", [
    ("1h", {}),
    ("1h", {"lookback_sessions": 3, "session_start_hour": 5, "k_long": 0.25, "k_short": 0.25}),
    ("4h", {"expected_interval_ms": 4 * H, "lookback_sessions": 4, "session_start_hour": 4, "k_long": 0.25,
            "k_short": 0.25}),
])
def test_full_series_every_prefix_and_trailing_window_agree(freq, params):
    per = 24 if freq == "1h" else 6
    df = _defective(n_days=20 if freq == "1h" else 40, per=per, freq=freq)
    full = _apply(df, **params)
    assert (full["signal"] != 0).sum() >= 2
    for i in range(len(df)):
        prefix = _apply(df.iloc[: i + 1], **params).iloc[-1]
        trailing = _apply(df.iloc[max(0, i - 199): i + 1], **params).iloc[-1]
        _same(full.iloc[i], prefix)
        _same(full.iloc[i], trailing)


def test_later_rows_and_rows_outside_the_required_span_never_change_a_decision():
    df = _walk(24 * 10, seed=23)
    base = _apply(df)
    later = _frame([(1.0, 0.5, 2.0, 1.0)] * 30, start=str(df.index[-1] + pd.Timedelta(minutes=30)))
    extended = pd.concat([df, later])
    extended.attrs = dict(df.attrs)
    ext = _apply(extended)
    for i in range(len(df)):
        _same(base.iloc[i], ext.iloc[i])
    i = 8 * 24 + 5
    for k in (0, 3 * 24, 6 * 24 - 1):
        poisoned = df.copy()
        poisoned.iloc[k, poisoned.columns.get_loc("high")] = -5.0
        _same(base.iloc[i], _apply(poisoned).iloc[i])
    hit = df.copy()
    hit.iloc[6 * 24, hit.columns.get_loc("low")] = -5.0
    assert not bool(_apply(hit)["dt_valid"].iloc[i])


def _bt(**kw):
    base = dict(initial_capital=1000.0, platform="hyperliquid", open_strategy={"name": NAME, "params": P},
                close_strategies=[TP], stop_loss_atr_mult=1.0, direction="both")
    base.update(kw)
    return Backtester(**base)


def _run_bt(df, params=None, **kw):
    sig = ensure_atr_indicator(_apply(df, **(params or {})))
    return sig, _bt(**kw).run(sig, strategy_name=NAME, symbol="BTC/USDT", timeframe="1h", save=False)


def test_long_entry_fills_at_the_next_bar_open_and_the_explicit_close_owns_the_exit():
    df = _frame(_base() + _session([101.0, 102.0, 103.5, 104.2, 105.0, 106.0, 107.5]))
    sig, res = _run_bt(df)
    assert int(np.flatnonzero(sig["signal"].to_numpy() == 1)[0]) == 50
    t = res["trades"][0]
    assert t["side"] == "long" and t["entry_date"] == str(df.index[51])
    assert t["entry_price"] == pytest.approx(df["open"].iloc[51] * (1 + _ENGINE_SLIPPAGE_PCT))
    assert "tp" in t["exit_reason"]


def test_short_entry_fills_at_the_next_bar_open():
    df = _frame(_base() + _session([99.0, 98.0, 96.5, 95.5, 94.5, 93.5, 92.0]))
    sig, res = _run_bt(df)
    t = res["trades"][0]
    assert t["side"] == "short" and t["entry_date"] == str(df.index[51])
    assert t["entry_price"] == pytest.approx(df["open"].iloc[51] * (1 - _ENGINE_SLIPPAGE_PCT))


def test_opposite_and_later_signals_never_reverse_or_add_while_open():
    tomorrow = _session([110.0, 80.0])
    df = _frame(_base() + _session([103.5, 100.0, 96.5, 101.0]) + tomorrow)
    sig, res = _run_bt(df, stop_loss_atr_mult=None, stop_loss_pct=60.0,
                       close_strategies=[{"name": "tiered_tp_pct",
                                          "params": {"tp_tiers": [{"profit_pct": 0.5, "close_fraction": 1.0}]}}])
    s = sig["signal"].to_numpy()
    assert (s == 1).sum() == 2 and (s == -1).sum() == 2
    trades = res["trades"]
    assert len(trades) == 1 and trades[0]["side"] == "long"
    assert trades[0]["entry_date"] == str(df.index[49])


def test_a_consumed_event_never_re_enters_after_an_exit_in_the_same_session():
    df = _frame(_base() + _session([103.5, 106.8, 104.0, 105.0, 106.0, 104.5]))
    sig, res = _run_bt(df)
    longs = [t for t in res["trades"] if t["side"] == "long"]
    assert len(longs) == 1 and "tp" in longs[0]["exit_reason"]
    assert pd.Timestamp(longs[0]["exit_date"]) <= df.index[51]
    assert list(sig["dt_reason"].iloc[51:54]) == ["long_consumed"] * 3


def test_an_event_blocked_by_the_regime_gate_never_reappears_later_in_its_session():
    today = _session([103.5, 104.0, 104.5, 105.0, 104.5], fill=104.0)
    tomorrow = _session([105.0, 106.0])
    df = _frame(_base() + today + tomorrow)
    sig = ensure_atr_indicator(_apply(df))
    sig["regime"] = np.where((np.arange(len(sig)) >= 47) & (np.arange(len(sig)) <= 50), "ranging", "trending")
    assert list(np.flatnonzero(sig["signal"].to_numpy() == 1)) == [48, 72]
    assert (sig["dt_reason"].iloc[49:72] == "long_consumed").all()
    wide = [{"name": "tiered_tp_pct", "params": {"tp_tiers": [{"profit_pct": 0.5, "close_fraction": 1.0}]}}]
    gated = _bt(close_strategies=wide, stop_loss_atr_mult=None, stop_loss_pct=60.0, regime_enabled=True,
                allowed_regimes=["trending"]).run(sig, strategy_name=NAME, symbol="BTC/USDT", timeframe="1h",
                                                  save=False)
    assert [t["entry_date"] for t in gated["trades"]] == [str(df.index[73])]
    open_gate = _bt(close_strategies=wide, stop_loss_atr_mult=None, stop_loss_pct=60.0).run(
        sig.drop(columns=["regime"]), strategy_name=NAME, symbol="BTC/USDT", timeframe="1h", save=False)
    assert [t["entry_date"] for t in open_gate["trades"]] == [str(df.index[49])]


@pytest.mark.parametrize("path,want", [("up", "tp"), ("down", "sl")])
def test_invalid_entry_data_keeps_close_and_stop_management(path, want):
    head = _base() + _session([101.0, 102.0, 103.5], per=3)
    tail = ([104.0, 105.5, 106.5, 107.5, 108.5, 109.0] if path == "up" else [102.0, 99.0, 97.0, 95.0, 93.0, 92.0])
    rows = head + _session(tail, per=len(tail), anchor=103.5)
    df = _frame(rows)
    df = df.drop(df.index[len(head)])
    sig, res = _run_bt(df)
    after = sig.iloc[len(head):]
    assert (after["signal"] == 0).all() and (after["dt_reason"] == "cadence_gap").all()
    t = res["trades"][0]
    assert t["side"] == "long" and pd.Timestamp(t["exit_date"]) > df.index[len(head) - 1]
    assert ("tp" in t["exit_reason"]) if want == "tp" else (t["exit_reason"] == "sl")


def test_composer_suppresses_entries_while_open_consumes_the_event_and_keeps_closes_on_invalid_data():
    fut = _reg()
    df = _frame(_base() + _session([101.0, 103.5, 104.0, 104.5]))
    pos = {"side": "long", "avg_cost": 100.0, "current_quantity": 1.0, "initial_quantity": 1.0}
    kwargs = dict(close_evaluate=close_evaluate, close_params_by_name={"tiered_tp_pct": TP["params"]})
    held = evaluate_open_close(fut.apply_strategy, fut.get_strategy, df.iloc[:50], NAME, None, ["tiered_tp_pct"],
                               "long", dict(P), pos, market_ctx={"mark_price": 101.0}, **kwargs)
    assert held.open_signal == 1
    decided = finalize_decision(held, "long")
    assert decided["signal"] == 0 and decided["close_fraction"] == 0.0
    later = evaluate_open_close(fut.apply_strategy, fut.get_strategy, df.iloc[:51], NAME, None,
                                ["tiered_tp_pct"], "", dict(P), {}, market_ctx={"mark_price": 104.0}, **kwargs)
    assert later.open_signal == 0 and later.open_result_df["dt_reason"].iloc[-1] == "long_consumed"
    assert finalize_decision(later, "")["signal"] == 0

    gapped = df.iloc[:51].drop(df.index[49])
    out = evaluate_open_close(fut.apply_strategy, fut.get_strategy, gapped, NAME, None, ["tiered_tp_pct"], "long",
                              dict(P), pos, market_ctx={"mark_price": 104.0}, **kwargs)
    assert out.open_signal == 0 and out.open_result_df["dt_reason"].iloc[-1] == "cadence_gap"
    closed = finalize_decision(out, "long")
    assert closed["close_fraction"] == 1.0 and closed["close_strategy"] == "tiered_tp_pct" and closed["signal"] == -1

    flat = finalize_decision(evaluate_open_close(fut.apply_strategy, fut.get_strategy, df.iloc[:50], NAME, None,
                                                 ["tiered_tp_pct"], "", dict(P), {},
                                                 market_ctx={"mark_price": 103.5}, **kwargs), "")
    assert flat["open_action"] == "long" and flat["signal"] == 1


def _parity_cfg(freq, **kw):
    interval = H if freq == "1h" else 4 * H
    base = dict(strategy_name=NAME, params={"lookback_sessions": 2, "k_long": 0.25, "k_short": 0.25,
                                            "session_start_hour": 0, "expected_interval_ms": interval},
                registry="futures", platform="hyperliquid", symbol="BTC", timeframe=freq,
                close_refs=[TP], direction="both")
    base.update(kw)
    return ParityConfig(**base)


@pytest.mark.parametrize("freq,bars", [("1h", 24 * 14), ("4h", 6 * 50)])
@pytest.mark.parametrize("window", [None, 200])
def test_non_batched_decision_parity_is_clean(freq, bars, window):
    df = _walk(bars, freq=freq, seed=31, step=2.0)
    frame = compute_parity_frame(df, cfg=_parity_cfg(freq), window=window)
    s = summarize(frame)
    assert s["bars_compared"] > 0 and s["mismatches"] == 0 and s["clean"] is True
    assert (frame["live_signal"] == 1).any() and (frame["live_signal"] == -1).any()


@pytest.mark.parametrize("freq,bars", [("1h", 24 * 11), ("4h", 6 * 45)])
@pytest.mark.parametrize("window", [None, 200])
def test_batched_explicit_paper_slots_match_solo(freq, bars, window):
    df = _walk(bars, freq=freq, seed=37, step=2.0)
    frame = compute_parity_frame(df, cfg=_parity_cfg(freq, batched=True), window=window)
    s = summarize(frame)
    assert s["bars_compared"] > 0 and s["mismatches"] == 0 and s.get("batch_mismatches", 0) == 0
    assert s["clean"] is True


def _env():
    env = {k: v for k, v in os.environ.items() if not k.startswith(CREDENTIAL_PREFIXES)}
    env["GO_TRADER_HL_OHLCV_CACHE"] = "0"
    return env


def _run(argv, stdin=None):
    return subprocess.run([sys.executable, CHECK_HL] + argv, input=None if stdin is None else json.dumps(stdin),
                          capture_output=True, text=True, cwd=_REPO, env=_env(), timeout=240)


def _payload(proc):
    return json.loads(proc.stdout.strip().splitlines()[-1])


def _hl_bars(df):
    out = []
    for ts, r in df.iterrows():
        t = int(ts.value // 1_000_000)
        out.append({"t": t, "T": t + H - 1, "o": float(r["open"]), "h": float(r["high"]), "l": float(r["low"]),
                    "c": float(r["close"]), "v": 1000.0})
    return out


def _hl_frame(bars, timing=True):
    rows = [[b["T"], b["o"], b["h"], b["l"], b["c"], b["v"]] for b in bars]
    frame = {"rows": rows, "required": len(rows), "bars": len(rows), "coverage_short": False,
             "first_open_ms": bars[0]["t"], "last_open_ms": bars[-1]["t"], "last_close_ms": bars[-1]["T"],
             "last_recv_at_ms": bars[-1]["t"], "source": "ws", "ready": True, "forming_bar_included": True}
    if timing:
        frame["timing"] = {"rule": "hyperliquid_native_close", "interval_ms": H,
                           "bars": [[b["t"], b["T"], True] for b in bars]}
    return frame


def _market(bars, cutoff_ms=None, mid=None, timing=True):
    market = {"version": 4, "snapshot_id": "3600s/1", "generation": 1, "sealed_at_ms": cutoff_ms or bars[-1]["T"],
              "frames": {"BTC|1h": _hl_frame(bars, timing=timing)}, "feed_complete": True}
    if cutoff_ms is not None:
        market["decision_cutoff_ms"] = cutoff_ms
    px = mid if mid is not None else bars[-1]["c"]
    market["mids"] = {"BTC": {"px": px, "recv_at_ms": bars[-1]["T"], "source": "ws", "age_ms": 0, "stale": False,
                              "confirmed": True}}
    return market


def _crossing_bars():
    df = _frame(_days(8) + _session([100.5, 101.0, 101.5, 103.5], per=4))
    return _hl_bars(df)


def _hl_check(bars, *, closed, extra=(), mid=None, cutoff=None):
    argv = [NAME, "BTC", "1h", "--mode=paper", "--market-stdin", "--ohlcv-limit", "200", *extra]
    if closed:
        argv.append("--closed-bar-decisions")
    cutoff = cutoff if cutoff is not None else bars[-1]["t"] + H // 2
    refs = json.dumps({"open": {"name": NAME, "params": dict(P)}, "closes": [TP]})
    proc = _run(argv + ["--strategy-refs", refs], {"v": 2, "market": _market(bars, cutoff, mid)})
    assert proc.returncode == 0, proc.stderr[-3000:] + proc.stdout[-2000:]
    return _payload(proc)


def test_closed_bar_decisions_drop_the_forming_bar_and_report_full_precision_inputs():
    bars = _crossing_bars()
    enabled = _hl_check(bars, closed=True)
    disabled = _hl_check(bars, closed=False)
    assert enabled["closed_bar_decision"]["held"] is False
    assert enabled["closed_bar_decision"]["forming_rows_dropped"] == 1
    assert disabled["signal"] == 1 and enabled["signal"] == 0
    ind = enabled["indicators"]
    assert ind["dt_valid"] == 1.0 and ind["dt_eval_ts_ms"] == float(bars[-2]["t"])
    assert ind["dt_close"] == bars[-2]["c"] and ind["dt_bar_in_session"] == 2.0
    assert enabled["indicator_labels"]["dt_reason"] == "inside_range"
    dind = disabled["indicators"]
    assert disabled["indicator_labels"]["dt_reason"] == "entry_long"
    assert dind["dt_anchor"] == 100.0 and dind["dt_close"] == 103.5
    assert dind["dt_range"] == max(dind["dt_hh"] - dind["dt_lc"], dind["dt_hc"] - dind["dt_ll"])
    assert dind["dt_upper"] == dind["dt_anchor"] + 0.5 * dind["dt_range"]
    calm = _hl_check(bars[:-1], closed=True, cutoff=bars[-1]["t"] + H // 2)
    assert calm["indicators"] == ind


def test_closed_bar_decisions_emit_the_session_long_from_the_closed_bar_after_a_forming_miss():
    bars = _crossing_bars()
    crossing = bars[-1]
    forming_inside = dict(crossing, h=crossing["o"] + 0.1, l=crossing["o"] - 0.1, c=crossing["o"])
    early = bars[:-1] + [forming_inside]
    early_default = _hl_check(early, closed=False)
    assert early_default["signal"] == 0 and early_default["indicator_labels"]["dt_reason"] == "inside_range"
    assert _hl_check(early, closed=True)["signal"] == 0
    nxt = {"t": crossing["t"] + H, "T": crossing["T"] + H, "o": crossing["c"], "h": crossing["c"] + 0.6,
           "l": crossing["c"] - 0.1, "c": crossing["c"] + 0.5, "v": 1000.0}
    late = bars + [nxt]
    late_closed = _hl_check(late, closed=True)
    assert late_closed["closed_bar_decision"]["forming_rows_dropped"] == 1
    assert late_closed["signal"] == 1 and late_closed["indicator_labels"]["dt_reason"] == "entry_long"
    assert late_closed["indicators"]["dt_eval_ts_ms"] == float(crossing["t"])
    late_default = _hl_check(late, closed=False)
    assert late_default["signal"] == 0 and late_default["indicator_labels"]["dt_reason"] == "long_consumed"


def test_batch_slots_match_individual_checks():
    bars = _crossing_bars()[-200:]
    cutoff = bars[-1]["t"] + H // 2
    market = _market(bars, cutoff)
    refs = {"open": {"name": NAME, "params": dict(P)}, "closes": [TP]}
    slots = [
        {"id": "on", "strategy": NAME, "mode": "paper", "mode_args": ["--mode=paper"], "closed_bar_decisions": True,
         "strategy_refs": refs},
        {"id": "off", "strategy": NAME, "mode": "paper", "mode_args": ["--mode=paper"], "strategy_refs": refs},
    ]
    proc = _run(["--batch-check", "--symbol=BTC", "--timeframe=1h", "--ohlcv-limit", "200", "--atr-method=simple",
                 "--market-stdin"], {"v": 2, "slots": slots, "market": market})
    assert proc.returncode == 0, proc.stderr[-3000:]
    results = {r["id"]: r for r in json.loads(proc.stdout)["results"]}
    for slot_id, closed in (("on", True), ("off", False)):
        argv = [NAME, "BTC", "1h", "--mode=paper", "--market-stdin", "--ohlcv-limit", "200", "--strategy-refs",
                json.dumps(refs)]
        if closed:
            argv.append("--closed-bar-decisions")
        single = _run(argv, {"v": 2, "market": market})
        assert single.returncode == 0, single.stderr[-3000:]
        solo = _payload(single)
        for key in ("signal", "open_action", "close_fraction", "indicators", "indicator_labels"):
            assert results[slot_id].get(key) == solo.get(key), (slot_id, key)
    assert results["off"]["signal"] == 1 and results["on"]["signal"] == 0


@pytest.mark.parametrize("closed", [True, False])
def test_invalid_entry_data_in_the_check_holds_the_entry_and_keeps_close_management(closed):
    bars = _crossing_bars()
    gapped = bars[:-3] + [dict(b, t=b["t"] + H, T=b["T"] + H) for b in bars[-3:]]
    flat = _hl_check(gapped, closed=closed)
    assert flat["signal"] == 0 and flat["open_action"] == "none"
    assert flat["indicator_labels"]["dt_reason"] == "cadence_gap"
    held_long = ["--position-side", "long", "--position-avg-cost=100", "--position-qty=1",
                 "--position-initial-qty=1"]
    out = _hl_check(gapped, closed=closed, extra=held_long, mid=104.0)
    assert out["open_action"] == "none"
    assert out["close_fraction"] == 1.0 and out["close_strategy"] == "tiered_tp_pct" and out["signal"] == -1
    assert out["indicators"]["dt_valid"] == 0.0 and out["indicators"]["dt_reason_code"] == 14.0


def _admission_market():
    return _market(_crossing_bars()[-200:], timing=False)


def test_open_reference_without_mode_is_refused():
    proc = _run([NAME, "BTC", "1h"])
    assert proc.returncode == 1, proc.stderr[-2000:]
    payload = _payload(proc)
    assert payload["signal"] == 0 and payload["strategy"] == NAME
    assert f"open strategy '{NAME}' is edge_status=no_edge" in payload["error"]
    assert "missing mode" in payload["error"]


def test_close_fallback_reference_without_mode_is_refused():
    refs = json.dumps({"open": {"name": "breakout"}, "closes": [{"name": NAME}]})
    proc = _run(["breakout", "BTC", "1h", "--strategy-refs", refs])
    assert proc.returncode == 1, proc.stderr[-2000:]
    error = _payload(proc)["error"]
    assert f"close strategy '{NAME}' is edge_status=no_edge" in error and "missing mode" in error


@pytest.mark.parametrize("extra", [["--mode=paper"], ["--mode", "paper"], ["--mode=live", "--allow-no-edge"],
                                   ["--allow-no-edge"]])
def test_paper_or_acknowledged_evaluation_runs_offline_and_reports_inputs(extra):
    proc = _run([NAME, "BTC", "1h", "--market-stdin"] + extra, {"v": 2, "market": _admission_market()})
    assert proc.returncode == 0, proc.stderr[-2000:]
    out = _payload(proc)
    assert out["strategy"] == NAME and not out.get("error")
    assert out["signal"] == 1 and out["indicator_labels"]["dt_reason"] == "entry_long"
    for key in ("dt_hh", "dt_lc", "dt_hc", "dt_ll", "dt_range", "dt_anchor", "dt_upper", "dt_lower", "dt_close",
                "dt_eval_ts_ms", "dt_session_open_ms", "dt_valid", "dt_reason_code"):
        assert key in out["indicators"], key


@pytest.mark.parametrize("extra", [["--mode=paper"], ["--mode=live", "--allow-no-edge"]])
def test_close_fallback_admitted_in_paper_or_acknowledged(extra):
    refs = json.dumps({"open": {"name": "breakout"}, "closes": [{"name": NAME}]})
    proc = _run(["breakout", "BTC", "1h", "--strategy-refs", refs, "--market-stdin"] + extra,
                {"v": 2, "market": _admission_market()})
    assert proc.returncode == 0, proc.stderr[-2000:]
    out = _payload(proc)
    assert not out.get("error") and out["close_strategies"] == [NAME]


def test_batch_slots_apply_the_admission_matrix_and_match_solo():
    fallback = {"strategy": "breakout", "open_strategy": "breakout", "close_strategies": [NAME]}
    slots = [
        {"id": "paper", "strategy": NAME, "mode": "paper", "mode_args": ["--mode=paper"]},
        {"id": "paper-space", "strategy": NAME, "mode": "paper", "mode_args": ["--mode", "paper"]},
        {"id": "acked", "strategy": NAME, "mode": "live", "mode_args": ["--mode=live"], "allow_no_edge": True},
        {"id": "unacked-live", "strategy": NAME, "mode": "live", "mode_args": ["--mode=live"]},
        {"id": "false-live", "strategy": NAME, "mode": "live", "mode_args": ["--mode=live"], "allow_no_edge": False},
        {"id": "missing", "strategy": NAME, "mode": "paper"},
        {"id": "repeated", "strategy": NAME, "mode": "paper", "mode_args": ["--mode=paper", "--mode=paper"]},
        dict(fallback, id="close-paper", mode="paper", mode_args=["--mode=paper"]),
        dict(fallback, id="close-acked", mode="live", mode_args=["--mode=live"], allow_no_edge=True),
        dict(fallback, id="close-unacked", mode="live", mode_args=["--mode=live"]),
        dict(fallback, id="close-missing", mode="paper"),
    ]
    proc = _run(["--batch-check", "--symbol=BTC", "--timeframe=1h", "--market-stdin"],
                {"v": 2, "slots": slots, "market": _admission_market()})
    assert proc.returncode == 1
    results = {r["id"]: r for r in _payload(proc)["results"]}
    for ok in ("paper", "paper-space", "acked", "close-paper", "close-acked"):
        assert not results[ok].get("error"), (ok, results[ok].get("error"))
    refused = {"unacked-live": "explicit live mode", "false-live": "explicit live mode", "missing": "missing mode",
               "repeated": "invalid mode input", "close-unacked": "explicit live mode",
               "close-missing": "missing mode"}
    for slot_id, reason in refused.items():
        assert results[slot_id]["signal"] == 0
        assert reason in results[slot_id]["error"], (slot_id, results[slot_id]["error"])
    solo = _payload(_run([NAME, "BTC", "1h", "--mode=paper", "--market-stdin"], {"v": 2, "market": _admission_market()}))
    for key in ("signal", "open_action", "close_fraction", "indicators", "indicator_labels"):
        assert results["paper"].get(key) == solo.get(key)
        assert results["acked"].get(key) == solo.get(key)
    assert solo["signal"] == 1


@pytest.mark.parametrize("bad", [{"allow_no_edge": "true"}, {"allow_no_edge": 1}, {"mode_args": "--mode=paper"}])
def test_malformed_batch_admission_evidence_rejects_the_payload(bad):
    proc = _run(["--batch-check", "--symbol=BTC", "--timeframe=1h", "--market-stdin"],
                {"v": 2, "slots": [dict({"id": "slot", "strategy": NAME, "mode": "paper"}, **bad)],
                 "market": _admission_market()})
    assert proc.returncode == 1
    payload = _payload(proc)
    assert payload["error_scope"] == "shared_state" and payload["results"] == []


def test_invalid_parameters_fail_only_that_slot_before_evaluation():
    bad = {"id": "bad", "strategy": NAME, "mode": "paper", "mode_args": ["--mode=paper"],
           "params": {"session_start_hour": 1, "expected_interval_ms": 4 * H}}
    good = {"id": "good", "strategy": NAME, "mode": "paper", "mode_args": ["--mode=paper"]}
    proc = _run(["--batch-check", "--symbol=BTC", "--timeframe=1h", "--market-stdin"],
                {"v": 2, "slots": [bad, good], "market": _admission_market()})
    results = {r["id"]: r for r in _payload(proc)["results"]}
    assert results["bad"]["signal"] == 0 and "not on the 14400000 ms candle grid" in results["bad"]["error"]
    assert not results["good"].get("error") and results["good"]["signal"] == 1


RUN_BACKTEST = os.path.join(_REPO, "backtest", "run_backtest.py")


def test_run_backtest_binds_the_interval_from_a_4h_timeframe(tmp_path):
    import storage
    db = str(tmp_path / "ohlcv.db")
    storage.init_db(db)
    df = _walk(6 * 120, freq="4h", start="2025-01-01", seed=41, step=2.5)
    rows = df.reset_index(drop=True)
    rows.insert(0, "timestamp", (df.index.astype("int64") // 10**6).astype("int64"))
    storage.store_ohlcv(rows, "binanceus", "BTC/USDT", "4h", db_path=db)
    env = dict(os.environ, GO_TRADER_OHLCV_CACHE_DB=db)
    cmd = [sys.executable, RUN_BACKTEST, "--mode", "single", "--strategy", NAME, "--registry", "futures",
           "--platform", "hyperliquid", "--symbol", "BTC/USDT", "--since", "2025-01-01", "--timeframe", "4h",
           "--funding", "off", "--direction", "both", "--close-strategy", json.dumps(TP),
           "--stop-loss-atr-mult", "1.0"]
    proc = subprocess.run(cmd, cwd=_REPO, env=env, capture_output=True, text=True, timeout=600)
    assert proc.returncode == 0, proc.stderr + proc.stdout
    params = next(l for l in proc.stdout.splitlines() if l.strip().startswith("Params:"))
    assert "'expected_interval_ms': 14400000" in params
    trades = [l for l in proc.stdout.splitlines() if "Total Trades:" in l]
    assert trades and int(trades[0].split(":")[1]) > 0
