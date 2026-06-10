"""#641: Backtester accepts co-located strategy refs (name + params), CLI
parses both bare-name and JSON-ref forms, and load_strategy_config produces
ready-to-use kwargs from a live go-trader config.

These tests pin the ref-shape contract on the Python side after #641 (mirror
of the Go-side StrategyRef change in #640) so a future refactor that loses
per-ref params or breaks the live-config import path fails immediately.
"""
import json

import pandas as pd
import pytest

from backtester import Backtester
import run_backtest


def _flat_df():
    """3-bar flat DataFrame; Backtester runs but produces no signal action."""
    return pd.DataFrame(
        {
            "open":   [100, 100, 100],
            "high":   [101, 101, 101],
            "low":    [ 99,  99,  99],
            "close":  [100, 100, 100],
            "volume": [1000, 1000, 1000],
            "signal": [0, 0, 0],
        },
        index=pd.date_range("2024-01-01", periods=3, freq="D"),
    )


# ─── Backtester accepts ref shape ────────────────────────────────────────────


def test_backtester_accepts_open_strategy_ref():
    bt = Backtester(
        initial_capital=1000,
        open_strategy={"name": "tema_cross_bd", "params": {"short_period": 5}},
    )
    # Run records the open ref on the result for parity with live config.
    df = _flat_df()
    result = bt.run(df, save=False)
    assert result["open_strategy"]["name"] == "tema_cross_bd"
    assert result["open_strategy"]["params"]["short_period"] == 5


def test_backtester_accepts_close_strategy_ref_with_params():
    """Close ref params must reach the eval loop's per-name params dict
    (mirrors how the live scheduler passes per-ref params, post #640)."""
    bt = Backtester(
        initial_capital=1000,
        close_strategies=[
            {"name": "tiered_tp_atr", "params": {"tp_tiers": [
                {"atr_multiple": 2.0, "close_fraction": 1.0},
            ]}},
        ],
    )
    assert bt.close_strategies == ["tiered_tp_atr"]
    assert bt.close_params["tiered_tp_atr"]["tp_tiers"] == [
        {"atr_multiple": 2.0, "close_fraction": 1.0}
    ]


def test_backtester_close_strategies_records_refs_on_result():
    bt = Backtester(
        initial_capital=1000,
        close_strategies=[
            {"name": "tp_at_pct", "params": {"pct": 0.05}},
            {"name": "tiered_tp_pct"},
        ],
    )
    result = bt.run(_flat_df(), save=False)
    refs = result["close_strategies"]
    assert [r["name"] for r in refs] == ["tiered_tp_pct", "tiered_tp_pct"]
    assert refs[0]["params"] == {
        "tp_tiers": [{"profit_pct": 0.05, "close_fraction": 1.0}],
    }
    assert refs[1]["params"] == {}


def test_backtester_rejects_close_strategy_without_name():
    with pytest.raises(ValueError, match="missing 'name'"):
        Backtester(close_strategies=[{"params": {"pct": 0.03}}])


def test_backtester_rejects_close_strategy_non_dict():
    with pytest.raises(ValueError, match="must be dicts"):
        Backtester(close_strategies=["bare_string_no_longer_supported"])


# ─── CLI: --close-strategy parses bare names and JSON refs ───────────────────


def test_parse_close_strategy_arg_bare_name():
    ref = run_backtest._parse_close_strategy_arg("tp_at_pct")
    assert ref == {"name": "tp_at_pct", "params": {}}


def test_parse_close_strategy_arg_json_with_params():
    ref = run_backtest._parse_close_strategy_arg(
        '{"name": "tiered_tp_atr", "params": {"tp_tiers": [{"atr_multiple": 2.0}]}}'
    )
    assert ref["name"] == "tiered_tp_atr"
    assert ref["params"]["tp_tiers"][0]["atr_multiple"] == 2.0


def test_parse_close_strategy_arg_json_without_params():
    ref = run_backtest._parse_close_strategy_arg('{"name": "tiered_tp_pct"}')
    assert ref == {"name": "tiered_tp_pct", "params": {}}


