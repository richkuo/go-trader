import builtins
import contextlib
import importlib.util
import json
import os
import sys
import traceback
import types
from importlib.machinery import SourceFileLoader

import egress_policy

_BOOTSTRAP_FILE = os.path.realpath(__file__)
_REPO_ROOT = os.path.dirname(os.path.dirname(_BOOTSTRAP_FILE))


def _refuse(reason):
    message = egress_policy.redact(f"egress policy: {reason}")
    try:
        sys.stderr.write(f"go-trader {message}\n")
        sys.stderr.flush()
    except (OSError, ValueError):
        pass
    sys.stdout.write(json.dumps({"error": message}) + "\n")
    sys.stdout.flush()
    return 1


def _print_redacted_traceback(exc_type, exc, tb):
    while tb is not None and os.path.realpath(tb.tb_frame.f_code.co_filename) == _BOOTSTRAP_FILE:
        tb = tb.tb_next
    text = "".join(traceback.format_exception(exc_type, exc, tb))
    try:
        sys.stderr.write(egress_policy.redact(text))
        sys.stderr.flush()
    except (OSError, ValueError):
        pass


def _run_target(script, args):
    path = os.path.abspath(script)
    try:
        with open(path, "rb") as handle:
            source = handle.read()
    except OSError as exc:
        sys.stderr.write(f"{sys.executable}: can't open file {path!r}: [Errno {exc.errno}] {exc.strerror}\n")
        return 2
    code = compile(source, path, "exec", dont_inherit=True)
    module = types.ModuleType("__main__")
    module.__dict__.update({
        "__file__": path,
        "__cached__": None,
        "__loader__": SourceFileLoader("__main__", path),
        "__spec__": None,
        "__package__": None,
        "__builtins__": builtins,
    })
    sys.argv = [script, *args]
    sys.path[0] = os.path.dirname(os.path.realpath(script))
    sys.modules["__main__"] = module
    try:
        exec(code, module.__dict__)
    except (SystemExit, KeyboardInterrupt):
        raise
    except BaseException:
        _print_redacted_traceback(*sys.exc_info())
        return 1
    return 0


class _CheckFailure(Exception):
    pass


def _load_repo_module(name, relative):
    path = os.path.join(_REPO_ROOT, relative)
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def _expect(condition, reason):
    if not condition:
        raise _CheckFailure(reason)


def _expect_session(session, what):
    expected = egress_policy.requests_proxies()
    _expect(session is not None, f"{what} has no session")
    _expect(session.trust_env is False, f"{what} session still trusts the environment")
    _expect(dict(session.proxies) == expected, f"{what} session proxies differ from the frozen policy")


def _expect_ccxt(exchange, what):
    current = egress_policy.policy()
    want_socks = current.url if current.is_socks else None
    want_https = None if current.is_socks else current.url
    _expect(getattr(exchange, "socksProxy", None) == want_socks, f"{what} socksProxy differs from the frozen policy")
    _expect(getattr(exchange, "httpsProxy", None) == want_https, f"{what} httpsProxy differs from the frozen policy")
    for attr in ("proxy", "proxyUrl", "proxy_url", "httpProxy", "http_proxy", "proxies",
                 "proxyUrlCallback", "httpProxyCallback", "httpsProxyCallback", "socksProxyCallback"):
        _expect(getattr(exchange, attr, None) is None, f"{what} has a conflicting {attr} setting")
    http_proxy, https_proxy, socks_proxy = exchange.check_proxy_settings("https://example.invalid/")
    _expect(http_proxy is None and https_proxy == want_https and socks_proxy == want_socks,
            f"{what} resolves a different proxy than the frozen policy")
    _expect_session(exchange.session, what)


def _check_ccxt():
    import ccxt

    _expect_ccxt(egress_policy.pin_ccxt(ccxt.binanceus({"enableRateLimit": True})), "ccxt binanceus")
    _expect_ccxt(ccxt.okx({"enableRateLimit": True}), "ccxt okx constructed without an explicit pin")


def _check_hyperliquid():
    _load_repo_module("_egress_check_hyperliquid_adapter", os.path.join("platforms", "hyperliquid", "adapter.py"))
    from hyperliquid.api import API
    from hyperliquid.exchange import Exchange
    from hyperliquid.info import Info

    meta = {"universe": []}
    spot_meta = {"universe": [], "tokens": []}
    _expect_session(API(base_url="https://api.hyperliquid.xyz").session, "hyperliquid base API")
    info = Info("https://api.hyperliquid.xyz", True, meta, spot_meta)
    _expect_session(info.session, "hyperliquid Info")
    import eth_account

    wallet = eth_account.Account.from_key("0x" + "11" * 32)
    exchange = Exchange(wallet, "https://api.hyperliquid.xyz", meta=meta, spot_meta=spot_meta)
    _expect_session(exchange.session, "hyperliquid Exchange")
    _expect_session(exchange.info.session, "hyperliquid Exchange Info")


