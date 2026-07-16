"""#1276: scale-in / pyramiding simulation with the frozen risk anchor.

Live (#873) blends price+size into ``AvgCost`` for PnL while FREEZING the
SL/TP trigger geometry to the first entry (``Position.riskAnchorPrice()``).
These tests pin: the ``perpsScaleInDecision`` gate port (caps, signed ATR
spacing, 1e-9 tolerances, last-add-price fallback), the ``applyScaleIn``
blend math (qty-weighted avg cost, ``InitialQuantity`` growth, first-add
anchor freeze), anchor-not-blend for stop/TP geometry after an add, per-add
fees pooled into the #1241 entry-fee pro-rating, live #T parity (adds create
no Trade rows), default-off byte-identical results, and the loader/init
rejects mirroring the live validator.
"""
import json
import pathlib
import sys

import numpy as np
import pandas as pd
import pytest

sys.path.insert(0, str(pathlib.Path(__file__).parent.parent.parent / "shared_tools"))
sys.path.insert(0, str(pathlib.Path(__file__).parent.parent))

from backtester import Backtester, _scale_in_decision, _normalize_scale_in_cfg
import run_backtest


def _df(closes, signals, atr=None, highs=None, lows=None, opens=None):
    closes = np.asarray(closes, dtype=float)
    n = len(closes)
    idx = pd.date_range("2024-01-01", periods=n, freq="D")
    df = pd.DataFrame(
        {
            "open":   np.asarray(opens, dtype=float) if opens is not None else closes,
            "high":   np.asarray(highs, dtype=float) if highs is not None else closes + 0.4,
            "low":    np.asarray(lows, dtype=float) if lows is not None else closes - 0.4,
            "close":  closes,
            "volume": np.full(n, 1000.0),
            "signal": np.asarray(signals, dtype=float),
        },
        index=idx,
    )
    if atr is not None:
        df["atr"] = np.asarray(atr, dtype=float)
    return df


def _run(df, capital=10000.0, **kw):
    kw.setdefault("commission_pct", 0.0)
    kw.setdefault("slippage_pct", 0.0)
    bt = Backtester(initial_capital=capital, **kw)
    return bt.run(df.copy(), strategy_name="x", symbol="BTC/USDT",
                  timeframe="1d", save=False)


# ─── Pure gate parity with perpsScaleInDecision (scheduler/scale_in.go) ───────


def _decide(cfg=None, side="long", qty=1.0, avg_cost=100.0, entry_atr=2.0,
            count=0, added=0.0, last_add=0.0, signal=1, price=100.0,
            default_notional=1000.0):
    return _scale_in_decision(
        _normalize_scale_in_cfg(cfg), side, qty, avg_cost, entry_atr,
        count, added, last_add, signal, price, default_notional,
    )


def test_gate_direction_match():
    # Same-direction long add passes; everything else is not-an-add.
    _, ok, _ = _decide(side="long", signal=1)
    assert ok
    _, ok, reason = _decide(side="long", signal=-1)
    assert not ok and reason == "not a same-direction add"
    _, ok, _ = _decide(side="short", signal=-1)
    assert ok
    _, ok, _ = _decide(side="short", signal=1)
    assert not ok
    _, ok, _ = _decide(side="long", signal=1, qty=0.0)
    assert not ok


def test_gate_no_price():
    _, ok, reason = _decide(price=0.0)
    assert not ok and reason == "no price for scale-in"


def test_gate_max_adds():
    cfg = {"max_adds": 2}
    _, ok, _ = _decide(cfg, count=1)
    assert ok
    _, ok, reason = _decide(cfg, count=2)
    assert not ok and reason == "scale-in max_adds reached"


def test_gate_max_added_notional_boundary():
    # Cap compares added + requested against cap + 1e-9 (live tolerance):
    # exactly-at-cap passes, a hair over is rejected.
    cfg = {"max_added_notional_usd": 2000.0}
    _, ok, _ = _decide(cfg, added=1000.0, default_notional=1000.0)
    assert ok
    _, ok, reason = _decide(cfg, added=1000.0 + 1e-6, default_notional=1000.0)
    assert not ok and reason == "scale-in max_added_notional_usd reached"


