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


@pytest.mark.parametrize("close_strategies,match", [
    ([{"params": {"pct": 0.03}}], "missing 'name'"),
    (["bare_string_no_longer_supported"], "must be dicts"),
])
def test_backtester_rejects_malformed_close_strategy_ref(close_strategies, match):
    with pytest.raises(ValueError, match=match):
        Backtester(close_strategies=close_strategies)


@pytest.mark.parametrize("raw,expected", [
    ("tp_at_pct", {"name": "tp_at_pct", "params": {}}),
    ('{"name": "tiered_tp_pct"}', {"name": "tiered_tp_pct", "params": {}}),
    (
        '{"name": "tiered_tp_atr", "params": {"tp_tiers": [{"atr_multiple": 2.0}]}}',
        {"name": "tiered_tp_atr",
         "params": {"tp_tiers": [{"atr_multiple": 2.0}]}},
    ),
])
def test_parse_close_strategy_arg_accepts(raw, expected):
    assert run_backtest._parse_close_strategy_arg(raw) == expected


@pytest.mark.parametrize("raw,match", [
    ('{"params": {"pct": 0.03}}', "missing 'name'"),
    ('{"name": "tiered_tp_pct"', "not valid JSON"),
    ('["tp_at_pct"]', "must be an object"),
])
def test_parse_close_strategy_arg_rejects(raw, match):
    with pytest.raises(SystemExit, match=match):
        run_backtest._parse_close_strategy_arg(raw)


@pytest.mark.parametrize("argv,expected_mode,warn_contains", [
    (["--config", "scheduler/config.json", "--strategy", "hl-r",
      "--mode", "single"], "user", None),
    (["--strategy", "tema_cross_bd", "--mode", "single"], "system", None),
    (["--config", "scheduler/config.json", "--strategy", "hl-r",
      "--mode", "single", "--defaults", "system"], "system", None),
    (["--strategy", "tema_cross_bd", "--mode", "single",
      "--defaults", "user"], "system", "requires --config"),
])
def test_resolve_defaults_mode(capsys, argv, expected_mode, warn_contains):
    args = run_backtest._build_parser().parse_args(argv)
    assert run_backtest._resolve_defaults_mode(args) == expected_mode
    if warn_contains is not None:
        assert warn_contains in capsys.readouterr().out


def _write_config(tmp_path, version, strategies):
    cfg = {"config_version": version, "strategies": strategies}
    p = tmp_path / "config.json"
    p.write_text(json.dumps(cfg, indent=2))
    return str(p)


def _write_full_config(tmp_path, cfg):
    p = tmp_path / "config.json"
    p.write_text(json.dumps(cfg, indent=2))
    return str(p)


def _cfg(version, strategies, **top):
    out = {"config_version": version, "strategies": strategies}
    out.update(top)
    return out


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


def _dig(obj, path):
    for step in path:
        if step == "__len__":
            obj = len(obj)
        elif step == "__names__":
            obj = [r["name"] for r in obj]
        elif isinstance(obj, dict):
            obj = obj.get(step)
        else:
            obj = obj[step]
    return obj


def _assert_config_kwargs(tmp_path, spec):
    path = _write_full_config(tmp_path, spec["cfg"])
    kwargs = run_backtest.load_strategy_config(
        path, spec["sid"], inject_user_defaults=spec.get("inject", False))
    for check_path, expected in spec["checks"]:
        assert _dig(kwargs, check_path) == expected, (check_path, kwargs)


_TIERED_TP_ATR_CLOSE = {"name": "tiered_tp_atr", "params": {"tp_tiers": [
    {"atr_multiple": 2.0, "close_fraction": 0.5},
    {"atr_multiple": 3.0, "close_fraction": 1.0},
]}}

_REGIME_ON = {"enabled": True, "period": 10, "adx_threshold": 25}

_REGIME_DIRECTIONAL_POLICY = {
    "trend_regime": {
        "trending_up": {"direction": "long", "invert_signal": False},
        "trending_down": {"direction": "short", "invert_signal": True},
        "ranging": {"direction": "long", "invert_signal": False},
    },
}

_SPOT_NEVER_FIRES_CLOSE = {"name": "tiered_tp_pct", "params": {"tp_tiers": [
    {"profit_pct": 0.9, "close_fraction": 1.0},
]}}


