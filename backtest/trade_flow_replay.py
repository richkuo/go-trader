import gzip
import hashlib
import io
import json
import os
import re
from typing import Dict, List, Optional

RUN_SCHEMA_V2 = "go_trader_observation_run/v2"
SEGMENT_SCHEMA_V2 = "go_trader_observation_segment/v2"
RECORDING_SCHEMA_V2 = "go_trader_observation_recording/v2"
SERIES_SCHEMA = "go_trader_trade_flow_series/v1"
KIND = "trade_flow"

CONTRACT = {
    "source": "hyperliquid_ws_trades",
    "units": "base",
    "time_basis": "event_time_receipt_cutoff",
    "identity": "venue+namespace+coin+time+tid",
    "side_evidence": "hyperliquid-trades-side-aggressor/1638-v1",
    "interval_ms": 60_000,
    "reorder_grace_ms": 5_000,
    "max_event_gap_ms": 30_000,
    "max_future_skew_ms": 5_000,
    "identity_retain_ms": 900_000,
    "identity_cap": 400_000,
    "decimal_scale": 8,
}
PAYLOAD_CONTRACT_KEYS = ("source", "units", "time_basis", "identity", "side_evidence", "interval_ms",
                         "reorder_grace_ms", "max_event_gap_ms", "decimal_scale")
RECORDER_WINDOW_MS = 2 * 3_600_000
RETAIN_SLACK_MS = 10 * 60_000
MAX_WINDOW_MS = 26 * 3_600_000
MAX_INTERVALS = (MAX_WINDOW_MS + RETAIN_SLACK_MS) // 60_000
SCALE = 10 ** 8
INT64_MAX = (1 << 63) - 1
HASH_MOD = 1 << 256
_NUMBER_RE = re.compile(r"^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?$")


class TradeRecordingError(ValueError):
    pass


def sha256_bytes(blob: bytes) -> str:
    return hashlib.sha256(blob).hexdigest()


def canonical_decimal(raw):
    if not isinstance(raw, str):
        raise ValueError("decimal is not a string")
    s = raw.strip()
    if not s:
        raise ValueError("empty decimal")
    whole, dot, frac = s.partition(".")
    if whole == "" or (dot and frac == ""):
        raise ValueError(f"decimal {raw!r} is malformed")
    if not (whole + frac).isdigit() or not (whole + frac).isascii():
        raise ValueError(f"decimal {raw!r} is not a plain nonnegative decimal")
    whole = whole.lstrip("0") or "0"
    frac = frac.rstrip("0")
    return whole + ("." + frac if frac else "")


def decimal_units(canonical: str) -> int:
    whole, _, frac = canonical.partition(".")
    if len(frac) > CONTRACT["decimal_scale"]:
        raise ValueError(f"size {canonical} has more than {CONTRACT['decimal_scale']} decimal places")
    if len(whole) > 10:
        raise ValueError(f"size {canonical} is too large for the trade-volume accumulator")
    return int(whole) * SCALE + int((frac + "0" * CONTRACT["decimal_scale"])[:CONTRACT["decimal_scale"]])


def format_units(units: int) -> str:
    if units < 0:
        return "-" + format_units(-units)
    whole, part = divmod(units, SCALE)
    if part == 0:
        return str(whole)
    return f"{whole}." + f"{part:0{CONTRACT['decimal_scale']}d}".rstrip("0")


def interval_start(ms: int) -> int:
    step = CONTRACT["interval_ms"]
    return ms - ((ms % step) + step) % step


def _number_text(value):
    if isinstance(value, bool):
        return None
    if isinstance(value, int):
        return str(value)
    if isinstance(value, float):
        return repr(value)
    if isinstance(value, str) and _NUMBER_RE.match(value):
        return value
    return None


