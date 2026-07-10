"""#1268: opt-in risk-per-trade (fixed-fractional) position sizing.

With ``risk_per_trade_pct`` set, each open commits ``shares = (cash × pct/100)
/ stop_distance`` — the dollar risk realized on a stop-out is constant across
strategies with different stop distances, mirroring the live sizing formula.
These tests pin: constant dollar risk across differing ATR-mult stops, the
1×-cash cap (the backtester's no-leverage tightening of live's cash ×
exchange_leverage cap), the fail-closed entry skip when no usable ATR exists
at the signal bar, mutual exclusion with a strategy-emitted entry_fraction
column, init-time rejects mirroring the live validator, the ``--config``
loader gate, and byte-identical legacy sizing when the field is unset.
"""
import json
import sys
import pathlib

import numpy as np
import pandas as pd
import pytest

sys.path.insert(0, str(pathlib.Path(__file__).parent.parent.parent / "shared_tools"))
sys.path.insert(0, str(pathlib.Path(__file__).parent.parent))

from backtester import Backtester
import run_backtest


def _df(closes, signals, atr=None, entry_fraction=None):
    closes = np.asarray(closes, dtype=float)
    n = len(closes)
    idx = pd.date_range("2024-01-01", periods=n, freq="D")
    df = pd.DataFrame(
        {
            "open":   closes,
            "high":   closes + 0.4,
            "low":    closes - 0.4,
            "close":  closes,
            "volume": np.full(n, 1000.0),
            "signal": np.asarray(signals, dtype=float),
        },
        index=idx,
    )
    if atr is not None:
        df["atr"] = np.asarray(atr, dtype=float)
    if entry_fraction is not None:
        df["entry_fraction"] = np.asarray(entry_fraction, dtype=float)
    return df


def _run(df, **kw):
    kw.setdefault("commission_pct", 0.0)
    kw.setdefault("slippage_pct", 0.0)
    bt = Backtester(initial_capital=10000.0, **kw)
    return bt.run(df.copy(), strategy_name="x", symbol="BTC/USDT",
                  timeframe="1d", save=False)


# ─── Constant dollar risk (the core invariant) ────────────────────────────────


def test_equal_dollar_risk_across_differing_atr_stops():
    # cash 10000, risk 1% = $100/trade, price 100, ATR 2.
    # mult=1 → dist 2 → 50 shares; mult=2 → dist 4 → 25 shares.
    closes = [100, 100, 100, 100, 100]
    signals = [1, 0, 0, -1, 0]
    atr = [2.0] * 5
    tight = _run(_df(closes, signals, atr=atr),
                 risk_per_trade_pct=1.0, stop_loss_atr_mult=1.0)
    wide = _run(_df(closes, signals, atr=atr),
                risk_per_trade_pct=1.0, stop_loss_atr_mult=2.0)
    st = tight["trades"][0]["shares"]
    sw = wide["trades"][0]["shares"]
    assert st == pytest.approx(50.0)
    assert sw == pytest.approx(25.0)
    # shares × stop_distance = risked dollars, equal for both.
    assert st * 2.0 == pytest.approx(sw * 4.0) == pytest.approx(100.0)


def test_stop_out_realizes_the_risked_dollars():
    # Long 50 shares at 100 with a 1×ATR(2) stop at 98; bar 2 opens exactly at
    # the trigger so the OHLC walk fills at 98 — loss = exactly the $100 risked.
    closes = [100, 100, 98, 98, 98]
    signals = [1, 0, 0, 0, 0]
    res = _run(_df(closes, signals, atr=[2.0] * 5),
               risk_per_trade_pct=1.0, stop_loss_atr_mult=1.0)
    assert res["total_trades"] == 1
    assert res["trades"][0]["exit_reason"] == "signal_sl"
    assert res["final_capital"] == pytest.approx(10000.0 - 100.0)


def test_short_side_sizes_constant_dollar_risk():
    closes = [100, 100, 100, 100, 100]
    signals = [-1, 0, 0, 1, 0]
    res = _run(_df(closes, signals, atr=[2.0] * 5),
               risk_per_trade_pct=1.0, stop_loss_atr_mult=1.0,
               direction="short")
    assert res["total_trades"] == 1
    assert res["trades"][0]["side"] == "short"
    assert res["trades"][0]["shares"] == pytest.approx(50.0)


def test_pct_stop_owner_sizes_from_price_fraction():
    # This engine's stop_loss_pct is a FRACTION: 0.02 → dist = 2 at price 100.
    closes = [100, 100, 100, 100, 100]
    signals = [1, 0, 0, -1, 0]
    res = _run(_df(closes, signals),
               risk_per_trade_pct=1.0, stop_loss_pct=0.02)
    assert res["trades"][0]["shares"] == pytest.approx(50.0)


