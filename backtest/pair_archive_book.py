import gzip
import hashlib
import json
import os
import re

import book_archive as ba
from archive_cache import ArchiveError
from book_replay import CONTRACT, payload_from_series

REPORT_NAME = "import_report.json"
REPORT_SCHEMA = "go_trader_pair_archive_import/v1"
BOUND_KEYS = ("coverage_first_ms", "evicted_last_ms", "open_rows", "row_first_ms", "row_last_ms", "rows")
_DAY_RE = re.compile(r"^(?P<coin>.+)_(?P<day>[0-9]{8}|tail)\.json\.gz$")


def day_bounds(body: dict) -> dict:
    rows = [(o["recv_ms"], o["evicted_ms"]) for o in body["observations"]] + [(m["at_ms"], m["evicted_ms"]) for m in body["markers"]]
    closed = [e for _, e in rows if e is not None]
    return {"rows": len(rows), "row_first_ms": min((a for a, _ in rows), default=None),
            "row_last_ms": max((a for a, _ in rows), default=None), "open_rows": len(rows) - len(closed),
            "evicted_last_ms": max(closed, default=None), "coverage_first_ms": min((c[0] for c in body["coverage"]), default=None)}


def prepare_archive(archive_dir: str, out_dir: str, coins: list, days: list, manifest: dict) -> dict:
    result = ba.import_window(archive_dir, out_dir, coins, days, manifest)
    bounds = {name: day_bounds(_read_verified_day(os.path.join(out_dir, name), digest)) for name, digest in sorted(result["day_files"].items())}
    report = {"schema": REPORT_SCHEMA, "coins": sorted(coins), "days": list(days), "day_files": result["day_files"], "day_bounds": bounds}
    tmp = os.path.join(out_dir, REPORT_NAME + ".tmp")
    with open(tmp, "w") as fh:
        json.dump(report, fh, sort_keys=True, indent=1)
        fh.write("\n")
    os.replace(tmp, os.path.join(out_dir, REPORT_NAME))
    return report


def _read_verified_day(path: str, want: str) -> dict:
    with gzip.open(path, "rb") as fh:
        blob = fh.read()
    if hashlib.sha256(blob).hexdigest() != want:
        raise ArchiveError(f"{path} does not match its import report digest")
    body = json.loads(blob)
    if body.get("schema") != ba.ARCHIVE_DAY_SCHEMA:
        raise ArchiveError(f"{path} is not a {ba.ARCHIVE_DAY_SCHEMA} file")
    return body


def _is_ms(v, nullable: bool) -> bool:
    return (v is None and nullable) or (type(v) is int and v >= 0)


def _checked_bounds(name: str, b) -> dict:
    if not isinstance(b, dict) or tuple(sorted(b)) != BOUND_KEYS:
        raise ArchiveError(f"{name} has no well-formed bounds in the import report")
    if not (_is_ms(b["rows"], False) and _is_ms(b["open_rows"], False) and b["open_rows"] <= b["rows"]
            and all(_is_ms(b[k], True) for k in ("coverage_first_ms", "evicted_last_ms", "row_first_ms", "row_last_ms"))
            and (b["row_first_ms"] is None) == (b["row_last_ms"] is None) == (b["rows"] == 0)
            and (b["evicted_last_ms"] is None) == (b["open_rows"] == b["rows"])
            and (b["rows"] == 0 or b["row_first_ms"] <= b["row_last_ms"])):
        raise ArchiveError(f"{name} has inconsistent bounds in the import report")
    return b


def _verified_header(import_dir: str, coin: str) -> tuple:
    with open(os.path.join(import_dir, REPORT_NAME)) as fh:
        report = json.load(fh)
    if report.get("schema") != REPORT_SCHEMA or coin not in report.get("coins", []):
        raise ArchiveError(f"{import_dir} holds no verified {coin} import")
    with open(os.path.join(import_dir, f"{coin}_header.json")) as fh:
        header = json.load(fh)
    if (header.get("schema") != ba.ARCHIVE_SERIES_SCHEMA or header.get("coin") != coin or header.get("contract") != CONTRACT
            or header.get("time_basis") != ba.ARCHIVE_TIME_BASIS):
        raise ArchiveError(f"{coin} archive header is not a {ba.ARCHIVE_SERIES_SCHEMA} series under the frozen contract")
    bounds = report.get("day_bounds")
    if not isinstance(bounds, dict) or not isinstance(report.get("day_files"), dict) or set(bounds) != set(report["day_files"]):
        raise ArchiveError(f"{import_dir} import report has no bounds for exactly its day files")
    names = []
    for name, digest in sorted(report["day_files"].items()):
        m = _DAY_RE.match(name)
        if m and m.group("coin") == coin:
            names.append((m.group("day") == "tail", m.group("day"), name, digest, _checked_bounds(name, bounds.get(name))))
    names.sort()
    if not names or not names[-1][0]:
        raise ArchiveError(f"{coin} archive import has no tail file")
    return header, names


def _load(import_dir: str, coin: str, header: dict, files: list) -> dict:
    series = {"coin": coin, "needs": header["needs"], "observations": [], "markers": [], "coverage": []}
    for _, _, name, digest, bounds in files:
        day = _read_verified_day(os.path.join(import_dir, name), digest)
        if day.get("coin") != coin:
            raise ArchiveError(f"{name} names coin {day.get('coin')!r}")
        if day_bounds(day) != bounds:
            raise ArchiveError(f"{name} does not match its import report bounds")
        series["observations"].extend(day["observations"])
        series["markers"].extend(day["markers"])
        series["coverage"].extend(day["coverage"])
    return series


def load_archive_series(import_dir: str, coin: str) -> dict:
    header, names = _verified_header(import_dir, coin)
    return _load(import_dir, coin, header, names)


def _may_hold_present_row(b: dict, start_ms: int, cutoff_ms: int, as_of_ms: int) -> bool:
    if b["rows"] == 0 or b["row_first_ms"] >= as_of_ms or b["row_first_ms"] > cutoff_ms or b["row_last_ms"] < start_ms:
        return False
    return b["open_rows"] > 0 or b["evicted_last_ms"] >= as_of_ms


def archive_files_for_read(names: list, cutoff_ms: int, as_of_ms: int, window_ms: int) -> list:
    start = cutoff_ms - window_ms
    coverage_last = None
    for i, entry in enumerate(names):
        first = entry[4]["coverage_first_ms"]
        if first is not None and first < as_of_ms:
            coverage_last = i
    return [entry for i, entry in enumerate(names)
            if entry[0] or i == coverage_last or _may_hold_present_row(entry[4], start, cutoff_ms, as_of_ms)]


def archive_payload(import_dir: str, coin: str, cutoff_ms: int, as_of_ms: int, need: dict) -> dict:
    header, names = _verified_header(import_dir, coin)
    series = _load(import_dir, coin, header, archive_files_for_read(names, cutoff_ms, as_of_ms, need["window_ms"]))
    out = payload_from_series(series, cutoff_ms, need, as_of_ms, "")
    out["time_semantics"] = ba.ARCHIVE_TIME_BASIS
    return out
