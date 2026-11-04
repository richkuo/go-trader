from datetime import date, timedelta

import exchange_calendars
import pandas as pd

CALENDAR_NAME = "XNYS"
SETTLEMENT_READ_DELAY = timedelta(minutes=60)

_CALENDAR = None


class SessionUnresolved(Exception):

    def __init__(self, reason):
        super().__init__(reason)
        self.reason = reason


def calendar():
    global _CALENDAR
    if _CALENDAR is None:
        _CALENDAR = exchange_calendars.get_calendar(CALENDAR_NAME)
    return _CALENDAR


def _utc_text(ts):
    return ts.tz_convert("UTC").strftime("%Y-%m-%dT%H:%M:%SZ")


def resolve_expiry_session(expiry):
    try:
        day = date.fromisoformat(expiry) if isinstance(expiry, str) else None
    except ValueError:
        day = None
    if day is None:
        raise SessionUnresolved(f"session unresolved: expiry {expiry!r} is not a YYYY-MM-DD date")
    cal = calendar()
    ts = pd.Timestamp(day)
    first, last = cal.first_session, cal.last_session
    if ts < first or ts > last:
        raise SessionUnresolved(
            f"session unresolved: expiry {day.isoformat()} is outside the {CALENDAR_NAME} calendar bounds "
            f"{first.date().isoformat()}..{last.date().isoformat()}")
    mapped = False
    if cal.is_session(ts):
        session = ts
    else:
        session = cal.date_to_session(ts, direction="previous")
        mapped = True
        if session < first or (ts - session) > pd.Timedelta(days=7):
            raise SessionUnresolved(
                f"session unresolved: no {CALENDAR_NAME} session within 7 days before non-session expiry {day.isoformat()}")
    close = cal.session_close(session)
    if close.tzinfo is None:
        close = close.tz_localize("UTC")
    ready = close + pd.Timedelta(SETTLEMENT_READ_DELAY)
    return {
        "session_date": session.date().isoformat(),
        "cutoff_utc": _utc_text(close),
        "settlement_ready_utc": _utc_text(ready),
        "calendar": CALENDAR_NAME,
        "non_session_mapped": mapped,
        "cutoff": close.tz_convert("UTC").to_pydatetime(),
        "ready": ready.tz_convert("UTC").to_pydatetime(),
    }
