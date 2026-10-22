import gzip
import hashlib
import io
import json
import math
import os
import re
from typing import Dict, List, Optional

from observation_kinds import BOOK_RECORD_KINDS, L2_BOOK, v3_kinds_problem

RUN_SCHEMA_V3 = "go_trader_observation_run/v3"
SEGMENT_SCHEMA_V3 = "go_trader_observation_segment/v3"
RECORDING_SCHEMA_V3 = "go_trader_observation_recording/v3"
SERIES_SCHEMA = "go_trader_premium_series/v1"
KIND = "perp_premium"

CONTRACT = {
    "source": "hyperliquid_ws_activeAssetCtx+bbo",
    "contract": "perp-premium-quote-oracle/1639-v1",
    "time_basis": "receipt_cutoff_latest_component",
    "oracle_time_basis": "receipt_only_no_source_time",
    "sample_ms": 60_000,
    "max_future_skew_ms": 5_000,
    "stale_after_ms": 120_000,
}
PAYLOAD_CONTRACT_KEYS = ("source", "contract", "time_basis", "oracle_time_basis", "sample_ms", "max_future_skew_ms")
SAMPLE_MS = CONTRACT["sample_ms"]
MAX_WINDOW_MS = 26 * 3_600_000
RECORDER_WINDOW_MS = 2 * 3_600_000
RETAIN_SLACK_MS = 10 * 60_000
MAX_SAMPLES = (MAX_WINDOW_MS + RETAIN_SLACK_MS) // SAMPLE_MS
INT64_MAX = (1 << 63) - 1
_INT_RE = re.compile(r"^[+-]?[0-9]+$")
_JSON_NUMBER_RE = re.compile(r"^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?$")
KNOWN_REJECT_REASONS = ("future_event", "clock_regression")


class PremiumRecordingError(ValueError):
    pass


class _Num(str):
    pass


def sha256_bytes(blob: bytes) -> str:
    return hashlib.sha256(blob).hexdigest()


def grid_at_or_after(ms: int) -> int:
    q = ms // SAMPLE_MS if ms >= 0 else -((-ms) // SAMPLE_MS)
    if ms % SAMPLE_MS != 0 and ms > 0:
        q += 1
    return q * SAMPLE_MS


def canonical_decimal(raw: str) -> str:
    s = raw.strip()
    if not s:
        raise ValueError("empty decimal")
    whole, dot, frac = s.partition(".")
    if whole == "" or (dot and frac == ""):
        raise ValueError(f"decimal {raw!r} is malformed")
    if not all("0" <= ch <= "9" for ch in whole + frac):
        raise ValueError(f"decimal {raw!r} is not a plain nonnegative decimal")
    whole = whole.lstrip("0") or "0"
    frac = frac.rstrip("0")
    return whole + ("." + frac if frac else "")


def positive_decimal(raw, what: str) -> str:
    if not isinstance(raw, str):
        raise ValueError(f"{what} is not a string")
    c = canonical_decimal(raw)
    try:
        v = float(c)
    except ValueError:
        raise ValueError(f"{what} {raw!r} is not a positive finite decimal")
    if not (math.isfinite(v) and v > 0):
        raise ValueError(f"{what} {raw!r} is not a positive finite decimal")
    return c


def _loads(raw: str):
    return json.loads(raw, parse_int=_Num, parse_float=_Num)


def _number_text(value) -> Optional[str]:
    if value is None:
        return ""
    if isinstance(value, _Num):
        return str(value)
    if isinstance(value, str) and _JSON_NUMBER_RE.match(value):
        return value
    return None


def _go_int(text: str) -> Optional[int]:
    if not _INT_RE.match(text):
        return None
    v = int(text)
    if v > INT64_MAX or v < -INT64_MAX - 1:
        return None
    return v


def _string_field(obj: dict, key: str):
    v = obj.get(key)
    if v is None:
        return ""
    if isinstance(v, str) and not isinstance(v, _Num):
        return v
    raise ValueError(f"{key} is not a string")


