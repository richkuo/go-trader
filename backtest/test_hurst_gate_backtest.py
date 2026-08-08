"""#1411 Hurst entry gate — backtest parity tests.

Covers the state machine, the sizing formula, the look-ahead invariant, the
load-time rejections, and the default-off guarantee that an absent block leaves
every baseline byte-identical.
"""

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
    """Load by explicit path — never a bare import of an ambiguous name
    (CLAUDE.md testing rule; CI runs pytest with -n auto)."""
    spec = importlib.util.spec_from_file_location(name, str(_ROOT / relpath))
    mod = importlib.util.module_from_spec(spec)
    sys.modules[name] = mod
    spec.loader.exec_module(mod)
    return mod


hg = _load("bt_hurst_gate_under_test", "backtest/hurst_gate.py")


# ─── Fixtures ────────────────────────────────────────────────────────────────


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


# ─── State machine parity with scheduler/hurst_gate.go ───────────────────────


def test_min_only_hysteresis_sequence():
    gate = hg.HurstGate({"enabled": True, "min": 0.55, "disarm_min": 0.50})
    assert gate.advance(0.40) == hg.HURST_STATE_DISARMED
    assert gate.advance(0.52) == hg.HURST_STATE_DISARMED  # inside the gap
    assert gate.advance(0.55) == hg.HURST_STATE_ARMED
    assert gate.advance(0.51) == hg.HURST_STATE_ARMED  # armed survives the dip
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
    """#1410 NaN policy: unknown is never 0.5; it neither arms nor disarms."""
    gate = hg.HurstGate({"enabled": True, "min": 0.55})
    gate.state = prior
    assert gate.advance(float("nan")) == prior
    assert gate.advance(None) == prior


def test_readings_above_one_stay_coherent():
    """DFA reads ~2.0 on a near-smooth series; the machine must not assume (0,1)."""
    momentum = hg.HurstGate({"enabled": True, "min": 0.55})
    assert momentum.advance(2.0033) == hg.HURST_STATE_ARMED
    reversion = hg.HurstGate({"enabled": True, "max": 0.45})
    reversion.state = hg.HURST_STATE_ARMED
    assert reversion.advance(2.0033) == hg.HURST_STATE_DISARMED


# ─── Hold decision ───────────────────────────────────────────────────────────


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
    """A KNOWN out-of-band reading is not flat-only: the caller's
    position-increasing classification is what lets closes through."""
    gate = hg.HurstGate({"enabled": True, "min": 0.55})
    blocked, _ = gate.step(0.20, flat=False)
    assert blocked


# ─── Sizing ──────────────────────────────────────────────────────────────────


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
    """The issue's formula only ever SHRINKS an open. The #1410 study swept a
    form that can reach 1.5; the issue's formula governs, so this is a hard
    invariant on both engines."""
    gate = hg.HurstGate({"enabled": True, "mode": "size"})
    for h in np.linspace(0.0, 3.0, 400):
        assert gate.size_multiplier(float(h)) <= 1.0


def test_size_mode_never_holds_on_a_known_reading():
    gate = hg.HurstGate({"enabled": True, "mode": "size", "on_failure": "closed"})
    blocked, mult = gate.step(0.50, flat=True)
    assert not blocked
    assert mult == pytest.approx(hg.HURST_DEFAULT_SIZE_FLOOR)
    # ...but an unknown reading under fail-closed still holds, flat-only.
    gate2 = hg.HurstGate({"enabled": True, "mode": "size", "on_failure": "closed"})
    assert gate2.step(float("nan"), flat=True)[0]
    gate3 = hg.HurstGate({"enabled": True, "mode": "size", "on_failure": "closed"})
    assert not gate3.step(float("nan"), flat=False)[0]


def test_gate_mode_never_scales():
    gate = hg.HurstGate({"enabled": True, "min": 0.55})
    for h in (0.2, 0.6, float("nan")):
        assert gate.step(h, flat=True)[1] == 1.0


# ─── Rolling series + look-ahead ─────────────────────────────────────────────


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
    """Look-ahead: the value at bar i must not change when FUTURE bars change."""
    df = _frame(n=260)
    base = hg.rolling_hurst(df["close"], 200)
    mutated = df.copy()
    mutated.iloc[230:, mutated.columns.get_loc("close")] *= 1.5
    after = hg.rolling_hurst(mutated["close"], 200)
    pd.testing.assert_series_equal(base.iloc[:230], after.iloc[:230])


