"""#1338: tests for the live-strategy tuning pipeline (backtest/tune_live.py).

Pure helpers (neighborhood generation, override merge/validate, window
partitioning, patch emission) are exercised without data access, matching the
auto_suggest pure-core convention. Integration paths monkeypatch
``load_cached_data`` (a synthetic frame) and ``run_stage2`` (a canned
auto_suggest report) so no market data or M-harness subprocess is needed —
mirroring the eval_windows / run_backtest test recipe. The selection-aware
statistics (disjoint stage-1 slice, BH family size = searched N) are asserted
directly."""
import json
import os
import types

import numpy as np
import pandas as pd
import pytest

import auto_suggest
import tune_live as tl
from registry_loader import load_registry


# ==========================================================================
# Fixtures / builders
# ==========================================================================

def _synthetic_df(start="2019-01-01", end="2026-03-01", freq="1D"):
    idx = pd.date_range(start, end, freq=freq)
    n = len(idx)
    base = 100 + np.cumsum(np.sin(np.arange(n) / 9.0)) + np.arange(n) * 0.01
    return pd.DataFrame({
        "open": base, "high": base * 1.02, "low": base * 0.98,
        "close": base + np.cos(np.arange(n) / 5.0),
        "volume": np.full(n, 1000.0),
    }, index=idx)


def _write_config(tmp_path, cfg):
    p = tmp_path / "config.json"
    p.write_text(json.dumps(cfg, indent=2))
    return str(p)


def _sma_config(params=None, sid="spot-sma-btc"):
    return {
        "config_version": 15,
        "strategies": [{
            "id": sid, "type": "spot", "platform": "binanceus",
            "args": ["sma_crossover", "BTC/USDT", "1d"],
            "open_strategy": {"name": "sma_crossover",
                              "params": params if params is not None
                              else {"fast_period": 18, "slow_period": 50}},
        }],
    }


def _make_args(**over):
    base = dict(
        param=None, datasets=None, windows=["is", "oos"], step_frac=0.25,
        neighborhood_steps=0, splits=5, capital=1000.0,
        optimize_metric="sharpe_ratio", alpha=0.05, max_candidates=64, jobs=1,
        dry_run=False, verbose=False,
    )
    base.update(over)
    return types.SimpleNamespace(**base)


def _canned_stage2(spec_path, out_json, out_dir, jobs, survivor_key="cand_0"):
    """A stand-in for run_stage2: reads the generated spec and returns an
    auto_suggest-shaped report marking one candidate a survivor."""
    with open(spec_path) as fh:
        spec = json.load(fh)
    ranked = []
    for c in spec["candidates"]:
        verdict = "survivor" if c["key"] == survivor_key else "incumbent_stands"
        ranked.append({"key": c["key"], "verdict": verdict,
                       "candidate": c["candidate"], "evidence": {},
                       "limitations": []})
    return {
        "correction": {"method": "benjamini_hochberg", "alpha": 0.05,
                       "m": spec["correction"]["family_size"],
                       "tests_run": len(spec["candidates"]),
                       "effective_threshold": 0.01, "n_survivors": 1},
        "ranked": ranked, "_exit_code": 0,
    }


# ==========================================================================
# 1. Pure — value coercion + neighborhood generation
# ==========================================================================

@pytest.mark.parametrize("raw,expected", [
    ("10", 10), ("1.5", 1.5), ("true", True), ("false", False),
    ("us_open", "us_open"), ("-3", -3),
])
def test_coerce_scalar(raw, expected):
    assert tl.coerce_scalar(raw) == expected
    assert type(tl.coerce_scalar(raw)) is type(expected)


def test_perturb_numeric_int_and_float():
    assert sorted(tl.perturb_numeric(20, 0.25, 1)) == [15, 25]      # step=5
    assert tl.perturb_numeric(0.5, 0.25, 1) == [0.375, 0.625]
    # zero sentinel and n_steps=0 → no perturbation
    assert tl.perturb_numeric(0, 0.25, 2) == []
    assert tl.perturb_numeric(20, 0.25, 0) == []
    # bool is not perturbed (it is a category, not a magnitude)
    assert tl.perturb_numeric(True, 0.25, 2) == []


def test_param_neighborhood_includes_live_and_range_sorted():
    n = tl.param_neighborhood(18, [10, 15, 20, 25], 0.25, 0)
    assert n == [10, 15, 18, 20, 25]           # live value injected, sorted
    assert 18 in n


def test_param_neighborhood_categorical_preserves_and_no_perturb():
    n = tl.param_neighborhood("us_open", ["asian", "us_open", "us_close"], 0.25, 2)
    assert set(n) == {"asian", "us_open", "us_close"}
    assert n[0] == "us_open"                    # live value first (insertion order)


def test_effective_params_config_over_defaults():
    eff = tl.effective_params({"fast_period": 18}, {"fast_period": 20, "slow_period": 50})
    assert eff == {"fast_period": 18, "slow_period": 50}


def test_build_search_grid_searched_frozen_override():
    eff = {"fast_period": 18, "slow_period": 50, "extra": 7}
    default_row = {"fast_period": [10, 15, 20, 25], "slow_period": [40, 50, 60, 80]}
    grid = tl.build_search_grid(
        eff, default_row, override_grids={"slow_period": [45, 55]},
        frozen={"fast_period"}, step_frac=0.25, n_steps=0, value_ok=lambda p, v: True)
    assert grid["fast_period"] == [18]          # frozen → pinned to live
    assert grid["slow_period"] == [45, 55]      # override replaces neighborhood
    assert grid["extra"] == [7]                 # not in default_row → pinned


def test_build_search_grid_value_ok_filters_but_keeps_live():
    eff = {"fast_period": 18}
    default_row = {"fast_period": [10, 15, 20, 25]}
    # value_ok rejects everything; the live value 18 must still survive.
    grid = tl.build_search_grid(eff, default_row, {}, set(), 0.25, 0,
                                value_ok=lambda p, v: False)
    assert grid["fast_period"] == [18]


def test_grid_size():
    assert tl.grid_size({"a": [1, 2, 3], "b": [1, 2], "c": [9]}) == 6


# ==========================================================================
# 2. Pure — override resolution (fail-loud), windows, patches, candidates
# ==========================================================================

def _check_value(reg):
    return lambda p, v: reg.validate_param_value("sma_crossover", p, v)


def test_parse_cli_param_grids():
    out = tl.parse_cli_param_grids(["fast_period=10,15,20", "num_std=1.5,2.0"])
    assert out == {"fast_period": [10, 15, 20], "num_std": [1.5, 2.0]}


def test_parse_cli_param_grids_malformed():
    with pytest.raises(ValueError, match="name=v1"):
        tl.parse_cli_param_grids(["fast_period"])