def parse_trade(elem) -> dict:
    out = {"coin": "", "tid": 0, "err": None, "trade": None}
    if not isinstance(elem, dict):
        out["err"] = "decode trade: not an object"
        return out
    for key in ("coin", "side", "px", "sz", "hash"):
        if key in elem and elem[key] is not None and not isinstance(elem[key], str):
            out["err"] = f"decode trade: {key} is not a string"
            return out
    texts = {}
    for key in ("time", "tid"):
        if key in elem and elem[key] is not None:
            text = _number_text(elem[key])
            if text is None:
                out["err"] = f"decode trade: {key} is not a number"
                return out
            texts[key] = text
        else:
            texts[key] = ""
    tid_text = texts["tid"]
    tid_ok = tid_text.isdigit() and int(tid_text) < (1 << 64)
    out["tid"] = int(tid_text) if tid_ok else 0
    coin = (elem.get("coin") or "").strip()
    if not coin:
        out["err"] = "trade carries no coin"
        return out
    out["coin"] = coin
    if not tid_ok:
        out["err"] = f"trade tid {tid_text!r} is not an unsigned integer"
        return out
    time_text = texts["time"]
    if not re.match(r"^[+-]?[0-9]+$", time_text) or int(time_text) <= 0 or int(time_text) > INT64_MAX:
        out["err"] = f"trade time {time_text!r} is not a positive millisecond timestamp"
        return out
    side = elem.get("side") or ""
    if side not in ("B", "A"):
        out["err"] = f"trade side {side!r} is not B or A"
        return out
    try:
        px = canonical_decimal(elem.get("px") or "")
        pxv = float(px)
        if not (pxv > 0 and pxv != float("inf")):
            raise ValueError("price")
    except ValueError as exc:
        out["err"] = f"trade px: {exc}"
        return out
    try:
        sz = canonical_decimal(elem.get("sz") or "")
        units = decimal_units(sz)
    except ValueError as exc:
        out["err"] = f"trade sz: {exc}"
        return out
    if units <= 0:
        out["err"] = "trade sz is not positive"
        return out
    users = elem.get("users")
    if not isinstance(users, list) or len(users) != 2 or not all(isinstance(u, str) for u in users):
        out["err"] = "trade users is not a [buyer, seller] pair"
        return out
    out["trade"] = {
        "coin": coin, "tid": int(tid_text), "time": int(time_text), "side": side, "px": px, "sz": sz,
        "units": units, "hash": (elem.get("hash") or "").strip().lower(),
        "buyer": users[0].strip().lower(), "seller": users[1].strip().lower(),
    }
    return out


def trade_canonical(t: dict) -> str:
    return "|".join([t["coin"], str(t["tid"]), str(t["time"]), t["side"], t["px"], t["sz"], t["hash"],
                     t["buyer"], t["seller"]])


def trade_digest(t: dict) -> bytes:
    return hashlib.sha256(trade_canonical(t).encode("utf-8")).digest()


class _Interval:
    __slots__ = ("start", "buy", "sell", "trades", "hash", "first", "last", "session", "status", "reason",
                 "finalized", "invalid")

    def __init__(self, start: int, session: int):
        self.start = start
        self.buy = 0
        self.sell = 0
        self.trades = 0
        self.hash = 0
        self.first = 0
        self.last = 0
        self.session = session
        self.status = "open"
        self.reason = ""
        self.finalized = 0
        self.invalid = 0

    def invalidate(self, at_ms: int, reason: str) -> bool:
        if self.invalid != 0:
            return False
        self.invalid = at_ms
        self.reason = reason
        return True


