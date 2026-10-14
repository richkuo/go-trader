import datetime as dt
import gzip
import hashlib
import html
import io
import json
import os
import re
import sys
import time
import urllib.request
from zoneinfo import ZoneInfo

HERE = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.abspath(os.path.join(HERE, "..", "..", ".."))
sys.path.insert(0, os.path.join(REPO, "shared_tools"))

import macro_event_inputs as mi

SOURCES_DIR = os.path.join(HERE, "data", "sources")
SOURCES_INDEX = os.path.join(SOURCES_DIR, "sources.json")
CALENDAR_DIR = os.path.join(HERE, "data", "calendar")
OFFICIAL_PATH = os.path.join(CALENDAR_DIR, "fomc_calendar.json")
CONTROL_PATH = os.path.join(CALENDAR_DIR, "fomc_matched_control.json")
IMPORT_REPORT = os.path.join(CALENDAR_DIR, "import_report.json")
ET = ZoneInfo("America/New_York")
PUBLISHER = "Board of Governors of the Federal Reserve System"
STATEMENT_LOCAL_TIME = "14:00:00"
FED = "https://www.federalreserve.gov"

SOURCES = {
    "fed_press_release_index": {"url": FED + "/json/ne-press.json", "ext": "json",
                                "note": "Official press release index; each entry carries its release date and time "
                                        "(Eastern Time) and the release page path."},
    "fed_tentative_schedule_2023": {"url": FED + "/newsevents/pressreleases/monetary20220624a.htm", "ext": "htm",
                                    "page": "/newsevents/pressreleases/monetary20220624a.htm",
                                    "note": "Tentative 2023 meeting schedule. It lists meeting dates only; it does not "
                                            "state the statement release time."},
    "fed_tentative_schedule_2024": {"url": FED + "/newsevents/pressreleases/monetary20230623a.htm", "ext": "htm",
                                    "page": "/newsevents/pressreleases/monetary20230623a.htm",
                                    "note": "Tentative 2024 meeting schedule. It lists meeting dates only; it does not "
                                            "state the statement release time."},
    "fed_tentative_schedule_2025_2026": {"url": FED + "/newsevents/pressreleases/monetary20240809a.htm", "ext": "htm",
                                         "page": "/newsevents/pressreleases/monetary20240809a.htm",
                                         "note": "Tentative 2025 and 2026 meeting schedule. It states that the Committee "
                                                 "releases its policy statement at 2 p.m. Eastern Time on the second "
                                                 "day of each regularly scheduled meeting."},
    "fed_fomc_calendar_page": {"url": FED + "/monetarypolicy/fomccalendars.htm", "ext": "htm",
                               "note": "Current FOMC meeting calendar page, retrieved for the study. A current "
                                       "download establishes no historical availability; it is used only to check "
                                       "that no listed meeting moved after the tentative schedule."},
}
MONTHS = {m: i for i, m in enumerate(
    ["January", "February", "March", "April", "May", "June", "July", "August", "September", "October",
     "November", "December"], start=1)}
MONTH_ABBR = {k[:3]: v for k, v in MONTHS.items()}


def _sha(blob: bytes) -> str:
    return hashlib.sha256(blob).hexdigest()


def _text(blob: bytes) -> str:
    raw = blob.decode("utf-8", errors="replace")
    raw = re.sub(r"<script.*?</script>", " ", raw, flags=re.S)
    return re.sub(r"\s+", " ", html.unescape(re.sub(r"<[^>]+>", " ", raw)))