def test_resolve_overrides_valid_and_freeze():
    reg = load_registry("spot")
    og, fr = tl.resolve_overrides(
        "s", {"fast_period": [10, 15]},
        {"s": {"freeze": ["slow_period"]}}, {"fast_period", "slow_period"},
        _check_value(reg))
    assert og == {"fast_period": [10, 15]}
    assert fr == {"slow_period"}


def test_resolve_overrides_unknown_param_refused():
    reg = load_registry("spot")
    with pytest.raises(ValueError, match="unknown param"):
        tl.resolve_overrides("s", {"nope": [1]}, {}, {"fast_period"}, _check_value(reg))


def test_resolve_overrides_bad_value_refused_loudly():
    reg = load_registry("spot")
    with pytest.raises(ValueError, match="constraint"):
        tl.resolve_overrides("s", {"fast_period": [-3]}, {},
                             {"fast_period", "slow_period"}, _check_value(reg))


def test_resolve_overrides_freeze_and_override_conflict():
    reg = load_registry("spot")
    with pytest.raises(ValueError, match="both overridden and frozen"):
        tl.resolve_overrides("s", {"fast_period": [10]},
                             {"s": {"freeze": ["fast_period"]}},
                             {"fast_period", "slow_period"}, _check_value(reg))


def test_resolve_overrides_frozen_unknown_refused():
    reg = load_registry("spot")
    with pytest.raises(ValueError, match="unknown params"):
        tl.resolve_overrides("s", {}, {"s": {"freeze": ["ghost"]}},
                             {"fast_period"}, _check_value(reg))


def test_earliest_stage2_start_and_unknown_window():
    assert tl.earliest_stage2_start(["is", "oos"], tl.M1_WINDOWS) == "2025-06-10"
    # a held-out window pulls the disjoint boundary earlier
    assert tl.earliest_stage2_start(["2023", "is"], tl.M1_WINDOWS) == "2023-01-01"
    with pytest.raises(ValueError, match="unknown stage-2 window"):
        tl.earliest_stage2_start(["is", "nope"], tl.M1_WINDOWS)


def test_param_changes_and_build_patch():
    changes = tl.param_changes({"fast_period": 10, "slow_period": 50},
                               {"fast_period": 18, "slow_period": 50})
    assert changes == {"fast_period": 10}
    patch = tl.build_patch("s", "sma_crossover", {"fast_period": 10, "slow_period": 50},
                           {"fast_period": 18, "slow_period": 50})
    assert patch["strategy_id"] == "s"
    assert patch["open_strategy"] == {"name": "sma_crossover",
                                      "params": {"fast_period": 10, "slow_period": 50}}
    assert patch["param_changes"] == {"fast_period": 10}


def test_build_candidate_carries_close_direction_stops():
    resolution = {
        "strategy_type": "perps", "direction": "both",
        "close_strategies": [{"name": "tiered_tp_atr", "params": {}}],
        "stop_loss_atr_mult": 2.0, "regime_enabled": True,
        "allowed_regimes": ["trending_up"],
    }
    cand = tl.build_candidate("tema_cross_bd", {"short_period": 5}, resolution)
    assert cand["direction"] == "both"
    assert cand["type"] == "perps"
    assert cand["close_strategies"][0]["name"] == "tiered_tp_atr"
    assert cand["stop_loss_atr_mult"] == 2.0
    assert cand["allowed_regimes"] == ["trending_up"]
    # Live lookback defaults are made explicit so stage 2 can never drift.
    assert cand["regime_period"] == 14
    assert cand["regime_adx_threshold"] == 20.0


def test_build_candidate_dormant_gate_not_copied():
    # #1343 re-review: allowed_regimes with regime.enabled=false (the state
    # /apply-regime-gate reactivates) is UNGATED live and in stage 1 —
    # stage 2 must not force-activate it via a bare allowed_regimes copy.
    resolution = {
        "strategy_type": "perps", "direction": "long",
        "allowed_regimes": ["trending_up"], "regime_enabled": False,
        "regime_windows_spec": {"primary": {"classifier": "adx", "period": 14}},
    }
    cand = tl.build_candidate("sma_crossover", {"fast_period": 9}, resolution)
    assert "allowed_regimes" not in cand
    assert "regime_windows_spec" not in cand
    assert "regime_period" not in cand
    assert "regime_adx_threshold" not in cand


def test_build_candidate_threads_live_adx_lookback():
    # #1343 re-review: a non-default regime.period / adx_threshold must reach
    # stage 2, else eval_windows gates on ADX(14, 20) while live and stage 1
    # gate on the configured lookback.
    resolution = {
        "regime_enabled": True, "allowed_regimes": ["trending_up"],
        "regime_period": 21, "regime_adx_threshold": 25.0,
    }
    cand = tl.build_candidate("sma_crossover", {"fast_period": 9}, resolution)
    assert cand["allowed_regimes"] == ["trending_up"]
    assert cand["regime_period"] == 21
    assert cand["regime_adx_threshold"] == 25.0
    # The emitted candidate must pass the stage-2 schema it feeds.
    import eval_windows as ew
    assert ew.validate_candidate(dict(cand))


def test_build_candidate_composite_gate_owns_lookback():
    # An active composite gate carries its windows spec; the legacy lookback
    # fields must NOT ride along (validate_candidate rejects the mix).
    spec = {"primary": {"classifier": "composite", "period": 14}}
    resolution = {
        "regime_enabled": True, "allowed_regimes": ["trending_up"],
        "regime_period": 21, "regime_adx_threshold": 25.0,
        "regime_windows_spec": spec,
    }
    cand = tl.build_candidate("sma_crossover", {"fast_period": 9}, resolution)
    assert cand["allowed_regimes"] == ["trending_up"]
    assert cand["regime_windows_spec"] == spec
    assert "regime_period" not in cand
    assert "regime_adx_threshold" not in cand


def test_stage1_skip_composite_only_when_gate_active():
    spec = {"primary": {"classifier": "composite", "period": 14}}
    active = {"regime_enabled": True, "regime_windows_spec": spec}
    dormant = {"regime_enabled": False, "regime_windows_spec": spec}
    assert tl.stage1_skip_reason(active) == (
        "composite_regime_gate_unmodelable_in_walk_forward")
    assert tl.stage1_skip_reason(dormant) is None