def test_gate_zero_notional():
    _, ok, reason = _decide(default_notional=0.0)
    assert not ok and reason == "scale-in add notional resolves to zero"


def test_gate_add_notional_override():
    qty, ok, _ = _decide({"add_notional_usd": 500.0}, price=100.0,
                         default_notional=1000.0)
    assert ok and qty == pytest.approx(5.0)


def test_gate_spacing_add_to_winners():
    # +1 ATR spacing from the last leg: long at 100, ATR 2 → needs price
    # >= 102 in favor.
    cfg = {"add_spacing_atr": 1.0}
    _, ok, reason = _decide(cfg, last_add=100.0, price=101.9)
    assert not ok and "add-to-winners" in reason
    _, ok, _ = _decide(cfg, last_add=100.0, price=102.0)
    assert ok


def test_gate_spacing_average_down():
    # -1 ATR spacing: needs an ADVERSE move of >= 2 (price <= 98 for a long).
    cfg = {"add_spacing_atr": -1.0}
    _, ok, reason = _decide(cfg, last_add=100.0, price=98.1)
    assert not ok and "average-down" in reason
    _, ok, _ = _decide(cfg, last_add=100.0, price=98.0)
    assert ok


def test_gate_spacing_short_sign():
    # Short add-to-winners: favorable = DOWN moves.
    cfg = {"add_spacing_atr": 1.0}
    _, ok, _ = _decide(cfg, side="short", signal=-1, last_add=100.0, price=98.0)
    assert ok
    _, ok, _ = _decide(cfg, side="short", signal=-1, last_add=100.0, price=99.0)
    assert not ok


def test_gate_spacing_requires_entry_atr():
    _, ok, reason = _decide({"add_spacing_atr": 1.0}, entry_atr=0.0)
    assert not ok and reason == "scale-in spacing requires a positive EntryATR"


def test_gate_spacing_last_add_falls_back_to_avg_cost():
    # Before the first add, last_add_price is 0 → the reference is avg_cost.
    cfg = {"add_spacing_atr": 1.0}
    _, ok, _ = _decide(cfg, avg_cost=100.0, last_add=0.0, price=102.0)
    assert ok
    _, ok, _ = _decide(cfg, avg_cost=100.0, last_add=0.0, price=101.0)
    assert not ok


def test_gate_spacing_zero_no_gate():
    _, ok, _ = _decide({"add_spacing_atr": 0.0}, last_add=100.0, price=100.0)
    assert ok


# ─── Construction / config validation ────────────────────────────────────────


def test_init_rejects_scale_in_without_flag():
    with pytest.raises(ValueError, match="allow_scale_in"):
        Backtester(scale_in={"max_adds": 2})


def test_init_rejects_risk_sizing_combo():
    with pytest.raises(ValueError, match="risk_per_trade_pct"):
        Backtester(allow_scale_in=True, risk_per_trade_pct=1.0,
                   stop_loss_atr_mult=1.0)


def test_init_rejects_unknown_scale_in_key():
    with pytest.raises(ValueError, match="unknown key"):
        Backtester(allow_scale_in=True, scale_in={"max_ads": 2})


def test_init_rejects_negative_bounds():
    with pytest.raises(ValueError, match="max_adds"):
        Backtester(allow_scale_in=True, scale_in={"max_adds": -1})
    with pytest.raises(ValueError, match="add_notional_usd"):
        Backtester(allow_scale_in=True, scale_in={"add_notional_usd": -5})


def test_run_rejects_entry_fraction_column():
    df = _df([100, 100, 100], [1, 0, 0])
    df["entry_fraction"] = 0.5
    with pytest.raises(ValueError, match="entry_fraction"):
        _run(df, allow_scale_in=True)


# ─── Default-off golden equality ──────────────────────────────────────────────


