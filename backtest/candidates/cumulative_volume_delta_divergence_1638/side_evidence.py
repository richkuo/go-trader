#!/usr/bin/env python3

import argparse
import hashlib
import json
import sys
import time
import urllib.error
import urllib.request

INFO_URL = "https://api.hyperliquid.xyz/info"
SCHEMA = "cvd_1638_side_evidence/v1"
EVIDENCE_VERSION = "hyperliquid-trades-side-aggressor/1638-v1"


def post(body: dict, attempts: int = 8) -> bytes:
    req = urllib.request.Request(INFO_URL, data=json.dumps(body).encode("utf-8"),
                                 headers={"Content-Type": "application/json"})
    for attempt in range(attempts):
        try:
            with urllib.request.urlopen(req, timeout=30) as resp:
                return resp.read()
        except urllib.error.HTTPError as exc:
            if exc.code != 429 and exc.code < 500 or attempt == attempts - 1:
                raise
        except urllib.error.URLError:
            if attempt == attempts - 1:
                raise
        time.sleep(min(120, 5 * 2 ** attempt))
    raise RuntimeError("unreachable")


def sample_trades(segment_path: str, per_coin: int, stride: int) -> list:
    picked = {}
    seen = {}
    with open(segment_path, "rb") as fh:
        for raw_line in fh:
            if not raw_line.endswith(b"\n"):
                break
            rec = json.loads(raw_line)
            if rec.get("k") != "trades":
                continue
            env = json.loads(rec["raw"])
            outcomes = {o["tid"]: o["o"] for o in rec.get("outcomes") or []}
            for t in env.get("data") or []:
                if outcomes.get(t["tid"]) != "accepted":
                    continue
                coin = t["coin"]
                n = seen.get(coin, 0)
                seen[coin] = n + 1
                bucket = picked.setdefault(coin, [])
                if n % stride == 0 and len(bucket) < per_coin:
                    bucket.append({"trade": t, "recv_ms": rec["recv_ms"], "session": rec["session"]})
    out = []
    for coin in sorted(picked):
        out.extend(picked[coin])
    return out


def check(entry: dict, pause_s: float) -> dict:
    t = entry["trade"]
    users = t["users"]
    fills = []
    for idx, user in enumerate(users):
        body = {"type": "userFillsByTime", "user": user, "startTime": int(t["time"]) - 1000,
                "endTime": int(t["time"]) + 1000}
        blob = post(body)
        time.sleep(pause_s)
        rows = json.loads(blob)
        match = [r for r in rows if r.get("tid") == t["tid"] and r.get("coin") == t["coin"]]
        fills.append({"role": "buyer" if idx == 0 else "seller", "user": user, "request": body,
                      "response_sha256": hashlib.sha256(blob).hexdigest(), "response_fills": len(rows),
                      "matched": [{"side": m.get("side"), "crossed": m.get("crossed"), "px": m.get("px"),
                                   "sz": m.get("sz"), "time": m.get("time"), "hash": m.get("hash"),
                                   "tid": m.get("tid")} for m in match]})
    buyer = fills[0]["matched"][0] if len(fills[0]["matched"]) == 1 else None
    seller = fills[1]["matched"][0] if len(fills[1]["matched"]) == 1 else None
    facts = {
        "buyer_fill_found": buyer is not None,
        "seller_fill_found": seller is not None,
        "buyer_side_is_B": buyer is not None and buyer["side"] == "B",
        "seller_side_is_A": seller is not None and seller["side"] == "A",
    }
    taker = None
    if buyer is not None and seller is not None and buyer["crossed"] != seller["crossed"]:
        taker = "B" if buyer["crossed"] else "A"
    elif buyer is not None and seller is None:
        taker = "B" if buyer["crossed"] else "A"
    elif seller is not None and buyer is None:
        taker = "A" if seller["crossed"] else "B"
    facts["taker_side"] = taker
    facts["trade_side"] = t["side"]
    facts["side_is_taker"] = taker is not None and taker == t["side"]
    facts["fill_matches_trade"] = all(
        m["px"] == t["px"] and m["sz"] == t["sz"] and m["time"] == t["time"] and m["hash"] == t["hash"]
        for f in fills for m in f["matched"])
    return {"coin": t["coin"], "tid": t["tid"], "time": t["time"], "recv_ms": entry["recv_ms"],
            "session": entry["session"], "trade": t, "fills": fills, "facts": facts}


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description="Collect independent taker-fill evidence for the Hyperliquid trades side field (#1638); public info endpoint only, no order")
    ap.add_argument("--segment", required=True, help="Recorder segment JSONL to sample accepted trades from")
    ap.add_argument("--per-coin", type=int, default=40)
    ap.add_argument("--stride", type=int, default=97)
    ap.add_argument("--pause", type=float, default=1.0)
    ap.add_argument("--out", required=True)
    args = ap.parse_args(argv)
    sample = sample_trades(args.segment, args.per_coin, args.stride)
    rows = [check(e, args.pause) for e in sample]
    both = [r for r in rows if r["facts"]["buyer_fill_found"] and r["facts"]["seller_fill_found"]]
    any_taker = [r for r in rows if r["facts"]["taker_side"] is not None]
    summary = {
        "sampled": len(rows),
        "both_fills_found": len(both),
        "taker_identified": len(any_taker),
        "side_equals_taker": sum(1 for r in any_taker if r["facts"]["side_is_taker"]),
        "side_differs_from_taker": sum(1 for r in any_taker if not r["facts"]["side_is_taker"]),
        "buyer_is_users0": sum(1 for r in rows if r["facts"]["buyer_side_is_B"]),
        "seller_is_users1": sum(1 for r in rows if r["facts"]["seller_side_is_A"]),
        "fill_content_mismatch": sum(1 for r in rows if not r["facts"]["fill_matches_trade"]),
    }
    verified = (summary["taker_identified"] >= 30 and summary["side_differs_from_taker"] == 0
                and summary["fill_content_mismatch"] == 0)
    doc = {"schema": SCHEMA, "evidence_version": EVIDENCE_VERSION, "endpoint": INFO_URL,
           "collected_at_ms": int(time.time() * 1000), "segment": args.segment.split("/")[-1],
           "rule": "side B means the buyer (users[0]) crossed the book as taker and side A means the seller (users[1]) did; "
                   "verified when every identified taker fill agrees and at least 30 trades identify a taker",
           "verified": verified, "summary": summary, "trades": rows}
    with open(args.out, "w") as fh:
        json.dump(doc, fh, indent=1, sort_keys=True)
        fh.write("\n")
    print(json.dumps({"verified": verified, **summary}, sort_keys=True))
    return 0 if verified else 1


if __name__ == "__main__":
    sys.exit(main())
