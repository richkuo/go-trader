#!/usr/bin/env python3
import argparse
import http.server
import json
import math
import sys
import threading
import time
import urllib.error
import urllib.request
from collections import defaultdict

USAGE = """Count Hyperliquid /info requests per process to measure a REST request baseline.

Proxy mode forwards every request to the venue and appends one JSON line per
request to the ledger file:

    python3 scripts/hl-request-ledger.py proxy --port 18901 --label paper-btc \\
        --ledger /var/tmp/hl-ledger.jsonl

Point a measurement build of go-trader at it with
    go -C scheduler build -ldflags "-X main.hlMainnetURL=http://127.0.0.1:18901" -o <binary> .
and point the check scripts at it by starting that binary with
    PYTHONPATH=<repo>/scripts/hl_request_ledger HL_REQUEST_LEDGER_URL=http://127.0.0.1:18901
A websocket-mode consumer also needs -X main.hlFeedWebsocketURLOverride=wss://api.hyperliquid.xyz/ws.

Report mode summarizes one or more ledger files per label:

    python3 scripts/hl-request-ledger.py report --ledger /var/tmp/hl-ledger.jsonl --startup-seconds 180
"""

UPSTREAM = "https://api.hyperliquid.xyz"


def describe_request(body):
    out = {"type": "?"}
    try:
        payload = json.loads(body or b"{}")
    except ValueError:
        return out
    if not isinstance(payload, dict):
        return out
    out["type"] = str(payload.get("type", "?"))
    req = payload.get("req") if isinstance(payload.get("req"), dict) else {}
    for key in ("coin", "interval", "startTime", "endTime"):
        if key in req:
            out[key] = req[key]
        elif key in payload:
            out[key] = payload[key]
    return out


def make_handler(upstream, label, ledger_path, lock):
    class Handler(http.server.BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def log_message(self, fmt, *args):
            return

        def do_POST(self):
            length = int(self.headers.get("Content-Length") or 0)
            body = self.rfile.read(length) if length else b""
            started = time.time()
            req = urllib.request.Request(upstream + self.path, data=body, method="POST",
                                         headers={"Content-Type": self.headers.get("Content-Type", "application/json")})
            status, data = 502, b'{"error":"ledger proxy upstream failure"}'
            try:
                with urllib.request.urlopen(req, timeout=30) as resp:
                    status, data = resp.status, resp.read()
            except urllib.error.HTTPError as exc:
                status, data = exc.code, exc.read()
            except Exception as exc:
                data = json.dumps({"error": f"ledger proxy: {exc}"}).encode()
            record = describe_request(body)
            record.update({"ts": round(started, 3), "label": label, "path": self.path, "status": status,
                           "ms": int((time.time() - started) * 1000)})
            with lock:
                with open(ledger_path, "a") as fh:
                    fh.write(json.dumps(record, sort_keys=True) + "\n")
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

    return Handler


def run_proxy(args):
    lock = threading.Lock()
    handler = make_handler(args.upstream.rstrip("/"), args.label, args.ledger, lock)
    server = http.server.ThreadingHTTPServer(("127.0.0.1", args.port), handler)
    print(f"hl-request-ledger: {args.label} listening on http://127.0.0.1:{args.port} -> {args.upstream}, ledger {args.ledger}", flush=True)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    return 0


def percentile(values, pct):
    if not values:
        return 0
    ordered = sorted(values)
    idx = max(0, math.ceil(pct / 100 * len(ordered)) - 1)
    return ordered[idx]


def minute_counts(stamps, start, end):
    buckets = defaultdict(int)
    for ts in stamps:
        buckets[int((ts - start) // 60)] += 1
    minutes = max(1, int((end - start) // 60) + 1)
    return [buckets.get(i, 0) for i in range(minutes)]


def run_report(args):
    records = []
    for path in args.ledger:
        with open(path) as fh:
            for line in fh:
                line = line.strip()
                if line:
                    records.append(json.loads(line))
    if not records:
        print("hl-request-ledger: no records", file=sys.stderr)
        return 1
    by_label = defaultdict(list)
    for r in records:
        by_label[r.get("label", "?")].append(r)
    all_steady = []
    all_start = min(r["ts"] for r in records)
    all_end = max(r["ts"] for r in records)
    print(f"hl-request-ledger: {len(records)} requests over {int(all_end - all_start)}s from {len(by_label)} label(s); startup window {args.startup_seconds}s per label")
    for label in sorted(by_label):
        recs = sorted(by_label[label], key=lambda r: r["ts"])
        start, end = recs[0]["ts"], recs[-1]["ts"]
        cold = [r for r in recs if r["ts"] - start < args.startup_seconds]
        steady = [r for r in recs if r["ts"] - start >= args.startup_seconds]
        all_steady.extend(steady)
        types = defaultdict(int)
        for r in recs:
            types[r.get("type", "?")] += 1
        errors = sum(1 for r in recs if r.get("status") != 200)
        per_min = minute_counts([r["ts"] for r in steady], start + args.startup_seconds, end) if steady else [0]
        mean = sum(per_min) / len(per_min)
        print(f"  {label}: total={len(recs)} errors={errors} cold_start={len(cold)} steady={len(steady)} "
              f"steady_per_minute mean={mean:.1f} p95={percentile(per_min, 95)} peak={max(per_min)}")
        print(f"    by type: " + ", ".join(f"{t}={n}" for t, n in sorted(types.items())))
        candles = defaultdict(int)
        for r in recs:
            if r.get("type") == "candleSnapshot":
                candles[f"{r.get('coin')}|{r.get('interval')}"] += 1
        if candles:
            print(f"    candle reads by key: " + ", ".join(f"{k}={n}" for k, n in sorted(candles.items())))
    if all_steady:
        begin = min(r["ts"] for r in all_steady)
        per_min = minute_counts([r["ts"] for r in all_steady], begin, max(r["ts"] for r in all_steady))
        print(f"  summed steady load: minutes={len(per_min)} mean={sum(per_min) / len(per_min):.1f} p95={percentile(per_min, 95)} peak={max(per_min)} "
              f"hourly_estimate={sum(per_min) * 60 / len(per_min):.0f}")
    cold_total = 0
    for label, recs in by_label.items():
        start = min(r["ts"] for r in recs)
        cold_total += sum(1 for r in recs if r["ts"] - start < args.startup_seconds)
    print(f"  summed cold start: {cold_total} requests")
    return 0


def main():
    parser = argparse.ArgumentParser(description=USAGE, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest="cmd", required=True)
    proxy = sub.add_parser("proxy")
    proxy.add_argument("--port", type=int, required=True)
    proxy.add_argument("--label", required=True)
    proxy.add_argument("--ledger", required=True)
    proxy.add_argument("--upstream", default=UPSTREAM)
    report = sub.add_parser("report")
    report.add_argument("--ledger", action="append", required=True)
    report.add_argument("--startup-seconds", type=int, default=180)
    args = parser.parse_args()
    if args.cmd == "proxy":
        return run_proxy(args)
    return run_report(args)


if __name__ == "__main__":
    sys.exit(main())
