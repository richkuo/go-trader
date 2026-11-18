import contextlib
import itertools
import os

import numpy as np
import pandas as pd
import pytest

import offline_manifest as om
from atr import ensure_atr_indicator
from backtester import Backtester
from eval_windows import run_leg
from optimizer import DEFAULT_PARAM_RANGES
from registry_loader import load_registry

NAME = "on_balance_volume_divergence"
PARAMS = {"left_span": 2, "right_span": 2, "min_separation": 3, "max_separation": 20,
          "volume_threshold": 0.1, "setup_expiry": 5, "volume_test": True}
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


def _check_every_optimizer_grid_combination_validates_through_the_registry():
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


def _check_largest_declared_support_stays_inside_the_scheduler_candle_limit():
    out = _run(_frame(_long_closes()), left_span=10, right_span=10, max_separation=100, setup_expiry=50)
    assert int(out["obvd_support_bars"].iloc[0]) == 171 <= 200
    assert (out["obvd_reason"] == "insufficient_history").all()


def test_every_grid_combination_validates_and_the_largest_support_fits_the_candle_limit(subtests):
    for label, check in (
        ("every_optimizer_grid_combination_validates_through_the_registry", _check_every_optimizer_grid_combination_validates_through_the_registry),
        ("largest_declared_support_stays_inside_the_scheduler_candle_limit", _check_largest_declared_support_stays_inside_the_scheduler_candle_limit),
    ):
        with subtests.test(case=label):
            check()


def _threshold_is_strict(threshold, expected):
    closes = _long_closes()
    vols = _volumes(closes)
    for i in range(33, 36):
        vols[i] = 4.0
    for i in range(36, 40):
        vols[i] = 1.0
    out = _run(_frame(closes, vols), volume_threshold=threshold)
    assert out["obvd_long_divergence"].iloc[CONFIRM_ROW] == 0.5
    assert _entries(out) == expected


def _check_long_entry_fills_next_bar_open_and_the_take_profit_fills_the_bar_after_its_close():
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


def _check_short_entry_and_no_reversal_while_open():
    df = _mirror(_frame(_long_closes()))
    sig = ensure_atr_indicator(_run(df))
    res = _bt().run(sig, strategy_name=NAME, symbol="BTC/USDT", timeframe="4h", save=False)
    assert [t["side"] for t in res["trades"]] == ["short"]
    assert res["trades"][0]["entry_date"] == str(df.index[ENTRY_ROW + 1])
    assert res["trades"][0]["exit_reason"] == "tiered_tp_pct:0.02"


def test_simulator_fills_next_open_exits_on_take_profit_and_never_reverses(subtests):
    for label, check in (
        ("long_entry_fills_next_bar_open_and_the_take_profit_fills_the_bar_after_its_close", _check_long_entry_fills_next_bar_open_and_the_take_profit_fills_the_bar_after_its_close),
        ("short_entry_and_no_reversal_while_open", _check_short_entry_and_no_reversal_while_open),
    ):
        with subtests.test(case=label):
            check()


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