def test_default_off_same_direction_signal_still_skipped():
    # A repeat same-direction signal changes NOTHING without the opt-in —
    # byte-identical results (the pre-#1276 behavior).
    closes = [100, 100, 110, 110, 120, 100]
    repeat = _run(_df(closes, [1, 0, 1, 0, -1, 0]))
    plain = _run(_df(closes, [1, 0, 0, 0, -1, 0]))
    assert repeat["trades"] == plain["trades"]
    assert repeat["final_capital"] == plain["final_capital"]
    assert "scale_in_adds" not in repeat


# ─── Blend + anchor semantics (plain path) ────────────────────────────────────


def test_add_blends_avg_cost_and_pnl_uses_blend():
    # Open at 100 (bar 1 fill from bar-0 signal), add at 110 (bar 3 fill from
    # bar-2 signal), close at 120 (bar 5 fill). No fees/slippage.
    # Capital 10000 → 100 shares at 100. Default add notional = first-leg
    # notional 10000; decision price = bar 2's close (110) → add qty
    # 10000/110 = 90.909…; fill at bar 3's open 110.
    closes = [100, 100, 110, 110, 110, 120, 120]
    df = _df(closes, [1, 0, 1, 0, -1, 0, 0])
    res = _run(df, allow_scale_in=True)
    assert res["scale_in_adds"] == 1
    add_qty = 10000.0 / 110.0
    blend = (100.0 * 100.0 + add_qty * 110.0) / (100.0 + add_qty)
    (trade,) = res["trades"]
    assert trade["shares"] == pytest.approx(100.0 + add_qty)
    assert trade["entry_price"] == pytest.approx(blend)
    assert trade["scale_in_adds"] == 1
    # PnL uses the blend: shares × (exit − blend).
    assert trade["pnl"] == pytest.approx((100.0 + add_qty) * (120.0 - blend))
    # Equity conservation: final capital = initial + pnl.
    assert res["final_capital"] == pytest.approx(10000.0 + trade["pnl"], abs=0.01)


def test_two_adds_accumulate_applyscalein_math():
    closes = [100, 100, 105, 105, 110, 110, 110, 130, 130]
    #        sig:1     add    add          close
    df = _df(closes, [1, 0, 1, 0, 1, 0, -1, 0, 0])
    res = _run(df, allow_scale_in=True,
               scale_in={"add_notional_usd": 1050.0})
    assert res["scale_in_adds"] == 2
    q0 = 100.0                       # 10000 @ 100
    q1 = 1050.0 / 105.0              # decision price bar-2 close = 105
    b1 = (q0 * 100.0 + q1 * 105.0) / (q0 + q1)
    q2 = 1050.0 / 110.0              # decision price bar-4 close = 110
    b2 = ((q0 + q1) * b1 + q2 * 110.0) / (q0 + q1 + q2)
    (trade,) = res["trades"]
    assert trade["shares"] == pytest.approx(q0 + q1 + q2)
    assert trade["entry_price"] == pytest.approx(b2)
    assert res["scale_in_added_notional_usd"] == pytest.approx(
        q1 * 105.0 + q2 * 110.0)


def test_fixed_sl_trigger_frozen_across_add():
    # Long at 100 with a 1×ATR(2) fixed stop at 98, add-to-winners at 104.
    # The blend is ≈ 101.96, so a re-derived trigger would sit at ≈ 99.96 —
    # bar 4's low of 99.0 would stop THAT out. The FROZEN anchor trigger
    # stays at 98, so the position survives and exits on the signal instead.
    closes = [100.0, 100.0, 104.0, 104.0, 99.5, 106.0, 106.0, 106.0]
    lows = [99.6, 99.6, 103.6, 103.6, 99.0, 105.6, 105.6, 105.6]
    highs = [c + 0.4 for c in closes]
    df = _df(closes, [1, 0, 1, 0, 0, 0, -1, 0], atr=[2.0] * 8,
             highs=highs, lows=lows)
    res = _run(df, allow_scale_in=True, stop_loss_atr_mult=1.0,
               scale_in={"add_spacing_atr": 1.0})
    assert res["scale_in_adds"] == 1
    (trade,) = res["trades"]
    # Survived the 99.0 low (a blend trigger would have fired there).
    assert trade["exit_reason"] == "signal"
    assert trade["exit_price"] == pytest.approx(106.0)
    assert trade["shares"] == pytest.approx(100.0 + 10000.0 / 104.0)


