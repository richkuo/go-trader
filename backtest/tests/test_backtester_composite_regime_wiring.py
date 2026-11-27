import sys
import pathlib
import json

import numpy as np
import pandas as pd
import pytest

sys.path.insert(0, str(pathlib.Path(__file__).parent.parent.parent / "shared_tools"))
sys.path.insert(0, str(pathlib.Path(__file__).parent.parent))

import run_backtest
from backtester import Backtester
from regime import (
    ensure_regime_columns,
    VALID_LABELS_COMPOSITE,
    VALID_LABELS_ADX,
    _DEFAULT_COMPOSITE_THRESHOLDS,
)

COMPOSITE_SPEC = {"medium": {"classifier": "composite", "period": 20}}


def _mixed_regime_ohlcv(n: int = 160, seed: int = 1) -> pd.DataFrame:
    idx = pd.date_range("2024-01-01", periods=n, freq="D")
    rng = np.random.default_rng(seed)
    close = np.empty(n)
    half = n // 2
    close[:half] = 100.0 + np.cumsum(rng.normal(0.0, 0.5, half))
    close[half:] = close[half - 1] + np.cumsum(np.abs(rng.normal(1.0, 0.15, n - half)))
    return pd.DataFrame(
        {"open": close, "high": close + 0.6, "low": close - 0.6,
         "close": close, "volume": 1000.0},
        index=idx,
    )


def _ground_truth_labels(df: pd.DataFrame, spec=COMPOSITE_SPEC) -> pd.Series:
    truth = df.copy()
    ensure_regime_columns(truth, windows_spec=spec)
    return truth["regime"]


def _buy_at(df: pd.DataFrame, bar: int) -> pd.DataFrame:
    out = df.copy()
    out["signal"] = 0
    out.iloc[bar, out.columns.get_loc("signal")] = 1
    return out


def test_windows_spec_stamps_composite_substates_and_default_stays_adx(subtests):
    with subtests.test(case="windows_spec_computes_composite_substates_from_ohlcv"):
        _assert_windows_spec_computes_composite_substates_from_ohlcv()
    with subtests.test(case="position_regime_is_composite_substate"):
        _assert_position_regime_is_composite_substate()
    with subtests.test(case="default_path_stays_adx_without_windows_spec"):
        _assert_default_path_stays_adx_without_windows_spec()


def _assert_windows_spec_computes_composite_substates_from_ohlcv():
    labels = set(_ground_truth_labels(_mixed_regime_ohlcv()).unique()) - {""}
    assert labels <= VALID_LABELS_COMPOSITE
    assert labels & VALID_LABELS_ADX == set()
    assert labels, "expected non-empty composite labels"


def _assert_position_regime_is_composite_substate():
    df = _mixed_regime_ohlcv()
    truth = _ground_truth_labels(df)
    entry = 130
    expected = truth.iloc[entry]
    assert expected in VALID_LABELS_COMPOSITE
    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        regime_enabled=True, regime_windows_spec=COMPOSITE_SPEC,
    )
    result = bt.run(_buy_at(df, entry), save=False)
    assert result["total_trades"] == 1
    assert bt._run_position_regime == expected
    assert bt._run_position_regime in VALID_LABELS_COMPOSITE


def _assert_default_path_stays_adx_without_windows_spec():
    df = _mixed_regime_ohlcv()
    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        regime_enabled=True,
    )
    bt.run(_buy_at(df, 130), save=False)
    assert bt._run_position_regime in VALID_LABELS_ADX
    assert bt._run_position_regime not in VALID_LABELS_COMPOSITE


def test_composite_gate_allows_matching_blocks_mismatching():
    df = _mixed_regime_ohlcv()
    truth = _ground_truth_labels(df)
    entry = 130
    label = truth.iloc[entry]
    assert label in VALID_LABELS_COMPOSITE
    other = next(l for l in sorted(VALID_LABELS_COMPOSITE) if l != label)

    bt_ok = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        regime_enabled=True, regime_windows_spec=COMPOSITE_SPEC,
        allowed_regimes=[label],
    )
    assert bt_ok.run(_buy_at(df, entry), save=False)["total_trades"] == 1

    bt_no = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        regime_enabled=True, regime_windows_spec=COMPOSITE_SPEC,
        allowed_regimes=[other],
    )
    assert bt_no.run(_buy_at(df, entry), save=False)["total_trades"] == 0


