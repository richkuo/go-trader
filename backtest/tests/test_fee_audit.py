"""Tests for backtest/fee_audit.py — the #999 M5 fee-aware selectivity screen.

Pure helpers (verdict matrix, trades/year guards, aggregation, ranking,
markdown) are tested without data access; one monkeypatched end-to-end leg
confirms the gross (zero-friction) re-run reuses eval_windows.run_leg, keeps an
identical trade count, and out-returns the net leg.
"""
import os
import sys
from types import SimpleNamespace

import numpy as np
import pandas as pd
import pytest

_BT_DIR = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
if _BT_DIR not in sys.path:
    sys.path.insert(0, _BT_DIR)

import eval_windows as ew  # noqa: E402
import fee_audit as fa  # noqa: E402


# ---------------------------------------------------------------------------
# salvage_verdict — the core triage matrix
# ---------------------------------------------------------------------------

def test_salvage_verdict_no_trades():
    assert fa.salvage_verdict(0, None, None) == "no_trades"
    # zero trades dominates even if a stray gross figure leaked in
    assert fa.salvage_verdict(0, 5.0, 5.0) == "no_trades"
    # mean_gross None (e.g. all gross legs errored) → unscored
    assert fa.salvage_verdict(10, None, -3.0) == "no_trades"


def test_salvage_verdict_deprecate_when_gross_nonpositive():
    # gross <= 0 → no edge to salvage, regardless of net
    assert fa.salvage_verdict(100, -2.0, -5.0) == "deprecate"
    assert fa.salvage_verdict(100, 0.0, -5.0) == "deprecate"  # boundary: gross == 0


def test_salvage_verdict_graduate_when_gross_positive_net_dead():
    assert fa.salvage_verdict(100, 8.0, -3.0) == "graduate_m1"
    assert fa.salvage_verdict(100, 8.0, 0.0) == "graduate_m1"  # boundary: net == 0
    assert fa.salvage_verdict(100, 8.0, None) == "graduate_m1"


def test_salvage_verdict_healthy_when_net_survives():
    assert fa.salvage_verdict(50, 12.0, 4.0) == "healthy"


def test_salvage_verdict_short_unmeasured_withholds_negative_calls():
    # A non-positive *measured* long leg cannot justify deprecate/no_trades
    # when short entries were dropped — the short half is simply unknown.
    assert fa.salvage_verdict(0, None, None, short_unmeasured=True) == "unscreened_short"
    assert fa.salvage_verdict(100, -2.0, -5.0, short_unmeasured=True) == "unscreened_short"
    assert fa.salvage_verdict(100, 0.0, -5.0, short_unmeasured=True) == "unscreened_short"
    # A positive long edge is a real finding even with the short half
    # unmeasured — graduate/healthy still stand (report flags them long-only).
    assert fa.salvage_verdict(100, 8.0, -3.0, short_unmeasured=True) == "graduate_m1"
    assert fa.salvage_verdict(50, 12.0, 4.0, short_unmeasured=True) == "healthy"


# ---------------------------------------------------------------------------
# trades_per_year — annualization + guards
# ---------------------------------------------------------------------------

def test_trades_per_year_basic():
    # 100 trades over 365.25 days → 100/yr
    assert fa.trades_per_year(100, 365.25) == pytest.approx(100.0)
    # 50 trades over half a year → ~100/yr
    assert fa.trades_per_year(50, 365.25 / 2) == pytest.approx(100.0)


def test_trades_per_year_zero_span_and_zero_trades_guarded():
    assert fa.trades_per_year(100, 0) is None
    assert fa.trades_per_year(100, None) is None
    assert fa.trades_per_year(100, -5) is None
    # zero trades over a real span is a real 0.0 rate, not None
    assert fa.trades_per_year(0, 365.25) == pytest.approx(0.0)


# ---------------------------------------------------------------------------
# aggregate_strategy — per-leg means, error exclusion, totals
# ---------------------------------------------------------------------------

def _leg(trades, span, net, gross, sharpe=1.0):
    return {"error": None, "trades": trades, "span_days": span,
            "net_ret": net, "gross_ret": gross, "net_sharpe": sharpe}