def test_parse_close_strategy_arg_json_missing_name_rejected():
    with pytest.raises(SystemExit, match="missing 'name'"):
        run_backtest._parse_close_strategy_arg('{"params": {"pct": 0.03}}')


def test_parse_close_strategy_arg_invalid_json_rejected():
    with pytest.raises(SystemExit, match="not valid JSON"):
        run_backtest._parse_close_strategy_arg('{"name": "tiered_tp_pct"')


def test_parse_close_strategy_arg_non_object_json_rejected():
    with pytest.raises(SystemExit, match="must be an object"):
        run_backtest._parse_close_strategy_arg('["tp_at_pct"]')


# ─── load_strategy_config: live config → Backtester kwargs ───────────────────


def _write_config(tmp_path, version, strategies):
    cfg = {"config_version": version, "strategies": strategies}
    p = tmp_path / "config.json"
    p.write_text(json.dumps(cfg, indent=2))
    return str(p)


def test_load_strategy_config_extracts_refs(tmp_path):
    path = _write_config(tmp_path, version=15, strategies=[
        {
            "id": "hl-temacb-btc",
            "type": "perps",
            "open_strategy": {"name": "tema_cross_bd", "params": {"short_period": 5}},
            "close_strategies": [
                {"name": "tiered_tp_atr", "params": {"tp_tiers": [
                    {"atr_multiple": 2.0, "close_fraction": 0.5},
                    {"atr_multiple": 3.0, "close_fraction": 1.0},
                ]}},
            ],
        },
    ])
    kwargs = run_backtest.load_strategy_config(path, "hl-temacb-btc")
    assert kwargs["open_strategy"]["name"] == "tema_cross_bd"
    assert kwargs["open_strategy"]["params"]["short_period"] == 5
    assert len(kwargs["close_strategies"]) == 1
    assert kwargs["close_strategies"][0]["name"] == "tiered_tp_atr"
    assert kwargs["close_strategies"][0]["params"]["tp_tiers"][0]["atr_multiple"] == 2.0


def test_load_strategy_config_reads_single_close_strategy(tmp_path):
    # #842: configs use a single close_strategy ref; load_strategy_config must
    # read it into the backtester's close_strategies= list (length 1).
    path = _write_config(tmp_path, version=15, strategies=[
        {
            "id": "hl-temacb-btc",
            "type": "perps",
            "open_strategy": {"name": "tema_cross_bd"},
            "close_strategy": {"name": "tiered_tp_atr", "params": {"tp_tiers": [
                {"atr_multiple": 2.0, "close_fraction": 0.5},
                {"atr_multiple": 3.0, "close_fraction": 1.0},
            ]}},
        },
    ])
    kwargs = run_backtest.load_strategy_config(path, "hl-temacb-btc")
    assert len(kwargs["close_strategies"]) == 1
    assert kwargs["close_strategies"][0]["name"] == "tiered_tp_atr"
    assert kwargs["close_strategies"][0]["params"]["tp_tiers"][1]["atr_multiple"] == 3.0


# ─── #866: --defaults system|user (user_close_defaults injection) ────────────


def _write_full_config(tmp_path, cfg):
    p = tmp_path / "config.json"
    p.write_text(json.dumps(cfg, indent=2))
    return str(p)


_USER_RATCHET = {
    "trailing_tp_ratchet": {"tp_tiers": [
        {"atr_multiple": 1.0, "trailing_mult_after": 2.0, "close_fraction": 0.0},
        {"atr_multiple": 2.0, "trailing_mult_after": 1.0, "close_fraction": 0.0},
    ]}
}


def _ratchet_cfg(tmp_path, close_params):
    return _write_full_config(tmp_path, {
        "config_version": 15,
        "user_close_defaults": _USER_RATCHET,
        "strategies": [{
            "id": "hl-r", "type": "perps", "platform": "hyperliquid",
            "open_strategy": {"name": "tema_cross_bd"},
            "trailing_stop_atr_mult": 3.0,
            "close_strategy": {"name": "trailing_tp_ratchet", "params": close_params},
        }],
    })


