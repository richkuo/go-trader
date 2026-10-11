import os

import numpy as np
import pandas as pd
import pytest

from backtester import Backtester
from registry_loader import load_registry
from atr import ensure_atr_indicator
from eval_windows import compute_incumbent_legs, evaluate_window, incumbent_contract_kwargs, INCUMBENTS
from optimizer import DEFAULT_PARAM_RANGES
from parity_diff import compute_parity_frame, summarize

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


def test_registered_futures_only_hidden_and_grid_fits_the_live_window():
    fut = load_registry("futures")
    assert NAME in fut.STRATEGY_REGISTRY
    assert NAME not in fut.list_strategies()
    assert NAME not in load_registry("spot").STRATEGY_REGISTRY
    entry = fut.STRATEGY_REGISTRY[NAME]
    assert entry["edge_status"] == "no_edge"
    assert entry["edge_ref"] == "backtest/candidates/aroon_trend_1643/REPORT.md"
    grid = DEFAULT_PARAM_RANGES[NAME]
    df = _market(260)
    for period in grid["period"]:
        assert period + 2 <= 200
        for strength in grid["strength"]:
            out = _apply(df, {"period": period, "strength": strength})
            assert out["aroon_valid"].iloc[: period + 1].sum() == 0
            assert bool(out["aroon_valid"].iloc[period + 1])


@pytest.mark.parametrize("params,needle", [
    ({"period": True}, "period"),
    ({"period": 2.5}, "period must be an integer"),
    ({"period": 1}, "period"),
    ({"period": 199}, "period"),
    ({"period": "25"}, "period"),
    ({"period": None}, "period"),
    ({"strength": False}, "strength"),
    ({"strength": -0.1}, "strength"),
    ({"strength": 100.01}, "strength"),
    ({"strength": float("nan")}, "strength"),
    ({"strength": float("inf")}, "strength"),
    ({"strength": "70"}, "strength must be a number"),
])
def test_invalid_parameters_are_refused_by_every_registry_entry_point(params, needle):
    fut = load_registry("futures")
    with pytest.raises(ValueError, match=needle):
        fut.apply_strategy(NAME, _market(40), params)
    with pytest.raises(ValueError, match=needle):
        fut.validate_params(NAME, params)
    (key, value), = params.items()
    with pytest.raises(ValueError, match=needle):
        fut.validate_param_value(NAME, key, value)


def test_rising_and_falling_series_hold_without_a_crossing():
    for step, up, down in ((0.5, 100.0, 0.0), (-0.5, 0.0, 100.0)):
        out = _apply(_frame(_trend(40, step=step)), {"period": 10, "strength": 70.0})
        valid = out[out["aroon_valid"]]
        assert len(valid) == 40 - 11
        assert (valid["aroon_up"] == up).all() and (valid["aroon_down"] == down).all()
        assert (out["signal"] == 0).all()
        assert set(out["aroon_reason"].iloc[11:]) == {"no_cross"}
        assert set(out["aroon_reason"].iloc[:11]) == {"insufficient_history"}


def test_crossings_fire_once_on_the_crossing_bar_both_sides():
    up = _apply(_frame(_spike_up(_trend(30))), {"period": 10, "strength": 70.0})
    row = up.iloc[-1]
    assert row["signal"] == 1 and row["aroon_reason"] == "entry_long" and row["aroon_reason_code"] == 1
    assert row["aroon_up"] == 100.0 and row["aroon_down"] == 90.0
    assert row["aroon_prev_up"] == 0.0 and row["aroon_prev_down"] == 100.0
    assert row["aroon_high_age"] == 0 and row["aroon_low_age"] == 1
    assert (up["signal"].iloc[:-1] == 0).all()

    down = _apply(_frame(_spike_down(_trend(30, step=0.5))), {"period": 10, "strength": 70.0})
    row = down.iloc[-1]
    assert row["signal"] == -1 and row["aroon_reason_code"] == -1
    assert row["aroon_down"] == 100.0 and row["aroon_up"] == 90.0
    assert (down["signal"].iloc[:-1] == 0).all()

    after = _spike_up(_trend(30))
    after += [(after[-1][3], after[-1][2] + 60.0, after[-1][3] - 0.5, after[-1][3] - 0.2, 1.0)] * 3
    tail = _apply(_frame(after), {"period": 10, "strength": 70.0})
    assert int((tail["signal"] != 0).sum()) == 1


