import contextlib

import numpy as np
import pandas as pd
import pytest

from backtester import Backtester
from atr import ensure_atr_indicator
from closed_bar import declare_frame_cadence
from optimizer import DEFAULT_PARAM_RANGES
from parity_diff import ParityConfig, compute_parity_frame, summarize
from registry_loader import load_registry

NAME = "regression_channel_reversion"
TP = [{"name": "tiered_tp_pct", "params": {"tp_tiers": [{"profit_pct": 0.03, "close_fraction": 1.0}]}}]
SEED = {"fit_window": 50, "band_multiplier": 2.0, "slope_ceiling": 0.10, "min_residual_std": 0.0}
SMALL = {"fit_window": 8, "band_multiplier": 2.0, "slope_ceiling": 0.10, "min_residual_std": 0.0}
FIT_FIELDS = ("rcr_slope", "rcr_intercept", "rcr_fit_mean", "rcr_resid_std", "rcr_slope_norm",
              "rcr_forecast", "rcr_lower", "rcr_upper")
REG = load_registry("futures")


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


def _frame(closes, freq="4h", kind="open", declare=True, start="2026-01-01"):
    closes = np.asarray(closes, dtype=float)
    df = pd.DataFrame({"open": closes, "high": closes + 0.5, "low": closes - 0.5, "close": closes,
                       "volume": np.full(len(closes), 100.0)})
    df.index = pd.date_range(start, periods=len(df), freq=freq)
    if declare:
        declare_frame_cadence(df, freq, kind)
    return df


def _market(n=420, seed=1644, freq="4h"):
    rng = np.random.RandomState(seed)
    t = np.arange(n)
    close = 100 + 4 * np.sin(t / 9.0) + np.cumsum(rng.randn(n) * 0.35)
    open_ = np.concatenate([[close[0]], close[:-1]])
    span = np.abs(rng.randn(n)) * 0.6 + 0.1
    df = pd.DataFrame({"open": open_, "high": np.maximum(open_, close) + span,
                       "low": np.minimum(open_, close) - span, "close": close,
                       "volume": 50 + np.abs(rng.randn(n)) * 40})
    df.index = pd.date_range("2025-01-01", periods=n, freq=freq)
    return declare_frame_cadence(df, freq, "open")


def _apply(df, params=None):
    return REG.apply_strategy(NAME, df, dict(SEED if params is None else params))


def _reference(closes, i, w, k):
    window = np.asarray(closes[i - w:i], dtype=float)
    x = np.arange(w, dtype=float)
    slope, intercept = np.polyfit(x, window, 1)
    resid = window - (intercept + slope * x)
    std = float(np.sqrt(np.mean(resid ** 2)))
    forecast = intercept + slope * w
    mean = float(window.mean())
    return {"rcr_slope": slope, "rcr_intercept": intercept, "rcr_fit_mean": mean, "rcr_resid_std": std,
            "rcr_slope_norm": abs(slope) * (w - 1) / mean, "rcr_forecast": forecast,
            "rcr_lower": forecast - k * std, "rcr_upper": forecast + k * std}


def _symmetric(n_blocks, d=0.5, base=100.0):
    return [base + d, base - d, base - d, base + d] * n_blocks


def test_registered_futures_only_hidden_and_grid_fits_the_closed_window():
    assert NAME in REG.STRATEGY_REGISTRY
    assert NAME not in REG.list_strategies()
    assert NAME not in load_registry("spot").STRATEGY_REGISTRY
    entry = REG.STRATEGY_REGISTRY[NAME]
    assert entry["edge_status"] == "no_edge"
    assert entry["edge_ref"] == "backtest/candidates/regression_channel_reversion_1644/REPORT.md"
    assert entry["default_params"] == SEED
    grid = DEFAULT_PARAM_RANGES[NAME]
    assert sorted(grid) == sorted(SEED)
    df = _market(260)
    for w in grid["fit_window"]:
        assert w + 2 <= 199
        out = _apply(df, dict(SEED, fit_window=w, slope_ceiling=1.0))
        assert out["rcr_valid"].iloc[: w + 1].sum() == 0
        assert bool(out["rcr_valid"].iloc[w + 1])
    for key, values in grid.items():
        for value in values:
            REG.validate_param_value(NAME, key, value)