def test_defaults_user_injects_user_close_defaults(tmp_path):
    path = _ratchet_cfg(tmp_path, {"use_defaults": True})
    kwargs = run_backtest.load_strategy_config(path, "hl-r", inject_user_defaults=True)
    tp = kwargs["close_strategies"][0]["params"].get("tp_tiers")
    assert tp is not None and len(tp) == 2
    assert tp[0]["trailing_mult_after"] == 2.0


def test_defaults_system_does_not_inject(tmp_path):
    path = _ratchet_cfg(tmp_path, {"use_defaults": True})
    kwargs = run_backtest.load_strategy_config(path, "hl-r", inject_user_defaults=False)
    # No injection: the close ref omits tp_tiers and falls through to the
    # evaluator's built-in system default at runtime.
    assert kwargs["close_strategies"][0]["params"].get("tp_tiers") is None


def test_defaults_user_empty_tiers_not_injected(tmp_path):
    # Go↔Python parity: an empty user tp_tiers is not a valid override — skip it
    # so resolution falls through to the system default instead of injecting [].
    path = _write_full_config(tmp_path, {
        "config_version": 15,
        "user_close_defaults": {"trailing_tp_ratchet": {"tp_tiers": []}},
        "strategies": [{
            "id": "hl-r", "type": "perps", "platform": "hyperliquid",
            "open_strategy": {"name": "tema_cross_bd"},
            "trailing_stop_atr_mult": 3.0,
            "close_strategy": {"name": "trailing_tp_ratchet", "params": {"use_defaults": True}},
        }],
    })
    kwargs = run_backtest.load_strategy_config(path, "hl-r", inject_user_defaults=True)
    assert kwargs["close_strategies"][0]["params"].get("tp_tiers") is None


def test_defaults_user_strategy_tiers_win(tmp_path):
    explicit = [{"atr_multiple": 5.0, "trailing_mult_after": 1.0, "close_fraction": 0.0}]
    path = _ratchet_cfg(tmp_path, {"tp_tiers": explicit})
    kwargs = run_backtest.load_strategy_config(path, "hl-r", inject_user_defaults=True)
    tp = kwargs["close_strategies"][0]["params"]["tp_tiers"]
    assert len(tp) == 1 and tp[0]["atr_multiple"] == 5.0  # not overridden


def test_load_strategy_config_rejects_multi_legacy_close_array(tmp_path):
    # #842: the live Go loader rejects a len>1 close_strategies array; the
    # backtester loader must reject it the same way instead of running it under
    # the old max-fraction semantics (live↔backtest divergence).
    path = _write_config(tmp_path, version=15, strategies=[
        {
            "id": "hl-temacb-btc",
            "type": "perps",
            "open_strategy": {"name": "tema_cross_bd"},
            "close_strategies": [
                {"name": "tiered_tp_atr"},
                {"name": "tiered_tp_pct", "params": {"pct": 0.05}},
            ],
        },
    ])
    with pytest.raises(ValueError, match="collapsed to a single close_strategy"):
        run_backtest.load_strategy_config(path, "hl-temacb-btc")


def test_load_strategy_config_single_close_wins_over_legacy_array(tmp_path):
    # When both keys are present (defensive), the canonical close_strategy wins.
    path = _write_config(tmp_path, version=15, strategies=[
        {
            "id": "hl-temacb-btc",
            "type": "perps",
            "open_strategy": {"name": "tema_cross_bd"},
            "close_strategy": {"name": "tiered_tp_pct", "params": {"pct": 0.05}},
            "close_strategies": [{"name": "tiered_tp_atr"}],
        },
    ])
    kwargs = run_backtest.load_strategy_config(path, "hl-temacb-btc")
    assert [r["name"] for r in kwargs["close_strategies"]] == ["tiered_tp_pct"]


def test_load_strategy_config_rejects_pre_v15_gate(tmp_path):
    # #942 (D2.8): the loader gates on v15 (not v13) because the v15 migration
    # canonicalizes close params on disk. A pre-gate version raises before any
    # ref parsing.
    path = _write_config(tmp_path, version=12, strategies=[
        # Pre-v13 flat shape: open_strategy is a string, params is flat.
        {"id": "hl-temacb-btc", "open_strategy": "tema_cross_bd",
         "close_strategies": ["tiered_tp_atr"], "params": {"tp_tiers": []}},
    ])
    with pytest.raises(ValueError, match="config_version=12"):
        run_backtest.load_strategy_config(path, "hl-temacb-btc")


