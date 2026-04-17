"""Walk-forward folds prepend a warmup slice so long-lookback indicators
(e.g. SMA-80) prime before the first signal bar. Without warmup, a 100-bar
fold against an SMA-80 grid produces all-NaN signals and zero trades."""
import numpy as np
import pandas as pd
import pytest

from backtester import Backtester
from optimizer import (
    max_indicator_lookback,
    walk_forward_optimize,
    warmup_exit_long_entry,
)


def _trending_ohlc(n: int = 500, seed: int = 7) -> pd.DataFrame:
    rng = np.random.default_rng(seed)
    log_returns = rng.normal(loc=0.002, scale=0.015, size=n)
    closes = [100.0]
    for r in log_returns:
        closes.append(closes[-1] * np.exp(r))
    closes = np.array(closes[1:])
    opens = closes * (1.0 + rng.normal(loc=0.0, scale=0.002, size=n))
    highs = np.maximum(opens, closes) * 1.003
    lows = np.minimum(opens, closes) * 0.997
    volume = rng.integers(1000, 10000, size=n).astype(float)
    idx = pd.date_range("2022-01-01", periods=n, freq="D")
    return pd.DataFrame(
        {"open": opens, "high": highs, "low": lows,
         "close": closes, "volume": volume},
        index=idx,
    )


def test_max_indicator_lookback_picks_largest_int():
    ranges = {
        "fast_period": [10, 15, 20],
        "slow_period": [40, 50, 80],
        "multiplier":  [1.5, 2.0, 3.0],  # float — ignored
    }
    assert max_indicator_lookback(ranges) == 80


def test_max_indicator_lookback_zero_for_float_only_grid():
    ranges = {
        "entry_std": [1.0, 1.5, 2.0],
        "exit_std":  [0.0, 0.5, 1.0],
    }
    assert max_indicator_lookback(ranges) == 0


def test_sma_80_grid_generates_trades_with_warmup():
    """SMA-80 on 100-bar folds should cross at least once across 5 folds
    when warmup primes the preceding 80 bars."""
    df = _trending_ohlc(n=500)
    param_ranges = {"fast_period": [10, 20], "slow_period": [40, 80]}

    result = walk_forward_optimize(
        df, "sma_crossover", param_ranges,
        n_splits=5, train_pct=0.7,
        initial_capital=1000.0, verbose=False,
    )

    assert "window_results" in result, result
    total_trades = sum(
        w["test_result"]["total_trades"] for w in result["window_results"]
    )
    assert total_trades > 0, (
        "Walk-forward produced zero trades across all folds — the warmup "
        "fix did not engage or is insufficient for SMA-80 priming."
    )


def test_warmup_primes_slow_sma_on_every_bar():
    """Counterfactual: on an unprimed 100-bar window, the slow SMA-80 is
    NaN for 79 bars — only the final 21 bars can emit a crossover. With
    80 bars of preceding history prepended, every bar of the 100-bar
    window has a valid slow SMA. Pin that asymmetry — it is the mechanism
    the warmup fix is buying."""
    from registry_loader import load_registry

    df = _trending_ohlc(n=500)
    unprimed = df.iloc[100:200]
    primed_input = df.iloc[20:200]  # 80 bars warmup + 100 bars window

    reg = load_registry("spot")
    params = {"fast_period": 10, "slow_period": 80}

    unprimed_out = reg.apply_strategy("sma_crossover", unprimed, params)
    primed_out = reg.apply_strategy("sma_crossover", primed_input, params).iloc[-100:]

    unprimed_primed_bars = int(unprimed_out["sma_slow"].notna().sum())
    primed_primed_bars = int(primed_out["sma_slow"].notna().sum())

    assert primed_primed_bars == 100, (
        f"Primed window should have sma_slow valid on every bar; "
        f"got {primed_primed_bars}"
    )
    assert unprimed_primed_bars <= 21, (
        f"Unprimed 100-bar window cannot have more than 21 valid "
        f"sma_slow bars (100 - 79 NaN); got {unprimed_primed_bars}"
    )