class _Flow:
    def __init__(self, coin: str, window_ms: int):
        self.coin = coin
        self.window = window_ms
        self.intervals: Dict[int, _Interval] = {}
        self.gaps: List[list] = []
        self.cover_from = 0
        self.frontier = 0
        self.session = 0
        self.last_frame = 0
        self.last_recv = 0
        self.ident_floor = 0
        self.resumable = False
        self.idents: Dict[int, tuple] = {}
        self.ident_order: List[int] = []
        self.stats = {k: 0 for k in ("frames", "received", "accepted", "duplicates", "conflicts", "late",
                                     "uncovered", "rejected", "finalized", "invalid")}
        self.history: Dict[int, _Interval] = {}
        self.latency: List[int] = []
        self.profile = None

    def sorted_intervals(self):
        return [self.intervals[k] for k in sorted(self.intervals)]

    def ensure(self, start: int) -> _Interval:
        iv = self.intervals.get(start)
        if iv is None:
            iv = _Interval(start, self.session)
            self.intervals[start] = iv
        return iv

    def open_gap(self):
        if self.gaps and self.gaps[-1][1] == 0:
            return self.gaps[-1]
        return None

    def event_interval(self, iv: _Interval) -> dict:
        return {"k": "interval", "coin": self.coin, "kind": KIND, "start_ms": iv.start, "status": iv.status,
                "buy": format_units(iv.buy), "sell": format_units(iv.sell), "trades": iv.trades,
                "hash": f"{iv.hash:064x}", "session": iv.session, "finalized_ms": iv.finalized,
                "invalid_ms": iv.invalid, "reason": iv.reason, "recv_ms": iv.finalized}

    def stop(self, at_ms: int, reason: str, ev: list):
        self.resumable = False
        if self.cover_from == 0:
            return
        for iv in self.sorted_intervals():
            if iv.start < self.frontier or iv.status != "open":
                continue
            iv.invalidate(at_ms, reason)
        start = self.frontier
        while start < at_ms and start - self.frontier < MAX_WINDOW_MS:
            self.ensure(start).invalidate(at_ms, reason)
            start += CONTRACT["interval_ms"]
        for iv in self.sorted_intervals():
            if iv.status != "open":
                continue
            iv.status = "invalid"
            iv.finalized = at_ms
            self.stats["finalized"] += 1
            self.stats["invalid"] += 1
            self.history[iv.start] = iv
            ev.append(self.event_interval(iv))
            if self.profile is not None:
                self.profile.finalize_minute(iv, at_ms)
        gap_start = self.last_frame
        if gap_start == 0 or gap_start > at_ms:
            gap_start = at_ms
        if self.open_gap() is None:
            self.gaps.append([gap_start, 0, at_ms, reason])
        ev.append({"k": "cover", "coin": self.coin, "kind": KIND, "state": "stop", "reason": reason,
                   "recv_ms": at_ms, "frontier_ms": self.frontier})
        self.cover_from = 0
        self.frontier = 0

    def start(self, recv_ms: int, session: int, ev: list):
        if self.cover_from != 0 and self.session == session:
            return
        if self.cover_from != 0:
            self.stop(recv_ms, "disconnected", ev)
        self.session = session
        self.begin(recv_ms, "subscribed", ev)

    def begin(self, recv_ms: int, reason: str, ev: list):
        self.resumable = False
        self.cover_from = interval_start(recv_ms) + CONTRACT["interval_ms"]
        self.frontier = self.cover_from
        self.last_frame = recv_ms
        gap = self.open_gap()
        if gap is not None:
            gap[1] = self.cover_from
        ev.append({"k": "cover", "coin": self.coin, "kind": KIND, "state": "start", "reason": reason,
                   "from_ms": self.cover_from, "recv_ms": recv_ms, "session": self.session})

    def stop_on_silence(self, now_ms: int, ev: list) -> bool:
        if self.cover_from == 0 or self.last_frame <= 0 or now_ms - self.last_frame <= CONTRACT["max_event_gap_ms"]:
            return False
        self.stop(now_ms, "silence", ev)
        self.resumable = True
        return True

    def advance(self, now_ms: int, ev: list):
        if self.cover_from == 0 or self.stop_on_silence(now_ms, ev):
            return
        for _ in range(MAX_INTERVALS):
            end = self.frontier + CONTRACT["interval_ms"]
            if end + CONTRACT["reorder_grace_ms"] > now_ms or self.last_frame < end:
                break
            iv = self.ensure(self.frontier)
            iv.finalized = now_ms
            if iv.invalid != 0:
                iv.status = "invalid"
                self.stats["invalid"] += 1
            else:
                iv.status = "complete"
            iv.session = self.session
            self.stats["finalized"] += 1
            self.history[iv.start] = iv
            ev.append(self.event_interval(iv))
            if self.profile is not None:
                self.profile.finalize_minute(iv, now_ms)
            self.frontier = end
        self.trim(now_ms)

    def trim(self, now_ms: int):
        floor = now_ms - (self.window + RETAIN_SLACK_MS)
        for start in [k for k in self.intervals if k < floor]:
            del self.intervals[start]
        keys = sorted(self.intervals)
        for start in keys[:max(0, len(keys) - MAX_INTERVALS)]:
            del self.intervals[start]
        self.gaps = [g for g in self.gaps if not (g[1] != 0 and g[1] < floor)]
        ident_floor = now_ms - CONTRACT["identity_retain_ms"]
        n = 0
        while n < len(self.ident_order):
            ik = self.ident_order[n]
            ident = self.idents.get(ik)
            if ident is not None and ident[2] >= ident_floor and len(self.idents) <= CONTRACT["identity_cap"]:
                break
            if ident is not None:
                if len(self.idents) > CONTRACT["identity_cap"] and ident[1] + CONTRACT["interval_ms"] > self.ident_floor:
                    self.ident_floor = ident[1] + CONTRACT["interval_ms"]
                del self.idents[ik]
            n += 1
        if n:
            self.ident_order = self.ident_order[n:]
        if self.profile is not None:
            self.profile.trim(now_ms)

    def invalidate_open(self, at_ms: int, reason: str):
        if self.cover_from == 0:
            return
        start = self.frontier
        while start <= interval_start(at_ms):
            self.ensure(start).invalidate(at_ms, reason)
            start += CONTRACT["interval_ms"]
        for iv in self.sorted_intervals():
            if iv.status == "open":
                iv.invalidate(at_ms, reason)
        if not self.gaps or self.gaps[-1][3] != reason or self.gaps[-1][2] != at_ms:
            self.gaps.append([at_ms, at_ms, at_ms, reason])

    def ingest(self, t: dict, recv_ms: int, ev: list) -> dict:
        self.stats["received"] += 1
        start = interval_start(t["time"])
        out = {"tid": t["tid"], "o": "", "i": start}
        digest = trade_digest(t)
        ik = (t["time"], t["tid"])
        prior = self.idents.get(ik)
        if prior is not None:
            if prior[0] == digest:
                self.stats["duplicates"] += 1
                out["o"] = "duplicate"
                return out
            self.stats["conflicts"] += 1
            out["o"] = "conflict"
            out["why"] = "same time and tid with different content"
            for st in (prior[1], start):
                if self.profile is not None:
                    self.profile.invalidate_minute(st, recv_ms, "conflicting_duplicate")
                iv = self.intervals.get(st)
                if iv is not None:
                    if iv.invalidate(recv_ms, "conflicting_duplicate") and iv.status != "open":
                        self.stats["invalid"] += 1
                        ev.append({"k": "invalidate", "coin": self.coin, "kind": KIND, "start_ms": st,
                                   "reason": "conflicting_duplicate", "recv_ms": recv_ms})
                elif self.cover_from != 0 and st >= self.frontier:
                    self.ensure(st).invalidate(recv_ms, "conflicting_duplicate")
            return out
        if t["time"] > recv_ms + CONTRACT["max_future_skew_ms"]:
            self.stats["rejected"] += 1
            out["o"] = "rejected"
            out["why"] = "future_event"
            self.invalidate_open(recv_ms, "future_event")
            return out
        self.idents[ik] = (digest, start, recv_ms)
        self.ident_order.append(ik)
        if len(self.idents) > CONTRACT["identity_cap"]:
            self.invalidate_open(recv_ms, "identity_overflow")
        if self.profile is not None:
            self.profile.observe_new_trade(start, recv_ms)
        existing = self.intervals.get(start)
        if existing is not None and existing.status != "open":
            self.stats["late"] += 1
            out["o"] = "late"
            if existing.invalidate(recv_ms, "late_event"):
                self.stats["invalid"] += 1
                ev.append({"k": "invalidate", "coin": self.coin, "kind": KIND, "start_ms": start,
                           "reason": "late_event", "recv_ms": recv_ms})
            return out
        if self.cover_from == 0 or start < self.cover_from:
            self.stats["uncovered"] += 1
            out["o"] = "uncovered"
            return out
        if start < self.frontier:
            self.stats["late"] += 1
            out["o"] = "late"
            out["why"] = "interval_evicted"
            return out
        iv = self.ensure(start)
        if t["side"] == "B":
            iv.buy += t["units"]
            ok = iv.buy <= INT64_MAX
        else:
            iv.sell += t["units"]
            ok = iv.sell <= INT64_MAX
        if not ok:
            iv.invalidate(recv_ms, "volume_overflow")
        iv.hash = (iv.hash + int.from_bytes(digest, "big")) % HASH_MOD
        iv.trades += 1
        if iv.first == 0:
            iv.first = recv_ms
        iv.last = recv_ms
        self.stats["accepted"] += 1
        self.latency.append(recv_ms - t["time"])
        if self.profile is not None:
            self.profile.add_trade(t, digest, start, recv_ms)
        out["o"] = "accepted"
        return out