@pytest.mark.parametrize("version", [13, 14])
def test_load_strategy_config_rejects_pre_v15_with_legacy_tiers(tmp_path, version):
    # #942 (D2.8) regression: a v13/v14 config still carries pre-canonicalization
    # close keys (legacy `tiers` rather than `tp_tiers`, `atr_multiple` written
    # as `atr`). The Python close evaluators read ONLY the canonical runtime
    # keys, so these would silently no-op (explicit tiers dropped to the system
    # default; --defaults user injecting over them) while live canonicalizes on
    # read. The v15 gate must reject the file instead of running it.
    path = _write_config(tmp_path, version=version, strategies=[
        {
            "id": "hl-temacb-btc",
            "type": "perps",
            "open_strategy": {"name": "tema_cross_bd"},
            "close_strategy": {"name": "tiered_tp_atr", "params": {
                # Legacy on-disk shape the v15 migration rewrites:
                "tiers": [
                    {"atr": 2.0, "fraction": 0.5},
                    {"atr": 3.0, "fraction": 1.0},
                ],
            }},
        },
    ])
    with pytest.raises(ValueError, match=f"config_version={version}"):
        run_backtest.load_strategy_config(path, "hl-temacb-btc")


def test_load_strategy_config_rejects_unknown_id(tmp_path):
    path = _write_config(tmp_path, version=15, strategies=[
        {"id": "hl-temacb-btc",
         "open_strategy": {"name": "tema_cross_bd"},
         "close_strategies": []},
    ])
    with pytest.raises(ValueError, match="no strategy with id='hl-other-eth'"):
        run_backtest.load_strategy_config(path, "hl-other-eth")


def test_load_strategy_config_rejects_dynamic_regime_close_single(tmp_path):
    path = _write_config(tmp_path, version=15, strategies=[
        {
            "id": "hl-dyn-btc",
            "open_strategy": {"name": "tema_cross_bd"},
            "close_strategy": {"name": "tiered_tp_atr_live_regime_dynamic", "params": {}},
        },
    ])
    with pytest.raises(ValueError, match="tiered_tp_atr_live_regime_dynamic"):
        run_backtest.load_strategy_config(path, "hl-dyn-btc")


def test_load_strategy_config_rejects_dynamic_regime_close_legacy_array(tmp_path):
    path = _write_config(tmp_path, version=15, strategies=[
        {
            "id": "hl-dyn-btc",
            "open_strategy": {"name": "tema_cross_bd"},
            "close_strategies": [
                {"name": "tiered_tp_atr_live_regime_dynamic", "params": {}},
            ],
        },
    ])
    with pytest.raises(ValueError, match="tiered_tp_atr_live_regime_dynamic"):
        run_backtest.load_strategy_config(path, "hl-dyn-btc")


def test_load_strategy_config_then_backtester_parity(tmp_path):
    """Same JSON block produces same Backtester wiring as constructing the
    refs by hand. This is the live↔backtest parity contract from the issue."""
    path = _write_config(tmp_path, version=15, strategies=[
        {
            "id": "hl-temacb-btc",
            "open_strategy": {"name": "tema_cross_bd", "params": {"short_period": 5}},
            "close_strategies": [
                {"name": "tiered_tp_pct", "params": {"pct": 0.05}},
            ],
        },
    ])
    kwargs = run_backtest.load_strategy_config(path, "hl-temacb-btc")
    bt_from_config = Backtester(initial_capital=1000, **kwargs)
    bt_inline = Backtester(
        initial_capital=1000,
        open_strategy={"name": "tema_cross_bd", "params": {"short_period": 5}},
        close_strategies=[{"name": "tiered_tp_pct", "params": {"pct": 0.05}}],
    )
    assert bt_from_config.open_strategy == bt_inline.open_strategy
    assert bt_from_config.close_strategies == bt_inline.close_strategies
    assert bt_from_config.close_params == bt_inline.close_params


# ─── End-to-end: --config threads live open params (#643 review #1) ──────────