INVALID_PARAMS = [
    ({"fit_window": True}, "fit_window"),
    ({"fit_window": 2.5}, "fit_window must be an integer"),
    ({"fit_window": 1}, "fit_window"),
    ({"fit_window": 198}, "fit_window"),
    ({"fit_window": "50"}, "fit_window must be an integer"),
    ({"fit_window": None}, "fit_window must be an integer"),
    ({"band_multiplier": 0.0}, "band_multiplier"),
    ({"band_multiplier": -1.0}, "band_multiplier"),
    ({"band_multiplier": float("inf")}, "band_multiplier"),
    ({"band_multiplier": False}, "band_multiplier"),
    ({"slope_ceiling": -0.01}, "slope_ceiling"),
    ({"slope_ceiling": float("nan")}, "slope_ceiling"),
    ({"slope_ceiling": "0.1"}, "slope_ceiling must be a number"),
    ({"min_residual_std": -1.0}, "min_residual_std"),
    ({"min_residual_std": float("inf")}, "min_residual_std"),
    ({"min_residual_std": None}, "min_residual_std must be a number"),
]


def test_invalid_parameters_are_refused_by_every_registry_entry_point():
    for params, needle in INVALID_PARAMS:
        with _case((params, needle)):
            with pytest.raises(ValueError, match=needle):
                REG.apply_strategy(NAME, _market(60), params)
            with pytest.raises(ValueError, match=needle):
                REG.validate_params(NAME, params)
            (key, value), = params.items()
            with pytest.raises(ValueError, match=needle):
                REG.validate_param_value(NAME, key, value)


@pytest.mark.parametrize("params", [SEED, dict(SEED, fit_window=20, band_multiplier=1.5, slope_ceiling=1.0),
                                    dict(SEED, fit_window=197, band_multiplier=3.0, slope_ceiling=0.2)])
def test_frozen_fit_matches_an_independent_least_squares_reference(params):
    df = _market(420)
    out = _apply(df, params)
    w, k = params["fit_window"], params["band_multiplier"]
    closes = df["close"].to_numpy()
    checked = 0
    for i in range(w + 1, len(df), 3):
        ref, prev = _reference(closes, i, w, k), _reference(closes, i - 1, w, k)
        row = out.iloc[i]
        assert bool(row["rcr_valid"])
        for col in FIT_FIELDS:
            assert row[col] == pytest.approx(ref[col], rel=1e-9, abs=1e-9), (i, col)
            assert row[col.replace("rcr_", "rcr_prev_")] == pytest.approx(prev[col], rel=1e-9, abs=1e-9), (i, col)
        margins = (closes[i - 1] - prev["rcr_lower"], closes[i - 1] - prev["rcr_upper"],
                   closes[i] - ref["rcr_lower"], closes[i] - ref["rcr_upper"],
                   ref["rcr_slope_norm"] - params["slope_ceiling"])
        if min(abs(m) for m in margins) < 1e-7:
            continue
        setup_long = closes[i - 1] <= prev["rcr_lower"] and ref["rcr_lower"] < closes[i] <= ref["rcr_upper"]
        setup_short = closes[i - 1] >= prev["rcr_upper"] and ref["rcr_lower"] <= closes[i] < ref["rcr_upper"]
        gate = ref["rcr_slope_norm"] <= params["slope_ceiling"] and ref["rcr_resid_std"] >= params["min_residual_std"]
        want = 1 if setup_long and gate else (-1 if setup_short and gate else 0)
        assert row["signal"] == want, i
        assert row["rcr_close"] == closes[i] and row["rcr_prev_close"] == closes[i - 1]
        checked += 1
    assert checked > 50


def _dip(closes, depth):
    return closes + [closes[-1] - depth]


