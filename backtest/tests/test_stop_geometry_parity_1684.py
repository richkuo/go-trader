import copy
import importlib.util
import json
import math
import os

import pandas as pd
import pytest

import eval_windows
import exit_policy_ab
import ledger_compare
import parity_diff
import run_backtest
import tune_live
from backtester import Backtester, CapabilityContext, CloseCapabilityError

REPO = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
FIXTURE_PATH = os.path.join(REPO, "backtest", "testdata", "stop_geometry_parity.json")
STRATEGY_ID = "hl-stop-geo"

with open(FIXTURE_PATH) as _fh:
    FIXTURE = json.load(_fh)
assert FIXTURE["schema_version"] == 1
EXPECTED = FIXTURE["expected"]
TOL = FIXTURE["tolerance"]
GEOMETRY = {c["id"]: c for c in FIXTURE["geometry"]}


def _simulate_module():
    spec = importlib.util.spec_from_file_location(
        "_simulate_strategy_stop_geometry_1684",
        os.path.join(REPO, "shared_scripts", "simulate_strategy.py"))
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def _config(case: dict) -> dict:
    cfg = copy.deepcopy(FIXTURE["base_config"])
    if case.get("regime"):
        cfg.update(copy.deepcopy(FIXTURE["regime_enabled"]))
    cfg.update(copy.deepcopy(case.get("config") or {}))
    sc = copy.deepcopy(FIXTURE["base_strategy"])
    if case.get("unified"):
        sc["close_strategy"] = copy.deepcopy(FIXTURE["unified_close"])
    sc.update(copy.deepcopy(case.get("strategy") or {}))
    cfg["strategies"] = [sc]
    return cfg


def _write(tmp_path, cfg: dict) -> str:
    path = tmp_path / "config.json"
    path.write_text(json.dumps(cfg))
    return str(path)


def _load(tmp_path, case: dict) -> dict:
    return run_backtest.load_strategy_config(
        _write(tmp_path, _config(case)), STRATEGY_ID, inject_user_defaults=True)


def _frame(case: dict) -> pd.DataFrame:
    pos = case["position"]
    marks = case["marks"]
    anchor = float(pos["anchor"])
    side_sign = 1 if pos["side"] == "long" else -1
    n = len(marks) + 1
    opens, closes, signals = [anchor], [anchor], [side_sign] + [0] * len(marks)
    for j, mark in enumerate(marks, start=1):
        opens.append(anchor if j == 1 else marks[j - 2])
        closes.append(float(mark))
    scale_in = case.get("scale_in")
    if scale_in:
        signals[scale_in["mark_index"]] = side_sign
        opens[scale_in["mark_index"] + 1] = float(scale_in["price"])
    df = pd.DataFrame({
        "open": opens,
        "high": [max(o, c) for o, c in zip(opens, closes)],
        "low": [min(o, c) for o, c in zip(opens, closes)],
        "close": closes,
        "volume": [1000.0] * n,
        "atr": [float(pos["entry_atr"])] * n,
        "signal": signals,
    }, index=pd.date_range("2026-03-01", periods=n, freq="1h"))
    if pos.get("regime"):
        df["regime"] = pos["regime"]
    return df


def _run(kwargs: dict, case: dict) -> tuple:
    events: list = []
    bt = Backtester(initial_capital=1000.0, commission_pct=0.0, slippage_pct=0.0,
                    intrabar_resolution="bar_close", **kwargs)
    result = bt.run(_frame(case), save=False, stop_observer=events.append)
    return bt, result, events


def _near(a: float, b: float) -> bool:
    assert math.isfinite(a), a
    return abs(a - b) <= TOL * max(1.0, abs(a), abs(b))


def _assert_step(event: dict, step: dict, label: str) -> None:
    assert _near(event["mark"], step["mark"]), (label, event, step)
    assert _near(event["high_water"], step["high_water"]), (label, event, step)
    assert _near(event["trigger"], step["trigger"]), (label, event, step)
    assert event["replaced"] == step["replaced"], (label, event, step)


