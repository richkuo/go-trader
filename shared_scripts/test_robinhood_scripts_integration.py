import hashlib
import json
import os
import pickle
import subprocess
import sys
import textwrap
import time
from datetime import datetime, timezone

import pytest

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
FETCH = os.path.join(ROOT, "shared_scripts", "fetch_robinhood_positions.py")
CLOSE = os.path.join(ROOT, "shared_scripts", "close_robinhood_position.py")

TOKEN_URL = "https://api.robinhood.com/oauth2/token/"
HOLDINGS = "https://nummus.robinhood.com/holdings/"
PAGE2 = HOLDINGS + "?cursor=2"
PAGE3 = HOLDINGS + "?cursor=3"
CURRENCY_PAIRS = "https://nummus.robinhood.com/currency_pairs/"
ORDERS = "https://nummus.robinhood.com/orders/"
ACCOUNTS = "https://nummus.robinhood.com/accounts/"
PAIR_ID = "pair-btc"
QUOTE = "https://api.robinhood.com/marketdata/forex/quotes/" + PAIR_ID + "/"
ORDER_ID = "ord-1"
ORDER = ORDERS + ORDER_ID + "/"
CANCEL = ORDER + "cancel/"
REF_ID = "11111111-1111-4111-8111-111111111111"
EXECUTE = os.path.join(ROOT, "shared_scripts", "check_robinhood.py")
OTHER_HOST = "https://holdings.example.com/holdings/?cursor=2"

LOGIN_OK = [{"status": 200, "json": {
    "access_token": "fake-access-token",
    "token_type": "Bearer",
    "refresh_token": "fake-refresh-token",
    "expires_in": 86400,
    "scope": "internal",
}}]
LOGIN_REFUSED = [{"status": 400, "json": {"detail": "Unable to log in with provided credentials."}}]

SITECUSTOMIZE = textwrap.dedent("""
    import json
    import os
    import socket

    def _blocked(*args, **kwargs):
        raise OSError("network blocked by robinhood integration harness")

    socket.socket.connect = _blocked
    socket.create_connection = _blocked
    socket.getaddrinfo = _blocked

    import requests
    from requests.adapters import HTTPAdapter
    from requests.models import Response
    from requests.structures import CaseInsensitiveDict

    with open(os.environ["RH_FAKE_FIXTURE"]) as _f:
        _ROUTES = json.load(_f)
    _LOG = os.environ["RH_FAKE_LOG"]
    _COUNTS = {}

    def _send(self, request, stream=False, timeout=None, verify=True, cert=None, proxies=None):
        key = request.method + " " + request.url
        items = _ROUTES.get(key)
        body = request.body
        if isinstance(body, bytes):
            body = body.decode("utf-8", "replace")
        if "/oauth2/token/" in request.url:
            body = None
        try:
            parsed = json.loads(body) if body else None
        except ValueError:
            parsed = body
        record = {
            "method": request.method,
            "url": request.url,
            "timeout": list(timeout) if isinstance(timeout, tuple) else timeout,
            "auth": bool(request.headers.get("Authorization")),
            "listed": items is not None,
            "body": parsed,
            "content_type": request.headers.get("Content-Type"),
        }
        with open(_LOG, "a") as f:
            f.write(json.dumps(record) + "\\n")
        if items is None:
            raise RuntimeError("unlisted request " + key)
        n = _COUNTS.get(key, 0)
        _COUNTS[key] = n + 1
        item = items[min(n, len(items) - 1)]
        if item.get("sleep_s"):
            import time
            time.sleep(float(item["sleep_s"]))
        if item.get("raise") == "connection":
            raise requests.exceptions.ConnectionError("fake connection failure")
        if item.get("raise") == "connect_timeout":
            raise requests.exceptions.ConnectTimeout("fake connect timeout")
        if item.get("raise") == "read_timeout":
            raise requests.exceptions.ReadTimeout("fake read timeout")
        if item.get("raise") == "new_connection":
            from urllib3.exceptions import NewConnectionError
            raise requests.exceptions.ConnectionError(NewConnectionError(None, "fake connection refused"))
        res = Response()
        res.status_code = item.get("status", 200)
        res.reason = "Fake"
        if "json" in item:
            res._content = json.dumps(item["json"]).encode()
        else:
            res._content = item.get("text", "").encode()
        res._content_consumed = True
        headers = {"Content-Type": "application/json"}
        headers.update(item.get("headers", {}))
        res.headers = CaseInsensitiveDict(headers)
        res.url = request.url
        res.request = request
        res.encoding = "utf-8"
        res.connection = self
        return res

    HTTPAdapter.send = _send
""")


def _page(results, next_url=None):
    return {"status": 200, "json": {"results": results, "next": next_url}}


def _held(code, quantity, **extra):
    row = {"currency": {"code": code}, "quantity": quantity}
    row.update(extra)
    return row


BTC = _held("BTC", "0.5", cost_bases=[{"direct_cost_basis": "20000"}])
BTC_SELLABLE = _held("BTC", "0.5", quantity_available="0.5", cost_bases=[{"direct_cost_basis": "20000"}])
ETH = _held("ETH", "1.25", cost_bases=[{"direct_cost_basis": "2500"}])


def _chain(pages):
    urls = [HOLDINGS] + [f"{HOLDINGS}?cursor={i}" for i in range(2, pages + 1)]
    routes = {}
    for i, url in enumerate(urls):
        routes["GET " + url] = [_page([], urls[i + 1] if i + 1 < len(urls) else HOLDINGS + "?cursor=end")]
    routes["GET " + HOLDINGS + "?cursor=end"] = [_page([])]
    return routes


def _single(rows):
    return {"GET " + HOLDINGS: [_page(rows)]}


def _two_pages(page2_item):
    return {"GET " + HOLDINGS: [_page([BTC], PAGE2)], "GET " + PAGE2: [page2_item]}