def test_config_flag_threads_live_open_params_to_result(tmp_path, monkeypatch):
    """Drive main() with --config and verify the live config's open_strategy.params
    reach the Backtester result, instead of being silently overridden by the
    registry's default_params. Regression for #643 review #1.
    """
    # Real strategy that has overridable defaults: triple_ema (default short=8).
    config_path = _write_config(tmp_path, version=15, strategies=[
        {
            "id": "hl-triple-btc",
            "type": "perps",
            "open_strategy": {
                "name": "triple_ema",
                # Non-default value: registry default short_period is 8.
                "params": {"short_period": 3, "mid_period": 13, "long_period": 34},
            },
            "close_strategies": [],
        },
    ])

    captured = {}
    real_run_single = run_backtest.run_single_backtest

    def spy_run_single(*args, **kwargs):
        captured["params"] = kwargs.get("params")
        captured["close_refs"] = kwargs.get("close_strategies")
        captured["strategy_name"] = kwargs.get("strategy_name") or (args[0] if args else None)
        # Don't actually run the backtest — just record what main() forwarded.
        return None

    monkeypatch.setattr(run_backtest, "run_single_backtest", spy_run_single)
    monkeypatch.setattr("sys.argv", [
        "run_backtest.py",
        "--mode", "single",
        "--config", config_path,
        "--strategy", "hl-triple-btc",
    ])

    run_backtest.main()

    assert captured["strategy_name"] == "triple_ema", (
        f"main() did not rewrite --strategy to the live open ref name; "
        f"got {captured.get('strategy_name')!r}"
    )
    assert captured["params"] == {"short_period": 3, "mid_period": 13, "long_period": 34}, (
        f"main() did not thread live open_strategy.params; got {captured.get('params')!r}. "
        f"Without this, run_single_backtest falls back to triple_ema's registry default "
        f"short_period=8 and silently ignores the live config."
    )
    # Restore (defensive — monkeypatch handles it but explicit reads better).
    assert real_run_single is not None  # silence unused


def test_config_flag_rejects_non_single_modes(tmp_path):
    """--config loads exactly one strategy; rejecting compare/multi/optimize
    upfront prevents misleading reports where only the matched strategy gets
    the live close refs and the rest run with no close strategies (#643 review #4)."""
    config_path = _write_config(tmp_path, version=15, strategies=[
        {"id": "x", "open_strategy": {"name": "triple_ema"}, "close_strategies": []},
    ])
    import sys as _sys
    for bad_mode in ("compare", "multi", "optimize"):
        old_argv = _sys.argv
        _sys.argv = ["run_backtest.py", "--mode", bad_mode,
                     "--config", config_path, "--strategy", "x"]
        try:
            with pytest.raises(SystemExit):
                run_backtest.main()
        finally:
            _sys.argv = old_argv


# ─── #942: direction / invert_signal / regime_window_divergence parity ───────


def _perps_strategy(strategy_id="hl-d-btc", **extra):
    base = {
        "id": strategy_id,
        "type": "perps",
        "open_strategy": {"name": "tema_cross_bd"},
        "close_strategy": {"name": "tiered_tp_atr", "params": {"tp_tiers": [
            {"atr_multiple": 2.0, "close_fraction": 1.0},
        ]}},
    }
    base.update(extra)
    return base


def test_load_strategy_config_returns_direction_and_invert(tmp_path):
    path = _write_config(tmp_path, version=15, strategies=[
        _perps_strategy(direction="short", invert_signal=True),
    ])
    kwargs = run_backtest.load_strategy_config(path, "hl-d-btc")
    assert kwargs["direction"] == "short"
    assert kwargs["invert_signal"] is True


def test_load_strategy_config_direction_defaults_long(tmp_path):
    # No direction field on a perps strategy → effective "long" (matches
    # EffectiveDirection); invert_signal defaults False.
    path = _write_config(tmp_path, version=15, strategies=[_perps_strategy()])
    kwargs = run_backtest.load_strategy_config(path, "hl-d-btc")
    assert kwargs["direction"] == "long"
    assert kwargs["invert_signal"] is False


