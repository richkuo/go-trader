import hashlib
import json
import math

import numpy as np
import pandas as pd

from closed_bar import (
    FIXED_DURATION_MS,
    FRAME_TIMESTAMP_KIND_ATTR,
    RULE_HYPERLIQUID_NATIVE,
    TIMESTAMP_KIND_NATIVE_CLOSE,
    TIMESTAMP_KIND_OPEN,
    ClosedBarHold,
    close_boundary_ms,
    sealed_frame_timings,
    select_closed,
)
from market_payload import (
    MarketPayloadError,
    frame_key,
    market_decision_cutoff_ms,
    market_frame_rows_with_timing,
    validate_market_payload,
)

LEAD_LAG_STRATEGY = "bitcoin_lead_lag"
LEADER_COIN = "BTC"
CONTEXT_KIND = "bitcoin_lead_lag_inputs/v1"
BASIS_SEALED = "sealed_closed"
BASIS_HISTORICAL = "historical_closed"

HOLD_REASONS = (
    "leader_unavailable",
    "follower_is_leader",
    "payload_invalid",
    "frame_missing",
    "frame_not_ready",
    "frame_stale",
    "timing_invalid",
    "duplicate_bar",
    "reordered_bar",
    "cutoff_missing",
    "no_closed_bar",
    "invalid_row",
    "insufficient_history",
    "boundary_mismatch",
    "bar_gap",
    "bar_mismatch",
)


def support_bars(lag_bars: int, horizon_bars: int, training_bars: int) -> int:
    return int(training_bars) + 2 * int(lag_bars) + 2 * int(horizon_bars) - 2


def coin_of(symbol: str) -> str:
    return str(symbol or "").split("/")[0].split("-")[0].strip().upper()


def unavailable(reason: str, detail: str = "", **extra) -> dict:
    if reason not in HOLD_REASONS:
        raise ValueError(f"unknown lead-lag input hold reason {reason!r}")
    return {"kind": CONTEXT_KIND, "available": False, "hold_reason": reason, "detail": str(detail), **extra}


class _Hold(Exception):
    def __init__(self, reason: str, detail: str):
        super().__init__(detail)
        self.reason = reason
        self.detail = detail


def interval_ms(timeframe: str) -> int:
    return int(FIXED_DURATION_MS.get(str(timeframe or "").strip()) or 0)


def _row_defect(row) -> str:
    try:
        o, h, low, c, v = (float(row[k]) for k in (1, 2, 3, 4, 5))
    except (TypeError, ValueError, IndexError):
        return "is malformed"
    if not all(math.isfinite(x) for x in (o, h, low, c, v)):
        return "carries a non-finite price or volume"
    if min(o, h, low, c) <= 0:
        return "carries a non-positive price"
    if h < low or o > h or o < low or c > h or c < low:
        return "has inconsistent high/low geometry"
    if v < 0:
        return "has negative volume"
    return ""


def _select(market, coin: str, timeframe: str, cutoff: int, required_rows: int, role: str):
    key = frame_key(coin, timeframe)
    frame = (market.get("frames") or {}).get(key)
    if not isinstance(frame, dict):
        raise _Hold("frame_missing", f"{role} frame {key} is absent from the sealed payload")
    if not frame.get("ready"):
        raise _Hold("frame_not_ready", f"{role} frame {key} is not ready")
    if frame.get("stale"):
        raise _Hold("frame_stale", f"{role} frame {key} is stale: {frame.get('stale_reason') or 'no reason given'}")
    try:
        rows, timing = market_frame_rows_with_timing(market, coin, timeframe, MarketPayloadError)
    except (MarketPayloadError, TypeError, ValueError) as exc:
        raise _Hold("payload_invalid", f"{role} frame {key}: {exc}")
    try:
        timings = sealed_frame_timings(rows, timing, timeframe)
    except ClosedBarHold as exc:
        raise _Hold("timing_invalid", f"{role} frame {key}: {exc.reason}")
    for k in range(1, len(timings)):
        if timings[k].open_ms == timings[k - 1].open_ms:
            raise _Hold("duplicate_bar", f"{role} frame {key} rows {k - 1} and {k} share opening time {timings[k].open_ms}")
        if timings[k].open_ms < timings[k - 1].open_ms:
            raise _Hold("reordered_bar", f"{role} frame {key} row {k} opens before row {k - 1}")
    closed = [k for k, t in enumerate(timings) if close_boundary_ms(t, interval_ms(timeframe)) <= cutoff]
    for k in closed:
        why = _row_defect(rows[k])
        if why:
            raise _Hold("invalid_row", f"{role} frame {key} closed row {k} {why}")
    try:
        sel = select_closed(rows, timings, timeframe=timeframe, cutoff_ms=cutoff, rule=RULE_HYPERLIQUID_NATIVE,
                            keep=0, min_rows=1)
    except ClosedBarHold as exc:
        if "no bar in the frame closed" in exc.reason:
            raise _Hold("no_closed_bar", f"{role} frame {key}: {exc.reason}")
        raise _Hold("timing_invalid", f"{role} frame {key}: {exc.reason}")
    if len(sel.rows) < required_rows:
        raise _Hold("insufficient_history",
                    f"{role} frame {key} has {len(sel.rows)} closed bars at the cutoff, needs {required_rows}")
    recv = frame.get("last_recv_at_ms")
    return sel, frame, (int(recv) if isinstance(recv, (int, float)) and not isinstance(recv, bool) else None)


