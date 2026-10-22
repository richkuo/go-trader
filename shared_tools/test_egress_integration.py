import base64
import datetime
import json
import os
import socket
import socketserver
import ssl
import struct
import subprocess
import sys
import textwrap
import threading
import uuid
from pathlib import Path
from urllib.parse import urlsplit

import pytest

from shared_tools.conftest import load_module

REPO_ROOT = Path(__file__).resolve().parents[1]
BOOTSTRAP = REPO_ROOT / "shared_tools" / "egress_bootstrap.py"
PYTHON = sys.executable
POLICY = load_module("_egress_policy_integration", REPO_ROOT / "shared_tools" / "egress_policy.py")
OUTCOME_PREFIX = "go-trader-egress-outcome "
SENTINEL_USER = "gtuser" + uuid.uuid4().hex[:10]
SENTINEL_PASS = "GTsentinel" + uuid.uuid4().hex
WRONG_PASS = "GTwrong" + uuid.uuid4().hex
SENTINELS = (SENTINEL_USER, SENTINEL_PASS, WRONG_PASS)
PROXY_KEYS = {"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE",
              "SSL_CERT_FILE", "GO_TRADER_EGRESS_MODE", "GO_TRADER_EGRESS_PROXY"}


class _Recorder:
    def __init__(self):
        self.lock = threading.Lock()
        self.events = []

    def add(self, **event):
        with self.lock:
            self.events.append(event)

    def snapshot(self):
        with self.lock:
            return list(self.events)

    def targets(self):
        return [e["target"] for e in self.snapshot() if e["kind"] == "connect"]


class _Server(socketserver.ThreadingTCPServer):
    daemon_threads = True
    allow_reuse_address = True


def _recv_exact(sock, size):
    data = b""
    while len(data) < size:
        chunk = sock.recv(size - len(data))
        if not chunk:
            raise ConnectionError("peer closed")
        data += chunk
    return data


class _Proxy:
    def __init__(self, handler, credentials=None, tls_context=None):
        self.recorder = _Recorder()
        self.credentials = credentials
        self.tls_context = tls_context
        owner = self

        class Handler(socketserver.BaseRequestHandler):
            def handle(self):
                sock = self.request
                sock.settimeout(15)
                try:
                    handler(owner, sock)
                except (OSError, ValueError, ConnectionError):
                    pass

        self.server = _Server(("127.0.0.1", 0), Handler)
        self.port = self.server.server_address[1]
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    def close(self):
        self.server.shutdown()
        self.server.server_close()


def _http_connect_handler(proxy, sock):
    if proxy.tls_context is not None:
        try:
            sock = proxy.tls_context.wrap_socket(sock, server_side=True)
        except (ssl.SSLError, OSError):
            proxy.recorder.add(kind="tls_failed")
            return
    data = b""
    while b"\r\n\r\n" not in data and len(data) < 65536:
        chunk = sock.recv(4096)
        if not chunk:
            break
        data += chunk
    head = data.split(b"\r\n\r\n", 1)[0].decode("latin-1")
    lines = head.split("\r\n")
    method, target, _ = lines[0].split(" ", 2)
    headers = {}
    for line in lines[1:]:
        if ":" in line:
            key, value = line.split(":", 1)
            headers[key.strip().lower()] = value.strip()
    if proxy.credentials is not None:
        user, password = proxy.credentials
        expected = "Basic " + base64.b64encode(f"{user}:{password}".encode()).decode()
        if headers.get("proxy-authorization") != expected:
            proxy.recorder.add(kind="auth_failed")
            sock.sendall(b"HTTP/1.1 407 Proxy Authentication Required\r\nProxy-Authenticate: Basic realm=\"t\"\r\n"
                         b"Content-Length: 0\r\nConnection: close\r\n\r\n")
            return
    if method == "CONNECT":
        host_port = target
    else:
        parts = urlsplit(target)
        host_port = f"{parts.hostname}:{parts.port or 80}"
    proxy.recorder.add(kind="connect", method=method, target=host_port)
    sock.sendall(b"HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")


