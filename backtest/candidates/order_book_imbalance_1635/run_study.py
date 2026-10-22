#!/usr/bin/env python3

import argparse
import gzip
import hashlib
import json
import os
import sys
import time
import urllib.request

INFO_URL = "https://api.hyperliquid.xyz/info"
COINS = ("BTC", "ETH")
CAPTURE_HOURS = 72
MAX_CAPTURE_HOURS = 80
MINUTE_MS = 60_000
CHUNK_MINUTES = 4000
CAPTURE_SCHEMA = "go_trader_obi_1635_capture/v1"


def _now_ms() -> int:
    return int(time.time() * 1000)


def _post(body: dict, attempts: int = 5) -> object:
    blob = json.dumps(body).encode("utf-8")
    last = None
    for i in range(attempts):
        try:
            req = urllib.request.Request(INFO_URL, data=blob, headers={"Content-Type": "application/json"})
            with urllib.request.urlopen(req, timeout=30) as resp:
                return json.load(resp)
        except Exception as exc:
            last = exc
            time.sleep(2 * (i + 1))
    raise RuntimeError(f"{body.get('type')} request failed after {attempts} attempts: {last}")


def _write_gz_json(path: str, value) -> str:
    blob = json.dumps(value, separators=(",", ":"), sort_keys=True).encode("utf-8")
    tmp = path + ".tmp"
    with open(tmp, "wb") as raw:
        with gzip.GzipFile(fileobj=raw, mode="wb", mtime=0) as fh:
            fh.write(blob)
        raw.flush()
        os.fsync(raw.fileno())
    os.replace(tmp, path)
    with open(path, "rb") as fh:
        return hashlib.sha256(fh.read()).hexdigest()


def _append_index(out_dir: str, entry: dict) -> None:
    path = os.path.join(out_dir, "captures.jsonl")
    with open(path, "a") as fh:
        fh.write(json.dumps(entry, separators=(",", ":"), sort_keys=True) + "\n")
        fh.flush()
        os.fsync(fh.fileno())


def capture_candles(out_dir: str, coins, hours: int) -> dict:
    if not 1 <= hours <= MAX_CAPTURE_HOURS:
        raise SystemExit(f"--hours must be 1 to {MAX_CAPTURE_HOURS}; the venue keeps about 3.5 days of 1m candles")
    os.makedirs(out_dir, exist_ok=True)
    captured = _now_ms()
    end = captured // MINUTE_MS * MINUTE_MS
    start = end - hours * 3_600_000
    out = {}
    for coin in coins:
        rows = {}
        cursor = start
        while cursor < end:
            stop = min(end, cursor + CHUNK_MINUTES * MINUTE_MS)
            batch = _post({"type": "candleSnapshot", "req": {"coin": coin, "interval": "1m", "startTime": cursor, "endTime": stop}})
            if not isinstance(batch, list):
                raise RuntimeError(f"{coin} candleSnapshot returned {type(batch).__name__}")
            for c in batch:
                t, close_t = int(c["t"]), int(c["T"])
                if start <= t < end and close_t < captured:
                    rows[t] = {k: c[k] for k in ("t", "T", "o", "h", "l", "c", "v", "n", "s", "i")}
            cursor = stop
        ordered = [rows[k] for k in sorted(rows)]
        name = f"candles_1m_{coin}_{captured}.json.gz"
        digest = _write_gz_json(os.path.join(out_dir, name), ordered)
        entry = {"schema": CAPTURE_SCHEMA, "kind": "candles_1m", "coin": coin, "file": name, "sha256": digest,
                 "captured_ms": captured, "start_ms": start, "end_ms": end, "rows": len(ordered),
                 "first_open_ms": ordered[0]["t"] if ordered else None, "last_open_ms": ordered[-1]["t"] if ordered else None}
        _append_index(out_dir, entry)
        out[coin] = {"rows": len(ordered), "file": name, "sha256": digest}
    return out


def capture_venue(out_dir: str, label: str) -> dict:
    os.makedirs(out_dir, exist_ok=True)
    captured = _now_ms()
    meta = _post({"type": "meta"})
    if not isinstance(meta, dict) or "universe" not in meta:
        raise RuntimeError("meta returned no universe")
    name = f"venue_meta_{label}_{captured}.json.gz"
    digest = _write_gz_json(os.path.join(out_dir, name), meta)
    assets = {a["name"]: a for a in meta["universe"] if a.get("name") in COINS}
    entry = {"schema": CAPTURE_SCHEMA, "kind": "venue_meta", "label": label, "file": name, "sha256": digest,
             "captured_ms": captured, "assets": assets}
    _append_index(out_dir, entry)
    return {"file": name, "sha256": digest, "assets": assets}


def main(argv=None) -> int:
    p = argparse.ArgumentParser(description="Issue 1635 order-book imbalance study: data capture during the recording")
    sub = p.add_subparsers(dest="cmd", required=True)
    cc = sub.add_parser("capture-candles", help="Capture closed 1m candles for the last --hours hours (run every 24 h during the recording)")
    cc.add_argument("--out-dir", required=True)
    cc.add_argument("--coins", default=",".join(COINS))
    cc.add_argument("--hours", type=int, default=CAPTURE_HOURS)
    cv = sub.add_parser("capture-venue", help="Pin venue metadata (size decimals, leverage, margin tables)")
    cv.add_argument("--out-dir", required=True)
    cv.add_argument("--label", required=True, choices=("start", "end"))
    args = p.parse_args(argv)
    if args.cmd == "capture-candles":
        coins = [c.strip() for c in args.coins.split(",") if c.strip()]
        result = capture_candles(args.out_dir, coins, args.hours)
    else:
        result = capture_venue(args.out_dir, args.label)
    print(json.dumps(result, sort_keys=True))
    return 0


if __name__ == "__main__":
    sys.exit(main())
