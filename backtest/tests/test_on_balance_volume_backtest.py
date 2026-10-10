import itertools
import math
import os

import numpy as np
import pandas as pd
import pytest

import offline_manifest as om
from atr import ensure_atr_indicator
from backtester import Backtester
from eval_windows import run_leg
from optimizer import DEFAULT_PARAM_RANGES
from parity_diff import ParityConfig, compute_parity_frame, summarize
from registry_loader import load_registry

NAME = "on_balance_volume_divergence"
PARAMS = {"left_span": 2, "right_span": 2, "min_separation": 3, "max_separation": 20,
          "volume_threshold": 0.1, "setup_expiry": 5, "volume_test": True}
SUPPORT = 2 + 20 + 2 + 5 + 1
TP_CLOSE = [{"name": "tiered_tp_pct", "params": {"tp_tiers": [{"profit_pct": 0.02, "close_fraction": 1.0}]}}]
FIXTURE_SEED = 1657
STUDY_MANIFEST = os.path.abspath(os.path.join(
    os.path.dirname(__file__), "..", "candidates", "on_balance_volume_divergence_1657", "study_manifest.json"))

PRE = [100.0] * 23 + [100.0, 97.0, 91.0, 97.0, 100.0, 100.0, 100.0]
BODY = [99.0, 98.0, 96.0, 98.0, 100.0, 101.0, 100.0, 98.0, 96.0, 95.0, 97.0, 98.0, 98.5, 100.0]
TAIL = [100.5, 101.0, 101.5, 102.0, 102.5, 103.0, 104.0, 105.0, 104.0, 103.0]
P_ROW, Q_ROW, CONFIRM_ROW, ENTRY_ROW = 32, 39, 41, 43


def _reg():
    return load_registry("futures")


def _volumes(closes, up=10.0, down=1.0, flat=5.0):
    vols = [flat]
    for prev, cur in zip(closes[:-1], closes[1:]):
        vols.append(up if cur > prev else down if cur < prev else flat)
    return vols


def _frame(closes, vols=None, freq="4h", start="2026-01-01"):
    cl = np.asarray(closes, dtype=float)
    df = pd.DataFrame({"open": cl, "high": cl + 1.0, "low": cl - 1.0, "close": cl,
                       "volume": np.asarray(vols if vols is not None else _volumes(list(cl)), dtype=float)})
    df.index = pd.date_range(start, periods=len(df), freq=freq)
    return df


def _long_closes(body=None, tail=None):
    return PRE + list(body or BODY) + list(tail or TAIL)


def _mirror(df):
    out = df.copy()
    out["open"] = 200.0 - df["open"]
    out["close"] = 200.0 - df["close"]
    out["high"] = 200.0 - df["low"]
    out["low"] = 200.0 - df["high"]
    return out


def _run(df, **overrides):
    return _reg().apply_strategy(NAME, df, dict(PARAMS, **overrides))


def _entries(out):
    return {int(i): int(out["signal"].iloc[i]) for i in np.flatnonzero(out["signal"].to_numpy())}


def _market(n=900, seed=FIXTURE_SEED, length=900):
    rng = np.random.RandomState(seed)
    t = np.arange(length)
    close = np.round(1000 + 60 * np.sin(t / 18.0) + np.cumsum(rng.randn(length) * 5.0), 1)
    span = np.round(np.abs(rng.randn(length)) * 4.0, 1) + 0.5
    open_ = np.concatenate([[close[0]], close[:-1]])
    direction = np.sign(np.diff(close, prepend=close[0]))
    tilt = np.sin(t / 23.0)
    volume = np.round((np.abs(rng.randn(length)) * 20.0 + 5.0) * (1.0 + 0.9 * tilt * direction), 2)
    df = pd.DataFrame({"open": open_, "high": np.maximum(open_, close) + span,
                       "low": np.minimum(open_, close) - span, "close": close, "volume": volume})
    df.index = pd.date_range("2025-01-01", periods=length, freq="4h")
    return df.iloc[:n]


