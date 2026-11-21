import json

import numpy as np
import pandas as pd
import pytest

from backtester import Backtester
from atr import ensure_atr_indicator
from optimizer import generate_close_stack_grid, walk_forward_optimize, warmup_exit_long_entry


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


def test_warmup_exit_long_entry_detects_unclosed_buy(subtests):
    with subtests.test("test_warmup_exit_long_entry_detects_unclosed_buy"):
        df = _warmup_train_df()
        seed = warmup_exit_long_entry(df.iloc[:30], slippage_pct=0.0)
        assert seed is not None, "warmup ends long — seed must be non-None"
        assert seed["entry_price"] == pytest.approx(100.0)
    with subtests.test("test_no_seed_when_fold_zero_has_no_warmup"):
        empty = pd.DataFrame(columns=["open", "close", "signal"])
        assert warmup_exit_long_entry(empty, slippage_pct=0.0) is None
    with subtests.test("test_warmup_exit_long_entry_applies_slippage"):
        df = _warmup_train_df()
        seed = warmup_exit_long_entry(df.iloc[:30], slippage_pct=0.001)
        assert seed is not None
        assert seed["entry_price"] == pytest.approx(100.1)
    with subtests.test("test_warmup_exit_long_entry_ignores_repeat_buy_while_long"):
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
    with subtests.test("test_warmup_exit_long_entry_returns_none_when_flat"):
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


_SHORT_CAPABLE_CONTRACT_STARTS_EVERY_SCORED_RUN_FLAT_CASES = [
    {"direction": "both"},
    {"direction": None},
    {"direction": " Both "},
    {"direction": "both", "close_stack_grid": SCRIPT_GRID_STACKS},
]
_SHORT_CAPABLE_CONTRACT_STARTS_EVERY_SCORED_RUN_FLAT_IDS = ["both", "omitted_fixed_close", "normalized_both", "both_grid"]


def test_short_capable_contract_starts_every_scored_run_flat(monkeypatch, subtests):
    for i, (case_id, kwargs) in enumerate(zip(_SHORT_CAPABLE_CONTRACT_STARTS_EVERY_SCORED_RUN_FLAT_IDS, _SHORT_CAPABLE_CONTRACT_STARTS_EVERY_SCORED_RUN_FLAT_CASES)):
        with subtests.test(case=case_id, kwargs=kwargs):
            with monkeypatch.context() as mp:
                _register_scripted(mp)
                by_phase = _scripted_walk_forward(mp, **kwargs)
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


_LONG_ONLY_CONTRACT_SEEDS_NOTHING_THE_ENGINE_CLOSED_IN_THE_PREFIX_CASES = [
    {"direction": "long"},
    {"direction": " LONG "},
    {"close_stack_grid": SCRIPT_GRID_STACKS},
]
_LONG_ONLY_CONTRACT_SEEDS_NOTHING_THE_ENGINE_CLOSED_IN_THE_PREFIX_IDS = ["explicit_long", "normalized_long", "grid_default_long"]


def test_long_only_contract_seeds_nothing_the_engine_closed_in_the_prefix(monkeypatch, subtests):
    for i, (case_id, kwargs) in enumerate(zip(_LONG_ONLY_CONTRACT_SEEDS_NOTHING_THE_ENGINE_CLOSED_IN_THE_PREFIX_IDS, _LONG_ONLY_CONTRACT_SEEDS_NOTHING_THE_ENGINE_CLOSED_IN_THE_PREFIX_CASES)):
        with subtests.test(case=case_id, kwargs=kwargs):
            with monkeypatch.context() as mp:
                _register_scripted(mp)
                by_phase = _scripted_walk_forward(mp, **kwargs)
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


_SMA_CROSSOVER_FOLLOWS_THE_SCORED_ENGINE_UNDER_A_FIXED_CLOSE_CASES = [(None, False), ("long", True)]
_SMA_CROSSOVER_FOLLOWS_THE_SCORED_ENGINE_UNDER_A_FIXED_CLOSE_IDS = ["omitted_fixed_close", "explicit_long"]


def test_sma_crossover_follows_the_scored_engine_under_a_fixed_close(monkeypatch, subtests):
    for i, (case_id, (direction, seeded)) in enumerate(zip(_SMA_CROSSOVER_FOLLOWS_THE_SCORED_ENGINE_UNDER_A_FIXED_CLOSE_IDS, _SMA_CROSSOVER_FOLLOWS_THE_SCORED_ENGINE_UNDER_A_FIXED_CLOSE_CASES)):
        with subtests.test(case=case_id, direction=direction, seeded=seeded):
            with monkeypatch.context() as mp:
                calls = _capture_runs(mp)
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
