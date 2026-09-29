#!/usr/bin/env python3
import argparse
import heapq
import json
import math
import sys
import threading
import time
import urllib.error
import urllib.request
from collections import defaultdict
from concurrent.futures import ThreadPoolExecutor

USAGE = """Measure when Hyperliquid revises a closed candle, from the socket and from REST.

Capture mode subscribes to the candle channel for every coin and interval and
appends one JSON line per socket message. After each bar closes it also reads
candleSnapshot at fixed offsets from the close, each read covering the last
--window-bars bars, so every closed bar is re-read many times over several
intervals:

    uv run --no-sync python scripts/feed-revision-capture.py capture \\
        --coins BTC,ETH,SOL,HYPE,DOGE --intervals 1m,5m --minutes 90 \\
        --out /var/tmp/feed-revisions.jsonl

Report mode reads one or more capture files and prints, per interval, how many
closed bars changed after their close, when the last socket update and the
last REST change arrived, how many bars end with a socket value that differs
from the final REST value (a missed revision), how often a REST read returned
data older than the socket already held, and every read failure:

    uv run --no-sync python scripts/feed-revision-capture.py report \\
        --capture /var/tmp/feed-revisions.jsonl [--json]

Delays are measured from the bar's close (open time plus one interval). A
revision is observed only when a sample sees it: a REST change is bounded by
the last read that differed from the final value and the first read that
matched it, so the offsets set the resolution. Observed delays are a sample,
not a guarantee about venue finality. Needs the websocket-client package
(installed with hyperliquid-python-sdk).
"""

UPSTREAM = "https://api.hyperliquid.xyz"
INTERVAL_MS = {
    "1m": 60_000, "3m": 180_000, "5m": 300_000, "15m": 900_000, "30m": 1_800_000,
    "1h": 3_600_000, "2h": 7_200_000, "4h": 14_400_000,
}
FIELDS = ("o", "h", "l", "c", "v")


def now_ms():
    return int(time.time() * 1000)


class Writer:
    def __init__(self, path):
        self.lock = threading.Lock()
        self.fh = open(path, "a", buffering=1)

    def write(self, record):
        line = json.dumps(record, separators=(",", ":"), sort_keys=True)
        with self.lock:
            self.fh.write(line + "\n")


def bar_of(raw):
    out = {"t": int(raw["t"]), "T": int(raw["T"])}
    for f in FIELDS:
        out[f] = str(raw[f])
    if "n" in raw:
        out["n"] = int(raw["n"])
    return out


def run_socket(args, writer, stop):
    import websocket

    url = args.url.replace("https://", "wss://").rstrip("/") + "/ws"

    def on_open(ws):
        writer.write({"kind": "ws_event", "event": "open", "at_ms": now_ms()})
        for coin in args.coins:
            for iv in args.intervals:
                ws.send(json.dumps({"method": "subscribe",
                                    "subscription": {"type": "candle", "coin": coin, "interval": iv}}))

    def on_message(ws, message):
        recv = now_ms()
        try:
            env = json.loads(message)
        except ValueError:
            return
        if env.get("channel") != "candle":
            return
        data = env.get("data") or {}
        try:
            bar = bar_of(data)
        except (KeyError, TypeError, ValueError):
            return
        writer.write({"kind": "ws", "recv_ms": recv, "coin": data.get("s"), "i": data.get("i"), "bar": bar})

    def on_close(ws, code, msg):
        writer.write({"kind": "ws_event", "event": "close", "at_ms": now_ms(), "code": code, "msg": str(msg)})

    def on_error(ws, err):
        writer.write({"kind": "ws_event", "event": "error", "at_ms": now_ms(), "error": str(err)})

    while not stop.is_set():
        ws = websocket.WebSocketApp(url, on_open=on_open, on_message=on_message, on_close=on_close, on_error=on_error)
        ws.run_forever(ping_interval=30, ping_timeout=10)
        if not stop.is_set():
            time.sleep(2)


