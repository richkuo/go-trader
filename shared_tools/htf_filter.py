import math
import sys

import numpy as np


_HTF_MAP = {
    "1m": "15m",
    "5m": "1h",
    "15m": "1h",
    "30m": "4h",
    "1h": "4h",
    "4h": "1d",
    "1d": "1w",
    "1w": "1M",
}

HTF_EMA_PERIOD = 50

HTF_STATUS_OK = "ok"
HTF_STATUS_UNAVAILABLE = "unavailable"
HTF_STATUS_SKIPPED = "skipped"
HTF_STATUS_NOT_EVALUATED = "not_evaluated"

HTF_EXCLUDED_DEFAULT = ("delta_neutral_funding",)


def get_default_htf(timeframe: str) -> str:
    return _HTF_MAP.get(timeframe, "4h")


def _unavailable(result, reason):
    result["htf_status"] = HTF_STATUS_UNAVAILABLE
    result["htf_reason"] = str(reason)
    return result


def htf_trend_filter(symbol, timeframe, fetch_fn, htf=None, ema_period=HTF_EMA_PERIOD):
    htf = htf or get_default_htf(timeframe)
    result = {"htf_timeframe": htf, "htf_trend": 0, "htf_ema": 0.0, "htf_close": 0.0,
              "htf_status": HTF_STATUS_UNAVAILABLE, "htf_reason": ""}

    try:
        df = fetch_fn(symbol, htf, ema_period + 10)
    except Exception as e:
        return _unavailable(result, f"fetching {symbol} {htf} candles failed: {type(e).__name__}: {e}")
    if df is None:
        return _unavailable(result, f"no {htf} candle frame was returned for {symbol}")
    try:
        rows = len(df)
    except Exception as e:
        return _unavailable(result, f"the {symbol} {htf} candle frame is unreadable: {type(e).__name__}: {e}")
    if rows < ema_period:
        return _unavailable(result, f"{rows} {htf} candles available for {symbol} (need {ema_period})")

    try:
        closes = np.asarray(df["close"].astype(float).values, dtype=float)
    except Exception as e:
        return _unavailable(result, f"the {symbol} {htf} close values are unreadable: {type(e).__name__}: {e}")
    bad = int(np.count_nonzero(~np.isfinite(closes)))
    if bad:
        return _unavailable(result, f"{bad} of {rows} {htf} close values for {symbol} are not finite")
    nonpositive = int(np.count_nonzero(closes <= 0))
    if nonpositive:
        return _unavailable(result, f"{nonpositive} of {rows} {htf} close values for {symbol} are not positive")

    try:
        ema = _compute_ema(closes, ema_period)
        current_close = float(closes[-1])
        current_ema = float(ema[-1])
    except Exception as e:
        return _unavailable(result, f"the {symbol} {htf} EMA calculation failed: {type(e).__name__}: {e}")
    if not (math.isfinite(current_close) and math.isfinite(current_ema)):
        return _unavailable(result, f"the {symbol} {htf} EMA or final close is not finite")

    result["htf_close"] = round(current_close, 6)
    result["htf_ema"] = round(current_ema, 6)
    if current_close > current_ema:
        result["htf_trend"] = 1
    elif current_close < current_ema:
        result["htf_trend"] = -1
    else:
        result["htf_trend"] = 0
    result["htf_status"] = HTF_STATUS_OK
    result["htf_reason"] = ""
    return result


def apply_htf_filter(signal, htf_trend):
    if signal == 0 or htf_trend == 0:
        return signal

    if signal == 1 and htf_trend == 1:
        return 1
    if signal == -1 and htf_trend == -1:
        return -1

    return 0


def htf_filter_block(status, timeframe, trend=0, reason=""):
    return {"status": str(status), "timeframe": str(timeframe), "trend": int(trend), "reason": str(reason)}


def htf_skipped_block(timeframe, strategy_name):
    return htf_filter_block(
        HTF_STATUS_SKIPPED, get_default_htf(timeframe), 0,
        f"open strategy {strategy_name} is excluded from the higher-timeframe filter")


def htf_not_evaluated_block(timeframe, cause):
    return htf_filter_block(HTF_STATUS_NOT_EVALUATED, get_default_htf(timeframe), 0, cause)


def htf_unavailable_block(symbol, htf, reason, stream=None):
    print(f"HTF filter unavailable for {symbol} {htf}: {reason} - the open signal is not filtered; "
          f"the scheduler holds position-increasing entries", file=stream or sys.stderr)
    return htf_filter_block(HTF_STATUS_UNAVAILABLE, htf, 0, reason)


def run_htf_filter(signal, symbol, timeframe, fetch_fn, stream=None):
    info = htf_trend_filter(symbol, timeframe, fetch_fn)
    htf = info["htf_timeframe"]
    if info["htf_status"] != HTF_STATUS_OK:
        return signal, htf_unavailable_block(symbol, htf, info["htf_reason"], stream), {}
    trend = int(info["htf_trend"])
    filtered = apply_htf_filter(signal, trend)
    if filtered != signal:
        print(f"HTF filter: {signal} → {filtered} (HTF trend={trend})", file=stream or sys.stderr)
    indicators = {k: v for k, v in info.items()
                  if k in ("htf_trend", "htf_ema", "htf_close") and isinstance(v, (int, float))}
    return filtered, htf_filter_block(HTF_STATUS_OK, htf, trend, ""), indicators


def _compute_ema(values, period):
    alpha = 2.0 / (period + 1)
    ema = np.empty_like(values, dtype=float)
    ema[0] = values[0]
    for i in range(1, len(values)):
        ema[i] = alpha * values[i] + (1 - alpha) * ema[i - 1]
    return ema
