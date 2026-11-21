import json

import pandas as pd
import pytest

import exit_policy_ab as m


def _leg(entry_date="2025-01-01", side="long", pnl_pct=2.0, shares=1.0,
         entry_price=100.0, exit_price=102.0, entry_fee=0.1, exit_fee=0.1,
         mfe_pct=3.0, mae_pct=-1.0, bars_held=5, exit_reason="tp"):
    return {
        "entry_date": entry_date, "side": side, "pnl_pct": pnl_pct,
        "shares": shares, "entry_price": entry_price, "exit_price": exit_price,
        "entry_fee": entry_fee, "exit_fee": exit_fee, "mfe_pct": mfe_pct,
        "mae_pct": mae_pct, "bars_held": bars_held, "exit_reason": exit_reason,
    }


def test_build_paired_rows_pairs_counts_unmatched_and_labels_unknown_regime():
    control = [
        {"entry_date": "d1", "side": "long", "net_pct": 1.0, "mfe_pct": 2.0,
         "mae_pct": -1.0, "bars_held": 3},
        {"entry_date": "d2", "side": "long", "net_pct": -1.0, "mfe_pct": 0.5,
         "mae_pct": -2.0, "bars_held": 4},
    ]
    candidate_by_date = {
        "d1": {"net_pct": 2.5, "mfe_pct": 3.0, "mae_pct": -0.5, "bars_held": 6},
        "d2": None,
    }
    regime_by_date = {"d1": "ranging_quiet"}
    rows, diag = m.build_paired_rows(control, candidate_by_date, regime_by_date)
    assert diag == {"schedule_entries": 2, "paired": 1, "unmatched": 1}
    assert len(rows) == 1
    assert rows[0]["regime"] == "ranging_quiet"
    assert rows[0]["delta_net_pct"] == pytest.approx(1.5)

    control = [{"entry_date": "d1", "side": "long", "net_pct": 1.0, "mfe_pct": 1.0,
                "mae_pct": -1.0, "bars_held": 2}]
    rows, _ = m.build_paired_rows(
        control, {"d1": {"net_pct": 1.0, "mfe_pct": 1.0, "mae_pct": -1.0,
                         "bars_held": 2}}, {})
    assert rows[0]["regime"] == m.UNKNOWN_REGIME, "test_build_paired_rows_unknown_regime_label"


def _row(regime, ctrl, cand, mfe=2.0, mae=-1.0):
    return {"entry_date": "d", "regime": regime, "side": "long",
            "control_net_pct": ctrl, "candidate_net_pct": cand,
            "delta_net_pct": cand - ctrl, "control_mfe_pct": mfe,
            "candidate_mfe_pct": mfe, "control_mae_pct": mae,
            "candidate_mae_pct": mae, "control_bars_held": 3,
            "candidate_bars_held": 3}


def test_stops_from_kwargs_collects_present_drops_none_and_empty_without_stops():
    kwargs = {
        "open_strategy": {"name": "x", "params": {}},
        "close_strategies": [{"name": "tiered_tp_atr", "params": {}}],
        "stop_loss_atr_mult": 1.5,
        "stop_loss_pct": None,
        "stop_loss_margin_pct": None,
        "trailing_stop_atr_mult": None,
        "trailing_stop_pct": None,
        "stop_loss_atr_mult_regime": None,
        "trailing_stop_atr_mult_regime": {"trending_up": 2.0, "ranging_quiet": 3.0},
    }
    stops = m._stops_from_kwargs(kwargs)
    assert stops == {"stop_loss_atr_mult": 1.5,
                     "trailing_stop_atr_mult_regime": {"trending_up": 2.0, "ranging_quiet": 3.0}}
    assert all(v is not None for v in stops.values())

    assert m._stops_from_kwargs({"open_strategy": {}, "close_strategies": None}) == {}, \
        "test_stops_from_kwargs_empty_when_no_stops"


def test_backtester_kwargs_threads_present_stops_only_none_and_regime_trailing():
    case = "test_backtester_kwargs_threads_present_stops_only"
    kw = m._backtester_kwargs(
        "sqz", {}, [{"name": "tiered_tp_atr", "params": {}}], "long", 10000.0,
        {"allowed_regimes": None}, stops={"stop_loss_atr_mult": 1.5})
    assert kw["stop_loss_atr_mult"] == 1.5, case
    assert "trailing_stop_atr_mult" not in kw, case
    assert "stop_loss_pct" not in kw, case

    kw = m._backtester_kwargs("sqz", {}, None, "long", 10000.0,
                              {"allowed_regimes": None}, stops=None)
    assert not any(k in kw for k in m.STOP_FIELD_KEYS), \
        "test_backtester_kwargs_no_stops_means_no_stop_keys"

    case = "test_backtester_kwargs_threads_regime_trailing_stop_with_open_as_close"
    regime_trail = {"trending_up": 2.0, "ranging_quiet": 3.0}
    kw = m._backtester_kwargs("sqz", {}, None, "long", 10000.0,
                              {"allowed_regimes": None},
                              stops={"trailing_stop_atr_mult_regime": regime_trail})
    assert kw["close_strategies"] is None, case
    assert kw["trailing_stop_atr_mult_regime"] == regime_trail, case


