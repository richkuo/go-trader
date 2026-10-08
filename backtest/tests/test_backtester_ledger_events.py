import json
import math

import numpy as np
import pandas as pd
import pytest

from backtester import LEDGER_EVENTS_SCHEMA, LEDGER_EVENTS_SCHEMA_VERSION, Backtester
from atr import ensure_atr_indicator
from registry_loader import load_registry

SPEC = {"taker_fee_pct": 0.00045, "maker_fee_pct": 0.00015, "half_spread_pct": 0.0001,
        "slippage_pct": 0.0005, "size_decimals": 3, "min_notional_usd": 10.0, "min_notional_margin": 0.03}
TIERS = [{"name": "tiered_tp_pct", "params": {"tp_tiers": [{"profit_pct": 0.02, "close_fraction": 0.5},
                                                           {"profit_pct": 0.04, "close_fraction": 1.0}]}}]
CASES = {
    "plain_long": (dict(), {}, "sma_crossover", {"fast_period": 5, "slow_period": 20}),
    "plain_short": (dict(direction="short"), {}, "sma_crossover", {"fast_period": 5, "slow_period": 20}),
    "plain_stop_walk": (dict(stop_loss_atr_mult=1.0), {}, "ema_crossover", {"fast_period": 5, "slow_period": 15}),
    "plain_trail_bar_close": (dict(trailing_stop_atr_mult=1.5, intrabar_resolution="bar_close"), {},
                              "ema_crossover", {"fast_period": 5, "slow_period": 15}),
    "plain_scale_in": (dict(allow_scale_in=True, scale_in={"max_adds": 3}), {}, "rsi", {}),
    "engine_tiers": (dict(close_strategies=TIERS), {}, "rsi", {}),
    "engine_both_stop": (dict(direction="both", stop_loss_atr_mult=1.2,
                              close_strategies=[{"name": "tiered_tp_atr", "params": {}}]), {}, "rsi", {}),
    "engine_execution_spec": (dict(execution_spec=SPEC, close_strategies=[{"name": "tiered_tp_pct", "params": {}}]),
                              {}, "rsi", {}),
    "engine_resting_rule": (dict(execution_spec=SPEC, close_strategies=[{"name": "tiered_tp_atr", "params": {}}],
                                 resting_tp_trade_through=True), {}, "rsi", {}),
    "engine_scale_in": (dict(allow_scale_in=True, scale_in={"max_adds": 2},
                             close_strategies=[{"name": "tiered_tp_pct", "params": {}}]), {}, "rsi", {}),
    "engine_trailing_ratchet": (dict(trailing_stop_atr_mult=1.5,
                                     close_strategies=[{"name": "trailing_tp_ratchet", "params": {}}]), {}, "rsi", {}),
    "seeded_long": (dict(), {"starting_long": {"entry_price": 101.0, "entry_atr": 1.0}}, "sma_crossover",
                    {"fast_period": 5, "slow_period": 20}),
}


def _frame(n=700, seed=7):
    rng = np.random.default_rng(seed)
    idx = pd.date_range("2025-01-01", periods=n, freq="1h")
    close = np.maximum(100 + np.cumsum(rng.normal(0, 1.2, n)) + 12 * np.sin(np.arange(n) / 11.0), 5)
    open_ = np.concatenate([[close[0]], close[:-1]])
    high = np.maximum(open_, close) * (1 + rng.uniform(0, 0.01, n))
    low = np.minimum(open_, close) * (1 - rng.uniform(0, 0.01, n))
    df = pd.DataFrame({"open": open_, "high": high, "low": low, "close": close,
                       "volume": rng.uniform(10, 100, n)}, index=idx)
    df["funding_accrual"] = rng.normal(0, 0.0001, n)
    return df


def _run(name, record):
    kwargs, run_kw, strat, params = CASES[name]
    df = ensure_atr_indicator(load_registry("futures").apply_strategy(strat, _frame(), params))
    bt = Backtester(initial_capital=1000.0, platform="hyperliquid", **kwargs)
    extra = {"record_events": True} if record else {}
    return bt.run(df, strategy_name=strat, symbol="X/USDT", timeframe="1h", params=params, save=False,
                  **run_kw, **extra)


