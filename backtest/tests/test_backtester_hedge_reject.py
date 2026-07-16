"""#1159 phase 1: correlated hedge legs are HL-perps-live-only. The bar-level
backtester has no resolver hook to mirror the scheduler's fill-driven hedge
mirroring or the per-cycle coherence-sync backstop, so a hedge-enabled
config must be rejected loudly by the ``--config`` loader rather than
silently backtesting the primary leg as if it were unhedged (or worse,
inventing hedge PnL/fee modeling that doesn't exist yet).
"""
import json

import pytest

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


def test_loader_rejects_hedge_enabled(tmp_path):
    path = _config(tmp_path, _hl_strategy(
        hedge={"enabled": True, "symbol": "BTC", "ratio": 1.0, "leverage": 3},
    ))
    with pytest.raises(ValueError, match="hedge"):
        run_backtest.load_strategy_config(path, "hl-test")


def test_loader_accepts_hedge_disabled(tmp_path):
    path = _config(tmp_path, _hl_strategy(
        hedge={"enabled": False, "symbol": "BTC"},
    ))
    kwargs = run_backtest.load_strategy_config(path, "hl-test")
    assert kwargs is not None


def test_loader_accepts_no_hedge_block(tmp_path):
    path = _config(tmp_path, _hl_strategy())
    kwargs = run_backtest.load_strategy_config(path, "hl-test")
    assert kwargs is not None