def _read_segment_bytes(run_dir: str, name: str) -> bytes:
    plain = os.path.join(run_dir, name)
    packed = plain + ".gz"
    if os.path.isfile(plain):
        with open(plain, "rb") as fh:
            return fh.read()
    if os.path.isfile(packed):
        with gzip.open(packed, "rb") as fh:
            return fh.read()
    raise TradeRecordingError(f"segment {name} is missing (looked for {name} and {name}.gz)")


def _outcome_key(o: dict) -> tuple:
    code = o.get("o")
    why = o.get("why", "")
    if code == "rejected" and why not in ("future_event", "clock_regression", "untracked_coin"):
        why = "parse"
    return (o.get("tid"), code, o.get("i", 0), why)


def _canon_event(e: dict) -> str:
    return json.dumps({k: v for k, v in e.items() if k != "session"}, sort_keys=True, separators=(",", ":"))


def load_trade_recording(run_dir: str, attach=None) -> dict:
    run_dir = os.path.abspath(run_dir)
    with open(os.path.join(run_dir, "run.manifest.json")) as fh:
        run = json.load(fh)
    if run.get("schema") != RUN_SCHEMA_V2:
        raise TradeRecordingError(f"run manifest schema must be {RUN_SCHEMA_V2!r}")
    if KIND not in (run.get("kinds") or []):
        raise TradeRecordingError("the run did not record trade_flow")
    if run.get("trade_contract") != CONTRACT:
        raise TradeRecordingError(f"the run recorded trade contract {run.get('trade_contract')!r}; this importer "
                                  f"accepts only {CONTRACT!r}")
    if run.get("sources", {}).get(KIND) != CONTRACT["source"]:
        raise TradeRecordingError("the run declares a different trade-flow source")
    coins = list(run.get("coins") or [])
    if coins != sorted(coins) or not coins:
        raise TradeRecordingError("the run lists no coin or lists coins out of order")
    segments = run.get("segments") or []
    if not segments:
        raise TradeRecordingError("the run has no closed segment")
    flows = {c: _Flow(c, RECORDER_WINDOW_MS) for c in coins}
    if attach is not None:
        attach(flows)
    report = {"run_id": run.get("run_id"), "recorder_version": run.get("recorder_version"), "coins": coins,
              "started_ms": run.get("started_ms"), "closed_ms": run.get("closed_ms"), "closed": bool(run.get("closed")),
              "dropped": 0, "segments": [], "sessions": set(), "frames": 0, "malformed_frames": 0,
              "outcome_mismatches": [], "event_mismatches": [], "max_frame_silence_ms": {c: 0 for c in coins},
              "refusals": []}
    if not run.get("closed"):
        report["refusals"].append("the recording run was not closed; its last segment may be truncated")
    go_events: List[str] = []
    py_events: List[str] = []
    connected = False
    session = None
    last_frame_by_coin: Dict[str, int] = {}
    for expected_index, seg in enumerate(segments, start=1):
        if seg.get("schema") != SEGMENT_SCHEMA_V2 or seg.get("index") != expected_index:
            raise TradeRecordingError(f"segment manifest {expected_index} is missing or out of order")
        with open(os.path.join(run_dir, seg["file"] + ".manifest.json")) as fh:
            standalone = json.load(fh)
        if standalone != seg:
            raise TradeRecordingError(f"segment {seg['file']}: its manifest file disagrees with the run manifest")
        blob = _read_segment_bytes(run_dir, seg["file"])
        digest = sha256_bytes(blob)
        if digest != seg.get("sha256"):
            raise TradeRecordingError(f"segment {seg['file']}: sha256 {digest} does not match the manifest {seg.get('sha256')}")
        if not blob.endswith(b"\n"):
            raise TradeRecordingError(f"segment {seg['file']} is truncated (no final newline)")
        lines = blob.decode("utf-8").split("\n")[:-1]
        if len(lines) < 2:
            raise TradeRecordingError(f"segment {seg['file']} has no header and end record")
        header = json.loads(lines[0])
        end = json.loads(lines[-1])
        if (header.get("k") != "header" or header.get("schema") != RECORDING_SCHEMA_V2
                or header.get("run_id") != run.get("run_id") or header.get("trade_contract") != CONTRACT
                or header.get("coins") != coins):
            raise TradeRecordingError(f"segment {seg['file']} header does not match the run")
        if end.get("k") != "end" or end.get("records") != len(lines) - 2:
            raise TradeRecordingError(f"segment {seg['file']} end record is missing or miscounts its records")
        if not seg.get("complete"):
            report["refusals"].append(f"segment {seg['file']} is marked incomplete")
        for line_no, line in enumerate(lines[1:-1], start=2):
            rec = json.loads(line)
            kind = rec.get("k")
            recv = rec.get("recv_ms")
            if not isinstance(recv, int):
                raise TradeRecordingError(f"{seg['file']}:{line_no} has no integer recv_ms")
            report["sessions"].add(rec.get("session"))
            ev: List[dict] = []
            if kind == "conn":
                if rec.get("state") == "connected":
                    connected, session = True, rec.get("session")
                elif rec.get("state") == "disconnected":
                    for c in coins:
                        flows[c].stop(recv, "disconnected", ev)
                    connected = False
                else:
                    raise TradeRecordingError(f"{seg['file']}:{line_no} has unknown connection state {rec.get('state')!r}")
            elif kind == "sub":
                coin = rec.get("coin")
                if coin not in flows or rec.get("kind") != KIND:
                    raise TradeRecordingError(f"{seg['file']}:{line_no} subscribes an unrecorded coin or kind")
                if connected:
                    flows[coin].start(recv, rec.get("session"), ev)
            elif kind == "tick":
                for c in coins:
                    flows[c].advance(recv, ev)
            elif kind == "drop":
                report["dropped"] += int(rec.get("count") or 0)
                report["refusals"].append(f"{seg['file']}:{line_no} recorder queue dropped {rec.get('count')} record(s)")
            elif kind == "trades":
                report["frames"] += 1
                ev_frame, outcomes = _replay_frame(flows, coins, rec, recv, report, connected, session)
                ev.extend(ev_frame)
                recorded = [_outcome_key(o) for o in rec.get("outcomes") or []]
                mine = [_outcome_key(o) for o in outcomes]
                if recorded != mine and len(report["outcome_mismatches"]) < 20:
                    report["outcome_mismatches"].append({"line": f"{seg['file']}:{line_no}", "go": recorded[:5], "py": mine[:5]})
                for c in coins:
                    if rec.get("coin") == c:
                        prev = last_frame_by_coin.get(c)
                        if prev is not None and flows[c].cover_from:
                            report["max_frame_silence_ms"][c] = max(report["max_frame_silence_ms"][c], recv - prev)
                        last_frame_by_coin[c] = recv
            elif kind in ("interval", "invalidate", "cover"):
                go_events.append(_canon_event(rec))
                continue
            elif kind == "obs":
                continue
            else:
                raise TradeRecordingError(f"{seg['file']}:{line_no} has unknown record kind {kind!r}")
            for e in ev:
                py_events.append(_canon_event(e))
        report["segments"].append({"file": seg["file"], "sha256": digest, "records": seg.get("records"),
                                   "complete": seg.get("complete"), "trade_frames": seg.get("trade_frames"),
                                   "trade_records": seg.get("trade_records")})
    if go_events != py_events:
        first = next((i for i, (a, b) in enumerate(zip(go_events, py_events)) if a != b), min(len(go_events), len(py_events)))
        report["event_mismatches"].append({"index": first, "go_events": len(go_events), "py_events": len(py_events),
                                           "go": go_events[first] if first < len(go_events) else None,
                                           "py": py_events[first] if first < len(py_events) else None})
    if report["outcome_mismatches"] or report["event_mismatches"]:
        raise TradeRecordingError(f"independent replay disagrees with the recorder: {json.dumps(report['outcome_mismatches'][:2] + report['event_mismatches'])}")
    report["events_compared"] = len(go_events)
    report["sessions"] = sorted(s for s in report["sessions"] if s is not None)
    report["economic_evidence"] = not report["refusals"]
    series = {c: build_trade_series(flows[c], report) for c in coins}
    return {"series": series, "report": report}


