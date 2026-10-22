import gzip
import hashlib
import json
import os
import re
from typing import Dict, List, Optional

import lz4_frame
from book_replay import (CONTRACT, HOST, KIND, NAMESPACE, UnreplayableFrame, _go_field, _go_loads, _Obj, _Owner,
                         bounded_reason, parse_book_data)

ARCHIVE_BUCKET = "hyperliquid-archive"
ARCHIVE_SERIES_SCHEMA = "go_trader_l2_book_archive_series/v1"
ARCHIVE_DAY_SCHEMA = "go_trader_l2_book_archive_day/v1"
ARCHIVE_TIME_BASIS = "archiver_receipt_ns_floor_ms/1635-v1"
ARCHIVE_INSTANCE = "hyperliquid-archive"
ARCHIVE_VER_NUM = 1
RESEARCH_NEED = {"window_ms": 60_000, "capacity": CONTRACT["max_observations"], "depth": CONTRACT["max_levels"]}
HOUR_MS = 3_600_000
DAY_MS = 86_400_000
_TIME_RE = re.compile(r"^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.(\d{1,9}))?$")


class ArchiveError(ValueError):
    pass


def book_key(day: str, hour: int, coin: str) -> str:
    return f"market_data/{day}/{hour}/l2Book/{coin}.lz4"


def asset_ctx_key(day: str) -> str:
    return f"asset_ctxs/{day}.csv.lz4"


def parse_archive_time(text) -> Optional[int]:
    import calendar
    if not isinstance(text, str):
        return None
    m = _TIME_RE.match(text)
    if not m:
        return None
    y, mo, d, h, mi, s = (int(m.group(i)) for i in range(1, 7))
    if not (1 <= mo <= 12 and 1 <= d <= 31 and h < 24 and mi < 60 and s < 60):
        return None
    try:
        secs = calendar.timegm((y, mo, d, h, mi, s, 0, 0, 0))
    except (ValueError, OverflowError):
        return None
    frac = (m.group(7) or "").ljust(9, "0")
    return secs * 1000 + int(frac[:3])