_REF_CASES = {
    "extracts_refs": dict(
        cfg=_cfg(15, [{
            "id": "hl-temacb-btc",
            "type": "perps",
            "open_strategy": {"name": "tema_cross_bd",
                              "params": {"short_period": 5}},
            "close_strategies": [_TIERED_TP_ATR_CLOSE],
        }]),
        sid="hl-temacb-btc",
        checks=[
            (("open_strategy", "name"), "tema_cross_bd"),
            (("open_strategy", "params", "short_period"), 5),
            (("close_strategies", "__len__"), 1),
            (("close_strategies", 0, "name"), "tiered_tp_atr"),
            (("close_strategies", 0, "params", "tp_tiers", 0, "atr_multiple"), 2.0),
        ],
    ),
    "reads_single_close_strategy": dict(
        cfg=_cfg(15, [{
            "id": "hl-temacb-btc",
            "type": "perps",
            "open_strategy": {"name": "tema_cross_bd"},
            "close_strategy": _TIERED_TP_ATR_CLOSE,
        }]),
        sid="hl-temacb-btc",
        checks=[
            (("close_strategies", "__len__"), 1),
            (("close_strategies", 0, "name"), "tiered_tp_atr"),
            (("close_strategies", 0, "params", "tp_tiers", 1, "atr_multiple"), 3.0),
        ],
    ),
    "init_config_falls_back_to_args0": dict(
        cfg=_cfg(15, [_init_shaped_strategy()]),
        sid="momentum-btc",
        checks=[
            (("open_strategy", "name"), "momentum"),
            (("open_strategy", "params"), {}),
            (("close_strategies",), []),
        ],
    ),
    "missing_open_strategy_key_falls_back": dict(
        cfg=_cfg(15, [{"id": "spot-x", "type": "spot",
                       "args": ["mean_reversion", "ETH/USDT", "1h"]}]),
        sid="spot-x",
        checks=[(("open_strategy", "name"), "mean_reversion")],
    ),
    "open_name_wins_over_args0": dict(
        cfg=_cfg(15, [{"id": "hl-x", "type": "perps",
                       "args": ["legacy_positional", "BTC/USDT", "1h"],
                       "open_strategy": {"name": "tema_cross_bd",
                                         "params": {"short_period": 5}}}]),
        sid="hl-x",
        checks=[
            (("open_strategy", "name"), "tema_cross_bd"),
            (("open_strategy", "params", "short_period"), 5),
        ],
    ),
    "whitespace_open_name_falls_back": dict(
        cfg=_cfg(15, [{"id": "spot-y", "type": "spot",
                       "args": ["momentum", "BTC/USDT", "1h"],
                       "open_strategy": {"name": "   "}}]),
        sid="spot-y",
        checks=[(("open_strategy", "name"), "momentum")],
    ),
    "single_close_wins_over_legacy_array": dict(
        cfg=_cfg(15, [{
            "id": "hl-temacb-btc",
            "type": "perps",
            "open_strategy": {"name": "tema_cross_bd"},
            "close_strategy": {"name": "tiered_tp_pct", "params": {"pct": 0.05}},
            "close_strategies": [{"name": "tiered_tp_atr"}],
        }]),
        sid="hl-temacb-btc",
        checks=[(("close_strategies", "__names__"), ["tiered_tp_pct"])],
    ),
}


@pytest.mark.parametrize("name", list(_REF_CASES))
def test_load_strategy_config_threads_refs(tmp_path, name):
    _assert_config_kwargs(tmp_path, _REF_CASES[name])