def parse_level(raw, side: str):
    if raw is None:
        return None
    if not isinstance(raw, dict):
        raise ValueError(f"bbo {side} level: not an object")
    px_raw = _string_field(raw, "px")
    sz_raw = _string_field(raw, "sz")
    n_text = _number_text(raw.get("n"))
    if n_text is None:
        raise ValueError(f"bbo {side} level: n is not a number")
    px = positive_decimal(px_raw, f"bbo {side} px")
    sz = positive_decimal(sz_raw, f"bbo {side} sz")
    n = _go_int(n_text)
    if n is None or n < 0:
        raise ValueError(f"bbo {side} order count {n_text!r} is not a nonnegative integer")
    return {"px": px, "sz": sz, "n": n}


def parse_bbo(data) -> dict:
    out = {"coin": "", "event_ms": 0, "one_sided": False, "quote": None, "digest": None, "err": None, "crossed": False}
    try:
        if not isinstance(data, dict):
            raise ValueError("decode bbo: not an object")
        coin = _string_field(data, "coin")
        time_text = _number_text(data.get("time"))
        if time_text is None:
            raise ValueError("decode bbo: time is not a number")
        levels = data.get("bbo")
        if levels is not None and not isinstance(levels, list):
            raise ValueError("decode bbo: bbo is not an array")
    except ValueError as exc:
        out["err"] = str(exc)
        return out
    out["coin"] = coin.strip()
    if not out["coin"]:
        out["err"] = "bbo carries no coin"
        return out
    t = _go_int(time_text)
    if t is None or t <= 0:
        out["err"] = f"bbo time {time_text!r} is not a positive millisecond timestamp"
        return out
    out["event_ms"] = t
    levels = levels or []
    if len(levels) != 2:
        out["err"] = f"bbo carries {len(levels)} levels, want [bid, ask]"
        return out
    try:
        bid = parse_level(levels[0], "bid")
        ask = parse_level(levels[1], "ask")
    except ValueError as exc:
        out["err"] = str(exc)
        return out
    parts = [out["coin"], str(t)]
    for lvl in (bid, ask):
        parts += [lvl["px"], lvl["sz"], str(lvl["n"])] if lvl else ["", "", "0"]
    out["digest"] = hashlib.sha256("|".join(parts).encode("utf-8")).digest()
    if bid is None or ask is None:
        out["one_sided"] = True
        return out
    if float(bid["px"]) >= float(ask["px"]):
        out["crossed"] = True
        out["err"] = f"bbo bid {bid['px']} is not below ask {ask['px']}"
        return out
    out["quote"] = {"bid": bid["px"], "bid_sz": bid["sz"], "bid_n": bid["n"], "ask": ask["px"], "ask_sz": ask["sz"],
                    "ask_n": ask["n"], "event_ms": t}
    return out


def parse_context(data) -> dict:
    out = {"coin": "", "context": None, "err": None}
    try:
        if not isinstance(data, dict):
            raise ValueError("decode activeAssetCtx: not an object")
        coin = _string_field(data, "coin")
        ctx = data.get("ctx")
        if ctx is not None and not isinstance(ctx, dict):
            raise ValueError("decode activeAssetCtx: ctx is not an object")
    except ValueError as exc:
        out["err"] = str(exc)
        return out
    out["coin"] = coin.strip()
    if not out["coin"]:
        out["err"] = "activeAssetCtx carries no coin"
        return out
    ctx = ctx or {}

    def text(name: str, nullable: bool) -> str:
        if name not in ctx:
            raise ValueError(f"activeAssetCtx for {out['coin']} carries no {name}")
        v = ctx[name]
        if v is None:
            if nullable:
                return ""
            raise ValueError(f"activeAssetCtx {name} for {out['coin']} is null")
        if not isinstance(v, str) or isinstance(v, _Num):
            raise ValueError(f"activeAssetCtx {name} for {out['coin']} is not a decimal string")
        return positive_decimal(v, "activeAssetCtx " + name)

    try:
        oracle = text("oraclePx", False)
        mark = text("markPx", False)
        mid = text("midPx", True)
    except ValueError as exc:
        out["err"] = str(exc)
        return out
    out["context"] = {"oracle": oracle, "mark": mark, "mid": mid}
    return out


