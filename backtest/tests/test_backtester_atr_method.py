import contextlib
import json
import pathlib
import sys

import numpy as np
import pandas as pd
import pytest

sys.path.insert(0, str(pathlib.Path(__file__).parent.parent.parent / "shared_tools"))
sys.path.insert(0, str(pathlib.Path(__file__).parent.parent))

import run_backtest
from backtester import Backtester


def _write_config(tmp_path, cfg):
    p = tmp_path / "config.json"
    p.write_text(json.dumps(cfg, indent=2))
    return str(p)


def _base_config(global_atr=None, strategy_atr=None):
    sc = {
        "id": "hl-temacb-btc",
        "type": "perps",
        "platform": "hyperliquid",
        "open_strategy": {"name": "tema_cross_bd"},
        "close_strategy": {"name": "tiered_tp_atr", "params": {"tp_tiers": [
            {"atr_multiple": 2.0, "close_fraction": 1.0},
        ]}},
    }
    if strategy_atr is not None:
        sc["atr_method"] = strategy_atr
    cfg = {"config_version": 15, "strategies": [sc]}
    if global_atr is not None:
        cfg["atr_method"] = global_atr
    return cfg


@contextlib.contextmanager
def _raises_for(case, exc=ValueError, match=None):
    try:
        with pytest.raises(exc, match=match) as info:
            yield info
    except (AssertionError, pytest.fail.Exception) as err:
        raise AssertionError(f"{case}: {err}") from None


def test_backtester_normalizes_and_rejects_atr_method():
    with _raises_for("test_backtester_rejects_unknown_atr_method", match="atr_method"):
        Backtester(atr_method="rma")
    for raw, want in [
        ("simple", "simple"),
        ("wilder", "wilder"),
        (" Wilder ", "wilder"),
        ("", "simple"),
        (None, "simple"),
    ]:
        assert Backtester(atr_method=raw).atr_method == want, f"test_backtester_normalizes_atr_method[{raw!r}]"


def _big_ohlcv(n=80, seed=3):
    rng = np.random.default_rng(seed)
    close = 50_000 + np.cumsum(rng.normal(0, 300, n))
    high = close + rng.uniform(100, 500, n)
    low = close - rng.uniform(100, 500, n)
    df = pd.DataFrame(
        {"open": close, "high": high, "low": low, "close": close,
         "volume": np.full(n, 100.0)},
        index=pd.date_range("2024-01-01", periods=n, freq="1h"),
    )
    df["signal"] = 0
    df.iloc[20, df.columns.get_loc("signal")] = 1
    return df


def test_wilder_changes_stamped_entry_atr():
    df = _big_ohlcv()
    results = {}
    for method in ("simple", "wilder"):
        bt = Backtester(stop_loss_atr_mult=2.0, atr_method=method)
        res = bt.run(df.copy(), strategy_name="t", symbol="BTC/USDT", timeframe="1h")
        trades = res["trades"]
        assert trades, f"{method}: expected at least one trade"
        results[method] = res
    assert (
        results["simple"]["trades"] != results["wilder"]["trades"]
        or results["simple"]["metrics"] != results["wilder"]["metrics"]
    )


def test_config_resolves_and_rejects_atr_method(tmp_path):
    for global_atr, strategy_atr, want in [
        (None, None, "simple"),
        ("wilder", None, "wilder"),
        ("wilder", "simple", "simple"),
    ]:
        kwargs = run_backtest.load_strategy_config(
            _write_config(tmp_path, _base_config(global_atr=global_atr, strategy_atr=strategy_atr)),
            "hl-temacb-btc")
        assert kwargs["atr_method"] == want, f"test_config_resolves_atr_method[{global_atr}-{strategy_atr}]"
    for global_atr, strategy_atr in [
        ("rma", None),
        ("simple", "bogus"),
    ]:
        with _raises_for(f"test_config_rejects_unknown_atr_method[{global_atr}-{strategy_atr}]", match="atr_method"):
            run_backtest.load_strategy_config(
                _write_config(tmp_path, _base_config(global_atr=global_atr, strategy_atr=strategy_atr)),
                "hl-temacb-btc")