def _dump(res):
    return json.dumps(res, sort_keys=True, default=str)


@pytest.mark.parametrize("name", sorted(CASES))
def test_event_output_changes_no_existing_result_field(name):
    off = _run(name, record=False)
    on = _run(name, record=True)
    assert "ledger_events" not in off
    env = on.pop("ledger_events")
    assert env["schema"] == LEDGER_EVENTS_SCHEMA and env["schema_version"] == LEDGER_EVENTS_SCHEMA_VERSION
    assert _dump(on) == _dump(off)


@pytest.mark.parametrize("name", sorted(CASES))
def test_events_reconcile_cash_quantity_fees_and_trade_rows(name):
    res = _run(name, record=True)
    env = res["ledger_events"]
    events = env["events"]
    assert [e["seq"] for e in events] == list(range(1, len(events) + 1))
    cash, qty = env["initial_cash"], 0.0
    for e in events:
        assert e["cash_before"] == pytest.approx(cash, abs=1e-9)
        assert e["qty_before"] == pytest.approx(qty, abs=1e-12)
        assert e["kind"] == "funding" or e["position_local_id"] is not None
        cash, qty = e["cash_after"], e["qty_after"]
    assert qty == 0.0
    assert round(cash, 2) == res["final_capital"]
    end = env["interval_end"]
    pre = [e for e in events if e["kind"] != "terminal_liquidation"]
    lhs = env["initial_cash"] + math.fsum(e["gross_realized"] or 0.0 for e in pre) \
        - math.fsum(e["fee_charged"] for e in pre) + math.fsum(e["funding_cash"] or 0.0 for e in pre)
    rhs = end["cash"] + end["position_qty"] * (end["avg_cost"] or 0.0)
    assert lhs == pytest.approx(rhs, abs=1e-6)
    charged = math.fsum(e["fee_charged"] for e in pre if e["kind"] in ("open", "scale_in", "seed_inventory"))
    allocated = math.fsum(e["entry_fee_allocated"] or 0.0 for e in pre)
    assert charged == pytest.approx(allocated + end["entry_fee_outstanding"], abs=1e-9)
    closes = [e for e in events if e["kind"] in ("close", "terminal_liquidation")]
    assert len(closes) == len(res["trades"])
    for e, row in zip(closes, res["trades"]):
        assert row["exit_reason"] == e["reason"]
        assert row["exit_fee"] == pytest.approx(e["fee_charged"], abs=5e-7)
        assert row["entry_fee"] == pytest.approx(e["entry_fee_allocated"], abs=5e-7)
        net = e["gross_realized"] - e["entry_fee_allocated"] - e["fee_charged"]
        assert abs(row["pnl"] - net) <= 0.005 + 1e-9
    terminal = [e for e in events if e["kind"] == "terminal_liquidation"]
    assert all(e["synthetic"] for e in terminal)
    if terminal:
        assert end["position_qty"] != 0 and terminal[0]["qty_before"] == end["position_qty"]
        assert events[-1] is terminal[0]


def test_scale_in_and_partial_closes_allocate_each_entry_fee_once():
    env = _run("engine_scale_in", record=True)["ledger_events"]
    by_pos = {}
    for e in env["events"]:
        if e["kind"] != "funding":
            by_pos.setdefault(e["position_local_id"], []).append(e)
    mixed = [evs for evs in by_pos.values()
             if any(e["kind"] == "scale_in" for e in evs) and sum(e["kind"] == "close" for e in evs) >= 2]
    assert mixed
    for evs in mixed:
        charged = math.fsum(e["fee_charged"] for e in evs if e["kind"] in ("open", "scale_in"))
        allocated = math.fsum(e["entry_fee_allocated"] or 0.0 for e in evs)
        last = evs[-1]
        outstanding = last["entry_fee_outstanding_after"] if last["qty_after"] != 0 else 0.0
        assert charged == pytest.approx(allocated + outstanding, abs=1e-9)
        adds = [e for e in evs if e["kind"] == "scale_in"]
        for a in adds:
            assert a["qty_after"] == pytest.approx(a["qty_before"] + (a["quantity"] if a["side"] == "long" else -a["quantity"]))
            assert a["timing"] == "bar_open_fill" and a["decision_timestamp"] < a["bar_timestamp"]
