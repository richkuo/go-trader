"""#1159: the backtester must reject an ENABLED correlated hedge block.

The single-instrument simulator has no way to model the hedge leg's PnL, fees,
or funding. Silently dropping it would report the primary's standalone edge as
if it were the hedged strategy's — the exact misdiagnosis the repo's
loud-reject pattern exists to prevent. An explicitly DISABLED block changes
nothing live, so it must still backtest.
"""

import importlib.util
import json
import pathlib
import tempfile

import pytest

_SPEC = importlib.util.spec_from_file_location(
    "run_backtest_hedge_reject",
    pathlib.Path(__file__).resolve().parent / "run_backtest.py",
)
run_backtest = importlib.util.module_from_spec(_SPEC)
_SPEC.loader.exec_module(run_backtest)


def _write_config(hedge):
    strategy = {
        "id": "hl-eth",
        "type": "perps",
        "platform": "hyperliquid",
        "script": "shared_scripts/check_hyperliquid.py",
        "args": ["--mode=live", "ETH", "1h"],
        "open_strategy": {"name": "ema_cross", "params": {}},
    }
    if hedge is not None:
        strategy["hedge"] = hedge
    cfg = {"config_version": 17, "strategies": [strategy]}
    fh = tempfile.NamedTemporaryFile("w", suffix=".json", delete=False)
    json.dump(cfg, fh)
    fh.close()
    return fh.name


def test_enabled_hedge_block_is_rejected():
    path = _write_config({"enabled": True, "symbol": "BTC", "ratio": 1.0})
    with pytest.raises(ValueError) as excinfo:
        run_backtest.load_strategy_config(path, "hl-eth")
    msg = str(excinfo.value)
    assert "hedge" in msg
    assert "#1159" in msg


def test_disabled_hedge_block_still_backtests():
    path = _write_config({"enabled": False, "symbol": "BTC", "ratio": 1.0})
    out = run_backtest.load_strategy_config(path, "hl-eth")
    assert out["open_strategy"]["name"] == "ema_cross"


def test_no_hedge_block_is_unaffected():
    path = _write_config(None)
    out = run_backtest.load_strategy_config(path, "hl-eth")
    assert out["open_strategy"]["name"] == "ema_cross"