def test_aggregate_excludes_errors_and_sums_trades():
    legs = [
        _leg(800, 182.6, -40.0, 5.0),
        _leg(700, 182.6, -30.0, 7.0),
        {"error": "boom", "dataset": "X 1h"},
    ]
    row = fa.aggregate_strategy("vwap_reversion", "spot", legs)
    assert row["trades"] == 1500
    assert row["n_legs"] == 2
    assert row["n_errors"] == 1
    # mean gross +6, mean net -35 → drag 41pp, gross > 0 net < 0 → graduate
    assert row["mean_gross_ret"] == pytest.approx(6.0)
    assert row["mean_net_ret"] == pytest.approx(-35.0)
    assert row["fee_drag_pp"] == pytest.approx(41.0)
    assert row["verdict"] == "graduate_m1"
    assert row["short_unmeasured"] is False
    # Unit-consistent drag/trade: total drag (mean 41pp x 2 legs) over total
    # trades (1500), rounded to 4dp — NOT the per-leg mean over the leg-summed
    # denominator (the #1003 unit-mismatch fix).
    assert row["drag_per_trade_pp"] == pytest.approx(round(41.0 * 2 / 1500, 4))


def test_aggregate_all_no_data_is_no_trades():
    row = fa.aggregate_strategy("x", "spot", [])
    assert row["verdict"] == "no_trades"
    assert row["trades"] == 0
    assert row["fee_drag_pp"] is None
    assert row["trades_per_year"] is None
    assert row["drag_per_trade_pp"] is None


def test_aggregate_deprecate_when_gross_negative():
    legs = [_leg(300, 182.6, -10.0, -2.0), _leg(300, 182.6, -12.0, -4.0)]
    row = fa.aggregate_strategy("macd", "spot", legs)
    assert row["mean_gross_ret"] == pytest.approx(-3.0)
    assert row["verdict"] == "deprecate"
    assert row["short_unmeasured"] is False


def test_aggregate_short_unmeasured_withholds_deprecate():
    # Same gross-negative long leg, but the strategy is short-capable: the
    # verdict must withhold deprecate (short half unmeasured), not assert it.
    legs = [_leg(300, 182.6, -10.0, -2.0), _leg(300, 182.6, -12.0, -4.0)]
    row = fa.aggregate_strategy("triple_ema_bidir", "futures", legs,
                                short_capable=True)
    assert row["short_unmeasured"] is True
    assert row["verdict"] == "unscreened_short"


def test_aggregate_short_only_no_long_trades_is_unscreened_not_no_trades():
    # Short-only strategy with zero long trades. The honest verdict is
    # unscreened_short, NOT no_trades ("never fired").
    legs = [_leg(0, 182.6, 0.0, 0.0)]
    row = fa.aggregate_strategy("bear_pullback_st", "futures", legs,
                                short_capable=True)
    assert row["trades"] == 0
    assert row["verdict"] == "unscreened_short"


def test_aggregate_short_unmeasured_keeps_positive_long_verdict():
    # Gross-positive long leg stays a real graduate finding (flagged
    # long-only), not withheld, even when short-capable.
    legs = [_leg(200, 182.6, -3.0, 8.0)]
    row = fa.aggregate_strategy("vol_momentum", "futures", legs,
                                short_capable=True)
    assert row["short_unmeasured"] is True
    assert row["verdict"] == "graduate_m1"


def test_strategy_is_short_capable_predicate():
    # Long-only name: never short-capable regardless of params.
    assert fa.strategy_is_short_capable({}, "sma_crossover") is False
    # Bidirectional name with no allow_short gate: short-capable.
    assert fa.strategy_is_short_capable({"short_period": 8}, "triple_ema_bidir") is True
    assert fa.strategy_is_short_capable(None, "bear_pullback_st") is True
    # allow_short-gated name: per-variant flag decides (spot long-only, futures short).
    assert fa.strategy_is_short_capable({"allow_short": False}, "mtf_confluence") is False
    assert fa.strategy_is_short_capable({"allow_short": True}, "mtf_confluence") is True