def test_load_strategy_config_allow_shorts_maps_to_both(tmp_path):
    # Legacy pre-v14 toggle: allow_shorts=true with no explicit direction →
    # "both" (mirrors EffectiveDirection's AllowShorts fallback).
    path = _write_config(tmp_path, version=15, strategies=[
        _perps_strategy(allow_shorts=True),
    ])
    kwargs = run_backtest.load_strategy_config(path, "hl-d-btc")
    assert kwargs["direction"] == "both"


def test_load_strategy_config_spot_direction_is_long(tmp_path):
    # direction is meaningful only for perps/manual; a spot strategy is long by
    # construction even if a stray direction field is present.
    path = _write_config(tmp_path, version=15, strategies=[
        {
            "id": "spot-x",
            "type": "spot",
            "open_strategy": {"name": "sma_crossover"},
            "direction": "short",
        },
    ])
    kwargs = run_backtest.load_strategy_config(path, "spot-x")
    assert kwargs["direction"] == "long"


def test_load_strategy_config_rejects_regime_window_divergence(tmp_path):
    # #942 (D2.5): regime_window_divergence (#907) is HL-live-only and was
    # silently ignored; the loader must reject it loudly like its siblings.
    path = _write_config(tmp_path, version=15, strategies=[
        _perps_strategy(regime_window_divergence={
            "short_window": "short", "medium_window": "medium",
            "on_divergence": {"mode": "trust_short"},
        }),
    ])
    with pytest.raises(ValueError, match="regime_window_divergence"):
        run_backtest.load_strategy_config(path, "hl-d-btc")


@pytest.mark.parametrize("direction", ["short", "both"])
def test_load_strategy_config_rejects_short_or_both_without_close(tmp_path, direction):
    # The plain long/flat signal path cannot open shorts, so a short/both
    # direction with no close evaluator would silently drop the short side.
    # Reject instead of backtesting long-only.
    path = _write_config(tmp_path, version=15, strategies=[
        {
            "id": "hl-noclose",
            "type": "perps",
            "open_strategy": {"name": "tema_cross_bd"},
            "direction": direction,
            # No close_strategy → plain long/flat path.
        },
    ])
    with pytest.raises(ValueError, match="cannot open shorts"):
        run_backtest.load_strategy_config(path, "hl-noclose")


def test_load_strategy_config_long_without_close_is_allowed(tmp_path):
    # direction="long" + no close evaluator is fine: the plain long/flat path
    # is already long-only and matches live (signal=-1 closes the long).
    path = _write_config(tmp_path, version=15, strategies=[
        {
            "id": "hl-longnoclose",
            "type": "perps",
            "open_strategy": {"name": "tema_cross_bd"},
            "direction": "long",
        },
    ])
    kwargs = run_backtest.load_strategy_config(path, "hl-longnoclose")
    assert kwargs["direction"] == "long"
    assert kwargs["close_strategies"] == []


def test_load_strategy_config_both_with_close_is_allowed(tmp_path):
    # direction="both" WITH a close evaluator uses the open/close engine path,
    # which opens both sides — allowed.
    path = _write_config(tmp_path, version=15, strategies=[
        _perps_strategy(direction="both"),
    ])
    kwargs = run_backtest.load_strategy_config(path, "hl-d-btc")
    assert kwargs["direction"] == "both"


# ─── #942 review: spot/futures --config masks shorts (long-by-construction) ──
#
# Requires-Human-Review item on PR #951: a non-perps --config with a close
# evaluator now forces direction='long' (_effective_direction), so the
# open/close engine path masks short opens. The behavior is more correct (spot
# can't short) but silently shifts pre-PR spot/futures --config numbers, where a
# raw signal=-1 used to open an (erroneous) short. These tests pin the kept
# behavior end-to-end and cover the compound case the masking exposes.


_SPOT_NEVER_FIRES_CLOSE = {"name": "tiered_tp_pct", "params": {"tp_tiers": [
    {"profit_pct": 0.9, "close_fraction": 1.0},
]}}


def _flat_ohlc(signal):
    # Flat prices so the 90%-profit close never fires: the position survives to
    # the end-of-run flush and the recorded trade carries its OPEN side.
    n = len(signal)
    return pd.DataFrame(
        {
            "open":   [100.0] * n,
            "high":   [101.0] * n,
            "low":    [99.0] * n,
            "close":  [100.0] * n,
            "volume": [1.0] * n,
            "signal": signal,
        },
        index=pd.date_range("2024-01-01", periods=n, freq="D"),
    )


