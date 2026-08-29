import json

import pandas as pd
import pytest

from backtester import Backtester
import run_backtest


def _flat_df():
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


def test_backtester_accepts_open_strategy_ref():
    bt = Backtester(
        initial_capital=1000,
        open_strategy={"name": "tema_cross_bd", "params": {"short_period": 5}},
    )
    df = _flat_df()
    result = bt.run(df, save=False)
    assert result["open_strategy"]["name"] == "tema_cross_bd"
    assert result["open_strategy"]["params"]["short_period"] == 5


def test_backtester_accepts_close_strategy_ref_with_params():
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


def test_defaults_auto_uses_user_defaults_for_config_runs():
    args = run_backtest._build_parser().parse_args([
        "--config", "scheduler/config.json",
        "--strategy", "hl-r",
        "--mode", "single",
    ])

    assert run_backtest._resolve_defaults_mode(args) == "user"


def test_defaults_auto_uses_system_defaults_without_config():
    args = run_backtest._build_parser().parse_args([
        "--strategy", "tema_cross_bd",
        "--mode", "single",
    ])

    assert run_backtest._resolve_defaults_mode(args) == "system"


def test_defaults_system_overrides_config_auto_default():
    args = run_backtest._build_parser().parse_args([
        "--config", "scheduler/config.json",
        "--strategy", "hl-r",
        "--mode", "single",
        "--defaults", "system",
    ])

    assert run_backtest._resolve_defaults_mode(args) == "system"


def test_defaults_user_without_config_warns_and_falls_back(capsys):
    args = run_backtest._build_parser().parse_args([
        "--strategy", "tema_cross_bd",
        "--mode", "single",
        "--defaults", "user",
    ])

    assert run_backtest._resolve_defaults_mode(args) == "system"
    assert "requires --config" in capsys.readouterr().out


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


def _init_shaped_strategy():
    return {
        "id": "momentum-btc",
        "type": "spot",
        "platform": "binanceus",
        "script": "shared_scripts/check_strategy.py",
        "args": ["momentum", "BTC/USDT", "1h"],
        "open_strategy": {"name": ""},
        "capital": 1000,
        "max_drawdown_pct": 5,
        "interval_seconds": 3600,
    }


def test_load_strategy_config_init_config_falls_back_to_args0(tmp_path):
    path = _write_config(tmp_path, version=15, strategies=[_init_shaped_strategy()])
    kwargs = run_backtest.load_strategy_config(path, "momentum-btc")
    assert kwargs["open_strategy"]["name"] == "momentum"
    assert kwargs["open_strategy"]["params"] == {}
    assert kwargs["close_strategies"] == []


def test_load_strategy_config_missing_open_strategy_key_falls_back(tmp_path):
    path = _write_config(tmp_path, version=15, strategies=[
        {"id": "spot-x", "type": "spot",
         "args": ["mean_reversion", "ETH/USDT", "1h"]},
    ])
    kwargs = run_backtest.load_strategy_config(path, "spot-x")
    assert kwargs["open_strategy"]["name"] == "mean_reversion"


def test_load_strategy_config_open_name_wins_over_args0(tmp_path):
    path = _write_config(tmp_path, version=15, strategies=[
        {"id": "hl-x", "type": "perps",
         "args": ["legacy_positional", "BTC/USDT", "1h"],
         "open_strategy": {"name": "tema_cross_bd", "params": {"short_period": 5}}},
    ])
    kwargs = run_backtest.load_strategy_config(path, "hl-x")
    assert kwargs["open_strategy"]["name"] == "tema_cross_bd"
    assert kwargs["open_strategy"]["params"]["short_period"] == 5


def test_load_strategy_config_whitespace_open_name_falls_back(tmp_path):
    path = _write_config(tmp_path, version=15, strategies=[
        {"id": "spot-y", "type": "spot",
         "args": ["momentum", "BTC/USDT", "1h"],
         "open_strategy": {"name": "   "}},
    ])
    kwargs = run_backtest.load_strategy_config(path, "spot-y")
    assert kwargs["open_strategy"]["name"] == "momentum"


def test_load_strategy_config_rejects_when_no_open_name_and_no_args(tmp_path):
    path = _write_config(tmp_path, version=15, strategies=[
        {"id": "spot-z", "type": "spot", "args": [], "open_strategy": {"name": ""}},
    ])
    with pytest.raises(ValueError, match="neither open_strategy.name nor"):
        run_backtest.load_strategy_config(path, "spot-z")


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