FAILED_READS = {
    "page1_http_500": {"GET " + HOLDINGS: [{"status": 500, "json": {"detail": "server error"}}]},
    "page1_http_401": {"GET " + HOLDINGS: [{"status": 401, "json": {"detail": "token expired"}}]},
    "page1_http_302": {"GET " + HOLDINGS: [{"status": 302, "text": "", "headers": {"Location": OTHER_HOST}}]},
    "page1_transport_error": {"GET " + HOLDINGS: [{"raise": "connection"}]},
    "page1_invalid_json": {"GET " + HOLDINGS: [{"status": 200, "text": "{not json"}]},
    "page1_json_list": {"GET " + HOLDINGS: [{"status": 200, "json": [BTC]}]},
    "page1_no_results": {"GET " + HOLDINGS: [{"status": 200, "json": {"next": None}}]},
    "page1_results_not_list": {"GET " + HOLDINGS: [{"status": 200, "json": {"results": {"0": BTC}, "next": None}}]},
    "page1_no_next_key": {"GET " + HOLDINGS: [{"status": 200, "json": {"results": [BTC]}}]},
    "page2_http_503": _two_pages({"status": 503, "json": {"detail": "unavailable"}}),
    "page2_transport_error": _two_pages({"raise": "connection"}),
    "page2_invalid_json": _two_pages({"status": 200, "text": "<html>"}),
    "page2_no_results": _two_pages({"status": 200, "json": {"next": None}}),
    "next_repeats_page1": {"GET " + HOLDINGS: [_page([BTC], HOLDINGS)]},
    "next_cycle": {
        "GET " + HOLDINGS: [_page([], PAGE2)],
        "GET " + PAGE2: [_page([], PAGE3)],
        "GET " + PAGE3: [_page([], PAGE2)],
    },
    "next_other_host": {"GET " + HOLDINGS: [_page([BTC], OTHER_HOST)]},
    "next_http_scheme": {"GET " + HOLDINGS: [_page([BTC], "http://nummus.robinhood.com/holdings/?cursor=2")]},
    "next_not_string": {"GET " + HOLDINGS: [_page([BTC], 2)]},
    "page_cap_exceeded": _chain(51),
    "row_not_object": _single(["BTC"]),
    "row_currency_missing": _single([{"quantity": "0.5"}]),
    "row_code_missing": _single([{"currency": {}, "quantity": "0.5"}]),
    "row_code_empty": _single([_held("", "0.5")]),
    "row_code_blank": _single([_held("   ", "0.5")]),
    "row_code_not_string": _single([_held(5, "0.5")]),
    "row_quantity_missing": _single([{"currency": {"code": "BTC"}}]),
    "row_quantity_null": _single([_held("BTC", None)]),
    "row_quantity_empty": _single([_held("BTC", "")]),
    "row_quantity_text": _single([_held("BTC", "abc")]),
    "row_quantity_nan_text": _single([_held("BTC", "NaN")]),
    "row_quantity_inf_text": _single([_held("BTC", "inf")]),
    "row_quantity_negative": _single([_held("BTC", "-1")]),
    "row_quantity_bool": _single([_held("BTC", True)]),
    "row_quantity_bare_nan": {"GET " + HOLDINGS: [{"status": 200, "text": (
        '{"results": [{"currency": {"code": "BTC"}, "quantity": NaN}], "next": null}'
    )}]},
    "row_duplicate_symbol": {"GET " + HOLDINGS: [_page([BTC], PAGE2)], "GET " + PAGE2: [_page([BTC])]},
}


def _run(tmp_path, script, args, routes, login=LOGIN_OK):
    run_dir = tmp_path / f"run-{len(list(tmp_path.iterdir()))}"
    home = run_dir / "home"
    site = run_dir / "site"
    home.mkdir(parents=True)
    site.mkdir()
    (site / "sitecustomize.py").write_text(SITECUSTOMIZE)
    fixture = run_dir / "routes.json"
    fixture.write_text(json.dumps(dict(routes, **{"POST " + TOKEN_URL: login})))
    log = run_dir / "requests.jsonl"
    log.write_text("")
    env = {k: v for k, v in os.environ.items() if not k.startswith("ROBINHOOD")}
    env.update({
        "HOME": str(home),
        "GO_TRADER_ROBINHOOD_SESSION_DIR": os.path.join(os.path.realpath(run_dir), "robinhood-session"),
        "PYTHONPATH": os.pathsep.join([str(site)] + [p for p in [os.environ.get("PYTHONPATH")] if p]),
        "PYTHONDONTWRITEBYTECODE": "1",
        "RH_FAKE_FIXTURE": str(fixture),
        "RH_FAKE_LOG": str(log),
        "ROBINHOOD_USERNAME": "integration-user",
        "ROBINHOOD_PASSWORD": "integration-password",
        "ROBINHOOD_TOTP_SECRET": "JBSWY3DPEHPK3PXP",
    })
    proc = subprocess.run(
        [sys.executable, script, *args],
        cwd=ROOT, env=env, capture_output=True, text=True, timeout=120,
    )
    lines = [ln for ln in proc.stdout.splitlines() if ln.strip()]
    assert lines, proc.stderr
    payload = json.loads(lines[-1])
    requests_made = [json.loads(ln) for ln in log.read_text().splitlines() if ln.strip()]
    assert [r["url"] for r in requests_made if not r["listed"]] == []
    return proc.returncode, payload, requests_made


def _count(requests_made, prefix, method="GET"):
    return sum(1 for r in requests_made if r["method"] == method and r["url"].startswith(prefix))


@pytest.mark.parametrize("shape", sorted(FAILED_READS))
def test_failed_holdings_read_fails_fetch_script(tmp_path, shape):
    code, payload, made = _run(tmp_path, FETCH, [], FAILED_READS[shape])
    assert code == 1
    assert isinstance(payload.get("error"), str) and payload["error"]
    assert payload["positions"] == []
    if shape == "page_cap_exceeded":
        assert _count(made, HOLDINGS) == 50, "test_page_cap_stops_after_fifty_holdings_requests"


@pytest.mark.parametrize("shape", sorted(FAILED_READS))
def test_failed_holdings_read_fails_close_script(tmp_path, shape):
    code, payload, made = _run(tmp_path, CLOSE, ["--symbol=BTC", "--mode=live", "--quantity=0.5"], FAILED_READS[shape])
    assert code == 1
    assert isinstance(payload.get("error"), str) and payload["error"]
    assert payload["close"].get("already_flat") is None
    assert _count(made, CURRENCY_PAIRS) == 0
    assert _count(made, ORDERS, "POST") == 0


def test_complete_two_page_read_returns_every_positive_holding(tmp_path):
    routes = {
        "GET " + HOLDINGS: [_page([ETH, {"quantity": "0"}], PAGE2)],
        "GET " + PAGE2: [_page([_held("btc", "0.5")])],
    }
    code, payload, made = _run(tmp_path, FETCH, [], routes)
    assert code == 0
    assert payload.get("error") is None
    assert payload["positions"] == [
        {"coin": "ETH", "size": 1.25, "avg_price": 2000.0},
        {"coin": "BTC", "size": 0.5, "avg_price": 0.0},
    ]
    holdings = [r for r in made if r["url"].startswith(HOLDINGS)]
    assert [r["url"] for r in holdings] == [HOLDINGS, PAGE2]
    assert all(r["timeout"] is not None and r["auth"] for r in holdings)


@pytest.mark.parametrize("extra", [
    {},
    {"cost_bases": None},
    {"cost_bases": []},
    {"cost_bases": ["x"]},
    {"cost_bases": [{}]},
    {"cost_bases": {"direct_cost_basis": "100"}},
    {"cost_bases": [{"direct_cost_basis": "abc"}]},
    {"cost_bases": [{"direct_cost_basis": "NaN"}]},
    {"cost_bases": [{"direct_cost_basis": None}]},
    {"cost_bases": [{"direct_cost_basis": "-5"}]},
], ids=lambda extra: json.dumps(extra))
def test_unusable_cost_basis_gives_zero_avg_price(tmp_path, extra):
    code, payload, _ = _run(tmp_path, FETCH, [], _single([_held("BTC", "0.5", **extra)]))
    assert code == 0
    assert payload.get("error") is None
    assert payload["positions"] == [{"coin": "BTC", "size": 0.5, "avg_price": 0.0}]