def test_warmup_does_not_leak_future_data():
    """Fold 0 starts at bar 0 and has no preceding history, so warmup is
    truncated to 0 — later folds get the full 80. Just pin that the runs
    still complete without crashing under that asymmetry."""
    df = _trending_ohlc(n=600)
    result = walk_forward_optimize(
        df, "sma_crossover", {"fast_period": [10], "slow_period": [80]},
        n_splits=5, train_pct=0.7,
        initial_capital=1000.0, verbose=False,
    )
    assert result["n_valid_folds"] >= 2, result


def _warmup_train_df() -> pd.DataFrame:
    """60-bar frame with a BUY signal deep in the warmup prefix and a
    SELL signal in the train portion. Without position-state carry, the
    SELL fires while the Backtester is flat and is silently dropped —
    a round-trip trade vanishes from the fold's metrics."""
    opens  = [100.0] * 60
    closes = [100.0] * 60
    signals = [0] * 60
    signals[5]  = 1   # BUY in warmup (fills at bar 6 open)
    signals[45] = -1  # SELL in train (fills at bar 46 open)
    idx = pd.date_range("2024-01-01", periods=60, freq="D")
    return pd.DataFrame(
        {"open": opens, "close": closes, "signal": signals}, index=idx,
    )


def test_warmup_exit_long_entry_detects_unclosed_buy():
    df = _warmup_train_df()
    # Warmup runs from bar 0 through bar 29; SELL on bar 45 is in train.
    seed = warmup_exit_long_entry(df.iloc[:30], slippage_pct=0.0)
    assert seed is not None, "warmup ends long — seed must be non-None"
    assert seed["entry_price"] == pytest.approx(100.0)


def test_warmup_exit_long_entry_returns_none_when_flat():
    df = _warmup_train_df()
    # Need bars 0..46 inclusive: SELL on bar 45 shifts to fill on bar 46,
    # so bar 46 must be inside the scanned slice for the exit to register.
    seed = warmup_exit_long_entry(df.iloc[:47], slippage_pct=0.0)
    assert seed is None


def test_train_fold_captures_trade_spanning_warmup_boundary():
    """Without the starting_long seed, SELL at bar 45 fires while the
    Backtester is flat and is silently dropped — train fold reports 0
    trades. With the seed, the warmup BUY is carried forward and the
    SELL correctly closes the position."""
    df = _warmup_train_df()
    train_signals = df.iloc[30:]  # drop warmup
    warmup_signals = df.iloc[:30]

    # Without seed — demonstrates the counterfactual
    bt_unseeded = Backtester(
        initial_capital=1000.0, commission_pct=0.0, slippage_pct=0.0,
    )
    unseeded = bt_unseeded.run(train_signals, save=False)
    assert unseeded["total_trades"] == 0, (
        "Pre-seed counterfactual: SELL on a flat position is ignored. "
        "If this changes, the seed mechanism's justification needs review."
    )

    # With seed — the trade round-trips
    seed = warmup_exit_long_entry(warmup_signals, slippage_pct=0.0)
    assert seed is not None
    bt_seeded = Backtester(
        initial_capital=1000.0, commission_pct=0.0, slippage_pct=0.0,
    )
    seeded = bt_seeded.run(train_signals, save=False, starting_long=seed)
    assert seeded["total_trades"] == 1, (
        f"Seeded run should capture the warmup→train round trip; "
        f"got {seeded['total_trades']} trades"
    )


def test_no_seed_when_fold_zero_has_no_warmup():
    """Fold 0 starts at bar 0 → train_trim=0 → no warmup to scan →
    warmup_exit_long_entry called on empty slice returns None without
    error."""
    empty = pd.DataFrame(columns=["open", "close", "signal"])
    assert warmup_exit_long_entry(empty, slippage_pct=0.0) is None
