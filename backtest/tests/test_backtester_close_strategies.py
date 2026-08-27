import pandas as pd
import pytest

from backtester import Backtester


def _df_open_then_hold(opens, closes, atrs=None):
    n = len(closes)
    idx = pd.date_range("2024-01-01", periods=n, freq="D")
    open_actions = ["long"] + ["none"] * (n - 1)
    data = {"open": opens, "close": closes, "open_action": open_actions}
    if atrs is not None:
        data["atr"] = atrs
    return pd.DataFrame(data, index=idx)


def test_tp_at_pct_closes_full_position_when_threshold_hit():
    df = _df_open_then_hold(
        opens=[100, 100, 100, 103, 103],
        closes=[100, 100, 103, 103, 103],
    )
    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        close_strategies=[{"name": "tp_at_pct", "params": {"pct": 0.03}}],
    )
    result = bt.run(df, save=False)

    assert result["total_trades"] == 1
    assert result["trades"][0]["side"] == "long"
    assert result["trades"][0]["entry_price"] == 100.0
    assert result["trades"][0]["exit_price"] == 103.0
    assert result["final_capital"] == 1030.0


def test_tp_at_pct_does_not_fire_when_threshold_not_hit():
    df = _df_open_then_hold(
        opens=[100, 100, 100, 101, 101],
        closes=[100, 100, 101, 101, 101],
    )
    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        close_strategies=[{"name": "tp_at_pct", "params": {"pct": 0.03}}],
    )
    result = bt.run(df, save=False)
    assert result["total_trades"] == 1
    assert result["trades"][0]["exit_price"] == 101.0


def test_tiered_tp_atr_partial_then_full_close():
    df = _df_open_then_hold(
        opens=[100, 100, 100, 110, 120],
        closes=[100, 100, 110, 120, 120],
        atrs=[10, 10, 10, 10, 10],
    )
    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        close_strategies=[
            {"name": "tiered_tp_atr", "params": {"tp_tiers": [
            {"atr_multiple": 1.0, "close_fraction": 0.5},
            {"atr_multiple": 2.0, "close_fraction": 1.0},
        ]}},
        ],
    )
    result = bt.run(df, save=False)

    assert result["total_trades"] == 2
    assert result["trades"][0]["shares"] == 5.0
    assert result["trades"][0]["exit_price"] == 110.0
    assert result["trades"][1]["shares"] == 5.0
    assert result["trades"][1]["exit_price"] == 120.0
    assert result["final_capital"] == 1150.0


def test_tiered_tp_atr_live_uses_live_atr_from_market():
    df = _df_open_then_hold(
        opens=[100, 100, 100, 110, 120],
        closes=[100, 100, 110, 120, 120],
        atrs=[10, 10, 10, 10, 10],
    )
    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        close_strategies=[
            {"name": "tiered_tp_atr_live", "params": {
            "atr_source": "live",
            "tp_tiers": [
                {"atr_multiple": 1.0, "close_fraction": 0.5},
                {"atr_multiple": 2.0, "close_fraction": 1.0},
            ],
        }},
        ],
    )
    result = bt.run(df, save=False)

    assert result["total_trades"] == 2
    assert result["trades"][0]["exit_price"] == 110.0
    assert result["trades"][1]["exit_price"] == 120.0
    assert result["final_capital"] == 1150.0


def test_max_close_fraction_wins_between_two_evaluators():
    df = _df_open_then_hold(
        opens=[100, 100, 100, 102, 102],
        closes=[100, 100, 102, 102, 102],
    )
    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        close_strategies=[
            {"name": "tp_at_pct", "params": {"pct": 0.02}},
            {"name": "tiered_tp_pct", "params": {"tp_tiers": [
                {"profit_pct": 0.05, "close_fraction": 1.0},
            ]}},
        ],
    )
    result = bt.run(df, save=False)

    assert result["total_trades"] == 1
    assert result["trades"][0]["exit_price"] == 102.0
    assert result["final_capital"] == 1020.0


