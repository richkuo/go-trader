"""Close-stack co-optimization (#996): walk_forward_optimize sweeps complete
exit configurations (close-evaluator refs + the exclusive ATR stop owners)
jointly with the open-param grid, so selection picks (entry, exit) pairs.

Also regression-covers the pre-#996 silent no-op: the optimize path never
injected an `atr` column, so a tiered_tp_atr close ref threaded through
walk_forward_optimize ran with entry_atr=0 and never fired a TP.
"""
import json

import numpy as np
import pandas as pd
import pytest

from optimizer import (
    DEFAULT_CLOSE_STACK_SPECS,
    _result_metric,
    close_stack_label,
    generate_close_stack_grid,
    walk_forward_optimize,
)


def _trending_ohlc(n: int = 400, seed: int = 7) -> pd.DataFrame:
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


TIGHT_LADDER = [
    {"atr_multiple": 0.5, "close_fraction": 0.5},
    {"atr_multiple": 1.0, "close_fraction": 1.0},
]
WIDE_LADDER = [
    {"atr_multiple": 3.0, "close_fraction": 0.5},
    {"atr_multiple": 6.0, "close_fraction": 1.0},
]


# ---------------------------------------------------------------------------
# generate_close_stack_grid
# ---------------------------------------------------------------------------

def test_grid_expands_cartesian_per_spec():
    grid = generate_close_stack_grid([
        {"close": {"name": "tiered_tp_atr",
                   "params": {"tp_tiers": [TIGHT_LADDER, WIDE_LADDER]}},
         "stop_loss_atr_mult": [None, 2.0]},
    ])
    assert len(grid) == 4  # 2 ladders x 2 stop values
    # Ladders are swept as whole candidates, structure preserved.
    ladders = [g["close_strategies"][0]["params"]["tp_tiers"] for g in grid]
    assert TIGHT_LADDER in ladders and WIDE_LADDER in ladders
    sl_values = {g["stop_loss_atr_mult"] for g in grid}
    assert sl_values == {None, 2.0}
    assert all(g["label"] for g in grid)


def test_grid_baseline_spec_yields_empty_stack():
    grid = generate_close_stack_grid([{"close": None}])
    assert len(grid) == 1
    stack = grid[0]
    assert stack["close_strategies"] == []
    assert stack["stop_loss_atr_mult"] is None
    assert stack["trailing_stop_atr_mult"] is None
    assert stack["label"] == "baseline"


def test_grid_dedupes_identical_stacks_across_specs():
    grid = generate_close_stack_grid([
        {"close": None},
        {"close": None, "stop_loss_atr_mult": [None, 1.5]},
    ])
    # The (no close, no stop) combo appears in both specs — kept once.
    assert len(grid) == 2


def test_grid_rejects_both_stop_owners():
    with pytest.raises(ValueError, match="mutually exclusive"):
        generate_close_stack_grid([
            {"close": None,
             "stop_loss_atr_mult": [1.5],
             "trailing_stop_atr_mult": [2.0]},
        ])


def test_grid_rejects_unknown_spec_keys():
    with pytest.raises(ValueError, match="unknown keys"):
        generate_close_stack_grid([{"close": None, "tp_tiers": [TIGHT_LADDER]}])


def test_grid_rejects_non_list_param_candidates():
    with pytest.raises(ValueError, match="non-empty list"):
        generate_close_stack_grid([
            {"close": {"name": "tiered_tp_atr",
                       "params": {"tp_tiers": TIGHT_LADDER}}},  # ladder, not list-of-ladders
        ])


def test_grid_rejects_close_without_name():
    with pytest.raises(ValueError, match="needs a 'name'"):
        generate_close_stack_grid([{"close": {"params": {}}}])


def test_default_specs_expand_and_are_exclusive():
    grid = generate_close_stack_grid(DEFAULT_CLOSE_STACK_SPECS)
    assert len(grid) > 10
    labels = [g["label"] for g in grid]
    assert len(labels) == len(set(labels)), "duplicate stack labels"
    assert "baseline" in labels
    for g in grid:
        assert not (g["stop_loss_atr_mult"] and g["trailing_stop_atr_mult"])


