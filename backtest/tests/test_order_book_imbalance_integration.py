import json
import os
import sys

import numpy as np
import pytest

_HERE = os.path.dirname(os.path.abspath(__file__))
_BACKTEST = os.path.abspath(os.path.join(_HERE, ".."))
_ROOT = os.path.abspath(os.path.join(_BACKTEST, ".."))
for _p in (_BACKTEST, os.path.join(_ROOT, "shared_tools"), os.path.join(_ROOT, "shared_strategies", "open")):
    if _p not in sys.path:
        sys.path.insert(0, _p)

import book_archive as ba  # noqa: E402
import book_event_simulator as sim  # noqa: E402
import order_book_imbalance as obi  # noqa: E402
from book_inputs import CONTRACT
from book_replay import book_identity  # noqa: E402
from registry_loader import load_registry  # noqa: E402

NAME = "order_book_imbalance"
T0 = ba._day_ms("20260801")
PARAM_CASES = os.path.join(_ROOT, "shared_strategies", "open", "testdata", "order_book_imbalance_param_cases.json")
SPEC = os.path.join(_BACKTEST, "candidates", "order_book_imbalance_1635", "candidate_spec.json")


def levels(best_bid, best_ask, bid_sz, ask_sz, count=20, step=1):
    bids = [[f"{best_bid - i * step}", f"{bid_sz}", 1] for i in range(count)]
    asks = [[f"{best_ask + i * step}", f"{ask_sz}", 1] for i in range(count)]
    return bids, asks


def obs(seq, recv, bid_sz, ask_sz, event=None, coin="BTC", count=20, best_bid=100000, best_ask=100001):
    event = recv - 300 if event is None else event
    bids, asks = levels(best_bid, best_ask, bid_sz, ask_sz, count)
    identity = book_identity(coin, event, [tuple(b) for b in bids], [tuple(a) for a in asks])
    return {"seq": seq, "event_ms": event, "recv_ms": recv, "session": 1, "identity": identity, "bid_depth": count,
            "ask_depth": count, "bids": bids, "asks": asks}


def section(observations, markers, cutoff, depth=5, coin="BTC"):
    return {"kind": "l2_book", "coin": coin, **CONTRACT, "window_ms": 60_000, "capacity": 256, "depth": depth,
            "cutoff_ms": cutoff, "retained_from_ms": cutoff - 60_000, "prefix_complete": True, "owner_instance": "fixture",
            "snapshot_id": "fixture", "available": True,
            "observations": [{**o, "bids": o["bids"][:depth], "asks": o["asks"][:depth]} for o in observations
                             if cutoff - 60_000 <= o["recv_ms"] <= cutoff],
            "markers": [m for m in markers if cutoff - 60_000 <= m["at_ms"] <= cutoff]}


def marker(seq, at, reason="subscribed"):
    return {"seq": seq, "at_ms": at, "reason": reason, "session": 1, "detail": ""}


P = {"depth_levels": 5, "entry_threshold": 0.3, "confirmation_observations": 3, "max_spread_bps": 5.0,
     "min_combined_depth_usd": 100000.0, "max_snapshot_age_ms": 2000}


def long_run(base=T0 + 10_000, n=4, bid=3, ask=1):
    out = [marker(1, base - 9_000)]
    books = [obs(2, base - 6_000, 1, 1)]
    for i in range(n):
        books.append(obs(3 + i, base + i * 5_400, bid, ask))
    return books, out


def decide(books, marks, deadline, p=P):
    return obi.evaluate_section(section(books, marks, deadline, p["depth_levels"]), "BTC", p, deadline)


def test_grid_matches_the_spec_and_the_mirrored_validator():
    registry = load_registry("futures")._registry
    entry = registry.STRATEGIES[NAME]
    from optimizer import DEFAULT_PARAM_RANGES
    with open(SPEC) as fh:
        spec = json.load(fh)
    assert DEFAULT_PARAM_RANGES[NAME] == spec["parameters"]["search"] == obi.SEARCH
    with open(PARAM_CASES) as fh:
        cases = json.load(fh)
    assert len(cases) >= 20
    for case in cases:
        if case["valid"]:
            obi.validate_params(case["params"])
            registry.validate_params(NAME, case["params"], entry["default_params"])
        else:
            with pytest.raises(ValueError):
                obi.validate_params(case["params"])


def _market(books_by, minutes=120, marks=None, funding=None, eligible=True):
    first = T0 - 30 * sim.MINUTE_MS
    bars = {c: sim.Bars(cb, first, T0 + minutes * sim.MINUTE_MS) for c, cb in books_by.items()}
    mk = {}
    for c, cb in books_by.items():
        rows = {}
        for k in range(-30, minutes):
            t = T0 + k * sim.MINUTE_MS
            rows[t] = (100100.0, 100100.0)
        if marks:
            rows.update(marks.get(c, {}))
        mk[c] = sim.Marks(rows)
    return {"books": books_by, "bars": bars, "marks": mk, "funding": funding or {c: {} for c in books_by},
            "eligible_days": {T0 - T0 % sim.DAY_MS} if eligible else set(), "sz_decimals": {c: 5 for c in books_by},
            "max_leverage": {c: 40.0 for c in books_by}}


def _cfg(**over):
    base = {"settle_ms": 5000, "processing_ms": 250, "taker_fee_pct": 0.00045, "slippage_bps": 5.0, "margin_per_entry": 100.0,
            "leverage": 1, "initial_equity": 10000.0, "min_notional_usd": 10.0, "pre_entry_stop_pct": 1.0, "daily_loss_pct": 2.0,
            "drawdown_latch_pct": 10.0, "time_stop_bars": 5, "purge_ms": 300_000, "exit_tail_ms": 1_800_000, "fill_within_ms": 10_000}
    base.update(over)
    return base