def test_frozen_sl_exits_full_grown_quantity():
    # Same geometry, but the bar after the add pierces the FROZEN 98 trigger
    # — the FULL grown quantity exits at the frozen trigger price.
    closes = [100.0, 100.0, 104.0, 104.0, 99.5, 99.5]
    lows = [99.6, 99.6, 103.6, 103.6, 97.9, 99.0]
    highs = [c + 0.4 for c in closes]
    df = _df(closes, [1, 0, 1, 0, 0, 0], atr=[2.0] * 6,
             highs=highs, lows=lows)
    res = _run(df, allow_scale_in=True, stop_loss_atr_mult=1.0,
               scale_in={"add_spacing_atr": 1.0})
    assert res["scale_in_adds"] == 1
    (trade,) = res["trades"]
    assert trade["exit_reason"] == "signal_sl"
    assert trade["exit_price"] == pytest.approx(98.0)
    assert trade["shares"] == pytest.approx(100.0 + 10000.0 / 104.0)


def test_add_leaves_cash_negative_but_equity_exact():
    # Long add spends notional from cash (margin semantics) — cash may go
    # negative while equity (cash + position×mark) stays exact.
    closes = [100, 100, 100, 100, 100, 100]
    df = _df(closes, [1, 0, 1, 0, -1, 0])
    res = _run(df, allow_scale_in=True)
    # Flat price, no fees: opening 100 sh + adding 100 sh then closing all
    # at 100 conserves capital exactly.
    assert res["scale_in_adds"] == 1
    assert res["final_capital"] == pytest.approx(10000.0)


def test_short_add_mirrors_long_math():
    # Plain short/flat path (direction="short"): -1 opens, repeat -1 adds,
    # +1 covers. Open 100 sh @ 100, add 10000/95 sh @ 95 (decision price =
    # bar 2's close), cover all @ 90.
    closes = [100, 100, 95, 95, 90, 90]
    df = _df(closes, [-1, 0, -1, 0, 1, 0])
    res = _run(df, allow_scale_in=True, direction="short")
    assert res["scale_in_adds"] == 1
    add_qty = 10000.0 / 95.0
    blend = (100.0 * 100.0 + add_qty * 95.0) / (100.0 + add_qty)
    (trade,) = res["trades"]
    assert trade["side"] == "short"
    assert trade["shares"] == pytest.approx(100.0 + add_qty)
    assert trade["entry_price"] == pytest.approx(blend)
    # Short PnL on the blend: shares × (blend − exit).
    expected_pnl = (100.0 + add_qty) * (blend - 90.0)
    assert trade["pnl"] == pytest.approx(expected_pnl, abs=0.01)
    assert res["final_capital"] == pytest.approx(10000.0 + expected_pnl, abs=0.01)


# ─── Anchor-not-blend for close-evaluator geometry (engine path) ──────────────


