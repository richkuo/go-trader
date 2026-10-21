import importlib
import json
import os
import socket
import sys
import time

_MODE = os.environ.get("RH_OFFLINE_STUB_MODE", "")
_MODES = ("login_ok", "login_rejected", "transport_error", "order_accepted", "order_rejected", "order_unknown", "routes")
_ORDER_MODES = ("order_accepted", "order_rejected", "order_unknown")
_PAIR_ID = "offline-btc-pair"
_NOISE_MARKER = "robinhood offline stub stdout noise"


def _install():
    if _MODE not in _MODES:
        sys.stderr.write("robinhood offline stub: unknown RH_OFFLINE_STUB_MODE %r\n" % _MODE)
        sys.stderr.flush()
        os._exit(97)

    real_connect = socket.socket.connect

    def blocked_connect(self, address, *args, **kwargs):
        if self.family in (socket.AF_INET, socket.AF_INET6):
            raise OSError("network blocked by the robinhood offline stub: %r" % (address,))
        return real_connect(self, address, *args, **kwargs)

    socket.socket.connect = blocked_connect

    try:
        import curl_cffi.requests as curl_requests
    except ImportError:
        curl_requests = None
    if curl_requests is not None:
        def blocked_curl(self, method, url, *args, **kwargs):
            raise OSError("network blocked by the robinhood offline stub: %s %s" % (method, url))

        curl_requests.Session.request = blocked_curl

    import pandas as pd
    import requests
    import requests.adapters
    import requests.models
    import yfinance

    def stub_history(self, *args, **kwargs):
        n = 260
        index = pd.date_range(end=pd.Timestamp("2026-10-01 00:00", tz="UTC"), periods=n, freq="h")
        close = [100.0 + ((i * 7) % 23) - 11 + i * 0.05 for i in range(n)]
        return pd.DataFrame(
            {
                "Open": close,
                "High": [c + 1.0 for c in close],
                "Low": [c - 1.0 for c in close],
                "Close": close,
                "Volume": [1000.0] * n,
            },
            index=index,
        )

    yfinance.Ticker.history = stub_history

    def response(request, status, body):
        resp = requests.models.Response()
        resp.status_code = status
        resp._content = json.dumps(body).encode()
        resp.headers["Content-Type"] = "application/json"
        resp.url = request.url
        resp.request = request
        resp.reason = "robinhood offline stub"
        resp.encoding = "utf-8"
        return resp

    def order_answer(request):
        path = request.url.split("?", 1)[0]
        if request.method == "GET" and path == "https://nummus.robinhood.com/currency_pairs/":
            return response(request, 200, {"next": None, "results": [
                {"id": _PAIR_ID, "symbol": "BTC-USD", "asset_currency": {"code": "BTC"}},
            ]})
        if request.method == "GET" and path == "https://api.robinhood.com/marketdata/forex/quotes/%s/" % _PAIR_ID:
            return response(request, 200, {"id": _PAIR_ID, "ask_price": "100.00", "bid_price": "99.00", "mark_price": "99.50"})
        if request.method == "GET" and path == "https://nummus.robinhood.com/accounts/":
            return response(request, 200, {"next": None, "results": [{"id": "offline-account"}]})
        if request.method == "GET" and path == "https://nummus.robinhood.com/holdings/":
            return response(request, 200, {"next": None, "results": [
                {"quantity": "0.5", "currency": {"code": "BTC"}, "cost_bases": [{"direct_cost_basis": "50.00"}]},
            ]})
        if request.method == "POST" and path == "https://nummus.robinhood.com/orders/":
            if _MODE == "order_rejected":
                return response(request, 400, {"detail": "robinhood offline stub: insufficient buying power"})
            if _MODE == "order_unknown":
                return response(request, 503, {"detail": "robinhood offline stub has no answer for this request"})
            body = json.loads(request.body or b"{}")
            return response(request, 201, {
                "id": "offline-order-1",
                "state": "unconfirmed",
                "side": body.get("side"),
                "quantity": str(body.get("quantity")),
                "price": str(body.get("price")),
                "average_price": None,
                "cumulative_quantity": "0",
            })
        return None

    routes = {}
    route_counts = {}
    if _MODE == "routes":
        with open(os.environ["RH_OFFLINE_STUB_ROUTES"]) as f:
            routes = json.load(f)
    route_log = os.environ.get("RH_OFFLINE_STUB_LOG", "")

    def log_request(request, timeout, listed, with_body=True):
        if not route_log:
            return
        body = request.body if with_body else None
        if isinstance(body, bytes):
            body = body.decode("utf-8", "replace")
        try:
            parsed = json.loads(body) if body else None
        except ValueError:
            parsed = body
        record = {
            "method": request.method,
            "url": request.url,
            "timeout": list(timeout) if isinstance(timeout, tuple) else timeout,
            "body": parsed,
            "listed": listed,
            "pid": os.getpid(),
        }
        with open(route_log, "a") as f:
            f.write(json.dumps(record) + "\n")

    def route_answer(request, timeout):
        key = request.method + " " + request.url
        items = routes.get(key)
        if items is None and request.method == "POST" and "/oauth2/token/" in request.url:
            log_request(request, timeout, True, with_body=False)
            return response(request, 200, {
                "access_token": "offline-access-token",
                "token_type": "Bearer",
                "refresh_token": "offline-refresh-token",
                "expires_in": 86400,
                "scope": "internal",
            })
        log_request(request, timeout, items is not None)
        if items is None:
            raise RuntimeError("robinhood offline stub: unlisted request " + key)
        n = route_counts.get(key, 0)
        route_counts[key] = n + 1
        item = items[min(n, len(items) - 1)]
        if item.get("sleep_s"):
            time.sleep(float(item["sleep_s"]))
        failure = item.get("raise")
        if failure == "connect_timeout":
            raise requests.exceptions.ConnectTimeout("robinhood offline stub: connect timeout for %s" % request.url)
        if failure == "new_connection":
            from urllib3.exceptions import NewConnectionError
            raise requests.exceptions.ConnectionError(NewConnectionError(None, "robinhood offline stub: connection refused"))
        if failure == "read_timeout":
            raise requests.exceptions.ReadTimeout("robinhood offline stub: read timeout for %s" % request.url)
        if failure == "connection":
            raise requests.exceptions.ConnectionError("robinhood offline stub: connection reset for %s" % request.url)
        resp = requests.models.Response()
        resp.status_code = item.get("status", 200)
        if "json" in item:
            resp._content = json.dumps(item["json"]).encode()
        else:
            resp._content = item.get("text", "").encode()
        resp.headers["Content-Type"] = "application/json"
        for name, value in (item.get("headers") or {}).items():
            resp.headers[name] = value
        resp.url = request.url
        resp.request = request
        resp.reason = "robinhood offline stub"
        resp.encoding = "utf-8"
        return resp

    def stub_send(self, request, *args, **kwargs):
        if _MODE == "routes":
            return route_answer(request, kwargs.get("timeout"))
        if _MODE == "transport_error":
            raise requests.exceptions.ConnectionError("robinhood offline stub: transport down for %s" % request.url)
        if _MODE in _ORDER_MODES:
            answer = order_answer(request)
            if answer is not None:
                return answer
        if request.method == "POST" and "/oauth2/token/" in request.url:
            if _MODE == "login_ok" or _MODE in _ORDER_MODES:
                return response(request, 200, {
                    "access_token": "offline-access-token",
                    "token_type": "Bearer",
                    "refresh_token": "offline-refresh-token",
                    "expires_in": 86400,
                    "scope": "internal",
                })
            return response(request, 400, {"detail": "robinhood offline stub rejected the credentials"})
        return response(request, 503, {"detail": "robinhood offline stub has no answer for this request"})

    requests.adapters.HTTPAdapter.send = stub_send

    if os.environ.get("RH_OFFLINE_STUB_STDOUT_NOISE") == "1":
        import pyotp

        real_now = pyotp.TOTP.now

        def noisy_now(self, *args, **kwargs):
            sys.stdout.write(_NOISE_MARKER + "\n")
            sys.stdout.flush()
            return real_now(self, *args, **kwargs)

        pyotp.TOTP.now = noisy_now

    importlib.import_module("robin_stocks.robinhood")
    sys.stderr.write("robinhood offline stub active: %s\n" % _MODE)
    sys.stderr.flush()


if _MODE:
    _install()
