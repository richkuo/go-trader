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


def test_warmup_exit_long_entry_applies_slippage():
    """Seed entry_price must include the slippage band on the BUY leg,
    matching Backtester.run's ``fill_price * (1 + slippage_pct)``.
    Otherwise the carried-over entry price is too low and the seeded
    fold's P&L is systematically inflated."""
    df = _warmup_train_df()
    seed = warmup_exit_long_entry(df.iloc[:30], slippage_pct=0.001)
    assert seed is not None
    # BUY on bar 5 (raw), shifted to bar 6 (fills at bar 6 open = 100.0),
    # slippage adds 0.1% → 100.1.
    assert seed["entry_price"] == pytest.approx(100.1)


def test_warmup_exit_long_entry_ignores_repeat_buy_while_long():
    """Matches Backtester semantics: a BUY while already long is dropped.
    Regression guard: the scan must not overwrite the first entry price
    with a later BUY, otherwise the carried P&L anchors to the wrong
    price."""
    opens = [100.0, 100.0, 100.0, 100.0, 100.0, 150.0, 150.0, 150.0]
    closes = opens[:]
    signals = [1, 0, 0, 1, 0, 0, 0, 0]  # BUY at bar 0 and bar 3
    idx = pd.date_range("2024-01-01", periods=8, freq="D")
    df = pd.DataFrame(
        {"open": opens, "close": closes, "signal": signals}, index=idx,
    )
    seed = warmup_exit_long_entry(df, slippage_pct=0.0)
    assert seed is not None
    # First BUY at bar 0 shifts to fill at bar 1 open = 100.0. Second BUY
    # at bar 3 must be ignored (already long).
    assert seed["entry_price"] == pytest.approx(100.0)


def test_boundary_bar_signal_fires_on_first_train_bar():
    """Regression for the last-warmup-bar gap. A SELL on the final warmup
    bar (bar 29) must fill on the first train bar's open (bar 30 — a
    $200 gap-up). Previously both the scan shift and the Backtester shift
    dropped this signal, so the warmup BUY was never closed and force-
    closed at the train's final bar instead."""
    opens = [100.0] * 30 + [200.0] * 30
    closes = opens[:]
    signals = [0] * 60
    signals[5] = 1    # BUY in warmup
    signals[29] = -1  # SELL on LAST warmup bar
    idx = pd.date_range("2024-01-01", periods=60, freq="D")
    df = pd.DataFrame(
        {"open": opens, "close": closes, "signal": signals}, index=idx,
    )

    # Optimizer's new slicing: scan warmup strictly before the boundary
    # bar, pass the boundary bar (+ train) to Backtester.
    boundary = 29
    warmup_scan = df.iloc[:boundary]
    backtester_df = df.iloc[boundary:]

    seed = warmup_exit_long_entry(warmup_scan, slippage_pct=0.0)
    assert seed is not None, "warmup BUY on bar 5 must carry forward"
    assert seed["entry_price"] == pytest.approx(100.0)

    bt = Backtester(initial_capital=1000.0, commission_pct=0.0, slippage_pct=0.0)
    result = bt.run(backtester_df, save=False, starting_long=seed)

    # SELL at bar 29 shifts into Backtester's row 1 (bar 30) and fills at
    # bar 30's open = 200. Warmup entry was 100, so 2× return.
    assert result["total_trades"] == 1
    trade = result["trades"][0]
    assert trade["entry_price"] == pytest.approx(100.0)
    assert trade["exit_price"] == pytest.approx(200.0)
    # Anchor check: total_return_pct should be ~100% (doubled capital),
    # not a shifted-baseline number.
    assert result["total_return_pct"] == pytest.approx(100.0, abs=0.1)


def test_seeded_metrics_anchor_at_initial_capital():
    """Seeded run's equity[0] is mark-to-market, not initial_capital.
    _calculate_metrics must anchor total_return_pct and max_drawdown_pct
    at self.initial_capital so the baseline matches unseeded runs."""
    # 10 flat-price bars, no signals. Seed says "already long at $100".
    # With flat $100 prices: shares = 1000/100 = 10. equity each bar = 10
    # * 100 = 1000 = initial_capital. Final position force-closed at 1000.
    # total_return should be 0, drawdown 0.
    opens = closes = [100.0] * 10
    signals = [0] * 10
    idx = pd.date_range("2024-01-01", periods=10, freq="D")
    df = pd.DataFrame(
        {"open": opens, "close": closes, "signal": signals}, index=idx,
    )
    bt = Backtester(initial_capital=1000.0, commission_pct=0.0, slippage_pct=0.0)
    result = bt.run(
        df, save=False,
        starting_long={"entry_price": 100.0, "entry_date": idx[0]},
    )
    assert result["total_return_pct"] == pytest.approx(0.0, abs=0.01)
    assert result["max_drawdown_pct"] == pytest.approx(0.0, abs=0.01)


def test_warmup_seed_stamps_entry_atr_and_high_water():
    """PR #1000 review: the seed must carry the fill-bar ATR and the max
    close since entry so ATR-based close stacks and trailing stops manage
    the carried position like a mid-window open."""
    n = 30
    opens = [100.0] * n
    closes = [100.0] * 10 + [104.0, 108.0] + [103.0] * (n - 12)
    signals = [0] * n
    signals[5] = 1  # BUY → fills at bar 6 open
    idx = pd.date_range("2024-01-01", periods=n, freq="D")
    df = pd.DataFrame({
        "open": opens, "close": closes, "signal": signals,
        "atr": [2.5] * n,
    }, index=idx)
    seed = warmup_exit_long_entry(df, slippage_pct=0.0)
    assert seed is not None
    assert seed["entry_atr"] == pytest.approx(2.5)
    assert seed["high_water"] == pytest.approx(108.0)


def test_warmup_seed_omits_implausible_entry_atr():
    """Mirrors Backtester._stamp_entry_atr: an ATR above 50% of the entry
    price (or NaN/non-positive) is rejected — the seed simply omits the
    key and the carried position degrades to the pre-stamp behavior."""
    n = 20
    signals = [0] * n
    signals[5] = 1
    idx = pd.date_range("2024-01-01", periods=n, freq="D")
    df = pd.DataFrame({
        "open": [100.0] * n, "close": [100.0] * n, "signal": signals,
        "atr": [80.0] * n,  # > 50% of the $100 entry
    }, index=idx)
    seed = warmup_exit_long_entry(df, slippage_pct=0.0)
    assert seed is not None
    assert "entry_atr" not in seed
    assert seed["high_water"] == pytest.approx(100.0)


def test_warmup_seed_high_water_resets_on_reentry():
    """A round trip inside the warmup must not leak its high-water mark
    into a later entry's seed."""
    n = 20
    closes = [100.0] * 5 + [150.0] * 5 + [100.0] * 10
    signals = [0] * n
    signals[2] = 1    # first BUY → fills bar 3
    signals[9] = -1   # SELL → fills bar 10 (HWM 150 ends here)
    signals[12] = 1   # re-entry → fills bar 13
    idx = pd.date_range("2024-01-01", periods=n, freq="D")
    df = pd.DataFrame({
        "open": closes[:], "close": closes, "signal": signals,
        "atr": [2.0] * n,
    }, index=idx)
    seed = warmup_exit_long_entry(df, slippage_pct=0.0)
    assert seed is not None
    assert seed["high_water"] == pytest.approx(100.0), (
        "the closed first trade's 150 HWM must not survive the re-entry")