def acquire() -> dict:
    os.makedirs(SOURCES_DIR, exist_ok=True)
    index = {}
    for sid, spec in sorted(SOURCES.items()):
        req = urllib.request.Request(spec["url"], headers={"User-Agent": "go-trader-research/1654"})
        last = None
        for attempt in range(4):
            try:
                with urllib.request.urlopen(req, timeout=60) as resp:
                    blob = resp.read()
                break
            except Exception as exc:
                last = exc
                time.sleep(3 * (attempt + 1))
        else:
            raise SystemExit(f"{sid}: {spec['url']} could not be fetched: {last}")
        retrieved_ms = int(time.time() * 1000)
        name = f"{sid}.{spec['ext']}.gz"
        buf = io.BytesIO()
        with gzip.GzipFile(filename="", mode="wb", fileobj=buf, mtime=0) as gz:
            gz.write(blob)
        with open(os.path.join(SOURCES_DIR, name), "wb") as fh:
            fh.write(buf.getvalue())
        index[sid] = {"url": spec["url"], "file": name, "retrieved_ms": retrieved_ms, "sha256": _sha(blob),
                      "bytes": len(blob)}
    with open(SOURCES_INDEX, "w") as fh:
        json.dump(index, fh, indent=1, sort_keys=True)
        fh.write("\n")
    return index


def load_sources() -> dict:
    with open(SOURCES_INDEX) as fh:
        index = json.load(fh)
    out = {}
    for sid, meta in index.items():
        with gzip.open(os.path.join(SOURCES_DIR, meta["file"]), "rb") as fh:
            blob = fh.read()
        if _sha(blob) != meta["sha256"]:
            raise SystemExit(f"{sid}: frozen source bytes do not match sources.json")
        out[sid] = (meta, blob)
    return out


def release_time_ms(index_blob: bytes, page_blob: bytes, page: str) -> int:
    entries = json.loads(index_blob.decode("utf-8-sig"))
    hits = [e for e in entries if e.get("l") == page]
    if len(hits) != 1:
        raise SystemExit(f"{page}: the press release index lists it {len(hits)} times")
    stamp = dt.datetime.strptime(hits[0]["d"], "%m/%d/%Y %I:%M:%S %p").replace(tzinfo=ET)
    m = re.search(r"For release at (\d{1,2}):(\d{2}) ([ap])\.m\. E[DS]T", _text(page_blob))
    if not m:
        raise SystemExit(f"{page}: the page carries no 'For release at' time")
    hour = int(m.group(1)) % 12 + (12 if m.group(3) == "p" else 0)
    if (hour, int(m.group(2))) != (stamp.hour, stamp.minute):
        raise SystemExit(f"{page}: the page release time disagrees with the index entry {hits[0]['d']}")
    return int(stamp.timestamp() * 1000)


def tentative_statement_days(page_blob: bytes, schedule_years: tuple) -> list:
    text = _text(page_blob)
    start = text.find("announced its tentative meeting schedule")
    end = text.find("For media inquiries", start)
    body = text[start:end]
    days = []
    for a in re.finditer(r"Tuesday, (\w+) (\d+), and Wednesday, (\w+) (\d+)(?:, (\d{4}))?", body):
        days.append((a.group(3), int(a.group(4)), a.group(5)))
    for a in re.finditer(r"(\w+) (\d+)-(?:(\w+) )?(\d+)(?:, (\d{4}))? \(\w+-\w+\)", body):
        days.append((a.group(3) or a.group(1), int(a.group(4)), a.group(5)))
    out = []
    year_idx = 0
    prev_month = 0
    for month_name, day, year in days:
        month = MONTHS[month_name]
        if year:
            y = int(year)
        else:
            if prev_month and month < prev_month:
                year_idx += 1
            y = schedule_years[min(year_idx, len(schedule_years) - 1)]
        prev_month = month
        out.append(dt.date(y, month, day))
    if not out:
        raise SystemExit("no meeting dates were parsed from a tentative schedule release")
    return out


