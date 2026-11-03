import pandas as pd
import pytest

from backtester import Backtester

COL = "regime_w_side"
COLUMNS = {"gate": COL, "directional": COL, "atr": COL}
FAR_CLOSE = [{"name": "tiered_tp_pct", "params": {"tp_tiers": [
    {"profit_pct": 0.9, "close_fraction": 1.0}]}}]
NEAR_CLOSE = [{"name": "tiered_tp_pct", "params": {"tp_tiers": [
    {"profit_pct": 0.01, "close_fraction": 1.0}]}}]
PAPER = {"context": "paper"}
START = pd.Timestamp("2024-01-01", tz="UTC")


def _frame(signal, labels, ready=None, closes=None, htf=None):
    n = len(signal)
    closes = closes or [100.0] * n
    frame = pd.DataFrame({
        "open": [closes[0]] + closes[:-1],
        "high": [c + 0.2 for c in closes],
        "low": [c - 0.2 for c in closes],
        "close": closes,
        "volume": [1.0] * n,
        "signal": signal,
        COL: labels,
        COL + "_ready": ready if ready is not None else [True] * n,
        COL + "_bars": [30] * n,
        COL + "_need": [20] * n,
    }, index=pd.date_range(START, periods=n, freq="D"))
    if htf is not None:
        frame["htf_trend"] = htf
    return frame


def _bt(**kw):
    kw.setdefault("direction", "both")
    kw.setdefault("close_strategies", FAR_CLOSE)
    kw.setdefault("regime_side_certification", PAPER)
    return Backtester(initial_capital=1000, commission_pct=0.0, slippage_pct=0.0,
                      regime_enabled=True, regime_label_columns=COLUMNS,
                      open_strategy={"name": "scripted_trigger"},
                      regime_side=True, **kw)


def _sides(result):
    return [t["side"] for t in result["trades"]]


def _entries(result):
    return [str(t["entry_date"]) for t in result["trades"]]


def _run(signal, labels, **kw):
    frame_kw = {k: kw.pop(k) for k in ("ready", "closes", "htf") if k in kw}
    return _bt(**kw).run(_frame(signal, labels, **frame_kw), save=False)


def test_either_trigger_sign_opens_the_label_side(subtests):
    for label, trigger, side in [
            ("trending_down_choppy", 1, "short"), ("trending_down_choppy", -1, "short"),
            ("trending_up_clean", 1, "long"), ("trending_up_clean", -1, "long"),
            ("trending_up_choppy", -1, "long"), ("trending_down_clean", 1, "short"),
            ("ranging_directional_up", -1, "long"), ("ranging_directional_down", 1, "short"),
            ("trending_up", -1, "long"), ("trending_down", 1, "short")]:
        with subtests.test(label=label, trigger=trigger):
            res = _run([0, trigger, 0, 0], [label] * 4)
            assert _sides(res) == [side]
            assert _entries(res) == [str(START + pd.Timedelta(days=2))]
            assert res["regime_side"]["counts"]["opened_" + side] == 1


def test_zero_triggers_non_directional_labels_and_unready_windows_hold(subtests):
    cases = [
        ("zero_trigger", [0, 0, 0, 0], ["trending_down_choppy"] * 4, None, "held:zero_trigger"),
        ("ranging_quiet", [1, -1, 1, 0], ["ranging_quiet"] * 4, None, "held:no_side_label"),
        ("ranging_volatile", [1, -1, 1, 0], ["ranging_volatile"] * 4, None, "held:no_side_label"),
        ("bare_ranging_directional", [1, -1, 1, 0], ["ranging_directional"] * 4, None,
         "held:no_side_label"),
        ("legacy_ranging", [1, -1, 1, 0], ["ranging"] * 4, None, "held:no_side_label"),
        ("unknown_label", [1, -1, 1, 0], ["sideways"] * 4, None, "held:no_side_label"),
        ("empty_label", [1, -1, 1, 0], [""] * 4, None, "held:window_unavailable"),
        ("warming_window", [1, -1, 1, 0], ["trending_down_choppy"] * 4, [False] * 4,
         "held:window_warming"),
    ]
    for name, signal, labels, ready, outcome in cases:
        with subtests.test(case=name):
            res = _run(signal, labels, ready=ready)
            assert res["trades"] == []
            assert res["regime_side"]["counts"].get(outcome, 0) >= 1
            assert not any(k.startswith("opened") for k in res["regime_side"]["counts"])


