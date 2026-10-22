import ipaddress
import json
import os
import re
import sys
import threading
from urllib.parse import quote, quote_plus, unquote

MODE_ENV = "GO_TRADER_EGRESS_MODE"
PROXY_ENV = "GO_TRADER_EGRESS_PROXY"
MODE_PROXY = "proxy"
LOOPBACK_NO_PROXY = "localhost,127.0.0.1,::1"
PROXY_ENV_KEYS = ("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy")
NO_PROXY_ENV_KEYS = ("NO_PROXY", "no_proxy")
OUTCOME_PREFIX = "go-trader-egress-outcome "
REDACTED = "[redacted]"
INVALID_LABEL = "<invalid>"
SCHEMES = ("http", "https", "socks5", "socks5h")
SOCKS_SCHEMES = ("socks5", "socks5h")

_CCXT_PROXY_ATTRS = (
    "proxy", "proxyUrl", "proxy_url", "proxyUrlCallback", "proxy_url_callback",
    "httpProxy", "http_proxy", "httpProxyCallback", "http_proxy_callback",
    "httpsProxy", "https_proxy", "httpsProxyCallback", "https_proxy_callback",
    "socksProxy", "socks_proxy", "socksProxyCallback", "socks_proxy_callback",
    "proxies",
)
_PIN_MARK = "_go_trader_egress_pinned"
_USERINFO_EXTRA = set("-._:~!$&'()*+,;=%@")
_HEX = "0123456789abcdefABCDEF"


class EgressPolicyError(RuntimeError):
    pass


class Policy:
    __slots__ = ("scheme", "host", "port", "username", "password", "has_auth", "url", "redacted", "raw")

    def __init__(self, scheme, host, port, username, password, has_auth, url, redacted, raw):
        object.__setattr__(self, "scheme", scheme)
        object.__setattr__(self, "host", host)
        object.__setattr__(self, "port", port)
        object.__setattr__(self, "username", username)
        object.__setattr__(self, "password", password)
        object.__setattr__(self, "has_auth", has_auth)
        object.__setattr__(self, "url", url)
        object.__setattr__(self, "redacted", redacted)
        object.__setattr__(self, "raw", raw)

    def __setattr__(self, name, value):
        raise AttributeError("egress policy is frozen")

    def __delattr__(self, name):
        raise AttributeError("egress policy is frozen")

    def __repr__(self):
        return f"Policy({self.redacted})"

    __str__ = __repr__

    @property
    def is_socks(self):
        return self.scheme in SOCKS_SCHEMES


def _join_host_port(host, port):
    if ":" in host:
        return f"[{host}]:{port}"
    return f"{host}:{port}"


def _valid_optional_port(text):
    if text == "":
        return True
    if text[0] != ":":
        return False
    return all("0" <= c <= "9" for c in text[1:])


def _unescape_userinfo(text):
    out = bytearray()
    i = 0
    raw = text.encode("utf-8", "surrogateescape")
    while i < len(raw):
        b = raw[i]
        if b == 0x25:
            if i + 2 >= len(raw):
                raise ValueError("bad escape")
            pair = raw[i + 1:i + 3]
            if chr(pair[0]) not in _HEX or chr(pair[1]) not in _HEX:
                raise ValueError("bad escape")
            out.append(int(pair.decode("ascii"), 16))
            i += 3
            continue
        out.append(b)
        i += 1
    return bytes(out)


def _go_scheme(raw):
    for i, c in enumerate(raw):
        if "a" <= c <= "z" or "A" <= c <= "Z":
            continue
        if "0" <= c <= "9" or c in "+-.":
            if i == 0:
                return "", raw
            continue
        if c == ":":
            if i == 0:
                raise ValueError("missing protocol scheme")
            return raw[:i], raw[i + 1:]
        return "", raw
    return "", raw


def _go_parse_host(scheme, hostport):
    open_idx = hostport.rfind("[")
    if open_idx > 0:
        raise ValueError("invalid IP-literal")
    if open_idx == 0:
        close_idx = hostport.rfind("]")
        if close_idx < 0:
            raise ValueError("missing ]")
        if not _valid_optional_port(hostport[close_idx + 1:]):
            raise ValueError("invalid port")
        inner = hostport[1:close_idx]
        if "%" in inner:
            raise ValueError("invalid host")
        try:
            ipaddress.IPv6Address(inner)
        except ValueError:
            raise ValueError("invalid host") from None
        return hostport
    first = hostport.find(":")
    if first != -1:
        idx = first
        last = hostport.rfind(":")
        if last != first and scheme not in ("http", "https"):
            idx = last
        if not _valid_optional_port(hostport[idx:]):
            raise ValueError("invalid port")
    if "%" in hostport:
        raise ValueError("invalid host")
    return hostport