_DIRECTION_REGIME_CASES = {
    "returns_direction_and_invert": dict(
        cfg=_cfg(15, [_perps_strategy(direction="short", invert_signal=True)]),
        sid="hl-d-btc",
        checks=[(("direction",), "short"), (("invert_signal",), True)],
    ),
    "direction_defaults_long": dict(
        cfg=_cfg(15, [_perps_strategy()]),
        sid="hl-d-btc",
        checks=[(("direction",), "long"), (("invert_signal",), False)],
    ),
    "allow_shorts_maps_to_both": dict(
        cfg=_cfg(15, [_perps_strategy(allow_shorts=True)]),
        sid="hl-d-btc",
        checks=[(("direction",), "both")],
    ),
    "spot_direction_is_long": dict(
        cfg=_cfg(15, [{"id": "spot-x", "type": "spot",
                       "open_strategy": {"name": "sma_crossover"},
                       "direction": "short"}]),
        sid="spot-x",
        checks=[(("direction",), "long")],
    ),
    "short_without_close_is_allowed": dict(
        cfg=_cfg(15, [{"id": "hl-shortnoclose", "type": "perps",
                       "open_strategy": {"name": "tema_cross_bd"},
                       "direction": "short"}]),
        sid="hl-shortnoclose",
        checks=[(("direction",), "short"), (("close_strategies",), [])],
    ),
    "long_without_close_is_allowed": dict(
        cfg=_cfg(15, [{"id": "hl-longnoclose", "type": "perps",
                       "open_strategy": {"name": "tema_cross_bd"},
                       "direction": "long"}]),
        sid="hl-longnoclose",
        checks=[(("direction",), "long"), (("close_strategies",), [])],
    ),
    "both_with_close_is_allowed": dict(
        cfg=_cfg(15, [_perps_strategy(direction="both")]),
        sid="hl-d-btc",
        checks=[(("direction",), "both")],
    ),
    "invert_signal_on_perps": dict(
        cfg=_cfg(15, [_perps_strategy("inv-x", type="perps", invert_signal=True)]),
        sid="inv-x",
        checks=[(("invert_signal",), True), (("strategy_type",), "perps")],
    ),
    "invert_signal_on_manual": dict(
        cfg=_cfg(15, [_perps_strategy("inv-x", type="manual", invert_signal=True)]),
        sid="inv-x",
        checks=[(("invert_signal",), True), (("strategy_type",), "manual")],
    ),
    "threads_regime_directional_policy": dict(
        cfg=_cfg(15, [_perps_strategy(
            regime_directional_policy=_REGIME_DIRECTIONAL_POLICY)],
            regime=_REGIME_ON),
        sid="hl-d-btc",
        checks=[
            (("regime_directional_policy",), _REGIME_DIRECTIONAL_POLICY),
            (("regime_enabled",), True),
            (("regime_period",), 10),
            (("regime_adx_threshold",), 25.0),
        ],
    ),
    "threads_allowed_regimes": dict(
        cfg=_cfg(15, [_perps_strategy(
            allowed_regimes=["trending_up", "ranging"])], regime=_REGIME_ON),
        sid="hl-d-btc",
        checks=[
            (("allowed_regimes",), ["trending_up", "ranging"]),
            (("regime_enabled",), True),
        ],
    ),
    "allowed_regimes_none_when_unset": dict(
        cfg=_cfg(15, [_perps_strategy()], regime=_REGIME_ON),
        sid="hl-d-btc",
        checks=[(("allowed_regimes",), None)],
    ),
    "empty_allowed_regimes_is_none": dict(
        cfg=_cfg(15, [_perps_strategy(allowed_regimes=[])], regime=_REGIME_ON),
        sid="hl-d-btc",
        checks=[(("allowed_regimes",), None)],
    ),
    "empty_gate_window_threads": dict(
        cfg=_cfg(15, [_perps_strategy(allowed_regimes=["ranging"],
                                      regime_gate_window="")], regime=_REGIME_ON),
        sid="hl-d-btc",
        checks=[(("allowed_regimes",), ["ranging"])],
    ),
    "default_gate_window_threads": dict(
        cfg=_cfg(15, [_perps_strategy(allowed_regimes=["ranging"],
                                      regime_gate_window="default")],
                 regime=_REGIME_ON),
        sid="hl-d-btc",
        checks=[(("allowed_regimes",), ["ranging"])],
    ),
    "named_gate_window_no_op_when_regime_disabled": dict(
        cfg=_cfg(15, [_perps_strategy(allowed_regimes=["trending_up"],
                                      regime_gate_window="slow")],
                 regime={"enabled": False, "windows": {"slow": 40}}),
        sid="hl-d-btc",
        checks=[
            (("regime_enabled",), False),
            (("allowed_regimes",), ["trending_up"]),
        ],
    ),
}