def test_close_reports_already_flat_only_after_complete_read(tmp_path):
    routes = {"GET " + HOLDINGS: [_page([ETH], PAGE2)], "GET " + PAGE2: [_page([])]}
    code, payload, made = _run(tmp_path, CLOSE, ["--symbol=BTC", "--mode=live", "--quantity=0.5"], routes)
    assert code == 0
    assert payload.get("error") is None
    assert payload["close"]["already_flat"] is True
    assert _count(made, HOLDINGS) == 2
    assert _count(made, CURRENCY_PAIRS) == 0
    assert _count(made, ORDERS, "POST") == 0


def test_close_sells_holding_found_on_later_page(tmp_path):
    routes = {
        "GET " + HOLDINGS: [_page([ETH], PAGE2)],
        "GET " + PAGE2: [_page([BTC_SELLABLE])],
        "GET " + CURRENCY_PAIRS: [{"raise": "connection"}],
    }
    code, payload, made = _run(tmp_path, CLOSE, ["--symbol=BTC", "--mode=live", "--quantity=0.5", "--ref-id=" + REF_ID], routes)
    assert code == 1
    assert isinstance(payload.get("error"), str) and payload["error"]
    assert payload["close"].get("already_flat") is None
    assert _count(made, CURRENCY_PAIRS) == 1
    assert _count(made, ORDERS, "POST") == 0


PREP = {
    "GET " + CURRENCY_PAIRS: [_page([
        {"id": PAIR_ID, "asset_currency": {"code": "BTC"}, "quote_currency": {"code": "USD"},
         "min_order_size": "0.000001", "min_order_quantity_increment": "0.00000001", "max_order_size": "20"},
        {"id": "pair-eth", "asset_currency": {"code": "ETH"}, "quote_currency": {"code": "USD"}},
    ])],
    "GET " + ACCOUNTS: [_page([{"id": "acct-1"}])],
    "GET " + QUOTE: [{"status": 200, "json": {"ask_price": "100.00", "bid_price": "99.00"}}],
}


def _order(state, cumulative="0", average=None, side="buy", order_id=ORDER_ID, **extra):
    body = {"id": order_id, "ref_id": REF_ID, "state": state, "side": side, "currency_pair_id": PAIR_ID,
            "cumulative_quantity": cumulative, "average_price": average}
    body.update(extra)
    return {"status": 200, "json": body}


def _accepted(side="buy", **extra):
    item = _order("unconfirmed", side=side, **extra)
    item["status"] = 201
    return item


def _routes(post, polls=None, cancel=None, **more):
    routes = dict(PREP)
    routes["POST " + ORDERS] = post
    if polls is not None:
        routes["GET " + ORDER] = polls
    if cancel is not None:
        routes["POST " + CANCEL] = cancel
    routes.update(more)
    return routes


def _buy(budget_ms=6000):
    return ["--execute", "--symbol=BTC", "--side=buy", "--amount_usd=10", "--mode=live",
            "--ref-id=" + REF_ID, f"--budget-ms={budget_ms}"]


def _sell(quantity="0.5", budget_ms=6000):
    return ["--execute", "--symbol=BTC", "--side=sell", f"--quantity={quantity}", "--mode=live",
            "--ref-id=" + REF_ID, f"--budget-ms={budget_ms}"]


def _posts(made):
    return [r for r in made if r["method"] == "POST" and r["url"] == ORDERS]


def _assert_finite_document(payload):
    json.dumps(payload, allow_nan=False)


def test_execute_buy_polls_until_filled_and_reports_the_venue_fill(tmp_path):
    routes = _routes(
        [_accepted()],
        [_order("queued"), _order("confirmed"), _order("filled", "0.1", "100.5", executions=[{"fees": [{"amount": "0.02"}]}])],
    )
    code, payload, made = _run(tmp_path, EXECUTE, _buy(), routes)
    assert code == 0, payload
    assert payload["order_outcome"] == "filled"
    assert payload["execution"]["fill"] == {"avg_px": 100.5, "quantity": 0.1, "oid": ORDER_ID, "fee": 0.02}
    assert payload["requested_quantity"] == 0.1
    assert "error" not in payload
    posts = _posts(made)
    assert len(posts) == 1
    assert posts[0]["body"] == {
        "account_id": "acct-1", "currency_pair_id": PAIR_ID, "price": 100.0, "quantity": 0.1,
        "ref_id": REF_ID, "side": "buy", "time_in_force": "gtc", "type": "market",
    }
    assert posts[0]["content_type"] == "application/json"
    assert [r for r in made if r["timeout"] is None and "/oauth2/" not in r["url"]] == []
    assert all(r["auth"] for r in made if "/oauth2/" not in r["url"])
    _assert_finite_document(payload)


def test_execute_sell_canceled_after_partial_fill_reports_only_the_filled_part(tmp_path):
    routes = _routes(
        [_accepted(side="sell")],
        [_order("confirmed", side="sell"), _order("canceled", "0.2", "99.5", side="sell")],
        **_holding("0.5", "0.5"),
    )
    code, payload, _ = _run(tmp_path, EXECUTE, _sell(), routes)
    assert code == 0, payload
    assert payload["order_outcome"] == "partially_filled"
    assert payload["execution"]["fill"]["quantity"] == 0.2
    assert payload["execution"]["fill"]["avg_px"] == 99.5


def test_execute_open_at_deadline_is_canceled_and_unconfirmed_cancel_stays_open(tmp_path):
    routes = _routes([_accepted()], [_order("confirmed")], [{"status": 200, "json": {}}])
    started = time.monotonic()
    code, payload, made = _run(tmp_path, EXECUTE, _buy(budget_ms=4000), routes)
    assert code == 1
    assert payload["order_outcome"] == "open"
    assert payload["cancel"]["requested"] is True
    assert payload["cancel"]["acknowledged"] is True
    assert "fill" not in payload["execution"]
    assert "unconfirmed" in payload["error"]
    assert _count(made, CANCEL, "POST") == 1
    assert time.monotonic() - started < 4 + 15


def test_execute_canceled_with_zero_fill_is_no_fill(tmp_path):
    routes = _routes([_accepted()], [_order("canceled", "0")])
    code, payload, _ = _run(tmp_path, EXECUTE, _buy(), routes)
    assert code == 1
    assert payload["order_outcome"] == "no_fill"
    assert "fill" not in payload["execution"]


@pytest.mark.parametrize("body", [
    {"detail": "insufficient buying power"},
    {"non_field_errors": ["order size too small"]},
], ids=["detail", "non_field_errors"])
def test_execute_refusal_body_is_rejected(tmp_path, body):
    code, payload, made = _run(tmp_path, EXECUTE, _buy(), _routes([{"status": 400, "json": body}]))
    assert code == 1
    assert payload["order_outcome"] == "rejected"
    assert "Robinhood refused the order" in payload["error"]
    assert len(_posts(made)) == 1


def test_execute_accepted_empty_body_is_unknown(tmp_path):
    code, payload, _ = _run(tmp_path, EXECUTE, _buy(), _routes([{"status": 201, "json": {}}]))
    assert code == 1
    assert payload["order_outcome"] == "unknown"


def test_execute_accepted_refused_state_with_zero_fill_is_rejected(tmp_path):
    item = _order("rejected", "0")
    item["status"] = 201
    code, payload, _ = _run(tmp_path, EXECUTE, _buy(), _routes([item]))
    assert code == 1
    assert payload["order_outcome"] == "rejected"


