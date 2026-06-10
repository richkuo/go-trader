"""Tests for the backtest-vs-live parity diff tool (#906 D7.4).

The tool's job is to detect strategies whose bar-N decision depends on the
frame they were computed in (full-frame vectorized vs trailing live window).
These tests prove both directions: a window-invariant strategy diffs clean,
and a deliberately frame-dependent strategy is caught.
"""

import os
import sys

import numpy as np
import pandas as pd
import pytest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

import parity_diff
from parity_diff import (
    LIVE_MIN_CANDLES,
    ParityConfig,
    compute_parity_frame,
    config_from_live_config,
    extract_fills,
    main,
    summarize,
)
from registry_loader import load_registry


def _ohlcv(n: int = 300, seed: int = 7) -> pd.DataFrame:
    rng = np.random.default_rng(seed)
    drift = np.linspace(0, 12, n)
    wave = 4.0 * np.sin(np.linspace(0, 14, n))
    noise = rng.normal(0, 0.4, n)
    close = 100.0 + drift + wave + noise
    df = pd.DataFrame({
        "open": close + rng.normal(0, 0.1, n),
        "high": close + np.abs(rng.normal(0, 0.5, n)) + 0.2,
        "low": close - np.abs(rng.normal(0, 0.5, n)) - 0.2,
        "close": close,
        "volume": rng.uniform(900, 1100, n),
    }, index=pd.date_range("2024-01-01", periods=n, freq="1h"))
    return df


def test_window_invariant_strategy_diffs_clean():
    """sma_crossover at bar N only needs the trailing slow-period bars, so
    a full window evaluation must equal the full-frame vectorized value on
    every compared bar."""
    df = _ohlcv(260)
    frame = compute_parity_frame(
        df, "sma_crossover",
        params={"fast_period": 10, "slow_period": 30},
        window=120,
    )
    result = summarize(frame)
    assert result["bars_compared"] > 100
    assert result["clean"], (
        f"window-invariant strategy should not diff: "
        f"{frame[~frame['match']].head()}"
    )


def test_frame_dependent_strategy_is_caught():
    """A strategy keyed on the FULL frame's mean (classic silent-parity
    bug: full-series normalization) must produce mismatches — the live
    window's mean differs from the backtest frame's mean."""
    reg = load_registry("spot")

    def full_frame_mean_strategy(df: pd.DataFrame) -> pd.DataFrame:
        out = df.copy()
        out["signal"] = (out["close"] > out["close"].mean()).astype(int)
        return out

    name = "_parity_diff_test_frame_dependent"
    reg.STRATEGY_REGISTRY[name] = {
        "fn": full_frame_mean_strategy,
        "description": "test-only frame-dependent strategy",
        "default_params": {},
    }
    try:
        df = _ohlcv(260)
        frame = compute_parity_frame(df, name, window=60)
        result = summarize(frame)
        assert result["mismatches"] > 0, (
            "frame-dependent strategy must be detected by the parity diff"
        )
        assert "first_mismatch" in result
    finally:
        del reg.STRATEGY_REGISTRY[name]


def test_regime_labels_diff_clean_per_bar():
    """latest_regime on the trailing window must match compute_regime's
    full-frame label on every bar — the per-bar generalization of the
    last-bar parity test in test_backtester_regime.py."""
    df = _ohlcv(220)
    frame = compute_parity_frame(
        df, "sma_crossover",
        params={"fast_period": 10, "slow_period": 30},
        window=120,
        regime_enabled=True,
    )
    assert "bt_regime" in frame.columns and "live_regime" in frame.columns
    regime_mismatch = frame[frame["bt_regime"] != frame["live_regime"]]
    assert regime_mismatch.empty, regime_mismatch.head()


def test_expanding_window_mode():
    """window=None replays live with an ever-growing frame from bar
    LIVE_MIN_CANDLES on; sma_crossover converges once the slow period is
    seeded, so only the comparison start moves."""
    df = _ohlcv(120)
    frame = compute_parity_frame(
        df, "sma_crossover",
        params={"fast_period": 5, "slow_period": 15},
        window=None,
    )
    assert len(frame) == len(df) - (LIVE_MIN_CANDLES - 1)
    assert summarize(frame)["clean"]