def test_the_decision_reads_the_label_of_the_trigger_bar(subtests):
    labels = ["ranging_quiet", "trending_down_choppy", "trending_up_clean", "trending_up_clean"]
    res = _run([0, 1, 0, 0], labels)
    assert _sides(res) == ["short"]
    flipped = _run([0, 1, 0, 0], ["ranging_quiet", "ranging_quiet", "trending_down_choppy",
                                  "trending_down_choppy"])
    assert flipped["trades"] == []


def test_base_direction_is_a_permission_filter(subtests):
    for direction, label, expected in [
            ("long", "trending_up_clean", ["long"]), ("long", "trending_down_choppy", []),
            ("short", "trending_down_choppy", ["short"]), ("short", "trending_up_clean", []),
            ("both", "trending_up_clean", ["long"]), ("both", "trending_down_choppy", ["short"])]:
        for trigger in (1, -1):
            with subtests.test(direction=direction, label=label, trigger=trigger):
                res = _run([0, trigger, 0, 0], [label] * 4, direction=direction)
                assert _sides(res) == expected
                if not expected:
                    assert res["regime_side"]["counts"]["held:base_direction"] == 1


def test_higher_timeframe_filter_reads_the_mapped_side(subtests):
    down = ["trending_down_choppy"] * 4
    for name, trigger, htf, expected in [
            ("raw_disagrees_mapped_agrees_opens", 1, -1, ["short"]),
            ("raw_agrees_mapped_disagrees_holds", 1, 1, []),
            ("raw_short_mapped_short_trend_up_holds", -1, 1, []),
            ("flat_trend_passes", 1, 0, ["short"])]:
        with subtests.test(case=name):
            res = _run([0, trigger, 0, 0], down, htf=[htf] * 4, htf_filter=True)
            assert _sides(res) == expected
            if not expected:
                assert res["regime_side"]["counts"]["held:htf_filter"] == 1
    with pytest.raises(ValueError, match="htf_trend"):
        _run([0, 1, 0, 0], down, htf_filter=True)


def test_an_open_position_cannot_add_reverse_or_close_and_reopen(subtests):
    n = 8
    labels = ["trending_down_choppy"] * 3 + ["trending_up_clean"] * (n - 3)
    with subtests.test(case="no_add_or_reverse"):
        res = _run([1] * n, labels, allow_scale_in=True)
        assert _sides(res) == ["short"]
        assert res["scale_in_adds"] == 0
        assert res["regime_side"]["counts"]["held:position_open"] == n - 2

    closes = [100.0, 100.0, 100.0, 98.0, 98.0, 98.0, 98.0, 98.0]
    down = ["trending_down_choppy"] * n
    with subtests.test(case="close_then_entry_needs_a_later_decision"):
        res = _run([1] * n, down, closes=closes, close_strategies=NEAR_CLOSE)
        trades = res["trades"]
        assert [t["side"] for t in trades[:2]] == ["short", "short"]
        first_exit = pd.Timestamp(trades[0]["exit_date"])
        second_entry = pd.Timestamp(trades[1]["entry_date"])
        assert second_entry - first_exit == pd.Timedelta(days=1)
        baseline = Backtester(
            initial_capital=1000, commission_pct=0.0, slippage_pct=0.0,
            close_strategies=NEAR_CLOSE, direction="both",
        ).run(_frame([-1] * n, down, closes=closes), save=False)
        assert pd.Timestamp(baseline["trades"][1]["entry_date"]) == pd.Timestamp(
            baseline["trades"][0]["exit_date"])


def test_a_future_bar_cannot_change_an_earlier_decision():
    n = 10
    signal = [0, 1, 0, 0, 0, -1, 0, 0, 1, 1]
    labels = ["trending_down_choppy"] * 5 + ["trending_up_clean"] * 5
    base = _run(signal, labels, close_strategies=NEAR_CLOSE,
                closes=[100.0, 100.0, 100.0, 98.0, 98.0, 98.0, 99.5, 101.0, 101.0, 101.0])
    changed_labels = labels[:-2] + ["trending_down_choppy", "ranging_quiet"]
    changed = _run([*signal[:-2], -1, 0], changed_labels, close_strategies=NEAR_CLOSE,
                   closes=[100.0, 100.0, 100.0, 98.0, 98.0, 98.0, 99.5, 101.0, 90.0, 120.0])
    cutoff = str(START + pd.Timedelta(days=8))
    early = [d for d in base["regime_side"]["decisions"] if d["bar"] < cutoff]
    assert early == [d for d in changed["regime_side"]["decisions"] if d["bar"] < cutoff]
    assert len(early) >= 2
    assert [t["entry_date"] for t in base["trades"] if str(t["entry_date"]) < cutoff] == [
        t["entry_date"] for t in changed["trades"] if str(t["entry_date"]) < cutoff]


