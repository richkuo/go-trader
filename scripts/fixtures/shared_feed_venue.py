#!/usr/bin/env python3
import argparse
import base64
import hashlib
import json
import math
import os
import socket
import struct
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

INTERVALS_MS = {
    "1m": 60_000,
    "3m": 180_000,
    "5m": 300_000,
    "15m": 900_000,
    "30m": 1_800_000,
    "1h": 3_600_000,
    "2h": 7_200_000,
    "4h": 14_400_000,
    "8h": 28_800_000,
    "12h": 43_200_000,
    "1d": 86_400_000,
    "3d": 259_200_000,
    "1w": 604_800_000,
    "1M": 2_678_400_000,
}
HISTORY_BARS = 5000
CANDLE_ROW_CAP = 5000
FUNDING_ROW_CAP = 500
FUNDING_STEP_MS = 3_600_000
FUNDING_RATE = "0.0000125"
WS_GUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
PUSH_EVERY = 1.0


def now_ms():
    return int(time.time() * 1000)


def num(v):
    return f"{v:.4f}"


def seed_of(coin, interval):
    return sum(ord(ch) for ch in coin + "|" + interval)


def bar(coin, interval, open_ms):
    ims = INTERVALS_MS[interval]
    n = open_ms // ims
    step = (n * 7 + seed_of(coin, interval)) % 40
    base = 100.0 + step * 0.25
    o = base
    c = base + 0.125 * ((n % 3) - 1)
    h = max(o, c) + 0.5
    lo = min(o, c) - 0.5
    v = 10.0 + n % 7
    return {"t": open_ms, "T": open_ms + ims - 1, "s": coin, "i": interval, "o": num(o), "c": num(c),
            "h": num(h), "l": num(lo), "v": num(v), "n": 5}