def _socks_handler(proxy, sock):
    version, count = _recv_exact(sock, 2)
    if version != 5:
        proxy.recorder.add(kind="bad_version")
        return
    methods = _recv_exact(sock, count)
    if proxy.credentials is not None:
        if 2 not in methods:
            proxy.recorder.add(kind="auth_failed")
            sock.sendall(b"\x05\xff")
            return
        sock.sendall(b"\x05\x02")
        _recv_exact(sock, 1)
        user = _recv_exact(sock, _recv_exact(sock, 1)[0]).decode("utf-8", "replace")
        password = _recv_exact(sock, _recv_exact(sock, 1)[0]).decode("utf-8", "replace")
        if (user, password) != proxy.credentials:
            proxy.recorder.add(kind="auth_failed")
            sock.sendall(b"\x01\x01")
            return
        sock.sendall(b"\x01\x00")
    else:
        sock.sendall(b"\x05\x00")
    _, command, _, atyp = _recv_exact(sock, 4)
    if atyp == 1:
        host = socket.inet_ntoa(_recv_exact(sock, 4))
    elif atyp == 3:
        host = _recv_exact(sock, _recv_exact(sock, 1)[0]).decode("ascii", "replace")
    elif atyp == 4:
        host = socket.inet_ntop(socket.AF_INET6, _recv_exact(sock, 16))
    else:
        proxy.recorder.add(kind="bad_atyp")
        return
    port = struct.unpack(">H", _recv_exact(sock, 2))[0]
    proxy.recorder.add(kind="connect", target=f"{host}:{port}", atyp=atyp, command=command)
    sock.sendall(b"\x05\x02\x00\x01\x00\x00\x00\x00\x00\x00")


def _direct_handler(proxy, sock):
    proxy.recorder.add(kind="direct")


@pytest.fixture
def servers():
    made = []

    def make(handler, credentials=None, tls_context=None):
        proxy = _Proxy(handler, credentials, tls_context)
        made.append(proxy)
        return proxy

    yield make
    for proxy in made:
        proxy.close()


@pytest.fixture(scope="module")
def tls_material(tmp_path_factory):
    from cryptography import x509
    from cryptography.hazmat.primitives import hashes, serialization
    from cryptography.hazmat.primitives.asymmetric import ec
    from cryptography.x509.oid import NameOID
    import ipaddress

    key = ec.generate_private_key(ec.SECP256R1())
    name = x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, "127.0.0.1")])
    now = datetime.datetime.now(datetime.timezone.utc)
    cert = (
        x509.CertificateBuilder()
        .subject_name(name)
        .issuer_name(name)
        .public_key(key.public_key())
        .serial_number(x509.random_serial_number())
        .not_valid_before(now - datetime.timedelta(days=1))
        .not_valid_after(now + datetime.timedelta(days=2))
        .add_extension(x509.SubjectAlternativeName([x509.IPAddress(ipaddress.ip_address("127.0.0.1"))]), critical=False)
        .add_extension(x509.BasicConstraints(ca=True, path_length=None), critical=True)
        .sign(key, hashes.SHA256())
    )
    directory = tmp_path_factory.mktemp("egress_tls")
    cert_path = directory / "proxy.pem"
    key_path = directory / "proxy.key"
    cert_path.write_bytes(cert.public_bytes(serialization.Encoding.PEM))
    key_path.write_bytes(key.private_bytes(serialization.Encoding.PEM, serialization.PrivateFormat.PKCS8,
                                           serialization.NoEncryption()))
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    context.load_cert_chain(str(cert_path), str(key_path))
    return context, cert_path


def _start_proxy(servers, kind, credentials=(SENTINEL_USER, SENTINEL_PASS), tls_context=None):
    handler = _socks_handler if kind.startswith("socks") else _http_connect_handler
    proxy = servers(handler, credentials, tls_context)
    scheme = {"http": "http", "https": "https", "socks5": "socks5", "socks5h": "socks5h"}[kind]
    auth = f"{credentials[0]}:{credentials[1]}@" if credentials else ""
    return proxy, f"{scheme}://{auth}127.0.0.1:{proxy.port}"


def _base_env(tmp_path):
    env = {k: v for k, v in os.environ.items() if k.upper() not in PROXY_KEYS}
    home = tmp_path / "home"
    home.mkdir(exist_ok=True)
    env["HOME"] = str(home)
    env["PYTHONDONTWRITEBYTECODE"] = "1"
    for key in ("HYPERLIQUID_SECRET_KEY", "HYPERLIQUID_ACCOUNT_ADDRESS", "HYPERLIQUID_TESTNET", "OKX_API_KEY",
                "OKX_API_SECRET", "OKX_PASSPHRASE", "ANTHROPIC_API_KEY", "TOPSTEP_API_KEY", "TOPSTEP_API_SECRET",
                "TOPSTEP_ACCOUNT_ID", "ROBINHOOD_USERNAME", "ROBINHOOD_PASSWORD", "ROBINHOOD_TOTP_SECRET"):
        env.pop(key, None)
    return env


def _contract_env(tmp_path, raw, overrides=None):
    env = _base_env(tmp_path)
    env.update(overrides or {})
    url = POLICY.parse_proxy_url(raw).url
    env["GO_TRADER_EGRESS_MODE"] = "proxy"
    env["GO_TRADER_EGRESS_PROXY"] = raw
    for key in POLICY.PROXY_ENV_KEYS:
        env[key] = url
    for key in POLICY.NO_PROXY_ENV_KEYS:
        env[key] = POLICY.LOOPBACK_NO_PROXY
    return env


