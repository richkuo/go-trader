import itertools

import numpy as np
import pandas as pd
import pytest

from backtester import Backtester
from atr import ensure_atr_indicator
from optimizer import DEFAULT_PARAM_RANGES
from parity_diff import ParityConfig, compute_parity_frame, summarize
from registry_loader import load_registry

NAME = "ultimate_oscillator"
CLOSE = [{"name": "tiered_tp_pct", "params": {"tp_tiers": [{"profit_pct": 0.03, "close_fraction": 1.0}]}}]


def _market(n, seed, freq="1h"):
    rng = np.random.RandomState(seed)
    t = np.arange(n)
    drift = np.where((t // 60) % 2 == 0, 0.45, -0.45)
    close = 200.0 + np.cumsum(drift + rng.randn(n) * 0.6)
    open_ = np.concatenate([[close[0]], close[:-1]])
    span = np.abs(rng.randn(n)) * 0.5 + 0.1
    df = pd.DataFrame({"open": open_, "high": np.maximum(open_, close) + span,
                       "low": np.minimum(open_, close) - span, "close": close,
                       "volume": np.ones(n)})
    df.index = pd.date_range("2025-01-01", periods=n, freq=freq)
    return df


def test_optimizer_grid_is_valid_for_the_registry_and_history_bound():
    reg = load_registry("futures")
    ranges = DEFAULT_PARAM_RANGES[NAME]
    keys = sorted(ranges)
    combos = [dict(zip(keys, v)) for v in itertools.product(*(ranges[k] for k in keys))]
    assert len(combos) == 729
    for combo in combos:
        reg._registry.validate_params(NAME, combo)
        assert max(combo["long_window"] + 1, combo["breakout_lookback"] + 2) <= 42


def test_simulator_keeps_exits_and_stop_running_through_invalid_entry_spans():
    df = _market(1500, 1664)
    df = df.drop(df.index[200::150])
    sig = ensure_atr_indicator(load_registry("futures").apply_strategy(NAME, df, None))
    bt = Backtester(initial_capital=1000.0, platform="hyperliquid",
                    open_strategy={"name": NAME, "params": {}}, close_strategies=CLOSE,
                    stop_loss_atr_mult=1.0, direction="both")
    res = bt.run(sig, strategy_name=NAME, symbol="BTC/USDT", timeframe="1h", save=False)
    trades = res["trades"]
    assert len(trades) >= 4
    assert res["close_validation"]["incomplete_parity"] is False
    pos = {ts: i for i, ts in enumerate(sig.index)}
    exits_on_invalid = 0
    reasons = set()
    last_exit = None
    for t in trades:
        entry_i = pos[pd.Timestamp(t["entry_date"])]
        exit_i = pos[pd.Timestamp(t["exit_date"])]
        want = 1 if t["side"] == "long" else -1
        assert sig["signal"].iloc[entry_i - 1] == want
        assert bool(sig["uo_valid"].iloc[entry_i - 1])
        if last_exit is not None:
            assert entry_i >= last_exit
        last_exit = exit_i
        reasons.add(t["exit_reason"].split(":")[0])
        if not bool(sig["uo_valid"].iloc[exit_i]):
            exits_on_invalid += 1
    assert {t["side"] for t in trades} == {"long", "short"}
    assert exits_on_invalid > 0
    assert "tiered_tp_pct" in reasons and len(reasons) >= 2


@pytest.mark.parametrize("window,batched", [(None, False), (200, False), (200, True)])
def test_strict_decision_parity_for_the_frozen_close_and_stop(window, batched):
    df = _market(420, 7)
    cfg = ParityConfig(strategy_name=NAME, registry="futures", platform="hyperliquid",
                       close_refs=CLOSE, direction="both", batched=batched,
                       stop_kwargs={"stop_loss_atr_mult": 1.0})
    frame = compute_parity_frame(df, cfg=cfg, window=window)
    s = summarize(frame)
    assert s["bars_compared"] > 0
    assert s["mismatches"] == 0
    assert s["clean"] is True
    if batched:
        assert s["batch_mismatches"] == 0
    assert (frame["live_open_action"] != "none").sum() > 0
