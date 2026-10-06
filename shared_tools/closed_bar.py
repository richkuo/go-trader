from __future__ import annotations

import hashlib
import json
import math
from dataclasses import dataclass
from typing import Callable, Optional


CLOSED_BAR_FLAG = "--closed-bar-decisions"
DECISION_REGIME_TIMEFRAME_FLAG = "--decision-regime-timeframe"

RULE_OPENING_TIME = "opening_time_fixed_duration"
RULE_HYPERLIQUID_NATIVE = "hyperliquid_native_close"

TIMESTAMP_KIND_OPEN = "open"
TIMESTAMP_KIND_NATIVE_CLOSE = "native_close_inclusive"

MIN_CLOSED_ROWS = 30

FIXED_DURATION_MS = {
    "1m": 60_000,
    "3m": 180_000,
    "5m": 300_000,
    "15m": 900_000,
    "30m": 1_800_000,
    "1h": 3_600_000,
    "2h": 7_200_000,
    "4h": 14_400_000,
    "6h": 21_600_000,
    "8h": 28_800_000,
    "12h": 43_200_000,
    "1d": 86_400_000,
}

UNSUPPORTED_OPEN_STRATEGIES = {
    "delta_neutral_funding": "its scalar and rolling funding average have no historical value at the decision boundary",
    "open_interest_breakout": "its open-interest observations have no closed-bar decision contract yet",
}


class ClosedBarHold(Exception):

    def __init__(self, reason: str):
        self.reason = str(reason)
        super().__init__(self.reason)


@dataclass(frozen=True)
class BarTiming:
    open_ms: int
    native_close_ms: Optional[int]


@dataclass(frozen=True)
class ClosedSelection:
    rows: list
    timings: list
    cutoff_ms: int
    boundary_ms: int
    bar_open_ms: int
    forming_rows_dropped: int
    rule: str
    interval_ms: int
    input_sha256: str

    def metadata(self) -> dict:
        return {
            "enabled": True,
            "held": False,
            "hold_reason": "",
            "cutoff_ms": int(self.cutoff_ms),
            "decision_boundary_ms": int(self.boundary_ms),
            "bar_open_ms": int(self.bar_open_ms),
            "closure_rule": self.rule,
            "interval_ms": int(self.interval_ms),
            "rows": len(self.rows),
            "forming_rows_dropped": int(self.forming_rows_dropped),
            "input_sha256": self.input_sha256,
        }


def hold_metadata(reason: str, cutoff_ms: Optional[int] = None) -> dict:
    return {
        "enabled": True,
        "held": True,
        "hold_reason": str(reason),
        "cutoff_ms": int(cutoff_ms or 0),
    }


def interval_ms_for(timeframe: str) -> int:
    key = str(timeframe or "").strip()
    ms = FIXED_DURATION_MS.get(key)
    if not ms:
        raise ClosedBarHold(
            f"timeframe {key!r} has no supported fixed-duration closure rule "
            f"(supported: {', '.join(FIXED_DURATION_MS)})"
        )
    return ms


def unsupported_open_strategy_reason(name: str) -> str:
    return UNSUPPORTED_OPEN_STRATEGIES.get(str(name or "").strip(), "")


def opening_time_timings(rows: list) -> list:
    return [BarTiming(open_ms=int(r[0]), native_close_ms=None) for r in rows]


def hyperliquid_rows_and_timings(candles: list) -> tuple[list, list]:
    rows = []
    timings = []
    for i, c in enumerate(candles):
        if not isinstance(c, dict):
            raise ClosedBarHold(f"candle {i} is not an object")
        try:
            open_ms = int(c["t"])
        except (KeyError, TypeError, ValueError):
            raise ClosedBarHold(f"candle {i} has no opening time 't'")
        native = c.get("T")
        native_close = None
        if native is not None:
            try:
                native_close = int(native)
            except (TypeError, ValueError):
                raise ClosedBarHold(f"candle {i} has an unreadable closing time 'T'")
        ts = native_close if native_close is not None else open_ms
        try:
            row = [ts, float(c["o"]), float(c["h"]), float(c["l"]), float(c["c"]), float(c["v"])]
        except (KeyError, TypeError, ValueError):
            raise ClosedBarHold(f"candle {i} has unreadable price or volume fields")
        rows.append(row)
        timings.append(BarTiming(open_ms=open_ms, native_close_ms=native_close))
    return rows, timings


def sealed_frame_timings(rows: list, timing: Optional[dict], timeframe: str) -> list:
    if not isinstance(timing, dict):
        raise ClosedBarHold("the sealed market frame carries no per-row timing; an older producer cannot prove bar closure")
    if timing.get("rule") != RULE_HYPERLIQUID_NATIVE:
        raise ClosedBarHold(f"the sealed market frame timing rule {timing.get('rule')!r} is not {RULE_HYPERLIQUID_NATIVE!r}")
    expected_interval = interval_ms_for(timeframe)
    try:
        sidecar_interval = int(timing.get("interval_ms") or 0)
    except (TypeError, ValueError):
        sidecar_interval = 0
    if sidecar_interval != expected_interval:
        raise ClosedBarHold(
            f"the sealed market frame interval {sidecar_interval} ms does not match {timeframe} ({expected_interval} ms)")
    bars = timing.get("bars")
    if not isinstance(bars, list) or len(bars) != len(rows):
        raise ClosedBarHold("the sealed market frame timing does not cover every row")
    out = []
    for i, (row, bar) in enumerate(zip(rows, bars)):
        if not isinstance(bar, list) or len(bar) != 3:
            raise ClosedBarHold(f"sealed timing row {i} is malformed")
        try:
            open_ms = int(bar[0])
            close_ms = int(bar[1])
        except (TypeError, ValueError):
            raise ClosedBarHold(f"sealed timing row {i} is malformed")
        has_close = bar[2]
        if not isinstance(has_close, bool):
            raise ClosedBarHold(f"sealed timing row {i} has a non-boolean close marker")
        native = close_ms if has_close else None
        expected_ts = close_ms if has_close else open_ms
        if int(row[0]) != expected_ts:
            raise ClosedBarHold(
                f"sealed row {i} timestamp {int(row[0])} contradicts its timing ({expected_ts})")
        out.append(BarTiming(open_ms=open_ms, native_close_ms=native))
    return out