def _driver(tmp_path, body, name="driver.py"):
    path = tmp_path / name
    prelude = textwrap.dedent(f"""
        import json
        import os
        import sys
        REPO = {str(REPO_ROOT)!r}
        sys.path.insert(0, os.path.join(REPO, "shared_tools"))
        RESULTS = {{}}

        def attempt(label, fn):
            try:
                value = fn()
                RESULTS[label] = {{"ok": True, "value": value if isinstance(value, (str, int, float, bool, list, dict, type(None))) else repr(type(value))}}
            except BaseException as exc:
                RESULTS[label] = {{"ok": False, "error": type(exc).__name__}}

        import egress_policy

        def mark(value):
            if value is None:
                return None
            if egress_policy.active() and value == egress_policy.policy().url:
                return "policy-url"
            return egress_policy.redact(value)

        def session_state(session):
            proxies = dict(session.proxies)
            return {{"trust_env": session.trust_env, "proxies": {{k: mark(v) for k, v in proxies.items()}}}}
    """)
    path.write_text(prelude + textwrap.dedent(body) + "\nprint(json.dumps(RESULTS))\n")
    return path


def _run(target, env, args=(), stdin=None, boot=True, timeout=240):
    argv = [PYTHON, str(BOOTSTRAP), str(target), *args] if boot else [PYTHON, str(target), *args]
    return subprocess.run(argv, cwd=str(REPO_ROOT), env=env, input=stdin, capture_output=True, text=True,
                          timeout=timeout)


def _assert_no_sentinel(result):
    combined = result.stdout + result.stderr
    for sentinel in SENTINELS:
        assert sentinel not in combined


def _results(result):
    lines = [line for line in result.stdout.splitlines() if line.strip()]
    assert lines, result.stderr[-4000:]
    return json.loads(lines[-1])


def _outcome(result):
    lines = [line for line in result.stderr.splitlines() if line.startswith(OUTCOME_PREFIX)]
    assert len(lines) == 1, result.stderr[-4000:]
    return json.loads(lines[0][len(OUTCOME_PREFIX):])


def _assert_routed(proxy, kind, host, minimum=1):
    connects = [e for e in proxy.recorder.snapshot() if e["kind"] == "connect" and e["target"] == f"{host}:443"]
    assert len(connects) >= minimum, proxy.recorder.snapshot()
    if kind.startswith("socks"):
        assert all(e["atyp"] == 3 for e in connects)
    return connects


PROXY_KINDS = ["http", "socks5", "socks5h"]


@pytest.mark.parametrize("raw_template,label", [
    (None, "missing"),
    ("", "empty"),
    ("socks5://{user}:{password}@proxy.example:1080/path", "path"),
    ("http://{user}:{password}@proxy.example", "no-port"),
    ("ftp://{user}:{password}@proxy.example:21", "scheme"),
    ("http://{user}:{password}@proxy.example:8080?x=1", "query"),
    ("http://{user}:{password}@proxy.example:99999", "port-range"),
    ("{user}:{password}@proxy.example:1080", "no-scheme"),
    ("socks5://{user}:{password} @proxy.example:1080", "whitespace"),
])
def test_invalid_policy_refuses_before_target_runs(tmp_path, raw_template, label):
    marker = tmp_path / "target-ran"
    driver = _driver(tmp_path, f"open({str(marker)!r}, 'w').write('ran')\n")
    env = _base_env(tmp_path)
    env["GO_TRADER_EGRESS_MODE"] = "proxy"
    if raw_template is not None:
        env["GO_TRADER_EGRESS_PROXY"] = raw_template.format(user=SENTINEL_USER, password=SENTINEL_PASS)
    result = _run(driver, env)
    assert result.returncode == 1, result.stderr
    assert not marker.exists()
    payload = json.loads(result.stdout.strip())
    assert set(payload) == {"error"}
    assert payload["error"].startswith("egress policy: ")
    assert OUTCOME_PREFIX not in result.stderr
    _assert_no_sentinel(result)


