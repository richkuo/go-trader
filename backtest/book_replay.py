import gzip
import hashlib
import json
import os
from fractions import Fraction
from typing import Dict, List, Optional

RUN_SCHEMA_V3 = "go_trader_observation_run/v3"
SEGMENT_SCHEMA_V3 = "go_trader_observation_segment/v3"
RECORDING_SCHEMA_V3 = "go_trader_observation_recording/v3"
SERIES_SCHEMA = "go_trader_l2_book_series/v1"
KIND = "l2_book"
HOST = "https://api.hyperliquid.xyz"
NAMESPACE = "perps"

CONTRACT = {
    "source": "hyperliquid_ws_l2Book",
    "contract": "hyperliquid-l2book-native-slow/1635-v1",
    "time_semantics": "source_event_ms+first_receipt_ms/1635-v1",
    "identity_semantics": "sha256-lp(host,namespace,coin,event_ms,levels)/1635-v1",
    "identity_tag": "go-trader-l2book-identity/v1",
    "max_levels": 20,
    "max_observations": 4096,
    "max_window_ms": 900_000,
    "max_markers": 64,
    "max_future_skew_ms": 250,
    "max_lag_ms": 5_000,
    "max_gap_ms": 10_000,
    "max_frame_bytes": 16_384,
    "decimal_max_len": 32,
    "decimal_max_frac": 18,
    "max_orders": 4_294_967_295,
    "retain_slack_ms": 60_000,
    "subscription": '{"type":"l2Book","coin":"<coin>","fast":false}',
}
PAYLOAD_CONTRACT_KEYS = ("source", "contract", "time_semantics", "identity_semantics", "max_future_skew_ms", "max_lag_ms",
                         "max_gap_ms")
BOOK_RECORD_KINDS = ("book", "book_sub", "book_marker", "book_need", "book_release")
MAX_COIN_BYTES = 64
MAX_REASON_BYTES = 240
UNCOVERED_REASON = "no acknowledged l2Book subscription in this socket session"
CONFLICT_REASON = "same source event time with different content"
ENVELOPE_REASON = "malformed: the socket message is not a JSON envelope"
MARKER_REASONS = frozenset({"epoch_start", "subscribed", "subscription_failed", "disconnected", "invalid_frame", "conflict",
                            "out_of_order", "clock_regression", "silence", "evidence_loss", "depth_raised"})
_GO_SPACE = frozenset("\t\n\v\f\r \x85\xa0            "
                      "    　")
_MISSING = object()


class BookRecordingError(ValueError):
    pass


class _Num:
    __slots__ = ("text",)

    def __init__(self, text: str):
        self.text = text


class _Obj(list):
    pass


class _GoTypeError(ValueError):
    pass


class UnreplayableFrame(ValueError):
    pass


def _reject_constant(name):
    raise ValueError(f"{name} is not JSON")


def _go_loads(text: str):
    return json.loads(text, object_pairs_hook=_Obj, parse_int=_Num, parse_float=_Num, parse_constant=_reject_constant)


def _fold(s: str) -> str:
    return s.casefold()


def _go_field(obj, name: str) -> list:
    vals = [v for k, v in obj if k == name or _fold(k) == _fold(name)]
    if len(vals) > 1:
        raise UnreplayableFrame(f"the object repeats the {name!r} field; Go merges repeated fields in ways this replay does not model")
    return vals


def _go_string(values: list) -> str:
    out = ""
    for v in values:
        if v is None:
            continue
        if not isinstance(v, str):
            raise _GoTypeError("not a string")
        out = v
    return out


def _go_number_text(values: list) -> str:
    out = ""
    for v in values:
        if v is None:
            continue
        if isinstance(v, _Num):
            out = v.text
            continue
        if isinstance(v, str):
            try:
                parsed = _go_loads(v)
            except ValueError:
                raise _GoTypeError("invalid number literal")
            if not isinstance(parsed, _Num) or v != v.strip(" \t\r\n") or v == "":
                raise _GoTypeError("invalid number literal")
            out = v
            continue
        raise _GoTypeError("not a number")
    return out


def _go_struct(value, fields: dict) -> dict:
    if value is None:
        return {k: d for k, (_, d) in fields.items()}
    if not isinstance(value, _Obj):
        raise _GoTypeError("not an object")
    out = {}
    for name, (decode, default) in fields.items():
        vals = _go_field(value, name)
        out[name] = decode(vals) if vals else default
    return out


def _go_int(text: str, lo: int, hi: int) -> Optional[int]:
    if not text:
        return None
    body = text[1:] if text[0] in "+-" else text
    if not body or not body.isascii() or not body.isdigit():
        return None
    v = int(text)
    return v if lo <= v <= hi else None


def _go_uint(text: str) -> Optional[int]:
    if not text or not text.isascii() or not text.isdigit():
        return None
    v = int(text)
    return v if v < (1 << 64) else None


def go_trim_space(s: str) -> str:
    i, j = 0, len(s)
    while i < j and s[i] in _GO_SPACE:
        i += 1
    while j > i and s[j - 1] in _GO_SPACE:
        j -= 1
    return s[i:j]


def bounded_reason(s: str) -> str:
    b = bytearray(s.encode("utf-8")[:MAX_REASON_BYTES])
    for i, c in enumerate(b):
        if c < 0x20 or c > 0x7E or c in (0x22, 0x5C):
            b[i] = 0x3F
    return b.decode("ascii")


def go_search(n: int, f) -> int:
    i, j = 0, n
    while i < j:
        h = (i + j) >> 1
        if not f(h):
            i = h + 1
        else:
            j = h
    return i


