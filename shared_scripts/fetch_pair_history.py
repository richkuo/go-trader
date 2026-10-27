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


def _oracle_now_ms():
    return time.time_ns() // 1000000


def _funding_module():
    sys.path.insert(0, os.path.join(ROOT, "shared_tools"))
    return _load_module("go_trader_pair_history_funding", os.path.join(ROOT, "shared_tools", "pair_funding.py"))


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
    funding = _funding_module()
    key = funding.loads_strict(args.member_key_json.encode("utf-8"))
    adapter, host = _adapter(args.provider)
    request_ms = _now_ms()
    prices = []
    rule_version = args.rule_version
    if args.provider == "hyperliquid":
        window = adapter.public_funding_window(args.instrument, args.from_ms + 1, args.through_ms)
        receipt_ms = _now_ms()
        evidence_id = f"hyperliquid-funding:{args.instrument}:{args.from_ms}:{args.through_ms}:{receipt_ms}"
        rates, coverage = funding.hyperliquid_window_records(key, window["records"], args.from_ms, args.through_ms, receipt_ms,
                                                             HL_FUNDING_PROVENANCE + "@" + host, evidence_id,
                                                             rule_version=rule_version or funding.RULE_HYPERLIQUID)
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
    if args.funding_policy_json is not None:
        batch = {"v": funding.RESEARCH_BATCH_VERSION, "source_id": args.source_id, "seq": args.seq, "prev_hash": args.prev_hash,
                 "policy_digest": args.policy_digest, "rates": rates, "prices": prices, "observations": [], "coverage": [coverage], "batch_hash": ""}
    sealed = funding.seal_batch(batch)
    return {"ok": True, "kind": "funding", "provider": args.provider, "instrument": args.instrument, "host": host,
            "batch": funding.batch_bytes(sealed).decode("utf-8"), "request_ms": request_ms, "receipt_ms": receipt_ms}


def _funding_oracle(args):
    funding = _funding_module()
    policy = funding.validate_policy(funding.loads_strict(args.funding_policy_json.encode("utf-8")))
    key = funding.loads_strict(args.member_key_json.encode("utf-8"))
    adapter, host = _adapter(args.provider)
    previous = _oracle_now_ms()
    while previous < args.not_before_ms:
        if previous > args.deadline_ms:
            raise RuntimeError("the oracle preparation ended after its deadline")
        time.sleep(min(0.05, (args.not_before_ms - previous) / 1000))
        current = _oracle_now_ms()
        if current < previous:
            raise RuntimeError("the Python clock moved backward during oracle preparation")
        previous = current
    if previous > args.deadline_ms:
        raise RuntimeError("the oracle preparation ended after its deadline")
    request_ms = _oracle_now_ms()
    if request_ms < previous:
        raise RuntimeError("the Python clock moved backward before the oracle request")
    price = adapter.public_oracle_observation(args.instrument)
    receipt_ms = _oracle_now_ms()
    if receipt_ms < request_ms:
        raise RuntimeError("the Python clock moved backward during the oracle request")
    deadline = min(args.target_event_ms + policy["window_ms"], policy["drain_through_ms"])
    if request_ms < args.target_event_ms or receipt_ms > deadline:
        raise RuntimeError(f"the oracle request/receipt times are outside [{args.target_event_ms}, {deadline}]")
    observation = {"v": funding.OBSERVATION_VERSION, "key": key, "evidence_id": funding.oracle_evidence_id(key, args.target_event_ms, receipt_ms),
                   "attempt_id": funding.oracle_attempt_id(key, args.target_event_ms, args.policy_digest), "policy_digest": args.policy_digest,
                   "price_kind": funding.PRICE_ORACLE_RECEIPT, "target_event_ms": args.target_event_ms, "request_ms": request_ms,
                   "receipt_ms": receipt_ms, "at_ms": receipt_ms, "visible_ms": receipt_ms, "time_basis": funding.OBSERVATION_TIME_BASIS,
                   "price": price, "provenance": funding.ORACLE_PROVENANCE, "record_hash": ""}
    sealed = funding.seal(observation, "observation")
    funding.validate_observation(sealed, policy)
    return {"ok": True, "kind": "funding_oracle", "provider": args.provider, "instrument": args.instrument, "host": host,
            "observation": sealed, "request_ms": request_ms, "receipt_ms": receipt_ms}


