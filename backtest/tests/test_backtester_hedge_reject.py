"""#1159 phase 1: the backtester loud-rejects an ENABLED `hedge` config block
(HL-live-only, single-instrument model has no hedge PnL/fee parity), while an
explicitly disabled block passes through unchanged, mirroring the existing
regime_window_divergence reject pattern in run_backtest.load_strategy_config.
"""
import json

import pytest

import run_backtest


def _write_config(tmp_path, strategies, version=15):
    cfg = {"config_version": version, "strategies": strategies}
    p = tmp_path / "config.json"
    p.write_text(json.dumps(cfg, indent=2))
    return str(p)


def _hedged_strategy(hedge):
    return {
        "id": "hl-eth-hedged",
        "type": "perps",
        "platform": "hyperliquid",
        "open_strategy": {"name": "tema_cross_bd"},
        "close_strategy": {"name": "tiered_tp_atr", "params": {"tp_tiers": [
            {"atr_multiple": 2.0, "close_fraction": 1.0},
        ]}},
        "hedge": hedge,
    }


def test_load_strategy_config_rejects_enabled_hedge(tmp_path):
    path = _write_config(tmp_path, [_hedged_strategy({
        "enabled": True, "symbol": "BTC", "side": "inverse", "ratio": 1.0,
    })])
    with pytest.raises(ValueError, match="HL-live-only in phase 1"):
        run_backtest.load_strategy_config(path, "hl-eth-hedged")


def test_load_strategy_config_rejects_enabled_hedge_default_true(tmp_path):
    # "enabled" omitted defaults to True in the reject check (mirrors the
    # config's own JSON `enabled bool` zero-value semantics being explicit
    # opt-in at the Go layer — a present hedge block without an explicit
    # false must not silently pass backtesting).
    path = _write_config(tmp_path, [_hedged_strategy({
        "symbol": "BTC", "side": "inverse", "ratio": 1.0,
    })])
    with pytest.raises(ValueError, match="HL-live-only in phase 1"):
        run_backtest.load_strategy_config(path, "hl-eth-hedged")


def test_load_strategy_config_allows_disabled_hedge(tmp_path):
    path = _write_config(tmp_path, [_hedged_strategy({
        "enabled": False, "symbol": "BTC",
    })])
    kwargs = run_backtest.load_strategy_config(path, "hl-eth-hedged")
    assert kwargs["open_strategy"]["name"] == "tema_cross_bd"


def test_load_strategy_config_allows_no_hedge_block(tmp_path):
    strat = _hedged_strategy({"enabled": True, "symbol": "BTC"})
    del strat["hedge"]
    path = _write_config(tmp_path, [strat])
    kwargs = run_backtest.load_strategy_config(path, "hl-eth-hedged")
    assert kwargs["open_strategy"]["name"] == "tema_cross_bd"
