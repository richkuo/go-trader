
import importlib.util
import json
import math
import os
import pathlib
import sys

import numpy as np
import pandas as pd
import pytest

_HERE = pathlib.Path(__file__).parent
_ROOT = _HERE.parent
for _p in (str(_HERE), str(_ROOT), str(_ROOT / "shared_tools")):
    if _p not in sys.path:
        sys.path.insert(0, _p)


def _load(name: str, relpath: str):
    spec = importlib.util.spec_from_file_location(name, str(_ROOT / relpath))
    mod = importlib.util.module_from_spec(spec)
    sys.modules[name] = mod
    spec.loader.exec_module(mod)
    return mod


hg = _load("bt_hurst_gate_under_test", "backtest/hurst_gate.py")




def _frame(n: int = 400, seed: int = 1411) -> pd.DataFrame:
    rng = np.random.default_rng(seed)
    close = 100.0 * np.exp(np.cumsum(rng.normal(0, 0.01, n)))
    idx = pd.date_range("2024-01-01", periods=n, freq="1h", tz="UTC")
    return pd.DataFrame(
        {
            "open": close,
            "high": close * 1.005,
            "low": close * 0.995,
            "close": close,
            "volume": 1000.0,
        },
        index=idx,
    )




def test_min_only_hysteresis_sequence():
    gate = hg.HurstGate({"enabled": True, "min": 0.55, "disarm_min": 0.50})
    assert gate.advance(0.40) == hg.HURST_STATE_DISARMED
    assert gate.advance(0.52) == hg.HURST_STATE_DISARMED
    assert gate.advance(0.55) == hg.HURST_STATE_ARMED
    assert gate.advance(0.51) == hg.HURST_STATE_ARMED
    assert gate.advance(0.4999) == hg.HURST_STATE_DISARMED


def test_max_only_hysteresis_sequence():
    gate = hg.HurstGate({"enabled": True, "max": 0.45, "disarm_max": 0.50})
    assert gate.advance(0.40) == hg.HURST_STATE_ARMED
    assert gate.advance(0.48) == hg.HURST_STATE_ARMED
    assert gate.advance(0.51) == hg.HURST_STATE_DISARMED
    assert gate.advance(0.47) == hg.HURST_STATE_DISARMED
    assert gate.advance(0.45) == hg.HURST_STATE_ARMED


def test_unset_disarm_collapses_onto_the_arm_bound():
    gate = hg.HurstGate({"enabled": True, "min": 0.55})
    gate.state = hg.HURST_STATE_ARMED
    assert gate.advance(0.5499) == hg.HURST_STATE_DISARMED
    assert gate.advance(0.55) == hg.HURST_STATE_ARMED


@pytest.mark.parametrize("prior", [hg.HURST_STATE_UNKNOWN, hg.HURST_STATE_ARMED, hg.HURST_STATE_DISARMED])
def test_nan_never_transitions_state(prior):
    gate = hg.HurstGate({"enabled": True, "min": 0.55})
    gate.state = prior
    assert gate.advance(float("nan")) == prior
    assert gate.advance(None) == prior


def test_readings_above_one_stay_coherent():
    momentum = hg.HurstGate({"enabled": True, "min": 0.55})
    assert momentum.advance(2.0033) == hg.HURST_STATE_ARMED
    reversion = hg.HurstGate({"enabled": True, "max": 0.45})
    reversion.state = hg.HURST_STATE_ARMED
    assert reversion.advance(2.0033) == hg.HURST_STATE_DISARMED




def test_disabled_gate_is_completely_inert():
    gate = hg.HurstGate({"enabled": False, "min": 0.9})
    for h in (0.1, 0.9, float("nan")):
        assert gate.step(h, flat=True) == (False, 1.0)


def test_fail_closed_is_flat_only():
    gate = hg.HurstGate({"enabled": True, "min": 0.55, "on_failure": "closed"})
    blocked_flat, _ = gate.step(float("nan"), flat=True)
    assert blocked_flat, "unknown H must hold a FRESH open under fail-closed"
    gate2 = hg.HurstGate({"enabled": True, "min": 0.55, "on_failure": "closed"})
    blocked_open, _ = gate2.step(float("nan"), flat=False)
    assert not blocked_open, "fail-closed must never hold while a position is open"


