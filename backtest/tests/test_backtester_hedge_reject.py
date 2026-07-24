"""#1159: the backtester rejects hedge-enabled configs loudly (phase 1).

A correlated hedge leg is a SECOND live position on a different coin, opened
opposite the primary. The backtester simulates one instrument, so a
hedge-enabled config would silently drop the hedge's entire PnL, fee and
slippage stream and report the naked primary's results as the strategy's.

That is not a rounding error. An inverse leg sized at the primary's full
notional roughly halves realized directional PnL and doubles round-trip fees —
a backtest that ignores it measures a different strategy. Phase 1 therefore
follows the repo's live-only convention (regime_window_divergence,
tiered_tp_atr_live_regime_dynamic) and refuses rather than returning a number
an operator would reasonably trust.

An explicitly DISABLED block changes nothing live, so it must pass through:
parking a disabled hedge block on a strategy must not make it unbacktestable.
"""
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


def test_reject_message_names_the_issue_and_the_hedge_symbol(tmp_path):
    """The operator must be able to act on the message without reading code."""
    path = _config(tmp_path, _hl_strategy(
        hedge={"enabled": True, "symbol": "BTC", "ratio": 1.0},
    ))
    with pytest.raises(ValueError) as excinfo:
        run_backtest.load_strategy_config(path, "hl-test")
    msg = str(excinfo.value)
    assert "#1159" in msg
    assert "BTC" in msg
    assert "hedge.enabled=false" in msg


def test_allows_explicitly_disabled_hedge_block(tmp_path):
    path = _config(tmp_path, _hl_strategy(
        hedge={"enabled": False, "symbol": "BTC", "ratio": 1.0},
    ))
    kwargs = run_backtest.load_strategy_config(path, "hl-test")
    assert kwargs["open_strategy"] == {"name": "tema_cross", "params": {}}


def test_allows_config_without_hedge_block(tmp_path):
    path = _config(tmp_path, _hl_strategy())
    kwargs = run_backtest.load_strategy_config(path, "hl-test")
    assert kwargs["open_strategy"] == {"name": "tema_cross", "params": {}}


def test_only_the_hedge_enabled_strategy_is_rejected(tmp_path):
    """A hedge on one strategy must not make its SIBLINGS unbacktestable."""
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