_USER_RATCHET_REGIME = {
    "trailing_tp_ratchet_regime": {
        "tp_tiers": {
            "trending_up": [
                {"atr_multiple": 1.0, "trailing_mult_after": 1.0, "close_fraction": 0.0}
            ],
            "trending_down": [
                {"atr_multiple": 1.0, "trailing_mult_after": 1.0, "close_fraction": 0.0}
            ],
            "ranging": [
                {"atr_multiple": 1.0, "trailing_mult_after": 1.0, "close_fraction": 0.0}
            ],
        },
        "trailing_stop_atr_mult_regime": {
            "trend_regime": {
                "trending_up": {"atr_multiple": 2.75},
                "trending_down": {"atr_multiple": 2.75},
                "ranging": {"atr_multiple": 1.5},
            }
        },
    }
}


def _ratchet_cfg(tmp_path, close_params):
    return _write_full_config(tmp_path, {
        "config_version": 16,
        "user_defaults": {"close": _USER_RATCHET},
        "strategies": [{
            "id": "hl-r", "type": "perps", "platform": "hyperliquid",
            "open_strategy": {"name": "tema_cross_bd"},
            "trailing_stop_atr_mult": 3.0,
            "close_strategy": {"name": "trailing_tp_ratchet", "params": close_params},
        }],
    })


def test_defaults_user_injects_user_defaults_close(tmp_path):
    path = _ratchet_cfg(tmp_path, {"use_defaults": True})
    kwargs = run_backtest.load_strategy_config(path, "hl-r", inject_user_defaults=True)
    tp = kwargs["close_strategies"][0]["params"].get("tp_tiers")
    assert tp is not None and len(tp) == 2
    assert tp[0]["trailing_mult_after"] == 2.0


def test_defaults_user_accepts_legacy_alias_when_canonical_absent(tmp_path):
    path = _write_full_config(tmp_path, {
        "config_version": 16,
        "user_close_defaults": _USER_RATCHET,
        "strategies": [{
            "id": "hl-r", "type": "perps", "platform": "hyperliquid",
            "open_strategy": {"name": "tema_cross_bd"},
            "trailing_stop_atr_mult": 3.0,
            "close_strategy": {"name": "trailing_tp_ratchet", "params": {"use_defaults": True}},
        }],
    })
    kwargs = run_backtest.load_strategy_config(path, "hl-r", inject_user_defaults=True)
    tp = kwargs["close_strategies"][0]["params"].get("tp_tiers")
    assert tp is not None and tp[0]["trailing_mult_after"] == 2.0


def test_defaults_user_rejects_conflicting_legacy_alias(tmp_path):
    path = _write_full_config(tmp_path, {
        "config_version": 16,
        "user_defaults": {"close": _USER_RATCHET},
        "user_close_defaults": {
            "trailing_tp_ratchet": {"tp_tiers": [
                {"atr_multiple": 9.0, "trailing_mult_after": 1.0, "close_fraction": 0.0},
            ]},
        },
        "strategies": [{
            "id": "hl-r", "type": "perps", "platform": "hyperliquid",
            "open_strategy": {"name": "tema_cross_bd"},
            "trailing_stop_atr_mult": 3.0,
            "close_strategy": {"name": "trailing_tp_ratchet", "params": {"use_defaults": True}},
        }],
    })
    with pytest.raises(ValueError, match="conflicts"):
        run_backtest.load_strategy_config(path, "hl-r", inject_user_defaults=True)


def test_defaults_system_does_not_inject(tmp_path):
    path = _ratchet_cfg(tmp_path, {"use_defaults": True})
    kwargs = run_backtest.load_strategy_config(path, "hl-r", inject_user_defaults=False)
    assert kwargs["close_strategies"][0]["params"].get("tp_tiers") is None


