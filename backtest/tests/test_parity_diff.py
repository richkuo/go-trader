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

from parity_diff import (
    LIVE_MIN_CANDLES,
    ParityConfig,
    compute_parity_frame,
    config_from_live_config,
    extract_fills,
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
