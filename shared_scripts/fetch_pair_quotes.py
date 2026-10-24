#!/usr/bin/env python3
import argparse
import importlib.util
import json
import os
import sys
import time

ROOT = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
REFUSED_ENV = ("HYPERLIQUID_SECRET_KEY", "HYPERLIQUID_ACCOUNT_ADDRESS", "OKX_API_KEY", "OKX_API_SECRET", "OKX_PASSPHRASE",
               "HYPERLIQUID_TESTNET", "OKX_SANDBOX")
ADAPTERS = {
    "hyperliquid": ("go_trader_pair_quotes_hyperliquid_adapter", os.path.join(ROOT, "platforms", "hyperliquid", "adapter.py")),
    "okx": ("go_trader_pair_quotes_okx_adapter", os.path.join(ROOT, "platforms", "okx", "adapter.py")),
}
SOURCES = {"hyperliquid": ("public_snapshot", "archive"), "okx": ("public_snapshot",)}
CONTRACTS = {("hyperliquid", "public_snapshot"): "hyperliquid_public_l2_snapshot_v1", ("okx", "public_snapshot"): "okx_public_books_snapshot_v1",
             ("hyperliquid", "archive"): "hyperliquid_archive_l2_v1"}
HOSTS = {"hyperliquid": "https://api.hyperliquid.xyz", "okx": "https://www.okx.com"}
ARCHIVE_FLAGS = ("window_ms", "capacity", "archive_dir", "cutoff_ms", "as_of_ms")


class _ArgumentError(Exception):
    pass


class _Parser(argparse.ArgumentParser):
    def error(self, message):
        raise _ArgumentError(message)


def _emit(payload, code):
    sys.stdout.write(json.dumps(payload, sort_keys=True, separators=(",", ":")) + "\n")
    sys.stdout.flush()
    return code


def _redact(text):
    policy = sys.modules.get("egress_policy")
    if policy is not None:
        try:
            return policy.redact(text)
        except Exception:
            return "public quote read failed (detail withheld)"
    return text


def _load_module(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    sys.modules[name] = module
    spec.loader.exec_module(module)
    return module


def _now_ms():
    return int(time.time() * 1000)


def _public(provider, instrument, depth):
    name, path = ADAPTERS[provider]
    module = _load_module(name, path)
    if provider == "hyperliquid":
        adapter = module.HyperliquidExchangeAdapter(public_only=True)
        host = adapter.public_host()
        request_ms = _now_ms()
        payload = adapter.public_l2_snapshot(instrument, depth)
    else:
        adapter = module.OKXExchangeAdapter(public_only=True)
        host = adapter.public_host()
        request_ms = _now_ms()
        payload = adapter.public_order_book(instrument, depth)
    receipt_ms = _now_ms()
    if host != HOSTS[provider]:
        raise RuntimeError(f"the {provider} public host {host!r} is not the mainnet host {HOSTS[provider]!r}")
    return {"ok": True, "provider": provider, "source": "public_snapshot", "instrument": instrument, "host": host,
            "provenance": CONTRACTS[(provider, "public_snapshot")] + "@" + host, "request_ms": request_ms, "receipt_ms": receipt_ms,
            "payload": payload}


def _archive(args):
    backtest = os.path.join(ROOT, "backtest")
    if backtest not in sys.path:
        sys.path.insert(0, backtest)
    module = _load_module("go_trader_pair_archive_book", os.path.join(backtest, "pair_archive_book.py"))
    need = {"depth": args.depth, "window_ms": args.window_ms, "capacity": args.capacity}
    payload = module.archive_payload(args.archive_dir, args.instrument, args.cutoff_ms, args.as_of_ms, need)
    return {"ok": True, "provider": args.provider, "source": "archive", "instrument": args.instrument, "payload": payload}


def _validate(args):
    if not args.instrument.strip() or args.instrument.strip() != args.instrument:
        raise _ArgumentError("--instrument must be a nonempty trimmed native instrument")
    if args.source not in SOURCES[args.provider]:
        raise _ArgumentError(f"--source={args.source} is not a {args.provider} quote source")
    if not 1 <= args.depth <= 20:
        raise _ArgumentError("--depth must be in [1, 20]")
    present = [f for f in ARCHIVE_FLAGS if getattr(args, f) is not None]
    if args.source == "archive":
        missing = [f for f in ARCHIVE_FLAGS if getattr(args, f) is None]
        if missing:
            raise _ArgumentError("the archive source needs --" + ", --".join(m.replace("_", "-") for m in missing))
        if not os.path.isabs(args.archive_dir):
            raise _ArgumentError("--archive-dir must be absolute")
        if not 1 <= args.window_ms <= 900_000 or not 1 <= args.capacity <= 4096:
            raise _ArgumentError("--window-ms must be in [1, 900000] and --capacity in [1, 4096]")
        if args.cutoff_ms < 1 or args.as_of_ms < 1:
            raise _ArgumentError("--cutoff-ms and --as-of-ms must be positive")
    elif present:
        raise _ArgumentError("a public snapshot takes no archive flag: --" + ", --".join(p.replace("_", "-") for p in present))


def main(argv):
    parser = _Parser(prog="fetch_pair_quotes.py", add_help=False)
    parser.add_argument("--provider", required=True, choices=sorted(ADAPTERS))
    parser.add_argument("--source", required=True, choices=["archive", "public_snapshot"])
    parser.add_argument("--instrument", required=True)
    parser.add_argument("--depth", required=True, type=int)
    parser.add_argument("--window-ms", dest="window_ms", type=int)
    parser.add_argument("--capacity", type=int)
    parser.add_argument("--archive-dir", dest="archive_dir")
    parser.add_argument("--cutoff-ms", dest="cutoff_ms", type=int)
    parser.add_argument("--as-of-ms", dest="as_of_ms", type=int)
    parser.add_argument("--probe-only", action="store_true")
    try:
        args = parser.parse_args(argv)
        _validate(args)
    except _ArgumentError as exc:
        return _emit({"ok": False, "error": f"invalid arguments: {exc}"}, 1)
    if args.probe_only:
        return _emit({"ok": True, "probe": True}, 0)
    set_env = [k for k in REFUSED_ENV if os.environ.get(k, "") != ""]
    if set_env:
        return _emit({"ok": False, "error": "refused: the quote reader runs public-only and requires these variables to be empty: " + ", ".join(set_env)}, 1)
    sys.dont_write_bytecode = True
    try:
        if args.source == "archive":
            result = _archive(args)
        else:
            result = _public(args.provider, args.instrument, args.depth)
    except Exception as exc:
        return _emit({"ok": False, "error": _redact(f"{type(exc).__name__}: {exc}")}, 1)
    return _emit(result, 0)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