@pytest.mark.parametrize("case", FIXTURE["admission"], ids=lambda c: c["id"])
def test_admission_matches_fixture(tmp_path, case):
    assert EXPECTED["admission"][case["id"]] == case["go_accept"]
    if case["python_refusal"] is None:
        kwargs = _load(tmp_path, case)
        Backtester(**kwargs)
        return
    with pytest.raises(CloseCapabilityError) as exc:
        Backtester(**_load(tmp_path, case))
    codes = {r["reason_code"] for r in exc.value.reasons}
    assert case["python_refusal"] in codes, codes
    if case["go_accept"]:
        assert case.get("note"), "a Python-only refusal must document why it is stricter"


@pytest.mark.parametrize("case", FIXTURE["geometry"], ids=lambda c: c["id"])
def test_engine_geometry_matches_real_go_functions(tmp_path, case):
    want = EXPECTED["geometry"][case["id"]]
    kwargs = _load(tmp_path, case)
    resolved = kwargs["capability_context"].to_dict()["input_evidence"][
        "resolved_live_units"]["value"]
    for key, go_value in want["resolved"].items():
        mine = resolved[key]
        if isinstance(go_value, bool):
            assert bool(mine) == go_value, (key, mine, go_value)
        elif go_value is None:
            assert mine is None, (key, mine)
        else:
            assert _near(float(mine), float(go_value)), (key, mine, go_value)

    bt, result, events = _run(kwargs, case)
    arm = [e for e in events if e["event"] == "arm"]
    assert len(arm) == 1, events
    arm = arm[0]
    assert arm["geometry"] == want["owner"], (arm, want["owner"])
    assert arm["date"] == str(_frame(case).index[1])
    assert _near(arm["anchor"], case["position"]["anchor"])
    assert _near(arm["trigger"], want["arm_trigger"]), (arm, want)
    assert _near(arm["high_water"], want["arm_high_water"]), (arm, want)
    if want["owner"] == "trailing":
        assert _near(arm["fraction"] * 100.0, want["trailing_pct"]), (arm, want)

    trail = [e for e in events if e["event"] == "trail"]
    assert len(trail) == len(want["path"]), (trail, want["path"])
    for event, step in zip(trail, want["path"]):
        _assert_step(event, step, case["id"])
        assert event["bypass_min_move"] == step["bypass_min_move"], (event, step)
        assert _near(event["anchor"], case["position"]["anchor"]), event
    sl_after = [e for e in events if e["event"] == "sl_after"]
    if want.get("sl_after"):
        assert len(sl_after) == 1, events
        _assert_step(sl_after[0], want["sl_after"], case["id"])
    else:
        assert not sl_after, sl_after

    stop_exits = [t for t in result["trades"] if t["exit_reason"] in ("sl", "signal_sl")]
    assert not stop_exits, "fixture marks must never breach the stop"
    if case.get("risk"):
        shares = result["trades"][0]["shares"]
        risk_usd = 1000.0 * float(case["strategy"]["risk_per_trade_pct"]) / 100.0
        assert _near(risk_usd / shares, want["risk_distance"]), (shares, want)
    if case.get("scale_in"):
        assert result["scale_in_adds"] == 1
        assert result["trades"][0]["entry_price"] > case["position"]["anchor"]