@pytest.mark.parametrize("resolution,expected", [
    ({"stop_loss_atr_regime": {"x": 1}}, "unsupported_stop:stop_loss_atr_regime"),
    ({"stop_loss_pct": 0.05}, "unsupported_stop:stop_loss_pct"),
    ({"regime_directional_policy": {"x": 1}}, "unsupported_regime_directional_policy"),
    ({"profile_allocation": {"x": 1}}, "unsupported_profile_allocation"),
    ({"invert_signal": True}, "unsupported_invert_signal"),
    ({"risk_per_trade_pct": 1.0}, "unsupported_risk_per_trade_pct"),
    ({"allow_scale_in": True}, "unsupported_allow_scale_in"),
    ({"atr_method": "wilder"}, "unsupported_atr_method:wilder"),
    ({"regime_gate_on_failure": "closed"}, "unsupported_regime_gate_on_failure_closed"),
    ({"stop_loss_atr_mult": 2.0}, None),
    ({"atr_method": "simple"}, None),
    ({"regime_gate_on_failure": "open"}, None),
    ({}, None),
])
def test_unsupported_reason(resolution, expected):
    assert tl.unsupported_reason(resolution) == expected


@pytest.mark.parametrize("resolution,expected", [
    ({"direction": "short"}, "short_direction_long_only_seeder"),
    ({"direction": "long", "regime_enabled": True,
      "regime_windows_spec": {"d": {"classifier": "adx", "period": 14}}},
     "composite_regime_gate_unmodelable_in_walk_forward"),
    ({"direction": "long"}, None),
    ({"direction": "both"}, None),
])
def test_stage1_skip_reason(resolution, expected):
    assert tl.stage1_skip_reason(resolution) == expected


# ==========================================================================
# 3. Integration — baseline parity, selection-aware family size, artifact
# ==========================================================================

def test_baseline_params_byte_match_load_strategy_config(tmp_path):
    from run_backtest import load_strategy_config
    cfg = _sma_config(params={"fast_period": 18, "slow_period": 47})
    path = _write_config(tmp_path, cfg)
    resolved = load_strategy_config(path, "spot-sma-btc", inject_user_defaults=True)
    res = tl.tune_strategy(path, "spot-sma-btc", "BTC/USDT", "1d", "spot",
                           load_registry("spot"), _make_args(dry_run=True), {},
                           str(tmp_path))
    assert res["baseline_params"] == resolved["open_strategy"]["params"]


def test_stage2_spec_family_size_is_searched_N_not_survivor_count(tmp_path, monkeypatch):
    monkeypatch.setattr(tl, "load_cached_data", lambda *a, **k: _synthetic_df())
    monkeypatch.setattr(tl, "run_stage2", _canned_stage2)
    path = _write_config(tmp_path, _sma_config())
    res = tl.tune_strategy(path, "spot-sma-btc", "BTC/USDT", "1d", "spot",
                           load_registry("spot"), _make_args(), {}, str(tmp_path))
    assert res["status"] == "ranked"
    # searched N = grid size (5 fast x 4 slow = 20 for live fast=18 in [10,15,20,25])
    n = res["searched_family_size"]
    assert n == 20
    # far more than the stage-2 candidate count (baseline + few survivors)
    assert res["n_candidates"] < n
    spec = json.loads((tmp_path / "suggest.spot-sma-btc.json").read_text())
    assert spec["correction"]["family_size"] == n     # BH corrects against N
    assert spec["harnesses"] == ["m1_noise", "m1", "m3", "m5"]


def test_generated_candidates_pass_auto_suggest_load_spec(tmp_path):
    # A dry run emits the full-neighborhood spec; auto_suggest.load_spec must
    # accept every generated candidate (validate_candidate) without error.
    path = _write_config(tmp_path, _sma_config())
    tl.tune_strategy(path, "spot-sma-btc", "BTC/USDT", "1d", "spot",
                     load_registry("spot"), _make_args(dry_run=True), {}, str(tmp_path))
    spec_path = tmp_path / "suggest.spot-sma-btc.json"
    raw = json.loads(spec_path.read_text())
    spec = auto_suggest.load_spec(raw, str(tmp_path))    # raises on a bad candidate
    assert spec["correction"]["family_size"] == raw["correction"]["family_size"]
    assert spec["candidates"][0]["key"] == "baseline"


_PROMOTION_BASELINE_KEYS = (
    "open_strategy", "open_strategy_present",
    "user_defaults", "user_defaults_present",
    "user_close_defaults", "user_close_defaults_present",
)


def _assert_complete_promotion_baseline(baseline):
    assert isinstance(baseline, dict)
    assert set(baseline) == set(_PROMOTION_BASELINE_KEYS)
    assert isinstance(baseline["open_strategy_present"], bool)
    assert isinstance(baseline["user_defaults_present"], bool)
    assert isinstance(baseline["user_close_defaults_present"], bool)


def test_full_main_writes_versioned_artifact_and_progress(tmp_path, monkeypatch):
    monkeypatch.setattr(tl, "load_cached_data", lambda *a, **k: _synthetic_df())
    monkeypatch.setattr(tl, "run_stage2", _canned_stage2)
    path = _write_config(tmp_path, _sma_config())
    out = str(tmp_path / "art.json")
    rc = tl.main(["--config", path, "--strategy", "spot-sma-btc",
                  "--out-dir", str(tmp_path), "--json", out, "--jobs", "1"])
    assert rc == 0
    art = json.loads((tmp_path / "art.json").read_text())
    assert art["schema_version"] == 2
    assert art["schema_version"] == tl.SCHEMA_VERSION
    assert art["tool"] == "tune_live" and art["issue"] == 1338
    s = art["strategies"][0]
    assert s["status"] == "ranked"
    assert s["baseline_params"] == {"fast_period": 18, "slow_period": 50}
    _assert_complete_promotion_baseline(s["promotion_baseline"])
    assert s["promotion_baseline"]["open_strategy"] == {
        "name": "sma_crossover",
        "params": {"fast_period": 18, "slow_period": 50},
    }
    assert s["promotion_baseline"]["open_strategy_present"] is True
    assert s["promotion_baseline"]["user_defaults"] is None
    assert s["promotion_baseline"]["user_defaults_present"] is False
    assert s["promotion_baseline"]["user_close_defaults"] is None
    assert s["promotion_baseline"]["user_close_defaults_present"] is False
    surv = s["survivors"]
    assert surv and "patch" in surv[0]
    assert surv[0]["patch"]["open_strategy"]["name"] == "sma_crossover"
    # progress JSON is machine-consumable and reaches "done"
    prog = json.loads((tmp_path / "tune_live.progress.json").read_text())
    assert prog["schema_version"] == 2
    assert prog["schema_version"] == tl.SCHEMA_VERSION
    assert prog["phase"] == "done"