def _bt(**kw):
    base = dict(initial_capital=1000.0, platform="hyperliquid",
                open_strategy={"name": NAME, "params": PARAMS},
                close_strategies=TP_CLOSE, stop_loss_atr_mult=1.0, direction="both",
                comparison_mode="strict")
    base.update(kw)
    return Backtester(**base)


def test_every_optimizer_grid_combination_validates_through_the_registry():
    reg = _reg()
    ranges = DEFAULT_PARAM_RANGES[NAME]
    keys = list(ranges)
    df = _market(260)
    combos = 0
    for values in itertools.product(*(ranges[k] for k in keys)):
        params = dict(zip(keys, values))
        reg.validate_params(NAME, params)
        out = reg.apply_strategy(NAME, df.iloc[-120:], params)
        assert int(out["obvd_support_bars"].iloc[0]) == (
            params["left_span"] + params["max_separation"] + params["right_span"] + params["setup_expiry"] + 1)
        combos += 1
    assert combos == 972


@pytest.mark.parametrize("override,match", [
    ({"left_span": 0}, "left_span"),
    ({"right_span": 11}, "right_span"),
    ({"left_span": 2.5}, "left_span must be an integer"),
    ({"left_span": True}, "left_span must be an integer"),
    ({"min_separation": 9, "max_separation": 8}, "min_separation"),
    ({"max_separation": 101}, "max_separation"),
    ({"setup_expiry": 0}, "setup_expiry"),
    ({"volume_threshold": float("nan")}, "volume_threshold"),
    ({"volume_threshold": float("inf")}, "volume_threshold"),
    ({"volume_threshold": -0.01}, "volume_threshold"),
    ({"volume_threshold": 1.5}, "volume_threshold"),
    ({"volume_threshold": True}, "volume_threshold must be a number"),
    ({"volume_test": 1}, "volume_test must be a boolean"),
    ({"volume_test": "true"}, "volume_test must be a boolean"),
])
def test_invalid_parameters_are_rejected(override, match):
    with pytest.raises(ValueError, match=match):
        _run(_frame(_long_closes()), **override)


def test_largest_declared_support_stays_inside_the_scheduler_candle_limit():
    out = _run(_frame(_long_closes()), left_span=10, right_span=10, max_separation=100, setup_expiry=50)
    assert int(out["obvd_support_bars"].iloc[0]) == 171 <= 200
    assert (out["obvd_reason"] == "insufficient_history").all()


def test_bullish_divergence_arms_after_confirmation_and_enters_once_on_the_strict_reclaim():
    df = _frame(_long_closes())
    out = _run(df)
    assert _entries(out) == {ENTRY_ROW: 1}
    row = out.iloc[ENTRY_ROW]
    assert row["obvd_reason"] == "entry_long"
    assert row["obvd_long_pivot1_price"] == 95.0 and row["obvd_long_pivot2_price"] == 94.0
    assert row["obvd_long_pivot1_ts_ms"] == df.index[P_ROW].value // 1_000_000
    assert row["obvd_long_confirm_ts_ms"] == df.index[CONFIRM_ROW].value // 1_000_000
    assert row["obvd_long_signed_sum"] == 26.0 and row["obvd_long_abs_sum"] == 34.0
    assert row["obvd_long_divergence"] == pytest.approx(26.0 / 34.0, abs=1e-15)
    assert row["obvd_long_reclaim"] == 99.0
    assert row["obvd_long_obv2"] - row["obvd_long_obv1"] == 26.0
    assert out["obvd_reason"].iloc[CONFIRM_ROW] == "setup_armed"
    assert out["obvd_reason"].iloc[CONFIRM_ROW + 1] == "awaiting_reclaim"
    assert (out["obvd_long_state"].iloc[ENTRY_ROW + 1:ENTRY_ROW + 3] == 9).all()
    assert out["obvd_reason"].iloc[CONFIRM_ROW - 1] == "no_setup"