def test_live_bidirectional_set_matches_go_source():
    # Anti-staleness guard: the Python set must equal init.go's
    # bidirectionalPerpsStrategies map, so a new short-entry strategy added in
    # Go cannot silently leave a stale long-only classification here.
    import re
    init_go = os.path.abspath(os.path.join(
        _BT_DIR, "..", "scheduler", "init.go"))
    with open(init_go) as fh:
        src = fh.read()
    block = src.split("bidirectionalPerpsStrategies = map[string]bool{", 1)[1]
    block = block.split("}", 1)[0]
    go_names = set(re.findall(r'"([^"]+)":\s*true', block))
    assert go_names, "failed to parse init.go bidirectional set"
    assert set(fa.LIVE_BIDIRECTIONAL_STRATEGIES) == go_names


# ---------------------------------------------------------------------------
# rank_rows — fee drag desc, no_trades last
# ---------------------------------------------------------------------------

def test_rank_rows_orders_by_drag_then_no_trades_last():
    rows = [
        {"strategy": "low", "verdict": "healthy", "fee_drag_pp": 2.0},
        {"strategy": "high", "verdict": "graduate_m1", "fee_drag_pp": 40.0},
        {"strategy": "dead", "verdict": "no_trades", "fee_drag_pp": None},
        {"strategy": "mid", "verdict": "deprecate", "fee_drag_pp": 15.0},
    ]
    ordered = [r["strategy"] for r in fa.rank_rows(rows)]
    assert ordered == ["high", "mid", "low", "dead"]


def test_rank_rows_no_trades_tiebreak_by_name():
    rows = [
        {"strategy": "zeta", "verdict": "no_trades", "fee_drag_pp": None},
        {"strategy": "alpha", "verdict": "no_trades", "fee_drag_pp": None},
    ]
    ordered = [r["strategy"] for r in fa.rank_rows(rows)]
    assert ordered == ["alpha", "zeta"]


# ---------------------------------------------------------------------------
# render_markdown — structure + verdict sections
# ---------------------------------------------------------------------------

def test_render_markdown_has_table_and_sections():
    ranked = [
        fa.aggregate_strategy("churner", "spot",
                              [_leg(1500, 365.0, -40.0, 6.0)]),   # graduate
        fa.aggregate_strategy("hopeless", "spot",
                              [_leg(900, 365.0, -20.0, -5.0)]),   # deprecate
        fa.aggregate_strategy("idle", "spot", []),                # no_trades
    ]
    meta = {"command": "uv run ...", "registry": "spot",
            "windows_desc": "oos", "datasets_desc": "BTC/USDT 4h",
            "capital": 1000.0, "date": "2026-06-12"}
    md = fa.render_markdown(ranked, meta)
    assert "# Fee-aware selectivity audit (#999 M5)" in md
    assert "| rank | strategy |" in md
    assert "## Deprecation list" in md
    assert "hopeless" in md
    assert "## M1 graduations" in md
    assert "churner" in md
    assert "raise selectivity" in md


def test_render_markdown_unscreened_short_section_and_dagger():
    ranked = fa.rank_rows([
        # gross-negative long leg BUT short-capable → withheld, not deprecated
        fa.aggregate_strategy("shorty", "futures",
                              [_leg(80, 365.0, -10.0, -2.0)],
                              short_capable=True),
        fa.aggregate_strategy("clean", "spot",
                              [_leg(900, 365.0, -20.0, -5.0)]),   # deprecate
    ])
    meta = {"command": "uv run ...", "registry": "both",
            "windows_desc": "oos", "datasets_desc": "BTC/USDT 4h",
            "capital": 1000.0, "date": "2026-06-12"}
    md = fa.render_markdown(ranked, meta)
    assert "## Unscreened short legs" in md
    assert "shorty †" in md                       # dagger flags the row
    # a withheld strategy must NOT appear in the deprecation deliverable
    dep_section = md.split("## Deprecation list")[1].split("## M1 graduations")[0]
    assert "shorty" not in dep_section
    assert "clean" in dep_section


# ---------------------------------------------------------------------------
# enumerate_targets — registry union + subset filter
# ---------------------------------------------------------------------------