def test_promotion_baseline_raw_fidelity_with_user_defaults(tmp_path):
    """#1386: baseline stores file blocks verbatim — no close injection leak."""
    open_block = {
        "name": "sma_crossover",
        "params": {"fast_period": 18},  # slow_period omitted → registry fills eff
    }
    user_defaults = {
        "close": {
            "trailing_tp_ratchet": {
                "tp_tiers": [
                    {"atr_multiple": 2.0, "trailing_mult_after": 1.5,
                     "close_fraction": 0.5},
                ],
            },
        },
    }
    cfg = {
        "config_version": 15,
        "user_defaults": user_defaults,
        "strategies": [{
            "id": "spot-sma-btc", "type": "spot", "platform": "binanceus",
            "args": ["sma_crossover", "BTC/USDT", "1d"],
            "open_strategy": open_block,
            "close_strategy": {
                "name": "trailing_tp_ratchet",
                "params": {"use_defaults": True},
            },
            "trailing_stop_atr_mult": 3.0,
        }],
    }
    path = _write_config(tmp_path, cfg)
    file_cfg = json.loads((tmp_path / "config.json").read_text())

    res = tl.tune_strategy(path, "spot-sma-btc", "BTC/USDT", "1d", "spot",
                           load_registry("spot"), _make_args(dry_run=True), {},
                           str(tmp_path))
    baseline = res["promotion_baseline"]
    _assert_complete_promotion_baseline(baseline)
    assert baseline["open_strategy"] == file_cfg["strategies"][0]["open_strategy"]
    assert baseline["user_defaults"] == file_cfg["user_defaults"]
    assert baseline["open_strategy_present"] is True
    assert baseline["user_defaults_present"] is True
    # No injected close data inside the baseline — close injection lives on
    # close_strategies / stop_owner evidence fields only.
    assert baseline["open_strategy"]["params"] == res["baseline_params"]
    # Registry defaults fill omitted keys in effective_params, not baseline.
    defaults = load_registry("spot").STRATEGY_REGISTRY["sma_crossover"]["default_params"]
    eff = tl.effective_params(res["baseline_params"], defaults)
    assert "slow_period" in eff and "slow_period" not in res["baseline_params"]
    # Injected close lands on evidence, not on the raw baseline.
    assert res["close_strategies"][0]["params"].get("tp_tiers") is not None
    assert baseline["user_defaults"] == user_defaults


def test_promotion_baseline_presence_matrix(tmp_path):
    """#1386: absent → null+false; empty object → {}+true; legacy-only captured."""
    from run_backtest import load_strategy_config

    def _load(subdir, cfg, sid, **kw):
        d = tmp_path / subdir
        d.mkdir()
        return load_strategy_config(_write_config(d, cfg), sid,
                                    include_promotion_baseline=True, **kw)

    # Absent open_strategy key (args-form) → null + false; name still resolves.
    kwargs = _load("args", {
        "config_version": 15,
        "strategies": [{
            "id": "spot-args", "type": "spot", "platform": "binanceus",
            "args": ["sma_crossover", "BTC/USDT", "1d"],
        }],
    }, "spot-args")
    b = kwargs["promotion_baseline"]
    _assert_complete_promotion_baseline(b)
    assert b["open_strategy"] is None and b["open_strategy_present"] is False
    assert b["user_defaults"] is None and b["user_defaults_present"] is False
    assert kwargs["open_strategy"]["name"] == "sma_crossover"  # resolved

    # Empty-object user_defaults → {} + true.
    b = _load("empty_ud", {
        "config_version": 15,
        "user_defaults": {},
        "strategies": [{
            "id": "spot-empty-ud", "type": "spot",
            "args": ["sma_crossover", "BTC/USDT", "1d"],
            "open_strategy": {"name": "sma_crossover", "params": {}},
        }],
    }, "spot-empty-ud")["promotion_baseline"]
    assert b["user_defaults"] == {} and b["user_defaults_present"] is True

    # Present-but-null open_strategy → null + true (key present).
    b = _load("null_open", {
        "config_version": 15,
        "strategies": [{
            "id": "spot-null-open", "type": "spot",
            "args": ["sma_crossover", "BTC/USDT", "1d"],
            "open_strategy": None,
        }],
    }, "spot-null-open")["promotion_baseline"]
    assert b["open_strategy"] is None and b["open_strategy_present"] is True

    # Args-form with empty open_strategy.name — raw stores empty name, not resolved.
    kwargs = _load("empty_name", {
        "config_version": 15,
        "strategies": [{
            "id": "spot-empty-name", "type": "spot",
            "args": ["sma_crossover", "BTC/USDT", "1d"],
            "open_strategy": {"name": "", "params": {"fast_period": 18}},
        }],
    }, "spot-empty-name")
    b = kwargs["promotion_baseline"]
    assert b["open_strategy"] == {"name": "", "params": {"fast_period": 18}}
    assert kwargs["open_strategy"]["name"] == "sma_crossover"

    # Legacy-only user_close_defaults — canonical absent, legacy present.
    legacy = {
        "trailing_tp_ratchet": {
            "tp_tiers": [
                {"atr_multiple": 2.0, "trailing_mult_after": 1.5,
                 "close_fraction": 0.5},
            ],
        },
    }
    b = _load("legacy", {
        "config_version": 15,
        "user_close_defaults": legacy,
        "strategies": [{
            "id": "hl-legacy", "type": "perps", "platform": "hyperliquid",
            "args": ["tema_cross_bd", "BTC", "1h"],
            "open_strategy": {"name": "tema_cross_bd", "params": {}},
            "trailing_stop_atr_mult": 3.0,
            "close_strategy": {
                "name": "trailing_tp_ratchet",
                "params": {"use_defaults": True},
            },
        }],
    }, "hl-legacy", inject_user_defaults=True)["promotion_baseline"]
    assert b["user_defaults"] is None and b["user_defaults_present"] is False
    assert b["user_close_defaults"] == legacy
    assert b["user_close_defaults_present"] is True


def test_promotion_baseline_opt_in_spread_contract(tmp_path):
    """#1386 Finding 1: without the flag, no promotion_baseline key (Backtester-safe)."""
    from run_backtest import load_strategy_config
    path = _write_config(tmp_path, _sma_config())
    kwargs = load_strategy_config(path, "spot-sma-btc")
    assert "promotion_baseline" not in kwargs
    kwargs_flag = load_strategy_config(path, "spot-sma-btc",
                                       include_promotion_baseline=True)
    assert "promotion_baseline" in kwargs_flag
    _assert_complete_promotion_baseline(kwargs_flag["promotion_baseline"])