def canonical_book_decimal(raw: str) -> str:
    size = len(raw.encode("utf-8"))
    if size == 0 or size > CONTRACT["decimal_max_len"]:
        raise ValueError(f"decimal of {size} characters is empty or longer than {CONTRACT['decimal_max_len']}")
    int_part, dot, frac = raw.partition(".")
    if int_part == "" or (dot and frac == "") or "." in frac:
        raise ValueError(f"decimal {raw!r} is malformed")
    for c in raw:
        if c != "." and not ("0" <= c <= "9"):
            raise ValueError(f"decimal {raw!r} is not a plain positive ASCII decimal")
    int_part = int_part.lstrip("0") or "0"
    frac = frac.rstrip("0")
    if len(frac) > CONTRACT["decimal_max_frac"]:
        raise ValueError(f"decimal {raw!r} has more than {CONTRACT['decimal_max_frac']} fractional digits")
    out = int_part + ("." + frac if frac else "")
    if out == "0":
        raise ValueError(f"decimal {raw!r} is not positive")
    return out


def validate_book_sides(bids: list, asks: list) -> Optional[str]:
    if not bids or not asks:
        return "empty_side: a book side carries no level"
    if len(bids) > CONTRACT["max_levels"] or len(asks) > CONTRACT["max_levels"]:
        return f"too_many_levels: {len(bids)} bids and {len(asks)} asks, over {CONTRACT['max_levels']}"
    for side, levels in (("bid", bids), ("ask", asks)):
        for i, (px, sz, n) in enumerate(levels):
            for name, v in (("px", px), ("sz", sz)):
                try:
                    ok = canonical_book_decimal(v) == v
                except ValueError:
                    ok = False
                if not ok:
                    return f"invalid_level: {side} {i} {name} {v!r} is not a canonical positive decimal"
            if n == 0 or n > CONTRACT["max_orders"]:
                return f"invalid_level: {side} {i} order count {n} is outside [1, {CONTRACT['max_orders']}]"
    for i in range(1, len(bids)):
        if Fraction(bids[i][0]) >= Fraction(bids[i - 1][0]):
            return f"unsorted_side: bid {i} is not strictly below bid {i - 1}"
    for i in range(1, len(asks)):
        if Fraction(asks[i][0]) <= Fraction(asks[i - 1][0]):
            return f"unsorted_side: ask {i} is not strictly above ask {i - 1}"
    if Fraction(bids[0][0]) >= Fraction(asks[0][0]):
        return "crossed_book: the best bid is not strictly below the best ask"
    return None


def _lp(buf: bytearray, s: str) -> None:
    b = s.encode("utf-8")
    buf += str(len(b)).encode("ascii") + b":" + b + b";"


def book_identity(coin: str, event_ms: int, bids: list, asks: list, host: str = HOST, namespace: str = NAMESPACE) -> str:
    buf = bytearray()
    _lp(buf, CONTRACT["identity_tag"])
    _lp(buf, host)
    _lp(buf, namespace)
    _lp(buf, coin)
    _lp(buf, str(event_ms))
    for side in (bids, asks):
        _lp(buf, str(len(side)))
        for px, sz, n in side:
            _lp(buf, px)
            _lp(buf, sz)
            _lp(buf, str(n))
    return hashlib.sha256(bytes(buf)).hexdigest()


def _decode_level(value) -> dict:
    return _go_struct(value, {"px": (_go_string, ""), "sz": (_go_string, ""), "n": (_go_number_text, "")})


def _decode_sides(values: list):
    out = None
    for v in values:
        if v is None:
            out = None
            continue
        if not isinstance(v, list) or isinstance(v, _Obj):
            raise _GoTypeError("levels is not an array")
        sides = []
        for side in v:
            if side is None:
                sides.append([])
                continue
            if not isinstance(side, list) or isinstance(side, _Obj):
                raise _GoTypeError("a level side is not an array")
            sides.append([_decode_level(level) for level in side])
        out = sides
    return out


def _parse_levels(levels: list, side: str):
    if len(levels) > CONTRACT["max_levels"]:
        return None, f"too_many_levels: {side} side carries {len(levels)} levels, over {CONTRACT['max_levels']}"
    out = []
    for i, level in enumerate(levels):
        try:
            px = canonical_book_decimal(level["px"])
        except ValueError as e:
            return None, f"invalid_level: {side} {i} px: {e}"
        try:
            sz = canonical_book_decimal(level["sz"])
        except ValueError as e:
            return None, f"invalid_level: {side} {i} sz: {e}"
        n = _go_uint(level["n"])
        if n is None or n == 0 or n > CONTRACT["max_orders"]:
            return None, f"invalid_level: {side} {i} order count {level['n']!r} is not an integer in [1, {CONTRACT['max_orders']}]"
        out.append([px, sz, n])
    return out, None


def parse_book_data(data) -> dict:
    out = {"coin": "", "event_ms": 0, "bids": None, "asks": None, "identity": "", "err": None}
    if data is _MISSING:
        out["err"] = "malformed: the l2Book data does not decode"
        return out
    try:
        msg = _go_struct(data, {"coin": (_go_string, ""), "time": (_go_number_text, ""), "levels": (_decode_sides, None)})
    except _GoTypeError:
        out["err"] = "malformed: the l2Book data does not decode"
        return out
    coin = msg["coin"]
    if coin == "" or len(coin.encode("utf-8")) > MAX_COIN_BYTES or go_trim_space(coin) != coin:
        out["err"] = "malformed: the l2Book frame carries no usable coin"
        return out
    out["coin"] = coin
    event = _go_int(msg["time"], -(1 << 63), (1 << 63) - 1)
    if event is None or event <= 0:
        out["err"] = f"malformed: time {msg['time']!r} is not a positive millisecond timestamp"
        return out
    out["event_ms"] = event
    levels = msg["levels"] or []
    if len(levels) != 2:
        out["err"] = f"malformed: {len(levels)} level sides, want 2"
        return out
    bids, err = _parse_levels(levels[0], "bid")
    if err:
        out["err"] = err
        return out
    asks, err = _parse_levels(levels[1], "ask")
    if err:
        out["err"] = err
        return out
    err = validate_book_sides(bids, asks)
    if err:
        out["err"] = err
        return out
    out["bids"], out["asks"] = bids, asks
    out["identity"] = book_identity(coin, event, bids, asks)
    return out


