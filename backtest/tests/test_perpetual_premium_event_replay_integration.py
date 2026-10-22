import importlib.util
import json
import os
from decimal import Decimal

import pytest

_REPO = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", ".."))
_STUDY = os.path.join(_REPO, "backtest", "candidates", "perpetual_premium_reversion_1639")
_SPEC = importlib.util.spec_from_file_location("_ppr_1639_run_study", os.path.join(_STUDY, "run_study.py"))
study = importlib.util.module_from_spec(_SPEC)
_SPEC.loader.exec_module(study)
er = study.er

T = 1_800_000_000_000
BAR = 60_000
FEE = er.COST_CASES["x1"]["taker_fee_pct"]


def q(recv, bid, bid_sz, ask, ask_sz, seq=0):
    return [recv, recv - 100, str(bid), str(bid_sz), str(ask), str(ask_sz), 1, seq or recv]


def decision(ms, signal=1):
    return {"decision_ms": ms, "signal": signal, "atr": None, "premium": None, "z_extreme": None}


def replay(quotes, decisions, funding=(), case="x1", coin="BTC", decimals=5, capital=1200.0):
    return er.replay_coin(coin, decisions, er.QuoteTape(quotes), list(funding), decimals, capital, 1, BAR, 1.0,
                          er.COST_CASES[case])


def d(x):
    return Decimal(repr(x))


def assert_quantity_conserved(rec):
    closed = sum((d(f["qty"]) for f in rec["exit_fills"]), Decimal(0))
    entered = sum((d(f["qty"]) for f in rec["entry_fills"]), Decimal(0))
    assert d(rec["qty"]) == entered
    remaining = d(rec.get("remaining_qty", 0.0))
    assert entered == closed + remaining
    if "closed_qty" in rec:
        assert d(rec["closed_qty"]) == closed


def assert_quote_capacity(res, quotes):
    shown = {}
    for row in quotes:
        shown[(row[0], "bid")] = float(row[3])
        shown[(row[0], "ask")] = float(row[5])
    taken = {}
    for rec in res["trades"] + res["open_positions"]:
        buy_entry = rec["side"] == "long"
        for f in rec["entry_fills"]:
            key = (f["recv_ms"], "ask" if buy_entry else "bid")
            taken[key] = taken.get(key, Decimal(0)) + d(f["qty"])
        for f in rec["exit_fills"]:
            key = (f["recv_ms"], "bid" if buy_entry else "ask")
            taken[key] = taken.get(key, Decimal(0)) + d(f["qty"])
    for key, total in taken.items():
        assert total <= d(shown[key]), key


def test_a_partial_first_exit_window_keeps_its_fill_and_closes_the_rest_from_a_later_quote():
    quotes = [q(T + 1_000, 99_990, 0, 100_000, 1), q(T + 61_000, 100_100, 0.004, 100_110, 0),
              q(T + 81_000, 100_200, 1, 100_210, 0)]
    res = replay(quotes, [decision(T)])
    assert res["refusals"] == [] and res["open_positions"] == []
    (t,) = res["trades"]
    assert t["qty"] == 0.012
    assert t["exit_fills"] == [{"recv_ms": T + 61_000, "px": 100_100.0, "qty": 0.004},
                               {"recv_ms": T + 81_000, "px": 100_200.0, "qty": 0.008}]
    vwap = (100_100 * 0.004 + 100_200 * 0.008) / 0.012
    assert t["exit_px"] == pytest.approx(vwap, rel=1e-12)
    assert t["exit_ms"] == T + 81_000 and t["exit_windows"] == 2 and t["exit_delayed"] is True
    assert t["fees_usd"] == pytest.approx(FEE * (100_000 * 0.012 + 100_100 * 0.004 + 100_200 * 0.008), rel=1e-12)
    assert t["gross_usd"] == pytest.approx(100_100 * 0.004 + 100_200 * 0.008 - 100_000 * 0.012, rel=1e-9)
    assert_quantity_conserved(t)


