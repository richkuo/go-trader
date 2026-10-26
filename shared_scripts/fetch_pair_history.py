#!/usr/bin/env python3
import argparse
import importlib.util
import json
import os
import re
import sys
import time

ROOT = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
REFUSED_ENV = ("HYPERLIQUID_SECRET_KEY", "HYPERLIQUID_ACCOUNT_ADDRESS", "OKX_API_KEY", "OKX_API_SECRET", "OKX_PASSPHRASE",
               "HYPERLIQUID_TESTNET", "OKX_SANDBOX")
ADAPTERS = {
    "hyperliquid": ("go_trader_pair_history_hyperliquid_adapter", os.path.join(ROOT, "platforms", "hyperliquid", "adapter.py")),
    "okx": ("go_trader_pair_history_okx_adapter", os.path.join(ROOT, "platforms", "okx", "adapter.py")),
}
HOSTS = {"hyperliquid": "https://api.hyperliquid.xyz", "okx": "https://www.okx.com"}
TIMEFRAMES = ("1m", "3m", "5m", "15m", "30m", "1h", "2h", "4h", "8h", "12h", "1d")
HEX64 = re.compile(r"^[0-9a-f]{64}$")
OKX_SWAP = re.compile(r"^[A-Z0-9]+-USDT-SWAP$")
HL_FUNDING_PROVENANCE = "hyperliquid_info_fundingHistory_v1"
OKX_FUNDING_PROVENANCE = "okx_public_funding_rate_history_v1"
OKX_MARK_PROVENANCE = "okx_mark_price_candle_1m_open_v1"


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
            return "public history read failed (detail withheld)"
    return text