def test_close_strategies_unset_preserves_legacy_close_fraction_behavior():
    idx = pd.date_range("2024-01-01", periods=4, freq="D")
    df = pd.DataFrame({
        "open": [100, 100, 110, 110],
        "close": [100, 110, 110, 110],
        "open_action": ["long", "none", "none", "none"],
        "close_fraction": [0, 0, 1.0, 0],
    }, index=idx)

    bt = Backtester(initial_capital=1000, commission_pct=0, slippage_pct=0)
    result = bt.run(df, save=False)

    assert result["total_trades"] == 1
    assert result["trades"][0]["exit_price"] == 110.0
    assert result["final_capital"] == 1100.0


def test_close_strategy_short_position_long_take_profit():
    n = 5
    idx = pd.date_range("2024-01-01", periods=n, freq="D")
    df = pd.DataFrame({
        "open": [100, 100, 100, 97, 97],
        "close": [100, 100, 97, 97, 97],
        "open_action": ["short", "none", "none", "none", "none"],
    }, index=idx)
    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        close_strategies=[{"name": "tp_at_pct", "params": {"pct": 0.03}}],
    )
    result = bt.run(df, save=False)

    assert result["total_trades"] == 1
    assert result["trades"][0]["side"] == "short"
    assert result["trades"][0]["entry_price"] == 100.0
    assert result["trades"][0]["exit_price"] == 97.0
    assert result["final_capital"] == 1030.0


def test_starting_long_seed_with_entry_atr_lets_tiered_tp_atr_fire():
    n = 3
    idx = pd.date_range("2024-01-01", periods=n, freq="D")
    df = pd.DataFrame({
        "open":  [100, 110, 120],
        "close": [110, 120, 120],
        "atr":   [10,  10,  10],
        "open_action": ["none", "none", "none"],
    }, index=idx)
    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        close_strategies=[
            {"name": "tiered_tp_atr", "params": {"tp_tiers": [
            {"atr_multiple": 1.0, "close_fraction": 0.5},
            {"atr_multiple": 2.0, "close_fraction": 1.0},
        ]}},
        ],
    )
    result = bt.run(
        df, save=False,
        starting_long={"entry_price": 100.0, "entry_atr": 10.0},
    )
    assert result["total_trades"] == 2
    assert result["trades"][0]["exit_price"] == 110.0
    assert result["trades"][0]["shares"] == 5.0
    assert result["trades"][1]["exit_price"] == 120.0
    assert result["trades"][1]["shares"] == 5.0
    assert result["final_capital"] == 1150.0


def test_starting_long_seed_without_entry_atr_atr_evaluator_noops():
    n = 3
    idx = pd.date_range("2024-01-01", periods=n, freq="D")
    df = pd.DataFrame({
        "open":  [100, 110, 120],
        "close": [110, 120, 120],
        "atr":   [10,  10,  10],
        "open_action": ["none", "none", "none"],
    }, index=idx)
    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        close_strategies=[{"name": "tiered_tp_atr"}],
    )
    result = bt.run(
        df, save=False,
        starting_long={"entry_price": 100.0},
    )
    assert result["total_trades"] == 1
    assert result["trades"][0]["exit_price"] == 120.0


def test_trailing_tp_ratchet_trail_only_tier_exits_on_tightened_trail():
    idx = pd.date_range("2024-01-01", periods=6, freq="D")
    df = pd.DataFrame({
        "open": [100, 100, 100, 110, 99, 120],
        "close": [100, 100, 110, 99, 120, 120],
        "atr": [10, 10, 10, 10, 10, 10],
        "open_action": ["long", "none", "none", "none", "none", "none"],
    }, index=idx)
    bt = Backtester(
        intrabar_resolution="bar_close",
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        trailing_stop_atr_mult=3.0,
        close_strategies=[{"name": "trailing_tp_ratchet", "params": {
            "tp_tiers": [
                {"atr_multiple": 1.0, "close_fraction": 0.0, "trailing_mult_after": 1.0},
            ],
        }}],
    )
    result = bt.run(df, save=False)

    assert result["total_trades"] == 1
    assert result["trades"][0]["exit_date"] == str(idx[4])
    assert result["trades"][0]["exit_price"] == 99.0