@pytest.mark.parametrize("post", [
    {"status": 503, "json": {"detail": "unavailable"}},
    {"raise": "read_timeout"},
    {"raise": "connection"},
    {"status": 200, "text": "<html>"},
], ids=["http_503", "read_timeout", "connection_reset", "not_json"])
def test_execute_failure_after_send_is_unknown(tmp_path, post):
    code, payload, made = _run(tmp_path, EXECUTE, _buy(), _routes([post]))
    assert code == 1
    assert payload["order_outcome"] == "unknown"
    assert len(_posts(made)) == 1


def test_execute_pre_send_failures_resend_with_the_same_ref_id_then_report_not_sent(tmp_path):
    routes = _routes([{"raise": "new_connection"}, {"raise": "connect_timeout"}, {"raise": "new_connection"}])
    code, payload, made = _run(tmp_path, EXECUTE, _buy(), routes)
    assert code == 1
    assert payload["order_outcome"] == "not_sent"
    posts = _posts(made)
    assert len(posts) == 3
    assert {p["body"]["ref_id"] for p in posts} == {REF_ID}


def test_execute_pre_send_failure_then_accept_uses_one_ref_id(tmp_path):
    routes = _routes([{"raise": "connect_timeout"}, _accepted()], [_order("filled", "0.1", "100")])
    code, payload, made = _run(tmp_path, EXECUTE, _buy(), routes)
    assert code == 0, payload
    posts = _posts(made)
    assert len(posts) == 2
    assert {p["body"]["ref_id"] for p in posts} == {REF_ID}


@pytest.mark.parametrize("prep", ["GET " + CURRENCY_PAIRS, "GET " + ACCOUNTS, "GET " + QUOTE])
def test_execute_prep_read_failure_sends_nothing(tmp_path, prep):
    routes = _routes([_accepted()])
    routes[prep] = [{"status": 500, "json": {"detail": "server error"}}]
    code, payload, made = _run(tmp_path, EXECUTE, _buy(), routes)
    assert code == 1
    assert payload["order_outcome"] == "not_sent"
    assert len(_posts(made)) == 0


@pytest.mark.parametrize("average", [None, "NaN", "-1", "abc"])
def test_execute_filled_without_usable_average_price_is_unknown(tmp_path, average):
    routes = _routes([_accepted()], [_order("filled", "0.1", average)])
    code, payload, _ = _run(tmp_path, EXECUTE, _buy(), routes)
    assert code == 1
    assert payload["order_outcome"] == "unknown"
    assert "fill" not in payload["execution"]
    _assert_finite_document(payload)


def test_execute_fill_above_the_requested_quantity_is_inconsistent(tmp_path):
    routes = _routes([_accepted()], [_order("filled", "0.2", "100")])
    code, payload, _ = _run(tmp_path, EXECUTE, _buy(), routes)
    assert code == 1
    assert payload["order_outcome"] == "inconsistent"


def test_execute_poll_returning_another_order_is_unknown(tmp_path):
    routes = _routes([_accepted()], [_order("filled", "0.1", "100", order_id="ord-other")])
    code, payload, _ = _run(tmp_path, EXECUTE, _buy(), routes)
    assert code == 1
    assert payload["order_outcome"] == "unknown"


def test_execute_hung_poll_still_writes_one_document_inside_the_budget(tmp_path):
    routes = _routes([_accepted()], [{"sleep_s": 30, "status": 200, "json": {}}])
    started = time.monotonic()
    code, payload, _ = _run(tmp_path, EXECUTE, _buy(budget_ms=4000), routes)
    elapsed = time.monotonic() - started
    assert code == 1
    assert payload["order_outcome"] in ("unknown", "open")
    assert elapsed < 4 + 15


def test_execute_hung_login_is_not_sent_inside_the_budget(tmp_path):
    started = time.monotonic()
    code, payload, made = _run(tmp_path, EXECUTE, _buy(budget_ms=3000), _routes([_accepted()]),
                               login=[dict(LOGIN_OK[0], sleep_s=30)])
    assert code == 1
    assert payload["order_outcome"] == "not_sent"
    assert len(_posts(made)) == 0
    assert time.monotonic() - started < 3 + 15


def _resolve(order_id=None, cancel_open=False, before_ms=None, after_ms=None, quantity="0.1", side="buy", extra=()):
    now_ms = int(time.time() * 1000)
    args = ["--resolve-order", "--symbol=BTC", f"--side={side}", "--ref-id=" + REF_ID, "--budget-ms=6000",
            f"--submitted-after-ms={after_ms if after_ms is not None else now_ms - 60000}",
            f"--submitted-before-ms={before_ms if before_ms is not None else now_ms - 30000}",
            "--mode=live", f"--requested-quantity={quantity}"]
    if order_id:
        args.append("--order-id=" + order_id)
    if cancel_open:
        args.append("--cancel-open")
    args.extend(extra)
    return args


def _list_row(order_id, created_ms, ref_id=REF_ID, quantity="0.1", side="buy", **extra):
    stamp = datetime.fromtimestamp(created_ms / 1000, tz=timezone.utc).isoformat()
    row = {"id": order_id, "state": "filled", "side": side, "currency_pair_id": PAIR_ID,
           "quantity": quantity, "created_at": stamp}
    if ref_id is not None:
        row["ref_id"] = ref_id
    row.update(extra)
    return row


def test_resolve_by_order_id_reports_the_fill(tmp_path):
    routes = dict(PREP)
    routes["GET " + ORDER] = [_order("filled", "0.1", "101")]
    code, payload, made = _run(tmp_path, EXECUTE, _resolve(order_id=ORDER_ID), routes)
    assert code == 0, payload
    assert payload["order_outcome"] == "filled"
    assert payload["execution"]["fill"]["avg_px"] == 101.0
    assert _count(made, ORDERS, "POST") == 0


def test_resolve_open_order_without_cancel_stays_open(tmp_path):
    routes = dict(PREP)
    routes["GET " + ORDER] = [_order("confirmed")]
    code, payload, made = _run(tmp_path, EXECUTE, _resolve(order_id=ORDER_ID), routes)
    assert code == 1
    assert payload["order_outcome"] == "open"
    assert _count(made, CANCEL, "POST") == 0


def test_resolve_open_order_with_cancel_reports_the_partial_fill(tmp_path):
    routes = dict(PREP)
    routes["GET " + ORDER] = [_order("confirmed"), _order("confirmed"), _order("cancelled", "0.05", "100")]
    routes["POST " + CANCEL] = [{"status": 200, "json": {}}]
    code, payload, made = _run(tmp_path, EXECUTE, _resolve(order_id=ORDER_ID, cancel_open=True), routes)
    assert code == 0, payload
    assert payload["order_outcome"] == "partially_filled"
    assert payload["execution"]["fill"]["quantity"] == 0.05


