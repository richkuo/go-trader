import os
import sys

import numpy as np
import pandas as pd
import pytest

_BT_DIR = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
if _BT_DIR not in sys.path:
    sys.path.insert(0, _BT_DIR)

import fee_audit as fa


def test_salvage_verdict_no_trades_deprecate_graduate_healthy_and_short_unmeasured():
    case = "test_salvage_verdict_no_trades"
    assert fa.salvage_verdict(0, None, None) == "no_trades", case
    assert fa.salvage_verdict(0, 5.0, 5.0) == "no_trades", case
    assert fa.salvage_verdict(10, None, -3.0) == "no_trades", case

    case = "test_salvage_verdict_deprecate_when_gross_nonpositive"
    assert fa.salvage_verdict(100, -2.0, -5.0) == "deprecate", case
    assert fa.salvage_verdict(100, 0.0, -5.0) == "deprecate", case

    case = "test_salvage_verdict_graduate_when_gross_positive_net_dead"
    assert fa.salvage_verdict(100, 8.0, -3.0) == "graduate_m1", case
    assert fa.salvage_verdict(100, 8.0, 0.0) == "graduate_m1", case
    assert fa.salvage_verdict(100, 8.0, None) == "graduate_m1", case

    case = "test_salvage_verdict_healthy_when_net_survives"
    assert fa.salvage_verdict(50, 12.0, 4.0) == "healthy", case

    case = "test_salvage_verdict_short_unmeasured_withholds_negative_calls"
    assert fa.salvage_verdict(0, None, None, short_unmeasured=True) == "unscreened_short", case
    assert fa.salvage_verdict(100, -2.0, -5.0, short_unmeasured=True) == "unscreened_short", case
    assert fa.salvage_verdict(100, 0.0, -5.0, short_unmeasured=True) == "unscreened_short", case
    assert fa.salvage_verdict(100, 8.0, -3.0, short_unmeasured=True) == "graduate_m1", case
    assert fa.salvage_verdict(50, 12.0, 4.0, short_unmeasured=True) == "healthy", case


def _leg(trades, span, net, gross, sharpe=1.0):
    return {"error": None, "trades": trades, "span_days": span,
            "net_ret": net, "gross_ret": gross, "net_sharpe": sharpe}


def test_aggregate_strategy_sums_legs_and_assigns_every_verdict():
    case = "test_aggregate_excludes_errors_and_sums_trades"
    legs = [
        _leg(800, 182.6, -40.0, 5.0),
        _leg(700, 182.6, -30.0, 7.0),
        {"error": "boom", "dataset": "X 1h"},
    ]
    row = fa.aggregate_strategy("vwap_reversion", "spot", legs)
    assert row["trades"] == 1500, case
    assert row["n_legs"] == 2, case
    assert row["n_errors"] == 1, case
    assert row["mean_gross_ret"] == pytest.approx(6.0), case
    assert row["mean_net_ret"] == pytest.approx(-35.0), case
    assert row["fee_drag_pp"] == pytest.approx(41.0), case
    assert row["verdict"] == "graduate_m1", case
    assert row["short_unmeasured"] is False, case
    assert row["drag_per_trade_pp"] == pytest.approx(round(41.0 * 2 / 1500, 4)), case

    case = "test_aggregate_all_no_data_is_no_trades"
    row = fa.aggregate_strategy("x", "spot", [])
    assert row["verdict"] == "no_trades", case
    assert row["trades"] == 0, case
    assert row["fee_drag_pp"] is None, case
    assert row["trades_per_year"] is None, case
    assert row["drag_per_trade_pp"] is None, case

    case = "test_aggregate_deprecate_when_gross_negative"
    legs = [_leg(300, 182.6, -10.0, -2.0), _leg(300, 182.6, -12.0, -4.0)]
    row = fa.aggregate_strategy("macd", "spot", legs)
    assert row["mean_gross_ret"] == pytest.approx(-3.0), case
    assert row["verdict"] == "deprecate", case
    assert row["short_unmeasured"] is False, case

    case = "test_aggregate_short_unmeasured_withholds_deprecate"
    legs = [_leg(300, 182.6, -10.0, -2.0), _leg(300, 182.6, -12.0, -4.0)]
    row = fa.aggregate_strategy("triple_ema_bidir", "futures", legs,
                                short_capable=True)
    assert row["short_unmeasured"] is True, case
    assert row["verdict"] == "unscreened_short", case

    case = "test_aggregate_short_only_no_long_trades_is_unscreened_not_no_trades"
    legs = [_leg(0, 182.6, 0.0, 0.0)]
    row = fa.aggregate_strategy("bear_pullback_st", "futures", legs,
                                short_capable=True)
    assert row["trades"] == 0, case
    assert row["verdict"] == "unscreened_short", case

    case = "test_aggregate_short_unmeasured_keeps_positive_long_verdict"
    legs = [_leg(200, 182.6, -3.0, 8.0)]
    row = fa.aggregate_strategy("vol_momentum", "futures", legs,
                                short_capable=True)
    assert row["short_unmeasured"] is True, case
    assert row["verdict"] == "graduate_m1", case