def _validate(args):
    if not args.instrument.strip() or args.instrument.strip() != args.instrument:
        raise _ArgumentError("--instrument must be a nonempty trimmed native instrument")
    if args.provider == "okx" and not OKX_SWAP.match(args.instrument):
        raise _ArgumentError("an OKX instrument must be <UNDERLYING>-USDT-SWAP")
    candle_flags = [f for f in ("timeframe", "limit") if getattr(args, f) is not None]
    rate_flags = [f for f in ("from_ms", "through_ms", "source_id", "seq", "prev_hash") if getattr(args, f) is not None]
    policy_flags = [f for f in ("funding_policy_json", "policy_digest", "rule_version") if getattr(args, f) is not None]
    oracle_flags = [f for f in ("target_event_ms", "not_before_ms", "deadline_ms") if getattr(args, f) is not None]
    if args.kind == "candles":
        refused = rate_flags + policy_flags + oracle_flags + (["member_key_json"] if args.member_key_json is not None else [])
        if refused:
            raise _ArgumentError("a candle read takes no funding flag: --" + ", --".join(f.replace("_", "-") for f in refused))
        if args.timeframe not in TIMEFRAMES:
            raise _ArgumentError(f"--timeframe must be one of {', '.join(TIMEFRAMES)}")
        top = 5000 if args.provider == "hyperliquid" else 300
        if args.limit is None or not 1 <= args.limit <= top:
            raise _ArgumentError(f"--limit must be in [1, {top}]")
        return
    if candle_flags:
        raise _ArgumentError("a funding read takes no candle flag: --" + ", --".join(f.replace("_", "-") for f in candle_flags))
    if args.kind == "funding_oracle":
        if args.provider != "hyperliquid":
            raise _ArgumentError("an oracle read requires provider hyperliquid")
        refused = rate_flags + (["rule_version"] if args.rule_version is not None else [])
        if refused:
            raise _ArgumentError("an oracle read takes no rate-batch flag: --" + ", --".join(f.replace("_", "-") for f in refused))
        missing = [f for f in ("target_event_ms", "not_before_ms", "deadline_ms", "funding_policy_json", "policy_digest", "member_key_json") if getattr(args, f) is None]
        if missing:
            raise _ArgumentError("an oracle read needs --" + ", --".join(f.replace("_", "-") for f in missing))
    else:
        if oracle_flags:
            raise _ArgumentError("a rate read takes no oracle flag: --" + ", --".join(f.replace("_", "-") for f in oracle_flags))
        missing = [f for f in ("from_ms", "through_ms", "member_key_json", "source_id", "seq", "prev_hash") if getattr(args, f) is None]
        if missing:
            raise _ArgumentError("a funding read needs --" + ", --".join(f.replace("_", "-") for f in missing))
        if args.from_ms < 0 or args.through_ms <= args.from_ms:
            raise _ArgumentError("--from-ms must be at least 0 and before --through-ms")
        if args.seq < 1 or (args.seq == 1) != (args.prev_hash == "") or (args.prev_hash and not HEX64.match(args.prev_hash)):
            raise _ArgumentError("--seq starts at 1 with an empty --prev-hash, and a later batch names the previous batch hash")
        if not args.source_id or len(args.source_id) > 128 or any(not 0x20 <= ord(c) <= 0x7E for c in args.source_id):
            raise _ArgumentError("--source-id must be 1..128 printable ASCII characters")
        if policy_flags and len(policy_flags) != 3:
            raise _ArgumentError("policy funding needs --funding-policy-json, --policy-digest and --rule-version together")
    try:
        funding = _funding_module()
        key = funding.loads_strict(args.member_key_json.encode("utf-8"))
        funding.validate_key(key)
        if key["provider"] != args.provider or key["instrument"] != args.instrument:
            raise _ArgumentError("--member-key-json must name the same provider and instrument")
        if args.funding_policy_json is not None:
            policy = funding.validate_policy(funding.loads_strict(args.funding_policy_json.encode("utf-8")))
            if args.policy_digest != funding.policy_digest(policy):
                raise _ArgumentError("--policy-digest does not match the canonical policy")
            selected = policy["member_rules"].get(key["member_id"])
            expected = args.rule_version if args.kind == "funding" else funding.RULE_HL_RECEIPT_RESEARCH
            if selected != expected:
                raise _ArgumentError("the member key or rule version is not selected by the funding policy")
            if args.kind == "funding_oracle":
                if args.target_event_ms != args.not_before_ms or funding.research_target(args.target_event_ms, policy) != args.target_event_ms:
                    raise _ArgumentError("the oracle target must equal --not-before-ms and be a policy-eligible UTC hour")
                deadline = min(args.target_event_ms + policy["window_ms"], policy["drain_through_ms"])
                if args.deadline_ms != deadline:
                    raise _ArgumentError(f"--deadline-ms must be {deadline}")
    except _ArgumentError:
        raise
    except Exception as exc:
        raise _ArgumentError(str(exc))


def main(argv):
    parser = _Parser(prog="fetch_pair_history.py", add_help=False)
    parser.add_argument("--kind", required=True, choices=["candles", "funding", "funding_oracle"])
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
    parser.add_argument("--target-event-ms", dest="target_event_ms", type=int)
    parser.add_argument("--not-before-ms", dest="not_before_ms", type=int)
    parser.add_argument("--deadline-ms", dest="deadline_ms", type=int)
    parser.add_argument("--funding-policy-json", dest="funding_policy_json")
    parser.add_argument("--policy-digest", dest="policy_digest")
    parser.add_argument("--rule-version", dest="rule_version")
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
        if args.kind == "candles":
            result = _candles(args)
        elif args.kind == "funding_oracle":
            result = _funding_oracle(args)
        else:
            result = _funding(args)
    except _ArgumentError as exc:
        return _emit({"ok": False, "error": f"invalid arguments: {exc}"}, 1)
    except Exception as exc:
        return _emit({"ok": False, "error": _redact(f"{type(exc).__name__}: {exc}")}, 1)
    return _emit(result, 0)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