def test_tiered_tp_threshold_reads_anchor_not_blend():
    # tiered_tp_atr: first tier at entry + 1×ATR (ATR frozen at entry = 2.0)
    # → 102 from the FROZEN anchor 100. Average down at 96 (blend ≈ 97.96,
    # blend-derived tier ≈ 99.96). Bars marked at 100.5 would fire a
    # blend-derived tier but NOT the anchor tier; the bar at 102.5 fires the
    # anchor tier. Asserting the first partial-close leg exits on the
    # 102.5-close bar (fill next open) proves the evaluator saw the anchor.
    closes = [100, 100, 96, 96, 100.5, 100.5, 102.5, 102.5, 102.5]
    df = _df(closes, [1, 0, 1, 0, 0, 0, 0, 0, 0], atr=[2.0] * 9)
    tiers = [
        {"atr_multiple": 1.0, "close_fraction": 0.5},
        {"atr_multiple": 10.0, "close_fraction": 1.0},
    ]
    res = _run(
        df, allow_scale_in=True,
        close_strategies=[{"name": "tiered_tp_atr",
                           "params": {"tp_tiers": tiers, "atr_source": "entry"}}],
        scale_in={"add_spacing_atr": -1.0},
    )
    assert res["scale_in_adds"] == 1
    legs = res["trades"]
    assert legs, "expected at least the tier-1 partial close"
    first = legs[0]
    # Anchor tier (102) first confirmed by the 102.5 close at bar index 6;
    # fills at bar 7's open. A blend tier (~99.96) would have filled at bar
    # 5's open off the 100.5 close.
    assert str(df.index[7]) in first["exit_date"]
    # PnL still uses the blend entry.
    add_qty = 10000.0 / 96.0
    blend = (100.0 * 100.0 + add_qty * 96.0) / (100.0 + add_qty)
    assert first["entry_price"] == pytest.approx(blend)


def test_partial_close_pro_rates_against_grown_initial_quantity():
    closes = [100, 100, 96, 96, 102.5, 102.5, 102.5]
    df = _df(closes, [1, 0, 1, 0, 0, 0, 0], atr=[2.0] * 7)
    tiers = [
        {"atr_multiple": 1.0, "close_fraction": 0.5},
        {"atr_multiple": 10.0, "close_fraction": 1.0},
    ]
    res = _run(
        df, allow_scale_in=True,
        close_strategies=[{"name": "tiered_tp_atr",
                           "params": {"tp_tiers": tiers, "atr_source": "entry"}}],
        scale_in={"add_spacing_atr": -1.0},
    )
    assert res["scale_in_adds"] == 1
    total = 100.0 + 10000.0 / 96.0
    first = res["trades"][0]
    # Tier 1 closes 50% of the GROWN initial quantity.
    assert first["shares"] == pytest.approx(total * 0.5)


# ─── Fees ─────────────────────────────────────────────────────────────────────


def test_add_pays_commission_and_joins_entry_fee_pool():
    fee = 0.001
    closes = [100, 100, 100, 100, 100, 100]
    df = _df(closes, [1, 0, 1, 0, -1, 0])
    res = _run(df, allow_scale_in=True, commission_pct=fee)
    assert res["scale_in_adds"] == 1
    (trade,) = res["trades"]
    # Entry fee pool = open commission + add commission, pro-rated 100%
    # onto the single close leg (#1241).
    open_invest = 10000.0
    open_comm = open_invest * fee
    open_shares = (open_invest - open_comm) / 100.0
    add_notional = open_shares * 100.0  # first-leg committed notional
    add_comm = add_notional * fee
    assert trade["entry_fee"] == pytest.approx(open_comm + add_comm, rel=1e-6)


def test_entry_fee_conserves_partial_close_then_add():
    # PR #1323 review: pool × leg/IQ stops conserving when an add follows a
    # partial close at a different price — the final leg must true-up so
    # Σ(netted entry fees) == open commission + Σ(add commissions).
    fee = 0.001
    closes = [100.0, 100.0, 102.5, 102.5, 104.0, 104.0]
    df = _df(closes, [1, 0, 0, 1, 0, 0], atr=[2.0] * 6)
    tiers = [
        {"atr_multiple": 1.0, "close_fraction": 0.5},
        {"atr_multiple": 10.0, "close_fraction": 1.0},
    ]
    res = _run(
        df, allow_scale_in=True, commission_pct=fee,
        close_strategies=[{"name": "tiered_tp_atr",
                           "params": {"tp_tiers": tiers, "atr_source": "entry"}}],
    )
    assert res["scale_in_adds"] == 1
    open_comm = 10000.0 * fee
    shares0 = (10000.0 - open_comm) / 100.0
    base_notional = shares0 * 100.0
    add_qty = base_notional / 102.5          # decision price = bar 3's close
    add_comm = add_qty * 104.0 * fee         # filled at bar 4's open
    legs = res["trades"]
    assert len(legs) >= 2                    # tier-1 partial + final leg
    assert sum(t["entry_fee"] for t in legs) == pytest.approx(
        open_comm + add_comm, abs=1e-4)


