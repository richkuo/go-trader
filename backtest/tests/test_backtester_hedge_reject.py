"""#1159 phase 1: the hedge block is HL-live(+paper)-only — no bar-level PnL/
fee/slippage model exists for the hedge leg yet (parity modeling is a
follow-up). ``--config`` must reject a hedge-enabled strategy loudly rather
than silently backtest the primary leg alone under a config that live would
run hedged. Mirrors the ``regime_window_divergence`` reject test shape.
"""
import json
import pathlib
import sys

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).parent.parent))

import run_backtest


def _config(tmp_path, strategy):
    cfg = {"config_version": 18, "strategies": [strategy]}
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


def test_loader_rejects_enabled_hedge_block(tmp_path):
    path = _config(tmp_path, _hl_strategy(
        hedge={"enabled": True, "symbol": "BTC", "ratio": 1.0},
    ))
    with pytest.raises(ValueError, match="hedge"):
        run_backtest.load_strategy_config(path, "hl-test")


def test_loader_accepts_disabled_hedge_block(tmp_path):
    path = _config(tmp_path, _hl_strategy(
        hedge={"enabled": False, "symbol": "BTC", "ratio": 1.0},
    ))
    # Must not raise — enabled=false is inert, matching the Go accessor.
    run_backtest.load_strategy_config(path, "hl-test")


def test_loader_accepts_no_hedge_block(tmp_path):
    path = _config(tmp_path, _hl_strategy())
    run_backtest.load_strategy_config(path, "hl-test")