def test_fail_open_admits_unknown_h():
    gate = hg.HurstGate({"enabled": True, "min": 0.55, "on_failure": "open"})
    for flat in (True, False):
        assert gate.step(float("nan"), flat=flat) == (False, 1.0)


def test_known_disarmed_holds_regardless_of_position():
    gate = hg.HurstGate({"enabled": True, "min": 0.55})
    blocked, _ = gate.step(0.20, flat=False)
    assert blocked




@pytest.mark.parametrize(
    "h,floor,want",
    [
        (0.50, 0.25, 0.25),
        (0.575, 0.25, 0.5),
        (0.65, 0.25, 1.0),
        (0.80, 0.25, 1.0),
        (0.35, 0.25, 1.0),
        (2.0033, 0.25, 1.0),
        (0.50, 0.9, 0.9),
    ],
)
def test_size_multiplier_matches_the_go_formula(h, floor, want):
    gate = hg.HurstGate({"enabled": True, "mode": "size", "size_floor": floor})
    assert gate.size_multiplier(h) == pytest.approx(want)


def test_size_multiplier_never_exceeds_one():
    gate = hg.HurstGate({"enabled": True, "mode": "size"})
    for h in np.linspace(0.0, 3.0, 400):
        assert gate.size_multiplier(float(h)) <= 1.0


def test_size_mode_never_holds_on_a_known_reading():
    gate = hg.HurstGate({"enabled": True, "mode": "size", "on_failure": "closed"})
    blocked, mult = gate.step(0.50, flat=True)
    assert not blocked
    assert mult == pytest.approx(hg.HURST_DEFAULT_SIZE_FLOOR)
    gate2 = hg.HurstGate({"enabled": True, "mode": "size", "on_failure": "closed"})
    assert gate2.step(float("nan"), flat=True)[0]
    gate3 = hg.HurstGate({"enabled": True, "mode": "size", "on_failure": "closed"})
    assert not gate3.step(float("nan"), flat=False)[0]


def test_gate_mode_never_scales():
    gate = hg.HurstGate({"enabled": True, "min": 0.55})
    for h in (0.2, 0.6, float("nan")):
        assert gate.step(h, flat=True)[1] == 1.0




def test_rolling_hurst_warmup_is_nan_and_values_are_rounded():
    df = _frame(n=260)
    series = hg.rolling_hurst(df["close"], 200)
    assert series.iloc[:199].isna().all(), "warm-up bars must be genuinely unknown"
    tail = series.iloc[199:].dropna()
    assert len(tail) > 0
    for v in tail:
        assert math.isfinite(v)
        assert round(v, 4) == v, "must match regime.py's 4-decimal rounding"


def test_rolling_hurst_uses_only_trailing_closes():
    df = _frame(n=260)
    base = hg.rolling_hurst(df["close"], 200)
    mutated = df.copy()
    mutated.iloc[230:, mutated.columns.get_loc("close")] *= 1.5
    after = hg.rolling_hurst(mutated["close"], 200)
    pd.testing.assert_series_equal(base.iloc[:230], after.iloc[:230])


def test_decision_series_lags_one_bar():
    df = _frame(n=260)
    raw = hg.rolling_hurst(df["close"], 200)
    decision = raw.shift(1)
    for i in range(201, len(df)):
        assert decision.iloc[i] == raw.iloc[i - 1] or (
            pd.isna(decision.iloc[i]) and pd.isna(raw.iloc[i - 1])
        )


def test_live_frame_bars_matches_the_go_fetch_depth():
    assert hg.hurst_live_frame_bars(None, 14) == 200
    assert hg.hurst_live_frame_bars({"medium": {"period": 20}}, 14) == 200
    assert hg.hurst_live_frame_bars({"long": {"period": 150}}, 14) == 2 * 150 - 1 + 10
    assert hg.hurst_live_frame_bars({"a": {"period": 30}, "b": {"period": 200}}, 14) == 409




