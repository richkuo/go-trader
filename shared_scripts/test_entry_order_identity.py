import importlib.util
import json
import os
import sys
import types

import pytest

CLOID = "0x0000000000000000000000000000abcd"
STOP_CLOID = "0x0000000000000000000000000000abce"


def _load_script(filename, tag):
    script_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), filename)
    spec = importlib.util.spec_from_file_location(filename.replace(".py", "") + "_" + tag, script_path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def _filled(oid=4242, px="100.5", sz="0.3"):
    return {"status": "ok", "response": {"type": "order", "data": {"statuses": [
        {"filled": {"avgPx": px, "totalSz": sz, "oid": oid}}]}}}


class _HLVenue:
    def __init__(self, order_response=None, order_error=None, expiry_error=None, stop_response=None):
        self.calls = []
        self.order_response = order_response if order_response is not None else _filled()
        self.order_error = order_error
        self.expiry_error = expiry_error
        self.stop_response = stop_response or {"status": "ok", "response": {"data": {"statuses": [{"resting": {"oid": 5151}}]}}}
        self.order_status = {}
        self.fills = []

    def adapter_module(self):
        venue = self
        mod = types.ModuleType("adapter")

        class HyperliquidExchangeAdapter:
            def set_order_expiry(self, ms):
                venue.calls.append(("expiry", ms))
                if venue.expiry_error and ms is not None:
                    raise venue.expiry_error

            def market_open(self, symbol, is_buy, size, cloid=None):
                venue.calls.append(("market_open", cloid))
                if venue.order_error:
                    raise venue.order_error
                return venue.order_response

            def lookup_fill_fee_by_oid(self, oid, since_ms):
                return {}

            def round_perps_trigger_px(self, symbol, px):
                return round(px, 4)

            def place_stop_loss(self, symbol, sz, trigger_px, is_buy, cloid=None):
                venue.calls.append(("stop", cloid))
                return venue.stop_response

            def query_order_by_cloid(self, cloid):
                return venue.order_status[cloid]

            def user_fills_since(self, since_ms):
                return venue.fills

        mod.HyperliquidExchangeAdapter = HyperliquidExchangeAdapter
        return mod


def _run_hl_execute(monkeypatch, capsys, venue, **kwargs):
    mod = _load_script("check_hyperliquid.py", "entry_identity")
    monkeypatch.setitem(sys.modules, "adapter", venue.adapter_module())
    code = 0
    try:
        mod.run_execute("BTC", "buy", 0.3, "live", **kwargs)
    except SystemExit as e:
        code = e.code or 0
    return json.loads(capsys.readouterr().out), code


def test_hl_guarded_entry_sends_saved_ids_under_the_signed_deadline(monkeypatch, capsys):
    venue = _HLVenue()
    out, code = _run_hl_execute(monkeypatch, capsys, venue, stop_loss_pct=2.0, cloid=CLOID, stop_cloid=STOP_CLOID,
                                expires_after_ms=1_000, stop_expires_after_ms=2_000)
    assert code == 0
    assert out["order_outcome"] == "filled"
    assert out["client_order_id"] == CLOID and out["stop_client_order_id"] == STOP_CLOID
    assert venue.calls[:4] == [("expiry", 1_000), ("market_open", CLOID), ("expiry", 2_000), ("stop", STOP_CLOID)]
    assert out["execution"]["fill"]["stop_loss_oid"] == 5151


def test_hl_guarded_entry_without_a_deadline_is_not_sent(monkeypatch, capsys, subtests):
    with subtests.test(case="no_deadline"):
        venue = _HLVenue()
        out, code = _run_hl_execute(monkeypatch, capsys, venue, cloid=CLOID)
        assert code == 1 and out["order_outcome"] == "not_sent"
        assert ("market_open", CLOID) not in venue.calls
    for extra in ({"close_mode": "reduce_only"}, {"prev_pos_qty": 0.2}, {"close_full_position": True}):
        with subtests.test(case="refuses_a_close_or_flip", extra=extra):
            venue = _HLVenue()
            out, code = _run_hl_execute(monkeypatch, capsys, venue, cloid=CLOID, expires_after_ms=1_000, **extra)
            assert code == 1 and out["order_outcome"] == "not_sent", extra
            assert not any(c[0] == "market_open" for c in venue.calls), extra
    with subtests.test(case="deadline_failure_sends_nothing"):
        venue = _HLVenue(expiry_error=RuntimeError("exchange init failed"))
        out, code = _run_hl_execute(monkeypatch, capsys, venue, cloid=CLOID, expires_after_ms=1_000)
        assert code == 1 and out["order_outcome"] == "not_sent"
        assert not any(c[0] == "market_open" for c in venue.calls)


def test_hl_guarded_entry_exception_after_send_is_unknown(monkeypatch, capsys):
    venue = _HLVenue(order_error=TimeoutError("read timed out"))
    out, code = _run_hl_execute(monkeypatch, capsys, venue, cloid=CLOID, expires_after_ms=1_000)
    assert code == 1
    assert out["order_outcome"] == "unknown"
    assert out["client_order_id"] == CLOID


def test_hl_guarded_entry_rejection_is_rejected(monkeypatch, capsys):
    rejected = {"status": "ok", "response": {"data": {"statuses": [{"error": "Insufficient margin"}]}}}
    venue = _HLVenue(order_response=rejected)
    out, code = _run_hl_execute(monkeypatch, capsys, venue, cloid=CLOID, expires_after_ms=1_000)
    assert code == 1 and out["order_outcome"] == "rejected" and out["client_order_id"] == CLOID


def _order_status(status, oid=9001, orig="0.4", sz="0", trigger=None):
    order = {"oid": oid, "origSz": orig, "sz": sz, "cloid": CLOID}
    if trigger is not None:
        order["triggerPx"] = trigger
    return {"status": "order", "order": {"order": order, "status": status}}


def test_hl_recovery_evidence_matches_fills_by_order_id():
    mod = _load_script("check_hyperliquid.py", "entry_recovery")
    venue = _HLVenue()
    venue.order_status[CLOID] = _order_status("filled")
    venue.fills = [
        {"oid": 9001, "sz": "0.25", "px": "100", "fee": "0.01", "tid": 1},
        {"oid": 9001, "sz": "0.15", "px": "101", "fee": "0.005", "tid": 2},
        {"oid": 77, "sz": "5", "px": "1", "fee": "0", "tid": 3},
    ]
    ev = mod.entry_recovery_evidence(venue.adapter_module().HyperliquidExchangeAdapter(), CLOID, 0)
    assert ev["found"] and ev["terminal"] and ev["fills_complete"]
    assert ev["fill_qty"] == pytest.approx(0.4)
    assert ev["fill_avg_px"] == pytest.approx((0.25 * 100 + 0.15 * 101) / 0.4)
    assert ev["fill_fee"] == pytest.approx(0.015)
    assert ev["fill_tids"] == ["1", "2"]


def test_hl_recovery_evidence_flags_partial_history_and_open_orders():
    mod = _load_script("check_hyperliquid.py", "entry_recovery_partial")
    venue = _HLVenue()
    adapter = venue.adapter_module().HyperliquidExchangeAdapter()
    venue.order_status[CLOID] = _order_status("canceled", sz="0.1")
    venue.fills = [{"oid": 9001, "sz": "0.2", "px": "100", "fee": "0.01"}]
    ev = mod.entry_recovery_evidence(adapter, CLOID, 0)
    assert ev["executed_sz"] == pytest.approx(0.3) and not ev["fills_complete"]
    venue.order_status[CLOID] = _order_status("open", sz="0.4")
    venue.fills = []
    ev = mod.entry_recovery_evidence(adapter, CLOID, 0)
    assert not ev["terminal"] and ev["fill_qty"] == 0


def test_hl_recovery_evidence_reports_unknown_order_with_cloid_fills():
    mod = _load_script("check_hyperliquid.py", "entry_recovery_unknown")
    venue = _HLVenue()
    adapter = venue.adapter_module().HyperliquidExchangeAdapter()
    venue.order_status[CLOID] = {"status": "unknownOid"}
    venue.fills = [{"oid": 1, "sz": "0.1", "px": "100", "cloid": CLOID}]
    ev = mod.entry_recovery_evidence(adapter, CLOID, 0)
    assert not ev["found"] and ev["fill_qty"] == pytest.approx(0.1) and not ev["fills_complete"]
    venue.order_status[CLOID] = {"status": "weird"}
    with pytest.raises(ValueError):
        mod.entry_recovery_evidence(adapter, CLOID, 0)


class _HLHistoryVenue(_HLVenue):
    def __init__(self, pages=None, orders_by_oid=None):
        super().__init__()
        self.pages = pages or {}
        self.orders_by_oid = orders_by_oid or {}
        self.fill_starts = []
        self.oid_lookups = []

    def adapter_module(self):
        mod = super().adapter_module()
        venue = self
        base = mod.HyperliquidExchangeAdapter

        class HyperliquidExchangeAdapter(base):
            account_address = "0x0000000000000000000000000000000000C0FFEE"

            def user_fills_since(self, since_ms):
                venue.fill_starts.append(since_ms)
                return venue.pages.get(since_ms, [])

            def query_order_by_oid(self, oid):
                venue.oid_lookups.append(oid)
                return venue.orders_by_oid.get(oid, {"status": "unknownOid"})

        mod.HyperliquidExchangeAdapter = HyperliquidExchangeAdapter
        return mod


def _hl_fill(t, cloid=None, oid=1, sz="0.1", px="100"):
    f = {"time": t, "tid": t, "oid": oid, "sz": sz, "px": px, "fee": "0.01", "coin": "BTC"}
    if cloid is not None:
        f["cloid"] = cloid
    return f


def test_hl_recovery_scan_pages_and_attributes_fills_without_a_client_id():
    mod = _load_script("check_hyperliquid.py", "entry_recovery_pages")
    other = "0x00000000000000000000000000000fff"
    first = [_hl_fill(t, cloid=other, oid=t) for t in range(1, 2001)]
    second = [_hl_fill(2000, cloid=other, oid=2000), _hl_fill(2001, oid=77, sz="0.3", px="101")]
    venue = _HLHistoryVenue(pages={1: first, 2000: second}, orders_by_oid={77: _order_status("filled", oid=77)})
    venue.order_status[CLOID] = {"status": "unknownOid"}
    adapter = venue.adapter_module().HyperliquidExchangeAdapter()
    ev = mod.entry_recovery_evidence(adapter, CLOID, 1)
    cov = ev["coverage"]
    assert venue.fill_starts == [1, 2000] and venue.oid_lookups == [77]
    assert cov["complete"] and cov["pages"] == 2 and cov["rows"] == 2001 and cov["through_ms"] > 0
    assert not ev["found"] and ev["fill_qty"] == pytest.approx(0.3) and cov["matched_count"] == 1


def test_hl_recovery_scan_reports_incomplete_history():
    mod = _load_script("check_hyperliquid.py", "entry_recovery_incomplete")
    stuck = [_hl_fill(5, cloid="0x01") for _ in range(2000)]
    for i, f in enumerate(stuck):
        f["tid"] = i
    untimed = [{"tid": i, "oid": i, "sz": "1", "px": "1", "cloid": "0x01"} for i in range(2000)]
    many = [_hl_fill(t, oid=t) for t in range(1, 60)]
    cases = [
        (_HLHistoryVenue(pages={5: stuck}), 5, "did not advance in time"),
        (_HLHistoryVenue(pages={1: untimed}), 1, "carries a fill with no time"),
        (_HLHistoryVenue(pages={1: [_hl_fill(3, oid=9)]}), 1, "no longer retained"),
        (_HLHistoryVenue(pages={1: many}, orders_by_oid={t: _order_status("filled", oid=t) for t in range(1, 60)}), 1, "need a client id lookup"),
    ]
    for venue, since, reason in cases:
        venue.order_status[CLOID] = {"status": "unknownOid"}
        ev = mod.entry_recovery_evidence(venue.adapter_module().HyperliquidExchangeAdapter(), CLOID, since)
        cov = ev["coverage"]
        assert not ev["found"] and not cov["complete"] and reason in cov["reason"] and cov["through_ms"] == 0, (reason, cov)


def test_hl_run_recover_entry_reports_the_account_and_both_client_ids(monkeypatch, capsys):
    mod = _load_script("check_hyperliquid.py", "entry_recovery_run")
    venue = _HLHistoryVenue(pages={1: [_hl_fill(3, cloid="0x01")]})
    venue.order_status[CLOID] = {"status": "unknownOid"}
    venue.order_status[STOP_CLOID] = {"status": "unknownOid"}
    monkeypatch.setitem(sys.modules, "adapter", venue.adapter_module())
    monkeypatch.setenv("HYPERLIQUID_TESTNET", "")
    mod.run_recover_entry("BTC", CLOID, STOP_CLOID, 1, "live")
    out = json.loads(capsys.readouterr().out)
    assert out["account"] == "hyperliquid:mainnet:0x0000000000000000000000000000000000c0ffee"
    for key in ("order", "stop"):
        ev = out[key]
        assert ev["found"] is False and ev["fill_qty"] == 0 and ev["coverage"]["complete"] and ev["coverage"]["rows"] == 1
    assert venue.fill_starts == [1]


def test_hl_recovery_fills_after_the_release_deadline_need_no_client_id_lookup():
    mod = _load_script("check_hyperliquid.py", "entry_recovery_bound")
    until = 100
    later = [_hl_fill(until + i, oid=1000 + i) for i in range(1, 42)]
    venue = _HLHistoryVenue(pages={1: later})
    venue.order_status[CLOID] = {"status": "unknownOid"}
    ev = mod.entry_recovery_evidence(venue.adapter_module().HyperliquidExchangeAdapter(), CLOID, 1, until_ms=until)
    cov = ev["coverage"]
    assert venue.oid_lookups == [] and not ev["found"] and ev["fill_qty"] == 0
    assert cov["complete"] and cov["through_ms"] == until and cov["matched_count"] == 0 and cov["rows"] == 41


def test_hl_recovery_counts_fills_after_the_deadline_against_the_venue_retention():
    mod = _load_script("check_hyperliquid.py", "entry_recovery_retention")
    until = 100
    pages = {}
    start = 1
    t = until + 1
    for _ in range(6):
        page = [_hl_fill(t + i, cloid="0x01", oid=t + i) for i in range(2000)]
        pages[start] = page
        start = page[-1]["time"]
        t = start
    venue = _HLHistoryVenue(pages=pages)
    venue.order_status[CLOID] = {"status": "unknownOid"}
    ev = mod.entry_recovery_evidence(venue.adapter_module().HyperliquidExchangeAdapter(), CLOID, 1, until_ms=until)
    cov = ev["coverage"]
    assert not cov["complete"] and "fills the venue retains" in cov["reason"] and cov["through_ms"] == 0


def test_hl_recovery_matches_entry_fills_just_before_the_release_deadline():
    mod = _load_script("check_hyperliquid.py", "entry_recovery_edge")
    until = 100
    cases = [
        (_HLHistoryVenue(pages={1: [_hl_fill(until - 1, cloid=CLOID)]}), True),
        (_HLHistoryVenue(pages={1: [_hl_fill(until - 1, oid=88)]}, orders_by_oid={88: _order_status("filled", oid=88)}), True),
        (_HLHistoryVenue(pages={1: [_hl_fill(until + 1, cloid=CLOID)]}), True),
        (_HLHistoryVenue(pages={1: [_hl_fill(until - 1, oid=88)]}), False),
    ]
    for venue, complete in cases:
        venue.order_status[CLOID] = {"status": "unknownOid"}
        ev = mod.entry_recovery_evidence(venue.adapter_module().HyperliquidExchangeAdapter(), CLOID, 1, until_ms=until)
        cov = ev["coverage"]
        assert not ev["found"] and cov["complete"] == complete, cov
        if complete:
            assert cov["matched_count"] == 1 and ev["fill_qty"] == pytest.approx(0.1), cov
        else:
            assert "no longer retained" in cov["reason"] and cov["through_ms"] == 0, cov


def test_hl_recovery_keeps_paging_a_page_shorter_than_the_documented_maximum():
    mod = _load_script("check_hyperliquid.py", "entry_recovery_short_page")
    first = [_hl_fill(t, cloid="0x01", oid=t) for t in range(1, 501)]
    second = [_hl_fill(500, cloid="0x01", oid=500), _hl_fill(501, cloid=CLOID, oid=501)]
    venue = _HLHistoryVenue(pages={1: first, 500: second})
    venue.order_status[CLOID] = {"status": "unknownOid"}
    ev = mod.entry_recovery_evidence(venue.adapter_module().HyperliquidExchangeAdapter(), CLOID, 1, until_ms=1000)
    assert venue.fill_starts == [1, 500] and ev["coverage"]["complete"] and ev["coverage"]["matched_count"] == 1


def test_hl_recovery_books_a_found_stop_that_filled_after_the_release_deadline():
    mod = _load_script("check_hyperliquid.py", "entry_recovery_late_stop")
    until = 100
    venue = _HLHistoryVenue(pages={1: [_hl_fill(until + 5000, oid=9001, sz="0.3")]})
    venue.order_status[STOP_CLOID] = _order_status("filled", orig="0.3")
    ev = mod.entry_recovery_evidence(venue.adapter_module().HyperliquidExchangeAdapter(), STOP_CLOID, 1, until_ms=until)
    assert ev["found"] and ev["fills_complete"] and ev["fill_qty"] == pytest.approx(0.3)
