
import os
import sys
from typing import Optional

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


def test_frame_dependent_strategy_is_caught_with_and_without_close_refs(subtests):
    with subtests.test(case="frame_dependent_strategy_is_caught"):
        _assert_frame_dependent_strategy_is_caught()
    with subtests.test(case="frame_dependent_strategy_caught_with_close_refs_too"):
        _assert_frame_dependent_strategy_caught_with_close_refs_too()


def _assert_frame_dependent_strategy_is_caught():
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
    rng = np.random.default_rng(seed)
    close = 100.0 + np.linspace(0, 80, n) + rng.normal(0, 0.5, n)
    return pd.DataFrame({
        "open": close + rng.normal(0, 0.1, n),
        "high": close + np.abs(rng.normal(0, 0.6, n)) + 0.3,
        "low": close - np.abs(rng.normal(0, 0.6, n)) - 0.3,
        "close": close,
        "volume": rng.uniform(900, 1100, n),
    }, index=pd.date_range("2024-01-01", periods=n, freq="1h"))


def test_close_evaluator_parity_clean_and_composed_signal_matches(subtests):
    with subtests.test(case="close_evaluator_parity_clean_and_exercised"):
        _assert_close_evaluator_parity_clean_and_exercised()
    with subtests.test(case="composed_signal_with_close_refs_diffs_clean"):
        _assert_composed_signal_with_close_refs_diffs_clean()


def _assert_close_evaluator_parity_clean_and_exercised():
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


def _assert_composed_signal_with_close_refs_diffs_clean():
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


def test_regime_directional_policy_decision_layer_parity(monkeypatch):
    import regime as regime_mod

    df = _ohlcv(140)
    df["_test_regime"] = "trending_down"

    def fake_compute_regime(frame, period=14, adx_threshold=20.0):
        out = frame.copy()
        out["regime"] = frame["_test_regime"].values
        out["regime_ready"] = True
        out["regime_bars"] = 29
        out["regime_need"] = 29
        out["regime_score"] = 1.0
        out["adx"] = 50.0
        out["plus_di"] = 10.0
        out["minus_di"] = 40.0
        return out

    def fake_prepare_check_regime(window, **kwargs):
        label = str(window["_test_regime"].iloc[-1])
        snap = {"regime": label, "score": 1.0, "metrics": {}, "ready": True, "bars": 29, "need": 29}
        return label, label, snap

    monkeypatch.setattr(regime_mod, "compute_regime", fake_compute_regime)
    monkeypatch.setattr(parity_diff, "prepare_check_regime", fake_prepare_check_regime)

    reg = load_registry("futures")
    name = "_parity_regime_directional_always_buy"

    def always_buy(frame: pd.DataFrame) -> pd.DataFrame:
        out = frame.copy()
        out["signal"] = 1
        return out

    reg.STRATEGY_REGISTRY[name] = {
        "fn": always_buy,
        "description": "test-only always buy",
        "default_params": {},
    }
    try:
        cfg = ParityConfig(
            strategy_name=name,
            registry="futures",
            close_refs=[{"name": "tiered_tp_pct", "params": {"tp_tiers": [
                {"profit_pct": 0.9, "close_fraction": 1.0},
            ]}}],
            regime_enabled=True,
            direction="long",
            invert_signal=False,
            regime_directional_policy={
                "trend_regime": {
                    "trending_up": {"direction": "long", "invert_signal": False},
                    "trending_down": {"direction": "short", "invert_signal": True},
                    "ranging": {"direction": "long", "invert_signal": False},
                },
            },
        )
        frame = compute_parity_frame(df, cfg=cfg, window=60)
        assert summarize(frame)["clean"], frame[~frame["match"]].head()
        assert (frame["bt_open_action"] == "short").any()
        assert (frame["live_open_action"] == "short").any()
    finally:
        del reg.STRATEGY_REGISTRY[name]


def _assert_frame_dependent_strategy_caught_with_close_refs_too():
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


