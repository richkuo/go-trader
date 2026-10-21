import json
import os
import subprocess
import sys

import pytest

REPO = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
SCRIPTS = os.path.join(REPO, "shared_scripts")
CREDENTIAL_PREFIXES = ("HYPERLIQUID", "OKX", "ROBINHOOD", "TOPSTEP", "PROJECTX")

HYPERLIQUID_SOLO = ("check_hyperliquid.py", ["vortex_trend", "BTC", "4h"])
SOLO_SCRIPTS = [
    HYPERLIQUID_SOLO,
    ("check_topstep.py", ["rsi", "ES", "1h"]),
    ("check_okx.py", ["rsi", "BTC", "1h", "--inst-type=swap"]),
    ("check_robinhood.py", ["rsi", "BTC", "1h"]),
    ("check_strategy.py", ["rsi", "BTC/USDT", "1h"]),
]

REFUSED_INPUTS = [
    (["--mode=live"], "explicit live mode"),
    (["--mode", "live"], "explicit live mode"),
    ([], "missing mode"),
    (["--mode=paper", "--mode=paper"], "invalid mode input"),
    (["--mode=paper", "--mode", "live"], "invalid mode input"),
    (["--mode="], "invalid mode input"),
    (["--mode=Paper"], "invalid mode input"),
    (["--mode=live", "--allow-no-edge=1"], "invalid acknowledgement"),
    (["--mode=live", "--allow-no-edge=true"], "invalid acknowledgement"),
    (["--mode=paper", "--allow-no-edge", "--allow-no-edge"], "invalid acknowledgement"),
]

REFUSAL_CLASS_INPUTS = [
    (["--mode=live"], "explicit live mode"),
    ([], "missing mode"),
    (["--mode=Paper"], "invalid mode input"),
    (["--mode=live", "--allow-no-edge=1"], "invalid acknowledgement"),
]


def _env():
    return {k: v for k, v in os.environ.items() if not k.startswith(CREDENTIAL_PREFIXES)}


def _run(script, argv, stdin=None):
    return subprocess.run(
        [sys.executable, os.path.join(SCRIPTS, script)] + argv,
        input=None if stdin is None else json.dumps(stdin),
        capture_output=True, text=True, cwd=REPO, env=_env(), timeout=180,
    )


def _payload(proc):
    return json.loads(proc.stdout.strip().splitlines()[-1])