def _check_robinhood():
    adapter = _load_repo_module("_egress_check_robinhood_adapter", os.path.join("platforms", "robinhood", "adapter.py"))
    with adapter.robinhood_library() as rh:
        session = rh.helper.SESSION
    _expect_session(session, "robin_stocks module")


def _check_topstep():
    adapter = _load_repo_module("_egress_check_topstep_adapter", os.path.join("platforms", "topstep", "adapter.py"))
    for key in ("TOPSTEP_API_KEY", "TOPSTEP_API_SECRET", "TOPSTEP_ACCOUNT_ID"):
        os.environ.setdefault(key, "egress-check")
    _expect_session(adapter.TopStepExchangeAdapter(mode="live")._session, "topstep")


def _check_yfinance():
    egress_policy.pin_yfinance()
    from yfinance.config import YfConfig
    from yfinance.data import YfData

    url = egress_policy.policy().url
    _expect(YfConfig.network.proxy == url, "yfinance network proxy differs from the frozen policy")
    proxies = dict(YfData()._session.proxies or {})
    _expect(proxies == {"http": url, "https": url}, "yfinance session proxies differ from the frozen policy")


def _check_requests():
    import requests
    from requests.utils import get_environ_proxies

    expected = egress_policy.proxy_env()
    for key, value in expected.items():
        _expect(os.environ.get(key) == value, f"environment variable {key} differs from the frozen policy")
    url = egress_policy.policy().url
    for target in ("https://api.hyperliquid.xyz/info", "http://api.hyperliquid.xyz/info"):
        scheme = target.split(":", 1)[0]
        _expect(get_environ_proxies(target).get(scheme) == url, "environment proxies differ from the frozen policy")
    _expect_session(egress_policy.pin_requests_session(requests.Session()), "requests")


def _check_deribit():
    utils = _load_repo_module("_egress_check_deribit_utils", os.path.join("platforms", "deribit", "utils.py"))
    _expect(utils.egress_policy.requests_proxies() == egress_policy.requests_proxies(),
            "deribit helper proxies differ from the frozen policy")
    adapter = _load_repo_module("_egress_check_deribit_adapter", os.path.join("platforms", "deribit", "adapter.py"))
    _expect_ccxt(adapter.DeribitOptionsAdapter(sandbox=False).exchange, "ccxt deribit")


def _check_advisory():
    review = _load_repo_module("_egress_check_llm_review", os.path.join("shared_scripts", "llm_review.py"))
    _expect_session(review.new_session(), "advisory")


_CHECKS = {
    "ccxt": _check_ccxt,
    "hyperliquid": _check_hyperliquid,
    "robinhood": _check_robinhood,
    "topstep": _check_topstep,
    "yfinance": _check_yfinance,
    "requests": _check_requests,
    "deribit": _check_deribit,
    "advisory": _check_advisory,
}


def _check_result(payload, out):
    out.write(json.dumps(payload) + "\n")
    out.flush()


def _check_policy(families):
    out = sys.stdout
    if not families:
        sys.stderr.write("usage: egress_bootstrap.py --check-policy <family> [<family>...]\n")
        return 2
    unknown = [family for family in families if family not in _CHECKS]
    if unknown:
        _check_result({"ok": False, "error": "unknown family", "family": unknown[0]}, out)
        return 2
    ordered = list(dict.fromkeys(families))
    try:
        if not egress_policy.active():
            raise egress_policy.EgressPolicyError("egress proxy mode is not active")
        egress_policy.policy()
        egress_policy.install()
    except egress_policy.EgressPolicyError as exc:
        _check_result({"ok": False, "error": egress_policy.redact(f"egress policy: {exc}"), "family": "policy"}, out)
        return 1
    for family in ordered:
        try:
            with contextlib.redirect_stdout(sys.stderr):
                _CHECKS[family]()
        except Exception as exc:
            reason = str(exc) if isinstance(exc, _CheckFailure) else f"{type(exc).__name__}: {exc}"
            _check_result({"ok": False, "error": egress_policy.redact(reason), "family": family}, out)
            return 1
    _check_result({"ok": True, "families": ordered}, out)
    return 0


def main():
    if len(sys.argv) < 2:
        sys.stderr.write("usage: egress_bootstrap.py <script> [args...]\n")
        return 2
    if sys.argv[1] == "--check-policy":
        return _check_policy(sys.argv[2:])
    script, args = sys.argv[1], sys.argv[2:]
    if egress_policy.active():
        try:
            egress_policy.policy()
            egress_policy.install()
        except egress_policy.EgressPolicyError as exc:
            return _refuse(str(exc))
    return _run_target(script, args)


if __name__ == "__main__":
    sys.exit(main())