@pytest.mark.parametrize("rc", FIXTURE["resolver_order"], ids=lambda c: c["id"])
def test_fixed_atr_resolver_order_matches_go(tmp_path, rc):
    from backtester import _parse_stop_regime_blocks
    base = GEOMETRY[rc["base_case"]]
    bt = Backtester(**_load(tmp_path, base))
    pos = base["position"]
    regime_only, _ = _parse_stop_regime_blocks(
        {"stop_loss_atr_mult_regime": {"use_defaults": True}}, None, True)
    bt._unified_close_params = None
    bt.stop_loss_atr_mult = None
    bt._stop_loss_regime_block = regime_only["stop_loss_atr_mult_regime"]
    regime_pct = bt._capped_atr_fraction(
        bt._hl_fixed_atr_mult(pos["regime"]), pos["entry_atr"], pos["anchor"]) * 100.0
    assert regime_pct > 0
    bt = Backtester(**_load(tmp_path, base))
    bt.stop_loss_atr_mult = float(rc["inject_stop_loss_atr_mult"])
    if rc["inject_stop_loss_atr_mult_regime"]:
        bt._stop_loss_regime_block = regime_only["stop_loss_atr_mult_regime"]
    if rc["drop_unified"]:
        bt._unified_close_params = None
    got = bt._capped_atr_fraction(
        bt._hl_fixed_atr_mult(pos["regime"]), pos["entry_atr"], pos["anchor"]) * 100.0
    assert _near(got, EXPECTED["resolver_order"][rc["id"]]), (rc["id"], got)
    assert not _near(got, regime_pct), (rc["id"], got, regime_pct)


def test_direct_fraction_contract_is_unchanged():
    case = GEOMETRY["pct_long"]
    _, _, events = _run({"platform": "hyperliquid", "stop_loss_pct": 0.02}, case)
    arm = next(e for e in events if e["event"] == "arm")
    assert _near(arm["trigger"], EXPECTED["geometry"]["pct_long"]["arm_trigger"])


def _ledger_segment(sc: dict, basis: str) -> dict:
    proof = {"status": "verified", "source": "stop geometry parity fixture"}
    raw = {k: dict(proof, present=sc.get(k) is not None, value=sc.get(k))
           for k in ledger_compare.STOP_EVIDENCE_FIELDS}
    segment = {
        "basis": basis, "strategy": copy.deepcopy(sc), "regime": {},
        "stop_defaults": {k: dict(proof, present=False, value=None)
                          for k in ledger_compare.STOP_DEFAULT_KEYS},
        "stop_evidence": {"raw_fields": raw, "leverage_origin": dict(proof, value="default"),
                          "regime_atr_window": dict(proof, present=False, value=None)},
    }
    if basis == "loader_resolved":
        resolved = {k: sc.get(k) for k in ledger_compare.STOP_EVIDENCE_FIELDS}
        resolved.update(leverage=1, max_drawdown_pct=50)
        segment["strategy"].update(leverage=1, max_drawdown_pct=50)
        segment["stop_evidence"]["resolved_fields"] = {
            k: dict(proof, value=v) for k, v in resolved.items()}
    return segment


def test_every_live_config_consumer_converts_once(tmp_path):
    case = GEOMETRY["pct_long"]
    path = _write(tmp_path, _config(case))
    loaded = run_backtest.load_strategy_config(path, STRATEGY_ID)
    assert loaded["stop_loss_pct"] == pytest.approx(0.02)
    parity = parity_diff.config_from_live_config(path, STRATEGY_ID)
    assert parity.stop_kwargs["stop_loss_pct"] == pytest.approx(0.02)
    baseline = exit_policy_ab.resolve_from_baseline(path, STRATEGY_ID)
    assert baseline["stops"]["stop_loss_pct"] == pytest.approx(0.02)
    tuned = run_backtest.load_strategy_config(path, STRATEGY_ID, inject_user_defaults=True)
    candidate = tune_live.build_candidate("sma_crossover", {}, tuned)
    assert candidate["stop_context"]["stop_loss_pct"] == pytest.approx(0.02)
    round_trip = eval_windows.candidate_stop_kwargs(json.loads(json.dumps(candidate)))
    assert round_trip["stop_loss_pct"] == pytest.approx(0.02)
    ledger_inputs = []
    for basis in ledger_compare.STOP_BASES:
        segment = _ledger_segment(_config(case)["strategies"][0], basis)
        verdict = ledger_compare.resolve_historical_stops(segment, 2, "strict")
        assert verdict["status"] == "modeled" and verdict["owner"] == "fixed_pct"
        assert verdict["resolved_live_units"]["stop_loss_pct"] == 2
        assert verdict["kwargs"]["stop_loss_pct"] == pytest.approx(0.02)
        ledger_inputs.append(verdict["kwargs"])
    for kwargs in (parity.stop_kwargs, baseline["stops"], round_trip, *ledger_inputs):
        _, _, events = _run(dict(kwargs, platform="hyperliquid"), case)
        arm = next(e for e in events if e["event"] == "arm")
        assert _near(arm["trigger"], EXPECTED["geometry"]["pct_long"]["arm_trigger"])

    report = tune_live.tune_strategy(path, STRATEGY_ID, "", "", "futures", None, None,
                                     {}, str(tmp_path))
    assert report["stop_owner"]["stop_loss_pct"] == 2
    assert report["stop_owner_units"]["stop_loss_pct"] == "live_percent"