def test_entry_fee_conserves_partial_add_partial_full():
    # ≥3 close legs with an interleaved add: partial → add → partial → final.
    fee = 0.001
    closes = [100.0, 100.0, 102.5, 102.5, 104.5, 104.5, 104.5]
    df = _df(closes, [1, 0, 0, 1, 0, 0, 0], atr=[2.0] * 7)
    tiers = [
        {"atr_multiple": 1.0, "close_fraction": 0.25},
        {"atr_multiple": 2.0, "close_fraction": 0.25},
        {"atr_multiple": 10.0, "close_fraction": 1.0},
    ]
    res = _run(
        df, allow_scale_in=True, commission_pct=fee,
        close_strategies=[{"name": "tiered_tp_atr",
                           "params": {"tp_tiers": tiers, "atr_source": "entry"}}],
    )
    assert res["scale_in_adds"] == 1
    legs = res["trades"]
    assert len(legs) >= 3
    open_comm = 10000.0 * fee
    shares0 = (10000.0 - open_comm) / 100.0
    add_qty = shares0 * 100.0 / 102.5
    add_comm = add_qty * 104.5 * fee
    assert sum(t["entry_fee"] for t in legs) == pytest.approx(
        open_comm + add_comm, abs=1e-4)


def test_two_adds_straddling_partial_close_pnl_reconciles():
    # Two adds straddling a partial close: Σ trade pnl (net of all fees)
    # must equal final_capital − initial_capital with fees on.
    fee = 0.001
    closes = [100.0, 100.0, 102.5, 102.5, 102.6, 103.0, 103.0]
    df = _df(closes, [1, 0, 1, 0, 1, 0, 0], atr=[2.0] * 7)
    tiers = [
        {"atr_multiple": 1.0, "close_fraction": 0.5},
        {"atr_multiple": 10.0, "close_fraction": 1.0},
    ]
    res = _run(
        df, allow_scale_in=True, commission_pct=fee,
        close_strategies=[{"name": "tiered_tp_atr",
                           "params": {"tp_tiers": tiers, "atr_source": "entry"}}],
    )
    assert res["scale_in_adds"] == 2
    total_pnl = sum(t["pnl"] for t in res["trades"])
    assert total_pnl == pytest.approx(
        res["final_capital"] - 10000.0, abs=0.05)


# ─── Counting / results parity ────────────────────────────────────────────────


def test_adds_create_no_trade_rows():
    closes = [100, 100, 100, 100, 100, 100]
    df = _df(closes, [1, 0, 1, 0, -1, 0])
    res = _run(df, allow_scale_in=True)
    assert res["scale_in_adds"] == 1
    assert len(res["trades"]) == 1  # one round trip, no add rows (#T parity)
    assert res["total_trades"] == 1


def test_second_position_starts_with_clean_scale_state():
    # Full close resets the anchor/counters: the next position's add gates
    # against ITS OWN first leg, and max_adds=1 is per-position.
    closes = [100, 100, 100, 100, 100, 100, 200, 200, 200, 200, 200, 200]
    #  pos1: open, add, close        pos2: open, add(allowed again), close
    sig = [1, 0, 1, 0, -1, 0, 1, 0, 1, 0, -1, 0]
    res = _run(_df(closes, sig), allow_scale_in=True,
               scale_in={"max_adds": 1})
    assert res["scale_in_adds"] == 2
    assert len(res["trades"]) == 2
    assert all(t["scale_in_adds"] == 1 for t in res["trades"])


# ─── Loader plumbing (--config parity) ────────────────────────────────────────


