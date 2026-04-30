import importlib.util
import json
import os
import sys
import types

import pandas as pd


def _load_check_strategy():
    script_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "check_strategy.py")
    spec = importlib.util.spec_from_file_location("check_strategy_open_close_test", script_path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def test_check_strategy_open_close_uses_close_registry_loader(monkeypatch, capsys):
    module_names = ("strategies", "_go_trader_close_registry", "_strategy_registry_spot")
    saved_modules = {name: sys.modules.get(name) for name in module_names}
    for name in module_names:
        monkeypatch.delitem(sys.modules, name, raising=False)
    mod = _load_check_strategy()
    idx = pd.date_range("2024-01-01", periods=60, freq="1h")
    closes = [100.0] * 59 + [106.0]
    df = pd.DataFrame(
        {
            "open": closes,
            "high": [price + 1 for price in closes],
            "low": [price - 1 for price in closes],
            "close": closes,
            "volume": [100.0] * len(closes),
        },
        index=idx,
    )
    fake_data_fetcher = types.ModuleType("data_fetcher")
    fake_data_fetcher.fetch_ohlcv = lambda **kwargs: df
    monkeypatch.setitem(sys.modules, "data_fetcher", fake_data_fetcher)
    monkeypatch.setattr(
        sys,
        "argv",
        [
            "check_strategy.py",
            "sma_crossover",
            "BTC/USDT",
            "1h",
            "--open-strategy",
            "sma_crossover",
            "--close-strategies",
            "tp_at_pct",
            "--position-side",
            "long",
            "--position-avg-cost",
            "100",
            "--position-qty",
            "1",
        ],
    )

    try:
        mod.main()

        output = json.loads(capsys.readouterr().out)
        assert output["close_strategy"] == "tp_at_pct"
        assert output["close_fraction"] == 1.0
        assert output["signal"] == -1
    finally:
        for name, module in saved_modules.items():
            if module is None:
                sys.modules.pop(name, None)
            else:
                sys.modules[name] = module