@pytest.mark.parametrize("side", ["long", "short"])
def test_exits_cross_more_than_two_windows_and_an_empty_window_does_not_stop_the_search(side):
    sizes = [(T + 61_000, 0.003), (T + 65_000, 0.002), (T + 75_000, 0.0), (T + 90_000, 0.004), (T + 105_000, 0.01)]
    if side == "long":
        quotes = [q(T + 1_000, 99_990, 0, 100_000, 1)] + [q(ms, 100_100 + i, sz, 100_120 + i, 0)
                                                          for i, (ms, sz) in enumerate(sizes)]
    else:
        quotes = [q(T + 1_000, 100_000, 1, 100_010, 0)] + [q(ms, 99_880 - i, 0, 99_900 - i, sz)
                                                           for i, (ms, sz) in enumerate(sizes)]
    res = replay(quotes, [decision(T, 1 if side == "long" else -1)])
    (t,) = res["trades"]
    assert t["side"] == side and t["qty"] == 0.012
    assert [(f["recv_ms"], f["qty"]) for f in t["exit_fills"]] == [
        (T + 61_000, 0.003), (T + 65_000, 0.002), (T + 90_000, 0.004), (T + 105_000, 0.003)]
    assert t["exit_windows"] == 4 and t["exit_ms"] == T + 105_000
    notional = sum(f["px"] * f["qty"] for f in t["exit_fills"])
    sign = 1 if side == "long" else -1
    assert t["gross_usd"] == pytest.approx(sign * (notional - 100_000 * 0.012), rel=1e-9)
    assert t["fees_usd"] == pytest.approx(FEE * (100_000 * 0.012 + notional), rel=1e-12)
    assert_quantity_conserved(t)
    assert_quote_capacity(res, quotes)


def test_the_fill_timing_gate_checks_every_exit_fill_of_a_closed_trade():
    sizes = [(T + 61_000, 0.003), (T + 65_000, 0.002), (T + 75_000, 0.0), (T + 90_000, 0.004), (T + 105_000, 0.01)]
    quotes = [q(T + 1_000, 99_990, 0, 100_000, 1)] + [q(ms, 100_100 + i, sz, 100_120 + i, 0)
                                                      for i, (ms, sz) in enumerate(sizes)]
    res = replay(quotes, [decision(T)])
    (t,) = res["trades"]
    lat = er.COST_CASES["x1"]["latency_ms"]
    assert t["exit_windows"] == 4 and all(f["recv_ms"] >= t["exit_decision_ms"] + lat for f in t["exit_fills"])
    assert study.event_result([("BTC", res)], "x1", 1200.0)["fill_timing_violations"] == 0
    early = json.loads(json.dumps(res))
    early["trades"][0]["exit_fills"][0]["recv_ms"] = t["exit_decision_ms"] + lat - 1
    assert early["trades"][0]["exit_ms"] == T + 105_000
    assert study.event_result([("BTC", early)], "x1", 1200.0)["fill_timing_violations"] == 1


def test_quote_side_capacity_is_used_once_across_positions_in_one_coin_and_cost_case():
    quotes = [q(T + 1_000, 99_990, 0, 100_000, 1), q(T + 61_000, 100_100, 0.004, 100_110, 0),
              q(T + 61_500, 100_100, 0.004, 100_110, 0), q(T + 70_000, 100_150, 0.01, 100_160, 0.02),
              q(T + 121_500, 100_140, 0.02, 100_150, 0.02), q(T + 181_000, 100_200, 0.02, 100_210, 0.02)]
    res = replay(quotes, [decision(T), decision(T + 120_000)])
    assert len(res["trades"]) == 2 and res["open_positions"] == []
    assert_quote_capacity(res, quotes)
    for t in res["trades"]:
        assert_quantity_conserved(t)


def _open_tape(first_exit_size):
    return [q(T + 1_000, 99_990, 0, 100_000, 1), q(T + 61_000, 100_100, first_exit_size, 100_110, 0),
            q(T + 66_000, 100_050, 0, 100_060, 0), q(T + 121_000, 100_000, 0, 100_010, 1)]


