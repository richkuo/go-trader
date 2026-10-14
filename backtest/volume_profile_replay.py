import gzip
import hashlib
import io
import json
import os
import sys
from typing import Dict, Optional

_HERE = os.path.dirname(os.path.abspath(__file__))
_TOOLS = os.path.abspath(os.path.join(_HERE, "..", "shared_tools"))
if _TOOLS not in sys.path:
    sys.path.insert(0, _TOOLS)

from trade_flow_replay import TradeRecordingError, load_trade_recording, sha256_bytes  # noqa: E402
from volume_profile_inputs import (  # noqa: E402
    CONTRACT,
    INT64_MAX,
    SOURCES,
    KIND,
    MINUTES,
    SESSION_MS,
    STATUS_AVAILABLE,
    STATUS_INVALIDATED,
    STATUS_PENDING,
    STATUS_UNAVAILABLE,
    DUAL_WINDOW_MS,
    WINDOW_MS,
    artifact_sha256,
    compare_decimal,
    format_units,
    pair_bytes,
    sealed_sessions,
    session_start,
)

SERIES_SCHEMA = "go_trader_volume_profile_series/v1"
VOLUME_PROFILE_INPUT_STRATEGIES = frozenset({"volume_profile_reclaim"})
PAYLOAD_KEYS = ("schema", "source", "units", "identity", "time_basis", "session_ms", "interval_ms", "reorder_grace_ms",
                "decimal_scale", "max_entries", "max_price_chars", "max_pair_bytes")
HASH_MOD = 1 << 256
MAX_REASON_CHARS = 240

DAY_ACCUMULATING = "accumulating"
DAY_FINAL = "final"
DAY_UNAVAILABLE = "unavailable"


def refuse_volume_profile_strategy(name: str, consumer: str) -> None:
    if str(name or "").strip() in VOLUME_PROFILE_INPUT_STRATEGIES:
        raise ValueError(f"{name} reads executed-trade volume profiles, which {consumer} does not prepare; run it only "
                         "through run_backtest.py --mode single --manifest or eval_windows.py --manifest with a "
                         "volume_profile series")


def _bounded(text: str) -> str:
    return text[:MAX_REASON_CHARS]


class _Day:
    def __init__(self, start: int):
        self.start = start
        self.status = DAY_ACCUMULATING
        self.code = ""
        self.detail = ""
        self.unavailable_ms = 0
        self.complete = 0
        self.seen = [False] * MINUTES
        self.prices: Optional[Dict[str, int]] = {}
        self.trades = 0
        self.units = 0
        self.hash = 0
        self.first = 0
        self.last = 0
        self.artifact = None
        self.invalid_ms = 0
        self.invalid_code = ""
        self.invalid_why = ""