@pytest.mark.parametrize("name", list(_DIRECTION_REGIME_CASES))
def test_load_strategy_config_threads_direction_and_regime(tmp_path, name):
    _assert_config_kwargs(tmp_path, _DIRECTION_REGIME_CASES[name])


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


def _ratchet_strategy(close_params):
    return {
        "id": "hl-r", "type": "perps", "platform": "hyperliquid",
        "open_strategy": {"name": "tema_cross_bd"},
        "trailing_stop_atr_mult": 3.0,
        "close_strategy": {"name": "trailing_tp_ratchet", "params": close_params},
    }


def _ratchet_regime_strategy(**extra):
    strategy = {
        "id": "hl-rr",
        "type": "perps",
        "platform": "hyperliquid",
        "open_strategy": {"name": "tema_cross_bd"},
        "close_strategy": {"name": "trailing_tp_ratchet_regime",
                           "params": {"use_defaults": True}},
    }
    strategy.update(extra)
    return strategy


_RATCHET_TP = ("close_strategies", 0, "params", "tp_tiers")

_USER_DEFAULTS_CASES = {
    "injects_user_defaults_close": dict(
        cfg=_cfg(16, [_ratchet_strategy({"use_defaults": True})],
                 user_defaults={"close": _USER_RATCHET}),
        sid="hl-r",
        inject=True,
        checks=[
            (_RATCHET_TP + ("__len__",), 2),
            (_RATCHET_TP + (0, "trailing_mult_after"), 2.0),
        ],
    ),
    "accepts_legacy_alias_when_canonical_absent": dict(
        cfg=_cfg(16, [_ratchet_strategy({"use_defaults": True})],
                 user_close_defaults=_USER_RATCHET),
        sid="hl-r",
        inject=True,
        checks=[(_RATCHET_TP + (0, "trailing_mult_after"), 2.0)],
    ),
    "system_defaults_do_not_inject": dict(
        cfg=_cfg(16, [_ratchet_strategy({"use_defaults": True})],
                 user_defaults={"close": _USER_RATCHET}),
        sid="hl-r",
        inject=False,
        checks=[(_RATCHET_TP, None)],
    ),
    "empty_tiers_not_injected": dict(
        cfg=_cfg(16, [_ratchet_strategy({"use_defaults": True})],
                 user_defaults={"close": {"trailing_tp_ratchet": {"tp_tiers": []}}}),
        sid="hl-r",
        inject=True,
        checks=[(_RATCHET_TP, None)],
    ),
    "strategy_tiers_win": dict(
        cfg=_cfg(16, [_ratchet_strategy({"tp_tiers": [
            {"atr_multiple": 5.0, "trailing_mult_after": 1.0,
             "close_fraction": 0.0}]})],
            user_defaults={"close": _USER_RATCHET}),
        sid="hl-r",
        inject=True,
        checks=[
            (_RATCHET_TP + ("__len__",), 1),
            (_RATCHET_TP + (0, "atr_multiple"), 5.0),
        ],
    ),
    "injects_ratchet_regime_trail": dict(
        cfg=_cfg(16, [_ratchet_regime_strategy()],
                 regime={"enabled": True, "period": 14, "adx_threshold": 20},
                 user_defaults={"close": _USER_RATCHET_REGIME}),
        sid="hl-rr",
        inject=True,
        checks=[
            (("trailing_stop_atr_mult_regime", "trend_regime", "ranging",
              "atr_multiple"), 1.5),
            (_RATCHET_TP + ("trending_up", 0, "trailing_mult_after"), 1.0),
        ],
    ),
    "system_defaults_do_not_inject_ratchet_regime_trail": dict(
        cfg=_cfg(16, [_ratchet_regime_strategy()],
                 regime={"enabled": True, "period": 14, "adx_threshold": 20},
                 user_defaults={"close": _USER_RATCHET_REGIME}),
        sid="hl-rr",
        inject=False,
        checks=[
            (("trailing_stop_atr_mult_regime",), None),
            (_RATCHET_TP, None),
        ],
    ),
    "ratchet_regime_trail_does_not_override_stop_owner": dict(
        cfg=_cfg(16, [_ratchet_regime_strategy(trailing_stop_atr_mult=3.0)],
                 regime={"enabled": True, "period": 14, "adx_threshold": 20},
                 user_defaults={"close": _USER_RATCHET_REGIME}),
        sid="hl-rr",
        inject=True,
        checks=[
            (("trailing_stop_atr_mult_regime",), None),
            (("trailing_stop_atr_mult",), 3.0),
        ],
    ),
}


