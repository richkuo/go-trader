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
            {"name": "tiered_tp_atr", "params": {"tiers": [
                {"atr_multiple": 2.0, "close_fraction": 1.0},
            ]}},
        ],
    )
    assert bt.close_strategies == ["tiered_tp_atr"]
    assert bt.close_params["tiered_tp_atr"]["tiers"] == [
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
    assert [r["name"] for r in refs] == ["tp_at_pct", "tiered_tp_pct"]
    assert refs[0]["params"] == {"pct": 0.05}
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
        '{"name": "tiered_tp_atr", "params": {"tiers": [{"atr_multiple": 2.0}]}}'
    )
    assert ref["name"] == "tiered_tp_atr"
    assert ref["params"]["tiers"][0]["atr_multiple"] == 2.0


def test_parse_close_strategy_arg_json_without_params():
    ref = run_backtest._parse_close_strategy_arg('{"name": "tp_at_pct"}')
    assert ref == {"name": "tp_at_pct", "params": {}}


def test_parse_close_strategy_arg_json_missing_name_rejected():
    with pytest.raises(SystemExit, match="missing 'name'"):
        run_backtest._parse_close_strategy_arg('{"params": {"pct": 0.03}}')


def test_parse_close_strategy_arg_invalid_json_rejected():
    with pytest.raises(SystemExit, match="not valid JSON"):
        run_backtest._parse_close_strategy_arg('{"name": "tp_at_pct"')


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
                {"name": "tiered_tp_atr", "params": {"tiers": [
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
    assert kwargs["close_strategies"][0]["params"]["tiers"][0]["atr_multiple"] == 2.0


def test_load_strategy_config_rejects_pre_v13(tmp_path):
    path = _write_config(tmp_path, version=12, strategies=[
        # Pre-v13 flat shape: open_strategy is a string, params is flat.
        {"id": "hl-temacb-btc", "open_strategy": "tema_cross_bd",
         "close_strategies": ["tiered_tp_atr"], "params": {"tiers": []}},
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


def test_load_strategy_config_then_backtester_parity(tmp_path):
    """Same JSON block produces same Backtester wiring as constructing the
    refs by hand. This is the live↔backtest parity contract from the issue."""
    path = _write_config(tmp_path, version=13, strategies=[
        {
            "id": "hl-temacb-btc",
            "open_strategy": {"name": "tema_cross_bd", "params": {"short_period": 5}},
            "close_strategies": [
                {"name": "tp_at_pct", "params": {"pct": 0.05}},
            ],
        },
    ])
    kwargs = run_backtest.load_strategy_config(path, "hl-temacb-btc")
    bt_from_config = Backtester(initial_capital=1000, **kwargs)
    bt_inline = Backtester(
        initial_capital=1000,
        open_strategy={"name": "tema_cross_bd", "params": {"short_period": 5}},
        close_strategies=[{"name": "tp_at_pct", "params": {"pct": 0.05}}],
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