def test_resolve_finds_the_order_by_ref_id(tmp_path):
    now_ms = int(time.time() * 1000)
    routes = dict(PREP)
    routes["GET " + ORDERS] = [_page([_list_row("ord-x", now_ms - 1000, ref_id="22222222-2222-4222-8222-222222222222"),
                                      _list_row(ORDER_ID, now_ms - 40000)])]
    routes["GET " + ORDER] = [_order("filled", "0.1", "100")]
    code, payload, _ = _run(tmp_path, EXECUTE, _resolve(), routes)
    assert code == 0, payload
    assert payload["execution"]["fill"]["oid"] == ORDER_ID


def test_resolve_falls_back_to_a_unique_field_match(tmp_path):
    now_ms = int(time.time() * 1000)
    routes = dict(PREP)
    routes["GET " + ORDERS] = [_page([_list_row(ORDER_ID, now_ms - 40000, ref_id=None),
                                      _list_row("ord-y", now_ms - 40000, ref_id=None, quantity="0.3")])]
    routes["GET " + ORDER] = [{"status": 200, "json": {"id": ORDER_ID, "state": "filled", "side": "buy",
                                                       "cumulative_quantity": "0.1", "average_price": "100"}}]
    code, payload, _ = _run(tmp_path, EXECUTE, _resolve(), routes)
    assert code == 0, payload
    assert payload["execution"]["fill"]["oid"] == ORDER_ID


def test_resolve_ambiguous_field_match_is_unknown(tmp_path):
    now_ms = int(time.time() * 1000)
    routes = dict(PREP)
    routes["GET " + ORDERS] = [_page([_list_row(ORDER_ID, now_ms - 40000, ref_id=None),
                                      _list_row("ord-y", now_ms - 41000, ref_id=None)])]
    code, payload, _ = _run(tmp_path, EXECUTE, _resolve(), routes)
    assert code == 1
    assert payload["order_outcome"] == "unknown"


def test_resolve_reports_not_found_only_after_the_settle_window(tmp_path):
    now_ms = int(time.time() * 1000)
    old_ms = now_ms - 30 * 60 * 1000
    routes = dict(PREP)
    routes["GET " + ORDERS] = [_page([_list_row("ord-x", old_ms, ref_id="22222222-2222-4222-8222-222222222222")])]
    code, payload, _ = _run(tmp_path, EXECUTE, _resolve(after_ms=old_ms - 1000, before_ms=old_ms), routes)
    assert code == 1
    assert payload["order_outcome"] == "not_found"
    code, payload, _ = _run(tmp_path, EXECUTE, _resolve(), routes)
    assert code == 1
    assert payload["order_outcome"] == "unknown"


def test_resolve_reads_past_older_rows_to_a_later_page(tmp_path):
    now_ms = int(time.time() * 1000)
    old_ms = now_ms - 60 * 60 * 1000
    other = "22222222-2222-4222-8222-222222222222"
    routes = dict(PREP)
    routes["GET " + ORDERS] = [_page([_list_row("ord-x", old_ms, ref_id=other), _list_row("ord-y", old_ms - 1000, ref_id=other)], ORDERS + "?cursor=2")]
    routes["GET " + ORDERS + "?cursor=2"] = [_page([_list_row(ORDER_ID, now_ms - 40000)])]
    routes["GET " + ORDER] = [_order("filled", "0.1", "100")]
    code, payload, made = _run(tmp_path, EXECUTE, _resolve(), routes)
    assert code == 0, payload
    assert payload["order_outcome"] == "filled"
    assert _count(made, ORDERS + "?cursor=2") == 1


def test_resolve_page_cap_is_unknown(tmp_path):
    routes = dict(PREP)
    for i in range(1, 22):
        url = ORDERS if i == 1 else f"{ORDERS}?cursor={i}"
        routes["GET " + url] = [_page([], f"{ORDERS}?cursor={i + 1}")]
    code, payload, _ = _run(tmp_path, EXECUTE, _resolve(), routes)
    assert code == 1
    assert payload["order_outcome"] == "unknown"


def test_resolve_account_mismatch_is_unknown(tmp_path):
    routes = dict(PREP)
    routes["GET " + ORDER] = [_order("filled", "0.1", "100")]
    code, payload, _ = _run(tmp_path, EXECUTE, _resolve(order_id=ORDER_ID, extra=["--expected-account-id=acct-other"]), routes)
    assert code == 1
    assert payload["order_outcome"] == "unknown"


def test_resolve_operator_order_without_ref_id_must_match_the_attempt_fields(tmp_path):
    now_ms = int(time.time() * 1000)
    routes = dict(PREP)
    stamp = datetime.fromtimestamp((now_ms - 40000) / 1000, tz=timezone.utc).isoformat()
    routes["GET " + ORDER] = [{"status": 200, "json": {"id": ORDER_ID, "state": "filled", "side": "buy",
                                                       "currency_pair_id": PAIR_ID, "quantity": "0.3", "created_at": stamp,
                                                       "cumulative_quantity": "0.3", "average_price": "100"}}]
    code, payload, _ = _run(tmp_path, EXECUTE, _resolve(order_id=ORDER_ID, extra=["--verify-order-fields"]), routes)
    assert code == 1
    assert payload["order_outcome"] == "unknown"


def _pairs(min_size="0.000001", increment="0.00000001", max_size="20", drop=()):
    row = {"id": PAIR_ID, "asset_currency": {"code": "BTC"}, "quote_currency": {"code": "USD"},
           "min_order_size": min_size, "min_order_quantity_increment": increment, "max_order_size": max_size}
    for key in drop:
        row.pop(key, None)
    return {"GET " + CURRENCY_PAIRS: [_page([row])]}


def _holding(quantity, available, **extra):
    row = _held("BTC", quantity, **extra)
    if available is not None:
        row["quantity_available"] = available
    return {"GET " + HOLDINGS: [_page([row])]}


def _sized_sell(quantity, close_kind=None, sole_owner=False, budget_ms=6000):
    args = _sell(quantity, budget_ms)
    if close_kind is not None:
        args.append("--close-kind=" + close_kind)
    if sole_owner:
        args.append("--sole-owner")
    return args


def _sell_routes(sent, holding, pairs=None):
    routes = _routes([_accepted(side="sell")], [_order("filled", sent, "99", side="sell")], **holding)
    routes.update(pairs or {})
    return routes


def _run_sized_sell(tmp_path, quantity, holding, sent, pairs=None, close_kind=None, sole_owner=False):
    code, payload, made = _run(tmp_path, EXECUTE, _sized_sell(quantity, close_kind, sole_owner), _sell_routes(sent, holding, pairs))
    assert code == 0, payload
    posts = _posts(made)
    assert len(posts) == 1, made
    assert _count(made, HOLDINGS) == 1
    _assert_finite_document(payload)
    return payload, posts[0]["body"]["quantity"]


def test_execute_sell_capped_by_the_available_quantity_sends_only_what_the_account_can_sell(tmp_path):
    payload, sent = _run_sized_sell(tmp_path, "0.5", _holding("0.5", "0.3"), "0.3", close_kind="whole", sole_owner=True)
    assert sent == 0.3
    assert payload["requested_quantity"] == 0.3
    assert payload["execution"]["quantity"] == 0.5
    sizing = payload["sizing"]
    assert sizing["asked_quantity"] == 0.5
    assert sizing["account_available"] == 0.3
    assert sizing["sent_quantity"] == 0.3
    assert sizing["capped"] is True
    assert sizing["account_short"] is True
    assert sizing["swept"] is False
    assert payload["execution"]["fill"]["quantity"] == 0.3


