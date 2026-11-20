import os

import numpy as np
import pandas as pd
import pytest

from backtester import Backtester
from registry_loader import load_registry
from atr import ensure_atr_indicator
from eval_windows import compute_incumbent_legs, evaluate_window, INCUMBENTS

NAME = "aroon_trend"
TP = [{"name": "tiered_tp_pct", "params": {"tp_tiers": [{"profit_pct": 0.03, "close_fraction": 1.0}]}}]
CONTRACT = {"direction": "both", "close_strategies": TP, "stop_loss_atr_mult": 1.0, "comparison_mode": "strict"}
_MANIFEST = os.path.abspath(os.path.join(
    os.path.dirname(__file__), "..", "candidates", "aroon_trend_1643", "study_manifest.json"))


def _frame(rows):
    df = pd.DataFrame(rows, columns=["open", "high", "low", "close", "volume"], dtype=float)
    df.index = pd.date_range("2026-01-01", periods=len(df), freq="4h")
    return df


def _trend(n, start=100.0, step=-0.5):
    rows = []
    px = start
    for _ in range(n):
        px += step
        rows.append((px - step / 2, px + 0.4, px - 0.4, px, 1.0))
    return rows


def _spike_up(rows, size=50.0):
    px = rows[-1][3]
    return rows + [(px + 0.3, px + size, px + 0.2, px + size - 1.0, 1.0)]


def _spike_down(rows, size=50.0):
    px = rows[-1][3]
    return rows + [(px - 0.3, px - 0.2, px - size, px - size + 1.0, 1.0)]


def _apply(df, params):
    return load_registry("futures").apply_strategy(NAME, df, params)


def _bt(**kw):
    base = dict(initial_capital=1000.0, platform="hyperliquid",
                open_strategy={"name": NAME, "params": {"period": 10, "strength": 70.0}},
                close_strategies=TP, stop_loss_atr_mult=1.0, direction="both", comparison_mode="strict")
    base.update(kw)
    return Backtester(**base)


def _market(n=420, seed=1643):
    rng = np.random.RandomState(seed)
    t = np.arange(n)
    close = 100 + 10 * np.sin(t / 25.0) + np.cumsum(rng.randn(n) * 0.7)
    open_ = np.concatenate([[close[0]], close[:-1]])
    span = np.abs(rng.randn(n)) * 0.8 + 0.1
    df = pd.DataFrame({"open": open_, "high": np.maximum(open_, close) + span,
                       "low": np.minimum(open_, close) - span, "close": close,
                       "volume": 50 + np.abs(rng.randn(n)) * 40})
    df.index = pd.date_range("2025-01-01", periods=n, freq="4h")
    return df


def test_flat_range_and_input_defects_hold_with_reasons_then_recover(subtests):
    with subtests.test(case="flat_range"):
        _constant_prices_hold_as_a_flat_range()
    with subtests.test(case="missing_columns"):
        _missing_columns_hold_every_row()
    for bad, reason, code in BAD_ROWS:
        with subtests.test(case=f"bad_row {bad}"):
            _a_bad_row_holds_its_whole_support_then_recovers(bad, reason, code)
    with subtests.test(case="timestamp_order"):
        _timestamp_defects_hold()


def _constant_prices_hold_as_a_flat_range():
    out = _apply(_frame([(100.0, 100.0, 100.0, 100.0, 1.0)] * 30), {"period": 10, "strength": 70.0})
    assert (out["signal"] == 0).all()
    assert set(out["aroon_reason"].iloc[10:]) == {"flat_range"}
    assert (out["aroon_reason_code"].iloc[10:] == 14).all()
    assert out["aroon_up"].isna().all()


def _missing_columns_hold_every_row():
    df = _frame(_trend(30)).drop(columns=["low"])
    out = _apply(df, {"period": 10, "strength": 70.0})
    assert (out["signal"] == 0).all()
    assert set(out["aroon_reason"]) == {"missing_columns:low"}
    assert (out["aroon_reason_code"] == 15).all()


BAD_ROWS = [
    ({"close": float("nan")}, "nonfinite_input", 11),
    ({"high": float("inf")}, "nonfinite_input", 11),
    ({"high": 50.0}, "inconsistent_hlc", 12),
    ({"close": 200.0}, "inconsistent_hlc", 12),
]