def test_composite_label_respects_lookahead_shift():
    df = _mixed_regime_ohlcv()
    truth = _ground_truth_labels(df)
    entry = next(
        i for i in range(60, len(df) - 2)
        if truth.iloc[i] != truth.iloc[i + 1]
        and truth.iloc[i] in VALID_LABELS_COMPOSITE
    )
    decision_label = truth.iloc[entry]
    fill_label = truth.iloc[entry + 1]
    assert decision_label != fill_label

    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        regime_enabled=True, regime_windows_spec=COMPOSITE_SPEC,
    )
    bt.run(_buy_at(df, entry), save=False)
    assert bt._run_position_regime == decision_label
    assert bt._run_position_regime != fill_label


def test_resolve_windows_spec(subtests):
    with subtests.test(case="none_when_disabled_or_no_windows"):
        _assert_resolve_windows_spec_none_when_disabled_or_no_windows()
    with subtests.test(case="composite_medium"):
        _assert_resolve_windows_spec_composite_medium()
    with subtests.test(case="adx_inherits_top_level_threshold_and_period"):
        _assert_resolve_windows_spec_adx_inherits_top_level_threshold_and_period()
    with subtests.test(case="keeps_explicit_window_values"):
        _assert_resolve_windows_spec_keeps_explicit_window_values()


def _assert_resolve_windows_spec_none_when_disabled_or_no_windows():
    assert run_backtest._resolve_regime_windows_spec(None) is None
    assert run_backtest._resolve_regime_windows_spec({}) is None
    assert run_backtest._resolve_regime_windows_spec({"enabled": False, "windows": {"medium": 20}}) is None
    assert run_backtest._resolve_regime_windows_spec({"enabled": True, "period": 14}) is None


def _assert_resolve_windows_spec_composite_medium():
    spec = run_backtest._resolve_regime_windows_spec({
        "enabled": True,
        "windows": {"medium": {"classifier": "composite", "period": 30}},
    })
    assert spec is not None
    assert spec["medium"]["classifier"] == "composite"
    assert spec["medium"]["period"] == 30
    assert spec["medium"]["thresholds"] == _DEFAULT_COMPOSITE_THRESHOLDS


def _assert_resolve_windows_spec_adx_inherits_top_level_threshold_and_period():
    spec = run_backtest._resolve_regime_windows_spec({
        "enabled": True,
        "period": 21,
        "adx_threshold": 28.0,
        "windows": {"medium": {"classifier": "adx"}},
    })
    assert spec["medium"]["classifier"] == "adx"
    assert spec["medium"]["period"] == 21
    assert spec["medium"]["adx_threshold"] == 28.0


def _assert_resolve_windows_spec_keeps_explicit_window_values():
    spec = run_backtest._resolve_regime_windows_spec({
        "enabled": True,
        "period": 14,
        "adx_threshold": 20.0,
        "windows": {"short": {"classifier": "adx", "period": 7, "adx_threshold": 30.0}},
    })
    assert spec["short"]["period"] == 7
    assert spec["short"]["adx_threshold"] == 30.0


def _write_config(tmp_path, cfg):
    p = tmp_path / "config.json"
    p.write_text(json.dumps(cfg, indent=2))
    return str(p)


def _composite_config(tmp_path):
    return _write_config(tmp_path, {
        "config_version": 15,
        "regime": {
            "enabled": True,
            "timeframe": " 1D ",
            "period": 14,
            "adx_threshold": 20.0,
            "windows": {"medium": {"classifier": "composite", "period": 30}},
        },
        "strategies": [{
            "id": "hl-temacb-btc",
            "type": "perps",
            "platform": "hyperliquid",
            "open_strategy": {"name": "tema_cross_bd"},
            "close_strategy": {"name": "tiered_tp_atr", "params": {"tp_tiers": [
                {"atr_multiple": 2.0, "close_fraction": 0.5},
                {"atr_multiple": 3.0, "close_fraction": 1.0},
            ]}},
        }],
    })