@pytest.mark.parametrize("first_exit_size", [0.004, 0.0])
def test_exposure_still_open_at_the_end_of_the_tape_is_reported_and_blocks_a_later_entry(first_exit_size):
    quotes = _open_tape(first_exit_size)
    funding = [(T + 30_000, 0.0001), (T + 65_000, 0.0002), (T + 200_000, 0.0003)]
    res = replay(quotes, [decision(T), decision(T + 120_000)], funding)
    assert res["trades"] == [] and res["refusals"] == []
    (p,) = res["open_positions"]
    closed = first_exit_size
    assert p["status"] == "open_at_recording_end" and p["coin"] == "BTC" and p["side"] == "long"
    assert p["decision_ms"] == T and p["entry_ms"] == T + 1_000 and p["entry_px"] == 100_000.0
    assert p["qty"] == 0.012 and p["closed_qty"] == closed and d(p["remaining_qty"]) == d(0.012) - d(closed)
    assert_quantity_conserved(p)
    assert p["entry_fees_usd"] == pytest.approx(FEE * 100_000 * 0.012, rel=1e-12)
    assert p["exit_fees_usd"] == pytest.approx(FEE * 100_100 * closed, rel=1e-12)
    assert p["realized_gross_usd"] == pytest.approx((100_100 - 100_000) * closed, rel=1e-9)
    funding_due = -(0.012 * 99_995 * 0.0001 + (0.012 - closed) * 100_105 * 0.0002)
    assert p["funding_usd"] == pytest.approx(funding_due, rel=1e-12)
    assert p["funding_through_ms"] == T + 121_000
    assert p["realized_net_usd"] == pytest.approx(
        p["realized_gross_usd"] - p["entry_fees_usd"] - p["exit_fees_usd"] + p["funding_usd"], rel=1e-12)
    if closed:
        assert p["exit_fills"] == [{"recv_ms": T + 61_000, "px": 100_100.0, "qty": 0.004}]
        assert p["exit_px"] == 100_100.0
    else:
        assert p["exit_fills"] == [] and p["exit_px"] is None and p["exit_ms"] is None


@pytest.mark.parametrize("side", ["long", "short"])
def test_funding_charges_the_quantity_open_at_each_settlement_with_settlement_before_an_equal_time_fill(side):
    if side == "long":
        quotes = [q(T + 1_000, 99_990, 0, 100_000, 1), q(T + 61_000, 100_100, 0.004, 100_110, 0),
                  q(T + 81_000, 100_200, 1, 100_210, 0), q(T + 95_000, 100_300, 0, 100_310, 0)]
        mids = {"entry": 99_995.0, "first": 100_105.0, "final": 100_205.0}
        exit_notional = 100_100 * 0.004 + 100_200 * 0.008
    else:
        quotes = [q(T + 1_000, 100_000, 1, 100_010, 0), q(T + 61_000, 99_890, 0, 99_900, 0.004),
                  q(T + 81_000, 99_790, 0, 99_800, 1), q(T + 95_000, 99_690, 0, 99_700, 0)]
        mids = {"entry": 100_005.0, "first": 99_895.0, "final": 99_795.0}
        exit_notional = 99_900 * 0.004 + 99_800 * 0.008
    funding = [(T + 1_000, 0.001), (T + 30_000, 0.0001), (T + 61_000, 0.0002), (T + 70_000, 0.0003),
               (T + 81_000, 0.0004), (T + 90_000, 0.0005)]
    sign = 1 if side == "long" else -1
    res = replay(quotes, [decision(T, sign)], funding)
    (t,) = res["trades"]
    assert [f["qty"] for f in t["exit_fills"]] == [0.004, 0.008]
    due = -sign * (0.012 * mids["entry"] * 0.0001 + 0.012 * mids["first"] * 0.0002
                   + 0.008 * mids["first"] * 0.0003 + 0.008 * mids["final"] * 0.0004)
    assert t["funding_usd"] == pytest.approx(due, rel=1e-12)
    assert t["fees_usd"] == pytest.approx(FEE * (100_000 * 0.012 + exit_notional), rel=1e-12)
    assert t["net_usd"] == pytest.approx(t["gross_usd"] - t["fees_usd"] + t["funding_usd"], rel=1e-12)
    assert_quantity_conserved(t)