def test_long_and_short_reentries_fire_only_on_the_reentry_bar():
    base = _symmetric(4)
    long_rows = _dip(base, 3.0) + [100.0]
    out = _apply(_frame(long_rows), SMALL)
    dip, back = out.iloc[-2], out.iloc[-1]
    assert dip["signal"] == 0 and dip["rcr_lower"] == 99.0 and dip["rcr_upper"] == 101.0
    assert dip["rcr_slope"] == 0.0 and dip["rcr_resid_std"] == 0.5 and dip["rcr_forecast"] == 100.0
    assert back["signal"] == 1 and back["rcr_reason"] == "entry_long" and back["rcr_reason_code"] == 1
    assert back["rcr_prev_close"] == long_rows[-2] and back["rcr_prev_lower"] == 99.0
    assert back["rcr_lower"] < 100.0 <= back["rcr_upper"]
    after = _apply(_frame(long_rows + [100.0]), SMALL).iloc[-1]
    assert after["signal"] == 0 and after["rcr_reason"] == "no_reentry"

    short_rows = base + [base[-1] + 4.0, 100.0]
    out = _apply(_frame(short_rows), SMALL)
    spike, back = out.iloc[-2], out.iloc[-1]
    assert spike["signal"] == 0 and spike["rcr_upper"] == 101.0
    assert back["signal"] == -1 and back["rcr_reason"] == "entry_short" and back["rcr_reason_code"] == -1
    assert back["rcr_prev_upper"] == 101.0
    after = _apply(_frame(short_rows + [100.0]), SMALL).iloc[-1]
    assert after["signal"] == 0 and after["rcr_reason"] == "no_reentry"

    stays_out = _apply(_frame(_dip(base, 3.0) + [90.0]), SMALL).iloc[-1]
    assert stays_out["signal"] == 0 and stays_out["rcr_reason"] == "no_reentry"


def test_band_boundaries_follow_the_declared_inequalities():
    rows = _dip(_symmetric(4), 3.0) + [100.0]
    base = _apply(_frame(rows), SMALL)
    lower, upper = float(base["rcr_lower"].iloc[-1]), float(base["rcr_upper"].iloc[-1])

    at_lower = _apply(_frame(rows[:-1] + [lower]), SMALL).iloc[-1]
    assert at_lower["rcr_lower"] == lower and at_lower["signal"] == 0
    at_upper = _apply(_frame(rows[:-1] + [upper]), SMALL).iloc[-1]
    assert at_upper["rcr_upper"] == upper and at_upper["signal"] == 1
    above = _apply(_frame(rows[:-1] + [np.nextafter(upper, np.inf)]), SMALL).iloc[-1]
    assert above["signal"] == 0

    prev_equal = _symmetric(4) + [99.0, 100.0]
    out = _apply(_frame(prev_equal), SMALL)
    assert out["rcr_prev_lower"].iloc[-1] == 99.0 and out["rcr_prev_close"].iloc[-1] == 99.0
    assert out["signal"].iloc[-1] == 1
    prev_inside = _apply(_frame(_symmetric(4) + [np.nextafter(99.0, np.inf), 100.0]), SMALL).iloc[-1]
    assert prev_inside["signal"] == 0 and prev_inside["rcr_reason"] == "no_reentry"


def test_slope_and_deviation_gates_hold_with_reasons_at_inclusive_bounds():
    rows = _dip(_symmetric(4), 3.0) + [100.0]
    ref = _apply(_frame(rows), SMALL).iloc[-1]
    assert ref["signal"] == 1
    slope_norm, std = float(ref["rcr_slope_norm"]), float(ref["rcr_resid_std"])
    assert slope_norm > 0

    gated = _apply(_frame(rows), dict(SMALL, slope_ceiling=np.nextafter(slope_norm, 0))).iloc[-1]
    assert gated["signal"] == 0 and gated["rcr_reason"] == "slope_gate" and gated["rcr_reason_code"] == 2
    at_ceiling = _apply(_frame(rows), dict(SMALL, slope_ceiling=slope_norm)).iloc[-1]
    assert at_ceiling["signal"] == 1
    zero_ceiling = _apply(_frame(rows), dict(SMALL, slope_ceiling=0.0)).iloc[-1]
    assert zero_ceiling["rcr_reason"] == "slope_gate"

    floor = _apply(_frame(rows), dict(SMALL, min_residual_std=np.nextafter(std, np.inf))).iloc[-1]
    assert floor["signal"] == 0 and floor["rcr_reason"] == "deviation_gate" and floor["rcr_reason_code"] == 3
    exact = _apply(_frame(rows), dict(SMALL, min_residual_std=std)).iloc[-1]
    assert exact["signal"] == 1