def test_trailing_tp_ratchet_regime_uses_open_time_regime():
    idx = pd.date_range("2024-01-01", periods=6, freq="D")
    df = pd.DataFrame({
        "open": [100, 100, 100, 110, 99, 120],
        "close": [100, 100, 110, 99, 120, 120],
        "atr": [10, 10, 10, 10, 10, 10],
        "regime": ["ranging", "ranging", "trending_up", "trending_up", "trending_up", "trending_up"],
        "open_action": ["long", "none", "none", "none", "none", "none"],
    }, index=idx)
    close_ref = {
        "name": "trailing_tp_ratchet_regime",
        "params": {"tp_tiers": {
            "ranging": [
                {"atr_multiple": 1.0, "close_fraction": 0.0, "trailing_mult_after": 1.0},
            ],
            "trending_up": [
                {"atr_multiple": 99.0, "close_fraction": 0.0, "trailing_mult_after": 1.0},
            ],
            "trending_down": [
                {"atr_multiple": 99.0, "close_fraction": 0.0, "trailing_mult_after": 1.0},
            ],
        }},
    }
    bt = Backtester(
        intrabar_resolution="bar_close",
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        trailing_stop_atr_regime={"trend_regime": {
            "ranging": {"atr_multiple": 3.0},
            "trending_up": {"atr_multiple": 3.0},
            "trending_down": {"atr_multiple": 3.0},
        }},
        close_strategies=[close_ref],
    )
    result = bt.run(df, save=False)

    assert result["total_trades"] == 1
    assert result["trades"][0]["exit_date"] == str(idx[4])
    assert result["trades"][0]["exit_price"] == 99.0


def test_close_strategy_unknown_name_raises():
    try:
        Backtester(
            initial_capital=1000, commission_pct=0, slippage_pct=0,
            close_strategies=[{"name": "does_not_exist"}],
        )
    except ValueError as exc:
        assert "does_not_exist" in str(exc)
    else:
        raise AssertionError("expected ValueError for unknown close strategy")


_FAR_TP = [{"name": "tp_at_pct", "params": {"pct": 0.5}}]


def test_scalar_atr_stop_fires_alongside_close_evaluator():
    df = _df_open_then_hold(
        opens=[100, 100, 100, 95, 95],
        closes=[100, 100, 96, 95, 95],
        atrs=[2.0] * 5,
    )
    bt = Backtester(
        intrabar_resolution="bar_close",
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        close_strategies=_FAR_TP, stop_loss_atr_mult=1.0,
    )
    result = bt.run(df, save=False)
    assert result["total_trades"] == 1
    assert result["trades"][0]["exit_price"] == 95.0
    assert result["final_capital"] == 950.0


def test_scalar_atr_stop_inverse_no_breach_is_noop():
    df = _df_open_then_hold(
        opens=[100, 100, 100, 99, 99],
        closes=[100, 100, 99, 99, 99],
        atrs=[2.0] * 5,
    )
    kw = dict(initial_capital=1000, commission_pct=0, slippage_pct=0,
              close_strategies=_FAR_TP)
    with_stop = Backtester(stop_loss_atr_mult=1.0, **kw).run(df.copy(), save=False)
    no_stop = Backtester(**kw).run(df.copy(), save=False)
    assert with_stop["final_capital"] == no_stop["final_capital"]
    assert with_stop["total_trades"] == no_stop["total_trades"]


def test_scalar_trailing_stop_walks_alongside_close_evaluator():
    df = _df_open_then_hold(
        opens=[100, 100, 106, 106, 103, 103],
        closes=[100, 106, 106, 103, 103, 103],
        atrs=[2.0] * 6,
    )
    bt = Backtester(
        intrabar_resolution="bar_close",
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        close_strategies=_FAR_TP, trailing_stop_atr_mult=1.0,
    )
    result = bt.run(df, save=False)
    assert result["total_trades"] == 1
    assert result["trades"][0]["exit_price"] == 103.0
    assert result["final_capital"] == 1030.0


def test_scalar_atr_stop_protects_short_side():
    n = 5
    idx = pd.date_range("2024-01-01", periods=n, freq="D")
    df = pd.DataFrame({
        "open": [100, 100, 100, 103, 103],
        "close": [100, 100, 103, 103, 103],
        "atr": [2.0] * n,
        "open_action": ["short"] + ["none"] * (n - 1),
    }, index=idx)
    bt = Backtester(
        intrabar_resolution="bar_close",
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        close_strategies=_FAR_TP, stop_loss_atr_mult=1.0,
    )
    result = bt.run(df, save=False)
    assert result["total_trades"] == 1
    assert result["trades"][0]["side"] == "short"
    assert result["trades"][0]["exit_price"] == 103.0
    assert result["final_capital"] == 970.0


