import json

import pytest

import monte_carlo as mc
import run_backtest
from parity_diff import config_from_live_config


def _strategy(sid, stype, platform=None, args=None, **extra):
    sc = {"id": sid, "type": stype, "args": args or ["sma_crossover", "BTC/USDT", "1h"],
          "open_strategy": {"name": "sma_crossover", "params": {"fast_period": 5, "slow_period": 20}}}
    if platform is not None:
        sc["platform"] = platform
    sc.update(extra)
    return sc


def _config(*strategies, platforms=None):
    cfg = {"config_version": 15, "strategies": list(strategies)}
    if platforms is not None:
        cfg["platforms"] = platforms
    return cfg


REFUSED = {
    "deribit_platform": (_config(_strategy("vol-btc", "options", "deribit", ["vol_mean_reversion", "BTC"])),
                         "vol-btc", "platform 'deribit' is retired"),
    "deribit_id_prefix": (_config(_strategy("deribit-spot", "spot", "binanceus")),
                          "deribit-spot", "id prefix 'deribit-'"),
    "deribit_arg_before_supported_arg": (
        _config(_strategy("ibkr-vol", "options", "ibkr",
                          ["vol_mean_reversion", "BTC", "--platform=deribit", "--platform=ibkr"])),
        "ibkr-vol", "--platform=deribit is retired"),
    "deribit_platform_settings": (
        _config(_strategy("s1", "spot", "binanceus"), platforms={"deribit": {"risk": {"max_drawdown_pct": 30}}}),
        "s1", "settings key 'deribit'"),
    "okx_options_platform": (_config(_strategy("okx-opt", "options", "okx", ["vol_mean_reversion", "BTC"])),
                             "okx-opt", "options on platform 'okx' is retired"),
    "okx_options_prefix_without_platform": (
        _config(_strategy("okx-opt", "options", None, ["vol_mean_reversion", "BTC"])),
        "okx-opt", "'okx-' id prefix and no platform field"),
    "okx_options_argument": (
        _config(_strategy("ibkr-opt", "options", "ibkr", ["vol_mean_reversion", "BTC", "--platform", "okx"])),
        "ibkr-opt", "options script argument --platform=okx"),
    "options_without_platform_on_ibkr_prefix": (
        _config(_strategy("ibkr-opt", "options", None, ["vol_mean_reversion", "BTC"])),
        "ibkr-opt", "needs an explicit platform field"),
    "options_with_blank_platform_on_rh_prefix": (
        _config(_strategy("rh-opt", "options", " ", ["wheel", "SPY"])),
        "rh-opt", "needs an explicit platform field"),
}


def _write(tmp_path, name, cfg):
    path = tmp_path / f"{name}.json"
    path.write_text(json.dumps(cfg))
    return str(path)


def test_offline_config_consumers_refuse_retired_and_missing_venues(tmp_path, subtests):
    for name, (cfg, sid, want) in REFUSED.items():
        path = _write(tmp_path, name, cfg)
        with subtests.test(case=name, consumer="run_backtest.load_strategy_config"):
            with pytest.raises(ValueError, match="config validation errors") as exc:
                run_backtest.load_strategy_config(path, sid)
            assert want in str(exc.value)
        with subtests.test(case=name, consumer="parity_diff explicit platform override"):
            with pytest.raises(ValueError) as exc:
                config_from_live_config(path, sid, platform="hyperliquid")
            assert want in str(exc.value)
        with subtests.test(case=name, consumer="monte_carlo explicit drawdown"):
            explicit = json.loads(json.dumps(cfg))
            for sc in explicit["strategies"]:
                sc["max_drawdown_pct"] = 12.5
            with pytest.raises(ValueError) as exc:
                mc.resolve_kill_switch_pct(explicit, sid)
            assert want in str(exc.value)
        with subtests.test(case=name, consumer="monte_carlo main"):
            with pytest.raises(SystemExit) as exc:
                mc.main(["--strategy", "sma_crossover", "--dataset", "BTC/USDT:1h", "--config", path,
                         "--strategy-id", sid])
            assert want in str(exc.value.code)


def test_supported_option_venues_pass_admission(subtests):
    for platform, args in (("ibkr", ["vol_mean_reversion", "BTC", "--platform=ibkr"]),
                           ("robinhood", ["wheel", "SPY", "--platform=robinhood"])):
        with subtests.test(platform=platform):
            cfg = _config(_strategy(f"{platform}-opt", "options", platform, args),
                          _strategy("okx-spot", "spot", "okx", ["sma_crossover", "BTC/USDT", "1h", "--mode=paper"]))
            assert mc.resolve_kill_switch_pct(cfg, f"{platform}-opt") == 40.0
            assert mc.resolve_kill_switch_pct(cfg, "okx-spot") == 60.0