def _spot_close_cfg(tmp_path, strategy_type="spot", **extra):
    strat = {
        "id": "sc-x",
        "type": strategy_type,
        "open_strategy": {"name": "sma_crossover"},
        "close_strategy": dict(_SPOT_NEVER_FIRES_CLOSE),
    }
    strat.update(extra)
    return _write_config(tmp_path, version=15, strategies=[strat])


def _run_config(path, strategy_id, signal):
    kwargs = run_backtest.load_strategy_config(path, strategy_id)
    bt = Backtester(initial_capital=1000, commission_pct=0.0,
                    slippage_pct=0.0, **kwargs)
    return bt.run(_flat_ohlc(signal), save=False)


@pytest.mark.parametrize("strategy_type", ["spot", "futures"])
def test_config_non_perps_masks_short_open_end_to_end(tmp_path, strategy_type):
    # The flagged item: a non-perps --config with a close evaluator forces
    # direction='long', so a short-opening signal opens NOTHING (not a short).
    path = _spot_close_cfg(tmp_path, strategy_type=strategy_type)
    assert _run_config(path, "sc-x", [-1, 0, 0, 0])["trades"] == []


@pytest.mark.parametrize("strategy_type", ["spot", "futures"])
def test_config_non_perps_allows_long_open_end_to_end(tmp_path, strategy_type):
    # Inverse of the masked case: a long-opening signal is untouched and opens a
    # long. The mask must not suppress the allowed side.
    path = _spot_close_cfg(tmp_path, strategy_type=strategy_type)
    result = _run_config(path, "sc-x", [1, 0, 0, 0])
    assert [t["side"] for t in result["trades"]] == ["long"]


def test_config_spot_stray_direction_short_is_ignored(tmp_path):
    # A stray direction='short' on a spot strategy is ignored (long-by-
    # construction, matching EffectiveDirection): a long signal still opens
    # long, and a short signal is still masked. Direction never makes spot short.
    path = _spot_close_cfg(tmp_path, direction="short")
    assert [t["side"] for t in _run_config(path, "sc-x", [1, 0, 0, 0])["trades"]] == ["long"]
    assert _run_config(path, "sc-x", [-1, 0, 0, 0])["trades"] == []


@pytest.mark.parametrize("strategy_type", ["spot", "futures"])
def test_load_strategy_config_rejects_invert_signal_on_non_perps(tmp_path, strategy_type):
    # Compound case the masking exposes: invert_signal is HL-perps/manual-only —
    # live (config.go) rejects the config at startup for any other type. Without
    # a matching gate the backtester would flip BUY<->SELL (then mask the
    # inverted short), producing numbers for a config the daemon won't load.
    path = _write_config(tmp_path, version=15, strategies=[
        {
            "id": "inv-x",
            "type": strategy_type,
            "open_strategy": {"name": "sma_crossover"},
            "close_strategy": dict(_SPOT_NEVER_FIRES_CLOSE),
            "invert_signal": True,
        },
    ])
    with pytest.raises(ValueError, match="invert_signal"):
        run_backtest.load_strategy_config(path, "inv-x")


@pytest.mark.parametrize("strategy_type", ["perps", "manual"])
def test_load_strategy_config_allows_invert_signal_on_hl_types(tmp_path, strategy_type):
    # The two HL types that honor invert_signal in live are accepted unchanged.
    path = _write_config(tmp_path, version=15, strategies=[
        {
            "id": "inv-x",
            "type": strategy_type,
            "open_strategy": {"name": "tema_cross_bd"},
            "close_strategy": {"name": "tiered_tp_atr", "params": {"tp_tiers": [
                {"atr_multiple": 2.0, "close_fraction": 1.0},
            ]}},
            "invert_signal": True,
        },
    ])
    kwargs = run_backtest.load_strategy_config(path, "inv-x")
    assert kwargs["invert_signal"] is True
    assert kwargs["strategy_type"] == strategy_type