def _replay_frame(flows: Dict[str, _Flow], coins: list, rec: dict, recv: int, report: dict,
                  connected: bool, session):
    ev: List[dict] = []
    outcomes: List[dict] = []
    raw = rec.get("raw") or ""
    elems = None
    try:
        env = json.loads(raw)
        if isinstance(env, dict):
            data = env.get("data")
            elems = [] if data is None else data if isinstance(data, list) else None
    except ValueError:
        elems = None
    parsed = [parse_trade(e) for e in elems] if elems is not None else []
    frame_coins = sorted({p["coin"] for p in parsed if p["coin"]})
    if elems is None or not frame_coins:
        report["malformed_frames"] += 1
        for c in coins:
            fl = flows[c]
            fl.stats["rejected"] += 1
            fl.invalidate_open(recv, "malformed")
    else:
        for c in frame_coins:
            fl = flows.get(c)
            if fl is not None:
                fl.stop_on_silence(recv, ev)
        for p in parsed:
            fl = flows.get(p["coin"])
            if fl is None:
                outcomes.append({"tid": p["tid"], "o": "rejected", "i": 0, "why": "untracked_coin"})
                continue
            if p["err"] is not None:
                fl.stats["received"] += 1
                fl.stats["rejected"] += 1
                fl.invalidate_open(recv, "malformed")
                outcomes.append({"tid": p["tid"], "o": "rejected", "i": 0, "why": p["err"]})
                continue
            if recv < fl.last_recv:
                fl.stats["received"] += 1
                fl.stats["rejected"] += 1
                fl.invalidate_open(recv, "clock_regression")
                outcomes.append({"tid": p["tid"], "o": "rejected", "i": interval_start(p["trade"]["time"]),
                                 "why": "clock_regression"})
                continue
            outcomes.append(fl.ingest(p["trade"], recv, ev))
        for c in frame_coins:
            fl = flows.get(c)
            if fl is None:
                continue
            fl.stats["frames"] += 1
            if recv >= fl.last_recv:
                fl.last_recv = recv
                if fl.cover_from != 0:
                    fl.last_frame = recv
                elif fl.resumable and connected and fl.session == session:
                    fl.begin(recv, "resumed", ev)
    for c in coins:
        flows[c].advance(recv, ev)
    return ev, outcomes