def test_pct_stop_fires_alongside_close_evaluator():
    df = _df_open_then_hold(
        opens=[100, 100, 100, 95, 95],
        closes=[100, 100, 96, 95, 95],
        atrs=[2.0] * 5,
    )
    bt = Backtester(
        intrabar_resolution="bar_close",
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        close_strategies=_FAR_TP, stop_loss_pct=0.02,
    )
    result = bt.run(df, save=False)
    assert result["total_trades"] == 1
    assert result["final_capital"] == 950.0


def test_tp_tier_partial_then_scalar_stop_closes_remainder():
    df = _df_open_then_hold(
        opens=[100, 100, 100, 102, 102, 96, 96],
        closes=[100, 100, 102, 102, 96, 96, 96],
        atrs=[2.0] * 7,
    )
    bt = Backtester(
        intrabar_resolution="bar_close",
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        close_strategies=[{"name": "tiered_tp_atr", "params": {
            "tp_tiers": [{"atr_multiple": 1.0, "close_fraction": 0.5}],
        }}],
        stop_loss_atr_mult=1.0,
    )
    result = bt.run(df, save=False)
    assert result["total_trades"] == 2
    exits = sorted(t["exit_price"] for t in result["trades"])
    assert exits == [96.0, 102.0]
    assert result["final_capital"] == 5 * 102.0 + 5 * 96.0


def test_seeded_position_fixed_atr_stop_fires_plain_path():
    n = 3
    idx = pd.date_range("2024-01-01", periods=n, freq="D")
    df = pd.DataFrame({
        "open": [100, 95, 95],
        "high": [100, 95, 95],
        "low": [95, 95, 95],
        "close": [95, 95, 95],
        "signal": [0] * n,
    }, index=idx)
    bt = Backtester(
        intrabar_resolution="bar_close",
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        stop_loss_atr_mult=2.0,
    )
    result = bt.run(
        df, save=False,
        starting_long={"entry_price": 100.0, "entry_atr": 2.0},
    )
    assert result["total_trades"] == 1
    assert result["trades"][0]["exit_price"] == 95.0
    assert result["final_capital"] == 950.0


def test_seeded_position_trailing_stop_anchors_at_seed_high_water():
    n = 3
    idx = pd.date_range("2024-01-01", periods=n, freq="D")
    df = pd.DataFrame({
        "open": [105, 105, 120],
        "high": [105, 120, 120],
        "low": [105, 105, 120],
        "close": [105, 120, 120],
        "signal": [0] * n,
    }, index=idx)
    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        trailing_stop_atr_mult=2.0,
    )
    result = bt.run(
        df, save=False,
        starting_long={"entry_price": 100.0, "entry_atr": 2.0,
                       "high_water": 110.0},
    )
    assert result["total_trades"] == 1
    assert result["trades"][0]["exit_price"] == 105.0


def test_seeded_position_fixed_atr_stop_fires_engine_path():
    n = 3
    idx = pd.date_range("2024-01-01", periods=n, freq="D")
    df = pd.DataFrame({
        "open": [100, 95, 95],
        "close": [95, 95, 95],
        "atr": [2.0] * n,
        "open_action": ["none"] * n,
    }, index=idx)
    bt = Backtester(
        intrabar_resolution="bar_close",
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        close_strategies=_FAR_TP, stop_loss_atr_mult=2.0,
    )
    result = bt.run(
        df, save=False,
        starting_long={"entry_price": 100.0, "entry_atr": 2.0},
    )
    assert result["total_trades"] == 1
    assert result["trades"][0]["exit_price"] == 95.0
    assert result["final_capital"] == 950.0


def test_seeded_position_without_entry_atr_stop_stays_unarmed():
    n = 3
    idx = pd.date_range("2024-01-01", periods=n, freq="D")
    df = pd.DataFrame({
        "open": [100, 95, 95],
        "high": [100, 95, 95],
        "low": [95, 95, 95],
        "close": [95, 95, 95],
        "signal": [0] * n,
    }, index=idx)
    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        stop_loss_atr_mult=2.0,
    )
    result = bt.run(
        df, save=False,
        starting_long={"entry_price": 100.0},
    )
    assert result["total_trades"] == 1
    assert result["trades"][0]["exit_date"] == str(idx[-1])