def test_bearish_mirror_enters_short():
    out = _run(_mirror(_frame(_long_closes())))
    assert _entries(out) == {ENTRY_ROW: -1}
    row = out.iloc[ENTRY_ROW]
    assert row["obvd_reason"] == "entry_short"
    assert row["obvd_short_divergence"] == pytest.approx(-26.0 / 34.0, abs=1e-15)
    assert row["obvd_short_reclaim"] == 101.0


@pytest.mark.parametrize("closes,vols", [
    ([100.0] * 80, None),
    (list(np.linspace(100.0, 180.0, 80)), None),
    (list(np.linspace(180.0, 100.0, 80)), None),
])
def test_constant_rising_and_falling_input_never_enters(closes, vols):
    out = _run(_frame(closes, vols))
    assert _entries(out) == {}
    assert set(out["obvd_reason"].iloc[SUPPORT - 1:]) <= {"no_setup", "no_pivot_pair"}


def test_equal_pivot_prices_hold():
    body = list(BODY)
    body[Q_ROW - 31] = 97.0
    body[Q_ROW - 30] = 96.0
    out = _run(_frame(_long_closes(body)))
    assert _entries(out) == {}
    assert out["obvd_reason"].iloc[CONFIRM_ROW] == "no_price_divergence"


def test_equal_indicator_values_hold_and_the_ablation_ignores_them():
    closes = _long_closes()
    vols = _volumes(closes)
    for i in range(33, 36):
        vols[i] = 1.0
    for i in range(36, 40):
        vols[i] = 0.75
    df = _frame(closes, vols)
    out = _run(df)
    assert out["obvd_long_signed_sum"].iloc[CONFIRM_ROW] == 0.0
    assert out["obvd_reason"].iloc[CONFIRM_ROW] == "volume_not_confirmed"
    assert _entries(out) == {}
    ablation = _run(df, volume_test=False)
    assert _entries(ablation) == {ENTRY_ROW: 1}


def test_falling_indicator_blocks_the_long_and_the_ablation_keeps_it():
    df = _frame(_long_closes(), _volumes(_long_closes(), up=1.0, down=10.0))
    out = _run(df)
    assert out["obvd_long_divergence"].iloc[CONFIRM_ROW] < 0
    assert _entries(out) == {}
    assert _entries(_run(df, volume_test=False)) == {ENTRY_ROW: 1}


@pytest.mark.parametrize("threshold,expected", [(0.5, {}), (0.49, {ENTRY_ROW: 1})])
def test_threshold_is_strict(threshold, expected):
    closes = _long_closes()
    vols = _volumes(closes)
    for i in range(33, 36):
        vols[i] = 4.0
    for i in range(36, 40):
        vols[i] = 1.0
    out = _run(_frame(closes, vols), volume_threshold=threshold)
    assert out["obvd_long_divergence"].iloc[CONFIRM_ROW] == 0.5
    assert _entries(out) == expected


def test_zero_volume_span_holds_even_for_the_ablation():
    closes = _long_closes()
    vols = _volumes(closes)
    for i in range(33, 40):
        vols[i] = 0.0
    df = _frame(closes, vols)
    for volume_test in (True, False):
        out = _run(df, volume_test=volume_test)
        assert out["obvd_reason"].iloc[CONFIRM_ROW] == "zero_volume_span"
        assert _entries(out) == {}


def test_nearest_pivot_is_selected_within_inclusive_separation_bounds():
    df = _frame(_long_closes())
    assert _entries(_run(df)) == {ENTRY_ROW: 1}
    assert _entries(_run(df, min_separation=7)) == {ENTRY_ROW: 1}
    assert _entries(_run(df, max_separation=7)) == {ENTRY_ROW: 1}
    farther = _run(df, min_separation=8)
    assert farther["obvd_long_pivot1_price"].iloc[CONFIRM_ROW] == 90.0
    assert farther["obvd_reason"].iloc[CONFIRM_ROW] == "no_price_divergence"
    assert _entries(farther) == {}
    too_close = _run(df, max_separation=6, min_separation=3)
    assert too_close["obvd_reason"].iloc[CONFIRM_ROW] == "no_pivot_pair"
    assert _entries(too_close) == {}