def _steady(start=T0 - 30 * sim.MINUTE_MS, end=T0 + 120 * sim.MINUTE_MS, step=5_000, ask_sz="1", count=20):
    out, seq, t = [], 2, start + 1_000
    while t < end:
        bump = (seq % 5) * 2
        out.append(obs(seq, t, "1", ask_sz, count=count, best_bid=100000 + bump, best_ask=100001 + bump))
        seq += 1
        t += step
    return out


def test_entries_fill_strictly_after_the_decision_plus_latency_and_exit_on_time():
    books = _steady()
    cb = sim.CoinBooks("BTC", [books], [marker(1, T0 - 40 * sim.MINUTE_MS)], 1, 5)
    market = _market({"BTC": cb})
    D = T0 + 10 * sim.MINUTE_MS
    split = {"start_ms": T0, "end_ms": T0 + 100 * sim.MINUTE_MS}
    res = sim.simulate([{"deadline_ms": D, "coin": "BTC", "direction": 1, "entry_id": "a"}], market, _cfg(), split)
    pos = res["positions"][0]
    assert pos["fill_ms"] >= D + 5250 and pos["fill_ms"] - (D + 5250) < 5_000
    assert pos["exit_reason"] == "time_stop" and pos["exit_ms"] >= obi.ceil_grid(pos["fill_ms"] + 300_000) + 5250
    fill_i = int(np.searchsorted(cb.recv, pos["fill_ms"]))
    ref = int(cb.best_ask[fill_i]) / cb.px_scale
    assert pos["qty"] == pytest.approx(0.00099) and pos["entry_price"] == pytest.approx(ref * 1.0005)
    assert res["closed"] == 1 and res["held_complete"]
    exact = sim.find_book(cb, D, D + 5250, 10_000)
    assert int(cb.recv[exact]) >= D + 5250 and int(cb.recv[exact - 1]) < D + 5250
    res2 = sim.simulate([{"deadline_ms": D, "coin": "BTC", "direction": 1, "entry_id": "a"},
                         {"deadline_ms": D + 60_000, "coin": "BTC", "direction": -1, "entry_id": "b"}], market, _cfg(), split)
    assert res2["refusals"].get("position_open") == 1


def test_stop_on_marks_funding_partial_fill_minimum_and_terminal_inventory():
    books = _steady(ask_sz="0.0004")
    cb = sim.CoinBooks("BTC", [books], [marker(1, T0 - 40 * sim.MINUTE_MS)], 1, 5)
    D = T0 + 58 * sim.MINUTE_MS
    drop = {"BTC": {T0 + 60 * sim.MINUTE_MS + k * sim.MINUTE_MS: (90000.0, 90000.0) for k in range(5)}}
    market = _market({"BTC": cb}, marks=drop, funding={"BTC": {T0 + 60 * sim.MINUTE_MS: 0.0001}})
    split = {"start_ms": T0, "end_ms": T0 + 110 * sim.MINUTE_MS}
    res = sim.simulate([{"deadline_ms": D, "coin": "BTC", "direction": 1, "entry_id": "a"}], market, _cfg(), split)
    pos = res["positions"][0]
    assert pos["qty"] == pytest.approx(0.00099)
    assert pos["exit_reason"] == "stop" and pos["exit_ms"] > T0 + 60 * sim.MINUTE_MS
    assert pos["funding"] == pytest.approx(pos["qty"] * 90000.0 * 0.0001)
    thin = sim.CoinBooks("BTC", [_steady(ask_sz="0.0002", count=2)], [marker(1, T0 - 40 * sim.MINUTE_MS)], 1, 5)
    res = sim.simulate([{"deadline_ms": D, "coin": "BTC", "direction": 1, "entry_id": "a"}], _market({"BTC": thin}, marks=drop), _cfg(), split)
    assert res["positions"][0]["qty"] == pytest.approx(0.0004) and res["closed"] == 1
    tiny = sim.CoinBooks("BTC", [_steady(ask_sz="0.00001", count=1)], [marker(1, T0 - 40 * sim.MINUTE_MS)], 1, 5)
    res = sim.simulate([{"deadline_ms": D, "coin": "BTC", "direction": 1, "entry_id": "a"}], _market({"BTC": tiny}), _cfg(), split)
    assert res["refusals"] == {"below_minimum": 1}
    cut = [b for b in books if b["recv_ms"] < D + 20_000]
    gone = sim.CoinBooks("BTC", [cut], [marker(1, T0 - 40 * sim.MINUTE_MS)], 1, 5)
    res = sim.simulate([{"deadline_ms": D, "coin": "BTC", "direction": -1, "entry_id": "a"}], _market({"BTC": gone}), _cfg(), split)
    assert res["terminal"] == 1 and not res["held_complete"]
    res = sim.simulate([{"deadline_ms": D, "coin": "BTC", "direction": 1, "entry_id": "a"}], _market({"BTC": cb}, eligible=False), _cfg(), split)
    assert res["refusals"] == {"ineligible_day": 1}
    res = sim.simulate([{"deadline_ms": split["end_ms"] - 60_000, "coin": "BTC", "direction": 1, "entry_id": "a"}], market, _cfg(), split)
    assert res["refusals"] == {"purge": 1}