def test_close_stack_label_is_compact():
    grid = generate_close_stack_grid([
        {"close": {"name": "tiered_tp_atr",
                   "params": {"tp_tiers": [TIGHT_LADDER]}},
         "stop_loss_atr_mult": [2.0]},
    ])
    assert grid[0]["label"] == "tiered_tp_atr[0.5x:0.5,1x:1] sl_atr=2"


# ---------------------------------------------------------------------------
# _result_metric
# ---------------------------------------------------------------------------

def test_result_metric_reads_plain_keys():
    assert _result_metric({"sharpe_ratio": 1.5}, "sharpe_ratio") == 1.5


def test_result_metric_dd_adjusted_return():
    assert _result_metric(
        {"total_return_pct": 30.0, "max_drawdown_pct": -15.0},
        "dd_adjusted_return") == pytest.approx(2.0)
    # Zero-DD legs score 0.0 (eval_windows.dd_adjusted_return parity) so an
    # untraded combo never outranks a traded one.
    assert _result_metric(
        {"total_return_pct": 10.0, "max_drawdown_pct": 0.0},
        "dd_adjusted_return") == 0.0


# ---------------------------------------------------------------------------
# walk_forward_optimize joint sweep
# ---------------------------------------------------------------------------

def test_joint_sweep_reports_best_close_stack():
    df = _trending_ohlc()
    grid = generate_close_stack_grid([
        {"close": None},
        {"close": {"name": "tiered_tp_atr",
                   "params": {"tp_tiers": [TIGHT_LADDER]}}},
    ])
    result = walk_forward_optimize(
        df, "sma_crossover", {"fast_period": [10], "slow_period": [40]},
        n_splits=2, train_pct=0.7, initial_capital=1000.0, verbose=False,
        close_stack_grid=grid,
    )
    assert "window_results" in result, result
    assert result["close_stack_grid_size"] == 2
    labels = {g["label"] for g in grid}
    for w in result["window_results"]:
        assert w["best_close_stack"] in labels
        assert set(w["best_close_stack_spec"]) == {
            "close_strategies", "stop_loss_atr_mult", "trailing_stop_atr_mult"}
    assert result["most_common_best_close_stack"] in labels


def test_joint_sweep_rejects_fixed_close_kwargs():
    df = _trending_ohlc()
    grid = generate_close_stack_grid([{"close": None}])
    with pytest.raises(ValueError, match="mutually exclusive"):
        walk_forward_optimize(
            df, "sma_crossover", {"fast_period": [10], "slow_period": [40]},
            n_splits=2, verbose=False,
            close_stack_grid=grid,
            close_strategies=[{"name": "tp_at_pct", "params": {"pct": 0.03}}],
        )


def test_joint_sweep_rejects_invalid_stack_loudly():
    """Invalid stacks must fail at Backtester construction, not be silently
    skipped inside the fold loop (trailing_tp_ratchet needs a trailing mult)."""
    df = _trending_ohlc()
    grid = [{
        "label": "bad ratchet",
        "close_strategies": [{"name": "trailing_tp_ratchet", "params": {}}],
        "stop_loss_atr_mult": None,
        "trailing_stop_atr_mult": None,
    }]
    with pytest.raises(ValueError, match="trailing_tp_ratchet"):
        walk_forward_optimize(
            df, "sma_crossover", {"fast_period": [10], "slow_period": [40]},
            n_splits=2, verbose=False, close_stack_grid=grid,
        )