def _run_main(monkeypatch, argv):
    monkeypatch.setattr(sys, "argv", ["run_backtest.py", *argv])
    return run_backtest.main()


def test_cli_rejects_invalid_regime_wiring(monkeypatch, tmp_path, subtests):
    for i, argv_factory in enumerate([
            lambda tmp_path: [
                "--mode", "compare", "--strategy", "sma_crossover",
                "--regime-windows-spec-json", json.dumps(COMPOSITE_SPEC),
            ],
            lambda tmp_path: [
                "--mode", "single", "--strategy", "sma_crossover",
                "--regime-windows-spec-json", "{not valid json",
            ],
            lambda tmp_path: [
                "--mode", "single", "--strategy", "hl-temacb-btc",
                "--config", _composite_config(tmp_path),
                "--regime-windows-spec-json", json.dumps(COMPOSITE_SPEC),
            ],
            lambda tmp_path: [
                "--mode", "single", "--strategy", "sma_crossover",
                "--regime-enabled", "--allowed-regimes", "ranging_quiet",
            ],
            lambda tmp_path: [
                "--mode", "single", "--strategy", "hl-temacb-btc",
                "--config", _config_with_regime(
                    tmp_path, classifier="composite", allowed_regimes=["ranging"]),
            ],
            lambda tmp_path: [
                "--mode", "single", "--strategy", "hl-temacb-btc",
                "--config", _config_with_regime(
                    tmp_path, classifier="adx", allowed_regimes=["ranging_quiet"]),
            ],
        ]):
        with subtests.test(case=i):
            case_dir = tmp_path / str(i)
            case_dir.mkdir()
            _assert_cli_rejects_invalid_regime_wiring(monkeypatch, case_dir, argv_factory)


def _assert_cli_rejects_invalid_regime_wiring(monkeypatch, tmp_path, argv_factory):
    with pytest.raises(SystemExit):
        _run_main(monkeypatch, argv_factory(tmp_path))


def test_cli_and_config_thread_regime_wiring_to_backtester(monkeypatch, tmp_path, subtests):
    with subtests.test(case="cli_by_name_threads_windows_spec"):
        _assert_cli_by_name_threads_windows_spec_to_backtester(monkeypatch)
    with subtests.test(case="cli_composite_label_reaches_backtester_with_spec"):
        _assert_cli_composite_label_reaches_backtester_with_spec(monkeypatch)
    for allowed_regimes in [["ranging_quiet"], None]:
        with subtests.test(case="config", allowed_regimes=allowed_regimes):
            case_dir = tmp_path / str(allowed_regimes)
            case_dir.mkdir()
            _assert_config_threads_regime_wiring_to_backtester(monkeypatch, case_dir, allowed_regimes)


def _assert_cli_by_name_threads_windows_spec_to_backtester(monkeypatch):
    seen = {}

    class SpyBacktester:
        commission_pct = 0.0
        slippage_pct = 0.0

        def __init__(self, *args, regime_windows_spec=None, **kwargs):
            seen["regime_windows_spec"] = regime_windows_spec

        def run(self, df, **kwargs):
            return {
                "strategy_name": "sma_crossover", "symbol": "BTC/USDT",
                "timeframe": "1d", "start_date": str(df.index[0]),
                "end_date": str(df.index[-1]), "initial_capital": 1000.0,
                "final_capital": 1000.0, "total_return_pct": 0.0,
                "annual_return_pct": 0.0, "sharpe_ratio": 0.0,
                "sortino_ratio": 0.0, "max_drawdown_pct": 0.0,
                "calmar_ratio": 0.0, "volatility_pct": 0.0, "win_rate": 0.0,
                "profit_factor": 0.0, "total_trades": 0, "avg_win_pct": 0.0,
                "avg_loss_pct": 0.0, "trades": [], "params": {},
            }

    df = pd.DataFrame(
        {"open": [100.0] * 60, "high": [101.0] * 60, "low": [99.0] * 60,
         "close": [100.0] * 60, "volume": [1000.0] * 60},
        index=pd.date_range("2024-01-01", periods=60, freq="D"),
    )
    monkeypatch.setattr(run_backtest, "Backtester", SpyBacktester)
    monkeypatch.setattr(run_backtest, "load_cached_data", lambda *a, **kw: df)

    _run_main(monkeypatch, [
        "--mode", "single", "--strategy", "sma_crossover",
        "--regime-enabled",
        "--regime-windows-spec-json", json.dumps(COMPOSITE_SPEC),
    ])
    spec = seen.get("regime_windows_spec")
    assert spec is not None, "windows spec did not thread to the Backtester"
    assert spec["medium"]["classifier"] == "composite"
    assert spec["medium"]["period"] == 20