def test_config_mode_builds_parity_config(tmp_path, subtests):
    with subtests.test(case="config_mode_builds_parity_config"):
        _assert_config_mode_builds_parity_config(tmp_path)
    with subtests.test(case="config_mode_unknown_strategy_id_raises"):
        _assert_config_mode_unknown_strategy_id_raises(tmp_path)
    with subtests.test(case="config_mode_injects_user_close_defaults"):
        _assert_config_mode_injects_user_close_defaults(tmp_path)


def _assert_config_mode_builds_parity_config(tmp_path):
    import json as _json
    cfg_path = tmp_path / "config.json"
    cfg_path.write_text(_json.dumps({
        "config_version": 15,
        "regime": {"enabled": True, "period": 10, "adx_threshold": 25},
        "strategies": [{
            "id": "hl-sma-btc",
            "type": "perps",
            "script": "shared_scripts/check_hyperliquid.py",
            "args": ["sma_crossover", "BTC/USDT", "4h"],
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


def _assert_config_mode_unknown_strategy_id_raises(tmp_path):
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
    df = _ohlcv(200)
    frame = compute_parity_frame(
        df, "sma_crossover", params={"fast_period": 5, "slow_period": 15},
        window=60,
    )
    assert "backtest_effective_signal" in frame.columns
    got = frame["backtest_effective_signal"].iloc[1:].tolist()
    want = frame["bt_signal"].iloc[:-1].tolist()
    assert got == want

def _live_config_json(stype: str = "perps", sid: str = "test-strat",
                      platform: Optional[str] = None, **extra) -> dict:
    sc = {
        "id": sid,
        "type": stype,
        "script": "shared_scripts/check_hyperliquid.py",
        "args": ["sma_crossover", "BTC/USDT", "1h"],
        "open_strategy": {
            "name": "sma_crossover",
            "params": {"fast_period": 5, "slow_period": 20},
        },
    }
    if platform:
        sc["platform"] = platform
    sc.update(extra)
    return {"config_version": 15, "strategies": [sc]}


def _write_live_config(tmp_path, stype="perps", sid="test-strat", platform=None, **extra):
    import json as _json
    cfg_path = tmp_path / f"{sid}-{stype}-{platform}.json"
    cfg_path.write_text(_json.dumps(_live_config_json(stype, sid, platform, **extra)))
    return str(cfg_path)


_CONFIG_AUTOMATIC_PLATFORM_CASES = [
    ("test-strat", "perps", None, "binanceus"),
    ("test-strat", "spot", None, "binanceus"),
    ("test-strat", "perps", "hyperliquid", "hyperliquid"),
    ("test-strat", "manual", "hyperliquid", "hyperliquid"),
    ("hl-strat", "perps", None, "hyperliquid"),
    ("rh-strat", "spot", None, "robinhood"),
    ("hl-strat", "spot", "robinhood", "robinhood"),
    ("okx-strat", "perps", None, "okx-perps"),
    ("okx-strat", "spot", None, "okx"),
    ("luno-strat", "spot", None, "luno"),
]


def test_config_mode_platform_autodetect_and_explicit_override(tmp_path, capsys, subtests):
    for sid, stype, platform, want_platform in _CONFIG_AUTOMATIC_PLATFORM_CASES:
        with subtests.test(sid=sid, stype=stype, platform=platform):
            path = _write_live_config(tmp_path, stype, sid, platform)
            cfg = config_from_live_config(path, sid, platform="")
            assert cfg.platform == want_platform
            assert "platform selection" not in capsys.readouterr().err
    for sid, stype, platform in [("test-strat", "futures", None),
                                 ("test-strat", "manual", None),
                                 ("deribit-strat", "options", None),
                                 ("test-strat", "spot", "kraken")]:
        with subtests.test(sid=sid, stype=stype, platform=platform, refused=True):
            path = _write_live_config(tmp_path, stype, sid, platform)
            with pytest.raises(ValueError, match="explicit --platform"):
                config_from_live_config(path, sid, platform="")
    with subtests.test(case="explicit_platform_overrides_autodetect"):
        _assert_config_mode_explicit_platform_overrides_autodetect(tmp_path, capsys)


def _assert_config_mode_explicit_platform_overrides_autodetect(tmp_path, capsys):
    path = _write_live_config(tmp_path, "spot")
    cfg = config_from_live_config(path, "test-strat", platform="hyperliquid")
    assert cfg.platform == "hyperliquid"
    warnings = [l for l in capsys.readouterr().err.splitlines()
                if l.startswith("[WARN] platform selection")]
    assert len(warnings) == 1
    assert "'binanceus'" in warnings[0] and "'spot'" in warnings[0]
    assert "--platform hyperliquid" in warnings[0]


def test_main_config_mode_fills_use_autodetected_platform(tmp_path, monkeypatch):
    import data_fetcher
    path = _write_live_config(tmp_path, "perps", platform="hyperliquid")
    monkeypatch.setattr(data_fetcher, "load_cached_data",
                        lambda *a, **k: _trending_ohlcv(200))
    captured = {}

    def spy_extract_fills(df, cfg):
        captured["platform"] = cfg.platform
        return []

    monkeypatch.setattr(parity_diff, "extract_fills", spy_extract_fills)
    rc = main(["--config", path, "--strategy-id", "test-strat",
               "--fills", "--window", "60"])
    assert rc in (0, 1)
    assert captured["platform"] == "hyperliquid"


def _recording_engine(monkeypatch):
    seen = []

    class RecordingBacktester(parity_diff.Backtester):
        def run(self, *a, **k):
            out = super().run(*a, **k)
            seen.append((self, out))
            return out

    monkeypatch.setattr(parity_diff, "Backtester", RecordingBacktester)
    return seen


def _guard_funding(monkeypatch, allow=False):
    import funding_fetcher
    calls = []

    def load(*a, **k):
        calls.append(a)
        if not allow:
            raise AssertionError("unexpected funding history request")
        raise RuntimeError("fixture: funding history unavailable")

    monkeypatch.setattr(funding_fetcher, "load_cached_funding", load)
    return calls


def _assert_parity_engine_costs(df, report, engine, metrics, commission, slippage=0.0005):
    assert engine.commission_pct == commission
    assert engine.slippage_pct == slippage
    trades = metrics["trades"]
    assert trades, "fixture must produce fills"
    entries = [f for f in report["fills"] if f["event"] == "entry"]
    assert [f["fill_px"] for f in entries] == [t["entry_price"] for t in trades]
    for t in trades:
        sign = 1 if t["side"] == "long" else -1
        raw = float(df["open"].loc[pd.Timestamp(t["entry_date"])])
        assert t["entry_price"] == pytest.approx(raw * (1 + sign * slippage), rel=1e-12)
        notional = t["shares"] * t["entry_price"]
        assert t["entry_fee"] in (pytest.approx(notional * commission, abs=2e-6),
                                  pytest.approx(notional * commission / (1 - commission), abs=2e-6))
        if t["exit_reason"]:
            assert t["exit_fee"] == pytest.approx(t["shares"] * t["exit_price"] * commission, abs=2e-6)


_PARITY_ENGINE_FEE_CASES = [
    ("rh-strat", "spot", None, "robinhood", 0.0),
    ("okx-strat", "perps", None, "okx-perps", 0.0005),
    ("okx-strat", "spot", None, "okx", 0.001),
    ("luno-strat", "spot", None, "luno", 0.01),
    ("hl-strat", "perps", None, "hyperliquid", 0.00045),
    ("test-strat", "manual", "hyperliquid", "hyperliquid", 0.00045),
    ("test-strat", "spot", "binanceus", "binanceus", 0.001),
    ("test-strat", "perps", None, "binanceus", 0.001),
]


def test_config_mode_fills_charge_the_resolved_fee_in_the_real_engine(tmp_path, monkeypatch, subtests):
    df = _ohlcv(300)
    for sid, stype, platform, want, commission in _PARITY_ENGINE_FEE_CASES:
        with subtests.test(sid=sid, stype=stype):
            with monkeypatch.context() as mp:
                seen = _recording_engine(mp)
                _guard_funding(mp)
                cfg = config_from_live_config(
                    _write_live_config(tmp_path, stype, sid, platform), sid)
                cfg.funding_mode = "off"
                assert cfg.platform == want
                report = parity_diff.extract_fills_report(df, cfg)
                engine, metrics = seen[-1]
                _assert_parity_engine_costs(df, report, engine, metrics, commission)


def test_config_mode_platform_scope_and_refusals(tmp_path, monkeypatch, capsys, subtests):
    df = _ohlcv(300)
    with subtests.test("automatic okx perps requests no hyperliquid funding"):
        with monkeypatch.context() as mp:
            calls = _guard_funding(mp)
            cfg = config_from_live_config(_write_live_config(tmp_path, "perps", "okx-strat"), "okx-strat")
            assert cfg.funding_mode == "charge"
            parity_diff.extract_fills_report(df, cfg)
            assert calls == []
    with subtests.test("explicit hyperliquid proxy prices hyperliquid funding"):
        with monkeypatch.context() as mp:
            calls = _guard_funding(mp, allow=True)
            cfg = config_from_live_config(_write_live_config(tmp_path, "perps", "okx-strat"),
                                          "okx-strat", platform="hyperliquid")
            assert cfg.stop_kwargs["stop_platform"] == "okx"
            cfg.funding_mode = "partial"
            report = parity_diff.extract_fills_report(df, cfg)
            assert calls
            assert report["funding"]["available"] is False
    with subtests.test("explicit okx-perps on okx perps is silent; okx warns"):
        path = _write_live_config(tmp_path, "perps", "okx-strat")
        capsys.readouterr()
        assert config_from_live_config(path, "okx-strat", platform="okx-perps").platform == "okx-perps"
        assert "platform selection" not in capsys.readouterr().err
        assert config_from_live_config(path, "okx-strat", platform="okx").platform == "okx"
        err = capsys.readouterr().err
        assert err.count("[WARN] platform selection") == 1 and "'okx-perps'" in err
    with subtests.test("refused automatic selection warns even for an equal explicit key"):
        path = _write_live_config(tmp_path, "manual")
        capsys.readouterr()
        assert config_from_live_config(path, "test-strat", platform="binanceus").platform == "binanceus"
        err = capsys.readouterr().err
        assert err.count("[WARN] platform selection") == 1 and "'manual'" in err
    with subtests.test("config validation still refuses an explicit proxy"):
        path = _write_live_config(tmp_path, "spot", "rh-hedge",
                                  hedge={"enabled": True, "symbol": "ETH"})
        with pytest.raises(ValueError, match="hedge"):
            config_from_live_config(path, "rh-hedge", platform="robinhood")
    for sid, stype in [("deribit-strat", "options"), ("test-strat", "futures")]:
        with subtests.test("main exits 2 on refused automatic selection", stype=stype):
            with monkeypatch.context() as mp:
                import data_fetcher
                mp.setattr(data_fetcher, "load_cached_data",
                           lambda *a, **k: pytest.fail("data access before the refusal"))
                rc = main(["--config", _write_live_config(tmp_path, stype, sid),
                           "--strategy-id", sid])
                assert rc == 2
                err = capsys.readouterr().err
                assert f"type='{stype}'" in err and "explicit --platform" in err
    with subtests.test("unknown explicit platform fails during argument parsing"):
        with pytest.raises(SystemExit) as exc:
            main(["--config", _write_live_config(tmp_path, "spot", "rh-strat"),
                  "--strategy-id", "rh-strat", "--platform", "kraken"])
        assert exc.value.code == 2


def test_main_zero_bars_compared_is_data_error(monkeypatch):
    import data_fetcher
    monkeypatch.setattr(data_fetcher, "load_cached_data",
                        lambda *a, **k: _ohlcv(40))
    rc = main(["--strategy", "sma_crossover", "--window", "200"])
    assert rc == 2


def test_out_of_contract_signal_rejected_loudly(subtests):
    with subtests.test(case="out_of_contract_signal_rejected_loudly"):
        _assert_out_of_contract_signal_rejected_loudly()
    with subtests.test(case="normalize_signal_contract"):
        _assert_normalize_signal_contract()


def _assert_out_of_contract_signal_rejected_loudly():
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


def _assert_normalize_signal_contract():
    assert parity_diff._normalize_signal(np.float64(1.0)) == 1
    assert parity_diff._normalize_signal(-1) == -1
    assert parity_diff._normalize_signal(0.0) == 0
    assert parity_diff._normalize_signal(float("nan")) == 0
    assert parity_diff._normalize_signal(None) == 0
    for bad in (0.5, -0.5, 2, -3):
        with pytest.raises(ValueError, match="signal must be in"):
            parity_diff._normalize_signal(bad)


def test_position_context_matches_the_engine(subtests):
    with subtests.test(case="position_context_entry_regime_is_decision_bar_label"):
        _assert_position_context_entry_regime_is_decision_bar_label()
    with subtests.test(case="entry_atr_plausibility_guard_matches_engine"):
        _assert_entry_atr_plausibility_guard_matches_engine()
    with subtests.test(case="position_context_avg_cost_is_fill_bar_open_with_slippage"):
        _assert_position_context_avg_cost_is_fill_bar_open_with_slippage()
    with subtests.test(case="registry_close_advances_quantity_ladder"):
        _assert_registry_close_advances_quantity_ladder()


def _assert_position_context_entry_regime_is_decision_bar_label():
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
    contexts, _ = parity_diff._simulate_position_contexts(
        bt, df, atr_full, regime_full)
    assert contexts[9] is None
    assert contexts[10] is not None
    assert contexts[10]["regime"] == "label9", (
        "entry regime must be the decision bar's (9) label, "
        "not the fill bar's (10)"
    )


def test_bt_close_evaluator_uses_engine_dict_shape(monkeypatch):
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


def _assert_entry_atr_plausibility_guard_matches_engine():
    from atr import ensure_atr_indicator
    n = 40
    rng = np.random.default_rng(3)
    close = 100.0 + rng.normal(0, 0.2, n)
    df = pd.DataFrame({
        "open": close,
        "high": close + 90.0,
        "low": close - 90.0,
        "close": close,
        "volume": [1000.0] * n,
    }, index=pd.date_range("2024-01-01", periods=n, freq="1h"))
    bt = pd.DataFrame({
        "signal": [0] * n,
        "open_action": ["none"] * n,
        "close_fraction": [0.0] * n,
    }, index=df.index)
    bt.iloc[19, bt.columns.get_loc("open_action")] = "long"
    atr_full = ensure_atr_indicator(df.copy())["atr"]
    assert float(atr_full.iloc[20]) > 0.5 * float(df["close"].iloc[20])
    contexts, _ = parity_diff._simulate_position_contexts(
        bt, df, atr_full, None)
    assert contexts[20] is not None
    assert "entry_atr" not in contexts[20]

def _assert_position_context_avg_cost_is_fill_bar_open_with_slippage():
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
        bt.iloc[19, bt.columns.get_loc("open_action")] = action
        contexts, _ = parity_diff._simulate_position_contexts(
            bt, df, atr_full, None)
        expected = float(df["open"].iloc[20]) * (1 + sign * slip)
        assert contexts[20]["avg_cost"] == pytest.approx(expected), action
        assert contexts[20]["avg_cost"] != pytest.approx(
            float(df["close"].iloc[20]))

def _assert_registry_close_advances_quantity_ladder():
    from atr import ensure_atr_indicator
    n = 60
    close = np.array([100.0] * 30 + [104.0] * 10 + [108.0] * 20)
    df = pd.DataFrame({
        "open": close,
        "high": close + 1.0,
        "low": close - 1.0,
        "close": close,
        "volume": [1000.0] * n,
    }, index=pd.date_range("2024-01-01", periods=n, freq="1h"))
    bt = pd.DataFrame({
        "signal": [0] * n,
        "open_action": ["none"] * n,
        "close_fraction": [0.0] * n,
    }, index=df.index)
    bt.iloc[19, bt.columns.get_loc("open_action")] = "long"
    atr_full = ensure_atr_indicator(df.copy())["atr"]
    cfg = ParityConfig(
        strategy_name="sma_crossover",
        close_refs=[{"name": "tiered_tp_atr", "params": {}}],
    )
    contexts, fracs = parity_diff._simulate_position_contexts(
        bt, df, atr_full, None, cfg)
    assert fracs[30] == pytest.approx(0.4)
    assert contexts[31]["current_quantity"] == pytest.approx(0.6)
    assert fracs[31] == pytest.approx(0.0)
    assert fracs[40] == pytest.approx(2.0 / 3.0)
    assert contexts[41]["current_quantity"] == pytest.approx(0.2)
    assert fracs[41] == pytest.approx(0.0)


def _assert_config_mode_injects_user_close_defaults(tmp_path):
    import json as _json
    cfg_path = tmp_path / "config.json"
    ladder = [
        {"atr_multiple": 1.0, "close_fraction": 0.5},
        {"atr_multiple": 2.0, "close_fraction": 1.0},
    ]
    cfg_path.write_text(_json.dumps({
        "config_version": 16,
        "user_defaults": {"close": {"tiered_tp_atr": {"tp_tiers": ladder}}},
        "strategies": [{
            "id": "hl-sma-btc",
            "type": "perps",
            "script": "shared_scripts/check_hyperliquid.py",
            "args": ["sma_crossover", "BTC/USDT", "4h"],
            "open_strategy": {"name": "sma_crossover", "params": {}},
            "close_strategy": {"name": "tiered_tp_atr",
                               "params": {"use_defaults": True}},
        }],
    }))
    cfg = config_from_live_config(str(cfg_path), "hl-sma-btc")
    assert cfg.close_refs[0]["params"].get("tp_tiers") == ladder


def _batched_cfg(**overrides) -> ParityConfig:
    cfg = ParityConfig(
        strategy_name="breakout",
        registry="futures",
        platform="hyperliquid",
        symbol="BTC",
        timeframe="1h",
        batched=True,
    )
    for key, value in overrides.items():
        setattr(cfg, key, value)
    return cfg


def test_batched_dimension_zero_diff_off_by_default_and_catches_divergence(monkeypatch, subtests):
    with subtests.test(case="batched_dimension_reports_zero_diff"):
        _assert_batched_dimension_reports_zero_diff()
    with subtests.test(case="batched_dimension_zero_diff_with_close_refs_and_regime"):
        _assert_batched_dimension_zero_diff_with_close_refs_and_regime()
    with subtests.test(case="batched_dimension_is_off_by_default"):
        _assert_batched_dimension_is_off_by_default()
    with subtests.test(case="batched_dimension_catches_a_divergent_slot"):
        _assert_batched_dimension_catches_a_divergent_slot(monkeypatch)


def _assert_batched_dimension_reports_zero_diff():
    frame = compute_parity_frame(_ohlcv(140), cfg=_batched_cfg(), window=60, stride=5)
    result = summarize(frame)
    assert result["bars_compared"] > 0
    assert result["batch_mismatches"] == 0, frame[
        frame["solo_signal"] != frame["batch_signal"]].head()
    assert result["batch_clean"]
    for column in ("solo_signal", "batch_signal", "solo_open_action",
                   "batch_open_action", "solo_close_fraction", "batch_close_fraction"):
        assert column in frame.columns


def _assert_batched_dimension_zero_diff_with_close_refs_and_regime():
    cfg = _batched_cfg(
        close_refs=[{"name": "tiered_tp_pct", "params": {"tp_tiers": [
            {"profit_pct": 0.9, "close_fraction": 1.0},
        ]}}],
        regime_enabled=True,
    )
    frame = compute_parity_frame(_ohlcv(140), cfg=cfg, window=60, stride=5)
    result = summarize(frame)
    assert result["bars_compared"] > 0
    assert result["batch_mismatches"] == 0, frame[~frame["match"]].head()


def test_batched_slot_reaches_the_close_evaluators_and_params():
    from parity_diff import _hl_batch_slot, _load_hl_batch_module
    cfg = _batched_cfg(
        params={"lookback": 11},
        close_refs=[{"name": "tiered_tp_pct", "params": {"tp_tiers": [
            {"profit_pct": 0.5, "close_fraction": 1.0},
        ]}}],
    )
    mod = _load_hl_batch_module()
    slot = _hl_batch_slot(mod, cfg, "batched", "long", {"side": "long"})
    assert slot["open_strategy"] == "breakout"
    assert slot["close_strategies"] == "tiered_tp_pct"
    assert slot["params"] == {"lookback": 11}
    assert slot["close_params_by_name"]["tiered_tp_pct"]["tp_tiers"]


def test_batched_close_fraction_can_be_non_zero():
    from parity_diff import _batched_bar_decisions
    df = _ohlcv(140)
    window = df.iloc[:80]
    mark = float(window["close"].iloc[-1])
    cfg = _batched_cfg(
        close_refs=[{"name": "tiered_tp_pct", "params": {"tp_tiers": [
            {"profit_pct": 0.02, "close_fraction": 1.0},
        ]}}],
    )
    ctx = {
        "side": "long",
        "avg_cost": mark * 0.90,
        "current_quantity": 1.0,
        "initial_quantity": 1.0,
        "entry_atr": 1.0,
    }
    solo, batched = _batched_bar_decisions(
        window, cfg, position_side="long", position_ctx=ctx)
    assert batched["close_fraction"] > 0.0
    assert solo["close_fraction"] == batched["close_fraction"]


def _assert_batched_dimension_is_off_by_default():
    frame = compute_parity_frame(_ohlcv(140), cfg=_batched_cfg(batched=False),
                                 window=60, stride=5)
    for column in ("solo_signal", "batch_signal", "batch_close_fraction"):
        assert column not in frame.columns
    result = summarize(frame)
    assert "batch_mismatches" not in result
    assert "batch_clean" not in result


def _assert_batched_dimension_catches_a_divergent_slot(monkeypatch):
    cfg = _batched_cfg()
    real = parity_diff._batched_bar_decisions

    def diverging(window, config, position_side="", position_ctx=None):
        solo, batched = real(window, config, position_side, position_ctx)
        batched = dict(batched)
        batched["signal"] = solo["signal"] + 1
        return solo, batched

    monkeypatch.setattr(parity_diff, "_batched_bar_decisions", diverging)
    frame = compute_parity_frame(_ohlcv(140), cfg=cfg, window=60, stride=5)
    result = summarize(frame)
    assert result["batch_mismatches"] == result["bars_compared"]
    assert not result["clean"]


def _register_flip(name):
    reg = load_registry("futures")

    def flip(frame: pd.DataFrame) -> pd.DataFrame:
        out = frame.copy()
        enter = pd.Timestamp("2024-01-02 12:00:00")
        flip = pd.Timestamp("2024-01-03 00:00:00")
        out["signal"] = 0
        out.loc[out.index >= enter, "signal"] = 1
        out.loc[out.index >= flip, "signal"] = -1
        return out

    reg.STRATEGY_REGISTRY[name] = {
        "fn": flip,
        "description": "test-only flip",
        "default_params": {},
        "edge_status": None,
    }
    return name


def test_inverted_short_registry_close_and_open_as_close_match():
    name = _register_flip("_parity_invert_flip")
    reg = load_registry("futures")
    try:
        _assert_inverted_short_parity(name)
    finally:
        reg.STRATEGY_REGISTRY.pop(name, None)


def _assert_inverted_short_parity(name):
    df = _ohlcv(80, seed=3)
    registry = ParityConfig(
        strategy_name=name,
        registry="futures",
        platform="hyperliquid",
        direction="short",
        invert_signal=True,
        close_refs=[{"name": "atr_stop", "params": {"atr_mult": 0.05}}],
        open_close_config=True,
    )
    frame = compute_parity_frame(df, cfg=registry, window=30)
    summary = summarize(frame)
    assert summary["clean"], summary
    assert (frame["live_signal"] == -1).any()
    assert ((frame["live_close_fraction"] > 0) & (frame["live_signal"] == 1)).any()

    fallback = ParityConfig(
        strategy_name=name,
        registry="futures",
        platform="hyperliquid",
        direction="short",
        invert_signal=True,
        open_close_config=True,
    )
    fallback_frame = compute_parity_frame(df, cfg=fallback, window=30)
    fallback_summary = summarize(fallback_frame)
    assert fallback_summary["clean"], fallback_summary
    assert (fallback_frame["live_signal"] == -1).any()
    assert ((fallback_frame["live_close_fraction"] > 0) & (fallback_frame["live_signal"] == 1)).any()
    close_rows = fallback_frame.index[fallback_frame["live_close_fraction"] > 0]
    assert len(close_rows) == 1
    after = fallback_frame.loc[close_rows[0] + 1:]
    assert not after.empty
    assert (after["live_close_fraction"] == 0).all()
    assert (after["bt_close_fraction"] == 0).all()
    assert (after["live_signal"] == 0).all()
    assert (after["bt_signal"] == 0).all()


def _eom_cfg(batched):
    return ParityConfig(
        strategy_name="ease_of_movement_breakout",
        params={"eom_period": 14, "breakout_window": 20},
        registry="futures",
        platform="hyperliquid",
        symbol="BTC",
        timeframe="1h",
        close_refs=[{"name": "tiered_tp_atr", "params": {}}],
        direction="both",
        batched=batched,
    )


def test_eom_candidate_window_batch_parity_and_undeclared_cadence_hold(subtests):
    for window in [None, 200]:
        with subtests.test(window=window):
            _assert_eom_candidate_full_prefix_window_and_batch_parity(window)
    with subtests.test(case="without_declared_cadence_holds_in_both_paths"):
        _assert_eom_candidate_without_declared_cadence_holds_in_both_paths()


def _assert_eom_candidate_full_prefix_window_and_batch_parity(window):
    from closed_bar import declare_frame_cadence
    df = declare_frame_cadence(_ohlcv(330, seed=3), "1h", "open")
    frame = compute_parity_frame(df, cfg=_eom_cfg(True), window=window, stride=1)
    result = summarize(frame)
    assert result["bars_compared"] > 100
    assert result["clean"], frame[~frame["match"]].head()
    assert result["close_parity"] != "unknown"
    assert set(frame["live_signal"]) >= {1, -1}
    assert (frame["live_close_fraction"] > 0).any()
    assert result["batch_mismatches"] == 0 and result["batch_clean"]


def _assert_eom_candidate_without_declared_cadence_holds_in_both_paths():
    frame = compute_parity_frame(_ohlcv(260, seed=3), cfg=_eom_cfg(False), window=200, stride=5)
    assert summarize(frame)["clean"]
    assert (frame["live_signal"] == 0).all() and (frame["bt_signal"] == 0).all()