def test_risk_fraction_capped_at_full_cash():
    # risk 10% with dist 0.5 wants fraction 20 → capped at 1.0 (full cash;
    # the backtester models no leverage). 100 shares at price 100.
    closes = [100, 100, 100, 100, 100]
    signals = [1, 0, 0, -1, 0]
    res = _run(_df(closes, signals, atr=[0.5] * 5),
               risk_per_trade_pct=10.0, stop_loss_atr_mult=1.0)
    assert res["trades"][0]["shares"] == pytest.approx(100.0)
    assert res["risk_per_trade_pct"] == pytest.approx(10.0)


# ─── Fail-closed on unresolvable stop distance ────────────────────────────────


def test_no_usable_atr_skips_entry_fail_closed():
    # ATR is NaN at every signal bar → every entry skipped, none sized from a
    # notional fallback.
    closes = [100, 100, 100, 100, 100]
    signals = [1, 0, 1, 0, 0]
    res = _run(_df(closes, signals, atr=[np.nan] * 5),
               risk_per_trade_pct=1.0, stop_loss_atr_mult=1.0)
    assert res["total_trades"] == 0
    assert res["risk_sizing_skipped_entries"] == 2


def test_entry_taken_once_atr_becomes_available():
    # Signal bar 0 (ATR NaN) skipped; signal bar 2 (ATR 2) fills bar 3.
    closes = [100, 100, 100, 100, 100, 100]
    signals = [1, 0, 1, 0, -1, 0]
    atr = [np.nan, np.nan, 2.0, 2.0, 2.0, 2.0]
    res = _run(_df(closes, signals, atr=atr),
               risk_per_trade_pct=1.0, stop_loss_atr_mult=1.0)
    assert res["total_trades"] == 1
    assert res["trades"][0]["shares"] == pytest.approx(50.0)
    assert res["risk_sizing_skipped_entries"] == 1


def test_close_never_blocked_by_unresolvable_atr():
    # Open with ATR available; ATR disappears before the close signal — the
    # close must still execute (risk sizing gates entries only).
    closes = [100, 100, 100, 110, 110, 110]
    signals = [1, 0, 0, -1, 0, 0]
    atr = [2.0, 2.0, np.nan, np.nan, np.nan, np.nan]
    res = _run(_df(closes, signals, atr=atr),
               risk_per_trade_pct=1.0, stop_loss_atr_mult=1.0)
    assert res["total_trades"] == 1
    assert res["trades"][0]["exit_date"] is not None


# ─── Legacy behavior untouched ────────────────────────────────────────────────


def test_unset_field_keeps_full_notional_sizing():
    closes = [100, 100, 110, 110, 110]
    signals = [1, 0, -1, 0, 0]
    res = _run(_df(closes, signals, atr=[2.0] * 5), stop_loss_atr_mult=1.0)
    assert res["trades"][0]["shares"] == pytest.approx(100.0)  # full 10000/100
    assert "risk_per_trade_pct" not in res
    assert "risk_sizing_skipped_entries" not in res


# ─── Validation ───────────────────────────────────────────────────────────────


def test_rejects_entry_fraction_column_combo():
    closes = [100, 100, 100]
    with pytest.raises(ValueError, match="entry_fraction"):
        _run(_df(closes, [1, 0, 0], atr=[2.0] * 3, entry_fraction=[0.5] * 3),
             risk_per_trade_pct=1.0, stop_loss_atr_mult=1.0)


def test_rejects_out_of_bounds_pct():
    with pytest.raises(ValueError, match=r"\(0, 10\]"):
        Backtester(risk_per_trade_pct=0.0, stop_loss_atr_mult=1.0)
    with pytest.raises(ValueError, match=r"\(0, 10\]"):
        Backtester(risk_per_trade_pct=12.0, stop_loss_atr_mult=1.0)


def test_rejects_regime_resolved_stop_owner():
    with pytest.raises(ValueError, match="regime-resolved"):
        Backtester(
            risk_per_trade_pct=1.0,
            stop_loss_atr_regime={"use_defaults": True},
        )


def test_rejects_margin_pct_only_stop():
    with pytest.raises(ValueError, match="stop_loss_margin_pct"):
        Backtester(risk_per_trade_pct=1.0, stop_loss_margin_pct=20.0)


def test_rejects_missing_stop_owner():
    with pytest.raises(ValueError, match="explicit stop owner"):
        Backtester(risk_per_trade_pct=1.0)


# ─── load_strategy_config (--config) gate ─────────────────────────────────────


def _write_config(tmp_path, strategy, extra=None):
    cfg = {"config_version": 16, "strategies": [strategy]}
    cfg.update(extra or {})
    p = tmp_path / "config.json"
    p.write_text(json.dumps(cfg, indent=2))
    return str(p)