@pytest.mark.parametrize("name", list(_USER_DEFAULTS_CASES))
def test_load_strategy_config_user_close_defaults(tmp_path, name):
    _assert_config_kwargs(tmp_path, _USER_DEFAULTS_CASES[name])


_REJECT_CASES = {
    "multi_legacy_close_array": dict(
        cfg=_cfg(15, [{
            "id": "hl-temacb-btc",
            "type": "perps",
            "open_strategy": {"name": "tema_cross_bd"},
            "close_strategies": [
                {"name": "tiered_tp_atr"},
                {"name": "tiered_tp_pct", "params": {"pct": 0.05}},
            ],
        }]),
        sid="hl-temacb-btc",
        match="collapsed to a single close_strategy",
    ),
    "pre_v15_gate": dict(
        cfg=_cfg(12, [{"id": "hl-temacb-btc", "open_strategy": "tema_cross_bd",
                       "close_strategies": ["tiered_tp_atr"],
                       "params": {"tp_tiers": []}}]),
        sid="hl-temacb-btc",
        match="config_version=12",
    ),
    "v13_legacy_tiers": dict(
        cfg=_cfg(13, [{
            "id": "hl-temacb-btc", "type": "perps",
            "open_strategy": {"name": "tema_cross_bd"},
            "close_strategy": {"name": "tiered_tp_atr", "params": {"tiers": [
                {"atr": 2.0, "fraction": 0.5}, {"atr": 3.0, "fraction": 1.0}]}},
        }]),
        sid="hl-temacb-btc",
        match="config_version=13",
    ),
    "v14_legacy_tiers": dict(
        cfg=_cfg(14, [{
            "id": "hl-temacb-btc", "type": "perps",
            "open_strategy": {"name": "tema_cross_bd"},
            "close_strategy": {"name": "tiered_tp_atr", "params": {"tiers": [
                {"atr": 2.0, "fraction": 0.5}, {"atr": 3.0, "fraction": 1.0}]}},
        }]),
        sid="hl-temacb-btc",
        match="config_version=14",
    ),
    "unknown_id": dict(
        cfg=_cfg(15, [{"id": "hl-temacb-btc",
                       "open_strategy": {"name": "tema_cross_bd"},
                       "close_strategies": []}]),
        sid="hl-other-eth",
        match="no strategy with id='hl-other-eth'",
    ),
    "dynamic_regime_close_single": dict(
        cfg=_cfg(15, [{"id": "hl-dyn-btc",
                       "open_strategy": {"name": "tema_cross_bd"},
                       "close_strategy": {
                           "name": "tiered_tp_atr_live_regime_dynamic",
                           "params": {}}}]),
        sid="hl-dyn-btc",
        match="tiered_tp_atr_live_regime_dynamic",
    ),
    "dynamic_regime_close_legacy_array": dict(
        cfg=_cfg(15, [{"id": "hl-dyn-btc",
                       "open_strategy": {"name": "tema_cross_bd"},
                       "close_strategies": [{
                           "name": "tiered_tp_atr_live_regime_dynamic",
                           "params": {}}]}]),
        sid="hl-dyn-btc",
        match="tiered_tp_atr_live_regime_dynamic",
    ),
    "regime_window_divergence": dict(
        cfg=_cfg(15, [_perps_strategy(regime_window_divergence={
            "short_window": "short", "medium_window": "medium",
            "on_divergence": {"mode": "trust_short"}})]),
        sid="hl-d-btc",
        match="regime_window_divergence",
    ),
    "policy_without_regime_enabled": dict(
        cfg=_cfg(15, [_perps_strategy(
            regime_directional_policy=_REGIME_DIRECTIONAL_POLICY)]),
        sid="hl-d-btc",
        match="regime.enabled=true",
    ),
    "policy_requires_top_level_regime": dict(
        cfg=_cfg(15, [_perps_strategy(
            regime=_REGIME_ON,
            regime_directional_policy=_REGIME_DIRECTIONAL_POLICY)]),
        sid="hl-d-btc",
        match="regime.enabled=true",
    ),
    "both_without_close": dict(
        cfg=_cfg(15, [{"id": "hl-noclose", "type": "perps",
                       "open_strategy": {"name": "tema_cross_bd"},
                       "direction": "both"}]),
        sid="hl-noclose",
        match="silently dropped",
    ),
    "invert_signal_on_spot": dict(
        cfg=_cfg(15, [{"id": "inv-x", "type": "spot",
                       "open_strategy": {"name": "sma_crossover"},
                       "close_strategy": dict(_SPOT_NEVER_FIRES_CLOSE),
                       "invert_signal": True}]),
        sid="inv-x",
        match="invert_signal",
    ),
    "invert_signal_on_futures": dict(
        cfg=_cfg(15, [{"id": "inv-x", "type": "futures",
                       "open_strategy": {"name": "sma_crossover"},
                       "close_strategy": dict(_SPOT_NEVER_FIRES_CLOSE),
                       "invert_signal": True}]),
        sid="inv-x",
        match="invert_signal",
    ),
    "allowed_regimes_with_named_gate_window": dict(
        cfg=_cfg(15, [_perps_strategy(allowed_regimes=["trending_up"],
                                      regime_gate_window="slow")],
                 regime={"enabled": True, "period": 10, "adx_threshold": 25,
                         "windows": {"slow": {"classifier": "adx",
                                              "period": 40}}}),
        sid="hl-d-btc",
        match="regime_gate_window",
    ),
    "no_open_name_and_no_args": dict(
        cfg=_cfg(15, [{"id": "spot-z", "type": "spot", "args": [],
                       "open_strategy": {"name": ""}}]),
        sid="spot-z",
        match="neither open_strategy.name nor",
    ),
    "conflicting_user_defaults_legacy_alias": dict(
        cfg=_cfg(16, [_ratchet_strategy({"use_defaults": True})],
                 user_defaults={"close": _USER_RATCHET},
                 user_close_defaults={"trailing_tp_ratchet": {"tp_tiers": [
                     {"atr_multiple": 9.0, "trailing_mult_after": 1.0,
                      "close_fraction": 0.0}]}}),
        sid="hl-r",
        inject=True,
        match="conflicts",
    ),
}