@pytest.mark.parametrize("expiry,expected", [(1, {}), (2, {ENTRY_ROW: 1})])
def test_setup_expiry_is_inclusive(expiry, expected):
    assert _entries(_run(_frame(_long_closes()), setup_expiry=expiry)) == expected


def test_a_newer_pivot_replaces_the_setup():
    tail = [98.0, 96.0, 97.0, 98.0, 100.0, 101.0, 102.0, 103.0, 104.0, 105.0]
    body = BODY[:-2]
    out = _run(_frame(_long_closes(body, tail)))
    replacement_confirm = CONFIRM_ROW + 4
    assert out["obvd_long_pivot2_price"].iloc[replacement_confirm] == 95.0
    assert out["obvd_reason"].iloc[replacement_confirm] == "no_price_divergence"
    assert _entries(out) == {}
    unreplaced = _run(_frame(_long_closes(body, [98.0, 98.0, 98.0, 98.0, 100.0] + tail[5:])))
    assert _entries(unreplaced) == {CONFIRM_ROW + 5: 1}


def test_confirmation_uses_only_closed_right_side_bars():
    df = _frame(_long_closes())
    for cut in range(Q_ROW, CONFIRM_ROW):
        out = _run(df.iloc[:cut + 1])
        assert out["obvd_long_state"].iloc[-1] not in (2, 3)
        assert out["obvd_long_pivot2_price"].iloc[-1] != 94.0
    assert _run(df.iloc[:CONFIRM_ROW + 1])["obvd_long_state"].iloc[-1] == 2


@pytest.mark.parametrize("corrupt,reason", [
    (lambda df, r: df.__setitem__("volume", df["volume"].where(df.index != df.index[r], np.nan)), "invalid_volume"),
    (lambda df, r: df.__setitem__("volume", df["volume"].where(df.index != df.index[r], -1.0)), "invalid_volume"),
    (lambda df, r: df.__setitem__("volume", df["volume"].where(df.index != df.index[r], np.inf)), "invalid_volume"),
    (lambda df, r: df.__setitem__("close", df["close"].where(df.index != df.index[r], np.nan)), "nonfinite_input"),
    (lambda df, r: df.__setitem__("low", df["low"].where(df.index != df.index[r], 0.0)), "invalid_price"),
    (lambda df, r: df.__setitem__("high", df["high"].where(df.index != df.index[r], 10.0)), "inconsistent_ohlc"),
])
def test_invalid_row_inside_the_support_holds_and_decisions_recover(corrupt, reason):
    clean = _frame(_long_closes() + [103.0] * 40)
    bad = clean.copy()
    corrupt(bad, 36)
    out = _run(bad)
    ref = _run(clean)
    assert _entries(out) == {}
    assert out["obvd_reason"].iloc[ENTRY_ROW] == reason
    assert out["obvd_reason"].iloc[36 + SUPPORT - 1] == reason
    after = slice(36 + SUPPORT, len(clean))
    assert (out["signal"].iloc[after].to_numpy() == ref["signal"].iloc[after].to_numpy()).all()
    assert (out["obvd_reason"].iloc[after].to_numpy() == ref["obvd_reason"].iloc[after].to_numpy()).all()


@pytest.mark.parametrize("edit,reason", [("duplicate", "timestamp_order"), ("reverse", "timestamp_order"),
                                         ("gap", "cadence_gap")])
def test_timestamp_defects_hold_until_they_leave_the_support(edit, reason):
    df = _frame(_long_closes() + [103.0] * 40)
    idx = list(df.index)
    if edit == "duplicate":
        idx[36] = idx[35]
    elif edit == "reverse":
        idx[35], idx[36] = idx[36], idx[35]
    else:
        idx = idx[:36] + [t + pd.Timedelta(hours=4) for t in idx[36:]]
    df.index = pd.DatetimeIndex(idx)
    out = _run(df)
    assert _entries(out) == {}
    assert out["obvd_reason"].iloc[ENTRY_ROW] == reason
    assert (out["obvd_reason"].iloc[36 + SUPPORT:] == "no_setup").all()