def test_tiered_tp_atr_actually_fires_in_optimize_path():
    """Regression (#996): the optimize path must inject `atr` so tiered_tp_atr
    stamps entry_atr and fires TPs. Pre-fix, the evaluator silently no-oped and
    a tight-TP stack scored identically to the baseline."""
    df = _trending_ohlc(seed=11)
    grid = generate_close_stack_grid([
        {"close": None},
        {"close": {"name": "tiered_tp_atr",
                   "params": {"tp_tiers": [TIGHT_LADDER]}}},
    ])
    param_ranges = {"fast_period": [10], "slow_period": [40]}

    def _oos(close_stack_grid):
        r = walk_forward_optimize(
            df, "sma_crossover", param_ranges,
            n_splits=2, train_pct=0.7, initial_capital=1000.0, verbose=False,
            close_stack_grid=close_stack_grid,
        )
        assert "window_results" in r, r
        return r

    baseline = _oos([grid[0]])
    tight_tp = _oos([grid[1]])
    base_trades = sum(w["test_result"]["total_trades"]
                      for w in baseline["window_results"])
    tp_trades = sum(w["test_result"]["total_trades"]
                    for w in tight_tp["window_results"])
    # A 0.5-ATR first tier on daily bars with ~1.5% vol fires almost
    # immediately — the tight-TP stack must produce a different (richer)
    # trade record than open-signal-as-close. Identical records mean the
    # evaluator never saw an ATR column (the pre-fix silent no-op).
    assert tp_trades > base_trades, (
        f"tiered_tp_atr produced the same trade count as baseline "
        f"({tp_trades} vs {base_trades}) — entry_atr was never stamped")


def test_fixed_close_path_unchanged_and_atr_injected():
    """The pre-existing fixed close_strategies kwarg still works, now with ATR
    injection so the evaluator actually fires."""
    df = _trending_ohlc(seed=11)
    result = walk_forward_optimize(
        df, "sma_crossover", {"fast_period": [10], "slow_period": [40]},
        n_splits=2, train_pct=0.7, initial_capital=1000.0, verbose=False,
        close_strategies=[{"name": "tiered_tp_atr",
                           "params": {"tp_tiers": TIGHT_LADDER}}],
    )
    assert "window_results" in result, result
    assert result["close_stack_grid_size"] == 1
    total_trades = sum(w["test_result"]["total_trades"]
                       for w in result["window_results"])
    assert total_trades > 0


def test_dd_adjusted_metric_selects_and_reports():
    df = _trending_ohlc()
    grid = generate_close_stack_grid([
        {"close": None, "stop_loss_atr_mult": [None, 2.0]},
    ])
    result = walk_forward_optimize(
        df, "sma_crossover", {"fast_period": [10], "slow_period": [40]},
        n_splits=2, train_pct=0.7, initial_capital=1000.0, verbose=False,
        close_stack_grid=grid, optimize_metric="dd_adjusted_return",
    )
    assert "window_results" in result, result
    assert result["optimize_metric"] == "dd_adjusted_return"
    for w in result["window_results"]:
        assert np.isfinite(w["train_metric"])


def test_stack_specs_survive_json_round_trip():
    """--close-stacks-json feeds specs through json.load — the expansion must
    treat null as None for stop values."""
    specs = json.loads(json.dumps([
        {"close": {"name": "tiered_tp_atr",
                   "params": {"tp_tiers": [TIGHT_LADDER]}},
         "stop_loss_atr_mult": [None, 1.5]},
    ]))
    grid = generate_close_stack_grid(specs)
    assert len(grid) == 2
    assert {g["stop_loss_atr_mult"] for g in grid} == {None, 1.5}


# ---------------------------------------------------------------------------
# Entry-universe consistency: close refs activate the open/close engine where
# raw signal=-1 OPENS a short, while the no-close baseline stack runs the
# structurally long/flat plain path. The sweep must hold the entry side
# constant (default: long) or the stacks score different universes.
# ---------------------------------------------------------------------------

def test_joint_sweep_defaults_to_long_universe():
    df = _trending_ohlc(seed=11)
    grid = generate_close_stack_grid([
        {"close": {"name": "tiered_tp_atr",
                   "params": {"tp_tiers": [WIDE_LADDER]}}},
    ])
    result = walk_forward_optimize(
        df, "sma_crossover", {"fast_period": [10], "slow_period": [40]},
        n_splits=2, train_pct=0.7, initial_capital=1000.0, verbose=False,
        close_stack_grid=grid,
    )
    assert "window_results" in result, result
    sides = {t["side"] for w in result["window_results"]
             for t in w["test_result"]["trades"]}
    assert sides <= {"long"}, (
        f"close-stack sweep opened {sides} — sma_crossover's sell signals "
        f"must not open shorts under the default long entry universe")