_ADX_SPEC = {"medium": {"classifier": "adx", "period": 14}}


def _assert_cli_composite_label_reaches_backtester_with_spec(monkeypatch):
    seen = {}

    class SpyBacktester:
        commission_pct = 0.0
        slippage_pct = 0.0

        def __init__(self, *args, regime_windows_spec=None, allowed_regimes=None, **kwargs):
            seen["regime_windows_spec"] = regime_windows_spec
            seen["allowed_regimes"] = allowed_regimes

        def run(self, df, **kwargs):
            return {
                "strategy_name": "sma_crossover", "symbol": "BTC/USDT",
                "timeframe": "1d", "start_date": str(df.index[0]),
                "end_date": str(df.index[-1]), "initial_capital": 1000.0,
                "final_capital": 1000.0, "total_return_pct": 0.0,
                "annual_return_pct": 0.0, "sharpe_ratio": 0.0,
                "sortino_ratio": 0.0, "max_drawdown_pct": 0.0,
                "calmar_ratio": 0.0, "volatility_pct": 0.0, "win_rate": 0.0,
                "profit_factor": 0.0, "total_trades": 0, "avg_win_pct": 0.0,
                "avg_loss_pct": 0.0, "trades": [], "params": {},
            }

    df = pd.DataFrame(
        {"open": [100.0] * 60, "high": [101.0] * 60, "low": [99.0] * 60,
         "close": [100.0] * 60, "volume": [1000.0] * 60},
        index=pd.date_range("2024-01-01", periods=60, freq="D"),
    )
    monkeypatch.setattr(run_backtest, "Backtester", SpyBacktester)
    monkeypatch.setattr(run_backtest, "load_cached_data", lambda *a, **kw: df)

    _run_main(monkeypatch, [
        "--mode", "single", "--strategy", "sma_crossover",
        "--regime-enabled",
        "--regime-windows-spec-json", json.dumps(COMPOSITE_SPEC),
        "--allowed-regimes", "ranging_quiet",
        "--allowed-regimes", "trending_up_clean",
    ])
    assert seen.get("allowed_regimes") == ["ranging_quiet", "trending_up_clean"]
    assert seen["regime_windows_spec"]["medium"]["classifier"] == "composite"


def _config_with_regime(tmp_path, *, classifier, allowed_regimes=None):
    strat = {
        "id": "hl-temacb-btc", "type": "perps", "platform": "hyperliquid",
        "open_strategy": {"name": "sma_crossover"},
        "close_strategy": {"name": "tiered_tp_atr", "params": {"tp_tiers": [
            {"atr_multiple": 2.0, "close_fraction": 1.0}]}},
    }
    if allowed_regimes is not None:
        strat["allowed_regimes"] = allowed_regimes
    return _write_config(tmp_path, {
        "config_version": 15,
        "regime": {
            "enabled": True, "period": 14, "adx_threshold": 20.0,
            "windows": {"medium": {"classifier": classifier, "period": 30}},
        },
        "strategies": [strat],
    })