def test_promotion_baseline_deepcopy_no_alias(tmp_path):
    """#1386: mutating the baseline must not touch other resolution fields."""
    from run_backtest import load_strategy_config
    path = _write_config(tmp_path, _sma_config(
        params={"fast_period": 18, "slow_period": 50}))
    kwargs = load_strategy_config(path, "spot-sma-btc",
                                  include_promotion_baseline=True)
    baseline = kwargs["promotion_baseline"]
    baseline["open_strategy"]["params"]["fast_period"] = 999
    baseline["open_strategy"]["name"] = "mutated"
    assert kwargs["open_strategy"]["params"]["fast_period"] == 18
    assert kwargs["open_strategy"]["name"] == "sma_crossover"
    # Result-side: tune_strategy also deepcopies via the loader capture.
    res = tl.tune_strategy(path, "spot-sma-btc", "BTC/USDT", "1d", "spot",
                           load_registry("spot"), _make_args(dry_run=True), {},
                           str(tmp_path))
    res["promotion_baseline"]["open_strategy"]["params"]["fast_period"] = 111
    assert res["baseline_params"]["fast_period"] == 18


# ==========================================================================
# 4. Integration — skip / failure / refusal paths
# ==========================================================================

def test_dry_run_emits_spec_without_running_stage2(tmp_path, monkeypatch):
    def _boom(*a, **k):
        raise AssertionError("run_stage2 must not run in --dry-run")
    monkeypatch.setattr(tl, "run_stage2", _boom)
    # load_cached_data must ALSO not be touched in dry-run (no stage 1).
    monkeypatch.setattr(tl, "load_cached_data",
                        lambda *a, **k: (_ for _ in ()).throw(
                            AssertionError("no data fetch in --dry-run")))
    path = _write_config(tmp_path, _sma_config())
    res = tl.tune_strategy(path, "spot-sma-btc", "BTC/USDT", "1d", "spot",
                           load_registry("spot"), _make_args(dry_run=True), {},
                           str(tmp_path))
    assert res["status"] == "dry_run"
    assert res["stage1"]["ran"] is False
    assert (tmp_path / "suggest.spot-sma-btc.json").exists()


def test_stage1_failed_marks_and_fleet_continues(tmp_path, monkeypatch):
    # walk-forward errors for sma, succeeds for ema → sma is stage1_failed, ema
    # is ranked, and BOTH appear (the fleet run did not abort).
    monkeypatch.setattr(tl, "load_cached_data", lambda *a, **k: _synthetic_df())
    monkeypatch.setattr(tl, "run_stage2", _canned_stage2)

    def fake_wfo(df, name, grid, **kw):
        if name == "sma_crossover":
            return {"error": "No valid optimization windows", "strategy": name}
        return {"n_valid_folds": 3, "param_grid_size": tl.grid_size(grid),
                "window_results": [{"best_params": {"fast_period": 8, "slow_period": 21}}]}
    monkeypatch.setattr(tl, "walk_forward_optimize", fake_wfo)

    cfg = {"config_version": 15, "strategies": [
        {"id": "a-sma", "type": "spot", "args": ["sma_crossover", "BTC/USDT", "1d"],
         "open_strategy": {"name": "sma_crossover", "params": {"fast_period": 18, "slow_period": 50}}},
        {"id": "b-ema", "type": "spot", "args": ["ema_crossover", "BTC/USDT", "1d"],
         "open_strategy": {"name": "ema_crossover", "params": {"fast_period": 12, "slow_period": 26}}},
    ]}
    path = _write_config(tmp_path, cfg)
    out = str(tmp_path / "art.json")
    tl.main(["--config", path, "--out-dir", str(tmp_path), "--json", out, "--jobs", "1"])
    art = json.loads((tmp_path / "art.json").read_text())
    by_id = {s["strategy_id"]: s for s in art["strategies"]}
    assert by_id["a-sma"]["status"] == "stage1_failed"
    assert by_id["b-ema"]["status"] == "ranked"


def test_short_direction_skips_stage1(tmp_path, monkeypatch):
    monkeypatch.setattr(tl, "run_stage2", _canned_stage2)
    # data must NOT be fetched — stage 1 is skipped for a short strategy.
    monkeypatch.setattr(tl, "load_cached_data",
                        lambda *a, **k: (_ for _ in ()).throw(
                            AssertionError("short skips stage 1 — no data fetch")))
    cfg = {"config_version": 15, "strategies": [{
        "id": "hl-sma-short", "type": "perps", "platform": "hyperliquid",
        "args": ["sma_crossover", "BTC/USDT", "1d"], "direction": "short",
        "open_strategy": {"name": "sma_crossover", "params": {"fast_period": 18, "slow_period": 50}},
    }]}
    path = _write_config(tmp_path, cfg)
    res = tl.tune_strategy(path, "hl-sma-short", "BTC/USDT", "1d", "spot",
                           load_registry("spot"), _make_args(), {}, str(tmp_path))
    assert res["status"] == "ranked"
    assert res["stage1"]["ran"] is False
    assert res["stage1"]["skipped_reason"] == "short_direction_long_only_seeder"
    # full neighborhood became the candidate set
    assert res["n_candidates"] == res["searched_family_size"]
    # no cross-param-invalid combos in this grid → nothing dropped, no note
    assert not any("constraint-invalid" in n for n in res["notes"])


def test_constraint_invalid_combos_dropped_on_skip_path(tmp_path, monkeypatch):
    # #1343 re-review optional: an operator --param grid sweeping BOTH operands
    # of fast_period < slow_period on a stage-1-skipped (short) strategy must
    # not send guaranteed-invalid combos to stage 2, count them in the BH N,
    # or let them consume --max-candidates.
    monkeypatch.setattr(tl, "run_stage2", _canned_stage2)
    monkeypatch.setattr(tl, "load_cached_data",
                        lambda *a, **k: (_ for _ in ()).throw(
                            AssertionError("short skips stage 1 — no data fetch")))
    cfg = {"config_version": 15, "strategies": [{
        "id": "hl-sma-short", "type": "perps", "platform": "hyperliquid",
        "args": ["sma_crossover", "BTC/USDT", "1d"], "direction": "short",
        "open_strategy": {"name": "sma_crossover",
                          "params": {"fast_period": 18, "slow_period": 50}},
    }]}
    path = _write_config(tmp_path, cfg)
    args = _make_args(param=["fast_period=30,45,60", "slow_period=40,50"])
    res = tl.tune_strategy(path, "hl-sma-short", "BTC/USDT", "1d", "spot",
                           load_registry("spot"), args, {}, str(tmp_path))
    assert res["status"] == "ranked"
    # 3 of the 6 cartesian combos violate fast_period < slow_period
    assert any("3 constraint-invalid" in n for n in res["notes"])
    # baseline (18/50) is off the overridden fast axis → +1 extra hypothesis;
    # N counts only the 3 valid combos, not the cartesian 6
    assert res["searched_family_size"] == 4
    assert res["n_candidates"] == 4
    spec = json.loads((tmp_path / "suggest.hl-sma-short.json").read_text())
    for c in spec["candidates"]:
        p = c["candidate"]["params"]
        assert p["fast_period"] < p["slow_period"]