def test_stride_thins_comparison():
    df = _ohlcv(200)
    full = compute_parity_frame(
        df, "sma_crossover", params={"fast_period": 5, "slow_period": 15},
        window=60,
    )
    thinned = compute_parity_frame(
        df, "sma_crossover", params={"fast_period": 5, "slow_period": 15},
        window=60, stride=5,
    )
    assert len(thinned) == (len(full) + 4) // 5


def test_window_below_live_minimum_rejected():
    df = _ohlcv(100)
    with pytest.raises(ValueError, match="window must be >="):
        compute_parity_frame(df, "sma_crossover", window=10)


def _trending_ohlcv(n: int = 260, seed: int = 11) -> pd.DataFrame:
    """Strongly trending series so ATR-tiered TPs actually fire."""
    rng = np.random.default_rng(seed)
    close = 100.0 + np.linspace(0, 80, n) + rng.normal(0, 0.5, n)
    return pd.DataFrame({
        "open": close + rng.normal(0, 0.1, n),
        "high": close + np.abs(rng.normal(0, 0.6, n)) + 0.3,
        "low": close - np.abs(rng.normal(0, 0.6, n)) - 0.3,
        "close": close,
        "volume": rng.uniform(900, 1100, n),
    }, index=pd.date_range("2024-01-01", periods=n, freq="1h"))


def test_close_evaluator_parity_clean_and_exercised():
    """A registry close evaluator (tiered_tp_atr) runs through the SAME
    close_registry_loader.evaluate on both sides with a shared position
    context — so it must diff clean, and the test only counts if the
    evaluator actually fired (close_fraction > 0 somewhere)."""
    df = _trending_ohlcv(260)
    frame = compute_parity_frame(
        df, "sma_crossover",
        params={"fast_period": 10, "slow_period": 30},
        window=60,
        close_refs=[{"name": "tiered_tp_atr", "params": {}}],
    )
    result = summarize(frame)
    assert result["bars_compared"] > 100
    assert result["clean"], frame[~frame["match"]].head()
    assert (frame["live_close_fraction"] > 0).any(), (
        "tiered_tp_atr never fired — the close-evaluator path is untested"
    )
    assert (frame["bt_close_fraction"] > 0).any()


def test_composed_signal_with_close_refs_diffs_clean():
    """With close refs the live signal is the composed finalize_decision
    output (0 while positioned, ±1 on close); the bt side must compose
    identically so signal never diffs on composition alone."""
    df = _trending_ohlcv(220)
    frame = compute_parity_frame(
        df, "sma_crossover",
        params={"fast_period": 5, "slow_period": 20},
        window=60,
        close_refs=[{"name": "tiered_tp_atr", "params": {}}],
    )
    assert (frame["bt_signal"] == frame["live_signal"]).all(), (
        frame[frame["bt_signal"] != frame["live_signal"]].head()
    )


def test_frame_dependent_strategy_caught_with_close_refs_too():
    """The detection guarantee must survive the close-ref code path —
    composition and position simulation may not mask open-signal drift."""
    reg = load_registry("spot")

    def full_frame_mean_strategy(df: pd.DataFrame) -> pd.DataFrame:
        out = df.copy()
        out["signal"] = (out["close"] > out["close"].mean()).astype(int)
        return out

    name = "_parity_diff_test_frame_dependent_close"
    reg.STRATEGY_REGISTRY[name] = {
        "fn": full_frame_mean_strategy,
        "description": "test-only frame-dependent strategy",
        "default_params": {},
    }
    try:
        df = _ohlcv(260)
        frame = compute_parity_frame(
            df, name, window=60,
            close_refs=[{"name": "tiered_tp_atr", "params": {}}],
        )
        assert summarize(frame)["mismatches"] > 0
    finally:
        del reg.STRATEGY_REGISTRY[name]