def _split_host_port(hostport):
    host, port = hostport, ""
    colon = hostport.rfind(":")
    if colon != -1 and _valid_optional_port(hostport[colon:]):
        host, port = hostport[:colon], hostport[colon + 1:]
    if host.startswith("[") and host.endswith("]"):
        host = host[1:-1]
    return host, port


def _parse_ip(host):
    if "%" in host:
        return None
    try:
        return ipaddress.ip_address(host)
    except ValueError:
        return None


def _host_problem(host):
    if host == "":
        return "the proxy URL has no host"
    ip = _parse_ip(host)
    if ip is not None:
        check = ip.ipv4_mapped if isinstance(ip, ipaddress.IPv6Address) and ip.ipv4_mapped is not None else ip
        if check.is_unspecified or check.is_multicast:
            return "the proxy host is an unspecified or multicast address"
        return None
    if len(host.encode("utf-8", "surrogateescape")) > 253:
        return "the proxy host name is too long"
    trimmed = host[:-1] if host.endswith(".") else host
    for label in trimmed.split("."):
        if label == "" or len(label) > 63:
            return "the proxy host name is not a valid DNS name"
        for i, c in enumerate(label):
            ok = (c == "-" and 0 < i < len(label) - 1) or ("a" <= c <= "z") or ("A" <= c <= "Z") or ("0" <= c <= "9") or c == "_"
            if not ok:
                return "the proxy host name is not a valid DNS name"
    return None


def _userinfo_escape(value):
    return quote(value.encode("utf-8", "surrogateescape"), safe="$&+,;=")


def _decode(value):
    return value.decode("utf-8", "surrogateescape")


def _fail(reason, label=INVALID_LABEL):
    raise EgressPolicyError(f"{label}: {reason}")


def parse_proxy_url(raw):
    if raw == "":
        _fail("the proxy URL is empty")
    if any(ord(c) <= 0x20 or ord(c) == 0x7F for c in raw):
        _fail("the proxy URL contains whitespace or a control character")
    try:
        scheme_raw, rest = _go_scheme(raw)
    except ValueError:
        _fail("the proxy URL does not parse")
    scheme = scheme_raw.lower()
    if scheme == "":
        _fail("the proxy URL has no scheme (use http, https, socks5 or socks5h)")
    if scheme not in SCHEMES:
        _fail("unsupported scheme (use http, https, socks5 or socks5h)")
    if "#" in raw:
        _fail("the proxy URL must not have a fragment")
    if "?" in rest:
        _fail("the proxy URL must not have a query")
    if not rest.startswith("//") or rest.startswith("///"):
        _fail("the proxy URL must have the form scheme://host:port")
    authority, slash, path = rest[2:].partition("/")
    if slash + path not in ("", "/"):
        _fail("the proxy URL must not have a path")
    at = authority.rfind("@")
    userinfo = authority[:at] if at >= 0 else None
    hostport = authority[at + 1:] if at >= 0 else authority
    try:
        hostport = _go_parse_host(scheme, hostport)
    except ValueError:
        _fail("the proxy URL does not parse")
    username = password = b""
    has_auth = userinfo is not None
    if has_auth:
        if not all(("A" <= c <= "Z") or ("a" <= c <= "z") or ("0" <= c <= "9") or c in _USERINFO_EXTRA for c in userinfo):
            _fail("the proxy URL does not parse")
        try:
            if ":" in userinfo:
                user_part, _, pass_part = userinfo.partition(":")
                username = _unescape_userinfo(user_part)
                password = _unescape_userinfo(pass_part)
            else:
                username = _unescape_userinfo(userinfo)
        except ValueError:
            _fail("the proxy URL does not parse")
    host, port_text = _split_host_port(hostport)
    problem = _host_problem(host)
    if problem:
        _fail(problem)
    if port_text == "":
        _fail("the proxy URL needs an explicit port")
    port = int(port_text)
    if port < 1 or port > 65535:
        _fail("the proxy port is not a number in 1-65535")
    host = host.lower()
    redacted = f"{scheme}://{_join_host_port(host, port)}"
    if has_auth and username == b"":
        _fail("the proxy credentials have an empty user name", redacted)
    if has_auth and scheme in SOCKS_SCHEMES and (len(username) > 255 or len(password) > 255):
        _fail("SOCKS5 credentials are limited to 255 bytes each", redacted)
    url_scheme = "socks5h" if scheme == "socks5" else scheme
    user_text = _decode(username)
    pass_text = _decode(password)
    auth = ""
    if has_auth:
        auth = _userinfo_escape(user_text)
        if pass_text:
            auth += ":" + _userinfo_escape(pass_text)
        auth += "@"
    url = f"{url_scheme}://{auth}{_join_host_port(host, port)}"
    return Policy(scheme, host, port, user_text if has_auth else None, pass_text if has_auth else None,
                  has_auth, url, redacted, raw)