def test_preview_payload_converts_once_and_requires_unit_marker():
    sim = _simulate_module()
    payload = {"type": "perps", "platform": "hyperliquid", "stop_loss_pct": 2,
               "stop_units": "live_percent", "leverage": 1,
               "leverage_source": "strategy_config", "max_drawdown_pct": 50}
    kwargs = sim._preview_stop_kwargs(payload)
    assert kwargs["stop_loss_pct"] == pytest.approx(0.02)
    assert kwargs["max_drawdown_pct"] == pytest.approx(0.5)
    _, _, events = _run(kwargs, GEOMETRY["pct_long"])
    arm = next(e for e in events if e["event"] == "arm")
    assert _near(arm["trigger"], EXPECTED["geometry"]["pct_long"]["arm_trigger"])
    with pytest.raises(ValueError, match="stop_units"):
        sim._preview_stop_kwargs(dict(payload, stop_units=None))
    margin = dict(payload, stop_loss_pct=None, stop_loss_margin_pct=20, leverage=5)
    Backtester(**sim._preview_stop_kwargs(margin))
    with pytest.raises(CloseCapabilityError) as exc:
        Backtester(**sim._preview_stop_kwargs(dict(margin, leverage=1,
                                                   leverage_source="loader_default")))
    assert exc.value.reason_code == "UNVERIFIED_MARGIN_LEVERAGE"
    Backtester(**sim._preview_stop_kwargs(dict(margin, leverage_source="tuner_override")))
    with pytest.raises(ValueError, match="leverage_source"):
        sim._preview_stop_kwargs(dict(margin, leverage_source="loaded_config"))


@pytest.mark.parametrize("window,windows,accepted", [
    ("medium", {"medium": 14, "long": 50}, True),
    ("long", {"medium": 14, "long": 50}, True),
    ("", {"medium": 14, "long": 50}, True),
    ("short", {"long": 50, "short": 7}, True),
    ("long", {"long": 50, "short": 7}, True),
])
def test_preview_regime_window_follows_translator_rule(tmp_path, window, windows, accepted):
    sim = _simulate_module()
    regime = {"enabled": True, "period": 14, "adx_threshold": 20, "windows": windows}
    strategy = {"trailing_stop_atr_mult_regime": {"use_defaults": True},
                "regime_atr_window": window}
    payload = {"type": "perps", "platform": "hyperliquid", "stop_units": "live_percent",
               "leverage": 1, "leverage_source": "strategy_config", "max_drawdown_pct": 50,
               "regime": regime, **strategy}
    case = {"config": {"regime": regime}, "strategy": strategy}
    if accepted:
        Backtester(**_load(tmp_path, case))
        Backtester(**sim._preview_stop_kwargs(payload))
        return
    with pytest.raises(CloseCapabilityError) as loaded:
        Backtester(**_load(tmp_path, case))
    with pytest.raises(CloseCapabilityError) as previewed:
        Backtester(**sim._preview_stop_kwargs(payload))
    assert loaded.value.reason_code == previewed.value.reason_code == "MISSING_STOP_INPUT"