class ProfileReplay:
    def __init__(self, coin: str):
        self.coin = coin
        self.days: Dict[int, _Day] = {}
        self.history: Dict[int, _Day] = {}
        self.stats = {"accepted": 0, "finalized": 0, "unavailable": 0, "invalidated": 0}

    @staticmethod
    def _floor(now_ms: int) -> int:
        return session_start(now_ms - DUAL_WINDOW_MS) - SESSION_MS

    def _day(self, start_ms: int, now_ms: int, create: bool):
        day = session_start(start_ms)
        d = self.days.get(day)
        if d is not None:
            return d
        if not create or day < self._floor(now_ms):
            return None
        d = _Day(day)
        self.days[day] = d
        return d

    def _unavailable(self, d: _Day, at_ms: int, code: str, detail: str):
        if d.status != DAY_ACCUMULATING:
            return
        d.status = DAY_UNAVAILABLE
        d.code = code
        d.detail = _bounded(detail)
        d.unavailable_ms = at_ms
        d.prices = None
        self.stats["unavailable"] += 1

    def add_trade(self, t: dict, digest: bytes, start_ms: int, recv_ms: int):
        d = self._day(start_ms, recv_ms, True)
        if d is None or d.status != DAY_ACCUMULATING:
            return
        self.stats["accepted"] += 1
        px = t["px"]
        if len(px) > CONTRACT["max_price_chars"]:
            self._unavailable(d, recv_ms, "price_field_length",
                              f"trade {t['tid']} price {px[:CONTRACT['max_price_chars']]} is longer than {CONTRACT['max_price_chars']} characters")
            return
        prior = d.prices.get(px)
        if prior is None and len(d.prices) >= CONTRACT["max_entries"]:
            self._unavailable(d, recv_ms, "price_entries_over_limit",
                              f"session holds more than {CONTRACT['max_entries']} distinct prices")
            return
        total = (prior or 0) + t["units"]
        if total > INT64_MAX or d.units + t["units"] > INT64_MAX:
            self._unavailable(d, recv_ms, "volume_overflow", f"trade {t['tid']} overflows the exact volume accumulator")
            return
        d.prices[px] = total
        d.units += t["units"]
        d.trades += 1
        d.hash = (d.hash + int.from_bytes(digest, "big")) % HASH_MOD
        if d.first == 0 or t["time"] < d.first:
            d.first = t["time"]
        if t["time"] > d.last:
            d.last = t["time"]

    def finalize_minute(self, iv, now_ms: int):
        d = self._day(iv.start, now_ms, True)
        if d is None:
            return
        idx = (iv.start - d.start) // CONTRACT["interval_ms"]
        if idx < 0 or idx >= MINUTES:
            return
        if d.seen[idx]:
            self._unavailable(d, now_ms, "minute_refinalized", f"minute {iv.start} finalized twice")
            return
        d.seen[idx] = True
        if iv.status != "complete" or iv.invalid != 0:
            self._unavailable(d, now_ms, "minute_invalid", f"minute {iv.start} is invalid: {iv.reason}")
            return
        d.complete += 1
        if d.complete == MINUTES and d.status == DAY_ACCUMULATING:
            self._finalize_day(d, now_ms)

    def _finalize_day(self, d: _Day, now_ms: int):
        if d.units <= 0 or d.trades <= 0:
            self._unavailable(d, now_ms, "zero_volume", "the complete session recorded no accepted volume")
            return
        if len(d.prices) < 2:
            self._unavailable(d, now_ms, "zero_price_range", "the complete session traded at one price")
            return
        pairs = [[px, format_units(u)] for px, u in d.prices.items()]
        size = sum(pair_bytes(px, sz) for px, sz in pairs)
        if size > CONTRACT["max_pair_bytes"]:
            self._unavailable(d, now_ms, "profile_bytes_over_limit",
                              f"the price pairs serialize to {size} bytes, over the {CONTRACT['max_pair_bytes']}-byte cap")
            return
        import functools
        pairs.sort(key=functools.cmp_to_key(lambda a, b: compare_decimal(a[0], b[0])))
        art = {
            "session_start_ms": d.start, "session_end_ms": d.start + SESSION_MS, "trades": d.trades,
            "volume": format_units(d.units), "trades_hash": f"{d.hash:064x}", "first_event_ms": d.first,
            "last_event_ms": d.last, "complete_minutes": d.complete, "available_ms": now_ms, "prices": pairs,
        }
        art["sha256"] = artifact_sha256(self.coin, art)
        d.artifact = art
        d.status = DAY_FINAL
        d.prices = None
        self.stats["finalized"] += 1

    def invalidate_minute(self, start_ms: int, at_ms: int, code: str):
        d = self.days.get(session_start(start_ms))
        if d is None:
            return
        if d.status == DAY_ACCUMULATING:
            self._unavailable(d, at_ms, code, f"minute {start_ms} was revoked at {at_ms}: {code}")
        elif d.status == DAY_FINAL and d.invalid_ms == 0:
            d.invalid_ms = at_ms
            d.invalid_code = code
            d.invalid_why = _bounded(f"minute {start_ms} was revoked at {at_ms} after the session became final: {code}")
            self.stats["invalidated"] += 1

    def observe_new_trade(self, start_ms: int, recv_ms: int):
        d = self.days.get(session_start(start_ms))
        if d is None:
            return
        idx = (start_ms - d.start) // CONTRACT["interval_ms"]
        if idx < 0 or idx >= MINUTES or not d.seen[idx]:
            return
        self.invalidate_minute(start_ms, recv_ms, "late_event")

    def trim(self, now_ms: int):
        floor = self._floor(now_ms)
        for start in [k for k in self.days if k < floor]:
            self.history[start] = self.days.pop(start)

    def all_days(self) -> Dict[int, _Day]:
        out = dict(self.history)
        out.update(self.days)
        return out


