#!/usr/bin/env python3

import argparse
import hashlib
import json
import os
import sys
import time
import urllib.error
import urllib.request

INFO_URL = "https://api.hyperliquid.xyz/info"
SCHEMA = "go-trader.resting-tp-capture"
SCHEMA_VERSION = 1
ALLOWED_TYPES = frozenset({
    "userFillsByTime",
    "orderStatus",
    "historicalOrders",
    "recentTrades",
    "candleSnapshot",
})
PAGE_CAP = 200
DEFAULT_TIMEOUT_S = 20.0
DEFAULT_RETRIES = 3


class CaptureError(Exception):
    pass


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def sha256_file(path: str) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as fh:
        for chunk in iter(lambda: fh.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def canonical(obj) -> str:
    return json.dumps(obj, sort_keys=True, separators=(",", ":"), ensure_ascii=True)


def post_info(payload, timeout, retries, opener=None):
    if not isinstance(payload, dict) or payload.get("type") not in ALLOWED_TYPES:
        raise CaptureError(
            f"request type {payload.get('type') if isinstance(payload, dict) else None!r} "
            "is not allowlisted"
        )
    if timeout <= 0:
        raise CaptureError("timeout must be positive")
    if retries < 1:
        raise CaptureError("retries must be at least 1")
    body = canonical(payload).encode("utf-8")
    open_fn = opener if opener is not None else urllib.request.urlopen
    last = None
    for attempt in range(1, retries + 1):
        req = urllib.request.Request(
            INFO_URL, data=body, headers={"Content-Type": "application/json"})
        try:
            with open_fn(req, timeout=timeout) as resp:
                status = getattr(resp, "status", None) or resp.getcode()
                raw = resp.read()
            return attempt, int(status), raw
        except urllib.error.HTTPError as exc:
            raw = exc.read() if exc.fp is not None else b""
            last = (attempt, int(exc.code), raw, str(exc))
            if attempt == retries:
                return attempt, int(exc.code), raw
        except Exception as exc:
            last = (attempt, 0, b"", str(exc))
            if attempt == retries:
                raise CaptureError(
                    f"{payload['type']} failed after {retries} attempts: {exc}") from exc
            time.sleep(min(2 * attempt, 8))
    raise CaptureError(f"{payload['type']} failed: {last}")


def _evidence_value(node):
    if isinstance(node, dict) and node.get("status") == "available":
        return node.get("value")
    return None


def read_export_targets(path: str):
    with open(path) as fh:
        doc = json.load(fh)
    if not isinstance(doc, dict) or doc.get("schema") != "go-trader.booked-ledger":
        raise CaptureError(f"{path}: schema must be go-trader.booked-ledger")
    oids = []
    coins = []
    seen_oid = set()
    seen_coin = set()
    for event in doc.get("events") or []:
        if not isinstance(event, dict):
            continue
        symbol = _evidence_value(event.get("symbol"))
        if isinstance(symbol, str) and symbol.strip():
            coin = symbol.split("/")[0].strip()
            if coin and coin not in seen_coin:
                seen_coin.add(coin)
                coins.append(coin)
        raw_tps = _evidence_value(event.get("tp_oids_json"))
        if isinstance(raw_tps, str) and raw_tps.strip():
            parsed = json.loads(raw_tps)
            if isinstance(parsed, list):
                for item in parsed:
                    oid = int(item)
                    if oid > 0 and oid not in seen_oid:
                        seen_oid.add(oid)
                        oids.append(oid)
        raw_stop = _evidence_value(event.get("stop_loss_oid"))
        if isinstance(raw_stop, str) and raw_stop.strip():
            oid = int(raw_stop)
            if oid > 0 and oid not in seen_oid:
                seen_oid.add(oid)
                oids.append(oid)
    return oids, coins


def _redact(payload):
    out = dict(payload)
    if "user" in out:
        out["user"] = "redacted"
    return out


def _prepare_out(path: str, repo_root: str):
    out = os.path.abspath(path)
    root = os.path.abspath(repo_root)
    if out == root or out.startswith(root + os.sep):
        raise CaptureError("out directory must be outside the repository")
    if os.path.exists(out):
        if not os.path.isdir(out):
            raise CaptureError("out path exists and is not a directory")
        if any(os.scandir(out)):
            raise CaptureError("out directory exists and is not empty")
    else:
        os.makedirs(out)
    os.makedirs(os.path.join(out, "responses"), exist_ok=True)
    return out


def _fill_time(row):
    if isinstance(row, dict) and row.get("time") is not None:
        return int(row["time"])
    return None


def _page_user_fills(address, since_ms, end_ms, timeout, retries, record):
    cursor = int(since_ms)
    pages = []
    while len(pages) < PAGE_CAP:
        payload = {
            "type": "userFillsByTime",
            "user": address,
            "startTime": cursor,
            "endTime": int(end_ms),
        }
        attempt, status, raw = post_info(payload, timeout, retries)
        parsed = json.loads(raw.decode("utf-8")) if raw else None
        record(payload, attempt, status, raw)
        if status != 200 or not isinstance(parsed, list):
            return {
                "complete": False,
                "pages": len(pages) + 1,
                "observed_page_maximum": max((len(p) for p in pages), default=0),
                "uncovered_from_ms": cursor,
                "uncovered_to_ms": int(end_ms),
                "failure": f"status {status}",
            }
        pages.append(parsed)
        if not parsed:
            cursor = int(end_ms) + 1
            break
        times = [t for t in (_fill_time(row) for row in parsed) if t is not None]
        if not times:
            return {
                "complete": False,
                "pages": len(pages),
                "observed_page_maximum": max(len(p) for p in pages),
                "uncovered_from_ms": cursor,
                "uncovered_to_ms": int(end_ms),
                "failure": "page has no fill time",
            }
        nxt = max(times) + 1
        if nxt <= cursor:
            return {
                "complete": False,
                "pages": len(pages),
                "observed_page_maximum": max(len(p) for p in pages),
                "uncovered_from_ms": cursor,
                "uncovered_to_ms": int(end_ms),
                "failure": "fill cursor did not advance",
            }
        cursor = nxt
        if cursor > int(end_ms):
            break
    observed = max((len(p) for p in pages), default=0)
    prior = max((len(p) for p in pages[:-1]), default=None)
    shorter = prior is not None and len(pages[-1]) < prior
    single = len(pages) == 1
    hit_cap = len(pages) >= PAGE_CAP and cursor <= int(end_ms)
    complete = (not hit_cap) and cursor > int(end_ms) and (single or shorter)
    out = {
        "complete": complete,
        "pages": len(pages),
        "observed_page_maximum": observed,
        "uncovered_from_ms": None if complete else cursor,
        "uncovered_to_ms": None if complete else int(end_ms),
    }
    return out


def _one_shot(payload, timeout, retries, record):
    attempt, status, raw = post_info(payload, timeout, retries)
    record(payload, attempt, status, raw)
    ok = status == 200 and bool(raw)
    return {"complete": ok, "http_status": status}


def capture(exports, address, since_ms, end_ms, out_dir, timeout, retries):
    oids = []
    coins = []
    seen_oid = set()
    seen_coin = set()
    export_hashes = []
    for path in exports:
        digest = sha256_file(path)
        export_hashes.append({"path": os.path.basename(path), "sha256": digest})
        found_oids, found_coins = read_export_targets(path)
        for oid in found_oids:
            if oid not in seen_oid:
                seen_oid.add(oid)
                oids.append(oid)
        for coin in found_coins:
            if coin not in seen_coin:
                seen_coin.add(coin)
                coins.append(coin)
    oids.sort()
    coins = sorted(coins)
    responses_dir = os.path.join(out_dir, "responses")
    requests_path = os.path.join(out_dir, "requests.jsonl")
    n = 0
    failed = False

    def record(payload, attempt, status, raw):
        nonlocal n, failed
        n += 1
        digest = sha256_bytes(raw or b"")
        name = f"{n}.json"
        with open(os.path.join(responses_dir, name), "wb") as fh:
            fh.write(raw or b"")
        line = {
            "n": n,
            "type": payload.get("type"),
            "body": _redact(payload),
            "timeout_s": timeout,
            "attempt": attempt,
            "http_status": status,
            "response_sha256": digest,
            "response_file": f"responses/{name}",
        }
        if payload.get("type") == "userFillsByTime":
            line["startTime"] = payload.get("startTime")
        if "coin" in payload:
            line["coin"] = payload["coin"]
        req = payload.get("req")
        if isinstance(req, dict) and req.get("coin"):
            line["coin"] = req["coin"]
        with open(requests_path, "a") as fh:
            fh.write(canonical(line) + "\n")
        if status != 200:
            failed = True

    fills = _page_user_fills(address, since_ms, end_ms, timeout, retries, record)
    if not fills["complete"]:
        failed = True
    historical = _one_shot(
        {"type": "historicalOrders", "user": address}, timeout, retries, record)
    if not historical["complete"]:
        failed = True
    order_rows = []
    for oid in oids:
        row = _one_shot(
            {"type": "orderStatus", "user": address, "oid": oid},
            timeout, retries, record)
        row["oid"] = oid
        order_rows.append(row)
        if not row["complete"]:
            failed = True
    trade_rows = []
    candle_rows = []
    for coin in coins:
        trades = _one_shot(
            {"type": "recentTrades", "coin": coin}, timeout, retries, record)
        trades["coin"] = coin
        trade_rows.append(trades)
        candles = _one_shot({
            "type": "candleSnapshot",
            "req": {
                "coin": coin,
                "interval": "1m",
                "startTime": int(since_ms),
                "endTime": int(end_ms),
            },
        }, timeout, retries, record)
        candles["coin"] = coin
        candles["interval"] = "1m"
        candle_rows.append(candles)
        if not trades["complete"] or not candles["complete"]:
            failed = True
    bundle = {
        "schema": SCHEMA,
        "schema_version": SCHEMA_VERSION,
        "inputs": {
            "address_sha256": sha256_bytes(address.encode("utf-8")),
            "end_ms": int(end_ms),
            "export_sha256": export_hashes,
            "since_ms": int(since_ms),
        },
        "completeness": {
            "candleSnapshot": candle_rows,
            "historicalOrders": historical,
            "orderStatus": order_rows,
            "recentTrades": trade_rows,
            "userFillsByTime": fills,
        },
        "page_cap": PAGE_CAP,
        "captured_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
    }
    with open(os.path.join(out_dir, "bundle.json"), "w") as fh:
        json.dump(bundle, fh, sort_keys=True, indent=2)
        fh.write("\n")
    if failed:
        raise CaptureError("one or more info requests failed or the fill stream is incomplete")


def _address_from(args):
    raw = (args.address or os.environ.get("HYPERLIQUID_ACCOUNT_ADDRESS") or "").strip()
    if not raw:
        raise CaptureError("pass --address or set HYPERLIQUID_ACCOUNT_ADDRESS")
    if len(raw) != 42 or not raw.startswith("0x"):
        raise CaptureError("address must be a 0x-prefixed 40-digit hex string")
    hexpart = raw[2:]
    if any(c not in "0123456789abcdefABCDEF" for c in hexpart):
        raise CaptureError("address must be a 0x-prefixed 40-digit hex string")
    return raw


def main(argv):
    parser = argparse.ArgumentParser(
        description="Read-only Hyperliquid info capture for resting take-profit orders.")
    parser.add_argument("--export", action="append", required=True,
                        help="go-trader booked-ledger export; repeatable")
    parser.add_argument("--address", default="",
                        help="account address; else HYPERLIQUID_ACCOUNT_ADDRESS")
    parser.add_argument("--since-ms", type=int, required=True)
    parser.add_argument("--end-ms", type=int, required=True)
    parser.add_argument("--out", required=True,
                        help="new empty directory outside the repository")
    parser.add_argument("--timeout", type=float, default=DEFAULT_TIMEOUT_S)
    parser.add_argument("--retries", type=int, default=DEFAULT_RETRIES)
    args = parser.parse_args(argv)
    if args.since_ms < 0 or args.end_ms <= args.since_ms:
        raise CaptureError("--since-ms must be >= 0 and less than --end-ms")
    repo_root = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
    out = _prepare_out(args.out, repo_root)
    address = _address_from(args)
    capture(args.export, address, args.since_ms, args.end_ms, out, args.timeout, args.retries)


if __name__ == "__main__":
    try:
        main(sys.argv[1:])
    except CaptureError as exc:
        print(str(exc), file=sys.stderr)
        sys.exit(1)