def _rising_tape(base, minutes, step_bps=3.0):
    rows = []
    for k in range(minutes * 12 + 1):
        mid = base * (1 + step_bps * k / 10_000)
        rows.append(q(T + 1_000 + k * 5_000, round(mid * 0.99995, 4), 10, round(mid * 1.00005, 4), 10))
    return rows


def _event_window(case, with_open):
    per_coin = []
    for coin, base, decimals in (("BTC", 100_000.0, 5), ("ETH", 4_000.0, 4)):
        tape = _rising_tape(base, 40)
        decisions = [decision(T + k * 120_000) for k in range(20)]
        if with_open:
            decisions.append(decision(T + 40 * 60_000 - 30_000))
        per_coin.append((coin, replay(tape, decisions, case=case, coin=coin, decimals=decimals)))
    return study.event_result(per_coin, case, 1200.0)


def _verdict(x1, x2):
    return study.verdict(x1, x2, {"net_pnl_usd": 5.0, "mean_ddadj": 1.0}, {"mean_reversion_pro": {"mean_ddadj": 0.0}},
                         {"funding_skew": {"mean_ddadj": 0.0}}, {"BTC": {"share": 1.0}, "ETH": {"share": 1.0}},
                         {"economic_evidence": True, "refusals": []}, True)


def test_study_aggregation_report_and_verdict_keep_an_open_position_and_refuse_a_closed_subset_pass():
    closed = {c: _event_window(c, False) for c in ("x1", "x2")}
    assert closed["x1"]["summary"]["positions"] >= study.MIN_TEST_POSITIONS
    assert closed["x1"]["summary"]["complete"] is True
    assert _verdict(closed["x1"], closed["x2"])["outcome"] == "pass"

    events = {c: _event_window(c, True) for c in ("x1", "x2")}
    payload = json.loads(json.dumps({w: {c: study.event_payload(v) for c, v in events.items()}
                                     for w in (study.SELECTION_WINDOW, study.HELD_OUT_WINDOW)}, sort_keys=True))
    for c in ("x1", "x2"):
        ev = payload[study.HELD_OUT_WINDOW][c]
        s = ev["summary"]
        assert s["positions"] == closed[c]["summary"]["positions"]
        assert s["net_pnl_usd"] == closed[c]["summary"]["net_pnl_usd"]
        assert s["complete"] is False and s["statistics_scope"] == "closed_positions_only"
        assert "still open" in s["incomplete_reason"]
        assert s["open_positions"] == 2 and set(s["open_remaining_qty"]) == {"BTC", "ETH"}
        assert len(ev["open_positions"]) == 2 and ev["fill_timing_violations"] == 0
        for p in ev["open_positions"]:
            assert p["exit_fills"] == [] and p["remaining_qty"] == p["qty"] and p["entry_fees_usd"] > 0
            assert_quantity_conserved(p)
        assert s["open_fees_usd"] == pytest.approx(sum(p["fees_usd"] for p in ev["open_positions"]), abs=1e-6)

    held = payload[study.HELD_OUT_WINDOW]
    v = _verdict(held["x1"], held["x2"])
    assert v["outcome"] == "inconclusive"
    assert v["checks"]["event_positions_resolved"] is False
    assert "still open at the end of the recording" in v["reason"]

    lines = "\n".join(study.event_completeness_lines(payload))
    assert "| test | x1 | no | 2 |" in lines and "| test | x2 | no | 2 |" in lines
    for p in held["x1"]["open_positions"]:
        assert f"| test | x1 | {p['coin']} | long |" in lines