def test_strength_is_strict_and_never_delays_an_event():
    df = _frame(_spike_up(_trend(30)))
    at = _apply(df, {"period": 10, "strength": 100.0})
    assert at["signal"].iloc[-1] == 0 and at["aroon_reason"].iloc[-1] == "strength_not_met"
    assert at["aroon_reason_code"].iloc[-1] == 2
    below = _apply(df, {"period": 10, "strength": 99.999})
    assert below["signal"].iloc[-1] == 1
    zero = _apply(df, {"period": 10, "strength": 0.0})
    assert zero["signal"].iloc[-1] == 1
    later = _frame(_spike_up(_trend(30)) + [(150.0, 200.0, 149.0, 199.0, 1.0)])
    held = _apply(later, {"period": 10, "strength": 100.0})
    assert (held["signal"] == 0).all()


def test_equal_extrema_pick_the_most_recent_bar():
    rows = _trend(30, step=0.0)
    rows = [(r[0], 100.4, 99.6, 100.0, 1.0) for r in rows]
    rows[24] = (100.0, 105.0, 99.6, 100.0, 1.0)
    rows[27] = (100.0, 105.0, 99.6, 100.0, 1.0)
    rows[22] = (100.0, 100.4, 95.0, 100.0, 1.0)
    rows[26] = (100.0, 100.4, 95.0, 100.0, 1.0)
    out = _apply(_frame(rows), {"period": 10, "strength": 70.0})
    last = out.iloc[29]
    assert last["aroon_high_age"] == 2 and last["aroon_low_age"] == 3
    assert last["aroon_up"] == 80.0 and last["aroon_down"] == 70.0


def test_constant_prices_hold_as_a_flat_range():
    out = _apply(_frame([(100.0, 100.0, 100.0, 100.0, 1.0)] * 30), {"period": 10, "strength": 70.0})
    assert (out["signal"] == 0).all()
    assert set(out["aroon_reason"].iloc[10:]) == {"flat_range"}
    assert (out["aroon_reason_code"].iloc[10:] == 14).all()
    assert out["aroon_up"].isna().all()


def test_missing_columns_hold_every_row():
    df = _frame(_trend(30)).drop(columns=["low"])
    out = _apply(df, {"period": 10, "strength": 70.0})
    assert (out["signal"] == 0).all()
    assert set(out["aroon_reason"]) == {"missing_columns:low"}
    assert (out["aroon_reason_code"] == 15).all()


@pytest.mark.parametrize("bad,reason,code", [
    ({"close": float("nan")}, "nonfinite_input", 11),
    ({"high": float("inf")}, "nonfinite_input", 11),
    ({"high": 50.0}, "inconsistent_hlc", 12),
    ({"close": 200.0}, "inconsistent_hlc", 12),
])
def test_a_bad_row_holds_its_whole_support_then_recovers(bad, reason, code):
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


def test_timestamp_defects_hold():
    df = _frame(_trend(40))
    idx = list(df.index)
    idx[25] = idx[24]
    df.index = pd.DatetimeIndex(idx)
    out = _apply(df, {"period": 10, "strength": 70.0})
    assert (out["aroon_reason"].iloc[25:37] == "timestamp_order").all()
    assert bool(out["aroon_valid"].iloc[37])


