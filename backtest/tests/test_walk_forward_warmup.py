import json

import numpy as np
import pandas as pd
import pytest

from backtester import Backtester
from atr import ensure_atr_indicator
from optimizer import (
    generate_close_stack_grid,
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


@pytest.mark.parametrize(
    "ranges,expected",
    [
        ({"fast_period": [10, 15, 20],
          "slow_period": [40, 50, 80],
          "multiplier": [1.5, 2.0, 3.0]}, 80),
        ({"entry_std": [1.0, 1.5, 2.0],
          "exit_std": [0.0, 0.5, 1.0]}, 0),
    ],
    ids=["largest_int", "float_only_grid"],
)
def test_max_indicator_lookback(ranges, expected):
    assert max_indicator_lookback(ranges) == expected


def test_sma_80_grid_generates_trades_with_warmup():
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
    from registry_loader import load_registry

    df = _trending_ohlc(n=500)
    unprimed = df.iloc[100:200]
    primed_input = df.iloc[20:200]

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
    df = _trending_ohlc(n=600)
    result = walk_forward_optimize(
        df, "sma_crossover", {"fast_period": [10], "slow_period": [80]},
        n_splits=5, train_pct=0.7,
        initial_capital=1000.0, verbose=False,
    )
    assert result["n_valid_folds"] >= 2, result


def _warmup_train_df() -> pd.DataFrame:
    opens  = [100.0] * 60
    closes = [100.0] * 60
    signals = [0] * 60
    signals[5]  = 1
    signals[45] = -1
    idx = pd.date_range("2024-01-01", periods=60, freq="D")
    return pd.DataFrame(
        {"open": opens, "close": closes, "signal": signals}, index=idx,
    )


def test_warmup_exit_long_entry_detects_unclosed_buy():
    df = _warmup_train_df()
    seed = warmup_exit_long_entry(df.iloc[:30], slippage_pct=0.0)
    assert seed is not None, "warmup ends long — seed must be non-None"
    assert seed["entry_price"] == pytest.approx(100.0)


def test_warmup_exit_long_entry_returns_none_when_flat():
    df = _warmup_train_df()
    seed = warmup_exit_long_entry(df.iloc[:47], slippage_pct=0.0)
    assert seed is None


def test_train_fold_captures_trade_spanning_warmup_boundary():
    df = _warmup_train_df()
    train_signals = df.iloc[30:]
    warmup_signals = df.iloc[:30]

    bt_unseeded = Backtester(
        initial_capital=1000.0, commission_pct=0.0, slippage_pct=0.0,
    )
    unseeded = bt_unseeded.run(train_signals, save=False)
    assert unseeded["total_trades"] == 0, (
        "Pre-seed counterfactual: SELL on a flat position is ignored. "
        "If this changes, the seed mechanism's justification needs review."
    )

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
    empty = pd.DataFrame(columns=["open", "close", "signal"])
    assert warmup_exit_long_entry(empty, slippage_pct=0.0) is None


def test_warmup_exit_long_entry_applies_slippage():
    df = _warmup_train_df()
    seed = warmup_exit_long_entry(df.iloc[:30], slippage_pct=0.001)
    assert seed is not None
    assert seed["entry_price"] == pytest.approx(100.1)


def test_warmup_exit_long_entry_ignores_repeat_buy_while_long():
    opens = [100.0, 100.0, 100.0, 100.0, 100.0, 150.0, 150.0, 150.0]
    closes = opens[:]
    signals = [1, 0, 0, 1, 0, 0, 0, 0]
    idx = pd.date_range("2024-01-01", periods=8, freq="D")
    df = pd.DataFrame(
        {"open": opens, "close": closes, "signal": signals}, index=idx,
    )
    seed = warmup_exit_long_entry(df, slippage_pct=0.0)
    assert seed is not None
    assert seed["entry_price"] == pytest.approx(100.0)


def test_boundary_bar_signal_fires_on_first_train_bar():
    opens = [100.0] * 30 + [200.0] * 30
    closes = opens[:]
    signals = [0] * 60
    signals[5] = 1
    signals[29] = -1
    idx = pd.date_range("2024-01-01", periods=60, freq="D")
    df = pd.DataFrame(
        {"open": opens, "close": closes, "signal": signals}, index=idx,
    )

    boundary = 29
    warmup_scan = df.iloc[:boundary]
    backtester_df = df.iloc[boundary:]

    seed = warmup_exit_long_entry(warmup_scan, slippage_pct=0.0)
    assert seed is not None, "warmup BUY on bar 5 must carry forward"
    assert seed["entry_price"] == pytest.approx(100.0)

    bt = Backtester(initial_capital=1000.0, commission_pct=0.0, slippage_pct=0.0)
    result = bt.run(backtester_df, save=False, starting_long=seed)

    assert result["total_trades"] == 1
    trade = result["trades"][0]
    assert trade["entry_price"] == pytest.approx(100.0)
    assert trade["exit_price"] == pytest.approx(200.0)
    assert result["total_return_pct"] == pytest.approx(100.0, abs=0.1)


def test_seeded_metrics_anchor_at_initial_capital():
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
    n = 30
    opens = [100.0] * n
    closes = [100.0] * 10 + [104.0, 108.0] + [103.0] * (n - 12)
    signals = [0] * n
    signals[5] = 1
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
    n = 20
    signals = [0] * n
    signals[5] = 1
    idx = pd.date_range("2024-01-01", periods=n, freq="D")
    df = pd.DataFrame({
        "open": [100.0] * n, "close": [100.0] * n, "signal": signals,
        "atr": [80.0] * n,
    }, index=idx)
    seed = warmup_exit_long_entry(df, slippage_pct=0.0)
    assert seed is not None
    assert "entry_atr" not in seed
    assert seed["high_water"] == pytest.approx(100.0)


def test_warmup_seed_high_water_resets_on_reentry():
    n = 20
    closes = [100.0] * 5 + [150.0] * 5 + [100.0] * 10
    signals = [0] * n
    signals[2] = 1
    signals[9] = -1
    signals[12] = 1
    idx = pd.date_range("2024-01-01", periods=n, freq="D")
    df = pd.DataFrame({
        "open": closes[:], "close": closes, "signal": signals,
        "atr": [2.0] * n,
    }, index=idx)
    seed = warmup_exit_long_entry(df, slippage_pct=0.0)
    assert seed is not None
    assert seed["high_water"] == pytest.approx(100.0), (
        "the closed first trade's 150 HWM must not survive the re-entry")


def test_walk_forward_still_seeds_a_long_only_strategy(monkeypatch):
    calls = _capture_runs(monkeypatch)
    walk_forward_optimize(
        _trending_ohlc(n=500), "sma_crossover", {"fast_period": [10, 20], "slow_period": [40, 80]},
        n_splits=5, train_pct=0.7, initial_capital=1000.0, verbose=False,
    )
    assert all(c["starting_long"] is None for c in calls)
    assert any(c["seed"] is not None for c in calls)


SCRIPTED = "wf_scripted_entry_1785"
SCRIPT_TP = {"name": "tiered_tp_atr", "params": {"tp_tiers": [
    {"atr_multiple": 2.0, "close_fraction": 0.5},
    {"atr_multiple": 4.0, "close_fraction": 1.0},
]}}
SCRIPT_STOP = 1.5
SCRIPT_LADDERS = {"tp_tiers": [SCRIPT_TP["params"]["tp_tiers"]]}
SCRIPT_GRID_STACKS = generate_close_stack_grid(
    [{"close": {"name": "tiered_tp_atr", "params": SCRIPT_LADDERS}, "stop_loss_atr_mult": [1.5, 2.0]}])
SCRIPT_GRID = {"lookback": [60]}
TP_PREFIX = (80, 139)
STOP_PREFIX = (140, 199)
FLIP_PREFIX = (280, 339)
SCORED_STARTS = {"train_0": 0, "test_0": 139, "train_1": 199, "test_1": 339}
PREFIX_OF = {"test_0": TP_PREFIX, "train_1": STOP_PREFIX, "test_1": FLIP_PREFIX}


def _scripted_frame() -> pd.DataFrame:
    n = 400
    levels = np.full(n, 100.0)
    levels[103:164] = 110.0
    levels[303:] = 110.0
    opens = np.concatenate([[100.0], levels[:-1]])
    signals = np.zeros(n, dtype=int)
    for bar in (100, 160, 300, 139, 199, 339):
        signals[bar] = 1
    signals[320] = -1
    idx = pd.date_range("2025-01-01", periods=n, freq="D")
    return pd.DataFrame({
        "open": opens, "high": np.maximum(opens, levels) + 1.0,
        "low": np.minimum(opens, levels) - 1.0, "close": levels,
        "volume": np.full(n, 1000.0), "script": signals,
        "funding_accrual": np.arange(n) * 1e-6, "funding_rate": np.arange(n) * 1e-6,
        "funding_missing_hours": np.zeros(n, dtype=int), "funding_mode": "charge",
    }, index=idx)


def _scripted_fn(df, lookback=60):
    out = df[["open", "high", "low", "close", "volume"]].copy()
    out["signal"] = df["script"].astype(int)
    out["regime"] = "trending_down"
    return out


def _register_scripted(monkeypatch, short_entries=True):
    from registry_loader import load_registry
    reg = load_registry("futures")
    canon = reg._registry
    canon.register(name=SCRIPTED, description="walk-forward seeding fixture (#1785)",
                   default_params={"lookback": 60}, platforms=("futures",),
                   short_entries=short_entries)(_scripted_fn)
    entry = canon.STRATEGIES.pop(SCRIPTED)
    monkeypatch.setitem(canon.STRATEGIES, SCRIPTED, entry)
    monkeypatch.setitem(canon.PLATFORM_ORDER, "futures",
                        list(canon.PLATFORM_ORDER["futures"]) + [SCRIPTED])
    projected = canon.build_registry("futures", include_hidden=True)[SCRIPTED]
    monkeypatch.setitem(reg.STRATEGY_REGISTRY, SCRIPTED, projected)
    return reg


def _capture_runs(monkeypatch):
    calls = []
    real_run = Backtester.run

    def run(self, signals, **kw):
        result = real_run(self, signals, **kw)
        handoff = result.get("warmup_handoff")
        calls.append({"start": signals.index[0], "engine": self,
                      "seed": handoff if handoff and handoff["side"] else None,
                      "handoff": handoff, "prefix": kw.get("warmup_prefix"),
                      "starting_long": kw.get("starting_long"),
                      "signals": signals, "result": result})
        return result

    monkeypatch.setattr(Backtester, "run", run)
    return calls


def _scripted_walk_forward(monkeypatch, **kw):
    calls = _capture_runs(monkeypatch)
    if not kw.get("close_stack_grid"):
        kw.setdefault("close_strategies", [SCRIPT_TP])
        kw["stop_loss_atr_mult"] = SCRIPT_STOP if kw["close_strategies"] else None
    result = walk_forward_optimize(
        _scripted_frame(), SCRIPTED, SCRIPT_GRID, n_splits=2, train_pct=0.7,
        timeframe="1d", registry="futures", platform="hyperliquid", verbose=False, **kw)
    assert result.get("n_valid_folds") == 2, result
    df = _scripted_frame()
    by_phase = {phase: [c for c in calls if c["start"] == df.index[bar]]
                for phase, bar in SCORED_STARTS.items()}
    assert all(by_phase.values()), {p: len(v) for p, v in by_phase.items()}
    return by_phase


def _prefix_signals(reg, prefix):
    df = _scripted_frame()
    ext = ensure_atr_indicator(reg.apply_strategy(SCRIPTED, df.iloc[prefix[0]:prefix[1] + 1],
                                                  {"lookback": 60}))
    return ext.iloc[:prefix[1] - prefix[0]]


def test_prefix_fixtures_hold_their_stated_engine_state(monkeypatch):
    reg = _register_scripted(monkeypatch)
    engine = dict(platform="hyperliquid", close_strategies=[SCRIPT_TP],
                  stop_loss_atr_mult=SCRIPT_STOP, direction="both")
    tp_prefix = _prefix_signals(reg, TP_PREFIX)
    tp_trades = Backtester(**engine).run(tp_prefix, save=False)["trades"]
    assert [(t["side"], t["exit_reason"]) for t in tp_trades] == [("long", "tiered_tp_atr:4")]
    stop_prefix = _prefix_signals(reg, STOP_PREFIX)
    stop_trades = Backtester(**engine).run(stop_prefix, save=False)["trades"]
    assert [(t["side"], t["exit_reason"]) for t in stop_trades] == [("long", "sl")]
    flip_prefix = _prefix_signals(reg, FLIP_PREFIX)
    nonzero = flip_prefix["signal"][flip_prefix["signal"] != 0].tolist()
    assert nonzero[-2:] == [1, -1]
    flip_trades = Backtester(**engine).run(flip_prefix, save=False)["trades"]
    assert [(t["side"], t["exit_reason"]) for t in flip_trades] == [
        ("long", "tiered_tp_atr:4"), ("short", "end_of_data")]
    assert warmup_exit_long_entry(tp_prefix, 0.0) is not None
    assert warmup_exit_long_entry(stop_prefix, 0.0) is not None
    assert warmup_exit_long_entry(flip_prefix, 0.0) is None
    long_engine = dict(engine, direction="long")
    for prefix, reason in ((tp_prefix, "tiered_tp_atr:4"), (stop_prefix, "sl")):
        trades = Backtester(**long_engine).run(prefix, save=False)["trades"]
        assert [(t["side"], t["exit_reason"]) for t in trades] == [("long", reason)]


def _seeded_phases(by_phase):
    return sorted(p for p, calls in by_phase.items() if any(c["seed"] is not None for c in calls))


def _replayed_phases(by_phase):
    return sorted(p for p, calls in by_phase.items() if any(c["prefix"] is not None for c in calls))


REPLAYED = ["test_0", "test_1", "train_1"]


@pytest.mark.parametrize("kwargs", [
    {"direction": "both"},
    {"direction": None},
    {"direction": " Both "},
    {"direction": "both", "close_stack_grid": SCRIPT_GRID_STACKS},
], ids=["both", "omitted_fixed_close", "normalized_both", "both_grid"])
def test_short_capable_contract_starts_every_scored_run_flat(monkeypatch, kwargs):
    _register_scripted(monkeypatch)
    by_phase = _scripted_walk_forward(monkeypatch, **kwargs)
    assert _seeded_phases(by_phase) == []
    assert _replayed_phases(by_phase) == []
    df = _scripted_frame()
    for phase, bar in SCORED_STARTS.items():
        for call in by_phase[phase]:
            scored = call["signals"]
            assert int(scored["signal"].iloc[0]) == (1 if phase != "train_0" else 0)
            np.testing.assert_array_equal(scored["funding_accrual"].to_numpy(),
                                          df["funding_accrual"].iloc[bar:len(scored) + bar].to_numpy())
            if phase != "train_0":
                first = call["result"]["trades"][0]
                assert first["entry_date"] == str(df.index[bar + 1])
                assert first["side"] == "long"


@pytest.mark.parametrize("kwargs", [
    {"direction": "long"},
    {"direction": " LONG "},
    {"close_stack_grid": SCRIPT_GRID_STACKS},
], ids=["explicit_long", "normalized_long", "grid_default_long"])
def test_long_only_contract_seeds_nothing_the_engine_closed_in_the_prefix(monkeypatch, kwargs):
    _register_scripted(monkeypatch)
    by_phase = _scripted_walk_forward(monkeypatch, **kwargs)
    assert _replayed_phases(by_phase) == REPLAYED
    assert _seeded_phases(by_phase) == []
    df = _scripted_frame()
    for phase, bar in SCORED_STARTS.items():
        for call in by_phase[phase]:
            assert call["starting_long"] is None
            if phase == "train_0":
                assert call["prefix"] is None
                continue
            assert call["handoff"]["side"] is None
            assert call["handoff"]["prefix_trades"] >= 1
            prefix = call["prefix"]
            assert prefix.index[-1] == df.index[bar - 1] and len(prefix) == 59
            np.testing.assert_array_equal(prefix["funding_accrual"].to_numpy(),
                                          df["funding_accrual"].loc[prefix.index].to_numpy())
            assert int(call["signals"]["signal"].iloc[0]) == 1
            assert call["result"]["trades"][0]["entry_date"] == str(df.index[bar + 1])


def test_plain_long_only_path_keeps_the_seed_it_still_holds(monkeypatch):
    reg = _register_scripted(monkeypatch)
    by_phase = _scripted_walk_forward(monkeypatch, close_strategies=None)
    assert _replayed_phases(by_phase) == REPLAYED
    assert _seeded_phases(by_phase) == ["test_0", "train_1"]
    df = _scripted_frame()
    for phase, entry_bar in (("test_0", 101), ("train_1", 161)):
        for call in by_phase[phase]:
            seed = call["seed"]
            assert seed["entry_date"] == str(df.index[entry_bar])
            assert seed["entry_price"] == pytest.approx(
                df["open"].iloc[entry_bar] * (1 + call["engine"].slippage_pct))
            assert seed["bars_held"] == SCORED_STARTS[phase] - entry_bar
            assert call["result"]["trades"][0]["entry_price"] == df["close"].iloc[SCORED_STARTS[phase] - 1]
    assert warmup_exit_long_entry(_prefix_signals(reg, TP_PREFIX), 0.0)["entry_date"] == df.index[101]


@pytest.mark.parametrize("kwargs", [
    {"direction": "both"},
    {"direction": None},
], ids=["both", "omitted_fixed_close"])
def test_short_entries_false_registration_starts_flat_when_the_engine_can_short(monkeypatch, kwargs):
    _register_scripted(monkeypatch, short_entries=False)
    by_phase = _scripted_walk_forward(monkeypatch, **kwargs)
    assert _seeded_phases(by_phase) == []


FAR_TIERS = [{"atr_multiple": 40.0, "close_fraction": 1.0}]
FAR_TP = {"name": "tiered_tp_atr", "params": {"tp_tiers": [FAR_TIERS]}}
FAR_CLOSE = [{"name": "tiered_tp_atr", "params": {"tp_tiers": FAR_TIERS}}]


def _stack_stop(call):
    return call["engine"].stop_loss_atr_mult


def test_long_grid_candidate_seeds_the_long_its_engine_still_holds(monkeypatch):
    reg = _register_scripted(monkeypatch)
    grid = generate_close_stack_grid([{"close": FAR_TP, "trailing_stop_atr_mult": [8.0]}])
    by_phase = _scripted_walk_forward(monkeypatch, close_stack_grid=grid)
    assert _seeded_phases(by_phase) == REPLAYED
    df = _scripted_frame()
    for phase, prefix_span in sorted(PREFIX_OF.items()):
        (call,) = by_phase[phase]
        engine = Backtester(platform="hyperliquid", direction="long",
                            close_strategies=FAR_CLOSE, trailing_stop_atr_mult=8.0)
        held = engine.run(_prefix_signals(reg, prefix_span), save=False)["trades"][-1]
        seed = call["seed"]
        assert held["exit_reason"] == "end_of_data"
        assert seed["entry_date"] == held["entry_date"]
        assert seed["entry_atr"] == pytest.approx(held["entry_atr"], abs=1e-6)
        assert seed["entry_price"] == pytest.approx(held["entry_price"])
        boundary = df.index[SCORED_STARTS[phase] - 1]
        assert seed["high_water"] == pytest.approx(max(seed["entry_price"],
                                                       df.loc[seed["entry_date"]:boundary, "close"].max()))
        assert 0 < seed["stop_trigger"] < seed["high_water"] and seed["stop_armed"]


def test_engine_entry_gates_decide_whether_a_prefix_long_exists(monkeypatch):
    reg = _register_scripted(monkeypatch)
    by_phase = _scripted_walk_forward(monkeypatch, close_strategies=None, regime_enabled=True,
                                      allowed_regimes=["trending_up"])
    assert _replayed_phases(by_phase) == REPLAYED
    assert _seeded_phases(by_phase) == []
    assert warmup_exit_long_entry(_prefix_signals(reg, TP_PREFIX), 0.0) is not None


def test_each_close_stack_replays_its_own_prefix_state(monkeypatch):
    _register_scripted(monkeypatch)
    specs = [{"close": FAR_TP, "stop_loss_atr_mult": [1.5, 30.0]}]
    runs = []
    for stacks in (generate_close_stack_grid(specs), list(reversed(generate_close_stack_grid(specs)))):
        by_phase = _scripted_walk_forward(monkeypatch, close_stack_grid=stacks)
        monkeypatch.undo()
        _register_scripted(monkeypatch)
        train = {_stack_stop(c): c["handoff"] for c in by_phase["train_1"]}
        assert sorted(train) == [1.5, 30.0]
        assert train[1.5]["side"] is None and train[1.5]["prefix_trades"] == 1
        assert train[30.0]["side"] == "long"
        for phase in ("test_0", "test_1"):
            (call,) = by_phase[phase]
            replay = Backtester(platform="hyperliquid", direction="long",
                                close_strategies=FAR_CLOSE,
                                stop_loss_atr_mult=_stack_stop(call)).run(
                call["signals"], save=False, warmup_prefix=call["prefix"])
            assert replay["warmup_handoff"] == call["handoff"]
        runs.append({p: [(_stack_stop(c), c["handoff"], c["result"]["final_capital"]) for c in
                         sorted(calls, key=_stack_stop)] for p, calls in by_phase.items()})
    assert runs[0] == runs[1]


def test_a_failed_prefix_replay_skips_the_run_without_a_substitute_seed(monkeypatch):
    import optimizer
    _register_scripted(monkeypatch)
    real_run = Backtester.run
    scored = []

    def run(self, signals, **kw):
        if kw.get("warmup_prefix") is not None:
            raise ValueError("prefix replay failed")
        scored.append(kw.get("starting_long"))
        return real_run(self, signals, **kw)

    def no_seed(*_a, **_k):
        raise AssertionError("the signal-only seeder substituted for a failed replay")

    monkeypatch.setattr(Backtester, "run", run)
    monkeypatch.setattr(optimizer, "warmup_exit_long_entry", no_seed)
    result = walk_forward_optimize(_scripted_frame(), SCRIPTED, SCRIPT_GRID, n_splits=2, train_pct=0.7,
                                   timeframe="1d", registry="futures", platform="hyperliquid",
                                   verbose=False, close_strategies=None)
    assert result["n_valid_folds"] == 0 and result["funding_skipped_folds"] == 0
    assert scored == [None]


def test_unpriced_prefix_funding_rejects_the_candidate_and_the_fold(monkeypatch):
    _register_scripted(monkeypatch)
    real_run = Backtester.run
    results = []

    block = json.dumps({"mode": "charge", "available": True, "complete": False, "coverage": None,
                        "source": "cache", "sha256": None, "row_count": 0, "first": None, "last": None})

    def run(self, signals, **kw):
        prefix = kw.get("warmup_prefix")
        if prefix is not None:
            kw["warmup_prefix"] = prefix.assign(funding_missing_hours=1, funding_block_json=block)
        result = real_run(self, signals.assign(funding_block_json=block), **kw)
        results.append(result)
        return result

    monkeypatch.setattr(Backtester, "run", run)
    result = walk_forward_optimize(_scripted_frame(), SCRIPTED, SCRIPT_GRID, n_splits=2, train_pct=0.7,
                                   timeframe="1d", registry="futures", platform="hyperliquid",
                                   verbose=False, close_strategies=None)
    assert result["error"] == "No valid optimization windows: incomplete funding"
    assert result["funding_skipped_folds"] == 2 and result["funding_skipped_candidates"] == 1
    rejected = [r for r in results if r.get("warmup_handoff")]
    assert len(rejected) == 2
    for r in rejected:
        assert r["funding_unpriced_held_hours"] == 0
        assert r["warmup_handoff"]["funding_unpriced_held_hours"] > 0


def test_entry_only_strategies_stay_unseeded_on_a_long_only_engine(monkeypatch):
    import optimizer
    assert optimizer.UNSEEDED_ENTRY_ONLY_STRATEGIES == {"calendar_conditioned_entry", "elder_force_pullback"}
    _register_scripted(monkeypatch)
    monkeypatch.setattr(optimizer, "UNSEEDED_ENTRY_ONLY_STRATEGIES", frozenset({SCRIPTED}))
    by_phase = _scripted_walk_forward(monkeypatch, close_strategies=None)
    assert _replayed_phases(by_phase) == []
    assert all(c["starting_long"] is None for calls in by_phase.values() for c in calls)


def test_short_entries_false_registration_replays_the_prefix_when_long_only(monkeypatch):
    _register_scripted(monkeypatch, short_entries=False)
    by_phase = _scripted_walk_forward(monkeypatch, direction="long")
    assert _replayed_phases(by_phase) == REPLAYED
    assert _seeded_phases(by_phase) == []


@pytest.mark.parametrize("direction,seeded", [(None, False), ("long", True)],
                         ids=["omitted_fixed_close", "explicit_long"])
def test_sma_crossover_follows_the_scored_engine_under_a_fixed_close(monkeypatch, direction, seeded):
    calls = _capture_runs(monkeypatch)
    walk_forward_optimize(
        _trending_ohlc(n=500), "sma_crossover", {"fast_period": [10, 20], "slow_period": [40, 80]},
        n_splits=5, train_pct=0.7, initial_capital=1000.0, timeframe="1d", registry="futures",
        platform="hyperliquid", verbose=False, close_strategies=[SCRIPT_TP],
        stop_loss_atr_mult=SCRIPT_STOP, direction=direction,
    )
    assert calls
    sides = {t["side"] for c in calls for t in c["result"]["trades"]}
    if seeded:
        assert any(c["prefix"] is not None for c in calls)
        assert sides == {"long"}
    else:
        assert all(c["prefix"] is None and c["seed"] is None for c in calls)
        assert "short" in sides


def _policy_engine(monkeypatch, policy, **cert):
    import optimizer

    class PolicyEngine(Backtester):
        def __init__(self, *args, **kw):
            super().__init__(*args, regime_directional_policy={"trend_regime": policy}, **cert, **kw)

    monkeypatch.setattr(optimizer, "Backtester", PolicyEngine)


@pytest.mark.parametrize("policy,cert,kwargs,replayed,seeded", [
    ({"trending_down": {"direction": "short"}}, {"regime_directional_certified": True},
     {"close_strategies": None}, [], []),
    ({"trending_down": {"direction": "both"}}, {"regime_directional_certified": True},
     {"direction": "long"}, [], []),
    ({"trending_down": {"direction": "short"}},
     {"regime_directional_certified_states": {"trending_down": "short"}},
     {"direction": "long"}, [], []),
    ({"trending_down": {"direction": "short"}}, {"regime_directional_certified": False},
     {"close_strategies": None}, REPLAYED, ["test_0", "train_1"]),
    ({"trending_down": {"direction": "long"}}, {"regime_directional_certified": True},
     {"direction": "long"}, REPLAYED, []),
], ids=["plain_short_override", "long_base_both_override", "state_certified_short",
        "uncertified_dropped", "long_override"])
def test_certified_regime_policy_decides_the_seed(monkeypatch, policy, cert, kwargs, replayed, seeded):
    _register_scripted(monkeypatch)
    _policy_engine(monkeypatch, policy, **cert)
    by_phase = _scripted_walk_forward(monkeypatch, regime_enabled=True, **kwargs)
    assert _replayed_phases(by_phase) == replayed
    assert _seeded_phases(by_phase) == seeded


@pytest.mark.parametrize("kwargs", [
    {"direction": " Short "},
    {"direction": " BOTH ", "close_strategies": None},
    {"direction": "Both", "close_stack_grid": generate_close_stack_grid(
        [{"close": None}, {"close": {"name": "tiered_tp_atr", "params": SCRIPT_LADDERS}}])},
], ids=["short_only", "both_without_close", "both_grid_with_baseline"])
def test_normalized_unsupported_directions_refuse_before_scoring(monkeypatch, kwargs):
    _register_scripted(monkeypatch)
    calls = _capture_runs(monkeypatch)
    kw = {"close_strategies": [SCRIPT_TP], **kwargs}
    if kw.get("close_stack_grid") or kw["close_strategies"] is None:
        kw.pop("close_strategies")
    with pytest.raises(ValueError, match="direction='(short|both)'"):
        walk_forward_optimize(_scripted_frame(), SCRIPTED, SCRIPT_GRID, n_splits=2,
                              registry="futures", platform="hyperliquid", verbose=False, **kw)
    assert calls == []


def _random_walk(timeframe="1h", n=1200, seed=3):
    from closed_bar import declare_frame_cadence
    rng = np.random.default_rng(seed)
    close = 100 * np.exp(np.cumsum(rng.normal(0.0, 0.02, n)))
    open_ = np.concatenate([[100.0], close[:-1]])
    df = pd.DataFrame({
        "open": open_, "high": np.maximum(open_, close) * (1 + rng.uniform(0.001, 0.01, n)),
        "low": np.minimum(open_, close) * (1 - rng.uniform(0.001, 0.01, n)), "close": close,
        "volume": rng.uniform(100, 1000, n),
    }, index=pd.date_range("2026-01-01", periods=n, freq={"1h": "1h", "5m": "5min"}[timeframe]))
    return declare_frame_cadence(df, timeframe, "open")


def _singleton_grid(reg, name):
    from optimizer import DEFAULT_PARAM_RANGES
    ranges = DEFAULT_PARAM_RANGES[name]
    if not ranges:
        return {k: [v] for k, v in reg.STRATEGY_REGISTRY[name]["default_params"].items()}
    return {k: [v[0]] for k, v in ranges.items()}


def _singleton_grid_values(reg, name):
    return {k: v[0] for k, v in _singleton_grid(reg, name).items()}


def test_every_registered_short_entry_strategy_follows_the_engine_contract(monkeypatch):
    from optimizer import DEFAULT_PARAM_RANGES
    from registry_loader import load_registry
    reg = load_registry("futures")
    from run_backtest import LEADER_INPUT_STRATEGIES, TRADE_FLOW_INPUT_STRATEGIES
    names = sorted(n for n in reg._registry.short_entry_strategies() if n in reg.STRATEGY_REGISTRY)
    assert set(names) <= set(DEFAULT_PARAM_RANGES)
    for name in sorted(LEADER_INPUT_STRATEGIES):
        with pytest.raises(ValueError, match="pinned BTC leader series"):
            walk_forward_optimize(_random_walk("1h"), name, _singleton_grid(reg, name), n_splits=2, timeframe="1h",
                                  registry="futures", platform="hyperliquid", verbose=False,
                                  close_strategies=[SCRIPT_TP], stop_loss_atr_mult=SCRIPT_STOP, direction="both")
    names = [n for n in names if n not in LEADER_INPUT_STRATEGIES]
    from run_backtest import VOLUME_PROFILE_INPUT_STRATEGIES
    for name in sorted(VOLUME_PROFILE_INPUT_STRATEGIES):
        with pytest.raises(ValueError, match="executed-trade volume profiles"):
            walk_forward_optimize(_random_walk("1h"), name, _singleton_grid(reg, name), n_splits=2, timeframe="1h",
                                  registry="futures", platform="hyperliquid", verbose=False,
                                  close_strategies=[SCRIPT_TP], stop_loss_atr_mult=SCRIPT_STOP, direction="both")
    names = [n for n in names if n not in VOLUME_PROFILE_INPUT_STRATEGIES]
    for name in sorted(TRADE_FLOW_INPUT_STRATEGIES):
        with pytest.raises(ValueError, match="cannot fit one"):
            reg.validate_params(name, {**_singleton_grid_values(reg, name), "expected_interval_ms": 3_600_000})
    assert DEFAULT_PARAM_RANGES["camarilla_pivot_reversal"] == {}
    representatives = {"stochastic_momentum_index", "mass_index_reversal", "camarilla_pivot_reversal",
                       "trix_momentum", "elder_ray_pullback", "schaff_trend_cycle"}
    assert representatives <= set(names)
    frames = {"1h": _random_walk("1h"), "5m": _random_walk("5m")}
    seeded_long = set()
    shorts_seen = set()
    calls = _capture_runs(monkeypatch)
    for direction in ("both", "long"):
        for name in names:
            calls.clear()
            timeframe = "5m" if name in TRADE_FLOW_INPUT_STRATEGIES else "1h"
            walk_forward_optimize(frames[timeframe], name, _singleton_grid(reg, name), n_splits=2, timeframe=timeframe,
                                  registry="futures", platform="hyperliquid", verbose=False,
                                  close_strategies=[SCRIPT_TP], stop_loss_atr_mult=SCRIPT_STOP,
                                  direction=direction)
            assert calls, name
            if direction == "both":
                assert all(c["prefix"] is None and c["seed"] is None for c in calls), name
                if any((c["signals"]["signal"] == -1).any() for c in calls):
                    shorts_seen.add(name)
            elif any(c["seed"] is not None for c in calls):
                seeded_long.add(name)
    assert seeded_long
    assert representatives - {"mass_index_reversal"} <= shorts_seen