def _candles(n=220, start_ms=1_700_000_000_000, step_ms=14_400_000):
    out = []
    price = 100.0
    for i in range(n):
        drift = 0.9 if (i // 25) % 2 == 0 else -0.8
        price = max(price + drift + (1.4 if i % 5 == 0 else -0.6), 5.0)
        out.append([start_ms + i * step_ms, price - 0.4, price + 1.5, price - 1.5, price, 1000.0 + i])
    return out


def _market(rows, mid):
    return {
        "version": 3,
        "snapshot_id": "14400s/1",
        "generation": 1,
        "sealed_at_ms": rows[-1][0],
        "frames": {"BTC|4h": {
            "rows": rows, "required": 200, "bars": len(rows), "coverage_short": False,
            "first_open_ms": rows[0][0], "last_open_ms": rows[-1][0], "last_close_ms": rows[-1][0],
            "last_recv_at_ms": rows[-1][0], "source": "ws", "ready": True, "forming_bar_included": True,
        }},
        "mids": {"BTC": {"px": mid, "recv_at_ms": rows[-1][0], "source": "ws", "age_ms": 0,
                         "stale": False, "confirmed": True}},
        "feed_complete": True,
    }


def _solo_envelope():
    rows = _candles()
    return {"v": 2, "market": _market(rows, rows[-1][4])}


def _assert_refused(proc, name, reason, *evidence):
    assert proc.returncode == 1, proc.stderr[-2000:]
    payload = _payload(proc)
    assert payload["signal"] == 0
    assert payload["strategy"] == name
    for needle in (f"open strategy '{name}' is edge_status=no_edge", reason, *evidence):
        assert needle in payload["error"], needle
    assert "Fetching" not in proc.stderr


@pytest.mark.parametrize("extra,reason", REFUSED_INPUTS)
def test_hyperliquid_check_refuses_every_no_edge_input_outside_explicit_paper(extra, reason):
    script, positional = HYPERLIQUID_SOLO
    _assert_refused(_run(script, positional + extra), positional[0], reason)


@pytest.mark.parametrize("script,positional", [s for s in SOLO_SCRIPTS if s != HYPERLIQUID_SOLO])
@pytest.mark.parametrize("extra,reason", REFUSAL_CLASS_INPUTS)
def test_every_other_check_script_wires_each_refusal_class(script, positional, extra, reason):
    _assert_refused(_run(script, positional + extra), positional[0], reason)


@pytest.mark.parametrize("script,positional", [s for s in SOLO_SCRIPTS if s[0] != "check_strategy.py"])
def test_dangling_mode_never_reaches_evaluation(script, positional):
    proc = _run(script, positional + ["--mode"])
    assert proc.returncode == 2
    assert "--mode: expected one argument" in proc.stderr
    assert proc.stdout.strip() == ""


def test_spot_dangling_mode_is_refused_as_invalid_input():
    proc = _run("check_strategy.py", ["rsi", "BTC/USDT", "1h", "--mode"])
    assert proc.returncode == 1
    assert "dangling --mode" in _payload(proc)["error"]


def test_close_fallback_reference_is_gated_and_native_close_is_not():
    refs_fallback = json.dumps({"open": {"name": "breakout"}, "closes": [{"name": "rsi"}]})
    refused = _run("check_hyperliquid.py", ["breakout", "BTC", "4h", "--mode=live", "--strategy-refs", refs_fallback])
    assert refused.returncode == 1
    error = _payload(refused)["error"]
    assert "close strategy 'rsi' is edge_status=no_edge" in error and "explicit live mode" in error

    refs_native = json.dumps({"open": {"name": "breakout"}, "closes": [{"name": "tiered_tp_atr"}]})
    native = _run("check_hyperliquid.py", ["breakout", "BTC", "4h", "--mode=live", "--strategy-refs", refs_native,
                                           "--market-stdin"], _solo_envelope())
    assert native.returncode == 0, native.stderr[-2000:]
    assert _payload(native)["close_strategies"] == ["tiered_tp_atr"]


def test_explicit_open_reference_replaces_the_positional_name():
    refs = json.dumps({"open": {"name": "breakout"}})
    proc = _run("check_hyperliquid.py", ["rsi", "BTC", "4h", "--mode=live", "--strategy-refs", refs,
                                         "--market-stdin"], _solo_envelope())
    assert proc.returncode == 0, proc.stderr[-2000:]
    assert _payload(proc)["open_strategy"] == "breakout"


@pytest.mark.parametrize("extra", [["--mode=paper"], ["--mode", "paper"], ["--mode=live", "--allow-no-edge"],
                                   ["--allow-no-edge"]])
def test_solo_paper_or_acknowledged_evaluation_runs_offline(extra):
    proc = _run("check_hyperliquid.py", ["vortex_trend", "BTC", "4h", "--market-stdin"] + extra, _solo_envelope())
    assert proc.returncode == 0, proc.stderr[-2000:]
    out = _payload(proc)
    assert out["strategy"] == "vortex_trend"
    assert out["signal"] in (-1, 0, 1)
    assert not out.get("error")


def _batch(slots):
    rows = _candles()
    return _run("check_hyperliquid.py", ["--batch-check", "--symbol=BTC", "--timeframe=4h", "--market-stdin"],
                {"v": 2, "slots": slots, "market": _market(rows, rows[-1][4])})


def test_batch_slots_carry_their_own_mode_evidence_and_match_solo():
    proc = _batch([
        {"id": "paper", "strategy": "vortex_trend", "mode": "paper", "mode_args": ["--mode=paper"]},
        {"id": "normalized-paper-only", "strategy": "vortex_trend", "mode": "paper"},
        {"id": "acked", "strategy": "vortex_trend", "mode": "live", "mode_args": ["--mode=live"],
         "allow_no_edge": True},
        {"id": "unacked-live", "strategy": "vortex_trend", "mode": "live", "mode_args": ["--mode=live"]},
        {"id": "repeated", "strategy": "vortex_trend", "mode": "paper", "mode_args": ["--mode=paper", "--mode=paper"]},
        {"id": "peer", "strategy": "breakout", "mode": "paper"},
    ])
    assert proc.returncode == 1
    results = {r["id"]: r for r in _payload(proc)["results"]}
    assert not results["paper"].get("error") and not results["acked"].get("error") and not results["peer"].get("error")
    assert "missing mode" in results["normalized-paper-only"]["error"]
    assert "explicit live mode" in results["unacked-live"]["error"]
    assert "invalid mode input" in results["repeated"]["error"]

    solo = _run("check_hyperliquid.py", ["vortex_trend", "BTC", "4h", "--mode=paper", "--market-stdin"], _solo_envelope())
    solo_out = _payload(solo)
    for key in ("signal", "open_action", "close_fraction"):
        assert results["paper"].get(key) == solo_out.get(key)
        assert results["acked"].get(key) == solo_out.get(key)


@pytest.mark.parametrize("bad", [{"allow_no_edge": "true"}, {"allow_no_edge": 1}, {"mode_args": "--mode=paper"},
                                 {"mode_args": ["--mode=paper", 1]}])
def test_malformed_batch_evidence_rejects_the_payload_before_evaluation(bad):
    proc = _batch([dict({"id": "slot", "strategy": "vortex_trend", "mode": "paper"}, **bad)])
    assert proc.returncode == 1
    payload = _payload(proc)
    assert payload["error_scope"] == "shared_state"
    assert payload["results"] == []
    assert payload["error"].startswith("invalid batch payload")


OBV = "on_balance_volume_divergence"


def test_obv_candidate_without_mode_is_refused():
    _assert_refused(_run("check_hyperliquid.py", [OBV, "BTC", "4h"]), OBV, "missing mode")


@pytest.mark.parametrize("extra,reason", [(["--mode=live"], "explicit live mode"), ([], "missing mode")])
def test_obv_close_fallback_reference_is_refused_without_paper_or_acknowledgement(extra, reason):
    refs = json.dumps({"open": {"name": "breakout"}, "closes": [{"name": OBV}]})
    proc = _run("check_hyperliquid.py", ["breakout", "BTC", "4h", "--strategy-refs", refs] + extra)
    assert proc.returncode == 1
    error = _payload(proc)["error"]
    assert f"close strategy '{OBV}' is edge_status=no_edge" in error and reason in error


@pytest.mark.parametrize("extra", [["--mode=paper"], ["--mode=live", "--allow-no-edge"]])
def test_obv_paper_or_acknowledged_frozen_input_check_runs_without_an_order(extra):
    refs = json.dumps({"open": {"name": OBV}, "closes": [{"name": "tiered_tp_pct"}]})
    proc = _run("check_hyperliquid.py", [OBV, "BTC", "4h", "--strategy-refs", refs, "--market-stdin"] + extra,
                _solo_envelope())
    assert proc.returncode == 0, proc.stderr[-2000:]
    out = _payload(proc)
    assert out["strategy"] == OBV and out["open_strategy"] == OBV
    assert out["close_strategies"] == ["tiered_tp_pct"]
    assert out["signal"] in (-1, 0, 1)
    assert not out.get("error")
    assert out["indicators"]["obvd_support_bars"] == 59
    assert "order" not in out and "fill" not in out


def test_obv_batch_slots_carry_their_own_mode_evidence_and_match_solo():
    proc = _batch([
        {"id": "paper", "strategy": OBV, "mode": "paper", "mode_args": ["--mode=paper"]},
        {"id": "normalized-paper-only", "strategy": OBV, "mode": "paper"},
        {"id": "acked", "strategy": OBV, "mode": "live", "mode_args": ["--mode=live"], "allow_no_edge": True},
        {"id": "unacked-live", "strategy": OBV, "mode": "live", "mode_args": ["--mode=live"]},
        {"id": "peer", "strategy": "breakout", "mode": "paper", "mode_args": ["--mode=paper"]},
    ])
    assert proc.returncode == 1
    results = {r["id"]: r for r in _payload(proc)["results"]}
    assert not results["paper"].get("error") and not results["acked"].get("error") and not results["peer"].get("error")
    assert "missing mode" in results["normalized-paper-only"]["error"]
    assert "explicit live mode" in results["unacked-live"]["error"]
    solo = _payload(_run("check_hyperliquid.py", [OBV, "BTC", "4h", "--mode=paper", "--market-stdin"], _solo_envelope()))
    for key in ("signal", "open_action", "close_fraction", "indicators"):
        assert results["paper"].get(key) == solo.get(key)
        assert results["acked"].get(key) == solo.get(key)


CMO = "chande_momentum"
HOUR_MS = 3_600_000


def _hourly_market():
    rows = _candles(step_ms=HOUR_MS)
    market = _market(rows, rows[-1][4])
    market["snapshot_id"] = "3600s/1"
    market["frames"] = {"BTC|1h": market["frames"]["BTC|4h"]}
    return market


def _cmo_run(argv):
    return _run("check_hyperliquid.py", argv + ["--market-stdin"], {"v": 2, "market": _hourly_market()})


def _cmo_batch(slots):
    return _run("check_hyperliquid.py", ["--batch-check", "--symbol=BTC", "--timeframe=1h", "--market-stdin"],
                {"v": 2, "slots": slots, "market": _hourly_market()})


def test_chande_open_reference_without_mode_is_refused():
    _assert_refused(_run("check_hyperliquid.py", [CMO, "BTC", "1h"]), CMO, "missing mode")


def test_chande_close_fallback_reference_without_mode_is_refused():
    refs = json.dumps({"open": {"name": "breakout"}, "closes": [{"name": CMO}]})
    proc = _run("check_hyperliquid.py", ["breakout", "BTC", "1h", "--strategy-refs", refs])
    assert proc.returncode == 1, proc.stderr[-2000:]
    error = _payload(proc)["error"]
    assert f"close strategy '{CMO}' is edge_status=no_edge" in error
    assert "missing mode" in error


@pytest.mark.parametrize("extra", [["--mode=paper"], ["--mode", "paper"], ["--mode=live", "--allow-no-edge"],
                                   ["--allow-no-edge"]])
def test_chande_paper_or_acknowledged_evaluation_runs_offline_and_reports_inputs(extra):
    proc = _cmo_run([CMO, "BTC", "1h"] + extra)
    assert proc.returncode == 0, proc.stderr[-2000:]
    out = _payload(proc)
    assert out["strategy"] == CMO and not out.get("error")
    assert out["signal"] in (-1, 0, 1)
    ind = out["indicators"]
    for key in ("cmo", "cmo_prev", "cmo_gain_sum", "cmo_loss_sum", "cmo_close", "cmo_eval_ts_ms",
                "cmo_valid", "cmo_reason_code"):
        assert key in ind, key
    assert ind["cmo_valid"] == 1.0


@pytest.mark.parametrize("extra", [["--mode=paper"], ["--mode=live", "--allow-no-edge"]])
def test_chande_close_fallback_admitted_in_paper_or_acknowledged(extra):
    refs = json.dumps({"open": {"name": "breakout"}, "closes": [{"name": CMO}]})
    proc = _cmo_run(["breakout", "BTC", "1h", "--strategy-refs", refs] + extra)
    assert proc.returncode == 0, proc.stderr[-2000:]
    out = _payload(proc)
    assert not out.get("error")
    assert out["close_strategies"] == [CMO]


def test_chande_batch_slots_apply_the_admission_matrix_and_match_solo():
    fallback = {"strategy": "breakout", "open_strategy": "breakout", "close_strategies": [CMO]}
    proc = _cmo_batch([
        {"id": "paper", "strategy": CMO, "mode": "paper", "mode_args": ["--mode=paper"]},
        {"id": "paper-space", "strategy": CMO, "mode": "paper", "mode_args": ["--mode", "paper"]},
        {"id": "acked", "strategy": CMO, "mode": "live", "mode_args": ["--mode=live"], "allow_no_edge": True},
        {"id": "unacked-live", "strategy": CMO, "mode": "live", "mode_args": ["--mode=live"]},
        {"id": "false-live", "strategy": CMO, "mode": "live", "mode_args": ["--mode=live"], "allow_no_edge": False},
        {"id": "missing", "strategy": CMO, "mode": "paper"},
        {"id": "repeated", "strategy": CMO, "mode": "paper", "mode_args": ["--mode=paper", "--mode=paper"]},
        dict(fallback, id="close-paper", mode="paper", mode_args=["--mode=paper"]),
        dict(fallback, id="close-acked", mode="live", mode_args=["--mode=live"], allow_no_edge=True),
        dict(fallback, id="close-unacked", mode="live", mode_args=["--mode=live"]),
        dict(fallback, id="close-missing", mode="paper"),
    ])
    assert proc.returncode == 1
    results = {r["id"]: r for r in _payload(proc)["results"]}
    for ok in ("paper", "paper-space", "acked", "close-paper", "close-acked"):
        assert not results[ok].get("error"), (ok, results[ok].get("error"))
    assert "explicit live mode" in results["unacked-live"]["error"]
    assert "explicit live mode" in results["false-live"]["error"]
    assert "missing mode" in results["missing"]["error"]
    assert "invalid mode input" in results["repeated"]["error"]
    assert f"close strategy '{CMO}'" in results["close-unacked"]["error"]
    assert "explicit live mode" in results["close-unacked"]["error"]
    assert "missing mode" in results["close-missing"]["error"]

    solo = _payload(_cmo_run([CMO, "BTC", "1h", "--mode=paper"]))
    for key in ("signal", "open_action", "close_fraction", "indicators"):
        assert results["paper"].get(key) == solo.get(key)
        assert results["acked"].get(key) == solo.get(key)


@pytest.mark.parametrize("bad", [{"allow_no_edge": "true"}, {"allow_no_edge": 1}, {"mode_args": "--mode=paper"}])
def test_chande_malformed_batch_evidence_rejects_the_payload(bad):
    proc = _cmo_batch([dict({"id": "slot", "strategy": CMO, "mode": "paper"}, **bad)])
    assert proc.returncode == 1
    payload = _payload(proc)
    assert payload["error_scope"] == "shared_state"
    assert payload["results"] == []


MFI = "money_flow_index_reversal"
MFI_REF = {"name": MFI, "params": {"lookback": 14, "oversold": 20.0, "overbought": 80.0}}


def test_money_flow_index_reversal_without_mode_is_refused():
    _assert_refused(_run("check_hyperliquid.py", [MFI, "BTC", "4h"]), MFI, "missing mode",
                    "backtest/candidates/money_flow_index_reversal_1658/REPORT.md")


@pytest.mark.parametrize("refs,mode,needle", [
    ({"open": MFI_REF}, ["--mode=live"], f"open strategy '{MFI}' is edge_status=no_edge"),
    ({"open": MFI_REF}, [], "missing mode"),
    ({"open": {"name": "breakout"}, "closes": [{"name": MFI}]}, ["--mode=live"],
     f"close strategy '{MFI}' is edge_status=no_edge"),
    ({"open": {"name": "breakout"}, "closes": [{"name": MFI}]}, [], "missing mode"),
])
def test_money_flow_index_effective_open_and_close_fallback_are_gated(refs, mode, needle):
    proc = _run("check_hyperliquid.py", ["breakout", "BTC", "4h", "--strategy-refs", json.dumps(refs),
                                         "--market-stdin"] + mode, _solo_envelope())
    assert proc.returncode == 1
    error = _payload(proc)["error"]
    assert needle in error


@pytest.mark.parametrize("extra", [["--mode=paper"], ["--mode", "paper"], ["--mode=live", "--allow-no-edge"],
                                   ["--allow-no-edge"]])
def test_money_flow_index_paper_or_acknowledged_runs_on_sealed_input(extra):
    refs = {"open": MFI_REF, "closes": [{"name": "tiered_tp_atr", "params": {}}]}
    proc = _run("check_hyperliquid.py", [MFI, "BTC", "4h", "--strategy-refs", json.dumps(refs), "--market-stdin"]
                + extra, _solo_envelope())
    assert proc.returncode == 0, proc.stderr[-2000:]
    out = _payload(proc)
    assert not out.get("error")
    assert out["open_strategy"] == MFI and out["close_strategies"] == ["tiered_tp_atr"]
    assert out["indicators"]["mfi_lookback"] == 14.0
    assert out["indicators"]["mfi_valid"] in (0.0, 1.0)
    assert "fill" not in out and "order" not in out


def test_money_flow_index_batch_slots_apply_the_same_admission_and_match_solo():
    refs = {"open": MFI_REF, "closes": [{"name": "tiered_tp_atr", "params": {}}]}
    fallback = {"open": {"name": "breakout"}, "closes": [{"name": MFI}]}
    proc = _batch([
        {"id": "paper", "strategy": MFI, "mode": "paper", "mode_args": ["--mode=paper"], "strategy_refs": refs},
        {"id": "acked", "strategy": MFI, "mode": "live", "mode_args": ["--mode=live"], "allow_no_edge": True,
         "strategy_refs": refs},
        {"id": "unacked-live", "strategy": MFI, "mode": "live", "mode_args": ["--mode=live"], "strategy_refs": refs},
        {"id": "missing", "strategy": MFI, "mode": "paper", "strategy_refs": refs},
        {"id": "repeated", "strategy": MFI, "mode": "paper", "mode_args": ["--mode=paper", "--mode=paper"],
         "strategy_refs": refs},
        {"id": "fallback-live", "strategy": "breakout", "mode": "live", "mode_args": ["--mode=live"],
         "strategy_refs": fallback},
        {"id": "fallback-paper", "strategy": "breakout", "mode": "paper", "mode_args": ["--mode=paper"],
         "strategy_refs": fallback},
    ])
    assert proc.returncode == 1
    results = {r["id"]: r for r in _payload(proc)["results"]}
    for ok in ("paper", "acked", "fallback-paper"):
        assert not results[ok].get("error"), results[ok]
    assert "explicit live mode" in results["unacked-live"]["error"]
    assert "missing mode" in results["missing"]["error"]
    assert "invalid mode input" in results["repeated"]["error"]
    assert f"close strategy '{MFI}' is edge_status=no_edge" in results["fallback-live"]["error"]

    solo = _run("check_hyperliquid.py", [MFI, "BTC", "4h", "--mode=paper", "--strategy-refs", json.dumps(refs),
                                         "--market-stdin"], _solo_envelope())
    solo_out = _payload(solo)
    for key in ("signal", "open_action", "close_fraction", "indicators"):
        assert results["paper"].get(key) == solo_out.get(key)
        assert results["acked"].get(key) == solo_out.get(key)


EOM = "ease_of_movement_breakout"
EOM_REFS = json.dumps({"open": {"name": EOM}, "closes": [{"name": "tiered_tp_atr"}]})


def test_eom_candidate_without_mode_is_refused():
    _assert_refused(_run("check_hyperliquid.py", [EOM, "BTC", "4h"]), EOM, "missing mode", "source study_fail")


@pytest.mark.parametrize("extra,reason", [(["--mode=live"], "explicit live mode"), ([], "missing mode")])
def test_eom_close_fallback_reference_is_refused_without_paper_or_acknowledgement(extra, reason):
    refs = json.dumps({"open": {"name": "breakout"}, "closes": [{"name": EOM}]})
    proc = _run("check_hyperliquid.py", ["breakout", "BTC", "4h", "--strategy-refs", refs] + extra)
    assert proc.returncode == 1
    error = _payload(proc)["error"]
    assert f"close strategy '{EOM}' is edge_status=no_edge" in error and reason in error


@pytest.mark.parametrize("extra", [["--mode=paper"], ["--mode", "paper"], ["--mode=live", "--allow-no-edge"],
                                   ["--allow-no-edge"]])
def test_eom_paper_or_acknowledged_frozen_input_check_runs_without_an_order(extra):
    proc = _run("check_hyperliquid.py", [EOM, "BTC", "4h", "--strategy-refs", EOM_REFS, "--market-stdin"] + extra,
                _solo_envelope())
    assert proc.returncode == 0, proc.stderr[-2000:]
    out = _payload(proc)
    assert out["strategy"] == EOM and out["open_strategy"] == EOM
    assert out["close_strategies"] == ["tiered_tp_atr"]
    assert out["signal"] in (-1, 0, 1)
    assert not out.get("error")
    assert out["indicators"]["eom_interval_ms"] == 14_400_000
    assert out["indicators"]["eom_valid"] == 1.0
    assert "eom_eval_ts_ms" not in out["indicators"]
    assert out["indicator_labels"]["eom_reason"] in ("no_breakout", "breakout_continuation", "entry_long",
                                                     "entry_short", "indicator_disagrees", "zero_indicator")
    assert "order" not in out and "fill" not in out


def test_eom_batch_slots_carry_their_own_mode_evidence_and_match_solo():
    proc = _batch([
        {"id": "paper", "strategy": EOM, "mode": "paper", "mode_args": ["--mode=paper"]},
        {"id": "normalized-paper-only", "strategy": EOM, "mode": "paper"},
        {"id": "acked", "strategy": EOM, "mode": "live", "mode_args": ["--mode=live"], "allow_no_edge": True},
        {"id": "unacked-live", "strategy": EOM, "mode": "live", "mode_args": ["--mode=live"]},
        {"id": "close-fallback-live", "strategy": "breakout", "mode": "live", "mode_args": ["--mode=live"],
         "close_strategies": EOM},
        {"id": "peer", "strategy": "breakout", "mode": "paper", "mode_args": ["--mode=paper"]},
    ])
    assert proc.returncode == 1
    results = {r["id"]: r for r in _payload(proc)["results"]}
    assert not results["paper"].get("error") and not results["acked"].get("error") and not results["peer"].get("error")
    assert "missing mode" in results["normalized-paper-only"]["error"]
    assert "explicit live mode" in results["unacked-live"]["error"]
    assert f"close strategy '{EOM}'" in results["close-fallback-live"]["error"]
    solo = _payload(_run("check_hyperliquid.py", [EOM, "BTC", "4h", "--mode=paper", "--market-stdin"], _solo_envelope()))
    for key in ("signal", "open_action", "close_fraction", "indicators", "indicator_labels"):
        assert results["paper"].get(key) == solo.get(key)
        assert results["acked"].get(key) == solo.get(key)


def test_ultimate_oscillator_open_reference_without_mode_is_refused():
    proc = _run("check_hyperliquid.py", ["ultimate_oscillator", "BTC", "4h", "--market-stdin"], _solo_envelope())
    _assert_refused(proc, "ultimate_oscillator", "missing mode")


@pytest.mark.parametrize("extra,reason", [(["--mode=live"], "explicit live mode"), ([], "missing mode")])
def test_ultimate_oscillator_close_fallback_is_refused_before_market_access(extra, reason):
    refs = json.dumps({"open": {"name": "breakout"}, "closes": [{"name": "ultimate_oscillator"}]})
    proc = _run("check_hyperliquid.py", ["breakout", "BTC", "4h", "--strategy-refs", refs] + extra)
    assert proc.returncode == 1, proc.stderr[-2000:]
    error = _payload(proc)["error"]
    assert "close strategy 'ultimate_oscillator' is edge_status=no_edge" in error and reason in error


def test_ultimate_oscillator_paper_and_acknowledged_solo_and_batch_agree_offline():
    solo = _run("check_hyperliquid.py", ["ultimate_oscillator", "BTC", "4h", "--mode=paper", "--market-stdin"],
                _solo_envelope())
    assert solo.returncode == 0, solo.stderr[-2000:]
    solo_out = _payload(solo)
    assert solo_out["strategy"] == "ultimate_oscillator" and not solo_out.get("error")
    assert "uo_reason_code" in solo_out["indicators"]
    acked = _run("check_hyperliquid.py", ["ultimate_oscillator", "BTC", "4h", "--mode=live", "--allow-no-edge",
                                          "--market-stdin"], _solo_envelope())
    assert acked.returncode == 0, acked.stderr[-2000:]
    proc = _batch([
        {"id": "paper", "strategy": "ultimate_oscillator", "mode": "paper", "mode_args": ["--mode=paper"]},
        {"id": "fallback", "strategy": "breakout", "open_strategy": "breakout",
         "close_strategies": "ultimate_oscillator", "mode": "paper", "mode_args": ["--mode=paper"]},
        {"id": "unacked-live", "strategy": "ultimate_oscillator", "mode": "live", "mode_args": ["--mode=live"]},
        {"id": "fallback-live", "strategy": "breakout", "open_strategy": "breakout",
         "close_strategies": "ultimate_oscillator", "mode": "live", "mode_args": ["--mode=live"]},
    ])
    results = {r["id"]: r for r in _payload(proc)["results"]}
    assert not results["paper"].get("error") and not results["fallback"].get("error")
    assert "explicit live mode" in results["unacked-live"]["error"]
    assert "close strategy 'ultimate_oscillator'" in results["fallback-live"]["error"]
    for key in ("signal", "open_action", "close_fraction"):
        assert results["paper"].get(key) == solo_out.get(key) == _payload(acked).get(key)


AROON = ["aroon_trend", "BTC", "4h"]
AROON_TP = {"name": "tiered_tp_pct", "params": {"tp_tiers": [{"profit_pct": 0.03, "close_fraction": 1.0}]}}


def _aroon_refs(params=None, closes=(AROON_TP,)):
    refs = {"open": {"name": "aroon_trend", "params": params or {"period": 25, "strength": 70.0}},
            "closes": list(closes)}
    return ["--strategy-refs", json.dumps(refs)]


def test_aroon_solo_check_without_mode_is_refused():
    _assert_refused(_run("check_hyperliquid.py", AROON + _aroon_refs()), "aroon_trend", "missing mode",
                    "backtest/candidates/aroon_trend_1643/REPORT.md")


@pytest.mark.parametrize("extra", [["--mode=paper"], ["--mode", "paper"], ["--mode=live", "--allow-no-edge"],
                                   ["--allow-no-edge"]])
def test_aroon_paper_or_acknowledged_solo_check_runs_offline(extra):
    proc = _run("check_hyperliquid.py", AROON + ["--market-stdin"] + extra + _aroon_refs(), _solo_envelope())
    assert proc.returncode == 0, proc.stderr[-2000:]
    out = _payload(proc)
    assert out["strategy"] == "aroon_trend" and not out.get("error")
    assert out["close_strategies"] == ["tiered_tp_pct"]
    assert out["indicators"]["aroon_valid"] == 1.0
    assert out["indicators"]["aroon_reason_code"] in (-1, 0, 1, 2)


@pytest.mark.parametrize("params,needle", [
    ({"period": True}, "period"),
    ({"period": 2.5}, "period must be an integer"),
    ({"period": 1}, "period"),
    ({"period": 199}, "period"),
    ({"period": "25"}, "period"),
    ({"strength": None}, "strength must be a number"),
    ({"strength": 100.5}, "strength"),
    ({"strength": "70"}, "strength must be a number"),
])
def test_aroon_invalid_parameters_are_refused_before_any_market_fetch(params, needle):
    proc = _run("check_hyperliquid.py", AROON + ["--mode=paper"] + _aroon_refs(params))
    assert proc.returncode == 1, proc.stderr[-2000:]
    payload = _payload(proc)
    assert payload["signal"] == 0
    assert needle in payload["error"]
    assert "Fetching" not in proc.stderr


def test_aroon_close_fallback_is_gated_and_its_parameters_are_preflighted():
    fallback = {"name": "aroon_trend", "params": {"period": 14, "strength": 50.0}}
    refs = json.dumps({"open": {"name": "breakout"}, "closes": [fallback]})
    refused = _run("check_hyperliquid.py", ["breakout", "BTC", "4h", "--mode=live", "--strategy-refs", refs])
    assert refused.returncode == 1
    assert "close strategy 'aroon_trend' is edge_status=no_edge" in _payload(refused)["error"]

    acked = _run("check_hyperliquid.py", ["breakout", "BTC", "4h", "--mode=live", "--allow-no-edge",
                                          "--strategy-refs", refs, "--market-stdin"], _solo_envelope())
    assert acked.returncode == 0, acked.stderr[-2000:]
    assert _payload(acked)["close_strategies"] == ["aroon_trend"]

    bad = json.dumps({"open": {"name": "breakout"}, "closes": [{"name": "aroon_trend", "params": {"period": 0}}]})
    proc = _run("check_hyperliquid.py", ["breakout", "BTC", "4h", "--mode=paper", "--strategy-refs", bad])
    assert proc.returncode == 1
    assert "period" in _payload(proc)["error"]


def test_aroon_batch_slots_admit_refuse_and_match_solo():
    refs = {"open": {"name": "aroon_trend", "params": {"period": 25, "strength": 70.0}}, "closes": [AROON_TP]}
    proc = _batch([
        {"id": "paper", "strategy": "aroon_trend", "mode": "paper", "mode_args": ["--mode=paper"], "strategy_refs": refs},
        {"id": "acked", "strategy": "aroon_trend", "mode": "live", "mode_args": ["--mode=live"],
         "allow_no_edge": True, "strategy_refs": refs},
        {"id": "unacked-live", "strategy": "aroon_trend", "mode": "live", "mode_args": ["--mode=live"],
         "strategy_refs": refs},
        {"id": "missing", "strategy": "aroon_trend", "mode": "paper", "strategy_refs": refs},
        {"id": "repeated", "strategy": "aroon_trend", "mode": "paper", "mode_args": ["--mode=paper", "--mode=live"],
         "strategy_refs": refs},
        {"id": "bad-params", "strategy": "aroon_trend", "mode": "paper", "mode_args": ["--mode=paper"],
         "strategy_refs": {"open": {"name": "aroon_trend", "params": {"period": 2.5}}}},
        {"id": "peer", "strategy": "breakout", "mode": "paper"},
    ])
    assert proc.returncode == 1
    results = {r["id"]: r for r in _payload(proc)["results"]}
    assert not results["paper"].get("error") and not results["acked"].get("error") and not results["peer"].get("error")
    assert "explicit live mode" in results["unacked-live"]["error"]
    assert "missing mode" in results["missing"]["error"]
    assert "invalid mode input" in results["repeated"]["error"]
    assert "period must be an integer" in results["bad-params"]["error"]

    solo = _payload(_run("check_hyperliquid.py", AROON + ["--mode=paper", "--market-stdin"] + _aroon_refs(),
                         _solo_envelope()))
    for key in ("signal", "open_action", "close_fraction", "indicators"):
        assert results["paper"].get(key) == solo.get(key)
        assert results["acked"].get(key) == solo.get(key)


RCR = ["regression_channel_reversion", "BTC", "4h"]
RCR_PARAMS = {"fit_window": 50, "band_multiplier": 2.0, "slope_ceiling": 0.10, "min_residual_std": 0.0}
RCR_TP = {"name": "tiered_tp_pct", "params": {"tp_tiers": [{"profit_pct": 0.03, "close_fraction": 1.0}]}}
RCR_REPORT = "backtest/candidates/regression_channel_reversion_1644/REPORT.md"


def _rcr_refs(params=None, closes=(RCR_TP,)):
    refs = {"open": {"name": "regression_channel_reversion", "params": params or RCR_PARAMS},
            "closes": list(closes)}
    return ["--strategy-refs", json.dumps(refs)]


def test_rcr_solo_check_without_mode_is_refused():
    _assert_refused(_run("check_hyperliquid.py", RCR + _rcr_refs()), "regression_channel_reversion", "missing mode",
                    RCR_REPORT)


@pytest.mark.parametrize("extra", [["--mode=paper"], ["--mode", "paper"], ["--mode=live", "--allow-no-edge"],
                                   ["--allow-no-edge"]])
def test_rcr_paper_or_acknowledged_solo_check_runs_offline(extra):
    proc = _run("check_hyperliquid.py", RCR + ["--market-stdin"] + extra + _rcr_refs(), _solo_envelope())
    assert proc.returncode == 0, proc.stderr[-2000:]
    out = _payload(proc)
    assert out["strategy"] == "regression_channel_reversion" and not out.get("error")
    assert out["close_strategies"] == ["tiered_tp_pct"]
    assert out["indicators"]["rcr_fit_window"] == 50
    assert out["indicators"]["rcr_interval_ms"] == 14_400_000
    assert out["indicators"]["rcr_reason_code"] in (-1, 0, 1, 2, 3, 17)
    assert out["indicator_labels"]["rcr_reason"]


@pytest.mark.parametrize("params,needle", [
    ({"fit_window": True}, "fit_window"),
    ({"fit_window": 2.5}, "fit_window must be an integer"),
    ({"fit_window": 1}, "fit_window"),
    ({"fit_window": 198}, "fit_window"),
    ({"fit_window": "50"}, "fit_window must be an integer"),
    ({"band_multiplier": 0}, "band_multiplier"),
    ({"band_multiplier": None}, "band_multiplier must be a number"),
    ({"slope_ceiling": -0.1}, "slope_ceiling"),
    ({"min_residual_std": "1"}, "min_residual_std must be a number"),
])
def test_rcr_invalid_parameters_are_refused_before_any_market_fetch(params, needle):
    proc = _run("check_hyperliquid.py", RCR + ["--mode=paper"] + _rcr_refs(params))
    assert proc.returncode == 1, proc.stderr[-2000:]
    payload = _payload(proc)
    assert payload["signal"] == 0
    assert needle in payload["error"]
    assert "Fetching" not in proc.stderr


def test_rcr_close_fallback_is_gated_and_its_parameters_are_preflighted():
    fallback = {"name": "regression_channel_reversion", "params": {"fit_window": 20}}
    refs = json.dumps({"open": {"name": "breakout"}, "closes": [fallback]})
    for mode in (["--mode=live"], []):
        refused = _run("check_hyperliquid.py", ["breakout", "BTC", "4h", *mode, "--strategy-refs", refs])
        assert refused.returncode == 1
        assert "close strategy 'regression_channel_reversion' is edge_status=no_edge" in _payload(refused)["error"]

    acked = _run("check_hyperliquid.py", ["breakout", "BTC", "4h", "--mode=live", "--allow-no-edge",
                                          "--strategy-refs", refs, "--market-stdin"], _solo_envelope())
    assert acked.returncode == 0, acked.stderr[-2000:]
    assert _payload(acked)["close_strategies"] == ["regression_channel_reversion"]

    bad = json.dumps({"open": {"name": "breakout"},
                      "closes": [{"name": "regression_channel_reversion", "params": {"fit_window": 0}}]})
    proc = _run("check_hyperliquid.py", ["breakout", "BTC", "4h", "--mode=paper", "--strategy-refs", bad])
    assert proc.returncode == 1
    assert "fit_window" in _payload(proc)["error"]


def test_rcr_batch_slots_admit_refuse_and_match_solo():
    refs = {"open": {"name": "regression_channel_reversion", "params": RCR_PARAMS}, "closes": [RCR_TP]}
    name = "regression_channel_reversion"
    proc = _batch([
        {"id": "paper", "strategy": name, "mode": "paper", "mode_args": ["--mode=paper"], "strategy_refs": refs},
        {"id": "acked", "strategy": name, "mode": "live", "mode_args": ["--mode=live"],
         "allow_no_edge": True, "strategy_refs": refs},
        {"id": "unacked-live", "strategy": name, "mode": "live", "mode_args": ["--mode=live"], "strategy_refs": refs},
        {"id": "missing", "strategy": name, "mode": "paper", "strategy_refs": refs},
        {"id": "repeated", "strategy": name, "mode": "paper", "mode_args": ["--mode=paper", "--mode=live"],
         "strategy_refs": refs},
        {"id": "bad-params", "strategy": name, "mode": "paper", "mode_args": ["--mode=paper"],
         "strategy_refs": {"open": {"name": name, "params": {"fit_window": 2.5}}}},
        {"id": "fallback-live", "strategy": "breakout", "mode": "live", "mode_args": ["--mode=live"],
         "strategy_refs": {"open": {"name": "breakout"}, "closes": [{"name": name}]}},
        {"id": "peer", "strategy": "breakout", "mode": "paper"},
    ])
    assert proc.returncode == 1
    results = {r["id"]: r for r in _payload(proc)["results"]}
    assert not results["paper"].get("error") and not results["acked"].get("error") and not results["peer"].get("error")
    assert "explicit live mode" in results["unacked-live"]["error"]
    assert "missing mode" in results["missing"]["error"]
    assert "invalid mode input" in results["repeated"]["error"]
    assert "fit_window must be an integer" in results["bad-params"]["error"]
    assert "close strategy 'regression_channel_reversion'" in results["fallback-live"]["error"]

    solo = _payload(_run("check_hyperliquid.py", RCR + ["--mode=paper", "--market-stdin"] + _rcr_refs(),
                         _solo_envelope()))
    for key in ("signal", "open_action", "close_fraction", "indicators", "indicator_labels"):
        assert results["paper"].get(key) == solo.get(key)
        assert results["acked"].get(key) == solo.get(key)