def _series_intervals(flow: _Flow) -> list:
    out = []
    for start in sorted(flow.history):
        iv = flow.history[start]
        out.append({
            "start_ms": iv.start, "end_ms": iv.start + CONTRACT["interval_ms"], "buy": format_units(iv.buy),
            "sell": format_units(iv.sell), "total": format_units(iv.buy + iv.sell), "trades": iv.trades,
            "records_hash": f"{iv.hash:064x}", "session": iv.session, "finalized_ms": iv.finalized,
            "status": "invalid" if iv.status == "invalid" else "complete",
            "invalid_ms": iv.invalid if iv.invalid else None, "reason": iv.reason,
        })
    return out


def intervals_sha256(intervals: list) -> str:
    return sha256_bytes(json.dumps(intervals, sort_keys=True, separators=(",", ":")).encode("utf-8"))


def _percentile(values: list, q: float):
    if not values:
        return None
    s = sorted(values)
    return s[min(len(s) - 1, int(round(q * (len(s) - 1))))]


def build_trade_series(flow: _Flow, report: dict) -> dict:
    intervals = _series_intervals(flow)
    gaps = [{"start_ms": g[0], "end_ms": g[1] or None, "detected_ms": g[2], "reason": g[3]} for g in flow.gaps]
    return {
        "schema": SERIES_SCHEMA,
        "kind": KIND,
        "coin": flow.coin,
        **{k: CONTRACT[k] for k in PAYLOAD_CONTRACT_KEYS},
        "intervals": intervals,
        "intervals_sha256": intervals_sha256(intervals),
        "economic_evidence": report["economic_evidence"],
        "refusals": list(report["refusals"]),
        "stats": dict(flow.stats),
        "latency_ms": {"p50": _percentile(flow.latency, 0.5), "p99": _percentile(flow.latency, 0.99),
                       "max": max(flow.latency) if flow.latency else None, "min": min(flow.latency) if flow.latency else None},
        "provenance": {"run_id": report["run_id"], "recorder_version": report["recorder_version"],
                       "segments": [{"file": s["file"], "sha256": s["sha256"]} for s in report["segments"]]},
    }