def parse_envelope(raw: str):
    try:
        env = _go_loads(raw)
    except RecursionError:
        raise UnreplayableFrame("the message nests deeper than this replay can decode")
    except ValueError:
        return None, None, False
    if env is None:
        return "", _MISSING, True
    if not isinstance(env, _Obj):
        return None, None, False
    try:
        channel = _go_string(_go_field(env, "channel"))
    except _GoTypeError:
        return None, None, False
    datas = _go_field(env, "data")
    return channel, (datas[-1] if datas else _MISSING), True


def ack_contract_error(sub: dict) -> str:
    for name in ("nSigFigs", "mantissa"):
        if name in sub and sub[name] is not None:
            return f"the acknowledgement echoes {name}="
    if "fast" not in sub:
        return "the acknowledgement does not echo fast=false"
    if sub["fast"] is not False:
        return "the acknowledgement echoes fast="
    return ""


def _decode_ack(raw: str):
    channel, data, ok = parse_envelope(raw)
    if not ok or channel != "subscriptionResponse" or data is _MISSING:
        return None
    try:
        ack = _go_struct(data, {"method": (_go_string, ""), "subscription": (
            lambda vals: _go_struct(vals[0], {"type": (_go_string, ""), "coin": (_go_string, "")}), None)})
    except _GoTypeError:
        return None
    sub = ack["subscription"] or {"type": "", "coin": ""}
    if ack["method"] != "subscribe" or sub["type"] != "l2Book" or sub["coin"] == "":
        return None
    subs = _go_field(data, "subscription") if isinstance(data, _Obj) else []
    mapping = {}
    if subs and subs[0] is not None:
        if not isinstance(subs[0], _Obj):
            return None
        for k, v in subs[0]:
            mapping[k] = v
    return {"coin": sub["coin"], "sub": mapping}


def _detail_key(reason: str, detail: str) -> str:
    if reason in ("invalid_frame", "clock_regression"):
        return (detail or "").split(":", 1)[0]
    if reason == "subscription_failed":
        return (detail or "").split("=", 1)[0]
    return detail or ""


def _category(reason: str) -> str:
    return (reason or "").split(":", 1)[0]


class _BookState:
    def __init__(self, coin: str, need: dict, now_ms: int, instance: str):
        self.coin = coin
        self.need = dict(need)
        self.instance = instance
        self.obs: List[dict] = []
        self.markers: List[dict] = []
        self.covered = False
        self.session = 0
        self.high_recv = 0
        self.high_event = 0
        self.last_accept = 0
        self.silence_marked = False
        self.retained_from = now_ms
        self.depth_from = now_ms
        self.stats = {k: 0 for k in ("frames", "accepted", "duplicate", "conflict", "out_of_order", "rejected", "uncovered",
                                     "markers", "evicted")}

    def coverage_from(self) -> int:
        return max(self.retained_from, self.depth_from)

    def retain_floor(self, now_ms: int) -> int:
        return now_ms - self.need["window_ms"] - CONTRACT["retain_slack_ms"]