def test_enumerate_targets_both_skips_hold_and_handles_variants():
    targets = fa.enumerate_targets("both")
    names = [t[0] for t in targets]
    assert "hold" not in names
    pairs = {(t[0], t[1]) for t in targets}

    # A futures-only name resolves to futures.
    assert ("delta_neutral_funding", "futures") in pairs

    # A shared name with byte-identical params is screened ONCE (spot only).
    assert ("sma_crossover", "spot") in pairs
    assert ("sma_crossover", "futures") not in pairs
    assert names.count("sma_crossover") == 1

    # A shared name whose futures default_params differ materially (e.g.
    # momentum threshold 3.0 vs 5.0) is screened on BOTH registries (#1003).
    assert ("momentum", "spot") in pairs
    assert ("momentum", "futures") in pairs
    assert names.count("momentum") == 2


def test_enumerate_targets_variant_detection_matches_registry():
    # The variant set must be derived from the registries, not a stale list:
    # every shared name with differing default_params appears twice, every
    # byte-identical shared name once.
    from registry_loader import load_registry
    s, f = load_registry("spot"), load_registry("futures")
    shared = (set(s.list_strategies()) & set(f.list_strategies())) - fa.SKIP_STRATEGIES
    differing = {n for n in shared
                 if s.STRATEGY_REGISTRY[n].get("default_params")
                 != f.STRATEGY_REGISTRY[n].get("default_params")}

    names = [t[0] for t in fa.enumerate_targets("both")]
    for n in differing:
        assert names.count(n) == 2, n
    for n in shared - differing:
        assert names.count(n) == 1, n


def test_enumerate_targets_subset_filter_and_unknown_raises():
    targets = fa.enumerate_targets("spot", subset=["macd", "vwap_reversion"])
    assert {t[0] for t in targets} == {"macd", "vwap_reversion"}
    with pytest.raises(SystemExit):
        fa.enumerate_targets("spot", subset=["does_not_exist"])


# ---------------------------------------------------------------------------
# End-to-end leg: gross run reuses run_leg, matches trade count, out-returns net
# ---------------------------------------------------------------------------

class _FakeRegistry:
    STRATEGY_REGISTRY = {"alternator": {"default_params": {}, "description": "t"}}

    @staticmethod
    def list_strategies():
        return ["alternator"]

    @staticmethod
    def apply_strategy(name, df, params):
        out = df.copy()
        sig = np.zeros(len(out), dtype=int)
        sig[10::20] = 1
        sig[20::20] = -1
        out["signal"] = sig
        return out


def _synthetic_df(n=160):
    idx = pd.date_range("2026-01-01", periods=n, freq="1h")
    base = 100 + np.cumsum(np.sin(np.arange(n) / 5.0))
    return pd.DataFrame({
        "open": base, "high": base * 1.01, "low": base * 0.99,
        "close": base, "volume": np.full(n, 1000.0),
    }, index=idx)


def test_screen_leg_count_mismatch_becomes_error(monkeypatch):
    # If the net and gross runs disagree on trade count, the pair is not
    # comparable — screen_leg must demote it to an error leg, not a drag row.
    def fake_run_leg(reg, name, params, sym, tf, window, capital=0.0,
                     commission_pct=None, slippage_pct=None, **kw):
        trades = 5 if commission_pct is None else 4   # gross differs
        return {"trades": trades, "span_days": 10.0, "return_pct": 1.0,
                "sharpe": 0.5}

    monkeypatch.setattr(fa, "run_leg", fake_run_leg, raising=True)
    leg = fa.screen_leg(object(), "x", "BTC/USDT", "1h",
                        ("2026-01-01", None), capital=1000.0)
    assert leg is not None
    assert leg["error"] is not None and "mismatch" in leg["error"]
    # and an aggregate over it counts the error, scores no data
    row = fa.aggregate_strategy("x", "spot", [leg])
    assert row["n_errors"] == 1 and row["verdict"] == "no_trades"


