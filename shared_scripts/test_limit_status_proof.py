import importlib.util
import json
import os
import sys
import types

import pytest


def _load_script(tag):
    script_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "check_hyperliquid.py")
    spec = importlib.util.spec_from_file_location("check_hyperliquid_limit_" + tag, script_path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


class _RateLimited(Exception):
    def __init__(self):
        super().__init__(429, "rate limited")
        self.status_code = 429


def _order(oid=9001, coin="ETH", status="canceled", orig="0.5", sz="0.5", placed=1_000, status_ms=2_000):
    body = {"coin": coin, "oid": oid, "timestamp": placed}
    if orig is not None:
        body["origSz"] = orig
    if sz is not None:
        body["sz"] = sz
    return {"status": "order", "order": {"order": body, "status": status, "statusTimestamp": status_ms}}


def _fill(oid=9001, sz="0.5", px="2000", fee="0.25", t=1_500, tid=None, coin="ETH"):
    return {"coin": coin, "oid": oid, "sz": sz, "px": px, "fee": fee, "time": t, "tid": tid if tid is not None else t}


class _Venue:
    def __init__(self, orders=None, open_oids=(), pages=None, lot=4):
        self.orders = orders or {}
        self.open_oids = open_oids
        self.pages = pages if pages is not None else {}
        self.lot = lot
        self.fill_starts = []

    def query_order_by_oid(self, oid):
        resp = self.orders.get(oid, {"status": "unknownOid"})
        if isinstance(resp, Exception):
            raise resp
        return resp

    def open_order_oids(self, symbol=None):
        if isinstance(self.open_oids, Exception):
            raise self.open_oids
        return set(self.open_oids)

    def user_fills_since(self, since_ms):
        self.fill_starts.append(since_ms)
        page = self.pages.get(since_ms, [])
        if isinstance(page, Exception):
            raise page
        return page

    def lot_size_decimals(self, symbol):
        return self.lot


def _status(mod, venue, oids=(9001,), since=500):
    results, open_error = mod.limit_status_orders(venue, "ETH", list(oids), since)
    return {r["oid"]: r for r in results}, open_error


def test_http_429_on_the_fill_read_is_retryable_and_unproven():
    mod = _load_script("429")
    venue = _Venue(orders={9001: _order(status="filled", sz="0.0")}, pages={500: _RateLimited()})
    got, _ = _status(mod, venue)
    r = got[9001]
    assert r["fills_proven"] is False and r["fills_reason"] == "retryable" and "429" in r["fills_error"]
    assert r["resting"] is False and r["order_state"] == "filled" and r["executed_size"] == 0.5
    assert r["filled_size"] == 0.0


def test_a_filled_order_with_a_complete_empty_scan_stays_retryable():
    mod = _load_script("filled_empty")
    venue = _Venue(orders={9001: _order(status="filled", sz="0.0")}, pages={500: []})
    r = _status(mod, venue)[0][9001]
    assert r["fills_proven"] is False and r["fills_reason"] == "retryable"
    assert "fill index lag" in r["fills_error"]


def test_a_cancelled_order_with_explicit_zero_execution_proves_zero():
    mod = _load_script("cancel_zero")
    venue = _Venue(orders={9001: _order(status="canceled", sz="0.5")}, pages={500: []})
    r = _status(mod, venue)[0][9001]
    assert venue.fill_starts == [500]
    assert r["fills_proven"] is True and r["executed_size"] == 0.0 and r["resting"] is False and r["status_error"] == ""


def test_a_proven_live_zero_execution_order_reads_no_fill_history():
    mod = _load_script("live_zero")
    venue = _Venue(orders={9001: _order(status="open", sz="0.5")}, open_oids={9001})
    r = _status(mod, venue)[0][9001]
    assert venue.fill_starts == []
    assert r["fills_proven"] is True and r["resting"] is True and r["order_state"] == "live"


def test_a_partial_fill_while_live_is_proven_from_its_complete_history():
    mod = _load_script("partial_live")
    venue = _Venue(orders={9001: _order(status="open", sz="0.6", orig="1.0")}, open_oids={9001},
                   pages={500: [_fill(sz="0.1", px="2000", fee="0.05", t=1_100), _fill(sz="0.3", px="2010", fee="-0.01", t=1_200),
                                _fill(oid=7, sz="9", t=1_300)]})
    r = _status(mod, venue)[0][9001]
    assert r["fills_proven"] is True and r["resting"] is True
    assert r["filled_size"] == pytest.approx(0.4) and r["executed_size"] == pytest.approx(0.4)
    assert r["avg_px"] == pytest.approx((0.1 * 2000 + 0.3 * 2010) / 0.4)
    assert r["fee"] == pytest.approx(0.04) and r["count"] == 2


def test_the_scan_starts_at_the_verified_placement_time_and_is_shared():
    mod = _load_script("shared_scan")
    venue = _Venue(orders={9001: _order(status="filled", sz="0", placed=300), 9002: _order(oid=9002, status="canceled", orig="0.5", sz="0.5", placed=400)},
                   pages={300: [_fill(t=350)]})
    got, _ = _status(mod, venue, oids=(9001, 9002), since=500)
    assert venue.fill_starts == [300]
    assert got[9001]["fills_proven"] is True and got[9002]["fills_proven"] is True


@pytest.mark.parametrize("name, order, open_oids, needle", [
    ("missing status", {"status": "order", "order": {"order": {"coin": "ETH", "oid": 9001, "origSz": "0.5", "sz": "0.5"}}}, set(), "no order state"),
    ("unfamiliar status", _order(status="mysteryCanceled"), set(), "unfamiliar order state"),
    ("unknown order id", {"status": "unknownOid"}, set(), "does not know this order id"),
    ("wrong order id", _order(oid=1234), set(), "not the requested 9001"),
    ("wrong coin", _order(coin="BTC"), set(), "not the requested 'ETH'"),
    ("missing original size", _order(orig=None), set(), "no original size"),
    ("malformed remaining size", _order(sz="abc"), set(), "remaining size"),
    ("remaining above original", _order(sz="0.7"), set(), "exceeds the original size"),
    ("filled with a remainder", _order(status="filled", sz="0.1"), set(), "remaining size of 0.1"),
    ("terminal but still listed", _order(status="canceled"), {9001}, "still carries it"),
    ("live but missing from the list", _order(status="open"), set(), "omits it"),
])
def test_unusable_or_conflicting_status_evidence_cannot_authorize(name, order, open_oids, needle):
    mod = _load_script("bad_" + name.replace(" ", "_"))
    venue = _Venue(orders={9001: order}, open_oids=open_oids, pages={500: []})
    r = _status(mod, venue)[0][9001]
    assert r["status_error"] and needle in r["status_error"], (name, r)
    assert r["fills_proven"] is False and r["fills_reason"] == "retryable"
    assert venue.fill_starts == []
    if name != "live but missing from the list":
        assert r["resting"] is None, (name, r)


def test_an_unreadable_open_order_list_leaves_a_terminal_order_unverified():
    mod = _load_script("list_unreadable")
    venue = _Venue(orders={9001: _order(status="canceled")}, open_oids=ValueError("openOrders returned no list: {'error': 'html'}"), pages={500: []})
    got, open_error = _status(mod, venue)
    assert "no list" in open_error
    assert got[9001]["resting"] is None and got[9001]["fills_proven"] is True


@pytest.mark.parametrize("name, fills, needle", [
    ("one-lot shortfall", [_fill(sz="0.4999")], "of the 0.5"),
    ("one-lot excess", [_fill(sz="0.4"), _fill(sz="0.1001", t=1_600)], "more than the 0.5"),
    ("missing fee", [dict(_fill(), fee=None)], "no fee"),
    ("non-positive price", [_fill(px="0")], "non-positive"),
    ("fill on another coin", [_fill(coin="BTC")], "names coin"),
])
def test_incomplete_or_excessive_fill_totals_cannot_authorize(name, fills, needle):
    mod = _load_script("fills_" + name.replace(" ", "_").replace("-", "_"))
    venue = _Venue(orders={9001: _order(status="filled", sz="0")}, pages={500: fills})
    r = _status(mod, venue)[0][9001]
    assert r["fills_proven"] is False and r["fills_reason"] == "retryable" and needle in r["fills_error"], (name, r)


def test_conflicting_duplicate_fill_rows_cannot_authorize():
    mod = _load_script("dup_conflict")
    page1 = [_fill(sz="0.25", t=1_000 + i, tid=100 + i) for i in range(mod.HL_FILLS_MIN_PAGE)]
    last = page1[-1]["time"]
    page2 = [dict(page1[-1], sz="0.3"), _fill(sz="0.25", t=last + 1, tid=99_999)]
    venue = _Venue(orders={9001: _order(status="filled", sz="0", orig="125.25")}, pages={500: page1, last: page2})
    r = _status(mod, venue)[0][9001]
    assert r["fills_proven"] is False and "conflicting duplicate" in r["fills_error"]


def test_an_incomplete_page_scan_is_retryable():
    mod = _load_script("stuck_page")
    page = [_fill(sz="0.001", t=1_000, tid=i) for i in range(mod.HL_FILLS_MIN_PAGE)]
    venue = _Venue(orders={9001: _order(status="filled", sz="0")}, pages={500: page, 1_000: page})
    r = _status(mod, venue)[0][9001]
    assert r["fills_proven"] is False and r["fills_reason"] == "retryable" and "incomplete" in r["fills_error"]


def test_unknown_lot_metadata_holds_the_proof():
    mod = _load_script("no_lot")
    venue = _Venue(orders={9001: _order(status="filled", sz="0")}, pages={500: [_fill()]}, lot=None)
    r = _status(mod, venue)[0][9001]
    assert r["fills_proven"] is False and "lot metadata" in r["fills_error"]
    assert venue.fill_starts == []


def test_a_retained_history_that_starts_after_the_order_ended_is_permanent(monkeypatch):
    mod = _load_script("retention")
    monkeypatch.setattr(mod, "HL_FILLS_RECENT_LIMIT", 4)
    monkeypatch.setattr(mod, "HL_FILLS_MIN_PAGE", 2)
    later = [_fill(oid=7, sz="1", t=5_000 + i, tid=i) for i in range(4)]
    venue = _Venue(orders={9001: _order(status="filled", sz="0", status_ms=2_000)}, pages={500: later[:2], 5_001: later[1:]})
    r = _status(mod, venue)[0][9001]
    assert r["fills_proven"] is False and r["fills_reason"] == "permanent"


def test_a_scan_limit_alone_is_not_permanent(monkeypatch):
    mod = _load_script("retention_open")
    monkeypatch.setattr(mod, "HL_FILLS_RECENT_LIMIT", 4)
    monkeypatch.setattr(mod, "HL_FILLS_MIN_PAGE", 2)
    early = [_fill(oid=7, sz="1", t=1_100 + i, tid=i) for i in range(4)]
    venue = _Venue(orders={9001: _order(status="filled", sz="0", status_ms=2_000)}, pages={500: early[:2], 1_101: early[1:]})
    r = _status(mod, venue)[0][9001]
    assert r["fills_proven"] is False and r["fills_reason"] == "retryable"


def test_run_limit_status_keeps_its_arguments_and_emits_the_proof_fields(monkeypatch, capsys):
    mod = _load_script("run")
    venue = _Venue(orders={9001: _order(status="filled", sz="0")}, pages={500: [_fill(fee="0.25")]})
    adapter_mod = types.ModuleType("adapter")
    adapter_mod.HyperliquidExchangeAdapter = lambda: venue
    monkeypatch.setitem(sys.modules, "platforms.hyperliquid.adapter", adapter_mod)
    mod.run_limit_status("ETH", [9001], "live", since_ms=500)
    out = json.loads(capsys.readouterr().out)
    r = out["orders"][0]
    for key in ("oid", "coin", "resting", "order_status", "order_state", "orig_size", "executed_size", "status_error",
                "sz_decimals", "filled_size", "avg_px", "fee", "count", "fills_proven", "fills_error", "fills_reason"):
        assert key in r, key
    assert r["fills_proven"] is True and r["filled_size"] == 0.5 and r["order_status"] == "filled" and r["sz_decimals"] == 4


def test_quote_status_keeps_its_classification():
    mod = _load_script("quote_parity")
    for raw, want in [("open", "resting"), ("resting", "resting"), ("triggered", "triggered"), ("filled", "filled"),
                      ("canceled", "cancelled"), ("reduceOnlyCanceled", "cancelled"), ("badAloPxRejected", "rejected"),
                      ("brandNewState", "unknown")]:
        out = mod.quote_status_outcome(_order(status=raw, orig="1", sz="0.4"), 7)
        assert out["status"] == want, (raw, out)
        if want in ("cancelled", "rejected"):
            assert out["detail"] == raw