_lock = threading.RLock()
_state = None


def _escape_variants(value):
    out = [value]
    data = value.encode("utf-8", "surrogateescape")
    for variant in (
        quote(data, safe=""),
        quote_plus(data, safe=""),
        quote(data, safe="$&+:=@"),
        quote(data, safe="$&+,;="),
        quote(data),
    ):
        out.append(variant)
        out.append(_lower_escapes(variant))
    try:
        out.append(json.dumps(value)[1:-1])
        out.append(json.dumps(value, ensure_ascii=False)[1:-1])
    except (TypeError, ValueError):
        pass
    return out


def _lower_escapes(text):
    return re.sub(r"%[0-9A-F]{2}", lambda match: match.group(0).lower(), text)


def _policy_secrets(policy):
    found = [policy.raw]
    if policy.has_auth:
        found.append(policy.url)
        found.append(f"{policy.username}:{policy.password}")
        found.append(f"{_userinfo_escape(policy.username)}:{_userinfo_escape(policy.password)}")
        if policy.password:
            found.append(policy.password)
        if len(policy.username.encode("utf-8", "surrogateescape")) >= 3:
            found.append(policy.username)
    return found


def _loose_secrets(raw):
    found = [raw]
    body = raw.split("://", 1)[1] if "://" in raw else raw
    for index, char in enumerate(body):
        if char != "@" or index == 0:
            continue
        userinfo = body[:index]
        found.append(userinfo)
        user, _, password = userinfo.partition(":")
        for piece, minimum in ((password, 1), (user, 3)):
            if len(piece) >= minimum:
                found.append(piece)
                decoded = unquote(piece)
                if len(decoded) >= minimum:
                    found.append(decoded)
    return found


def _build_secrets(raw, policy):
    if not raw:
        return ()
    base = _policy_secrets(policy) if policy is not None else _loose_secrets(raw)
    expanded = []
    for value in base:
        if value:
            expanded.extend(v for v in _escape_variants(value) if v)
    unique = sorted(set(expanded), key=len, reverse=True)
    return tuple(unique)


def _load():
    global _state
    with _lock:
        if _state is None:
            mode = os.environ.get(MODE_ENV, "")
            raw = os.environ.get(PROXY_ENV, "")
            is_active = mode == MODE_PROXY or raw != ""
            policy = None
            error = None
            if is_active:
                if raw == "":
                    error = EgressPolicyError(f"{MODE_ENV}={MODE_PROXY} but {PROXY_ENV} is unset or empty")
                else:
                    try:
                        policy = parse_proxy_url(raw)
                    except EgressPolicyError as exc:
                        error = exc
            _state = {
                "active": is_active,
                "policy": policy,
                "error": error,
                "secrets": _build_secrets(raw, policy),
            }
        return _state


def active():
    return _load()["active"]


def policy():
    state = _load()
    if not state["active"]:
        raise EgressPolicyError("egress proxy mode is not active")
    if state["error"] is not None:
        raise EgressPolicyError(redact(str(state["error"])))
    return state["policy"]


def redact(text):
    if text is None:
        return text
    out = str(text)
    for secret in _load()["secrets"]:
        if secret in out:
            out = out.replace(secret, REDACTED)
    return out


def requests_proxies():
    if not active():
        return None
    url = policy().url
    return {"http": url, "https": url}