def test_screen_leg_forwards_direction_to_net_and_gross(monkeypatch):
    seen = []

    def fake_run_leg(reg, name, params, sym, tf, window, capital=0.0,
                     direction=None, commission_pct=None, slippage_pct=None, **kw):
        seen.append((direction, commission_pct, slippage_pct))
        return {"trades": 2, "span_days": 10.0, "return_pct": 1.0,
                "sharpe": 0.5}

    monkeypatch.setattr(fa, "run_leg", fake_run_leg, raising=True)
    leg = fa.screen_leg(object(), "x", "BTC/USDT", "1h",
                        ("2026-01-01", None), capital=1000.0,
                        direction="short")
    assert leg is not None and leg["error"] is None
    assert seen == [
        ("short", None, None),
        ("short", 0.0, 0.0),
    ]


def test_screen_strategy_direction_short_is_measured_not_unscreened(monkeypatch):
    class _ShortRegistry:
        STRATEGY_REGISTRY = {"bear_pullback_st": {"default_params": {}}}

    def fake_screen_leg(*args, **kwargs):
        assert kwargs["direction"] == "short"
        return {"dataset": "BTC/USDT 1h", "error": None, "trades": 4,
                "span_days": 30.0, "net_ret": -2.0, "gross_ret": -1.0,
                "net_sharpe": -0.5}

    monkeypatch.setattr(fa, "screen_leg", fake_screen_leg, raising=True)
    row = fa.screen_strategy(_ShortRegistry(), "bear_pullback_st", "futures",
                             [("BTC/USDT", "1h")], ["oos"], 1000.0,
                             direction="short")
    assert row["short_unmeasured"] is False
    assert row["verdict"] == "deprecate"


def test_run_leg_stamps_span_days(monkeypatch):
    df = _synthetic_df()
    import data_fetcher
    monkeypatch.setattr(data_fetcher, "load_cached_data",
                        lambda *a, **k: df, raising=True)
    leg = ew.run_leg(_FakeRegistry(), "alternator", None, "BTC/USDT", "1h",
                     ("2026-01-01", None))
    assert leg is not None
    # 160 hourly bars → ~159 hours span
    assert leg["span_days"] == pytest.approx(159 / 24.0, abs=0.01)


def test_screen_leg_gross_zeroes_friction(monkeypatch):
    df = _synthetic_df()
    import data_fetcher
    monkeypatch.setattr(data_fetcher, "load_cached_data",
                        lambda *a, **k: df, raising=True)
    leg = fa.screen_leg(_FakeRegistry(), "alternator", "BTC/USDT", "1h",
                        ("2026-01-01", None), capital=1000.0)
    assert leg is not None and leg["error"] is None
    assert leg["trades"] > 0
    # Same signals → identical trade count on both runs (gross uses net's count).
    # Zero friction must not reduce return: gross >= net for the same fills.
    assert leg["gross_ret"] >= leg["net_ret"]
    # With real trades and a fee model, friction is strictly positive.
    assert leg["gross_ret"] > leg["net_ret"]


def test_screen_leg_no_data_returns_none(monkeypatch):
    import data_fetcher
    monkeypatch.setattr(data_fetcher, "load_cached_data",
                        lambda *a, **k: pd.DataFrame(), raising=True)
    assert fa.screen_leg(_FakeRegistry(), "alternator", "BTC/USDT", "1h",
                         ("2023-01-01", "2024-01-01"), capital=1000.0) is None


def test_screen_leg_error_is_captured_not_raised(monkeypatch):
    df = _synthetic_df()
    import data_fetcher
    monkeypatch.setattr(data_fetcher, "load_cached_data",
                        lambda *a, **k: df, raising=True)

    class _Boom(_FakeRegistry):
        @staticmethod
        def apply_strategy(name, df, params):
            raise RuntimeError("strategy blew up")

    leg = fa.screen_leg(_Boom(), "alternator", "BTC/USDT", "1h",
                        ("2026-01-01", None), capital=1000.0)
    assert leg is not None
    assert leg["error"] is not None and "blew up" in leg["error"]


def test_reproduce_command_includes_direction():
    args = SimpleNamespace(
        registry="futures",
        strategies="vwap_rejection_st",
        windows="oos",
        datasets=None,
        direction="short",
        capital=fa.DEFAULT_CAPITAL,
        markdown_out=None,
    )
    cmd = fa._reproduce_command(args)
    assert "--direction short" in cmd