def _df_avwap_hold(opens, closes, avwaps, atrs):
    n = len(closes)
    idx = pd.date_range("2024-01-01", periods=n, freq="D")
    return pd.DataFrame({
        "open": opens, "close": closes,
        "open_action": ["long"] + ["none"] * (n - 1),
        "avwap": avwaps, "atr": atrs,
    }, index=idx)


def test_avwap_stop_fires_on_loss_of_line():
    df = _df_avwap_hold(
        opens=[100, 100, 100, 95, 95],
        closes=[100, 100, 95, 95, 95],
        avwaps=[100.0] * 5,
        atrs=[2.0] * 5,
    )
    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        close_strategies=[{"name": "avwap_stop", "params": {"buffer_atr_mult": 0.5}}],
    )
    result = bt.run(df, save=False)
    assert result["total_trades"] == 1
    assert result["trades"][0]["entry_price"] == 100.0
    assert result["trades"][0]["exit_price"] == 95.0


def test_avwap_stop_holds_above_buffered_line():
    df = _df_avwap_hold(
        opens=[100, 100, 100, 100, 100],
        closes=[100, 100, 99.5, 99.5, 99.5],
        avwaps=[100.0] * 5,
        atrs=[2.0] * 5,
    )
    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        close_strategies=[{"name": "avwap_stop", "params": {"buffer_atr_mult": 0.5}}],
    )
    result = bt.run(df, save=False)
    assert result["total_trades"] == 1
    assert result["trades"][0]["exit_price"] == 99.5


def test_avwap_stop_noops_without_avwap_column():
    df = _df_open_then_hold(
        opens=[100, 100, 100, 95, 95],
        closes=[100, 100, 95, 95, 95],
        atrs=[2.0] * 5,
    )
    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        close_strategies=[{"name": "avwap_stop", "params": {"buffer_atr_mult": 0.5}}],
    )
    result = bt.run(df, save=False)
    assert result["total_trades"] == 1
    assert result["trades"][0]["exit_price"] == 95.0
    assert "avwap_stop" not in str(result["trades"][0].get("exit_reason", ""))


def test_avwap_stop_short_side_fires_on_reclaim():
    df = pd.DataFrame({
        "open": [100, 100, 100, 105, 105],
        "close": [100, 100, 105, 105, 105],
        "open_action": ["short"] + ["none"] * 4,
        "avwap": [100.0] * 5,
        "atr": [2.0] * 5,
    }, index=pd.date_range("2024-01-01", periods=5, freq="D"))
    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        close_strategies=[{"name": "avwap_stop", "params": {"buffer_atr_mult": 0.5}}],
    )
    result = bt.run(df, save=False)
    assert result["total_trades"] == 1
    assert result["trades"][0]["side"] == "short"
    assert result["trades"][0]["exit_price"] == 105.0


_AVWAP_WARN_MARK = "avwap_stop"


def _run_avwap_stop_backtest(df, close_strategies):
    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        close_strategies=close_strategies,
    )
    return bt.run(df, save=False)


def test_avwap_stop_warns_once_when_column_absent(capsys):
    df = _df_open_then_hold(
        opens=[100, 100, 100, 95, 95],
        closes=[100, 100, 95, 95, 95],
        atrs=[2.0] * 5,
    )
    _run_avwap_stop_backtest(df, [{"name": "avwap_stop", "params": {"buffer_atr_mult": 0.5}}])
    err = capsys.readouterr().err
    assert err.count(_AVWAP_WARN_MARK) == 1


def test_avwap_stop_warns_once_when_column_all_nan(capsys):
    df = _df_avwap_hold(
        opens=[100, 100, 100, 95, 95],
        closes=[100, 100, 95, 95, 95],
        avwaps=[float("nan")] * 5,
        atrs=[2.0] * 5,
    )
    _run_avwap_stop_backtest(df, [{"name": "avwap_stop", "params": {"buffer_atr_mult": 0.5}}])
    err = capsys.readouterr().err
    assert err.count(_AVWAP_WARN_MARK) == 1


