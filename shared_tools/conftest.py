
import importlib.util
import sys
from pathlib import Path

import numpy as np
import pandas as pd

_HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(_HERE))


def load_module(name, path):
    spec = importlib.util.spec_from_file_location(name, Path(path))
    if spec is None or spec.loader is None:
        raise ImportError(f"cannot load module {name} from {path}")
    module = importlib.util.module_from_spec(spec)
    previous = sys.modules.get(name)
    sys.modules[name] = module
    try:
        spec.loader.exec_module(module)
    except Exception:
        if previous is None:
            sys.modules.pop(name, None)
        else:
            sys.modules[name] = previous
        raise
    return module


def make_ohlcv(
    closes,
    volume=None,
    noise=0.5,
    index=None,
    start="2026-01-01",
    freq="1h",
    opens=None,
    highs=None,
    lows=None,
):
    closes = np.asarray(closes, dtype=float)
    n = len(closes)
    if volume is None:
        volume = np.full(n, 100.0)
    volume = np.full(n, float(volume)) if np.isscalar(volume) else np.asarray(volume, dtype=float)
    highs = closes + noise if highs is None else np.asarray(highs, dtype=float)
    lows = closes - noise if lows is None else np.asarray(lows, dtype=float)
    opens = closes - noise * 0.3 if opens is None else np.asarray(opens, dtype=float)
    frame = pd.DataFrame({
        "open": opens,
        "high": highs,
        "low": lows,
        "close": closes,
        "volume": volume,
    })
    frame.index = (
        pd.date_range(start, periods=n, freq=freq)
        if index is None else index
    )
    return frame


def make_trend(n=100, start=100.0, end=200.0, noise=0.5):
    close = np.linspace(start, end, n)
    return make_ohlcv(close, noise=noise)