def _a_bad_row_holds_its_whole_support_then_recovers(bad, reason, code):
    p = 10
    df = _frame(_trend(60))
    bad_row = 30
    for col, value in bad.items():
        df.iloc[bad_row, df.columns.get_loc(col)] = value
    out = _apply(df, {"period": p, "strength": 70.0})
    held = out.iloc[bad_row:bad_row + p + 2]
    assert (held["aroon_reason"] == reason).all() and (held["aroon_reason_code"] == code).all()
    assert not held["aroon_valid"].any() and (held["signal"] == 0).all()
    assert held["aroon_prev_up"].isna().all()
    assert bool(out["aroon_valid"].iloc[bad_row + p + 2])


def _timestamp_defects_hold():
    df = _frame(_trend(40))
    idx = list(df.index)
    idx[25] = idx[24]
    df.index = pd.DatetimeIndex(idx)
    out = _apply(df, {"period": 10, "strength": 70.0})
    assert (out["aroon_reason"].iloc[25:37] == "timestamp_order").all()
    assert bool(out["aroon_valid"].iloc[37])


PREFIX_PARAMS = [{"period": 25, "strength": 70.0}, {"period": 2, "strength": 0.0},
                 {"period": 198, "strength": 100.0}, {"period": 14, "strength": 50.0}]


def test_backtest_entries_fill_at_the_next_open_and_never_reverse_or_add():
    rows = _spike_up(_trend(30))
    rows += [(rows[-1][3] + 0.5, rows[-1][3] + 1.0, rows[-1][3] - 0.5, rows[-1][3] + 0.5, 1.0)] * 3
    rows = _spike_down(rows, size=120.0)
    rows += [(rows[-1][3], rows[-1][3] + 0.3, rows[-1][3] - 0.3, rows[-1][3], 1.0)] * 4
    df = _frame(rows)
    sig = ensure_atr_indicator(_apply(df, {"period": 10, "strength": 70.0}))
    entries = sig.index[sig["signal"] != 0].tolist()
    assert sig.loc[entries[0], "signal"] == 1
    res = _bt(stop_loss_atr_mult=None, stop_loss_pct=50.0).run(
        sig, strategy_name=NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    first = res["trades"][0]
    fill = df.index.get_loc(entries[0]) + 1
    assert first["side"] == "long"
    assert first["entry_date"] == str(df.index[fill])
    assert first["entry_price"] == pytest.approx(df["open"].iloc[fill] * 1.0005)
    assert sig.loc[entries[1], "signal"] == -1 and entries[1] < pd.Timestamp(first["exit_date"])
    assert len(res["trades"]) == 1
    assert first["exit_reason"] == "end_of_data"


def test_incumbent_contract_reaches_every_incumbent_and_default_callers_are_unchanged():
    import offline_manifest as om
    manifest = om.load_manifest(_MANIFEST)
    reg = load_registry("futures")
    ctx = {"manifest": manifest, "window": "test", "cost_multiplier": 1.0}
    datasets = [("BTC", "4h")]
    plain = compute_incumbent_legs(reg, datasets, (None, None), 1000.0, manifest_ctx=ctx)
    shaped = compute_incumbent_legs(reg, datasets, (None, None), 1000.0, manifest_ctx=ctx, contract=CONTRACT)
    assert sorted(plain["BTC 4h"]) == sorted(shaped["BTC 4h"]) == sorted(INCUMBENTS)
    for name in INCUMBENTS:
        p, s = plain["BTC 4h"][name], shaped["BTC 4h"][name]
        assert p["manifest"]["cost_model"] == "legacy_flat"
        assert p["execution"]["short_positions"] == 0
        assert s["manifest"]["cost_model"] == "execution_spec"
        assert s["close_validation"]["mode"] == "strict"
    assert sum(shaped["BTC 4h"][n]["execution"]["short_positions"] for n in INCUMBENTS) > 0

    memo = {}
    cand = dict(CONTRACT, name=NAME, params={"period": 25, "strength": 70.0})
    default = evaluate_window(reg, cand, datasets, "test", 1000.0, memo, manifest=manifest)
    contract = evaluate_window(reg, cand, datasets, "test", 1000.0, memo, manifest=manifest,
                               incumbent_contract=CONTRACT)
    keys = sorted(memo, key=len)
    assert len(keys) == 2 and len(keys[0]) == 4 and len(keys[1]) == 5
    assert "incumbent_contract" not in default and contract["incumbent_contract"] == CONTRACT
    assert default["bars"] != contract["bars"]
    assert default["rows"][0]["leg"]["return_pct"] == contract["rows"][0]["leg"]["return_pct"]