def proxy_env():
    url = policy().url
    env = {MODE_ENV: MODE_PROXY, PROXY_ENV: policy().raw}
    for key in PROXY_ENV_KEYS:
        env[key] = url
    for key in NO_PROXY_ENV_KEYS:
        env[key] = LOOPBACK_NO_PROXY
    return env


def assert_env():
    if not active():
        return
    os.environ.update(proxy_env())


def pin_requests_session(session):
    if session is None or not active():
        return session
    session.proxies = requests_proxies()
    session.trust_env = False
    return session


def pin_ccxt(exchange):
    if exchange is None or not active():
        return exchange
    current = policy()
    for attr in _CCXT_PROXY_ATTRS:
        if getattr(exchange, attr, None) is not None:
            setattr(exchange, attr, None)
    if current.is_socks:
        exchange.socksProxy = current.url
    else:
        exchange.httpsProxy = current.url
    session = getattr(exchange, "session", None)
    if session is not None and hasattr(session, "trust_env") and hasattr(session, "proxies"):
        pin_requests_session(session)
    return exchange


def _pin_yfinance_config(config_module):
    config_module.YfConfig.network.proxy = policy().url


def pin_yfinance():
    if not active():
        return
    policy()
    import yfinance.config
    _pin_yfinance_config(sys.modules["yfinance.config"])


def _wrap_hyperliquid_api(api_module):
    api_cls = api_module.API
    original = api_cls.__init__
    if getattr(original, _PIN_MARK, False):
        return

    def __init__(self, *args, **kwargs):
        original(self, *args, **kwargs)
        pin_requests_session(getattr(self, "session", None))

    setattr(__init__, _PIN_MARK, True)
    __init__.__wrapped__ = original
    api_cls.__init__ = __init__


def pin_hyperliquid_sdk():
    if not active():
        return
    policy()
    try:
        import hyperliquid.api
    except ImportError:
        return
    _wrap_hyperliquid_api(hyperliquid.api)


def _pin_robin_stocks_globals(globals_module):
    pin_requests_session(getattr(globals_module, "SESSION", None))


def pin_robin_stocks():
    if not active():
        return
    policy()
    import robin_stocks.robinhood.globals
    _pin_robin_stocks_globals(robin_stocks.robinhood.globals)


def _wrap_ccxt_exchange(exchange_module):
    exchange_cls = exchange_module.Exchange
    original = exchange_cls.__init__
    if getattr(original, _PIN_MARK, False):
        return

    def __init__(self, *args, **kwargs):
        original(self, *args, **kwargs)
        pin_ccxt(self)

    setattr(__init__, _PIN_MARK, True)
    __init__.__wrapped__ = original
    exchange_cls.__init__ = __init__


_outcome = {"requests": 0, "proxy_failures": 0, "last_error": ""}
_outcome_lock = threading.Lock()


def _record_attempt():
    with _outcome_lock:
        _outcome["requests"] += 1


def _record_failure(exc):
    reason = redact(f"{type(exc).__name__}: {exc}")
    if len(reason) > 300:
        reason = reason[:300]
    with _outcome_lock:
        _outcome["proxy_failures"] += 1
        _outcome["last_error"] = reason


def outcome():
    with _outcome_lock:
        return dict(_outcome)


def _exception_chain(exc):
    seen = set()
    stack = [exc]
    while stack:
        item = stack.pop()
        if item is None or id(item) in seen or not isinstance(item, BaseException):
            continue
        seen.add(id(item))
        yield item
        stack.extend(a for a in item.args if isinstance(a, BaseException))
        stack.append(item.__cause__)
        stack.append(item.__context__)
        stack.append(getattr(item, "reason", None))


def _is_requests_proxy_failure(exc, proxy_url):
    import requests

    if isinstance(exc, requests.exceptions.ProxyError):
        return True
    if not proxy_url.lower().startswith("socks"):
        return False
    if isinstance(exc, requests.exceptions.SSLError) or not isinstance(exc, requests.exceptions.ConnectionError):
        return False
    from urllib3.exceptions import ConnectTimeoutError, NewConnectionError

    return any(isinstance(item, (NewConnectionError, ConnectTimeoutError)) for item in _exception_chain(exc))


