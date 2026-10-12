import importlib.util
import json
import os
import sys
import types

import ccxt
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


def test_hl_guarded_entry_without_a_deadline_is_not_sent(monkeypatch, capsys):
    venue = _HLVenue()
    out, code = _run_hl_execute(monkeypatch, capsys, venue, cloid=CLOID)
    assert code == 1 and out["order_outcome"] == "not_sent"
    assert ("market_open", CLOID) not in venue.calls


def test_hl_guarded_entry_refuses_a_close_or_flip(monkeypatch, capsys):
    for extra in ({"close_mode": "reduce_only"}, {"prev_pos_qty": 0.2}, {"close_full_position": True}):
        venue = _HLVenue()
        out, code = _run_hl_execute(monkeypatch, capsys, venue, cloid=CLOID, expires_after_ms=1_000, **extra)
        assert code == 1 and out["order_outcome"] == "not_sent", extra
        assert not any(c[0] == "market_open" for c in venue.calls), extra


def test_hl_guarded_entry_deadline_failure_sends_nothing(monkeypatch, capsys):
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


class _OKXVenue:
    def __init__(self, create_error=None, orders=None, trades=None, missing=False):
        self.calls = []
        self.exp_times = []
        self.create_error = create_error
        self.orders = list(orders or [])
        self.trades = trades or []
        self.missing = missing

    def adapter_module(self):
        venue = self
        mod = types.ModuleType("adapter")

        class OKXExchangeAdapter:
            def market_open(self, symbol, is_buy, size, inst_type="spot", client_order_id=None, reduce_only=False, exp_time_ms=None):
                venue.calls.append(("market_open", client_order_id, reduce_only))
                venue.exp_times.append(exp_time_ms)
                if venue.create_error:
                    raise venue.create_error
                return {"id": "okx-1", "clientOrderId": client_order_id}

            def fetch_order_by_client_id(self, symbol, inst_type, client_order_id):
                venue.calls.append(("fetch", client_order_id))
                if venue.missing:
                    raise ccxt.OrderNotFound("51603 Order does not exist")
                if len(venue.orders) > 1:
                    return venue.orders.pop(0)
                return venue.orders[0]

            def fetch_order_trades(self, symbol, inst_type, order_id):
                return venue.trades

        mod.OKXExchangeAdapter = OKXExchangeAdapter
        return mod


def _okx_deadline():
    import time
    return int(time.time() * 1000) + 120_000


def _run_okx(monkeypatch, capsys, venue, fn, *args, **kwargs):
    mod = _load_script("check_okx.py", "entry_identity")
    monkeypatch.setitem(sys.modules, "adapter", venue.adapter_module())
    monkeypatch.setattr(mod.time, "sleep", lambda s: None)
    code = 0
    try:
        getattr(mod, fn)(*args, **kwargs)
    except SystemExit as e:
        code = e.code or 0
    return json.loads(capsys.readouterr().out), code


def test_okx_guarded_entry_reads_the_fill_back_by_client_id(monkeypatch, capsys):
    venue = _OKXVenue(orders=[{"id": "okx-1", "status": "closed", "filled": 0.3, "average": 101.0, "fee": {"cost": 0.02}}])
    out, code = _run_okx(monkeypatch, capsys, venue, "run_execute", "BTC", "buy", 0.3, "live", "swap", client_order_id="gtabc", exp_time_ms=_okx_deadline())
    assert code == 0 and out["order_outcome"] == "filled" and out["client_order_id"] == "gtabc"
    assert out["execution"]["fill"] == {"avg_px": 101.0, "total_sz": 0.3, "fee": 0.02, "oid": "okx-1"}
    assert venue.calls[0] == ("market_open", "gtabc", False)


def test_okx_guarded_entry_outcomes(monkeypatch, capsys):
    cases = [
        (_OKXVenue(orders=[{"id": "o", "status": "canceled", "filled": 0, "average": None, "fee": {"cost": 0}}]), "rejected"),
        (_OKXVenue(create_error=ccxt.InsufficientFunds("51008")), "rejected"),
        (_OKXVenue(create_error=ccxt.RequestTimeout("timeout")), "unknown"),
        (_OKXVenue(orders=[{"id": "o", "status": "open", "filled": 0.1, "average": 100, "fee": {"cost": 0}}]), "unknown"),
        (_OKXVenue(orders=[{"id": "o", "status": "closed", "filled": 0.3, "average": 100}]), "unknown"),
    ]
    for venue, want in cases:
        out, code = _run_okx(monkeypatch, capsys, venue, "run_execute", "BTC", "buy", 0.3, "live", "spot", client_order_id="gtabc", exp_time_ms=_okx_deadline())
        assert code == 1 and out["order_outcome"] == want, (want, out)