def _config(tmp_path, strategy):
    cfg = {"config_version": 16, "strategies": [strategy]}
    path = tmp_path / "config.json"
    path.write_text(json.dumps(cfg))
    return str(path)


def _hl_strategy(**over):
    sc = {
        "id": "hl-test", "type": "perps", "platform": "hyperliquid",
        "script": "shared_scripts/check_hyperliquid.py",
        "args": ["tema_cross", "BTC", "4h", "--mode", "paper"],
        "capital": 1000, "max_drawdown_pct": 50,
        "open_strategy": {"name": "tema_cross", "params": {}},
        "stop_loss_atr_mult": 2.0,
    }
    sc.update(over)
    return sc


def test_loader_threads_scale_in_fields(tmp_path):
    path = _config(tmp_path, _hl_strategy(
        allow_scale_in=True,
        scale_in={"max_adds": 3, "add_spacing_atr": -0.5},
    ))
    kwargs = run_backtest.load_strategy_config(path, "hl-test")
    assert kwargs["allow_scale_in"] is True
    assert kwargs["scale_in"] == {"max_adds": 3, "add_spacing_atr": -0.5}


def test_loader_defaults_off(tmp_path):
    path = _config(tmp_path, _hl_strategy())
    kwargs = run_backtest.load_strategy_config(path, "hl-test")
    assert kwargs["allow_scale_in"] is False
    assert kwargs["scale_in"] is None


def test_loader_rejects_enabled_hedge(tmp_path):
    path = _config(tmp_path, _hl_strategy(
        hedge={"enabled": True, "symbol": "ETH", "side": "inverse"},
    ))
    with pytest.raises(ValueError, match="HL-live-only.*#1159"):
        run_backtest.load_strategy_config(path, "hl-test")


def test_loader_allows_disabled_hedge(tmp_path):
    path = _config(tmp_path, _hl_strategy(
        hedge={"enabled": False, "symbol": "ETH", "side": "inverse"},
    ))
    assert run_backtest.load_strategy_config(path, "hl-test")["open_strategy"]["name"] == "tema_cross"


def test_loader_rejects_block_without_flag(tmp_path):
    path = _config(tmp_path, _hl_strategy(scale_in={"max_adds": 3}))
    with pytest.raises(ValueError, match="allow_scale_in"):
        run_backtest.load_strategy_config(path, "hl-test")


def test_loader_rejects_wrong_type(tmp_path):
    path = _config(tmp_path, _hl_strategy(type="spot", allow_scale_in=True))
    with pytest.raises(ValueError, match="perps/manual-only"):
        run_backtest.load_strategy_config(path, "hl-test")


def test_loader_rejects_wrong_platform(tmp_path):
    path = _config(tmp_path, _hl_strategy(platform="okx", allow_scale_in=True))
    with pytest.raises(ValueError, match="hyperliquid-only"):
        run_backtest.load_strategy_config(path, "hl-test")


def test_loader_rejects_live_static_scalar_sl(tmp_path):
    path = _config(tmp_path, _hl_strategy(
        allow_scale_in=True,
        args=["tema_cross", "BTC", "4h", "--mode", "live"],
        stop_loss_atr_mult=None, stop_loss_pct=5.0,
    ))
    with pytest.raises(ValueError, match="static scalar"):
        run_backtest.load_strategy_config(path, "hl-test")


def test_loader_accepts_live_trailing_sl(tmp_path):
    path = _config(tmp_path, _hl_strategy(
        allow_scale_in=True,
        args=["tema_cross", "BTC", "4h", "--mode", "live"],
        stop_loss_atr_mult=None, trailing_stop_atr_mult=2.0,
    ))
    kwargs = run_backtest.load_strategy_config(path, "hl-test")
    assert kwargs["allow_scale_in"] is True


def test_loader_preserves_1268_combo_reject(tmp_path):
    path = _config(tmp_path, _hl_strategy(
        allow_scale_in=True, risk_per_trade_pct=1.0,
    ))
    with pytest.raises(ValueError, match="allow_scale_in"):
        run_backtest.load_strategy_config(path, "hl-test")
