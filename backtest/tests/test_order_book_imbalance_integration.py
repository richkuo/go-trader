import json
import os
import subprocess
import sys

import numpy as np
import pandas as pd
import pytest

_HERE = os.path.dirname(os.path.abspath(__file__))
_BACKTEST = os.path.abspath(os.path.join(_HERE, ".."))
_ROOT = os.path.abspath(os.path.join(_BACKTEST, ".."))
for _p in (_BACKTEST, os.path.join(_ROOT, "shared_tools"), os.path.join(_ROOT, "shared_strategies", "open")):
    if _p not in sys.path:
        sys.path.insert(0, _p)

import book_archive as ba  # noqa: E402
import book_event_simulator as sim  # noqa: E402
import lz4_frame  # noqa: E402
import order_book_imbalance as obi  # noqa: E402
from book_inputs import ARCHIVE_TIME_SEMANTICS, CONTRACT  # noqa: E402
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


def test_registration_metadata_grid_and_mirrored_validator():
    reg = load_registry("futures")
    registry = reg._registry
    entry = registry.STRATEGIES[NAME]
    assert entry["platforms"] == ("futures",) and entry["short_entries"] is True
    assert entry["production_refused"] is True and entry["edge_status"] == "no_edge"
    assert entry["edge_ref"] == "backtest/candidates/order_book_imbalance_1635/REPORT.md"
    assert NAME in registry.DISCOVERY_HIDDEN_STRATEGIES and NAME not in reg.list_strategies()
    assert NAME not in load_registry("spot").STRATEGY_REGISTRY
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


def test_threshold_equality_filters_and_depth_are_exact():
    p = dict(P, entry_threshold=0.5)
    feat = {"B": 3, "A": 1, "total": obi.Fraction(10 ** 6), "imbalance": obi.Fraction(1, 2), "spread_bps": obi.Fraction(1), "best_bid": 1, "best_ask": 1}
    assert obi.classify(feat, p) == (0, "neutral")
    feat["imbalance"] = obi.Fraction(1, 2) + obi.Fraction(1, 10 ** 30)
    assert obi.classify(feat, p) == (1, "long")
    feat["imbalance"] = -obi.Fraction(1, 2) - obi.Fraction(1, 10 ** 30)
    assert obi.classify(feat, p) == (-1, "short")
    feat["spread_bps"] = obi.Fraction(5)
    assert obi.classify(feat, P)[0] == -1
    feat["spread_bps"] = obi.Fraction(5) + obi.Fraction(1, 10 ** 20)
    assert obi.classify(feat, P) == (0, "spread_filter")
    feat["spread_bps"], feat["total"] = obi.Fraction(1), obi.Fraction(100000)
    assert obi.classify(feat, P)[0] == -1
    feat["total"] = obi.Fraction(100000) - obi.Fraction(1, 10 ** 20)
    assert obi.classify(feat, P) == (0, "depth_filter")
    assert obi.classify(None, P) == (0, "insufficient_depth")


def test_episode_fires_once_at_its_sole_deadline_and_later_intervals_are_consumed():
    books, marks = long_run()
    crossing = books[3]
    deadline = obi.ceil_grid(max(crossing["event_ms"], crossing["recv_ms"]))
    before = decide(books[:3], marks, deadline - 1000)
    assert before["signal"] == 0 and before["reason"] == "confirming"
    first = decide(books, marks, deadline)
    again = decide(books, marks, deadline)
    assert first["signal"] == 1 and first["reason"] == "entry_long" and first["entry_id"] == again["entry_id"] != ""
    assert first["used"] == [b["identity"] for b in books[1:4]]
    later = decide(books, marks, deadline + 1000)
    assert later["signal"] == 0 and later["reason"] == "consumed" and later["episode_id"] == first["episode_id"]
    after_next = decide(books, marks, obi.ceil_grid(books[4]["recv_ms"]) + 1000)
    assert after_next["reason"] == "consumed"
    short_books = [b if i < 1 else obs(b["seq"], b["recv_ms"], 1, 3) for i, b in enumerate(books)]
    short = decide(short_books, marks, deadline)
    assert short["signal"] == -1 and short["entry_id"] != first["entry_id"]