@pytest.mark.parametrize("quantity,available,increment,want", [
    ("0.30000000000000004", "1", "0.00000001", 0.3),
    ("1", "0.123456789", "0.0001", 0.1234),
], ids=["ask_off_increment", "available_off_increment"])
def test_execute_sell_floors_to_the_pair_increment_with_an_exact_wire_number(tmp_path, quantity, available, increment, want):
    payload, sent = _run_sized_sell(tmp_path, quantity, _holding("1", available), str(want), pairs=_pairs(min_size="0", increment=increment))
    assert sent == want
    assert repr(sent) == repr(want)
    assert payload["sizing"]["sent_quantity"] == want
    assert payload["requested_quantity"] == want


@pytest.mark.parametrize("close_kind,sole_owner,want,swept", [
    ("whole", True, 0.5004, True),
    ("whole", False, 0.5, False),
    ("partial", True, 0.5, False),
    (None, True, 0.5, False),
], ids=["whole_sole_owner_sweeps", "whole_with_peer_sends_the_ask", "partial_never_sweeps", "missing_close_kind_never_sweeps"])
def test_execute_sell_sweeps_sub_minimum_dust_only_for_a_sole_owner_whole_close(tmp_path, close_kind, sole_owner, want, swept):
    payload, sent = _run_sized_sell(tmp_path, "0.5", _holding("0.5004", "0.5004"), str(want), pairs=_pairs(min_size="0.001"),
                                    close_kind=close_kind, sole_owner=sole_owner)
    assert sent == want
    assert payload["sizing"]["swept"] is swept
    assert payload["sizing"]["close_kind"] == (close_kind or "partial")
    assert payload["sizing"]["sole_owner"] is sole_owner


def test_execute_sell_whole_close_with_a_large_residue_sends_the_ask_and_reports_the_residue(tmp_path):
    payload, sent = _run_sized_sell(tmp_path, "0.5", _holding("0.7", "0.7"), "0.5", pairs=_pairs(min_size="0.001"),
                                    close_kind="whole", sole_owner=True)
    assert sent == 0.5
    assert payload["sizing"]["swept"] is False
    assert payload["sizing"]["capped"] is False
    assert abs(payload["sizing"]["residue_quantity"] - 0.2) < 1e-12


def test_execute_sell_partial_close_sends_the_asked_share(tmp_path):
    payload, sent = _run_sized_sell(tmp_path, "0.25", _holding("1", "1"), "0.25", close_kind="partial")
    assert sent == 0.25
    assert payload["sizing"]["capped"] is False
    assert payload["sizing"]["account_short"] is False


SELL_REFUSALS = {
    "coin_absent": ("0.5", {"GET " + HOLDINGS: [_page([ETH])]}, None, True),
    "available_zero": ("0.5", _holding("0.5", "0"), None, True),
    "below_minimum": ("0.5", _holding("0.5", "0.0005"), _pairs(min_size="0.001"), True),
    "above_maximum": ("0.5", _holding("0.5", "0.5"), _pairs(max_size="0.1"), True),
    "holdings_http_500": ("0.5", {"GET " + HOLDINGS: [{"status": 500, "json": {"detail": "down"}}]}, None, False),
    "available_missing": ("0.5", _holding("0.5", None), None, False),
    "available_nan": ("0.5", _holding("0.5", "NaN"), None, False),
    "available_negative": ("0.5", _holding("0.5", "-1"), None, False),
    "available_above_quantity": ("0.5", _holding("0.5", "0.6"), None, False),
    "minimum_missing": ("0.5", _holding("0.5", "0.5"), _pairs(drop=("min_order_size",)), False),
    "increment_missing": ("0.5", _holding("0.5", "0.5"), _pairs(drop=("min_order_quantity_increment",)), False),
    "increment_zero": ("0.5", _holding("0.5", "0.5"), _pairs(increment="0"), False),
    "ask_text": ("abc", _holding("0.5", "0.5"), None, False),
    "ask_nan": ("NaN", _holding("0.5", "0.5"), None, False),
    "ask_negative": ("-1", _holding("0.5", "0.5"), None, False),
    "ask_overflow": ("1e400", _holding("0.5", "0.5"), None, False),
}


@pytest.mark.parametrize("case", sorted(SELL_REFUSALS))
def test_execute_sell_refusals_send_no_order(tmp_path, case):
    quantity, holding, pairs, names_inputs = SELL_REFUSALS[case]
    code, payload, made = _run(tmp_path, EXECUTE, _sized_sell(quantity, "whole", True), _sell_routes("0.5", holding, pairs))
    assert code == 1
    assert payload["order_outcome"] == "not_sent"
    assert len(_posts(made)) == 0
    assert "fill" not in payload["execution"]
    assert "sizing" not in payload
    if names_inputs:
        for token in ("R=", "A=", "M="):
            assert token in payload["error"], payload["error"]
    _assert_finite_document(payload)


def _close(budget_ms=6000, quantity="0.5"):
    return ["--symbol=BTC", "--mode=live", "--quantity=" + quantity, "--ref-id=" + REF_ID, f"--budget-ms={budget_ms}"]


def test_close_reports_the_polled_fill_and_not_the_held_quantity(tmp_path):
    routes = _routes([_accepted(side="sell")], [_order("filled", "0.4", "99.25", side="sell")],
                     **{"GET " + HOLDINGS: [_page([BTC_SELLABLE])]})
    code, payload, made = _run(tmp_path, CLOSE, _close(), routes)
    assert code == 0, payload
    assert payload["order_outcome"] == "filled"
    assert payload["close"]["fill"]["total_sz"] == 0.4
    assert payload["close"]["fill"]["avg_px"] == 99.25
    assert payload["close"]["held_qty"] == 0.5
    assert _posts(made)[0]["body"]["quantity"] == 0.5


def test_close_partial_then_cancel_reports_the_partial_fill(tmp_path):
    routes = _routes([_accepted(side="sell")], [_order("confirmed", side="sell"), _order("canceled", "0.1", "99", side="sell")],
                     **{"GET " + HOLDINGS: [_page([BTC_SELLABLE])]})
    code, payload, _ = _run(tmp_path, CLOSE, _close(), routes)
    assert code == 0, payload
    assert payload["order_outcome"] == "partially_filled"
    assert payload["close"]["fill"]["total_sz"] == 0.1


@pytest.mark.parametrize("post,outcome", [
    ({"status": 400, "json": {"detail": "refused"}}, "rejected"),
    ({"status": 503, "json": {"detail": "unavailable"}}, "unknown"),
], ids=["refused", "http_503"])
def test_close_failed_submit_exits_one(tmp_path, post, outcome):
    routes = _routes([post], **{"GET " + HOLDINGS: [_page([BTC_SELLABLE])]})
    code, payload, _ = _run(tmp_path, CLOSE, _close(), routes)
    assert code == 1
    assert payload["order_outcome"] == outcome
    assert "fill" not in payload["close"]


