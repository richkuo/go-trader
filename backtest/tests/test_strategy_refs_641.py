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
    path = _write_config(tmp_path, version=13, strategies=[
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


def test_load_strategy_config_rejects_pre_v13(tmp_path):
    path = _write_config(tmp_path, version=12, strategies=[
        # Pre-v13 flat shape: open_strategy is a string, params is flat.
        {"id": "hl-temacb-btc", "open_strategy": "tema_cross_bd",
         "close_strategies": ["tiered_tp_atr"], "params": {"tp_tiers": []}},
    ])
    with pytest.raises(ValueError, match="config_version=12"):
        run_backtest.load_strategy_config(path, "hl-temacb-btc")


def test_load_strategy_config_rejects_unknown_id(tmp_path):
    path = _write_config(tmp_path, version=13, strategies=[
        {"id": "hl-temacb-btc",
         "open_strategy": {"name": "tema_cross_bd"},
         "close_strategies": []},
    ])
    with pytest.raises(ValueError, match="no strategy with id='hl-other-eth'"):
        run_backtest.load_strategy_config(path, "hl-other-eth")


def test_load_strategy_config_rejects_dynamic_regime_close_single(tmp_path):
    path = _write_config(tmp_path, version=13, strategies=[
        {
            "id": "hl-dyn-btc",
            "open_strategy": {"name": "tema_cross_bd"},
            "close_strategy": {"name": "tiered_tp_atr_live_regime_dynamic", "params": {}},
        },
    ])
    with pytest.raises(ValueError, match="tiered_tp_atr_live_regime_dynamic"):
        run_backtest.load_strategy_config(path, "hl-dyn-btc")


def test_load_strategy_config_rejects_dynamic_regime_close_legacy_array(tmp_path):
    path = _write_config(tmp_path, version=13, strategies=[
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
    path = _write_config(tmp_path, version=13, strategies=[
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
    config_path = _write_config(tmp_path, version=13, strategies=[
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
    config_path = _write_config(tmp_path, version=13, strategies=[
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
