"""#1159: enabled live hedge blocks must not silently backtest as primary-only."""

import json

import pytest

import run_backtest


def _write_config(tmp_path, hedge):
    path = tmp_path / "config.json"
    path.write_text(json.dumps({
        "config_version": 17,
        "strategies": [{
            "id": "hl-btc",
            "type": "perps",
            "platform": "hyperliquid",
            "open_strategy": {"name": "tema_cross_bd"},
            "hedge": hedge,
        }],
    }))
    return str(path)


def test_enabled_hedge_rejected_for_backtest(tmp_path):
    with pytest.raises(ValueError, match="hedge block.*HL-live-only"):
        run_backtest.load_strategy_config(_write_config(tmp_path, {
            "enabled": True,
            "symbol": "ETH/USDC:USDC",
            "side": "inverse",
            "ratio": 1.0,
            "platform": "hyperliquid",
            "type": "perps",
        }), "hl-btc")


def test_disabled_hedge_remains_backtest_compatible(tmp_path):
    kwargs = run_backtest.load_strategy_config(_write_config(tmp_path, {"enabled": False}), "hl-btc")
    assert "hedge" not in kwargs