def test_constraint_invalid_combos_dropped_from_auto_neighborhood(tmp_path, monkeypatch):
    # Auto-neighborhood variant: a live baseline whose fast/slow sweeps overlap
    # (fast up-steps reach the slow down-steps) produces cross-invalid combos;
    # none may reach stage 2 and the BH N must count only the valid ones.
    monkeypatch.setattr(tl, "run_stage2", _canned_stage2)
    monkeypatch.setattr(tl, "load_cached_data",
                        lambda *a, **k: (_ for _ in ()).throw(
                            AssertionError("short skips stage 1 — no data fetch")))
    cfg = {"config_version": 15, "strategies": [{
        "id": "hl-sma-short", "type": "perps", "platform": "hyperliquid",
        "args": ["sma_crossover", "BTC/USDT", "1d"], "direction": "short",
        "open_strategy": {"name": "sma_crossover",
                          "params": {"fast_period": 40, "slow_period": 50}},
    }]}
    path = _write_config(tmp_path, cfg)
    res = tl.tune_strategy(path, "hl-sma-short", "BTC/USDT", "1d", "spot",
                           load_registry("spot"),
                           _make_args(neighborhood_steps=1, step_frac=0.25),
                           {}, str(tmp_path))
    assert res["status"] == "ranked"
    assert any("constraint-invalid" in n for n in res["notes"])
    # baseline is always on an auto-neighborhood grid and live-valid → no +1,
    # and every candidate (baseline + valid combos) satisfies the constraint
    assert res["n_candidates"] == res["searched_family_size"]
    spec = json.loads((tmp_path / "suggest.hl-sma-short.json").read_text())
    for c in spec["candidates"]:
        p = c["candidate"]["params"]
        assert p["fast_period"] < p["slow_period"]


def test_bidirectional_runs_stage1_with_direction_both(tmp_path, monkeypatch):
    monkeypatch.setattr(tl, "load_cached_data", lambda *a, **k: _synthetic_df())
    monkeypatch.setattr(tl, "run_stage2", _canned_stage2)
    seen = {}

    def fake_wfo(df, name, grid, **kw):
        seen["direction"] = kw.get("direction")
        return {"n_valid_folds": 3, "param_grid_size": tl.grid_size(grid),
                "window_results": [{"best_params": {"fast_period": 8, "slow_period": 21}}]}
    monkeypatch.setattr(tl, "walk_forward_optimize", fake_wfo)

    cfg = {"config_version": 15, "strategies": [{
        "id": "hl-sma-both", "type": "perps", "platform": "hyperliquid",
        "args": ["sma_crossover", "BTC/USDT", "1d"], "direction": "both",
        "open_strategy": {"name": "sma_crossover", "params": {"fast_period": 18, "slow_period": 50}},
        "close_strategy": {"name": "tiered_tp_atr", "params": {"tp_tiers": [
            {"atr_multiple": 2.0, "close_fraction": 1.0}]}},
    }]}
    path = _write_config(tmp_path, cfg)
    res = tl.tune_strategy(path, "hl-sma-both", "BTC/USDT", "1d", "spot",
                           load_registry("spot"), _make_args(), {}, str(tmp_path))
    assert res["status"] == "ranked"
    assert res["stage1"]["ran"] is True
    assert seen["direction"] == "both"


def test_disjoint_slice_no_data_refuses(monkeypatch):
    # A frame that begins after the earliest stage-2 window start leaves too few
    # disjoint bars → loud refusal, never a silent overlap.
    small = pd.DataFrame(
        {"open": 100.0, "high": 101.0, "low": 99.0, "close": 100.0, "volume": 1000.0},
        index=pd.date_range("2025-05-20", "2026-01-01", freq="1D"))
    monkeypatch.setattr(tl, "load_cached_data", lambda *a, **k: small)
    with pytest.raises(ValueError, match="disjoint slice leaves only"):
        tl.run_stage1("sma_crossover", {"fast_period": [10, 20]},
                      {"direction": "long"}, "BTC/USDT", "1d", "spot",
                      "2025-06-10", 5, 1000.0, "sharpe_ratio", False)


def test_pre_v15_config_error_status(tmp_path):
    cfg = _sma_config()
    cfg["config_version"] = 13
    path = _write_config(tmp_path, cfg)
    res = tl.tune_strategy(path, "spot-sma-btc", "BTC/USDT", "1d", "spot",
                           load_registry("spot"), _make_args(dry_run=True), {},
                           str(tmp_path))
    assert res["status"] == "config_error"
    assert "config_version" in res["error"] or "v15" in res["error"]


def test_config_strategy_entries_uses_trade_timeframe_not_regime(tmp_path):
    # #1338 review finding 1: the strategy trades on args[2] (4h); a global
    # regime.timeframe (1d) must NOT become the trade interval.
    cfg = {"config_version": 15, "regime": {"enabled": True, "timeframe": "1d"},
           "strategies": [{"id": "s", "type": "spot",
                           "args": ["sma_crossover", "BTC/USDT", "4h"],
                           "open_strategy": {"name": "sma_crossover", "params": {}}}]}
    path = _write_config(tmp_path, cfg)
    entries = tl.config_strategy_entries(path, None)
    assert entries == [("s", "BTC/USDT", "4h", "spot")]


def test_config_strategy_entries_accepts_ordered_strategy_subset(tmp_path):
    cfg = _sma_config(sid="first")
    second = dict(cfg["strategies"][0])
    second["id"] = "second"
    second["args"] = ["sma_crossover", "ETH/USDT", "1h"]
    cfg["strategies"].append(second)
    path = _write_config(tmp_path, cfg)

    entries = tl.config_strategy_entries(path, ["second", "first"])

    assert entries == [
        ("second", "ETH/USDT", "1h", "spot"),
        ("first", "BTC/USDT", "1d", "spot"),
    ]


def test_config_strategy_entries_rejects_duplicate_strategy_subset(tmp_path):
    path = _write_config(tmp_path, _sma_config(sid="first"))
    with pytest.raises(ValueError, match="duplicate strategy id"):
        tl.config_strategy_entries(path, ["first", "first"])