@pytest.mark.parametrize("name", list(_REJECT_CASES))
def test_load_strategy_config_rejects(tmp_path, name):
    spec = _REJECT_CASES[name]
    path = _write_full_config(tmp_path, spec["cfg"])
    with pytest.raises(ValueError, match=spec["match"]):
        run_backtest.load_strategy_config(
            path, spec["sid"], inject_user_defaults=spec.get("inject", False))


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

    assert captured["strategy_name"] == "triple_ema"
    assert captured["params"] == {
        "short_period": 3, "mid_period": 13, "long_period": 34}


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


@pytest.mark.parametrize("cfg,sid,extra_argv", [
    (
        _cfg(15, [{"id": "x", "open_strategy": {"name": "triple_ema"},
                   "close_strategies": []}]),
        "x",
        ["--direction", "short"],
    ),
    (
        _cfg(15, [_perps_strategy(allowed_regimes=["trending_up"])],
             regime=_REGIME_ON),
        "hl-d-btc",
        ["--allowed-regimes", "ranging"],
    ),
])
def test_flag_rejected_alongside_config(tmp_path, monkeypatch, cfg, sid, extra_argv):
    config_path = _write_full_config(tmp_path, cfg)
    called = {}
    monkeypatch.setattr(run_backtest, "run_single_backtest",
                        lambda *a, **kw: called.setdefault("hit", True))
    monkeypatch.setattr("sys.argv", [
        "run_backtest.py", "--mode", "single",
        "--config", config_path, "--strategy", sid, *extra_argv,
    ])
    with pytest.raises(SystemExit):
        run_backtest.main()
    assert "hit" not in called


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


def test_config_flag_threads_allowed_regimes_to_run_single(tmp_path, monkeypatch):
    config_path = _write_full_config(tmp_path, _cfg(
        15, [_perps_strategy(allowed_regimes=["trending_up", "ranging"])],
        regime=_REGIME_ON))
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