def close_boundary_ms(timing: BarTiming, interval_ms: int) -> int:
    boundary = int(timing.open_ms) + int(interval_ms)
    if timing.native_close_ms is not None and int(timing.native_close_ms) + 1 != boundary:
        raise ClosedBarHold(
            f"bar opening {timing.open_ms} reports closing time {timing.native_close_ms}, "
            f"which contradicts the {interval_ms} ms interval")
    return boundary


def _finite(value) -> bool:
    try:
        return math.isfinite(float(value))
    except (TypeError, ValueError):
        return False


def _validate_row(i: int, row) -> None:
    if not isinstance(row, (list, tuple)) or len(row) != 6:
        raise ClosedBarHold(f"row {i} is malformed")
    o, h, low, c, v = row[1], row[2], row[3], row[4], row[5]
    if not all(_finite(x) for x in (o, h, low, c, v)):
        raise ClosedBarHold(f"row {i} carries a non-finite price or volume")
    if min(float(o), float(h), float(low), float(c)) <= 0:
        raise ClosedBarHold(f"row {i} carries a non-positive price")
    if float(h) < float(low):
        raise ClosedBarHold(f"row {i} has high below low")
    if float(v) < 0:
        raise ClosedBarHold(f"row {i} has negative volume")


def rows_sha256(rows: list, timings: list) -> str:
    blob = json.dumps(
        {
            "rows": [[int(r[0])] + [float(x) for x in r[1:6]] for r in rows],
            "timing": [[t.open_ms, t.native_close_ms] for t in timings],
        },
        sort_keys=True,
        separators=(",", ":"),
    )
    return hashlib.sha256(blob.encode("utf-8")).hexdigest()


def select_closed(rows: list, timings: list, *, timeframe: str, cutoff_ms: int, rule: str,
                  keep: int = 0, min_rows: int = MIN_CLOSED_ROWS) -> ClosedSelection:
    try:
        cutoff = int(cutoff_ms)
    except (TypeError, ValueError):
        raise ClosedBarHold("no evaluation cutoff was captured")
    if cutoff <= 0:
        raise ClosedBarHold("no evaluation cutoff was captured")
    interval = interval_ms_for(timeframe)
    if not rows:
        raise ClosedBarHold("the candle frame carries no rows")
    if len(rows) != len(timings):
        raise ClosedBarHold("the candle timing does not cover every row")
    boundaries = []
    prev_open = None
    prev_boundary = None
    for i, timing in enumerate(timings):
        boundary = close_boundary_ms(timing, interval)
        if prev_open is not None:
            if timing.open_ms == prev_open:
                raise ClosedBarHold(f"rows {i - 1} and {i} share opening time {timing.open_ms}")
            if timing.open_ms < prev_open:
                raise ClosedBarHold(f"row {i} opens before row {i - 1}")
            if timing.open_ms < prev_boundary:
                raise ClosedBarHold(f"row {i} opens before row {i - 1} closes")
        prev_open = timing.open_ms
        prev_boundary = boundary
        boundaries.append(boundary)
    last = -1
    for i, boundary in enumerate(boundaries):
        if boundary <= cutoff:
            last = i
    if last < 0:
        raise ClosedBarHold(f"no bar in the frame closed at or before the cutoff {cutoff}")
    selected_rows = [list(r) for r in rows[:last + 1]]
    selected_timings = list(timings[:last + 1])
    if keep and len(selected_rows) > keep:
        selected_rows = selected_rows[-keep:]
        selected_timings = selected_timings[-keep:]
    if len(selected_rows) < min_rows:
        raise ClosedBarHold(
            f"insufficient closed history: {len(selected_rows)} closed bars (need {min_rows})")
    for i, row in enumerate(selected_rows):
        _validate_row(i, row)
    return ClosedSelection(
        rows=selected_rows,
        timings=selected_timings,
        cutoff_ms=cutoff,
        boundary_ms=boundaries[last],
        bar_open_ms=timings[last].open_ms,
        forming_rows_dropped=len(rows) - (last + 1),
        rule=rule,
        interval_ms=interval,
        input_sha256=rows_sha256(selected_rows, selected_timings),
    )


def filter_records_to_boundary(records: Optional[list], boundary_ms: int) -> list:
    out = []
    for record in records or []:
        if not isinstance(record, dict):
            raise ClosedBarHold("a funding record is malformed")
        try:
            ts = int(record.get("time"))
        except (TypeError, ValueError):
            raise ClosedBarHold("a funding record has no readable time")
        if ts <= int(boundary_ms):
            out.append(record)
    return out


def htf_closed_fetcher(frame) -> Callable:
    def _fetch(_symbol, _timeframe, _limit):
        return frame.copy() if frame is not None else None
    return _fetch


def select_opening_time(rows: list, *, timeframe: str, cutoff_ms: int, keep: int = 0,
                        min_rows: int = MIN_CLOSED_ROWS) -> ClosedSelection:
    return select_closed(rows, opening_time_timings(rows), timeframe=timeframe, cutoff_ms=cutoff_ms,
                         rule=RULE_OPENING_TIME, keep=keep, min_rows=min_rows)
