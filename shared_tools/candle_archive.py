import hashlib
import json
import math
import os
import re
import shutil
import struct
import tempfile

FORMAT = "go-trader-candle-archive/v1"
RECORD = struct.Struct("<q5d")
CURRENT = "CURRENT"
GENERATIONS = "generations"
MANIFEST = "manifest.json"
DEFAULT_CHUNK_BARS = 1024
MAX_CHUNK_BARS = 1 << 16
TIMESTAMP_OPEN = "open"
REFUSED_VENUES = ("okx", "topstep", "robinhood")

GRID_SECONDS = {
    "1m": 60, "3m": 180, "5m": 300, "15m": 900, "30m": 1800,
    "1h": 3600, "2h": 7200, "4h": 14400, "6h": 21600, "8h": 28800, "12h": 43200,
    "1d": 86400, "3d": 259200, "1w": 604800,
}
EPOCH_ALIGNED = {"1m", "3m", "5m", "15m", "30m", "1h", "2h", "4h", "6h", "8h", "12h", "1d"}
GENERATION_RE = re.compile(r"^g-([0-9]{1,16})-([0-9a-f]{16})$")
IDENTITY_KEYS = ("venue", "network", "instrument_class", "symbol", "timeframe")


class ArchiveError(ValueError):
    pass


def canonical_json(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=True, allow_nan=False).encode("ascii")


def generation_id(published_at_ms, manifest_bytes):
    return "g-%d-%s" % (published_at_ms, hashlib.sha256(manifest_bytes).hexdigest()[:16])


def _identity_label(series):
    return "|".join(str(series.get(k, "")) for k in IDENTITY_KEYS)


def _check_identity(series):
    label = _identity_label(series)
    for key in IDENTITY_KEYS:
        value = series.get(key)
        if not isinstance(value, str) or not value or value != value.strip() or any(c in value for c in "|\x00\n\r\t"):
            raise ArchiveError(f"series {label}: {key} must be non-empty canonical text")
    for key in ("venue", "network", "instrument_class"):
        if series[key] != series[key].lower():
            raise ArchiveError(f"series {label}: {key} must be lower case")
    if series["venue"] in REFUSED_VENUES:
        raise ArchiveError(f"series {label}: venue {series['venue']} has no verified opening-time range read; format v1 refuses it")
    if series["timeframe"] not in GRID_SECONDS:
        raise ArchiveError(f"series {label}: timeframe {series['timeframe']!r} has no fixed bar grid")
    return label


def valid_bar(o, h, l, c, v):
    for x in (o, h, l, c):
        if not isinstance(x, (int, float)) or isinstance(x, bool) or not math.isfinite(x) or x <= 0:
            return False
    if not isinstance(v, (int, float)) or isinstance(v, bool) or not math.isfinite(v) or v < 0:
        return False
    return h >= max(o, c, l) and l <= min(o, c)


def observed_at_ms(label, source):
    value = (source or {}).get("observed_at_ms")
    if isinstance(value, bool) or not isinstance(value, int) or value <= 0:
        raise ArchiveError(f"series {label}: source.observed_at_ms must be the positive time its rows were read from the venue")
    return value


def finalize_rows(series, rows, published_at_ms, min_close_age_ms, observed_ms):
    label = _check_identity(series)
    if isinstance(observed_ms, bool) or not isinstance(observed_ms, int) or observed_ms <= 0:
        raise ArchiveError(f"series {label}: the rows carry no positive venue read time")
    final_by_ms = min(published_at_ms, observed_ms)
    interval_ms = GRID_SECONDS[series["timeframe"]] * 1000
    aligned = series["timeframe"] in EPOCH_ALIGNED
    out = []
    seen = set()
    for row in rows:
        if not isinstance(row, (list, tuple)) or len(row) < 6:
            raise ArchiveError(f"series {label}: row {row!r} is not [open_ms, o, h, l, c, v]")
        open_ms = row[0]
        if isinstance(open_ms, bool) or not isinstance(open_ms, int) or open_ms < 0 or open_ms % 1000:
            raise ArchiveError(f"series {label}: opening time {open_ms!r} is not a whole-second millisecond integer")
        if aligned and open_ms % interval_ms:
            raise ArchiveError(f"series {label}: opening time {open_ms} is off the {series['timeframe']} grid")
        try:
            o, h, l, c, v = (float(x) for x in row[1:6])
        except (TypeError, ValueError) as exc:
            raise ArchiveError(f"series {label}: bar {open_ms} has a non-numeric value: {exc}") from exc
        if not valid_bar(o, h, l, c, v):
            raise ArchiveError(f"series {label}: bar {open_ms} has invalid OHLC or volume")
        if open_ms in seen:
            raise ArchiveError(f"series {label}: opening time {open_ms} appears twice")
        seen.add(open_ms)
        if open_ms + interval_ms + min_close_age_ms > final_by_ms:
            continue
        out.append((open_ms, o, h, l, c, v))
    out.sort(key=lambda r: r[0])
    for prev, cur in zip(out, out[1:]):
        if (cur[0] - prev[0]) % interval_ms:
            raise ArchiveError(f"series {label}: opening time {cur[0]} is off the {series['timeframe']} grid after {prev[0]}")
    if not out:
        raise ArchiveError(f"series {label}: no finalized bar remains")
    return out


def _segments(bars, interval_ms):
    segs = []
    for open_ms, *_ in bars:
        if segs and open_ms - segs[-1]["last_open_ms"] == interval_ms:
            segs[-1]["last_open_ms"] = open_ms
            segs[-1]["bars"] += 1
        else:
            segs.append({"first_open_ms": open_ms, "last_open_ms": open_ms, "bars": 1})
    return segs


