"""#1058: the Backtester CLASSIFIES composite (7-state) regime labels from OHLCV
when threaded a ``regime_windows_spec``, and feeds those substate labels to the
entry gate AND the close evaluator's position regime.

The sibling ``test_backtester_composite_regime.py`` covers the entry GATE with a
*pre-supplied* ``regime`` column — it never exercises the backtester computing
composite labels itself. These tests cover the new wiring: ``ensure_regime_columns``
threaded with the live ``regime.windows`` spec, picking the PRIMARY window
(medium-first) exactly as the live regime store's ``regime_from_injected_payload``
does, so ``_run_position_regime`` (the label close evaluators consume) carries the
composite substate instead of the ADX 3-state label.

Ground truth for the per-bar labels is the SAME ``ensure_regime_columns`` call the
backtester makes internally, so assertions track the classifier instead of
hard-coding synthetic-OHLCV buckets.
"""
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
    """Chop for the first half, clean uptrend for the second — guarantees a mix
    of composite substates (ranging_* and trending_*) within one series."""
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
    """The exact per-bar bar-close labels the backtester computes internally:
    the same ``ensure_regime_columns`` + windows_spec (medium-first primary)."""
    truth = df.copy()
    ensure_regime_columns(truth, windows_spec=spec)
    return truth["regime"]


def _buy_at(df: pd.DataFrame, bar: int) -> pd.DataFrame:
    out = df.copy()
    out["signal"] = 0
    out.iloc[bar, out.columns.get_loc("signal")] = 1
    return out


# ─── The classifier the backtester threads is composite, not ADX ─────────────


def test_windows_spec_computes_composite_substates_from_ohlcv():
    labels = set(_ground_truth_labels(_mixed_regime_ohlcv()).unique())
    # Every emitted label is from the 7-state composite vocabulary…
    assert labels <= VALID_LABELS_COMPOSITE
    # …and the ADX (3-state) and composite vocabularies are disjoint, so a
    # composite substate could never be produced by the legacy ADX path.
    assert labels & VALID_LABELS_ADX == set()
    assert labels, "expected non-empty composite labels"


# ─── Close-evaluator position regime carries the composite substate ──────────


def test_position_regime_is_composite_substate():
    """``_run_position_regime`` is the label fed to close evaluators
    (``position_regime=`` in the per-bar loop). With a composite windows_spec it
    must be the substate computed at the open bar — not an ADX label."""
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
    assert result["total_trades"] == 1  # opened, held to the end
    # Signal at bar N fills at N+1, stamping bar N's regime (shift parity).
    assert bt._run_position_regime == expected
    assert bt._run_position_regime in VALID_LABELS_COMPOSITE


def test_default_path_stays_adx_without_windows_spec():
    """No windows_spec → the legacy single-lookback ADX path is unchanged: the
    stamped position regime is a 3-state ADX label, never a composite substate."""
    df = _mixed_regime_ohlcv()
    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        regime_enabled=True,  # regime_windows_spec defaults to None
    )
    bt.run(_buy_at(df, 130), save=False)
    assert bt._run_position_regime in VALID_LABELS_ADX
    assert bt._run_position_regime not in VALID_LABELS_COMPOSITE


# ─── The computed composite label gates entries ──────────────────────────────


def test_composite_gate_allows_matching_blocks_mismatching():
    """The backtester gates entries on the COMPUTED composite label: an
    allowed_regimes naming the entry bar's substate permits the fill; naming a
    different substate blocks it."""
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


# ─── Look-ahead: the gate reads the DECISION bar's label, never the fill bar's ─


def test_composite_label_respects_lookahead_shift():
    """The composite label is consumed under the same N→N+1 shift as ADX: the
    position regime is stamped from the entry-DECISION bar (N), so a label that
    changes between bar N and the fill bar N+1 proves no look-ahead."""
    df = _mixed_regime_ohlcv()
    truth = _ground_truth_labels(df)
    # A bar whose composite label differs from the next bar's — reading the
    # wrong bar would stamp a different regime.
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


# ─── run_backtest._resolve_regime_windows_spec: config → normalized spec ──────


def test_resolve_windows_spec_none_when_disabled_or_no_windows():
    assert run_backtest._resolve_regime_windows_spec(None) is None
    assert run_backtest._resolve_regime_windows_spec({}) is None
    assert run_backtest._resolve_regime_windows_spec({"enabled": False, "windows": {"medium": 20}}) is None
    # Enabled but no windows → None (legacy single-lookback ADX path handles it).
    assert run_backtest._resolve_regime_windows_spec({"enabled": True, "period": 14}) is None