class _Owner:
    def __init__(self):
        self.seq = 0
        self.books: Dict[str, _BookState] = {}
        self.connected = False
        self.session = 0
        self.generated: List[dict] = []
        self.history: Dict[str, dict] = {}

    def next_seq(self) -> int:
        self.seq += 1
        return self.seq

    def _hist(self, coin: str) -> dict:
        if coin not in self.history:
            self.history[coin] = {"observations": [], "markers": [], "coverage": [], "needs": [], "latency_ms": [],
                                  "max_accept_gap_ms": 0, "stats": None}
        return self.history[coin]

    def _note_coverage(self, st: _BookState, at_ms: int) -> None:
        cov = self._hist(st.coin)["coverage"]
        value = st.coverage_from()
        if not cov or cov[-1][1] != value:
            cov.append([at_ms, value])

    def sorted_coins(self) -> list:
        return sorted(self.books, key=lambda c: c.encode("utf-8"))

    @staticmethod
    def marker_time(st: _BookState, at_ms: int) -> int:
        if st.markers and st.markers[-1]["at_ms"] > at_ms:
            return st.markers[-1]["at_ms"]
        return at_ms

    def add_marker(self, st: _BookState, at_ms: int, reason: str, detail: str) -> None:
        at_ms = self.marker_time(st, at_ms)
        m = {"seq": self.next_seq(), "at_ms": at_ms, "reason": reason, "session": st.session,
             "detail": bounded_reason(detail), "evicted_ms": None}
        st.markers.append(m)
        st.stats["markers"] += 1
        self._hist(st.coin)["markers"].append(m)
        self.generated.append((st.coin, m))
        if len(st.markers) > CONTRACT["max_markers"]:
            self._coalesce(st, at_ms)

    def _coalesce(self, st: _BookState, at_ms: int) -> None:
        drop = len(st.markers) - CONTRACT["max_markers"] + 1
        cut = st.markers[drop - 1]
        keep = go_search(len(st.obs), lambda i: st.obs[i]["seq"] > cut["seq"])
        if keep > 0:
            st.stats["evicted"] += keep
            for o in st.obs[:keep]:
                o["evicted_ms"] = at_ms
            st.obs = st.obs[keep:]
        for m in st.markers[:drop]:
            m["evicted_ms"] = at_ms
        st.markers = st.markers[drop:]
        if cut["at_ms"] + 1 > st.retained_from:
            st.retained_from = cut["at_ms"] + 1
            self._note_coverage(st, at_ms)
        m = {"seq": self.next_seq(), "at_ms": at_ms, "reason": "evidence_loss", "session": st.session,
             "detail": bounded_reason(f"{drop} markers through seq {cut['seq']} coalesced; history is complete only from {st.retained_from}"),
             "evicted_ms": None}
        st.markers.append(m)
        st.stats["markers"] += 1
        self._hist(st.coin)["markers"].append(m)
        self.generated.append((st.coin, m))

    def trim(self, st: _BookState, now_ms: int) -> None:
        floor = st.retain_floor(now_ms)
        cut = go_search(len(st.obs), lambda i: st.obs[i]["recv_ms"] >= floor)
        over = len(st.obs) - cut - st.need["capacity"]
        if over > 0:
            cut += over
        if cut > 0:
            last = st.obs[cut - 1]["recv_ms"]
            if last + 1 > st.retained_from:
                st.retained_from = last + 1
                self._note_coverage(st, now_ms)
            st.stats["evicted"] += cut
            for o in st.obs[:cut]:
                o["evicted_ms"] = now_ms
            st.obs = st.obs[cut:]
        mcut = go_search(len(st.markers), lambda i: st.markers[i]["at_ms"] >= floor)
        if mcut > 0:
            for m in st.markers[:mcut]:
                m["evicted_ms"] = now_ms
            st.markers = st.markers[mcut:]

    def find_event(self, st: _BookState, event_ms: int):
        i = go_search(len(st.obs), lambda k: st.obs[k]["event_ms"] >= event_ms)
        if i < len(st.obs) and st.obs[i]["event_ms"] == event_ms:
            return st.obs[i]
        return None

    def ingest(self, st: _BookState, p: dict, recv_ms: int, seq: int) -> dict:
        st.stats["frames"] += 1
        out = {"outcome": "", "reason": "", "event_ms": p["event_ms"], "identity": p["identity"]}

        def reject(reason: str, marker: str) -> dict:
            st.stats["rejected"] += 1
            out["outcome"], out["reason"] = "rejected", bounded_reason(reason)
            if st.covered and marker:
                self.add_marker(st, recv_ms, marker, reason)
            return out

        if recv_ms < st.high_recv:
            return reject(f"clock_regression: receipt {recv_ms} is before the receipt high-water mark {st.high_recv}", "clock_regression")
        st.high_recv = recv_ms
        if p["err"]:
            return reject(p["err"], "invalid_frame")
        if not self.connected or not st.covered or st.session != self.session:
            st.stats["uncovered"] += 1
            out["outcome"], out["reason"] = "uncovered", UNCOVERED_REASON
            return out
        if p["event_ms"] > recv_ms + CONTRACT["max_future_skew_ms"]:
            return reject(f"future_event: event {p['event_ms']} is more than {CONTRACT['max_future_skew_ms']}ms after receipt {recv_ms}", "invalid_frame")
        if recv_ms - p["event_ms"] > CONTRACT["max_lag_ms"]:
            return reject(f"stale_receipt: receipt {recv_ms} is more than {CONTRACT['max_lag_ms']}ms after event {p['event_ms']}", "invalid_frame")
        prior = self.find_event(st, p["event_ms"])
        if prior is not None:
            if prior["identity"] == p["identity"]:
                st.stats["duplicate"] += 1
                out["outcome"] = "duplicate"
                return out
            st.stats["conflict"] += 1
            out["outcome"], out["reason"] = "conflict", CONFLICT_REASON
            self.add_marker(st, recv_ms, "conflict", CONFLICT_REASON)
            return out
        if p["event_ms"] <= st.high_event:
            st.stats["out_of_order"] += 1
            out["outcome"] = "out_of_order"
            out["reason"] = f"event {p['event_ms']} is not after the event high-water mark {st.high_event}"
            self.add_marker(st, recv_ms, "out_of_order", out["reason"])
            return out
        gap = CONTRACT["max_gap_ms"]
        if st.last_accept > 0 and not st.silence_marked and recv_ms - st.last_accept > gap:
            self.add_marker(st, recv_ms, "silence", f"receipt gap {recv_ms - st.last_accept}ms over {gap}ms")
        elif st.high_event > 0 and not st.silence_marked and p["event_ms"] - st.high_event > gap:
            self.add_marker(st, recv_ms, "silence", f"event gap {p['event_ms'] - st.high_event}ms over {gap}ms")
        hist = self._hist(st.coin)
        if st.last_accept > 0:
            hist["max_accept_gap_ms"] = max(hist["max_accept_gap_ms"], recv_ms - st.last_accept)
        depth = st.need["depth"]
        obs = {"seq": seq, "event_ms": p["event_ms"], "recv_ms": recv_ms, "session": self.session, "identity": p["identity"],
               "bid_depth": len(p["bids"]), "ask_depth": len(p["asks"]), "bids": [list(l) for l in p["bids"][:depth]],
               "asks": [list(l) for l in p["asks"][:depth]], "evicted_ms": None}
        st.obs.append(obs)
        hist["observations"].append(obs)
        hist["latency_ms"].append(recv_ms - p["event_ms"])
        st.stats["accepted"] += 1
        st.high_event = p["event_ms"]
        st.last_accept = recv_ms
        st.silence_marked = False
        out["outcome"] = "accepted"
        self.trim(st, recv_ms)
        return out

    def malformed(self, at_ms: int, reason: str) -> None:
        for coin in self.sorted_coins():
            st = self.books[coin]
            st.stats["rejected"] += 1
            if st.covered:
                self.add_marker(st, at_ms, "invalid_frame", reason)

    def apply_need(self, coin: str, need: dict, now_ms: int, instance: str) -> None:
        st = self.books.get(coin)
        hist = self._hist(coin)
        if st is None:
            st = _BookState(coin, need, now_ms, instance)
            self.books[coin] = st
            hist["needs"].append({"at_ms": now_ms, **need, "instance": instance, "released": False})
            self._note_coverage(st, now_ms)
            self.add_marker(st, now_ms, "epoch_start", "owner " + instance)
            return
        if need["depth"] > st.need["depth"]:
            st.depth_from = now_ms
            st.stats["evicted"] += len(st.obs)
            for o in st.obs:
                o["evicted_ms"] = now_ms
            st.obs = []
            self._note_coverage(st, now_ms)
            self.add_marker(st, now_ms, "depth_raised", f"depth raised from {st.need['depth']} to {need['depth']} levels")
        st.need = dict(need)
        hist["needs"].append({"at_ms": now_ms, **need, "instance": st.instance, "released": False})
        self.trim(st, now_ms)

    def release(self, coin: str, now_ms: int) -> None:
        st = self.books.pop(coin)
        hist = self._hist(coin)
        hist["needs"].append({"at_ms": now_ms, **st.need, "instance": st.instance, "released": True})
        for o in st.obs:
            o["evicted_ms"] = now_ms
        for m in st.markers:
            m["evicted_ms"] = now_ms
        hist["stats"] = dict(st.stats)

    def disconnect(self, at_ms: int) -> None:
        for coin in self.sorted_coins():
            st = self.books[coin]
            if not st.covered:
                continue
            self.add_marker(st, at_ms, "disconnected", "")
            st.covered = False

    def tick(self, coin: str, now_ms: int) -> bool:
        st = self.books.get(coin)
        gap = CONTRACT["max_gap_ms"]
        if st is None:
            return False
        at = self.marker_time(st, now_ms)
        if not st.covered or st.silence_marked or st.last_accept == 0 or at - st.last_accept <= gap:
            return False
        self.add_marker(st, at, "silence", f"no accepted book for {at - st.last_accept}ms, over {gap}ms")
        st.silence_marked = True
        return True

    def subscription(self, coin: str, recv_ms: int, why: str) -> None:
        st = self.books[coin]
        if why:
            if st.covered:
                self.add_marker(st, recv_ms, "subscription_failed", why)
            st.covered = False
            return
        if st.covered and st.session == self.session:
            return
        st.covered, st.session = True, self.session
        st.last_accept, st.silence_marked = recv_ms, False
        self.add_marker(st, recv_ms, "subscribed", "")