def _parity_frame_hurst(windows_spec, *, period: int = 150, n: int = 420):
    pdiff = _load("bt_parity_diff_under_test", "backtest/parity_diff.py")
    df = _frame(n=n)
    cfg = pdiff.ParityConfig(
        strategy_name="momentum",
        params={},
        registry="spot",
        hurst_gate={"enabled": True, "min": 0.55},
        regime_windows_spec=windows_spec,
    )
    frame = pdiff.compute_parity_frame(df, cfg=cfg, window=200, stride=25)
    return df, frame


def test_parity_diff_reads_hurst_over_the_engines_frame_depth():
    spec = {"long": {"period": 150, "classifier": "composite"}}
    depth = hg.hurst_live_frame_bars(spec, 14)
    assert depth == 309

    df, frame = _parity_frame_hurst(spec)
    expected = hg.rolling_hurst(df["close"], depth).shift(1)
    stale = hg.rolling_hurst(df["close"], 200).shift(1)

    compared = 0
    for _, row in frame.iterrows():
        want = expected.loc[row["ts"]]
        got = row["bt_hurst"]
        if pd.isna(want):
            assert got is None or pd.isna(got), f"expected NaN at {row['ts']}, got {got}"
        else:
            assert got is not None and math.isclose(got, float(want), rel_tol=1e-12)
            compared += 1
    assert compared, "the fixture must produce at least one defined H"

    warm = [ts for ts in frame["ts"] if pd.isna(expected.loc[ts]) and not pd.isna(stale.loc[ts])]
    assert warm, "fixture must contain a bar where the two depths disagree"


def test_parity_diff_single_window_default_stays_at_two_hundred_bars():
    assert hg.hurst_live_frame_bars(None, 14) == 200
    df, frame = _parity_frame_hurst(None)
    expected = hg.rolling_hurst(df["close"], 200).shift(1)
    for _, row in frame.iterrows():
        want = expected.loc[row["ts"]]
        got = row["bt_hurst"]
        if pd.isna(want):
            assert got is None or pd.isna(got)
        else:
            assert got is not None and math.isclose(got, float(want), rel_tol=1e-12)


def test_parity_config_carries_the_resolved_windows_spec_and_hurst_block():
    pdiff = _load("bt_parity_diff_under_test", "backtest/parity_diff.py")
    cfg = pdiff.ParityConfig(
        strategy_name="momentum",
        regime_windows_spec={
            "medium": {"period": 20, "classifier": "composite"},
            "long": {"period": 120, "classifier": "composite"},
        },
    )
    assert hg.hurst_live_frame_bars(cfg.regime_windows_spec, cfg.regime_period) == 249
    assert pdiff.ParityConfig(strategy_name="momentum").regime_windows_spec is None




def test_validation_rejects_bad_vocabulary():
    with pytest.raises(ValueError, match="hurst_gate.mode"):
        hg.validate_hurst_gate_config({"mode": "throttle", "min": 0.55})
    with pytest.raises(ValueError, match="hurst_gate_on_failure"):
        hg.validate_hurst_gate_config({"min": 0.55, "on_failure": "halt"})


def test_validation_enforces_bound_ordering_and_range():
    with pytest.raises(ValueError, match="requires at least one of min/max"):
        hg.validate_hurst_gate_config({})
    with pytest.raises(ValueError, match="must be <"):
        hg.validate_hurst_gate_config({"min": 0.7, "max": 0.3})
    with pytest.raises(ValueError, match="disarm_min requires"):
        hg.validate_hurst_gate_config({"disarm_min": 0.4})
    with pytest.raises(ValueError, match="disarm_max requires"):
        hg.validate_hurst_gate_config({"disarm_max": 0.6})
    with pytest.raises(ValueError, match="must be <= hurst_gate.min"):
        hg.validate_hurst_gate_config({"min": 0.5, "disarm_min": 0.6})
    with pytest.raises(ValueError, match="must be >= hurst_gate.max"):
        hg.validate_hurst_gate_config({"max": 0.5, "disarm_max": 0.4})
    for bad in ({"min": 0.0}, {"min": 1.0}, {"max": 0.0}, {"max": 1.0}):
        with pytest.raises(ValueError, match=r"\(0, 1\) exclusive"):
            hg.validate_hurst_gate_config(bad)
    hg.validate_hurst_gate_config({"min": 0.55, "disarm_min": 0.55})