def test_avwap_stop_does_not_warn_when_line_usable(capsys):
    df = _df_avwap_hold(
        opens=[100, 100, 100, 95, 95],
        closes=[100, 100, 95, 95, 95],
        avwaps=[100.0] * 5,
        atrs=[2.0] * 5,
    )
    _run_avwap_stop_backtest(df, [{"name": "avwap_stop", "params": {"buffer_atr_mult": 0.5}}])
    err = capsys.readouterr().err
    assert _AVWAP_WARN_MARK not in err


def test_avwap_stop_does_not_warn_when_not_configured(capsys):
    df = _df_open_then_hold(
        opens=[100, 100, 100, 103, 103],
        closes=[100, 100, 103, 103, 103],
    )
    _run_avwap_stop_backtest(df, [{"name": "tp_at_pct", "params": {"pct": 0.03}}])
    err = capsys.readouterr().err
    assert _AVWAP_WARN_MARK not in err


_UNIFIED_CLOSE = {
    "name": "tiered_tp_atr_regime",
    "params": {"trend_regime": {
        "ranging": {
            "tp_tiers": [{"atr_multiple": 98.0, "close_fraction": 0.5},
                         {"atr_multiple": 99.0, "close_fraction": 1.0}],
            "stop_loss_atr": 1.0,
        },
        "trending_up": {
            "tp_tiers": [{"atr_multiple": 98.0, "close_fraction": 0.5},
                         {"atr_multiple": 99.0, "close_fraction": 1.0}],
            "stop_loss_atr": 2.0,
        },
        "trending_down": {
            "tp_tiers": [{"atr_multiple": 98.0, "close_fraction": 0.5},
                         {"atr_multiple": 99.0, "close_fraction": 1.0}],
            "stop_loss_atr": 2.0,
        },
    }},
}


def _df_unified():
    idx = pd.date_range("2024-01-01", periods=6, freq="D")
    return pd.DataFrame({
        "open": [100, 100, 100, 95, 95, 95],
        "close": [100, 100, 96, 95, 95, 95],
        "atr": [2, 2, 2, 2, 2, 2],
        "regime": ["ranging"] * 6,
        "open_action": ["long", "none", "none", "none", "none", "none"],
    }, index=idx)


def test_unified_regime_close_arms_per_regime_stop_loss():
    bt = Backtester(
        intrabar_resolution="bar_close",
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        close_strategies=[_UNIFIED_CLOSE],
    )
    result = bt.run(_df_unified(), save=False)
    assert result["total_trades"] == 1
    assert result["trades"][0]["exit_price"] == 95.0
    assert result["trades"][0]["exit_date"] == str(_df_unified().index[3])


def test_unified_regime_close_no_stop_without_breach():
    idx = pd.date_range("2024-01-01", periods=5, freq="D")
    df = pd.DataFrame({
        "open": [100, 100, 100, 99, 99],
        "close": [100, 100, 99, 99, 99],
        "atr": [2, 2, 2, 2, 2],
        "regime": ["ranging"] * 5,
        "open_action": ["long", "none", "none", "none", "none"],
    }, index=idx)
    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        close_strategies=[_UNIFIED_CLOSE],
    )
    result = bt.run(df, save=False)
    assert result["total_trades"] == 1
    assert result["trades"][0]["exit_date"] == str(idx[4])


def test_unified_regime_close_rejects_second_sl_owner():
    with pytest.raises(ValueError, match="unified per-regime close"):
        Backtester(
            initial_capital=1000, commission_pct=0, slippage_pct=0,
            stop_loss_atr_mult=1.5,
            close_strategies=[_UNIFIED_CLOSE],
        )
    with pytest.raises(ValueError, match="unified per-regime close"):
        Backtester(
            initial_capital=1000, commission_pct=0, slippage_pct=0,
            stop_loss_atr_regime={"trend_regime": {
                "ranging": {"atr_multiple": 1.0},
                "trending_up": {"atr_multiple": 1.0},
                "trending_down": {"atr_multiple": 1.0},
            }},
            close_strategies=[_UNIFIED_CLOSE],
        )


_COMPOSITE_SPEC_1228 = {"medium": {"classifier": "composite", "period": 21}}