class _State:
    def __init__(self, coin: str):
        self.coin = coin
        self.samples: List[dict] = []
        self.history: List[dict] = []
        self.gaps: List[list] = []
        self.gap_log: List[list] = []
        self.next_ms = 0
        self.quote = None
        self.quote_missing = "no_quote"
        self.ctx = None
        self.ctx_missing = "no_context"
        self.last_quote_ms = 0
        self.last_quote_hash = None
        self.poisoned_ms = 0
        self.quote_seq = 0
        self.ctx_seq = 0
        self.last_recv = 0
        self.pending_invalid = ""
        self.stats = {k: 0 for k in ("quote_frames", "quote_accepted", "quote_one_sided", "quote_duplicates",
                                     "quote_conflicts", "quote_out_of_order", "ctx_frames", "ctx_accepted",
                                     "rejected", "finalized", "sampled", "missing", "invalid")}
        self.quote_latency: List[int] = []

    def open_gap(self):
        if self.gaps and self.gaps[-1][1] == 0:
            return self.gaps[-1]
        return None

    def init_at(self, recv: int):
        if self.next_ms == 0:
            self.next_ms = grid_at_or_after(recv)

    @staticmethod
    def quote_fields(q):
        if q is None:
            return None
        return {k: q[k] for k in ("bid", "bid_sz", "bid_n", "ask", "ask_sz", "ask_n", "event_ms", "recv_ms", "session", "seq")}

    @staticmethod
    def ctx_fields(c):
        if c is None:
            return None
        return {"oracle": c["oracle"], "mark": c["mark"], "mid": c["mid"] or None, "oracle_event_ms": None,
                "recv_ms": c["recv_ms"], "session": c["session"], "seq": c["seq"]}

    def advance(self, now: int, ev: list):
        if self.next_ms == 0:
            return
        floor = grid_at_or_after(now - MAX_WINDOW_MS - RETAIN_SLACK_MS)
        if self.next_ms < floor:
            self.next_ms = floor
        steps = 0
        while steps <= MAX_SAMPLES and self.next_ms < now:
            smp = {"t": self.next_ms, "quote": dict(self.quote) if self.quote else None,
                   "context": dict(self.ctx) if self.ctx else None, "finalized_ms": now,
                   "invalid_ms": 0, "invalid_reason": ""}
            if self.pending_invalid:
                smp["status"], smp["reason"] = "invalid", self.pending_invalid
                self.stats["invalid"] += 1
            elif self.quote is None:
                smp["status"], smp["reason"] = "missing", self.quote_missing
                self.stats["missing"] += 1
            elif self.ctx is None:
                smp["status"], smp["reason"] = "missing", self.ctx_missing
                self.stats["missing"] += 1
            else:
                smp["status"], smp["reason"] = "sampled", ""
                self.stats["sampled"] += 1
            self.pending_invalid = ""
            self.stats["finalized"] += 1
            self.samples.append(smp)
            self.history.append(smp)
            ev.append({"k": "psample", "coin": self.coin, "kind": KIND, "t": smp["t"], "status": smp["status"],
                       "reason": smp["reason"], "quote": self.quote_fields(smp["quote"]),
                       "context": self.ctx_fields(smp["context"]), "finalized_ms": now, "recv_ms": now})
            self.next_ms += SAMPLE_MS
            steps += 1
        self.trim(now)

    def trim(self, now: int):
        floor = now - RECORDER_WINDOW_MS - RETAIN_SLACK_MS
        cut = 0
        while cut < len(self.samples) and self.samples[cut]["t"] < floor:
            cut += 1
        over = len(self.samples) - cut - MAX_SAMPLES
        if over > 0:
            cut += over
        if cut:
            self.samples = self.samples[cut:]
        self.gaps = [g for g in self.gaps if not (g[1] != 0 and g[1] < floor)]

    def add_gap(self, gap: list):
        self.gaps.append(gap)
        self.gap_log.append(gap)

    def invalidate_open(self, at: int, reason: str):
        if not self.pending_invalid:
            self.pending_invalid = reason
        if not self.gaps or self.gaps[-1][3] != reason or self.gaps[-1][2] != at:
            g = self.open_gap()
            if g is not None:
                g[1] = at
            self.add_gap([at, at, at, reason])

    def reject(self, at: int, reason: str, clear_quote: bool, clear_ctx: bool):
        self.stats["rejected"] += 1
        if clear_quote:
            self.quote, self.quote_missing = None, reason
        if clear_ctx:
            self.ctx, self.ctx_missing = None, reason
        self.invalidate_open(at, reason)

    def close_gap(self, recv: int):
        g = self.open_gap()
        if g is not None:
            g[1] = recv

    def mark_disconnected(self, at: int, ev: list):
        if self.next_ms == 0:
            return
        if at >= self.last_recv:
            self.advance(at, ev)
        self.quote, self.quote_missing = None, "disconnected"
        self.ctx, self.ctx_missing = None, "disconnected"
        if self.open_gap() is None:
            start = self.last_recv
            if start == 0 or start > at:
                start = at
            self.add_gap([start, 0, at, "disconnected"])

    def invalidate_quote_samples(self, event_ms: int, at: int, ev: list):
        for smp in reversed(self.samples):
            if smp["quote"] is None or smp["quote"]["event_ms"] != event_ms:
                break
            if smp["invalid_ms"]:
                continue
            smp["invalid_ms"], smp["invalid_reason"] = at, "conflicting_duplicate"
            ev.append({"k": "pinvalidate", "coin": self.coin, "kind": KIND, "t": smp["t"],
                       "reason": "conflicting_duplicate", "recv_ms": at})

    def ingest_quote(self, p: dict, recv: int, session: int, ev: list) -> dict:
        self.stats["quote_frames"] += 1
        self.init_at(recv)
        if recv < self.last_recv:
            self.reject(recv, "clock_regression", True, True)
            return {"o": "rejected", "why": "clock_regression"}
        self.last_recv = recv
        self.advance(recv, ev)
        if p["err"] is not None:
            self.reject(recv, "crossed_book" if p["crossed"] else "malformed", True, False)
            return {"o": "rejected", "why": p["err"]}
        if p["event_ms"] > recv + CONTRACT["max_future_skew_ms"]:
            self.reject(recv, "future_event", True, False)
            return {"o": "rejected", "why": "future_event"}
        has_hash = self.last_quote_hash is not None
        if has_hash and p["event_ms"] < self.last_quote_ms:
            self.stats["quote_out_of_order"] += 1
            self.reject(recv, "out_of_order", True, False)
            return {"o": "out_of_order", "why": "out_of_order"}
        if self.poisoned_ms and p["event_ms"] == self.poisoned_ms:
            self.stats["quote_conflicts"] += 1
            self.reject(recv, "conflicting_duplicate", True, False)
            return {"o": "conflict", "why": "conflicting_duplicate"}
        if has_hash and p["event_ms"] == self.last_quote_ms:
            if p["digest"] == self.last_quote_hash:
                if self.quote is not None or self.quote_missing == "one_sided_book":
                    self.stats["quote_duplicates"] += 1
                    return {"o": "duplicate"}
            else:
                self.stats["quote_conflicts"] += 1
                self.poisoned_ms = p["event_ms"]
                self.reject(recv, "conflicting_duplicate", True, False)
                self.invalidate_quote_samples(p["event_ms"], recv, ev)
                return {"o": "conflict", "why": "conflicting_duplicate"}
        self.last_quote_ms, self.last_quote_hash = p["event_ms"], p["digest"]
        self.close_gap(recv)
        if p["one_sided"]:
            self.stats["quote_one_sided"] += 1
            self.quote, self.quote_missing = None, "one_sided_book"
            return {"o": "one_sided", "why": "one_sided_book"}
        self.quote_seq += 1
        self.quote = {**p["quote"], "recv_ms": recv, "session": session, "seq": self.quote_seq}
        self.stats["quote_accepted"] += 1
        self.quote_latency.append(recv - p["event_ms"])
        return {"o": "accepted", "seq": self.quote_seq}

    def ingest_context(self, p: dict, recv: int, session: int, ev: list) -> dict:
        self.stats["ctx_frames"] += 1
        self.init_at(recv)
        if recv < self.last_recv:
            self.reject(recv, "clock_regression", True, True)
            return {"o": "rejected", "why": "clock_regression"}
        self.last_recv = recv
        self.advance(recv, ev)
        if p["err"] is not None:
            self.reject(recv, "malformed", False, True)
            return {"o": "rejected", "why": p["err"]}
        self.close_gap(recv)
        self.ctx_seq += 1
        self.ctx = {**p["context"], "recv_ms": recv, "session": session, "seq": self.ctx_seq}
        self.stats["ctx_accepted"] += 1
        return {"o": "accepted", "seq": self.ctx_seq}