def test_steep_rising_and_falling_channels_are_slope_gated():
    for step, start in ((1.5, 100.0), (-1.5, 250.0)):
        with _case(f"step={step} start={start}"):
            _steep_channel_is_slope_gated(step, start)


def _steep_channel_is_slope_gated(step, start):
    closes = [start + step * i + (0.4 if i % 2 else -0.4) for i in range(80)]
    for i in range(14, 80, 15):
        closes[i] -= 4.0 if step > 0 else -4.0
    params = dict(SEED, fit_window=20, band_multiplier=1.0)
    out = _apply(_frame(closes), params)
    valid = out[out["rcr_valid"]]
    assert len(valid) == 80 - 21
    assert (valid["rcr_slope_norm"] > params["slope_ceiling"]).all()
    assert (out["signal"] == 0).all()
    assert set(out["rcr_reason"].iloc[21:]) == {"no_reentry", "slope_gate"}
    ungated = _apply(_frame(closes), dict(params, slope_ceiling=1.0))
    assert (ungated["signal"] != 0).sum() == (out["rcr_reason"] == "slope_gate").sum()


def test_constant_prices_and_exact_lines_hold_as_degenerate_fits():
    out = _apply(_frame([100.0] * 40), SMALL)
    assert (out["signal"] == 0).all()
    assert set(out["rcr_reason"].iloc[9:]) == {"degenerate_fit"}
    assert (out["rcr_reason_code"].iloc[9:] == 17).all()
    assert out["rcr_lower"].isna().all()

    line = _apply(_frame([100.0 + 0.1 * i for i in range(40)]), dict(SMALL, slope_ceiling=1.0))
    assert set(line["rcr_reason"].iloc[9:]) == {"degenerate_fit"}
    assert (line["signal"] == 0).all()

    two = _apply(_market(60), dict(SEED, fit_window=2, slope_ceiling=1.0))
    assert set(two["rcr_reason"].iloc[3:]) == {"degenerate_fit"}


def test_missing_columns_cadence_and_timestamps_hold_every_row():
    df = _frame(_symmetric(5)).drop(columns=["close"])
    out = _apply(df, SMALL)
    assert set(out["rcr_reason"]) == {"missing_columns:close"} and (out["rcr_reason_code"] == 19).all()
    nodecl = _apply(_frame(_symmetric(5), declare=False), SMALL)
    assert set(nodecl["rcr_reason"]) == {"missing_cadence"} and (nodecl["signal"] == 0).all()
    noidx = _frame(_symmetric(5)).reset_index(drop=True)
    out = _apply(noidx, SMALL)
    assert set(out["rcr_reason"]) == {"missing_timestamps"} and (out["rcr_reason_code"] == 14).all()


BAD_PRICES = [
    (float("nan"), "nonfinite_input", 15),
    (float("inf"), "nonfinite_input", 15),
    (0.0, "nonpositive_price", 16),
    (-5.0, "nonpositive_price", 16),
]


def test_a_bad_price_holds_its_whole_dependency_span_then_recovers():
    for value, reason, code in BAD_PRICES:
        with _case(f"{value}-{reason}-{code}"):
            _a_bad_price_holds_its_whole_dependency_span_then_recovers(value, reason, code)


def _a_bad_price_holds_its_whole_dependency_span_then_recovers(value, reason, code):
    w = 10
    df = _market(80)
    bad = 40
    df.iloc[bad, df.columns.get_loc("close")] = value
    out = _apply(df, dict(SEED, fit_window=w, slope_ceiling=1.0))
    held = out.iloc[bad:bad + w + 2]
    assert (held["rcr_reason"] == reason).all() and (held["rcr_reason_code"] == code).all()
    assert not held["rcr_valid"].any() and (held["signal"] == 0).all()
    assert held["rcr_prev_lower"].isna().all()
    assert bool(out["rcr_valid"].iloc[bad + w + 2])


def test_timestamp_and_cadence_defects_hold_the_span_then_recover():
    for mutate, reason, last_held in (("gap", "cadence_gap", 10), ("duplicate", "timestamp_order", 11),
                                      ("reverse", "timestamp_order", 11)):
        with _case(f"{mutate}-{reason}-{last_held}"):
            _timestamp_and_cadence_defects_hold_the_span_then_recover(mutate, reason, last_held)