def sha256_bytes(blob: bytes) -> str:
    return hashlib.sha256(blob).hexdigest()


def _read_segment_bytes(run_dir: str, name: str) -> bytes:
    plain = os.path.join(run_dir, name)
    if os.path.isfile(plain):
        with open(plain, "rb") as fh:
            return fh.read()
    if os.path.isfile(plain + ".gz"):
        with gzip.open(plain + ".gz", "rb") as fh:
            return fh.read()
    raise BookRecordingError(f"segment {name} is missing (looked for {name} and {name}.gz)")


def _same_marker(gen: tuple, rec: dict) -> bool:
    coin, m = gen
    same = (coin == rec.get("coin") and rec.get("kind") == KIND and m["seq"] == rec.get("seq")
            and m["at_ms"] == rec.get("at_ms") == rec.get("recv_ms") and m["reason"] == rec.get("reason")
            and m["session"] == rec.get("marker_session") and isinstance(rec.get("detail"), str)
            and _detail_key(m["reason"], m["detail"]) == _detail_key(rec.get("reason"), rec.get("detail")))
    if same and m["reason"] in ("invalid_frame", "clock_regression", "subscription_failed"):
        m["detail"] = rec["detail"]
    return same


def _percentile(values: list, q: float):
    if not values:
        return None
    s = sorted(values)
    return s[min(len(s) - 1, int(len(s) * q))]