@pytest.mark.parametrize("post,outcome,sends", [
    ({"raise": "read_timeout"}, "unknown", 1),
    ({"raise": "connection"}, "unknown", 1),
    ({"raise": "new_connection"}, "not_sent", 3),
    ({"raise": "connect_timeout"}, "not_sent", 3),
], ids=["read_timeout_after_send", "connection_reset_after_send", "refused_before_send", "connect_timeout_before_send"])
def test_close_transport_failure_on_the_sell_exits_one_with_no_fill(tmp_path, post, outcome, sends):
    routes = _routes([post], **{"GET " + HOLDINGS: [_page([BTC_SELLABLE])]})
    code, payload, made = _run(tmp_path, CLOSE, _close(), routes)
    assert code == 1, payload
    assert payload["order_outcome"] == outcome
    assert isinstance(payload.get("error"), str) and payload["error"]
    assert "fill" not in payload["close"]
    assert payload["close"].get("already_flat") is None
    posts = _posts(made)
    assert len(posts) == sends, made
    assert {p["body"]["ref_id"] for p in posts} == {REF_ID}
    _assert_finite_document(payload)


def _run_emergency_close(tmp_path, quantity, holding, sent="0.4", pairs=None):
    routes = _sell_routes(sent, holding, pairs)
    return _run(tmp_path, CLOSE, _close(quantity=quantity), routes)


@pytest.mark.parametrize("quantity,want", [("0.4", 0.4), ("0.30000000000000004", 0.3)], ids=["book_below_account", "ask_off_increment"])
def test_emergency_close_sells_at_most_the_book_and_reports_the_residue(tmp_path, quantity, want):
    code, payload, made = _run_emergency_close(tmp_path, quantity, _holding("1.0", "1.0"), sent=str(want))
    assert code == 0, payload
    posts = _posts(made)
    assert len(posts) == 1, made
    assert posts[0]["body"]["quantity"] == want
    assert payload["requested_quantity"] == want
    assert payload["close"]["held_qty"] == 1.0
    assert payload["close"]["available_qty"] == 1.0
    sizing = payload["sizing"]
    assert sizing["asked_quantity"] == float(quantity)
    assert sizing["account_quantity"] == 1.0
    assert sizing["account_available"] == 1.0
    assert sizing["sent_quantity"] == want
    assert abs(sizing["residue_quantity"] - (1.0 - want)) < 1e-12
    assert sizing["close_kind"] == "emergency"
    assert sizing["swept"] is False
    assert sizing["sole_owner"] is False
    _assert_finite_document(payload)


def test_emergency_close_never_sweeps_a_sub_minimum_residue(tmp_path):
    code, payload, made = _run_emergency_close(tmp_path, "0.5", _holding("0.5004", "0.5004"), sent="0.5", pairs=_pairs(min_size="0.001"))
    assert code == 0, payload
    posts = _posts(made)
    assert len(posts) == 1 and posts[0]["body"]["quantity"] == 0.5
    assert payload["sizing"]["swept"] is False
    assert abs(payload["sizing"]["residue_quantity"] - 0.0004) < 1e-12


def test_emergency_close_below_the_venue_minimum_sends_no_order(tmp_path):
    code, payload, made = _run_emergency_close(tmp_path, "0.0005", _holding("0.5", "0.5"), pairs=_pairs(min_size="0.001"))
    assert code == 1
    assert payload["order_outcome"] == "not_sent"
    assert "minimum" in payload["error"] and "R=0.0005" in payload["error"] and "M=0.001" in payload["error"]
    assert len(_posts(made)) == 0
    assert "sizing" not in payload


def test_emergency_close_with_no_total_holding_is_already_flat(tmp_path):
    code, payload, made = _run(tmp_path, CLOSE, _close(quantity="0.5"), {"GET " + HOLDINGS: [_page([ETH])]})
    assert code == 0, payload
    assert payload["close"]["already_flat"] is True
    assert payload.get("error") is None
    assert len(_posts(made)) == 0
    assert _count(made, CURRENCY_PAIRS) == 0


def test_emergency_close_with_reserved_holdings_is_not_flat_and_sends_no_order(tmp_path):
    code, payload, made = _run_emergency_close(tmp_path, "0.5", _holding("0.5", "0"))
    assert code == 1
    assert payload["order_outcome"] == "not_sent"
    assert payload["close"].get("already_flat") is None
    assert payload["close"]["held_qty"] == 0.5
    assert payload["close"]["available_qty"] == 0.0
    assert "A=0" in payload["error"]
    assert len(_posts(made)) == 0


@pytest.mark.parametrize("args", [
    ["--symbol=BTC", "--mode=live", "--ref-id=" + REF_ID],
    ["--symbol=BTC", "--mode=live", "--quantity=abc", "--ref-id=" + REF_ID],
    ["--symbol=BTC", "--mode=live", "--quantity=0", "--ref-id=" + REF_ID],
    ["--symbol=BTC", "--mode=live", "--quantity=-1", "--ref-id=" + REF_ID],
    ["--symbol=BTC", "--mode=live", "--quantity=NaN", "--ref-id=" + REF_ID],
    ["--symbol=BTC", "--mode=live", "--quantity=0.5", "--unknown-flag"],
], ids=["missing", "text", "zero", "negative", "nan", "unknown_flag"])
def test_emergency_close_refuses_a_missing_or_invalid_quantity_before_login(tmp_path, args):
    proc, payload, made = _run_capture(tmp_path, CLOSE, args, _single([BTC_SELLABLE]))
    assert proc.returncode == 1, proc.stdout
    assert len([ln for ln in proc.stdout.splitlines() if ln.strip()]) == 1, proc.stdout
    assert payload["order_outcome"] == "not_sent"
    assert isinstance(payload.get("error"), str) and payload["error"]
    assert made == []


LOGIN_FAILED = "Robinhood login failed:"
PATHFINDER = "https://api.robinhood.com/pathfinder/user_machine/"
POSITIONS_CHECK = "https://api.robinhood.com/positions/?nonzero=true"
FAKE_SECRETS = ("integration-user", "integration-password", "JBSWY3DPEHPK3PXP", "fake-access-token", "fake-refresh-token")
LOGIN_FAILURES = {
    "refused": (LOGIN_REFUSED, {}, None),
    "connection_error": ([{"raise": "connection"}], {}, None),
    "no_refresh_token": ([{"status": 200, "json": {"access_token": "fake-access-token", "token_type": "Bearer"}}], {}, "'refresh_token'"),
    "empty_access_token": ([{"status": 200, "json": {
        "access_token": "", "token_type": "Bearer", "refresh_token": "fake-refresh-token",
    }}], {}, "no access token"),
    "no_token_type": ([{"status": 200, "json": {
        "access_token": "fake-access-token", "refresh_token": "fake-refresh-token",
    }}], {}, "'token_type'"),
    "verification_workflow": (
        [{"status": 200, "json": {"verification_workflow": {"id": "fake-workflow"}}}],
        {},
        "go-trader robinhood-login --config",
    ),
}
SCRIPTS = {"fetch": (FETCH, []), "close": (CLOSE, ["--symbol=BTC", "--mode=live", "--quantity=0.5"])}


def _default_session(run_dir):
    return os.path.join(os.path.realpath(run_dir), "robinhood-session")