@pytest.mark.parametrize("kind", PROXY_KINDS)
def test_ccxt_clients_route_through_proxy(tmp_path, servers, kind):
    proxy, raw = _start_proxy(servers, kind)
    driver = _driver(tmp_path, """
        sys.path.insert(0, os.path.join(REPO, "platforms", "binanceus"))
        import adapter as binanceus_adapter
        binanceus = binanceus_adapter._get_ccxt_exchange()
        RESULTS["binanceus_proxy"] = [mark(binanceus.httpsProxy), mark(binanceus.socksProxy)]
        RESULTS["binanceus_session"] = session_state(binanceus.session)
        attempt("binanceus", lambda: binanceus.fetch_ticker("BTC/USDT"))
        sys.path.remove(os.path.join(REPO, "platforms", "binanceus"))
        del sys.modules["adapter"]
        sys.path.insert(0, os.path.join(REPO, "platforms", "okx"))
        import adapter as okx_adapter
        okx = okx_adapter.OKXExchangeAdapter()
        RESULTS["okx_proxy"] = [mark(okx._exchange.httpsProxy), mark(okx._exchange.socksProxy)]
        attempt("okx", lambda: okx._exchange.fetch_ticker("BTC/USDT"))
        import ccxt
        kraken = ccxt.kraken({"enableRateLimit": False})
        RESULTS["kraken_proxy"] = [mark(kraken.httpsProxy), mark(kraken.socksProxy)]
        attempt("kraken", lambda: kraken.fetch_ticker("BTC/USD"))
    """)
    result = _run(driver, _contract_env(tmp_path, raw))
    _assert_no_sentinel(result)
    results = _results(result)
    expected_pair = [None, "policy-url"] if kind.startswith("socks") else ["policy-url", None]
    assert results["binanceus_proxy"] == expected_pair
    assert results["okx_proxy"] == expected_pair
    assert results["kraken_proxy"] == expected_pair
    assert results["binanceus_session"] == {"trust_env": False, "proxies": {"http": "policy-url", "https": "policy-url"}}
    for label in ("binanceus", "okx", "kraken"):
        assert results[label]["ok"] is False
    _assert_routed(proxy, kind, "api.binance.us")
    _assert_routed(proxy, kind, "www.okx.com")
    _assert_routed(proxy, kind, "api.kraken.com")
    outcome = _outcome(result)
    assert outcome["requests"] >= 3
    assert outcome["proxy_failures"] == outcome["requests"]
    assert outcome["last_error"]


@pytest.mark.parametrize("kind", PROXY_KINDS)
def test_requests_clients_route_through_proxy(tmp_path, servers, kind):
    proxy, raw = _start_proxy(servers, kind)
    child = tmp_path / "child.py"
    child.write_text(textwrap.dedent("""
        import sys
        import requests
        assert "egress_policy" not in sys.modules
        try:
            requests.get("https://api.hyperliquid-testnet.xyz/info", timeout=20)
        except requests.RequestException as exc:
            print(type(exc).__name__)
    """))
    driver = _driver(tmp_path, f"""
        import subprocess
        import requests
        sys.path.insert(0, os.path.join(REPO, "platforms", "deribit"))
        import utils as deribit_utils
        attempt("deribit", lambda: deribit_utils.fetch_available_expiries("BTC"))
        attempt("module_get", lambda: requests.get("https://api.coinbase.com/v2/time", timeout=20))
        attempt("session_get", lambda: requests.Session().get("https://api.gemini.com/v1/symbols", timeout=20))
        os.environ.update({{"TOPSTEP_API_KEY": "k", "TOPSTEP_API_SECRET": "s", "TOPSTEP_ACCOUNT_ID": "1"}})
        sys.path.insert(0, os.path.join(REPO, "platforms", "topstep"))
        del sys.modules["utils"]
        import importlib.util
        spec = importlib.util.spec_from_file_location("topstep_adapter", os.path.join(REPO, "platforms", "topstep", "adapter.py"))
        topstep = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(topstep)
        live = topstep.TopStepExchangeAdapter(mode="live")
        RESULTS["topstep_session"] = session_state(live._session)
        attempt("topstep", live.get_open_positions_raise)
        os.environ["ANTHROPIC_API_KEY"] = "test-key"
        spec = importlib.util.spec_from_file_location("llm_review_driver", os.path.join(REPO, "shared_scripts", "llm_review.py"))
        review = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(review)
        attempt("advisory", lambda: review.build_llm_call("claude-opus-5-5")("system", "user"))
        child = subprocess.run([sys.executable, {str(child)!r}], capture_output=True, text=True, timeout=120)
        RESULTS["child"] = [child.returncode, child.stdout.strip()]
    """)
    result = _run(driver, _contract_env(tmp_path, raw))
    _assert_no_sentinel(result)
    results = _results(result)
    assert results["deribit"] == {"ok": True, "value": []}
    for label in ("module_get", "session_get", "topstep", "advisory"):
        assert results[label]["ok"] is False, results
    assert results["topstep_session"] == {"trust_env": False, "proxies": {"http": "policy-url", "https": "policy-url"}}
    assert results["child"][0] == 0
    for host in ("www.deribit.com", "api.coinbase.com", "api.gemini.com", "api.topstepx.com", "api.anthropic.com",
                 "api.hyperliquid-testnet.xyz"):
        _assert_routed(proxy, kind, host)
    outcome = _outcome(result)
    assert outcome["requests"] >= 5
    assert outcome["proxy_failures"] == outcome["requests"]