def test_validation_enforces_mode_scoping():
    with pytest.raises(ValueError, match="size_floor only applies"):
        hg.validate_hurst_gate_config({"min": 0.55, "size_floor": 0.5})
    with pytest.raises(ValueError, match="size_floor must be in"):
        hg.validate_hurst_gate_config({"mode": "size", "size_floor": 1.5})
    with pytest.raises(ValueError, match="has no meaning with"):
        hg.validate_hurst_gate_config({"mode": "size", "min": 0.55})
    hg.validate_hurst_gate_config({"mode": "size"})
    hg.validate_hurst_gate_config({"mode": "size", "size_floor": 0.3})


def test_validation_runs_even_when_disabled():
    with pytest.raises(ValueError):
        hg.validate_hurst_gate_config({"enabled": False, "mode": "bogus"})




def _write_cfg(tmp_path, strategy_extra: dict, regime: dict) -> str:
    cfg = {
        "config_version": 17,
        "regime": regime,
        "strategies": [
            dict(
                {
                    "id": "s1",
                    "type": "perps",
                    "platform": "hyperliquid",
                    "script": "check.py",
                    "args": ["momentum", "BTC/USDT", "1h"],
                    "open_strategy": {"name": "momentum", "params": {}},
                },
                **strategy_extra,
            )
        ],
    }
    p = tmp_path / "config.json"
    p.write_text(json.dumps(cfg))
    return str(p)


@pytest.fixture(scope="module")
def load_strategy_config():
    rb = _load("bt_run_backtest_under_test", "backtest/run_backtest.py")
    return rb.load_strategy_config


_COMPOSITE_REGIME = {
    "enabled": True,
    "windows": {"medium": {"classifier": "composite", "period": 20}},
}


def test_backtest_accepts_a_primary_composite_window(tmp_path, load_strategy_config):
    path = _write_cfg(
        tmp_path,
        {"hurst_gate": {"enabled": True, "min": 0.55, "disarm_min": 0.5}},
        _COMPOSITE_REGIME,
    )
    loaded = load_strategy_config(path, "s1")
    assert loaded["hurst_gate"]["enabled"] is True
    assert loaded["hurst_gate"]["window_key"] == "medium"
    assert loaded["hurst_gate"]["on_failure"] == "open"


def test_backtest_resolves_global_on_failure_default(tmp_path, load_strategy_config):
    regime = dict(_COMPOSITE_REGIME, hurst_gate_on_failure="closed")
    path = _write_cfg(tmp_path, {"hurst_gate": {"enabled": True, "min": 0.55}}, regime)
    assert load_strategy_config(path, "s1")["hurst_gate"]["on_failure"] == "closed"
    path = _write_cfg(
        tmp_path, {"hurst_gate": {"enabled": True, "min": 0.55, "on_failure": "open"}}, regime
    )
    assert load_strategy_config(path, "s1")["hurst_gate"]["on_failure"] == "open"


def test_backtest_rejects_non_composite_window(tmp_path, load_strategy_config):
    regime = {"enabled": True, "windows": {"medium": {"classifier": "adx", "period": 20}}}
    path = _write_cfg(tmp_path, {"hurst_gate": {"enabled": True, "min": 0.55}}, regime)
    with pytest.raises(ValueError, match='emitted ONLY by the "composite" classifier'):
        load_strategy_config(path, "s1")


def test_backtest_rejects_regime_disabled(tmp_path, load_strategy_config):
    path = _write_cfg(tmp_path, {"hurst_gate": {"enabled": True, "min": 0.55}}, {"enabled": False})
    with pytest.raises(ValueError, match="regime.enabled=false"):
        load_strategy_config(path, "s1")


def test_backtest_rejects_missing_windows(tmp_path, load_strategy_config):
    path = _write_cfg(tmp_path, {"hurst_gate": {"enabled": True, "min": 0.55}}, {"enabled": True})
    with pytest.raises(ValueError, match="regime.windows is empty"):
        load_strategy_config(path, "s1")