def prepare_sealed_inputs(market, follower_symbol: str, timeframe: str, required_rows: int):
    follower = coin_of(follower_symbol)
    if follower == LEADER_COIN:
        return None, None, unavailable("follower_is_leader", "the follower is the BTC leader", follower=follower)
    try:
        payload = validate_market_payload(market, MarketPayloadError)
    except MarketPayloadError as exc:
        return None, None, unavailable("payload_invalid", str(exc), follower=follower)
    interval = interval_ms(timeframe)
    if not interval:
        return None, None, unavailable("timing_invalid", f"timeframe {timeframe!r} has no fixed duration",
                                       follower=follower)
    cutoff = market_decision_cutoff_ms(payload, MarketPayloadError)
    if cutoff <= 0:
        return None, None, unavailable("cutoff_missing", "the sealed payload carries no decision cutoff",
                                       follower=follower)
    required_rows = int(required_rows)
    try:
        fsel, fframe, frecv = _select(payload, follower, timeframe, cutoff, required_rows, "follower")
        lsel, lframe, lrecv = _select(payload, LEADER_COIN, timeframe, cutoff, required_rows, "leader")
        if fsel.boundary_ms != lsel.boundary_ms:
            raise _Hold("boundary_mismatch", f"follower last closed boundary {fsel.boundary_ms} != leader "
                                             f"{lsel.boundary_ms} at cutoff {cutoff}")
        f_rows, f_tim = fsel.rows[-required_rows:], fsel.timings[-required_rows:]
        l_rows, l_tim = lsel.rows[-required_rows:], lsel.timings[-required_rows:]
        for k in range(required_rows):
            fo, lo = f_tim[k].open_ms, l_tim[k].open_ms
            if fo != lo or close_boundary_ms(f_tim[k], interval) != close_boundary_ms(l_tim[k], interval):
                raise _Hold("bar_mismatch", f"selected row {k} follower opens {fo}, leader opens {lo}")
            if k and fo - f_tim[k - 1].open_ms != interval:
                raise _Hold("bar_gap", f"selected rows {k - 1} and {k} are {fo - f_tim[k - 1].open_ms} ms apart, "
                                       f"not {interval}")
    except _Hold as hold:
        return None, None, unavailable(hold.reason, hold.detail, follower=follower, cutoff_ms=cutoff)
    except ClosedBarHold as exc:
        return None, None, unavailable("timing_invalid", exc.reason, follower=follower, cutoff_ms=cutoff)
    context = {
        "kind": CONTEXT_KIND,
        "available": True,
        "basis": BASIS_SEALED,
        "leader": LEADER_COIN,
        "follower": follower,
        "timeframe": timeframe,
        "interval_ms": interval,
        "snapshot_id": str(payload.get("snapshot_id")),
        "sealed_at_ms": int(payload.get("sealed_at_ms") or 0) or None,
        "cutoff_ms": int(cutoff),
        "boundary_ms": int(fsel.boundary_ms),
        "leader_open_ms": [int(t.open_ms) for t in l_tim],
        "leader_close": [float(r[4]) for r in l_rows],
        "leader_last_recv_at_ms": lrecv,
        "follower_last_recv_at_ms": frecv,
        "leader_source": str(lframe.get("source") or ""),
        "follower_source": str(fframe.get("source") or ""),
        "leader_forming_rows_dropped": int(lsel.forming_rows_dropped),
        "follower_forming_rows_dropped": int(fsel.forming_rows_dropped),
        "leader_rows_sha256": rows_digest(l_rows, l_tim),
        "follower_rows_sha256": rows_digest(f_rows, f_tim),
        "rows": required_rows,
    }
    return f_rows, f_tim, context


