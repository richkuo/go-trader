import json

import pytest

from run_backtest import load_strategy_config


def _config(tmp_path, enabled):
    path = tmp_path / "config.json"
    path.write_text(json.dumps({
        "config_version": 17,
        "strategies": [{
            "id": "alpha",
            "type": "perps",
            "platform": "hyperliquid",
            "args": ["hold", "ETH", "paper"],
            "open_strategy": {"name": "hold"},
            "hedge": {"enabled": enabled, "symbol": "BTC"},
        }],
    }))
    return path


def test_enabled_hedge_rejected_loudly(tmp_path):
    with pytest.raises(ValueError, match="backtester parity deferred.*#1159"):
        load_strategy_config(str(_config(tmp_path, True)), "alpha")


def test_disabled_hedge_is_inert(tmp_path):
    loaded = load_strategy_config(str(_config(tmp_path, False)), "alpha")
    assert loaded["open_strategy"]["name"] == "hold"
