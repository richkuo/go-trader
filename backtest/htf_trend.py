import os
import sys

import numpy as np
import pandas as pd

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "shared_tools"))

from closed_bar import FIXED_DURATION_MS
from htf_filter import HTF_EMA_PERIOD, apply_htf_filter, get_default_htf

MIN_CLOSED_HTF_BUCKETS = HTF_EMA_PERIOD


def align_htf_index(htf_index: pd.Index, ltf_index: pd.Index) -> pd.Index:
    if not isinstance(htf_index, pd.DatetimeIndex) or not isinstance(ltf_index, pd.DatetimeIndex):
        return htf_index
    if ltf_index.tz is not None and htf_index.tz is None:
        return htf_index.tz_localize("UTC").tz_convert(ltf_index.tz)
    if ltf_index.tz is None and htf_index.tz is not None:
        return htf_index.tz_convert("UTC").tz_localize(None)
    return htf_index


def htf_trend_from_candles(htf_df: pd.DataFrame, ltf_index: pd.Index,
                           ema_period: int = HTF_EMA_PERIOD) -> pd.Series:
    htf_index = align_htf_index(htf_df.index, ltf_index)
    closes = htf_df["close"].astype(float)
    ema = closes.ewm(span=ema_period, adjust=False).mean()
    trend = pd.Series(
        np.where(closes > ema, 1, np.where(closes < ema, -1, 0)),
        index=htf_index,
        dtype=int,
    )
    return trend.shift(1).reindex(ltf_index, method="ffill").fillna(0).astype(int)


def _fixed_ms(timeframe: str, role: str) -> int:
    ms = FIXED_DURATION_MS.get(str(timeframe or "").strip())
    if not ms:
        raise ValueError(f"frozen higher-timeframe input needs a fixed-duration {role} timeframe, "
                         f"got {timeframe!r}")
    return int(ms)


def derive_htf_candles(ltf_df: pd.DataFrame, ltf_timeframe: str) -> tuple:
    htf = get_default_htf(ltf_timeframe)
    ltf_ms = _fixed_ms(ltf_timeframe, "chart")
    htf_ms = _fixed_ms(htf, "higher")
    if htf_ms <= ltf_ms or htf_ms % ltf_ms:
        raise ValueError(f"{htf} is not a whole multiple of {ltf_timeframe}")
    if not isinstance(ltf_df.index, pd.DatetimeIndex):
        raise ValueError("frozen higher-timeframe input needs a datetime-indexed candle frame")
    idx = ltf_df.index
    if idx.tz is not None:
        idx = idx.tz_convert("UTC").tz_localize(None)
    open_ms = idx.values.astype("datetime64[ms]").astype(np.int64)
    if len(open_ms) > 1 and not (np.diff(open_ms) > 0).all():
        raise ValueError("frozen higher-timeframe input needs strictly increasing candle times")
    bucket = (open_ms // htf_ms) * htf_ms
    last_slot = bucket + htf_ms - ltf_ms
    frame = pd.DataFrame({
        "bucket": bucket,
        "open": ltf_df["open"].astype(float).to_numpy(),
        "high": ltf_df["high"].astype(float).to_numpy(),
        "low": ltf_df["low"].astype(float).to_numpy(),
        "close": ltf_df["close"].astype(float).to_numpy(),
        "volume": ltf_df["volume"].astype(float).to_numpy(),
        "is_last": open_ms == last_slot,
        "slots": 1,
    })
    grouped = frame.groupby("bucket", sort=True)
    agg = grouped.agg(open=("open", "first"), high=("high", "max"), low=("low", "min"),
                      close=("close", "last"), volume=("volume", "sum"),
                      closed=("is_last", "any"), slots=("slots", "sum"))
    want = htf_ms // ltf_ms
    kept = agg[agg["closed"]]
    dropped = agg[~agg["closed"]]
    out = kept.loc[:, ["open", "high", "low", "close", "volume"]].copy()
    out.index = pd.to_datetime(kept.index.to_numpy(), unit="ms")
    out.index.name = "datetime"
    coverage = {
        "htf_timeframe": htf,
        "buckets_closed": int(len(kept)),
        "buckets_dropped_unclosed": int(len(dropped)),
        "dropped_bucket_opens": [str(pd.Timestamp(int(b), unit="ms")) for b in dropped.index[:20]],
        "closed_buckets_with_missing_slots": int((kept["slots"] < want).sum()),
        "first_bucket": str(out.index[0]) if len(out) else None,
        "last_bucket": str(out.index[-1]) if len(out) else None,
    }
    return out, coverage


def closed_buckets_before(htf_df: pd.DataFrame, htf_timeframe: str, first_scored) -> int:
    if not len(htf_df):
        return 0
    htf_ms = _fixed_ms(htf_timeframe, "higher")
    first = pd.Timestamp(first_scored)
    if first.tzinfo is not None:
        first = first.tz_convert("UTC").tz_localize(None)
    closes = htf_df.index + pd.Timedelta(milliseconds=htf_ms)
    return int((closes <= first).sum())


def frozen_htf_trend(ltf_df: pd.DataFrame, ltf_timeframe: str,
                     ema_period: int = HTF_EMA_PERIOD) -> tuple:
    htf_df, coverage = derive_htf_candles(ltf_df, ltf_timeframe)
    if len(htf_df) < ema_period:
        raise ValueError(f"frozen higher-timeframe input has {len(htf_df)} closed "
                         f"{coverage['htf_timeframe']} buckets, needs at least {ema_period}")
    trend = htf_trend_from_candles(htf_df, ltf_df.index, ema_period=ema_period)
    coverage["ema_period"] = int(ema_period)
    return trend, coverage, htf_df


def apply_frozen_htf_filter(signals: pd.DataFrame, ltf_timeframe: str, first_scored,
                            ema_period: int = HTF_EMA_PERIOD,
                            min_closed_buckets: int = MIN_CLOSED_HTF_BUCKETS) -> tuple:
    if "signal" not in signals.columns:
        raise ValueError("frozen higher-timeframe filter needs a signal column")
    trend, coverage, htf_df = frozen_htf_trend(signals, ltf_timeframe, ema_period=ema_period)
    before = closed_buckets_before(htf_df, coverage["htf_timeframe"], first_scored)
    if before < min_closed_buckets:
        raise ValueError(f"frozen higher-timeframe filter: {before} closed {coverage['htf_timeframe']} "
                         f"buckets precede the first scored bar {first_scored}, needs {min_closed_buckets}")
    raw = signals["signal"].fillna(0).astype(int).to_numpy()
    tv = trend.to_numpy()
    filtered = np.array([apply_htf_filter(int(s), int(t)) for s, t in zip(raw, tv)], dtype=int)
    out = signals.copy()
    out["signal"] = filtered
    first = pd.Timestamp(first_scored)
    scored = np.asarray(signals.index >= first)
    stats = {}
    for side, value in (("long", 1), ("short", -1)):
        mask = scored & (raw == value)
        stats[f"{side}_signals"] = int(mask.sum())
        stats[f"{side}_passed"] = int((mask & (filtered == value)).sum())
        stats[f"{side}_blocked"] = int((mask & (filtered == 0)).sum())
    coverage.update({
        "source": "derived_from_leg_candles",
        "closed_buckets_before_first_scored_bar": before,
        "min_closed_buckets": int(min_closed_buckets),
        "first_scored_bar": str(first),
        "trend_counts_scored": {str(k): int(v) for k, v in
                                sorted(pd.Series(tv[scored]).value_counts().items())},
        **stats,
    })
    return out, coverage