def _load_module(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    sys.modules[name] = module
    spec.loader.exec_module(module)
    return module


def _now_ms():
    return int(time.time() * 1000)


def _adapter(provider):
    name, path = ADAPTERS[provider]
    module = _load_module(name, path)
    if provider == "hyperliquid":
        adapter = module.HyperliquidExchangeAdapter(public_only=True)
    else:
        adapter = module.OKXExchangeAdapter(public_only=True)
    host = adapter.public_host()
    if host != HOSTS[provider]:
        raise RuntimeError(f"the {provider} public host {host!r} is not the mainnet host {HOSTS[provider]!r}")
    return adapter, host


def _candles(args):
    adapter, host = _adapter(args.provider)
    request_ms = _now_ms()
    if args.provider == "hyperliquid":
        out = adapter.public_candles(args.instrument, args.timeframe, args.limit, request_ms)
    else:
        out = adapter.public_candles(args.instrument, args.timeframe, args.limit)
    receipt_ms = _now_ms()
    bars = [b for b in out["bars"] if b[0] + _interval_ms(args.timeframe) <= receipt_ms]
    forming = out["forming"] if out["forming"] and out["forming"][0] <= receipt_ms else []
    return {"ok": True, "kind": "candles", "provider": args.provider, "instrument": args.instrument, "timeframe": args.timeframe, "host": host,
            "bars": bars, "forming": forming, "request_ms": request_ms, "receipt_ms": receipt_ms}


def _interval_ms(tf):
    unit = tf[-1]
    n = int(tf[:-1])
    return n * {"m": 60_000, "h": 3_600_000, "d": 86_400_000}[unit]


def _funding(args):
    sys.path.insert(0, os.path.join(ROOT, "shared_tools"))
    funding = _load_module("go_trader_pair_history_funding", os.path.join(ROOT, "shared_tools", "pair_funding.py"))
    key = json.loads(args.member_key_json)
    if not isinstance(key, dict) or key.get("provider") != args.provider or key.get("instrument") != args.instrument:
        raise _ArgumentError("--member-key-json must name the same provider and instrument")
    adapter, host = _adapter(args.provider)
    request_ms = _now_ms()
    prices = []
    if args.provider == "hyperliquid":
        window = adapter.public_funding_window(args.instrument, args.from_ms + 1, args.through_ms)
        receipt_ms = _now_ms()
        evidence_id = f"hyperliquid-funding:{args.instrument}:{args.from_ms}:{args.through_ms}:{receipt_ms}"
        rates, coverage = funding.hyperliquid_window_records(key, window["records"], args.from_ms, args.through_ms, receipt_ms,
                                                             HL_FUNDING_PROVENANCE + "@" + host, evidence_id)
    else:
        result = adapter.get_funding_history_range(args.instrument, args.from_ms, args.through_ms)
        if result.get("error"):
            raise RuntimeError(f"OKX funding range read stopped: {result.get('stop_reason')}: {result['error']}")
        marks = {}
        for ev in result["events"]:
            marks[ev["event_ms"]] = adapter.public_mark_price_at(args.instrument, ev["event_ms"])
        receipt_ms = _now_ms()
        evidence_id = f"okx-funding:{args.instrument}:{args.from_ms}:{args.through_ms}:{receipt_ms}"
        rates, coverage = funding.okx_range_records(key, result, receipt_ms, OKX_FUNDING_PROVENANCE + "@" + host, evidence_id)
        for t in sorted(marks):
            if marks[t] is not None:
                prices.append(funding.mark_price_record(key, t, marks[t], receipt_ms, OKX_MARK_PROVENANCE + "@" + host, f"okx-mark:{args.instrument}:{t}"))
    batch = {"v": funding.BATCH_VERSION, "source_id": args.source_id, "seq": args.seq, "prev_hash": args.prev_hash, "rates": rates,
             "prices": prices, "coverage": [coverage], "batch_hash": ""}
    sealed = funding.seal_batch(batch)
    return {"ok": True, "kind": "funding", "provider": args.provider, "instrument": args.instrument, "host": host,
            "batch": funding.batch_bytes(sealed).decode("utf-8"), "request_ms": request_ms, "receipt_ms": receipt_ms}


def _validate(args):
    if not args.instrument.strip() or args.instrument.strip() != args.instrument:
        raise _ArgumentError("--instrument must be a nonempty trimmed native instrument")
    if args.provider == "okx" and not OKX_SWAP.match(args.instrument):
        raise _ArgumentError("an OKX instrument must be <UNDERLYING>-USDT-SWAP")
    candle_flags = [f for f in ("timeframe", "limit") if getattr(args, f) is not None]
    funding_flags = [f for f in ("from_ms", "through_ms", "member_key_json", "source_id", "seq", "prev_hash") if getattr(args, f) is not None]
    if args.kind == "candles":
        if funding_flags:
            raise _ArgumentError("a candle read takes no funding flag: --" + ", --".join(f.replace("_", "-") for f in funding_flags))
        if args.timeframe not in TIMEFRAMES:
            raise _ArgumentError(f"--timeframe must be one of {', '.join(TIMEFRAMES)}")
        top = 5000 if args.provider == "hyperliquid" else 300
        if args.limit is None or not 1 <= args.limit <= top:
            raise _ArgumentError(f"--limit must be in [1, {top}]")
        return
    if candle_flags:
        raise _ArgumentError("a funding read takes no candle flag: --" + ", --".join(f.replace("_", "-") for f in candle_flags))
    missing = [f for f in ("from_ms", "through_ms", "member_key_json", "source_id", "seq", "prev_hash") if getattr(args, f) is None]
    if missing:
        raise _ArgumentError("a funding read needs --" + ", --".join(f.replace("_", "-") for f in missing))
    if args.from_ms < 0 or args.through_ms <= args.from_ms:
        raise _ArgumentError("--from-ms must be at least 0 and before --through-ms")
    if args.seq < 1 or (args.seq == 1) != (args.prev_hash == "") or (args.prev_hash and not HEX64.match(args.prev_hash)):
        raise _ArgumentError("--seq starts at 1 with an empty --prev-hash, and a later batch names the previous batch hash")
    if not args.source_id or len(args.source_id) > 128 or any(not 0x20 <= ord(c) <= 0x7E for c in args.source_id):
        raise _ArgumentError("--source-id must be 1..128 printable ASCII characters")
    try:
        key = json.loads(args.member_key_json)
    except ValueError as exc:
        raise _ArgumentError(f"--member-key-json is not JSON: {exc}")
    if not isinstance(key, dict) or sorted(key) != ["account_key", "instrument", "member_id", "partition", "provider"]:
        raise _ArgumentError("--member-key-json must hold exactly partition, provider, account_key, instrument and member_id")


def main(argv):
    parser = _Parser(prog="fetch_pair_history.py", add_help=False)
    parser.add_argument("--kind", required=True, choices=["candles", "funding"])
    parser.add_argument("--provider", required=True, choices=sorted(ADAPTERS))
    parser.add_argument("--instrument", required=True)
    parser.add_argument("--timeframe")
    parser.add_argument("--limit", type=int)
    parser.add_argument("--from-ms", dest="from_ms", type=int)
    parser.add_argument("--through-ms", dest="through_ms", type=int)
    parser.add_argument("--member-key-json", dest="member_key_json")
    parser.add_argument("--source-id", dest="source_id")
    parser.add_argument("--seq", type=int)
    parser.add_argument("--prev-hash", dest="prev_hash")
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
        return _emit({"ok": False, "error": "refused: the history reader runs public-only and requires these variables to be empty: " + ", ".join(set_env)}, 1)
    sys.dont_write_bytecode = True
    try:
        result = _candles(args) if args.kind == "candles" else _funding(args)
    except _ArgumentError as exc:
        return _emit({"ok": False, "error": f"invalid arguments: {exc}"}, 1)
    except Exception as exc:
        return _emit({"ok": False, "error": _redact(f"{type(exc).__name__}: {exc}")}, 1)
    return _emit(result, 0)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