def test_resolve_windows_spec_composite_medium():
    spec = run_backtest._resolve_regime_windows_spec({
        "enabled": True,
        "windows": {"medium": {"classifier": "composite", "period": 30}},
    })
    assert spec is not None
    assert spec["medium"]["classifier"] == "composite"
    assert spec["medium"]["period"] == 30
    # Composite thresholds default-merged to the shared defaults.
    assert spec["medium"]["thresholds"] == _DEFAULT_COMPOSITE_THRESHOLDS


def test_resolve_windows_spec_adx_inherits_top_level_threshold_and_period():
    """resolvedForEmit parity: an ADX window missing adx_threshold/period inherits
    the top-level regime.adx_threshold / regime.period (Go RegimeWindowSpec)."""
    spec = run_backtest._resolve_regime_windows_spec({
        "enabled": True,
        "period": 21,
        "adx_threshold": 28.0,
        # Bare int = ADX shorthand; an explicit object missing period/threshold.
        "windows": {"medium": {"classifier": "adx"}},
    })
    assert spec["medium"]["classifier"] == "adx"
    assert spec["medium"]["period"] == 21        # inherited from top-level period
    assert spec["medium"]["adx_threshold"] == 28.0  # inherited from top-level adx_threshold


def test_resolve_windows_spec_keeps_explicit_window_values():
    spec = run_backtest._resolve_regime_windows_spec({
        "enabled": True,
        "period": 14,
        "adx_threshold": 20.0,
        "windows": {"short": {"classifier": "adx", "period": 7, "adx_threshold": 30.0}},
    })
    assert spec["short"]["period"] == 7
    assert spec["short"]["adx_threshold"] == 30.0


# ─── load_strategy_config threads regime_windows_spec from --config ──────────


def _write_config(tmp_path, cfg):
    p = tmp_path / "config.json"
    p.write_text(json.dumps(cfg, indent=2))
    return str(p)