def test_preview_composite_primary_window_uses_the_translator_label_vocabulary(tmp_path):
    from regime import valid_labels_for_classifier
    sim = _simulate_module()
    labels = sorted(valid_labels_for_classifier("composite"))
    block = {"trend_regime": {
        label: {"atr_multiple": 2.0} for label in labels
        if label not in ("ranging_directional_up", "ranging_directional_down")}}
    regime = {"enabled": True, "period": 14, "adx_threshold": 20,
              "windows": {"medium": {"classifier": "composite", "period": 14}}}
    strategy = {"trailing_stop_atr_mult_regime": block}
    Backtester(**_load(tmp_path, {"config": {"regime": regime}, "strategy": strategy}))
    # The live lookback is at least 200 bars, so a shorter preview has blank labels.
    candles = [{"time": 1767225600 + i * 3600, "open": 100.0 + (i % 17) - (i % 7),
                "high": 101.0 + (i % 17) - (i % 7), "low": 99.0 + (i % 17) - (i % 7),
                "close": 100.5 + (i % 17) - (i % 7), "volume": 10.0} for i in range(320)]
    payload = {"type": "perps", "platform": "hyperliquid", "symbol": "BTC", "timeframe": "1h",
               "open_strategy": {"name": "sma_crossover"}, "stop_units": "live_percent",
               "leverage": 1, "leverage_source": "strategy_config", "max_drawdown_pct": 50,
               "regime": regime, **strategy}
    out = sim._run_payload({"candles": candles,
                            "configs": [{"label": "live", "config": payload}]})
    assert "error" not in out, out
    assert out["markers"]["live"], out


@pytest.mark.parametrize("case_id", ["regime_fixed_atr", "regime_trailing_system_defaults",
                                     "unified_regime_close"])
def test_label_owner_without_label_source_refuses_before_the_loop(tmp_path, case_id):
    case = GEOMETRY[case_id]
    kwargs = dict(_load(tmp_path, case), regime_enabled=False)
    frame = _frame(case).drop(columns=["regime"])
    events: list = []
    with pytest.raises(CloseCapabilityError) as exc:
        _bt_for(kwargs).run(frame, save=False, stop_observer=events.append)
    assert exc.value.reason_code == "MISSING_STOP_INPUT"
    assert exc.value.validation.phase == "runtime"
    assert events == []


def _long_labelled_frame(entry_bars: list) -> pd.DataFrame:
    from atr import ensure_atr_indicator
    n = 220
    idx = pd.date_range("2026-01-01", periods=n, freq="1h")
    closes = [100.0 + 0.35 * i + 2.5 * math.sin(i / 4.0) for i in range(n)]
    opens = [closes[0]] + closes[:-1]
    signals = [0] * n
    for bar in entry_bars:
        signals[bar] = 1
    df = pd.DataFrame({
        "open": opens,
        "high": [max(o, c) + 0.6 for o, c in zip(opens, closes)],
        "low": [min(o, c) - 0.6 for o, c in zip(opens, closes)],
        "close": closes,
        "volume": [1000.0] * n,
        "signal": signals,
    }, index=idx)
    return ensure_atr_indicator(df)