def _outcome_key(o: dict) -> tuple:
    code = o.get("o")
    why = o.get("why", "")
    if code == "rejected" and why not in KNOWN_REJECT_REASONS:
        why = "parse"
    return (code, why, o.get("seq", 0))


def _canon_event(e: dict) -> str:
    return json.dumps({k: v for k, v in e.items() if k != "session"}, sort_keys=True, separators=(",", ":"))


def _read_segment_bytes(run_dir: str, name: str) -> bytes:
    plain = os.path.join(run_dir, name)
    packed = plain + ".gz"
    if os.path.isfile(plain):
        with open(plain, "rb") as fh:
            return fh.read()
    if os.path.isfile(packed):
        with gzip.open(packed, "rb") as fh:
            return fh.read()
    raise PremiumRecordingError(f"segment {name} is missing (looked for {name} and {name}.gz)")


def _envelope_data(raw: str, channel: str):
    try:
        env = _loads(raw)
    except ValueError:
        return False, None
    if not isinstance(env, dict):
        return False, None
    if env.get("channel") != channel:
        return True, None
    return True, env.get("data")


def load_premium_recording(run_dir: str, keep_quotes: bool = False) -> dict:
    run_dir = os.path.abspath(run_dir)
    with open(os.path.join(run_dir, "run.manifest.json")) as fh:
        run = json.load(fh)
    if run.get("schema") != RUN_SCHEMA_V3:
        raise PremiumRecordingError(f"run manifest schema must be {RUN_SCHEMA_V3!r}")
    if v3_kinds_problem(run):
        raise PremiumRecordingError(f"run manifest {v3_kinds_problem(run)}")
    book_kinds = BOOK_RECORD_KINDS if L2_BOOK in run["kinds"] else ()
    if KIND not in (run.get("kinds") or []):
        raise PremiumRecordingError("the run did not record perp_premium")
    if run.get("premium_contract") != CONTRACT:
        raise PremiumRecordingError(f"the run recorded premium contract {run.get('premium_contract')!r}; this importer "
                                    f"accepts only {CONTRACT!r}")
    if run.get("sources", {}).get(KIND) != CONTRACT["source"]:
        raise PremiumRecordingError("the run declares a different premium source")
    coins = list(run.get("coins") or [])
    if not coins or coins != sorted(coins):
        raise PremiumRecordingError("the run lists no coin or lists coins out of order")
    segments = run.get("segments") or []
    if not segments:
        raise PremiumRecordingError("the run has no closed segment")
    states = {c: _State(c) for c in coins}
    report = {"run_id": run.get("run_id"), "recorder_version": run.get("recorder_version"), "coins": coins,
              "started_ms": run.get("started_ms"), "closed_ms": run.get("closed_ms"), "closed": bool(run.get("closed")),
              "dropped": 0, "segments": [], "sessions": set(), "quote_frames": 0, "context_frames": 0,
              "malformed_frames": 0, "outcome_mismatches": [], "event_mismatches": [], "refusals": [],
              "connections": [], "max_quote_silence_ms": {c: 0 for c in coins},
              "max_context_silence_ms": {c: 0 for c in coins}}
    if not run.get("closed"):
        report["refusals"].append("the recording run was not closed; its last segment may be truncated")
    go_events: List[str] = []
    py_events: List[str] = []
    quotes: Dict[str, list] = {c: [] for c in coins}
    last_quote_recv: Dict[str, int] = {}
    last_ctx_recv: Dict[str, int] = {}
    for expected_index, seg in enumerate(segments, start=1):
        if seg.get("schema") != SEGMENT_SCHEMA_V3 or seg.get("index") != expected_index:
            raise PremiumRecordingError(f"segment manifest {expected_index} is missing or out of order")
        with open(os.path.join(run_dir, seg["file"] + ".manifest.json")) as fh:
            standalone = json.load(fh)
        if standalone != seg:
            raise PremiumRecordingError(f"segment {seg['file']}: its manifest file disagrees with the run manifest")
        blob = _read_segment_bytes(run_dir, seg["file"])
        digest = sha256_bytes(blob)
        if digest != seg.get("sha256"):
            raise PremiumRecordingError(f"segment {seg['file']}: sha256 {digest} does not match the manifest {seg.get('sha256')}")
        if not blob.endswith(b"\n"):
            raise PremiumRecordingError(f"segment {seg['file']} is truncated (no final newline)")
        lines = blob.decode("utf-8").split("\n")[:-1]
        if len(lines) < 2:
            raise PremiumRecordingError(f"segment {seg['file']} has no header and end record")
        header = json.loads(lines[0])
        end = json.loads(lines[-1])
        if v3_kinds_problem(header) or header.get("kinds") != run.get("kinds"):
            raise PremiumRecordingError(f"segment {seg['file']} header {v3_kinds_problem(header) or 'lists different kinds than the run'}")
        if (header.get("k") != "header" or header.get("schema") != RECORDING_SCHEMA_V3
                or header.get("run_id") != run.get("run_id") or header.get("premium_contract") != CONTRACT
                or header.get("coins") != coins):
            raise PremiumRecordingError(f"segment {seg['file']} header does not match the run")
        if end.get("k") != "end" or end.get("records") != len(lines) - 2:
            raise PremiumRecordingError(f"segment {seg['file']} end record is missing or miscounts its records")
        if not seg.get("complete"):
            report["refusals"].append(f"segment {seg['file']} is marked incomplete")
        for line_no, line in enumerate(lines[1:-1], start=2):
            rec = json.loads(line)
            kind = rec.get("k")
            recv = rec.get("recv_ms")
            if not isinstance(recv, int):
                raise PremiumRecordingError(f"{seg['file']}:{line_no} has no integer recv_ms")
            session = rec.get("session")
            report["sessions"].add(session)
            ev: List[dict] = []
            outcome = None
            if kind == "conn":
                state = rec.get("state")
                if state == "disconnected":
                    for c in coins:
                        states[c].mark_disconnected(recv, ev)
                elif state != "connected":
                    raise PremiumRecordingError(f"{seg['file']}:{line_no} has unknown connection state {state!r}")
                report["connections"].append({"state": state, "recv_ms": recv, "session": session})
            elif kind in ("bbo", "pctx"):
                channel = "bbo" if kind == "bbo" else "activeAssetCtx"
                ok, data = _envelope_data(rec.get("raw") or "", channel)
                if not ok:
                    raise PremiumRecordingError(f"{seg['file']}:{line_no} {kind} record does not hold a {channel} envelope")
                p = parse_bbo(data) if kind == "bbo" else parse_context(data)
                coin = rec.get("coin")
                if kind == "bbo":
                    report["quote_frames"] += 1
                else:
                    report["context_frames"] += 1
                if not p["coin"]:
                    if coin != "":
                        raise PremiumRecordingError(f"{seg['file']}:{line_no} names coin {coin!r} but its frame names none")
                    report["malformed_frames"] += 1
                    _reject_all(states, coins, recv, kind, ev)
                    outcome = {"o": "rejected", "why": p["err"] or "frame names no coin"}
                else:
                    if p["coin"] != coin or coin not in states:
                        raise PremiumRecordingError(f"{seg['file']}:{line_no} {kind} record coin {coin!r} disagrees with its frame {p['coin']!r}")
                    st = states[coin]
                    if kind == "bbo":
                        outcome = st.ingest_quote(p, recv, session, ev)
                        prev = last_quote_recv.get(coin)
                        if prev is not None and st.quote is not None:
                            report["max_quote_silence_ms"][coin] = max(report["max_quote_silence_ms"][coin], recv - prev)
                        last_quote_recv[coin] = recv
                        if keep_quotes and outcome["o"] == "accepted":
                            q = st.quote
                            quotes[coin].append([recv, q["event_ms"], q["bid"], q["bid_sz"], q["ask"], q["ask_sz"], session, q["seq"]])
                    else:
                        outcome = st.ingest_context(p, recv, session, ev)
                        prev = last_ctx_recv.get(coin)
                        if prev is not None and st.ctx is not None:
                            report["max_context_silence_ms"][coin] = max(report["max_context_silence_ms"][coin], recv - prev)
                        last_ctx_recv[coin] = recv
                    if p["err"] is not None:
                        report["malformed_frames"] += 1
            elif kind == "pbad":
                report["malformed_frames"] += 1
                _reject_all(states, coins, recv, "pbad", ev)
                outcome = {"o": "rejected", "why": "socket frame is not a JSON envelope"}
            elif kind == "ptick":
                for c in coins:
                    st = states[c]
                    if st.next_ms == 0 or recv < st.last_recv:
                        continue
                    st.advance(recv, ev)
            elif kind == "drop":
                report["dropped"] += int(rec.get("count") or 0)
                report["refusals"].append(f"{seg['file']}:{line_no} recorder queue dropped {rec.get('count')} record(s)")
            elif kind in ("psample", "pinvalidate"):
                go_events.append(_canon_event(rec))
                continue
            elif kind in ("obs", "trades", "sub", "cover", "interval", "invalidate", "tick") or kind in book_kinds:
                continue
            else:
                raise PremiumRecordingError(f"{seg['file']}:{line_no} has unknown record kind {kind!r}")
            if outcome is not None:
                recorded = {"o": rec.get("o"), "why": rec.get("why", ""), "seq": rec.get("seq", 0)}
                mine = {"o": outcome.get("o"), "why": outcome.get("why", ""), "seq": outcome.get("seq", 0)}
                if _outcome_key(recorded) != _outcome_key(mine) and len(report["outcome_mismatches"]) < 20:
                    report["outcome_mismatches"].append({"line": f"{seg['file']}:{line_no}", "go": recorded, "py": mine})
            for e in ev:
                py_events.append(_canon_event(e))
        report["segments"].append({"file": seg["file"], "sha256": digest, "records": seg.get("records"),
                                   "complete": seg.get("complete"), "quote_frames": seg.get("quote_frames"),
                                   "context_frames": seg.get("context_frames"), "premium_samples": seg.get("premium_samples")})
    if go_events != py_events:
        first = next((i for i, (a, b) in enumerate(zip(go_events, py_events)) if a != b), min(len(go_events), len(py_events)))
        report["event_mismatches"].append({"index": first, "go_events": len(go_events), "py_events": len(py_events),
                                           "go": go_events[first] if first < len(go_events) else None,
                                           "py": py_events[first] if first < len(py_events) else None})
    if report["outcome_mismatches"] or report["event_mismatches"]:
        raise PremiumRecordingError("independent replay disagrees with the recorder: "
                                    + json.dumps(report["outcome_mismatches"][:2] + report["event_mismatches"]))
    report["events_compared"] = len(go_events)
    report["sessions"] = sorted(s for s in report["sessions"] if s is not None)
    report["economic_evidence"] = not report["refusals"]
    series = {c: build_premium_series(states[c], report) for c in coins}
    out = {"series": series, "report": report}
    if keep_quotes:
        out["quotes"] = quotes
    return out