def test_defaults_user_empty_tiers_not_injected(tmp_path):
    path = _write_full_config(tmp_path, {
        "config_version": 16,
        "user_defaults": {"close": {"trailing_tp_ratchet": {"tp_tiers": []}}},
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
    assert len(tp) == 1 and tp[0]["atr_multiple"] == 5.0


def _ratchet_regime_cfg(tmp_path, extra_strategy=None):
    strategy = {
        "id": "hl-rr",
        "type": "perps",
        "platform": "hyperliquid",
        "open_strategy": {"name": "tema_cross_bd"},
        "close_strategy": {"name": "trailing_tp_ratchet_regime", "params": {"use_defaults": True}},
    }
    if extra_strategy:
        strategy.update(extra_strategy)
    return _write_full_config(tmp_path, {
        "config_version": 16,
        "regime": {"enabled": True, "period": 14, "adx_threshold": 20},
        "user_defaults": {"close": _USER_RATCHET_REGIME},
        "strategies": [strategy],
    })


def test_defaults_user_injects_ratchet_regime_trail(tmp_path):
    path = _ratchet_regime_cfg(tmp_path)
    kwargs = run_backtest.load_strategy_config(path, "hl-rr", inject_user_defaults=True)
    assert kwargs["trailing_stop_atr_mult_regime"]["trend_regime"]["ranging"]["atr_multiple"] == 1.5
    tp = kwargs["close_strategies"][0]["params"].get("tp_tiers")
    assert tp["trending_up"][0]["trailing_mult_after"] == 1.0


def test_defaults_system_does_not_inject_ratchet_regime_trail(tmp_path):
    path = _ratchet_regime_cfg(tmp_path)
    kwargs = run_backtest.load_strategy_config(path, "hl-rr", inject_user_defaults=False)
    assert kwargs["trailing_stop_atr_mult_regime"] is None
    assert kwargs["close_strategies"][0]["params"].get("tp_tiers") is None


def test_defaults_user_ratchet_regime_trail_does_not_override_stop_owner(tmp_path):
    path = _ratchet_regime_cfg(tmp_path, {"trailing_stop_atr_mult": 3.0})
    kwargs = run_backtest.load_strategy_config(path, "hl-rr", inject_user_defaults=True)
    assert kwargs["trailing_stop_atr_mult_regime"] is None
    assert kwargs["trailing_stop_atr_mult"] == 3.0


def test_load_strategy_config_rejects_multi_legacy_close_array(tmp_path):
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
    path = _write_config(tmp_path, version=12, strategies=[
        {"id": "hl-temacb-btc", "open_strategy": "tema_cross_bd",
         "close_strategies": ["tiered_tp_atr"], "params": {"tp_tiers": []}},
    ])
    with pytest.raises(ValueError, match="config_version=12"):
        run_backtest.load_strategy_config(path, "hl-temacb-btc")


@pytest.mark.parametrize("version", [13, 14])
def test_load_strategy_config_rejects_pre_v15_with_legacy_tiers(tmp_path, version):
    path = _write_config(tmp_path, version=version, strategies=[
        {
            "id": "hl-temacb-btc",
            "type": "perps",
            "open_strategy": {"name": "tema_cross_bd"},
            "close_strategy": {"name": "tiered_tp_atr", "params": {
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


def test_config_flag_threads_live_open_params_to_result(tmp_path, monkeypatch):
    config_path = _write_config(tmp_path, version=15, strategies=[
        {
            "id": "hl-triple-btc",
            "type": "perps",
            "open_strategy": {
                "name": "triple_ema",
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
    assert real_run_single is not None


def test_config_flag_rejects_non_single_modes(tmp_path):
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


def test_direction_flag_rejected_alongside_config(tmp_path, monkeypatch):
    config_path = _write_config(tmp_path, version=15, strategies=[
        {"id": "x", "open_strategy": {"name": "triple_ema"}, "close_strategies": []},
    ])
    called = {}
    monkeypatch.setattr(run_backtest, "run_single_backtest",
                        lambda *a, **kw: called.setdefault("hit", True))
    monkeypatch.setattr("sys.argv", [
        "run_backtest.py", "--mode", "single",
        "--config", config_path, "--strategy", "x",
        "--direction", "short",
    ])
    with pytest.raises(SystemExit):
        run_backtest.main()
    assert "hit" not in called


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
    path = _write_config(tmp_path, version=15, strategies=[_perps_strategy()])
    kwargs = run_backtest.load_strategy_config(path, "hl-d-btc")
    assert kwargs["direction"] == "long"
    assert kwargs["invert_signal"] is False


def test_load_strategy_config_allow_shorts_maps_to_both(tmp_path):
    path = _write_config(tmp_path, version=15, strategies=[
        _perps_strategy(allow_shorts=True),
    ])
    kwargs = run_backtest.load_strategy_config(path, "hl-d-btc")
    assert kwargs["direction"] == "both"


def test_load_strategy_config_spot_direction_is_long(tmp_path):
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
    path = _write_config(tmp_path, version=15, strategies=[
        _perps_strategy(regime_window_divergence={
            "short_window": "short", "medium_window": "medium",
            "on_divergence": {"mode": "trust_short"},
        }),
    ])
    with pytest.raises(ValueError, match="regime_window_divergence"):
        run_backtest.load_strategy_config(path, "hl-d-btc")


_REGIME_DIRECTIONAL_POLICY = {
    "trend_regime": {
        "trending_up": {"direction": "long", "invert_signal": False},
        "trending_down": {"direction": "short", "invert_signal": True},
        "ranging": {"direction": "long", "invert_signal": False},
    },
}


def test_load_strategy_config_threads_regime_directional_policy(tmp_path):
    path = _write_full_config(tmp_path, {
        "config_version": 15,
        "regime": {"enabled": True, "period": 10, "adx_threshold": 25},
        "strategies": [
            _perps_strategy(regime_directional_policy=_REGIME_DIRECTIONAL_POLICY),
        ],
    })
    kwargs = run_backtest.load_strategy_config(path, "hl-d-btc")
    assert kwargs["regime_directional_policy"] == _REGIME_DIRECTIONAL_POLICY
    assert kwargs["regime_enabled"] is True
    assert kwargs["regime_period"] == 10
    assert kwargs["regime_adx_threshold"] == 25.0


def test_load_strategy_config_rejects_policy_without_regime_enabled(tmp_path):
    path = _write_config(tmp_path, version=15, strategies=[
        _perps_strategy(regime_directional_policy=_REGIME_DIRECTIONAL_POLICY),
    ])
    with pytest.raises(ValueError, match="regime.enabled=true"):
        run_backtest.load_strategy_config(path, "hl-d-btc")


def test_load_strategy_config_requires_top_level_regime_for_policy(tmp_path):
    path = _write_config(tmp_path, version=15, strategies=[
        _perps_strategy(
            regime={"enabled": True, "period": 10, "adx_threshold": 25},
            regime_directional_policy=_REGIME_DIRECTIONAL_POLICY,
        ),
    ])
    with pytest.raises(ValueError, match="regime.enabled=true"):
        run_backtest.load_strategy_config(path, "hl-d-btc")


def test_load_strategy_config_rejects_both_without_close(tmp_path):
    path = _write_config(tmp_path, version=15, strategies=[
        {
            "id": "hl-noclose",
            "type": "perps",
            "open_strategy": {"name": "tema_cross_bd"},
            "direction": "both",
        },
    ])
    with pytest.raises(ValueError, match="silently dropped"):
        run_backtest.load_strategy_config(path, "hl-noclose")


def test_load_strategy_config_short_without_close_is_allowed(tmp_path):
    path = _write_config(tmp_path, version=15, strategies=[
        {
            "id": "hl-shortnoclose",
            "type": "perps",
            "open_strategy": {"name": "tema_cross_bd"},
            "direction": "short",
        },
    ])
    kwargs = run_backtest.load_strategy_config(path, "hl-shortnoclose")
    assert kwargs["direction"] == "short"
    assert kwargs["close_strategies"] == []


def test_load_strategy_config_long_without_close_is_allowed(tmp_path):
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
    path = _write_config(tmp_path, version=15, strategies=[
        _perps_strategy(direction="both"),
    ])
    kwargs = run_backtest.load_strategy_config(path, "hl-d-btc")
    assert kwargs["direction"] == "both"


_SPOT_NEVER_FIRES_CLOSE = {"name": "tiered_tp_pct", "params": {"tp_tiers": [
    {"profit_pct": 0.9, "close_fraction": 1.0},
]}}


def _flat_ohlc(signal):
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
    path = _spot_close_cfg(tmp_path, strategy_type=strategy_type)
    assert _run_config(path, "sc-x", [-1, 0, 0, 0])["trades"] == []


@pytest.mark.parametrize("strategy_type", ["spot", "futures"])
def test_config_non_perps_allows_long_open_end_to_end(tmp_path, strategy_type):
    path = _spot_close_cfg(tmp_path, strategy_type=strategy_type)
    result = _run_config(path, "sc-x", [1, 0, 0, 0])
    assert [t["side"] for t in result["trades"]] == ["long"]


def test_config_spot_stray_direction_short_is_ignored(tmp_path):
    path = _spot_close_cfg(tmp_path, direction="short")
    assert [t["side"] for t in _run_config(path, "sc-x", [1, 0, 0, 0])["trades"]] == ["long"]
    assert _run_config(path, "sc-x", [-1, 0, 0, 0])["trades"] == []


@pytest.mark.parametrize("strategy_type", ["spot", "futures"])
def test_load_strategy_config_rejects_invert_signal_on_non_perps(tmp_path, strategy_type):
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


def test_load_strategy_config_threads_allowed_regimes(tmp_path):
    path = _write_full_config(tmp_path, {
        "config_version": 15,
        "regime": {"enabled": True, "period": 10, "adx_threshold": 25},
        "strategies": [_perps_strategy(allowed_regimes=["trending_up", "ranging"])],
    })
    kwargs = run_backtest.load_strategy_config(path, "hl-d-btc")
    assert kwargs["allowed_regimes"] == ["trending_up", "ranging"]
    assert kwargs["regime_enabled"] is True


def test_load_strategy_config_allowed_regimes_none_when_unset(tmp_path):
    path = _write_full_config(tmp_path, {
        "config_version": 15,
        "regime": {"enabled": True, "period": 10, "adx_threshold": 25},
        "strategies": [_perps_strategy()],
    })
    kwargs = run_backtest.load_strategy_config(path, "hl-d-btc")
    assert kwargs["allowed_regimes"] is None


def test_load_strategy_config_empty_allowed_regimes_is_none(tmp_path):
    path = _write_full_config(tmp_path, {
        "config_version": 15,
        "regime": {"enabled": True, "period": 10, "adx_threshold": 25},
        "strategies": [_perps_strategy(allowed_regimes=[])],
    })
    kwargs = run_backtest.load_strategy_config(path, "hl-d-btc")
    assert kwargs["allowed_regimes"] is None


def test_load_strategy_config_rejects_allowed_regimes_with_named_gate_window(tmp_path):
    path = _write_full_config(tmp_path, {
        "config_version": 15,
        "regime": {"enabled": True, "period": 10, "adx_threshold": 25,
                   "windows": {"slow": {"classifier": "adx", "period": 40}}},
        "strategies": [
            _perps_strategy(allowed_regimes=["trending_up"], regime_gate_window="slow"),
        ],
    })
    with pytest.raises(ValueError, match="regime_gate_window"):
        run_backtest.load_strategy_config(path, "hl-d-btc")


@pytest.mark.parametrize("gate_window", ["", "default"])
def test_load_strategy_config_default_gate_window_threads(tmp_path, gate_window):
    path = _write_full_config(tmp_path, {
        "config_version": 15,
        "regime": {"enabled": True, "period": 10, "adx_threshold": 25},
        "strategies": [
            _perps_strategy(allowed_regimes=["ranging"], regime_gate_window=gate_window),
        ],
    })
    kwargs = run_backtest.load_strategy_config(path, "hl-d-btc")
    assert kwargs["allowed_regimes"] == ["ranging"]


def test_load_strategy_config_named_gate_window_no_op_when_regime_disabled(tmp_path):
    path = _write_full_config(tmp_path, {
        "config_version": 15,
        "regime": {"enabled": False, "windows": {"slow": 40}},
        "strategies": [
            _perps_strategy(allowed_regimes=["trending_up"], regime_gate_window="slow"),
        ],
    })
    kwargs = run_backtest.load_strategy_config(path, "hl-d-btc")
    assert kwargs["regime_enabled"] is False
    assert kwargs["allowed_regimes"] == ["trending_up"]


def test_allowed_regimes_flag_rejected_alongside_config(tmp_path, monkeypatch):
    config_path = _write_full_config(tmp_path, {
        "config_version": 15,
        "regime": {"enabled": True, "period": 10, "adx_threshold": 25},
        "strategies": [_perps_strategy(allowed_regimes=["trending_up"])],
    })
    called = {}
    monkeypatch.setattr(run_backtest, "run_single_backtest",
                        lambda *a, **kw: called.setdefault("hit", True))
    monkeypatch.setattr("sys.argv", [
        "run_backtest.py", "--mode", "single",
        "--config", config_path, "--strategy", "hl-d-btc",
        "--allowed-regimes", "ranging",
    ])
    with pytest.raises(SystemExit):
        run_backtest.main()
    assert "hit" not in called


def test_config_flag_threads_allowed_regimes_to_run_single(tmp_path, monkeypatch):
    config_path = _write_full_config(tmp_path, {
        "config_version": 15,
        "regime": {"enabled": True, "period": 10, "adx_threshold": 25},
        "strategies": [_perps_strategy(allowed_regimes=["trending_up", "ranging"])],
    })
    captured = {}
    monkeypatch.setattr(run_backtest, "run_single_backtest",
                        lambda *a, **kw: captured.update(kw))
    monkeypatch.setattr("sys.argv", [
        "run_backtest.py", "--mode", "single",
        "--config", config_path, "--strategy", "hl-d-btc",
    ])
    run_backtest.main()
    assert captured.get("allowed_regimes") == ["trending_up", "ranging"]
    assert captured.get("regime_enabled") is True