def _risk_strategy(**overrides):
    sc = {
        "id": "hl-r-btc",
        "type": "perps",
        "platform": "hyperliquid",
        "open_strategy": {"name": "tema_cross_bd"},
        "risk_per_trade_pct": 1.0,
        "stop_loss_atr_mult": 1.5,
    }
    sc.update(overrides)
    return sc


def test_config_threads_risk_per_trade_pct(tmp_path):
    path = _write_config(tmp_path, _risk_strategy())
    kwargs = run_backtest.load_strategy_config(path, "hl-r-btc")
    assert kwargs["risk_per_trade_pct"] == 1.0
    assert kwargs["stop_loss_atr_mult"] == 1.5


def test_config_rejects_sizing_leverage_combo(tmp_path):
    path = _write_config(tmp_path, _risk_strategy(sizing_leverage=2.0))
    with pytest.raises(ValueError, match="sizing_leverage"):
        run_backtest.load_strategy_config(path, "hl-r-btc")


def test_config_rejects_margin_per_trade_combo(tmp_path):
    path = _write_config(tmp_path, _risk_strategy(margin_per_trade_usd=50.0))
    with pytest.raises(ValueError, match="margin_per_trade_usd"):
        run_backtest.load_strategy_config(path, "hl-r-btc")


def test_config_rejects_scale_in_combo(tmp_path):
    path = _write_config(tmp_path, _risk_strategy(allow_scale_in=True))
    with pytest.raises(ValueError, match="allow_scale_in"):
        run_backtest.load_strategy_config(path, "hl-r-btc")


def test_config_rejects_pct_stop_owner(tmp_path):
    # Live pct owners are percent-denominated; this engine's are fractions —
    # sizing from them would skew 100×, so the loader rejects loudly.
    sc = _risk_strategy(stop_loss_pct=2.0)
    del sc["stop_loss_atr_mult"]
    path = _write_config(tmp_path, sc)
    with pytest.raises(ValueError, match="fraction-denominated"):
        run_backtest.load_strategy_config(path, "hl-r-btc")


def test_config_explicit_zero_stop_owner_rejects(tmp_path):
    # An explicit-zero stop owner (stop_loss_pct: 0 etc.) explicitly DISABLES
    # the stop, and live rejects the config at load ("no distance to size
    # risk against"). The loader must treat the present-but-zero key as an
    # owner (is not None) — never materialize a default ATR stop — so the
    # config rejects loudly at Backtester construction instead of silently
    # running a 1.0×ATR-stopped, risk-sized position live refuses (#1268).
    for owner in ("stop_loss_pct", "trailing_stop_pct", "stop_loss_margin_pct",
                  "stop_loss_atr_mult", "trailing_stop_atr_mult"):
        sc = _risk_strategy(**{owner: 0})
        if owner != "stop_loss_atr_mult":
            del sc["stop_loss_atr_mult"]
        path = _write_config(tmp_path, sc)
        kwargs = run_backtest.load_strategy_config(path, "hl-r-btc")
        # No default materialized — the zero owner is preserved as-is.
        assert kwargs[owner] == 0, owner
        # ...and the resulting sizing/stop combination rejects loudly.
        with pytest.raises(ValueError, match="stop"):
            Backtester(
                risk_per_trade_pct=kwargs["risk_per_trade_pct"],
                stop_loss_atr_mult=kwargs["stop_loss_atr_mult"],
                stop_loss_pct=kwargs["stop_loss_pct"],
                stop_loss_margin_pct=kwargs["stop_loss_margin_pct"],
                trailing_stop_atr_mult=kwargs["trailing_stop_atr_mult"],
                trailing_stop_pct=kwargs["trailing_stop_pct"],
            )


def test_config_materializes_default_stop_owner(tmp_path):
    # No stop-owner key: live materializes default_stop_loss_atr_mult (1.0)
    # at load, and risk sizing derives its distance from exactly that owner.
    sc = _risk_strategy()
    del sc["stop_loss_atr_mult"]
    path = _write_config(tmp_path, sc)
    kwargs = run_backtest.load_strategy_config(path, "hl-r-btc")
    assert kwargs["stop_loss_atr_mult"] == 1.0

    # Fleet override is honored...
    path = _write_config(tmp_path, sc, extra={"default_stop_loss_atr_mult": 2.5})
    kwargs = run_backtest.load_strategy_config(path, "hl-r-btc")
    assert kwargs["stop_loss_atr_mult"] == 2.5

    # ...and an explicit opt-out (=0) leaves no stop owner → reject.
    path = _write_config(tmp_path, sc, extra={"default_stop_loss_atr_mult": 0})
    with pytest.raises(ValueError, match="no stop owner"):
        run_backtest.load_strategy_config(path, "hl-r-btc")