def test_backtest_rejects_non_primary_window(tmp_path, load_strategy_config):
    regime = {
        "enabled": True,
        "windows": {
            "medium": {"classifier": "composite", "period": 20},
            "long": {"classifier": "composite", "period": 60},
        },
    }
    path = _write_cfg(
        tmp_path, {"hurst_gate": {"enabled": True, "min": 0.55, "window_key": "long"}}, regime
    )
    with pytest.raises(ValueError, match="classifies\\s+only the PRIMARY window"):
        load_strategy_config(path, "s1")


def test_backtest_rejects_unknown_window(tmp_path, load_strategy_config):
    path = _write_cfg(
        tmp_path,
        {"hurst_gate": {"enabled": True, "min": 0.55, "window_key": "nope"}},
        _COMPOSITE_REGIME,
    )
    with pytest.raises(ValueError, match="not in\\s+regime.windows"):
        load_strategy_config(path, "s1")


def test_backtest_rejects_invalid_bounds(tmp_path, load_strategy_config):
    path = _write_cfg(
        tmp_path, {"hurst_gate": {"enabled": True, "min": 0.7, "max": 0.3}}, _COMPOSITE_REGIME
    )
    with pytest.raises(ValueError, match="must be <"):
        load_strategy_config(path, "s1")


def test_backtest_passes_through_a_disabled_block(tmp_path, load_strategy_config):
    path = _write_cfg(
        tmp_path,
        {"hurst_gate": {"enabled": False, "min": 0.55}},
        {"enabled": True, "windows": {"medium": {"classifier": "adx", "period": 20}}},
    )
    assert load_strategy_config(path, "s1")["hurst_gate"] is None


def test_backtest_absent_block_is_none(tmp_path, load_strategy_config):
    path = _write_cfg(tmp_path, {}, _COMPOSITE_REGIME)
    assert load_strategy_config(path, "s1")["hurst_gate"] is None




@pytest.fixture(scope="module")
def Backtester():
    mod = _load("bt_backtester_under_test", "backtest/backtester.py")
    return mod.Backtester


def _run(Backtester, df, **kwargs):
    bt = Backtester(
        initial_capital=1000.0,
        platform="binanceus",
        open_strategy={"name": "momentum", "params": {}},
        **kwargs,
    )
    work = df.copy()
    sig = np.zeros(len(work))
    sig[50::40] = 1
    sig[70::40] = -1
    work["signal"] = sig
    return bt.run(work, strategy_name="momentum", symbol="BTC/USDT", timeframe="1h", save=False)


def test_absent_hurst_gate_leaves_the_baseline_byte_identical(Backtester):
    df = _frame(n=400)
    base = _run(Backtester, df)
    off = _run(Backtester, df, hurst_gate=None)
    disabled = _run(Backtester, df, hurst_gate={"enabled": False, "min": 0.55})
    assert base["total_return_pct"] == off["total_return_pct"] == disabled["total_return_pct"]
    assert base["total_trades"] == off["total_trades"] == disabled["total_trades"]


def test_size_mode_shrinks_exposure_relative_to_the_ungated_run(Backtester):
    df = _frame(n=400)
    base = _run(Backtester, df)
    scaled = _run(Backtester, df, hurst_gate={"enabled": True, "mode": "size", "size_floor": 0.25})
    assert scaled["total_trades"] == base["total_trades"], (
        "size mode must never change WHICH trades are taken — only their size"
    )
    base_trades = base.get("trades") or []
    scaled_trades = scaled.get("trades") or []
    assert base_trades and scaled_trades
    for b, s in zip(base_trades, scaled_trades):
        assert abs(float(s["shares"])) <= abs(float(b["shares"])) + 1e-9
    assert any(
        abs(float(s["shares"])) < abs(float(b["shares"])) - 1e-9
        for b, s in zip(base_trades, scaled_trades)
    ), "at least one entry should actually be shrunk"


def test_gate_mode_can_only_remove_entries_never_add_them(Backtester):
    df = _frame(n=400)
    base = _run(Backtester, df)
    gated = _run(
        Backtester,
        df,
        hurst_gate={"enabled": True, "mode": "gate", "min": 0.55, "disarm_min": 0.50},
    )
    assert gated["total_trades"] <= base["total_trades"]