def load_book_recording(run_dir: str) -> dict:
    run_dir = os.path.abspath(run_dir)
    with open(os.path.join(run_dir, "run.manifest.json")) as fh:
        run = json.load(fh)
    if run.get("schema") != RUN_SCHEMA_V3:
        raise BookRecordingError(f"run manifest schema must be {RUN_SCHEMA_V3!r}, got {run.get('schema')!r}")
    if KIND not in (run.get("kinds") or []):
        raise BookRecordingError("the run did not record l2_book")
    if run.get("book_contract") != CONTRACT:
        raise BookRecordingError(f"the run recorded book contract {run.get('book_contract')!r}; this importer accepts only {CONTRACT!r}")
    if (run.get("sources") or {}).get(KIND) != CONTRACT["source"]:
        raise BookRecordingError("the run declares a different l2_book source")
    coins = list(run.get("coins") or [])
    if not coins or coins != sorted(coins):
        raise BookRecordingError("the run lists no coin or lists coins out of order")
    segments = run.get("segments") or []
    if not segments:
        raise BookRecordingError("the run has no closed segment")
    owner = _Owner()
    refusals: List[str] = []
    mismatches: List[str] = []
    report = {"run_id": run.get("run_id"), "recorder_version": run.get("recorder_version"), "coins": coins,
              "started_ms": run.get("started_ms"), "closed_ms": run.get("closed_ms"), "segments": [], "frames": 0,
              "malformed_frames": 0, "markers_compared": 0, "dropped": 0, "sessions": set()}
    if not run.get("closed"):
        refusals.append("the recording run was not closed; its last segment may be truncated")
    deferred: List[dict] = []

    def mismatch(text: str) -> None:
        mismatches.append(text)

    def drain(where: str) -> None:
        nonlocal deferred
        if deferred:
            for rec in deferred:
                if owner.generated:
                    gen = owner.generated.pop(0)
                    report["markers_compared"] += 1
                    if not _same_marker(gen, rec):
                        mismatch(f"{where}: marker {rec.get('coin')}/{rec.get('seq')}/{rec.get('reason')} disagrees with the replay {gen[1]}")
                else:
                    mismatch(f"{where}: the recorder wrote marker {rec.get('coin')}/{rec.get('seq')}/{rec.get('reason')} that the replay cannot explain")
            deferred = []

    def settle(where: str) -> None:
        drain(where)
        if owner.generated:
            mismatch(f"{where}: the recording is missing {len(owner.generated)} marker(s) the replay produced, first {owner.generated[0][1]}")
            owner.generated.clear()

    for expected_index, seg in enumerate(segments, start=1):
        if seg.get("schema") != SEGMENT_SCHEMA_V3 or seg.get("index") != expected_index:
            raise BookRecordingError(f"segment manifest {expected_index} is missing or out of order")
        with open(os.path.join(run_dir, seg["file"] + ".manifest.json")) as fh:
            if json.load(fh) != seg:
                raise BookRecordingError(f"segment {seg['file']}: its manifest file disagrees with the run manifest")
        blob = _read_segment_bytes(run_dir, seg["file"])
        digest = sha256_bytes(blob)
        if digest != seg.get("sha256"):
            raise BookRecordingError(f"segment {seg['file']}: sha256 {digest} does not match the manifest {seg.get('sha256')}")
        if not blob.endswith(b"\n"):
            raise BookRecordingError(f"segment {seg['file']} is truncated (no final newline)")
        lines = blob.decode("utf-8").split("\n")[:-1]
        if len(lines) < 2:
            raise BookRecordingError(f"segment {seg['file']} has no header and end record")
        header, end = json.loads(lines[0]), json.loads(lines[-1])
        if (header.get("k") != "header" or header.get("schema") != RECORDING_SCHEMA_V3 or header.get("run_id") != run.get("run_id")
                or header.get("book_contract") != CONTRACT or header.get("coins") != coins
                or (header.get("sources") or {}).get(KIND) != CONTRACT["source"]):
            raise BookRecordingError(f"segment {seg['file']} header does not match the run")
        if end.get("k") != "end" or end.get("records") != len(lines) - 2 or end.get("records") != seg.get("records"):
            raise BookRecordingError(f"segment {seg['file']} end record is missing or miscounts its records")
        if not seg.get("complete"):
            refusals.append(f"segment {seg['file']} is marked incomplete")
        if seg.get("dropped"):
            refusals.append(f"segment {seg['file']} dropped {seg.get('dropped')} record(s)")
        seg_books = seg_markers = 0
        for line_no, line in enumerate(lines[1:-1], start=2):
            where = f"{seg['file']}:{line_no}"
            rec = json.loads(line)
            kind = rec.get("k")
            recv = rec.get("recv_ms")
            if not isinstance(recv, int) or isinstance(recv, bool):
                raise BookRecordingError(f"{where} has no integer recv_ms")
            report["sessions"].add(rec.get("session"))
            if kind == "book_marker":
                seg_markers += 1
                if rec.get("reason") not in MARKER_REASONS:
                    mismatch(f"{where}: marker reason {rec.get('reason')!r} is not in the contract")
                if deferred or not owner.generated:
                    if not deferred and rec.get("reason") == "silence" and rec.get("at_ms") == recv:
                        if owner.tick(rec.get("coin"), recv):
                            gen = owner.generated.pop(0)
                            report["markers_compared"] += 1
                            if not _same_marker(gen, rec):
                                mismatch(f"{where}: timer silence marker disagrees with the replay {gen}")
                        else:
                            mismatch(f"{where}: the recorder wrote a timer silence marker for {rec.get('coin')} the replayed state does not allow")
                        continue
                    deferred.append(rec)
                    continue
                gen = owner.generated.pop(0)
                report["markers_compared"] += 1
                if not _same_marker(gen, rec):
                    mismatch(f"{where}: marker {rec.get('coin')}/{rec.get('seq')}/{rec.get('reason')} disagrees with the replay {gen}")
                continue
            if kind not in BOOK_RECORD_KINDS and kind not in ("conn", "drop"):
                continue
            if kind == "conn":
                if rec.get("state") == "connected":
                    settle(where)
                    owner.connected, owner.session = True, rec.get("session")
                elif rec.get("state") == "disconnected":
                    if owner.generated:
                        settle(where)
                    owner.disconnect(recv)
                    owner.connected = False
                    drain(where)
                else:
                    raise BookRecordingError(f"{where} has unknown connection state {rec.get('state')!r}")
                continue
            if kind == "book_need":
                if owner.generated:
                    settle(where)
                coin = rec.get("coin")
                need = {"window_ms": rec.get("window_ms"), "capacity": rec.get("capacity"), "depth": rec.get("depth")}
                if (coin not in coins or rec.get("kind") != KIND
                        or not all(isinstance(v, int) and not isinstance(v, bool) and v > 0 for v in need.values())
                        or need["window_ms"] > CONTRACT["max_window_ms"] or need["capacity"] > CONTRACT["max_observations"]
                        or need["depth"] > CONTRACT["max_levels"]):
                    mismatch(f"{where}: book need {rec} is outside the run or the contract")
                    deferred = []
                    continue
                owner.apply_need(coin, need, recv, str(rec.get("instance") or ""))
                drain(where)
                continue
            settle(where)
            if kind == "drop":
                report["dropped"] += int(rec.get("count") or 0)
                refusals.append(f"{where}: recorder queue dropped {rec.get('count')} record(s)")
                continue
            if kind == "book_release":
                coin = rec.get("coin")
                if coin in owner.books:
                    owner.release(coin, recv)
                else:
                    mismatch(f"{where}: release of untracked {coin!r}")
                continue
            if rec.get("session") != owner.session:
                mismatch(f"{where}: record session {rec.get('session')} is not the connection session {owner.session}")
            if kind == "book_sub":
                coin = rec.get("coin")
                if coin not in owner.books:
                    mismatch(f"{where}: subscription record for untracked coin {coin!r}")
                    continue
                try:
                    ack = _decode_ack(rec.get("raw") or "")
                except UnreplayableFrame as exc:
                    mismatch(f"{where}: {exc}")
                    continue
                if ack is None or ack["coin"] != coin:
                    mismatch(f"{where}: the recorded acknowledgement does not decode to an l2Book subscribe for {coin}")
                    continue
                why = ack_contract_error(ack["sub"])
                if not owner.connected and not why:
                    why = "the socket is not connected"
                if (why == "") != (rec.get("accepted") is True) or _detail_key("subscription_failed", why) != _detail_key("subscription_failed", rec.get("reason")):
                    mismatch(f"{where}: {coin} acknowledgement accepted={rec.get('accepted')} {rec.get('reason')!r}, replay {why or 'accepted'}")
                    continue
                owner.subscription(coin, recv, bounded_reason(rec.get("reason") or "") if why else "")
                continue
            if kind != "book":
                continue
            seg_books += 1
            report["frames"] += 1
            if rec.get("kind") != KIND or rec.get("recv_ms") != recv:
                mismatch(f"{where}: book record is mislabeled")
            if not owner.books:
                mismatch(f"{where}: the recorder wrote a book frame while no book key was tracked")
                continue
            seq = owner.next_seq()
            if rec.get("seq") != seq:
                mismatch(f"{where}: frame sequence {rec.get('seq')}, replay {seq}")
                owner.seq = rec.get("seq") if isinstance(rec.get("seq"), int) else owner.seq
            raw = rec.get("raw")
            size = rec.get("bytes")
            if raw is None:
                if not isinstance(size, int) or size <= CONTRACT["max_frame_bytes"] or not isinstance(rec.get("raw_sha256"), str):
                    mismatch(f"{where}: a frame without raw text must be an oversized frame with its hash")
                    continue
                reasons = {ENVELOPE_REASON, f"malformed: the frame is {size} bytes, over the {CONTRACT['max_frame_bytes']}-byte bound"}
                report["malformed_frames"] += 1
                if rec.get("coin") != "" or rec.get("outcome") != "rejected" or rec.get("reason") not in reasons:
                    mismatch(f"{where}: oversized frame recorded as {rec.get('coin')!r}/{rec.get('outcome')!r}/{rec.get('reason')!r}")
                owner.malformed(recv, rec.get("reason") if rec.get("reason") in reasons else ENVELOPE_REASON)
                continue
            if size != len(raw.encode("utf-8")) or size > CONTRACT["max_frame_bytes"]:
                mismatch(f"{where}: recorded size {size} does not match its raw text")
            try:
                channel, data, ok = parse_envelope(raw)
                p = parse_book_data(data) if ok and channel == "l2Book" else None
            except UnreplayableFrame as exc:
                mismatch(f"{where}: {exc}")
                continue
            if not ok:
                report["malformed_frames"] += 1
                if rec.get("coin") != "" or rec.get("outcome") != "rejected" or rec.get("reason") != ENVELOPE_REASON:
                    mismatch(f"{where}: a non-JSON envelope recorded as {rec.get('coin')!r}/{rec.get('outcome')!r}/{rec.get('reason')!r}")
                owner.malformed(recv, ENVELOPE_REASON)
                continue
            if channel != "l2Book":
                mismatch(f"{where}: book record holds channel {channel!r}")
                continue
            if rec.get("coin") != p["coin"]:
                mismatch(f"{where}: recorder coin {rec.get('coin')!r}, replay {p['coin']!r}")
            if p["coin"] == "":
                report["malformed_frames"] += 1
                reason = bounded_reason(p["err"])
                if rec.get("outcome") != "rejected" or _category(rec.get("reason")) != _category(reason):
                    mismatch(f"{where}: unattributable book recorded as {rec.get('outcome')!r} {rec.get('reason')!r}, replay {reason!r}")
                owner.malformed(recv, rec.get("reason") if _category(rec.get("reason")) == "malformed" else reason)
                continue
            st = owner.books.get(p["coin"])
            if st is None:
                if rec.get("outcome") != "untracked":
                    mismatch(f"{where}: untracked {p['coin']} book recorded as {rec.get('outcome')!r}")
                continue
            got = owner.ingest(st, p, recv, seq)
            want_reason = rec.get("reason") or ""
            same = (rec.get("outcome") == got["outcome"] and rec.get("event_ms") == got["event_ms"]
                    and (rec.get("identity") or "") == got["identity"])
            if got["outcome"] == "rejected":
                same = same and _category(want_reason) == _category(got["reason"])
            else:
                same = same and want_reason == got["reason"]
            if not same:
                mismatch(f"{where}: {p['coin']} book recorded {{outcome: {rec.get('outcome')}, reason: {want_reason!r}, event_ms: "
                         f"{rec.get('event_ms')}, identity: {rec.get('identity')}}}, replay {got}")
        settle(f"{seg['file']}:end")
        if seg.get("book_frames", 0) != seg_books or seg.get("book_markers", 0) != seg_markers:
            mismatch(f"segment {seg['file']} declares {seg.get('book_frames')} book frames and {seg.get('book_markers')} markers; "
                     f"it holds {seg_books} and {seg_markers}")
        report["segments"].append({"file": seg["file"], "sha256": digest, "records": seg.get("records"),
                                   "first_recv_ms": seg.get("first_recv_ms"), "last_recv_ms": seg.get("last_recv_ms"),
                                   "book_frames": seg_books, "book_markers": seg_markers})
    if mismatches:
        refusals.append(f"{len(mismatches)} replay outcomes disagree with the recorder (first: {mismatches[0]})")
    report["sessions"] = sorted(s for s in report["sessions"] if isinstance(s, int))
    report["mismatches"] = mismatches[:50]
    report["mismatch_count"] = len(mismatches)
    report["refusals"] = refusals
    report["economic_evidence"] = not refusals
    series = {}
    for coin in coins:
        hist = owner._hist(coin)
        st = owner.books.get(coin)
        stats = dict(st.stats) if st is not None else (hist["stats"] or {})
        lat = hist["latency_ms"]
        series[coin] = {"schema": SERIES_SCHEMA, "coin": coin, "host": HOST, "namespace": NAMESPACE, "kind": KIND,
                        "contract": CONTRACT, "observations": hist["observations"], "markers": hist["markers"],
                        "coverage": hist["coverage"], "needs": hist["needs"], "stats": stats,
                        "max_accept_gap_ms": hist["max_accept_gap_ms"],
                        "latency_ms": {"n": len(lat), "p50": _percentile(lat, 0.5), "p99": _percentile(lat, 0.99),
                                       "max": max(lat) if lat else None}}
        series[coin]["series_sha256"] = series_sha256(series[coin])
    return {"report": report, "series": series}


