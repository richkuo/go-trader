"""#1159 phase 1: correlated hedge legs are HL-live-only — the backtester's
--config reader must reject an enabled hedge block loudly instead of silently
backtesting only the primary leg (which would misstate PnL/fees vs live).
Mirrors the regime_window_divergence reject pattern.
"""
import json

import pytest

import run_backtest


def _write_config(tmp_path, strategies):
    cfg = {"config_version": 15, "strategies": strategies}
    p = tmp_path / "config.json"
    p.write_text(json.dumps(cfg, indent=2))
    return str(p)


def _hedge_strategy(hedge):
    return {
        "id": "hl-x",
        "type": "perps",
        "platform": "hyperliquid",
        "args": ["tema_cross_bd", "ETH", "--mode=live"],
        "open_strategy": {"name": "tema_cross_bd"},
        "hedge": hedge,
    }


def test_enabled_hedge_block_rejected(tmp_path):
    path = _write_config(tmp_path, [_hedge_strategy({
        "enabled": True,
        "symbol": "BTC/USDC:USDC",
        "side": "inverse",
        "ratio": 1.0,
        "platform": "hyperliquid",
        "type": "perps",
        "margin_mode": "cross",
        "leverage": 3,
    })])
    with pytest.raises(ValueError, match="hedge.*HL-live-only.*1159"):
        run_backtest.load_strategy_config(path, "hl-x")


def test_disabled_hedge_block_tolerated(tmp_path):
    path = _write_config(tmp_path, [_hedge_strategy({
        "enabled": False,
        "symbol": "BTC/USDC:USDC",
        "side": "inverse",
        "ratio": 1.0,
    })])
    kwargs = run_backtest.load_strategy_config(path, "hl-x")
    assert kwargs["open_strategy"]["name"] == "tema_cross_bd"
