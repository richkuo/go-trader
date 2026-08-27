import numpy as np
_HTF_MAP = {'1m': '15m', '5m': '1h', '15m': '1h', '30m': '4h', '1h': '4h', '4h': '1d', '1d': '1w', '1w': '1M'}

def get_default_htf(timeframe: str) -> str:
    return _HTF_MAP.get(timeframe, '4h')

def htf_trend_filter(symbol, timeframe, fetch_fn, htf=None, ema_period=50):
    htf = htf or get_default_htf(timeframe)
    result = {'htf_timeframe': htf, 'htf_trend': 0, 'htf_ema': 0.0, 'htf_close': 0.0}
    try:
        df = fetch_fn(symbol, htf, ema_period + 10)
        if df is None or len(df) < ema_period:
            return result
        closes = df['close'].astype(float).values
        ema = _compute_ema(closes, ema_period)
        current_close = float(closes[-1])
        current_ema = float(ema[-1])
        result['htf_close'] = round(current_close, 6)
        result['htf_ema'] = round(current_ema, 6)
        if current_close > current_ema:
            result['htf_trend'] = 1
        elif current_close < current_ema:
            result['htf_trend'] = -1
        else:
            result['htf_trend'] = 0
    except Exception:
        pass
    return result

def apply_htf_filter(signal, htf_trend):
    if signal == 0 or htf_trend == 0:
        return signal
    if signal == 1 and htf_trend == 1:
        return 1
    if signal == -1 and htf_trend == -1:
        return -1
    return 0

def _compute_ema(values, period):
    alpha = 2.0 / (period + 1)
    ema = np.empty_like(values, dtype=float)
    ema[0] = values[0]
    for i in range(1, len(values)):
        ema[i] = alpha * values[i] + (1 - alpha) * ema[i - 1]
    return ema