@pytest.mark.parametrize("kind", PROXY_KINDS)
def test_hyperliquid_sdk_constructor_branches_route_through_proxy(tmp_path, servers, kind):
    proxy, raw = _start_proxy(servers, kind)
    driver = _driver(tmp_path, """
        sys.path.insert(0, os.path.join(REPO, "platforms", "hyperliquid"))
        import adapter as hl
        from hyperliquid.info import Info
        calls = []

        def counted(label, fn):
            calls.append(label)
            attempt(label, fn)

        counted("raw_meta", lambda: hl._fetch_raw_meta(hl.MAINNET_URL))
        hl._load_meta_cache = lambda *a, **k: None
        hl._save_meta_cache = lambda *a, **k: None
        attempt("cache_miss_adapter", hl.HyperliquidExchangeAdapter)
        calls.extend(["cache_miss_raw", "cache_miss_fallback"])
        counted("info_constructor", lambda: Info(hl.MAINNET_URL, skip_ws=True))
        meta = {"universe": [{"name": "BTC", "szDecimals": 5}]}
        spot = {"universe": [{"name": "PURR/USDC", "tokens": [0, 1], "index": 0}],
                "tokens": [{"name": "PURR", "szDecimals": 0, "index": 0}, {"name": "USDC", "szDecimals": 8, "index": 1}]}
        hl._load_meta_cache = lambda *a, **k: (spot, meta)
        os.environ["HYPERLIQUID_SECRET_KEY"] = "0x" + "11" * 32
        adapter = hl.HyperliquidExchangeAdapter()
        RESULTS["cache_hit_info_session"] = session_state(adapter._info.session)
        counted("cache_hit_info_post", lambda: adapter._info.post("/info", {"type": "meta"}))
        exchange = adapter._ensure_exchange()
        RESULTS["exchange_sessions"] = [session_state(exchange.session), session_state(exchange.info.session)]
        counted("exchange_post", lambda: exchange.post("/exchange", {}))
        counted("exchange_info_post", lambda: exchange.info.post("/info", {"type": "meta"}))
        RESULTS["expected_calls"] = len(calls)
    """)
    result = _run(driver, _contract_env(tmp_path, raw))
    _assert_no_sentinel(result)
    results = _results(result)
    pinned = {"trust_env": False, "proxies": {"http": "policy-url", "https": "policy-url"}}
    assert results["cache_miss_adapter"]["ok"] is False
    assert results["cache_hit_info_session"] == pinned
    assert results["exchange_sessions"] == [pinned, pinned]
    for label in ("raw_meta", "info_constructor", "cache_hit_info_post", "exchange_post", "exchange_info_post"):
        assert results[label]["ok"] is False, results
    connects = _assert_routed(proxy, kind, "api.hyperliquid.xyz")
    assert len(connects) == results["expected_calls"]


@pytest.mark.parametrize("kind", PROXY_KINDS)
def test_robin_stocks_session_routes_before_login(tmp_path, servers, kind):
    proxy, raw = _start_proxy(servers, kind)
    driver = _driver(tmp_path, """
        import robin_stocks.robinhood as rh_direct
        RESULTS["hook_pinned"] = session_state(rh_direct.helper.SESSION)
        sys.path.insert(0, os.path.join(REPO, "platforms", "robinhood"))
        import adapter as robinhood
        with robinhood.robinhood_library() as rh:
            session = rh.helper.SESSION
        RESULTS["adapter_pinned"] = session_state(session)
        attempt("pre_login", lambda: session.get("https://api.robinhood.com/oauth2/token/", timeout=20))
    """)
    result = _run(driver, _contract_env(tmp_path, raw))
    _assert_no_sentinel(result)
    results = _results(result)
    pinned = {"trust_env": False, "proxies": {"http": "policy-url", "https": "policy-url"}}
    assert results["hook_pinned"] == pinned
    assert results["adapter_pinned"] == pinned
    assert results["pre_login"]["ok"] is False
    _assert_routed(proxy, kind, "api.robinhood.com")


