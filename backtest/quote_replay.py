#!/usr/bin/env python3
import argparse
import bisect
import copy
import hashlib
import importlib.util
import json
import math
import os
import subprocess
import sys
from fractions import Fraction
from typing import Dict, List, Optional, Tuple

_HERE = os.path.dirname(os.path.abspath(__file__))
_ROOT = os.path.dirname(_HERE)
for _p in (_HERE, os.path.join(_ROOT, "shared_strategies", "open")):
    if _p not in sys.path:
        sys.path.insert(0, _p)

import book_replay
import trade_flow_replay
from inventory_market_making import (
    NAME,
    QUOTE_POLICY_SPEC,
    spec_sha256,
    validate_run_config,
)

PROTOCOL = "go-trader.quote-replay"
VERSION = 1
QUEUE_MODEL = "strict_trade_through_v1"
BREAK_MARKERS = frozenset({"disconnected", "silence", "subscription_failed", "invalid_frame", "conflict", "out_of_order",
                           "clock_regression", "evidence_loss", "depth_raised"})
ENV_ALLOW = ("PATH", "HOME", "TMPDIR", "LANG", "LC_ALL", "TZ", "SYSTEMROOT")
HOUR_MS = 3_600_000
SAMPLE_EVERY_MS = 300_000
MIN_SPACING_MS = 1_800_000
GAP_SPACING_MS = 5_400_000
LAG_ALERT_MS = 7_200_000
ACCOUNTING_WINDOW_MS = 7 * 86_400_000
FEE_DECIMALS = 12
SLIP_DECIMALS = 8


class QuoteReplayError(ValueError):
    pass