def _live(states, active_from="2023-12-01T00:00:00Z", expires_at=None):
    return {"context": "live", "states": states, "active_from": active_from,
            "expires_at": expires_at}


def test_live_equivalence_needs_the_exact_label_for_that_side(subtests):
    down = ["trending_down_choppy"] * 4
    fill = START + pd.Timedelta(days=2)
    cases = [
        ("paper_needs_no_evidence", PAPER, False, ["short"]),
        ("exact_label_certified", _live({"trending_down_choppy": "short"}), False, ["short"]),
        ("sibling_label_only", _live({"trending_down_clean": "short"}), False, []),
        ("legacy_parent_only", _live({"trending_down": "short"}), False, []),
        ("opposite_side", _live({"trending_down_choppy": "long"}), False, []),
        ("empty_cell", _live({}), False, []),
        ("expired_before_decision",
         _live({"trending_down_choppy": "short"}, expires_at=str(fill)), False, []),
        ("active_after_decision",
         _live({"trending_down_choppy": "short"}, active_from=str(fill + pd.Timedelta(hours=1))),
         False, []),
        ("unknown_start", _live({"trending_down_choppy": "short"}, active_from=None), False, []),
        ("acknowledged_without_evidence", _live({}), True, ["short"]),
    ]
    for name, cert, ack, expected in cases:
        with subtests.test(case=name):
            res = _run([0, 1, 0, 0], down, regime_side_certification=cert,
                       allow_uncertified_regime_side=ack)
            assert _sides(res) == expected
            if not expected:
                assert res["regime_side"]["counts"]["held:uncertified"] == 1
    with subtests.test(case="bare_cert_never_covers_a_sub_label"):
        res = _run([0, 1, 0, 0], ["ranging_directional_down"] * 4,
                   regime_side_certification=_live({"ranging_directional": "short"}))
        assert res["trades"] == []


def test_expiry_is_checked_at_each_decision():
    labels = ["trending_down_choppy"] * 8
    expires = str(START + pd.Timedelta(days=5))
    res = _run([0, 1, 0, 0, 0, 1, 0, 0], labels, close_strategies=NEAR_CLOSE,
               closes=[100.0, 100.0, 100.0, 98.0, 98.0, 98.0, 98.0, 98.0],
               regime_side_certification=_live({"trending_down_choppy": "short"},
                                               expires_at=expires))
    assert _sides(res) == ["short"]
    assert res["regime_side"]["counts"]["held:uncertified"] == 1


def test_structural_refusals(subtests):
    cases = [
        ("invert_signal", {"invert_signal": True}),
        ("directional_policy", {"regime_directional_policy": {"trend_regime": {
            "trending_up": {"direction": "long"}}}}),
        ("no_direction", {"direction": None}),
        ("no_close", {"close_strategies": None}),
        ("open_as_close", {"close_strategies": [{"name": "scripted_trigger", "params": {}}]}),
        ("no_certification_context", {"regime_side_certification": None}),
        ("bad_context", {"regime_side_certification": {"context": "backtest"}}),
        ("paper_with_states", {"regime_side_certification": {
            "context": "paper", "states": {"trending_up": "long"}}}),
        ("spot", {"strategy_type": "spot"}),
    ]
    for name, kw in cases:
        with subtests.test(case=name), pytest.raises(ValueError):
            _bt(**kw)
    with subtests.test(case="ack_without_mode"), pytest.raises(ValueError):
        Backtester(initial_capital=1000, allow_uncertified_regime_side=True)
    with subtests.test(case="no_directional_column"), pytest.raises(ValueError):
        Backtester(initial_capital=1000, regime_enabled=True, direction="both",
                   close_strategies=FAR_CLOSE, regime_side=True,
                   regime_side_certification=PAPER)
    with subtests.test(case="vocabulary_without_side_labels"), pytest.raises(ValueError):
        _bt(regime_feature_labels={"directional": ("ranging_quiet", "ranging_volatile")})