def test_missing_columns_or_timestamps_hold_without_throwing():
    df = _frame(_long_closes())
    no_volume = _run(df.drop(columns=["volume"]))
    assert (no_volume["obvd_reason"] == "missing_columns").all() and _entries(no_volume) == {}
    no_ts = _run(df.reset_index(drop=True))
    assert (no_ts["obvd_reason"] == "missing_timestamps").all() and _entries(no_ts) == {}
    assert list(no_ts.index) == list(range(len(df)))


def test_shifted_accumulator_origin_and_future_bars_leave_decisions_unchanged():
    df = _market()
    base = _run(df, **{"left_span": 3, "right_span": 3, "min_separation": 5, "max_separation": 40,
                       "setup_expiry": 12})
    support = int(base["obvd_support_bars"].iloc[0])
    prefix = _market(150, seed=FIXTURE_SEED + 1).copy()
    prefix.index = df.index[0] - pd.Timedelta(hours=4) * np.arange(150, 0, -1)
    shifted = _run(pd.concat([prefix, df]), left_span=3, right_span=3, min_separation=5, max_separation=40,
                   setup_expiry=12).iloc[150:]
    cols = ["signal", "obvd_reason", "obvd_long_divergence", "obvd_short_divergence",
            "obvd_long_reclaim", "obvd_short_reclaim"]
    tail = slice(support - 1, None)
    pd.testing.assert_frame_equal(base[cols].iloc[tail].reset_index(drop=True),
                                  shifted[cols].iloc[tail].reset_index(drop=True))
    offsets = (shifted["obv"] - base["obv"]).iloc[tail].dropna()
    assert np.allclose(offsets, offsets.iloc[0], rtol=0, atol=1e-6)
    assert (base["signal"] != 0).sum() > 0
    future = df.copy()
    future.iloc[600:, future.columns.get_loc("volume")] *= 7.0
    changed = _run(future, left_span=3, right_span=3, min_separation=5, max_separation=40, setup_expiry=12)
    pd.testing.assert_frame_equal(base[cols].iloc[:600], changed[cols].iloc[:600])


def test_full_series_every_prefix_and_200_row_tail_agree():
    df = _market(520)
    full = _run(df)
    support = int(full["obvd_support_bars"].iloc[0])
    for end in range(support, len(df) + 1):
        prefix = _run(df.iloc[:end])
        assert prefix["signal"].iloc[-1] == full["signal"].iloc[end - 1]
        assert prefix["obvd_reason"].iloc[-1] == full["obvd_reason"].iloc[end - 1]
        tail = _run(df.iloc[max(0, end - 200):end])
        assert tail["signal"].iloc[-1] == full["signal"].iloc[end - 1]
        for col in ("obvd_long_divergence", "obvd_short_divergence", "obvd_long_reclaim", "obvd_short_reclaim"):
            a, b = tail[col].iloc[-1], full[col].iloc[end - 1]
            assert (math.isnan(a) and math.isnan(b)) or a == b
    assert set(full["signal"]) == {-1, 0, 1}