def _session_record(d: _Day) -> dict:
    if d.status == DAY_FINAL:
        return {"session_start_ms": d.start, "status": "final", "reason_code": d.invalid_code,
                "reason": d.invalid_why, "unavailable_ms": None, "invalid_ms": d.invalid_ms or None,
                "complete_minutes": d.complete, "artifact": d.artifact}
    if d.status == DAY_UNAVAILABLE:
        return {"session_start_ms": d.start, "status": "unavailable", "reason_code": d.code, "reason": d.detail,
                "unavailable_ms": d.unavailable_ms, "invalid_ms": None, "complete_minutes": d.complete, "artifact": None}
    return {"session_start_ms": d.start, "status": "open", "reason_code": "session_not_final",
            "reason": f"{d.complete} of {MINUTES} minutes complete when the recording ended", "unavailable_ms": None,
            "invalid_ms": None, "complete_minutes": d.complete, "artifact": None}


def sessions_sha256(sessions: list) -> str:
    return sha256_bytes(json.dumps(sessions, sort_keys=True, separators=(",", ":")).encode("utf-8"))


def load_profile_recording(run_dir: str) -> dict:
    profiles: Dict[str, ProfileReplay] = {}

    def attach(flows):
        for coin, flow in flows.items():
            flow.profile = ProfileReplay(coin)
            profiles[coin] = flow.profile

    loaded = load_trade_recording(run_dir, attach=attach)
    report = loaded["report"]
    series = {}
    for coin, prof in profiles.items():
        days = prof.all_days()
        sessions = [_session_record(days[k]) for k in sorted(days)]
        series[coin] = {
            "series_schema": SERIES_SCHEMA, "kind": KIND, "coin": coin, **{k: CONTRACT[k] for k in PAYLOAD_KEYS},
            "sessions": sessions, "sessions_sha256": sessions_sha256(sessions),
            "complete_sessions": sum(1 for s in sessions if s["status"] == "final"),
            "economic_evidence": report["economic_evidence"], "refusals": list(report["refusals"]),
            "stats": dict(prof.stats),
            "provenance": {"run_id": report["run_id"], "recorder_version": report["recorder_version"],
                           "segments": [{"file": s["file"], "sha256": s["sha256"]} for s in report["segments"]]},
        }
    return {"series": series, "report": report, "trade_series": loaded["series"]}


def write_profile_series(series: dict, path: str) -> str:
    text = json.dumps(series, sort_keys=True, separators=(",", ":")).encode("utf-8")
    os.makedirs(os.path.dirname(os.path.abspath(path)), exist_ok=True)
    buf = io.BytesIO()
    with gzip.GzipFile(filename="", mode="wb", fileobj=buf, mtime=0) as gz:
        gz.write(text)
    with open(path, "wb") as fh:
        fh.write(buf.getvalue())
    return sha256_bytes(buf.getvalue())


def read_profile_series(path: str) -> dict:
    with gzip.open(path, "rb") as fh:
        series = json.loads(fh.read().decode("utf-8"))
    if not isinstance(series, dict) or series.get("series_schema") != SERIES_SCHEMA:
        raise TradeRecordingError(f"{path}: series_schema must be {SERIES_SCHEMA!r}")
    if series.get("kind") != KIND:
        raise TradeRecordingError(f"{path}: kind must be {KIND!r}")
    for key in PAYLOAD_KEYS:
        if key == "source":
            if series.get(key) not in SOURCES:
                raise TradeRecordingError(f"{path}: source {series.get(key)!r} is not one of {SOURCES}")
            continue
        if series.get(key) != CONTRACT[key]:
            raise TradeRecordingError(f"{path}: {key} {series.get(key)!r} is not the frozen contract value {CONTRACT[key]!r}")
    if series.get("sessions_sha256") != sessions_sha256(series.get("sessions") or []):
        raise TradeRecordingError(f"{path}: sessions_sha256 does not match its sessions")
    for sess in series["sessions"]:
        art = sess.get("artifact")
        if art is not None and artifact_sha256(series["coin"], art) != art.get("sha256"):
            raise TradeRecordingError(f"{path}: session {sess.get('session_start_ms')} artifact hash does not match its content")
    return series


