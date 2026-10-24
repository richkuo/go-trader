import gzip
import hashlib
import json
import os
import struct
import sys

import pytest

_HERE = os.path.dirname(os.path.abspath(__file__))
_BACKTEST = os.path.abspath(os.path.join(_HERE, ".."))
if _BACKTEST not in sys.path:
    sys.path.insert(0, _BACKTEST)

import book_archive as ba  # noqa: E402
import lz4_frame  # noqa: E402
import pair_archive_book as pab  # noqa: E402
from archive_cache import ArchiveError  # noqa: E402
from book_replay import payload_from_series  # noqa: E402

FIXTURE = os.path.join(_HERE, "testdata", "obi_archive_hour.lz4")
DAYS = [f"202608{d:02d}" for d in range(1, 8)]
NEED = {"depth": 5, "window_ms": 60_000, "capacity": 64}
FIRST_MS, STEP_MS, FRAMES = 1000, 5400, 30


def _stored_lz4(data: bytes) -> bytes:
    desc = bytes([0x60, 0x70])
    out = bytearray(struct.pack("<I", 0x184D2204) + desc + bytes([(lz4_frame.xxh32(desc) >> 8) & 0xFF]))
    for i in range(0, len(data), 1 << 22):
        chunk = data[i:i + (1 << 22)]
        out += struct.pack("<I", len(chunk) | 0x80000000) + chunk
    return bytes(out + struct.pack("<I", 0))


def _iso(ms: int) -> str:
    import datetime
    t = datetime.datetime.fromtimestamp(ms / 1000, tz=datetime.timezone.utc)
    return t.strftime("%Y-%m-%dT%H:%M:%S.") + f"{ms % 1000:03d}000123"


def _shifted_hour(lines: list, shift_ms: int) -> bytes:
    out = []
    for line in lines:
        env = json.loads(line)
        env["time"] = _iso(ba.parse_archive_time(env["time"]) + shift_ms)
        env["raw"]["data"]["time"] += shift_ms
        out.append(json.dumps(env, separators=(",", ":")))
    return ("\n".join(out) + "\n").encode()


def _hour_start(day: str, hour: int) -> int:
    return ba._day_ms(day) + hour * ba.HOUR_MS


def _hour_receipts(day: str, hour: int) -> list:
    base = _hour_start(day, hour) + (ba.HOUR_MS - 2000 - FIRST_MS - STEP_MS * (FRAMES - 1) if hour == 23 else 0)
    return [base + FIRST_MS + STEP_MS * i for i in range(FRAMES)]


@pytest.fixture(scope="module")
def imported(tmp_path_factory):
    root = tmp_path_factory.mktemp("pair_archive")
    raw, out = str(root / "raw"), str(root / "import")
    lines = [ln for ln in lz4_frame.decompress(open(FIXTURE, "rb").read()).decode().split("\n") if ln]
    origin = _hour_start("20260801", 0)
    files = []
    for day in DAYS:
        if day == "20260805":
            continue
        for hour in (0, 23):
            key = ba.book_key(day, hour, "BTC")
            blob = _stored_lz4(_shifted_hour(lines, _hour_receipts(day, hour)[0] - FIRST_MS - origin))
            path = os.path.join(raw, key)
            os.makedirs(os.path.dirname(path), exist_ok=True)
            with open(path, "wb") as fh:
                fh.write(blob)
            files.append({"key": key, "status": "present", "size": len(blob), "sha256": hashlib.sha256(blob).hexdigest()})
    pab.prepare_archive(raw, out, ["BTC"], DAYS, {"files": files})
    return out


def _full_payload(series: dict, cutoff: int) -> bytes:
    out = payload_from_series(series, cutoff, NEED, cutoff, "")
    out["time_semantics"] = ba.ARCHIVE_TIME_BASIS
    return json.dumps(out, sort_keys=True).encode()


