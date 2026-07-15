"""#1159: the backtester loudly rejects an ENABLED correlated hedge block.

A hedge leg opens a second, inverse position on a different coin. The bar-level
backtester models a single instrument and would silently drop the hedge's
PnL/fees, overstating the strategy's edge — so ``load_strategy_config`` refuses
an enabled hedge block. An explicitly disabled block is inert (matching the Go
``HedgeEnabled()`` accessor) and loads clean.
"""
import json
import sys
import pathlib

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).parent.parent.parent / "shared_tools"))
sys.path.insert(0, str(pathlib.Path(__file__).parent.parent))

import run_backtest


def _write_config(tmp_path, strategy):
    cfg = {"config_version": 17, "strategies": [strategy]}
    p = tmp_path / "config.json"
    p.write_text(json.dumps(cfg, indent=2))
    return str(p)


def _hedged_strategy(hedge):
    return {
        "id": "hl-eth",
        "type": "perps",
        "platform": "hyperliquid",
        "open_strategy": {"name": "tema_cross_bd"},
        "stop_loss_atr_mult": 1.5,
        "hedge": hedge,
    }


def test_config_rejects_enabled_hedge(tmp_path):
    path = _write_config(tmp_path, _hedged_strategy(
        {"enabled": True, "symbol": "BTC", "side": "inverse", "ratio": 1.0}
    ))
    with pytest.raises(ValueError, match="hedge"):
        run_backtest.load_strategy_config(path, "hl-eth")


def test_config_allows_disabled_hedge(tmp_path):
    path = _write_config(tmp_path, _hedged_strategy(
        {"enabled": False, "symbol": "BTC"}
    ))
    # A disabled hedge block is inert and must load without error.
    kwargs = run_backtest.load_strategy_config(path, "hl-eth")
    assert kwargs["stop_loss_atr_mult"] == 1.5