def test_main_auto_resolves_mixed_registry_per_strategy(tmp_path, monkeypatch):
    cfg = _sma_config(sid="spot-one")
    futures = dict(cfg["strategies"][0])
    futures.update({
        "id": "futures-one",
        "type": "futures",
        "platform": "topstep",
        "args": ["breakout", "ES", "1h"],
        "open_strategy": {"name": "breakout", "params": {}},
    })
    cfg["strategies"].append(futures)
    path = _write_config(tmp_path, cfg)
    loaded = []
    tuned = []

    def fake_load_registry(registry):
        loaded.append(registry)
        return types.SimpleNamespace(name=registry)

    def fake_tune(config_path, sid, symbol, timeframe, registry, reg_mod,
                  args, overrides, out_dir):
        tuned.append((sid, registry, reg_mod.name))
        return {"strategy_id": sid, "status": "dry_run"}

    monkeypatch.setattr(tl, "load_registry", fake_load_registry)
    monkeypatch.setattr(tl, "tune_strategy", fake_tune)
    rc = tl.main(["--config", path, "--out-dir", str(tmp_path),
                  "--json", str(tmp_path / "mixed.json"), "--dry-run"])

    assert rc == 0
    assert loaded == ["spot", "futures"]
    assert tuned == [
        ("spot-one", "spot", "spot"),
        ("futures-one", "futures", "futures"),
    ]


@pytest.mark.parametrize("override", ["spot", "futures"])
def test_main_explicit_registry_overrides_whole_mixed_run(
        tmp_path, monkeypatch, override):
    cfg = _sma_config(sid="spot-one")
    perps = dict(cfg["strategies"][0])
    perps.update({"id": "perps-one", "type": "perps"})
    cfg["strategies"].append(perps)
    path = _write_config(tmp_path, cfg)
    loaded = []
    tuned = []

    monkeypatch.setattr(tl, "load_registry", lambda registry: (
        loaded.append(registry) or types.SimpleNamespace(name=registry)))
    monkeypatch.setattr(tl, "tune_strategy", lambda config_path, sid, symbol,
                        timeframe, registry, reg_mod, args, overrides, out_dir: (
                            tuned.append((sid, registry)) or
                            {"strategy_id": sid, "status": "dry_run"}))

    rc = tl.main(["--config", path, "--registry", override,
                  "--out-dir", str(tmp_path),
                  "--json", str(tmp_path / f"{override}.json"), "--dry-run"])

    assert rc == 0
    assert loaded == [override]
    assert tuned == [("spot-one", override), ("perps-one", override)]


def test_regime_timeframe_mismatch_skipped_unsupported(tmp_path):
    # regime enabled with regime.timeframe (1d) != trade tf (4h) → neither stage
    # can thread a separate regime interval → unsupported, never a tf swap.
    cfg = {"config_version": 15,
           "regime": {"enabled": True, "timeframe": "1d", "period": 14},
           "strategies": [{"id": "hl-sma", "type": "perps", "platform": "hyperliquid",
                           "args": ["sma_crossover", "BTC/USDT", "4h"],
                           "allowed_regimes": ["trending"],
                           "open_strategy": {"name": "sma_crossover", "params": {}}}]}
    path = _write_config(tmp_path, cfg)
    res = tl.tune_strategy(path, "hl-sma", "BTC/USDT", "4h", "spot",
                           load_registry("spot"), _make_args(dry_run=True), {},
                           str(tmp_path))
    assert res["status"] == "unsupported"
    assert res["reason"].startswith("unsupported_regime_timeframe_mismatch")


def test_regime_disabled_stale_timeframe_still_supported(tmp_path):
    # regime.enabled=false with a stale regime.timeframe present must NOT skip —
    # the trade tf (args[2]) governs and there is no active regime interval.
    cfg = {"config_version": 15,
           "regime": {"enabled": False, "timeframe": "1d"},
           "strategies": [{"id": "s", "type": "spot",
                           "args": ["sma_crossover", "BTC/USDT", "4h"],
                           "open_strategy": {"name": "sma_crossover", "params": {}}}]}
    path = _write_config(tmp_path, cfg)
    res = tl.tune_strategy(path, "s", "BTC/USDT", "4h", "spot",
                           load_registry("spot"), _make_args(dry_run=True), {},
                           str(tmp_path))
    assert res["status"] == "dry_run"       # not skipped


@pytest.mark.parametrize("atr_key,atr_val", [("atr_method", "wilder")])
def test_wilder_atr_method_skipped_unsupported(tmp_path, atr_key, atr_val):
    # #1338 review finding 3: a wilder-ATR strategy can't be replayed under the
    # simple-ATR engines — skip rather than tune on the wrong geometry. Set it
    # globally so no per-strategy override is needed.
    cfg = {"config_version": 15, atr_key: atr_val,
           "strategies": [{"id": "hl-sma", "type": "perps", "platform": "hyperliquid",
                           "args": ["sma_crossover", "BTC/USDT", "1d"],
                           "open_strategy": {"name": "sma_crossover", "params": {}}}]}
    path = _write_config(tmp_path, cfg)
    res = tl.tune_strategy(path, "hl-sma", "BTC/USDT", "1d", "spot",
                           load_registry("spot"), _make_args(dry_run=True), {},
                           str(tmp_path))
    assert res["status"] == "unsupported"
    assert res["reason"] == "unsupported_atr_method:wilder"


def test_all_failed_fleet_returns_nonzero(tmp_path):
    # #1338 review (judgment call): a fleet where EVERY strategy fails must exit
    # non-zero so a scheduler/CI consumer catches a total wipeout.
    cfg = {"config_version": 13,   # pre-v15 → config_error for both
           "strategies": [
               {"id": "a", "type": "spot", "args": ["sma_crossover", "BTC/USDT", "1d"],
                "open_strategy": {"name": "sma_crossover", "params": {}}},
               {"id": "b", "type": "spot", "args": ["ema_crossover", "BTC/USDT", "1d"],
                "open_strategy": {"name": "ema_crossover", "params": {}}},
           ]}
    path = _write_config(tmp_path, cfg)
    rc = tl.main(["--config", path, "--out-dir", str(tmp_path),
                  "--json", str(tmp_path / "art.json"), "--dry-run"])
    assert rc == 1
    art = json.loads((tmp_path / "art.json").read_text())
    assert all(s["status"] == "config_error" for s in art["strategies"])