def calendar_page_statement_days(page_blob: bytes) -> dict:
    raw = page_blob.decode("utf-8", errors="replace")
    out = {}
    for ym in re.finditer(r"(\d{4}) FOMC Meetings", raw):
        year = int(ym.group(1))
        nxt = re.search(r"\d{4} FOMC Meetings", raw[ym.end():])
        section = raw[ym.end(): ym.end() + (nxt.start() if nxt else len(raw))]
        months = re.findall(r'fomc-meeting__month[^"]*"><strong>([^<]+)</strong>', section)
        dates = re.findall(r'fomc-meeting__date[^"]*">([^<]+)</div>', section)
        if len(months) != len(dates):
            raise SystemExit(f"{year}: calendar page month/date blocks do not pair")
        days = []
        for mtext, dtext in zip(months, dates):
            mtext = mtext.strip()
            dclean = re.sub(r"[^0-9\-]", "", dtext.split("(")[0])
            last_month = mtext.split("/")[-1].strip()
            month = MONTHS.get(last_month) or MONTH_ABBR.get(last_month[:3])
            if month is None or not dclean:
                raise SystemExit(f"{year}: calendar page entry {mtext!r} {dtext!r} is not a meeting date")
            last_day = int(dclean.split("-")[-1])
            notation = "unscheduled" if "unscheduled" in dtext.lower() or "notation" in dtext.lower() else ""
            days.append((dt.date(year, month, last_day), notation, f"{mtext} {dtext.strip()}"))
        out[year] = days
    return out


def statement_record(day: dt.date, known_ms, evidence: str, source_id: str, event_type=mi.EVENT_TYPE_FOMC,
                     event_id=None) -> dict:
    return {"event_id": event_id or f"{mi.EVENT_TYPE_FOMC}/{day.isoformat()}", "event_type": event_type,
            "revision": 1, "status": mi.STATUS_SCHEDULED,
            "scheduled_local": f"{day.isoformat()}T{STATEMENT_LOCAL_TIME}", "source_timezone": "America/New_York",
            "known_ms": known_ms, "publication_evidence": evidence, "source_id": source_id}


