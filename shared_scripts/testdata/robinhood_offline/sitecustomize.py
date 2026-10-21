import importlib
import json
import os
import socket
import sys

_MODE = os.environ.get("RH_OFFLINE_STUB_MODE", "")
_MODES = ("login_ok", "login_rejected", "transport_error")
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

    def stub_send(self, request, *args, **kwargs):
        if _MODE == "transport_error":
            raise requests.exceptions.ConnectionError("robinhood offline stub: transport down for %s" % request.url)
        if request.method == "POST" and "/oauth2/token/" in request.url:
            if _MODE == "login_ok":
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