@pytest.mark.parametrize("case_id", ["unified_regime_close", "regime_trailing_system_defaults"])
def test_m6_label_owner_trades_once_labels_exist(tmp_path, case_id):
    case = GEOMETRY[case_id]
    gate = {"allowed_regimes": None, "period": 14, "adx_threshold": 20.0,
            "windows_spec": None}
    baseline = exit_policy_ab.resolve_from_baseline(
        _write(tmp_path, _config(case)), STRATEGY_ID)
    kw = exit_policy_ab._backtester_kwargs(
        "sma_crossover", None, baseline["incumbent_close"], "long", 1000.0, gate,
        baseline["stops"])
    assert kw["regime_enabled"] is True and kw["allowed_regimes"] is None
    frame = _long_labelled_frame([3, 8, 150])
    events: list = []
    result = Backtester(**kw).run(frame, save=False, stop_observer=events.append)
    assert result["stop_warmup_skipped_entries"] == 2, result.get("stop_warmup_skipped_entries")
    assert len(result["trades"]) >= 1, result["trades"]
    entry = pd.Timestamp(result["trades"][0]["entry_date"])
    assert entry > frame.index[8], entry
    arms = [e for e in events if e["event"] == "arm"]
    assert arms and all(e["trigger"] > 0 for e in arms), arms


def test_m6_arm_stamps_labels_for_a_label_owner(tmp_path):
    case = GEOMETRY["unified_regime_close"]
    gate = {"allowed_regimes": None, "period": 14, "adx_threshold": 20.0,
            "windows_spec": None}
    baseline = exit_policy_ab.resolve_from_baseline(
        _write(tmp_path, _config(case)), STRATEGY_ID)
    kw = exit_policy_ab._backtester_kwargs(
        "sma_crossover", None, baseline["incumbent_close"], "long", 1000.0, gate,
        baseline["stops"])
    assert kw["regime_enabled"] is True and kw["allowed_regimes"] is None
    dropped = exit_policy_ab._backtester_kwargs(
        "sma_crossover", None, baseline["incumbent_close"], "long", 1000.0, gate, {})
    assert dropped["regime_enabled"] is True
    plain = exit_policy_ab._backtester_kwargs(
        "sma_crossover", None, None, "long", 1000.0, gate,
        exit_policy_ab.resolve_from_baseline(
            _write(tmp_path, _config(GEOMETRY["pct_long"])), STRATEGY_ID)["stops"])
    assert plain["regime_enabled"] is False
    frame = _frame(case).drop(columns=["regime"])
    Backtester(**kw).run(frame, save=False)
    with pytest.raises(CloseCapabilityError) as exc:
        Backtester(**dict(kw, regime_enabled=False)).run(frame, save=False)
    assert exc.value.reason_code == "MISSING_STOP_INPUT"


def _bt_for(kwargs):
    return Backtester(initial_capital=1000.0, commission_pct=0.0, slippage_pct=0.0,
                      intrabar_resolution="bar_close", **kwargs)


def test_entry_during_atr_warmup_is_skipped_not_left_unprotected(tmp_path):
    case = GEOMETRY["fixed_atr_long"]
    kwargs = _load(tmp_path, case)
    frame = _frame(case)
    frame["atr"] = float("nan")
    result = _bt_for(kwargs).run(frame, save=False)
    assert result["trades"] == []
    assert result["stop_warmup_skipped_entries"] == 1


def test_implausible_entry_atr_after_warmup_refuses(tmp_path):
    case = GEOMETRY["fixed_atr_long"]
    kwargs = _load(tmp_path, case)
    frame = _frame(case)
    frame["atr"] = 60.0
    with pytest.raises(CloseCapabilityError) as exc:
        _bt_for(kwargs).run(frame, save=False)
    assert exc.value.reason_code == "MISSING_STOP_INPUT"
    assert exc.value.validation.phase == "runtime"


def test_regime_label_warmup_skips_and_unresolvable_label_refuses(tmp_path):
    case = GEOMETRY["regime_fixed_atr"]
    kwargs = _load(tmp_path, case)
    frame = _frame(case)
    frame["regime"] = ""
    result = _bt_for(kwargs).run(frame, save=False)
    assert result["trades"] == [] and result["stop_warmup_skipped_entries"] == 1
    frame["regime"] = "not_a_label"
    with pytest.raises(CloseCapabilityError) as exc:
        _bt_for(kwargs).run(frame, save=False)
    assert exc.value.reason_code == "MISSING_STOP_INPUT"