@pytest.mark.parametrize("period", [2, 198])
def test_boundary_periods_need_exactly_period_plus_two_rows(period):
    need = period + 2
    short = _apply(_market(need - 1), {"period": period, "strength": 0.0})
    assert not short["aroon_valid"].any()
    assert (short["aroon_reason"] == "insufficient_history").all()
    exact = _apply(_market(need), {"period": period, "strength": 0.0})
    assert exact["aroon_valid"].tolist() == [False] * (need - 1) + [True]


@pytest.mark.parametrize("params", [{"period": 25, "strength": 70.0}, {"period": 2, "strength": 0.0},
                                    {"period": 198, "strength": 100.0}, {"period": 14, "strength": 50.0}])
def test_every_prefix_and_bounded_tail_repeat_the_full_series_decision(params):
    df = _market(420)
    full = _apply(df, params)
    need = params["period"] + 2
    cols = ["signal", "aroon_reason_code", "aroon_up", "aroon_down"]
    for end in range(1, len(df) + 1):
        prefix = _apply(df.iloc[:end], params).iloc[-1]
        assert prefix[cols].equals(full.iloc[end - 1][cols]), end
        if end >= need:
            tail = _apply(df.iloc[max(0, end - 200):end], params).iloc[-1]
            assert tail[cols].equals(full.iloc[end - 1][cols]), end


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


def test_invalid_entry_data_keeps_close_and_stop_processing():
    rows = _spike_up(_trend(30))
    entry_px = rows[-1][3]
    rows += [(entry_px, entry_px + 0.5, entry_px - 0.5, float("nan"), 1.0)]
    rows += [(entry_px + 1, entry_px * 1.05, entry_px, entry_px * 1.04, 1.0)] * 3
    df = _frame(rows)
    sig = ensure_atr_indicator(_apply(df, {"period": 10, "strength": 70.0}))
    assert (sig["signal"].iloc[31:] == 0).all()
    assert (sig["aroon_reason"].iloc[31:] == "nonfinite_input").all()
    res = _bt(stop_loss_atr_mult=None, stop_loss_pct=50.0).run(
        sig, strategy_name=NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    assert len(res["trades"]) == 1
    assert res["trades"][0]["exit_reason"].startswith("tiered_tp_pct")

    crash = _spike_up(_trend(30))
    px = crash[-1][3]
    crash += [(px, px + 0.2, px - 0.2, px, 1.0)]
    crash += [(px - 1, px - 0.5, px * 0.5, px * 0.55, 1.0)]
    crash += [(px * 0.55, px * 0.56, px * 0.5, float("nan"), 1.0)] * 3
    sig2 = ensure_atr_indicator(_apply(_frame(crash), {"period": 10, "strength": 70.0}))
    res2 = _bt().run(sig2, strategy_name=NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    assert res2["trades"][0]["exit_reason"] == "sl"


@pytest.mark.parametrize("batched", [False, True])
def test_parity_frame_matches_on_every_compared_bar(batched):
    df = _market(420)
    params = {"period": 14, "strength": 50.0}
    if batched:
        from parity_diff import ParityConfig
        cfg = ParityConfig(strategy_name=NAME, params=params, registry="futures", batched=True,
                           symbol="BTC", timeframe="4h")
        frame = compute_parity_frame(df, cfg=cfg, window=200)
        assert summarize(frame)["batch_mismatches"] == 0
    else:
        frame = compute_parity_frame(df, NAME, params=params, registry="futures", close_refs=TP,
                                     comparison_mode="strict", window=None)
        assert summarize(frame)["close_parity"] == "unverified"
    assert bool(frame["match"].all())
    assert set(frame["live_open_action"]) >= {"long", "short"}


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


@pytest.mark.parametrize("bad", [{"direction": "both"}, {"direction": "long", "params": {}}, "both",
                                 {"direction": "both", "close_strategies": TP, "stop_loss_atr_mult": 1.0,
                                  "trailing_stop_atr_mult": 1.0}])
def test_invalid_incumbent_contracts_are_refused(bad):
    with pytest.raises(ValueError):
        incumbent_contract_kwargs(bad)
