import json
import os
import subprocess
import sys
import textwrap

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
        record = {
            "method": request.method,
            "url": request.url,
            "timeout": list(timeout) if isinstance(timeout, tuple) else timeout,
            "auth": bool(request.headers.get("Authorization")),
            "listed": items is not None,
        }
        with open(_LOG, "a") as f:
            f.write(json.dumps(record) + "\\n")
        if items is None:
            raise RuntimeError("unlisted request " + key)
        n = _COUNTS.get(key, 0)
        _COUNTS[key] = n + 1
        item = items[min(n, len(items) - 1)]
        if item.get("raise") == "connection":
            raise requests.exceptions.ConnectionError("fake connection failure")
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
    code, payload, _ = _run(tmp_path, FETCH, [], FAILED_READS[shape])
    assert code == 1
    assert isinstance(payload.get("error"), str) and payload["error"]
    assert payload["positions"] == []


@pytest.mark.parametrize("shape", sorted(FAILED_READS))
def test_failed_holdings_read_fails_close_script(tmp_path, shape):
    code, payload, made = _run(tmp_path, CLOSE, ["--symbol=BTC", "--mode=live"], FAILED_READS[shape])
    assert code == 1
    assert isinstance(payload.get("error"), str) and payload["error"]
    assert payload["close"].get("already_flat") is None
    assert _count(made, CURRENCY_PAIRS) == 0
    assert _count(made, ORDERS, "POST") == 0


def test_page_cap_stops_after_fifty_holdings_requests(tmp_path):
    code, payload, made = _run(tmp_path, FETCH, [], FAILED_READS["page_cap_exceeded"])
    assert code == 1
    assert payload["positions"] == []
    assert _count(made, HOLDINGS) == 50


def test_unlogged_library_session_sends_no_holdings_request(tmp_path):
    for script, args in ((FETCH, []), (CLOSE, ["--symbol=BTC", "--mode=live"])):
        code, payload, made = _run(tmp_path, script, args, _single([BTC]), login=LOGIN_REFUSED)
        assert code == 1
        assert isinstance(payload.get("error"), str) and payload["error"]
        assert _count(made, HOLDINGS) == 0
        assert _count(made, CURRENCY_PAIRS) == 0


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
    code, payload, made = _run(tmp_path, CLOSE, ["--symbol=BTC", "--mode=live"], routes)
    assert code == 0
    assert payload.get("error") is None
    assert payload["close"]["already_flat"] is True
    assert _count(made, HOLDINGS) == 2
    assert _count(made, CURRENCY_PAIRS) == 0
    assert _count(made, ORDERS, "POST") == 0


def test_close_sells_holding_found_on_later_page(tmp_path):
    routes = {
        "GET " + HOLDINGS: [_page([ETH], PAGE2)],
        "GET " + PAGE2: [_page([BTC])],
        "GET " + CURRENCY_PAIRS: [{"raise": "connection"}],
    }
    code, payload, made = _run(tmp_path, CLOSE, ["--symbol=BTC", "--mode=live"], routes)
    assert code == 1
    assert isinstance(payload.get("error"), str) and payload["error"]
    assert payload["close"].get("already_flat") is None
    assert _count(made, CURRENCY_PAIRS) == 1
    assert _count(made, ORDERS, "POST") == 0


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
        {"POST " + PATHFINDER: [{"status": 200, "json": {}}]},
        "robin_stocks reported Error: No verification ID returned in user-machine response",
    ),
}
SCRIPTS = {"fetch": (FETCH, []), "close": (CLOSE, ["--symbol=BTC", "--mode=live"])}


def _run_capture(tmp_path, script, args, routes, login=LOGIN_OK, home=None):
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


def _home_with_tokens_file(run_dir):
    home = run_dir / "home"
    home.mkdir()
    (home / ".tokens").write_text("")
    return home


def _home_under_file(run_dir):
    blocker = run_dir / "home-blocker"
    blocker.write_text("")
    return blocker / "home"


