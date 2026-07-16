"""#1159: the backtester has no PnL/fee/slippage model for a correlated
hedge leg — it's an HL-perps-LIVE-only surface (the scheduler places a real
second on-chain order mirroring the primary's lifecycle). load_strategy_config
must reject a hedge-enabled strategy loudly rather than silently backtest the
primary leg alone and produce a result that diverges from live's actual net
exposure/PnL.
"""
import json

import pytest

import run_backtest


def _write_config(tmp_path, strategies, version=15):
    cfg = {"config_version": version, "strategies": strategies}
    p = tmp_path / "config.json"
    p.write_text(json.dumps(cfg, indent=2))
    return str(p)


def _hedge_enabled_strategy(**hedge_overrides):
    hedge = {
        "enabled": True,
        "symbol": "BTC",
        "side": "inverse",
        "ratio": 1.0,
        "margin_mode": "isolated",
        "leverage": 3,
    }
    hedge.update(hedge_overrides)
    return {
        "id": "hl-temacb-eth",
        "type": "perps",
        "platform": "hyperliquid",
        "args": ["tema_cross_bd", "ETH", "1h", "--mode=live"],
        "open_strategy": {"name": "tema_cross_bd"},
        "hedge": hedge,
    }


def test_load_strategy_config_rejects_hedge_enabled_strategy(tmp_path):
    path = _write_config(tmp_path, [_hedge_enabled_strategy()])
    with pytest.raises(ValueError, match="correlated hedge leg"):
        run_backtest.load_strategy_config(path, "hl-temacb-eth")


def test_load_strategy_config_accepts_hedge_disabled_strategy(tmp_path):
    strategy = _hedge_enabled_strategy(enabled=False)
    path = _write_config(tmp_path, [strategy])
    # Must NOT raise — a present-but-disabled hedge block is identical to no
    # hedge at all.
    kwargs = run_backtest.load_strategy_config(path, "hl-temacb-eth")
    assert kwargs["open_strategy"]["name"] == "tema_cross_bd"


def test_load_strategy_config_accepts_strategy_without_hedge_block(tmp_path):
    strategy = _hedge_enabled_strategy()
    del strategy["hedge"]
    path = _write_config(tmp_path, [strategy])
    kwargs = run_backtest.load_strategy_config(path, "hl-temacb-eth")
    assert kwargs["open_strategy"]["name"] == "tema_cross_bd"