def _reject_all(states: Dict[str, _State], coins: list, recv: int, kind: str, ev: list):
    for c in coins:
        st = states[c]
        if st.next_ms == 0:
            continue
        if recv < st.last_recv:
            st.reject(recv, "clock_regression", True, True)
            continue
        st.last_recv = recv
        st.advance(recv, ev)
        st.reject(recv, "malformed", kind != "pctx", kind != "bbo")


def _payload_sample(smp: dict) -> dict:
    q, c = smp["quote"], smp["context"]
    out = {"t": smp["t"], "status": smp["status"], "finalized_ms": smp["finalized_ms"],
           "quote": _State.quote_fields(q), "context": _State.ctx_fields(c),
           "invalid_ms": smp["invalid_ms"] or None}
    if smp["reason"]:
        out["reason"] = smp["reason"]
    if smp["invalid_ms"]:
        out["invalid_reason"] = smp["invalid_reason"]
    return out


def samples_sha256(samples: list) -> str:
    return sha256_bytes(json.dumps(samples, sort_keys=True, separators=(",", ":")).encode("utf-8"))


def _percentile(values: list, q: float):
    if not values:
        return None
    s = sorted(values)
    return s[min(len(s) - 1, int(round(q * (len(s) - 1))))]


def build_premium_series(st: _State, report: dict) -> dict:
    samples = [_payload_sample(s) for s in st.history]
    gaps = [{"start_ms": g[0], "end_ms": g[1] or None, "detected_ms": g[2], "reason": g[3]} for g in st.gap_log]
    return {
        "schema": SERIES_SCHEMA,
        "kind": KIND,
        "coin": st.coin,
        **{k: CONTRACT[k] for k in PAYLOAD_CONTRACT_KEYS},
        "samples": samples,
        "samples_sha256": samples_sha256(samples),
        "gaps": gaps,
        "economic_evidence": report["economic_evidence"],
        "refusals": list(report["refusals"]),
        "stats": dict(st.stats),
        "quote_latency_ms": {"p50": _percentile(st.quote_latency, 0.5), "p99": _percentile(st.quote_latency, 0.99),
                             "max": max(st.quote_latency) if st.quote_latency else None,
                             "min": min(st.quote_latency) if st.quote_latency else None},
        "provenance": {"run_id": report["run_id"], "recorder_version": report["recorder_version"],
                       "segments": [{"file": s["file"], "sha256": s["sha256"]} for s in report["segments"]]},
    }


