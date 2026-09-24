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


def test_equal_dollar_risk_across_differing_atr_stops():
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
    assert st * 2.0 == pytest.approx(sw * 4.0) == pytest.approx(100.0)


def test_stop_out_realizes_the_risked_dollars():
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
    closes = [100, 100, 100, 100, 100]
    signals = [1, 0, 0, -1, 0]
    res = _run(_df(closes, signals),
               risk_per_trade_pct=1.0, stop_loss_pct=0.02)
    assert res["trades"][0]["shares"] == pytest.approx(50.0)


def test_risk_fraction_capped_at_full_cash():
    closes = [100, 100, 100, 100, 100]
    signals = [1, 0, 0, -1, 0]
    res = _run(_df(closes, signals, atr=[0.5] * 5),
               risk_per_trade_pct=10.0, stop_loss_atr_mult=1.0)
    assert res["trades"][0]["shares"] == pytest.approx(100.0)
    assert res["risk_per_trade_pct"] == pytest.approx(10.0)


def test_no_usable_atr_skips_entry_fail_closed():
    closes = [100, 100, 100, 100, 100]
    signals = [1, 0, 1, 0, 0]
    res = _run(_df(closes, signals, atr=[np.nan] * 5),
               risk_per_trade_pct=1.0, stop_loss_atr_mult=1.0)
    assert res["total_trades"] == 0
    assert res["risk_sizing_skipped_entries"] == 2


def test_entry_taken_once_atr_becomes_available():
    closes = [100, 100, 100, 100, 100, 100]
    signals = [1, 0, 1, 0, -1, 0]
    atr = [np.nan, np.nan, 2.0, 2.0, 2.0, 2.0]
    res = _run(_df(closes, signals, atr=atr),
               risk_per_trade_pct=1.0, stop_loss_atr_mult=1.0)
    assert res["total_trades"] == 1
    assert res["trades"][0]["shares"] == pytest.approx(50.0)
    assert res["risk_sizing_skipped_entries"] == 1


def test_close_never_blocked_by_unresolvable_atr():
    closes = [100, 100, 100, 110, 110, 110]
    signals = [1, 0, 0, -1, 0, 0]
    atr = [2.0, 2.0, np.nan, np.nan, np.nan, np.nan]
    res = _run(_df(closes, signals, atr=atr),
               risk_per_trade_pct=1.0, stop_loss_atr_mult=1.0)
    assert res["total_trades"] == 1
    assert res["trades"][0]["exit_date"] is not None


def test_unset_field_keeps_full_notional_sizing():
    closes = [100, 100, 110, 110, 110]
    signals = [1, 0, -1, 0, 0]
    res = _run(_df(closes, signals, atr=[2.0] * 5), stop_loss_atr_mult=1.0)
    assert res["trades"][0]["shares"] == pytest.approx(100.0)
    assert "risk_per_trade_pct" not in res
    assert "risk_sizing_skipped_entries" not in res


def test_rejects_entry_fraction_column_combo():
    closes = [100, 100, 100]
    with pytest.raises(ValueError, match="entry_fraction"):
        _run(_df(closes, [1, 0, 0], atr=[2.0] * 3, entry_fraction=[0.5] * 3),
             risk_per_trade_pct=1.0, stop_loss_atr_mult=1.0)


@pytest.mark.parametrize(
    "kwargs,match",
    [
        ({"risk_per_trade_pct": 0.0, "stop_loss_atr_mult": 1.0}, r"\(0, 10\]"),
        ({"risk_per_trade_pct": 12.0, "stop_loss_atr_mult": 1.0}, r"\(0, 10\]"),
        ({"risk_per_trade_pct": 1.0,
          "stop_loss_atr_mult_regime": {"use_defaults": True}}, "regime-resolved"),
        ({"risk_per_trade_pct": 1.0, "stop_loss_margin_pct": 20.0},
         "stop_loss_margin_pct"),
        ({"risk_per_trade_pct": 1.0}, "explicit stop owner"),
    ],
)
def test_init_rejects_invalid_risk_sizing(kwargs, match):
    with pytest.raises(ValueError, match=match):
        Backtester(**kwargs)


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


@pytest.mark.parametrize(
    "overrides,drop_atr_mult,match",
    [
        ({"sizing_leverage": 2.0}, False, "sizing_leverage"),
        ({"margin_per_trade_usd": 50.0}, False, "margin_per_trade_usd"),
        ({"allow_scale_in": True}, False, "allow_scale_in"),
        ({"stop_loss_pct": 2.0}, True, "fraction-denominated"),
    ],
)
def test_config_rejects_invalid_risk_sizing_combo(tmp_path, overrides,
                                                  drop_atr_mult, match):
    sc = _risk_strategy(**overrides)
    if drop_atr_mult:
        del sc["stop_loss_atr_mult"]
    path = _write_config(tmp_path, sc)
    with pytest.raises(ValueError, match=match):
        run_backtest.load_strategy_config(path, "hl-r-btc")


def test_config_explicit_zero_stop_owner_rejects(tmp_path):
    for owner in ("stop_loss_pct", "trailing_stop_pct", "stop_loss_margin_pct",
                  "stop_loss_atr_mult", "trailing_stop_atr_mult"):
        sc = _risk_strategy(**{owner: 0})
        if owner != "stop_loss_atr_mult":
            del sc["stop_loss_atr_mult"]
        path = _write_config(tmp_path, sc)
        kwargs = run_backtest.load_strategy_config(path, "hl-r-btc")
        assert kwargs[owner] == 0, owner
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
    sc = _risk_strategy()
    del sc["stop_loss_atr_mult"]
    path = _write_config(tmp_path, sc)
    kwargs = run_backtest.load_strategy_config(path, "hl-r-btc")
    assert kwargs["stop_loss_atr_mult"] == 1.0

    path = _write_config(tmp_path, sc, extra={"default_stop_loss_atr_mult": 2.5})
    kwargs = run_backtest.load_strategy_config(path, "hl-r-btc")
    assert kwargs["stop_loss_atr_mult"] == 2.5

    path = _write_config(tmp_path, sc, extra={"default_stop_loss_atr_mult": 0})
    with pytest.raises(ValueError, match="no stop owner"):
        run_backtest.load_strategy_config(path, "hl-r-btc")