def _run_capture(tmp_path, script, args, routes, login=LOGIN_OK, home=None, session=_default_session):
    run_dir = tmp_path / f"run-{len(list(tmp_path.iterdir()))}"
    site = run_dir / "site"
    site.mkdir(parents=True)
    (site / "sitecustomize.py").write_text(SITECUSTOMIZE)
    if home is None:
        home_dir = run_dir / "home"
        home_dir.mkdir()
    else:
        home_dir = home(run_dir)
    fixture = run_dir / "routes.json"
    fixture.write_text(json.dumps(dict(routes, **{"POST " + TOKEN_URL: login})))
    log = run_dir / "requests.jsonl"
    log.write_text("")
    env = {k: v for k, v in os.environ.items() if not k.startswith("ROBINHOOD")}
    env.update({
        "HOME": str(home_dir),
        "GO_TRADER_ROBINHOOD_SESSION_DIR": session(run_dir),
        "PYTHONPATH": os.pathsep.join([str(site)] + [p for p in [os.environ.get("PYTHONPATH")] if p]),
        "PYTHONDONTWRITEBYTECODE": "1",
        "RH_FAKE_FIXTURE": str(fixture),
        "RH_FAKE_LOG": str(log),
        "ROBINHOOD_USERNAME": "integration-user",
        "ROBINHOOD_PASSWORD": "integration-password",
        "ROBINHOOD_TOTP_SECRET": "JBSWY3DPEHPK3PXP",
    })
    proc = subprocess.run(
        [sys.executable, script, *args],
        cwd=ROOT, env=env, capture_output=True, text=True, timeout=120, stdin=subprocess.DEVNULL,
    )
    lines = [ln for ln in proc.stdout.splitlines() if ln.strip()]
    assert lines, proc.stderr
    payload = json.loads(lines[-1])
    requests_made = [json.loads(ln) for ln in log.read_text().splitlines() if ln.strip()]
    assert [r["url"] for r in requests_made if not r["listed"]] == []
    return proc, payload, requests_made


def _assert_login_refused(proc, payload, made):
    detail = f"exit={proc.returncode}\nstdout:\n{proc.stdout}\nstderr:\n{proc.stderr}\nrequests:\n{made}"
    assert proc.returncode == 1, detail
    assert isinstance(payload.get("error"), str) and payload["error"].startswith(LOGIN_FAILED), detail
    assert _count(made, HOLDINGS) == 0, detail
    assert _count(made, CURRENCY_PAIRS) == 0, detail
    assert _count(made, ORDERS, "POST") == 0, detail
    assert [r["url"] for r in made if r["auth"]] == [], detail
    assert [s for s in FAKE_SECRETS if s in proc.stdout or s in proc.stderr] == [], detail
    if "close" in payload:
        assert payload["close"].get("already_flat") is None, detail
    return detail


CACHED_DEVICE = "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0"


def _account_key(username):
    return hashlib.sha256(("go-trader/robinhood-username/v1:" + username.strip().lower()).encode()).hexdigest()[:16]


def _seeded_session(run_dir):
    folder = _default_session(run_dir)
    os.mkdir(folder, 0o700)
    path = os.path.join(folder, "robinhood-%s.pickle" % _account_key("integration-user"))
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "wb") as f:
        pickle.dump({
            "token_type": "Bearer",
            "access_token": "fake-access-token",
            "refresh_token": "fake-refresh-token",
            "device_token": CACHED_DEVICE,
        }, f)
    return folder


def _unwritable_session(run_dir):
    folder = _default_session(run_dir)
    os.mkdir(folder, 0o500)
    return folder


def _session_under_file(run_dir):
    blocker = run_dir / "session-blocker"
    blocker.write_text("")
    return os.path.join(os.path.realpath(blocker), "robinhood-session")


@pytest.mark.parametrize("script_id", sorted(SCRIPTS))
@pytest.mark.parametrize("shape", sorted(LOGIN_FAILURES))
def test_failed_login_stops_script_before_any_account_request(tmp_path, shape, script_id):
    login, extra_routes, want_reason = LOGIN_FAILURES[shape]
    script, args = SCRIPTS[script_id]
    proc, payload, made = _run_capture(tmp_path, script, args, dict(_single([BTC]), **extra_routes), login=login)
    detail = _assert_login_refused(proc, payload, made)
    assert _count(made, TOKEN_URL, "POST") == 1, detail
    assert _count(made, PATHFINDER, "POST") == 0, detail
    if want_reason is not None:
        assert want_reason in payload["error"], detail


@pytest.mark.skipif(os.geteuid() == 0, reason="root ignores folder permissions")
@pytest.mark.parametrize("script_id", sorted(SCRIPTS))
def test_unwritable_session_folder_refuses_login_before_any_request(tmp_path, script_id):
    script, args = SCRIPTS[script_id]
    proc, payload, made = _run_capture(tmp_path, script, args, _single([BTC]), session=_unwritable_session)
    detail = _assert_login_refused(proc, payload, made)
    assert "is not writable" in payload["error"], detail
    assert made == [], detail


@pytest.mark.parametrize("script_id", sorted(SCRIPTS))
def test_session_folder_under_a_file_refuses_login_before_any_request(tmp_path, script_id):
    script, args = SCRIPTS[script_id]
    proc, payload, made = _run_capture(tmp_path, script, args, _single([BTC]), session=_session_under_file)
    detail = _assert_login_refused(proc, payload, made)
    assert "GO_TRADER_ROBINHOOD_SESSION_DIR" in payload["error"], detail
    assert made == [], detail


def test_cached_session_login_reads_holdings_with_its_token(tmp_path):
    routes = dict(_single([BTC]), **{"GET " + POSITIONS_CHECK: [_page([])]})
    proc, payload, made = _run_capture(tmp_path, FETCH, [], routes, session=_seeded_session)
    detail = f"exit={proc.returncode}\nstdout:\n{proc.stdout}\nstderr:\n{proc.stderr}\nrequests:\n{made}"
    assert proc.returncode == 0, detail
    assert payload.get("error") is None, detail
    assert payload["positions"] == [{"coin": "BTC", "size": 0.5, "avg_price": 40000.0}], detail
    assert _count(made, TOKEN_URL, "POST") == 0, detail
    holdings = [r for r in made if r["url"].startswith(HOLDINGS)]
    assert len(holdings) == 1 and all(r["auth"] for r in holdings), detail


def test_rejected_cached_session_falls_back_to_password_login(tmp_path):
    routes = dict(_single([BTC]), **{"GET " + POSITIONS_CHECK: [{"status": 401, "json": {"detail": "expired"}}]})
    proc, payload, made = _run_capture(tmp_path, FETCH, [], routes, session=_seeded_session)
    detail = f"exit={proc.returncode}\nstdout:\n{proc.stdout}\nstderr:\n{proc.stderr}\nrequests:\n{made}"
    assert proc.returncode == 0, detail
    assert payload.get("error") is None, detail
    assert payload["positions"] == [{"coin": "BTC", "size": 0.5, "avg_price": 40000.0}], detail
    assert _count(made, TOKEN_URL, "POST") == 1, detail
    assert _count(made, POSITIONS_CHECK) == 1, detail


if __name__ == "__main__":
    sys.exit(pytest.main([__file__, "-v"]))
