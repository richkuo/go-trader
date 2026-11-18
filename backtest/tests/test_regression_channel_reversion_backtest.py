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


def _dip(closes, depth):
    return closes + [closes[-1] - depth]


def test_native_close_timestamps_report_the_bar_opening_time():
    df = _market(80, freq="1h")
    df.index = df.index + pd.Timedelta(milliseconds=3_600_000 - 1)
    declare_frame_cadence(df, "1h", "native_close_inclusive")
    out = _apply(df, dict(SEED, fit_window=20))
    row = out.iloc[-1]
    assert row["rcr_eval_ts_ms"] == (df.index[-1].value // 1_000_000) - 3_600_000 + 1
    assert row["rcr_prev_eval_ts_ms"] == row["rcr_eval_ts_ms"] - 3_600_000
    assert row["rcr_interval_ms"] == 3_600_000.0


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