def _spy_backtester_seen(monkeypatch, df):
    seen = {}

    class SpyBacktester:
        commission_pct = 0.0
        slippage_pct = 0.0

        def __init__(self, *args, regime_windows_spec=None, allowed_regimes=None, **kwargs):
            seen["regime_windows_spec"] = regime_windows_spec
            seen["allowed_regimes"] = allowed_regimes

        def run(self, df, **kwargs):
            return {
                "strategy_name": "tema_cross_bd", "symbol": "BTC/USDT",
                "timeframe": "1d", "start_date": str(df.index[0]),
                "end_date": str(df.index[-1]), "initial_capital": 1000.0,
                "final_capital": 1000.0, "total_return_pct": 0.0,
                "annual_return_pct": 0.0, "sharpe_ratio": 0.0,
                "sortino_ratio": 0.0, "max_drawdown_pct": 0.0,
                "calmar_ratio": 0.0, "volatility_pct": 0.0, "win_rate": 0.0,
                "profit_factor": 0.0, "total_trades": 0, "avg_win_pct": 0.0,
                "avg_loss_pct": 0.0, "trades": [], "params": {},
            }

    monkeypatch.setattr(run_backtest, "Backtester", SpyBacktester)
    monkeypatch.setattr(run_backtest, "load_cached_data", lambda *a, **kw: df)
    return seen


def _assert_config_threads_regime_wiring_to_backtester(monkeypatch, tmp_path, allowed_regimes):
    df = pd.DataFrame(
        {"open": [100.0] * 60, "high": [101.0] * 60, "low": [99.0] * 60,
         "close": [100.0] * 60, "volume": [1000.0] * 60},
        index=pd.date_range("2024-01-01", periods=60, freq="D"),
    )
    seen = _spy_backtester_seen(monkeypatch, df)
    cfg = _config_with_regime(tmp_path, classifier="composite",
                              allowed_regimes=allowed_regimes)
    _run_main(monkeypatch, [
        "--mode", "single", "--strategy", "hl-temacb-btc", "--config", cfg,
    ])
    assert seen["allowed_regimes"] == allowed_regimes
    assert seen["regime_windows_spec"]["medium"]["classifier"] == "composite"



_COMPOSITE_SL_BLOCK = {"trend_regime": {
    "trending_up_clean": {"atr_multiple": 2.0},
    "trending_up_choppy": {"atr_multiple": 1.8},
    "trending_down_clean": {"atr_multiple": 2.0},
    "trending_down_choppy": {"atr_multiple": 1.8},
    "ranging_quiet": {"atr_multiple": 1.2},
    "ranging_volatile": {"atr_multiple": 1.5},
    "ranging_directional": {"atr_multiple": 1.4},
    "ranging_directional_up": {"atr_multiple": 1.4},
    "ranging_directional_down": {"atr_multiple": 1.4},
}}
_ADX_SL_BLOCK = {"trend_regime": {
    "trending_up": {"atr_multiple": 2.0},
    "trending_down": {"atr_multiple": 2.0},
    "ranging": {"atr_multiple": 1.5},
}}
_COMPOSITE_TRAIL_BLOCK = {"trend_regime": {
    k: {"atr_multiple": v["atr_multiple"] + 0.5}
    for k, v in _COMPOSITE_SL_BLOCK["trend_regime"].items()
}}


def _bt_with_sl_regime(spec, block, *, trailing=False):
    kw = {"trailing_stop_atr_mult_regime": block} if trailing else {"stop_loss_atr_mult_regime": block}
    return Backtester(initial_capital=1000.0, regime_enabled=True,
                      regime_windows_spec=spec, **kw)