def build_artifacts() -> dict:
    sources = load_sources()
    index_blob = sources["fed_press_release_index"][1]
    rows = []
    plan = (("fed_tentative_schedule_2023", (2023, 2024)), ("fed_tentative_schedule_2024", (2024, 2025)),
            ("fed_tentative_schedule_2025_2026", (2025, 2026, 2027)))
    published = {}
    tentative = {}
    for sid, years in plan:
        meta, blob = sources[sid]
        published[sid] = release_time_ms(index_blob, blob, SOURCES[sid]["page"])
        days = tentative_statement_days(blob, years)
        tentative[sid] = days
        states_time = "policy statement at 2 p.m. Eastern Time" in _text(blob)
        for day in days:
            if states_time:
                evidence = (f"{sid}: the official tentative schedule, released "
                            f"{dt.datetime.fromtimestamp(published[sid] / 1000, ET):%Y-%m-%d %H:%M} ET, lists this "
                            "meeting and states the 2 p.m. ET statement time")
                rows.append(statement_record(day, published[sid], evidence, sid))
            else:
                evidence = (f"{sid}: the official tentative schedule lists this meeting date but does not state the "
                            "statement release time, so no advance publication of the full schedule is evidenced")
                rows.append(statement_record(day, None, evidence, sid))
    page = calendar_page_statement_days(sources["fed_fomc_calendar_page"][1])
    listed = {d for year_days in page.values() for d, note, _ in year_days if not note}
    unscheduled = sorted(d.isoformat() for year_days in page.values() for d, note, _ in year_days if note)
    expected = {d for days in tentative.values() for d in days if d.year <= 2026}
    first_page_year = min(d.year for d in expected)
    moved = sorted(d.isoformat() for d in expected if d.year >= first_page_year and d not in listed)
    extra = sorted(d.isoformat() for d in listed if first_page_year <= d.year <= 2026 and d not in expected)
    if moved or extra:
        raise SystemExit(f"the current calendar page disagrees with the tentative schedules: moved {moved}, "
                         f"extra {extra}; record revisions by hand before importing")
    source_entries = []
    for sid, (meta, _) in sorted(sources.items()):
        source_entries.append({"source_id": sid, "publisher": PUBLISHER, "url": meta["url"],
                               "retrieved_ms": meta["retrieved_ms"], "sha256": meta["sha256"],
                               "published_ms": published.get(sid), "note": SOURCES[sid]["note"]})
    start_ms = int(dt.datetime(2025, 1, 1, tzinfo=dt.timezone.utc).timestamp() * 1000)
    end_ms = int(dt.datetime(2027, 1, 1, tzinfo=dt.timezone.utc).timestamp() * 1000)
    coverage = [{"event_type": mi.EVENT_TYPE_FOMC, "start_ms": start_ms, "end_ms": end_ms}]
    official, official_stats = mi.import_calendar(rows, source_entries, coverage, "official", [mi.EVENT_TYPE_FOMC])
    official_sha = mi.write_artifact(OFFICIAL_PATH, official)

    eligible = [r for r in official["records"] if r["known_ms"] is not None]
    release_days = sorted({dt.date.fromisoformat(r["scheduled_local"][:10]) for r in official["records"]})
    controls = []
    skipped = []
    for rec in eligible:
        day = dt.date.fromisoformat(rec["scheduled_local"][:10])
        for offset in (-7, 7):
            cday = day + dt.timedelta(days=offset)
            if any(abs((cday - d).days) <= 3 for d in release_days):
                skipped.append(cday.isoformat())
                continue
            controls.append(statement_record(
                cday, rec["known_ms"],
                f"research control matched to {rec['event_id']}: same weekday and 2 p.m. ET local time, "
                f"{offset:+d} days; not an official release",
                "study_matched_control", event_type=mi.EVENT_TYPE_CONTROL,
                event_id=f"{mi.EVENT_TYPE_CONTROL}/{cday.isoformat()}"))
    control_source = [{"source_id": "study_matched_control", "publisher": "go-trader research study 1654",
                       "url": "backtest/candidates/macro_event_breakout_1654/calendar_sources.py",
                       "retrieved_ms": max(m["retrieved_ms"] for m, _ in sources.values()),
                       "sha256": official_sha, "published_ms": None,
                       "note": "Matched non-event controls derived from the official calendar artifact with this sha256; "
                               "each control inherits its source event's publication time."}]
    control_cov = [{"event_type": mi.EVENT_TYPE_CONTROL, "start_ms": start_ms, "end_ms": end_ms}]
    control, control_stats = mi.import_calendar(controls, control_source, control_cov, "research_control",
                                                [mi.EVENT_TYPE_CONTROL])
    control_sha = mi.write_artifact(CONTROL_PATH, control)
    report = {
        "official": {"path": os.path.relpath(OFFICIAL_PATH, REPO), "sha256": official_sha, "stats": official_stats,
                     "records": len(official["records"]), "eligible_records": len(eligible),
                     "unknown_publication_records": len(official["records"]) - len(eligible)},
        "control": {"path": os.path.relpath(CONTROL_PATH, REPO), "sha256": control_sha, "stats": control_stats,
                    "records": len(control["records"]), "skipped_near_official": skipped},
        "calendar_page_check": {"years_checked": [first_page_year, 2026], "years_on_page": sorted(page), "listed_statements": len(listed),
                                "unscheduled_entries": unscheduled, "moved": moved, "extra": extra},
        "published_ms": published,
    }
    with open(IMPORT_REPORT, "w") as fh:
        json.dump(report, fh, indent=1, sort_keys=True)
        fh.write("\n")
    return report


if __name__ == "__main__":
    cmd = sys.argv[1] if len(sys.argv) > 1 else ""
    if cmd == "acquire":
        print(json.dumps(acquire(), indent=1, sort_keys=True))
    elif cmd == "import":
        print(json.dumps(build_artifacts(), indent=1, sort_keys=True))
    else:
        raise SystemExit("usage: calendar_sources.py acquire|import")