def test_age_truncation_marker_and_flip_boundaries():
    books, marks = long_run()
    crossing = books[3]
    deadline = obi.ceil_grid(max(crossing["event_ms"], crossing["recv_ms"]))
    stale = dict(P, max_snapshot_age_ms=250)
    assert decide(books, marks, deadline, stale)["reason"] == "stale_crossing"
    neutral_boundary = decide(books, [], deadline)
    assert neutral_boundary["signal"] == 1 and neutral_boundary["boundary"] == f"book:{books[0]['identity']}"
    truncated = decide(books[1:], [], deadline)
    assert truncated["reason"] == "evidence_truncated" and truncated["signal"] == 0
    reset = marks + [marker(99, books[2]["recv_ms"] + 100, "silence")]
    assert decide(books, reset, deadline)["reason"] == "confirming"
    base = books[1]["recv_ms"]
    flipped = [books[0], obs(3, base, 1, 3), obs(4, base + 5_400, 1, 3)] + [obs(5 + i, base + (2 + i) * 5_400, 3, 1) for i in range(3)]
    cross = flipped[5]
    d = obi.ceil_grid(max(cross["event_ms"], cross["recv_ms"]))
    out = decide(flipped, marks, d)
    assert out["signal"] == 1 and out["boundary"] == f"book:{flipped[2]['identity']}"
    early = decide(flipped[:3], marks, obi.ceil_grid(flipped[2]["recv_ms"]))
    assert early["signal"] == 0 and early["reason"] == "confirming" and early["direction"] == -1
    thin = [b if i < 3 else obs(b["seq"], b["recv_ms"], 3, 1, count=4) for i, b in enumerate(books)]
    assert decide(thin, marks, deadline)["signal"] == 0

    cut_base = T0 + 200_000
    cut_marks = [marker(1, cut_base - 50_000)]
    run = [obs(2 + i, cut_base - 49_000 + i * 2_000, 3, 1) for i in range(6)]
    surviving = run[3:]
    cut_deadline = obi.ceil_grid(max(surviving[2]["event_ms"], surviving[2]["recv_ms"]))
    cut = section(surviving, cut_marks, cut_deadline)
    cut["prefix_complete"], cut["retained_from_ms"] = False, run[2]["recv_ms"] + 1
    held = obi.evaluate_section(cut, "BTC", P, cut_deadline)
    assert held["signal"] == 0 and held["reason"] == "evidence_truncated" and held["entry_id"] == ""
    unguarded = dict(cut, prefix_complete=True, retained_from_ms=cut_deadline - 60_000)
    assert obi.evaluate_section(unguarded, "BTC", P, cut_deadline)["signal"] == 1
    full_deadline = obi.ceil_grid(max(run[2]["event_ms"], run[2]["recv_ms"]))
    full = decide(run, cut_marks, full_deadline)
    assert full["signal"] == 1 and full["reason"] == "entry_long" and full["boundary"].startswith("marker:1:subscribed:")
    assert decide(run, cut_marks, full_deadline + 1000)["reason"] == "consumed"
    assert decide(run, cut_marks, cut_deadline)["reason"] == "consumed"
    unmarked = section(surviving, [], cut_deadline)
    unmarked["prefix_complete"], unmarked["retained_from_ms"] = False, cut_deadline - 30_000
    unmarked_out = obi.evaluate_section(unmarked, "BTC", P, cut_deadline)
    assert unmarked_out["signal"] == 0 and unmarked_out["reason"] == "evidence_truncated"
    late = dict(cut, prefix_complete=True)
    assert obi.evaluate_section(late, "BTC", P, cut_deadline)["reason"] == "evidence_truncated"


def test_missing_and_malformed_books_hold_and_candle_only_consumers_are_refused():
    reg = load_registry("futures")
    df = pd.DataFrame({"open": [1.0, 1.0], "high": [1.0, 1.0], "low": [1.0, 1.0], "close": [1.0, 1.0], "volume": [1.0, 1.0]})
    with pytest.raises(ValueError, match="candle-only run cannot score it"):
        reg.apply_strategy(NAME, df, {})
    out = reg.apply_strategy(NAME, df, {"l2_book_observations": {"available": False, "coin": "BTC", "kind": "l2_book", "reason": "gap"}})
    assert list(out["signal"]) == [0, 0] and out["obi_reason"].iloc[-1] == "book_unavailable"
    bad = section(*long_run(), T0 + 30_000)
    bad["observations"][0]["bids"][0][0] = "1e5"
    assert obi.evaluate_section(bad, "BTC", P)["reason"] == "book_malformed"
    books, marks = long_run()
    crossing = books[3]
    d = obi.ceil_grid(max(crossing["event_ms"], crossing["recv_ms"]))
    good = reg.apply_strategy(NAME, df, {**P, "l2_book_observations": section(books, marks, d)})
    assert list(good["signal"]) == [0, 1] and good["obi_reason"].iloc[0] == "not_decision_row"
    code = ("import json,sys; sys.path[:0]=sys.argv[1:4]; import order_book_imbalance as o; "
            "print(json.dumps(o.evaluate_section(json.loads(sys.stdin.read()), 'BTC', json.loads(sys.argv[4]))))")
    fresh = subprocess.run([sys.executable, "-c", code, _BACKTEST, os.path.join(_ROOT, "shared_tools"),
                            os.path.join(_ROOT, "shared_strategies", "open"), json.dumps(P)],
                           input=json.dumps(section(books, marks, d)), capture_output=True, text=True, check=True)
    assert json.loads(fresh.stdout)["entry_id"] == decide(books, marks, d)["entry_id"]