@pytest.mark.parametrize("kind", PROXY_KINDS)
def test_yfinance_native_curl_routes_through_proxy(tmp_path, servers, kind):
    proxy, raw = _start_proxy(servers, kind)
    driver = _driver(tmp_path, """
        import importlib.util
        spec = importlib.util.spec_from_file_location("topstep_adapter", os.path.join(REPO, "platforms", "topstep", "adapter.py"))
        topstep = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(topstep)
        RESULTS["price"] = topstep.TopStepExchangeAdapter(mode="paper")._get_yahoo_price("ES")
        from yfinance.config import YfConfig
        RESULTS["yf_proxy"] = mark(YfConfig.network.proxy)
        from curl_cffi import requests as curl_requests
        attempt("native_curl", lambda: curl_requests.get("https://query2.finance.yahoo.com/v8/finance/chart/NQ=F", timeout=20))
    """)
    result = _run(driver, _contract_env(tmp_path, raw))
    _assert_no_sentinel(result)
    results = _results(result)
    assert results["price"] == 0.0
    assert results["yf_proxy"] == "policy-url"
    assert results["native_curl"]["ok"] is False
    yahoo = [e for e in proxy.recorder.snapshot() if e["kind"] == "connect" and e["target"].endswith("yahoo.com:443")]
    assert len(yahoo) >= 2, proxy.recorder.snapshot()
    _assert_routed(proxy, kind, "query2.finance.yahoo.com")
    if kind.startswith("socks"):
        assert all(e["atyp"] == 3 for e in yahoo)
    outcome = _outcome(result)
    assert outcome["requests"] >= 2
    assert outcome["proxy_failures"] >= 1


def test_overrides_restored_and_no_direct_fallback(tmp_path, servers):
    proxy, raw = _start_proxy(servers, "socks5h")
    direct = servers(_direct_handler)
    hostile = f"http://127.0.0.1:{direct.port}"
    overrides = {"NO_PROXY": "*", "no_proxy": "*", "HTTP_PROXY": hostile, "HTTPS_PROXY": hostile, "ALL_PROXY": hostile,
                 "http_proxy": hostile, "https_proxy": hostile, "all_proxy": hostile}
    env = _base_env(tmp_path)
    env.update(overrides)
    env["GO_TRADER_EGRESS_MODE"] = "proxy"
    env["GO_TRADER_EGRESS_PROXY"] = raw
    target = f"https://[::ffff:127.0.0.1]:{direct.port}/"
    driver = _driver(tmp_path, f"""
        import requests
        RESULTS["env"] = {{k: mark(os.environ.get(k)) for k in sorted({sorted(overrides)!r})}}
        attempt("module_get", lambda: requests.get({target!r}, timeout=20))
        attempt("venue_get", lambda: requests.get("https://api.hyperliquid.xyz/info", timeout=20))
        from curl_cffi import requests as curl_requests
        attempt("curl_get", lambda: curl_requests.get({target!r}, timeout=20))
    """)
    result = _run(driver, env)
    _assert_no_sentinel(result)
    results = _results(result)
    for key, value in results["env"].items():
        assert value == ("localhost,127.0.0.1,::1" if key.lower() == "no_proxy" else "policy-url")
    for label in ("module_get", "venue_get", "curl_get"):
        assert results[label]["ok"] is False
    assert direct.recorder.snapshot() == []
    assert proxy.recorder.targets().count(f"::ffff:127.0.0.1:{direct.port}") == 2
    _assert_routed(proxy, "socks5h", "api.hyperliquid.xyz")


@pytest.mark.parametrize("kind", ["http", "socks5h"])
def test_wrong_credentials_fail_without_direct_connection(tmp_path, servers, kind):
    handler = _socks_handler if kind.startswith("socks") else _http_connect_handler
    proxy = servers(handler, (SENTINEL_USER, SENTINEL_PASS))
    direct = servers(_direct_handler)
    raw = f"{kind}://{SENTINEL_USER}:{WRONG_PASS}@127.0.0.1:{proxy.port}"
    target = f"https://[::ffff:127.0.0.1]:{direct.port}/"
    driver = _driver(tmp_path, f"""
        import requests
        attempt("module_get", lambda: requests.get({target!r}, timeout=20))
        attempt("venue_get", lambda: requests.get("https://api.hyperliquid.xyz/info", timeout=20))
        from curl_cffi import requests as curl_requests
        attempt("curl_get", lambda: curl_requests.get({target!r}, timeout=20))
    """)
    result = _run(driver, _contract_env(tmp_path, raw))
    _assert_no_sentinel(result)
    results = _results(result)
    for label in ("module_get", "venue_get", "curl_get"):
        assert results[label]["ok"] is False
    events = proxy.recorder.snapshot()
    assert events and all(e["kind"] == "auth_failed" for e in events), events
    assert direct.recorder.snapshot() == []
    outcome = _outcome(result)
    assert outcome["proxy_failures"] >= 2
    assert outcome["last_error"]