def write_premium_series(series: dict, path: str) -> str:
    text = json.dumps(series, sort_keys=True, separators=(",", ":")).encode("utf-8")
    os.makedirs(os.path.dirname(os.path.abspath(path)), exist_ok=True)
    buf = io.BytesIO()
    with gzip.GzipFile(filename="", mode="wb", fileobj=buf, mtime=0) as gz:
        gz.write(text)
    with open(path, "wb") as fh:
        fh.write(buf.getvalue())
    return sha256_bytes(buf.getvalue())


def read_premium_series(path: str) -> dict:
    with gzip.open(path, "rb") as fh:
        series = json.loads(fh.read().decode("utf-8"))
    if not isinstance(series, dict) or series.get("schema") != SERIES_SCHEMA:
        raise PremiumRecordingError(f"{path}: schema must be {SERIES_SCHEMA!r}")
    for key in PAYLOAD_CONTRACT_KEYS:
        if series.get(key) != CONTRACT[key]:
            raise PremiumRecordingError(f"{path}: {key} {series.get(key)!r} is not the frozen contract value {CONTRACT[key]!r}")
    if series.get("samples_sha256") != samples_sha256(series.get("samples") or []):
        raise PremiumRecordingError(f"{path}: samples_sha256 does not match its samples")
    return series