def _canonical(obj) -> bytes:
    return json.dumps(obj, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode("utf-8")


def sha256_hex(blob: bytes) -> str:
    return hashlib.sha256(blob).hexdigest()


def _load_registry():
    path = os.path.join(_ROOT, "shared_strategies", "open", "registry.py")
    spec = importlib.util.spec_from_file_location("_quote_replay_registry", path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def clean_env() -> Dict[str, str]:
    return {k: os.environ[k] for k in ENV_ALLOW if k in os.environ}


def discover(binary: Optional[str] = None) -> dict:
    reg = _load_registry()
    caps = reg.quote_capabilities()
    out = {"protocol": PROTOCOL, "version": VERSION, "capabilities": {}, "spec_sha256": spec_sha256(), "binary": None}
    for name, cap in caps.items():
        out["capabilities"][name] = {k: cap[k] for k in ("quote", "spec_version", "spec_sha256", "platforms", "edge_status",
                                                         "edge_source", "edge_ref", "production_refused", "default_params")}
        out["capabilities"][name]["spec"] = cap["spec"]
    if binary:
        proc = subprocess.run([binary, "quote-replay", "discover"], capture_output=True, text=True, env=clean_env(), timeout=60)
        if proc.returncode != 0:
            raise QuoteReplayError(f"binary discovery failed ({proc.returncode}): {proc.stderr.strip()[-400:]}")
        got = json.loads(proc.stdout)
        out["binary"] = {"spec_sha256": got["spec_sha256"], "strategy": got["strategy"], "queue_model": got["queue_model"]}
        if got["spec_sha256"] != out["spec_sha256"] or got["spec"] != QUOTE_POLICY_SPEC or got["strategy"] != NAME:
            raise QuoteReplayError("the binary and the Python registry carry different quote policy specifications")
    return out


class QuoteTradeObserver:
    def __init__(self, coin: str):
        self.coin = coin
        self.trades: List[dict] = []
        self.invalid: List[Tuple[int, int, str]] = []

    def observe_new_trade(self, start_ms: int, recv_ms: int):
        return None

    def add_trade(self, t: dict, digest: bytes, start_ms: int, recv_ms: int):
        self.trades.append({"recv_ms": recv_ms, "time": t["time"], "tid": t["tid"], "side": t["side"], "px": t["px"],
                            "sz": t["sz"], "hash": t["hash"], "digest": digest.hex()})

    def finalize_minute(self, iv, now_ms: int):
        return None

    def invalidate_minute(self, start_ms: int, at_ms: int, code: str):
        self.invalid.append((start_ms, at_ms, code))

    def trim(self, now_ms: int):
        return None


def load_trades(run_dir: str, coin: str) -> dict:
    observers: Dict[str, QuoteTradeObserver] = {}

    def attach(flows):
        for c, flow in flows.items():
            if flow.profile is not None:
                raise QuoteReplayError("another observer already holds the trade-flow profile slot")
            flow.profile = QuoteTradeObserver(c)
            observers[c] = flow.profile

    loaded = trade_flow_replay.load_trade_recording(run_dir, attach=attach)
    if coin not in observers:
        raise QuoteReplayError(f"the trade recording does not hold {coin}")
    series = loaded["series"][coin]
    complete = sorted(iv["start_ms"] for iv in series["intervals"] if iv["status"] == "complete" and iv["invalid_ms"] is None)
    bad = {s for s, _at, _why in observers[coin].invalid}
    complete = [s for s in complete if s not in bad]
    coverage: List[dict] = []
    run: List[int] = []
    for s in complete + [None]:
        if s is not None and run and s == run[-1] + 60_000:
            run.append(s)
            continue
        if len(run) >= 3:
            coverage.append({"from_ms": run[0] + 60_000, "to_ms": run[-1]})
        run = [s] if s is not None else []
    trades = sorted(observers[coin].trades, key=lambda t: t["recv_ms"])
    seen = {}
    for t in trades:
        if t["tid"] in seen and seen[t["tid"]] != t["digest"]:
            raise QuoteReplayError(f"trade id {t['tid']} appears with two different contents")
        seen[t["tid"]] = t["digest"]
    deduped, kept = [], set()
    for t in trades:
        if t["tid"] in kept:
            continue
        kept.add(t["tid"])
        deduped.append(t)
    return {"trades": deduped, "coverage": coverage, "report": loaded["report"], "series_hash": trade_flow_replay.intervals_sha256(series["intervals"])}


def load_books(run_dir: str, coin: str) -> dict:
    loaded = book_replay.load_book_recording(run_dir)
    if coin not in loaded["series"]:
        raise QuoteReplayError(f"the book recording does not hold {coin}")
    series = loaded["series"][coin]
    books = []
    for o in series["observations"]:
        books.append({"recv_ms": o["recv_ms"], "event_ms": o["event_ms"], "session": o["session"] or 0, "seq": o["seq"],
                      "identity": o["identity"], "bids": [[str(l[0]), str(l[1]), str(l[2])] for l in o["bids"]],
                      "asks": [[str(l[0]), str(l[1]), str(l[2])] for l in o["asks"]]})
    books.sort(key=lambda b: (b["recv_ms"], b["seq"]))
    gaps = [{"from_ms": m["at_ms"], "to_ms": m["at_ms"], "reason": m["reason"]} for m in series["markers"] if m["reason"] in BREAK_MARKERS]
    return {"books": books, "gaps": gaps, "report": loaded["report"], "series_hash": series["series_sha256"]}


def load_recording(run_dir: str, coin: str) -> dict:
    with open(os.path.join(run_dir, "run.manifest.json")) as fh:
        manifest = json.load(fh)
    kinds = set(manifest.get("kinds") or [])
    refusals = []
    if "l2_book" not in kinds:
        raise QuoteReplayError("the recording holds no l2_book depth; the quote replay needs recorded books")
    if "trade_flow" not in kinds:
        refusals.append("the recording holds depth only; archive or depth-only input supplies no raw-trade queue evidence")
    books = load_books(run_dir, coin)
    refusals += [f"book recording: {r}" for r in books["report"]["refusals"]]
    trades = {"trades": [], "coverage": [], "report": {"refusals": []}, "series_hash": ""}
    if "trade_flow" in kinds:
        trades = load_trades(run_dir, coin)
        refusals += [f"trade recording: {r}" for r in trades["report"]["refusals"]]
    with open(os.path.join(run_dir, "run.manifest.json"), "rb") as fh:
        manifest_sha = sha256_hex(fh.read())
    return {"coin": coin, "books": books["books"], "book_gaps": books["gaps"], "trades": trades["trades"],
            "trade_coverage": trades["coverage"], "refusals": refusals, "manifest_sha256": manifest_sha,
            "book_series_sha256": books["series_hash"], "trade_series_sha256": trades["series_hash"],
            "run_id": manifest.get("run_id"), "segments": [s.get("sha256") for s in manifest.get("segments") or []]}


def load_archive_books(archive_dir: str, manifest_path: str, coin: str, days: List[str], work_dir: str) -> dict:
    import book_archive
    with open(manifest_path) as fh:
        manifest = json.load(fh)
    result = book_archive.import_window(archive_dir, work_dir, [coin], days, manifest)
    books, gaps = [], []
    for name in sorted(result["day_files"]):
        if not name.startswith(coin + "_"):
            continue
        day = book_archive.read_day(os.path.join(work_dir, name))
        for o in day["observations"]:
            books.append({"recv_ms": o["recv_ms"], "event_ms": o["event_ms"], "session": o.get("session") or 0, "seq": o["seq"],
                          "identity": o["identity"], "bids": [[str(l[0]), str(l[1]), str(l[2])] for l in o["bids"]],
                          "asks": [[str(l[0]), str(l[1]), str(l[2])] for l in o["asks"]]})
        for m in day["markers"]:
            if m["reason"] in BREAK_MARKERS or m["reason"] == "epoch_start":
                gaps.append({"from_ms": m["at_ms"], "to_ms": m["at_ms"], "reason": m["reason"]})
    books.sort(key=lambda b: (b["recv_ms"], b["seq"]))
    with open(manifest_path, "rb") as fh:
        manifest_sha = sha256_hex(fh.read())
    return {"coin": coin, "books": books, "book_gaps": gaps, "trades": [], "trade_coverage": [],
            "refusals": [f"archive depth only (time basis {book_archive.ARCHIVE_TIME_BASIS}, inferred coverage barriers): it supplies no raw-trade "
                         "queue proof and no measured local delivery latency"],
            "manifest_sha256": manifest_sha, "run_id": "archive:" + ",".join(days), "segments": [],
            "book_series_sha256": "", "trade_series_sha256": ""}


def eligible_windows(rec: dict, cycle_s: int, warmup_ms: int, min_ms: int) -> List[dict]:
    if not rec["books"] or not rec["trade_coverage"]:
        return []
    gaps = sorted(g["from_ms"] for g in rec["book_gaps"])
    out = []
    step = cycle_s * 1000
    for cov in rec["trade_coverage"]:
        start, end = cov["from_ms"], cov["to_ms"]
        cuts = [start] + [g for g in gaps if start < g < end] + [end]
        for a, b in zip(cuts, cuts[1:]):
            first_book = next((bk["recv_ms"] for bk in rec["books"] if bk["recv_ms"] > a), None)
            if first_book is None:
                continue
            first = max(a, first_book) + warmup_ms
            first = -(-first // step) * step
            last = ((b - step) // step) * step
            if last - first >= min_ms:
                out.append({"first_deadline_ms": first, "last_deadline_ms": last})
    return out


def funding_records(path: str, coin: str) -> List[dict]:
    with open(path) as fh:
        doc = json.load(fh)
    rows = doc.get("coins", doc).get(coin) if isinstance(doc, dict) else None
    if rows is None:
        raise QuoteReplayError(f"{path} holds no funding records for {coin}")
    out = []
    for r in rows:
        rate = r.get("rate", r.get("fundingRate"))
        out.append({"time": int(r["time"]), "rate": str(rate)})
    out.sort(key=lambda r: r["time"])
    return out


def build_plan(rec: dict, protocol: dict, window: dict, params: dict, run_id: str, state_dir: str, funding: List[dict],
               costs: Optional[dict] = None, latency: Optional[dict] = None, scored: bool = True, limits: Optional[List[str]] = None,
               source: Optional[dict] = None) -> dict:
    config = protocol["config"]
    full = dict(protocol["base_params"])
    full.update(params)
    validate_run_config(full, config)
    warm = int(full["volatility_window_seconds"]) * 1000 + 1000 + int(full["max_input_age_ms"])
    lo, hi = window["first_deadline_ms"] - warm - 60_000, window["last_deadline_ms"] + 60_000
    books = [b for b in rec["books"] if lo <= b["recv_ms"] <= hi]
    trades = [{k: t[k] for k in ("recv_ms", "time", "tid", "side", "px", "sz", "hash")} for t in rec["trades"] if lo <= t["recv_ms"] <= hi]
    prior = [f for f in funding if f["time"] <= window["first_deadline_ms"]]
    from_ms = prior[-1]["time"] if prior else (funding[0]["time"] if funding else window["first_deadline_ms"])
    recs = [f for f in funding if from_ms <= f["time"] <= window["last_deadline_ms"]]
    n = (window["last_deadline_ms"] - window["first_deadline_ms"]) // (int(config["interval_seconds"]) * 1000) + 1
    all_limits = list(limits or []) + list(rec.get("refusals") or [])
    plan = {
        "protocol": PROTOCOL, "version": VERSION, "mode": "new",
        "run": {"run_id": run_id, "state_dir": state_dir, "source": dict(source or {"recording": rec.get("run_id") or "",
                                                                                     "manifest_sha256": rec.get("manifest_sha256", "")})},
        "coin": rec["coin"],
        "window": {"first_deadline_ms": window["first_deadline_ms"], "last_deadline_ms": window["last_deadline_ms"],
                   "label": window.get("label", ""), "max_deadlines": int(protocol["eligibility"]["max_deadlines_per_run"])},
        "params": full, "config": config, "quote": protocol["quote"], "lot": {"sz_decimals": int(protocol["lot"][rec["coin"]])},
        "costs": costs or protocol["costs"], "latency": latency or protocol["latency"], "queue": protocol["queue"],
        "funding": {"from_ms": from_ms, "records": recs},
        "books": books, "book_gaps": [g for g in rec["book_gaps"] if lo <= g["from_ms"] <= hi],
        "trades": trades, "trade_coverage": rec["trade_coverage"],
        "scoring": {"scored": scored and not all_limits, "limits": all_limits},
    }
    if n > plan["window"]["max_deadlines"]:
        raise QuoteReplayError(f"the window holds {n} deadlines, above the frozen bound {plan['window']['max_deadlines']}")
    return plan


def run_plan(binary: str, plan_path: str, extra: Tuple[str, ...] = (), timeout: int = 3600) -> Tuple[int, dict, str]:
    proc = subprocess.run([binary, "quote-replay", "--spec", plan_path, *extra], capture_output=True, text=True, env=clean_env(), timeout=timeout)
    out = {}
    for line in proc.stdout.splitlines():
        if line.startswith("{"):
            out = json.loads(line)
    return proc.returncode, out, proc.stderr


def _frac(text) -> Fraction:
    if isinstance(text, Fraction):
        return text
    s = str(text)
    return Fraction(s)


def _ceil_to(x: Fraction, decimals: int) -> Fraction:
    scale = 10 ** decimals
    return Fraction(-((-x.numerator * scale) // x.denominator), scale)


def _floor_to(x: Fraction, decimals: int) -> Fraction:
    scale = 10 ** decimals
    return Fraction((x.numerator * scale) // x.denominator, scale)


def _go_decimal_float(f: float) -> Fraction:
    return Fraction(repr(float(f)))


class _FundingCoin:
    def __init__(self, since: int):
        self.anchor = {"qty": 0.0, "pid": "", "since": since, "verified": since}
        self.segments: List[dict] = []
        self.settled = since
        self.last_record = 0
        self.gaps: List[dict] = []
        self.samples: List[Tuple[int, float]] = []
        self.held: List[dict] = []

    def exposure_nonzero(self) -> bool:
        return self.anchor["qty"] != 0 or bool(self.segments)

    def open_gaps(self):
        return [g for g in self.gaps if not g.get("expired")]

    def needs_coverage(self) -> bool:
        return self.exposure_nonzero() or bool(self.open_gaps())

    @staticmethod
    def qty_equal(a: float, b: float) -> bool:
        if a == b:
            return True
        return abs(a - b) <= 1e-12 * max(1.0, max(abs(a), abs(b)))

    def anchor_matches(self, qty: float, pid: str) -> bool:
        return self.qty_equal(self.anchor["qty"], qty) and (qty == 0 or self.anchor["pid"] == pid)

    def append_segment(self, qty, pid, frm, to):
        if to <= frm or qty == 0:
            return
        if self.segments:
            last = self.segments[-1]
            if last["to"] == frm and last["pid"] == pid and self.qty_equal(last["qty"], qty):
                last["to"] = to
                return
        self.segments.append({"qty": qty, "pid": pid, "from": frm, "to": to})

    def move_to(self, qty, pid, at):
        if self.anchor_matches(qty, pid):
            if at > self.anchor["verified"]:
                self.anchor["verified"] = at
            return
        a = self.anchor
        self.append_segment(a["qty"], a["pid"], a["since"], at)
        self.anchor = {"qty": qty, "pid": pid, "since": at, "verified": at}

    def exposure_at(self, t):
        for seg in self.segments:
            if seg["from"] < t <= seg["to"]:
                return seg["qty"], seg["pid"], "known"
        a = self.anchor
        if t > a["verified"]:
            return 0.0, "", "unverified"
        if a["since"] < t:
            return a["qty"], a["pid"], "known"
        return 0.0, "", "known"

    def first_nonzero_after(self, frm, to):
        best = None

        def consider(start, end):
            nonlocal best
            if not (start < to and frm < end):
                return
            start = max(start, frm)
            if best is None or start < best:
                best = start

        for seg in self.segments:
            consider(seg["from"], seg["to"])
        if self.anchor["qty"] != 0:
            consider(self.anchor["since"], 2 ** 63 - 1)
        return best

    def maybe_sample(self, px: float, now: int):
        if not (px > 0) or math.isinf(px):
            return
        if self.samples:
            last = self.samples[-1][0]
            hour_start = now - now % HOUR_MS
            if last >= now or (last >= hour_start and now - last < SAMPLE_EVERY_MS):
                return
        self.samples.append((now, px))

    def exposure_nonzero_in(self, frm, to):
        if to <= frm:
            return False
        for seg in self.segments:
            if seg["from"] < to and frm < seg["to"]:
                return True
        return self.anchor["qty"] != 0 and to > self.anchor["since"]

    def sample_at_or_after(self, t):
        for at, px in self.samples:
            if at >= t:
                return at, px
        return None


class IndependentLedger:
    def __init__(self, initial_cash: Fraction):
        self.qty = Fraction(0)
        self.cost = Fraction(0)
        self.cash = initial_cash
        self.realized = Fraction(0)
        self.fees = Fraction(0)
        self.funding = Fraction(0)
        self.pid = ""
        self.pid_seq = 0
        self.first_ms = 0
        self.round_trips: List[dict] = []
        self.open_trip: Optional[dict] = None

    def _open(self, side: int, qty: Fraction, px: Fraction, fee: Fraction, t: int, add: bool):
        if not add:
            self.pid_seq += 1
            self.pid = f"p{self.pid_seq}"
            self.first_ms = t
            self.cost = qty * px
            self.qty = qty if side > 0 else -qty
            self.open_trip = {"pid": self.pid, "opened_ms": t, "net": Fraction(0)}
        else:
            self.cost += qty * px
            self.qty += qty if side > 0 else -qty
        self.cash -= fee
        self.fees += fee
        if self.open_trip is not None:
            self.open_trip["net"] -= fee

    def apply(self, side: str, size: Fraction, px: Fraction, fee: Fraction, t: int):
        d = -1 if side == "sell" else 1
        sign = (self.qty > 0) - (self.qty < 0)
        if sign == 0 or sign == d:
            self._open(d, size, px, fee, t, sign != 0)
            return
        absq = abs(self.qty)
        reduce = min(size, absq)
        residual = size - reduce
        close_fee = fee * reduce / size
        open_fee = fee - close_fee
        avg = self.cost / absq
        gross = reduce * (px - avg)
        if sign < 0:
            gross = -gross
        self.cost -= avg * reduce
        self.qty += reduce if d > 0 else -reduce
        self.cash += gross - close_fee
        self.realized += gross
        self.fees += close_fee
        if self.open_trip is not None:
            self.open_trip["net"] += gross - close_fee
        if self.qty == 0:
            self.cost = Fraction(0)
            if self.open_trip is not None:
                self.open_trip["closed_ms"] = t
                self.round_trips.append(self.open_trip)
                self.open_trip = None
            self.pid = ""
            self.first_ms = 0
        if residual > 0:
            self._open(d, residual, px, open_fee, t, False)

    def book(self) -> dict:
        return {"qty": self.qty, "cost": self.cost, "cash": self.cash, "realized_gross": self.realized, "fees": self.fees, "funding": self.funding}


def _events(out: dict) -> List[dict]:
    evs = []
    for e in out.get("venue_events") or []:
        evs.append({"seq": e["seq"], "kind": e["kind"], "cid": e.get("client_order_id", ""), "effect_ms": e["effect_ms"], "event": e["event"]})
    return evs


def verify(plan: dict, out: dict) -> dict:
    problems: List[str] = []
    trades = {t["tid"]: t for t in plan["trades"]}
    books = plan["books"]
    book_recv = [b["recv_ms"] for b in books]
    participation = Fraction(plan["queue"]["participation"])
    maker_rate = _frac(plan["costs"]["maker_fee_bps"]) / 10000
    taker_rate = _frac(plan["costs"]["taker_fee_bps"]) / 10000
    slip = _frac(plan["costs"]["slippage_reserve_bps"]) / 10000
    sz_dec = int(plan["lot"]["sz_decimals"])
    orders: Dict[str, dict] = {}
    used: Dict[int, Fraction] = {}
    position = Fraction(0)
    fills: List[dict] = []
    keys = set()
    for e in _events(out):
        ev = e["event"]
        if e["kind"] == "place":
            req, outcome = ev["request"], ev["outcome"]
            if e["cid"].lower() in orders:
                problems.append(f"client id {e['cid']} was sent twice")
            orders[e["cid"].lower()] = {"req": req, "accepted": outcome.get("accepted_at_ms", 0) if outcome["status"] == "resting" else 0,
                                        "status": outcome["status"], "ended": None, "filled": Fraction(0)}
            continue
        if e["kind"] in ("cancel", "venue_cancel"):
            o = orders.get(e["cid"].lower())
            if o is not None and o["ended"] is None and ev.get("outcome", {}).get("status") == "cancelled":
                o["ended"] = e["effect_ms"]
            continue
        if e["kind"] != "fill":
            continue
        f = ev["fill"]
        key = (f["order_id"], f["trade_id"])
        if key in keys:
            problems.append(f"fill {key} repeats")
        keys.add(key)
        o = orders.get(f["client_order_id"].lower())
        if o is None:
            problems.append(f"fill {key} names no placed order")
            continue
        req = o["req"]
        size, px, fee = _frac(f["size"]), _frac(f["px"]), _frac(f["fee"])
        if o["status"] != "resting":
            problems.append(f"fill {key} belongs to an order the venue did not accept ({o['status']})")
        if o["ended"] is not None and f["time_ms"] > o["ended"]:
            problems.append(f"fill {key} at {f['time_ms']} is after its order ended at {o['ended']}")
        o["filled"] += size
        if o["filled"] > _frac(req["size"]):
            problems.append(f"fill {key} takes the order past its size {req['size']}")
        if _frac(f["start_position"]) != position:
            problems.append(f"fill {key} starts at {f['start_position']}, the independent position is {position}")
        before = position
        position += size if f["side"] == "buy" else -size
        if req.get("reduce_only") and (before == 0 or (position != 0 and (position > 0) != (before > 0))):
            problems.append(f"reduce-only fill {key} crossed flat")
        if _floor_to(size, sz_dec) != size or size <= 0:
            problems.append(f"fill {key} size {f['size']} is not on the lot grid")
        if ev.get("liquidity") == "maker":
            tr = trades.get(ev.get("source_tid"))
            if tr is None:
                problems.append(f"maker fill {key} names trade {ev.get('source_tid')}, which the plan does not hold")
                continue
            if tr["recv_ms"] != f["time_ms"] or tr["recv_ms"] <= o["accepted"]:
                problems.append(f"maker fill {key} at {f['time_ms']} is not at a receipt strictly after acceptance {o['accepted']}")
            tpx = _frac(tr["px"])
            through = (f["side"] == "buy" and tr["side"] == "A" and tpx < px) or (f["side"] == "sell" and tr["side"] == "B" and tpx > px)
            if not through or px != _frac(req["px"]):
                problems.append(f"maker fill {key} has no strict opposing trade-through at its own price")
            used[tr["tid"]] = used.get(tr["tid"], Fraction(0)) + size
            budget = _floor_to(_frac(tr["sz"]) * participation, sz_dec)
            if used[tr["tid"]] > budget:
                problems.append(f"trade {tr['tid']} supplied {used[tr['tid']]} to own orders, above its {budget} participation budget")
            want_fee = _ceil_to(size * px * maker_rate, FEE_DECIMALS)
        else:
            i = bisect.bisect_right(book_recv, f["time_ms"]) - 1
            levels = (books[i]["bids"] if f["side"] == "sell" else books[i]["asks"]) if i >= 0 else []
            limit = _frac(req["px"]) if req.get("px") else None
            ok = False
            for lv in levels:
                lp = _frac(lv[0])
                adj = _floor_to(lp * (1 - slip), SLIP_DECIMALS) if f["side"] == "sell" else _ceil_to(lp * (1 + slip), SLIP_DECIMALS)
                if limit is not None and ((f["side"] == "sell" and adj < limit) or (f["side"] == "buy" and adj > limit)):
                    adj = limit
                if adj == px:
                    ok = True
            if not ok:
                problems.append(f"taker fill {key} price {f['px']} is not a recorded level less the slippage reserve")
            want_fee = _ceil_to(size * px * taker_rate, FEE_DECIMALS)
        if fee != want_fee:
            problems.append(f"fill {key} fee {f['fee']} differs from the frozen rate ({want_fee})")
        fills.append(f)
    boundary = int(out.get("applied_boundary_ms") or 0)
    apply_through = boundary + 1
    ledger = IndependentLedger(Fraction(int(QUOTE_POLICY_SPEC["fixed"]["capital"])))
    applied, pending = [], []
    for f in fills:
        (applied if f["time_ms"] <= apply_through else pending).append(f)
    funding = mirror_funding(plan, out, applied)
    pay_iter = sorted(funding["payments"], key=lambda p: p["time"])
    pi = 0
    for f in applied:
        while pi < len(pay_iter) and pay_iter[pi]["time"] <= f["time_ms"]:
            amt = pay_iter[pi]["amount"]
            ledger.funding += amt
            ledger.cash += amt
            if ledger.open_trip is not None:
                ledger.open_trip["net"] += amt
            pi += 1
        ledger.apply(f["side"], _frac(f["size"]), _frac(f["px"]), _frac(f["fee"]), f["time_ms"])
    while pi < len(pay_iter):
        amt = pay_iter[pi]["amount"]
        ledger.funding += amt
        ledger.cash += amt
        if ledger.open_trip is not None:
            ledger.open_trip["net"] += amt
        pi += 1
    go_book = out.get("book") or {}
    mine = ledger.book()
    for k in ("qty", "cost", "cash", "realized_gross", "fees", "funding"):
        if k not in go_book:
            problems.append(f"the Go book has no {k}")
            continue
        if _frac(go_book[k]) != mine[k]:
            problems.append(f"independent {k} {mine[k]} differs from the Go settlement {go_book[k]}")
    go_funding = sorted((t["time_ms"], _go_decimal_float(t["realized_pnl"])) for t in out.get("trades") or [] if t["trade_type"] == "funding")
    py_funding = sorted((p["time"], p["amount"]) for p in funding["payments"])
    if go_funding != py_funding:
        problems.append(f"independent funding payments {py_funding} differ from the Go payments {go_funding}")
    problems += funding["problems"]
    go_positions = sorted({t["position_id"] for t in out.get("trades") or [] if t["trade_type"] not in ("funding", "fee_adjustment") and t["position_id"]})
    opened = len(ledger.round_trips) + (1 if ledger.open_trip is not None else 0)
    if opened != len(go_positions):
        problems.append(f"the independent ledger opened {opened} position identities, the Go trades hold {len(go_positions)}")
    return {"ok": not problems, "problems": problems, "book": {k: str(v) for k, v in mine.items()}, "applied_fills": len(applied),
            "pending_fills": [{"time_ms": f["time_ms"], "side": f["side"], "size": f["size"], "px": f["px"]} for f in pending],
            "pending_funding": funding["pending"], "round_trips": [{"pid": r["pid"], "opened_ms": r["opened_ms"], "closed_ms": r["closed_ms"],
                                                                  "net": str(r["net"])} for r in ledger.round_trips],
            "open_trip": None if ledger.open_trip is None else {"opened_ms": ledger.open_trip["opened_ms"], "net": str(ledger.open_trip["net"])},
            "funding_payments": [{"time": p["time"], "amount": str(p["amount"])} for p in funding["payments"]]}


def mirror_funding(plan: dict, out: dict, applied: List[dict]) -> dict:
    problems: List[str] = []
    results = sorted((r for r in out.get("results") or []), key=lambda r: (r["deadline_ms"], r["attempt"]))
    decisions = {}
    for raw in out.get("decisions") or []:
        decisions[raw["deadline_ms"]] = raw
    records = [{"time": int(r["time"]), "rate": float(Fraction(r["rate"]))} for r in plan["funding"]["records"]]
    cov_from = int(plan["funding"]["from_ms"])
    payments: List[dict] = []
    if not results:
        return {"payments": payments, "problems": problems, "pending": []}
    first = results[0]["deadline_ms"]
    observed = first
    coin: Optional[_FundingCoin] = _FundingCoin(first)
    qty_exact = Fraction(0)
    pid = ""
    pid_seq = 0
    fi = 0
    seen = set()
    for r in results:
        d = r["deadline_ms"]
        if (d, r["attempt"]) in seen:
            continue
        seen.add((d, r["attempt"]))
        summary = r.get("summary") or {}
        boundary = int(summary.get("applied_boundary_ms") or 0)
        if boundary > 0:
            through = boundary + 1
            while fi < len(applied) and applied[fi]["time_ms"] <= through:
                f = applied[fi]
                at = f["time_ms"]
                if coin is None:
                    if qty_exact != 0:
                        problems.append(f"fill at {at} moves inventory with no funding state")
                    verified = min(max(observed, first), at)
                    coin = _FundingCoin(verified)
                coin.anchor["verified"] = at
                size = Fraction(f["size"])
                before = qty_exact
                qty_exact += size if f["side"] == "buy" else -size
                if qty_exact == 0:
                    pid = ""
                elif before == 0 or (before > 0) != (qty_exact > 0):
                    pid_seq += 1
                    pid = f"p{pid_seq}"
                q = float(abs(qty_exact)) * (1 if qty_exact > 0 else -1) if qty_exact != 0 else 0.0
                coin.move_to(q, pid, at)
                fi += 1
            q = float(abs(qty_exact)) * (1 if qty_exact > 0 else -1) if qty_exact != 0 else 0.0
            if coin is not None and coin.anchor_matches(q, pid) and boundary > coin.anchor["verified"]:
                coin.anchor["verified"] = boundary
            observed = max(observed, boundary)
        if coin is None:
            continue
        discard = summary.get("settlement_kind") == "none"
        saved_coin, saved_problems = copy.deepcopy(coin), len(problems)
        rec = decisions.get(d) or {}
        mark = rec.get("mark") or {}
        px = 0.0
        if mark and mark.get("evidence_ms", d + 1) <= d and d - mark.get("evidence_ms", 0) <= mark.get("max_age_ms", -1) and mark.get("price", 0) > 0:
            px = float(mark["price"])
        if coin.exposure_nonzero() or coin.open_gaps():
            coin.maybe_sample(px, d)
        cov_records = [x for x in records if cov_from <= x["time"] <= d]
        cov_to = cov_records[-1]["time"] if cov_records else 0
        backlog = False
        if coin.needs_coverage() and cov_from > coin.settled + 1:
            if coin.exposure_nonzero_in(coin.settled, cov_from - 1):
                backlog = True
            else:
                coin.settled = cov_from - 1
        if backlog:
            pass
        elif coin.needs_coverage():
            limit = min(cov_to, coin.anchor["verified"])
            stopped = False
            for x in cov_records:
                if x["time"] <= coin.settled:
                    continue
                if x["time"] > limit:
                    break
                prev = coin.last_record
                if prev > 0 and x["time"] > prev and x["time"] - prev < MIN_SPACING_MS:
                    problems.append(f"funding records at {prev} and {x['time']} are less than 30 minutes apart")
                    stopped = True
                    break
                ref = prev if prev > 0 else coin.settled
                start = coin.first_nonzero_after(ref, x["time"] - 1)
                if start is not None and x["time"] - start > GAP_SPACING_MS:
                    coin.gaps.append({"after": start, "before": x["time"]})
                    problems.append(f"funding gap between {start} and {x['time']} while a position was held")
                q, qpid, kind = coin.exposure_at(x["time"])
                if kind == "unverified":
                    stopped = True
                    break
                if q != 0 and x["rate"] != 0:
                    smp = coin.sample_at_or_after(x["time"])
                    if smp is None:
                        stopped = True
                        break
                    payment = -q * smp[1] * x["rate"]
                    if not math.isfinite(payment):
                        problems.append(f"funding at {x['time']} is not finite")
                    else:
                        payments.append({"time": x["time"], "amount": _go_decimal_float(payment)})
                coin.settled = x["time"]
                coin.last_record = max(coin.last_record, x["time"])
            if not stopped and limit > coin.settled:
                coin.settled = limit
        elif coin.anchor["verified"] > coin.settled:
            coin.settled = coin.anchor["verified"]
        for g in coin.gaps:
            if not g.get("expired") and d - g["before"] > ACCOUNTING_WINDOW_MS:
                g["expired"] = True
        cutoff = coin.settled
        for g in coin.open_gaps():
            cutoff = min(cutoff, g["after"])
        coin.segments = [s for s in coin.segments if s["to"] > cutoff]
        coin.samples = [s for s in coin.samples if s[0] > cutoff]
        if discard:
            coin = saved_coin
            del problems[saved_problems:]
            continue
        if qty_exact == 0 and coin.anchor["qty"] == 0 and not coin.segments and not coin.held and not coin.gaps:
            coin = None
    pending = []
    if coin is not None:
        pending = [{"time": x["time"], "rate": x["rate"]} for x in records if x["time"] > coin.settled]
    return {"payments": payments, "problems": problems, "pending": pending}


def _write(path: str, obj) -> None:
    with open(path, "w") as fh:
        json.dump(obj, fh, sort_keys=True)
        fh.write("\n")


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description="Offline quote replay driver for the production-refused inventory_market_making candidate (#1861)")
    sub = ap.add_subparsers(dest="cmd", required=True)
    d = sub.add_parser("discover")
    d.add_argument("--binary")
    p = sub.add_parser("plan")
    p.add_argument("--recording", required=True)
    p.add_argument("--coin", required=True)
    p.add_argument("--protocol", required=True)
    p.add_argument("--funding", required=True)
    p.add_argument("--first-deadline-ms", type=int)
    p.add_argument("--last-deadline-ms", type=int)
    p.add_argument("--run-id", required=True)
    p.add_argument("--state-dir", required=True)
    p.add_argument("--out", required=True)
    r = sub.add_parser("run")
    r.add_argument("--binary", required=True)
    r.add_argument("--plan", required=True)
    r.add_argument("--out", required=True)
    v = sub.add_parser("verify")
    v.add_argument("--plan", required=True)
    v.add_argument("--output", required=True)
    args = ap.parse_args(argv)
    try:
        if args.cmd == "discover":
            print(json.dumps(discover(args.binary), sort_keys=True))
            return 0
        if args.cmd == "plan":
            with open(args.protocol) as fh:
                protocol = json.load(fh)
            rec = load_recording(args.recording, args.coin)
            cycle = int(protocol["config"]["interval_seconds"])
            warm = int(protocol["base_params"]["volatility_window_seconds"]) * 1000 + 1000 + int(protocol["base_params"]["max_input_age_ms"])
            if args.first_deadline_ms is None or args.last_deadline_ms is None:
                wins = eligible_windows(rec, cycle, warm, int(protocol["eligibility"]["min_window_minutes"]) * 60_000)
                if not wins:
                    raise QuoteReplayError("the recording holds no eligible window with continuous book and raw-trade coverage")
                window = wins[0]
            else:
                window = {"first_deadline_ms": args.first_deadline_ms, "last_deadline_ms": args.last_deadline_ms}
            plan = build_plan(rec, protocol, window, {}, args.run_id, os.path.abspath(args.state_dir), funding_records(args.funding, args.coin))
            _write(args.out, plan)
            print(json.dumps({"plan": args.out, "window": plan["window"], "books": len(plan["books"]), "trades": len(plan["trades"]),
                              "scored": plan["scoring"]["scored"], "limits": plan["scoring"]["limits"]}, sort_keys=True))
            return 0
        if args.cmd == "run":
            code, out, err = run_plan(args.binary, args.plan)
            _write(args.out, out)
            sys.stderr.write(err[-2000:])
            print(json.dumps({"exit": code, "status": out.get("status"), "scored": out.get("scored")}, sort_keys=True))
            return 0 if code == 0 else code
        if args.cmd == "verify":
            with open(args.plan) as fh:
                plan = json.load(fh)
            with open(args.output) as fh:
                out = json.load(fh)
            rep = verify(plan, out)
            print(json.dumps(rep, sort_keys=True))
            return 0 if rep["ok"] else 1
    except (QuoteReplayError, book_replay.BookRecordingError, trade_flow_replay.TradeRecordingError, ValueError) as exc:
        print(json.dumps({"error": str(exc)}), file=sys.stderr)
        return 1
    return 2


if __name__ == "__main__":
    sys.exit(main())