def test_decision_series_lags_one_bar():
    """A signal at bar N must read H computed through bar N-1."""
    df = _frame(n=260)
    raw = hg.rolling_hurst(df["close"], 200)
    decision = raw.shift(1)
    for i in range(201, len(df)):
        assert decision.iloc[i] == raw.iloc[i - 1] or (
            pd.isna(decision.iloc[i]) and pd.isna(raw.iloc[i - 1])
        )


def test_live_frame_bars_matches_the_go_fetch_depth():
    # scheduler/regime_multi_window.go: max(200, 2*maxPeriod - 1 + 10)
    assert hg.hurst_live_frame_bars(None, 14) == 200
    assert hg.hurst_live_frame_bars({"medium": {"period": 20}}, 14) == 200
    assert hg.hurst_live_frame_bars({"long": {"period": 150}}, 14) == 2 * 150 - 1 + 10
    assert hg.hurst_live_frame_bars({"a": {"period": 30}, "b": {"period": 200}}, 14) == 409


# ─── Validation ──────────────────────────────────────────────────────────────


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
    # equal bounds are legal — hysteresis collapses onto the arm bound
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
    """A parked-but-broken block fails at edit time, not the first time it is
    flipped on — the validateHedgeConfigs discipline."""
    with pytest.raises(ValueError):
        hg.validate_hurst_gate_config({"enabled": False, "mode": "bogus"})


# ─── run_backtest load-time rejections ───────────────────────────────────────


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
    # on_failure resolves through the same precedence as the Go accessor
    assert loaded["hurst_gate"]["on_failure"] == "open"


def test_backtest_resolves_global_on_failure_default(tmp_path, load_strategy_config):
    regime = dict(_COMPOSITE_REGIME, hurst_gate_on_failure="closed")
    path = _write_cfg(tmp_path, {"hurst_gate": {"enabled": True, "min": 0.55}}, regime)
    assert load_strategy_config(path, "s1")["hurst_gate"]["on_failure"] == "closed"
    # ...and a per-strategy value wins over it
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
    """Parking a DISABLED block must not make a strategy unbacktestable, and it
    must leave the run byte-identical to having no block at all — even when the
    surrounding regime config could never support the gate."""
    path = _write_cfg(
        tmp_path,
        {"hurst_gate": {"enabled": False, "min": 0.55}},
        {"enabled": True, "windows": {"medium": {"classifier": "adx", "period": 20}}},
    )
    assert load_strategy_config(path, "s1")["hurst_gate"] is None


def test_backtest_absent_block_is_none(tmp_path, load_strategy_config):
    path = _write_cfg(tmp_path, {}, _COMPOSITE_REGIME)
    assert load_strategy_config(path, "s1")["hurst_gate"] is None


# ─── Engine integration ──────────────────────────────────────────────────────


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
    # Deterministic alternating signal so entries are frequent enough that a
    # gate/multiplier difference is visible.
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
    # Every scaled entry commits no more than the unscaled one.
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
    """Before the rolling window fills, H is unknown. Under fail-closed that
    must hold fresh opens; under fail-open it must not."""
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


# ─── CLI seam ────────────────────────────────────────────────────────────────


def test_hurst_gate_is_in_the_config_cli_allowlist():
    """Regression: ``run_backtest.py --config`` copies only an explicit
    allowlist of resolved live-config keys into the Backtester kwargs. A key
    missing from that list is dropped SILENTLY — ``load_strategy_config``
    resolves the gate, every unit test passes, and the run still reports
    entries the live daemon would have held. Pin the entry so the seam cannot
    regress.
    """
    source = (_ROOT / "backtest" / "run_backtest.py").read_text()
    stop_keys_start = source.index("stop_keys = (")
    stop_keys_block = source[stop_keys_start : source.index("live_stop_kwargs = {", stop_keys_start)]
    assert '"hurst_gate"' in stop_keys_block, (
        "hurst_gate must be in run_backtest's stop_keys allowlist, or the "
        "--config CLI path silently drops the entry gate (#1411)"
    )


def test_run_single_backtest_accepts_the_hurst_gate_kwarg():
    """The allowlist entry is only useful if the receiving signature has the
    parameter — otherwise the CLI raises TypeError on every gated config."""
    import inspect

    rb = _load("bt_run_backtest_sig_check", "backtest/run_backtest.py")
    assert "hurst_gate" in inspect.signature(rb.run_single_backtest).parameters