def _random_books(seed=1635, n=900, coin="BTC"):
    rng = np.random.default_rng(seed)
    out, marks = [], [marker(1, T0)]
    t = T0 + 2_000
    seq = 2
    for i in range(n):
        t += int(rng.integers(4_900, 5_700))
        if rng.random() < 0.01:
            marks.append({"seq": seq, "at_ms": t - 2_000, "reason": "silence", "session": 1, "detail": ""})
            seq += 1
        b = rng.integers(1, 6)
        a = rng.integers(1, 6)
        out.append(obs(seq, t, f"{b}.{rng.integers(0, 10)}", f"{a}.{rng.integers(0, 10)}", event=t - int(rng.integers(200, 900)), coin=coin))
        seq += 1
    return out, marks


def test_study_path_matches_the_pure_evaluator_on_every_deadline():
    books, marks = _random_books()
    cb = sim.CoinBooks("BTC", [books], marks, 1, 5)
    for p in (P, dict(P, confirmation_observations=2, entry_threshold=0.2), dict(P, confirmation_observations=5, depth_levels=1)):
        vp = obi.validate_params(p)
        q = cb.classify(vp["depth_levels"], vp["entry_threshold"], vp["max_spread_bps"], vp["min_combined_depth_usd"])
        fast = {}
        for e in sim.candidate_entries(cb, q, vp["confirmation_observations"], vp["max_snapshot_age_ms"], 60_000):
            _, eid = obi.episode_ids("BTC", vp, e["boundary"], e["direction"], cb.identity[e["first"]], cb.identity[e["crossing"]], e["deadline_ms"])
            fast[e["deadline_ms"]] = (e["direction"], eid)
        assert fast, p
        first, last = obi.ceil_grid(books[0]["recv_ms"]), obi.ceil_grid(books[-1]["recv_ms"])
        for d in range(first, last, 1000):
            sec = sim.section_at(cb, d, vp["depth_levels"], 60_000, ARCHIVE_TIME_SEMANTICS,
                                 {k: CONTRACT[k] for k in ("source", "contract", "identity_semantics", "max_future_skew_ms", "max_lag_ms", "max_gap_ms")})
            got = obi.evaluate_section(sec, "BTC", vp, d, accept_time_semantics=(ARCHIVE_TIME_SEMANTICS,))
            assert (got["signal"], got["entry_id"]) == fast.get(d, (0, "")), (p, d, got["reason"])


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