def write_trade_series(series: dict, path: str) -> str:
    text = json.dumps(series, sort_keys=True, separators=(",", ":")).encode("utf-8")
    os.makedirs(os.path.dirname(os.path.abspath(path)), exist_ok=True)
    buf = io.BytesIO()
    with gzip.GzipFile(filename="", mode="wb", fileobj=buf, mtime=0) as gz:
        gz.write(text)
    with open(path, "wb") as fh:
        fh.write(buf.getvalue())
    return sha256_bytes(buf.getvalue())


def read_trade_series(path: str) -> dict:
    with gzip.open(path, "rb") as fh:
        series = json.loads(fh.read().decode("utf-8"))
    if not isinstance(series, dict) or series.get("schema") != SERIES_SCHEMA:
        raise TradeRecordingError(f"{path}: schema must be {SERIES_SCHEMA!r}")
    for key in PAYLOAD_CONTRACT_KEYS:
        if series.get(key) != CONTRACT[key]:
            raise TradeRecordingError(f"{path}: {key} {series.get(key)!r} is not the frozen contract value {CONTRACT[key]!r}")
    if series.get("intervals_sha256") != intervals_sha256(series.get("intervals") or []):
        raise TradeRecordingError(f"{path}: intervals_sha256 does not match its intervals")
    return series