def rows_digest(rows, timings) -> str:
    blob = json.dumps({"rows": [[int(r[0])] + [float(x) for x in r[1:6]] for r in rows],
                       "timing": [[t.open_ms, t.native_close_ms] for t in timings]},
                      sort_keys=True, separators=(",", ":"))
    return hashlib.sha256(blob.encode("utf-8")).hexdigest()


def frame_open_ms(df: pd.DataFrame, interval: int) -> np.ndarray:
    if isinstance(df.index, pd.DatetimeIndex):
        idx = df.index
        if idx.tz is not None:
            idx = idx.tz_convert("UTC").tz_localize(None)
        values = idx.as_unit("ms").asi8.astype(np.int64)
    elif "timestamp" in df.columns:
        values = pd.to_numeric(df["timestamp"], errors="raise").to_numpy(dtype=np.int64)
    else:
        raise ValueError("the candle frame has neither a DatetimeIndex nor a timestamp column")
    if df.attrs.get(FRAME_TIMESTAMP_KIND_ATTR) == TIMESTAMP_KIND_NATIVE_CLOSE:
        values = values - interval + 1
    return values


def series_digest(opens, closes) -> str:
    blob = json.dumps([[int(t), float(c)] for t, c in zip(opens, closes)], separators=(",", ":"))
    return hashlib.sha256(blob.encode("utf-8")).hexdigest()


def prepare_historical_inputs(follower_df: pd.DataFrame, leader_df: pd.DataFrame, follower_symbol: str,
                              timeframe: str, *, source: dict = None) -> dict:
    follower = coin_of(follower_symbol)
    if follower == LEADER_COIN:
        return unavailable("follower_is_leader", "the follower is the BTC leader", follower=follower)
    interval = FIXED_DURATION_MS.get(str(timeframe or "").strip())
    if not interval:
        return unavailable("timing_invalid", f"timeframe {timeframe!r} has no fixed duration", follower=follower)
    if leader_df is None or len(leader_df) == 0:
        return unavailable("frame_missing", "no historical BTC leader series was supplied", follower=follower)
    if follower_df is None or len(follower_df) == 0:
        return unavailable("frame_missing", "no historical follower series was supplied", follower=follower)
    if leader_df.attrs.get(FRAME_TIMESTAMP_KIND_ATTR) not in (TIMESTAMP_KIND_OPEN, TIMESTAMP_KIND_NATIVE_CLOSE, None):
        return unavailable("timing_invalid", "the leader frame declares an unknown timestamp kind", follower=follower)
    lopen = frame_open_ms(leader_df, interval)
    fopen = frame_open_ms(follower_df, interval)
    if len(lopen) > 1:
        step = np.diff(lopen)
        if (step == 0).any():
            return unavailable("duplicate_bar", "the leader series repeats an opening time", follower=follower)
        if (step < 0).any():
            return unavailable("reordered_bar", "the leader series is not in time order", follower=follower)
    if (lopen % interval).any():
        return unavailable("timing_invalid", f"a leader opening time is off the {interval} ms grid", follower=follower)
    lclose = pd.to_numeric(leader_df["close"], errors="coerce").to_numpy(dtype="float64")
    lo, hi = int(fopen.min()), int(fopen.max())
    keep = (lopen >= lo) & (lopen <= hi) & np.isfinite(lclose) & (lclose > 0)
    opens = lopen[keep]
    closes = lclose[keep]
    fclose = pd.to_numeric(follower_df["close"], errors="coerce").to_numpy(dtype="float64")
    expected = (hi - lo) // interval + 1
    return {
        "kind": CONTEXT_KIND,
        "available": True,
        "basis": BASIS_HISTORICAL,
        "leader": LEADER_COIN,
        "follower": follower,
        "timeframe": timeframe,
        "interval_ms": interval,
        "leader_open_ms": [int(t) for t in opens],
        "leader_close": [float(c) for c in closes],
        "coverage": {
            "follower_first_open_ms": lo,
            "follower_last_open_ms": hi,
            "follower_rows": int(len(fopen)),
            "leader_rows_matched": int(np.isin(fopen, opens).sum()),
            "leader_rows_in_span": int(len(opens)),
            "grid_bars": int(expected),
            "leader_missing_in_span": int(expected - len(opens)),
        },
        "leader_sha256": series_digest(opens, closes),
        "follower_sha256": series_digest(fopen, fclose),
        "source": dict(source or {}),
    }