def mid(coin):
    return bar(coin, "1m", now_ms() // 60_000 * 60_000)["c"]


class Venue:
    def __init__(self, coins, log_path):
        self.coins = sorted(set(coins))
        self.log_path = log_path
        self.lock = threading.Lock()

    def log(self, **rec):
        rec["at"] = now_ms()
        line = json.dumps(rec, sort_keys=True)
        with self.lock:
            with open(self.log_path, "a") as f:
                f.write(line + "\n")
        if rec.get("kind") == "unknown":
            print(f"shared-feed venue: UNKNOWN {line}", file=sys.stderr, flush=True)

    def candles(self, req):
        coin = req.get("coin")
        interval = req.get("interval")
        start = req.get("startTime")
        end = req.get("endTime")
        if coin not in self.coins or interval not in INTERVALS_MS or not isinstance(start, int):
            return None
        ims = INTERVALS_MS[interval]
        now = now_ms()
        if not isinstance(end, int) or end > now:
            end = now
        floor = (now // ims - HISTORY_BARS + 1) * ims
        first = max(-(-start // ims) * ims, floor)
        last = end // ims * ims
        rows = []
        t = first
        while t <= last and len(rows) < CANDLE_ROW_CAP:
            rows.append(bar(coin, interval, t))
            t += ims
        return rows

    def funding(self, body):
        coin = body.get("coin")
        start = body.get("startTime")
        end = body.get("endTime")
        if coin not in self.coins or not isinstance(start, int) or set(body) - {"type", "coin", "startTime", "endTime"}:
            return None
        now = now_ms()
        if not isinstance(end, int) or end > now:
            end = now
        rows = []
        t = -(-start // FUNDING_STEP_MS) * FUNDING_STEP_MS
        while t <= end and len(rows) < FUNDING_ROW_CAP:
            rows.append({"coin": coin, "fundingRate": FUNDING_RATE, "premium": "0.0", "time": t})
            t += FUNDING_STEP_MS
        return rows

    def info(self, body):
        kind = body.get("type")
        if kind == "candleSnapshot":
            req = body.get("req") or {}
            rows = self.candles(req)
            if rows is None:
                return None, f"candleSnapshot for an unknown coin or interval: {json.dumps(req, sort_keys=True)}"
            self.log(kind="rest", type=kind, coin=req.get("coin"), interval=req.get("interval"), rows=len(rows))
            return rows, ""
        if kind == "fundingHistory":
            rows = self.funding(body)
            if rows is None:
                return None, f"fundingHistory for an unknown coin or with unknown fields: {json.dumps(body, sort_keys=True)}"
            self.log(kind="rest", type=kind, coin=body.get("coin"), rows=len(rows))
            return rows, ""
        if kind == "allMids" and set(body) == {"type"}:
            self.log(kind="rest", type=kind)
            return {coin: mid(coin) for coin in self.coins}, ""
        return None, f"unsupported /info request: {json.dumps(body, sort_keys=True)[:400]}"


def ws_accept(key):
    return base64.b64encode(hashlib.sha1((key + WS_GUID).encode()).digest()).decode()


class WSConn:
    def __init__(self, venue, sock):
        self.venue = venue
        self.sock = sock
        self.wlock = threading.Lock()
        self.subs = set()
        self.mids = False
        self.closed = threading.Event()

    def send_frame(self, opcode, payload):
        head = bytes([0x80 | opcode])
        n = len(payload)
        if n < 126:
            head += bytes([n])
        elif n < 65536:
            head += bytes([126]) + struct.pack("!H", n)
        else:
            head += bytes([127]) + struct.pack("!Q", n)
        with self.wlock:
            self.sock.sendall(head + payload)

    def send_json(self, obj):
        self.send_frame(0x1, json.dumps(obj, separators=(",", ":")).encode())

    def recv_exact(self, n):
        buf = b""
        while len(buf) < n:
            chunk = self.sock.recv(n - len(buf))
            if not chunk:
                raise ConnectionError("peer closed")
            buf += chunk
        return buf

    def recv_frame(self):
        b1, b2 = self.recv_exact(2)
        fin = b1 & 0x80
        opcode = b1 & 0x0F
        masked = b2 & 0x80
        n = b2 & 0x7F
        if n == 126:
            n = struct.unpack("!H", self.recv_exact(2))[0]
        elif n == 127:
            n = struct.unpack("!Q", self.recv_exact(8))[0]
        mask = self.recv_exact(4) if masked else b""
        data = self.recv_exact(n)
        if masked:
            data = bytes(x ^ mask[i % 4] for i, x in enumerate(data))
        return fin, opcode, data

    def fail(self, detail):
        self.venue.log(kind="unknown", transport="ws", detail=detail)
        try:
            self.send_json({"channel": "error", "data": "fixture venue: " + detail})
        except OSError:
            pass

    def handle_text(self, data):
        try:
            msg = json.loads(data)
        except ValueError:
            self.fail(f"websocket text that is not JSON: {data[:200]!r}")
            return
        method = msg.get("method")
        if method == "ping":
            self.send_json({"channel": "pong"})
            return
        sub = msg.get("subscription") or {}
        stype = sub.get("type")
        if method == "subscribe" and stype == "candle" and sub.get("coin") in self.venue.coins and sub.get("interval") in INTERVALS_MS:
            self.subs.add((sub["coin"], sub["interval"]))
        elif method == "subscribe" and stype == "allMids" and set(sub) == {"type"}:
            self.mids = True
        else:
            self.fail(f"unsupported websocket message: {json.dumps(msg, sort_keys=True)[:400]}")
            return
        self.venue.log(kind="ws", type=stype, coin=sub.get("coin"), interval=sub.get("interval"))
        self.send_json({"channel": "subscriptionResponse", "data": msg})
        self.push()

    def push(self):
        t = now_ms()
        for coin, interval in sorted(self.subs):
            ims = INTERVALS_MS[interval]
            self.send_json({"channel": "candle", "data": bar(coin, interval, t // ims * ims)})
        if self.mids:
            self.send_json({"channel": "allMids", "data": {"mids": {coin: mid(coin) for coin in self.venue.coins}}})

    def pusher(self):
        while not self.closed.wait(PUSH_EVERY):
            try:
                self.push()
            except OSError:
                self.closed.set()

    def run(self):
        threading.Thread(target=self.pusher, daemon=True).start()
        try:
            while not self.closed.is_set():
                fin, opcode, data = self.recv_frame()
                if opcode == 0x8:
                    try:
                        self.send_frame(0x8, data[:2])
                    except OSError:
                        pass
                    break
                if opcode == 0x9:
                    self.send_frame(0xA, data)
                    continue
                if opcode == 0xA:
                    continue
                if opcode != 0x1 or not fin:
                    self.fail(f"unsupported websocket frame opcode={opcode} fin={bool(fin)}")
                    continue
                self.handle_text(data.decode("utf-8", "replace"))
        except (ConnectionError, OSError):
            pass
        finally:
            self.closed.set()


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    venue = None

    def log_message(self, fmt, *args):
        pass

    def reply(self, status, obj):
        data = json.dumps(obj, separators=(",", ":")).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def unknown(self, detail):
        self.venue.log(kind="unknown", transport="http", detail=detail)
        self.reply(400, {"error": "fixture venue: " + detail})

    def do_GET(self):
        if self.path == "/fixture/ready":
            self.reply(200, {"ready": True, "coins": self.venue.coins})
            return
        if self.path == "/ws" and self.headers.get("Upgrade", "").lower() == "websocket" and self.headers.get("Sec-WebSocket-Key"):
            self.send_response(101)
            self.send_header("Upgrade", "websocket")
            self.send_header("Connection", "Upgrade")
            self.send_header("Sec-WebSocket-Accept", ws_accept(self.headers["Sec-WebSocket-Key"]))
            self.end_headers()
            self.wfile.flush()
            self.venue.log(kind="ws", type="connect")
            self.close_connection = True
            WSConn(self.venue, self.connection).run()
            return
        self.unknown(f"GET {self.path[:200]}")

    def do_POST(self):
        n = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(n) if n > 0 else b""
        if self.path != "/info":
            self.unknown(f"POST {self.path[:200]}")
            return
        try:
            body = json.loads(raw)
        except ValueError:
            self.unknown(f"POST /info with a body that is not JSON: {raw[:200]!r}")
            return
        if not isinstance(body, dict):
            self.unknown(f"POST /info with a body that is not an object: {raw[:200]!r}")
            return
        out, detail = self.venue.info(body)
        if detail:
            self.unknown(detail)
            return
        self.reply(200, out)


def main():
    ap = argparse.ArgumentParser(description="Local Hyperliquid stand-in for the shared feed service fixture.")
    ap.add_argument("--port", type=int, required=True)
    ap.add_argument("--coin", action="append", required=True)
    ap.add_argument("--log", required=True)
    args = ap.parse_args()
    Handler.venue = Venue(args.coin, args.log)
    srv = ThreadingHTTPServer(("127.0.0.1", args.port), Handler)
    srv.daemon_threads = True
    print(f"shared-feed venue: listening on 127.0.0.1:{args.port} coins={','.join(Handler.venue.coins)} log={args.log}", flush=True)
    srv.serve_forever()


if __name__ == "__main__":
    main()