def _wrap_requests_adapters(adapters_module):
    adapter_cls = adapters_module.HTTPAdapter
    original = adapter_cls.send
    if getattr(original, _PIN_MARK, False):
        return
    from requests.utils import select_proxy

    def send(self, request, *args, **kwargs):
        proxies = kwargs.get("proxies")
        if proxies is None and len(args) >= 5:
            proxies = args[4]
        try:
            proxy_url = select_proxy(request.url, proxies or {})
        except Exception:
            proxy_url = None
        if not proxy_url:
            return original(self, request, *args, **kwargs)
        _record_attempt()
        try:
            return original(self, request, *args, **kwargs)
        except Exception as exc:
            if _is_requests_proxy_failure(exc, proxy_url):
                _record_failure(exc)
            raise

    setattr(send, _PIN_MARK, True)
    send.__wrapped__ = original
    adapter_cls.send = send


def _is_loopback_url(url):
    from urllib.parse import urlsplit

    try:
        host = (urlsplit(str(url)).hostname or "").lower()
    except ValueError:
        return False
    if host == "localhost":
        return True
    ip = _parse_ip(host)
    return ip is not None and ip.is_loopback


def _wrap_curl_session(session_module):
    session_cls = session_module.Session
    original = session_cls.request
    if getattr(original, _PIN_MARK, False):
        return

    def request(self, method, url, *args, **kwargs):
        if _is_loopback_url(url):
            return original(self, method, url, *args, **kwargs)
        _record_attempt()
        try:
            return original(self, method, url, *args, **kwargs)
        except Exception as exc:
            if _is_curl_proxy_failure(exc):
                _record_failure(exc)
            raise

    setattr(request, _PIN_MARK, True)
    request.__wrapped__ = original
    session_cls.request = request


def _is_curl_proxy_failure(exc):
    try:
        from curl_cffi.requests.exceptions import ConnectionError as CurlConnectionError
        from curl_cffi.requests.exceptions import ProxyError as CurlProxyError
    except ImportError:
        return False
    if isinstance(exc, CurlProxyError):
        return True
    return isinstance(exc, CurlConnectionError) and getattr(exc, "code", None) == 7


_HOOKS = {
    "requests.adapters": _wrap_requests_adapters,
    "curl_cffi.requests.session": _wrap_curl_session,
    "yfinance.config": _pin_yfinance_config,
    "hyperliquid.api": _wrap_hyperliquid_api,
    "robin_stocks.robinhood.globals": _pin_robin_stocks_globals,
    "ccxt.base.exchange": _wrap_ccxt_exchange,
}


class _HookedLoader:
    def __init__(self, loader, hook):
        self._loader = loader
        self._hook = hook

    def create_module(self, spec):
        create = getattr(self._loader, "create_module", None)
        return create(spec) if create is not None else None

    def exec_module(self, module):
        self._loader.exec_module(module)
        self._hook(module)

    def __getattr__(self, name):
        return getattr(self._loader, name)


class _PostImportFinder:
    def __init__(self, hooks):
        self._hooks = hooks

    def find_spec(self, name, path=None, target=None):
        hook = self._hooks.get(name)
        if hook is None:
            return None
        for finder in sys.meta_path:
            if finder is self:
                continue
            find = getattr(finder, "find_spec", None)
            if find is None:
                continue
            spec = find(name, path, target)
            if spec is not None:
                break
        else:
            return None
        if spec.loader is None or not hasattr(spec.loader, "exec_module"):
            return spec
        spec.loader = _HookedLoader(spec.loader, hook)
        return spec


_installed = False


def _emit_outcome():
    snapshot = outcome()
    if snapshot["requests"] <= 0:
        return
    line = OUTCOME_PREFIX + json.dumps(
        {"requests": snapshot["requests"], "proxy_failures": snapshot["proxy_failures"],
         "last_error": redact(snapshot["last_error"])},
        separators=(",", ":"),
    ) + "\n"
    try:
        if sys.stderr is not None:
            sys.stderr.flush()
    except (OSError, ValueError):
        pass
    try:
        os.write(2, line.encode("utf-8", "replace"))
    except OSError:
        pass


def install():
    global _installed
    if not active():
        return
    policy()
    with _lock:
        if _installed:
            return
        _installed = True
        assert_env()
        for name, hook in _HOOKS.items():
            module = sys.modules.get(name)
            if module is not None:
                hook(module)
        sys.meta_path.insert(0, _PostImportFinder(dict(_HOOKS)))
        import atexit

        atexit.register(_emit_outcome)