def series_sha256(series: dict) -> str:
    body = {k: series[k] for k in ("coin", "host", "namespace", "kind", "contract", "observations", "markers", "coverage", "needs")}
    return hashlib.sha256(json.dumps(body, sort_keys=True, separators=(",", ":")).encode("utf-8")).hexdigest()


def _as_of(rows: list, at_key, as_of_ms: int):
    out = None
    for r in rows:
        if at_key(r) < as_of_ms:
            out = r
    return out


def payload_from_series(series: dict, cutoff_ms: int, need: dict, as_of_ms: int, snapshot_id: str = "") -> dict:
    coin = series["coin"]
    out = {"kind": KIND, "coin": coin, **{k: CONTRACT[k] for k in PAYLOAD_CONTRACT_KEYS}, "window_ms": need["window_ms"],
           "capacity": need["capacity"], "depth": need["depth"], "cutoff_ms": cutoff_ms, "retained_from_ms": 0,
           "prefix_complete": False, "owner_instance": "", "snapshot_id": snapshot_id, "available": False,
           "observations": [], "markers": []}
    owner_need = _as_of(series["needs"], lambda r: r["at_ms"], as_of_ms)
    key = f"{coin}|{KIND}"
    if owner_need is None or owner_need["released"]:
        out["reason"] = f"{key} is not in the sealed snapshot"
        return out
    out["owner_instance"] = owner_need["instance"]
    for dim, label in (("window_ms", "window"), ("capacity", "capacity"), ("depth", "depth")):
        if owner_need[dim] < need[dim]:
            out["reason"] = f"{key} owner {label} {owner_need[dim]} is below the {need[dim]} this need requires"
            return out
    cov = _as_of(series["coverage"], lambda r: r[0], as_of_ms)
    coverage_from = cov[1] if cov else 0
    start = cutoff_ms - need["window_ms"]

    def present(row: dict, at: int) -> bool:
        return at < as_of_ms and (row["evicted_ms"] is None or row["evicted_ms"] >= as_of_ms)

    obs = [o for o in series["observations"] if present(o, o["recv_ms"]) and start <= o["recv_ms"] <= cutoff_ms]
    out["retained_from_ms"] = coverage_from
    out["prefix_complete"] = coverage_from <= start
    skip = max(0, len(obs) - need["capacity"])
    if skip:
        out["prefix_complete"] = False
        out["retained_from_ms"] = max(coverage_from, obs[skip - 1]["recv_ms"] + 1)
    depth = need["depth"]
    out["observations"] = [{"seq": o["seq"], "event_ms": o["event_ms"], "recv_ms": o["recv_ms"], "session": o["session"],
                            "identity": o["identity"], "bid_depth": o["bid_depth"], "ask_depth": o["ask_depth"],
                            "bids": [list(l) for l in o["bids"][:depth]], "asks": [list(l) for l in o["asks"][:depth]]}
                           for o in obs[skip:]]
    markers = []
    for m in series["markers"]:
        if not present(m, m["at_ms"]) or not start <= m["at_ms"] <= cutoff_ms:
            continue
        row = {"seq": m["seq"], "at_ms": m["at_ms"], "reason": m["reason"], "session": m["session"]}
        if m["detail"]:
            row["detail"] = m["detail"]
        markers.append(row)
    out["markers"] = markers
    out["available"] = True
    return out


COMPARABLE_KEYS = ("kind", "coin", *PAYLOAD_CONTRACT_KEYS, "window_ms", "capacity", "depth", "cutoff_ms", "retained_from_ms",
                   "prefix_complete", "owner_instance", "available", "observations", "markers")


def comparable_payload(p: dict) -> dict:
    return {k: p.get(k) for k in COMPARABLE_KEYS}


def write_book_series(series: dict, path: str) -> str:
    blob = json.dumps(series, sort_keys=True, separators=(",", ":")).encode("utf-8")
    with gzip.GzipFile(path, "wb", mtime=0) as fh:
        fh.write(blob)
    return sha256_bytes(blob)


def read_book_series(path: str) -> dict:
    with gzip.open(path, "rb") as fh:
        blob = fh.read()
    series = json.loads(blob)
    if not isinstance(series, dict) or series.get("schema") != SERIES_SCHEMA or series.get("contract") != CONTRACT:
        raise BookRecordingError(f"{path} is not a {SERIES_SCHEMA} series under the frozen contract")
    if series_sha256(series) != series.get("series_sha256"):
        raise BookRecordingError(f"{path}: its content does not match its series hash")
    return series