def _home_with_cached_session(run_dir):
    import pickle

    home = run_dir / "home"
    (home / ".tokens").mkdir(parents=True)
    with open(home / ".tokens" / "robinhood.pickle", "wb") as f:
        pickle.dump({
            "token_type": "Bearer",
            "access_token": "fake-access-token",
            "refresh_token": "fake-refresh-token",
            "device_token": "fake-device-token",
        }, f)
    return home


@pytest.mark.parametrize("script_id", sorted(SCRIPTS))
@pytest.mark.parametrize("shape", sorted(LOGIN_FAILURES))
def test_failed_login_stops_script_before_any_account_request(tmp_path, shape, script_id):
    login, extra_routes, want_reason = LOGIN_FAILURES[shape]
    script, args = SCRIPTS[script_id]
    proc, payload, made = _run_capture(tmp_path, script, args, dict(_single([BTC]), **extra_routes), login=login)
    detail = _assert_login_refused(proc, payload, made)
    assert _count(made, TOKEN_URL, "POST") == 1, detail
    if want_reason is not None:
        assert want_reason in payload["error"], detail


@pytest.mark.parametrize("script_id", sorted(SCRIPTS))
def test_token_cache_write_failure_refuses_login(tmp_path, script_id):
    script, args = SCRIPTS[script_id]
    proc, payload, made = _run_capture(tmp_path, script, args, _single([BTC]), home=_home_with_tokens_file)
    detail = _assert_login_refused(proc, payload, made)
    assert "token cache" in payload["error"], detail
    assert _count(made, TOKEN_URL, "POST") == 1, detail


@pytest.mark.parametrize("script_id", sorted(SCRIPTS))
def test_token_cache_directory_failure_refuses_login(tmp_path, script_id):
    script, args = SCRIPTS[script_id]
    proc, payload, made = _run_capture(tmp_path, script, args, _single([BTC]), home=_home_under_file)
    detail = _assert_login_refused(proc, payload, made)
    assert "raised NotADirectoryError" in payload["error"], detail
    assert _count(made, TOKEN_URL, "POST") == 0, detail


def test_cached_session_login_reads_holdings_with_its_token(tmp_path):
    routes = dict(_single([BTC]), **{"GET " + POSITIONS_CHECK: [_page([])]})
    proc, payload, made = _run_capture(tmp_path, FETCH, [], routes, home=_home_with_cached_session)
    detail = f"exit={proc.returncode}\nstdout:\n{proc.stdout}\nstderr:\n{proc.stderr}\nrequests:\n{made}"
    assert proc.returncode == 0, detail
    assert payload.get("error") is None, detail
    assert payload["positions"] == [{"coin": "BTC", "size": 0.5, "avg_price": 40000.0}], detail
    assert _count(made, TOKEN_URL, "POST") == 0, detail
    holdings = [r for r in made if r["url"].startswith(HOLDINGS)]
    assert len(holdings) == 1 and all(r["auth"] for r in holdings), detail


def test_rejected_cached_session_falls_back_to_password_login(tmp_path):
    routes = dict(_single([BTC]), **{"GET " + POSITIONS_CHECK: [{"status": 401, "json": {"detail": "expired"}}]})
    proc, payload, made = _run_capture(tmp_path, FETCH, [], routes, home=_home_with_cached_session)
    detail = f"exit={proc.returncode}\nstdout:\n{proc.stdout}\nstderr:\n{proc.stderr}\nrequests:\n{made}"
    assert proc.returncode == 0, detail
    assert payload.get("error") is None, detail
    assert payload["positions"] == [{"coin": "BTC", "size": 0.5, "avg_price": 40000.0}], detail
    assert _count(made, TOKEN_URL, "POST") == 1, detail
    assert _count(made, POSITIONS_CHECK) == 1, detail


if __name__ == "__main__":
    sys.exit(pytest.main([__file__, "-v"]))