def test_config_mode_builds_parity_config(tmp_path):
    """--config/--strategy-id must reuse the #641 loader semantics and
    pull symbol/timeframe/registry/regime from the live strategy entry."""
    import json as _json
    cfg_path = tmp_path / "config.json"
    cfg_path.write_text(_json.dumps({
        "config_version": 15,
        "strategies": [{
            "id": "hl-sma-btc",
            "type": "perps",
            "script": "shared_scripts/check_hyperliquid.py",
            "args": ["sma_crossover", "BTC/USDT", "4h"],
            "regime": {"enabled": True, "period": 10, "adx_threshold": 25},
            "open_strategy": {
                "name": "sma_crossover",
                "params": {"fast_period": 5, "slow_period": 20},
            },
            "close_strategy": {"name": "tiered_tp_atr", "params": {}},
        }],
    }))
    cfg = config_from_live_config(str(cfg_path), "hl-sma-btc")
    assert cfg.strategy_name == "sma_crossover"
    assert cfg.params == {"fast_period": 5, "slow_period": 20}
    assert cfg.registry == "futures"
    assert cfg.platform == "hyperliquid"
    assert cfg.symbol == "BTC/USDT" and cfg.timeframe == "4h"
    assert cfg.close_refs == [{"name": "tiered_tp_atr", "params": {}}]
    assert cfg.regime_enabled and cfg.regime_period == 10
    assert cfg.regime_adx_threshold == 25.0

    frame = compute_parity_frame(_trending_ohlcv(200), cfg=cfg, window=60)
    assert summarize(frame)["bars_compared"] > 50


def test_config_mode_unknown_strategy_id_raises(tmp_path):
    import json as _json
    cfg_path = tmp_path / "config.json"
    cfg_path.write_text(_json.dumps({"config_version": 15, "strategies": []}))
    with pytest.raises(ValueError):
        config_from_live_config(str(cfg_path), "missing-id")


def test_extract_fills_reports_entry_and_exit_legs():
    df = _trending_ohlcv(220)
    cfg = ParityConfig(
        strategy_name="sma_crossover",
        params={"fast_period": 5, "slow_period": 20},
    )
    fills = extract_fills(df, cfg)
    assert fills, "trending data + sma crossover must produce fills"
    entries = [f for f in fills if f["event"] == "entry"]
    exits = [f for f in fills if f["event"] == "exit"]
    assert entries and all(f["fill_px"] > 0 and f["fee"] >= 0 for f in entries)
    assert all("pnl" in f for f in exits)


def test_backtest_effective_columns_are_prior_bar_inputs():
    """backtest_effective_* must be the shift(1) inputs the engine reads —
    i.e. the previous row's unshifted bt values (stride=1, no close refs
    so bt_signal is the raw column)."""
    df = _ohlcv(200)
    frame = compute_parity_frame(
        df, "sma_crossover", params={"fast_period": 5, "slow_period": 15},
        window=60,
    )
    assert "backtest_effective_signal" in frame.columns
    got = frame["backtest_effective_signal"].iloc[1:].tolist()
    want = frame["bt_signal"].iloc[:-1].tolist()
    assert got == want

def _live_config_json(stype: str = "perps") -> dict:
    return {
        "config_version": 15,
        "strategies": [{
            "id": "test-strat",
            "type": stype,
            "script": "shared_scripts/check_hyperliquid.py",
            "args": ["sma_crossover", "BTC/USDT", "1h"],
            "open_strategy": {
                "name": "sma_crossover",
                "params": {"fast_period": 5, "slow_period": 20},
            },
        }],
    }


@pytest.mark.parametrize("stype,want_platform", [
    ("perps", "hyperliquid"),
    ("manual", "hyperliquid"),
    ("futures", "binanceus"),
    ("spot", "binanceus"),
])
def test_config_mode_platform_autodetect(tmp_path, stype, want_platform):
    """An unset --platform (empty string, as main passes it) must let the
    strategy type drive the fee platform — perps/manual map to hyperliquid,
    everything else to binanceus."""
    import json as _json
    cfg_path = tmp_path / "config.json"
    cfg_path.write_text(_json.dumps(_live_config_json(stype)))
    cfg = config_from_live_config(str(cfg_path), "test-strat", platform="")
    assert cfg.platform == want_platform


def test_config_mode_explicit_platform_overrides_autodetect(tmp_path):
    import json as _json
    cfg_path = tmp_path / "config.json"
    cfg_path.write_text(_json.dumps(_live_config_json("spot")))
    cfg = config_from_live_config(str(cfg_path), "test-strat",
                                  platform="hyperliquid")
    assert cfg.platform == "hyperliquid"