def test_regime_keyed_stop_and_tp_blocks_resolve_under_the_primary_classifier(subtests):
    with subtests.test(case="composite_keyed_sl_regime"):
        bt = _bt_with_sl_regime(COMPOSITE_SPEC, _COMPOSITE_SL_BLOCK)
        assert bt._stop_loss_regime_block.resolve("ranging_quiet").atr == 1.2
        assert bt._stop_loss_regime_block.resolve("trending_up_clean").atr == 2.0
    with subtests.test(case="adx_primary_adx_keyed_sl_regime_byte_identical"):
        bt_adx = _bt_with_sl_regime({"medium": {"classifier": "adx", "period": 14}}, _ADX_SL_BLOCK)
        assert bt_adx._stop_loss_regime_block.resolve("ranging").atr == 1.5
        bt_legacy = _bt_with_sl_regime(None, _ADX_SL_BLOCK)
        assert bt_legacy._stop_loss_regime_block.resolve("trending_up").atr == 2.0
    with subtests.test(case="composite_keyed_trailing_regime"):
        bt = _bt_with_sl_regime(COMPOSITE_SPEC, _COMPOSITE_TRAIL_BLOCK, trailing=True)
        assert bt._trailing_stop_regime_block.resolve("ranging_quiet").atr == 1.7
    with subtests.test(case="validator_accepts_composite_sl_with_sl_after"):
        _assert_validator_accepts_composite_sl_with_sl_after()
    with subtests.test(case="composite_keyed_tiered_tp"):
        bt = _bt_with_tiered_regime(COMPOSITE_SPEC, _COMPOSITE_TP_LABELS)
        fr = bt._sl_mod.parse_tp_tier_close_fractions(
            [_regime_tiered_ref(_COMPOSITE_TP_LABELS)], regime="ranging_quiet")
        assert fr == [0.5, 1.0]
    with subtests.test(case="adx_keyed_tiered_tp_byte_identical"):
        bt_adx = _bt_with_tiered_regime({"medium": {"classifier": "adx", "period": 14}},
                                        _ADX_TP_LABELS)
        assert bt_adx._sl_mod.parse_tp_tier_close_fractions(
            [_regime_tiered_ref(_ADX_TP_LABELS)], regime="ranging") == [0.5, 1.0]
        bt_legacy = _bt_with_tiered_regime(None, _ADX_TP_LABELS)
        assert bt_legacy._sl_mod.parse_tp_tier_close_fractions(
            [_regime_tiered_ref(_ADX_TP_LABELS)], regime="trending_up") == [0.5, 1.0]


def test_regime_label_vocabulary_mismatch_and_non_exhaustive_blocks_reject(subtests):
    with subtests.test(case="composite_primary_adx_keyed_sl_regime"):
        with pytest.raises(ValueError) as exc:
            _bt_with_sl_regime(COMPOSITE_SPEC, _ADX_SL_BLOCK)
        msg = str(exc.value)
        assert "unknown regime label" in msg
        assert "ranging_quiet" in msg or "trending_up_clean" in msg
    with subtests.test(case="composite_primary_adx_keyed_trailing"):
        with pytest.raises(ValueError):
            _bt_with_sl_regime(COMPOSITE_SPEC, _ADX_SL_BLOCK, trailing=True)
    with subtests.test(case="composite_primary_adx_keyed_tiered_tp"):
        with pytest.raises(ValueError) as exc:
            _bt_with_tiered_regime(COMPOSITE_SPEC, _ADX_TP_LABELS)
        assert "tiered-TP" in str(exc.value) and "unknown regime label" in str(exc.value)
    with subtests.test(case="adx_primary_composite_keyed_tiered_tp"):
        with pytest.raises(ValueError):
            _bt_with_tiered_regime({"medium": {"classifier": "adx", "period": 14}},
                                   _COMPOSITE_TP_LABELS)
    with subtests.test(case="composite_primary_non_exhaustive_tiered_tp"):
        ref = _partial_tiered_ref(_COMPOSITE_TP_LABELS, "ranging_directional")
        with pytest.raises(ValueError) as exc:
            _bt_with_tiered_ref(COMPOSITE_SPEC, ref)
        msg = str(exc.value)
        assert "tiered-TP" in msg
        assert "missing required regime labels" in msg
        assert "ranging_directional" in msg
    with subtests.test(case="adx_primary_non_exhaustive_tiered_tp"):
        ref = _partial_tiered_ref(_ADX_TP_LABELS, "ranging")
        with pytest.raises(ValueError) as exc:
            _bt_with_tiered_ref(None, ref)
        msg = str(exc.value)
        assert "missing required regime labels" in msg
        assert "ranging" in msg
    with subtests.test(case="validate_regime_tiered_tp_labels_helper"):
        _assert_validate_regime_tiered_tp_labels_helper()
    with subtests.test(case="validate_regime_tiered_tp_labels_exhaustiveness_unit"):
        _assert_validate_regime_tiered_tp_labels_exhaustiveness_unit()