def test_gate_mode_fail_closed_before_warmup_blocks_early_entries(Backtester):
    df = _frame(n=400)
    closed = _run(
        Backtester,
        df,
        hurst_gate={"enabled": True, "min": 0.55, "on_failure": "closed"},
    )
    opened = _run(
        Backtester,
        df,
        hurst_gate={"enabled": True, "min": 0.55, "on_failure": "open"},
    )
    assert closed["total_trades"] <= opened["total_trades"]




def test_hurst_gate_is_in_the_config_cli_allowlist():
    source = (_ROOT / "backtest" / "run_backtest.py").read_text()
    stop_keys_start = source.index("stop_keys = (")
    stop_keys_block = source[stop_keys_start : source.index("live_stop_kwargs = {", stop_keys_start)]
    assert '"hurst_gate"' in stop_keys_block, (
        "hurst_gate must be in run_backtest's stop_keys allowlist, or the "
        "--config CLI path silently drops the entry gate (#1411)"
    )


def test_run_single_backtest_accepts_the_hurst_gate_kwarg():
    import inspect

    rb = _load("bt_run_backtest_sig_check", "backtest/run_backtest.py")
    assert "hurst_gate" in inspect.signature(rb.run_single_backtest).parameters




@pytest.fixture()
def hurst_module():
    previous = sys.modules.get("hurst_gate")
    mod = _load("hurst_gate", "backtest/hurst_gate.py")
    yield mod
    if previous is not None:
        sys.modules["hurst_gate"] = previous
    else:
        sys.modules.pop("hurst_gate", None)


def _pin_hurst(monkeypatch, hurst_module, series_fn):

    def fake_rolling_hurst(close, window):
        return pd.Series(series_fn(len(close)), index=close.index, dtype=float)

    monkeypatch.setattr(hurst_module, "rolling_hurst", fake_rolling_hurst)


def _run_scale_in(Backtester, df, signal, scale_in=None, **kwargs):
    bt = Backtester(
        initial_capital=1000.0,
        platform="binanceus",
        open_strategy={"name": "momentum", "params": {}},
        allow_scale_in=True,
        scale_in=dict(scale_in or {"add_notional_usd": 50.0, "max_adds": 500}),
        **kwargs,
    )
    work = df.copy()
    work["signal"] = signal
    return bt.run(
        work, strategy_name="momentum", symbol="BTC/USDT", timeframe="1h", save=False
    )


def _hold_then_add_signal(n: int, open_bar: int) -> np.ndarray:
    sig = np.zeros(n)
    sig[open_bar:] = 1.0
    return sig


def test_gate_mode_holds_scale_in_adds_once_disarmed(Backtester, hurst_module, monkeypatch):
    df = _frame(n=400)
    disarm_at = 200
    _pin_hurst(
        monkeypatch, hurst_module,
        lambda n: [0.80] * disarm_at + [0.20] * (n - disarm_at),
    )
    sig = _hold_then_add_signal(len(df), open_bar=100)
    gate = {"enabled": True, "mode": "gate", "min": 0.55, "disarm_min": 0.50}

    gated = _run_scale_in(Backtester, df, sig, hurst_gate=gate)
    ungated = _run_scale_in(Backtester, df, sig)

    assert ungated["scale_in_adds"] > 0
    assert gated["scale_in_adds"] > 0, "adds taken while ARMED must still fire"
    assert gated["scale_in_adds"] < ungated["scale_in_adds"], (
        "a disarmed gate must hold scale-in adds, exactly as pausedBlocksSignal "
        "holds a same-side signal on an open position live (#1411)"
    )
    open_fill_bar = 100 + 1
    last_armed_bar = disarm_at
    assert gated["scale_in_adds"] == last_armed_bar - open_fill_bar
    assert gated["scale_in_added_notional_usd"] < ungated["scale_in_added_notional_usd"]


