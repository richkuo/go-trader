import json
import pathlib
import sys

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).parent.parent.parent / "shared_tools"))
sys.path.insert(0, str(pathlib.Path(__file__).parent.parent))

import run_backtest


def _config(tmp_path, strategy):
    cfg = {"config_version": 16, "strategies": [strategy]}
    path = tmp_path / "config.json"
    path.write_text(json.dumps(cfg))
    return str(path)


def _hl_strategy(**over):
    sc = {
        "id": "hl-test", "type": "perps", "platform": "hyperliquid",
        "script": "shared_scripts/check_hyperliquid.py",
        "args": ["tema_cross", "ETH", "4h", "--mode", "paper"],
        "capital": 1000, "max_drawdown_pct": 50,
        "open_strategy": {"name": "tema_cross", "params": {}},
        "stop_loss_atr_mult": 2.0,
    }
    sc.update(over)
    return sc


def test_rejects_enabled_hedge_block(tmp_path):
    path = _config(tmp_path, _hl_strategy(
        hedge={"enabled": True, "symbol": "BTC", "ratio": 1.0},
    ))
    with pytest.raises(ValueError, match="correlated"):
        run_backtest.load_strategy_config(path, "hl-test")


@pytest.mark.parametrize("hedge", [
    {"enabled": False, "symbol": "BTC", "ratio": 1.0},
    None,
])
def test_allows_config_without_enabled_hedge(tmp_path, hedge):
    over = {} if hedge is None else {"hedge": hedge}
    path = _config(tmp_path, _hl_strategy(**over))
    kwargs = run_backtest.load_strategy_config(path, "hl-test")
    assert kwargs["open_strategy"] == {"name": "tema_cross", "params": {}}


def test_only_the_hedge_enabled_strategy_is_rejected(tmp_path):
    cfg = {
        "config_version": 16,
        "strategies": [
            _hl_strategy(id="hl-hedged", hedge={"enabled": True, "symbol": "BTC"}),
            _hl_strategy(id="hl-plain"),
        ],
    }
    path = tmp_path / "config.json"
    path.write_text(json.dumps(cfg))

    kwargs = run_backtest.load_strategy_config(str(path), "hl-plain")
    assert kwargs["open_strategy"] == {"name": "tema_cross", "params": {}}
    with pytest.raises(ValueError, match="correlated"):
        run_backtest.load_strategy_config(str(path), "hl-hedged")