def test_joint_sweep_rejects_short_universe_with_no_close_stack():
    df = _trending_ohlc()
    grid = generate_close_stack_grid([{"close": None}])
    with pytest.raises(ValueError, match="close evaluator"):
        walk_forward_optimize(
            df, "sma_crossover", {"fast_period": [10], "slow_period": [40]},
            n_splits=2, verbose=False, close_stack_grid=grid,
            direction="both",
        )


def test_optimize_rejects_short_direction_with_no_close_ref():
    # PR #1004 review: direction=short is rejected wholesale — the warmup
    # seeder (warmup_exit_long_entry) is long-only, so walk-forward cannot
    # measure the short leg faithfully. The error must state that true
    # reason, not the stale "plain path cannot open shorts" claim (#989
    # made the plain path open shorts under direction=short).
    df = _trending_ohlc()
    with pytest.raises(ValueError, match="warmup seeder"):
        walk_forward_optimize(
            df, "sma_crossover", {"fast_period": [10], "slow_period": [40]},
            n_splits=2, verbose=False, direction="short",
        )


def test_optimize_rejects_short_direction_even_with_close_refs():
    # PR #1004 review must-survive: close-evaluator-only stacks pass the
    # "both" stack guard, but the long-only warmup seeder would still carry
    # a phantom long into a short engine run — short is rejected regardless
    # of the close stack.
    df = _trending_ohlc()
    with pytest.raises(ValueError, match="warmup seeder"):
        walk_forward_optimize(
            df, "sma_crossover", {"fast_period": [10], "slow_period": [40]},
            n_splits=2, verbose=False, direction="short",
            close_strategies=[{"name": "tiered_tp_atr",
                               "params": {"tp_tiers": WIDE_LADDER}}],
        )
    grid = generate_close_stack_grid([
        {"close": {"name": "tiered_tp_atr",
                   "params": {"tp_tiers": [WIDE_LADDER]}}},
    ])
    with pytest.raises(ValueError, match="warmup seeder"):
        walk_forward_optimize(
            df, "sma_crossover", {"fast_period": [10], "slow_period": [40]},
            n_splits=2, verbose=False, direction="short",
            close_stack_grid=grid,
        )


def test_optimize_both_direction_with_fixed_close_ref_runs():
    # Inverse case: a fixed --close-strategy activates the open/close engine,
    # which legitimately opens both sides — the guard must not over-reject.
    df = _trending_ohlc()
    result = walk_forward_optimize(
        df, "sma_crossover", {"fast_period": [10], "slow_period": [40]},
        n_splits=2, verbose=False, direction="both",
        close_strategies=[{"name": "tiered_tp_atr",
                           "params": {"tp_tiers": WIDE_LADDER}}],
    )
    assert "window_results" in result, result


def test_optimize_long_direction_with_no_close_ref_runs():
    # Boundary: explicit long with no close ref is the structurally-valid
    # default and must keep running long/flat unchanged.
    df = _trending_ohlc()
    result = walk_forward_optimize(
        df, "sma_crossover", {"fast_period": [10], "slow_period": [40]},
        n_splits=2, verbose=False, direction="long",
    )
    sides = {t["side"] for w in result["window_results"]
             for t in w["test_result"]["trades"]}
    assert sides <= {"long"}, sides


def test_result_metric_dd_adjusted_return_floors_liquidated():
    # #1228: a blown-up combo's raw DDadj (−100/|−100| = −1.0) would outrank a
    # surviving loser (−60%/30% DD = −2.0); the floor keeps dead combos last.
    blown = _result_metric(
        {"total_return_pct": -100.0, "max_drawdown_pct": -100.0,
         "liquidated": True},
        "dd_adjusted_return")
    survivor = _result_metric(
        {"total_return_pct": -60.0, "max_drawdown_pct": -30.0,
         "liquidated": False},
        "dd_adjusted_return")
    assert blown == -100.0
    assert survivor > blown