def _fsync_dir(path):
    fd = os.open(path, os.O_RDONLY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def _write_file(path, data):
    with open(path, "wb") as fh:
        fh.write(data)
        fh.flush()
        os.fsync(fh.fileno())


def build_generation(archive_dir, series_list, publisher, published_at_ms, expires_at_ms, min_close_age_ms,
                     chunk_bars=DEFAULT_CHUNK_BARS):
    if not os.path.isabs(archive_dir):
        raise ArchiveError("the archive directory must be an absolute path")
    if expires_at_ms <= published_at_ms:
        raise ArchiveError("expires_at_ms must be after published_at_ms")
    if min_close_age_ms < 0:
        raise ArchiveError("min_close_age_ms must not be negative")
    if not 0 < chunk_bars <= MAX_CHUNK_BARS:
        raise ArchiveError(f"chunk_bars must be in 1..{MAX_CHUNK_BARS}")
    for key in ("id", "tool", "version", "egress_id"):
        if not str(publisher.get(key, "")).strip():
            raise ArchiveError(f"publisher.{key} is empty")
    if not series_list:
        raise ArchiveError("no series to publish")
    labels = set()
    gen_root = os.path.join(archive_dir, GENERATIONS)
    os.makedirs(gen_root, exist_ok=True)
    tmp = tempfile.mkdtemp(prefix=".build-", dir=gen_root)
    try:
        os.mkdir(os.path.join(tmp, "series"))
        manifest_series = []
        for i, item in enumerate(series_list):
            series = {k: item[k] for k in IDENTITY_KEYS}
            label = _check_identity(series)
            if label in labels:
                raise ArchiveError(f"series {label}: the identity appears twice")
            labels.add(label)
            source = item.get("source") or {}
            observed_ms = observed_at_ms(label, source)
            bars = finalize_rows(series, item["rows"], published_at_ms, min_close_age_ms, observed_ms)
            interval_ms = GRID_SECONDS[series["timeframe"]] * 1000
            data = b"".join(RECORD.pack(*b) for b in bars)
            step = chunk_bars * RECORD.size
            chunks = [hashlib.sha256(data[off:off + step]).hexdigest() for off in range(0, len(data), step)]
            name = "series/s%04d.bars" % i
            _write_file(os.path.join(tmp, name), data)
            manifest_series.append({
                **series,
                "interval_ms": interval_ms,
                "timestamp": TIMESTAMP_OPEN,
                "file": name,
                "bars": len(bars),
                "bytes": len(data),
                "sha256": hashlib.sha256(data).hexdigest(),
                "chunk_bars": chunk_bars,
                "chunk_sha256": chunks,
                "segments": _segments(bars, interval_ms),
                "finalized_through_open_ms": bars[-1][0],
                "history_start_known": bool(item.get("history_start_known", False)),
                "source": {
                    "kind": str(source.get("kind") or "unknown"),
                    "detail": str(source.get("detail") or ""),
                    "observed_at_ms": observed_ms,
                },
            })
        _fsync_dir(os.path.join(tmp, "series"))
        manifest = {
            "format": FORMAT,
            "published_at_ms": published_at_ms,
            "expires_at_ms": expires_at_ms,
            "publisher": {k: str(publisher[k]) for k in ("id", "tool", "version", "egress_id")},
            "finalization": {"min_close_age_ms": min_close_age_ms},
            "series": manifest_series,
        }
        raw = canonical_json(manifest)
        _write_file(os.path.join(tmp, MANIFEST), raw)
        _fsync_dir(tmp)
        gen = generation_id(published_at_ms, raw)
        final = os.path.join(gen_root, gen)
        if os.path.exists(final):
            raise ArchiveError(f"generation {gen} already exists")
        os.rename(tmp, final)
        _fsync_dir(gen_root)
    except BaseException:
        shutil.rmtree(tmp, ignore_errors=True)
        raise
    return gen


def set_current(archive_dir, gen):
    if not GENERATION_RE.match(gen):
        raise ArchiveError(f"{gen!r} is not a generation id")
    if not os.path.isdir(os.path.join(archive_dir, GENERATIONS, gen)):
        raise ArchiveError(f"generation {gen} does not exist")
    tmp = os.path.join(archive_dir, CURRENT + ".tmp")
    _write_file(tmp, (gen + "\n").encode("ascii"))
    os.replace(tmp, os.path.join(archive_dir, CURRENT))
    _fsync_dir(archive_dir)


def read_current(archive_dir):
    with open(os.path.join(archive_dir, CURRENT), "rb") as fh:
        text = fh.read(256).decode("ascii").rstrip("\n")
    if not GENERATION_RE.match(text):
        raise ArchiveError(f"{CURRENT} does not name a generation id")
    return text


def prune_generations(archive_dir, keep, protect=()):
    gen_root = os.path.join(archive_dir, GENERATIONS)
    names = []
    for name in os.listdir(gen_root):
        m = GENERATION_RE.match(name)
        if m and os.path.isdir(os.path.join(gen_root, name)) and not os.path.islink(os.path.join(gen_root, name)):
            names.append((int(m.group(1)), name))
    names.sort(reverse=True)
    removed = []
    kept = 0
    for _, name in names:
        if name in protect:
            continue
        if kept < keep:
            kept += 1
            continue
        shutil.rmtree(os.path.join(gen_root, name))
        removed.append(name)
    return removed