def test_size_mode_scales_scale_in_adds_by_the_multiplier(Backtester, hurst_module, monkeypatch):
    df = _frame(n=400)
    _pin_hurst(monkeypatch, hurst_module, lambda n: [0.53] * n)
    expected_mult = 0.2
    sig = _hold_then_add_signal(len(df), open_bar=100)

    gated = _run_scale_in(
        Backtester, df, sig,
        hurst_gate={"enabled": True, "mode": "size", "size_floor": 0.05},
    )
    ungated = _run_scale_in(Backtester, df, sig)

    assert ungated["scale_in_adds"] > 0
    assert gated["scale_in_adds"] == ungated["scale_in_adds"], (
        "size mode must never change WHICH adds fire — only their size"
    )
    assert gated["scale_in_added_notional_usd"] == pytest.approx(
        expected_mult * ungated["scale_in_added_notional_usd"], rel=1e-7,
    )


def test_open_bar_multiplier_never_leaks_into_later_adds(Backtester, hurst_module, monkeypatch):
    df = _frame(n=400)
    open_bar, recover_at = 100, 200
    sig = np.zeros(len(df))
    sig[open_bar] = 1.0
    sig[recover_at + 5: recover_at + 8] = 1.0
    gate = {"enabled": True, "mode": "size", "size_floor": 0.05}
    scale_cfg = {"max_adds": 10}

    _pin_hurst(
        monkeypatch, hurst_module,
        lambda n: [0.53] * recover_at + [0.80] * (n - recover_at),
    )
    low_open = _run_scale_in(Backtester, df, sig, scale_in=scale_cfg, hurst_gate=gate)

    _pin_hurst(monkeypatch, hurst_module, lambda n: [0.80] * n)
    full_open = _run_scale_in(Backtester, df, sig, scale_in=scale_cfg, hurst_gate=gate)

    assert low_open["scale_in_adds"] == full_open["scale_in_adds"] > 0
    assert low_open["scale_in_added_notional_usd"] == pytest.approx(
        full_open["scale_in_added_notional_usd"], rel=1e-7,
    ), (
        "the open bar's multiplier must not survive into the per-add default "
        "notional — live's defOpenNotional is recomputed ungated every cycle"
    )


def test_armed_gate_leaves_scale_in_adds_byte_identical(Backtester, hurst_module, monkeypatch):
    df = _frame(n=400)
    _pin_hurst(monkeypatch, hurst_module, lambda n: [0.80] * n)
    sig = _hold_then_add_signal(len(df), open_bar=100)

    gated = _run_scale_in(
        Backtester, df, sig,
        hurst_gate={"enabled": True, "mode": "gate", "min": 0.55, "disarm_min": 0.50},
    )
    ungated = _run_scale_in(Backtester, df, sig)

    assert gated["scale_in_adds"] == ungated["scale_in_adds"] > 0
    assert gated["scale_in_added_notional_usd"] == ungated["scale_in_added_notional_usd"]
    assert gated["total_return_pct"] == ungated["total_return_pct"]


def test_absent_hurst_gate_leaves_the_scale_in_baseline_byte_identical(Backtester):
    df = _frame(n=400)
    sig = _hold_then_add_signal(len(df), open_bar=100)
    base = _run_scale_in(Backtester, df, sig)
    off = _run_scale_in(Backtester, df, sig, hurst_gate=None)
    disabled = _run_scale_in(
        Backtester, df, sig, hurst_gate={"enabled": False, "min": 0.55},
    )
    for other in (off, disabled):
        assert other["scale_in_adds"] == base["scale_in_adds"] > 0
        assert other["scale_in_added_notional_usd"] == base["scale_in_added_notional_usd"]
        assert other["total_return_pct"] == base["total_return_pct"]


def test_scale_in_hurst_arms_live_inside_the_add_helper():
    source = (_ROOT / "backtest" / "backtester.py").read_text()
    start = source.index("def _try_scale_in_add(")
    body = source[start: source.index("\n        for i, (idx, row) in enumerate(", start)]
    assert "if hurst_blocked:" in body, (
        "the hold arm must be the add helper's own guard (#1411)"
    )
    assert "add_qty *= hurst_size_mult" in body, (
        "the size arm must scale the DECIDED add quantity (#1411)"
    )
    call_lines = [ln for ln in source.splitlines() if "_try_scale_in_add(i, " in ln]
    assert len(call_lines) == 4, call_lines
    for line in call_lines:
        assert "hurst" not in line, (
            "call sites must stay gate-free so no site can be missed: " + line
        )