def test_strategy_is_short_capable_predicate():
    assert fa.strategy_is_short_capable({}, "sma_crossover") is False
    assert fa.strategy_is_short_capable({"short_period": 8}, "triple_ema_bidir") is True
    assert fa.strategy_is_short_capable(None, "bear_pullback_st") is True
    assert fa.strategy_is_short_capable({"allow_short": False}, "mtf_confluence") is False
    assert fa.strategy_is_short_capable({"allow_short": True}, "mtf_confluence") is True


def test_live_bidirectional_set_is_the_registry_short_entry_set():
    from registry_loader import load_registry
    registry = load_registry("futures")._registry
    assert fa.LIVE_BIDIRECTIONAL_STRATEGIES == registry.short_entry_strategies()


def test_enumerate_targets_both_skips_hold_and_handles_variants():
    targets = fa.enumerate_targets("both")
    names = [t[0] for t in targets]
    assert "hold" not in names
    pairs = {(t[0], t[1]) for t in targets}

    assert ("delta_neutral_funding", "futures") in pairs

    assert ("range_scalper", "spot") in pairs
    assert ("macd", "spot") in pairs

    assert ("sma_crossover", "spot") in pairs
    assert ("sma_crossover", "futures") not in pairs
    assert names.count("sma_crossover") == 1

    assert ("momentum", "spot") in pairs
    assert ("momentum", "futures") in pairs
    assert names.count("momentum") == 2


def test_enumerate_targets_variant_detection_matches_registry():
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
    def fake_run_leg(reg, name, params, sym, tf, window, capital=0.0,
                     commission_pct=None, slippage_pct=None, **kw):
        trades = 5 if commission_pct is None else 4
        return {"trades": trades, "span_days": 10.0, "return_pct": 1.0,
                "sharpe": 0.5}

    monkeypatch.setattr(fa, "run_leg", fake_run_leg, raising=True)
    leg = fa.screen_leg(object(), "x", "BTC/USDT", "1h",
                        ("2026-01-01", None), capital=1000.0)
    assert leg is not None
    assert leg["error"] is not None and "mismatch" in leg["error"]
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


def test_screen_leg_gross_zeroes_friction(monkeypatch):
    df = _synthetic_df()
    import data_fetcher
    monkeypatch.setattr(data_fetcher, "load_cached_data",
                        lambda *a, **k: df, raising=True)
    leg = fa.screen_leg(_FakeRegistry(), "alternator", "BTC/USDT", "1h",
                        ("2026-01-01", None), capital=1000.0)
    assert leg is not None and leg["error"] is None
    assert leg["trades"] > 0
    assert leg["gross_ret"] >= leg["net_ret"]
    assert leg["gross_ret"] > leg["net_ret"]