def test_main_config_mode_fills_use_autodetected_platform(tmp_path, monkeypatch):
    """Regression: the CLI's --platform default must not short-circuit the
    --config auto-detect — an HL perps config run exactly as the CLI runs it
    must reach extract_fills with platform=hyperliquid, not binanceus."""
    import json as _json
    import data_fetcher
    cfg_path = tmp_path / "config.json"
    cfg_path.write_text(_json.dumps(_live_config_json("perps")))
    monkeypatch.setattr(data_fetcher, "load_cached_data",
                        lambda *a, **k: _trending_ohlcv(200))
    captured = {}

    def spy_extract_fills(df, cfg):
        captured["platform"] = cfg.platform
        return []

    monkeypatch.setattr(parity_diff, "extract_fills", spy_extract_fills)
    rc = main(["--config", str(cfg_path), "--strategy-id", "test-strat",
               "--fills", "--window", "60"])
    assert rc in (0, 1)
    assert captured["platform"] == "hyperliquid"


def test_main_zero_bars_compared_is_data_error(monkeypatch):
    """A run whose data is shorter than the trailing window compares zero
    bars — that is a data error (exit 2), never a CLEAN pass (exit 0)."""
    import data_fetcher
    monkeypatch.setattr(data_fetcher, "load_cached_data",
                        lambda *a, **k: _ohlcv(40))
    rc = main(["--strategy", "sma_crossover", "--window", "200"])
    assert rc == 2


def test_out_of_contract_signal_rejected_loudly():
    """A strategy emitting out-of-contract signals (0.5) must NOT diff
    clean: the engine raises on non-integral signals while the live check
    script truncates to 0 — a real divergence. The tool mirrors the
    engine's strict {-1, 0, 1} rejection on both paths so the class is
    surfaced, never normalized into a false CLEAN."""
    reg = load_registry("spot")

    def fractional_signal_strategy(df: pd.DataFrame) -> pd.DataFrame:
        out = df.copy()
        sma = out["close"].rolling(10).mean()
        out["signal"] = np.where(out["close"] > sma, 0.5, -0.5)
        return out

    name = "_parity_diff_test_fractional_signal"
    reg.STRATEGY_REGISTRY[name] = {
        "fn": fractional_signal_strategy,
        "description": "test-only fractional-signal strategy",
        "default_params": {},
    }
    try:
        df = _ohlcv(200)
        with pytest.raises(ValueError, match="signal must be in"):
            compute_parity_frame(df, name, window=60)
    finally:
        del reg.STRATEGY_REGISTRY[name]


def test_normalize_signal_contract():
    """In-contract values collapse identically on both paths; NaN → 0;
    anything else (fractional, out-of-domain integer) raises."""
    assert parity_diff._normalize_signal(np.float64(1.0)) == 1
    assert parity_diff._normalize_signal(-1) == -1
    assert parity_diff._normalize_signal(0.0) == 0
    assert parity_diff._normalize_signal(float("nan")) == 0
    assert parity_diff._normalize_signal(None) == 0
    for bad in (0.5, -0.5, 2, -3):
        with pytest.raises(ValueError, match="signal must be in"):
            parity_diff._normalize_signal(bad)


def test_position_context_entry_regime_is_decision_bar_label():
    """The scaffold must stamp the entry regime from the DECISION bar
    (i-1), mirroring the engine's shifted-regime ``_entry_stamp`` and the
    live label computed alongside the signal — not the fill bar's label."""
    from atr import ensure_atr_indicator
    n = 40
    df = _ohlcv(n)
    bt = pd.DataFrame({
        "signal": [0] * n,
        "open_action": ["none"] * n,
        "close_fraction": [0.0] * n,
    }, index=df.index)
    bt.iloc[9, bt.columns.get_loc("open_action")] = "long"
    atr_full = ensure_atr_indicator(df.copy())["atr"]
    regime_full = pd.Series([f"label{i}" for i in range(n)], index=df.index)
    contexts = parity_diff._simulate_position_contexts(
        bt, df, atr_full, regime_full)
    assert contexts[10] is None  # fill happens AT bar 10; ctx visible after
    assert contexts[11] is not None
    assert contexts[11]["regime"] == "label9", (
        "entry regime must be the decision bar's (9) label, "
        "not the fill bar's (10)"
    )


