"""
End-to-end golden-file regression for issue #302.

Runs the Backtester on a deterministic synthetic OHLC series with an
SMA-crossover signal stream and pins the summary stats. The unit tests
cover each fix in isolation; this locks in their combined behavior —
a regression that fixes each piece individually but drifts the integrated
P&L will fail here.
"""
import numpy as np
import pandas as pd
import pytest

from backtester import Backtester


def _synthetic_ohlc(seed: int = 42, n: int = 200) -> pd.DataFrame:
    """Generate a deterministic OHLC path with intrabar open/close spread."""
    rng = np.random.default_rng(seed)
    log_returns = rng.normal(loc=0.0003, scale=0.02, size=n)
    closes = [100.0]
    for r in log_returns:
        closes.append(closes[-1] * np.exp(r))
    closes = np.array(closes[1:])
    # Opens deterministically offset from close so fills at open != close.
    opens = closes * (1.0 + rng.normal(loc=0.0, scale=0.003, size=n))
    highs = np.maximum(opens, closes) * 1.002
    lows = np.minimum(opens, closes) * 0.998

    idx = pd.date_range("2024-01-01", periods=n, freq="D")
    return pd.DataFrame(
        {"open": opens, "high": highs, "low": lows, "close": closes},
        index=idx,
    )


def _sma_crossover_signals(df: pd.DataFrame, fast: int = 10, slow: int = 30) -> pd.Series:
    fast_ma = df["close"].rolling(fast).mean()
    slow_ma = df["close"].rolling(slow).mean()
    position = (fast_ma > slow_ma).astype(int)
    # position.diff() → 1 on buy crossover, -1 on sell crossover, 0 otherwise.
    return position.diff().fillna(0).astype(int)


def test_end_to_end_golden_file():
    df = _synthetic_ohlc()
    df["signal"] = _sma_crossover_signals(df)

    bt = Backtester(
        initial_capital=1000.0, commission_pct=0.001, slippage_pct=0.0005
    )
    results = bt.run(df, strategy_name="e2e-golden", save=False)

    # Golden values — update intentionally if (and only if) the execution
    # model changes. A regression that drifts signal-shift alignment,
    # fill-at-next-open, or the metrics pipeline will break at least one.
    assert results["total_trades"] == 6
    assert results["final_capital"] == pytest.approx(923.53, abs=0.01)
    assert results["total_return_pct"] == pytest.approx(-7.51, abs=0.01)
    assert results["sharpe_ratio"] == pytest.approx(-0.686, abs=0.001)
    assert results["max_drawdown_pct"] == pytest.approx(-17.66, abs=0.01)