def test_seed_without_stop_inputs_starts_flat(tmp_path):
    case = GEOMETRY["fixed_atr_long"]
    kwargs = _load(tmp_path, case)
    frame = _frame(case)
    frame["signal"] = 0
    seed = {"entry_price": 100.0, "entry_date": frame.index[0], "high_water": 100.0}
    result = _bt_for(kwargs).run(frame, save=False, starting_long=seed)
    assert result["trades"] == [] and result["stop_seed_dropped"] is True
    events: list = []
    seeded = _bt_for(kwargs).run(frame, save=False, stop_observer=events.append,
                                 starting_long=dict(seed, entry_atr=2.0))
    assert len(seeded["trades"]) == 1 and "stop_seed_dropped" not in seeded
    arm = next(e for e in events if e["event"] == "arm")
    assert _near(arm["trigger"], EXPECTED["geometry"]["fixed_atr_long"]["arm_trigger"])


def test_approximate_mode_cannot_waive_stop_refusals(tmp_path):
    case = next(c for c in FIXTURE["admission"] if c["id"] == "margin_with_defaulted_leverage")
    path = _write(tmp_path, _config(case))
    with pytest.raises(CloseCapabilityError) as exc:
        run_backtest.load_strategy_config(path, STRATEGY_ID, comparison_mode="approximate")
    assert exc.value.reason_code == "UNVERIFIED_MARGIN_LEVERAGE"


def test_handoff_present_falsy_raw_field_keeps_its_value(tmp_path):
    case = {"strategy": {"stop_loss_pct": 2, "leverage": 0}}
    kwargs = _load(tmp_path, case)
    raw = kwargs["capability_context"].to_dict()["raw_fields"]
    assert raw["leverage"] == {"present": True, "value": 0}
    assert raw["trailing_stop_pct"] == {"present": False, "value": None}
    bt = Backtester(**kwargs)
    assert bt._stop_context.to_dict()["raw_fields"]["leverage"] == {"present": True, "value": 0}
    assert bt._close_validation.capability_context.to_dict()["raw_fields"]["leverage"] == {
        "present": True, "value": 0}


@pytest.mark.parametrize("context", [
    CapabilityContext(input_evidence={"leverage": {"status": "assumed", "source": "x",
                                                   "value": 1}}),
    CapabilityContext(raw_fields={"leverage": {"present": False, "value": 5}}),
    {"raw_fields": {}, "resolved_stop_owner": None, "input_evidence": {}},
], ids=["unknown_evidence_status", "absent_field_with_value", "plain_dict"])
def test_handoff_invalid_context_refuses(context):
    with pytest.raises(CloseCapabilityError) as exc:
        Backtester(platform="hyperliquid", stop_loss_pct=0.02, capability_context=context)
    assert exc.value.reason_code == "INVALID_CAPABILITY_CONTEXT"


@pytest.mark.parametrize("consumer,phase", [("bogus", "preflight"), ("engine", "bogus")])
def test_handoff_unknown_consumer_or_phase_refuses(tmp_path, consumer, phase):
    cfg = _config({"strategy": {"stop_loss_pct": 2}})
    sc = cfg["strategies"][0]
    with pytest.raises(CloseCapabilityError) as exc:
        run_backtest.translate_live_stop_config(
            cfg, sc, run_backtest.stop_raw_fields(sc), platform="hyperliquid",
            strategy_type="perps", close_refs=[], regime_cfg={},
            regime_windows_spec=None, risk_per_trade_pct=None,
            consumer=consumer, phase=phase)
    assert exc.value.reason_code == "INVALID_CAPABILITY_CONTEXT"


def test_capability_context_survives_copy(tmp_path):
    kwargs = _load(tmp_path, GEOMETRY["pct_long"])
    clone = copy.deepcopy(kwargs)
    assert clone["capability_context"] == kwargs["capability_context"]
    assert clone["capability_context"].to_dict() == kwargs["capability_context"].to_dict()