def rest_read(args, writer, coin, iv, close_ms, offset_s):
    iv_ms = INTERVAL_MS[iv]
    req = now_ms()
    start = close_ms - iv_ms * args.window_bars
    body = json.dumps({"type": "candleSnapshot",
                       "req": {"coin": coin, "interval": iv, "startTime": start, "endTime": req}}).encode()
    record = {"kind": "rest", "coin": coin, "i": iv, "req_ms": req, "anchor_close_ms": close_ms, "offset_s": offset_s}
    try:
        http_req = urllib.request.Request(args.url.rstrip("/") + "/info", data=body,
                                          headers={"Content-Type": "application/json"})
        with urllib.request.urlopen(http_req, timeout=args.timeout) as resp:
            rows = json.loads(resp.read())
        record["ok"] = True
        record["bars"] = [bar_of(r) for r in rows]
    except (urllib.error.URLError, OSError, ValueError, KeyError, TypeError) as exc:
        record["ok"] = False
        record["error"] = str(exc)
    record["resp_ms"] = now_ms()
    writer.write(record)


def run_reads(args, writer, stop, deadline_ms):
    heap = []
    start = now_ms()
    for coin in args.coins:
        for iv in args.intervals:
            iv_ms = INTERVAL_MS[iv]
            first_close = (start // iv_ms + 1) * iv_ms
            heapq.heappush(heap, (first_close, coin, iv))
    pool = ThreadPoolExecutor(max_workers=args.workers)
    pending = []
    while not stop.is_set():
        close_ms, coin, iv = heapq.heappop(heap)
        if close_ms > deadline_ms:
            break
        for off in args.offsets:
            heapq.heappush(pending, (close_ms + int(off * 1000), coin, iv, close_ms, off))
        heapq.heappush(heap, (close_ms + INTERVAL_MS[iv], coin, iv))
        next_close = heap[0][0]
        while pending and pending[0][0] < next_close and not stop.is_set():
            due, c, i, cm, off = heapq.heappop(pending)
            wait = (due - now_ms()) / 1000.0
            if wait > 0 and stop.wait(wait):
                break
            pool.submit(rest_read, args, writer, c, i, cm, off)
    while pending and not stop.is_set():
        due, c, i, cm, off = heapq.heappop(pending)
        wait = (due - now_ms()) / 1000.0
        if wait > 0 and stop.wait(wait):
            break
        pool.submit(rest_read, args, writer, c, i, cm, off)
    pool.shutdown(wait=True)


def capture(args):
    writer = Writer(args.out)
    stop = threading.Event()
    deadline = now_ms() + int(args.minutes * 60_000)
    writer.write({"kind": "meta", "started_ms": now_ms(), "coins": args.coins, "intervals": args.intervals,
                  "offsets_s": args.offsets, "window_bars": args.window_bars, "minutes": args.minutes, "url": args.url})
    sock = threading.Thread(target=run_socket, args=(args, writer, stop), daemon=True)
    sock.start()
    try:
        run_reads(args, writer, stop, deadline)
    except KeyboardInterrupt:
        pass
    stop.set()
    writer.write({"kind": "meta", "stopped_ms": now_ms()})
    return 0


def vals(bar):
    return tuple(float(bar[f]) for f in FIELDS)


def percentile(xs, p):
    if not xs:
        return None
    xs = sorted(xs)
    k = (len(xs) - 1) * p
    lo, hi = math.floor(k), math.ceil(k)
    return xs[lo] + (xs[hi] - xs[lo]) * (k - lo)


def summarize(xs):
    if not xs:
        return {"n": 0}
    return {"n": len(xs), "p50": round(percentile(xs, 0.5), 3), "p95": round(percentile(xs, 0.95), 3),
            "p99": round(percentile(xs, 0.99), 3), "max": round(max(xs), 3)}


def report(args):
    ws = defaultdict(list)
    rest = defaultdict(list)
    failures = []
    reads = defaultdict(int)
    disconnects = []
    started = stopped = None
    for path in args.capture:
        with open(path) as fh:
            for line in fh:
                try:
                    rec = json.loads(line)
                except ValueError:
                    continue
                kind = rec.get("kind")
                if kind == "meta":
                    started = rec.get("started_ms", started) if started is None else started
                    stopped = rec.get("stopped_ms", stopped)
                elif kind == "ws":
                    b = rec["bar"]
                    ws[(rec["coin"], rec["i"], b["t"])].append((rec["recv_ms"], b))
                elif kind == "ws_event" and rec.get("event") in ("close", "error"):
                    disconnects.append(rec.get("at_ms"))
                elif kind == "rest":
                    reads[rec["i"]] += 1
                    if not rec.get("ok"):
                        failures.append(rec)
                        continue
                    for b in rec.get("bars", []):
                        rest[(rec["coin"], rec["i"], b["t"])].append((rec["req_ms"], rec["resp_ms"], b))
    per_iv = {}
    missed = []
    rest_older = []
    for key in sorted(set(ws) | set(rest)):
        coin, iv, t = key
        iv_ms = INTERVAL_MS.get(iv)
        if iv_ms is None:
            continue
        close = t + iv_ms
        obs = sorted(rest.get(key, []), key=lambda x: x[0])
        post_close = [o for o in obs if o[0] >= close]
        socket_msgs = sorted(ws.get(key, []), key=lambda x: x[0])
        if not post_close or not socket_msgs:
            continue
        final_req, _, final_bar = post_close[-1]
        if (final_req - close) / 1000.0 < args.final_after:
            continue
        s = per_iv.setdefault(iv, {"bars": 0, "changed_after_close_socket": 0, "socket_last_update_s": [],
                                   "rest_changed_after_close": 0, "rest_change_between_s": [],
                                   "missed_revisions": 0, "rest_older_than_socket": 0, "reads": reads[iv]})
        s["bars"] += 1
        final = vals(final_bar)
        after = [m for m in socket_msgs if m[0] >= close]
        if after:
            s["socket_last_update_s"].append((after[-1][0] - close) / 1000.0)
            before = [m for m in socket_msgs if m[0] < close]
            if before and vals(after[-1][1]) != vals(before[-1][1]):
                s["changed_after_close_socket"] += 1
        last_diff = None
        first_match = None
        for req, _, b in post_close:
            if vals(b) != final:
                last_diff = req
                first_match = None
            elif first_match is None:
                first_match = req
        if last_diff is not None:
            s["rest_changed_after_close"] += 1
            s["rest_change_between_s"].append([round((last_diff - close) / 1000.0, 3),
                                               round((first_match - close) / 1000.0, 3)])
        sock_final = vals(socket_msgs[-1][1])
        if sock_final != final:
            s["missed_revisions"] += 1
            missed.append({"coin": coin, "i": iv, "t": t, "socket_last_recv_s": round((socket_msgs[-1][0] - close) / 1000.0, 3),
                           "socket": dict(zip(FIELDS, sock_final)), "rest_final": dict(zip(FIELDS, final)),
                           "rest_final_req_s": round((final_req - close) / 1000.0, 3)})
        for req, resp, b in post_close:
            held = [m for m in socket_msgs if m[0] < req]
            if held and float(b["v"]) < float(held[-1][1]["v"]):
                s["rest_older_than_socket"] += 1
                rest_older.append({"coin": coin, "i": iv, "t": t, "req_s": round((req - close) / 1000.0, 3),
                                   "rest_v": b["v"], "socket_v": held[-1][1]["v"]})
                break
    out = {"started_ms": started, "stopped_ms": stopped, "final_after_s": args.final_after,
           "read_failures": len(failures), "socket_disconnects": len(disconnects), "intervals": {}}
    for iv, s in sorted(per_iv.items(), key=lambda kv: INTERVAL_MS[kv[0]]):
        between = s.pop("rest_change_between_s")
        s["socket_last_update_s"] = summarize(s["socket_last_update_s"])
        s["rest_change_upper_bound_s"] = summarize([b for _, b in between])
        s["rest_change_windows_s"] = between
        out["intervals"][iv] = s
    out["missed"] = missed
    out["rest_older"] = rest_older
    out["failures"] = [{"coin": f["coin"], "i": f["i"], "offset_s": f["offset_s"], "error": f.get("error")} for f in failures]
    if args.json:
        json.dump(out, sys.stdout, indent=2, sort_keys=True)
        print()
        return 0
    dur = ((stopped or 0) - (started or 0)) / 60000.0 if started and stopped else float("nan")
    print(f"capture: {dur:.1f} min, read failures {len(failures)}, socket disconnects {len(disconnects)}, "
          f"bars counted once a REST read at or after close+{args.final_after:g}s exists")
    for iv, s in out["intervals"].items():
        print(f"{iv}: bars={s['bars']} reads={s['reads']} socket_changed_after_close={s['changed_after_close_socket']} "
              f"socket_last_update_s={s['socket_last_update_s']} rest_changed_after_close={s['rest_changed_after_close']} "
              f"rest_change_upper_bound_s={s['rest_change_upper_bound_s']} missed_revisions={s['missed_revisions']} "
              f"rest_older_than_socket={s['rest_older_than_socket']}")
        for w in s["rest_change_windows_s"]:
            print(f"  {iv} rest change between close+{w[0]}s and close+{w[1]}s")
    for m in missed:
        print(f"MISSED {m['coin']} {m['i']} open={m['t']} socket_last_recv=close+{m['socket_last_recv_s']}s "
              f"socket={m['socket']} rest_final(close+{m['rest_final_req_s']}s)={m['rest_final']}")
    for r in rest_older:
        print(f"REST_OLDER {r['coin']} {r['i']} open={r['t']} read=close+{r['req_s']}s rest_v={r['rest_v']} socket_v={r['socket_v']}")
    for f in out["failures"]:
        print(f"FAILED_READ {f['coin']} {f['i']} offset={f['offset_s']}s: {f['error']}")
    return 0


def main():
    ap = argparse.ArgumentParser(description=USAGE, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest="cmd", required=True)
    cap = sub.add_parser("capture")
    cap.add_argument("--coins", default="BTC,ETH,SOL,HYPE,DOGE")
    cap.add_argument("--intervals", default="1m,5m")
    cap.add_argument("--minutes", type=float, default=60)
    cap.add_argument("--offsets", default="1.5,3,5,8,15,30,45")
    cap.add_argument("--window-bars", type=int, default=8)
    cap.add_argument("--workers", type=int, default=4)
    cap.add_argument("--timeout", type=float, default=10)
    cap.add_argument("--url", default=UPSTREAM)
    cap.add_argument("--out", required=True)
    rep = sub.add_parser("report")
    rep.add_argument("--capture", action="append", required=True)
    rep.add_argument("--final-after", type=float, default=120,
                     help="count a bar only when a REST read at or after close+N seconds exists (default 120)")
    rep.add_argument("--json", action="store_true")
    args = ap.parse_args()
    if args.cmd == "capture":
        args.coins = [c.strip() for c in args.coins.split(",") if c.strip()]
        args.intervals = [i.strip() for i in args.intervals.split(",") if i.strip()]
        for iv in args.intervals:
            if iv not in INTERVAL_MS:
                ap.error(f"unsupported interval {iv}")
        args.offsets = sorted(float(o) for o in args.offsets.split(",") if o.strip())
        return capture(args)
    return report(args)


if __name__ == "__main__":
    sys.exit(main())