def test_bt_close_evaluator_uses_engine_dict_shape(monkeypatch):
    """The backtest-side evaluator call must mirror the engine (#747):
    ``regime`` always present (possibly empty) in BOTH dicts and
    ``entry_atr`` always a float — even when regime is disabled and the
    shared context omits the keys."""
    from atr import ensure_atr_indicator
    captured = {}

    def fake_evaluate(name, position, market, params):
        captured["position"] = position
        captured["market"] = market
        return {"close_fraction": 0.0}

    monkeypatch.setattr(parity_diff, "close_evaluate", fake_evaluate)
    df = _trending_ohlcv(80)
    atr_full = ensure_atr_indicator(df.copy())["atr"]
    cfg = ParityConfig(
        strategy_name="sma_crossover",
        close_refs=[{"name": "tiered_tp_atr", "params": {}}],
    )
    ctx = {"side": "long", "avg_cost": 100.0,
           "current_quantity": 1.0, "initial_quantity": 1.0}
    parity_diff._bt_close_evaluator_fraction(cfg, 60, df, atr_full, None, ctx)
    assert captured["market"]["regime"] == ""
    assert captured["position"]["regime"] == ""
    assert isinstance(captured["position"]["entry_atr"], float)
    assert captured["position"]["entry_atr"] == 0.0

def test_non_registry_close_ref_rejected_like_engine():
    """A signal-strategy close ref has no engine path (Backtester rejects
    unknown close names at init), so the tool must fail with the same
    error instead of silently scoring 0 on the bt side while the live
    fallback fully evaluates it — a manufactured mismatch."""
    df = _trending_ohlcv(120)
    with pytest.raises(ValueError, match="Unknown close strategy"):
        compute_parity_frame(
            df, "sma_crossover",
            params={"fast_period": 5, "slow_period": 20},
            window=60,
            close_refs=[{"name": "rsi_oversold", "params": {}}],
        )
    from backtester import Backtester
    with pytest.raises(ValueError, match="Unknown close strategy"):
        Backtester(
            initial_capital=1000.0,
            open_strategy={"name": "sma_crossover", "params": {}},
            close_strategies=[{"name": "rsi_oversold", "params": {}}],
        )


def test_entry_atr_plausibility_guard_matches_engine():
    """The scaffold must apply the engine's _stamp_entry_atr guard: an ATR
    above 50% of the entry price stamps 0.0 (key omitted from the shared
    context) so ATR-requiring close evaluators no-op, matching both the
    engine and Go's stampEntryATRIfOpened."""
    from atr import ensure_atr_indicator
    n = 40
    rng = np.random.default_rng(3)
    close = 100.0 + rng.normal(0, 0.2, n)
    df = pd.DataFrame({
        "open": close,
        "high": close + 90.0,   # huge bar ranges → ATR ≈ 180 > 50% of price
        "low": close - 90.0,
        "close": close,
        "volume": [1000.0] * n,
    }, index=pd.date_range("2024-01-01", periods=n, freq="1h"))
    bt = pd.DataFrame({
        "signal": [0] * n,
        "open_action": ["none"] * n,
        "close_fraction": [0.0] * n,
    }, index=df.index)
    bt.iloc[19, bt.columns.get_loc("open_action")] = "long"  # fill at bar 20
    atr_full = ensure_atr_indicator(df.copy())["atr"]
    assert float(atr_full.iloc[20]) > 0.5 * float(df["close"].iloc[20])
    contexts = parity_diff._simulate_position_contexts(bt, df, atr_full, None)
    assert contexts[21] is not None
    assert "entry_atr" not in contexts[21]

def test_position_context_avg_cost_is_fill_bar_open_with_slippage():
    """The scaffold must open at the fill bar's OPEN adjusted by the
    engine's default slippage (Backtester's effective_price) — not the
    bar's close — so tier triggers ((mark - avg_cost)/entry_atr) see the
    same entry price the engine and live would on large-bodied bars."""
    from atr import ensure_atr_indicator
    n = 40
    df = _ohlcv(n)
    atr_full = ensure_atr_indicator(df.copy())["atr"]
    slip = parity_diff._ENGINE_SLIPPAGE_PCT
    assert slip == pytest.approx(0.0005)

    for action, sign in (("long", 1), ("short", -1)):
        bt = pd.DataFrame({
            "signal": [0] * n,
            "open_action": ["none"] * n,
            "close_fraction": [0.0] * n,
        }, index=df.index)
        bt.iloc[19, bt.columns.get_loc("open_action")] = action  # fill @ 20
        contexts = parity_diff._simulate_position_contexts(
            bt, df, atr_full, None)
        expected = float(df["open"].iloc[20]) * (1 + sign * slip)
        assert contexts[21]["avg_cost"] == pytest.approx(expected), action
        assert contexts[21]["avg_cost"] != pytest.approx(
            float(df["close"].iloc[20]))