def _assert_validator_accepts_composite_sl_with_sl_after():
    close_ref = {"name": "tiered_tp_atr", "params": {
        "tp_tiers": [
            {"atr_multiple": 2.0, "close_fraction": 0.5},
            {"atr_multiple": 3.0, "close_fraction": 1.0, "sl_after": {"kind": "breakeven"}},
        ],
    }}
    bt = Backtester(
        initial_capital=1000.0, regime_enabled=True,
        regime_windows_spec=COMPOSITE_SPEC,
        stop_loss_atr_mult_regime=_COMPOSITE_SL_BLOCK,
        close_strategies=[close_ref],
    )
    assert bt._stop_loss_regime_block.resolve("ranging_volatile").atr == 1.5


_ADX_TP_LABELS = ["trending_up", "trending_down", "ranging"]
_COMPOSITE_TP_LABELS = list(VALID_LABELS_COMPOSITE)


def _regime_tp_tier(labels, atr, frac):
    return {"trend_regime": {l: {"atr_multiple": atr, "close_fraction": frac} for l in labels}}


def _regime_tiered_ref(labels):
    return {"name": "tiered_tp_atr_regime", "params": {"tp_tiers": [
        _regime_tp_tier(labels, 2.0, 0.5),
        _regime_tp_tier(labels, 3.0, 1.0),
    ]}}


def _bt_with_tiered_regime(spec, labels):
    return Backtester(initial_capital=1000.0, regime_enabled=True,
                      regime_windows_spec=spec,
                      close_strategies=[_regime_tiered_ref(labels)])


def _assert_validate_regime_tiered_tp_labels_helper():
    _sl = _bt_with_tiered_regime(None, _ADX_TP_LABELS)._sl_mod
    assert _sl.validate_regime_tiered_tp_labels([_regime_tiered_ref(_ADX_TP_LABELS)]) == []
    assert _sl.validate_regime_tiered_tp_labels(
        [_regime_tiered_ref(_ADX_TP_LABELS)],
        labels=_COMPOSITE_TP_LABELS) != []
    assert _sl.validate_regime_tiered_tp_labels(
        [_regime_tiered_ref(_COMPOSITE_TP_LABELS)],
        labels=_COMPOSITE_TP_LABELS) == []
    assert _sl.validate_regime_tiered_tp_labels(
        [{"name": "tiered_tp_atr", "params": {"tp_tiers": [
            {"atr_multiple": 2.0, "close_fraction": 0.5},
            {"atr_multiple": 3.0, "close_fraction": 1.0}]}}],
        labels=_COMPOSITE_TP_LABELS) == []


def _partial_tiered_ref(labels, omit):
    kept = [l for l in labels if l != omit]
    return {"name": "tiered_tp_atr_regime", "params": {"tp_tiers": [
        _regime_tp_tier(kept, 2.0, 0.5),
        _regime_tp_tier(kept, 3.0, 1.0),
    ]}}


def _bt_with_tiered_ref(spec, ref):
    return Backtester(initial_capital=1000.0, regime_enabled=True,
                      regime_windows_spec=spec, close_strategies=[ref])


def _assert_validate_regime_tiered_tp_labels_exhaustiveness_unit():
    _sl = _bt_with_tiered_regime(None, _ADX_TP_LABELS)._sl_mod
    errs = _sl.validate_regime_tiered_tp_labels(
        [_partial_tiered_ref(_COMPOSITE_TP_LABELS, "ranging_volatile")],
        labels=_COMPOSITE_TP_LABELS)
    assert errs and any("missing required regime labels" in e for e in errs)
    assert any("ranging_volatile" in e for e in errs)
    use_defaults_ref = {"name": "tiered_tp_atr_regime", "params": {"tp_tiers": [
        {"use_defaults": True, "close_fraction": 1.0}]}}
    assert _sl.validate_regime_tiered_tp_labels(
        [use_defaults_ref], labels=_COMPOSITE_TP_LABELS) == []
    assert _sl.validate_regime_tiered_tp_labels(
        [_regime_tiered_ref(_COMPOSITE_TP_LABELS)],
        labels=_COMPOSITE_TP_LABELS) == []