def test_long_entry_fills_next_bar_open_and_the_take_profit_fills_the_bar_after_its_close():
    df = _frame(_long_closes())
    sig = ensure_atr_indicator(_run(df))
    res = _bt().run(sig, strategy_name=NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    trades = res["trades"]
    assert len(trades) == 1
    t = trades[0]
    assert t["side"] == "long"
    assert t["entry_date"] == str(df.index[ENTRY_ROW + 1])
    assert t["entry_price"] == pytest.approx(df["open"].iloc[ENTRY_ROW + 1] * 1.0005)
    target = t["entry_price"] * 1.02
    hit = next(i for i in range(ENTRY_ROW + 1, len(df)) if df["close"].iloc[i] >= target)
    assert t["exit_reason"] == "tiered_tp_pct:0.02"
    assert t["exit_date"] == str(df.index[hit + 1])


def test_short_entry_and_no_reversal_while_open():
    df = _mirror(_frame(_long_closes()))
    sig = ensure_atr_indicator(_run(df))
    res = _bt().run(sig, strategy_name=NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    assert [t["side"] for t in res["trades"]] == ["short"]
    assert res["trades"][0]["entry_date"] == str(df.index[ENTRY_ROW + 1])
    assert res["trades"][0]["exit_reason"] == "tiered_tp_pct:0.02"


def test_invalid_entry_data_keeps_close_and_stop_processing():
    df = _frame(_long_closes())
    blank = df.copy()
    blank.iloc[ENTRY_ROW + 1:, blank.columns.get_loc("volume")] = np.nan
    sig = ensure_atr_indicator(_run(blank))
    assert (sig["obvd_reason"].iloc[ENTRY_ROW + 1:] == "invalid_volume").all()
    assert (sig["signal"].iloc[ENTRY_ROW + 1:] == 0).all()
    res = _bt().run(sig, strategy_name=NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    assert len(res["trades"]) == 1
    assert res["trades"][0]["exit_reason"] == "tiered_tp_pct:0.02"

    crash = _long_closes(tail=[100.5, 90.0, 80.0, 79.0, 78.0])
    cdf = _frame(crash)
    cdf.iloc[ENTRY_ROW + 2:, cdf.columns.get_loc("volume")] = -1.0
    sig2 = ensure_atr_indicator(_run(cdf))
    assert (sig2["obvd_reason"].iloc[ENTRY_ROW + 2:] == "invalid_volume").all()
    res2 = _bt().run(sig2, strategy_name=NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    assert res2["trades"][0]["exit_reason"] == "sl"


@pytest.mark.parametrize("window", [None, 200])
def test_non_batched_parity_matches_full_series(window):
    cfg = ParityConfig(strategy_name=NAME, params={}, registry="futures", close_refs=TP_CLOSE,
                       direction="both", comparison_mode="strict")
    frame = compute_parity_frame(_market(520), cfg=cfg, window=window)
    assert len(frame) > 0
    assert bool(frame["match"].all())
    assert set(frame.loc[frame["live_open_action"] != "none", "live_open_action"]) == {"long", "short"}


def test_batched_parity_admits_explicit_paper_and_matches_solo():
    cfg = ParityConfig(strategy_name=NAME, params={}, registry="futures", batched=True,
                       symbol="BTC", timeframe="4h")
    frame = compute_parity_frame(_market(240), cfg=cfg, window=200)
    assert len(frame) > 0
    assert bool(frame["match"].all())
    assert (frame["batch_signal"] != 0).sum() > 0


def test_frozen_manifest_frame_full_series_prefix_and_bounded_window_agree():
    manifest = om.load_manifest(STUDY_MANIFEST)
    dataset = om.dataset_by_key(manifest, "BTC 4h")
    frame, _, coverage = om.window_frame(manifest, dataset, "test")
    assert coverage["complete"]
    sub = frame.iloc[:420]
    cfg = ParityConfig(strategy_name=NAME, params={}, registry="futures", close_refs=TP_CLOSE,
                       direction="both", comparison_mode="strict")
    for window in (None, 200):
        parity = compute_parity_frame(sub, cfg=cfg, window=window)
        assert bool(parity["match"].all())
        assert summarize(parity)["clean"]


def test_manifest_leg_books_funding_and_execution_spec():
    manifest = om.load_manifest(STUDY_MANIFEST)
    reg = load_registry("futures")
    leg = run_leg(reg, NAME, dict(reg.STRATEGY_REGISTRY[NAME]["default_params"]), "BTC", "4h", ("", ""),
                  capital=1000.0, close_strategies=TP_CLOSE, direction="both", stop_loss_atr_mult=1.0,
                  manifest_ctx={"manifest": manifest, "window": "train"}, comparison_mode="strict")
    info = leg["manifest"]
    assert info["cost_model"] == "execution_spec"
    assert info["funding_coverage"]["available"] is True
    assert info["candle_coverage"]["complete"] is True
    ex = leg["execution"]
    assert ex["positions"] > 0
    assert ex["funding_pnl_usd"] != 0.0