def payload_from_series(series: dict, bar_interval_ms: int, cutoff_ms: Optional[int] = None,
                        window_ms: Optional[int] = None) -> dict:
    out = {"available": True, "kind": KIND, "coin": series["coin"],
           **{k: series[k] for k in PAYLOAD_CONTRACT_KEYS}, "bar_interval_ms": int(bar_interval_ms), "gaps": []}
    if not series.get("economic_evidence"):
        return {**out, "available": False, "samples": [],
                "reason": "the premium recording is incomplete or damaged: " + "; ".join(series.get("refusals") or [])}
    samples = []
    for smp in series["samples"]:
        if cutoff_ms is not None:
            if smp["t"] > cutoff_ms or (window_ms is not None and smp["t"] < cutoff_ms - window_ms):
                continue
            cp = dict(smp)
            if cp["invalid_ms"] is not None and cp["invalid_ms"] > cutoff_ms:
                cp["invalid_ms"] = None
                cp.pop("invalid_reason", None)
            samples.append(cp)
        else:
            samples.append(dict(smp))
    out["samples"] = samples
    if cutoff_ms is not None:
        out["cutoff_ms"] = int(cutoff_ms)
        out["window_ms"] = int(window_ms) if window_ms is not None else None
    return out


def coverage_report(series: Optional[dict], start_ms: int, end_ms: int) -> dict:
    if series is None:
        return {"available": False, "reason": "no premium series is attached to this dataset"}
    expected = max(0, (end_ms - start_ms) // SAMPLE_MS)
    inside = [s for s in series["samples"] if start_ms <= s["t"] < end_ms]
    statuses: dict = {}
    reasons: dict = {}
    for s in inside:
        statuses[s["status"]] = statuses.get(s["status"], 0) + 1
        if s["status"] != "sampled":
            r = s.get("reason") or ""
            reasons[r] = reasons.get(r, 0) + 1
    sampled = statuses.get("sampled", 0)
    return {
        "available": bool(series.get("economic_evidence")) and bool(inside),
        "economic_evidence": bool(series.get("economic_evidence")),
        "refusals": list(series.get("refusals") or []),
        "expected_samples": int(expected),
        "recorded_samples": len(inside),
        "status_counts": dict(sorted(statuses.items())),
        "non_sampled_reasons": dict(sorted(reasons.items())),
        "sampled_share": round(sampled / expected, 6) if expected else None,
        "revoked_after_finalize": sum(1 for s in inside if s.get("invalid_ms") is not None),
        "samples_sha256": series["samples_sha256"],
    }