def test_okx_reduce_only_close_and_id_validation(monkeypatch, capsys):
    venue = _OKXVenue()
    venue.orders = [{"id": "o", "status": "closed", "filled": 0.3, "average": 100}]
    _run_okx(monkeypatch, capsys, venue, "run_execute", "BTC", "sell", 0.3, "live", "swap", reduce_only=True)
    assert venue.calls[0] == ("market_open", None, True)
    for bad in ("GT-UPPER", "x" * 33):
        venue = _OKXVenue()
        out, code = _run_okx(monkeypatch, capsys, venue, "run_execute", "BTC", "buy", 0.3, "live", "swap", client_order_id=bad)
        assert code == 1 and out["order_outcome"] == "not_sent" and venue.calls == []


def test_okx_guarded_entry_sends_the_saved_deadline_and_refuses_without_one(monkeypatch, capsys):
    deadline = _okx_deadline()
    venue = _OKXVenue(orders=[{"id": "okx-1", "status": "closed", "filled": 0.3, "average": 101.0, "fee": {"cost": 0.02}}])
    out, code = _run_okx(monkeypatch, capsys, venue, "run_execute", "BTC", "buy", 0.3, "live", "swap", client_order_id="gtabc", exp_time_ms=deadline)
    assert code == 0 and venue.exp_times == [deadline]
    for exp in (0, deadline - 10 * 60_000):
        venue = _OKXVenue()
        out, code = _run_okx(monkeypatch, capsys, venue, "run_execute", "BTC", "buy", 0.3, "live", "swap", client_order_id="gtabc", exp_time_ms=exp)
        assert code == 1 and out["order_outcome"] == "not_sent" and venue.calls == [], (exp, out)
    venue = _OKXVenue()
    out, code = _run_okx(monkeypatch, capsys, venue, "run_execute", "BTC", "buy", 0.3, "live", "swap", exp_time_ms=deadline)
    assert code == 1 and out["order_outcome"] == "not_sent" and venue.calls == []


def test_okx_guarded_entry_reads_back_an_unclassified_create_error(monkeypatch, capsys):
    filled = {"id": "okx-1", "status": "closed", "filled": 0.3, "average": 101.0, "fee": {"cost": 0.02}}
    zero = {"id": "okx-1", "status": "canceled", "filled": 0, "average": None, "fee": {"cost": 0}}
    cases = [
        (_OKXVenue(create_error=ccxt.ExchangeError("okx 51999 unmapped"), orders=[filled]), "filled", 0, True),
        (_OKXVenue(create_error=ccxt.DuplicateOrderId("51016"), orders=[filled]), "filled", 0, True),
        (_OKXVenue(create_error=ccxt.ExchangeError("okx 51999 unmapped"), orders=[zero]), "rejected", 1, True),
        (_OKXVenue(create_error=ccxt.ExchangeError("okx 51999 unmapped"), missing=True), "unknown", 1, True),
        (_OKXVenue(create_error=ccxt.InsufficientFunds("51008")), "rejected", 1, False),
        (_OKXVenue(create_error=ccxt.InvalidOrder("51020")), "rejected", 1, False),
        (_OKXVenue(create_error=ccxt.NetworkError("reset")), "unknown", 1, False),
    ]
    for venue, want, want_code, reads_back in cases:
        out, code = _run_okx(monkeypatch, capsys, venue, "run_execute", "BTC", "buy", 0.3, "live", "swap", client_order_id="gtabc", exp_time_ms=_okx_deadline())
        assert code == want_code and out["order_outcome"] == want, (venue.create_error, out)
        assert any(c[0] == "fetch" for c in venue.calls) == reads_back, (venue.create_error, venue.calls)
        if want == "filled":
            assert out["execution"]["fill"] == {"avg_px": 101.0, "total_sz": 0.3, "fee": 0.02, "oid": "okx-1"}


def test_okx_recovery_reports_missing_and_matched_trades(monkeypatch, capsys):
    out, code = _run_okx(monkeypatch, capsys, _OKXVenue(missing=True), "run_recover_entry", "BTC", "swap", "gtabc", "live")
    assert code == 0 and out["order"] == {"found": False}
    venue = _OKXVenue(orders=[{"id": "okx-9", "status": "closed", "filled": 0.3, "average": 100.5, "fee": {"cost": 0.02}}],
                      trades=[{"amount": 0.1, "price": 100, "fee": {"cost": 0.01}}, {"amount": 0.2, "price": 100.75, "fee": {"cost": 0.01}}])
    out, code = _run_okx(monkeypatch, capsys, venue, "run_recover_entry", "BTC", "swap", "gtabc", "live")
    order = out["order"]
    assert code == 0 and order["found"] and order["terminal"] and order["fills_complete"]
    assert order["fill_qty"] == pytest.approx(0.3) and order["fill_fee"] == pytest.approx(0.02)
    assert order["fill_avg_px"] == pytest.approx((0.1 * 100 + 0.2 * 100.75) / 0.3)