def _unified_composite_block(bare_sl=1.0, include_bare_sl=True):
    far = [{"atr_multiple": 98.0, "close_fraction": 0.5},
           {"atr_multiple": 99.0, "close_fraction": 1.0}]
    bare = {"tp_tiers": [dict(t) for t in far]}
    if include_bare_sl:
        bare["stop_loss_atr"] = bare_sl
    block = {"ranging_directional": bare}
    for lab in ("ranging_quiet", "ranging_volatile", "trending_up_clean",
                "trending_up_choppy", "trending_down_clean",
                "trending_down_choppy"):
        block[lab] = {"tp_tiers": [dict(t) for t in far], "stop_loss_atr": 99.0}
    return block


def test_unified_regime_close_bare_block_arms_sl_for_directional_sub_stamp():
    close_ref = {
        "name": "tiered_tp_atr_regime",
        "params": {"trend_regime": _unified_composite_block(bare_sl=1.0)},
    }
    idx = pd.date_range("2024-01-01", periods=6, freq="D")
    df = pd.DataFrame({
        "open": [100, 100, 100, 95, 95, 95],
        "close": [100, 100, 96, 95, 95, 95],
        "atr": [2, 2, 2, 2, 2, 2],
        "regime": ["ranging_directional_up"] * 6,
        "open_action": ["long", "none", "none", "none", "none", "none"],
    }, index=idx)
    bt = Backtester(
        intrabar_resolution="bar_close",
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        regime_windows_spec=_COMPOSITE_SPEC_1228,
        close_strategies=[close_ref],
    )
    result = bt.run(df, save=False)
    assert result["total_trades"] == 1
    assert result["trades"][0]["exit_price"] == 95.0
    assert result["trades"][0]["exit_date"] == str(idx[3])


def _unified_adx_block(sl_overrides=None, drop_sl_for=()):
    far = [{"atr_multiple": 98.0, "close_fraction": 0.5},
           {"atr_multiple": 99.0, "close_fraction": 1.0}]
    block = {}
    for lab in ("ranging", "trending_up", "trending_down"):
        entry = {"tp_tiers": [dict(t) for t in far]}
        if lab not in drop_sl_for:
            entry["stop_loss_atr"] = (sl_overrides or {}).get(lab, 1.0)
        block[lab] = entry
    return {"name": "tiered_tp_atr_regime", "params": {"trend_regime": block}}


def test_unified_close_missing_stop_loss_atr_rejected_at_load():
    with pytest.raises(ValueError, match="stop_loss_atr"):
        Backtester(
            initial_capital=1000, commission_pct=0, slippage_pct=0,
            close_strategies=[_unified_adx_block(drop_sl_for=("trending_up",))],
        )


def test_unified_close_nonpositive_stop_loss_atr_rejected_at_load():
    with pytest.raises(ValueError, match="must be > 0"):
        Backtester(
            initial_capital=1000, commission_pct=0, slippage_pct=0,
            close_strategies=[_unified_adx_block(sl_overrides={"ranging": 0})],
        )
    with pytest.raises(ValueError, match="must be > 0"):
        Backtester(
            initial_capital=1000, commission_pct=0, slippage_pct=0,
            close_strategies=[_unified_adx_block(sl_overrides={"ranging": -1.5})],
        )


def test_unified_close_bare_block_without_sl_rejected_at_load():
    close_ref = {
        "name": "tiered_tp_atr_regime",
        "params": {"trend_regime": _unified_composite_block(
            include_bare_sl=False)},
    }
    with pytest.raises(ValueError, match="stop_loss_atr"):
        Backtester(
            initial_capital=1000, commission_pct=0, slippage_pct=0,
            regime_windows_spec=_COMPOSITE_SPEC_1228,
            close_strategies=[close_ref],
        )


def test_unified_close_single_tier_rejected_at_load():
    ref = _unified_adx_block()
    ref["params"]["trend_regime"]["ranging"]["tp_tiers"] = [
        {"atr_multiple": 2.0, "close_fraction": 1.0}]
    with pytest.raises(ValueError, match="at least 2 tiers"):
        Backtester(
            initial_capital=1000, commission_pct=0, slippage_pct=0,
            close_strategies=[ref],
        )