def test_mixed_fleet_with_one_success_returns_zero(tmp_path, monkeypatch):
    # One good strategy + one failing one → exit 0 (partial success; the artifact
    # carries the per-strategy statuses).
    monkeypatch.setattr(tl, "load_cached_data", lambda *a, **k: _synthetic_df())
    monkeypatch.setattr(tl, "run_stage2", _canned_stage2)
    cfg = {"config_version": 15, "strategies": [
        {"id": "good", "type": "spot", "args": ["sma_crossover", "BTC/USDT", "1d"],
         "open_strategy": {"name": "sma_crossover", "params": {"fast_period": 18, "slow_period": 50}}},
        {"id": "bad-stop", "type": "perps", "platform": "hyperliquid",
         "args": ["sma_crossover", "ETH/USDT", "1d"], "stop_loss_pct": 0.05,
         "open_strategy": {"name": "sma_crossover", "params": {}}},
    ]}
    path = _write_config(tmp_path, cfg)
    rc = tl.main(["--config", path, "--out-dir", str(tmp_path),
                  "--json", str(tmp_path / "art.json"), "--jobs", "1"])
    assert rc == 0
    art = json.loads((tmp_path / "art.json").read_text())
    by_id = {s["strategy_id"]: s["status"] for s in art["strategies"]}
    assert by_id["good"] == "ranked" and by_id["bad-stop"] == "unsupported"


def test_all_benign_skip_fleet_returns_zero(tmp_path):
    # A fleet of only benignly-declined (unsupported) strategies is not a
    # failure — nothing went wrong, there was just nothing to tune.
    cfg = {"config_version": 15, "strategies": [{
        "id": "hl-sma", "type": "perps", "platform": "hyperliquid",
        "args": ["sma_crossover", "BTC/USDT", "1d"], "stop_loss_pct": 0.05,
        "open_strategy": {"name": "sma_crossover", "params": {}}}]}
    path = _write_config(tmp_path, cfg)
    rc = tl.main(["--config", path, "--out-dir", str(tmp_path),
                  "--json", str(tmp_path / "art.json"), "--dry-run"])
    assert rc == 0


def test_unsupported_stop_owner_skipped(tmp_path):
    cfg = {"config_version": 15, "strategies": [{
        "id": "hl-sma", "type": "perps", "platform": "hyperliquid",
        "args": ["sma_crossover", "BTC/USDT", "1d"],
        "open_strategy": {"name": "sma_crossover", "params": {}},
        "stop_loss_pct": 0.05,
    }]}
    path = _write_config(tmp_path, cfg)
    res = tl.tune_strategy(path, "hl-sma", "BTC/USDT", "1d", "spot",
                           load_registry("spot"), _make_args(dry_run=True), {},
                           str(tmp_path))
    assert res["status"] == "unsupported"
    assert res["reason"] == "unsupported_stop:stop_loss_pct"


def test_overrides_unknown_strategy_id_refused(tmp_path):
    path = _write_config(tmp_path, _sma_config())
    ov = tmp_path / "ov.json"
    ov.write_text(json.dumps({"ghost-strategy": {"params": {"fast_period": [10]}}}))
    with pytest.raises(SystemExit, match="unknown strategy ids"):
        tl.main(["--config", path, "--overrides", str(ov),
                 "--out-dir", str(tmp_path), "--dry-run"])


def test_override_grid_counts_toward_searched_family(tmp_path, monkeypatch):
    # An operator override REPLACES the param's neighborhood and its values are
    # counted in the searched family N exactly like auto-derived ones. Because
    # this override EXCLUDES the live value (fast=18, slow=50), the baseline is
    # an extra candidate not on the grid, so N = grid (6) + 1 baseline = 7
    # (#1338 review finding 2).
    monkeypatch.setattr(tl, "load_cached_data", lambda *a, **k: _synthetic_df())
    monkeypatch.setattr(tl, "run_stage2", _canned_stage2)
    path = _write_config(tmp_path, _sma_config())
    args = _make_args(param=["fast_period=10,12,14", "slow_period=40,60"])
    res = tl.tune_strategy(path, "spot-sma-btc", "BTC/USDT", "1d", "spot",
                           load_registry("spot"), args, {}, str(tmp_path))
    assert res["status"] == "ranked"
    # override grids: fast {10,12,14} x slow {40,60} = 6, + baseline = 7
    assert res["searched_family_size"] == 7
    assert res["neighborhood"]["fast_period"] == [10, 12, 14]


def test_family_size_covers_baseline_against_real_bh_guard(tmp_path, monkeypatch):
    # #1338 review finding 2 regression: a live-EXCLUDING override on a
    # stage-1-skipped (short) strategy makes the baseline an extra stage-2
    # candidate. The emitted family_size must cover every candidate so the REAL
    # auto_suggest BH guard (family_size >= len(pvals)) does not trip. Stage 2 is
    # stubbed to actually invoke that guard with one p-value per candidate.
    def stage2_with_real_guard(spec_path, out_json, out_dir, jobs):
        spec = json.loads(open(spec_path).read())
        fam = spec["correction"]["family_size"]
        n_cand = len(spec["candidates"])
        assert fam >= n_cand, f"family_size {fam} < candidate count {n_cand}"
        # one primary p-value per candidate — the worst case; must NOT raise
        auto_suggest.apply_family_correction(
            [{"p": 0.5} for _ in range(n_cand)], 0.05, family_size=fam)
        ranked = [{"key": c["key"], "verdict": "incumbent_stands",
                   "candidate": c["candidate"], "evidence": {}, "limitations": []}
                  for c in spec["candidates"]]
        return {"correction": {"m": fam, "tests_run": n_cand, "n_survivors": 0},
                "ranked": ranked, "_exit_code": 0}
    monkeypatch.setattr(tl, "run_stage2", stage2_with_real_guard)
    cfg = {"config_version": 15, "strategies": [{
        "id": "hl-rsi-short", "type": "perps", "platform": "hyperliquid",
        "args": ["rsi", "BTC/USDT", "1d"], "direction": "short",
        "open_strategy": {"name": "rsi", "params": {"period": 14}},
    }]}
    path = _write_config(tmp_path, cfg)
    # override EXCLUDES the live period=14 → baseline is an extra candidate
    args = _make_args(param=["period=10,20,28"])
    res = tl.tune_strategy(path, "hl-rsi-short", "BTC/USDT", "1d", "spot",
                           load_registry("spot"), args, {}, str(tmp_path))
    assert res["status"] == "ranked"        # did not die on the BH guard
    assert res["searched_family_size"] >= res["n_candidates"]


def test_too_many_candidates_refused(tmp_path):
    # A wide override grid past the cap is refused loudly (never silently
    # truncated) — the operator is told to narrow.
    path = _write_config(tmp_path, _sma_config())
    args = _make_args(dry_run=True, max_candidates=3,
                      param=["fast_period=10,15,20,25", "slow_period=40,50,60,80"])
    res = tl.tune_strategy(path, "spot-sma-btc", "BTC/USDT", "1d", "spot",
                           load_registry("spot"), args, {}, str(tmp_path))
    assert res["status"] == "too_many_candidates"
    assert "max-candidates" in res["error"]