def payload_from_series(series: dict, bar_interval_ms: int, cutoff_ms: Optional[int] = None,
                        window_ms: Optional[int] = None) -> dict:
    out = {"available": True, "kind": KIND, "coin": series["coin"],
           **{k: series[k] for k in PAYLOAD_CONTRACT_KEYS}, "bar_interval_ms": int(bar_interval_ms), "gaps": []}
    if not series.get("economic_evidence"):
        return {**out, "available": False, "intervals": [],
                "reason": "the trade recording is incomplete or damaged: " + "; ".join(series.get("refusals") or [])}
    intervals = []
    for iv in series["intervals"]:
        if cutoff_ms is None:
            intervals.append(dict(iv))
            continue
        if iv["finalized_ms"] > cutoff_ms or iv["end_ms"] > cutoff_ms:
            continue
        if window_ms is not None and iv["start_ms"] < cutoff_ms - window_ms:
            continue
        cp = dict(iv)
        if cp["invalid_ms"] is not None and cp["invalid_ms"] <= cutoff_ms:
            cp["status"] = "invalid"
        else:
            cp["status"], cp["invalid_ms"], cp["reason"] = "complete", None, ""
        intervals.append(cp)
    out["intervals"] = intervals
    if cutoff_ms is not None:
        out["cutoff_ms"] = int(cutoff_ms)
    return out