def _timestamp_and_cadence_defects_hold_the_span_then_recover(mutate, reason, last_held):
    w = 10
    df = _market(80)
    idx = list(df.index)
    at = 40
    if mutate == "gap":
        idx = idx[:at] + [t + pd.Timedelta(hours=4) for t in idx[at:]]
    elif mutate == "duplicate":
        idx[at] = idx[at - 1]
    else:
        idx[at], idx[at - 1] = idx[at - 1], idx[at]
    df.index = pd.DatetimeIndex(idx)
    out = _apply(df, dict(SEED, fit_window=w, slope_ceiling=1.0))
    assert out["rcr_reason"].iloc[at] == reason
    first = at - 1 if mutate == "reverse" else at
    held = out.iloc[first:at + last_held + 1]
    assert not held["rcr_valid"].any() and (held["signal"] == 0).all()
    assert set(held["rcr_reason"]) <= {"cadence_gap", "timestamp_order"}
    assert bool(out["rcr_valid"].iloc[at + last_held + 1])
    assert bool(out["rcr_valid"].iloc[first - 1])


def test_native_close_timestamps_report_the_bar_opening_time():
    df = _market(80, freq="1h")
    df.index = df.index + pd.Timedelta(milliseconds=3_600_000 - 1)
    declare_frame_cadence(df, "1h", "native_close_inclusive")
    out = _apply(df, dict(SEED, fit_window=20))
    row = out.iloc[-1]
    assert row["rcr_eval_ts_ms"] == (df.index[-1].value // 1_000_000) - 3_600_000 + 1
    assert row["rcr_prev_eval_ts_ms"] == row["rcr_eval_ts_ms"] - 3_600_000
    assert row["rcr_interval_ms"] == 3_600_000.0


def test_boundary_fit_windows_need_exactly_fit_window_plus_two_rows():
    for w in (2, 3, 197):
        with _case(f"w={w}"):
            _boundary_fit_window_needs_exactly_fit_window_plus_two_rows(w)


def _boundary_fit_window_needs_exactly_fit_window_plus_two_rows(w):
    need = w + 2
    params = dict(SEED, fit_window=w, slope_ceiling=1.0)
    short = _apply(_market(need - 1), params)
    assert not short["rcr_valid"].any()
    assert (short["rcr_reason"] == "insufficient_history").all()
    exact = _apply(_market(need), params)
    if w == 2:
        assert exact["rcr_reason"].iloc[-1] == "degenerate_fit"
    else:
        assert exact["rcr_valid"].tolist() == [False] * (need - 1) + [True]


@pytest.mark.parametrize("params", [SEED, dict(SEED, fit_window=20, band_multiplier=1.5, slope_ceiling=1.0),
                                    dict(SEED, fit_window=197, band_multiplier=3.0, slope_ceiling=0.2,
                                         min_residual_std=0.1),
                                    dict(SEED, fit_window=3, band_multiplier=2.5, slope_ceiling=0.05)])
def test_every_prefix_and_bounded_tail_repeat_the_full_series_decision(params):
    df = _market(420)
    full = _apply(df, params)
    need = params["fit_window"] + 2
    cols = ["signal", "rcr_reason_code", "rcr_lower", "rcr_upper", "rcr_prev_lower", "rcr_prev_upper"]
    for end in range(1, len(df) + 1):
        prefix = _apply(df.iloc[:end], params).iloc[-1]
        assert prefix[cols].equals(full.iloc[end - 1][cols]), end
        if end >= need:
            for size in (200, 199):
                tail = _apply(df.iloc[max(0, end - size):end], params).iloc[-1]
                assert tail[cols].equals(full.iloc[end - 1][cols]), (end, size)
    changed = df.copy()
    changed.iloc[300:, changed.columns.get_loc("close")] *= 1.5
    again = _apply(changed, params)
    assert again.iloc[:300][cols].equals(full.iloc[:300][cols])


def _bt(**kw):
    base = dict(initial_capital=1000.0, platform="hyperliquid",
                open_strategy={"name": NAME, "params": dict(SMALL)},
                close_strategies=TP, stop_loss_atr_mult=1.0, direction="both", comparison_mode="strict")
    base.update(kw)
    return Backtester(**base)


def test_backtest_entries_fill_at_the_next_open_and_never_reverse_or_add():
    rows = _dip(_symmetric(4), 3.0) + [100.0]
    rows += [100.2, 100.4, 100.1, 100.3]
    rows += [rows[-1] + 5.0, 100.5]
    rows += [100.3, 100.4, 100.2, 100.3, 100.25]
    df = _frame(rows)
    sig = ensure_atr_indicator(_apply(df, SMALL))
    entries = sig.index[sig["signal"] != 0].tolist()
    assert sig.loc[entries[0], "signal"] == 1
    assert len(entries) >= 2
    res = _bt(stop_loss_atr_mult=None, stop_loss_pct=50.0).run(
        sig, strategy_name=NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    first = res["trades"][0]
    fill = df.index.get_loc(entries[0]) + 1
    assert first["side"] == "long"
    assert first["entry_date"] == str(df.index[fill])
    assert first["entry_price"] == pytest.approx(df["open"].iloc[fill] * 1.0005)
    exit_at = pd.Timestamp(first["exit_date"])
    suppressed = [e for e in entries if df.index[fill] <= e < exit_at]
    assert suppressed, "the fixture must emit an entry signal while the first position is open"
    for later in res["trades"][1:]:
        assert pd.Timestamp(later["entry_date"]) > exit_at
    fills = {str(df.index[df.index.get_loc(e) + 1]) for e in entries if df.index.get_loc(e) + 1 < len(df)}
    for trade in res["trades"]:
        assert trade["entry_date"] in fills
    assert sum(1 for t in res["trades"] if pd.Timestamp(t["entry_date"]) < exit_at) == 1


def test_invalid_entry_data_keeps_close_and_stop_processing():
    rows = _dip(_symmetric(4), 3.0) + [100.0]
    rows += [float("nan")]
    rows += [104.0, 104.5, 105.0]
    df = _frame(rows)
    df["high"] = df["high"].fillna(100.6)
    df["low"] = df["low"].fillna(99.6)
    df["open"] = df["open"].fillna(100.1)
    df.iloc[len(rows) - 3:, df.columns.get_loc("high")] = 106.0
    sig = ensure_atr_indicator(_apply(df, SMALL))
    assert (sig["signal"].iloc[len(rows) - 4:] == 0).all()
    assert (sig["rcr_reason"].iloc[len(rows) - 4:] == "nonfinite_input").all()
    res = _bt(stop_loss_atr_mult=None, stop_loss_pct=50.0).run(
        sig, strategy_name=NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    assert len(res["trades"]) == 1
    assert res["trades"][0]["exit_reason"].startswith("tiered_tp_pct")

    crash = _dip(_symmetric(4), 3.0) + [100.0, 100.0, 60.0, float("nan"), float("nan")]
    cdf = _frame(crash)
    cdf.iloc[-3:, cdf.columns.get_loc("low")] = 55.0
    cdf.iloc[-3:, cdf.columns.get_loc("high")] = 101.0
    cdf.iloc[-3:, cdf.columns.get_loc("open")] = 99.0
    sig2 = ensure_atr_indicator(_apply(cdf, SMALL))
    res2 = _bt().run(sig2, strategy_name=NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    assert res2["trades"][0]["exit_reason"] == "sl"


@pytest.mark.parametrize("batched,window", [
    pytest.param(False, None, id="None-False"),
    pytest.param(False, 200, id="200-False"),
    pytest.param(True, 200, id="200-True"),
])
def test_parity_frame_matches_on_every_compared_bar(batched, window):
    df = _market(420)
    params = dict(SEED, fit_window=20, slope_ceiling=1.0)
    if batched:
        cfg = ParityConfig(strategy_name=NAME, params=params, registry="futures", batched=True,
                           symbol="BTC", timeframe="4h")
        frame = compute_parity_frame(df, cfg=cfg, window=window or 200)
        assert summarize(frame)["batch_mismatches"] == 0
    else:
        frame = compute_parity_frame(df, NAME, params=params, registry="futures", close_refs=TP,
                                     comparison_mode="strict", window=window)
    assert len(frame) > 100
    assert bool(frame["match"].all())
    assert set(frame["live_open_action"]) >= {"long", "short"}
