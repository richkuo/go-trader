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
_DAY_RE = re.compile(r"^(?P<coin>.+)_(?P<day>[0-9]{8}|tail)\.json\.gz$")


def prepare_archive(archive_dir: str, out_dir: str, coins: list, days: list, manifest: dict) -> dict:
    result = ba.import_window(archive_dir, out_dir, coins, days, manifest)
    report = {"schema": REPORT_SCHEMA, "coins": sorted(coins), "days": list(days), "day_files": result["day_files"]}
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


def load_archive_series(import_dir: str, coin: str) -> dict:
    with open(os.path.join(import_dir, REPORT_NAME)) as fh:
        report = json.load(fh)
    if report.get("schema") != REPORT_SCHEMA or coin not in report.get("coins", []):
        raise ArchiveError(f"{import_dir} holds no verified {coin} import")
    with open(os.path.join(import_dir, f"{coin}_header.json")) as fh:
        header = json.load(fh)
    if (header.get("schema") != ba.ARCHIVE_SERIES_SCHEMA or header.get("coin") != coin or header.get("contract") != CONTRACT
            or header.get("time_basis") != ba.ARCHIVE_TIME_BASIS):
        raise ArchiveError(f"{coin} archive header is not a {ba.ARCHIVE_SERIES_SCHEMA} series under the frozen contract")
    names = []
    for name, digest in sorted(report["day_files"].items()):
        m = _DAY_RE.match(name)
        if m and m.group("coin") == coin:
            names.append((m.group("day") == "tail", m.group("day"), name, digest))
    names.sort()
    if not names or not names[-1][0]:
        raise ArchiveError(f"{coin} archive import has no tail file")
    series = {"coin": coin, "needs": header["needs"], "observations": [], "markers": [], "coverage": []}
    for _, _, name, digest in names:
        day = _read_verified_day(os.path.join(import_dir, name), digest)
        if day.get("coin") != coin:
            raise ArchiveError(f"{name} names coin {day.get('coin')!r}")
        series["observations"].extend(day["observations"])
        series["markers"].extend(day["markers"])
        series["coverage"].extend(day["coverage"])
    return series


def archive_payload(import_dir: str, coin: str, cutoff_ms: int, as_of_ms: int, need: dict) -> dict:
    series = load_archive_series(import_dir, coin)
    out = payload_from_series(series, cutoff_ms, need, as_of_ms, "")
    out["time_semantics"] = ba.ARCHIVE_TIME_BASIS
    return out