def _composite_config(tmp_path):
    return _write_config(tmp_path, {
        "config_version": 15,
        "regime": {
            "enabled": True,
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


def test_load_strategy_config_includes_composite_windows_spec(tmp_path):
    kwargs = run_backtest.load_strategy_config(_composite_config(tmp_path), "hl-temacb-btc")
    spec = kwargs["regime_windows_spec"]
    assert spec is not None
    assert spec["medium"]["classifier"] == "composite"
    assert spec["medium"]["period"] == 30


def test_load_strategy_config_no_windows_yields_none(tmp_path):
    path = _write_config(tmp_path, {
        "config_version": 15,
        "regime": {"enabled": True, "period": 14, "adx_threshold": 20.0},
        "strategies": [{
            "id": "hl-temacb-btc", "type": "perps", "platform": "hyperliquid",
            "open_strategy": {"name": "tema_cross_bd"},
            "close_strategy": {"name": "tiered_tp_atr", "params": {"tp_tiers": [
                {"atr_multiple": 2.0, "close_fraction": 1.0},
            ]}},
        }],
    })
    kwargs = run_backtest.load_strategy_config(path, "hl-temacb-btc")
    assert kwargs["regime_windows_spec"] is None


# ─── CLI: --regime-windows-spec-json parsing + rejections ────────────────────


def _run_main(monkeypatch, argv):
    monkeypatch.setattr(sys, "argv", ["run_backtest.py", *argv])
    return run_backtest.main()


def test_cli_rejects_windows_spec_with_non_single_mode(monkeypatch):
    with pytest.raises(SystemExit):
        _run_main(monkeypatch, [
            "--mode", "compare", "--strategy", "sma_crossover",
            "--regime-windows-spec-json", json.dumps(COMPOSITE_SPEC),
        ])


def test_cli_rejects_malformed_windows_spec(monkeypatch):
    with pytest.raises(SystemExit):
        _run_main(monkeypatch, [
            "--mode", "single", "--strategy", "sma_crossover",
            "--regime-windows-spec-json", "{not valid json",
        ])


def test_cli_rejects_windows_spec_with_config(monkeypatch, tmp_path):
    with pytest.raises(SystemExit):
        _run_main(monkeypatch, [
            "--mode", "single", "--strategy", "hl-temacb-btc",
            "--config", _composite_config(tmp_path),
            "--regime-windows-spec-json", json.dumps(COMPOSITE_SPEC),
        ])


def test_cli_by_name_threads_windows_spec_to_backtester(monkeypatch):
    """End-to-end: --regime-windows-spec-json on a by-name single backtest must
    reach the Backtester constructor (parse → main → run_single_backtest →
    Backtester), not stop at argparse."""
    seen = {}

    class SpyBacktester:
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


# ─── --allowed-regimes vocabulary tracks the primary window's classifier ──────
# (#1058 review: composite primary → 7-state gate labels; ADX primary → 3 labels.
# The gate must be expressible through the SAME surface that computes the label.)


_ADX_SPEC = {"medium": {"classifier": "adx", "period": 14}}
_COMPOSITE_PRIMARY_WITH_ADX = {
    "medium": {"classifier": "composite", "period": 30},
    "short": {"classifier": "adx", "period": 7},
}
_COMPOSITE_NO_MEDIUM = {"slow": {"classifier": "composite", "period": 40}}


def test_primary_classifier_none_spec_is_adx():
    assert run_backtest._primary_window_classifier(None) == "adx"
    assert run_backtest._primary_window_classifier({}) == "adx"


def test_primary_classifier_medium_first():
    assert run_backtest._primary_window_classifier(COMPOSITE_SPEC) == "composite"


def test_primary_classifier_mixed_spec_uses_medium_not_other_window():
    # Must-survive (c): medium is the primary even when an ADX window coexists.
    assert run_backtest._primary_window_classifier(
        _COMPOSITE_PRIMARY_WITH_ADX) == "composite"


def test_primary_classifier_no_medium_uses_sorted_first():
    assert run_backtest._primary_window_classifier(_COMPOSITE_NO_MEDIUM) == "composite"
    assert run_backtest._primary_window_classifier(
        {"z": {"classifier": "composite"}, "a": {"classifier": "adx"}}) == "adx"


def test_validate_accepts_adx_label_no_spec():
    # Preserves the old argparse choices behavior for the legacy ADX path.
    run_backtest._validate_allowed_regimes_vocabulary(["trending_up", "ranging"], None)


def test_validate_accepts_composite_label_with_composite_spec():
    # Must-survive (a): composite gate label with a composite primary is valid.
    run_backtest._validate_allowed_regimes_vocabulary(["ranging_quiet"], COMPOSITE_SPEC)
    run_backtest._validate_allowed_regimes_vocabulary(
        ["trending_up_clean", "ranging_directional"], COMPOSITE_SPEC)


def test_validate_rejects_composite_label_without_spec():
    # Must-survive (b): the inverse — ADX primary must NOT accept composite labels.
    with pytest.raises(SystemExit):
        run_backtest._validate_allowed_regimes_vocabulary(["ranging_quiet"], None)
    with pytest.raises(SystemExit):
        run_backtest._validate_allowed_regimes_vocabulary(["trending_up_clean"], _ADX_SPEC)


def test_validate_rejects_bare_adx_label_with_composite_primary():
    # Must-survive (c): a composite classifier never emits bare "trending_up",
    # so gating on it would silently block every entry — reject loudly.
    with pytest.raises(SystemExit):
        run_backtest._validate_allowed_regimes_vocabulary(["trending_up"], COMPOSITE_SPEC)
    with pytest.raises(SystemExit):
        run_backtest._validate_allowed_regimes_vocabulary(
            ["trending_up"], _COMPOSITE_PRIMARY_WITH_ADX)


def test_validate_rejects_garbage_label():
    with pytest.raises(SystemExit):
        run_backtest._validate_allowed_regimes_vocabulary(["not_a_regime"], None)


def test_validate_noop_on_empty():
    run_backtest._validate_allowed_regimes_vocabulary(None, COMPOSITE_SPEC)
    run_backtest._validate_allowed_regimes_vocabulary([], COMPOSITE_SPEC)


def test_validate_compound_partial_invalid_rejects():
    # One valid + one invalid → reject (graded by the weakest member).
    with pytest.raises(SystemExit):
        run_backtest._validate_allowed_regimes_vocabulary(
            ["ranging_quiet", "trending_up"], COMPOSITE_SPEC)


def test_cli_composite_label_reaches_backtester_with_spec(monkeypatch):
    """Must-survive (a) end-to-end: --allowed-regimes <composite> no longer
    argparse-rejects when a composite spec is supplied; it threads through."""
    seen = {}

    class SpyBacktester:
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


def test_cli_composite_label_rejected_without_spec(monkeypatch):
    """Must-survive (b) end-to-end: composite label on an ADX (no-spec) by-name
    backtest is rejected by our validator (not silently accepted)."""
    with pytest.raises(SystemExit):
        _run_main(monkeypatch, [
            "--mode", "single", "--strategy", "sma_crossover",
            "--regime-enabled",
            "--allowed-regimes", "ranging_quiet",
        ])


# ─── --config path enforces the SAME vocabulary check (#1058 review 2) ────────
# The backtester reads config JSON directly and never runs the Go
# validateStrategyRegimeVocabulary, so the --config path must validate the
# config-threaded (allowed_regimes, regime_windows_spec) pair itself — else a
# hand-edited config silently blocks every entry (0-trade run).


def _config_with_regime(tmp_path, *, classifier, allowed_regimes=None):
    strat = {
        "id": "hl-temacb-btc", "type": "perps", "platform": "hyperliquid",
        # sma_crossover (a registry strategy) so the accept-path tests reach the
        # Backtester; the open name is irrelevant to the vocabulary check itself.
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


def test_config_rejects_adx_label_under_composite_primary(monkeypatch, tmp_path):
    # Must-survive (a): composite primary window + config allowed_regimes holding
    # a bare ADX label → reject, not a silent 0-trade run.
    cfg = _config_with_regime(tmp_path, classifier="composite", allowed_regimes=["ranging"])
    with pytest.raises(SystemExit):
        _run_main(monkeypatch, [
            "--mode", "single", "--strategy", "hl-temacb-btc", "--config", cfg,
        ])


def test_config_rejects_composite_label_under_adx_primary(monkeypatch, tmp_path):
    # Must-survive (b): inverse — ADX primary + config allowed_regimes holding a
    # composite substate → reject.
    cfg = _config_with_regime(tmp_path, classifier="adx", allowed_regimes=["ranging_quiet"])
    with pytest.raises(SystemExit):
        _run_main(monkeypatch, [
            "--mode", "single", "--strategy", "hl-temacb-btc", "--config", cfg,
        ])


def _spy_backtester_seen(monkeypatch, df):
    seen = {}

    class SpyBacktester:
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


def test_config_matching_composite_label_runs(monkeypatch, tmp_path):
    # Composite primary + matching composite label → no false rejection; threads.
    df = pd.DataFrame(
        {"open": [100.0] * 60, "high": [101.0] * 60, "low": [99.0] * 60,
         "close": [100.0] * 60, "volume": [1000.0] * 60},
        index=pd.date_range("2024-01-01", periods=60, freq="D"),
    )
    seen = _spy_backtester_seen(monkeypatch, df)
    cfg = _config_with_regime(tmp_path, classifier="composite", allowed_regimes=["ranging_quiet"])
    _run_main(monkeypatch, [
        "--mode", "single", "--strategy", "hl-temacb-btc", "--config", cfg,
    ])
    assert seen["allowed_regimes"] == ["ranging_quiet"]
    assert seen["regime_windows_spec"]["medium"]["classifier"] == "composite"


def test_config_absent_allowed_regimes_runs(monkeypatch, tmp_path):
    # Must-survive (c): empty/absent allowed_regimes must NOT be falsely rejected.
    df = pd.DataFrame(
        {"open": [100.0] * 60, "high": [101.0] * 60, "low": [99.0] * 60,
         "close": [100.0] * 60, "volume": [1000.0] * 60},
        index=pd.date_range("2024-01-01", periods=60, freq="D"),
    )
    seen = _spy_backtester_seen(monkeypatch, df)
    cfg = _config_with_regime(tmp_path, classifier="composite", allowed_regimes=None)
    _run_main(monkeypatch, [
        "--mode", "single", "--strategy", "hl-temacb-btc", "--config", cfg,
    ])
    assert seen["allowed_regimes"] is None
    assert seen["regime_windows_spec"]["medium"]["classifier"] == "composite"


# ─── Regime-keyed exit consumers resolve composite labels (#1058 review 3) ────
# The composite label now feeds _run_position_regime, so the regime-keyed
# stop_loss_atr_regime / trailing_stop_atr_regime / sl_after blocks must validate
# and resolve against the PRIMARY window's classifier vocabulary — mirroring live
# regimeLabelsForStrategyWindow -> parseRegimeATRBlock. Else a composite-keyed
# block is falsely rejected, and an ADX-keyed block silently resolves to the
# default stop under a composite stamp.


from backtester import _regime_primary_labels  # noqa: E402

_COMPOSITE_SL_BLOCK = {"trend_regime": {
    "trending_up_clean": {"atr_multiple": 2.0},
    "trending_up_choppy": {"atr_multiple": 1.8},
    "trending_down_clean": {"atr_multiple": 2.0},
    "trending_down_choppy": {"atr_multiple": 1.8},
    "ranging_quiet": {"atr_multiple": 1.2},
    "ranging_volatile": {"atr_multiple": 1.5},
    "ranging_directional": {"atr_multiple": 1.4},
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


def test_regime_primary_labels_helper():
    labels = _regime_primary_labels(COMPOSITE_SPEC)
    assert set(labels) == set(VALID_LABELS_COMPOSITE)
    # ADX-primary and no-spec both fall back to the canonical ADX default (None).
    assert _regime_primary_labels({"medium": {"classifier": "adx", "period": 14}}) is None
    assert _regime_primary_labels(None) is None
    # Mixed spec: primary is medium (composite) even with an ADX sibling window.
    assert set(_regime_primary_labels({
        "medium": {"classifier": "composite", "period": 30},
        "short": {"classifier": "adx", "period": 7},
    })) == set(VALID_LABELS_COMPOSITE)


def _bt_with_sl_regime(spec, block, *, trailing=False):
    kw = {"trailing_stop_atr_regime": block} if trailing else {"stop_loss_atr_regime": block}
    return Backtester(initial_capital=1000.0, regime_enabled=True,
                      regime_windows_spec=spec, **kw)


def test_composite_keyed_sl_regime_parses_and_resolves():
    # Must-survive (a): composite primary + exhaustive composite-keyed SL block
    # parses and resolves per substate (not rejected, not defaulted).
    bt = _bt_with_sl_regime(COMPOSITE_SPEC, _COMPOSITE_SL_BLOCK)
    assert bt._stop_loss_regime_block.resolve("ranging_quiet").atr == 1.2
    assert bt._stop_loss_regime_block.resolve("trending_up_clean").atr == 2.0


def test_composite_primary_adx_keyed_sl_regime_rejects():
    # Must-survive (b): ADX-keyed block under a composite primary is rejected
    # loudly — never a silent fall-back to the default stop.
    with pytest.raises(ValueError) as exc:
        _bt_with_sl_regime(COMPOSITE_SPEC, _ADX_SL_BLOCK)
    msg = str(exc.value)
    assert "unknown regime label" in msg
    # The message names the composite vocabulary, not the misleading ADX one.
    assert "ranging_quiet" in msg or "trending_up_clean" in msg


def test_adx_primary_adx_keyed_sl_regime_byte_identical():
    # Must-survive (c): ADX primary + ADX-keyed and the legacy no-spec path both
    # resolve the 3 ADX labels exactly as before.
    bt_adx = _bt_with_sl_regime({"medium": {"classifier": "adx", "period": 14}}, _ADX_SL_BLOCK)
    assert bt_adx._stop_loss_regime_block.resolve("ranging").atr == 1.5
    bt_legacy = _bt_with_sl_regime(None, _ADX_SL_BLOCK)
    assert bt_legacy._stop_loss_regime_block.resolve("trending_up").atr == 2.0


def test_composite_primary_adx_keyed_sl_regime_does_not_silently_default():
    # The pre-fix bug: ADX-keyed block under composite parsed, then resolve of the
    # composite stamp returned None → default stop. Assert it now raises instead
    # of producing a block that silently misses on every composite label.
    with pytest.raises(ValueError):
        _bt_with_sl_regime(COMPOSITE_SPEC, _ADX_SL_BLOCK)


def test_composite_keyed_trailing_regime_parses_and_resolves():
    # Same guarantee on the trailing-stop surface.
    bt = _bt_with_sl_regime(COMPOSITE_SPEC, _COMPOSITE_TRAIL_BLOCK, trailing=True)
    assert bt._trailing_stop_regime_block.resolve("ranging_quiet").atr == 1.7


def test_composite_primary_adx_keyed_trailing_rejects():
    with pytest.raises(ValueError):
        _bt_with_sl_regime(COMPOSITE_SPEC, _ADX_SL_BLOCK, trailing=True)


def test_validator_accepts_composite_sl_with_sl_after():
    # Exercises validate_post_tp_stop_loss_rules' threaded labels: a composite SL
    # block paired with a tiered close carrying sl_after must NOT be falsely
    # rejected (the validator re-parses stop_loss_atr_regime with the labels).
    close_ref = {"name": "tiered_tp_atr", "params": {
        "tp_tiers": [
            {"atr_multiple": 2.0, "close_fraction": 0.5},
            {"atr_multiple": 3.0, "close_fraction": 1.0, "sl_after": {"kind": "breakeven"}},
        ],
    }}
    bt = Backtester(
        initial_capital=1000.0, regime_enabled=True,
        regime_windows_spec=COMPOSITE_SPEC,
        stop_loss_atr_regime=_COMPOSITE_SL_BLOCK,
        close_strategies=[close_ref],
    )
    assert bt._stop_loss_regime_block.resolve("ranging_volatile").atr == 1.5


# ─── tiered_tp_atr_regime tier vocabulary validated at load (#1058 review 4) ──
# A tier set keyed by labels the primary classifier can never emit must be
# rejected at construction (mirroring live regime_atr.go parseRegimeTPTiers),
# not silently no-op every take-profit tier at runtime.

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


def test_composite_keyed_tiered_tp_resolves():
    # Must-survive (c): composite primary + composite-keyed tiers → constructs and
    # the per-substate close fractions resolve.
    bt = _bt_with_tiered_regime(COMPOSITE_SPEC, _COMPOSITE_TP_LABELS)
    fr = bt._sl_mod.parse_tp_tier_close_fractions(
        [_regime_tiered_ref(_COMPOSITE_TP_LABELS)], regime="ranging_quiet")
    assert fr == [0.5, 1.0]


def test_composite_primary_adx_keyed_tiered_tp_rejects():
    # Must-survive (a): ADX-keyed tiers under a composite primary → reject at load,
    # never a silent 0-TP run.
    with pytest.raises(ValueError) as exc:
        _bt_with_tiered_regime(COMPOSITE_SPEC, _ADX_TP_LABELS)
    assert "tiered-TP" in str(exc.value) and "unknown regime label" in str(exc.value)


def test_adx_primary_composite_keyed_tiered_tp_rejects():
    # Must-survive (b): inverse — composite-keyed tiers under an ADX primary →
    # reject, not a silent no-op.
    with pytest.raises(ValueError):
        _bt_with_tiered_regime({"medium": {"classifier": "adx", "period": 14}},
                               _COMPOSITE_TP_LABELS)


def test_adx_keyed_tiered_tp_byte_identical():
    # Must-survive (c): ADX primary + ADX-keyed and legacy no-spec + ADX-keyed
    # both construct and resolve exactly as before.
    bt_adx = _bt_with_tiered_regime({"medium": {"classifier": "adx", "period": 14}},
                                    _ADX_TP_LABELS)
    assert bt_adx._sl_mod.parse_tp_tier_close_fractions(
        [_regime_tiered_ref(_ADX_TP_LABELS)], regime="ranging") == [0.5, 1.0]
    bt_legacy = _bt_with_tiered_regime(None, _ADX_TP_LABELS)
    assert bt_legacy._sl_mod.parse_tp_tier_close_fractions(
        [_regime_tiered_ref(_ADX_TP_LABELS)], regime="trending_up") == [0.5, 1.0]


def test_validate_regime_tiered_tp_labels_helper():
    # Direct unit on the shared-module validator: labels=None (legacy) accepts ADX
    # keys; composite labels reject ADX keys; composite labels accept composite.
    # Reach the close module via a constructed Backtester so the close-strategies
    # path is on sys.path regardless of test ordering.
    _sl = _bt_with_tiered_regime(None, _ADX_TP_LABELS)._sl_mod
    assert _sl.validate_regime_tiered_tp_labels([_regime_tiered_ref(_ADX_TP_LABELS)]) == []
    assert _sl.validate_regime_tiered_tp_labels(
        [_regime_tiered_ref(_ADX_TP_LABELS)],
        labels=_COMPOSITE_TP_LABELS) != []
    assert _sl.validate_regime_tiered_tp_labels(
        [_regime_tiered_ref(_COMPOSITE_TP_LABELS)],
        labels=_COMPOSITE_TP_LABELS) == []
    # A non-regime close ref is ignored (no false positive).
    assert _sl.validate_regime_tiered_tp_labels(
        [{"name": "tiered_tp_atr", "params": {"tp_tiers": [
            {"atr_multiple": 2.0, "close_fraction": 0.5},
            {"atr_multiple": 3.0, "close_fraction": 1.0}]}}],
        labels=_COMPOSITE_TP_LABELS) == []