def test_tls_proxy_trusted_and_untrusted(tmp_path, servers, tls_material):
    context, cert_path = tls_material
    proxy, raw = _start_proxy(servers, "https", tls_context=context)
    direct = servers(_direct_handler)
    target = f"https://[::ffff:127.0.0.1]:{direct.port}/"
    driver = _driver(tmp_path, f"""
        import requests
        attempt("module_get", lambda: requests.get("https://api.hyperliquid.xyz/info", timeout=20))
        attempt("direct_target", lambda: requests.get({target!r}, timeout=20))
    """)
    trusted = _run(driver, _contract_env(tmp_path, raw, {"REQUESTS_CA_BUNDLE": str(cert_path)}))
    _assert_no_sentinel(trusted)
    assert _results(trusted)["module_get"]["ok"] is False
    _assert_routed(proxy, "https", "api.hyperliquid.xyz")
    before = len(proxy.recorder.snapshot())
    untrusted = _run(driver, _contract_env(tmp_path, raw))
    _assert_no_sentinel(untrusted)
    results = _results(untrusted)
    assert results["module_get"]["ok"] is False
    assert results["direct_target"]["ok"] is False
    new_events = proxy.recorder.snapshot()[before:]
    assert new_events and all(e["kind"] == "tls_failed" for e in new_events), new_events
    assert direct.recorder.snapshot() == []
    outcome = _outcome(untrusted)
    assert outcome["proxy_failures"] == outcome["requests"] == 2


def test_outcome_line_counts_and_absence(tmp_path, servers):
    proxy, raw = _start_proxy(servers, "socks5")
    driver = _driver(tmp_path, """
        import requests
        for _ in range(3):
            attempt("get", lambda: requests.get("https://api.hyperliquid.xyz/info", timeout=20))
    """)
    result = _run(driver, _contract_env(tmp_path, raw))
    _assert_no_sentinel(result)
    outcome = _outcome(result)
    assert outcome["requests"] == 3
    assert outcome["proxy_failures"] == 3
    assert "SOCKS" in outcome["last_error"] or "Connection" in outcome["last_error"]
    idle = _driver(tmp_path, "RESULTS['idle'] = True\n", name="idle.py")
    quiet = _run(idle, _contract_env(tmp_path, raw))
    assert quiet.returncode == 0
    assert OUTCOME_PREFIX not in quiet.stderr
    assert len(proxy.recorder.targets()) == 3


def test_real_scripts_keep_stdout_contract(tmp_path, servers):
    proxy, raw = _start_proxy(servers, "socks5h")
    env = _contract_env(tmp_path, raw)
    price = _run(REPO_ROOT / "shared_scripts" / "check_price.py", env, args=("BTC/USDT",))
    _assert_no_sentinel(price)
    assert price.returncode == 0, price.stderr
    assert json.loads(price.stdout) == {}
    _assert_routed(proxy, "socks5h", "api.binance.us")
    probe = _run(REPO_ROOT / "shared_scripts" / "llm_review.py", env, args=("--probe-only",))
    assert probe.returncode == 0
    assert json.loads(probe.stdout) == {"status": "ok"}
    ctx = {"strategy_id": "hl-btc", "symbol": "BTC", "platform": "nonexistent", "type": "perps", "side": "long",
           "entry_price": 1.0, "timeframe": "1h", "model": "claude-opus-5-5"}
    review = _run(REPO_ROOT / "shared_scripts" / "llm_review.py", dict(env, ANTHROPIC_API_KEY="test-key"),
                  stdin=json.dumps(ctx))
    _assert_no_sentinel(review)
    assert review.returncode == 1
    payload = json.loads(review.stdout)
    assert payload["usage"] == {"calls": 0, "input_tokens": 0, "output_tokens": 0}
    assert payload["error"]
    _assert_routed(proxy, "socks5h", "api.anthropic.com")


def test_bootstrap_preserves_script_semantics(tmp_path):
    script = tmp_path / "semantics.py"
    script.write_text(textwrap.dedent("""
        import os
        import sys
        data = sys.stdin.read()
        print(repr(sys.argv), repr(__name__), repr(__file__), repr(sys.path[0]), repr(__spec__), repr(data))
        sys.stdout.flush()
        sys.exit(int(sys.argv[1]))
    """))
    env = _base_env(tmp_path)
    plain = _run(script, env, args=("7", "x y"), stdin="payload\n", boot=False)
    booted = _run(script, env, args=("7", "x y"), stdin="payload\n", boot=True)
    assert plain.returncode == booted.returncode == 7
    assert plain.stdout == booted.stdout
    assert plain.stderr == booted.stderr == ""
    failing = tmp_path / "failing.py"
    failing.write_text("raise ValueError('boom')\n")
    plain_fail = _run(failing, env, boot=False)
    booted_fail = _run(failing, env, boot=True)
    assert plain_fail.returncode == booted_fail.returncode == 1
    assert plain_fail.stderr == booted_fail.stderr
    assert "egress_bootstrap" not in booted_fail.stderr