def _read(import_dir: str, cutoff: int) -> bytes:
    return json.dumps(pab.archive_payload(import_dir, "BTC", cutoff, cutoff, NEED), sort_keys=True).encode()


def _opened(import_dir: str, cutoff: int) -> list:
    _, names = pab._verified_header(import_dir, "BTC")
    return [e[2] for e in pab.archive_files_for_read(names, cutoff, cutoff, NEED["window_ms"])]


def test_selective_archive_read_equals_full_load_at_every_boundary(imported):
    series = pab.load_archive_series(imported, "BTC")
    cutoffs = set()
    for day in DAYS:
        cutoffs.update({ba._day_ms(day) + k for k in (-1, 0, 1, 30_000, 43_200_000)})
        if day != "20260805":
            for hour in (0, 23):
                for r in _hour_receipts(day, hour)[::7] + _hour_receipts(day, hour)[-1:]:
                    cutoffs.update({r - 1, r, r + 1, r + NEED["window_ms"], r + NEED["window_ms"] + 1})
    cutoffs.update({ba._day_ms("20260808") + k for k in (-1, 0, 3_600_000)})
    for cutoff in sorted(cutoffs):
        assert _read(imported, cutoff) == _full_payload(series, cutoff), cutoff
    mid = ba._day_ms("20260804") + 43_200_000
    assert _opened(imported, mid) == ["BTC_20260804.json.gz", "BTC_tail.json.gz"]
    late = _hour_receipts("20260801", 23)[-1] + 1
    assert _opened(imported, late) == ["BTC_20260802.json.gz", "BTC_tail.json.gz"]
    late_rows = json.loads(_read(imported, late))["observations"]
    assert late_rows and all(o["recv_ms"] < ba._day_ms("20260802") for o in late_rows)
    across = ba._day_ms("20260802") + 30_000
    rows = json.loads(_read(imported, across))["observations"]
    assert min(o["recv_ms"] for o in rows) < ba._day_ms("20260802") <= max(o["recv_ms"] for o in rows)
    assert "BTC_20260802.json.gz" in _opened(imported, across)


def _tamper(path: str) -> None:
    with gzip.open(path, "rb") as fh:
        body = json.loads(fh.read())
    body["observations"] = body["observations"][1:]
    with gzip.GzipFile(path, "wb", mtime=0) as fh:
        fh.write(json.dumps(body, sort_keys=True, separators=(",", ":")).encode())


def test_archive_read_verifies_exactly_the_files_it_opens(imported, tmp_path):
    import shutil
    copy = str(tmp_path / "import")
    shutil.copytree(imported, copy)
    mid = ba._day_ms("20260804") + 43_200_000
    before = _read(copy, mid)
    _tamper(os.path.join(copy, "BTC_20260801.json.gz"))
    assert _read(copy, mid) == before
    with pytest.raises(ArchiveError, match="does not match its import report digest"):
        pab.archive_payload(copy, "BTC", ba._day_ms("20260801") + 30_000, ba._day_ms("20260801") + 30_000, NEED)
    _tamper(os.path.join(copy, "BTC_20260804.json.gz"))
    with pytest.raises(ArchiveError, match="does not match its import report digest"):
        pab.archive_payload(copy, "BTC", mid, mid, NEED)
    shutil.rmtree(copy)
    shutil.copytree(imported, copy)
    report_path = os.path.join(copy, pab.REPORT_NAME)
    report = json.load(open(report_path))
    report["day_bounds"]["BTC_tail.json.gz"]["rows"] += 1
    json.dump(report, open(report_path, "w"))
    with pytest.raises(ArchiveError, match="bounds"):
        pab.archive_payload(copy, "BTC", mid, mid, NEED)
    del report["day_bounds"]["BTC_20260803.json.gz"]
    json.dump(report, open(report_path, "w"))
    with pytest.raises(ArchiveError, match="no bounds for exactly its day files"):
        pab.archive_payload(copy, "BTC", mid, mid, NEED)