def _base_payload(series: dict, bar_interval_ms: int) -> dict:
    return {"kind": KIND, "coin": series["coin"], **{k: series[k] for k in PAYLOAD_KEYS},
            "window_ms": WINDOW_MS, "bar_interval_ms": int(bar_interval_ms), "snapshot_id": "", "available": True}


def _payload_session(rec: dict, cutoff_ms: Optional[int]) -> dict:
    start = rec["session_start_ms"]
    if rec["status"] == "final":
        art = rec["artifact"]
        if cutoff_ms is not None and art["available_ms"] > cutoff_ms:
            return {"session_start_ms": start, "status": STATUS_PENDING, "reason_code": "session_not_final",
                    "reason": f"session {start} is not final by {cutoff_ms}", "invalid_ms": None, "artifact": None}
        invalid = rec["invalid_ms"]
        if invalid is not None and (cutoff_ms is None or invalid <= cutoff_ms):
            return {"session_start_ms": start, "status": STATUS_INVALIDATED, "reason_code": rec["reason_code"],
                    "reason": rec["reason"], "invalid_ms": invalid, "artifact": art}
        return {"session_start_ms": start, "status": STATUS_AVAILABLE, "reason_code": "", "reason": "",
                "invalid_ms": None, "artifact": art}
    if rec["status"] == "unavailable" and (cutoff_ms is None or rec["unavailable_ms"] <= cutoff_ms):
        return {"session_start_ms": start, "status": STATUS_UNAVAILABLE, "reason_code": rec["reason_code"],
                "reason": rec["reason"], "invalid_ms": None, "artifact": None}
    return {"session_start_ms": start, "status": STATUS_PENDING, "reason_code": "session_not_final",
            "reason": f"session {start} is not final by {cutoff_ms}", "invalid_ms": None, "artifact": None}


def payload_from_profile_series(series: dict, bar_interval_ms: int, cutoff_ms: Optional[int] = None) -> dict:
    out = _base_payload(series, bar_interval_ms)
    if not series.get("economic_evidence"):
        return {**out, "available": False, "sessions": [],
                "reason": "the trade recording is incomplete or damaged: " + "; ".join(series.get("refusals") or [])}
    by_start = {s["session_start_ms"]: s for s in series["sessions"]}
    if cutoff_ms is None:
        out["sessions"] = [_payload_session(by_start[k], None) for k in sorted(by_start)]
        return out
    sessions = []
    for start in sealed_sessions(int(cutoff_ms)):
        rec = by_start.get(start)
        if rec is None:
            sessions.append({"session_start_ms": start, "status": STATUS_PENDING, "reason_code": "session_not_final",
                             "reason": f"session {start} has no finalized minute in this recording", "invalid_ms": None,
                             "artifact": None})
        else:
            sessions.append(_payload_session(rec, int(cutoff_ms)))
    out["cutoff_ms"] = int(cutoff_ms)
    out["sessions"] = sessions
    return out


def coverage_report(series: dict) -> dict:
    sessions = series.get("sessions") or []
    return {
        "coin": series.get("coin"),
        "sessions_seen": len(sessions),
        "complete_sessions": sum(1 for s in sessions if s["status"] == "final"),
        "invalidated_sessions": sum(1 for s in sessions if s["status"] == "final" and s.get("invalid_ms")),
        "unavailable": [{"session_start_ms": s["session_start_ms"], "reason_code": s["reason_code"], "reason": s["reason"],
                         "complete_minutes": s.get("complete_minutes")} for s in sessions if s["status"] != "final"],
        "economic_evidence": bool(series.get("economic_evidence")),
        "sessions_sha256": series.get("sessions_sha256"),
    }


def file_sha256(path: str) -> str:
    with open(path, "rb") as fh:
        return hashlib.sha256(fh.read()).hexdigest()