def test_inactive_mode_sets_no_proxies(tmp_path):
    driver = _driver(tmp_path, """
        import egress_policy
        import requests
        import ccxt
        from hyperliquid.api import API
        original = API.__init__
        RESULTS["active"] = egress_policy.active()
        RESULTS["proxies"] = egress_policy.requests_proxies()
        session = egress_policy.pin_requests_session(requests.Session())
        RESULTS["session"] = session_state(session)
        exchange = egress_policy.pin_ccxt(ccxt.binanceus({"enableRateLimit": True}))
        RESULTS["ccxt"] = [exchange.httpsProxy, exchange.socksProxy, exchange.session.trust_env]
        egress_policy.pin_hyperliquid_sdk()
        RESULTS["hl_unwrapped"] = API.__init__ is original
        egress_policy.pin_yfinance()
        from yfinance.config import YfConfig
        RESULTS["yf_proxy"] = YfConfig.network.proxy
        RESULTS["env"] = [os.environ.get(k) for k in ("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY")]
    """)
    env = _base_env(tmp_path)
    for result in (_run(driver, env, boot=False), _run(driver, env, boot=True)):
        assert result.returncode == 0, result.stderr
        assert OUTCOME_PREFIX not in result.stderr
        results = _results(result)
        assert results["active"] is False
        assert results["proxies"] is None
        assert results["session"] == {"trust_env": True, "proxies": {}}
        assert results["ccxt"] == [None, None, False]
        assert results["hl_unwrapped"] is True
        assert results["yf_proxy"] is None
        assert results["env"] == [None, None, None, None]


ALL_FAMILIES = ["ccxt", "hyperliquid", "robinhood", "topstep", "yfinance", "requests", "deribit", "advisory"]


def _check(env, *families):
    return subprocess.run([PYTHON, str(BOOTSTRAP), "--check-policy", *families], cwd=str(REPO_ROOT), env=env,
                          capture_output=True, text=True, timeout=240)


@pytest.mark.parametrize("kind", ["http", "socks5"])
def test_check_policy_passes_for_every_family(tmp_path, kind):
    raw = f"{kind}://{SENTINEL_USER}:{SENTINEL_PASS}@127.0.0.1:1"
    result = _check(_contract_env(tmp_path, raw), *ALL_FAMILIES)
    _assert_no_sentinel(result)
    assert result.returncode == 0, result.stdout + result.stderr[-4000:]
    assert result.stdout.strip().splitlines() == [json.dumps({"ok": True, "families": ALL_FAMILIES})]


def test_check_policy_failures(tmp_path):
    bad = _base_env(tmp_path)
    bad["GO_TRADER_EGRESS_MODE"] = "proxy"
    bad["GO_TRADER_EGRESS_PROXY"] = f"socks5://{SENTINEL_USER}:{SENTINEL_PASS}@proxy.example:1080/x"
    invalid = _check(bad, "ccxt")
    _assert_no_sentinel(invalid)
    assert invalid.returncode == 1
    payload = json.loads(invalid.stdout)
    assert payload["ok"] is False and payload["family"] == "policy"
    missing = dict(bad)
    del missing["GO_TRADER_EGRESS_PROXY"]
    assert _check(missing, "requests").returncode == 1
    inactive = _check(_base_env(tmp_path), "requests")
    assert inactive.returncode == 1
    assert json.loads(inactive.stdout)["ok"] is False
    raw = f"socks5h://{SENTINEL_USER}:{SENTINEL_PASS}@127.0.0.1:1"
    unknown = _check(_contract_env(tmp_path, raw), "ccxt", "bogus")
    assert unknown.returncode == 2
    assert json.loads(unknown.stdout) == {"ok": False, "error": "unknown family", "family": "bogus"}
    tamper = tmp_path / "tamper"
    tamper.mkdir()
    (tamper / "sitecustomize.py").write_text(textwrap.dedent("""
        import requests.sessions
        requests.sessions.Session.trust_env = property(lambda self: True, lambda self, value: None)
    """))
    env = _contract_env(tmp_path, raw)
    env["PYTHONPATH"] = str(tamper) + os.pathsep + env.get("PYTHONPATH", "")
    mismatch = _check(env, "requests")
    _assert_no_sentinel(mismatch)
    assert mismatch.returncode == 1
    payload = json.loads(mismatch.stdout)
    assert payload == {"ok": False, "error": "requests session still trusts the environment", "family": "requests"}