def test_control_keeps_stop_regardless_of_candidate_mode(subtests):
    with subtests.test("test_control_keeps_stop_regardless_of_candidate_mode"):
        incumbent_stops = {"stop_loss_atr_mult": 1.5}
        control_kw = m._backtester_kwargs(
            "sqz", {}, [{"name": "tiered_tp_atr", "params": {}}], "long", 10000.0,
            {"allowed_regimes": None}, stops=incumbent_stops)
        assert control_kw["stop_loss_atr_mult"] == 1.5

        inherit_kw = m._backtester_kwargs(
            "sqz", {}, [{"name": "atr_stop", "params": {}}], "long", 10000.0,
            {"allowed_regimes": None},
            stops=m._candidate_stops("inherit", incumbent_stops))
        drop_kw = m._backtester_kwargs(
            "sqz", {}, [{"name": "atr_stop", "params": {}}], "long", 10000.0,
            {"allowed_regimes": None},
            stops=m._candidate_stops("drop", incumbent_stops))
        assert inherit_kw["stop_loss_atr_mult"] == 1.5
        assert "stop_loss_atr_mult" not in drop_kw
    with subtests.test("test_candidate_stops_inherit_copies_and_drop_clears"):
        incumbent = {"stop_loss_atr_mult": 1.5}
        inh = m._candidate_stops("inherit", incumbent)
        assert inh == incumbent and inh is not incumbent
        assert m._candidate_stops("drop", incumbent) == {}
        assert m._candidate_stops("inherit", None) == {}


def test_reject_unreplayable_entry_shapers(subtests):
    for case_id, incumbent in [
        ("invert_signal", {"invert_signal": True}),
        ("regime_directional_policy", {"regime_directional_policy": {"trending_up": "long"}}),
        ("profile_allocation", {"profile_allocation": {"window": "long", "param_sets": {}}}),
    ]:
        with subtests.test(case=case_id):
            with pytest.raises(SystemExit):
                m._reject_unreplayable_entry_shapers(incumbent)

    with subtests.test(case="test_no_reject_when_entry_shapers_absent_or_falsy"):
        m._reject_unreplayable_entry_shapers({"open_strategy": {"name": "x"}})
        m._reject_unreplayable_entry_shapers(
            {"invert_signal": False, "regime_directional_policy": None,
             "profile_allocation": {}})


def test_replay_positions_anchored_on_df_signals_not_df(monkeypatch):
    import data_fetcher

    idx = pd.date_range("2025-01-01", periods=10, freq="h")
    df = pd.DataFrame({"close": range(10)}, index=idx)
    df_signals = df.iloc[2:].copy()
    df_signals["signal"] = 0
    entry_ts = str(df.index[5])

    captured = {}

    def fake_prepare(reg, open_name, params, _df):
        return df_signals

    def fake_regime_series(frame, regime_cfg):
        captured["regime_frame_len"] = len(frame)
        return ["r"] * len(frame)

    def fake_run_free_arm(reg, open_name, params, sig, close_refs, direction,
                          capital, gate, symbol, timeframe, stops=None,
                          comparison_mode=None, perps_sizing=None):
        return {"total_trades": 1, "total_return_pct": 0.0, "max_drawdown_pct": 0.0,
                "sharpe_ratio": 0.0, "liquidated": False,
                "trades": [_leg(entry_date=entry_ts, side="long")]}

    def fake_replay(reg, open_name, params, sig, sig_pos, side_sign, candidate_close,
                    direction, capital, gate, symbol, timeframe, stops=None,
                    comparison_mode=None, perps_sizing=None):
        captured["sig_pos"] = sig_pos
        return {"net_pct": 1.0, "mfe_pct": 1.0, "mae_pct": -1.0, "bars_held": 2}

    monkeypatch.setattr(data_fetcher, "load_cached_data", lambda *a, **k: df)
    monkeypatch.setattr(m, "_prepare_signals", fake_prepare)
    monkeypatch.setattr(m, "_regime_label_series", fake_regime_series)
    monkeypatch.setattr(m, "run_free_arm", fake_run_free_arm)
    monkeypatch.setattr(m, "replay_candidate_for_entry", fake_replay)

    spec = {
        "open_name": "test_open", "params": None, "direction": "long",
        "incumbent_close": [{"name": "tiered_tp_atr", "params": {}}],
        "candidate_close": [{"name": "atr_stop", "params": {}}],
        "control_stops": {}, "candidate_stops": {}, "replayable": True,
        "gate": {"allowed_regimes": None}, "regime_cfg": {"classifier": "adx"},
        "capital": 10000.0, "n_resamples": 50, "ci": 0.95, "seed": 1,
    }
    res = m.evaluate_dataset_window(object(), spec, "BTC/USDT", "1h",
                                    ("2025-01-01", None))
    assert captured["sig_pos"] == 2
    assert captured["regime_frame_len"] == 8
    assert res is not None and res["paired_diag"]["paired"] == 1


def test_parser_accepts_and_rejects_intrabar_modes():
    p = m.build_parser()
    base = ["--strategy", "momentum", "--candidate-close", "[]"]
    assert p.parse_args(base).intrabar_resolution == "ohlc_walk"
    assert p.parse_args(base + ["--intrabar-resolution", "bar_close"]
                        ).intrabar_resolution == "bar_close"
    with pytest.raises(SystemExit):
        p.parse_args(base + ["--intrabar-resolution", "nonsense"])
