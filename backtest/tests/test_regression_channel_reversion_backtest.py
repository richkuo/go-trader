import numpy as np
import pandas as pd
import pytest

from backtester import Backtester
from atr import ensure_atr_indicator
from closed_bar import declare_frame_cadence
from registry_loader import load_registry

NAME = "regression_channel_reversion"
TP = [{"name": "tiered_tp_pct", "params": {"tp_tiers": [{"profit_pct": 0.03, "close_fraction": 1.0}]}}]
SEED = {"fit_window": 50, "band_multiplier": 2.0, "slope_ceiling": 0.10, "min_residual_std": 0.0}
SMALL = {"fit_window": 8, "band_multiplier": 2.0, "slope_ceiling": 0.10, "min_residual_std": 0.0}
FIT_FIELDS = ("rcr_slope", "rcr_intercept", "rcr_fit_mean", "rcr_resid_std", "rcr_slope_norm",
              "rcr_forecast", "rcr_lower", "rcr_upper")
REG = load_registry("futures")


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


REFERENCE_PARAMS = [SEED, dict(SEED, fit_window=20, band_multiplier=1.5, slope_ceiling=1.0),
                    dict(SEED, fit_window=197, band_multiplier=3.0, slope_ceiling=0.2)]


def test_frozen_fit_matches_an_independent_least_squares_reference(subtests):
    for i, params in enumerate(REFERENCE_PARAMS):
        with subtests.test(case=f"params{i}"):
            _frozen_fit_matches_an_independent_least_squares_reference(params)


def _frozen_fit_matches_an_independent_least_squares_reference(params):
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


def test_reentry_entry_rule_band_inequalities_and_gates(subtests):
    with subtests.test(case="long_and_short_reentries"):
        _long_and_short_reentries_fire_only_on_the_reentry_bar()
    with subtests.test(case="band_boundaries"):
        _band_boundaries_follow_the_declared_inequalities()
    with subtests.test(case="slope_and_deviation_gates"):
        _slope_and_deviation_gates_hold_with_reasons_at_inclusive_bounds()
    for step, start in ((1.5, 100.0), (-1.5, 250.0)):
        with subtests.test(case=f"steep_channel step={step} start={start}"):
            _steep_channel_is_slope_gated(step, start)


def _long_and_short_reentries_fire_only_on_the_reentry_bar():
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


def _band_boundaries_follow_the_declared_inequalities():
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


def _slope_and_deviation_gates_hold_with_reasons_at_inclusive_bounds():
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


def test_native_close_timestamps_report_the_bar_opening_time():
    df = _market(80, freq="1h")
    df.index = df.index + pd.Timedelta(milliseconds=3_600_000 - 1)
    declare_frame_cadence(df, "1h", "native_close_inclusive")
    out = _apply(df, dict(SEED, fit_window=20))
    row = out.iloc[-1]
    assert row["rcr_eval_ts_ms"] == (df.index[-1].value // 1_000_000) - 3_600_000 + 1
    assert row["rcr_prev_eval_ts_ms"] == row["rcr_eval_ts_ms"] - 3_600_000
    assert row["rcr_interval_ms"] == 3_600_000.0


def test_later_rows_never_change_an_earlier_decision():
    params = dict(SEED, fit_window=20, band_multiplier=1.5, slope_ceiling=1.0)
    df = _market(420)
    full = _apply(df, params)
    cols = ["signal", "rcr_reason_code", "rcr_lower", "rcr_upper", "rcr_prev_lower", "rcr_prev_upper"]
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