def sha256_file(path: str) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as fh:
        for chunk in iter(lambda: fh.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


class ArchiveCoinImporter:
    def __init__(self, coin: str, window_start_ms: int):
        self.coin = coin
        self.owner = _Owner()
        self.owner.connected = True
        self.owner.session = 1
        self.owner.apply_need(coin, RESEARCH_NEED, window_start_ms, ARCHIVE_INSTANCE)
        self.owner.subscription(coin, window_start_ms, "")
        self.last_recv = window_start_ms
        self.counts = {"lines": 0, "accepted": 0, "duplicate": 0, "conflict": 0, "out_of_order": 0, "rejected": 0,
                       "envelope_rejected": 0, "foreign_coin": 0, "outside_hour": 0, "oversized": 0,
                       "missing_hours": 0, "hours": 0}
        self.rejections: Dict[str, int] = {}
        self.missing: List[int] = []

    @property
    def state(self):
        return self.owner.books[self.coin]

    def _timer(self, now_ms: int) -> None:
        st = self.state
        gap = CONTRACT["max_gap_ms"]
        if st.covered and not st.silence_marked and st.last_accept > 0 and now_ms - st.last_accept > gap:
            self.owner.tick(self.coin, st.last_accept + gap + 1)

    def missing_hour(self, hour_start_ms: int) -> None:
        self._timer(hour_start_ms)
        self.counts["missing_hours"] += 1
        self.missing.append(hour_start_ms)
        if self.state.covered:
            self.owner.disconnect(hour_start_ms)
        self.last_recv = max(self.last_recv, hour_start_ms)

    def _reject_global(self, at_ms: int, reason: str) -> None:
        self.counts["envelope_rejected"] += 1
        self.rejections[reason.split(":", 1)[0]] = self.rejections.get(reason.split(":", 1)[0], 0) + 1
        st = self.state
        st.stats["rejected"] += 1
        if st.covered:
            self.owner.add_marker(st, at_ms, "invalid_frame", reason)

    def hour(self, hour_start_ms: int, blob: bytes) -> None:
        self.counts["hours"] += 1
        st = self.state
        if not st.covered:
            self.owner.session += 1
            self.owner.subscription(self.coin, hour_start_ms, "")
        hour_end = hour_start_ms + HOUR_MS
        text = blob.decode("utf-8")
        for line in text.split("\n"):
            if not line:
                continue
            self.counts["lines"] += 1
            try:
                env = _go_loads(line)
            except (ValueError, RecursionError):
                self._reject_global(self.last_recv, "malformed: an archive line is not JSON")
                continue
            try:
                if not isinstance(env, _Obj):
                    raise ValueError("not an object")
                times = [v for k, v in env if k == "time"]
                vers = [v for k, v in env if k == "ver_num"]
                raws = [v for k, v in env if k == "raw"]
                if len(times) != 1 or len(vers) != 1 or len(raws) != 1:
                    raise ValueError("archive line must hold exactly one time, ver_num and raw")
                recv = parse_archive_time(times[0])
                if recv is None:
                    raise ValueError(f"time {times[0]!r} is not a UTC ISO timestamp")
                if getattr(vers[0], "text", None) != str(ARCHIVE_VER_NUM):
                    raise ValueError("ver_num is not 1")
                raw = raws[0]
                if not isinstance(raw, _Obj):
                    raise ValueError("raw is not an object")
                channels = _go_field(raw, "channel")
                if len(channels) != 1 or channels[0] != "l2Book":
                    raise ValueError("raw channel is not l2Book")
                datas = _go_field(raw, "data")
                if len(datas) != 1:
                    raise ValueError("raw holds no single data object")
            except (ValueError, UnreplayableFrame) as exc:
                self._reject_global(self.last_recv, f"malformed: {exc}")
                continue
            self._timer(recv)
            raw_size = len(_compact(raw).encode("utf-8"))
            if raw_size > CONTRACT["max_frame_bytes"]:
                self.counts["oversized"] += 1
                self._reject_global(recv, f"malformed: the frame is {raw_size} bytes, over the {CONTRACT['max_frame_bytes']}-byte bound")
                self.last_recv = max(self.last_recv, recv)
                continue
            if not hour_start_ms <= recv < hour_end:
                self.counts["outside_hour"] += 1
                p = {"coin": self.coin, "event_ms": 0, "bids": None, "asks": None, "identity": "",
                     "err": f"malformed: receipt {recv} lies outside its archive hour {hour_start_ms}"}
            else:
                try:
                    p = parse_book_data(datas[0])
                except UnreplayableFrame as exc:
                    p = {"coin": self.coin, "event_ms": 0, "bids": None, "asks": None, "identity": "",
                         "err": f"malformed: {exc}"}
                if p["coin"] == "":
                    self._reject_global(recv, bounded_reason(p["err"]))
                    self.last_recv = max(self.last_recv, recv)
                    continue
                if p["coin"] != self.coin:
                    self.counts["foreign_coin"] += 1
                    p = {**p, "coin": self.coin, "err": f"malformed: the frame names coin {p['coin']!r} in the {self.coin} file"}
            seq = self.owner.next_seq()
            out = self.owner.ingest(st, p, recv, seq)
            self.counts[out["outcome"]] = self.counts.get(out["outcome"], 0) + 1
            if out["outcome"] == "rejected":
                cat = out["reason"].split(":", 1)[0]
                self.rejections[cat] = self.rejections.get(cat, 0) + 1
            self.last_recv = max(self.last_recv, recv)
        self.owner.generated.clear()

    def finish(self, window_end_ms: int) -> None:
        self._timer(window_end_ms)
        self.owner.generated.clear()

    def take_settled(self, before_ms: int) -> dict:
        hist = self.owner._hist(self.coin)
        obs, keep = [], []
        for o in hist["observations"]:
            (obs if o["evicted_ms"] is not None and o["recv_ms"] < before_ms else keep).append(o)
        hist["observations"] = keep
        marks, mkeep = [], []
        for m in hist["markers"]:
            (marks if m["evicted_ms"] is not None and m["at_ms"] < before_ms else mkeep).append(m)
        hist["markers"] = mkeep
        cov = hist["coverage"]
        cut = 0
        while cut < len(cov) - 1 and cov[cut][0] < before_ms:
            cut += 1
        coverage, hist["coverage"] = cov[:cut], cov[cut:]
        return {"observations": obs, "markers": marks, "coverage": coverage}

    def take_all(self) -> dict:
        hist = self.owner._hist(self.coin)
        out = {"observations": hist["observations"], "markers": hist["markers"], "coverage": hist["coverage"]}
        hist["observations"], hist["markers"] = [], []
        return out

    def header(self) -> dict:
        hist = self.owner._hist(self.coin)
        return {"schema": ARCHIVE_SERIES_SCHEMA, "coin": self.coin, "host": HOST, "namespace": NAMESPACE, "kind": KIND,
                "contract": CONTRACT, "time_basis": ARCHIVE_TIME_BASIS, "needs": hist["needs"],
                "stats": dict(self.state.stats), "counts": dict(self.counts), "rejections": dict(sorted(self.rejections.items())),
                "missing_hours_ms": list(self.missing), "max_accept_gap_ms": hist["max_accept_gap_ms"]}


def _compact(obj) -> str:
    return json.dumps(_plain(obj), separators=(",", ":"), ensure_ascii=False)


def _plain(v):
    if isinstance(v, _Obj):
        return {k: _plain(x) for k, x in v}
    if isinstance(v, list):
        return [_plain(x) for x in v]
    if hasattr(v, "text"):
        return json.loads(v.text)
    return v


def write_day(path: str, coin: str, day: str, part: dict) -> str:
    body = {"schema": ARCHIVE_DAY_SCHEMA, "coin": coin, "day": day, "observations": part["observations"], "markers": part["markers"],
            "coverage": part["coverage"]}
    blob = json.dumps(body, sort_keys=True, separators=(",", ":")).encode("utf-8")
    tmp = path + ".tmp"
    with gzip.GzipFile(tmp, "wb", mtime=0) as fh:
        fh.write(blob)
    os.replace(tmp, path)
    return hashlib.sha256(blob).hexdigest()


def read_day(path: str) -> dict:
    with gzip.open(path, "rb") as fh:
        body = json.loads(fh.read())
    if body.get("schema") != ARCHIVE_DAY_SCHEMA:
        raise ArchiveError(f"{path} is not a {ARCHIVE_DAY_SCHEMA} file")
    return body


def import_window(archive_dir: str, out_dir: str, coins: list, days: list, manifest: dict) -> dict:
    files = {f["key"]: f for f in manifest["files"]}
    os.makedirs(out_dir, exist_ok=True)
    start_ms = _day_ms(days[0])
    end_ms = _day_ms(days[-1]) + DAY_MS
    result = {"coins": {}, "day_files": {}}
    for coin in coins:
        imp = ArchiveCoinImporter(coin, start_ms)
        for day in days:
            for hour in range(24):
                key = book_key(day, hour, coin)
                h0 = _day_ms(day) + hour * HOUR_MS
                ref = files.get(key)
                if ref is None or ref.get("status") == "absent":
                    imp.missing_hour(h0)
                    continue
                path = os.path.join(archive_dir, key)
                if sha256_file(path) != ref["sha256"] or os.path.getsize(path) != ref["size"]:
                    raise ArchiveError(f"{key}: the local file does not match the manifest size and sha256")
                with open(path, "rb") as fh:
                    imp.hour(h0, lz4_frame.decompress(fh.read()))
            settled = imp.take_settled(_day_ms(day) + DAY_MS)
            name = f"{coin}_{day}.json.gz"
            result["day_files"][name] = write_day(os.path.join(out_dir, name), coin, day, settled)
        imp.finish(end_ms)
        tail = imp.take_all()
        name = f"{coin}_tail.json.gz"
        result["day_files"][name] = write_day(os.path.join(out_dir, name), coin, "tail", tail)
        header = imp.header()
        with open(os.path.join(out_dir, f"{coin}_header.json"), "w") as fh:
            json.dump(header, fh, sort_keys=True, indent=1)
        result["coins"][coin] = {k: header[k] for k in ("stats", "counts", "rejections", "missing_hours_ms", "max_accept_gap_ms")}
    return result


def _day_ms(day: str) -> int:
    import calendar
    return calendar.timegm((int(day[:4]), int(day[4:6]), int(day[6:8]), 0, 0, 0, 0, 0, 0)) * 1000
