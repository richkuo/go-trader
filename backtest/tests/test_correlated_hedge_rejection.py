"""#1159: a live correlated hedge must never silently backtest primary-only."""

from __future__ import annotations

import json
import sys
from pathlib import Path

import pytest

REPO_ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(REPO_ROOT / "backtest"))

from run_backtest import load_strategy_config  # noqa: E402


def test_load_strategy_config_rejects_enabled_correlated_hedge(tmp_path):
    path = tmp_path / "config.json"
    path.write_text(json.dumps({
        "config_version": 17,
        "strategies": [{
            "id": "hl-eth",
            "type": "perps",
            "platform": "hyperliquid",
            "args": ["tema_cross_bd", "ETH", "1h"],
            "hedge": {"enabled": True, "symbol": "BTC/USDC:USDC", "ratio": 0.5},
        }],
    }))

    with pytest.raises(ValueError, match="correlated hedge leg.*live-only"):
        load_strategy_config(str(path), "hl-eth")


def test_load_strategy_config_allows_disabled_correlated_hedge(tmp_path):
    path = tmp_path / "config.json"
    path.write_text(json.dumps({
        "config_version": 17,
        "strategies": [{
            "id": "hl-eth",
            "type": "perps",
            "platform": "hyperliquid",
            "args": ["tema_cross_bd", "ETH", "1h"],
            "hedge": {"enabled": False, "symbol": "BTC/USDC:USDC"},
        }],
    }))

    assert load_strategy_config(str(path), "hl-eth")["open_strategy"]["name"] == "tema_cross_bd"