def test_archive_importer_refuses_contract_breaks_and_infers_barriers(tmp_path):
    def iso(ms):
        import datetime as dt
        return dt.datetime.fromtimestamp(ms / 1000, dt.UTC).strftime("%Y-%m-%dT%H:%M:%S") + ".%03d000123" % (ms % 1000)

    def line(recv, ev, tag="1", coin="BTC", ver=1, bad=False):
        bids, asks = levels(100000, 100001, f"1.{tag}", "1.0")
        data = {"coin": coin, "time": ev, "levels": [[{"px": b[0], "sz": b[1], "n": 1} for b in bids], [{"px": a[0], "sz": a[1], "n": 1} for a in asks]]}
        if bad:
            data["levels"][0][0]["px"] = "1e5"
        return json.dumps({"time": iso(recv), "ver_num": ver, "raw": {"channel": "l2Book", "data": data}})
    h0 = T0
    rows = [line(h0 + 1000, h0 + 700), line(h0 + 6000, h0 + 5700), line(h0 + 7000, h0 + 5700), line(h0 + 8000, h0 + 5700, tag="2"),
            line(h0 + 30000, h0 + 29700), line(h0 + 35000, h0 + 34700, bad=True), line(h0 + 40000, h0 + 39700, coin="ETH"),
            line(h0 + 45000, h0 + 44700, ver=2), "not json", line(h0 + 50000, h0 + 49700)]
    imp = ba.ArchiveCoinImporter("BTC", h0)
    imp.hour(h0, ("\n".join(rows) + "\n").encode())
    imp.missing_hour(h0 + ba.HOUR_MS)
    imp.hour(h0 + 2 * ba.HOUR_MS, (line(h0 + 2 * ba.HOUR_MS + 500, h0 + 2 * ba.HOUR_MS + 200) + "\n").encode())
    imp.finish(h0 + 3 * ba.HOUR_MS)
    assert imp.counts["accepted"] == 5 and imp.counts["duplicate"] == 1 and imp.counts["conflict"] == 1
    assert imp.counts["rejected"] == 2 and imp.counts["envelope_rejected"] == 2 and imp.counts["missing_hours"] == 1
    reasons = [(m["reason"], m["at_ms"] - h0) for m in imp.owner._hist("BTC")["markers"]]
    assert reasons[:3] == [("epoch_start", 0), ("subscribed", 0), ("conflict", 8000)]
    assert ("silence", 16001) in reasons and ("disconnected", ba.HOUR_MS) in reasons and ("subscribed", 2 * ba.HOUR_MS) in reasons
    fixture = os.path.join(_HERE, "testdata", "obi_archive_hour.lz4")
    with open(fixture, "rb") as fh:
        blob = lz4_frame.decompress(fh.read())
    assert blob.count(b"\n") == 30 and b'"channel":"l2Book"' in blob
    corrupt = bytearray(open(fixture, "rb").read())
    corrupt[-5] ^= 1
    with pytest.raises(lz4_frame.LZ4FrameError):
        lz4_frame.decompress(bytes(corrupt))
    manifest = {"files": [{"key": ba.book_key("20260801", 0, "BTC"), "status": "present", "size": 1, "sha256": "0" * 64}]}
    os.makedirs(tmp_path / "raw" / "market_data" / "20260801" / "0" / "l2Book")
    with open(tmp_path / "raw" / ba.book_key("20260801", 0, "BTC"), "wb") as fh:
        fh.write(open(fixture, "rb").read())
    with pytest.raises(ba.ArchiveError, match="does not match the manifest"):
        ba.import_window(str(tmp_path / "raw"), str(tmp_path / "out"), ["BTC"], ["20260801"], manifest)
    manifest["files"][0].update(size=os.path.getsize(fixture), sha256=ba.sha256_file(fixture))
    out = ba.import_window(str(tmp_path / "raw"), str(tmp_path / "out"), ["BTC"], ["20260801"], manifest)
    assert out["coins"]["BTC"]["counts"]["accepted"] == 30 and out["coins"]["BTC"]["counts"]["missing_hours"] == 23
    day = ba.read_day(str(tmp_path / "out" / "BTC_20260801.json.gz"))
    tail = ba.read_day(str(tmp_path / "out" / "BTC_tail.json.gz"))
    assert len(day["observations"]) + len(tail["observations"]) == 30


def test_every_candle_research_entrypoint_refuses_the_candidate():
    import run_backtest
    with pytest.raises(SystemExit, match="supplied l2Book depth observations"):
        run_backtest.run_single_backtest(NAME, "BTC", "1m", 30, platform="hyperliquid", registry="futures")
    from optimizer import walk_forward_optimize
    with pytest.raises(ValueError, match="supplied l2Book depth observations"):
        walk_forward_optimize(pd.DataFrame(), NAME, [{}], n_splits=2, timeframe="1m", registry="futures", platform="hyperliquid", verbose=False)
    import eval_windows
    with pytest.raises(ValueError, match="supplied l2Book depth observations"):
        eval_windows.run_leg(load_registry("futures"), NAME, {}, "BTC", "1m", ("2026-08-01", "2026-08-02"))
    import ledger_compare
    market = {"coin": "BTC", "interval": "1m", "registry": {NAME}, "funding_input_strategies": set(),
              "observation_input_strategies": set(), "trade_flow_input_strategies": set(), "macro_event_input_strategies": set(),
              "leader_input_strategies": set(), "volume_profile_input_strategies": set(), "premium_input_strategies": set(),
              "book_input_strategies": {NAME}}
    seg = {"strategy": {"type": "perps", "platform": "hyperliquid", "args": [NAME, "BTC", "1m"], "open_strategy": {"name": NAME}}}
    stop = {"applicable": False, "status": "not_applicable", "refusals": [], "owner": None, "needs_labels": False, "kwargs": None,
            "labels": None, "required_inputs": [], "input_version": 2, "basis": None, "resolved_live_units": None,
            "capability_context": None, "close_refs": None}
    rows = ledger_compare.capability_matrix(seg, {}, market, stop)
    opened = [r for r in rows if r["field"] == "open_strategy"]
    assert opened and opened[0]["decision"] == "refused" and "l2Book" in opened[0]["reason"]
    import exit_policy_ab
    with pytest.raises(ValueError, match="supplied l2Book depth observations"):
        exit_policy_ab._prepare_signals(load_registry("futures"), NAME, {}, pd.DataFrame())
    from closed_bar import unsupported_open_strategy_reason
    assert unsupported_open_strategy_reason(NAME).startswith("unsupported_cadence")
    from book_inputs import refuse_book_input_strategy
    refuse_book_input_strategy("donchian_breakout", "a test")
